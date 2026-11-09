package runmgr

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/apexcloud/thvv/server/internal/config"
	"github.com/apexcloud/thvv/server/internal/domain"
	"github.com/apexcloud/thvv/server/internal/engine"
	"github.com/apexcloud/thvv/server/internal/report"
	"github.com/apexcloud/thvv/server/internal/store"
)

// 缓存测试的硬上限（与 docs/CACHE.md §3.2 一致）。
const (
	cacheMinDurationSec = 10
	cacheMaxDurationSec = 3600
	cacheMaxQPS         = 100.0
	cacheMaxTiers       = 20  // 并发档位上限
	cacheStepBudgetSec  = 120 // 每档预留的预热 + 进程启停预算（用于总时长校验）
	cacheMaxSessions    = 10000
)

func (m *Manager) cacheCapability(tool string) *config.Capability {
	eng := m.cfg.Engines[tool]
	if eng == nil {
		return nil
	}
	return eng.Capabilities["cache"]
}

// normalizeCachePlan 规范化 cache plan：单档展开 1 个 step，ladder 展开多个 step。
func (m *Manager) normalizeCachePlan(tool string, p domain.Plan) (domain.Plan, []domain.StepSpec, error) {
	cap := m.cacheCapability(tool)
	if cap == nil || !cap.Enabled {
		return p, nil, invalid("工具 %q 未启用 cache 能力", tool)
	}
	if p.Workload == "" {
		p.Workload = "multi-round-chat"
	}
	if p.Workload != "multi-round-chat" {
		return p, nil, invalid("v1 仅支持 multi-round-chat workload（当前 %q）", p.Workload)
	}
	p.Mode = "single"

	d := cap.CacheDefaults
	if d != nil {
		if p.KVCacheVolumeGB == 0 {
			p.KVCacheVolumeGB = d.KVCacheVolumeGB
		}
		if p.TokensPerGBKV == 0 {
			p.TokensPerGBKV = d.TokensPerGBKV
		}
		if p.QPS == 0 {
			p.QPS = d.QPS
		}
		if p.DurationSec == 0 {
			p.DurationSec = d.DurationSec
		}
		if p.Seed == nil {
			p.Seed = &d.Seed
		}
		if p.SharedPromptLen == 0 {
			p.SharedPromptLen = d.SharedPromptLen
		}
		if p.ChatHistoryLen == 0 {
			p.ChatHistoryLen = d.ChatHistoryLen
		}
		if p.UserInputLen == 0 {
			p.UserInputLen = d.UserInputLen
		}
		if p.OutputLen == 0 {
			p.OutputLen = d.OutputLen
		}
	}
	if p.DurationSec == 0 {
		p.DurationSec = 60
	}
	if p.QPS == 0 {
		p.QPS = 1.0
	}
	if p.KVCacheVolumeGB == 0 {
		p.KVCacheVolumeGB = 100
	}
	if p.TokensPerGBKV == 0 {
		p.TokensPerGBKV = 65536
	}

	// 校验
	if p.DurationSec < cacheMinDurationSec || p.DurationSec > cacheMaxDurationSec {
		return p, nil, invalid("duration_sec 非法（%d..%d，当前 %d）", cacheMinDurationSec, cacheMaxDurationSec, p.DurationSec)
	}
	if p.QPS <= 0 || p.QPS > cacheMaxQPS {
		return p, nil, invalid("qps 非法（0 < qps ≤ %g，当前 %g）", cacheMaxQPS, p.QPS)
	}
	if p.KVCacheVolumeGB <= 0 {
		return p, nil, invalid("kv_cache_volume_gb 必须为正数")
	}
	if p.TokensPerGBKV < 1024 || p.TokensPerGBKV > 10_000_000 {
		return p, nil, invalid("tokens_per_gb_kvcache 非法（1024..10000000）")
	}
	for _, v := range []struct {
		name string
		v    int
	}{
		{"shared_prompt_length", p.SharedPromptLen},
		{"chat_history_length", p.ChatHistoryLen},
		{"user_input_length", p.UserInputLen},
		{"output_length", p.OutputLen},
	} {
		if v.v < 0 || v.v > 1_000_000 {
			return p, nil, invalid("%s 非法（0..1000000，当前 %d；0=用默认）", v.name, v.v)
		}
	}

	steps := []domain.StepSpec{{Index: 0, Bucket: p.Workload}}
	if len(p.CacheLadder) > 0 {
		var lerr error
		if steps, lerr = m.expandCacheLadder(&p); lerr != nil {
			return p, nil, lerr
		}
	}
	// 总时长校验：每档都有独立的 duration + 预热/启停预算。
	if m.cfg.Limits.MaxRunDuration > 0 {
		est := time.Duration(len(steps)) * (time.Duration(p.DurationSec)*time.Second +
			cacheStepBudgetSec*time.Second)
		if est > m.cfg.Limits.MaxRunDuration {
			return p, nil, invalid("并发档位总时长约 %s（%d 档 ×（%ds + 预热预算 %ds））超过单次运行上限 %s，请缩短时长或减少档位",
				est.Round(time.Minute), len(steps), p.DurationSec, cacheStepBudgetSec, m.cfg.Limits.MaxRunDuration)
		}
	}
	return p, steps, nil
}

