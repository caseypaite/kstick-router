package main

import (
	"testing"
	"time"
)

func TestProbeGate(t *testing.T) {
	var g probeGate
	t0 := time.Unix(1_000_000, 0)
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	steps := []struct {
		name           string
		s              int
		rx, tx         uint64
		failing, force bool
		want           bool
	}{
		{"new device", 0, 100, 100, true, false, true},
		{"within probeMin", 10, 100, 100, false, false, false},
		{"idle, 40 s", 40, 100, 100, false, false, false},
		{"two-way traffic", 70, 900, 400, false, false, false},
		{"one-way traffic (stall?)", 110, 900, 800, false, false, true},
		{"idle, < 5 min after probe", 200, 900, 800, false, false, false},
		{"idle, >= 5 min after probe", 420, 900, 800, false, false, true},
		{"failing probes again after 30 s", 460, 900, 800, true, false, true},
		{"forced even within probeMin", 465, 900, 800, false, true, true},
		{"other device", 470, 5, 5, false, false, true},
	}
	for _, s := range steps {
		got := g.need("wg-warp", s.rx, s.tx, s.failing, s.force, at(s.s))
		if s.name == "other device" {
			got = g.need("wg-other", s.rx, s.tx, s.failing, s.force, at(s.s))
		}
		if got != s.want {
			t.Errorf("%s: need=%v, want %v", s.name, got, s.want)
		}
		if got {
			g.probed(s.rx, s.tx, at(s.s))
		}
	}
}
