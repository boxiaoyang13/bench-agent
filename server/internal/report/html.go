package report

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"html/template"
	"strings"
	"time"

	"github.com/apexcloud/thvv/server/internal/domain"
)

// HTMLInput 是渲染性能报告的输入。
type HTMLInput struct {
	Title       string
	Model       string
	Vendor      string
	Company     string
	Capability  string
	GeneratedAt time.Time
	PlanMode    string
	Buckets     []string
	Ladder      []int
	Summary     map[string]any
	Gate        *domain.GateResult
	Steps       []domain.Step
	Metrics     []domain.StepMetrics
	Failures    []domain.FailureItem
}

// RenderHTML 生成自包含的性能测试报告（单文件，便于交付）。
func RenderHTML(in HTMLInput) ([]byte, error) {
	var buf bytes.Buffer
	if err := perfTpl.Execute(&buf, in); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

var tplFuncs = template.FuncMap{
	"num": func(v any, digits int) string {
		f, ok := toFloat(v)
		if !ok {
			return "—"
		}
		return fmt.Sprintf("%.*f", digits, f)
	},
	"pct": func(v any) string {
		f, ok := toFloat(v)
		if !ok {
			return "—"
		}
		return fmt.Sprintf("%.2f%%", f*100)
	},
	"dur": func(v any) string {
		f, ok := toFloat(v)
		if !ok {
			return "—"
		}
		return fmt.Sprintf("%.1fs", f)
	},
	"ms": func(v any) string {
		f, ok := toFloat(v)
		if !ok {
			return "—"
		}
		return fmt.Sprintf("%.1fms", f)
	},
	"idx": func(m map[string]any, k string) any {
		if m == nil {
			return nil
		}
		return m[k]
	},
	"statusClass": func(s domain.RunStatus) string {
		switch s {
		case domain.StatusSucceeded:
			return "ok"
		case domain.StatusPartial:
			return "warn"
		case domain.StatusFailed:
			return "err"
		}
		return ""
	},
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case *float64:
		if x == nil {
			return 0, false
		}
		return *x, true
	}
	return 0, false
}

var perfTpl = template.Must(template.New("perf").Funcs(tplFuncs).Parse(perfTemplate))

