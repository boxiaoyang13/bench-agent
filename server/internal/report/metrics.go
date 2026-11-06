// Package report 从 benchmark_data.db 计算指标、做验收判定并生成交付物。
//
// 计算口径必须与 perf/scripts/gen_report_from_db.py 一致（尤其是：
//   - ITL 与 TPOT 是两个不同口径，不可混用
//   - 分位数使用线性插值，与 Python 端 percentile() 相同
//   - 分档用的 avg_prompt_tokens 与 sla_eval.py 一致（上界不含）
//
// ）
package report

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"time"

	"github.com/apexcloud/thvv/server/internal/domain"
)

// Percentile 与 python percentile() 完全一致：k=(n-1)*p/100，线性插值。
func Percentile(data []float64, p float64) (float64, bool) {
	if len(data) == 0 {
		return 0, false
	}
	s := make([]float64, len(data))
	copy(s, data)
	sort.Float64s(s)
	k := float64(len(s)-1) * p / 100
	f := math.Floor(k)
	c := math.Ceil(k)
	if f == c {
		return s[int(f)], true
	}
	return s[int(f)] + (s[int(c)]-s[int(f)])*(k-f), true
}

func avg(v []float64) (float64, bool) {
	if len(v) == 0 {
		return 0, false
	}
	var sum float64
	for _, x := range v {
		sum += x
	}
	return sum / float64(len(v)), true
}

func minmax(v []float64) (float64, float64) {
	if len(v) == 0 {
		return 0, 0
	}
	mn, mx := v[0], v[0]
	for _, x := range v {
		mn = math.Min(mn, x)
		mx = math.Max(mx, x)
	}
	return mn, mx
}

// secScalar 以秒为单位算 Avg/P50/P90/P95/P99，同时填好毫秒镜像字段。
// 与 gen_report_from_db.py 的 calc_stats 一一对应。
func secScalar(v []float64, digits int) domain.Scalar {
	out := domain.Scalar{}
	if a, ok := avg(v); ok {
		out.AvgSec = round(a, digits)
		out.AvgMs = round(a*1000, 9)
	}
	for _, p := range []struct {
		p  float64
		sp *float64
		mp *float64
	}{
		{50, &out.P50Sec, &out.P50Ms},
		{90, &out.P90Sec, &out.P90Ms},
		{95, &out.P95Sec, &out.P95Ms},
		{99, &out.P99Sec, &out.P99Ms},
	} {
		if x, ok := Percentile(v, p.p); ok {
			*p.sp = round(x, digits)
			*p.mp = round(x*1000, 9)
		}
	}
	return out
}

func round(v float64, digits int) float64 {
	if math.IsInf(v, 0) || math.IsNaN(v) {
		return 0
	}
	p := math.Pow(10, float64(digits))
	return math.Round(v*p) / p
}

