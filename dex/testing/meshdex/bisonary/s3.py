#!/usr/bin/env python3
"""SIMNET/REGTEST ONLY. S3 (active validator set) end-to-end test on the box.

Same network as s012.py (private 3-node mesh, validators v1-v4, traders
alice, bob, carol and the market maker mm with realistic balances: 1-5 M
LBC, ~200 DCR, ~0.355 BTC; books seeded at real CoinGecko prices with 10+
orders per side; an emulated market maker on every market). It reuses the
s012 run directory so the funded wallets are only topped up.

The S3 release uses 2 s epochs with the plan's simnet active-set values: a
600-epoch window ending 100 epochs back, 80 %, judged once 60 epochs are in
the window.

Phases (default all, in order):
  switch   restart every node with the S3 release
  book     seed books (10+ orders per side), start market makers
  warm     window filled: all 4 validators active, identical on every peer
  drop     v4 offline: drops out of the active set on every peer
  same     all peers compute the same active set (live + full replay)
  quorum   v4 inactive, v3 down: 2 of the 3 active (2 of 4 bonded) still
           finalize; an epoch match made then swaps to completion
  rejoin   v4 back: signs again, is credited and rejoins the active set
  same2    all peers agree again after the rejoin
  floor    5 validators, short window: cascade of drop-outs until active
           power < 1/2 of bonded -> no finalization; window drains ->
           all validators count again; enough come back -> resumes
Usage: ./s3.py [phase ...]
"""
import os, sys, time, json, shutil, signal
HERE = os.path.dirname(os.path.abspath(__file__))
os.environ.setdefault("EVID", "/workspace/tatanka-dev/evidence/s3-active-set")
os.environ.setdefault("BISONW", "/workspace/tatanka-dev/bin/bisonw-bisonary-s3")
os.environ.setdefault("RUN", "/workspace/tatanka-dev/run/s012")
sys.path.insert(0, HERE)
import harness as h
from harness import log, Fail
import meshmm
import s012 as b

EVID = b.EVID
EPOCH_MS = 2000
S3_FLAGS = ["-epochms", "2000", "-windowms", "1000", "-proposems", "200", "-fallbackms", "300", "-omissionms", "500",
            "-activewindow", "600", "-activelag", "100", "-activepct", "80", "-activemin", "60"]
FLOOR_FLAGS = ["-epochms", "2000", "-windowms", "1000", "-proposems", "200", "-fallbackms", "300", "-omissionms", "500",
               "-activewindow", "30", "-activelag", "5", "-activepct", "80", "-activemin", "10"]
TIMELINE = []

def st(bw):
    return bw.epochs()

def active_names(s):
    return [v["name"] for v in s["validators"] if v["active"]]

def snap(net, tag, nodes=None):
    """One line per node: last finalized, active set, credit % per validator."""
    row = {"t": time.strftime("%T"), "tag": tag}
    for n in nodes or running(net):
        try:
            s = st(n)
        except Exception as e:
            row[n.name] = "error %s" % e
            continue
        row[n.name] = {"last": s.get("lastFinalized"), "active": active_names(s), "fallback": s["activeSet"]["fallback"],
                       "sample": s["activeSet"]["sample"], "canFinalize": s["canFinalize"],
                       "credit": {v["name"]: round(v["creditPct"], 1) for v in s["validators"]}}
    TIMELINE.append(row)
    json.dump(TIMELINE, open(os.path.join(EVID, "timeline.json"), "w"), indent=1)
    a = row.get("alice") if isinstance(row.get("alice"), dict) else None
    if a:
        log("%s: alice last %s active %s sample %s credit %s" % (tag, a["last"], a["active"], a["sample"], a["credit"]))
    return row

def running(net):
    return [n for n in net.nodes.values() if n.proc and n.proc.poll() is None and n.name not in ("tamper",)]

def finalized(bw):
    return st(bw).get("lastFinalized", 0)

def sign(net, name, ids, names, flags, bonds=None):
    extra = list(flags) + (["-bonds", ",".join(map(str, bonds))] if bonds else [])
    path = b.sign_release(os.path.join(b.RUN, name), ids, names, extra)
    shutil.copy(path, EVID)
    return path

def restart_all(net, nodes):
    for n in net.nodes.values():
        n.stop()
    time.sleep(2)
    for n in nodes:
        net.start(n)
    for n in nodes:
        h.wait_until(lambda: b.epoch_ok(n), 180, "%s finalizing" % n.name, 2)

