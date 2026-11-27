package main

// Thermal guard: the LTE modem heats up with data volume (measured 2026-11-22: ~3 Mbit/s took it
// from 72 to 83 C within a minute; the kernel powers the stick off at 95 C). When the hottest
// sensor crosses a threshold, client traffic through LTE is rate-limited so the modem cools while
// staying online. The kernel has no tc qdiscs (HTB/TBF/CAKE not built), so the limit is an
// nftables policer in its own table: excess packets are dropped and TCP/QUIC slow down to fit.
// The router's own traffic (DNS, tunnels, web UI) is not limited.
//
// Client traffic routed into a tunnel (WireGuard "Route LAN through", LAN IPv6 via WARP, Cloudflare
// Mesh -> LAN) leaves the radio as the router's own encrypted packets, which the forward hook never
// sees on wwan0. So the policer matches traffic between the LAN and every LTE path: wwan0 and each
// tunnel interface, one shared budget per direction. Tunnels count only while LTE is the uplink.

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type ThermalCfg struct {
	Enabled  bool `json:"enabled"`
	StartC   int  `json:"start_c"`   // limit at or above this temperature (hottest sensor)
	ReleaseC int  `json:"release_c"` // lift the limit at or below this
	DownKbit int  `json:"down_kbit"` // client download limit (LTE -> clients)
	UpKbit   int  `json:"up_kbit"`   // client upload limit (clients -> LTE); uploads heat the radio most
	// Emergency cooldown: at CooldownC the LTE radio is switched off (nmcli radio wwan off) for at least
	// CooldownMin minutes, until the stick is back at ReleaseC (or 3x CooldownMin at most), so the stick
	// cools instead of hitting the 95 °C hardware power-off. Internet and Mesh access are down meanwhile;
	// the Wi-Fi hotspot and the web UI stay up.
	Cooldown    bool `json:"cooldown"`
	CooldownC   int  `json:"cooldown_c"`
	CooldownMin int  `json:"cooldown_min"`
}

func defaultThermal() ThermalCfg {
	return ThermalCfg{Enabled: true, StartC: 85, ReleaseC: 75, DownKbit: 2000, UpKbit: 1000, Cooldown: true, CooldownC: 92, CooldownMin: 10}
}

// Level 2 ("strict", a quarter of the rates) starts strictGap degrees above StartC.
const strictGap = 5

func validateThermal(t *ThermalCfg) error {
	if t.StartC == 0 && t.ReleaseC == 0 && t.DownKbit == 0 && t.UpKbit == 0 {
		d := defaultThermal()
		d.Enabled = t.Enabled
		*t = d
	}
	if t.CooldownC == 0 && t.CooldownMin == 0 { // saved before the cooldown existed
		t.Cooldown, t.CooldownC, t.CooldownMin = true, 92, 10
	}
	switch {
	case t.StartC < 60 || t.StartC > 90:
		return badRequest("thermal guard: start temperature must be 60-90 °C (the stick powers off at 95 °C)")
	case t.ReleaseC < 40 || t.ReleaseC > t.StartC-5:
		return badRequest("thermal guard: release temperature must be at least 5 °C below the start temperature")
	case t.DownKbit < 256 || t.DownKbit > 100000 || t.UpKbit < 128 || t.UpKbit > 50000:
		return badRequest("thermal guard: limits must be 256-100000 kbit/s down and 128-50000 kbit/s up")
	case t.CooldownC <= t.StartC || t.CooldownC > 94:
		return badRequest("thermal guard: cooldown temperature must be above the start temperature and at most 94 °C")
	case t.CooldownMin < 3 || t.CooldownMin > 60:
		return badRequest("thermal guard: cooldown must last 3-60 minutes")
	}
	return nil
}

// nextThermalLevel: 0 off, 1 limited, 2 strict. Hysteresis keeps it from flapping.
func nextThermalLevel(level int, maxC float64, t ThermalCfg) int {
	if !t.Enabled {
		return 0
	}
	start, strict, release := float64(t.StartC), float64(t.StartC+strictGap), float64(t.ReleaseC)
	switch {
	case maxC >= strict:
		return 2
	case level == 2 && maxC > start-2:
		return 2
	case maxC >= start:
		return 1
	case level > 0 && maxC > release:
		return 1
	}
	return 0
}

