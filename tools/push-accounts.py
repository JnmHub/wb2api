#!/usr/bin/env python3
"""Preview or push account credentials to a wb2api instance."""

import argparse
from http.client import HTTPException
import json
import math
import os
from pathlib import Path
import sys
from urllib.error import HTTPError, URLError
from urllib.parse import urlsplit
from urllib.request import HTTPRedirectHandler, Request, build_opener


REALMS = ("cn", "intl", "codearts", "raccoon")
MAX_BODY = 24 << 20
BUCKETS = ("added", "updated", "skipped", "invalid")
SECRET_FIELDS = {
    "token", "accesstoken", "refreshtoken", "securitytoken", "clouddragontoken",
    "accesskeyid", "accesskey", "secretaccesskey", "secretkey", "access", "secret",
    "codeverifier", "d", "x", "y",
}
ACCESS_FIELDS = ("accessToken", "access_token", "token")
REFRESH_FIELDS = ("refreshToken", "refresh_token")


class PushError(Exception):
    pass


class NoRedirect(HTTPRedirectHandler):
    def redirect_request(self, *args):
        return None


def encode(value):
    return json.dumps(value, ensure_ascii=False, allow_nan=False, separators=(",", ":")).encode("utf-8")


def protect_raccoon_row(row):
    # An older WorkBuddy parser looks for AT in the row, auth or account.
    # Keep Raccoon credentials in the provider block even on a mixed rollout.
    extra = row.get("extra")
    extra = extra if isinstance(extra, dict) else {}
    auth = row.get("auth")
    auth = auth if isinstance(auth, dict) else {}
    provider = row.get("raccoon")
    if not isinstance(provider, dict):
        provider = extra.get("raccoon")
    provider = dict(provider) if isinstance(provider, dict) else {}
    for fields in (ACCESS_FIELDS, REFRESH_FIELDS):
        token = next((layer[key].strip() for layer in (row, auth, provider, extra) for key in fields
                      if isinstance(layer.get(key), str) and layer[key].strip()), "")
        for key in fields:
            row.pop(key, None)
            provider.pop(key, None)
        if token:
            provider[fields[0]] = token
    for name in ("auth", "account"):
        if isinstance(row.get(name), dict):
            row[name] = {key: value for key, value in row[name].items() if key not in ACCESS_FIELDS + REFRESH_FIELDS}
    row["raccoon"] = provider


def read_rows(path, default_realm):
    try:
        doc = json.loads(path.read_text(encoding="utf-8-sig"))
        if isinstance(doc, dict) and "document" in doc:
            doc = doc["document"]
            if isinstance(doc, str):
                doc = json.loads(doc)
    except (OSError, ValueError) as exc:
        raise PushError("无法读取账号文件，请检查 UTF-8 编码和 JSON 格式") from exc

    if isinstance(doc, dict):
        doc = doc.get("accounts", [doc])
    if not isinstance(doc, list) or not doc:
        raise PushError("账号文件必须包含非空账号列表")
    rows = []
    for index, original in enumerate(doc, 1):
        if not isinstance(original, dict):
            raise PushError(f"第 {index} 行必须是 JSON 对象")
        row = dict(original)
        realm = row.get("realm", "")
        if not isinstance(realm, str):
            raise PushError(f"第 {index} 行的 realm 必须是字符串")
        realm = realm.strip().lower()
        provider = row.get("provider")
        provider_realm = provider if provider in ("codearts", "raccoon") else ""
        if realm and provider_realm and realm != provider_realm:
            raise PushError(f"第 {index} 行的 realm 与 provider 冲突")
        realm = realm or provider_realm or default_realm
        if realm not in REALMS or (default_realm and realm != default_realm):
            raise PushError(f"第 {index} 行的渠道缺失或冲突，请设置正确的 realm 或 --realm")
        row["realm"] = realm
        if "uid" in row and (not isinstance(row["uid"], str) or not row["uid"].strip()):
            raise PushError(f"第 {index} 行的 uid 必须是非空字符串")
        if realm in ("cn", "intl") and not row.get("accessToken") and "access_token" in row:
            for source, target in (("access_token", "accessToken"), ("refresh_token", "refreshToken"),
                                   ("expires_at", "expiresAt"), ("email", "nickname")):
                if not row.get(target) and source in row:
                    row[target] = row[source]
        if realm == "raccoon":
            protect_raccoon_row(row)
        try:
            raw = encode(row)
        except (ValueError, UnicodeError) as exc:
            raise PushError(f"第 {index} 行不能编码为 UTF-8 JSON") from exc
        if b"REPLACE_WITH_" in raw:
            raise PushError(f"第 {index} 行还有模板占位符")
        rows.append((row, raw))
    return rows


