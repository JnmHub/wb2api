# 进度记录（v2-go 分支）

对应 [REFACTOR_PLAN.md](REFACTOR_PLAN.md)。本文只记**已经做了什么、验证到什么程度、哪里还没做**。

## 已完成（均有测试）

| 包 | 内容 | 验证方式 |
|---|---|---|
| `internal/fingerprint` | `derive_id` / machineId / sessionId / requestId | 与旧 Python 逐字节对拍的黄金向量（28 条，含中文/空 uid） |
| `internal/realm` | JWT 声明解析、区域判定、域名对齐、`normalize_epoch`、禁用模型归一、`detect_model_realm`、`WB_CN_CHAT_UPSTREAM` | 黄金向量（60+13+6+7+36 条，含乱码 token） |
| `internal/store/accounts` | 账号文件读/写（原子写、保留未知字段、旧字段默认值/裁剪规则 1:1） | ① 读 Python 写出的目录，字段全部对上 ② Go 写出的目录，旧 Python 加载器能读（`tools/crosscheck/py_load.py`） |
| `internal/store/settings` | `settings.json`（快照缓存、原子更新）、面板密码 PBKDF2、API Key 列表与 fnmatch 模型限制 | PBKDF2/fnmatch/API Key 黄金向量；旧哈希能验证；并发更新不丢写 |
| `internal/gateway/leak` | 工具调用泄漏的解析（`parse_leaked_tool_calls` / DSML）与流式过滤器 | 400 条随机拼接语料 × 2 种分块与旧 Python **逐步对拍**（Legacy 模式）；新增 Precise 模式 |
| `internal/pool` | 账号池：不可变快照、原子就绪判断（永不做网络 IO）、轮转选号、按账号并发上限 | 单测 + 并发测试；基准见下 |
| `internal/upstream` | 每账号独立的 `http.Transport`（HTTP/1.1，分层超时） | 测试：两个账号绝不共用同一条 TCP 连接 |
| `internal/gateway/openai` | `/v1/chat/completions` 流式+非流式、鉴权/Key 绑定/模型限制、请求体整形 + 出站身份头 + gzip（CLI/VSCode）+ CLI 专有字段、401 刷新后重试同号、403 风控/内容审核分流（含降级提示词重试）、429/402/5xx 换号、SSE 管道、用量事件 | 14 个行为测试 + 11 个出站/风控测试 |
| `internal/catalog` | **模型目录**：内嵌国际/国内两张静态表；`/v3/config` 同步（桌面 UA 双尝试、10 s 超时、`agents[].models` + `models[]` 元数据解析）、整理（去虚拟别名/`lite`/`codewise-`/`-image-alpha*`/`-sg`/`-x`、有免费 `x0.00` 兄弟时去掉付费版）、与静态表合并（reasoning 字段级合并、`hy4-preview-f` 替换、无倍率新模型不外露）、5 分钟缓存 + 单飞、手动刷新 30 s 冷却、每日兜底、桌面缓存文件回退、`/v3/config` 响应归档；`/v1/models` 的富条目（各客户端约定的能力字段全部输出）；上下文/输出上限四级查找（上游 > 内建知识表 > `model.json` > models.dev 异步预热）；`output_probes.json` 钳制标注；同时实现 `shape.Catalog`（缺省 `max_tokens` 与 DeepSeek 默认思考档位现在与旧版一致） | **黄金向量**（`tools/genvectors/gen_catalog.py`，共 200 余条）：`is_chat_model`、`parse_remote_catalog`（10 种载荷）、`curate_remote_catalog`（9 条）、`merge_catalog`（19 条，含 `all`/extras/`hy4-preview-f`）、`model_entry`（70 条，逐字节）、上限查找、models.dev 索引构建、目录默认值（67 个模型）全部与 Python 逐项一致 |
| `internal/gateway/openai`（信息端点） | `/v1/models`、`/models`、`/v1/balance-summary`（只读缓存快照、不识别账号、未知余额为 null 而不是 0）、`/health`（永远应答；Key 通过后才附账号身份） | 11 个 HTTP 测试（含模拟 `/v3/config`、缓存命中次数、Key 绑定区域优先于 `X-Realm`、失败刷新保留旧清单、手动刷新冷却） |
| `internal/gateway/responses` + `openai.Responses` | **Responses API（`/v1/responses`）**：请求→chat 转换（`input` 各类条目、`instructions`/developer、reasoning 回填、多模态、function/custom 工具调用及输出、`agent_message`、孤儿输出、`additional_tools`、namespace 工具展开与回程还原、custom 工具降级为 function 再还原为 `custom_tool_call`、`text.format`、`reasoning.effort`）、整段与流式事件翻译（reasoning/文本/工具调用事件序、DSML 文本防泄漏缓冲、半截工具参数丢弃、缺 usage 时估算）、`previous_response_id` 明确拒绝；与 chat 共用账号选择/换号/身份/错误处理/用量记账 | **257 条黄金向量**（`tools/genvectors/gen_responses.py`）：56 条请求转换 + 15 条整段响应 + 186 条流（含 160 条针对 DSML 缓冲的随机模糊）与 Python **逐字节一致**；7 个端点集成测试 + 真实二进制端到端（hub→mock 上游：流式/非流式、用量入库） |
| `internal/jsonx` | 保序 JSON（对象保持插入顺序、Python `json.dumps(separators=(",",":"), ensure_ascii=False)` 的逐字节输出） | 单测 + 被 shape 的 325 条对拍间接覆盖 |
| `internal/gateway/shape` | 旧版 `build_upstream_body` 全套请求体整形：角色归一、提示词模式、指纹清洗、reasoning 回填、工具调用配对修复（合并/重排/清孤儿）、工具 schema 归一、`tool_choice`/`stop`/`max_tokens` 归一、DeepSeek 思考注入、`stream_options` | **325 条**（手写 + 随机）与 Python 输出**逐字节一致**，并断言不改动入参 |
| `internal/upstream/identity` | 三套出站身份（桌面端/VSCode/CLI）的头族、UUIDv7、会话 id 派生、trace 头、设备 token 规则 | **192 种**组合与 Python 对拍：头顺序/拼写/稳定值逐一相同，随机值形态相同 |
| `internal/upstream/wire` | 自写 HTTP/1.1 客户端：线上字节顺序与拼写完全可控（与旧版 `http.client` 同一形状）、按账号独立连接池、过期连接安全重试、取消即断开、CONNECT 代理 | 10 个测试，含原始字节断言 |
| `internal/refresh` | token 后台刷新：提前量+抖动、有界并发、同账号单飞、失败指数退避、401/无 token 标记需重新登录 | 9 个测试；`Pick` 在刷新进行中不被阻塞 |
| `internal/risk` | 风控连坐保护：11140 识别、账号停放、窗口内多账号受限则整区域暂停 | 单测 |
| `internal/panelauth` | **面板登录持久化**：HttpOnly+SameSite=Lax Cookie（HTTPS/可信反代下加 Secure）、服务端会话落盘且只存令牌哈希、滑动续期（记住我 30 天 / 否则 12 小时空闲）、CSRF 头、JSON-only 写请求、登录限流（可信反代下按真实客户端 IP 分桶，伪造头无效）、已登录设备列表/撤销、改密撤销其他设备、默认密码强制先改 | 14 个测试 + **真实二进制重启验证**：登录→杀进程→重启→同一 Cookie 仍是 200 |
| `internal/usage` | 用量块解析（`prompt/completion/reasoning/cached/total/credit`，缓存命中取各别名里第一个正值） | 单测 |
| `internal/usagedb` | **SQLite 用量库**：异步批写（请求路径只入队，队满丢弃并计数，绝不阻塞）、写入时增量维护日/时预聚合、`usage.jsonl` 断点续导、按范围/模型/账号/Key 聚合、按小时/天序列、游标分页的请求流水、当日计数（限额护栏的输入） | 与 Python 对 496 行样本日志**独立算术**的合计/分模型/分账号逐项一致；预聚合与精确扫描两条路径互相校验；断点续导、半行不导、并发读写、队满丢弃均有测试 |
| `internal/api` | `/api/v2/usage/{summary,series,recent}`（`today/week/month/all/custom`，周一为一周起点，均走面板鉴权） | 8 个测试 |
| `internal/events` | SSE 事件总线：发布永不阻塞、慢消费者丢事件后收到 `resync`、`Last-Event-ID` 重放、心跳、按键合并（`Coalescer`） | 5 个测试 |
| `internal/jobs` | 后台批量任务：有界并发、进度/取消、单项失败（含 panic）不拖垮整批、失败清单有上限 | 6 个测试 |
| `internal/api/accounts` | **账号管理 API**：服务端分页/过滤/排序（状态、区域、搜索 uid/昵称/备注、今日 token/请求数…）、汇总、详情、PATCH（校验+落盘）、删除、**批量任务**（启停/权重/备注/代理槽/身份/清冷却/清风控/删除/刷新 token；可按"当前筛选结果全部"而不只是可见页；破坏性操作须 `confirm`）、变更经事件总线合并推送 | 16 个测试；**5000 账号下"按状态过滤+按名排序取 50 条"单次请求 1.6 ms**；真实二进制端到端：2000 账号分页/批量禁用 1000 个/SSE 推送/CSRF/落盘 |
| `internal/webui` | 内嵌面板的静态服务：构建期预压缩（br/gzip）直接下发、哈希资源 `immutable` 长缓存、`index.html` 用 ETag 重验证（回访 304）、SPA 路由回退、`/api` `/v1` 未知路径返回 JSON 404 而不是 HTML、CSP 等安全头、路径穿越防护、前端未构建时的提示页 | 9 个测试 |
| `web/`（Vite + React + TS + TanStack Query/Virtual） | **新面板**：登录（含"记住我"、强制改默认密码）、**虚拟滚动账号表**（按需分页取数、随机跳转只取目标页）、服务端搜索/筛选/排序、状态汇总、多选 +"选择全部符合筛选的账号"批量栏（启停/权重/备注/代理槽/身份/刷新 Token/刷新积分/测试/签到/每日活跃/清冷却/解除风控/删除）、后台任务进度、详情抽屉（编辑/机器码/冷却/刷新状态/账号操作与测试结果）、SSE 实时推送行级更新、用量页（范围/区域、KPI、柱状图、分模型/账号/Key/区域表）；页面隐藏 15 秒后自动断开事件流；本轮补全**设置页**（出口/模型清单/Key/限额与调度偏好/开关/上游与提示词/代理槽）、**日志页**（级别/模块/搜索/暂停/跟随/导出/清空）、**任务页**（调度器时间表、立即巡检、积分刷新状态、签到与活跃历史）、**添加账号**（浏览器登录 + 轮询）、**导入（预演报告）/导出**弹窗，`App.tsx` 五个标签的 hash 路由 | 21 个 vitest（含日志/任务/设置页渲染冒烟）；`tsc` 严格模式零错误；真实二进制下登录/设置/日志/调度/导入导出/网关/SPA 全部跑通（见下） |
| `cmd/hub` `cmd/mockup` `cmd/loadtest` | 网关、模拟上游、压测器 | 手动跑通；**端到端实测**：200 个请求→用量库恰好 200 行、8000 completion tokens、TPS≈192（模拟上游 200/s），重启后同一 Cookie、同一数据 |
| `internal/gateway/ratelimit` + `openai`（失败治理） | **账号失败治理阶梯**：软限流阶梯（600 s→2 h）、429 按作用域分流（账号级 / 模型级 / 6004 额度窗口落盘 `model_caps`）、402 停到本地次日 04:00、5xx 与客户端断连熔断（30 min→6 h）、本机断网不扣账号、TLS 校验失败短路、出口级秒级退避、429 时自动切换出站身份；同一次请求里同一账号最多记一次失败，最终错误优先级与旧版一致 | 6 个 ratelimit 单测 + 11 个网关行为测试 |
| `internal/guards` + `pool`（调度偏好） | 手动权重、临期积分优先、会话亲和（客户端会话键或对话开头派生）、超长对话分页轮转、保留积分线与每日 Token / 积分 / 单模型限额护栏 | 单测 + 网关集成测试 |
| `internal/gateway/anthropic` | **Anthropic Messages**：`/v1/messages`、`/v1/messages/count_tokens`（请求体互转、SSE 事件翻译），默认关闭 | 4 个测试；与 Python 对拍向量逐字节一致 |
| `internal/ops` | 账号操作：积分刷新（国内详细 / 国际回退 / 企业）、国内签到与活跃上报、国际每日活跃对话、浏览器（OAuth device）登录、昵称同步、账号测试、导入（含预演）/导出 | 9 个测试（模拟上游） |
| `internal/scheduler` | 定时调度器：签到 / 活跃 / Token 保活按整点执行、账号间 2–5 s 错峰、跳过风控账号与暂停区域；积分后台刷新每次只挑一个最久未更新的账号 | 6 个测试（模拟上游） |
| `internal/activity` `internal/logbuf` | 签到/活跃历史（JSONL，90 天 / 5000 条裁剪）与内存日志环（级别/模块/文本筛选、游标追加、导出） | 各 1–2 个测试 |
| `internal/api`（设置/Key/代理槽/日志/运维） | 设置快照与 PATCH（出口、开关、限额、超时、提示词、积分刷新间隔）、API Key 增删改与一次性展示、代理槽、日志查询/清空/导出、活动历史、调度器状态/时间表/立即巡检、导入/导出、单账号操作、模型清单刷新；`auth_disabled` 现在真正关闭鉴权 | 22 个 API 测试 |
| `internal/realmswitch` | 面板出口（国际/国内）持久化并热切换 | 单测 |
| 免费模型积分回退（`openai`） | 国际版 `deepseek-v4.1-flash` 的 24h 窗口在**所有**可用账号上都用尽（落盘的 6004 窗口）后，自动改打付费同款 `deepseek-v4.1-flash-sg` 一次；开关 `free_model_credit_fallback`（默认关）或 `WB_ENABLE_FREE_MODEL_CREDIT_FALLBACK`，用量记在付费模型上，不再二次回退 | 2 个网关测试（回退成功、开关关闭不回退） |
| `internal/quota`（额度窗口探针） | 429 带重置时刻时，把「账号 × 模型」窗口落盘到 `accounts/model_windows.json`；到点发一个最小请求（`max_tokens=16`、`reasoning_effort=none`）：通了就清该模型冷却，仍 429 且给了新时刻就重排，否则 15 s 后重试、最多 20 次；设置页 `window_probe` 开关（默认开），调度器每 30 s 检查且不受定时任务开关影响 | 7 个单测 + 1 个网关测试 |
| `internal/ops`（国内成长任务） | 成长中心：任务清单/汇总、自动接取、按任务类型生成最小事件（canvas/template/expert/team/skill/automation/playbook/skin/chat/glm/cat）、完成后领奖、不可伪造任务与仅桌面任务跳过；猫猫旅行（状态/领奖/派出、无 Buddy 时先补 `first_buddy`）；连登管家（补签/礼包/补偿/7d14d28d 兑换/抽奖）；夜猫子（23:00–08:00 窗口）。所有写入间隔 ≥1 s | 7 个 ops 测试 + 3 个 API 测试 |
| `internal/api` + 任务页（成长任务） | `GET /api/v2/tasks`、`POST /api/v2/tasks/{run,travel,streak-bonus}`；面板任务页新增成长任务卡片（账号切换、连登/能量/猫猫状态、任务列表、全自动跑一轮/猫猫旅行/连登管家与结果展示） | 3 个 API 测试；前端 21 vitest |
| `internal/usageest` + 用量页 | **剩余用量估算**：预算由真实撞线样本反推（窗口起点按上一次重置切分，按账号平均→按天平均→最近三天 6/3/1 加权）；24h 窗口分段、冷却中剩余记 0、无样本记未知；`GET /api/v2/usage/remaining` 带 ETag/304，用量页新增「剩余用量估算」区块 | 4 个估算单测 + 1 个 API 测试 |
| 剩余用量优先调度 | 估算剩余/预算 ≤30% / ≤15% / ≤5% 分别加权 2/3/4，作为池的第三层偏好（手动权重 → 临期积分 → 剩余用量 → 轮转）；冷却、无预算、剩余为 0 的组合不参与；设置页 `remaining_priority_enabled` 开关（默认关） | 3 个 pool 测试 |
| 模型封锁表 | 精确名单 + 封锁子串 + 白名单/前缀（`allowed_models` / `allowed_prefixes`，默认全空）；命中的模型在网关本机 400、不发上游，并从 `/v1/models` 隐藏；设置页可维护名单 | 1 个 settings 单测 + 2 个网关测试 |
| `internal/updates`（更新检查） | GitHub Releases 查询 + `releases/latest` 302 回退、版本比较、只读不下载；每天最多一次（`update_check_enabled` 默认关）+ 手动立即检查；状态（上次检查/最新版本）落盘；设置页显示当前/最新版本与发行页链接 | 5 个单测 + 1 个 API 测试 |
| `internal/quota`（额度主动查询） | `quota_scan_models` 勾选的模型每 6 小时为每个「账号×模型」发一个最小请求体检：通了清该模型冷却；429 带重置时刻则记冷却并把这条配额线交给窗口探针；失败按 15 分钟重试、6 次后顺延一个完整周期；已有 429 窗口或冷却中的线跳过。调度器独立执行；`GET/POST /api/v2/quota-scan*`，设置页可维护模型并立即查询 | 4 个单测 + 1 个 API 测试 |
| `internal/webtools`（Responses 本地 web 工具） | 开关 `local_web_tools`：把客户端声明的 `web_search` / `web_fetch` 换成网关自己的 function，模型调用后本地执行——搜索按 DuckDuckGo → Wikipedia → Google News 链式回退，抓取只允许公网 http(s) 文本页（私网/重定向复检、4 MB 上限、10 万字符分页），结果作为 tool 消息喂回并多轮继续（默认 3 轮、上限 8）。流式发 `web_search_call` 卡片并隐藏内部 tool_call；非流式只返回最终答案。客户端原始工具声明仍回显 | 6 个 webtools 单测 + 2 个 Responses 集成测试 |
| `internal/atrest`（桌面凭据导入） | AES-256-GCM 信封（AAD 逐字节对齐）、keyblob 解封自检、内存密钥缓存（换钥自动失效）、桌面 `*.info` 扫描、`accessToken`/`refreshToken`/昵称解密、按 token/域名判定区域后导入（国内版自动补签到）；支持手工 64 位 hex 保护密钥或 44 字符 base64 的 atRestSecretKey。`GET/POST /api/v2/accounts/desktop`，面板账号页新增“桌面导入”弹窗 | 6 个 atrest 单测 + 1 个 API 测试 |
| `internal/ops/webchat.go`（国际版网页会话） | 国际版每日活跃补上真正计入积分的网页腿：`POST /console/as/conversations/` 建会话 → 取沙箱 `link/token` → 接 ACP SSE（`Acp-Connection-Id`）→ `initialize` / `session/load` / `session/set_model` / `session/prompt` → 轮询会话到 `completed`。失败按首轮 120s 起翻倍重试（默认 3 次，可在设置里改次数/首轮等待）；桌面腿与网页腿合并成一次活动记录，只有网页腿成功才算当天完成。开关 `daily_chat_web` / `daily_chat_web_retry`，账号抽屉新增“网页活跃”动作 | 3 个 ops 单测（ACP 全流程、慢会话重试、CN 拒绝）+ 设置默认值断言 |
| `internal/pricing`（价格估算） | 从 OpenRouter `/api/v1/models` 拉快照（美元/百万 token，缓存读价），按固定 `usd_cny=7.1` 折算；模型解析顺序：手工策略 → 精确 OR id → provider 前缀别名（唯一才认）→ 去渠道后缀继承（唯一才认）；支持 `auto/manual/ignore` 策略、手工 OR id 映射与显式单价。快照/策略落盘到 `pricing.json` / `pricing_policies.json`，启动与每 12 小时自动刷新。`GET /api/v2/pricing`、`POST /pricing/refresh`、`POST /pricing/policy|mapping`、`GET /pricing/cost`（按模型汇总 prompt/completion/cached 后算等值 API 成本），用量页新增价格卡（总价、已定价/未定价、逐模型映射） | 5 个 pricing 单测 + 1 个 API 测试 |
| `internal/agents`（智能体配置） | 检测并接管 Claude Code（`settings.json` 的 env）、Codex（`config.toml` + `auth.json`）、OpenCode、DSH（`settings.yaml` + `.credentials.yaml`）、Crush、ZCode；首次接管备份每个文件（保留最早备份，最多 10 份），状态记 `agents.json`，可检测接管后的外部改动并一键恢复（原始字节 / 删除新建文件 / 清理空目录）。`GET /api/v2/agents`、`POST /agents/apply|restore`，仅 loopback + 面板登录，设置页新增配置卡片 | 4 个 agents 单测 + 1 个 API 测试 |
| `internal/upstream/tlsfp`（TLS 指纹） | 用 uTLS 自定义 ClientHello，按本机抓到的 Python 3.13 / OpenSSL 3.5.9 逐项复刻：cipher 顺序、extension 顺序（含 renegotiation_info 首项、encrypt-then-MAC、无 ALPN）、点格式；JA3 只差 supported_groups（去掉了 uTLS 无法完成握手的 X25519MLKEM768 / secp192r1 / FFDHE）。默认启用，`WB_TLS_PROFILE=go` 退回标准库；`Wire` 的直连、代理 TLS、CONNECT 隧道三条路径统一走它 | JA3 抓包单测（对拍 Python 的 cipher/extension 顺序）+ 完整握手测试 + Wire 集成测试（服务端看到的 cipher 顺序）+ 真实账号请求成功 |

