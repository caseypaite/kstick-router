package main

// Bluetooth (Tools → Bluetooth): the WCN3620 Wi-Fi chip also does Bluetooth (driver btqcomsmd). Off by default
// to save RAM: the driver is blacklisted in /etc/modprobe.d/router-no-bt.conf, which this file owns. Turning it on
// removes the blacklist, loads the driver, and BlueZ (bluetoothd, and bluez-alsa if installed) starts by itself via
// bluetooth.target; ~5 MB RAM, Wi-Fi unaffected (tested 2026-11-26). The setting survives reboots.
//
// State is read in one call (busctl GetManagedObjects, JSON); actions go through bluetoothctl. Pairing uses a
// NoInputNoOutput agent ("just works"): outgoing pairing from the device list, or a 3-minute window in which the
// stick is visible and accepts pairing from a phone or headset. Devices paired in that window are trusted (they may
// reconnect by themselves).

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

type BTCfg struct {
	Enabled          bool   `json:"enabled"`
	Uplink           string `json:"uplink"`              // paired phone used as the backup uplink ("" = none; btuplink.go)
	UplinkWhenLTEOff bool   `json:"uplink_when_lte_off"` // also while LTE is off on purpose (cooldown, schedule)
}

const (
	noBTConf    = "/etc/modprobe.d/router-no-bt.conf"
	oldNoBTConf = "/etc/modprobe.d/router-no-video-bt.conf" // held the Bluetooth lines before this page existed
	noBTText    = "# Bluetooth off (router web UI, Tools -> Bluetooth): don't load the driver.\nblacklist btqcomsmd\nblacklist btqca\ninstall btqcomsmd /bin/false\n"
	btPairSecs  = 180
)

var (
	reBTMAC  = regexp.MustCompile(`^([0-9A-F]{2}:){5}[0-9A-F]{2}$`)
	btOp     sync.Mutex // one Bluetooth action at a time
	btPairMu sync.Mutex
	btPairTo time.Time // pairing window open until
)

func btLoaded() bool { return exists("/sys/module/btqcomsmd") }

// btSetup moves the Bluetooth blacklist into its own file and makes the system match the setting (at start).
func btSetup() {
	if b, err := os.ReadFile(oldNoBTConf); err == nil && (strings.Contains(string(b), "btqcomsmd") || strings.Contains(string(b), "no Bluetooth use")) {
		var keep []string
		for _, l := range strings.Split(string(b), "\n") {
			switch {
			case strings.Contains(l, "btqcomsmd") || strings.Contains(l, "btqca"):
			case strings.HasPrefix(l, "#") && strings.Contains(l, "Bluetooth"):
				keep = append(keep, "# KStick router is headless: do not load the video codec drivers.")
			default:
				keep = append(keep, l)
			}
		}
		_ = writeAtomic(oldNoBTConf, []byte(strings.Join(keep, "\n")), 0o644)
	}
	on := snapshot().Bluetooth.Enabled
	if on != btLoaded() || on == exists(noBTConf) {
		if err := btApply(on); err != nil {
			log.Printf("bluetooth: %v", err)
		}
	}
}

func btApply(on bool) error {
	if on {
		_ = os.Remove(noBTConf)
		if out, err := run(20*time.Second, "modprobe", "--ignore-install", "btqcomsmd"); err != nil {
			return fmt.Errorf("loading the Bluetooth driver: %v %s", err, strings.TrimSpace(out))
		}
		for i := 0; i < 20 && !exists("/sys/class/bluetooth/hci0"); i++ {
			time.Sleep(250 * time.Millisecond)
		}
		_, err := run(30*time.Second, "systemctl", "start", "bluetooth")
		log.Printf("bluetooth: on")
		return err
	}
	if err := writeAtomic(noBTConf, []byte(noBTText), 0o644); err != nil {
		return err
	}
	_, _ = run(30*time.Second, "systemctl", "stop", "bluealsa-aplay", "bluealsa", "bluetooth")
	if btLoaded() {
		if out, err := run(20*time.Second, "modprobe", "-r", "btqcomsmd", "btqca"); err != nil {
			return fmt.Errorf("unloading the Bluetooth driver: %v %s", err, strings.TrimSpace(out))
		}
	}
	log.Printf("bluetooth: off")
	return nil
}

// ---------------------------------------------------------------- state from BlueZ (D-Bus)

type btDevice struct {
	MAC       string `json:"mac"`
	Name      string `json:"name"`
	Icon      string `json:"icon"`
	Paired    bool   `json:"paired"`
	Trusted   bool   `json:"trusted"`
	Connected bool   `json:"connected"`
	RSSI      *int   `json:"rssi"` // only while seen by a scan
	Audio     bool   `json:"audio"`
}

