package engine

import (
	"context"
	"fmt"
	"sort"

	"github.com/apexcloud/thvv/server/internal/config"
	"github.com/apexcloud/thvv/server/internal/domain"
)

// Registry 是工具注册表：engines.<name> → (capability → Engine)。
type Registry struct {
	entries map[string]*Entry
	order   []string
}

// Entry 是一个工具的运行时视图。
type Entry struct {
	Name   string
	Desc   Description
	byCap  map[domain.Capability]Engine
	status map[domain.Capability]string
	reason map[domain.Capability]string
}

// NewRegistry 依据配置构造注册表。
func NewRegistry(cfg *config.Config) *Registry {
	r := &Registry{entries: map[string]*Entry{}}

	names := make([]string, 0, len(cfg.Engines))
	for n := range cfg.Engines {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, name := range names {
		eng := cfg.Engines[name]
		if eng == nil {
			continue
		}
		e := &Entry{
			Name:   name,
			byCap:  map[domain.Capability]Engine{},
			status: map[domain.Capability]string{},
			reason: map[domain.Capability]string{},
		}
		perfCfg := eng.Capabilities["perf"]
		var buckets []domain.Bucket
		if perfCfg != nil {
			for _, b := range perfCfg.Buckets {
				builtIn := true
				if b.DatasetBuiltIn != nil {
					builtIn = *b.DatasetBuiltIn
				}
				buckets = append(buckets, domain.Bucket{
					Label: b.Label,
					PromptTokens: struct {
						Min int `json:"min"`
						Max int `json:"max"`
					}{Min: b.Min, Max: b.Max},
					MaxParallel: b.MaxParallel,
					BuiltIn:     builtIn,
				})
			}
		}

		for capName, capCfg := range eng.Capabilities {
			if capCfg == nil {
				continue
			}
			cap := domain.Capability(capName)
			if !eng.Enabled {
				e.status[cap] = "unavailable"
				e.reason[cap] = "引擎未启用（enabled=false）"
				continue
			}
			if !capCfg.Enabled {
				st := capCfg.Status
				if st == "" {
					st = "unavailable"
				}
				e.status[cap] = st
				e.reason[cap] = "capability 未启用"
				continue
			}
			switch eng.Kind {
			case "builtin":
				f := NewFake(buckets)
				e.byCap[cap] = f
				e.status[cap] = orDefault(capCfg.Status, "ga")
			case "subprocess":
				home := ""
				if eng.Runtime != nil && eng.Runtime.Home != "" {
					home = string(eng.Runtime.Home)
				} else if v, ok := cfg.Resolved["roots.thvv"]; ok {
					home = v
				}
				if cap == domain.CapEval {
					e.byCap[cap] = NewEval(name, eng, capCfg, home)
				} else {
					e.byCap[cap] = NewSubprocess(name, eng, capCfg, home)
				}
				e.status[cap] = orDefault(capCfg.Status, "ga")
			case "http":
				e.status[cap] = "unavailable"
				e.reason[cap] = "外部 HTTP 工具代理尚未实现（v1.1）"
			default:
				e.status[cap] = "unavailable"
				e.reason[cap] = "未知的 kind: " + eng.Kind
			}
		}

		e.Desc = Description{
			Name:        name,
			DisplayName: orDefault(eng.DisplayName, defaultDisplayName(name)),
			Version:     orDefault(eng.Version, "1.0.0"),
			Kind:        eng.Kind,
			Runtime:     runtimeInfo(eng),
		}
		capNames := make([]string, 0, len(eng.Capabilities))
		for c := range eng.Capabilities {
			capNames = append(capNames, c)
		}
		sort.Strings(capNames)
		for _, c := range capNames {
			capCfg := eng.Capabilities[c]
			cap := domain.Capability(c)
			st := e.status[cap]
			if st == "" {
				st = "unavailable"
			}
			desc := CapabilityDesc{
				Name: c, Status: st,
				Description: capCfg.Description, AvailableAt: capCfg.AvailableAt,
				PlanSchema: capCfg.PlanSchema, ResultSchema: capCfg.ResultSchema,
			}
			if desc.Description == "" {
				desc.Description = defaultCapDescription(c)
			}
			if e.byCap[cap] != nil {
				desc.Artifacts = []string{"html_report", "xlsx_report", "failure_csv", "sla_json", "raw_db", "run_log"}
			}
			e.Desc.Capabilities = append(e.Desc.Capabilities, desc)
		}
		r.entries[name] = e
		r.order = append(r.order, name)
	}
	return r
}

// Tools 返回全部工具描述（GET /v1/tools）。
func (r *Registry) Tools() []Description {
	out := make([]Description, 0, len(r.order))
	for _, n := range r.order {
		out = append(out, r.entries[n].Desc)
	}
	return out
}

// Get 返回某工具的某 capability 引擎。
func (r *Registry) Get(tool string, cap domain.Capability) (Engine, error) {
	e, ok := r.entries[tool]
	if !ok {
		return nil, fmt.Errorf("未知工具 %q", tool)
	}
	eng, ok := e.byCap[cap]
	if !ok {
		st := e.status[cap]
		reason := e.reason[cap]
		if st == "planned" {
			return nil, fmt.Errorf("capability %s 尚未实现（available_at=%s）", cap, e.Desc.Capabilities)
		}
		if reason == "" {
			reason = "未实现"
		}
		return nil, fmt.Errorf("capability %s 不可用: %s", cap, reason)
	}
	return eng, nil
}

// Status 返回某工具某能力的状态。
func (r *Registry) Status(tool string, cap domain.Capability) (string, string) {
	e, ok := r.entries[tool]
	if !ok {
		return "unavailable", "未知工具"
	}
	st, ok := e.status[cap]
	if !ok {
		return "unavailable", "未配置"
	}
	return st, e.reason[cap]
}

// AnyProbe 选一个可用引擎做 probe。
func (r *Registry) AnyProbe(tool string) Engine {
	if e, ok := r.entries[tool]; ok {
		for _, cap := range []domain.Capability{domain.CapPerf, domain.CapEval, domain.CapFunctional} {
			if eng, ok := e.byCap[cap]; ok {
				return eng
			}
		}
	}
	for _, name := range r.order {
		e := r.entries[name]
		for _, eng := range e.byCap {
			return eng
		}
	}
	return nil
}

// SelfCheck 启动自检；返回告警列表（不阻塞启动）。
func (r *Registry) SelfCheck(ctx context.Context) []string {
	var warns []string
	for _, name := range r.order {
		e := r.entries[name]
		for cap, eng := range e.byCap {
			_ = eng
			_ = cap
		}
		if len(e.byCap) == 0 {
			warns = append(warns, fmt.Sprintf("engines.%s: 没有可用的 capability", name))
		}
	}
	return warns
}

func runtimeInfo(eng *config.Engine) map[string]string {
	out := map[string]string{"kind": eng.Kind}
	if eng.Runtime != nil {
		if eng.Runtime.Python != "" {
			out["python"] = string(eng.Runtime.Python)
		}
		if eng.Runtime.Evalscope != "" {
			out["evalscope"] = string(eng.Runtime.Evalscope)
		}
		if eng.Runtime.Home != "" {
			out["home"] = string(eng.Runtime.Home)
		}
	}
	return out
}

func defaultDisplayName(name string) string {
	if name == "thvv" {
		return "THVV TokenHub Vendor Verifier"
	}
	return name
}

func defaultCapDescription(c string) string {
	switch c {
	case "perf":
		return "性能压测 1k~1m 档位 × 并发梯度"
	case "eval":
		return "效果评测（LLM Judge）"
	case "functional":
		return "功能测试（工具调用 / 长上下文 / 结构化输出）"
	}
	return ""
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
