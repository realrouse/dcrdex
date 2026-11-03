#!/usr/bin/env python3
"""SIMNET/REGTEST ONLY. S7 (verified price history) end-to-end test on the box.

Same network as s6.py: 4 seeded validators (SPV wallets for BTC, LBC and DCR,
used to check swap contracts on chain), traders alice, bob, carol, mm, dave
and erin with realistic balances (1-5 M LBC, ~200 DCR, ~0.355 BTC), books at
real CoinGecko prices with 10+ orders per side, an emulated market maker on
every market, and trading bonds required (S6). The release adds the S7
rules: price history only from trades whose swaps completed on chain,
admitted in finalized epochs (1 confirmation per contract on simnet).

Phases (default all, in order):
  wallets   dave, erin: SPV wallets, realistic funding
  switch    sign the S6+S7 release, restart every node with it
  bonds     trading bonds (erin stays unbonded)
  book      seed books, start market makers
  trade     honest epoch swaps on dcr_btc, lbc_dcr and lbc_btc complete; each
            enters the verified history on every peer with the match's rate
            and the swap's contracts
  unswapped dave (maker) never initiates: dave and alice both sign a proof
            with invented contracts; erin forges proofs for that match and for
            a match that does not exist. None is admitted
  refund    alice/dave swap refunded on both sides (short locks, maker silent
            before redeem); both sign a proof with the real contracts: refused
            (spent only after the lock = refund). A one-sided proof stays one-
            sided
  fake      erin publishes a fake signed trade summary (the pre-S7 path) at
            10x the price: the UI candles and spots do not move
  agree     every peer: the same verified trades and candles; the candles
            equal those rebuilt from the completed swaps of every trader
  restart   every node restarted: the same history
Usage: ./s7.py [phase ...]
"""
import os, sys, time, json, shutil, signal
HERE = os.path.dirname(os.path.abspath(__file__))
os.environ.setdefault("EVID", "/workspace/tatanka-dev/evidence/s7-price-history")
os.environ.setdefault("BISONW", "/workspace/tatanka-dev/bin/bisonw-bisonary-s7")
os.environ.setdefault("RUN", "/workspace/tatanka-dev/run/s012")
sys.path.insert(0, HERE)
import harness as h
from harness import log, Fail
import s012 as b
import s3
import s6

EVID = b.EVID
EPOCH_MS = 2000
S7_FLAGS = s6.S6_FLAGS + ["-s7", "-tradeconfs", "1", "-trademaxproofs", "16", "-tradeage", "3600", "-tradelockleft", "60"]
LOCK = 7200
s6.BONDS = {"alice": [(0, 50_000, LOCK, False)],
            "bob": [(140, 50_000 * 10**8, LOCK, False)],
            "carol": [(42, 250_000_000, LOCK, False)],
            "mm": [(0, 50_000, LOCK, False), (140, 50_000 * 10**8, LOCK, False)],
            "dave": [(42, 250_000_000, LOCK, False)],
            "erin": [(140, 5_000 * 10**8, s6.SHORT_LOCK, False), (42, 50_000_000, LOCK, True)]}
s6.WANT_TIERS = {"alice": 5, "bob": 5, "carol": 5, "mm": 10, "dave": 5, "erin": 0}
UTXO = ("dcr_btc", "lbc_dcr", "lbc_btc")
BIN = "1m"
TRADES = []   # honest trades: {"market", "match", "maker", "taker", ...}

def prices(bw, bin=BIN):
    return bw.dbgapi("/mesh/epochs/prices?bin=" + bin)

def vtrades(bw):
    st = prices(bw)["status"]
    return {(t["makerTx"], t["takerTx"]): t for m in st["markets"].values() for t in m["trades"]}

def peers(net):
    return s6.peers(net)

def wait_admitted(net, sw, timeout=240):
    key = (sw["makerSwap"]["txid"], sw["takerSwap"]["txid"])
    t0 = time.time()
    def every():
        got = {}
        for n in peers(net):
            t = vtrades(n).get(key)
            if not t:
                return None
            got[n.name] = t
        return got
    got = h.wait_until(every, timeout, "trade %s verified on every peer" % sw["match"]["id"][:10], 3)
    return got, time.time() - t0

