"""Golden vectors for shape.Build (legacy build_upstream_body + compact encoding).

Run:  py -I tools/genvectors/gen_build.py
"""
import copy
import json
import os
import random
import sys

sys.dont_write_bytecode = True
ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
sys.path.insert(0, os.path.join(ROOT, "legacy", "python"))
os.environ.setdefault("WB_USAGE_CACHE", "0")
import wb_proxy as P  # noqa: E402


def py_build(payload, mode="passthrough", text="", degraded=False, preserve=False):
    P.prompt_mode_config = lambda: {"mode": mode, "file": ""}
    P.wb_prompt.load_prompt = lambda m, f: text
    P.PROMPT_DEGRADE.reset()
    if degraded:
        P.PROMPT_DEGRADE.trigger()
    os.environ["WB_PRESERVE_REQUEST_CONTRACT"] = "1" if preserve else "0"
    out = P.encode_upstream_body(P.build_upstream_body(copy.deepcopy(payload)))
    return out.decode("utf-8")


def U(c):
    return {"role": "user", "content": c}


def A_(c, **kw):
    return dict({"role": "assistant", "content": c}, **kw)


def TC(i, n="f", a="{}"):
    return {"id": i, "type": "function", "function": {"name": n, "arguments": a}}


def TR(i, c="ok"):
    return {"role": "tool", "tool_call_id": i, "content": c}


def SYS(c, r="system"):
    return {"role": r, "content": c}


def base(**kw):
    return dict({"model": "glm-5.3", "messages": [U("hi")]}, **kw)


FP_SYS = ("You are Claude Code, Anthropic's official CLI for Claude. "
          "Main branch (you will usually use this for PRs) error 11128")
