import sys, time, json, os, random, tempfile, threading
sys.dont_write_bytecode = True
sys.path.insert(0, r"C:\wb2a\src")
os.environ["WB_USAGE_CACHE"]="0"
import wb_proxy as P
random.seed(2)
N=int(sys.argv[1]) if len(sys.argv)>1 else 200000
d=tempfile.mkdtemp(); path=os.path.join(d,"usage.jsonl")
models=["glm-5.3","deepseek-v4.1-flash","gpt-5.5","kimi-k2.6","hy3"]
now=time.time()
with open(path,"w",encoding="utf-8") as f:
    for i in range(N):
        at=now-random.random()*86400*14
        pt=random.randint(2000,60000); ct=random.randint(20,2000); gen=random.randint(500,20000)
        row={"account":"acct-%04d"%random.randint(0,299),"at":at,"cache_hit_pct":round(random.random()*90,1),"cached_tokens":int(pt*0.5),
          "completion_tokens":ct,"credit":random.random()*3,"elapsed_ms":gen+1200,"gen_ms":gen,"iso":time.strftime("%Y-%m-%dT%H:%M:%S",time.localtime(at)),
          "model":random.choice(models),"msgs_sha":"a"*12,"n_msgs":20,"outcome":"completed","prefix_sha":"b"*12,"prompt_tokens":pt,"realm":"intl",
          "reasoning_tokens":0,"stream":True,"system_sha":"c"*12,"tokens_per_sec":round(ct/(gen/1000),2),"total_tokens":pt+ct,"ttft_ms":random.randint(300,3000)}
        f.write(json.dumps(row)+"\n")
print("synthetic log: %d rows, %.0f MB"%(N,os.path.getsize(path)/1e6))
P.USAGE_DIR=d; P.USAGE_LOG=path
def t(label, fn):
    s=time.perf_counter(); fn(); print("  %-36s %8.0f ms"%(label,(time.perf_counter()-s)*1000))
t("usage_snapshot cold (all)", lambda: P.usage_snapshot(ttl=0, range="all"))
t("usage_snapshot again ttl=0", lambda: P.usage_snapshot(ttl=0, range="all"))
t("compute_usage_analytics cold", lambda: P.compute_usage_analytics(ttl=0, range="all"))
t("usage_by_account cold", lambda: P.usage_by_account(ttl=0))
t("perf_stats", lambda: P.perf_stats(ttl=0, range="all"))
t("usage_timeseries", lambda: P.usage_timeseries(range="week"))
t("recent_usage(20)", lambda: P.recent_usage(limit=20))
# interference: a pure-Python 'stream' thread doing small ops, measure its max stall while stats run
stalls=[]; stop=False
def ticker():
    last=time.perf_counter()
    while not stop:
        time.sleep(0.001)
        now=time.perf_counter(); stalls.append(now-last-0.001); last=now
th=threading.Thread(target=ticker); th.start()
time.sleep(0.2); stalls.clear()
P.usage_snapshot(ttl=0, range="all"); P.compute_usage_analytics(ttl=0, range="all"); P.usage_by_account(ttl=0)
stop=True; th.join()
stalls.sort()
print("  while stats rebuild ran: max stall of a concurrent 1ms ticker = %.0f ms, p99 = %.0f ms"%(stalls[-1]*1000, stalls[int(len(stalls)*.99)]*1000))
