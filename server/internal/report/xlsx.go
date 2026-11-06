package report

import (
	"bytes"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/apexcloud/thvv/server/internal/domain"
	"github.com/xuri/excelize/v2"
)

// XLSXInput 是生成 Excel 指标报告的输入。
type XLSXInput struct {
	SheetMetrics  string
	SheetFailures string
	Model         string
	Vendor        string
	Company       string
	CompanyLabel  string
	GeneratedAt   time.Time
	Gate          *domain.GateResult // 仅用于兼容调用方，不写入 sheet
	APIURL        string             // 写入失败明细的「请求URL」
	Metrics       []domain.StepMetrics
	Failures      []domain.FailureItem
}

// metricHeaders 是「性能指标」sheet 的 45 列。
// 列序/列名与 thvv/perf/scripts/gen_report_from_db.py 的 HEADERS **逐字一致**，
// 保证 Go 交付物与 `bash run.sh bench` 的 xlsx 可以逐列对比。勿随意调整。
var metricHeaders = []string{
	"客户端", "后端网路链接",
	"数据集", "并发数", "总请求数", "成功数", "失败数", "成功率",
	"Avg TTFT(s)", "Min TTFT(s)", "Max TTFT(s)", "P50 TTFT(s)", "P90 TTFT(s)", "P95 TTFT(s)", "P99 TTFT(s)",
	"Avg TTLT(s)", "P50 TTLT(s)", "P90 TTLT(s)", "P95 TTLT(s)", "P99 TTLT(s)",
	"Avg Rate(t/s)", "P50 Rate(t/s)", "P90 Rate(t/s)", "P95 Rate(t/s)", "P99 Rate(t/s)",
	"Avg ITL(s)", "P50 ITL(s)", "P90 ITL(s)", "P95 ITL(s)", "P99 ITL(s)",
	"Avg TPOT(s)", "P50 TPOT(s)", "P90 TPOT(s)", "P95 TPOT(s)", "P99 TPOT(s)",
	"Avg Prompt Tokens", "Avg Completion Tokens", "Total Tokens",
	"Avg Cached Tokens", "Avg Cache Hit Ratio", "Avg Total Time(ms)",
	"Output TPM", "Output TPS", "Input TPM", "TPM",
}

// outputTPM / outputTPS 等按 Python 口径（Output TPM 在前）。
func metricRow(in XLSXInput, m domain.StepMetrics) []any {
	success := m.Requests.Success
	avgPrompt, avgCompletion := 0.0, 0.0
	if success > 0 {
		avgPrompt = float64(m.Tokens.PromptSum) / float64(success)
		avgCompletion = float64(m.Tokens.CompletionSum) / float64(success)
	}
	dataset := m.Bucket
	if tag := runTagFromDB(m.RawDB); tag != "" {
		dataset = fmt.Sprintf("%s (%s)", m.Bucket, tag)
	}
	return []any{
		in.Company, in.Vendor,
		dataset, m.Parallel, m.Requests.Total, m.Requests.Success, m.Requests.Failed, m.Requests.SuccessRate,
		m.TTFT.AvgSec, m.TTFT.MinSec, m.TTFT.MaxSec, m.TTFT.P50Sec, m.TTFT.P90Sec, m.TTFT.P95Sec, m.TTFT.P99Sec,
		m.TTLT.AvgSec, m.TTLT.P50Sec, m.TTLT.P90Sec, m.TTLT.P95Sec, m.TTLT.P99Sec,
		m.TokenRateAvg, m.TokenRate.P50Sec, m.TokenRate.P90Sec, m.TokenRate.P95Sec, m.TokenRate.P99Sec,
		m.ITL.AvgSec, m.ITL.P50Sec, m.ITL.P90Sec, m.ITL.P95Sec, m.ITL.P99Sec,
		m.TPOT.AvgSec, m.TPOT.P50Sec, m.TPOT.P90Sec, m.TPOT.P95Sec, m.TPOT.P99Sec,
		avgPrompt, avgCompletion, m.Tokens.TotalSum,
		0, 0, m.TTLT.AvgSec * 1000,
		m.Throughput.OutputTPM, m.Throughput.OutputTPS, m.Throughput.InputTPM, m.Throughput.TPM,
	}
}

