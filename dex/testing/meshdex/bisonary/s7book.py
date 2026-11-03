#!/usr/bin/env python3
"""SIMNET/REGTEST ONLY. S7 book cross-check end-to-end test on the box.

Same network and release as s7.py (4 seeded validators, 6 traders with
realistic balances, CoinGecko-priced books with 10+ orders per side, the
emulated market maker, trading bonds required). Every validator publishes a
signed book digest every 3 epochs; every peer compares it with its own book.

Phases (default all, in order):
  wallets switch bonds book   as s7.py
  xbaseline  honest network: every peer compares every validator's digests,
             no suspects, no wrong digests
  xhide      v2 hides dave's orders (left out of its proposals and digests):
             every other peer, validators and clients, flags v2; dave's orders
             still rest on every book; no honest validator is suspected
  xwithhold  finalization stalled (v3, v4 stopped), so orders wait in pools.
             a) v1 never receives dave's orders (blocked on arrival): peers see
             them missing from v1's digest and re-send them; v1 gets them and
             is never a suspect. b) v2 withholds dave's new pending orders:
             reported as withheld (still missing after the re-send) by every
             running peer. v3 and v4 restart; the orders finalize
  xclear     v2 stops hiding: no suspects anywhere within a few digests
  xagree     identical chains on every peer
Usage: ./s7book.py [phase ...]
"""
import os, sys, time, json
HERE = os.path.dirname(os.path.abspath(__file__))
os.environ.setdefault("EVID", "/workspace/tatanka-dev/evidence/s7-price-history/book-crosscheck")
os.environ.setdefault("BISONW", "/workspace/tatanka-dev/bin/bisonw-bisonary-s8")
os.environ.setdefault("RUN", "/workspace/tatanka-dev/run/s012")
sys.path.insert(0, HERE)
import harness as h
from harness import log, Fail
import s012 as b
import s3
import s6
import s7

EVID = b.EVID
TL = []

def bookcheck(n):
    return n.dbgapi("/mesh/epochs/bookcheck")

def snapshot(net, tag, nodes=None):
    row = {"t": time.strftime("%T"), "tag": tag}
    for n in nodes or s6.peers(net):
        try:
            bc = bookcheck(n)
        except Exception as e:
            row[n.name] = "error %s" % e
            continue
        row[n.name] = {"suspects": bc["suspects"], "sent": bc["digestsSent"],
                       "peers": {p["name"]: {k: p[k] for k in ("digests", "compared", "hiddenNow", "extraNow", "withheldNow", "missingNow",
                                                               "badDigests", "badStreak", "repairs", "suspect")} for p in bc["peers"] or []}}
    TL.append(row)
    json.dump(TL, open(os.path.join(EVID, "book-crosscheck-timeline.json"), "w"), indent=1)
    return row

def book_flags(n, kinds=("book-hidden", "book-withheld", "book-mismatch")):
    return [f for f in n.epochs()["flags"] or [] if f["kind"] in kinds]

def phase_xbaseline(net):
    time.sleep(60)
    row = snapshot(net, "baseline")
    vals = {v.name for v in net.vals}
    bad = {}
    for k, v in row.items():
        if not isinstance(v, dict):
            continue
        want = vals - {k}
        got = {p for p, x in v["peers"].items() if x["compared"] > 0}
        wrong = {p: x for p, x in v["peers"].items() if x["badDigests"] or x["suspect"]}
        if v["suspects"] or wrong or not want <= got:
            bad[k] = {"suspects": v["suspects"], "wrong": wrong, "compared": sorted(got)}
    res = {"peers": len([k for k in row if isinstance(row[k], dict)]),
           "digestsComparedPerPeer": {k: {p: x["compared"] for p, x in v["peers"].items()} for k, v in row.items() if isinstance(v, dict)},
           "suspectsAnywhere": {k: v["suspects"] for k, v in row.items() if isinstance(v, dict) and v["suspects"]},
           "bookFlags": {n.name: len(book_flags(n)) for n in s6.peers(net)}, "bad": bad}
    b.result("xbaseline", res)
    if bad or any(res["bookFlags"].values()):
        raise Fail("baseline: %s" % res)

