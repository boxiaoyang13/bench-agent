package report

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/apexcloud/thvv/server/internal/config"
	"github.com/apexcloud/thvv/server/internal/domain"

	"github.com/xuri/excelize/v2"

	_ "modernc.org/sqlite"
)

// makeDB 造一个与 evalscope 同结构的 benchmark_data.db（字段取自 gen_report_from_db.py）。
func makeDB(t *testing.T, rows []map[string]any) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "benchmark_data.db")
	db, err := sql.Open("sqlite", "file:"+p)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(`CREATE TABLE result (
		success INTEGER, first_chunk_latency REAL, latency REAL, time_per_output_token REAL,
		inter_token_latencies TEXT, prompt_tokens INTEGER, completion_tokens INTEGER,
		start_time REAL, completed_time REAL, error TEXT, http_status INTEGER,
		request TEXT, response TEXT)`)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		itl, _ := json.Marshal(r["itl"])
		_, err := db.Exec(`INSERT INTO result VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			r["success"], r["ttft"], r["latency"], r["tpot"], string(itl),
			r["prompt"], r["completion"], r["start"], r["end"],
			r["error"], r["status"], r["request"], r["response"])
		if err != nil {
			t.Fatal(err)
		}
	}
	return p
}

func okRow(ttft, latency, tpot float64, prompt, completion int, start float64, itl []float64) map[string]any {
	return map[string]any{
		"success": 1, "ttft": ttft, "latency": latency, "tpot": tpot,
		"itl": itl, "prompt": prompt, "completion": completion,
		"start": start, "end": start + latency, "error": "", "status": 200,
		"request": "req", "response": "resp",
	}
}

func failRow(status int, errMsg string, start float64) map[string]any {
	var itl []float64
	return map[string]any{
		"success": 0, "ttft": nil, "latency": nil, "tpot": nil, "itl": itl,
		"prompt": 0, "completion": 0, "start": start, "end": start + 1,
		"error": errMsg, "status": status, "request": "req-body", "response": "resp-body",
	}
}

// TestPercentile 必须与 python percentile() 一致（线性插值）。
func TestPercentile(t *testing.T) {
	data := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	cases := map[float64]float64{
		0: 1, 50: 5.5, 90: 9.1, 100: 10,
	}
	for p, want := range cases {
		got, ok := Percentile(data, p)
		if !ok {
			t.Fatalf("p=%v 无结果", p)
		}
		if diff(got, want) > 1e-9 {
			t.Errorf("p=%v 期望 %v，实际 %v", p, want, got)
		}
	}
	if _, ok := Percentile(nil, 50); ok {
		t.Fatal("空数据应返回 false")
	}
}

func diff(a, b float64) float64 {
	if a > b {
		return a - b
	}
	return b - a
}

// TestLoadStepMetrics 验证指标口径。
func TestLoadStepMetrics(t *testing.T) {
	rows := []map[string]any{
		okRow(0.5, 2.5, 0.02, 1000, 100, 1000, []float64{0.01, 0.02, 0.03}),
		okRow(1.5, 3.5, 0.03, 1000, 100, 1002, []float64{0.02, 0.03, 0.04}),
		okRow(2.5, 4.5, 0.04, 1000, 100, 1004, []float64{0.03, 0.04, 0.05}),
		failRow(429, "rate limit exceeded", 1006),
	}
	p := makeDB(t, rows)
	m, _, err := LoadStepMetrics(p, domain.Step{Index: 0, Bucket: "1k", Parallel: 8, Number: 4})
	if err != nil {
		t.Fatal(err)
	}

	if m.Requests.Total != 4 || m.Requests.Success != 3 || m.Requests.Failed != 1 {
		t.Fatalf("请求计数错误: %+v", m.Requests)
	}
	if m.Requests.SuccessRate != 0.75 {
		t.Fatalf("成功率错误: %v", m.Requests.SuccessRate)
	}
	// TTFT: [0.5,1.5,2.5] → avg 1.5, p50 1.5, p90 2.3（线性插值）
	if diff(m.TTFT.AvgSec, 1.5) > 1e-6 {
		t.Fatalf("avg ttft 错误: %v", m.TTFT.AvgSec)
	}
	if diff(m.TTFT.P50Sec, 1.5) > 1e-6 {
		t.Fatalf("p50 ttft 错误: %v", m.TTFT.P50Sec)
	}
	if diff(m.TTFT.P90Sec, 2.3) > 1e-6 {
		t.Fatalf("p90 ttft 期望 2.3，实际 %v", m.TTFT.P90Sec)
	}
	// TPOT 单位毫秒（time_per_output_token=0.02/0.03/0.04s → 30ms）
	if diff(m.TPOT.AvgMs, 30) > 1e-6 {
		t.Fatalf("tpot 错误: %v", m.TPOT.AvgMs)
	}
	// ITL 按 chunk 汇总（9 个值 0.01..0.05），与 TPOT 不同口径
	if diff(m.ITL.AvgMs, 30) > 1e-6 {
		t.Fatalf("itl avg 错误: %v", m.ITL.AvgMs)
	}
	// ITL 分位数按毫秒口径：9 个值 [0.01..0.05]s → p50=0.03s=30ms，p90=0.042s=42ms
	if diff(m.ITL.P50Ms, 30) > 1e-6 {
		t.Fatalf("itl p50 期望 30ms，实际 %v", m.ITL.P50Ms)
	}
	if diff(m.ITL.P90Ms, 42) > 1e-6 {
		t.Fatalf("itl p90 期望 42ms，实际 %v", m.ITL.P90Ms)
	}
	if m.ITL.P50Ms == m.TPOT.AvgMs && m.ITL.P90Ms == m.TPOT.AvgMs {
		t.Fatal("ITL 与 TPOT 不应完全同源")
	}
	// tokens
	if m.Tokens.PromptSum != 3000 || m.Tokens.CompletionSum != 300 {
		t.Fatalf("token 汇总错误: %+v", m.Tokens)
	}
	// 吞吐：span = max(end)-min(start) = 1004+4.5-1000 = 8.5s
	// output_tps = 300/8.5 = 35.29
	if diff(m.Throughput.OutputTPS, 35.29) > 0.05 {
		t.Fatalf("output tps 期望 ~35.29，实际 %v", m.Throughput.OutputTPS)
	}
	// TPM = (input_tps + output_tps) * 60
	if m.Throughput.TPM <= 0 {
		t.Fatalf("tpm 未计算: %+v", m.Throughput)
	}
	// 失败分类
	if m.Failures["http_429"] != 1 {
		t.Fatalf("失败分类错误: %+v", m.Failures)
	}
}

// TestFailures 失败明细提取。
func TestFailures(t *testing.T) {
	p := makeDB(t, []map[string]any{
		okRow(0.5, 2.5, 0.02, 1000, 100, 1000, []float64{0.01}),
		failRow(429, "rate limit", 1001),
		failRow(500, "upstream timeout", 1002),
	})
	items, err := LoadFailures(p, domain.Step{Index: 2, Bucket: "64k", Parallel: 32}, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("失败条数期望 2，实际 %d", len(items))
	}
	if items[0].Reason != "http_429" || items[1].Reason != "timeout" {
		t.Fatalf("失败原因分类错误: %q %q", items[0].Reason, items[1].Reason)
	}
	if items[0].Bucket != "64k" || items[0].Parallel != 32 {
		t.Fatalf("step 归属错误: %+v", items[0])
	}
	if items[0].HTTPStatus == nil || *items[0].HTTPStatus != 429 {
		t.Fatalf("HTTP 状态错误: %+v", items[0].HTTPStatus)
	}
}

// TestEvaluateGate 验收判定：分档、成功率、吞吐下限。
func TestEvaluateGate(t *testing.T) {
	profile := &config.GateProfile{
		Capability: "perf",
		Tiers: []config.GateTier{
			{Tier: "<4K", Lo: 1000, Hi: 4000, TTFTp50Max: 2.0, TTFTp90Max: 5.0},
			{Tier: "<128K", Lo: 64000, Hi: 128000, TTFTp50Max: 15.0, TTFTp90Max: 35.0},
		},
		Global: config.GateGlobal{MinOutputTokensPerSec: 30, MinSuccessRate: 0.99},
	}

	pass := domain.StepMetrics{
		Bucket: "1k", Parallel: 8,
		Requests:     domain.RequestCounts{Total: 10, Success: 10, Failed: 0, SuccessRate: 1},
		TTFT:         domain.TTFTStats{P50Sec: 1.0, P90Sec: 2.0},
		Tokens:       domain.TokenSums{PromptSum: 10000}, // avg 1000 → <4K
		TokenRateAvg: 50,
	}
	slow := domain.StepMetrics{
		Bucket: "64k", Parallel: 8,
		Requests:     domain.RequestCounts{Total: 10, Success: 10, Failed: 0, SuccessRate: 1},
		TTFT:         domain.TTFTStats{P50Sec: 20.0, P90Sec: 40.0}, // 超阈值
		Tokens:       domain.TokenSums{PromptSum: 70000 * 10},      // avg 70000 → <128K
		TokenRateAvg: 20,                                           // 低于 30
	}
	lowSR := domain.StepMetrics{
		Bucket: "1k", Parallel: 64,
		Requests:     domain.RequestCounts{Total: 10, Success: 9, Failed: 1, SuccessRate: 0.9},
		TTFT:         domain.TTFTStats{P50Sec: 99, P90Sec: 99}, // 不应参与档位统计
		Tokens:       domain.TokenSums{PromptSum: 10000},
		TokenRateAvg: 50,
	}

	res := EvaluateGate(profile, []domain.StepMetrics{pass, slow, lowSR})
	if res.Passed {
		t.Fatal("应判定不通过")
	}
	rules := map[string]int{}
	for _, v := range res.Violations {
		rules[v.Rule]++
	}
	if rules["ttft_p50_max"] != 1 || rules["ttft_p90_max"] != 1 {
		t.Fatalf("TTFT 阈值违规未识别: %+v", res.Violations)
	}
	if rules["min_output_tokens_per_sec"] != 1 {
		t.Fatalf("吞吐下限违规未识别: %+v", res.Violations)
	}
	if rules["min_success_rate"] != 1 {
		t.Fatalf("成功率违规未识别: %+v", res.Violations)
	}
	// 低成功率的 step 不应污染 <4K 档的 P50
	for _, ts := range res.TierSummary {
		if ts.Tier == "<4K" && ts.P50MaxSec > 1.0001 {
			t.Fatalf("<4K 档被低成功率 step 污染: %+v", ts)
		}
	}
}

// TestRenderArtifacts 报告渲染不 panic 且包含关键内容。
func TestRenderArtifacts(t *testing.T) {
	steps := []domain.Step{
		{Index: 0, Bucket: "1k", Parallel: 8, Number: 60, Status: domain.StepSucceeded},
	}
	metrics := []domain.StepMetrics{{
		StepIndex: 0, Bucket: "1k", Parallel: 8, Number: 60,
		Requests:     domain.RequestCounts{Total: 60, Success: 60, SuccessRate: 1},
		TTFT:         domain.TTFTStats{AvgSec: 0.3, P50Sec: 0.28, P90Sec: 0.4},
		TPOT:         domain.Scalar{AvgMs: 20},
		ITL:          domain.Scalar{AvgMs: 19, P50Ms: 18},
		Throughput:   domain.Throughput{OutputTPS: 40, TPM: 20000},
		Tokens:       domain.TokenSums{PromptSum: 60000, CompletionSum: 6000},
		TokenRateAvg: 40,
		DurationSec:  30,
	}}
	gate := EvaluateGate(&config.GateProfile{
		Tiers:  []config.GateTier{{Tier: "<4K", Lo: 1000, Hi: 4000, TTFTp50Max: 2, TTFTp90Max: 5}},
		Global: config.GateGlobal{MinOutputTokensPerSec: 30, MinSuccessRate: 0.99},
	}, metrics)
	summary := BuildSummary(steps, metrics, gate)

	html, err := RenderHTML(HTMLInput{
		Title: "测试报告", Model: "glm-5.2", Vendor: "zhipu", Capability: "perf",
		GeneratedAt: time.Now(), PlanMode: "单档 1k", Summary: summary, Gate: gate,
		Steps: steps, Metrics: metrics,
		Failures: []domain.FailureItem{{Bucket: "1k", Parallel: 8, Reason: "http_429", Error: "rate limited"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"测试报告", "验收结论", "执行步骤与指标", "口径说明", "http_429"} {
		if !containsBytes(html, want) {
			t.Errorf("HTML 缺少内容 %q", want)
		}
	}
	if len(html) < 2000 {
		t.Fatalf("HTML 过短: %d 字节", len(html))
	}

	xlsx, err := RenderXLSX(XLSXInput{
		Model: "glm-5.2", GeneratedAt: time.Now(), Gate: gate, Metrics: metrics,
		Failures: []domain.FailureItem{{Bucket: "1k", Parallel: 8, Reason: "http_429"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(xlsx) < 3000 {
		t.Fatalf("xlsx 过短: %d 字节", len(xlsx))
	}
	// xlsx 是 zip：文件头应为 PK
	if len(xlsx) < 2 || xlsx[0] != 'P' || xlsx[1] != 'K' {
		t.Fatal("xlsx 不是合法 zip")
	}

	csvData, err := RenderFailureCSV([]domain.FailureItem{{Bucket: "1k", Reason: "http_429"}})
	if err != nil {
		t.Fatal(err)
	}
	if !containsBytes(csvData, "失败原因") {
		t.Fatal("CSV 表头缺失")
	}
}

func containsBytes(b []byte, s string) bool {
	return indexOfBytes(string(b), s) >= 0
}

func indexOfBytes(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}

var _ = os.Getenv

// TestLoadFailuresLegacySchema evalscope 1.9.x 的 result 表没有 error/http_status 列，
// 失败行也必须能被导出（给出兜底说明），否则失败明细会整体丢失。
func TestLoadFailuresLegacySchema(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "benchmark_data.db")
	db, err := sql.Open("sqlite", "file:"+p)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE result (
		request TEXT, start_time REAL, inter_token_latencies TEXT, success INTEGER,
		response_messages TEXT, completed_time REAL, latency REAL, first_chunk_latency REAL,
		prompt_tokens INTEGER, completion_tokens INTEGER, max_gpu_memory_cost REAL,
		time_per_output_token REAL)`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO result VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		`{"model":"m"}`, 1000.0, "[]", 0, "", 1001.0, nil, nil, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	db.Close()

	items, err := LoadFailures(p, domain.Step{Index: 0, Bucket: "1k", Parallel: 1}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("失败条数期望 1，实际 %d", len(items))
	}
	if items[0].Error == "" {
		t.Fatal("失败项应有兜底说明，不能为空")
	}
	if items[0].RequestBodyExcerpt == "" {
		t.Fatal("失败项应带上请求摘要")
	}
}

