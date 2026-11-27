#!/bin/sh
# Run on the router as root, from the directory holding these files.
set -e
install -m 0755 mesh-lan-gw /usr/local/sbin/mesh-lan-gw
install -m 0644 mesh-lan-gw.service mesh-lan-gw.path mesh-lan-gw.timer /etc/systemd/system/
systemctl daemon-reload
/usr/local/sbin/mesh-lan-gw apply
systemctl enable --now mesh-lan-gw.path mesh-lan-gw.timer
systemctl enable mesh-lan-gw.service
/usr/local/sbin/mesh-lan-gw status
