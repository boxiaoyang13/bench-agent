// Package runmgr 实现 Run 生命周期：排队、状态机、SSE 广播、产物生成。
package runmgr

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/apexcloud/thvv/server/internal/config"
	"github.com/apexcloud/thvv/server/internal/domain"
	"github.com/apexcloud/thvv/server/internal/engine"
	"github.com/apexcloud/thvv/server/internal/report"
	"github.com/apexcloud/thvv/server/internal/secret"
	"github.com/apexcloud/thvv/server/internal/store"
)

// ErrQueueFull 表示排队已满。
var ErrQueueFull = errors.New("队列已满")

// ValidationError 表示请求参数不合法（HTTP 400）。
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

func invalid(format string, a ...any) error {
	return &ValidationError{Msg: fmt.Sprintf(format, a...)}
}

// Manager 是 Run 管理器。
type Manager struct {
	cfg *config.Config
	st  *store.Store
	reg *engine.Registry
	box *secret.Box
	log *slog.Logger

	hub  *hub
	mu   sync.Mutex
	seq  map[string]int
	canc map[string]context.CancelFunc
	live map[string]*liveRun

	queue     chan string
	targetSem map[string]chan struct{}
	targetMu  sync.Mutex
	wg        sync.WaitGroup
	closed    bool
}

type liveRun struct {
	mu      sync.Mutex
	run     *domain.Run
	steps   []domain.StepSpec
	started time.Time
}

// New 构造 Manager。
func New(cfg *config.Config, st *store.Store, reg *engine.Registry, box *secret.Box, log *slog.Logger) *Manager {
	return &Manager{
		cfg: cfg, st: st, reg: reg, box: box, log: log,
		hub: newHub(), seq: map[string]int{}, canc: map[string]context.CancelFunc{},
		live: map[string]*liveRun{}, targetSem: map[string]chan struct{}{},
		queue: make(chan string, max(1, cfg.Limits.MaxQueuedRuns)),
	}
}

// Start 启动 maxConcurrentRuns 个 worker。
func (m *Manager) Start(ctx context.Context) {
	n := max(1, m.cfg.Limits.MaxConcurrentRuns)
	for i := 0; i < n; i++ {
		m.wg.Add(1)
		go func(id int) {
			defer m.wg.Done()
			m.worker(ctx, id)
		}(i)
	}
	m.log.Info("run manager started", "workers", n, "queue", cap(m.queue))
}

// Shutdown 停止接收新任务并等待在跑的 run。
func (m *Manager) Shutdown(ctx context.Context) {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	close(m.queue)
	done := make(chan struct{})
	go func() { m.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		m.mu.Lock()
		for _, c := range m.canc {
			c()
		}
		m.mu.Unlock()
		<-done
	}
}

// ---------------- 提交 ----------------

// SubmitRequest 是创建 run 的输入。
type SubmitRequest struct {
	Tool           string
	Capability     domain.Capability
	Target         *domain.Target
	APIKey         string // 明文，仅内存
	Plan           domain.Plan
	Gate           *domain.GateSpec
	ReportSpec     domain.ReportSpec
	Labels         map[string]string
	IdempotencyKey string
	CreatedBy      string
}

// Submit 规范化 plan、计算 steps、落库并入队。
func (m *Manager) Submit(ctx context.Context, req SubmitRequest) (*domain.Run, error) {
	// 幂等
	if req.IdempotencyKey != "" {
		if old, err := m.st.GetRunByIdempotencyKey(ctx, req.IdempotencyKey); err == nil {
			return old, nil
		}
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, errors.New("服务正在关闭")
	}
	if len(m.queue) >= cap(m.queue) {
		m.mu.Unlock()
		return nil, ErrQueueFull
	}
	m.mu.Unlock()

	norm, err := m.normalizer(req.Tool, req.Capability)
	if err != nil {
		return nil, err
	}
	plan, err := m.normalizePlan(req.Plan, norm)
	if err != nil {
		return nil, err
	}
	steps, skipped := domain.ComputeSteps(plan, norm)
	for _, s := range skipped {
		m.log.Warn("档位跳过", "run", req.Target.Name, "bucket", s.Bucket, "reason", s.Reason, "detail", s.Detail)
	}
	if len(steps) == 0 {
		return nil, fmt.Errorf("没有可执行的步骤（档位/并发都被裁剪，或数据集未构建）")
	}
	plan.StepsTotal = len(steps)

	run := &domain.Run{
		ID:         newID("run"),
		Status:     domain.StatusQueued,
		Tool:       req.Tool,
		Capability: req.Capability,
		Target:     req.Target.Snapshot(false),
		Plan:       plan,
		Gate:       req.Gate,
		Report:     normalizeReport(req.ReportSpec, m.cfg),
		Labels:     req.Labels,
		CreatedAt:  time.Now().UTC(),
		Progress: domain.Progress{
			Phase: string(domain.StatusQueued), StepsTotal: len(steps), Percent: 0,
		},
	}
	for _, s := range steps {
		run.Steps = append(run.Steps, domain.Step{
			Index: s.Index, Bucket: s.Bucket, Parallel: s.Parallel, Number: s.Number, Status: domain.StepPending,
		})
	}

	if err := m.st.CreateRun(ctx, run, req.CreatedBy, req.IdempotencyKey); err != nil {
		if errors.Is(err, store.ErrConflict) {
			if old, e2 := m.st.GetRunByIdempotencyKey(ctx, req.IdempotencyKey); e2 == nil {
				return old, nil
			}
			return nil, err
		}
		return nil, err
	}
	for _, st := range run.Steps {
		_ = m.st.UpsertStep(ctx, run.ID, st)
	}

	lr := &liveRun{run: run, steps: steps}
	m.mu.Lock()
	m.live[run.ID] = lr
	m.mu.Unlock()

	m.log.Info("run submitted", "run_id", run.ID, "tool", req.Tool, "cap", req.Capability, "steps", len(steps))

	select {
	case m.queue <- run.ID:
	default:
		// 队列满：标记失败
		m.finish(ctx, lr, domain.StatusFailed, domain.NewProblem(429, "quota_exceeded", "队列已满，请稍后重试"))
		return nil, ErrQueueFull
	}
	return run, nil
}

