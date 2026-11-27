
/* ====================================================================================================
   KStick XP — Windows-XP-style desktop shell for the KStick router web UI.

   Every page of the router UI (src/pages.js) opens in its own window. The page functions are the
   original ones, unchanged in what they call on the API; this shell supplies the few globals they
   expect from their host: toast(), every(), route(), showLogin(), showPending(), updatePills().

   The server sends X-Frame-Options: DENY and a strict CSP (script-src 'self', no inline <style>),
   so all windows live in this one document (no iframes) and nothing here uses inline scripts.
   ==================================================================================================== */

// ---------------------------------------------------------------- icons (32×32 inline SVG)
const SVG = (body, vb = '0 0 32 32') => `<svg viewBox="${vb}" aria-hidden="true">${body}</svg>`;
const ICONS = {
  logo: SVG('<path d="M16 2l6 7-6 7-6-7z" fill="#f25022"/><path d="M23 9l7 7-7 7-6-7z" fill="#7fba00"/><path d="M16 16l6 7-6 7-6-7z" fill="#00a4ef"/><path d="M9 9l6 7-6 7-7-7z" fill="#ffb900"/>'),
  computer: SVG('<rect x="3" y="4" width="26" height="18" rx="2" fill="url(#g-grey)" stroke="#5b6573"/><rect x="5.5" y="6.5" width="21" height="13" fill="url(#g-screen)"/><path d="M6 15c5-3 10 2 20-4v8.5H6z" fill="#4fae3a" opacity=".85"/><path d="M12 22h8l1.5 4h-11z" fill="#8a929c"/><rect x="7" y="26" width="18" height="3" rx="1" fill="url(#g-grey)" stroke="#5b6573"/>'),
  network: SVG('<rect x="1.5" y="3" width="15" height="11" rx="1.5" fill="url(#g-grey)" stroke="#5b6573"/><rect x="3.5" y="5" width="11" height="7" fill="url(#g-screen)"/><rect x="15.5" y="15" width="15" height="11" rx="1.5" fill="url(#g-grey)" stroke="#5b6573"/><rect x="17.5" y="17" width="11" height="7" fill="url(#g-screen)"/><path d="M9 14v6h7M23 15v-5h-6" fill="none" stroke="#2a8a1c" stroke-width="2"/><circle cx="9" cy="27" r="3" fill="url(#g-green)"/>'),
  globe: SVG('<circle cx="16" cy="16" r="13" fill="url(#g-blue)" stroke="#164ea8"/><path d="M8 9c3 0 4 2 6 2s2 3 0 4-3 3-1 5-1 5-3 4-4-5-4-8 0-5 2-7zM19 6c3 1 5 3 6 6-2 0-3 1-4 3s-3 0-3-2 2-2 1-4-2-2 0-3z" fill="#4fbf3a"/><ellipse cx="16" cy="16" rx="13" ry="5" fill="none" stroke="#fff" stroke-opacity=".35"/><path d="M16 3v26" stroke="#fff" stroke-opacity=".35"/>'),
  wifi: SVG('<path d="M3 13a18 18 0 0 1 26 0" fill="none" stroke="#1c5fd1" stroke-width="3" stroke-linecap="round"/><path d="M7.5 17.5a12 12 0 0 1 17 0" fill="none" stroke="#2f7ae5" stroke-width="3" stroke-linecap="round"/><path d="M12 22a6 6 0 0 1 8 0" fill="none" stroke="#4b95f0" stroke-width="3" stroke-linecap="round"/><circle cx="16" cy="26" r="2.6" fill="url(#g-green)" stroke="#2a8a1c"/>'),
  users: SVG('<circle cx="11" cy="10" r="5" fill="url(#g-yellow)" stroke="#b07a10"/><path d="M2 27c0-6 4-10 9-10s9 4 9 10z" fill="url(#g-blue)" stroke="#164ea8"/><circle cx="22" cy="11" r="4.2" fill="url(#g-yellow)" stroke="#b07a10"/><path d="M15 27c0-5 3-8.5 7-8.5s8 3.5 8 8.5z" fill="url(#g-green)" stroke="#2a8a1c"/>'),
  shield: SVG('<path d="M16 2l12 4v8c0 8-5 13-12 16C9 27 4 22 4 14V6z" fill="url(#g-red)" stroke="#8e1c10"/><path d="M16 2v28C9 27 4 22 4 14V6z" fill="url(#g-green)" opacity=".9"/><path d="M4 14h24" stroke="#fff" stroke-opacity=".6"/><path d="M16 2l12 4v8H16z" fill="url(#g-blue)" opacity=".9"/><path d="M16 14h12c0 8-5 13-12 16z" fill="url(#g-yellow)" opacity=".95"/>'),
  lock: SVG('<path d="M10 14V9a6 6 0 0 1 12 0v5" fill="none" stroke="#8a929c" stroke-width="3"/><rect x="6" y="13" width="20" height="16" rx="2" fill="url(#g-yellow)" stroke="#a46d0a"/><circle cx="16" cy="20" r="2.5" fill="#6b4300"/><path d="M15 21h2v4h-2z" fill="#6b4300"/>'),
  cloud: SVG('<path d="M8 25a6 6 0 0 1-.5-12 8.5 8.5 0 0 1 16.3-2A7 7 0 0 1 24 25z" fill="url(#g-orange)" stroke="#b45309"/><path d="M10 21h13" stroke="#fff" stroke-width="2" stroke-linecap="round" opacity=".8"/>'),
  mail: SVG('<rect x="2.5" y="7" width="27" height="19" rx="1.5" fill="url(#g-paper)" stroke="#6b7c93"/><path d="M3 8l13 10L29 8" fill="none" stroke="#6b7c93" stroke-width="1.6"/><path d="M3 25l10-9M29 25l-10-9" stroke="#9aa9bd"/>'),
  phone: SVG('<rect x="9" y="2.5" width="14" height="27" rx="2.5" fill="url(#g-grey)" stroke="#4b5563"/><rect x="11" y="6" width="10" height="15" fill="url(#g-screen)"/><circle cx="16" cy="25" r="1.6" fill="#4b5563"/>'),
  bt: SVG('<rect x="6" y="2" width="20" height="28" rx="10" fill="url(#g-blue)" stroke="#164ea8"/><path d="M11 11l10 9-5 4.5V7.5l5 4.5-10 9" fill="none" stroke="#fff" stroke-width="2" stroke-linejoin="round"/>'),
  chat: SVG('<path d="M4 6h24v15H14l-6 6v-6H4z" fill="url(#g-green)" stroke="#1f6d16" stroke-linejoin="round"/><path d="M9 11h14M9 15h10" stroke="#fff" stroke-width="2" stroke-linecap="round"/>'),
  forward: SVG('<rect x="2.5" y="7" width="20" height="15" rx="1.5" fill="url(#g-paper)" stroke="#6b7c93"/><path d="M3 8l9.5 7L22 8" fill="none" stroke="#6b7c93" stroke-width="1.4"/><path d="M16 22h7v-4l7 6-7 6v-4h-7z" fill="url(#g-green)" stroke="#1f6d16" stroke-linejoin="round"/>'),
  gateway: SVG('<rect x="3" y="3" width="15" height="25" rx="1.5" fill="url(#g-grey)" stroke="#4b5563"/><path d="M6 8h9M6 12h9M6 16h9" stroke="#4b5563"/><circle cx="7" cy="23" r="1.4" fill="#3fbf3f"/><rect x="14" y="15" width="16" height="12" rx="1.2" fill="url(#g-paper)" stroke="#6b7c93"/><path d="M14.5 16l7.5 6 7.5-6" fill="none" stroke="#6b7c93" stroke-width="1.3"/>'),
  tools: SVG('<circle cx="13" cy="13" r="8.5" fill="#cfe8ff" fill-opacity=".7" stroke="#4b5563" stroke-width="3"/><path d="M19.5 19.5l8 8" stroke="#8a5a12" stroke-width="5" stroke-linecap="round"/><path d="M9 11a4.5 4.5 0 0 1 4-3" fill="none" stroke="#fff" stroke-width="2" stroke-linecap="round"/>'),
  notepad: SVG('<rect x="6" y="3" width="20" height="26" rx="1" fill="url(#g-paper)" stroke="#6b7c93"/><rect x="6" y="3" width="20" height="5" fill="url(#g-blue)"/><path d="M9.5 12.5h13M9.5 16.5h13M9.5 20.5h13M9.5 24.5h8" stroke="#8aa2c2"/>'),
  gear: SVG('<circle cx="16" cy="16" r="10.5" fill="none" stroke="#6b7480" stroke-width="5" stroke-dasharray="4.1 4.15"/><circle cx="16" cy="16" r="9" fill="url(#g-grey)" stroke="#5b6573"/><circle cx="16" cy="16" r="3.5" fill="#fff" stroke="#5b6573"/>'),
  chart: SVG('<rect x="3" y="3" width="26" height="26" rx="1.5" fill="url(#g-paper)" stroke="#6b7c93"/><rect x="7" y="17" width="4" height="9" fill="url(#g-blue)"/><rect x="14" y="10" width="4" height="16" fill="url(#g-green)"/><rect x="21" y="13" width="4" height="13" fill="url(#g-orange)"/>'),
  history: SVG('<rect x="3" y="3" width="26" height="26" rx="1.5" fill="url(#g-paper)" stroke="#6b7c93"/><path d="M6 22l5-7 5 4 5-9 5 5" fill="none" stroke="#1c5fd1" stroke-width="2.2" stroke-linejoin="round"/><path d="M6 25l6-3 5 1 5-5 4 2" fill="none" stroke="#2a8a1c" stroke-width="1.8"/>'),
  clock: SVG('<circle cx="16" cy="16" r="13" fill="url(#g-paper)" stroke="#164ea8" stroke-width="2.2"/><path d="M16 8v8l5 4" fill="none" stroke="#163a7a" stroke-width="2.4" stroke-linecap="round"/>'),
  antenna: SVG('<path d="M16 12l-6 18M16 12l6 18M12 24h8" stroke="#4b5563" stroke-width="2" fill="none"/><circle cx="16" cy="11" r="2.6" fill="url(#g-red)"/><path d="M9.5 5a9 9 0 0 0 0 12M22.5 5a9 9 0 0 1 0 12M6 2a14 14 0 0 0 0 18M26 2a14 14 0 0 1 0 18" fill="none" stroke="#1c5fd1" stroke-width="1.8" stroke-linecap="round"/>'),
  mode: SVG('<path d="M5 11h17v-5l7 7-7 7v-5H5z" fill="url(#g-blue)" stroke="#164ea8" stroke-linejoin="round"/><path d="M27 21H10v5l-7-7 7-7v5h17z" fill="url(#g-green)" stroke="#1f6d16" stroke-linejoin="round" transform="translate(0 2)"/>'),
  book: SVG('<path d="M5 5h10a3 3 0 0 1 3 3v19a3 3 0 0 0-3-3H5z" fill="url(#g-blue)" stroke="#164ea8"/><path d="M27 5H18v22a3 3 0 0 1 3-3h6z" fill="url(#g-screen)" stroke="#164ea8"/><path d="M8 10h6M8 14h6M21 10h3M21 14h3" stroke="#fff" stroke-opacity=".8"/>'),
  control: SVG('<rect x="2.5" y="4" width="27" height="22" rx="2" fill="url(#g-paper)" stroke="#6b7c93"/><rect x="2.5" y="4" width="27" height="5" rx="2" fill="url(#g-blue)"/><path d="M8 13v10M16 13v10M24 13v10" stroke="#8aa2c2" stroke-width="2"/><rect x="5.5" y="15" width="5" height="3" fill="url(#g-green)"/><rect x="13.5" y="19" width="5" height="3" fill="url(#g-orange)"/><rect x="21.5" y="13.5" width="5" height="3" fill="url(#g-red)"/>'),
  folder: SVG('<path d="M3 8h9l3 3h14v16H3z" fill="url(#g-yellow)" stroke="#a46d0a" stroke-linejoin="round"/><path d="M3 13h26v14H3z" fill="#ffe27a" stroke="#a46d0a" stroke-linejoin="round"/>'),
  info: SVG('<circle cx="16" cy="16" r="13" fill="url(#g-blue)" stroke="#164ea8"/><circle cx="16" cy="9.5" r="2" fill="#fff"/><path d="M16 14v10" stroke="#fff" stroke-width="3.4" stroke-linecap="round"/>'),
  error: SVG('<circle cx="16" cy="16" r="13" fill="url(#g-red)" stroke="#8e1c10"/><path d="M11 11l10 10M21 11L11 21" stroke="#fff" stroke-width="3.4" stroke-linecap="round"/>'),
  warn: SVG('<path d="M16 3l14 25H2z" fill="url(#g-yellow)" stroke="#a46d0a" stroke-linejoin="round"/><path d="M16 11v8" stroke="#3b2a00" stroke-width="3" stroke-linecap="round"/><circle cx="16" cy="23.5" r="1.8" fill="#3b2a00"/>'),
  key: SVG('<circle cx="11" cy="16" r="7" fill="url(#g-yellow)" stroke="#a46d0a"/><circle cx="9" cy="16" r="2" fill="#a46d0a"/><path d="M17 14h12v4h-2v3h-3v-3h-2v3h-3v-3h-2z" fill="url(#g-yellow)" stroke="#a46d0a" stroke-linejoin="round"/>'),
  power: SVG('<rect x="2" y="2" width="28" height="28" rx="4" fill="url(#g-red)" stroke="#8e1c10"/><path d="M11 10a8 8 0 1 0 10 0" fill="none" stroke="#fff" stroke-width="2.6" stroke-linecap="round"/><path d="M16 7v9" stroke="#fff" stroke-width="2.6" stroke-linecap="round"/>'),
  restart: SVG('<rect x="2" y="2" width="28" height="28" rx="4" fill="url(#g-green)" stroke="#1f6d16"/><path d="M22.5 12A7.5 7.5 0 1 0 23 19" fill="none" stroke="#fff" stroke-width="2.6" stroke-linecap="round"/><path d="M24 6v7h-7z" fill="#fff"/>'),
  logoff: SVG('<rect x="2" y="2" width="28" height="28" rx="4" fill="url(#g-yellow)" stroke="#a46d0a"/><circle cx="12" cy="16" r="5" fill="none" stroke="#6b4300" stroke-width="2.4"/><path d="M17 16h9M23 16v4M20 16v3" stroke="#6b4300" stroke-width="2.4" stroke-linecap="round"/>'),
  user: SVG('<rect x="1" y="1" width="46" height="46" rx="4" fill="url(#g-blue)"/><path d="M1 34c10-6 20 3 46-4v13a4 4 0 0 1-4 4H5a4 4 0 0 1-4-4z" fill="url(#g-green)"/><path d="M24 30l-7 15M24 30l7 15M19.5 39h9" stroke="#3f4752" stroke-width="2" fill="none"/><circle cx="24" cy="28" r="3" fill="url(#g-red)"/><path d="M15 21a12 12 0 0 1 18 0M11 16a18 18 0 0 1 26 0" fill="none" stroke="#fff" stroke-width="2.2" stroke-linecap="round"/>', '0 0 48 48'),
  arrow: SVG('<path d="M6 16h17M16 8l8 8-8 8" fill="none" stroke="#fff" stroke-width="4" stroke-linecap="round" stroke-linejoin="round"/>'),
  progs: SVG('<path d="M4 16h20M17 8l8 8-8 8" fill="none" stroke="#fff" stroke-width="4" stroke-linecap="round" stroke-linejoin="round"/>'),
};
const icon = (name, size = 32, cls = '') => `<span class="ico ${cls}" style="width:${size}px;height:${size}px">${ICONS[name] || ICONS.control}</span>`;

