# THVV Server HTTP 协议规范 v1

> 服务端：Go（方案 A：Go 薄封装，内部 fork Python `evalscope` 引擎）
> 消费者：任意外部客户端（供应商验收平台、CI、QCI、门禁脚本、Web 前端）
> 状态：草案 v1.0 — 待评审
> 机器可读契约：[`openapi.yaml`](./openapi.yaml)

---

## 0. 设计原则

| 原则 | 落地方式 |
|---|---|
| **被测对象资源化** | 模型/地址/Key/协议/tokenizer 打包成一个 `Target`，后续所有测试引用 `target_id`，不必每次重传凭据 |
| **执行一律异步** | `bench-all` 单次可能跑数小时（1m 档 + 120s 档间冷却），`POST /v1/runs` 立即返回 `run_id`，进度用 SSE |
| **能力（capability）与工具（tool）解耦** | 协议只认 `tool` + `capability` + `plan`，`thvv` 是第一个工具，后续接入其他工具/自研引擎不需要改协议 |
| **预留但不空转** | `eval` / `functional` 的端点、状态机、结果结构现在就有，未实现的返回 `501 not_implemented` + `available_at` 提示 |
| **凭据只进不出** | `api_key`（target 与 `plan.judge`）写入后只回 `api_key_hint`（后 4 位），任何 GET 响应体与日志中不出现明文 |
| **结果与协议解耦** | 结构化指标走 `/results`（JSON），交付物走 `/artifacts`（HTML/xlsx/CSV 下载），两者都从 `benchmark_data.db` 派生 |
| **幂等** | `POST /v1/runs`、`POST /v1/targets` 支持 `Idempotency-Key`，24h 内重放返回原对象 |

---

## 1. 通用约定

### 1.1 Base URL 与版本

```
https://{host}/v1
```

版本只在 URI 主版本位演进（`/v1` → `/v2`）。新增字段、新增枚举值**不**升版本；客户端必须忽略未知字段。

### 1.2 认证与授权

```
Authorization: Bearer <api_token>
```

| 角色 | 权限 |
|---|---|
| `viewer` | 所有 GET |
| `operator` | GET + `POST/PATCH/DELETE` targets、`POST runs`、`POST cancel`、`POST reports` |
| `admin` | operator + 工具注册、全局配置、清理 |

### 1.3 通用请求头

| Header | 必需 | 说明 |
|---|:---:|---|
| `Authorization` | ✅ | Bearer token |
| `Content-Type` | ✅ | `application/json`（POST/PATCH） |
| `X-Request-Id` | ❌ | 客户端生成的链路 ID，服务端原样回显 |
| `Idempotency-Key` | ❌ | 写操作幂等键，建议 UUID |
| `Accept-Language` | ❌ | `zh-CN` / `en-US`，影响 `message` 语言 |

### 1.4 时间、ID、分页

- 时间：RFC 3339 UTC，秒级精度，如 `2026-11-05T08:41:12Z`
- ID：带前缀的 ULID — `tgt_01JB7Q…`、`run_01JB7R…`、`art_01JB7S…`
- 分页：游标式
  ```
  GET /v1/runs?limit=20&cursor=run_01JB7Q...&status=succeeded&capability=perf
  → {"items":[...], "next_cursor":"run_01JB7P...", "has_more":true}
  ```
  `limit` 默认 20，最大 100。

### 1.5 错误格式（RFC 9457 `application/problem+json`）

```json
{
  "type": "https://thvv.dev/errors/target_unreachable",
  "title": "Target unreachable",
  "status": 422,
  "code": "target_unreachable",
  "detail": "connect to https://api.xxx.com/v1/chat/completions timeout after 15s",
  "instance": "/v1/runs",
  "request_id": "req_01JB7Q...",
  "retryable": true,
  "details": [{"field": "target.api_url", "issue": "timeout"}]
}
```

**错误码总表**

| code | HTTP | retryable | 场景 |
|---|:---:|:---:|---|
| `invalid_request` | 400 | ❌ | 字段类型/枚举错误 |
| `validation_failed` | 400 | ❌ | 语义校验失败（见 `details[]`） |
| `unauthorized` | 401 | ❌ | token 缺失/失效 |
| `forbidden` | 403 | ❌ | 角色不足 |
| `not_found` | 404 | ❌ | 资源不存在 |
| `conflict` | 409 | ❌ | 同名 target / 幂等键冲突且参数不同 |
| `not_implemented` | 501 | ❌ | `capability` 尚未实现（预留位），响应含 `available_at` |
| `target_unreachable` | 422 | ✅ | probe 或 run 前置连通性失败 |
| `tokenizer_unavailable` | 422 | ✅ | tokenizer 未缓存且下载失败 |
| `dataset_missing` | 422 | ❌ | 档位数据集缺失（如未构建 512k/1m） |
| `credential_invalid` | 422 | ❌ | 上游 401/403 |
| `quota_exceeded` | 429 | ✅ | 服务端并发 run 超限，响应含 `Retry-After` |
| `upstream_rate_limited` | 429 | ✅ | 被测端 429（不视为 run 失败，计入成功率） |
| `engine_failed` | 500 | ✅ | 引擎进程退出码非 0 |
| `eval_failed` | 500 | ✅ | eval 全部数据集失败（无非空样本 / 无报告），`detail` 含失败数据集与日志摘要 |
| `engine_timeout` | 504 | ✅ | 引擎整体超时（`detail` 含已完成步数、当前档位/数据集、日志摘要） |
| `internal_error` | 500 | ✅ | 其他 |

> `Retry-After` 单位秒。所有 429/500/504 都会带。

---

## 2. 资源模型

```
Tool (只读, 内置注册)
 └─ capability: perf | eval | functional

Target (被测端点)  ──1:N──►  Run (一次执行)  ──1:N──►  Artifact (交付物文件)
                                │
                                └─► Step (bucket × parallel 的最小执行单元)

Gate (验收阈值, 可内联或引用内置 profile)
Catalog (buckets / datasets / suites, 只读)
```

