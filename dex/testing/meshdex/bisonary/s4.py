#!/usr/bin/env python3
"""SIMNET/REGTEST ONLY. S4 (validator bonds behind the dormant one-way switch)
end-to-end test on the box.

Same network as s012.py/s3.py (private 3-node mesh, seeded validators v1-v4,
traders alice, bob, carol and mm with realistic balances: 1-5 M LBC, ~200
DCR, ~0.355 BTC; books at real CoinGecko prices with 10+ orders per side; an
emulated market maker on every market). The seeded validators get SPV
wallets for BTC, LBC and DCR (unfunded) to check bonds on chain. Newcomers:

  nb-btc   bonds 0.001 BTC        (funded 1.0 M LBC, 200 DCR, 0.355 BTC)
  nb-lbc   bonds 1,000,000 LBC    (funded 2.5 M LBC, 200 DCR, 0.355 BTC)
  nb-dcr   bonds 10 DCR, and one valid-looking DCR bond that is never
           broadcast              (funded 1.5 M LBC, 200 DCR, 0.355 BTC)
  nb-none  validator mode, no bond, no wallets

The S4 release: 2 s epochs; S3 window 30 epochs ending 5 back, 80 %, judged
from 10; bonds: BTC 0.0001, LBC 1,000,000, DCR 10 (placeholder), age 180 s
("minutes on simnet" for the 4 weeks), lock >= 900 s at admission, counts
while >= 120 s are left, 1 confirmation, switch at 3/4 of the 4 seeded.

Phases (default all, in order):
  switch   sign the S4 release, restart every node with it; bonds dormant
  book     seed books (10+ orders per side), start market makers
  wallets  validator SPV wallets; newcomers funded with realistic balances
  dormant  nb-btc bonds while the switch is off: not admitted, not counted
  signal2  v1, v2 signal: 2 of 4 -> still off on every peer
  signal3  v3 signals: 3 of 4 -> on, same switch epoch on every peer
  bonds    LBC and DCR bonds (+ the BTC one): admitted, not counted before
           the 180 s age, counted after it on every peer; the unbroadcast
           bond and nb-none never count
  active   newcomers sign, join the active set; all peers agree (replay)
  quorum   v3, v4 down: v1, v2 + 3 bonded finalize; an epoch match swaps
  restart  every node restarted: the switch is still on (chain + latch)
Usage: ./s4.py [phase ...]
"""
import os, sys, time, json, shutil, signal
HERE = os.path.dirname(os.path.abspath(__file__))
os.environ.setdefault("EVID", "/workspace/tatanka-dev/evidence/s4-bonds")
os.environ.setdefault("BISONW", "/workspace/tatanka-dev/bin/bisonw-bisonary-s4")
os.environ.setdefault("RUN", "/workspace/tatanka-dev/run/s012")
sys.path.insert(0, HERE)
import harness as h
from harness import log, Fail
import s012 as b
import s3

EVID = b.EVID
EPOCH_MS = 2000
AGE_S = 180
LOCK_S = 1800
S4_FLAGS = ["-epochms", "2000", "-windowms", "1000", "-proposems", "200", "-fallbackms", "300", "-omissionms", "500",
            "-activewindow", "30", "-activelag", "5", "-activepct", "80", "-activemin", "10",
            "-s4", "-bondbtc", "0:10000", "-bondlbc", "0:100000000000000,31536000:50000000000000", "-bonddcr", "0:1000000000",
            "-bondage", str(AGE_S), "-bondminlock", "900", "-bondlockleft", "120", "-bondconfs", "1", "-bondswitch", "3/4",
            "-bondmaxclaims", "8"]
NEWBIES = [("nb-btc", 1, 1_000_000), ("nb-lbc", 2, 2_500_000), ("nb-dcr", 3, 1_500_000), ("nb-none", 1, 0)]
BONDS = {"nb-btc": (0, 100_000), "nb-lbc": (140, 1_000_000 * 10**8), "nb-dcr": (42, 10 * 10**8)}
TL = []

def st(bw):
    return bw.epochs()

def bonds(bw, epoch=0, asof=0):
    return bw.dbgapi("/mesh/epochs/bonds?epoch=%d&asof=%d" % (epoch, asof))

def members(bw):
    return sorted(m["name"] for m in bonds(bw)["members"])

def running(net):
    return [n for n in net.nodes.values() if n.proc and n.proc.poll() is None and n.name not in ("tamper",)]

