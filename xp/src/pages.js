'use strict';
/* KStick Router web UI — page logic (derived from the original SPA).
   The XP desktop shell (src/shell.js) provides: toast, every, route, showLogin, showPending, updatePills. */

const $ = (s, el = document) => el.querySelector(s);
const $$ = (s, el = document) => [...el.querySelectorAll(s)];
const esc = v => String(v ?? '').replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
let CSRF = '';

// ---------------------------------------------------------------- API
async function api(method, path, body) {
  const opt = { method, headers: {}, credentials: 'same-origin' };
  if (method !== 'GET') opt.headers['X-CSRF-Token'] = CSRF;
  if (body !== undefined) { opt.headers['Content-Type'] = 'application/json'; opt.body = JSON.stringify(body); }
  if (method === 'GET' && typeof AbortSignal !== 'undefined' && AbortSignal.timeout) opt.signal = AbortSignal.timeout(30000);
  const r = await fetch('/api' + path, opt);
  let data = null;
  try { data = await r.json(); } catch { /* empty */ }
  if (r.status === 401 && path !== '/login' && path !== '/session') { showLogin(); throw new Error('Session expired — please sign in again'); }
  if (!r.ok) throw new Error((data && data.error) || r.statusText);
  if (data && typeof data === 'object' && !Array.isArray(data) && 'pending' in data) showPending(data.pending);
  return data;
}

// ---------------------------------------------------------------- formatting
const fmtBytes = n => { n = Number(n) || 0; const u = ['B', 'KB', 'MB', 'GB', 'TB']; let i = 0; while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; } return (i ? n.toFixed(1) : n) + ' ' + u[i]; };
const fmtRate = bps => { let n = (Number(bps) || 0) * 8; const u = ['bit/s', 'kbit/s', 'Mbit/s', 'Gbit/s']; let i = 0; while (n >= 1000 && i < u.length - 1) { n /= 1000; i++; } return n.toFixed(i ? 1 : 0) + ' ' + u[i]; };
const fmtDur = s => { s = Math.floor(Number(s) || 0); const d = Math.floor(s / 86400), h = Math.floor(s % 86400 / 3600), m = Math.floor(s % 3600 / 60); return d ? `${d}d ${h}h ${m}m` : h ? `${h}h ${m}m` : m ? `${m}m ${s % 60}s` : `${s}s`; };
const pct = (a, b) => b ? Math.round(100 * a / b) : 0;
const dig = (o, ...p) => { for (const k of p) { if (o == null) return ''; o = o[k]; } return (o == null || o === '--') ? '' : o; };
const list = v => Array.isArray(v) ? v.join(', ') : (v ?? '');


// run an async action with error toast; returns result or undefined
async function act(fn, okMsg) {
  try { const r = await fn(); const m = okMsg || (r && r.message); if (m) toast(m); return r ?? true; }
  catch (e) { toast(e.message, true); }
}

// ---------------------------------------------------------------- UI helpers
const card = (title, body, head = '') => `<div class="card"><div class="card-head"><h3>${esc(title)}</h3>${head}</div>${body}</div>`;
const kv = rows => `<table class="kv">${rows.filter(r => r).map(([k, v]) => `<tr><td>${esc(k)}</td><td>${v}</td></tr>`).join('')}</table>`;
const pill = (text, cls = '') => `<span class="pill ${cls}">${esc(text)}</span>`;
const bar = p => `<div class="bar ${p > 90 ? 'bad' : p > 75 ? 'warn' : ''}"><i style="width:${Math.min(100, p)}%"></i></div>`;
const signalBars = q => { q = Number(q) || 0; let s = '<span class="signal">'; for (let i = 1; i <= 5; i++) s += `<i class="${q >= i * 20 - 10 ? 'on' : ''}" style="height:${i * 20}%"></i>`; return s + '</span>'; };

function field(f, val) {
  const id = 'f_' + f.key, help = f.help ? `<div class="help">${esc(f.help)}</div>` : '';
  const attrs = `name="${esc(f.key)}" id="${id}" ${f.required ? 'required' : ''} ${f.placeholder ? `placeholder="${esc(f.placeholder)}"` : ''}`;
  switch (f.type) {
    case 'check':
      return `<label class="check"><input type="checkbox" ${attrs} ${val ? 'checked' : ''}> ${esc(f.label)}</label>${help}`;
    case 'select':
      return `<label>${esc(f.label)}<select ${attrs}>${f.options.map(([v, l]) => `<option value="${esc(v)}" ${String(v) === String(val ?? '') ? 'selected' : ''}>${esc(l)}</option>`).join('')}</select>${help}</label>`;
    case 'textarea':
      return `<label>${esc(f.label)}<textarea ${attrs}>${esc(Array.isArray(val) ? val.join('\n') : val)}</textarea>${help}</label>`;
    case 'password':
      return `<label>${esc(f.label)}<input type="password" ${attrs} value="${esc(val)}" autocomplete="new-password">${help}</label>`;
    case 'number':
      return `<label>${esc(f.label)}<input type="number" ${attrs} value="${esc(val)}" ${f.min != null ? `min="${f.min}"` : ''} ${f.max != null ? `max="${f.max}"` : ''}>${help}</label>`;
    default:
      return `<label>${esc(f.label)}<input ${attrs} value="${esc(val)}" ${f.pattern ? `pattern="${esc(f.pattern)}"` : ''}>${help}</label>`;
  }
}

function readFields(root, fields) {
  const o = {};
  for (const f of fields) {
    const el = root.querySelector(`[name="${f.key}"]`);
    if (!el) continue;
    if (f.type === 'check') o[f.key] = el.checked;
    else if (f.type === 'number') o[f.key] = Number(el.value);
    else if (f.type === 'textarea' && f.list) o[f.key] = el.value.split(/[\n,]+/).map(s => s.trim()).filter(Boolean);
    else o[f.key] = el.value.trim();
  }
  return o;
}

const form = (id, fields, vals, btn = 'Save', extra = '') =>
  `<form id="${id}">${fields.map(f => field(f, vals[f.key])).join('')}<div class="btns"><button class="btn primary">${esc(btn)}</button>${extra}</div></form>`;

function onSubmit(id, fields, fn) {
  const f = $('#' + id);
  if (!f) return;
  f.addEventListener('submit', async e => {
    e.preventDefault();
    const b = f.querySelector('button.primary');
    b.disabled = true;
    try { await fn(readFields(f, fields), f); } finally { b.disabled = false; }
  });
}

// modal dialog returning the field values (or null when cancelled)
function modal(title, fields, vals = {}, okLabel = 'Save', note = '') {
  return new Promise(resolve => {
    const d = $('#modal');
    $('#modal-title').textContent = title;
    $('#modal-body').innerHTML = (note ? `<div class="notice">${note}</div>` : '') + fields.map(f => field(f, vals[f.key])).join('');
    $('#modal-ok').textContent = okLabel;
    $('#modal-ok').className = 'btn ' + (okLabel.match(/delete|reboot|disconnect|block|revoke/i) ? 'danger' : 'primary');
    const done = () => { d.removeEventListener('close', done); resolve(d.returnValue === 'ok' ? readFields($('#modal-form'), fields) : null); };
    d.addEventListener('close', done);
    d.returnValue = '';
    d.showModal();
  });
}
const confirmBox = (title, html, okLabel = 'Confirm') => modal(title, [], {}, okLabel, html).then(r => r !== null);

/* Generic list editor (rules, port forwards, leases …). Changes stay local
   until the page-level "Apply" button sends the whole list. */
function listEditor(el, opts) {
  const { columns, fields, newRow, onChange, title, empty } = opts;
  let rows = opts.rows || [];
  const render = () => {
    el.innerHTML = `<div class="card-head"><h3>${esc(title)}</h3><button class="btn sm" data-le="add">+ Add</button></div>
      <div class="table-wrap"><table><thead><tr>${columns.map(c => `<th>${esc(c[0])}</th>`).join('')}<th></th></tr></thead><tbody>
      ${rows.length ? rows.map((r, i) => `<tr class="${r.enabled === false ? 'disabled' : ''}">${columns.map(c => `<td>${c[1](r)}</td>`).join('')}
        <td class="actions">${opts.reorder ? `<button class="btn sm ghost" data-le="up" data-i="${i}" title="Move up">↑</button><button class="btn sm ghost" data-le="down" data-i="${i}" title="Move down">↓</button>` : ''}
        <button class="btn sm" data-le="edit" data-i="${i}">Edit</button> <button class="btn sm danger" data-le="del" data-i="${i}">Delete</button></td></tr>`).join('')
      : `<tr><td colspan="${columns.length + 1}" class="muted">${esc(empty || 'No entries.')}</td></tr>`}
      </tbody></table></div>`;
  };
  el.addEventListener('click', async e => {
    const b = e.target.closest('[data-le]');
    if (!b) return;
    const i = Number(b.dataset.i), a = b.dataset.le;
    if (a === 'add') { const v = await modal('Add — ' + title, fields, newRow()); if (v) rows.push({ ...newRow(), ...v }); }
    if (a === 'edit') { const v = await modal('Edit — ' + title, fields, rows[i]); if (v) rows[i] = { ...rows[i], ...v }; }
    if (a === 'del') rows.splice(i, 1);
    if (a === 'up' && i > 0) [rows[i - 1], rows[i]] = [rows[i], rows[i - 1]];
    if (a === 'down' && i < rows.length - 1) [rows[i + 1], rows[i]] = [rows[i], rows[i + 1]];
    if (['add', 'edit', 'del', 'up', 'down'].includes(a)) { render(); onChange(rows); }
  });
  render();
  return { set(r) { rows = r || []; render(); } };
}

function dirtyBar(el, label, onApply, onRevert) {
  el.innerHTML = `<div class="dirty-bar"><span>${esc(label)}</span><button class="btn sm ghost" data-d="revert">Discard</button><button class="btn sm primary" data-d="apply">Apply changes</button></div>`;
  el.querySelector('[data-d="apply"]').onclick = onApply;
  el.querySelector('[data-d="revert"]').onclick = onRevert;
}

// traffic chart on a canvas
function drawChart(canvas, pts) {
  const dpr = window.devicePixelRatio || 1, w = canvas.clientWidth, h = canvas.clientHeight;
  canvas.width = w * dpr; canvas.height = h * dpr;
  const c = canvas.getContext('2d'); c.scale(dpr, dpr); c.clearRect(0, 0, w, h);
  const max = Math.max(1024, ...pts.map(p => Math.max(p.rx, p.tx))) * 1.15;
  c.strokeStyle = '#dcdad5'; c.fillStyle = '#555'; c.font = '11px Tahoma, Verdana, sans-serif'; c.lineWidth = 1;
  for (let i = 0; i <= 3; i++) { const y = h - 16 - (h - 26) * i / 3; c.beginPath(); c.moveTo(0, y); c.lineTo(w, y); c.stroke(); c.fillText(fmtRate(max * i / 3), 4, y - 3); }
  const line = (key, color) => {
    if (pts.length < 2) return;
    c.strokeStyle = color; c.lineWidth = 2; c.beginPath();
    pts.forEach((p, i) => { const x = w * i / (pts.length - 1), y = h - 16 - (h - 26) * p[key] / max; i ? c.lineTo(x, y) : c.moveTo(x, y); });
    c.stroke();
  };
  line('rx', '#3fa7ff'); line('tx', '#37c978');
}

// ---------------------------------------------------------------- pages
const PAGES = [
  ['Status'],
  ['dashboard', 'Dashboard', pgDashboard],
  ['interfaces', 'Interfaces', pgInterfaces],
  ['data', 'Data usage', pgData],
  ['history', 'History', pgHistory],
  ['clients', 'Clients & DHCP', pgClients],
  ['Network'],
  ['mode', 'Operating mode', pgMode],
  ['wan', 'WAN / LTE', pgWAN],
  ['lan', 'Wi-Fi & LAN', pgLAN],
  ['dns', 'DNS', pgDNS],
  ['signal', 'Signal & bands', pgSignal],
  ['schedule', 'Schedules', pgSchedule],
  ['Security'],
  ['firewall', 'Firewall', pgFirewall],
  ['vpn', 'VPN (WireGuard)', pgVPN],
  ['mesh', 'Cloudflare Mesh', pgMesh],
  ['Tools'],
  ['mobile', 'SMS & USSD', pgMobile],
  ['smsgw', 'SMS gateway', pgSMSGW],
  ['remote', 'SMS remote control', pgRemote],
  ['bluetooth', 'Bluetooth', pgBluetooth],
  ['whatsapp', 'WhatsApp forward', pgWhatsApp],
  ['forward', 'SMS forwarding', pgForward],
  ['diag', 'Diagnostics', pgDiag],
  ['logs', 'Logs', pgLogs],
  ['system', 'System', pgSystem],
];

// Optional modules (GET /api/modules); pages of modules that are not installed are hidden.
let MODS = {};
// Operating mode (GET /api/mode): in Wi-Fi client mode there is no LAN, so its pages are hidden.
let MODE = 'router';
const LAN_PAGES = ['clients', 'lan', 'dns', 'firewall', 'vpn'];
// Read-only viewer (served by lofi-viewer): only these pages exist; set by the shell from /api/session
let VIEWER = false;
const VIEWER_PAGES = ['dashboard'];
const pageVisible = p => (p[0] !== 'whatsapp' || MODS.whatsapp) && !(MODE === 'client' && LAN_PAGES.includes(p[0])) && (!VIEWER || VIEWER_PAGES.includes(p[0]));


// modem + mobile network details (/api/status .modem, /api/wan .modem_summary); shared by the dashboard and the LTE page
const modemInfo = m => m && m.present ? kv([
        ['Operator', `${esc(m.operator || '—')} <span class="muted">${esc(m.operator_code)}</span>`],
        ['Registration', `${esc(m.registration || '—')}${m.packet_service ? ` · data ${esc(m.packet_service)}` : ''}`],
        ['Signal', `${signalBars(m.signal)}${esc(m.signal)}%` + (m.rsrp ? `<div class="small muted">RSRP ${esc(m.rsrp)} dBm · RSRQ ${esc(m.rsrq)} dB · SNR ${esc(m.snr)} dB · RSSI ${esc(m.rssi)} dBm</div>` : '')],
        ['Technology', `${esc(m.tech).toUpperCase()} <span class="small muted">${esc(m.modes)}</span>`],
        m.bands ? ['Bands', `<span class="small">${esc(m.bands.replace(/eutran-/g, 'B').replace(/utran-/g, '3G-B'))}</span>`] : null,
        m.ipv4 || m.ipv6 ? ['Data session', `<span class="mono small">${esc(m.apn || '')} ${esc(m.ipv4 || '')}${m.ipv6 ? '<br>' + esc(m.ipv6) : ''}</span>${m.session_s ? `<div class="small muted">connected ${fmtDur(Number(m.session_s))}</div>` : ''}`] : null,
        ['Modem state', `${esc(m.state)}${m.power_state && m.power_state !== 'on' ? ` (power ${esc(m.power_state)})` : ''}`],
        ['Own number', esc(m.own_number || '—')],
        ['IMEI', `<span class="mono">${esc(m.imei || '—')}</span>`],
        ['IMSI', `<span class="mono">${esc(m.imsi || '—')}</span>`],
        ['SIM ICCID', `<span class="mono">${esc(m.iccid || '—')}</span>${m.sim_operator ? ` <span class="small muted">${esc(m.sim_operator)}</span>` : ''}`],
        m.pin_retries ? ['SIM PIN tries left', `<span class="small">${esc(m.pin_retries)}</span>`] : null,
        ['Firmware', `<span class="small">${esc(m.firmware || '—')}${m.hw_revision ? ' · hw ' + esc(m.hw_revision) : ''}</span>`],
      ]) : '<p class="muted">No modem detected.</p>';