`go vet`、`gofmt` 干净；`go test ./...` 全绿。本机没有 C 编译器，`-race` 跑不了，需在 CI（Linux）补。

### 出站/风控测试覆盖
线上头部顺序（Accept-Encoding → Content-Length → Host → 身份头 → Connection）· 紧凑 JSON、客户端键序保留 ·
CLI 身份：gzip、`cache_retention`、丢弃 `metadata`/`conversation_id`/`previous_response_id`、稳定的 `X-Conversation-ID` ·
换号重试复用同一个 `X-Conversation-Request-ID`、`X-Request-ID` 每次新生成 · 401→刷新→同号重试 / 刷新失败则换号 ·
11140 不换号、落盘 `restrictedUntil`、两个账号受限后整区域暂停并返回 429 + `Retry-After` ·
内容审核 403→中性提示词重试一次、之后窗口内持续降级、再失败则透传 · 自定义提示词文件 · 设置里的响应头超时生效。

### 网关行为测试覆盖
透传并去掉上游填充字段 · `<thinking>` 不再卡住（并有 Legacy 模式对照）· 流式中恢复被打印的工具调用 ·
429 换号且只冷却该模型 · 403 不换号直接透传 · 全部 429 · 无账号 503 · 鉴权/Key 模型限制/用量归属 ·
请求体超限 413 · 坏 JSON 400 · 非流式聚合与工具调用修复 · 缺失 usage 标记为估算且不编造 prompt 数 ·
客户端断连取消上游并释放账号 · 每账号独立连接池。

