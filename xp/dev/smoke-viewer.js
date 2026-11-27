#!/usr/bin/env node
/* Headless check of the XP UI in read-only viewer mode, against a running lofi-viewer
   (e.g. lofi-viewer serve -listen 127.0.0.1:8099 -web web -users /tmp/users -insecure-cookie).
   Usage: node dev/smoke-viewer.js http://127.0.0.1:8099 viewer 'password' */
'use strict';
const path = require('path');
const { JSDOM, VirtualConsole } = require(path.join(__dirname, 'node_modules', 'jsdom'));
const [BASE = 'http://127.0.0.1:8099', USER = 'viewer', PASS = ''] = process.argv.slice(2);
const sleep = ms => new Promise(r => setTimeout(r, ms));
const errors = [];
(async () => {
  const vc = new VirtualConsole();
  vc.on('jsdomError', e => errors.push('jsdom: ' + (e.message || e)));
  let cookie = '';
  const dom = await JSDOM.fromURL(BASE + '/', {
    runScripts: 'dangerously', resources: 'usable', pretendToBeVisual: true, virtualConsole: vc,
    beforeParse(w) {
      w.fetch = async (u, o = {}) => {
        const { signal, credentials, cache, ...rest } = o;
        const r = await fetch(new URL(u, BASE), { ...rest, headers: { ...(rest.headers || {}), cookie } });
        const sc = r.headers.get('set-cookie'); if (sc) cookie = sc.split(';')[0].replace(/=$/, '=x');
        return { ok: r.ok, status: r.status, statusText: r.statusText, json: () => r.json() };
      };
      w.matchMedia = q => ({ matches: false, media: q, addEventListener() {}, removeEventListener() {} });
      w.HTMLDialogElement.prototype.showModal = function () { this.open = true; };
      w.HTMLDialogElement.prototype.close = function () { this.open = false; this.dispatchEvent(new w.Event('close')); };
      w.HTMLCanvasElement.prototype.getContext = () => new Proxy({}, { get: (t, k) => k in t ? t[k] : () => {}, set: (t, k, v) => (t[k] = v, true) });
      w.Element.prototype.setPointerCapture = () => {};
      w.structuredClone = v => JSON.parse(JSON.stringify(v));
      w.addEventListener('error', e => errors.push('window.onerror: ' + e.message));
      w.addEventListener('unhandledrejection', e => errors.push('unhandled: ' + (e.reason && e.reason.stack || e.reason)));
    },
  });
  const w = dom.window, $ = s => w.document.querySelector(s), ev = s => w.eval(s);
  const check = (n, c) => { console.log(`${c ? 'PASS' : 'FAIL'}  ${n}`); if (!c) process.exitCode = 1; };
  await new Promise(r => w.addEventListener('load', r));
  await sleep(2200);
  check('welcome screen in read-only mode', !$('#login').classList.contains('hidden') && /Read-only/.test($('.wl-left p').textContent) && w.document.body.classList.contains('ro'));
  const f = $('#login-form');
  f.querySelector('[name=username]').value = USER; f.querySelector('[name=password]').value = PASS;
  f.dispatchEvent(new w.Event('submit', { cancelable: true }));
  await sleep(2500); await ev('renderQ');
  check('signed in, desktop shown', !$('#desktop').classList.contains('hidden'));
  check('VIEWER flag set from server role', ev('VIEWER') === true);
  check('only the Dashboard page is visible', JSON.stringify(ev('PAGES.filter(p => p.length > 1 && pageVisible(p)).map(p => p[0])')) === '["dashboard"]');
  check('desktop icons: just My Router', [...w.document.querySelectorAll('.dicon')].map(x => x.dataset.dicon).join() === 'dashboard');
  check('dashboard rendered with data', !$('.win[data-id=dashboard] .errbox') && /System/.test($('.win[data-id=dashboard] .wv').textContent));
  if ($('.win[data-id=dashboard] .errbox')) console.log('      error:', $('.win[data-id=dashboard] .errbox').textContent);
  check('no mesh icon in tray', !$('#tray-icons [data-tray=mesh]'));
  check('Turn Off hidden (CSS), Control Panel not openable', /#sm-turnoff/.test(w.document.styleSheets[0] ? [...w.document.styleSheets[0].cssRules].map(r => r.cssText).join('') : '') && ev('openWin("control")') === null);
  check('opening an admin page is refused', ev('openWin("firewall")') === null && ev('openWin("system")') === null);
  const r = await ev(`api('PUT', '/system', {}).then(() => 'allowed', e => e.message)`);
  check('server refuses a change: ' + r, /read-only/i.test(r));
  const real = errors.filter(e => !/Could not parse CSS stylesheet|Not implemented/.test(e));
  if (real.length) console.log('      errors:\n        ' + real.join('\n        '));
  check('no uncaught errors', real.length === 0);
  w.close(); process.exit(process.exitCode || 0);
})().catch(e => { console.error(e); process.exit(2); });
