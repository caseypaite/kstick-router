package main

import (
	"strings"
	"sync"
	"time"

	"rsc.io/qr"
)

// Wi-Fi join QR code for the status screen. It is cached by its encoded text, so a new code is built only
// when the SSID, password, security or hidden flag differs. The settings are re-read from NetworkManager at
// most every 30 s while the QR page is showing, and at once after the web UI changes them (lcdWifiChanged).

type lcdWifiState struct {
	SSID string
	Code *qr.Code // nil: no hotspot settings (or encoding failed)
}

var lcdWifi struct {
	sync.Mutex
	key   string
	st    lcdWifiState
	at    time.Time
	stale bool
}

// lcdWifiChanged: the hotspot settings were just changed; rebuild the QR on the next draw.
func lcdWifiChanged() {
	lcdWifi.Lock()
	lcdWifi.stale = true
	lcdWifi.Unlock()
}

func lcdWifiQR() lcdWifiState {
	lcdWifi.Lock()
	defer lcdWifi.Unlock()
	if !lcdWifi.stale && time.Since(lcdWifi.at) < 30*time.Second {
		return lcdWifi.st
	}
	lcdWifi.at, lcdWifi.stale = time.Now(), false
	m, err := nmGet(hotspotCon, "802-11-wireless.ssid", "802-11-wireless-security.psk",
		"802-11-wireless-security.key-mgmt", "802-11-wireless.hidden")
	if err != nil || m["802-11-wireless.ssid"] == "" {
		lcdWifi.key, lcdWifi.st = "", lcdWifiState{}
		return lcdWifi.st
	}
	ssid := m["802-11-wireless.ssid"]
	key := wifiQRText(ssid, m["802-11-wireless-security.psk"], m["802-11-wireless-security.key-mgmt"],
		m["802-11-wireless.hidden"] == "yes")
	if key == lcdWifi.key {
		return lcdWifi.st
	}
	lcdWifi.key, lcdWifi.st = key, lcdWifiState{SSID: ssid}
	lcdWifi.st.Code = wifiQRCode(key)
	return lcdWifi.st
}

// wifiQRCode: the smallest code (fewest modules = biggest pixels on the 128 px panel). Level L (7 % recovery) is
// enough on a screen, which can't get smudged or torn; M is used when it is no bigger.
func wifiQRCode(text string) *qr.Code {
	var best *qr.Code
	for _, lvl := range []qr.Level{qr.M, qr.L} {
		if c, err := qr.Encode(text, lvl); err == nil && (best == nil || c.Size < best.Size) {
			best = c
		}
	}
	return best
}

// wifiQRText: the de-facto standard Wi-Fi join string (read by the Android and iOS cameras).
func wifiQRText(ssid, psk, keyMgmt string, hidden bool) string {
	esc := strings.NewReplacer(`\`, `\\`, `;`, `\;`, `,`, `\,`, `:`, `\:`, `"`, `\"`)
	var b strings.Builder
	b.WriteString("WIFI:")
	switch {
	case keyMgmt == "" || keyMgmt == "none" && psk == "":
		b.WriteString("T:nopass;")
	default: // wpa-psk, sae (WPA3) and transition modes all join as "WPA"
		b.WriteString("T:WPA;")
	}
	b.WriteString("S:" + esc.Replace(ssid) + ";")
	if keyMgmt != "" && psk != "" {
		b.WriteString("P:" + esc.Replace(psk) + ";")
	}
	if hidden {
		b.WriteString("H:true;")
	}
	b.WriteString(";")
	return b.String()
}
