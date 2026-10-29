# wb2api

WorkBuddy 账号池与多协议网关。把一组 WorkBuddy 账号收进一个本地面板，对外提供
OpenAI Chat Completions、OpenAI Responses 和 Anthropic Messages 三种兼容接口，
同时处理配额、冷却、风控、签到、每日活跃、用量统计与价格估算。

作者：**JnmHub** · 仓库：<https://github.com/JnmHub/wb2api>

## 功能

- **三套协议**：`/v1/chat/completions`、`/v1/responses`、`/v1/messages`（含
  `count_tokens`），支持流式与非流式、工具调用、思考档位、图像输入。
- **账号池**：多账号轮换、权重、按 Key 绑定出口/模型、手动置顶、失败切换、
  429 作用域（账号/模型/6004）、冷却与风控停放、免费模型积分回退。
  模型级 429 冷却会写回账号记录，重启后仍生效；账号可单独关闭每日任务
  （取号与运营任务解耦），面板显示会员档位与免费额度状态。
- **Key 级账号白名单**：可选把 API Key 限制到指定账号；未列入的账号一律 403，
  `/u/<ref>` 显式前缀同样被拒，会话亲和不会绕过白名单。
- **配额与调度**：24 小时窗口探针、主动额度体检、剩余用量估算与优先调度、
  会话亲和（配置 Redis 后跨副本共享）、慢账号逃逸（TTFT/错误率 EWMA 跨副本合并）、
  最低负载优先、可选 top-K 负载分散、PostgreSQL 用量记账；
  账号槽位和 API Key 并发可选用 Redis 跨进程共享，全部满槽时短暂排队后返回 429。
- **可靠用量账本**：每副本独立持久化 outbox，FULL-WAL 批量提交成功后确认，再按稳定事件身份
  异步幂等投递 SQLite/PostgreSQL；`drop`/`sample` 只削减可选明细，不采样已接受的用量事实。
  面板区分本地可写状态、持久化积压和远端投递错误。
- **首包保护**：流式请求在收到语义输出前不向客户端发送响应头；超过
  `WB_FIRST_OUTPUT_MS` 仍未产出时取消该账号并换号，避免把半截 SSE 暴露给客户端。
  流式默认每 15 秒发一次 SSE 注释帧心跳；只有思考、没有任何正文或工具调用的回复
  按失败处理（换号重试），不再当成成功返回。
- **运营**：国内签到/活跃上报/成长任务/猫猫旅行/连登管家，国际每日活跃
  （桌面 + 网页 ACP 会话），OAuth 登录，积分刷新，账号导入导出。每类每日任务
  有自己的整点表（`checkin` / `report` / `daily_chat` / `keepalive`），只在各自
  整点执行，错过最后一个整点后当天会补跑；「立即巡检」与启动巡检不受整点限制。
- **可观测**：账号积分总览（剩余/可用/到期/零积分）、请求记录、用量趋势、模型用量、
  价格估算、日志、活动历史、逐请求详情、CSV 导出、后台任务进度；PostgreSQL 模式下
  后台任务历史跨副本可见，取消请求会转交给任务所在副本。
- **输出限速**：仅对流式响应生效的 TPS 上限，可设全局默认并让每个 API Key 覆盖（0 跟随、
  -1 关闭）；首 token 不受影响，上游计费不变。
- **模型过滤**：可选让 `/v1/models` 只返回当前 API Key 允许的模型（默认关闭，保持全量）。
- **模型路由**：别名映射把客户端模型名换成真实模型（`claude-*` → 实际 id，大小写不敏感，
  在封锁/限额/选号之前解析）；可选只列出当前可用的模型，带别名的模型也会出现在 `/v1/models`。
- **持续连接**：`/v1/models` 与流式输出都带心跳，长思考不会被中间设备当成空闲连接掐断。
- **接入工具**：导入 WorkBuddy 桌面客户端凭据（手工密钥）、一键接管 Claude
  Code / Codex / OpenCode / DSH / Crush / ZCode 的本机配置。
- **本地 web 工具**：可选在网关侧执行 Responses 的 `web_search` / `web_fetch`。
- **安全**：面板登录、API Key、CSRF、TLS 指纹（OpenSSL 形状 ClientHello）、
  私网抓取防护。
- **更新检查**：可选检查 GitHub Releases（启动后一两分钟内先查一次，之后按设定间隔），
  只读，不自动下载。

## 最新发行版

