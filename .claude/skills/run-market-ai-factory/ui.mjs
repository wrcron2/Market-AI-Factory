#!/usr/bin/env node
// Browser UI driver for Market-AI-Factory — drives headless Chrome over the
// DevTools Protocol using only Node ≥21 built-ins (fetch + WebSocket). This
// exists so features get verified the way a USER experiences them (click,
// type, look), not just via curl: the wizard's stuck-run bug shipped because
// nothing ever walked the UI.
//
// Usage:
//   node ui.mjs start                       # launch headless Chrome (port 9333)
//   node ui.mjs goto <url>
//   node ui.mjs click <css-selector>
//   node ui.mjs clickText <button-text>       # first button/link containing text; accepts confirm()s
//   node ui.mjs type <css-selector> <text>  # React-safe value set + input event
//   node ui.mjs eval <js-expression>        # prints the JSON result
//   node ui.mjs text <css-selector>         # prints innerText of first match
//   node ui.mjs shot <path.png>
//   node ui.mjs stop
//
// State (Chrome pid) lives in /tmp/factory-ui-driver/. Commands attach to the
// running instance, so a session persists across invocations.

import { spawn } from 'node:child_process'
import { mkdirSync, writeFileSync, readFileSync, rmSync } from 'node:fs'

const DEBUG_PORT = 9333
const STATE_DIR = '/tmp/factory-ui-driver'
const CHROME = process.env.CHROME_BIN ?? '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome'

const [, , cmd, ...args] = process.argv

async function attach() {
  const targets = await (await fetch(`http://127.0.0.1:${DEBUG_PORT}/json`)).json()
  const page = targets.find((t) => t.type === 'page')
  if (!page) throw new Error('no page target — did you run `ui.mjs start`?')
  const ws = new WebSocket(page.webSocketDebuggerUrl)
  await new Promise((res, rej) => { ws.onopen = res; ws.onerror = () => rej(new Error('CDP socket failed')) })
  let id = 0
  const pending = new Map()
  ws.onmessage = (ev) => {
    const msg = JSON.parse(ev.data)
    if (msg.id && pending.has(msg.id)) {
      pending.get(msg.id)(msg)
      pending.delete(msg.id)
    }
  }
  const send = (method, params = {}) =>
    new Promise((resolve, reject) => {
      const mid = ++id
      pending.set(mid, (msg) => (msg.error ? reject(new Error(`${method}: ${msg.error.message}`)) : resolve(msg.result)))
      ws.send(JSON.stringify({ id: mid, method, params }))
    })
  return { ws, send }
}

// Evaluate an expression; throws if the page threw. Returns the value.
async function evalJS(send, expression, { awaitPromise = true } = {}) {
  const r = await send('Runtime.evaluate', { expression, returnByValue: true, awaitPromise })
  if (r.exceptionDetails) {
    throw new Error(`page exception: ${r.exceptionDetails.exception?.description ?? r.exceptionDetails.text}`)
  }
  return r.result.value
}

const q = (sel) => `document.querySelector(${JSON.stringify(sel)})`

