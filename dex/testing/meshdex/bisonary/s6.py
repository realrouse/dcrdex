#!/usr/bin/env python3
"""SIMNET/REGTEST ONLY. S6 (user fidelity bonds required to trade) end-to-end
test on the box.

Same network as s012.py/s3.py/s4.py/s5.py (private 3-node mesh, seeded
validators v1-v4 with SPV wallets for BTC, LBC and DCR to check bonds on
chain; traders alice, bob, carol and the market maker mm with realistic
balances: 1-5 M LBC, ~200 DCR, ~0.355 BTC; books at real CoinGecko prices
with 10+ orders per side; an emulated market maker on every market), plus
two more traders with realistic balances:

  dave  bonds 0.5 DCR (1 tier) with a short lock: trades, hits the tier
        limit, then the bond expires (orders dropped) and is refunded
  erin  first unbonded, then under-bonded (5,000 LBC and 5,000 sats, below
        one tier, short locks, refunded) and an unbroadcast DCR bond

The S6 release (SIMNET DEFAULTS, the mainnet sizes are an open decision):
2 s epochs; trading bonds required; one tier = 0.0001 BTC, 10,000 LBC or
0.5 DCR (about USD 5-8); a bond counts 60 s after its admitting record
(maturity) and stops counting with less than 60 s of lock left; at least
300 s of lock at admission; 1 confirmation; per tier 5 lots per order and
10 resting orders.

Phases (default all, in order):
  wallets   dave, erin: SPV wallets, realistic funding
  switch    sign the S6 release, restart every node with it
  unbonded  nobody is bonded: alice's order refused locally; erin's orders
            sent anyway are refused by every validator, in no record, on no
            book
  bonds     alice BTC, bob LBC, carol DCR, mm BTC+LBC, dave DCR; erin's
            under-sized LBC and BTC bonds and unbroadcast DCR bond. Every
            peer: admitted, tier 0 before maturity (orders refused), tiers
            after; erin never counts
  book      seed books (10+ orders per side), start market makers
  trade     dave (tier 1): 6 lots refused (locally and, sent anyway, by
            validators); dave dcr_btc and bob lbc_dcr epoch matches swap to
            completion; market makers trade
  agree     every peer: same bonded users and tiers, same chain
  expiry    dave's bond expires: his resting order dropped on every peer,
            tier 0, new orders refused
  refund    refund before the lock refused; after it DCR (dave), LBC and BTC
            (erin) refunds confirmed
  restart   every node restarted: same tiers on every peer
Usage: ./s6.py [phase ...]
"""
import os, sys, time, json, shutil, signal
HERE = os.path.dirname(os.path.abspath(__file__))
os.environ.setdefault("EVID", "/workspace/tatanka-dev/evidence/s6-user-bonds")
os.environ.setdefault("BISONW", "/workspace/tatanka-dev/bin/bisonw-bisonary-s6")
os.environ.setdefault("RUN", "/workspace/tatanka-dev/run/s012")
sys.path.insert(0, HERE)
import harness as h
from harness import log, Fail
import s012 as b
import s3

EVID = b.EVID
EPOCH_MS = 2000
AGE_S, LOCKLEFT_S, MINLOCK_S = 60, 60, 300
SHORT_LOCK = 600
LONG_LOCK = 3600
TIER = {0: 10_000, 140: 10_000 * 10**8, 42: 50_000_000}
S6_FLAGS = ["-epochms", "2000", "-windowms", "1000", "-proposems", "200", "-fallbackms", "300", "-omissionms", "500",
            "-activewindow", "30", "-activelag", "5", "-activepct", "80", "-activemin", "10",
            "-s6", "-ubondbtc", str(TIER[0]), "-ubondlbc", str(TIER[140]), "-ubonddcr", str(TIER[42]),
            "-ubondage", str(AGE_S), "-ubondminlock", str(MINLOCK_S), "-ubondlockleft", str(LOCKLEFT_S), "-ubondconfs", "1",
            "-ubondlots", "5", "-ubondorders", "10", "-ubondmaxtier", "100", "-ubondmaxclaims", "8"]
