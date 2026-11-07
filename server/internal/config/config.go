// Package config 加载并校验 THVV Server 的 YAML 配置。
//
// 解析顺序（后者覆盖前者）：
//
//	内置默认值 → YAML → ${ENV} 展开 → 环境变量 THVV_* → 命令行
//
// 之后依次：解析 roots → 解析所有 Path（前缀寻址）→ 指纹校验 → 语义校验。
// 详见 docs/CONFIG.md。
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config 是完整配置树。
type Config struct {
	Version       int                `yaml:"version"`
	Roots         Roots              `yaml:"roots"`
	Server        Server             `yaml:"server"`
	Auth          Auth               `yaml:"auth"`
	Store         Store              `yaml:"store"`
	Secrets       Secrets            `yaml:"secrets"`
	Paths         Paths              `yaml:"paths"`
	Limits        Limits             `yaml:"limits"`
	Engines       map[string]*Engine `yaml:"engines"`
	Reports       Reports            `yaml:"reports"`
	Gates         Gates              `yaml:"gates"`
	Webhooks      Webhooks           `yaml:"webhooks"`
	Security      Security           `yaml:"security"`
	Observability Observability      `yaml:"observability"`

	// 解析期填充，不从 YAML 读
	ConfigDir string `yaml:"-"` // 配置文件所在目录（绝对）
	// Resolved 是解析后的绝对路径表（字段路径 → 绝对路径），供 -check 与启动日志使用
	Resolved map[string]string `yaml:"-"`
}

// ---------- roots ----------

type Roots struct {
	Thvv string            `yaml:"thvv"`
	Data string            `yaml:"data"`
	Bin  string            `yaml:"bin"`
	Ext  map[string]string `yaml:",inline"`
}

// ---------- server ----------

type Server struct {
	Listen            string        `yaml:"listen"`
	BasePath          string        `yaml:"base_path"`
	ReadHeaderTimeout time.Duration `yaml:"read_header_timeout"`
	ReadTimeout       time.Duration `yaml:"read_timeout"`
	WriteTimeout      time.Duration `yaml:"write_timeout"`
	IdleTimeout       time.Duration `yaml:"idle_timeout"`
	ShutdownTimeout   time.Duration `yaml:"shutdown_timeout"`
	TLS               TLS           `yaml:"tls"`
	CORS              CORS          `yaml:"cors"`
	UI                UI            `yaml:"ui"`
	TrustProxyHeaders bool          `yaml:"trust_proxy_headers"`
	PublicBaseURL     string        `yaml:"public_base_url"`
}

