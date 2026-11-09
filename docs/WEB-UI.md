# THVV Web 页面与前后端交互规范

> 目标：用户打开网页 → 填一份表单（地址 / 模型 / Key / 测试范围）→ 点「开始测试」→ 实时看进度 → 拿报告下载。
> 本文定义：**页面结构**、**每个页面动作调用的 HTTP 接口**、**浏览器侧的特殊约束**（Cookie 会话 / SSE / 文件下载）。
>
> 前端实现：[`../../server/web/index.html`](../../server/web/index.html) · [`app.js`](../../server/web/app.js) · [`styles.css`](../../server/web/styles.css)
> （无构建、无依赖；页面只连真实服务端，无前端 Mock/演示模式）
>
> **实现状态**：Go 服务端与前端均已落地（`server/`），接口与本文一致；
> 差异见 §12「实现记录」。
> 服务端契约：[`API.md`](./API.md) · [`openapi.yaml`](./openapi.yaml)

---

## 1. 设计边界

| 决策 | 选择 | 理由 |
|---|---|---|
| 前端形态 | **纯静态 SPA**（HTML + 原生 JS，无 npm/无打包） | 服务端 `go:embed` 直接托管；无 Node 工具链；代码可评审、可 diff |
| 路由 | **Hash 路由**（`#/new`、`#/run/<id>`） | 服务端不需要 SPA fallback 重写，任意静态服务器可托管 |
| 页面数量 | 5 个视图（登录 / 新建 / 运行 / 历史 / 能力清单） | 与协议的能力面一一对应，不引入额外概念 |
| 前后端接口 | **复用同一套 `/v1` REST API**，仅新增 3 个 Web 专用 auth 端点 | 避免维护第二套 BFF；Web 客户端只是「另一种客户端」 |
| 认证 | Web 用 **HttpOnly Cookie 会话**；外部客户端仍用 Bearer token | `EventSource` 与 `<a download>` 都无法带自定义 Header（见 §5.1） |
| 数据获取 | 首屏 4 个并行 GET（`/meta` `/tools` `/catalog/buckets` `/catalog/gates`），支持 ETag 缓存 | 不额外引入 `/ui/bootstrap`，避免协议膨胀 |
| 提交方式 | 一个 `POST /v1/runs` + `Idempotency-Key` | 防用户在慢网下双击重复提交 |

**不做的事**：不在前端做任何指标计算（除 `steps_total` 预估）、不在前端存 API Key、不在前端拼 URL（端点是资源）。

---

## 2. 信息架构

```
#/login      登录（默认入口；未登录时任意路由都落在这里）
#/new        新建测试 —— 三步向导（核心页面）
#/run/<id>   运行监控 + 结果 + 交付物下载
#/history    历史记录（游标分页、按状态筛选）
#/catalog    能力清单（工具/能力、档位表、验收阈值模板）
```

---

## 3. 页面设计

### 3.1 新建测试（三步向导）

三个步骤**同页切换**（不跳路由），已完成步骤可点击回退，避免填一半丢掉状态。

```
┌─ 顶栏 ────────────────────────────────────────────────────────────────┐
│ [T] THVV  Subcontractor Verifier   新建测试 历史记录 能力清单   服务正常 v1.0.1 │
└───────────────────────────────────────────────────────────────────────┘
┌─① 被测端点──┐┌─② 测试计划──┐┌─③ 确认提交──┐
└─────────────┘└─────────────┘└─────────────┘

╭─ ① 被测端点 ────────────────────────── [选择已保存端点 ▾] ╮
│ 供应商名称 [智谱          ]   端点别名 [zhipu-glm52-prod  ] │
│ API 地址   [https://api.zhipuai.com/v1/chat/completions    ] │
│            须为完整请求路径，服务端不做拼接                    │
│ 协议 [openai ▾]  模型名 [glm-5.2 ]  API Key [••••••••     ] │
│                       只写，保存后仅显示后 4 位                │
│ Tokenizer [留空自动推断]  Temperature [auto ▾]  max_tokens[4096]│
│            按模型名自动匹配  GLM-4.6（auto_detected，已缓存）   │
│                                                              │
│ [保存并预检]                              [下一步：测试计划 →] │
│ ── 预检结果 ────────────────────────────────────────────────  │
│ ✓ protocol     openai                                        │
│ ✓ connectivity HTTP 200, first token 412ms                   │
│ ✓ tokenizer    zai-org/GLM-4.6 cached                        │
│ ! datasets     512k/1m 未构建（warn，不阻塞）                  │
╰──────────────────────────────────────────────────────────────╯
```