def wait_active(net, who, want, timeout, nodes=None, every=10, tag=""):
    """Wait until validator `who` is (want=True) or is not active on alice."""
    t0 = time.time()
    end = t0 + timeout
    while time.time() < end:
        row = snap(net, tag or ("wait %s %s" % (who, "active" if want else "inactive")), nodes)
        a = row.get("alice") if isinstance(row.get("alice"), dict) else row.get(next(iter(row.keys() - {"t", "tag"})))
        if a and (who in a["active"]) == want:
            return time.time() - t0, a
        time.sleep(every)
    raise Fail("%s did not become %s within %ds" % (who, "active" if want else "inactive", timeout))

def first_record_without(bw, vid, frm):
    for e, r in sorted(b.records(bw, frm).items()):
        if vid not in r["activeSet"]:
            return e
    return None

def same_set(net, nodes, label):
    """Every node computes the same active set for the next epochs from the
    same chain prefix, and replays its whole chain's active sets."""
    lasts = {n.name: finalized(n) for n in nodes}
    asof = min(lasts.values())
    out = {"asOf": asof, "sets": {}, "replay": {}}
    for nn in (asof + 1, asof + 60, asof + 101):
        sets = {}
        for n in nodes:
            r = n.dbgapi("/mesh/epochs/active?epoch=%d&asof=%d" % (nn, asof))
            sets[n.name] = (tuple(r["active"]), r["sample"], json.dumps(r["credits"], sort_keys=True))
        if len(set(sets.values())) != 1:
            raise Fail("%s: active sets for epoch %d differ: %s" % (label, nn, sets))
        names = sorted(net.name_of.get(x, x[-8:]) for x in sets[nodes[0].name][0])
        out["sets"][str(nn)] = {"active": names, "sample": sets[nodes[0].name][1], "nodes": len(nodes)}
    for n in nodes:
        r = n.dbgapi("/mesh/epochs/activecheck?from=0")
        out["replay"][n.name] = r
        if r["mismatches"] or not r["checked"]:
            raise Fail("%s: %s active-set replay: %s" % (label, n.name, r))
    out["chain"] = b.compare_chains(nodes, asof - 300)
    return out

# ---------------- phases ----------------
def phase_switch(net):
    ids = [v.peer_id for v in net.vals]
    net.release = sign(net, "s3-simnet-2s-w600.json", ids, [v.name for v in net.vals], S3_FLAGS)
    log(h.run([b.RELTOOL, "verify", net.release]).splitlines()[0])
    for m in net.mms:
        m.stop()
    net.mms = []
    restart_all(net, net.vals + net.traders)
    s = st(net["alice"])
    b.result("switch", {"release": os.path.basename(net.release), "params": {k: s["params"][k] for k in
                        ("epochMs", "activeWindowEpochs", "activeLagEpochs", "activePct", "activeMinEpochs", "quorumNum", "quorumDen", "floorNum", "floorDen")},
                        "activeSet": s["activeSet"], "active": active_names(s)})

def phase_book(net):
    prices = h.fetch_prices()
    net.prices = prices
    seeders = [net["alice"], net["bob"], net["carol"]]
    bal = {t.name: {a: t.coins(a) for a in ("lbc", "dcr", "btc")} for t in seeders}
    def budget(t, mkt, sell):
        bb, q = h.market_assets(mkt)
        lot = h.LOTS[mkt] / 1e8
        if sell:
            return int(bal[t.name].get(bb, 0) * 0.12 / lot)
        have = bal[t.name].get(q, 5000 if q.startswith("usdc") else 0)
        rate = h.rate_for(prices, mkt) / 10 ** h.DECIMALS[q]
        return int(have * 0.12 / (lot * rate)) if rate else 0
    for t in net.traders:
        for m in b.MARKETS:
            t.subscribe(m)
    books = {}
    for m in b.MARKETS:
        poly = m.endswith("polygon")
        sd = [net["alice"], net["bob"]] if poly else seeders
        mid, placed = meshmm.seed_book(sd, m, prices, per_wallet=(4, 5) if poly else (3, 4), budget=budget, max_lots=3)
        books[m] = {"midRate": mid, "seeded": len(placed)}
    for m in b.MARKETS:
        # Sub-resolution books (lbc_btc) are short of seeded asks (100k-LBC lots on
        # small wallets), so the mm quotes more levels there.
        mm = meshmm.MeshMM(net["mm"], m, lambda: net.prices, levels=6 if h.rate_for(net.prices, m) < 50 else 4, lots=1)
        mm.start()
        net.mms.append(mm)
    time.sleep(40)
    short = []
    for m in b.MARKETS:
        ords = net["carol"].book(m)
        bids = [o for o in ords if not o["sell"]]
        asks = [o for o in ords if o["sell"]]
        books[m].update({"bids": len(bids), "asks": len(asks), "bestBid": max([o["rate"] for o in bids] or [0]),
                         "bestAsk": min([o["rate"] for o in asks] or [0]), "owners": len({o["owner"] for o in ords})})
        if len(bids) < 10 or len(asks) < 10:
            short.append(m)
    b.result("book", {"prices": prices, "books": books})
    b.shot(net["carol"], "/markets", "s3-book-markets", 6000)
    if short:
        raise Fail("fewer than 10 orders per side on %s: %s" % (short, {m: (books[m]["bids"], books[m]["asks"]) for m in short}))

