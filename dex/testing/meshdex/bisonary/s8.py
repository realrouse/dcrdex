#!/usr/bin/env python3
"""SIMNET/REGTEST ONLY. S8 resilience campaign on the box.

Network as s7.py / s7book.py: private 3-node Tatanka mesh, 4 seeded
validators, traders alice, bob, carol, mm, dave and erin with realistic
balances (1-5 M LBC, ~200 DCR, ~0.355 BTC), CoinGecko-priced books with 10+
orders per side on 5 markets, the emulated market maker, trading bonds
required (S6), verified price history (S7) and book cross-checks.

Faults: app-level partitions, loss and delay come from the simnet fault
layer in bisonw (POST /mesh/faults: drop everything from given peers, drop a
percentage of messages, delay with jitter); mesh-node outages, kills and
box freezes are real (tmux kill of a Tatanka node, SIGKILL, SIGSTOP/SIGCONT).

Phases (default all, in order):
  wallets switch bonds book   as s7.py (bond locks 6 h for the long run)
  partition22  validators split 2|2 (with traders): neither side finalizes,
               no fork; heal: finalization resumes, orders from both sides
               reach every book, identical chains
  partition31  v4 cut off: the 3 others keep finalizing; heal: v4 catches up
               and signs again, identical chains
  meshnode     Tatanka mesh node 3 (v3, bob) killed 60 s: the rest keeps
               finalizing; node restarted: v3 and bob reconnect and sync
  churn        each validator SIGKILLed and restarted in turn, then 2 at once
               (stall, resume), then one restarted 3 times quickly: no bans,
               no equivocation, every validator signs again, same chains
  freeze       one validator SIGSTOPped 45 s; then every bisonw and mesh node
               frozen 60 s (box pause): resume, no bans, same chains
  loss         all peers drop 10 % of messages with 200-800 ms delay
               (scaled to the 2 s epoch: 40-160 ms) for 120 s while a swap
               is matched and runs, then 30 % with 0-1500 ms (0-300 ms) for
               60 s and 10 % with unscaled 200-800 ms for 60 s (both
               reported only); cleared: recovery, same chains, no suspects
  flood        erin floods the epoch topic (junk, bad JSON, bad-signature
               orders) and v1's inbox: finalization continues, nothing admitted
  fakerelay    erin replays captured epoch messages unchanged and tampered
               (fingerprints, matches, orders, heartbeats, book digests):
               nothing accepted, no clock or ban effects, same chains
  swaprestart  dcr_btc and lbc_dcr swaps; maker SIGKILLed after its init,
               taker after its participation, a validator restarted mid-swap:
               both swaps complete
  soak         SOAK_MIN (default 30) minutes of trading with the emulated mm
               and taker bots on 3 markets: finalization rate, chain
               agreement, memory, suspects sampled every minute
  agree        every completed swap of the run is in the verified history on
               every peer; identical chains
Usage: ./s8.py [phase ...]
"""
import os, sys, time, json, signal, subprocess, threading, random
HERE = os.path.dirname(os.path.abspath(__file__))
os.environ.setdefault("EVID", "/workspace/tatanka-dev/evidence/s8-resilience")
os.environ.setdefault("BISONW", "/workspace/tatanka-dev/bin/bisonw-bisonary-s8")
os.environ.setdefault("RUN", "/workspace/tatanka-dev/run/s012")
sys.path.insert(0, HERE)
import harness as h
from harness import log, Fail
import s012 as b
import s3
import s6
import s7
import s7book

EVID = b.EVID
EPOCH_MS = 2000
SOAK_MIN = float(os.environ.get("SOAK_MIN", "30"))
LOCK = 6 * 3600
for k, bl in s6.BONDS.items():
    s6.BONDS[k] = [(a, amt, LOCK if lock == s7.LOCK else lock, fake) for (a, amt, lock, fake) in bl]
TL = []

# ---------------- helpers ----------------
def peers(net):
    return s6.peers(net)

def fin(n):
    return s3.finalized(n)

def cur_epoch(n):
    return n.epochs()["currentEpoch"]

def nrec(n, c0, c1):
    """Epochs in [c0, c1) with a finalized record on n's chain. (The
    lastFinalized delta overstates finalization: an epoch that did not
    finalize rolls its orders into the next one and lastFinalized jumps over
    it, so run1 counted rolled epochs as finalized.)"""
    out, frm = 0, max(0, c0 - 1)  # the API returns epochs > from
    while frm + 1 < c1:
        rs = b.records(n, frm)
        if not rs:
            break
        out += len([e for e in rs if c0 <= e < c1])
        if len(rs) < 1000:
            break
        frm = max(rs)
    return out

