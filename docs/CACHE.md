# 缓存测试（cache）接入设计

> 目标：把 LMCache 的 `lmcache bench engine --workload multi-round-chat` 接进 Go 服务端，
> 作为与 perf / eval 并列的第三种能力（`capability=cache`），复用现有
> Target / Run / Step / Artifact / SSE / 取消 / 报告下载协议。
>
> 状态：v1 已实现并真机端到端验证通过（§11 清单全部完成；真机结论见文末「验证记录」）。
> v1.1（方案 A）已接入「性能维度」：同一个 multi-round-chat run 同时产出缓存收益与稳态性能
> （TPOT / 解码速度 / 时延分位 / 吞吐），并支持 `cache-perf-sla-v1` 验收，见 §3.3、§7、§11.2。
> v1.2（方案 B）已支持「并发档位」：plan.ladder 多档 → 多 step 顺序执行，
> 逐档出缓存收益 + 性能指标，gate 逐档判定，见 §3.2、§3.3、§11.3。
> 相关：[`API.md`](./API.md) · [`CONFIG.md`](./CONFIG.md) · [`EVAL.md`](./EVAL.md)（接入模式的先例）
> 上游文档：https://docs.lmcache.ai/zh_CN/cli/bench.html

---

## 1. 测试工具（已部署）

### 1.1 安装位置与方式

```bash
mkdir -p /infer-data/bench/bench-cache && cd /infer-data/bench/bench-cache
python3 -m venv .venv
source .venv/bin/activate
pip install lmcache openai      # openai 是 bench engine 的可选依赖（AsyncOpenAI 客户端）
```

- 版本：LMCache **v0.5.7**（`lmcache --version` 可查）
- 解释器：`/infer-data/bench/bench-cache/.venv/bin/python`
- CLI 入口：`/infer-data/bench/bench-cache/.venv/bin/lmcache`
- 与 thvv / bench-agent 的 venv 完全独立，互不影响

### 1.2 调用契约

```
lmcache bench engine \
  --engine-url <ENGINE_URL>          # OpenAI 兼容端点；工具自动补 scheme 与结尾 /v1
  --model <MODEL>                    # 缺省时自动探测（走 /v1/models）
  --workload multi-round-chat
  --kv-cache-volume <GB>             # 目标活跃 KV 量（决定并发 session 数）
  --tokens-per-gb-kvcache <N>        # 每 GB KV 的 token 数（无 --lmcache-url 时必填）
  [--mrc-shared-prompt-length 2000]  # system prompt token 数
  [--mrc-chat-history-length 10000]  # 预填历史 token 数
  [--mrc-user-input-length 50]       # 每轮用户输入 token 数
  [--mrc-output-length 200]         # 每轮最大输出 token 数
  [--mrc-qps 1.0]                    # 全局请求速率
  [--mrc-duration 60]                # 测量时长（秒）
  [--seed 42] [--no-warmup] [--ignore-eos] [-q]
  --output-dir <DIR> --json --no-interactive
```

关键行为（读源码确认，v0.5.7）：

| 行为 | 说明 |
|---|---|
| 凭据 | 只认环境变量 **`OPENAI_API_KEY`**（未设则用 `sk-dummy`）；不进 argv，日志不落 key |
| URL 规范化 | `_normalize_url`：无 scheme 补 `http://`，结尾非 `/v1` 补 `/v1` |
| 并发用户数 | `num_concurrent_users = kv_cache_volume_gb × tokens_per_gb_kvcache ÷ (shared_prompt + chat_history)`，**由容量推导，不直接指定** |
| 会话状态 | 每个用户一个有状态 session：system prompt + 预填历史 + 累积 Q&A；第 N 轮请求包含前 N-1 轮问答 → 前缀单调增长，天然考察前缀缓存 |
| 预热 | warmup 阶段每 session 发一条 `max_tokens=1` 请求，**统计丢弃**（`--no-warmup` 跳过，用于测冷启动） |
| thinking 模型 | 流式 chunk 提取顺序 `content → reasoning_content → reasoning`，glm-5.3 等思考模型可直接测 |
| 退出码 | 成功 0；参数/依赖错误非 0（`--no-interactive` 下缺参直接报错退出，不会卡交互） |

### 1.3 产物契约（冒烟实测）

`--output-dir` 下固定两个文件：

**`bench_results.csv`**（逐请求，11 列）：

```csv
request_id,successful,ttft,request_latency,num_input_tokens,num_output_tokens,
decode_speed,submit_time,first_token_time,finish_time,error
s0_r0,True,1.0550086498,1.9992547035,4583,128,135.55788716,1794032745.09,…
```