关键点：

1. **「保存并预检」= `POST /targets` + `POST /targets/{id}/probe`**，一个按钮串两步。预检结果用 `✓ / ! / ✕` 三色区分 `ok / warn / error`——`warn`（如超长档位数据集未构建）**不阻塞**后续测试，只提示。
2. **Tokenizer / Temperature 是「可覆盖的自动推断」**：默认留空，服务端推断后回显 `value + source + cached`。用户能看到"它替我选了什么"，而不是黑盒。
3. **API Key 只写**：提交后输入框清空并改为占位符「已保存，留空则不修改」，下方显示 `api_key_hint`（后 4 位）。任何响应/日志都不出现明文。
4. 填完第 1 步才允许进第 2 步（未登记端点时「下一步」按钮给出明确提示）。

```
╭─ ② 测试计划 ────────────────────────────────────────────────╮
│ ┌ 单档测试 ┬ 全档位矩阵 ┐   ← 分段控件                        │
│                                                              │
│  [单档]  档位 [64k · 56000~72000 tokens ▾]                   │
│          并发 [8 ▾]  请求总数 [200]                           │
│          并发上限 64；该档数据集已就绪                         │
│                                                              │
│  [全档]  档位 [1k][9k][16k][32k][64k][128k][200k] [512k̶][1m̶] │
│          并发梯度 [1,8,32,64,128]  冷却[120]s  早停[0]        │
│          TPM 预算 [1000000]  失败处理 [跳过该档剩余并发 ▾]     │
│          ▸ 单档覆盖（并发/请求数）  ← 折叠的高级项             │
│          ┌──────────────────────────────────────────────┐   │
│          │ 本次共 23 个执行步骤，约 2,340 条请求，档间冷却  │   │
│          │ 合计 12m。全档位常达数小时（串行执行）。        │   │
│          └──────────────────────────────────────────────┘   │
│                                                              │
│ ── 验收门槛 ────────────────────────────────────────────────  │
│  阈值模板 [perf-sla-v1 ▾]  你的 TPM 配额 [5000000]  模式[仅报告▾]│
│ ── 报告 ────────────────────────────────────────────────────  │
│  署名 [智谱]  标题 [留空自动生成]  产物 [✓HTML] [✓Excel]       │
│                                                              │
│ [← 上一步]                              [下一步：确认 →]      │
╰──────────────────────────────────────────────────────────────╯
```

关键点：

5. **模式切换即语义切换**：单档 = `mode:single`（`buckets[1]` + `parallel_ladder[1]` + `number`）；全档 = `mode:matrix`（多档 + 梯度，`number` 由服务端按 `number_by_parallel` 决定）。UI 不做「高级参数拼接」，只暴露 run.sh 里已经存在的概念。
6. **档位 chip 灰显 = 数据集未构建**（`built_in:false`）。点不动，鼠标悬停给原因。这样用户不会提交一个必然 `dataset_missing` 的任务。
7. **实时预估条**：前端用与后端**同一份算法**（§6）算出 `steps_total / 请求数 / 档间冷却总时长`，用户提交前就知道这是"23 步 / 两小时"的量级，而不是点下去才发现。
8. **高级项折叠**：`bucket_overrides`（如 200k 补 16 并发）默认收起，展开是张表——既保留 `run.sh` 的能力，又不干扰主流程。

