# THVV Server 设计文档

> 目标：把现有的 Python 工具链（`thvv/`）封装成**可被外部客户端通过 HTTP 驱动的 Go 服务**，
> 同时保留后续接入**其他工具**（第三方压测/评测/红队/功能测试）的扩展能力。
> 对应方案：**方案 A（Go 薄封装）**——Go 负责编排、状态机、存储、报告聚合；Python 只作为被驱动的执行引擎。
>
> 配套文档：[`API.md`](./API.md)（协议规范）、[`openapi.yaml`](./openapi.yaml)（机器可读契约）

---

## 1. 为什么是"Go 薄封装 + 引擎进程"

现有资产盘点：

| 现有实现 | 位置 | 搬迁策略 |
|---|---|---|
| 档位定义（token 区间） | `perf/run.sh: bucket_range` | **Go 常量表**（`/v1/catalog/buckets` 直接输出） |
| 档位并发上限 | `perf/run.sh: bucket_max_parallel` | Go 常量表 |
| 档位补充并发 | `perf/run.sh: bucket_extra_parallels` | Go 常量表 |
| 并发 → 请求数映射 | `perf/run.sh: parallel_number` | Go 常量表 |
| tokenizer 自动推断 | `perf/run.sh: auto_detect_tokenizer` | Go 映射表（20 行 switch） |
| temperature 自适应 | `perf/run.sh: TEMPERATURE` 逻辑 | Go 函数 |
| TPM 预算裁剪并发 | `perf/run.sh: cmd_bench_all` | Go 函数（`tpm_par = budget / bucket_tokens`） |
| 成功率早停 | `perf/run.sh: SUCCESS_RATE_MIN` | Go 函数 |
| 压测执行 | `evalscope perf` | **fork 子进程**（不改引擎） |
| tokenizer 计数/过滤 | `transformers` | 引擎进程内，Go 不管 |
| HTML 报告 | `gen_perf_dashboard.py` (571 行) | **Go 重写**（读 SQLite → `html/template`） |
| Excel 报告 | `gen_report_from_db.py` (750 行) | **Go 重写**（`excelize`，45 列） |
| SLA 判定 | `sla_eval.py` (411 行) | **Go 重写**（纯算术，阈值表见 catalog） |
| 失败明细导出 | `export_failure_details.py` | Go 重写（读同一个 `result` 表，映射 `/results/failures`） |
| 效果评测 | `eval/run_eval.py` | **v1.1 fork 子进程**（同机制，接口已预留） |
| 数据集 | `perf/datasets*/` | 原样挂载只读卷 |

**为什么报告层用 Go 重写而不是 fork？**
- `benchmark_data.db` 是稳定的数据契约（SQLite，表 `result`），字段全在 `gen_report_from_db.py:184-260`；
- 报告是**纯计算 + 序列化**，无 ML 依赖，Go 侧 `modernc.org/sqlite`（纯 Go，无 CGO）+ `excelize`（纯 Go）即可；
- 好处：`/results` 结构化指标与 HTML/xlsx 共用同一份 Go 计算代码 → **数字天然一致**，不会再出现 Python 两个脚本口径漂移；且 `run` 结束后报告生成是毫秒级，不需要"reporting"阶段跑 Python。

**为什么引擎仍然 fork Python？**
- `evalscope perf` 是外部依赖（不是我们的代码），重写成本高、口径风险大；
- tokenizer 计数（`transformers` + 分词器区间过滤）是压测的前置计算，Go 侧实现需要 CGO/离线 manifest，v1 不做。

> **演进路径已预留**：`Engine` 是 Go 接口，`thvv-python` 只是其中一个实现。后续可加 `thvv-native`（纯 Go HTTP 压测）、`acme-tool`（HTTP 调用第三方服务），对协议零影响。

---

## 2. 架构

