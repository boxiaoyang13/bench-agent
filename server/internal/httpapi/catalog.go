package httpapi

import (
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/apexcloud/thvv/server/internal/config"
)

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":     "ok",
		"version":    s.version,
		"uptime_sec": int(time.Since(s.started).Seconds()),
	})
}

func (s *Server) handleMeta(w http.ResponseWriter, r *http.Request) {
	caps := map[string]string{}
	for _, t := range s.reg.Tools() {
		for _, c := range t.Capabilities {
			if cur, ok := caps[c.Name]; !ok || rank(c.Status) > rank(cur) {
				caps[c.Name] = c.Status
			}
		}
	}
	order := []string{"perf", "eval", "functional"}
	list := make([]map[string]any, 0, len(order))
	for _, n := range order {
		if st, ok := caps[n]; ok {
			list = append(list, map[string]any{"name": n, "status": st})
		}
	}
	protocols := []string{"openai", "anthropic"}
	writeJSON(w, http.StatusOK, map[string]any{
		"server": map[string]any{
			"version":    s.version,
			"commit":     s.commit,
			"started_at": s.started.UTC(),
		},
		"capabilities": list,
		"limits": map[string]any{
			"max_concurrent_runs":    s.cfg.Limits.MaxConcurrentRuns,
			"max_runs_queued":        s.cfg.Limits.MaxQueuedRuns,
			"run_ttl_hours":          int(s.cfg.Limits.RunRetention.Hours()),
			"artifact_max_bytes":     s.cfg.Limits.ArtifactMaxBytes,
			"sse_heartbeat_sec":      15,
			"max_run_duration_hours": int(s.cfg.Limits.MaxRunDuration.Hours()),
		},
		"protocols": protocols,
		"auth": map[string]any{
			"scheme": "bearer",
			"mode":   s.cfg.Auth.Mode,
			"roles":  []string{"viewer", "operator", "admin"},
		},
	})
}

func rank(status string) int {
	switch status {
	case "ga":
		return 4
	case "preview":
		return 3
	case "planned":
		return 2
	case "unavailable":
		return 1
	}
	return 0
}

func (s *Server) handleTools(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"items": s.reg.Tools()})
}

// perfCapability 返回当前生效的 perf 能力配置（按引擎名排序取第一个启用的）。
func (s *Server) perfCapability() *config.Capability {
	names := make([]string, 0, len(s.cfg.Engines))
	for n := range s.cfg.Engines {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		eng := s.cfg.Engines[n]
		if eng == nil || !eng.Enabled {
			continue
		}
		if p := eng.Capabilities["perf"]; p != nil && p.Enabled {
			return p
		}
	}
	for _, n := range names {
		if p := s.cfg.Engines[n].Capabilities["perf"]; p != nil {
			return p
		}
	}
	return nil
}

func (s *Server) handleCatalogBuckets(w http.ResponseWriter, r *http.Request) {
	perf := s.perfCapability()
	if perf == nil {
		writeJSON(w, http.StatusOK, map[string]any{"items": []any{}})
		return
	}
	type bucketJSON struct {
		Label        string `json:"label"`
		PromptTokens struct {
			Min int `json:"min"`
			Max int `json:"max"`
		} `json:"prompt_tokens"`
		MaxParallel int  `json:"max_parallel"`
		BuiltIn     bool `json:"built_in"`
	}
	items := make([]bucketJSON, 0, len(perf.Buckets))
	for _, b := range perf.Buckets {
		var item bucketJSON
		item.Label = b.Label
		item.PromptTokens.Min, item.PromptTokens.Max = b.Min, b.Max
		item.MaxParallel = b.MaxParallel
		item.BuiltIn = b.DatasetBuiltIn == nil || *b.DatasetBuiltIn
		items = append(items, item)
	}
	var scenarios []map[string]any
	for _, sc := range perf.Scenarios {
		scenarios = append(scenarios, map[string]any{
			"label": sc.Label, "bucket": sc.Bucket, "note": sc.Note,
		})
	}
	if scenarios == nil {
		scenarios = []map[string]any{
			{"label": "9k_cold", "bucket": "9k", "note": "前缀缓存冷启动"},
			{"label": "9k_hot", "bucket": "9k", "note": "前缀缓存热命中"},
			{"label": "9k_mix50", "bucket": "9k", "note": "50% 前缀缓存混合"},
		}
	}
	var ladder []int
	var nbp map[string]int
	if perf.Matrix != nil {
		ladder = perf.Matrix.DefaultLadder
	}
	nbp = map[string]int{}
	for k, v := range perf.NumberByParallel {
		nbp[itoa(k)] = v
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":              items,
		"scenarios":          scenarios,
		"default_ladder":     ladder,
		"number_by_parallel": nbp,
	})
}

func (s *Server) handleCatalogDatasets(w http.ResponseWriter, r *http.Request) {
	var items []map[string]any
	for _, eng := range s.cfg.Engines {
		if eng == nil {
			continue
		}
		ev := eng.Capabilities["eval"]
		if ev == nil {
			continue
		}
		for _, d := range ev.Datasets {
			items = append(items, map[string]any{
				"name": d.Name, "display_name": orDefault(d.DisplayName, d.Name),
				"kind": orDefault(d.Kind, "text"), "needs_judge": d.NeedsJudge,
				"default_limit": d.DefaultLimit, "excluded_from_all": d.ExcludedFromAll,
			})
		}
		break
	}
	if items == nil {
		items = []map[string]any{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handleCatalogGates(w http.ResponseWriter, r *http.Request) {
	names := make([]string, 0, len(s.cfg.Gates.Profiles))
	for n := range s.cfg.Gates.Profiles {
		names = append(names, n)
	}
	sort.Strings(names)
	items := make([]map[string]any, 0, len(names))
	for _, n := range names {
		p := s.cfg.Gates.Profiles[n]
		if p == nil {
			continue
		}
		tiers := make([]map[string]any, 0, len(p.Tiers))
		for _, t := range p.Tiers {
			tiers = append(tiers, map[string]any{
				"tier":             t.Tier,
				"prompt_tokens":    map[string]any{"lo": t.Lo, "hi": t.Hi},
				"ttft_p50_max_sec": t.TTFTp50Max,
				"ttft_p90_max_sec": t.TTFTp90Max,
			})
		}
		items = append(items, map[string]any{
			"profile": n, "capability": orDefault(p.Capability, "perf"),
			"description": p.Description, "tiers": tiers,
			"global": map[string]any{
				"min_output_tokens_per_sec": p.Global.MinOutputTokensPerSec,
				"min_success_rate":          p.Global.MinSuccessRate,
			},
			"params":  p.DefaultParams,
			"default": n == s.cfg.Gates.DefaultProfile,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// handleMetrics 输出极简 Prometheus 文本（够用即止）。
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	uptime := time.Since(s.started).Seconds()
	body := "# HELP thvv_uptime_seconds 服务运行时长\n" +
		"# TYPE thvv_uptime_seconds gauge\n" +
		"thvv_uptime_seconds " + strconv.FormatFloat(uptime, 'f', 0, 64) + "\n" +
		"# HELP thvv_build_info 构建信息\n" +
		"# TYPE thvv_build_info gauge\n" +
		"thvv_build_info{version=\"" + s.version + "\",commit=\"" + s.commit + "\"} 1\n"
	_, _ = w.Write([]byte(body))
}