def status_row(net, tag, nodes=None):
    row = {"t": time.strftime("%T"), "tag": tag}
    for n in nodes or peers(net):
        try:
            s = n.epochs()
            row[n.name] = {"last": s.get("lastFinalized"), "cur": s["currentEpoch"], "unfin": s.get("unfinalizedSinceLast"),
                           "signers": s.get("lastSigners"), "signing": s.get("signing"), "banned": s.get("banned"),
                           "clockOK": s.get("clockOK"), "skew": s.get("clockSkewMs"), "pool": s.get("pool"), "resting": s.get("resting"),
                           "suspects": s.get("bookSuspects"), "role": s.get("role")}
        except Exception as e:
            row[n.name] = "error %s" % e
    TL.append(row)
    json.dump(TL, open(os.path.join(EVID, "timeline.json"), "w"), indent=1)
    return row

def window(net, n, secs, tag, skip=()):
    """Finalized epochs on n over secs seconds and the epochs that passed
    (skip: frozen nodes that would not answer)."""
    f0, c0 = fin(n), cur_epoch(n)
    end = time.time() + secs
    paused, prev = 0.0, time.time()
    while time.time() < end:
        step = min(10, max(0.5, end - time.time()))
        time.sleep(step)
        now = time.time()
        if now - prev - step > 20:  # a box pause: the whole VM stopped
            paused += now - prev - step
            end += now - prev - step
            log("%s: box pause of ~%d s" % (tag, now - prev - step))
        status_row(net, tag, [x for x in peers(net) if x not in skip])
        prev = time.time()
    f1, c1 = fin(n), cur_epoch(n)
    pe = int(paused * 1000 / EPOCH_MS)
    k = nrec(n, c0, c1)
    return {"node": n.name, "finalized": k, "epochs": c1 - c0, "boxPauseEpochs": pe, "lastFinalizedAdvance": f1 - f0,
            "ratio": round(k / max(1, c1 - c0 - pe), 3)}

def bans_and_eqv(net):
    out = {}
    for n in peers(net):
        s = n.epochs()
        eq = [f for f in s["flags"] or [] if f["kind"] in ("equivocation", "conflict", "bad-proof")]
        if s.get("banned") or eq:
            out[n.name] = {"banned": s.get("banned"), "flags": [(f["kind"], f["peer"][-8:], f["detail"][:80]) for f in eq]}
    return out

def chains(net, back=400, nodes=None):
    nodes = nodes or peers(net)
    frm = max(0, min(fin(n) for n in nodes) - back)
    return b.compare_chains(nodes, frm)

def set_faults(nodes, f):
    for n in nodes:
        n.dbgapi("/mesh/faults", f)

def clear_faults(net):
    for n in peers(net):
        try:
            n.dbgapi("/mesh/faults", {})
        except Exception:
            pass

def restart(net, n, kill=False):
    n.stop(kill=kill)
    time.sleep(1)
    start(net, n)

def start(net, n):
    if n in getattr(net, "extra", []):
        s6.start_extra(net, n)
    else:
        net.start(n)

def wait_ok(nodes, timeout, what):
    t0 = time.time()
    h.wait_until(lambda: all(b.epoch_ok(n) for n in nodes), timeout, what, 1)
    return round(time.time() - t0, 1)

def signs_again(net, v, timeout=120):
    t0 = time.time()
    def ok():
        recs = b.records(net["alice"], fin(net["alice"]) - 3)
        return any(v.peer_id in (r.get("signers") or []) for r in recs.values())
    h.wait_until(ok, timeout, "%s signing again" % v.name, 2)
    return round(time.time() - t0, 1)

def suspects(net):
    return {n.name: n.epochs()["bookSuspects"] for n in peers(net) if n.epochs()["bookSuspects"]}

def far_orders(n, mkt, k, frac, base=0, lifetime=600):
    mid = h.rate_for(net_prices(), mkt)
    return [n.order(mkt, False, 1, max(1, int(mid * frac) - base - i), lifetime) for i in range(k)]

_NET = {}
def net_prices():
    return _NET["net"].prices

