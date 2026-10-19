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
| `internal/gateway/openai` | `/v1/chat/completions` 流式+非流式、鉴权/Key 绑定/模型限制、429/401/402/403/5xx 的换号策略、SSE 管道、用量事件 | 14 个行为测试（见下） |
| `cmd/hub` `cmd/mockup` `cmd/loadtest` | 网关雏形、模拟上游、压测器 | 手动跑通 |

`go vet`、`gofmt` 干净；`go test ./...` 全绿。本机没有 C 编译器，`-race` 跑不了，需在 CI（Linux）补。

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

### 这些数字**不能**证明什么
- **TTFT（首字延迟）这次没有测准。** 压测器、模拟上游、网关同机运行，Windows 回环上瞬间建立几百条新连接本身就要几百毫秒（压测器直连模拟上游、不经过网关，在无 ramp 的 100 并发下也是 ~280 ms）。网关内部从进入处理到首个上游 chunk 的耗时（hub 侧）是 p50 ≈ 258 ms，其中大部分是网关→上游新建连接。需要在 Linux 上、用连接复用或独立机器重测。
- 2000 并发时压测器自身和模拟上游已接近饱和（直连模拟上游时 p1 也只有 39.6），所以 2000 档的"每流速率"主要反映测试环境，不是网关上限。
- 没有和 Python 版做同机同条件的对照压测（Phase 0 的"Python 基线"还没跑）。

### 压测中发现并修掉的真实问题
`settings.Store` 最初每次读取都 `os.Stat` 一次 `settings.json`。在 Windows 上 500 并发时鉴权一步的 p99 达到 **~400 ms**（修复前实测），改成"至多每 500 ms 检查一次文件变更 + 快照修订号 + 解析后的 API Key 按修订号缓存"后降到 < 1 ms。旧 Python 的 `wb_settings.load()` 也是每次调用 `os.stat`、且请求热路径上约 10 次——这是否同样造成尾延迟**未在 Python 上实测**，但值得在 Phase 0 基线里确认。

## 与旧版有意不同的地方（需要你认可）

1. **泄漏过滤器默认用 Precise 策略**：只扣留"确实可能是工具调用"的标签（精确标签名或还没写完的前缀），`<thinking>`、`<think>`、`<args>`、`<params>` 之类普通文本立即放行；完整的 DSML 开标签（旧版因大小写正则 bug 根本不会扣留）现在会被正确扣留并恢复。`WB_LEGACY_LEAK_FILTER=1` 可切回旧行为。全部恢复场景（hybrid / DSML，整块与逐字符）在两种模式下都有测试。
2. 缺失的 `usage` 现在**标记为估算**，且 `prompt_tokens` 记 0，不再写成 `completion//2`。
3. 扣留文本有墙钟上限（默认 20 s，旧版只有 256 KiB 上限，等同于"到流结束"）。
4. 上游 chunk 正常时**保留上游的 id/created**，只有被过滤器改写的文本才合成新 chunk（旧版总是合成 `chatcmpl-wb`）。
5. 响应头在上游返回 200 后立即发出（旧版同样如此，因 Python 的 wfile 无缓冲）。
6. 换号逻辑**没有** `sleep` 退避；每次尝试的等待上限由分层超时决定（响应头默认 45 s，旧版 120 s）。
7. `created_at` 缺失时 API Key 条目保持空串（旧版每次读取都会盖当前时间，导致值不稳定）。
8. 账号文件里的 JSON 对象键顺序：嵌套对象（如 `credits`）按字母序写回，旧版保持插入序。语义相同。

## 还没做（按计划书优先级）

**Phase 2 网关热路径剩余**
- `build_upstream_body`：系统提示词模式、推理档位映射、`stop`/`max_tokens` 归一、Responses→Chat 转换、工具参数归一……
- 出站身份：`wb_identity` 的三种身份（桌面端/VSCode/CLI）头、gzip 请求体、`X-Conversation-*` 派生。**目前网关只发 Bearer / X-User-Id / X-Domain，不能直接打真实上游。**
- token 刷新（后台协程 + 抖动 + 指数退避 + singleflight）与积分刷新。**目前就绪判断遇到临期 token 只是"不可用"，没人去刷新。**
- 调度偏好：手动权重 / 临期积分 / 剩余用量加权轮询、会话亲和、`apply_daily_guards` 四条限额护栏、风控连坐保护、窗口探针。
- `usage` 持久化（SQLite 单写协程）与内存滚动计数；`/v1/models`、`/v1/balance-summary`、`/health` 的完整语义、`/u/<ref>/v1/*`。
- 90% 冷却时的选号从线性扫描改为就绪集合。
- usage 里 `normalize_usage_cache_aliases` 的归一尚未移植（目前原样透传 usage）。

**Phase 3+**：面板鉴权（Cookie 会话落库）、`/api/v2`、SSE 事件总线、新前端、统计预聚合、各类自动化任务……（见计划书）。

## 怎么跑

```bash
go test ./...                         # 全部测试
py -I tools/genvectors/gen.py         # 用 legacy/python 重新生成黄金向量
go build -o bin/ ./cmd/...            # hub / mockup / loadtest

bin/mockup -addr 127.0.0.1:19000 -tokens 200 -tps 50
bin/loadtest genaccounts -dir run/accounts -n 50 -key sk-test
bin/hub -port 18788 -accounts-dir run/accounts -upstream-override http://127.0.0.1:19000 -debug
bin/loadtest -url http://127.0.0.1:18788/v1/chat/completions -c 500 -key sk-test -ramp 5s \
             -stats http://127.0.0.1:18788/debug/runtime
```

## 版本控制说明
当前在分支 `v2-go`。除第一个脚手架提交外，之后的改动都**只暂存（`git add`）、没有提交**，等你确认再提交。
