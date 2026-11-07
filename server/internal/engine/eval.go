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

// EvalEngine 是效果评测引擎（kind=subprocess，驱动 thvv/eval/scripts/run_eval.py）。
//
// 与 perf 的差异：
//   - 每个数据集一个子进程（run_eval.py 的 --output_dir 是单目录，多个数据集会互相覆盖）
//   - 结果以 eval_summary.json 为准，退出码只作辅助
//   - Step = 数据集，产物是 eval_report_v2.html / CSV / tar.gz 等文件
type EvalEngine struct {
	name      string
	cfg       *config.Engine
	capCfg    *config.Capability
	home      string
	python    string
	evalscope string
	script    string
	workdir   string
	env       map[string]string
	pyPath    []string
	killGrace time.Duration
}

// NewEval 构造效果评测引擎。
func NewEval(name string, eng *config.Engine, cap *config.Capability, home string) *EvalEngine {
	rt := cap.Runtime
	if rt == nil {
		rt = eng.Runtime
	}
	e := &EvalEngine{name: name, cfg: eng, capCfg: cap, home: home, killGrace: 10 * time.Second}
	e.script = string(cap.Script)
	e.workdir = string(cap.WorkingDir)
	if rt != nil {
		e.python = string(rt.Python)
		e.evalscope = string(rt.Evalscope)
		e.env = rt.Env
		for _, p := range rt.PythonPathPrepend {
			e.pyPath = append(e.pyPath, string(p))
		}
	}
	return e
}

func (e *EvalEngine) Name() string { return e.name }

func (e *EvalEngine) Describe() Description {
	d := Description{
		Name: e.name, DisplayName: e.cfg.DisplayName, Version: e.cfg.Version, Kind: "subprocess",
		Runtime: map[string]string{"python": e.python, "evalscope": e.evalscope, "home": e.home, "script": e.script},
	}
	if d.DisplayName == "" {
		d.DisplayName = "THVV TokenHub Vendor Verifier"
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
			PlanSchema: cap.PlanSchema, ResultSchema: cap.ResultSchema,
			Artifacts: []string{"eval_report", "eval_summary", "eval_samples", "eval_archive", "run_log"},
		})
	}
	return d
}

// Validate 校验 plan（协议层已规范化，这里做引擎侧兜底）。
func (e *EvalEngine) Validate(cap domain.Capability, plan domain.Plan) error {
	if cap != domain.CapEval {
		return fmt.Errorf("capability %s 不是 eval", cap)
	}
	if len(plan.Datasets) == 0 {
		return fmt.Errorf("plan.datasets 不能为空")
	}
	return nil
}

