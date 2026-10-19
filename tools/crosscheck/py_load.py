"""Load a Go-written accounts dir with the LEGACY Python loader and compare it
against the attributes Python derived from its own files (py_expected.json).

  WB_CROSSCHECK_OUT=<dir> go test ./internal/store/accounts -run PythonCrossCheck
  py -I tools/crosscheck/py_load.py <dir>
"""
import json, os, sys, tempfile
sys.dont_write_bytecode = True
ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
sys.path.insert(0, os.path.join(ROOT, "legacy", "python"))
import wb_accounts as A
d = sys.argv[1]
pool = A.AccountPool(d)
pool.load()
exp = json.load(open(os.path.join(ROOT, "internal/store/accounts/testdata/py_expected.json"), encoding="utf-8"))
got = {os.path.basename(a.path): a for a in pool.accounts}
bad = 0
for e in exp:
    a = got.get(e["file"])
    if a is None:
        print("MISSING", e["file"]); bad += 1; continue
    actual = {"uid": a.uid, "nickname": a.nickname, "domain": a.domain, "realm": a.realm, "platform": a.platform,
              "product": a.product, "enterprise_id": a.enterprise_id, "access_token": a.access_token,
              "refresh_token": a.refresh_token, "expires_at": a.expires_at, "source": a.source,
              "proxy_slot": a.proxy_slot, "proxy_legacy": a.proxy_legacy, "enabled": bool(a.enabled),
              "weight": a.weight, "note": a.note, "disabled_models": list(a.disabled_models),
              "restricted_until": a.restricted_until, "credits": a.credits, "last_checkin": a.last_checkin,
              "last_daily_chat": a.last_daily_chat, "last_activity_report": a.last_activity_report,
              "model_caps": a.model_caps}
    for k, v in actual.items():
        if v != e[k]:
            print("DIFF", e["file"], k, repr(v), "!=", repr(e[k])); bad += 1
print("python loader OK on Go-written files" if not bad else "%d differences" % bad)
sys.exit(1 if bad else 0)