## 实测结果（本机 Windows，单机同时跑压测器+模拟上游+网关，仅供量级参考）

**账号选择**（`go test -bench`，14 逻辑核）：

| 池大小 | 旧 Python（上一轮实测） | Go |
|---|---|---|
| 100 | ≈ 0.13 ms/请求 | ≈ 37 ns |
| 1,000 | ≈ 1.2 ms/请求 | ≈ 38 ns |
| 5,000 | ≈ 8 ms/请求 | ≈ 36 ns |
| 5,000 且 90% 在冷却 | — | ≈ 7.7 µs（线性扫描，待优化为就绪集合） |

**泄漏过滤器**：持有状态下每 chunk 22 ns（旧 Python 在 16000 chunk 时 665 µs/chunk，且是 O(n²)）。

**并发流**（200 token、50 tok/s 的流，全部成功、没有 503）：

| 并发流 | 网关堆内存 | 稳态每流速率（要求 50/s） |
|---|---|---|
| 500 | ≈ 35–70 MB | p50 ≈ 47，p1 ≈ 41 |
| 2000 | ≈ 78 MB（进程总占用 ≈ 380 MB） | p50 ≈ 40，p1 ≈ 27 |

网关自身 CPU 很低：500 流时 8 秒窗口内只用了约 26% 个核，其中 57% 是 Windows 套接字写调用。