EXTRA = [("dave", 1, 1_000_000), ("erin", 2, 2_000_000)]
# name -> [(asset, atoms, lockSecs, fake)]
BONDS = {"alice": [(0, 50_000, LONG_LOCK, False)],                         # 0.0005 BTC: 5 tiers
         "bob": [(140, 50_000 * 10**8, LONG_LOCK, False)],                 # 50,000 LBC: 5 tiers
         "carol": [(42, 250_000_000, LONG_LOCK, False)],                   # 2.5 DCR: 5 tiers
         "mm": [(0, 50_000, SHORT_LOCK, False), (140, 50_000 * 10**8, LONG_LOCK, False)],  # 10 tiers, 5 after the BTC bond expires
         "dave": [(42, 50_000_000, SHORT_LOCK, False)],                    # 0.5 DCR: 1 tier
         "erin": [(140, 5_000 * 10**8, SHORT_LOCK, False),                 # below one tier
                  (42, 50_000_000, LONG_LOCK, True)]}                      # never broadcast
# erin also tries 5,000 sats of BTC (half a tier): the BTC wallet refuses it as
# dust at the simnet fee rate (100 sat/vB), so a sub-tier BTC bond can't exist.
ERIN_DUST = (0, 5_000, SHORT_LOCK)
WANT_TIERS = {"alice": 5, "bob": 5, "carol": 5, "mm": 10, "dave": 1, "erin": 0}
TL = []

def st(bw):
    return bw.epochs()

def ub(bw, epoch=0, asof=0):
    return bw.dbgapi("/mesh/epochs/userbonds?epoch=%d&asof=%d" % (epoch, asof))

def running(net):
    return [n for n in net.nodes.values() if n.proc and n.proc.poll() is None and n.name not in ("tamper", "v2-twin")]

def peers(net):
    return [n for n in running(net) if n.name in [x.name for x in net.vals + net.traders]]

def tiers_of(bs, net):
    out = {}
    for u in bs["users"] or []:
        out[net.name_of.get(u["user"], u["user"][-8:])] = u["tier"]
    return out

def snap(net, tag):
    row = {"t": time.strftime("%T"), "tag": tag}
    for n in peers(net):
        try:
            s, bs = st(n), ub(n)
        except Exception as e:
            row[n.name] = "error %s" % e
            continue
        row[n.name] = {"last": s.get("lastFinalized"), "role": s["role"], "required": bs["required"], "tiers": tiers_of(bs, net),
                       "admitted": len(bs["admitted"] or []), "self": bs["self"] and bs["self"]["tier"], "ignored": bs["ignoredOrders"],
                       "resting": s["resting"]}
    TL.append(row)
    json.dump(TL, open(os.path.join(EVID, "timeline.json"), "w"), indent=1)
    a = row.get("alice")
    if isinstance(a, dict):
        log("%s: last %s tiers %s admitted %s" % (tag, a["last"], a["tiers"], a["admitted"]))
    return row

def every_peer(row, fn):
    return {k: v for k, v in row.items() if isinstance(v, dict) and not fn(v)}

def agreed_status(net, nodes=None):
    """UserBondsStatus at the same (epoch, asOf) on every peer; must be identical."""
    nodes = nodes or peers(net)
    asof = min(s3.finalized(n) for n in nodes)
    out, ref = {}, None
    for n in nodes:
        bs = ub(n, asof + 1, asof)
        key = json.dumps({"users": [(u["user"], u["tier"]) for u in bs["users"]],
                          "admitted": [(x["key"], x["user"], x["admittedEpoch"], x["matureEpoch"], x["expiryEpoch"], x["counts"]) for x in bs["admitted"]]},
                         sort_keys=True)
        out[n.name] = key
        ref = ref or key
    diff = [k for k, v in out.items() if v != ref]
    return asof, json.loads(ref), diff

def start_extra(net, n):
    n.start(["--meshrelease", net.release] if net.release else [])
    if n.pw:
        n.login()
    else:
        n.init()

def restart_all(net, nodes):
    for m in net.mms:
        m.stop()
    net.mms = []
    for n in net.nodes.values():
        n.stop()
    time.sleep(2)
    for n in nodes:
        if n in net.extra:
            start_extra(net, n)
        else:
            net.start(n)
    for n in nodes:
        h.wait_until(lambda: b.epoch_ok(n), 240, "%s finalizing" % n.name, 2)

