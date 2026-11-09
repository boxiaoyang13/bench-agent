# THVV Server

THVV（TokenHub Vendor Verifier）的服务端：把现有的 Python 压测工具链（evalscope）封装成
**HTTP 服务 + Web 页面**，外部客户端填表即可发起性能压测，实时看进度并下载报告。

- 协议规范：[`docs/API.md`](docs/API.md) · 契约 [`docs/openapi.yaml`](docs/openapi.yaml)
- 设计文档：[`docs/DESIGN.md`](docs/DESIGN.md) · 配置规范 [`docs/CONFIG.md`](docs/CONFIG.md)
- 前端与交互：[`docs/WEB-UI.md`](docs/WEB-UI.md)

## 30 秒跑起来（不需要 Python）

内置 `builtin` 模拟引擎，用于联调 UI ↔ 服务端：

```bash
cd server
export THVV_SECRET_KEY=$(openssl rand -base64 32)
make dev                     # 或: go run ./cmd/thvv-server -config ./config/thvv.local.yaml
open http://127.0.0.1:18080/ui/
```

登录：`admin` / `password`（示例 hash，仅供本地）；
也可用 Bearer：`curl -H 'Authorization: Bearer dev-token' http://127.0.0.1:18080/v1/meta`

> 本机 8080 若被占用，`config/thvv.local.yaml` 默认用 **18080**。

## 工具安装（三种能力各自的 Python 环境）

服务端编排三类测试，各自依赖独立的 Python 工具链（互不影响，安装位置任意，
在配置里用根前缀寻址即可）：

| 能力 | 工具 | 安装位置（本机参考） |
|---|---|---|
| `perf` 性能压测 | evalscope[perf] + transformers（随 thvv 仓库） | `<repo>/.venv` |
| `eval` 效果评测 | evalscope==1.9.0 + modelscope 等（随 thvv 仓库） | `<repo>/.venv` |
| `cache` 缓存测试 | **lmcache** + openai（独立 venv） | `/infer-data/bench/bench-cache/.venv` |

### 安装 lmcache（缓存测试）

```bash
mkdir -p /infer-data/bench/bench-cache && cd /infer-data/bench/bench-cache
python3 -m venv .venv
source .venv/bin/activate
pip install lmcache openai      # openai 是 bench engine 的可选依赖（AsyncOpenAI 客户端），必装
```

- 版本要求：LMCache **v0.5.7+**（`lmcache --help` 出现 `bench` 子命令即可）
- CLI 入口：`/infer-data/bench/bench-cache/.venv/bin/lmcache`
- 冒烟自检：

```bash
lmcache bench engine --help                                # 子命令可用
lmcache bench engine --workload multi-round-chat \
  --engine-url https://your-endpoint/v1 --model your-model \
  --tokens-per-gb-kvcache 65536 --export-config /tmp/c.json  # 参数组合合法
```

### 在服务端配置中引用

`server/config/thvv.dev.yaml`（路径换成你的实际安装位置）：

```yaml
roots:
  bench-cache: "/infer-data/bench/bench-cache"   # lmcache 安装根

engines:
  lmcache:
    enabled: true
    kind: subprocess
    runtime:
      python: "bench-cache:.venv/bin/python"
      evalscope: "bench-cache:.venv/bin/lmcache"   # 复用字段：此处表示 lmcache CLI 入口
    capabilities:
      cache:
        enabled: true
        status: ga
        working_dir: "bench-cache:"
        script: "bench-cache:.venv/bin/lmcache"
```

要点：

- 凭据只经 **`OPENAI_API_KEY` 环境变量**注入子进程（lmcache 不认 argv key），服务端自动完成
- `--engine-url` 会传剥掉 `/chat/completions` 后的 base（lmcache 会给非 `/v1` 结尾的 URL 追加 `/v1`）
- 缓存测试的设计与产物见 [`docs/CACHE.md`](docs/CACHE.md)