- `request_id` 形如 `s<session>_r<round>`，**session 与轮次可从 ID 解析**
- `ttft`/`request_latency` 单位秒；`num_input_tokens` 来自服务端 usage（真实口径）

**`bench_summary.json`**：

```json
{
  "config": { "engine_url": "...", "model": "...", "workload": "multi-round-chat",
              "kv_cache_volume_gb": 0.25, "mrc_duration": 30.0, ... },
  "results": {
    "total_requests": 15, "successful_requests": 15, "failed_requests": 0,
    "elapsed_time": 30.0,
    "mean_ttft_ms": 502.5, "p50_ttft_ms": 450.8, "p90_ttft_ms": 539.8, "p99_ttft_ms": 984.9,
    "mean_decode_speed": 138.3, "p50/p90/p99_decode_speed": ...,
    "mean/p50/p90/p99_request_latency_ms": ...,
    "input_throughput": 2468.7, "output_throughput": 61.8,
    "total_input_tokens": 74069, "total_output_tokens": 1855
  }
}
```

**冒烟结果**（glm-5.3-test @ super.theturbo.ai，30s / 0.5 QPS / 3 sessions）：

- 15/15 成功；多轮前缀增长可见（s0_r0 输入 4583 → s0_r3 输入 4762 token）
- **首轮 TTFT 1055ms → 后续轮 ~440ms**：前缀缓存收益直接体现在 TTFT 上，这正是本测试的核心观测点

---

## 2. 设计边界

| 做 | 不做 |
|---|---|
| multi-round-chat 单 workload | 不接 kv-tier-pressure / long-doc-* 等其它 workload（协议留 `workload` 字段） |
| 复用现有 Target（OpenAI 兼容端点 + 凭据） | 不管理 LMCache server 本身（`lmcache server` 的部署/配置属被测环境） |
| bench_summary.json → 结构化指标；CSV 逐条归档 | 不在 Go 侧重算逐请求 TTFT / 吞吐（以 Python 落盘为准）；TPOT / 稳态分位由 CSV 派生 |
| 缓存收益分析：首轮 vs 稳态 TTFT 对比（从 CSV 派生） | 不做 token 级命中归因（需要被测端开 token trace，另用 `bench trace-analyze`） |
| `cache-sla-v1` 验收：稳态 TTFT 阈值 + 改善率 | 不做跨 run 的缓存收益趋势对比（v1.3） |
| **性能维度（方案 A）**：稳态 TPOT / 解码速度 / 时延分位 / 吞吐 → summary + `cache-perf-sla-v1` | — |
| **并发档位（方案 B）**：plan.ladder 多档 → 多 step 顺序执行，出并发-性能曲线（每档独立 gate） | 不做档位**并发**执行（会互相污染延迟；档位必须顺序跑） |

---

## 3. 数据模型

### 3.1 引擎与能力注册

新引擎 `lmcache`（与 `thvv` 并列的第二个工具）：

```yaml
roots:
  bench-cache: "/infer-data/bench/bench-cache"   # 新增根
  bin-cache: "bench-cache:.venv/bin"

engines:
  lmcache:
    enabled: true
    kind: subprocess
    display_name: "LMCache 缓存测试"
    runtime:
      python: "bin-cache:python"
      # lmcache CLI 必须在 PATH：bench 子命令用 python -m 亦可
      env:
        OPENAI_API_KEY: ""    # 由 run 注入（凭据只经环境变量，不进 argv/日志）
    capabilities:
      cache:
        enabled: true
        status: ga
        working_dir: "bench-cache:"
        script: "bin-cache:lmcache"   # 直接执行 CLI
        cache_defaults: { seed: 42, duration_sec: 60, qps: 1.0, ... }
```

`GET /v1/tools` 返回两个工具；`capability=cache` 归属 `tool=lmcache`。
协议层不变：`POST /v1/runs { "tool": "lmcache", "capability": "cache", ... }`。

### 3.2 plan

```json
{
  "mode": "single",
  "workload": "multi-round-chat",
  "request": {
    "shared_prompt_length": 2000,
    "chat_history_length": 10000,
    "user_input_length": 50,
    "output_length": 200
  },
  "kv_cache_volume_gb": 100,
  "tokens_per_gb_kvcache": 65536,
  "qps": 1.0,
  "duration_sec": 60,
  "seed": 42,
  "no_warmup": false,
  "ignore_eos": false,
  "ladder": [
    { "sessions": 1, "qps": 0.5 },
    { "sessions": 2, "qps": 1.0 },
    { "sessions": 4, "qps": 2.0 }
  ]
}
```