---

## 3. 系统与能力发现

### 3.1 `GET /v1/health`

```json
{"status":"ok","version":"1.0.0","uptime_sec":38211}
```

### 3.2 `GET /v1/meta`

```json
{
  "server": {"version":"1.0.0","commit":"a1b2c3d","started_at":"2026-11-05T01:00:00Z"},
  "capabilities": [
    {"name":"perf","status":"ga"},
    {"name":"eval","status":"preview"},
    {"name":"functional","status":"planned"}
  ],
  "limits": {
    "max_concurrent_runs": 4,
    "max_runs_queued": 32,
    "run_ttl_hours": 720,
    "artifact_max_bytes": 209715200,
    "sse_heartbeat_sec": 15
  },
  "protocols": ["openai", "anthropic"],
  "auth": {"scheme":"bearer","roles":["viewer","operator","admin"]}
}
```

### 3.3 `GET /v1/tools` — **多工具接入的核心**

```json
{
  "items": [
    {
      "name": "thvv",
      "display_name": "THVV TokenHub Vendor Verifier",
      "version": "1.0.0",
      "kind": "subprocess",
      "runtime": {"python": ">=3.10", "packages": ["evalscope[perf]>=0.13.0"]},
      "capabilities": [
        {
          "name": "perf",
          "status": "ga",
          "description": "性能压测 1k~1m 档位 × 并发梯度",
          "plan_schema": "/v1/tools/thvv/schemas/perf.plan.json",
          "result_schema": "/v1/tools/thvv/schemas/perf.result.json",
          "artifacts": ["html_report","xlsx_report","failure_csv","raw_db"]
        },
        {
          "name": "eval",
          "status": "preview",
          "description": "效果评测（LLM Judge + 17 数据集）",
          "plan_schema": "/v1/tools/thvv/schemas/eval.plan.json"
        },
        {
          "name": "functional",
          "status": "planned",
          "description": "功能测试（工具调用/长上下文/多模态）",
          "available_at": "v1.2",
          "plan_schema": null
        }
      ]
    },
    {
      "name": "acme-redteam",
      "display_name": "第三方安全红队工具",
      "kind": "http",
      "capabilities": [
        {"name":"functional","status":"ga","plan_schema":"/v1/tools/acme-redteam/schemas/functional.plan.json"}
      ]
    }
  ]
}
```

**扩展点定义**：新工具只需注册 `name` + 每个 `capability` 的 `plan_schema`（JSON Schema）。
`POST /v1/runs` 中 `plan` 的**公共字段**（`mode/buckets/…`）由服务端校验，
**工具私有字段**一律放进 `plan.tool_params`，由对应工具的 JSON Schema 校验 → **协议永不因新工具而变更**。

### 3.4 `GET /v1/catalog/buckets?tool=thvv`

档位定义来自 `perf/run.sh` 的 `bucket_range` / `bucket_max_parallel`，由服务端内建（Go 侧常量表）。

```json
{
  "items": [
    {"label":"1k",  "prompt_tokens":{"min":870,   "max":1228},   "max_parallel":128, "built_in":true},
    {"label":"9k",  "prompt_tokens":{"min":8000,  "max":11000},  "max_parallel":128, "built_in":true},
    {"label":"16k", "prompt_tokens":{"min":14000, "max":18000},  "max_parallel":128, "built_in":true},
    {"label":"32k", "prompt_tokens":{"min":28000, "max":36000},  "max_parallel":128, "built_in":true},
    {"label":"64k", "prompt_tokens":{"min":56000, "max":72000},  "max_parallel":64,  "built_in":true},
    {"label":"128k","prompt_tokens":{"min":112000,"max":144000}, "max_parallel":32,  "built_in":true},
    {"label":"200k","prompt_tokens":{"min":180000,"max":220000}, "max_parallel":16,  "built_in":true},
    {"label":"512k","prompt_tokens":{"min":500000,"max":540000}, "max_parallel":8,   "built_in":false},
    {"label":"1m",  "prompt_tokens":{"min":980000,"max":1030000},"max_parallel":8,   "built_in":false}
  ],
  "scenarios": [
    {"label":"9k_cold",  "bucket":"9k", "note":"前缀缓存冷启动"},
    {"label":"9k_hot",   "bucket":"9k", "note":"前缀缓存热命中"},
    {"label":"9k_mix50", "bucket":"9k", "note":"50% 前缀缓存混合"}
  ],
  "default_ladder": [1, 8, 32, 64, 128],
  "number_by_parallel": {"1":20,"4":40,"8":60,"16":80,"32":120,"64":200,"128":400}
}
```

> `built_in=false` 的档位需要先构建数据集（`POST /v1/datasets:build` 预留位，v1.1），否则 run 前置检查返回 `dataset_missing`。

### 3.5 `GET /v1/catalog/datasets?tool=thvv&capability=eval`

```json
{
  "items": [
    {"name":"aime26","display_name":"AIME 2026","kind":"text","needs_judge":false,"default_limit":null},
    {"name":"hle","display_name":"Humanity's Last Exam","kind":"text","needs_judge":true,"default_limit":30}
  ],
  "note": "数据集清单以本接口实时返回为准，随工具版本变化"
}
```

### 3.6 `GET /v1/catalog/gates`

内置验收阈值模板（来自 `perf/scripts/sla_eval.py` 的 `TIERS` 与 `RATE_MIN_TOKENS_PER_SEC`）。

