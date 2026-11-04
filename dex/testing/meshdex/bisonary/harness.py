#!/usr/bin/env python3
"""SIMNET/REGTEST ONLY. Shared harness for bisonary mesh tests (S0-S2 and later).

- Bisonw: start/stop/init/login one bisonw-tatanka instance, its web API and
  the mesh debug API (TATANKA_MESH_DEBUG_ADDR).
- Chains: the dex/testing dcr (simnet), btc (regtest), lbc (regtest) and
  polygon (dev) harnesses in ~/dextest: mining and sends from alpha.
- Realistic funding (Rouse 2026-11-02): every test trader wallet gets
  1-5 million LBC (varied per wallet), about 200 DCR and about 0.355 BTC, as
  native SPV wallets funded from the harness alpha wallets. LBC is sent in
  chunks because the alpha wallet's coins are many small coinbase outputs.
- Prices: real-world USD prices (CoinGecko, Kraken fallback, cached) turned
  into mesh message rates per market.
"""
import json, os, random, shutil, subprocess, threading, time, urllib.request, urllib.error
from http.cookiejar import CookieJar

BIN = os.environ.get("BIN", "/workspace/tatanka-dev/bin")
DEXTEST = os.environ.get("DEXTEST", os.path.expanduser("~/dextest"))
os.environ["PATH"] = BIN + ":" + os.environ["PATH"]
CTL = {a: os.path.join(DEXTEST, a, "harness-ctl") for a in ("dcr", "btc", "lbc", "polygon")}
ASSET = {"dcr": 42, "btc": 0, "lbc": 140, "polygon": 966, "usdc.polygon": 966001}
DECIMALS = {"dcr": 8, "btc": 8, "lbc": 8, "polygon": 9, "usdc.polygon": 6}

# Realistic per-wallet balances.
REALISTIC_DCR = 200
REALISTIC_BTC = 0.355
REALISTIC_LBC = [1_000_000, 2_500_000, 5_000_000, 3_500_000, 1_500_000, 4_000_000]
LBC_CHUNK = 50_000

# Lot sizes (base atoms), as client/mesh/markets.go.
LOTS = {"dcr_btc": 10**8, "lbc_btc": 100_000 * 10**8, "lbc_dcr": 1_000 * 10**8,
        "dcr_usdc.polygon": 10**8, "lbc_usdc.polygon": 5_000 * 10**8}

def log(*a):
    print(time.strftime("%H:%M:%S"), *a, flush=True)

class Fail(Exception):
    pass

def run(cmd, cwd=None, timeout=120, check=True):
    r = subprocess.run(cmd, cwd=cwd, capture_output=True, text=True, timeout=timeout, shell=isinstance(cmd, str))
    if check and r.returncode != 0:
        raise Fail("%s: %s" % (cmd if isinstance(cmd, str) else " ".join(cmd), (r.stderr or r.stdout).strip()[-400:]))
    return r.stdout.strip()

def jrpc(out):
    try:
        d = json.loads(out)
    except ValueError:
        return out  # bare results such as a txid
    if isinstance(d, dict) and "result" in d and "jsonrpc" in d:
        if d.get("error"):
            raise Fail(str(d["error"]))
        return d["result"]
    return d

# ---------------- chains ----------------
_mine_lock = threading.Lock()

def mine(asset, n=1):
    with _mine_lock:
        run([os.path.join(CTL[asset], "mine-alpha"), str(n)], check=False, timeout=600)

def mine_all(n=1):
    for a in ("dcr", "btc", "lbc"):
        mine(a, n)

class Miner(threading.Thread):
    """One dcr, btc and lbc block every `every` seconds (polygon dev mines itself)."""
    def __init__(self, every=3):
        super().__init__(daemon=True)
        self.every, self.stop_ev = every, threading.Event()
    def run(self):
        while not self.stop_ev.is_set():
            try:
                mine_all(1)
            except Exception as e:
                log("miner:", e)
            self.stop_ev.wait(self.every)
    def stop(self):
        self.stop_ev.set()

def alpha(asset, *args):
    return jrpc(run([os.path.join(CTL[asset], "alpha")] + [str(a) for a in args]))

def lbc_alpha_balance():
    return float(alpha("lbc", "getbalance"))

def send(asset, addr, amount):
    if asset == "lbc":
        run([os.path.join(CTL["lbc"], "alpha"), "walletpassphrase", "abc", "600"], check=False)
    return alpha(asset, "sendtoaddress", addr, amount)