def order(bw, mkt, sell, lots, rate, lifetime=600, unchecked=False):
    lot = h.LOTS[mkt]
    return bw.dbgapi("/mesh/order", {"market": mkt, "sell": sell, "qty": lots * lot, "lotSize": lot, "rate": rate,
                                      "lifetimeSec": lifetime, "unchecked": unchecked})

def order_status(bw, oid):
    return bw.dbgapi("/mesh/epochs/order?id=" + oid)["status"]

def owner_orders_in_records(net, bw, frm, owner):
    """Orders of owner in finalized proposals after epoch frm (all pages)."""
    n = total = 0
    while True:
        recs = bw.dbgapi("/mesh/epochs/records?from=%d&limit=500&full=1" % frm)["records"] or []
        if not recs:
            return n, total
        for r in recs:
            for o in (r.get("proposal") or {}).get("orders") or []:
                if o.get("owner") == owner:
                    n += 1
        total += len(recs)
        frm = recs[-1]["epoch"]

# ---------------- phases ----------------
def phase_wallets(net):
    for n in net.extra:
        for a in ("lbc", "btc", "dcr"):
            n.new_wallet(a, "SPV")
    h.ensure_lbc_supply(sum(l for _, _, l in EXTRA) + 1000)
    funded, bal = {}, {}
    for name, _, lbc in EXTRA:
        n = net[name]
        have = {a: n.coins(a, "total") for a in ("lbc", "dcr", "btc")}
        need = {"lbc": max(0, int(lbc - have["lbc"])) if have["lbc"] < lbc * 0.98 else 0,
                "dcr": round(h.REALISTIC_DCR - have["dcr"], 4) if have["dcr"] < 194 else 0,
                "btc": round(h.REALISTIC_BTC - have["btc"], 6) if have["btc"] < 0.344 else 0}
        h.fund_realistic(n, need["lbc"], dcr=need["dcr"], btc=need["btc"])
        funded[name] = {"target": {"lbc": lbc, "dcr": h.REALISTIC_DCR, "btc": h.REALISTIC_BTC}, "sent": need}
    for name, _, lbc in EXTRA:
        bal[name] = h.wait_funded(net[name], {"lbc": lbc * 0.97, "dcr": 194, "btc": 0.344}, 1200)
    for t in net.traders:
        bal[t.name] = {a: round(t.coins(a), 4) for a in ("lbc", "dcr", "btc")}
    vw = {}
    for v in net.vals:
        for a in ("lbc", "btc", "dcr"):
            v.new_wallet(a, "SPV")  # no-op if present (S4)
        vw[v.name] = "SPV btc/lbc/dcr (bond checks)"
    b.result("wallets", {"extraTraders": funded, "balances": bal, "validatorWallets": vw})

def phase_switch(net):
    ids = [v.peer_id for v in net.vals]
    path = b.sign_release(os.path.join(b.RUN, "s6-simnet-2s-userbonds.json"), ids, [v.name for v in net.vals], S6_FLAGS)
    shutil.copy(path, EVID)
    net.release = path
    log(h.run([b.RELTOOL, "verify", path]).splitlines()[0])
    restart_all(net, net.vals + net.traders)
    time.sleep(6)
    row = snap(net, "switch: S6 release")
    rel = json.load(open(path))
    relc = rel["release"] if isinstance(rel["release"], dict) else json.loads(rel["release"])
    bad = every_peer(row, lambda v: v["required"] and not v["tiers"] and v["admitted"] == 0)
    b.result("switch", {"release": os.path.basename(path), "userBondRules": relc.get("userBonds"),
                        "roles": {k: v["role"] for k, v in row.items() if isinstance(v, dict)}, "simnetDefaults": True})
    if bad:
        raise Fail("switch: %s" % bad)

