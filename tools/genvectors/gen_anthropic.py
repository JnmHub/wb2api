"""Golden vectors for internal/gateway/anthropic (legacy Messages <-> chat code).

Run:  py -I tools/genvectors/gen_anthropic.py
"""
import copy
import json
import os
import sys

sys.dont_write_bytecode = True
ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
sys.path.insert(0, os.path.join(ROOT, "legacy", "python"))
os.environ.setdefault("WB_USAGE_CACHE", "0")
import wb_proxy as P  # noqa: E402

_c = {}


def fake_id(prefix):
    _c[prefix] = _c.get(prefix, 0) + 1
    return "%s%d" % (prefix, _c[prefix])


P._new_id = fake_id
P._models_cache["intl"]["data"] = None
P._models_cache["cn"]["data"] = None


def dump(v):
    return json.dumps(v, ensure_ascii=False, separators=(",", ":"))


TOOL = {"name": "get_weather", "description": "d", "input_schema": {"type": "object", "properties": {"c": {"type": "string"}}}}
IMG = {"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "QUJD"}}
cases = [
    {"model": "glm-5.3", "max_tokens": 100, "messages": [{"role": "user", "content": "hi"}]},
    {"model": "glm-5.3", "messages": [{"role": "user", "content": "hi"}], "system": "be brief", "temperature": 0.3, "top_p": 0.9,
     "stop_sequences": ["STOP"], "metadata": {"user_id": "u1"}, "stream": True},
    {"model": "glm-5.3", "messages": [{"role": "user", "content": "hi"}], "system": [{"type": "text", "text": "a"}, {"type": "text", "text": "b"}, "x"]},
    {"model": "glm-5.3", "tools": [TOOL, {"type": "web_search_20250305", "name": "web_search"}, {"function": {"name": "f"}, "type": "function"}],
     "tool_choice": {"type": "tool", "name": "get_weather", "disable_parallel_tool_use": True},
     "messages": [{"role": "user", "content": "weather?"}]},
    {"model": "glm-5.3", "tool_choice": "any", "messages": [{"role": "user", "content": "x"}]},
    {"model": "glm-5.3", "tool_choice": {"type": "auto"}, "messages": [{"role": "user", "content": "x"}]},
    {"model": "glm-5.3", "tool_choice": {"type": "none"}, "messages": [{"role": "user", "content": "x"}]},
    {"model": "glm-5.3", "tool_choice": "AUTO", "messages": [{"role": "user", "content": "x"}]},
    {"model": "glm-5.3", "thinking": {"type": "enabled", "budget_tokens": 9000}, "messages": [{"role": "user", "content": "x"}]},
    {"model": "glm-5.3", "thinking": {"type": "enabled", "budget_tokens": 40000}, "messages": [{"role": "user", "content": "x"}]},
    {"model": "glm-5.3", "thinking": {"type": "enabled", "budget_tokens": 100}, "messages": [{"role": "user", "content": "x"}]},
    {"model": "glm-5.3", "thinking": {"type": "enabled"}, "messages": [{"role": "user", "content": "x"}]},
    {"model": "glm-5.3", "thinking": {"type": "disabled"}, "messages": [{"role": "user", "content": "x"}]},
    {"model": "glm-5.3", "output_config": {"effort": "high"}, "thinking": {"type": "disabled"}, "messages": [{"role": "user", "content": "x"}]},
    {"model": "glm-5.3", "messages": [
        {"role": "user", "content": [{"type": "text", "text": "look "}, IMG, {"type": "text", "text": "here"}]},
        {"role": "assistant", "content": [{"type": "thinking", "thinking": "hm"}, {"type": "text", "text": "ok "},
                                           {"type": "tool_use", "id": "tu1", "name": "get_weather", "input": {"c": "P"}}]},
        {"role": "user", "content": [{"type": "tool_result", "tool_use_id": "tu1", "content": "sunny"},
                                      {"type": "tool_result", "tool_use_id": "tu2", "content": [{"type": "text", "text": "a"}, {"type": "text", "text": "b"}], "is_error": True},
                                      {"type": "tool_result", "tool_use_id": "tu3", "content": [IMG, {"type": "text", "text": "cap"}]},
                                      {"type": "tool_result", "tool_use_id": "tu4", "content": [IMG], "is_error": True},
                                      {"type": "tool_result", "tool_use_id": "tu5", "content": [{"type": "image", "source": {"type": "url", "url": ""}}]},
                                      {"type": "tool_result", "tool_use_id": "tu6"},
                                      {"type": "text", "text": "thanks"}]},
        {"role": "assistant", "content": "plain"}]},
    {"model": "glm-5.3", "messages": [
        {"role": "user", "content": [{"type": "document", "title": "T", "source": {"type": "text", "data": "body"}},
                                      {"type": "document", "source": {"type": "base64", "media_type": "application/pdf", "data": "x"}},
                                      {"type": "document", "source": {"type": "url", "url": "http://d"}},
                                      {"type": "document", "source": {"type": "content", "content": [{"type": "text", "text": "in"}]}},
                                      {"type": "image", "source": {"type": "file", "file_id": "f1"}},
                                      {"type": "image", "source": {"type": "base64"}}]}]},
    {"model": "glm-5.3", "messages": [{"role": "system", "content": "mid system"}, {"role": "user", "content": "x"},
                                       {"role": "developer", "content": [{"type": "text", "text": "dev"}]}, {"role": "user", "content": None},
                                       {"role": "assistant", "content": 12}, {"role": "user", "content": []},
                                       {"role": "user", "content": [{"type": "tool_use", "id": "a", "name": "n", "input": "raw"},
                                                                    {"type": "server_tool_use", "id": "b", "name": "m"},
                                                                    {"type": "redacted_thinking"}]}, 5, None]},
    {"model": "hy3", "messages": [{"role": "user", "content": "x"}]},
    {"model": "deepseek-v4.1-flash", "messages": [{"role": "user", "content": "x"}], "max_tokens": None},
    {"model": "", "messages": [{"role": "user", "content": "x"}]},
    {"messages": [{"role": "user", "content": "x"}]},
    {"model": "m", "messages": "no"},
    {"model": "m", "messages": []},
    {"model": "m", "messages": [{"role": "tool", "content": "x"}]},
    {"model": "m", "messages": [{"role": "it's", "content": "x"}]},
]
out = {"to_chat": [], "to_messages": [], "stream": [], "count": []}
for c in cases:
    try:
        chat = P.messages_to_chat(copy.deepcopy(c))
        out["to_chat"].append({"payload": c, "chat": dump(chat), "error": ""})
    except ValueError as exc:
        out["to_chat"].append({"payload": c, "chat": "", "error": str(exc)})
    else:
        out["count"].append({"chat": chat, "want": P._anthropic_estimate_chat_tokens(chat)})


def msg_case(chat):
    _c.clear()
    return {"chat": chat, "want": dump(P.chat_to_messages(copy.deepcopy(chat)))}


def chat_obj(content=None, tool_calls=None, reasoning=None, finish="stop", usage=None, id_="chatcmpl-1", model="glm-5.3"):
    m = {"role": "assistant", "content": content}
    if tool_calls is not None:
        m["tool_calls"] = tool_calls
    if reasoning is not None:
        m["reasoning_content"] = reasoning
    o = {"id": id_, "model": model, "choices": [{"index": 0, "message": m, "finish_reason": finish}]}
    if usage is not None:
        o["usage"] = usage
    return o


U = {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15, "prompt_tokens_details": {"cached_tokens": 4}, "service_tier": "default"}
out["to_messages"] = [msg_case(x) for x in [
    chat_obj("hello", usage=U),
    chat_obj("hello", finish="length"),
    chat_obj("hi", reasoning="think", finish="tool_calls", tool_calls=[
        {"id": "c1", "type": "function", "function": {"name": "f", "arguments": "{\"a\":1}"}},
        {"id": None, "function": {"name": "g", "arguments": "not json"}},
        {"id": "c3", "function": {"name": "h", "arguments": "[1,2]"}},
        {"id": "c4", "function": {"name": "i"}}, "junk"]),
    chat_obj([{"type": "text", "text": "a"}, {"type": "text", "text": "b"}]),
    chat_obj(None), chat_obj("", finish="content_filter"), chat_obj("x", finish="function_call"),
    chat_obj("x", id_="msg_abc"), chat_obj("x", id_=None, model=None),
    chat_obj("x", usage={"prompt_cache_hit_tokens": 7, "prompt_tokens": 1, "completion_tokens": 2, "prompt_cache_write_tokens": 3}),
    chat_obj("x", usage={"prompt_cache_hit_tokens": None, "cache_read_input_tokens": 6}),
    {"choices": []}, {}, None.__class__ and {"choices": [{"message": None}]},
]]


def line(c):
    return ("data: " + json.dumps(c, ensure_ascii=False) + "\n").encode("utf-8")


def ch(content=None, reasoning=None, tool_calls=None, finish=None, usage=None, id_="c1", model=None):
    d = {}
    if content is not None:
        d["content"] = content
    if reasoning is not None:
        d["reasoning_content"] = reasoning
    if tool_calls is not None:
        d["tool_calls"] = tool_calls
    o = {"id": id_, "choices": [{"delta": d, "finish_reason": finish}]}
    if model:
        o["model"] = model
    if usage is not None:
        o["usage"] = usage
    return o


def tc(index, id_=None, name=None, args=None):
    fn = {}
    if name is not None:
        fn["name"] = name
    if args is not None:
        fn["arguments"] = args
    d = {"index": index, "function": fn}
    if id_ is not None:
        d["id"] = id_
    return d


def stream_case(chunks, raw=()):
    _c.clear()
    lines = []
    for c in chunks:
        lines.append(line(c))
        lines.append(b"\n")
    for r in raw:
        lines.append(r.encode("utf-8"))
    lines.append(b"data: [DONE]\n")
    holder = {}
    frames = [f.decode("utf-8") for f in P.stream_messages_events(iter(lines), "glm-5.3", holder)]
    return {"chunks": chunks, "raw": list(raw), "want": "".join(frames), "usage": holder.get("usage")}


out["stream"] = [stream_case(x) for x in [
    [ch("Hel"), ch("lo"), ch(None, finish="stop"), ch(None, usage=U)],
    [ch(None, reasoning="think"), ch("answer", model="glm-5.3-x"), ch(None, finish="length", usage=U)],
    [ch(None, tool_calls=[tc(0, "tu1", "f", "")]), ch(None, tool_calls=[tc(0, None, None, "{\"a\":")]),
     ch(None, tool_calls=[tc(0, None, None, "1}")]), ch(None, finish="tool_calls", usage=U)],
    [ch("let me see "), ch(None, tool_calls=[tc(0, "a", "f", "{}")]), ch(None, tool_calls=[tc(1, None, "g", "{\"x\":1}")]), ch(None, finish="tool_calls")],
    [ch(None, reasoning="r"), ch(None, tool_calls=[tc(0, None, "f", None)]), ch(None, tool_calls=[tc(0, None, None, "{}")])],
    [ch(None, tool_calls=[tc(0, None, None, None)]), ch("after")],
    [], [ch(None, usage=U)],
    [{"error": {"message": "boom"}}], [ch("x"), {"error": {}}],
    [ch("é中文🙂", finish="stop")],
    [{"id": "c9", "choices": [{"delta": {}, "finish_reason": "stop"}], "usage": {"prompt_tokens": 3, "completion_tokens": 1}}],
]] + [stream_case([ch("k")], raw=[": ping\n", "data: nope\n", "data:data: {\"id\":\"z\",\"choices\":[{\"delta\":{\"content\":\"!\"}}]}\n"])]

dest = os.path.join(ROOT, "internal", "gateway", "anthropic", "testdata", "anthropic.json")
os.makedirs(os.path.dirname(dest), exist_ok=True)
with open(dest, "w", encoding="utf-8", newline="\n") as fh:
    json.dump(out, fh, ensure_ascii=False)
print("wrote", dest, {k: len(v) for k, v in out.items()})
