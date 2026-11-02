#!/usr/bin/env python3
"""SIMNET/REGTEST ONLY. Bisonary S0-S2 end-to-end test on the box.

Private 3-node mesh (../mesh-p4.sh), 4 validator nodes (bisonw --meshvalidator,
no wallets) and 4 traders (alice, bob, carol, mm) with native SPV wallets
funded with realistic balances (1-5 M LBC, ~200 DCR, ~0.355 BTC; alice and
bob also POL + USDC on the polygon dev chain). Books seeded from real prices,
a basic market maker on every market.

Phases (default all, in order):
  s0       tampered / foreign-key releases are refused; protocol v2
  final    all 4 validators finalize; same chain on every node
  book     seed books, start market makers, screenshots
  swaps    epoch-matched swaps dcr_btc, lbc_btc, lbc_dcr, dcr_usdc.polygon
  refund   epoch-matched dcr_btc swap refunded (maker silent before redeem)
  kill     1/4 down still finalizes; 2/4 down stalls, orders roll; restart resumes
  clock    validator +3 s: stops signing; others continue
  omit     proposer leaving out alice's orders is flagged
  forge    forged single-validator match and legacy proposal rejected
  det500   500 epochs at 2 s: identical records on all nodes
Usage: ./s012.py [phase ...]
"""
import signal
import json, os, shutil, subprocess, sys, time, random
import harness as h
from harness import log, Fail
import meshmm

HERE = os.path.dirname(os.path.abspath(__file__))
BISONW = os.environ.get("BISONW", "/workspace/tatanka-dev/bin/bisonw-bisonary-s012")
RELTOOL = os.path.join(h.BIN, "bisonary-release")
DEVKEY = os.environ.get("DEVKEY", "/workspace/tatanka-dev/keys/bisonary-dev-release.key")
RUN = os.environ.get("RUN", "/workspace/tatanka-dev/run/s012")
EVID = os.environ.get("EVID", "/workspace/tatanka-dev/evidence/s0-s2-epochs")
MESH = os.path.join(HERE, "..", "mesh-p4.sh")
NODE_PATH = "/workspace/tatanka-dev/scratch/uicheck/node_modules"
MARKETS = ["dcr_btc", "lbc_btc", "lbc_dcr", "dcr_usdc.polygon", "lbc_usdc.polygon"]
SWAP_MARKETS = ["dcr_btc", "lbc_btc", "lbc_dcr", "dcr_usdc.polygon"]
RESULTS = {}
RUN_STAMP = time.strftime("%Y%m%d-%H%M%S")

def save_results():
    """results-<stamp>.json for this run; results.json keeps the latest
    result and pass/fail of every phase across runs."""
    json.dump(RESULTS, open(os.path.join(EVID, "results-%s.json" % RUN_STAMP), "w"), indent=2)
    path = os.path.join(EVID, "results.json")
    try:
        allr = json.load(open(path))
    except Exception:
        allr = {}
    status = allr.setdefault("status", {})
    for name in RESULTS.get("pass", []):
        status[name] = {"ok": True, "run": RUN_STAMP}
    for f in RESULTS.get("fail", []):
        for name, why in f.items():
            status[name] = {"ok": False, "run": RUN_STAMP, "error": why}
    for k, v in RESULTS.items():
        if k not in ("pass", "fail"):
            allr[k] = v
    json.dump(allr, open(path, "w"), indent=2)

def result(k, v):
    RESULTS[k] = v
    save_results()
    log("RESULT %s: %s" % (k, json.dumps(v)[:600]))

def mesh_addr(n):
    return subprocess.run([MESH, "addr", str(n)], capture_output=True, text=True, check=True).stdout.strip()