```json
{
  "items": [
    {
      "profile": "perf-sla-v1",
      "capability": "perf",
      "description": "按 avg_prompt_tokens 分档的 TTFT P50/P90 阈值 + 吞吐下限",
      "tiers": [
        {"tier":"<4K",   "prompt_tokens":{"lo":1000,  "hi":4000},   "ttft_p50_max_sec":2.0,  "ttft_p90_max_sec":5.0},
        {"tier":"<8K",   "prompt_tokens":{"lo":4000,  "hi":8000},   "ttft_p50_max_sec":2.5,  "ttft_p90_max_sec":5.0},
        {"tier":"<32K",  "prompt_tokens":{"lo":8000,  "hi":32000},  "ttft_p50_max_sec":4.0,  "ttft_p90_max_sec":8.0},
        {"tier":"<64K",  "prompt_tokens":{"lo":32000, "hi":64000},  "ttft_p50_max_sec":8.0,  "ttft_p90_max_sec":15.0},
        {"tier":"<128K", "prompt_tokens":{"lo":64000, "hi":128000}, "ttft_p50_max_sec":15.0, "ttft_p90_max_sec":35.0},
        {"tier":"<256K", "prompt_tokens":{"lo":128000,"hi":256000}, "ttft_p50_max_sec":30.0, "ttft_p90_max_sec":70.0}
      ],
      "global": {"min_output_tokens_per_sec": 30.0, "min_success_rate": 0.99},
      "params": {"user_tpm_quota": {"type":"integer","default":5000000}}
    }
  ]
}
```

---

## 4. Target — 初始化（第一步）

### 4.1 `POST /v1/targets`

```json
{
  "name": "zhipu-glm52-prod",
  "vendor": "zhipu",
  "api_url": "https://api.zhipuai.com/v1/chat/completions",
  "protocol": "openai",
  "model": "glm-5.2",
  "api_key": "sk-xxxxxxxxxxxxxxxx",
  "tokenizer": "zai-org/GLM-4.6",
  "defaults": {
    "temperature": "auto",
    "max_tokens": 4096,
    "connect_timeout_sec": 60,
    "read_timeout_sec": 600
  },
  "labels": {"env": "prod", "ticket": "THVV-123"}
}
```

字段约束：

| 字段 | 必填 | 说明 |
|---|:---:|---|
| `name` | ✅ | 全局唯一；重复 → `409 conflict` |
| `vendor` | ❌ | 供应商名，用于目录命名与报告署名（对应 `CLIENT`） |
| `api_url` | ✅ | **完整请求路径**，服务端不做拼接。`openai` → `.../chat/completions`；`anthropic` → `.../v1/messages` |
| `protocol` | ✅ | `openai` \| `anthropic` |
| `model` | ✅ | 模型名（对应 `MODEL_NAME`） |
| `api_key` | ✅ | 只写。`PUT`/`PATCH` 可轮换 |
| `tokenizer` | ❌ | 缺省时按 `model` 关键词自动推断（沿用 `run.sh` 的 `auto_detect_tokenizer` 映射表），响应回显最终取值与推断来源 |
| `defaults.temperature` | ❌ | `"auto"` 时按模型名决定（kimi/moonshot → 1.0，其余 0.0） |

响应 `201 Created`（**注意 `api_key` 不返回**）：

```json
{
  "target_id": "tgt_01JB7QZ9",
  "name": "zhipu-glm52-prod",
  "vendor": "zhipu",
  "api_url": "https://api.zhipuai.com/v1/chat/completions",
  "protocol": "openai",
  "model": "glm-5.2",
  "api_key_hint": "••••••••xxxxxxxx",
  "tokenizer": {"value":"zai-org/GLM-4.6","source":"auto_detected","cached": true},
  "defaults": {"temperature":0.0,"max_tokens":4096,"connect_timeout_sec":60,"read_timeout_sec":600},
  "labels": {"env":"prod","ticket":"THVV-123"},
  "health": {"status":"unknown","checked_at":null},
  "created_at": "2026-11-05T08:41:12Z",
  "updated_at": "2026-11-05T08:41:12Z"
}
```

`tokenizer.source` ∈ `explicit | auto_detected | fallback`。

### 4.2 `GET /v1/targets/{target_id}` / `GET /v1/targets` / `PATCH` / `DELETE`

- `PATCH` 支持局部更新与 `api_key` 轮换；`tokenizer` 显式设为 `null` 等价于重置为自动推断。
- `DELETE` 为软删除（`deleted_at`），已存在 run 的历史结果保留。

### 4.3 `POST /v1/targets/{target_id}/probe` — 预检（对应 `quickstart.sh check`）

```json
{"checks": ["connectivity","tokenizer","datasets","protocol"], "timeout_sec": 30}
```

```json
{
  "target_id": "tgt_01JB7QZ9",
  "ok": false,
  "checked_at": "2026-11-05T08:42:00Z",
  "elapsed_ms": 4120,
  "checks": [
    {"name":"protocol",     "ok":true,  "detail":"openai"},
    {"name":"connectivity", "ok":true,  "detail":"HTTP 200, first token 412ms", "latency_ms":412},
    {"name":"tokenizer",    "ok":true,  "detail":"zai-org/GLM-4.6 cached"},
    {"name":"datasets",     "ok":false, "detail":"512k/1m 未构建（perf_zh_512k.jsonl 缺失）",
                                          "missing":["512k","1m"], "severity":"warn"}
  ],
  "engine": {"name":"thvv","version":"1.0.0","python":"3.11.9","evalscope":"0.13.2"}
}
```

`severity` ∈ `error | warn`；`warn` 不阻塞可运行的档位。

---

## 5. Run — 测试执行（第二步）

### 5.1 `POST /v1/runs`

请求头：`Idempotency-Key: <uuid>`（推荐）

```json
{
  "tool": "thvv",
  "capability": "perf",
  "target_id": "tgt_01JB7QZ9",
  "plan": {
    "mode": "single",
    "buckets": ["1k"],
    "parallel_ladder": [10],
    "number": 200
  },
  "gate": {"profile": "perf-sla-v1", "params": {"user_tpm_quota": 1000000}},
  "report": {"formats": ["html","xlsx"], "client_name": "智谱", "title": "GLM-5.2 性能压测报告"},
  "labels": {"round": "r1"},
  "callback": {"url":"https://ci.example.com/hook","secret":"whsec_x","events":["run.succeeded","run.failed"]}
}
```

