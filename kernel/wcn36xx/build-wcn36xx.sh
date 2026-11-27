#!/bin/sh
set -e
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq >/dev/null && apt-get install -y -qq gcc-13-aarch64-linux-gnu make bc flex bison libssl-dev libelf-dev kmod python3 >/dev/null
ln -sf /usr/bin/aarch64-linux-gnu-gcc-13 /usr/local/bin/aarch64-linux-gnu-gcc
cd /src/linux-msm8916
cp /src/kstick-config .config
M="make ARCH=arm64 CROSS_COMPILE=aarch64-linux-gnu- LOCALVERSION= -j16"
$M olddefconfig >/dev/null
$M modules_prepare >/dev/null
KBUILD_MODPOST_WARN=1 $M M=drivers/net/wireless/ath/wcn36xx modules 2>&1 | grep -vE "undefined!$" | tail -15
modinfo drivers/net/wireless/ath/wcn36xx/wcn36xx.ko | grep -E "vermagic|depends"
cat include/config/kernel.release