**用量统计冷启动**（合成 20 万行 / 88 MB 日志、同一台机器；旧 Python 在关闭落盘缓存的最坏情况下为 snapshot 16.9 s、analytics 19.4 s、时序 3.3 s、分账号 1.2 s，且同步占用 HTTP 线程）：

| 查询 | Go + SQLite 预聚合 |
|---|---|
| 总计 | 19 ms |
| 按模型 / 按账号 | 31 ms / 31 ms |
| 小时序列 | 24 ms |
| 今日各账号计数 | 2 ms |
| 一次性导入 20 万行旧日志 | ≈ 22 s（约 9k 行/s，后台进行，可断点续导） |

第一版把预聚合键做成"小时×账号×模型×Key"，结果每个请求几乎独占一行，查询仍要扫 20 万行（分账号 0.9 s、序列 1.6 s）。改成"按本地日的完整维度表 + 只含模型维度的小时表"后才降到上面的数字——**预聚合必须按"压缩比"设计，不是随便加一层表**。非整日边界的范围会自动退回精确扫描请求表，两条路径有测试互相校验。

**新面板实测**（内置浏览器 1024×768，3000 个账号，Go 二进制内嵌前端）：

| 项目 | 结果 |
|---|---|
| 前端体积 | 325 KB 原始 → **85 KB brotli**（旧面板：855 KB 单文件、无压缩） |
| 滚动到第 2500 行 | DOM 中只有 **36 个行元素、全页 598 个节点**；只请求了 `offset=2400` 和 `offset=2500` 两页（旧面板：3000 行整表重绘，每 15 秒一次） |
| **重启网关进程 + 重新加载页面** | **仍然是登录状态**（Cookie 为 HttpOnly，JS 读不到） |
| 另一端修改一个账号（权重 1→42、写入备注） | 当前页面上那一行 **2.5 秒内自行更新**，没有刷新、没有轮询 |
| 用量页 | 400 个请求：成功率 100%、64K tokens、首字延迟 85 ms、84.7 t/s，与模拟上游一致 |