// ---------------- 查询 ----------------

// Get 读取 run（含 steps 与 artifacts）。
func (m *Manager) Get(ctx context.Context, runID string, includeSteps bool) (*domain.Run, error) {
	run, err := m.st.GetRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	m.decorate(ctx, run, includeSteps)
	return run, nil
}

func (m *Manager) decorate(ctx context.Context, run *domain.Run, includeSteps bool) {
	if includeSteps {
		if steps, err := m.st.ListSteps(ctx, run.ID); err == nil {
			run.Steps = steps
		}
	}
	if arts, err := m.st.ListArtifacts(ctx, run.ID); err == nil {
		run.Artifacts = arts
	}
	if lr := m.liveRun(run.ID); lr != nil {
		lr.mu.Lock()
		run.Progress = lr.run.Progress
		run.Engine = lr.run.Engine
		lr.mu.Unlock()
	}
	run.Links = map[string]string{
		"self":      "/v1/runs/" + run.ID,
		"events":    "/v1/runs/" + run.ID + "/events",
		"results":   "/v1/runs/" + run.ID + "/results",
		"artifacts": "/v1/runs/" + run.ID + "/artifacts",
		"cancel":    "/v1/runs/" + run.ID + "/cancel",
	}
}

// List 列出 runs。
func (m *Manager) List(ctx context.Context, f store.RunFilter) ([]*domain.Run, string, error) {
	runs, next, err := m.st.ListRuns(ctx, f)
	if err != nil {
		return nil, "", err
	}
	for _, r := range runs {
		m.decorate(ctx, r, false)
	}
	return runs, next, err
}

// Cancel 取消 run（幂等）。
func (m *Manager) Cancel(ctx context.Context, runID string) (*domain.Run, error) {
	run, err := m.st.GetRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	if run.Status.Terminal() {
		return run, nil
	}
	m.mu.Lock()
	cancel := m.canc[runID]
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	// 排队中的直接置 canceled
	if run.Status == domain.StatusQueued {
		now := time.Now().UTC()
		_ = m.st.UpdateRun(ctx, runID, store.RunState{Status: domain.StatusCanceled, FinishedAt: &now})
		m.emit(ctx, runID, EvFinished, map[string]any{"status": string(domain.StatusCanceled), "finished_at": now})
	}
	run.Status = domain.StatusCanceled
	return run, nil
}

// Subscribe 订阅事件流。
func (m *Manager) Subscribe(runID string) (<-chan Event, func()) {
	return m.hub.subscribe(runID)
}

// Replay 返回 seq 之后的历史事件（用于 SSE 断点续传）。
func (m *Manager) Replay(ctx context.Context, runID string, seq int) ([]Event, error) {
	recs, err := m.st.EventsSince(ctx, runID, seq, 1000)
	if err != nil {
		return nil, err
	}
	out := make([]Event, 0, len(recs))
	for _, r := range recs {
		out = append(out, Event{Seq: r.Seq, Event: EventName(r.Event), Data: r.Data, At: r.At})
	}
	return out, nil
}

// ---------------- worker ----------------

func (m *Manager) worker(ctx context.Context, id int) {
	for runID := range m.queue {
		if ctx.Err() != nil {
			return
		}
		m.execute(ctx, runID)
	}
}

