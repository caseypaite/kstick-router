package main

// Battery protection (MF800; the KStick dongle has no battery): on battery power, a warning at WarnPct
// (logged, and sent by SMS when SMS remote control notifications are on), power saving at SavePct (CPU
// capped at 200 MHz, the screen sleeps after 15 s) and a clean shutdown at OffPct, so the stick never
// dies mid-write. The PM8916 "VM" fuel gauge estimates the charge from the voltage, which dips under
// load: the shutdown needs three readings in a row (90 s) at or below OffPct, and never runs in the
// first 3 minutes after boot. Power from USB ends power saving at once.
//
// Also here: the hardware watchdog status for the System page (deploy/watchdog-router.conf arms it).

import (
	"bufio"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type BatteryCfg struct {
	Enabled bool `json:"enabled"`
	WarnPct int  `json:"warn_pct"`
	SavePct int  `json:"save_pct"`
	OffPct  int  `json:"off_pct"`
}

func defaultBattery() BatteryCfg {
	return BatteryCfg{Enabled: true, WarnPct: 20, SavePct: 10, OffPct: 5}
}

func validateBattery(b *BatteryCfg) error {
	if b.WarnPct == 0 && b.SavePct == 0 && b.OffPct == 0 { // saved before this section existed
		*b = defaultBattery()
	}
	if b.OffPct < 3 || b.SavePct <= b.OffPct || b.WarnPct <= b.SavePct || b.WarnPct > 50 {
		return badRequest("battery: need 3 <= shutdown < power saving < warning <= 50 %%")
	}
	return nil
}

const (
	batInterval  = 30 * time.Second
	batOffStreak = 3
	batBootGrace = 3 * time.Minute
	batSaveMHz   = 200000 // kHz
)

// batterySaving: the screen sleeps sooner (lcd.go).
var batterySaving atomic.Bool

var (
	batMu    sync.Mutex
	batState = map[string]any{}
)

type batStep struct {
	level     int  // 0 ok, 1 warning, 2 power saving, 3 shutdown
	save      bool // power saving should be on
	warnNow   bool // announce the warning now
	offStreak int
}

type batMemory struct {
	saving, warned bool
	offStreak      int
}

// batDecide is the pure decision for one reading.
func batDecide(c BatteryCfg, pct int, onPower bool, uptime time.Duration, m batMemory) (batStep, batMemory) {
	var s batStep
	if !c.Enabled || pct < 0 || onPower {
		if onPower || pct > c.WarnPct+5 {
			m.warned = false
		}
		m.saving, m.offStreak = false, 0
		return s, m
	}
	switch {
	case pct <= c.OffPct:
		s.level = 3
	case pct <= c.SavePct:
		s.level = 2
	case pct <= c.WarnPct:
		s.level = 1
	}
	if s.level >= 1 && !m.warned {
		s.warnNow, m.warned = true, true
	}
	if pct > c.WarnPct+5 {
		m.warned = false
	}
	if s.level >= 2 {
		m.saving = true
	} else if pct >= c.SavePct+5 {
		m.saving = false
	}
	if s.level == 3 && uptime >= batBootGrace {
		m.offStreak++
	} else {
		m.offStreak = 0
	}
	s.save, s.offStreak = m.saving, m.offStreak
	return s, m
}

func uptime() time.Duration {
	f := strings.Fields(readTrim("/proc/uptime") + " 0")
	v, _ := strconv.ParseFloat(f[0], 64)
	return time.Duration(v * float64(time.Second))
}

func setCPUMax(kHz int) {
	ps, _ := filepath.Glob("/sys/devices/system/cpu/cpufreq/policy*")
	for _, p := range ps {
		_ = os.WriteFile(p+"/scaling_max_freq", []byte(strconv.Itoa(kHz)), 0o644)
	}
}

func batteryLoop() {
	if batteryInfo() == nil {
		return // no battery (KStick)
	}
	var m batMemory
	saving := false
	for {
		c := snapshot().Battery
		b := batteryInfo()
		pct := -1
		if v, ok := b["percent"].(int); ok {
			pct = v
		}
		onPower := b["charging"] == true || b["external_power"] == true
		var s batStep
		s, m = batDecide(c, pct, onPower, uptime(), m)
		if s.warnNow {
			msg := fmt.Sprintf("KStick battery low: %d%%. Power saving at %d%%, shutdown at %d%%.", pct, c.SavePct, c.OffPct)
			log.Printf("battery: %s", msg)
			go smsNotify(msg)
		}
		if s.save != saving {
			saving = s.save
			batterySaving.Store(saving)
			if saving {
				log.Printf("battery: %d%%, power saving on (CPU max 200 MHz, screen sleeps after 15 s)", pct)
				setCPUMax(batSaveMHz)
			} else {
				log.Printf("battery: power saving off (%d%%, external power %v)", pct, onPower)
				if _, err := run(20*time.Second, "systemctl", "restart", "router-cpufreq"); err != nil {
					log.Printf("battery: restoring the CPU clock: %v", err)
				}
			}
		}
		batMu.Lock()
		batState = map[string]any{"level": s.level, "saving": saving, "percent": pct, "on_power": onPower, "off_streak": s.offStreak}
		batMu.Unlock()
		if s.offStreak >= batOffStreak {
			msg := fmt.Sprintf("KStick battery at %d%%: shutting down now. Plug in USB power and press the power key to start it again.", pct)
			log.Printf("battery: %s", msg)
			done := make(chan struct{})
			go func() { smsNotify(msg); close(done) }()
			select { // the notification may take a while; the battery may not
			case <-done:
			case <-time.After(60 * time.Second):
			}
			flushState()
			if _, err := run(10*time.Second, "systemctl", "--no-block", "poweroff"); err != nil {
				log.Printf("battery: poweroff: %v", err)
			}
			return
		}
		time.Sleep(batInterval)
	}
}

func batterySnapshot() map[string]any {
	batMu.Lock()
	defer batMu.Unlock()
	out := map[string]any{}
	for k, v := range batState {
		out[k] = v
	}
	return out
}

// flushState writes what is kept in memory before a planned reboot or shutdown.
func flushState() {
	usageMu.Lock()
	saveUsage(time.Now(), true)
	usageMu.Unlock()
	saveHistory()
}

func apiPutBattery(r *http.Request) (any, error) {
	var b BatteryCfg
	if err := readJSON(r, &b); err != nil {
		return nil, err
	}
	return updateCfg(r, "battery protection", func(c *Config) { c.Battery = b })
}

// ---------------------------------------------------------------- hardware watchdog

// watchdogInfo: the watchdog device, whether systemd is petting it, and whether it reset the stick
// last time (from the PMIC reset reason that kstick-blackbox logs at every boot).
func watchdogInfo() map[string]any {
	d := "/sys/class/watchdog/watchdog0"
	if _, err := os.Stat(d); err != nil {
		return map[string]any{"present": false}
	}
	to, _ := strconv.Atoi(readTrim(d + "/timeout"))
	res := map[string]any{"present": true, "identity": readTrim(d + "/identity"), "state": readTrim(d + "/state"), "timeout_s": to}
	if line := lastBootReason("/var/log/kstick-blackbox.log"); line != "" {
		res["boot_reason"] = line
		res["watchdog_reset"] = strings.Contains(line, "PMIC_WD")
	}
	return res
}

// lastBootReason: the reset-reason line after the last "=== BOOT" in the black box log.
func lastBootReason(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() > 256<<10 {
		_, _ = f.Seek(st.Size()-256<<10, 0)
	}
	last, boot := "", false
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 64<<10)
	for sc.Scan() {
		l := sc.Text()
		switch {
		case strings.HasPrefix(l, "=== BOOT"):
			boot = true
		case boot && strings.Contains(l, "PON="):
			last, boot = strings.TrimSpace(l), false
		default:
			boot = false
		}
	}
	return last
}
