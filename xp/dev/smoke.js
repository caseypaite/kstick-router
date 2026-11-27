#!/usr/bin/env node
/* Headless smoke test of the XP shell against dev/mock-server.js (jsdom; needs devtools/node_modules).
   Usage: node dev/mock-server.js 8088 & node dev/smoke.js http://127.0.0.1:8088 */
'use strict';
const path = require('path');
const { JSDOM, VirtualConsole } = require(path.join(__dirname, 'node_modules', 'jsdom'));
const BASE = process.argv[2] || 'http://127.0.0.1:8088';
const sleep = ms => new Promise(r => setTimeout(r, ms));
const errors = [];

(async () => {
  const vc = new VirtualConsole();
  vc.on('jsdomError', e => errors.push('jsdom: ' + (e.message || e)));
  vc.on('error', (...a) => errors.push('console.error: ' + a.join(' ')));
  const dom = await JSDOM.fromURL(BASE + '/', {
    runScripts: 'dangerously', resources: 'usable', pretendToBeVisual: true, virtualConsole: vc,
    beforeParse(w) {
      let cookie = '';
      w.fetch = async (u, o = {}) => {
        const { signal, ...rest } = o;
        const r = await fetch(new URL(u, BASE), { ...rest, headers: { ...(rest.headers || {}), cookie } });
        return { ok: r.ok, status: r.status, statusText: r.statusText, json: () => r.json() };
      };
      w.matchMedia = q => ({ matches: false, media: q, addEventListener() {}, removeEventListener() {} });
      w.HTMLDialogElement.prototype.showModal = function () { this.open = true; };
      w.HTMLDialogElement.prototype.close = function (v) { this.open = false; if (v !== undefined) this.returnValue = v; this.dispatchEvent(new w.Event('close')); };
      w.HTMLCanvasElement.prototype.getContext = () => new Proxy({}, { get: (t, k) => k in t ? t[k] : () => {}, set: (t, k, v) => (t[k] = v, true) });
      w.Element.prototype.setPointerCapture = () => {};
      w.structuredClone = v => JSON.parse(JSON.stringify(v));
      w.addEventListener('error', e => errors.push('window.onerror: ' + e.message));
      w.addEventListener('unhandledrejection', e => errors.push('unhandled: ' + (e.reason && e.reason.stack || e.reason)));
    },
  });
  const w = dom.window, $ = s => w.document.querySelector(s), ev = s => w.eval(s);
  const check = (name, cond) => { console.log(`${cond ? 'PASS' : 'FAIL'}  ${name}`); if (!cond) process.exitCode = 1; };
  await new Promise(r => w.addEventListener('load', r));
  await sleep(2200);   // boot splash

  check('boot splash hidden after start', $('#boot').classList.contains('hidden'));
  check('welcome screen shown when not signed in', !$('#login').classList.contains('hidden') && $('#desktop').classList.contains('hidden'));

  // wrong password (empty → mock rejects) is required by the form; sign in properly
  const f = $('#login-form');
  f.querySelector('[name=username]').value = 'user'; f.querySelector('[name=password]').value = 'secret';
  f.dispatchEvent(new w.Event('submit', { cancelable: true }));
  await sleep(1500); await ev('renderQ');
  check('desktop shown after sign-in', !$('#desktop').classList.contains('hidden') && $('#login').classList.contains('hidden'));
  check('desktop icons drawn', w.document.querySelectorAll('.dicon').length >= 6);
  check('dashboard window opened by default', ev('WINS.has("dashboard")'));
  check('dashboard rendered without error box', !$('.win[data-id=dashboard] .errbox') && !!$('.win[data-id=dashboard] #chart'));
  check('dashboard registered its poll timer', ev('WINS.get("dashboard").timers.length') >= 1);
  check('taskbar has a button for the window', w.document.querySelectorAll('#tasks .task').length === 1);
  check('tray shows Mesh connected', !!$('#tray-icons [data-tray=mesh]') && !$('#tray-icons [data-tray=mesh]').classList.contains('dim'));

  // open every page in its own window, one after another
  const ids = ev('PAGES.filter(p => p.length > 1 && pageVisible(p)).map(p => p[0])').concat(['control']);
  const results = {};
  for (const id of ids) { ev(`openWin(${JSON.stringify(id)})`); await ev('renderQ'); await sleep(30); results[id] = $(`.win[data-id="${id}"] .errbox p`)?.textContent || 'ok'; }
  check(`all ${ids.length} windows open at once`, ev('WINS.size') === ids.length && w.document.querySelectorAll('#windows .win').length === ids.length);
  // failures the shell could cause: a global it should provide is missing, or a page's element isn't in the document
  const shellish = Object.entries(results).filter(([, m]) => /\b(toast|every|route|showLogin|showPending|updatePills|buildNav|pageTimers|shellTimer)\b.*not (defined|a function)|of null \(/.test(m));
  console.log('      page results (mock data is minimal; shape errors are expected):');
  for (const [id, m] of Object.entries(results)) console.log(`        ${id.padEnd(11)} ${m === 'ok' ? 'ok' : 'error box: ' + m}`);
  check('no page failed for a shell reason (missing global / missing element)', shellish.length === 0);
  check('System window has the Web interface style switch', !!$('.win[data-id=system] #ui-form select[name=style]') && /Windows XP style/.test($('.win[data-id=system]').textContent));
  check('Control Panel offers Switch to Classic View', !!$('.win[data-id=control] [data-act=classic]'));
  check('control panel lists categories', w.document.querySelectorAll('.win[data-id=control] .cp-item').length >= 10);

  // window management
  const dash = 'WINS.get("dashboard")';
  ev(`minimize(${dash})`);
  check('minimize hides the window', $('.win[data-id=dashboard]').classList.contains('hidden') && ev(`${dash}.min`));
  ev(`taskClick("dashboard")`);
  check('taskbar click restores it and makes it active', !ev(`${dash}.min`) && ev('ACTIVE.id') === 'dashboard');
  ev(`setMax(${dash}, true)`);
  check('maximize', $('.win[data-id=dashboard]').classList.contains('max'));
  ev(`closeWin(WINS.get("logs"))`);
  check('close removes window + task button', !ev('WINS.has("logs")') && !$('.win[data-id=logs]') && !$('#tasks [data-task=logs]'));
  const t = ev(`(() => { const x = WINS.get("dashboard"); closeWin(x); return x.timers.length; })()`);
  check('closing a window stops its timers', t === 0);

  // start menu
  $('#start').dispatchEvent(new w.MouseEvent('click', { bubbles: true }));
  check('start menu opens', !$('#startmenu').classList.contains('hidden'));
  $('#sm-left [data-open="wan"]').dispatchEvent(new w.MouseEvent('click', { bubbles: true }));
  await ev('renderQ');
  check('start menu item opens/focuses window and closes menu', ev('ACTIVE.id') === 'wan' && $('#startmenu').classList.contains('hidden'));

  // confirm-or-revert balloon
  await ev(`api('PUT', '/thermal', {})`);
  check('pending change shows the confirm balloon', !$('#pending').classList.contains('hidden') && /Keep changes/.test($('#pending').textContent));
  check('tray shows the pending warning icon', !!$('#tray-icons [data-tray=pending]'));
  $('#pending [data-p=confirm]').dispatchEvent(new w.MouseEvent('click', { bubbles: true }));
  await sleep(500);
  check('confirming hides the balloon', $('#pending').classList.contains('hidden') && ev('PEND') === null);

  // modal dialog (used by pages for edit forms / confirmations)
  const pr = ev(`confirmBox('Test', 'body', 'OK')`);
  check('page modal opens', $('#modal').open === true && $('#modal-title').textContent === 'Test');
  $('#modal').close('ok'); check('modal resolves', (await pr) === true);

  // log off
  await ev(`api('POST', '/logout').then(() => { CSRF = ''; showLogin(); })`);
  check('log off closes windows and returns to the welcome screen', ev('WINS.size') === 0 && !$('#login').classList.contains('hidden'));
  check('user name remembered on welcome screen', $('#login-form [name=username]').value === 'user');

  const real = errors.filter(e => !/Could not parse CSS stylesheet|Not implemented: (navigation|HTMLCanvasElement)/.test(e));
  if (real.length) console.log('      errors:\n        ' + real.join('\n        '));
  check('no uncaught errors', real.length === 0);
  w.close();
  process.exit(process.exitCode || 0);
})().catch(e => { console.error(e); process.exit(2); });
