package httpapi_test

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTerminalRun 建 target + 提交一个 verf run 并等到终态，返回 run_id。
func (h *harness) newTerminalRun(t *testing.T, key string, capName string, plan map[string]any) string {
	t.Helper()
	resp, body := h.do("POST", "/targets", map[string]any{
		"name": "del-target-" + key, "vendor": "zhipu",
		"api_url":  "https://api.example.com/v1/chat/completions",
		"protocol": "openai", "model": "glm-5.2", "api_key": "sk-del-1234",
	}, nil)
	if resp.StatusCode != 201 {
		t.Fatalf("建端点: %d %s", resp.StatusCode, body)
	}
	var target map[string]any
	mustJSON(t, body, &target)
	resp, body = h.do("POST", "/runs", map[string]any{
		"tool": "thvv", "capability": capName, "target_id": target["target_id"], "plan": plan,
	}, map[string]string{"Idempotency-Key": "del-" + key})
	if resp.StatusCode != 202 {
		t.Fatalf("提交 run: %d %s", resp.StatusCode, body)
	}
	var run map[string]any
	mustJSON(t, body, &run)
	return h.waitTerminal(run["run_id"].(string), 20*time.Second)["run_id"].(string)
}

// TestDeleteRun 验证 DELETE /v1/runs/{id}：204 → 再取 404，产物与 run 目录一并清理。
func TestDeleteRun(t *testing.T) {
	t.Setenv("THVV_FAKE_STEP_MS", "20")
	t.Setenv("THVV_FAKE_FAIL_RATE", "0")
	h := newHarness(t)
	h.login()

	runID := h.newTerminalRun(t, "one", "perf", map[string]any{
		"mode": "single", "buckets": []string{"1k"}, "parallel_ladder": []int{1}, "number": 1,
	})

	// 删除前：有产物、有 run 目录
	resp, body := h.do("GET", "/runs/"+runID+"/artifacts", nil, nil)
	if resp.StatusCode != 200 || !strings.Contains(string(body), "artifact_id") {
		t.Fatalf("删除前应能读到产物: %d %s", resp.StatusCode, body)
	}
	runDir := filepath.Join(string(h.cfg.Paths.RunsDir), runID)
	if _, err := os.Stat(runDir); err != nil {
		t.Fatalf("删除前 run 目录应存在: %v", err)
	}

	// 删除
	if resp, body := h.do("DELETE", "/runs/"+runID, nil, nil); resp.StatusCode != 204 {
		t.Fatalf("删除应 204，实际 %d %s", resp.StatusCode, body)
	}

	// 删除后：404；run 目录与产物文件都不在了
	if resp, _ := h.do("GET", "/runs/"+runID, nil, nil); resp.StatusCode != 404 {
		t.Fatalf("删除后读取应 404，实际 %d", resp.StatusCode)
	}
	if _, err := os.Stat(runDir); !os.IsNotExist(err) {
		t.Fatalf("run 目录应被删除，err=%v", err)
	}
	// 列表里也不应再出现
	_, body = h.do("GET", "/runs?limit=50", nil, nil)
	if strings.Contains(string(body), runID) {
		t.Fatal("已删除的 run 仍出现在列表里")
	}
	// 重复删除 → 404
	if resp, _ := h.do("DELETE", "/runs/"+runID, nil, nil); resp.StatusCode != 404 {
		t.Fatalf("重复删除应 404，实际 %d", resp.StatusCode)
	}
}

// TestDeleteRunningRunRejected 运行中的 run 不能删（409），取消后才能删。
func TestDeleteRunningRunRejected(t *testing.T) {
	t.Setenv("THVV_FAKE_STEP_MS", "1500")
	t.Setenv("THVV_FAKE_FAIL_RATE", "0")
	h := newHarness(t)
	h.login()

	resp, body := h.do("POST", "/targets", map[string]any{
		"name": "del-running", "vendor": "zhipu",
		"api_url":  "https://api.example.com/v1/chat/completions",
		"protocol": "openai", "model": "glm-5.2", "api_key": "sk-run-1234",
	}, nil)
	if resp.StatusCode != 201 {
		t.Fatalf("建端点: %d %s", resp.StatusCode, body)
	}
	var target map[string]any
	mustJSON(t, body, &target)
	resp, body = h.do("POST", "/runs", map[string]any{
		"tool": "thvv", "capability": "perf", "target_id": target["target_id"],
		"plan": map[string]any{"mode": "single", "buckets": []string{"1k"}, "parallel_ladder": []int{1}, "number": 1},
	}, nil)
	if resp.StatusCode != 202 {
		t.Fatalf("提交 run: %d %s", resp.StatusCode, body)
	}
	var run map[string]any
	mustJSON(t, body, &run)
	runID := run["run_id"].(string)

	// 等它真的进 running
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		_, b := h.do("GET", "/runs/"+runID, nil, nil)
		if strings.Contains(string(b), `"status":"running"`) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if resp, body := h.do("DELETE", "/runs/"+runID, nil, nil); resp.StatusCode != http.StatusConflict {
		t.Fatalf("运行中删除应 409，实际 %d %s", resp.StatusCode, body)
	}
	// 取消后可以删
	if resp, _ := h.do("POST", "/runs/"+runID+"/cancel", nil, nil); resp.StatusCode != 202 {
		t.Fatalf("取消失败: %d", resp.StatusCode)
	}
	final := h.waitTerminal(runID, 20*time.Second)
	if st, _ := final["status"].(string); st != "canceled" {
		t.Fatalf("取消后应为 canceled，实际 %v", st)
	}
	if resp, body := h.do("DELETE", "/runs/"+runID, nil, nil); resp.StatusCode != 204 {
		t.Fatalf("取消后删除应 204，实际 %d %s", resp.StatusCode, body)
	}
}

