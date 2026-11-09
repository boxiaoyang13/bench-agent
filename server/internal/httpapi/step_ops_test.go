package httpapi_test

import (
	"fmt"
	"net/http"
	"testing"
	"time"
)

// mkStepTarget 建一个 cache target。
func mkStepTarget(t *testing.T, h *harness, name string) string {
	t.Helper()
	resp, body := h.do("POST", "/targets", map[string]any{
		"name": name, "vendor": "zhipu",
		"api_url":  "https://api.example.com/v1/chat/completions",
		"protocol": "openai", "model": "glm-5.2", "api_key": "sk-step-1234",
	}, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("建端点: %d %s", resp.StatusCode, body)
	}
	var target map[string]any
	mustJSON(t, body, &target)
	return target["target_id"].(string)
}

type stepView struct {
	Index  int    `json:"index"`
	Bucket string `json:"bucket"`
	Status string `json:"status"`
}

// listSteps 拉取 run 的步骤（include=steps）。
func listSteps(t *testing.T, h *harness, runID string) (status string, steps []stepView) {
	t.Helper()
	resp, body := h.do("GET", "/runs/"+runID+"?include=steps", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("取 run: %d %s", resp.StatusCode, body)
	}
	var run struct {
		Status string     `json:"status"`
		Steps  []stepView `json:"steps"`
	}
	mustJSON(t, body, &run)
	return run.Status, run.Steps
}

