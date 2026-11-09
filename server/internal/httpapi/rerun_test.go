package httpapi_test

import (
	"strings"
	"testing"
	"time"
)

// TestRerunFailedRun 失败记录一键重测：同 target / 同 plan，新 run 带 labels.rerun_of。
func TestRerunFailedRun(t *testing.T) {
	t.Setenv("THVV_FAKE_STEP_MS", "10")
	t.Setenv("THVV_FAKE_FAIL_RATE", "1") // builtin 引擎每个数据集都失败 → run=failed
	h := newHarness(t)
	h.login()

	resp, body := h.do("POST", "/targets", map[string]any{
		"name": "rerun-target", "vendor": "zhipu",
		"api_url":  "https://api.example.com/v1/chat/completions",
		"protocol": "openai", "model": "glm-5.2", "api_key": "sk-rr-1234",
	}, nil)
	if resp.StatusCode != 201 {
		t.Fatalf("建端点: %d %s", resp.StatusCode, body)
	}
	var target map[string]any
	mustJSON(t, body, &target)

	resp, body = h.do("POST", "/runs", map[string]any{
		"tool": "thvv", "capability": "eval", "target_id": target["target_id"],
		"plan":   map[string]any{"datasets": []string{"aime26", "gpqa_diamond"}, "limit": 3},
		"labels": map[string]string{"round": "r1"},
	}, map[string]string{"Idempotency-Key": "rerun-orig"})
	if resp.StatusCode != 202 {
		t.Fatalf("提交: %d %s", resp.StatusCode, body)
	}
	var orig map[string]any
	mustJSON(t, body, &orig)
	origID := orig["run_id"].(string)
	final := h.waitTerminal(origID, 20*time.Second)
	if st, _ := final["status"].(string); st != "failed" {
		t.Fatalf("前置条件：原 run 应 failed，实际 %v", st)
	}

	// 重测
	resp, body = h.do("POST", "/runs/"+origID+"/rerun", nil, nil)
	if resp.StatusCode != 202 {
		t.Fatalf("重测应 202，实际 %d %s", resp.StatusCode, body)
	}
	var again map[string]any
	mustJSON(t, body, &again)
	newID, _ := again["run_id"].(string)
	if newID == "" || newID == origID {
		t.Fatalf("重测应产生新 run_id，实得 %q", newID)
	}
	// plan / target / capability 必须一致
	op, np := orig["plan"].(map[string]any), again["plan"].(map[string]any)
	if np["steps_total"].(float64) != op["steps_total"].(float64) {
		t.Fatalf("steps_total 不一致: %v vs %v", np["steps_total"], op["steps_total"])
	}
	od, nd := op["datasets"].([]any), np["datasets"].([]any)
	if len(od) != len(nd) {
		t.Fatalf("datasets 不一致: %v vs %v", od, nd)
	}
	if again["capability"] != "eval" {
		t.Fatalf("capability 未继承: %v", again["capability"])
	}
	if again["target"].(map[string]any)["target_id"] != target["target_id"] {
		t.Fatalf("target 未继承: %v", again["target"])
	}
	labels, _ := again["labels"].(map[string]any)
	if labels["rerun_of"] != origID || labels["round"] != "r1" {
		t.Fatalf("labels 未正确打标: %v", labels)
	}

	// 新 run 会真的跑起来（这里只确认能进终态且原记录仍在）
	h.waitTerminal(newID, 20*time.Second)
	if resp, _ := h.do("GET", "/runs/"+origID, nil, nil); resp.StatusCode != 200 {
		t.Fatal("重测不应删除原记录")
	}
}

