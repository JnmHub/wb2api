import sys, time, json, tempfile, os
sys.dont_write_bytecode = True
sys.path.insert(0, r"C:\wb2a\src")
import wb_accounts as A
def mk(n, weighted=0, with_credits=True):
    pool = A.AccountPool(tempfile.mkdtemp())
    now = time.time()
    for i in range(n):
        d = {"uid": "u%06d" % i, "accessToken": "x.y.z", "refreshToken": "r", "realm": "intl",
             "expiresAt": now + 86400*3, "enabled": True, "weight": 5 if i < weighted else 1}
        a = A.Account(d, None)
        if with_credits:
            a.credits = {"remain": 5000, "total": 10000, "packages": [
                {"expireAt": now + 86400*(5+i%20), "remain": 100} for _ in range(3)]}
        pool.accounts.append(a)
    return pool
def bench(label, fn, reps):
    t = time.perf_counter()
    for _ in range(reps): fn()
    dt = (time.perf_counter() - t) / reps
    print("  %-34s %9.1f us" % (label, dt * 1e6))
for n in (100, 1000, 5000):
    print("N=%d accounts" % n)
    pool = mk(n)
    reps = max(20, 20000 // n)
    bench("count_ready (per request)", lambda: pool.count_ready("intl", model="glm-5.3"), reps)
    bench("pick_for_session (no affinity)", lambda: pool.pick_for_session("intl", None, set(), "glm-5.3"), reps)
    pool2 = mk(n, weighted=max(1, n//10))
    bench("pick (10%% accounts weight>1)", lambda: pool2.pick_for_session("intl", None, set(), "glm-5.3"), reps)
    pool3 = mk(n)
    for a in pool3.accounts: a.throttle = None
    # worst case: first 90% cooling down -> rotation scans past them
    for a in pool3.accounts[: int(n*0.9)]: a.cooldown_until = time.time() + 600
    bench("pick (90%% cooling)", lambda: pool3.pick_for_session("intl", None, set(), "glm-5.3"), reps)
