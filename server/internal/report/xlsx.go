package report

import (
	"bytes"
	"fmt"
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
	Gate          *domain.GateResult
	Metrics       []domain.StepMetrics
	Failures      []domain.FailureItem
}

// 45 列（性能指标 sheet）。列序即交付表结构，勿随意调整。
var metricHeaders = []string{
	"数据集", "并发数", "总请求数", "成功数", "失败数", "成功率",
	"Avg TTFT(s)", "Min TTFT(s)", "Max TTFT(s)", "P50 TTFT(s)", "P90 TTFT(s)", "P95 TTFT(s)", "P99 TTFT(s)",
	"Avg TTLT(s)", "Avg TPOT(ms)", "Avg ITL(ms)", "P50 ITL(ms)", "P90 ITL(ms)",
	"平均输出速率(t/s)", "Input TPS", "Output TPS", "Input TPM", "Output TPM", "总 TPM", "QPM",
	"Prompt Tokens", "Completion Tokens", "Total Tokens",
	"总耗时(s)", "HTTP 429 次数", "超时次数", "内容拦截次数", "连接错误次数", "其他失败",
	"空档位提示", "SLA 档位", "P50 阈值(s)", "P90 阈值(s)", "SLA 结论",
	"模型", "供应商", "客户端", "生成时间", "原始 DB", "备注",
}

// RenderXLSX 生成 45 列指标表 + 失败请求详情的 Excel。
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

	// ---- sheet 1: 性能指标 ----
	f.SetSheetName("Sheet1", metricsSheet)
	headerStyle, _ := f.NewStyle(&excelize.Style{
		Font:      &excelize.Font{Bold: true, Color: "FFFFFF"},
		Fill:      excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{"2563EB"}},
		Alignment: &excelize.Alignment{Horizontal: "center", Vertical: "center", WrapText: true},
	})
	for i, h := range metricHeaders {
		cell, _ := excelize.CoordinatesToCellName(i+1, 1)
		f.SetCellValue(metricsSheet, cell, h)
	}
	f.SetRowHeight(metricsSheet, 1, 32)
	_ = f.SetCellStyle(metricsSheet, "A1", mustCell(len(metricHeaders), 1), headerStyle)

	// 档位阈值查找
	tierFor := func(m domain.StepMetrics) (string, float64, float64) {
		if in.Gate == nil {
			return "", 0, 0
		}
		return "", 0, 0
	}
	_ = tierFor

	for r, m := range in.Metrics {
		row := r + 2
		vals := []any{
			m.Bucket, m.Parallel, m.Requests.Total, m.Requests.Success, m.Requests.Failed, m.Requests.SuccessRate,
			m.TTFT.AvgSec, m.TTFT.MinSec, m.TTFT.MaxSec, m.TTFT.P50Sec, m.TTFT.P90Sec, m.TTFT.P95Sec, m.TTFT.P99Sec,
			m.TTLT.AvgSec, m.TPOT.AvgMs, m.ITL.AvgMs, m.ITL.P50Ms, m.ITL.P90Ms,
			m.TokenRateAvg, m.Throughput.InputTPS, m.Throughput.OutputTPS, m.Throughput.InputTPM,
			m.Throughput.OutputTPM, m.Throughput.TPM, m.Throughput.QPM,
			m.Tokens.PromptSum, m.Tokens.CompletionSum, m.Tokens.TotalSum,
			m.DurationSec,
			m.Failures["http_429"], m.Failures["timeout"], m.Failures["content_filter"],
			m.Failures["connection"], m.Failures["other"] + m.Failures["server_error"] + m.Failures["bad_request"] + m.Failures["auth_error"] + m.Failures["unknown"],
			"", "", "", "", "",
			in.Model, in.Vendor, in.Company, in.GeneratedAt.Format("2006-01-02 15:04:05"), m.RawDB, "",
		}
		for i, v := range vals {
			cell, _ := excelize.CoordinatesToCellName(i+1, row)
			f.SetCellValue(metricsSheet, cell, v)
		}
	}
	// 百分比与小数格式
	pctStyle, _ := f.NewStyle(&excelize.Style{NumFmt: 10}) // 0.00%
	f4Style, _ := f.NewStyle(&excelize.Style{NumFmt: 4})   // #,##0.00
	lastRow := len(in.Metrics) + 1
	if lastRow >= 2 {
		_ = f.SetCellStyle(metricsSheet, "F2", fmt.Sprintf("F%d", lastRow), pctStyle)
		for _, col := range []string{"G", "H", "I", "J", "K", "L", "M", "N", "O", "P", "Q", "R", "S", "T", "U", "V", "W", "X", "AC"} {
			_ = f.SetCellStyle(metricsSheet, col+"2", fmt.Sprintf("%s%d", col, lastRow), f4Style)
		}
	}
	f.SetColWidth(metricsSheet, "A", "A", 10)
	f.SetColWidth(metricsSheet, "B", "Z", 14)
	f.SetColWidth(metricsSheet, "AP", "AR", 18)
	f.SetPanes(metricsSheet, &excelize.Panes{Freeze: true, XSplit: 2, YSplit: 1, TopLeftCell: "C2", ActivePane: "bottomRight"})

	// ---- sheet 2: 失败请求详情 ----
	_, _ = f.NewSheet(failuresSheet)
	failHeaders := []string{"档位", "并发", "请求序号", "失败原因", "HTTP状态", "错误信息", "请求片段", "响应片段"}
	for i, h := range failHeaders {
		cell, _ := excelize.CoordinatesToCellName(i+1, 1)
		f.SetCellValue(failuresSheet, cell, h)
	}
	_ = f.SetCellStyle(failuresSheet, "A1", mustCell(len(failHeaders), 1), headerStyle)
	for r, it := range in.Failures {
		row := r + 2
		httpStatus := ""
		if it.HTTPStatus != nil {
			httpStatus = fmt.Sprint(*it.HTTPStatus)
		}
		for i, v := range []any{it.Bucket, it.Parallel, it.RequestIndex, it.Reason, httpStatus,
			it.Error, it.RequestBodyExcerpt, it.ResponseBodyExcerpt} {
			cell, _ := excelize.CoordinatesToCellName(i+1, row)
			f.SetCellValue(failuresSheet, cell, v)
		}
	}
	f.SetColWidth(failuresSheet, "A", "E", 12)
	f.SetColWidth(failuresSheet, "F", "F", 40)
	f.SetColWidth(failuresSheet, "G", "H", 60)

	var buf bytes.Buffer
	if err := f.Write(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func mustCell(col, row int) string {
	c, _ := excelize.CoordinatesToCellName(col, row)
	return c
}