// Probe 做环境 / 连通性 / Judge 自查。
func (e *EvalEngine) Probe(ctx context.Context, t ResolvedTarget) ProbeReport {
	start := time.Now()
	rep := ProbeReport{
		TargetID: t.TargetID, CheckedAt: start.UTC(),
		Engine: map[string]string{"name": e.name, "kind": "subprocess", "python": e.python, "evalscope": e.evalscope},
	}
	rep.Checks = append(rep.Checks, CheckItem{Name: "protocol", OK: true, Detail: string(t.Protocol)})

	// python / evalscope
	if out, err := exec.CommandContext(ctx, e.python, "-c", "import sys;print(sys.version.split()[0])").CombinedOutput(); err == nil {
		rep.Engine["python_version"] = strings.TrimSpace(string(out))
		rep.Checks = append(rep.Checks, CheckItem{Name: "python", OK: true, Detail: strings.TrimSpace(string(out))})
	} else {
		rep.Checks = append(rep.Checks, CheckItem{Name: "python", OK: false, Severity: "error",
			Detail: fmt.Sprintf("执行 %s 失败: %v", e.python, err)})
	}
	verCmd := exec.CommandContext(ctx, e.evalscope, "--version")
	verCmd.Env = e.childEnv()
	if out, err := verCmd.CombinedOutput(); err == nil {
		v := strings.TrimSpace(string(out))
		rep.Engine["evalscope_version"] = v
		rep.Checks = append(rep.Checks, CheckItem{Name: "engine", OK: true, Detail: v})
	} else {
		rep.Checks = append(rep.Checks, CheckItem{Name: "engine", OK: false, Severity: "error",
			Detail: fmt.Sprintf("执行 %s --version 失败: %v", e.evalscope, err)})
	}

	// 评测脚本
	if e.script == "" {
		rep.Checks = append(rep.Checks, CheckItem{Name: "script", OK: false, Severity: "error", Detail: "未配置 eval.script"})
	} else if st, err := os.Stat(e.script); err != nil || st.IsDir() {
		rep.Checks = append(rep.Checks, CheckItem{Name: "script", OK: false, Severity: "error",
			Detail: e.script + " 不存在"})
	} else {
		rep.Checks = append(rep.Checks, CheckItem{Name: "script", OK: true, Detail: filepath.Base(e.script)})
	}

	// 端点连通性
	if lat, err := pingTarget(ctx, t); err != nil {
		rep.Checks = append(rep.Checks, CheckItem{Name: "connectivity", OK: false, Severity: "error", Detail: err.Error()})
	} else {
		rep.Checks = append(rep.Checks, CheckItem{Name: "connectivity", OK: true, LatencyMS: lat,
			Detail: fmt.Sprintf("HTTP 200，首 token %dms", lat)})
	}

	// Judge 数据集的凭据提示（服务端配置里的 judge key）
	needJudge := false
	for _, d := range e.capCfg.Datasets {
		if d.NeedsJudge {
			needJudge = true
			break
		}
	}
	if needJudge {
		if e.capCfg.Judge != nil && e.capCfg.Judge.APIKey != "" {
			rep.Checks = append(rep.Checks, CheckItem{Name: "judge", OK: true, Detail: "服务端已配置 Judge 凭据"})
		} else {
			rep.Checks = append(rep.Checks, CheckItem{Name: "judge", OK: false, Severity: "warn",
				Detail: "hle / simple_qa 等数据集需要 Judge 凭据，提交时需在 plan.judge.api_key 传入"})
		}
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

var evalProgressRe = regexp.MustCompile(`Running\[eval\]:\s*(\d+)%`)

// Run 逐数据集执行 run_eval.py。
func (e *EvalEngine) Run(ctx context.Context, spec Spec, emit func(Event)) (Result, error) {
	res := Result{DBPaths: map[int]string{}, Eval: &domain.EvalMetrics{
		Model: spec.Target.Model, Provider: evalProvider(spec),
	}}
	res.LogPath = filepath.Join(spec.WorkDir, "logs", fmt.Sprintf("%s-eval.log", spec.RunID))
	if err := os.MkdirAll(filepath.Dir(res.LogPath), 0o755); err != nil {
		return res, err
	}
	logFile, err := os.Create(res.LogPath)
	if err != nil {
		return res, err
	}
	defer logFile.Close()

	outRoot := filepath.Join(spec.WorkDir, "eval")
	if err := os.MkdirAll(outRoot, 0o755); err != nil {
		return res, err
	}

	for i, st := range spec.Steps {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		emit(Event{Kind: EvStepStarted, At: time.Now().UTC(), Data: map[string]any{
			"step_index": i, "bucket": st.Bucket, "parallel": st.Parallel, "number": st.Number,
		}})

		outDir := filepath.Join(outRoot, sanitizeDirName(st.Bucket))
		if err := os.MkdirAll(outDir, 0o755); err != nil {
			return res, err
		}
		started := time.Now()
		args := e.evalArgs(spec, st, outDir)
		cmd := exec.CommandContext(ctx, e.python, args...)
		cmd.Dir = e.workdir
		cmd.Env = e.childEnv()
		setPgid(cmd)

		fmt.Fprintf(logFile, "\n=== dataset %d: %s ===\n$ %s %s\n", i, st.Bucket, e.python, strings.Join(redactArgs(args), " "))

		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return res, err
		}
		cmd.Stderr = cmd.Stdout
		if err := cmd.Start(); err != nil {
			emit(Event{Kind: EvError, At: time.Now().UTC(), Data: map[string]any{
				"code": "engine_failed", "detail": err.Error()}})
			return res, err
		}

		lastPct := -1
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			sc := bufio.NewScanner(stdout)
			sc.Buffer(make([]byte, 64*1024), 2*1024*1024)
			for sc.Scan() {
				line := redactSecrets(sc.Text(), spec.Target.APIKey, judgeKey(spec))
				fmt.Fprintln(logFile, line)
				emit(Event{Kind: EvLog, At: time.Now().UTC(), Data: map[string]any{"line": line}})
				if m := evalProgressRe.FindStringSubmatch(line); m != nil {
					pct, _ := strconv.Atoi(m[1])
					if pct != lastPct && pct >= 0 && pct <= 100 {
						lastPct = pct
						emit(Event{Kind: EvStepProgress, At: time.Now().UTC(), Data: map[string]any{
							"step_index": i, "completed": pct, "total": 100,
						}})
					}
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
			return res, ctx.Err()
		case err := <-waitErr:
			wg.Wait()
			runErr = err
		}
		if runErr != nil {
			res.ExitCode = exitCode(runErr)
		}

		item := readEvalSummary(outDir, st)
		item.ElapsedSec = roundTo(time.Since(started).Seconds(), 1)
		if runErr != nil && item.Error == "" {
			item.Error = fmt.Sprintf("run_eval.py 退出码 %d（详见 run.log）", res.ExitCode)
		}
		status := domain.StepSucceeded
		if !item.Success {
			status = domain.StepFailed
		}
		sr := 0.0
		if item.Success {
			sr = 1
		}
		dur := item.ElapsedSec
		res.Steps = append(res.Steps, domain.Step{
			Index: i, Bucket: st.Bucket, Parallel: st.Parallel, Number: st.Number,
			Status: status, SuccessRate: &sr, DurationSec: &dur,
		})
		res.Eval.Datasets = append(res.Eval.Datasets, item)

		emit(Event{Kind: EvStepFinished, At: time.Now().UTC(), Data: map[string]any{
			"step_index": i, "status": string(status), "success_rate": sr,
			"score": item.Score, "duration_sec": dur,
		}})

		res.Files = append(res.Files, collectEvalFiles(st.Bucket, outDir)...)
	}
	return res, nil
}

// evalArgs 构造 run_eval.py 的参数。
func (e *EvalEngine) evalArgs(spec Spec, st domain.StepSpec, outDir string) []string {
	plan := spec.Plan
	args := []string{
		e.script,
		"--datasets", st.Bucket,
		"--model_name", spec.Target.Model,
		"--provider", evalProvider(spec),
		"--base_url", evalBaseURL(spec.Target.APIURL),
		"--api_key", spec.Target.APIKey,
		"--output_dir", outDir,
	}
	if st.Number > 0 {
		args = append(args, "--limit", strconv.Itoa(st.Number))
	}
	if st.Repeats > 0 {
		args = append(args, "--repeats", strconv.Itoa(st.Repeats))
	}
	if st.Parallel > 0 {
		args = append(args, "--eval_batch_size", strconv.Itoa(st.Parallel))
	}
	if v, ok := toFloat(plan.Request.Temperature); ok {
		args = append(args, "--temperature", strconv.FormatFloat(v, 'f', -1, 64))
	}
	if plan.Request.MaxTokens > 0 {
		args = append(args, "--max_tokens", strconv.Itoa(plan.Request.MaxTokens))
	}
	if plan.Thinking != nil {
		args = append(args, "--thinking", strconv.FormatBool(*plan.Thinking))
	}
	if plan.ThinkingType != "" {
		args = append(args, "--thinking-type", plan.ThinkingType)
	}
	if plan.Judge != nil {
		if plan.Judge.APIKey != "" {
			args = append(args, "--judge_api_key", plan.Judge.APIKey)
		}
		if plan.Judge.Model != "" {
			args = append(args, "--judge_model", plan.Judge.Model)
		}
		if plan.Judge.BaseURL != "" {
			args = append(args, "--judge_base_url", plan.Judge.BaseURL)
		}
	}
	return args
}

func (e *EvalEngine) childEnv() []string {
	env := os.Environ()
	for k, v := range e.env {
		env = append(env, k+"="+v)
	}
	// run_eval.py 的 pre_check 用 shutil.which('evalscope') 判断环境，
	// 服务进程的 PATH 通常不含 venv/bin（我们用绝对路径调 python），必须补上。
	paths := []string{}
	if d := filepath.Dir(e.python); d != "" && d != "." {
		paths = append(paths, d)
	}
	if d := filepath.Dir(e.evalscope); d != "" && d != "." {
		paths = append(paths, d)
	}
	if cur := os.Getenv("PATH"); cur != "" {
		paths = append(paths, cur)
	}
	if len(paths) > 0 {
		env = append(env, "PATH="+strings.Join(paths, string(os.PathListSeparator)))
	}
	if len(e.pyPath) > 0 {
		env = append(env, "PYTHONPATH="+strings.Join(e.pyPath, string(os.PathListSeparator)))
	}
	env = append(env, "PYTHONUNBUFFERED=1")
	return env
}

// ---------------- helpers ----------------

func evalProvider(spec Spec) string {
	if spec.Plan.ProviderName != "" {
		return spec.Plan.ProviderName
	}
	if spec.Target.Vendor != "" {
		return spec.Target.Vendor
	}
	return "unknown"
}

// evalBaseURL 去掉完整请求路径后缀，得到 OpenAI 兼容 base_url（与 eval/run.sh 一致）。
func evalBaseURL(apiURL string) string {
	u := strings.TrimRight(apiURL, "/")
	u = strings.TrimSuffix(u, "/chat/completions")
	u = strings.TrimSuffix(u, "/v1/messages")
	return u
}

func judgeKey(spec Spec) string {
	if spec.Plan.Judge == nil {
		return ""
	}
	return spec.Plan.Judge.APIKey
}

func orDefaultStr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func redactSecrets(s string, secrets ...string) string {
	for _, sec := range secrets {
		if sec != "" {
			s = strings.ReplaceAll(s, sec, "***")
		}
	}
	return s
}

func sanitizeDirName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "" || out == "." || out == ".." {
		return "dataset"
	}
	return out
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	}
	return 0, false
}

