# THVV Server 配置规范（YAML）

> 一个 YAML 文件描述全部运行期行为：监听、认证、存储、引擎、档位表、验收阈值、报告格式、限额。
> 目标：**改配置不改代码**；配错在启动时立刻报错，而不是跑到一半才发现。
>
> 示例文件：[`config/thvv.yaml`](./config/thvv.yaml)（全量带注释） · [`config/thvv.min.yaml`](./config/thvv.min.yaml)（最小可跑）

## 0. 内置默认值 = 现 `run.sh` 的行为

**YAML 只需要写「你要改的」**。以下项在代码里有默认值，且与现有 Python 工具链逐项一致；
不写就等于「维持现状」：

| 配置路径 | 内置默认 | 来源 |
|---|---|---|
| `engines.thvv.capabilities.perf.buckets` | 9 档：1k/9k/16k/32k/64k/128k/200k(+512k/1m 待构建) 及各自的 token 区间与并发上限 | `run.sh: bucket_range` `bucket_max_parallel` |
| `number_by_parallel` | `{1:20, 4:40, 8:60, 16:80, 32:120, 64:200, 128:400}` | `run.sh: parallel_number` |
| `extra_parallels` | `{"200k": [16]}` | `run.sh: bucket_extra_parallels` |
| `temperature_map` | kimi/moonshot → 1.0，其余 → 0.0 | `run.sh: TEMPERATURE` 分支 |
| `tokenizer_map` | 13 条模型族 → tokenizer 映射 | `run.sh: auto_detect_tokenizer` |
| `matrix.*` | 梯度 `[1,8,32,64,128]`、冷却 `120s`、早停关闭、TPM 不限、失败即跳 | `run.sh: BUCKETS` / `CONCURRENCY_LADDER` / … |
| `gates.profiles["perf-sla-v1"]` | 6 档 TTFT P50/P90 阈值 + 30 t/s + 99% 成功率 | `sla_eval.py: TIERS` |
| `gates.profiles["cache-sla-v1"]` / `["cache-perf-sla-v1"]` | 缓存收益（稳态 TTFT + 改善率 + 命中率）；后者再加 TPOT / 解码速度 / 吞吐 / 最小样本数 | 新增（`docs/CACHE.md` §7） |
| `reports.title.*` | `{model} 性能压测报告 ({bucket})` / `(全档位 {ts})` | `gen_perf_dashboard.py` |
| `limits.*` | 并发 2 / 队列 16 / 单 run 12h / 保留 30 天 | 新增（无历史对应） |

所以一个能跑性能压测的配置只需写：`roots.thvv` + `listen` + `auth` + `store` + `secrets` +
`paths.datasets_dir` + `engines.thvv.runtime`（python/evalscope 走 `bin:` 前缀即无需写全路径）。其余全部省略。

---

## 1. 加载与优先级

```bash
./thvv-server -config ./config/thvv.yaml
./thvv-server -config ./config/thvv.yaml -check                  # 只校验不启动
./thvv-server -config ./config/thvv.yaml -thvv-home /opt/thvv    # 覆盖 roots.thvv
```

加载顺序（**后者覆盖前者**）：

```
① 内置默认值（代码里的结构体初始化）
② YAML 文件
③ 环境变量 THVV_*
④ 命令行参数（-listen / -log-level 等少数常用项）
```

- 只支持**单个** YAML 文件（不做 `config.d/*.yaml` 合并）。需要多环境差异请用环境变量覆盖，或用脚本生成。
- **不支持热加载**。改配置必须重启（`SIGHUP` 仅打印与磁盘的差异，不生效）。理由：`engines.*.capabilities` 变更涉及进程与缓存一致性，热更收益低风险高。
- `version: 1` 必填，用于将来做迁移。

### 环境变量覆盖规则

路径转大写，层级用 `__`（双下划线）：

```bash
THVV_SERVER__LISTEN=0.0.0.0:8080
THVV_LIMITS__MAX_CONCURRENT_RUNS=8
THVV_ENGINES__THVV__CAPABILITIES__PERF__MATRIX__COOLDOWN=300s
THVV_ENGINES__THVV__RUNTIME__PYTHON=/usr/bin/python3
```

数组用 `__<索引>` 或整段 JSON：

```bash
THVV_AUTH__TOKENS__0__ROLE=viewer
THVV_ENGINES__THVV__CAPABILITIES__PERF__BUCKETS='[{"label":"1k","min":870,"max":1228,"max_parallel":128}]'
```

环境变量值按目标字段类型解析（duration / size / int / bool / 字符串）。解析失败 → 启动失败并指出变量名。

---

## 2. 变量展开

字符串值支持两种占位符，**在解析 YAML 后、类型转换前**执行：

| 语法 | 行为 |
|---|---|
| `${VAR}` | 必填。未设置 → **启动失败**，错误信息指出字段路径（如 `auth.tokens[0].token`） |
| `${VAR:-默认值}` | 未设置或为空时取默认值 |

```yaml
secrets:
  env:
    key: ${THVV_SECRET_KEY}                 # 必填
auth:
  session:
    signing_key: ${THVV_SESSION_KEY:-dev}   # 有默认值
```

注意：
- 展开**只做一轮**，不递归（避免 `${A}` 的值里再含 `${B}` 造成意外）。
- 默认值里可以含 `$`（如 bcrypt hash `$2a$10$...`），因为只匹配到第一个 `}`。
- 需要字面量 `${` 时写 `$${`。
- 密钥类字段**只允许**通过 `${VAR}` 提供（见 §6），禁止直接写在 YAML 里。

---

## 3. 根目录与路径寻址（★ Go 服务与 THVV 可部署在不同位置）

Go 服务与 THVV Python 工具链**不需要同目录**，也不需要同一个用户。THVV 装在哪，只在 `roots.thvv` 写一次。

### 3.1 `roots` — 先填地址

