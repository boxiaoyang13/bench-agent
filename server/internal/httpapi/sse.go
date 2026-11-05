package httpapi

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/apexcloud/thvv/server/internal/domain"
	"github.com/apexcloud/thvv/server/internal/runmgr"
)

// handleRunEvents 提供 SSE 进度流。
//
// 语义（docs/API.md §5.10）：
//   - 建连先推 run.snapshot（无 id）
//   - 之后按 seq 重放历史事件（支持 Last-Event-ID / ?since= 断点续传）
//   - 再进入实时广播；15s 无事件发一条注释心跳
//   - 收到终态后推 run.finished 并关闭
func (s *Server) handleRunEvents(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("run_id")
	run, err := s.mgr.Get(r.Context(), runID, false)
	if err != nil {
		writeProblem(w, r, errProblem(err))
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeProblem(w, r, domain.NewProblem(500, "internal_error", "当前连接不支持流式响应"))
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no") // 关闭 nginx 缓冲
	w.WriteHeader(http.StatusOK)

	since := atoiDefault(r.URL.Query().Get("since"), 0)
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			since = n
		}
	}

	// 1) 快照
	if raw, err := json.Marshal(run); err == nil {
		writeSSE(w, flusher, 0, runmgr.EvSnapshot, raw)
	}

	// 2) 重放历史
	if since >= 0 {
		if evs, err := s.mgr.Replay(r.Context(), runID, since); err == nil {
			for _, ev := range evs {
				writeSSE(w, flusher, ev.Seq, ev.Event, ev.Data)
			}
		}
	}

	// 终态：推完就关
	if run.Status.Terminal() {
		return
	}

	// 3) 实时订阅
	ch, unsub := s.mgr.Subscribe(runID)
	defer unsub()

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			_, _ = w.Write([]byte(": heartbeat\n\n"))
			flusher.Flush()
		case ev, ok := <-ch:
			if !ok {
				return
			}
			writeSSE(w, flusher, ev.Seq, ev.Event, ev.Data)
			if ev.Event == runmgr.EvFinished {
				return
			}
		}
	}
}

func writeSSE(w http.ResponseWriter, f http.Flusher, seq int, event runmgr.EventName, data []byte) {
	if seq > 0 {
		_, _ = w.Write([]byte("id: " + strconv.Itoa(seq) + "\n"))
	}
	_, _ = w.Write([]byte("event: " + string(event) + "\n"))
	_, _ = w.Write([]byte("data: "))
	_, _ = w.Write(data)
	_, _ = w.Write([]byte("\n\n"))
	f.Flush()
}
