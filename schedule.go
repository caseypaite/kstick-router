package main

// Schedules (local time; nothing happens until the clock is set, the stick has no RTC):
//   - LTE off: the radio is off in a daily window (coolest; no mobile data, SMS or Mesh over LTE meanwhile).
//   - Wi-Fi off: the Wi-Fi radio is off in a daily window.
//   - Reboot: weekly (or daily) at a set time.
//   - Device internet times: listed devices (by MAC) get no internet in their windows; the LAN still works.
// LTE and Wi-Fi off may not overlap, or nothing but USB could reach the stick.
//
// The radio switches are remembered in marker files (NetworkManager keeps "radio off" across reboots, so
// after a reboot inside a window that has since ended, the schedule must know it switched the radio off).
// wan-failover skips its LTE repairs while the LTE marker exists, as it does for the thermal cooldown.

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Window struct {
	Enabled bool   `json:"enabled"`
	From    string `json:"from"` // "HH:MM"
	To      string `json:"to"`   // "HH:MM"; earlier than From = overnight
}

type RebootSched struct {
	Enabled bool   `json:"enabled"`
	Day     int    `json:"day"` // 0 Sunday … 6 Saturday, 7 every day
	At      string `json:"at"`
}

type DevSched struct {
	MAC  string `json:"mac"`
	Name string `json:"name"`
	Days []int  `json:"days"` // days the window starts on (0 Sunday … 6); empty = every day
	From string `json:"from"`
	To   string `json:"to"`
}

type SchedCfg struct {
	LTEOff  Window      `json:"lte_off"`
	WifiOff Window      `json:"wifi_off"`
	Reboot  RebootSched `json:"reboot"`
	Devices []DevSched  `json:"devices"`
}

var reHHMM = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)

func hhmm(s string) int { // minutes after midnight; -1 invalid
	if !reHHMM.MatchString(s) {
		return -1
	}
	h, _ := strconv.Atoi(s[:2])
	m, _ := strconv.Atoi(s[3:])
	return h*60 + m
}

func validWindow(what, from, to string) error {
	f, t := hhmm(from), hhmm(to)
	if f < 0 || t < 0 {
		return badRequest("%s: times must be HH:MM (24 h)", what)
	}
	if f == t {
		return badRequest("%s: start and end must differ", what)
	}
	return nil
}

// inWindow: is minute-of-day m inside [from, to) (overnight when to < from)?
func inWindow(m, from, to int) bool {
	if from < to {
		return m >= from && m < to
	}
	return m >= from || m < to
}

func validateSchedule(s *SchedCfg) error {
	for _, w := range []struct {
		name string
		w    *Window
	}{{"LTE off", &s.LTEOff}, {"Wi-Fi off", &s.WifiOff}} {
		if w.w.From == "" && w.w.To == "" && !w.w.Enabled {
			continue
		}
		if err := validWindow(w.name, w.w.From, w.w.To); err != nil {
			return err
		}
	}
	if s.LTEOff.Enabled && s.WifiOff.Enabled {
		lf, lt, wf, wt := hhmm(s.LTEOff.From), hhmm(s.LTEOff.To), hhmm(s.WifiOff.From), hhmm(s.WifiOff.To)
		for m := 0; m < 24*60; m++ {
			if inWindow(m, lf, lt) && inWindow(m, wf, wt) {
				return badRequest("LTE off and Wi-Fi off overlap (from %02d:%02d): the router would be unreachable", m/60, m%60)
			}
		}
	}
	if s.Reboot.Enabled || s.Reboot.At != "" {
		if hhmm(s.Reboot.At) < 0 || s.Reboot.Day < 0 || s.Reboot.Day > 7 {
			return badRequest("reboot: pick a day and a time (HH:MM)")
		}
	}
	if len(s.Devices) > 32 {
		return badRequest("at most 32 device schedules")
	}
	for i := range s.Devices {
		d := &s.Devices[i]
		d.MAC = strings.ToLower(strings.TrimSpace(d.MAC))
		if !reMAC.MatchString(d.MAC) {
			return badRequest("device schedule %d: invalid MAC %q", i+1, d.MAC)
		}
		if err := checkComment(d.Name, fmt.Sprintf("device schedule %d", i+1)); err != nil {
			return err
		}
		if err := validWindow(fmt.Sprintf("device schedule %d", i+1), d.From, d.To); err != nil {
			return err
		}
		seen := map[int]bool{}
		for _, x := range d.Days {
			if x < 0 || x > 6 || seen[x] {
				return badRequest("device schedule %d: invalid days", i+1)
			}
			seen[x] = true
		}
		if d.Days == nil {
			d.Days = []int{}
		}
	}
	return nil
}