[v2.4.7 更新说明](docs/releases/v2.4.7.md)：面板「清除冷却」现在也解除额度耗尽 park（此前要等 04:00）。
[v2.4.6 更新说明](docs/releases/v2.4.6.md)：额度耗尽（429 `14018`）按账号停用而不是 60 秒模型限流；`code` 支持嵌套解析；面板「冷却」与「模型冷却」分开计数。
[v2.4.5 更新说明](docs/releases/v2.4.5.md)：风控只封命中账号，不再暂停整个区域。
[v2.4.4 更新说明](docs/releases/v2.4.4.md)：修复上游 11128 渠道校验拦截（DSH `@` 路径段落指纹）。
[v2.4.3](docs/releases/v2.4.3.md)：签到判定、会话释放、模型别名/白名单、SSE 心跳等 17 项对齐。
上一版 [v2.4.2](docs/releases/v2.4.2.md)：每日任务独立排程（国际版每日活跃对话可单独设时间）。
[v2.4.1](docs/releases/v2.4.1.md)：修复超长对话绕开会话亲和导致的缓存失效。
[v2.4.0](docs/releases/v2.4.0.md)：高并发调度、可靠记账与三协议流式修复。
**升级前请阅读迁移与默认策略变化；旧版用量 writer 不能与新版混跑，每个副本必须保留独立 outbox。**

## 快速开始（二进制）

1. 从 Releases 下载 `wb2api-linux-amd64`（或 `arm64`）。
2. 放到服务器后直接运行：

```bash
chmod +x wb2api-linux-amd64
./wb2api-linux-amd64 \
  -lan -port 8788 \
  -accounts-dir ./data/accounts \
  -usage-dir ./data/usage \
  -catalog-dir ./data/catalog \
  -panel-password '换成一个强密码'
```

3. 浏览器打开 `http://<服务器>:8788/`。

首次启动会自动建目录。面板密码也可以用环境变量 `WB_PANEL_PASSWORD` 提供。
没有设置密码时初始密码是 `admin`，登录后会强制修改。

## Docker Compose

一键启动应用、PostgreSQL 和 Redis：

```bash
export WB_PANEL_PASSWORD='换成一个强密码'
export WB_POSTGRES_PASSWORD='换成一个数据库密码'
docker compose up -d --build
```

面板：`http://127.0.0.1:8788/`。

需要验证多副本时，用独立的 HA 编排（两个 wb2api + Caddy，独立数据卷）：

> **已有数据时不要直接切换编排来“升级”。** 单副本默认用 `pgdata`，HA 默认用
> `hapgdata`，Compose 项目名还会改变卷名前缀。切换后看到空设置/空 Key 列表，
> 可能是连接了另一份数据库，而不是原数据被删除。先备份并核对实际 DSN、项目名和挂载。
> 详见[数据保全与排查指南](<docs/SETTINGS_RECOVERY.md>)。

```bash
docker compose -f compose.ha.yaml up -d --build
```

入口仍是 `http://127.0.0.1:8788/`，Caddy 按最少连接转发到两个应用副本。两份应用共享
PostgreSQL/Redis；启动时的 schema 迁移和旧用量导入都由 advisory lock 串行化，不会因
两个副本同时启动而重复建表或重复写入。面板任务历史写入 `wb_jobs`，在一个副本创建的任务可从另一个副本查看和取消。
`compose.yaml` 的单副本用法不变。

- **PostgreSQL**：账号、`settings.json`、**请求用量**、**面板登录会话**、**价格覆盖**、
  **模型额度窗口**和**主动额度扫描计划**的权威存储；首次启动会把现有
  `./data/accounts/*.json`、`settings.json`、`usage.jsonl`、旧版 `usage.db`、
  `panel_sessions.json`、`pricing_policies.json`、`model_windows.json` 和
  `quota_scan.json` 导入数据库，之后读写走数据库。`usage.db` 只读导入，原文件保留，
  导入进度记录在 `meta` 表；价格覆盖在副本间 5 秒内同步，额度窗口和扫描计划 2 秒内
  同步。后台巡检、额度探针和余额刷新通过 PostgreSQL advisory lock 只在一个副本上运行。
- **Redis**：跨进程的账号并发槽位（`WB_REDIS_INFLIGHT`，CLI 默认 0，Compose
  默认 8）、API Key 并发槽位、会话亲和绑定、调度 EWMA 快照和健康检查；所有账号
  都满槽时可排队 `WB_REDIS_WAIT_MS`（默认 1500）再返回 429，减少冷账号溢出；
  不配 Redis 时退回本地并发限制和本地 EWMA。
- **`./data`**：仍保留 catalog 等文件；PostgreSQL 模式下 active realm、模型额度窗口
  和主动额度扫描计划存在 `wb_state` 表，分别 1 秒、2 秒内同步；后台任务快照存在
  `wb_jobs` 表，其他副本 2 秒内可见。旧版 standalone 模式不需要 PostgreSQL/Redis，
  继续使用文件 + `usage.db`，后台任务仍只保存在当前进程。
- 容器以非 root 用户运行，应用健康检查走 `/health`。

