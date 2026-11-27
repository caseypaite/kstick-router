#!/bin/sh
# Build the MF800 LCD DTB (stock OpenStick DTB + mf800-lcd.dtsi) and the panel init firmware into ./out.
#   MADCTL=0xC0 ./build.sh    optional rotation/mirroring for the panel
set -eu
cd "$(dirname "$0")"
mkdir -p out
dtc -q -I dtb -O dts base-msm8916-fy-mf800-fixed.dtb > out/combined.dts
cat mf800-lcd.dtsi >> out/combined.dts
dtc -q -I dts -O dtb -o out/msm8916-fy-mf800-lcd.dtb out/combined.dts
python3 -I mkfw.py out/fy,mf800-lcd.bin ${MADCTL:+--madctl "$MADCTL"}
sha256sum out/msm8916-fy-mf800-lcd.dtb out/fy,mf800-lcd.bin