func (m *Manager) execute(ctx context.Context, runID string) {
	lr := m.liveRun(runID)
	if lr == nil {
		m.log.Warn("run 不在活跃表，跳过", "run_id", runID)
		return
	}
	run := lr.run

	// 同一 target 串行
	sem := m.targetSemaphore(run.Target.TargetID)
	select {
	case sem <- struct{}{}:
		defer func() { <-sem }()
	case <-ctx.Done():
		return
	}

	runCtx, cancel := context.WithTimeout(ctx, m.cfg.Limits.MaxRunDuration)
	m.mu.Lock()
	m.canc[runID] = cancel
	m.mu.Unlock()
	defer func() {
		cancel()
		m.mu.Lock()
		delete(m.canc, runID)
		m.mu.Unlock()
	}()

	// 1) preparing
	m.setStatus(runCtx, lr, domain.StatusPreparing)
	now := time.Now().UTC()
	lr.started = now
	run.StartedAt = &now
	m.mu.Lock()
	lr.run.StartedAt = &now
	m.mu.Unlock()
	m.emit(runCtx, runID, EvStarted, map[string]any{"phase": string(domain.StatusPreparing), "at": now})

	eng, err := m.reg.Get(run.Tool, run.Capability)
	if err != nil {
		m.finish(runCtx, lr, domain.StatusFailed, domain.NewProblem(501, "not_implemented", err.Error()))
		return
	}

	// 2) 解密凭据并执行
	apiKey, err := m.targetKey(runCtx, run)
	if err != nil {
		m.finish(runCtx, lr, domain.StatusFailed, domain.NewProblem(500, "internal_error", err.Error()))
		return
	}
	spec := engine.Spec{
		RunID:      runID,
		Capability: run.Capability,
		Target: engine.ResolvedTarget{
			TargetID: run.Target.TargetID, Vendor: run.Target.Vendor, Model: run.Target.Model,
			APIURL: run.Target.APIURL, Protocol: run.Target.Protocol, APIKey: apiKey,
			Tokenizer: m.tokenizerOf(runCtx, run), MaxTokens: m.maxTokensOf(run),
			ConnectTimeout: 60 * time.Second, ReadTimeout: 600 * time.Second,
		},
		Plan:        run.Plan,
		Steps:       lr.steps,
		WorkDir:     filepath.Join(string(m.cfg.Paths.RunsDir), runID),
		DatasetsDir: string(m.cfg.Paths.DatasetsDir),
	}
	// 长档位（512k/1m）的 token 计数必须用数据集构建时的分词器（对齐 run.sh 的
	// DATASET_TOKENIZER），显式配置 long_buckets 优先；未配置时回退为
	// dataset_built_in=false 的档位（历史行为）。
	if perf := m.perfCapability(run.Tool); perf != nil {
		spec.DatasetTokenizer = perf.DatasetTokenizer
		if len(perf.LongBuckets) > 0 {
			spec.LongBuckets = map[string]bool{}
			for _, label := range perf.LongBuckets {
				spec.LongBuckets[label] = true
			}
		}
	}
	if spec.LongBuckets == nil {
		for _, b := range m.buckets() {
			if !b.BuiltIn {
				spec.LongBuckets = map[string]bool{}
				spec.LongBuckets[b.Label] = true
			}
		}
	}
	if err := os.MkdirAll(filepath.Join(spec.WorkDir, "logs"), 0o755); err != nil {
		m.finish(runCtx, lr, domain.StatusFailed, domain.NewProblem(500, "internal_error", err.Error()))
		return
	}

	m.setStatus(runCtx, lr, domain.StatusRunning)

	res, runErr := eng.Run(runCtx, spec, func(ev engine.Event) {
		m.onEngineEvent(runCtx, lr, ev)
	})

	// 引擎返回的 step 结果更权威（含最终状态与 benchmark_data.db 路径）：
	// 事件流只带聚合指标，不合并的话 db_path 会永远为空，导致
	// /results/failures 与失败请求明细表取不到任何数据。
	for _, st := range res.Steps {
		m.updateStep(runCtx, lr, st.Index, func(dst *domain.Step) { *dst = st })
	}

	// 3) 结果处理
	switch {
	case errors.Is(runErr, context.Canceled):
		m.settleUnfinishedSteps(runCtx, lr)
		m.setStatus(runCtx, lr, domain.StatusCanceled)
		m.report(runCtx, lr, res)
		m.finish(runCtx, lr, domain.StatusCanceled, nil)
		return
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		m.settleUnfinishedSteps(runCtx, lr)
		m.report(runCtx, lr, res)
		m.finish(runCtx, lr, domain.StatusFailed, domain.NewProblem(504, "engine_timeout", "超过单次运行时长上限"))
		return
	case runErr != nil:
		m.settleUnfinishedSteps(runCtx, lr)
		m.report(runCtx, lr, res)
		m.finish(runCtx, lr, domain.StatusFailed, domain.NewProblem(500, "engine_failed", runErr.Error()))
		return
	}

	m.report(runCtx, lr, res)
	status := domain.StatusSucceeded
	if hasFailedStep(lr) {
		status = domain.StatusPartial
	}
	m.finish(runCtx, lr, status, nil)
}

// onEngineEvent 把引擎事件翻译成 run 事件并落库。
func (m *Manager) onEngineEvent(ctx context.Context, lr *liveRun, ev engine.Event) {
	switch ev.Kind {
	case engine.EvStepStarted:
		idx := asInt(ev.Data["step_index"])
		m.emit(ctx, lr.run.ID, EvStepStarted, ev.Data)
		m.updateStep(ctx, lr, idx, func(st *domain.Step) {
			st.Status = domain.StepRunning
		})
		lr.mu.Lock()
		lr.run.Progress.Current = &domain.StepHead{
			Bucket: asString(ev.Data["bucket"]), Parallel: asInt(ev.Data["parallel"]), Number: asInt(ev.Data["number"]),
		}
		lr.mu.Unlock()

	case engine.EvStepProgress:
		m.emit(ctx, lr.run.ID, EvStepProgress, ev.Data)

	case engine.EvStepFinished:
		idx := asInt(ev.Data["step_index"])
		m.emit(ctx, lr.run.ID, EvStepFinished, ev.Data)
		sr := asFloat(ev.Data["success_rate"])
		tps := asFloat(ev.Data["output_tps"])
		dur := asFloat(ev.Data["duration_sec"])
		ttft := asFloat(ev.Data["ttft_p50_sec"])
		status := domain.StepFailed
		if asString(ev.Data["status"]) == string(domain.StepSucceeded) {
			status = domain.StepSucceeded
		}
		m.updateStep(ctx, lr, idx, func(st *domain.Step) {
			st.Status = status
			st.SuccessRate = &sr
			st.OutputTPS = &tps
			st.DurationSec = &dur
			st.TTFTp50Sec = &ttft
		})
		m.bumpProgress(ctx, lr)

	case engine.EvLog:
		m.emit(ctx, lr.run.ID, EvLog, ev.Data)

	case engine.EvError:
		m.emit(ctx, lr.run.ID, EvError, ev.Data)
	}
}

