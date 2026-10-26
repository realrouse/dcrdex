#!/usr/bin/env python3
"""SIMNET/REGTEST ONLY. P6 scale/churn/soak campaign for the tatanka mesh DEX.

4 bisonw peers (see p6-setup.sh for wallets) on a private 5-node mesh
(mesh-p4.sh with SESSION=tatanka-p6 N=5 PORT_BASE=13445), trading dcr_btc,
lbc_btc and lbc_dcr. Phases:
  scale:  bursts of concurrent orders and takes, no churn
  churn:  steady trades while bisonw peers are SIGKILLed/restarted and mesh
          nodes are stopped/restarted at random
  steady: trades with no churn (soak)
  drain:  no new orders; everyone up; wait until every swap is final
Checks: every swap record final on both sides; no party lost funds (pairwise
redeem/refund consistency); per-wallet balance delta equals the expected
amount from the swap records less bounded fees.
Usage: SCALE_MIN=8 CHURN_MIN=45 STEADY_MIN=30 ./p6-campaign.py
"""
import urllib.error
import json, os, random, subprocess, sys, threading, time, urllib.request, shutil, datetime
from http.cookiejar import CookieJar
from decimal import Decimal

HERE = os.path.dirname(os.path.abspath(__file__))
BIN = os.environ.get("BIN", "/workspace/tatanka-dev/bin")
BISONW = os.environ.get("BISONW", BIN + "/bisonw-tatanka")
RUN = os.environ.get("RUN", "/workspace/tatanka-dev/run/p6")
DEXTEST = os.environ.get("DEXTEST", os.path.expanduser("~/dextest"))
MESH_ENV = dict(os.environ, SESSION="tatanka-p6", N="5", PORT_BASE="13445",
                ROOT="/workspace/tatanka-dev/run/p6mesh", PATH=BIN + ":" + os.environ["PATH"])
os.environ["PATH"] = BIN + ":" + os.environ["PATH"]  # harness CLIs (bitcoin-cli, dcrctl)
PEER_OBJS = {}
MESHSH = HERE + "/mesh-p4.sh"
SCALE_MIN = float(os.environ.get("SCALE_MIN", 8))
CHURN_MIN = float(os.environ.get("CHURN_MIN", 45))
STEADY_MIN = float(os.environ.get("STEADY_MIN", 30))
DRAIN_MAX_MIN = float(os.environ.get("DRAIN_MAX_MIN", 30))
MAKER_LOCK, TAKER_LOCK = int(os.environ.get("MAKER_LOCK", 480)), int(os.environ.get("TAKER_LOCK", 240))
SEED = int(os.environ.get("SEED", int(time.time())))
rnd = random.Random(SEED)
LOT = 100_000_000
RATE = 1_000_000  # 0.01 quote per base
ASSET = {"dcr": 42, "btc": 0, "lbc": 140}
MARKETS = {"dcr_btc": ("dcr", "btc"), "lbc_btc": ("lbc", "btc"), "lbc_dcr": ("lbc", "dcr")}
BTCCLI = ["bitcoin-cli", "-rpcport=20556", "-regtest=1", "-rpcuser=user", "-rpcpassword=pass"]

PEERS = {
    "p1": dict(web=15821, dbg=15921, node=1, lbc="beta", btc="p6w1", dcr=("trading1", "default"), markets=list(MARKETS)),
    "p2": dict(web=15822, dbg=15922, node=2, lbc="gamma", btc="p6w2", dcr=("trading2", "default"), markets=list(MARKETS)),
    "p3": dict(web=15823, dbg=15923, node=3, lbc=None, btc="p6w3", dcr=("trading1", "p6"), markets=["dcr_btc"]),
    "p4": dict(web=15824, dbg=15924, node=4, lbc=None, btc="p6w4", dcr=("trading2", "p6"), markets=["dcr_btc"]),
}

LOGF = None
lock = threading.Lock()


def log(*a):
    line = datetime.datetime.now().strftime("%H:%M:%S ") + " ".join(str(x) for x in a)
    with lock:
        print(line, flush=True)
        LOGF.write(line + "\n"); LOGF.flush()


def sh(cmd, env=None, check=True):
    r = subprocess.run(cmd, capture_output=True, text=True, env=env)
    if check and r.returncode != 0:
        raise RuntimeError("%s: %s %s" % (cmd, r.stdout, r.stderr))
    return r.stdout.strip()


def mesh(*args):
    return sh([MESHSH] + [str(a) for a in args], env=MESH_ENV)


