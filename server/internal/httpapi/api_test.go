package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/apexcloud/thvv/server/internal/config"
	"github.com/apexcloud/thvv/server/internal/engine"
	"github.com/apexcloud/thvv/server/internal/httpapi"
	"github.com/apexcloud/thvv/server/internal/runmgr"
	"github.com/apexcloud/thvv/server/internal/secret"
	"github.com/apexcloud/thvv/server/internal/store"
)

// 公开示例：口令 password 的 bcrypt hash
const testPasswordHash = "$2a$10$IBU8oAPb/vYyjaHjJHAhMuaUS9nblXw3MuVZLit8R88wEZEZ.e9xC"

type harness struct {
	t      *testing.T
	srv    *httptest.Server
	client *http.Client
	cfg    *config.Config
	dir    string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()

	// 伪造 THVV 根目录（指纹校验需要 perf/run.sh）
	thvv := filepath.Join(dir, "thvv")
	mustDir(t, filepath.Join(thvv, "perf", "scripts"))
	mustDir(t, filepath.Join(thvv, "perf", "datasets_clean"))
	mustWrite(t, filepath.Join(thvv, "perf", "run.sh"), "#!/bin/sh\n")
	mustWrite(t, filepath.Join(thvv, "perf", "scripts", "setup_tokenizer.py"), "")
	mustWrite(t, filepath.Join(thvv, "perf", "datasets_clean", "perf_zh_1k.jsonl"), "{}\n")

	cfgPath := filepath.Join(dir, "thvv.yaml")
	mustWrite(t, cfgPath, fmt.Sprintf(`
version: 1
roots:
  thvv: %q
  data: %q
server:
  listen: "127.0.0.1:0"
  write_timeout: 0s
auth:
  mode: both
  session: { ttl: 1h, secure: false, signing_key: test-signing-key }
  tokens:
    - { name: ci, token: test-token, role: operator }
    - { name: ro, token: viewer-token, role: viewer }
  users:
    - username: admin
      password_hash: %q
      role: admin
store:
  driver: sqlite
  sqlite: { path: "data:test.db" }
secrets:
  provider: env
  env: { key: "0123456789abcdef0123456789abcdef" }
paths:
  datasets_dir: "thvv:perf/datasets_clean"
engines:
  thvv:
    enabled: true
    kind: builtin
    capabilities:
      perf: { enabled: true, status: ga }
      eval: { enabled: true, status: ga }
      functional: { enabled: false, status: planned, available_at: "v1.2" }
observability:
  log_level: error
  access_log: false
`, thvv, filepath.Join(dir, "data"), testPasswordHash))

	cfg, err := config.Load(config.LoadOptions{Path: cfgPath})
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}

	st, err := store.Open(string(cfg.Store.SQLite.Path), time.Second, 4)
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	box, err := secret.New(cfg.Secrets.MasterKey)
	if err != nil {
		t.Fatalf("初始化加密失败: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := engine.NewRegistry(cfg)
	mgr := runmgr.New(cfg, st, reg, box, log)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	mgr.Start(ctx)

	h := httpapi.New(cfg, st, reg, mgr, box, log, "test", "test")
	srv := httptest.NewServer(h.Handler())
	t.Cleanup(srv.Close)

	jar, _ := cookiejar.New(nil)
	hc := &http.Client{Jar: jar, Timeout: 30 * time.Second}

	return &harness{t: t, srv: srv, client: hc, cfg: cfg, dir: dir}
}

func (h *harness) do(method, path string, body any, headers map[string]string) (*http.Response, []byte) {
	h.t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			h.t.Fatalf("marshal: %v", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, h.srv.URL+"/v1"+path, rdr)
	if err != nil {
		h.t.Fatalf("new request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp, data
}

func (h *harness) login() {
	h.t.Helper()
	resp, body := h.do("POST", "/auth/login", map[string]string{
		"username": "admin", "password": "password",
	}, nil)
	if resp.StatusCode != http.StatusNoContent {
		h.t.Fatalf("登录失败: %d %s", resp.StatusCode, body)
	}
}

// TestFullFlow 覆盖 UI 用到的全部关键接口：登录 → 建端点 → 预检 → 提交 → 轮询 → 结果 → 产物下载。
func TestFullFlow(t *testing.T) {
	h := newHarness(t)

	// 未登录必须 401
	if resp, _ := h.do("GET", "/meta", nil, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("未登录访问 /meta 应 401，实际 %d", resp.StatusCode)
	}

	h.login()

	// 会话
	resp, body := h.do("GET", "/auth/session", nil, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("/auth/session: %d %s", resp.StatusCode, body)
	}
	var sess struct {
		Username string   `json:"username"`
		Roles    []string `json:"roles"`
	}
	mustJSON(t, body, &sess)
	if sess.Username != "admin" || len(sess.Roles) != 1 || sess.Roles[0] != "admin" {
		t.Fatalf("会话内容异常: %+v", sess)
	}

	// 参考数据
	for _, p := range []string{"/meta", "/tools", "/catalog/buckets", "/catalog/gates", "/catalog/datasets"} {
		if resp, body := h.do("GET", p, nil, nil); resp.StatusCode != 200 {
			t.Fatalf("%s: %d %s", p, resp.StatusCode, body)
		}
	}
	_, b := h.do("GET", "/catalog/buckets", nil, nil)
	var cat struct {
		Items []struct {
			Label       string `json:"label"`
			MaxParallel int    `json:"max_parallel"`
			BuiltIn     bool   `json:"built_in"`
		} `json:"items"`
		DefaultLadder []int `json:"default_ladder"`
	}
	mustJSON(t, b, &cat)
	if len(cat.Items) != 9 {
		t.Fatalf("档位数量应为 9，实际 %d", len(cat.Items))
	}
	if cat.Items[0].Label != "1k" || cat.Items[0].MaxParallel != 128 {
		t.Fatalf("档位 1k 定义异常: %+v", cat.Items[0])
	}

	// 建端点
	resp, body = h.do("POST", "/targets", map[string]any{
		"name": "glm-test", "vendor": "zhipu",
		"api_url":  "https://api.example.com/v1/chat/completions",
		"protocol": "openai", "model": "glm-5.2", "api_key": "sk-secret-abcd1234",
	}, map[string]string{"Idempotency-Key": "t1"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("建端点: %d %s", resp.StatusCode, body)
	}
	var target map[string]any
	mustJSON(t, body, &target)
	targetID, _ := target["target_id"].(string)
	if targetID == "" {
		t.Fatal("缺少 target_id")
	}
	// 凭据不得回显
	if strings.Contains(string(body), "sk-secret-abcd1234") {
		t.Fatal("响应泄露了明文 api_key")
	}
	if hint, _ := target["api_key_hint"].(string); !strings.HasSuffix(hint, "1234") {
		t.Fatalf("api_key_hint 异常: %q", hint)
	}
	// tokenizer 自动推断
	tok, _ := target["tokenizer"].(map[string]any)
	if tok["value"] != "zai-org/GLM-4.6" || tok["source"] != "auto_detected" {
		t.Fatalf("tokenizer 推断异常: %+v", tok)
	}
	// 重名冲突
	if resp, _ := h.do("POST", "/targets", map[string]any{
		"name": "glm-test", "api_url": "https://a.com/v1/chat/completions",
		"protocol": "openai", "model": "m", "api_key": "k",
	}, nil); resp.StatusCode != http.StatusConflict {
		t.Fatalf("重名应 409，实际 %d", resp.StatusCode)
	}

	// 预检
	resp, body = h.do("POST", "/targets/"+targetID+"/probe", map[string]any{"timeout_sec": 5}, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("预检: %d %s", resp.StatusCode, body)
	}
	var probe struct {
		OK     bool `json:"ok"`
		Checks []struct {
			Name string `json:"name"`
			OK   bool   `json:"ok"`
		} `json:"checks"`
	}
	mustJSON(t, body, &probe)
	if len(probe.Checks) < 3 {
		t.Fatalf("预检项过少: %+v", probe)
	}

	// 提交单档任务
	resp, body = h.do("POST", "/runs", map[string]any{
		"tool": "thvv", "capability": "perf", "target_id": targetID,
		"plan": map[string]any{
			"mode": "single", "buckets": []string{"1k"},
			"parallel_ladder": []int{4}, "number": 10,
		},
		"gate":   map[string]any{"profile": "perf-sla-v1"},
		"report": map[string]any{"formats": []string{"html", "xlsx"}, "client_name": "测试"},
	}, map[string]string{"Idempotency-Key": "run-1"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("提交任务: %d %s", resp.StatusCode, body)
	}
	var run map[string]any
	mustJSON(t, body, &run)
	runID, _ := run["run_id"].(string)
	if runID == "" {
		t.Fatalf("缺少 run_id: %s", body)
	}
	if st, _ := run["status"].(string); st != string("queued") && st != "preparing" {
		t.Fatalf("初始状态异常: %s", st)
	}
	// steps 预置
	if steps, _ := run["steps"].([]any); len(steps) != 1 {
		t.Fatalf("应预置 1 个 step，实际 %d", len(steps))
	}

	// 幂等：同 key 返回同一个 run
	resp, body2 := h.do("POST", "/runs", map[string]any{
		"tool": "thvv", "capability": "perf", "target_id": targetID,
		"plan": map[string]any{"mode": "single", "buckets": []string{"1k"}, "parallel_ladder": []int{4}, "number": 10},
	}, map[string]string{"Idempotency-Key": "run-1"})
	var run2 map[string]any
	mustJSON(t, body2, &run2)
	if run2["run_id"] != runID {
		t.Fatalf("幂等未生效: %v vs %v", run2["run_id"], runID)
	}

	// 等待终态
	final := h.waitTerminal(runID, 20*time.Second)
	if st := final["status"]; st != "succeeded" && st != "partial" {
		t.Fatalf("任务未成功: %v (%v)", st, final["error"])
	}

	// 结果
	resp, body = h.do("GET", "/runs/"+runID+"/results", nil, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("/results: %d %s", resp.StatusCode, body)
	}
	var res struct {
		GateResult map[string]any `json:"gate_result"`
		Summary    map[string]any `json:"summary"`
		Metrics    struct {
			PerStep []map[string]any `json:"per_step"`
		} `json:"metrics"`
	}
	mustJSON(t, body, &res)
	if len(res.Metrics.PerStep) != 1 {
		t.Fatalf("per_step 数量应为 1，实际 %d", len(res.Metrics.PerStep))
	}
	step := res.Metrics.PerStep[0]
	for _, k := range []string{"requests", "ttft", "tpot", "itl", "throughput", "token_rate_avg"} {
		if _, ok := step[k]; !ok {
			t.Fatalf("per_step 缺少字段 %q", k)
		}
	}
	if res.GateResult == nil {
		t.Fatal("缺少 gate_result")
	}
	if _, ok := res.Summary["success_rate"]; !ok {
		t.Fatalf("summary 缺少 success_rate: %+v", res.Summary)
	}

	// 产物
	resp, body = h.do("GET", "/runs/"+runID+"/artifacts", nil, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("/artifacts: %d", resp.StatusCode)
	}
	var arts struct {
		Items []struct {
			ID          string `json:"artifact_id"`
			Kind        string `json:"kind"`
			Filename    string `json:"filename"`
			DownloadURL string `json:"download_url"`
		} `json:"items"`
	}
	mustJSON(t, body, &arts)
	kinds := map[string]bool{}
	for _, a := range arts.Items {
		kinds[a.Kind] = true
	}
	for _, want := range []string{"html_report", "xlsx_report", "sla_json"} {
		if !kinds[want] {
			t.Fatalf("缺少产物 %q（实际 %v）", want, kinds)
		}
	}

	// 下载产物：中文文件名、ETag、内容非空
	var html *struct {
		ID, Name string
	}
	for i := range arts.Items {
		if arts.Items[i].Kind == "html_report" {
			html = &struct{ ID, Name string }{arts.Items[i].ID, arts.Items[i].Filename}
		}
	}
	if html == nil {
		t.Fatal("找不到 html_report")
	}
	dlReq, _ := http.NewRequest("GET", h.srv.URL+"/v1/artifacts/"+html.ID, nil)
	dlResp, err := h.client.Do(dlReq)
	if err != nil {
		t.Fatalf("下载失败: %v", err)
	}
	defer dlResp.Body.Close()
	if dlResp.StatusCode != 200 {
		t.Fatalf("下载状态 %d", dlResp.StatusCode)
	}
	if cd := dlResp.Header.Get("Content-Disposition"); !strings.Contains(cd, "filename*=UTF-8''") {
		t.Fatalf("Content-Disposition 未使用 RFC 5987: %q", cd)
	}
	htmlBody, _ := io.ReadAll(dlResp.Body)
	if !bytes.Contains(htmlBody, []byte("性能压测报告")) {
		t.Fatal("HTML 报告内容异常")
	}
	if dlResp.Header.Get("ETag") == "" {
		t.Fatal("缺少 ETag")
	}

	// 重新生成报告：应原地更新（artifact_id 不变），并返回 200
	oldIDs := map[string]string{}
	for _, a := range arts.Items {
		oldIDs[a.Kind] = a.ID
	}
	resp, body = h.do("POST", "/runs/"+runID+"/reports", map[string]any{"formats": []string{"html", "xlsx"}}, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("/reports: %d %s", resp.StatusCode, body)
	}
	var re struct {
		Artifacts []struct {
			ID   string `json:"artifact_id"`
			Kind string `json:"kind"`
		} `json:"artifacts"`
	}
	mustJSON(t, body, &re)
	seen := map[string]string{}
	for _, a := range re.Artifacts {
		seen[a.Kind] = a.ID
	}
	for _, k := range []string{"html_report", "xlsx_report"} {
		if seen[k] == "" || seen[k] != oldIDs[k] {
			t.Fatalf("重新生成应复用 artifact_id: %s old=%s new=%s", k, oldIDs[k], seen[k])
		}
	}

	// 历史列表
	resp, body = h.do("GET", "/runs?limit=10", nil, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("/runs 列表: %d", resp.StatusCode)
	}
	var list struct {
		Items []map[string]any `json:"items"`
	}
	mustJSON(t, body, &list)
	if len(list.Items) != 1 {
		t.Fatalf("历史应 1 条，实际 %d", len(list.Items))
	}
}

// TestCapabilityNotImplemented 验证预留能力的 501 响应。
func TestCapabilityNotImplemented(t *testing.T) {
	h := newHarness(t)
	h.login()
	resp, body := h.do("GET", "/tools", nil, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("/tools: %d", resp.StatusCode)
	}
	if !strings.Contains(string(body), "planned") {
		t.Fatalf("tools 未体现 planned 状态: %s", body)
	}

	resp, body = h.do("POST", "/targets", map[string]any{
		"name": "t1", "api_url": "https://a.com/v1/chat/completions",
		"protocol": "openai", "model": "m", "api_key": "k",
	}, nil)
	var target map[string]any
	mustJSON(t, body, &target)

	resp, body = h.do("POST", "/runs", map[string]any{
		"tool": "thvv", "capability": "functional",
		"target_id": target["target_id"],
		"plan":      map[string]any{"mode": "suite", "suites": []string{"tool_call"}},
	}, nil)
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("未实现能力应 501，实际 %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "not_implemented") {
		t.Fatalf("错误码不是 not_implemented: %s", body)
	}
}

// TestValidation 校验层。
func TestValidation(t *testing.T) {
	h := newHarness(t)
	h.login()

	cases := []struct {
		name string
		body map[string]any
		code int
	}{
		{"url 缺协议", map[string]any{"name": "a", "api_url": "api.com/v1/chat/completions",
			"protocol": "openai", "model": "m", "api_key": "k"}, 400},
		{"openai 路径不匹配", map[string]any{"name": "b", "api_url": "https://a.com/v1/messages",
			"protocol": "openai", "model": "m", "api_key": "k"}, 400},
		{"缺 api_key", map[string]any{"name": "c", "api_url": "https://a.com/v1/chat/completions",
			"protocol": "openai", "model": "m"}, 400},
		{"非法 protocol", map[string]any{"name": "d", "api_url": "https://a.com/v1/chat/completions",
			"protocol": "grpc", "model": "m", "api_key": "k"}, 400},
	}
	for _, c := range cases {
		resp, body := h.do("POST", "/targets", c.body, nil)
		if resp.StatusCode != c.code {
			t.Errorf("%s: 期望 %d，实际 %d (%s)", c.name, c.code, resp.StatusCode, body)
		}
	}

	// 未知档位
	_, body := h.do("POST", "/targets", map[string]any{
		"name": "ok", "api_url": "https://a.com/v1/chat/completions",
		"protocol": "openai", "model": "m", "api_key": "k",
	}, nil)
	var target map[string]any
	mustJSON(t, body, &target)
	resp, body := h.do("POST", "/runs", map[string]any{
		"tool": "thvv", "capability": "perf", "target_id": target["target_id"],
		"plan": map[string]any{"mode": "single", "buckets": []string{"999k"}, "parallel_ladder": []int{1}, "number": 1},
	}, nil)
	if resp.StatusCode != 400 {
		t.Fatalf("未知档位应 400，实际 %d: %s", resp.StatusCode, body)
	}
}

// TestAuthRoles 校验 RBAC。
func TestAuthRoles(t *testing.T) {
	h := newHarness(t)
	// viewer token 不能写
	resp, body := h.do("POST", "/targets", map[string]any{
		"name": "x", "api_url": "https://a.com/v1/chat/completions",
		"protocol": "openai", "model": "m", "api_key": "k",
	}, map[string]string{"Authorization": "Bearer viewer-token"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("viewer 写操作应 403，实际 %d: %s", resp.StatusCode, body)
	}
	// operator token 可以读
	if resp, _ := h.do("GET", "/meta", nil, map[string]string{"Authorization": "Bearer test-token"}); resp.StatusCode != 200 {
		t.Fatalf("operator 读应 200，实际 %d", resp.StatusCode)
	}
	// 错误口令
	if resp, _ := h.do("POST", "/auth/login", map[string]string{"username": "admin", "password": "wrong"}, nil); resp.StatusCode != 401 {
		t.Fatalf("错误口令应 401，实际 %d", resp.StatusCode)
	}
}

// ---------------- helpers ----------------

func (h *harness) waitTerminal(runID string, timeout time.Duration) map[string]any {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	var last map[string]any
	for time.Now().Before(deadline) {
		resp, body := h.do("GET", "/runs/"+runID+"?include=steps", nil, nil)
		if resp.StatusCode != 200 {
			h.t.Fatalf("轮询: %d %s", resp.StatusCode, body)
		}
		var run map[string]any
		mustJSON(h.t, body, &run)
		last = run
		if st, _ := run["status"].(string); st == "succeeded" || st == "partial" ||
			st == "failed" || st == "canceled" {
			return run
		}
		time.Sleep(80 * time.Millisecond)
	}
	h.t.Fatalf("等待终态超时，最后一次: %v", last["status"])
	return nil
}

func mustJSON(t *testing.T, b []byte, dst any) {
	t.Helper()
	if err := json.Unmarshal(b, dst); err != nil {
		t.Fatalf("解析响应失败: %v\n%s", err, b)
	}
}

func mustDir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestEvalFlow 验证 capability=eval 的全链路：提交 → 步骤 → 结果 → 产物。
func TestEvalFlow(t *testing.T) {
	t.Setenv("THVV_FAKE_STEP_MS", "20")
	t.Setenv("THVV_FAKE_FAIL_RATE", "0")
	h := newHarness(t)
	h.login()

	resp, body := h.do("POST", "/targets", map[string]any{
		"name": "eval-target", "vendor": "zhipu",
		"api_url":  "https://api.example.com/v1/chat/completions",
		"protocol": "openai", "model": "glm-5.2", "api_key": "sk-secret-1234",
	}, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("建端点: %d %s", resp.StatusCode, body)
	}
	var target map[string]any
	mustJSON(t, body, &target)
	targetID, _ := target["target_id"].(string)

	resp, body = h.do("POST", "/runs", map[string]any{
		"tool": "thvv", "capability": "eval", "target_id": targetID,
		"plan": map[string]any{
			"datasets": []string{"aime26", "gpqa_diamond"},
			"limit":    4, "repeats": 1, "eval_batch_size": 4,
		},
	}, map[string]string{"Idempotency-Key": "eval-1"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("提交评测: %d %s", resp.StatusCode, body)
	}
	var run map[string]any
	mustJSON(t, body, &run)
	runID, _ := run["run_id"].(string)
	if runID == "" {
		t.Fatal("缺少 run_id")
	}
	plan := run["plan"].(map[string]any)
	if int(plan["steps_total"].(float64)) != 2 {
		t.Fatalf("steps_total 期望 2，实际 %v", plan["steps_total"])
	}

	final := h.waitTerminal(runID, 20*time.Second)
	if final["status"] != "succeeded" {
		t.Fatalf("评测未成功: %v (%v)", final["status"], final["error"])
	}

	resp, body = h.do("GET", "/runs/"+runID+"/results", nil, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("/results: %d %s", resp.StatusCode, body)
	}
	var res struct {
		Capability string `json:"capability"`
		Summary    map[string]any
		Eval       struct {
			Datasets []struct {
				Dataset string  `json:"dataset"`
				Success bool    `json:"success"`
				Score   float64 `json:"score"`
			} `json:"datasets"`
		} `json:"eval"`
	}
	mustJSON(t, body, &res)
	if res.Capability != "eval" || len(res.Eval.Datasets) != 2 {
		t.Fatalf("eval 结果异常: %+v", res)
	}
	for _, d := range res.Eval.Datasets {
		if !d.Success || d.Score <= 0 {
			t.Fatalf("数据集结果异常: %+v", d)
		}
	}
	if res.Summary["verdict"] != "pass" {
		t.Fatalf("verdict 期望 pass，实际 %v", res.Summary["verdict"])
	}

	resp, body = h.do("GET", "/runs/"+runID+"/artifacts", nil, nil)
	var arts struct {
		Items []struct {
			Kind string `json:"kind"`
		} `json:"items"`
	}
	mustJSON(t, body, &arts)
	found := false
	for _, a := range arts.Items {
		if a.Kind == "eval_summary" {
			found = true
		}
	}
	if !found {
		t.Fatalf("缺少 eval_summary 产物: %+v", arts.Items)
	}
}

// TestEvalValidation 数据集未知 / 缺 Judge key / 未知 capability 的校验。
func TestEvalValidation(t *testing.T) {
	t.Setenv("THVV_FAKE_STEP_MS", "10")
	h := newHarness(t)
	h.login()

	resp, body := h.do("POST", "/targets", map[string]any{
		"name": "eval-v", "vendor": "zhipu",
		"api_url":  "https://api.example.com/v1/chat/completions",
		"protocol": "openai", "model": "glm-5.2", "api_key": "sk-x",
	}, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("建端点: %d %s", resp.StatusCode, body)
	}
	var target map[string]any
	mustJSON(t, body, &target)
	tid, _ := target["target_id"].(string)

	cases := []struct {
		name string
		plan map[string]any
		code int
	}{
		{"未知数据集", map[string]any{"datasets": []string{"no_such_ds"}}, http.StatusBadRequest},
		{"空数据集", map[string]any{"datasets": []string{}}, http.StatusBadRequest},
		{"缺 Judge key", map[string]any{"datasets": []string{"hle"}}, http.StatusBadRequest},
		{"repeats 超限", map[string]any{"datasets": []string{"aime26"}, "repeats": 999}, http.StatusBadRequest},
	}
	for _, c := range cases {
		resp, body := h.do("POST", "/runs", map[string]any{
			"tool": "thvv", "capability": "eval", "target_id": tid, "plan": c.plan,
		}, map[string]string{"Idempotency-Key": uuidForTest(c.name)})
		if resp.StatusCode != c.code {
			t.Fatalf("%s: 期望 %d，实际 %d %s", c.name, c.code, resp.StatusCode, body)
		}
	}
}

func uuidForTest(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r < 128 {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// TestEvalGate 内置 eval-sla-v1 模板：builtin 引擎的模拟分数会触发 gate_result。
func TestEvalGate(t *testing.T) {
	t.Setenv("THVV_FAKE_STEP_MS", "10")
	t.Setenv("THVV_FAKE_FAIL_RATE", "0")
	h := newHarness(t)
	h.login()

	resp, body := h.do("POST", "/targets", map[string]any{
		"name": "eval-gate", "vendor": "zhipu",
		"api_url":  "https://api.example.com/v1/chat/completions",
		"protocol": "openai", "model": "glm-5.2", "api_key": "sk-x",
	}, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("建端点: %d %s", resp.StatusCode, body)
	}
	var target map[string]any
	mustJSON(t, body, &target)
	tid, _ := target["target_id"].(string)

	resp, body = h.do("POST", "/runs", map[string]any{
		"tool": "thvv", "capability": "eval", "target_id": tid,
		"plan": map[string]any{"datasets": []string{"aime26"}, "limit": 4},
	}, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("提交: %d %s", resp.StatusCode, body)
	}
	var run map[string]any
	mustJSON(t, body, &run)
	runID, _ := run["run_id"].(string)

	final := h.waitTerminal(runID, 20*time.Second)
	if final["status"] != "succeeded" {
		t.Fatalf("run 未成功: %v", final["error"])
	}

	resp, body = h.do("GET", "/runs/"+runID+"/results", nil, nil)
	var res struct {
		Summary map[string]any `json:"summary"`
		Eval    struct {
			Datasets []struct {
				Dataset string  `json:"dataset"`
				Score   float64 `json:"score"`
			} `json:"datasets"`
		} `json:"eval"`
	}
	mustJSON(t, body, &res)
	if len(res.Eval.Datasets) != 1 {
		t.Fatalf("数据集结果数异常: %+v", res.Eval)
	}
	// builtin 模拟分数 0.4~0.95，低于 aime26 默认下限 0.8912，gate 应为 false 且带 violation
	resp, body = h.do("GET", "/runs/"+runID, nil, nil)
	var runFull struct {
		GateResult *struct {
			Passed     bool `json:"passed"`
			Profile    string
			Violations []struct {
				Rule string `json:"rule"`
			} `json:"violations"`
		} `json:"gate_result"`
	}
	mustJSON(t, body, &runFull)
	if runFull.GateResult == nil {
		t.Fatal("eval run 应有 gate_result")
	}
	if runFull.GateResult.Profile != "eval-sla-v1" {
		t.Fatalf("gate profile 期望 eval-sla-v1，实际 %q", runFull.GateResult.Profile)
	}
	if runFull.GateResult.Passed && res.Eval.Datasets[0].Score < 0.8912 {
		t.Fatal("分数低于下限却判通过")
	}
}