def shot(bw, path, name, wait=4000, sel=""):
    out = os.path.join(EVID, "shots", name + ".png")
    os.makedirs(os.path.dirname(out), exist_ok=True)
    env = dict(os.environ, APP_PASS=bw.pw, NODE_PATH=NODE_PATH)
    r = subprocess.run(["node", os.path.join(HERE, "shot.js"), "http://" + bw.web, path, out, str(wait), sel], env=env,
                       capture_output=True, text=True, timeout=120)
    log("screenshot %s %s" % (out, (r.stdout + r.stderr).strip()[:200]))
    return out

def sign_release(path, ids, names, extra=()):
    h.run([RELTOOL, "sign", "-key", DEVKEY, "-name", os.path.basename(path).replace(".json", ""), "-validators", ",".join(ids),
           "-names", ",".join(names), "-out", path] + list(extra))
    return path

class Net:
    def __init__(self):
        subprocess.run([MESH, "start"], check=True, capture_output=True)
        self.addr = {n: mesh_addr(n) for n in (1, 2, 3)}
        self.nodes = {}
        port = 0
        layout = [("v1", 1), ("v2", 2), ("v3", 3), ("v4", 1), ("alice", 2), ("bob", 3), ("carol", 1), ("mm", 2)]
        for i, (name, mn) in enumerate(layout):
            self.nodes[name] = h.Bisonw(name, RUN, "127.0.0.1:%d" % (16101 + i), "127.0.0.1:%d" % (16201 + i), self.addr[mn],
                                        BISONW, 16301 + i)
        self.vals = [self.nodes[v] for v in ("v1", "v2", "v3", "v4")]
        self.traders = [self.nodes[t] for t in ("alice", "bob", "carol", "mm")]
        self.release = None
        self.mms = []

    def __getitem__(self, k):
        return self.nodes[k]

    def start(self, bw, env=None):
        extra = ["--meshrelease", self.release] if self.release else []
        if bw in self.vals:
            extra.append("--meshvalidator")
        bw.start(extra, env)
        if bw.pw:
            bw.login()
        else:
            bw.init()

    def restart(self, bw, env=None):
        bw.stop()
        time.sleep(1)
        self.start(bw, env)

    def stop_all(self):
        for m in self.mms:
            m.stop()
        for bw in self.nodes.values():
            bw.stop()

def epoch_ok(bw):
    st = bw.epochs()
    return st.get("haveFinalized") and st.get("unfinalizedSinceLast", 99) <= 1 and st

def records(bw, frm=0):
    return {r["epoch"]: r for r in bw.dbgapi("/mesh/epochs/records?from=%d&limit=1000" % frm)["records"] or []}

def compare_chains(nodes, frm):
    ref = records(nodes[0], frm)
    rep = {"nodes": [n.name for n in nodes], "epochs": len(ref)}
    common = set(ref)
    for n in nodes[1:]:
        rs = records(n, frm)
        common &= set(rs)
        for e in set(rs) & set(ref):
            if rs[e]["fingerprint"] != ref[e]["fingerprint"] or rs[e]["stateHash"] != ref[e]["stateHash"]:
                raise Fail("epoch %d differs between %s and %s" % (e, nodes[0].name, n.name))
    rep["common"] = len(common)
    rep["matches"] = sum(len(ref[e]["matches"] or []) for e in common)
    return rep