| 根 | 必填 | 默认 | 含义 |
|---|:---:|---|---|
| `thvv` | ✅ | — | **THVV Python 工具链根目录**（含 `perf/`、`eval/`、`configs/`、`quickstart.sh`） |
| `data` | ❌ | `./data` | 服务端自有数据（sqlite / runs / logs / cache），与 thvv 完全解耦，可放另一块盘 |
| `bin` | ❌ | `thvv:.venv/bin` | Python 解释器与 CLI 所在目录 |
| *自定义* | ❌ | — | 任意命名根，可引用其他根（按声明顺序解析，禁止循环） |

```yaml
roots:
  thvv: "${THVV_HOME}"            # /opt/thvv、/srv/vendor/thvv、D:\\tools\\thvv 都行
  data: "${THVV_DATA:-./data}"
  bin:  "thvv:.venv/bin"          # 想用系统 python → "/usr/local/bin"
  thvv-eval: "${THVV_EVAL_HOME}"  # 自定义根：给 eval 单独一套 venv（见 §8.3）
```

三种等效指定方式（优先级从低到高）：

```bash
# ① 配置文件
roots: { thvv: /opt/thvv }
# ② 环境变量
THVV_HOME=/opt/thvv      # 或通用形式 THVV_ROOTS__THVV=/opt/thvv
# ③ 命令行（最方便，运维脚本用）
./thvv-server -config ./config/thvv.yaml -thvv-home /opt/thvv
```

> `-thvv-home` 存在的原因：**同一份配置可以在不同机器上跑**（开发机 `/Users/x/...`、测试机 `/opt/thvv`），
> 不必为每台机器改 YAML。

### 3.2 前缀寻址

所有路径类字段支持 6 种写法：

| 写法 | 解析为 | 示例 |
|---|---|---|
| `thvv:<rel>` | `<roots.thvv>/<rel>` | `thvv:perf/datasets_clean` |
| `data:<rel>` | `<roots.data>/<rel>` | `data:runs` |
| `bin:<rel>` | `<roots.bin>/<rel>` | `bin:python` |
| `cfg:<rel>` | `<配置文件目录>/<rel>` | `cfg:templates` |
| `<自定义根>:<rel>` | `<roots.<name>>/<rel>` | `thvv-eval:.venv/bin/python` |
| `/abs` 或无前缀相对 | 原样 / 相对配置文件目录 | `/var/lib/x`、`templates` |

规则：

- `cfg` 是**隐式内置根**（= 配置文件所在目录），不需在 `roots` 里声明；另三个保留名 `thvv` / `data` / `bin` 需/可声明。
- 只按**第一个** `:` 切分；单字母前缀（`C:`、`D:`）视为 Windows 盘符 → 当作绝对路径处理。
- `<rel>` 必须是相对路径；出现 `../` 逃逸出根 → **启动失败**（`paths.runs_dir: escapes root "data"`）。
- `.` 或空 = 该根本身：`home: "thvv:"`。
- 支持 `~` 展开。
- 根必须先解析、再解析前缀；`thvv-eval` 这类值可写 `thvv:xxx`（根引用根）。

**路径类字段 vs `runtime.env` 值（两种严格度，别搞混）**

| 场景 | 规则 |
|---|---|
| 路径字段（`paths.*` / `runtime.python` / `working_dir` …） | 严格：未知前缀 → 报错；必定返回一个绝对路径 |
| `runtime.env` 的值 | 宽松：**只有**命中已知根前缀、或以 `~` / `/` 开头时才解析，其余视为**字面量** |

所以下面四个都能共存，不会因 `1` / `false` 被当成相对路径而报错：

```yaml
env:
  HF_HOME: "data:cache/huggingface"     # → /var/lib/thvv/cache/huggingface
  TMPDIR: "/tmp"                        # 绝对路径
  PYTHONUNBUFFERED: "1"                 # 字面量
  TOKENIZERS_PARALLELISM: "false"       # 字面量
```

**参考实现**：[`config/path_resolver_reference.py`](./config/path_resolver_reference.py)
（可直接跑，作为 Go 侧 `Resolver` 的 golden 对照）：

```bash
export THVV_HOME=/opt/thvv
python3 config/path_resolver_reference.py config/thvv.yaml
```

输出会逐行打印每个路径字段的原始写法 → 绝对路径 + 状态（`exists` / `will create` / `MISSING` / `ERROR`）。

改前 vs 改后（这是本次的核心改动）：

```yaml
# ❌ 之前：Go 服务与 thvv 的相对位置硬编码，thvv 换位置就要改一堆
paths:
  datasets_dir: "../../thvv/perf/datasets_clean"
  cache_dir: "../data/cache"
engines:
  thvv:
    runtime:
      python: "../../.venv/bin/python"
      pythonpath_prepend: ["../../thvv/perf/scripts"]
    capabilities:
      perf: { working_dir: "../../thvv/perf" }

# ✅ 现在：只改 roots.thvv，其余保持不变
roots:
  thvv: "/opt/thvv"
  data: "/var/lib/thvv"
paths:
  datasets_dir: "thvv:perf/datasets_clean"
  cache_dir: "data:cache"
engines:
  thvv:
    runtime:
      python: "bin:python"
      pythonpath_prepend: ["thvv:perf/scripts"]
    capabilities:
      perf: { working_dir: "thvv:perf" }
```

开发机换成 `/Users/me/code/thvv` 只需 `THVV_HOME=/Users/me/code/thvv`，YAML 一行不动。

### 3.3 启动时的指纹校验（防止把 thvv 指错目录）

路径存在不代表「这是 THVV 根」。启动时依次校验，任一必需项失败 → 退出：

| 校验 | 期望 | 失败级别 |
|---|---|---|
| `thvv:perf` 是目录 | — | **error** |
| `thvv:perf/run.sh` 或 `thvv:quickstart.sh` 存在 | 用两个特征文件确认是 THVV 根 | **error** |
| `thvv:perf/scripts/setup_tokenizer.py` 存在 | tokenizer 初始化脚本 | error（perf 启用时） |
| `thvv:eval/scripts/run_eval.py` 存在 | 评测入口 | error（eval 启用时） |
| `paths.datasets_dir` 存在 | 否则回落到 `thvv:perf/datasets` | warn → 两者均无则 error |
| `bin:python` 可执行且 `sys.version >= 3.10` | 一次 `python -c` | **error** |
| `bin:evalscope` 可执行 | `evalscope --version` | error（`check_on_startup: true`） |
| `bin:python -c "import transformers, modelscope"` | tokenizer 依赖 | warn（仅 perf） |
| `data` 目录可读写 | 写入探针文件后删除 | **error** |
| `store.sqlite.path` 目录可写 | — | **error** |
| 所有派生路径不含 `..` 逃逸 | — | **error** |