// ---- Dashboard
let chartPick = 'wwan0', chartIface = 'wwan0'; // LTE by default; the dropdown changes it
async function pgDashboard(page) {
  // the status cards are redrawn every 4 s; the speed test card lives outside them so it keeps its state
  page.innerHTML = '<div id="dash"></div><div id="speedtest"></div>';
  const v = $('#dash');
  const draw = async () => {
    const [s, tr] = await Promise.all([api('GET', '/status'), api('GET', '/traffic')]);
    const sy = s.system, w = s.wan || {}, m = s.modem || {}, vpn = s.vpn || {};
    const memUsed = sy.mem_total - sy.mem_available, diskUsed = sy.disk_total - sy.disk_free;
    const temps = sy.temps || [], temp = temps.reduce((a, t) => Math.max(a, t.c), 0);
    // colour: red at/over the zone's throttle trip point, amber within 5 °C of it (or plain thresholds when no trip is known)
    const tcls = t => t.trip ? (t.c >= t.trip ? 'bad' : t.c >= t.trip - 5 ? 'warn' : '') : (t.c > 75 ? 'bad' : t.c > 60 ? 'warn' : '');
    const hot = temps.find(t => t.c === temp) || {};
    const f = sy.cpufreq;
    const freqRow = f ? ['CPU clock', `<span class="stat">${f.cur_mhz}</span> MHz${bar(pct(f.cur_mhz, f.hw_max_mhz))}<span class="small muted">limit ${f.max_mhz} MHz`
      + (f.thermal_mhz ? ` · ${pill('thermal throttle ' + f.thermal_mhz + ' MHz', 'warn')}` : '')
      + (f.cap_mhz ? ` · cap ${f.cap_mhz}` : '') + ` · max ${f.hw_max_mhz} · ${esc(f.governor)}</span>`] : null;
    const up = k => (w[k] || '').startsWith('up');
    const client = s.mode === 'client', wl = s.wifi_uplink || {};
    const ifaces = Object.keys(tr).filter(n => /^(wwan0|usb0|wlan0|wg)/.test(n)).sort();
    chartIface = ifaces.includes(chartPick) ? chartPick : ifaces[0]; // LTE rates appear a few seconds after the page opens
    v.innerHTML = `<div class="grid">
      ${card('System', kv([
        ['Host', esc(sy.hostname)], ['Model', esc(sy.model)], ['Uptime', fmtDur(sy.uptime)],
        battRow(s.battery),
        ['CPU', `${sy.cpu.toFixed(0)}% · load ${esc((sy.load || []).slice(0, 3).join(' '))}${bar(sy.cpu)}`],
        ['Memory', `${fmtBytes(memUsed)} / ${fmtBytes(sy.mem_total)}${bar(pct(memUsed, sy.mem_total))}`],
        ['Storage', `${fmtBytes(diskUsed)} / ${fmtBytes(sy.disk_total)}${bar(pct(diskUsed, sy.disk_total))}`],
        freqRow,
        temps.length ? ['Temperature', `<span class="stat ${tcls(hot)}">${temp.toFixed(1)} °C</span> <span class="small muted">${esc(hot.label || hot.name || '')}${hot.trip ? ` · throttles at ${hot.trip} °C` : ''}</span>`
          + `<div class="small">${temps.map(t => `<span class="${tcls(t)}" title="${t.trip ? 'throttles at ' + t.trip + ' °C' : ''}">${esc(t.label || t.name)} ${t.c.toFixed(0)}°</span>`).join(' · ')}</div>`] : null,
        sy.thermal_guard ? ['Thermal guard', tgStatus(sy.thermal_guard)] : null,
      ]))}
      ${card('Internet', kv([
        ['Active uplink', w.active && w.active !== 'none' ? pill({ wwan0: 'LTE (wwan0)', usb0: 'USB host (usb0)', bnep0: 'Phone over Bluetooth (backup)', wlan0: 'Wi-Fi (wlan0)' }[w.active] || w.active, w.active === 'bnep0' || (client && w.active === 'wwan0') ? 'warn' : 'ok') : pill('none', 'bad')],
        client ? ['Wi-Fi uplink', up('wlan0') ? pill('online', 'ok') : pill(wl.connected ? 'no internet' : 'not connected', 'bad')] : null,
        ['LTE uplink', up('wwan0') ? pill('online', 'ok') : pill('no internet', client ? '' : 'bad')],
        ['USB uplink', up('usb0') ? pill('online', 'ok') : pill('no internet', w.usb0 && !client ? 'bad' : '')],
        client || !s.vpn ? null : ['VPN for LAN', vpn.route_lan ? `${esc(vpn.route_lan)} ${(vpn.active || []).includes(vpn.route_lan) ? pill('up', 'ok') : pill('down', vpn.kill_switch ? 'bad' : 'warn')}` : '<span class="muted">off</span>'],
        client || !s.ipv6 ? null : ['IPv6 for LAN', !s.ipv6 || !s.ipv6.active ? '<span class="muted">off</span>' : s.ipv6.internet ? `${pill('via ' + s.ipv6.device, 'ok')}<div class="mono small">${esc(s.ipv6.egress_address)}</div>` : pill('no IPv6 internet', 'warn')],
        s.bt_uplink && s.bt_uplink.configured ? ['Bluetooth backup', s.bt_uplink.connected ? pill('phone connected' + (s.bt_uplink.active ? ', in use' : ''), 'warn') : pill('standby', '')] : null,
        s.data ? ['LTE data', dataLine(s.data)] : null,
        ['Last check', esc((w.updated || '').replace('T', ' ').slice(0, 19))],
      ]))}
      ${card('Mobile network', modemInfo(m))}
      ${client ? card('Wi-Fi uplink', kv([
        ['Network', wl.connected ? `${esc(wl.ssid)} ${pill('connected', 'ok')}` : pill('not connected', 'bad')],
        wl.connected ? ['Signal', wifiSignal(wl.signal_dbm) + `<span class="small muted"> · ${wl.freq_mhz > 3000 ? '5' : '2.4'} GHz (${esc(wl.freq_mhz)} MHz)${wl.tx_bitrate ? ' · ' + esc(wl.tx_bitrate) : ''}</span>`] : null,
        ['Address', `<span class="mono">${esc(wl.ipv4 || '—')}</span>${wl.gateway ? ` <span class="small muted">gateway ${esc(wl.gateway)}</span>` : ''}`],
        ['Connections', `${sy.conntrack} / ${sy.conntrack_max}${bar(pct(sy.conntrack, sy.conntrack_max))}`],
        s.firewall ? ['Dropped (in)', `${esc(s.firewall.dropped.input || 0)} packets`] : null,
      ])) : card('LAN & firewall', kv([
        ['Wi-Fi', `${esc((s.wifi || {}).ssid)} ${(s.wifi || {}).state === 'activated' ? pill('on', 'ok') : pill((s.wifi || {}).state || 'off', 'bad')}`],
        s.clients ? ['Wi-Fi clients', `<span class="stat">${s.clients.wifi}</span>${s.clients.known != null ? ` <span class="muted">(${s.clients.known} known)</span>` : ''}`] : null,
        ['Connections', `${sy.conntrack} / ${sy.conntrack_max}${bar(pct(sy.conntrack, sy.conntrack_max))}`],
        s.firewall ? ['Dropped (in / fwd)', `${esc(s.firewall.dropped.input || 0)} / ${esc(s.firewall.dropped.forward || 0)} packets`] : null,
        s.firewall ? ['Custom rules / forwards', `${s.firewall.rules} / ${s.firewall.forwards}`] : null,
      ]))}
      </div>
      ${card('Traffic', `<canvas class="chart" id="chart"></canvas><div class="legend"><span><i style="background:#3fa7ff"></i>download (rx)</span><span><i style="background:#37c978"></i>upload (tx)</span><span id="chart-now"></span></div>`,
        `<select id="chart-if">${ifaces.map(n => `<option ${n === chartIface ? 'selected' : ''}>${esc(n)}</option>`).join('')}</select>`)}`;
    const pts = tr[chartIface] || [];
    drawChart($('#chart'), pts);
    const last = pts[pts.length - 1];
    if (last) $('#chart-now').textContent = `now: ↓ ${fmtRate(last.rx)}  ↑ ${fmtRate(last.tx)}`;
    $('#chart-if').onchange = e => { chartPick = chartIface = e.target.value; draw(); };
    updatePills(s);
  };
  await draw();
  every(4000, draw);
  if (await drawSpeedtest()) setTimeout(pollSpeedtest, 2000); // a test started earlier is still running
}

// ---- Speed test (dashboard): runs on the stick over the active uplink, short to limit modem heat
async function drawSpeedtest() {
  const el = $('#speedtest');
  if (!el) return false;
  const st = await api('GET', '/speedtest'), r = st.last;
  const phase = { starting: 'starting…', latency: 'measuring latency…', download: 'testing download…', upload: 'testing upload…' }[st.phase] || 'running…';
  const big = (label, val, unit) => `<div class="speed-num"><div class="small muted">${label}</div><span class="stat">${val}</span> <span class="muted">${unit}</span></div>`;
  const body = (r ? `<div class="speed-row">
      ${big('Download', r.down_mbps.toFixed(1), 'Mbit/s')}
      ${big('Upload', r.up_mbps ? r.up_mbps.toFixed(1) : '—', r.up_mbps ? 'Mbit/s' : '')}
      ${big('Latency', r.ping_ms ? r.ping_ms.toFixed(0) : '—', r.ping_ms ? 'ms' : '')}
    </div>
    <p class="small muted">${esc(new Date(r.time).toLocaleString())} via ${esc(r.iface === 'wwan0' ? 'LTE (wwan0)' : r.iface)}
      · ${r.down_mb.toFixed(1)} MB down${r.down_s ? ` in ${r.down_s.toFixed(0)} s` : ''}${r.up_mb ? `, ${r.up_mb.toFixed(1)} MB up${r.up_s ? ` in ${r.up_s.toFixed(0)} s` : ''}` : ''}${r.ping_ms ? ` · ping ${r.ping_min_ms.toFixed(0)}–${r.ping_max_ms.toFixed(0)} ms` : ''}
      · ${r.temp_start_c.toFixed(0)} → ${r.temp_end_c.toFixed(0)} °C${r.note ? ` · ${esc(r.note)}` : ''}</p>`
    : '<p class="muted">No speed test has been run yet.</p>')
    + (st.running ? `<p>${pill(phase, 'warn')}</p>` : '')
    + (st.error && !st.running ? `<p class="bad small">Last attempt failed: ${esc(st.error)}</p>` : '')
    + `<p class="help">Runs on the router over the active uplink: 15 s download, then 10 s upload; speeds are measured after a 2 s warm-up.
      Data used depends on the speed (about 13 MB at 5 Mbit/s down, 3 Mbit/s up).
      Uploading heats the LTE modem quickly, so it won't start at the thermal guard's limit, skips the upload if the download left the stick that hot, and stops early at 90 °C.</p>`;
  el.innerHTML = card('Speed test', body, `<button class="btn sm primary" id="speed-run" ${st.running ? 'disabled' : ''}>${st.running ? 'Testing…' : 'Run test'}</button>`);
  $('#speed-run').onclick = async () => {
    if (await act(() => api('POST', '/speedtest', { upload: true }))) pollSpeedtest();
  };
  return st.running;
}

// poll every 2 s while a test runs (it stops by itself after ~30 s, or when the page changes)
function pollSpeedtest() {
  const el = $('#speedtest');
  drawSpeedtest().then(running => { if (running && el && el.isConnected) setTimeout(pollSpeedtest, 2000); }).catch(() => {});
}

// ---- Interfaces
async function pgInterfaces(v) {
  const draw = async () => {
    const ifs = await api('GET', '/interfaces');
    v.innerHTML = card('Interfaces', `<div class="table-wrap"><table><thead><tr><th>Name</th><th>Role</th><th>State</th><th>Addresses</th><th>MAC</th><th>MTU</th><th>RX / TX total</th><th>Now ↓ / ↑</th></tr></thead><tbody>
      ${ifs.map(i => `<tr><td class="mono">${esc(i.name)}</td><td>${esc(i.role)}</td>
        <td>${i.state === 'UP' || (i.up && i.state === 'UNKNOWN') ? pill(i.state, 'ok') : pill(i.state, i.up ? 'warn' : '')}</td>
        <td class="mono">${(i.addresses || []).map(esc).join('<br>')}</td><td class="mono">${esc(i.mac)}</td><td>${i.mtu}</td>
        <td>${fmtBytes(i.rx_bytes)} / ${fmtBytes(i.tx_bytes)}</td><td>${fmtRate(i.rx_rate)} / ${fmtRate(i.tx_rate)}</td></tr>`).join('')}
      </tbody></table></div>`);
  };
  await draw();
  every(4000, draw);
}

// ---- Clients & DHCP
async function pgClients(v) {
  v.innerHTML = `<div id="cl-table"></div><div id="cl-dirty"></div><div class="card" id="cl-static"></div>`;
  const drawClients = async () => {
    const cs = (await api('GET', '/clients')) || [];
    $('#cl-table').innerHTML = card('Connected & known clients', `<div class="table-wrap"><table><thead><tr><th>Name</th><th>IP</th><th>MAC</th><th>Wi-Fi</th><th>Signal</th><th>Connected</th><th>Up / Down</th><th>Type</th><th></th></tr></thead><tbody>
      ${cs.length ? cs.map(c => `<tr><td>${esc(c.name || '—')}</td><td class="mono">${esc(c.ip)}${(c.ipv6 || []).map(a => `<br><span class="small muted">${esc(a)}</span>`).join('')}</td><td class="mono">${esc(c.mac)}</td>
        <td>${c.wifi ? pill('online', 'ok') : '<span class="muted">offline</span>'}</td><td>${esc(c.signal)}</td>
        <td>${c.wifi ? fmtDur(c.connected_s) : ''}</td><td>${fmtBytes(c.upload)} / ${fmtBytes(c.download)}</td>
        <td>${c.blocked ? pill('blocked', 'bad') : c.static ? pill('static') : 'dynamic'}</td>
        <td class="actions">${!c.static && c.ip ? `<button class="btn sm" data-c="reserve" data-m="${esc(c.mac)}">Reserve IP</button> ` : ''}
          ${c.blocked ? `<button class="btn sm" data-c="unblock" data-m="${esc(c.mac)}">Unblock</button>` : `<button class="btn sm danger" data-c="block" data-m="${esc(c.mac)}">Block</button>`}
          ${c.wifi ? ` <button class="btn sm" data-c="disconnect" data-m="${esc(c.mac)}">Kick</button>` : ''}</td></tr>`).join('')
      : '<tr><td colspan="9" class="muted">No clients yet.</td></tr>'}</tbody></table></div>
      <p class="help">Up/Down counts traffic since the firewall was last reloaded. "Block" cuts a device off from the LAN and the internet; "Kick" only disconnects it from Wi-Fi once.</p>`);
  };
  $('#cl-table').addEventListener('click', async e => {
    const b = e.target.closest('[data-c]');
    if (!b) return;
    const a = b.dataset.c, mac = b.dataset.m;
    if (a === 'block' && !await confirmBox('Block client', `Block <b class="mono">${esc(mac)}</b> from the network and internet?`, 'Block')) return;
    if (await act(() => api('POST', `/clients/${encodeURIComponent(mac)}/${a}`), a === 'disconnect' ? 'Client disconnected' : null)) { await drawClients(); if (a === 'reserve') loadStatic(); }
  });
  let lan;
  const leaseFields = [
    { key: 'mac', label: 'MAC address', required: true, placeholder: 'aa:bb:cc:dd:ee:ff' },
    { key: 'ip', label: 'IP address', required: true },
    { key: 'name', label: 'Host name (optional)', help: 'Resolvable as <name>.lan' },
  ];
  const loadStatic = async () => {
    lan = (await api('GET', '/lan')).lan;
    let rows = lan.static_leases || [];
    listEditor($('#cl-static'), {
      title: 'Static DHCP leases', rows, fields: leaseFields, empty: 'No reservations.',
      newRow: () => ({ mac: '', ip: '', name: '' }),
      columns: [['MAC', r => `<span class="mono">${esc(r.mac)}</span>`], ['IP', r => `<span class="mono">${esc(r.ip)}</span>`], ['Name', r => esc(r.name)]],
      onChange: r => { rows = r; dirtyBar($('#cl-dirty'), 'Static leases changed.', async () => { if (await act(() => api('PUT', '/dhcp/static', rows))) { $('#cl-dirty').innerHTML = ''; loadStatic(); } }, () => { $('#cl-dirty').innerHTML = ''; loadStatic(); }); },
    });
  };
  await Promise.all([drawClients(), loadStatic()]);
  every(8000, drawClients);
}

// ---- WAN / LTE
async function pgWAN(v) {
  const d = await api('GET', '/wan');
  const md = d.modem || {};
  const st = d.state || {};
  const bearers = (md.bearers || []).map(b => b.bearer || {}).filter(b => dig(b, 'status', 'connected') === 'yes');
  const uplinkRow = n => { const s = st[n] || ''; const [state, ...rest] = s.split(' '); return `<tr><td class="mono">${esc(n)}</td><td>${n === 'wwan0' ? 'LTE modem' : 'USB host'}</td><td>${state === 'up' ? pill('online', 'ok') : pill(state || 'unknown', 'bad')}</td><td class="mono">${esc(rest.join(' '))}</td><td>${st.active === n ? pill('ACTIVE', 'ok') : ''}</td></tr>`; };
  const fo = d.failover;
  const foFields = [
    { key: 'primary', label: 'Preferred uplink', type: 'select', options: [['wwan0', 'LTE first, USB host as backup'], ['usb0', 'USB host first, LTE as backup'], ['wwan0only', 'LTE only'], ['usb0only', 'USB host only']] },
    { key: 'targets', label: 'Health-check targets', type: 'textarea', list: true, help: 'IPv4 addresses pinged through each uplink, one per line' },
    { key: 'interval', label: 'Check interval (s)', type: 'number', min: 5, max: 300 },
    { key: 'fail_after', label: 'Failed checks before switching', type: 'number', min: 1, max: 20 },
  ];
  const primary = fo.order.length === 1 ? fo.order[0] + 'only' : fo.order[0];
  const lteFields = [
    { key: 'apn', label: 'APN', help: 'Access point name from your carrier (e.g. airtelgprs.com, jionet)' },
    { key: 'username', label: 'Username (usually empty)' },
    { key: 'password', label: 'Password', type: 'password', placeholder: d.lte.has_password ? '(unchanged)' : '(none)' },
    { key: 'roaming', label: 'Allow roaming', type: 'check' },
    { key: 'autoconnect', label: 'Connect automatically', type: 'check' },
  ];
  const bearer = bearers[0] || {};
  v.innerHTML = `<div class="grid two">
    ${card('Uplinks & failover', `<table><thead><tr><th>Uplink</th><th></th><th>Internet</th><th>Details</th><th></th></tr></thead><tbody>${fo.order.concat(['wwan0', 'usb0'].filter(x => !fo.order.includes(x))).map(uplinkRow).join('')}</tbody></table>
      <p class="help">Each uplink is pinged every ${fo.interval}s; traffic uses the first healthy uplink in the preferred order.
      If the modem's data driver gets stuck, LTE stalls (connected but nothing gets through for 2 minutes), or LTE stays connected without IPv4, the router repairs it automatically.</p>
      ${st.lte_recovery ? `<div class="notice">Last automatic LTE repair: ${esc(st.lte_recovery.replace(/^bam/, 'stuck modem driver').replace(/^nov4/, 'no IPv4 address').replace(/^stall/, 'data stall (connected but no traffic)').replace(/^noreg/, 'modem not registering'))}</div>` : ''}
      ${form('fo-form', foFields, { primary, targets: fo.targets, interval: fo.interval, fail_after: fo.fail_after }, 'Save failover settings')}`)}
    ${card('LTE connection', `${form('lte-form', lteFields, { ...d.lte, password: '' }, 'Save LTE settings',
      `<button type="button" class="btn" id="lte-up">Connect</button><button type="button" class="btn" id="lte-down">Disconnect</button>`)}
      <p class="help">NetworkManager profile state: ${esc(d.lte.state || 'inactive')}</p>`)}
    ${card('Modem & mobile network', modemInfo(d.modem_summary))}
    ${card('Mobile data bearer', bearers.length ? kv([
      ['Interface', `<span class="mono">${esc(dig(bearer, 'status', 'interface'))}</span>`],
      ['APN / IP type', `${esc(dig(bearer, 'properties', 'apn'))} / ${esc(dig(bearer, 'properties', 'ip-type'))}`],
      ['IPv4', `<span class="mono">${esc(dig(bearer, 'ipv4-config', 'address'))}/${esc(dig(bearer, 'ipv4-config', 'prefix'))} via ${esc(dig(bearer, 'ipv4-config', 'gateway'))}</span>`],
      ['IPv6', `<span class="mono small">${esc(dig(bearer, 'ipv6-config', 'address'))}</span>`],
      ['DNS', `<span class="mono small">${esc(list(dig(bearer, 'ipv4-config', 'dns')))}</span>`],
      ['MTU', esc(dig(bearer, 'ipv4-config', 'mtu'))],
      ['Session', `${fmtDur(dig(bearer, 'stats', 'duration'))} · ↓ ${fmtBytes(dig(bearer, 'stats', 'bytes-rx'))} ↑ ${fmtBytes(dig(bearer, 'stats', 'bytes-tx'))}`],
    ]) + (Number(dig(bearer, 'stats', 'bytes-rx')) < 2048 && Number(dig(bearer, 'stats', 'duration')) > 60 ? '<p class="warn small">The bearer is connected but has received almost nothing — check the APN and the SIM\'s data balance (SMS & USSD page).</p>' : '') : '<p class="muted">No connected data bearer.</p>')}
  </div>`;
  onSubmit('fo-form', foFields, async x => {
    const order = { wwan0: ['wwan0', 'usb0'], usb0: ['usb0', 'wwan0'], wwan0only: ['wwan0'], usb0only: ['usb0'] }[x.primary];
    await act(() => api('PUT', '/wan/failover', { order, targets: x.targets, interval: x.interval, fail_after: x.fail_after }));
  });
  onSubmit('lte-form', lteFields, async x => {
    const body = { ...x }; if (!x.password) body.password = null;
    await act(() => api('PUT', '/wan/lte', body));
  });
  $('#lte-up').onclick = () => act(() => api('POST', '/wan/lte/connect'), 'LTE connected').then(() => route());
  $('#lte-down').onclick = async () => { if (await confirmBox('Disconnect LTE', 'Disconnect the mobile data connection?', 'Disconnect')) act(() => api('POST', '/wan/lte/disconnect'), 'LTE disconnected').then(() => route()); };
}