# ---------------- phases ----------------
def setup(net):
    os.makedirs(EVID, exist_ok=True)
    # Reuse the previous run's appdata (S012_REUSE=1, default): the traders
    # keep their funded wallets and are only topped up. Mining millions of
    # LBC again on regtest takes the better part of an hour.
    reuse = os.environ.get("S012_REUSE", "1") == "1" and os.path.exists(os.path.join(RUN, "alice", ".testpw"))
    if not reuse:
        if os.path.exists(RUN):
            os.rename(RUN, RUN + time.strftime(".%Y%m%d-%H%M%S"))   # never delete appdata with possible contracts
        os.makedirs(RUN)
        for n in net.nodes.values():
            n.pw = None
    log("setup: %s %s" % ("reusing" if reuse else "fresh", RUN))
    # 1. Learn the validators' mesh peer IDs (derived from their app seeds).
    for v in net.vals:
        net.start(v)
    ids = [v.id() for v in net.vals]
    for v in net.vals:
        v.stop()
    net.release = sign_release(os.path.join(RUN, "bisonary-simnet-10s.json"), ids, [v.name for v in net.vals])
    shutil.copy(net.release, EVID)
    log("release %s signed with the dev key for validators %s" % (net.release, ids))
    log(h.run([RELTOOL, "verify", net.release]).splitlines()[0])
    for v in net.vals:
        net.start(v)
    # 2. Traders with realistic balances.
    for t in net.traders:
        net.start(t)
    lbc_amounts = dict(zip([t.name for t in net.traders], h.REALISTIC_LBC))
    for t in net.traders:
        for a in ("lbc", "btc", "dcr"):
            t.new_wallet(a, "SPV")
    for t in (net["alice"], net["bob"], net["mm"]):
        t.new_wallet("polygon", "rpc", {"providers": "ws://127.0.0.1:34983"})
        t.new_wallet("usdc.polygon", "token", {})
    if reuse:  # let reused SPV wallets sync before reading balances
        for t in net.traders:
            for a in ("lbc", "dcr", "btc"):
                try:
                    h.wait_until(lambda: t.coins(a, "total") > 0, 120, "%s %s balance" % (t.name, a), 2)
                except Fail as e:
                    log("note: %s" % e)
    usdcs = {"alice": 5000, "bob": 8000, "mm": 3000}
    def deficit(t, a, target):
        have = t.coins(a, "total")
        return 0 if have >= target * 0.98 else target - have
    plan = {}
    for t in net.traders:
        plan[t.name] = {"lbc": int(round(deficit(t, "lbc", lbc_amounts[t.name]))), "dcr": round(deficit(t, "dcr", h.REALISTIC_DCR), 4),
                        "btc": round(deficit(t, "btc", h.REALISTIC_BTC), 6),
                        "usdc": usdcs.get(t.name) and deficit(t, "usdc.polygon", usdcs[t.name])}
    log("funding plan (top-up to target): %s" % plan)
    h.ensure_lbc_supply(sum(p["lbc"] for p in plan.values()) + 1000)
    net.miner = h.Miner(3)
    net.miner.start()
    funding = {}
    for t in net.traders:
        p = plan[t.name]
        h.fund_realistic(t, p["lbc"], dcr=p["dcr"], btc=p["btc"], polygon_usdc=int(p["usdc"]) if p["usdc"] else None)
        funding[t.name] = {"targetLBC": lbc_amounts[t.name], "targetDCR": h.REALISTIC_DCR, "targetBTC": h.REALISTIC_BTC,
                           "targetUSDC": usdcs.get(t.name), "sent": p}
    bal = {}
    for t in net.traders:
        need_t = {"lbc": lbc_amounts[t.name] * 0.97, "dcr": 194, "btc": 0.344}
        if t.name in usdcs:
            need_t["usdc.polygon"] = usdcs[t.name] * 0.97
        bal[t.name] = h.wait_funded(t, need_t, 900)
    result("setup", {"release": os.path.basename(net.release), "validators": dict(zip([v.name for v in net.vals], ids)),
                     "funding": funding, "balances": bal})

