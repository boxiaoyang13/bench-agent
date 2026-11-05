package engine

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
	"github.com/apexcloud/thvv/server/internal/report"
)

// Subprocess 是 fork evalscope 的生产引擎（kind: subprocess）。
//
// 注意：本实现按 thvv/perf/run.sh 的调用方式构造 argv；
// 它需要真实的 Python 环境，未纳入自动化联调范围（见 docs/DESIGN.md M3）。
type Subprocess struct {
	name      string
	cfg       *config.Engine
	capCfg    *config.Capability
	home      string
	python    string
	evalscope string
	workdir   string
	env       map[string]string
	pyPath    []string
	killGrace time.Duration
	logPath   string
}

// NewSubprocess 构造 evalscope 引擎。
func NewSubprocess(name string, eng *config.Engine, cap *config.Capability, home string) *Subprocess {
	rt := cap.Runtime
	if rt == nil {
		rt = eng.Runtime
	}
	s := &Subprocess{name: name, cfg: eng, capCfg: cap, home: home, killGrace: 10 * time.Second}
	if rt != nil {
		s.python = string(rt.Python)
		s.evalscope = string(rt.Evalscope)
		s.env = rt.Env
		for _, p := range rt.PythonPathPrepend {
			s.pyPath = append(s.pyPath, string(p))
		}
	}
	s.workdir = string(cap.WorkingDir)
	return s
}

func (s *Subprocess) Name() string { return s.name }

func (s *Subprocess) Describe() Description {
	d := Description{
		Name:        s.name,
		DisplayName: s.cfg.DisplayName,
		Version:     s.cfg.Version,
		Kind:        "subprocess",
		Runtime: map[string]string{
			"python":    s.python,
			"evalscope": s.evalscope,
			"home":      s.home,
		},
	}
	if d.DisplayName == "" {
		d.DisplayName = "THVV TokenHub Vendor Verifier"
	}
	if d.Version == "" {
		d.Version = "1.0.0"
	}
	for name, cap := range s.cfg.Capabilities {
		if cap == nil {
			continue
		}
		d.Capabilities = append(d.Capabilities, CapabilityDesc{
			Name: name, Status: cap.Status, Description: cap.Description,
			AvailableAt: cap.AvailableAt,
			PlanSchema:  cap.PlanSchema, ResultSchema: cap.ResultSchema,
			Artifacts: []string{"html_report", "xlsx_report", "failure_csv", "sj_json", "raw_db"},
		})
	}
	return d
}

func (s *Subprocess) Validate(cap domain.Capability, plan domain.Plan) error {
	if cap != domain.CapPerf {
		return fmt.Errorf("capability %s 暂未实现", cap)
	}
	if len(plan.Buckets) == 0 {
		return fmt.Errorf("plan.buckets 不能为空")
	}
	return nil
}

