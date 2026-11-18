# 远程推送账号（API）

用一把 API key 就能把账号推进网关，不需要面板会话、不需要 CSRF。适合登录机器人、
另一台网关、cron 任务。

## 端点

```
POST /api/v2/accounts/push
Authorization: Bearer <API key>
Content-Type: application/json
```

查询参数 `?dry_run=1` 只校验不落盘。

## 支持的请求体

四种形状都行：

```jsonc
// 1. 面板导出的整包
{"accounts": [ {...}, {...} ]}

// 2. 裸数组
[ {...}, {...} ]

// 3. 单个账号对象
{ "uid": "...", "realm": "codearts", "accessToken": "...", ... }

// 4. 面板导出文件的原文（{"document": {...}} 或直接文件内容）
{"document": {"accounts": [...]}}
```

同 `uid` 已存在时**覆盖**（这是重新登录机器人需要的语义），不会重复添加。

## 华为 CodeArts 账号的最小字段

```jsonc
{
  "uid": "6425e0187ea318dc",
  "realm": "codearts",
  "nickname": "16528645151",
  "accessToken": "<STS security token>",
  "refreshToken": "<refresh token>",
  "expiresAt": 1794973868,
  "codearts": {
    "access_key_id": "AK...",
    "secret_access_key": "SK...",
    "security_token": "<同一个 STS token>",
    "dpop_private_key": "<DPoP 私钥，缺了无法续期>"
  }
}
```

缺 `dpop_private_key` 或 `refreshToken` 时仍能导入，但 2 小时后会失效，需要重新登录。

## 响应

```json
{
  "ok": true,
  "count": 2,
  "dry_run": false,
  "result": {
    "added":   ["uid-a"],
    "updated": ["uid-b"],
    "skipped": [],
    "invalid": [{"index": 2, "reason": "缺少 access_key_id / secret_access_key"}]
  }
}
```

## 认证规则

| 情况 | 行为 |
| --- | --- |
| 已创建 API key | 必须带 `Authorization: Bearer <key>`（或 `x-api-key`），不匹配即 401 |
| 没创建 key，且请求来自本机 | 放行（方便本机脚本；写接口不对公网开放） |
| 没创建 key，请求来自外部 | 401，提示先在面板创建 key |
| 面板里关掉鉴权（`auth_disabled`） | 放行 |

## 例子

```bash
KEY=wb-xxxxxxxxxxxxxxxx
curl -X POST "http://服务器:8080/api/v2/accounts/push?dry_run=1" \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  --data-binary @account.json

curl -X POST "http://服务器:8080/api/v2/accounts/push" \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d "{\"accounts\": $(cat batch.json)}"
```

推完之后网关会自动：补一次模型发现、按渠道拉起后台续期、把账号纳入调度。
