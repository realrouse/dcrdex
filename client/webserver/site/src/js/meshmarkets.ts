/*
 * Tatanka mesh markets on the Trade page.
 *
 * With the mesh running (simnet --mesh), the mesh markets are listed on the
 * normal Trade (markets) page under the pseudo host MeshHost, next to any DEX
 * server markets. The page looks and works as usual; this module supplies the
 * mesh side:
 *   - an Exchange-shaped description of the mesh markets (from user.mesh),
 *   - the mesh order book in the page's MarketOrderBook form,
 *   - order submission: a limit or market order first takes the crossing
 *     mesh orders of other peers (best price first, at the maker's rate) and
 *     a limit order's remainder is posted as a standing mesh order,
 *   - our mesh orders, matches and swaps as Orders for "Your Orders",
 *   - candles and 24h spot prices from the trade history that mesh peers
 *     publish when their swaps complete.
 */
import { postJSON, getJSON } from './http'
import {
  app,
  Exchange,
  Market,
  Asset,
  MarketOrderBook,
  MiniOrder,
  Order,
  Match,
  TradeForm,
  ConnectionStatus,
  CandlesPayload,
  Spot
} from './registry'
import * as OrderUtil from './orderutil'
import { BookView, ViewOrder, Activity, MeshMatch, MatchRecord, SwapView } from './mesh'

/* MeshHost is the host name the Trade page uses for the mesh markets. */
export const MeshHost = 'tatanka-mesh'

/* OrderLifetimeSec is how long a standing mesh order stays on the book. */
const OrderLifetimeSec = 3600

export function isMeshHost (host: string | undefined): boolean {
  return host === MeshHost
}

let cached: { key: string, xc: Exchange } | null = null

/* spots are the latest 24h price summaries of the mesh markets. */
let spots: Record<string, Spot> = {}

/* CandleDurs are the candle bin sizes served for mesh markets. */
export const CandleDurs = ['24h', '1h', '5m']

/* meshExchange describes the mesh markets as an Exchange, or null if the mesh
 * is not running. */
export function meshExchange (): Exchange | null {
  const mesh = app().user?.mesh
  if (!mesh || !mesh.markets) return null
  const key = JSON.stringify(mesh.markets)
  if (cached && cached.key === key) return cached.xc
  const markets: Record<string, Market> = {}
  const assets: Record<number, Asset> = {}
  const addAsset = (id: number) => {
    if (assets[id]) return
    const a = app().assets[id]
    if (!a) return
    assets[id] = {
      id, symbol: a.symbol, version: 0, maxFeeRate: 0, swapSize: 0, swapSizeBase: 0,
      redeemSize: 0, swapConf: 1, unitInfo: a.unitInfo
    }
  }
  for (const [name, m] of Object.entries(mesh.markets)) {
    addAsset(m.baseID)
    addAsset(m.quoteID)
    if (!assets[m.baseID] || !assets[m.quoteID]) continue
    markets[name] = {
      name,
      baseid: m.baseID,
      basesymbol: assets[m.baseID].symbol,
      quoteid: m.quoteID,
      quotesymbol: assets[m.quoteID].symbol,
      lotsize: m.lotSize || 1e8,
      parcelsize: 1,
      ratestep: 1,
      epochlen: 0,
      startepoch: 0,
      buybuffer: 1,
      orders: [],
      spot: spots[name],
      atomToConv: 1,
      inflight: [],
      minimumRate: 1
    }
  }
  const xc: Exchange = {
    host: MeshHost,
    acctID: '',
    auth: {
      rep: { bondedTier: 1, penalties: 0, legacyTier: false, score: 0 },
      bondAssetID: 0,
      pendingStrength: 0,
      weakStrength: 0,
      liveStrength: 1,
      targetTier: 1,
      effectiveTier: 1,
      maxBondedAmt: 0,
      penaltyComps: 0,
      pendingBonds: [],
      expiredBonds: [],
      compensation: 0
    },
    markets,
    assets,
    connectionStatus: ConnectionStatus.Connected,
    viewOnly: false,
    bondAssets: {},
    candleDurs: CandleDurs,
    candlePageSize: 0,
    maxScore: 0,
    penaltyThreshold: 0,
    disabled: false
  }
  cached = { key, xc }
  return xc
}

/* exchangeFor returns the Exchange for a host, including the mesh. */
export function exchangeFor (host: string): Exchange {
  if (isMeshHost(host)) return meshExchange() as Exchange
  return app().exchanges[host]
}

/* allExchanges lists the DEX servers and, if running, the mesh. */
export function allExchanges (): Exchange[] {
  const out = Object.values(app().exchanges || {})
  const mesh = meshExchange()
  if (mesh) out.push(mesh)
  return out
}

export async function fetchBook (market: string): Promise<BookView | null> {
  const res = await postJSON('/api/mesh/book', { market })
  if (!res || !res.ok) return null
  return res.book as BookView
}

export async function fetchActivity (): Promise<Activity | null> {
  const res = await getJSON('/api/mesh/activity')
  if (!res || !res.ok) return null
  return res.activity as Activity
}