def req(port, path, body=None, timeout=15, opener=None):
    data = None if body is None else json.dumps(body).encode()
    rq = urllib.request.Request("http://127.0.0.1:%d%s" % (port, path), data=data,
                                 headers={"Content-Type": "application/json"})
    o = opener or urllib.request.build_opener()
    with o.open(rq, timeout=timeout) as resp:
        return json.loads(resp.read() or b"null")


class Peer:
    def __init__(self, name, cfg):
        self.name, self.cfg = name, cfg
        self.dir = os.path.join(RUN, name)
        os.makedirs(self.dir, exist_ok=True)
        self.proc = None
        self.up = False
        self.opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(CookieJar()))
        self.pw = None
        self.restarts = 0

    def start(self):
        env = dict(os.environ, TATANKA_SIMNET_ADDR=mesh("addr", self.cfg["node"]),
                   TATANKA_MESH_DEBUG_ADDR="127.0.0.1:%d" % self.cfg["dbg"])
        out = open(os.path.join(self.dir, "console.log"), "a")
        self.proc = subprocess.Popen([BISONW, "--simnet", "--mesh", "--appdata", self.dir,
                                      "--webaddr", "127.0.0.1:%d" % self.cfg["web"], "--log=debug"],
                                     stdout=out, stderr=subprocess.STDOUT, env=env)
        self.wait(None, "web", 90, path=True)

    def wait(self, f, what, timeout, path=False):
        end = time.time() + timeout
        while time.time() < end:
            try:
                if path:
                    try:
                        urllib.request.urlopen("http://127.0.0.1:%d/" % self.cfg["web"], timeout=3)
                    except urllib.error.HTTPError:
                        pass  # any HTTP answer means the server is up
                    return
                if f():
                    return
            except Exception:
                pass
            time.sleep(1)
        raise RuntimeError("%s: timeout waiting for %s" % (self.name, what))

    def streams(self):
        try:
            return open(os.path.join(self.dir, "console.log"), errors="replace").read().count("push stream established")
        except FileNotFoundError:
            return 0

    def init(self):
        self.pw = os.urandom(9).hex()
        p = os.path.join(self.dir, ".testpw")
        with open(p, "w") as f:
            f.write(self.pw)
        os.chmod(p, 0o600)
        r = req(self.cfg["web"], "/api/init", {"pass": self.pw}, opener=self.opener)
        assert r["ok"], r

    def login(self):
        r = req(self.cfg["web"], "/api/login", {"pass": self.pw}, opener=self.opener, timeout=60)
        assert r["ok"], r

    def dbg(self, path, body=None, timeout=15):
        return req(self.cfg["dbg"], path, body, timeout=timeout)

    def configure(self):
        for mk in self.cfg["markets"]:
            self.dbg("/mesh/subscribe", {"market": mk})
            self.dbg("/mesh/config", {"market": mk, "makerLockSec": MAKER_LOCK, "takerLockSec": TAKER_LOCK})

    def connect(self, n0):
        self.wait(lambda: self.dbg("/mesh/id"), "debug API", 60)
        self.wait(lambda: self.streams() > n0, "mesh stream", 90)
        self.configure()
        self.up = True

    def new_wallet(self, asset, wtype, wpass, cfg):
        body = {"assetID": asset, "walletType": wtype, "config": cfg, "pass": wpass, "appPass": self.pw}
        r = req(self.cfg["web"], "/api/newwallet", body, opener=self.opener, timeout=90)
        assert r.get("ok"), (self.name, asset, r)

    def kill(self):
        self.up = False
        if self.proc:
            self.proc.kill(); self.proc.wait()
        self.proc = None

    def restart(self):
        n0 = self.streams()
        self.start(); self.login(); self.connect(n0)
        self.restarts += 1

    def swaps(self):
        return self.dbg("/mesh/swaps", timeout=30)["swaps"] or []


def conf(path, **kv):
    cfg = {}
    for line in open(path):
        line = line.strip()
        if line and not line.startswith(("#", ";", "[")) and "=" in line:
            k, v = line.split("=", 1); cfg[k.strip()] = v.strip()
    cfg.update({k: str(v) for k, v in kv.items()})
    return cfg


# ---------- balances ----------
def bal(peer, a):
    c = PEERS[peer]
    if a == "btc":
        m = json.loads(sh(BTCCLI + ["-rpcwallet=" + c["btc"], "getbalances"]), parse_float=Decimal)["mine"]
        return m["trusted"] + m["untrusted_pending"]
    if a == "dcr":
        w, acct = c["dcr"]
        d = json.loads(sh([DEXTEST + "/dcr/harness-ctl/" + w, "getbalance", acct, "0"]))
        return Decimal(str(d["balances"][0]["total"]))
    if a == "lbc":
        d = json.loads(sh([DEXTEST + "/lbc/harness-ctl/" + c["lbc"], "getbalance", '["*", 0]']))
        return Decimal(str(d["result"]))