func thermalRates(level int, t ThermalCfg) (down, up int) {
	down, up = t.DownKbit, t.UpKbit
	if level == 2 {
		down, up = max(down/4, 256), max(up/4, 128)
	}
	return
}

// thermalPaths: the interfaces whose LAN traffic crosses the LTE radio: wwan0, plus the tunnel
// interfaces that exist now unless the USB uplink carries them (USB transfers don't heat the modem).
func thermalPaths(uplink string, ifaces []string) []string {
	paths := []string{"wwan0"}
	if uplink == "usb0" || uplink == btIf { // USB or the phone over Bluetooth: tunnels don't cross the LTE radio
		return paths
	}
	for _, n := range ifaces {
		if strings.HasPrefix(n, "wg") || n == meshIf {
			paths = append(paths, n)
		}
	}
	sort.Strings(paths[1:])
	return paths
}

// netIfaces lists the interface names in /sys/class/net (cheap; no process).
func netIfaces() []string {
	ents, _ := os.ReadDir("/sys/class/net")
	out := make([]string, 0, len(ents))
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

func renderThermalGuard(level int, t ThermalCfg, lan string, paths []string) string {
	if level == 0 {
		return "table inet thermalguard\ndelete table inet thermalguard\n"
	}
	down, up := thermalRates(level, t)
	kB := func(kbit int) int { return max(kbit/8, 16) }
	q := make([]string, len(paths))
	for i, p := range paths {
		q[i] = `"` + p + `"`
	}
	set := "{ " + strings.Join(q, ", ") + " }"
	// One rule per direction, so all LTE paths share a single policer (the radio's budget).
	return fmt.Sprintf(`table inet thermalguard
delete table inet thermalguard
table inet thermalguard {
	chain limit_clients {
		type filter hook forward priority -10; policy accept;
		iifname %s oifname "%s" limit rate over %d kbytes/second burst %d kbytes counter drop comment "LTE -> clients"
		iifname "%s" oifname %s limit rate over %d kbytes/second burst %d kbytes counter drop comment "clients -> LTE"
	}
}
`, set, lan, kB(down), max(kB(down)/2, 32), lan, set, kB(up), max(kB(up)/2, 16))
}

var (
	tgMu    sync.Mutex
	tgState = map[string]any{"level": 0}
)

func maxThermalC() float64 {
	m := 0.0
	zones, _ := filepath.Glob("/sys/class/thermal/thermal_zone*")
	for _, z := range zones {
		if v, err := strconv.Atoi(readTrim(z + "/temp")); err == nil && float64(v)/1000 > m {
			m = float64(v) / 1000
		}
	}
	return m
}

func thermalSnapshot() map[string]any {
	tgMu.Lock()
	defer tgMu.Unlock()
	out := map[string]any{}
	for k, v := range tgState {
		out[k] = v
	}
	return out
}

// cooldownMarker survives reboots: NetworkManager remembers "radio wwan off" across reboots, so after
// a power loss mid-cooldown the guard must know it was the one that switched LTE off.
const cooldownMarker = "/var/lib/router/thermal-cooldown"

// cooldownStep decides on the emergency cooldown: start it, end it, or leave it.
func cooldownStep(active bool, elapsed time.Duration, maxC float64, t ThermalCfg) (start, stop bool) {
	minDur := time.Duration(t.CooldownMin) * time.Minute
	switch {
	case !active:
		return t.Enabled && t.Cooldown && maxC >= float64(t.CooldownC), false
	case !t.Enabled || !t.Cooldown:
		return false, true
	case elapsed >= 3*minDur: // give LTE another chance even if it is still warm
		return false, true
	case elapsed >= minDur && maxC <= float64(t.ReleaseC):
		return false, true
	}
	return false, false
}

// resumeSince: when a resumed cooldown counts as started. The marker holds the original start, so
// restarts don't extend the cooldown; a time in the future or too far back (the stick has no RTC,
// its clock can be off right after boot) counts from now.
func resumeSince(marker string, now time.Time, t ThermalCfg) time.Time {
	ts, err := time.Parse(time.RFC3339, marker)
	if err != nil || ts.After(now) || now.Sub(ts) > 3*time.Duration(t.CooldownMin)*time.Minute {
		return now
	}
	return ts
}

func setLTERadio(on bool) error {
	arg := map[bool]string{true: "on", false: "off"}[on]
	_, err := run(30*time.Second, "nmcli", "radio", "wwan", arg)
	return err
}

func applyThermalLevel(level int, t ThermalCfg, paths []string) error {
	_, err := runStdin(10*time.Second, renderThermalGuard(level, t, lanIf, paths), "nft", "-f", "-")
	return err
}

// thermalGuardLoop checks every 5 s. The table is re-applied every minute while limiting, in case
// something removed it; changing the settings takes effect on the next tick.
func thermalGuardLoop() {
	level, applied, lastApply, since, engaged := 0, -1, time.Time{}, time.Now(), 0
	var lastCfg ThermalCfg
	lastPaths := ""
	coolActive, coolSince, coolCount := false, time.Time{}, 0
	if b, err := os.ReadFile(cooldownMarker); err == nil { // restarted (or rebooted) during a cooldown: finish it
		coolActive, coolSince = true, resumeSince(strings.TrimSpace(string(b)), time.Now(), snapshot().Thermal)
		log.Printf("thermal guard: resuming an LTE cooldown that was in progress (started %s)", coolSince.Format("15:04:05"))
		if err := lteHold("thermal", true); err != nil {
			log.Printf("thermal guard: %v", err)
		}
	}
	for {
		t := snapshot().Thermal
		c := maxThermalC()
		if start, stop := cooldownStep(coolActive, time.Since(coolSince), c, t); start {
			log.Printf("thermal guard: %.0f °C, EMERGENCY COOLDOWN: LTE radio off for at least %d min", c, t.CooldownMin)
			_ = writeAtomic(cooldownMarker, []byte(time.Now().Format(time.RFC3339)+"\n"), 0o600)
			if err := lteHold("thermal", true); err != nil {
				log.Printf("thermal guard: %v", err)
			}
			coolActive, coolSince = true, time.Now()
			coolCount++
		} else if stop {
			log.Printf("thermal guard: %.0f °C after %s, cooldown over: LTE radio on", c, time.Since(coolSince).Round(time.Second))
			if err := lteHold("thermal", false); err != nil { // stays off if the schedule also wants it off
				log.Printf("thermal guard: %v", err)
			} else {
				_ = os.Remove(cooldownMarker)
				coolActive = false
			}
		}
		next := nextThermalLevel(level, c, t)
		if next != level {
			down, up := thermalRates(next, t)
			if next == 0 {
				log.Printf("thermal guard: %.0f °C, limit lifted after %s", c, time.Since(since).Round(time.Second))
			} else {
				log.Printf("thermal guard: %.0f °C, limiting LTE client traffic to %d/%d kbit/s (level %d)", c, down, up, next)
				if level == 0 {
					engaged++
				}
			}
			level, since = next, time.Now()
		}
		paths := thermalPaths(readWANState()["active"], netIfaces())
		pkey := strings.Join(paths, " ")
		if level != applied || t != lastCfg || (level > 0 && (pkey != lastPaths || time.Since(lastApply) > time.Minute)) {
			if err := applyThermalLevel(level, t, paths); err != nil {
				log.Printf("thermal guard: %v", err)
			} else {
				if level > 0 && pkey != lastPaths {
					log.Printf("thermal guard: limiting LAN traffic via %s", pkey)
				}
				applied, lastCfg, lastApply, lastPaths = level, t, time.Now(), pkey
			}
		}
		down, up := thermalRates(level, t)
		tgMu.Lock()
		tgState = map[string]any{"level": level, "max_c": c, "since": since.Format(time.RFC3339), "engaged": engaged,
			"down_kbit": down, "up_kbit": up, "enabled": t.Enabled, "cooldown": coolActive, "cooldowns": coolCount}
		if coolActive {
			tgState["cooldown_since"] = coolSince.Format(time.RFC3339)
		}
		tgMu.Unlock()
		time.Sleep(5 * time.Second)
	}
}

func apiPutThermal(r *http.Request) (any, error) {
	var t ThermalCfg
	if err := readJSON(r, &t); err != nil {
		return nil, err
	}
	return updateCfg(r, "Thermal guard", func(c *Config) { c.Thermal = t })
}
