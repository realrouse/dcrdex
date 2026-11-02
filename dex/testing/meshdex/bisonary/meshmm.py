#!/usr/bin/env python3
"""SIMNET ONLY. Book seeding and a basic market maker for mesh markets.

bisonw's own market maker (client/mm) only trades on DEX server markets; it
has no mesh host support yet. MeshMM is a harness-side stand-in for the
basic (gap) strategy: it keeps `levels` bids and asks around the real-world
mid price (harness.fetch_prices, refreshed every few minutes) through the
bisonw mesh debug API, and re-quotes when the price moves, an order fills or
it is about to expire.
"""
import random, threading, time
import harness as h

def ladder(mid_rate, side_sell, n, gap, step):
    """n rates on one side of mid: gap first, then step apart (fractions)."""
    out = []
    for i in range(n):
        f = gap + i * step
        r = mid_rate * (1 + f) if side_sell else mid_rate * (1 - f)
        out.append(max(1, int(round(r))))
    return out

def fix_tiny(bids, asks):
    """Rates near the resolution floor (lbc_btc: 1 sat per LBC) collapse to
    the same integer. Keep bids <= asks - 1 and every rate >= 1."""
    hi_bid = max(bids) if bids else 0
    asks = [max(a, hi_bid + 1) for a in asks]
    # spread equal asks apart
    seen, out = set(), []
    for a in sorted(asks):
        while a in seen:
            a += 1
        seen.add(a)
        out.append(a)
    return bids, out

def seed_book(traders, mkt, prices, per_wallet=(2, 3), max_lots=5, lifetime=1800, budget=None):
    """Several resting bids and asks from each trader at varied rates and sizes
    around the real mid. budget(trader, mkt, sell) -> max lots on that side."""
    mid = h.rate_for(prices, mkt)
    placed = []
    rng = random.Random(hash(mkt) & 0xffff)
    for t in traders:
        for sell in (False, True):
            n = rng.randint(*per_wallet)
            rates = sorted({max(1, int(round(mid * (1 + (1 if sell else -1) * rng.uniform(0.005, 0.04))))) for _ in range(n)})
            if mid < 50:   # below rate resolution: bids at the floor, asks above
                rates = [max(1, mid)] if not sell else [mid + 2 + i for i in range(n)]
            cap = budget(t, mkt, sell) if budget else max_lots * n
            for r in rates:
                lots = min(rng.randint(1, max_lots), cap)
                if lots <= 0:
                    break
                cap -= lots
                oid = t.order(mkt, sell, lots, r, lifetime)
                placed.append((t.name, "ask" if sell else "bid", lots, r, oid))
    return mid, placed

class MeshMM(threading.Thread):
    def __init__(self, bw, mkt, prices_fn, levels=3, gap=0.004, step=0.006, lots=2, lifetime=600, every=10, drift=0.01):
        super().__init__(daemon=True, name="mm-%s-%s" % (bw.name, mkt))
        self.bw, self.mkt, self.prices_fn = bw, mkt, prices_fn
        self.levels, self.gap, self.step, self.lots, self.lifetime, self.every, self.drift = levels, gap, step, lots, lifetime, every, drift
        self.stop_ev = threading.Event()
        self.mid = None
        self.orders = {}   # id -> (sell, rate, expiry)
        self.log = []

    def quote(self):
        mid = h.rate_for(self.prices_fn(), self.mkt)
        bids = ladder(mid, False, self.levels, self.gap, self.step)
        asks = ladder(mid, True, self.levels, self.gap, self.step)
        if mid < 50:
            bids, asks = [max(1, mid)], [mid + 2 + i for i in range(self.levels)]
        bids, asks = fix_tiny(bids, asks)
        return mid, bids, asks

    def refresh(self):
        mid, bids, asks = self.quote()
        own = {o["id"]: o for o in self.bw.own()}
        now = time.time()
        moved = self.mid is None or abs(mid - self.mid) / max(self.mid, 1) > self.drift
        live = {oid: v for oid, v in self.orders.items() if oid in own and v[2] - now > self.every * 3}
        if moved or not self.orders or len(live) != len(self.orders):
            for oid in list(self.orders):
                if oid in own:
                    try:
                        self.bw.cancel(oid)
                    except Exception:
                        pass
            self.orders = {}
            for sell, rates in ((False, bids), (True, asks)):
                for r in rates:
                    try:
                        oid = self.bw.order(self.mkt, sell, self.lots, r, self.lifetime)
                        self.orders[oid] = (sell, r, now + self.lifetime)
                    except Exception as e:
                        self.log.append("order error: %s" % e)
            self.log.append("%s requote mid=%d bids=%s asks=%s" % (time.strftime("%T"), mid, bids, asks))
            self.mid = mid

    def run(self):
        while not self.stop_ev.is_set():
            try:
                self.refresh()
            except Exception as e:
                self.log.append("%s error: %s" % (time.strftime("%T"), e))
            self.stop_ev.wait(self.every)

    def stop(self):
        self.stop_ev.set()
