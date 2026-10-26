#!/usr/bin/env python3
"""SIMNET/REGTEST ONLY. P6 adversarial e2e on dcr_btc with real dcrwallet and
bitcoind contracts, over the private 3-node mesh tatanka-p4 (mesh-p4.sh
defaults). A is the adversary (debug tamper/halt/flood knobs), B is honest.
  A: btc p6adv1, dcr trading1/adv     B: btc p6adv2, dcr trading2/adv
Scenarios:
  maker-<mode>   A (maker) locks a wrong contract (value, secret hash, lock
                 time, recipient): B must not participate, B abandons, A refunds
  taker-<mode>   A (taker) participates with a wrong contract: B must not
                 redeem, both refund
  never-redeem   A (maker) never redeems: both refund
  never-part     B (taker) never participates: A refunds, B ends final
  flood          A replays its last real message to B 500x, sends 500 junk
                 envelopes and oversize (4 KiB) ones during an honest swap: B
                 drops them and the swap still completes
Every wallet's balance delta must equal the swap records' expectation less
bounded fees. Usage: ./p6-adversarial.py
"""
import importlib.util, json, os, sys, time, datetime, subprocess
from decimal import Decimal

HERE = os.path.dirname(os.path.abspath(__file__))
spec = importlib.util.spec_from_file_location("camp", os.path.join(HERE, "p6-campaign.py"))
C = importlib.util.module_from_spec(spec); spec.loader.exec_module(C)

C.RUN = os.environ.get("RUN", "/workspace/tatanka-dev/run/p6adv")
C.BISONW = os.environ.get("BISONW", C.BIN + "/bisonw-tatanka-adv")
C.MESH_ENV = dict(os.environ, PATH=C.BIN + ":" + os.environ["PATH"])  # tatanka-p4 defaults
C.MAKER_LOCK, C.TAKER_LOCK = 300, 150
C.PEERS.clear()
C.PEERS.update({
    "A": dict(web=15831, dbg=15931, node=1, lbc=None, btc="p6adv1", dcr=("trading1", "adv"), markets=["dcr_btc"]),
    "B": dict(web=15832, dbg=15932, node=2, lbc=None, btc="p6adv2", dcr=("trading2", "adv"), markets=["dcr_btc"]),
})
MK = "dcr_btc"
LOT = C.LOT
RESULTS = []


def setup_wallets():
    btc = C.BTCCLI
    for w in ("p6adv1", "p6adv2"):
        if w not in C.sh(btc + ["listwallets"]):
            if subprocess.run(btc + ["loadwallet", w], capture_output=True).returncode != 0:
                C.sh(btc + ["createwallet", w])
        if Decimal(C.sh(btc + ["-rpcwallet=" + w, "getbalance"])) < 5:
            C.sh(btc + ["-rpcwallet=default", "sendtoaddress", C.sh(btc + ["-rpcwallet=" + w, "getnewaddress"]), "20"])
    for t in ("trading1", "trading2"):
        ctl = C.DEXTEST + "/dcr/harness-ctl/" + t
        subprocess.run([ctl, "walletpassphrase", "abc", "0"], capture_output=True)
        if subprocess.run([ctl, "getaccountaddress", "adv"], capture_output=True).returncode != 0:
            C.sh([ctl, "createnewaccount", "adv"])
        b = json.loads(C.sh([ctl, "getbalance", "adv", "0"]))["balances"][0]["total"]
        if b < 50:
            C.sh([ctl, "sendfrom", "default", C.sh([ctl, "getnewaddress", "adv"]), "200"])
    time.sleep(4); C.mine(2); time.sleep(6)


def swap(p, mid):
    for r in p.swaps():
        if r["match"]["id"] == mid:
            return r
    return None


def wait(p, mid, pred, what, timeout):
    end = time.time() + timeout
    r = None
    while time.time() < end:
        try:
            r = swap(p, mid)
            if r and pred(r):
                return r
        except Exception:
            pass
        time.sleep(2)
    raise RuntimeError("%s %s: timeout waiting for %s; have %s" % (p.name, mid[:16], what, r and (r["status"], r.get("reason"))))


def trade(P, maker, taker, lots=1):
    sell = True
    o = P[maker].dbg("/mesh/order", {"market": MK, "sell": sell, "qty": lots * LOT, "lotSize": LOT,
                                       "rate": C.RATE, "lifetimeSec": 120})
    end = time.time() + 20
    while time.time() < end:
        if any(x.get("id") == o["id"] for x in (P[taker].dbg("/mesh/book?market=" + MK).get("orders") or [])):
            break
        time.sleep(0.5)
    r = P[taker].dbg("/mesh/take", {"market": MK, "sell": not sell, "qty": lots * LOT, "rate": C.RATE, "lotSize": LOT,
                                     "lifetimeSec": 60}, timeout=30)
    assert len(r.get("proposed") or []) == 1, r
    return r["proposed"][0]


