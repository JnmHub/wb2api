# 高并发性能与正确性修复记录（2026-10-22）

## 范围和状态

> 本文是第一批开发完成时的历史记录；当前实现、验证和发行状态以[剩余工作完成记录](PERFORMANCE_REMAINING_2026-10-22.md)及 [v2.4.0 更新说明](releases/v2.4.0.md)为准。

基线：`v2.3.8 / 3fb5b767180d8cbbf4b20c94c523947f833931e9`。本轮针对审计中可复现的热点和正确性问题修改代码，保留 Chat / Responses / Messages、工具调用、会话亲和、固定账号、配额、限速等既有路径。没有部署、重启生产服务、提交、推送或发布。

这是一次已实现的修复批次，不是“所有架构瓶颈均已消失”的结论。性能证据来自 Windows 本地测试/微基准，不代表真实上游或生产 HA 的延迟。

## 1. 已实现的修改

### 三协议流式首包、刷新与空闲超时

- [relay_io.go](../internal/gateway/openai/relay_io.go)：首个有语义的输出立即刷新；之后保留批处理。真实上游 Read 前刷新已有完整帧，避免下一行只到达一个前缀时阻塞已经完成的内容。
- [stream.go](../internal/gateway/openai/stream.go)、[responses.go](../internal/gateway/openai/responses.go)、[messages.go](../internal/gateway/openai/messages.go)：统一使用上述读写边界，不再以 `br.Buffered()==0` 作为唯一刷新条件。恰好填满缓冲区和大帧也会正确执行 HTTP flush。
- [pacer.go](../internal/gateway/openai/pacer.go)：真正需要 sleep 时先刷新，不给第一片内容额外等待；追赶进度时不逐片强制刷新。
- 上游 idle 只计算实际 Read 等待，不再把主动 TPS 限速或下游写入算作上游空闲。旧定时器回调不能错误取消后续读；超时关闭返回 EOF 时仍判定失败，不能伪造成功结束。
- Responses 复用已解析的 Chunk，减少重复解码/重新编码；错误、DONE、心跳及 Web 工具后续轮次路径保留。

回归：[relay_io_test.go](../internal/gateway/openai/relay_io_test.go)、[pacer_test.go](../internal/gateway/openai/pacer_test.go)。

### 亲和缓存

- [affinity.go](../internal/pool/affinity.go)：有界 LRU，避免达到默认 5000 会话后每次 Bind 都持锁扫描整个缓存；正缓存、空结果、错误缓存与版本状态受容量控制。
- 同键共享读取合并；空结果/失败有短缓存，避免新会话重复同步访问 Redis。新增可取消的 `RecentContext`，并接入 [pick.go](../internal/pool/pick.go) 与网关调用路径。
- 排队 Bind 作废检查、同键 Demote 屏障与执行中旧 Bind 的补偿，避免本进程已知的旧写覆盖降级结果。队列有界，工作协程排空后退出。
- 重复绑定现有会话不再分配新切片。

回归：[affinity_regression_test.go](../internal/pool/affinity_regression_test.go)、[affinity_capacity_test.go](../internal/pool/affinity_capacity_test.go)、[pick_context_test.go](../internal/pool/pick_context_test.go)。

**边界：**未改变共享存储协议；这不是跨副本原子 fencing。超时、模糊提交或进程退出仍可能妨碍补偿。选择阶段仍会发布亲和关系，尚未改为仅在实际使用后提交。

### Redis 槽位与动态 Key 并发

- [limiter.go](../internal/platform/redisx/limiter.go)：清理过期成员后，已存在的同一 member 可幂等重试并刷新租约；不会因为上一次提交成功但响应丢失，被自己的槽位误判为已满。
- 续租不能复活已释放/过期的 member；保留请求生命周期与等待预算的分离。没有对模糊提交盲目执行 ZREM。
- [keylimit.go](../internal/gateway/openai/keylimit.go)：正并发上限变更不再替换计数器；稳定 active 计数、变更通知、幂等释放替代 25ms 轮询。
- 本地 limit=0 的请求也计入在途计数，重新开启限制时不漏掉旧持有者；已在等待的旧请求不会把上限改回旧参数。

