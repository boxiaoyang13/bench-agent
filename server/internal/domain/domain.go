// Package domain 定义 THVV 的领域模型。
//
// 这里的 computeSteps 与前端 web/app.js 的 computeSteps() **必须逐行等价**，
// 否则提交前后的 steps_total 会不一致（协议要求 202 时即可精确给出总步数）。
// 算法来源：thvv/perf/run.sh 的 cmd_bench_all。
package domain

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ---------------- 能力 ----------------

type Capability string

const (
	CapPerf       Capability = "perf"
	CapEval       Capability = "eval"
	CapCache      Capability = "cache"
	CapFunctional Capability = "functional"
)

func (c Capability) Valid() bool {
	return c == CapPerf || c == CapEval || c == CapCache || c == CapFunctional
}

// ---------------- Target ----------------

type Protocol string

const (
	ProtocolOpenAI    Protocol = "openai"
	ProtocolAnthropic Protocol = "anthropic"
)

type TargetDefaults struct {
	Temperature       any `json:"temperature,omitempty"` // number | "auto"
	MaxTokens         int `json:"max_tokens,omitempty"`
	ConnectTimeoutSec int `json:"connect_timeout_sec,omitempty"`
	ReadTimeoutSec    int `json:"read_timeout_sec,omitempty"`
}

type TokenizerInfo struct {
	Value  string `json:"value"`
	Source string `json:"source"` // explicit | auto_detected | fallback
	Cached bool   `json:"cached"`
}

type TargetHealth struct {
	Status    string     `json:"status"` // unknown | ok | warn | error
	CheckedAt *time.Time `json:"checked_at"`
}

type Target struct {
	ID         string            `json:"target_id"`
	Name       string            `json:"name"`
	Vendor     string            `json:"vendor,omitempty"`
	APIURL     string            `json:"api_url"`
	Protocol   Protocol          `json:"protocol"`
	Model      string            `json:"model"`
	APIKeyHint string            `json:"api_key_hint"`
	Tokenizer  *TokenizerInfo    `json:"tokenizer,omitempty"`
	Defaults   TargetDefaults    `json:"defaults"`
	Labels     map[string]string `json:"labels,omitempty"`
	Health     TargetHealth      `json:"health"`
	CreatedAt  time.Time         `json:"created_at"`
	UpdatedAt  time.Time         `json:"updated_at"`

	// 仅内部使用，不序列化
	APIKeyEnc []byte     `json:"-"`
	DeletedAt *time.Time `json:"-"`
}

// TargetSnapshot 是写入 Run 的不可变副本（不含凭据）。
type TargetSnapshot struct {
	TargetID  string   `json:"target_id"`
	Vendor    string   `json:"vendor,omitempty"`
	Model     string   `json:"model"`
	Protocol  Protocol `json:"protocol"`
	APIURL    string   `json:"api_url"`
	Ephemeral bool     `json:"ephemeral"`
}

func (t Target) Snapshot(ephemeral bool) TargetSnapshot {
	return TargetSnapshot{TargetID: t.ID, Vendor: t.Vendor, Model: t.Model, Protocol: t.Protocol, APIURL: t.APIURL, Ephemeral: ephemeral}
}

// ---------------- 档位与计划 ----------------

type Bucket struct {
	Label        string `json:"label"`
	PromptTokens struct {
		Min int `json:"min"`
		Max int `json:"max"`
	} `json:"prompt_tokens"`
	MaxParallel int  `json:"max_parallel"`
	BuiltIn     bool `json:"built_in"`
}

type BucketOverride struct {
	Parallels   []int `json:"parallels,omitempty"`
	MaxParallel int   `json:"max_parallel,omitempty"`
	Number      int   `json:"number,omitempty"`
}

// ---------------- cache（capability=cache） ----------------

// CacheTier 是并发档位（plan.ladder[]）。
//
// MRC 不直接接受并发数：session 数由 kv-cache-volume 与单会话 token 数推导，
// 因此规范化时会把 sessions 反算成 KVCacheVolumeGB 回填到这里（见 runmgr）。
type CacheTier struct {
	Sessions        int     `json:"sessions"`                     // 目标并发 session 数
	QPS             float64 `json:"qps,omitempty"`                // 该档全局 QPS；0 = 继承 plan.qps
	KVCacheVolumeGB float64 `json:"kv_cache_volume_gb,omitempty"` // 规范化后回填：sessions × 单会话 token / tokens_per_gb
}