> 这一组是**对"地址"的校验**，与 §7 的语义校验分开：前者回答「装在哪」，后者回答「配得对不对」。
> `-check` 会把两者都跑完并打印结果表。

典型错误信息：

```
roots.thvv: /opt/thvv 不存在
roots.thvv: /opt/thvv 看起来不是 THVV 根目录
            （未找到 perf/run.sh 或 quickstart.sh）
bin:python: /opt/thvv/.venv/bin/python 不可执行
            → 若使用系统 python，请设置 roots.bin: "/usr/local/bin"
paths.datasets_dir: thvv:perf/datasets_clean 与回落目录均不存在
            → 请先 cd $THVV_HOME/perf && python scripts/sanitize_dataset.py
```

### 3.4 解析结果打印

启动日志（与 `-check`）都会输出一张解析表，运维排障基本不用猜：

```
[root] thvv        = /opt/thvv                    (exists, fingerprint ok)
[root] data        = /var/lib/thvv                (exists, writable)
[root] bin         = /opt/thvv/.venv/bin          (exists)
[path] runs_dir    = /var/lib/thvv/runs           (exists)
[path] datasets    = /opt/thvv/perf/datasets_clean(exists, 10 files)
[path] cache_dir   = /var/lib/thvv/cache          (created)
[engine] thvv/perf python=/opt/thvv/.venv/bin/python  3.11.9
[engine] thvv/perf evalscope=0.13.2              ok
[engine] thvv/eval python=/opt/thvv-eval/.venv/bin/python  3.11.9
```

### 3.5 跨机器部署（预留，v1 不实现）

若 THVV 必须跑在**另一台机器**（例如压测需要独立网卡 / GPU 机器），v1 不支持。
协议上的预留位是 `engines.<name>.kind: ssh`：

```yaml
engines:
  thvv-remote:
    kind: ssh                     # v2
    runtime:
      host: "bench-node-01"
      user: "runner"
      home: "/opt/thvv"           # 远端根（同样用前缀寻址）
      identity_file: "cfg:keys/bench_ed25519"
```

即便跨机，**产物的落点仍是本地 `data:runs`**（rsync/SFTP 拉回），因为报告聚合与下载都在 Go 服务侧。
所以 `benchmark_data.db` 的数据契约不变，`/results` 与 `/artifacts` 也不变。

---

### 3.6 `capabilities.eval`

| 字段 | 说明 |
|---|---|
| `enabled` / `status` | 是否对外提供 `capability=eval`（`ga` / `preview`） |
| `working_dir` | `run_eval.py` 的工作目录（一般 `thvv:eval`） |
| `script` | 评测入口，`thvv:eval/scripts/run_eval.py` |
| `datasets[]` | 数据集表；不写则用内置 17 个。字段：`name` / `display_name` / `kind`(`text|agent|video|image`) / `needs_judge` / `needs_docker` / `default_limit` / `default_repeats` / `default_eval_batch_size` / `excluded_from_all` |
| `judge` | 默认 Judge：`model`（默认 `deepseek-v4-pro`）、`base_url`、`api_key`（建议 `${THVV_JUDGE_API_KEY:-}`） |
| `runtime` | 可选，覆盖引擎级 runtime（例如 eval 单独一套 venv） |

执行模型见 [`EVAL.md`](./EVAL.md)：**一个数据集一个子进程**，Step = 数据集。

## 4. 类型约定

| 类型 | 写法 | 示例 |
|---|---|---|
| Duration | `[0-9]+(ms\|s\|m\|h)` | `10s`、`12h`、`720h` |
| Size | `[0-9]+(B\|KiB\|MiB\|GiB)` | `200MiB`、`1GiB` |
| Bool | `true` / `false` | — |
| Enum | 小写字符串 | `lax`、`operator` |
| Regex | 字符串（自动编译，失败即报错） | `"(?i)glm\|chatglm"` |

---

## 5. 字段说明

### 5.1 `server`

| 字段 | 默认 | 说明 |
|---|---|---|
| `listen` | `127.0.0.1:8080` | 监听地址 |
| `base_path` | `/v1` | API 前缀；改这里会同时改变 UI 的请求路径（需同步改前端常量） |
| `read_header_timeout` | `10s` | — |
| `read_timeout` / `write_timeout` | `0s` | **必须为 0**：SSE 长连接会被非零值掐断。启动时若配成非 0 且 `engines.*` 启用 → 警告 |
| `idle_timeout` | `120s` | keep-alive 空闲 |
| `shutdown_timeout` | `30s` | 优雅退出；正在进行中的 run 会被标记 `canceled` |
| `tls.enabled` | `false` | 本地一般用反代终止 TLS |
| `cors.allowed_origins` | — | UI 同源部署时无需配置 |
| `ui.enabled` / `ui.dir` | `true` / 空 | `dir` 为空 = 使用 `go:embed` 的二进制内置版本；非空 = 从磁盘目录读（便于改页面不重编译） |
| `ui.redirect_root` | `true` | `GET /` → 302 到 `/ui/` |

### 5.2 `auth`

| 字段 | 默认 | 说明 |
|---|---|---|
| `mode` | `both` | `bearer`（仅 API）/ `cookie`（仅网页）/ `both`。中间件先查 Cookie 再查 `Authorization` |
| `session.ttl` | `12h` | 会话有效期，与 `POST /auth/login` 的 `Max-Age` 一致 |
| `session.secure` | `false` | HTTPS 下**必须** `true` |
| `session.same_site` | `lax` | — |
| `session.signing_key` | — | HMAC 签名密钥，会话 Cookie 不透明且防篡改 |
| `tokens[]` | — | `{name, token, role}`；`role ∈ viewer\|operator\|admin` |
| `users[]` | — | `{username, password_hash, role}`；hash 为 bcrypt |

