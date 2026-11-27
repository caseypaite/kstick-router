package main

import (
	"testing"
	"time"
)

func TestParseLTECounters(t *testing.T) {
	txt := "table inet router {\n\tcounter lte_rx {\n\t\tpackets 12 bytes 3456\n\t}\n\tcounter lte_tx {\n\t\tpackets 3 bytes 789\n\t}\n}\n"
	if s, ok := parseLTECounters(txt); !ok || s.RX != 3456 || s.TX != 789 {
		t.Errorf("got %+v ok=%v", s, ok)
	}
	// an older firewall without the counters
	if _, ok := parseLTECounters("table inet router {\n}\n"); ok {
		t.Error("missing counters must not be ok")
	}
	if _, ok := parseLTECounters("table inet router {\n\tcounter lte_rx {\n\t\tpackets 1 bytes 2\n\t}\n}\n"); ok {
		t.Error("only one counter must not be ok")
	}
}

func TestCurrentRateStale(t *testing.T) {
	sampMu.Lock()
	history["test0"] = []ratePoint{{T: time.Now().Unix(), RX: 100, TX: 50}}
	history["test1"] = []ratePoint{{T: time.Now().Add(-time.Minute).Unix(), RX: 100, TX: 50}}
	sampMu.Unlock()
	defer func() { sampMu.Lock(); delete(history, "test0"); delete(history, "test1"); sampMu.Unlock() }()
	if r := currentRate("test0"); r.RX != 100 {
		t.Errorf("fresh rate: %+v", r)
	}
	if r := currentRate("test1"); r.RX != 0 || r.TX != 0 {
		t.Errorf("stale rate must read as 0: %+v", r)
	}
}

func TestLTEDemand(t *testing.T) {
	lteDemand.Store(0)
	if lteWatched(time.Now()) {
		t.Error("watched without demand")
	}
	wantLTERates()
	if !lteWatched(time.Now()) || lteWatched(time.Now().Add(lteDemandWindow+time.Second)) {
		t.Error("demand window")
	}
}
