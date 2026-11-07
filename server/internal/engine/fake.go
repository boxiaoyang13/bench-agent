package engine

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"os"
	"strconv"
	"time"

	"github.com/apexcloud/thvv/server/internal/domain"
)

// Fake 是 builtin 模拟引擎：不依赖 Python，用于 UI 联调、演示与回归。
//
// 行为尽量贴近真实：按档位 token 数缩放 TTFT 与吞吐，偶发失败，遵守 ctx 取消。
// 节奏由环境变量控制：
//
//	THVV_FAKE_STEP_MS   每个 step 的总耗时（默认 900）
//	THVV_FAKE_FAIL_RATE 单 step 注入失败的概率（默认 0.08）
type Fake struct {
	StepMS   int
	FailRate float64
	Buckets  []domain.Bucket
}

// NewFake 构造模拟引擎。
func NewFake(buckets []domain.Bucket) *Fake {
	stepMS := 900
	if v := os.Getenv("THVV_FAKE_STEP_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			stepMS = n
		}
	}
	failRate := 0.08
	if v := os.Getenv("THVV_FAKE_FAIL_RATE"); v != "" {
		if r, err := strconv.ParseFloat(v, 64); err == nil && r >= 0 {
			failRate = r
		}
	}
	return &Fake{StepMS: stepMS, FailRate: failRate, Buckets: buckets}
}

func (f *Fake) Name() string { return "fake" }

func (f *Fake) Describe() Description {
	return Description{
		Name:        "fake",
		DisplayName: "内置模拟引擎（无 Python 依赖）",
		Version:     "1.0.0",
		Kind:        "builtin",
		Capabilities: []CapabilityDesc{
			{
				Name: "perf", Status: "ga",
				Description: "模拟性能压测：按档位缩放 TTFT/吞吐，用于 UI 联调与演示",
				PlanSchema:  "", ResultSchema: "",
				Artifacts: []string{"html_report", "xlsx_report", "failure_csv", "sla_json"},
			},
		},
		Runtime: map[string]string{"python": "不需要", "note": "THVV_FAKE_STEP_MS / THVV_FAKE_FAIL_RATE 可调"},
	}
}

func (f *Fake) Validate(domain.Capability, domain.Plan) error { return nil }