// CacheRoundStats 是 multi-round-chat 单轮（跨 session 聚合）的统计。
type CacheRoundStats struct {
	Round           int     `json:"round"`
	Requests        int     `json:"requests"`
	TTFTMeanMS      float64 `json:"ttft_mean_ms"`
	InputTokensMean float64 `json:"input_tokens_mean,omitempty"`
}

// CacheProbeRound 是前缀命中率探针的单轮原始 usage。
type CacheProbeRound struct {
	Round        int     `json:"round"`
	PromptTokens int     `json:"prompt_tokens"`
	CachedTokens int     `json:"cached_tokens"`
	HitRate      float64 `json:"hit_rate"`         // cached / prompt（端点未上报时为 0）
	ReqMS        float64 `json:"req_ms,omitempty"` // 非流式请求耗时（prefill 主导，作 TTFT 代理）
}

// CacheMetrics 是一次 cache run 的结果（bench_summary.json + CSV 派生）。
//
// 单档 run 直接使用顶层字段；多档 run（plan.ladder 非空）顶层为跨档聚合，
// 每档的完整指标在 Tiers 里（Tiers 元素不再嵌套 Tiers）。
type CacheMetrics struct {
	Workload      string            `json:"workload"`
	Model         string            `json:"model,omitempty"`
	Bucket        string            `json:"bucket,omitempty"`   // 多档档位标识（c1/c2/c4…）
	Sessions      int               `json:"sessions,omitempty"` // 该档计划并发 session 数
	QPS           float64           `json:"qps,omitempty"`      // 该档计划 QPS
	Tiers         []*CacheMetrics   `json:"tiers,omitempty"`    // 多档：每档完整指标
	NumSessions   int               `json:"num_sessions,omitempty"`
	TotalRequests int               `json:"total_requests"`
	SuccessReqs   int               `json:"successful_requests"`
	FailedReqs    int               `json:"failed_requests"`
	SuccessRate   float64           `json:"success_rate"`
	ElapsedSec    float64           `json:"elapsed_sec"`
	FirstRound    *CacheRoundStats  `json:"first_round,omitempty"`
	SteadyState   *CacheSteadyStats `json:"steady_state,omitempty"`
	Rounds        []CacheRoundStats `json:"rounds,omitempty"`
	// 前缀命中率探针（Go 侧直采 usage.prompt_tokens_details.cached_tokens，
	// lmcache bench 的 CSV 不含该字段）
	ProbeRounds        []CacheProbeRound `json:"probe_rounds,omitempty"`
	PrefixHitRate      *float64          `json:"prefix_hit_rate,omitempty"`      // 稳态（非首轮）cached/prompt
	CacheFieldReported bool              `json:"cache_field_reported,omitempty"` // 端点是否上报 cached_tokens
	InputTokens        int               `json:"total_input_tokens,omitempty"`
	OutputTokens       int               `json:"total_output_tokens,omitempty"`
	// 来自 bench_summary.json 的聚合（Python 口径）
	MeanTTFTMS      float64 `json:"mean_ttft_ms,omitempty"`
	DecodeSpeedP50  float64 `json:"decode_speed_p50,omitempty"`
	DecodeSpeedP90  float64 `json:"decode_speed_p90,omitempty"`
	InputThroughput float64 `json:"input_throughput,omitempty"`

	// 性能维度（方案 A）：同一份 MRC 负载的「前缀缓存命中态」性能。
	// 注意 TTFT 受缓存影响大，decode speed / TPOT 主要反映模型生成性能。
	MeanDecodeSpeed  float64 `json:"mean_decode_speed,omitempty"` // tokens/s（输出侧，全量成功请求）
	OutputThroughput float64 `json:"output_throughput,omitempty"` // tokens/s（全 run）
	TPOTMeanMS       float64 `json:"tpot_mean_ms,omitempty"`      // 1000 / MeanDecodeSpeed
	LatencyP50MS     float64 `json:"request_latency_p50_ms,omitempty"`
	LatencyP90MS     float64 `json:"request_latency_p90_ms,omitempty"`
	LatencyP99MS     float64 `json:"request_latency_p99_ms,omitempty"`
}