def phase_unbonded(net):
    al, er = net["alice"], net["erin"]
    for n in net.traders:
        for m in b.MARKETS:
            n.subscribe(m)
    rate = h.rate_for(net.prices, "dcr_btc")
    r = order(al, "dcr_btc", True, 1, rate)
    local = r.get("error") or r
    if "id" in r:
        raise Fail("unbonded alice's order accepted: %s" % r)
    ids = []
    for i in range(3):
        r = order(er, "dcr_btc", i % 2 == 0, 1, rate + (50 if i % 2 == 0 else -50), unchecked=True)
        if "id" not in r:
            raise Fail("erin unchecked order: %s" % r)
        ids.append(r["id"])
    e0 = s3.finalized(al)
    time.sleep(16)  # 8 epochs
    stat = {v.name: [order_status(v, o) for o in ids] for v in net.vals}
    books = {n.name: sum(1 for o in n.book("dcr_btc") if o["id"] in ids) for n in (net["alice"], net["bob"], net["carol"], net["mm"])}
    inrec, nrec = owner_orders_in_records(net, net["v1"], e0 - 5, er.peer_id)
    res = {"aliceLocal": local, "erinOrders": ids, "validatorStatus": stat, "onOtherBooks": books,
           "erinOrdersInRecords": inrec, "recordsChecked": nrec}
    b.result("unbonded", res)
    if any(s != "refused: no counted trading bond" for l in stat.values() for s in l) or any(books.values()) or inrec:
        raise Fail("unbonded: %s" % res)

def post_bond(n, asset, amt, lock, fake):
    r = n.dbgapi("/mesh/epochs/userbond", {"assetID": asset, "amount": amt, "lockSecs": lock, "fake": fake})
    if "key" not in r:
        raise Fail("%s bond: %s" % (n.name, r))
    log("%s posted %s%d atoms of %d (lock %d s): %s" % (n.name, "FAKE " if fake else "", amt, asset, lock, r["key"]))
    return r["key"]

def phase_bonds(net):
    net.bond_keys, net.bond_posted = {}, {}
    t0 = time.time()
    for name, bl in BONDS.items():
        for (a, amt, lock, fake) in bl:
            k = post_bond(net[name], a, amt, lock, fake)
            net.bond_keys.setdefault(name, []).append((k, a, amt, lock, fake))
            net.bond_posted[k] = time.time()
    a, amt, lock = ERIN_DUST
    r = net["erin"].dbgapi("/mesh/epochs/userbond", {"assetID": a, "amount": amt, "lockSecs": lock, "fake": False})
    net.erin_dust = r.get("error") or r
    log("erin %d sats BTC bond: %s" % (amt, net.erin_dust))
    if "key" in r:
        net.bond_keys["erin"].append((r["key"], a, amt, lock, False))
        net.bond_posted[r["key"]] = time.time()
    real = {k for bl in net.bond_keys.values() for (k, a, amt, lock, fake) in bl if not fake and amt >= TIER[a]}
    bad_keys = {k for bl in net.bond_keys.values() for (k, a, amt, lock, fake) in bl if fake or amt < TIER[a]}
    def admitted():
        bs = ub(net["alice"])
        keys = {x["key"]: x for x in bs["admitted"]}
        return real <= set(keys) and keys
    adm = h.wait_until(admitted, 180, "%d trading bonds admitted" % len(real), 2)
    t_adm = time.time() - t0
    # Immature: tier 0 everywhere and alice's order refused.
    r = order(net["alice"], "dcr_btc", True, 1, h.rate_for(net.prices, "dcr_btc"))
    immature_order = r.get("error") or r
    last_mature = max(x["matureEpoch"] for x in adm.values())
    seen_imm, seen_mat = set(), set()
    hard_end = time.time() + 600
    def check_all():
        for n in peers(net):
            bs = ub(n)
            for x in bs["admitted"]:
                if x["key"] in bad_keys:
                    raise Fail("%s: bad bond %s admitted" % (n.name, x["key"]))
                counts = x["matureEpoch"] <= bs["epoch"] < x["expiryEpoch"]
                if x["counts"] != counts:
                    raise Fail("%s: bond %s counts %s at epoch %d (mature %d)" % (n.name, x["key"], x["counts"], bs["epoch"], x["matureEpoch"]))
                (seen_mat if counts else seen_imm).add(x["key"])
            t = tiers_of(bs, net)
            if t.get("erin"):
                raise Fail("erin has tier %s on %s" % (t["erin"], n.name))
    while time.time() < hard_end:
        row = snap(net, "bonds: aging")
        check_all()
        cur = st(net["alice"])["currentEpoch"]
        if all(v["tiers"] == {k: w for k, w in WANT_TIERS.items() if k != "erin"} for v in row.values() if isinstance(v, dict)):
            break
        if cur > last_mature + 30:
            break
        time.sleep(4)
    check_all()
    row = snap(net, "bonds: mature")
    asof, agreed, diff = agreed_status(net)
    bs = ub(net["v1"])
    pend = {x["name"] or x["user"][-8:]: (x["key"], x["err"]) for x in bs["pending"]}
    per = {}
    for x in ub(net["alice"])["admitted"]:
        per[net.name_of.get(x["user"], x["user"])+":"+x["key"][:10]] = {"asset": x["assetID"], "amount": x["amount"], "tiers": x["tiers"],
            "admittedEpoch": x["admittedEpoch"], "matureEpoch": x["matureEpoch"], "expiryEpoch": x["expiryEpoch"],
            "ageS": (x["matureEpoch"] - x["admittedEpoch"]) * EPOCH_MS / 1000, "lockTime": x["lockTime"], "counts": x["counts"]}
    res = {"secondsToAdmission": round(t_adm), "bonds": per, "seenImmature": len(seen_imm), "seenMature": len(seen_mat),
           "orderWhileImmature": immature_order, "tiersEveryPeer": {k: v["tiers"] for k, v in row.items() if isinstance(v, dict)},
           "agreedAsOf": asof, "peersDisagree": diff, "erinPendingOnV1": pend,
           "erinBtcHalfTier": net.erin_dust}
    b.result("bonds", res)
    b.shot(net["alice"], "/mesh", "s6-bonded")
    want = {k: v for k, v in WANT_TIERS.items() if k != "erin"}
    bad = every_peer(row, lambda v: v["tiers"] == want)
    if bad or diff or (isinstance(immature_order, dict) and "id" in immature_order):
        raise Fail("bonds: tiers %s diff %s immature %s" % (bad, diff, immature_order))
    if len(seen_mat) != len(real) or not seen_imm:
        raise Fail("bonds: mature %d/%d immature %d" % (len(seen_mat), len(real), len(seen_imm)))

