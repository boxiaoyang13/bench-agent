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
