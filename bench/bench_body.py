import sys, time, json, gzip, os
sys.dont_write_bytecode = True
sys.path.insert(0, r"C:\wb2a\src")
os.environ.setdefault("WB_USAGE_CACHE","0")
import wb_proxy as P
import random
random.seed(1)
def mkpayload(n_msgs, avg_kb):
    msgs=[{"role":"system","content":"You are a coding agent. "*200}]
    for i in range(n_msgs):
        if i%3==0: msgs.append({"role":"user","content":"please do step %d "%i + "x"*200})
        elif i%3==1: msgs.append({"role":"assistant","content":"ok","tool_calls":[{"id":"c%d"%i,"type":"function","function":{"name":"read_file","arguments":json.dumps({"path":"/a/b/%d.py"%i})}}]})
        else: msgs.append({"role":"tool","tool_call_id":"c%d"%(i-1),"content":("line of code %d\n"%i)*(avg_kb*1024//20)})
    tools=[{"type":"function","function":{"name":"read_file","description":"d"*300,"parameters":{"type":"object","properties":{"path":{"type":"string"}}}}} for _ in range(30)]
    return {"model":"glm-5.3","stream":True,"messages":msgs,"tools":tools}
def t(label, fn, reps=5):
    fn(); s=time.perf_counter()
    for _ in range(reps): r=fn()
    dt=(time.perf_counter()-s)/reps*1000
    print("   %-34s %8.2f ms"%(label,dt)); return r
for n,kb in ((60,4),(300,8),(600,16)):
    p=mkpayload(n,kb)
    raw=json.dumps(p).encode()
    print("conversation: %d msgs, body %.2f MB"%(len(p["messages"]),len(raw)/1e6))
    t("json.loads(body)", lambda: json.loads(raw))
    body=t("build_upstream_body", lambda: P.build_upstream_body(json.loads(raw)))
    t("prompt_fingerprint (3x dumps+sha)", lambda: P.prompt_fingerprint(body["messages"]))
    t("affinity_route", lambda: P.affinity_route(body["messages"]))
    data=t("encode_upstream_body", lambda: P.encode_upstream_body(body))
    t("gzip level default", lambda: gzip.compress(data))
    print("   (sum on hot path before upstream is contacted)")