const PAGE_ICON = {
  dashboard: 'computer', interfaces: 'network', data: 'chart', history: 'history', clients: 'users',
  mode: 'mode', wan: 'globe', lan: 'wifi', dns: 'book', signal: 'antenna', schedule: 'clock',
  firewall: 'shield', vpn: 'lock', mesh: 'cloud', mobile: 'mail', smsgw: 'gateway', remote: 'phone',
  bluetooth: 'bt', whatsapp: 'chat', forward: 'forward', diag: 'tools', logs: 'notepad', system: 'gear',
  control: 'control',
};
// friendlier XP-ish names for desktop shortcuts
const DESKTOP = [
  ['dashboard', 'My Router'], ['control', 'Control Panel'], ['wan', 'Internet (LTE)'], ['lan', 'Wi-Fi & LAN'],
  ['mesh', 'Cloudflare Mesh'], ['firewall', 'Firewall'], ['mobile', 'Messages'], ['logs', 'Logs'],
];

// ---------------------------------------------------------------- small helpers
const isTouch = () => matchMedia('(pointer: coarse)').matches;
const narrow = () => innerWidth < 720;
const viaMeshHost = () => /^100\.(9[6-9]|10[0-9]|11[01])\./.test(location.hostname) || location.hostname.startsWith('2606:4700:cf1') || location.hostname.startsWith('[2606:4700:cf1');
const pageById = id => id === 'control' ? CONTROL_PAGE : PAGES.find(p => p[0] === id && p.length > 1);
let USER = localStorage.getItem('xp.user') || '';