回归含真实 TCP 丢弃 Redis 提交回包、竞争者限额、过期/降低限制、请求取消，以及真实 HTTP 的“无限制→限制 1”切换：[limiter_idempotency_test.go](../internal/platform/redisx/limiter_idempotency_test.go)、[limiter_recovery_test.go](../internal/platform/redisx/limiter_recovery_test.go)、[keylimit_dynamic_test.go](../internal/gateway/openai/keylimit_dynamic_test.go)、[keylimit_integration_test.go](../internal/gateway/openai/keylimit_integration_test.go)。

**边界：**Redis 不可用时的本地回退策略没有改变。共享成功计数与本地回退计数不是同一份账，不能承诺故障期间或跨模式切换仍满足集群硬上限。

### 等待重选与 DSML

- [acquire.go](../internal/gateway/openai/acquire.go)：等待阶段重查本次合格的满载候选，而非只盯住一个最优账号；固定账号仍只等待其持有者。多个共享候选使用单次至多 10ms 的准入预算，并检查同一个等待阶段总 deadline。失败、取消、健康变化及迟到成功均释放预留。实际 Redis 实现支持独立等待预算；旧的仅实现 `Acquire` 的插件不能强制中断单次阻塞调用，保留请求上下文以免提前停止租约续租。资源清理及操作系统调度也不计作硬实时返回保证。
- [dsml_hold.go](../internal/gateway/responses/dsml_hold.go)：增量扫描、不反复拼接整个未完成块；推测性扣留上限 256KiB，超限按原文释放而非截断。有效 DSML、自定义工具/namespace、原生结构化工具参数和最终文本一致性有回归覆盖。
- 20 秒 DSML 时间预算在下一 Chunk 检查；不是没有新数据也能独立发包的定时器。完整 Responses 结果仍按协议保留，不能把 256KiB 误解为总响应内存上限。超预算 DSML 按原文回退，不会事后重新解释已经发送的文本。

回归：[acquire_reselect_test.go](../internal/gateway/openai/acquire_reselect_test.go)、[dsml_hold_test.go](../internal/gateway/responses/dsml_hold_test.go)。

### 设置刷新与每日配额

- [settings.go](../internal/store/settings/settings.go)：合并并发刷新；冷读在初始读取进行中等待，而不是提前返回空快照，慢/错误/未变化结果供同批等待者共用。保留本地写立即可见、较新撤销不会被旧读覆盖、原子更新及未知字段往返。读取失败后的既有回退策略未改变，冷启动后端读取失败仍可能得到空设置，不应视为已实现鉴权 fail-closed。
- [guards.go](../internal/guards/guards.go)：读取用量失败时保留已知同日计数，再按最新设置、免费模型和余额重新计算限制；串行发布避免旧扫描覆盖新扫描；跨午夜不复用昨天的计数。关闭限制仍能立即清除对应限制。

回归：[refresh_coalesce_test.go](../internal/store/settings/refresh_coalesce_test.go)、[guards_test.go](../internal/guards/guards_test.go)。

**边界：**到期设置/用量刷新仍是同步 I/O，不是零 I/O 热路径。冷启动或新一天无法读取用量时仍沿用 fail-open；同日旧计数也无法涵盖故障期间新增用量。

### 不安全重发与公开错误

- [wire.go](../internal/upstream/wire.go)：只有确认没有请求字节可能发出，才允许复用连接的透明重试；部分写、写后读头失败、异常读体均可携带 `AmbiguousRequestError`。TLS 即使报告写入 0 个明文字节，也保守视为可能发送。
- [chat_loop.go](../internal/gateway/openai/chat_loop.go) 和 [client.go](../internal/ops/client.go)：识别不确定结果，停止换账号重试及付费模型回退，明确返回“结果未知，未自动重发”。明确 HTTP 拒绝的原有分类策略并未全面移除。
- [public_error.go](../internal/gateway/openai/public_error.go)：使用固定公开分类，不回显任意上游原文、内部地址、网络诊断或代理凭据。保留必要状态、已知错误码和重试提示；内部分类仍读取原始诊断。

回归：[wire_retry_test.go](../internal/upstream/wire_retry_test.go)、网关 [ambiguous_retry_test.go](../internal/gateway/openai/ambiguous_retry_test.go)、Ops [ambiguous_retry_test.go](../internal/ops/ambiguous_retry_test.go)、[public_error_test.go](../internal/gateway/openai/public_error_test.go)。三协议 × 流式/非流式 × 是否先出现 429 共 12 个网关案例验证只发送一次已被接受的 POST、无付费重放且只记一次终态用量。