/* toMarketBook converts a mesh book to the Trade page's order book. Orders of
 * protocol-incompatible peers are not in book.orders, so they don't show. */
export function toMarketBook (book: BookView, base: number, quote: number): MarketOrderBook {
  const bf = app().unitInfo(base).conventional.conversionFactor
  const qf = app().unitInfo(quote).conventional.conversionFactor
  const mini = (o: ViewOrder): MiniOrder => ({
    qty: o.remaining / bf,
    qtyAtomic: o.remaining,
    rate: o.rate / OrderUtil.RateEncodingFactor * bf / qf,
    msgRate: o.rate,
    epoch: 0,
    sell: o.sell,
    token: o.id
  })
  const orders = (book.orders || []).filter(o => o.remaining > 0)
  const sells = orders.filter(o => o.sell).sort((a, b) => a.rate - b.rate || a.stamp - b.stamp).map(mini)
  const buys = orders.filter(o => !o.sell).sort((a, b) => b.rate - a.rate || a.stamp - b.stamp).map(mini)
  return { base, quote, book: { sells, buys, epoch: [], recentMatches: [] } }
}

export interface SubmitResult {
  taken: { orderID: string, lots: number, rate: number, matchID: string }[]
  posted: ViewOrder | null
  errors: string[]
}

/*
 * submit places a Trade page order on the mesh. It takes other peers'
 * crossing orders first, best price first, each at the maker's rate. For a
 * limit order with standing time-in-force, the remaining lots are posted as a
 * standing mesh order at the limit rate.
 */
export async function submit (mktName: string, lotSize: number, order: TradeForm): Promise<SubmitResult> {
  const out: SubmitResult = { taken: [], posted: null, errors: [] }
  const book = await fetchBook(mktName)
  if (!book) {
    out.errors.push('could not read the mesh order book')
    return out
  }
  const buy = !order.sell
  const marketBuy = !order.isLimit && buy
  let lots = marketBuy ? 0 : Math.floor(order.qty / lotSize)
  let quoteBudget = marketBuy ? order.qty : 0
  if (!marketBuy && lots < 1) {
    out.errors.push('the quantity is less than one lot')
    return out
  }
  // Best price first; at the same price the newest order first, since a peer
  // that restarted can leave older orders on other books until they expire.
  const crossing = (book.orders || []).filter(o => {
    if (o.own || o.hidden || o.sell !== buy || o.remaining < o.lotSize) return false
    if (!order.isLimit) return true
    return buy ? o.rate <= order.rate : o.rate >= order.rate
  }).sort((a, b) => buy ? a.rate - b.rate || b.stamp - a.stamp : b.rate - a.rate || b.stamp - a.stamp)
  for (const o of crossing) {
    const avail = Math.floor(o.remaining / o.lotSize)
    let n = Math.min(avail, marketBuy ? Infinity : lots)
    if (marketBuy) n = Math.min(n, Math.floor(quoteBudget / (o.lotSize * o.rate / OrderUtil.RateEncodingFactor)))
    if (n < 1) {
      if (marketBuy) break
      continue
    }
    const res = await postJSON('/api/mesh/take', { market: mktName, orderID: o.id, lots: n })
    if (!res || !res.ok) {
      out.errors.push((res && res.msg) || 'take failed')
      break
    }
    out.taken.push({ orderID: o.id, lots: n, rate: o.rate, matchID: res.matchID })
    if (marketBuy) quoteBudget -= Math.floor(n * o.lotSize * o.rate / OrderUtil.RateEncodingFactor)
    else lots -= n
    if (!marketBuy && lots <= 0) break
  }
  if (out.errors.length) return out
  if (order.isLimit && !order.tifnow && lots > 0) {
    const res = await postJSON('/api/mesh/order', { market: mktName, sell: order.sell, lots, rate: order.rate, lifetimeSec: OrderLifetimeSec })
    if (!res || !res.ok) out.errors.push((res && res.msg) || 'order failed')
    else out.posted = res.order as ViewOrder
  }
  if (!out.taken.length && !out.posted && !out.errors.length) {
    out.errors.push(order.isLimit ? 'no mesh orders to match at this rate' : 'no mesh orders to match')
  }
  return out
}

export async function cancel (orderID: string): Promise<{ ok: boolean, msg?: string }> {
  const res = await postJSON('/api/mesh/cancel', { orderID })
  if (!res || !res.ok) return { ok: false, msg: (res && res.msg) || 'cancel failed' }
  return { ok: true }
}

const finalSwap = ['complete', 'refunded', 'abandoned']
const failedMatch = ['rejected', 'timed_out']

/* matchStatus maps a mesh swap/match status to the DEX match status. */
function matchStatus (side: string, status: string): number {
  if (status === 'complete') return OrderUtil.MatchComplete
  if (side === 'maker') {
    switch (status) {
      case 'init_sent': return OrderUtil.MakerSwapCast
      case 'part_audited': return OrderUtil.TakerSwapCast
      case 'redeemed': return OrderUtil.MakerRedeemed
    }
    return OrderUtil.NewlyMatched
  }
  switch (status) {
    case 'init_audited':
    case 'part_pending': return OrderUtil.MakerSwapCast
    case 'part_sent': return OrderUtil.TakerSwapCast
  }
  return OrderUtil.NewlyMatched
}

