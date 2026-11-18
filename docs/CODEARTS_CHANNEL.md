# 接入华为云 CodeArts Agent 通道（可行性评估与落地方案）

> 目标：让 wb2api 的账号池里能同时存在 WorkBuddy 账号和华为云 CodeArts Agent 账号，
> 对外仍是同一套 `/v1/chat/completions`、`/v1/responses`、`/v1/messages`。
>
> 参照实现：`codexarts-source/codexarts`（module `codearts2api`，15002 行，零外部依赖）。

## 0. 结论

**能接，而且比想象的省事。** 两侧上游在 HTTP 形状上是同一类东西：

| 维度 | wb2api（现状） | codexarts |
| --- | --- | --- |
| 聊天端点 | `{base}/v2/chat/completions` | `{base}/api/v2/chat/completions` |
| base | `https://copilot.tencent.com`(cn) / `https://www.workbuddy.ai`(intl) | `https://snap-access.cn-north-4.myhuaweicloud.com` |
| body | 客户端 OpenAI body 经 `shape.Prepare` 整形 | `chatBodyV2()` 生成的 OpenAI body（model/stream/messages/tools/…） |
| 响应 | SSE，OpenAI chunk | SSE，OpenAI chunk |
| 鉴权 | `Authorization: Bearer <access_token>` + WorkBuddy 身份头 | `x-auth-token: <STS security_token>` + 华为云 `SDK-HMAC-SHA256` AK/SK 签名 |
| 凭据刷新 | 自有 refresh（`internal/refresh`） | OAuth2 `grant_type=refresh_token` + DPoP（与 client_id 绑定） |
| 模型目录 | 上游拉取 + 本地 catalog | 三源发现（agent-center / builtin / 福利网关）+ 30 分钟探测缓存 |

真正不同的只有三处：**路径前缀、鉴权头与签名、凭据刷新链**。这三处都是 codexarts 里已写好、
可以直接搬过来的代码（`signer.go` 140 行、`dpop.go`、`client.go` 的 `RefreshToken`）。

所以推荐 **路线 B：原生 provider 接入**，把 CodeArts 当成 wb2api 的第二种上游，
复用现有的选号、冷却、Key 白名单、会话亲和、三协议网关、用量账本。
路线 A（旁挂 codearts2api 当中转）只用来半天验证链路。

---

## 1. 两侧结构对照

### wb2api 的分层（44.5k 非测试行 / 27.3k 测试行）

```
ingress:  internal/gateway/{openai,responses,anthropic}   三协议 + 流式/工具/思考档位
调度层:   internal/pool                                  选号、冷却、配额、亲和、模型窗口
出口层:   internal/upstream + upstream/identity + tlsfp   自写 HTTP/1.1（逐字节控头）+ TLS 指纹
持久化:   internal/store/accounts (JSON) + usagedb (SQLite/PG) + platform/{pg,redisx}
运营:     internal/ops + internal/jobs + internal/sched    签到/成长任务/猫猫旅行（CN 专有）
```

关键接缝，全都在 4 个函数里：

| 接缝 | 位置 | 现状 |
| --- | --- | --- |
| 上游 base | `internal/upstream/identity/identity.go:100` `Endpoint(realm, product)` | 按 realm 返回 WorkBuddy 域名 |
| 上游路径 | `internal/gateway/openai/chat.go:40` `const ChatPath = "/v2/chat/completions"` | 全局常量 |
| 上游头 | `internal/gateway/openai/chat_loop.go:702` `(*Handler).chatHeaders` | `identity.ChatHeaders()` 生成 Bearer + 身份头 |
| 发送 | `internal/gateway/openai/chat_loop.go:626` `(*Handler).try` | `h.Up.Wire(...).Do(ctx, upstream.Request{...})` |

`try()` 的执行顺序正好够用：先 `enc, gz := q.bodyFor(product)` 拿到 body 字节，
再 `hdrs := h.chatHeaders(...)` 造头，最后 `enc.wire(gz)` 发出。
**签名需要的 payload hash 在这一刻已经可得**，只需把 body 传进 `chatHeaders`。

### codexarts 的分层

