package engine

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/apexcloud/thvv/server/internal/config"
	"github.com/apexcloud/thvv/server/internal/domain"
)

// CacheEngine 驱动 lmcache bench engine（kind=subprocess）。
//
// 与 perf/eval 的差异：
//   - 凭据只经 OPENAI_API_KEY 环境变量注入（工具不认 argv key）
//   - 一个 workload 一次子进程，Step = workload 执行
//   - 结果以 bench_summary.json + bench_results.csv 落盘为准
type CacheEngine struct {
	name      string
	cfg       *config.Engine
	capCfg    *config.Capability
	home      string
	cli       string // lmcache CLI（.venv/bin/lmcache）
	python    string
	pythonDir string
	script    string
	workdir   string
	env       map[string]string
	killGrace time.Duration
}

// NewCache 构造 lmcache 引擎。
func NewCache(name string, eng *config.Engine, cap *config.Capability, home string) *CacheEngine {
	rt := cap.Runtime
	if rt == nil {
		rt = eng.Runtime
	}
	e := &CacheEngine{name: name, cfg: eng, capCfg: cap, home: home, killGrace: 10 * time.Second}
	e.script = string(cap.Script)
	e.workdir = string(cap.WorkingDir)
	if rt != nil {
		e.python = string(rt.Python)
		e.cli = string(rt.Evalscope) // 复用字段：lmcache 引擎里表示 CLI 入口
		e.env = rt.Env
	}
	if e.cli == "" && e.python != "" {
		e.cli = filepath.Join(filepath.Dir(e.python), "lmcache")
	}
	if e.python != "" {
		e.pythonDir = filepath.Dir(e.python)
	}
	return e
}

func (e *CacheEngine) Name() string { return e.name }

func (e *CacheEngine) Describe() Description {
	d := Description{
		Name: e.name, DisplayName: e.cfg.DisplayName, Version: e.cfg.Version, Kind: "subprocess",
		Runtime: map[string]string{"cli": e.cli, "python": e.python, "home": e.home},
	}
	if d.DisplayName == "" {
		d.DisplayName = "LMCache 缓存测试"
	}
	if d.Version == "" {
		d.Version = "1.0.0"
	}
	for name, cap := range e.cfg.Capabilities {
		if cap == nil {
			continue
		}
		d.Capabilities = append(d.Capabilities, CapabilityDesc{
			Name: name, Status: cap.Status, Description: cap.Description, AvailableAt: cap.AvailableAt,
			Artifacts: []string{"bench_csv", "bench_summary", "bench_config", "run_log"},
		})
	}
	return d
}

// Validate 校验 plan。
func (e *CacheEngine) Validate(cap domain.Capability, plan domain.Plan) error {
	if cap != domain.CapCache {
		return fmt.Errorf("capability %s 不是 cache", cap)
	}
	if plan.Workload != "" && plan.Workload != "multi-round-chat" {
		return fmt.Errorf("v1 仅支持 multi-round-chat workload（当前 %q）", plan.Workload)
	}
	return nil
}

