"""Golden vectors for internal/catalog (legacy catalogue logic).

Run:  py -I tools/genvectors/gen_catalog.py
Writes internal/catalog/testdata/catalog.json
"""
import copy
import json
import os
import sys
import tempfile

sys.dont_write_bytecode = True
ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
sys.path.insert(0, os.path.join(ROOT, "legacy", "python"))
os.environ.setdefault("WB_USAGE_CACHE", "0")
import wb_catalog  # noqa: E402
import wb_modelsdev  # noqa: E402
import wb_proxy as P  # noqa: E402


def dump(v):
    return json.dumps(v, ensure_ascii=False, separators=(",", ":"))


empty_dir = tempfile.mkdtemp(prefix="wbcat-")
P.ACCOUNTS_DIR = empty_dir
CLAMPS = {"glm-5.2": 8000, "hy3": 4096}
P.wb_probes.clamp_for = lambda directory, model: CLAMPS.get(str(model))

out = {"clamps": CLAMPS}

# ---- is_chat_model
ids = ["", "lite", "auto", "default-model", "fast-model", "deep-model", "codewise-x", "completion-a",
       "foo-image-alpha", "foo-image-alpha-edit", "bar-taco-completion", "glm-5.3", "hy3",
       "gpt-5.5", "x-image-alpha-2", "lite2", "Auto"]
out["is_chat"] = [{"id": i, "want": bool(P.is_chat_model(i))} for i in ids]

# ---- remote payloads
intl = list(wb_catalog.STATIC_INTL_MODELS)
cn = list(wb_catalog.STATIC_CN_MODELS)


def meta_of(m, **kw):
    d = copy.deepcopy(m)
    d.update(kw)
    return d


def agents_payload(model_ids, metas, envelope=True, agent_name="cli", as_dict=False):
    body = {"agents": ({agent_name: {"models": model_ids}} if as_dict
                       else [{"name": agent_name, "models": model_ids}]),
            "models": metas}
    return {"code": 0, "data": body} if envelope else body


intl_ids = [m["id"] for m in intl]
payloads = {
    "full_intl": agents_payload(intl_ids + ["default-model", "auto", "lite", "glm-5.3-sg", "hy3-x"], intl),
    "no_envelope": agents_payload(intl_ids, intl, envelope=False),
    "agents_dict": agents_payload(intl_ids, intl, as_dict=True),
    "other_agent": agents_payload(intl_ids, intl, agent_name="craft"),
    "two_lists": {"data": {"agents": [{"name": "craft", "models": intl_ids[:3]},
                                      {"name": "cli", "models": intl_ids[:5]},
                                      {"name": "cli", "models": intl_ids[:5][::-1]}],
                           "models": intl}},
    "dup_ids": agents_payload(["a", "b", "a", " c ", "", "b"], []),
    "empty_list": {"data": {"agents": [{"name": "cli", "models": []}], "models": intl}},
    "no_agents": {"data": {"models": intl}},
    "bad": [1, 2],
    "meta_first_wins": {"agents": [{"name": "cli", "models": ["m1"]}],
                        "models": [{"id": "m1", "credits": "x1.00"}],
                        "extra": {"models": [{"id": "m1", "credits": "x9.00"}, {"id": "m2"}]}},
}
parsed = {}
for name, pl in payloads.items():
    r = P.parse_remote_catalog(copy.deepcopy(pl))
    parsed[name] = {"payload": pl, "want": None if r is None else {"ids": r[0], "meta": r[1]}}
out["parse"] = parsed

# ---- curate
curate_cases = []


def add_curate(realm, ids_, meta):
    want = P.curate_remote_catalog(realm, list(ids_), copy.deepcopy(meta))
    curate_cases.append({"realm": realm, "ids": ids_, "meta": meta, "want": want})


add_curate("intl", intl_ids + ["default-model", "auto", "glm-5.3-sg", "hy3-x", "lite"], {m["id"]: m for m in intl})
add_curate("cn", [m["id"] for m in cn] + ["hy4-preview", "hy4-preview-f"], {m["id"]: m for m in cn})
add_curate("cn", ["hy4-preview", "hy3"], {"hy4-preview": {"id": "hy4-preview", "credits": "x1.00"}})
add_curate("cn", ["hy4-preview", "hy4-preview-f"],
           {"hy4-preview": {"id": "hy4-preview", "credits": "x1.00"},
            "hy4-preview-f": {"id": "hy4-preview-f", "credits": "x0.00"}})
add_curate("intl", ["m", "m-f"], {"m": {"credits": "x1.00"}, "m-f": {"credits": "x0"}})
add_curate("intl", ["m", "m-f"], {"m": {"credits": "x0.00"}, "m-f": {"credits": "x0.00"}})
add_curate("intl", ["z-f", "z"], {"z": {"credits": " X0.00 "}, "z-f": {"credits": "x2"}})
add_curate("intl", ["only-f"], {})
add_curate("intl", [], {})
out["curate"] = curate_cases

# ---- merge
merge_cases = []


def add_merge(realm, live, extras):
    want = P.merge_catalog(copy.deepcopy(live), realm=realm, extras=extras)
    merge_cases.append({"realm": realm, "extras": extras,
                        "live": [{"id": a, "meta": b} for a, b in live],
                        "want": [{"id": a, "meta": b} for a, b in want]})


def live_from(models, ids_=None):
    ids_ = ids_ or [m["id"] for m in models]
    by = {m["id"]: m for m in models}
    return [(i, by.get(i) or {}) for i in ids_]


for realm in ("intl", "cn", "all"):
    add_merge(realm, [], False)
    add_merge(realm, [], True)
    add_merge(realm, live_from(intl if realm != "cn" else cn), False)
    add_merge(realm, live_from(intl if realm != "cn" else cn), True)