def phase_warm(net):
    def ready():
        s = st(net["alice"])
        return not s["activeSet"]["fallback"] and s
    s = h.wait_until(ready, 900, "active-set window filled (60 counted epochs)", 10)
    row = snap(net, "warm")
    sets = {n: tuple(v["active"]) for n, v in row.items() if isinstance(v, dict)}
    if len(set(sets.values())) != 1 or len(next(iter(sets.values()))) != 4:
        raise Fail("warm: active sets %s" % sets)
    b.result("warm", {"activeSet": s["activeSet"], "active": active_names(s), "credit": {v["name"]: v["creditPct"] for v in s["validators"]},
                      "nodesAgree": sorted(sets)})
    b.shot(net["alice"], "/mesh", "s3-warm-all-active")

def phase_drop(net):
    v4 = net["v4"]
    vid = v4.peer_id
    e0 = finalized(net["alice"])
    v4.stop()
    t0 = time.time()
    log("v4 stopped at epoch %d" % e0)
    took, a = wait_active(net, "v4", False, 1200, tag="drop")
    time.sleep(8)
    e_drop = first_record_without(net["alice"], vid, e0)
    others = [n for n in running(net)]
    rows = snap(net, "drop: v4 inactive")
    agree = {n: v["active"] for n, v in rows.items() if isinstance(v, dict)}
    if any("v4" in x for x in agree.values()):
        raise Fail("v4 still active on some peers: %s" % agree)
    # Expected: missed > 20 % of the window's counted epochs, 100 epochs back.
    b.result("drop", {"stoppedAtEpoch": e0, "firstRecordWithoutV4": e_drop, "epochsUntilDrop": (e_drop - e0) if e_drop else None,
                      "seconds": round(time.time() - t0), "activeOnEveryPeer": agree, "v4CreditPct": a["credit"].get("v4"),
                      "sample": a["sample"]})
    b.shot(net["alice"], "/mesh", "s3-drop-v4-inactive")
    net.e_drop = e_drop

def phase_same(net, label="same"):
    nodes = running(net)
    rep = same_set(net, nodes, label)
    b.result(label, rep)

def phase_same2(net):
    phase_same(net, "same2")

def phase_quorum(net):
    al, v3 = net["alice"], net["v3"]
    s = st(al)
    if "v4" in active_names(s):
        raise Fail("quorum phase needs v4 inactive: %s" % active_names(s))
    v3.stop()
    log("v3 stopped: v1+v2 = 2 of 3 active, 2 of 4 bonded")
    time.sleep(6)
    f0 = finalized(al)
    # An epoch match while only 2 validators sign; restart v3 once agreed.
    m, oid, rate, took = epoch_trade3(net, "dcr_btc", net["alice"], net["bob"])
    f1 = finalized(al)
    time.sleep(max(0, 30 - took))
    f2 = finalized(al)
    recs = b.records(al, f0)
    sigs = sorted({len(r["signers"]) for r in recs.values()})
    act = sorted({r["active"] for r in recs.values()})
    stq = st(al)
    b.shot(al, "/mesh", "s3-quorum-2of3-active")
    net.start(v3)
    mid = m["match"]["id"]
    epoch = m["match"]["stamp"] // EPOCH_MS - 1
    rec = b.records(net["bob"], epoch - 1).get(epoch)
    log("match %s from epoch %d signed by %s" % (mid[:12], epoch, rec and len(rec["signers"])))
    mk, tk = net["alice"], net["bob"]
    h.wait_until(lambda: (mk.swap(mid) or {}).get("status") == "complete", 900, "maker swap complete", 3)
    h.wait_until(lambda: (tk.swap(mid) or {}).get("status") == "complete", 600, "taker swap complete", 3)
    sw = mk.swap(mid)
    json.dump(sw, open(os.path.join(EVID, "swap-dcr_btc-2of3-maker.json"), "w"), indent=2)
    res = {"finalizedWithV3V4Down": {"from": f0, "to": f2, "epochs": f2 - f0, "signersPerRecord": sigs, "activePerRecord": act,
                                     "activePower": stq["activePower"], "bondedPower": stq["bondedPower"], "canFinalize": stq["canFinalize"]},
           "match": {"id": mid, "epoch": epoch, "recordSigners": len(rec["signers"]) if rec else None,
                     "recordActive": rec and rec["active"], "agreedSecondsAfterTake": round(took, 1),
                     "txs": {k: sw[k]["txid"] for k in ("makerSwap", "takerSwap", "makerRedeem", "takerRedeem") if sw.get(k)},
                     "maker": sw["status"], "taker": tk.swap(mid)["status"]}}
    b.result("quorum", res)
    if f2 - f0 < 10 or sigs != [2] or act != [3]:
        raise Fail("quorum: %s" % res)

