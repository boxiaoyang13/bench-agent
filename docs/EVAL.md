# 效果评测（eval）接入设计

> 目标：把 `thvv/eval` 的 17 个数据集评测能力接进 Go 服务端，与 `perf` 共用
> Target / Run / Step / Artifact / SSE / 报告下载这套协议，不新增第二套资源模型。
>
> 状态：v1 已实现（`capability=eval`，`status=ga` 可配置；含 eval-sla-v1 分数验收，见 §9.1）。
> 相关：[`API.md`](./API.md) §5.3 · [`CONFIG.md`](./CONFIG.md) · [`DESIGN.md`](./DESIGN.md) ·
> [`WEB-UI.md`](./WEB-UI.md) · Python 侧 [`thvv/eval/README.md`](../../../TokenHub-Vendor-Verifier/thvv/eval/README.md)

---

## 1. 设计边界

| 做 | 不做（v1） |
|---|---|
| 单/多数据集评测，复用现成 `run_eval.py` | 不重写评测引擎，不碰 evalscope 适配层 |
| 每个数据集一个 Step，独立成功/失败 | 不做数据集内部的逐题进度/逐题取消 |
| 结构化分数 + `eval_report_v2.html`/CSV/JSON 交付 | 不在 Go 侧重算分数（分数以 Python 落盘为准） |
| Judge 模型凭据由 Run 级 plan 传入 | 不做 eval 的 SLA 门禁（对照《效果验收标准.xlsx》留 v1.1） |
| 与 perf 相同的 Target/凭据/限流/SSE/取消语义 | 不做 Docker 沙箱编排（依赖 Docker 的数据集需宿主自备） |

关键原则：**Go 只做编排与归档，评测口径由 `thvv/eval` 决定**。这样上游
`run_eval.py` 迭代（新增数据集、改判分）不需要改 Go 代码。

---

## 2. 与 `thvv/eval` 的契约

### 2.1 调用方式：一数据集一进程

```
python3 <thvv>/eval/scripts/run_eval.py \
  --datasets <dataset> \
  --model_name <MODEL> \
  --provider <PROVIDER> \
  --base_url <API_URL 去掉 /chat/completions | /v1/messages> \
  --api_key <TARGET_KEY> \
  --output_dir <runDir>/eval/<dataset> \
  [--limit N] [--repeats K] [--eval_batch_size B] \
  [--temperature T] [--max_tokens M] [--thinking true|false] [--thinking-type t] \
  [--judge_model m] [--judge_base_url u] [--judge_api_key k]
```

**为什么不让它一次跑多个数据集**：`run_eval.py` 里
`output_dir = args.output_dir or get_result_dir(...)`，一旦显式传 `--output_dir`，
所有数据集会共用同一个 work-dir，`eval_summary.json` / `eval_report_v2.html`
互相覆盖；不传又只能落在 `thvv/eval/results/<provider>-<model>-<ts>/`，与服务的
`data:runs/<run_id>` 产物目录脱节。因此 v1 固定「一数据集一进程」：

- 每个数据集产物天然隔离，失败互不影响；
- Step 与进程一一对应，取消、进度、失败原因都能精确归因；
- 归档时由 Go 统一搬进 `data:runs/<run_id>/artifacts/`。

### 2.2 参数映射（plan → argv）

| plan 字段 | argv | 默认 |
|---|---|---|
| `datasets[]` | `--datasets <name>`（逐数据集） | 必填 |
| — | `--model_name` | `target.model` |
| — | `--provider` | `plan.provider_name` → `target.vendor` → `unknown` |
| — | `--base_url` | `target.api_url` 去掉 `/chat/completions`、`/v1/messages` |
| — | `--api_key` | Target 的解密凭据 |
| `limit` | `--limit` | 不传＝数据集默认（全量） |
| `repeats` | `--repeats` | 不传＝数据集默认（见 §3.1） |
| `eval_batch_size` | `--eval_batch_size` | 不传＝数据集默认 |
| `request.temperature` | `--temperature` | 不传＝引擎按模型推断 |
| `request.max_tokens` | `--max_tokens` | 不传＝引擎按模型/模态推断 |
| `thinking` | `--thinking` | 默认 `true` |
| `thinking_type` | `--thinking-type` | 按供应商映射 |
| `judge.api_key` | `--judge_api_key` | 必填（若所选数据集需要 Judge） |
| `judge.model` | `--judge_model` | `deepseek-v4-pro` |
| `judge.base_url` | `--judge_base_url` | `https://api.deepseek.com/v1` |