// ---- Wi-Fi & LAN
async function pgLAN(v) {
  const [d, v6] = await Promise.all([api('GET', '/lan'), api('GET', '/ipv6')]);
  const wifiFields = [
    { key: 'ssid', label: 'Network name (SSID)', required: true },
    { key: 'psk', label: 'Password (WPA2-PSK)', type: 'password', required: true, help: '8-63 characters. Use 16+ random characters.' },
    { key: 'channel', label: 'Channel (2.4 GHz)', type: 'select', options: [[0, 'Automatic'], ...Array.from({ length: 13 }, (_, i) => [i + 1, `Channel ${i + 1}`])] },
    { key: 'hidden', label: 'Hide network name', type: 'check' },
  ];
  const lanFields = [
    { key: 'address', label: 'Router LAN address / prefix', required: true, help: 'e.g. 192.168.4.1/24' },
    { key: 'dhcp_enabled', label: 'Enable DHCP server', type: 'check' },
    { key: 'dhcp_start', label: 'DHCP range start' },
    { key: 'dhcp_end', label: 'DHCP range end' },
    { key: 'lease_time', label: 'Lease time', help: '30m, 12h, 7d or infinite' },
  ];
  const w = d.wifi;
  v.innerHTML = `<div class="grid two">
    ${card('Wi-Fi access point', `<div class="notice">Saving restarts Wi-Fi: connected devices drop for a few seconds. Security: ${esc(w.security)} · band ${esc(w.band === 'bg' ? '2.4 GHz' : w.band)} · state ${esc(w.state)}</div>
      ${form('wifi-form', wifiFields, w, 'Save Wi-Fi settings', '<button type="button" class="btn ghost" id="psk-show">Show password</button>')}`)}
    ${card('LAN & DHCP server', `<div class="notice">Changing the LAN address restarts Wi-Fi; reconnect and open the new address afterwards.</div>${form('lan-form', lanFields, d.lan, 'Save LAN settings')}`)}
    ${card('IPv6 for LAN clients', ipv6Card(v6))}
  </div>`;
  $('#psk-show').onclick = () => { const i = $('#wifi-form [name=psk]'); i.type = i.type === 'password' ? 'text' : 'password'; };
  onSubmit('wifi-form', wifiFields, async x => {
    if (!await confirmBox('Restart Wi-Fi', 'Apply the new Wi-Fi settings? All Wi-Fi clients will be disconnected briefly.', 'Apply')) return;
    await act(() => api('PUT', '/wifi', { ...x, channel: Number(x.channel) }));
  });
  onSubmit('lan-form', lanFields, async x => { await act(() => api('PUT', '/lan', x)); });
  onSubmit('v6-form', v6Fields(v6), async x => { if (await act(() => api('PUT', '/ipv6', x))) setTimeout(route, 2500); });
}

const v6Fields = v6 => [
  { key: 'enabled', label: 'Give Wi-Fi clients IPv6', type: 'check' },
  { key: 'tunnel', label: 'IPv6 goes out through', type: 'select', options: [['', '— choose a WireGuard tunnel —'], ...v6.tunnels.map(t => [t, t])], help: 'A tunnel with an IPv6 address, e.g. Cloudflare WARP (VPN page). Your LTE carrier provides no usable native IPv6.' },
];
function ipv6Card(v6) {
  const st = v6.status || {};
  const state = !st.active ? pill('off') : st.internet ? pill('working', 'ok') : pill('no IPv6 internet', 'warn');
  return kv([
    ['Status', `${state} <span class="muted small">${esc(st.note || '')}</span>`],
    ['LAN prefix', `<span class="mono">${esc(v6.config.ula)}</span>`],
    st.active ? ['Router address', `<span class="mono">${esc(st.router_address)}</span>`] : null,
    st.egress_address ? ['Public IPv6 (via tunnel)', `<span class="mono">${esc(st.egress_address)}</span>`] : null,
  ]) + form('v6-form', v6Fields(v6), v6.config, 'Save IPv6 settings')
    + '<p class="help">Clients get addresses from the private (ULA) prefix by SLAAC; the router NATs their IPv6 to the tunnel\'s public address. Incoming IPv6 connections cannot reach clients. Devices prefer IPv4 for sites that have both, so IPv6 is mainly used for IPv6-only services.</p>';
}

// ---- DNS
async function pgDNS(v) {
  let d = await api('GET', '/dns');
  const fields = [
    { key: 'upstreams', label: 'Upstream DNS servers', type: 'textarea', list: true, help: 'One IP per line. Empty = use the active uplink\'s DNS. Examples: 1.1.1.1, 9.9.9.9, 1.1.1.3 (Cloudflare family filter)' },
    { key: 'rebind_protection', label: 'DNS rebinding protection (block private IPs in public answers)', type: 'check' },
    { key: 'blocklist', label: 'Blocked domains', type: 'textarea', list: true, help: 'One domain per line; subdomains are blocked too. Answers 0.0.0.0 / ::' },
  ];
  v.innerHTML = `<div class="grid two"><div class="card"><h3>Resolver</h3>${form('dns-form', fields, d, 'Save & apply DNS settings')}</div><div class="card" id="dns-hosts"></div></div><div class="card" id="dns-ub"></div>`;
  drawUpdateBlock(d);
  const hostFields = [{ key: 'name', label: 'Host name', required: true, help: 'e.g. nas.lan or printer.home.arpa' }, { key: 'ip', label: 'IP address (v4 or v6)', required: true }];
  listEditor($('#dns-hosts'), {
    title: 'Local host records', rows: d.hosts || [], fields: hostFields, empty: 'No host records. router.lan always points to the router.',
    newRow: () => ({ name: '', ip: '' }), columns: [['Name', r => esc(r.name)], ['IP', r => `<span class="mono">${esc(r.ip)}</span>`]],
    onChange: r => { d.hosts = r; toast('Host records changed — press "Save & apply DNS settings"'); },
  });
  onSubmit('dns-form', fields, async x => { await act(() => api('PUT', '/dns', { ...d, ...x })); d = await api('GET', '/dns'); drawUpdateBlock(d); });
  every(5000, async () => { const n = await api('GET', '/dns'); d.update_block_status = n.update_block_status; $('#ub-status').innerHTML = ubStatus(n); });
}

// OS update blocking: DNS answers NXDOMAIN for update servers (always, or only while LTE is the uplink).
function ubStatus(d) {
  const st = d.update_block_status || {}, up = st.uplink === 'wwan0' ? 'LTE' : st.uplink === 'usb0' ? 'USB host' : 'none';
  return st.active ? `${pill('blocking now', 'ok')} <span class="small muted">${st.domains} domains · uplink ${esc(up)}</span>`
    : `${pill(st.mode === 'off' ? 'off' : 'not blocking', '')} <span class="small muted">${st.mode === 'mobile' ? `uplink ${esc(up)} (blocks only on LTE)` : ''}</span>`;
}
function drawUpdateBlock(d) {
  const u = d.update_block || { mode: 'off', groups: [] }, groups = d.update_block_groups || [];
  $('#dns-ub').innerHTML = `<div class="card-head"><h3>Block OS updates</h3><div id="ub-status">${ubStatus(d)}</div></div>
    <p class="help">Saves mobile data: devices on the Wi-Fi cannot reach update download servers, so Windows and others postpone updates until the block is lifted.
    Works for devices that use the router's DNS (the default); a device with its own DNS or DNS-over-HTTPS setting is not affected.</p>
    <form id="ub-form">
      <label>When to block<select name="mode">${[['off', 'Off'], ['mobile', 'Only while on mobile data (LTE) — recommended'], ['always', 'Always']].map(([k, l]) => `<option value="${k}" ${u.mode === k ? 'selected' : ''}>${esc(l)}</option>`).join('')}</select>
      <div class="help">"Only while on mobile data" lifts the block within seconds when the USB host (a PC sharing its connection) becomes the uplink.</div></label>
      ${groups.map(g => `<label class="check"><input type="checkbox" name="g_${esc(g.key)}" ${(u.groups || []).includes(g.key) ? 'checked' : ''}> ${esc(g.label)}</label>
        <div class="help">${esc(g.help)} <span class="mono">${g.domains.map(esc).join(', ')}</span>${g.allowed.length ? `<br>Still allowed: <span class="mono">${g.allowed.map(esc).join(', ')}</span> (certificates, virus definitions)` : ''}</div>`).join('')}
      <div class="btns"><button class="btn primary">Save update blocking</button></div></form>`;
  $('#ub-form').addEventListener('submit', async e => {
    e.preventDefault();
    const f = e.target, next = { mode: f.mode.value, groups: groups.filter(g => f['g_' + g.key].checked).map(g => g.key) };
    if (next.mode !== 'off' && !next.groups.length) return toast('Choose at least one category', true);
    if (await act(() => api('PUT', '/dns', { ...d, update_block: next }), 'Update blocking saved')) {
      const n = await api('GET', '/dns'); Object.assign(d, n); drawUpdateBlock(d);
    }
  });
}

// ---- Firewall
let fwTab = 'rules';
async function pgFirewall(v) {
  let saved = await api('GET', '/firewall');
  let fw = structuredClone(saved);
  const ifOpts = [['any', 'any'], ['lan', 'LAN (Wi-Fi)'], ['wan', 'WAN (LTE)'], ['usb', 'USB link'], ['vpn', 'VPN (wg*)']];
  const ruleFields = [
    { key: 'enabled', label: 'Enabled', type: 'check' },
    { key: 'chain', label: 'Direction', type: 'select', options: [['forward', 'Forward (traffic through the router)'], ['input', 'Input (traffic to the router itself)']] },
    { key: 'action', label: 'Action', type: 'select', options: [['accept', 'Accept'], ['drop', 'Drop (silently)'], ['reject', 'Reject (tell sender)']] },
    { key: 'in', label: 'Incoming interface', type: 'select', options: ifOpts },
    { key: 'out', label: 'Outgoing interface (forward only)', type: 'select', options: ifOpts },
    { key: 'proto', label: 'Protocol', type: 'select', options: [['any', 'any'], ['tcp', 'TCP'], ['udp', 'UDP'], ['tcp/udp', 'TCP + UDP'], ['icmp', 'ICMP / ICMPv6']] },
    { key: 'src', label: 'Source address / network', placeholder: 'any', help: 'IPv4 or IPv6, e.g. 192.168.4.20 or 192.168.4.0/25' },
    { key: 'dst', label: 'Destination address / network', placeholder: 'any' },
    { key: 'dport', label: 'Destination port(s)', placeholder: 'any', help: 'e.g. 443 or 80,443 or 6000-6100' },
    { key: 'log', label: 'Log matches (Logs → Firewall)', type: 'check' },
    { key: 'comment', label: 'Description' },
  ];
  const pfFields = [
    { key: 'enabled', label: 'Enabled', type: 'check' },
    { key: 'proto', label: 'Protocol', type: 'select', options: [['tcp', 'TCP'], ['udp', 'UDP'], ['tcp/udp', 'TCP + UDP']] },
    { key: 'ext_port', label: 'External port or range', required: true, placeholder: '8080 or 10000-10010' },
    { key: 'int_ip', label: 'Internal IP (LAN)', required: true },
    { key: 'int_port', label: 'Internal port', placeholder: 'same as external', help: 'Leave empty for ranges' },
    { key: 'source', label: 'Allowed source (optional)', placeholder: 'any', help: 'IPv4 address or network' },
    { key: 'comment', label: 'Description' },
  ];
  const blkFields = [{ key: 'mac', label: 'MAC address', required: true }, { key: 'comment', label: 'Description' }];
  const ifName = x => ({ any: '*', lan: 'LAN', wan: 'WAN', usb: 'USB', vpn: 'VPN' }[x] || x);

  const markDirty = () => {
    const dirty = JSON.stringify(fw) !== JSON.stringify(saved);
    if (dirty) dirtyBar($('#fw-dirty'), 'Firewall configuration changed — not applied yet.', async () => {
      if (await act(() => api('PUT', '/firewall', fw))) { saved = await api('GET', '/firewall'); fw = structuredClone(saved); $('#fw-dirty').innerHTML = ''; draw(); }
    }, () => { fw = structuredClone(saved); $('#fw-dirty').innerHTML = ''; draw(); });
    else $('#fw-dirty').innerHTML = '';
  };
  const tabs = [['rules', 'Rules'], ['forwards', 'Port forwarding'], ['blocked', 'Blocked clients'], ['settings', 'Settings'], ['live', 'Live ruleset']];
  const draw = async () => {
    v.innerHTML = `<div class="tabs">${tabs.map(([k, l]) => `<a data-t="${k}" class="${fwTab === k ? 'active' : ''}">${esc(l)}</a>`).join('')}</div><div id="fw-dirty"></div><div class="card" id="fw-body"></div>`;
    $$('.tabs a', v).forEach(a => a.onclick = () => { fwTab = a.dataset.t; draw(); });
    const body = $('#fw-body');
    if (fwTab === 'rules') {
      listEditor(body, {
        title: 'Firewall rules (evaluated top to bottom, before the defaults)', rows: fw.rules, fields: ruleFields, reorder: true,
        empty: 'No custom rules. Defaults: LAN → internet allowed, everything from WAN blocked, router reachable from LAN only for DNS/DHCP/web/SSH.',
        newRow: () => ({ enabled: true, chain: 'forward', action: 'drop', in: 'lan', out: 'wan', proto: 'tcp', src: '', dst: '', dport: '', log: false, comment: '' }),
        columns: [['Action', r => pill(r.action, r.action === 'accept' ? 'ok' : 'bad')], ['Dir', r => esc(r.chain)], ['In → Out', r => `${ifName(r.in)} → ${r.chain === 'input' ? 'router' : ifName(r.out)}`],
          ['Proto', r => esc(r.proto)], ['Source', r => `<span class="mono">${esc(r.src || '*')}</span>`], ['Destination', r => `<span class="mono">${esc(r.dst || '*')}${r.dport ? ':' + esc(r.dport) : ''}</span>`],
          ['Description', r => esc(r.comment) + (r.log ? ' ' + pill('log') : '')]],
        onChange: r => { fw.rules = r; markDirty(); },
      });
    } else if (fwTab === 'forwards') {
      listEditor(body, {
        title: 'Port forwarding (WAN/USB → LAN, IPv4)', rows: fw.port_forwards, fields: pfFields,
        empty: 'No port forwards. Note: most LTE carriers use CGNAT, so inbound IPv4 from the internet usually cannot reach the router.',
        newRow: () => ({ enabled: true, proto: 'tcp', ext_port: '', int_ip: '', int_port: '', source: '', comment: '' }),
        columns: [['Proto', r => esc(r.proto)], ['External', r => esc(r.ext_port)], ['→ Internal', r => `<span class="mono">${esc(r.int_ip)}:${esc(r.int_port || r.ext_port)}</span>`], ['Source', r => esc(r.source || 'any')], ['Description', r => esc(r.comment)]],
        onChange: r => { fw.port_forwards = r; markDirty(); },
      });
    } else if (fwTab === 'blocked') {
      listEditor(body, {
        title: 'Blocked clients (by MAC address)', rows: fw.blocked, fields: blkFields, empty: 'No blocked devices.',
        newRow: () => ({ mac: '', comment: '' }), columns: [['MAC', r => `<span class="mono">${esc(r.mac)}</span>`], ['Description', r => esc(r.comment)]],
        onChange: r => { fw.blocked = r; markDirty(); },
      });
    } else if (fwTab === 'settings') {
      const sf = [
        { key: 'wan_ping', label: 'Answer ping from WAN (rate-limited)', type: 'check' },
        { key: 'lan_ssh', label: 'Allow SSH to the router from Wi-Fi LAN', type: 'check', help: 'The USB management link always allows SSH and the web UI.' },
        { key: 'log_dropped', label: 'Log dropped packets (rate-limited)', type: 'check' },
        { key: 'mesh_admin', label: 'Allow management over Cloudflare Mesh', type: 'check', help: 'Ping, SSH (keys only) and this web UI from devices in your Zero Trust organization, via the router\'s Mesh IP.' },
        { key: 'mesh_lan', label: 'Make the Wi-Fi LAN reachable over Cloudflare Mesh', type: 'check', help: 'Needs a route for the LAN subnet to this node in the Cloudflare dashboard (Networking > Mesh).' },
      ];
      body.innerHTML = `<h3>General settings</h3>${sf.map(f => field(f, fw[f.key])).join('')}
        <div class="notice">Always on: stateful filtering, default-deny from WAN, NAT for the LAN, TCP MSS clamping, ICMP needed for path-MTU discovery. All outbound LAN traffic — including WireGuard, Cloudflare WARP / Zero Trust (UDP 2408, MASQUE on 443), IPsec NAT-T and OpenVPN — is allowed through.</div>`;
      sf.forEach(f => body.querySelector(`[name=${f.key}]`).onchange = e => { fw[f.key] = e.target.checked; markDirty(); });
    } else {
      const rs = await api('GET', '/firewall/ruleset');
      body.innerHTML = `<h3>Active nftables ruleset</h3><pre>${esc(rs.ruleset)}</pre>`;
    }
    markDirty();
  };
  await draw();
}

