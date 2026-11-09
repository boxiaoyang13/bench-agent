package runmgr

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/apexcloud/thvv/server/internal/domain"
	"github.com/apexcloud/thvv/server/internal/engine"
)

// 上游探测：evalscope 1.9.x 的 result 表不写 error/status_code，压测失败时只能看到
// 「成功率 0%」，看不到是 429 / 502 / 连接被重置还是鉴权失败。
// 因此在有 step 全失败时，服务端自己发一条最小请求，把 HTTP 状态与响应片段记进
// summary.upstream_probe，作为排障依据（也进 run.log）。

const upstreamProbeTimeout = 15 * time.Second

// probeEndpoint 发一条最小 completion 请求，返回可序列化的探测结果。
func probeEndpoint(ctx context.Context, t engine.ResolvedTarget, timeout time.Duration) map[string]any {
	if timeout <= 0 {
		timeout = upstreamProbeTimeout
	}
	out := map[string]any{"at": time.Now().UTC()}
	hdr := map[string]string{"Content-Type": "application/json"}
	var body []byte
	if t.Protocol == domain.ProtocolAnthropic {
		hdr["x-api-key"] = t.APIKey
		hdr["anthropic-version"] = "2023-06-01"
		body, _ = json.Marshal(map[string]any{
			"model": t.Model, "max_tokens": 8, "stream": false,
			"messages": []map[string]any{{"role": "user", "content": "ping"}},
		})
	} else {
		hdr["Authorization"] = "Bearer " + t.APIKey
		body, _ = json.Marshal(map[string]any{
			"model": t.Model, "max_tokens": 8, "stream": false,
			"messages": []map[string]any{{"role": "user", "content": "ping"}},
		})
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.APIURL, bytes.NewReader(body))
	if err != nil {
		out["ok"] = false
		out["detail"] = err.Error()
		return out
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	start := time.Now()
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	out["elapsed_ms"] = time.Since(start).Milliseconds()
	if err != nil {
		out["ok"] = false
		out["error_class"] = classifyNetErr(err)
		out["detail"] = clipStr(sanitizeOneLine(err.Error()), 300)
		return out
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	out["ok"] = resp.StatusCode < 400
	out["status"] = resp.StatusCode
	if excerpt := strings.TrimSpace(string(raw)); excerpt != "" {
		out["body_excerpt"] = clipStr(sanitizeOneLine(excerpt), 400)
	}
	return out
}

// classifyNetErr 给网络错误归个类，方便一眼看出是 DNS / 连接 / 超时。
func classifyNetErr(err error) string {
	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "no such host"):
		return "dns"
	case strings.Contains(s, "connection refused"):
		return "connection_refused"
	case strings.Contains(s, "connection reset"):
		return "connection_reset"
	case strings.Contains(s, "timeout") || strings.Contains(s, "deadline exceeded"):
		return "timeout"
	case strings.Contains(s, "tls") || strings.Contains(s, "certificate"):
		return "tls"
	}
	return "network"
}

// maybeProbeUpstream 在某档全部失败时探测一次上游，把结果塞进 summary。
// 返回 true 表示做了探测（便于日志记录）。
func (m *Manager) maybeProbeUpstream(ctx context.Context, lr *liveRun, apiKey string) bool {
	lr.mu.Lock()
	run := lr.run
	var zeroFailed []string
	for _, st := range run.Steps {
		if st.Status != domain.StepFailed || st.SuccessRate == nil {
			continue
		}
		if *st.SuccessRate <= 0.01 {
			zeroFailed = append(zeroFailed, st.Bucket)
		}
	}
	lr.mu.Unlock()
	if len(zeroFailed) == 0 {
		return false
	}
	// run 已被取消 / 服务端在关闭：不再打扰上游
	if ctx.Err() != nil {
		return false
	}

	dctx, dcancel := detached(ctx)
	defer dcancel()
	probeCtx, cancel := context.WithTimeout(dctx, upstreamProbeTimeout+5*time.Second)
	defer cancel()
	res := probeEndpoint(probeCtx, engine.ResolvedTarget{
		TargetID: run.Target.TargetID, Vendor: run.Target.Vendor, Model: run.Target.Model,
		APIURL: run.Target.APIURL, Protocol: run.Target.Protocol, APIKey: apiKey,
	}, upstreamProbeTimeout)

	lr.mu.Lock()
	if lr.run.Summary == nil {
		lr.run.Summary = map[string]any{}
	}
	lr.run.Summary["upstream_probe"] = res
	lr.run.Summary["zero_success_buckets"] = zeroFailed
	lr.mu.Unlock()

	status, _ := res["status"].(int)
	detail, _ := res["detail"].(string)
	if status == 0 {
		m.log.Warn("上游探测失败", "run_id", run.ID, "buckets", zeroFailed, "class", res["error_class"], "detail", detail)
	} else {
		m.log.Warn("上游探测返回非 2xx", "run_id", run.ID, "buckets", zeroFailed, "status", status,
			"body", fmt.Sprintf("%v", res["body_excerpt"]))
	}
	return true
}
