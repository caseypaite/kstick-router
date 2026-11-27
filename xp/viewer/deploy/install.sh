#!/bin/sh
# Run on the router as root from this directory (with the lofi-viewer binary next to it).
# Optional: VIEWER_PASSWORD=... sets/creates the account "viewer".
# lofi-viewer listens on loopback only; a cloudflared tunnel on the same device publishes it.
set -e
id lofiview >/dev/null 2>&1 || useradd --system --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin --comment "lofi-viewer service" lofiview
install -m 0755 lofi-viewer /usr/local/sbin/lofi-viewer
install -d -m 0750 -o root -g lofiview /etc/lofi-viewer
if [ -n "${VIEWER_PASSWORD:-}" ]; then printf '%s\n' "$VIEWER_PASSWORD" | /usr/local/sbin/lofi-viewer passwd viewer; fi
[ -f /etc/lofi-viewer/users ] && chown root:lofiview /etc/lofi-viewer/users && chmod 0640 /etc/lofi-viewer/users
# older installs reached the viewer over Cloudflare Mesh through a firewall helper: remove it
if [ -e /etc/systemd/system/lofi-viewer-fw.service ]; then
	systemctl disable --now lofi-viewer-fw.path lofi-viewer-fw.timer lofi-viewer-fw.service 2>/dev/null || true
	[ -x /usr/local/sbin/lofi-viewer-fw ] && /usr/local/sbin/lofi-viewer-fw remove || true
	rm -f /etc/systemd/system/lofi-viewer-fw.path /etc/systemd/system/lofi-viewer-fw.timer /etc/systemd/system/lofi-viewer-fw.service /usr/local/sbin/lofi-viewer-fw
fi
install -m 0644 lofi-viewer.service /etc/systemd/system/
systemctl daemon-reload
systemctl enable lofi-viewer.service
systemctl restart lofi-viewer.service
sleep 2
systemctl is-active lofi-viewer