**`target` 内联形态**（免注册，一次性验收场景）：

```json
{
  "tool": "thvv",
  "capability": "perf",
  "target": {
    "api_url": "https://api.xxx.com/v1/chat/completions",
    "protocol": "openai",
    "model": "glm-5.2",
    "api_key": "sk-xxx"
  },
  "plan": {"mode":"single","buckets":["1k"],"parallel_ladder":[1]}
}
```

> `target_id` 与 `target` **二选一**，同时出现 → `400 validation_failed`。内联 target 不持久化，仅存于该 run（审计需要，仍落库但标记 `ephemeral: true`）。

响应 `202 Accepted`：

```json
{
  "run_id": "run_01JB7R2K",
  "status": "queued",
  "queue_position": 1,
  "tool": "thvv",
  "capability": "perf",
  "target": {"target_id":"tgt_01JB7QZ9","vendor":"zhipu","model":"glm-5.2","protocol":"openai",
             "api_url":"https://api.zhipuai.com/v1/chat/completions","ephemeral":false},
  "plan": {"mode":"single","buckets":["1k"],"parallel_ladder":[10],"number":200,"steps_total":1},
  "gate": {"profile":"perf-sla-v1","params":{"user_tpm_quota":1000000}},
  "progress": {"phase":"queued","steps_total":1,"steps_done":0,"percent":0,"elapsed_sec":0},
  "created_at": "2026-11-05T08:43:00Z",
  "links": {
    "self":       "/v1/runs/run_01JB7R2K",
    "events":     "/v1/runs/run_01JB7R2K/events",
    "results":    "/v1/runs/run_01JB7R2K/results",
    "artifacts":  "/v1/runs/run_01JB7R2K/artifacts",
    "cancel":     "/v1/runs/run_01JB7R2K/cancel"
  }
}
```

### 5.2 `plan` 规范 —— perf

| 字段 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `mode` | enum | `single` | `single`（单档）/ `matrix`（全档或指定多档 × 并发梯度） |
| `buckets` | string[] | `single`→必填；`matrix`→全部 built_in 档位 | 档位标签，取自 catalog |
| `parallel_ladder` | int[] | `single`→必填；`matrix`→`[1,8,32,64,128]` | 并发梯度；显式给出时不再追加档位专用并发（对齐 `CONCURRENCY_LADDER` 语义） |
| `number` | int | `single`→500 | **仅 `single` 模式**：请求总数（对齐 `perf bench <bucket> [N]`） |
| `number_by_parallel` | map[int]int | 内建表 | 按并发决定每步请求数（对齐 `parallel_number`） |
| `bucket_overrides` | map[string]obj | — | 单档覆盖：`{"200k":{"parallels":[8,16],"max_parallel":16,"number":16}}`；`max_parallel` ≤ catalog 上限 |
| `cooldown_sec` | int | 120 | 档间冷却（对齐 `BUCKET_COOLDOWN`） |
| `success_rate_min` | number | 0 | 成功率早停阈值，**取 0~1 比值**（内部按百分比传 `SUCCESS_RATE_MIN`） |
| `tpm_budget` | int | 0 | tokens/min 预算，0=不限；用于裁剪并发与请求数（对齐 `TPM_BUDGET`，口径见 run.sh 注释：按单条 token × 并发折算，非硬限流） |
| `stop_on_failure` | bool | true | 某档 `benchmark_summary.json` 缺失时是否跳过该档剩余并发 |
| `request` | obj | — | `temperature`(number\|"auto")、`max_tokens`(4096)、`max_turns`(1)、`connect_timeout_sec`(60)、`read_timeout_sec`(600) |
| `dataset_dir` | enum | `auto` | `auto`\|`clean`\|`raw`（对应 `datasets_clean` / `datasets`） |
| `tool_params` | obj | `{}` | **工具私有透传字段**，由工具 JSON Schema 校验 |

`steps_total` 计算方式：`Σ_{bucket ∈ buckets} |{p ∈ ladder ∪ extra : p ≤ max_parallel(bucket)}|`，服务端在 `202` 响应中给出，客户端可用于算百分比。

### 5.3 `plan` 规范 —— eval

```json
{
  "mode": "single",
  "datasets": ["aime26", "gpqa_diamond"],
  "limit": 30,
  "repeats": 1,
  "eval_batch_size": 10,
  "thinking": true,
  "judge": {"api_key":"sk-xxx","model":"deepseek-v4-pro","base_url":"https://api.deepseek.com/v1"},
  "provider_name": "zhipu",
  "request": {"max_tokens": 8192}
}
```

- `datasets`：取自 `GET /v1/catalog/datasets`，去重保序；未知数据集 → 400。
- `limit`：每数据集条数，0 = 全量（若数据集配置了 `default_limit` 则用它）。
- `repeats` / `eval_batch_size`：0 = 数据集默认；上限 50 / 256。
- `judge`：所选数据集含 `needs_judge=true`（hle / simple_qa）时 `api_key` 必填，否则 400。
  `api_key` 是**只写字段**：落库走 AES-GCM 密文（`runs.judge_key_enc`），任何 GET/SSE 响应只回
  `api_key_hint`（如 `••••••••-7e0`），`model` / `base_url` 照常回显。未配置时由服务端
  `engines.<tool>.capabilities.eval.judge` 补齐。
- `provider_name`：缺省取 target.vendor，再缺省 `unknown`。
- **Step = 数据集**：`steps_total = len(datasets)`，一个数据集一个子进程。
- 结果在 `GET /v1/runs/{id}/results` 的 `eval` 字段（见 §5.9）；`metrics` 为空。
- 设计与产物见 [`EVAL.md`](./EVAL.md)。

### 5.4 `plan` 规范 —— functional（planned，501）

