package main

import (
	"strings"
	"testing"
	"time"
)

func TestThermalLevels(t *testing.T) {
	c := defaultThermal() // 85 start, 90 strict, 75 release
	steps := []struct {
		temp float64
		want int
	}{{70, 0}, {84.9, 0}, {85, 1}, {80, 1}, {76, 1}, {90, 2}, {86, 2}, {82.9, 1}, {75.5, 1}, {75, 0}, {84, 0}, {95, 2}, {74, 0}}
	level := 0
	for i, s := range steps {
		level = nextThermalLevel(level, s.temp, c)
		if level != s.want {
			t.Fatalf("step %d (%.1f °C): level %d, want %d", i, s.temp, level, s.want)
		}
	}
	c.Enabled = false
	if nextThermalLevel(2, 99, c) != 0 {
		t.Error("disabled guard must not limit")
	}
}

func TestThermalRender(t *testing.T) {
	c := defaultThermal()
	if r := renderThermalGuard(0, c, "wlan0", []string{"wwan0"}); !strings.Contains(r, "delete table inet thermalguard") || strings.Contains(r, "limit") {
		t.Errorf("level 0:\n%s", r)
	}
	r := renderThermalGuard(1, c, "wlan0", []string{"wwan0", "wg-warp"})
	for _, w := range []string{
		`iifname { "wwan0", "wg-warp" } oifname "wlan0" limit rate over 250 kbytes/second`,
		`iifname "wlan0" oifname { "wwan0", "wg-warp" } limit rate over 125 kbytes/second`,
	} {
		if !strings.Contains(r, w) {
			t.Errorf("level 1 missing %q:\n%s", w, r)
		}
	}
	if d, u := thermalRates(2, c); d != 500 || u != 250 {
		t.Errorf("strict rates %d/%d", d, u)
	}
}

func TestCooldownStep(t *testing.T) {
	c := defaultThermal() // 92 °C, 10 min, release 75
	m := time.Minute
	cases := []struct {
		active      bool
		elapsed     time.Duration
		temp        float64
		start, stop bool
	}{
		{false, 0, 91.9, false, false}, {false, 0, 92, true, false},
		{true, 5 * m, 70, false, false},  // too early even if cool
		{true, 10 * m, 80, false, false}, // long enough but still warm
		{true, 10 * m, 75, false, true},  // cooled down
		{true, 30 * m, 88, false, true},  // max 3x: try again
	}
	for i, tc := range cases {
		if s, p := cooldownStep(tc.active, tc.elapsed, tc.temp, c); s != tc.start || p != tc.stop {
			t.Errorf("case %d: start=%v stop=%v", i, s, p)
		}
	}
	off := c
	off.Cooldown = false
	if s, _ := cooldownStep(false, 0, 99, off); s {
		t.Error("disabled cooldown started")
	}
	if _, p := cooldownStep(true, m, 99, off); !p {
		t.Error("disabling must end a running cooldown")
	}
}

func TestValidateThermal(t *testing.T) {
	ok := defaultThermal()
	if err := validateThermal(&ok); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []ThermalCfg{{Enabled: true, StartC: 94, ReleaseC: 75, DownKbit: 2000, UpKbit: 1000},
		{Enabled: true, StartC: 85, ReleaseC: 83, DownKbit: 2000, UpKbit: 1000},
		{Enabled: true, StartC: 85, ReleaseC: 75, DownKbit: 10, UpKbit: 1000, Cooldown: true, CooldownC: 92, CooldownMin: 10},
		{Enabled: true, StartC: 85, ReleaseC: 75, DownKbit: 2000, UpKbit: 1000, Cooldown: true, CooldownC: 96, CooldownMin: 10},
		{Enabled: true, StartC: 85, ReleaseC: 75, DownKbit: 2000, UpKbit: 1000, Cooldown: true, CooldownC: 92, CooldownMin: 1}} {
		if validateThermal(&bad) == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

func TestResumeSince(t *testing.T) {
	c := defaultThermal() // 10 min, so at most 30 min back
	now := time.Date(2026, 11, 22, 20, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		marker string
		want   time.Time
	}{
		{"2026-11-22T19:55:00Z", now.Add(-5 * time.Minute)}, // keeps the original start
		{"2026-11-22T20:05:00Z", now},                       // clock behind: future marker
		{"2026-11-22T19:00:00Z", now},                       // too old
		{"garbage", now},
	} {
		if got := resumeSince(tc.marker, now, c); !got.Equal(tc.want) {
			t.Errorf("%s: got %s", tc.marker, got)
		}
	}
}

func TestThermalPaths(t *testing.T) {
	ifs := []string{"lo", "wlan0", "usb0", "wwan0", "wwan1", "wg-warp", "wgvpn", "CloudflareWARP"}
	if got := strings.Join(thermalPaths("wwan0", ifs), " "); got != "wwan0 CloudflareWARP wg-warp wgvpn" {
		t.Errorf("LTE uplink: %q", got)
	}
	if got := strings.Join(thermalPaths("none", ifs), " "); got != "wwan0 CloudflareWARP wg-warp wgvpn" {
		t.Errorf("no uplink: %q", got)
	}
	if got := strings.Join(thermalPaths("usb0", ifs), " "); got != "wwan0" {
		t.Errorf("USB uplink: tunnels must not be limited: %q", got)
	}
}