def main():
    if os.path.isdir(C.RUN):
        os.rename(C.RUN, C.RUN + "." + datetime.datetime.now().strftime("%Y%m%d-%H%M%S"))
    os.makedirs(C.RUN)
    C.LOGF = open(os.path.join(C.RUN, "adversarial.log"), "a")
    log = C.log
    C.mesh("start")
    setup_wallets()
    P = {n: C.Peer(n, c) for n, c in C.PEERS.items()}
    C.PEER_OBJS.update(P)
    for p in P.values():
        p.start()
    for p in P.values():
        p.init()
        c = p.cfg
        p.new_wallet(0, "bitcoindRPC", "", C.conf("%s/btc/alpha/alpha.conf" % C.DEXTEST, walletname=c["btc"]))
        w, acct = c["dcr"]
        p.new_wallet(42, "dcrwalletRPC", "abc", C.conf("%s/dcr/%s/%s.conf" % (C.DEXTEST, w, w), account=acct))
    for p in P.values():
        p.connect(0)
    A, B = P["A"], P["B"]
    idB = B.dbg("/mesh/id")["peerID"]
    b0 = C.balances()
    log("start balances", {k: str(v) for k, v in b0.items()})
    log("policy", A.dbg("/mesh/policy?market=" + MK))
    cases = {}

    # --- A as a malicious maker ---
    for mode in ("value", "secrethash", "locktime", "recipient"):
        A.dbg("/mesh/swap/tamper", {"mode": mode})
        mid = trade(P, "A", "B")
        wait(A, mid, lambda r: r["status"] == "init_sent", "A init_sent", 90)
        cases["maker-" + mode] = mid
        log("maker-%s: match %s, A initiated a wrong contract" % (mode, mid[:16]))
    A.dbg("/mesh/swap/tamper", {"mode": ""})
    # --- A as a malicious taker ---
    for mode in ("value", "secrethash", "locktime", "recipient"):
        A.dbg("/mesh/swap/tamper", {"mode": mode})
        mid = trade(P, "B", "A")
        wait(A, mid, lambda r: r["status"] == "part_sent", "A part_sent", 120)
        cases["taker-" + mode] = mid
        log("taker-%s: match %s, A participated with a wrong contract" % (mode, mid[:16]))
    A.dbg("/mesh/swap/tamper", {"mode": ""})
    # --- maker never redeems, taker never participates ---
    A.dbg("/mesh/swap/halt", {"stage": "redeem"})
    mid = trade(P, "A", "B")
    wait(B, mid, lambda r: r["status"] == "part_sent", "B part_sent", 120)
    cases["never-redeem"] = mid
    log("never-redeem: match %s, B participated, A halted before redeem" % mid[:16])
    B.dbg("/mesh/swap/halt", {"stage": "participate"})
    mid = trade(P, "A", "B")
    wait(B, mid, lambda r: r["status"] == "init_audited", "B init_audited", 120)
    cases["never-part"] = mid
    log("never-part: match %s, A initiated, B halted before participating" % mid[:16])

    expect = {
        **{"maker-" + m: (lambda a, b, m=m: (a["status"] == "refunded" and b["status"] == "abandoned"
                                             and not b.get("takerSwap"))) for m in ("value", "secrethash", "locktime", "recipient")},
        **{"taker-" + m: (lambda a, b: (a["status"] == "refunded" and b["status"] == "refunded"
                                         and not b.get("makerRedeem"))) for m in ("value", "secrethash", "locktime", "recipient")},
        "never-redeem": lambda a, b: a["status"] == "refunded" and b["status"] == "refunded",
        "never-part": lambda a, b: a["status"] == "refunded" and b["status"] in ("abandoned",) and not b.get("takerSwap"),
    }
    reasons = {"maker-value": "maker contract rejected: value", "maker-secrethash": "maker contract rejected: secret hash",
               "maker-locktime": "maker contract rejected: lock time", "maker-recipient": "maker contract rejected: recipient",
               "taker-value": "taker contract rejected: value", "taker-secrethash": "taker contract rejected: secret hash",
               "taker-locktime": "taker contract rejected: lock time", "taker-recipient": "taker contract rejected: recipient"}
    # Honest side must report the rejection reason while the swap is live.
    for name, sub in reasons.items():
        honest = B
        try:
            r = wait(honest, cases[name], lambda r: sub in (r.get("reason") or "") or any(sub in e["msg"] for e in r["events"]), "reason " + sub, 60)
            RESULTS.append((name + " rejected", True, sub))
        except Exception as e:
            RESULTS.append((name + " rejected", False, str(e)))
    # Let everything resolve (maker lock 300s, plus confirmations).
    end = time.time() + 900
    while time.time() < end:
        done = True
        for mid in cases.values():
            for p in (A, B):
                r = swap(p, mid)
                if not r or r["status"] not in ("complete", "refunded", "abandoned"):
                    done = False
        if done:
            break
        if B.dbg("/mesh/swaps") and time.time() % 60 < 3:
            log("waiting for refunds:", {n: (swap(A, m)["status"], swap(B, m)["status"]) for n, m in cases.items()})
        if all(swap(B, cases["never-part"])["status"] == s for s in ("init_audited",)) and swap(A, cases["never-part"])["status"] == "refunded":
            B.dbg("/mesh/swap/halt", {"stage": ""})  # too late now: B must not participate
        time.sleep(3)
    B.dbg("/mesh/swap/halt", {"stage": ""})
    time.sleep(15)
    for name, mid in cases.items():
        a, b = swap(A, mid), swap(B, mid)
        ok = bool(a and b and expect[name](a, b))
        RESULTS.append((name + " outcome", ok, "A %s / B %s" % (a and a["status"], b and b["status"])))
        log("%-18s A=%-9s B=%-9s %s" % (name, a and a["status"], b and b["status"], "ok" if ok else "BAD"))
    # --- floods during an honest swap ---
    A.dbg("/mesh/swap/halt", {"stage": ""})
    d0 = B.dbg("/mesh/inbox/dropped")["dropped"]
    mid = trade(P, "A", "B")
    fl = {}
    fl["replay"] = A.dbg("/mesh/adversary/flood", {"peer": idB, "mode": "replay", "n": 500}, timeout=120)
    fl["junk"] = A.dbg("/mesh/adversary/flood", {"peer": idB, "mode": "junk", "n": 500, "size": 900}, timeout=120)
    fl["oversize"] = A.dbg("/mesh/adversary/flood", {"peer": idB, "mode": "junk", "n": 5, "size": 4096}, timeout=60)
    log("floods from A to B:", fl)
    try:
        wait(A, mid, lambda r: r["status"] == "complete", "A complete", 240)
        wait(B, mid, lambda r: r["status"] == "complete", "B complete", 120)
        ok = True
    except Exception as e:
        ok = False; log(e)
    d1 = B.dbg("/mesh/inbox/dropped")["dropped"]
    log("B dropped %d inbox messages during the flood" % (d1 - d0))
    RESULTS.append(("flood: swap completes", ok, "B dropped %d (rate limit/replay); oversize %s" % (d1 - d0, fl["oversize"])))
    RESULTS.append(("flood: replays/junk dropped", d1 - d0 >= 400, "dropped %d" % (d1 - d0)))
    cases["flood-honest"] = mid
    # --- accounting ---
    for _ in range(5):
        time.sleep(6); C.mine(2); time.sleep(6)
        b1 = C.balances()
        res = C.verify(P, b0, b1)
        if all(a["ok"] for a in res["accounting"]):
            break
    for a in res["accounting"]:
        log("  %-6s expected %+.8f actual %+.8f fees %.8f txs %d %s" % (a["wallet"], Decimal(a["expected"]), Decimal(a["actual"]),
                                                                        Decimal(a["fees"]), a["txs"], "ok" if a["ok"] else "BAD"))
    RESULTS.append(("balances = records less fees", all(a["ok"] for a in res["accounting"]), ""))
    RESULTS.append(("no pairwise loss", not res["loss"], str(res["loss"])))
    for n, p in P.items():
        with open(os.path.join(C.RUN, "swaps-%s.json" % n), "w") as f:
            json.dump(p.swaps(), f, indent=1)
    with open(os.path.join(C.RUN, "result.json"), "w") as f:
        json.dump(dict(cases=cases, results=RESULTS, verify=res, floods=fl), f, indent=1, default=str)
    for name, ok, info in RESULTS:
        log("%s %-34s %s" % ("PASS" if ok else "FAIL", name, info))
    allok = all(ok for _, ok, _ in RESULTS)
    log("PASS" if allok else "FAIL")
    return 0 if allok else 1


if __name__ == "__main__":
    try:
        sys.exit(main())
    finally:
        for p in C.PEER_OBJS.values():
            p.kill()