def snap(net, tag):
    row = {"t": time.strftime("%T"), "tag": tag}
    for n in running(net):
        try:
            bs = bonds(n)
            s = st(n)
        except Exception as e:
            row[n.name] = "error %s" % e
            continue
        sw = bs["switch"]
        row[n.name] = {"last": s.get("lastFinalized"), "role": s["role"], "switch": sw["on"], "onEpoch": sw.get("onEpoch"),
                       "signals": sw["signals"], "members": sorted(m["name"] for m in bs["members"]), "bonded": bs["bondedPower"],
                       "active": sorted(v["name"] for v in s["validators"] if v["active"]), "admitted": len(bs["admitted"] or [])}
    TL.append(row)
    json.dump(TL, open(os.path.join(EVID, "timeline.json"), "w"), indent=1)
    a = row.get("alice")
    if isinstance(a, dict):
        log("%s: last %s switch %s signals %s members %s active %s" % (tag, a["last"], a["switch"], a["signals"], a["members"], a["active"]))
    return row

def every_peer(row, fn):
    return {k: v for k, v in row.items() if isinstance(v, dict) and not fn(v)}

def start_newbie(net, n):
    n.start(["--meshrelease", net.release, "--meshvalidator"])
    if n.pw:
        n.login()
    else:
        n.init()

def name_all(net):
    for n in running(net):
        for nb in net.newbies:
            if getattr(nb, "peer_id", None):
                try:
                    n.dbgapi("/mesh/epochs/peername", {"peerID": nb.peer_id, "name": nb.name})
                except Exception:
                    pass

def restart_all(net, nodes):
    for n in net.nodes.values():
        n.stop()
    time.sleep(2)
    for n in nodes:
        if n in net.newbies:
            start_newbie(net, n)
        else:
            net.start(n)
    name_all(net)
    for n in nodes:
        h.wait_until(lambda: b.epoch_ok(n), 240, "%s finalizing" % n.name, 2)

# ---------------- phases ----------------
def phase_switch(net):
    ids = [v.peer_id for v in net.vals]
    path = b.sign_release(os.path.join(b.RUN, "s4-simnet-2s-bonds.json"), ids, [v.name for v in net.vals], S4_FLAGS)
    shutil.copy(path, EVID)
    net.release = path
    log(h.run([b.RELTOOL, "verify", path]).splitlines()[0])
    for m in net.mms:
        m.stop()
    net.mms = []
    restart_all(net, net.vals + net.traders + net.newbies)
    time.sleep(6)
    row = snap(net, "switch: S4 release")
    bad = every_peer(row, lambda v: not v["switch"] and v["members"] == ["v1", "v2", "v3", "v4"])
    rel = json.load(open(path))
    b.result("switch", {"release": os.path.basename(path), "bondRules": (rel["release"] if isinstance(rel["release"], dict) else json.loads(rel["release"])).get("bonds"),
                        "aliceBonds": bonds(net["alice"])["switch"], "roles": {k: v["role"] for k, v in row.items() if isinstance(v, dict)}})
    if bad:
        raise Fail("not dormant / wrong members: %s" % bad)

def phase_book(net):
    s3.phase_book(net)

def phase_wallets(net):
    for v in net.vals:
        for a in ("lbc", "btc", "dcr"):
            v.new_wallet(a, "SPV")
    funded = {}
    for nb in net.newbies:
        if nb.name == "nb-none":
            continue
        for a in ("lbc", "btc", "dcr"):
            nb.new_wallet(a, "SPV")
    h.ensure_lbc_supply(sum(l for _, _, l in NEWBIES) + 1000)
    for name, _, lbc in NEWBIES:
        if not lbc:
            continue
        nb = net[name]
        have = {a: nb.coins(a, "total") for a in ("lbc", "dcr", "btc")}
        need = {"lbc": max(0, int(lbc - have["lbc"])) if have["lbc"] < lbc * 0.98 else 0,
                "dcr": round(h.REALISTIC_DCR - have["dcr"], 4) if have["dcr"] < 194 else 0,
                "btc": round(h.REALISTIC_BTC - have["btc"], 6) if have["btc"] < 0.344 else 0}
        h.fund_realistic(nb, need["lbc"], dcr=need["dcr"], btc=need["btc"])
        funded[name] = {"target": {"lbc": lbc, "dcr": h.REALISTIC_DCR, "btc": h.REALISTIC_BTC}, "sent": need}
    bal = {}
    for name, _, lbc in NEWBIES:
        if lbc:
            bal[name] = h.wait_funded(net[name], {"lbc": lbc * 0.97, "dcr": 194, "btc": 0.344}, 1200)
    b.result("wallets", {"newcomers": funded, "balances": bal, "validatorWallets": "SPV btc/lbc/dcr on v1-v4 (unfunded, for bond checks)"})