`ladder`（可选）是**并发档位**：非空时一个档位一个 step、顺序执行，每档独立跑一次
`lmcache bench engine multi-round-chat`；`duration_sec` 表示**每档**时长。

规范化规则：

1. `workload` 缺省 `multi-round-chat`；只接受这一个值，其它 → `400`
2. `duration_sec` ∈ [10, 3600]；`qps` ∈ (0, 100]
3. token 长度参数 ≥ 1 且 ≤ 1M；`kv_cache_volume_gb` > 0（ladder 非空时仅作兜底）；
   `tokens_per_gb_kvcache` ∈ [1024, 10M]
4. `no_warmup` 用于冷启动对比 run（正常验收 run 必须预热）
5. **ladder 非空**时：每档 `sessions` ∈ [1, 10000]、最多 20 档、不可重复；
   `qps` 缺省继承全局 `qps`（0 < qps ≤ 100）；按 `sessions` 升序执行
6. 每档容量由 session 数反算并**回填**到 `plan.ladder[].kv_cache_volume_gb`（便于复现）：
   `ceil(sessions × (shared_prompt + chat_history) / tokens_per_gb_kvcache × 1e6) / 1e6`。
   因此 ladder 要求 `shared_prompt_length + chat_history_length > 0`（可由 `cache_defaults` 提供）
7. `steps_total` = 1（单档）或档位数；总时长按 `档数 × (duration_sec + 120s 预热预算)`
   校验 `limits.max_run_duration`（默认 12h），超限 → 400
8. 单档（ladder 为空）语义与 v1 完全一致，`steps_total = 1`

### 3.3 Step 与 Run 结果

Step（单档）：`bucket = "multi-round-chat"`、`number = 请求数（事后回填）`、
`parallel = 并发 session 数（事后回填）`。
Step（多档）：`bucket = "c<sessions>"`（如 `c4`）、`parallel = 该档计划 session 数`、
`number = 该档请求数（事后回填）`。

`GET /v1/runs/{id}/results`（`capability=cache`）：

```json
{
  "run_id": "run_...",
  "capability": "cache",
  "summary": {
    "verdict": "pass",
    "total_requests": 15, "successful_requests": 15, "success_rate": 1.0,
    "ttft_first_round_ms": 1055.0, "ttft_steady_p50_ms": 447.7,
    "ttft_steady_p90_ms": 539.8, "ttft_steady_p99_ms": 984.9,
    "ttft_improvement": 0.576,
    "mean_decode_speed": 138.3, "tpot_mean_ms": 7.2,
    "tpot_steady_p50_ms": 7.4, "tpot_steady_p90_ms": 8.1,
    "decode_speed_steady_p50": 134.0,
    "input_throughput": 2468.7, "output_throughput": 61.8,
    "request_latency_p50_ms": 1375.5, "request_latency_p90_ms": 1502.0,
    "request_latency_p99_ms": 1984.9,
    "samples_steady": 12
  },
  "cache": {
    "workload": "multi-round-chat",
    "num_sessions": 3,
    "mean_decode_speed": 138.3, "tpot_mean_ms": 7.2,
    "output_throughput": 61.8,
    "request_latency_p50_ms": 1375.5, "request_latency_p90_ms": 1502.0,
    "request_latency_p99_ms": 1984.9,
    "steady_state": {
      "ttft_p50_ms": 447.7, "ttft_p90_ms": 539.8, "ttft_p99_ms": 984.9,
      "request_latency_p50_ms": 1375.5,
      "decode_speed_p50": 134.0, "tpot_p50_ms": 7.4, "tpot_p90_ms": 8.1,
      "requests": 12
    },
    "first_round": { "ttft_mean_ms": 1055.0, "requests": 3 },
    "rounds": [
      { "round": 0, "requests": 3, "ttft_mean_ms": 1055.0, "input_tokens_mean": 4583 },
      { "round": 1, "requests": 3, "ttft_mean_ms": 443.5, "input_tokens_mean": 4762 }
    ]
  }
}
```

派生口径（Go 从 `bench_results.csv` 计算，与 Python 的 summary 互补）：