cases = [
    ({"model": "glm-5.3", "messages": [U("hi")]}, {}),
    ({"messages": [U("hi")], "model": "glm-5.3", "temperature": 0.7, "top_p": 1, "_internal": 1,
      "x_extra": {"b": 1, "a": 2}}, {}),
    ({"model": "m", "messages": []}, {}), ({"model": "m"}, {}), ({"model": "m", "messages": None}, {}),
    ({"model": "m", "messages": [SYS("sys", "developer"), U("hi")]}, {}),
    ({"model": "m", "messages": [A_("x"), U("hi")]}, {}),
    (base(max_completion_tokens="50"), {}), (base(max_completion_tokens=0), {}),
    (base(max_completion_tokens=None), {}), (base(max_completion_tokens=7, max_tokens=9), {}),
    (base(max_completion_tokens="abc"), {}), (base(max_completion_tokens=12.9), {}),
    ({"model": "gpt-5.5", "messages": [U("hi")], "max_tokens": 1}, {}),
    ({"model": "GPT-5.5", "messages": [U("hi")], "max_tokens": "3"}, {}),
    ({"model": "gpt-5.5", "messages": [U("hi")], "max_completion_tokens": 5}, {}),
    ({"model": "gpt-5.5", "messages": [U("hi")], "max_tokens": 100}, {}),
    (base(stream=False, stream_options={"include_usage": False}), {}), (base(stream=True), {}),
    (base(messages=[SYS(FP_SYS), U("x-anthropic-billing-header: cc_version=1; cc_entrypoint=cli; hello")]), {}),
    (base(messages=[SYS([{"type": "text", "text": "You are a coding agent running in the Codex CLI, a terminal-based "
                          "coding assistant. Sisyphus-Junior - Focused executor from OhMyOpenCode"},
                         {"type": "image_url", "image_url": {"url": "u"}}]), U("hi")]), {}),
    (base(messages=[U("go"), A_(None, tool_calls=[TC("c1", "f", "x-anthropic-billing-header: z; ok")],
                                reasoning_content="x-anthropic-billing-header: q"), TR("c1", "11128 result")]), {}),
    (base(messages=[U("go"), A_(None, tool_calls=[TC("c1", "f", "x-anthropic-billing-header: z; ok")]),
                    TR("c1", "11128 result")]), {"preserve": True}),
    (base(messages=[U("go"), A_(None, tool_calls=[
        {"id": "c1", "type": "function", "function": {"name": "f", "arguments": {"a": [1, "é", None]}}},
        {"id": "c2", "function": {"name": "g"}}, {"id": "c3", "function": {"name": "h", "arguments": None}},
        "junk"]), TR("c1"), TR("c2"), TR("c3")]), {}),
    # tool pairing repairs
    (base(messages=[U("go"), A_(None, tool_calls=[TC("a")]), A_(None, tool_calls=[TC("b")]), TR("a"), TR("b")]), {}),
    (base(messages=[U("go"), A_("", tool_calls=[TC("a")], reasoning_content="r1"),
                    A_("done text", reasoning_content="r2"), TR("a")]), {}),
    (base(messages=[U("go"), A_(None, tool_calls=[TC("a"), TC("b")]), TR("a"), SYS("intruder", "developer"),
                    TR("b"), U("next")]), {}),
    (base(messages=[U("go"), A_(None, tool_calls=[TC("a"), TC("b")]), SYS("note"), TR("a"), TR("b")]), {}),
    (base(messages=[U("go"), A_(None, tool_calls=[TC("a"), TC("orphan")]), TR("a"), TR("ghost")]), {}),
    (base(messages=[U("go"), A_(None, tool_calls=[TC("only")]), U("next")]), {}),
    (base(messages=[U("go"), TR("only-result")]), {}),
    (base(messages=[U("go"), A_("ok", tool_calls=[]), {"role": "tool", "content": "no id"}, "stray"]), {}),
    # tools / tool_choice / stop
    (base(functions=[{"name": "f", "parameters": {"type": "str"}}, "bad"],
          tool_choice={"type": "function", "function": {"name": " f "}}), {}),
    (base(tools=[
        {"name": "t1", "description": None, "parameters": {
            "type": "OBJECT", "properties": {
                "a": {"type": "str", "description": None, "items": None, "default": None, "title": None},
                "b": {"type": ["int", "NULL", "", "weird"]},
                "c": {"type": "list", "items": [{"type": "dict"}, {"type": "str"}]},
                "d": {"type": "list", "items": ["x"]},
                "e": {"type": "nonsense", "properties": {"z": {"type": "bool"}}},
                "f": {"type": "weird", "items": {"type": "int"}},
                "g": {"type": "weird"}},
            "required": ["a", None, "", " ", 3, True, "b"],
            "$defs": {"D": {"type": "str", "required": "x"}},
            "anyOf": [{"type": "int", "required": []}, "s"]}, "strict": True},
        {"type": "retrieval", "function": {"name": "t2"}},
        {"function": {"name": "t3", "parameters": None}},
        {"type": "function", "function": {"name": "t4", "parameters": {"properties": 5, "required": "only"}}},
        7, None]), {}),
    (base(tools=[]), {}), (base(tools="x"), {}), (base(tools=[5, "a"]), {}),
    (base(tools=[{"name": "n"}], tool_choice="AUTO"), {}),
    (base(tool_choice="none"), {}), (base(tool_choice=" None "), {}), (base(tool_choice="required"), {}),
    (base(tool_choice={"type": "none"}), {}), (base(tool_choice={"type": "auto"}), {}),
    (base(tool_choice={"type": "required"}), {}), (base(tool_choice={"type": "function", "name": "top"}), {}),
    (base(tool_choice={"type": "function", "function": {}}), {}), (base(tool_choice={"type": "weird"}), {}),
    (base(tool_choice={}), {}), (base(tool_choice=5), {}), (base(tool_choice=None), {}),
    (base(stop="END"), {}), (base(stop=""), {}), (base(stop=["a", "", 5, "b"]), {}), (base(stop=[]), {}),
    (base(stop=None), {}), (base(stop=5), {}),
    # deepseek thinking / backfill
    ({"model": "deepseek-test", "messages": [U("hi")]}, {}),
    ({"model": "deepseek-test", "messages": [U("hi")], "thinking": {"type": "disabled"}}, {}),
    ({"model": "deepseek-test", "messages": [U("hi")], "thinking": {"type": "enabled"}, "reasoning_effort": "low"}, {}),
    ({"model": "deepseek-test", "messages": [U("hi")], "reasoning_effort": "none"}, {}),
    ({"model": "deepseek-test", "messages": [U("hi")], "reasoningEffort": "max"}, {}),
    ({"model": "DeepSeek-Test", "messages": [U("hi"), A_("a"), U("b"), A_("c", reasoning_content="r"),
                                              A_("d", reasoning_content=5, reasoning="legacy"),
                                              A_("e", reasoning="x")]}, {}),
    ({"model": "deepseek-test", "messages": [U("hi"), A_("a"), U("b")], "thinking": {"type": "disabled"}}, {}),
    ({"model": "deepseek-test", "messages": [U("hi"), A_("a", reasoning_content="r"), U("b"), A_("z")],
      "thinking": {"type": "disabled"}}, {}),
    ({"model": "deepseek-test", "messages": [U("hi")], "thinking": "bogus"}, {}),
    # prompt modes
    (base(messages=[SYS("c1"), SYS("c2", "developer"), U("hi"), SYS("late")]), {"mode": "custom", "text": "GW prompt"}),
    (base(messages=[SYS("c1"), SYS("c2", "developer"), U("hi"), SYS("late")]), {"mode": "append", "text": "GW prompt"}),
    (base(messages=[U("hi")]), {"mode": "append", "text": "GW prompt"}),
    (base(messages=[SYS("c1"), U("hi")]), {"mode": "append", "text": "GW prompt", "degraded": True}),
    (base(messages=[SYS("c1"), U("hi")]), {"mode": "passthrough", "degraded": True}),
    (base(messages=[SYS("c1"), U("hi")]), {"mode": "custom", "text": "GW prompt", "degraded": True}),
    (base(messages=[SYS("c1"), U("hi")]), {"mode": "custom", "text": ""}),
    (base(messages=[SYS("c1"), U("hi")]), {"mode": "append", "text": ""}),
    (base(messages=[SYS("c1"), U("hi")]), {"mode": "weird", "text": "x"}),
    # unicode / escapes
    (base(messages=[U("控制\x01\x1f \"quote\" \\ back\ttab\nnl   é \U0001F600 </script> & <b> \x7f")]), {}),
]