def phase_s0(net):
    bad = os.path.join(RUN, "tampered.json")
    txt = open(net.release).read()
    tam = txt.replace('"epochMs": 10000', '"epochMs": 1000', 1)
    if tam == txt:
        raise Fail("could not tamper with the release text")
    open(bad, "w").write(tam)
    shutil.copy(bad, EVID)
    verify = subprocess.run([RELTOOL, "verify", bad], capture_output=True, text=True)
    td = os.path.join(RUN, "tamper")
    if os.path.exists(td):  # fresh appdata each time (no wallets in it)
        os.rename(td, td + time.strftime(".%H%M%S"))
    t = h.Bisonw("tamper", RUN, "127.0.0.1:16120", "127.0.0.1:16220", net.addr[1], BISONW, 16320)
    t.start(["--meshrelease", bad])
    try:
        t.pw = random.randbytes(8).hex()
        open(os.path.join(t.dir, ".testpw"), "w").write(t.pw)
        t.api("init", {"pass": t.pw})
        time.sleep(5)
        st = t.api("mesh/status")["status"]
        con = open(os.path.join(t.dir, "console.log")).read()
        line = [l for l in con.splitlines() if "release file" in l and "rejected" in l]
        shot(t, "/mesh", "s0-tampered-release-mesh-off", 3000)
    finally:
        t.stop()
        shutil.copy(os.path.join(t.dir, "console.log"), os.path.join(EVID, "console-tamper.log"))
    if st.get("enabled") or not line:
        raise Fail("tampered release accepted: %s" % st)
    st2 = net["alice"].api("mesh/status")["status"]
    result("s0", {"tamperedVerify": (verify.stdout + verify.stderr).strip(), "bisonwLog": line[0][-300:], "meshEnabled": st.get("enabled"),
                  "protocol": st2["protocolVersion"], "minProtocol": st2["minProtocolVersion"],
                  "peersProto": sorted({p.get("proto") for p in st2.get("peers") or []})})

def phase_final(net):
    for bw in net.vals + net.traders:
        h.wait_until(lambda: epoch_ok(bw), 120, "%s finalizing" % bw.name)
    time.sleep(35)
    st = net["alice"].epochs()
    frm = st["lastFinalized"] - 5
    rep = compare_chains(net.vals + net.traders, frm)
    sig = {r["epoch"]: len(r["signers"]) for r in records(net["alice"], frm).values()}
    result("final", {"chain": rep, "signers": sig, "aliceStatus": {k: st[k] for k in ("lastFinalized", "finalized", "skipped", "activePower", "bondedPower", "clockSkewMs", "clockOK")}})
    shot(net["alice"], "/mesh", "s1-mesh-epochs-trader")
    shot(net["v1"], "/mesh", "s1-mesh-epochs-validator")

def phase_book(net):
    prices = h.fetch_prices()
    net.prices = prices
    seeders = [net["alice"], net["bob"], net["carol"]]
    books = {}
    bal = {t.name: {a: t.coins(a) for a in ("lbc", "dcr", "btc")} for t in seeders}
    def budget(t, mkt, sell):
        b, q = h.market_assets(mkt)
        lot = h.LOTS[mkt] / 1e8
        if sell:
            have = bal[t.name].get(b, 0)
            return int(have * 0.15 / lot)
        have = bal[t.name].get(q, 5000 if q.startswith("usdc") else 0)
        rate = h.rate_for(prices, mkt) / 10 ** h.DECIMALS[q]
        return int(have * 0.15 / (lot * rate)) if rate else 0
    for t in net.traders:
        for m in MARKETS:
            t.subscribe(m)
    for m in MARKETS:
        poly = m.endswith("polygon")
        sd = [net["alice"], net["bob"]] if poly else seeders
        # Only two wallets hold USDC: more orders each to keep 5-10 per side.
        mid, placed = meshmm.seed_book(sd, m, prices, per_wallet=(3, 5) if poly else (2, 3), budget=budget, max_lots=4)
        books[m] = {"midRate": mid, "seeded": len(placed)}
    for m in MARKETS:
        mm = meshmm.MeshMM(net["mm"], m, lambda: net.prices, levels=3, lots=1)
        mm.start()
        net.mms.append(mm)
    time.sleep(40)
    for m in MARKETS:
        ords = net["carol"].book(m)
        bids = [o for o in ords if not o["sell"]]
        asks = [o for o in ords if o["sell"]]
        books[m].update({"bids": len(bids), "asks": len(asks), "bestBid": max([o["rate"] for o in bids] or [0]),
                         "bestAsk": min([o["rate"] for o in asks] or [0]), "owners": len({o["owner"] for o in ords})})
    result("book", {"prices": prices, "books": books, "mm": {m.mkt: m.log[-1:] for m in net.mms}})
    shot(net["carol"], "/markets", "book-markets-page", 6000)
    shot(net["carol"], "/mesh", "book-mesh-page", 3000)

