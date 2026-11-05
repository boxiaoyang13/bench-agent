package config

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"time"
)

func boolPtr(b bool) *bool { return &b }
func intPtr(i int) *int    { return &i }

// applyDefaults 填充内置默认值（= 现有 Python 工具链的行为，见 docs/CONFIG.md §0）。
func (c *Config) applyDefaults() {
	if c.Version == 0 {
		c.Version = 1
	}

	// server
	d(&c.Server.Listen, "127.0.0.1:8080")
	d(&c.Server.BasePath, "/v1")
	c.Server.BasePath = trimSlash(c.Server.BasePath)
	dz(&c.Server.ReadHeaderTimeout, 10*time.Second)
	dz(&c.Server.IdleTimeout, 120*time.Second)
	dz(&c.Server.ShutdownTimeout, 30*time.Second)
	d(&c.Server.UI.Path, "/ui/")
	c.Server.UI.RedirectRoot = true

	// roots
	d(&c.Roots.Data, "./data")
	d(&c.Roots.Bin, "thvv:.venv/bin")

	// auth
	d(&c.Auth.Mode, "both")
	d(&c.Auth.Session.CookieName, "thvv_session")
	dc(&c.Auth.Session.TTL, 12*time.Hour)
	d(&c.Auth.Session.SameSite, "lax")
	d(&c.Auth.Session.SigningKey, "dev-only-change-me")

	// store
	d(&c.Store.Driver, "sqlite")
	dz(&c.Store.SQLite.BusyTimeout, 5*time.Second)
	if c.Store.SQLite.MaxOpenConns == 0 {
		c.Store.SQLite.MaxOpenConns = 4
	}
	if c.Store.SQLite.Path == "" {
		c.Store.SQLite.Path = "data:thvv.db"
	}

	// paths
	dp(&c.Paths.RunsDir, "data:runs")
	dp(&c.Paths.LogsDir, "data:logs")
	dp(&c.Paths.CacheDir, "data:cache")
	dp(&c.Paths.TemplatesDir, "cfg:templates")

	// limits
	if c.Limits.MaxConcurrentRuns == 0 {
		c.Limits.MaxConcurrentRuns = 2
	}
	if c.Limits.MaxQueuedRuns == 0 {
		c.Limits.MaxQueuedRuns = 16
	}
	if c.Limits.MaxParallelPerTarget == 0 {
		c.Limits.MaxParallelPerTarget = 1
	}
	dz(&c.Limits.MaxRunDuration, 12*time.Hour)
	dz(&c.Limits.RunRetention, 720*time.Hour)
	if c.Limits.ArtifactMaxBytes == 0 {
		c.Limits.ArtifactMaxBytes = 200 << 20
	}
	dz(&c.Limits.API.RequestTimeout, 30*time.Second)
	if c.Limits.API.MaxBodyBytes == 0 {
		c.Limits.API.MaxBodyBytes = 1 << 20
	}
	if c.Limits.Plan.MaxNumber == 0 {
		c.Limits.Plan.MaxNumber = 10000
	}
	if c.Limits.Plan.MaxParallel == 0 {
		c.Limits.Plan.MaxParallel = 256
	}
	if c.Limits.Plan.MaxBuckets == 0 {
		c.Limits.Plan.MaxBuckets = 9
	}
	if c.Limits.Plan.MaxTPMBudget == 0 {
		c.Limits.Plan.MaxTPMBudget = 100_000_000
	}

	// reports
	if len(c.Reports.DefaultFormats) == 0 {
		c.Reports.DefaultFormats = []string{"html", "xlsx"}
	}
	d(&c.Reports.Title.Single, "{model} 性能压测报告 ({bucket})")
	d(&c.Reports.Title.Matrix, "{model} 性能压测报告 (全档位 {ts})")
	d(&c.Reports.XLSX.SheetMetrics, "性能指标")
	d(&c.Reports.XLSX.SheetFailures, "失败请求详情")
	if c.Reports.FailureExcerptChars == 0 {
		c.Reports.FailureExcerptChars = 1200
	}

	// security / observability / webhooks
	if c.Security.MaxTargets == 0 {
		c.Security.MaxTargets = 200
	}
	c.Security.RedactLogs = true
	d(&c.Observability.LogLevel, "info")
	d(&c.Observability.LogFormat, "text")
	dz(&c.Observability.SlowRequestThreshold, 2*time.Second)
	d(&c.Observability.Metrics.Path, "/metrics")
	d(&c.Observability.Pprof.Path, "/debug/pprof")
	dz(&c.Webhooks.Timeout, 10*time.Second)
	if len(c.Webhooks.RetryBackoff) == 0 {
		c.Webhooks.RetryBackoff = []time.Duration{5 * time.Second, 25 * time.Second, 2 * time.Minute, 10 * time.Minute, 30 * time.Minute}
	}
	d(&c.Webhooks.SignatureHeader, "X-THVV-Signature")

	// gates: 内置 perf-sla-v1
	if c.Gates.Profiles == nil {
		c.Gates.Profiles = map[string]*GateProfile{}
	}
	if _, ok := c.Gates.Profiles["perf-sla-v1"]; !ok {
		c.Gates.Profiles["perf-sla-v1"] = defaultSLAProfile()
	}
	d(&c.Gates.DefaultProfile, "perf-sla-v1")

	// engines: 每个 capability 的默认值
	for _, eng := range c.Engines {
		if eng == nil {
			continue
		}
		d(&eng.Kind, "subprocess")
		if eng.Capabilities == nil {
			eng.Capabilities = map[string]*Capability{}
		}
		if perf := eng.Capabilities["perf"]; perf != nil {
			applyPerfDefaults(perf)
		}
		for _, cap := range eng.Capabilities {
			if cap == nil {
				continue
			}
			if cap.Enabled && cap.Status == "" {
				cap.Status = "ga"
			}
			if !cap.Enabled && cap.Status == "" {
				cap.Status = "unavailable"
			}
		}
	}
}