- **round**：解析 `request_id` 的 `r<N>`；每个 session 的第 0 轮记为「首轮」
- `ttft_first_round_ms`：所有 session 首轮 TTFT 均值（冷/半冷缓存）
- `ttft_steady_p50_ms`：非首轮请求的 TTFT P50（稳态，前缀已命中）
- `ttft_improvement = 1 - steady_p50 / first_round_mean`：**缓存收益的核心指标**
- 失败请求（`successful=False`）不进统计，计入 `success_rate`
- **性能维度（方案 A）**：
  - `tpot_*` / `decode_speed_*` 从 CSV 的 `decode_speed` 列派生（`TPOT = 1000 / decode_speed`）；
    该列缺失时用 `(request_latency - ttft) / (num_output_tokens - 1)` 兜底
  - `request_latency_p50/p90/p99_ms`、`mean_decode_speed`、`input/output_throughput` 直接取
    `bench_summary.json`（Python 口径，全量成功请求）
  - **口径**：这些是「多轮对话 + 前缀缓存命中态」下的表观性能。`TTFT` 受前缀缓存影响大；
    `TPOT / 解码速度 / 输出吞吐`主要反映模型生成性能。与 perf（冷启动 / 定压 / 按输入长度分档）
    的数字**不可直接横比**，比较时须 workload 形状与参数一致
  - 统计可靠性由 `samples_steady` 体现：默认 60s × 1QPS 只有约 60 条请求，P99 基本等于最大值

**多档（ladder）结果**：顶层是跨档聚合，每档明细在 `cache.tiers[]`（元素不再嵌套 `tiers`）：

```json
{
  "capability": "cache",
  "summary": {
    "verdict": "fail", "tiers_count": 3, "max_sessions": 4,
    "total_requests": 72, "successful_requests": 72, "success_rate": 1.0,
    "mean_ttft_ms": 512.3, "tpot_mean_ms": 7.1,
    "input_throughput": 2300.5, "output_throughput": 58.2,
    "prefix_hit_rate": 0.91, "samples_steady": 63
  },
  "cache": {
    "workload": "multi-round-chat",
    "total_requests": 72, "successful_requests": 72, "failed_requests": 0,
    "tiers": [
      {
        "bucket": "c1", "sessions": 1, "qps": 0.5, "num_sessions": 1,
        "total_requests": 15, "successful_requests": 15, "success_rate": 1.0,
        "elapsed_sec": 30.0,
        "steady_state": {"requests": 12, "ttft_p50_ms": 447.7,
                          "tpot_p50_ms": 7.4, "decode_speed_p50": 134.0},
        "output_throughput": 61.8,
        "rounds": [{"round": 0, "requests": 1, "ttft_mean_ms": 1055.0}]
      },
      { "bucket": "c2", "sessions": 2, "qps": 1.0, "...": "同结构" },
      { "bucket": "c4", "sessions": 4, "qps": 2.0, "...": "同结构" }
    ],
    "probe_rounds": [{"round": 0, "prompt_tokens": 12000, "cached_tokens": 0}],
    "prefix_hit_rate": 0.91, "cache_field_reported": true
  }
}
```

聚合口径：

- 顶层只做**可加**聚合：请求数 / 成功数 / token 数 / 时长求和；`success_rate` 按总请求重算
- `mean_ttft_ms` 按成功请求加权；`tpot_mean_ms` / `mean_decode_speed` 按输出 token 加权；
  `input_throughput` / `output_throughput` = 总 token / 总时长
- **分位数（P50/P90/P99）无法跨档合并**，只在各档 `tiers[]` 内保留
- `samples_steady` / `tiers_count` / `max_sessions` 供列表页与前端快速展示
- gate **逐档判定**：违规带 `tier` 字段（如 `c4`），`gate_result.tier_summary` 每档一行

---

## 4. 执行流程

```
POST /v1/runs (tool=lmcache, capability=cache)
  └─ normalizeCachePlan → steps[1]（单档）或 steps[N]（ladder 多档，按 sessions 升序）
  └─ 入队（与 perf/eval 共享并发上限、同 target 串行）

worker:
  ├─ 解密 target 凭据
  ├─ 前缀命中率探针（整 run 一次，写 <runDir>/cache/prefix_usage.csv）
  └─ for 每个 step（档位）:
       ├─ 生效 plan = base + ladder[i]（kv 容量 / qps）
       ├─ spawn: lmcache bench engine ... --output-dir <runDir>/cache/<bucket>
       │    env: OPENAI_API_KEY=<target key>，PATH 前置 .venv/bin
       ├─ stdout → 日志 + SSE；进度解析（MRC 无逐请求输出，只推阶段事件）
       ├─ 进程结束 → --export-config + 读 bench_summary.json / bench_results.csv
       ├─ 派生该档 rounds / first-round / steady / 性能指标
       └─ 回填该 step（parallel = 实测 session 数，number = 请求数）
  ├─ 多档：聚合顶层指标（Tiers 明细保留）→ run.summary + metrics.json
  └─ 归档 artifacts → run.succeeded/failed/partial
```