// CacheSteadyStats 是稳态（非首轮）请求的分位统计——缓存命中下的真实表现。
type CacheSteadyStats struct {
	TTFTp50MS       float64 `json:"ttft_p50_ms"`
	TTFTp90MS       float64 `json:"ttft_p90_ms"`
	TTFTp99MS       float64 `json:"ttft_p99_ms"`
	Latencyp50MS    float64 `json:"request_latency_p50_ms,omitempty"`
	DecodeSpeedP50  float64 `json:"decode_speed_p50,omitempty"` // tokens/s（稳态，CSV 派生）
	TPOTp50MS       float64 `json:"tpot_p50_ms,omitempty"`      // 每输出 token 耗时（稳态）
	TPOTp90MS       float64 `json:"tpot_p90_ms,omitempty"`
	Requests        int     `json:"requests"`
	TTFTImprovement float64 `json:"ttft_improvement,omitempty"` // 1 - steady_p50 / first_mean
}

// JudgeSpec 是 LLM Judge 的凭据与地址。
//
// 凭据规则与 Target.api_key 一致：**明文只在内存**。对外的 JSON 里永远只出现
// `api_key_hint`（后 4 位掩码），落库走 runs.judge_key_enc 的 AES-GCM 密文列。
// 因此 APIKey 标 `json:"-"`，进出 JSON 由本类型的 Marshal/Unmarshal 单独处理。
type JudgeSpec struct {
	Model   string `json:"model,omitempty"`
	BaseURL string `json:"base_url,omitempty"`

	// APIKey 是明文，仅存在于进程内存（提交时 → 执行子进程）；永不落盘、永不回显。
	APIKey string `json:"-"`
	// APIKeyHint 是展示用掩码（••••••••abcd），可落盘、可回显。
	APIKeyHint string `json:"api_key_hint,omitempty"`
}

// MarshalJSON 只输出非敏感字段（契约：响应体不含明文凭据）。
func (j JudgeSpec) MarshalJSON() ([]byte, error) {
	type out struct {
		Model      string `json:"model,omitempty"`
		BaseURL    string `json:"base_url,omitempty"`
		APIKeyHint string `json:"api_key_hint,omitempty"`
	}
	return json.Marshal(out{Model: j.Model, BaseURL: j.BaseURL, APIKeyHint: j.APIKeyHint})
}

