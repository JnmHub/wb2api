"""Golden vectors for internal/gateway/responses (legacy Responses <-> chat code).

Run:  py -I tools/genvectors/gen_responses.py
Writes internal/gateway/responses/testdata/responses.json

Ids are deterministic (a per-prefix counter) and the clock is frozen, so the
Go port can be compared byte for byte.
"""
import copy
import json
import os
import random
import sys
import time

sys.dont_write_bytecode = True
ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
sys.path.insert(0, os.path.join(ROOT, "legacy", "python"))
os.environ.setdefault("WB_USAGE_CACHE", "0")
import wb_proxy as P  # noqa: E402

NOW = 1790000000
time.time = lambda: NOW + 0.7

_counters = {}


def reset_ids():
    _counters.clear()


def fake_id(prefix):
    _counters[prefix] = _counters.get(prefix, 0) + 1
    return "%s%d" % (prefix, _counters[prefix])


P._new_id = fake_id
P.local_web_tools_enabled = lambda: False
P._models_cache["intl"]["data"] = None
P._models_cache["cn"]["data"] = None


def dump(v):
    return json.dumps(v, ensure_ascii=False, separators=(",", ":"))


def to_chat(payload, preserve=False):
    os.environ["WB_PRESERVE_REQUEST_CONTRACT"] = "1" if preserve else "0"
    p = copy.deepcopy(payload)
    all_tools = p.get("tools")
    all_tools = list(all_tools) if isinstance(all_tools, list) else []
    all_tools.extend(P.additional_tools_from_input(p))
    custom = P.custom_tool_names(all_tools)
    chat = P.responses_to_chat(p)
    ns = chat.pop("_namespace_map", None)
    chat.pop("_web_tools", None)
    return {
        "payload": payload, "preserve": preserve,
        "chat": dump(chat), "ns": ns or {}, "custom": sorted(custom),
        "tools": all_tools, "has_ns": ns is not None,
        "tool_choice": p.get("tool_choice", "auto"),
        "parallel": p.get("parallel_tool_calls", True),
    }


def U(text):
    return {"type": "message", "role": "user", "content": [{"type": "input_text", "text": text}]}


def A_(text):
    return {"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": text}]}


FN = {"type": "function", "name": "get_weather", "description": "d",
      "parameters": {"type": "object", "properties": {"city": {"type": "string"}}}}
CUSTOM = {"type": "custom", "name": "apply_patch", "description": "patch it",
          "format": {"type": "grammar", "definition": "start: /.+/"}}
NS = {"type": "namespace", "name": "codex_app", "tools": [
    {"name": "list_threads", "description": "lt", "parameters": {"type": "object", "properties": {}}},
    {"type": "function", "name": "js", "parameters": {"type": "object"}, "strict": True},
    {"type": "custom", "name": "exec"},
    {"name": "list_threads"},
    {"type": "namespace", "name": "inner", "functions": [{"name": "deep", "input_schema": {"type": "object"}}]},
]}