func (m *Manager) bumpProgress(ctx context.Context, lr *liveRun) {
	lr.mu.Lock()
	lr.run.Progress.StepsDone++
	done := lr.run.Progress.StepsDone
	total := lr.run.Progress.StepsTotal
	started := lr.started
	lr.run.Progress.Current = nil
	elapsed := int(time.Since(started).Seconds())
	lr.run.Progress.ElapsedSec = elapsed
	lr.run.Progress.Percent = float64(done) / float64(max(total, 1)) * 100
	var eta *int
	if done > 0 {
		e := elapsed / done * (total - done)
		eta = &e
	}
	lr.run.Progress.ETASec = eta
	lr.mu.Unlock()

	dctx, dcancel := detached(ctx)
	_ = m.st.UpdateRun(dctx, lr.run.ID, store.RunState{
		Status: lr.run.Status, StepsTotal: total, StepsDone: done,
		StartedAt: lr.run.StartedAt,
	})
	dcancel()
	m.emit(ctx, lr.run.ID, EvProgress, map[string]any{
		"steps_done": done, "steps_total": total,
		"percent": lr.run.Progress.Percent, "elapsed_sec": elapsed, "eta_sec": eta,
		"current": nil,
	})
}

func (m *Manager) updateStep(ctx context.Context, lr *liveRun, idx int, mut func(*domain.Step)) {
	lr.mu.Lock()
	for i := range lr.run.Steps {
		if lr.run.Steps[i].Index == idx {
			mut(&lr.run.Steps[i])
			st := lr.run.Steps[i]
			lr.mu.Unlock()
			dctx, dcancel := detached(ctx)
			_ = m.st.UpsertStep(dctx, lr.run.ID, st)
			dcancel()
			return
		}
	}
	lr.mu.Unlock()
}

// ---------------- 结果与产物 ----------------

func (m *Manager) report(ctx context.Context, lr *liveRun, res engine.Result) {
	m.setStatus(ctx, lr, domain.StatusReporting)
	lr.mu.Lock()
	run := lr.run
	steps := append([]domain.Step(nil), run.Steps...)
	lr.mu.Unlock()

	metrics := res.Metrics
	// 若引擎没直接给指标（subprocess 场景），从产物 db 补齐
	if len(metrics) == 0 && len(res.DBPaths) > 0 {
		idxs := make([]int, 0, len(res.DBPaths))
		for i := range res.DBPaths {
			idxs = append(idxs, i)
		}
		sort.Ints(idxs)
		for _, i := range idxs {
			st := domain.Step{Index: i}
			for _, s := range steps {
				if s.Index == i {
					st = s
					break
				}
			}
			if mm, _, err := report.LoadStepMetrics(res.DBPaths[i], st); err == nil {
				mm.RawDB = res.DBPaths[i]
				metrics = append(metrics, mm)
			}
		}
	}
	lr.mu.Lock()
	lr.run.Summary = nil
	lr.mu.Unlock()

	// 验收判定
	var gate *domain.GateResult
	if run.Gate != nil && run.Gate.Profile != "" {
		profile := m.cfg.Gates.Profiles[run.Gate.Profile]
		gate = report.EvaluateGate(profile, metrics)
		if gate != nil {
			gate.Profile = run.Gate.Profile
		}
	}
	summary := report.BuildSummary(steps, metrics, gate)

	lr.mu.Lock()
	lr.run.GateResult = gate
	lr.run.Summary = summary
	lr.mu.Unlock()
	if gate != nil {
		m.emit(ctx, run.ID, EvGate, gate)
	}
	dctx, dcancel := detached(ctx)
	_ = m.st.UpdateRun(dctx, run.ID, store.RunState{
		Status: domain.StatusReporting, StepsTotal: len(steps), StepsDone: countDone(steps),
		GateResult: gate, Summary: summary, StartedAt: run.StartedAt,
	})
	dcancel()

	// 持久化指标（供 /results 回读；subprocess 引擎则由 DBPath 重算）
	if b, err := json.Marshal(metrics); err == nil {
		runDir := filepath.Join(string(m.cfg.Paths.RunsDir), run.ID)
		_ = os.MkdirAll(runDir, 0o755)
		_ = os.WriteFile(filepath.Join(runDir, "metrics.json"), b, 0o644)
	}

	// 产物
	failures := collectFailures(res, steps, m.cfg.Reports.FailureExcerptChars)
	formats := run.Report.Formats
	if len(formats) == 0 {
		formats = m.cfg.Reports.DefaultFormats
	}
	m.writeReportArtifacts(ctx, run, steps, metrics, gate, failures, formats)

	// 原始 db 与日志（仅在首次跑完时归档）
	artDir := filepath.Join(string(m.cfg.Paths.RunsDir), run.ID, "artifacts")
	for _, p := range res.DBPaths {
		if b, err := os.ReadFile(p); err == nil {
			m.addArtifact(ctx, run.ID, "benchmark_data.db", domain.ArtRawDB, "application/x-sqlite3", b, artDir)
			break
		}
	}
	if res.LogPath != "" {
		if b, err := os.ReadFile(res.LogPath); err == nil {
			m.addArtifact(ctx, run.ID, "run.log", domain.ArtRunLog, "text/plain; charset=utf-8", b, artDir)
		}
	}
}