def best_rates(bw, mkt):
    ords = bw.book(mkt)
    bids = [o["rate"] for o in ords if not o["sell"]]
    asks = [o["rate"] for o in ords if o["sell"]]
    return max(bids or [0]), min(asks or [0])

def epoch_trade(net, mkt, maker, taker, lots=1, sell=True):
    """maker posts the best ask (sell) and taker crosses it; returns the match ID."""
    bb, ba = best_rates(taker, mkt)
    rate = ba - 1 if ba - 1 > bb else ba
    if rate <= bb:
        rate = bb + 1
    oid = maker.order(mkt, sell, lots, rate, 900)
    h.wait_until(lambda: oid in [o["id"] for o in taker.book(mkt)], 60, "order on taker's book")
    t0 = time.time()
    r = taker.dbgapi("/mesh/take", {"market": mkt, "sell": not sell, "qty": lots * h.LOTS[mkt], "rate": rate, "lotSize": h.LOTS[mkt]})
    if "remain" not in r:
        raise Fail("take: %s" % r)
    def agreed():
        for m in taker.matches():
            if m["match"]["makerOrderID"] == oid and m["status"] == "agreed":
                return m
    m = h.wait_until(agreed, 90, "epoch match agreed for %s" % mkt)
    return m, oid, rate, time.time() - t0

def phase_swaps(net):
    out = {}
    pairs = {"dcr_btc": ("alice", "bob"), "lbc_btc": ("carol", "bob"), "lbc_dcr": ("bob", "alice"), "dcr_usdc.polygon": ("alice", "bob")}
    for mkt in SWAP_MARKETS:
        mk, tk = net[pairs[mkt][0]], net[pairs[mkt][1]]
        b, q = h.market_assets(mkt)
        before = {n.name: {a: n.coins(a, "total") for a in (b, q)} for n in (mk, tk)}
        m, oid, rate, took = epoch_trade(net, mkt, mk, tk)
        mid = m["match"]["id"]
        stamp = m["match"]["stamp"]
        epoch = stamp // 10000 - 1
        rec = records(tk, epoch - 1).get(epoch)
        log("%s: match %s from epoch %d (signers %d), agreed %.1fs after the take" % (mkt, mid[:12], epoch, len(rec["signers"]) if rec else -1, took))
        h.wait_until(lambda: (mk.swap(mid) or {}).get("status") == "complete", 900, "%s maker swap complete" % mkt, 3)
        h.wait_until(lambda: (tk.swap(mid) or {}).get("status") == "complete", 600, "%s taker swap complete" % mkt, 3)
        time.sleep(10)
        after = {n.name: {a: n.coins(a, "total") for a in (b, q)} for n in (mk, tk)}
        sw = mk.swap(mid)
        out[mkt] = {"match": mid, "maker": mk.name, "taker": tk.name, "rate": rate, "epoch": epoch,
                    "recordSigners": len(rec["signers"]) if rec else None, "matchStampIsEpochEnd": stamp % 10000 == 0,
                    "txs": {k: sw[k]["txid"] for k in ("makerSwap", "takerSwap", "makerRedeem", "takerRedeem") if sw.get(k)},
                    "balanceDelta": {n: {a: round(after[n][a] - before[n][a], 8) for a in (b, q)} for n in after}}
        json.dump(sw, open(os.path.join(EVID, "swap-%s-maker.json" % mkt), "w"), indent=2)
        result("swaps", out)
    shot(net["alice"], "/mesh", "s2-swaps-complete-mesh", 3000)