停止服务（保留数据卷）：

```bash
docker compose down
```

**升级、排障、恢复数据时不要使用 `down -v`；它会删除 PostgreSQL/Redis 数据卷。**
多副本升级前先备份，替换全部旧副本后再开放写入；新旧设置写入协议混用仍可能覆盖数据。
本轮缺陷修复、实测数据与未证实事项见[审查报告](<docs/REVIEW_2026-10-22.md>)。

如果只想拉镜像（自行构建并推送后）：

```yaml
services:
  wb2api:
    image: ghcr.io/jnmhub/wb2api:latest
    ports: ["8788:8788"]
    environment:
      WB_PANEL_PASSWORD: ${WB_PANEL_PASSWORD}
      WB_POSTGRES_DSN: postgres://wb2api:wb2api@postgres:5432/wb2api?sslmode=disable
      WB_REDIS_ADDR: redis:6379
    volumes: ["./data:/data"]
```

## 配置

主要命令行参数：

| 参数 | 默认值 | 说明 |
|---|---|---|
| `-postgres-dsn` / `WB_POSTGRES_DSN` | 空 | 启用 PostgreSQL 账号/设置/用量存储；空则使用文件 + `usage.db` |
| `-redis-addr` / `WB_REDIS_ADDR` | 空 | 启用 Redis；空则不启用跨进程状态 |
| `-redis-inflight` / `WB_REDIS_INFLIGHT` | `0` | 每账号 Redis 并发槽位；0 = 关闭 |
| `-redis-wait-ms` / `WB_REDIS_WAIT_MS` | `0` | 所有账号都满槽时排队等待毫秒数，超时才返回 429；0 = 不排队 |
| `-first-output-ms` / `WB_FIRST_OUTPUT_MS` | `0` | 流式首包超时；超过该毫秒数仍无语义输出则换号，0 = 关闭（Compose 默认 30000） |
| `-port` | `8788` | 监听端口 |
| `-host` / `-lan` | `127.0.0.1` / 关 | 监听地址；`-lan` 等价于所有网卡 |
| `-accounts-dir` | `accounts` | 账号 JSON 目录 |
| `-usage-dir` | `usage` | `usage.db` 与日志 |
| `-usage-queue` / `WB_USAGE_QUEUE` | `8192` | 可选请求明细的降载阈值，不是可靠账本的容量 |
| `-usage-workers` / `WB_USAGE_WORKERS` | `1` | 兼容参数；持久化 outbox 按每副本单写者顺序投递 |
| `-usage-overflow` / `WB_USAGE_OVERFLOW` | `drop` | 仅对可选明细降载：`drop` 或 `sample`；不采样已接受的配额事实 |
| `-usage-sample` / `WB_USAGE_SAMPLE` | `20` | `sample` 模式下达到明细阈值 75% 后每 N 条保留 1 条明细 |
| `-usage-outbox` / `WB_USAGE_OUTBOX` | usage-dir 下的 `usage-outbox.db` | 每副本独占、重启后保留的本地持久化 outbox，不能多个副本共用 |
| `-usage-outbox-mb` / `WB_USAGE_OUTBOX_MB` | `256` | 待投递有效载荷预算（MiB）；SQLite 索引、WAL、空闲页另占空间 |
| `-shared-limit-fail-open` / `WB_SHARED_LIMIT_FAIL_OPEN` | 关 | Redis 限流不可用时默认拒绝新准入；显式开启才退回本地，故障时不保证集群上限 |
| `-downstream-write-ms` / `WB_DOWNSTREAM_WRITE_MS` | `30000` | 每次实际客户端写入/刷新超时；不包含 TPS 主动等待，负数关闭 |
| `-catalog-dir` | `catalog` | 模型目录缓存 |
| `-panel-password` | 空 | 面板密码，也可用 `WB_PANEL_PASSWORD` |
| `-max-attempts` | `5` | 单请求最多切换账号次数 |
| `-max-body-mb` | `16` | 单请求体上限（MiB）；也可用 `WB_MAX_BODY_MB` |
| `-per-account-inflight` | `0` | 单账号并发上限，0 = 不限（网关不额外限制） |
| `-sticky-escape-ttft-ms` | `15000` | 会话亲和账号 TTFT EWMA 超过该值时临时逃逸 |
| `-sticky-escape-error-rate` | `0.5` | 会话亲和账号错误率 EWMA 超过该值时临时逃逸 |
| `-sched-top-k` / `WB_SCHED_TOP_K` | `1` | 从评分最优的 K 个可用账号中选择；1 = 严格最优 |
| `-no-modelsdev` | 关 | 不访问 models.dev 的额度数据 |
| `-debug` | 关 | 暴露 `/debug/runtime` |