def phase_book(net):
    s3.phase_book(net)

def epoch_trade(net, mkt, maker, taker, lots=1):
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

def phase_trade(net):
    dv = net["dave"]
    out = {}
    rate = h.rate_for(net.prices, "dcr_btc")
    r = order(dv, "dcr_btc", True, 6, rate * 2)
    out["dave6LotsLocal"] = r.get("error") or r
    if "id" in r:
        raise Fail("6-lot order with tier 1 accepted locally")
    r = order(dv, "dcr_btc", True, 6, rate * 2, unchecked=True)
    big = r["id"]
    time.sleep(10)
    out["dave6LotsValidators"] = {v.name: order_status(v, big) for v in net.vals}
    if any(s != "refused: order larger than the trading bond tier allows" for s in out["dave6LotsValidators"].values()):
        raise Fail("6-lot order: %s" % out)
    mm_before = len(net["mm"].matches())
    for mkt, mk, tk in (("dcr_btc", dv, net["alice"]), ("lbc_dcr", net["bob"], net["carol"])):
        bq = h.market_assets(mkt)
        before = {n.name: {a: n.coins(a, "total") for a in bq} for n in (mk, tk)}
        m, oid, rate, took = epoch_trade(net, mkt, mk, tk)
        mid = m["match"]["id"]
        epoch = m["match"]["stamp"] // EPOCH_MS - 1
        rec = b.records(tk, epoch - 1).get(epoch)
        h.wait_until(lambda: (mk.swap(mid) or {}).get("status") == "complete", 900, "%s maker swap complete" % mkt, 3)
        h.wait_until(lambda: (tk.swap(mid) or {}).get("status") == "complete", 600, "%s taker swap complete" % mkt, 3)
        time.sleep(6)
        after = {n.name: {a: n.coins(a, "total") for a in bq} for n in (mk, tk)}
        sw = mk.swap(mid)
        json.dump(sw, open(os.path.join(EVID, "swap-%s-maker.json" % mkt), "w"), indent=2)
        out[mkt] = {"match": mid, "maker": mk.name, "taker": tk.name, "rate": rate, "epoch": epoch, "agreedSecondsAfterTake": round(took, 1),
                    "recordSigners": rec and len(rec["signers"]),
                    "txs": {k: sw[k]["txid"] for k in ("makerSwap", "takerSwap", "makerRedeem", "takerRedeem") if sw.get(k)},
                    "status": {"maker": sw["status"], "taker": tk.swap(mid)["status"]},
                    "balanceDelta": {n: {a: round(after[n][a] - before[n][a], 8) for a in bq} for n in after}}
        b.result("trade", out)
    out["mmMatchesDuringPhase"] = len(net["mm"].matches()) - mm_before
    out["mmMatchesTotal"] = len(net["mm"].matches())
    b.result("trade", out)
    b.shot(net["alice"], "/mesh", "s6-trade")

