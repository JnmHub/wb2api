# 真实账号与并发/协议测试报告

测试时间：2026-10-19（Asia/Shanghai）。
环境：Windows 11，Go 1.27.1，单机同时跑网关、模拟上游与压测器（所有网络都是 127.0.0.1 回环）。
被测代码：`v2-go` 分支 `054a691`（本轮工具与文档改动另提交）。

## 1. 导入真实账号

账号导出文件由用户提供（1 个国内版账号，昵称已脱敏，企业版字段为空，Token 有效期至 2026-11-14）。

| 步骤 | 结果 |
|---|---|
| `POST /api/v2/accounts/import` 预演 | count=1，added=1，invalid=0，skipped=0 |
| 正式导入 | count=1，added=1；`/accounts/summary` → `{"cn":{"ready":1}}` |
| 凭据落盘 | `accounts/<uid>.json`，`active` 状态，`realm=cn` |
| API Key | 新建 `real-test`，realm=cn，`auth_required=true` |

导出回读（`secrets=0`）：`format=workbuddy-accounts`，`accessToken`/`refreshToken` 均被移除，UID 与区域保持正确。

## 2. 账号功能实测（真实上游）

| 功能 | 结果 |
|---|---|
| 积分刷新 | ok，余额 **2451.7** |
| 账号测试（probe，模型 hy3） | ok，**2147 ms**，回复 `Hello! How can I help you today?` |
| 国内签到 | ok，上游返回“今天已签到，请明天再来”（幂等成功） |
| 对话活跃上报 | ok，**连登 2 天** |
| 成长任务快照 | 16 个任务：11 个已领取、3 个已接取（仅桌面端）、2 个未接取（公众号关注 / Valorant 活动） |
| 猫猫旅行 | ok，“今日已完成旅行，明日 00:00 刷新” |
| 连登管家与抽奖 | ok，“连登状态正常（无需兑换/无抽奖次数）” |
| 全自动成长任务 | ok，16 个任务，6 条日志；桌面端任务与不可伪造任务按设计跳过，本轮 +0 积分 |
| 导出（脱敏） | ok，Token 字段被移除 |
| 日志 / 用量 | 日志 3 条（accounts/scheduler）；用量 93 次请求、92 成功、9343 tokens、3 个模型 |

## 3. 三套协议兼容性（真实上游，模型 glm-5.3-flash）

脚本：`tools/livetest/compat.ps1`（13 项，全部 PASS）。

| 检查 | 结果 |
|---|---|
| Chat Completions 非流式 | 200，`content=pong`，usage 27 |
| Chat Completions 流式 | 200，增量拼出 `pong`，`data: [DONE]` |
| Responses 非流式 | 200，`output_text=pong`，`status=completed`，output_tokens=3 |
| Responses 流式 | 200，事件序 `created → in_progress → output_item.added → output_text.delta → output_item.done → completed`，文本 `pong` |
| Anthropic Messages 非流式 | 200，`type=message`，text `pong`，in=24/out=3 |
| Anthropic Messages 流式 | 200，事件 `message_start … content_block_delta … message_stop`，文本 `pong` |
| `count_tokens` | 200，`input_tokens=3` |
| `/u/<uid>/v1/chat/completions` 定址路由 | 200 |
| Chat 工具调用 | 200，`tool_calls=1` |
| Responses 工具调用 | 200，`output` 条目类型为 `function_call` |
| Anthropic 工具调用 | 200，内容块 `tool_use`，`stop_reason=tool_use` |
| 未知模型 | 400（透传上游 `code=11102 model ... service info not found`） |
| `/v1/models?realm=cn` | 200，17 个模型 |

## 4. 真实账号并发 / TPS / RPM

模型 `glm-5.3-flash`，每次请求 `max_tokens=64`，全部流式，同账号、单区域：