def epoch_trade3(net, mkt, maker, taker, lots=1):
    bb, ba = b.best_rates(taker, mkt)
    rate = ba - 1 if ba - 1 > bb else ba
    if rate <= bb:
        rate = bb + 1
    oid = maker.order(mkt, True, lots, rate, 900)
    h.wait_until(lambda: oid in [o["id"] for o in taker.book(mkt)], 60, "order on taker's book")
    t0 = time.time()
    r = taker.dbgapi("/mesh/take", {"market": mkt, "sell": False, "qty": lots * h.LOTS[mkt], "rate": rate, "lotSize": h.LOTS[mkt]})
    if "remain" not in r:
        raise Fail("take: %s" % r)
    def agreed():
        for m in taker.matches():
            if m["match"]["makerOrderID"] == oid and m["status"] == "agreed":
                return m
    m = h.wait_until(agreed, 90, "epoch match agreed for %s" % mkt)
    return m, oid, rate, time.time() - t0

def phase_rejoin(net):
    v4 = net["v4"]
    vid = v4.peer_id
    e1 = finalized(net["alice"])
    net.start(v4)
    t0 = time.time()
    h.wait_until(lambda: b.epoch_ok(v4), 300, "v4 synced", 3)
    s4 = st(v4)
    log("v4 back at epoch %d, synced; signing=%s, active on itself: %s" % (e1, s4["signing"], "v4" in active_names(s4)))
    took, a = wait_active(net, "v4", True, 3000, every=20, tag="rejoin")
    time.sleep(8)
    recs = b.records(net["alice"], e1)
    credited = sum(1 for r in recs.values() if vid in (r.get("credits") or []))
    first_in = None
    for e, r in sorted(recs.items()):
        if vid in r["activeSet"]:
            first_in = e
            break
    rows = snap(net, "rejoin: v4 active")
    agree = {n: v["active"] for n, v in rows.items() if isinstance(v, dict)}
    if any("v4" not in x for x in agree.values()):
        raise Fail("v4 not active on every peer: %s" % agree)
    b.result("rejoin", {"restartedAtEpoch": e1, "creditedRecordsSinceRestart": credited, "recordsSinceRestart": len(recs),
                        "firstRecordWithV4Active": first_in, "seconds": round(time.time() - t0),
                        "v4SelfStatus": {"signing": s4["signing"]}, "activeOnEveryPeer": agree, "v4CreditPct": a["credit"].get("v4")})
    b.shot(net["alice"], "/mesh", "s3-rejoin-v4-active")
    if credited == 0:
        raise Fail("v4 never credited")