def phase_agree(net):
    time.sleep(4)
    nodes = peers(net)
    asof, agreed, diff = agreed_status(net, nodes)
    chain = b.compare_chains(nodes, asof - 300)
    er = net["erin"].peer_id
    inrec, nrec = owner_orders_in_records(net, net["v2"], 0, er)
    res = {"asOf": asof, "peers": [n.name for n in nodes], "disagree": diff,
           "tiers": {net.name_of.get(u, u[-8:]): t for u, t in agreed["users"]}, "admitted": len(agreed["admitted"]),
           "chain": chain, "erinOrdersInRecords": inrec, "recordsScanned": nrec}
    json.dump(agreed, open(os.path.join(EVID, "agreed-userbonds.json"), "w"), indent=1)
    b.result("agree", res)
    if diff or inrec:
        raise Fail("agree: %s" % res)

def phase_expiry(net):
    dv = net["dave"]
    bs = ub(dv)
    mine = [x for x in bs["admitted"] if x["own"]]
    if not mine:
        raise Fail("dave has no admitted bond")
    exp = max(x["expiryEpoch"] for x in mine)
    bb, ba = b.best_rates(dv, "dcr_btc")
    oid = dv.order("dcr_btc", True, 1, max(ba, bb) * 3 or 10**7, 1800)  # far above the market: rests
    h.wait_until(lambda: all(order_status(n, oid) == "resting" for n in peers(net)), 60, "dave's order resting everywhere", 2)
    cur = st(dv)["currentEpoch"]
    log("dave's bond expires at epoch %d (now %d, %.0f s)" % (exp, cur, (exp - cur) * EPOCH_MS / 1000))
    if exp - cur > 600:
        raise Fail("expiry too far: %d epochs" % (exp - cur))
    h.wait_until(lambda: st(net["alice"])["lastFinalized"] >= exp + 2, (exp - cur) * EPOCH_MS / 1000 + 300, "epoch %d finalized" % exp, 4)
    time.sleep(4)
    stat = {n.name: order_status(n, oid) for n in peers(net)}
    row = snap(net, "expiry")
    onbook = {n.name: sum(1 for o in n.book("dcr_btc") if o["id"] == oid) for n in (net["alice"], net["bob"], net["mm"])}
    r = order(dv, "dcr_btc", True, 1, h.rate_for(net.prices, "dcr_btc"))
    res = {"expiryEpoch": exp, "orderStatusEveryPeer": stat, "onBooks": onbook, "daveTierEveryPeer": {k: v["tiers"].get("dave", 0) for k, v in row.items() if isinstance(v, dict)},
           "newOrderAfterExpiry": r.get("error") or r,
           "mmTierEveryPeer": {k: v["tiers"].get("mm", 0) for k, v in row.items() if isinstance(v, dict)}}
    b.result("expiry", res)
    if any(s == "resting" for s in stat.values()) or any(onbook.values()) or any(res["daveTierEveryPeer"].values()) or "id" in r \
            or any(t != 5 for t in res["mmTierEveryPeer"].values()):
        raise Fail("expiry: %s" % res)

def bond_unspent(key):
    """True if the bond output is unspent on the chain node (mempool included)."""
    import subprocess
    a, c = key.split(":")
    txid, vout = bytes.fromhex(c[:64])[::-1].hex(), int.from_bytes(bytes.fromhex(c[64:72]), "big")
    if a == "0":
        cmd = [os.path.join(h.BIN, "bitcoin-cli"), "-rpcport=20556", "-regtest=1", "-rpcuser=user", "-rpcpassword=pass", "gettxout", txid, str(vout), "true"]
    elif a == "42":
        cmd = [os.path.join(h.BIN, "dcrctl"), "-C", os.path.expanduser("~/dextest/dcr/alpha/alpha-ctl.conf"), "gettxout", txid, str(vout), "0", "true"]
    else:
        cmd = [os.path.expanduser("~/dextest/lbc/harness-ctl/alpha"), "gettxout", json.dumps([txid, vout, True])]
    out = subprocess.run(cmd, capture_output=True, text=True, timeout=30).stdout
    return '"value"' in out

