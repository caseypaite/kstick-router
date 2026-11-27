package main

import (
	"strings"
	"testing"
	"time"
)

// ---- data usage

func resetUsage() {
	usage = usageFile{Days: map[string]usagePair{}, Notified: map[string]string{}}
	usagePending, usageDirty, usageLoaded = usagePair{}, false, true
}

func TestAddUsage(t *testing.T) {
	resetUsage()
	day := time.Date(2026, 11, 26, 10, 0, 0, 0, time.Local)
	addUsage(ifSample{RX: 1000, TX: 100}, day)
	addUsage(ifSample{RX: 1500, TX: 300}, day)
	if p := usage.Days["2026-11-26"]; p.RX != 1500 || p.TX != 300 {
		t.Fatalf("plain growth: %+v", p)
	}
	addUsage(ifSample{RX: 200, TX: 50}, day) // firewall reloaded: counters restarted
	if p := usage.Days["2026-11-26"]; p.RX != 1700 || p.TX != 350 {
		t.Fatalf("after counter reset: %+v", p)
	}
	// clock not set yet (no RTC): held back, then booked on the first sane reading
	addUsage(ifSample{RX: 700, TX: 150}, time.Date(1970, 1, 1, 0, 1, 0, 0, time.Local))
	if usagePending.RX != 500 || usagePending.TX != 100 {
		t.Fatalf("pending: %+v", usagePending)
	}
	addUsage(ifSample{RX: 800, TX: 150}, day.Add(24*time.Hour))
	if p := usage.Days["2026-11-27"]; p.RX != 600 || p.TX != 100 {
		t.Fatalf("pending booked: %+v", p)
	}
}

func TestCycleStart(t *testing.T) {
	cases := []struct {
		now  string
		day  int
		want string
	}{
		{"2026-11-26", 1, "2026-11-01"},
		{"2026-11-26", 26, "2026-11-26"},
		{"2026-11-25", 26, "2026-10-26"},
		{"2026-01-05", 10, "2025-12-10"}, // across the year
	}
	for _, c := range cases {
		now, _ := time.ParseInLocation("2006-01-02", c.now, time.Local)
		if got := cycleStart(now.Add(12*time.Hour), c.day).Format("2006-01-02"); got != c.want {
			t.Errorf("cycleStart(%s, %d) = %s, want %s", c.now, c.day, got, c.want)
		}
	}
}

func TestUsageTotalsAndCaps(t *testing.T) {
	resetUsage()
	usage.Days = map[string]usagePair{
		"2026-10-31": {RX: 900 * mb}, // previous period
		"2026-11-01": {RX: 300 * mb, TX: 100 * mb},
		"2026-11-26": {RX: 1000 * mb, TX: 250 * mb},
	}
	now := time.Date(2026, 11, 26, 20, 0, 0, 0, time.Local)
	today, month, start := usageTotals(now, 1)
	if today.total() != 1250*mb || month.total() != 1650*mb || start.Day() != 1 {
		t.Fatalf("totals: today %d month %d start %s", today.total()/mb, month.total()/mb, start)
	}
	c := defaultData()
	if l, _, _ := capLevel(c, today, month, now, start); l != 0 {
		t.Errorf("no caps: level %d", l)
	}
	c.DailyMB = 1500 // 1250/1500 = 83 % >= 80 %
	if l, w, p := capLevel(c, today, month, now, start); l != 1 || w != "daily" || p != "2026-11-26" {
		t.Errorf("daily warning: %d %s %s", l, w, p)
	}
	c.MonthlyMB = 1600 // over
	if l, w, p := capLevel(c, today, month, now, start); l != 2 || w != "monthly" || p != "2026-11-01" {
		t.Errorf("monthly cap: %d %s %s", l, w, p)
	}
}

