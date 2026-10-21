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
- **配额与调度**：24 小时窗口探针、主动额度体检、剩余用量估算与优先调度、
  并发不设网关侧上限（由上游决定）。
- **运营**：国内签到/活跃上报/成长任务/猫猫旅行/连登管家，国际每日活跃
  （桌面 + 网页 ACP 会话），OAuth 登录，积分刷新，账号导入导出。
- **可观测**：账号积分总览（剩余/可用/到期/零积分）、请求记录、用量趋势、模型用量、
  价格估算、日志、活动历史、逐请求详情、CSV 导出、后台任务进度。
- **输出限速**：仅对流式响应生效的 TPS 上限，可设全局默认并让每个 API Key 覆盖（0 跟随、
  -1 关闭）；首 token 不受影响，上游计费不变。
- **模型过滤**：可选让 `/v1/models` 只返回当前 API Key 允许的模型（默认关闭，保持全量）。
- **接入工具**：导入 WorkBuddy 桌面客户端凭据（手工密钥）、一键接管 Claude
  Code / Codex / OpenCode / DSH / Crush / ZCode 的本机配置。
- **本地 web 工具**：可选在网关侧执行 Responses 的 `web_search` / `web_fetch`。
- **安全**：面板登录、API Key、CSRF、TLS 指纹（OpenSSL 形状 ClientHello）、
  私网抓取防护。
- **更新检查**：可选每天检查 GitHub Releases，只读，不自动下载。

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

- **PostgreSQL**：账号和 `settings.json` 的权威存储；首次启动会把现有
  `./data/accounts/*.json` 和 `settings.json` 导入数据库，之后读写走数据库。
- **Redis**：跨进程的账号并发槽位（`WB_REDIS_INFLIGHT`，默认 8）和健康检查；
  账号满槽时可排队 `WB_REDIS_WAIT_MS`（默认 1500）再切换，减少冷账号溢出；
  不配 Redis 时退回本地并发限制。
- **`./data`**：仍保留 catalog、usage、panel 会话等文件；旧版 standalone 模式不需要
  PostgreSQL/Redis。
- 容器以非 root 用户运行，应用健康检查走 `/health`。

资源清理：

```bash
docker compose down          # 保留数据卷
docker compose down -v       # 删除 PostgreSQL/Redis 数据
```

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
| `-postgres-dsn` / `WB_POSTGRES_DSN` | 空 | 启用 PostgreSQL 账号/设置存储；空则使用 `accounts/*.json` |
| `-redis-addr` / `WB_REDIS_ADDR` | 空 | 启用 Redis；空则不启用跨进程状态 |
| `-redis-inflight` / `WB_REDIS_INFLIGHT` | `0` | 每账号 Redis 并发槽位；0 = 关闭 |
| `-redis-wait-ms` / `WB_REDIS_WAIT_MS` | `0` | 满槽时排队等待毫秒数，超时才切换账号；0 = 立即切换 |
| `-port` | `8788` | 监听端口 |
| `-host` / `-lan` | `127.0.0.1` / 关 | 监听地址；`-lan` 等价于所有网卡 |
| `-accounts-dir` | `accounts` | 账号 JSON 目录 |
| `-usage-dir` | `usage` | `usage.db` 与日志 |
| `-catalog-dir` | `catalog` | 模型目录缓存 |
| `-panel-password` | 空 | 面板密码，也可用 `WB_PANEL_PASSWORD` |
| `-max-attempts` | `5` | 单请求最多切换账号次数 |
| `-max-body-mb` | `16` | 单请求体上限（MiB）；也可用 `WB_MAX_BODY_MB` |
| `-per-account-inflight` | `0` | 单账号并发上限，0 = 不限（网关不额外限制） |
| `-sticky-escape-ttft-ms` | `15000` | 会话亲和账号 TTFT EWMA 超过该值时临时逃逸 |
| `-sticky-escape-error-rate` | `0.5` | 会话亲和账号错误率 EWMA 超过该值时临时逃逸 |
| `-no-modelsdev` | 关 | 不访问 models.dev 的额度数据 |
| `-debug` | 关 | 暴露 `/debug/runtime` |

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