**本轮新增功能的端到端**（真实 `bin/hub` + `bin/mockup`，6 个模拟账号，`127.0.0.1:18788`，面板密码登录）：

| 项目 | 结果 |
|---|---|
| 面板 | 登录（Cookie + CSRF）、设置读/写（开关、限额、时间表）、日志查询（级别/模块）、活动历史、调度器立即巡检（运行中返回 409 "已有巡检正在执行"）、导入预演 + 正式导入（6 → 7 个账号）、脱敏导出（不含 `accessToken`）、`/api/v2/settings` 返回 5 项限额与模型清单状态 |
| 网关 | `/v1/models` 无 Key 401、带 Key 18 个模型；`/v1/chat/completions` 流式 123 个 SSE 帧；打开"关闭 API Key 校验"后无 Key 也能访问，关掉后立即恢复 401 |
| 面板资源 | 首页 + 哈希 JS 正常下发（363 KB 原始 → 108 KB gzip / **94 KB brotli**）；未知 `/api` 路径返回 JSON 404 |
| 导入校验 | 非 JWT 的 `accessToken` 被报告为不合格行而不是整批失败；合法 JWT 预演与正式导入结果一致 |

**真实账号与并发/协议测试**：见 [TEST_REPORT.md](TEST_REPORT.md)。13 项协议兼容检查（Chat Completions / Responses / Anthropic Messages 的流式、非流式、工具调用、`count_tokens`、定址路由、错误透传）在真实国内版账号上全部通过；单账号 12 并发 225 RPM / 231 TPS 无 429；模拟上游 200 并发、8k TPS 无错误。**多账号调度测试**：10 → 1 000 账号的吞吐/首字延迟/面板接口无退化，1 000 账号 200 并发 8 047 TPS，账号选择 37.6 ns/次，1 000 账号启动 0.84 s、堆内存 8 MB。压测器 `cmd/loadtest` 新增 `-mode chat|responses|anthropic`、`-max` 与 RPM / TPS 统计。