```
                        ┌──────────────────────────────────────────────┐
 外部客户端             │              thvv-server (Go)                │
 ───────────            │                                              │
  curl / CI / 平台  ───► │  HTTP API  ──►  Run Manager (状态机/队列)     │
                        │      │                │                      │
                        │      │                ├──► Engine Registry   │
                        │      │                │       ├─ thvv-python ─┼──► fork: evalscope perf
                        │      │                │       ├─ thvv-eval  ──┼──► fork: run_eval.py
                        │      │                │       └─ (future: 第三方工具, http)
                        │      │                │                      │
                        │      │                └──► Runner (step 调度) │
                        │      │                         │              │
                        │      ├──► SSE Hub (进度广播)                   │
                        │      │                                        │
                        │  Store (SQLite/Postgres)                      │
                        │      ├─ targets / runs / steps / artifacts     │
                        │      └─ run_events (SSE 重放缓冲)               │
                        │                                              │
                        │  Report (Go)                                  │
                        │      ├─ sqlite reader ──► metrics              │
                        │      ├─ html/template ──► html_report          │
                        │      ├─ excelize      ──► xlsx_report          │
                        │      └─ gate (SLA)    ──► sla_json             │
                        │                                              │
                        │  Artifact Store (本地卷 / S3)                  │
                        └──────────────────────────────────────────────┘
```

### 2.1 分层

| 层 | 包 | 职责 | 依赖 |
|---|---|---|---|
| L1 传输 | `internal/httpapi` | 路由、DTO、参数校验、problem+json、SSE 编码 | L2 |
| L2 应用 | `internal/run` | Run 状态机、队列、取消、回调、进度聚合 | L3,L4 |
| L3 引擎 | `internal/engine` | `Engine` 接口 + 各工具适配器（进程管理、日志解析、退出码映射） | L5 |
| L4 报告 | `internal/report` | SQLite → metrics → html/xlsx/gate | L5 |
| L5 领域 | `internal/domain` | Target/Run/Step/Artifact 结构、档位常量表、tokenizer 推断、纯函数 | 无 |
| L6 存储 | `internal/store` | 仓储接口 + SQLite 实现（后续可换 Postgres） | L5 |

> 关键约束：**L4/L5 不得 import L3**（报告与引擎解耦），**L3 不得 import L4**。这样"Go 重写引擎"和"Python 报告"两条演进路径都保持可行。

### 2.2 目录骨架

```
server/
├── cmd/thvv-server/main.go
├── internal/
│   ├── httpapi/    {router.go, targets.go, runs.go, sse.go, errors.go, dto.go}
│   ├── run/        {manager.go, state.go, queue.go, events.go, webhook.go}
│   ├── engine/     {engine.go, registry.go, thvv_python.go, parse.go, proc_unix.go, proc_windows.go}
│   ├── report/     {metrics.go, failures.go, html.go, xlsx.go, gate.go, tpl/*.gohtml}
│   ├── domain/     {target.go, run.go, buckets.go, tokenizer.go, temperature.go}
│   └── store/      {store.go, sqlite/*.go, migrations/*.sql}
├── deploy/         {Dockerfile, docker-compose.yml, k8s/}
└── docs/           {API.md, DESIGN.md, openapi.yaml}
```

---

## 3. 核心抽象：Engine 接口（多工具接入的落点）

```go
package engine

// Capability 是协议层的固定枚举；工具可以只实现其中一部分。
type Capability string

const (
    CapPerf       Capability = "perf"
    CapEval       Capability = "eval"
    CapFunctional Capability = "functional"
)

// Spec 已由协议层规范化（默认值填充、校验完成），引擎只消费不判断。
type Spec struct {
    RunID      string
    Capability Capability
    Target     domain.Target      // 已解密出 api_key
    Plan       domain.Plan        // 含 ToolParams json.RawMessage
    WorkDir    string             // 引擎可写的独立目录
    DatasetsDir string
}

// Event 是引擎 → Run Manager 的单向事件流（对应 SSE 事件）。
type Event struct {
    Kind     string        // step.started | step.progress | step.finished | log | error
    StepIndex int
    Bucket   string
    Parallel int
    Number   int
    Data     json.RawMessage
    At       time.Time
}

type Engine interface {
    Name() string
    // Describe 用于 GET /v1/tools
    Describe() Description          // capabilities + plan_schema + artifacts
    // Probe 用于 POST /v1/targets/{id}/probe
    Probe(ctx context.Context, t domain.Target, timeout time.Duration) (ProbeReport, error)
    // Validate 校验 plan 的工具私有字段（JSON Schema）
    Validate(cap Capability, plan domain.Plan) []FieldError
    // Run 执行；事件通过 ch 推送；ctx 取消必须能杀掉子进程
    Run(ctx context.Context, spec Spec, ch chan<- Event) (Result, error)
}

type Result struct {
    Steps     []domain.StepResult   // 引擎自报的每步结果（可空，Go 侧会从 DB 重算）
    RunDir    string                // 产物根目录
    DBPaths   []string              // benchmark_data.db 列表
    ExitCode  int
    Err       error
}
```

