package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/apexcloud/thvv/server/internal/domain"
)

// TestPerfArgsLongBucketTokenizer 512k/1m 必须用数据集构建分词器（对齐 run.sh DATASET_TOKENIZER），
// 其余档位用被测 target 的 tokenizer。
func TestPerfArgsLongBucketTokenizer(t *testing.T) {
	s := &Subprocess{}
	spec := Spec{
		Target: ResolvedTarget{
			Model: "glm-5.3-test", Protocol: domain.ProtocolOpenAI,
			APIURL: "https://example.com/v1/chat/completions", APIKey: "sk-x",
			Tokenizer: "zai-org/GLM-5.3",
		},
		LongBuckets:      map[string]bool{"512k": true, "1m": true},
		DatasetTokenizer: "zai-org/GLM-4.6",
	}
	argValue := func(args []string, name string) string {
		for i, a := range args {
			if a == name && i+1 < len(args) {
				return args[i+1]
			}
		}
		return ""
	}

	short := s.perfArgs(spec, domain.StepSpec{Bucket: "1k", Parallel: 1, Number: 1}, "/tmp/out")
	if got := argValue(short, "--tokenizer-path"); got != "zai-org/GLM-5.3" {
		t.Fatalf("1k 档 tokenizer 期望 target tokenizer，实际 %q", got)
	}

	long := s.perfArgs(spec, domain.StepSpec{Bucket: "512k", Parallel: 1, Number: 1}, "/tmp/out")
	if got := argValue(long, "--tokenizer-path"); got != "zai-org/GLM-4.6" {
		t.Fatalf("512k 档 tokenizer 期望数据集 tokenizer，实际 %q", got)
	}
}

// TestRedact 凭据不得出现在日志/产物里（run.log 与 run 目录可被下载）。
func TestRedact(t *testing.T) {
	const key = "sk-secret-123"
	args := []string{"perf", "--api-key", key, "--model", "m"}
	got := redactArgs(args)
	if got[2] != "***" {
		t.Fatalf("--api-key 未脱敏: %v", got)
	}
	if got[0] != "perf" || got[3] != "--model" || got[4] != "m" {
		t.Fatalf("脱敏破坏了其它参数: %v", got)
	}
	if s := redactSecret(`{"Authorization": "Bearer `+key+`"}`, key); strings.Contains(s, key) {
		t.Fatalf("行内凭据未脱敏: %s", s)
	}
}

func TestSanitizeRunDir(t *testing.T) {
	const key = "sk-secret-123"
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "m", "parallel_1_number_1"), 0o755); err != nil {
		t.Fatal(err)
	}
	argsFile := filepath.Join(dir, "m", "parallel_1_number_1", "benchmark_args.json")
	if err := os.WriteFile(argsFile, []byte(`{"api_key":"`+key+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	logFile := filepath.Join(dir, "m", "benchmark.log")
	if err := os.WriteFile(logFile, []byte(`"api_key": "`+key+`"`), 0o644); err != nil {
		t.Fatal(err)
	}
	dbFile := filepath.Join(dir, "m", "parallel_1_number_1", "benchmark_data.db")
	if err := os.WriteFile(dbFile, []byte("sqlite-binary-"+key), 0o644); err != nil {
		t.Fatal(err)
	}

	sanitizeRunDir(dir, key)

	for _, p := range []string{argsFile, logFile} {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), key) {
			t.Fatalf("%s 仍含明文凭据", p)
		}
	}
	// 二进制 DB 不能被文本替换破坏
	if b, _ := os.ReadFile(dbFile); !strings.Contains(string(b), key) {
		t.Fatal("DB 不应被文本脱敏改写")
	}
}

// TestProgressRe 只认 evalscope 的逐 step 进度行，其它含 a/b 的数字不能误报。
func TestProgressRe(t *testing.T) {
	cases := []struct {
		line      string
		done, all int
		ok        bool
	}{
		{"Processing[parallel_64_number_200]:  12%|██        | 24/200 [00:35<04:10,  1.4s/it]", 24, 200, true},
		{"Processing[parallel_1_number_20]:   0%|          | 0/20 [00:00<?, ?it/s]", 0, 20, true},
		{"Running[perf]:   0%|          | 0/1 [00:49<?, ?it/s]", 0, 0, false},
		{"Loading[conversations]   0%| 164/? [Elapsed: 00:30 < Remaining: 00:00]", 0, 0, false},
		{"│ TTFT (ms)      │ 421.76 │ 429.38 │ 430.54 │", 0, 0, false},
		{"Total / Success / Failed  │ 2 / 0 / 2", 0, 0, false},
	}
	for _, c := range cases {
		m := progressRe.FindStringSubmatch(c.line)
		if !c.ok {
			if m != nil {
				t.Errorf("不应匹配 %q，却得到 %v", c.line, m)
			}
			continue
		}
		if m == nil {
			t.Fatalf("应匹配 %q", c.line)
		}
		done, all := atoiForTest(m[1]), atoiForTest(m[2])
		if done != c.done || all != c.all {
			t.Errorf("%q 解析为 %d/%d，期望 %d/%d", c.line, done, all, c.done, c.all)
		}
	}
}

func atoiForTest(s string) int {
	n := 0
	for _, r := range s {
		n = n*10 + int(r-'0')
	}
	return n
}