| 并发 × 请求数 | 成功 | RPM | TPS（completion tokens/s） | TTFT p50 | TTFT p99 | 429/错误 |
|---|---|---|---|---|---|---|
| 1 × 1 | 1/1 | 24 | 26.0 | 1.23 s | — | 0 |
| 3 × 9 | 9/9 | 64 | 67.8 | 1.73 s | 1.99 s | 0 |
| 6 × 12 | 12/12 | 141 | 150.6 | 1.25 s | 1.35 s | 0 |
| 12 × 24 | 24/24 | 225 | 231.4 | 1.48 s | 2.53 s | 0 |

单账号、单模型在 12 并发下仍有 225 RPM / 231 TPS，未触发 429 或风控。注意这里的 RPM 是“按当前并发与单请求 2–5 秒耗时实测的吞吐”，不是账号配额上限；本轮刻意为单账号压到 12 并发后停止，避免长时间高 RPM 触发上游风控。

协议并发（同一真实账号，`max_tokens=256`，3 并发 × 2 轮）：

| 协议 | 成功 | RPM | TPS | TTFT p50 | 说明 |
|---|---|---|---|---|---|
| Responses | 6/6 | 34 | 135.1 | 4.12 s | 思考型回答，首字包含推理阶段 |
| Anthropic Messages | 6/6 | 34 | 146.9 | 4.25 s | 同上 |

## 5. 模拟上游并发基准（网关侧）

`tools/bench/bench.ps1`：50 个模拟国际版账号，模拟上游每流 200 token、50 token/s，全部流式；网关与 mock 同机。

| 场景 | 成功 | RPM | TPS | TTFT p50 | TTFT p99 | 错误 |
|---|---|---|---|---|---|---|
| chat c=10 n=40 | 40/40 | 148 | 494 | 0.7 ms | 178 ms | 0 |
| chat c=50 n=200 | 200/200 | 714 | 2 379 | 0.6 ms | 808 ms | 0 |
| chat c=100 n=400 | 400/400 | 1 337 | 4 456 | 0.6 ms | 1.90 s | 0 |
| chat c=200 n=800 | 800/800 | 2 420 | 8 067 | 0.7 ms | 3.11 s | 0 |
| Responses c=50 n=200 | 200/200 | 714 | 2 380 | 0.6 ms | 764 ms | 0 |
| Anthropic c=50 n=200 | 200/200 | 714 | 2 378 | 0.7 ms | 781 ms | 0 |

三种协议在同一并发下吞吐几乎相同（Responses / Anthropic 只是请求与事件的翻译层，不改变上游流式管道）。c=200 时 8 067 TPS 已接近模拟上游 200 并发 × 50 token/s 的 pacing 上限；TTFT p99 随并发上升主要是 200 条同机回环连接同时建立的成本。

## 6. 多账号调度（10 / 100 / 1000 个账号）

`tools/bench/many-accounts.ps1`：模拟上游每流 200 token @ 50 token/s，固定 50 并发、每档 200 请求；账号全部参与调度。面板接口列是本机 PowerShell 计时（含 ~8 ms 客户端开销），用于横向比较而非绝对值。

| 账号数 | 启动到健康 | 成功 | RPM | TPS | TTFT p50 | TTFT p99 | 堆内存 | 账号页(100行) | 过滤排序(50) | 汇总 | 用量汇总 |
|---|---|---|---|---|---|---|---|---|---|---|---|
| 10 | 309 ms | 200/200 | 710 | 2 367 | 583 µs | 898 ms | 4.0 MB | 9.5 ms | 8.8 ms | 8.9 ms | 8.2 ms |
| 100 | 293 ms | 200/200 | 706 | 2 353 | 573 µs | 962 ms | 4.9 MB | 33.6 ms | 17.6 ms | 9.3 ms | 9.3 ms |
| 1000 | 837 ms | 200/200 | 717 | 2 390 | 561 µs | 623 ms | 8.0 MB | 21.8 ms | 13.3 ms | 8.4 ms | 10.4 ms |
| 1000（90% 停用） | 284 ms | 200/200 | 708 | 2 359 | 597 µs | 918 ms | 7.2 MB | 17.8 ms | 10.8 ms | 7.0 ms | 7.2 ms |

