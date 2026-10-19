"""Generate golden vectors from the legacy Python implementation.

Run:  py -I tools/genvectors/gen.py
The Go tests load these files and must reproduce the legacy output exactly.
"""
import base64, json, os, sys
sys.dont_write_bytecode = True
ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
sys.path.insert(0, os.path.join(ROOT, "legacy", "python"))
import wb_fingerprint as F
import wb_accounts as A

def w(rel, obj):
    p = os.path.join(ROOT, rel)
    os.makedirs(os.path.dirname(p), exist_ok=True)
    with open(p, "w", encoding="utf-8") as fh:
        json.dump(obj, fh, ensure_ascii=False, indent=1)
    print("wrote", rel, len(obj))

uids = ["", "u1", "1234567890abcdef", "用户-中文", "a" * 64, "UID_with-dash.and.dot", "anonymous"]
salts = ["machine", "session", "x", ""]
w("internal/fingerprint/testdata/vectors.json", [
    {"uid": u, "salt": s, "id": F.derive_id(u, s)} for u in uids for s in salts])

def jwt(claims):
    seg = lambda o: base64.urlsafe_b64encode(json.dumps(o).encode()).decode().rstrip("=")
    return "%s.%s.sig" % (seg({"alg": "none"}), seg(claims))

tokens = [
    jwt({"sub": "uid-1", "exp": 1790000000, "iss": "https://www.workbuddy.ai/x"}),
    jwt({"sub": "uid-2", "exp": 1790000000, "iss": "https://www.workbuddy.cn/x", "enterprise_id": "ent-9"}),
    jwt({"sub": "uid-3", "iss": "https://copilot.tencent.com"}),
    jwt({"sub": "uid-4", "exp": "bad"}),
    jwt({"sub": "uid-5", "iss": "https://www.codebuddy.ai"}),
    jwt({"exp": 5}),
    "garbage", "", "a.b", "a.%%%.c",
]
domains = [None, "", "www.workbuddy.ai", "copilot.tencent.com", "www.codebuddy.cn", "other.example"]
jv = []
for t in tokens:
    for d in domains:
        jv.append({"token": t, "domain": d,
                   "exp": A.jwt_exp(t), "uid": A.jwt_uid(t), "iss": A.jwt_issuer(t),
                   "ent": str(A._jwt_claims(t).get("enterprise_id") or ""),
                   "evidence": A.realm_evidence(t, d), "realm": A.detect_realm_from_token(t, d),
                   "domain_for": {r: A.domain_for_realm(r, d) for r in ("intl", "cn")}})
w("internal/realm/testdata/jwt_realm.json", jv)
ep = [0, 1, "1790000000", 1790000000, 1790000000123, "1790000000123", 1.5e12, None, "x", "", -5, 99999999999, 100000000001]
w("internal/realm/testdata/epoch.json", [{"in": e, "out": A.normalize_epoch(e)} for e in ep])
dm = [["A", "b ", "a", "", " C"], [1, "x", None], "str", None, [], ("t",)]
w("internal/realm/testdata/disabled_models.json",
  [{"in": list(x) if isinstance(x, tuple) else x, "out": A.normalise_disabled_models(x)} for x in dm])
fc = [("intl", "hy3"), ("cn", "hy3"), ("cn", "deepseek-v4.1-flash"), ("intl", "deepseek-v4.1-flash"), ("", "hy3"), ("intl", ""), ("x", "hy3")]
w("internal/realm/testdata/free_cap.json", [{"realm": r, "model": m, "out": A.is_free_cap_model(r, m)} for r, m in fc])