```json
{
  "mode": "suite",
  "suites": ["tool_call", "long_context_recall", "structured_output"],
  "cases": [{"id":"tc-001","input":"...","expect":{"schema":{}}}],
  "assertions": ["json_schema", "exact_match", "regex"]
}
```

> v1 对 `capability=functional` 返回 `501 not_implemented`，`detail` 中给出 `available_at: "v1.2"`。

### 5.5 `Run` 对象（完整）

```json
{
  "run_id": "run_01JB7R2K",
  "status": "running",
  "tool": "thvv",
  "capability": "perf",
  "target": { "...": "同 5.1" },
  "plan": { "...": "规范化后的 plan（含默认值填充结果）" },
  "gate": {"profile":"perf-sla-v1","params":{"user_tpm_quota":1000000},"mode":"report_only"},
  "progress": {
    "phase": "running",
    "steps_total": 20,
    "steps_done": 7,
    "percent": 35.0,
    "current": {"bucket":"16k","parallel":32,"number":120},
    "elapsed_sec": 1840,
    "eta_sec": 4210
  },
  "steps": [
    {"index":0,"bucket":"1k","parallel":1,"number":20,"status":"succeeded",
     "success_rate":1.0,"ttft_p50_sec":0.31,"output_tps":42.1,"duration_sec":38},
    {"index":1,"bucket":"1k","parallel":8,"number":60,"status":"running"}
  ],
  "gate_result": null,
  "summary": null,
  "artifacts": [],
  "engine": {"name":"thvv","version":"1.0.0","workdir":"/var/lib/thvv/runs/run_01JB7R2K","pid":48213},
  "error": null,
  "labels": {"round":"r1"},
  "created_at":"2026-11-05T08:43:00Z",
  "started_at":"2026-11-05T08:43:05Z",
  "finished_at":null,
  "links": {"...": "..."}
}
```

`error` 非空即代表失败终态（`status=failed`），且**不必翻 run.log 就能定位原因**：
`detail` 里带「失败范围 + 引擎日志摘要」。示例：

```json
// eval 全部数据集失败（例如端点不支持多模态、鉴权失败）
"error": {
  "type": "https://thvv.dev/errors/eval_failed", "status": 500, "code": "eval_failed",
  "detail": "2/2 个数据集评测失败（aime26、gpqa_diamond），没有产出有效样本或报告；已完成 2/2 步\n日志摘要：HTTP 400：model is not a multimodal model ×5706 次；ignore_errors 容错跳过 951 条样本"
}

// 超过单次运行时长上限
"error": {
  "type": "https://thvv.dev/errors/engine_timeout", "status": 504, "code": "engine_timeout",
  "detail": "超过单次运行时长上限 12h0m0s；已完成 4/15 步，当前 mmlu_pro（并发 30）"
}
```

- `detail` 已脱敏（target/judge 的 key 一律 `***`）、压成单行并截断到 1.6KB
- `status=partial` 不算失败（`error=null`），失败的数据集名单在 `summary.failed_datasets`

> `steps[]` 仅在 `include=steps` 查询参数下返回（`GET /v1/runs/{id}?include=steps`），默认省略以免响应过大。

### 5.6 状态机

```
                 ┌───────────── cancel ─────────────┐
queued ──► preparing ──► running ──► reporting ──► succeeded
   │            │           │            │
   │            │           │            ├──► partial   (全部 step 跑完但有失败 step)
   │            │           │            └──► failed    (报告生成失败 / 引擎崩溃)
   │            │           └──► failed  (前置检查失败 / 引擎退出码非 0)
   │            └──► failed  (probe 失败 / tokenizer 下载失败 / 数据集缺失)
   └──► canceled (排队中被取消)
                                    任一非终态 ──► canceled
```

| status | 终态 | 说明 |
|---|:---:|---|
| `queued` | ❌ | 等待执行槽位 |
| `preparing` | ❌ | 依赖检查、tokenizer 预热、数据集校验 |
| `running` | ❌ | 逐步执行 steps |
| `reporting` | ❌ | 聚合 `benchmark_data.db` 产出 HTML/xlsx/JSON |
| `succeeded` | ✅ | 全部 step 成功且 gate 满足（或 gate 未启用） |
| `partial` | ✅ | 已跑完但存在失败 step / gate 不满足 |
| `failed` | ✅ | 无法完成 |
| `canceled` | ✅ | 客户端取消 |

**`gate_result.passed=false` 不改变 `status`**：`status` 只描述"执行是否完成"，`gate_result` 描述"是否达标"。两者分开，避免下游把"未达标"误判为"执行失败"。

### 5.7 `GET /v1/runs` 列表

查询参数：`status`（可重复）、`capability`、`tool`、`target_id`、`vendor`、`created_after`、`created_before`、`label.<k>=<v>`、`q`（模糊搜 name/model）、`limit`、`cursor`、`order`（`created_at` \| `-created_at`）。

### 5.8 `POST /v1/runs/{run_id}/cancel`

`202 Accepted` → `{"run_id":"...","status":"canceled","canceled_at":"..."}`

语义：幂等；对终态 run 调用返回 `200` + 当前状态（不报错）。实现上对引擎进程发 `SIGTERM`，10s 后 `SIGKILL`，并保留已产出的 step 结果（可对 `partial` run 生成报告）。

### 5.8.1 `DELETE /v1/runs/{run_id}` — 删除一条记录

- 仅 `operator`/`admin`（写操作）。
- 只能删**终态** run（`succeeded` / `partial` / `failed` / `canceled`）；仍在排队/运行的返回
  `409 conflict`，提示先取消。
- 删除范围：`runs` / `steps` / `artifacts` / `run_events` 四张表 + 磁盘上的
  `runs/<run_id>/`（含 HTML/xlsx/CSV/原始 `benchmark_data.db`/`run.log`）。
- 响应：`204 No Content`；不存在 → `404 not_found`。不可恢复（审计日志里留一条 `run.delete`）。

### 5.8.2 `POST /v1/runs/{run_id}/rerun` — 一键重测