def batches(rows, batch_size):
    start, batch, size = 1, [], len(b'{"accounts":[]}')
    for row, raw in rows:
        if len(raw) + len(b'{"accounts":[]}') > MAX_BODY:
            raise PushError(f"第 {start + len(batch)} 行超过单次请求体上限")
        if batch and (len(batch) == batch_size or size + len(raw) + 1 > MAX_BODY):
            yield start, batch
            start += len(batch)
            batch, size = [], len(b'{"accounts":[]}')
        size += len(raw) + bool(batch)
        batch.append((row, raw))
    if batch:
        yield start, batch


def endpoint(url, allow_http):
    if any(char.isspace() or ord(char) < 32 for char in url):
        raise PushError("wb2api 地址不能包含空白或控制字符")
    try:
        parsed = urlsplit(url)
        port = parsed.port
    except ValueError as exc:
        raise PushError("wb2api 地址格式无效") from exc
    if (parsed.scheme not in ("https", "http") or not parsed.hostname or parsed.username is not None
            or parsed.query or parsed.fragment or port == 0):
        raise PushError("请提供 wb2api 的服务根地址，可保留反向代理的路径前缀")
    if parsed.scheme == "http" and parsed.hostname not in ("127.0.0.1", "::1", "localhost") and not allow_http:
        raise PushError("远程地址请使用 HTTPS，私有可信链路上的 HTTP 可显式传 --allow-http")
    return parsed.geturl().rstrip("/") + "/api/v2/accounts/push"


def push(opener, url, key, timeout, batch, dry_run):
    body = b'{"accounts":[' + b','.join(raw for _, raw in batch) + b']}'
    try:
        request = Request(url + ("?dry_run=1" if dry_run else ""), data=body, method="POST", headers={
            "Authorization": "Bearer " + key, "Content-Type": "application/json", "Accept": "application/json",
        })
        with opener.open(request, timeout=timeout) as response:
            doc = json.loads(response.read(MAX_BODY + 1))
    except HTTPError as exc:
        raise PushError(f"网关返回 HTTP {exc.code}") from exc
    except (URLError, OSError, HTTPException, ValueError) as exc:
        raise PushError("请求失败或网关未返回可读的 JSON") from exc
    if (not isinstance(doc, dict) or doc.get("ok") is not True or doc.get("dry_run") is not dry_run
            or type(doc.get("count")) is not int or doc["count"] != len(batch)):
        raise PushError("网关响应与账号推送协议不符")
    report = doc.get("result")
    if not isinstance(report, dict) or any(not isinstance(report.get(name), list) for name in BUCKETS):
        raise PushError("网关未返回完整的逐条结果")
    if (sum(len(report[name]) for name in BUCKETS) != len(batch)
            or any(not isinstance(uid, str) or not uid for name in ("added", "updated") for uid in report[name])
            or any(not isinstance(item, dict) for name in ("invalid", "skipped") for item in report[name])
            or any(type(item.get("index")) is not int or not 1 <= item["index"] <= len(batch) for item in report["invalid"])):
        raise PushError("逐条结果与请求中的账号数量或行号不符")
    return report


def diagnostic_secrets(value):
    if isinstance(value, list):
        for item in value:
            yield from diagnostic_secrets(item)
    if isinstance(value, dict):
        for name, item in value.items():
            if isinstance(item, str) and item and name.lower().replace("_", "") in SECRET_FIELDS:
                yield item.strip()
            else:
                yield from diagnostic_secrets(item)