// expandCacheLadder 把 plan.ladder 展开成多个 step，并把 sessions 反算成 KV 容量回填。
//
// MRC 只接受 kv-cache-volume（session 数 = 容量 × tokens/GB ÷ 单会话 token），
// 所以每档的 KVCacheVolumeGB 由 sessions 反推；档位按 sessions 升序执行。
func (m *Manager) expandCacheLadder(p *domain.Plan) ([]domain.StepSpec, error) {
	if len(p.CacheLadder) > cacheMaxTiers {
		return nil, invalid("并发档位最多 %d 档（当前 %d）", cacheMaxTiers, len(p.CacheLadder))
	}
	perSession := p.SharedPromptLen + p.ChatHistoryLen
	if perSession <= 0 {
		return nil, invalid("并发档位需要 shared_prompt_length + chat_history_length > 0（可由 cache_defaults 提供）")
	}
	seen := make(map[int]bool, len(p.CacheLadder))
	tiers := make([]domain.CacheTier, 0, len(p.CacheLadder))
	for i, t := range p.CacheLadder {
		if t.Sessions < 1 || t.Sessions > cacheMaxSessions {
			return nil, invalid("ladder[%d].sessions 非法（1..%d，当前 %d）", i, cacheMaxSessions, t.Sessions)
		}
		if seen[t.Sessions] {
			return nil, invalid("ladder 档位重复（sessions=%d）", t.Sessions)
		}
		seen[t.Sessions] = true
		qps := t.QPS
		if qps == 0 {
			qps = p.QPS
		}
		if qps <= 0 || qps > cacheMaxQPS {
			return nil, invalid("ladder[%d].qps 非法（0 < qps ≤ %g，当前 %g）", i, cacheMaxQPS, qps)
		}
		// 向上取整到 1e-6 GB：容量 × tokens/GB ÷ perSession 必须 ≥ sessions
		vol := math.Ceil(float64(t.Sessions*perSession)/float64(p.TokensPerGBKV)*1e6) / 1e6
		tiers = append(tiers, domain.CacheTier{
			Sessions: t.Sessions, QPS: qps, KVCacheVolumeGB: vol,
		})
	}
	sort.Slice(tiers, func(i, j int) bool { return tiers[i].Sessions < tiers[j].Sessions })
	p.CacheLadder = tiers
	steps := make([]domain.StepSpec, 0, len(tiers))
	for i, t := range tiers {
		steps = append(steps, domain.StepSpec{
			Index: i, Bucket: fmt.Sprintf("c%d", t.Sessions), Parallel: t.Sessions,
		})
	}
	return steps, nil
}