```
internal/upstream   client.go / signer.go / dpop.go / models.go / checkin.go / sse.go
internal/auth       auths/codearts-{user_id}.json 凭证文件
internal/pool       单账号并发、冷却、刷新
internal/server     三协议 + 面板
```

它本身就是参考 wb2api 写的（见其 `docs/wb2api-adaptation.md`），所以协议适配层几乎是同构的，
差异只在上游。

---

## 2. 华为云 CodeArts Agent 到底提供什么

从 `const.go` + `models.go` + README 逆向出来的能力面：

### 2.1 产品本体

华为云 **CodeArts Agent**（盘古大模型 / 「码道」），官方形态是 VS Code 插件
`vscode-codebot` + 云端 Agent。云端 API 已经完全脱离客户端可直连：

| 端点 | 用途 |
| --- | --- |
| `POST /api/v2/chat/completions` | 主聊天（OpenAI 兼容，SSE） |
| `POST /v1/chat/chat` | 旧版聊天 |
| `GET /v1/agent-center/agents/useragents` / `detail` | 用户自建 Agent 与模型注册表 |
| `GET /v1/model/builtin` | 内置模型目录（`Agent-Type: PromptCenter`） |
| `GET https://opengw.developer.huaweicloud.com/api/v1/gateway/config` | 限时福利套餐模型 |
| `POST .../api/v1/benefit/claim` | 领取福利（幂等） |
| `POST /v1/oauth2/tokens` | OAuth2 换 STS 临时 AK/SK |
| `POST /v1/login/ticket` | 无浏览器登录的 ticket 轮询 |
| `GET /v5/caller-identity` | 账号自检 |

### 2.2 模型

精确 ID 区分大小写，`/v1/models` 同时给一条小写别名：

- `GLM-5.2` / `glm-5.2`（默认模型）
- `Qwen3-VL-235B`（多模态）
- `deepseek-v4.1-flash-1017`、`deepseek-v4-flash-0731`（是两个不同 ID，不互相替换）
- `OpenPangu-2.0-Pro`、`GLM-5.2-ArkTS-SPARK` —— 目录里有但账号没注册，调用返回
  `InferHub.002002009.404`，所以服务会用一条最小请求探测并缓存 30 分钟
- **福利模型**：随免费套餐轮换；调用必须带 `maas_type: benefit` 头，
  且必须先「领取」（否则 `InferHub.4004.200 benefit not found`）。领取是幂等写操作，
  官方客户端打开模型菜单时也会调，所以 codexarts 默认自动领取。

### 2.3 积分与风控

- 每日签到 **1000 积分**（30 天有效，活动截止 2026-12-31），新用户 **4000 积分**
- 错误语义：`InferHub.001001005` 参数错误（不换号、不冷却）、`TM.000010051` 无权调用该模型、
  `InferHub.002002009.404` 未注册
- 单账号并发默认 `max_concurrent: 1`

---

## 3. 差异矩阵（决定改动量的那张表）

| 项 | 需要改 | 说明 |
| --- | --- | --- |
| base URL + path | 是 | `/api/v2/chat/completions` vs `/v2/chat/completions` |
| 请求头集合 | 是 | `x-auth-token` / `app-id: CodeAgent3.0` / `is_confidential` / `x-snap-traceid` / `X-Language`，**没有** WorkBuddy 的 `X-User-Id`、UA 指纹头 |
| 签名 | 是 | 全部头参与 `SDK-HMAC-SHA256`；`X-Sdk-Date`、`X-Security-Token`、`X-Sdk-Content-Sha256`、`Authorization` |
| body | 否（基本） | 需去掉 WorkBuddy 私有字段，保留 model/stream/messages/tools/tool_choice/max_tokens/temperature/top_p/reasoning_effort/prompt_cache_key |
| 刷新 | 是 | OAuth2 refresh + DPoP（client_id 与私钥绑定，换 client 会 `invalid client id`） |
| 模型目录 | 是 | 三源发现 + benefit 标记 + 探测缓存 |
| 每日任务 | 是 | CodeArts 只有签到（积分），没有成长任务/猫猫旅行/动态专家 |
| 并发 | 是 | 单账号默认 1，需要在池里按 provider 覆盖 |
| 404/错误码映射 | 是 | 见下 |
| TLS 指纹 | 否 | 华为云不吃 OpenSSL 指纹伪装，用普通 transport 即可（Wire 可保留） |