// findStepDB 在 run 目录里按档位找回 benchmark_data.db（历史 run 的 db_path 可能为空）。
func findStepDB(runDir, bucket string) string {
	var anyDB string
	_ = filepath.Walk(runDir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info == nil {
			return nil
		}
		if info.IsDir() {
			if info.Name() == "artifacts" || info.Name() == "logs" {
				return filepath.SkipDir
			}
			return nil
		}
		if info.Name() != "benchmark_data.db" {
			return nil
		}
		if anyDB == "" {
			anyDB = p
		}
		if bucket != "" && strings.Contains(p, string(filepath.Separator)+"perf-"+bucket+"-") {
			anyDB = p
			return filepath.SkipAll
		}
		return nil
	})
	return anyDB
}

// RegenerateReports 用已落库的 steps 与 benchmark_data.db 重新生成交付物。
// 不重跑压测、不改 run 状态，用于报告模板/口径修正后补生成历史 run。
func (m *Manager) RegenerateReports(ctx context.Context, runID string, formats []string) ([]domain.Artifact, error) {
	run, err := m.st.GetRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	steps, err := m.st.ListSteps(ctx, runID)
	if err != nil {
		return nil, err
	}

	runDir := filepath.Join(string(m.cfg.Paths.RunsDir), runID)
	metrics := make([]domain.StepMetrics, 0, len(steps))
	repaired := make([]domain.Step, 0, len(steps))
	for _, st := range steps {
		if st.DBPath == "" {
			// 历史 run 可能没把 db_path 落库（修复前创建的），按档位目录名回找一次
			st.DBPath = findStepDB(runDir, st.Bucket)
			repaired = append(repaired, st)
		}
		if st.DBPath == "" {
			continue
		}
		if mm, _, e := report.LoadStepMetrics(st.DBPath, st); e == nil {
			mm.RawDB = st.DBPath
			metrics = append(metrics, mm)
		}
	}
	for _, st := range repaired {
		if st.DBPath != "" {
			_ = m.st.UpsertStep(ctx, runID, st)
		}
	}
	if len(metrics) == 0 { // 回落到 run 结束时落盘的 metrics.json
		p := filepath.Join(runDir, "metrics.json")
		if b, e := os.ReadFile(p); e == nil {
			_ = json.Unmarshal(b, &metrics)
		}
	}

	var gate *domain.GateResult
	if run.Gate != nil && run.Gate.Profile != "" {
		gate = report.EvaluateGate(m.cfg.Gates.Profiles[run.Gate.Profile], metrics)
		if gate != nil {
			gate.Profile = run.Gate.Profile
		}
	}
	summary := report.BuildSummary(steps, metrics, gate)

	dctx, dcancel := detached(ctx)
	_ = m.st.UpdateRun(dctx, runID, store.RunState{
		Status: run.Status, StepsTotal: len(steps), StepsDone: countDone(steps),
		GateResult: gate, Summary: summary, Error: run.Error, Engine: run.Engine,
		StartedAt: run.StartedAt, FinishedAt: run.FinishedAt,
	})
	dcancel()
	run.GateResult, run.Summary = gate, summary

	var failures []domain.FailureItem
	for _, st := range steps {
		if st.DBPath == "" {
			continue
		}
		if items, e := report.LoadFailures(st.DBPath, st, m.cfg.Reports.FailureExcerptChars); e == nil {
			failures = append(failures, items...)
		}
	}

	if len(formats) == 0 {
		formats = run.Report.Formats
	}
	if len(formats) == 0 {
		formats = m.cfg.Reports.DefaultFormats
	}
	m.writeReportArtifacts(ctx, run, steps, metrics, gate, failures, formats)
	return m.st.ListArtifacts(ctx, runID)
}

// writeReportArtifacts 生成/覆盖 HTML、xlsx、失败 CSV、SLA json。
// 首次跑完与 POST /runs/{id}/reports 重新生成共用这一段，保证口径一致。
func (m *Manager) writeReportArtifacts(ctx context.Context, run *domain.Run, steps []domain.Step,
	metrics []domain.StepMetrics, gate *domain.GateResult, failures []domain.FailureItem, formats []string) {

	artDir := filepath.Join(string(m.cfg.Paths.RunsDir), run.ID, "artifacts")
	_ = os.MkdirAll(artDir, 0o755)

	summary := report.BuildSummary(steps, metrics, gate)
	htmlIn := report.HTMLInput{
		Title:       m.reportTitle(run, steps),
		Model:       run.Target.Model,
		Vendor:      run.Target.Vendor,
		Company:     run.Report.ClientName,
		Capability:  string(run.Capability),
		GeneratedAt: time.Now().UTC(),
		PlanMode:    planScope(run.Plan),
		Buckets:     run.Plan.Buckets,
		Ladder:      run.Plan.ParallelLadder,
		Summary:     summary,
		Gate:        gate,
		Steps:       steps,
		Metrics:     metrics,
		Failures:    failures,
	}
	for _, f := range formats {
		switch f {
		case "html":
			if b, err := report.RenderHTML(htmlIn); err == nil {
				m.addArtifact(ctx, run.ID, "性能测试报告.html", domain.ArtHTMLReport,
					"text/html; charset=utf-8", b, artDir)
			} else {
				m.log.Warn("生成 HTML 报告失败", "err", err)
			}
		case "xlsx":
			if b, err := report.RenderXLSX(report.XLSXInput{
				SheetMetrics:  m.cfg.Reports.XLSX.SheetMetrics,
				SheetFailures: m.cfg.Reports.XLSX.SheetFailures,
				Model:         run.Target.Model,
				Vendor:        run.Target.Vendor,
				Company:       run.Report.ClientName,
				GeneratedAt:   time.Now().UTC(),
				Gate:          gate,
				APIURL:        run.Target.APIURL,
				Metrics:       metrics,
				Failures:      failures,
			}); err == nil {
				m.addArtifact(ctx, run.ID, "性能测试报告.xlsx", domain.ArtXLSXReport,
					"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", b, artDir)
			} else {
				m.log.Warn("生成 Excel 报告失败", "err", err)
			}
		case "csv":
			if len(failures) > 0 {
				if b, err := report.RenderFailureCSV(failures); err == nil {
					m.addArtifact(ctx, run.ID, "失败请求详情.csv", domain.ArtFailureCSV, "text/csv; charset=utf-8", b, artDir)
				}
			}
		}
	}
	if gate != nil {
		if b, err := json.MarshalIndent(gate, "", "  "); err == nil {
			m.addArtifact(ctx, run.ID, "sla_evaluation.json", domain.ArtSLAJSON, "application/json", b, artDir)
		}
	}
}