async function main() {
  switch (cmd) {
    case 'start': {
      mkdirSync(STATE_DIR, { recursive: true })
      const child = spawn(CHROME, [
        '--headless=new', '--disable-gpu', `--remote-debugging-port=${DEBUG_PORT}`,
        `--user-data-dir=${STATE_DIR}/profile`, '--window-size=1440,900', '--no-first-run', 'about:blank',
      ], { detached: true, stdio: 'ignore' })
      child.unref()
      writeFileSync(`${STATE_DIR}/pid`, String(child.pid))
      // Wait for the debug endpoint to come up.
      for (let i = 0; i < 30; i++) {
        try { await fetch(`http://127.0.0.1:${DEBUG_PORT}/json`); console.log(`chrome ready (pid ${child.pid})`); return }
        catch { await new Promise((r) => setTimeout(r, 200)) }
      }
      throw new Error('chrome did not come up on the debug port')
    }
    case 'stop': {
      try { process.kill(Number(readFileSync(`${STATE_DIR}/pid`, 'utf8')), 'SIGTERM') } catch {}
      // Chrome flushes its profile on shutdown — retry the cleanup briefly
      // instead of racing it.
      for (let i = 0; i < 10; i++) {
        try { rmSync(STATE_DIR, { recursive: true, force: true }); break }
        catch { await new Promise((r) => setTimeout(r, 300)) }
      }
      console.log('stopped')
      return
    }
    case 'goto': {
      const { ws, send } = await attach()
      await send('Page.enable')
      const done = new Promise((r) => { ws.addEventListener('message', (ev) => { if (JSON.parse(ev.data).method === 'Page.loadEventFired') r() }) })
      await send('Page.navigate', { url: args[0] })
      await Promise.race([done, new Promise((r) => setTimeout(r, 8000))])
      await new Promise((r) => setTimeout(r, 400)) // let React settle
      console.log(await evalJS(send, 'location.href'))
      ws.close()
      return
    }
    case 'click': {
      const { ws, send } = await attach()
      // Headless Chrome auto-DISMISSES window.confirm/alert unless handled —
      // accept them so flows guarded by a confirm (e.g. Cancel run) work.
      await send('Page.enable')
      ws.addEventListener('message', (ev) => {
        const msg = JSON.parse(ev.data)
        if (msg.method === 'Page.javascriptDialogOpening') {
          send('Page.handleJavaScriptDialog', { accept: true })
        }
      })
      const ok = await evalJS(send, `(() => { const el = ${q(args[0])}; if (!el) return false; el.click(); return true })()`, { awaitPromise: false })
      if (!ok) throw new Error(`no element matches ${args[0]}`)
      await new Promise((r) => setTimeout(r, 600))
      console.log('clicked', args[0], '→', await evalJS(send, 'location.href'))
      ws.close()
      return
    }
    // Click the first button whose text includes the given string.
    case 'clickText': {
      const { ws, send } = await attach()
      await send('Page.enable')
      ws.addEventListener('message', (ev) => {
        const msg = JSON.parse(ev.data)
        if (msg.method === 'Page.javascriptDialogOpening') {
          send('Page.handleJavaScriptDialog', { accept: true })
        }
      })
      const ok = await evalJS(send, `(() => {
        const el = [...document.querySelectorAll('button, a')].find((b) => b.innerText.trim().includes(${JSON.stringify(args[0])}));
        if (!el) return false; el.click(); return true })()`, { awaitPromise: false })
      if (!ok) throw new Error(`no button/link with text ${JSON.stringify(args[0])}`)
      await new Promise((r) => setTimeout(r, 600))
      console.log('clicked text', JSON.stringify(args[0]), '→', await evalJS(send, 'location.href'))
      ws.close()
      return
    }
    case 'type': {
      const { ws, send } = await attach()
      const ok = await evalJS(send, `(() => {
        const el = ${q(args[0])}; if (!el) return false;
        const setter = Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, 'value').set;
        setter.call(el, ${JSON.stringify(args[1] ?? '')});
        el.dispatchEvent(new Event('input', { bubbles: true }));
        return true })()`)
      if (!ok) throw new Error(`no element matches ${args[0]}`)
      console.log('typed into', args[0], '— value is now:', await evalJS(send, `${q(args[0])}.value`))
      ws.close()
      return
    }
    case 'eval': {
      const { ws, send } = await attach()
      console.log(JSON.stringify(await evalJS(send, args[0]), null, 2))
      ws.close()
      return
    }
    case 'text': {
      const { ws, send } = await attach()
      console.log(await evalJS(send, `${q(args[0])}?.innerText ?? '(no match)'`))
      ws.close()
      return
    }
    case 'shot': {
      const { ws, send } = await attach()
      const { data } = await send('Page.captureScreenshot', { format: 'png' })
      writeFileSync(args[0], Buffer.from(data, 'base64'))
      console.log('wrote', args[0])
      ws.close()
      return
    }
    default:
      console.error('usage: ui.mjs start|goto|click|type|eval|text|shot|stop  (see file header)')
      process.exit(2)
  }
}

main().catch((e) => { console.error('ERROR:', e.message); process.exit(1) })
