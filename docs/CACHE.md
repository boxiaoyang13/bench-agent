# 缓存测试（cache）接入设计

> 目标：把 LMCache 的 `lmcache bench engine --workload multi-round-chat` 接进 Go 服务端，
> 作为与 perf / eval 并列的第三种能力（`capability=cache`），复用现有
> Target / Run / Step / Artifact / SSE / 取消 / 报告下载协议。
>
> 状态：v1 已实现并真机端到端验证通过（§11 清单全部完成；真机结论见文末「验证记录」）。
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

| 做（v1） | 不做（v1） |
|---|---|
| multi-round-chat 单 workload，一次 run 一个 step | 不接 kv-tier-pressure / long-doc-* 等其它 workload（协议留 `workload` 字段） |
| 复用现有 Target（OpenAI 兼容端点 + 凭据） | 不管理 LMCache server 本身（`lmcache server` 的部署/配置属被测环境） |
| bench_summary.json → 结构化指标；CSV 逐条归档 | 不在 Go 侧重算逐请求指标（以 Python 落盘为准） |
| 缓存收益分析：首轮 vs 稳态 TTFT 对比（从 CSV 派生） | 不做 token 级命中归因（需要被测端开 token trace，另用 `bench trace-analyze`） |
| `cache-sla-v1` 验收：稳态 TTFT 阈值 + 改善率 | 不做跨 run 的缓存收益趋势对比（v1.1） |

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
  "ignore_eos": false
}
```

规范化规则：

1. `workload` 缺省 `multi-round-chat`；v1 只接受这一个值，其它 → `400`
2. `duration_sec` ∈ [10, 3600]；`qps` ∈ (0, 100]
3. token 长度参数 ≥ 1 且 ≤ 1M；`kv_cache_volume_gb` > 0；`tokens_per_gb_kvcache` ∈ [1024, 10M]
4. `no_warmup` 用于冷启动对比 run（正常验收 run 必须预热）
5. **step = workload 执行一次**；`steps_total = 1`（v1）；`run.max_run_duration` 需覆盖
   `duration_sec + 预热 + 余量`（默认 12h 上限内取 `duration×3 + 600s` 与上限的较小者）

### 3.3 Step 与 Run 结果

Step：`bucket = "multi-round-chat"`、`number = 请求数（事后回填）`、`parallel = 并发 session 数（事后回填）`。

`GET /v1/runs/{id}/results`（`capability=cache`）：

```json
{
  "run_id": "run_...",
  "capability": "cache",
  "summary": {
    "verdict": "pass",
    "total_requests": 15, "successful_requests": 15, "success_rate": 1.0,
    "ttft_first_round_ms": 1055.0, "ttft_steady_p50_ms": 447.7,
    "ttft_improvement": 0.576,
    "mean_decode_speed": 138.3, "input_throughput": 2468.7
  },
  "cache": {
    "workload": "multi-round-chat",
    "num_sessions": 3,
    "steady_state": {
      "ttft_p50_ms": 447.7, "ttft_p90_ms": 539.8, "ttft_p99_ms": 984.9,
      "request_latency_p50_ms": 1375.5, "decode_speed_p50": 134.0
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
- 失败请求（`successful=False`）不进 TTFT 统计，计入 `success_rate`

---

## 4. 执行流程

```
POST /v1/runs (tool=lmcache, capability=cache)
  └─ normalizeCachePlan → steps[1]
  └─ 入队（与 perf/eval 共享并发上限、同 target 串行）

worker:
  ├─ 解密 target 凭据
  ├─ spawn: lmcache bench engine ... --output-dir <runDir>/cache/multi-round-chat
  │    env: OPENAI_API_KEY=<target key>，PATH 前置 .venv/bin
  ├─ stdout → 日志 + SSE；进度解析（MRC 无逐请求输出，v1 只推阶段事件）
  ├─ 进程结束 → 读 bench_summary.json + bench_results.csv
  ├─ 派生 rounds / first-round / steady 指标 → run.summary + metrics.json
  └─ 归档 artifacts → run.succeeded/failed
```

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
├── metrics.json                     # { "cache": {...} }（§3.3 结构）
├── logs/<run_id>-cache.log          # 引擎原始输出（脱敏）
├── cache/multi-round-chat/          # --output-dir（原始）
│   ├── bench_results.csv
│   └── bench_summary.json
└── artifacts/
    ├── bench_results.csv            # 逐请求 11 列（重命名为 mrc-bench_results.csv）
    ├── bench_summary.json           # mrc-bench_summary.json
    ├── resolved_config.json         # --export-config 导出的最终生效参数
    └── run.log
```

artifact kinds：`bench_csv`、`bench_summary`（json）、`bench_config`（json）、`run_log`。

---

## 7. 验收（cache-sla-v1）

```yaml
gates:
  profiles:
    cache-sla-v1:
      capability: cache
      description: "多轮对话前缀缓存：稳态 TTFT 下限 + 首轮改善率"
      cache_tiers:
        - steady_ttft_p50_max_ms: 800     # 稳态 TTFT P50 上限
          ttft_improvement_min: 0.30      # 稳态相对首轮至少改善 30%
          success_rate_min: 0.99
```

判定（`report.EvaluateCacheGate`）：

- `success_rate < min` → `min_success_rate` 违规
- `ttft_steady_p50_ms > max` → `steady_ttft_p50_max` 违规
- `ttft_improvement < min` → `ttft_improvement_min` 违规（缓存未生效/未配置的强信号）
- 未预热 run（`no_warmup=true`）不判 improvement（首轮即冷态，口径不同）

---

## 8. UI

1. 「测试计划」能力切换新增第三项：`性能压测 / 效果评测 / 缓存测试`（来自 `/v1/tools` 聚合）
2. cache 面板：时长、QPS、prompt/历史/输入/输出 token 长度、KV 容量与 tokens/GB、
   预热开关（默认开）、随机种子（高级）
3. 步骤表：单行（workload 名 / 时长 / 状态 / 稳态 TTFT / 改善率 / 耗时）
4. 结果页：KPI 卡（稳态 TTFT P50、首轮 TTFT、改善率、成功率）+ 逐轮 TTFT 表 +
   违规明细 + 产物下载
5. 历史列表：`能力` 列显示 `缓存测试`，结论列同 eval

---

## 9. 安全与限额

| 项 | 措施 |
|---|---|
| 凭据 | 只经 `OPENAI_API_KEY` 环境变量注入子进程；argv 不含 key；日志回显脱敏沿用 `redactSecrets` |
| 产物 | `bench_summary.json` 的 `config.engine_url` 含 host，不含 key，可直接归档 |
| 限额 | 与 perf/eval 共享 `max_concurrent_runs` / 队列 / 同 target 串行；`duration_sec` 上限 3600；`max_run_duration` 动态收紧 |
| 费用 | MRC 每轮输出 ≤ `output_length` token，总 token 消耗 ≈ `qps × duration × (prefix + output)`，提交前在 UI 预估展示 |

---

## 10. 验收标准（本功能的测试计划）

| 层级 | 用例 |
|---|---|
| 单测 | plan 规范化（duration/qps/token 长度边界、未知 workload → 400） |
| 单测 | CSV 派生：rounds 解析、首轮/稳态口径、失败请求剔除、improvement 计算 |
| 集成 | builtin 引擎：提交 cache run → SSE → `/results.cache` → artifacts 下载 |
| 真机 | `multi-round-chat 30s`：step 成功、稳态 TTFT < 首轮、改善率 > 0、CSV/JSON 可下载 |
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

## 12. 后续（v1.1）

1. 其它 workload：`long-doc-qa`（多查询共享长文档）、`prefix-suffix-tuner`（前缀击穿）、
   `kv-tier-pressure`（容量压力 + Zipf 热点）——plan.workload 已预留
2. 跨 run 缓存收益趋势（同 target 多次 run 的 steady TTFT 曲线）
3. 对接 `lmcache bench trace-analyze`：被测端开 token trace 后做命中归因
4. 双跑对账：Go 派生的 steady/rounds 指标与 Python 侧（若有）核对