brand_new = {"id": "brand-new-x9", "name": "Brand New", "credits": "x2.50", "supportsToolCall": True,
             "reasoning": {"supportedEfforts": ["low", "high"], "effort": "high"}}
no_credit = {"id": "creditless", "name": "Creditless"}
add_merge("intl", live_from(intl) + [("brand-new-x9", brand_new), ("creditless", no_credit)], True)
add_merge("intl", live_from(intl) + [("brand-new-x9", brand_new), ("creditless", no_credit)], False)
add_merge("intl", [("hy4-preview", {"id": "hy4-preview", "credits": "x1.00", "name": "HY4"}), ("hy3", {})], True)
add_merge("cn", [("hy4-preview", {"id": "hy4-preview", "credits": "x1.00", "name": "HY4"}), ("hy3", {})], False)
add_merge("cn", [("hy4-preview-f", {"id": "hy4-preview-f"}), ("hy4-preview", {"id": "hy4-preview"})], False)
add_merge("intl", [("glm-5.3", {"id": "glm-5.3", "reasoning": {"effort": "medium"}}),
                   ("kimi-k3", {"id": "kimi-k3", "reasoning": {"supportedEfforts": ["a", "b"], "effort": "b"}})], False)
add_merge("cn", [("auto", {}), ("lite", {}), ("codewise-z", {"id": "codewise-z"}), ("glm-5.3", {})], True)
out["merge"] = merge_cases

# ---- model_entry (created pinned to 0 by overwriting)
entry_cases = []
all_models = [(m["id"], m) for m in intl] + [(m["id"], m) for m in cn]
extra_meta = [
    ("glm-5.3-flash", {"supportsImages": True, "name": "G"}),
    ("v1", {"supportsImages": True, "disabledMultimodal": True, "supportsToolCall": True, "supportsReasoning": 1}),
    ("v2", {"supportsImages": 1, "tags": ["a", "badge:x", 3, "b"], "isDefault": True, "temperature": 0, "top_p": 0.9,
            "maxInputTokens": "123456", "maxOutputTokens": 4096.0, "onlyReasoning": 0, "credits": "x1.00",
            "vendor": "acme", "contextWindow": {"supportedLengths": [1000, 2000]},
            "reasoning": {"supportedEfforts": ["low"], "effort": "low", "defaultEffort": "low", "canDisableThinking": False}}),
    ("glm-5.2", {}),
    ("hy3", {"maxOutputTokens": 0}),
    ("some/NS-Model", {"descriptionZh": "中文", "descriptionEn": ""}),
    ("empty", {}),
]
for mid, meta in all_models + extra_meta:
    item = P.model_entry(mid, copy.deepcopy(meta))
    item["created"] = 0
    entry_cases.append({"id": mid, "meta": meta, "want": dump(item)})
out["entry"] = entry_cases

# ---- limits
lim = []
index = {"foo": (777, 55), "glm-5.3": (1, 2), "only-ctx": (500, 0)}
for mid, rc, ro in [("glm-5.3", 0, 0), ("glm-5.3", 5, 6), ("foo", 0, 0), ("ns/FOO", 0, 0), ("only-ctx", 0, 0),
                    ("unknown", 0, 0), ("auto", 0, 0), ("kimi-k2.8-preview", 0, 0), ("x", "9000", None),
                    ("x", "bad", 3.0), ("hy3", None, 10)]:
    c, o, _ = wb_modelsdev.lookup(mid, rc, ro, index=index)
    lim.append({"id": mid, "rc": rc, "ro": ro, "want": [c, o]})
out["limits"] = {"index": {k: list(v) for k, v in index.items()}, "cases": lim}

# ---- models.dev index
doc = {
    "openai": {"models": {"gpt-9": {"limit": {"context": 100, "output": 10}}}},
    "agg1": {"models": {"openai/gpt-9": {"limit": {"context": 200, "output": 20}},
                        "m1": {"limit": {"context": 5, "output": 1}},
                        "NS/M2": {"limit": {"context": 7, "output": 0}}}},
    "agg2": {"models": {"m1": {"limit": {"context": 5, "output": 1}}, "m2": {"limit": {"context": 7, "output": 0}},
                        "bad": {"limit": {"context": 0, "output": 0}}, "huge": {"limit": {"context": 10 ** 10}},
                        "tie": {"limit": {"context": 9, "output": 9}}}},
    "agg3": {"models": {"m1": {"limit": {"context": 6, "output": 1}}, "tie": {"limit": {"context": 3, "output": 3}}}},
    "zai": "not a dict",
    "deepseek": {"models": {"deepseek-v9": {"limit": {"context": 64, "output": 8}}}},
}
out["modelsdev"] = {"doc": doc, "want": {k: list(v) for k, v in wb_modelsdev.build_index(doc).items()}}

# ---- catalog-derived request defaults (cold cache: bundled tables only)
defaults = []
for mid in [m["id"] for m in intl + cn] + ["nope", "GLM-5.3", " glm-5.3 ", ""]:
    P._models_cache["intl"]["data"] = None
    P._models_cache["cn"]["data"] = None
    defaults.append({"id": mid, "max_tokens": P.model_default_max_output_tokens(mid) or 0,
                     "effort": P.model_default_effort(mid) or ""})
out["defaults"] = defaults

dest = os.path.join(ROOT, "internal", "catalog", "testdata", "catalog.json")
os.makedirs(os.path.dirname(dest), exist_ok=True)
with open(dest, "w", encoding="utf-8", newline="\n") as fh:
    json.dump(out, fh, ensure_ascii=False)
print("wrote", dest, os.path.getsize(dest), "bytes;",
      {k: (len(v) if hasattr(v, "__len__") else v) for k, v in out.items()})