---

## 4. 路线 A：旁挂中转（半天，用于验证）

不改 codexarts，让 wb2api 把 CodeArts 当成第三方 OpenAI 上游：

```yaml
providers:
  codearts:
    base: http://127.0.0.1:7866      # codearts2api 实例
    key: CA2A_API_KEY
    models: [glm-5.2, qwen3-vl-235b, deepseek-v4.1-flash-1017]
```

改动点（约 300–500 行）：

1. `accounts.Record` 增加 `Provider string`（`""`→workbuddy 保持兼容）
2. `identity.Endpoint` 与 `chatURL` 按 provider 分派
3. `chatHeaders`：provider 为 codearts 时只发 `Authorization: Bearer <key>` + `Content-Type`
4. `pool.Pick` 增加 provider 过滤（模型→provider 路由表）
5. `/v1/models` 合并 CodeArts 的模型列表

**代价**：账号池、配额、冷却、账本被切成两套；面板看不到 CodeArts 账号；
两层 failover 语义互相打架（wb2api 换号 → codearts2api 也换号）。

只适合「先跑通再说」。

---

## 5. 路线 B：原生 provider（推荐，3–5 天）

### 5.1 新增包

```
internal/upstream/codearts/
  const.go      端点常量（搬 codexarts/internal/upstream/const.go）
  signer.go     SDK-HMAC-SHA256（原样搬，140 行）
  dpop.go       ES256 DPoP（搬）
  oauth.go      PKCE authorize / exchange / refresh / ticket 轮询（搬）
  chat.go       body 构造 + SSE 预检 + 发送
  models.go     agent-center / builtin / benefit 三源发现 + 探测缓存
```

### 5.2 账号凭据：不碰 schema

`accounts.Record` 已有 `Extra map[string]any`，读写时原样保留（`store.go:165` 会合并回去）。
CodeArts 凭证全部塞进去，零迁移：

```jsonc
{
  "uid": "hw-1234", "realm": "cn", "provider": "codearts",
  "extra": {
    "access_key_id": "…", "secret_access_key": "…",
    "security_token": "…",            // = x-auth-token
    "expiration": "2026-11-17T08:00:00Z",
    "refresh_token": "…", "client_id": "codearts-agent",
    "code_verifier": "…",
    "dpop_private_key": {"kty":"EC","crv":"P-256","d":"…","x":"…","y":"…"}
  }
}
```

`Record` 里 `AccessToken` 复用为 `security_token`，`ExpiresAt` 复用为 `expiration` 的 unix 秒，
这样现有的「到期前刷新」逻辑（`refresh` 包）不用改判断条件，只改刷新实现。

### 5.3 接缝改造（文件级清单）

| 文件 | 改动 |
| --- | --- |
| `internal/store/accounts/record.go` | 加 `Provider string`（空=workbuddy） |
| `internal/realm/realm.go` | 加 `ProviderCodeArts = "codearts"`；`realm.Get` 保留，新增 `provider.Get` |
| `internal/upstream/identity/identity.go:100` | `Endpoint` 增 provider 参数，codearts 返回 `snap-access` base |
| `internal/gateway/openai/chat.go:40` | `ChatPath` 改成函数 `chatPath(provider)` |
| `internal/gateway/openai/chat_loop.go:702` | `chatHeaders` 按 provider 分派；签名需要 body → 加参数 `wireBody []byte` |
| `internal/gateway/openai/chat_loop.go:626` | `try()` 把 `enc.wire(gz)` 先算出来传进 `chatHeaders` |
| `internal/gateway/openai/chat_loop.go:723+` | 错误分类：`001001005`→不换号不冷却；`002002009`/`TM.000010051`→账号能力问题；`4004.200`→先 claim 再重试一次 |
| `internal/refresh/*` | `Refresh` 分派：codearts 走 `/v1/oauth2/tokens` refresh，DPoP 复用 `client_id` + 私钥 |
| `internal/pool/pick.go` | 选号加 provider 过滤（模型→provider，见 5.4） |
| `internal/pool/pool.go` | `AccountInflight` 按 provider 覆盖为 1 |
| `internal/jobs/*` | `provider == codearts` 只排签到（积分），跳过成长任务/猫猫旅行 |
| `internal/api/accounts.go` | 导入接口接收 CodeArts 凭证 JSON（字段校验照抄 codexarts `server/import.go`） |
| `internal/gateway/openai/models.go` | `/v1/models` 合并 CodeArts 目录，福利模型带 `benefit: true` |
| `internal/pricing` | 积分口径与 WorkBuddy 配额分开记账 |