// ---------------------------------------------------------------- balloons (replace the original toasts)
function toast(msg, err = false) {
  const t = document.createElement('div');
  t.className = 'balloon' + (err ? ' err' : '');
  t.innerHTML = `<div class="bl-h">${icon(err ? 'error' : 'info', 16)}<b>${err ? 'KStick — problem' : 'KStick'}</b><button class="bl-x" type="button" aria-label="Close"></button></div><div class="bl-b"></div>`;
  t.querySelector('.bl-b').textContent = msg;
  t.querySelector('.bl-x').onclick = () => t.remove();
  $('#toasts').appendChild(t);
  while ($('#toasts').children.length > 4) $('#toasts').firstElementChild.remove();
  setTimeout(() => t.remove(), err ? 9000 : 4500);
}

// ---------------------------------------------------------------- window manager
const WINS = new Map();          // page id -> window object
let Z = 20, ACTIVE = null, BUILD = null, renderQ = Promise.resolve(), cascade = 0;

function makeWin(id, title, ic) {
  const el = document.createElement('div');
  el.className = 'win';
  el.dataset.id = id;
  el.innerHTML = `
    <div class="tb">${icon(ic, 16, 'tb-ic')}<span class="tb-t"></span>
      <span class="tb-b"><button class="tbb min" data-w="min" title="Minimize" aria-label="Minimize"></button><button class="tbb max" data-w="max" title="Maximize" aria-label="Maximize"></button><button class="tbb close" data-w="close" title="Close" aria-label="Close"></button></span></div>
    <div class="mb"><span data-m="file">File</span><span data-m="view">View</span><span data-m="go">Go</span><span data-m="help">Help</span></div>
    <div class="tbar">
      <button class="tbtn" data-w="refresh" title="Refresh">${icon('restart', 20)}<span>Refresh</span></button>
      <span class="tsep"></span>
      <button class="tbtn" data-open="dashboard" title="My Router">${icon('computer', 20)}<span>My Router</span></button>
      <button class="tbtn" data-open="control" title="Control Panel">${icon('control', 20)}<span>Control Panel</span></button>
    </div>
    <div class="addr"><span class="addr-l">Address</span><div class="addr-box">${icon(ic, 14)}<span class="addr-u"></span></div></div>
    <div class="wv"></div>
    <div class="sb"><span class="sb-t">Done</span><span class="sb-z">${icon(viaMeshHost() ? 'cloud' : 'globe', 14)}${viaMeshHost() ? 'Cloudflare Mesh' : 'Local network'}</span></div>
    <div class="grip"></div>`;
  const w = { id, el, title, ic, open: true, min: false, max: false, timers: [], view: el.querySelector('.wv') };
  w.setTitle = t => { w.title = t; el.querySelector('.tb-t').textContent = t; el.querySelector('.addr-u').textContent = `${location.origin}/#/${id}`; updateTasks(); };
  w.status = t => { el.querySelector('.sb-t').textContent = t; };
  w.setTitle(title);

  // position: cascade from the top-left; on small screens every window is maximized
  const W = Math.min(960, innerWidth - 24), H = Math.min(660, innerHeight - 60);
  const off = (cascade++ % 8) * 26;
  Object.assign(el.style, { left: Math.max(4, Math.min(110 + off, innerWidth - W - 4)) + 'px', top: Math.max(4, 16 + off) + 'px', width: W + 'px', height: H + 'px' });
  if (narrow()) setMax(w, true);

  el.addEventListener('pointerdown', () => focusWin(w), true);
  el.querySelector('.tb').addEventListener('dblclick', e => { if (!e.target.closest('.tbb')) setMax(w, !w.max); });
  el.addEventListener('click', e => {
    const b = e.target.closest('[data-w]');
    if (b) { const a = b.dataset.w; if (a === 'min') minimize(w); if (a === 'max') setMax(w, !w.max); if (a === 'close') closeWin(w); if (a === 'refresh') renderWin(w); return; }
    const m = e.target.closest('[data-m]');
    if (m) { const r = m.getBoundingClientRect(); popMenu(r.left, r.bottom, winMenu(w, m.dataset.m), m); }
  });
  dragify(w);
  $('#windows').appendChild(el);
  WINS.set(id, w);
  focusWin(w);
  return w;
}