def phase_refund(net):
    mkt = "dcr_btc"
    a, b = net["alice"], net["bob"]
    for n in (a, b):
        n.dbgapi("/mesh/config", {"market": mkt, "makerLockSec": 240, "takerLockSec": 120})
    a.dbgapi("/mesh/swap/halt", {"stage": "redeem"})
    m, oid, rate, took = epoch_trade(net, mkt, a, b)
    mid = m["match"]["id"]
    h.wait_until(lambda: (b.swap(mid) or {}).get("status") == "refunded", 600, "taker refund", 3)
    h.wait_until(lambda: (a.swap(mid) or {}).get("status") == "refunded", 600, "maker refund", 3)
    a.dbgapi("/mesh/swap/halt", {"stage": ""})
    sa, sb = a.swap(mid), b.swap(mid)
    result("refund", {"match": mid, "maker": sa["status"], "taker": sb["status"],
                      "txs": {k: sa[k]["txid"] for k in ("makerSwap", "takerSwap", "makerRefund", "takerRefund") if sa.get(k)}})

def finalized(bw):
    return bw.epochs().get("lastFinalized", 0)

def phase_kill(net):
    al = net["alice"]
    net["v4"].stop()
    log("v4 stopped")
    time.sleep(45)
    f0 = finalized(al)
    time.sleep(60)
    f1 = finalized(al)
    st = al.epochs()
    one_down = {"finalizedIn60s": f1 - f0, "activePower": st["activePower"], "lastSigners": st["lastSigners"]}
    shot(al, "/mesh", "kill-1of4-still-finalizing")
    if f1 - f0 < 4:
        raise Fail("1/4 down: only %d epochs finalized in 60 s" % (f1 - f0))
    net["v3"].stop()
    log("v3 stopped (2 of 4 down)")
    time.sleep(15)
    f2 = finalized(al)
    # Orders submitted during the stall roll over.
    c, b = net["carol"], net["bob"]
    bb, ba = best_rates(b, "lbc_dcr")
    rate = ba - 1 if ba - 1 > bb else ba
    oid = c.order("lbc_dcr", True, 1, rate, 900)
    time.sleep(3)
    # A standing crossing order (a take's order lives only max(4 epochs, 30 s)
    # and would expire during the stall).
    b.order("lbc_dcr", False, 1, rate, 900)
    time.sleep(60)
    f3 = finalized(al)
    pool = al.dbgapi("/mesh/epochs/order?id=" + oid)["status"]
    st2 = net["v1"].epochs()
    shot(net["v1"], "/mesh", "kill-2of4-not-finalizing")
    if f3 - f2 > 1 or pool != "pool":
        raise Fail("2/4 down: finalized %d epochs, order %s" % (f3 - f2, pool))
    net.start(net["v3"])
    log("v3 restarted")
    h.wait_until(lambda: finalized(al) > f3 + 2, 120, "finalization resumes")
    f4 = finalized(al)
    h.wait_until(lambda: al.dbgapi("/mesh/epochs/order?id=" + oid)["status"] in ("done", "resting"), 60, "rolled order included")
    def matched():
        for m in b.matches():
            if m["match"]["makerOrderID"] == oid:
                return m["status"]
    ms = h.wait_until(matched, 90, "rolled order matched")
    net.start(net["v4"])
    h.wait_until(lambda: epoch_ok(net["v4"]), 120, "v4 synced")
    rep = compare_chains(net.vals + [al], f0)
    result("kill", {"oneDown": one_down, "twoDown": {"finalizedIn60s": f3 - f2, "orderStatus": pool, "canFinalize": st2["canFinalize"],
                                                       "activePower": st2["activePower"]},
                    "resumed": {"stalledAt": f3, "resumedTo": f4, "rolledOrder": al.dbgapi("/mesh/epochs/order?id=" + oid)["status"], "rolledMatch": ms},
                    "chainAfterRestart": rep})