对应协议：`GET /v1/meta.auth` 只暴露 `scheme` 与 `roles`，**不暴露 token 数量或用户名**。

### 5.3 `store`

| 字段 | 默认 | 说明 |
|---|---|---|
| `driver` | `sqlite` | v1 仅 `sqlite`；`postgres` 预留（`store` 接口已抽象） |
| `sqlite.path` | `../data/thvv.db` | 文件路径，目录不存在会自动建 |
| `sqlite.wal` | `true` | WAL 模式，允许读不阻塞写 |
| `sqlite.busy_timeout` | `5s` | 写锁等待 |
| `sqlite.max_open_conns` | `4` | 配合 `busy_timeout` 防 `database is locked` |

### 5.4 `secrets`

Target 的 `api_key` 落库前用 **AES-GCM** 加密。这里只说明主密钥来源，YAML 中永远不出现被测端点的明文 Key。

| 字段 | 说明 |
|---|---|
| `provider` | `env`（从环境变量读） / `file`（从文件读，建议 `0600`） |
| `env.key` | 值为 32 字节的 base64（44 字符，`openssl rand -base64 32`）或 64 位 hex |
| `file.path` | 同上格式，读取文件内容 |

校验：解码后长度必须恰好 32 字节，否则启动失败。**启动时不会打印密钥内容**，只打印 `provider` 与指纹（前 4 位 sha256）。

### 5.5 `paths`

全部支持 §3.2 的前缀写法（`thvv:` / `data:` / `bin:` / `cfg:` / 自定义根）。

| 字段 | 默认 | 说明 |
|---|---|---|
| `runs_dir` | `data:runs` | 每个 run 一个子目录，结构与 Python 版一致：`runs/<run_id>/results/perf-<bucket>-<ts>/<vendor>/parallel_<p>_number_<n>/benchmark_data.db` |
| `logs_dir` | `data:logs` | 服务日志 |
| `datasets_dir` | `thvv:perf/datasets_clean` | 脱敏数据集。不存在时**回落** `thvv:perf/datasets` 并 `warn` |
| `cache_dir` | `data:cache` | `HF_HOME` / `MODELSCOPE_CACHE` 落点（tokenizer 只下载一次） |
| `templates_dir` | `cfg:templates` | 报告模板；不存在时用二进制内置模板 |

> 旧版的 `paths.data_dir` 已合并进 `roots.data`（避免同一概念两处定义）。

### 5.6 `limits`

| 字段 | 默认 | 对应协议 |
|---|---|---|
| `max_concurrent_runs` | `2` | `/v1/meta.limits.max_concurrent_runs`；超出返回 `429 quota_exceeded` + `Retry-After` |
| `max_queued_runs` | `16` | 队列满则直接 429 |
| `max_parallel_runs_per_target` | `1` | 同一 target 串行，避免自相干扰污染指标 |
| `max_run_duration` | `12h` | 超时 → `failed` + `engine_timeout` |
| `run_retention` | `720h` | 到期清理 run 目录 + DB 记录（artifacts 一并删） |
| `artifact_max_bytes` | `200MiB` | 单个产物上限，超出截断并 `warn` |
| `api.max_body_bytes` | `1MiB` | 请求体上限（内联 target 场景足够） |
| `api.rate_limit` | 关闭 | 每 token 的 rps/burst |
| `plan.max_number` / `max_parallel` | `10000` / `256` | `plan` 数值上限，超出 → `400 validation_failed` |
| `plan.max_buckets_per_run` | `9` | — |
| `plan.max_tpm_budget` | `1e8` | — |

> 这些值直接决定 `/v1/meta` 的返回内容，前端用它们渲染限制说明，**不要在前端硬编码**。

### 5.7 `engines`（最核心的一节）

`engines.<name>` 是工具注册表。`name` 即 `POST /v1/runs` 的 `tool` 字段。

#### 公共字段

| 字段 | 说明 |
|---|---|
| `enabled` | `false` 时该工具不出现在 `/v1/tools`（或标 `unavailable`） |
| `kind` | `subprocess`（fork 本地进程）/ `http`（外部服务）/ `builtin`（编译进二进制的 Go 实现） |
| `display_name` / `version` | 回显在 `/v1/tools` |

#### `thvv.runtime`（kind=subprocess）

| 字段 | 默认 | 说明 |
|---|---|---|
| `home` | `roots.thvv` | 工具链根。单引擎可覆盖全局 `roots.thvv`（例如要同时测两个 THVV 版本） |
| `python` | `bin:python` | 解释器路径（venv 里的那个，不是系统 python） |
| `evalscope` | `bin:evalscope` | `evalscope` CLI；`check_on_startup=true` 时启动跑一次 `--version` |
| `pythonpath_prepend` | `[thvv:perf/scripts]` | 注入 `sitecustomize.py`，修 transformers 5.x 的 token 计数 bug |
| `env` | — | 追加给子进程的环境变量（`HF_HOME` 等，同样支持前缀） |
| `check_on_startup` | `true` | 启动自检；失败则该引擎所有 capability 标 `unavailable`，服务仍能起（只读查询可用） |

> **capability 级可覆盖 `runtime`**：只需写要改的字段，其余继承引擎默认。这是给 eval 单独一套 venv 的入口（见 §8.3）。

```yaml
engines:
  thvv:
    runtime:                       # 默认：perf 与 eval 共用
      home: "thvv:"
      python: "bin:python"
    capabilities:
      eval:
        runtime:                   # 只覆盖 python / evalscope
          python: "thvv-eval:.venv/bin/python"
          evalscope: "thvv-eval:.venv/bin/evalscope"
```

启动时会对**每个不同的 python 路径**各跑一次自检（同路径只检一次）。

#### `thvv.capabilities.perf.buckets`

**与 `perf/run.sh` 的 `bucket_range` + `bucket_max_parallel` 一一对应**。这是唯一真正需要人工核对的数据，改成不同的值会直接改变压测语义（过滤区间与并发上限）。