// ---- VPN
async function pgVPN(v) {
  const d = await api('GET', '/vpn');
  const conns = d.connections || [];
  const routeFields = [
    { key: 'route_lan', label: 'Route all LAN traffic through', type: 'select', options: [['', 'Nothing — LAN uses the WAN directly'], ...conns.map(c => [c.name, c.name])] },
    { key: 'kill_switch', label: 'Kill switch: block LAN internet while the tunnel is down', type: 'check' },
  ];
  const impFields = [
    { key: 'name', label: 'Name (becomes the interface)', required: true, placeholder: 'wg-warp', help: 'Starts with "wg", 3-15 chars: a-z 0-9 _ -' },
    { key: 'config', label: 'WireGuard configuration', type: 'textarea', required: true, placeholder: '[Interface]\nPrivateKey = …\nAddress = …\n\n[Peer]\nPublicKey = …\nEndpoint = …\nAllowedIPs = 0.0.0.0/0' },
  ];
  v.innerHTML = `<div class="grid two">
    ${card('WireGuard tunnels', `<div class="table-wrap"><table><thead><tr><th>Name</th><th>State</th><th>Autostart</th><th></th></tr></thead><tbody>
      ${conns.length ? conns.map(c => `<tr><td class="mono">${esc(c.name)}</td><td>${c.active ? pill('up', 'ok') : pill('down')}</td><td>${c.autoconnect ? 'yes' : 'no'}</td>
        <td class="actions">${c.active ? `<button class="btn sm" data-v="down" data-n="${esc(c.name)}">Stop</button>` : `<button class="btn sm primary" data-v="up" data-n="${esc(c.name)}">Start</button>`}
        <button class="btn sm" data-v="autoconnect-${c.autoconnect ? 'off' : 'on'}" data-n="${esc(c.name)}">${c.autoconnect ? 'No autostart' : 'Autostart'}</button>
        <button class="btn sm danger" data-v="delete" data-n="${esc(c.name)}">Delete</button></td></tr>`).join('')
      : '<tr><td colspan="4" class="muted">No tunnels yet — import one below.</td></tr>'}</tbody></table></div>
      ${form('vpn-route', routeFields, d.routing, 'Save routing')}
      ${d.routing.route_lan ? `<p class="help">DNS: ${d.dns_via ? `LAN lookups go through <b class="mono">${esc(d.dns_via)}</b> too, so the mobile network does not see them${(d.connections || []).some(c => c.active && c.name === d.routing.route_lan) ? '' : ' (the tunnel is down, so lookups fail until it is back)'}.` : 'the tunnel is down, so LAN lookups use the WAN like the rest of the LAN traffic.'} Through a tunnel the DNS page's IPv4 servers are used, or 1.1.1.1 and 9.9.9.9 if none are set.</p>` : ''}
      <p class="help">The router itself always stays on the WAN; only LAN clients are sent through the tunnel. LAN DNS still resolves through the router.</p>`)}
    ${card('Import a tunnel', `<div class="notice"><b>Cloudflare WARP on the router:</b> create a WireGuard profile with <span class="mono">wgcf register && wgcf generate</span> on any computer and paste <span class="mono">wgcf-profile.conf</span> here.<br><b>Cloudflare Zero Trust / WARP on devices:</b> nothing to configure — the firewall lets the WARP client's traffic through.</div>
      ${form('vpn-import', impFields, {}, 'Import')}`)}
  </div>
  ${card('Tunnel status (wg show)', `<pre>${esc(d.wg_show || 'no WireGuard interfaces up')}</pre><p class="help">LAN routing table 200: <span class="mono">${esc(d.table || 'empty')}</span></p>`)}`;
  v.querySelector('.table-wrap').addEventListener('click', async e => {
    const b = e.target.closest('[data-v]');
    if (!b) return;
    if (b.dataset.v === 'delete' && !await confirmBox('Delete tunnel', `Delete <b class="mono">${esc(b.dataset.n)}</b>?`, 'Delete')) return;
    if (await act(() => api('POST', `/vpn/${encodeURIComponent(b.dataset.n)}/${b.dataset.v}`), 'Done')) route();
  });
  onSubmit('vpn-route', routeFields, async x => { if (await act(() => api('PUT', '/vpn/routing', x))) route(); });
  onSubmit('vpn-import', impFields, async x => {
    const raw = $('#vpn-import [name=config]').value;
    if (await act(() => api('POST', '/vpn/import', { name: x.name, config: raw }))) route();
  });
}

