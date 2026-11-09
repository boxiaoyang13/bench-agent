package httpapi_test

import (
	"strings"
	"testing"
	"time"
)

// TestEvalJudgeKeyNeverEchoed judge 凭据的契约：写入可见 hint，任何响应/库里都没有明文。
func TestEvalJudgeKeyNeverEchoed(t *testing.T) {
	t.Setenv("THVV_FAKE_STEP_MS", "20")
	t.Setenv("THVV_FAKE_FAIL_RATE", "0")
	h := newHarness(t)
	h.login()

	resp, body := h.do("POST", "/targets", map[string]any{
		"name": "judge-target", "vendor": "test-1",
		"api_url":  "https://api.example.com/v1/chat/completions",
		"protocol": "openai", "model": "glm-5.3-test", "api_key": "sk-target-plain-1234",
	}, nil)
	if resp.StatusCode != 201 {
		t.Fatalf("建端点: %d %s", resp.StatusCode, body)
	}
	var target map[string]any
	mustJSON(t, body, &target)
	targetID, _ := target["target_id"].(string)

	const judgeKey = "sk-judge-plain-ABCD9876"
	resp, body = h.do("POST", "/runs", map[string]any{
		"tool": "thvv", "capability": "eval", "target_id": targetID,
		"plan": map[string]any{
			"datasets": []string{"aime26"},
			"limit":    2, "repeats": 1, "eval_batch_size": 2,
			"judge": map[string]any{
				"model": "glm-5.3", "base_url": "https://gw.example/v1", "api_key": judgeKey,
			},
		},
	}, map[string]string{"Idempotency-Key": "judge-1"})
	if resp.StatusCode != 202 {
		t.Fatalf("提交评测: %d %s", resp.StatusCode, body)
	}
	if strings.Contains(string(body), judgeKey) {
		t.Fatalf("202 响应泄漏 judge 明文: %s", body)
	}
	var run map[string]any
	mustJSON(t, body, &run)
	runID, _ := run["run_id"].(string)

	judge, _ := run["plan"].(map[string]any)["judge"].(map[string]any)
	if judge == nil {
		t.Fatalf("响应应保留 judge 元信息: %s", body)
	}
	if _, leaked := judge["api_key"]; leaked {
		t.Fatalf("judge 对象不应出现 api_key 字段: %v", judge)
	}
	if hint, _ := judge["api_key_hint"].(string); !strings.HasSuffix(hint, "9876") {
		t.Fatalf("judge.api_key_hint 异常: %q", hint)
	}
	if judge["model"] != "glm-5.3" || judge["base_url"] != "https://gw.example/v1" {
		t.Fatalf("judge 元信息丢失: %v", judge)
	}

	final := h.waitTerminal(runID, 20*time.Second)
	if st, _ := final["status"].(string); st != "succeeded" {
		t.Fatalf("评测未成功: %v (%v)", st, final["error"])
	}

	// 详情 / 列表 / 结果 / 事件都不能带明文
	for _, p := range []string{"/runs/" + runID + "?include=steps", "/runs?limit=50"} {
		if _, b := h.do("GET", p, nil, nil); strings.Contains(string(b), judgeKey) {
			t.Fatalf("%s 泄漏 judge 明文", p)
		}
	}
}