## 接真实引擎（fork evalscope）

```bash
export THVV_HOME=/opt/thvv                    # ★ THVV Python 工具链装在哪（与 server 可不同目录）
export THVV_SECRET_KEY=$(openssl rand -base64 32)
export THVV_CI_TOKEN=xxx THVV_ADMIN_HASH="$(htpasswd -bnBC 10 "" '你的密码' | tr -d ':\n')"
make check CONFIG=./config/thvv.example.yaml  # 先校验：根目录/指纹/python/evalscope/数据集/阈值
make run   CONFIG=./config/thvv.example.yaml
```

`config/thvv.example.yaml` 里所有路径都用根前缀（`thvv:perf`、`data:runs`、`bin:python`），
**THVV 换位置只改 `roots.thvv` 一处**，或直接用 `-thvv-home /path`。

本机联调可用 `config/thvv.dev.yaml`（venv 不在 `thvv/` 下的布局，用自定义根 `venv:`）：

```bash
cd server
export THVV_SECRET_KEY=$(openssl rand -base64 32)
make check CONFIG=./config/thvv.dev.yaml
make run   CONFIG=./config/thvv.dev.yaml     # http://127.0.0.1:18080/ui/
```

## 目录结构

```
.                                 仓库根
├── README.md                     本文档
├── docs/                         协议与设计文档
│   ├── API.md / openapi.yaml     HTTP 协议规范 + 接口契约
│   ├── EVAL.md                   效果评测接入设计（Step=数据集 / 产物 / 限制）
│   ├── CACHE.md                  缓存测试接入设计（lmcache bench multi-round-chat）
│   ├── DESIGN.md / CONFIG.md     设计文档（§9.1 双跑对账）/ 配置规范
│   ├── WEB-UI.md                 前端与交互（含实现记录）
│   └── config/                   Python 侧配置样例（thvv.yaml、path_resolver_reference.py）
└── server/                       Go 服务端
    ├── cmd/thvv-server/main.go   入口：flag / -check / -print-roots / 优雅关闭
    ├── internal/
    │   ├── config/               YAML 加载、根与路径解析（前缀寻址）、默认值、校验
    │   ├── domain/               领域模型 + computeSteps（与前端逐行等价）
    │   ├── store/                SQLite 持久化（targets/runs/steps/artifacts/events/audit）
    │   ├── engine/               引擎抽象 + fake(builtin) + subprocess(evalscope)、进程组取消
    │   ├── runmgr/               Run 状态机、队列、SSE 事件广播、产物生成
    │   ├── report/               benchmark_data.db → 指标 / SLA 判定 / 失败明细 / HTML / xlsx / CSV
    │   ├── httpapi/              路由、鉴权（Cookie+Bearer）、problem+json、SSE、静态页面
    │   └── secret/               Target 凭据的 AES-GCM 加解密
    ├── web/                      前端：原生 JS SPA，无构建；go:embed 打包进二进制
    │   ├── index.html / app.js / styles.css
    │   └── embed.go              //go:embed → web.Assets
    ├── config/
    │   ├── thvv.local.yaml       本地联调（builtin 模拟引擎）
    │   ├── thvv.dev.yaml         本地联调（subprocess 真实引擎，venv 在仓库根）
    │   └── thvv.example.yaml     生产示例（subprocess 引擎）
    ├── go.mod / go.sum           模块 github.com/apexcloud/thvv/server
    ├── Makefile                  dev / run / check / test / test-race
    ├── bin/                      构建产物（gitignore）
    └── data/                     运行时数据：SQLite 库 + runs 产物（gitignore）
```

## 已实现 / 未实现