def phase_refund(net):
    out = {}
    dv, er = net["dave"], net["erin"]
    targets = [(dv, k, a) for (k, a, amt, lock, fake) in net.bond_keys["dave"]] + \
              [(er, k, a) for (k, a, amt, lock, fake) in net.bond_keys["erin"] if not fake] + \
              [(net["mm"], k, a) for (k, a, amt, lock, fake) in net.bond_keys["mm"] if lock == SHORT_LOCK]
    # Before the lock: the chain refuses the refund.
    early = dv.dbgapi("/mesh/epochs/userbond/refund", {"key": targets[0][1]})
    out["beforeLock"] = early.get("error") or early
    out["beforeLockBondUnspent"] = bond_unspent(targets[0][1])
    if "refund" in early or not out["beforeLockBondUnspent"]:
        raise Fail("refund before lock accepted: %s" % early)
    lock = max(net.bond_posted[k] for _, k, _ in targets) + SHORT_LOCK
    log("waiting %.0f s for the short locks" % max(0, lock - time.time()))
    done = {}
    end = time.time() + max(0, lock - time.time()) + 600
    while time.time() < end and len(done) < len(targets):
        for n, k, a in targets:
            if k in done:
                continue
            if time.time() < net.bond_posted[k] + SHORT_LOCK:
                continue
            bal0 = n.coins({0: "btc", 140: "lbc", 42: "dcr"}[a], "total")
            r = n.dbgapi("/mesh/epochs/userbond/refund", {"key": k})
            if "refund" in r:
                done[k] = {"who": n.name, "asset": a, "refund": r["refund"], "secondsAfterLock": round(time.time() - net.bond_posted[k] - SHORT_LOCK), "balBefore": bal0}
                log("%s refunded %s: %s" % (n.name, k, r["refund"]))
            else:
                out.setdefault("retries", {}).setdefault(k, (r.get("error") or str(r))[:160])
        time.sleep(8)
    h.wait_until(lambda: not any(bond_unspent(k) for k in done), 180, "refunds spend the bonds on chain", 5)
    time.sleep(10)
    for k, d in done.items():
        d["bondSpentOnChain"] = not bond_unspent(k)
        n = net[d["who"]]
        d["balAfter"] = n.coins({0: "btc", 140: "lbc", 42: "dcr"}[d["asset"]], "total")
    out["refunds"] = done
    b.result("refund", out)
    if len(done) != len(targets) or not all(d["bondSpentOnChain"] for d in done.values()):
        raise Fail("refunds: %d of %d" % (len(done), len(targets)))

def phase_restart(net):
    asof0, before, _ = agreed_status(net)
    restart_all(net, net.vals + net.traders)
    time.sleep(8)
    asof, after, diff = agreed_status(net)
    row = snap(net, "restart")
    res = {"tiersBefore": before["users"], "tiersAfter": after["users"], "disagree": diff, "admittedBefore": len(before["admitted"]),
           "admittedAfter": len(after["admitted"]), "asOf": asof}
    b.result("restart", res)
    if diff or len(after["admitted"]) != len(before["admitted"]):
        raise Fail("restart: %s" % res)

PHASES = [("wallets", phase_wallets), ("switch", phase_switch), ("unbonded", phase_unbonded), ("bonds", phase_bonds),
          ("book", phase_book), ("trade", phase_trade), ("agree", phase_agree), ("expiry", phase_expiry),
          ("refund", phase_refund), ("restart", phase_restart)]

def main():
    signal.signal(signal.SIGTERM, lambda *a: sys.exit(143))
    want = sys.argv[1:] or [p for p, _ in PHASES]
    os.makedirs(EVID, exist_ok=True)
    net = b.Net()
    net.extra = []
    for i, (name, mn, _) in enumerate(EXTRA):
        n = h.Bisonw(name, b.RUN, "127.0.0.1:%d" % (16141 + i), "127.0.0.1:%d" % (16241 + i), net.addr[mn], b.BISONW, 16341 + i)
        net.nodes[name] = n
        net.extra.append(n)
    ok = True
    try:
        b.setup(net)
        for n in net.extra:
            start_extra(net, n)
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
                if name in ("wallets", "switch", "bonds"):
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