def balances():
    out = {}
    for p, c in PEERS.items():
        for a in ("btc", "dcr", "lbc"):
            if a == "lbc" and not c["lbc"]:
                continue
            out["%s/%s" % (p, a)] = bal(p, a)
    return out


def mine(n=1):
    for chain in ("dcr", "btc", "lbc"):
        subprocess.run([DEXTEST + "/%s/harness-ctl/mine-alpha" % chain, str(n)], capture_output=True)


STOP = threading.Event()


def miner():
    while not STOP.is_set():
        mine(1)
        STOP.wait(3)


# ---------- trading ----------
STATS = dict(orders=0, takes=0, proposed=0, take_err=0, order_err=0, kills_bw=0, kills_mesh=0)
TRADING = threading.Event()


def peers_for(mk):
    return [p for p, c in PEERS.items() if mk in c["markets"]]


def one_trade(P, mk, maker, taker, lots, tag):
    sell = rnd.random() < 0.5
    rate = RATE + rnd.randint(-50, 50) * 1000
    try:
        o = P[maker].dbg("/mesh/order", {"market": mk, "sell": sell, "qty": lots * LOT, "lotSize": LOT,
                                          "rate": rate, "lifetimeSec": 90})
        STATS["orders"] += 1
    except Exception as e:
        STATS["order_err"] += 1
        log("order err", maker, mk, e)
        return
    oid = o["id"]
    end = time.time() + 15
    while time.time() < end:
        try:
            b = P[taker].dbg("/mesh/book?market=" + mk)
            if any(x.get("id") == oid for x in (b.get("orders") or [])):
                break
        except Exception:
            pass
        time.sleep(0.5)
    try:
        r = P[taker].dbg("/mesh/take", {"market": mk, "sell": not sell, "qty": lots * LOT, "rate": rate,
                                         "lotSize": LOT, "lifetimeSec": 60}, timeout=30)
        STATS["takes"] += 1
        STATS["proposed"] += len(r.get("proposed") or [])
        log("%s %s %s %s(%s) <- %s lots=%d rate=%d proposed=%d" % (tag, mk, oid[:8], maker, "sell" if sell else "buy",
                                                                   taker, lots, rate, len(r.get("proposed") or [])))
    except Exception as e:
        STATS["take_err"] += 1
        log("take err", taker, mk, e)


def trader(P, burst):
    while TRADING.is_set() and not STOP.is_set():
        mk = rnd.choice(list(MARKETS))
        live = [p for p in peers_for(mk) if P[p].up]
        if len(live) < 2:
            time.sleep(1); continue
        if burst.is_set():
            # several concurrent trades across markets
            ts = []
            for _ in range(rnd.randint(3, 6)):
                mk2 = rnd.choice(list(MARKETS))
                live2 = [p for p in peers_for(mk2) if P[p].up]
                if len(live2) < 2:
                    continue
                m, t = rnd.sample(live2, 2)
                th = threading.Thread(target=one_trade, args=(P, mk2, m, t, rnd.randint(1, 3), "burst"))
                th.start(); ts.append(th)
            for th in ts:
                th.join()
            time.sleep(rnd.uniform(4, 8))
        else:
            m, t = rnd.sample(live, 2)
            one_trade(P, mk, m, t, rnd.randint(1, 3), "trade")
            time.sleep(rnd.uniform(3, 7))


def churner(P, active):
    mesh_down = set()
    while not STOP.is_set():
        if not active.is_set():
            time.sleep(1); continue
        time.sleep(rnd.uniform(40, 100))
        if not active.is_set():
            continue
        if rnd.random() < 0.7:
            name = rnd.choice([p for p in P if P[p].up] or [None])
            if not name:
                continue
            # Mostly short crashes; some outages outlast the taker lock time
            # so counterparties (and the crashed peer, on return) refund.
            down = rnd.uniform(3, 45) if rnd.random() < 0.8 else rnd.uniform(TAKER_LOCK + 10, MAKER_LOCK - 60)
            log("CHURN kill -9 bisonw %s for %.0fs" % (name, down))
            P[name].kill(); STATS["kills_bw"] += 1
            time.sleep(down)
            try:
                P[name].restart()
                log("CHURN %s restarted, logged in, re-subscribed" % name)
            except Exception as e:
                log("CHURN restart %s failed: %s" % (name, e))
                FAILS.append("restart %s: %s" % (name, e))
        else:
            up = [i for i in range(1, 6) if i not in mesh_down]
            if len(up) <= 3:
                continue
            i = rnd.choice(up)
            down = rnd.uniform(10, 60)
            log("CHURN stop mesh node-%d for %.0fs" % (i, down))
            mesh("kill", i); STATS["kills_mesh"] += 1; mesh_down.add(i)
            time.sleep(down)
            mesh("restart", i); mesh_down.discard(i)
            log("CHURN mesh node-%d restarted" % i)


