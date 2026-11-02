// SIMNET ONLY. Screenshot a bisonw page after logging in.
// Usage: APP_PASS=... NODE_PATH=.../node_modules node shot.js <base> <path> <out.png> [waitMs] [selectorToScroll]
const puppeteer = require('puppeteer-core')
const [BASE, PATH, OUT, WAIT = '4000', SEL = ''] = process.argv.slice(2)
const sleep = ms => new Promise(r => setTimeout(r, ms))
;(async () => {
  const b = await puppeteer.launch({ executablePath: '/usr/bin/google-chrome', headless: 'new',
    args: ['--no-sandbox', `--user-data-dir=/tmp/bisonary-chrome-${process.pid}`] })
  const p = await b.newPage()
  await p.setViewport({ width: 1600, height: 1100 })
  const errs = []
  p.on('pageerror', e => errs.push(e.message))
  await p.goto(BASE + '/login', { waitUntil: 'networkidle0' })
  await p.type('#loginForm input[type=password]', process.env.APP_PASS)
  await p.click('#loginSubmit')
  await sleep(2500)
  await p.goto(BASE + PATH, { waitUntil: 'networkidle0' })
  await sleep(parseInt(WAIT))
  if (SEL) await p.evaluate(s => document.querySelector(s)?.scrollIntoView(), SEL)
  await sleep(500)
  await p.screenshot({ path: OUT })
  if (errs.length) console.log('page errors:', errs.join(' | '))
  await b.close()
})().catch(e => { console.error(e); process.exit(1) })