**注册**（`internal/engine/registry.go`）：

```go
// 编译期内置 + 配置期动态（配置文件声明外部工具，kind=http）
type Registration struct {
    Name    string
    Kind    string            // "builtin" | "http"
    BaseURL string            // kind=http 时，转发 POST /runs 与 SSE
    Enabled bool
    Scheme  json.RawMessage   // plan/result JSON Schema
}
```

三条接入路径，均**不需要改协议**：

| 接入方式 | 适用 | 实现 |
|---|---|---|
| 内置 fork 进程 | 我们自己维护的 Python 脚本（thvv-thvv/evalscope） | 实现 `Engine` 接口 |
| HTTP 外部工具 | 腾讯其他工具、第三方 SaaS | 配置 `kind=http`，服务端做**反向代理 + 事件翻译** |
| 编译期插件 | 自研 Go 引擎 | 实现 `Engine` + `init()` 注册 |

外部工具（`kind=http`）的契约（**最小可对接集**，写进 `docs/TOOL-PROTOCOL.md`，v1.1 出）：

```
POST {base_url}/runs        ← {run_id, capability, target, plan}  → {tool_run_id}
GET  {base_url}/runs/{id}/events (SSE)  → 与 THVV 事件同结构
GET  {base_url}/runs/{id}/results       → {metrics, artifacts[]}
POST {base_url}/runs/{id}/cancel
POST {base_url}/probe                   → ProbeReport
```

> 这就是"我还想接入其他工具"的答案：**任何能吐出「事件流 + 指标 JSON + 产物文件」的工具，改一份配置即可挂上**。

---

## 4. Run Manager：状态机与调度

### 4.1 队列与并发

- 全局 `max_concurrent_runs`（默认 4，见 `/v1/meta.limits`）。超出进 `queued`，FIFO，同 target 串行（避免自相干扰污染压测结果）。
- 每个 run 独占 `WorkDir = {data_dir}/runs/{run_id}`，内部结构沿用现约定以保证 Python 脚本可无缝复用：
  ```
  runs/run_01JB7R2K/
    ├── results/perf-1k-20261105-0843/{vendor}/parallel_10_number_200/benchmark_data.db
    ├── artifacts/性能测试报告.html
    ├── artifacts/性能测试报告.xlsx
    └── logs/run.log
  ```
  → **目录约定与 `gen_perf_dashboard.py:find_dbs` 完全一致**，即使将来回退到 Python 脚本生成报告也能直接跑。

### 4.2 状态推进（Go 侧实现）

```
queued
  └─► preparing:  ① engine.Validate(plan)
                  ② 数据集存在性校验（built_in=false 的档位缺失 → dataset_missing）
                  ③ tokenizer 缓存检查（未命中 → 交给引擎 probe/预下载）
                  ④ 创建 WorkDir、落审计记录
  └─► running:    按 step 调度（见 4.3）
  └─► reporting:  metrics 重算 → gate → html/xlsx/csv 生成 → artifact 落库
  └─► succeeded | partial | failed
```

`reporting` 因为改成 Go 实现，通常是秒级，但保留独立阶段以便将来挂重报告（如视频评测）。

### 4.3 Step 调度（对齐 `cmd_bench_all` 的语义）

```
for bucket in plan.buckets:
    if !datasetExists(bucket): record skip(dataset_missing); continue
    maxPar = min(catalog.max_parallel[bucket], tpmBudget/bucketTokens(bucket) or ∞)
    parallels = plan.ladder(filtered ≤ maxPar) ∪ plan.bucket_overrides[bucket].parallels
    parallels = sortedUnique(parallels)
    if empty: record skip(budget_too_low); continue
    for p in parallels:
        n = override.N_<bucket> ?? plan.number_by_parallel[p] ?? 100
        if tpmBudget > 0 && no explicit N: n = min(n, tpmBudget/bucketTokens(bucket))
        emit step.started
        engine.Run(stepSpec) → 流式事件
        sr = readSuccessRate(benchmark_summary.json)
        emit step.finished
        if !summaryExists:  n_fail++; if plan.stop_on_failure: break
        if plan.success_rate_min > 0 && sr < plan.success_rate_min: break  // 早停
    sleep(plan.cooldown_sec)
```