func (m *Manager) addArtifact(ctx context.Context, runID, filename string, kind domain.ArtifactKind, ctype string, data []byte, dir string) {
	name := report.SanitizeFilename(filename)
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		m.log.Warn("写产物失败", "file", p, "err", err)
		return
	}
	a := domain.Artifact{
		ID: newID("art"), Kind: kind, Filename: name, ContentType: ctype,
		SizeBytes: int64(len(data)), SHA256: sha256Hex(data), CreatedAt: time.Now().UTC(),
		Path: p, DownloadURL: "",
	}
	dctx, dcancel := detached(ctx)
	err := m.st.PutArtifact(dctx, runID, a)
	dcancel()
	if err != nil {
		m.log.Warn("记录产物失败", "err", err)
		return
	}
	a.DownloadURL = "/v1/artifacts/" + a.ID
	lr := m.liveRun(runID)
	if lr != nil {
		lr.mu.Lock()
		lr.run.Artifacts = append(lr.run.Artifacts, a)
		lr.mu.Unlock()
	}
}

// finish 落终态并广播。
// settleUnfinishedSteps 把「运行中」标成 canceled、「待执行」标成 skipped，
// 避免取消/超时后步骤永远停在 running/pending。
func (m *Manager) settleUnfinishedSteps(ctx context.Context, lr *liveRun) {
	lr.mu.Lock()
	var updates []domain.Step
	for i := range lr.run.Steps {
		switch lr.run.Steps[i].Status {
		case domain.StepRunning:
			lr.run.Steps[i].Status = domain.StepCanceled
			updates = append(updates, lr.run.Steps[i])
		case domain.StepPending:
			lr.run.Steps[i].Status = domain.StepSkipped
			updates = append(updates, lr.run.Steps[i])
		}
	}
	lr.mu.Unlock()
	dctx, dcancel := detached(ctx)
	defer dcancel()
	for _, st := range updates {
		_ = m.st.UpsertStep(dctx, lr.run.ID, st)
	}
}

func (m *Manager) finish(ctx context.Context, lr *liveRun, status domain.RunStatus, prob *domain.Problem) {
	now := time.Now().UTC()
	lr.mu.Lock()
	lr.run.Status = status
	lr.run.FinishedAt = &now
	lr.run.Error = prob
	// 任何终态都应收口：进度 100%，StepsDone 只统计真正结束的 step
	lr.run.Progress.Percent = 100
	lr.run.Progress.StepsDone = countDone(lr.run.Steps)
	arts := lr.run.Artifacts
	summary := lr.run.Summary
	gate := lr.run.GateResult
	lr.mu.Unlock()

	dctx, dcancel := detached(ctx)
	defer dcancel()
	_ = m.st.UpdateRun(dctx, lr.run.ID, store.RunState{
		Status: status, StepsTotal: lr.run.Progress.StepsTotal, StepsDone: lr.run.Progress.StepsDone,
		GateResult: gate, Summary: summary, Error: prob,
		StartedAt: lr.run.StartedAt, FinishedAt: &now,
	})

	lr.mu.Lock()
	lr.run.Progress.Current = nil
	lr.mu.Unlock()
	m.emit(ctx, lr.run.ID, EvFinished, map[string]any{
		"status": string(status), "finished_at": now, "artifacts": arts,
	})
	m.log.Info("run finished", "run_id", lr.run.ID, "status", status,
		"artifacts", len(arts), "duration", now.Sub(lr.started).Round(time.Second))

	m.mu.Lock()
	delete(m.live, lr.run.ID)
	m.mu.Unlock()
}

func (m *Manager) setStatus(ctx context.Context, lr *liveRun, st domain.RunStatus) {
	lr.mu.Lock()
	lr.run.Status = st
	lr.run.Progress.Phase = string(st)
	total := lr.run.Progress.StepsTotal
	done := lr.run.Progress.StepsDone
	lr.mu.Unlock()
	dctx, dcancel := detached(ctx)
	_ = m.st.UpdateRun(dctx, lr.run.ID, store.RunState{
		Status: st, StepsTotal: total, StepsDone: done, StartedAt: lr.run.StartedAt,
	})
	dcancel()
}

// emit 落库（除 run.log）并广播。
func (m *Manager) emit(ctx context.Context, runID string, name EventName, data any) {
	raw, err := json.Marshal(data)
	if err != nil {
		return
	}
	m.mu.Lock()
	m.seq[runID]++
	seq := m.seq[runID]
	m.mu.Unlock()

	ev := Event{Seq: seq, Event: name, Data: raw, At: time.Now().UTC()}
	if name != EvLog {
		pctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = m.st.AppendEvent(pctx, runID, seq, string(name), raw, ev.At)
		cancel()
	}
	m.hub.publish(runID, ev)
}