// ---- SMS & USSD
async function pgMobile(v) {
  v.innerHTML = `<div class="grid two"><div class="card" id="sms-in"></div><div>
    ${card('Send SMS', form('sms-form', [{ key: 'number', label: 'Number', required: true, placeholder: '+91…' }, { key: 'text', label: 'Message', type: 'textarea', required: true }], {}, 'Send'))}
    ${card('USSD', `<form id="ussd-form" class="inline"><label>Code<input name="code" placeholder="*121#" required></label><button class="btn primary">Send</button><button type="button" class="btn" id="ussd-reply">Reply</button><button type="button" class="btn ghost" id="ussd-cancel">End session</button></form>
      <p class="help">Check data balance or packs with your carrier's code. "Reply" answers a menu inside an open session.</p><pre id="ussd-out" class="hidden"></pre>`)}
  </div></div>`;
  let dir = 'in', search = '';
  const loadSMS = async () => {
    const d = await api('GET', `/sms?dir=${dir}&limit=200&q=${encodeURIComponent(search)}`);
    const st = d.stats, rows = d[dir] || [];
    const tab = (k, l) => `<button class="btn sm ${dir === k ? 'primary' : ''}" data-tab="${k}">${l}</button>`;
    $('#sms-in').innerHTML = `<div class="card-head"><h3>Messages</h3><div class="btns" style="margin:0">${tab('in', `Received (${st.in_total})`)}${tab('out', `Sent (${st.out_total})`)}<button class="btn sm" id="sms-refresh">Refresh</button></div></div>
      <form id="sms-search" class="inline"><label>Search<input name="q" value="${esc(search)}" placeholder="${dir === 'in' ? 'sender or text' : 'number, text or API key'}"></label><button class="btn sm">Find</button></form>
      <p class="help">All SMS are kept in the router's SQLite log (${st.in_today} received and ${st.out_today} sent today); received messages are removed from the SIM.</p>
      <div class="table-wrap"><table>${dir === 'in' ? `<thead><tr><th>From</th><th>Time</th><th>Message</th>${MODS.whatsapp ? '<th>WhatsApp</th>' : ''}<th>Other forwards</th></tr></thead><tbody>
      ${rows.length ? rows.map(m => `<tr>
        <td class="mono">${esc(m.sender)}${m.origin === 'sim-import' ? '<br><span class="muted small">imported from SIM</span>' : ''}${m.ctl ? '<br><span class="muted small">remote control command</span>' : ''}</td>
        <td class="small">${esc(fmtTS(m.sms_time || m.stored_at))}</td>
        <td style="white-space:pre-wrap">${esc(m.text)}</td>
        ${MODS.whatsapp ? `<td>${waStatus(m)}</td>` : ''}<td>${fwdStatus(m)}</td></tr>`).join('') : `<tr><td colspan="${MODS.whatsapp ? 5 : 4}" class="muted">No messages.</td></tr>`}`
      : `<thead><tr><th>To</th><th>Time</th><th>Message</th><th>Sent by</th><th>Status</th></tr></thead><tbody>
      ${rows.length ? rows.map(m => `<tr>
        <td class="mono">${esc(m.recipient)}</td>
        <td class="small">${esc(fmtTS(m.created_at))}${m.sent_at ? `<br><span class="muted">sent ${esc(fmtTS(m.sent_at).slice(11))}</span>` : ''}</td>
        <td style="white-space:pre-wrap">${esc(m.text)}${m.message_source === 'default' ? ' ' + pill('default') : ''}</td>
        <td>${m.channel === 'api' ? `API key <b>${esc(m.api_key)}</b>` : esc({ 'web-ui': 'Web UI', 'sms-control': 'SMS remote control reply', notify: 'Notification' }[m.channel] || m.channel)}<br><span class="mono small muted">${esc(m.client_ip)}</span></td>
        <td>${smsStatus(m.status)}${deliveryStatus(m)}<div class="small muted">${esc(m.error)}</div></td></tr>`).join('') : '<tr><td colspan="5" class="muted">No messages.</td></tr>'}`}
      </tbody></table></div>`;
    $('#sms-refresh').onclick = loadSMS;
    $('#sms-search').addEventListener('submit', e => { e.preventDefault(); search = e.target.q.value.trim(); loadSMS(); });
  };
  $('#sms-in').addEventListener('click', async e => {
    const t = e.target.closest('[data-tab]');
    if (t) { dir = t.dataset.tab; loadSMS(); return; }
    const b = e.target.closest('[data-refwd]');
    if (b && await act(() => api('POST', `/sms/in/${b.dataset.refwd}/forward` + (b.dataset.target ? '/' + b.dataset.target : '')))) loadSMS();
  });
  onSubmit('sms-form', [{ key: 'number' }, { key: 'text' }], async (x, f) => { if (await act(() => api('POST', '/sms', x))) { f.reset(); loadSMS(); } });
  const ussd = async action => {
    const out = $('#ussd-out'); out.classList.remove('hidden'); out.textContent = 'Waiting for the network…';
    try { const r = await api('POST', '/ussd', { code: $('#ussd-form [name=code]').value.trim(), action }); out.textContent = r.output; }
    catch (e) { out.textContent = e.message; }
  };
  $('#ussd-form').addEventListener('submit', e => { e.preventDefault(); ussd('initiate'); });
  $('#ussd-reply').onclick = () => ussd('respond');
  $('#ussd-cancel').onclick = () => ussd('cancel');
  await loadSMS();
}

const fmtTS = t => String(t || '').replace('T', ' ').slice(0, 19);
const smsStatus = s => s === 'sent' ? pill('sent', 'ok') : s === 'sending' || s === 'queued' ? pill(s, 'warn')
  : s === 'unknown' ? pill('maybe sent', 'warn') : pill(s || 'failed', 'bad');
// delivery report of a sent SMS (requested for web UI and, by default, API sends)
const deliveryStatus = m => {
  if (!m.delivery) return '';
  const t = m.delivery_at ? ' ' + fmtTS(m.delivery_at).slice(11) : '';
  const p = m.delivery === 'delivered' ? pill('delivered' + t, 'ok') : m.delivery === 'failed' ? pill('not delivered', 'bad')
    : m.status === 'sent' ? pill('awaiting report') : '';
  return p ? `<div>${p}${m.delivery_note && m.delivery !== 'delivered' ? `<span class="small muted"> ${esc(m.delivery_note)}</span>` : ''}</div>` : '';
};
// webhook / Telegram / ntfy forwarding of a received SMS
function fwdStatus(m) {
  const f = m.forwards || [];
  if (!f.length) return '<span class="muted small">—</span>';
  return f.map(x => {
    const name = { webhook: 'Webhook', telegram: 'Telegram', ntfy: 'ntfy' }[x.target] || x.target;
    const p = x.status === 'forwarded' ? pill(name, 'ok') : x.status === 'pending' ? pill(name + (x.attempts ? ` retrying (${x.attempts})` : ' queued'), 'warn') : pill(name + ' failed', 'bad');
    const retry = x.status === 'failed' ? ` <button class="btn sm" data-refwd="${m.id}" data-target="${esc(x.target)}">Retry</button>` : '';
    return `<div>${p}${retry}${x.note ? `<div class="small muted">${esc(x.note)}</div>` : ''}</div>`;
  }).join('');
}
function waStatus(m) {
  const note = m.wa_note ? `<div class="small muted">${esc(m.wa_note)}</div>` : '';
  switch (m.wa_status) {
    case 'forwarded': return pill('forwarded', 'ok') + `<div class="small muted">${esc(fmtTS(m.wa_forwarded_at))}<br>to ${esc(m.wa_target)}</div>`;
    case 'pending': return pill(m.wa_attempts ? `retrying (${m.wa_attempts})` : 'queued', 'warn') + note;
    case 'failed': return pill('failed', 'bad') + note + `<button class="btn sm" data-refwd="${m.id}">Retry</button>`;
    default: return pill(m.wa_status === 'off' ? 'not forwarded' : m.wa_status) + note + `<button class="btn sm ghost" data-refwd="${m.id}">Forward</button>`;
  }
}

// ---- Public SMS gateway (kstick-smsgw, published through Cloudflare Tunnel)
async function pgSMSGW(v) {
  const d = await api('GET', '/smsgw');
  const c = d.config, l = c.limits;
  const svc = s => s === 'active' ? pill('running', 'ok') : pill(s || 'unknown', 'bad');
  const fields = [
    { key: 'enabled', label: 'Accept API requests', type: 'check' },
    { key: 'default_message', label: 'Default message', type: 'textarea', help: 'Sent when an API request has no "message". Leave empty to require a message in every request.' },
    { key: 'max_length', label: 'Maximum message length (characters)', type: 'number', help: '160 GSM characters (70 with Unicode/emoji) fit in one SMS; longer messages are sent as several parts.' },
    { key: 'public_url', label: 'Public URL', placeholder: 'https://sms.example.com', help: 'Shown in the docs here; the hostname itself is set in the Cloudflare Tunnel.' },
    { key: 'delivery_reports', label: 'Request delivery reports', type: 'check', help: 'The network reports when each API message reaches the phone; apps read it with GET /api/v1/sms/{id}.' },
  ];
  const lf = [
    { key: 'per_key_per_minute', label: 'Per API key, per minute', type: 'number' },
    { key: 'per_key_per_day', label: 'Per API key, per day', type: 'number' },
    { key: 'per_number_per_hour', label: 'Per phone number, per hour', type: 'number' },
    { key: 'global_per_day', label: 'Whole gateway, per day', type: 'number' },
  ];
  v.innerHTML = `<div class="grid two">
    ${card('Status', kv([
      ['Gateway', c.enabled ? pill('accepting requests', 'ok') : pill('disabled', 'warn')],
      ['Gateway service', svc(d.services.smsgw)],
      ['Cloudflare Tunnel', svc(d.services.cloudflared)],
      ['Public URL', c.public_url ? `<span class="mono">${esc(c.public_url)}</span>` : '—'],
      ['Today', `${d.today.sent} sent · ${d.today.failed} failed`],
    ]) + `<p class="help">API: <span class="mono">POST ${esc(c.public_url || '')}/api/v1/sms</span> with <span class="mono">Authorization: Bearer &lt;key&gt;</span> and JSON <span class="mono">{"to":"9876543210","message":"…"}</span>. Numbers must be 10-digit Indian mobiles; they are sent as +91…
      Send an <span class="mono">Idempotency-Key</span> header so a retried request never sends twice. Status and delivery report: <span class="mono">GET /api/v1/sms/{id}</span>.
      Keys with the inbox permission can read received SMS: <span class="mono">GET /api/v1/inbox?after_id=N</span>. Full docs on the public URL.</p>`)}
    ${card('Settings', form('smsgw-form', fields, c, 'Save settings'))}
  </div>
  <div class="grid two">
    ${card('Rate limits', `<p class="help">0 means no limit. Counted per request accepted for sending.</p>` + form('smsgw-limits', lf, l, 'Save limits'))}
    <div class="card" id="smsgw-keys"></div>
  </div>
  ${card('Recent API messages', `<div class="table-wrap"><table><thead><tr><th>Time</th><th>To</th><th>API key</th><th>From IP</th><th>Message</th><th>Status</th></tr></thead><tbody>
    ${d.log.length ? d.log.map(e => `<tr>
      <td class="small">${esc(e.created_at.replace('T', ' ').slice(0, 19))}</td><td class="mono">${esc(e.recipient)}</td><td>${esc(e.api_key)}</td>
      <td class="mono small">${esc(e.client_ip)}</td><td style="white-space:pre-wrap">${esc(e.text)}${e.message_source === 'default' ? ' ' + pill('default') : ''}</td>
      <td>${smsStatus(e.status)}${deliveryStatus(e)}<div class="small muted">${esc(e.error || '')}</div></td></tr>`).join('')
    : '<tr><td colspan="6" class="muted">No messages sent yet.</td></tr>'}</tbody></table></div>`)}`;

  const save = async extra => {
    const body = { enabled: c.enabled, default_message: c.default_message, max_length: c.max_length, public_url: c.public_url,
      delivery_reports: c.delivery_reports, limits: { ...l }, ...extra };
    if (await act(() => api('PUT', '/smsgw', body))) route();
  };
  onSubmit('smsgw-form', fields, async x => save(x));
  onSubmit('smsgw-limits', lf, async x => save({ limits: x }));

  const keysEl = $('#smsgw-keys');
  keysEl.innerHTML = `<div class="card-head"><h3>API keys</h3><button class="btn sm primary" id="key-new">+ New key</button></div>
    <div class="table-wrap"><table><thead><tr><th>Name</th><th>Key</th><th>Permissions</th><th>Created</th><th></th></tr></thead><tbody>
    ${d.keys.length ? d.keys.map(k => `<tr><td>${esc(k.name)}</td><td class="mono">${esc(k.prefix)}…</td>
      <td>${(k.scopes || ['send']).map(sc => pill(sc === 'inbox' ? 'read inbox' : 'send', sc === 'inbox' ? 'warn' : '')).join(' ')}</td>
      <td class="small">${esc(k.created.replace('T', ' ').slice(0, 16))}</td>
      <td class="actions"><span class="small muted">${d.today.per_key?.[k.name] || 0} today</span> <button class="btn sm" data-scopes="${esc(k.name)}">Permissions</button>
        <button class="btn sm danger" data-revoke="${esc(k.name)}">Revoke</button></td></tr>`).join('')
    : '<tr><td colspan="5" class="muted">No keys yet. Create one for each app that sends SMS.</td></tr>'}</tbody></table></div>
    <p class="help">Keys are stored hashed and shown only once, when created. "Read inbox" lets an app read every received SMS,
    including one-time codes: give it only to apps that need it.</p>`;
  const scopeFields = [
    { key: 'send', label: 'Send SMS (POST /api/v1/sms, GET /api/v1/sms/{id})', type: 'check' },
    { key: 'inbox', label: 'Read received SMS (GET /api/v1/inbox), including one-time codes', type: 'check' },
  ];
  const scopesOf = x => ['send', 'inbox'].filter(k => x[k]);
  $('#key-new').onclick = async () => {
    const sug = await act(() => api('GET', '/smsgw/keyname'));
    const x = await modal('New API key', [{ key: 'name', label: 'Name', placeholder: 'leave empty for three random words', help: 'Generated automatically; you can change it (e.g. to the app that will use it).' }, ...scopeFields],
      { name: sug && sug.name || '', send: true }, 'Create');
    if (!x) return;
    const r = await act(() => api('POST', '/smsgw/keys', { name: x.name, scopes: scopesOf(x) }));
    if (!r || !r.key) return;
    await modal('API key created', [], {}, 'Done', `Copy this key now; it will not be shown again.<pre class="mono" style="user-select:all;white-space:pre-wrap;word-break:break-all">${esc(r.key)}</pre>`);
    route();
  };
  keysEl.addEventListener('click', async e => {
    const sb = e.target.closest('[data-scopes]');
    if (sb) {
      const k = d.keys.find(x => x.name === sb.dataset.scopes), cur = k.scopes || ['send'];
      const x = await modal('Permissions — ' + k.name, scopeFields, { send: cur.includes('send'), inbox: cur.includes('inbox') });
      if (x && await act(() => api('PUT', '/smsgw/keys/' + encodeURIComponent(k.name), { scopes: scopesOf(x) }))) route();
      return;
    }
    const b = e.target.closest('[data-revoke]');
    if (!b) return;
    if (await confirmBox('Revoke API key', `Apps using <b>${esc(b.dataset.revoke)}</b> will stop working immediately.`, 'Revoke'))
      if (await act(() => api('DELETE', '/smsgw/keys/' + encodeURIComponent(b.dataset.revoke)))) route();
  });
}

// ---- Forward received SMS to WhatsApp via a WAWAT Messaging API server
async function pgWhatsApp(v) {
  const d = await api('GET', '/wawat');
  const c = d.config;
  const conn = [
    { key: 'server_url', label: 'Server address', required: true, placeholder: 'https://wa.example.com', help: 'Base URL of the WAWAT server, no trailing slash.' },
    { key: 'api_key', label: 'API token', type: 'password', placeholder: d.key_set ? `saved (${d.key_hint}) — leave empty to keep` : 'paste the API token', help: 'Sent as x-api-key. Stored on the router only and never shown again.' },
  ];
  const fwd = [
    { key: 'enabled', label: 'Forward received SMS to WhatsApp', type: 'check', help: 'Only SMS that arrive after enabling are forwarded. Every SMS is kept in the SMS log (SMS & USSD page), where older ones can be forwarded manually.' },
    { key: 'target', label: 'WhatsApp number', required: true, placeholder: '9876543210', help: '10 digits = Indian number (91 is added). Other countries: full number with country code, digits only. A group id ending in @g.us also works.' },
    { key: 'template', label: 'Message format', type: 'textarea', help: 'Placeholders: {from} {time} {text} {device}' },
    { key: 'ignore', label: 'Ignore senders containing (one per line)', type: 'textarea', list: true, placeholder: 'AD-\nVM-AIRTEL', help: 'Optional, case-insensitive. Useful for carrier promotions.' },
      ];
  const vals = { ...c, ignore: (c.ignore || []).join('\n') };
  const status = d.last_error ? pill('retrying: ' + d.last_error, 'bad') : c.enabled ? pill('forwarding', 'ok') : pill('off', 'warn');
  const hdrs = h => h && Object.keys(h).length ? `<div class="small muted mono">${Object.entries(h).map(([k, x]) => `${esc(k)}=${esc(x)}`).join(' · ')}</div>` : '';
  v.innerHTML = `<div class="grid two">
    <div>${card('Connection', form('wa-conn', conn, { server_url: c.server_url }, 'Save & verify') + '<pre id="wa-verify" class="hidden"></pre>')}
    ${card('Test', `<p class="help">Sends a sample forwarded SMS to <b class="mono">${esc(d.resolved_target || '— set a number first')}</b> (resolved WhatsApp id).</p><button class="btn" id="wa-test">Send test message</button>`)}</div>
    ${card('Forwarding', kv([['Status', status], ['Sends to', d.resolved_target ? `<span class="mono">${esc(d.resolved_target)}</span>` : '—'], ['Received SMS', Object.entries(d.handled || {}).map(([k, n]) => `${n} ${esc(k === 'off' ? 'not forwarded' : k)}`).join(' · ') || '—']]) + form('wa-fwd', fwd, vals, 'Save'))}
  </div>
  ${card('Debug log', `<div class="table-wrap"><table><thead><tr><th>Time</th><th>Kind</th><th>Request</th><th>Result</th><th>SMS</th><th>Response</th></tr></thead><tbody>
    ${d.log.length ? d.log.slice().reverse().map(e => `<tr>
      <td class="small">${esc((e.time || '').replace('T', ' ').slice(0, 19))}</td><td>${esc(e.kind)}</td>
      <td class="mono small">${e.method ? esc(e.method + ' ' + e.path) + `<br>${e.status ? 'HTTP ' + e.status : ''} ${e.ms ? e.ms + ' ms' : ''}` : ''}</td>
      <td>${e.result === 'ok' ? pill('ok', 'ok') : e.result === 'skipped' ? pill('skipped', 'warn') : pill('error', 'bad')}<div class="small muted">${esc(e.error || '')}</div></td>
      <td class="small">${e.from ? `<b>${esc(e.from)}</b><br>` : ''}<span style="white-space:pre-wrap">${esc(e.text || '')}</span></td>
      <td class="small mono" style="max-width:320px;word-break:break-all">${esc(e.response || '')}${hdrs(e.auth_headers)}</td></tr>`).join('')
    : '<tr><td colspan="6" class="muted">No activity yet.</td></tr>'}</tbody></table></div>
    <p class="help">Token is never logged. auth headers show which API key and WhatsApp session the server matched. 503 = WhatsApp client not ready; 429 = server rate limit.</p>`, '<button class="btn sm" id="wa-refresh">Refresh</button>')}`;

  const body = (x = {}) => ({ enabled: c.enabled, server_url: c.server_url, api_key: '', target: c.target, template: c.template, ignore: c.ignore || [], ...x });
  onSubmit('wa-conn', conn, async x => {
    if (!await act(() => api('PUT', '/wawat', body(x)))) return;
    const out = $('#wa-verify'); out.classList.remove('hidden'); out.textContent = 'Checking /api/v1/status…';
    try {
      const r = await api('POST', '/wawat/verify');
      out.textContent = r.error ? `✗ ${r.error}` : `${r.ready ? '✓' : '✗'} ${r.status}${r.linked_number ? ' · linked number ' + r.linked_number : ''}`
        + (r.auth_headers && Object.keys(r.auth_headers).length ? '\n' + Object.entries(r.auth_headers).map(([k, x]) => `${k}: ${x}`).join('\n') : '');
    } catch (e) { out.textContent = e.message; }
  });
  onSubmit('wa-fwd', fwd, async x => { if (await act(() => api('PUT', '/wawat', body(x)))) route(); });
  $('#wa-test').onclick = async () => { await act(() => api('POST', '/wawat/test')); route(); };
  $('#wa-refresh').onclick = () => route();
}

// ---- Forward received SMS to a webhook, a Telegram bot or an ntfy topic
async function pgForward(v) {
  const d = await api('GET', '/forward');
  const c = d.config, sec = d.secrets;
  const keepHint = (h, what) => h ? `saved (${h}): leave empty to keep, "-" to remove` : what;
  const wf = [
    { key: 'enabled', label: 'Send each received SMS to a webhook', type: 'check' },
    { key: 'url', label: 'URL', placeholder: 'https://example.com/sms-hook', help: 'POST with JSON {"event":"sms.received","id","from","text","sms_time","received_at","device"}. https required, except for private IP addresses (LAN, Mesh).' },
    { key: 'secret', label: 'Signing secret (optional)', type: 'password', placeholder: keepHint(sec.webhook_secret, 'any long random text'),
      help: 'Adds X-KStick-Signature: sha256=HMAC-SHA256(secret, X-KStick-Timestamp + "." + body). Check it, and reject old timestamps.' },
  ];
  const tf = [
    { key: 'enabled', label: 'Send each received SMS to Telegram', type: 'check' },
    { key: 'bot_token', label: 'Bot token', type: 'password', placeholder: keepHint(sec.telegram_token, '123456789:AA… from @BotFather') },
    { key: 'chat_id', label: 'Chat id', placeholder: '123456789 or -100… for a group', help: 'Send the bot a message first; your chat id is in https://api.telegram.org/bot<token>/getUpdates.' },
  ];
  const nf = [
    { key: 'enabled', label: 'Send each received SMS to ntfy', type: 'check' },
    { key: 'server', label: 'Server', placeholder: 'https://ntfy.sh' },
    { key: 'topic', label: 'Topic', placeholder: 'a long, hard-to-guess name', help: 'On ntfy.sh anyone who knows the topic can read it: pick a random name, or use an access token.' },
    { key: 'token', label: 'Access token (optional)', type: 'password', placeholder: keepHint(sec.ntfy_token, 'tk_…') },
  ];
  const ig = [{ key: 'ignore', label: 'Ignore senders containing (one per line)', type: 'textarea', list: true, placeholder: 'AD-\nVM-AIRTEL', help: 'Applies to all three. Case-insensitive.' }];
  const logRows = Object.entries(d.log || {}).flatMap(([t, l]) => l.map(e => ({ ...e, target: t }))).sort((a, b) => (b.time || '').localeCompare(a.time || ''));
  const state = x => x.enabled ? pill('on', 'ok') : pill('off');
  v.innerHTML = `<p class="help">Received SMS can go to any of these as well as WhatsApp. Each SMS is queued when it arrives and retried
    with growing gaps (30 s up to hourly) for about a day; SMS remote control commands are never forwarded. Status per SMS: SMS &amp; USSD page.</p>
  <div class="grid two">
    ${card('Webhook', form('fw-webhook', wf, c.webhook, 'Save', '<button type="button" class="btn" data-test="webhook">Send test</button>'), state(c.webhook))}
    ${card('Telegram', form('fw-telegram', tf, { ...c.telegram, bot_token: '' }, 'Save', '<button type="button" class="btn" data-test="telegram">Send test</button>'), state(c.telegram))}
  </div>
  <div class="grid two">
    ${card('ntfy', form('fw-ntfy', nf, { ...c.ntfy, token: '' }, 'Save', '<button type="button" class="btn" data-test="ntfy">Send test</button>'), state(c.ntfy))}
    ${card('Filter', form('fw-ignore', ig, { ignore: c.ignore }, 'Save'))}
  </div>
  ${card('Recent requests', `<div class="table-wrap"><table><thead><tr><th>Time</th><th>Target</th><th>Kind</th><th>Request</th><th>Result</th><th>SMS</th></tr></thead><tbody>
    ${logRows.length ? logRows.slice(0, 60).map(e => `<tr><td class="small">${esc(fmtTS(e.time))}</td><td>${esc(e.target)}</td><td>${esc(e.kind)}</td>
      <td class="mono small">${esc(e.path || '')}<br>${e.status ? 'HTTP ' + e.status : ''} ${e.ms ? e.ms + ' ms' : ''}</td>
      <td>${e.result === 'ok' ? pill('ok', 'ok') : pill('error', 'bad')}<div class="small muted">${esc(e.error || '')}</div></td>
      <td class="small">${e.from ? `<b>${esc(e.from)}</b><br>` : ''}<span style="white-space:pre-wrap">${esc(e.text || '')}</span></td></tr>`).join('')
    : '<tr><td colspan="6" class="muted">No activity since the router web UI started.</td></tr>'}</tbody></table></div>
    <p class="help">Secrets and the Telegram bot token are never logged.</p>`, '<button class="btn sm" id="fw-refresh">Refresh</button>')}`;
  // every save sends the whole config; empty secrets keep the stored ones
  const body = () => ({ webhook: { ...c.webhook, secret: '' }, telegram: { ...c.telegram, bot_token: '' }, ntfy: { ...c.ntfy, token: '' }, ignore: c.ignore || [] });
  const save = async (part, x) => { const b = body(); if (part === 'ignore') b.ignore = x.ignore; else b[part] = { ...b[part], ...x }; if (await act(() => api('PUT', '/forward', b))) route(); };
  onSubmit('fw-webhook', wf, x => save('webhook', x));
  onSubmit('fw-telegram', tf, x => save('telegram', x));
  onSubmit('fw-ntfy', nf, x => save('ntfy', x));
  onSubmit('fw-ignore', ig, x => save('ignore', x));
  v.addEventListener('click', async e => {
    const b = e.target.closest('[data-test]');
    if (b) { await act(() => api('POST', '/forward/test/' + b.dataset.test)); route(); }
  });
  $('#fw-refresh').onclick = () => route();
}

// ---- Diagnostics
async function pgDiag(v) {
  const ifSel = `<label>Interface<select name="iface"><option value="">auto (routing)</option><option>wwan0</option><option>usb0</option><option>wlan0</option></select></label>`;
  v.innerHTML = `<div class="grid">
    ${card('Ping', `<form class="inline" data-tool="ping"><label>Host<input name="target" value="1.1.1.1" required></label>${ifSel}<label>Count<input name="count" type="number" value="4" min="1" max="10" style="width:70px"></label><button class="btn primary">Run</button></form>`)}
    ${card('Traceroute', `<form class="inline" data-tool="traceroute"><label>Host<input name="target" value="1.1.1.1" required></label>${ifSel}<button class="btn primary">Run</button></form>`)}
    ${card('DNS lookup', `<form class="inline" data-tool="dns"><label>Name<input name="target" value="cloudflare.com" required></label><label>Server (optional)<input name="server" placeholder="system"></label><button class="btn primary">Lookup</button></form>`)}
  </div>
  ${card('Output', `<div class="btns" style="margin:0 0 12px"><button class="btn sm" data-view="routes">Routing tables</button><button class="btn sm" data-view="arp">ARP / neighbours</button><button class="btn sm" data-view="conntrack">Connection table</button><button class="btn sm" data-view="sockets">Listening sockets</button></div><pre id="diag-out">Run a tool or pick a view.</pre>`)}`;
  const out = $('#diag-out');
  $$('form[data-tool]', v).forEach(f => f.addEventListener('submit', async e => {
    e.preventDefault();
    const b = f.querySelector('button'); b.disabled = true; out.textContent = 'Running…';
    const fd = Object.fromEntries(new FormData(f));
    try { const r = await api('POST', '/diag/' + f.dataset.tool, { ...fd, count: Number(fd.count || 4) }); out.textContent = r.output; }
    catch (err) { out.textContent = err.message; } finally { b.disabled = false; }
  }));
  $$('[data-view]', v).forEach(b => b.onclick = async () => { out.textContent = 'Loading…'; try { out.textContent = (await api('GET', '/diag/' + b.dataset.view)).output; } catch (e) { out.textContent = e.message; } });
}

// ---- Logs
async function pgLogs(v) {
  const srcs = [['system', 'System (all)'], ['firewall', 'Firewall'], ['wan', 'WAN failover'], ['network', 'NetworkManager'], ['modem', 'ModemManager'], ['dhcp', 'DHCP / DNS'], ['auth', 'Logins & auth'], ['webui', 'Web UI'], ['kernel', 'Kernel']];
  v.innerHTML = card('Logs', `<div class="inline" style="margin-bottom:12px"><label>Source<select id="log-src">${srcs.map(([k, l]) => `<option value="${k}">${esc(l)}</option>`).join('')}</select></label>
    <label>Lines<select id="log-n"><option>100</option><option selected>200</option><option>500</option><option>1000</option></select></label>
    <button class="btn" id="log-go">Refresh</button><label class="check"><input type="checkbox" id="log-auto"> auto-refresh</label></div><pre id="log-out"></pre>`);
  const load = async () => {
    const r = await api('GET', `/logs?source=${$('#log-src').value}&lines=${$('#log-n').value}`);
    const o = $('#log-out'); o.textContent = r.output || '(empty)'; o.scrollTop = o.scrollHeight;
  };
  $('#log-go').onclick = load; $('#log-src').onchange = load; $('#log-n').onchange = load;
  every(5000, async () => { if ($('#log-auto')?.checked) await load(); });
  await load();
}

// ---- System
// Web interface style (ui.go): classic (this UI's sibling) or the Windows-XP-style desktop; same API and settings.
const UI_NAMES = { classic: 'Classic', xp: 'Windows XP style' };
const UI_FIELDS = [{ key: 'style', label: 'Style for every browser', type: 'select', options: Object.entries(UI_NAMES) }];
function sysUICard(u) {
  const avail = u.available || [];
  const links = avail.map(st => u.browser === st ? `<b>${esc(UI_NAMES[st])}</b>` : `<a href="/?ui=${st}">${esc(UI_NAMES[st])}</a>`).join(' · ')
    + (u.browser ? ` · <a href="/?ui=default">follow the router's setting</a>` : '');
  return card('Web interface', `<p class="muted">Two looks for the same settings. Now showing: <b>${esc(UI_NAMES[u.serving] || u.serving)}</b>${u.browser ? ' ' + pill('this browser only', 'warn') : ''}.</p>`
    + (avail.length > 1 ? form('ui-form', UI_FIELDS, u, 'Save') + `<p class="help">Only this browser: ${links}. If one style ever stops working, open <span class="mono">/?ui=classic</span> on the router's address.</p>`
      : `<p class="help">Only the ${esc(UI_NAMES[avail[0]] || 'classic')} files are installed.</p>`));
}
async function pgSystem(v) {
  const [s, svcs, upd, ui] = await Promise.all([api('GET', '/system'), api('GET', '/services'), api('GET', '/updates'), api('GET', '/ui').catch(() => null)]);
  setTimeout(drawThermalCard, 0);
  const pwFields = [
    { key: 'current', label: 'Current password', type: 'password', required: true },
    { key: 'new', label: 'New password', type: 'password', required: true, help: '10+ characters. This is the system password of the "' + s.admin_user + '" account.' },
    { key: 'new2', label: 'Repeat new password', type: 'password', required: true },
  ];
  const batFields = [
    { key: 'enabled', label: 'Protect the battery', type: 'check' },
    { key: 'warn_pct', label: 'Warn at (%)', type: 'number', min: 7, max: 50 },
    { key: 'save_pct', label: 'Power saving at (%)', type: 'number', min: 4, max: 49 },
    { key: 'off_pct', label: 'Shut down at (%)', type: 'number', min: 3, max: 48 },
  ];
  const adminFields = [{ key: 'confirm_secs', label: 'Seconds to confirm (0 = off; 30-600)', type: 'number', min: 0, max: 600 }];
  const sysFields = [{ key: 'hostname', label: 'Hostname' }, { key: 'timezone', label: 'Timezone', type: 'select', options: [[s.timezone, s.timezone]] }];
  v.innerHTML = `<div class="grid two">
    ${card('Device', kv([['Model', esc(s.model)], ['OS', esc(s.os)], ['Kernel', esc(s.kernel)], ['Uptime', fmtDur(s.uptime)],
      ['Time', `${esc(s.time.replace('T', ' ').slice(0, 19))} ${s.ntp_synced ? pill('NTP synced', 'ok') : pill('not synced', 'warn')}`]])
      + form('sys-form', sysFields, s, 'Save') + `<div class="btns"><button class="btn danger" id="reboot">Reboot router</button></div>`)}
    ${card('Admin password', form('pw-form', pwFields, {}, 'Change password'))}
    ${sysWatchdogCard(s.watchdog)}
    ${s.battery_cfg ? card('Battery protection', `<p class="muted">On battery: a warning, then power saving (CPU 200 MHz, screen sleeps after 15 s), then a clean shutdown before the battery is empty. ${s.battery_state && s.battery_state.saving ? pill('power saving on', 'warn') : ''}</p>` + form('bat-form', batFields, s.battery_cfg, 'Save') + '<p class="help">The warning and the shutdown notice go by SMS when SMS remote control notifications are on. The shutdown waits for 3 readings in a row (90 s), never in the first 3 minutes after boot.</p>') : ''}
    ${card('Undo risky changes', `<p class="muted">Firewall, LAN, VPN routing and IPv6 changes, and blocking a client, are undone automatically unless you confirm them in time, so a bad rule can't lock you out (handy over Mesh). A restart of the web UI or the router before confirming also undoes them.</p>`
      + form('admin-form', adminFields, s, 'Save'))}
    ${ui ? sysUICard(ui) : ''}
    <div class="card" id="tg-card"></div>
    ${card('Services', `<table><thead><tr><th>Service</th><th>State</th><th>Since</th><th></th></tr></thead><tbody>${svcs.map(x => `<tr><td class="mono">${esc(x.name)}</td>
      <td>${x.ActiveState === 'active' ? pill(x.SubState, 'ok') : pill(x.ActiveState, x.ActiveState === 'failed' ? 'bad' : '')}</td><td class="small">${esc((x.ActiveEnterTimestamp || '').replace(/ [A-Z]+$/, ''))}</td>
      <td class="actions"><button class="btn sm" data-svc="${esc(x.name)}">Restart</button></td></tr>`).join('')}</tbody></table>`)}
    ${card('Software updates', `<p>${(upd.upgradable || []).length ? `<b>${upd.upgradable.length}</b> package(s) can be upgraded.` : upd.upgradable_known === false ? 'Not checked yet.' : 'System is up to date (as of the last check).'} <span class="muted small">Last check: ${esc((upd.last_check || '').replace('T', ' ').slice(0, 16))}</span></p>
      ${(upd.upgradable || []).length ? `<pre style="max-height:140px">${esc(upd.upgradable.join('\n'))}</pre>` : ''}
      <div class="btns"><button class="btn" id="upd-check" ${upd.running ? 'disabled' : ''}>Check now</button><button class="btn primary" id="upd-go" ${upd.running ? 'disabled' : ''}>Install updates</button>${upd.running ? pill('running…', 'warn') : ''}</div>
      ${upd.log ? `<pre id="upd-log" style="margin-top:12px;max-height:240px">${esc(upd.log)}</pre>` : ''}`)}
    ${card('Backup & restore', `<p class="muted">The backup contains the firewall, DHCP, DNS, WAN and VPN routing settings plus the Wi-Fi password and LTE APN. Keep it private. WireGuard keys are not included.</p>
      <div class="btns"><button class="btn" id="backup">Download backup</button><label class="btn" style="margin:0">Restore from file…<input type="file" id="restore" accept=".json,application/json" class="hidden"></label></div>`)}
  </div>`;
  api('GET', '/system/timezones').then(z => { const sel = $('#sys-form [name=timezone]'); if (sel) sel.innerHTML = z.map(t => `<option ${t === s.timezone ? 'selected' : ''}>${esc(t)}</option>`).join(''); }).catch(() => {});
  onSubmit('sys-form', sysFields, async x => { await act(() => api('PUT', '/system', x)); });
  onSubmit('bat-form', batFields, async x => { await act(() => api('PUT', '/battery', x)); });
  onSubmit('admin-form', adminFields, async x => { await act(() => api('PUT', '/admin', x)); });
  onSubmit('ui-form', UI_FIELDS, async x => { if (await act(() => api('PUT', '/ui', x), 'Saved. Reload the page to switch.')) setTimeout(route, 500); });
  onSubmit('pw-form', pwFields, async (x, f) => {
    if (x.new !== x.new2) return toast('The new passwords do not match', true);
    if (await act(() => api('POST', '/system/password', { current: x.current, new: x.new }))) f.reset();
  });
  $('#reboot').onclick = async () => { if (await confirmBox('Reboot', 'Reboot the router now? Everything goes offline for about a minute.', 'Reboot')) act(() => api('POST', '/system/reboot')); };
  v.addEventListener('click', async e => { const b = e.target.closest('[data-svc]'); if (b && await confirmBox('Restart service', `Restart <b class="mono">${esc(b.dataset.svc)}</b>?`, 'Restart')) { await act(() => api('POST', `/services/${b.dataset.svc}/restart`)); setTimeout(route, 1500); } });
  $('#upd-check').onclick = async () => { if (await act(() => api('POST', '/updates/check'))) setTimeout(route, 1000); };
  $('#upd-go').onclick = async () => { if (await confirmBox('Install updates', 'Download and install all available updates? Services may restart.', 'Install')) { if (await act(() => api('POST', '/updates/upgrade'))) setTimeout(route, 1000); } };
  if (upd.running) every(3000, async () => { const u = await api('GET', '/updates?log_only=1'); const l = $('#upd-log'); if (l) { l.textContent = u.log; l.scrollTop = l.scrollHeight; } if (!u.running) route(); });
  $('#backup').onclick = async () => {
    const b = await act(() => api('GET', '/backup'), 'Backup downloaded');
    if (!b || b === true) return;
    const a = document.createElement('a');
    a.href = URL.createObjectURL(new Blob([JSON.stringify(b, null, 2)], { type: 'application/json' }));
    a.download = `kstick-router-${s.hostname}-${new Date().toISOString().slice(0, 10)}.json`;
    a.click(); setTimeout(() => URL.revokeObjectURL(a.href), 5000);
  };
  $('#restore').onchange = async e => {
    const file = e.target.files[0]; e.target.value = '';
    if (!file) return;
    let doc; try { doc = JSON.parse(await file.text()); } catch { return toast('That file is not valid JSON', true); }
    if (await confirmBox('Restore backup', `Restore settings from <b>${esc(file.name)}</b> (created ${esc(doc.created || '?')})? Current settings are replaced.`, 'Restore')) act(() => api('POST', '/restore', doc));
  };
}

// ---------------------------------------------------------------- shell

// battery row for the dashboard (MF800 pocket router; absent on the KStick)
function battRow(b) {
  if (!b) return null;
  const p = b.percent, cls = b.charging ? 'ok' : p != null && p <= 15 ? 'bad' : p != null && p <= 35 ? 'warn' : '';
  const state = b.charging ? pill('⚡ charging', 'ok') : b.external_power ? pill('on USB power', '') : pill('on battery', p != null && p <= 15 ? 'bad' : '');
  return ['Battery', `<span class="stat ${cls}">${p != null ? p + '%' : '—'}</span> ${state}${p != null ? bar(p) : ''}`
    + `<span class="small muted">${b.voltage_mv ? (b.voltage_mv / 1000).toFixed(2) + ' V · ' : ''}${esc(b.health || '')}</span>`];
}


// ---- Thermal guard: rate-limits LTE client traffic when the stick runs hot (thermal.go)
function tgStatus(g) {
  if (!g.enabled) return pill('off', '');
  if (g.cooldown) return `${pill('LTE cooldown', 'bad')} <span class="small muted">radio off since ${esc((g.cooldown_since || '').slice(11, 16))}</span>`;
  return g.level ? `${pill(g.level === 2 ? 'strict limit' : 'limiting', 'warn')} <span class="small muted">clients ${g.down_kbit}/${g.up_kbit} kbit/s since ${esc((g.since || '').slice(11, 16))}</span>`
    : `${pill('standby', 'ok')} <span class="small muted">${g.engaged ? `engaged ${g.engaged}× since start` : 'not needed so far'}</span>`;
}
async function drawThermalCard() {
  const el = $('#tg-card'); if (!el) return;
  const d = await api('GET', '/thermal'), c = d.config;
  const fields = [
    { key: 'enabled', label: 'Limit LTE client traffic when the stick runs hot', type: 'check' },
    { key: 'start_c', label: 'Start limiting at (°C, hottest sensor)', type: 'number', min: 60, max: 90, help: `At ${c.start_c + 5} °C the limit drops to a quarter. The stick powers itself off at 95 °C.` },
    { key: 'release_c', label: 'Lift the limit at (°C)', type: 'number', min: 40, max: 85 },
    { key: 'down_kbit', label: 'Client download limit (kbit/s)', type: 'number', min: 256, max: 100000 },
    { key: 'up_kbit', label: 'Client upload limit (kbit/s)', type: 'number', min: 128, max: 50000, help: 'Uploads make the radio transmit, which heats it most.' },
    { key: 'cooldown', label: 'Emergency cooldown: switch the LTE radio off when it gets too hot', type: 'check' },
    { key: 'cooldown_c', label: 'Emergency cooldown at (°C)', type: 'number', min: 61, max: 94, help: 'Internet and remote (Mesh) access are down during the cooldown; Wi-Fi and this page stay up. Better than the 95 °C hardware power-off, after which the stick restarts and heats up again.' },
    { key: 'cooldown_min', label: 'Cooldown duration (minutes)', type: 'number', min: 3, max: 60, help: 'LTE comes back after this time once the stick is at the release temperature, or after 3× this time at most.' },
  ];
  el.innerHTML = `<div class="card-head"><h3>Thermal guard</h3><div>${tgStatus(d.status)}</div></div>
    <p class="help">The LTE modem heats up with data volume (about +10 °C within a minute at 3 Mbit/s). While the stick is hot, traffic between Wi-Fi/USB clients and LTE is slowed so the modem cools down instead of the stick powering off. The router's own traffic (DNS, VPN tunnels, this page) is not limited. Now: ${(d.status.max_c || 0).toFixed(0)} °C.</p>
    ${form('tg-form', fields, c, 'Save thermal guard')}`;
  onSubmit('tg-form', fields, async x => { if (await act(() => api('PUT', '/thermal', x))) drawThermalCard(); });
}


// ---------------------------------------------------------------- data usage
const capOf = mb => mb > 0 ? mb * 1048576 : 0;
const used = p => (p.rx || 0) + (p.tx || 0);
function dataLine(d) {
  const t = used(d.today), m = used(d.period), dc = capOf(d.daily_mb), mc = capOf(d.monthly_mb);
  const st = d.state || {};
  const flag = st.level === 2 ? pill(st.enforcing && st.enforcing !== 'none' ? `${st.which} cap reached: ${st.enforcing === 'block' ? 'LAN blocked' : 'LAN slowed'}` : `${st.which} cap reached`, 'bad')
    : st.level === 1 ? pill(`${st.which} cap ${d.warn_pct}%`, 'warn') : '';
  return `${fmtBytes(t)}${dc ? ' / ' + fmtBytes(dc) : ''} today · ${fmtBytes(m)}${mc ? ' / ' + fmtBytes(mc) : ''} this period ${flag}`
    + (dc ? bar(pct(t, dc)) : mc ? bar(pct(m, mc)) : '');
}

async function pgData(v) {
  const d = await api('GET', '/data');
  const c = d.config, st = d.state || {};
  const t = used(d.today), m = used(d.period), dc = capOf(c.daily_mb), mc = capOf(c.monthly_mb);
  const days = d.days || [], peak = Math.max(1, ...days.map(used), dc || 0);
  const today = days.length ? days[days.length - 1].date : '';
  const fields = [
    { key: 'daily_mb', label: 'Daily cap (MB, 0 = none)', type: 'number', min: 0, help: 'e.g. 1536 for a 1.5 GB/day plan' },
    { key: 'monthly_mb', label: 'Monthly cap (MB, 0 = none)', type: 'number', min: 0 },
    { key: 'cycle_day', label: 'Monthly period starts on day', type: 'number', min: 1, max: 28 },
    { key: 'warn_pct', label: 'Warn at (% of a cap)', type: 'number', min: 50, max: 99 },
    { key: 'action', label: 'At the cap', type: 'select', options: [['none', 'Only warn'], ['limit', 'Slow the LAN down'], ['block', 'Cut the LAN off from LTE']] },
    { key: 'limit_kbit', label: 'Slowed speed (kbit/s, each direction)', type: 'number', min: 64, max: 100000 },
  ];
  v.innerHTML = `<div class="grid two">
    ${card('LTE data used', kv([
      ['Today', `<span class="stat">${fmtBytes(t)}</span>${dc ? ` <span class="muted">of ${fmtBytes(dc)}</span>${bar(pct(t, dc))}` : ''}<div class="small muted">↓ ${fmtBytes(d.today.rx)} · ↑ ${fmtBytes(d.today.tx)}</div>`],
      ['This period', `<span class="stat">${fmtBytes(m)}</span>${mc ? ` <span class="muted">of ${fmtBytes(mc)}</span>${bar(pct(m, mc))}` : ''}<div class="small muted">${esc(d.period_start)} – ${esc(d.period_end)} · ↓ ${fmtBytes(d.period.rx)} · ↑ ${fmtBytes(d.period.tx)}</div>`],
      ['State', st.level === 2 ? pill(`${st.which} cap reached` + (st.enforcing === 'block' ? ': LAN cut off from LTE' : st.enforcing === 'limit' ? ': LAN slowed down' : ''), 'bad')
        : st.level === 1 ? pill(`over ${c.warn_pct}% of the ${st.which} cap`, 'warn') : pill('within limits', 'ok')],
    ]) + (st.counters === false ? '<div class="notice">The LTE byte counters are not in the firewall yet; they appear after the next firewall reload.</div>' : '')
      + (!d.clock_ok ? '<div class="notice">The clock is not set yet (no network time); usage is held back until it is.</div>' : '')
      + `<p class="help">Counts everything that crosses the LTE radio, the router's own traffic included (as the operator does), updated every minute. The operator's count can differ a little. Restrictions apply to Wi-Fi/LAN clients only; the router itself, Mesh and SMS remote control keep working.</p>`)}
    ${card('Caps & warnings', form('data-form', fields, c, 'Save') + '<p class="help">Warnings and the cap are logged and, with SMS remote control notifications on, sent by SMS once per day or period.</p>')}
  </div>
  ${card('Last 35 days', `<div class="days">${days.map(x => `<i class="${dc && used(x) >= dc ? 'bad' : ''} ${x.date === today ? 'today' : ''}" style="height:${Math.max(0.5, 100 * used(x) / peak)}%" title="${esc(x.date)}: ${fmtBytes(used(x))} (↓ ${fmtBytes(x.rx)} ↑ ${fmtBytes(x.tx)})"></i>`).join('')}</div>
    <div class="days-axis"><span>${esc(days[0]?.date || '')}</span><span>today</span></div>`)}`;
  onSubmit('data-form', fields, async x => { if (await act(() => api('PUT', '/data', x))) setTimeout(route, 1200); });
}

// ---------------------------------------------------------------- SMS remote control
async function pgRemote(v) {
  const d = await api('GET', '/smsctl');
  const fields = [
    { key: 'enabled', label: 'Accept commands by SMS', type: 'check' },
    { key: 'numbers', label: 'Allowed phone numbers (one per line)', type: 'textarea', list: true, placeholder: '9876543210', help: '10 digits for Indian numbers, or the full international number (digits only). Up to 5.' },
    { key: 'pin', label: d.pin_set ? 'PIN (leave empty to keep the current one)' : 'PIN (4-12 digits)', type: 'password', placeholder: d.pin_set ? 'unchanged' : '' },
    { key: 'notify', label: 'Also send notifications to these numbers (data cap warnings)', type: 'check' },
  ];
  v.innerHTML = `<div class="grid two">
    ${card('SMS remote control', `<p class="muted">Control the router by SMS when Mesh or mobile data is down. Send <b class="mono">&lt;PIN&gt; &lt;COMMAND&gt;</b>, e.g. <b class="mono">4821 STATUS</b>; the reply comes back by SMS (each reply costs one SMS).</p>`
      + form('ctl-form', fields, { ...d, numbers: d.numbers || [], pin: '' }, 'Save')
      + '<p class="help">Only the numbers above are answered. Three wrong PINs lock a number out for an hour; at most 10 replies per number per hour; commands older than 15 minutes are ignored. Command SMS are stored with the PIN hidden and never forwarded to WhatsApp.</p>')}
    ${card('Commands', `<table><tbody>${(d.commands || []).map(c => `<tr><td class="mono">${esc(c.command)}</td><td class="muted">${esc(c.what)}</td></tr>`).join('')}</tbody></table>`)}
  </div>
  ${card('Recent commands', (d.log || []).length ? `<div class="table-wrap"><table><thead><tr><th>Time</th><th>From</th><th>Command</th><th>Result</th></tr></thead><tbody>
    ${d.log.slice().reverse().map(l => `<tr><td class="small">${esc(fmtTS(l.time))}</td><td class="mono">${esc(l.from)}</td><td class="mono">${esc(l.command)}</td><td>${esc(l.result)}</td></tr>`).join('')}</tbody></table></div>`
    : '<p class="muted">No commands since the web UI started.</p>')}`;
  onSubmit('ctl-form', fields, async x => { if (await act(() => api('PUT', '/smsctl', x))) route(); });
}

// ---------------------------------------------------------------- watchdog (System page)
function sysWatchdogCard(w) {
  if (!w || !w.present) return '';
  const on = w.state === 'active';
  return card('Hardware watchdog', kv([
    ['State', on ? pill(`armed · resets after ${w.timeout_s} s without a sign of life`, 'ok') : pill('not armed', 'warn')],
    ['Device', esc(w.identity)],
    w.boot_reason ? ['This boot', (w.watchdog_reset ? pill('the watchdog reset the stick', 'bad') + ' ' : '') + `<span class="mono small">${esc(w.boot_reason)}</span>`] : null,
  ]) + '<p class="help">systemd keeps the PMIC watchdog fed; if the kernel or systemd hangs, the stick resets by itself instead of staying frozen.</p>');
}

// ---------------------------------------------------------------- history charts
function drawSeries(canvas, t, series, opt) {
  const dpr = window.devicePixelRatio || 1, w = canvas.clientWidth, h = canvas.clientHeight;
  canvas.width = w * dpr; canvas.height = h * dpr;
  const c = canvas.getContext('2d'); c.scale(dpr, dpr); c.clearRect(0, 0, w, h);
  const vals = series.flatMap(s => s.v.filter(x => x != null));
  if (!t.length || !vals.length) { c.fillStyle = '#555'; c.font = '12px Tahoma, Verdana, sans-serif'; c.fillText('no data yet', 8, h / 2); return; }
  let lo = opt.min ?? Math.min(...vals), hi = opt.max ?? Math.max(...vals);
  if (opt.min == null) lo = Math.min(lo, ...(opt.floor != null ? [opt.floor] : []));
  if (hi - lo < (opt.span || 1)) { const m = (hi + lo) / 2; lo = m - (opt.span || 1) / 2; hi = m + (opt.span || 1) / 2; }
  const pad = (hi - lo) * 0.08; if (opt.min == null) lo -= pad; if (opt.max == null) hi += pad;
  const t0 = t[0], t1 = Math.max(t[t.length - 1], t0 + 1), L = 46, B = 16;
  const X = x => L + (w - L - 4) * (x - t0) / (t1 - t0), Y = v => h - B - (h - B - 6) * (v - lo) / (hi - lo);
  c.strokeStyle = '#dcdad5'; c.fillStyle = '#555'; c.font = '11px Tahoma, Verdana, sans-serif'; c.lineWidth = 1;
  for (let i = 0; i <= 3; i++) { const v = lo + (hi - lo) * i / 3, y = Y(v); c.beginPath(); c.moveTo(L, y); c.lineTo(w, y); c.stroke(); c.fillText(opt.fmt(v), 2, y + 4); }
  const span = t1 - t0, step = span > 2 * 86400 ? 86400 : span > 6 * 3600 ? 4 * 3600 : 3600;
  for (let x = Math.ceil(t0 / step) * step; x <= t1; x += step) {
    const d = new Date(x * 1000), lab = step >= 86400 ? d.toLocaleDateString([], { weekday: 'short' }) : d.toTimeString().slice(0, 5);
    c.fillText(lab, Math.min(X(x) - 12, w - 34), h - 3);
  }
  const gap = (opt.step || 60) * 3;
  for (const s of series) {
    c.strokeStyle = s.color; c.lineWidth = 1.6; c.beginPath();
    let pen = false;
    s.v.forEach((v, i) => {
      if (v == null || (i && t[i] - t[i - 1] > gap)) { pen = false; if (v == null) return; }
      pen ? c.lineTo(X(t[i]), Y(v)) : c.moveTo(X(t[i]), Y(v)); pen = true;
    });
    c.stroke();
  }
}

let HIST_RANGE = '24h';
async function pgHistory(v) {
  const d = await api('GET', '/history?range=' + HIST_RANGE);
  const F = d.fields, col = k => d.points.map(p => p[F.indexOf(k)]);
  const t = col('t'), step = d.step;
  const perSec = k => col(k).map(x => x == null ? null : x / step); // bytes per slot -> bytes/s (fmtRate shows bit/s)
  const legend = items => `<div class="legend">${items.map(([c, l]) => `<span><i style="background:${c}"></i>${esc(l)}</span>`).join('')}</div>`;
  const charts = [
    ['h-temp', 'Temperature & CPU load', [{ v: col('temp'), color: '#ff8a3d' }, { v: col('cpu'), color: '#9b7bff' }], { fmt: x => x.toFixed(0), span: 10 }, [['#ff8a3d', 'hottest sensor °C'], ['#9b7bff', 'CPU %']]],
    ['h-rsrp', 'Signal: RSRP (dBm)', [{ v: col('rsrp'), color: '#3fa7ff' }], { fmt: x => x.toFixed(0), span: 10 }, [['#3fa7ff', 'RSRP: above -90 good, below -100 poor']]],
    ['h-snr', 'Signal: SINR & RSRQ (dB)', [{ v: col('snr'), color: '#37c978' }, { v: col('rsrq'), color: '#e0b84a' }], { fmt: x => x.toFixed(0), span: 10 }, [['#37c978', 'SINR: above 13 good, below 0 poor'], ['#e0b84a', 'RSRQ']]],
    ['h-lte', 'LTE traffic (average per ' + (step === 60 ? 'minute' : '5 minutes') + ')', [{ v: perSec('rx'), color: '#3fa7ff' }, { v: perSec('tx'), color: '#37c978' }], { fmt: fmtRate, min: 0, span: 8000 }, [['#3fa7ff', 'download'], ['#37c978', 'upload']]],
  ];
  if (col('battery').some(x => x != null)) charts.push(['h-bat', 'Battery (%)', [{ v: col('battery'), color: '#e0b84a' }], { fmt: x => x.toFixed(0), min: 0, max: 100 }, [['#e0b84a', 'charge']]]);
  const tabs = `<span class="btns" style="margin:0">${['24h', '7d'].map(r => `<button class="btn sm ${r === HIST_RANGE ? 'primary' : ''}" data-range="${r}">${r === '24h' ? '24 hours' : '7 days'}</button>`).join('')}</span>`;
  v.innerHTML = `<div class="notice">One sample a minute (7 days: 5-minute averages). Saved every 30 minutes and before a reboot; nothing is recorded until the clock is set after boot.</div>
    <div class="grid two">${charts.map(([id, title, , , leg]) => card(title, `<canvas class="chart" id="${id}"></canvas>${legend(leg)}`, id === 'h-temp' ? tabs : '')).join('')}</div>`;
  const draw = () => charts.forEach(([id, , series, opt]) => { const el = $('#' + id); if (el) drawSeries(el, t, series, { ...opt, step }); });
  draw();
  v.querySelectorAll('[data-range]').forEach(b => b.addEventListener('click', () => { HIST_RANGE = b.dataset.range; route(); }));
  every(60000, async () => { if (HIST_RANGE === '24h') route(); });
}

// ---------------------------------------------------------------- signal & band lock
const QCOL = { excellent: 'ok', good: 'ok', fair: 'warn', poor: 'bad' };
async function pgSignal(v) {
  let d = await api('GET', '/signal');
  const trail = [];
  const sup = d.supported_bands || [], cfgB = new Set(d.config.bands || []);
  const bandName = b => 'B' + b.replace('eutran-', '');
  v.innerHTML = `<div class="grid two">
    ${card('Live signal', `<div id="sig-live"></div><canvas class="chart" id="sig-chart" style="height:140px"></canvas>
      <div class="legend"><span><i style="background:#3fa7ff"></i>RSRP, last 3 minutes</span><span><i style="background:#37c978"></i>SINR</span></div>
      <p class="help">While this page is open the modem measures every 2 s. Move the stick a little at a time and wait a few seconds after each move: RSRP is how strong the signal is, SINR how clean. Near a window and up high usually helps; a weak signal is the biggest cause of heat. The LCD has the same readout as its last screen.</p>`)}
    ${card('LTE bands', d.bands_known ? `<p class="muted">Currently allowed: <b>${esc((d.current_bands || []).map(bandName).join(', ') || '—')}</b>${cfgB.size ? ' ' + pill('locked', 'warn') : ''}</p>
      <form id="band-form"><div class="band-grid">${sup.map(b => `<label class="check"><input type="checkbox" name="${esc(b)}" ${cfgB.has(b) ? 'checked' : ''}> ${esc(bandName(b))}</label>`).join('')}</div>
      <div class="btns"><button class="btn primary">Lock to the ticked bands</button><button type="button" class="btn" id="band-all">Allow all bands</button></div></form>
      <p class="help">Locking to the operator's best band here can mean a cleaner signal or less crowding. If the stick loses service after a change, it is undone automatically unless you confirm it (yellow bar). Common in India: B3 (1800), B40 (2300), B5/B8 (850/900, longer reach), B1, B41.</p>`
      : '<p class="muted">No modem information yet.</p>')}
  </div>
  <div class="grid two">
    ${card('Cell tower', '<div id="cell-info"><p class="muted">Reading cell info…</p></div>')}
    ${card('Tower position lookup', '<div id="cell-key"></div>')}
  </div>`;
  cellKeyDrawn = '';
  loadCell();
  every(10000, loadCell);
  const live = () => {
    const m = d.modem || {}, q = d.quality || {};
    const big = (label, val, unit, k) => `<div class="sig-big"><div class="small muted">${label}</div><div class="stat">${val !== '' && val != null ? esc(val) : '—'} <span class="small muted">${unit}</span></div>${q[k] ? pill(q[k], QCOL[q[k]]) : ''}</div>`;
    $('#sig-live').innerHTML = m.present ? `<div class="sig-row">${big('RSRP', m.rsrp, 'dBm', 'rsrp')}${big('SINR', m.snr, 'dB', 'snr')}${big('RSRQ', m.rsrq, 'dB', 'rsrq')}${big('Bars', m.signal, '%', '')}</div>
      <p class="small muted">${esc(m.operator || '')} · ${esc((m.tech || '').toUpperCase())} · RSSI ${esc(m.rssi || '—')} dBm</p>` : '<p class="muted">No modem.</p>';
    const now = Date.now() / 1000;
    if (m.rsrp !== '' && m.rsrp != null) trail.push([now, Number(m.rsrp), m.snr === '' ? null : Number(m.snr)]);
    while (trail.length && trail[0][0] < now - 180) trail.shift();
    const el = $('#sig-chart');
    if (el) {
      const t = trail.map(p => p[0]), rs = trail.map(p => p[1]), sn = trail.map(p => p[2]);
      // both on one axis: RSRP (dBm) and SINR (dB) scaled into RSRP's range would mislead, so SINR is offset to -120 + SINR*2
      drawSeries(el, t, [{ v: rs, color: '#3fa7ff' }, { v: sn.map(x => x == null ? null : -120 + x * 2), color: '#37c978' }], { fmt: x => x.toFixed(0), span: 20, step: 2 });
    }
  };
  live();
  every(2000, async () => { d = await api('GET', '/signal'); live(); });
  const send = async bands => { if (await act(() => api('PUT', '/radio', { bands }))) setTimeout(route, 3000); };
  onSubmit('band-form', [], async (_, f) => {
    const bands = sup.filter(b => f.querySelector(`[name="${b}"]`).checked);
    if (!bands.length) return toast('Tick at least one band, or use "Allow all bands".', true);
    await send(bands);
  });
  $('#band-all')?.addEventListener('click', () => send([]));
}

// Cell tower card (Signal & bands): serving cell from the modem, tower position from OpenCelliD / BeaconDB.
let cellKeyDrawn = '';
async function loadCell() {
  const d = await api('GET', '/celltower');
  const c = d.cell, p = d.position;
  let pos = '—';
  if (p && p.found) {
    const ll = `${Number(p.lat).toFixed(5)}, ${Number(p.lon).toFixed(5)}`;
    pos = `<a class="mono" href="https://www.openstreetmap.org/?mlat=${p.lat}&mlon=${p.lon}#map=15/${p.lat}/${p.lon}" target="_blank" rel="noopener noreferrer">${esc(ll)}</a>`
      + `<div class="small muted">±${esc(p.range_m)} m · ${esc(p.source)}</div>`;
  } else if (d.looking) pos = '<span class="muted">looking up…</span>';
  else if (p && p.error) pos = `${pill('lookup failed', 'bad')}<div class="small muted">${esc(p.error)}</div>`;
  else if (p) pos = `<span class="muted">not in ${d.key_set ? 'OpenCelliD or BeaconDB' : 'BeaconDB'}</span>`;
  const el = $('#cell-info');
  if (!el) return;
  el.innerHTML = c ? kv([
    ['Tower (eNB-sector)', c.enb != null ? `<b>${esc(c.enb)}-${esc(c.sector)}</b>` : '—'],
    ['Position', pos],
    ['Distance', c.distance_m >= 0 ? `~${c.distance_m >= 1000 ? (c.distance_m / 1000).toFixed(1) + ' km' : esc(c.distance_m) + ' m'} <span class="small muted">(timing advance ${esc(c.timing_advance_us)} µs)</span>` : '—'],
    ['Band', c.band ? `B${esc(c.band)} <span class="small muted">· ${esc(c.freq_mhz)} MHz · EARFCN ${esc(c.earfcn)}</span>` : esc(c.earfcn >= 0 ? 'EARFCN ' + c.earfcn : '—')],
    ['Operator · TAC · cell ID', `<span class="mono">${esc(c.operator_id)} · ${esc(c.tac)} · ${esc(c.ci)}</span>`],
    ['PCI', esc(c.pci >= 0 ? c.pci : '—')],
    ['Neighbour cells heard', esc(d.neighbours)],
  ]) + '<p class="help">Distance is estimated from the LTE timing advance (round-trip time). It is accurate to roughly ±80 m and assumes a direct path.</p>'
    : '<p class="muted">No serving LTE cell.</p>';
  // the key form is redrawn only when the saved state changes, so typing isn't interrupted by the 10 s refresh
  const st = `${d.key_set}${d.key_hint}`;
  if (st === cellKeyDrawn) return;
  cellKeyDrawn = st;
  const fields = [{ key: 'opencellid_key', label: 'OpenCelliD API key', type: 'password',
    placeholder: d.key_set ? `saved (${d.key_hint}) — leave empty to keep` : 'pk.…',
    help: 'Free from opencellid.org (sign up, then copy the access token). Stored on the router only (0600) and never shown again.' }];
  $('#cell-key').innerHTML = `<p class="muted">Status: ${d.key_set ? pill('OpenCelliD + BeaconDB', 'ok') : pill('BeaconDB only', 'warn')}</p>`
    + form('cell-key-form', fields, {}, 'Save', d.key_set ? '<button type="button" class="btn" id="cell-test">Test key</button><button type="button" class="btn danger" id="cell-clear">Remove key</button>' : '')
    + '<pre id="cell-test-out" class="hidden"></pre>'
    + '<p class="help">To find the tower\'s position, the serving cell\'s IDs (operator, TAC, cell ID) are sent to OpenCelliD (if a key is saved) and then <a href="https://beacondb.net" target="_blank" rel="noopener noreferrer">BeaconDB</a>. That reveals roughly where this device is to those services. It happens once per new cell, while this page or the LCD\'s cell tower screen is open.</p>';
  const redraw = () => { cellKeyDrawn = ''; loadCell(); };
  onSubmit('cell-key-form', fields, async (x, f) => {
    if (!x.opencellid_key) return toast('Paste a key first.', true);
    if (await act(() => api('PUT', '/celltower', x))) { f.reset(); redraw(); }
  });
  $('#cell-clear')?.addEventListener('click', async () => { if (await act(() => api('PUT', '/celltower', { clear: true }))) redraw(); });
  $('#cell-test')?.addEventListener('click', async () => {
    const out = $('#cell-test-out'); out.classList.remove('hidden'); out.textContent = 'Asking OpenCelliD…';
    try {
      const r = await api('POST', '/celltower/test');
      out.textContent = !r.ok ? `✗ ${r.error}` : r.found ? `✓ Key works. Tower at ${Number(r.lat).toFixed(5)}, ${Number(r.lon).toFixed(5)} (±${r.range_m} m)` : `✓ ${r.message}`;
      if (r.found) loadCell();
    } catch (e) { out.textContent = e.message; }
  });
}

// ---------------------------------------------------------------- schedules
const DAYS = ['sun', 'mon', 'tue', 'wed', 'thu', 'fri', 'sat'];
function fmtDays(ds) {
  if (!ds || !ds.length || ds.length === 7) return 'daily';
  const s = [...ds].sort();
  if (s.join() === '1,2,3,4,5') return 'mon-fri';
  if (s.join() === '0,6') return 'sat,sun';
  return s.map(d => DAYS[d]).join(',');
}
function parseDays(s) {
  s = s.toLowerCase();
  if (s === 'daily' || s === '') return [];
  const out = new Set();
  for (const part of s.split(',')) {
    const [a, b] = part.split('-').map(x => DAYS.indexOf(x.trim().slice(0, 3)));
    if (a < 0 || (part.includes('-') && b < 0)) throw new Error(`unknown day "${part}" (use mon, tue, … or ranges like mon-fri)`);
    if (!part.includes('-')) { out.add(a); continue; }
    for (let i = a; ; i = (i + 1) % 7) { out.add(i); if (i === b) break; }
  }
  return [...out].sort();
}
const devLine = d => `${d.mac}  ${fmtDays(d.days)}  ${d.from}-${d.to}${d.name ? '  ' + d.name : ''}`;
function parseDevLines(text) {
  return text.split('\n').map(l => l.trim()).filter(l => l && !l.startsWith('#')).map((l, i) => {
    const m = l.match(/^([0-9a-fA-F:]{17})\s+(\S+)\s+(\d\d:\d\d)-(\d\d:\d\d)(?:\s+(.*))?$/);
    if (!m) throw new Error(`line ${i + 1}: expected "MAC days HH:MM-HH:MM name"`);
    return { mac: m[1].toLowerCase(), days: parseDays(m[2]), from: m[3], to: m[4], name: (m[5] || '').trim() };
  });
}

async function pgSchedule(v) {
  const [d, clients] = await Promise.all([api('GET', '/schedule'), api('GET', '/clients').catch(() => [])]);
  const c = d.config, st = d.state || {};
  const win = (k, label, w) => [
    { key: k + '_on', label, type: 'check' },
    { key: k + '_from', label: 'From (HH:MM)', placeholder: '01:00', pattern: '\\d\\d:\\d\\d' },
    { key: k + '_to', label: 'Until (HH:MM)', placeholder: '06:00', pattern: '\\d\\d:\\d\\d' },
  ];
  const client = MODE === 'client'; // no hotspot or LAN clients: Wi-Fi off would cut the uplink
  const fields = [
    ...win('lte', 'Switch LTE off every night', c.lte_off),
    ...(client ? [] : win('wifi', 'Switch Wi-Fi off every day', c.wifi_off)),
    { key: 'reboot_on', label: 'Reboot regularly', type: 'check' },
    { key: 'reboot_day', label: 'On', type: 'select', options: [[7, 'every day'], [0, 'Sunday'], [1, 'Monday'], [2, 'Tuesday'], [3, 'Wednesday'], [4, 'Thursday'], [5, 'Friday'], [6, 'Saturday']] },
    { key: 'reboot_at', label: 'At (HH:MM)', placeholder: '04:30', pattern: '\\d\\d:\\d\\d' },
    client ? null : { key: 'devices', label: 'Device internet times: no internet in these windows (one device per line)', type: 'textarea', placeholder: 'aa:bb:cc:dd:ee:ff  mon-fri  22:00-07:00  Tablet',
      help: 'MAC, days the window starts on (daily, mon-fri, sat,sun, …), time window, optional name. The device keeps Wi-Fi and the LAN.' },
  ].filter(Boolean);
  const vals = {
    lte_on: c.lte_off.enabled, lte_from: c.lte_off.from, lte_to: c.lte_off.to,
    wifi_on: c.wifi_off.enabled, wifi_from: c.wifi_off.from, wifi_to: c.wifi_off.to,
    reboot_on: c.reboot.enabled, reboot_day: c.reboot.day ?? 0, reboot_at: c.reboot.at,
    devices: (c.devices || []).map(devLine).join('\n'),
  };
  const now = [st.lte_off ? pill('LTE off now', 'warn') : '', st.wifi_off ? pill('Wi-Fi off now', 'warn') : '',
    ...(st.devices_blocked || []).map(n => pill('no internet: ' + n, 'warn'))].join(' ');
  v.innerHTML = `<div class="grid two">
    ${card('Schedules', (st.clock_ok === false ? '<div class="notice">The clock is not set yet; schedules start once it is.</div>' : '') + (now ? `<p>${now}</p>` : '')
      + form('sched-form', fields, vals, 'Save')
      + `<p class="help">LTE off is the coolest option (the radio is off), but in that window there is no mobile data, no SMS (so no SMS remote control) and no Mesh over LTE. LTE off and Wi-Fi off may not overlap. A reboot never runs within an hour of the last boot. Times are in the router's timezone (System page).</p>`)}
    ${client ? '' : card('Known devices', clients.length ? `<table><thead><tr><th>Name</th><th>MAC</th></tr></thead><tbody>${clients.map(x => `<tr><td>${esc(x.hostname || x.name || '—')}</td><td class="mono small">${esc(x.mac)}</td></tr>`).join('')}</tbody></table>` : '<p class="muted">No clients seen.</p>')}
  </div>`;
  onSubmit('sched-form', fields, async x => {
    let devices;
    try { devices = client ? (c.devices || []) : parseDevLines(x.devices); } catch (e) { return toast(e.message, true); }
    const body = {
      lte_off: { enabled: x.lte_on, from: x.lte_from, to: x.lte_to },
      wifi_off: client ? { ...c.wifi_off, enabled: false } : { enabled: x.wifi_on, from: x.wifi_from, to: x.wifi_to },
      reboot: { enabled: x.reboot_on, day: Number(x.reboot_day), at: x.reboot_at },
      devices,
    };
    if (await act(() => api('PUT', '/schedule', body))) setTimeout(route, 2500);
  });
}

// ---------------------------------------------------------------- Bluetooth
const BT_ICON = { 'audio-headset': '🎧', 'audio-headphones': '🎧', 'audio-card': '🔊', phone: '📱', computer: '💻', 'input-keyboard': '⌨', 'input-mouse': '🖱', 'input-gaming': '🎮' };
async function pgBluetooth(v) {
  let d = await api('GET', '/bt');
  let showAll = false;
  const btAct = async (action, mac, busyText) => {
    if (busyText) toast(busyText);
    if (await act(() => api('POST', '/bt/' + action, mac ? { mac } : {}))) { d = await api('GET', '/bt'); draw(); }
  };
  const draw = () => {
    const a = d.adapter || {}, devs = d.devices || [];
    const paired = devs.filter(x => x.paired), near = devs.filter(x => !x.paired && (showAll || x.name));
    const hidden = devs.filter(x => !x.paired && !x.name).length;
    const name = x => `${BT_ICON[x.icon] || ''} ${esc(x.name || x.mac)}${x.name ? `<div class="mono small muted">${esc(x.mac)}</div>` : ''}`;
    const btn = (label, action, mac, cls = '') => `<button class="btn sm ${cls}" data-bt="${action}" data-mac="${esc(mac || '')}">${esc(label)}</button>`;
    const onOff = `<label class="check"><input type="checkbox" id="bt-enabled" ${d.enabled ? 'checked' : ''}> Bluetooth on</label>`;
    if (!d.enabled) {
      v.innerHTML = card('Bluetooth', onOff + `<p class="muted">Bluetooth is off: the driver is not loaded, which saves about 5 MB of RAM. Turning it on loads the driver of the Wi-Fi chip's Bluetooth part and starts BlueZ${d.audio ? ' and bluez-alsa (Bluetooth audio)' : ''}; Wi-Fi keeps working. The setting survives reboots.</p>`);
    } else {
      v.innerHTML = `<div class="grid two">
        ${card('Bluetooth', onOff + (d.error ? `<div class="notice">${esc(d.error)}</div>` : !d.adapter ? '<div class="notice">Waiting for the adapter…</div>' : kv([
          ['Adapter', `${esc(a.alias)} <span class="mono small muted">${esc(a.address)}</span>`],
          ['Radio', `${a.powered ? pill('on', 'ok') : pill('off', 'warn')} ${btn(a.powered ? 'Switch off' : 'Switch on', a.powered ? 'power-off' : 'power-on')}`],
          ['Visible to others', d.pairing_left ? pill(`pairing open, ${d.pairing_left} s left`, 'warn') : `${pill(a.discoverable ? 'yes' : 'no')} ${btn('Allow pairing (3 min)', 'pairing')}`],
        ])) + '<p class="help">To pair a phone or headset: press <b>Allow pairing</b> and pair from the device, or put the device in pairing mode, <b>Scan</b> and press <b>Pair</b>. Pairing is "just works" (no PIN); paired devices are trusted.</p>')}
        ${card('Paired devices', paired.length ? `<table><tbody>${paired.map(x => `<tr><td>${name(x)}</td><td>${x.connected ? pill('connected', 'ok') : pill('not connected')}${x.audio ? ' ' + pill('audio') : ''}</td>
          <td class="btns" style="margin:0">${x.connected ? btn('Disconnect', 'disconnect', x.mac) : btn('Connect', 'connect', x.mac)}${btn('Remove', 'remove', x.mac, 'danger')}</td></tr>`).join('')}</tbody></table>` : '<p class="muted">No paired devices.</p>')}
      </div>
      ${card('Nearby devices', `<div class="btns" style="margin-top:0">${btn(a.discovering ? 'Scanning…' : 'Scan (12 s)', 'scan', '', 'primary')}
          <label class="check" style="margin-left:8px"><input type="checkbox" id="bt-all" ${showAll ? 'checked' : ''}> show unnamed (${hidden})</label></div>
        ${near.length ? `<table><thead><tr><th>Device</th><th>Signal</th><th></th></tr></thead><tbody>${near.map(x => `<tr><td>${name(x)}</td><td>${x.rssi != null ? esc(x.rssi) + ' dBm' : '—'}</td><td>${btn('Pair', 'pair', x.mac)}</td></tr>`).join('')}</tbody></table>`
          : '<p class="muted">Nothing seen yet: put the device in pairing mode and scan.</p>'}`)}
      ${btUplinkCard(d, paired)}`;
    }
    $('#bt-enabled').onchange = async e => {
      e.target.disabled = true;
      toast(e.target.checked ? 'Turning Bluetooth on…' : 'Turning Bluetooth off…');
      if (await act(() => api('PUT', '/bt', { enabled: e.target.checked }))) setTimeout(route, 2500); else route();
    };
    const all = $('#bt-all'); if (all) all.onchange = e => { showAll = e.target.checked; draw(); };
    onSubmit('btup-form', btUpFields(paired, d), async x => {
      if (await act(() => api('PUT', '/bt/uplink', { device: x.device, when_lte_off: x.when_lte_off }))) { d = await api('GET', '/bt'); draw(); }
    });
    v.querySelectorAll('[data-bt]').forEach(b => b.onclick = async () => {
      const a = b.dataset.bt, mac = b.dataset.mac;
      if (a === 'remove' && !(await confirmBox('Remove device', `Forget <b>${esc(mac)}</b>? It has to be paired again.`, 'Remove'))) return;
      v.querySelectorAll('[data-bt]').forEach(x => x.disabled = true);
      await btAct(a, mac, { scan: 'Scanning for 12 s…', pair: 'Pairing… (up to 30 s)', connect: 'Connecting…' }[a]);
    });
  };
  draw();
  every(5000, async () => { if (!v.querySelector('[data-bt]:disabled') && !v.querySelector('#btup-form :focus')) { d = await api('GET', '/bt'); draw(); } });
}