func (f *Fake) Probe(ctx context.Context, t ResolvedTarget) ProbeReport {
	start := time.Now()
	rep := ProbeReport{
		TargetID: t.TargetID, CheckedAt: start.UTC(),
		Engine: map[string]string{"name": "fake", "version": "1.0.0"},
	}
	lat := 180 + rand.Int63n(400)
	rep.Checks = append(rep.Checks,
		CheckItem{Name: "protocol", OK: true, Detail: string(t.Protocol)},
		CheckItem{Name: "connectivity", OK: true, Detail: fmt.Sprintf("模拟连通性正常，首 token %dms", lat), LatencyMS: lat},
		CheckItem{Name: "tokenizer", OK: true, Detail: "模拟引擎不做 tokenizer 校验（生产请用 thvv 引擎）"},
	)
	unbuilt := []string{}
	for _, b := range f.Buckets {
		if !b.BuiltIn {
			unbuilt = append(unbuilt, b.Label)
		}
	}
	if len(unbuilt) > 0 {
		rep.Checks = append(rep.Checks, CheckItem{
			Name: "datasets", OK: false, Severity: "warn",
			Detail:  "以下档位数据集未构建（不影响其余档位）",
			Missing: unbuilt,
		})
	} else {
		rep.Checks = append(rep.Checks, CheckItem{Name: "datasets", OK: true, Detail: "全部档位就绪"})
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

func (f *Fake) Run(ctx context.Context, spec Spec, emit func(Event)) (Result, error) {
	if spec.Capability == domain.CapEval {
		return f.runEval(ctx, spec, emit)
	}
	res := Result{DBPaths: map[int]string{}}
	total := len(spec.Steps)
	perStep := time.Duration(f.StepMS) * time.Millisecond

	for i, st := range spec.Steps {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		emit(Event{Kind: EvStepStarted, At: time.Now().UTC(), Data: map[string]any{
			"step_index": i, "bucket": st.Bucket, "parallel": st.Parallel, "number": st.Number,
		}})

		scale := f.scale(st.Bucket)
		ttft := roundTo(0.28*math.Pow(scale, 0.55), 3)
		tps := roundTo(42/math.Pow(scale, 0.35), 2)
		tpot := roundTo(1000/(tps*6+40)*6, 2)

		// 分段推进（3 段），并支持取消
		ticks := 3
		for t := 1; t <= ticks; t++ {
			select {
			case <-ctx.Done():
				return res, ctx.Err()
			case <-time.After(perStep / time.Duration(ticks)):
			}
			if t < ticks {
				emit(Event{Kind: EvStepProgress, At: time.Now().UTC(), Data: map[string]any{
					"step_index": i, "completed": st.Number * t / ticks, "total": st.Number,
					"success_rate": 1.0, "ttft_p50_sec": ttft,
				}})
			}
		}

		failed := rand.Float64() < f.FailRate
		successRate := 1.0
		if failed {
			successRate = 0.9721
		}
		status := domain.StepSucceeded
		if failed {
			status = domain.StepFailed
		}
		success := st.Number
		if failed {
			success = int(math.Round(float64(st.Number) * successRate))
		}
		dur := float64(perStep.Milliseconds()) / 1000 * 1.5

		m := domain.StepMetrics{
			StepIndex: i, Bucket: st.Bucket, Parallel: st.Parallel, Number: st.Number,
			Requests: domain.RequestCounts{Total: st.Number, Success: success, Failed: st.Number - success, SuccessRate: successRate},
			TTFT: domain.TTFTStats{
				AvgSec: ttft, MinSec: roundTo(ttft*0.7, 3), MaxSec: roundTo(ttft*1.8, 3),
				P50Sec: ttft, P90Sec: roundTo(ttft*1.4, 3), P95Sec: roundTo(ttft*1.6, 3), P99Sec: roundTo(ttft*1.8, 3),
			},
			TTLT: domain.Scalar{AvgSec: roundTo(dur, 2)},
			TPOT: domain.Scalar{AvgMs: tpot},
			ITL:  domain.Scalar{AvgMs: roundTo(tpot*0.97, 2), P50Ms: roundTo(tpot*0.95, 2), P90Ms: roundTo(tpot*1.15, 2)},
			Throughput: domain.Throughput{
				OutputTPS: tps, InputTPS: roundTo(tps*8, 2),
				OutputTPM: roundTo(tps*60, 1), InputTPM: roundTo(tps*8*60, 1),
				TPM: roundTo(tps*9*60, 1), QPM: roundTo(float64(success)/dur*60, 1),
			},
			Tokens: domain.TokenSums{
				PromptSum:     st.Number * st.MaxLen,
				CompletionSum: st.Number * 1024,
				TotalSum:      st.Number * (st.MaxLen + 1024),
			},
			TokenRateAvg: tps,
			DurationSec:  roundTo(dur, 2),
			Failures:     map[string]int{},
		}
		if failed {
			m.Failures["http_429"] = st.Number - success
		}
		res.Metrics = append(res.Metrics, m)
		res.Steps = append(res.Steps, domain.Step{
			Index: i, Bucket: st.Bucket, Parallel: st.Parallel, Number: st.Number,
			Status: status, SuccessRate: fp(successRate), TTFTp50Sec: fp(ttft),
			OutputTPS: fp(tps), DurationSec: fp(dur),
		})

		emit(Event{Kind: EvStepFinished, At: time.Now().UTC(), Data: map[string]any{
			"step_index": i, "status": string(status), "success_rate": successRate,
			"output_tps": tps, "duration_sec": dur, "ttft_p50_sec": ttft,
		}})
		emit(Event{Kind: EvLog, At: time.Now().UTC(), Data: map[string]any{
			"line": fmt.Sprintf("[fake] step=%d bucket=%s parallel=%d number=%d success=%.2f%% ttft=%.3fs tps=%.2f",
				i+1, st.Bucket, st.Parallel, st.Number, successRate*100, ttft, tps),
		}})
		_ = total
	}
	return res, nil
}

// scale 用档位 token 上界做吞吐/TTFT 缩放。
func (f *Fake) scale(label string) float64 {
	for _, b := range f.Buckets {
		if b.Label == label {
			return math.Max(float64(b.PromptTokens.Max)/1200.0, 1)
		}
	}
	return 1
}

func roundTo(v float64, digits int) float64 {
	p := math.Pow(10, float64(digits))
	return math.Round(v*p) / p
}

func fp(v float64) *float64 { return &v }

// runEval 是 eval 能力的模拟实现：按数据集产出随机分数，便于 UI 与集成测试。
func (f *Fake) runEval(ctx context.Context, spec Spec, emit func(Event)) (Result, error) {
	res := Result{DBPaths: map[int]string{}, Eval: &domain.EvalMetrics{
		Model: spec.Target.Model, Provider: evalProvider(spec),
	}}
	for i, st := range spec.Steps {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		emit(Event{Kind: EvStepStarted, At: time.Now().UTC(), Data: map[string]any{
			"step_index": i, "bucket": st.Bucket, "parallel": st.Parallel, "number": st.Number,
		}})
		steps := 5
		for p := 1; p <= steps; p++ {
			select {
			case <-ctx.Done():
				return res, ctx.Err()
			case <-time.After(time.Duration(f.StepMS/steps) * time.Millisecond):
			}
			emit(Event{Kind: EvStepProgress, At: time.Now().UTC(), Data: map[string]any{
				"step_index": i, "completed": p * 100 / steps, "total": 100,
			}})
		}
		ok := rand.Float64() >= f.FailRate
		score := 0.4 + rand.Float64()*0.55
		if !ok {
			score = 0
		}
		sr := 0.0
		if ok {
			sr = 1
		}
		dur := float64(f.StepMS) / 1000
		status := domain.StepFailed
		if ok {
			status = domain.StepSucceeded
		}
		res.Steps = append(res.Steps, domain.Step{
			Index: i, Bucket: st.Bucket, Parallel: st.Parallel, Number: st.Number,
			Status: status, SuccessRate: &sr, DurationSec: &dur,
		})
		res.Eval.Datasets = append(res.Eval.Datasets, domain.EvalDatasetResult{
			Dataset: st.Bucket, Label: st.Bucket, Success: ok, Score: math.Round(score*1000) / 1000,
			Metric: "mean_acc", NumSamples: st.Number, Limit: st.Number,
			Repeats: st.Repeats, EvalBatch: st.Parallel, ElapsedSec: dur,
		})
		emit(Event{Kind: EvStepFinished, At: time.Now().UTC(), Data: map[string]any{
			"step_index": i, "status": string(status), "success_rate": sr,
			"score": score, "duration_sec": dur,
		}})
	}
	return res, nil
}