// TestEvalAllDatasetsFailedSetsError 全数据集失败时 run.error 必须能一眼看出原因。
func TestEvalAllDatasetsFailedSetsError(t *testing.T) {
	t.Setenv("THVV_FAKE_STEP_MS", "10")
	t.Setenv("THVV_FAKE_FAIL_RATE", "1") // 让 builtin 引擎每个数据集都失败
	h := newHarness(t)
	h.login()

	resp, body := h.do("POST", "/targets", map[string]any{
		"name": "fail-target", "vendor": "test-1",
		"api_url":  "https://api.example.com/v1/chat/completions",
		"protocol": "openai", "model": "glm-5.3-test", "api_key": "sk-x-1234",
	}, nil)
	if resp.StatusCode != 201 {
		t.Fatalf("建端点: %d %s", resp.StatusCode, body)
	}
	var target map[string]any
	mustJSON(t, body, &target)

	resp, body = h.do("POST", "/runs", map[string]any{
		"tool": "thvv", "capability": "eval", "target_id": target["target_id"],
		"plan": map[string]any{"datasets": []string{"aime26", "gpqa_diamond"}, "limit": 2, "repeats": 1},
	}, nil)
	if resp.StatusCode != 202 {
		t.Fatalf("提交评测: %d %s", resp.StatusCode, body)
	}
	var run map[string]any
	mustJSON(t, body, &run)
	runID, _ := run["run_id"].(string)

	final := h.waitTerminal(runID, 20*time.Second)
	if st, _ := final["status"].(string); st != "failed" {
		t.Fatalf("全部数据集失败时 run 应为 failed，实际 %v", st)
	}
	prob, _ := final["error"].(map[string]any)
	if prob == nil {
		t.Fatalf("failed run 必须带 error，实际 %v", final)
	}
	if code, _ := prob["code"].(string); code != "eval_failed" {
		t.Fatalf("error.code 期望 eval_failed，实际 %q", code)
	}
	detail, _ := prob["detail"].(string)
	if !strings.Contains(detail, "个数据集评测失败") {
		t.Fatalf("error.detail 应说明失败数据集: %q", detail)
	}
	if !strings.Contains(detail, "aime26") {
		t.Fatalf("error.detail 应点名失败的数据集: %q", detail)
	}
	// RFC 9457 规范化字段
	if tp, _ := prob["type"].(string); !strings.HasPrefix(tp, "https://thvv.dev/errors/") {
		t.Fatalf("error.type 未规范化: %q", tp)
	}
}

// TestCancelQueuedRunNeverStarts 排队期间取消的 run 不能再被 worker 拉起。
// （曾经的现象：cancel 后 worker 出队仍执行，把 11h 的 mmlu_pro 跑起来、堵住整个队列。）
func TestCancelQueuedRunNeverStarts(t *testing.T) {
	t.Setenv("THVV_FAKE_STEP_MS", "600") // 让第一个 run 占住 worker
	t.Setenv("THVV_FAKE_FAIL_RATE", "0")
	h := newHarness(t)
	h.login()

	resp, body := h.do("POST", "/targets", map[string]any{
		"name": "q-target", "vendor": "zhipu",
		"api_url":  "https://api.example.com/v1/chat/completions",
		"protocol": "openai", "model": "glm-5.2", "api_key": "sk-q-1234",
	}, nil)
	if resp.StatusCode != 201 {
		t.Fatalf("建端点: %d %s", resp.StatusCode, body)
	}
	var target map[string]any
	mustJSON(t, body, &target)
	tid := target["target_id"]

	newRun := func(key string) string {
		resp, body := h.do("POST", "/runs", map[string]any{
			"tool": "thvv", "capability": "perf", "target_id": tid,
			"plan": map[string]any{"mode": "single", "buckets": []string{"1k"}, "parallel_ladder": []int{1}, "number": 1},
		}, map[string]string{"Idempotency-Key": key})
		if resp.StatusCode != 202 {
			t.Fatalf("提交 run: %d %s", resp.StatusCode, body)
		}
		var run map[string]any
		mustJSON(t, body, &run)
		return run["run_id"].(string)
	}
	first := newRun("q-1")
	second := newRun("q-2") // 同一 target 串行 → 排队

	// 确认第二个还在排队，然后取消
	if resp, b := h.do("GET", "/runs/"+second, nil, nil); resp.StatusCode != 200 {
		t.Fatalf("读 run: %d %s", resp.StatusCode, b)
	}
	if resp, _ := h.do("POST", "/runs/"+second+"/cancel", nil, nil); resp.StatusCode != 202 {
		t.Fatalf("取消排队 run 应 202，实际 %d", resp.StatusCode)
	}

	// 等第一个跑完，再给被取消的 run 一点时间：它不应被启动
	h.waitTerminal(first, 20*time.Second)
	time.Sleep(1500 * time.Millisecond)

	resp, body = h.do("GET", "/runs/"+second+"?include=steps", nil, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("读 run: %d %s", resp.StatusCode, body)
	}
	var run map[string]any
	mustJSON(t, body, &run)
	if st, _ := run["status"].(string); st != "canceled" {
		t.Fatalf("被取消的排队 run 状态应为 canceled，实际 %q", st)
	}
	steps, _ := run["steps"].([]any)
	for _, s := range steps {
		m := s.(map[string]any)
		if st, _ := m["status"].(string); st != "pending" && st != "canceled" && st != "skipped" {
			t.Fatalf("被取消的 run 不应执行到 step，实际 step 状态 %q", st)
		}
	}
}