用原记录的 **target + capability + plan + gate + report** 新建一个 run（原记录保留）：

- 响应 `202 Accepted`，body 是**新** run 对象；新 run 的 `labels.rerun_of` = 原 run_id
- 只允许对终态 run 重测（运行中 → `400 validation_failed`，提示先取消）
- `target` 已被删除 → `400`（detail 说明「原端点已被删除」）
- 内联 target（`target.ephemeral=true`，凭据未持久化）→ `409 conflict`
- `plan.judge.api_key` 从 `runs.judge_key_enc` 解回明文后重新加密落到新 run，响应里仍只有 `api_key_hint`
- 审计留 `run.rerun`（detail=`from=<原 run_id>`）

### 5.8.3 `POST /v1/runs/purge` — 批量清理

```json
{ "run_ids": ["run_...","run_..."], "dry_run": false }
```

| 字段 | 默认 | 说明 |
|---|---|---|
| `run_ids` | 空 | **勾选删除**：显式指定要删的 run；给出时忽略 `statuses/capability/before`，不存在的 id 忽略，运行中的计入 `skipped_active` |
| `statuses` | `["failed","canceled"]` | 只允许终态；非终态 → `400 validation_failed` |
| `capability` | 空 | `perf` / `eval` / `cache` 过滤 |
| `before` | 空 | 只删该时间（RFC3339，按 `created_at`）之前的记录 |
| `dry_run` | `false` | `true` 时只返回将命中的 `run_ids`，不真删 |

响应：

```json
{ "deleted": 12, "dry_run": false, "run_ids": ["run_..."], "skipped_active": 0 }
```

Web 界面「历史记录」页的入口：

- **勾选删除**：行首复选框（表头可全选当前列表，运行中的行禁用）→ 工具栏出现
  「删除选中 (N)」→ 确认后调 `POST /v1/runs/purge {run_ids:[...]}`
- 行内「删除」按钮：单条 `DELETE /v1/runs/{id}`

> 按状态批量删（`statuses` + `dry_run`）只保留在 API 侧给自动化/运维用，Web 界面不提供入口。

### 5.9 `GET /v1/runs/{run_id}/results` — 结构化指标

统一响应外壳（不同 capability 填不同 `metrics` 结构）：

```json
{
  "run_id": "run_01JB7R2K",
  "capability": "perf",
  "gate_result": {
    "passed": false,
    "profile": "perf-sla-v1",
    "violations": [
      {"scope":"step","bucket":"64k","parallel":64,"rule":"ttft_p50_max",
       "tier":"<128K","actual":18.4,"threshold":15.0,"message":"P50 TTFT 18.4s > 15.0s"},
      {"scope":"global","rule":"min_success_rate","actual":0.972,"threshold":0.99}
    ],
    "tier_summary": [
      {"tier":"<4K","steps":5,"p50_max_sec":0.42,"p90_max_sec":1.1,"passed":true}
    ]
  },
  "summary": {
    "steps_total": 20, "steps_succeeded": 19, "steps_failed": 1,
    "success_rate": 0.9821,
    "peak_output_tps": 1843.2,
    "tpm_peak": 6120000,
    "ttft_p50_sec": 0.42, "ttft_p90_sec": 1.93, "tpot_ms": 18.7,
    "verdict": "conditional_pass"
  },
  "metrics": {
    "per_step": [
      {
        "step_index": 0, "bucket": "1k", "parallel": 1, "number": 20,
        "requests": {"total":20,"success":20,"failed":0,"success_rate":1.0},
        "ttft":  {"avg_sec":0.31,"min_sec":0.22,"max_sec":0.55,
                  "p50_sec":0.30,"p90_sec":0.44,"p95_sec":0.48,"p99_sec":0.54},
        "ttlt":  {"avg_sec":3.21},
        "tpot":  {"avg_ms":18.4},
        "itl":   {"avg_ms":17.9,"p50_ms":17.2,"p90_ms":21.0},
        "throughput": {"input_tps":310.2,"output_tps":42.1,
                       "input_tpm":18612,"output_tpm":2526,"tpm":21138,"qpm":240},
        "tokens": {"prompt_sum":2520,"completion_sum":4096,"total_sum":6616},
        "duration_sec": 38.2,
        "failures": {"by_reason":{"http_429":0,"timeout":0,"content_filter":0}},
        "raw_db": "art_01JB7S1"
      }
    ],
    "difficulty_curve": [
      {"bucket":"1k","max_parallel_ok":128,"ttft_p50_sec":0.30},
      {"bucket":"64k","max_parallel_ok":32,"ttft_p50_sec":9.7}
    ]
  }
}
```

> 字段与 `perf/scripts/gen_report_from_db.py` 的计算口径逐项对齐：
> **`itl` 与 `tpot` 是两个不同口径**（ITL = 相邻流式 chunk 间隔按 chunk 汇总；TPOT = evalscope 写入的 `time_per_output_token`），不可混用。
> 分位数算法与该脚本一致（线性插值），保证服务端与历史 Python 报告数字可比。

`capability=eval` 的结构化结果放在 `eval` 字段（`metrics` 为空对象）：

```json
{
  "run_id": "run_...",
  "capability": "eval",
  "summary": {"verdict":"pass","datasets_total":2,"datasets_succeeded":2,
              "datasets_failed":0,"score_avg":0.63,"model":"glm-5.3-test","provider":"turbo"},
  "eval": {
    "model": "glm-5.3-test",
    "provider": "turbo",
    "datasets": [{
      "dataset":"aime26","label":"aime26","success":true,
      "score":1.0,"metric":"mean_acc","num_samples":4,
      "limit":4,"repeats":1,"eval_batch_size":4,
      "retries_used":0,"rate_limited":false,"elapsed_sec":170.0,
      "skipped":0,"cache_hit_rate":0.0
    }]
  }
}
```