// Probe 环境自检。
func (e *CacheEngine) Probe(ctx context.Context, t ResolvedTarget) ProbeReport {
	start := time.Now()
	rep := ProbeReport{
		TargetID: t.TargetID, CheckedAt: start.UTC(),
		Engine: map[string]string{"name": e.name, "kind": "subprocess", "cli": e.cli},
	}
	rep.Checks = append(rep.Checks, CheckItem{Name: "protocol", OK: true, Detail: string(t.Protocol)})

	// lmcache CLI 可用性（v0.5.7 无 --version 子命令，用 --help 退出码判定 + 横幅提取版本）
	verCmd := exec.CommandContext(ctx, e.cli, "--help")
	verCmd.Env = e.childEnv("")
	if out, err := verCmd.CombinedOutput(); err == nil {
		v := "lmcache CLI 可用"
		for _, ln := range strings.Split(string(out), "\n") {
			if strings.Contains(ln, "LMCache v") {
				if i := strings.Index(ln, "LMCache v"); i >= 0 {
					v = strings.TrimSpace(ln[i:])
				}
				break
			}
		}
		rep.Engine["lmcache_version"] = v
		rep.Checks = append(rep.Checks, CheckItem{Name: "engine", OK: true, Detail: v})
	} else {
		rep.Checks = append(rep.Checks, CheckItem{Name: "engine", OK: false, Severity: "error",
			Detail: fmt.Sprintf("执行 %s --help 失败: %v", e.cli, err)})
	}

	// bench engine 子命令
	benchCmd := exec.CommandContext(ctx, e.cli, "bench", "engine", "--help")
	benchCmd.Env = e.childEnv("")
	if err := benchCmd.Run(); err == nil {
		rep.Checks = append(rep.Checks, CheckItem{Name: "script", OK: true, Detail: "bench engine 子命令可用"})
	} else {
		rep.Checks = append(rep.Checks, CheckItem{Name: "script", OK: false, Severity: "error",
			Detail: "lmcache bench engine 不可用（检查 venv 与 openai 依赖）"})
	}

	// 端点连通性
	if lat, err := pingTarget(ctx, t); err != nil {
		rep.Checks = append(rep.Checks, CheckItem{Name: "connectivity", OK: false, Severity: "error", Detail: err.Error()})
	} else {
		rep.Checks = append(rep.Checks, CheckItem{Name: "connectivity", OK: true, LatencyMS: lat,
			Detail: fmt.Sprintf("HTTP 200，首 token %dms", lat)})
	}

	rep.OK = true
	for _, c := range rep.Checks {
		if !c.OK && c.Severity != "warn" {
			rep.OK = false
		}
	}
	rep.ElapsedMS = time.Since(start).Milliseconds()
	return rep
}

// benchPhaseRe 匹配工具的关键阶段行（预热/运行中/CSV 落盘），用于进度事件。
var benchPhaseRe = regexp.MustCompile(`(?i)(warmup|benchmark|CSV results written|JSON summary written|Workload:)`)

// tierPlan 返回第 idx 档的生效 plan：容量 / QPS 用 ladder 里规范化后的值。
func (e *CacheEngine) tierPlan(base domain.Plan, idx int) domain.Plan {
	p := base
	if len(p.CacheLadder) == 0 || idx < 0 || idx >= len(p.CacheLadder) {
		return p
	}
	t := p.CacheLadder[idx]
	if t.KVCacheVolumeGB > 0 {
		p.KVCacheVolumeGB = t.KVCacheVolumeGB
	}
	if t.QPS > 0 {
		p.QPS = t.QPS
	}
	return p
}

// tierResult 是单档执行结果。
type tierResult struct {
	step    domain.Step
	metrics *domain.CacheMetrics
	note    string
	files   []FileArtifact
}

// attachProbe 把前缀命中率探针结果挂到汇总指标上（探针整 run 只跑一次）。
func attachProbe(m *domain.CacheMetrics, rounds []domain.CacheProbeRound) {
	if m == nil || len(rounds) == 0 {
		return
	}
	m.ProbeRounds = rounds
	if hr, reported := DeriveProbeHitRate(rounds); len(rounds) > 1 {
		m.PrefixHitRate = &hr
		m.CacheFieldReported = reported
	}
}