type TLS struct {
	Enabled  bool   `yaml:"enabled"`
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

type CORS struct {
	Enabled        bool          `yaml:"enabled"`
	AllowedOrigins []string      `yaml:"allowed_origins"`
	AllowedHeaders []string      `yaml:"allowed_headers"`
	MaxAge         time.Duration `yaml:"max_age"`
}

type UI struct {
	Enabled      bool   `yaml:"enabled"`
	Path         string `yaml:"path"`
	Dir          string `yaml:"dir"`
	RedirectRoot bool   `yaml:"redirect_root"`
}

// ---------- auth ----------

type Auth struct {
	Mode    string     `yaml:"mode"` // bearer | cookie | both
	Session Session    `yaml:"session"`
	Tokens  []APIToken `yaml:"tokens"`
	Users   []User     `yaml:"users"`
}

type Session struct {
	CookieName string        `yaml:"cookie_name"`
	TTL        time.Duration `yaml:"ttl"`
	Secure     bool          `yaml:"secure"`
	SameSite   string        `yaml:"same_site"`
	SigningKey string        `yaml:"signing_key"`
}

type APIToken struct {
	Name  string `yaml:"name"`
	Token string `yaml:"token"`
	Role  string `yaml:"role"`
}

type User struct {
	Username     string `yaml:"username"`
	PasswordHash string `yaml:"password_hash"`
	Role         string `yaml:"role"`
}

// ---------- store / secrets / paths ----------

type Store struct {
	Driver string `yaml:"driver"`
	SQLite struct {
		Path         Path          `yaml:"path"`
		WAL          bool          `yaml:"wal"`
		BusyTimeout  time.Duration `yaml:"busy_timeout"`
		MaxOpenConns int           `yaml:"max_open_conns"`
	} `yaml:"sqlite"`
}

type Secrets struct {
	Provider string `yaml:"provider"` // env | file
	Env      struct {
		Key string `yaml:"key"`
	} `yaml:"env"`
	File struct {
		Path Path `yaml:"path"`
	} `yaml:"file"`

	// 解析后（不序列化）：32 字节主密钥
	MasterKey []byte `yaml:"-"`
}

type Paths struct {
	RunsDir      Path `yaml:"runs_dir"`
	LogsDir      Path `yaml:"logs_dir"`
	DatasetsDir  Path `yaml:"datasets_dir"`
	CacheDir     Path `yaml:"cache_dir"`
	TemplatesDir Path `yaml:"templates_dir"`
}

// ---------- limits ----------

type Limits struct {
	MaxConcurrentRuns    int           `yaml:"max_concurrent_runs"`
	MaxQueuedRuns        int           `yaml:"max_queued_runs"`
	MaxParallelPerTarget int           `yaml:"max_parallel_runs_per_target"`
	MaxRunDuration       time.Duration `yaml:"max_run_duration"`
	RunRetention         time.Duration `yaml:"run_retention"`
	ArtifactMaxBytes     int64         `yaml:"artifact_max_bytes"`
	API                  struct {
		RequestTimeout time.Duration `yaml:"request_timeout"`
		MaxBodyBytes   int64         `yaml:"max_body_bytes"`
	} `yaml:"api"`
	Plan struct {
		MaxNumber    int   `yaml:"max_number"`
		MaxParallel  int   `yaml:"max_parallel"`
		MaxBuckets   int   `yaml:"max_buckets_per_run"`
		MaxTPMBudget int64 `yaml:"max_tpm_budget"`
	} `yaml:"plan"`
}

// ---------- engines ----------

type Engine struct {
	Enabled      bool                   `yaml:"enabled"`
	Kind         string                 `yaml:"kind"` // subprocess | builtin | http
	DisplayName  string                 `yaml:"display_name"`
	Version      string                 `yaml:"version"`
	Runtime      *Runtime               `yaml:"runtime"`
	BaseURL      string                 `yaml:"base_url"`
	Timeout      time.Duration          `yaml:"timeout"`
	Capabilities map[string]*Capability `yaml:"capabilities"`
}

type Runtime struct {
	Home              Path              `yaml:"home"`
	Python            Path              `yaml:"python"`
	Evalscope         Path              `yaml:"evalscope"`
	PythonPathPrepend []Path            `yaml:"pythonpath_prepend"`
	Env               map[string]string `yaml:"env"`
	CheckOnStartup    bool              `yaml:"check_on_startup"`
	CheckTimeout      time.Duration     `yaml:"check_timeout"`
}

type Capability struct {
	Enabled     bool     `yaml:"enabled"`
	Status      string   `yaml:"status"` // ga | preview | planned | unavailable
	AvailableAt string   `yaml:"available_at"`
	Description string   `yaml:"description"`
	WorkingDir  Path     `yaml:"working_dir"`
	Script      Path     `yaml:"script"`
	Runtime     *Runtime `yaml:"runtime"` // 覆盖引擎级 runtime

	// perf
	Defaults         *PerfDefaults    `yaml:"defaults"`
	Buckets          []BucketCfg      `yaml:"buckets"`
	NumberByParallel map[int]int      `yaml:"number_by_parallel"`
	ExtraParallels   map[string][]int `yaml:"extra_parallels"`
	DefaultNumber    *int             `yaml:"default_number"`
	Matrix           *MatrixCfg       `yaml:"matrix"`
	LongBuckets      []string         `yaml:"long_buckets"`
	Scenarios        []ScenarioCfg    `yaml:"scenarios"`
	DatasetTokenizer string           `yaml:"dataset_tokenizer"`

	// eval
	Datasets []Dataset `yaml:"datasets"`
	Judge    *Judge    `yaml:"judge"`

	// cache（lmcache bench engine）；键名不能与 perf 的 defaults 重复
	CacheDefaults *CacheDefaults `yaml:"cache_defaults"`

	// http
	PlanSchema   string `yaml:"plan_schema"`
	ResultSchema string `yaml:"result_schema"`
}

type PerfDefaults struct {
	MaxTokens        int           `yaml:"max_tokens"`
	MaxTurns         int           `yaml:"max_turns"`
	ConnectTimeout   time.Duration `yaml:"connect_timeout"`
	ReadTimeout      time.Duration `yaml:"read_timeout"`
	DBCommitInterval int           `yaml:"db_commit_interval"`
	TemperatureMap   []MatchRule   `yaml:"temperature_map"`
	TokenizerMap     []MatchRule   `yaml:"tokenizer_map"`
}

type MatchRule struct {
	Match     string  `yaml:"match"`
	Value     float64 `yaml:"value"`
	Tokenizer string  `yaml:"tokenizer"`

	re *regexp.Regexp
}

type BucketCfg struct {
	Label          string `yaml:"label"`
	Min            int    `yaml:"min"`
	Max            int    `yaml:"max"`
	MaxParallel    int    `yaml:"max_parallel"`
	DatasetBuiltIn *bool  `yaml:"dataset_built_in"`
}

type MatrixCfg struct {
	Buckets        []string      `yaml:"buckets"`
	DefaultLadder  []int         `yaml:"default_ladder"`
	Cooldown       time.Duration `yaml:"cooldown"`
	SuccessRateMin float64       `yaml:"success_rate_min"`
	TPMBudget      int64         `yaml:"tpm_budget"`
	StopOnFailure  *bool         `yaml:"stop_on_failure"`
}

// ScenarioCfg 是前缀缓存等特殊场景数据集。
type ScenarioCfg struct {
	Label  string `yaml:"label"`
	Bucket string `yaml:"bucket"`
	File   string `yaml:"file"`
	Note   string `yaml:"note"`
}

type Dataset struct {
	Name             string `yaml:"name"`
	DisplayName      string `yaml:"display_name"`
	Kind             string `yaml:"kind"` // text | agent | video | image
	NeedsJudge       bool   `yaml:"needs_judge"`
	NeedsDocker      bool   `yaml:"needs_docker"`
	DefaultLimit     *int   `yaml:"default_limit"`
	DefaultRepeats   *int   `yaml:"default_repeats"`
	DefaultEvalBatch *int   `yaml:"default_eval_batch_size"`
	ExcludedFromAll  bool   `yaml:"excluded_from_all"`
}

type Judge struct {
	Model   string `yaml:"model"`
	BaseURL string `yaml:"base_url"`
	APIKey  string `yaml:"api_key"`
}

// CacheDefaults 是 lmcache bench multi-round-chat 的默认参数。
type CacheDefaults struct {
	Workload        string  `yaml:"workload"`
	KVCacheVolumeGB float64 `yaml:"kv_cache_volume_gb"`
	TokensPerGBKV   int     `yaml:"tokens_per_gb_kvcache"`
	QPS             float64 `yaml:"qps"`
	DurationSec     int     `yaml:"duration_sec"`
	Seed            int     `yaml:"seed"`
	SharedPromptLen int     `yaml:"shared_prompt_length"`
	ChatHistoryLen  int     `yaml:"chat_history_length"`
	UserInputLen    int     `yaml:"user_input_length"`
	OutputLen       int     `yaml:"output_length"`
}

// ---------- reports / gates ----------

type Reports struct {
	DefaultFormats []string `yaml:"default_formats"`
	DefaultCompany string   `yaml:"default_company"`
	Title          struct {
		Single string `yaml:"single"`
		Matrix string `yaml:"matrix"`
	} `yaml:"title"`
	XLSX struct {
		SheetMetrics  string `yaml:"sheet_metrics"`
		SheetFailures string `yaml:"sheet_failures"`
	} `yaml:"xlsx"`
	FailureExcerptChars int `yaml:"failure_excerpt_chars"`
	HTML                struct {
		InlineAssets bool   `yaml:"inline_assets"`
		TemplateDir  string `yaml:"template_dir"`
	} `yaml:"html"`
}

type Gates struct {
	DefaultProfile string                  `yaml:"default_profile"`
	Profiles       map[string]*GateProfile `yaml:"profiles"`
}

type GateProfile struct {
	Capability    string         `yaml:"capability"`
	Description   string         `yaml:"description"`
	Tiers         []GateTier     `yaml:"tiers"`
	Global        GateGlobal     `yaml:"global"`
	DefaultParams map[string]any `yaml:"default_params"`

	// eval：按数据集的分数下限（0~1）。
	// 与《效果验收标准.xlsx》对齐：基线可按 target.model 配置覆盖，未命中用 default。
	EvalTiers []EvalGateTier `yaml:"eval_tiers,omitempty"`

	// cache：multi-round-chat 前缀缓存验收（cache-sla-v1）。
	CacheTiers []CacheGateTier `yaml:"cache_tiers,omitempty"`
}

type CacheGateTier struct {
	SteadyTTFTp50MaxMS float64 `yaml:"steady_ttft_p50_max_ms"` // 稳态 TTFT P50 上限
	TTFTImprovementMin float64 `yaml:"ttft_improvement_min"`   // 稳态相对首轮的最小改善率（0~1）
	SuccessRateMin     float64 `yaml:"success_rate_min"`       // 成功率下限（0~1）
	PrefixHitRateMin   float64 `yaml:"prefix_hit_rate_min"`    // 前缀命中率下限（0~1；cached/prompt）
}

type EvalGateTier struct {
	Dataset    string             `yaml:"dataset"`               // 数据集名（aime26 / hle / ...）
	Subitem    string             `yaml:"subitem,omitempty"`     // 子项（tau2_bench 的领域、longbench 子集等），空 = 整体
	MinScore   float64            `yaml:"min_score"`             // 分数下限（0~1）
	BaselineBy map[string]float64 `yaml:"baseline_by,omitempty"` // 可选：模型名（小写包含匹配）→ 基线，未命中用 MinScore
}

type GateTier struct {
	Tier       string  `yaml:"tier"`
	Lo         int     `yaml:"lo"`
	Hi         int     `yaml:"hi"`
	TTFTp50Max float64 `yaml:"ttft_p50_max_sec"`
	TTFTp90Max float64 `yaml:"ttft_p90_max_sec"`
}

type GateGlobal struct {
	MinOutputTokensPerSec float64 `yaml:"min_output_tokens_per_sec"`
	MinSuccessRate        float64 `yaml:"min_success_rate"`
}

// ---------- webhooks / security / observability ----------

type Webhooks struct {
	Enabled         bool            `yaml:"enabled"`
	Timeout         time.Duration   `yaml:"timeout"`
	RetryBackoff    []time.Duration `yaml:"retry_backoff"`
	AllowedHosts    []string        `yaml:"allowed_hosts"`
	SignatureHeader string          `yaml:"signature_header"`
}

type Security struct {
	AllowPrivateTargets bool     `yaml:"allow_private_targets"`
	TargetHostDenylist  []string `yaml:"target_host_denylist"`
	TargetHostAllowlist []string `yaml:"target_host_allowlist"`
	MaxTargets          int      `yaml:"max_targets"`
	RedactLogs          bool     `yaml:"redact_logs"`
	ExposeEngineCmdline bool     `yaml:"expose_engine_cmdline"`
}

type Observability struct {
	LogLevel             string        `yaml:"log_level"`
	LogFormat            string        `yaml:"log_format"`
	AccessLog            bool          `yaml:"access_log"`
	SlowRequestThreshold time.Duration `yaml:"slow_request_threshold"`
	Metrics              struct {
		Enabled bool   `yaml:"enabled"`
		Path    string `yaml:"path"`
	} `yaml:"metrics"`
	Pprof struct {
		Enabled bool   `yaml:"enabled"`
		Path    string `yaml:"path"`
	} `yaml:"pprof"`
}

// ---------- 加载 ----------

// LoadOptions 是命令行传入的覆盖项。
type LoadOptions struct {
	Path     string
	ThvvHome string // -thvv-home
	Listen   string // -listen
	LogLevel string // -log-level
	Env      []string
}

// Load 读取、展开、解析、校验配置。
func Load(opts LoadOptions) (*Config, error) {
	abs, err := filepath.Abs(opts.Path)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件失败: %w", err)
	}

	env := envMap(opts.Env)
	expanded := expandEnvInYAML(string(raw), env)

	var cfg Config
	dec := yaml.NewDecoder(strings.NewReader(expanded))
	dec.KnownFields(false)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("解析 YAML 失败: %w", err)
	}
	cfg.ConfigDir = filepath.Dir(abs)

	if err := applyEnvOverrides(&cfg, env); err != nil {
		return nil, err
	}
	// 命令行覆盖
	if opts.ThvvHome != "" {
		cfg.Roots.Thvv = opts.ThvvHome
	}
	if opts.Listen != "" {
		cfg.Server.Listen = opts.Listen
	}
	if opts.LogLevel != "" {
		cfg.Observability.LogLevel = opts.LogLevel
	}

	cfg.applyDefaults()

	if err := cfg.resolvePaths(); err != nil {
		return nil, err
	}
	if err := cfg.resolveSecrets(); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func envMap(list []string) map[string]string {
	m := map[string]string{}
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i > 0 {
			m[kv[:i]] = kv[i+1:]
		}
	}
	for _, kv := range list {
		if i := strings.IndexByte(kv, '='); i > 0 {
			m[kv[:i]] = kv[i+1:]
		}
	}
	return m
}

