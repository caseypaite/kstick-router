package main

// LTE data usage: daily totals of everything that crosses the radio (the router's own traffic too, as the
// operator counts it), a daily and/or monthly cap with a warning, and an optional action at the cap:
// slow LAN traffic down, or cut the LAN off from LTE (the router itself, Mesh and the web UI keep working).
//
// The modem driver keeps no byte counters for wwan0, so the bytes come from the router firewall's nft
// counters lte_rx/lte_tx (render.go). They restart at 0 when the firewall is reloaded or the stick boots:
// applyFirewall takes a reading just before it reloads, and a counter that went down counts from 0.
// Totals are written to flash at most every 5 minutes (and at day changes) to spare it.

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

type DataCfg struct {
	DailyMB   int    `json:"daily_mb"`   // 0 = no daily cap
	MonthlyMB int    `json:"monthly_mb"` // 0 = no monthly cap
	CycleDay  int    `json:"cycle_day"`  // the monthly period starts on this day of the month (1-28)
	WarnPct   int    `json:"warn_pct"`   // warn at this share of a cap
	Action    string `json:"action"`     // at the cap: none | limit | block
	LimitKbit int    `json:"limit_kbit"` // speed for "limit", each direction
}

func defaultData() DataCfg {
	return DataCfg{CycleDay: 1, WarnPct: 80, Action: "none", LimitKbit: 256}
}

func validateData(d *DataCfg) error {
	if d.CycleDay == 0 && d.WarnPct == 0 && d.Action == "" { // saved before this section existed
		*d = defaultData()
	}
	switch {
	case d.DailyMB < 0 || d.DailyMB > 1_000_000 || d.MonthlyMB < 0 || d.MonthlyMB > 10_000_000:
		return badRequest("data caps must be 0 (none) or a positive number of MB")
	case d.CycleDay < 1 || d.CycleDay > 28:
		return badRequest("the monthly period must start on day 1-28")
	case d.WarnPct < 50 || d.WarnPct > 99:
		return badRequest("warn at 50-99 %% of a cap")
	case d.Action != "none" && d.Action != "limit" && d.Action != "block":
		return badRequest("action at the cap must be none, limit or block")
	case d.LimitKbit < 64 || d.LimitKbit > 100000:
		return badRequest("the limited speed must be 64-100000 kbit/s")
	}
	return nil
}

const (
	usagePath     = "/var/lib/router/datausage.json"
	usageKeepDays = 100
	usageSaveGap  = 5 * time.Minute
)

type usagePair struct {
	RX uint64 `json:"rx"`
	TX uint64 `json:"tx"`
}

func (p usagePair) total() uint64 { return p.RX + p.TX }

type usageFile struct {
	Days     map[string]usagePair `json:"days"` // local date "2006-01-02"
	BootID   string               `json:"boot_id"`
	LastRX   uint64               `json:"last_rx"` // nft counters at the last reading
	LastTX   uint64               `json:"last_tx"`
	Notified map[string]string    `json:"notified"` // what was already announced in which period
}

var (
	usageMu      sync.Mutex
	usage        = usageFile{Days: map[string]usagePair{}, Notified: map[string]string{}}
	usageLoaded  bool
	usageSaved   time.Time
	usageDirty   bool
	usagePending usagePair // bytes counted while the clock was not yet set (no RTC; before NTP)
	usageKick    = make(chan struct{}, 1)
	usageState   = map[string]any{}
)

func clockSane(t time.Time) bool { return t.Year() >= 2025 }

func bootID() string { return readTrim("/proc/sys/kernel/random/boot_id") }

func loadUsage() {
	if usageLoaded {
		return
	}
	usageLoaded = true
	if b, err := os.ReadFile(usagePath); err == nil {
		var f usageFile
		if json.Unmarshal(b, &f) == nil {
			usage = f
		}
	}
	if usage.Days == nil {
		usage.Days = map[string]usagePair{}
	}
	if usage.Notified == nil {
		usage.Notified = map[string]string{}
	}
	if usage.BootID != bootID() { // rebooted: the nft counters started again at 0
		usage.BootID, usage.LastRX, usage.LastTX = bootID(), 0, 0
	}
}

// addUsage accounts a new counter reading (usageMu held).
func addUsage(cur ifSample, now time.Time) {
	var d usagePair
	if cur.RX >= usage.LastRX && cur.TX >= usage.LastTX {
		d = usagePair{cur.RX - usage.LastRX, cur.TX - usage.LastTX}
	} else { // the firewall was reloaded: the counters restarted at 0
		d = usagePair{cur.RX, cur.TX}
	}
	usage.LastRX, usage.LastTX = cur.RX, cur.TX
	histAddBytes(d)
	if !clockSane(now) {
		usagePending.RX += d.RX
		usagePending.TX += d.TX
		return
	}
	d.RX += usagePending.RX
	d.TX += usagePending.TX
	usagePending = usagePair{}
	if d.total() == 0 {
		return
	}
	day := now.Format("2006-01-02")
	p := usage.Days[day]
	p.RX += d.RX
	p.TX += d.TX
	usage.Days[day] = p
	usageDirty = true
}