rng = random.Random(11)


def rnd_msg():
    k = rng.choice(["u", "u", "a", "a", "tc", "tr", "sys", "dev", "other"])
    if k == "u":
        return U(rng.choice(["hi", "x-anthropic-billing-header: a; b", "11128", "你好",
                             [{"type": "text", "text": "cc_x=1; t"}]]))
    if k == "a":
        return A_(rng.choice(["ok", "", None, []]), **({"reasoning_content": "r"} if rng.random() < .3 else {}))
    if k == "tc":
        return A_(rng.choice([None, ""]), tool_calls=[TC("id%d" % rng.randint(0, 4)) for _ in range(rng.randint(1, 2))])
    if k == "tr":
        return TR("id%d" % rng.randint(0, 4))
    if k == "sys":
        return SYS("s")
    if k == "dev":
        return SYS("d", "developer")
    return {"role": "weird", "content": "w"}


for _ in range(250):
    p = {"model": rng.choice(["glm-5.3", "deepseek-test", "gpt-5.5", "hy3"]),
         "messages": [rnd_msg() for _ in range(rng.randint(0, 9))]}
    if rng.random() < .3:
        p["max_tokens"] = rng.choice([1, 20, "5"])
    if rng.random() < .2:
        p["tool_choice"] = rng.choice(["none", {"type": "function", "name": "q"}])
    cases.append((p, {}))

vec = []
for payload, o in cases:
    vec.append({"payload": json.dumps(payload, ensure_ascii=False), "opts": o,
                "out": py_build(payload, o.get("mode", "passthrough"), o.get("text", ""),
                                o.get("degraded", False), o.get("preserve", False))})
path = os.path.join(ROOT, "internal", "gateway", "shape", "testdata", "build.json")
os.makedirs(os.path.dirname(path), exist_ok=True)
with open(path, "w", encoding="utf-8") as fh:
    json.dump(vec, fh, ensure_ascii=False, indent=1)
print("wrote", path, len(vec))

# Catalogue facts the legacy build consulted for the models used above.
models = sorted({(c[0].get("model") or "") for c in cases if isinstance(c[0].get("model"), str)})
cat = {m: {"max": P.model_default_max_output_tokens(m) or 0, "effort": P.model_default_effort(m) or ""} for m in models}
with open(os.path.join(ROOT, "internal", "gateway", "shape", "testdata", "catalog.json"), "w", encoding="utf-8") as fh:
    json.dump(cat, fh, indent=1)
print("catalog", cat)