### 这些数字**不能**证明什么
- **TTFT（首字延迟）这次没有测准。** 压测器、模拟上游、网关同机运行，Windows 回环上瞬间建立几百条新连接本身就要几百毫秒（压测器直连模拟上游、不经过网关，在无 ramp 的 100 并发下也是 ~280 ms）。网关内部从进入处理到首个上游 chunk 的耗时（hub 侧）是 p50 ≈ 258 ms，其中大部分是网关→上游新建连接。需要在 Linux 上、用连接复用或独立机器重测。
- 2000 并发时压测器自身和模拟上游已接近饱和（直连模拟上游时 p1 也只有 39.6），所以 2000 档的"每流速率"主要反映测试环境，不是网关上限。
- 没有和 Python 版做同机同条件的对照压测（Phase 0 的"Python 基线"还没跑）。

### 压测中发现并修掉的真实问题
`settings.Store` 最初每次读取都 `os.Stat` 一次 `settings.json`。在 Windows 上 500 并发时鉴权一步的 p99 达到 **~400 ms**（修复前实测），改成"至多每 500 ms 检查一次文件变更 + 快照修订号 + 解析后的 API Key 按修订号缓存"后降到 < 1 ms。旧 Python 的 `wb_settings.load()` 也是每次调用 `os.stat`、且请求热路径上约 10 次——这是否同样造成尾延迟**未在 Python 上实测**，但值得在 Phase 0 基线里确认。

## 与旧版有意不同的地方（需要你认可）