def far_orders(n, k, base=0):
    """k resting lbc_dcr bids 20-30 % below mid (never matched)."""
    mid = h.rate_for(net_prices(), "lbc_dcr")
    return [n.order("lbc_dcr", False, 1, int(mid * 0.75) - base - i, 600) for i in range(k)]

_NET = {}
def net_prices():
    return _NET["net"].prices

def wait_suspect(net, bad, nodes, timeout, field=None, need=1):
    t0 = time.time()
    seen = {}
    def every():
        for n in nodes:
            if n.name in seen:
                continue
            bc = bookcheck(n)
            p = next((p for p in bc["peers"] or [] if p["name"] == bad.name), None)
            if bad.name in bc["suspects"] and p and (field is None or p[field] >= need):
                seen[n.name] = {"seconds": round(time.time() - t0, 1), "suspects": bc["suspects"],
                                "report": {k: p.get(k) for k in ("hiddenNow", "withheldNow", "extraNow", "mismatchNow", "badStreak",
                                                             "badDigests", "suspectSince", "examples", "omittedBy")}}
        return len(seen) == len(nodes)
    try:
        h.wait_until(every, timeout, "%s suspected by every peer" % bad.name, 2)
    except Fail:
        pass
    return seen

def phase_xhide(net):
    _NET["net"] = net
    bad, dv = net["v2"], net["dave"]
    others = [n for n in s6.peers(net) if n is not bad]
    f0, c0 = s3.finalized(net["v1"]), s3.st(net["v1"])["currentEpoch"]
    bad.dbgapi("/mesh/epochs/hide", {"owner": dv.peer_id})
    oids = far_orders(dv, 6)
    seen = wait_suspect(net, bad, others, 120, "hiddenNow", 6)
    row = snapshot(net, "hide: detected")
    resting = {}
    for n in [net["v1"], net["v3"], net["v4"], net["alice"], net["carol"], bad]:
        ids = {o["id"] for o in n.book("lbc_dcr")}
        resting[n.name] = sum(1 for o in oids if o in ids)
    honest_suspected = {k: v["suspects"] for k, v in row.items() if isinstance(v, dict) and set(v["suspects"]) - {bad.name}}
    flags = {n.name: [f["kind"] for f in book_flags(n) if f["peer"] == bad.peer_id] for n in others}
    res = {"hider": bad.name, "owner": "dave", "orders": len(oids), "detectedBy": seen, "peersChecking": [n.name for n in others],
           "notDetectedBy": [n.name for n in others if n.name not in seen], "daveOrdersRestingOnBooks": resting,
           "honestSuspected": honest_suspected, "bookFlags": flags,
           "epochsFinalizedMeanwhile": s3.finalized(net["v1"]) - f0, "epochsMeanwhile": s3.st(net["v1"])["currentEpoch"] - c0}
    b.result("xhide", res)
    net.hidden_oids = oids
    if res["notDetectedBy"] or honest_suspected or any(v != len(oids) for v in resting.values()) or res["epochsFinalizedMeanwhile"] < 0.7 * res["epochsMeanwhile"]:
        raise Fail("xhide: %s" % res)

