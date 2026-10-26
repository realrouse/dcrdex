#!/usr/bin/env python3
"""REGTEST ONLY. Relay blocks and mempool txs between the lbc harness lbcd
nodes over RPC. Workaround for a partitioned harness: lbcd (btcd) bans peers
for repeated "transaction not found" replies under heavy tx load, and all
harness nodes share 127.0.0.1, so one ban splits the network for 24h.
New harnesses start lbcd with --nobanning; this heals a running one without
restarting lbcd (which would drop the mempools).
Usage: rpc-relay.py [--once]"""
import json, sys, time, urllib.request, urllib.error, base64
sys.setrecursionlimit(100000)

NODES = {"alpha": 39245, "beta": 39248, "gamma": 39251}
AUTH = "Basic " + base64.b64encode(b"user:pass").decode()


def rpc(port, method, *params):
    body = json.dumps({"jsonrpc": "1.0", "id": 1, "method": method, "params": list(params)}).encode()
    req = urllib.request.Request("http://127.0.0.1:%d/" % port, data=body, headers={"Authorization": AUTH, "Content-Type": "text/plain"})
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            d = json.load(r)
    except urllib.error.HTTPError as e:
        d = json.load(e)
    if d.get("error"):
        raise RuntimeError(d["error"])
    return d["result"]


def relay():
    nb = nt = 0
    heights = {n: rpc(p, "getblockcount") for n, p in NODES.items()}
    src = max(heights, key=heights.get)
    for n, p in NODES.items():
        for h in range(heights[n] + 1, heights[src] + 1):
            blk = rpc(NODES[src], "getblock", rpc(NODES[src], "getblockhash", h), 0)
            rpc(p, "submitblock", blk)
            nb += 1
    pools = {n: rpc(p, "getrawmempool", True) for n, p in NODES.items()}
    deps, raw = {}, {}
    for n, pool in pools.items():
        for t, info in pool.items():
            if t not in raw:
                deps[t] = info.get("depends") or []
                raw[t] = rpc(NODES[n], "getrawtransaction", t, 0)
    order, seen = [], set()

    def visit(t):  # parents first
        if t in seen:
            return
        seen.add(t)
        for d in deps.get(t, []):
            visit(d)
        order.append(t)
    for t in raw:
        visit(t)
    for n, p in NODES.items():
        for t in order:
            if t in pools[n] or t not in raw:
                continue
            try:
                rpc(p, "sendrawtransaction", raw[t]); nt += 1
            except Exception as e:
                if "already" not in str(e):
                    print("  %s: %s %s" % (n, t[:12], str(e)[:120]))
    return nb, nt


if __name__ == "__main__":
    once = "--once" in sys.argv
    while True:
        try:
            nb, nt = relay()
            if nb or nt:
                print(time.strftime("%H:%M:%S"), "relayed %d blocks, %d txs" % (nb, nt), flush=True)
        except Exception as e:
            print(time.strftime("%H:%M:%S"), "relay error:", e, flush=True)
        if once:
            break
        time.sleep(2)