> 分数直接采信 `thvv/eval` 落盘的 `eval_summary.json`（Python 侧口径），Go 不二次计算。
> 产物：`eval_report`（eval_report_v2.html）、`eval_summary`（json）、`eval_samples`（csv）、
> `eval_archive`（tar.gz）、`run_log`、汇总 `all_eval_summary.json`。详见 [`EVAL.md`](./EVAL.md)。

`capability=cache` 的结构化结果放在 `cache` 字段（`metrics` 为空对象），`summary` 同时包含缓存收益
（稳态 TTFT、改善率、前缀命中率）与性能维度（TPOT、解码速度、in/out 吞吐、时延分位、样本数）：

```json
{
  "run_id": "run_...",
  "capability": "cache",
  "summary": {"verdict":"pass","samples_steady":12,"tpot_steady_p50_ms":7.4,
              "decode_speed_steady_p50":134.0,"input_throughput":2468.7,
              "output_throughput":61.8,"request_latency_p99_ms":1984.9},
  "cache": {
    "workload": "multi-round-chat", "num_sessions": 3,
    "steady_state": {"requests":12,"ttft_p50_ms":447.7,"tpot_p50_ms":7.4,"decode_speed_p50":134.0}
  }
}
```

> 口径与完整字段见 [`CACHE.md`](./CACHE.md) §3.3；性能数字是「前缀缓存命中态」的表观性能，
> 与 perf 的冷启动口径不可直接横比。

**多档（并发档位，方案 B）**：`plan.ladder` 非空时一次 run 顺序执行多档，`steps_total = 档数`，
`summary.tiers_count` / `summary.max_sessions` 给出档位概览，每档完整指标在 `cache.tiers[]`
（元素不嵌套 `tiers`，`bucket` 形如 `c1`/`c4`）。gate 逐档判定，`violations[].tier` 标档位，
`tier_summary[]` 每档一行。`plan` 示例与校验规则见 [`CACHE.md`](./CACHE.md) §3.2。

**`GET /v1/runs/{run_id}/results/failures`** — 失败明细分页（对应 `export_failure_details.py`）：
`?limit=50&cursor=&reason=timeout`，返回 `{items:[{step_index,request_index,reason,http_status,request_body_excerpt,response_body_excerpt,error}], next_cursor}`。

### 5.10 `GET /v1/runs/{run_id}/events` — SSE 进度流

```
GET /v1/runs/run_01JB7R2K/events?since=0
Accept: text/event-stream
```

支持断线续传：初始发 `Last-Event-ID`（即上次收到的 `id`），或 `?since=<seq>`。

```
retry: 3000

id: 1
event: run.snapshot
data: {"run_id":"run_01JB7R2K","status":"queued","progress":{...},"steps_total":20}

id: 2
event: run.started
data: {"phase":"preparing","at":"2026-11-05T08:43:05Z","engine":{"pid":48213}}

id: 3
event: step.started
data: {"step_index":0,"bucket":"1k","parallel":1,"number":20}

id: 4
event: step.progress
data: {"step_index":0,"completed":12,"total":20,"success_rate":1.0,"ttft_p50_sec":0.29}

id: 5
event: step.finished
data: {"step_index":0,"status":"succeeded","success_rate":1.0,"output_tps":42.1,"duration_sec":38}

id: 6
event: run.progress
data: {"steps_done":1,"steps_total":20,"percent":5.0,"elapsed_sec":42,"eta_sec":760}

id: 7
event: run.gate
data: {"passed":false,"violations":2}

id: 8
event: run.finished
data: {"status":"partial","finished_at":"...","artifacts":[{"artifact_id":"art_01JB7S1","kind":"html_report"}]}

: heartbeat
```

事件类型：

| event | 时机 | data 关键字段 |
|---|---|---|
| `run.snapshot` | 连接建立时 | 完整 `Run` |
| `run.started` | `queued → preparing` | `phase`, `engine` |
| `step.started` | 每个 step 开始 | `step_index`, `bucket`, `parallel`, `number` |
| `step.progress` | 每 5s 或每 5% | `completed`, `total`, `success_rate`, 实时 TTFT |
| `step.finished` | 每个 step 结束 | `status`, `success_rate`, `output_tps`, `duration_sec` |
| `run.progress` | step 边界 | `steps_done`, `percent`, `eta_sec` |
| `run.log` | 有新增 stdout（可选订阅） | `line`, `stream` |
| `run.gate` | gate 计算完成 | `passed`, `violations` |
| `run.finished` | 终态 | `status`, `finished_at`, `artifacts` |
| `error` | 引擎致命错误 | `code`, `detail` |

心跳：无事件时每 15s 发 `: heartbeat`（SSE 注释行），防止中间代理断连。
服务端保留每 run 最近 1000 条事件用于 `Last-Event-ID` 重放；超出的返回 `409 event_history_gone`，客户端改用 `GET /v1/runs/{id}` 拉全量。
终态 run 仍可订阅（立即推 `run.snapshot` + `run.finished` 后关闭）。

### 5.11 回调（Webhook）

`callback.url` 收到与 SSE 同结构的 JSON POST：

```
POST https://ci.example.com/hook
X-THVV-Event: run.finished
X-THVV-Signature: sha256=<hmac(secret, raw_body)>
```

重试：5 次指数退避（5s/25s/2m/10m/30m），全失败后置 `callback.status=failed` 并在 `GET /v1/runs/{id}` 的 `callback` 字段可见。

---

## 6. Artifact — 交付物

### 6.1 `GET /v1/runs/{run_id}/artifacts`