def show_report(report, key, start, batch, dry_run):
    secrets = {key, *(value for row, _ in batch for value in diagnostic_secrets(row))}
    secrets.discard("")
    exact_secrets = sorted(secrets, key=len, reverse=True)

    def safe(text):
        text = text if isinstance(text, str) else ""
        for secret in exact_secrets:
            text = text.replace(secret, "[已隐藏]")
        return text[:400]

    output = {"dry_run": dry_run, "first_row": start, "count": len(batch)}
    for name in ("added", "updated"):
        output[name] = [safe(uid) for uid in report[name]]
    output["skipped"] = [{"uid": safe(item.get("uid")), "reason": safe(item.get("reason"))} for item in report["skipped"]]
    output["invalid"] = [{"row": start + item["index"] - 1, "reason": safe(item.get("reason"))} for item in report["invalid"]]
    print(json.dumps(output, ensure_ascii=False), flush=True)


def main():
    parser = argparse.ArgumentParser(description="预演或推送 wb2api 账号")
    parser.add_argument("file", type=Path, help="UTF-8 账号 JSON 文件")
    parser.add_argument("--url", default=os.environ.get("WB2API_URL"), help="wb2api 服务根地址")
    parser.add_argument("--realm", choices=REALMS, default="", help="为同一渠道的文件补齐 realm")
    parser.add_argument("--apply", action="store_true", help="先预演全部账号，再写入。已有 UID 走更新")
    parser.add_argument("--batch-size", type=int, default=100, help="每批账号上限，默认 100")
    parser.add_argument("--timeout", type=float, default=30, help="单次 HTTP 超时秒数，默认 30")
    parser.add_argument("--allow-http", action="store_true", help="允许在私有可信链路上使用远程明文 HTTP")
    args = parser.parse_args()
    key = os.environ.get("WB2API_KEY", "").strip()
    if not args.url or not key or any(ord(char) < 33 or ord(char) > 126 for char in key):
        parser.error("请设置 WB2API_KEY 和 WB2API_URL，或用 --url 指定地址")
    if not 1 <= args.batch_size <= 5000 or not math.isfinite(args.timeout) or args.timeout <= 0:
        parser.error("--batch-size 须在 1 到 5000 之间，--timeout 须为有限正数")

    try:
        url = endpoint(args.url, args.allow_http)
        rows = read_rows(args.file, args.realm)
        plan = list(batches(rows, args.batch_size))
        opener = build_opener(NoRedirect)
        if any(row["realm"] == "raccoon" for row, _ in rows):
            probe = {"realm": "raccoon"}
            result = push(opener, url, key, args.timeout, [(probe, encode(probe))], True)
            if len(result["invalid"]) != 1 or "raccoon" not in str(result["invalid"][0].get("reason", "")).lower():
                raise PushError("目标实例未确认支持小浣熊，实际账号凭据尚未发送")

        previewed = set()
        bad_preview = False
        for start, batch in plan:
            report = push(opener, url, key, args.timeout, batch, True)
            show_report(report, key, start, batch, True)
            uids = report["added"] + report["updated"]
            duplicate = len(set(uids)) != len(uids) or bool(previewed.intersection(uids))
            previewed.update(uids)
            bad_preview |= duplicate or bool(report["invalid"] or report["skipped"])
            if duplicate:
                print(f"从第 {start} 行开始的批次包含重复的归一化 UID", file=sys.stderr)
        if bad_preview:
            raise PushError("预演有无效行、跳过行或重复 UID，未执行写入")
        if args.apply:
            for start, batch in plan:
                try:
                    report = push(opener, url, key, args.timeout, batch, False)
                except PushError as exc:
                    raise PushError(f"第 {start} 行起的批次提交结果未确认。{exc}。请核对网关账号记录后再处理") from exc
                show_report(report, key, start, batch, False)
                if report["invalid"] or report["skipped"]:
                    raise PushError(f"第 {start} 行起的批次有未完成的账号，已停止后续批次。此前成功项可能已写入")
    except PushError as exc:
        print(str(exc), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    if hasattr(sys.stdout, "reconfigure"):
        sys.stdout.reconfigure(encoding="utf-8")
        sys.stderr.reconfigure(encoding="utf-8")
    raise SystemExit(main())
