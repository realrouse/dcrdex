#!/usr/bin/env python3
"""SIMNET/REGTEST ONLY. Restart a finished p6-campaign run's peers (same
appdata) with a new bisonw build, wait until every swap record is final, and
redo the campaign checks against the run's start balances.
Usage: RUN=/workspace/tatanka-dev/run/p6 BISONW=... ./p6-recover.py"""
import ast, importlib.util, json, os, re, sys, threading, time
from decimal import Decimal

HERE = os.path.dirname(os.path.abspath(__file__))
spec = importlib.util.spec_from_file_location("camp", os.path.join(HERE, "p6-campaign.py"))
C = importlib.util.module_from_spec(spec); spec.loader.exec_module(C)
C.RUN = os.environ.get("RUN", "/workspace/tatanka-dev/run/p6")
WAIT_MIN = float(os.environ.get("WAIT_MIN", 25))


def main():
    C.LOGF = open(os.path.join(C.RUN, "recover.log"), "a")
    m = re.search(r"start balances (\{.*\})", open(os.path.join(C.RUN, "campaign.log")).read())
    b0 = {k: Decimal(v) for k, v in ast.literal_eval(m.group(1)).items()}
    P = {n: C.Peer(n, c) for n, c in C.PEERS.items()}
    C.PEER_OBJS.update(P)
    for p in P.values():
        p.pw = open(os.path.join(p.dir, ".testpw")).read()
        n0 = p.streams()
        p.start(); p.login(); p.connect(n0)
    C.log("recovery: peers restarted with", C.BISONW)
    mt = threading.Thread(target=C.miner, daemon=True); mt.start()
    end = time.time() + WAIT_MIN * 60
    while time.time() < end:
        n = C.all_final(P)
        C.log("recovery: %d swap records not final" % n)
        if n == 0:
            break
        time.sleep(30)
    C.STOP.set(); mt.join(timeout=30)
    for _ in range(5):
        time.sleep(6); C.mine(2); time.sleep(6)
        res = C.verify(P, b0, C.balances())
        if all(a["ok"] for a in res["accounting"]):
            break
    ok = not res["nonfinal"] and not res["loss"] and all(a["ok"] for a in res["accounting"])
    res["pass"] = ok
    for n, p in P.items():
        with open(os.path.join(C.RUN, "swaps-%s-recovered.json" % n), "w") as f:
            json.dump(p.swaps(), f, indent=1)
    with open(os.path.join(C.RUN, "recover-result.json"), "w") as f:
        json.dump(res, f, indent=1, default=str)
    C.log("RECOVERY RESULT", json.dumps({k: res[k] for k in ("swaps", "statuses", "loss", "nonfinal")}, default=str)[:3000])
    for a in res["accounting"]:
        C.log("  %-8s expected %+.8f actual %+.8f fees %.8f txs %d %s" % (a["wallet"], Decimal(a["expected"]), Decimal(a["actual"]), Decimal(a["fees"]), a["txs"], "ok" if a["ok"] else "BAD"))
    C.log("PASS" if ok else "FAIL")
    return 0 if ok else 1


if __name__ == "__main__":
    try:
        sys.exit(main())
    finally:
        C.STOP.set()
        for p in C.PEER_OBJS.values():
            p.kill()