def order_everywhere(net, oids, statuses=("resting", "done")):
    for n in peers(net):
        for o in oids:
            if n.dbgapi("/mesh/epochs/order?id=" + o)["status"] not in statuses:
                return False
    return True

def mesh_pids(i=None):
    # pgrep -f takes a regex; no index = every mesh node of this run.
    pat = "p4mesh/tatanka-%s/tatanka.conf" % (i if i else "[0-9]+")
    r = subprocess.run(["pgrep", "-f", pat], capture_output=True, text=True)
    return [int(x) for x in r.stdout.split()]

def rss_mb(n):
    try:
        for line in open("/proc/%d/status" % n.proc.pid):
            if line.startswith("VmRSS:"):
                return int(line.split()[1]) // 1024
    except Exception:
        return None

# ---------------- phases ----------------
def phase_partition22(net):
    _NET["net"] = net
    A = [net[x] for x in ("v1", "v4", "carol", "dave")]
    B = [n for n in peers(net) if n not in A]
    ida, idb = [n.peer_id for n in A], [n.peer_id for n in B]
    before = {n.name: fin(n) for n in peers(net)}
    set_faults(A, {"block": idb})
    set_faults(B, {"block": ida})
    t0 = time.time()
    time.sleep(6)
    oa = far_orders(net["carol"], "lbc_dcr", 2, 0.7)
    ob = far_orders(net["bob"], "lbc_dcr", 2, 0.7, 50)
    time.sleep(40)
    during = status_row(net, "partition22")
    after = {n.name: fin(n) for n in peers(net)}
    progressed = {k: after[k] - before[k] for k in after}
    blocked = {n.name: n.dbgapi("/mesh/faults")["blocked"] for n in peers(net)}
    # No fork: every record both sides hold is identical.
    rep_split = chains(net, 100)
    set_faults(peers(net), {})
    heal = wait_ok(peers(net), 120, "finalization after heal")
    t_orders = time.time()
    h.wait_until(lambda: order_everywhere(net, oa + ob), 120, "both sides' orders on every node", 3)
    orders_s = round(time.time() - t_orders, 1)
    time.sleep(10)
    rep = chains(net, 200)
    res = {"sideA": [n.name for n in A], "sideB": [n.name for n in B], "partitionSeconds": round(time.time() - t0),
           "finalizedDuringPartition": progressed, "blockedMessages": blocked, "chainsDuringPartition": rep_split,
           "secondsToResumeAfterHeal": heal, "secondsUntilBothSidesOrdersOnEveryBook": orders_s, "chainsAfterHeal": rep,
           "bansOrEquivocation": bans_and_eqv(net), "suspectsAfter": suspects(net)}
    b.result("partition22", res)
    for o in oa:
        net["carol"].cancel(o)
    for o in ob:
        net["bob"].cancel(o)
    if max(progressed.values()) > 2 or res["bansOrEquivocation"]:
        raise Fail("partition22: %s" % res)

def phase_partition31(net):
    v4 = net["v4"]
    rest = [n for n in peers(net) if n is not v4]
    f4 = fin(v4)
    set_faults([v4], {"block": [n.peer_id for n in rest]})
    set_faults(rest, {"block": [v4.peer_id]})
    w = window(net, net["v1"], 40, "partition31")
    stuck = fin(v4) - f4
    set_faults(peers(net), {})
    t0 = time.time()
    h.wait_until(lambda: abs(fin(v4) - fin(net["v1"])) <= 1, 90, "v4 caught up", 1)
    caught = round(time.time() - t0, 1)
    again = signs_again(net, v4)
    rep = chains(net, 200)
    res = {"cutOff": "v4", "majorityWindow": w, "v4FinalizedWhileCut": stuck, "v4CatchUpSeconds": caught,
           "v4SignsAgainSeconds": again, "chains": rep, "bansOrEquivocation": bans_and_eqv(net)}
    b.result("partition31", res)
    if w["ratio"] < 0.7 or stuck > 2 or res["bansOrEquivocation"]:
        raise Fail("partition31: %s" % res)