# ---- account files: written by the legacy Account.save(), expectations are the
# legacy in-memory attributes after loading each input dict. ----
import shutil, time
acc_dir = os.path.join(ROOT, "internal", "store", "accounts", "testdata", "py_written")
shutil.rmtree(acc_dir, ignore_errors=True)
os.makedirs(acc_dir)
T_INTL = jwt({"sub": "intl-uid-1", "exp": 1790000000, "iss": "https://www.workbuddy.ai/"})
T_CN = jwt({"sub": "cn-uid-2", "exp": 1790000000, "iss": "https://www.workbuddy.cn/", "enterprise_id": "ent-7"})
inputs = [
    {"uid": "intl-uid-1", "nickname": "Alice", "accessToken": T_INTL, "refreshToken": "r1",
     "expiresAt": 1790000000123, "addedAt": 1790000000.5, "enabled": True, "weight": 3,
     "note": "批量号 " + "x" * 300, "disabledModels": ["GPT-5.5", " gpt-5.5 ", "", 4],
     "credits": {"remain": 100, "total": 200, "packages": [{"n": "a"}]}, "lastCheckin": "2026-10-01 00:00:00",
     "modelCaps": {"hy3": {"at": 1.5e9, "reset": 1.6e9}, "gpt-5.5": {"at": 1.5e9, "reset": 1.6e9}, "bad": 5},
     "product": "VSC", "proxySlot": " slot-1 ", "proxy": "http://p:1"},
    {"accessToken": T_CN, "refreshToken": "r2", "nickname": {"$wbEncrypted": "zzz"}, "weight": 999,
     "enabled": False, "restrictedUntil": 1790001234.5, "platform": "", "source": ""},
    {"uid": "weird/uid with spaces", "accessToken": "plain", "weight": "7", "enabled": "yes", "domain": "www.codebuddy.cn"},
    {"uid": "zero-weight", "accessToken": T_INTL, "weight": 0, "note": 12, "expiresAt": "bad",
     "lastDailyChat": "", "modelCaps": {"hy3": {"at": 0, "reset": 5}}},
]
expected = []
for d in inputs:
    a = A.Account(d, None)
    a.save(acc_dir)
    safe = A.re.sub(r"[^A-Za-z0-9_-]", "_", str(a.uid or "")).strip("_ ")
    expected.append({
        "file": safe + ".json", "uid": a.uid, "nickname": a.nickname, "domain": a.domain, "realm": a.realm,
        "platform": a.platform, "product": a.product, "enterprise_id": a.enterprise_id,
        "access_token": a.access_token, "refresh_token": a.refresh_token, "expires_at": a.expires_at,
        "source": a.source, "proxy_slot": a.proxy_slot, "proxy_legacy": a.proxy_legacy,
        "enabled": bool(a.enabled), "weight": a.weight, "note": a.note,
        "disabled_models": list(a.disabled_models), "restricted_until": a.restricted_until,
        "credits": a.credits, "last_checkin": a.last_checkin, "last_daily_chat": a.last_daily_chat,
        "last_activity_report": a.last_activity_report, "model_caps": a.model_caps})
# a non-account file and a corrupt one that the loader must tolerate
open(os.path.join(acc_dir, "settings.json"), "w").write('{"x":1}')
open(os.path.join(acc_dir, "broken.json"), "w").write('{not json')
w("internal/store/accounts/testdata/py_expected.json", expected)

# ---- panel password digests (PBKDF2-SHA256) ----
import wb_settings as S
cases = []
for pw, salt, rounds in [("admin2", "00" * 16, 120000), ("pässwörd-中文", "ab" * 16, 120000),
                         ("x", "0123456789abcdef0123456789abcdef", 1000), ("", "ff" * 16, 5000)]:
    cases.append({"password": pw, "salt": salt, "rounds": rounds, "digest": S._digest(pw, salt, rounds)})
w("internal/store/settings/testdata/pbkdf2.json", cases)

# ---- fnmatch + api key entries ----
import fnmatch
pats = ["gpt-*", "deepseek/*", "*", "glm-5.?", "[gk]*", "[!g]*", "a[", "x[]]y", "gpt-5.5", "", "*-flash", "GPT*"]
names = ["gpt-5.5", "gpt-6-astra", "deepseek/r1", "deepseek-v4.1-flash", "glm-5.3", "kimi-k3", "a[", "x]y", "", "hy3", "gpt/x"]
w("internal/store/settings/testdata/fnmatch.json",
  [{"pattern": p, "name": n, "out": fnmatch.fnmatchcase(n, p)} for p in pats for n in names])
