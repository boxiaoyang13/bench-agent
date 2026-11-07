package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/apexcloud/thvv/server/internal/domain"
)

// TestEvalBaseURL 与 eval/run.sh 的推导一致：去掉完整请求路径后缀。
func TestEvalBaseURL(t *testing.T) {
	cases := map[string]string{
		"https://api.x.com/v1/chat/completions":  "https://api.x.com/v1",
		"https://api.x.com/v1/chat/completions/": "https://api.x.com/v1",
		"https://api.x.com/v1/messages":          "https://api.x.com",
		"https://api.x.com/v1":                   "https://api.x.com/v1",
	}
	for in, want := range cases {
		if got := evalBaseURL(in); got != want {
			t.Errorf("evalBaseURL(%q)=%q，期望 %q", in, got, want)
		}
	}
}

// TestEvalArgs plan → argv 映射（limit/repeats/batch/judge/thinking/base_url）。
func TestEvalArgs(t *testing.T) {
	thinking := false
	s := &EvalEngine{script: "/opt/thvv/eval/scripts/run_eval.py"}
	spec := Spec{
		Target: ResolvedTarget{
			Model: "glm-5.3-test", Vendor: "turbo", Protocol: domain.ProtocolOpenAI,
			APIURL: "https://api.example.com/v1/chat/completions", APIKey: "sk-target-secret",
		},
		Plan: domain.Plan{
			ProviderName: "turbo",
			Thinking:     &thinking,
			Request:      domain.RequestDefaults{MaxTokens: 8192},
			Judge:        &domain.JudgeSpec{Model: "deepseek-v4-pro", BaseURL: "https://api.deepseek.com/v1", APIKey: "sk-judge-secret"},
		},
	}
	st := domain.StepSpec{Index: 0, Bucket: "aime26", Number: 30, Repeats: 4, Parallel: 8}
	args := s.evalArgs(spec, st, "/tmp/out")
	joined := strings.Join(args, " ")

	for _, want := range []string{
		"--datasets aime26",
		"--model_name glm-5.3-test",
		"--provider turbo",
		"--base_url https://api.example.com/v1 ",
		"--limit 30",
		"--repeats 4",
		"--eval_batch_size 8",
		"--max_tokens 8192",
		"--thinking false",
		"--judge_api_key sk-judge-secret",
		"--judge_model deepseek-v4-pro",
		"--output_dir /tmp/out",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv 缺少 %q\n实际: %s", want, joined)
		}
	}
	// 日志回显必须脱敏两个 key
	red := strings.Join(redactArgs(args), " ")
	if strings.Contains(red, "sk-target-secret") || strings.Contains(red, "sk-judge-secret") {
		t.Fatalf("日志回显泄露凭据: %s", red)
	}
}

// TestEvalProgressRe 只认 evalscope 的 Running[eval]: N%。
func TestEvalProgressRe(t *testing.T) {
	cases := []struct {
		line string
		pct  string
	}{
		{"Running[eval]:  42%|████     | 1/2 [00:10<00:12, 12.3s/it]", "42"},
		{"Running[eval]: 100%|██████████| 1/1 [00:20<00:00]", "100"},
	}
	for _, c := range cases {
		m := evalProgressRe.FindStringSubmatch(c.line)
		if m == nil || m[1] != c.pct {
			t.Errorf("%q → %v，期望 %s", c.line, m, c.pct)
		}
	}
	for _, noise := range []string{"Running[perf]: 0/1", "Processing[something]: 0/200", "Total / Success / Failed │ 2 / 0 / 2"} {
		if evalProgressRe.MatchString(noise) {
			t.Errorf("噪声行不应匹配: %q", noise)
		}
	}
}

// TestRedactSecrets 目标 key 与 judge key 都要抹掉。
func TestRedactSecrets(t *testing.T) {
	s := redactSecrets("a sk-1 b sk-2 c", "sk-1", "sk-2")
	if strings.Contains(s, "sk-1") || strings.Contains(s, "sk-2") {
		t.Fatalf("未脱敏: %s", s)
	}
}

// TestReadEvalSummaryNoSamples 上游全挂（ignore_errors 吞掉全部失败样本）时，
// success=true 但无有效样本 —— 应判为失败而不是 0 分成功。
func TestReadEvalSummaryNoSamples(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "eval_summary.json")
	if err := os.WriteFile(p, []byte(`{"dataset":"aime25","success":true,"retries_used":0,"elapsed_seconds":60.0}`), 0o644); err != nil {
		t.Fatal(err)
	}
	item := readEvalSummary(dir, domain.StepSpec{Bucket: "aime25"})
	if item.Success {
		t.Fatal("无有效样本应判失败")
	}
	if item.Error == "" {
		t.Fatal("应给出可读错误")
	}
}