// detached 让收尾阶段的落库不受 run 取消影响
// （否则 cancel 之后 UpdateRun 会因为 ctx 已取消而静默失败，状态卡在 running）。
func detached(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
}

// ---------------- 辅助 ----------------

func (m *Manager) liveRun(runID string) *liveRun {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.live[runID]
}

func (m *Manager) targetSemaphore(targetID string) chan struct{} {
	m.targetMu.Lock()
	defer m.targetMu.Unlock()
	if targetID == "" {
		targetID = "_ephemeral"
	}
	s, ok := m.targetSem[targetID]
	if !ok {
		n := max(1, m.cfg.Limits.MaxParallelPerTarget)
		s = make(chan struct{}, n)
		m.targetSem[targetID] = s
	}
	return s
}

func (m *Manager) targetKey(ctx context.Context, run *domain.Run) (string, error) {
	if run.Target.TargetID == "" {
		return "", nil
	}
	t, err := m.st.GetTarget(ctx, run.Target.TargetID)
	if err != nil {
		// target 被删：允许用空 key 继续（可能是内网免鉴权端点）
		return "", nil
	}
	return m.box.Decrypt(t.APIKeyEnc)
}

func (m *Manager) tokenizerOf(ctx context.Context, run *domain.Run) string {
	// 优先用 Target 上已固定下来的 tokenizer（创建时解析：显式指定或自动推断）。
	// 不能每次按 model 名重推，否则显式设置的 tokenizer 会被忽略（如 GLM-5.3 被推成 GLM-4.6）。
	if run.Target.TargetID != "" {
		if t, err := m.st.GetTarget(ctx, run.Target.TargetID); err == nil && t.Tokenizer != nil && t.Tokenizer.Value != "" {
			return t.Tokenizer.Value
		}
	}
	perf := m.perfCapability(run.Tool)
	if perf == nil {
		return ""
	}
	for _, r := range perf.Defaults.TokenizerMap {
		if r.Hits(run.Target.Model) {
			return r.Tokenizer
		}
	}
	return ""
}

func (m *Manager) maxTokensOf(run *domain.Run) int {
	if run.Plan.Request.MaxTokens > 0 {
		return run.Plan.Request.MaxTokens
	}
	if perf := m.perfCapability(run.Tool); perf != nil && perf.Defaults != nil {
		return perf.Defaults.MaxTokens
	}
	return 4096
}

// profileOf 返回该 run 使用的 SLA 阈值档（供报告逐行回填）。
func (m *Manager) profileOf(run *domain.Run) *config.GateProfile {
	if run.Gate == nil || run.Gate.Profile == "" {
		return m.cfg.Gates.Profiles[m.cfg.Gates.DefaultProfile]
	}
	return m.cfg.Gates.Profiles[run.Gate.Profile]
}

func (m *Manager) perfCapability(tool string) *config.Capability {
	eng := m.cfg.Engines[tool]
	if eng == nil {
		return nil
	}
	return eng.Capabilities["perf"]
}

func (m *Manager) buckets() []domain.Bucket {
	for _, eng := range m.cfg.Engines {
		if p := eng.Capabilities["perf"]; p != nil {
			out := make([]domain.Bucket, 0, len(p.Buckets))
			for _, b := range p.Buckets {
				builtIn := true
				if b.DatasetBuiltIn != nil {
					builtIn = *b.DatasetBuiltIn
				}
				bk := domain.Bucket{Label: b.Label, MaxParallel: b.MaxParallel, BuiltIn: builtIn}
				bk.PromptTokens.Min, bk.PromptTokens.Max = b.Min, b.Max
				out = append(out, bk)
			}
			return out
		}
	}
	return nil
}

func (m *Manager) normalizer(tool string, cap domain.Capability) (domain.Normalizer, error) {
	eng := m.cfg.Engines[tool]
	if eng == nil {
		return domain.Normalizer{}, fmt.Errorf("未知工具 %q", tool)
	}
	if cap != domain.CapPerf {
		// 其它能力的 plan 结构不同，v1 只归一化 perf
		return domain.Normalizer{}, nil
	}
	p := eng.Capabilities["perf"]
	if p == nil {
		return domain.Normalizer{}, fmt.Errorf("工具 %q 未配置 perf 能力", tool)
	}
	n := domain.Normalizer{
		Buckets:               m.buckets(),
		DefaultLadder:         p.Matrix.DefaultLadder,
		NumberByParallel:      p.NumberByParallel,
		ExtraParallels:        p.ExtraParallels,
		DefaultNumber:         derefInt(p.DefaultNumber),
		DefaultCooldownSec:    int(p.Matrix.Cooldown.Seconds()),
		DefaultSuccessRateMin: p.Matrix.SuccessRateMin,
		DefaultStopOnFailure:  p.Matrix.StopOnFailure == nil || *p.Matrix.StopOnFailure,
	}
	return n, nil
}