// UnmarshalJSON 接受请求里的 `api_key`（写入用），也兼容带 `api_key_hint` 的读回。
func (j *JudgeSpec) UnmarshalJSON(b []byte) error {
	var raw struct {
		Model      string `json:"model,omitempty"`
		BaseURL    string `json:"base_url,omitempty"`
		APIKey     string `json:"api_key,omitempty"`
		APIKeyHint string `json:"api_key_hint,omitempty"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	j.Model, j.BaseURL, j.APIKeyHint = raw.Model, raw.BaseURL, raw.APIKeyHint
	j.APIKey = raw.APIKey
	return nil
}

// HasKey 表示是否带可用凭据（明文或掩码二者之一即可表示已配置）。
func (j *JudgeSpec) HasKey() bool { return j != nil && (j.APIKey != "" || j.APIKeyHint != "") }

type RequestDefaults struct {
	Temperature       any `json:"temperature,omitempty"`
	MaxTokens         int `json:"max_tokens,omitempty"`
	MaxTurns          int `json:"max_turns,omitempty"`
	ConnectTimeoutSec int `json:"connect_timeout_sec,omitempty"`
	ReadTimeoutSec    int `json:"read_timeout_sec,omitempty"`
}

type Plan struct {
	Mode             string                    `json:"mode"`
	Buckets          []string                  `json:"buckets"`
	ParallelLadder   []int                     `json:"parallel_ladder,omitempty"`
	Number           int                       `json:"number,omitempty"`
	NumberByParallel map[int]int               `json:"number_by_parallel,omitempty"`
	BucketOverrides  map[string]BucketOverride `json:"bucket_overrides,omitempty"`
	CooldownSec      int                       `json:"cooldown_sec,omitempty"`
	SuccessRateMin   float64                   `json:"success_rate_min,omitempty"`
	TPMBudget        int64                     `json:"tpm_budget,omitempty"`
	StopOnFailure    *bool                     `json:"stop_on_failure,omitempty"`
	Request          RequestDefaults           `json:"request,omitempty"`
	DatasetDir       string                    `json:"dataset_dir,omitempty"`
	StepsTotal       int                       `json:"steps_total,omitempty"`

	// cache（capability=cache，tool=lmcache）
	Workload        string  `json:"workload,omitempty"` // multi-round-chat
	KVCacheVolumeGB float64 `json:"kv_cache_volume_gb,omitempty"`
	TokensPerGBKV   int     `json:"tokens_per_gb_kvcache,omitempty"`
	QPS             float64 `json:"qps,omitempty"`
	DurationSec     int     `json:"duration_sec,omitempty"`
	Seed            *int    `json:"seed,omitempty"`
	NoWarmup        bool    `json:"no_warmup,omitempty"`
	IgnoreEOS       bool    `json:"ignore_eos,omitempty"`
	// 并发档位（方案 B）：非空时按档位展开多 step；每档 sessions 反算 KV 容量，
	// qps 缺省继承 p.QPS；duration_sec 为「每档」时长。
	CacheLadder []CacheTier `json:"ladder,omitempty"`
	// 目标命中率（可选；与手动 user_input_length / output_length 互斥）。
	// 只能设置 min 或 max 其中一个，服务端据此反推每轮输入/输出长度（见 normalizeCachePlan）。
	HitRateMin float64 `json:"prefix_hit_rate_min,omitempty"`
	HitRateMax float64 `json:"prefix_hit_rate_max,omitempty"`
	// 请求形状（MRC）
	SharedPromptLen int `json:"shared_prompt_length,omitempty"`
	ChatHistoryLen  int `json:"chat_history_length,omitempty"`
	UserInputLen    int `json:"user_input_length,omitempty"`
	OutputLen       int `json:"output_length,omitempty"`

	// eval（capability=eval）
	Datasets      []string   `json:"datasets,omitempty"`
	Limit         int        `json:"limit,omitempty"`
	Repeats       int        `json:"repeats,omitempty"`
	EvalBatchSize int        `json:"eval_batch_size,omitempty"`
	Judge         *JudgeSpec `json:"judge,omitempty"`
	ProviderName  string     `json:"provider_name,omitempty"`
	Thinking      *bool      `json:"thinking,omitempty"`
	ThinkingType  string     `json:"thinking_type,omitempty"`

	// 工具私有字段透传
	ToolParams json.RawMessage `json:"tool_params,omitempty"`
}

func (p Plan) Cooldown() time.Duration { return time.Duration(p.CooldownSec) * time.Second }

// StepSpec 是一个 (bucket, parallel, number) 执行单元。
type StepSpec struct {
	Index    int    `json:"index"`
	Bucket   string `json:"bucket"`
	Parallel int    `json:"parallel"`
	Number   int    `json:"number"`
	Repeats  int    `json:"repeats,omitempty"` // eval：重复次数
	MinLen   int    `json:"min_prompt_len"`
	MaxLen   int    `json:"max_prompt_len"`
}

type Skip struct {
	Bucket string `json:"bucket"`
	Reason string `json:"reason"`
	Detail string `json:"detail,omitempty"`
}

// Normalizer 携带计算 steps 所需的默认值。
type Normalizer struct {
	Buckets               []Bucket
	DefaultLadder         []int
	NumberByParallel      map[int]int
	DefaultNumber         int
	ExtraParallels        map[string][]int
	DefaultCooldownSec    int
	DefaultSuccessRateMin float64
	DefaultStopOnFailure  bool
}

func (n Normalizer) Bucket(label string) (Bucket, bool) {
	for _, b := range n.Buckets {
		if b.Label == label {
			return b, true
		}
	}
	return Bucket{}, false
}

// ComputeSteps 与前端 computeSteps() 一致：
//
//	maxPar = min(bucket.max_parallel, override.max_parallel, tpm_budget>0 ? floor(tpm_budget/bucketTokens) : ∞)
//	parallels = sortedUnique(ladder ∪ override.parallels) filtered ≤ maxPar
//	为空 → 该档跳过
func ComputeSteps(p Plan, n Normalizer) (steps []StepSpec, skipped []Skip) {
	ladder := p.ParallelLadder
	if len(ladder) == 0 {
		ladder = n.DefaultLadder
	}
	idx := 0
	for _, label := range p.Buckets {
		b, ok := n.Bucket(label)
		if !ok {
			skipped = append(skipped, Skip{Bucket: label, Reason: "unknown_bucket"})
			continue
		}
		maxPar := b.MaxParallel
		ov := p.BucketOverrides[label]
		if ov.MaxParallel > 0 && ov.MaxParallel < maxPar {
			maxPar = ov.MaxParallel
		}
		if p.TPMBudget > 0 {
			if byTPM := int(p.TPMBudget / int64(maxInt(b.PromptTokens.Max, 1))); byTPM < maxPar {
				maxPar = byTPM
			}
		}
		set := map[int]bool{}
		for _, v := range ladder {
			if v > 0 && v <= maxPar {
				set[v] = true
			}
		}
		for _, v := range ov.Parallels {
			if v > 0 && v <= maxPar {
				set[v] = true
			}
		}
		ps := make([]int, 0, len(set))
		for v := range set {
			ps = append(ps, v)
		}
		sort.Ints(ps)
		if len(ps) == 0 {
			skipped = append(skipped, Skip{
				Bucket: label, Reason: "budget_too_low",
				Detail: fmt.Sprintf("并发上限 %d，梯度最小值 %d", maxPar, minPositive(ladder)),
			})
			continue
		}
		for _, par := range ps {
			// 请求数优先级（对齐 run.sh）：
			//   plan.number（单档显式指定）> bucket_overrides[b].number（对齐 N_<label>）
			//   > number_by_parallel（对齐 parallel_number）> default_number
			num := 0
			switch {
			case p.Number > 0:
				num = p.Number
			case ov.Number > 0:
				num = ov.Number
			case n.NumberByParallel[par] > 0:
				num = n.NumberByParallel[par]
			case n.DefaultNumber > 0:
				num = n.DefaultNumber
			default:
				num = 100
			}
			// TPM 预算收敛请求数（仅在未显式给 N 时）
			if p.TPMBudget > 0 && ov.Number == 0 {
				if perMin := int(p.TPMBudget / int64(maxInt(b.PromptTokens.Max, 1))); perMin > 0 && num > perMin {
					num = perMin
				}
			}
			steps = append(steps, StepSpec{
				Index: idx, Bucket: label, Parallel: par, Number: num,
				MinLen: b.PromptTokens.Min, MaxLen: b.PromptTokens.Max,
			})
			idx++
		}
	}
	return steps, skipped
}

// StepPlanDetail 用于前端预估展示。
type StepPlanDetail struct {
	Bucket      string `json:"bucket"`
	Parallels   []int  `json:"parallels"`
	MaxParallel int    `json:"max_parallel"`
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minPositive(v []int) int {
	m := 0
	for _, x := range v {
		if x > 0 && (m == 0 || x < m) {
			m = x
		}
	}
	return m
}

func containsInt(v []int, x int) bool {
	for _, e := range v {
		if e == x {
			return true
		}
	}
	return false
}

// ---------------- Run ----------------

type RunStatus string

const (
	StatusQueued    RunStatus = "queued"
	StatusPreparing RunStatus = "preparing"
	StatusRunning   RunStatus = "running"
	StatusReporting RunStatus = "reporting"
	StatusSucceeded RunStatus = "succeeded"
	StatusPartial   RunStatus = "partial"
	StatusFailed    RunStatus = "failed"
	StatusCanceled  RunStatus = "canceled"
)

func (s RunStatus) Terminal() bool {
	switch s {
	case StatusSucceeded, StatusPartial, StatusFailed, StatusCanceled:
		return true
	}
	return false
}

func (s RunStatus) Known() bool {
	switch s {
	case StatusQueued, StatusPreparing, StatusRunning, StatusReporting,
		StatusSucceeded, StatusPartial, StatusFailed, StatusCanceled:
		return true
	}
	return false
}

type StepStatus string

const (
	StepPending   StepStatus = "pending"
	StepRunning   StepStatus = "running"
	StepSucceeded StepStatus = "succeeded"
	StepFailed    StepStatus = "failed"
	StepSkipped   StepStatus = "skipped"  // 未开始就结束（取消/前序失败）
	StepCanceled  StepStatus = "canceled" // 已开始的被中断
)

type Step struct {
	Index       int        `json:"index"`
	Bucket      string     `json:"bucket"`
	Parallel    int        `json:"parallel"`
	Number      int        `json:"number"`
	Status      StepStatus `json:"status"`
	SuccessRate *float64   `json:"success_rate"`
	TTFTp50Sec  *float64   `json:"ttft_p50_sec"`
	OutputTPS   *float64   `json:"output_tps"`
	DurationSec *float64   `json:"duration_sec"`
	DBPath      string     `json:"-"`
}

type Progress struct {
	Phase      string    `json:"phase"`
	StepsTotal int       `json:"steps_total"`
	StepsDone  int       `json:"steps_done"`
	Percent    float64   `json:"percent"`
	Current    *StepHead `json:"current"`
	ElapsedSec int       `json:"elapsed_sec"`
	ETASec     *int      `json:"eta_sec"`
}

type StepHead struct {
	Bucket   string `json:"bucket"`
	Parallel int    `json:"parallel"`
	Number   int    `json:"number"`
}

type GateSpec struct {
	Profile   string         `json:"profile"`
	Mode      string         `json:"mode,omitempty"` // report_only | enforce
	Overrides map[string]any `json:"overrides,omitempty"`
	Params    map[string]any `json:"params,omitempty"`
}

type Violation struct {
	Scope     string  `json:"scope"`
	Bucket    string  `json:"bucket,omitempty"`
	Parallel  int     `json:"parallel,omitempty"`
	Rule      string  `json:"rule"`
	Tier      string  `json:"tier,omitempty"`
	Actual    float64 `json:"actual"`
	Threshold float64 `json:"threshold"`
	Message   string  `json:"message,omitempty"`
}

type TierSummary struct {
	Tier      string  `json:"tier"`
	Steps     int     `json:"steps"`
	P50MaxSec float64 `json:"p50_max_sec"`
	P90MaxSec float64 `json:"p90_max_sec"`
	Passed    bool    `json:"passed"`
}

type GateResult struct {
	Passed      bool          `json:"passed"`
	Profile     string        `json:"profile"`
	Violations  []Violation   `json:"violations"`
	TierSummary []TierSummary `json:"tier_summary"`
}

type EngineInfo struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	Workdir string `json:"workdir,omitempty"`
	PID     *int   `json:"pid"`
	Cmdline string `json:"cmdline,omitempty"`
}

// ReportSpec 是报告生成参数（plan 的 report 块）。
type ReportSpec struct {
	Formats    []string `json:"formats,omitempty"`
	ClientName string   `json:"client_name,omitempty"`
	Title      string   `json:"title,omitempty"`
}

type Run struct {
	ID         string            `json:"run_id"`
	Status     RunStatus         `json:"status"`
	Tool       string            `json:"tool"`
	Capability Capability        `json:"capability"`
	Target     TargetSnapshot    `json:"target"`
	Plan       Plan              `json:"plan"`
	Gate       *GateSpec         `json:"gate,omitempty"`
	Report     ReportSpec        `json:"report,omitempty"`
	Progress   Progress          `json:"progress"`
	Steps      []Step            `json:"steps,omitempty"`
	GateResult *GateResult       `json:"gate_result"`
	Summary    map[string]any    `json:"summary"`
	Artifacts  []Artifact        `json:"artifacts"`
	Engine     *EngineInfo       `json:"engine,omitempty"`
	Error      *Problem          `json:"error"`
	Labels     map[string]string `json:"labels,omitempty"`
	// JudgeKeyEnc 是 judge 凭据的 AES-GCM 密文，只在库与内存间流转，永不序列化。
	JudgeKeyEnc []byte            `json:"-"`
	CreatedAt   time.Time         `json:"created_at"`
	StartedAt   *time.Time        `json:"started_at"`
	FinishedAt  *time.Time        `json:"finished_at"`
	Links       map[string]string `json:"links,omitempty"`
}

// ---------------- Artifact ----------------

type ArtifactKind string

const (
	ArtHTMLReport   ArtifactKind = "html_report"
	ArtXLSXReport   ArtifactKind = "xlsx_report"
	ArtFailureCSV   ArtifactKind = "failure_csv"
	ArtSLAJSON      ArtifactKind = "sla_json"
	ArtRawDB        ArtifactKind = "raw_db"
	ArtRunLog       ArtifactKind = "run_log"
	ArtEvalReport   ArtifactKind = "eval_report"
	ArtEvalSummary  ArtifactKind = "eval_summary"
	ArtEvalSamples  ArtifactKind = "eval_samples"
	ArtEvalArchive  ArtifactKind = "eval_archive"
	ArtBenchCSV     ArtifactKind = "bench_csv"
	ArtBenchSummary ArtifactKind = "bench_summary"
	ArtBenchConfig  ArtifactKind = "bench_config"
	ArtArchive      ArtifactKind = "archive"
)

type Artifact struct {
	ID          string       `json:"artifact_id"`
	Kind        ArtifactKind `json:"kind"`
	Filename    string       `json:"filename"`
	ContentType string       `json:"content_type,omitempty"`
	SizeBytes   int64        `json:"size_bytes"`
	SHA256      string       `json:"sha256,omitempty"`
	CreatedAt   time.Time    `json:"created_at"`
	DownloadURL string       `json:"download_url"`
	Path        string       `json:"-"`
}

// ---------------- Problem ----------------

type Problem struct {
	Type        string `json:"type,omitempty"`
	Title       string `json:"title"`
	Status      int    `json:"status"`
	Code        string `json:"code"`
	Detail      string `json:"detail,omitempty"`
	Instance    string `json:"instance,omitempty"`
	RequestID   string `json:"request_id,omitempty"`
	Retryable   bool   `json:"retryable,omitempty"`
	AvailableAt string `json:"available_at,omitempty"`
}

var errorBase = "https://thvv.dev/errors/"

func (p *Problem) normalize() {
	if p.Type == "" && p.Code != "" {
		p.Type = errorBase + p.Code
	}
	if p.Title == "" {
		p.Title = strings.ReplaceAll(p.Code, "_", " ")
	}
}

// NewProblem 构造一个符合 RFC 9457 的错误。
func NewProblem(status int, code, detail string) *Problem {
	p := &Problem{Status: status, Code: code, Detail: detail}
	p.normalize()
	return p
}

// ---------------- 指标结果 ----------------

type RequestCounts struct {
	Total       int     `json:"total"`
	Success     int     `json:"success"`
	Failed      int     `json:"failed"`
	SuccessRate float64 `json:"success_rate"`
}

type TTFTStats struct {
	AvgSec float64 `json:"avg_sec"`
	MinSec float64 `json:"min_sec"`
	MaxSec float64 `json:"max_sec"`
	P50Sec float64 `json:"p50_sec"`
	P90Sec float64 `json:"p90_sec"`
	P95Sec float64 `json:"p95_sec"`
	P99Sec float64 `json:"p99_sec"`
}

type Scalar struct {
	AvgSec float64 `json:"avg_sec,omitempty"`
	AvgMs  float64 `json:"avg_ms,omitempty"`
	P50Sec float64 `json:"p50_sec,omitempty"`
	P90Sec float64 `json:"p90_sec,omitempty"`
	P95Sec float64 `json:"p95_sec,omitempty"`
	P99Sec float64 `json:"p99_sec,omitempty"`
	P50Ms  float64 `json:"p50_ms,omitempty"`
	P90Ms  float64 `json:"p90_ms,omitempty"`
	P95Ms  float64 `json:"p95_ms,omitempty"`
	P99Ms  float64 `json:"p99_ms,omitempty"`
}

type Throughput struct {
	InputTPS  float64 `json:"input_tps"`
	OutputTPS float64 `json:"output_tps"`
	InputTPM  float64 `json:"input_tpm"`
	OutputTPM float64 `json:"output_tpm"`
	TPM       float64 `json:"tpm"`
	QPM       float64 `json:"qpm"`
}

type TokenSums struct {
	PromptSum     int `json:"prompt_sum"`
	CompletionSum int `json:"completion_sum"`
	TotalSum      int `json:"total_sum"`
}

// StepMetrics 与 perf/scripts/gen_report_from_db.py 的口径逐项对齐。
// 注意：ITL（相邻 chunk 间隔，按 chunk 汇总）≠ TPOT（time_per_output_token，按请求）。
type StepMetrics struct {
	StepIndex    int            `json:"step_index"`
	Bucket       string         `json:"bucket"`
	Parallel     int            `json:"parallel"`
	Number       int            `json:"number"`
	Requests     RequestCounts  `json:"requests"`
	TTFT         TTFTStats      `json:"ttft"`
	TTLT         Scalar         `json:"ttlt"`
	TPOT         Scalar         `json:"tpot"`
	ITL          Scalar         `json:"itl"`
	Throughput   Throughput     `json:"throughput"`
	Tokens       TokenSums      `json:"tokens"`
	TokenRateAvg float64        `json:"token_rate_avg"`
	TokenRate    Scalar         `json:"token_rate,omitempty"` // 逐请求输出速率的分位数（t/s）
	DurationSec  float64        `json:"duration_sec"`
	Failures     map[string]int `json:"failures,omitempty"`
	RawDB        string         `json:"raw_db,omitempty"`
}

type PerfMetrics struct {
	PerStep         []StepMetrics    `json:"per_step"`
	DifficultyCurve []map[string]any `json:"difficulty_curve,omitempty"`
}

type FailureItem struct {
	StepIndex           int      `json:"step_index"`
	Bucket              string   `json:"bucket"`
	Parallel            int      `json:"parallel"`
	RequestIndex        int      `json:"request_index"`
	Reason              string   `json:"reason"`
	HTTPStatus          *int     `json:"http_status"`
	Error               string   `json:"error,omitempty"`
	Detail              string   `json:"detail,omitempty"`   // 错误摘要（140 字符内，写入报告「详细说明」）
	TTFTSec             *float64 `json:"ttft_sec,omitempty"` // 失败请求若已收到首包
	TTLTSec             *float64 `json:"ttlt_sec,omitempty"` // 失败请求的总耗时
	RequestBodyExcerpt  string   `json:"request_body_excerpt,omitempty"`
	ResponseBodyExcerpt string   `json:"response_body_excerpt,omitempty"`
}

// RunResults 是 GET /v1/runs/{id}/results 的响应体。
type RunResults struct {
	RunID      string         `json:"run_id"`
	Capability Capability     `json:"capability"`
	GateResult *GateResult    `json:"gate_result,omitempty"`
	Summary    map[string]any `json:"summary"`
	Metrics    PerfMetrics    `json:"metrics"`
	Eval       *EvalMetrics   `json:"eval,omitempty"`
	Cache      *CacheMetrics  `json:"cache,omitempty"`
}

// EvalDatasetResult 是单个数据集的评测结果（来自 eval_summary.json）。
type EvalDatasetResult struct {
	Dataset      string   `json:"dataset"`
	Label        string   `json:"label,omitempty"`
	Success      bool     `json:"success"`
	Score        float64  `json:"score"`
	Metric       string   `json:"metric,omitempty"`
	NumSamples   int      `json:"num_samples,omitempty"`
	Limit        int      `json:"limit,omitempty"`
	Repeats      int      `json:"repeats,omitempty"`
	EvalBatch    int      `json:"eval_batch_size,omitempty"`
	RetriesUsed  int      `json:"retries_used,omitempty"`
	RateLimited  bool     `json:"rate_limited,omitempty"`
	ElapsedSec   float64  `json:"elapsed_sec,omitempty"`
	Skipped      int      `json:"skipped,omitempty"`
	CacheHitRate *float64 `json:"cache_hit_rate,omitempty"`
	Error        string   `json:"error,omitempty"`
	Artifacts    []string `json:"artifacts,omitempty"`
}

// SubitemOf 返回子项名（当前恒为空，留给 tau2_bench 的领域、longbench 子集等场景）。
func (d EvalDatasetResult) SubitemOf() string { return "" }

// EvalMetrics 是一次 eval run 的全部数据集结果。
type EvalMetrics struct {
	Model    string              `json:"model,omitempty"`
	Provider string              `json:"provider,omitempty"`
	Datasets []EvalDatasetResult `json:"datasets"`
}
