#!/usr/bin/env bash
# Repack Cloudflare's cloudflare-warp .deb (arm64, Debian trixie) without the desktop GUI
# and its GTK/WebKit dependencies, then (optionally) install it on the KStick.
#
# Run on the dev host, NOT on the device: apt on the 317 MB, swapless stick can hang it.
#   ./deploy/repack-warp-headless.sh            # newest version in the repo
#   ./deploy/repack-warp-headless.sh 2026.8.2115.0
#   INSTALL=1 HOST=user@10.42.0.2 ./deploy/repack-warp-headless.sh
# Install over USB (10.42.0.2): warp-svc restarts, which briefly drops Cloudflare Mesh.
set -euo pipefail

REPO=https://pkg.cloudflareclient.com
DIST=trixie ARCH=arm64
HOST=${HOST:-user@10.42.0.2}
WANT=${1:-}

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
cd "$work"

curl -fsS "$REPO/dists/$DIST/main/binary-$ARCH/Packages" -o Packages
# One line per cloudflare-warp version: "version filename sha256"; pick requested or highest.
read -r ver file sha < <(awk '
  /^Package: /  { p = ($2 == "cloudflare-warp") }
  p && /^Version: /  { v = $2 }
  p && /^Filename: / { f = $2 }
  p && /^SHA256: /   { s = $2 }
  /^$/ { if (p && v) print v, f, s; p = 0; v = "" }
  END  { if (p && v) print v, f, s }' Packages |
  { if [ -n "$WANT" ]; then awk -v w="$WANT" '$1 == w'; else cat; fi; } | sort -V -k1,1 | tail -n1) || true
[ -n "${ver:-}" ] || { echo "version ${WANT:-?} not found in $REPO" >&2; exit 1; }
echo "cloudflare-warp $ver ($file)"

curl -fsS "$REPO/$file" -o orig.deb
echo "$sha  orig.deb" | sha256sum -c --quiet

mkdir x x/control x/new x/new/control
(cd x && ar x ../orig.deb && bsdtar -xf control.tar.* -C control)

# Data: drop the Flutter tray app and desktop integration; keep warp-svc/cli/dex/diag + unit.
bsdtar -czf x/new/data.tar.gz \
  --exclude './usr/lib/warp' --exclude './usr/lib/warp/*' --exclude './bin/warp-taskbar' \
  --exclude './usr/share/applications*' --exclude './usr/share/dbus-1*' \
  --exclude './usr/share/icons*' --exclude './usr/lib/systemd/user*' \
  @"$(ls x/data.tar.*)"
bsdtar -tf x/new/data.tar.gz | grep -q '^\./bin/warp-svc$' || { echo "warp-svc missing" >&2; exit 1; }

# Control: strip GUI deps, desktop steps in postinst, md5sums of removed files.
c=x/control n=x/new/control
cp "$c/prerm" "$c/postrm" "$n/"
sed -e '/^# Full-package-only desktop/,/^$/d' "$c/postinst" > "$n/postinst"
awk 'NR == FNR { sub(/^\.\//, ""); keep[$0]; next } ($2 in keep)' \
  <(bsdtar -tf x/new/data.tar.gz) "$c/md5sums" > "$n/md5sums"
size=$(bsdtar -tvf x/new/data.tar.gz | awk '{s+=$5} END{print int(s/1024)+1}')
newver="$ver+headless1"
sed -e "s/^Version: .*/Version: $newver/" -e "s/^Installed-Size: .*/Installed-Size: $size/" \
    -e 's/, *desktop-file-utils//; s/, *libayatana-appindicator3-1//; s/, *libwebkit2gtk-[0-9.]*-0//' \
    -e 's/^Description: .*/Description: Cloudflare WARP Client (headless repack: GUI and desktop dependencies removed)/' \
    "$c/control" > "$n/control"
grep -qE 'webkit|appindicator|desktop-file-utils' "$n/control" && { echo "GUI deps still present" >&2; exit 1; }
for s in postinst prerm postrm; do sh -n "$n/$s"; chmod 755 "$n/$s"; done
chmod 644 "$n/control" "$n/md5sums"
(cd "$n" && bsdtar --uid 0 --gid 0 --uname root --gname root -czf ../control.tar.gz ./control ./md5sums ./postinst ./prerm ./postrm)
printf '2.0\n' > x/new/debian-binary
out="cloudflare-warp_${newver}_${ARCH}.deb"
(cd x/new && ar rc "$OLDPWD/$out" debian-binary control.tar.gz data.tar.gz)

dest=${OUT_DIR:-$(dirname "$(readlink -f "$0")")/../build}
mkdir -p "$dest" && cp "$out" "$dest/"
echo "built $dest/$out"

if [ "${INSTALL:-0}" = 1 ]; then
  scp -q "$out" "$HOST:/tmp/$out"
  ssh "$HOST" "set -e
    sudo tar -czf /root/cloudflare-warp-state-\$(date +%Y%m%d-%H%M).tgz -C /var/lib cloudflare-warp
    sudo apt-mark unhold cloudflare-warp >/dev/null
    sudo dpkg -i /tmp/$out && rm -f /tmp/$out
    sudo apt-mark hold cloudflare-warp
    sleep 10; warp-cli --accept-tos status | head -2"
fi