def phase_meshnode(net):
    mesh = b.MESH
    on3 = [net["v3"], net["bob"]]
    subprocess.run([mesh, "kill", "3"], check=True)
    time.sleep(3)
    killed = not mesh_pids(3)
    w = window(net, net["v1"], 60, "meshnode 3 down")
    stalled3 = {n.name: n.epochs()["unfinalizedSinceLast"] for n in on3}
    subprocess.run([mesh, "restart", "3"], check=True)
    t0 = time.time()
    try:
        back = wait_ok(on3, 180, "v3 and bob back after mesh node restart")
    except Fail as e:
        back = None
        log("meshnode: %s" % e)
    res = {"meshNodeKilled": killed, "clientsOnNode3": [n.name for n in on3], "othersWindow": w, "node3ClientsUnfinalized": stalled3,
           "secondsUntilNode3ClientsFinalize": back}
    if back is None:  # record the state, then restart the clients so later phases run
        res["node3ClientStatus"] = {n.name: n.epochs() for n in on3}
        for n in on3:
            restart(net, n)
        res["afterClientRestart"] = wait_ok(on3, 120, "v3 and bob after client restart")
    res["v3SignsAgainSeconds"] = signs_again(net, net["v3"])
    res["chains"] = chains(net, 200)
    res["bansOrEquivocation"] = bans_and_eqv(net)
    b.result("meshnode", res)
    if w["ratio"] < 0.7 or back is None or res["bansOrEquivocation"]:
        raise Fail("meshnode: %s" % res)

def phase_churn(net):
    res = {"single": {}, "double": {}, "rapid": {}}
    for v in net.vals:
        f0, c0 = fin(net["alice"]), cur_epoch(net["alice"])
        v.stop(kill=True)
        time.sleep(20)
        start(net, v)
        back = wait_ok([v], 120, "%s back" % v.name)
        again = signs_again(net, v)
        c1 = cur_epoch(net["alice"])
        res["single"][v.name] = {"finalizedWhileDown": nrec(net["alice"], c0, c1), "epochs": c1 - c0, "lastFinalizedAdvance": fin(net["alice"]) - f0,
                                 "secondsToSync": back, "secondsToSignAgain": again}
    a, c = net["v1"], net["v3"]
    a.stop(kill=True)
    c.stop(kill=True)
    time.sleep(5)
    f0 = fin(net["alice"])
    time.sleep(25)
    res["double"]["finalizedWithTwoDown"] = fin(net["alice"]) - f0
    start(net, a)
    start(net, c)
    res["double"]["secondsToResume"] = wait_ok(peers(net), 180, "resume after double kill")
    v2 = net["v2"]
    for i in range(3):
        v2.stop(kill=True)
        time.sleep(2)
        start(net, v2)
        time.sleep(10)
    res["rapid"]["v2SecondsToSync"] = wait_ok([v2], 120, "v2 after rapid restarts")
    res["rapid"]["v2SignsAgainSeconds"] = signs_again(net, v2, 180)
    w = window(net, net["alice"], 30, "churn: after")
    res["after"] = w
    res["chains"] = chains(net, 400)
    res["bansOrEquivocation"] = bans_and_eqv(net)
    res["holds"] = {v.name: v.epochs().get("signHold") for v in net.vals}
    b.result("churn", res)
    single_ok = all(x["finalizedWhileDown"] >= 0.6 * x["epochs"] for x in res["single"].values())
    if res["bansOrEquivocation"] or res["double"]["finalizedWithTwoDown"] > 1 or w["ratio"] < 0.8 or not single_ok:
        raise Fail("churn: %s" % res)

def sig(pids, s):
    for p in pids:
        try:
            os.kill(p, s)
        except ProcessLookupError:
            pass

def phase_freeze(net):
    res = {}
    v2 = net["v2"]
    f0 = fin(net["v1"])
    sig([v2.proc.pid], signal.SIGSTOP)
    w = window(net, net["v1"], 45, "freeze v2", skip=[v2])
    sig([v2.proc.pid], signal.SIGCONT)
    t0 = time.time()
    h.wait_until(lambda: abs(fin(v2) - fin(net["v1"])) <= 1, 90, "v2 caught up after freeze", 1)
    res["single"] = {"frozen": "v2", "othersWindow": w, "v2CatchUpSeconds": round(time.time() - t0, 1),
                     "v2SignsAgainSeconds": signs_again(net, v2), "v2Status": status_row(net, "freeze v2: after", [v2])["v2"]}
    allp = [n.proc.pid for n in peers(net)] + mesh_pids()
    for m in net.mms:
        pass  # the mm thread just gets errors while frozen
    fb = fin(net["v1"])
    sig(allp, signal.SIGSTOP)
    t_freeze = time.time()
    time.sleep(60)
    sig(allp, signal.SIGCONT)
    frozen = round(time.time() - t_freeze, 1)
    t0 = time.time()
    resumed = wait_ok(peers(net), 240, "resume after box freeze")
    res["box"] = {"frozenProcesses": len(allp), "frozenMeshNodes": len(allp) - len(peers(net)), "frozenSeconds": frozen, "secondsToResume": resumed, "finalizedBefore": fb,
                  "status": status_row(net, "freeze box: after")}
    res["box"]["window"] = window(net, net["v1"], 30, "freeze box: after")
    res["chains"] = chains(net, 300)
    res["bansOrEquivocation"] = bans_and_eqv(net)
    b.result("freeze", res)
    if w["ratio"] < 0.7 or res["bansOrEquivocation"] or res["box"]["window"]["ratio"] < 0.8 or res["box"]["frozenMeshNodes"] < 3:
        raise Fail("freeze: %s" % res)