// devBlocked: is the device inside its no-internet window at t? Days name the day a window starts on.
func devBlocked(d DevSched, t time.Time) bool {
	m := t.Hour()*60 + t.Minute()
	f, to := hhmm(d.From), hhmm(d.To)
	if !inWindow(m, f, to) {
		return false
	}
	day := int(t.Weekday())
	if f > to && m < to { // after midnight in an overnight window: it started yesterday
		day = (day + 6) % 7
	}
	if len(d.Days) == 0 {
		return true
	}
	for _, x := range d.Days {
		if x == day {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- LTE radio: several reasons to be off

var (
	lteHoldMu sync.Mutex
	lteHolds  = map[string]bool{}
)

// lteHold records whether reason wants the LTE radio off and switches the radio to match all reasons.
func lteHold(reason string, off bool) error {
	lteHoldMu.Lock()
	defer lteHoldMu.Unlock()
	if prev, known := lteHolds[reason]; known && prev == off {
		return nil
	}
	lteHolds[reason] = off
	off = false
	for _, v := range lteHolds {
		off = off || v
	}
	return setLTERadio(!off)
}

const (
	lteSchedMarker  = "/var/lib/router/lte-schedule-off"
	wifiSchedMarker = "/var/lib/router/wifi-schedule-off"
	rebootMarker    = "/var/lib/router/scheduled-reboot"
)

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

var (
	schedMu    sync.Mutex
	schedState = map[string]any{}
)

func renderDevSched(lan string, macs []string) string {
	const head = "table inet devsched\ndelete table inet devsched\n"
	if len(macs) == 0 {
		return head
	}
	return head + fmt.Sprintf("table inet devsched {\n\tchain no_internet {\n\t\ttype filter hook forward priority -4; policy accept;\n"+
		"\t\tiifname \"%s\" oifname != \"%s\" ether saddr { %s } counter drop comment \"device internet schedule\"\n\t}\n}\n",
		lan, lan, strings.Join(macs, ", "))
}

func scheduleLoop() {
	first, lastDev := true, "-"
	for {
		c := snapshot().Schedule
		now := time.Now()
		st := map[string]any{"clock_ok": clockSane(now)}
		if clockSane(now) {
			m := now.Hour()*60 + now.Minute()
			// LTE off
			want := c.LTEOff.Enabled && inWindow(m, hhmm(c.LTEOff.From), hhmm(c.LTEOff.To))
			had := exists(lteSchedMarker)
			if want != had || (first && had) {
				if want {
					_ = writeAtomic(lteSchedMarker, []byte(now.Format(time.RFC3339)+"\n"), 0o600)
					log.Printf("schedule: LTE radio off until %s", c.LTEOff.To)
				} else {
					log.Printf("schedule: LTE off time over: LTE radio on")
				}
				if err := lteHold("schedule", want); err != nil {
					log.Printf("schedule: %v", err)
				} else if !want {
					_ = os.Remove(lteSchedMarker)
				}
			}
			st["lte_off"] = want
			// Wi-Fi off
			want = c.WifiOff.Enabled && inWindow(m, hhmm(c.WifiOff.From), hhmm(c.WifiOff.To))
			had = exists(wifiSchedMarker)
			if want != had || (first && had) {
				arg := map[bool]string{true: "off", false: "on"}[want]
				if _, err := run(30*time.Second, "nmcli", "radio", "wifi", arg); err != nil {
					log.Printf("schedule: Wi-Fi %s: %v", arg, err)
				} else if want {
					_ = writeAtomic(wifiSchedMarker, []byte(now.Format(time.RFC3339)+"\n"), 0o600)
					log.Printf("schedule: Wi-Fi off until %s", c.WifiOff.To)
				} else {
					_ = os.Remove(wifiSchedMarker)
					log.Printf("schedule: Wi-Fi off time over: Wi-Fi on")
				}
			}
			st["wifi_off"] = want
			// devices
			var macs, names []string
			for _, d := range c.Devices {
				if devBlocked(d, now) {
					macs = append(macs, d.MAC)
					names = append(names, firstNonEmpty(d.Name, d.MAC))
				}
			}
			if key := strings.Join(macs, " "); key != lastDev {
				if _, err := runStdin(10*time.Second, renderDevSched(lanIf, macs), "nft", "-f", "-"); err != nil {
					log.Printf("schedule: device rules: %v", err)
				} else {
					if len(macs) > 0 || lastDev != "-" {
						log.Printf("schedule: no internet now for: %s", firstNonEmpty(strings.Join(names, ", "), "nobody"))
					}
					lastDev = key
				}
			}
			st["devices_blocked"] = names
			// reboot: at the set minute, at most once in 2 h, and not within an hour of boot (no loops)
			r := c.Reboot
			if r.Enabled && m == hhmm(r.At) && (r.Day == 7 || r.Day == int(now.Weekday())) && uptime() > time.Hour {
				last, _ := time.Parse(time.RFC3339, readTrim(rebootMarker))
				if now.Sub(last) > 2*time.Hour || now.Before(last) {
					_ = writeAtomic(rebootMarker, []byte(now.Format(time.RFC3339)), 0o600)
					log.Printf("schedule: weekly reboot")
					flushState()
					_, _ = run(10*time.Second, "systemctl", "reboot")
				}
			}
			first = false
		}
		schedMu.Lock()
		schedState = st
		schedMu.Unlock()
		select {
		case <-time.After(time.Until(time.Now().Truncate(time.Minute).Add(time.Minute + 2*time.Second))):
		case <-schedKick:
		}
	}
}

func apiGetSchedule(r *http.Request) (any, error) {
	schedMu.Lock()
	st := schedState
	schedMu.Unlock()
	return map[string]any{"config": snapshot().Schedule, "state": st, "last_reboot": readTrim(rebootMarker)}, nil
}

var schedKick = make(chan struct{}, 1)

func apiPutSchedule(r *http.Request) (any, error) {
	var s SchedCfg
	if err := readJSON(r, &s); err != nil {
		return nil, err
	}
	res, err := updateCfg(r, "schedules", func(c *Config) { c.Schedule = s })
	select {
	case schedKick <- struct{}{}:
	default:
	}
	return res, err
}