req_cases = [
    {"model": "glm-5.3", "input": "hello"},
    {"model": "glm-5.3", "input": "hello", "instructions": "  be brief  ", "temperature": 0.2, "top_p": 1, "seed": 7},
    {"model": "glm-5.3", "input": "x", "instructions": "   "},
    {"model": "hy3", "input": [U("a"), A_("b"), A_("c"), U("d")], "max_output_tokens": 77},
    {"model": "hy3", "input": [{"role": "developer", "content": "dev rules"}, {"role": "user", "content": "hi"}]},
    {"model": "hy3", "input": ["plain string item", 5, None, {"type": "mystery", "a": 1}]},
    {"model": "deepseek-v4.1-flash", "reasoning": {"effort": "high"}, "input": "x"},
    {"model": "deepseek-v4.1-flash", "reasoning_effort": "low", "input": "x"},
    {"model": "deepseek-v4.1-flash", "reasoning": {"effort": ""}, "reasoning_effort": "medium", "input": "x"},
    {"model": "glm-5.3", "tools": [FN], "tool_choice": "required", "parallel_tool_calls": False,
     "input": [U("weather?"),
               {"type": "reasoning", "summary": [{"type": "summary_text", "text": "thinking..."}]},
               {"type": "function_call", "call_id": "c1", "name": "get_weather", "arguments": "{\"city\":\"X\"}"},
               {"type": "function_call_output", "call_id": "c1", "output": "sunny"},
               A_("it is sunny")]},
    {"model": "glm-5.3", "tools": [FN],
     "input": [{"type": "function_call", "id": "i1", "name": "a"}, {"type": "function_call", "call_id": "c2", "name": "b", "arguments": "{}"},
               {"type": "function_call_output", "call_id": "c2", "output": {"k": "v"}},
               {"type": "function_call_output", "call_id": "c3", "output": [{"type": "output_text", "text": "x"}, {"type": "output_text", "text": "y"}]},
               {"type": "function_call_output", "call_id": "c4", "output": 12},
               {"type": "function_call_output", "call_id": "c5", "output": None},
               {"type": "function_call_output", "call_id": "c6", "output": {"type": "input_image", "image_url": "http://i/x.png"}}]},
    {"model": "glm-5.3", "input": [
        {"type": "function_call_output", "output": "orphan text"},
        {"type": "function_call_output", "output": ""},
        {"type": "function_call_output", "output": {"a": 1}},
        {"type": "function_call_output", "output": [{"type": "input_image", "image_url": "http://i/y.png"}]},
        {"type": "function_call_output", "output": [{"type": "output_text", "text": "listy"}]}]},
    {"model": "glm-5.3", "tools": [CUSTOM, FN],
     "input": [U("patch"), {"type": "custom_tool_call", "call_id": "p1", "name": "apply_patch", "input": "*** Begin"},
               {"type": "custom_tool_call_output", "call_id": "p1", "output": "done"},
               {"type": "custom_tool_call", "id": "p2", "name": "apply_patch", "input": {"x": [1, 2]}},
               {"type": "custom_tool_call_output", "call_id": "p2", "output": {"ok": True}},
               {"type": "custom_tool_call", "call_id": "p3", "name": "apply_patch"},
               {"type": "custom_tool_call_output", "call_id": "p3", "output": None}]},
    {"model": "glm-5.3", "tools": [NS, FN, CUSTOM], "input": "go"},
    {"model": "glm-5.3", "input": [U("go"), {"type": "additional_tools", "tools": [NS, CUSTOM]}]},
    {"model": "glm-5.3", "tools": [FN], "input": [{"type": "additional_tools", "tools": [CUSTOM]}, U("x")]},
    {"model": "glm-5.3", "input": [
        {"type": "message", "role": "user", "content": [
            {"type": "input_text", "text": "look"}, {"type": "input_image", "image_url": "http://i/z.png"},
            {"type": "input_image", "image_url": {"url": "http://i/w.png"}},
            {"type": "image", "data": "QUJD", "mimeType": "image/jpeg"}, {"type": "image", "data": "QUJD"},
            {"type": "other"}, "bare string"]}]},
    {"model": "glm-5.3", "input": [{"role": "user", "content": "plain content string"},
                                    {"type": "user", "content": [{"type": "text", "text": "t1"}, {"type": "output_text", "text": ""}, {"type": "summary_text", "text": "t2"}]},
                                    {"role": "user", "content": []}, {"role": "user", "content": None}]},
    {"model": "glm-5.3", "input": [
        {"type": "reasoning", "summary": [{"text": "r1"}, {"text": ""}, {"text": "r2"}, "junk"]},
        A_("answer"),
        {"type": "reasoning", "content": "thought"},
        {"type": "function_call", "call_id": "q", "name": "n", "arguments": "{}"},
        {"type": "reasoning", "summary": "str summary"},
        {"type": "reasoning", "content": [{"type": "output_text", "text": "listed"}]},
        {"type": "reasoning"}]},
    {"model": "glm-5.3", "input": [
        {"type": "agent_message", "content": [{"text": "from agent"}, {"encrypted_content": "enc"}, "plain", {"x": 1}]},
        {"type": "agent_message", "content": "string form"}, {"type": "agent_message", "content": "   "}]},
    {"model": "glm-5.3", "input": "x", "text": {"format": {"type": "json_schema", "name": "n", "schema": {"type": "object"}, "strict": True, "extra": 1}}},
    {"model": "glm-5.3", "input": "x", "text": {"format": {"type": "json_object"}}},
    {"model": "glm-5.3", "input": "x", "text": {"format": {"type": "text"}}},
    {"model": "glm-5.3", "input": "x", "tool_choice": {"type": "function", "name": "get_weather"}, "tools": [FN]},
    {"model": None, "input": "x"},
    {"input": "x"},
    {"model": "some-unknown-model", "input": "x", "max_output_tokens": None, "temperature": None},
    {"model": "glm-5.3", "tools": [NS], "input": "x", "tool_choice": "auto", "parallel_tool_calls": None},
]
out = {"to_chat": []}
for preserve in (False, True):
    for c in req_cases:
        reset_ids()
        out["to_chat"].append(to_chat(c, preserve))