var envRefRe = regexp.MustCompile(`\$\$|\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-([^}]*))?\}`)

// expandEnvInYAML 展开 ${VAR} / ${VAR:-default}，只做一轮；$$ 转义为 $。
// 未设置且无默认值的变量保持原样，留给 Validate 报出具体字段。
func expandEnvInYAML(s string, env map[string]string) string {
	return envRefRe.ReplaceAllStringFunc(s, func(m string) string {
		if m == "$$" {
			return "$"
		}
		sub := envRefRe.FindStringSubmatch(m)
		name, def := sub[1], sub[2]
		if v, ok := env[name]; ok && v != "" {
			return v
		}
		if sub[2] != "" || strings.Contains(m, ":-") {
			return def
		}
		return m
	})
}

// applyEnvOverrides 支持 THVV_SECTION__FIELD 覆盖。
func applyEnvOverrides(cfg *Config, env map[string]string) error {
	for k, v := range env {
		if !strings.HasPrefix(k, "THVV_") {
			continue
		}
		path := strings.TrimPrefix(k, "THVV_")
		if !strings.Contains(path, "__") {
			continue // 简单变量（THVV_HOME 等）由 YAML 里的 ${} 消费
		}
		if err := cfg.setByPath(strings.Split(strings.ToLower(path), "__"), v); err != nil {
			return fmt.Errorf("环境变量 %s: %w", k, err)
		}
	}
	return nil
}