def post_bond(nb, asset, amt, fake=False):
    r = nb.dbgapi("/mesh/epochs/bond", {"assetID": asset, "amount": amt, "lockSecs": LOCK_S, "fake": fake})
    if "key" not in r:
        raise Fail("%s bond: %s" % (nb.name, r))
    log("%s posted %s bond %s" % (nb.name, "FAKE" if fake else "", r["key"]))
    return r["key"]

def phase_dormant(net):
    nb = net["nb-btc"]
    a, amt = BONDS["nb-btc"]
    net.bond_keys = {"nb-btc": post_bond(nb, a, amt)}
    net.bond_posted = {"nb-btc": time.time()}
    e0 = st(net["alice"])["lastFinalized"]
    time.sleep(40)  # 20 epochs, 13 blocks
    row = snap(net, "dormant: BTC bond posted, switch off")
    bad = every_peer(row, lambda v: not v["switch"] and v["admitted"] == 0 and "nb-btc" not in v["members"])
    pend = bonds(net["v1"])["pending"]
    b.result("dormant", {"epochs": st(net["alice"])["lastFinalized"] - e0, "v1Pending": pend,
                         "everyPeer": {k: {x: v[x] for x in ("switch", "admitted", "members")} for k, v in row.items() if isinstance(v, dict)}})
    if bad:
        raise Fail("bond counted while dormant: %s" % bad)

def phase_signal2(net):
    for v in ("v1", "v2"):
        r = net[v].dbgapi("/mesh/epochs/bondswitch", {})
        if not r.get("ok"):
            raise Fail("%s signal: %s" % (v, r))
    r = net["nb-none"].dbgapi("/mesh/epochs/bondswitch", {})
    e0 = st(net["alice"])["lastFinalized"]
    time.sleep(40)
    row = snap(net, "signal2")
    bad = every_peer(row, lambda v: not v["switch"] and v["signals"] == ["v1", "v2"])
    b.result("signal2", {"signals": row["alice"]["signals"], "switchOnAnyPeer": any(v["switch"] for v in row.values() if isinstance(v, dict)),
                         "nonSeededSignal": r, "epochsWatched": st(net["alice"])["lastFinalized"] - e0, "peers": len([v for v in row.values() if isinstance(v, dict)])})
    if bad:
        raise Fail("2 of 4: %s" % bad)

def phase_signal3(net):
    e0 = st(net["alice"])["lastFinalized"]
    t0 = time.time()
    net["v3"].dbgapi("/mesh/epochs/bondswitch", {})
    def on():
        row = snap(net, "signal3: wait")
        return all(v["switch"] for v in row.values() if isinstance(v, dict)) and row
    row = h.wait_until(on, 120, "switch on on every peer", 3)
    eps = {k: v["onEpoch"] for k, v in row.items() if isinstance(v, dict)}
    latch = {}
    for v in net.vals:
        p = [f for f in os.listdir(os.path.join(v.dir, "simnet", "mesh")) if f.endswith(".bondswitch.json")] if os.path.isdir(os.path.join(v.dir, "simnet", "mesh")) else []
        latch[v.name] = p
    b.result("signal3", {"onEpoch": sorted(set(eps.values())), "onEveryPeer": eps, "secondsAfterThirdSignal": round(time.time() - t0),
                         "signals": row["alice"]["signals"], "latchFiles": latch})
    if len(set(eps.values())) != 1:
        raise Fail("peers disagree on the switch epoch: %s" % eps)
    b.shot(net["alice"], "/mesh", "s4-switch-on")

