package report

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/apexcloud/thvv/server/internal/domain"
)

// LoadFailures 从 benchmark_data.db 读取失败请求明细（对齐 export_failure_details.py）。
func LoadFailures(dbPath string, step domain.Step, excerptChars int) ([]domain.FailureItem, error) {
	if excerptChars <= 0 {
		excerptChars = 1200
	}
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro")
	if err != nil {
		return nil, err
	}
	defer db.Close()

	rows, err := db.Query(`SELECT * FROM result`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	idx := map[string]int{}
	for i, c := range cols {
		idx[c] = i
	}

	var out []domain.FailureItem
	no := 0
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		no++
		if asFloat(vals, idx, "success") == 1 {
			continue
		}
		item := domain.FailureItem{
			StepIndex:           step.Index,
			Bucket:              step.Bucket,
			Parallel:            step.Parallel,
			RequestIndex:        no,
			Error:               asString(vals, idx, "error"),
			RequestBodyExcerpt:  truncate(asStringAny(vals, idx, "request", "request_body", "messages"), excerptChars),
			ResponseBodyExcerpt: truncate(asStringAny(vals, idx, "response", "response_body", "output"), excerptChars),
		}
		if v := asFloat(vals, idx, "http_status"); v > 0 {
			s := int(v)
			item.HTTPStatus = &s
		} else if v := asFloat(vals, idx, "status_code"); v > 0 {
			s := int(v)
			item.HTTPStatus = &s
		}
		// evalscope 1.9.x 的 result 表没有 error / http_status 列，失败行只存 request +
		// 序列化后的 response_messages，无法在 Go 侧解 pickle。对齐
		// gen_report_from_db.py 的做法：至少给出可解释的兑底说明，而不是空白。
		if item.Error == "" && item.HTTPStatus == nil && item.ResponseBodyExcerpt == "" {
			item.Error = "benchmark_data.db 未记录响应/错误内容（evalscope 版本未落 error/status 列）"
		}
		item.Detail = summarize(item.Error, 140)
		if v, ok := nullableFloat(vals, idx, "first_chunk_latency"); ok {
			item.TTFTSec = &v
		}
		if v, ok := nullableFloat(vals, idx, "latency"); ok {
			item.TTLTSec = &v
		}
		item.Reason = classifyFailure(item.Error, func(v []any, name string) (float64, bool) {
			return asFloat(v, idx, name), idx[name] > 0 || hasKey(idx, name)
		}, vals)
		out = append(out, item)
	}
	return out, rows.Err()
}

func hasKey(m map[string]int, k string) bool { _, ok := m[k]; return ok }

func asFloat(vals []any, idx map[string]int, name string) float64 {
	i, ok := idx[name]
	if !ok || i >= len(vals) {
		return 0
	}
	switch x := vals[i].(type) {
	case float64:
		return x
	case int64:
		return float64(x)
	case []byte:
		var f float64
		fmt.Sscanf(string(x), "%g", &f)
		return f
	case string:
		var f float64
		fmt.Sscanf(x, "%g", &f)
		return f
	}
	return 0
}

func nullableFloat(vals []any, idx map[string]int, name string) (float64, bool) {
	i, ok := idx[name]
	if !ok || i >= len(vals) || vals[i] == nil {
		return 0, false
	}
	switch x := vals[i].(type) {
	case float64:
		return x, true
	case int64:
		return float64(x), true
	}
	return 0, false
}

// summarize 把多行错误压成单行摘要（对齐 gen_perf_dashboard._failed_detail 的 140 字符）。
func summarize(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func asString(vals []any, idx map[string]int, name string) string {
	i, ok := idx[name]
	if !ok || i >= len(vals) || vals[i] == nil {
		return ""
	}
	switch x := vals[i].(type) {
	case string:
		return x
	case []byte:
		return string(x)
	}
	return fmt.Sprint(vals[i])
}

func asStringAny(vals []any, idx map[string]int, names ...string) string {
	for _, n := range names {
		if s := asString(vals, idx, n); s != "" {
			return s
		}
	}
	return ""
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n]) + fmt.Sprintf("…[已截断，完整 %d 字符，见 benchmark_data.db]", len([]rune(s)))
}

// LoadAllFailures 遍历多个 step 的 db，汇总失败明细。
func LoadAllFailures(dbPaths map[domain.Step]string, excerptChars int) []domain.FailureItem {
	var out []domain.FailureItem
	for step, p := range dbPaths {
		items, err := LoadFailures(p, step, excerptChars)
		if err != nil {
			continue
		}
		out = append(out, items...)
	}
	return out
}

// MarshalFailuresJSON 便于写 csv/json 产物。
func MarshalFailuresJSON(items []domain.FailureItem) []byte {
	b, _ := json.MarshalIndent(items, "", "  ")
	return b
}