def phase_floor(net):
    if "v5" not in net.nodes:
        v5 = h.Bisonw("v5", b.RUN, "127.0.0.1:16109", "127.0.0.1:16209", net.addr[3], b.BISONW, 16309)
        net.nodes["v5"] = v5
    v5 = net["v5"]
    if v5 not in net.vals:
        net.vals.append(v5)
    if not v5.pw:
        net.start(v5)
        v5.peer_id = v5.id()
    elif not getattr(v5, "peer_id", None):
        net.start(v5)
        v5.peer_id = v5.id()
    net.name_of[v5.peer_id] = "v5"
    for m in net.mms:
        m.stop()
    ids = [v.peer_id for v in net.vals]
    net.release = sign(net, "s3-floor-5val-w30.json", ids, [v.name for v in net.vals], FLOOR_FLAGS)
    nodes = net.vals + [net["alice"], net["bob"]]
    restart_all(net, nodes)
    al = net["alice"]
    h.wait_until(lambda: not st(al)["activeSet"]["fallback"], 300, "short window filled", 5)
    snap(net, "floor: warm", nodes)
    steps = []
    def drop(name):
        net[name].stop()
        took, a = wait_active(net, name, False, 300, nodes=[n for n in nodes if n.proc], every=4, tag="floor: %s down" % name)
        s = st(al)
        steps.append({"stopped": name, "inactiveAfterS": round(took), "active": active_names(s), "activePower": s["activePower"],
                      "bonded": s["bondedPower"], "canFinalize": s["canFinalize"], "last": s["lastFinalized"]})
        log("floor: %s inactive after %ds; active %s power %d/%d canFinalize %s" % (name, took, active_names(s), s["activePower"], s["bondedPower"], s["canFinalize"]))
    drop("v5")
    drop("v4")
    drop("v3")
    s = st(al)
    if s["activePower"] != 2 or s["canFinalize"]:
        raise Fail("floor: %s" % steps)
    time.sleep(6)
    stalled = finalized(al)
    bb, ba = b.best_rates(net["bob"], "lbc_dcr")
    oid = net["bob"].order("lbc_dcr", True, 1, 10 ** 6, 900)
    time.sleep(30)
    after = finalized(al)
    pool = al.dbgapi("/mesh/epochs/order?id=" + oid)["status"]
    b.shot(al, "/mesh", "s3-floor-not-finalizing")
    if after != stalled:
        raise Fail("finalized below the floor: %d -> %d" % (stalled, after))
    # The window drains (30 + 5 epochs): every validator counts again.
    h.wait_until(lambda: st(al)["activeSet"]["fallback"], 200, "window drained -> fallback", 5)
    s = st(al)
    drained = {"active": active_names(s), "activePower": s["activePower"], "canFinalize": s["canFinalize"], "last": s["lastFinalized"]}
    if s["lastFinalized"] != stalled:
        raise Fail("finalized during drain")
    net.start(net["v3"])
    net.start(net["v4"])
    h.wait_until(lambda: finalized(al) > stalled + 3, 180, "finalization resumes with 4 of 5")
    resumed = finalized(al)
    h.wait_until(lambda: al.dbgapi("/mesh/epochs/order?id=" + oid)["status"] in ("resting", "done"), 60, "stalled order included")
    net.start(net["v5"])
    h.wait_until(lambda: b.epoch_ok(net["v5"]), 180, "v5 synced")
    time.sleep(10)
    rep = same_set(net, [n for n in nodes if n.proc], "floor")
    b.result("floor", {"cascade": steps, "stalledAt": stalled, "finalizedIn30sBelowFloor": after - stalled, "orderDuringStall": pool,
                       "drained": drained, "resumedTo": resumed, "orderAfterResume": al.dbgapi("/mesh/epochs/order?id=" + oid)["status"],
                       "sameSet": rep})
    b.shot(al, "/mesh", "s3-floor-resumed")

PHASES = [("switch", phase_switch), ("book", phase_book), ("warm", phase_warm), ("drop", phase_drop), ("same", phase_same),
          ("quorum", phase_quorum), ("rejoin", phase_rejoin), ("same2", phase_same2), ("floor", phase_floor)]

def main():
    signal.signal(signal.SIGTERM, lambda *a: sys.exit(143))
    want = sys.argv[1:] or [p for p, _ in PHASES]
    os.makedirs(EVID, exist_ok=True)
    net = b.Net()
    ok = True
    try:
        b.setup(net)
        for v in net.vals + net.traders:
            v.peer_id = getattr(v, "peer_id", None) or v.id()
        net.name_of = {n.peer_id: n.name for n in net.vals}
        net.prices = h.fetch_prices()
        for name, fn in PHASES:
            if name not in want:
                continue
            log("==== phase %s" % name)
            t0 = time.time()
            try:
                fn(net)
                b.RESULTS.setdefault("pass", []).append(name)
                log("PASS %s (%.0fs)" % (name, time.time() - t0))
            except Exception as e:
                ok = False
                b.RESULTS.setdefault("fail", []).append({name: str(e)})
                log("FAIL %s: %s" % (name, e))
                if name in ("switch", "warm"):
                    break
            b.save_results()
    finally:
        for m in net.mms:
            m.stop()
            json.dump(m.log, open(os.path.join(EVID, "mm-%s.log.json" % m.mkt), "w"), indent=1)
        net.stop_all()
        if getattr(net, "miner", None):
            net.miner.stop()
        for n in net.nodes.values():
            try:
                shutil.copy(os.path.join(n.dir, "console.log"), os.path.join(EVID, "console-%s.log" % n.name))
            except Exception:
                pass
    log("pass: %s fail: %s" % (b.RESULTS.get("pass"), b.RESULTS.get("fail")))
    sys.exit(0 if ok else 1)

if __name__ == "__main__":
    main()