function winMenu(w, which) {
  if (which === 'file') return [{ label: 'Refresh', fn: () => renderWin(w), key: 'F5' }, { sep: 1 }, { label: 'Close', fn: () => closeWin(w), key: 'Alt+F4' }];
  if (which === 'view') return [{ label: w.max ? 'Restore' : 'Maximize', fn: () => setMax(w, !w.max) }, { label: 'Minimize', fn: () => minimize(w) }, { sep: 1 }, { label: 'Refresh', fn: () => renderWin(w) }];
  if (which === 'go') return [{ label: 'My Router', fn: () => openWin('dashboard') }, { label: 'Control Panel', fn: () => openWin('control') }, { sep: 1 },
    ...PAGES.filter(p => p.length > 1 && pageVisible(p)).map(p => ({ label: p[1], ic: PAGE_ICON[p[0]], fn: () => openWin(p[0]) }))];
  return [{ label: 'About KStick XP', ic: 'info', fn: about }];
}

function dragify(w) {
  const el = w.el, tb = el.querySelector('.tb'), grip = el.querySelector('.grip');
  const drag = (handle, onMove) => handle.addEventListener('pointerdown', e => {
    if (e.button !== 0 || e.target.closest('.tbb') || w.max) return;
    e.preventDefault();
    const sx = e.clientX, sy = e.clientY, r = el.getBoundingClientRect();
    handle.setPointerCapture(e.pointerId);
    el.classList.add('moving');
    const mv = ev => onMove(ev.clientX - sx, ev.clientY - sy, r);
    const up = () => { handle.removeEventListener('pointermove', mv); handle.removeEventListener('pointerup', up); handle.removeEventListener('pointercancel', up); el.classList.remove('moving'); };
    handle.addEventListener('pointermove', mv); handle.addEventListener('pointerup', up); handle.addEventListener('pointercancel', up);
  });
  drag(tb, (dx, dy, r) => {
    el.style.left = Math.min(innerWidth - 80, Math.max(80 - r.width, r.left + dx)) + 'px';
    el.style.top = Math.min(innerHeight - 70, Math.max(0, r.top + dy)) + 'px';
  });
  drag(grip, (dx, dy, r) => { el.style.width = Math.max(340, r.width + dx) + 'px'; el.style.height = Math.max(220, r.height + dy) + 'px'; });
}

function focusWin(w) {
  if (!w || !w.open) return;
  if (ACTIVE !== w) { w.el.style.zIndex = ++Z; }
  ACTIVE = w;
  for (const x of WINS.values()) x.el.classList.toggle('inactive', x !== w);
  if (location.hash !== '#/' + w.id) history.replaceState(null, '', '#/' + w.id);
  updateTasks();
}
function topWin() { let t = null; for (const w of WINS.values()) if (!w.min && (!t || +w.el.style.zIndex > +t.el.style.zIndex)) t = w; return t; }
function minimize(w) {
  w.min = true; w.el.classList.add('hidden');
  if (ACTIVE === w) { ACTIVE = null; const t = topWin(); if (t) focusWin(t); else { for (const x of WINS.values()) x.el.classList.add('inactive'); history.replaceState(null, '', location.pathname); } }
  updateTasks();
}
function restore(w) { w.min = false; w.el.classList.remove('hidden'); focusWin(w); }
function setMax(w, on) {
  w.max = on; w.el.classList.toggle('max', on);
  const b = w.el.querySelector('.tbb.max, .tbb.rest'); if (b) { b.className = 'tbb ' + (on ? 'rest' : 'max'); b.title = on ? 'Restore Down' : 'Maximize'; }
}
function stopTimers(w) { w.timers.forEach(clearInterval); w.timers = []; }
function closeWin(w) {
  w.open = false; stopTimers(w); w.el.remove(); WINS.delete(w.id);
  if (ACTIVE === w) { ACTIVE = null; const t = topWin(); if (t) focusWin(t); else history.replaceState(null, '', location.pathname); }
  updateTasks();
}
function closeAll() { for (const w of [...WINS.values()]) closeWin(w); }

function openWin(id) {
  hideStart();
  const p = pageById(id);
  if (!p || (p === CONTROL_PAGE ? VIEWER : !pageVisible(p))) { toast(`“${id}” is not available on this router.`, true); return null; }
  let w = WINS.get(id);
  if (w) { if (w.min) restore(w); else focusWin(w); return w; }
  w = makeWin(id, p[1], PAGE_ICON[id]);
  w.page = p;
  renderWin(w);
  return w;
}

/* Windows are drawn one at a time. Page functions register their polling with every(), which has no
   window argument; serialising the renders means BUILD always names the window being drawn, so each
   timer is tied to its own window (stopped when it closes or redraws, paused while it is minimized).
   It also keeps a slow stick from being hit by several page loads at once. */
function renderWin(w) {
  if (!w || !w.open) return Promise.resolve();
  stopTimers(w);
  w.status('Opening page…');
  w.el.classList.add('busy');
  const job = renderQ.then(async () => {
    if (!w.open) return;
    BUILD = w;
    w.view.innerHTML = '<p class="muted loading">Loading…</p>';
    try { await w.page[2](w.view); w.status('Done'); }
    catch (e) { w.view.innerHTML = `<div class="errbox">${icon('error', 32)}<div><b>This page could not be displayed</b><p>${esc(e.message)}</p><button class="btn" data-w="refresh">Try again</button></div></div>`; w.status('Error'); }
    finally { if (BUILD === w) BUILD = null; w.el.classList.remove('busy'); }
  });
  renderQ = job.catch(() => {});
  return job;
}
function refreshAll() { for (const w of WINS.values()) renderWin(w); }