// Bluetooth backup uplink: a paired phone with Bluetooth tethering, used only while no other uplink works
const btUpFields = (paired, d) => [
  { key: 'device', label: 'Phone', type: 'select', options: [['', 'None (no backup)'], ...paired.map(x => [x.mac, `${x.name || x.mac}`]),
    ...(d.uplink && !paired.some(x => x.mac === d.uplink) ? [[d.uplink, d.uplink + ' (not paired)']] : [])] },
  { key: 'when_lte_off', label: 'Also while LTE is off on purpose (too hot, or the LTE off schedule)', type: 'check' },
];
function btUplinkCard(d, paired) {
  const u = d.uplink_state || {};
  const state = !d.uplink ? '' : u.connected
    ? `${pill(u.active ? 'in use: internet through the phone' : 'connected', 'warn')} <span class="mono small">${esc(u.ip || '')}</span>
       <div class="small muted">since ${esc(fmtTS(u.since))} · ↓ ${fmtBytes(u.rx)} ↑ ${fmtBytes(u.tx)} of phone data</div>`
    : pill(u.needed ? 'needed: connecting…' : 'standby (LTE is working)', u.needed ? 'warn' : 'ok') + (u.error ? `<div class="small muted">last try: ${esc(u.error)}</div>` : '');
  return card('Backup internet through a phone', (state ? `<p>${state}</p>` : '')
    + form('btup-form', btUpFields(paired, d), { device: d.uplink || '', when_lte_off: d.uplink_when_lte_off }, 'Save')
    + `<p class="help">When LTE (and the USB uplink) has been down for 45 s, the stick connects to this phone over Bluetooth and uses its mobile data, so Mesh and the web UI stay reachable. It disconnects once LTE has worked again for 3 minutes. On the phone: pair it here first, then turn on <b>Bluetooth tethering</b> (Android: Hotspot &amp; tethering; iPhone: Personal Hotspot) and keep it within ~10 m. Expect 1-2 Mbit/s. Phone data is not counted in the LTE data usage.</p>`);
}