func applyPerfDefaults(p *Capability) {
	if p.Defaults == nil {
		p.Defaults = &PerfDefaults{}
	}
	if p.Defaults.MaxTokens == 0 {
		p.Defaults.MaxTokens = 4096
	}
	if p.Defaults.MaxTurns == 0 {
		p.Defaults.MaxTurns = 1
	}
	dz(&p.Defaults.ConnectTimeout, 60*time.Second)
	dz(&p.Defaults.ReadTimeout, 600*time.Second)
	if len(p.Defaults.TemperatureMap) == 0 {
		p.Defaults.TemperatureMap = []MatchRule{
			{Match: "(?i)kimi|moonshot", Value: 1.0},
			{Match: "(?i).*", Value: 0.0},
		}
	}
	if len(p.Defaults.TokenizerMap) == 0 {
		p.Defaults.TokenizerMap = []MatchRule{
			{Match: "(?i)glm|chatglm", Tokenizer: "zai-org/GLM-4.6"},
			{Match: "(?i)deepseek", Tokenizer: "deepseek-ai/DeepSeek-V3"},
			{Match: "(?i)minimax", Tokenizer: "MiniMaxAI/MiniMax-M2"},
			{Match: "(?i)kimi|moonshot", Tokenizer: "moonshotai/Kimi-K2-Thinking"},
			{Match: "(?i)qwen|tongyi", Tokenizer: "Qwen/Qwen3-235B-A22B"},
			{Match: "(?i)llama", Tokenizer: "meta-llama/Llama-3.1-8B-Instruct"},
			{Match: "(?i)mistral", Tokenizer: "mistralai/Mistral-7B-Instruct-v0.3"},
			{Match: "(?i)baichuan", Tokenizer: "baichuan-inc/Baichuan2-13B-Chat"},
			{Match: "(?i)yi[-_]|yi-l", Tokenizer: "01-ai/Yi-1.5-34B-Chat"},
			{Match: "(?i)gemma|gemini", Tokenizer: "google/gemma-2-2b"},
			{Match: "(?i)claude|anthropic", Tokenizer: "Xenova/gpt-4o"},
			{Match: "(?i)gpt|openai", Tokenizer: "Xenova/gpt-4o"},
			{Match: "(?i)phi[-_]?3", Tokenizer: "microsoft/Phi-3-mini-4k-instruct"},
		}
	}
	if len(p.Buckets) == 0 {
		p.Buckets = defaultBuckets()
	}
	if len(p.NumberByParallel) == 0 {
		p.NumberByParallel = map[int]int{1: 20, 4: 40, 8: 60, 16: 80, 32: 120, 64: 200, 128: 400}
	}
	if len(p.ExtraParallels) == 0 {
		p.ExtraParallels = map[string][]int{"200k": {16}}
	}
	if p.DefaultNumber == nil {
		p.DefaultNumber = intPtr(100)
	}
	if p.Matrix == nil {
		p.Matrix = &MatrixCfg{}
	}
	if len(p.Matrix.Buckets) == 0 {
		p.Matrix.Buckets = []string{"1k", "9k", "16k", "32k", "64k", "128k", "200k"}
	}
	if len(p.Matrix.DefaultLadder) == 0 {
		p.Matrix.DefaultLadder = []int{1, 8, 32, 64, 128}
	}
	dz(&p.Matrix.Cooldown, 120*time.Second)
	if p.Matrix.StopOnFailure == nil {
		p.Matrix.StopOnFailure = boolPtr(true)
	}
	if len(p.LongBuckets) == 0 {
		p.LongBuckets = []string{"512k", "1m"}
	}
	d(&p.DatasetTokenizer, "zai-org/GLM-4.6")
	// 编译正则
	for i := range p.Defaults.TemperatureMap {
		compileRule(&p.Defaults.TemperatureMap[i])
	}
	for i := range p.Defaults.TokenizerMap {
		compileRule(&p.Defaults.TokenizerMap[i])
	}
}

