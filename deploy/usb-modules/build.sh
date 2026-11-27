#!/usr/bin/env bash
# Build extra USB driver modules for the OpenStick 6.6.0-msm8916 kernel (no headers ship with it).
#
#   ./build.sh                 -> out/6.6.0-msm8916/*.ko  (+ out/firmware/rtl_nic/*.fw)
#
# Source: msm8916-mainline/linux tag v6.6-msm8916, the exact config from a stick (/proc/config.gz), and
# Ubuntu 24.04's aarch64 GCC 13 in Docker (the kernel was built with GCC 13.2). The kernel has no module
# signing or MODVERSIONS, so modules only have to match the release string and config.
# Env: KSRC (kernel tree, default ~/.cache/kstick-kernel/linux), KCONFIG (default ~/.cache/kstick-kernel/config-6.6.0-msm8916)
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
KDIR=${KDIR:-$HOME/.cache/kstick-kernel}
KSRC=${KSRC:-$KDIR/linux}
KCONFIG=${KCONFIG:-$KDIR/config-6.6.0-msm8916}
REL=6.6.0-msm8916
OUT=$HERE/out
[ -f "$KSRC/Makefile" ] || { echo "kernel tree missing: git clone --depth 1 --branch v6.6-msm8916 https://github.com/msm8916-mainline/linux $KSRC && rm -rf $KSRC/.git"; exit 1; }
[ -f "$KCONFIG" ] || { echo "config missing: ssh <stick> 'zcat /proc/config.gz' > $KCONFIG"; exit 1; }

# Realtek USB Ethernet (most USB-C docks), CDC-ACM and the common USB-serial adapters.
MODS="USB_RTL8152 USB_ACM USB_SERIAL USB_SERIAL_GENERIC USB_SERIAL_FTDI_SIO USB_SERIAL_CP210X USB_SERIAL_CH341 USB_SERIAL_PL2303"

mkdir -p "$OUT"
docker run --rm -i -u "$(id -u):$(id -g)" -v "$KSRC:/src" -v "$KCONFIG:/config:ro" -v "$OUT:/out" \
	-e MODS="$MODS" -e REL="$REL" kstick-kbuild bash -euo pipefail <<'IN'
cd /src
export ARCH=arm64 CROSS_COMPILE=aarch64-linux-gnu- LOCALVERSION=
cp /config .config
args=(); for m in $MODS; do args+=(-m "$m"); done
[ "${args[*]}" ] && scripts/config "${args[@]}" -e USB_SERIAL_GENERIC
make -s olddefconfig
# only the requested options may differ from the running kernel's config (plus the compiler text)
diff <(grep -v -E "CC_VERSION_TEXT|GCC_VERSION|AS_VERSION|LD_VERSION|PAHOLE_VERSION|_IS_GCC|_IS_BFD" /config) \
     <(grep -v -E "CC_VERSION_TEXT|GCC_VERSION|AS_VERSION|LD_VERSION|PAHOLE_VERSION|_IS_GCC|_IS_BFD" .config) |
	grep '^[<>]' | grep -v -E "$(echo $MODS | sed 's/ /|/g')" && { echo "config drifted (see above)"; exit 1; } || true
make -s -j"$(nproc)" modules_prepare
[ "$(make -s kernelrelease)" = "$REL" ] || { echo "release is $(make -s kernelrelease), want $REL"; exit 1; }
for d in drivers/net/usb drivers/usb/serial drivers/usb/class; do
	make -s -j"$(nproc)" KBUILD_MODPOST_WARN=1 M=$d modules 2>&1 | grep -v -E "^WARNING: modpost: .*(undefined|Module.symvers)|^WARNING: modpost: .* is missing" || true
done
rm -rf /out/$REL && mkdir -p /out/$REL
cp drivers/net/usb/r8152.ko drivers/usb/class/cdc-acm.ko drivers/usb/serial/{usbserial,ftdi_sio,cp210x,ch341,pl2303}.ko /out/$REL/
aarch64-linux-gnu-strip --strip-debug /out/$REL/*.ko
for f in /out/$REL/*.ko; do echo "$(basename $f): $(modinfo -F vermagic $f 2>/dev/null || strings $f | grep -m1 '^vermagic=')"; done
IN

# r8152 loads rtl_nic/rtl815x firmware for some chip revisions (works without it on most)
mkdir -p "$OUT/firmware/rtl_nic"
for f in rtl8153a-2 rtl8153a-3 rtl8153a-4 rtl8153b-2 rtl8153c-1 rtl8156a-2 rtl8156b-2; do
	[ -s "$OUT/firmware/rtl_nic/$f.fw" ] || curl -fsSL -o "$OUT/firmware/rtl_nic/$f.fw" \
		"https://git.kernel.org/pub/scm/linux/kernel/git/firmware/linux-firmware.git/plain/rtl_nic/$f.fw" || echo "firmware $f not fetched"
done
ls -la "$OUT/$REL" "$OUT/firmware/rtl_nic"