### 5.4 模型路由：前缀还是路由表

两边模型名有重叠（`glm-5.1`、`deepseek-v4.1-flash` 都可能同时存在），
`realm.DetectModelRealm` 会把它们判给 CN，所以**必须显式路由**，不能靠猜：

```jsonc
"model_routes": {
  "hw/glm-5.2":            {"provider": "codearts", "model": "GLM-5.2"},
  "hw/qwen3-vl-235b":      {"provider": "codearts", "model": "Qwen3-VL-235B"},
  "hw/deepseek-v4.1-flash":{"provider": "codearts", "model": "deepseek-v4.1-flash-1017"}
}
```

别名解析在 `realm` 别名那一层之前完成（和现有 `claude-*` → 实 id 的映射同一个入口），
返回体的 `model` 仍然回写客户端请求名。

### 5.5 签名落地要点

`Wire` 是「逐字节控头」的自写 HTTP/1.1 客户端，正好适合做全头签名，但有三个坑：

1. **签名覆盖全部头**：`canonicalHeaders` 遍历 `req.Header`。如果 `Wire` 在签名后又加了头
   （或改了 `Content-Encoding`），签名语义不变但 `SignedHeaders` 不含它——
   codexarts 是签名后加 `Chat-Id`/`Session-Id`，说明上游只校验 `SignedHeaders` 列表内的头。
   照做即可，但 `Content-Length`/`Host` 必须在签名前确定。
2. **gzip**：wb2api 对非 desktop 产品会 `Content-Encoding: gzip`（`bodyFor` 的第二个返回值）。
   签名的 `payload hash` 必须是**压缩后**字节，否则 `X-Sdk-Content-Sha256` 与 body 不符。
   建议 codearts provider 强制 `gz=false`，省掉这层。
3. **每请求重签**：`X-Sdk-Date` 精确到秒且参与签名，`chatHeaders` 缓存不了，
   但成本只是一次 HMAC-SHA256。

### 5.6 错误码映射表

| 上游 | HTTP | 归类 | 动作 |
| --- | --- | --- | --- |
| `InferHub.001001005` | 400 | 请求参数错误 | 原样返回，**不换号、不冷却** |
| `InferHub.002002009.404` | 404 | 模型未注册 | 记账号能力问题，冷却该模型 30 分钟 |
| `TM.000010051` 无权调用该模型 | 403 | 权限 | 同上 |
| `InferHub.4004.200` benefit not found | 400 | 福利未领取 | 自动 claim 后重试一次，仍失败则记模型冷却 |
| 401 / STS 过期 | 401 | 凭据 | 走 refresh，失败再换号 |
| 429 | 429 | 限流 | 复用现有 `CoolDownModel` |

### 5.7 签到（唯一可复用的运营能力）

CodeArts 的签到是「领取积分」，与 WorkBuddy 的签到/成长任务语义不同：

```go
// internal/upstream/codearts/checkin.go
func (c *Client) FetchCreditCampaigns(ctx, cred) ([]CreditCampaign, error)  // 搬
func (c *Client) Checkin(ctx, cred) (CheckinResult, error)                  // 领取 + 确认
```

建议接入 `internal/jobs` 的 `checkin` 整点表，`provider == codearts` 时只跑这一个任务；
`credit.sh` / `apply.sh` 的逻辑（查登录态、仅续期）可作运维脚本参考，不必进主程序。

---

## 6. 验证清单

