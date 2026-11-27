#!/bin/sh
# Install the MF800 TFT support on a stick from this PC: LCD DTB + panel init firmware + SPI udev rule,
# and point extlinux at the LCD DTB (the original is kept; extlinux.conf.headless is the rollback copy).
# The status screen itself is part of router-webui (lcd.go) and starts when the DTB has the panel.
#   ./install.sh <ssh-host>      Reboot afterwards.
# Rollback: sudo cp /boot/extlinux/extlinux.conf.headless /boot/extlinux/extlinux.conf && sudo reboot
# Also installs lcd-fbcon-detach.service and the lcd_ctl wrapper, and disables the stock pwrkey-lcd/lcd-splash services.
# Undo: systemctl disable lcd-fbcon-detach; mv /usr/local/bin/lcd_ctl.real /usr/local/bin/lcd_ctl; systemctl enable pwrkey-lcd
set -eu
HOST=${1:?usage: ./install.sh <ssh-host>}
cd "$(dirname "$0")"
./build.sh
ssh "$HOST" 'tr "\0" " " </proc/device-tree/compatible' | grep -q 'fy,mf800' || { echo "not an MF800 (fy,mf800)" >&2; exit 1; }
scp -q out/msm8916-fy-mf800-lcd.dtb "out/fy,mf800-lcd.bin" 60-mf800-lcd-spi.rules \
	lcd-fbcon-detach lcd-fbcon-detach.service lcd_ctl-wrapper "$HOST:/var/tmp/"
ssh "$HOST" 'set -e; cd /var/tmp
# Stop the stock image from fighting router-webui for the panel (SPI -110 errors; see README "MF800"):
# fbcon off the panel, the stock power-key and splash daemons off, and lcd_ctl on/off through the kernel.
sudo install -m 755 lcd-fbcon-detach /usr/local/sbin/
sudo install -m 644 lcd-fbcon-detach.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now lcd-fbcon-detach.service
for u in pwrkey-lcd lcd-splash; do sudo systemctl disable --now $u.service 2>/dev/null || true; done
if [ -e /usr/local/bin/lcd_ctl ] && [ "$(head -c 2 /usr/local/bin/lcd_ctl)" != "#!" ]; then   # still the stock binary
	sudo mv /usr/local/bin/lcd_ctl /usr/local/bin/lcd_ctl.real   # the stock binary; rollback: move it back
fi
[ -e /usr/local/bin/lcd_ctl.real ] && sudo install -m 755 lcd_ctl-wrapper /usr/local/bin/lcd_ctl
rm -f lcd-fbcon-detach lcd-fbcon-detach.service lcd_ctl-wrapper
grep -q "^fdt /dtbs/qcom/msm8916-fy-mf800-\(fixed\|lcd\).dtb" /boot/extlinux/extlinux.conf || { echo "unexpected extlinux.conf, not changing it" >&2; exit 1; }
sudo install -m 644 msm8916-fy-mf800-lcd.dtb /boot/dtbs/qcom/
sudo install -m 644 "fy,mf800-lcd.bin" /lib/firmware/
sudo install -m 644 60-mf800-lcd-spi.rules /etc/udev/rules.d/
echo panel-mipi-dbi | sudo tee /etc/modules-load.d/mf800-lcd.conf >/dev/null
rm -f msm8916-fy-mf800-lcd.dtb "fy,mf800-lcd.bin" 60-mf800-lcd-spi.rules
[ -e /boot/extlinux/extlinux.conf.headless ] || sudo cp -a /boot/extlinux/extlinux.conf /boot/extlinux/extlinux.conf.headless
sudo sed -i "s|^fdt /dtbs/qcom/msm8916-fy-mf800-fixed.dtb|fdt /dtbs/qcom/msm8916-fy-mf800-lcd.dtb|" /boot/extlinux/extlinux.conf
sync; grep ^fdt /boot/extlinux/extlinux.conf'
echo "installed; reboot the stick to use the new DTB"