| 字段 | 说明 |
|---|---|
| `label` | 档位名，出现在 `plan.buckets` |
| `min` / `max` | prompt token 过滤区间（传给 `--min/max-prompt-length`） |
| `max_parallel` | 该档并发上限（超出被裁剪） |
| `dataset_built_in` | `false` 表示数据集需离线构建（`build_long_bucket.py`）；`/v1/catalog/buckets` 会标 `built_in:false`，前端灰显 |

#### `thvv.capabilities.perf` 其余关键项

| 字段 | 对应旧实现 |
|---|---|
| `number_by_parallel` | `run.sh: parallel_number` |
| `extra_parallels` | `run.sh: bucket_extra_parallels`（200k 补 16） |
| `default_number` | `parallel_number` 的 `*)` 兜底值 |
| `matrix.buckets` / `default_ladder` / `cooldown` / `success_rate_min` / `tpm_budget` / `stop_on_failure` | `BUCKETS` / `CONCURRENCY_LADDER` / `BUCKET_COOLDOWN` / `SUCCESS_RATE_MIN` / `TPM_BUDGET` |
| `long_buckets` + `dataset_tokenizer` | 512k/1m 固定用构建时分词器计数 |
| `temperature_map` | `run.sh` 里 kimi/moonshot → 1.0 的 case 分支 |
| `tokenizer_map` | `auto_detect_tokenizer()` 的整个 case 表 |
| `process.kill_grace` | 取消任务时 SIGTERM 等待时长（之后 SIGKILL 整个进程组） |
| `process.redact_patterns` | 日志落盘前的脱敏正则 |

> `temperature_map` / `tokenizer_map` 用**有序列表 + 正则**，首条命中生效，最后一条写 `.*` 兜底。顺序即优先级，改顺序会改变行为。

#### `kind: http` 的外部工具

```yaml
acme-redteam:
  enabled: true
  kind: http
  base_url: "http://127.0.0.1:9100"
  auth: { scheme: bearer, token: "${ACME_TOKEN}" }
  timeout: 30s
  capabilities:
    functional:
      enabled: true
      status: ga
      plan_schema: "./schemas/acme.functional.plan.json"   # 相对配置文件目录
```

THVV 只负责：把 `plan.tool_params` 按 schema 校验 → 转发 → 翻译事件流 → 收产物。
**新增工具只加配置，不改协议、不改代码**（第三篇 `DESIGN.md` §3 的落点）。

### 5.8 `reports`

| 字段 | 说明 |
|---|---|
| `default_formats` | `html` / `xlsx`；`plan` 未指定时用这个 |
| `title.single` / `title.matrix` | 模板变量：`{model}` `{bucket}` `{ts}` `{vendor}` `{company}` |
| `xlsx.sheet_metrics` / `sheet_failures` | sheet 名（中文名要保证 ≤31 字符，Excel 限制） |
| `xlsx.embed_tpm_chart` | `false` 等价旧脚本的 `--no-tpm-html` |
| `failure_excerpt_chars` | 失败明细里 Body 截断长度 |
| `html.inline_assets` | `true` = 单文件 HTML（CSS/图内联），便于交付 |

### 5.9 `gates`

| 字段 | 说明 |
|---|---|
| `default_profile` | `plan.gate.profile` 未指定时用哪个；留空则默认不判定 |
| `profiles.<name>.tiers[]` | 分档阈值，`lo <= avg_prompt_tokens < hi`（上界不含，与 `sla_eval.py` 一致） |
| `profiles.<name>.cache_tiers[]` | cache 能力专用阈值，见下表 |
| `global.min_output_tokens_per_sec` | 吞吐下限（原脚本硬编码 30） |
| `global.min_success_rate` | 成功率下限 |
| `default_params` | 如 `user_tpm_quota` |

`cache_tiers[]` 字段（除前四项外均为可选，0 = 不判该项）：

| 字段 | 说明 |
|---|---|
| `steady_ttft_p50_max_ms` | 稳态 TTFT P50 上限 |
| `ttft_improvement_min` | 稳态相对首轮的 TTFT 改善率下限（0~1） |
| `success_rate_min` | 成功率下限（0~1） |
| `prefix_hit_rate_min` | 前缀命中率下限（cached/prompt，0~1） |
| `tpot_p50_max_ms` | 稳态 TPOT P50 上限（ms/token） |
| `decode_speed_min` | 稳态解码速度 P50 下限（tokens/s） |
| `input_throughput_min` | 全 run 输入吞吐下限（tokens/s，含 cached token，慎用） |
| `min_samples` | 稳态样本数下限（低于阈值直接违规） |

> 注意：cache 的性能字段是「前缀缓存命中态」的表观性能，与 `perf-sla-v1` 的
> 冷启动/定压口径不可直接横比。阈值默认值只是起点，应按被测模型在 YAML 覆盖。

阈值模板通过 `GET /v1/catalog/gates` 暴露给前端，「验收门槛」下拉框直接读它 —— 加新模板无需改前端。

### 5.10 `webhooks` / `security` / `observability`

| 字段 | 说明 |
|---|---|
| `webhooks.retry_backoff` | 回调失败的重试间隔列表（5 次） |
| `webhooks.allowed_hosts` | 空 = 不限制；非空 = SSRF 白名单 |
| `security.allow_private_targets` | 允许被测端点解析到内网/回环。本地联调必须 `true`；对外部署设 `false` |
| `security.target_host_denylist` | 无论 allowlist 如何都拒绝的 host（云元数据地址） |
| `security.expose_engine_cmdline` | `false` 时 `/v1/runs` 的 `engine` 字段不返回完整 argv（避免泄露 Key） |
| `observability.log_format` | `text`（本地可读）/ `json`（采集） |
| `observability.metrics.path` | Prometheus 端点，**在 `/v1` 之外且不鉴权**（建议只监听回环） |

---

## 6. Go 结构体（可直接落地）

