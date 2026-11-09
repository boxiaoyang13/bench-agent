package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/apexcloud/thvv/server/internal/domain"
	"github.com/apexcloud/thvv/server/internal/report"
	"github.com/apexcloud/thvv/server/internal/runmgr"
	"github.com/apexcloud/thvv/server/internal/store"
)

// ---------------- DTO ----------------

type planDTO struct {
	Mode             string                           `json:"mode,omitempty"`
	Buckets          []string                         `json:"buckets,omitempty"`
	ParallelLadder   []int                            `json:"parallel_ladder,omitempty"`
	Number           int                              `json:"number,omitempty"`
	NumberByParallel map[int]int                      `json:"number_by_parallel,omitempty"`
	BucketOverrides  map[string]domain.BucketOverride `json:"bucket_overrides,omitempty"`
	CooldownSec      int                              `json:"cooldown_sec,omitempty"`
	SuccessRateMin   float64                          `json:"success_rate_min,omitempty"`
	TPMBudget        int64                            `json:"tpm_budget,omitempty"`
	StopOnFailure    *bool                            `json:"stop_on_failure,omitempty"`
	Request          domain.RequestDefaults           `json:"request,omitempty"`
	DatasetDir       string                           `json:"dataset_dir,omitempty"`
	ToolParams       json.RawMessage                  `json:"tool_params,omitempty"`

	// eval
	Datasets      []string          `json:"datasets,omitempty"`
	Limit         int               `json:"limit,omitempty"`
	Repeats       int               `json:"repeats,omitempty"`
	EvalBatchSize int               `json:"eval_batch_size,omitempty"`
	Judge         *domain.JudgeSpec `json:"judge,omitempty"`
	ProviderName  string            `json:"provider_name,omitempty"`
	Thinking      *bool             `json:"thinking,omitempty"`
	ThinkingType  string            `json:"thinking_type,omitempty"`

	// cache（tool=lmcache）
	Workload        string             `json:"workload,omitempty"`
	KVCacheVolumeGB float64            `json:"kv_cache_volume_gb,omitempty"`
	TokensPerGBKV   int                `json:"tokens_per_gb_kvcache,omitempty"`
	QPS             float64            `json:"qps,omitempty"`
	CacheDuration   int                `json:"duration_sec,omitempty"`
	Seed            *int               `json:"seed,omitempty"`
	NoWarmup        bool               `json:"no_warmup,omitempty"`
	IgnoreEOS       bool               `json:"ignore_eos,omitempty"`
	SharedPromptLen int                `json:"shared_prompt_length,omitempty"`
	ChatHistoryLen  int                `json:"chat_history_length,omitempty"`
	UserInputLen    int                `json:"user_input_length,omitempty"`
	OutputLen       int                `json:"output_length,omitempty"`
	Ladder          []domain.CacheTier `json:"ladder,omitempty"` // 并发档位（方案 B）
}

func (d planDTO) toDomain() domain.Plan {
	return domain.Plan{
		Mode: d.Mode, Buckets: d.Buckets, ParallelLadder: d.ParallelLadder, Number: d.Number,
		NumberByParallel: d.NumberByParallel, BucketOverrides: d.BucketOverrides,
		CooldownSec: d.CooldownSec, SuccessRateMin: d.SuccessRateMin, TPMBudget: d.TPMBudget,
		StopOnFailure: d.StopOnFailure, Request: d.Request, DatasetDir: d.DatasetDir,
		ToolParams: d.ToolParams,
		Datasets:   d.Datasets, Limit: d.Limit, Repeats: d.Repeats, EvalBatchSize: d.EvalBatchSize,
		Judge: d.Judge, ProviderName: d.ProviderName, Thinking: d.Thinking, ThinkingType: d.ThinkingType,
		Workload: d.Workload, KVCacheVolumeGB: d.KVCacheVolumeGB, TokensPerGBKV: d.TokensPerGBKV,
		QPS: d.QPS, DurationSec: d.CacheDuration, Seed: d.Seed, NoWarmup: d.NoWarmup,
		IgnoreEOS: d.IgnoreEOS, SharedPromptLen: d.SharedPromptLen, ChatHistoryLen: d.ChatHistoryLen,
		UserInputLen: d.UserInputLen, OutputLen: d.OutputLen, CacheLadder: d.Ladder,
	}
}