// evalSummaryFile 是 run_eval.py 落盘的 eval_summary.json 子集。
type evalSummaryFile struct {
	Dataset        string  `json:"dataset"`
	Success        bool    `json:"success"`
	Score          float64 `json:"score"`
	NumSamples     int     `json:"num_samples"`
	RetriesUsed    int     `json:"retries_used"`
	RateLimited    bool    `json:"rate_limited"`
	ElapsedSeconds float64 `json:"elapsed_seconds"`
	SkippedCount   int     `json:"skipped_count"`
	CacheStats     *struct {
		HitRate *float64 `json:"hit_rate"`
	} `json:"cache_stats"`
	Metrics struct {
		Metrics []struct {
			Name  string  `json:"name"`
			Score float64 `json:"score"`
		} `json:"metrics"`
	} `json:"metrics"`
}

func readEvalSummary(dir string, st domain.StepSpec) domain.EvalDatasetResult {
	item := domain.EvalDatasetResult{
		Dataset: st.Bucket, Label: st.Bucket,
		Limit: st.Number, EvalBatch: st.Parallel, Repeats: st.Repeats,
	}
	p := filepath.Join(dir, "eval_summary.json")
	b, err := os.ReadFile(p)
	if err != nil {
		item.Error = "未生成 eval_summary.json（评测未跑完或预检查失败，详见 run.log）"
		return item
	}
	var s evalSummaryFile
	if err := json.Unmarshal(b, &s); err != nil {
		item.Error = "eval_summary.json 解析失败: " + err.Error()
		return item
	}
	item.Success = s.Success
	item.Score = s.Score
	// 上游全挂时 run_eval.py 仍报 success=true（ignore_errors 把失败样本全吞了），
	// score/num_samples 缺失说明一条有效样本都没有 —— 不能当 0 分的成功评测。
	if s.Success && s.Score == 0 && s.NumSamples == 0 {
		item.Success = false
		item.Error = orDefaultStr(item.Error, "没有有效样本（请求可能全部失败，详见 run.log）")
	}
	item.NumSamples = s.NumSamples
	item.RetriesUsed = s.RetriesUsed
	item.RateLimited = s.RateLimited
	item.ElapsedSec = s.ElapsedSeconds
	item.Skipped = s.SkippedCount
	if s.Dataset != "" {
		item.Label = s.Dataset
	}
	if s.CacheStats != nil {
		item.CacheHitRate = s.CacheStats.HitRate
	}
	if len(s.Metrics.Metrics) > 0 {
		item.Metric = s.Metrics.Metrics[0].Name
	}
	return item
}

// collectEvalFiles 收集单个数据集目录里的可下载产物。
func collectEvalFiles(dataset, dir string) []FileArtifact {
	prefix := dataset + "-"
	candidates := []struct {
		name string
		kind domain.ArtifactKind
		ct   string
	}{
		{"eval_report_v2.html", domain.ArtEvalReport, "text/html; charset=utf-8"},
		{"eval_summary.json", domain.ArtEvalSummary, "application/json"},
		{"per_sample_details.csv", domain.ArtEvalSamples, "text/csv; charset=utf-8"},
		{"eval_results.tar.gz", domain.ArtEvalArchive, "application/gzip"},
	}
	var out []FileArtifact
	for _, c := range candidates {
		p := filepath.Join(dir, c.name)
		if st, err := os.Stat(p); err != nil || st.IsDir() {
			continue
		}
		out = append(out, FileArtifact{Kind: c.kind, Filename: prefix + c.name, Path: p, ContentType: c.ct})
	}
	return out
}