def scaled(ms):
    """Delays relative to the epoch: the campaign runs the 2 s simnet release
    (1 s signing window), the design is 10 s epochs with a 5 s window, so
    200-800 ms on the design release is 40-160 ms here."""
    return int(ms * EPOCH_MS / 10000)

def phase_loss(net):
    """Message loss. PLAN.md S8 asks for kill/flood/partition/fake-relay
    tests; loss is an extra. Pass: 10 % loss with the design's delays (scaled
    to the epoch) keeps finalizing (>= 50 % of epochs, unfinalized ones roll
    over) and a swap matches and completes under it; afterwards full
    finalization, no bans, no suspects. Reported only: 30 % loss, and 10 %
    loss with unscaled 200-800 ms delays (beyond the 1 s window of this
    release)."""
    _NET["net"] = net
    res = {"epochMs": EPOCH_MS}
    al, dv = net["alice"], net["dave"]
    f10 = {"dropInPct": 10, "delayMs": scaled(200), "jitterMs": scaled(600)}
    set_faults(peers(net), f10)
    res["faults10"] = f10
    trade = {}
    def do_trade():
        try:
            trade["r"] = s7.one_trade(net, "dcr_btc", dv, al)
        except Exception as e:
            trade["err"] = str(e)
    th = threading.Thread(target=do_trade, daemon=True)
    th.start()
    res["loss10"] = window(net, net["v1"], 120, "loss 10%")
    res["loss10"]["chains"] = chains(net, 100)
    res["faultStats10"] = {n.name: n.dbgapi("/mesh/faults") for n in peers(net)}
    # (The take waits at most 90 s for its match, so it is matched under the
    # 10 % loss above; the swap goes on under the heavier loss below.)
    f30 = {"dropInPct": 30, "delayMs": 0, "jitterMs": scaled(1500)}
    set_faults(peers(net), f30)
    res["faults30"] = f30
    res["loss30"] = window(net, net["v1"], 60, "loss 30%")
    res["loss30"]["chains"] = chains(net, 100)
    fraw = {"dropInPct": 10, "delayMs": 200, "jitterMs": 600}
    set_faults(peers(net), fraw)
    res["faults10Unscaled"] = fraw
    res["loss10Unscaled"] = window(net, net["v1"], 60, "loss 10% unscaled delay")
    res["faultStats"] = {n.name: n.dbgapi("/mesh/faults") for n in peers(net)}
    res["suspectsDuringLoss"] = suspects(net)
    clear_faults(net)
    res["secondsToRecover"] = wait_ok(peers(net), 120, "recovery after loss")
    th.join(900)
    res["tradeUnderLoss"] = trade.get("r") or trade.get("err") or "still running"
    t1 = time.time()
    try:
        h.wait_until(lambda: not suspects(net), 90, "no suspects after loss", 3)
        res["secondsUntilNoSuspects"] = round(time.time() - t1, 1)
    except Fail:
        res["secondsUntilNoSuspects"] = None
        res["suspectsAfter"] = suspects(net)
    res["after"] = window(net, net["v1"], 30, "loss: after")
    res["chains"] = chains(net, 300)
    res["bansOrEquivocation"] = bans_and_eqv(net)
    b.result("loss", res)
    if isinstance(res["tradeUnderLoss"], dict) and res["tradeUnderLoss"].get("ok"):
        s7.TRADES.append(res["tradeUnderLoss"])
    if res["bansOrEquivocation"] or res["after"]["ratio"] < 0.8 or res["secondsUntilNoSuspects"] is None or \
            not (isinstance(res["tradeUnderLoss"], dict) and res["tradeUnderLoss"].get("ok")) or res["loss10"]["ratio"] < 0.5:
        raise Fail("loss: %s" % res)