# ---- non-streaming
def nonstream(chat_obj, model="glm-5.3", custom=(), tools=(), ns=None, tool_choice="auto", parallel=True):
    reset_ids()
    meta = {"tools": list(tools), "tool_choice": tool_choice, "parallel_tool_calls": parallel}
    resp = P.chat_to_response(copy.deepcopy(chat_obj), model, set(custom), meta, ns)
    return {"chat": chat_obj, "model": model, "custom": sorted(custom), "tools": list(tools), "ns": ns or {},
            "has_ns": bool(ns), "tool_choice": tool_choice, "parallel": parallel, "want": dump(resp)}


def chat(content=None, tool_calls=None, reasoning=None, finish="stop", usage=None):
    msg = {"role": "assistant", "content": content}
    if tool_calls is not None:
        msg["tool_calls"] = tool_calls
    if reasoning is not None:
        msg["reasoning_content"] = reasoning
    obj = {"id": "chatcmpl-1", "choices": [{"index": 0, "message": msg, "finish_reason": finish}]}
    if usage is not None:
        obj["usage"] = usage
    return obj


def call(i, name, args):
    return {"id": i, "type": "function", "function": {"name": name, "arguments": args}}


DSML = ('before <｜DSML｜calls><｜DSML｜invoke name="get_weather">'
        '<｜DSML｜parameter name="city" string="true">Paris</｜DSML｜parameter>'
        '</｜DSML｜invoke></｜DSML｜calls> after')
DSML_ASCII = ('x <|DSML|calls><|DSML|invoke name="a"><|DSML|parameter name="p">v1</|DSML|parameter>'
              '<|DSML|parameter name="q">v2</|DSML|parameter></|DSML|invoke>'
              '<|DSML|invoke name="b"></|DSML|invoke></|DSML|calls>')
USAGE = {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15,
         "prompt_tokens_details": {"cached_tokens": 4}, "completion_tokens_details": {"reasoning_tokens": 2}}
ns_map = {"list_threads": "codex_app", "js": "codex_app"}
out["nonstream"] = [
    nonstream(chat("hello", usage=USAGE)),
    nonstream(chat("hello", finish="length", usage=USAGE)),
    nonstream(chat("", usage={})),
    nonstream(chat(None)),
    nonstream(chat("t", reasoning="because", usage={"prompt_tokens": 1, "prompt_cache_hit_tokens": 9})),
    nonstream(chat("", [call("c1", "get_weather", "{\"city\":\"X\"}"), call(None, "other", None)]),
              tools=[{"type": "function", "name": "get_weather"}], tool_choice="required", parallel=False),
    nonstream(chat("", [call("c1", "apply_patch", "{\"input\":\"*** patch\"}"), call("c2", "apply_patch", "plain"),
                        call("c3", "apply_patch", "{\"input\":{\"a\":1}}"), call("c4", "apply_patch", "{}"), call("c5", "apply_patch", "")]),
              custom=["apply_patch"]),
    nonstream(chat("", [call("c1", "list_threads", "{}"), call("c2", "codex_app__js", "{}"), call("c3", "codex_app::x", "{}"), call("c4", "plain", "{}")]),
              ns=ns_map),
    nonstream(chat(DSML)),
    nonstream(chat(DSML_ASCII)),
    nonstream(chat("DSML mentioned but no block")),
    nonstream(chat("text", [call("c1", "f", "{}")])),
    nonstream({"choices": []}),
    nonstream({}),
    nonstream({"choices": [{"message": None, "finish_reason": None}]}),
]