风控（403 / `11140`）默认策略是**只封该账号，不封网关**：命中的账号写入 `restrictedUntil`（约 2 小时，重启后仍在，面板可见）并被选择器跳过，当前请求继续用其它账号完成；不会把整个区域暂停。需要恢复旧的「连坐保护」（同区域多个账号受限就整体暂停）时显式开启：

| 环境变量 | 默认 | 说明 |
| --- | --- | --- |
| `WB_RESTRICTED_403_ROTATE_MAX` | `2` | 一次请求在撞到受限账号后还可以换几个账号；0 = 立刻把 403 返回客户端 |
| `WB_RESTRICTION_HOLD_SECONDS` | `0` | 区域暂停秒数；0 = 不暂停（推荐）。设为正数才启用连坐暂停 |
| `WB_RESTRICTION_WAVE_MIN` | `2` | 窗口内多少个不同账号受限才触发暂停（仅在上一项 > 0 时生效） |
| `WB_RESTRICTION_WAVE_WINDOW` | `900` | 上述窗口秒数 |


用量完成事件先写入本地持久化 outbox，再异步交付 SQLite/PostgreSQL 汇总。远端故障时可继续记账，直到本地容量或存储不可用；`/api/v2/usage/summary` 的 `durability` 区分本地可写状态、待交付事件与远端错误。配额读取的是已交付汇总，因此远端长时间中断时并非实时硬配额。完整边界、迁移和恢复要求见 [用量持久化契约](internal/usagedb/DURABILITY.md)。HA 配置为每个副本提供独立 outbox 卷；不要删除这些卷或使用 `down -v` 来升级。

常用环境变量：`WB_ACCOUNTS_DIR`、`WB_PROXY_USAGE_DIR`、
`WB_PROXY_CATALOG_DIR`、`WB_PANEL_PASSWORD`、`WB_UPDATE_REPO`、
`WB_MAX_BODY_MB`、`WB_TLS_PROFILE=go`（退回 Go 标准 TLS 指纹）。
批量账号操作（刷新积分、签到、每日活跃、批量检测）默认最多 4 个并发上游请求。
在“设置 → 批量账号任务”里可以直接调整并发数（1–16）和每个账号操作后的间隔
（0–60000 毫秒），保存立即生效，不需要重启；账号很多、批量任务影响正常请求时，
调低并发并加间隔即可（批量会变慢，但对同一出口 IP 的突发压力更小）。

面板里可以修改出口、API Key、限额、开关、上游超时、提示词、探针、模型封锁、
价格映射、更新检查、代理槽与智能体配置。

## HTTP 接口

网关（需要 `Authorization: Bearer sk-...`）：

- `POST /v1/chat/completions`
- `POST /v1/responses`
- `POST /v1/messages`、`POST /v1/messages/count_tokens`
- `GET /v1/models`
- `GET /health`

面板（Cookie + CSRF）：

- `/api/v2/session/*`、`/api/v2/settings`、`/api/v2/keys`
- `/api/v2/accounts/*`（含导入、导出、桌面导入、批量操作）
- `/api/v2/usage/*`、`/api/v2/pricing/*`
- `/api/v2/logs`、`/api/v2/activity`、`/api/v2/tasks`、`/api/v2/scheduler`
- `/api/v2/agents`、`/api/v2/models`、`/api/v2/updates`

## 从源码构建

需要 Go 1.27+ 与 Node.js 22+：

```bash
cd web && npm ci && npm run build && cd ..
go build -trimpath -o bin/wb2api ./cmd/hub
```

测试：

```bash
go test ./...
cd web && npm test && npm run build
```

浏览器回归（本机需 Edge，面板已启动）：

```bash
cd web
npm run uitest -- --url http://127.0.0.1:8788 --password '你的面板密码'
```

## GitHub Actions

`.github/workflows/release.yml` 在推送 `v*` 标签时构建 Linux `amd64` / `arm64`
二进制并发布 Release；`workflow_dispatch` 可只构建产物不发布。构建时会把标签写进
`main.version`，面板的更新检查据此比较版本。

## 更新检查

在“设置 → 更新检查”里开启后，后台每天最多查一次 GitHub Releases；只读取版本，
不下载、不替换、不重启。面板同时提供仓库链接与发行页链接。仓库可用
`WB_UPDATE_REPO=owner/name` 覆盖。

## 安全说明

- 面板默认只监听 `127.0.0.1`；用 `-lan` 暴露到公网前务必设置强密码和防火墙。
- 账号 Token、API Key 与 keyblob 密钥只保存在本机，勿提交到仓库。
- 代理槽、模型封锁、价格映射与智能体接管都会改动配置；智能体接管前会备份原文件，
  恢复时按备份还原。
- 桌面凭据是加密的；没有密钥时请使用浏览器 OAuth 登录或手工密钥导入。
