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

## 6. 结论与注意

- 三套协议在真实上游上全部跑通，流式事件序、工具调用、`count_tokens`、定址路由、错误透传均符合预期。
- 单账号 12 并发、225 RPM 无 429；更长时间/更高 RPM 的账号配额行为**未测**，不要据此推断账号上限。
- 模拟基准证明网关在 200 并发流、8k TPS 量级下无错误、无 503；数字受同机 mock pacing 限制，不能当作网关上限。
- 真实账号测试会消耗积分/额度并可能触发上游风控，`account-ops.ps1` 的 `-FullGrowth` 与高并发请按需使用。

## 7. 复现

```powershell
# 模拟并发基准（自带 mock 与假账号池）
pwsh tools/bench/bench.ps1

# 真实账号协议矩阵（网关需已导入账号并建好 Key）
pwsh tools/livetest/compat.ps1 -BaseUrl http://127.0.0.1:18888 -ApiKey sk-... -PanelPassword ...

# 真实账号功能扫一遍
pwsh tools/livetest/account-ops.ps1 -BaseUrl http://127.0.0.1:18888 -PanelPassword ... -Model hy3
```
