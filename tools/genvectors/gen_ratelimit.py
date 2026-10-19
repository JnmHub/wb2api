"""Golden vectors for internal/gateway/ratelimit and the pool backoff ladders.

Run:  py -I tools/genvectors/gen_ratelimit.py
"""
import json
import os
import sys

sys.dont_write_bytecode = True
ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
sys.path.insert(0, os.path.join(ROOT, "legacy", "python"))
os.environ.setdefault("WB_USAGE_CACHE", "0")
import wb_accounts as A  # noqa: E402
import wb_proxy as P  # noqa: E402

bodies = [
    "", "{}", "not json",
    '{"code":6004,"msg":"your usage will reset at 2026-09-19 18:29:03 UTC+8, please wait"}',
    '{"code":"6004","msg":"Your usage will reset at 2026-09-19 18:29:03 UTC+8"}',
    '{"code":6004.0}', '{"code":null}', '{"code":[6004]}',
    '{"code":14003,"msg":"too many requests"}',
    '{"msg":"usage exceeds frequency limit"}',
    '{"msg":"USAGE EXCEEDS FREQUENCY LIMIT"}',
    "请求过于频繁，请稍后再试",
    "您的使用量已超出频率限制，将在 2026-10-02 16:00:34 UTC+8 重置",
    "您的使用量已超出频率限制，将在2026-10-02T16:00:34 UTC+8重置",
    "limit hit 2026-10-02 16:00:34 UTC-5:30 later",
    "limit hit 2026-10-02 16:00:34 UTC+5:30 later",
    "limit hit 2026-10-02 16:00:34 UTC-0 later",
    "limit hit 2026-10-02 16:00:34 UTC+08:00 later",
    "reset at 2026-10-02 16:00:34 with no zone",
    "reset at 2026-13-45 99:99:99 UTC+8",
    "\\u5c06\\u5728 2026-10-02 16:00:34 UTC+8 \\u91cd\\u7f6e",
    "\\u5c06\\u5728 2026-10-02 16:00:34 UTC+8 \\u91cd\\u7f6e 中文",
    "bad escape \\u12 reset at 2026-10-02 16:00:34 UTC+8",
    "line\\nbreak \\u0041 reset at 2026-10-02 16:00:34 UTC+8",
    '{"accountId":"x","msg":"too many requests"}',
    "this account has hit its limit",
    "This Account has hit its limit",
    "accountId=5 only",
    "your accounts are fine",
    "the account is limited, reset at 2026-10-02 16:00:34 UTC+8",
    "账号级限流，请稍后", "该账号请求过多", "per-account throttle", "per_account throttle",
    "credential-level limit", "your credential is throttled",
    '{"code":11140,"msg":"request illegal"}', '{"code": "11140"}', '{"code":111400}',
    "Your account has been restricted due to unusual activity", "账号存在异常行为，已被限制使用",
    "content policy violation", "request id 11140abc",
]
out = {"bodies": []}
for b in bodies:
    reset = P.parse_rate_limit_reset(b)
    out["bodies"].append({
        "body": b,
        "reset": reset,
        "scope": P.credential_scope_phrase(b) or "",
        "soft": P.rate_limit_is_soft_shape(b),
        "cap": P.rate_limit_is_cap(b),
        "account_level": P.rate_limit_is_account_level(b, reset),
        "restriction": P.restriction_is_account_level(b),
    })
out["backoff"] = {
    "soft": [A.soft_backoff(n) for n in range(0, 12)],
    "unverified": [A.unverified_soft_backoff(n) for n in range(0, 12)],
    "breaker": [A.breaker_backoff(n) for n in range(0, 14)],
    "degrade": [A.degrade_backoff(n) for n in range(0, 14)],
}
dest = os.path.join(ROOT, "internal", "gateway", "ratelimit", "testdata", "ratelimit.json")
os.makedirs(os.path.dirname(dest), exist_ok=True)
with open(dest, "w", encoding="utf-8", newline="\n") as fh:
    json.dump(out, fh, ensure_ascii=False)
print("wrote", dest, len(out["bodies"]), "bodies")