档位**顺序**执行（不并发）：避免相邻档位互相抢带宽/缓存导致曲线失真。探针在全部档位之前
只跑一次，其结果挂到顶层结果上（各档 bench 自带 warmup，探针只影响首档前的缓存状态）。

取消：SIGTERM 进程组（httpx 客户端随进程终止），step 标 `canceled`；
CSV/JSON 已落盘的部分照常归档（`--json` 下 summary 在结束时才写，取消可能只有 CSV——
CSV 缺失时 step 判 failed，错误信息指向 run.log）。

---

## 5. 预检（probe）

`POST /v1/targets/{id}/probe?capability=cache`（lmcache 引擎）：

| 检查 | 内容 |
|---|---|
| `engine` | `lmcache --version`（校验 venv 可用） |
| `script` | `lmcache bench engine --help` 退出码 0（子命令存在） |
| `connectivity` | 对 target 端点发最小请求（与 perf/eval 共用 pingTarget） |
| `workload` | `--workload multi-round-chat --export-config` 到临时目录（参数组合合法性，不实际发请求） |

---

## 6. 产物

```
data:runs/<run_id>/
├── metrics.json                     # { "cache": {...} }（§3.3 结构；多档含 tiers[]）
├── logs/<run_id>-cache.log          # 引擎原始输出（脱敏，按档位分段）
├── cache/
│   ├── prefix_usage.csv             # 前缀命中率探针原始数据（整 run 一份）
│   ├── multi-round-chat/            # 单档：--output-dir（原始）
│   │   ├── bench_results.csv
│   │   ├── bench_summary.json
│   │   └── resolved_config.json
│   └── c1/  c4/  …                  # 多档：每档一个 --output-dir
│       ├── bench_results.csv
│       ├── bench_summary.json
│       └── resolved_config.json
└── artifacts/
    ├── multi-round-chat-prefix_usage.csv
    ├── multi-round-chat-bench_results.csv                  # 单档
    ├── multi-round-chat-bench_summary.json
    ├── multi-round-chat-resolved_config.json
    ├── c1-bench_results.csv / c1-bench_summary.json / c1-resolved_config.json
    ├── c4-bench_results.csv / c4-bench_summary.json / c4-resolved_config.json
    └── run.log
```

artifact kinds：`bench_csv`、`bench_summary`（json）、`bench_config`（json）、`run_log`。
多档时产物按档位前缀命名（`c1-*` / `c4-*`），同一档的 CSV/JSON/配置成组下载。

---

## 7. 验收（cache-sla-v1 / cache-perf-sla-v1）

```yaml
gates:
  profiles:
    cache-sla-v1:                      # v1：只看缓存收益（保持向后兼容）
      capability: cache
      description: "多轮对话前缀缓存：稳态 TTFT 上限 + 首轮改善率"
      cache_tiers:
        - steady_ttft_p50_max_ms: 800     # 稳态 TTFT P50 上限
          ttft_improvement_min: 0.30      # 稳态相对首轮至少改善 30%
          success_rate_min: 0.99
    cache-perf-sla-v1:                 # v1.1：缓存收益 + 稳态性能
      capability: cache
      description: "多轮对话：前缀缓存收益 + 稳态性能 + 最小样本数"
      cache_tiers:
        - steady_ttft_p50_max_ms: 800
          ttft_improvement_min: 0.30
          success_rate_min: 0.99
          prefix_hit_rate_min: 0.50
          min_samples: 100               # 稳态样本数下限（否则性能结论不可靠）
          tpot_p50_max_ms: 80            # 稳态 TPOT P50 上限（ms/token）
          decode_speed_min: 15           # 稳态解码速度 P50 下限（tokens/s）
          input_throughput_min: 0        # 0 = 不判；依赖 prompt 形状，需要时在 YAML 打开
```

`cache_tiers` 新增字段（均为可选，0 = 不判该项）：

| 字段 | 说明 |
|---|---|
| `tpot_p50_max_ms` | 稳态 TPOT P50 上限（ms/token）；由 CSV `decode_speed` 派生 |
| `decode_speed_min` | 稳态解码速度 P50 下限（tokens/s）；回退到全量 summary 口径 |
| `input_throughput_min` | 全 run input throughput 下限（tokens/s，含 cached token，慎用） |
| `min_samples` | 稳态样本数下限；低于阈值直接违规，不静默通过 |