1000 个账号再压 200 并发 × 800 请求：**800/800 成功，2 414 RPM，8 047 TPS，TTFT p50 614 µs / p99 3.32 s，堆内存 55 MB**，与 50 账号时同并发的结果（2 420 RPM / 8 067 TPS / p99 3.11 s）一致。

账号选择基准（`go test -bench`，14 逻辑核）：

| 池大小 | 每次选择 |
|---|---|
| 1 000 | 37.6 ns |
| 5 000 | 37.6 ns |
| 5 000，90% 冷却 | 6.0 µs |

结论：

- 请求路径的账号选择是 O(1) 轮转（37 ns），与账号数无关；只有在大量账号处于冷却时才会线性扫描（5 000 账号、90% 冷却仍只有 6 µs/次）。
- 每日限额扫描是每 5 秒一次的 O(n) 增量扫描，1 000 账号约几十微秒；请求路径只读原子状态，不持池锁。
- 吞吐、首字延迟、面板接口在 10 → 1 000 账号间没有可见退化；1 000 账号启动约 0.84 s，堆内存约 8 MB（1 000 个账号对象）。
- 90% 账号停用时同样无退化（轮转跳过不可用账号）。
- 注意：无人值守巡检按 2–5 秒错峰，1 000 个账号一整轮需要约 1–1.5 小时。这是刻意的防风控节奏；它在独立 goroutine 中运行，**不阻塞请求**，但“立即巡检”在这段时间会拒绝再次触发。

## 7. 结论与注意

- 三套协议在真实上游上全部跑通，流式事件序、工具调用、`count_tokens`、定址路由、错误透传均符合预期。
- 单账号 12 并发、225 RPM 无 429；更长时间/更高 RPM 的账号配额行为**未测**，不要据此推断账号上限。
- 模拟基准证明网关在 200 并发流、8k TPS 量级下无错误、无 503；数字受同机 mock pacing 限制，不能当作网关上限。
- 真实账号测试会消耗积分/额度并可能触发上游风控，`account-ops.ps1` 的 `-FullGrowth` 与高并发请按需使用。
- **本地 web 工具已在真实上游上跑通一次**：开启 `local_web_tools`、`tool_choice:"required"` + `web_search` 时，模型发出搜索调用，网关执行本地搜索并把结果喂回，最终回答明确提到“搜索没有返回官方信息”。结果质量受本机到各搜索后端的影响（本机 DuckDuckGo 返回 202 反爬，自动回退到 Wikipedia / Google News）；每次是否愿意调用仍取决于模型。
- **TLS 指纹已用真实账号验证可用**：默认的 OpenSSL 形状 ClientHello（`internal/upstream/tlsfp`）对真实上游发 `glm-5.3-flash` 请求返回 200（约 1.9s）；本机抓包显示 cipher/extension/point-format 与 Python 3.13 / OpenSSL 3.5.9 完全一致，supported_groups 少了 uTLS 无法握手的几个组。`WB_TLS_PROFILE=go` 可切回标准库。

## 8. 浏览器逐页回归

`tools/uitest/smoke.mjs` 通过 Chrome DevTools Protocol 驱动本机 headless Edge，不依赖 Playwright：

```powershell
go build -o bin/hub.exe ./cmd/hub
bin\hub.exe -port 18888 -accounts-dir run\real\accounts -panel-password real-test
cd web
npm run uitest -- --url http://127.0.0.1:18888 --password real-test
```

覆盖并通过：登录、账号 / 用量 / 模型 / 请求记录 / 设置 / 日志 / 任务七个页面、浅色→深色切换、行选择/批量条、请求详情抽屉、账号详情抽屉、添加账号 / 导入（同时注入两个 JSON 文件）/ 导出（含选中账号）/ 桌面导入弹窗、新建 Key 弹窗、日志暂停/继续；同时收集 `Runtime.exceptionThrown`、`Log.entryAdded`、`console.error`，忽略登录前的 401 session 探测。截图输出到 `.scratch/uitest*/`（不提交）。2026-10-20 在真实 CN 账号上通过。