每一步的 `bucket/parallel/number` 与响应中的 `steps_total` **在 `202` 时即可精确算出**（因为并发裁剪是纯函数），所以客户端进度条不会跳变。

### 4.4 取消

`cancel` → `ctx.Cancel()` → 引擎适配器 `SIGTERM`（10s）→ `SIGKILL`。
子进程用 `posix_spawn` 且 `Setpgid: true`，杀**进程组**——`evalscope` 会 fork 子进程，只杀父进程会漏。
取消后：保留已完成的 step 结果，状态置 `canceled`，仍允许 `POST /reports`（把已跑出的部分出成报告）。

---

## 5. 报告层（Go）

### 5.1 数据来源与计算

读 `benchmark_data.db` 的 `result` 表（字段：`success, first_chunk_latency, latency, time_per_output_token, inter_token_latencies, prompt_tokens, completion_tokens, start_time, completed_time, ...`）。

计算清单（逐条从 `gen_report_from_db.py:184-260` 移植）：

| 指标 | 口径（**必须一致**） |
|---|---|
| 成功率 | `success==1` / `total` |
| TTFT | 成功请求的 `first_chunk_latency`，avg/min/max/p50/p90/p95/p99 |
| TTLT | `latency`，avg |
| TPOT | **`time_per_output_token`（evalscope 写入，单位秒）**，每请求一个值取均值 |
| ITL | **`inter_token_latencies`（JSON 数组，相邻流式 chunk 间隔）按 chunk 汇总** ← 与 TPOT 不同口径 |
| output_tps | `Σcompletion_tokens / (max(completed_time) - min(start_time))` |
| input_tpm / output_tpm / tpm | `tps * 60` 各分量相加 |
| qpm | `successCount / window` |
| TPM 滑窗趋势 | 60s 窗口、50% 重叠的离线近似（`_compute_token_trends`） |
| 分位数 | 与 Python 一致的线性插值，**用于历史报告可比性，不要换成 nearest-rank** |
| average token rate（SLA 用） | `completion_tokens / (latency - ttft)`，仅 `ct>=5 && gen_t>=0.1` |

> 明确写进代码注释与文档：**ITL ≠ TPOT**。这是原脚本特意强调的点（`gen_report_from_db.py:229`），也是最容易在重写时踩的坑。

### 5.2 Gate（SLA 判定，Go 重写 `sla_eval.py`）

- `classify_tier(avg_prompt_tokens)` → 6 档（`<4K…<256K`），阈值来自 `/v1/catalog/gates`
- 判定：每 step 的 `p50_ttfb <= tier.p50` 且 `p90_ttfb <= tier.p90`；全局 `output_tokens_per_sec > 30`
- 成功率不足的 step **不参与档位合并统计**（否则污染 P50/P90）——原脚本 `sla_eval.py:145` 的行为要保留
- 输出与 `sla_evaluation.json` 同结构，落成 `sla_json` artifact

### 5.3 产物

| kind | 生成器 | 说明 |
|---|---|---|
| `html_report` | `html/template`（模板从 `gen_perf_dashboard.py` 的 `render()` 移植） | 单档标题 = `{model} 性能压测报告 (bucket)`；矩阵 = `(全档位 {ts})` |
| `xlsx_report` | `excelize` | 「性能指标」45 列 + 「失败请求详情」sheet；`--no-tpm-html` 语义保留（不内嵌 TPM 图） |
| `failure_csv` | `encoding/csv` | 失败请求明细，长文本截断规则沿用（`…[已截断，完整 N 字符，见 benchmark_data.db]`） |
| `sla_json` | `encoding/json` | SLA 判定结果 |
| `raw_db` | 直接拷贝 | 原始 `benchmark_data.db`，审计留证 |
| `run_log` | 引擎 stdout | **落盘即脱敏**：`api_key` 正则替换为 `sk-***` |

---

## 6. 存储

### 6.1 表结构（SQLite 起步，预留 Postgres）