```json
{
  "items": [
    {"artifact_id":"art_01JB7S1","kind":"html_report","filename":"性能测试报告.html",
     "content_type":"text/html; charset=utf-8","size_bytes":482113,"sha256":"9f2c...",
     "created_at":"2026-11-05T09:41:12Z","download_url":"/v1/artifacts/art_01JB7S1"},
    {"artifact_id":"art_01JB7S2","kind":"xlsx_report","filename":"性能测试报告.xlsx",
     "content_type":"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
     "size_bytes":31240,"sha256":"1a3b...","download_url":"/v1/artifacts/art_01JB7S2"},
    {"artifact_id":"art_01JB7S3","kind":"failure_csv","filename":"失败请求详情.csv","size_bytes":8102},
    {"artifact_id":"art_01JB7S4","kind":"raw_db","filename":"benchmark_data.db","size_bytes":2411230},
    {"artifact_id":"art_01JB7S5","kind":"run_log","filename":"run.log","size_bytes":10422}
  ]
}
```

`kind` 枚举：`html_report` \| `xlsx_report` \| `failure_csv` \| `sla_json` \| `raw_db` \| `run_log` \| `archive` \| `eval_html` \| `eval_csv`。

### 6.2 `GET /v1/artifacts/{artifact_id}`

- 直接返回文件流，`Content-Disposition: attachment; filename*=UTF-8''...`（中文名）
- 支持 `Range`（大文件断点续传）、`If-None-Match`（`ETag` = `sha256`）
- 权限：`viewer` 可下载；`api_key` 相关文件（`run_log`）在下载前已脱敏（服务端落盘即脱敏）

### 6.3 `POST /v1/runs/{run_id}/reports` — 重新生成报告

```json
{"formats": ["html","xlsx"], "client_name": "智谱", "title": "…", "sla_profile": "perf-sla-v1"}
```

`202 Accepted` → 异步执行（`run.status` 不变，`report_job_id` 在响应中）。场景：跑完没生成报告 / 换署名重出 / 只想要 Excel。

---

## 7. 典型调用序列

### 7.1 三步走（最常用）

```bash
BASE=https://thvv.internal/v1
TOKEN=xxx
H="Authorization: Bearer $TOKEN"

# 1) 初始化：登记被测端点
TGT=$(curl -sS -X POST $BASE/targets -H "$H" -H 'Content-Type: application/json' -d '{
  "name":"zhipu-glm52","vendor":"zhipu",
  "api_url":"https://api.zhipuai.com/v1/chat/completions",
  "protocol":"openai","model":"glm-5.2","api_key":"sk-xxx"
}' | jq -r .target_id)

# 2a) 预检（可选但强烈建议）
curl -sS -X POST $BASE/targets/$TGT/probe -H "$H" | jq '.ok, .checks[]'

# 2b) 单档测试
RUN=$(curl -sS -X POST $BASE/runs -H "$H" -H 'Content-Type: application/json' \
  -H "Idempotency-Key: $(uuidgen)" -d "{
  \"tool\":\"thvv\",\"capability\":\"perf\",\"target_id\":\"$TGT\",
  \"plan\":{\"mode\":\"single\",\"buckets\":[\"1k\"],\"parallel_ladder\":[10],\"number\":200}
}" | jq -r .run_id)

# 跟踪进度
curl -N $BASE/runs/$RUN/events -H "$H"

# 3) 结果与交付物
curl -sS $BASE/runs/$RUN/results -H "$H" | jq .gate_result.passed
curl -sS $BASE/runs/$RUN/artifacts -H "$H" | jq -r '.items[].download_url'
```

### 7.2 全档位矩阵

```bash
curl -sS -X POST $BASE/runs -H "$H" -H 'Content-Type: application/json' -d "{
  \"tool\":\"thvv\",\"capability\":\"perf\",\"target_id\":\"$TGT\",
  \"plan\":{
    \"mode\":\"matrix\",
    \"buckets\":[\"1k\",\"9k\",\"16k\",\"32k\",\"64k\",\"128k\",\"200k\"],
    \"parallel_ladder\":[1,8,32,64,128],
    \"bucket_overrides\":{\"200k\":{\"parallels\":[8,16],\"max_parallel\":16}},
    \"cooldown_sec\":120,
    \"success_rate_min\":0.99,
    \"tpm_budget\":1000000
  },
  \"gate\":{\"profile\":\"perf-sla-v1\",\"params\":{\"user_tpm_quota\":1000000}},
  \"report\":{\"formats\":[\"html\",\"xlsx\"],\"client_name\":\"智谱\"}
}"
```

### 7.3 CI 门禁（同步等待 + 退出码）

```bash
# 阻塞等待终态（服务端提供 ?wait=true&timeout_sec=7200 便利参数）
curl -sS "$BASE/runs/$RUN?wait=true&timeout_sec=7200" -H "$H"
# → status=partial 或 gate_result.passed=false 时 CI 判失败
```

---

## 8. 兼容性与演进

| 变更 | 是否需要 v2 |
|---|:---:|
| 新增 `capability` 枚举值 | ❌ |
| 新增 `tool` | ❌ |
| `plan` 工具私有字段（`tool_params` 内） | ❌ |
| 新增响应字段 | ❌ |
| 新增 `status` 值 | ⚠️ 客户端必须对未知 status 视为非终态并继续轮询 |
| 删除/重命名字段、改字段语义、改分页方式 | ✅ |

---

## 9. 待评审问题（Open Questions）

1. `POST /v1/runs` 是否需要"同步模式"（`?wait=true&max_wait=300`）？已按 7.3 预留，需确认超时语义（超时返回 `202` + run_id，不报错）。
2. 凭据存储：DB 内加密（`AES-GCM` + KMS/本地 key）vs 只存引用（HashiCorp Vault / K8s Secret）。建议先做后者，避免自建密钥管理。
3. 多租户隔离：是否需要 `tenant_id`（当前用 `labels` + RBAC 的粗粒度方案）。
4. `eval` 与 `perf` 是否允许"组合 run"（一次提交同时跑性能+效果，共享 target 与报告）。建议 v1.2 引入 `capability: ["perf","eval"]` 数组形态，当前保持单值。
5. 历史结果导入：现有 `perf/results/` 下已有产物，需要 `POST /v1/imports` 把历史 run 纳入服务端索引（用于跨版本对比）。
