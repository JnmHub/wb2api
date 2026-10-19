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