def phase_clock(net):
    v4 = net["v4"]
    v4.stop()
    net.start(v4, env={"TATANKA_CLOCK_OFFSET_MS": "3000"})
    h.wait_until(lambda: v4.epochs()["clockPeers"] >= 2 and not v4.epochs()["signing"], 180, "v4 detects skew")
    st = v4.epochs()
    f0 = finalized(net["alice"])
    time.sleep(45)
    recs = records(net["alice"], f0)
    v4id = st["self"]
    signed = [e for e, r in recs.items() if v4id in (r["signers"] or [])]
    shot(v4, "/mesh", "clock-plus3s-not-signing")
    con = open(os.path.join(v4.dir, "console.log")).read()
    warn = [l for l in con.splitlines() if "CLOCK SKEW" in l]
    res = {"skewMs": st["clockSkewMs"], "signing": st["signing"], "epochsFinalizedWithout": len(recs), "epochsSignedByV4": len(signed),
           "log": warn[-1][-250:] if warn else ""}
    v4.stop()
    net.start(v4)
    h.wait_until(lambda: v4.epochs()["signing"] and v4.epochs()["clockPeers"] >= 2, 240, "v4 signing again")
    res["afterFix"] = {"skewMs": v4.epochs()["clockSkewMs"], "signing": True}
    result("clock", res)
    if signed or not warn:
        raise Fail("clock test: %s" % res)

def phase_omit(net):
    al = net["alice"]
    bad = net["v2"]
    bad.dbgapi("/mesh/epochs/omit", {"owner": al.peer_id})
    prices = net.prices
    mid = h.rate_for(prices, "lbc_dcr")
    oids = []
    for i in range(20):
        oids.append(al.order("lbc_dcr", False, 1, int(mid * 0.9) - i, 180))
        time.sleep(3)
    time.sleep(25)
    bad.dbgapi("/mesh/epochs/omit", {"owner": ""})
    flags = {}
    for v in (net["v1"], net["v3"], net["v4"]):
        fl = [f for f in v.epochs()["flags"] or [] if f["kind"] == "omission" and f["peer"] == bad.peer_id]
        flags[v.name] = len(fl)
    included = sum(1 for o in oids if al.dbgapi("/mesh/epochs/order?id=" + o)["status"] in ("resting", "done"))
    shot(net["v1"], "/mesh", "omission-flagged", 3000)
    result("omit", {"flagsByValidator": flags, "ordersIncludedLater": "%d/%d" % (included, len(oids))})
    for o in oids:
        try:
            al.cancel(o)
        except Exception:
            pass
    if not all(flags.values()):
        raise Fail("omission not flagged: %s" % flags)

def phase_forge(net):
    al, v1, c = net["alice"], net["v1"], net["carol"]
    v1.subscribe("dcr_btc")
    bb, ba = best_rates(c, "dcr_btc")
    oid = al.order("dcr_btc", True, 1, ba + 50, 600)
    h.wait_until(lambda: oid in [o["id"] for o in v1.book("dcr_btc")], 60, "order on v1 book")
    fid = v1.dbgapi("/mesh/epochs/forge", {"market": "dcr_btc", "orderID": oid, "qty": h.LOTS["dcr_btc"]})["matchID"]
    lid = c.dbgapi("/mesh/propose", {"market": "dcr_btc", "orderID": oid, "qty": h.LOTS["dcr_btc"], "rate": 0})["matchID"]
    def status(bw, mid):
        for m in bw.matches():
            if m["match"]["id"] == mid and m["status"] not in ("proposed",):
                return m
    f = h.wait_until(lambda: status(v1, fid), 60, "forged answer")
    l = h.wait_until(lambda: status(c, lid), 60, "legacy answer")
    al.cancel(oid)
    result("forge", {"forged": {"status": f["status"], "reason": f.get("reason")}, "legacy": {"status": l["status"], "reason": l.get("reason")},
                     "makerSwaps": len([s for s in al.swaps() if s["match"]["id"] in (fid, lid)])})
    if f["status"] != "rejected" or l["status"] != "rejected":
        raise Fail("forged/legacy not rejected")