def phase_switch(net):
    ids = [v.peer_id for v in net.vals]
    path = b.sign_release(os.path.join(b.RUN, "s7-simnet-2s-pricehist.json"), ids, [v.name for v in net.vals], S7_FLAGS)
    shutil.copy(path, EVID)
    net.release = path
    log(h.run([b.RELTOOL, "verify", path]).splitlines()[0])
    s6.restart_all(net, net.vals + net.traders)
    time.sleep(6)
    rel = json.load(open(path))
    relc = rel["release"] if isinstance(rel["release"], dict) else json.loads(rel["release"])
    on = {n.name: prices(n)["status"]["on"] for n in peers(net)}
    b.result("switch", {"release": os.path.basename(path), "priceHistoryRules": relc.get("priceHistory"),
                        "userBondRules": relc.get("userBonds"), "priceHistoryOnEveryPeer": on})
    if not all(on.values()) or not relc.get("priceHistory"):
        raise Fail("switch: %s" % on)

def one_trade(net, mkt, mk, tk, lots=1):
    m, oid, rate, took = s6.epoch_trade(net, mkt, mk, tk, lots)
    mid = m["match"]["id"]
    h.wait_until(lambda: (mk.swap(mid) or {}).get("status") == "complete", 900, "%s maker swap complete" % mkt, 3)
    h.wait_until(lambda: (tk.swap(mid) or {}).get("status") == "complete", 600, "%s taker swap complete" % mkt, 3)
    t_done = time.time()
    sw = mk.swap(mid)
    got, secs = wait_admitted(net, sw)
    t = got["v1"]
    rec = {"market": mkt, "match": mid, "maker": mk.name, "taker": tk.name, "matchRate": m["match"]["rate"], "matchQty": m["match"]["qty"],
           "matchStamp": m["match"]["stamp"], "epochIndex": "%d:%d" % (t["epoch"], t["index"]),
           "verified": {"rate": t["rate"], "qty": t["qty"], "stamp": t["stamp"], "admittedEpoch": t["admitted"]},
           "txs": {k: sw[k]["txid"] for k in ("makerSwap", "takerSwap", "makerRedeem", "takerRedeem") if sw.get(k)},
           "secondsCompleteToVerifiedEveryPeer": round(time.time() - t_done, 1),
           "identicalOnEveryPeer": len({json.dumps(v, sort_keys=True) for v in got.values()}) == 1, "peers": len(got)}
    ok = t["rate"] == m["match"]["rate"] and t["qty"] == m["match"]["qty"] and t["stamp"] == m["match"]["stamp"] and rec["identicalOnEveryPeer"]
    rec["ok"] = ok
    json.dump(sw, open(os.path.join(EVID, "swap-%s-%s.json" % (mkt, mid[:8])), "w"), indent=2)
    return rec

def phase_trade(net):
    al, bo, ca, dv = net["alice"], net["bob"], net["carol"], net["dave"]
    plan = [("dcr_btc", dv, al), ("lbc_dcr", bo, ca), ("lbc_btc", ca, bo), ("dcr_btc", al, dv), ("lbc_dcr", ca, bo), ("dcr_btc", dv, al)]
    out = []
    for mkt, mk, tk in plan:
        r = one_trade(net, mkt, mk, tk)
        log("verified %s %s @ %d in %.0f s on %d peers" % (mkt, r["match"][:10], r["verified"]["rate"], r["secondsCompleteToVerifiedEveryPeer"], r["peers"]))
        out.append(r)
        TRADES.append(r)
        b.result("trade", {"trades": out})
    bad = [r for r in out if not r["ok"]]
    if bad:
        raise Fail("trade: %s" % bad)

def pool_of(v, key):
    st = prices(v)["status"]
    return [p for p in st["pool"] if p["key"] == key], {k: r for k, r in st["refused"].items() if k.startswith(key + ":")}