// normalizePlan 填充默认值并做上下限校验。
func (m *Manager) normalizePlan(p domain.Plan, n domain.Normalizer) (domain.Plan, error) {
	if p.Mode == "" {
		if len(p.Buckets) > 1 || (len(p.ParallelLadder) == 0 && p.Number == 0) {
			p.Mode = "matrix"
		} else {
			p.Mode = "single"
		}
	}
	if p.Mode != "single" && p.Mode != "matrix" {
		return p, invalid("plan.mode 必须是 single | matrix（当前 %q）", p.Mode)
	}
	if len(p.Buckets) == 0 {
		if p.Mode == "single" {
			return p, invalid("plan.buckets 不能为空")
		}
		p.Buckets = defaultMatrixBuckets(n)
	}
	if len(p.Buckets) > m.cfg.Limits.Plan.MaxBuckets {
		return p, invalid("档位数量 %d 超过上限 %d", len(p.Buckets), m.cfg.Limits.Plan.MaxBuckets)
	}
	for _, b := range p.Buckets {
		if _, ok := n.Bucket(b); !ok {
			return p, invalid("未知档位 %q（可用见 /v1/catalog/buckets）", b)
		}
	}
	if len(p.ParallelLadder) == 0 {
		if p.Mode == "single" {
			if p.Number == 0 {
				p.Number = 500
			}
			p.ParallelLadder = []int{10}
		} else {
			p.ParallelLadder = n.DefaultLadder
		}
	}
	for _, v := range p.ParallelLadder {
		if v <= 0 || v > m.cfg.Limits.Plan.MaxParallel {
			return p, invalid("并发 %d 非法（1..%d）", v, m.cfg.Limits.Plan.MaxParallel)
		}
	}
	if p.Mode == "single" && p.Number <= 0 {
		p.Number = 500
	}
	if p.Number > m.cfg.Limits.Plan.MaxNumber {
		return p, invalid("number %d 超过上限 %d", p.Number, m.cfg.Limits.Plan.MaxNumber)
	}
	if p.TPMBudget < 0 || p.TPMBudget > m.cfg.Limits.Plan.MaxTPMBudget {
		return p, invalid("tpm_budget 非法（0..%d）", m.cfg.Limits.Plan.MaxTPMBudget)
	}
	if p.SuccessRateMin < 0 || p.SuccessRateMin > 1 {
		return p, invalid("success_rate_min 必须是 0~1 的比值（当前 %v）", p.SuccessRateMin)
	}
	if p.CooldownSec == 0 {
		p.CooldownSec = n.DefaultCooldownSec
	}
	if p.StopOnFailure == nil {
		p.StopOnFailure = &n.DefaultStopOnFailure
	}
	if p.DatasetDir == "" {
		p.DatasetDir = "auto"
	}
	return p, nil
}

func defaultMatrixBuckets(n domain.Normalizer) []string {
	var out []string
	for _, b := range n.Buckets {
		if b.BuiltIn {
			out = append(out, b.Label)
		}
	}
	return out
}

func normalizeReport(r domain.ReportSpec, cfg *config.Config) domain.ReportSpec {
	if len(r.Formats) == 0 {
		r.Formats = cfg.Reports.DefaultFormats
	}
	if r.ClientName == "" {
		r.ClientName = cfg.Reports.DefaultCompany
	}
	return r
}

func (m *Manager) reportTitle(run *domain.Run, steps []domain.Step) string {
	if run.Report.Title != "" {
		return run.Report.Title
	}
	tpl := m.cfg.Reports.Title.Matrix
	bucket := ""
	if run.Plan.Mode == "single" && len(run.Plan.Buckets) > 0 {
		tpl = m.cfg.Reports.Title.Single
		bucket = run.Plan.Buckets[0]
	}
	r := stringsReplacer(map[string]string{
		"{model}": run.Target.Model, "{bucket}": bucket,
		"{ts}":     run.CreatedAt.Local().Format("20060102-1504"),
		"{vendor}": run.Target.Vendor, "{company}": run.Report.ClientName,
	})
	return r.Replace(tpl)
}

func planScope(p domain.Plan) string {
	if p.Mode == "single" {
		if len(p.Buckets) > 0 {
			return "单档 " + p.Buckets[0]
		}
		return "单档"
	}
	return fmt.Sprintf("全档位 %d 档", len(p.Buckets))
}

func collectFailures(res engine.Result, steps []domain.Step, excerpt int) []domain.FailureItem {
	var out []domain.FailureItem
	for idx, dbPath := range res.DBPaths {
		var st domain.Step
		st.Index = idx
		for _, s := range steps {
			if s.Index == idx {
				st = s
				break
			}
		}
		items, err := report.LoadFailures(dbPath, st, excerpt)
		if err == nil {
			out = append(out, items...)
		}
	}
	return out
}

func hasFailedStep(lr *liveRun) bool {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	for _, s := range lr.run.Steps {
		if s.Status == domain.StepFailed {
			return true
		}
	}
	return false
}

// countDone 统计「真正执行过」的 step：skipped（未开始）不计入。
func countDone(steps []domain.Step) int {
	n := 0
	for _, s := range steps {
		if s.Status == domain.StepSucceeded || s.Status == domain.StepFailed || s.Status == domain.StepCanceled {
			n++
		}
	}
	return n
}

func newID(prefix string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return prefix + "_" + hex.EncodeToString(b)
}

func asInt(v any) int {
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	case json.Number:
		n, _ := x.Int64()
		return int(n)
	}
	return 0
}

func asFloat(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case json.Number:
		f, _ := x.Float64()
		return f
	}
	return 0
}

func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

func derefInt(p *int) int {
	if p == nil {
		return 100
	}
	return *p
}

func sha256Hex(b []byte) string {
	h := sha256Sum(b)
	return hex.EncodeToString(h)
}

func stringsReplacer(pairs map[string]string) *strings.Replacer {
	oldnew := make([]string, 0, len(pairs)*2)
	keys := make([]string, 0, len(pairs))
	for k := range pairs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		oldnew = append(oldnew, k, pairs[k])
	}
	return strings.NewReplacer(oldnew...)
}