// reportCache 生成 cache run 的汇总与产物。
func (m *Manager) reportCache(ctx context.Context, lr *liveRun, res engine.Result) {
	m.setStatus(ctx, lr, domain.StatusReporting)
	lr.mu.Lock()
	run := lr.run
	steps := append([]domain.Step(nil), run.Steps...)
	lr.mu.Unlock()

	cm := res.Cache
	summary := map[string]any{}
	if cm == nil {
		cm = &domain.CacheMetrics{}
	}

	// 验收判定（cache-sla-v1）
	var gate *domain.GateResult
	if profile := m.cacheProfileOf(run); profile != nil {
		gate = report.EvaluateCacheGate(profile, cm, run.Plan.NoWarmup)
		if gate != nil {
			gate.Profile = m.cacheProfileName(run)
		}
	}

	ss := cm.SteadyState
	summary = map[string]any{
		"verdict":             cacheVerdict(cm, gate),
		"total_requests":      cm.TotalRequests,
		"successful_requests": cm.SuccessReqs,
		"success_rate":        round6(cm.SuccessRate),
		"mean_ttft_ms":        cm.MeanTTFTMS,
		"model":               cm.Model,
		"workload":            cm.Workload,
	}
	if ss != nil {
		summary["ttft_steady_p50_ms"] = round6(ss.TTFTp50MS)
		summary["ttft_steady_p90_ms"] = round6(ss.TTFTp90MS)
		summary["ttft_steady_p99_ms"] = round6(ss.TTFTp99MS)
		summary["ttft_improvement"] = round6(ss.TTFTImprovement)
		summary["samples_steady"] = ss.Requests
		if ss.TPOTp50MS > 0 {
			summary["tpot_steady_p50_ms"] = round6(ss.TPOTp50MS)
			summary["tpot_steady_p90_ms"] = round6(ss.TPOTp90MS)
		}
		if ss.DecodeSpeedP50 > 0 {
			summary["decode_speed_steady_p50"] = round6(ss.DecodeSpeedP50)
		}
	}
	// 多档：顶层是聚合，明细在 results.cache.tiers；history 等只需要档位数。
	if len(cm.Tiers) > 0 {
		summary["tiers_count"] = len(cm.Tiers)
		var maxSessions int
		for _, t := range cm.Tiers {
			if t != nil && t.Sessions > maxSessions {
				maxSessions = t.Sessions
			}
		}
		summary["max_sessions"] = maxSessions
	}
	// 性能维度：口径是「前缀缓存命中态下的表观性能」，与 perf 冷启动口径不可直接横比。
	// TTFT 受前缀缓存影响大，decode speed / TPOT 主要反映模型生成性能。
	if cm.TPOTMeanMS > 0 {
		summary["tpot_mean_ms"] = round6(cm.TPOTMeanMS)
	}
	if cm.MeanDecodeSpeed > 0 {
		summary["mean_decode_speed"] = round6(cm.MeanDecodeSpeed)
	}
	if cm.InputThroughput > 0 {
		summary["input_throughput"] = round6(cm.InputThroughput)
	}
	if cm.OutputThroughput > 0 {
		summary["output_throughput"] = round6(cm.OutputThroughput)
	}
	for k, v := range map[string]float64{
		"request_latency_p50_ms": cm.LatencyP50MS,
		"request_latency_p90_ms": cm.LatencyP90MS,
		"request_latency_p99_ms": cm.LatencyP99MS,
	} {
		if v > 0 {
			summary[k] = round6(v)
		}
	}
	if cm.FirstRound != nil {
		summary["ttft_first_round_ms"] = round6(cm.FirstRound.TTFTMeanMS)
	}
	if cm.PrefixHitRate != nil {
		summary["prefix_hit_rate"] = round6(*cm.PrefixHitRate)
		summary["cache_field_reported"] = cm.CacheFieldReported
	}
	if gate != nil {
		summary["gate_passed"] = gate.Passed
		summary["gate_violations"] = len(gate.Violations)
	}
	if res.CacheNote != "" {
		summary["error"] = res.CacheNote
	}

	lr.mu.Lock()
	lr.run.Summary = summary
	lr.run.GateResult = gate
	lr.mu.Unlock()

	runDir := filepath.Join(string(m.cfg.Paths.RunsDir), run.ID)
	_ = os.MkdirAll(runDir, 0o755)
	if b, err := json.MarshalIndent(struct {
		Cache *domain.CacheMetrics `json:"cache"`
	}{Cache: cm}, "", "  "); err == nil {
		_ = os.WriteFile(filepath.Join(runDir, "metrics.json"), b, 0o644)
	}
	dctx, dcancel := detached(ctx)
	_ = m.st.UpdateRun(dctx, run.ID, store.RunState{
		Status: domain.StatusReporting, StepsTotal: len(steps), StepsDone: countDone(steps),
		GateResult: gate, Summary: summary, StartedAt: run.StartedAt,
	})
	dcancel()

	artDir := filepath.Join(runDir, "artifacts")
	_ = os.MkdirAll(artDir, 0o755)
	for _, f := range res.Files {
		m.addArtifactFile(ctx, run.ID, f, artDir)
	}
	if gate != nil {
		if b, err := json.MarshalIndent(gate, "", "  "); err == nil {
			m.addArtifact(ctx, run.ID, "sla_evaluation.json", domain.ArtSLAJSON, "application/json", b, artDir)
		}
	}
	if res.LogPath != "" {
		if st, err := os.Stat(res.LogPath); err == nil && !st.IsDir() {
			m.addArtifactFile(ctx, run.ID, engine.FileArtifact{
				Kind: domain.ArtRunLog, Filename: "run.log", Path: res.LogPath, ContentType: "text/plain; charset=utf-8",
			}, artDir)
		}
	}
}

func cacheVerdict(cm *domain.CacheMetrics, gate *domain.GateResult) string {
	if gate != nil {
		if gate.Passed {
			return "pass"
		}
		return "fail"
	}
	if cm.TotalRequests == 0 {
		return "fail"
	}
	if cm.FailedReqs > 0 && cm.SuccessReqs == 0 {
		return "fail"
	}
	if cm.FailedReqs > 0 {
		return "partial"
	}
	return "pass"
}

// cacheProfileName 返回 cache run 的验收模板名。
func (m *Manager) cacheProfileName(run *domain.Run) string {
	if run.Gate != nil && run.Gate.Profile != "" {
		return run.Gate.Profile
	}
	return "cache-sla-v1"
}

// cacheProfileOf 返回 cache run 的阈值档（默认 cache-sla-v1）。
func (m *Manager) cacheProfileOf(run *domain.Run) *config.GateProfile {
	if run.Gate != nil && run.Gate.Profile != "" {
		return m.cfg.Gates.Profiles[run.Gate.Profile]
	}
	return m.cfg.Gates.Profiles["cache-sla-v1"]
}