```
╭─ ③ 确认提交 ────────────────────────────────────────────────╮
│ ╭ 被测端点 ─────────────╮ ╭ 测试计划 ────────────────────╮ │
│ │ 名称  zhipu-glm52    │ │ 模式  全档位矩阵              │ │
│ │ 供应商 智谱           │ │ 档位  1k/9k/16k/32k/64k/...   │ │
│ │ 模型  glm-5.2        │ │ 梯度  1, 8, 32, 64, 128       │ │
│ │ 协议  openai         │ │ 冷却  120s                    │ │
│ │ Key   ••••••••Ab3d   │ │ TPM   1000000                 │ │
│ │ Tokenizer GLM-4.6(自动)│ │ 步骤  23                      │ │
│ ╰──────────────────────╯ │ 门槛  perf-sla-v1（仅报告）    │ │
│                          │ 产物  html + xlsx             │ │
│                          ╰───────────────────────────────╯ │
│ ▸ 请求体预览（POST /v1/runs）                                │
│ [← 上一步]                              [ 开始测试 ]          │
╰──────────────────────────────────────────────────────────────╯
```

关键点：

9. **请求体预览**：折叠区直接展示将要发送的 JSON。既是调试手段，也让"我到底提交了什么"完全透明（尤其 `gate`/`report` 这类可选块）。

### 3.2 运行监控（`#/run/<id>`）

```
┌ 运行中…  glm-5.2 · 性能压测              [running] [取消] [新建测试] ┐
│ run_01JB7R2K · 全档 7 个档位 / 梯度 1,8,32,64,128 · 创建于 16:43    │
│ ████████████░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░                   │
│ 7 / 23 步 · 正在跑 32k / 并发 64              预计剩余 42m           │
├─────────────────────────────────────────────────────────────────────┤
│ 成功率 99.2% │ TTFT P50 0.31s │ TTFT P90 0.44s │ TPOT 18.4ms │ …   │
├─────────────────────────────────────────────────────────────────────┤
│ #  档位  并发  请求数  状态        成功率   TTFT P50  Output tps  耗时 │
│ 1  1k    1     20     ✓ 成功      100.00%  0.31s     42.1        38s  │
│ 2  1k    8     60     ✓ 成功      100.00%  0.34s     41.7        41s  │
│ 3  1k    32    120    ⟳ 12/120    —        —         —           —    │
│ 4  1k    64    200    ○ 待执行     —        —         —           —    │
│ …                                                                    │
├─ 验收结论 ────────────────────────────────────────────────────────────┤
│ ✕ 未达标      模板 perf-sla-v1 · 违规 2 项                            │
│ 档位   步数  P50 最大值  P90 最大值  结论                             │
│ <128K  4     18.42s     41.10s      ✕ 不通过                          │
├─ 交付物 ──────────────────────────────────────────────────────────────┤
│ HTML_REPORT  性能测试报告.html      471 KB        [下载]               │
│ XLSX_REPORT  性能测试报告.xlsx       30 KB        [下载]               │
│ RAW_DB       benchmark_data.db      2.3 MB        [下载]               │
└───────────────────────────────────────────────────────────────────────┘
```

关键点：

10. **进入即订阅 SSE**，按事件增量更新：`step.started` 插入行、`step.progress` 更新该行（显示 `12/120`）、`step.finished` 落定数值、`run.progress` 推进进度条与 ETA。
11. **进度条按 `steps_total` 精确计算**：因为服务端在 `202` 时就能算出总步数（并发裁剪是纯函数），所以进度从第一个事件起就是稳定的，不会出现"跑着跑着总步数变了"。
12. **`status` 与「验收结论」分开显示**：`partial`（执行完成但有失败步骤）用橙色 pill，「未达标」用红色 badge。**不把"没达标"显示成"测试失败"**——这是协议里刻意分开的两个概念，UI 必须一致。
13. 终态后自动 `GET /results` + `GET /artifacts`，展开结论与下载区。SSE 中 `run.finished` 到达时立刻拉取（幂等，可重复）。
14. 取消需二次确认，并说明「已完成步骤的结果会保留，仍可生成报告」。

### 3.3 历史记录（`#/history`）

表格：创建时间 / 模型（含供应商）/ 类型 / 状态 pill / 步数 / 成功率 / 结论 / 查看。
游标分页（`next_cursor`），状态筛选直接映射 `?status=`。点击「查看」→ `#/run/<id>`，对终态 run 会立即渲染已存结果。

### 3.4 能力清单（`#/catalog`）