> 性能口径：这些数字是「前缀缓存命中态」的表观性能，不是冷启动/独占压测性能；
> 与 `perf-sla-v1` 的数字不可直接横比。阈值只是默认值，各部署按被测模型覆盖。

前缀命中率探针（Go 直采）：

- bench 之前对端点发 1 次冷 + 4 次增长请求（同 workload 的 token 形状），
  抓 `usage.prompt_tokens_details.cached_tokens`
- 结果：`cache.probe_rounds[]`（逐轮原始 usage）、`cache.prefix_hit_rate`（稳态）、
  `cache.cache_field_reported`（端点是否上报该字段）
- 产物：`prefix_usage.csv`（round / prompt_tokens / cached_tokens / hit_rate / req_ms）
- 探针失败不阻断 bench

判定（`report.EvaluateCacheGate`）：

- `success_rate < min` → `min_success_rate` 违规
- `ttft_steady_p50_ms > max` → `steady_ttft_p50_max` 违规
- `ttft_improvement < min` → `ttft_improvement_min` 违规（缓存未生效/未配置的强信号）
- `prefix_hit_rate < prefix_hit_rate_min`（默认 0.50）→ `prefix_hit_rate_min` 违规；
  端点未上报 cached_tokens 时给出独立提示（无法确认前缀缓存）
- 稳态样本数 < `min_samples` → `min_samples` 违规（提示延长 duration / 提高 QPS）
- `tpot_p50_ms > tpot_p50_max_ms` → `tpot_p50_max_ms` 违规
- `decode_speed_p50 < decode_speed_min` → `decode_speed_min` 违规
- `input_throughput < input_throughput_min` → `input_throughput_min` 违规
- 性能数据缺失（TPOT / decode speed / throughput 取不到）视为违规，不静默通过
- 未预热 run（`no_warmup=true`）不判 improvement（首轮即冷态，口径不同）
- **多档 run**：以上规则逐档独立判定，违规带 `tier`（如 `c4`）；`tier_summary` 每档一行
  （`p50_max_sec` / `p90_max_sec` = 该档稳态 TTFT P50/P90，单位秒）。
  任一模不通过 → `gate_result.passed=false`；gate 结论不影响 run 状态（照常 `succeeded`/`partial`）

---

## 8. UI

1. 「测试计划」能力切换新增第三项：`性能压测 / 效果评测 / 缓存测试`（来自 `/v1/tools` 聚合）
2. cache 面板：时长、QPS、prompt/历史/输入/输出 token 长度、KV 容量与 tokens/GB、
   预热开关（默认开）、随机种子（高级）
3. 并发档位（方案 B）：`并发档位（session 数）` 逗号分隔（留空=单档，如 `1,2,4`）+
   `每 session QPS` + 负载口径（等比放大 / 固定全局 QPS）；估算框显示档位数、每档 QPS、
   预计总时长/总请求数/token 消耗与容量反算说明；第 2 步会校验档位重复、每档 QPS、
   形状参数（shared+history > 0）
4. 步骤表：单档一行；多档每档一行（`c1`/`c2`/`c4`，并发与请求数事后回填）；
   每行带「操作」：**运行中 → 取消**（kill 当前档位后继续后面的档位）、
   **待执行 → 跳过**（引擎轮到该档时直接跳过），用于剔除个别档位而不用停掉整个 run
5. 结果页：
   - 单档：KPI 卡（前缀命中率、稳态 TTFT P50、首轮 TTFT、改善率、TPOT P50、
     解码速度、输出吞吐、成功率）+ 逐轮 TTFT 表 + 性能指标表 + 口径说明 +
     违规明细 + 产物下载
   - 多档：KPI 卡（档位数、前缀命中率、总请求数、加权 TTFT/TPOT、总吞吐、验收）+
     **并发档位曲线表**（档位 / QPS / 请求数 / 成功率 / 稳态 TTFT P50 / 改善率 /
     TPOT P50 / 解码速度 / 输出吞吐 / 稳态样本）+ 违规明细（带档位列）
6. 历史列表：`能力` 列显示 `缓存测试`；多档显示 `分档 N（最大 M 并发）`，结论列同 eval

---

## 9. 安全与限额