def not_admitted_everywhere(net, key):
    """key epoch:index is in no peer's verified history."""
    bad = {}
    for n in peers(net):
        st = prices(n)["status"]
        for m in st["markets"].values():
            for t in m["trades"]:
                if "%d:%d" % (t["epoch"], t["index"]) == key:
                    bad[n.name] = t
    return bad

def epoch_key(net, mid):
    """epoch:index of a match from the records (taker side)."""
    for n in (net["alice"], net["dave"]):
        sw = n.swap(mid)
        if sw:
            stamp = sw["match"]["stamp"]
            ep = stamp // EPOCH_MS - 1
            rec = b.records(n, ep - 1).get(ep)
            for i, m in enumerate(rec["matches"] or []):
                if m["makerOrderID"] == sw["match"]["makerOrderID"] and m["taker"] == sw["match"]["taker"]:
                    return "%d:%d" % (ep, i)
    raise Fail("no epoch key for %s" % mid)

def phase_unswapped(net):
    al, dv, er = net["alice"], net["dave"], net["erin"]
    dv.dbgapi("/mesh/swap/halt", {"stage": "init"})
    try:
        m, oid, rate, took = s6.epoch_trade(net, "dcr_btc", dv, al)
        mid = m["match"]["id"]
        h.wait_until(lambda: dv.swap(mid) and al.swap(mid), 60, "swap records on both sides", 2)
        key = epoch_key(net, mid)
        out = {"match": mid, "key": key}
        out["daveFake"] = dv.dbgapi("/mesh/epochs/tradeproof", {"match": mid, "force": True, "fake": True})
        out["aliceFake"] = al.dbgapi("/mesh/epochs/tradeproof", {"match": mid, "force": True, "fake": True})
        ep = int(key.split(":")[0])
        out["erinForgedRealMatch"] = er.dbgapi("/mesh/epochs/tradeproof/forge", {"market": "dcr_btc", "epoch": ep, "index": int(key.split(":")[1]), "qty": 10**8})
        last = s3.finalized(al)
        out["erinForgedNoMatch"] = er.dbgapi("/mesh/epochs/tradeproof/forge", {"market": "dcr_btc", "epoch": last - 3, "index": 57, "qty": 10**8})
        e0 = s3.finalized(al)
        time.sleep(80)  # 40 epochs
        out["epochsWaited"] = s3.finalized(al) - e0
        out["validators"] = {}
        for v in net.vals:
            pool, refused = pool_of(v, key)
            _, refused2 = pool_of(v, "%d:57" % (last - 3))
            out["validators"][v.name] = {"pool": pool, "refused": refused, "refusedNoMatch": refused2}
        out["admittedOn"] = not_admitted_everywhere(net, key)
        out["swapStatus"] = {"dave": dv.swap(mid)["status"], "alice": al.swap(mid)["status"]}
        b.result("unswapped", out)
        fakes_seen = all(any(p["makerSig"] and p["takerSig"] and not p["verified"] and p["err"] for p in x["pool"]) for x in out["validators"].values())
        erin_refused = all(any("signature" in r for r in x["refused"].values()) for x in out["validators"].values())
        nomatch_refused = all(any("no such finalized epoch match" in r for r in x["refusedNoMatch"].values()) for x in out["validators"].values())
        out["checks"] = {"fakePairUnverifiedOnEveryValidator": fakes_seen, "erinRefusedEveryValidator": erin_refused,
                         "noMatchRefusedEveryValidator": nomatch_refused, "admitted": bool(out["admittedOn"])}
        b.result("unswapped", out)
        if out["admittedOn"] or not fakes_seen or not erin_refused or not nomatch_refused:
            raise Fail("unswapped: %s" % out["checks"])
    finally:
        dv.dbgapi("/mesh/swap/halt", {"stage": ""})