import tempfile
sd = tempfile.mkdtemp()
settings_doc = {"api_keys": [
    {"id": "k1", "name": "main", "key": "sk-aaa", "realm": "CN", "models": "deepseek*, GPT-5.5;glm-5.3", "concurrency_limit": 4, "created_at": "2026/10/01 10:00"},
    {"id": "k2", "name": "", "key": "sk-bbb", "realm": "weird", "models": ["a", "A", "b"], "max_concurrency": "7", "enabled": False},
    {"id": "k1", "name": "dupid", "key": "sk-ccc", "models": 5},
    {"id": "k4", "name": "dupkey", "key": "sk-aaa"},
    {"id": "gone", "name": "old", "key": "", "deleted_at": "2026/09/01 00:00", "realm": "intl"},
    {"id": "empty", "key": ""}, "not-a-dict",
]}
json.dump(settings_doc, open(os.path.join(sd, "settings.json"), "w"))
ak = S.api_keys(sd, include_deleted=True)
w("internal/store/settings/testdata/api_keys_doc.json", [settings_doc])
w("internal/store/settings/testdata/api_keys_expected.json", ak)
w("internal/store/settings/testdata/key_allows.json",
  [{"key_id": e["id"], "model": m, "out": S.key_allows_model(e, m)} for e in ak for m in ["deepseek-v4.1-flash", "gpt-5.5", "GPT-5.5", "glm-5.3", "kimi", "", "a", "b"]])
legacy_dir = tempfile.mkdtemp()
json.dump({"api_key_set": True, "api_key": " legacy-k "}, open(os.path.join(legacy_dir, "settings.json"), "w"))
w("internal/store/settings/testdata/api_keys_legacy.json", S.api_keys(legacy_dir))

# ---- leaked tool call parser + stream filter (legacy wb_proxy) ----
import random
os.environ.setdefault("WB_USAGE_CACHE", "0")
import wb_proxy as P
BAR = "｜｜"
FRAGS = ["<run_code>", "</run_code>", "<run_shell>", "<invoke name=\"x\">", "</invoke>", "<parameter name=\"code\">",
         "<parameter name=\"description\">", "</parameter>", "<param name=\"p\">v</param>", "<arg name=\"a\">1</arg>",
         "<%sDSML%s calls>" % (BAR, BAR), "</%sDSML%s calls>" % (BAR, BAR),
         "<%sDSML%s invoke name=\"t\">" % (BAR, BAR), "<%sDSML%s parameter name=\"k\" string=\"true\">" % (BAR, BAR),
         "</%sDSML%s parameter>" % (BAR, BAR), "</%sDSML%s invoke>" % (BAR, BAR),
         "<|DSML| calls>", "</|DSML| parameter>", "<think>", "</think>", "<thinking>", "<args>", "<params>", "<div>", "</div>",
         "List<String>", "a < b", "x<y>z", "<", ">", "<di", "<run", "<para", "print(1)", "hello ", " world", "\n", "  ",
         "中文", "<tool_calls>", "</tool_calls>", "<function_call>", "<function_calls>", "ls -l", "\"quoted\"", "<br/>"]
rng = random.Random(7)
texts = []
for _ in range(400):
    texts.append("".join(rng.choice(FRAGS) for _ in range(rng.randint(1, 14))))
texts += ["just a normal answer", "use a <parameter>less approach", "", "<", "<run_code><parameter name=\"code\">print(1)",
          "<run_code><parameter name=\"code\">print(1)</parameter>\n<parameter name=\"description\">say hi</%sDSML%s parameter>\n</run_code></invoke></%sDSML%s parameter></%sDSML%s invoke></%sDSML%s calls>" % ((BAR,)*8),
          "<%sDSML%s calls><%sDSML%s invoke name=\"run_shell\"><%sDSML%s parameter name=\"command\" string=\"true\">ls -l</%sDSML%s parameter></%sDSML%s invoke></%sDSML%s calls>" % ((BAR,)*12)]
def norm_calls(c):
    return None if c is None else [{"name": x["name"], "arguments": x["arguments"]} for x in c]
parse_cases = []
for tx in texts:
    c, cl = P.parse_leaked_tool_calls(tx)
    d, dcl = P.parse_dsml_tool_calls(tx)
    msg = {"role": "assistant", "content": tx}
    changed = P.repair_leaked_tool_calls(msg)
    parse_cases.append({"text": tx, "leaked": norm_calls(c), "leaked_clean": cl, "dsml": norm_calls(d), "dsml_clean": dcl,
                        "repair_changed": changed, "repair_content": msg["content"],
                        "repair_calls": [{"name": t["function"]["name"], "arguments": t["function"]["arguments"]} for t in msg.get("tool_calls", [])]})
w("internal/gateway/leak/testdata/parse.json", parse_cases)

def run_filter(chunks):
    f = P.LeakedToolCallStreamFilter()
    steps = []
    for ch in chunks:
        acts = f.feed(ch)
        steps.append([("text", a[1]) if a[0] == "text" else ("calls", norm_calls(a[1])) for a in acts])
    fl = f.flush()
    steps.append([("text", a[1]) if a[0] == "text" else ("calls", norm_calls(a[1])) for a in fl])
    return [[{"k": k, "v": v} for k, v in st] for st in steps]
