#!/usr/bin/env bash
# kstick.sh: set up and manage a KStick router from this PC (Linux/macOS with Go and ssh).
#
#   ./kstick.sh [-H user@host] install [options]   build, copy and install on a stick flashed with OpenStick
#   ./kstick.sh [-H user@host] backup [file]        full settings backup, downloaded to this PC
#   ./kstick.sh [-H user@host] restore <file>       upload a backup and restore it
#   ./kstick.sh [-H user@host] reset [--all] [-y]   clear VPN, firewall, API and other router settings
#
# Host: -H, or $KSTICK_HOST, default user@192.168.68.1 (OpenStick's USB address). An ssh alias works too.
# Run restore/reset over USB or Wi-Fi, not Cloudflare Mesh: they restart the Mesh client.
#
# install options:
#   --wifi-ssid NAME --wifi-pass PASS   Wi-Fi hotspot (new sticks: KStick-XXXX with a random password)
#   --apn APN                           mobile data APN (default: automatic)
#   --with-whatsapp                     include the WhatsApp forward module (needs a WAWAT server; off by default)
#   --no-blackbox                       skip the diagnostic black box logger (on by default; writes flash every 5 s)
#   --with-warp [--warp-token FILE]     Cloudflare WARP / Mesh node (headless repack; FILE = connector token)
#   --cloudflared-token FILE            Cloudflare Tunnel token, publishes the public SMS gateway
#   --cpu-max-mhz 200|400|800|998|0     CPU clock cap (new sticks 400; kept on reinstall; 0 = none)
#   --no-ssh-hardening                  keep SSH password login (default: key-only when a key is installed)
#   --no-slim                           keep docs/man pages and the default journal size
# backup options:  --no-sms-log  --no-cloudflare
set -euo pipefail
REPO=$(cd "$(dirname "$0")" && pwd) # file arguments stay relative to where you run this
HOST=${KSTICK_HOST:-user@192.168.68.1}
REMOTE_DIR=/var/tmp/kstick-bundle # on disk: /tmp may be RAM-backed on the stick

die() { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }
say() { printf '\033[1;32m==>\033[0m %s\n' "$*"; }
usage() { sed -n '2,22p' "$REPO/kstick.sh" | sed 's/^# \{0,1\}//'; exit "${1:-0}"; }

while [ $# -gt 0 ]; do
	case $1 in
		-H | --host) HOST=$2; shift 2 ;;
		-h | --help) usage ;;
		*) break ;;
	esac