// ---------------------------------------------------------------- Operating mode (router / Wi-Fi client)
const wifiSignal = d => d == null ? '—' : `${esc(d)} dBm ${d >= -55 ? pill('excellent', 'ok') : d >= -67 ? pill('good', 'ok') : d >= -75 ? pill('fair', 'warn') : pill('weak', 'bad')}`;

async function pgMode(v) {
  const d = await api('GET', '/mode');
  if (d.mode !== MODE) { MODE = d.mode; buildNav(); }
  const netFields = [
    { key: 'ssid', label: 'Network name (SSID)', required: true },
    { key: 'psk', label: 'Password', type: 'password', placeholder: d.client.has_psk ? 'unchanged' : '', help: d.client.has_psk ? 'Leave empty to keep the saved password (same network only).' : '' },
    { key: 'open', label: 'Open network (no password)', type: 'check' },
    { key: 'lan_admin', label: 'Allow SSH and this web UI from that network', type: 'check', help: 'Off: only Cloudflare Mesh and the USB link reach this device.' },
  ];
  const vals = { ssid: d.client.ssid, lan_admin: d.mode === 'client' ? d.client.lan_admin : true };
  const send = async (body, title, html) => {
    if (!await confirmBox(title, html, 'Switch')) return;
    if (await act(() => api('PUT', '/mode', body))) { MODE = body.mode; buildNav(); setTimeout(route, 3000); }
  };
  const scanUI = () => {
    $('#scan-btn').onclick = async () => {
      const el = $('#scan');
      el.innerHTML = '<p class="muted">Scanning…</p>';
      try {
        const nets = await api('GET', '/wifi/scan');
        el.innerHTML = nets.length ? `<table><thead><tr><th>Network</th><th>Signal</th><th>Band</th><th>Security</th></tr></thead><tbody>${nets.map(n =>
          `<tr class="clickable" data-ssid="${esc(n.ssid)}"><td>${esc(n.ssid)} ${n.in_use ? pill('connected', 'ok') : ''}</td><td>${signalBars(n.signal)}${esc(n.signal)}%</td><td>${n.freq_mhz > 3000 ? '5' : '2.4'} GHz</td><td class="small">${esc(n.security || 'open')}</td></tr>`).join('')}</tbody></table>
          <p class="help">Click a network to fill in its name.</p>` : '<p class="muted">No networks found.</p>';
        $$('tr[data-ssid]', el).forEach(tr => tr.onclick = () => { $('#f_ssid', v).value = tr.dataset.ssid; $('#f_psk', v).focus(); });
      } catch (e) { el.innerHTML = `<p class="bad">${esc(e.message)}</p>`; }
    };
  };
  const cli = '<p class="help">From a shell (ssh over USB or Mesh): <code>sudo router-webui -set-mode router</code> (or <code>client</code>) switches at once, without the confirm step.</p>';
  if (d.mode === 'client') {
    const l = d.link || {};
    v.innerHTML = `<div class="grid two">
      ${card('Wi-Fi client mode', kv([
        ['Mode', pill('Wi-Fi client', 'ok') + ' <span class="small muted">no hotspot, DHCP or LAN</span>'],
        ['Joined network', l.connected ? `${esc(l.ssid)} ${pill('connected', 'ok')}` : pill('not connected', 'bad') + (l.connection ? ` <span class="small muted">${esc(l.connection)}</span>` : '')],
        l.connected ? ['Signal', wifiSignal(l.signal_dbm)] : null,
        l.connected ? ['Band', `${l.freq_mhz > 3000 ? '5' : '2.4'} GHz <span class="small muted">${esc(l.freq_mhz)} MHz · ${esc(l.tx_bitrate || '')}</span>`] : null,
        ['Address', `<span class="mono">${esc(l.ipv4 || '—')}</span>${l.gateway ? ` <span class="small muted">gateway ${esc(l.gateway)}</span>` : ''}`],
      ]) + `<p class="help">The joined network is the internet uplink; LTE takes over when it fails (WAN / LTE page). SMS, the LTE pages, Cloudflare Mesh and forwarding keep working. LAN-only pages (clients, Wi-Fi &amp; LAN, DNS, firewall rules, VPN) are hidden.</p>`)}
      ${card('Joined network', form('net-form', netFields, vals, 'Save and reconnect') + '<div id="scan"></div>', '<button class="btn sm" id="scan-btn">Scan</button>')}
      ${card('Router mode', `<p>Turns the hotspot back on (it serves the LAN again with DHCP and DNS) and leaves ${esc(d.client.ssid)}. The router then uses LTE, or the USB link, for internet.</p>
        <div class="btns"><button class="btn danger" id="to-router">Switch to router mode</button></div>${cli}`)}
    </div>`;
    scanUI();
    onSubmit('net-form', netFields, x => send({ mode: 'client', ...x }, 'Change the Wi-Fi network?',
      `The device reconnects to <b>${esc(x.ssid)}</b>. Your connection to this page may drop; reach the device again (Mesh, or that network) and confirm within the countdown, or the previous network comes back.`));
    $('#to-router').onclick = () => send({ mode: 'router' }, 'Switch to router mode?',
      `The hotspot comes back (sign in at <b>https://${esc(d.lan)}</b> on it), and the device leaves <b>${esc(d.client.ssid)}</b>, so this page will disconnect. Confirm the change within the countdown from the hotspot, or over Cloudflare Mesh via LTE, or the device goes back to client mode.`);
    return;
  }
  const h = d.hotspot || {};
  v.innerHTML = `<div class="grid two">
    ${card('Router mode', kv([
      ['Mode', pill('router', 'ok') + ' <span class="small muted">hotspot LAN with DHCP and DNS</span>'],
      ['Hotspot', `${esc(h.ssid || '—')} ${h.state === 'activated' ? pill('on', 'ok') : pill(h.state || 'off', 'bad')}`],
    ]) + cli)}
    ${d.can_client ? card('Switch to Wi-Fi client mode', `<div class="notice">The device joins an existing Wi-Fi network for internet (LTE becomes the backup). The hotspot, DHCP and DNS server turn off,
        and LAN IPv6, VPN routing for the LAN, Mesh access to the LAN and the Wi-Fi off schedule are switched off. Make sure you can reach the device afterwards: over Cloudflare Mesh, from that network (if allowed below) or over USB.</div>`
      + form('client-form', netFields, vals, 'Switch to Wi-Fi client mode'))
      : card('Wi-Fi client mode', '<p class="muted">Not available while the wired LAN bridge is on (kstick-setup wired-lan off first).</p>')}
  </div>`;
  onSubmit('client-form', netFields, x => send({ mode: 'client', ...x }, 'Switch to Wi-Fi client mode?',
    `The hotspot <b>${esc(h.ssid || '')}</b> turns off and the device joins <b>${esc(x.ssid)}</b>, so this page will disconnect if you use the hotspot. Reach the device again and confirm within the countdown, or the hotspot comes back.`));
}