1. **泄漏过滤器默认用 Precise 策略**：只扣留"确实可能是工具调用"的标签（精确标签名或还没写完的前缀），`<thinking>`、`<think>`、`<args>`、`<params>` 之类普通文本立即放行；完整的 DSML 开标签（旧版因大小写正则 bug 根本不会扣留）现在会被正确扣留并恢复。`WB_LEGACY_LEAK_FILTER=1` 可切回旧行为。全部恢复场景（hybrid / DSML，整块与逐字符）在两种模式下都有测试。
2. 缺失的 `usage` 现在**标记为估算**，且 `prompt_tokens` 记 0，不再写成 `completion//2`。
3. 响应头超时**保持旧版的 120 s**（推翻了计划书里"降到 30–45 s"的提议）：思考型模型首字节可能很慢，贸然缩短会误杀正常请求。改为让**拨号 8 s / TLS 10 s** 各有独立短超时，死掉的主机或代理在几秒内失败；设置页里的 `upstream.header_timeout_seconds` 照常生效。
3b. 扣留文本有墙钟上限（默认 20 s，旧版只有 256 KiB 上限，等同于"到流结束"）。
4. 上游 chunk 正常时**保留上游的 id/created**，只有被过滤器改写的文本才合成新 chunk（旧版总是合成 `chatcmpl-wb`）。
5. 响应头在上游返回 200 后立即发出（旧版同样如此，因 Python 的 wfile 无缓冲）。
6. 换号逻辑**没有** `sleep` 退避；每次尝试的等待上限由分层超时决定（响应头默认 45 s，旧版 120 s）。
7. `created_at` 缺失时 API Key 条目保持空串（旧版每次读取都会盖当前时间，导致值不稳定）。
8. 账号文件里的 JSON 对象键顺序：嵌套对象（如 `credits`）按字母序写回，旧版保持插入序。语义相同。

9. **Responses 流式：上游中途断开时不再补发 `response.completed`**——只写 `data: [DONE]` 并记 `upstream_aborted`（旧版同样不补，这里只是明确不"伪造成功"）。
10. **Responses：缺省模型**——请求没带 `model` 时用 `deepseek-v4.1-flash`（旧版只在回显里用它，发给上游的是 null）。
11. **旧版会给被清洗改写过的 Responses 流帧多补一个空行**（`clean_responses_frame` 的 `splitlines` 副作用）；新版不补。SSE 解析上等价，黄金对拍在该处做了归一。
12. **`/v1/models?realm=` 只接受 `intl`/`cn`/`all`**，其余值回落默认区域（旧版把任意字符串当区域并用它做缓存键）。
13. **目录没有移植"窄接口回退"**（`/v2/enterprises/personal/models`，仅国际区且 `/v3/config` 与桌面缓存都拿不到时才用）：现在直接回落到内嵌快照。
14. **Responses 流里保留了旧版的一个小怪癖**：文本以 `<` 开头且疑似 DSML 时会先发一个空 `delta`。无害，保留是为了与旧版逐字节对拍。
15. **本地 web 工具**：只实现 DuckDuckGo / Wikipedia / Google News 三个搜索后端（旧版还有 Bing、StackOverflow），也不往最终文本注入 `url_citation` 注解；搜索结果文本本身带 URL。`web_fetch` 只接受公网 http(s) 文本页，私网或重定向到私网直接拒绝。
16. **桌面凭据的密钥回收未移植**：加密信封、keyblob 解封、凭据解密与导入都已实现，但没有旧版“扫 WorkBuddy 进程内存找 atRestSecretKey”的那一步；需要在面板里手工粘贴 64 位 hex 保护密钥或 44 字符 base64 的 atRestSecretKey。`hunt_supported=false`，接口会返回这条提示。
17. **价格估算只保留平面价**：没有旧版的条件 bands（输入长度阈值、UTC 分时价）、policy timeline / 历史引用 / 清理；汇率固定 7.1；成本在读取时按当前目录计算，历史记录不会按当时价格重算。未定价模型仍显示 token 数，可在价格卡里手工映射 OpenRouter ID。
18. **智能体配置的 YAML / TOML 会整份重写**：旧版是文本级 upsert，能保留注释和排版；Go 版用 yaml.v3 / go-toml 解析后重新序列化，注释会丢失（接管前的备份仍能完整恢复）。另外只写模型 id，不写上下文长度、最大输出和思考档位，所以 DSH 等客户端不会出现按模型的能力/档位选择器。
19. **TLS 指纹的 supported_groups 与 Python 不完全一致**：cipher 列表、extension 顺序、点格式逐字节对齐（JA3 的四个字段相同），但 uTLS 在宣告 X25519MLKEM768 / secp192r1 / FFDHE2048/3072 时无法完成握手，因此这三类组被去掉，JA3 的曲线字段不同。默认使用该 profile；`WB_TLS_PROFILE=go` 可退回 Go 标准库（用于排查）。

## 前端 UI 重构（进行中，参考 oh-my-cpa）