三块只读数据：工具与能力（`/tools`）、档位表（`/catalog/buckets`）、验收阈值模板（`/catalog/gates`）。
用途：**让用户知道"现在能测什么"**——尤其当 `eval` 是 `preview`、`functional` 是 `planned` 时，状态标记就是产品口径，而不是靠文档口口相传。

---

## 4. 页面动作 ↔ HTTP 接口对照表

所有请求以 `/v1` 为前缀；Web 侧认证走 Cookie（§5.1）。

| 页面动作 | 方法 & 路径 | 请求要点 | 响应用途 |
|---|---|---|---|
| 打开站点 | `GET /auth/session` | — | 200 → 进入；401 → `#/login` |
| 登录 | `POST /auth/login` | `{username, password}` | 204 + `Set-Cookie` |
| 退出 | `POST /auth/logout` | — | 清 Cookie |
| 首屏引导 | `GET /meta` | — | 版本、`limits`（并发上限）、协议枚举 |
| 首屏引导 | `GET /tools` | — | 工具卡片、capability 状态 |
| 首屏引导 | `GET /catalog/buckets` | — | 档位 chip、并发上限、`number_by_parallel` |
| 首屏引导 | `GET /catalog/gates` | — | 阈值模板下拉 |
| 打开「新建」 | `GET /targets` | `limit=20` | 「选择已保存端点」下拉 |
| 选择已有端点 | — | 本地填充表单，**不发请求** | — |
| 保存并预检（新建） | `POST /targets` | `TargetCreate` + `Idempotency-Key` | 回显 `api_key_hint` / `tokenizer` |
| 保存并预检（已有） | `POST /targets/{id}/probe` | `{timeout_sec:30}` | 预检清单 |
| 预检（新建后） | `POST /targets/{id}/probe` | `{timeout_sec:30}` | 同上（`warn` 不阻塞） |
| 计划模式切换 / 选档 / 改梯度 | — | **纯本地**：`computeSteps()` 预估 | 预估条（步数/请求数/冷却） |
| 确认页 | — | 纯本地渲染 + JSON 预览 | — |
| 开始测试 | `POST /runs` | `RunCreate` + `Idempotency-Key` | 202 → `run_id` → 跳 `#/run/<id>` |
| 进入运行页 | `GET /runs/{id}` | — | 初始快照（含 `progress`，`steps` 可选） |
| 进入运行页 | `GET /runs/{id}/events` | `EventSource` | 实时进度（§5.2） |
| 取消 | `POST /runs/{id}/cancel` | — | 幂等；终态返回 200 |
| 终态后 | `GET /runs/{id}/results` | — | 指标 + `gate_result` |
| 终态后 | `GET /runs/{id}/artifacts` | — | 下载列表 |
| 下载交付物 | `GET /artifacts/{id}` | `<a download>` | 文件流（中文名走 `filename*`） |
| 历史列表 | `GET /runs` | `limit` / `cursor` / `status` | 表格 + `next_cursor` |
| 能力清单 | `GET /tools` `/catalog/buckets` `/catalog/gates` | — | 三张只读表 |

> 页面**不调用**：`POST /runs/{id}/reports`（v1 的「重新生成报告」可作为一个按钮预留，当前未接）、`/results/failures`（失败明细页 v1.1）。

---

## 5. 浏览器侧的特殊约束（这一节是 Web 与 API 客户端的关键差异）

### 5.1 认证：为什么 Web 用 Cookie 而不是 Bearer

| 场景 | Bearer（`Authorization` 头） | HttpOnly Cookie |
|---|:---:|:---:|
| `fetch` 调 API | ✅ | ✅ |
| **`EventSource`（SSE）** | ❌ **无法设置自定义头**（标准限制） | ✅ 自动携带 |
| **`<a download>` 下载文件** | ❌ 无法带头 | ✅ 自动携带 |
| XSS 读取凭据 | ⚠️ JS 可见 token | ✅ HttpOnly 不可读 |

结论：Web 端登录后下发 **HttpOnly + Secure + SameSite=Lax** 的会话 Cookie（`thvv_session`，12h），
SSE 与文件下载直接用原生能力，无需在 JS 里拼 `?access_token=`（那样会把凭据写进日志、Referer 与浏览器历史）。