// parseBlueZ reads `busctl --json=short call org.bluez / …GetManagedObjects` output.
func parseBlueZ(b []byte) (adapter map[string]any, devs []btDevice, err error) {
	var r struct {
		Data []map[string]map[string]map[string]struct {
			Data json.RawMessage `json:"data"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &r); err != nil || len(r.Data) == 0 {
		return nil, nil, fmt.Errorf("unexpected BlueZ answer")
	}
	for path, ifs := range r.Data[0] {
		if a, ok := ifs["org.bluez.Adapter1"]; ok && adapter == nil {
			adapter = map[string]any{"path": path}
			for _, k := range []string{"Address", "Alias", "Powered", "Discoverable", "Pairable", "Discovering"} {
				var v any
				_ = json.Unmarshal(a[k].Data, &v)
				adapter[strings.ToLower(k)] = v
			}
		}
		d, ok := ifs["org.bluez.Device1"]
		if !ok {
			continue
		}
		var dv btDevice
		get := func(k string, p any) { _ = json.Unmarshal(d[k].Data, p) }
		get("Address", &dv.MAC)
		get("Icon", &dv.Icon)
		get("Paired", &dv.Paired)
		get("Trusted", &dv.Trusted)
		get("Connected", &dv.Connected)
		if _, has := d["Name"]; has {
			get("Name", &dv.Name)
		}
		if raw, has := d["RSSI"]; has {
			var v int
			if json.Unmarshal(raw.Data, &v) == nil {
				dv.RSSI = &v
			}
		}
		var uuids []string
		get("UUIDs", &uuids)
		for _, u := range uuids {
			if strings.HasPrefix(u, "0000110b") || strings.HasPrefix(u, "0000110a") || strings.HasPrefix(u, "0000111e") || strings.HasPrefix(u, "00001108") {
				dv.Audio = true
			}
		}
		devs = append(devs, dv)
	}
	sort.Slice(devs, func(i, j int) bool {
		a, b := devs[i], devs[j]
		if a.Paired != b.Paired {
			return a.Paired
		}
		if (a.Name != "") != (b.Name != "") {
			return a.Name != ""
		}
		ra, rb := -999, -999
		if a.RSSI != nil {
			ra = *a.RSSI
		}
		if b.RSSI != nil {
			rb = *b.RSSI
		}
		if ra != rb {
			return ra > rb
		}
		return a.MAC < b.MAC
	})
	return adapter, devs, nil
}

func btRead() (map[string]any, []btDevice, error) {
	out, err := run(10*time.Second, "busctl", "--json=short", "call", "org.bluez", "/", "org.freedesktop.DBus.ObjectManager", "GetManagedObjects")
	if err != nil {
		return nil, nil, fmt.Errorf("BlueZ is not answering (is bluetoothd running?)")
	}
	return parseBlueZ([]byte(out))
}

func btPairingLeft() int {
	btPairMu.Lock()
	defer btPairMu.Unlock()
	return max(0, int(time.Until(btPairTo).Seconds()))
}

func apiGetBT(r *http.Request) (any, error) {
	res := map[string]any{"enabled": snapshot().Bluetooth.Enabled, "loaded": btLoaded(), "adapter_present": exists("/sys/class/bluetooth/hci0"),
		"pairing_left": btPairingLeft(), "audio": exists("/usr/bin/bluealsa"),
		"uplink": snapshot().Bluetooth.Uplink, "uplink_when_lte_off": snapshot().Bluetooth.UplinkWhenLTEOff, "uplink_state": btUplinkSnapshot()}
	if !btLoaded() {
		return res, nil
	}
	ad, devs, err := btRead()
	if err != nil {
		res["error"] = err.Error()
		return res, nil
	}
	if len(devs) > 80 {
		devs = devs[:80]
	}
	if devs == nil {
		devs = []btDevice{}
	}
	res["adapter"], res["devices"] = ad, devs
	return res, nil
}

func apiPutBT(r *http.Request) (any, error) {
	var b BTCfg
	if err := readJSON(r, &b); err != nil {
		return nil, err
	}
	if !btOp.TryLock() {
		return nil, &httpError{http.StatusConflict, "another Bluetooth action is running"}
	}
	defer btOp.Unlock()
	res, err := updateCfg(r, "Bluetooth", func(c *Config) { c.Bluetooth.Enabled = b.Enabled }, func(c Config) error {
		if err := btApply(c.Bluetooth.Enabled); err != nil {
			return err
		}
		if c.Bluetooth.Uplink != "" { // the backup uplink joins or leaves wan-failover with Bluetooth
			return applyWAN(c)
		}
		return nil
	})
	select {
	case btUpKick <- struct{}{}:
	default:
	}
	return res, err
}

// btctl runs bluetoothctl with the "just works" agent; ok is the text that means success.
func btctl(timeout time.Duration, ok string, args ...string) error {
	secs := fmt.Sprint(int(timeout.Seconds()))
	out, err := run(timeout+10*time.Second, "bluetoothctl", append([]string{"--agent", "NoInputNoOutput", "--timeout", secs}, args...)...)
	if ok != "" && strings.Contains(out, ok) {
		return nil
	}
	if err == nil && ok == "" {
		return nil
	}
	msg := ""
	for _, l := range strings.Split(out, "\n") { // the last error line bluetoothctl printed
		l = stripANSI(strings.TrimSpace(l))
		if strings.Contains(l, "Fail") || strings.Contains(l, "rror") || strings.Contains(l, "not available") {
			msg = l
		}
	}
	return badRequest("%s", firstNonEmpty(msg, fmt.Sprintf("bluetoothctl %s did not succeed", args[0])))
}

var reANSI = regexp.MustCompile(`\x1b\[[0-9;]*m|\x01|\x02`)

func stripANSI(s string) string { return reANSI.ReplaceAllString(s, "") }

func apiBTAction(r *http.Request) (any, error) {
	if !snapshot().Bluetooth.Enabled || !btLoaded() {
		return nil, badRequest("Bluetooth is off")
	}
	var b struct {
		MAC string `json:"mac"`
	}
	_ = readJSON(r, &b)
	b.MAC = strings.ToUpper(strings.TrimSpace(b.MAC))
	act := r.PathValue("action")
	needMAC := map[string]bool{"pair": true, "connect": true, "disconnect": true, "remove": true}
	if needMAC[act] && !reBTMAC.MatchString(b.MAC) {
		return nil, badRequest("invalid Bluetooth address")
	}
	if !btOp.TryLock() {
		return nil, &httpError{http.StatusConflict, "another Bluetooth action is running"}
	}
	defer btOp.Unlock()
	var err error
	msg := "Done."
	switch act {
	case "power-on", "power-off":
		err = btctl(10*time.Second, "succeeded", "power", strings.TrimPrefix(act, "power-"))
	case "scan":
		_ = btctl(12*time.Second, "", "scan", "on") // runs until the timeout; then the device list holds what it saw
		msg = "Scan finished."
	case "pairing":
		go btPairingWindow()
		msg = fmt.Sprintf("The stick is visible as \"%s\" for 3 minutes: pair from your phone or put the headset in pairing mode and press Pair here.", hostname())
	case "pair":
		if err = btctl(30*time.Second, "Pairing successful", "pair", b.MAC); err == nil {
			_ = btctl(5*time.Second, "succeeded", "trust", b.MAC)
			_ = btctl(15*time.Second, "Connection successful", "connect", b.MAC)
			msg = "Paired."
		}
	case "connect":
		err = btctl(20*time.Second, "Connection successful", "connect", b.MAC)
	case "disconnect":
		err = btctl(10*time.Second, "Successful disconnected", "disconnect", b.MAC)
	case "remove":
		err = btctl(10*time.Second, "Device has been removed", "remove", b.MAC)
	default:
		return nil, badRequest("unknown action")
	}
	if err != nil {
		return nil, err
	}
	audit(r, "bluetooth %s %s", act, b.MAC)
	return map[string]string{"message": msg}, nil
}

func hostname() string { h, _ := os.Hostname(); return h }

// btPairingWindow: visible and pairable for 3 minutes, with an agent that accepts; afterwards devices that
// paired in the window are trusted (so they may reconnect).
func btPairingWindow() {
	btPairMu.Lock()
	if time.Now().Before(btPairTo) {
		btPairMu.Unlock()
		return
	}
	btPairTo = time.Now().Add(btPairSecs * time.Second)
	btPairMu.Unlock()
	_, before, _ := btRead()
	known := map[string]bool{}
	for _, d := range before {
		if d.Paired {
			known[d.MAC] = true
		}
	}
	log.Printf("bluetooth: visible and pairable for %d s", btPairSecs)
	_, _ = run(10*time.Second, "bluetoothctl", "discoverable-timeout", fmt.Sprint(btPairSecs))
	_, _ = run(10*time.Second, "bluetoothctl", "discoverable", "on")
	// this bluetoothctl stays for the window: its agent accepts the pairing
	_, _ = run((btPairSecs+15)*time.Second, "bluetoothctl", "--agent", "NoInputNoOutput", "--timeout", fmt.Sprint(btPairSecs),
		"pairable", "on")
	_, _ = run(10*time.Second, "bluetoothctl", "discoverable", "off")
	_, _ = run(10*time.Second, "bluetoothctl", "pairable", "off")
	if _, after, err := btRead(); err == nil {
		for _, d := range after {
			if d.Paired && !known[d.MAC] {
				log.Printf("bluetooth: %s (%s) paired; trusting it", d.MAC, d.Name)
				_, _ = run(10*time.Second, "bluetoothctl", "trust", d.MAC)
			}
		}
	}
	btPairMu.Lock()
	btPairTo = time.Time{}
	btPairMu.Unlock()
}
