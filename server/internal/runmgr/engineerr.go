package runmgr

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/apexcloud/thvv/server/internal/domain"
	"github.com/apexcloud/thvv/server/internal/engine"
)

// 失败摘要：把「为什么失败」写进 run.error.detail，避免只能翻 run.log。
//
// 早期实现里 eval 全数据集失败、12h 超时等场景的 error 是 null，排查必须下载
// 几 MB 的 run.log。这里从引擎日志尾部提炼几行可读结论挂到 Problem.Detail 上。

const (
	errorTailBytes    = 512 << 10 // 只读日志尾部 512KB
	errorDetailMaxLen = 1600
	maxSummaryLines   = 6
	lineMaxLen        = 220
)

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

// 日志行首的「时间 - logger - LEVEL:」前缀对排障无意义，摘要里剪掉。
var logPrefixRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}:\d{2}[.,]?\d*\s+-\s+\S+\s+-\s+[A-Z]+\S*\s*:?\s*`)

// 供应商 / 框架侧的错误形态（按出现次数聚合）。
var (
	reHTTPError  = regexp.MustCompile(`Error code: (\d{3}) - \{'error': \{'message': '([^']*)'`)
	reSampleFail = regexp.MustCompile(`(?i)processing item in subset=\S* failed: (.+)`)
	reEvalscope  = regexp.MustCompile(`ERROR\S*\s*(?:\[[^\]]*\]\s*)?:?\s*(.+)`)
)

// summarizeEngineLog 从引擎日志尾部提炼失败要点（已脱敏、已去重、限长）。
func summarizeEngineLog(path string, secrets ...string) []string {
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil
	}
	off := int64(0)
	if st.Size() > errorTailBytes {
		off = st.Size() - errorTailBytes
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return nil
	}
	raw, err := io.ReadAll(f)
	if err != nil {
		return nil
	}
	text := ansiRe.ReplaceAllString(string(raw), "")
	text = strings.ReplaceAll(text, "[stderr]", "")
	text = redactAll(text, secrets...)
	lines := strings.Split(text, "\n")

	var (
		httpCounts = map[string]int{}
		sampleFail = map[string]int{}
		skipped    int
		framework  []string
		seenFrame  = map[string]bool{}
		notables   []string
	)

	for _, ln := range lines {
		trimmed := strings.TrimSpace(ln)
		if trimmed == "" {
			continue
		}
		if m := reHTTPError.FindStringSubmatch(trimmed); m != nil {
			httpCounts[fmt.Sprintf("HTTP %s：%s", m[1], clipStr(sanitizeOneLine(m[2]), 160))]++
			continue
		}
		if m := reSampleFail.FindStringSubmatch(trimmed); m != nil {
			msg := clipStr(sanitizeOneLine(m[1]), 160)
			msg = strings.TrimSuffix(msg, "。")
			sampleFail[msg]++
			continue
		}
		if strings.Contains(trimmed, "Error ignored") {
			skipped++
			continue
		}
		for _, kw := range []string{"produced no conversations", "No reports found", "No objects to concatenate", "没有有效样本"} {
			if strings.Contains(trimmed, kw) && len(notables) < 3 {
				key := clipStr(sanitizeOneLine(trimmed), lineMaxLen)
				if !seenFrame[key] {
					seenFrame[key] = true
					notables = append(notables, key)
				}
			}
		}
		if strings.Contains(trimmed, "ERROR") || strings.Contains(trimmed, "Traceback") {
			if m := reEvalscope.FindStringSubmatch(trimmed); m != nil && len(framework) < 3 {
				key := clipStr(sanitizeOneLine(m[1]), lineMaxLen)
				if !seenFrame[key] {
					seenFrame[key] = true
					framework = append(framework, key)
				}
			} else if strings.Contains(trimmed, "Traceback") && len(framework) < 3 && !seenFrame["Traceback"] {
				seenFrame["Traceback"] = true
				framework = append(framework, "出现 Python Traceback（详见 run.log）")
			}
		}
	}

	out := make([]string, 0, maxSummaryLines)
	type kv struct {
		msg string
		n   int
	}
	kvs := make([]kv, 0, len(httpCounts))
	for msg, n := range httpCounts {
		kvs = append(kvs, kv{msg, n})
	}
	sort.Slice(kvs, func(i, j int) bool {
		if kvs[i].n != kvs[j].n {
			return kvs[i].n > kvs[j].n
		}
		return kvs[i].msg < kvs[j].msg
	})
	for _, e := range kvs {
		if len(out) >= 3 {
			break
		}
		out = append(out, fmt.Sprintf("%s ×%d 次", e.msg, e.n))
	}
	skvs := make([]kv, 0, len(sampleFail))
	for msg, n := range sampleFail {
		skvs = append(skvs, kv{msg, n})
	}
	sort.Slice(skvs, func(i, j int) bool {
		if skvs[i].n != skvs[j].n {
			return skvs[i].n > skvs[j].n
		}
		return skvs[i].msg < skvs[j].msg
	})
	for _, e := range skvs {
		if len(out) >= 3 {
			break
		}
		out = append(out, fmt.Sprintf("样本处理失败 ×%d 次：%s", e.n, e.msg))
	}
	if skipped > 0 {
		out = append(out, fmt.Sprintf("ignore_errors 容错跳过 %d 条样本", skipped))
	}
	// 去重：框架这类提示往往同时命中 ERROR 行与摘要行
	add := func(s string) {
		if s == "" {
			return
		}
		s = strings.TrimSpace(logPrefixRe.ReplaceAllString(s, ""))
		if s == "" {
			return
		}
		for _, e := range out {
			if strings.Contains(e, s) || strings.Contains(s, e) {
				return
			}
		}
		out = append(out, s)
	}
	for _, s := range notables {
		add(s)
	}
	for _, s := range framework {
		add(s)
	}
	if len(out) > maxSummaryLines {
		out = out[:maxSummaryLines]
	}
	return out
}