服务端同时保留 Bearer：外部客户端/CI 用 Bearer，Web 用 Cookie，两套并存互不干扰（中间件先看 Cookie，再看 `Authorization`）。

若确实需要无 Cookie 部署（跨域 SPA），协议支持 `GET /runs/{id}/events?access_token=<token>`，但**必须**在反代层脱敏访问日志，并在文档中标注为不推荐。

### 5.2 SSE 的浏览器行为与前端策略

| 行为 | 说明 | 前端策略 |
|---|---|---|
| 自动重连 | `EventSource` 断线后按服务端 `retry: 3000` 自动重连 | 不做额外重连；只在 `onerror` 给出「正在重连」提示 |
| 断点续传 | 浏览器会自动带 `Last-Event-ID` 头（值为最后收到的 `id`） | 服务端保留每 run 最近 1000 条事件即可无缝续传 |
| 心跳 | 服务端每 15s 发 `: heartbeat` 注释行 | 无需处理；用于穿透反代空闲超时（Nginx `proxy_read_timeout` 建议 ≥ 60s） |
| 终态 | 服务端推完 `run.finished` 后关闭连接 | `run.finished` 到达时主动 `close()`，避免无谓重连 |
| 事件过期 | `Last-Event-ID` 早于保留窗口 → `409 event_history_gone` | 降级为 `GET /runs/{id}` + `GET /results` 轮询一次 |
| 多标签页 | 每个标签独立订阅 | 允许；服务端按 run 广播，不限制订阅数 |

### 5.3 文件下载

- 列表里的 `download_url` 是稳定的 `/v1/artifacts/{id}`，直接 `<a href download>`，**不在 JS 里读 blob**（大文件省内存，且支持浏览器断点续传与 `If-None-Match`）。
- 中文文件名由服务端给 `Content-Disposition: attachment; filename*=UTF-8''%E6%80%A7%E8%83%BD...`，浏览器自动还原。
- `ETag = sha256`，重复下载命中 `304`。

### 5.4 幂等与防重

`POST /targets` 与 `POST /runs` 均由前端生成 `Idempotency-Key`（`crypto.randomUUID()`），**按钮点击期间禁用**（双保险）。
同一 key 重放返回原对象 → 用户在弱网下重试不会产生两条 run（这对"跑两小时的任务"尤其重要）。

---

## 6. 前后端数字一致性：`computeSteps` 双实现

前端要显示「本次共 23 个执行步骤」，就必须知道服务端的裁剪规则。这是**唯一一处前后端重复实现的逻辑**，规则只有 4 条：

```
maxPar = min(catalog.max_parallel[bucket], overrides[bucket].max_parallel ?? ∞, tpm_budget>0 ? floor(tpm_budget/bucketTokens) : ∞)
parallels = sortedUnique(ladder ∪ overrides[bucket].parallels) filtered ≤ maxPar
if parallels empty → 该档跳过
steps = Σ parallels
```

约束：

1. 前端实现（[`../../server/web/app.js`](../../server/web/app.js) `computeSteps`）**必须逐行对照服务端**，注释里标同一份伪代码出处（`perf/run.sh: cmd_bench_all`）。
2. `POST /runs` 返回的 `plan.steps_total` 是**权威值**。前端显示预估，提交后以服务端返回为准；若两者不一致，控制台输出告警（便于发现规则漂移）。
3. 纯展示用途，**不参与任何指标计算**。所有 KPI/分位数/TPM 一律来自 `GET /results`。

---

## 7. 状态与错误呈现规范

### 7.1 Run 状态 → UI

| status | pill 文案 | 颜色 | 进度条 | 「取消」按钮 |
|---|---|---|:---:|:---:|
| `queued` | 排队中 | 灰 | 2% 起步 | 可点 |
| `preparing` | 准备中 | 青 | 2% | 可点 |
| `running` | 运行中 | 青（带脉冲） | 按 steps | 可点 |
| `reporting` | 生成报告中 | 蓝 | 100% | 禁用 |
| `succeeded` | 已完成 | 绿 | 100% | 禁用 |
| `partial` | 部分完成 | **琥珀** | 100% | 禁用 |
| `failed` | 失败 | 红 | 保留最后值 | 禁用 |
| `canceled` | 已取消 | 灰 + 删除线 | 保留最后值 | 禁用 |

