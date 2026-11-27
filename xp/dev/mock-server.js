#!/usr/bin/env node
/* Local preview of the XP desktop without touching the router: serves ./web with the router's
   security headers (same CSP / X-Frame-Options) and answers /api/* with canned data.
   Usage: node dev/mock-server.js [port]   → http://127.0.0.1:8088 (any user name / password works) */
'use strict';
const http = require('http'), fs = require('fs'), path = require('path');
const WEB = path.join(__dirname, '..', '..', 'web-xp');
const PORT = +process.argv[2] || 8088;
const HDR = {
  'Content-Security-Policy': "default-src 'self'; img-src 'self' data:; style-src 'self'; style-src-attr 'unsafe-inline'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'",
  'X-Frame-Options': 'DENY', 'X-Content-Type-Options': 'nosniff', 'Referrer-Policy': 'no-referrer', 'Cache-Control': 'no-store',
};
let authed = false, pending = null;
const now = () => Math.floor(Date.now() / 1000);
const pts = n => Array.from({ length: n }, (_, i) => ({ rx: 20000 + 15000 * Math.sin(i / 4) ** 2, tx: 4000 + 3000 * Math.cos(i / 5) ** 2 }));
const status = () => ({
  mode: 'router',
  system: { hostname: 'kstick', model: 'HMUF02 4G Modem Stick', uptime: 43320, cpu: 7.5, load: [0.27, 0.17, 0.11], mem_total: 332 << 20, mem_available: 172 << 20,
    disk_total: 3.4 * 2 ** 30, disk_free: 2.6 * 2 ** 30, conntrack: 112, conntrack_max: 4096,
    cpufreq: { cur_mhz: 400, max_mhz: 998, hw_max_mhz: 998, governor: 'schedutil' },
    temps: [{ name: 'modem-thermal', label: 'modem', c: 49, trip: 85 }, { name: 'cpu0-1-thermal', label: 'cpu', c: 47, trip: 90 }],
    thermal_guard: { enabled: true, level: 0, engaged: 0 } },
  wan: { active: 'wlan0', wwan0: 'down', wlan0: 'up (ok)', updated: new Date().toISOString() },
  wifi_uplink: { connected: true, ssid: 'HomeNet', signal_dbm: -58, freq_mhz: 2437, ipv4: '192.168.1.50', gateway: '192.168.1.1' },
  modem: { present: true, state: 'failed', operator: '', signal: 0, tech: 'lte', modes: '', imei: '86xxxxxxxxxxxxx', firmware: 'HIMI_U01_MODEM_V1.0' },
  vpn: { active: [] }, ipv6: { active: false }, wifi: { ssid: 'KStick', state: 'activated' },
  clients: { wifi: 2, known: 5 }, firewall: { dropped: { input: 2905, forward: 0 }, rules: 0, forwards: 0 },
});
const routes = {
  'GET /session': () => authed ? [200, { csrf: 'mock-csrf', user: 'user' }] : [401, { error: 'not signed in' }],
  'POST /login': b => { if (!b.password) return [401, { error: 'Wrong user name or password' }]; authed = true; return [200, { csrf: 'mock-csrf' }]; },
  'POST /logout': () => { authed = false; return [200, {}]; },
  'GET /modules': () => [200, { whatsapp: false }],
  'GET /mode': () => [200, { mode: 'router', hotspot: { ssid: 'KStick' }, client: { ssid: '', has_psk: false } }],
  'GET /status': () => [200, status()],
  'GET /traffic': () => [200, { wwan0: pts(60), wlan0: pts(60) }],
  'GET /speedtest': () => [200, { running: false, last: null }],
  'GET /mesh': () => [200, { installed: true, service: 'active', enabled: true, status: 'Connected', network: 'healthy', ipv4: '100.96.0.10', ipv6: '2606:4700:cf1:1000::14', registered: true, organization: 'kc', account_type: 'Team', device_id: 'abcd-1234', admin: true }],
  'GET /pending': () => [200, pending ? { pending } : {}],
  'PUT /thermal': () => { pending = { id: 'p1', what: ['firewall'], seconds_left: 60 }; return [200, { message: 'Saved', pending }]; },
  'GET /thermal': () => [200, { config: { enabled: true, start_c: 75, release_c: 65, down_kbit: 4000, up_kbit: 1000, cooldown: false, cooldown_c: 88, cooldown_min: 10 }, status: { enabled: true, level: 0, max_c: 49 } }],
  'POST /pending/p1/confirm': () => { pending = null; return [200, { message: 'Kept', pending: null }]; },
  'POST /pending/p1/revert': () => { pending = null; return [200, { message: 'Undone', pending: null }]; },
  'POST /system/reboot': () => [200, { message: 'Rebooting' }],
  'GET /system': () => [200, { hostname: 'kstick', model: 'HMUF02 4G Modem Stick', os: 'Debian 13', kernel: '6.6.0-msm8916', uptime: 43320, time: new Date().toISOString(), ntp_synced: true, timezone: 'Asia/Kolkata', admin_user: 'user', confirm_secs: 90, watchdog: null }],
  'GET /services': () => [200, [{ name: 'router-webui', ActiveState: 'active', SubState: 'running', ActiveEnterTimestamp: 'Thu 2026-11-27 19:05:00 IST' }]],
  'GET /updates': () => [200, { upgradable: [], upgradable_known: true, running: false, last_check: '' }],
  'GET /ui': () => [200, { style: 'xp', browser: '', serving: 'xp', available: ['classic', 'xp'] }],
  'PUT /ui': () => [200, { message: 'Saved and applied.' }],
  'GET /logs': () => [200, { lines: ['Nov 27 12:00:00 kstick router-webui: started'], text: 'Nov 27 12:00:00 kstick router-webui: started' }],
};
const MIME = { '.html': 'text/html; charset=utf-8', '.js': 'text/javascript; charset=utf-8', '.css': 'text/css; charset=utf-8' };
http.createServer((req, res) => {
  const u = new URL(req.url, 'http://x');
  const send = (code, body, type = 'application/json') => { res.writeHead(code, { ...HDR, 'Content-Type': type }); res.end(typeof body === 'string' || Buffer.isBuffer(body) ? body : JSON.stringify(body)); };
  if (u.pathname.startsWith('/api/')) {
    let raw = '';
    req.on('data', c => raw += c);
    req.on('end', () => {
      const key = `${req.method} ${u.pathname.slice(4)}`, h = routes[key];
      if (!authed && !['GET /session', 'POST /login'].includes(key)) return send(401, { error: 'not signed in' });
      if (req.method !== 'GET' && key !== 'POST /login' && req.headers['x-csrf-token'] !== 'mock-csrf') return send(403, { error: 'bad CSRF token' });
      if (!h) return send(200, {});   // unknown endpoint: empty object (page may show an error box — fine for a mock)
      const [code, body] = h(raw ? JSON.parse(raw) : {});
      send(code, body);
    });
    return;
  }
  const f = u.pathname === '/' ? 'index.html' : u.pathname.slice(1);
  if (!/^[\w.-]+$/.test(f)) return send(404, 'not found', 'text/plain');
  fs.readFile(path.join(WEB, f), (e, d) => e ? send(404, 'not found', 'text/plain') : send(200, d, MIME[path.extname(f)] || 'application/octet-stream'));
}).listen(PORT, '127.0.0.1', () => console.log(`mock router UI on http://127.0.0.1:${PORT}`));