// ---------------------------------------------------------------- Cloudflare Mesh
async function pgMesh(v) {
  const viaMesh = /^100\.(9[6-9]|10[0-9]|11[01])\./.test(location.hostname) || location.hostname.startsWith('2606:4700:cf1');
  const draw = async () => {
    const m = await api('GET', '/mesh');
    if (!m.installed) {
      v.innerHTML = card('Cloudflare Mesh', '<p class="muted">The Cloudflare One client (cloudflare-warp) is not installed on this device. Install it with the headless repack: <code>INSTALL=1 ./deploy/repack-warp-headless.sh</code> on the dev host.</p>');
      return;
    }
    const running = m.service === 'active', connected = m.status === 'Connected';
    const state = !running ? pill('service ' + (m.service || 'stopped'), 'bad') : connected ? pill('connected', 'ok') : pill(m.status || 'unknown', 'warn');
    const btns = [
      !running || !m.enabled ? '<button class="btn sm primary" data-a="enable">Start, and start at boot</button>' : '',
      running && !connected && m.registered ? '<button class="btn sm primary" data-a="connect">Connect</button>' : '',
      running && connected ? '<button class="btn sm danger" data-a="disconnect">Disconnect</button>' : '',
      running ? '<button class="btn sm" data-a="restart">Restart client</button>' : '',
      '<button class="btn sm ghost" id="mesh-refresh">Refresh</button>',
    ].join(' ');
    const regFields = [
      { key: 'token', label: 'Mesh node token', type: 'password', required: true, help: 'From the Cloudflare dashboard (Zero Trust > Networking > Mesh > add a node). The whole "warp-cli connector new … && warp-cli connect" command can be pasted too.' },
      { key: 'replace', label: 'Replace the current registration', type: 'check', help: 'The old registration is moved aside on this device, not deleted at Cloudflare.' },
    ];
    v.innerHTML = `<div class="grid two">
      ${card('Cloudflare Mesh', kv([
        ['Status', state + (m.network ? ` <span class="small muted">network ${esc(m.network)}</span>` : '') + (m.reason ? `<div class="small muted">${esc(m.reason)}</div>` : '')],
        ['Mesh address', m.ipv4 ? `<span class="mono">${esc(m.ipv4)}</span>${m.ipv6 ? `<div class="mono small">${esc(m.ipv6)}</div>` : ''}` : '<span class="muted">none</span>'],
        ['Registration', m.registered ? `${esc(m.organization || '—')} <span class="small muted">${esc(m.account_type || '')}</span><div class="mono small">${esc(m.device_id)}</div>` : pill(running ? 'not registered' : 'unknown', 'warn')],
        ['Starts at boot', m.enabled ? pill('yes', 'ok') : pill('no', 'warn')],
      ]) + `<div class="btns">${btns}</div>`
        + (viaMesh ? '<p class="help warn">You are connected over Mesh: disconnecting or restarting the client cuts this page off (restart: for about 20 seconds).</p>' : ''))}
      ${card('Management over Mesh', field({ key: 'mesh-admin', label: 'Allow ping, SSH and this web UI over Cloudflare Mesh', type: 'check' }, m.admin)
        + '<p class="help">From devices in your Zero Trust organization, to the Mesh address above. A firewall change: confirm it within the countdown, or it is undone.</p>')}
      ${card(m.registered ? 'Enroll with another token' : 'Enroll this device', form('mesh-reg', regFields, {}, 'Register'))}
    </div>`;
    $('#mesh-refresh').onclick = draw;
    $$('[data-a]', v).forEach(b => b.onclick = async () => {
      const a = b.dataset.a;
      if ((a === 'disconnect' || a === 'restart') && viaMesh && !await confirmBox(a === 'disconnect' ? 'Disconnect Mesh?' : 'Restart the Mesh client?',
        a === 'disconnect' ? 'You are using this page over Mesh: it stops working until the client is connected again (from the USB link or the local network).' : 'This page is unreachable for about 20 seconds.', a === 'disconnect' ? 'Disconnect' : 'Restart')) return;
      if (await act(() => api('POST', '/mesh/' + a))) setTimeout(draw, a === 'restart' ? 20000 : 2500);
    });
    $('#f_mesh-admin', v).onchange = async e => {
      const on = e.target.checked;
      if (!on && viaMesh && !await confirmBox('Turn off management over Mesh?', 'You are using this page over Mesh: it stops working. Confirming the change will need the USB link or the local network.', 'Turn off')) { e.target.checked = true; return; }
      if (!await act(() => api('PUT', '/mesh', { admin: on }))) e.target.checked = !on;
    };
    onSubmit('mesh-reg', regFields, async x => {
      if (x.replace && viaMesh && !await confirmBox('Replace the registration?', 'The Mesh address changes and this page disconnects. Open the device again at its new Mesh address (shown here afterwards, or in the Cloudflare dashboard).', 'Replace')) return;
      if (await act(() => api('POST', '/mesh/register', x))) setTimeout(draw, 3000);
    });
  };
  await draw();
}