```sql
targets(target_id PK, tenant_id, name UNIQUE, vendor, api_url, protocol, model,
        api_key_enc BLOB, api_key_hint, tokenizer, tokenizer_source,
        defaults_json, labels_json, health_json,
        created_at, updated_at, deleted_at)

runs(run_id PK, tool, capability, target_id, target_snapshot_json, ephemeral,
     plan_json, plan_normalized_json, gate_json,
     status, phase, steps_total, steps_done, percent,
     exit_code, error_json, gate_result_json, summary_json,
     idempotency_key UNIQUE, labels_json, created_by,
     created_at, started_at, finished_at, canceled_at)

steps(run_id, step_index, bucket, parallel, number, status,
      success_rate, ttft_p50_sec, output_tps, duration_sec, db_path,
      PRIMARY KEY(run_id, step_index))

artifacts(artifact_id PK, run_id, kind, filename, content_type,
          size_bytes, sha256, storage_path, created_at)

run_events(run_id, seq, event, data_json, at, PRIMARY KEY(run_id, seq))  -- 只保留最近 1000

callbacks(run_id, url, status, attempts, last_error, next_attempt_at)
```

设计要点：

- `target_snapshot_json` + `plan_normalized_json`：**run 是自包含的**。target 后续被 PATCH/DELETE 不影响历史 run 的可解释性与可复现性。
- `api_key_enc`：32 字节 `AES-GCM`，key 来自 `THVV_SECRET_KEY`（env/KMS）。**或**改存引用（`secret_ref`），由外部 Vault 解引用——见 §10 待决。
- `idempotency_key UNIQUE`：24h 内同 key + 同 body hash → 返回原 run；同 key + 不同 body → `409 conflict`。
- `run_events` 环形清理：终态后保留 24h。

### 6.2 Artifact 存储

- v1：本地卷 `{data_dir}/runs/{run_id}/artifacts/`，DB 存相对路径；
- v2：S3 兼容对象存储，`storage_path` 换成 `s3://`，下载走 302 预签名（协议不变，`/v1/artifacts/{id}` 仍是稳定入口）。

---

## 7. 安全

| 项 | 措施 |
|---|---|
| 凭据不回显 | `api_key` 只写；响应仅 `api_key_hint`（后 4 位）；`target_snapshot` 落库前剔除 key |
| 日志脱敏 | 引擎 stdout/stderr 落盘前正则脱敏（`sk-[A-Za-z0-9]{8,}`、`Authorization: Bearer …`） |
| 命令注入 | **不用 shell**，`exec.Command` 直传 argv；`plan` 所有值白名单化（bucket 必须命中 catalog、number/parallel 有上限） |
| 路径穿越 | `WorkDir` 由服务端生成；`dataset_dir` 只接受枚举；`title/client_name` 进 HTML 时 `html/template` 自动转义，进文件名时白名单化 |
| 资源限制 | 单 run 最大时长（默认 12h）、单 run 最大产物大小、`max_tokens`/`number`/`parallel` 上限校验 |
| 审计 | 每个写操作落 `audit_log`（who/when/what/request_id），run 的开跑与产物齐全性可追溯 |
| 网络 | 支持 `THVV_ALLOW_PRIVATE_TARGETS=false` 拒绝内网地址（SSRF 防护，默认开启，允许通过白名单放行） |

---

## 8. 部署

```yaml
# docker-compose.yml（镜像内置 Python 引擎，Go 单二进制为 entrypoint）
services:
  thvv:
    image: thvv/server:1.0.0        # Go 静态二进制 + python3 + evalscope[perf]
    ports: ["8080:8080"]
    environment:
      THVV_SECRET_KEY: ${THVV_SECRET_KEY}
      THVV_DATA_DIR: /data
      THVV_MAX_CONCURRENT_RUNS: "4"
      # 共享缓存：tokenizer 只需下载一次
      HF_HOME: /data/cache/huggingface
      MODELSCOPE_CACHE: /data/cache/modelscope
    volumes:
      - ./data:/data
      - ../perf/datasets_clean:/app/thvv/perf/datasets:ro   # 数据集只读挂载
    healthcheck:
      test: ["CMD", "/app/thvv-server", "healthcheck"]
      interval: 30s
```

**镜像分层**（关键：Python 依赖层稳定，Go 层频繁变）

```
FROM python:3.11-slim AS py
  pip install -r thvv/perf/requirements.txt      # ← 最重的一层，很少变
FROM py
  COPY --from=golang:1.23 /app/thvv-server /app/   # ← Go 静态二进制
  COPY thvv/ /app/thvv/                            # 仅当 Python 脚本变更时重建
```

