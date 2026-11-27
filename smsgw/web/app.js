'use strict';
document.querySelectorAll('.host').forEach(e => { e.textContent = location.host || e.textContent; });
const f = document.getElementById('send'), out = document.getElementById('out');
f.addEventListener('submit', async e => {
  e.preventDefault();
  const b = f.querySelector('button');
  b.disabled = true; out.hidden = false; out.textContent = 'Sending…';
  const body = { to: f.to.value.trim() };
  if (f.message.value.trim()) body.message = f.message.value;
  try {
    const r = await fetch('/api/v1/sms', { method: 'POST', headers: { 'Authorization': 'Bearer ' + f.key.value.trim(), 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
    const j = await r.json().catch(() => ({ error: 'HTTP ' + r.status }));
    out.textContent = `HTTP ${r.status}\n` + JSON.stringify(j, null, 2);
    out.className = r.ok ? 'ok' : 'bad';
  } catch (err) { out.textContent = String(err); out.className = 'bad'; }
  finally { b.disabled = false; }
});