type inlineTargetDTO struct {
	APIURL    string  `json:"api_url"`
	Protocol  string  `json:"protocol"`
	Model     string  `json:"model"`
	APIKey    string  `json:"api_key"`
	Vendor    string  `json:"vendor,omitempty"`
	Tokenizer *string `json:"tokenizer,omitempty"`
}

type runCreateDTO struct {
	Tool       string            `json:"tool"`
	Capability string            `json:"capability"`
	TargetID   string            `json:"target_id,omitempty"`
	Target     *inlineTargetDTO  `json:"target,omitempty"`
	Plan       json.RawMessage   `json:"plan"`
	Gate       *domain.GateSpec  `json:"gate,omitempty"`
	Report     domain.ReportSpec `json:"report,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
	Callback   map[string]any    `json:"callback,omitempty"`
}

// ---------------- handlers ----------------

func (s *Server) handleCreateRun(w http.ResponseWriter, r *http.Request) {
	var dto runCreateDTO
	if !s.decode(w, r, &dto) {
		return
	}
	cap := domain.Capability(orDefault(dto.Capability, "perf"))
	if dto.Tool == "" {
		// 默认工具按请求的能力挑（多引擎后不能再取名字排序的第一个）
		dto.Tool = s.reg.ToolForCap(cap)
	}
	if !cap.Valid() {
		writeProblem(w, r, domain.NewProblem(400, "validation_failed", "capability 必须是 perf | eval | functional"))
		return
	}
	// capability 是否可用
	if _, err := s.reg.Get(dto.Tool, cap); err != nil {
		status, reason := s.reg.Status(dto.Tool, cap)
		p := domain.NewProblem(501, "not_implemented", err.Error())
		if status == "planned" || status == "preview" || status == "unavailable" {
			p.Detail = orDefault(reason, err.Error())
			p.AvailableAt = "v1.2"
		}
		writeProblem(w, r, p)
		return
	}
	if dto.TargetID != "" && dto.Target != nil {
		writeProblem(w, r, domain.NewProblem(400, "validation_failed", "target_id 与 target 只能二选一"))
		return
	}

	// 解析 target
	var (
		target    *domain.Target
		apiKey    string
		ephemeral bool
	)
	switch {
	case dto.TargetID != "":
		t, err := s.st.GetTarget(r.Context(), dto.TargetID)
		if err != nil {
			writeProblem(w, r, errProblem(err))
			return
		}
		target = t
		apiKey, _ = s.box.Decrypt(t.APIKeyEnc)
	case dto.Target != nil:
		protocol := domain.Protocol(orDefault(dto.Target.Protocol, "openai"))
		if p := s.validateTargetURL(protocol, dto.Target.APIURL); p != nil {
			writeProblem(w, r, p)
			return
		}
		tok := ""
		if dto.Target.Tokenizer != nil {
			tok = *dto.Target.Tokenizer
		}
		info := s.resolveTokenizer(dto.Target.Model, tok)
		now := time.Now().UTC()
		target = &domain.Target{
			ID: "tgt-ephemeral", Name: "ephemeral", Vendor: dto.Target.Vendor,
			APIURL: dto.Target.APIURL, Protocol: protocol, Model: dto.Target.Model,
			APIKeyHint: "••••", Tokenizer: &info,
			Defaults: domain.TargetDefaults{
				Temperature:       s.resolveTemperature(dto.Target.Model, nil),
				MaxTokens:         s.defaultMaxTokens(),
				ConnectTimeoutSec: 60, ReadTimeoutSec: 600,
			},
			CreatedAt: now, UpdatedAt: now,
		}
		apiKey = dto.Target.APIKey
		ephemeral = true
	default:
		writeProblem(w, r, domain.NewProblem(400, "validation_failed", "必须提供 target_id 或 target"))
		return
	}
	_ = ephemeral

	var planDTOv planDTO
	if len(dto.Plan) > 0 {
		// plan 里允许出现各 capability 的私有字段（eval 的 datasets、functional 的 suites 等）
		if err := json.Unmarshal(dto.Plan, &planDTOv); err != nil {
			writeProblem(w, r, domain.NewProblem(400, "validation_failed", "plan 解析失败: "+err.Error()))
			return
		}
	}
	plan := planDTOv.toDomain()
	// 请求参数默认值只在 perf 下从 target 补齐。
	// eval 不能补 max_tokens：run_eval.py 对思考模型的默认是 131072（kimi 98304），
	// 注入 target 的 perf 默认 4096 会把 reasoning 截断、最终答案为空（大分数损失）；
	// temperature 同理，eval 缺省让 Python 侧按模型推断。
	if cap == domain.CapPerf {
		if plan.Request.Temperature == nil {
			plan.Request.Temperature = target.Defaults.Temperature
		} else {
			plan.Request.Temperature = s.resolveTemperature(target.Model, plan.Request.Temperature)
		}
		if plan.Request.MaxTokens == 0 {
			plan.Request.MaxTokens = defaultInt(target.Defaults.MaxTokens, s.defaultMaxTokens())
		}
		if plan.Request.MaxTurns == 0 {
			plan.Request.MaxTurns = 1
		}
		if plan.Request.ConnectTimeoutSec == 0 {
			plan.Request.ConnectTimeoutSec = defaultInt(target.Defaults.ConnectTimeoutSec, 60)
		}
		if plan.Request.ReadTimeoutSec == 0 {
			plan.Request.ReadTimeoutSec = defaultInt(target.Defaults.ReadTimeoutSec, 600)
		}
	}

	gate := dto.Gate
	if gate != nil && gate.Profile == "" {
		gate.Profile = s.cfg.Gates.DefaultProfile
	}
	if gate != nil {
		if _, ok := s.cfg.Gates.Profiles[gate.Profile]; !ok {
			writeProblem(w, r, domain.NewProblem(400, "validation_failed", "未知的 gate.profile: "+gate.Profile))
			return
		}
		if gate.Mode == "" {
			gate.Mode = "report_only"
		}
	}

	idem := r.Header.Get("Idempotency-Key")
	run, err := s.mgr.Submit(r.Context(), runmgr.SubmitRequest{
		Tool: dto.Tool, Capability: cap, Target: target, APIKey: apiKey,
		Plan: plan, Gate: gate, ReportSpec: dto.Report, Labels: dto.Labels,
		IdempotencyKey: idem, CreatedBy: actorName(r),
	})
	if err != nil {
		writeProblem(w, r, errProblem(err))
		return
	}
	s.st.Audit(r.Context(), actorName(r), "run.create", run.ID, string(cap))
	full, err := s.mgr.Get(r.Context(), run.ID, true)
	if err != nil {
		full = run
	}
	writeJSON(w, http.StatusAccepted, full)
}

func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.RunFilter{
		Statuses:   q["status"],
		Capability: q.Get("capability"),
		Tool:       q.Get("tool"),
		TargetID:   q.Get("target_id"),
		Vendor:     q.Get("vendor"),
		Query:      q.Get("q"),
		Cursor:     q.Get("cursor"),
		Limit:      atoiDefault(q.Get("limit"), 20),
	}
	runs, next, err := s.mgr.List(r.Context(), f)
	if err != nil {
		writeProblem(w, r, errProblem(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": runs, "next_cursor": nullable(next), "has_more": next != "",
	})
}

func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	includeSteps := strings.Contains(r.URL.Query().Get("include"), "steps")
	run, err := s.mgr.Get(r.Context(), r.PathValue("run_id"), includeSteps)
	if err != nil {
		writeProblem(w, r, errProblem(err))
		return
	}
	// ?wait=true&timeout_sec=30 同步等待终态
	if r.URL.Query().Get("wait") == "true" && !run.Status.Terminal() {
		timeout := time.Duration(atoiDefault(r.URL.Query().Get("timeout_sec"), 30)) * time.Second
		deadline := time.Now().Add(timeout)
		for time.Now().Before(deadline) {
			time.Sleep(300 * time.Millisecond)
			run, err = s.mgr.Get(r.Context(), r.PathValue("run_id"), includeSteps)
			if err != nil {
				writeProblem(w, r, errProblem(err))
				return
			}
			if run.Status.Terminal() {
				break
			}
		}
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.mgr.Cancel(r.Context(), r.PathValue("run_id"))
	if err != nil {
		writeProblem(w, r, errProblem(err))
		return
	}
	s.st.Audit(r.Context(), actorName(r), "run.cancel", run.ID, "")
	writeJSON(w, http.StatusAccepted, map[string]any{
		"run_id": run.ID, "status": run.Status, "canceled_at": time.Now().UTC(),
	})
}

// handleDeleteRun 硬删除一条终态 run（含 steps / 产物记录 / 事件 / runs/<id> 目录）。
func (s *Server) handleDeleteRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("run_id")
	run, err := s.mgr.Get(r.Context(), id, false)
	if err != nil {
		writeProblem(w, r, errProblem(err))
		return
	}
	if !run.Status.Terminal() {
		p := domain.NewProblem(409, "conflict",
			fmt.Sprintf("run 正在%s，请先取消并等待进入终态", runStatusText(run.Status)))
		writeProblem(w, r, p)
		return
	}
	if err := s.mgr.Delete(r.Context(), id); err != nil {
		writeProblem(w, r, errProblem(err))
		return
	}
	s.st.Audit(r.Context(), actorName(r), "run.delete", id, string(run.Capability))
	w.WriteHeader(http.StatusNoContent)
}

// handlePurgeRuns 批量清理终态 run（默认 canceled + failed）。支持 dry_run 先看命中数。
type purgeRequest struct {
	RunIDs     []string `json:"run_ids,omitempty"` // 勾选删除：显式 id 列表
	Statuses   []string `json:"statuses,omitempty"`
	Capability string   `json:"capability,omitempty"`
	Before     string   `json:"before,omitempty"` // RFC3339；缺省=不限
	DryRun     bool     `json:"dry_run,omitempty"`
}

func (s *Server) handlePurgeRuns(w http.ResponseWriter, r *http.Request) {
	var req purgeRequest
	if r.ContentLength > 0 {
		if !s.decode(w, r, &req) {
			return
		}
	}
	f := runmgr.PurgeFilter{Statuses: req.Statuses, Capability: req.Capability, DryRun: req.DryRun, RunIDs: req.RunIDs}
	if req.Before != "" {
		t, err := time.Parse(time.RFC3339, req.Before)
		if err != nil {
			writeProblem(w, r, domain.NewProblem(400, "validation_failed", "before 必须是 RFC3339 时间: "+err.Error()))
			return
		}
		f.Before = &t
	}
	res, err := s.mgr.Purge(r.Context(), f)
	if err != nil {
		writeProblem(w, r, errProblem(err))
		return
	}
	if !res.DryRun {
		s.st.Audit(r.Context(), actorName(r), "run.purge", "",
			fmt.Sprintf("deleted=%d statuses=%v capability=%s", res.Deleted, req.Statuses, req.Capability))
	}
	writeJSON(w, http.StatusOK, res)
}

// handleRerunRun 用原记录的目标与参数重跑一份（历史页「重测」按钮）。
func (s *Server) handleRerunRun(w http.ResponseWriter, r *http.Request) {
	oldID := r.PathValue("run_id")
	run, err := s.mgr.Rerun(r.Context(), oldID, actorName(r))
	if err != nil {
		writeProblem(w, r, errProblem(err))
		return
	}
	s.st.Audit(r.Context(), actorName(r), "run.rerun", run.ID, "from="+oldID)
	full, err := s.mgr.Get(r.Context(), run.ID, true)
	if err != nil {
		full = run
	}
	writeJSON(w, http.StatusAccepted, full)
}

func runStatusText(st domain.RunStatus) string {
	switch st {
	case domain.StatusQueued:
		return "排队"
	case domain.StatusPreparing:
		return "准备中"
	case domain.StatusRunning:
		return "运行"
	case domain.StatusReporting:
		return "生成报告"
	}
	return string(st)
}

// handleRunResults 返回结构化指标 + 验收结论。
func (s *Server) handleRunResults(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("run_id")
	run, err := s.mgr.Get(r.Context(), runID, true)
	if err != nil {
		writeProblem(w, r, errProblem(err))
		return
	}
	if !run.Status.Terminal() && run.Status != domain.StatusReporting {
		writeProblem(w, r, domain.NewProblem(409, "conflict", "任务尚未结束，结果未就绪"))
		return
	}

	switch run.Capability {
	case domain.CapEval:
		writeJSON(w, http.StatusOK, domain.RunResults{
			RunID: runID, Capability: run.Capability,
			GateResult: run.GateResult, Summary: run.Summary, Eval: s.evalMetricsForRun(run),
		})
		return
	case domain.CapCache:
		writeJSON(w, http.StatusOK, domain.RunResults{
			RunID: runID, Capability: run.Capability,
			GateResult: run.GateResult, Summary: run.Summary, Cache: s.cacheMetricsForRun(run),
		})
		return
	}

	metrics := s.metricsForRun(r.Context(), run)
	res := domain.RunResults{
		RunID: runID, Capability: run.Capability,
		GateResult: run.GateResult, Summary: run.Summary,
		Metrics: domain.PerfMetrics{PerStep: metrics},
	}
	if res.Summary == nil {
		res.Summary = report.BuildSummary(run.Steps, metrics, run.GateResult)
	}
	// 难度曲线：每个档位取最大并发
	curve := map[string]map[string]any{}
	for _, m := range metrics {
		key := m.Bucket
		cur, ok := curve[key]
		if !ok || m.Parallel > cur["max_parallel_ok"].(int) {
			curve[key] = map[string]any{
				"bucket": key, "max_parallel_ok": m.Parallel,
				"ttft_p50_sec": m.TTFT.P50Sec, "output_tps": m.Throughput.OutputTPS,
			}
		}
	}
	for _, v := range curve {
		res.Metrics.DifficultyCurve = append(res.Metrics.DifficultyCurve, v)
	}
	writeJSON(w, http.StatusOK, res)
}

// evalMetricsForRun 回读 run 结束时落盘的 metrics.json（结构：{"eval": {...}}）。
func (s *Server) evalMetricsForRun(run *domain.Run) *domain.EvalMetrics {
	p := filepath.Join(string(s.cfg.Paths.RunsDir), run.ID, "metrics.json")
	b, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	var f struct {
		Eval *domain.EvalMetrics `json:"eval"`
	}
	if json.Unmarshal(b, &f) != nil {
		return nil
	}
	return f.Eval
}

// cacheMetricsForRun 回读 run 结束时落盘的 metrics.json（结构：{"cache": {...}}）。
func (s *Server) cacheMetricsForRun(run *domain.Run) *domain.CacheMetrics {
	p := filepath.Join(string(s.cfg.Paths.RunsDir), run.ID, "metrics.json")
	b, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	var f struct {
		Cache *domain.CacheMetrics `json:"cache"`
	}
	if json.Unmarshal(b, &f) != nil {
		return nil
	}
	return f.Cache
}

// metricsForRun 获取指标：优先从 benchmark_data.db 重算（保证与报告口径一致），
// 无 db（内置模拟引擎）时回读 run 结束时持久化的 metrics.json。
func (s *Server) metricsForRun(reqCtx context.Context, run *domain.Run) []domain.StepMetrics {
	var out []domain.StepMetrics
	for _, st := range run.Steps {
		if st.DBPath == "" {
			continue
		}
		if m, _, err := report.LoadStepMetrics(st.DBPath, st); err == nil {
			out = append(out, m)
		}
	}
	if len(out) > 0 {
		return out
	}
	p := filepath.Join(string(s.cfg.Paths.RunsDir), run.ID, "metrics.json")
	if b, err := os.ReadFile(p); err == nil {
		_ = json.Unmarshal(b, &out)
	}
	return out
}

func (s *Server) handleRunFailures(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("run_id")
	run, err := s.mgr.Get(r.Context(), runID, true)
	if err != nil {
		writeProblem(w, r, errProblem(err))
		return
	}
	limit := atoiDefault(r.URL.Query().Get("limit"), 50)
	reason := r.URL.Query().Get("reason")
	excerpt := s.cfg.Reports.FailureExcerptChars

	var items []domain.FailureItem
	for _, st := range run.Steps {
		if st.DBPath == "" {
			continue
		}
		fi, err := report.LoadFailures(st.DBPath, st, excerpt)
		if err != nil {
			continue
		}
		for _, f := range fi {
			if reason != "" && f.Reason != reason {
				continue
			}
			items = append(items, f)
		}
	}
	offset := atoiDefault(r.URL.Query().Get("offset"), 0)
	end := offset + limit
	if offset > len(items) {
		offset = len(items)
	}
	if end > len(items) {
		end = len(items)
	}
	next := ""
	if end < len(items) {
		next = strconv.Itoa(end)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": items[offset:end], "next_cursor": nullable(next), "total": len(items),
	})
}

func (s *Server) handleRunArtifacts(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("run_id")
	arts, err := s.st.ListArtifacts(r.Context(), runID)
	if err != nil {
		writeProblem(w, r, errProblem(err))
		return
	}
	if arts == nil {
		arts = []domain.Artifact{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": arts})
}

// handleRegenerateReports 从已落库的 benchmark_data.db 重新生成报告产物（不重跑压测）。
func (s *Server) handleRegenerateReports(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("run_id")
	var body domain.ReportSpec
	if r.ContentLength > 0 {
		if !s.decode(w, r, &body) {
			return
		}
	}
	// 只接受报告相关字段；未指定格式则沿用 run 的配置
	arts, err := s.mgr.RegenerateReports(r.Context(), runID, body.Formats)
	if err != nil {
		writeProblem(w, r, errProblem(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"run_id": runID, "artifacts": arts, "note": "已基于原始 benchmark_data.db 重新生成",
	})
}

// handleDownloadArtifact 流式下载交付物。
func (s *Server) handleDownloadArtifact(w http.ResponseWriter, r *http.Request) {
	a, _, err := s.st.GetArtifact(r.Context(), r.PathValue("artifact_id"))
	if err != nil {
		writeProblem(w, r, errProblem(err))
		return
	}
	f, err := os.Open(a.Path)
	if err != nil {
		writeProblem(w, r, domain.NewProblem(404, "not_found", "产物文件已丢失: "+filepath.Base(a.Path)))
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		writeProblem(w, r, errProblem(err))
		return
	}
	if a.SHA256 != "" {
		w.Header().Set("ETag", `"`+a.SHA256+`"`)
	}
	ctype := a.ContentType
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ctype)
	// Content-Disposition 同时给 ASCII fallback 与 RFC 5987 的 filename*：
	// 部分反向代理/网关不接受非 ASCII 头值，只给 filename* 时会被它们变成“下载失败”。
	w.Header().Set("Content-Disposition", "attachment; filename=\""+asciiFallbackName(a.Filename)+"\"; "+
		"filename*=UTF-8''"+urlEncode(a.Filename))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, a.Filename, st.ModTime(), f)
}

// ---------------- helpers ----------------

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// asciiFallbackName 生成 Content-Disposition 里的 ASCII 文件名（非 ASCII 字符用 '_' 顶替，
// 保留扩展名），供不支持 RFC 5987 filename* 的代理/客户端使用。
func asciiFallbackName(name string) string {
	if name == "" {
		return "download"
	}
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	var b strings.Builder
	for _, r := range base {
		if r < 0x20 || r > 0x7e || r == '"' || r == '\\' {
			b.WriteByte('_')
			continue
		}
		b.WriteRune(r)
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		out = "report"
	}
	return out + ext
}

func urlEncode(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
			continue
		}
		b.WriteString("%")
		const hexdigits = "0123456789ABCDEF"
		b.WriteByte(hexdigits[c>>4])
		b.WriteByte(hexdigits[c&0xf])
	}
	return b.String()
}
