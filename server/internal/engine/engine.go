// Package engine 定义执行引擎抽象与实现。
//
// 协议层只认 tool + capability + plan；引擎负责把 plan 变成实际执行。
// 已实现：
//   - fake:   builtin 模拟引擎（无 Python 依赖，用于 UI 联调与演示）
//   - thvv:   subprocess 引擎，fork evalscope（生产用）
//
// 新增工具只需实现 Engine 并注册（见 registry.go），协议不变。
package engine

import (
	"context"
	"encoding/json"
	"time"

	"github.com/apexcloud/thvv/server/internal/domain"
)

// EventKind 是引擎 → RunManager 的事件类型（与 SSE 事件同名）。
type EventKind string

const (
	EvStepStarted  EventKind = "step.started"
	EvStepProgress EventKind = "step.progress"
	EvStepFinished EventKind = "step.finished"
	EvLog          EventKind = "run.log"
	EvError        EventKind = "error"
)

// Event 是引擎推送的单条事件。
type Event struct {
	Kind EventKind      `json:"kind"`
	Data map[string]any `json:"data"`
	At   time.Time      `json:"at"`
}

// ResolvedTarget 是含明文凭据的 target（仅内存中短暂存在）。
type ResolvedTarget struct {
	TargetID       string
	Vendor         string
	Model          string
	APIURL         string
	Protocol       domain.Protocol
	APIKey         string
	Tokenizer      string
	Temperature    any
	MaxTokens      int
	ConnectTimeout time.Duration
	ReadTimeout    time.Duration
}

// Spec 是引擎执行所需的全部输入（协议层已规范化）。
type Spec struct {
	RunID            string
	Capability       domain.Capability
	Target           ResolvedTarget
	Plan             domain.Plan
	Steps            []domain.StepSpec
	WorkDir          string
	DatasetsDir      string
	LongBuckets      map[string]bool
	DatasetTokenizer string
}

// Result 是引擎执行结果。
type Result struct {
	Steps    []domain.Step
	Metrics  []domain.StepMetrics
	DBPaths  map[int]string // stepIndex → benchmark_data.db
	LogPath  string
	ExitCode int
}

// Description 用于 GET /v1/tools。
type Description struct {
	Name         string            `json:"name"`
	DisplayName  string            `json:"display_name"`
	Version      string            `json:"version"`
	Kind         string            `json:"kind"`
	Capabilities []CapabilityDesc  `json:"capabilities"`
	Runtime      map[string]string `json:"runtime,omitempty"`
}

type CapabilityDesc struct {
	Name         string   `json:"name"`
	Status       string   `json:"status"`
	Description  string   `json:"description,omitempty"`
	AvailableAt  string   `json:"available_at,omitempty"`
	PlanSchema   string   `json:"plan_schema"`
	ResultSchema string   `json:"result_schema"`
	Artifacts    []string `json:"artifacts,omitempty"`
}

// ProbeReport 是 POST /v1/targets/{id}/probe 的结果。
type ProbeReport struct {
	TargetID  string            `json:"target_id"`
	OK        bool              `json:"ok"`
	CheckedAt time.Time         `json:"checked_at"`
	ElapsedMS int64             `json:"elapsed_ms"`
	Checks    []CheckItem       `json:"checks"`
	Engine    map[string]string `json:"engine,omitempty"`
	Err       string            `json:"-"`
}

type CheckItem struct {
	Name      string   `json:"name"`
	OK        bool     `json:"ok"`
	Severity  string   `json:"severity,omitempty"` // error | warn
	Detail    string   `json:"detail,omitempty"`
	LatencyMS int64    `json:"latency_ms,omitempty"`
	Missing   []string `json:"missing,omitempty"`
}

// Engine 是执行引擎接口。
type Engine interface {
	Name() string
	Describe() Description
	// Probe 校验端点连通性/协议/tokenizer/数据集。
	Probe(ctx context.Context, t ResolvedTarget) ProbeReport
	// Run 执行计划；事件通过 emit 推送；ctx 取消必须能终止执行。
	Run(ctx context.Context, spec Spec, emit func(Event)) (Result, error)
	// Validate 校验工具私有字段（v1 只做 JSON 合法性检查）。
	Validate(cap domain.Capability, plan domain.Plan) error
}

// CapabilityStatus 反映某 capability 当前是否可用。
type CapabilityStatus struct {
	Name   string
	Status string // ga | preview | planned | unavailable
	Reason string
}

// jsonRaw 便捷解析 tool_params。
func jsonRaw(b json.RawMessage, dst any) error {
	if len(b) == 0 {
		return nil
	}
	return json.Unmarshal(b, dst)
}