## 9. 并发上限与长上下文

- **网关侧不再限制并发**：删除了按 API Key 的 `concurrency_limit` 拒绝逻辑（字段保留但忽略），
  `TestKeyConcurrencyIsNotCapped` 断言已存 `concurrency_limit=1` 时第二个在途请求仍返回 200。
  单账号仍可用 `-per-account-inflight` 限制（默认 0 = 不限），但默认不设任何并发上限。
- **长上下文高并发**：`TestLongContextHighConcurrency`（`internal/gateway/openai`）用真实
  gateway 路径压测：100 个并发非流式请求 + 48 个并发流式请求，每个 prompt 约 200 KB，
  上游返回约 12.8 KB 内容。断言全部 200、非流式与流式内容逐字节等于期望值、无截断/串流/
  并发上限。测试耗时约 10s，`go test ./...` 中包含。
- 该测试第一次运行时暴露的是 **测试夹具** 的 usage channel 只有 64 缓冲、同步回调阻塞
  handler，已改为按请求量扩容；不是网关长上下文传输 bug。
- Docker：`docker compose config` 校验通过；本机只有 Docker CLI，Docker daemon 未安装，
  因此镜像构建未在本机执行。GitHub Actions `release` 工作流已真实跑通，
  产出 `wb2api-linux-amd64` / `arm64` 及 sha256，发布在 v2.0.0 至 v2.1.0。
- **CI 真跑出了两个真实问题**：GitHub 的 `-race` 发现 `wireBody` 的 `eof/done` 与 ACP
  扫描协程并发读写（已加锁，并且只有 Close 前已观察到 EOF 才归还连接）；Windows runner
  上的 `wsarecv: ... was aborted` 未被识别为陈旧连接（已加入重试判断并补测试）。
  修复后 `ci`（Windows 普通测试 + Ubuntu `-race`）和 `release` 均通过。

## 10. 积分统计与零积分调度

- `GET /api/v2/accounts/summary` 新增 `credits`（remain/available/expiring_7d/today_used/known/zero）、
  `by_realm_credits`（每个区域的 remain/available/expiring_7d/total/ready/zero/keys）和 `keys`。
  真实账号实测返回 `remain=2321.16`、`available=2321.16`、`keys=1`。
- 账号页展示总剩余积分、可用积分、今日消耗、7 天到期、0 积分账号，以及国际版/国内版区域卡
  （剩余、可用、可用账号 x/y、0 积分、Key 数）；点击区域卡可切换筛选。
- **零积分不调度**：`pool.Account.State` 新增 `no_credit`，`Ready` 对已知 `remain <= 0` 的账号
  直接返回 false（免费模型也不放行）。`TestZeroCreditIsNotSchedulable` 验证该账号不会被 `Pick`，
  `TestSummaryCredits` 验证 summary 的 zero/available/no_credit 计数，`TestCreditExpiringWithin`
  验证 7 天到期求和。余额未知的账号仍可参与调度（无法判断时不应误杀）。
- 批量任务：批量启用/停用、签到（仅国内版）、每日活跃（仅国际版）仍在批量条“更多操作”中；
  smoke 会展开该菜单并断言签到入口存在。**混选两个区域时，签到会先过滤掉国际版账号
  （企业国内版也跳过），每日活跃会过滤掉国内版账号**，不再把“checkin is only available
  for CN realm accounts”记成失败；如果所选账号没有任何可执行对象，接口直接返回明确提示。
  `TestBatchRealmFilter` 覆盖这两个过滤方向与空结果。

## 11. 账号冷却的触发条件