```go
package config

type Config struct {
	Version int         `yaml:"version"`
	Server  Server      `yaml:"server"`
	Auth    Auth        `yaml:"auth"`
	Store   Store       `yaml:"store"`
	Secrets Secrets     `yaml:"secrets"`
	Paths   Paths       `yaml:"paths"`
	Limits  Limits      `yaml:"limits"`
	Engines map[string]Engine `yaml:"engines"`
	Reports Reports     `yaml:"reports"`
	Gates   Gates       `yaml:"gates"`
	Webhooks Webhooks   `yaml:"webhooks"`
	Security Security   `yaml:"security"`
	Observability Observability `yaml:"observability"`

	// Roots 在解析阶段单独处理：先解根、再解引用主的前缀语法（§3）。
	// 自定义根允许，所以用 map 而不是固定字段。
	Roots Roots `yaml:"roots"`
}

type Roots struct {
	Thvv string            `yaml:"thvv"`   // 必填
	Data string            `yaml:"data"`   // 默认 ./data
	Bin  string            `yaml:"bin"`    // 默认 thvv:.venv/bin
	Ext  map[string]string `yaml:",inline"` // thvv-eval 等自定义根
}

// Path 是前缀寻址的路径。落地为 string 即可，但建议包一层，
// 强制所有路径类字段只能经 Resolver 解析，防止有人直接拿相对路径去开文件。
type Path string

// Resolver 两阶段：① 解根（Roots）② 解前缀（Thvv/Data/Bin/Cfg/自定义）。
type Resolver struct {
	ConfigDir string   // -config 所在目录（绝对）
	Roots     Roots
}

func (r *Resolver) Resolve(p Path) (abs string, err error)     // 返回绝对路径
func (r *Resolver) MustResolveStrip(p Path) (abs string, err error) // 额外拒绝 ../ 逃逸
func (r *Resolver) ExpandAbs() map[string]string               // 供 -check / 启动日志打印

type Server struct {
	Listen             string        `yaml:"listen"`
	BasePath           string        `yaml:"base_path"`
	ReadHeaderTimeout  time.Duration `yaml:"read_header_timeout"`
	ReadTimeout        time.Duration `yaml:"read_timeout"`
	WriteTimeout       time.Duration `yaml:"write_timeout"`
	IdleTimeout        time.Duration `yaml:"idle_timeout"`
	ShutdownTimeout    time.Duration `yaml:"shutdown_timeout"`
	TLS                TLS           `yaml:"tls"`
	CORS               CORS          `yaml:"cors"`
	UI                 UI            `yaml:"ui"`
}

type Auth struct {
	Mode    string    `yaml:"mode"`      // bearer | cookie | both
	Session Session   `yaml:"session"`
	Tokens  []APIToken `yaml:"tokens"`
	Users   []User    `yaml:"users"`
}

type APIToken struct {
	Name  string `yaml:"name"`
	Token string `yaml:"token"`
	Role  string `yaml:"role"`
}

type Limits struct {
	MaxConcurrentRuns      int           `yaml:"max_concurrent_runs"`
	MaxQueuedRuns          int           `yaml:"max_queued_runs"`
	MaxParallelPerTarget   int           `yaml:"max_parallel_runs_per_target"`
	MaxRunDuration         time.Duration `yaml:"max_run_duration"`
	RunRetention           time.Duration `yaml:"run_retention"`
	ArtifactMaxBytes       int64         `yaml:"artifact_max_bytes"`
	API                    APILimits     `yaml:"api"`
	Plan                   PlanLimits    `yaml:"plan"`
}

type Engine struct {
	Enabled     bool                  `yaml:"enabled"`
	Kind        string                `yaml:"kind"`  // subprocess | http | builtin
	DisplayName string                `yaml:"display_name"`
	Version     string                `yaml:"version"`
	Runtime     Runtime               `yaml:"runtime"`
	BaseURL     string                `yaml:"base_url"` // kind=http
	Auth        EngineAuth            `yaml:"auth"`
	Timeout     time.Duration         `yaml:"timeout"`
	Capabilities map[string]Capability `yaml:"capabilities"`
}

type Capability struct {
	Enabled     bool       `yaml:"enabled"`
	Status      string     `yaml:"status"`      // ga | preview | planned | unavailable
	AvailableAt string     `yaml:"available_at"`
	WorkingDir  string     `yaml:"working_dir"`
	Script      string     `yaml:"script"`
	Defaults    PerfDefaults `yaml:"defaults"`
	Buckets     []Bucket   `yaml:"buckets"`
	NumberByParallel map[int]int   `yaml:"number_by_parallel"`
	ExtraParallels   map[string][]int `yaml:"extra_parallels"`
	DefaultNumber    int   `yaml:"default_number"`
	Matrix      MatrixDefaults `yaml:"matrix"`
	LongBuckets []string   `yaml:"long_buckets"`
	DatasetTokenizer string `yaml:"dataset_tokenizer"`
	Process     Process    `yaml:"process"`
	PlanSchema  string     `yaml:"plan_schema"`   // kind=http
}

type Bucket struct {
	Label            string `yaml:"label"`
	Min              int    `yaml:"min"`
	Max              int    `yaml:"max"`
	MaxParallel      int    `yaml:"max_parallel"`
	DatasetBuiltIn   *bool  `yaml:"dataset_built_in"` // 默认 true
}

type MatrixDefaults struct {
	Buckets         []string      `yaml:"buckets"`
	DefaultLadder   []int         `yaml:"default_ladder"`
	Cooldown        time.Duration `yaml:"cooldown"`
	SuccessRateMin  float64       `yaml:"success_rate_min"`
	TPMBudget       int           `yaml:"tpm_budget"`
	StopOnFailure   bool          `yaml:"stop_on_failure"`
	MaxParallelCap  any           `yaml:"max_parallel_cap"` // "default" | int
}

type MatchRule struct {
	Match     string  `yaml:"match"`     // 正则
	Value     float64 `yaml:"value"`     // temperature_map
	Tokenizer string  `yaml:"tokenizer"` // tokenizer_map
}
```

要点：

