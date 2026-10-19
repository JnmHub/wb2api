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
| `cmd/hub` `cmd/mockup` `cmd/loadtest` | 网关、模拟上游、压测器 | 手动跑通；**端到端实测**：200 个请求→用量库恰好 200 行、8000 completion tokens、TPS≈192（模拟上游 200/s），重启后同一 Cookie、同一数据 |

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

## 还没做（按计划书优先级）

**Phase 2 网关热路径剩余**
- 模型目录（`wb_catalog`/`wb_modelsdev`）：`shape.Catalog` 目前是接口，没有真实实现——`max_tokens` 缺省值与 DeepSeek 默认思考档位要等目录移植后才与旧版一致（对拍时用旧版导出的小表）。`upstream_effort_of`、`/v1/models`、`/v1/balance-summary` 同理。
- **账号失败治理状态机**：软限流阶梯（600 s→2 h）、无作用域 429 的 60 s 阶梯、连续失败熔断（30 min→6 h）、降级窗口、`fails` 计数衰减（`WB_FAIL_WINDOW`）、402 停放到本地 04:00、`model_caps`（6004 落盘）、窗口探针。**目前 429 一律冷却该模型 60 s、5xx 冷却账号 5 s，是占位策略。**
- 调度偏好：手动权重 / 临期积分 / 剩余用量加权轮询、会话亲和（`SessionAffinity`、`affinity_route`）、`apply_daily_guards` 四条限额护栏、`WB_BACKGROUND_MODEL_*` 后台模型重定向、模型封锁表、`/u/<ref>/v1/*` 定址调用。
- 限额护栏接入用量库（`usagedb.Day` 已提供当日计数，调度侧尚未使用）；`normalize_usage_cache_aliases`；Responses API / Anthropic Messages。
- 账号管理 API（`/api/v2/accounts`：分页/筛选/批量）、设置 API、SSE 事件总线、OAuth 登录流程；新前端。
- TLS 指纹（JA3/JA4）：现在用 Go 标准 `crypto/tls`，与旧版 Python 的 OpenSSL 指纹不同（风险 R1，Phase 0 的真实上游验证仍未做）。
- 90% 冷却时的选号从线性扫描改为就绪集合。

**Phase 3+**：面板鉴权（Cookie 会话落库）、`/api/v2`、SSE 事件总线、新前端、统计预聚合、各类自动化任务……（见计划书）。

## 怎么跑

```bash
go test ./...                         # 全部测试
py -I tools/genvectors/gen.py         # 用 legacy/python 重新生成黄金向量
py -I tools/genvectors/gen_build.py   # 请求体整形的 325 条对拍向量
go build -o bin/ ./cmd/...            # hub / mockup / loadtest

bin/mockup -addr 127.0.0.1:19000 -tokens 200 -tps 50
bin/loadtest genaccounts -dir run/accounts -n 50 -key sk-test
bin/hub -port 18788 -accounts-dir run/accounts -upstream-override http://127.0.0.1:19000 -debug
bin/loadtest -url http://127.0.0.1:18788/v1/chat/completions -c 500 -key sk-test -ramp 5s \
             -stats http://127.0.0.1:18788/debug/runtime
```

## 版本控制说明
当前在分支 `v2-go`，按里程碑提交。