# ---- streaming


def chunk(content=None, reasoning=None, tool_calls=None, finish=None, usage=None, extra_delta=None):
    delta = {}
    if content is not None:
        delta["content"] = content
    if reasoning is not None:
        delta["reasoning_content"] = reasoning
    if tool_calls is not None:
        delta["tool_calls"] = tool_calls
    if extra_delta:
        delta.update(extra_delta)
    obj = {"id": "c", "object": "chat.completion.chunk", "choices": [{"index": 0, "delta": delta, "finish_reason": finish}]}
    if usage is not None:
        obj["usage"] = usage
    return obj


def tcd(index, id_=None, name=None, args=None):
    fn = {}
    if name is not None:
        fn["name"] = name
    if args is not None:
        fn["arguments"] = args
    d = {"index": index, "function": fn}
    if id_ is not None:
        d["id"] = id_
    return d


def stream_case(chunks, done=True, custom=(), ns=None, tools=(), tool_choice="auto", parallel=True, model="glm-5.3", raw=None):
    reset_ids()
    lines = []
    for c in chunks:
        lines.append(("data: " + json.dumps(c, ensure_ascii=False) + "\n").encode("utf-8"))
        lines.append(b"\n")
    for r in raw or []:
        lines.append(r.encode("utf-8"))
    if done:
        lines.append(b"data: [DONE]\n")
    holder = {"usage": None, "custom_names": set(custom), "namespace_map": ns,
              "request_meta": {"tools": list(tools), "tool_choice": tool_choice, "parallel_tool_calls": parallel},
              "base_body": None}
    frames = [P.clean_responses_frame(f).decode("utf-8") for f in P.stream_responses_events(iter(lines), model, holder)]
    return {"chunks": chunks, "raw": raw or [], "done": done, "custom": sorted(custom), "ns": ns or {}, "has_ns": bool(ns),
            "tools": list(tools), "tool_choice": tool_choice, "parallel": parallel, "model": model,
            "want": "".join(frames), "usage": holder.get("usage")}


stream = []
stream.append(stream_case([chunk("Hel"), chunk("lo"), chunk(None, finish="stop"), chunk(None, usage=USAGE)]))
stream.append(stream_case([chunk(None, reasoning="think "), chunk(None, reasoning="more"), chunk("answer"), chunk(None, finish="stop", usage=USAGE)]))
stream.append(stream_case([chunk(None, reasoning="only thoughts"), chunk(None, finish="stop")]))
stream.append(stream_case([]))
stream.append(stream_case([], done=False))
stream.append(stream_case([chunk("partial")], done=False))
stream.append(stream_case([chunk("cut off", finish="length", usage=USAGE)]))
stream.append(stream_case([chunk("no usage at all 中文测试")]))
stream.append(stream_case([chunk(None, tool_calls=[tcd(0, "call_a", "get_weather", "")]),
                           chunk(None, tool_calls=[tcd(0, None, None, "{\"city\":")]),
                           chunk(None, tool_calls=[tcd(0, None, None, "\"Paris\"}")]),
                           chunk(None, finish="tool_calls", usage=USAGE)],
                          tools=[{"type": "function", "name": "get_weather"}]))
stream.append(stream_case([chunk("let me check. "), chunk(None, tool_calls=[tcd(0, "call_a", "f", "{}")]),
                           chunk(None, tool_calls=[tcd(1, "call_b", "g", "{\"x\":1}")]), chunk(None, finish="tool_calls")]))