- 全部用 `yaml.v3`；**不要**用 `map[string]any` 接收，否则类型安全与错误信息全丢。
- `DatasetBuiltIn` 用 `*bool`：区分「未写」与「显式 false」。
- `time.Duration` / `int64` 需要自定义 `UnmarshalYAML` 支持 `10s` / `200MiB`。
- 字符串类路径统一用 `Path` 类型，**先原样保留字符串，在 `Apply` 阶段用 `Resolver` 转绝对路径**（因为解析需先拿到配置文件目录与 `roots`）。
- 解析完成后立刻跑 `Validate()`，**顺序固定**：

```
解析 YAML
  → 展开 ${ENV}（§2）
  → 应用默认值（§0）
  → 覆写 roots.thvv（-thvv-home）
  → 解析根（roots，按声明顺序，检循环）
  → 解析所有 Path（§3.2，检 ../ 逃逸）
  → 指纹校验（§3.3，文件系统探针）
  → 语义校验（§7）
```

---

## 7. 启动校验清单（fail-fast）

任何一条不满足都直接退出并打印字段路径，**不允许"带病启动"**。
路径类校验（前 11 条）已在 §3.3 列出，本节是语义校验；`-check` 两组都跑。

| # | 校验 | 失败信息示例 |
|---|---|---|
| 0 | `roots.thvv` 非空（或 `-thvv-home` / `THVV_ROOTS__THVV` 提供） | `roots.thvv: required` |
| 1 | `version` 存在且为 `1` | `version: unsupported (got 2)` |
| 2 | `secrets` 密钥解码后 = 32 字节 | `secrets.env.key: must decode to 32 bytes, got 16` |
| 3 | `auth.mode` 合法；`both/bearer` 时 `tokens` 非空；`both/cookie` 时 `users` 非空 | `auth.users: empty while mode=cookie` |
| 4 | bcrypt hash 格式合法（`$2a$/$2b$/$2y$` 开头） | `auth.users[0].password_hash: invalid bcrypt hash` |
| 5 | `store.driver` 受支持；sqlite 目录可写 | `store.sqlite.path: permission denied` |
| 6 | `buckets[].label` 唯一；`min < max`；`max_parallel > 0` | `buckets[3]: min(36000) >= max(28000)` |
| 7 | `number_by_parallel` 的值 > 0 | — |
| 8 | `temperature_map` / `tokenizer_map` 正则可编译 | `tokenizer_map[2].match: invalid regexp` |
| 9 | `gates.*.tiers` 区间不重叠且升序 | `gates.perf-sla-v1.tiers[2]: overlaps previous` |
| 10 | `report.*.title` 模板变量在允许集合内 | `reports.title.single: unknown var {foo}` |
| 11 | `xlsx` sheet 名长度 ≤ 31、不含 `[]:*?/\` | — |
| 12 | `server.write_timeout != 0` 且存在 subprocess 引擎 → **warn**（SSE 会被掐断） | — |
| 13 | `engines.*.runtime.python` 可执行（`enabled` 且 `check_on_startup`） | `engines.thvv.runtime.python: not executable` |
| 14 | `evalscope --version` 成功（同上） | `engines.thvv: evalscope not found` |
| 15 | `perf` 启用时 `paths.datasets_dir` 可用（含回落；见 §3.3） | `paths.datasets_dir: not found` |
| 16 | `gates.default_profile` 存在 | `gates.default_profile: unknown profile "x"` |
| 17 | `kind: http` 工具 `base_url` 合法、`plan_schema` 文件存在 | — |
| 18 | 自定义根名不覆盖保留名（`thvv`/`data`/`bin`/`cfg`）、且无引用循环 | `roots: cycle detected thvv-eval → thvv-eval` |
| 19 | `engines.*.runtime.home`（若有）与 `roots.thvv` 均需通过指纹校验 | `engines.thvv.runtime.home: not a THVV root` |
| 20 | 每个不同的 `runtime.python` 路径都要可执行（去重后逐个检） | `python: /opt/x/bin/python not executable` |

`-check` 模式跑完这 20 条 + §3.3 的路径组就退出（退出码 0/1），适合放进 preflight 脚本或 CI。

---

## 8. 常见场景配方

### 8.1 本地联调（最快上手）

```bash
export THVV_HOME=/opt/thvv                       # ★ THVV 装在哪
export THVV_SECRET_KEY=$(openssl rand -base64 32)
export THVV_CI_TOKEN=local-dev
./thvv-server -config ./config/thvv.min.yaml -check   # 先校验地址与依赖
./thvv-server -config ./config/thvv.min.yaml
open http://127.0.0.1:8080/ui/
```

`thvv.min.yaml` 只写了 7 个必填节，档位表/梯度/阈值/模板全部走内置默认（见 §0）。
默认 `allow_private_targets: true`，可以直接打本地 mock 模型。

### 8.2 thvv 装在别处 / 在另一块盘（本次核心场景）

Go 服务放在 `/usr/local/bin`，THVV 放在 `/data/vendor/thvv`，数据放在 `/data/thvv-data`：

```bash
sudo mkdir -p /data/thvv-data
sudo chown thvv:thvv /data/thvv-data
./thvv-server -config /etc/thvv/thvv.yaml -thvv-home /data/vendor/thvv
```

```yaml
# /etc/thvv/thvv.yaml —— 全文不需要出现 "../"
roots:
  thvv: "/data/vendor/thvv"
  data: "/data/thvv-data"
  bin:  "thvv:.venv/bin"

paths:
  datasets_dir: "thvv:perf/datasets_clean"
store: { driver: sqlite, sqlite: { path: "data:thvv.db" } }
engines:
  thvv:
    runtime: { home: "thvv:", python: "bin:python", evalscope: "bin:evalscope" }
    capabilities:
      perf: { enabled: true, working_dir: "thvv:perf" }
