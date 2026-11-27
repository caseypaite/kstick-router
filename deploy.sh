#!/bin/sh
# Build router-webui for the KStick (arm64) and install it over SSH.
#   ./deploy.sh            build + update binaries, UI files, helper scripts, units and configs
#   ./deploy.sh --migrate  one-time: also switch LAN DHCP/DNS from NetworkManager to router-dnsmasq
#
# Light on the LTE link (uploads heat the modem and cost data): the stick keeps a mirror of the
# deployed files in /var/lib/kstick-deploy (no secrets: programs, units, configs, UI); only files whose SHA-256 differs are sent, as one
# compressed tar stream: xz -6 (8 MB dictionary, easy on the stick's RAM) when the stick has xz, else gzip -9.
# A UI-only change is ~25 KB.
# Services restart only when their own files changed.
set -eu
HOST=${HOST:-kstick}
cd "$(dirname "$0")"
MIRROR=/var/lib/kstick-deploy
go vet ./... && go test ./...
sh xp/build.sh   # the XP-style UI -> web-xp/
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -buildvcs=false -ldflags "-s -w" -o build/router-webui .
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -buildvcs=false -ldflags "-s -w" -o build/kstick-smsgw ./smsgw

# stage: exactly what the stick should have
ST=build/stage
rm -rf "$ST" && mkdir -p "$ST/web" "$ST/web-xp"
cp build/router-webui build/kstick-smsgw deploy/kstick-setup deploy/50-kstick-smsgw.rules deploy/router-firewall deploy/wan-failover \
  deploy/kstick-blackbox deploy/*.service deploy/90-router.conf deploy/sshd-90-router.conf deploy/modules-router.conf \
  deploy/warp-svc-lowmem.conf deploy/watchdog-router.conf "$ST/"
cp web/* "$ST/web/"
cp web-xp/* "$ST/web-xp/"
(cd "$ST" && find . -type f | sed 's|^\./||' | sort | xargs sha256sum) > build/stage.sha256

# what the stick has, and what differs
# mirror = what was uploaded; .installed.sha256 = what the last *completed* install put in place
ssh "$HOST" "sudo install -d -m 755 $MIRROR && cd $MIRROR && find . -type f ! -name .installed.sha256 | sed 's|^\./||' | sort | xargs -r sha256sum" > build/remote.sha256
ssh "$HOST" "cat $MIRROR/.installed.sha256 2>/dev/null" > build/installed.sha256 || :
if ssh "$HOST" 'command -v xz >/dev/null'; then ZIP="xz -6 -T0 -c"; UNZIP="xz -dc"; else ZIP="gzip -9 -c"; UNZIP="gzip -dc"; fi
CHANGED=$(sort build/stage.sha256 | comm -23 - "$(sort build/remote.sha256 > build/remote.sorted && echo build/remote.sorted)" | awk '{print $2}')
# not yet installed (new, or an earlier install was interrupted): decides restarts and whether to run the install
PENDING=$(sort build/stage.sha256 | comm -23 - "$(sort build/installed.sha256 > build/installed.sorted && echo build/installed.sorted)" | awk '{print $2}')
if [ -z "$CHANGED" ] && [ -z "$PENDING" ]; then
  echo "deploy: the stick is up to date"
  [ "${1:-}" = "--migrate" ] || exit 0
elif [ -z "$CHANGED" ]; then
  echo "deploy: nothing to upload; finishing an interrupted install of: $(echo $PENDING)"
else
  RAW=$(cd "$ST" && cat $CHANGED | wc -c)
  COMP=$(cd "$ST" && tar -cf - $CHANGED | $ZIP | wc -c)
  echo "deploy: $(echo "$CHANGED" | wc -l) changed file(s), $((RAW / 1024)) KB -> $((COMP / 1024)) KB (${ZIP%% *}):"
  echo "$CHANGED" | sed 's/^/  /'
  (cd "$ST" && tar -cf - $CHANGED | $ZIP) | ssh "$HOST" "sudo sh -c 'cd $MIRROR && $UNZIP | tar -xf - --no-same-owner'"
fi
has() { echo "$PENDING" | grep -qxE "$1"; } # did any not-yet-installed file match?

RESTART=""
has 'router-webui|router-webui.service' && RESTART="$RESTART router-webui"
has 'kstick-smsgw|kstick-smsgw.service' && RESTART="$RESTART kstick-smsgw"
has 'wan-failover|wan-failover.service' && RESTART="$RESTART wan-failover"
has 'kstick-blackbox|kstick-blackbox.service' && RESTART="$RESTART kstick-blackbox"
has 'router-cpufreq.service' && RESTART="$RESTART router-cpufreq"
SSHD=$(has 'sshd-90-router.conf' && echo 1 || echo 0)
WDOG=$(has 'watchdog-router.conf' && echo 1 || echo 0)

ssh "$HOST" "RESTART='$RESTART' SSHD=$SSHD WDOG=$WDOG MIRROR=$MIRROR"' sh -c '"'"'set -e; export PATH=$PATH:/usr/sbin:/sbin; cd $MIRROR
sudo install -m 755 router-webui kstick-smsgw kstick-setup router-firewall wan-failover kstick-blackbox /usr/local/sbin/
sudo install -d -m 755 /usr/local/share/router-webui/web
sudo install -m 644 web/* /usr/local/share/router-webui/web/   # served from disk: no restart needed for UI changes
sudo install -d -m 755 /usr/local/share/router-webui/web-xp
sudo install -m 644 web-xp/* /usr/local/share/router-webui/web-xp/   # XP-style UI (System → Web interface)
# public SMS gateway: unprivileged user, polkit permission to send SMS, config dir readable by its group
id smsgw >/dev/null 2>&1 || sudo useradd --system --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin smsgw
sudo install -m 644 50-kstick-smsgw.rules /etc/polkit-1/rules.d/50-kstick-smsgw.rules
sudo install -d -m 750 -o root -g smsgw /etc/smsgw
sudo install -d -m 750 -o smsgw -g smsgw /var/lib/smsgw   # SQLite SMS log (sms.db), shared with router-webui
sudo install -m 644 router-webui.service kstick-smsgw.service router-dnsmasq.service router-firewall.service wan-failover.service router-zram.service router-cpufreq.service kstick-blackbox.service /etc/systemd/system/
sudo install -m 644 90-router.conf /etc/sysctl.d/90-router.conf
sudo install -m 644 modules-router.conf /etc/modules-load.d/router.conf
sudo install -D -m 644 warp-svc-lowmem.conf /etc/systemd/system/warp-svc.service.d/10-router-lowmem.conf   # applies on next warp-svc start
sudo install -m 644 sshd-90-router.conf /etc/ssh/sshd_config.d/90-router.conf
[ "$SSHD" = 1 ] && sudo sshd -t && sudo systemctl reload ssh
if [ "$WDOG" = 1 ] && [ -e /dev/watchdog0 ]; then   # hardware watchdog, petted by systemd (PID 1 re-executes to pick it up)
  sudo install -D -m 644 watchdog-router.conf /etc/systemd/system.conf.d/90-router-watchdog.conf && sudo systemctl daemon-reexec
fi
sudo systemctl daemon-reload
sudo sysctl -q -p /etc/sysctl.d/90-router.conf
# zram swap (router-zram.service) is enabled by kstick-setup on a new install; deploys leave it as the device has it
sudo systemctl enable router-cpufreq.service router-webui >/dev/null 2>&1   # kstick-smsgw: enabled by the installer, left as the device has it
for s in $RESTART; do
  case $s in kstick-blackbox|kstick-smsgw) sudo systemctl try-restart $s ;; *) sudo systemctl restart $s ;; esac   # the installer decides whether the black box and the gateway run
done
echo "restarted:${RESTART:- nothing}"
find . -type f ! -name .installed.sha256 | sed "s|^\./||" | sort | xargs sha256sum | sudo tee .installed.sha256 >/dev/null   # only after a completed install
sleep 2; systemctl is-active router-webui'"'"
if [ "${1:-}" = "--migrate" ]; then
ssh "$HOST" 'set -e; export PATH=$PATH:/usr/sbin:/sbin
sudo cp -a /etc/router/firewall.nft /etc/router/firewall.nft.pre-webui 2>/dev/null || true
sudo /usr/local/sbin/router-webui -render
[ -f /var/lib/misc/router-dnsmasq.leases ] || sudo cp /var/lib/NetworkManager/dnsmasq-wlan0.leases /var/lib/misc/router-dnsmasq.leases 2>/dev/null || true
LAN=$(sudo sed -n '"'"'s/.*"address": *"\([0-9./]*\)".*/\1/p'"'"' /etc/router/router.json | head -1)
sudo /usr/local/sbin/router-firewall reload
# hand DHCP/DNS over from NetworkManager shared mode to router-dnsmasq (Wi-Fi blips once)
sudo nmcli connection modify hotspot ipv4.method manual ipv4.addresses "${LAN:-192.168.4.1/24}"
sudo nmcli -w 30 connection up hotspot >/dev/null
sudo systemctl enable --now router-dnsmasq
sudo systemctl restart wan-failover'
fi
