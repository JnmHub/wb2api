// Browser smoke regression for the console, driven through the Chrome DevTools
// Protocol with a headless Edge/Chrome. No third-party dependency: Node 22's
// global WebSocket and fetch are enough.
//
//   node tools/uitest/smoke.mjs --url http://127.0.0.1:18888 --password secret
//
// It logs in, visits every tab, exercises the main dialogs (add account, key
// editor, pause logs, theme switch, account drawer), records screenshots and
// fails on page errors or unexpected console errors.
import { spawn } from 'node:child_process'
import { mkdirSync, writeFileSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { setTimeout as sleep } from 'node:timers/promises'

const args = Object.fromEntries(
  process.argv.slice(2).map((a, i, all) => (a.startsWith('--') ? [a.slice(2), all[i + 1]] : null)).filter(Boolean),
)
const URL = args.url || 'http://127.0.0.1:18888'
const PASSWORD = args.password || process.env.WB_PANEL_PASSWORD || 'admin'
const EDGE =
  args.edge ||
  process.env.EDGE_PATH ||
  'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe'
const PORT = Number(args.port || 9333)
const OUT = args.out || join(process.cwd(), '.scratch', 'uitest')
mkdirSync(OUT, { recursive: true })
const profile = join(tmpdir(), `wb-uitest-${Date.now()}`)
const failures = []
let consoleErrors = []

const browser = spawn(EDGE, [
  '--headless=new',
  '--disable-gpu',
  '--hide-scrollbars',
  `--remote-debugging-port=${PORT}`,
  `--user-data-dir=${profile}`,
  '--no-first-run',
  '--no-default-browser-check',
  'about:blank',
])
browser.on('error', (e) => {
  console.error('browser failed:', e.message)
  process.exit(2)
})

async function waitForDevtools() {
  for (let i = 0; i < 80; i++) {
    try {
      const r = await fetch(`http://127.0.0.1:${PORT}/json/list`)
      const list = await r.json()
      const page = list.find((t) => t.type === 'page')
      if (page) return page
    } catch {}
    await sleep(250)
  }
  throw new Error('devtools did not start')
}

function connect(wsUrl) {
  const ws = new WebSocket(wsUrl)
  let id = 0
  const pending = new Map()
  const events = []
  ws.onmessage = (e) => {
    const msg = JSON.parse(e.data)
    if (msg.id && pending.has(msg.id)) {
      const { resolve, reject } = pending.get(msg.id)
      pending.delete(msg.id)
      msg.error ? reject(new Error(msg.error.message)) : resolve(msg.result)
    } else if (msg.method) {
      events.push(msg)
      if (msg.method === 'Runtime.exceptionThrown') {
        failures.push('page exception: ' + (msg.params.exceptionDetails?.text || 'unknown'))
      }
      if (msg.method === 'Log.entryAdded' && msg.params.entry.level === 'error') {
        consoleErrors.push(msg.params.entry.text)
      }
      if (msg.method === 'Runtime.consoleAPICalled' && msg.params.type === 'error') {
        consoleErrors.push(msg.params.args.map((a) => a.value ?? a.description ?? '').join(' '))
      }
    }
  }
  const ready = new Promise((resolve, reject) => {
    ws.onopen = resolve
    ws.onerror = reject
  })
  const send = (method, params = {}) =>
    new Promise((resolve, reject) => {
      const n = ++id
      pending.set(n, { resolve, reject })
      ws.send(JSON.stringify({ id: n, method, params }))
    })
  return { ws, ready, send, events }
}

async function main() {
  const target = await waitForDevtools()
  const cdp = connect(target.webSocketDebuggerUrl)
  await cdp.ready
  const { send } = cdp
  await send('Page.enable')
  await send('Runtime.enable')
  await send('Log.enable')

  const evaluate = async (expression) => {
    const r = await send('Runtime.evaluate', { expression, returnByValue: true, awaitPromise: true })
    if (r.exceptionDetails) throw new Error(r.exceptionDetails.text || 'evaluate failed')
    return r.result?.value
  }
  const waitFor = async (expression, label, timeout = 8000) => {
    const start = Date.now()
    while (Date.now() - start < timeout) {
      if (await evaluate(`Boolean(${expression})`)) return
      await sleep(150)
    }
    throw new Error('timeout waiting for ' + label)
  }
  const text = () => evaluate('document.body.innerText')
  const shot = async (name) => {
    const r = await send('Page.captureScreenshot', { format: 'png' })
    writeFileSync(join(OUT, name + '.png'), Buffer.from(r.data, 'base64'))
  }
  const clickText = (sel, label) =>
    evaluate(`(() => {
      const el = [...document.querySelectorAll(${JSON.stringify(sel)})].find((e) => (e.innerText || '').includes(${JSON.stringify(label)}));
      if (!el) return false;
      el.click(); return true;
    })()`)

  await send('Emulation.setDeviceMetricsOverride', { width: 1440, height: 900, deviceScaleFactor: 1, mobile: false })
  await send('Page.navigate', { url: URL })
  await sleep(1200)

  if (await evaluate(`Boolean(document.querySelector('.login-card'))`)) {
    const filled = await evaluate(`(() => {
      const inp = document.querySelector('.login-card input[type=password]');
      if (!inp) return false;
      const set = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set;
      set.call(inp, ${JSON.stringify(PASSWORD)});
      inp.dispatchEvent(new Event('input', { bubbles: true }));
      document.querySelector('.login-card button[type=submit], .login-card .ant-btn-primary')?.click();
      return true;
    })()`)
    if (!filled) failures.push('login form not found')
    await waitFor(`document.querySelector('.app-shell')`, 'app shell after login', 12000)
  }

  // Password-change gate: the fresh install asks for a new password; tests use
  // a configured password, but report it clearly rather than hanging.
  if (await evaluate(`document.body.innerText.includes('请先修改默认密码')`)) {
    failures.push('panel is still on the default password; change it before running the UI test')
    throw new Error('default password gate')
  }

  const pages = [
    { hash: 'accounts', title: '账号', checks: ['全部账号', '今日 Token'] },
    { hash: 'usage', title: '用量', checks: ['请求数', '价格估算'] },
    { hash: 'models', title: '模型', checks: ['能力', '上下文'] },
    { hash: 'requests', title: '请求记录', checks: ['最近的网关请求'] },
    { hash: 'settings', title: '设置', checks: ['API Key', '模型清单', '上游与提示词'] },
    { hash: 'logs', title: '日志', checks: ['跟随最新'] },
    { hash: 'tasks', title: '任务', checks: ['后台任务', '国内成长任务'] },
  ]
  for (const p of pages) {
    await evaluate(`location.hash = '#/${p.hash}'`)
    await sleep(1400)
    const body = await text()
    if (!body.includes(p.title)) failures.push(`${p.hash}: title ${p.title} missing`)
    for (const c of p.checks) if (!body.includes(c)) failures.push(`${p.hash}: expected text ${c}`)
    await shot(p.hash)
  }

  // Theme switch round-trip.
  await evaluate(`document.querySelector('button[aria-label="切换主题"]')?.click()`)
  await sleep(500)
  const theme = await evaluate('document.documentElement.dataset.theme')
  if (!theme) failures.push('theme toggle did not set data-theme')
  await shot('theme-' + theme)

  // The logs viewer must follow the theme too (it used to stay white in dark).
  await evaluate(`location.hash = '#/logs'`)
  await sleep(800)
  await shot('logs-' + theme)

  // Row selection / batch bar.
  await evaluate(`location.hash = '#/accounts'`)
  await sleep(800)
  const selected = await evaluate(`(() => {
    const box = document.querySelector('.ant-table-tbody .ant-checkbox-input');
    if (!box) return false; box.click(); return true;
  })()`)
  if (selected) {
    await sleep(500)
    if (!(await text()).includes('已选')) failures.push('batch bar did not appear after selecting a row')
    await shot('batch-bar')
    if (await clickText('button', '更多操作')) {
      await sleep(400)
      if (!(await evaluate(`document.body.innerText.includes('签到（仅国内版')`))) failures.push('batch overflow menu is missing check-in')
      await shot('batch-more')
      await evaluate(`document.body.click()`)
      await sleep(200)
    } else {
      failures.push('batch overflow menu missing')
    }
    if (await clickText('button', '导出选中')) {
      await sleep(500)
      if (!(await text()).includes('导出选中的 1 个账号')) failures.push('selected export did not pass the uid list')
      await shot('export-selected')
      await evaluate(`document.querySelector('.ant-modal-close')?.click()`)
      await sleep(250)
    } else {
      failures.push('batch export button missing')
    }
    if (!(await clickText('button', '取消选择'))) failures.push('batch bar clear button missing')
    await sleep(200)
  } else {
    failures.push('no row-selection checkbox')
  }

  // Request detail drawer.
  await evaluate(`location.hash = '#/requests'`)
  await sleep(900)
  const openedRequest = await evaluate(`(() => {
    const row = document.querySelector('.ant-table-tbody tr.ant-table-row');
    if (!row) return false; row.click(); return true;
  })()`)
  if (openedRequest) {
    await sleep(500)
    if (!(await text()).includes('请求 #')) failures.push('request detail drawer did not open')
    await shot('request-detail')
    await evaluate(`document.querySelector('.ant-drawer-close')?.click()`)
    await sleep(250)
  }

  // Account drawer.
  await evaluate(`location.hash = '#/accounts'`)
  await sleep(1000)
  const opened = await evaluate(`(() => {
    const a = document.querySelector('.acct-name a');
    if (!a) return false; a.click(); return true;
  })()`)
  if (opened) {
    await sleep(800)
    const body = await text()
    if (!/账号详情|账号操作/.test(body)) failures.push('account drawer did not open')
    await shot('account-drawer')
    await evaluate(`document.querySelector('.ant-drawer-close')?.click()`)
    await sleep(300)
  } else {
    failures.push('no account row to open')
  }

  // Add-account / import / export / desktop-import dialogs.
  await evaluate(`location.hash = '#/accounts'`)
  await sleep(600)
  const dialogs = [
    { button: '添加账号', expect: '开始登录', name: 'add-account' },
    { button: '导入', expect: '预演', name: 'import' },
    { button: '导出', expect: '包含 Token', name: 'export' },
    { button: '桌面导入', expect: 'at-rest 密钥', name: 'desktop-import' },
  ]
  for (const d of dialogs) {
    if (!(await clickText('button', d.button))) {
      failures.push(`${d.name}: button not found`)
      continue
    }
    await sleep(500)
    if (!(await evaluate(`document.body.innerText.includes(${JSON.stringify(d.expect)})`))) failures.push(`${d.name}: dialog content missing`)
    if (d.name === 'import') {
      const injected = await evaluate(`(() => {
        const input = document.querySelector('.ant-modal input[type=file]');
        if (!input) return false;
        const dt = new DataTransfer();
        dt.items.add(new File(['{"accounts":[]}'], 'import-a.json', { type: 'application/json' }));
        dt.items.add(new File(['{"accounts":[]}'], 'import-b.json', { type: 'application/json' }));
        input.files = dt.files;
        input.dispatchEvent(new Event('change', { bubbles: true }));
        return true;
      })()`)
      if (!injected) {
        failures.push('import: file input not found')
      } else {
        await sleep(700)
        if (!(await evaluate(`document.body.innerText.includes('将按顺序导入 2 个文件')`))) failures.push('import: multiple files were not accepted')
        await shot('import-multi')
      }
    }
    await shot(d.name)
    await evaluate(`document.querySelector('.ant-modal-close')?.click()`)
    await sleep(300)
  }

  // Key editor.
  await evaluate(`location.hash = '#/settings'`)
  await sleep(1000)
  if (await clickText('button', '新建 Key')) {
    await sleep(600)
    if (!(await evaluate(`document.body.innerText.includes('新建 Key')`))) failures.push('key editor did not open')
    await shot('key-editor')
    await evaluate(`document.querySelector('.ant-modal-close')?.click()`)
  } else {
    failures.push('new-key button not found')
  }

  // Logs pause toggle.
  await evaluate(`location.hash = '#/logs'`)
  await sleep(900)
  if (await clickText('button', '暂停')) {
    await sleep(400)
    if (!(await text()).includes('继续')) failures.push('log pause did not flip to continue')
  } else {
    failures.push('log pause button not found')
  }

  await shot('final')
  cdp.ws.close()
}

try {
  await main()
} catch (e) {
  console.error('FAILED:', e.message)
  failures.push(e.message)
} finally {
  browser.kill()
  try {
    rmSync(profile, { recursive: true, force: true })
  } catch {}
}

// Favicon, extension noise and the pre-login 401 session probe are not console
// regressions: the shell probes /api/v2/session before it knows who you are.
const realErrors = consoleErrors.filter((e) => !/favicon|net::ERR|DevTools|401 \(Unauthorized\)/i.test(e))
for (const e of realErrors) failures.push('console error: ' + e)

if (failures.length) {
  console.error('\nUI smoke failures:')
  for (const f of [...new Set(failures)]) console.error(' - ' + f)
  process.exit(1)
}
console.log(`UI smoke OK; screenshots in ${OUT}`)
