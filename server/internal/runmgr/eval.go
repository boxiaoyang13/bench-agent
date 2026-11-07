package runmgr

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/apexcloud/thvv/server/internal/config"
	"github.com/apexcloud/thvv/server/internal/domain"
	"github.com/apexcloud/thvv/server/internal/engine"
	"github.com/apexcloud/thvv/server/internal/report"
	"github.com/apexcloud/thvv/server/internal/store"
)

// 评测能力的硬上限（与 docs/EVAL.md §8 一致）。
const (
	maxEvalRepeats     = 50
	maxEvalBatchSize   = 256
	defaultEvalBatch   = 10
	defaultEvalRepeats = 1
)

func (m *Manager) evalCapability(tool string) *config.Capability {
	eng := m.cfg.Engines[tool]
	if eng == nil {
		return nil
	}
	return eng.Capabilities["eval"]
}

// normalizeEvalPlan 规范化 eval plan，并展开成「一个数据集一个 step」。
func (m *Manager) normalizeEvalPlan(tool string, target *domain.Target, p domain.Plan) (domain.Plan, []domain.StepSpec, error) {
	cap := m.evalCapability(tool)
	if cap == nil || !cap.Enabled {
		return p, nil, invalid("工具 %q 未启用 eval 能力", tool)
	}
	if len(p.Datasets) == 0 {
		return p, nil, invalid("plan.datasets 不能为空")
	}
	if len(p.Datasets) > len(cap.Datasets) {
		return p, nil, invalid("数据集数量 %d 超过上限 %d", len(p.Datasets), len(cap.Datasets))
	}

	byName := make(map[string]config.Dataset, len(cap.Datasets))
	for _, d := range cap.Datasets {
		byName[d.Name] = d
	}
	seen := map[string]bool{}
	names := make([]string, 0, len(p.Datasets))
	for _, raw := range p.Datasets {
		name := strings.TrimSpace(raw)
		if name == "" || seen[name] {
			continue
		}
		if _, ok := byName[name]; !ok {
			return p, nil, invalid("未知数据集 %q（可用见 /v1/catalog/datasets）", name)
		}
		seen[name] = true
		names = append(names, name)
	}
	if len(names) == 0 {
		return p, nil, invalid("plan.datasets 不能为空")
	}
	p.Datasets = names
	p.Mode = "single"

	if p.Limit < 0 {
		return p, nil, invalid("limit 不能为负数")
	}
	if p.Limit > m.cfg.Limits.Plan.MaxNumber {
		return p, nil, invalid("limit %d 超过上限 %d", p.Limit, m.cfg.Limits.Plan.MaxNumber)
	}
	if p.Repeats < 0 || p.Repeats > maxEvalRepeats {
		return p, nil, invalid("repeats 非法（0..%d，0=数据集默认）", maxEvalRepeats)
	}
	if p.EvalBatchSize < 0 || p.EvalBatchSize > maxEvalBatchSize {
		return p, nil, invalid("eval_batch_size 非法（0..%d，0=数据集默认）", maxEvalBatchSize)
	}
	if p.ProviderName == "" && target != nil {
		p.ProviderName = target.Vendor
	}

	// Judge：缺省从服务端配置补齐；任一所选数据集需要 Judge 时必须有 key
	needJudge := false
	for _, name := range names {
		if byName[name].NeedsJudge {
			needJudge = true
			break
		}
	}
	if cap.Judge != nil {
		if p.Judge == nil {
			p.Judge = &domain.JudgeSpec{}
		}
		if p.Judge.Model == "" {
			p.Judge.Model = cap.Judge.Model
		}
		if p.Judge.BaseURL == "" {
			p.Judge.BaseURL = cap.Judge.BaseURL
		}
		if p.Judge.APIKey == "" {
			p.Judge.APIKey = cap.Judge.APIKey
		}
	}
	if needJudge && (p.Judge == nil || p.Judge.APIKey == "") {
		return p, nil, invalid("所选数据集需要 LLM Judge，plan.judge.api_key 必填")
	}
	if p.Judge != nil && p.Judge.APIKey == "" {
		p.Judge = nil // 不需要 Judge 时不带空凭据
	}

	steps := make([]domain.StepSpec, 0, len(names))
	for i, name := range names {
		d := byName[name]
		limit := p.Limit
		if limit == 0 && d.DefaultLimit != nil {
			limit = *d.DefaultLimit
		}
		repeats := p.Repeats
		if repeats == 0 {
			if d.DefaultRepeats != nil {
				repeats = *d.DefaultRepeats
			} else {
				repeats = defaultEvalRepeats
			}
		}
		batch := p.EvalBatchSize
		if batch == 0 {
			if d.DefaultEvalBatch != nil {
				batch = *d.DefaultEvalBatch
			} else {
				batch = defaultEvalBatch
			}
		}
		steps = append(steps, domain.StepSpec{
			Index: i, Bucket: name, Number: limit, Parallel: batch, Repeats: repeats,
		})
	}
	return p, steps, nil
}

func evalSuccessCount(em *domain.EvalMetrics) (ok, total int) {
	if em == nil {
		return 0, 0
	}
	for _, d := range em.Datasets {
		total++
		if d.Success {
			ok++
		}
	}
	return ok, total
}

// evalMetricsFile 是落在 runs/<id>/metrics.json 里的结构（供 /results 回读）。
type evalMetricsFile struct {
	Eval *domain.EvalMetrics `json:"eval"`
}

