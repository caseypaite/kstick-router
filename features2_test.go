package main

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---- battery

func TestBatDecide(t *testing.T) {
	c := defaultBattery()
	up := 10 * time.Minute
	var m batMemory
	var s batStep
	s, m = batDecide(c, 50, false, up, m)
	if s.level != 0 || s.warnNow || s.save {
		t.Fatalf("50%%: %+v", s)
	}
	s, m = batDecide(c, 19, false, up, m)
	if s.level != 1 || !s.warnNow {
		t.Fatalf("19%%: %+v", s)
	}
	s, m = batDecide(c, 18, false, up, m)
	if s.warnNow {
		t.Fatal("the warning is sent once per discharge")
	}
	s, m = batDecide(c, 10, false, up, m)
	if !s.save || s.level != 2 {
		t.Fatalf("10%%: %+v", s)
	}
	s, m = batDecide(c, 12, false, up, m) // gauge bounces up a little: stays in power saving
	if !s.save {
		t.Fatal("power saving must have hysteresis")
	}
	for i := 1; i <= 3; i++ {
		s, m = batDecide(c, 5, false, up, m)
		if s.offStreak != i {
			t.Fatalf("off streak %d: %+v", i, s)
		}
	}
	s, m = batDecide(c, 6, false, up, m) // voltage recovered: streak starts over
	if s.offStreak != 0 {
		t.Fatal("streak must reset")
	}
	s, m = batDecide(c, 4, true, up, m) // plugged in
	if s.save || s.offStreak != 0 || s.level != 0 {
		t.Fatalf("on power: %+v", s)
	}
	s, m = batDecide(c, 19, false, up, m) // unplugged again: a new discharge warns again
	if !s.warnNow {
		t.Fatal("new discharge must warn")
	}
	m = batMemory{}
	for i := 0; i < 5; i++ {
		s, m = batDecide(c, 2, false, time.Minute, m)
	}
	if s.offStreak != 0 {
		t.Fatal("no shutdown in the first minutes after boot")
	}
	b := BatteryCfg{Enabled: true, WarnPct: 10, SavePct: 10, OffPct: 5}
	if validateBattery(&b) == nil {
		t.Error("save == warn accepted")
	}
}

func TestLastBootReason(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bb.log")
	os.WriteFile(p, []byte("=== BOOT 1\n    PON=0x20(USB_CHG) WARM=0x00() POFF=0x00()\n12:00 t=40\n=== BOOT 2\n    PON=0x01(HARD_RESET) WARM=0x04(PMIC_WD) POFF=0x00()\n12:05 t=41\n"), 0o644)
	if got := lastBootReason(p); !strings.Contains(got, "PMIC_WD") {
		t.Errorf("got %q", got)
	}
}

// ---- history

func TestHistory(t *testing.T) {
	h := &histStore{}
	base := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	mk := func(temp, rx float32) [histNF]float32 {
		var v [histNF]float32
		for i := range v {
			v[i] = nan()
		}
		v[0], v[hfRX] = temp, rx
		return v
	}
	for i := 0; i < 7; i++ { // 10:00 … 10:06
		histRecord(h, base.Add(time.Duration(i)*time.Minute+20*time.Second), mk(float32(40+i), 1000))
	}
	pts := histSeries(h, 60, base.Add(7*time.Minute))
	if len(pts) != 7 || pts[0].T != base.Unix() || pts[6].V[0] != 46 {
		t.Fatalf("minutes: %d points %+v", len(pts), pts)
	}
	five := histSeries(h, 300, base.Add(7*time.Minute))
	if len(five) != 2 || five[0].V[0] != 42 || five[0].V[hfRX] != 5000 || five[1].V[hfRX] != 2000 {
		t.Fatalf("5 min: %+v", five)
	}
	if !math.IsNaN(float64(five[0].V[2])) {
		t.Error("missing values must stay missing")
	}
	// a day later the minute ring has wrapped: old minutes are gone, 5-minute points stay
	histRecord(h, base.Add(24*time.Hour+30*time.Second), mk(50, 1))
	pts = histSeries(h, 60, base.Add(24*time.Hour+time.Minute))
	if len(pts) != 6 || pts[len(pts)-1].V[0] != 50 { // 10:00 was overwritten, 10:01-10:05 are just inside 24 h... and the new one
		t.Fatalf("after wrap: %d %+v", len(pts), pts[len(pts)-1])
	}
	if n := len(histSeries(h, 300, base.Add(24*time.Hour+time.Minute))); n != 3 {
		t.Errorf("5-minute points after a day: %d", n)
	}
}

// ---- signal

func TestSignalQuality(t *testing.T) {
	for _, c := range []struct {
		k    string
		v    float64
		want int
	}{{"rsrp", -75, 3}, {"rsrp", -85, 2}, {"rsrp", -95, 1}, {"rsrp", -110, 0}, {"snr", 20.4, 3}, {"snr", -2, 0}, {"rsrq", -11, 2}, {"x", 1, -1}} {
		if got := signalQuality(c.k, c.v); got != c.want {
			t.Errorf("%s %v: %d", c.k, c.v, got)
		}
	}
	r := RadioCfg{Bands: []string{"eutran-40", "eutran-3"}}
	if err := validateRadio(&r); err != nil || r.Bands[0] != "eutran-3" {
		t.Errorf("%v %v", err, r.Bands)
	}
	for _, bad := range [][]string{{"utran-1"}, {"eutran-3", "eutran-3"}, {"eutran-x"}} {
		if validateRadio(&RadioCfg{Bands: bad}) == nil {
			t.Errorf("accepted %v", bad)
		}
	}
	if !sameBands(bandsWanted(nil, []string{"eutran-1", "eutran-3"}), []string{"eutran-1", "eutran-3"}) {
		t.Error("no lock = all supported")
	}
}