def phase_flood(net):
    er, v1 = net["erin"], net["v1"]
    res = {}
    f0, c0 = fin(v1), cur_epoch(v1)
    out = {}
    t0 = time.time()
    out["junk"] = er.dbgapi("/mesh/epochs/flood", {"mode": "junk", "n": 3000, "size": 512})
    out["badjson"] = er.dbgapi("/mesh/epochs/flood", {"mode": "badjson", "n": 1000})
    out["orders"] = er.dbgapi("/mesh/epochs/flood", {"mode": "orders", "n": 1000})
    out["inboxV1"] = er.dbgapi("/mesh/adversary/flood", {"peer": v1.peer_id, "mode": "junk", "n": 1000, "size": 512})
    res["floodSeconds"] = round(time.time() - t0, 1)
    res["sent"] = out
    w = window(net, v1, 30, "flood: after")
    c1 = cur_epoch(v1)
    res["duringAndAfter"] = {"finalized": nrec(v1, c0, c1), "epochs": c1 - c0, "lastFinalizedAdvance": fin(v1) - f0}
    res["after"] = w
    res["alive"] = {n.name: n.alive() for n in peers(net)}
    n_erin, total = s6.owner_orders_in_records(net, v1, f0 - 1, er.peer_id)
    res["erinOrdersInRecords"] = n_erin
    res["v1InboxDropped"] = v1.dbgapi("/mesh/inbox/dropped")
    res["chains"] = chains(net, 200)
    res["bansOrEquivocation"] = bans_and_eqv(net)
    b.result("flood", res)
    if n_erin or not all(res["alive"].values()) or w["ratio"] < 0.8 or res["bansOrEquivocation"]:
        raise Fail("flood: %s" % res)

def phase_fakerelay(net):
    er, v1 = net["erin"], net["v1"]
    er.dbgapi("/mesh/epochs/fakerelay", {"capture": True})
    time.sleep(40)
    f0, c0 = fin(v1), cur_epoch(v1)
    skew0 = {v.name: v.epochs()["clockSkewMs"] for v in net.vals}
    res = {"replayed": er.dbgapi("/mesh/epochs/fakerelay", {"n": 400, "tamper": False})}
    time.sleep(10)
    res["tampered"] = er.dbgapi("/mesh/epochs/fakerelay", {"n": 400, "tamper": True})
    w = window(net, v1, 40, "fakerelay: after")
    res["window"] = w
    res["clockSkewBefore"] = skew0
    res["clockAfter"] = {v.name: {"skewMs": v.epochs()["clockSkewMs"], "ok": v.epochs()["clockOK"], "signing": v.epochs()["signing"]} for v in net.vals}
    res["suspects"] = suspects(net)
    res["chains"] = chains(net, 200)
    res["bansOrEquivocation"] = bans_and_eqv(net)
    b.result("fakerelay", res)
    if w["ratio"] < 0.8 or res["bansOrEquivocation"] or res["suspects"] or not all(x["ok"] and x["signing"] for x in res["clockAfter"].values()):
        raise Fail("fakerelay: %s" % res)

def swap_status(n, mid):
    s = n.swap(mid)
    return s and s["status"]

