#!/usr/bin/env python3
"""SIMNET/REGTEST ONLY. S5 (equivocation proof + ban) end-to-end test on the box.

Same network as s012.py/s3.py/s4.py (private 3-node mesh; traders alice,
bob, carol and the market maker mm with realistic balances: 1-5 M LBC,
~200 DCR, ~0.355 BTC; books at real CoinGecko prices with 10+ orders per
side; an emulated market maker on every market), with five seeded
validators v1-v5. No bond penalty exists in S5 (open decision); a ban only
removes the validator.

The S5 release: 2 s epochs; S3 window 30 epochs ending 5 back, 80 %,
judged from 10 counted records; no bond rules.

Phases (default all, in order):
  switch      sign the S5 release (v1-v5), restart every node with it
  book        seed books (10+ orders per side), start market makers
  warm        all 5 validators active on every peer
  equivocate  v5 signs two different results for one epoch: every peer
              holds the proof, bans v5 at the same record; v5 leaves the
              set and the active set; finalization never stops
  trade       after the ban: an epoch match swaps to completion (4 of 4
              signers), market makers keep trading
  forge       a forged proof against honest v1: gossiped by alice, then
              put into v3's proposals: rejected everywhere, v1 stays,
              v3's proposals never finalize, the next proposer takes over
  dupkey      v2's appdata started twice (twin on another mesh node): the
              twin sees v2's signatures in peers' records and never signs;
              v2 keeps signing; nobody holds a proof against v2
  restart     every node restarted: v5 still banned at the same record on
              every peer, v5 does not sign
  same        every peer's chain and active-set replay agree
Usage: ./s5.py [phase ...]
"""
import os, sys, time, json, shutil, signal
HERE = os.path.dirname(os.path.abspath(__file__))
os.environ.setdefault("EVID", "/workspace/tatanka-dev/evidence/s5-double-sign")
os.environ.setdefault("BISONW", "/workspace/tatanka-dev/bin/bisonw-bisonary-s5")
os.environ.setdefault("RUN", "/workspace/tatanka-dev/run/s012")
sys.path.insert(0, HERE)
import harness as h
from harness import log, Fail
import s012 as b
import s3

EVID = b.EVID
EPOCH_MS = 2000
S5_FLAGS = ["-epochms", "2000", "-windowms", "1000", "-proposems", "200", "-fallbackms", "300", "-omissionms", "500",
            "-activewindow", "30", "-activelag", "5", "-activepct", "80", "-activemin", "10"]
TL = []

def st(bw):
    return bw.epochs()

def bans(bw):
    return bw.dbgapi("/mesh/epochs/bans")

def running(net):
    return [n for n in net.nodes.values() if n.proc and n.proc.poll() is None and n.name not in ("tamper", "v2-twin")]

def finalized(bw):
    return st(bw).get("lastFinalized", 0)

def names(net, ids):
    return sorted(net.name_of.get(x, x[-8:]) for x in ids or [])

def snap(net, tag, nodes=None):
    row = {"t": time.strftime("%T"), "tag": tag}
    for n in nodes or running(net):
        try:
            s, bs = st(n), bans(n)
        except Exception as e:
            row[n.name] = "error %s" % e
            continue
        row[n.name] = {"last": s.get("lastFinalized"), "role": s["role"], "signing": s["signing"], "signHold": s.get("signHold"),
                       "members": sorted(v["name"] for v in s["validators"]),
                       "active": sorted(v["name"] for v in s["validators"] if v["active"]),
                       "banned": [(x["name"], x["banRecord"], x["proofEpoch"]) for x in bs["banned"] or []],
                       "held": [x["name"] for x in bs["held"] or []], "badProofs": bs["badProofs"],
                       "dupKey": bs["duplicateKey"], "dupMsgs": bs["duplicateKeyMsgs"], "guard": bs["signGuardEpoch"]}
    TL.append(row)
    json.dump(TL, open(os.path.join(EVID, "timeline.json"), "w"), indent=1)
    a = row.get("alice")
    if isinstance(a, dict):
        log("%s: last %s members %s active %s banned %s" % (tag, a["last"], a["members"], a["active"], a["banned"]))
    return row

def every_peer(row, fn):
    return {k: v for k, v in row.items() if isinstance(v, dict) and not fn(v)}