// ---- schedules

func TestWindows(t *testing.T) {
	if !inWindow(23*60, 22*60, 7*60) || !inWindow(3*60, 22*60, 7*60) || inWindow(12*60, 22*60, 7*60) || inWindow(7*60, 22*60, 7*60) {
		t.Error("overnight window")
	}
	if !inWindow(9*60, 9*60, 17*60) || inWindow(17*60, 9*60, 17*60) {
		t.Error("day window")
	}
	d := DevSched{MAC: "aa:bb:cc:dd:ee:ff", Days: []int{0, 1, 2, 3, 4}, From: "22:00", To: "07:00"} // school nights Sun-Thu
	at := func(s string) time.Time { x, _ := time.Parse("2006-01-02 15:04", s); return x }
	if !devBlocked(d, at("2026-11-26 23:00")) { // Thursday night
		t.Error("Thu 23:00")
	}
	if !devBlocked(d, at("2026-11-27 06:00")) { // Friday morning, window started Thursday
		t.Error("Fri 06:00")
	}
	if devBlocked(d, at("2026-11-27 23:00")) || devBlocked(d, at("2026-11-28 06:00")) { // Friday night
		t.Error("Friday night is free")
	}
	d.Days = []int{}
	if !devBlocked(d, at("2026-11-27 23:00")) {
		t.Error("no days = every day")
	}
}

func TestValidateSchedule(t *testing.T) {
	s := SchedCfg{LTEOff: Window{true, "01:00", "06:00"}, WifiOff: Window{true, "05:30", "08:00"}}
	if err := validateSchedule(&s); err == nil || !strings.Contains(err.Error(), "05:30") {
		t.Errorf("overlap: %v", err)
	}
	s.WifiOff = Window{true, "06:00", "08:00"}
	if err := validateSchedule(&s); err != nil {
		t.Errorf("adjacent windows: %v", err)
	}
	s.Devices = []DevSched{{MAC: "AA:BB:CC:DD:EE:FF", From: "22:00", To: "07:00"}}
	if err := validateSchedule(&s); err != nil || s.Devices[0].MAC != "aa:bb:cc:dd:ee:ff" {
		t.Errorf("%v %v", err, s.Devices)
	}
	for _, bad := range []SchedCfg{
		{LTEOff: Window{true, "25:00", "06:00"}},
		{WifiOff: Window{true, "06:00", "06:00"}},
		{Reboot: RebootSched{Enabled: true, Day: 8, At: "04:00"}},
		{Devices: []DevSched{{MAC: "nope", From: "22:00", To: "07:00"}}},
		{Devices: []DevSched{{MAC: "aa:bb:cc:dd:ee:ff", Days: []int{7}, From: "22:00", To: "07:00"}}},
	} {
		if validateSchedule(&bad) == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	if r := renderDevSched("wlan0", nil); strings.Contains(r, "chain") {
		t.Error("no devices: table removed")
	}
	if r := renderDevSched("wlan0", []string{"aa:bb:cc:dd:ee:ff"}); !strings.Contains(r, `iifname "wlan0" oifname != "wlan0" ether saddr { aa:bb:cc:dd:ee:ff } counter drop`) {
		t.Error(r)
	}
}

// ---- Bluetooth backup uplink

func TestBTUplink(t *testing.T) {
	if btNeed(true, false, false) || btNeed(true, true, true) {
		t.Error("a working main uplink never needs the phone")
	}
	if !btNeed(false, false, false) {
		t.Error("LTE down by fault: use the phone")
	}
	if btNeed(false, true, false) || !btNeed(false, true, true) {
		t.Error("LTE off on purpose: only when allowed")
	}
	c := defaultConfig()
	c.Bluetooth = BTCfg{Enabled: true, Uplink: "aa:bb:cc:dd:ee:ff"}
	if err := validateConfig(&c); err != nil || c.Bluetooth.Uplink != "AA:BB:CC:DD:EE:FF" {
		t.Fatalf("%v %v", err, c.Bluetooth)
	}
	if !strings.Contains(renderWANConf(c), `WANS="wwan0 usb0 bnep0"`) {
		t.Error(renderWANConf(c))
	}
	c.Bluetooth.Enabled = false
	if !strings.Contains(renderWANConf(c), `WANS="wwan0 usb0"`) {
		t.Error("Bluetooth off: no bnep0")
	}
	fw := renderFirewall(c)
	for _, want := range []string{`oifname "bnep*" accept comment "Bluetooth backup uplink (phone)"`, `oifname "bnep*" ip saddr`, `meta nfproto ipv6 oifname "bnep*" reject`} {
		if !strings.Contains(fw, want) {
			t.Errorf("firewall lacks %q", want)
		}
	}
	if p := thermalPaths(btIf, []string{"wg0"}); len(p) != 1 {
		t.Errorf("on the phone, tunnels don't heat the LTE radio: %v", p)
	}
	if validateBTCfg(&BTCfg{Uplink: "nope"}) == nil {
		t.Error("bad MAC accepted")
	}
}

func TestFirewallRenderStable(t *testing.T) {
	c := defaultConfig()
	_ = validateConfig(&c)
	a := renderFirewall(c)
	time.Sleep(1100 * time.Millisecond)
	if withoutStamp(a) != withoutStamp(renderFirewall(c)) {
		t.Error("the firewall render must not change between calls (it is compared at start)")
	}
}
