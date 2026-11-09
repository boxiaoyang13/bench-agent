package runmgr

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/apexcloud/thvv/server/internal/domain"
	"github.com/apexcloud/thvv/server/internal/engine"
)

const sampleEvalLog = `
  [stderr] 2026-11-07 11:32:14 - evalscope - WARNING: Attempt 1 / 5 failed: Error code: 400 - {'error': {'message': 'glm-5.3-test is not a multimodal model', 'type': 'BadRequestError', 'param': None, 'code': 400}}. Retrying...
  [stderr] 2026-11-07 11:32:24 - evalscope - WARNING: Attempt 2 / 5 failed: Error code: 400 - {'error': {'message': 'glm-5.3-test is not a multimodal model', 'type': 'BadRequestError', 'param': None, 'code': 400}}. Retrying...
  [stderr] 2026-11-07 16:20:37 - evalscope - WARNING: Attempt 1 / 5 failed: Error code: 502 - {'error': {'message': 'upstream connection failed: dial tcp 10.0.0.1:8000: connect: connection refused', 'type': 'server_error', 'code': 502}}. Retrying...
  [stderr] 2026-11-07 14:18:37 - evalscope - ERROR: Failed to generate report table: No objects to concatenate
  [stderr] 2026-11-07 14:18:37 - evalscope - WARNING: No reports found under /tmp/eval/reports. Generating an empty report.
  [stderr] 2026-11-07 14:18:37 - evalscope - WARNING: Error ignored, continuing with next sample.
  [stderr] 2026-11-07 14:18:37 - evalscope - WARNING: Error ignored, continuing with next sample.
`

func writeTempLog(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "engine.log")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("写日志失败: %v", err)
	}
	return p
}

func TestSummarizeEngineLog(t *testing.T) {
	p := writeTempLog(t, sampleEvalLog)
	got := summarizeEngineLog(p, "sk-secret-abcdef")
	if len(got) == 0 {
		t.Fatal("应从日志提炼出摘要")
	}
	joined := strings.Join(got, "\n")
	for _, want := range []string{"HTTP 400", "not a multimodal model", "容错跳过 2 条样本"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("摘要缺少 %q：\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "\x1b") {
		t.Fatal("摘要里不应残留 ANSI 控制符")
	}
}

func TestSummarizeEngineLogRedactsSecrets(t *testing.T) {
	p := writeTempLog(t, "  [stderr] ERROR: 鉴权失败 key=sk-judge-plain-xyz\n")
	got := summarizeEngineLog(p, "sk-judge-plain-xyz")
	if joined := strings.Join(got, "\n"); strings.Contains(joined, "sk-judge-plain-xyz") {
		t.Fatalf("摘要泄漏凭据: %s", joined)
	}
}

func TestSummarizeEngineLogMissingFile(t *testing.T) {
	if got := summarizeEngineLog(filepath.Join(t.TempDir(), "nope.log")); got != nil {
		t.Fatalf("日志不存在应返回空，实际 %v", got)
	}
}

func TestProblemWithLog(t *testing.T) {
	p := writeTempLog(t, sampleEvalLog)
	prob := problemWithLog(500, "eval_failed", "3/3 个数据集评测失败（hallusion_bench）", p, "sk-x-123456")
	if prob.Code != "eval_failed" || prob.Status != 500 {
		t.Fatalf("problem 字段异常: %+v", prob)
	}
	if !strings.Contains(prob.Detail, "3/3 个数据集评测失败") {
		t.Fatalf("detail 应保留 headline: %s", prob.Detail)
	}
	if !strings.Contains(prob.Detail, "日志摘要：") {
		t.Fatalf("detail 应附日志摘要: %s", prob.Detail)
	}
	if prob.Type == "" || prob.Title == "" {
		t.Fatalf("problem 应规范化 type/title: %+v", prob)
	}
	if len([]rune(prob.Detail)) > errorDetailMaxLen+1 {
		t.Fatalf("detail 未按上限截断: %d", len([]rune(prob.Detail)))
	}
}

func TestEvalFailureHeadline(t *testing.T) {
	res := engine.Result{Eval: &domain.EvalMetrics{Datasets: []domain.EvalDatasetResult{
		{Dataset: "hallusion_bench", Label: "hallusion_bench", Success: false, Error: "没有有效样本"},
		{Dataset: "aime26", Label: "aime26", Success: true, Score: 0.9},
	}}}
	head := evalFailureHeadline(res)
	if !strings.Contains(head, "1/2 个数据集评测失败") || !strings.Contains(head, "hallusion_bench") {
		t.Fatalf("headline 异常: %s", head)
	}
	if !strings.Contains(head, "没有有效样本") {
		t.Fatalf("headline 应带首因: %s", head)
	}

	if got := evalFailureHeadline(engine.Result{}); got == "" {
		t.Fatal("无结果时也应给出兜底说明")
	}
	if got := partialNotice(res); got != "hallusion_bench" {
		t.Fatalf("partial 提示异常: %q", got)
	}
}

func TestProgressHeadlineDefault(t *testing.T) {
	lr := &liveRun{run: &domain.Run{
		Progress: domain.Progress{StepsDone: 1, StepsTotal: 15},
		Steps: []domain.Step{
			{Index: 0, Bucket: "aime26", Status: domain.StepSucceeded},
			{Index: 1, Bucket: "hle", Status: domain.StepFailed},
			{Index: 2, Bucket: "mmlu_pro", Status: domain.StepRunning},
		},
	}}
	got := progressHeadline(lr)
	if !strings.Contains(got, "2/15") {
		t.Fatalf("进度摘要应以 step 状态统计（2/15）: %s", got)
	}
}

// TestProbeEndpoint 上游探测：2xx / 5xx / 连接失败 三种情况都要有可读结论。
func TestProbeEndpoint(t *testing.T) {
	// 502 + 供应商原话
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(502)
		_, _ = w.Write([]byte(`{"error":{"message":"upstream connection failed: dial tcp 10.0.0.1:8000: connect: connection refused"}}`))
	}))
	defer bad.Close()
	res := probeEndpoint(context.Background(), engine.ResolvedTarget{
		APIURL: bad.URL, Protocol: domain.ProtocolOpenAI, Model: "m", APIKey: "sk-x",
	}, 3*time.Second)
	if res["ok"] != false || res["status"] != 502 {
		t.Fatalf("502 探测结果异常: %+v", res)
	}
	if !strings.Contains(fmt.Sprint(res["body_excerpt"]), "connection refused") {
		t.Fatalf("应保留响应片段: %+v", res)
	}

	// 2xx
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"pong"}}]}`))
	}))
	defer good.Close()
	res2 := probeEndpoint(context.Background(), engine.ResolvedTarget{
		APIURL: good.URL, Protocol: domain.ProtocolOpenAI, Model: "m", APIKey: "sk-x",
	}, 3*time.Second)
	if res2["ok"] != true || res2["status"] != 200 {
		t.Fatalf("200 探测结果异常: %+v", res2)
	}

	// 连不上 → 归类为 connection_refused / network
	res3 := probeEndpoint(context.Background(), engine.ResolvedTarget{
		APIURL: "http://127.0.0.1:1/v1/chat/completions", Protocol: domain.ProtocolOpenAI, Model: "m", APIKey: "sk-x",
	}, 2*time.Second)
	if res3["ok"] != false {
		t.Fatalf("连接失败应 ok=false: %+v", res3)
	}
	if cls, _ := res3["error_class"].(string); cls == "" {
		t.Fatalf("应给出 error_class: %+v", res3)
	}
}
