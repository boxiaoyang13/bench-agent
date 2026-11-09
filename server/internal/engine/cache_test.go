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
	// 性能派生：decode_speed=60 t/s → 稳态 TPOT = 1000/60 ms
	if diff := steady.DecodeSpeedP50 - 60; diff > 0.01 || diff < -0.01 {
		t.Fatalf("稳态解码速度 P50 期望 60，实际 %v", steady.DecodeSpeedP50)
	}
	if diff := steady.TPOTp50MS - 1000.0/60; diff > 0.01 || diff < -0.01 {
		t.Fatalf("稳态 TPOT P50 期望 16.67ms，实际 %v", steady.TPOTp50MS)
	}
	// r2 失败行不计入轮次请求数
	if rounds[2].Requests != 1 {
		t.Fatalf("r2 请求数期望 1（失败剔除），实际 %d", rounds[2].Requests)
	}
}

// TestDeriveCacheRoundsTPOTFallback CSV 无 decode_speed 时用
// (request_latency - ttft) / (num_output_tokens - 1) 兜底。
func TestDeriveCacheRoundsTPOTFallback(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bench_results.csv")
	csv := `request_id,successful,ttft,request_latency,num_input_tokens,num_output_tokens,decode_speed,submit_time,first_token_time,finish_time,error
s0_r0,True,1.0,3.0,4000,201,0,1,2,3,
s0_r1,True,0.5,1.5,4200,101,0,4,5,6,
s0_r2,True,0.6,1.6,4400,101,0,7,8,9,
`
	if err := os.WriteFile(p, []byte(csv), 0o644); err != nil {
		t.Fatal(err)
	}
	_, steady, _, err := DeriveCacheRounds(p)
	if err != nil {
		t.Fatal(err)
	}
	// r1: (1.5-0.5)*1000/100 = 10ms；r2: (1.6-0.6)*1000/100 = 10ms
	if diff := steady.TPOTp50MS - 10; diff > 0.01 || diff < -0.01 {
		t.Fatalf("兜底 TPOT 期望 10ms，实际 %v", steady.TPOTp50MS)
	}
	if steady.DecodeSpeedP50 != 0 {
		t.Fatalf("无 decode_speed 列时不应伪造解码速度: %v", steady.DecodeSpeedP50)
	}
}

// TestReadBenchResultsPerf bench_summary.json 的性能字段映射 + 派生 TPOT。
func TestReadBenchResultsPerf(t *testing.T) {
	dir := t.TempDir()
	summary := `{
  "config": {"workload": "multi-round-chat", "model": "glm-5.3-test", "mrc_duration": 30.0},
  "results": {
    "total_requests": 4, "successful_requests": 4, "failed_requests": 0, "elapsed_time": 30.0,
    "mean_ttft_ms": 500, "p50_ttft_ms": 480, "p90_ttft_ms": 600, "p99_ttft_ms": 700,
    "mean_decode_speed": 50, "p50_decode_speed": 48, "p90_decode_speed": 60,
    "input_throughput": 2500, "output_throughput": 120,
    "p50_request_latency_ms": 1500, "p90_request_latency_ms": 2000, "p99_request_latency_ms": 2500,
    "total_input_tokens": 10000, "total_output_tokens": 800
  }
}`
	csv := `request_id,successful,ttft,request_latency,num_input_tokens,num_output_tokens,decode_speed,submit_time,first_token_time,finish_time,error
s0_r0,True,1.0,2.0,4000,128,50,1,2,3,
s1_r0,True,1.2,2.2,4000,128,50,1,2,3,
s0_r1,True,0.4,1.4,4200,128,50,4,5,6,
s1_r1,True,0.5,1.5,4200,128,50,4,5,6,
`
	if err := os.WriteFile(filepath.Join(dir, "bench_summary.json"), []byte(summary), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bench_results.csv"), []byte(csv), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := readBenchResults(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if m.MeanDecodeSpeed != 50 || m.OutputThroughput != 120 || m.InputThroughput != 2500 {
		t.Fatalf("summary 性能字段映射错误: %+v", m)
	}
	if diff := m.TPOTMeanMS - 20; diff > 0.01 || diff < -0.01 {
		t.Fatalf("TPOTMeanMS 期望 20，实际 %v", m.TPOTMeanMS)
	}
	if m.LatencyP50MS != 1500 || m.LatencyP90MS != 2000 || m.LatencyP99MS != 2500 {
		t.Fatalf("时延分位映射错误: %v/%v/%v", m.LatencyP50MS, m.LatencyP90MS, m.LatencyP99MS)
	}
	if m.NumSessions != 2 {
		t.Fatalf("session 数期望 2，实际 %d", m.NumSessions)
	}
	if m.SteadyState == nil || m.SteadyState.TPOTp50MS != 20 {
		t.Fatalf("稳态 TPOT 期望 20ms: %+v", m.SteadyState)
	}
	if m.SteadyState.DecodeSpeedP50 != 50 {
		t.Fatalf("稳态解码速度期望 50: %+v", m.SteadyState)
	}
}

// TestReadBenchResultsMissing summary 缺失时报可读错误。
func TestReadBenchResultsMissing(t *testing.T) {
	dir := t.TempDir()
	if _, err := readBenchResults(dir, nil); err == nil {
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

// TestDeriveProbeHitRate 探针命中率派生：稳态=非首轮；端点未上报的识别。
func TestDeriveProbeHitRate(t *testing.T) {
	rounds := []domain.CacheProbeRound{
		{Round: 0, PromptTokens: 12000, CachedTokens: 0},
		{Round: 1, PromptTokens: 12200, CachedTokens: 12000},
		{Round: 2, PromptTokens: 12400, CachedTokens: 12200},
	}
	hr, reported := DeriveProbeHitRate(rounds)
	// 稳态 = (12000+12200)/(12200+12400) = 24200/24600
	want := 24200.0 / 24600.0
	if hr < want-1e-9 || hr > want+1e-9 {
		t.Fatalf("命中率 %v，期望 %v", hr, want)
	}
	if !reported {
		t.Fatal("有 cached_tokens>0 应识别为已上报")
	}

	// 端点不返回 cached_tokens：reported=false，命中率 0
	noCache := []domain.CacheProbeRound{
		{Round: 0, PromptTokens: 100, CachedTokens: 0},
		{Round: 1, PromptTokens: 100, CachedTokens: 0},
	}
	hr2, reported2 := DeriveProbeHitRate(noCache)
	if hr2 != 0 || reported2 {
		t.Fatalf("未上报场景: hr=%v reported=%v", hr2, reported2)
	}

	// 单轮：无稳态
	if _, _ = DeriveProbeHitRate(rounds[:1]); true {
		hr3, _ := DeriveProbeHitRate(rounds[:1])
		if hr3 != 0 {
			t.Fatal("单轮无稳态，命中率应为 0")
		}
	}
}