// reportEval 生成 eval run 的汇总与产物（分数以 Python 落盘为准，不重算）。
func (m *Manager) reportEval(ctx context.Context, lr *liveRun, res engine.Result) {
	m.setStatus(ctx, lr, domain.StatusReporting)
	lr.mu.Lock()
	run := lr.run
	steps := append([]domain.Step(nil), run.Steps...)
	lr.mu.Unlock()

	em := res.Eval
	summary := map[string]any{}
	if em != nil {
		ok, total := evalSuccessCount(em)
		var sum float64
		for _, d := range em.Datasets {
			if d.Success {
				sum += d.Score
			}
		}
		avg := 0.0
		if ok > 0 {
			avg = sum / float64(ok)
		}
		verdict := "pass"
		switch {
		case total == 0 || ok == 0:
			verdict = "fail"
		case ok < total:
			verdict = "partial"
		}
		summary = map[string]any{
			"verdict":            verdict,
			"datasets_total":     total,
			"datasets_succeeded": ok,
			"datasets_failed":    total - ok,
			"score_avg":          round6(avg),
			"model":              em.Model,
			"provider":           em.Provider,
		}
	} else {
		summary = map[string]any{"verdict": "fail", "datasets_total": 0, "datasets_succeeded": 0, "datasets_failed": 0}
	}

	lr.mu.Lock()
	lr.run.Summary = summary
	lr.mu.Unlock()

	runDir := filepath.Join(string(m.cfg.Paths.RunsDir), run.ID)
	_ = os.MkdirAll(runDir, 0o755)
	if b, err := json.MarshalIndent(evalMetricsFile{Eval: em}, "", "  "); err == nil {
		_ = os.WriteFile(filepath.Join(runDir, "metrics.json"), b, 0o644)
	}
	// 验收判定（eval-sla-v1：按数据集分数下限）
	var gate *domain.GateResult
	if profile := m.evalProfileOf(run); profile != nil {
		gate = report.EvaluateEvalGate(profile, run.Target.Model, em.Datasets)
		if gate != nil {
			gate.Profile = m.evalProfileName(run)
			summary["gate_passed"] = gate.Passed
			summary["gate_violations"] = len(gate.Violations)
		}
	}
	lr.mu.Lock()
	lr.run.GateResult = gate
	lr.mu.Unlock()

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
	// Go 侧汇总（跨数据集）
	if em != nil {
		if b, err := json.MarshalIndent(map[string]any{
			"run_id": run.ID, "model": em.Model, "provider": em.Provider,
			"generated_at": time.Now().UTC(), "summary": summary, "results": em.Datasets,
		}, "", "  "); err == nil {
			m.addArtifact(ctx, run.ID, "all_eval_summary.json", domain.ArtEvalSummary, "application/json", b, artDir)
		}
	}
	// SLA json
	if gate != nil {
		if b, err := json.MarshalIndent(gate, "", "  "); err == nil {
			m.addArtifact(ctx, run.ID, "sla_evaluation.json", domain.ArtSLAJSON, "application/json", b, artDir)
		}
	}
	// 引擎日志
	if res.LogPath != "" {
		if st, err := os.Stat(res.LogPath); err == nil && !st.IsDir() {
			m.addArtifactFile(ctx, run.ID, engine.FileArtifact{
				Kind: domain.ArtRunLog, Filename: "run.log", Path: res.LogPath, ContentType: "text/plain; charset=utf-8",
			}, artDir)
		}
	}
}

// evalProfileName 返回 eval run 使用的验收模板名。
func (m *Manager) evalProfileName(run *domain.Run) string {
	if run.Gate != nil && run.Gate.Profile != "" {
		return run.Gate.Profile
	}
	return "eval-sla-v1"
}

// evalProfileOf 返回 eval run 的阈值档（默认 eval-sla-v1）。
func (m *Manager) evalProfileOf(run *domain.Run) *config.GateProfile {
	if run.Gate != nil && run.Gate.Profile != "" {
		return m.cfg.Gates.Profiles[run.Gate.Profile]
	}
	return m.cfg.Gates.Profiles["eval-sla-v1"]
}

// addArtifactFile 流式复制产物并登记（大文件不读进内存）。
func (m *Manager) addArtifactFile(ctx context.Context, runID string, fa engine.FileArtifact, dir string) {
	st, err := os.Stat(fa.Path)
	if err != nil || st.IsDir() {
		m.log.Warn("产物缺失，跳过", "file", fa.Path)
		return
	}
	if max := m.cfg.Limits.ArtifactMaxBytes; max > 0 && st.Size() > max {
		m.log.Warn("产物超过归档上限，未归档", "file", fa.Path, "size", st.Size(), "limit", max)
		return
	}
	name := report.SanitizeFilename(fa.Filename)
	dst := filepath.Join(dir, name)
	if err := copyFile(fa.Path, dst); err != nil {
		m.log.Warn("复制产物失败", "file", fa.Path, "err", err)
		return
	}
	sum, _ := fileSHA256(dst)
	a := domain.Artifact{
		ID: newID("art"), Kind: fa.Kind, Filename: name, ContentType: fa.ContentType,
		SizeBytes: st.Size(), SHA256: sum, CreatedAt: time.Now().UTC(), Path: dst,
	}
	dctx, dcancel := detached(ctx)
	err = m.st.PutArtifact(dctx, runID, a)
	dcancel()
	if err != nil {
		m.log.Warn("记录产物失败", "err", err)
		return
	}
	a.DownloadURL = "/v1/artifacts/" + a.ID
	if lr := m.liveRun(runID); lr != nil {
		lr.mu.Lock()
		lr.run.Artifacts = append(lr.run.Artifacts, a)
		lr.mu.Unlock()
	}
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func fileSHA256(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func round6(v float64) float64 {
	return float64(int64(v*1e6+0.5)) / 1e6
}