filter_cases = []
for tx in texts:
    whole = [tx]
    pieces = []
    i = 0
    while i < len(tx):
        n = rng.randint(1, 7); pieces.append(tx[i:i + n]); i += n
    for chunks in (whole, pieces):
        filter_cases.append({"chunks": chunks, "steps": run_filter(chunks)})
w("internal/gateway/leak/testdata/filter.json", filter_cases)

# ---- model realm detection ----
ms = ["", "gpt-6-astra", "GPT-5.5", "gpt-anything", "gemini-x", "grok-4.7", "deepseek-v4-pro", "deepseek-v4-pro-max", "minimax-m3",
      "minimax-zzz", "glm-5.1", "glm-5.3", "kimi-k3-1", "kimi-k2.6", "hy3", "hy3-x", "deepseek-v4.1-flash", "unknown-model"]
out = []
for cur in ("intl", "cn"):
    P.CURRENT_REALM = cur
    for m in ms:
        out.append({"model": m, "current": cur, "out": P.detect_model_realm(m)})
w("internal/realm/testdata/model_realm.json", out)

# ---- outbound identity headers (order + values) ----
import re as _re, itertools
os.environ["WB_IDENTITY_OS"] = "Windows"; os.environ["WB_IDENTITY_ARCH"] = "x64"; os.environ["WB_IDENTITY_NODE_VERSION"] = "v22.21.1"
os.environ["WB_VSCODE_IDE_VERSION"] = "1.119.0"
def _kind(v):
    v = str(v)
    if _re.fullmatch(r"[0-9a-f]{32}", v): return "hex32"
    if _re.fullmatch(r"[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}", v): return "uuid"
    if _re.fullmatch(r"[0-9a-f]{16}", v): return "hex16"
    if _re.fullmatch(r"\d{13}", v): return "ms13"
    if _re.fullmatch(r"00-[0-9a-f]{32}-[0-9a-f]{16}-01", v): return "traceparent"
    if _re.fullmatch(r"[0-9a-f]{32}-[0-9a-f]{16}-1", v): return "b3_3"
    if _re.fullmatch(r"[0-9a-f]{32}-[0-9a-f]{16}-1-[0-9a-f]{16}", v): return "b3_4"
    return None
cases = []
for product, rl, ent, sess, model, conv, turn in itertools.product(
        ("workbuddy", "vscode", "cli"), ("intl", "cn"), ("", "ent-1"), (None, "sess-1"), (None, "glm-5.3"), (None, "fixed-conv"), (None, "turn-1")):
    tok = T_INTL if rl == "intl" else T_CN
    acc = A.Account({"uid": "uid-x", "accessToken": tok, "realm": rl, "product": product, "enterpriseId": ent}, None)
    runs = [acc.headers(purpose="chat", session_key=sess, turn_request_id=turn, model=model, conversation_id=conv) for _ in range(2)]
    hs = []
    for (n1, v1), (n2, v2) in zip(runs[0].items(), runs[1].items()):
        assert n1 == n2
        if n1.startswith("monitor_"): hs.append({"name": n1, "kind": "ms13"}); assert _kind(v1) == "ms13"
        elif v1 == v2: hs.append({"name": n1, "value": v1})
        else: hs.append({"name": n1, "kind": _kind(v1) or "?"}); assert _kind(v1) == _kind(v2) and _kind(v1)
    cases.append({"product": product, "realm": rl, "enterprise": ent, "session": sess, "model": model, "conv": conv, "turn": turn,
                  "token": tok, "headers": hs})
w("internal/upstream/identity/testdata/headers.json", cases)
# derived-id vectors
w("internal/upstream/identity/testdata/conv_ids.json", [
    {"uid": u, "key": k, "out": __import__("wb_identity").conversation_id_for(u, k)}
    for u in ("u1", "uid-x", "") for k in ("", "  ", "sess", "会话-1")])

# ---- str.title() (legacy refresh/default header names) ----
names = ["Content-Type", "X-IDE-Type", "X-CodeBuddy-Request", "X-Refresh-Token", "x-stainless-lang", "b3", "X-B3-TraceId", "monitor_httpSendTime", "A1b", "it's", ""]
w("internal/refresh/testdata/title.json", [{"in": n, "out": n.title()} for n in names])
