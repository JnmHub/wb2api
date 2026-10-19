import ast, json, re, time, sys
SRC = open(r"C:\wb2a\src\wb_proxy.py", encoding="utf-8").read()
tree = ast.parse(SRC)
want_funcs = {"strip_data_prefix","clean_chunk","could_be_leaked_call","_normalize_dsml_bars",
              "chat_chunk","chat_text_chunk","normalize_usage_cache_aliases","_strip_empty_fc"}
want_cls = {"LeakedToolCallStreamFilter"}
want_assign = {"LEAK_TAG_RE","LEAK_TAG_PREFIXES","NOISE_KEYS","LEAK_NAME_ATTR_RE","LEAK_WRAPPER_TAGS","LEAK_PARAM_TAGS"}
ns = {"json":json,"re":re,"time":time}
body=[]
for n in tree.body:
    nm = getattr(n,"name",None)
    if isinstance(n,(ast.FunctionDef,ast.ClassDef)) and nm in (want_funcs|want_cls): body.append(n)
    elif isinstance(n,ast.Assign) and any(isinstance(t,ast.Name) and t.id in want_assign for t in n.targets): body.append(n)
mod = ast.Module(body=body, type_ignores=[])
try:
    exec(compile(mod,"x","exec"), ns)
except NameError as e:
    print("missing", e); sys.exit(1)
strip_data_prefix=ns["strip_data_prefix"]; clean_chunk=ns["clean_chunk"]
Filter=ns["LeakedToolCallStreamFilter"]; chat_text_chunk=ns["chat_text_chunk"]

def mk(content):
    return b'data: ' + json.dumps({"id":"x","object":"chat.completion.chunk","created":1,"model":"m",
        "choices":[{"index":0,"delta":{"content":content,"reasoning_content":"","tool_calls":[]},"finish_reason":None}]},ensure_ascii=False).encode()+b"\n"

def handle(line, flt, out):
    data = strip_data_prefix(line.decode("utf-8","replace"))
    if not data or data=="[DONE]": return
    maybe = json.loads(data)
    for ch in maybe.get("choices") or []:
        d = ch.get("delta") or {}
        p = d.get("content")
        if p:
            d.pop("content",None)
            for a in flt.feed(p):
                if a[0]=="text" and a[1]: out.append(chat_text_chunk("m",a[1]))
    cleaned = clean_chunk(json.dumps(maybe,ensure_ascii=False))
    if cleaned: out.append(cleaned)

# A: plain prose, per-chunk CPU
N=20000
lines=[mk("hello wor") for _ in range(N)]
flt=Filter(); out=[]
t=time.perf_counter()
for l in lines: handle(l,flt,out)
dt=time.perf_counter()-t
print("A prose: %.1f us/chunk -> %.0f chunks/s per core, emitted %d/%d"%(dt/N*1e6,N/dt,len(out),N))

# B: stream where a leak-looking tag appears early (e.g. '<args>' / '<thinking>')
for tag in ["<args>","<thinking>","List<String>"]:
    N=3000
    flt=Filter(); out=[]; emitted_before_end=0
    lines=[mk("text ") for _ in range(50)]+[mk(tag)]+[mk("more text here ") for _ in range(N)]
    t=time.perf_counter()
    for l in lines: handle(l,flt,out)
    held=len(flt.buf); emitted=len(out)
    dt=time.perf_counter()-t
    print("B tag=%-12s: total %.3fs for %d chunks (%.0f us/chunk avg); emitted-before-end=%d/%d; held bytes=%d"%(tag,dt,len(lines),dt/len(lines)*1e6,emitted,len(lines),held))

print("--- growth with held buffer (tag=<thinking>) ---")
for N in (1000,4000,16000):
    flt=Filter(); out=[]
    lines=[mk("<thinking>")]+[mk("more text here ") for _ in range(N)]
    t=time.perf_counter()
    for l in lines: handle(l,flt,out)
    dt=time.perf_counter()-t
    print("N=%5d chunks: %.3fs total, %.0f us/chunk, emitted %d, held %d bytes"%(N,dt,dt/N*1e6,len(out),len(flt.buf)))
