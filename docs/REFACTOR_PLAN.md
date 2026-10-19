# workbuddy2api-hub → Go 重构计划书

> 对象：<https://github.com/ardeyouxipianyi/workbuddy2api-hub>（v1.7.16，Python，MIT）
> 范围：① 后端用 Go 重写以解决并发瓶颈 ② 前端重做以解决「账号多了卡」③ 解决「登录信息不持久」
> 状态：草案，待确认第 15 节的决策点后进入 Phase 0
> 依据：本文所有"现状"结论均来自对仓库源码的阅读（文件与行号见各处），"目标/估时"是我的建议值，不是实测数。

---

## 0. 摘要

| 问题 | 根因（已在源码中确认） | 重构方案 |
|---|---|---|
| 并发性能差 | 标准库 `ThreadingHTTPServer` 每请求一线程 + GIL；对话并发被硬限 32 并在 30s 后返 503；上游 `http.client` 连接池手写；SSE 读取需手工分块规避 GIL 争用 | Go：goroutine 每连接、`net/http` + 按账号隔离的 `http.Transport`、无全局信号量（改为按账号/按池的背压）、写时复制的账号快照让调度无锁 |
| 前端卡 | 单文件 `dashboard.html`（15,373 行 / 855 KB）；账号表每 15 秒整表 `innerHTML` 重绘，无分页无虚拟化；全量 `GET /accounts?realm=all` 轮询；多个 5 秒轮询 | SPA（Vite + TS）；账号列表服务端分页/筛选/排序 + 虚拟滚动；SSE 推送增量取代轮询；批量操作走后台任务 |
| 登录不持久 | token 存 `sessionStorage`（关标签即失）；服务端 `PanelSessions` 是**刻意不落盘**的内存表（`wb_settings.py:2200`），重启即全员掉线 | HttpOnly Cookie + 服务端会话落 SQLite（哈希存储）、滑动续期、可多设备管理/撤销 |

**核心判断**：这不是"换语言重写 21k 行"这么简单——上游协议、风控/指纹口径、各种兼容修补沉淀在 `wb_proxy.py` 的 21,001 行和 172 个测试套件里。因此计划采用**分阶段、可并行对照验证**的路线，而不是一次性大爆炸重写；`/v1/*` 对外契约冻结，数据目录原样兼容，用户可以原地替换二进制。

---

## 1. 现状诊断

### 1.1 代码体量

| 项 | 数据 |
|---|---|
| Python 源码（`wb_*.py`） | ≈ 41,000 行，其中 `wb_proxy.py` 21,001 行、`wb_accounts.py` 4,239、`wb_pricing.py` 2,556、`wb_settings.py` 2,238 |
| 前端 | `dashboard.html` 15,373 行 / 855 KB，单文件，内联 JS/CSS，147 处 `innerHTML` 赋值 |
| 测试 | 172 个套件（127 Python + 45 JS，JS 靠 node 跑 DOM stub），`tests/` 下 180 个文件 |
| 部署形态 | Windows 绿色包（内置精简 Python）、macOS/Linux 脚本、Docker（amd64/arm64）、OpenWrt（ipk/apk，路由器内存约 480 MB 级别） |
| 依赖 | 纯标准库，零第三方依赖（这既是优点——部署简单——也是性能上限的来源） |

### 1.2 并发瓶颈（后端）

1. **线程模型**：`class GatewayServer(ThreadingHTTPServer)`（`wb_proxy.py:20933`）、`class Handler(BaseHTTPRequestHandler)`（`:16718`）。每个请求（含长达 600 秒的 SSE）占一个 OS 线程。
2. **硬并发上限**：`_chat_slots = threading.BoundedSemaphore(MAX_CONCURRENT_CHAT)`（`:346`），默认 32；获取不到等 `WB_CHAT_SLOT_WAIT=30s` 后返回 503。与账号池大小无关（除非设 `auto`）。代码注释自己解释了原因：线程无上限 + 大 body 会把路由器内存打穿。
3. **GIL 争用已经被作者在代码里量化过**：`iter_sse_lines`（`:14246`）注释记载 4 vCPU 真机 15–20 req/s 时 10 秒内 106,214 次 futex（占 syscall 时间 71%）。作者用 `read1()` 分块读、攒写等手段缓解，说明已在单进程 Python 的天花板附近做优化。
4. **连接复用靠手写**：`wb_upstream_pool.py` 因为 urllib 强制 `Connection: close` 自建 keep-alive 池，且**按账号凭据摘要分键**（同一出口的两个账号绝不共用 TCP/TLS 连接——这是项目的隔离口径，Go 版必须保留，见 4.3）。
5. **用量统计靠重读日志**：`usage.jsonl` 追加写，看板的各种聚合（snapshot / by_account / analytics / series / remaining）靠"增量扫描 + 偏移缓存文件"（`usage-aggregate-cache-<kind>.json`）撑着，仅聚合相关函数就有 77 个。这是复杂度来源，也是 CPU 热点。
6. **全局锁**：`wb_proxy.py` 27 处、`wb_accounts.py` 10 处 `Lock/RLock`；账号池 `pick()` 虽已做了快照，但权重/临期/剩余用量三层偏好 + 轮询的状态更新仍要持池锁。

### 1.2.1 流式吞吐 / TPS 问题（补充，已用脚本实测）

流式转发在 `_chat_stream_response`（`wb_proxy.py:20469`）。对每个上游 chunk，Python 版做了：`json.loads` → 改写 → `json.dumps` → `clean_chunk()` 里**再 `json.loads` 一次、可能再 `json.dumps`** → 有正文时 `chat_text_chunk()` **又 `json.dumps` 一次**。即每个 token chunk 4 次 JSON 编解码，且全部在 GIL 下，所有并发流共享一个核。

**更严重的是 `LeakedToolCallStreamFilter`（`:12217`）**：为了抓"模型把工具调用当文本打印出来"的情况，它会把可疑文本**扣住不发**。触发词包括 `<think`、`<arg`、`<param`、`<invoke`、`<calls`、`<function_call` 等前缀。一旦正文里出现 `<args>` 或 `<thinking>`，且后面没有出现能否定它的内容，**之后的全部文本都被缓冲，直到流结束（或攒满 256 KB）才一次性吐出**；而且每来一个 chunk，`feed()` 都用 Python 循环从头逐字符扫描整个缓冲区，复杂度 O(n²)。

我把这几个函数原样抽出来，用合成流在本机跑了对照（Python 3.13，单线程，不含网络）：

| 场景 | 结果 |
|---|---|
| 普通文本 chunk | 约 11 µs/chunk（约 8.9 万 chunk/s/核），并**逐条即时输出**——这条路径本身不慢 |
| 第 50 个 chunk 出现 `<args>` 或 `<thinking>`，后面 3000 个普通 chunk | 3051 个 chunk 中**只有前 50 个在流结束前发出**；客户端看到输出"卡住"，平均 137–151 µs/chunk |
| 同样的缓冲，chunk 数 1000 → 4000 → 16000 | 平均 48 → 172 → **665 µs/chunk**；16000 个 chunk 共耗 **10.6 秒 CPU**，期间持有 GIL，其他所有流一起被拖慢；输出始终为 0 |
| 正文含 `List<String>` 这类泛型 | 不触发，逐条输出（说明触发面主要是上面那批标签前缀） |