// aggregateCacheTiers 把多档结果聚合成顶层指标（明细留在 Tiers）。
//
// 顶层只做「可加」的聚合：请求数 / token 数 / 时长 / 成功数；
// 均值按样本加权（TTFT 按成功请求、decode speed 按输出 token），吞吐按总 token / 总时长。
// 分位数无法跨档合并，只在各档 Tiers 内保留。
func aggregateCacheTiers(tiers []*domain.CacheMetrics) *domain.CacheMetrics {
	agg := &domain.CacheMetrics{Workload: "multi-round-chat"}
	var ttftWeighted, dsWeighted float64
	for _, t := range tiers {
		if t == nil {
			continue
		}
		agg.TotalRequests += t.TotalRequests
		agg.SuccessReqs += t.SuccessReqs
		agg.FailedReqs += t.FailedReqs
		agg.InputTokens += t.InputTokens
		agg.OutputTokens += t.OutputTokens
		agg.ElapsedSec += t.ElapsedSec
		ttftWeighted += t.MeanTTFTMS * float64(t.SuccessReqs)
		dsWeighted += t.MeanDecodeSpeed * float64(t.OutputTokens)
		if agg.Model == "" {
			agg.Model = t.Model
		}
	}
	if agg.TotalRequests > 0 {
		agg.SuccessRate = float64(agg.SuccessReqs) / float64(agg.TotalRequests)
	}
	if agg.SuccessReqs > 0 {
		agg.MeanTTFTMS = ttftWeighted / float64(agg.SuccessReqs)
	}
	if agg.OutputTokens > 0 && dsWeighted > 0 {
		agg.MeanDecodeSpeed = dsWeighted / float64(agg.OutputTokens)
		agg.TPOTMeanMS = 1000 / agg.MeanDecodeSpeed
	}
	if agg.ElapsedSec > 0 {
		agg.InputThroughput = float64(agg.InputTokens) / agg.ElapsedSec
		agg.OutputThroughput = float64(agg.OutputTokens) / agg.ElapsedSec
	}
	return agg
}

// Run 执行 multi-round-chat：plan.ladder 为空时单档，否则按并发档位顺序执行多档。
func (e *CacheEngine) Run(ctx context.Context, spec Spec, emit func(Event)) (Result, error) {
	res := Result{DBPaths: map[int]string{}}
	res.LogPath = filepath.Join(spec.WorkDir, "logs", fmt.Sprintf("%s-cache.log", spec.RunID))
	if err := os.MkdirAll(filepath.Dir(res.LogPath), 0o755); err != nil {
		return res, err
	}
	logFile, err := os.Create(res.LogPath)
	if err != nil {
		return res, err
	}
	defer logFile.Close()

	cacheRoot := filepath.Join(spec.WorkDir, "cache")
	if err := os.MkdirAll(cacheRoot, 0o755); err != nil {
		return res, err
	}

	// 前缀命中率探针：整 run 只跑一次（在全部档位之前），抓原始 usage（含 cached_tokens）。
	// lmcache bench 的 CSV 不含该字段；探针失败不阻断 bench。
	probeRounds, probeErr := e.probePrefixCache(ctx, spec, cacheRoot)
	for _, pr := range probeRounds {
		fmt.Fprintf(logFile, "[probe] round=%d prompt=%d cached=%d hit=%.1f%% ttft=%.0fms\n",
			pr.Round, pr.PromptTokens, pr.CachedTokens, pr.HitRate*100, pr.ReqMS)
	}
	if probeErr != nil {
		fmt.Fprintf(logFile, "[probe] 探针未完成: %v（不影响 bench）\n", probeErr)
	}

	var (
		tiers []*domain.CacheMetrics
		files []FileArtifact
		notes []string
	)
	for i, st := range spec.Steps {
		if ctx.Err() != nil {
			break
		}
		tp := e.tierPlan(spec.Plan, i)
		tierSpec := spec
		tierSpec.Plan = tp

		tr, err := e.runTier(ctx, tierSpec, st, logFile, emit)
		res.Steps = append(res.Steps, tr.step)
		if tr.metrics != nil {
			tr.metrics.Bucket = st.Bucket
			tr.metrics.Sessions = st.Parallel
			tr.metrics.QPS = tp.QPS
			tiers = append(tiers, tr.metrics)
		}
		files = append(files, tr.files...)
		if tr.note != "" {
			notes = append(notes, fmt.Sprintf("%s: %s", st.Bucket, tr.note))
		}
		if err != nil {
			// 启动失败等致命错误：已完成档位的结果仍归档，由 runmgr 记录失败原因。
			res.Files = files
			res.Cache = aggregateCacheTiers(tiers)
			if len(tiers) > 0 {
				res.Cache.Tiers = tiers
			}
			return res, err
		}
		if ctx.Err() != nil {
			break
		}
	}

	// 探针原始文件归档（文件名保持与 v1 一致）。
	if p := filepath.Join(cacheRoot, "prefix_usage.csv"); fileExists(p) {
		files = append(files, FileArtifact{
			Kind: domain.ArtBenchCSV, Filename: "multi-round-chat-prefix_usage.csv",
			Path: p, ContentType: "text/csv; charset=utf-8",
		})
	}
	res.Files = files

	switch {
	case len(tiers) == 1 && len(spec.Steps) == 1:
		m := tiers[0]
		attachProbe(m, probeRounds)
		res.Cache = m
	case len(tiers) > 0:
		agg := aggregateCacheTiers(tiers)
		attachProbe(agg, probeRounds)
		agg.Tiers = tiers
		res.Cache = agg
	default:
		res.Cache = &domain.CacheMetrics{Workload: spec.Plan.Workload}
	}
	if len(notes) > 0 {
		res.CacheNote = strings.Join(notes, "；")
	}
	if ctx.Err() != nil {
		// 返回 ctx.Err() 而不是固定 context.Canceled：runmgr 要靠它区分
		// 用户取消（Canceled）与单次运行超时（DeadlineExceeded）。
		return res, ctx.Err()
	}
	return res, nil
}