def phase_swaprestart(net):
    """One swap at a time (run1 ran both at once and polled every 3 s; the
    swaps finished while the harness slept after the first kill)."""
    res = {}
    plan = [("dcr_btc", net["dave"], net["alice"]), ("lbc_dcr", net["bob"], net["carol"])]
    events, out = [], []
    killed_v = False
    n_mk = n_tk = 0
    for mkt, mk, tk in plan:
        m, oid, rate, took = s6.epoch_trade(net, mkt, mk, tk)
        mid = m["match"]["id"]
        killed_mk = killed_tk = False
        deadline = time.time() + 900
        while time.time() < deadline:
            smk, stk = swap_status(mk, mid), swap_status(tk, mid)
            if smk == "complete" and stk == "complete":
                break
            if not killed_mk and smk in ("init_sent", "part_audited"):
                events.append({"t": time.strftime("%T"), "market": mkt, "kill": mk.name, "role": "maker", "makerStatus": smk, "takerStatus": stk})
                mk.stop(kill=True)
                if not killed_v:  # a validator restarted mid-swap too
                    events.append({"t": time.strftime("%T"), "restart": "v1"})
                    restart(net, net["v1"], kill=True)
                    killed_v = True
                time.sleep(10)
                start(net, mk)
                killed_mk = True
                n_mk += 1
                continue
            if killed_mk and not killed_tk and (stk == "part_sent" or (smk == "redeemed" and stk != "complete")):
                events.append({"t": time.strftime("%T"), "market": mkt, "kill": tk.name, "role": "taker", "makerStatus": smk, "takerStatus": stk})
                tk.stop(kill=True)
                time.sleep(10)
                start(net, tk)
                killed_tk = True
                n_tk += 1
                continue
            time.sleep(0.5)
        sw = mk.swap(mid)
        out.append({"market": mkt, "match": mid, "maker": mk.name, "taker": tk.name, "makerKilled": killed_mk, "takerKilled": killed_tk,
                    "makerStatus": swap_status(mk, mid), "takerStatus": swap_status(tk, mid),
                    "txs": {k: (sw.get(k) or {}).get("txid") for k in ("makerSwap", "takerSwap", "makerRedeem", "takerRedeem")} if sw else None})
        log("swaprestart %s: maker %s, taker %s (killed maker %s, taker %s)" % (mkt, out[-1]["makerStatus"], out[-1]["takerStatus"], killed_mk, killed_tk))
    res["events"] = events
    res["swaps"] = out
    res["validatorRestarted"] = killed_v
    res["chains"] = chains(net, 300)
    res["bansOrEquivocation"] = bans_and_eqv(net)
    b.result("swaprestart", res)
    if not all(x["makerStatus"] == "complete" and x["takerStatus"] == "complete" for x in out) or n_mk < 2 or n_tk < 1 or not killed_v:
        raise Fail("swaprestart: %s" % res)

class Bot(threading.Thread):
    def __init__(self, net, name, plan):
        super().__init__(daemon=True, name=name)
        self.net, self.plan, self.stop_ev = net, plan, threading.Event()
        self.done, self.errors = [], []
    def run(self):
        i = 0
        while not self.stop_ev.is_set():
            mkt, mk, tk = self.plan[i % len(self.plan)]
            i += 1
            try:
                r = s7.one_trade(self.net, mkt, mk, tk)
                self.done.append(r)
                s7.TRADES.append(r)
            except Exception as e:
                self.errors.append("%s %s: %s" % (time.strftime("%T"), mkt, str(e)[:300]))
                self.stop_ev.wait(20)
    def stop(self):
        self.stop_ev.set()

