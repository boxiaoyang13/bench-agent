# ISSUES —— 评测/压测过程中发现的问题单

> 记录实测暴露、尚未在代码或配置层收口的问题。每条含：现象、证据、根因、建议动作。
> 状态：`open`（待处理）/ `workaround`（已规避）/ `fixed`（已修）

---

## ISSUE-001 多模态数据集对纯文本端点 100% 失败，且要烧掉数小时才报错

- **状态**：`open`（本轮已用 limit=1 探测规避）
- **影响**：`hallusion_bench` `math_vision` `msr_vtt` `mvbench` `tvbench` `videomme_v2` 共 6 个数据集
- **现象**：被测端点 `glm-5.3-test` 是纯文本模型，多模态数据集每条请求都被供应商拒绝；
  evalscope 的 `ignore_errors: true` 把错误静默吃掉，最后才报「没有有效样本」。
- **证据**
  - `run_28143eacead1a7f0`（`hallusion_bench`）：**5706 次** `HTTP 400
    {'message': 'glm-5.3-test is not a multimodal model'}`，951 条样本全部被丢弃，
    耗时 **2h48m**，产物为零，`run.status=failed`。
  - 本轮 limit=1 探测（`run_46f944f0f71ae7a6` / `run_ce62ac8272aaf848` / `run_7f4201fd25e3cba3` /
    `run_4835e6faacbd3dbe`）全部同样报 `not a multimodal model`。
  - `run_2929f6cf0cf56dd0`（`tvbench`）是**另一个原因**：`ValueError: Benchmark 'tvbench' not found`，
    见 ISSUE-002。
- **根因**
  1. 协议层不知道「数据集有模态」「target 能不能看图」。`GET /v1/catalog/datasets` 只给
     `kind=image|video|text|agent`，没有 `needs_vision`，`POST /v1/runs` 也不做任何交叉校验。
  2. evalscope 侧 `ignore_errors: true` + 5 次重试（10s 退避）→ 每条约 60s，**失败成本被放大 5 倍**，
     且不会 fail-fast。
- **建议动作（按性价比排序）**
  1. `catalog` 增加 `needs_vision: true`（由 `kind ∈ {image,video}` 推导即可），前端置灰 / 提交时
     对 `target` 未声明多模态能力的 run 直接 `400 validation_failed`。
     → 需要 target 侧新增 `supports_vision`（或 `modalities: [text,image]`）字段，probe 时探测。
  2. 服务端「连续 N 条相同 400」快速判死：解析引擎输出，同一 4xx 消息命中 ≥20 次且成功率 0 →
     立刻终止该 Step，`error.code` 给 `upstream_rejected`，省下 2h48m。
  3. 短期规避：文本模型只选 `kind=text|agent` 数据集；多模态数据集先用 `limit: 1` 探测再决定是否全量。
- **相关**：`docs/EVAL.md` §10、`docs/API.md` §5.3

---

## ISSUE-002 `tvbench` 在 evalscope 1.9.0 里没有对应 benchmark

- **状态**：`workaround`（本轮跳过该数据集）
- **现象**：`run_2929f6cf0cf56dd0` 25s 内失败：`ValueError: Benchmark 'tvbench' not found.
  Did you mean: 'mvbench', 'tir_bench'?`
- **根因**：`thvv/eval/scripts/run_eval.py` 的 `SUPPORTED_DATASETS['tvbench']` 声明了该数据集
  （`multimodal: video`，10 个子集），但 evalscope 1.9.0 的 `BENCHMARK_REGISTRY` 没有 `tvbench`
  这个注册名，脚本也没有做「先用 mvbench 的 tvbench 子集」之类的映射。
- **建议动作**：二选一
  - 在 `run_eval.py` 里把 `tvbench` 映射到 evalscope 支持的名称/子集（如 mvbench 的 tvbench 子集）；
  - 或从 `SUPPORTED_DATASETS` 摘掉，避免 `/v1/catalog/datasets` 继续对外宣称支持。
- **相关**：`thvv/eval/scripts/run_eval.py:196`

---

## ISSUE-003 排队中的 run 被取消后仍会被 worker 拉起（已修复）

- **状态**：`fixed`（2026-11-08）
- **现象**：`POST /v1/runs/{id}/cancel` 对 `queued` 的 run 返回 202 且状态变 `canceled`，
  但 worker 出队时照旧执行（实测 `mmlu_pro` 被取消后又跑了起来，堵住整个队列 ~11h）。
- **根因**：`Cancel` 只改了 DB，没有同步内存里的 `liveRun`；`worker`/`execute` 也没有终态检查。
- **修复**：`Cancel` 同步 `lr.run.Status`；`worker` 出队前与 `execute` 拿到 target 信号量后各做一次
  终态检查（`skipIfDone`）。回归测试：`internal/httpapi/eval_failure_test.go:TestCancelQueuedRunNeverStarts`

---

## ISSUE-004 供应商网关间歇性 502，样本被静默丢弃并污染分数

- **状态**：`open`（外部问题，已用「重跑」规避）
- **现象**：`run_cba1aaf48f59e298` 在 16:20–18:11 之间收到 **3210 次**
  `HTTP 502 upstream connection failed: dial tcp 192.168.60.221:8000: connect: connection refused`
  （供应商网关自己的后端不可用），导致 `aime26` 丢 240 题、`aime25` 丢 180 句、`hle` 丢 180 题，
  最终分数是「部分样本」的分数（覆盖率 50% / 62.5% / 25%）。
- **建议动作**
  1. 服务端把「跳过样本占比」作为一等指标：`skipped/total > 阈值` → `status=failed`（现在是照常出分）。
  2. 出分时在 `summary` 里显式给出 `coverage`，并在报告首页提示「覆盖率不足，勿用于验收」。
  3. 运维侧：`read_timeout_sec` ≥ 600，供应商故障窗口过后按数据集重跑。