def phase_bonds(net):
    for name in ("nb-lbc", "nb-dcr"):
        a, amt = BONDS[name]
        net.bond_keys[name] = post_bond(net[name], a, amt)
        net.bond_posted[name] = time.time()
    net.fake_key = post_bond(net["nb-dcr"], 42, 10 * 10**8, fake=True)
    # Admission of all three real bonds.
    def admitted():
        bs = bonds(net["alice"])
        keys = {x["key"]: x for x in bs["admitted"] or []}
        return all(k in keys for k in net.bond_keys.values()) and keys
    adm = h.wait_until(admitted, 240, "3 bonds admitted", 2)
    # Before maturity: sample every peer; a bond counts only from its mature epoch.
    samples = []
    end = time.time() + AGE_S + 60
    seen_immature, seen_mature = set(), set()
    while time.time() < end:
        row = snap(net, "bonds: aging")
        cur = st(net["alice"])["currentEpoch"]
        for n in running(net):
            bs = bonds(n)
            mem = {m["name"] for m in bs["members"]}
            for x in bs["admitted"] or []:
                nm = x["name"]
                if x["key"] == net.fake_key:
                    raise Fail("unbroadcast bond admitted")
                counts = bs["epoch"] >= x["matureEpoch"]
                if x["counts"] != counts or (nm in mem) != counts:
                    raise Fail("%s: %s counts %s member %s at epoch %d, mature %d" % (n.name, nm, x["counts"], nm in mem, bs["epoch"], x["matureEpoch"]))
                (seen_mature if counts else seen_immature).add(nm)
            if "nb-none" in mem:
                raise Fail("unbonded nb-none is a member on %s" % n.name)
        samples.append({"epoch": cur, "aliceMembers": row["alice"]["members"]})
        if len(seen_mature) == 3 and all(len(v["members"]) == 7 for v in row.values() if isinstance(v, dict)):
            break
        time.sleep(8)
    row = snap(net, "bonds: mature")
    bs = bonds(net["alice"])
    per = {}
    for x in bs["admitted"]:
        posted = net.bond_posted.get(x["name"])
        per[x["name"]] = {"asset": x["assetID"], "amount": x["amount"], "admittedEpoch": x["admitted"], "matureEpoch": x["matureEpoch"],
                          "ageS": (x["matureEpoch"] - x["admitted"]) * EPOCH_MS / 1000, "lockTime": x["lockTime"], "counts": x["counts"]}
    pend = bonds(net["v1"])["pending"]
    fake = [p for p in pend or [] if p["key"] == net.fake_key]
    res = {"bonds": per, "seenImmature": sorted(seen_immature), "seenMature": sorted(seen_mature), "membersEveryPeer":
           {k: v["members"] for k, v in row.items() if isinstance(v, dict)}, "bondedPower": bs["bondedPower"],
           "fakeBond": fake, "nbNoneRole": st(net["nb-none"])["role"], "samples": len(samples)}
    b.result("bonds", res)
    b.shot(net["alice"], "/mesh", "s4-bonded-members")
    if len(seen_mature) != 3 or not seen_immature:
        raise Fail("bonds: mature %s immature %s" % (seen_mature, seen_immature))
    bad = every_peer(row, lambda v: v["members"] == ["nb-btc", "nb-dcr", "nb-lbc", "v1", "v2", "v3", "v4"] and v["bonded"] == 7)
    if bad:
        raise Fail("members: %s" % bad)

def phase_active(net):
    def all_active():
        row = snap(net, "active: wait")
        return all(len(v["active"]) == 7 for v in row.values() if isinstance(v, dict)) and row
    row = h.wait_until(all_active, 300, "7 active on every peer", 6)
    time.sleep(6)
    nodes = running(net)
    lasts = {n.name: s3.finalized(n) for n in nodes}
    asof = min(lasts.values())
    sets = {}
    for n in nodes:
        r = n.dbgapi("/mesh/epochs/active?epoch=%d&asof=%d" % (asof + 1, asof))
        m = bonds(n, asof + 1, asof)
        sets[n.name] = (tuple(r["active"]), json.dumps(sorted(x["peerID"] for x in m["members"])))
    if len(set(sets.values())) != 1:
        raise Fail("active/member sets differ: %s" % sets)
    replay = {}
    for n in nodes:
        rr = n.dbgapi("/mesh/epochs/activecheck?from=0")
        replay[n.name] = rr
        if rr["mismatches"]:
            raise Fail("replay mismatch on %s: %s" % (n.name, rr))
    chain = b.compare_chains(nodes, asof - 200)
    recs = b.records(net["alice"], asof - 20)
    nb_ids = {net[x].peer_id: x for x in ("nb-btc", "nb-lbc", "nb-dcr", "nb-none")}
    signed = {x: sum(1 for r in recs.values() if any(s == pid for s in r["signers"])) for pid, x in nb_ids.items()}
    b.result("active", {"activeEveryPeer": {k: v["active"] for k, v in row.items() if isinstance(v, dict)}, "asOf": asof,
                        "nodesAgree": len(nodes), "replay": replay, "chain": chain,
                        "newcomerSignaturesInLast20Records": signed, "records": len(recs)})
    b.shot(net["alice"], "/mesh", "s4-active-7")
    if signed["nb-none"] or min(signed[x] for x in ("nb-btc", "nb-lbc", "nb-dcr")) == 0:
        raise Fail("signatures: %s" % signed)