// runTier 执行单个档位：一个 lmcache 子进程 + 独立输出目录。
func (e *CacheEngine) runTier(ctx context.Context, spec Spec, st domain.StepSpec, logFile *os.File, emit func(Event)) (tierResult, error) {
	tr := tierResult{}
	outDir := filepath.Join(spec.WorkDir, "cache", sanitizeDirName(st.Bucket))
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		tr.step = domain.Step{Index: st.Index, Bucket: st.Bucket, Status: domain.StepFailed}
		tr.note = err.Error()
		return tr, nil
	}

	emit(Event{Kind: EvStepStarted, At: time.Now().UTC(), Data: map[string]any{
		"step_index": st.Index, "bucket": st.Bucket, "parallel": st.Parallel, "number": 0,
	}})

	args := e.benchArgs(spec, outDir)
	cmd := exec.CommandContext(ctx, e.cli, args...)
	cmd.Dir = e.workdir
	cmd.Env = e.childEnv(spec.Target.APIKey)
	setPgid(cmd)

	fmt.Fprintf(logFile, "\n=== workload: %s（sessions≈%d, qps=%g） ===\n$ %s %s\n",
		st.Bucket, st.Parallel, spec.Plan.QPS, e.cli, strings.Join(redactArgs(args), " "))

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		tr.step = domain.Step{Index: st.Index, Bucket: st.Bucket, Status: domain.StepFailed}
		tr.note = err.Error()
		return tr, err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		emit(Event{Kind: EvError, At: time.Now().UTC(), Data: map[string]any{
			"code": "engine_failed", "detail": err.Error()}})
		tr.step = domain.Step{Index: st.Index, Bucket: st.Bucket, Status: domain.StepFailed}
		tr.note = err.Error()
		return tr, err
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 64*1024), 2*1024*1024)
		for sc.Scan() {
			line := redactSecrets(sc.Text(), spec.Target.APIKey)
			fmt.Fprintln(logFile, line)
			emit(Event{Kind: EvLog, At: time.Now().UTC(), Data: map[string]any{"line": line}})
			if benchPhaseRe.MatchString(line) {
				emit(Event{Kind: EvStepProgress, At: time.Now().UTC(), Data: map[string]any{
					"step_index": st.Index, "completed": 50, "total": 100, "phase": phaseOf(line),
				}})
			}
		}
	}()

	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()

	var runErr error
	select {
	case <-ctx.Done():
		killTree(cmd, true)
		select {
		case <-waitErr:
		case <-time.After(e.killGrace):
			killTree(cmd, false)
			<-waitErr
		}
		wg.Wait()
		tr.step = domain.Step{Index: st.Index, Bucket: st.Bucket, Status: domain.StepCanceled}
		tr.note = "已取消"
		return tr, nil
	case err := <-waitErr:
		wg.Wait()
		runErr = err
	}

	// 导出最终生效参数（幂等：失败也不影响已有产物）
	if cfgArgs := e.exportArgs(spec, outDir); cfgArgs != nil {
		exp := exec.Command(e.cli, cfgArgs...)
		exp.Env = e.childEnv(spec.Target.APIKey)
		_ = exp.Run()
	}

	metrics, ferr := readBenchResults(outDir, nil)
	status := domain.StepSucceeded
	errMsg := ""
	if ferr != nil {
		status = domain.StepFailed
		errMsg = ferr.Error()
		if runErr != nil {
			errMsg = fmt.Sprintf("%s（退出码 %d）", errMsg, exitCode(runErr))
		}
	} else if metrics.TotalRequests > 0 && metrics.FailedReqs == metrics.TotalRequests {
		status = domain.StepFailed
		errMsg = "全部请求失败（详见 run.log）"
	}
	sr := 0.0
	if metrics.TotalRequests > 0 {
		sr = float64(metrics.SuccessReqs) / float64(metrics.TotalRequests)
	}
	dur := metrics.ElapsedSec
	tr.step = domain.Step{
		Index: st.Index, Bucket: st.Bucket, Parallel: metrics.NumSessions, Number: metrics.TotalRequests,
		Status: status, SuccessRate: &sr, DurationSec: &dur,
	}
	tr.metrics = metrics
	tr.note = errMsg
	tr.files = collectCacheFiles(st.Bucket, outDir)

	var steadyP50 float64
	if metrics.SteadyState != nil {
		steadyP50 = metrics.SteadyState.TTFTp50MS
	}
	emit(Event{Kind: EvStepFinished, At: time.Now().UTC(), Data: map[string]any{
		"step_index": st.Index, "status": string(status), "success_rate": sr,
		"ttft_steady_p50_ms": steadyP50, "duration_sec": dur,
	}})
	return tr, nil
}