def phase_refund(net):
    al, dv = net["alice"], net["dave"]
    mkt = "dcr_btc"
    orig = {n.name: n.dbgapi("/mesh/policy?market=" + mkt) for n in (al, dv)}
    for n in (al, dv):
        n.dbgapi("/mesh/config", {"market": mkt, "makerLockSec": 240, "takerLockSec": 120})
    al.dbgapi("/mesh/swap/halt", {"stage": "redeem"})
    try:
        m, oid, rate, took = s6.epoch_trade(net, mkt, al, dv)
        mid = m["match"]["id"]
        key = epoch_key(net, mid)
        h.wait_until(lambda: (dv.swap(mid) or {}).get("status") == "refunded", 900, "taker refund", 3)
        h.wait_until(lambda: (al.swap(mid) or {}).get("status") == "refunded", 900, "maker refund", 3)
    finally:
        al.dbgapi("/mesh/swap/halt", {"stage": ""})
        for n in (al, dv):
            n.dbgapi("/mesh/config", dict(orig[n.name], market=mkt))
    sa = al.swap(mid)
    out = {"match": mid, "key": key, "status": {"maker": sa["status"], "taker": dv.swap(mid)["status"]},
           "txs": {k: sa[k]["txid"] for k in ("makerSwap", "takerSwap", "makerRefund", "takerRefund") if sa.get(k)}}
    out["normalSubmit"] = al.dbgapi("/mesh/epochs/tradeproof", {"match": mid})
    out["makerHalf"] = al.dbgapi("/mesh/epochs/tradeproof", {"match": mid, "force": True})
    time.sleep(12)
    out["afterMakerHalf"] = {v.name: pool_of(v, key)[0] for v in net.vals}
    out["takerHalf"] = dv.dbgapi("/mesh/epochs/tradeproof", {"match": mid, "force": True})
    time.sleep(60)
    out["afterBoth"] = {v.name: dict(zip(("pool", "refused"), pool_of(v, key))) for v in net.vals}
    out["admittedOn"] = not_admitted_everywhere(net, key)
    half_ok = all(p and all(x["makerSig"] and not x["takerSig"] and not x["verified"] for x in p) for p in out["afterMakerHalf"].values())
    refused = all(any("refund" in r for r in x["refused"].values()) or any(p["dead"] and "refund" in p["err"] for p in x["pool"])
                  for x in out["afterBoth"].values())
    out["checks"] = {"normalSubmitRefused": "error" in out["normalSubmit"], "oneSidedStaysOneSided": half_ok,
                     "refundRefusedEveryValidator": refused, "admitted": bool(out["admittedOn"])}
    b.result("refund", out)
    if out["admittedOn"] or not half_ok or not refused or "error" not in out["normalSubmit"]:
        raise Fail("refund: %s" % out["checks"])

def ui_candles(bw, mkt, dur="5m"):
    r = bw.api("mesh/candles", {"market": mkt, "dur": dur})
    return (r.get("candles") or {}).get("candles")

def phase_fake(net):
    er, al = net["erin"], net["alice"]
    before = {n.name: {"ui": ui_candles(n, "dcr_btc"), "spots": n.api("mesh/spots").get("spots"), "legacy": prices(n)["signedSummaries"]["dcr_btc"]}
              for n in (al, net["bob"], net["v1"])}
    rate = h.rate_for(net.prices, "dcr_btc") * 10
    r = er.dbgapi("/mesh/trade/fakesummary", {"market": "dcr_btc", "rate": rate, "qty": 5 * 10**8, "taker": al.peer_id})
    time.sleep(15)
    after = {n.name: {"ui": ui_candles(n, "dcr_btc"), "spots": n.api("mesh/spots").get("spots"), "legacy": prices(n)["signedSummaries"]["dcr_btc"]}
             for n in (al, net["bob"], net["v1"])}
    hi = {k: max([c["highRate"] for c in v["ui"] or []] or [0]) for k, v in after.items()}
    out = {"fake": r, "fakeRate": rate, "legacySummariesBefore": {k: v["legacy"] for k, v in before.items()},
           "legacySummariesAfter": {k: v["legacy"] for k, v in after.items()}, "uiHighRateAfter": hi,
           "uiCandlesUnchanged": {k: before[k]["ui"] == after[k]["ui"] for k in after},
           "spotsUnchanged": {k: (before[k]["spots"] or {}).get("dcr_btc") == (after[k]["spots"] or {}).get("dcr_btc") for k in after}}
    b.result("fake", out)
    if not all(out["uiCandlesUnchanged"].values()) or not all(out["spotsUnchanged"].values()) or any(v >= rate for v in hi.values()):
        raise Fail("fake summary moved the UI history: %s" % out)