def chain_stats(bw, frm, to=None):
    """Finalized vs skipped epochs, signers and matches in (frm, to]."""
    recs = b.records(bw, frm)
    if to is None:
        to = max(recs) if recs else frm
    es = sorted(e for e in recs if frm < e <= to)
    return {"from": frm, "to": to, "epochs": to - frm, "finalized": len(es), "skipped": (to - frm) - len(es),
            "matches": sum(len(recs[e]["matches"] or []) for e in es),
            "signersPerRecord": sorted({len(recs[e]["signers"]) for e in es}), "recs": recs}

def ensure_v5(net):
    if "v5" not in net.nodes:
        net.nodes["v5"] = h.Bisonw("v5", b.RUN, "127.0.0.1:16109", "127.0.0.1:16209", net.addr[3], b.BISONW, 16309)
    v5 = net["v5"]
    if v5 not in net.vals:
        net.vals.append(v5)
    if not getattr(v5, "peer_id", None):
        net.start(v5)
        v5.peer_id = v5.id()
    net.name_of[v5.peer_id] = "v5"

# ---------------- phases ----------------
def phase_switch(net):
    ensure_v5(net)
    ids = [v.peer_id for v in net.vals]
    path = b.sign_release(os.path.join(b.RUN, "s5-simnet-2s-5val.json"), ids, [v.name for v in net.vals], S5_FLAGS)
    shutil.copy(path, EVID)
    net.release = path
    log(h.run([b.RELTOOL, "verify", path]).splitlines()[0])
    for m in net.mms:
        m.stop()
    net.mms = []
    s3.restart_all(net, net.vals + net.traders)
    time.sleep(6)
    row = snap(net, "switch: S5 release")
    bad = every_peer(row, lambda v: v["members"] == ["v1", "v2", "v3", "v4", "v5"] and not v["banned"])
    b.result("switch", {"release": os.path.basename(path), "validators": {v.name: v.peer_id for v in net.vals},
                        "roles": {k: v["role"] for k, v in row.items() if isinstance(v, dict)}})
    if bad:
        raise Fail("switch: %s" % bad)

def phase_book(net):
    s3.phase_book(net)

def phase_warm(net):
    def ready():
        row = snap(net, "warm: wait")
        ok = all(len(v["active"]) == 5 for v in row.values() if isinstance(v, dict))
        return ok and not st(net["alice"])["activeSet"]["fallback"] and row
    row = h.wait_until(ready, 300, "5 active on every peer (window judged)", 5)
    b.result("warm", {"activeEveryPeer": {k: v["active"] for k, v in row.items() if isinstance(v, dict)},
                      "activeSet": st(net["alice"])["activeSet"]})