func compileRule(r *MatchRule) {
	if r.re == nil && r.Match != "" {
		re, err := reCompile(r.Match)
		if err == nil {
			r.re = re
		}
	}
}

// Match 判断模型名是否命中该规则。
func (r *MatchRule) Hits(model string) bool {
	if r.re == nil {
		compileRule(r)
	}
	if r.re == nil {
		return false
	}
	return r.re.MatchString(model)
}

func defaultBuckets() []BucketCfg {
	return []BucketCfg{
		{Label: "1k", Min: 870, Max: 1228, MaxParallel: 128},
		{Label: "9k", Min: 8000, Max: 11000, MaxParallel: 128},
		{Label: "16k", Min: 14000, Max: 18000, MaxParallel: 128},
		{Label: "32k", Min: 28000, Max: 36000, MaxParallel: 128},
		{Label: "64k", Min: 56000, Max: 72000, MaxParallel: 64},
		{Label: "128k", Min: 112000, Max: 144000, MaxParallel: 32},
		{Label: "200k", Min: 180000, Max: 220000, MaxParallel: 16},
		{Label: "512k", Min: 500000, Max: 540000, MaxParallel: 8, DatasetBuiltIn: boolPtr(false)},
		{Label: "1m", Min: 980000, Max: 1030000, MaxParallel: 8, DatasetBuiltIn: boolPtr(false)},
	}
}

func defaultSLAProfile() *GateProfile {
	return &GateProfile{
		Capability:  "perf",
		Description: "按平均输入长度分档的 TTFT P50/P90 阈值 + 吞吐下限",
		Tiers: []GateTier{
			{Tier: "<4K", Lo: 1000, Hi: 4000, TTFTp50Max: 2.0, TTFTp90Max: 5.0},
			{Tier: "<8K", Lo: 4000, Hi: 8000, TTFTp50Max: 2.5, TTFTp90Max: 5.0},
			{Tier: "<32K", Lo: 8000, Hi: 32000, TTFTp50Max: 4.0, TTFTp90Max: 8.0},
			{Tier: "<64K", Lo: 32000, Hi: 64000, TTFTp50Max: 8.0, TTFTp90Max: 15.0},
			{Tier: "<128K", Lo: 64000, Hi: 128000, TTFTp50Max: 15.0, TTFTp90Max: 35.0},
			{Tier: "<256K", Lo: 128000, Hi: 256000, TTFTp50Max: 30.0, TTFTp90Max: 70.0},
		},
		Global:        GateGlobal{MinOutputTokensPerSec: 30, MinSuccessRate: 0.99},
		DefaultParams: map[string]any{"user_tpm_quota": 5000000},
	}
}

// ---------- 小工具 ----------

func d(dst *string, def string) {
	if *dst == "" {
		*dst = def
	}
}

func dp(dst *Path, def string) {
	if *dst == "" {
		*dst = Path(def)
	}
}

func dz(dst *time.Duration, def time.Duration) {
	if *dst == 0 && !durationExplicitZero(dst) {
		*dst = def
	}
}

// durationExplicitZero 目前 YAML 无法区分「未写」与「写了 0」，
// 但对 0 有语义的字段（read/write timeout）在 validate 里单独处理。
func durationExplicitZero(*time.Duration) bool { return false }

func dc(dst *time.Duration, def time.Duration) {
	if *dst == 0 {
		*dst = def
	}
}

func trimSlash(s string) string {
	for len(s) > 1 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	if s != "" && s[0] != '/' {
		s = "/" + s
	}
	return s
}

func decodeKey(raw string) ([]byte, error) {
	if b, err := base64.StdEncoding.DecodeString(raw); err == nil && len(b) == 32 {
		return b, nil
	}
	if b, err := hex.DecodeString(raw); err == nil && len(b) == 32 {
		return b, nil
	}
	// 也接受任意长度口令，用 SHA-256 派生（本地开发方便）
	if len(raw) >= 16 {
		return deriveKey(raw), nil
	}
	return nil, fmt.Errorf("必须解码为 32 字节（base64 或 64 位 hex），或至少 16 字符的口令")
}