// Probe 做连通性 / tokenizer / 数据集自检。
func (s *Subprocess) Probe(ctx context.Context, t ResolvedTarget) ProbeReport {
	start := time.Now()
	rep := ProbeReport{
		TargetID: t.TargetID, CheckedAt: start.UTC(),
		Engine: map[string]string{"name": s.name, "python": s.python, "evalscope": s.evalscope},
	}
	rep.Checks = append(rep.Checks, CheckItem{Name: "protocol", OK: true, Detail: string(t.Protocol)})

	// evalscope 版本
	if out, err := exec.CommandContext(ctx, s.evalscope, "--version").CombinedOutput(); err == nil {
		rep.Engine["evalscope_version"] = strings.TrimSpace(string(out))
		rep.Checks = append(rep.Checks, CheckItem{Name: "engine", OK: true, Detail: strings.TrimSpace(string(out))})
	} else {
		rep.Checks = append(rep.Checks, CheckItem{Name: "engine", OK: false, Severity: "error",
			Detail: fmt.Sprintf("执行 %s --version 失败: %v", s.evalscope, err)})
	}

	// 端点连通性：发一条最小请求，测首 token 延迟
	if lat, err := s.ping(ctx, t); err != nil {
		rep.Checks = append(rep.Checks, CheckItem{Name: "connectivity", OK: false, Severity: "error", Detail: err.Error()})
	} else {
		rep.Checks = append(rep.Checks, CheckItem{Name: "connectivity", OK: true, LatencyMS: lat,
			Detail: fmt.Sprintf("HTTP 200，首 token %dms", lat)})
	}

	// tokenizer
	if t.Tokenizer == "" {
		rep.Checks = append(rep.Checks, CheckItem{Name: "tokenizer", OK: false, Severity: "warn", Detail: "未指定 tokenizer"})
	} else {
		args := []string{"-c",
			"from transformers import AutoTokenizer;AutoTokenizer.from_pretrained('" + t.Tokenizer + "',local_files_only=True)"}
		if _, err := exec.CommandContext(ctx, s.python, args...).CombinedOutput(); err == nil {
			rep.Checks = append(rep.Checks, CheckItem{Name: "tokenizer", OK: true, Detail: t.Tokenizer + " 已缓存"})
		} else {
			rep.Checks = append(rep.Checks, CheckItem{Name: "tokenizer", OK: false, Severity: "warn",
				Detail: t.Tokenizer + " 未缓存（首次压测会自动下载）"})
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

// ping 发一条最小 completion 请求，返回 TTFT 毫秒。
func (s *Subprocess) ping(ctx context.Context, t ResolvedTarget) (int64, error) {
	var body []byte
	hdr := map[string]string{"Content-Type": "application/json"}
	switch t.Protocol {
	case domain.ProtocolAnthropic:
		hdr["x-api-key"] = t.APIKey
		hdr["anthropic-version"] = "2023-06-01"
		body, _ = json.Marshal(map[string]any{
			"model": t.Model, "max_tokens": 1, "stream": true,
			"messages": []map[string]any{{"role": "user", "content": "hi"}},
		})
	default:
		hdr["Authorization"] = "Bearer " + t.APIKey
		body, _ = json.Marshal(map[string]any{
			"model": t.Model, "max_tokens": 1, "stream": true,
			"messages": []map[string]any{{"role": "user", "content": "hi"}},
		})
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.APIURL, strings.NewReader(string(body)))
	if err != nil {
		return 0, err
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	start := time.Now()
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.CopyN(io.Discard, resp.Body, 64)
	lat := time.Since(start).Milliseconds()
	if resp.StatusCode >= 300 {
		return lat, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return lat, nil
}

var progressRe = regexp.MustCompile(`(\d+)\s*/\s*(\d+)`)

// Run 逐 step 执行 evalscope perf。
func (s *Subprocess) Run(ctx context.Context, spec Spec, emit func(Event)) (Result, error) {
	res := Result{DBPaths: map[int]string{}}
	res.LogPath = filepath.Join(spec.WorkDir, "logs", fmt.Sprintf("%s-engine.log", spec.RunID))
	if err := os.MkdirAll(filepath.Dir(res.LogPath), 0o755); err != nil {
		return res, err
	}
	logFile, err := os.Create(res.LogPath)
	if err != nil {
		return res, err
	}
	defer logFile.Close()

	for i, st := range spec.Steps {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		emit(Event{Kind: EvStepStarted, At: time.Now().UTC(), Data: map[string]any{
			"step_index": i, "bucket": st.Bucket, "parallel": st.Parallel, "number": st.Number,
		}})
		outDir := filepath.Join(spec.WorkDir, "results",
			fmt.Sprintf("perf-%s-%s", st.Bucket, time.Now().Format("20060102-1504")))
		if err := os.MkdirAll(outDir, 0o755); err != nil {
			return res, err
		}

		args := s.perfArgs(spec, st, outDir)
		cmd := exec.CommandContext(ctx, s.evalscope, args...)
		cmd.Dir = s.workdir
		cmd.Env = s.childEnv(spec)
		setPgid(cmd)

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
		fmt.Fprintf(logFile, "\n=== step %d: %s ===\n$ %s %s\n", i, st.Bucket, s.evalscope, strings.Join(args, " "))

		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			sc := bufio.NewScanner(stdout)
			sc.Buffer(make([]byte, 64*1024), 1024*1024)
			for sc.Scan() {
				line := sc.Text()
				fmt.Fprintln(logFile, line)
				emit(Event{Kind: EvLog, At: time.Now().UTC(), Data: map[string]any{"line": line}})
				if m := progressRe.FindStringSubmatch(line); m != nil {
					done, _ := strconv.Atoi(m[1])
					all, _ := strconv.Atoi(m[2])
					if all > 0 && done <= all {
						emit(Event{Kind: EvStepProgress, At: time.Now().UTC(), Data: map[string]any{
							"step_index": i, "completed": done, "total": all,
						}})
					}
				}
			}
		}()

		// 取消时先 SIGTERM 整组，宽限期后 SIGKILL
		waitErr := make(chan error, 1)
		go func() { waitErr <- cmd.Wait() }()
		select {
		case <-ctx.Done():
			killTree(cmd, true)
			select {
			case <-waitErr:
			case <-time.After(s.killGrace):
				killTree(cmd, false)
				<-waitErr
			}
			wg.Wait()
			return res, ctx.Err()
		case err := <-waitErr:
			wg.Wait()
			if err != nil {
				res.ExitCode = exitCode(err)
				emit(Event{Kind: EvError, At: time.Now().UTC(), Data: map[string]any{
					"code": "engine_failed", "detail": fmt.Sprintf("evalscope 退出码 %d", res.ExitCode)}})
			}
		}

		// 汇总该 step 的产物
		dbPath := findBenchmarkDB(outDir)
		if dbPath != "" {
			res.DBPaths[i] = dbPath
			step := domain.Step{Index: i, Bucket: st.Bucket, Parallel: st.Parallel, Number: st.Number, DBPath: dbPath}
			m, _, err := report.LoadStepMetrics(dbPath, step)
			if err == nil {
				status := domain.StepSucceeded
				if m.Requests.SuccessRate < 1 {
					status = domain.StepFailed
				}
				sr, ttft, tps, dur := m.Requests.SuccessRate, m.TTFT.P50Sec, m.Throughput.OutputTPS, m.DurationSec
				step.Status, step.SuccessRate, step.TTFTp50Sec, step.OutputTPS, step.DurationSec =
					status, &sr, &ttft, &tps, &dur
				res.Metrics = append(res.Metrics, m)
				res.Steps = append(res.Steps, step)
				emit(Event{Kind: EvStepFinished, At: time.Now().UTC(), Data: map[string]any{
					"step_index": i, "status": string(status), "success_rate": sr,
					"output_tps": tps, "duration_sec": dur, "ttft_p50_sec": ttft,
				}})
				continue
			}
		}
		// 没有产物 → 记为失败 step
		sr := 0.0
		res.Steps = append(res.Steps, domain.Step{
			Index: i, Bucket: st.Bucket, Parallel: st.Parallel, Number: st.Number,
			Status: domain.StepFailed, SuccessRate: &sr,
		})
		emit(Event{Kind: EvStepFinished, At: time.Now().UTC(), Data: map[string]any{
			"step_index": i, "status": string(domain.StepFailed), "success_rate": 0.0,
		}})
	}
	return res, nil
}

// perfArgs 构造 evalscope perf 的参数（对齐 thvv/perf/run.sh cmd_bench）。
func (s *Subprocess) perfArgs(spec Spec, st domain.StepSpec, outDir string) []string {
	t := spec.Target
	dataset := filepath.Join(spec.DatasetsDir, fmt.Sprintf("perf_zh_%s.jsonl", st.Bucket))
	if spec.DatasetsDir == "" {
		dataset = fmt.Sprintf("datasets_clean/perf_zh_%s.jsonl", st.Bucket)
	}
	temp := "0.0"
	if v, ok := t.Temperature.(float64); ok {
		temp = strconv.FormatFloat(v, 'f', 1, 64)
	}
	args := []string{
		"perf",
		"--model", t.Model,
		"--api", string(t.Protocol),
		"--tokenizer-path", t.Tokenizer,
		"--url", t.APIURL,
		"--api-key", t.APIKey,
		"--temperature", temp,
		"--parallel", strconv.Itoa(st.Parallel),
		"--number", strconv.Itoa(st.Number),
		"--dataset", "custom_multi_turn",
		"--dataset-path", dataset,
		"--min-prompt-length", strconv.Itoa(st.MinLen),
		"--max-prompt-length", strconv.Itoa(st.MaxLen),
		"--max-tokens", strconv.Itoa(nonZero(t.MaxTokens, 4096)),
		"--multi-turn", "--max-turns", "1",
		"--connect-timeout", strconv.Itoa(int(t.ConnectTimeout.Seconds())),
		"--read-timeout", strconv.Itoa(int(t.ReadTimeout.Seconds())),
		"--db-commit-interval", "5",
		"--outputs-dir", outDir,
		"--no-timestamp", "--no-test-connection",
	}
	return args
}

func (s *Subprocess) childEnv(spec Spec) []string {
	env := os.Environ()
	for k, v := range s.env {
		env = append(env, k+"="+v)
	}
	if len(s.pyPath) > 0 {
		env = append(env, "PYTHONPATH="+strings.Join(s.pyPath, string(os.PathListSeparator)))
	}
	env = append(env, "PYTHONUNBUFFERED=1")
	return env
}

func nonZero(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}

func exitCode(err error) int {
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	return -1
}

func findBenchmarkDB(root string) string {
	var found string
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info == nil {
			return nil
		}
		if !info.IsDir() && info.Name() == "benchmark_data.db" {
			found = p
			return filepath.SkipAll
		}
		return nil
	})
	return found
}
