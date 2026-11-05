package runmgr

import (
	"encoding/json"
	"sync"
	"time"
)

// EventName 是 SSE 事件名（与 docs/API.md §5.10 一致）。
type EventName string

const (
	EvSnapshot     EventName = "run.snapshot"
	EvStarted      EventName = "run.started"
	EvStepStarted  EventName = "step.started"
	EvStepProgress EventName = "step.progress"
	EvStepFinished EventName = "step.finished"
	EvProgress     EventName = "run.progress"
	EvLog          EventName = "run.log"
	EvGate         EventName = "run.gate"
	EvFinished     EventName = "run.finished"
	EvError        EventName = "error"
)

// Event 是一条可广播、可落库的事件。
type Event struct {
	Seq   int             `json:"seq"`
	Event EventName       `json:"event"`
	Data  json.RawMessage `json:"data"`
	At    time.Time       `json:"at"`
}

// hub 负责 run 级事件广播。
type hub struct {
	mu   sync.RWMutex
	subs map[string]map[chan Event]struct{}
}

func newHub() *hub {
	return &hub{subs: map[string]map[chan Event]struct{}{}}
}

func (h *hub) subscribe(runID string) (<-chan Event, func()) {
	ch := make(chan Event, 256)
	h.mu.Lock()
	if h.subs[runID] == nil {
		h.subs[runID] = map[chan Event]struct{}{}
	}
	h.subs[runID][ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		if set, ok := h.subs[runID]; ok {
			if _, ok := set[ch]; ok {
				delete(set, ch)
				close(ch)
			}
			if len(set) == 0 {
				delete(h.subs, runID)
			}
		}
		h.mu.Unlock()
	}
}

func (h *hub) publish(runID string, ev Event) {
	h.mu.RLock()
	set := h.subs[runID]
	h.mu.RUnlock()
	for ch := range set {
		select {
		case ch <- ev:
		default: // 慢消费者丢弃，避免阻塞执行
		}
	}
}