// benchArgs 构造 lmcache bench engine 的 argv。
func (e *CacheEngine) benchArgs(spec Spec, outDir string) []string {
	p := spec.Plan
	args := []string{
		"bench", "engine",
		// lmcache 会给不以 /v1 结尾的 URL 追加 /v1，必须传剥掉请求路径后的 base
		"--engine-url", cacheBaseURL(spec.Target.APIURL),
		"--model", spec.Target.Model,
		"--workload", "multi-round-chat",
		"--output-dir", outDir,
		"--no-interactive", "-q", "--json",
	}
	args = append(args,
		"--kv-cache-volume", strconv.FormatFloat(p.KVCacheVolumeGB, 'f', -1, 64),
		"--tokens-per-gb-kvcache", strconv.Itoa(p.TokensPerGBKV),
	)
	if p.DurationSec > 0 {
		args = append(args, "--mrc-duration", strconv.Itoa(p.DurationSec))
	}
	if p.QPS > 0 {
		args = append(args, "--mrc-qps", strconv.FormatFloat(p.QPS, 'f', -1, 64))
	}
	if p.SharedPromptLen > 0 {
		args = append(args, "--mrc-shared-prompt-length", strconv.Itoa(p.SharedPromptLen))
	}
	if p.ChatHistoryLen > 0 {
		args = append(args, "--mrc-chat-history-length", strconv.Itoa(p.ChatHistoryLen))
	}
	if p.UserInputLen > 0 {
		args = append(args, "--mrc-user-input-length", strconv.Itoa(p.UserInputLen))
	}
	if p.OutputLen > 0 {
		args = append(args, "--mrc-output-length", strconv.Itoa(p.OutputLen))
	}
	if p.Seed != nil {
		args = append(args, "--seed", strconv.Itoa(*p.Seed))
	}
	if p.NoWarmup {
		args = append(args, "--no-warmup")
	}
	if p.IgnoreEOS {
		args = append(args, "--ignore-eos")
	}
	return args
}

// exportArgs 生成 --export-config 的 argv（落该档目录的 resolved_config.json，不实际发请求）。
func (e *CacheEngine) exportArgs(spec Spec, outDir string) []string {
	args := e.benchArgs(spec, "")
	if len(args) < 2 {
		return nil
	}
	// 去掉 --output-dir <dir> 与 -q/--json/--no-interactive（export 不需要）
	out := []string{"bench", "engine"}
	skip := map[string]bool{"--output-dir": true, "-q": true, "--json": true, "--no-interactive": true, "--quiet": true}
	for i := 2; i < len(args); i++ {
		if skip[args[i]] {
			continue
		}
		out = append(out, args[i])
	}
	out = append(out, "--export-config", filepath.Join(outDir, "resolved_config.json"))
	return out
}