function toMatch (mt: MeshMatch, side: string, status: string): Match {
  const revoked = failedMatch.includes(status) || status === 'refunded' || status === 'abandoned'
  return {
    matchID: mt.id,
    status: matchStatus(side, status),
    active: !revoked && !finalSwap.includes(status),
    revoked,
    rate: mt.rate,
    qty: mt.qty,
    side: side === 'maker' ? OrderUtil.MatchSideMaker : OrderUtil.MatchSideTaker,
    feeRate: 0,
    swap: undefined as any,
    counterSwap: undefined as any,
    redeem: undefined as any,
    counterRedeem: undefined as any,
    refund: undefined as any,
    stamp: mt.stamp,
    isCancel: false
  }
}

/*
 * userOrders turns our mesh activity on one market into Orders for the Trade
 * page's "Your Orders": standing orders (with their matches), filled or
 * expired standing orders that had matches, and orders we took.
 */
export function userOrders (act: Activity, mkt: Market): Order[] {
  const base = mkt.baseid
  const quote = mkt.quoteid
  const onMkt = (mt: MeshMatch) => mt.market.base === base && mt.market.quote === quote
  // All matches, with the swap status where there is a swap.
  const recs = new Map<string, { mt: MeshMatch, side: string, status: string }>()
  for (const r of (act.matches || []) as MatchRecord[]) {
    if (onMkt(r.match)) recs.set(r.match.id, { mt: r.match, side: r.side, status: r.status })
  }
  for (const s of (act.swaps || []) as SwapView[]) {
    if (onMkt(s.match)) recs.set(s.match.id, { mt: s.match, side: s.side, status: s.status })
  }
  const blank = (id: string, sell: boolean, rate: number, qty: number, stamp: number): Order => ({
    host: MeshHost,
    baseID: base,
    baseSymbol: mkt.basesymbol,
    quoteID: quote,
    quoteSymbol: mkt.quotesymbol,
    market: mkt.name,
    type: OrderUtil.Limit,
    id,
    stamp,
    submitTime: stamp,
    sig: '',
    status: OrderUtil.StatusExecuted,
    epoch: 0,
    qty,
    sell,
    filled: 0,
    matches: [],
    cancelling: false,
    canceled: false,
    feesPaid: { swap: 0, redemption: 0 },
    fundingCoins: [],
    accelerationCoins: [],
    lockedamt: 0,
    rate,
    tif: OrderUtil.StandingTiF,
    targetOrderID: '',
    readyToTick: true
  })
  const orders = new Map<string, Order>()
  for (const o of act.orders || []) {
    if (o.base !== base || o.quote !== quote) continue
    const ord = blank(o.id, o.sell, o.rate, o.qty, o.stamp)
    ord.status = OrderUtil.StatusBooked
    orders.set(o.id, ord)
  }
  for (const { mt, side, status } of recs.values()) {
    if (side === 'maker') {
      let ord = orders.get(mt.makerOrderID)
      if (!ord) {
        ord = blank(mt.makerOrderID, mt.makerSell, mt.rate, 0, mt.stamp)
        orders.set(mt.makerOrderID, ord)
      }
      ord.matches.push(toMatch(mt, side, status))
      if (ord.status !== OrderUtil.StatusBooked) ord.qty += mt.qty
    } else {
      const ord = blank('take-' + mt.id, !mt.makerSell, mt.rate, mt.qty, mt.stamp)
      const m = toMatch(mt, side, status)
      ord.matches.push(m)
      if (m.revoked && failedMatch.includes(status)) ord.status = OrderUtil.StatusRevoked
      orders.set(ord.id, ord)
    }
  }
  for (const ord of orders.values()) {
    ord.filled = ord.matches.filter(m => !(m.revoked && m.status === OrderUtil.NewlyMatched)).reduce((t, m) => t + m.qty, 0)
    if (ord.qty === 0) ord.qty = ord.filled || 1
  }
  return Array.from(orders.values())
}

/* isTakeID is true for the Your Orders entries of orders we took. */
export function isTakeID (id: string): boolean {
  return id.startsWith('take-')
}

/* fetchCandles gets a mesh market's candles for a bin size. */
export async function fetchCandles (market: string, dur: string): Promise<CandlesPayload | null> {
  const res = await postJSON('/api/mesh/candles', { market, dur })
  if (!res || !res.ok) return null
  return res.candles as CandlesPayload
}

/*
 * fetchSpots gets the mesh markets' 24h price summaries and sets them on the
 * mesh Exchange's markets. It returns the spots.
 */
export async function fetchSpots (): Promise<Record<string, Spot>> {
  const res = await getJSON('/api/mesh/spots')
  if (!res || !res.ok) return spots
  spots = res.spots || {}
  const xc = meshExchange()
  if (xc) for (const [name, mkt] of Object.entries(xc.markets)) mkt.spot = spots[name]
  return spots
}