| 项 | 措施 |
|---|---|
| 凭据 | 只经 `OPENAI_API_KEY` 环境变量注入子进程；argv 不含 key；日志回显脱敏沿用 `redactSecrets` |
| 产物 | `bench_summary.json` 的 `config.engine_url` 含 host，不含 key，可直接归档 |
| 限额 | 与 perf/eval 共享 `max_concurrent_runs` / 队列 / 同 target 串行；`duration_sec` 上限 3600；多档总时长 `档数 × (duration+120s)` 不得超过 `max_run_duration`（默认 12h），超限提交即 400 |
| 费用 | MRC 每轮输出 ≤ `output_length` token，总 token 消耗 ≈ `qps × duration × (prefix + output)`，提交前在 UI 预估展示 |

---

## 10. 验收标准（本功能的测试计划）

| 层级 | 用例 |
|---|---|
| 单测 | plan 规范化（duration/qps/token 长度边界、未知 workload → 400） |
| 单测 | CSV 派生：rounds 解析、首轮/稳态口径、失败请求剔除、improvement 计算 |
| 单测 | CSV 派生 TPOT：`decode_speed` 映射 + 时延/输出 token 兜底 |
| 单测 | `bench_summary.json` 性能字段映射（时延分位 / mean decode / 输出吞吐 / TPOTMean） |
| 单测 | 性能 gate：TPOT / 解码速度 / 吞吐 / `min_samples` 违规；数据缺失不静默通过 |
| 单测 | 多档 gate：逐档判定、违规带 `tier`、`tier_summary` 每档一行、TTFT 秒单位 |
| 单测 | 多档聚合：可加项求和、TTFT/解码速度按样本加权、吞吐按总时长 |
| 单测 | 档位 plan 校验：sessions 越界/重复、缺形状参数、总时长超限 → 400 |
| 集成 | builtin 引擎：提交 cache run → SSE → `/results.cache` → artifacts 下载 |
| 集成 | builtin 引擎多档：`ladder` 提交 → `steps_total=N`、`cache.tiers[]`、step 回填、逐档 gate |
| 集成 | 单步操作：跳过待执行档位（skipped）/ 取消运行中档位（canceled）→ 其余档位照常出结果 |
| 集成 | 单步状态冲突：运行中 skip / 待执行 cancel → 409；全部跳过 → run=canceled、`verdict=canceled` |
| 真机 | `multi-round-chat 30s`：step 成功、稳态 TTFT < 首轮、改善率 > 0、CSV/JSON 可下载 |
| 真机 | `cache-perf-sla-v1`：性能指标齐全、样本数不足时正确输出 `min_samples` 违规 |
| 真机 | 多档 `ladder=[1,2,4]`：3 个 step 顺序成功、tiers 指标齐全、产物按 c1/c2/c4 前缀归档 |
| 真机 | `--no-warmup` 冷启动 run：不判 improvement，结果照常产出 |
| 真机 | 端点 401/502：step failed、错误可读、run.log 无明文 key |
| 取消 | 运行中 `POST /cancel`：step canceled、已有 CSV 归档、run 状态正确 |

---

## 11. 实施清单（已完成）

1. `config`：`bench-cache` 根 + `engines.lmcache` + `cache` capability 默认值 + `cache-sla-v1`
2. `domain`：`CapabilityCache`、`Plan` 的 cache 字段、`CacheMetrics` / `CacheRound`、`RunResults.cache`
3. `engine/cache.go`：argv 构造、env 注入、进度/退出处理、summary/CSV 读取
4. `report/cache.go`：CSV → rounds/稳态派生 + `EvaluateCacheGate`
5. `runmgr/cache.go`：`normalizeCachePlan`、汇总、产物归档
6. `httpapi`：results 分支、probe 分支、catalog（cache 无数据集目录，`/catalog/buckets` 不变）
7. `web`：能力切换第三项 + cache 面板 + 结果页渲染
8. 测试：§10 全表

## 11.1 验证记录（真机）

- `multi-round-chat` 60s / 0.5 QPS / 3 session（容量推导）：30/30 请求成功
- Step 数据回填：3 session / 30 请求；逐轮 TTFT 表正常（`rounds[]`）
- gate：`cache-sla-v1` 判定工作正常——该部署预热后首轮 TTFT 即 ~415ms、
  逐轮无显著改善（改善率 -4.8%），正确输出违规
  「TTFT 改善率 -4.8% < 30.0%（前缀缓存未生效或收益不足）」
- 产物归档：`multi-round-chat-bench_results.csv` / `bench_summary.json` /
  `sla_evaluation.json` / `run.log`，均可下载