// TestPurgeRuns 批量清理：dry_run 只报数不删；执行后只剩未命中的记录。
func TestPurgeRuns(t *testing.T) {
	// 单 worker + 长 step：占位任务 keep 会占满整个测试期间，后两条一定停在 queued，
	// cancel 后状态稳定为 canceled（不依赖执行时序）。
	t.Setenv("THVV_FAKE_STEP_MS", "20000")
	t.Setenv("THVV_LIMITS__MAX_CONCURRENT_RUNS", "1")
	t.Setenv("THVV_FAKE_FAIL_RATE", "0")
	h := newHarness(t)
	h.login()

	mkTarget := func(name string) string {
		resp, body := h.do("POST", "/targets", map[string]any{
			"name": name, "vendor": "zhipu",
			"api_url":  "https://api.example.com/v1/chat/completions",
			"protocol": "openai", "model": "glm-5.2", "api_key": "sk-p-1234",
		}, nil)
		if resp.StatusCode != 201 {
			t.Fatalf("建端点: %d %s", resp.StatusCode, body)
		}
		var target map[string]any
		mustJSON(t, body, &target)
		return target["target_id"].(string)
	}
	submit := func(tid, key string) string {
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

	// 占位任务：占住唯一的 worker（不等待它结束）
	keep := submit(mkTarget("purge-keep"), "purge-keep")
	t.Cleanup(func() { _, _ = h.do("POST", "/runs/"+keep+"/cancel", nil, nil) })

	// 两条排队中的 run，提交后立刻取消 → 稳定为 canceled
	var canceled []string
	for _, key := range []string{"c1", "c2"} {
		rid := submit(mkTarget("purge-"+key), "purge-"+key)
		if resp, _ := h.do("POST", "/runs/"+rid+"/cancel", nil, nil); resp.StatusCode != 202 {
			t.Fatalf("取消失败: %d", resp.StatusCode)
		}
		canceled = append(canceled, rid)
	}
	for _, rid := range canceled {
		if st, _ := h.waitTerminal(rid, 10*time.Second)["status"].(string); st != "canceled" {
			t.Fatalf("%s 取消后应为 canceled，实际 %v", rid, st)
		}
	}

	// dry_run：报告将删除 2 条且不实际删除
	resp, body := h.do("POST", "/runs/purge", map[string]any{"statuses": []string{"canceled"}, "dry_run": true}, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("dry_run: %d %s", resp.StatusCode, body)
	}
	var dry struct {
		Deleted int      `json:"deleted"`
		DryRun  bool     `json:"dry_run"`
		RunIDs  []string `json:"run_ids"`
	}
	mustJSON(t, body, &dry)
	if !dry.DryRun || len(dry.RunIDs) != 2 || dry.Deleted != 0 {
		t.Fatalf("dry_run 结果异常: %+v", dry)
	}
	if resp, _ := h.do("GET", "/runs/"+canceled[0], nil, nil); resp.StatusCode != 200 {
		t.Fatal("dry_run 不应真删")
	}

	// 真删
	resp, body = h.do("POST", "/runs/purge", map[string]any{"statuses": []string{"canceled"}}, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("purge: %d %s", resp.StatusCode, body)
	}
	var res struct {
		Deleted int `json:"deleted"`
		Skipped int `json:"skipped_active"`
	}
	mustJSON(t, body, &res)
	if res.Deleted != 2 {
		t.Fatalf("应删除 2 条，实际 %d", res.Deleted)
	}
	for _, rid := range canceled {
		if resp, _ := h.do("GET", "/runs/"+rid, nil, nil); resp.StatusCode != 404 {
			t.Fatalf("purge 后 %s 应 404，实际 %d", rid, resp.StatusCode)
		}
	}
	// 未命中的记录（还在跑的 keep）必须保留
	if resp, _ := h.do("GET", "/runs/"+keep, nil, nil); resp.StatusCode != 200 {
		t.Fatalf("未命中的 run 不应被删，实际 %d", resp.StatusCode)
	}

	// 非终态状态不允许批量删
	if resp, body := h.do("POST", "/runs/purge", map[string]any{"statuses": []string{"running"}}, nil); resp.StatusCode != 400 {
		t.Fatalf("非终态 purge 应 400，实际 %d %s", resp.StatusCode, body)
	}
}

// TestPurgeByRunIDs 勾选删除：只删指定的 id，未勾选的保留；运行中的计入 skipped_active。
func TestPurgeByRunIDs(t *testing.T) {
	t.Setenv("THVV_FAKE_STEP_MS", "20")
	t.Setenv("THVV_FAKE_FAIL_RATE", "0")
	h := newHarness(t)
	h.login()

	plan := map[string]any{"mode": "single", "buckets": []string{"1k"}, "parallel_ladder": []int{1}, "number": 1}
	keep := h.newTerminalRun(t, "sel-keep", "perf", plan)
	del1 := h.newTerminalRun(t, "sel-del1", "perf", plan)
	del2 := h.newTerminalRun(t, "sel-del2", "perf", plan)

	// dry_run 只报命中
	resp, body := h.do("POST", "/runs/purge", map[string]any{"run_ids": []string{del1, del2}, "dry_run": true}, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("dry_run: %d %s", resp.StatusCode, body)
	}
	var dry struct {
		Deleted int      `json:"deleted"`
		RunIDs  []string `json:"run_ids"`
	}
	mustJSON(t, body, &dry)
	if len(dry.RunIDs) != 2 || dry.Deleted != 0 {
		t.Fatalf("dry_run 异常: %+v", dry)
	}

	// 真删（混入一个不存在的 id 应被忽略、一个未勾选的不受影响）
	resp, body = h.do("POST", "/runs/purge", map[string]any{
		"run_ids": []string{del1, del2, "run_not_exist"},
	}, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("purge: %d %s", resp.StatusCode, body)
	}
	var res struct {
		Deleted int `json:"deleted"`
	}
	mustJSON(t, body, &res)
	if res.Deleted != 2 {
		t.Fatalf("应删除 2 条，实际 %d", res.Deleted)
	}
	for _, rid := range []string{del1, del2} {
		if resp, _ := h.do("GET", "/runs/"+rid, nil, nil); resp.StatusCode != 404 {
			t.Fatalf("%s 应已删除（404），实际 %d", rid, resp.StatusCode)
		}
	}
	if resp, _ := h.do("GET", "/runs/"+keep, nil, nil); resp.StatusCode != 200 {
		t.Fatalf("未勾选的记录不应被删，实际 %d", resp.StatusCode)
	}
}

// TestArtifactDownloadHeaders 下载接口：中文文件名要给 ASCII fallback + filename*，且不出非 ASCII 原始字节。
func TestArtifactDownloadHeaders(t *testing.T) {
	t.Setenv("THVV_FAKE_STEP_MS", "10")
	t.Setenv("THVV_FAKE_FAIL_RATE", "0")
	h := newHarness(t)
	h.login()

	runID := h.newTerminalRun(t, "dl", "perf", map[string]any{
		"mode": "single", "buckets": []string{"1k"}, "parallel_ladder": []int{1}, "number": 1,
	})
	resp, body := h.do("GET", "/runs/"+runID+"/artifacts", nil, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("列产物: %d %s", resp.StatusCode, body)
	}
	var arts struct {
		Items []struct {
			Kind        string `json:"kind"`
			Filename    string `json:"filename"`
			DownloadURL string `json:"download_url"`
		} `json:"items"`
	}
	mustJSON(t, body, &arts)
	if len(arts.Items) == 0 {
		t.Fatal("内置引擎应产出报告产物")
	}
	for _, a := range arts.Items {
		req, _ := http.NewRequest("GET", h.srv.URL+a.DownloadURL, nil)
		res, err := h.client.Do(req)
		if err != nil {
			t.Fatalf("下载 %s: %v", a.Filename, err)
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatalf("下载 %s 应 200，实际 %d", a.Filename, res.StatusCode)
		}
		cd := res.Header.Get("Content-Disposition")
		if !strings.Contains(cd, `filename="`) || !strings.Contains(cd, "filename*=UTF-8''") {
			t.Fatalf("%s 的 Content-Disposition 缺少 ASCII fallback 或 filename*: %q", a.Filename, cd)
		}
		for _, r := range cd {
			if r > 0x7e {
				t.Fatalf("Content-Disposition 含非 ASCII 字节（代理会挂）: %q", cd)
			}
		}
		if got := res.Header.Get("X-Content-Type-Options"); got != "nosniff" {
			t.Fatalf("缺少 X-Content-Type-Options: %q", got)
		}
	}
}