> 未知 status（协议演进新增值）→ 显示原始字符串 + 灰色，且**视为非终态继续订阅**（协议 §8 的兼容约定）。

### 7.2 HTTP 错误 → 交互

| HTTP / code | UI 行为 |
|---|---|
| `400 validation_failed` | 定位到对应字段（`problem.details[].field`），输入框标红 + toast 首条 |
| `401 unauthorized` | 清状态 → `#/login`，toast「会话已过期，请重新登录」 |
| `403 forbidden` | toast「当前账号无此操作权限」，不改路由 |
| `409 conflict` | toast（重名端点 / 幂等键参数不一致） |
| `422 target_unreachable` / `tokenizer_unavailable` | 预检面板内标红该 check 项 + 「重试」按钮 |
| `422 dataset_missing` | toast + 提示先去构建数据集（指向 `/catalog/buckets` 的灰显项） |
| `429 quota_exceeded` | toast「服务端并发已满，稍后自动重试」，读 `Retry-After` 后自动重发提交 |
| `501 not_implemented` | 若 `available_at` 存在，显示「该能力将于 `v1.2` 提供」 |
| `5xx` / 网络错误 | toast + 保留表单状态，可重试；不做自动跳转 |

原则：**提交前的失败绝不丢用户输入**；提交后的失败以 run 状态呈现（`failed` + `error` 详情），不弹模态框打断。

---

## 8. 前端代码结构与托管

```
server/web/               # go:embed 打包进二进制；也支持 ui.dir 指向磁盘
├── index.html            # 5 个视图同页，无内联脚本/样式（便于 CSP）
├── app.js                # 状态、路由、API 封装、SSE、渲染、（Mock 后端）
├── styles.css            # CSS 变量 + 亮/暗色自适应，无预处理器
└── embed.go              # //go:embed 三个静态文件
```

Go 侧托管（Go 1.22+）：

```go
//go:embed all:web
var webFS embed.FS

sub, _ := fs.Sub(webFS, "web")
mux.Handle("GET /ui/", http.StripPrefix("/ui/", http.FileServerFS(sub)))
mux.Handle("GET /", http.RedirectHandler("/ui/", http.StatusFound))
```

- 因为用 **Hash 路由**，`/ui/` 下不需要任何 SPA fallback（`/ui/#/run/xxx` 对服务器来说只是 `/ui/`）。
- `index.html` 不含内联 script/style，可直接上严格 CSP：
  ```
  Content-Security-Policy: default-src 'self'; script-src 'self'; style-src 'self';
    img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'
  ```
- 静态资源带 `Cache-Control: public, max-age=3600` + 内容哈希查询参数（`app.js?v=<build>`）即可。

### 8.1 无前端 Mock（已移除）

v1.1 起页面只连同源 Go 服务（Cookie 会话 + SSE），不再提供 `?mock=1` 纯前端演示模式：
接口返回什么，页面就渲染什么，避免"演示数据看起来正常、真实链路不可用"的假阳性。
离线演示请用 `config/thvv.local.yaml` 的 `builtin` 引擎起一个真实服务端。

---

## 9. 可访问性 / 响应式 / 性能

| 项 | 措施 |
|---|---|
| 键盘可达 | 所有操作是原生 `button`/`input`/`select`；tab 顺序即视觉顺序；`details/summary` 支撑折叠 |
| 颜色不作为唯一信息 | 状态同时有文字（「部分完成」）和颜色；预检用 `✓ ! ✕` 字符 |
| 对比度 | 亮/暗两套 CSS 变量的正文对比度 ≥ 4.5:1 |
| 屏幕阅读器 | 进度条容器 `role="progressbar"` + `aria-valuenow`（待补）；状态变化用 `aria-live="polite"` 的 toast |
| 响应式 | `grid-template-columns` 在 ≤760px 降为单列；表格横向滚动；顶栏导航折叠为图标（v1.1） |
| 首屏 | 3 个静态文件（~80KB 未压缩），无第三方依赖，无阻塞字体；4 个并行 API 请求 |
| 长列表 | 历史记录游标分页，每页 20；步骤表全量渲染（单 run 最多约 30 行） |
| 长任务 | 页面刷新不丢进度（run 状态在服务端）；SSE 断线自动续传 |