// TestCacheStepSkipAndCancel 多档 run：跳过待执行档位 + 取消运行中档位，其余照常执行。
func TestCacheStepSkipAndCancel(t *testing.T) {
	t.Setenv("THVV_FAKE_STEP_MS", "800")
	t.Setenv("THVV_FAKE_FAIL_RATE", "0")
	h := newHarness(t)
	h.login()
	tid := mkStepTarget(t, h, "step-ops")

	resp, body := h.do("POST", "/runs", map[string]any{
		"tool": "lmcache", "capability": "cache", "target_id": tid,
		"plan": map[string]any{
			"workload": "multi-round-chat", "duration_sec": 30, "qps": 1,
			"shared_prompt_length": 2000, "chat_history_length": 10000,
			"tokens_per_gb_kvcache": 65536,
			"ladder": []map[string]any{
				{"sessions": 1}, {"sessions": 2}, {"sessions": 4},
			},
		},
	}, map[string]string{"Idempotency-Key": "step-ops-1"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("提交: %d %s", resp.StatusCode, body)
	}
	var run map[string]any
	mustJSON(t, body, &run)
	rid := run["run_id"].(string)

	// 等第一档进入 running（fake 每档 800ms）
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		status, steps := listSteps(t, h, rid)
		if status == "running" && len(steps) == 3 && steps[0].Status == "running" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	// 运行中的档位不能"跳过" → 409
	resp, _ = h.do("POST", "/runs/"+rid+"/steps/0/skip", nil, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("跳过运行中 step 应 409，实际 %d", resp.StatusCode)
	}
	// 跳过第 3 档（pending）→ 200
	resp, body = h.do("POST", "/runs/"+rid+"/steps/2/skip", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("跳过 step: %d %s", resp.StatusCode, body)
	}
	var skipRes struct {
		Step stepView `json:"step"`
	}
	mustJSON(t, body, &skipRes)
	if skipRes.Step.Status != "skipped" {
		t.Fatalf("跳过响应状态: %+v", skipRes.Step)
	}
	// 取消第 1 档（running）→ 202
	resp, body = h.do("POST", "/runs/"+rid+"/steps/0/cancel", nil, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("取消 step: %d %s", resp.StatusCode, body)
	}
	// pending 步骤不能"取消" → 409
	resp, _ = h.do("POST", "/runs/"+rid+"/steps/1/cancel", nil, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("取消 pending step 应 409，实际 %d", resp.StatusCode)
	}

	final := h.waitTerminal(rid, 15*time.Second)
	// c2 正常跑完且全部成功；c1 被取消、c4 被跳过 → run 仍是 succeeded
	if final["status"] != "succeeded" {
		t.Fatalf("run 状态: %v (%v)", final["status"], final["error"])
	}
	_, steps := listSteps(t, h, rid)
	if steps[0].Status != "canceled" || steps[1].Status != "succeeded" || steps[2].Status != "skipped" {
		t.Fatalf("步骤状态: %+v", steps)
	}
	// 只有实际执行的 c2 产出了 tier 指标
	resp, body = h.do("GET", "/runs/"+rid+"/results", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/results: %d %s", resp.StatusCode, body)
	}
	var res struct {
		Summary map[string]any `json:"summary"`
		Cache   struct {
			TotalRequests int `json:"total_requests"`
			Tiers         []struct {
				Bucket string `json:"bucket"`
			} `json:"tiers"`
		} `json:"cache"`
	}
	mustJSON(t, body, &res)
	if len(res.Cache.Tiers) != 1 || res.Cache.Tiers[0].Bucket != "c2" {
		t.Fatalf("应只剩 c2 档指标: %+v", res.Cache.Tiers)
	}
	if res.Cache.TotalRequests != 12 {
		t.Fatalf("聚合请求数应只含 c2: %d", res.Cache.TotalRequests)
	}
}

// TestAllStepsSkippedCancelsRun 排队中跳过全部步骤 → run 以 canceled 收口，且不算失败。
func TestAllStepsSkippedCancelsRun(t *testing.T) {
	t.Setenv("THVV_FAKE_STEP_MS", "1500")
	t.Setenv("THVV_FAKE_FAIL_RATE", "0")
	t.Setenv("THVV_LIMITS__MAX_CONCURRENT_RUNS", "1")
	h := newHarness(t)
	h.login()
	perfTid := mkStepTarget(t, h, "step-blocker")
	cacheTid := mkStepTarget(t, h, "step-allskip")

	// 占住唯一 worker
	resp, body := h.do("POST", "/runs", map[string]any{
		"tool": "thvv", "capability": "perf", "target_id": perfTid,
		"plan": map[string]any{"mode": "single", "buckets": []string{"1k"},
			"parallel_ladder": []int{1}, "number": 1},
	}, map[string]string{"Idempotency-Key": "step-blocker-1"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("提交 blocker: %d %s", resp.StatusCode, body)
	}
	var blocker map[string]any
	mustJSON(t, body, &blocker)
	blockerID := blocker["run_id"].(string)
	for i := 0; i < 200; i++ {
		if st, _ := listSteps(t, h, blockerID); st == "running" {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}

	// 提交 2 档 cache run（排在 blocker 后，一定停在 queued）
	resp, body = h.do("POST", "/runs", map[string]any{
		"tool": "lmcache", "capability": "cache", "target_id": cacheTid,
		"plan": map[string]any{
			"workload": "multi-round-chat", "duration_sec": 30, "qps": 1,
			"shared_prompt_length": 2000, "chat_history_length": 10000,
			"tokens_per_gb_kvcache": 65536,
			"ladder":                []map[string]any{{"sessions": 1}, {"sessions": 2}},
		},
	}, map[string]string{"Idempotency-Key": "step-allskip-1"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("提交 cache: %d %s", resp.StatusCode, body)
	}
	var run map[string]any
	mustJSON(t, body, &run)
	rid := run["run_id"].(string)

	// 排队中把两个 step 都跳过
	for idx := 0; idx < 2; idx++ {
		resp, body = h.do("POST", fmt.Sprintf("/runs/%s/steps/%d/skip", rid, idx), nil, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("跳过 step %d: %d %s", idx, resp.StatusCode, body)
		}
	}
	// 放掉 blocker
	if resp, _ := h.do("POST", "/runs/"+blockerID+"/cancel", nil, nil); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("取消 blocker: %d", resp.StatusCode)
	}

	final := h.waitTerminal(rid, 15*time.Second)
	if final["status"] != "canceled" {
		t.Fatalf("全部跳过应收口为 canceled，实际 %v（%v）", final["status"], final["error"])
	}
	_, steps := listSteps(t, h, rid)
	if len(steps) != 2 || steps[0].Status != "skipped" || steps[1].Status != "skipped" {
		t.Fatalf("步骤应全部 skipped: %+v", steps)
	}
	resp, body = h.do("GET", "/runs/"+rid, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("取 run: %d %s", resp.StatusCode, body)
	}
	var detail struct {
		Summary map[string]any `json:"summary"`
	}
	mustJSON(t, body, &detail)
	if detail.Summary["verdict"] != "canceled" {
		t.Fatalf("summary.verdict 应为 canceled: %v", detail.Summary)
	}
}

// TestPerfStepSkip 性能压测矩阵：跳过待执行档位，其余档位照常执行。
func TestPerfStepSkip(t *testing.T) {
	t.Setenv("THVV_FAKE_STEP_MS", "700")
	t.Setenv("THVV_FAKE_FAIL_RATE", "0")
	h := newHarness(t)
	h.login()
	tid := mkStepTarget(t, h, "perf-skip")

	resp, body := h.do("POST", "/runs", map[string]any{
		"tool": "thvv", "capability": "perf", "target_id": tid,
		"plan": map[string]any{
			"mode": "matrix", "buckets": []string{"1k", "9k"},
			"parallel_ladder": []int{1}, "number": 1,
		},
	}, map[string]string{"Idempotency-Key": "perf-skip-1"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("提交: %d %s", resp.StatusCode, body)
	}
	var run map[string]any
	mustJSON(t, body, &run)
	rid := run["run_id"].(string)

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		status, steps := listSteps(t, h, rid)
		if status == "running" && len(steps) == 2 && steps[0].Status == "running" {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	resp, body = h.do("POST", "/runs/"+rid+"/steps/1/skip", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("跳过 step: %d %s", resp.StatusCode, body)
	}
	final := h.waitTerminal(rid, 15*time.Second)
	if final["status"] != "succeeded" {
		t.Fatalf("run 状态: %v (%v)", final["status"], final["error"])
	}
	_, steps := listSteps(t, h, rid)
	if len(steps) != 2 || steps[0].Status != "succeeded" || steps[1].Status != "skipped" {
		t.Fatalf("步骤状态: %+v", steps)
	}
	resp, body = h.do("GET", "/runs/"+rid+"/results", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/results: %d %s", resp.StatusCode, body)
	}
	var res struct {
		Metrics struct {
			PerStep []struct {
				Bucket string `json:"bucket"`
			} `json:"per_step"`
		} `json:"metrics"`
	}
	mustJSON(t, body, &res)
	if len(res.Metrics.PerStep) != 1 || res.Metrics.PerStep[0].Bucket != "1k" {
		t.Fatalf("跳过后应只剩 1 个档位指标: %+v", res.Metrics.PerStep)
	}
}

// TestEvalStepSkip 效果评测：跳过待执行数据集，其余数据集照常出分。
func TestEvalStepSkip(t *testing.T) {
	t.Setenv("THVV_FAKE_STEP_MS", "700")
	t.Setenv("THVV_FAKE_FAIL_RATE", "0")
	h := newHarness(t)
	h.login()
	tid := mkStepTarget(t, h, "eval-skip")

	resp, body := h.do("POST", "/runs", map[string]any{
		"tool": "thvv", "capability": "eval", "target_id": tid,
		"plan": map[string]any{
			"datasets": []string{"aime26", "gpqa_diamond"},
			"limit":    2, "repeats": 1,
		},
	}, map[string]string{"Idempotency-Key": "eval-skip-1"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("提交: %d %s", resp.StatusCode, body)
	}
	var run map[string]any
	mustJSON(t, body, &run)
	rid := run["run_id"].(string)

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		status, steps := listSteps(t, h, rid)
		if status == "running" && len(steps) == 2 && steps[0].Status == "running" {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	resp, body = h.do("POST", "/runs/"+rid+"/steps/1/skip", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("跳过 step: %d %s", resp.StatusCode, body)
	}
	final := h.waitTerminal(rid, 15*time.Second)
	if final["status"] != "succeeded" {
		t.Fatalf("run 状态: %v (%v)", final["status"], final["error"])
	}
	_, steps := listSteps(t, h, rid)
	if len(steps) != 2 || steps[0].Status != "succeeded" || steps[1].Status != "skipped" {
		t.Fatalf("步骤状态: %+v", steps)
	}
	resp, body = h.do("GET", "/runs/"+rid+"/results", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/results: %d %s", resp.StatusCode, body)
	}
	var res struct {
		Eval struct {
			Datasets []struct {
				Dataset string `json:"dataset"`
			} `json:"datasets"`
		} `json:"eval"`
		Summary map[string]any `json:"summary"`
	}
	mustJSON(t, body, &res)
	if len(res.Eval.Datasets) != 1 || res.Eval.Datasets[0].Dataset != "aime26" {
		t.Fatalf("跳过后应只剩 1 个数据集结果: %+v", res.Eval.Datasets)
	}
	if res.Summary["datasets_total"].(float64) != 1 {
		t.Fatalf("datasets_total 应为 1: %v", res.Summary)
	}
}