// expected by pages.js ---------------------------------------------------------------
function every(ms, fn) {
  const w = BUILD || ACTIVE;
  if (!w) return;
  let busy = false;
  const t = setInterval(async () => {
    if (!w.open) { clearInterval(t); return; }
    if (busy || w.min || document.hidden) return;   // minimized windows and background tabs don't poll
    busy = true;
    try { await fn(); } catch { /* the next tick retries */ } finally { busy = false; }
  }, ms);
  w.timers.push(t);
}
// pages call route() after a change to redraw "the current page": that is the window being drawn or the one in use
function route() { const w = BUILD || ACTIVE; if (w) renderWin(w); }

// ---------------------------------------------------------------- taskbar
function updateTasks() {
  const t = $('#tasks');
  if (!t) return;
  t.innerHTML = [...WINS.values()].map(w => `<button class="task ${w === ACTIVE && !w.min ? 'on' : ''}" data-task="${esc(w.id)}" title="${esc(w.title)}">${icon(w.ic, 16)}<span>${esc(w.title)}</span></button>`).join('');
}
function taskClick(id) {
  const w = WINS.get(id); if (!w) return;
  if (w.min) restore(w); else if (w === ACTIVE) minimize(w); else focusWin(w);
}

// tray: WAN, clients, Mesh, battery, pending change; clock
let TRAY = { wan: null, clients: '', mesh: null, batt: null };
function drawTray() {
  const w = TRAY.wan, m = TRAY.mesh, b = TRAY.batt;
  const items = [];
  if (PEND) items.push(`<span class="ti blink" data-tray="pending" title="A change is waiting for confirmation">${icon('warn', 16)}</span>`);
  if (b) items.push(`<span class="ti" title="Battery ${b.percent ?? '?'}%${b.charging ? ', charging' : ''}"><span class="batt"><i style="width:${Math.max(4, b.percent || 0)}%"></i></span></span>`);
  if (!VIEWER) items.push(`<span class="ti ${m && m.status === 'Connected' ? '' : 'dim'}" data-tray="mesh" title="${esc(m ? `Cloudflare Mesh: ${m.status || m.service || 'unknown'}${m.ipv4 ? ' (' + m.ipv4 + ')' : ''}` : 'Cloudflare Mesh: checking…')}${viaMeshHost() ? '\nThis page is open over Mesh.' : ''}">${icon('cloud', 16)}</span>`);
  items.push(`<span class="ti" data-tray="clients" title="${esc(TRAY.clients || 'Clients')}">${icon('users', 16)}</span>`);
  items.push(`<span class="ti ${w && w.ok ? '' : 'dim'}" data-tray="wan" title="${esc(w ? w.text : 'Internet: checking…')}">${icon('network', 16)}${w && !w.ok ? '<i class="ti-x"></i>' : ''}</span>`);
  $('#tray-icons').innerHTML = items.join('');
}
function updatePills(s) {   // called by the Dashboard on every refresh, and by the shell's own status poll
  const w = s.wan || {}, names = { wwan0: 'LTE', usb0: 'USB host', bnep0: 'phone (Bluetooth)', wlan0: 'Wi-Fi' };
  const ok = w.active && w.active !== 'none';
  TRAY.wan = { ok, text: ok ? `Internet: connected via ${names[w.active] || w.active}` : 'Internet: offline' };
  TRAY.clients = s.mode === 'client' ? `Wi-Fi client of ${(s.wifi_uplink || {}).ssid || '(not connected)'}` : `${(s.clients || {}).wifi ?? 0} Wi-Fi client${(s.clients || {}).wifi === 1 ? '' : 's'}`;
  TRAY.batt = s.battery || null;
  if (s.system) $('#sm-host') && ($('#sm-host').textContent = `${s.system.hostname} · up ${fmtDur(s.system.uptime)}`);
  if (s.mode && s.mode !== MODE) { MODE = s.mode; buildMenus(); }
  drawTray();
}
function tick() { const d = new Date(); $('#clock').textContent = d.toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' }); $('#clock').title = d.toLocaleDateString([], { weekday: 'long', year: 'numeric', month: 'long', day: 'numeric' }); }

// ---------------------------------------------------------------- Start menu, All Programs, desktop icons
const CONTROL_PAGE = ['control', 'Control Panel', pgControl];
function sections() {
  const out = []; let cur = null;
  for (const p of PAGES) { if (p.length === 1) { cur = { name: p[0], items: [] }; out.push(cur); } else if (pageVisible(p)) cur.items.push(p); }
  return out.filter(s => s.items.length);
}
function buildMenus() {
  const vis = id => { const p = pageById(id); return p && (p === CONTROL_PAGE ? !VIEWER : pageVisible(p)); };
  $('#icons').innerHTML = DESKTOP.filter(([id]) => vis(id)).map(([id, label]) =>
    `<button class="dicon" data-dicon="${id}" type="button">${icon(PAGE_ICON[id], 32)}<span>${esc(label)}</span></button>`).join('');
  const big = (id, title, sub) => vis(id) ? `<a class="sm-item big" data-open="${id}">${icon(PAGE_ICON[id], 32)}<span><b>${esc(title)}</b><small>${esc(sub)}</small></span></a>` : '';
  const item = id => { const p = pageById(id); return vis(id) ? `<a class="sm-item" data-open="${id}">${icon(PAGE_ICON[id], 24)}<span>${esc(p[1])}</span></a>` : ''; };
  $('#sm-left').innerHTML = big('wan', 'Internet', 'WAN / LTE') + big('mobile', 'Messages', 'SMS & USSD')
    + '<div class="sm-sep"></div>' + ['dashboard', 'lan', 'signal', 'firewall', 'mesh', 'data'].map(item).join('')
    + '<div class="sm-sep"></div><a class="sm-all" id="sm-all">All Programs <span class="sm-allarrow">' + ICONS.progs + '</span></a>';
  const r = (id, label, b = true) => vis(id) ? `<a class="sm-ritem ${b ? 'b' : ''}" data-open="${id}">${icon(PAGE_ICON[id], 24)}<span>${esc(label)}</span></a>` : '';
  $('#sm-right').innerHTML = r('dashboard', 'My Router') + r('interfaces', 'My Network') + r('clients', 'Network Clients') + r('history', 'My History')
    + '<div class="sm-sep r"></div>' + r('control', 'Control Panel', false) + r('mode', 'Operating Mode', false) + r('system', 'System Properties', false)
    + '<div class="sm-sep r"></div>' + `<a class="sm-ritem" data-act="about">${icon('info', 24)}<span>Help and Support</span></a>` + r('diag', 'Run Diagnostics…', false) + r('logs', 'Event Viewer', false)
    + `<div class="sm-host" id="sm-host"></div>`;
  $('#allprogs').innerHTML = sections().map(s => `<div class="fl-sec">${icon('folder', 16)}<b>${esc(s.name)}</b></div>`
    + s.items.map(p => `<a class="fl-item" data-open="${p[0]}">${icon(PAGE_ICON[p[0]], 16)}<span>${esc(p[1])}</span></a>`).join('')).join('');
  $('#quick').innerHTML = ['dashboard', 'mesh', 'logs'].filter(vis).map(id => `<button class="qi" data-open="${id}" title="${esc(pageById(id)[1])}" type="button">${icon(PAGE_ICON[id], 18)}</button>`).join('');
}
function toggleStart() { $('#startmenu').classList.contains('hidden') ? showStart() : hideStart(); }
function showStart() { $('#startmenu').classList.remove('hidden'); $('#start').classList.add('on'); }
function hideStart() { $('#startmenu').classList.add('hidden'); $('#allprogs').classList.add('hidden'); $('#start').classList.remove('on'); }
function showAllProgs() {
  const fl = $('#allprogs'), a = $('#sm-all').getBoundingClientRect();
  fl.classList.remove('hidden');
  const h = fl.offsetHeight, top = Math.max(4, Math.min(a.top - h + a.height + 2, innerHeight - 34 - h));
  if (narrow()) Object.assign(fl.style, { left: '4px', right: '4px', top: Math.max(4, innerHeight - 34 - h) + 'px' });
  else Object.assign(fl.style, { left: Math.min(a.right - 4, innerWidth - fl.offsetWidth - 4) + 'px', right: '', top: top + 'px' });
}

// ---------------------------------------------------------------- context menus
function popMenu(x, y, items, anchor) {
  const m = $('#menu');
  m.innerHTML = items.map((it, i) => it.sep ? '<div class="cm-sep"></div>'
    : `<a class="cm-item ${it.disabled ? 'dis' : ''} ${it.bold ? 'b' : ''}" data-i="${i}">${it.ic ? icon(it.ic, 16) : '<span class="ico" style="width:16px;height:16px"></span>'}<span>${esc(it.label)}</span>${it.key ? `<small>${esc(it.key)}</small>` : ''}</a>`).join('');
  m.classList.remove('hidden');
  m.style.left = Math.max(0, Math.min(x, innerWidth - m.offsetWidth - 2)) + 'px';
  m.style.top = Math.max(0, Math.min(y, innerHeight - m.offsetHeight - 2)) + 'px';
  m.onclick = e => { const a = e.target.closest('[data-i]'); if (!a) return; const it = items[+a.dataset.i]; if (it.disabled) return; hideMenu(); it.fn(); };
  if (anchor) anchor.classList.add('open');
  m._anchor = anchor;
}
function hideMenu() { const m = $('#menu'); m.classList.add('hidden'); if (m._anchor) m._anchor.classList.remove('open'); m._anchor = null; }

// ---------------------------------------------------------------- XP dialogs (about, log off, turn off)
function xpBox({ title, html, buttons, cls = '' }) {
  return new Promise(resolve => {
    const o = $('#overlay');
    o.className = 'overlay ' + cls;
    o.innerHTML = `<div class="xpbox" role="dialog" aria-modal="true"><div class="tb"><span class="tb-t">${esc(title)}</span><span class="tb-b"><button class="tbb close" data-v="" aria-label="Close"></button></span></div>
      <div class="xpbox-b">${html}</div><div class="xpbox-btns">${buttons.map(b => `<button class="btn ${b.cls || ''}" data-v="${esc(b.value)}" type="button">${esc(b.label)}</button>`).join('')}</div></div>`;
    const done = v => { o.className = 'overlay hidden'; o.innerHTML = ''; document.removeEventListener('keydown', key); resolve(v); };
    const key = e => { if (e.key === 'Escape') done(''); };
    document.addEventListener('keydown', key);
    o.onclick = e => { const b = e.target.closest('[data-v]'); if (b) done(b.dataset.v); };
    const f = o.querySelector('.xpbox-btns .btn'); if (f) f.focus();
  });
}
async function about() {
  hideStart();
  let s = null; try { s = await api('GET', '/status'); } catch { }
  const sy = (s && s.system) || {};
  await xpBox({ title: 'About KStick XP', buttons: [{ label: 'OK', value: 'ok', cls: 'primary' }], html: `
    <div class="about">${icon('logo', 48)}<div><div class="about-name">KStick<sup>xp</sup></div><p>Router web desktop for the 4G LTE stick.</p>
    ${s ? kv([['Host', esc(sy.hostname)], ['Model', esc(sy.model)], ['Uptime', fmtDur(sy.uptime)],
      ['Memory', `${fmtBytes(sy.mem_total - sy.mem_available)} used of ${fmtBytes(sy.mem_total)}`], ['Opened via', viaMeshHost() ? 'Cloudflare Mesh' : esc(location.hostname)]]) : ''}
    <p class="small muted">Windows here are the router's regular settings pages. Changes that could cut you off (firewall, mode, Mesh) are undone automatically unless confirmed.</p></div></div>` });
}
async function logOff() {
  hideStart();
  const v = await xpBox({ title: 'Log Off KStick', cls: 'gray', buttons: [{ label: 'Cancel', value: '' }],
    html: `<div class="bigbtns"><button class="bigbtn" data-v="logoff" type="button">${icon('logoff', 34)}<span>Log Off</span></button></div>` });
  if (v !== 'logoff') return;
  await api('POST', '/logout').catch(() => {});
  CSRF = ''; showLogin();
}
async function turnOff() {
  hideStart();
  const v = await xpBox({ title: 'Turn off KStick', cls: 'gray', buttons: [{ label: 'Cancel', value: '' }],
    html: `<div class="bigbtns"><button class="bigbtn" data-v="logoff" type="button">${icon('logoff', 34)}<span>Log Off</span></button>
      <button class="bigbtn" data-v="restart" type="button">${icon('restart', 34)}<span>Restart</span></button>
      <button class="bigbtn dis" type="button" disabled title="The stick has no remote power-on: turning it off would need someone to replug it.">${icon('power', 34)}<span>Turn Off</span></button></div>
      <p class="small center">Restart reboots the router: everything goes offline for about a minute.${viaMeshHost() ? ' Cloudflare Mesh starts again by itself; this page reconnects when it is back.' : ''}</p>` });
  if (v === 'logoff') { await api('POST', '/logout').catch(() => {}); CSRF = ''; showLogin(); }
  if (v === 'restart') {
    if (!await act(() => api('POST', '/system/reboot'))) return;
    restarting();
  }
}
// "KStick is restarting" screen; reload once the web UI answers again
function restarting() {
  closeAll(); stopShell();
  const o = $('#overlay');
  o.className = 'overlay boot-like';
  o.innerHTML = `<div class="restart-box">${icon('logo', 64)}<p>KStick is restarting…</p><div class="boot-bar"><i></i><i></i><i></i></div><p class="small" id="rs-note">Waiting for the router</p></div>`;
  const t0 = Date.now();
  setTimeout(function poll() {
    fetch('/api/session', { credentials: 'same-origin', cache: 'no-store' }).then(r => { if (r.status === 200 || r.status === 401) location.reload(); else throw 0; })
      .catch(() => { const n = $('#rs-note'); if (n) n.textContent = `Waiting for the router · ${Math.round((Date.now() - t0) / 1000)} s`; setTimeout(poll, 4000); });
  }, 20000);
}

// ---------------------------------------------------------------- Control Panel (window)
async function pgControl(v) {
  let s = null; try { s = await api('GET', '/status'); } catch { }
  const sy = (s && s.system) || {};
  v.innerHTML = `<div class="xsplit">
    <aside class="xpane">
      <div class="xp-box"><div class="xp-h">Router Tasks</div><div class="xp-c">
        <a data-open="dashboard">${icon('computer', 16)}View router status</a>
        <a data-open="logs">${icon('notepad', 16)}View logs</a>
        <a data-open="diag">${icon('tools', 16)}Run diagnostics</a>
        <a data-act="turnoff">${icon('restart', 16)}Restart the router</a></div></div>
      <div class="xp-box"><div class="xp-h">See Also</div><div class="xp-c">
        <a data-open="mesh">${icon('cloud', 16)}Cloudflare Mesh</a><a data-act="about">${icon('info', 16)}Help and Support</a>
        ${VIEWER ? '' : `<a data-act="classic">${icon('computer', 16)}Switch to Classic View</a>`}</div></div>
      ${s ? `<div class="xp-box"><div class="xp-h">Details</div><div class="xp-c det"><b>${esc(sy.hostname)}</b><br>${esc(sy.model)}<br>Up ${fmtDur(sy.uptime)}<br>Mode: ${esc(s.mode || 'router')}</div></div>` : ''}
    </aside>
    <div class="xmain"><h2 class="cp-title">Pick a category</h2>
      ${sections().map(sec => `<div class="cp-sec"><h4>${esc(sec.name)}</h4><div class="cp-items">${sec.items.map(p =>
        `<a class="cp-item" data-open="${p[0]}">${icon(PAGE_ICON[p[0]], 32)}<span>${esc(p[1])}</span></a>`).join('')}</div></div>`).join('')}
    </div></div>`;
}

// ---------------------------------------------------------------- confirm-or-revert prompt (risky changes)
let PEND = null, pendTick;
function showPending(p) {
  const el = $('#pending');
  if (!el) return;
  clearInterval(pendTick);
  PEND = p && p.id ? { ...p, until: Date.now() + p.seconds_left * 1000 } : null;
  drawTray();
  if (!PEND) { el.classList.add('hidden'); el.innerHTML = ''; return; }
  const draw = () => {
    const left = Math.max(0, Math.round((PEND.until - Date.now()) / 1000));
    el.innerHTML = `<div class="bl-h">${icon('warn', 16)}<b>Keep these settings?</b></div>
      <div class="bl-b">Not confirmed yet: <b>${esc((PEND.what || []).join(', '))}</b>.<br>Undone automatically in <b>${left} s</b> — so if the change cut you off, just wait.</div>
      <div class="btns"><button class="btn primary" data-p="confirm" type="button">Keep changes</button><button class="btn" data-p="revert" type="button">Undo now</button></div>`;
    if (!left) {
      clearInterval(pendTick);
      setTimeout(() => api('GET', '/pending').then(x => { showPending(x); toast('The change was not confirmed and has been undone.', true); refreshAll(); }).catch(() => {}), 4000);
    }
  };
  el.classList.remove('hidden');
  draw();
  pendTick = setInterval(draw, 1000);
}

// read-only viewer mode (lofi-viewer): fewer pages, no admin actions; the server enforces it, this only tidies the UI
function setViewer(on) {
  VIEWER = !!on;
  document.body.classList.toggle('ro', VIEWER);
  $('.wl-left p').textContent = VIEWER ? 'Read-only access — sign in with your viewer account' : 'To begin, sign in with the device account';
  $('.wl-note').innerHTML = VIEWER ? 'This is a read-only view of the router status.<br>Nothing can be changed from here.' : "Use the device's system account and its password.<br>Settings that could cut off your connection are undone unless you confirm them.";
}

// ---------------------------------------------------------------- session: welcome screen <-> desktop
let shellTimers = [];
function stopShell() { shellTimers.forEach(clearInterval); shellTimers = []; }
function showLogin() {
  closeAll(); stopShell(); hideStart(); hideMenu();
  clearInterval(pendTick); PEND = null; $('#pending').classList.add('hidden');
  $('#desktop').classList.add('hidden');
  $('#login').classList.remove('hidden');
  const un = $('#login-form [name=username]'), pw = $('#login-form [name=password]');
  pw.value = '';
  if (!un.value) un.value = USER;
  (un.value ? pw : un).focus();
}
async function showDesktop() {
  $('#login').classList.add('hidden');
  $('#desktop').classList.remove('hidden');
  $('#sm-user').textContent = (USER || 'Administrator') + (VIEWER ? ' (read-only)' : '');
  [MODS, MODE] = await Promise.all([api('GET', '/modules').catch(() => ({})), api('GET', '/mode').then(m => m.mode).catch(() => 'router')]);
  buildMenus(); drawTray(); tick();
  stopShell();
  shellTimers.push(setInterval(tick, 10000));
  const status = () => { const d = WINS.get('dashboard'); if (document.hidden || (d && !d.min)) return; api('GET', '/status').then(updatePills).catch(() => {}); };
  const mesh = () => { if (!document.hidden) api('GET', '/mesh').then(m => { TRAY.mesh = m; drawTray(); }).catch(() => {}); };
  status();
  shellTimers.push(setInterval(status, 20000));
  if (!VIEWER) { mesh(); shellTimers.push(setInterval(mesh, 120000)); api('GET', '/pending').then(showPending).catch(() => {}); }
  const id = (location.hash.match(/^#\/(\w+)/) || [])[1];
  openWin(id && pageById(id) ? id : 'dashboard');
}

// ---------------------------------------------------------------- wiring
function wire() {
  for (const [id, ic] of [['boot-mark', 'logo'], ['wl-mark', 'logo'], ['start-mark', 'logo'], ['ic-logoff', 'logoff'], ['ic-turnoff', 'power']]) $('#' + id).innerHTML = ICONS[ic];
  $('#wl-pic').innerHTML = ICONS.user; $('#sm-pic').innerHTML = ICONS.user;
  $('.wl-go').innerHTML = ICONS.arrow;
  $('#wl-host').textContent = viaMeshHost() ? `Connected over Cloudflare Mesh · ${location.hostname}` : location.hostname;

  $('#login-form').addEventListener('submit', async e => {
    e.preventDefault();
    const f = e.target, btn = f.querySelector('button'), un = $('[name=username]', f), pw = $('[name=password]', f);
    btn.disabled = true; $('#login-error').textContent = '';
    try {
      const r = await api('POST', '/login', { username: un.value.trim(), password: pw.value });
      CSRF = r.csrf; pw.value = ''; setViewer(r.role === 'viewer');
      USER = un.value.trim(); localStorage.setItem('xp.user', USER);
      $('#login').classList.add('fade');
      setTimeout(() => $('#login').classList.remove('fade'), 600);
      showDesktop();
    } catch (err) { $('#login-error').textContent = err.message; pw.select(); }
    finally { btn.disabled = false; }
  });

  $('#start').onclick = e => { e.stopPropagation(); toggleStart(); };
  $('#sm-logoff').onclick = logOff;
  $('#sm-turnoff').onclick = turnOff;
  $('#tasks').addEventListener('click', e => { const b = e.target.closest('[data-task]'); if (b) taskClick(b.dataset.task); });
  $('#tasks').addEventListener('contextmenu', e => {
    const b = e.target.closest('[data-task]'); if (!b) return; e.preventDefault();
    const w = WINS.get(b.dataset.task); if (!w) return;
    popMenu(e.clientX, e.clientY - 90, [{ label: 'Restore', disabled: !w.min && !w.max, fn: () => { if (w.min) restore(w); else setMax(w, false); } },
      { label: 'Minimize', disabled: w.min, fn: () => minimize(w) }, { label: 'Maximize', disabled: w.max, fn: () => { if (w.min) restore(w); setMax(w, true); } },
      { sep: 1 }, { label: 'Close', bold: true, fn: () => closeWin(w), key: 'Alt+F4' }]);
  });
  $('#tray').addEventListener('click', e => {
    const t = e.target.closest('[data-tray]'); if (!t) return;
    const a = t.dataset.tray;
    if (a === 'pending') { $('#pending').classList.toggle('hidden', !PEND); return; }
    openWin({ wan: 'wan', mesh: 'mesh', clients: MODE === 'client' ? 'mode' : 'clients' }[a]);
  });

  // desktop icons: click selects, double-click (or a tap on touch screens) opens
  $('#icons').addEventListener('click', e => {
    const b = e.target.closest('[data-dicon]');
    $$('.dicon', $('#icons')).forEach(x => x.classList.toggle('sel', x === b));
    if (b && (isTouch() || e.detail === 0)) openWin(b.dataset.dicon);   // detail 0: keyboard Enter/Space
  });
  $('#icons').addEventListener('dblclick', e => { const b = e.target.closest('[data-dicon]'); if (b && !isTouch()) openWin(b.dataset.dicon); });
  $('#desktop').addEventListener('contextmenu', e => {
    if (e.target.closest('.win, .taskbar, .startmenu, .flyout, .balloon') ) return;
    e.preventDefault();
    popMenu(e.clientX, e.clientY, [{ label: 'Arrange Icons By Name', fn: () => { DESKTOP.sort((a, b) => a[1].localeCompare(b[1])); buildMenus(); } },
      { label: 'Refresh', fn: refreshAll, key: 'F5' }, { sep: 1 }, { label: 'Control Panel', ic: 'control', fn: () => openWin('control') },
      { label: 'Properties', ic: 'gear', fn: () => openWin('system') }]);
  });

  // generic open/act links anywhere (start menu, control panel, toolbars)
  document.addEventListener('click', e => {
    const o = e.target.closest('[data-open]');
    if (o && !o.closest('#icons')) { e.preventDefault(); hideMenu(); openWin(o.dataset.open); return; }
    const a = e.target.closest('[data-act]');
    if (a) { e.preventDefault(); ({ about, turnoff: turnOff, logoff: logOff, classic: () => { location.href = '/?ui=classic'; } }[a.dataset.act] || (() => {}))(); return; }
    if (e.target.closest('#sm-all')) { e.preventDefault(); $('#allprogs').classList.contains('hidden') ? showAllProgs() : $('#allprogs').classList.add('hidden'); return; }
    if (!e.target.closest('#startmenu, #allprogs, #start')) hideStart();
    if (!e.target.closest('#menu, [data-m]')) hideMenu();
  });
  $('#sm-left').addEventListener('mouseover', e => { if (!isTouch() && e.target.closest('#sm-all')) showAllProgs(); else if (!isTouch() && e.target.closest('.sm-item')) $('#allprogs').classList.add('hidden'); });
  $('#pending').addEventListener('click', async e => {
    const b = e.target.closest('[data-p]');
    if (!b || !PEND) return;
    const a = b.dataset.p;
    b.disabled = true;
    if (await act(() => api('POST', `/pending/${encodeURIComponent(PEND.id)}/${a}`))) { showPending(null); if (a === 'revert') setTimeout(refreshAll, 1500); }
    b.disabled = false;
  });

  document.addEventListener('keydown', e => {
    if (e.key === 'Escape') { hideStart(); hideMenu(); }
    if (e.key === 'F5' && ACTIVE && !$('#desktop').classList.contains('hidden')) { e.preventDefault(); renderWin(ACTIVE); }
    if (e.altKey && e.key === 'F4' && ACTIVE) { e.preventDefault(); closeWin(ACTIVE); }
    if ((e.key === 'Meta' || e.key === 'OS') && !$('#desktop').classList.contains('hidden')) toggleStart();
  });
  addEventListener('resize', () => { if (narrow()) for (const w of WINS.values()) setMax(w, true); });
  addEventListener('hashchange', () => { const id = (location.hash.match(/^#\/(\w+)/) || [])[1]; if (id && CSRF && pageById(id) && (!ACTIVE || ACTIVE.id !== id)) openWin(id); });
}

// ---------------------------------------------------------------- start
(async () => {
  wire();
  const firstBoot = !sessionStorage.getItem('xp.booted');
  if (firstBoot) { sessionStorage.setItem('xp.booted', '1'); $('#boot').classList.remove('hidden'); }
  const minBoot = new Promise(r => setTimeout(r, firstBoot ? 1600 : 0));
  let ok = false;
  try {
    const r = await fetch('/api/session', { credentials: 'same-origin', cache: 'no-store' });
    const s = await r.json().catch(() => ({}));
    setViewer(s.role === 'viewer' || s.viewer === true);
    if (r.ok) { CSRF = s.csrf; if (s.user || s.username) USER = s.user || s.username; ok = true; }
  } catch { }
  await minBoot;
  $('#boot').classList.add('hidden');
  ok ? showDesktop() : showLogin();
})();