FAILS = []


# ---------- verification ----------
def amounts(m):
    base = m["qty"]
    quote = m["qty"] * m["rate"] // LOT
    return base, quote


def verify(P, b0, b1):
    recs = {}
    for name, p in P.items():
        for r in p.swaps():
            recs.setdefault(r["match"]["id"], {})[name] = r
    statuses = {}
    nonfinal, loss, onesided = [], [], []
    expected = {k: Decimal(0) for k in b0}
    ntx = {k: 0 for k in b0}
    for mid, sides in recs.items():
        mk = None
        for name, r in sides.items():
            statuses[r["status"]] = statuses.get(r["status"], 0) + 1
            if r["status"] not in ("complete", "refunded", "abandoned"):
                nonfinal.append((mid[:16], name, r["status"], r.get("reason", "")))
            m = r["match"]
            b, q = amounts(m)
            mkt = "%s_%s" % ({42: "dcr", 0: "btc", 140: "lbc"}[m["market"]["base"]], {42: "dcr", 0: "btc", 140: "lbc"}[m["market"]["quote"]])
            ba, qa = MARKETS[mkt]
            maker = r["side"] == "maker"
            # what this side sends / receives
            sends_base = (maker and m["makerSell"]) or (not maker and not m["makerSell"])
            sa, sv, ra, rv = (ba, b, qa, q) if sends_base else (qa, q, ba, b)
            own, own_ref = ("makerSwap", "makerRefund") if maker else ("takerSwap", "takerRefund")
            own_red = "makerRedeem" if maker else "takerRedeem"
            if r.get(own):
                ntx["%s/%s" % (name, sa)] += 1
                if not r.get(own_ref):
                    expected["%s/%s" % (name, sa)] -= Decimal(sv) / LOT
                else:
                    ntx["%s/%s" % (name, sa)] += 1
            if r.get(own_red):
                expected["%s/%s" % (name, ra)] += Decimal(rv) / LOT
                ntx["%s/%s" % (name, ra)] += 1
        if len(sides) < 2:
            onesided.append((mid[:16], list(sides), [s["status"] for s in sides.values()]))
        mr = next((s for s in sides.values() if s["side"] == "maker"), None)
        tr = next((s for s in sides.values() if s["side"] == "taker"), None)
        if mr and tr:
            # taker redeemed maker's contract (maker paid) but maker didn't get the taker's coins
            if tr.get("takerRedeem") and not mr.get("makerRedeem"):
                loss.append((mid[:16], "maker lost", mr["status"], tr["status"]))
            if mr.get("makerRedeem") and not tr.get("takerRedeem"):
                loss.append((mid[:16], "taker lost", mr["status"], tr["status"]))
            if mr.get("makerRedeem") and tr.get("takerRefund"):
                loss.append((mid[:16], "taker refunded a redeemed contract?", mr["status"], tr["status"]))
    acct = []
    for k in b0:
        a = k.split("/")[1]
        tol = Decimal("0.002") if a == "dcr" else Decimal("0.001")
        actual = b1[k] - b0[k]
        fee = expected[k] - actual
        ok = Decimal("-0.00000001") <= fee <= tol * max(ntx[k], 1)
        acct.append(dict(wallet=k, start=str(b0[k]), end=str(b1[k]), actual=str(actual), expected=str(expected[k]),
                         fees=str(fee), txs=ntx[k], ok=ok))
    return dict(swaps=len(recs), statuses=statuses, nonfinal=nonfinal, loss=loss, onesided=onesided, accounting=acct)


def all_final(P):
    n = 0
    for p in P.values():
        try:
            for r in p.swaps():
                if r["status"] not in ("complete", "refunded", "abandoned"):
                    n += 1
        except Exception:
            n += 1000
    return n