`base_url` 推导规则与 `thvv/eval/run.sh` 完全一致，避免出现 `/chat/completions/chat/completions`。

### 2.3 退出码与成功判定

- `run_eval.py` 单数据集成功 → `0`；失败 → `1`（预检查失败同样 `1`）。
- 但**不以退出码作为唯一依据**：进程结束后读 `<output_dir>/eval_summary.json`，
  以 `success` 字段 + 文件存在性判定 Step 结果；退出码仅用于日志与错误分类。
- `run_eval.py` 内部已实现对 429 的等待重试（打印 `🔄 第 N/M 次尝试`），
  Go 侧不再叠加请求级重试，避免放大限流。

---

## 3. 数据模型

### 3.1 数据集目录（配置驱动）

`engines.thvv.capabilities.eval.datasets[]`，每项：

```yaml
- name: aime26
  display_name: AIME 2026
  kind: text            # text | agent | video | image
  needs_judge: false
  default_repeats: 16
  default_eval_batch_size: 30
  default_limit: 30     # 可选：UI/plan 缺省 limit
  excluded_from_all: false
```

代码内置 17 个数据集的默认值（与 `run_eval.py` 的 `SUPPORTED_DATASETS` 对齐），
YAML 不写就用内置表；写则整表覆盖。`excluded_from_all` 的数据集（`aime25`、
`swe_bench_pro`）只有显式点名才跑。

### 3.2 plan

在 perf 字段之外新增（见 `API.md` §5.3）：

```json
{
  "mode": "single",
  "datasets": ["aime26", "gpqa_diamond"],
  "limit": 30,
  "repeats": 1,
  "eval_batch_size": 10,
  "thinking": true,
  "thinking_type": "enabled",
  "judge": {"model": "deepseek-v4-pro", "base_url": "https://api.deepseek.com/v1", "api_key": "sk-..."},
  "provider_name": "zhipu",
  "request": {"temperature": 0, "max_tokens": 8192}
}
```

规范化规则：

1. `datasets` 去重、保序；空 → `400`；未知数据集 → `400`（列 `/v1/catalog/datasets`）；
2. `limit ≤ 0` 表示全量；上限 `limits.plan.max_number`；
3. `repeats`/`eval_batch_size` 缺省取数据集默认；`repeats ≤ 50`、`eval_batch_size ≤ 256`；
4. `provider_name` 缺省 `target.vendor`，再缺省 `unknown`；
5. 所选数据集里任一 `needs_judge=true` 时 `judge.api_key` 必填，否则 `400 validation_failed`。

### 3.3 Step = 数据集

| Step 字段 | 含义 |
|---|---|
| `index` | 数据集在 plan 中的顺序 |
| `bucket` | 数据集名（复用字段，UI 列名显示为「数据集」） |
| `parallel` | `eval_batch_size` |
| `number` | `limit`（0 = 全量） |
| `status` | `pending/running/succeeded/failed/canceled/skipped` |
| `success_rate` | v1 固定为 1.0 / 0.0（无“请求成功率”概念，用分数替代） |
| `ttft_p50_sec`/`output_tps`/`duration_sec` | 留空；耗时用 `duration_sec` 记录该数据集评测秒数 |

`steps_total = len(datasets)`。

### 3.4 Run 结果（`GET /v1/runs/{id}/results`）