// TestXLSXReportShape 「性能指标」45 列与 thvv 平台版逐字一致；失败详情 14 列。
func TestXLSXReportShape(t *testing.T) {
	m := domain.StepMetrics{
		Bucket: "1k", Parallel: 1, Number: 2,
		Requests:     domain.RequestCounts{Total: 2, Success: 2, SuccessRate: 1},
		TTFT:         domain.TTFTStats{AvgSec: 0.6, MinSec: 0.4, MaxSec: 0.8, P50Sec: 0.6, P90Sec: 0.75, P95Sec: 0.78, P99Sec: 0.79},
		TTLT:         domain.Scalar{AvgSec: 2.5, P50Sec: 2.4, P90Sec: 3.0, P95Sec: 3.1, P99Sec: 3.2},
		TPOT:         domain.Scalar{AvgSec: 0.007, P50Sec: 0.007, P90Sec: 0.008, P95Sec: 0.008, P99Sec: 0.009},
		ITL:          domain.Scalar{AvgSec: 0.02, P50Sec: 0.0000087, P90Sec: 0.04, P95Sec: 0.05, P99Sec: 0.06},
		TokenRateAvg: 50, TokenRate: domain.Scalar{AvgSec: 50, P50Sec: 49, P90Sec: 60, P95Sec: 61, P99Sec: 62},
		Tokens:     domain.TokenSums{PromptSum: 2400, CompletionSum: 600, TotalSum: 3000},
		Throughput: domain.Throughput{OutputTPS: 40, OutputTPM: 2400, InputTPM: 9000, TPM: 11400},
		RawDB:      "/data/runs/run_x/results/perf-1k-20260101-0101/m/parallel_1_number_2/benchmark_data.db",
	}
	ttft, ttlt := 0.5, 1.2
	data, err := RenderXLSX(XLSXInput{
		Model: "m", Vendor: "turbo", Company: "本地联调", GeneratedAt: time.Now(),
		APIURL:  "https://api.example.com/v1/chat/completions?key=secret",
		Metrics: []domain.StepMetrics{m},
		Failures: []domain.FailureItem{{
			Bucket: "1k", Parallel: 1, RequestIndex: 1, Reason: "other", Detail: "boom",
			TTFTSec: &ttft, TTLTSec: &ttlt, RequestBodyExcerpt: `{"messages":[]}`,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	f, err := excelize.OpenReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	rows, _ := f.GetRows("性能指标")
	if len(rows) != 2 || len(rows[0]) != 45 {
		t.Fatalf("性能指标应为 2 行 x 45 列，实际 %d x %d", len(rows), len(rows[0]))
	}
	wantHeaders := []string{"客户端", "后端网路链接", "数据集", "并发数", "总请求数", "成功数", "失败数", "成功率",
		"Avg TTFT(s)", "Min TTFT(s)", "Max TTFT(s)", "P50 TTFT(s)", "P90 TTFT(s)", "P95 TTFT(s)", "P99 TTFT(s)",
		"Avg TTLT(s)", "P50 TTLT(s)", "P90 TTLT(s)", "P95 TTLT(s)", "P99 TTLT(s)",
		"Avg Rate(t/s)", "P50 Rate(t/s)", "P90 Rate(t/s)", "P95 Rate(t/s)", "P99 Rate(t/s)",
		"Avg ITL(s)", "P50 ITL(s)", "P90 ITL(s)", "P95 ITL(s)", "P99 ITL(s)",
		"Avg TPOT(s)", "P50 TPOT(s)", "P90 TPOT(s)", "P95 TPOT(s)", "P99 TPOT(s)",
		"Avg Prompt Tokens", "Avg Completion Tokens", "Total Tokens", "Avg Cached Tokens", "Avg Cache Hit Ratio",
		"Avg Total Time(ms)", "Output TPM", "Output TPS", "Input TPM", "TPM"}
	for i, h := range wantHeaders {
		if rows[0][i] != h {
			t.Fatalf("第 %d 列表头期望 %q，实际 %q", i+1, h, rows[0][i])
		}
	}
	idx := map[string]int{}
	for i, h := range rows[0] {
		idx[h] = i
	}
	if rows[1][idx["数据集"]] != "1k (perf-1k-20260101-0101)" {
		t.Errorf("数据集列异常: %q", rows[1][idx["数据集"]])
	}
	if rows[1][idx["客户端"]] != "本地联调" || rows[1][idx["后端网路链接"]] != "turbo" {
		t.Errorf("客户端/后端列异常: %v %v", rows[1][idx["客户端"]], rows[1][idx["后端网路链接"]])
	}

	fail, _ := f.GetRows("失败请求详情")
	if len(fail) != 2 || len(fail[0]) != 14 {
		t.Fatalf("失败请求详情应为 2 行 x 14 列，实际 %d x %d", len(fail), len(fail[0]))
	}
	fh := map[string]int{}
	for i, h := range fail[0] {
		fh[h] = i
	}
	if got := fail[1][fh["请求URL"]]; got != "https://api.example.com/v1/chat/completions" {
		t.Errorf("请求URL 未去掉 query: %q", got)
	}
	if fail[1][fh["请求方法"]] != "POST" || fail[1][fh["详细说明"]] != "boom" {
		t.Errorf("失败明细字段异常: %v", fail[1])
	}
}

// TestEvaluateEvalGate 分数验收：低于下限/失败数据集/未配阈值/baseline_by 覆盖。
func TestEvaluateEvalGate(t *testing.T) {
	profile := &config.GateProfile{
		Capability: "eval",
		EvalTiers: []config.EvalGateTier{
			{Dataset: "aime26", MinScore: 0.89},
			{Dataset: "hle", MinScore: 0.36, BaselineBy: map[string]float64{"glm-5.3": 0.40, "kimi": 0.42}},
			{Dataset: "tau2_bench", Subitem: "retail", MinScore: 0.70},
		},
	}
	results := []domain.EvalDatasetResult{
		{Dataset: "aime26", Success: true, Score: 0.93},          // 达标
		{Dataset: "hle", Success: true, Score: 0.39},             // glm-5.3 → 0.40，未达标
		{Dataset: "gpqa_diamond", Success: true, Score: 0.10},    // 未配阈值 → 不判定
		{Dataset: "longbench_v2", Success: false, Error: "boom"}, // 失败
	}

	res := EvaluateEvalGate(profile, "GLM-5.3-test", results)
	if res.Passed {
		t.Fatal("应判定不通过")
	}
	rules := map[string]int{}
	for _, v := range res.Violations {
		rules[v.Rule+"/"+v.Bucket]++
	}
	if rules["min_score/hle"] != 1 {
		t.Fatalf("hle 分数违规未识别: %+v", res.Violations)
	}
	if rules["eval_success/longbench_v2"] != 1 {
		t.Fatalf("失败数据集未记违规: %+v", res.Violations)
	}
	if _, ok := rules["min_score/gpqa_diamond"]; ok {
		t.Fatal("未配阈值的数据集不应判定")
	}
	// aime26 达标：不应出现在违规里
	for _, v := range res.Violations {
		if v.Bucket == "aime26" {
			t.Fatal("aime26 达标却记了违规")
		}
	}

	// baseline_by：kimi 模型命中 0.42，0.43 应通过
	res2 := EvaluateEvalGate(profile, "kimi-k3", []domain.EvalDatasetResult{
		{Dataset: "hle", Success: true, Score: 0.43},
	})
	if !res2.Passed {
		t.Fatalf("kimi 基线 0.42，得分 0.43 应通过: %+v", res2.Violations)
	}
}

// TestEvaluateCacheGate 稳态 TTFT / 改善率 / 成功率 / no_warmup 口径。
func TestEvaluateCacheGate(t *testing.T) {
	profile := &config.GateProfile{
		Capability: "cache",
		CacheTiers: []config.CacheGateTier{
			{SteadyTTFTp50MaxMS: 800, TTFTImprovementMin: 0.30, SuccessRateMin: 0.99},
		},
	}
	good := &domain.CacheMetrics{
		TotalRequests: 20, SuccessReqs: 20, SuccessRate: 1,
		FirstRound:  &domain.CacheRoundStats{Round: 0, Requests: 3, TTFTMeanMS: 1000},
		SteadyState: &domain.CacheSteadyStats{Requests: 17, TTFTp50MS: 400, TTFTImprovement: 0.60},
	}
	if res := EvaluateCacheGate(profile, good, false); !res.Passed {
		t.Fatalf("应通过: %+v", res.Violations)
	}

	slow := &domain.CacheMetrics{
		TotalRequests: 20, SuccessReqs: 20, SuccessRate: 1,
		FirstRound:  &domain.CacheRoundStats{Round: 0, Requests: 3, TTFTMeanMS: 1000},
		SteadyState: &domain.CacheSteadyStats{Requests: 17, TTFTp50MS: 900, TTFTImprovement: 0.10},
	}
	res := EvaluateCacheGate(profile, slow, false)
	if res.Passed {
		t.Fatal("稳态超限且改善不足应不通过")
	}
	rules := map[string]bool{}
	for _, v := range res.Violations {
		rules[v.Rule] = true
	}
	if !rules["steady_ttft_p50_max_ms"] || !rules["ttft_improvement_min"] {
		t.Fatalf("违规规则缺失: %+v", rules)
	}

	// no_warmup：不判改善率，只判稳态上限
	cold := &domain.CacheMetrics{
		TotalRequests: 20, SuccessReqs: 20, SuccessRate: 1,
		FirstRound:  &domain.CacheRoundStats{Round: 0, Requests: 3, TTFTMeanMS: 1000},
		SteadyState: &domain.CacheSteadyStats{Requests: 17, TTFTp50MS: 500, TTFTImprovement: 0.10},
	}
	if res := EvaluateCacheGate(profile, cold, true); !res.Passed {
		t.Fatalf("no_warmup 不判改善率，应通过: %+v", res.Violations)
	}

	// 无请求
	empty := &domain.CacheMetrics{}
	if res := EvaluateCacheGate(profile, empty, false); res.Passed {
		t.Fatal("无请求应不通过")
	}
}
