package main

import (
	"strings"
	"testing"
	"time"
)

func TestTransferStats(t *testing.T) {
	st := &transferStats{warmup: 2 * time.Second}
	t0 := time.Date(2026, 11, 22, 23, 0, 0, 0, time.UTC)
	// slow 2 s ramp-up (0.5 MB), then 1 MB/s for 10 s
	for i := 0; i < 4; i++ {
		st.add(125_000, t0.Add(time.Duration(i)*500*time.Millisecond))
	}
	for i := 0; i <= 10; i++ {
		st.add(1_000_000, t0.Add(2*time.Second+time.Duration(i)*time.Second))
	}
	mb, mbps, secs := st.result(t0.Add(13 * time.Second))
	if mb != 11.5 || secs != 13 {
		t.Errorf("mb %v secs %v", mb, secs)
	}
	if mbps < 7.9 || mbps > 8.1 { // 10 MB after the warm-up point over 11 s ≈ 8 Mbit/s, not the 7 Mbit/s average
		t.Errorf("steady Mbit/s = %v", mbps)
	}
	var empty transferStats
	if mb, mbps, _ := empty.result(t0); mb != 0 || mbps != 0 {
		t.Errorf("empty: %v %v", mb, mbps)
	}
	if m := rePingSummary.FindStringSubmatch("rtt min/avg/max/mdev = 20.184/51.586/103.590/29.906 ms"); m == nil || m[2] != "51.586" {
		t.Errorf("ping summary: %v", m)
	}
}

func TestSpeedBlocked(t *testing.T) {
	c := defaultThermal() // starts limiting at 85
	for _, tc := range []struct {
		temp    float64
		cooling bool
		iface   string
		want    string
	}{
		{70, false, "wwan0", ""},
		{86, false, "wwan0", "°C"},
		{70, true, "wwan0", "cooldown"},
		{70, false, "none", "no uplink"},
		{70, false, "", "no uplink"},
	} {
		got := speedBlocked(tc.temp, c, tc.cooling, tc.iface)
		if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("%+v: got %q", tc, got)
		}
	}
	c.Enabled = false
	if got := speedBlocked(95, c, false, "wwan0"); got != "" {
		t.Errorf("guard off should not block on temperature: %q", got)
	}
}