// setByPath 只覆盖标量字段，够用且不易出错。
func (c *Config) setByPath(parts []string, val string) error {
	setStr := func(dst *string) error { *dst = val; return nil }
	setInt := func(dst *int) error {
		n, err := parseInt(val)
		if err != nil {
			return err
		}
		*dst = n
		return nil
	}
	setBool := func(dst *bool) error {
		b, err := parseBool(val)
		if err != nil {
			return err
		}
		*dst = b
		return nil
	}
	setDur := func(dst *time.Duration) error {
		d, err := parseDuration(val)
		if err != nil {
			return err
		}
		*dst = d
		return nil
	}

	switch parts[0] {
	case "server":
		if len(parts) < 2 {
			return fmt.Errorf("路径不完整")
		}
		switch parts[1] {
		case "listen":
			return setStr(&c.Server.Listen)
		case "base_path":
			return setStr(&c.Server.BasePath)
		case "write_timeout":
			return setDur(&c.Server.WriteTimeout)
		}
	case "roots":
		if len(parts) < 2 {
			return fmt.Errorf("路径不完整")
		}
		if c.Roots.Ext == nil {
			c.Roots.Ext = map[string]string{}
		}
		if parts[1] == "thvv" {
			return setStr(&c.Roots.Thvv)
		}
		if parts[1] == "data" {
			return setStr(&c.Roots.Data)
		}
		if parts[1] == "bin" {
			return setStr(&c.Roots.Bin)
		}
		c.Roots.Ext[parts[1]] = val
		return nil
	case "limits":
		if len(parts) < 2 {
			return fmt.Errorf("路径不完整")
		}
		switch parts[1] {
		case "max_concurrent_runs":
			return setInt(&c.Limits.MaxConcurrentRuns)
		case "max_queued_runs":
			return setInt(&c.Limits.MaxQueuedRuns)
		case "max_parallel_runs_per_target":
			return setInt(&c.Limits.MaxParallelPerTarget)
		case "max_run_duration":
			return setDur(&c.Limits.MaxRunDuration)
		}
	case "observability":
		if len(parts) >= 2 && parts[1] == "log_level" {
			return setStr(&c.Observability.LogLevel)
		}
		if len(parts) >= 2 && parts[1] == "log_format" {
			return setStr(&c.Observability.LogFormat)
		}
		if len(parts) >= 2 && parts[1] == "access_log" {
			return setBool(&c.Observability.AccessLog)
		}
	}
	// 其余路径（引擎深层次字段）走 YAML 或 ${} 展开，不在环境变量里做
	return nil
}