def phase_quorum(net):
    al = net["alice"]
    for v in ("v3", "v4"):
        net[v].stop()
    log("v3, v4 stopped: v1 + v2 + 3 bonded = 5 of 7")
    time.sleep(8)
    f0 = s3.finalized(al)
    m, oid, rate, took = s3.epoch_trade3(net, "dcr_btc", net["alice"], net["bob"])
    time.sleep(max(0, 24 - took))
    f2 = s3.finalized(al)
    recs = b.records(al, f0)
    nb_ids = {net[x].peer_id for x in ("nb-btc", "nb-lbc", "nb-dcr")}
    seeded = {net[x].peer_id for x in ("v1", "v2", "v3", "v4")}
    sig = sorted({(len(set(r["signers"]) & seeded), len(set(r["signers"]) & nb_ids)) for r in recs.values()})
    stq = st(al)
    b.shot(al, "/mesh", "s4-quorum-5of7")
    mid = m["match"]["id"]
    epoch = m["match"]["stamp"] // EPOCH_MS - 1
    rec = b.records(net["bob"], epoch - 1).get(epoch)
    for v in ("v3", "v4"):
        net.start(net[v])
    mk, tk = net["alice"], net["bob"]
    h.wait_until(lambda: (mk.swap(mid) or {}).get("status") == "complete", 900, "maker swap complete", 3)
    h.wait_until(lambda: (tk.swap(mid) or {}).get("status") == "complete", 600, "taker swap complete", 3)
    sw = mk.swap(mid)
    json.dump(sw, open(os.path.join(EVID, "swap-dcr_btc-5of7-maker.json"), "w"), indent=2)
    res = {"finalizedWhileV3V4Down": {"from": f0, "to": f2, "epochs": f2 - f0, "seededAndBondedSignersPerRecord": sig,
                                      "activePower": stq["activePower"], "bondedPower": stq["bondedPower"]},
           "match": {"id": mid, "epoch": epoch, "recordSigners": rec and len(rec["signers"]),
                     "bondedSigners": rec and len(set(rec["signers"]) & nb_ids), "agreedSecondsAfterTake": round(took, 1),
                     "txs": {k: sw[k]["txid"] for k in ("makerSwap", "takerSwap", "makerRedeem", "takerRedeem") if sw.get(k)},
                     "maker": sw["status"], "taker": tk.swap(mid)["status"]}}
    b.result("quorum", res)
    if f2 - f0 < 8 or not rec or len(set(rec["signers"]) & nb_ids) < 3:
        raise Fail("quorum: %s" % res)

def phase_restart(net):
    before = bonds(net["alice"])["switch"]
    nodes = net.vals + net.traders + net.newbies
    for m in net.mms:
        m.stop()
    restart_all(net, nodes)
    time.sleep(10)
    row = snap(net, "restart")
    bad = every_peer(row, lambda v: v["switch"] and v["onEpoch"] == before["onEpoch"] and len(v["members"]) == 7)
    lat = {v.name: bonds(v)["switch"]["latched"] for v in net.vals}
    b.result("restart", {"switchBefore": before, "afterEveryPeer": {k: {x: v[x] for x in ("switch", "onEpoch", "members", "last")} for k, v in row.items() if isinstance(v, dict)},
                         "latched": lat})
    b.shot(net["alice"], "/mesh", "s4-after-restart")
    if bad or not all(lat.values()):
        raise Fail("after restart: %s latched %s" % (bad, lat))

PHASES = [("switch", phase_switch), ("book", phase_book), ("wallets", phase_wallets), ("dormant", phase_dormant),
          ("signal2", phase_signal2), ("signal3", phase_signal3), ("bonds", phase_bonds), ("active", phase_active),
          ("quorum", phase_quorum), ("restart", phase_restart)]

def main():
    signal.signal(signal.SIGTERM, lambda *a: sys.exit(143))
    want = sys.argv[1:] or [p for p, _ in PHASES]
    os.makedirs(EVID, exist_ok=True)
    net = b.Net()
    net.newbies = []
    for i, (name, mn, _) in enumerate(NEWBIES):
        nb = h.Bisonw(name, b.RUN, "127.0.0.1:%d" % (16131 + i), "127.0.0.1:%d" % (16231 + i), net.addr[mn], b.BISONW, 16331 + i)
        net.nodes[name] = nb
        net.newbies.append(nb)
    ok = True
    try:
        b.setup(net)
        for nb in net.newbies:  # first start: learn peer IDs with the old release
            start_newbie(net, nb)
        for v in net.vals + net.traders + net.newbies:
            v.peer_id = getattr(v, "peer_id", None) or v.id()
        net.name_of = {n.peer_id: n.name for n in net.vals + net.newbies}
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
                if name in ("switch", "wallets", "signal3", "bonds"):
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