```bash
# 1. 编过
go build ./... && go vet ./...

# 2. 签名单测（对齐官方 SDK 的固定向量）
go test ./internal/upstream/codearts -run TestSigner -count=1

# 3. 双 provider 选号
go test ./internal/pool -run TestPickProvider -count=1

# 4. 三协议不回归（不监听端口）
go test ./internal/server -run '^TestCompat' -count=1

# 5. 真实链路（先跑 codexarts 的 probe 拿模型目录）
go run ./cmd/models            # 在 codexarts 目录，确认账号真实可用模型
curl -s http://127.0.0.1:8788/v1/models -H "Authorization: Bearer $WB_KEY" | jq '.data[].id'
curl -s http://127.0.0.1:8788/v1/chat/completions \
  -H "Authorization: Bearer $WB_KEY" -H 'Content-Type: application/json' \
  -d '{"model":"hw/glm-5.2","messages":[{"role":"user","content":"你好"}],"stream":true}'
```

---

## 7. 建议的落地顺序

1. **第 1 步（半天）**：跑起 codexarts 实例，确认账号能签到、能聊天、`/v1/models` 有货
2. **第 2 步（半天）**：路线 A 打通 wb2api → codearts2api，验证三协议与流式
3. **第 3 步（3–4 天）**：路线 B：搬 signer/dpop/oauth，接 `chatHeaders` + `try()`，加 provider 路由
4. **第 4 步**：模型目录合并、签到任务、面板显示 provider 列

风险最高的不是签名，是**冷却语义**：WorkBuddy 的 429/6004 与 CodeArts 的积分耗尽
不是同一件事，混在一起会让健康账号被误停放。

---

## 8. 方案 C：把渠道做成第三个 realm（推荐，比新增 provider 维度省）

### 8.1 为什么可行

`realm` 在 wb2api 里其实已经承担了「出站渠道 + 该渠道的账号集合」两个职责：

- `pool` 的 `byRealm` 是普通 `map[string][]*Account`（`pool.go:315/319`），**加第三个值不需要改任何代码**
- `RealmLen`、`affinity`、`modelcool` 的按 realm 聚合都是 map 取值，天然支持
- API Key 已经有 `Realm` 字段（`settings/apikeys.go:15`），请求侧优先级链也已存在：
  `requestRealm` = key.Realm → `X-Realm` 头 → `?realm=` → pinned 账号 → 面板当前值

所以「密钥选渠道」不是新功能，是把已有的 realm 绑定扩一个枚举值。

### 8.2 但必须先拆掉 4 个静默回落

不改这些，`realm=codearts` 会**悄悄按国际版处理**：发到 `www.workbuddy.ai`、带 WorkBuddy 身份头、
用 WorkBuddy 的模型目录，错误现场极难定位。

| # | 位置 | 现状 | 后果 |
| --- | --- | --- | --- |
| 1 | `realm/realm.go:Get` | 未知 realm 回落 Intl | 上游打到 workbuddy.ai |
| 2 | `gateway/openai/pinned.go:77` | `case realm.Intl, realm.CN` 白名单 | key 绑 codearts 被忽略，回落面板值 |
| 3 | `settings/manage.go:50` | 只接受 `intl|cn|""` | 面板存不进 codearts |
| 4 | `catalog/fetch.go:69,164` | `r != Intl && r != CN` 直接返回 | codearts 永远没有模型目录 |

加固：加 `realm.Valid(r)`，在 `pool.New` 建池时校验，写错值直接启动失败，而不是运行期漂移。

### 8.3 建议的形状：realm 加值 + Family() 分流

```go
// internal/realm/realm.go
const (
    Intl     = "intl"
    CN       = "cn"
    CodeArts = "codearts"   // 华为云 CodeArts Agent，只有 cn-north-4 一个区域
)

// Family 把 realm 归到上游族，所有 WorkBuddy 专有逻辑都按 Family 判，
// 而不是 realm 字符串等值——这样以后加第二个华为区域不用再改一遍。
func Family(r string) string {
    if r == CodeArts { return "codearts" }
    return "workbuddy"
}

func Valid(r string) bool { return r == Intl || r == CN || r == CodeArts }
```

`Get(CodeArts)` 返回：`ChatUpstream = https://snap-access.cn-north-4.myhuaweicloud.com`、
`HasCheckin = true`（积分签到，语义不同，见 8.6）、`Domain = ""`。

### 8.4 改动清单（按文件，estimate 1 天出可跑版本）