- **P0/P1 已完成**：前端换成 Ant Design 6 外壳（`web/src/ui/theme.tsx`、`AppShell.tsx`）；分组侧栏（运行 / 观测 / 控制）、顶栏面包屑、主题切换、登录/改密页、品牌字标；浅色 / 深色两套 token 对齐参考的 OMC Light / Dark；`antd/dist/reset.css` + 旧样式兼容层。已用 headless Edge 截图验证登录页、浅色外壳、深色外壳。
- **P2/P3 已完成（账号页）**：账号页改用 antd 的 PageHeader + 统计卡（全部/可用/冷却/风控/需登录）+ 工具栏（搜索、区域、排序、状态 Segmented）+ `Table`（100/页服务端分页、行选择、状态 Badge、区域 Tag、积分/有效期列、行内更多操作）；批量操作仍复用原 BatchBar（功能保留，样式后续统一）。浅色 / 深色截图均已核对。
- **P4 已完成（用量页）**：8 张统计卡（请求/成功率/总 Token/输出/缓存/首字/速度/积分）、`@ant-design/charts` 的 Token 趋势图（浅/深主题）、价格估算卡（总价、逐模型成本、未定价映射）、按模型/账号/Key/区域的明细表、剩余用量估算表（Progress 占用条）。浅色 / 深色截图均已核对。
- 页面内部其余部分仍是旧组件：账号抽屉与添加/导入/导出/桌面导入弹窗、设置 / 日志 / 任务页。
- **P5 完成**：所有页面、详情抽屉与弹窗均为 antd：账号、用量、设置、日志、任务、添加账号、导入、导出、桌面导入、Key 编辑、账号详情。用量页新增 antd `DatePicker.RangePicker`（对应后端 `range=custom&since&until`），预设区间与自定义日期可切换，趋势粒度按跨度自动选择小时/天。
- **P6 完成**：五个页面之外新增 **请求记录**（按时间/结果/区域/模型/账号筛选，行点击打开请求详情抽屉：token、缓存、首字、生成耗时、排队、TPS、思考档位、用量来源）和 **模型** 页（`GET /api/v2/models` 面板专用目录，展示能力标签、上下文/最大输出、思考档位）。用量页补充 **模型用量环形图** 与自定义日期范围；日志查看器修复深色模式下白底问题；请求记录在手机端改为卡片列表。批量操作条、JobToast、全部弹窗/抽屉均为 antd；Sarasa Mono SC 核心分片已打包；`tools/uitest/smoke.mjs`（`npm run uitest`）覆盖登录、全部页面、主题、批量选择、请求详情、账号抽屉、全部账号弹窗、Key 编辑、日志暂停，全部通过并逐页截图。
- 已知偏差：视觉是按参考的 token/组件语言复刻，不是逐像素克隆（品牌字标、页面文案、功能菜单按本项目实际能力调整）；`uitest` 覆盖主要路径，未逐字段提交每个表单。

## 还没做

- 本机没有 C 编译器，`go test -race` 跑不了，需在 CI（Linux）补。
- 90% 冷却时的选号仍是线性扫描（未改成就绪集合）。
- UI 视觉是按参考项目的 token / 组件语言复刻，不是逐像素克隆；品牌字标、菜单和文案按本项目能力调整。
- 浏览器回归已由 `tools/uitest/smoke.mjs` 覆盖登录、五页、主题、批量选择、抽屉、全部账号弹窗、Key 编辑、日志暂停；表单逐字段提交（新增 Key、导入预演、账号保存）仍只靠 API/单元测试。

## 怎么跑

```bash
cd web && npm ci && npm test && npm run build && cd ..   # 构建内嵌的面板（输出到 internal/webui/dist）
go test ./...                         # 全部测试
py -I tools/genvectors/gen.py         # 用 legacy/python 重新生成黄金向量
py -I tools/genvectors/gen_build.py   # 请求体整形的 325 条对拍向量
py -I tools/genvectors/gen_catalog.py     # 模型目录对拍向量
py -I tools/genvectors/gen_responses.py   # Responses 转换/流事件对拍向量
go build -o bin/ ./cmd/...            # hub / mockup / loadtest

bin/mockup -addr 127.0.0.1:19000 -tokens 200 -tps 50
bin/loadtest genaccounts -dir run/accounts -n 50 -key sk-test
bin/hub -port 18788 -accounts-dir run/accounts -upstream-override http://127.0.0.1:19000 -debug
bin/loadtest -url http://127.0.0.1:18788/v1/chat/completions -c 500 -key sk-test -ramp 5s \
             -stats http://127.0.0.1:18788/debug/runtime

# 面板/账号操作的端到端（模拟上游，6 个账号）
bin/loadtest genaccounts -dir run/e2e/accounts -n 6 -key sk-test
bin/hub -port 18788 -accounts-dir run/e2e/accounts -usage-dir run/e2e/usage \
        -catalog-dir run/e2e/catalog -upstream-override http://127.0.0.1:19000 \
        -panel-password e2e-pass -no-modelsdev
```

## 版本控制说明
当前在分支 `v2-go`，按里程碑提交。本轮提交：`feat: dashboard settings, logs, tasks, account import/export and account actions`。
