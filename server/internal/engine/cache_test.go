package engine

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/apexcloud/thvv/server/internal/domain"
)

// TestBenchArgs cache plan → lmcache argv 映射。
func TestBenchArgs(t *testing.T) {
	seed := 42
	e := &CacheEngine{}
	spec := Spec{
		Target: ResolvedTarget{Model: "glm-5.3-test", Protocol: domain.ProtocolOpenAI,
			APIURL: "https://api.example.com/v1", APIKey: "sk-secret"},
		Plan: domain.Plan{
			Workload: "multi-round-chat", KVCacheVolumeGB: 0.25, TokensPerGBKV: 65536,
			DurationSec: 30, QPS: 0.5, Seed: &seed,
			SharedPromptLen: 500, ChatHistoryLen: 4000, UserInputLen: 50, OutputLen: 128,
			NoWarmup: true, IgnoreEOS: true,
		},
	}
	args := e.benchArgs(spec, "/tmp/out")
	joined := args
	find := func(k string) string {
		for i, a := range joined {
			if a == k && i+1 < len(joined) {
				return joined[i+1]
			}
		}
		return ""
	}
	if find("--engine-url") != "https://api.example.com/v1" {
		t.Errorf("engine-url: %q", find("--engine-url"))
	}
	if find("--workload") != "multi-round-chat" {
		t.Errorf("workload: %q", find("--workload"))
	}
	if find("--mrc-duration") != "30" || find("--mrc-qps") != "0.5" {
		t.Errorf("duration/qps: %q %q", find("--mrc-duration"), find("--mrc-qps"))
	}
	if find("--kv-cache-volume") != "0.25" || find("--tokens-per-gb-kvcache") != "65536" {
		t.Errorf("volume/tpg: %q %q", find("--kv-cache-volume"), find("--tokens-per-gb-kvcache"))
	}
	if find("--seed") != "42" {
		t.Errorf("seed: %q", find("--seed"))
	}
	for _, want := range []string{"--no-warmup", "--ignore-eos", "--json", "--no-interactive"} {
		found := false
		for _, a := range joined {
			if a == want {
				found = true
			}
		}
		if !found {
			t.Errorf("缺少 %s", want)
		}
	}
	// 凭据不能进 argv
	for _, a := range joined {
		if a == "sk-secret" {
			t.Fatal("argv 泄露凭据")
		}
	}
}

// TestDeriveCacheRounds CSV 派生：首轮/稳态/逐轮。
func TestDeriveCacheRounds(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bench_results.csv")
	csv := `request_id,successful,ttft,request_latency,num_input_tokens,num_output_tokens,decode_speed,submit_time,first_token_time,finish_time,error
s0_r0,True,1.0,2.0,4000,128,60,1,2,3,
s1_r0,True,1.2,2.2,4000,128,60,1,2,3,
s0_r1,True,0.4,1.4,4200,128,60,4,5,6,
s1_r1,True,0.5,1.5,4200,128,60,4,5,6,
s0_r2,True,0.45,1.45,4400,128,60,7,8,9,
s1_r2,False,0,0,0,0,0,7,0,0,boom
`
	if err := os.WriteFile(p, []byte(csv), 0o644); err != nil {
		t.Fatal(err)
	}
	rounds, steady, first, err := DeriveCacheRounds(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(rounds) != 3 {
		t.Fatalf("轮次数期望 3，实际 %d", len(rounds))
	}
	if first.Requests != 2 || first.Round != 0 {
		t.Fatalf("首轮异常: %+v", first)
	}
	// 首轮均值 = (1000+1200)/2 = 1100ms
	if diff := first.TTFTMeanMS - 1100; diff > 0.01 || diff < -0.01 {
		t.Fatalf("首轮 TTFT 均值: %v", first.TTFTMeanMS)
	}
	// 稳态 = r1/r2 的成功请求（r2 的失败行被剔除）= 3 条
	if steady.Requests != 3 {
		t.Fatalf("稳态样本数期望 3，实际 %d", steady.Requests)
	}
	// 改善率 = 1 - steady_p50/first_mean；steady ttft = [400,500,450] → p50=450
	if steady.TTFTp50MS != 450 {
		t.Fatalf("稳态 P50: %v", steady.TTFTp50MS)
	}
	if diff := steady.TTFTImprovement - (1 - 450.0/1100.0); diff > 0.001 || diff < -0.001 {
		t.Fatalf("改善率: %v", steady.TTFTImprovement)
	}
	// r2 失败行不计入轮次请求数
	if rounds[2].Requests != 1 {
		t.Fatalf("r2 请求数期望 1（失败剔除），实际 %d", rounds[2].Requests)
	}
}

// TestReadBenchResultsMissing summary 缺失时报可读错误。
func TestReadBenchResultsMissing(t *testing.T) {
	dir := t.TempDir()
	if _, err := readBenchResults(dir); err == nil {
		t.Fatal("缺 bench_summary.json 应报错")
	}
}

// TestCacheBaseURL target 完整路径 → lmcache base（结尾 /v1，不会被二次追加）。
func TestCacheBaseURL(t *testing.T) {
	cases := map[string]string{
		"https://api.x.com/v1/chat/completions":  "https://api.x.com/v1",
		"https://api.x.com/v1/chat/completions/": "https://api.x.com/v1",
		"https://api.x.com/v1/messages":          "https://api.x.com", // lmcache 会补 /v1
		"https://api.x.com/v1":                   "https://api.x.com/v1",
	}
	for in, want := range cases {
		if got := cacheBaseURL(in); got != want {
			t.Errorf("cacheBaseURL(%q)=%q，期望 %q", in, got, want)
		}
	}
}