def main():
    global LOGF
    if os.path.isdir(RUN):
        shutil.move(RUN, RUN + "." + datetime.datetime.now().strftime("%Y%m%d-%H%M%S"))  # never delete appdata
    os.makedirs(RUN)
    LOGF = open(os.path.join(RUN, "campaign.log"), "a")
    log("P6 campaign seed=%d scale=%smin churn=%smin steady=%smin locks=%d/%d" % (SEED, SCALE_MIN, CHURN_MIN, STEADY_MIN, MAKER_LOCK, TAKER_LOCK))
    log("mesh nodes:", [mesh("up", i) for i in range(1, 6)])
    P = {n: Peer(n, c) for n, c in PEERS.items()}
    PEER_OBJS.update(P)
    for p in P.values():
        p.start()
    for p in P.values():
        p.init()
    for p in P.values():
        c = p.cfg
        if c["lbc"]:
            p.new_wallet(140, "lbcwalletRPC", "abc", conf("%s/lbc/%s/%s.conf" % (DEXTEST, c["lbc"], c["lbc"])))
        p.new_wallet(0, "bitcoindRPC", "", conf("%s/btc/alpha/alpha.conf" % DEXTEST, walletname=c["btc"]))
        w, acct = c["dcr"]
        p.new_wallet(42, "dcrwalletRPC", "abc", conf("%s/dcr/%s/%s.conf" % (DEXTEST, w, w), account=acct))
    for p in P.values():
        p.connect(0)
        log(p.name, "up: node-%d, markets %s, policy %s" % (p.cfg["node"], p.cfg["markets"], p.dbg("/mesh/policy?market=dcr_btc")))
    mine(2); time.sleep(8)
    b0 = balances()
    log("start balances", {k: str(v) for k, v in b0.items()})
    mt = threading.Thread(target=miner, daemon=True); mt.start()
    burst, churn = threading.Event(), threading.Event()
    TRADING.set()
    traders = [threading.Thread(target=trader, args=(P, burst), daemon=True) for _ in range(2)]
    ct = threading.Thread(target=churner, args=(P, churn), daemon=True); ct.start()
    t0 = time.time()
    log("== phase scale (bursts of 3-6 concurrent trades, 2 traders)"); burst.set()
    for t in traders:
        t.start()
    time.sleep(SCALE_MIN * 60)
    burst.clear(); churn.set(); log("== phase churn (bisonw kill -9 / mesh node stops)")
    time.sleep(CHURN_MIN * 60)
    churn.clear(); log("== phase steady (no churn)")
    # wait for an in-flight churn event to finish
    while any(not p.up for p in P.values()):
        time.sleep(2)
    time.sleep(STEADY_MIN * 60)
    TRADING.clear(); log("== drain: no new orders")
    for t in traders:
        t.join(timeout=120)
    for p in P.values():
        if not p.up:
            p.restart()
    end = time.time() + DRAIN_MAX_MIN * 60
    while time.time() < end:
        n = all_final(P)
        if n == 0:
            break
        log("drain: %d swap records not final" % n)
        time.sleep(30)
    STOP.set(); mt.join(timeout=30)
    b1 = None
    for _ in range(5):  # settle: mine and let relay catch up
        time.sleep(6); mine(2); time.sleep(6)
        b1 = balances()
        res = verify(P, b0, b1)
        if all(a["ok"] for a in res["accounting"]):
            break
    res["stats"] = STATS
    res["elapsed_min"] = round((time.time() - t0) / 60, 1)
    res["restarts"] = {n: p.restarts for n, p in P.items()}
    res["fails"] = FAILS
    res["seed"] = SEED
    res["inbox_dropped"] = {n: p.dbg("/mesh/inbox/dropped")["dropped"] for n, p in P.items()}
    for n, p in P.items():
        with open(os.path.join(RUN, "swaps-%s.json" % n), "w") as f:
            json.dump(p.swaps(), f, indent=1)
    ok = not res["nonfinal"] and not res["loss"] and all(a["ok"] for a in res["accounting"]) and not FAILS
    res["pass"] = ok
    with open(os.path.join(RUN, "result.json"), "w") as f:
        json.dump(res, f, indent=1, default=str)
    log("RESULT", json.dumps({k: res[k] for k in ("swaps", "statuses", "loss", "nonfinal", "stats", "restarts", "elapsed_min", "inbox_dropped")}, default=str))
    for a in res["accounting"]:
        log("  %-8s expected %+.8f actual %+.8f fees %.8f txs %d %s" % (a["wallet"], Decimal(a["expected"]), Decimal(a["actual"]), Decimal(a["fees"]), a["txs"], "ok" if a["ok"] else "BAD"))
    log("PASS" if ok else "FAIL")
    for p in P.values():
        p.kill()
    return 0 if ok else 1


if __name__ == "__main__":
    try:
        sys.exit(main())
    finally:
        STOP.set()
        for p in PEER_OBJS.values():  # never leave bisonw processes behind
            p.kill()