done
CMD=${1:-}; [ $# -gt 0 ] && shift
# Extra ssh/scp options, e.g. KSTICK_SSH_OPTS="-i ~/.ssh/other_key"
read -r -a XOPT <<<"${KSTICK_SSH_OPTS:-}"
SSH=(ssh -o ConnectTimeout=10 "${XOPT[@]}")
SCP=(scp -q -o ConnectTimeout=10 "${XOPT[@]}")
qargs() { local a out=""; for a in "$@"; do out+=" $(printf '%q' "$a")"; done; echo "$out"; }

# Key login for the rest of the session; a new OpenStick stick usually accepts the password first.
ensure_ssh() {
	if ! "${SSH[@]}" -o BatchMode=yes "$HOST" true 2>/dev/null; then
		say "No SSH key login to $HOST yet: installing your key (enter the stick's password)"
		ssh-copy-id -o ConnectTimeout=10 "${XOPT[@]}" "$HOST" || die "could not reach $HOST over SSH"
	fi
}
remote_sudo() { "${SSH[@]}" -t "$HOST" "sudo $*"; } # -t: sudo may ask for a password

cmd_install() {
	local pass=() warp=0 warp_token="" cf_token=""
	while [ $# -gt 0 ]; do
		case $1 in
			--with-whatsapp) pass+=(--whatsapp) ;;
			--without-whatsapp) pass+=(--no-whatsapp) ;;
			--blackbox | --no-blackbox) pass+=("$1") ;;
			--wifi-ssid | --wifi-pass | --apn | --cpu-max-mhz) pass+=("$1" "$2"); shift ;;
			--no-ssh-hardening | --no-slim) pass+=("$1") ;;
			--with-warp) warp=1 ;;
			--warp-token) warp_token=$2; shift ;;
			--cloudflared-token) cf_token=$2; shift ;;
			-h | --help) usage ;;
			*) die "install: unknown option $1" ;;
		esac
		shift
	done
	[ -z "$warp_token" ] || [ $warp = 1 ] || die "--warp-token needs --with-warp"
	for f in "$warp_token" "$cf_token"; do [ -z "$f" ] || [ -r "$f" ] || die "token file $f not readable"; done
	command -v go >/dev/null || die "Go is needed to build the router programs"
	for f in warp_token cf_token; do [ -z "${!f}" ] || printf -v "$f" %s "$(cd "$(dirname "${!f}")" && pwd)/$(basename "${!f}")"; done
	cd "$REPO"

	b=$(mktemp -d) # global: the EXIT trap runs after this function returns
	trap 'rm -rf "${b:-}"' EXIT
	say "Building (arm64)"
	go vet ./... && go test ./...
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -buildvcs=false -ldflags "-s -w" -o "$b/router-webui" .
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -buildvcs=false -ldflags "-s -w" -o "$b/kstick-smsgw" ./smsgw
	cp deploy/kstick-setup deploy/router-firewall deploy/wan-failover deploy/kstick-blackbox deploy/*.service deploy/*.conf \
		deploy/50-kstick-smsgw.rules deploy/01-router-slim deploy/regdb/*-sforshee "$b/"
	sh xp/build.sh
	mkdir "$b/web" "$b/web-xp" && cp web/* "$b/web/" && cp web-xp/* "$b/web-xp/" # served from disk by router-webui (UI-only updates skip the binary)
	if [ $warp = 1 ]; then
		say "Repacking Cloudflare WARP without its desktop GUI"
		OUT_DIR="$b" ./deploy/repack-warp-headless.sh
		pass+=(--warp-deb "$REMOTE_DIR/$(cd "$b" && ls cloudflare-warp_*.deb)")
	fi
	if [ -n "$warp_token" ]; then install -m 600 "$warp_token" "$b/warp.token"; pass+=(--warp-token-file "$REMOTE_DIR/warp.token"); fi
	if [ -n "$cf_token" ]; then install -m 600 "$cf_token" "$b/cloudflared.token"; pass+=(--cloudflared-token-file "$REMOTE_DIR/cloudflared.token"); fi

	ensure_ssh
	say "Copying the bundle to $HOST:$REMOTE_DIR"
	"${SSH[@]}" "$HOST" "rm -rf $REMOTE_DIR && mkdir -m 700 $REMOTE_DIR"
	"${SCP[@]}" -r "$b"/* "$HOST:$REMOTE_DIR/"
	say "Installing on the stick"
	local rc=0
	remote_sudo "bash $REMOTE_DIR/kstick-setup install$(qargs "${pass[@]}")" || rc=$?
	"${SSH[@]}" "$HOST" "rm -rf $REMOTE_DIR" || true
	[ $rc = 0 ] || die "install failed on the stick (exit $rc)"
}

cmd_backup() {
	local out="" pass=()
	while [ $# -gt 0 ]; do
		case $1 in
			--no-sms-log | --no-cloudflare) pass+=("$1") ;;
			-*) die "backup: unknown option $1" ;;
			*) out=$1 ;;
		esac
		shift
	done
	ensure_ssh
	local remote
	remote=$("${SSH[@]}" "$HOST" "sudo -n kstick-setup backup --quiet$(qargs "${pass[@]}")" | tail -1) ||
		die "backup failed (needs passwordless sudo, or run 'sudo kstick-setup backup' on the stick)"
	out=${out:-$(basename "$remote")}
	(umask 077 && "${SSH[@]}" "$HOST" "sudo -n cat $(printf '%q' "$remote")" >"$out")
	tar -tzf "$out" KSTICK-BACKUP >/dev/null || die "downloaded file is not a valid backup"
	say "Backup saved to $out ($(du -h "$out" | cut -f1)); a copy stays on the stick in $(dirname "$remote")"
	echo "It contains Wi-Fi/VPN keys, API key hashes and tokens: keep it private."
}

cmd_restore() {
	local in="" pass=()
	while [ $# -gt 0 ]; do
		case $1 in
			-y | --yes) pass+=(--yes) ;;
			-*) die "restore: unknown option $1" ;;
			*) in=$1 ;;
		esac
		shift
	done
	[ -f "$in" ] || die "usage: $0 restore <backup.tar.gz>"
	tar -tzf "$in" KSTICK-BACKUP >/dev/null 2>&1 || die "$in is not a kstick-setup backup"
	ensure_ssh
	local r="/var/tmp/kstick-restore-$$.tar.gz"
	"${SCP[@]}" "$in" "$HOST:$r"
	local rc=0
	remote_sudo "kstick-setup restore$(qargs "${pass[@]}") $r" || rc=$?
	"${SSH[@]}" "$HOST" "rm -f $r" || true
	[ $rc = 0 ] || die "restore failed (exit $rc)"
}

cmd_reset() {
	ensure_ssh
	remote_sudo "kstick-setup reset$(qargs "$@")"
}

case $CMD in
	install) cmd_install "$@" ;;
	backup) cmd_backup "$@" ;;
	restore) cmd_restore "$@" ;;
	reset) cmd_reset "$@" ;;
	"" | help) usage ;;
	*) die "unknown command '$CMD' (install, backup, restore, reset)" ;;
esac
