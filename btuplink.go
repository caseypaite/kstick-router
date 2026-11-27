package main

// Bluetooth backup uplink: a paired phone with Bluetooth tethering (PAN/NAP) becomes the last uplink in
// wan-failover (bnep0, after wwan0 and usb0). It is connected only when needed: when no other uplink has
// been healthy for 45 s (and, unless allowed, not while LTE is off on purpose: thermal cooldown or the
// LTE off schedule), and disconnected once another uplink has been healthy for 3 minutes, so the phone
// isn't kept busy and its data is only used while LTE is down. NetworkManager holds the connection
// (profile "bt-uplink", PANU, route metric 900); wan-failover health-checks it like the others.
//
// Phone data is not LTE data: the data usage meter and caps, and the thermal guard, ignore it.

import (
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	btIf           = "bnep0"
	btUplinkCon    = "bt-uplink"
	btNeedAfter    = 45 * time.Second
	btReleaseAfter = 3 * time.Minute
)

var (
	btUpMu    sync.Mutex
	btUpState = map[string]any{}
	btUpKick  = make(chan struct{}, 1)
)

func validateBTCfg(b *BTCfg) error {
	b.Uplink = strings.ToUpper(strings.TrimSpace(b.Uplink))
	if b.Uplink != "" && !reBTMAC.MatchString(b.Uplink) {
		return badRequest("Bluetooth uplink: invalid device address %q", b.Uplink)
	}
	return nil
}

// btUplinkActive: is the Bluetooth backup configured (and so part of wan-failover)?
func btUplinkActive(c Config) bool { return c.Bluetooth.Enabled && c.Bluetooth.Uplink != "" }

// wanList: the uplinks wan-failover checks, in order of preference.
func wanList(c Config) []string {
	var l []string
	if isClient(c) {
		l = append(l, "wlan0") // the joined Wi-Fi network first, LTE as the backup
	}
	for _, w := range c.WAN.Order {
		if w != "wlan0" {
			l = append(l, w)
		}
	}
	if btUplinkActive(c) {
		l = append(l, btIf)
	}
	return l
}

// ensureBTProfile creates, updates or removes the NetworkManager profile for the phone.
func ensureBTProfile(mac string) error {
	cur, err := run(10*time.Second, "nmcli", "-g", "bluetooth.bdaddr", "connection", "show", btUplinkCon)
	have := err == nil
	cur = strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(cur), `\:`, ":"))
	switch {
	case mac == "" && have:
		_, err = run(20*time.Second, "nmcli", "connection", "delete", btUplinkCon)
	case mac == "":
		return nil
	case !have:
		_, err = run(20*time.Second, "nmcli", "connection", "add", "type", "bluetooth", "con-name", btUplinkCon,
			"bluetooth.bdaddr", mac, "bluetooth.type", "panu", "connection.autoconnect", "no",
			"ipv4.route-metric", "900", "ipv6.method", "ignore")
	case cur != mac:
		_, _ = run(20*time.Second, "nmcli", "connection", "down", btUplinkCon)
		_, err = run(20*time.Second, "nmcli", "connection", "modify", btUplinkCon, "bluetooth.bdaddr", mac)
	default:
		return nil
	}
	if err != nil {
		return fmt.Errorf("Bluetooth uplink profile: %v", err)
	}
	return nil
}

// btNeed decides from the failover state whether the phone should be connected.
func btNeed(primaryUp, lteOffOnPurpose, whenLTEOff bool) bool {
	if primaryUp {
		return false
	}
	return !lteOffOnPurpose || whenLTEOff
}

func btIfUp() bool { return exists("/sys/class/net/" + btIf) }