def phase_equivocate(net):
    v5, al = net["v5"], net["alice"]
    vid = v5.peer_id
    e0 = finalized(al)
    t0 = time.time()
    v5.dbgapi("/mesh/epochs/equivocate", {"epochs": 1})
    log("v5 told to sign two results for its next epoch (alice at %d)" % e0)
    seen = {}
    def all_banned():
        for n in running(net):
            if n.name in seen:
                continue
            bs = bans(n)
            for x in bs["banned"] or []:
                if x["validator"] == vid:
                    seen[n.name] = {"banRecord": x["banRecord"], "proofEpoch": x["proofEpoch"], "bannedFrom": x["bannedFrom"],
                                    "proofID": x["proofID"], "afterS": round(time.time() - t0, 1)}
        return len(seen) == len(running(net))
    h.wait_until(all_banned, 120, "v5 banned on every peer", 0.5)
    took = time.time() - t0
    ban_recs = {v["banRecord"] for v in seen.values()}
    proof_ep = {v["proofEpoch"] for v in seen.values()}
    proof_ids = {v["proofID"] for v in seen.values()}
    time.sleep(30)
    row = snap(net, "equivocate: after ban")
    ban_rec = min(ban_recs)
    stats = chain_stats(al, e0)
    recs = stats.pop("recs")
    after = [e for e in recs if e > ban_rec]
    with_v5 = [e for e in after if vid in (recs[e]["activeSet"] or []) or vid in (recs[e]["signers"] or [])]
    proof_rec = recs.get(ban_rec)
    eq_flags = [f for f in st(al)["flags"] or [] if f["kind"] == "equivocation"]
    v5s = st(v5)
    b.shot(al, "/mesh", "s5-v5-banned")
    res = {"equivocationAfterEpoch": e0, "proofEpoch": sorted(proof_ep), "banRecord": sorted(ban_recs), "bannedFrom": ban_rec + 1,
           "epochsFromEquivocationToBan": ban_rec - min(proof_ep), "secondsUntilBannedOnEveryPeer": round(took, 1),
           "perPeer": seen, "sameProofEveryPeer": len(proof_ids) == 1,
           "proofRecordProposer": proof_rec and net.name_of.get(proof_rec["proposer"]),
           "afterBan": {"records": len(after), "recordsWithV5ActiveOrSigning": len(with_v5)},
           "finalization": stats, "membersEveryPeer": {k: v["members"] for k, v in row.items() if isinstance(v, dict)},
           "activeEveryPeer": {k: v["active"] for k, v in row.items() if isinstance(v, dict)},
           "v5Self": {"role": v5s["role"], "signing": v5s["signing"]}, "aliceEquivocationFlags": eq_flags[-3:]}
    b.result("equivocate", res)
    bad = every_peer(row, lambda v: v["members"] == ["v1", "v2", "v3", "v4"] and "v5" not in v["active"] and
                     [x[0] for x in v["banned"]] == ["v5"] and v["banned"][0][1] == ban_rec)
    if bad or len(ban_recs) != 1 or len(proof_ids) != 1 or with_v5 or v5s["role"] != "banned" or v5s["signing"]:
        raise Fail("equivocate: bad %s recs %s proofs %d withV5 %s v5 %s" % (bad, ban_recs, len(proof_ids), with_v5, v5s["role"]))
    if stats["skipped"] > max(3, stats["epochs"] // 10):
        raise Fail("finalization stalled: %s" % stats)
    net.ban_rec, net.ban_proof_ep = ban_rec, min(proof_ep)

def phase_trade(net):
    al, bo = net["alice"], net["bob"]
    f0 = finalized(al)
    m, oid, rate, took = s3.epoch_trade3(net, "dcr_btc", al, bo)
    mid = m["match"]["id"]
    epoch = m["match"]["stamp"] // EPOCH_MS - 1
    h.wait_until(lambda: (al.swap(mid) or {}).get("status") == "complete", 900, "maker swap complete", 3)
    h.wait_until(lambda: (bo.swap(mid) or {}).get("status") == "complete", 600, "taker swap complete", 3)
    sw = al.swap(mid)
    json.dump(sw, open(os.path.join(EVID, "swap-dcr_btc-after-ban-maker.json"), "w"), indent=2)
    rec = b.records(bo, epoch - 1).get(epoch)
    stats = chain_stats(al, f0)
    stats.pop("recs")
    res = {"match": {"id": mid, "epoch": epoch, "recordSigners": rec and names(net, rec["signers"]),
                     "recordActive": rec and names(net, rec["activeSet"]), "agreedSecondsAfterTake": round(took, 1),
                     "txs": {k: sw[k]["txid"] for k in ("makerSwap", "takerSwap", "makerRedeem", "takerRedeem") if sw.get(k)},
                     "maker": sw["status"], "taker": bo.swap(mid)["status"]},
           "chainDuringTrade": stats}
    b.result("trade", res)
    if not rec or net["v5"].peer_id in rec["signers"] or len(rec["signers"]) < 3:
        raise Fail("trade: %s" % res)

def phase_forge(net):
    al, v1, v3 = net["alice"], net["v1"], net["v3"]
    vid = v1.peer_id
    before = {n.name: bans(n)["badProofs"] for n in running(net)}
    f0 = finalized(al)
    al.dbgapi("/mesh/epochs/forgeproof", {"validator": vid, "mode": "gossip"})
    log("alice gossips forged proofs against v1")
    time.sleep(20)
    al.dbgapi("/mesh/epochs/forgeproof", {"mode": "off"})
    f1 = finalized(al)
    v3.dbgapi("/mesh/epochs/forgeproof", {"validator": vid, "mode": "propose"})
    log("v3 puts forged proofs against v1 in its proposals")
    time.sleep(40)
    v3.dbgapi("/mesh/epochs/forgeproof", {"mode": "off"})
    f2 = finalized(al)
    time.sleep(6)
    row = snap(net, "forge: after")
    gossip = chain_stats(al, f0, f1)
    gossip.pop("recs")
    prop = chain_stats(al, f1, f2)
    recs = prop.pop("recs")
    by_v3 = [e for e in recs if f1 < e <= f2 and recs[e]["proposer"] == v3.peer_id]
    full = {r["epoch"]: r for r in al.dbgapi("/mesh/epochs/records?from=%d&limit=%d&full=1" % (f1, max(1, f2 - f1)))["records"] or []}
    with_proofs = [e for e in full if f1 < e <= f2 and (full[e].get("proposal") or {}).get("proofs")]
    v3_turns = 0
    sets = [sorted(recs[e]["activeSet"]) for e in recs if f1 < e <= f2]
    act = sets[0] if sets else []
    for e in range(f1 + 1, f2 + 1):
        if act and act[e % len(act)] == v3.peer_id:
            v3_turns += 1
    after = {n.name: bans(n)["badProofs"] for n in running(net)}
    flags = [f for f in st(net["v2"])["flags"] or [] if f["kind"] == "bad-proof"]
    res = {"gossip": gossip, "propose": prop, "v3TurnsAsFirstProposer": v3_turns, "recordsProposedByV3": len(by_v3),
           "recordsWithProofs": len(with_proofs), "badProofsSeen": {k: after[k] - before.get(k, 0) for k in after},
           "v1EveryPeer": {k: {"member": "v1" in v["members"], "active": "v1" in v["active"], "held": v["held"],
                               "banned": [x[0] for x in v["banned"]]} for k, v in row.items() if isinstance(v, dict)},
           "v2BadProofFlags": [f["detail"] for f in flags[-3:]]}
    b.result("forge", res)
    b.shot(net["v2"], "/mesh", "s5-forged-proof-rejected")
    bad = every_peer(row, lambda v: "v1" in v["members"] and "v1" in v["active"] and [x[0] for x in v["banned"]] == ["v5"] and not v["held"])
    vals_saw = [n.name for n in net.vals if n.name != "v5" and after.get(n.name, 0) - before.get(n.name, 0) > 0]
    if bad or by_v3 or with_proofs or len(vals_saw) < 3:
        raise Fail("forge: bad %s byV3 %s proofs %s saw %s" % (bad, by_v3, with_proofs, vals_saw))
    if prop["skipped"] > max(3, prop["epochs"] // 10) or gossip["skipped"] > max(3, gossip["epochs"] // 10):
        raise Fail("finalization stalled during forgery: %s %s" % (gossip, prop))

def phase_dupkey(net):
    """v2's appdata (same key) started a second time on another mesh node.
    The mesh never delivers a peer's own messages to it, so the twin learns
    of the original from v2's signatures inside peers' records and never
    signs; the original keeps signing; nobody holds a proof against v2."""
    v2, al = net["v2"], net["alice"]
    v2.stop()
    tdir = os.path.join(b.RUN, "v2-twin")
    if os.path.exists(tdir):
        os.rename(tdir, tdir + time.strftime(".%H%M%S"))
    shutil.copytree(v2.dir, tdir, ignore=shutil.ignore_patterns("console.log", "logs"))
    net.start(v2)
    h.wait_until(lambda: b.epoch_ok(v2), 120, "v2 back", 2)
    h.wait_until(lambda: st(v2)["signing"], 120, "v2 signing", 2)
    time.sleep(6)
    g0 = bans(v2)["signGuardEpoch"]
    twin = h.Bisonw("v2-twin", b.RUN, "127.0.0.1:16110", "127.0.0.1:16210", net.addr[3], b.BISONW, 16310)
    net.nodes["v2-twin"] = twin
    f0 = finalized(al)
    twin.start(["--meshrelease", net.release, "--meshvalidator"])
    twin.login()
    t_start = time.time()
    tg0 = bans(twin)["signGuardEpoch"]
    log("v2-twin started with v2's key (guard copied: %d, v2 now %d)" % (tg0, g0))
    samples = []
    def sample():
        tb, vb = bans(twin), bans(v2)
        samples.append({"t": round(time.time() - t_start, 1),
                        "twin": {k: tb.get(k) for k in ("signHold", "duplicateKeyMsgs", "signGuardEpoch")},
                        "v2": {k: vb.get(k) for k in ("signHold", "duplicateKeyMsgs", "signGuardEpoch")}})
        return tb, vb
    def twin_saw():
        tb, _ = sample()
        return tb["duplicateKeyMsgs"] > 0
    try:
        h.wait_until(twin_saw, 60, "twin sees the other instance", 1)
        detected_s = round(time.time() - t_start, 1)
    except Fail:
        detected_s = None
    for _ in range(8):  # 40 s with the twin running (20 epochs)
        time.sleep(5)
        sample()
    tb, vb = sample()
    ts = st(twin)
    row = snap(net, "dupkey: twin running")
    twin_flags = [f["detail"] for f in ts["flags"] or [] if f["kind"] == "duplicate-key"]
    b.shot(twin, "/mesh", "s5-dupkey-twin-not-signing")
    twin.stop()
    log("twin stopped")
    time.sleep(10)
    f2 = finalized(al)
    vb2 = bans(v2)
    stats = chain_stats(al, f0, f2)
    recs = stats.pop("recs")
    v2_signed = sum(1 for e in recs if f0 < e <= f2 and v2.peer_id in recs[e]["signers"])
    row2 = snap(net, "dupkey: twin stopped")
    res = {"twinGuardBefore": tg0, "twinGuardAfter": tb["signGuardEpoch"], "twinSigned": tb["signGuardEpoch"] != tg0,
           "twinSignHold": tb.get("signHold"), "twinDupMsgs": tb["duplicateKeyMsgs"], "twinDetectedAfterS": detected_s,
           "twinFlags": twin_flags[-2:], "twinRole": ts["role"], "twinSigning": ts["signing"],
           "v2GuardDuringTwin": [g0, vb["signGuardEpoch"], vb2["signGuardEpoch"]], "v2DupMsgs": vb2["duplicateKeyMsgs"],
           "v2RecordsSignedWhileTwin": v2_signed,
           "proofAgainstV2": {k: ("v2" in v["held"] or "v2" in [x[0] for x in v["banned"]]) for k, v in row2.items() if isinstance(v, dict)},
           "finalization": stats, "samples": samples[-6:]}
    b.result("dupkey", res)
    if tb["signGuardEpoch"] != tg0 or detected_s is None or any(res["proofAgainstV2"].values()) or ts["signing"]:
        raise Fail("dupkey: %s" % {k: res[k] for k in ("twinSigned", "twinDetectedAfterS", "proofAgainstV2", "twinSigning")})
    if vb2["signGuardEpoch"] <= g0 + 10:
        raise Fail("v2 stopped signing with the twin: %s" % res["v2GuardDuringTwin"])
    if stats["skipped"] > max(3, stats["epochs"] // 10):
        raise Fail("finalization stalled with the twin: %s" % stats)

def phase_restart(net):
    al, v5 = net["alice"], net["v5"]
    g5 = bans(v5)["signGuardEpoch"]
    for m in net.mms:
        m.stop()
    net.mms = []
    s3.restart_all(net, net.vals + net.traders)
    time.sleep(10)
    row = snap(net, "restart")
    f0 = finalized(al)
    time.sleep(30)
    stats = chain_stats(al, f0)
    recs = stats.pop("recs")
    with_v5 = [e for e in recs if e > f0 and (v5.peer_id in recs[e]["activeSet"] or v5.peer_id in recs[e]["signers"])]
    v5s, v5b = st(v5), bans(v5)
    ban_rec = getattr(net, "ban_rec", None)
    res = {"afterEveryPeer": {k: {x: v[x] for x in ("banned", "members", "active", "role", "last")} for k, v in row.items() if isinstance(v, dict)},
           "v5": {"role": v5s["role"], "signing": v5s["signing"], "guardBefore": g5, "guardAfter": v5b["signGuardEpoch"]},
           "finalizationAfterRestart": stats, "recordsWithV5": len(with_v5),
           "proofFiles": {n.name: any(f.endswith(".bans.json") for f in os.listdir(os.path.join(n.dir, "simnet", "mesh"))) for n in net.vals + net.traders}}
    b.result("restart", res)
    b.shot(al, "/mesh", "s5-after-restart")
    bad = every_peer(row, lambda v: [x[0] for x in v["banned"]] == ["v5"] and (ban_rec is None or v["banned"][0][1] == ban_rec) and "v5" not in v["members"])
    if bad or v5s["role"] != "banned" or v5b["signGuardEpoch"] != g5 or with_v5 or stats["finalized"] < 10:
        raise Fail("restart: bad %s v5 %s withV5 %s stats %s" % (bad, res["v5"], with_v5, stats))

def phase_same(net):
    nodes = running(net)
    rep = s3.same_set(net, nodes, "same")
    b.result("same", rep)

PHASES = [("switch", phase_switch), ("book", phase_book), ("warm", phase_warm), ("equivocate", phase_equivocate),
          ("trade", phase_trade), ("forge", phase_forge), ("dupkey", phase_dupkey), ("restart", phase_restart), ("same", phase_same)]

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
                if name in ("switch", "warm", "equivocate"):
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