func saveUsage(now time.Time, force bool) {
	if !usageDirty || (!force && now.Sub(usageSaved) < usageSaveGap) {
		return
	}
	if len(usage.Days) > usageKeepDays {
		keys := make([]string, 0, len(usage.Days))
		for k := range usage.Days {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys[:len(keys)-usageKeepDays] {
			delete(usage.Days, k)
		}
	}
	b, _ := json.Marshal(usage)
	if err := writeAtomic(usagePath, b, 0o600); err != nil {
		log.Printf("data usage: %v", err)
		return
	}
	usageSaved, usageDirty = now, false
}

// usageCheckpoint takes a counter reading now; applyFirewall calls it right before a reload resets them.
func usageCheckpoint() {
	c, ok := lteCounters()
	if !ok {
		return
	}
	usageMu.Lock()
	defer usageMu.Unlock()
	loadUsage()
	addUsage(c, time.Now()) // after the reload the counters are lower than this reading: counted from 0
}

// cycleStart: the first day of the monthly period that contains now.
func cycleStart(now time.Time, day int) time.Time {
	y, m, d := now.Date()
	if d < day {
		m--
	}
	return time.Date(y, m, day, 0, 0, 0, 0, now.Location())
}

// usageTotals: today's and this period's usage (usageMu held).
func usageTotals(now time.Time, cycleDay int) (today, month usagePair, start time.Time) {
	today = usage.Days[now.Format("2006-01-02")]
	start = cycleStart(now, cycleDay)
	from := start.Format("2006-01-02")
	for k, p := range usage.Days {
		if k >= from {
			month.RX += p.RX
			month.TX += p.TX
		}
	}
	return
}

const mb = 1 << 20

// capLevel: 0 below the warning, 1 warning, 2 at or over the cap; which says which cap, and period
// identifies the day or month (so each is announced once).
func capLevel(c DataCfg, today, month usagePair, now, start time.Time) (level int, which, period string) {
	check := func(used uint64, capMB int, name, per string) {
		if capMB <= 0 {
			return
		}
		l := 0
		switch {
		case used >= uint64(capMB)*mb:
			l = 2
		case used*100 >= uint64(capMB)*mb*uint64(c.WarnPct):
			l = 1
		}
		if l > level {
			level, which, period = l, name, per
		}
	}
	check(month.total(), c.MonthlyMB, "monthly", start.Format("2006-01-02"))
	check(today.total(), c.DailyMB, "daily", now.Format("2006-01-02"))
	return
}

// renderDataCap: the nft table that enforces the cap on LAN <-> LTE traffic (paths as for the thermal
// guard). The router's own traffic is untouched, so Mesh, the web UI and SMS control keep working.
func renderDataCap(action string, kbit int, lan string, paths []string) string {
	const head = "table inet datacap\ndelete table inet datacap\n"
	if action != "limit" && action != "block" {
		return head
	}
	q := make([]string, len(paths))
	for i, p := range paths {
		q[i] = `"` + p + `"`
	}
	set := "{ " + strings.Join(q, ", ") + " }"
	var rules string
	if action == "block" {
		rules = fmt.Sprintf("\t\tiifname \"%s\" oifname %s counter drop comment \"data cap: LAN cut off from LTE\"\n"+
			"\t\tiifname %s oifname \"%s\" counter drop comment \"data cap\"\n", lan, set, set, lan)
	} else {
		kB := max(kbit/8, 8)
		rules = fmt.Sprintf("\t\tiifname %s oifname \"%s\" limit rate over %d kbytes/second burst %d kbytes counter drop comment \"data cap: LTE -> clients\"\n"+
			"\t\tiifname \"%s\" oifname %s limit rate over %d kbytes/second burst %d kbytes counter drop comment \"data cap: clients -> LTE\"\n",
			set, lan, kB, max(kB/2, 8), lan, set, kB, max(kB/2, 8))
	}
	return head + "table inet datacap {\n\tchain cap_clients {\n\t\ttype filter hook forward priority -5; policy accept;\n" + rules + "\t}\n}\n"
}

func fmtMB(b uint64) string {
	if b >= 1<<30 {
		return fmt.Sprintf("%.2f GB", float64(b)/(1<<30))
	}
	return fmt.Sprintf("%.0f MB", float64(b)/mb)
}

// dataSummaryText: one line for SMS replies and notifications.
func dataSummaryText() string {
	c := snapshot().Data
	now := time.Now()
	usageMu.Lock()
	loadUsage()
	today, month, _ := usageTotals(now, c.CycleDay)
	usageMu.Unlock()
	s := "today " + fmtMB(today.total())
	if c.DailyMB > 0 {
		s += "/" + fmtMB(uint64(c.DailyMB)*mb)
	}
	s += ", period " + fmtMB(month.total())
	if c.MonthlyMB > 0 {
		s += "/" + fmtMB(uint64(c.MonthlyMB)*mb)
	}
	return s
}

func kickUsage() {
	select {
	case usageKick <- struct{}{}:
	default:
	}
}

func usageLoop() {
	t := time.NewTicker(time.Minute)
	applied, lastKey := "none", "-" // at start nothing is restricted
	for {
		now := time.Now()
		c := snapshot().Data
		cur, ok := lteCounters()
		usageMu.Lock()
		loadUsage()
		if ok {
			addUsage(cur, now)
		}
		today, month, start := usageTotals(now, c.CycleDay)
		level, which, period := capLevel(c, today, month, now, start)
		var notify string
		if clockSane(now) && level > 0 {
			key := fmt.Sprintf("%s-%d", which, level)
			if usage.Notified[key] != period {
				usage.Notified[key] = period
				usageDirty = true
				if level == 2 {
					notify = fmt.Sprintf("KStick: %s data cap reached (%s).", which, dataCapUsed(which, today, month, c))
					switch c.Action {
					case "limit":
						notify += fmt.Sprintf(" LAN slowed to %d kbit/s.", c.LimitKbit)
					case "block":
						notify += " LAN internet blocked until the cap resets."
					}
				} else {
					notify = fmt.Sprintf("KStick: %d%% of the %s data cap used (%s).", c.WarnPct, which, dataCapUsed(which, today, month, c))
				}
			}
		}
		saveUsage(now, now.Format("2006-01-02") != usageSaved.Format("2006-01-02"))
		usageMu.Unlock()

		action := "none"
		if level == 2 {
			action = c.Action
		}
		paths := thermalPaths(readWANState()["active"], netIfaces())
		key := fmt.Sprintf("%s %d %s", action, c.LimitKbit, strings.Join(paths, " "))
		if key != lastKey {
			if _, err := runStdin(10*time.Second, renderDataCap(action, c.LimitKbit, lanIf, paths), "nft", "-f", "-"); err != nil {
				log.Printf("data cap: %v", err)
			} else {
				if action != applied {
					if action == "none" {
						log.Printf("data cap: LAN restrictions lifted")
					} else {
						log.Printf("data cap: %s cap reached, LAN traffic: %s", which, action)
					}
				}
				applied, lastKey = action, key
			}
		}
		if notify != "" {
			log.Printf("data usage: %s", notify)
			go smsNotify(notify)
		}
		usageMu.Lock()
		usageState = map[string]any{"level": level, "which": which, "enforcing": applied, "counters": ok}
		usageMu.Unlock()
		select {
		case <-t.C:
		case <-usageKick:
		}
	}
}

func dataCapUsed(which string, today, month usagePair, c DataCfg) string {
	if which == "daily" {
		return fmtMB(today.total()) + " of " + fmtMB(uint64(c.DailyMB)*mb)
	}
	return fmtMB(month.total()) + " of " + fmtMB(uint64(c.MonthlyMB)*mb)
}

// dataSummary for /api/status and the dashboard.
func dataSummary() map[string]any {
	c := snapshot().Data
	now := time.Now()
	usageMu.Lock()
	defer usageMu.Unlock()
	loadUsage()
	today, month, start := usageTotals(now, c.CycleDay)
	st := map[string]any{}
	for k, v := range usageState {
		st[k] = v
	}
	return map[string]any{"today": today, "period": month, "period_start": start.Format("2006-01-02"),
		"daily_mb": c.DailyMB, "monthly_mb": c.MonthlyMB, "warn_pct": c.WarnPct, "action": c.Action, "state": st}
}

func apiGetData(r *http.Request) (any, error) {
	c := snapshot().Data
	now := time.Now()
	res := dataSummary()
	usageMu.Lock()
	days := []map[string]any{}
	for i := 34; i >= 0; i-- {
		d := now.AddDate(0, 0, -i).Format("2006-01-02")
		p := usage.Days[d]
		days = append(days, map[string]any{"date": d, "rx": p.RX, "tx": p.TX})
	}
	usageMu.Unlock()
	res["config"], res["days"], res["clock_ok"] = c, days, clockSane(now)
	res["period_end"] = cycleStart(now, c.CycleDay).AddDate(0, 1, -1).Format("2006-01-02")
	return res, nil
}

func apiPutData(r *http.Request) (any, error) {
	var d DataCfg
	if err := readJSON(r, &d); err != nil {
		return nil, err
	}
	res, err := updateCfg(r, "data usage caps", func(c *Config) { c.Data = d })
	kickUsage()
	return res, err
}