```json
{
  "run_id": "run_...",
  "capability": "eval",
  "summary": {
    "verdict": "pass",
    "datasets_total": 2,
    "datasets_succeeded": 2,
    "datasets_failed": 0,
    "score_avg": 0.63,
    "elapsed_sec": 812
  },
  "eval": {
    "model": "glm-5.3-test",
    "provider": "turbo",
    "datasets": [
      {
        "dataset": "aime26",
        "label": "aime26",
        "success": true,
        "score": 1.0,
        "metric": "mean_acc",
        "num_samples": 4,
        "limit": 4,
        "repeats": 1,
        "eval_batch_size": 4,
        "retries_used": 0,
        "rate_limited": false,
        "elapsed_sec": 170.0,
        "skipped": 0,
        "cache_hit_rate": 0.0,
        "artifacts": ["aime26-eval_report_v2.html", "aime26-eval_summary.json", "aime26-per_sample_details.csv"]
      }
    ]
  }
}
```

`verdict`：全部成功 `pass`；部分失败 `partial`；全部失败 `fail`。
`metrics`（perf 字段）在 eval run 中为空对象，客户端按 `capability` 分支渲染。

---

## 4. 执行流程

```
POST /v1/runs (capability=eval)
  └─ normalizeEvalPlan → steps[dataset]
  └─ 入队（与 perf 共用队列、并发上限、同 target 串行）

worker:
  ├─ 解密 target 凭据
  ├─ 组装 Spec（Target/Plan/Steps/WorkDir）
  ├─ 每个 step:
  │    ├─ emit step.started
  │    ├─ spawn: python run_eval.py ... --output_dir <workdir>/eval/<ds>
  │    ├─ stdout → 日志 + 进度解析 + SSE
  │    ├─ 进程结束 → 读 eval_summary.json → step.succeeded/failed
  │    └─ emit step.finished
  ├─ 汇总 EvalMetrics → run.summary / metrics.json
  └─ 归档 artifacts → run.succeeded/partial/failed
```

取消：与 perf 相同，`GET /v1/runs/{id}/cancel` → SIGTERM 进程组 → 宽限期 SIGKILL，
当前数据集 Step 标 `canceled`，其余 `skipped`。

---

## 5. 进度与日志

`run_eval.py` 的 stdout 逐行读取（ANSI/回车刷屏已清洗），事件映射：

| 输出模式 | 事件 | 说明 |
|---|---|---|
| `📊 \[label\] ...` | `run.log` | 数据集开始横幅 |
| `🔄 第 N/M 次尝试` | `run.log`（warn） | 429 等待重试 |
| `Running\[eval\]:\s*(\d+)%` | `step.progress` | 百分比 → `completed/total=100`；数值不变不重复上报 |
| `✅ 评测完成! 耗时: (\d+)s` | `step.finished`（暂记成功） | 最终以 `eval_summary.json` 为准 |
| `❌ 评测失败` / `❌ 预检查失败` | `run.log`（err） | 失败原因进日志与 artifacts |
| 其它 | `run.log` | 截断后进 SSE，落 `run.log` 产物 |

**脱敏**：命中 `--api_key/--judge_api_key` 的值、以及 model/judge 的明文 key 一律替换为 `***`
后才写日志/SSE/产物（与 perf 的 `redactSecret` 同一套）。

---

## 6. 产物

### 6.1 落盘与归档

```
data:runs/<run_id>/
├── metrics.json                     # { "eval": {...} }（供 /results 回读）
├── logs/<run_id>-engine.log         # 引擎原始输出（脱敏）
├── eval/<dataset>/                  # run_eval.py 的 --output_dir（原始）
│   ├── eval_summary.json
│   ├── eval_report_v2.html
│   ├── per_sample_details.csv
│   ├── all_eval_summary.json        # 单数据集时也存在（run_eval.py 行为）
│   ├── eval_results.tar.gz
│   ├── configs/ logs/ usage_ledger.jsonl
│   └── …
└── artifacts/                       # 对外下载（扁平化，文件名带数据集前缀）
    ├── aime26-eval_report_v2.html
    ├── aime26-eval_summary.json
    ├── aime26-per_sample_details.csv
    ├── gpqa_diamond-eval_report_v2.html
    ├── …
    ├── all_eval_summary.json        # Go 汇总（跨数据集）
    ├── eval_results.tar.gz          # Go 打包（全部数据集产物）
    ├── run.log
    └── metrics.json
```