**权衡：**上游可能其实没有执行的模糊失败也会返回错误；宁可让调用者看到未知结果，也不冒险自动重复计费。调用者自身的重试不受本轮控制。

### 用量写入失败重试

- [usagedb.go](../internal/usagedb/usagedb.go)、[retry.go](../internal/usagedb/retry.go)：失败批次不再直接清空。一个批次最多 256 个事件，按 worker 分成带随机 ID 的单元，仅重试未确认单元，不重放成功的兄弟单元。
- `usage_batches` 回执与请求明细、daily/hourly 汇总在同一事务提交；COMMIT 成功但确认丢失后的同 ID 重试不会重复计数。确认成功后进行有界回执清理。
- 单事务默认 10 秒写入期限涵盖取连接、锁、SQL；`Flush() error`、`Close() error` 报告失败/剩余排队数，`pending_retry` 暴露待重试事件数。旧的忽略返回值的调用语法仍兼容。
- Close 与 Record 有短锁 admission 屏障；关闭后记录计入 dropped，不会排到最终 drain 后面。hub 会记录最终关闭/刷盘错误。

故障注入：[retry_test.go](../internal/usagedb/retry_test.go)，覆盖失败恢复、部分 worker 成功、丢失提交确认状态、连接等待超时、缓冲有界、关闭与并发记录。这里使用 SQLite 触发器和“成功提交后保留同 ID 再重试”的状态模拟；不是 PostgreSQL 网络丢包/故障切换验证。

**重要：这仍不是持久化 outbox。**队列 drop/sample、进程崩溃、毒性事件阻塞后续数据等风险仍存在。当前计数不能作为严格不丢账的计费账本。每次事务有界不等于整个 Flush/Close 总耗时有界；回执清理失败或进程崩溃可留下孤立行。PG 的全局 advisory lock 和每事件三次 SQL 仍保留，未冒险删除死锁保护。

## 2. 性能证据

以下均为同一开发机上的定向复现，不是生产 SLA。首包使用客户端/下游首次刷新到有语义内容的时刻，不把只发送 HTTP 头或 setup 帧当作文本首包。

- 审计基线：三协议 40 片、100TPS，一次性送入内存流，首次语义刷新约 400ms；TPS=0 但下一行只到达前缀并暂停 150ms，已完成首片也被拖延约 150–163ms。
- 修复后最终定向复测：上述三协议首次语义刷新为 0–1.123ms；其中 `0s` 是本机计时分辨率内完成，不是物理零延迟。100TPS 的总耗时仍约 400–401ms，说明没有通过关闭限速换取首包改善。下一行前缀暂停仍持续约 150ms，但不再扣住前面已完成的语义输出。
- 真实本地 HTTP Chat fixture（40 片、100TPS、first-output guard 关闭）：客户端 TTFT 从审计基线 529.50ms 降到本轮 109.98ms，总耗时 510.61ms，40 片内容完整。guard=1s 时本轮客户端 TTFT 106.62ms、总耗时 507.19ms。是单次开发机复现，不是负载匹配的生产统计结论。详见 本地归档 `final-stream-results.txt`（未随源码分发）。
- 亲和 Bind 微基准：原 5000 会话约 39–43µs，20000 会话约 168–189µs；本轮最终复测 5000 为 38.47–38.74ns，20000 为 39.32–43.69ns，均 0 B/op、0 allocs/op。改前直接注入缓存，改后使用公开 Bind 填充并配置相应容量；这是单个缓存操作的比较，不能外推为整体 QPS 倍数。
- 普通 Pick 仍为随候选规模增长的扫描：本轮 100 账号约 0.89µs，1000 约 4.4–5.0µs，5000 约 15.7–17.0µs。没有声称优化掉整个饱和调度循环。
- 未闭合 DSML 新微基准：32KiB 约 77–92µs；256KiB 约 2.10–2.19ms；1MiB 约 6.79–7.31ms。包含转换器帧构造、编码与结果保留；没有匹配旧实现基准，不报告速度提升倍数。

原始测量：本地归档 `final-bench-results.txt`（未随源码分发）。