| 能力 | 状态 |
|---|---|
| `GET /v1/health` `/meta` `/tools` `/catalog/{buckets,datasets,gates}` | ✅ |
| Target CRUD + `probe`（tokenizer 自适应推断、api_key 只写、AES-GCM 落库） | ✅ |
| `POST /v1/runs` 单档 / 全档矩阵（并发裁剪、TPM 预算、早停、冷却） | ✅ |
| 状态机 + 队列 + 同 target 串行 + 取消（SIGTERM 进程组） | ✅ |
| 记录删除：`DELETE /v1/runs/{id}`（连产物目录一起删）+ `POST /v1/runs/purge`（批量，支持 dry_run） | ✅ |
| 一键重测：`POST /v1/runs/{id}/rerun`（复用 target + plan + judge 凭据，新 run 标 `rerun_of`） | ✅ |
| `GET /v1/runs/{id}` / `results` / `results/failures` / `artifacts` / `artifacts/{id}` | ✅ |
| SSE 进度流（`Last-Event-ID` 断点续传 + 心跳） | ✅ |
| 报告：HTML / 45 列 xlsx / 失败 CSV / SLA json | ✅ |
| `capability=eval` 效果评测（17 数据集，Step=数据集，产物 eval_report_v2.html/CSV/JSON/tar.gz） | ✅ |
| eval 分数验收 `eval-sla-v1`（对齐《效果验收标准.xlsx》，支持 `baseline_by` 按模型覆盖） | ✅ |
| 缓存测试（lmcache bench multi-round-chat，`cache-sla-v1` 稳态 TTFT + 改善率验收，见 `docs/CACHE.md`） | ✅ |
| Web 页面（登录、三步向导、运行监控、结果、历史（勾选后批量删除 / 单条删除 / 重测）、能力清单） | ✅ |
| 引擎：`builtin` 模拟 / `subprocess` evalscope | ✅（subprocess 已端到端联调，见下） |
| `capability=functional` | ⏳ 预留（返回 `501 not_implemented` + `available_at`） |
| `kind: http` 外部工具代理 | ⏳ v1.1 |
| `POST /v1/runs/{id}/reports` 重新生成 | ⚠️ 占位返回 202（v1 复用跑完时的产物） |

> **subprocess 引擎说明**：已按 [`docs/DESIGN.md`](docs/DESIGN.md) §9.1 做过「双跑对账」：
> 同一 `benchmark_data.db` 下，Go 侧 `/results.metrics` 与
> `perf/scripts/gen_report_from_db.py` 生成的 xlsx 数值列相对误差 < 1e-6。
> 已知限制：evalscope 1.9.x 的 `result` 表不写 `error`/`status_code`，失败明细只能
> 给出兜底说明（与 Python 报告同源）；需要精确失败原因请升级/回退到落这两列的 evalscope 版本。

## 测试

```bash
make test        # 单元 + 集成（httptest 全链路：登录→建端点→预检→提交→SSE→结果→下载）
make test-race
```

覆盖的重点：

- `internal/config`：根与前缀寻址、`../` 逃逸拒绝、未知根报错、`${ENV}` 展开、
  `THVV_*` 覆盖、THVV 根**指纹校验**、默认值 = 现 `run.sh` 行为
- `internal/domain`：`computeSteps` 与前端逐项一致（含并发上限裁剪、TPM 预算、档位覆盖）
- `internal/report`：分位数插值、**TPOT ≠ ITL** 的口径、吞吐、失败分类、SLA 判定、报告渲染
- `internal/httpapi`：全流程 + 幂等 + RBAC + 参数校验 + 501 预留位

## 运维要点

```bash
./bin/thvv-server -config ./config/thvv.yaml -check         # 校验（含 20+ 项语义检查）
./bin/thvv-server -config ./config/thvv.yaml -print-roots   # 只看路径解析结果
```

- `server.write_timeout` **必须是 0**，否则 SSE 会被掐断（启动会 warn）
- `roots.data` 不要放 NFS/CIFS（SQLite WAL 会损坏）
- 凭据：`THVV_SECRET_KEY` 只通过环境变量提供；启动日志只打印指纹
