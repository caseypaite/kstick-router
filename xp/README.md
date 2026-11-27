# KStick XP — Windows-XP-style web UI

A Luna-style desktop for the router's web UI: a welcome (sign-in) screen, desktop icons, draggable and resizable
windows, a Start menu, a taskbar with tray and clock, and balloon notifications. Every router page (Dashboard, WAN/LTE,
Wi-Fi & LAN, Firewall, VPN, Cloudflare Mesh, SMS, System, …) opens as its own window, and several can be open at once.

It uses the same API, sign-in, CSRF protection and confirm-or-revert safety net as the Classic UI. Choose it under
System → Web interface, or per browser with `/?ui=xp` (see the main README, "Web UI").

## Layout

```
xp/src/pages.js    page logic, shared with the Classic UI (API calls unchanged), shell parts removed
xp/src/shell.js    desktop: window manager, Start menu, taskbar/tray, balloons, log off / restart
xp/src/index.html  markup (no inline JS)
xp/src/style.css   Luna theme; all icons are inline SVG
xp/build.sh        -> ../web-xp/{index.html,app.js,style.css}   (deploy.sh and kstick.sh run it)
xp/dev/            mock-server.js (local preview with the router's CSP), smoke.js / smoke-viewer.js (jsdom tests)
xp/viewer/         lofi-viewer: read-only public dashboard (below)
xp/mifi-mesh-lan/  mesh-lan-gw: Mesh access for a router's LAN clients (below)
```

The server sends `X-Frame-Options: DENY` and a strict CSP (`script-src 'self'`, no inline `<style>`,
`img-src 'self' data:`), and it serves only files that exist. So the UI is one document (no iframes), with no inline
scripts, and all icons are inline SVG.

**Keeping the UIs in step:** a page changed in `web/app.js` must be changed in `xp/src/pages.js` too. The page
functions are the same code; the XP version renders them into windows.

Multi-window integration: page renders are serialised, so each page's `every()` poll belongs to its own window (stopped
on close, paused while minimised or in a background tab), and `route()` redraws the window being used.

## Build and test

```sh
sh xp/build.sh                                  # build into web-xp/
cd xp/dev && npm i --no-save jsdom@24 && cd -   # once, for the tests
node xp/dev/mock-server.js 8088                 # preview at http://127.0.0.1:8088 (any password)
node xp/dev/smoke.js http://127.0.0.1:8088      # headless test of the desktop and every window
```

## Read-only public viewer (`xp/viewer`, `lofi-viewer`)

A Dashboard-only, read-only view for people who should see the router's status but not manage it, published on the
internet through a Cloudflare Tunnel:

```
browser ──https──▶ Cloudflare ──tunnel (cloudflared on the router)──▶ 127.0.0.1:8090  lofi-viewer (unprivileged)
```

- Serves the XP UI in viewer mode, with **its own accounts** (`/etc/lofi-viewer/users`, PBKDF2-SHA256) and **its own
  read-only data** (/proc, /sys, nmcli, mmcli). It holds no router credential and never calls the admin API, so nothing
  can be changed through it. Only `GET` on `/api/{status,traffic,mode,modules,speedtest,pending}`; everything else is 403.
- Modem identifiers (IMEI, IMSI, ICCID, number, APN), IP addresses, gateways and SSIDs are never collected.
- Sign-in limits: 5 failures in 15 minutes per client IP (`CF-Connecting-IP`, trusted only from loopback), 30 overall.
  Sessions: HttpOnly + Secure + SameSite=Strict cookie, 1 h idle, 12 h at most.
- Sandboxed systemd unit: its own user, no capabilities, read-only system, `IPAddressAllow=localhost`, MemoryMax=40M
  (it uses 2–3 MB). It listens on 127.0.0.1:8090 only, so it needs no firewall rule.

Set up:
1. Build: `cd xp/viewer && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "-s -w" -o deploy/lofi-viewer`.
2. Copy `xp/viewer/deploy/*` to the router and run `sudo VIEWER_PASSWORD='…' sh install.sh` there.
3. Create a Cloudflare Tunnel for the router with a public hostname → `http://localhost:8090`, and run its connector
   on the router: put the token in `/etc/cloudflared/token` and `systemctl enable --now cloudflared`
   (`deploy/cloudflared.service`). Use a separate tunnel from the SMS gateway's, or add the hostname to that one.

Manage accounts on the router:
```sh
read -rs PW && printf '%s\n' "$PW" | sudo lofi-viewer passwd viewer; unset PW   # set / change (min 6 characters)
sudo lofi-viewer deluser alice
sudo journalctl -u lofi-viewer -f                                                 # sign-ins with client IPs
```
Test locally: `lofi-viewer serve -listen 127.0.0.1:8099 -web web-xp -users /tmp/users -insecure-cookie`, then
`node xp/dev/smoke-viewer.js http://127.0.0.1:8099 viewer '<password>'`.

## Mesh access for a router's LAN (`xp/mifi-mesh-lan`, `mesh-lan-gw`)

For a router that is a Cloudflare Mesh node: lets its Wi-Fi/LAN clients reach other Mesh nodes directly. It adds a
forward rule LAN → `CloudflareWARP` inside router-webui's firewall table and a masquerade in its own `ip mesh-lan`
table, so LAN clients appear on the Mesh as the router. router-webui regenerates its firewall (dropping the forward
rule); `mesh-lan-gw.path` and a 60 s timer re-add it.

Edit `LAN_IF` and `LAN_NET` at the top of `mesh-lan-gw` to match the LAN, then run `sudo sh install.sh` on the router.
Remove with `sudo mesh-lan-gw remove` and disable the units.