// RenderXLSX 生成「性能指标」45 列 + 「失败请求详情」14 列的 Excel（对齐 thvv 平台版）。
func RenderXLSX(in XLSXInput) ([]byte, error) {
	f := excelize.NewFile()
	defer f.Close()

	metricsSheet := in.SheetMetrics
	if metricsSheet == "" {
		metricsSheet = "性能指标"
	}
	failuresSheet := in.SheetFailures
	if failuresSheet == "" {
		failuresSheet = "失败请求详情"
	}

	headerStyle, _ := f.NewStyle(&excelize.Style{
		Font:      &excelize.Font{Bold: true, Color: "FFFFFF"},
		Fill:      excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{"4472C4"}},
		Alignment: &excelize.Alignment{Horizontal: "center", Vertical: "center", WrapText: true},
	})

	// ---- sheet 1: 性能指标 ----
	f.SetSheetName("Sheet1", metricsSheet)
	for i, h := range metricHeaders {
		cell, _ := excelize.CoordinatesToCellName(i+1, 1)
		f.SetCellValue(metricsSheet, cell, h)
	}
	_ = f.SetCellStyle(metricsSheet, "A1", mustCell(len(metricHeaders), 1), headerStyle)
	f.SetRowHeight(metricsSheet, 1, 32)

	styleCache := map[string]int{}
	styleFor := func(numFmt int) int {
		key := fmt.Sprint(numFmt)
		if id, ok := styleCache[key]; ok {
			return id
		}
		id, _ := f.NewStyle(&excelize.Style{NumFmt: numFmt})
		styleCache[key] = id
		return id
	}
	// 与 gen_report_from_db.py 相同的数字格式约定；
	// 无成功样本的 step 不进「性能指标」（Python calc_stats 返回 None 跳过），
	// 这类 run 的诊断信息在「失败请求详情」里。
	rowNo := 1
	for _, m := range in.Metrics {
		if m.Requests.Success == 0 {
			continue
		}
		rowNo++
		row := rowNo
		for i, v := range metricRow(in, m) {
			cell, _ := excelize.CoordinatesToCellName(i+1, row)
			f.SetCellValue(metricsSheet, cell, v)
			if _, isNum := v.(float64); !isNum {
				continue
			}
			header := metricHeaders[i]
			switch {
			case strings.HasPrefix(header, "Avg ") || strings.HasPrefix(header, "Min ") ||
				strings.HasPrefix(header, "Max ") || strings.HasPrefix(header, "P50 ") ||
				strings.HasPrefix(header, "P90 ") || strings.HasPrefix(header, "P95 ") ||
				strings.HasPrefix(header, "P99 "):
				if strings.Contains(header, "(s)") || strings.Contains(header, "(t/s)") {
					_ = f.SetCellStyle(metricsSheet, cell, cell, styleFor(2)) // 0.000
				} else if strings.Contains(header, "(ms)") {
					_ = f.SetCellStyle(metricsSheet, cell, cell, styleFor(2))
				}
			case header == "成功率" || header == "Avg Cache Hit Ratio":
				_ = f.SetCellStyle(metricsSheet, cell, cell, styleFor(10)) // 0.00%
			case header == "Total Tokens" || header == "并发数" || header == "总请求数" ||
				header == "成功数" || header == "失败数":
				_ = f.SetCellStyle(metricsSheet, cell, cell, styleFor(1)) // 0
			case header == "Output TPM" || header == "Output TPS" || header == "Input TPM" || header == "TPM":
				_ = f.SetCellStyle(metricsSheet, cell, cell, styleFor(2)) // 0.00
			}
		}
	}
	_ = f.SetColWidth(metricsSheet, "A", "A", 18)
	_ = f.SetColWidth(metricsSheet, "B", "B", 18)
	_ = f.SetColWidth(metricsSheet, "C", "C", 30)
	_ = f.SetColWidth(metricsSheet, "D", "AS", 14)
	_ = f.SetPanes(metricsSheet, &excelize.Panes{Freeze: true, XSplit: 1, YSplit: 1, TopLeftCell: "B2", ActivePane: "bottomRight"})

	// ---- sheet 2: 失败请求详情（14 列，对齐 Python FAIL_HEADERS）----
	_, _ = f.NewSheet(failuresSheet)
	failHeaders := []string{"序号", "数据集", "并发数", "请求URL", "请求方法", "请求Headers", "请求Body",
		"HTTP状态码", "响应Body", "Request ID", "TTFT(s)", "TTLT(s)", "错误类型", "详细说明"}
	for i, h := range failHeaders {
		cell, _ := excelize.CoordinatesToCellName(i+1, 1)
		f.SetCellValue(failuresSheet, cell, h)
	}
	_ = f.SetCellStyle(failuresSheet, "A1", mustCell(len(failHeaders), 1), headerStyle)

	apiURL := safeURL(in.APIURL)
	for r, it := range in.Failures {
		row := r + 2
		httpStatus := ""
		if it.HTTPStatus != nil {
			httpStatus = fmt.Sprint(*it.HTTPStatus)
		}
		var ttft, ttlt any
		if it.TTFTSec != nil {
			ttft = *it.TTFTSec
		}
		if it.TTLTSec != nil {
			ttlt = *it.TTLTSec
		}
		vals := []any{r + 1, it.Bucket, it.Parallel, apiURL, "POST", nil, it.RequestBodyExcerpt,
			httpStatus, it.ResponseBodyExcerpt, nil, ttft, ttlt, it.Reason, it.Detail}
		for i, v := range vals {
			cell, _ := excelize.CoordinatesToCellName(i+1, row)
			f.SetCellValue(failuresSheet, cell, v)
			if h := failHeaders[i]; h == "TTFT(s)" || h == "TTLT(s)" {
				if _, ok := v.(float64); ok {
					_ = f.SetCellStyle(failuresSheet, cell, cell, styleFor(2))
				}
			}
		}
	}
	for i, w := range []float64{6, 30, 8, 40, 8, 20, 60, 10, 60, 24, 10, 10, 14, 50} {
		col, _ := excelize.ColumnNumberToName(i + 1)
		_ = f.SetColWidth(failuresSheet, col, col, w)
	}
	_ = f.SetPanes(failuresSheet, &excelize.Panes{Freeze: true, YSplit: 1, TopLeftCell: "A2", ActivePane: "bottomLeft"})

	var buf bytes.Buffer
	if err := f.Write(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// runTagFromDB 从 .../results/<run-tag>/<model>/parallel_x_y/benchmark_data.db 取 run-tag。
func runTagFromDB(dbPath string) string {
	if dbPath == "" {
		return ""
	}
	dir := filepath.Dir(filepath.Dir(filepath.Dir(dbPath)))
	tag := filepath.Base(dir)
	if tag == "." || tag == string(filepath.Separator) || tag == "results" {
		return ""
	}
	return tag
}

// safeURL 只保留 scheme://host[:port]/path，避免把 query/userinfo 写进报告。
func safeURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return raw
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

func mustCell(col, row int) string {
	c, _ := excelize.CoordinatesToCellName(col, row)
	return c
}