artifact kinds：`eval_report`（html）、`eval_summary`（json）、
`eval_samples`（csv）、`eval_archive`（tar.gz）、`run_log`、`metrics_json`。

文件名冲突策略：`<dataset>-<原文件名>`；同一 run 内数据集唯一，天然去重。
超过 `limits.artifact_max_bytes` 的单个产物不归档但保留在 `runs/` 目录，并在
`artifacts` 列表外通过日志提示。

### 6.2 汇总文件

Go 生成 `all_eval_summary.json`（覆盖多数据集）：

```json
{
  "run_id": "run_...", "model": "...", "provider": "...",
  "started_at": "...", "finished_at": "...",
  "total_datasets": 2, "success_count": 2, "failure_count": 0,
  "results": [ {"dataset":"aime26","score":1.0,...}, ... ]
}
```

---

## 7. 预检（`POST /v1/targets/{id}/probe`）

eval 能力启用时额外检查（`severity=error` 除非注明）：

| 检查 | 内容 |
|---|---|
| `engine` | `python3 --version`、`evalscope --version` |
| `script` | `thvv:eval/scripts/run_eval.py` 存在 |
| `connectivity` | 对 `target.api_url` 发 1 条最小请求，测首包延迟（与 perf 同实现） |
| `judge` | 有 `needs_judge` 数据集且服务端未配置 judge key 时 `warn` |
| `datasets` | 每个配置数据集的 `kind=video/image/agent` 能力提示；Docker 数据集标 `warn` |

预检不实际跑评测（避免产生费用），只在提交时做参数校验。

---

## 8. 校验与限额

复用 `limits`：

| 项 | 作用 |
|---|---|
| `max_concurrent_runs` / `max_queued_runs` | 与 perf 共享队列 |
| `max_parallel_runs_per_target` | 同一 target 的 perf/eval 串行，避免互相干扰 |
| `max_run_duration` | 整次 eval run 超时（默认 12h）→ `504 engine_timeout` |
| `plan.max_number` | 限制 `limit` 上限 |
| `artifact_max_bytes` | 单产物归档上限 |

额外常量：`repeats ≤ 50`，`eval_batch_size ≤ 256`，`datasets ≤ 17`。

---

### 9.1 分数验收（eval-sla-v1）

`gates.profiles["eval-sla-v1"]`（内置默认，可 YAML 覆盖）：

```yaml
gates:
  profiles:
    eval-sla-v1:
      capability: eval
      description: 效果评测分数下限（对齐《效果验收标准.xlsx》，基线 -4% 浮动）
      eval_tiers:
        - { dataset: aime26,    min_score: 0.8912 }   # 93.12% - 4%
        - { dataset: hle,      min_score: 0.3618 }
        - { dataset: mmlu_pro, min_score: 0.8196 }
        # ...（tau2_bench 可加 subitem 按领域判定）
        - dataset: hle
          min_score: 0.36            # 兜底下限
          baseline_by:               # 按模型覆盖（键对 model 小写包含匹配）
            glm-5.3: 0.40
            kimi: 0.42
```

判定规则（`report.EvaluateEvalGate`）：

- `success=false` 的数据集 → `eval_success` 违规；
- `score < min_score` → `min_score` 违规（`actual`/`threshold`/可读 message）；
- 未配阈值的数据集不判定；
- `baseline_by` 支持精确 → 前缀 → 包含三级模型名匹配；
- 结果落 `run.gate_result`、`/results.gate_result`、`sla_evaluation.json` 产物，UI 结果页/历史列表展示。

