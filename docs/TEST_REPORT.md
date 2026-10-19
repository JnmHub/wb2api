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

覆盖并通过：登录、账号 / 用量 / 设置 / 日志 / 任务五个页面、浅色→深色切换、账号详情抽屉、添加账号 / 导入 / 导出 / 桌面导入弹窗、新建 Key 弹窗、日志暂停/继续；同时收集 `Runtime.exceptionThrown`、`Log.entryAdded`、`console.error`，忽略登录前的 401 session 探测。截图输出到 `.scratch/uitest*/`（不提交）。2026-10-20 在真实 CN 账号上通过。

## 9. 复现

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