func TestValidateData(t *testing.T) {
	d := DataCfg{}
	if err := validateData(&d); err != nil || d != defaultData() {
		t.Errorf("old config gets defaults: %+v %v", d, err)
	}
	for _, bad := range []DataCfg{
		{CycleDay: 29, WarnPct: 80, Action: "none", LimitKbit: 256},
		{CycleDay: 1, WarnPct: 100, Action: "none", LimitKbit: 256},
		{CycleDay: 1, WarnPct: 80, Action: "drop", LimitKbit: 256},
		{CycleDay: 1, WarnPct: 80, Action: "limit", LimitKbit: 10},
		{DailyMB: -1, CycleDay: 1, WarnPct: 80, Action: "none", LimitKbit: 256},
	} {
		if validateData(&bad) == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

func TestRenderDataCap(t *testing.T) {
	if r := renderDataCap("none", 256, "wlan0", []string{"wwan0"}); strings.Contains(r, "chain") || !strings.Contains(r, "delete table inet datacap") {
		t.Errorf("none:\n%s", r)
	}
	r := renderDataCap("block", 256, "br-lan", []string{"wwan0", "wg-warp"})
	if !strings.Contains(r, `iifname "br-lan" oifname { "wwan0", "wg-warp" } counter drop`) {
		t.Errorf("block:\n%s", r)
	}
	r = renderDataCap("limit", 256, "wlan0", []string{"wwan0"})
	if !strings.Contains(r, `iifname { "wwan0" } oifname "wlan0" limit rate over 32 kbytes/second`) {
		t.Errorf("limit:\n%s", r)
	}
}

// ---- confirm-or-revert

func TestBeginConfirm(t *testing.T) {
	prev := defaultConfig()
	if p := beginConfirm("x", prev, 0, false); p != nil {
		t.Fatal("confirmation off must not create a pending change")
	}
	p1 := beginConfirm("firewall", prev, 60, false)
	if p1 == nil || p1["seconds_left"].(int) < 58 {
		t.Fatalf("first: %v", p1)
	}
	changed := prev
	changed.LAN.Address = "10.9.9.1/24"
	p2 := beginConfirm("LAN settings", changed, 60, true)
	if p2["id"] != p1["id"] || p2["seconds_left"].(int) < lanChangeSecs-2 {
		t.Errorf("second change must join the first and get the LAN reconnect time: %v", p2)
	}
	if w := p2["what"].([]string); len(w) != 2 {
		t.Errorf("what: %v", w)
	}
	pendMu.Lock()
	keptPrev := pending.Prev.LAN.Address
	pendMu.Unlock()
	if keptPrev != prev.LAN.Address {
		t.Errorf("the original before-state must be kept, got %s", keptPrev)
	}
	if _, err := takePending("wrong-id"); err == nil {
		t.Error("wrong id accepted")
	}
	if _, err := takePending(p1["id"].(string)); err != nil || pendingView() != nil {
		t.Errorf("take: %v", err)
	}
}

// ---- SMS control

func TestParseCtl(t *testing.T) {
	cases := []struct{ in, pin, cmd, masked string }{
		{"4821 status", "4821", "STATUS", "•••• STATUS"},
		{"  4821   Mesh  restart ", "4821", "MESH RESTART", "•••• MESH RESTART"},
		{"STATUS", "", "STATUS", "STATUS"},
		{"lte reset", "", "LTE RESET", "LTE RESET"},
		{"Hello there", "", "", "Hello there"},
		{"4821 status please", "", "", "4821 status please"},
		{"4821 shutdown", "", "", "4821 shutdown"},
		{"", "", "", ""},
	}
	for _, c := range cases {
		got := parseCtl(c.in)
		if got.pin != c.pin || got.cmd != c.cmd || got.masked != c.masked {
			t.Errorf("parseCtl(%q) = %+v", c.in, got)
		}
	}
}

func TestCtlNumberMatches(t *testing.T) {
	for _, s := range []string{"+919876543210", "919876543210", "9876543210", "09876543210"} {
		if !ctlNumberMatches("9876543210", s) {
			t.Errorf("%s should match", s)
		}
	}
	for _, s := range []string{"+919876543211", "+449876543210", "JX-JIOINF", ""} {
		if ctlNumberMatches("9876543210", s) {
			t.Errorf("%s must not match", s)
		}
	}
	if !ctlNumberMatches("447700900123", "+447700900123") || ctlNumberMatches("447700900123", "7700900123") {
		t.Error("international numbers match in full only")
	}
}

func TestCtlCheckLockout(t *testing.T) {
	ctlSenders = map[string]*ctlSender{}
	c := SMSCtlCfg{Enabled: true, Numbers: []string{"9876543210"}, PINHash: hashPIN("4821")}
	now := time.Date(2026, 11, 26, 10, 0, 0, 0, time.Local)
	from := "+919876543210"
	if ok, _ := ctlCheck(c, from, "4821", now); !ok {
		t.Fatal("right PIN refused")
	}
	for i := 1; i <= 3; i++ {
		ok, reply := ctlCheck(c, from, "0000", now.Add(time.Duration(i)*time.Minute))
		if ok || reply == "" || (i == 3) != strings.Contains(reply, "locked") {
			t.Fatalf("wrong PIN %d: ok=%v %q", i, ok, reply)
		}
	}
	if ok, reply := ctlCheck(c, from, "4821", now.Add(10*time.Minute)); ok || reply != "" {
		t.Error("locked out sender must be ignored silently, even with the right PIN")
	}
	if ok, _ := ctlCheck(c, from, "4821", now.Add(65*time.Minute)); !ok {
		t.Error("lockout must end after an hour")
	}
	// reply limit: 10 per hour
	ctlSenders = map[string]*ctlSender{}
	n := 0
	for i := 0; i < 15; i++ {
		if ok, _ := ctlCheck(c, from, "4821", now.Add(time.Duration(i)*time.Second)); ok {
			n++
		}
	}
	if n != 10 {
		t.Errorf("reply limit: %d commands ran", n)
	}
}

func TestCtlTooOld(t *testing.T) {
	now := time.Date(2026, 11, 26, 10, 0, 0, 0, time.FixedZone("IST", 19800))
	if ctlTooOld("2026-11-26T09:50:00+05:30", now) {
		t.Error("10 minutes is fine")
	}
	if !ctlTooOld("2026-11-26T09:40:00+05:30", now) || !ctlTooOld("2026-11-26T09:10:00+05", now) {
		t.Error("20 minutes is too old")
	}
	if ctlTooOld("garbage", now) || ctlTooOld("2026-11-26T09:00:00+05:30", time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Error("unknown times must not block a command")
	}
}

func TestValidateSMSCtl(t *testing.T) {
	s := SMSCtlCfg{Enabled: true, Numbers: []string{"+91 98765 43210"}}
	if validateSMSCtl(&s) == nil {
		t.Error("enabled without a PIN accepted")
	}
	s.PINHash = hashPIN("4821")
	if err := validateSMSCtl(&s); err != nil || s.Numbers[0] != "919876543210" {
		t.Errorf("%v %v", err, s.Numbers)
	}
	s.Numbers = []string{"12345"}
	if validateSMSCtl(&s) == nil {
		t.Error("short number accepted")
	}
}