内置基线取验收表 `glm-5.3` 列 − 4%（模型间差异见各模型列，按需用 `baseline_by` 覆盖）。

## 9. UI

1. 「测试计划」顶部新增能力切换：`性能压测 / 效果评测`（来自 `GET /v1/tools` 的已启用能力）。
2. eval 面板：数据集多选（来自 `/v1/catalog/datasets`，按 `kind` 分组，`needs_judge` 打标）、
   `limit` / `repeats` / `eval_batch_size`、思考模式、Judge 三项（所选含 Judge 数据集时必填）。
3. 步骤表列名切换：`数据集 / 并发 / 条数 / 状态 / 得分 / 耗时`（perf 仍为档位/并发/请求数/TTFT/TPS）。
4. 结果页：eval 渲染「数据集得分表 + 结论 + 产物下载」；不显示 perf 的验收阈值块。
5. 历史列表：`能力` 列显示 `性能压测 / 效果评测`，结论列对 eval 显示 `通过/部分通过/未通过`。

---

## 10. 兼容与已知限制

1. **Docker 数据集**（`live_code_bench`、`swe_bench_*`）需要宿主安装 Docker +
   `swebench`/`ms-enclave`；未安装时该 Step 失败并把 `run_eval.py` 的报错原样留下。
2. **多模态数据集**（MSR-VTT 等）默认关思考、`max_tokens=8192`，由 `run_eval.py` 决定；
   视频文件缺失会失败，错误同样落在日志与 artifacts。
3. **Judge**：`hle`/`simple_qa` 必须提供 judge key；key 只存在 Run plan 中，
   不落 Target、不写日志（日志里已脱敏），但会随 `metrics.json` 之外的内存结构传递。
4. **分数口径**：直接采信 `eval_summary.json.score`；
5. **分数验收**：`eval-sla-v1` 默认模板按《效果验收标准.xlsx》的 glm-5.3 基线 -4% 设下限，见 §9.1；同一个数据集多 repeats 的聚合由
   Python 侧负责，Go 不再二次计算。
5. **不做 eval SLA 门禁**：v1 的 `gate` 对 eval run 只做 `report_only`
   （全部成功即 `pass`），对照《效果验收标准.xlsx》的判定留 v1.1。
6. **产物体积**：`eval_results.tar.gz` 可能较大（含逐题思维链），下载走流式；
   `data:runs` 目录需预留空间（建议 ≥ 20GB）。

---

## 11. 验收标准与测试计划

| 层级 | 用例 |
|---|---|
| 单测 | plan 规范化（数据集去重/未知/Judge 必填/上限）；argv 构造（base_url 推导、参数透传、脱敏） |
| 单测 | 进度解析：`Running[eval]: 42%`、重试行、完成行；与噪声行（`0/1`）不混淆 |
| 集成 | builtin 引擎：提交 eval → 2 个 Step → SSE → `/results.eval` → artifacts 下载 |
| 真机 | `aime26 --limit 4`：Step 成功、分数与 `bash quickstart.sh eval bench` 一致、v2 HTML 可下载 |
| 真机 | 上游 502/401：Step failed、`run.partial/fail` 与日志可读、无明文 key |
| 真机 | 含 Judge 数据集缺 key：提交即 400，不产生进程 |
| 取消 | 评测中 `POST /cancel`：当前 Step `canceled`、其余 `skipped`、`run.log` 完整 |

---

## 12. 后续（v1.1）

1. ~~eval SLA~~（✅ 已实现，见 §9.1：`eval-sla-v1` + `baseline_by`）
2. 子集/场景选择：`subset_list`、`mvbench_subset` 等透传到 `tool_params`。
3. 评测结果横向对比：同 target 多次 run 的分数趋势（`/v1/runs?target=...&compare=baseline`）。
4. 失败样本重跑：只跑上次 `per_sample_details.csv` 中 `score=0` 的题（需要 run_eval 支持）。
5. 与 perf 组合 run：一次提交先压测后评测，共享 target 与报告包。
