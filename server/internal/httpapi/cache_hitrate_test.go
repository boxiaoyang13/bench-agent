package httpapi_test

import (
	"net/http"
	"testing"
)

// TestCacheHitRatePlan 目标命中率模式：服务端反推每轮输入/输出，min/max 互斥。
func TestCacheHitRatePlan(t *testing.T) {
	t.Setenv("THVV_FAKE_STEP_MS", "5")
	t.Setenv("THVV_FAKE_FAIL_RATE", "0")
	h := newHarness(t)
	h.login()
	tid := mkStepTarget(t, h, "hitrate-mode")

	submit := func(key string, extra map[string]any) (int, map[string]any) {
		t.Helper()
		plan := map[string]any{
			"workload": "multi-round-chat", "duration_sec": 30, "qps": 1,
			"shared_prompt_length": 2000, "chat_history_length": 10000,
			"tokens_per_gb_kvcache": 65536,
		}
		for k, v := range extra {
			plan[k] = v
		}
		resp, body := h.do("POST", "/runs", map[string]any{
			"tool": "lmcache", "capability": "cache", "target_id": tid, "plan": plan,
		}, map[string]string{"Idempotency-Key": key})
		var run map[string]any
		if resp.StatusCode == http.StatusAccepted {
			mustJSON(t, body, &run)
		}
		return resp.StatusCode, run
	}

	// min 80% → 输入 2800 / 输出 200，plan 里能看到反推结果
	code, run := submit("hitrate-min", map[string]any{"prefix_hit_rate_min": 0.80})
	if code != http.StatusAccepted {
		t.Fatalf("提交 min 80%%: %d", code)
	}
	plan := run["plan"].(map[string]any)
	if int(plan["user_input_length"].(float64)) != 2800 || int(plan["output_length"].(float64)) != 200 {
		t.Fatalf("min 80%% 反推结果错误: input=%v output=%v",
			plan["user_input_length"], plan["output_length"])
	}
	if plan["prefix_hit_rate_min"].(float64) != 0.80 {
		t.Fatalf("plan 应保留目标命中率: %v", plan["prefix_hit_rate_min"])
	}

	// max 80% → 输入 3384 / 输出 200
	code, run = submit("hitrate-max", map[string]any{"prefix_hit_rate_max": 0.80})
	if code != http.StatusAccepted {
		t.Fatalf("提交 max 80%%: %d", code)
	}
	plan = run["plan"].(map[string]any)
	if int(plan["user_input_length"].(float64)) != 3384 || int(plan["output_length"].(float64)) != 200 {
		t.Fatalf("max 80%% 反推结果错误: input=%v output=%v",
			plan["user_input_length"], plan["output_length"])
	}

	// 互斥：同时设置 min/max → 400
	if code, _ := submit("hitrate-both", map[string]any{
		"prefix_hit_rate_min": 0.7, "prefix_hit_rate_max": 0.9,
	}); code != http.StatusBadRequest {
		t.Fatalf("min+max 应 400，实际 %d", code)
	}

	// 缺 shared/history → 400（本 harness 的 cache capability 没有默认形状）
	resp, body := h.do("POST", "/runs", map[string]any{
		"tool": "lmcache", "capability": "cache", "target_id": tid,
		"plan": map[string]any{
			"workload": "multi-round-chat", "duration_sec": 30,
			"prefix_hit_rate_min": 0.8,
		},
	}, map[string]string{"Idempotency-Key": "hitrate-noshape"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("缺形状应 400，实际 %d %s", resp.StatusCode, body)
	}
}