def candles_from_swaps(swaps, bin_ms=60_000):
    by = {}
    for s in sorted(swaps, key=lambda s: (s["stamp"], s["index"])):
        by.setdefault(s["market"], []).append(s)
    out = {}
    for mkt, ts in by.items():
        cs = []
        for t in ts:
            start = t["stamp"] // bin_ms * bin_ms
            if not cs or cs[-1]["startStamp"] != start:
                cs.append({"startStamp": start, "rates": [], "matchVolume": 0, "startRate": t["rate"]})
            c = cs[-1]
            c["rates"].append(t["rate"])
            c["matchVolume"] += t["qty"]
            c["endRate"], c["endStamp"] = t["rate"], t["stamp"]
        for c in cs:
            r = sorted(c.pop("rates"))
            n = len(r)
            c.update({"highRate": max(r), "lowRate": min(r), "trades": n, "medianRate": r[n // 2] if n % 2 else (r[n // 2 - 1] + r[n // 2]) // 2})
        out[mkt] = cs
    return out

def run_start_ms(net):
    """Swaps from this S7 chain only (the appdata keeps earlier runs' swaps)."""
    return int(os.path.getmtime(os.path.join(b.RUN, "s7-simnet-2s-pricehist.json")) * 1000)

def completed_swaps(net):
    """Every completed epoch swap on the UTXO markets, from all traders' records."""
    seen = {}
    for t in net.traders:
        for s in t.swaps():
            mkt = "%s_%s" % tuple(next(k for k, v in h.ASSET.items() if v == s["match"]["market"][x]) for x in ("base", "quote"))
            if mkt not in UTXO or s["match"]["stamp"] % EPOCH_MS or s["match"]["stamp"] < run_start_ms(net):
                continue
            e = seen.setdefault(s["match"]["id"], {"market": mkt, "rate": s["match"]["rate"], "qty": s["match"]["qty"], "stamp": s["match"]["stamp"],
                                                   "statuses": {}, "makerTx": (s.get("makerSwap") or {}).get("txid"), "takerTx": (s.get("takerSwap") or {}).get("txid")})
            e["statuses"][t.name] = s["status"]
    return seen

def phase_agree(net):
    for m in net.mms:
        m.stop()
    net.mms = []
    # Let in-flight mm swaps finish and their proofs land.
    end = time.time() + 600
    while time.time() < end:
        sw = completed_swaps(net)
        active = [k for k, v in sw.items() if any(st not in ("complete", "refunded", "abandoned") for st in v["statuses"].values())]
        if not active:
            break
        time.sleep(10)
    time.sleep(30)
    nodes = peers(net)
    def same():
        sts = {n.name: prices(n)["status"] for n in nodes}
        hs = {k: (v["verified"], v["hash"]) for k, v in sts.items()}
        return sts if len(set(hs.values())) == 1 else None
    sts = h.wait_until(same, 120, "same verified history on every peer", 4)
    ref = sts["v1"]
    sw = completed_swaps(net)
    done = {k: v for k, v in sw.items() if all(st == "complete" for st in v["statuses"].values()) and len(v["statuses"]) == 2}
    notdone = {k: v for k, v in sw.items() if k not in done}
    vt = {(t["makerTx"], t["takerTx"]): dict(t, market=mk) for mk, m in ref["markets"].items() for t in m["trades"]}
    missing = [k for k, v in done.items() if (v["makerTx"], v["takerTx"]) not in vt]
    extra = [t for key, t in vt.items() if not any((v["makerTx"], v["takerTx"]) == key for v in done.values())]
    rebuilt = candles_from_swaps([{"market": v["market"], "rate": v["rate"], "qty": v["qty"], "stamp": v["stamp"],
                                   "index": vt[(v["makerTx"], v["takerTx"])]["index"] if (v["makerTx"], v["takerTx"]) in vt else 0}
                                  for v in done.values()])
    cand_eq = {}
    for mkt, cs in rebuilt.items():
        got = [{k: c[k] for k in ("startStamp", "endStamp", "matchVolume", "startRate", "endRate", "highRate", "lowRate", "trades", "medianRate")}
               for c in (ref["markets"].get(mkt) or {}).get("candles") or []]
        cand_eq[mkt] = got == cs
    ui = {mkt: ui_candles(net["alice"], mkt, "5m") for mkt in UTXO}
    res = {"peers": [n.name for n in nodes], "verifiedTrades": ref["verified"], "hash": ref["hash"],
           "candlesHash": {k: v["candlesHash"] for k, v in ref["markets"].items()},
           "completedSwaps": len(done), "notCompletedSwaps": {k[:10]: v["statuses"] for k, v in notdone.items()},
           "completedButNotVerified": missing, "verifiedButNotCompleted": extra, "candlesEqualRebuiltFromSwaps": cand_eq,
           "candles1m": {k: v["candles"] for k, v in ref["markets"].items()}, "uiCandles5mAlice": ui}
    json.dump(sts, open(os.path.join(EVID, "prices-every-peer.json"), "w"), indent=1)
    b.result("agree", res)
    net.agreed = ref
    if missing or extra or not all(cand_eq.values()) or ref["verified"] < len(TRADES):
        raise Fail("agree: missing %s extra %s candles %s" % (missing, extra, cand_eq))

def phase_restart(net):
    before = prices(net["v1"])["status"]
    s6.restart_all(net, net.vals + net.traders)
    time.sleep(10)
    nodes = peers(net)
    after = {n.name: prices(n)["status"] for n in nodes}
    res = {"before": {"verified": before["verified"], "hash": before["hash"]},
           "after": {k: {"verified": v["verified"], "hash": v["hash"]} for k, v in after.items()}}
    b.result("restart", res)
    if any(v["hash"] != before["hash"] or v["verified"] != before["verified"] for v in after.values()):
        raise Fail("restart: %s" % res)

PHASES = [("wallets", s6.phase_wallets), ("switch", phase_switch), ("bonds", s6.phase_bonds), ("book", s6.phase_book),
          ("trade", phase_trade), ("unswapped", phase_unswapped), ("refund", phase_refund), ("fake", phase_fake),
          ("agree", phase_agree), ("restart", phase_restart)]

def main():
    signal.signal(signal.SIGTERM, lambda *a: sys.exit(143))
    want = sys.argv[1:] or [p for p, _ in PHASES]
    os.makedirs(EVID, exist_ok=True)
    net = b.Net()
    net.extra = []
    for i, (name, mn, _) in enumerate(s6.EXTRA):
        n = h.Bisonw(name, b.RUN, "127.0.0.1:%d" % (16141 + i), "127.0.0.1:%d" % (16241 + i), net.addr[mn], b.BISONW, 16341 + i)
        net.nodes[name] = n
        net.extra.append(n)
    ok = True
    try:
        b.setup(net)
        for n in net.extra:
            s6.start_extra(net, n)
        net.traders = net.traders + net.extra
        for v in net.vals + net.traders:
            v.peer_id = getattr(v, "peer_id", None) or v.id()
        net.name_of = {n.peer_id: n.name for n in net.vals + net.traders}
        for n in net.vals + net.traders:
            for x in net.traders:
                try:
                    n.dbgapi("/mesh/epochs/peername", {"peerID": x.peer_id, "name": x.name})
                except Exception:
                    pass
        net.prices = h.fetch_prices()
        b.result("prices", net.prices)
        b.result("lbcBtcRate", {"lbcUsd": h.usd(net.prices, "lbc"), "btcUsd": h.usd(net.prices, "btc"),
                                "satsPerLbc": h.usd(net.prices, "lbc") / h.usd(net.prices, "btc") * 1e8,
                                "lbcBtcMsgRate": h.rate_for(net.prices, "lbc_btc"),
                                "btcLbcMsgRateIfFlipped": int(h.usd(net.prices, "btc") / h.usd(net.prices, "lbc") * 1e8)})
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
                if name in ("wallets", "switch", "bonds", "book"):
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