## 3. 验证与复现

最终集成通过：`git diff --check`、`go vet ./...`、`go test ./... -count=1 -timeout=6m` 全部 exit 0。完整日志：本地归档 `final-suite-results-v2.txt`（未随源码分发）。其中 OpenAI 包 140.909s、Redis 包 35.173s、上游包 6.785s；这些是测试运行耗时，不是请求延迟。

已完成定向回归包括：亲和 100 次、Redis 丢包/幂等相关 10 次、等待重选先前 30 次及最终版本 10 次、设置合并刷新 10 次、guard 30 次、用量故障注入 5 次、网关/Ops 不重放 5 次，以及 HTTP Key 动态切换 5 次。

验证中曾有两项时间相关失败：35ms 等待测试实际 304.574ms 超过 200ms 测试阈值；本地 TCP dial 超过 2 秒。原样分别复跑 30 次均通过，未据此认定已证明环境根因。等待测试随后改为行为断言：故意让第一候选返回时超过总期限，验证不继续探测/不准入，且迟到成功会释放；不再断言 OS 必须在某个墙钟阈值内调度。TCP 测试和生产超时未放宽。最终全量运行通过。

```powershell
git diff --check
go vet ./...
go test ./... -count=1 -timeout=6m
go test ./internal/gateway/openai -run '^TestKeyLimitIntegrationUnlimitedHolderSurvivesEnable$' -count=5
go test ./internal/usagedb -run '^TestUsageRetry' -count=5
go test ./internal/pool ./internal/gateway/responses -run '^$' -bench 'BenchmarkAffinityBindBounded|BenchmarkPick|BenchmarkDSMLUnclosed' -benchtime=300ms -count=2 -benchmem
```

- Go 1.27.1，Windows/amd64，Intel Core Ultra 5 225H。
- 未设置 `WB_TEST_POSTGRES_DSN`：依赖真实 PostgreSQL 的集成测试跳过；本轮没有访问生产数据库。
- Redis 使用 miniredis 和隔离的本地 TCP 故障注入，不是生产 Redis 压测。
- race 检测未完成：环境 CGO 禁用且没有 GCC/Clang；错误为 `go: -race requires cgo; enable cgo by setting CGO_ENABLED=1`。未安装工具链或把普通测试当作 race 通过。
- 原审计 overlay 中部分测试专门断言“缺陷存在”，不是修复后的回归标准；不要直接用旧 overlay 判断本轮失败。

## 4. 本批结束时的后续清单（历史）

以下是第一批结束时的状态，后续实现及验证已另记于[剩余工作完成记录](PERFORMANCE_REMAINING_2026-10-22.md)。上文关于本地回退、无跨副本 fencing、仅内存重试和尚无 Linux race 的边界同样只代表第一批当时的状态。

1. **严格可靠用量账本**：先把不可丢的配额/计费总量与可采样明细拆开，使用持久化 outbox 和幂等事件；再优化批量写入、预聚合与锁顺序。不能只增加 PG worker 或删除 advisory lock。
2. **饱和调度**：首次探测仍可串行尝试 N 个 Redis 账号，且重复选择造成扫描放大；本轮只修复后续等待阶段。需要批量容量提示/短 TTL/合并读取，再以最终原子 acquire 确认，兼顾固定账号及公平性。
3. **热路径刷新**：剩余额度估算仍可能在全局互斥下重扫 26 小时记录，PG 账号仍会周期性全表同步；应引入有版本的不可变快照/增量更新，不能倒退已修复的合并和撤销一致性。
4. **HA 故障语义**：Redis 失联的本地降级、租约续租丢失后的持有者行为、跨副本亲和 fencing，需要明确可用性与全局硬上限的取舍。
5. **指标口径和慢客户端**：历史内部 TTFT 不等于客户端首次语义帧；账号反馈仍混有排队/重试延迟。下游慢写没有独立滑动写 deadline，仍可长期占用槽位。需要分别采集上游首输出、网关排队、下游首次语义输出与取消延迟。
6. 在隔离的 Linux + PostgreSQL + Redis 环境完成 race、数据库故障恢复、真实 HTTP 高并发及匹配直连对照，再讨论发布。此次本地证据不能解释历史生产 HA 延迟或 API Key 丢失事件的根因。