def send_lbc_chunked(addr, amount, chunk=LBC_CHUNK):
    txs, left = [], amount
    while left > 0:
        amt = min(chunk, left)
        txs.append(send("lbc", addr, amt))
        left -= amt
        if len(txs) % 20 == 0:
            mine("lbc", 1)  # keep the mempool chains short
    return txs

def ensure_lbc_supply(need):
    """Mine LBC regtest blocks until alpha can pay `need` LBC. Regtest pays 1 LBC
    per block to height 5100, then 1+(h-5001)/100 up to ~500 at 55000."""
    bal = lbc_alpha_balance()
    while bal < need:
        h = int(jrpc(run([os.path.join(CTL["lbc"], "alpha-node"), "getblockcount"])))
        log("lbc alpha has %.0f LBC, need %.0f; mining 1000 blocks from height %d" % (bal, need, h))
        mine("lbc", 1000)
        bal = lbc_alpha_balance()
    return bal

def polygon(cmd, *args):
    return run([os.path.join(CTL["polygon"], cmd)] + [str(a) for a in args], cwd=CTL["polygon"])

# ---------------- prices ----------------
PRICE_CACHE = "/workspace/tatanka-dev/run/price-cache.json"

def fetch_prices():
    """USD prices for dcr, btc, lbc, usdc. CoinGecko, then Kraken (dcr, btc), then cache."""
    p = {}
    try:
        u = "https://api.coingecko.com/api/v3/simple/price?ids=decred,bitcoin,lbry-credits,usd-coin&vs_currencies=usd"
        d = json.load(urllib.request.urlopen(u, timeout=15))
        p = {"dcr": d["decred"]["usd"], "btc": d["bitcoin"]["usd"], "lbc": d["lbry-credits"]["usd"], "usdc": d["usd-coin"]["usd"],
             "source": "coingecko", "time": int(time.time())}
    except Exception as e:
        log("coingecko:", e)
        try:
            d = json.load(urllib.request.urlopen("https://api.kraken.com/0/public/Ticker?pair=DCRUSD,XBTUSD", timeout=15))["result"]
            c = json.load(open(PRICE_CACHE)) if os.path.exists(PRICE_CACHE) else {"lbc": 0.0005, "usdc": 1.0}
            p = {"dcr": float(d["DCRUSD"]["c"][0]), "btc": float(d["XXBTZUSD"]["c"][0]), "lbc": c["lbc"], "usdc": c.get("usdc", 1.0),
                 "source": "kraken+cache", "time": int(time.time())}
        except Exception as e2:
            log("kraken:", e2)
    if p:
        os.makedirs(os.path.dirname(PRICE_CACHE), exist_ok=True)
        json.dump(p, open(PRICE_CACHE, "w"))
        return p
    p = json.load(open(PRICE_CACHE))
    p["source"] = "cache"
    return p

def market_assets(mkt):
    b, q = mkt.split("_", 1)
    return b, q

def usd(prices, sym):
    return prices["usdc"] if sym.startswith("usdc") else prices[sym]

# Price steps (message-rate units), as RateStep in client/mesh/markets.go.
# lbc_btc: whole sats per LBC. 1 sat is the minimum and the step (next price
# up: 2 sat); a satoshi isn't divisible, so there is no sub-sat price.
RATE_STEP = {"lbc_btc": 1}

def rate_step(mkt):
    return RATE_STEP.get(mkt, 1)

def at_floor(mkt, rate):
    """True when rate is at the market's minimum price (one step): a one-price
    market, e.g. lbc_btc while LBC is below 1 sat."""
    return rate <= rate_step(mkt)

def rate_for(prices, mkt, factor=1.0):
    """Mesh message rate (quote atoms per 1e8 base atoms = quote atoms per base
    coin), rounded to a whole number of the market's price steps and at least
    one step. lbc_btc at the real price (about 0.65 sat per LBC) is 1 sat."""
    b, q = market_assets(mkt)
    price = usd(prices, b) / usd(prices, q) * factor   # quote coins per base coin
    step = rate_step(mkt)
    return max(step, int(round(price * 10 ** DECIMALS[q] / step)) * step)