// problemWithLog 组装带日志摘要的 Problem（截断到 errorDetailMaxLen）。
func problemWithLog(status int, code, headline string, logPath string, secrets ...string) *domain.Problem {
	detail := strings.TrimSpace(headline)
	if tail := summarizeEngineLog(logPath, secrets...); len(tail) > 0 {
		if detail != "" {
			detail += "\n"
		}
		detail += "日志摘要：" + strings.Join(tail, "；")
	}
	detail = clipStr(detail, errorDetailMaxLen)
	return domain.NewProblem(status, code, detail)
}

// progressHeadline 生成「已完成 x/y 步（当前档位/数据集 z）」这样的进度摘要。
func progressHeadline(lr *liveRun) string {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	done := countDone(lr.run.Steps) // 以 step 实际状态为准（Progress.StepsDone 只在终态收口）
	total := lr.run.Progress.StepsTotal
	cur := ""
	if p := lr.run.Progress.Current; p != nil {
		if p.Bucket != "" {
			cur = fmt.Sprintf("，当前 %s", p.Bucket)
		}
		if p.Parallel > 0 {
			cur += fmt.Sprintf("（并发 %d）", p.Parallel)
		}
	}
	return fmt.Sprintf("已完成 %d/%d 步%s", done, total, cur)
}

// evalFailureHeadline 汇总「哪些数据集失败、首因是什么」。
func evalFailureHeadline(res engine.Result) string {
	if res.Eval == nil || len(res.Eval.Datasets) == 0 {
		return "评测未产出任何数据集结果"
	}
	var failed []string
	var firstErr string
	for _, d := range res.Eval.Datasets {
		if d.Success {
			continue
		}
		name := d.Label
		if name == "" {
			name = d.Dataset
		}
		failed = append(failed, name)
		if firstErr == "" && d.Error != "" {
			firstErr = fmt.Sprintf("%s：%s", name, clipStr(sanitizeOneLine(d.Error), 200))
		}
	}
	if len(failed) == 0 {
		return ""
	}
	head := fmt.Sprintf("%d/%d 个数据集评测失败（%s），没有产出有效样本或报告",
		len(failed), len(res.Eval.Datasets), strings.Join(failed, "、"))
	if firstErr != "" {
		head += "；首个错误 " + firstErr
	}
	return head
}

// partialNotice 为 partial 状态生成「哪些数据集失败」的提示（不进 error，避免误判失败）。
func partialNotice(res engine.Result) string {
	if res.Eval == nil {
		return ""
	}
	var failed []string
	for _, d := range res.Eval.Datasets {
		if !d.Success {
			name := d.Label
			if name == "" {
				name = d.Dataset
			}
			failed = append(failed, name)
		}
	}
	if len(failed) == 0 {
		return ""
	}
	return strings.Join(failed, "、")
}

func redactAll(s string, secrets ...string) string {
	for _, sec := range secrets {
		if sec != "" && len(sec) >= 6 {
			s = strings.ReplaceAll(s, sec, "***")
		}
	}
	return s
}

// sanitizeOneLine 去控制字符、压空白，便于进单行 detail。
func sanitizeOneLine(s string) string {
	s = ansiRe.ReplaceAllString(s, "")
	s = strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', '\t':
			return ' '
		}
		if r < 0x20 {
			return -1
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

func clipStr(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	// 按 rune 截断，避免切坏 UTF-8
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