// LoadStepMetrics 读取一个 benchmark_data.db 并算出该 step 的指标。
func LoadStepMetrics(dbPath string, step domain.Step) (domain.StepMetrics, []domain.FailureItem, error) {
	m := domain.StepMetrics{
		StepIndex: step.Index,
		Bucket:    step.Bucket,
		Parallel:  step.Parallel,
		Number:    step.Number,
		Failures:  map[string]int{},
	}

	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro")
	if err != nil {
		return m, nil, err
	}
	defer db.Close()

	rows, err := db.Query(`SELECT * FROM result`)
	if err != nil {
		return m, nil, fmt.Errorf("读取 result 表失败: %w", err)
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return m, nil, err
	}
	idx := map[string]int{}
	for i, c := range cols {
		idx[c] = i
	}

	get := func(vals []any, name string) (any, bool) {
		i, ok := idx[name]
		if !ok || i >= len(vals) {
			return nil, false
		}
		return vals[i], true
	}
	getF := func(vals []any, name string) (float64, bool) {
		v, ok := get(vals, name)
		if !ok || v == nil {
			return 0, false
		}
		switch x := v.(type) {
		case float64:
			return x, true
		case int64:
			return float64(x), true
		case []byte:
			f, err := strconv.ParseFloat(string(x), 64)
			return f, err == nil
		case string:
			f, err := strconv.ParseFloat(x, 64)
			return f, err == nil
		case time.Time:
			return float64(x.UnixNano()) / 1e9, true
		}
		return 0, false
	}
	getStr := func(vals []any, name string) string {
		v, ok := get(vals, name)
		if !ok || v == nil {
			return ""
		}
		switch x := v.(type) {
		case string:
			return x
		case []byte:
			return string(x)
		}
		return fmt.Sprint(v)
	}

	var (
		ttfts, ttlts, tpots         []float64
		itls                        []float64
		rates                       []float64
		promptTokens, completionTks []float64
		starts, ends                []float64
		reqIndex                    int
	)

	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return m, nil, err
		}
		reqIndex++
		m.Requests.Total++

		success := false
		if v, ok := getF(vals, "success"); ok {
			success = v == 1
		}
		if success {
			m.Requests.Success++
			if v, ok := getF(vals, "first_chunk_latency"); ok {
				ttfts = append(ttfts, v)
			}
			lat, hasLat := getF(vals, "latency")
			if hasLat {
				ttlts = append(ttlts, lat)
			}
			if v, ok := getF(vals, "time_per_output_token"); ok {
				tpots = append(tpots, v)
			}
			if s := getStr(vals, "inter_token_latencies"); s != "" {
				var arr []float64
				if err := json.Unmarshal([]byte(s), &arr); err == nil {
					itls = append(itls, arr...)
				}
			}
			ct, _ := getF(vals, "completion_tokens")
			pt, _ := getF(vals, "prompt_tokens")
			completionTks = append(completionTks, ct)
			promptTokens = append(promptTokens, pt)
			if tt, ok := getF(vals, "first_chunk_latency"); ok && hasLat {
				if genT := lat - tt; ct >= 5 && genT >= 0.1 {
					rates = append(rates, ct/genT)
				}
			}
			if v, ok := getF(vals, "start_time"); ok {
				starts = append(starts, v)
			}
			if v, ok := getF(vals, "completed_time"); ok {
				ends = append(ends, v)
			}
		} else {
			m.Requests.Failed++
			reason := classifyFailure(getStr(vals, "error"), getF, vals)
			m.Failures[reason]++
			m.Failures[reason] += 0
		}
	}
	if err := rows.Err(); err != nil {
		return m, nil, err
	}

	if m.Requests.Total > 0 {
		m.Requests.SuccessRate = round(float64(m.Requests.Success)/float64(m.Requests.Total), 6)
	}
	if v, ok := avg(ttfts); ok {
		mn, mx := minmax(ttfts)
		m.TTFT = domain.TTFTStats{
			AvgSec: round(v, 6), MinSec: round(mn, 6), MaxSec: round(mx, 6),
			P50Sec: pct(ttfts, 50), P90Sec: pct(ttfts, 90), P95Sec: pct(ttfts, 95), P99Sec: pct(ttfts, 99),
		}
	}
	if v, ok := avg(rates); ok {
		m.TokenRateAvg = round(v, 6)
		m.TokenRate = secScalar(rates, 6)
	}
	if v, ok := avg(ttlts); ok {
		m.TTLT = secScalar(ttlts, 6)
		_ = v
	}
	if v, ok := avg(tpots); ok {
		// TPOT / ITL 是「秒」级的细小值，统一保留 9 位小数（纳秒）避免分位数被抹平。
		m.TPOT = secScalar(tpots, 12)
		_ = v
	}
	if v, ok := avg(itls); ok {
		m.ITL = secScalar(itls, 12)
		_ = v
	}
	var promptSum, completionSum float64
	for _, x := range promptTokens {
		promptSum += x
	}
	for _, x := range completionTks {
		completionSum += x
	}
	m.Tokens = domain.TokenSums{PromptSum: int(promptSum), CompletionSum: int(completionSum), TotalSum: int(promptSum + completionSum)}

	if len(starts) > 0 && len(ends) > 0 {
		_, maxEnd := minmax(ends)
		minStart, _ := minmax(starts)
		span := maxEnd - minStart
		m.DurationSec = round(span, 2)
		if span > 0 {
			outTPS := completionSum / span
			inTPS := promptSum / span
			m.Throughput = domain.Throughput{
				InputTPS: round(inTPS, 6), OutputTPS: round(outTPS, 6),
				InputTPM: round(inTPS*60, 6), OutputTPM: round(outTPS*60, 6),
				TPM: round((inTPS+outTPS)*60, 6),
				QPM: round(float64(m.Requests.Success)/span*60, 6),
			}
		}
	}
	return m, nil, nil
}

func pct(v []float64, p float64) float64 {
	if x, ok := Percentile(v, p); ok {
		return round(x, 6)
	}
	return 0
}

// classifyFailure 把失败原因归类（对齐 gen_perf_dashboard.failure_reasons 的思路）。
func classifyFailure(errStr string, getF func([]any, string) (float64, bool), vals []any) string {
	// 错误文本优先于泛化的 5xx：被上游超时导致的 500 应该读作 timeout 而不是 server_error
	e := errStr
	switch {
	case e != "" && containsAny(e, "timeout", "timed out", "deadline"):
		return "timeout"
	case e != "" && containsAny(e, "content", "sensitive", "filter", "safety"):
		return "content_filter"
	case e != "" && containsAny(e, "connection", "refused", "reset", "EOF"):
		return "connection"
	case e != "" && containsAny(e, "429", "rate limit", "too many"):
		return "http_429"
	}
	if code, ok := getF(vals, "http_status"); ok && code > 0 {
		switch int(code) {
		case 429:
			return "http_429"
		case 401, 403:
			return "auth_error"
		case 400, 422:
			return "bad_request"
		case 500, 502, 503, 504:
			return "server_error"
		}
	}
	if e == "" {
		return "unknown"
	}
	return "other"
}

func containsAny(s string, subs ...string) bool {
	l := toLower(s)
	for _, sub := range subs {
		if len(sub) > 0 && indexOf(l, toLower(sub)) >= 0 {
			return true
		}
	}
	return false
}

func toLower(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}