| 文件 | 改动 |
| --- | --- |
| `realm/realm.go` | `+CodeArts`、`+Family/Valid`、`Get` 分支、`ExclusiveRealm` 对 codearts 模型返回 `""` |
| `settings/manage.go:50` | 白名单加 `codearts` |
| `gateway/openai/pinned.go:77` | `requestRealm` 白名单加 `codearts` |
| `gateway/openai/pinned.go:98` | `crossRealmMessage`：`realm.Family(owner) != realm.Family(reqRealm)` 时不拦（否则 codearts key 请求 `glm-5.1` 会被误报「只在国內版提供」） |
| `upstream/identity/identity.go:100` | `Endpoint` 分支到 snap-access base；codearts 不需要 WorkBuddy 的 `X-User-Id`/UA 指纹头 |
| `gateway/openai/chat.go:40` | `ChatPath` → `chatPath(realm)`，codearts 用 `/api/v2/chat/completions` |
| `gateway/openai/chat_loop.go:702` | `chatHeaders` 按 Family 分派；codearts 分支要拿到 body 做签名 → 加参数 `wireBody []byte` |
| `gateway/openai/chat_loop.go:626` | `try()` 里 `enc.wire(gz)` 先算出来传给 `chatHeaders`；codearts 强制 `gz=false` |
| `refresh/*` | 到期刷新按 Family 分派（codearts 走 oauth2/tokens + DPoP） |
| `catalog/fetch.go` | codearts 走自己的三源发现，不套 WorkBuddy 的 catalog |
| `api/tasks.go`、`ops/daily.go` | 按 `Family()=="workbuddy"` 才跑成长任务/猫猫旅行；codearts 只跑积分签到 |
| `api/desktop.go:100`、`realmswitch` | 面板 realm 归一化加 codearts |
| `web/` | 面板渠道下拉多一项；账号列表显示渠道标签 |

`pool` / `pick` / `affinity` / `usagedb` **零改动**——它们只认 realm 字符串。

### 8.5 密钥选渠道的最终形态

```jsonc
// 面板「API Key」页
{ "name": "华为专用", "realm": "codearts", "models": ["glm-5.2", "qwen3-vl-235b"] }
{ "name": "国际主力", "realm": "intl",     "models": ["gpt-5.6-sol"] }
{ "name": "随便用",   "realm": "",         "models": [] }   // 跟随面板切换
```

客户端也可以临时覆盖：`X-Realm: codearts` 或 `?realm=codearts`。

**一个 key 一次只能选一个渠道**（realm 是单值）。想让同一条 key 混用两边的模型，
才需要模型前缀路由（`hw/glm-5.2`）——建议先不做，等真有需求再加。

### 8.6 剩下的语义问题（改代码解决不了，要在面板上说清）

1. **模型名撞车**：`glm-5.1`、`deepseek-v4.1-flash` 两边都有。选了 codearts 渠道就只发华为，
   `realm.ExclusiveRealm` 必须对 codearts 放行，否则误报。
2. **签到不同义**：WorkBuddy 是「签到 + 成长任务 + 猫猫旅行」，CodeArts 是「领 1000 积分」。
   面板文案要分开，别复用同一个「签到」开关的语义。
3. **计费口径**：WorkBuddy 看配额/积分余额，CodeArts 看积分（每日 1000）。
   `internal/pricing` 的估算不能混用同一套价格表。
4. **并发**：CodeArts 单账号 `max_concurrent=1`，wb2api 池默认高并发 →
   按 Family 把 `AccountInflight` 覆盖为 1，否则一上量全是 429。
5. **凭据刷新失败语义**：CodeArts 的 `refresh_token` 与 `client_id` + DPoP 私钥绑定，
   迁账号必须整包搬（`dpop_private_key` 丢了就再也刷不动，只能重新登录）。

### 8.7 工作量

| 阶段 | 内容 | 时间 |
| --- | --- | --- |
| P0 | realm 三值 + 白名单 + Endpoint/Path + 签名 + 单账号非流式 | 1 天 |
| P1 | 流式 + 换号 + 冷却 + 模型目录 | 1 天 |
| P2 | 刷新链（DPoP）+ 签到 + 面板 | 1 天 |
| P3 | 测试补齐（signer 固定向量、双渠道选号、三协议回归） | 1 天 |

