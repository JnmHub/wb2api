"""Independent aggregates of the legacy sample usage log (plain Python arithmetic),
used to check the SQLite rollups after importing the same file.

Run:  py -I tools/genvectors/gen_usage.py
"""
import json
import os
from collections import defaultdict

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
SAMPLE = os.path.join(ROOT, "legacy", "python", "tests", "fixtures", "pricing_tooltip", "usage_sample.jsonl")
OUT = os.path.join(ROOT, "internal", "usagedb", "testdata", "sample_expected.json")


def blank():
    return {"requests": 0, "ok": 0, "aborted": 0, "failed": 0, "prompt": 0, "completion": 0, "reasoning": 0,
            "cached": 0, "total": 0, "credit": 0.0, "gen_ms": 0, "gen_tokens": 0}


def add(a, row):
    a["requests"] += 1
    outcome = row.get("outcome") or ("failed" if row.get("error") else "completed")
    if outcome in ("completed", ""):
        a["ok"] += 1
    elif outcome == "client_aborted":
        a["aborted"] += 1
    else:
        a["failed"] += 1
    a["prompt"] += int(row.get("prompt_tokens") or 0)
    a["completion"] += int(row.get("completion_tokens") or 0)
    a["reasoning"] += int(row.get("reasoning_tokens") or 0)
    a["cached"] += int(row.get("cached_tokens") or 0)
    a["total"] += int(row.get("total_tokens") or 0)
    a["credit"] += float(row.get("credit") or 0)
    gen = row.get("gen_ms")
    if gen is not None and gen > 0:
        a["gen_ms"] += int(gen)
        a["gen_tokens"] += int(row.get("completion_tokens") or 0)


total = blank()
by_model = defaultdict(blank)
by_account = defaultdict(blank)
first = last = None
n = 0
with open(SAMPLE, encoding="utf-8") as fh:
    for line in fh:
        line = line.strip()
        if not line:
            continue
        row = json.loads(line)
        n += 1
        add(total, row)
        add(by_model[row.get("model") or ""], row)
        add(by_account[row.get("account") or ""], row)
        at = row["at"]
        first = at if first is None else min(first, at)
        last = at if last is None else max(last, at)
os.makedirs(os.path.dirname(OUT), exist_ok=True)
with open(OUT, "w", encoding="utf-8") as fh:
    json.dump({"rows": n, "first_at": first, "last_at": last, "total": total,
               "by_model": by_model, "by_account": by_account}, fh, indent=1)
print("rows", n, "models", len(by_model), "accounts", len(by_account), "->", OUT)