- **相关**：`docs/EVAL.md`、`run.error` 的 `detail`（现已带 `ignore_errors 容错跳过 N 条样本`）

---

## ISSUE-005 `THVV_STORE__SQLITE__PATH` 等环境变量被静默忽略，临时实例会连到生产库（已修复）

- **状态**：`fixed`（2026-11-08）
- **现象**：起临时实例做联调/演示时设置了 `THVV_STORE__SQLITE__PATH=/tmp/x.db`、
  `THVV_PATHS__RUNS_DIR=/tmp/x-runs`，但实例实际打开的是 `data/thvv.db`（启动日志
  `db=/data/bench/bench-agent/server/data/thvv.db`），测试 run 直接写进了生产库，
  历史记录里因此堆了一批 `x.example` / `127.0.0.1:18199` / mock 的脏数据。
- **根因**：`config.setByPath` 只实现了 `server.listen/base_path/write_timeout`、`roots.*`、
  `limits.*`、`observability.*` 四组；其余段（`store`、`paths`、`server.ui.*`）落到最后的
  兜底 `return nil`，**不报错也不生效**。
- **修复**：补齐 `store.sqlite.{path,busy_timeout,max_open_conns,wal}`、`paths.{runs_dir,logs_dir,
  datasets_dir,cache_dir,templates_dir}`、`server.ui.{dir,path,enabled}`；单测
  `config.TestEnvOverrides` 覆盖这三类。
- **备忘**：临时实例仍建议显式 `-config` 指向一份独立 YAML，别依赖环境变量“看起来生效”。

---

## ISSUE-006 报告下载失败（产物下载链路）+ perf 产物整文件读内存（已修复）

- **状态**：`fixed`（2026-11-09）
- **现象**：`run_d2e9c686965b6045`（perf，5 档全成功）跑完后，页面点「下载 性能测试报告.html / .xlsx」
  浏览器报失败；服务端日志显示对应 GET 返回 200（文件本身可正常打开：xlsx 45 列 6 行、HTML 结构完整）。
  访问路径为 `https://<隧道域名>:12443/ui/#/run/...`（请求都来自 127.0.0.1，即经隧道转发）。
- **根因（推断 + 加固）**：下载走 `<a href="/v1/artifacts/{id}" download>` 的**浏览器导航**，
  这种路径依赖 Cookie、且失败时前端拿不到任何原因；同时 `Content-Disposition` 只给了 RFC 5987 的
  `filename*=UTF-8''...`（中文名），部分反向代理/网关对非 ASCII 头值处理不友好，会表现为「下载失败」。
- **修复**
  1. 前端改为统一 `downloadArtifact()`：小文件用 `fetch`（带会话）取 blob 再落盘，非 2xx 时把
     `problem.detail` 直接 toast 出来（不再静默失败）；>32MB 的大文件仍交给浏览器原生导航（流式、省内存）。
  2. 服务端 `Content-Disposition` 同时给 ASCII fallback：`filename="report.xlsx"; filename*=UTF-8''...`，
     并加 `X-Content-Type-Options: nosniff`。
  3. 顺带修：perf 的 `benchmark_data.db` / `run.log` 归档原来用 `os.ReadFile` **整文件进内存**
     （该 run 的 db 有 212MB），既绕过 `limits.artifact_max_bytes` 也容易 OOM → 改走流式
     `addArtifactFile`；本机 `artifact_max_bytes` 提到 1GiB，避免全档位 db 被判超限跳过。
- **验证**：新增 `TestArtifactDownloadHeaders`（两个 filename 段、无非 ASCII 字节、nosniff）；
  `go test ./...` 连跑多轮通过；本机实测两个文件下载 200 且 xlsx/HTML 均可正常打开。
- **备注**：若仍失败，新前端会弹出具体原因（如 `HTTP 401` / `文件为空` / 网关错误），据此可定位到代理层。

---

## ISSUE-007 压测全失败时看不到原因（evalscope 1.9.x 不落 error 列）

- **状态**：`fixed`（2026-11-09，加了服务端上游探测）
- **现象**：`run_8c539fe1ff84060f`（9 档 × 梯度 8/16/32）1k/9k 全绿，**16k p32 起成功率 0%**，
  32k/64k/128k/200k 连 p8 都只有 0~5%；报告里只有「成功率 0%」，
  `benchmark_data.db` 里失败行的 `response_messages` 是空列表、`latency` 为 NULL，
  `benchmark_summary.json` 显示 `Test Duration 0.0s`（= 请求瞬时失败，不是 600s 读超时）。
- **根因**：evalscope 1.9.x 的 `result` 表不写 `error`/`status_code`，perf 也不打印逐条错误，
  所以「429 / 502 / 连接重置 / 鉴权失败」在平台侧全部退化成「失败」。
  该 run 的时间窗（03:16–03:41）与供应商网关上游故障高度吻合：
  复测同一 key 单请求 32k/64k/128k prompt 均 HTTP 200、8 并发 32k × 16 条全成功，
  说明不是并发上限也不是长上下文限制，而是当时的瞬时故障。
- **修复**：新增 `runmgr.maybeProbeUpstream` —— 只要有档位成功率 ≤1%，服务端再发一条
  最小 completion 请求，把 `{ok,status,error_class,body_excerpt,elapsed_ms}` 写进
  `summary.upstream_probe`（并记 `summary.zero_success_buckets` + run.log warn）。
  下次同样的故障会直接显示 `status=502, body_excerpt=upstream connection failed: ...`。
- **验证**：`TestProbeEndpoint`（502 带原话 / 200 / 连不上分类）。