| 上游/本地事件 | 作用范围 | 冷却或处置 |
|---|---|---|
| 429，账号级软限流 | 整个账号 | 10 分钟起，连续翻倍，最多 2 小时（`cooldownTo`） |
| 429，模型级且带 reset | 该账号的该模型 | 到 reset 时刻（最短 1 秒） |
| 429，无 reset 的软限流形状 | 该账号的该模型 | 60 秒起翻倍，最多 10 分钟 |
| 402 / 积分不足 | 整个账号 | 停到下一个本地 04:00（`balanceTo`）；若余额已知为 0，同时进入 `no_credit` |
| 401 且刷新失败 | 整个账号 | 60 秒（单账号 3 秒）冷却 + `need_login` |
| 连接/传输失败 | 整个账号 | 开启出口退避时只退避出口并重试；否则 60 秒（单账号 3 秒）冷却并计入连续失败 |
| 5xx 硬失败 | 整个账号 | 不立即冷却；连续 3 次触发熔断，30 分钟起翻倍，最多 6 小时 |
| 未知失败 | 整个账号 | 连续 5 次触发降权，10 分钟起翻倍，最多 2 小时 |
| 403 / 11140 风控 | 整个账号 | 风控限制 2 小时（持久化到账号记录） |
| 探测失败 | 整个账号 | 60 秒冷却 |
| 保留积分线 / 今日 Token / 今日积分额度 | 账号或模型 | 不算冷却，只是 `Ready=false`，不会选中 |

`State()` 现在把软冷却、熔断、降权、余额保护都显示为“冷却中”；0 积分优先显示“无积分”。
账号详情会显示 `unavailable_reason`（例如“熔断 剩余 30m”“余额保护 剩余 16h”），
便于区分“冷却”与“额度限制”。新增 `TestStateShowsBreakerAndBalanceAsCooling`。

## 12. 450 账号批量任务的性能实测

用 mock 上游 + 450 个国际版账号复现“批量刷积分时正常聊天是否变慢”：

- 上游 `/v2/billing/meter/get-user-resource` 人为延迟 500ms，批量刷新 450 个账号约 68s；
- 批量运行期间同时跑 100 个聊天流（c=10，200 token，50 tok/s）：
  - 基线 TTFT：p50 **584µs**、p99 **270.9ms**
  - 批量期间：p50 **591µs**、p99 **214.0ms**
  - 吞吐：496 → 486 chunks/s（约 -2%）
  - 单流 chunk 间隔：p50 24ms → 59ms
- 批量任务完成后 attempts=450、failed=0。

结论：网关请求路径没有被批量任务阻塞，首字延迟基本不变；影响主要是上游带宽/流式平滑度。
真实网络里 450 个账号的批量操作是对**同一个出口 IP** 的持续请求，供应商可能因此对
正常模型请求限速；这不是网关内部锁。可以在“设置 → 批量账号任务”里调低并发
（1–16）并增加每个账号操作后的间隔（0–60000ms），保存立即生效；`OpsGate` 支持运行时
改限流参数，`TestOpsGateLimitsAndReconfigures` 覆盖“满员等待/扩容后放行”，
`TestOpsSettingsPatch` 覆盖设置接口校验。
积分刷新遇到的 `HTTP 500 {"code":10000,...}` 是上游账单接口返回的 500（带 requestId），
不是网关错误；现在 CN 账单查询会重试一次，仍失败才会把这个账号记入失败清单。

## 13. DSH 系统提示词 11128 指纹

上游 WAF 会对客户端 system prompt 里的自我介绍句 + 品牌/型号返回
`400 {"code":11128,"msg":"Illegal API invocation from an unapproved channel"}`：

```text
You are an AI agent powered by DeepSeek Harness.
You are a coding agent powered by the deepseek-v4.1-flash model.
You are an AI assistant built on DeepSeek.
```

按上游 v1.7.18 的口径，`internal/gateway/shape/sanitize.go` 把 **system / developer**
消息里的这些整句改写为中性句（同时兼容早期的 `agent powered by` 句式和
v1.7.18 的 `assistant built on` 句式）：

```text
You are a helpful AI assistant.
```