func (e *CacheEngine) childEnv(apiKey string) []string {
	env := os.Environ()
	for k, v := range e.env {
		if k == "OPENAI_API_KEY" && apiKey != "" {
			continue // 凭据以 run 的 target 为准
		}
		env = append(env, k+"="+v)
	}
	if apiKey != "" {
		env = append(env, "OPENAI_API_KEY="+apiKey)
	}
	// PATH 前置 venv/bin（lmcache 内部可能 fork python）
	paths := []string{}
	for _, d := range []string{e.pythonDir, filepath.Dir(e.cli)} {
		if d != "" && d != "." {
			paths = append(paths, d)
		}
	}
	if cur := os.Getenv("PATH"); cur != "" {
		paths = append(paths, cur)
	}
	if len(paths) > 0 {
		env = append(env, "PATH="+strings.Join(paths, string(os.PathListSeparator)))
	}
	env = append(env, "PYTHONUNBUFFERED=1", "LMCACHE_DISABLE_BANNER=1")
	return env
}

// cacheBaseURL 剥掉完整请求路径后缀，得到 lmcache 期望的 base（结尾 /v1）。
func cacheBaseURL(apiURL string) string {
	u := strings.TrimRight(apiURL, "/")
	u = strings.TrimSuffix(u, "/chat/completions")
	u = strings.TrimSuffix(u, "/v1/messages")
	return u
}

func phaseOf(line string) string {
	switch {
	case strings.Contains(strings.ToLower(line), "warmup"):
		return "warmup"
	case strings.Contains(line, "Workload:"):
		return "started"
	case strings.Contains(line, "CSV results written"), strings.Contains(line, "JSON summary written"):
		return "exporting"
	default:
		return "running"
	}
}

// benchSummaryFile 是 bench_summary.json 的 results 部分。
type benchSummaryFile struct {
	Config struct {
		Workload string  `json:"workload"`
		Model    string  `json:"model"`
		Duration float64 `json:"mrc_duration"`
	} `json:"config"`
	Results struct {
		TotalRequests      int     `json:"total_requests"`
		SuccessfulRequests int     `json:"successful_requests"`
		FailedRequests     int     `json:"failed_requests"`
		ElapsedTime        float64 `json:"elapsed_time"`
		MeanTTFTMS         float64 `json:"mean_ttft_ms"`
		P50TTFTMS          float64 `json:"p50_ttft_ms"`
		P90TTFTMS          float64 `json:"p90_ttft_ms"`
		P99TTFTMS          float64 `json:"p99_ttft_ms"`
		MeanDecodeSpeed    float64 `json:"mean_decode_speed"`
		P50DecodeSpeed     float64 `json:"p50_decode_speed"`
		P90DecodeSpeed     float64 `json:"p90_decode_speed"`
		InputThroughput    float64 `json:"input_throughput"`
		OutputThroughput   float64 `json:"output_throughput"`
		P50LatencyMS       float64 `json:"p50_request_latency_ms"`
		P90LatencyMS       float64 `json:"p90_request_latency_ms"`
		P99LatencyMS       float64 `json:"p99_request_latency_ms"`
		TotalInputTokens   int     `json:"total_input_tokens"`
		TotalOutputTokens  int     `json:"total_output_tokens"`
	} `json:"results"`
}