stream.append(stream_case([chunk(None, tool_calls=[tcd(0, "call_a", "get_weather", "{\"city\":\"Pa")]), chunk(None, finish="length")]))
stream.append(stream_case([chunk(None, tool_calls=[tcd(0, "call_a", "f", "{\"ok\":true}")]),
                           chunk(None, tool_calls=[tcd(1, "call_b", "g", "{\"half\":")])], done=False))
stream.append(stream_case([chunk(None, tool_calls=[tcd(0, "call_p", "apply_patch", "")]),
                           chunk(None, tool_calls=[tcd(0, None, None, "{\"input\":\"*** Beg")]),
                           chunk(None, tool_calls=[tcd(0, None, None, "in\"}")]), chunk(None, finish="tool_calls")],
                          custom=["apply_patch"]))
stream.append(stream_case([chunk(None, tool_calls=[tcd(0, None, None, "{\"late\":")]),
                           chunk(None, tool_calls=[tcd(0, None, "f_late", "1}")]), chunk(None, finish="tool_calls")]))
stream.append(stream_case([chunk(None, tool_calls=[tcd(0, "c1", "list_threads", "{}")]),
                           chunk(None, tool_calls=[tcd(1, "c2", "codex_app__js", "{}")]),
                           chunk(None, tool_calls=[tcd(2, "c3", "plain", "{}")]), chunk(None, finish="tool_calls")],
                          ns=ns_map))
stream.append(stream_case([chunk("pre "), chunk(DSML[len("before "):]), chunk(" post")]))
stream.append(stream_case([chunk(DSML)]))
stream.append(stream_case([chunk(DSML_ASCII[:20]), chunk(DSML_ASCII[20:45]), chunk(DSML_ASCII[45:])], ns=ns_map))
stream.append(stream_case([chunk("a < b and c > d "), chunk("<b>bold</b> <"), chunk("tag "), chunk("<|DSML| half")]))
stream.append(stream_case([chunk("<"), chunk("|DSML|calls><|DSML|invoke name=\"z\"></|DSML|invoke></|DSML|calls>tail")]))
stream.append(stream_case([chunk("1 < 2"), chunk(" < 3 <")]))
stream.append(stream_case([chunk("unterminated <|DSML|calls><|DSML|invoke name=\"z\">")]))
stream.append(stream_case([chunk("keepalive"), ], raw=[": ping\n", "data: not json\n", "data: {\"choices\":[]}\n", "data:data: {\"id\":\"x\",\"choices\":[{\"delta\":{\"content\":\"!\"}}]}\n"]))
stream.append(stream_case([chunk(None, usage={"prompt_tokens": 3, "total_tokens": 9}), chunk("x", usage={"prompt_tokens": 1, "total_tokens": 4})]))
stream.append(stream_case([{"choices": [{"delta": {"function_call": {}, "tool_calls": []}, "finish_reason": None}]}, chunk("k")]))
stream.append(stream_case([chunk("x")], tools=[{"type": "function", "name": "t", "function_call": {}}, {"tool_calls": []}], tool_choice={"type": "function"}, parallel=False))

# fuzz: random pieces that stress the DSML hold-back logic
rnd = random.Random(20261019)
alphabet = ["<", ">", "DSML", "|", " ", "a", "b", "中", "\n", "calls", "invoke name=\"f\"", "/", "parameter name=\"p\"", "<|DSML|calls>",
            "</|DSML|calls>", "<|DSML|invoke name=\"f\">", "</|DSML|invoke>", "x" * 12, "☀"]
for n in range(160):
    pieces = ["".join(rnd.choice(alphabet) for _ in range(rnd.randint(1, 6))) for _ in range(rnd.randint(1, 7))]
    stream.append(stream_case([chunk(p) for p in pieces]))
out["stream"] = stream

dest = os.path.join(ROOT, "internal", "gateway", "responses", "testdata", "responses.json")
os.makedirs(os.path.dirname(dest), exist_ok=True)
with open(dest, "w", encoding="utf-8", newline="\n") as fh:
    json.dump(out, fh, ensure_ascii=False)
print("wrote", dest, os.path.getsize(dest), "bytes;", {k: len(v) for k, v in out.items()})