- 匹配 `You are an [AI ]agent powered by … .`、`You are a coding agent powered by … .`、
  `You are an [AI ]assistant built on … .` 整句，大小写不敏感；
- 首句之外残留的 `DeepSeek Harness` 品牌词（DSH 0.2.1-rc.2 的 implementation checkout /
  Web GUI 句）替换为 `runtime`；`DeepSeek-Harness`、`DeepSeek_Harness` 等分隔符变体同样覆盖；
- user / assistant / tool 里的同文本属于用户数据，保持原样；
- 句末后面的工具/行为契约不动；
- 品牌名不再保留（v1.7.18 的通用化做法，不依赖具体品牌）。

验证：

- `TestDSHAgentFingerprintRewrite`（agent powered by / assistant built on / 带点型号 / 大小写 / 后续文本）、
  `TestDSHIdentityOnlyAppliesToSystemAndDeveloper`、`TestDSHSystemPromptKeepsToolContract`、
  `TestDSHBrandOnlyAppliesToSystemAndDeveloper`、`TestDSHBrandIsAFingerprint`；
- 真实 CN 账号：修复前同一请求 400/11128；重启后 `hy3` 返回 **200**；
  `deepseek-v4.1-flash` 返回 **429**（当日模型额度用尽，不再出现 11128），说明 WAF 拦截已解除；
- 工具说明等后续内容未被改写。

## 14. 单请求体上限

三套协议共用 `Handler.MaxBody`，默认 **16 MiB**，在解析 JSON 前用
`http.MaxBytesReader` 拦截，因此超限返回 `413 payload too large`，不会进入上游。
现在可用 `-max-body-mb N` 或 `WB_MAX_BODY_MB=N` 调整（1–1024 MiB），重启生效；
Docker Compose 也会透传 `WB_MAX_BODY_MB`。`TestBodyTooLarge` 覆盖自定义上限下的 413。

## 15. v1.7.18 对照

已补到 Go 版本：

- **DSH 11-128 通用身份句**：system/developer 中性化为 `You are a helpful AI assistant.`，
  user/assistant/tool 不动（见第 13 节）。
- **WorkBuddy_Valorant 拼豆专家**：登记为固定专家任务，事件使用
  `ex_xQHHD1u5Jkua`，不再落回通用专家池；`TestGrowthEventShapes` 覆盖。
- **全池刷新积分单飞**：同一时刻只允许一个全池 `refresh_credits` 任务，重复点击复用
  正在运行的 job（返回 `busy: true`）；单账号/带筛选的刷新不受影响。
  `TestFullCreditSweepDetection` 覆盖判定边界。

本项目不适用 / 已有等价实现：

- Python 的 `random` 导入缺失、exFAT mtime 测试、CPython 3.9 HTTPError 分类：Go 实现不存在这些问题；
- 移动端 header/账号卡/积分格布局、消费倍率列截断、i18n 文案：我们的 antd 页面结构不同，已有自己的响应式布局；
- `X-Private-Data` 文案反转：Go 版本目前按身份固定发送，没有这个下拉项。

仍可后续补充（本轮未做）：

- DSH 的 `cordis.patch.yml` profile 补丁层与放宽后的 `retryPolicy`（当前只写 `settings.yaml`，DSH 升级后可能不生效；需要保持注释的 YAML 编辑器）；
- 面板/日志/导出中的手机号昵称脱敏；
- 流式 usage 尾帧的严格位置与模型级 5xx 的更细分类（本轮已处理模型级 5xx 不计熔断）。

## 16. 流式 TPS、模型过滤与 usage 修正

- **流式 TPS**：全局 `output_tps_limit`（默认 0=关闭）+ 每个 API Key 的 `tps_limit`
  （0=跟随全局，-1=该 Key 关闭，>0=覆盖）；只对流式响应生效，非流式不受影响。
  首 token 立即发送，后续按估算输出 token 的平均速率节流；上游生成/计费速度不变。
  高并发用例 `TestOutputTPSHighConcurrency`：50 路并发、每路 40 个 token、上限 100 t/s，
  全部 50 路返回完整内容；单路由 0.4s 的目标时长验证了节流确实生效（整组约 2.6s）。