> 因为报告改由 Go 生成，`openpyxl`/`matplotlib` 两个依赖可从镜像里去掉（openpyxl 仅 `gen_report_from_db.py` 用；matplotlib 仅 TPM 图用）。`transformers`/`modelscope` 仍在（引擎侧 tokenizer 计数需要）。

### 8.1 单二进制分发（可选）

Go 静态二进制 + `--engine=thvv-python` 时若无 Python 则自动降级：`/v1/tools` 里 `perf` 标 `unavailable`，`/v1/runs` 返回 `501`。适合只做报告聚合/查询的轻量部署。

---

## 9. 实施计划

| 阶段 | 交付 | 估时 | 验收 |
|---|---|---|---|
| **M0 骨架** | `domain` 常量表 + `store` 迁移 + `/v1/health` `/v1/meta` `/v1/tools` `/v1/catalog/*` + problem+json | 3d | 契约测试通过，catalog 数字与 `run.sh` 逐项对齐 |
| **M1 Target** | `POST/GET/PATCH/DELETE /targets` + `/probe` + tokenizer 推断 + 凭据加密 | 4d | probe 能真实打到端点并回 TTFT |
| **M2 单档** | `POST /runs`(single) + 队列 + 状态机 + `thvv-python` 引擎(perf single) + SSE | 1w | `1k/10/200` 端到端跑通，SSE 进度与 `run.log` 一致 |
| **M3 矩阵** | matrix 编排（ladder/override/TPM 裁剪/早停/冷却）+ 取消 | 1w | `bench-all` 结果与 `bash run.sh bench-all` 同参数下**指标误差 <0.1%** |
| **M4 报告** | `report/metrics.go` + gate + html + xlsx + failures + artifacts 下载 | 1.5w | 同一 `benchmark_data.db` 下，Go 出的 45 列与 Python 版**逐格一致** |
| **M5 生产化** | 鉴权/RBAC、幂等、webhook、审计、限流、Docker、K8s、压测自测 | 1w | 并发 4 run、48h 稳定性、100 档位回归 |
| **M6 eval** | `capability=eval`（fork `run_eval.py`）+ eval 报告 | 1w | `aime26 --limit 30` 端到端 |
| **M7 多工具** | `kind=http` 外部工具代理 + `TOOL-PROTOCOL.md` | 1w | 接入一个 mock 第三方工具全程跑通 |
| **M8 functional** | 功能测试引擎（原生 Go：tool_call / 长上下文 / 结构化输出） | 2w | 协议不变，仅新增 capability 实现 |

总估时 **~9 周**（M0-M5 为 v1.0，M6-M8 为 v1.1/v1.2）。

### 9.1 回归保障（最重要的一条）

**双跑对账**是 M3/M4 的硬门槛：同一 target、同一数据集，分别用
`bash run.sh bench <bucket> <N> <P>` 与 `POST /v1/runs`（等价 plan）跑，
要求 `/results.metrics.per_step` 与原 `性能测试报告.xlsx` 的所有数值列**完全一致**（浮点允许 1e-6 相对误差）。
差异必须能解释到具体口径（如时间窗口边界、分位数插值），否则不改结论只改文档。

---

## 10. 待决问题

1. **凭据存储**：自建 AES-GCM vs 只存 `secret_ref`（Vault/K8s Secret）。倾向后者，减少密钥管理责任。→ 影响 `targets.api_key_enc` 字段。
2. **数据库**：SQLite（单机、简单）vs Postgres（多副本、并发写）。v1 用 SQLite + WAL，`store` 接口抽象留切换点。
3. **是否保留 Python 报告脚本**：建议保留一段时间作为"黄金样本"生成器（回归对账用），但不再作为线上路径。
4. **多租户**：当前用 `labels` + RBAC 粗粒度；若要做供应商侧只读门户，需引入 `tenant_id` 与行级过滤。
5. **`/runs?wait=true` 同步等待**：便利性 vs 连接占用。倾向支持，上限 10 分钟，超时返回 `202`。
6. **历史结果导入**：`perf/results/` 已有产物是否要 `POST /v1/imports` 纳管（跨版本对比需要）。
7. **组合 run**：`capability: ["perf","eval"]` 一次提交跑两套，共享 target/报告。倾向 v1.2。