比新增 `provider` 维度少动约 6 个文件（`settings` schema、`pick` 过滤、`Record` 字段、
`api/accounts` 视图、面板列、Key 绑定 UI），代价是 `realm` 这个名字从此表示「渠道」而非「地域」。

---

## 9. 已落地（P0/P1/P2）

### 9.1 渠道标识

```go
realm.CodeArts       // "codearts"
realm.Family(r)      // "workbuddy" | "codearts"
realm.Valid(r)       // intl / cn / codearts
realm.IsCodeArts(r)
realm.CodeArtsChatBase // https://snap-access.cn-north-4.myhuaweicloud.com
```

`realm.Get("codearts")` 返回 snap-access 出口、空 `Domain`、`HasCheckin: true`（语义是积分，不是 WorkBuddy 签到）。

四个静默回落已拆：`realm.Get`（显式分支）、`requestRealm`（网关与 /v1/models 两处白名单）、
`settings.KeyInput.validate`、`roleswitch`/`realmswitch.valid`。

### 9.2 新增包 `internal/upstream/codearts`

| 文件 | 内容 |
| --- | --- |
| `const.go` | 端点、`CLIENT_ID=codearts-agent`、`maas_type` 常量 |
| `signer.go` | SDK-HMAC-SHA256（全头签名、canonicalURI 补尾斜杠、SignedHeaders 排序） |
| `dpop.go` | ES256 DPoP proof（低 S 归一化）+ 私钥恢复 |
| `oauth.go` | `grant_type=refresh_token` 换 STS；终态失效返回 `ErrRefreshExpired` |
| `credential.go` | 凭证读写（嵌套/扁平两种 Extra 布局）、`ApplyToken`（不改原记录）、`ModelsOf`、`IsBenefitModel` |
| `chat.go` | 请求体白名单 + 强制 `stream:true`、签名头、错误分类 |
| `checkin.go` | 积分投放/领取/确认（`/v1/ops/*`，`channel=IDE`，活动 1=签到、4=新手） |
| `models.go` | 三源模型发现（agent-center / builtin / 福利网关）+ 福利领取 + 合并 |
| `login.go` | PKCE、门户授权链接、`authorization_code` 换取、ticket 轮询、账号身份 |

`internal/codeartsync`（新包）把协议层接到账号池：`Service.Discover / Checkin / CanCheckin`
与后台 `Scanner`，面板、调度器共用同一实现。

签名器有固定向量测试（`signer_test.go`），向量由独立的 Python 实现按 SDK 规范算出，
所以 canonical 规则、大小写、头排序任一改动都会红。

### 9.3 网关接缝

- `chatPathFor(realm)`：CodeArts 用 `/api/v2/chat/completions`
- `(*Handler).buildOutbound`：CodeArts 走 `codearts.PrepareBody` 且**不压缩**（payload hash 即线上字节）
- `(*Handler).chatHeaders`：CodeArts 分支直接签名，不带 `X-User-Id`/UA 指纹
- `classify`：`InferHub.001001005` → 原样 400、不换号不冷却；`002002009`/`TM.000010051`/`4004.200`
  → 只给该模型 30 分钟冷却，继续换号
- `/v1/models`：CodeArts 渠道的目录来自账号记录（`codearts.models`，`benefit` 标记保留），不套 WorkBuddy catalog
- `refresh`：`realm.IsCodeArts` 时走 OAuth2+DPoP 链，`ErrRefreshExpired` 映射为 `ErrNeedsLogin`
- 签到：`upstreamAction` 的 `checkin` 按渠道分派，结果写入 `LastCheckin`；批量过滤 `filterBatchRealm`
  放行 CodeArts；调度器新增 `Channel` 钩子，按 `checkin` 整点表自动跑（`ops.CanCheckin` 仍只认 CN）
- **模型自动发现**：`internal/codeartsync` 的三源发现（agent-center / builtin / 福利网关），
  写回 `codearts.models` + `benefit_models` + `models_at`；后台 `Scanner` 每 30 分钟按账号刷新