def phase_det500(net):
    for m in net.mms:
        m.stop()
    nodes = net.vals + [net["alice"], net["bob"]]
    ids = [v.peer_id for v in net.vals]
    rel2 = sign_release(os.path.join(RUN, "bisonary-simnet-2s.json"), ids, [v.name for v in net.vals],
                        ["-epochms", "2000", "-windowms", "1000", "-proposems", "200", "-fallbackms", "300", "-omissionms", "500"])
    shutil.copy(rel2, EVID)
    net.release = rel2
    for n in net.nodes.values():
        n.stop()
    for n in nodes:
        net.start(n)
    for n in nodes:
        h.wait_until(lambda: epoch_ok(n), 120, "%s finalizing (2 s)" % n.name)
    start = finalized(net["alice"])
    log("determinism run from epoch %d" % start)
    rng = random.Random(500)
    mid = h.rate_for(net.prices, "lbc_dcr")
    t_end = time.time() + 500 * 2 + 4
    n_orders = 0
    while time.time() < t_end:
        tr = rng.choice([net["alice"], net["bob"]])
        sell = rng.random() < 0.5
        r = int(mid * (1 + (1 if sell else -1) * rng.uniform(0.01, 0.05)))
        if rng.random() < 0.04:   # now and then cross the book: a real match and swap
            r = int(mid * (0.9 if sell else 1.1))
        try:
            tr.order("lbc_dcr", sell, rng.randint(1, 3), r, 60)
            n_orders += 1
        except Exception as e:
            log("order:", e)
        time.sleep(rng.uniform(0.3, 2.5))
    time.sleep(6)
    end = finalized(net["alice"])
    rep = compare_chains(nodes, start)
    st = net["alice"].epochs()
    shot(net["alice"], "/mesh", "det500-2s-epochs")
    result("det500", {"fromEpoch": start, "toEpoch": end, "epochsElapsed": end - start, "ordersSubmitted": n_orders, "chain": rep,
                      "skipped": st["skipped"], "finalized": st["finalized"]})
    if rep["common"] < 480:
        raise Fail("only %d common records" % rep["common"])

PHASES = [("s0", phase_s0), ("final", phase_final), ("book", phase_book), ("swaps", phase_swaps), ("refund", phase_refund),
          ("kill", phase_kill), ("clock", phase_clock), ("omit", phase_omit), ("forge", phase_forge), ("det500", phase_det500)]

def main():
    signal.signal(signal.SIGTERM, lambda *a: sys.exit(143))  # still clean up
    want = sys.argv[1:] or [p for p, _ in PHASES]
    if set(want) & {"swaps", "refund", "kill", "omit", "forge"} and "book" not in want:
        want.append("book")  # these phases trade against the realistic books
    os.makedirs(EVID, exist_ok=True)
    net = Net()
    ok = True
    try:
        setup(net)
        net.prices = h.fetch_prices()
        for name, fn in PHASES:
            if name not in want:
                continue
            log("==== phase %s" % name)
            t0 = time.time()
            try:
                fn(net)
                RESULTS.setdefault("pass", []).append(name)
                log("PASS %s (%.0fs)" % (name, time.time() - t0))
            except Exception as e:
                ok = False
                RESULTS.setdefault("fail", []).append({name: str(e)})
                log("FAIL %s: %s" % (name, e))
            save_results()
    finally:
        for m in net.mms:
            json.dump(m.log, open(os.path.join(EVID, "mm-%s.log.json" % m.mkt), "w"), indent=1)
        net.stop_all()
        if getattr(net, "miner", None):
            net.miner.stop()
        for n in net.nodes.values():
            try:
                shutil.copy(os.path.join(n.dir, "console.log"), os.path.join(EVID, "console-%s.log" % n.name))
            except Exception:
                pass
    log("pass: %s fail: %s" % (RESULTS.get("pass"), RESULTS.get("fail")))
    sys.exit(0 if ok else 1)

if __name__ == "__main__":
    main()