def phase_xwithhold(net):
    _NET["net"] = net
    bad, v1, dv = net["v2"], net["v1"], net["dave"]
    bad.dbgapi("/mesh/epochs/hide", {"owner": ""})
    for v in (net["v3"], net["v4"]):
        v.stop()
    h.wait_until(lambda: s3.st(v1)["unfinalizedSinceLast"] >= 3, 60, "finalization stalled", 1)
    time.sleep(8)
    running = [n for n in s6.peers(net)]
    res = {"stopped": ["v3", "v4"], "running": [n.name for n in running]}
    # a) v1 loses dave's orders (dropped on arrival): repaired, not a suspect.
    v1.dbgapi("/mesh/faults", {"block": [dv.peer_id]})
    lost = far_orders(dv, 3, 100)
    def on_v1():
        return all(v1.dbgapi("/mesh/epochs/order?id=" + o)["status"] == "pool" for o in lost)
    t0 = time.time()
    try:
        h.wait_until(on_v1, 60, "lost orders repaired on v1", 2)
        res["lostRepairedOnV1Seconds"] = round(time.time() - t0, 1)
    except Fail:
        res["lostRepairedOnV1Seconds"] = None
    res["v1Faults"] = v1.dbgapi("/mesh/faults")
    time.sleep(15)
    row = snapshot(net, "withhold: lost on v1", running)
    res["v1AfterRepair"] = {k: v["peers"].get("v1") for k, v in row.items() if isinstance(v, dict) and k != "v1"}
    res["v1SuspectedBy"] = [k for k, v in row.items() if isinstance(v, dict) and "v1" in v["suspects"]]
    v1.dbgapi("/mesh/faults", {})
    # b) v2 withholds dave's new pending orders.
    bad.dbgapi("/mesh/epochs/hide", {"owner": dv.peer_id})
    held = far_orders(dv, 3, 200)
    others = [n for n in running if n is not bad]
    seen = wait_suspect(net, bad, others, 90, "withheldNow", 3)
    row = snapshot(net, "withhold: v2 withholding", running)
    res["withheldDetectedBy"] = seen
    res["withheldNotDetectedBy"] = [n.name for n in others if n.name not in seen]
    res["honestSuspected"] = {k: v["suspects"] for k, v in row.items() if isinstance(v, dict) and set(v["suspects"]) - {bad.name}}
    res["pendingOnV1"] = sum(1 for o in lost + held if v1.dbgapi("/mesh/epochs/order?id=" + o)["status"] == "pool")
    for v in (net["v3"], net["v4"]):
        net.start(v)
    h.wait_until(lambda: b.epoch_ok(v1), 180, "finalization resumed", 2)
    def finalized_all():
        return all(v1.dbgapi("/mesh/epochs/order?id=" + o)["status"] == "resting" for o in lost + held)
    t1 = time.time()
    h.wait_until(finalized_all, 120, "stalled orders resting after restart", 2)
    res["ordersRestingAfterResumeSeconds"] = round(time.time() - t1, 1)
    b.result("xwithhold", res)
    net.hidden_oids = getattr(net, "hidden_oids", []) + lost + held
    if res["lostRepairedOnV1Seconds"] is None or res["v1SuspectedBy"] or res["withheldNotDetectedBy"] or res["honestSuspected"]:
        raise Fail("xwithhold: %s" % res)

def phase_xclear(net):
    bad = net["v2"]
    bad.dbgapi("/mesh/epochs/hide", {"owner": ""})
    t0 = time.time()
    def clear():
        return all(not bookcheck(n)["suspects"] for n in s6.peers(net))
    try:
        h.wait_until(clear, 90, "no suspects anywhere", 2)
        secs = round(time.time() - t0, 1)
    except Fail:
        secs = None
    row = snapshot(net, "clear")
    res = {"secondsToClear": secs, "suspects": {k: v["suspects"] for k, v in row.items() if isinstance(v, dict)}}
    b.result("xclear", res)
    if secs is None:
        raise Fail("xclear: %s" % res)

def phase_xagree(net):
    nodes = s6.peers(net)
    frm = s3.finalized(net["v1"]) - 300
    rep = b.compare_chains(nodes, frm)
    for o in getattr(net, "hidden_oids", []):
        try:
            net["dave"].cancel(o)
        except Exception:
            pass
    b.result("xagree", rep)

s7.PHASES = [("wallets", s6.phase_wallets), ("switch", s7.phase_switch), ("bonds", s6.phase_bonds), ("book", s6.phase_book),
             ("xbaseline", phase_xbaseline), ("xhide", phase_xhide), ("xwithhold", phase_xwithhold), ("xclear", phase_xclear),
             ("xagree", phase_xagree)]

if __name__ == "__main__":
    s7.main()