- 修复记录：① `--engine-url` 必须传剥掉请求路径的 base（lmcache 会给非 /v1
  结尾的 URL 追加 /v1）；② SteadyState 为 nil 时的空指针 panic；
  ③ `UpsertStep` 的 ON CONFLICT 不更新 parallel/number（perf 未暴露，cache 回填 0）

## 11.2 方案 A（v1.1）：性能维度落地

同一份 `multi-round-chat` 负载同时产出「缓存收益 + 稳态性能」，不新增子进程、不改 CLI 调用：

1. `engine/cache.go`：补解析 `bench_summary.json` 的 `mean_decode_speed` / `output_throughput` /
   `p50|p90|p99_request_latency_ms`；从 CSV 第 7 列 `decode_speed` 派生稳态 TPOT（缺失时用
   `(request_latency - ttft) / (num_output_tokens - 1)` 兜底）与稳态解码速度
2. `domain`：`CacheMetrics` 增加 `mean_decode_speed` / `output_throughput` / `tpot_mean_ms` /
   `request_latency_p50|p90|p99_ms`；`CacheSteadyStats` 增加 `tpot_p50|p90_ms`
   （`decode_speed_p50` 由 CSV 稳态填充，原来为空）
3. `runmgr/cache.go`：`summary` 增加性能段（稳态 TTFT P90/P99、TPOT、解码速度、
   in/out 吞吐、时延分位、`samples_steady`），旧字段不变
4. `config`：`CacheGateTier` 增加 `tpot_p50_max_ms` / `decode_speed_min` /
   `input_throughput_min` / `min_samples`；新增内置模板 `cache-perf-sla-v1`
   （`cache-sla-v1` 行为不变，向后兼容）
5. `report/gate.go`：`EvaluateCacheGate` 增加性能与最小样本判定；性能数据缺失视为违规
6. `web`：结果页 KPI 8 张（新增 TPOT P50 / 解码速度 / 输出吞吐）+ 性能指标表 +
   「前缀缓存命中态」口径说明；计划页估算框与确认页显示预计请求数
7. 测试：CSV TPOT 派生（含兜底）、summary 性能字段映射、性能 gate 规则；§10 全表回归

## 11.3 方案 B（v1.2）：并发档位落地

plan 增加 `ladder` 多档，一次 run 顺序跑完，输出「TTFT/TPOT/吞吐 vs 并发」曲线：

1. `domain`：`Plan.CacheLadder []CacheTier`（sessions / qps / 规范化回填的 kv 容量）；
   `CacheMetrics` 增加 `bucket` / `sessions` / `qps` / `tiers[]`（每档完整指标，元素不嵌套）
2. `httpapi`：plan DTO 透传 `ladder`
3. `runmgr/cache.go`：`expandCacheLadder` 校验（≤20 档、sessions 1..10000、不重复、qps 继承）
   + 反算每档 KV 容量（`ceil(sessions × 单会话 token / tokens_per_gb)`）+ 总时长上限校验；
   `reportCache` summary 增加 `tiers_count` / `max_sessions`，gate 逐档判定
4. `engine/cache.go`：`Run` 改为多档循环（每档一个子进程、独立 `cache/<bucket>` 目录、
   独立 `--export-config` 与产物前缀），探针整 run 一次；`aggregateCacheTiers` 聚合顶层指标
5. `report/gate.go`：`EvaluateCacheGate` 识别 `m.Tiers` 逐档判定，违规带 `tier`，
   `tier_summary` 每档一行；单档行为不变
6. `engine/fake.go`：builtin 引擎按档位模拟（每档独立 step），用于 UI 联调与集成测试
7. `web`：计划页并发档位输入（每 session QPS / 负载口径）、预估与校验、确认页；
   结果页多档曲线表（带档位违规列）；历史列表显示分档
8. 测试：plan 校验、多档展开/回填、聚合口径、逐档 gate、builtin 多档全链路（§10 全表）

## 12. 后续（v1.3）

1. 其它 workload：`long-doc-qa`（多查询共享长文档）、`prefix-suffix-tuner`（前缀击穿）、
   `kv-tier-pressure`（容量压力 + Zipf 热点）——plan.workload 已预留
2. 跨 run 缓存收益趋势（同 target 多次 run 的 steady TTFT 曲线，可按档位对齐）
3. 对接 `lmcache bench trace-analyze`：被测端开 token trace 后做命中归因
4. 双跑对账：Go 派生的 steady/rounds 指标与 Python 侧（若有）核对