// readBenchResults 读 bench_summary.json + bench_results.csv，派生缓存指标。
// CSV 派生在 report 包实现（report.DeriveCacheMetrics），这里做 IO 与组装。
func readBenchResults(dir string, probeRounds []domain.CacheProbeRound) (*domain.CacheMetrics, error) {
	m := &domain.CacheMetrics{Workload: "multi-round-chat"}

	b, err := os.ReadFile(filepath.Join(dir, "bench_summary.json"))
	if err != nil {
		return m, fmt.Errorf("未生成 bench_summary.json（评测未跑完或启动失败，详见 run.log）")
	}
	var s benchSummaryFile
	if err := json.Unmarshal(b, &s); err != nil {
		return m, fmt.Errorf("bench_summary.json 解析失败: %w", err)
	}
	m.TotalRequests = s.Results.TotalRequests
	m.SuccessReqs = s.Results.SuccessfulRequests
	m.FailedReqs = s.Results.FailedRequests
	if s.Results.TotalRequests > 0 {
		m.SuccessRate = float64(s.Results.SuccessfulRequests) / float64(s.Results.TotalRequests)
	}
	m.ElapsedSec = s.Results.ElapsedTime
	m.MeanTTFTMS = s.Results.MeanTTFTMS
	m.DecodeSpeedP50 = s.Results.P50DecodeSpeed
	m.DecodeSpeedP90 = s.Results.P90DecodeSpeed
	m.InputThroughput = s.Results.InputThroughput
	m.OutputThroughput = s.Results.OutputThroughput
	m.MeanDecodeSpeed = s.Results.MeanDecodeSpeed
	if m.MeanDecodeSpeed > 0 {
		m.TPOTMeanMS = 1000 / m.MeanDecodeSpeed
	}
	m.LatencyP50MS = s.Results.P50LatencyMS
	m.LatencyP90MS = s.Results.P90LatencyMS
	m.LatencyP99MS = s.Results.P99LatencyMS
	m.InputTokens = s.Results.TotalInputTokens
	m.OutputTokens = s.Results.TotalOutputTokens
	m.Model = s.Config.Model

	// CSV 派生（首轮/稳态/逐轮）；CSV 缺失不致命，只缺派生指标
	if rounds, steady, first, err := DeriveCacheRounds(filepath.Join(dir, "bench_results.csv")); err == nil {
		m.Rounds = rounds
		m.SteadyState = steady
		m.FirstRound = first
	}
	m.NumSessions = countSessions(filepath.Join(dir, "bench_results.csv"))
	if probeRounds != nil {
		m.ProbeRounds = probeRounds
		if hr, reported := DeriveProbeHitRate(probeRounds); len(probeRounds) > 1 {
			m.PrefixHitRate = &hr
			m.CacheFieldReported = reported
		}
	}
	return m, nil
}

func countSessions(csvPath string) int {
	f, err := os.Open(csvPath)
	if err != nil {
		return 0
	}
	defer f.Close()
	seen := map[string]bool{}
	sc := bufio.NewScanner(f)
	sc.Scan() // header
	for sc.Scan() {
		var rid string
		if fields := strings.Split(sc.Text(), ","); len(fields) > 0 {
			rid = fields[0]
		}
		if i := strings.IndexByte(rid, '_'); i > 0 {
			seen[rid[:i]] = true
		}
	}
	return len(seen)
}

