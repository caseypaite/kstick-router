# To implement

Open items. Done so far: 1-11 (data usage caps, confirm-or-revert,
SMS remote control, hardware watchdog, battery protection, history, signal & bands, schedules, no duplicate gateway sends,
inbound API and delivery reports, webhook/Telegram/ntfy forwarding) and the audit leftovers.

## Network

- [ ] **12. Ethernet WAN through the USB dock:** the dock's `enx*` port as a third failover uplink in `wan-failover`.
- [ ] **13. Speed limits per device:** nft policers by MAC, like the thermal guard and the data cap.
- [ ] **14. Hairpin NAT:** port forwards that also work from inside the LAN.

## Not yet tried on the stick (implemented, unit-tested)

- [ ] Data cap enforcement (limit / block) with a small cap.
- [ ] LTE band lock + confirm/revert.
- [ ] Schedules: LTE off, Wi-Fi off, regular reboot, device internet times.
- [ ] Battery protection thresholds (needs a discharge).
- [ ] Watchdog actually resetting a hung stick (needs a deliberate hang).
- [ ] LCD signal screen on the real panel.
- [ ] Bluetooth page: switching on from the UI, pairing (outgoing and the 3-minute window), connect/disconnect.
- [ ] Bluetooth backup uplink: phone tethering taking over when LTE is down, and letting go when it is back.
- [ ] Delivery reports from the carrier (status-report SMS matched to the sent message).
- [ ] Webhook / Telegram / ntfy forwarding against the real services.
- [ ] A multi-part SMS left incomplete for 6 hours.

## Not planned

- Guest Wi-Fi (the Wi-Fi chip resets with two virtual interfaces), scheduled speed tests (heat, data),
  large ad-block lists (RAM), anything involving voice calls.
