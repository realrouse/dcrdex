#!/usr/bin/env python3
"""SIMNET/REGTEST ONLY. lbc_btc whole-sat check (Rouse 2026-11-04: lbc_btc
prices are whole sats per LBC, 1 sat minimum, next step 2 sat, no sub-sat).

The s012 network (4 validators, alice, bob, carol, mm) on the lbc_btc market
only:
  final   epochs finalize
  book    seeders and the emulated mm quote lbc_btc at the real price, which
          clamps to 1 sat
  onesat  the book is one-price and one-sided: asks only, every rate 1 sat,
          the mm never crosses itself; rate 0 is refused with the 1 sat
          minimum step error
  swaps   a 1 sat lbc_btc order matches in an epoch and the LBC/BTC regtest
          swap completes on both sides
  hist    the completed trade is in bob's price history (spot and candles)
          at 1 sat
Usage: ./lbcbtc1sat.py [phase ...]
"""
import os, sys, time
HERE = os.path.dirname(os.path.abspath(__file__))
os.environ.setdefault("EVID", "/workspace/tatanka-dev/evidence/lbcbtc-1sat")
os.environ.setdefault("BISONW", "/workspace/tatanka-dev/bin/bisonw-lbcbtc-1sat")
sys.path.insert(0, HERE)
import harness as h
from harness import log, Fail
import s012 as b

b.MARKETS = ["lbc_btc"]
b.SWAP_MARKETS = ["lbc_btc"]
MKT = "lbc_btc"

def phase_onesat(net):
    mid = h.rate_for(net.prices, MKT)
    sats = h.usd(net.prices, "lbc") / h.usd(net.prices, "btc") * 1e8
    ords = net["carol"].book(MKT)
    bids = [o for o in ords if not o["sell"]]
    asks = [o for o in ords if o["sell"]]
    mmid = net["mm"].id()
    mm = [o for o in ords if o["owner"] == mmid]
    out = {"realSatsPerLbc": round(sats, 4), "midRate": mid, "bids": len(bids), "asks": len(asks),
           "askRates": sorted({o["rate"] for o in asks}), "mmOrders": [(o["sell"], o["rate"]) for o in mm]}
    errs = []
    try:
        net["alice"].order(MKT, True, 1, 0, 60)
        errs.append("rate 0 accepted")
    except Exception as e:
        out["rate0Error"] = str(e)[:300]
        if "1 sat per LBC" not in str(e):
            errs.append("rate 0 error does not name the 1 sat step: %s" % e)
    b.result("onesat", out)
    log("onesat: %s" % out)
    if sats < 1.5 and mid != 1:
        errs.append("real price %.3f sat did not clamp to 1 sat (got %d)" % (sats, mid))
    if mid == 1:
        if bids:
            errs.append("bids on a one-price 1 sat book: %s" % [(o["owner"][:8], o["rate"]) for o in bids])
        if any(o["rate"] != 1 for o in asks) or not asks:
            errs.append("asks not all at 1 sat: %s" % out["askRates"])
        if not mm or any(not s for s, _ in out["mmOrders"]):
            errs.append("mm not quoting asks only: %s" % out["mmOrders"])
    if errs:
        raise Fail("; ".join(errs))

def phase_hist(net):
    sw = b.RESULTS.get("swaps", {}).get(MKT)
    if not sw:
        raise Fail("no lbc_btc swap")
    bo = net["bob"]
    def spot():
        r = bo.api("mesh/spots")
        s = (r.get("spots") or {}).get(MKT)
        return s if s and s.get("rate") else None
    s = h.wait_until(spot, 120, "lbc_btc spot on bob")
    c = bo.api("mesh/candles", {"market": MKT, "dur": "5m"})
    cs = ((c.get("candles") or {}).get("candles")) or []
    # Reused appdata keeps older history (pre-change lbc_btc trades at 2 sat):
    # only the bins from this run's match on count.
    since = (sw["epoch"] + 1) * 10000 - 300_000
    cs = [x for x in cs if x["endStamp"] >= (sw["epoch"] + 1) * 10000 and x["startStamp"] >= since]
    out = {"spot": s, "candles": cs[-3:], "swapRate": sw["rate"]}
    b.result("hist", out)
    log("hist: spot rate %s, %d candles" % (s.get("rate"), len(cs)))
    if s["rate"] != 1 or not cs or any(x[k] != 1 for x in cs for k in ("startRate", "endRate", "highRate", "lowRate")):
        raise Fail("price history not at 1 sat: %s" % out)

b.PHASES = [p for p in b.PHASES if p[0] in ("final", "book")] + [("onesat", phase_onesat)] + \
    [p for p in b.PHASES if p[0] == "swaps"] + [("hist", phase_hist)]

if __name__ == "__main__":
    if len(sys.argv) == 1:
        sys.argv += [p for p, _ in b.PHASES]
    b.main()