```

权限模型（建议）：

| 目录 | Go 服务运行用户 |
|---|---|
| `roots.thvv` | **只读**（只需读脚本、数据集、模板） |
| `roots.data` | **读写**（sqlite / runs / logs / cache） |
| `HF_HOME`（`data:cache/huggingface`） | 读写（tokenizer 首次下载） |

> `roots.data` 切勿放在 NFS/CIFS 上：SQLite WAL 在网络文件系统上会损坏。大数据盘用本地 SSD 即可。

### 8.3 perf 与 eval 用两套 venv（真实存在的冲突）

`thvv/perf/requirements.txt` 要求 `evalscope[perf]>=0.13.0`，
而 `thvv/eval/requirements.txt` **固定** `evalscope==1.9.0`。两者往往不能共存于同一个 venv。

```bash
/opt/thvv/.venv           # perf：evalscope >= 0.13.0
/opt/thvv-eval/.venv      # eval：evalscope == 1.9.0
```

```yaml
roots:
  thvv: "/opt/thvv"
  thvv-eval: "/opt/thvv-eval"        # 自定义根

engines:
  thvv:
    runtime:
      python: "bin:python"           # = /opt/thvv/.venv/bin/python
    capabilities:
      perf: { enabled: true, working_dir: "thvv:perf" }
      eval:
        enabled: true
        working_dir: "thvv:eval"     # 脚本仍读 thvv 里的（同一份代码）
        script: "thvv:eval/scripts/run_eval.py"
        runtime:                      # 但解释器换到另一套 venv
          python: "thvv-eval:.venv/bin/python"
          evalscope: "thvv-eval:.venv/bin/evalscope"
```

启动时会分别对两个 python 各跑一次自检，日志里能看到两个不同的 evalscope 版本。

### 8.4 只做报告聚合 / 查询（不装 Python）

```yaml
engines:
  thvv:
    enabled: true
    capabilities:
      perf:   { enabled: false }      # 关掉压测执行
      eval:   { enabled: false }
```

服务照常启动，`/v1/tools` 里 `perf` 标 `unavailable`，`POST /v1/runs` 返回 `501`；
但 `/v1/runs/{id}/results`、`/artifacts`、`/history` 全部可用，读的是已有 `benchmark_data.db`。
此模式下 `roots.thvv` 可留空（只校验 `roots.data`）。

### 8.5 提高并发（服务端够强 + 被测端额度足）

```bash
THVV_LIMITS__MAX_CONCURRENT_RUNS=4
THVV_LIMITS__MAX_PARALLEL_RUNS_PER_TARGET=2   # 注意：会互相干扰指标，仅在验收"抗压"时用
```

同时把 `engines.thvv.capabilities.perf.matrix.cooldown` 降到 `60s` 可缩短总时长。

### 8.6 对外部署（关闭内网探测）

```yaml
server: { listen: "0.0.0.0:8080", tls: { enabled: true, cert_file: ..., key_file: ... } }
auth:   { mode: both, session: { secure: true } }
security:
  allow_private_targets: false
  target_host_allowlist: ["api.zhipuai.com", "api.deepseek.com"]
observability:
  log_format: json
```

---

## 9. 明确不可配置的项

避免配置面无限膨胀，以下**写死在代码里**：

| 项 | 理由 |
|---|---|
| API 路由与状态机 | 改配置不该改协议 |
| `benchmark_data.db` 的读取与指标口径 | 口径漂移会毁掉历史可比性 |
| 目录结构 `runs/<run_id>/results/...` | 与 Python 版共用，需稳定 |
| SSE 事件类型 | 前端契约 |
| 错误码枚举 | 客户端契约 |
| `steps_total` 计算算法 | 前后端一致性依赖它 |

---

## 10. 待决问题

1. **`auth.users` 落 YAML vs 独立用户文件**：YAML 里放 bcrypt hash 可行，但账号多了难管。是否需要 `auth.users_file`（同样 YAML，可单独授权 0600）？
2. **`gates` 是否允许运行期通过 API 创建临时模板**（`POST /v1/gates`）？当前只读。
3. **多环境覆盖**：是否要支持 `-config a.yaml -config b.yaml`（后者覆盖前者）？比环境变量直观，但增加心智成本。
4. **`engines.*.capabilities.*.defaults` 与 `Target.defaults` 的优先级**：当前是 `plan.request > target.defaults > engine defaults`，需确认。
5. **`-check` 是否要输出解析后的完整生效配置**（脱敏后）？调试很有用，但可能泄露内部地址。

### 与「地址」相关的待决项

6. **自定义根命名空间**：保留名是 `thvv` / `data` / `bin` / `cfg`。`cfg` 是否改叫 `config` 更直观？
   另外，是否干脆只保留固定三根（`thvv`/`data`/`bin`）+ 绝对路径，不支持自定义根？
   现在的方案允许 `thvv-eval` 这类自定义根，好处是 §8.3 的两套 venv 写得最干净，代价是多一个概念。
7. **`roots.thvv` 与 symlink**：运维常把 `/opt/thvv` 做成指向 `/opt/thvv-1.0.3` 的软链（方便切版本）。
   指纹校验若用 `Lstat` 不跟随链接会误报。建议：**跟随链接校验，但日志里同时打印 resolved 路径**。
8. **一个机子装多个 THVV 版本**：目前靠 `engines.<name>.runtime.home` 注册多个引擎（如 `thvv-0-13` / `thvv-1-9`）。
   是否需要在 `/v1/tools` 里就体现版本区分，并在 `POST /v1/runs` 的 `tool` 里可选？
9. **跨机器（`kind: ssh`）**：DESIGN.md 列为 v2。若近期就有「压测机与被测网关同区域、Go 服务在别处」的需求，
   建议提前定死 `Engine.Run` 的产物回传方式（SFTP 拉回 / 共享盘 / 引擎侧上传 artifact），
   避免 v1 实现把「本地文件路径」写死。
10. **Windows 部署**：盘符歧义已处理（单字母前缀视为绝对路径），但 venv 的 `Scripts/` 与 `bin/` 差异需显式配 `roots.bin`，
   是否要加自动探测？暂定：不探测，报错信息里给出提示。
11. **是否暴露 `GET /v1/system/paths`（admin）**：方便运维查“服务到底用了哪个 thvv/哪个 python”，
    但也暴露内部路径。建议：仅 admin，且与 `-check` 输出同源。