# ---------------- bisonw ----------------
class Bisonw:
    def __init__(self, name, run_dir, web, dbg, mesh_addr, binary, meshport, extra=None, env=None):
        self.name, self.dir, self.web, self.dbg, self.mesh_addr = name, os.path.join(run_dir, name), web, dbg, mesh_addr
        self.binary, self.meshport, self.extra, self.env = binary, meshport, extra or [], env or {}
        self.proc, self.pw = None, None
        os.makedirs(self.dir, exist_ok=True)
        self.jar = CookieJar()
        self.opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(self.jar))
        pwf = os.path.join(self.dir, ".testpw")
        if os.path.exists(pwf):
            self.pw = open(pwf).read()

    def start(self, extra=None, env=None):
        e = dict(os.environ)
        e.update({"TATANKA_SIMNET_ADDR": self.mesh_addr, "TATANKA_MESH_DEBUG_ADDR": self.dbg})
        e.update(self.env)
        e.update(env or {})
        args = [self.binary, "--simnet", "--mesh", "--appdata", self.dir, "--webaddr", self.web, "--meshport", str(self.meshport),
                "--meshname", self.name, "--log=debug"] + self.extra + (extra or [])
        os.makedirs(self.dir, exist_ok=True)
        self.console = open(os.path.join(self.dir, "console.log"), "a")
        self.console.write("\n==== %s start %s\n" % (time.strftime("%F %T"), " ".join(args[1:])))
        self.console.flush()
        self.proc = subprocess.Popen(args, stdout=self.console, stderr=subprocess.STDOUT, env=e)
        self.wait_http("http://%s/" % self.web)
        return self

    def stop(self, kill=False):
        if self.proc and self.proc.poll() is None:
            (self.proc.kill if kill else self.proc.terminate)()
            try:
                self.proc.wait(30)
            except subprocess.TimeoutExpired:
                self.proc.kill()
                self.proc.wait()
        self.proc = None
        self.jar.clear()

    def alive(self):
        return self.proc is not None and self.proc.poll() is None

    def wait_http(self, url, timeout=90):
        end = time.time() + timeout
        while time.time() < end:
            if self.proc and self.proc.poll() is not None:
                raise Fail("%s exited (code %s); see %s/console.log" % (self.name, self.proc.returncode, self.dir))
            try:
                urllib.request.urlopen(url, timeout=3)
                return
            except urllib.error.HTTPError:
                return
            except Exception:
                time.sleep(0.5)
        raise Fail("%s: %s not up" % (self.name, url))

    def _req(self, base, path, body=None, method=None, opener=None):
        data = None if body is None else (body if isinstance(body, bytes) else json.dumps(body).encode())
        req = urllib.request.Request("http://%s%s" % (base, path), data=data, method=method or ("POST" if data is not None else "GET"),
                                     headers={"Content-Type": "application/json"})
        try:
            r = opener.open(req, timeout=60) if opener else urllib.request.urlopen(req, timeout=60)
            return json.loads(r.read() or b"null")
        except urllib.error.HTTPError as e:
            txt = e.read().decode()
            try:
                return json.loads(txt)
            except Exception:
                raise Fail("%s %s: HTTP %d %s" % (self.name, path, e.code, txt[:300]))

    def api(self, path, body=None):
        return self._req(self.web, "/api/" + path, body, opener=self.opener)

    def dbgapi(self, path, body=None):
        return self._req(self.dbg, path, body)

    def init(self):
        os.makedirs(self.dir, exist_ok=True)
        self.pw = random.randbytes(12).hex()
        open(os.path.join(self.dir, ".testpw"), "w").write(self.pw)
        os.chmod(os.path.join(self.dir, ".testpw"), 0o600)
        r = self.api("init", {"pass": self.pw})
        if not r.get("ok"):
            raise Fail("init %s: %s" % (self.name, r))
        self.wait_mesh()

    def login(self):
        r = self.api("login", {"pass": self.pw})
        if not r.get("ok"):
            raise Fail("login %s: %s" % (self.name, r))
        self.wait_mesh()

    def wait_mesh(self, timeout=90):
        self.wait_http("http://%s/mesh/id" % self.dbg, timeout)
        end = time.time() + timeout
        while time.time() < end:
            st = self.api("mesh/status").get("status") or {}
            if st.get("connected"):
                self.peer_id = st["peerID"]
                return
            time.sleep(1)
        raise Fail("%s not connected to the mesh" % self.name)

    def id(self):
        return self.dbgapi("/mesh/id")["peerID"]

    # wallets
    def new_wallet(self, asset, wtype="SPV", config=None, wpass=""):
        r = self.api("newwallet", {"assetID": ASSET[asset], "walletType": wtype, "config": config or {}, "pass": wpass, "appPass": self.pw})
        if not r.get("ok") and "already have" not in r.get("msg", ""):
            raise Fail("newwallet %s %s: %s" % (self.name, asset, r))

    def deposit_addr(self, asset):
        r = self.api("depositaddress", {"assetID": ASSET[asset]})
        if not r.get("ok"):
            raise Fail("depositaddress %s %s: %s" % (self.name, asset, r))
        return r["address"]

    def balance(self, asset):
        r = self.api("balance", {"assetID": ASSET[asset]})
        if not r.get("ok"):
            return {"available": 0, "locked": 0, "immature": 0}
        return r["balance"]

    def coins(self, asset, field="available"):
        b = self.balance(asset)
        v = b["available"] + b["locked"] + b["immature"] if field == "total" else b[field]
        return v / 10 ** DECIMALS[asset]

    # mesh debug
    def subscribe(self, mkt):
        self.dbgapi("/mesh/subscribe", {"market": mkt})

    def order(self, mkt, sell, lots, rate, lifetime=600, lot=None):
        lot = lot or LOTS[mkt]
        r = self.dbgapi("/mesh/order", {"market": mkt, "sell": sell, "qty": lots * lot, "lotSize": lot, "rate": rate, "lifetimeSec": lifetime})
        if "id" not in r:
            raise Fail("%s order %s: %s" % (self.name, mkt, r))
        return r["id"]

    def cancel(self, oid):
        return self.dbgapi("/mesh/cancel", {"id": oid})

    def own(self):
        return self.dbgapi("/mesh/own").get("orders") or []

    def book(self, mkt):
        return self.dbgapi("/mesh/book?market=" + mkt).get("orders") or []

    def matches(self):
        return self.dbgapi("/mesh/matches").get("matches") or []

    def swaps(self):
        return self.dbgapi("/mesh/swaps").get("swaps") or []

    def swap(self, mid):
        for s in self.swaps():
            if s["match"]["id"] == mid:
                return s
        return None

    def epochs(self):
        return self.dbgapi("/mesh/epochs/status")