func btUplinkLoop() {
	var needSince, okSince, lastTry, upSince time.Time
	backoff := 30 * time.Second
	lastMsg := ""
	note := func(s string) {
		if s != lastMsg {
			log.Printf("bluetooth uplink: %s", s)
			lastMsg = s
		}
	}
	for {
		c := snapshot()
		b := c.Bluetooth
		now := time.Now()
		st := map[string]any{"configured": btUplinkActive(c), "device": b.Uplink}
		connected := btIfUp()
		if !btUplinkActive(c) || !btLoaded() {
			if connected {
				_, _ = run(20*time.Second, "nmcli", "connection", "down", btUplinkCon)
				note("off (Bluetooth or the backup was switched off)")
			}
			needSince, okSince, upSince = time.Time{}, time.Time{}, time.Time{}
		} else {
			ws := readWANState()
			primaryUp := false
			for _, w := range c.WAN.Order {
				primaryUp = primaryUp || strings.HasPrefix(ws[w], "up")
			}
			offOnPurpose := exists(cooldownMarker) || exists(lteSchedMarker)
			need := btNeed(primaryUp, offOnPurpose, b.UplinkWhenLTEOff)
			if need {
				okSince = time.Time{}
				if needSince.IsZero() {
					needSince = now
				}
			} else {
				needSince = time.Time{}
				if okSince.IsZero() {
					okSince = now
				}
			}
			switch {
			case need && !connected && now.Sub(needSince) >= btNeedAfter && now.Sub(lastTry) >= backoff && uptime() > 3*time.Minute: // LTE needs a minute or two after boot
				lastTry = now
				note("no other uplink is working: connecting to the phone")
				if out, err := run(60*time.Second, "nmcli", "-w", "45", "connection", "up", btUplinkCon); err != nil {
					note(fmt.Sprintf("phone not reachable (%s); retrying in %s", firstLine(out, err), min(backoff*2, 10*time.Minute)))
					backoff = min(backoff*2, 10*time.Minute)
					st["error"] = firstLine(out, err)
				} else {
					backoff, upSince = 30*time.Second, now
					note("connected: internet through the phone")
				}
			case !need && connected && (primaryUp && now.Sub(okSince) >= btReleaseAfter || !primaryUp):
				_, _ = run(20*time.Second, "nmcli", "connection", "down", btUplinkCon)
				upSince = time.Time{}
				if primaryUp {
					note("the main uplink is back: phone disconnected")
				} else {
					note("LTE is off on purpose: phone disconnected (backup not used then)")
				}
			case !need:
				backoff = 30 * time.Second
			}
			st["needed"], st["primary_up"], st["lte_off_on_purpose"] = need, primaryUp, offOnPurpose
		}
		connected = btIfUp()
		st["connected"] = connected
		if connected {
			if upSince.IsZero() {
				upSince = now
			}
			st["since"] = upSince.Format(time.RFC3339)
			st["ip"] = ifaceIPv4(btIf)
			st["rx"], st["tx"] = readUint("/sys/class/net/"+btIf+"/statistics/rx_bytes"), readUint("/sys/class/net/"+btIf+"/statistics/tx_bytes")
		}
		st["active"] = readWANState()["active"] == btIf
		btUpMu.Lock()
		btUpState = st
		btUpMu.Unlock()
		select {
		case <-time.After(10 * time.Second):
		case <-btUpKick:
		}
	}
}

func firstLine(out string, err error) string {
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return truncRunes(strings.TrimPrefix(l, "Error: "), 120)
		}
	}
	return err.Error()
}

func readUint(p string) uint64 {
	var v uint64
	fmt.Sscan(readTrim(p), &v)
	return v
}

func btUplinkSnapshot() map[string]any {
	btUpMu.Lock()
	defer btUpMu.Unlock()
	out := map[string]any{}
	for k, v := range btUpState {
		out[k] = v
	}
	return out
}

func apiPutBTUplink(r *http.Request) (any, error) {
	var b struct {
		Device     string `json:"device"`
		WhenLTEOff bool   `json:"when_lte_off"`
	}
	if err := readJSON(r, &b); err != nil {
		return nil, err
	}
	res, err := updateCfg(r, "Bluetooth backup uplink", func(c *Config) {
		c.Bluetooth.Uplink, c.Bluetooth.UplinkWhenLTEOff = b.Device, b.WhenLTEOff
	}, func(c Config) error {
		if err := ensureBTProfile(c.Bluetooth.Uplink); err != nil {
			return err
		}
		return applyWAN(c)
	})
	select {
	case btUpKick <- struct{}{}:
	default:
	}
	return res, err
}
