# wb2api 账号导入对接

机器对接统一使用 `POST /api/v2/accounts/push`，带 `Authorization: Bearer <wb2api API Key>`。请求体使用 UTF-8、无 BOM 的 `{"accounts":[...]}`。三个渠道可混批，账号 ID 用字符串。

[三渠道 JSON 模板](examples/account-push.json) · [Python 推送客户端](../tools/push-accounts.py)

## 三类账号格式

| 渠道 | 统一写法 | 原始文件兼容 |
| --- | --- | --- |
| WorkBuddy | `realm` 填 `cn` 或 `intl`，带稳定 `uid` 和 JWT `accessToken`。要持续续期，保留 `refreshToken` 和 `expiresAt`。可带 `domain`、`nickname`、`platform`、`enterpriseId`。 | 接受 `auth` / `account` 嵌套对象，也接受 Cockpit 的 `access_token`、`refresh_token`、`expires_at`。脚本会将 WorkBuddy 的这些字段转换为驼峰写法。区域识别优先用 JWT 或域名证据，填的 `realm` 应与其一致。 |
| 华为 CodeArts | `realm:"codearts"`，带 `uid`、`expiresAt` 和 `codearts.access_key_id`、`codearts.secret_access_key`、`codearts.security_token`。要持续续期，还要保留 `refreshToken`、原 `client_id`、`code_verifier` 及 `dpop_private_key` 对象。组织账号应保留 `domain_id`。 | 接受平铺 `user_id`、`cloud_dragon_token`、`expiration`，也接受 Agent2API 的 `codearts_provider_credential`。Agent2API 格式要求有效到期时间、STS、RT、PKCE verifier 和有效的 P-256 DPoP 私钥。 |
| 小浣熊 | `realm:"raccoon"`，在 `raccoon` 对象中带 JWT `accessToken`。已有账号用网关导出的 UID，新账号通常用 `rc-<用户ID>`。企业账号保留 `raccoon.officeIdentity`。旧登录态有 RT 时可在同一对象中带 `refreshToken`。IAM 账号过期后需要重新登录。 | 通用入口接受显式标注 `raccoon` 的 camelCase 或 snake_case 登录态。面板专用 `POST /api/v2/raccoon/accounts/import` 也支持原始 `auth.json` 或 `{"token":"<JWT>"}`，并会尝试从上游补齐账号资料。 |

- `expiresAt` 建议传真实的 Unix 秒数。WorkBuddy 和小浣熊在字段缺失时尝试取 JWT 的 `exp`。CodeArts 还接受毫秒时间戳和 RFC3339 时间。
- CodeArts 的 `accessToken` 在部分 Agent2API 导出里是 AK。单独保留正确的 `security_token`。Agent2API 的 PKCE 和 DPoP 路径分别是 `oauth_context.pkce_pair.code_verifier` 和 `oauth_context.dpop_key_pair.private_key_jwk`。私钥是包含 `kty`、`crv`、`x`、`y`、`d` 的 JSON 对象。
- UID 在整个网关中全局唯一。CodeArts 优先用凭据里的 `user_id`，不能拿 Agent2API 的本地 `id` 顶替。WorkBuddy 和 CodeArts 会归一化 UID 中的部分字符。更新已有账号时应使用网关导出的 UID，并保持来源账号和渠道一致。
- 通用推送会重新归一化 WorkBuddy 记录，更新时可能重新启用原本停用的账号，也不能借这个入口还原所有 WorkBuddy 运维字段。CodeArts 和小浣熊能保留部分已有运维配置。

## 机器推送

| 用途 | 请求 |
| --- | --- |
| 预演 | `POST /api/v2/accounts/push?dry_run=1` |
| 实写 | `POST /api/v2/accounts/push`，相同 UID 按更新处理 |

原始 Push 还接受裸数组、有顶层 `uid` 的单条对象，或把对象、数组置于 `document` 字段中。用统一的 `accounts` 数组也能发送没有顶层 `uid` 的原始凭据。原始 Push 的 `document` 不接受 JSON 字符串，`overwrite` 和 `dry_run` 请求体字段也不会改变 Push 行为。

服务器最多读取 32 MiB 请求体并处理 5,000 条账号。代理可能先限制请求体。脚本默认每批 100 条，单批编码预算 24 MiB，可用 `--batch-size` 调小行数。

```powershell
# 先通过本机环境变量配置 WB2API_URL 和 WB2API_KEY
py -3 .\tools\push-accounts.py .\accounts.json
py -3 .\tools\push-accounts.py .\accounts.json --apply
# 原始 auth.json 或单渠道文件缺少 realm 时
py -3 .\tools\push-accounts.py .\auth.json --realm raccoon
```

脚本接受 UTF-8 或带 BOM 的文件，会将输入统一编码成 UTF-8 请求。默认预演，`--apply` 会先预演全部批次。遇到无效行、跳过行或重复 UID 就中止实写。提交结果不确定时，脚本停止后续批次，不会自动重发。需要经过代理前缀时，可在 `WB2API_URL` 中保留前缀。明文 HTTP 默认仅允许回环地址，私有可信链路可用 `--allow-http`。

小浣熊必须连接支持该渠道的版本。旧版本可能把带顶层 JWT 的小浣熊账号归入 WorkBuddy。脚本先用一条不含凭据的预演探测目标能力，并把小浣熊凭据放到 `raccoon` 对象中。混合版本切流时，即使某个批次落到旧版，旧版 WorkBuddy 解析器也读取不到该凭据。

## 逐条结果与重试

```json
{
  "ok": true,
  "count": 4,
  "dry_run": true,
  "result": {
    "added": ["uid-a"],
    "updated": ["uid-b"],
    "skipped": [{"uid": "uid-a", "reason": "duplicate inside the document"}],
    "invalid": [{"index": 4, "reason": "缺少 access_key_id / secret_access_key"}]
  }
}
```

HTTP 200 和 `ok:true` 表示批次已处理。调用方还要检查 `invalid` 和 `skipped`，并处理已经写入的成功项。服务端的 `invalid[].index` 从 1 开始，按批次计。脚本把它转换为源文件的账号行号。

预演会返回计划新增和更新的 UID，实际写入仍可能因并发修改或持久化失败而发生变化。预演通过也无法证明凭据没过期、RT 可续期或账号已可调度。写入超时后先核对网关账号记录，必要时重新获取最新凭据再推，避免用旧 AT/RT 覆盖刚刷新的凭据。

## 面板接口与权限

面板使用 `POST /api/v2/accounts/import`，请求体为 `{"document":{"accounts":[...]}, "overwrite":false, "dry_run":true}`，`document` 也接受 JSON 字符串。该入口使用已登录面板会话和 `X-WB-CSRF`，`overwrite:false` 会跳过已有 UID。导出用于回导的文件要保留凭据。

Push 的 API Key 与面板会话分开。当前 Push 不按 Key 的模型、渠道或账号范围授予导入权限，也没有按 Key 的停用状态拒绝请求。应在可信管理网络访问该路由，并使用专用凭据。正式迁移推荐 HTTPS，并避免把请求体、AT、RT 和 Key 写进脚本日志。
