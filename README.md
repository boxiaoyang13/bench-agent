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

## 目录结构

```
.                                 仓库根
├── README.md                     本文档
├── docs/                         协议与设计文档
│   ├── API.md / openapi.yaml     HTTP 协议规范 + 接口契约
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
| `GET /v1/runs/{id}` / `results` / `results/failures` / `artifacts` / `artifacts/{id}` | ✅ |
| SSE 进度流（`Last-Event-ID` 断点续传 + 心跳） | ✅ |
| 报告：HTML / 45 列 xlsx / 失败 CSV / SLA json | ✅ |
| Web 页面（登录、三步向导、运行监控、结果、历史、能力清单） | ✅ |
| 引擎：`builtin` 模拟 / `subprocess` evalscope | ✅（subprocess 未做端到端联调，见下） |
| `capability=eval` / `functional` | ⏳ 预留（返回 `501 not_implemented` + `available_at`） |
| `kind: http` 外部工具代理 | ⏳ v1.1 |
| `POST /v1/runs/{id}/reports` 重新生成 | ⚠️ 占位返回 202（v1 复用跑完时的产物） |

> **subprocess 引擎说明**：代码完整实现了 `evalscope perf` 的 argv 构造、进程组取消、
> stdout 解析与产物回读，但**未在真实 Python 环境做端到端调试**（按当前阶段要求）。
> 上线前需按 [`docs/DESIGN.md`](docs/DESIGN.md) §9.1 做一次「双跑对账」：
> 同一 target/数据集下，Go 侧指标与 `bash run.sh bench` 的 xlsx **逐格一致**。

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