后果：
1. 编码类客户端的输出里出现 `<think…>`/`<args…>`/`<param…>` 等标签时，用户看到"先卡住，最后一口气全出来"，**体感 TPS 骤降**；
2. 长输出时过滤器 O(n²) 吃满 GIL，拖累同进程里其他所有流——这是"并发上来后整体变慢"的一个直接原因；
3. **面板里的 TPS 指标也会失真**：`tokens_per_sec = completion_tokens / (gen_ms/1000)`（`wb_proxy.py:860`），而 `gen_ms = 总耗时 − first_ms`，`first_ms` 取的是"第一个**写给客户端**的 chunk"的时间（`write_chunk`），不是上游首字节。文本被扣住时 TTFT 被拉长、`gen_ms` 被压短，算出来的 t/s 会**虚高**；
4. 上游没给 `usage` 时回退为 `estimate_tokens`（CJK 按 1 字=1 token，其余按 3.6 字符=1 token），且 `prompt_tokens` 直接写成 `completion//2`，这是编造值，会污染统计与定价估算。

### 1.2.2 其他影响使用感受的问题（第二轮深度审计）

> 实测说明：下列数字来自我在本机（Windows，Python 3.13，单线程）对原项目函数的微基准，数据为合成数据，**只用于判断量级和相对占比**，不代表你机器上的绝对值。脚本在 `C:\wb2a\bench\`。

**已经调优、不需要动的部分**（避免误判）：服务端 listen backlog 已提到 128、已关 Nagle、客户端侧 HTTP/1.1 keep-alive、SSE 已做分块读与合并写、面板首页有 ETag/304。这些在 Go 版里默认就有，不是问题来源。

**A. 请求路径上的延迟 / 卡顿（按影响从大到小）**

| # | 问题 | 证据 | 体感 |
|---|---|---|---|
| A1 | **选号时同步刷新 token，且失败不退避** | `Account.ready()`（`wb_accounts.py:1317`）：token 剩余 ≤120 秒或已过期时直接调用 `self.refresh()`，内部是 30 秒超时的同步 HTTP。`open_upstream` 每个请求先调用 `count_ready()` 遍历全池的 `ready()`，`pick()` 又遍历一遍。非 401 的刷新失败**不会**标记 `token_expired`、也没有退避，下一个请求会再试一次。主动保活只在整点巡检（`wb_scheduler.py:265` 附近，仅 09/21/22/01 点）。 | 白天 token 到期的账号会让**恰好撞上的那个请求**卡几秒到 30 秒；刷新接口或代理出口不稳时，每个请求都可能重复踩坑；账号越多概率越高 |
| A2 | **故障转移串行 + 超时很长** | `upstream_timeouts()` 默认 header 超时 **120 秒**（覆盖连接/TLS/首字节），空闲 300 秒，且没有总时长上限；换号重试在同一个请求线程内顺序进行，之间还有 `time.sleep`（连接抖动 0.6–2 秒、无池 1.5–3 秒、本机网络 0.3–1.5 秒、出口退避至多 `EGRESS_SLEEP_MAX`）；整个过程一直占着对话槽位。 | 某个账号/代理出口"挂起不拒绝"时，用户这一条要等**最长 2 分钟**才会换号；连续几个坏号会叠加 |
| A3 | **每次重试都重新编码并 gzip 整个请求体** | `attempt_data = encode_upstream_body(...)`、`gzip_body(...)` 都在重试循环里（`wb_proxy.py:13670` 附近）；而同一个 body 对不同账号只有缓存键等少数字段不同 | 大上下文换号一次多付 20ms+ CPU 和一次内存拷贝 |
| A4 | **请求前置 CPU 开销，尤其是纯诊断用的 `prompt_fingerprint`** | 同一条 3.0 MB（600 条消息）会话：`json.loads` 4.9 ms、`build_upstream_body` 9.4 ms、**`prompt_fingerprint` 19.5 ms（对整个消息列表 `sort_keys` 序列化+SHA256 三遍）**、`encode` 8.7 ms、`gzip` 10.7 ms，合计约 50 ms。0.8 MB 会话约 14 ms，0.1 MB 约 1.7 ms。 | 都在 GIL 内；agent 类客户端上下文大、轮次密，每一轮都要付；同进程其他流会因此出现几十毫秒的"出字停顿" |
| A5 | **选号 O(账号数)，且多遍扫描** | `count_ready` + `pick` 每个请求遍历全池并多次调用 `ready()`。实测：100 账号 ≈ 0.13 ms/请求；1,000 账号 ≈ 1.2 ms；**5,000 账号 ≈ 8 ms**（`count_ready` 6.8 ms + `pick` 1.3 ms） | 账号上千后，纯选号就吃掉一个核的相当一部分；`count_ready` 其实只是为了算 `max_attempts` |
| A6 | **流式过滤器扣留 + O(n²)、每 chunk 4 次 JSON** | 见 1.2.1 | 输出突然停住然后一次性吐出；长输出 CPU 飙升 |
| A7 | **对话槽位 32 + 读 body 在拿槽位之前 + 线程无上限** | `wb_proxy.py:20336`：先 `_payload_or_error` 读完并解析 body，再 `_chat_slots.acquire(30s)`；超时返回 503 | 突发时请求在"已占线程、尚未占槽"的状态里排队，慢上传客户端会白占线程；用户看到 30 秒后的 503 |

**B. 面板对网关本身的拖累**

| # | 问题 | 证据 | 体感 |
|---|---|---|---|
| B1 | **统计接口冷启动全量扫描日志，同步在 HTTP 线程内、持锁** | 合成 200,000 行（103 MB）日志、**关闭落盘聚合缓存**（`WB_USAGE_CACHE=0`）的最坏情况：`usage_snapshot` **16.9 s**、`compute_usage_analytics` **19.4 s**、`usage_timeseries` 3.3 s、`usage_by_account` 1.2 s、`recent_usage(20)` 0.23 s；稳态命中 TTL 缓存仅 3 ms。`compute_usage_analytics` 在 `_analytics_lock` 内重建，其他轮询请求排队。 | 打开"数据看板"首屏转圈十几秒；面板每 5 秒轮询，重建期间会堆积请求线程。真实环境有偏移缓存文件，重启后不会每次都这么慢，但**缓存校验失败（价格表/区域分配变化、日志轮转）会回到全量**；日志越长越慢 |
| B2 | **面板首页与 JSON 都不压缩** | 全仓库没有对响应做 gzip/br；`dashboard.html` 现在是 855 KB，`_dashboard()` 每次请求从磁盘整读（README 里写的"约 470KB"已过时）；只有 ETag/304 缓解了回访 | 首次打开或升级后，在手机/Tailscale/公网访问下载近 1 MB；账号 JSON 大时同理 |
| B3 | **每个请求至少 2 行 stderr 日志并 flush；每条用量一次 open/append/close** | `log()`（`:10269`）每次 `sys.stderr.write + flush`；`_persist_usage`（`:887`）每条请求单独打开关闭文件 | 默认 `pythonw` 无控制台不受影响；用可见控制台窗口启动时，Windows 的"快速编辑"点选会让整个进程在写日志处卡住，直到按键。杀毒软件对频繁 open/close 的扫描也会放大延迟 |

**C. 数据正确性 / 可观测性**（"体感"是被误导）

- TPS 虚高、编造 `usage`（见 1.2.1）；
- 定价/积分估算依赖 `usage`，编造值会进入"API 等价花费"；
- 用量口径依赖增量扫描 + 偏移缓存的 77 个聚合函数，边界（0 点、窗口起点、闭合窗口桶）极易漂移——这也是 Go 版决定改用 SQL 预聚合的原因。

### 1.3 前端问题

- `renderAccounts()`（`dashboard.html:7849`）每次整表重建 HTML 字符串；`loadAccounts()`（`:7904`）每 15 秒并发拉 `/accounts?realm=all` + `/usage/by-account` + 今日活动，再 `renderAccounts()` + `updateUI()`。账号数 N 时每轮 O(N) DOM 重建。
- `refresh()` 每 5 秒一轮，统计页再加 5 秒一轮（`:9618–9624`）；源码注释自述后台标签页曾实测 50～110 次/分钟请求。
- 无分页、无虚拟滚动、无服务端筛选；账号太多时项目自己加了"账号独立成页签""折叠"等补丁（README 第五节）——是在**缓解**而不是解决。
- 单文件 855 KB：首屏必须整体下载解析；所有页（网关/账号/数据看板/智能体/设置/日志）的代码一起加载。

### 1.4 登录不持久

- 客户端：`PANEL_TOKEN = sessionStorage.getItem(PANEL_STORE)`（`dashboard.html:6846`），写入也是 `sessionStorage`（`:9538`、`:10624`）。**关闭标签页/浏览器即丢失**。
- 服务端：`PanelSessions`（`wb_settings.py:2200`）docstring 明确 *"Deliberately not persisted: restarting the gateway logs browsers out"*。Docker 自动更新（Watchtower）、面板里"拉取最新镜像并重启"都会让所有人掉线。
- 附带问题：`?pwd=面板密码` 免输入方式会把密码留进浏览器历史与反代日志（README 自己也警告了）。
- 另一处"不持久"：从桌面端回收的解码密钥"只留在网关进程内存、不落盘"（README 第三节），重启后要重新回收/粘贴。若你说的"登录信息"也包含这个，见 6.4。

---

## 2. 目标与非目标

### 2.1 目标（建议的可验收指标）

> 以下数字是**提案**，Phase 0 会先在 Python 版上测基线，再据此校准。

| 维度 | 指标 |
|---|---|
| 并发 | 单进程稳定承载 ≥ 2,000 条并发 SSE 流（对 mock 上游），无全局 32 上限；网关自身额外 TTFT 开销 P99 < 5 ms |
| 资源 | 2,000 并发流时 RSS < 300 MB；空闲 RSS < 40 MB；二进制 < 30 MB（含前端） |
| 账号规模 | 面板在 5,000 账号下首屏 < 1.5 s、滚动 60 fps、单次操作响应 < 200 ms |
| 登录 | 关闭浏览器/重启网关后仍保持登录（默认 30 天滑动续期，可配置） |
| 兼容 | `/v1/*` 请求/响应与旧版字节级或语义级一致（见第 11 节的差分测试）；旧 `accounts/`、`usage/` 目录零操作可用 |
| 部署 | 单个静态二进制（前端 `go:embed`），Docker 镜像 < 40 MB，无需 Python/Node 运行时 |

### 2.2 非目标

- 不做新功能（Phase 6 之前冻结功能面）；不改变调度语义、不扩大对上游的访问面。
- 不改变"每账号一台设备"的出站身份口径，也不新增任何风控规避手段——仅按现有行为 1:1 移植。
- 不在首个版本里重做 `wb_agents` 这类与网关热路径无关的工具（见第 9 节分级）。

---

## 3. 技术选型

### 3.1 后端

| 领域 | 选择 | 理由 / 备选 |
|---|---|---|
| 语言/版本 | Go 1.23+ | 当前稳定；`net/http` 路由增强（方法+路径模式）已够用 |
| HTTP 服务 | 标准库 `net/http` + 薄封装（或 `chi`） | 避免 Gin/Echo 的 Context 绑定，SSE 与超时控制更直接 |
| 上游客户端 | 标准库 `http.Transport`，**每账号（含代理出口）一个独立 Transport** | 对应 Python 池的"按凭据摘要分键"；需要时可换 `utls` 控制 TLS 指纹（见风险 R1） |
| JSON | `encoding/json` 起步，热路径（SSE chunk）用 `goccy/go-json` 或 `sonic`，局部取值用 `gjson` | 只在基准显示 JSON 成为热点后再换，避免过早优化 |
| 持久化 | 账号/设置：沿用 JSON 文件；用量/日志/会话：**SQLite（WAL）**，驱动用 `modernc.org/sqlite`（纯 Go，无 CGO） | 无 CGO → 交叉编译到 Windows/ARM/OpenWrt 简单；备选 DuckDB 不适合嵌入小设备 |
| 配置 | 同名环境变量 `WB_*` + 同名 CLI 参数 | 保持 Docker/脚本兼容 |
| 日志 | `log/slog`（结构化），环形缓冲 + 可选 SQLite 持久化（复刻 `wb_logstore`） | |
| 观测 | `/metrics`（Prometheus 文本）、`net/http/pprof`（仅回环 + 面板鉴权） | 压测和线上定位必需 |
| 定时任务 | 自写小调度器（goroutine + `time.Timer`，复刻 09:00/21:00/22:00/01:00 排程） | 不引 cron 库，语义简单 |

### 3.2 前端

建议 **React 18 + TypeScript + Vite + TanStack（Query / Table / Virtual）+ Tailwind**，原因是本问题的核心恰好是"服务端状态缓存、去重、分页、虚拟滚动"，TanStack 全家桶是现成解。

备选：Vue 3 + Pinia + `vue-virtual-scroller`（若你更熟 Vue）；Svelte（体积最小，但生态和表格虚拟化组件少）。**这是第 15 节的待确认项之一**，不影响后端。

- 构建产物 `go:embed` 进二进制，Cache-Control 用内容哈希文件名 + `immutable`。
- 图表：沿用现有"时序/热力/透视"需求，用 `uPlot`（轻量，万级点位流畅）。
- i18n：现有有简/繁/英切换（`_test_i18n_traditional.js`），用 `i18next` 并保留现有词条。

---

## 4. Go 后端总体设计

### 4.1 包结构草案

```
cmd/hub/                 main：参数解析、装配、优雅退出
internal/
  config/                环境变量 + 参数 + settings.json 视图
  server/                路由、中间件(鉴权/限流/CORS/请求ID/超时)、静态资源
  panelauth/             面板登录、会话、CSRF、登录限流
  gateway/
    openai/              /v1/chat/completions、/v1/models、/v1/balance-summary
    responses/           /v1/responses（含 custom/namespace 工具互转）
    anthropic/           /v1/messages 适配（默认禁用，延后）
    sse/                 SSE 读写、流式过滤器（泄漏 tool-call 过滤、DSML 回退）
    repair/              上游异常修补（orphan function_call_output、max_tokens 等）
  upstream/
    transport/           按账号/代理出口的连接池、超时、重试
    identity/            设备指纹 derive_id、出站头(X-IDE-*)、product 通道
    realm/               国际/国内出口配置、模型归属判定
  pool/
    account.go           账号状态（原子字段 + 小锁）
    picker.go            权重 > 临期积分 > 剩余用量 > 轮询；会话亲和
    breaker.go           冷却/熔断/按模型冷却/风控连坐保护
    limits.go            四条账号级护栏（保留积分/日 Token/日积分/按模型 Token）
    refresher.go         token 刷新（singleflight）、积分刷新
  store/
    accounts/            凭证 JSON 文件读写（原子写、fsync、debounce）
    settings/            settings.json（含向后兼容的迁移）
    usagedb/             SQLite：用量流水、聚合、限额事件
    sessions/            面板会话
  usage/                 记录器(异步批写)、聚合器、剩余用量估算
  pricing/               OpenRouter 价估算、策略版本化
  catalog/               模型目录、/v3/config 同步、modelsdev
  tasks/                 签到/成长/猫猫/每日活跃、webagent(ACP over SSE)
  scheduler/             整点调度
  probes/                窗口探针、额度扫描
  webtools/              本地 web_search/web_fetch（含 SSRF 防护）
  agentcfg/              （可选）一键配置客户端
  desktopimport/         （可选）桌面端凭据导入/解密（Windows 专用 build tag）
web/                     前端工程
```

### 4.2 并发模型（核心设计）

**请求生命周期（对话）**

```
client ──► server(middleware: 鉴权/Key→realm/模型限制/body上限16MB)
        ──► gateway: 解析 → 背景模型改写 → 协议转换
        ──► pool.Pick(realm, model, sessionKey, exclude)   ← 无锁读快照
        ──► limits.Check(account, model)                   ← 原子计数
        ──► upstream.Do(ctx, account, req)                 ← 账号专属 Transport
        ──► sse.Pipe(upstream → client, filters, usage tap)
        ──► usage.Record(event)  (异步 channel，批量写 SQLite)
```

关键决策：

1. **去掉全局对话信号量**。goroutine 很便宜，真正需要的是**背压**而不是硬上限：
   - 每账号设 `MaxInFlight`（默认值待 Phase 0 的上游实测定，可在设置页调）；超限时 `Pick` 跳过该账号；
   - 全局保留一个**软上限**（默认较大，如 4096）仅作内存保险，超限返回 503 + `Retry-After`；
   - 请求体 16 MB 上限保留，且**读 body 之后、拿账号之前**不持有任何锁。
2. **账号池读路径无锁**：`atomic.Pointer[[]*Account]` 存不可变快照；增删账号时复制-替换。`Account` 内热点字段（`cooldownUntil`、`inflight`、今日 token/积分计数）用 `atomic`；复杂状态（模型冷却表）用账号内的小 `sync.Mutex`，不碰全局锁。
3. **平滑加权轮询（nginx 算法）** 的状态按 `(realm, 偏好层)` 分片各持一把小锁，不再共用池锁。
4. **token 刷新用 `singleflight`**：同一账号并发请求只触发一次刷新，且**不在持任何调度锁时做网络 IO**（对应 Python 注释里"ready() 可能走网络"的坑）。
5. **全链路 `context.Context`**：客户端断连 → 取消上游请求 → 释放账号 `inflight`；超时分层（连接/首字节/空闲/总时长），沿用现有 `_test_upstream_timeouts` 的语义。
6. **SSE 管道**：`bufio.Reader` 64 KiB 块读 + 按行切分（等价于 `iter_sse_lines` 的思路，但在 Go 里是常规做法）；写端每个读块后统一 `Flush`；缓冲用 `sync.Pool`；流式过滤器（`LeakedToolCallStreamFilter`、DSML 回退、reasoning 回填）实现为 `func(chunk) []action` 的状态机，**单 goroutine 内顺序处理，无需锁**。
6b. **流式热路径优化（针对 1.2.1）**：
   - 透传优先：绝大多数 chunk 只需读 `choices[0].delta` 与 `usage`，用 `gjson`/手写扫描取字段，**默认零次完整反序列化**；只有被改写的 chunk 才重新序列化，且复用上游 `id/created`，不再硬编码 `chatcmpl-wb`；
   - 泄漏工具调用过滤器改为**增量状态机**：只维护"当前是否处于疑似标签中"的小状态，遇到 `<` 才检查后续 ≤96 字节，复杂度 O(n)，不重复扫描历史缓冲；
   - **收窄扣留范围并设置时间/长度上限**：只对"确实形如完整工具调用起始标签（带 `name=`/已知包装标签）"的文本扣留；其他情况（如 `<args>`、`<thinking>`）放行；扣留超过阈值（如 2 KB 或 300 ms）即放行并在日志里计数。具体语义以现有 `_test_*leak*`/`_test_dsml_chat_fallback` 为准，改动必须经差分测试确认不回退"模型打印工具调用"的修复；
   - TPS 口径改为：`TTFT` 取上游首个有效 chunk 的到达时间（网关内部），`生成速度` 用上游 chunk 到达时间计算，另外单独记录"网关扣留时长"，两者分开展示；
   - 上游缺 `usage` 时标记 `usage_estimated=true`，估算值不进入定价与限额统计（或单独分类显示）。
6c. **针对 1.2.2 的设计对策**：
   - **token 刷新彻底移出请求路径（A1）**：独立刷新协程，按"过期前 N 分钟 + 随机抖动"提前刷新；`singleflight` 去重；失败走**指数退避 + 熔断标记**（区分 401/网络错误/代理错误），退避期内账号只是"暂不可用"，不阻塞任何请求。`ready()` 只读原子字段，**绝不做网络 IO**。
   - **分层超时 + 快速失败（A2）**：dial ≤ 5–10 s、TLS ≤ 10 s、响应头（首字节）默认 30–45 s（可在设置里调，保持向后兼容的上限）、流中空闲超时沿用；对出口/代理维护健康度，连续失败的出口短期内直接跳过。**不做"并发对冲"同一请求到多个账号**——那会放大账号风险和积分消耗，换号仍然串行，但每一步等待上限可控。
   - **请求体只编码一次（A3）**：基础字节在重试循环外生成；账号相关的少量字段（缓存键、会话头）走头部或二次小改，gzip 结果复用。
   - **前置 CPU 降到最低（A4）**：`prompt_fingerprint` 改为对已序列化字节做一次流式 SHA（不再三遍 `sort_keys` 序列化），并在响应开始后异步计算，不挡首字节；`build_upstream_body` 尽量原地处理 `json.RawMessage`，只解析必须改写的部分。
   - **选号 O(1)（A5）**：每个 `(realm, model)` 维护"可用账号集合"，由事件（冷却开始/结束、额度达限、启停、token 状态变化）增量更新；`Pick` 只在集合上做加权轮询，不再遍历全池；`max_attempts` 直接取集合大小。5,000 账号目标 < 5 µs/次。
   - **背压前移（A7）**：先限制"在途读 body 的连接数"和"每 Key 并发"，再读 body；读 body 带超时（慢速攻击/慢客户端）；超限立即 429/503 + `Retry-After`，不排队 30 秒。
   - **统计零扫描（B1）**：写入时按"账号×模型×小时"增量更新预聚合表；面板查询只读预聚合；冷启动直接可用；旧 `usage.jsonl` 后台迁移，迁移中展示进度而不是阻塞。
   - **面板资源压缩（B2）**：前端构建产物预压缩（br/gzip）后 `go:embed`，按 `Accept-Encoding` 直接返回；JSON 接口开启流式 gzip（>1 KB）。
   - **日志与用量写入（B3）**：日志走有界异步通道 + 批量刷盘，请求线程不碰 IO；用量单写协程批量事务。
7. **用量记录异步化**：请求路径只往有界 channel 投一条事件（满则丢弃并计数 + 告警，绝不阻塞响应）；单写 goroutine 批量事务写 SQLite；同时更新内存中的滚动计数（今日 token/积分、24h 窗口），**限额护栏读内存计数而不是重扫日志**。
8. **优雅退出**：`SIGTERM` → 停止接新请求 → 等在途 SSE 到期（可配）→ flush 用量队列 → 落盘账号状态（对应 Dockerfile 里"python 为 PID 1 保证 SIGTERM"的考虑）。

### 4.3 与"每账号一台设备"口径的对应

- Python：连接池键 = `(scheme, host, port, 代理串, 凭据摘要)`；`derive_id(uid, salt)` 派生 machineId/sessionId；`X-Request-ID` 每条随机 32 hex。
- Go：`map[accountKey]*http.Transport`（懒创建、空闲回收），`derive_id` 用**同一算法逐字节一致**（MD5 + 版本/变体位修补）；移植后用 Python 版导出的一批 `(uid → id)` 向量做单测。
- 出站请求头：**Go 的 `net/http` 会规范化大小写并按字母序发送头**，Python `http.client` 保持插入顺序。若上游对头顺序/大小写敏感，需要自定义 HTTP/1.1 写出（或用 `fasthttp`/`utls`）——这是 Phase 0 必须验证的最高风险项（R1）。

### 4.4 数据流与状态落盘

| 数据 | 现状 | Go 版 |
|---|---|---|
| 账号凭证 | `accounts/*.json`，一号一文件，**明文 token** | 保持文件格式；内存为准，写入走"单写协程 + 防抖 + 临时文件 + `rename` + fsync"，token 刷新后立即落盘 |
| 设置 | `accounts/settings.json`，按 (path, mtime, size) 缓存解析 | 启动加载 + `atomic.Pointer` 持有；写入走同一落盘通道；热路径零磁盘访问；保留"旧扁平键首次读取时折入 `limits`"之类的向后兼容迁移 |
| 用量流水 | `usage/usage.jsonl` + 5 个聚合缓存文件 | **SQLite `usage.db`**（`requests` 表按时间分区索引 + 预聚合小时/日表）；首启把旧 `usage.jsonl` 后台导入，旧文件保留不删 |
| 运行日志 | 内存环 + 可选 SQLite | 保持（同一 SQLite 库的另一张表） |
| 限额事件 | `usage/limit-events.jsonl` | 表 `limit_events` |
| 面板会话 | 内存 | 表 `panel_sessions`（见第 6 节） |
| 定价 | `pricing.json`、`pricing-overrides.json`、策略时间轴 | 保持文件；策略版本化逻辑原样移植 |

> **关于是否把账号也搬进 SQLite**：本期不搬。理由：现有用户可能手改/脚本批量导入这些 JSON，文件格式是事实上的公共接口；账号写入频率低，文件方案不是瓶颈。若后续账号上万再评估。

---

## 5. 数据迁移与兼容

1. **目录与文件**：`accounts/`、`usage/`、`catalog/`、`pricing/` 路径与格式不变；Docker volume 不变。
2. **启动参数/环境变量**：保留 `--host --port --lan --api-key --panel-password --accounts-dir --usage-dir` 与全部 `WB_*`；对未实现的开关打印 *"not supported in Go build"* 而不是静默忽略。
3. **`usage.jsonl` → SQLite**：后台增量导入（记录已导入偏移），导入期间看板显示"历史数据导入中 x%"，不阻塞网关；可回滚（旧文件不动）。
4. **密码哈希**：沿用 PBKDF2-SHA256（120,000 轮，`wb_settings.py:21`）格式读取验证，**不强制用户重置密码**；改密时可升级到 argon2id 并标注版本。
5. **回退路径**：Go 版未写入任何 Python 版读不了的格式（用量除外，故 SQLite 仅为追加副本、旧 jsonl 在回退期间继续由 Go 版双写——可选开关 `WB_COMPAT_JSONL=1`，Phase 5 灰度期开启）。

---

## 6. 登录持久化方案

### 6.1 目标行为

关闭标签页/浏览器、重启网关、Docker 重建容器后，已登录设备**仍保持登录**，直到超时或被主动撤销。

### 6.2 设计

- **凭证载体**：登录成功后下发 `HttpOnly; SameSite=Lax; Path=/` 的 Cookie（HTTPS 或 `X-Forwarded-Proto: https` 时加 `Secure`）。JS 无法读取 → 不再把 token 放 `sessionStorage`。
- **服务端存储**：SQLite 表 `panel_sessions(id_hash, created_at, last_seen, expires_at, ua, ip, remember)`；Cookie 里是 32 字节随机值，库里只存 **SHA-256 哈希**（库泄漏也不能直接冒用）。
- **有效期**：默认滑动 30 天（登录页"记住我"勾选；不勾则 12 小时空闲过期）；可在设置页调。
- **CSRF**：`SameSite=Lax` + 所有写接口要求自定义头 `X-WB-CSRF`（值取自登录响应/`/api/session`，非 Cookie）；GET 只读。
- **登录限流**：保留"同源 60 秒内 5 次"并改成令牌桶 + 指数退避；限流键沿用 `login_rate_limit_key` 的反代判定逻辑（`wb_proxy.py:389` 附近）。
- **设备管理**：设置页新增"已登录设备"列表（UA/IP/最后活跃），可单个撤销、"撤销其他全部"；改密自动撤销其他会话（保持现有行为）。
- **首次/默认密码**：沿用 `admin` 默认值的提示逻辑，但**默认密码时强制先改密才能进入面板**（现状只是提示）——这是安全加固，属于你可以否决的建议项。
- **兼容**：`X-Panel-Token` 头保留一个版本（API 脚本可能在用），内部映射到同一会话表；`?pwd=` 默认关闭，开关 `WB_ALLOW_QUERY_PWD=1` 才启用，且首次使用后 302 去掉参数。

### 6.3 前端侧

- 启动时请求 `GET /api/session`：200 → 直接进应用；401 → 登录页。不再有"先渲染再发现没登录"的闪屏。
- 401 统一拦截：只清本地状态并跳登录，不重试风暴。
- 非敏感偏好（主题/语言/排序列/列布局）继续 `localStorage`；**跨设备要保留的偏好**（如已有的 `accounts_collapsed`）存服务端用户偏好表。

### 6.4 （可选）桌面端解码密钥持久化

现状只存进程内存。可选方案：用系统密钥环（Windows DPAPI / macOS Keychain / Linux libsecret，不可用时回落到由面板密码派生密钥加密的文件）加密落盘。**默认关闭、需用户在设置里显式开启**。待确认（第 15 节）。

---

## 7. 前端重构方案

### 7.1 信息架构

保持现有五个主页签（网关 / 账号 / 数据看板 / 智能体配置 / 设置 / 运行日志），**账号页默认独立**（取消"账号独立成页签"这个开关，它本来就是规模问题的补丁）。按路由做代码分割，首屏只加载当前页。

### 7.2 账号页（重点）

| 现状 | 新方案 |
|---|---|
| 一次拉全量账号，整表重绘 | `GET /api/accounts?realm=&status=&q=&sort=&cursor=&limit=`：**服务端分页/游标**；表格用 TanStack Virtual 只渲染可视行（~30 行 DOM，与总量无关） |
| 15 秒全量轮询 | 初始分页拉取 + `GET /api/events`（SSE）订阅：服务端推 `account.patch`（只含变化字段）；客户端按 `uid` 行级更新，未变行不重渲 |
| 筛选/排序在前端 | 全部在服务端（索引字段：状态、区域、权重、备注、积分区间、最近错误），搜索支持 uid 前 8 位/昵称/备注 |
| 逐个按钮操作 | **多选 + 批量操作栏**（启停、设权重、绑代理槽、刷新凭证/积分、签到、删除、导出）；批量走 `POST /api/jobs` 创建后台任务，页面显示进度条，可取消，失败项可重试 |
| 状态汇总靠遍历数组 | 独立轻量接口 `GET /api/accounts/summary`（各状态计数，服务端维护计数器，O(1)） |
| 行里塞一堆操作按钮 | 行内只留状态与关键指标；操作收进行菜单和详情抽屉（抽屉打开时才请求该账号详情：模型冷却、积分包、近期活动） |
| 每行实时用量 | 用量列由 SSE 增量推送，且只对"可视行"订阅（`subscribe: [uid...]`），不可视行不推 |

### 7.3 数据层与轮询

- TanStack Query：请求去重、缓存、`staleTime`、失败退避；
- **全局只保留一条 SSE 连接**（`/api/events`），服务端按事件类型与订阅范围合并后每 ~1 秒批量下发；断线自动重连并用 `Last-Event-ID` 补发；
- 页面不可见（`document.hidden`）时暂停订阅，回到前台一次性补拉（现有逻辑的意图是对的，保留）；
- 数据看板图表：服务端按所选区间预聚合（小时/日桶），前端只拿桶数据；大表（请求流水）用游标分页 + 虚拟滚动。

### 7.4 质量要求

- 5,000 账号 / 100,000 条请求流水的合成数据集作为前端性能回归基线（Playwright + trace，CI 里设阈值）；
- 无障碍与移动端：保留现有"手机布局"需求（`tests/_mobile_check.py` 的断言迁移成 Playwright 用例）；
- 主题：保留 claude 风格主题切换与深浅色。

---

## 8. API 契约

| 类别 | 路径 | 策略 |
|---|---|---|
| **对外网关（冻结）** | `/v1/chat/completions`、`/v1/responses`、`/v1/models`、`/v1/balance-summary`、`/u/<ref>/v1/*`、`/health`、`/v1/messages*`（默认禁用） | 请求/响应/错误体/状态码/头与旧版保持一致，是差分测试的主对象 |
| **外部脚本依赖** | `/agents/cli`（下发 `wb_agents.py`）、OpenWrt 的 `workbuddy2api-warm` 预热脚本访问的面板接口 | 暂时保留旧路径；实现后再评估是否迁移 |
| **面板管理 API** | 现有 ~89 处路径判断（`/accounts/*`、`/usage/*`、`/settings/*` 等，POST 居多） | **重新设计为 `/api/v2/*` RESTful + 分页/游标 + 统一错误结构**；旧路径在 Go 版里不实现（前端同步重写，无外部契约）；若发现有第三方在用，加一层薄兼容 shim |
| **实时事件** | `GET /api/events`（SSE） | 新增 |
| **运维** | `/metrics`、`/debug/pprof/*` | 新增，需鉴权 |

OpenAPI 文档由 Go 侧注解生成，前端 TypeScript 类型由其自动生成，避免前后端漂移。

---

## 9. 功能清单分级与迁移顺序

| 级别 | 模块（Python → Go 包） | 说明 |
|---|---|---|
| **P0 核心（Phase 1–2）** | 账号池与调度 `wb_accounts` → `pool`；上游传输与身份 `wb_upstream_pool/wb_identity/wb_fingerprint` → `upstream`；Chat Completions 链路 + SSE；`/v1/models` 与目录 `wb_catalog/wb_modelsdev`；API Key 管理/出口绑定/模型限制；用量记录 `wb_logstore/record_usage` | 没有它们网关不能用 |
| **P0 面板（Phase 3）** | 面板鉴权（第 6 节）、账号 CRUD、OAuth 登录流程（`/accounts/login/*`）、基础统计、设置 | 与新前端同步交付 |
| **P1（Phase 4）** | Responses API（含 custom/namespace 工具互转）；限额护栏（四条）；模型冷却/熔断/风控连坐保护；token 与积分刷新；剩余用量估算 `wb_quota_scan`、`/usage/remaining`；OpenRouter 价估算 `wb_pricing`；后台模型重定向；整点调度器 `wb_scheduler`；国内版成长/签到/猫猫、国际版每日活跃 `wb_tasks/wb_activity/wb_webagent` | 体量大但边界清晰 |
| **P2（Phase 5）** | Anthropic Messages 适配（默认禁用）；本地网络工具 `wb_webtools`（需完整 SSRF 防护）；窗口探针 `wb_window_probe`；日志持久化；代理槽 `wb_egress/wb_ipintel` | 使用面窄 |
| **P3 可延后/独立** | 桌面端凭据导入与 at-rest 解密 `wb_atrest`（Windows `ReadProcessMemory`，用 build tag 隔离）；智能体一键配置 `wb_agents`（建议做成**独立 Go CLI**，网关只保留 `/agents/cli` 下发）；自更新 `wb_updates`（Go 单二进制 + Docker 后可简化为"检查更新 + 提示"） | 与并发/规模问题无关，不阻塞替换 |

**建议削减项（需你拍板）**：`console_tail.py` 与多套 `.bat/.command/.sh` 启动脚本（二进制自带 `--lan` 即可）；`ops/aliyun` 自更新器；便携版内置 Python 运行时打包链路（不再需要）。

---

## 10. 分阶段里程碑

> 估时按"1 名开发者 + AI 协同"给出的**相对量级**，不是承诺；Phase 0 结束后会按实测重估。

### Phase 0：基线与可行性验证（约 1–1.5 周）

- 搭 **mock 上游**（Go 写，复用，后面差分测试也用它）：可配置延迟、SSE 分块节奏、429/403/WAF 形态、凭证校验。
- 在 Python 版上测基线：并发流数 vs 吞吐/TTFT/RSS/CPU，找到 32 并发与线程数的拐点。输出 `baseline.md`。
- **关键 Spike（任一失败都要改方案）**：
  1. **出站指纹对齐**：Go `net/http` 的 TLS（JA3/JA4）、HTTP/1.1 头顺序/大小写、是否协商 h2，与 Python 版出站是否一致？用抓包对比（Wireshark / `tlsfingerprint` 服务）。必要时评估 `utls` / 自定义 HTTP/1.1 写出。→ **用一个测试账号对真实上游做小流量验证后再继续**。
  2. SQLite（`modernc`）写入吞吐：单写协程批量，目标 ≥ 5k 行/秒；
  3. `modernc.org/sqlite` 在 OpenWrt 目标架构（mipsel/arm/aarch64/x86_64）上的可用性与体积。
- 补充基线（体感类问题，对应 1.2.2）：在 Python 版上用 mock 上游复现 ① token 临近过期时的首包延迟分布 ② 注入"连接挂起不拒绝"的账号时的换号耗时 ③ 1k/5k 账号下的选号开销 ④ 3 MB 会话的前置耗时 ⑤ 20 万/100 万行日志下统计接口冷启动耗时；这些数字就是 Go 版验收的对照基线。
- 补充基线（流式 TPS）：把 `/c/wb2a/bench` 里的 chunk 微基准扩展成端到端测试——mock 上游按固定速率（如 50 tok/s）出 token，在 1/10/100/500 并发下测每条流的**实际下行 token 速率**、TTFT、扣留导致的"卡顿时长"，以及含 `<thinking>` 等触发标签时的退化曲线。
- 产出：技术选型最终确认、风险登记更新。
- **验收**：基线报告 + 指纹对齐结论（通过/需 utls）+ 目标指标校准。

### Phase 1：Go 骨架 + 数据层（约 1.5 周）

- 工程脚手架、配置、日志、优雅退出、CI（`go test -race`、`golangci-lint`、交叉编译矩阵）。
- 账号文件读写（原子写）、`Account` 模型字段 1:1 移植（含 `disabledModels`、`weight`、`note`、`proxySlot`、`product`、`enterpriseId` 等）、`settings.json` 读取与向后兼容迁移。
- `derive_id` 等指纹函数 + 黄金向量测试。
- **验收**：能读取现网 `accounts/` 与 `settings.json`，往返写入后 Python 版仍能正常加载（字节差异仅限允许的格式化）。

### Phase 2：网关热路径（约 3 周）

- 账号池：快照 + 平滑加权轮询（权重 > 临期积分 > 剩余用量 > 轮询）、会话亲和、`exclude`、`ready(model)`、冷却/熔断。
- 上游传输：按账号 Transport、重试策略、超时分层、代理（HTTP/SOCKS 以实际 Python 支持范围为准——Python 只支持 HTTP 代理，Go 可顺带支持 SOCKS5，但作为"增强项"需标注）、TLS 校验错误提示（`WB_CA_BUNDLE`）。
- `/v1/chat/completions`（流式+非流式）、`/v1/models`、`/v1/balance-summary`、`/health`；SSE 管道与流式过滤器；上游异常修补；API Key 鉴权与出口绑定/模型限制。
- 用量记录（异步写 SQLite）+ 内存滚动计数。
- **验收**：对 mock 上游，Python/Go 差分测试在 chat 链路上全绿；2,000 并发流压测达标（第 12 节）。

### Phase 3：面板后端 + 新前端（约 3–4 周，可与 Phase 2 后半段并行）

- 面板认证（第 6 节）、`/api/v2` 账号/设置/统计接口、SSE 事件总线、批量任务（jobs）。
- 前端：登录、账号页（虚拟滚动+批量）、网关页、数据看板、设置、日志；i18n、主题、移动端。
- 用量聚合：SQLite 预聚合表 + 查询接口，取代"增量扫描 + 偏移缓存"。
- **验收**：5,000 合成账号的前端性能基线达标；重启网关/关浏览器后仍登录。

### Phase 4：P1 功能补全（约 3–4 周）

- Responses API 及工具互转、四条限额护栏、剩余用量估算、价估算与策略时间轴、后台模型重定向、整点调度器与全部自动化任务、token/积分刷新器、OAuth 流程。
- **验收**：对应 Python 测试套件的行为用例全部迁移并通过（映射表见 11.2）。

### Phase 5：P2/P3 + 灰度（约 2–3 周）

- Anthropic 适配、web tools、窗口探针、桌面导入（Windows）、Docker/Windows/OpenWrt 打包、迁移工具与回滚演练。
- **灰度**：先让 Go 版作为**旁路只读影子**（接同一份流量副本到 mock/测试账号，对比输出），再在自己的环境小流量切换，开启 `WB_COMPAT_JSONL=1` 双写保证可回退。
- **验收**：两周内无 P0/P1 缺陷，用量统计与 Python 版同一时段的口径偏差 < 0.1%（偏差来源需逐条解释）。

### Phase 6：切换与收尾（约 1 周）

- 发布 `v2.0.0`；Python 版转入"仅安全修复"分支；文档（迁移指南、环境变量对照表、升级/回滚手册）。

**总估时**：约 **15–19 周**（含缓冲）。若砍掉 P3 与部分 P2（第 9 节建议削减项），可压到约 12–14 周。

---

## 11. 测试与一致性验证

### 11.1 三层策略

1. **单元/表驱动测试（Go）**：协议转换、过滤器状态机、调度算法（含平滑加权轮询的序列断言）、限额与 0 点解封、定价策略。`go test -race` 必开。
2. **黑盒契约套件（语言无关）**：把现有 Python 测试里"起服务 → 发 HTTP → 断言响应"的那一类（如 `_test_request_contract`、`_test_anthropic_http`、`_test_panel_route_auth`、`_test_upstream_timeouts` 等），抽成**对任意实现都能跑的 HTTP 用例**，同时对 Python 与 Go 两个实现运行，输出差异报告。
3. **差分/影子测试**：同一份请求序列分别打到 Python 与 Go（背后是同一个 mock 上游），逐条比对状态码、头、SSE 事件序列、用量记录。

### 11.2 现有测试资产的处理

- 127 个 Python 套件按模块归类，先建**"行为清单 → 对应 Go 测试"映射表**（Phase 0 产出，约 200 行 CSV），每迁一个模块勾一项，避免"漏掉某个历史修补"。
- 45 个 JS 套件：与旧 DOM 紧耦合，**不迁移**；其断言意图转写成前端组件测试（Vitest）和 Playwright 用例。
- `tests/fixtures/pricing_tooltip/*` 等大体量夹具原样复用，作为定价与用量聚合的黄金数据（Go 输出须与夹具一致）。
- `docs/CHANGELOG.md`（191 KB）是"为什么这里有个奇怪分支"的证据库，移植每个模块时逐条检索相关条目，把边界条件转成测试。

### 11.3 质量门禁

`go vet` + `staticcheck` + `-race` + 覆盖率（热路径包 ≥ 70%）+ 模糊测试（SSE 解析器、JSON 修补、工具互转）+ 前端 Lighthouse/Playwright 预算。

---

## 12. 性能基准与压测方案

- **工具**：自写 Go 压测器（可控 SSE 慢速消费者）+ `k6`/`vegeta` 做对照。
- **场景**：
  1. 1/10/100/500/2000/5000 并发流式对话，上游 mock 每流 200–2000 个小 chunk、总时长 5–60 秒；
  2. 混合 5% 非流式、5% 大 body（接近 16 MB）；
  3. 上游 429/超时/断流注入（验证重试、冷却、释放 inflight 不泄漏）；
  4. 5,000 账号下 `Pick` 微基准（目标 < 5 µs/次）；
  5. 慢客户端（读得慢）占用连接，验证内存有界。
- **指标**：吞吐、TTFT 增量 P50/P99、RSS、goroutine 数、GC 暂停、fd 数、SQLite 写队列深度。
- **回归**：基准结果入库，CI 上每次发版比较，退化 > 10% 失败。

---

## 13. 构建、发布与部署

- **产物**：`hub-{linux,windows,darwin}-{amd64,arm64}`，另加 OpenWrt 目标；前端 `go:embed`；`-trimpath -ldflags "-s -w"`。
- **Docker**：`FROM scratch` 或 distroless + CA 证书；沿用 `HEALTHCHECK /health`、`PUID/PGID`、`./accounts` 与 `./usage` 挂载；多架构镜像继续发 GHCR 与 Docker Hub。
- **Windows**：单个 `.exe` + 双击即运行（可选系统托盘作为后续增强）；保留防火墙助手脚本。
- **OpenWrt**：沿用 `wrt/` 的打包脚本思路，改为打包 Go 二进制；验证内存占用。
- **升级**：`quick-deploy.sh` 与 Watchtower 流程不变；因为会话已持久化，升级不再把用户踢下线。
- **CI**：沿用现有 `tests.yml` / `release.yml` 的结构，新增 Go 矩阵、压测任务（手动触发）、前端构建缓存。

---

## 14. 风险登记表

| # | 风险 | 影响 | 概率 | 缓解 |
|---|---|---|---|---|
| R1 | **Go 出站 TLS/HTTP 指纹与 Python 版不同**（JA3、头顺序/大小写、h2 协商），上游 WAF/风控行为变化，账号被限流或封禁 | 高 | 中 | Phase 0 抓包对比 + 测试账号小流量验证；必要时 `utls` + 自定义头序；上线前不对生产账号放量；保留一键回退到 Python |
| R2 | 行为长尾丢失：21k 行里的历史修补（工具调用泄漏、DSML 回退、orphan output、reasoning 回填等）漏移植 | 高 | 高 | 11.2 行为清单映射；差分测试；逐模块对照 CHANGELOG |
| R3 | 上游协议在重构期间变化（CHANGELOG 达 191 KB，迭代很快） | 中 | 高 | Python 版冻结功能只做修复，每次修复登记到"待同步清单"，每个 Phase 结束合并一次 |
| R4 | 用量口径漂移（聚合从"增量扫描"改为 SQL 预聚合，边界处理如本地时区 0 点、窗口起点、闭合窗口桶） | 中 | 中 | 复用 `tests/fixtures/pricing_tooltip/*`、`_test_closed_window_buckets` 等黄金数据；双写对账 |
| R5 | 前端重写工作量被低估（15k 行 UI 含大量细节：悬停定价链、透视表、热力图、排序记忆…） | 中 | 高 | 先做账号页与网关页（用户痛点），其他页按"功能对等清单"逐页迁，允许 Phase 3 末期仍有页面走"旧页面 iframe 兜底"（仅过渡期） |
| R6 | 纯 Go SQLite 在极小设备（OpenWrt）上体积/性能不达标 | 中 | 低 | Phase 0 验证；退路是用量用追加文件 + 内存聚合 |
| R7 | 明文 token 存盘的既有安全面（任何读到 `accounts/` 的人拿到全部凭证） | 中 | — | 本期保持兼容；提供可选加密落盘（6.4）与文件权限 0600 |
| R8 | 范围蔓延 | 高 | 高 | 功能冻结 + P0–P3 分级 + 每阶段验收门 |
| R9 | **合规**：本项目对接的是第三方（腾讯）服务的非公开接口，含设备指纹派生、自动签到等；其服务条款可能不允许 | — | — | 本计划只做 1:1 行为移植，不新增访问面或规避能力；使用风险由使用者按项目免责声明自行承担 |

---

## 15. 需要你确认的决策点

| # | 决策 | 我的默认建议 |
|---|---|---|
| D1 | 前端框架 | React + TanStack（备选 Vue 3） |
| D2 | 新代码放哪里 | 同仓库新分支 `v2-go`，Go 代码根目录直接放（Python 移到 `legacy/`），而不是新建仓库——便于对照与共享测试夹具 |
| D3 | 功能范围 | 按第 9 节分级；P3（桌面导入/智能体配置/自更新）是否本次一并重构，还是首版放弃 |
| D4 | 面板管理 API 是否允许破坏性重设计 | 是（前端同步重写，仅 `/v1/*` 冻结） |
| D5 | 默认密码是否强制首次改密 | 是 |
| D6 | 用量存储是否切 SQLite | 是；保留 `WB_COMPAT_JSONL` 双写过渡 |
| D7 | 是否做解码密钥/凭证的加密落盘 | 作为可选项，默认关闭 |
| D8 | 是否有真实的测试账号与可承受的小流量用于 Phase 0 指纹验证 | 需要（没有的话 R1 无法闭环） |
| D9 | 你实际的账号规模、峰值并发、部署形态（Docker/Windows/OpenWrt） | 用来校准第 2.1 节指标与优先级 |

---

## 16. 下一步（确认后立即执行）

1. 建分支与目录骨架，落 `docs/baseline.md` 模板与行为映射表 CSV 骨架；
2. 写 mock 上游与 Python 基线压测脚本，出基线数据；
3. 做指纹对齐 Spike 并汇报结论；
4. 依据结论更新本计划的第 3、4、10 节，再进入 Phase 1。

---

## 附录 A：Python 模块 → Go 包映射

| Python | 行数 | Go 包 | 备注 |
|---|---|---|---|
| `wb_proxy.py` | 21,001 | `server` `gateway/*` `usage` `panelauth` 等拆分 | 拆分是重构的主要工作；按路由与职责切 |
| `wb_accounts.py` | 4,239 | `pool` `store/accounts` `upstream` | 含 `Account`、`AccountPool`、`SessionAffinity`、`CreditsRefresher` |
| `wb_pricing.py` | 2,556 | `pricing` | 策略版本化、变体后缀继承、汇率 |
| `wb_settings.py` | 2,238 | `store/settings` `panelauth` | 含 `PanelSessions`、PBKDF2 |
| `wb_catalog.py` | 1,731 | `catalog` | `/v3/config` 同步、桌面缓存回落 |
| `wb_agents.py` | 1,473 | `agentcfg`（独立 CLI） | 文本级 YAML/TOML/JSON 编辑器 |
| `wb_atrest.py` | 915 | `desktopimport`（windows build tag） | AES-256-GCM + `ReadProcessMemory` |
| `wb_tasks.py` | 883 | `tasks` | 成长/签到/猫猫 |
| `wb_upstream_pool.py` | 852 | `upstream/transport` | 由 `http.Transport` 取代大半 |
| `wb_webtools.py` | 1,099 | `webtools` | SSRF 防护必须完整 |
| `wb_identity.py` `wb_fingerprint.py` | 545 / 80 | `upstream/identity` | 向量测试 |
| `wb_logstore.py` | 415 | `usagedb` `logstore` | 已是 SQLite 单写线程模型，可直接对应 |
| `wb_quota_scan.py` `wb_window_probe.py` `wb_probes.py` | 421 / 407 / 120 | `probes` | |
| `wb_scheduler.py` `wb_activity.py` | 376 / 386 | `scheduler` `tasks` | |
| `wb_webagent.py` | 282 | `tasks/webagent` | ACP over HTTP+SSE |
| `wb_modelsdev.py` `wb_prompt.py` `wb_egress.py` `wb_ipintel.py` | 245 / 127 / 86 / 107 | 对应小包 | |
| `wb_updates.py` `wb_update_request.py` | 369 / 140 | `updates`（精简） | 视 D3 |
| `dashboard.html` | 15,373 | `web/`（SPA） | 重写，不移植 |