// TestRerunGuards 运行中 / 内联 target / 端点已删 都不能重测。
func TestRerunGuards(t *testing.T) {
	t.Setenv("THVV_FAKE_STEP_MS", "1500")
	t.Setenv("THVV_FAKE_FAIL_RATE", "0")
	h := newHarness(t)
	h.login()

	// 内联 target（ephemeral）：提交后等待终态，重测应 409
	resp, body := h.do("POST", "/runs", map[string]any{
		"tool": "thvv", "capability": "perf",
		"target": map[string]any{
			"api_url":  "https://api.example.com/v1/chat/completions",
			"protocol": "openai", "model": "glm-5.2", "api_key": "sk-eph-1234",
		},
		"plan": map[string]any{"mode": "single", "buckets": []string{"1k"}, "parallel_ladder": []int{1}, "number": 1},
	}, nil)
	if resp.StatusCode != 202 {
		t.Fatalf("提交内联 run: %d %s", resp.StatusCode, body)
	}
	var eph map[string]any
	mustJSON(t, body, &eph)
	ephID := eph["run_id"].(string)

	// 运行中（此时是 queued/running）→ 400
	if resp, body := h.do("POST", "/runs/"+ephID+"/rerun", nil, nil); resp.StatusCode != 400 {
		t.Fatalf("运行中重测应 400，实际 %d %s", resp.StatusCode, body)
	}
	h.waitTerminal(ephID, 25*time.Second)
	if resp, _ := h.do("POST", "/runs/"+ephID+"/rerun", nil, nil); resp.StatusCode != 409 {
		t.Fatalf("内联 target 重测应 409，实际 %d", resp.StatusCode)
	}

	// 端点被删 → 400 并说明原因
	resp, body = h.do("POST", "/targets", map[string]any{
		"name": "rerun-gone", "vendor": "zhipu",
		"api_url":  "https://api.example.com/v1/chat/completions",
		"protocol": "openai", "model": "glm-5.2", "api_key": "sk-gone-1234",
	}, nil)
	if resp.StatusCode != 201 {
		t.Fatalf("建端点: %d %s", resp.StatusCode, body)
	}
	var tg map[string]any
	mustJSON(t, body, &tg)
	resp, body = h.do("POST", "/runs", map[string]any{
		"tool": "thvv", "capability": "perf", "target_id": tg["target_id"],
		"plan": map[string]any{"mode": "single", "buckets": []string{"1k"}, "parallel_ladder": []int{1}, "number": 1},
	}, nil)
	if resp.StatusCode != 202 {
		t.Fatalf("提交 run: %d %s", resp.StatusCode, body)
	}
	var r2 map[string]any
	mustJSON(t, body, &r2)
	runID := r2["run_id"].(string)
	h.waitTerminal(runID, 25*time.Second)
	if resp, _ := h.do("DELETE", "/targets/"+tg["target_id"].(string), nil, nil); resp.StatusCode != 204 {
		t.Fatalf("删端点失败: %d", resp.StatusCode)
	}
	resp, body = h.do("POST", "/runs/"+runID+"/rerun", nil, nil)
	if resp.StatusCode != 400 {
		t.Fatalf("端点已删时重测应 400，实际 %d %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "已被删除") {
		t.Fatalf("错误信息应说明端点已删除: %s", body)
	}
}

// TestRerunKeepsJudgeKey judge 凭据（密文列）在重测时能解回并重新加密，回显仍只有掩码。
func TestRerunKeepsJudgeKey(t *testing.T) {
	t.Setenv("THVV_FAKE_STEP_MS", "10")
	t.Setenv("THVV_FAKE_FAIL_RATE", "0")
	h := newHarness(t)
	h.login()

	resp, body := h.do("POST", "/targets", map[string]any{
		"name": "rerun-judge", "vendor": "test-1",
		"api_url":  "https://api.example.com/v1/chat/completions",
		"protocol": "openai", "model": "glm-5.3-test", "api_key": "sk-t-1234",
	}, nil)
	if resp.StatusCode != 201 {
		t.Fatalf("建端点: %d %s", resp.StatusCode, body)
	}
	var target map[string]any
	mustJSON(t, body, &target)

	const judgeKey = "sk-judge-rerun-KEY-4321"
	resp, body = h.do("POST", "/runs", map[string]any{
		"tool": "thvv", "capability": "eval", "target_id": target["target_id"],
		"plan": map[string]any{
			"datasets": []string{"aime26"}, "limit": 1,
			"judge": map[string]any{"model": "glm-5.3", "base_url": "https://gw.example/v1", "api_key": judgeKey},
		},
	}, nil)
	if resp.StatusCode != 202 {
		t.Fatalf("提交: %d %s", resp.StatusCode, body)
	}
	if strings.Contains(string(body), judgeKey) {
		t.Fatal("提交响应不应包含 judge 明文")
	}
	var orig map[string]any
	mustJSON(t, body, &orig)
	origID := orig["run_id"].(string)
	h.waitTerminal(origID, 20*time.Second)

	resp, body = h.do("POST", "/runs/"+origID+"/rerun", nil, nil)
	if resp.StatusCode != 202 {
		t.Fatalf("重测: %d %s", resp.StatusCode, body)
	}
	if strings.Contains(string(body), judgeKey) {
		t.Fatal("重测响应不应包含 judge 明文")
	}
	var again map[string]any
	mustJSON(t, body, &again)
	judge, _ := again["plan"].(map[string]any)["judge"].(map[string]any)
	if judge == nil {
		t.Fatal("重测后的 run 应保留 judge 配置")
	}
	if hint, _ := judge["api_key_hint"].(string); !strings.HasSuffix(hint, "4321") {
		t.Fatalf("judge 掩码未继承: %v", judge)
	}
	if _, leaked := judge["api_key"]; leaked {
		t.Fatalf("重测响应出现 api_key 字段: %v", judge)
	}
}