---

## 10. 实施顺序

| 阶段 | 内容 | 依赖 |
|---|---|---|
| W0 | 本文 + 原型（已完成），UI 评审 | — |
| W1 | 登录/会话（Cookie 中间件）+ 首屏 4 个 GET 接通真实后端 | M0 |
| W2 | 向导步骤 1（target 创建 + probe）接通 | M1 |
| W3 | 向导步骤 2/3 + `computeSteps` 双实现对齐 | M2 |
| W4 | 运行页 SSE 接通 + 取消 | M2 |
| W5 | 结果页 + 下载 | M4 |
| W6 | 历史记录 + 能力清单 | M3 |
| W7 | a11y 补齐（progressbar aria、表格 caption）、暗色细节、i18n（`en-US`） | — |

---

## 11. 待决问题

1. **多语言**：当前文案硬编码中文。是否引入 `i18n.js`（约 200 条 key）？协议里已有 `Accept-Language`，服务端 `problem.detail` 也会跟着切换。
2. **断点续填**：向导状态是否落 `sessionStorage`？产品上填一半关掉浏览器的概率不低。
3. **失败明细页**：`GET /results/failures` 已定协议，前端是做成运行页的抽屉还是独立路由？
4. **实时曲线**：`step.progress` 里只有 TTFT 与成功率。若要在运行中画"并发-延迟"曲线，需要服务端额外推 `tps`（当前只在 `step.finished` 给）。
5. **供应商只读门户**：是否需要一个不含"新建测试"的 `viewer` 视图（给供应商看自己的报告）？涉及多租户（DESIGN.md §10.4）。
6. **移动端**：是否要支持手机上看进度（当前仅保证不破版）。

---

## 12. 实现记录（与设计稿的差异）

实现于 `server/`，已用 headless Chrome 跑通「登录 → 填表 → 预检 → 提交 → SSE 进度 → 结果 → 下载」。

| 设计稿 | 实现 | 说明 |
|---|---|---|
| `docs/web/` | `server/web/` | 移入 Go 模块以便 `go:embed`；本目录不再保留副本 |
| 首屏 4 个并行 GET | 同 | 但改为**登录后**才发起（未登录时这些接口是 401） |
| 设计稿的 `?mock=1` 演示模式 | 已移除 | 页面只连真实服务端；离线演示改用 `config/thvv.local.yaml`（builtin 引擎） |
| — | 新增 `GET /v1/auth/session` 返回 `kind`/`expires_at` | 便于前端展示会话状态 |
| — | 步骤表新增「操作」列 | 运行中 → 「取消」（kill 该 step 后继续）；待执行 → 「跳过」（引擎执行到该 step 时跳过）；终态后不显示按钮 |
| — | cache 面板新增「负载模式」 | 手动（填每轮输入/输出）与「目标命中率」（方向 min/max 二选一 + 数值下拉）互斥；命中率模式由服务端反推每轮输入/输出并回填 |

实现期发现并修掉的两个交互缺陷（都已回归覆盖）：

1. **未登录取数导致档位表为空**：`boot()` 在登录前就拉 `/catalog/buckets`，401 被吞掉后
   `state.buckets` 一直为空，「测试计划」里档位下拉是空的。改为登录成功后重新拉取参考数据，
   并在进入「新建测试」时兜底补拉。
2. **Toast 挡住主按钮**：右下角提示浮层覆盖了「下一步 / 开始测试」按钮，
   自动化点击报 *Node is not clickable*。给容器加 `pointer-events: none` 解决。

另外两处**前后端一致性**的修正（`domain.ComputeSteps` 与 `app.js computeSteps` 同步改）：

- `plan.number` 必须覆盖 `number_by_parallel`（对应 `run.sh bench <bucket> <N> <P>` 的 N）
- `bucket_overrides[b].number` 对该档**所有并发**生效（对应 `N_<label>` 环境变量语义）