- **福利自动领取**：发现时先 `benefit/claim`（幂等），`WB_CODEARTS_BENEFIT_CLAIM=0` 关闭写操作
- **OAuth 登录**：`POST /api/v2/accounts/login/start` + `GET .../login/poll`（`state` 即 login id），
  服务端生成 ticket/PKCE/DPoP，面板打开门户链接，ticket 轮询无需回调端口
- 面板：渠道下拉/筛选/密钥绑定新增「华为 CodeArts」；「添加账号」支持华为云登录；
  模型页 CodeArts 渠道有「重新发现」按钮

### 9.4 账号文件格式

```jsonc
// data/accounts/hw-<user_id>.json
{
  "uid": "hw-123456",
  "nickname": "华为云",
  "realm": "codearts",
  "accessToken": "<STS security_token>",
  "refreshToken": "<OAuth refresh_token>",
  "expiresAt": 1794000000,
  "codearts": {
    "access_key_id": "…",
    "secret_access_key": "…",
    "security_token": "<同 accessToken>",
    "expiration": "2026-11-18T00:00:00Z",
    "client_id": "codearts-agent",
    "code_verifier": "…",
    "dpop_private_key": {"kty":"EC","crv":"P-256","x":"…","y":"…","d":"…"},
    // models / benefit_models / models_at 由自动发现写入；手填也会被下一轮覆盖
    "models": ["GLM-5.2", "Qwen3-VL-235B", "deepseek-v4.1-flash-1017"],
    "benefit_models": ["Qwen3-VL-235B"]
  }
}
```

`realm` 必须显式写 `codearts`：留空会走 `realm.Detect`（WorkBuddy 的 JWT/域名判定），
把账号判成国际版。`codearts` 子对象也可以扁平地写在顶层，两种布局都读。

### 9.5 能力清单

| 项 | 状态 |
| --- | --- |
| 聊天（三协议 / 流式 / 工具） | 已接：签名 + SSE 转发，非流式由网关聚合 |
| 凭据续期（OAuth2 + DPoP） | 已接：`internal/refresh` 按渠道分派，终态失效转 `ErrNeedsLogin` |
| 模型自动发现（三源） | 已接：`codeartsync.Service.Discover` + 后台 `Scanner`（30 分钟/账号） |
| 福利自动领取 | 已接：发现时幂等领取，`WB_CODEARTS_BENEFIT_CLAIM=0` 关闭 |
| 签到（领 1000 积分） | 已接：面板手动 + 调度器自动（`Channel` 钩子） |
| OAuth 登录（PKCE + ticket） | 已接：无回调端口的服务端登录流程 |
| 账号校验（current/user） | 已接（`codearts.CurrentUser`），面板未单独开按钮 |
| 积分余额 | 已接：`GET /snap-manager/v1/statistics/plugin`（v2.6.3），面板「积分」列与详情显示剩余/总量/套餐/重置日 |
| 单账号并发 | 已按渠道生效：`realm.InflightFor` 把全局上限与渠道上限取小，默认 **3**（`WB_CODEARTS_INFLIGHT` 可调，0=不设渠道上限）；建池时同步写入 `SetMaxInflight`，Redis 共享限流也按账号取该值 |

### 9.6 新增接口

| 方法 | 路径 | 作用 |
| --- | --- | --- |
| POST | `/api/v2/accounts/login/start` | `{"realm":"codearts"}` → `{state, authUrl}` |
| GET | `/api/v2/accounts/login/poll?state=` | `pending` / `ok`(uid,nickname) / `error` / `expired` |
| POST | `/api/v2/accounts/login/cancel` | 丢弃未完成的登录 |
| POST | `/api/v2/codearts/models/refresh` | `{"uid":""}` 或全渠道，返回每个账号的模型与来源错误 |

环境变量：`WB_CODEARTS_BENEFIT_CLAIM=0` 关闭福利自动领取（默认开启）。

### 9.7 验证命令

```bash
go build ./... && go vet ./...
# 专项（按需；无需跑全量）
go test ./internal/upstream/codearts ./internal/codeartsync ./internal/api \
        ./internal/scheduler ./internal/refresh ./internal/realm \
        ./internal/store/accounts ./internal/store/settings ./internal/realmswitch -count=1
cd web && npm run typecheck && npm test && npm run build
```