class MMTaker(threading.Thread):
    """Takes 1 lot from the market maker's best ask or bid every few minutes."""
    def __init__(self, net, tk, mkts, every=150):
        super().__init__(daemon=True, name="mmtaker")
        self.net, self.tk, self.mkts, self.every, self.stop_ev = net, tk, mkts, every, threading.Event()
        self.takes, self.errors = [], []
    def run(self):
        i = 0
        mmid = self.net["mm"].peer_id
        while not self.stop_ev.is_set():
            mkt = self.mkts[i % len(self.mkts)]
            sell = (i // len(self.mkts)) % 2 == 1
            i += 1
            try:
                ords = [o for o in self.tk.book(mkt) if o["owner"] == mmid and o["sell"] != sell]
                if ords:
                    o = (min if not sell else max)(ords, key=lambda x: x["rate"])
                    r = self.tk.dbgapi("/mesh/take", {"market": mkt, "sell": sell, "qty": h.LOTS[mkt], "rate": o["rate"], "lotSize": h.LOTS[mkt]})
                    self.takes.append({"t": time.strftime("%T"), "market": mkt, "sell": sell, "rate": o["rate"], "r": r})
            except Exception as e:
                self.errors.append("%s %s: %s" % (time.strftime("%T"), mkt, str(e)[:200]))
            self.stop_ev.wait(self.every)
    def stop(self):
        self.stop_ev.set()

def phase_soak(net):
    bots = [Bot(net, "bot-dcr_btc", [("dcr_btc", net["dave"], net["alice"]), ("dcr_btc", net["alice"], net["dave"])]),
            Bot(net, "bot-lbc_dcr", [("lbc_dcr", net["bob"], net["carol"]), ("lbc_dcr", net["carol"], net["bob"])])]
    taker = MMTaker(net, net["carol"], ["dcr_btc", "lbc_dcr", "lbc_btc"])
    for x in bots + [taker]:
        x.start()
    t_end = time.time() + SOAK_MIN * 60
    samples = []
    f0, c0 = fin(net["v1"]), cur_epoch(net["v1"])
    while time.time() < t_end:
        fa, ca = fin(net["v1"]), cur_epoch(net["v1"])
        tb = time.time()
        time.sleep(60)
        gap = time.time() - tb - 60
        if gap > 20:
            t_end += gap
            log("soak: box pause of ~%d s" % gap)
        row = status_row(net, "soak")
        try:
            rep = chains(net, 60)
            chain_ok = True
        except Fail as e:
            rep, chain_ok = str(e), False
        cb = cur_epoch(net["v1"])
        smp = {"t": time.strftime("%T"), "finalized": nrec(net["v1"], ca, cb), "epochs": cb - ca, "chainsAgree": chain_ok,
               "rssMB": {n.name: rss_mb(n) for n in peers(net)}, "trades": sum(len(x.done) for x in bots), "mmTakes": len(taker.takes),
               "boxPauseS": round(gap) if gap > 20 else 0,
               "suspects": suspects(net), "bans": bans_and_eqv(net), "pool": {k: v.get("pool") for k, v in row.items() if isinstance(v, dict)}}
        samples.append(smp)
        json.dump(samples, open(os.path.join(EVID, "soak-samples.json"), "w"), indent=1)
        log("soak: %d/%d finalized, %d trades, %d mm takes, chains %s, rss v1 %s MB" % (smp["finalized"], smp["epochs"], smp["trades"], smp["mmTakes"],
                                                                                    chain_ok, smp["rssMB"].get("v1")))
    for x in bots + [taker]:
        x.stop()
    for x in bots:
        x.join(900)
    f1, c1 = fin(net["v1"]), cur_epoch(net["v1"])
    rss0, rss1 = samples[0]["rssMB"], samples[-1]["rssMB"]
    pe = int(sum(x["boxPauseS"] for x in samples) * 1000 / EPOCH_MS)
    k = nrec(net["v1"], c0, c1)
    res = {"minutes": SOAK_MIN, "finalized": k, "epochs": c1 - c0, "boxPauseEpochs": pe, "lastFinalizedAdvance": f1 - f0,
           "ratio": round(k / max(1, c1 - c0 - pe), 3),
           "trades": [{k: r[k] for k in ("market", "maker", "taker", "match", "ok", "secondsCompleteToVerifiedEveryPeer")} for x in bots for r in x.done],
           "tradeErrors": [e for x in bots for e in x.errors], "mmTakes": len(taker.takes), "mmTakeErrors": taker.errors[:20],
           "minuteRatioMin": min([s["finalized"] / max(1, s["epochs"]) for s in samples if not s["boxPauseS"]] or [0]), "chainsAgreeEveryMinute": all(s["chainsAgree"] for s in samples),
           "rssFirstLastMB": {k: (rss0.get(k), rss1.get(k)) for k in rss1}, "suspectSamples": sum(1 for s in samples if s["suspects"]),
           "bans": bans_and_eqv(net), "chains": chains(net, 900)}
    json.dump(taker.takes, open(os.path.join(EVID, "soak-mm-takes.json"), "w"), indent=1)
    b.result("soak", res)
    ok_trades = sum(1 for x in bots for r in x.done if r["ok"])
    if res["ratio"] < 0.9 or not res["chainsAgreeEveryMinute"] or res["bans"] or ok_trades < 4:
        raise Fail("soak: %s" % {k: res[k] for k in ("ratio", "chainsAgreeEveryMinute", "bans", "tradeErrors")})

def phase_agree(net):
    s7.phase_agree(net)
    b.result("agreeChains", chains(net, 900))

s7.PHASES = [("wallets", s6.phase_wallets), ("switch", s7.phase_switch), ("bonds", s6.phase_bonds), ("book", s6.phase_book),
             ("partition22", phase_partition22), ("partition31", phase_partition31), ("meshnode", phase_meshnode),
             ("churn", phase_churn), ("freeze", phase_freeze), ("loss", phase_loss), ("flood", phase_flood),
             ("fakerelay", phase_fakerelay), ("swaprestart", phase_swaprestart), ("soak", phase_soak), ("agree", phase_agree)]

if __name__ == "__main__":
    s7.main()