def fund_realistic(bw, lbc, dcr=REALISTIC_DCR, btc=REALISTIC_BTC, polygon_usdc=None):
    """Fund bw's SPV wallets: lbc LBC (chunked), dcr DCR, btc BTC; optionally POL + USDC."""
    out = {"lbc": []}
    if lbc > 0:
        out["lbc"] = send_lbc_chunked(bw.deposit_addr("lbc"), lbc)
    if dcr > 0:
        out["dcr"] = send("dcr", bw.deposit_addr("dcr"), dcr)
    if btc > 0:
        out["btc"] = send("btc", bw.deposit_addr("btc"), btc)
    if polygon_usdc:
        ev = bw.deposit_addr("polygon")
        polygon("sendtoaddress", ev, 10)
        polygon("sendUSDC", ev, polygon_usdc)
        out["polygon"] = ev
    log("%s funded: %s LBC (%d txs), %s DCR, %s BTC%s" % (bw.name, f"{lbc:,}", len(out["lbc"]), dcr, btc,
                                                         ", 10 POL + %s USDC" % polygon_usdc if polygon_usdc else ""))
    return out

def wait_funded(bw, need, timeout=600):
    """need: {asset: min available coins}"""
    end = time.time() + timeout
    while time.time() < end:
        have = {a: bw.coins(a) for a in need}
        if all(have[a] >= need[a] for a in need):
            log("%s balances: %s" % (bw.name, ", ".join("%s %s" % (f"{have[a]:,.4f}", a.upper()) for a in need)))
            return have
        time.sleep(3)
    raise Fail("%s not funded: have %s need %s" % (bw.name, have, need))

PAUSES = []   # box pauses seen by wait_until: (start, seconds)

def wait_until(fn, timeout, what, every=1.0):
    """Poll fn until it returns something true. A box pause (the whole VM
    stopped, every node and this harness frozen) does not eat the timeout:
    when one poll round took far longer than expected, the deadline moves
    by the gap."""
    end = time.time() + timeout
    last = None
    t_prev = time.time()
    while time.time() < end:
        try:
            last = fn()
            if last:
                return last
        except Fail:
            raise
        except Exception as e:
            last = e
        time.sleep(every)
        now = time.time()
        gap = now - t_prev - every
        if gap > 20 and gap > 4 * every:
            end += gap
            PAUSES.append((time.strftime("%T", time.localtime(t_prev)), round(gap)))
            log("box pause of ~%d s while waiting for %s; deadline extended" % (gap, what))
        t_prev = now
    raise Fail("timeout waiting for %s (last %r)" % (what, last))