const perfTemplate = `<!DOCTYPE html>
<html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{.Title}}</title>
<style>
:root{--bg:#f5f6f8;--panel:#fff;--ink:#111827;--muted:#6b7280;--line:#e5e7eb;
--ok:#059669;--warn:#b45309;--err:#dc2626;--accent:#2563eb}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--ink);
font:14px/1.6 system-ui,-apple-system,"PingFang SC","Microsoft YaHei",sans-serif}
.wrap{max-width:1080px;margin:0 auto;padding:28px 20px 60px}
h1{font-size:22px;margin:0 0 4px}
h2{font-size:16px;margin:0 0 14px}
.muted{color:var(--muted)}
.panel{background:var(--panel);border:1px solid var(--line);border-radius:10px;padding:20px;margin-bottom:16px}
.meta{color:var(--muted);font-size:13px;margin-bottom:20px}
.kpis{display:grid;grid-template-columns:repeat(auto-fit,minmax(150px,1fr));gap:12px;margin-bottom:16px}
.kpi{background:var(--panel);border:1px solid var(--line);border-radius:10px;padding:14px}
.kpi .k{font-size:11.5px;color:var(--muted);text-transform:uppercase;letter-spacing:.04em}
.kpi .v{font-size:22px;font-weight:650;margin-top:2px;font-variant-numeric:tabular-nums}
.kpi .s{font-size:11.5px;color:var(--muted)}
table{width:100%;border-collapse:collapse;font-size:13px}
th{text-align:left;font-size:11.5px;text-transform:uppercase;letter-spacing:.04em;color:var(--muted);
padding:8px 10px;border-bottom:1px solid var(--line);white-space:nowrap}
td{padding:8px 10px;border-bottom:1px solid var(--line)}
tr:last-child td{border-bottom:0}
.num{text-align:right;font-variant-numeric:tabular-nums}
.badge{display:inline-block;padding:2px 9px;border-radius:999px;font-size:11.5px;font-weight:600}
.badge.ok{background:#ecfdf5;color:var(--ok)}
.badge.warn{background:#fffbeb;color:var(--warn)}
.badge.err{background:#fef2f2;color:var(--err)}
.badge.info{background:#eff6ff;color:var(--accent)}
.verdict{display:inline-block;padding:4px 12px;border-radius:999px;font-weight:650;font-size:13px}
pre{background:#f9fafb;border:1px solid var(--line);border-radius:8px;padding:10px;
font-size:11.5px;overflow:auto;max-height:220px;white-space:pre-wrap;word-break:break-all;margin:6px 0 0}
details summary{cursor:pointer;color:var(--accent);font-size:12.5px}
footer{margin-top:24px;font-size:12px;color:var(--muted);text-align:center}
</style></head><body><div class="wrap">

<h1>{{.Title}}</h1>
<div class="meta">
  模型 <b>{{.Model}}</b>{{if .Vendor}} · 供应商 <b>{{.Vendor}}</b>{{end}} ·
  协议能力 {{.Capability}} · 范围 {{.PlanMode}} ·
  生成于 {{.GeneratedAt.Format "2006-01-02 15:04:05"}}
</div>

<div class="kpis">
  <div class="kpi"><div class="k">成功率</div><div class="v">{{pct (idx .Summary "success_rate")}}</div>
    <div class="s">{{idx .Summary "steps_succeeded"}}/{{idx .Summary "steps_total"}} 步成功</div></div>
  <div class="kpi"><div class="k">TTFT P50</div><div class="v">{{num (idx .Summary "ttft_p50_sec") 3}}s</div><div class="s">首 token 延迟</div></div>
  <div class="kpi"><div class="k">TTFT P90</div><div class="v">{{num (idx .Summary "ttft_p90_sec") 3}}s</div><div class="s">长尾</div></div>
  <div class="kpi"><div class="k">TPOT</div><div class="v">{{num (idx .Summary "tpot_ms") 2}}ms</div><div class="s">每输出 token</div></div>
  <div class="kpi"><div class="k">峰值 TPM</div><div class="v">{{num (idx .Summary "tpm_peak") 0}}</div><div class="s">tokens/min</div></div>
  <div class="kpi"><div class="k">峰值 Output</div><div class="v">{{num (idx .Summary "peak_output_tps") 2}}</div><div class="s">tokens/s</div></div>
</div>

{{with .Gate}}
<div class="panel">
  <h2>验收结论 <span class="verdict {{if .Passed}}badge ok{{else}}badge err{{end}}">{{if .Passed}}✓ 通过{{else}}✕ 未达标{{end}}</span></h2>
  <p class="muted">模板 {{.Profile}} · 违规 {{len .Violations}} 项</p>
  {{if .TierSummary}}
  <table><thead><tr><th>档位</th><th class="num">步数</th><th class="num">P50 最大值</th>
  <th class="num">P90 最大值</th><th>结论</th></tr></thead><tbody>
  {{range .TierSummary}}<tr><td><b>{{.Tier}}</b></td><td class="num">{{.Steps}}</td>
  <td class="num">{{num .P50MaxSec 2}}s</td><td class="num">{{num .P90MaxSec 2}}s</td>
  <td>{{if .Passed}}<span class="badge ok">通过</span>{{else}}<span class="badge err">不通过</span>{{end}}</td></tr>{{end}}
  </tbody></table>{{end}}
  {{if .Violations}}
  <h2 style="margin-top:18px">违规明细</h2>
  <table><thead><tr><th>范围</th><th>规则</th><th class="num">实测</th><th class="num">阈值</th><th>说明</th></tr></thead><tbody>
  {{range .Violations}}<tr><td>{{.Tier}}{{.Bucket}}</td><td>{{.Rule}}</td>
  <td class="num">{{num .Actual 3}}</td><td class="num">{{num .Threshold 3}}</td>
  <td class="muted">{{.Message}}</td></tr>{{end}}
  </tbody></table>{{end}}
</div>
{{end}}

<div class="panel">
  <h2>执行步骤与指标</h2>
  <table>
    <thead><tr>
      <th>#</th><th>档位</th><th class="num">并发</th><th class="num">请求</th><th>状态</th>
      <th class="num">成功率</th><th class="num">TTFT avg</th><th class="num">TTFT P50</th>
      <th class="num">TTFT P90</th><th class="num">TPOT</th><th class="num">ITL P50</th>
      <th class="num">Output tps</th><th class="num">TPM</th><th class="num">耗时</th>
    </tr></thead><tbody>
    {{range $i, $m := .Metrics}}
    <tr>
      <td class="muted">{{$i}}</td>
      <td><b>{{$m.Bucket}}</b></td>
      <td class="num">{{$m.Parallel}}</td>
      <td class="num">{{$m.Number}}</td>
      <td>
        {{if eq (printf "%.2f" $m.Requests.SuccessRate) "1.00"}}<span class="badge ok">成功</span>
        {{else if gt $m.Requests.SuccessRate 0.0}}<span class="badge warn">部分</span>
        {{else}}<span class="badge err">失败</span>{{end}}
      </td>
      <td class="num">{{pct $m.Requests.SuccessRate}}</td>
      <td class="num">{{num $m.TTFT.AvgSec 3}}s</td>
      <td class="num">{{num $m.TTFT.P50Sec 3}}s</td>
      <td class="num">{{num $m.TTFT.P90Sec 3}}s</td>
      <td class="num">{{ms $m.TPOT.AvgMs}}</td>
      <td class="num">{{ms $m.ITL.P50Ms}}</td>
      <td class="num">{{num $m.Throughput.OutputTPS 2}}</td>
      <td class="num">{{num $m.Throughput.TPM 0}}</td>
      <td class="num">{{dur $m.DurationSec}}</td>
    </tr>{{end}}
    </tbody>
  </table>
  <p class="muted" style="margin-top:10px;font-size:12px">
    口径说明：TPOT 取自 evalscope 的 <code>time_per_output_token</code>（按请求）；
    ITL 取自 <code>inter_token_latencies</code>（按 chunk 汇总）。两者不是同一口径，不可互相替代。
  </p>
</div>

{{if .Failures}}
<div class="panel">
  <h2>失败请求明细 <span class="badge err">{{len .Failures}}</span></h2>
  <table><thead><tr><th>#</th><th>档位</th><th class="num">并发</th><th>原因</th>
  <th class="num">HTTP</th><th>错误</th></tr></thead><tbody>
  {{range $i, $f := .Failures}}
  <tr><td>{{$i}}</td><td>{{$f.Bucket}}</td><td class="num">{{$f.Parallel}}</td>
  <td><span class="badge err">{{$f.Reason}}</span></td>
  <td class="num">{{if $f.HTTPStatus}}{{$f.HTTPStatus}}{{else}}—{{end}}</td>
  <td><details><summary>展开</summary><pre>{{$f.Error}}
请求：{{$f.RequestBodyExcerpt}}
响应：{{$f.ResponseBodyExcerpt}}</pre></details></td></tr>{{end}}
  </tbody></table>
</div>
{{end}}

<footer>THVV · TokenHub Vendor Verifier{{if .Company}} · {{.Company}}{{end}}</footer>
</div></body></html>`

// RenderFailureCSV 生成失败请求明细 CSV。
func RenderFailureCSV(items []domain.FailureItem) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString("\xEF\xBB\xBF") // BOM，Excel 打开中文不乱码
	w := csv.NewWriter(&buf)
	_ = w.Write([]string{"档位", "并发", "请求序号", "失败原因", "HTTP状态", "错误信息", "请求片段", "响应片段"})
	for _, it := range items {
		httpStatus := ""
		if it.HTTPStatus != nil {
			httpStatus = fmt.Sprint(*it.HTTPStatus)
		}
		_ = w.Write([]string{
			it.Bucket, fmt.Sprint(it.Parallel), fmt.Sprint(it.RequestIndex), it.Reason,
			httpStatus, it.Error, it.RequestBodyExcerpt, it.ResponseBodyExcerpt,
		})
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// SanitizeFilename 去掉文件名里不安全的字符（保留中文）。
func SanitizeFilename(s string) string {
	repl := strings.NewReplacer("/", "_", "\\", "_", ":", "_", "*", "_", "?", "_",
		"\"", "_", "<", "_", ">", "_", "|", "_", "\n", " ", "\r", " ")
	return strings.TrimSpace(repl.Replace(s))
}