// DeriveCacheRounds 从 bench_results.csv 派生逐轮/首轮/稳态统计。
// 导出给 report 包做门禁判定与单测复用。
func DeriveCacheRounds(csvPath string) ([]domain.CacheRoundStats, *domain.CacheSteadyStats, *domain.CacheRoundStats, error) {
	f, err := os.Open(csvPath)
	if err != nil {
		return nil, nil, nil, err
	}
	defer f.Close()

	type row struct {
		rid         string
		ok          bool
		ttft        float64
		latency     float64
		input       float64
		output      float64
		decodeSpeed float64 // tokens/s（第 7 列）
	}
	var rows []row
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	sc.Scan() // header
	for sc.Scan() {
		parts := strings.Split(sc.Text(), ",")
		if len(parts) < 7 {
			continue
		}
		r := row{rid: parts[0]}
		r.ok = parts[1] == "True" || parts[1] == "true" || parts[1] == "1"
		if !r.ok {
			continue
		}
		if v, e := strconv.ParseFloat(parts[2], 64); e == nil {
			r.ttft = v
		}
		if v, e := strconv.ParseFloat(parts[3], 64); e == nil {
			r.latency = v
		}
		if v, e := strconv.ParseFloat(parts[4], 64); e == nil {
			r.input = v
		}
		if v, e := strconv.ParseFloat(parts[5], 64); e == nil {
			r.output = v
		}
		if v, e := strconv.ParseFloat(parts[6], 64); e == nil {
			r.decodeSpeed = v
		}
		rows = append(rows, r)
	}

	// 按 round 聚合
	byRound := map[int][]row{}
	for _, r := range rows {
		i := strings.LastIndexByte(r.rid, 'r')
		if i < 1 {
			continue
		}
		n, err := strconv.Atoi(r.rid[i+1:])
		if err != nil {
			continue
		}
		byRound[n] = append(byRound[n], r)
	}
	if len(byRound) == 0 {
		return nil, nil, nil, fmt.Errorf("CSV 无有效请求行")
	}

	mean := func(v []float64) float64 {
		if len(v) == 0 {
			return 0
		}
		s := 0.0
		for _, x := range v {
			s += x
		}
		return s / float64(len(v))
	}

	var rounds []domain.CacheRoundStats
	for n := 0; ; n++ {
		rs, ok := byRound[n]
		if !ok {
			break
		}
		ttfts := make([]float64, 0, len(rs))
		inputs := make([]float64, 0, len(rs))
		for _, r := range rs {
			ttfts = append(ttfts, r.ttft*1000)
			inputs = append(inputs, r.input)
		}
		rounds = append(rounds, domain.CacheRoundStats{
			Round: n, Requests: len(rs), TTFTMeanMS: mean(ttfts), InputTokensMean: mean(inputs),
		})
	}

	first := &domain.CacheRoundStats{}
	if len(rounds) > 0 {
		first = &rounds[0]
	}

	// 稳态 = 非首轮
	steady := &domain.CacheSteadyStats{}
	var sTTFT, sLat, sSpd, sTPOT []float64
	for n, rs := range byRound {
		if n == 0 {
			continue
		}
		for _, r := range rs {
			sTTFT = append(sTTFT, r.ttft*1000)
			sLat = append(sLat, r.latency*1000)
			// TPOT：优先用工具落的 decode_speed（tokens/s）；缺失时用
			// (request_latency - ttft) / (num_output_tokens - 1) 兜底。
			if r.decodeSpeed > 0 {
				sSpd = append(sSpd, r.decodeSpeed)
				sTPOT = append(sTPOT, 1000/r.decodeSpeed)
			} else if r.output > 1 && r.latency > r.ttft {
				sTPOT = append(sTPOT, (r.latency-r.ttft)*1000/(r.output-1))
			}
		}
	}
	steady.Requests = len(sTTFT)
	if len(sTTFT) > 0 {
		steady.TTFTp50MS = pctF(sTTFT, 50)
		steady.TTFTp90MS = pctF(sTTFT, 90)
		steady.TTFTp99MS = pctF(sTTFT, 99)
		steady.Latencyp50MS = pctF(sLat, 50)
		if len(sSpd) > 0 {
			steady.DecodeSpeedP50 = pctF(sSpd, 50)
		}
		if len(sTPOT) > 0 {
			steady.TPOTp50MS = pctF(sTPOT, 50)
			steady.TPOTp90MS = pctF(sTPOT, 90)
		}
		if first.TTFTMeanMS > 0 {
			steady.TTFTImprovement = 1 - steady.TTFTp50MS/first.TTFTMeanMS
		}
	}
	return rounds, steady, first, nil
}

func pctF(v []float64, p float64) float64 {
	if x, ok := percentileF(v, p); ok {
		return x
	}
	return 0
}

// collectCacheFiles 收集产物。
func collectCacheFiles(prefix, dir string) []FileArtifact {
	names := []struct {
		name string
		kind domain.ArtifactKind
		ct   string
	}{
		{"bench_results.csv", domain.ArtBenchCSV, "text/csv; charset=utf-8"},
		{"bench_summary.json", domain.ArtBenchSummary, "application/json"},
		{"resolved_config.json", domain.ArtBenchConfig, "application/json"},
	}
	var out []FileArtifact
	for _, c := range names {
		p := filepath.Join(dir, c.name)
		if st, err := os.Stat(p); err != nil || st.IsDir() {
			continue
		}
		out = append(out, FileArtifact{Kind: c.kind, Filename: prefix + "-" + c.name, Path: p, ContentType: c.ct})
	}
	return out
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