- **`/v1/models` 按 Key 过滤**：新增开关 `filter_models_by_key`（默认关闭）。
  打开后，配了模型限制的 Key 只看到允许的模型；未限制的 Key 和面板仍看全部。
  `TestModelsFilteredByKeyWhenEnabled` 覆盖开关开/关。
- **Anthropic `input_tokens`**：从 `prompt_tokens` 中扣除 `cache_read` 和
  `cache_creation`，避免缓存命中的上下文被双计；golden 数据已按新口径更新。
- **模型级 5xx**：上游 `11102/11129/11134/11150/11158` 这类明确表示模型/服务不可用的
  5xx 不再计入账号熔断，直接返回上游错误，不轮转账号；
  `TestModelScoped5xxDoesNotChargeAccount` 覆盖。
- **冷却筛选**：账号级冷却和任意模型的模型冷却都归入“冷却”筛选；账号页新增
  “模型冷却”计数与行内标签，避免模型级 429 被误认为账号正常。
  `TestCoolingFilterIncludesModelCooldowns` 覆盖。

## 17. 性能优化（TPS 关闭零额外解析 / 冷却扫描）

- **TPS 关闭**时不再调用 `chunkOutputTokens`：Chat 复用已经 `parseChunk` 的 chunk，
  Responses/Anthropic 在 pacer 为 nil 时直接跳过 token 估算；只有 TPS > 0 才做额外解析。
- **账号列表**只在真正筛选冷却时调用 `HasModelCooldown`（无分配、无排序）；
  **summary** 每个账号只扫描一次模型冷却，不再调用两次 `ModelCooldowns`。
- 不改变任何协议输出或筛选语义；`TestOutputTPSHighConcurrency`、冷却筛选测试和
  完整 `go test ./...` 均通过。

## 18. 复现

```powershell
# 模拟并发基准（自带 mock 与假账号池）
pwsh tools/bench/bench.ps1

# 多账号调度（10 / 100 / 1000 账号 + 90% 停用变体）
pwsh tools/bench/many-accounts.ps1
pwsh tools/bench/many-accounts.ps1 -Sizes 1000 -Concurrency 200 -Requests 800

# 真实账号协议矩阵（网关需已导入账号并建好 Key）
pwsh tools/livetest/compat.ps1 -BaseUrl http://127.0.0.1:18888 -ApiKey sk-... -PanelPassword ...

# 真实账号功能扫一遍
pwsh tools/livetest/account-ops.ps1 -BaseUrl http://127.0.0.1:18888 -PanelPassword ... -Model hy3
```

## 19. PostgreSQL + Redis 架构迁移（Phase 1）

已实现：

- PostgreSQL 存账号和 settings（JSONB）；空库首次启动自动导入现有
  `accounts/*.json` / `settings.json`；
- Redis 账号级 in-flight 槽位（Lua sorted set）和 `/health` 后端状态；
- 会话亲和的 TTFT/错误率 EWMA 逃逸（默认 15s / 0.5）；
- `docker compose up -d --build` 会启动 wb2api + PostgreSQL 16 + Redis 7。

验证：

- `go test ./...`：通过；
- `go test ./internal/platform/redisx`（miniredis Lua 槽位）：通过；
- `TestStickySessionEscapesSlowAccount`、`TestStatsEscapesSlowAndRecovers`：通过；
- `docker compose config`：通过；
- 本机没有 Docker daemon，**没有实际启动 compose 栈**；这一步需要在有 Docker
  的机器上补做。无 DSN 的 standalone 模式已用浏览器 smoke 回归。

尚未完成（后续阶段）：SQLite usage 迁 PostgreSQL、top-K 负载评分、Redis 等待队列、
面板会话/quota/affinity/jobs 的共享存储。详见 `docs/ARCHITECTURE_MIGRATION.md`。
