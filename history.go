package main

// History for charts: one sample a minute of temperature, CPU load, LTE signal (RSRP, RSRQ, SINR, %),
// battery and LTE traffic, kept at 1-minute resolution for 24 h and as 5-minute averages (traffic: sums)
// for 7 days. Nothing here wakes the radio: signal values come from the modem cache, LTE bytes from the
// data usage meter's counter readings. Saved to flash every 30 minutes and before a planned reboot or
// shutdown (about 150 KB); a crash loses at most the last half hour.

import (
	"bytes"
	"compress/gzip"
	"encoding/gob"
	"log"
	"math"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

var histFields = []string{"temp", "cpu", "rsrp", "rsrq", "snr", "signal", "battery", "rx", "tx"}

const (
	hfRX, hfTX   = 7, 8 // summed, not averaged
	histNF       = 9
	histMinSlots = 24 * 60
	histFiveStep = 5 * 60
	histFiveSlot = 7 * 24 * 12
	histPath     = "/var/lib/router/history.gob.gz"
	histSaveGap  = 30 * time.Minute
)

type histPoint struct {
	T int64 // unix seconds at the start of the slot; 0 = empty
	V [histNF]float32
}

type histStore struct {
	Min  [histMinSlots]histPoint
	Five [histFiveSlot]histPoint
}

var (
	histMu    sync.Mutex
	hist      = &histStore{}
	histBytes usagePair // LTE bytes since the last sample (from addUsage)
	histSaved time.Time
	histDirty bool
)

func nan() float32 { return float32(math.NaN()) }

// histAddBytes is called by the data usage meter with each new LTE byte count.
func histAddBytes(d usagePair) {
	histMu.Lock()
	histBytes.RX += d.RX
	histBytes.TX += d.TX
	histMu.Unlock()
}

// histRecord stores a sample for the minute containing t and refreshes its 5-minute bucket (histMu held).
func histRecord(h *histStore, t time.Time, v [histNF]float32) {
	m := t.Unix() / 60 * 60
	h.Min[(m/60)%histMinSlots] = histPoint{T: m, V: v}
	b := m / histFiveStep * histFiveStep
	var sum [histNF]float64
	var n [histNF]int
	for s := b; s < b+histFiveStep; s += 60 {
		p := h.Min[(s/60)%histMinSlots]
		if p.T != s {
			continue
		}
		for i, x := range p.V {
			if !math.IsNaN(float64(x)) {
				sum[i] += float64(x)
				n[i]++
			}
		}
	}
	var out [histNF]float32
	for i := range out {
		switch {
		case n[i] == 0:
			out[i] = nan()
		case i == hfRX || i == hfTX:
			out[i] = float32(sum[i])
		default:
			out[i] = float32(sum[i] / float64(n[i]))
		}
	}
	h.Five[(b/histFiveStep)%histFiveSlot] = histPoint{T: b, V: out}
}

// histSeries: the points of the last 24 h (step 60) or 7 days (step 300), oldest first; empty slots
// are left out (the charts draw gaps where time jumps).
func histSeries(h *histStore, step int64, now time.Time) []histPoint {
	var ring []histPoint
	var span int64
	if step == 60 {
		ring, span = h.Min[:], 24*3600
	} else {
		ring, span = h.Five[:], 7*24*3600
	}
	from := now.Unix() - span
	n := int64(len(ring))
	end := now.Unix() / step
	out := make([]histPoint, 0, n)
	for k := end - n + 1; k <= end; k++ {
		p := ring[((k%n)+n)%n]
		if p.T == k*step && p.T > from {
			out = append(out, p)
		}
	}
	return out
}

func histSample() [histNF]float32 {
	var v [histNF]float32
	for i := range v {
		v[i] = nan()
	}
	if c := maxThermalC(); c > 0 {
		v[0] = float32(c)
	}
	sampMu.Lock()
	v[1] = float32(cpuPct)
	sampMu.Unlock()
	m := modemSummary()
	if p, _ := m["present"].(bool); p {
		for i, k := range []string{"rsrp", "rsrq", "snr", "signal"} {
			if f, err := strconv.ParseFloat(str(m[k]), 64); err == nil {
				v[2+i] = float32(f)
			}
		}
	}
	if b := batteryInfo(); b != nil {
		if p, ok := b["percent"].(int); ok {
			v[6] = float32(p)
		}
	}
	return v
}

func loadHistory() {
	f, err := os.Open(histPath)
	if err != nil {
		return
	}
	defer f.Close()
	z, err := gzip.NewReader(f)
	if err != nil {
		return
	}
	var h histStore
	if err := gob.NewDecoder(z).Decode(&h); err != nil {
		log.Printf("history: %v (starting empty)", err)
		return
	}
	histMu.Lock()
	hist = &h
	histMu.Unlock()
}

func saveHistory() {
	histMu.Lock()
	if !histDirty {
		histMu.Unlock()
		return
	}
	var buf bytes.Buffer
	z, _ := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	err := gob.NewEncoder(z).Encode(hist)
	histDirty = false
	histMu.Unlock()
	if err == nil {
		err = z.Close()
	}
	if err == nil {
		err = writeAtomic(histPath, buf.Bytes(), 0o600)
	}
	if err != nil {
		log.Printf("history: %v", err)
	}
	histSaved = time.Now()
}

func historyLoop() {
	loadHistory()
	histSaved = time.Now()
	for {
		now := time.Now()
		time.Sleep(time.Until(now.Truncate(time.Minute).Add(time.Minute + 5*time.Second))) // a few s into each minute
		now = time.Now()
		v := histSample()
		histMu.Lock()
		v[hfRX], v[hfTX] = float32(histBytes.RX), float32(histBytes.TX)
		histBytes = usagePair{}
		if clockSane(now) { // no RTC: before NTP the time is meaningless
			histRecord(hist, now.Add(-time.Minute), v) // the bytes and the averages are about the minute just ended
			histDirty = true
		}
		histMu.Unlock()
		if time.Since(histSaved) >= histSaveGap {
			saveHistory()
		}
	}
}

// apiHistory: ?range=24h (default) or 7d.
func apiHistory(r *http.Request) (any, error) {
	step := int64(60)
	if r.URL.Query().Get("range") == "7d" {
		step = histFiveStep
	}
	histMu.Lock()
	pts := histSeries(hist, step, time.Now())
	histMu.Unlock()
	rows := make([][]any, len(pts))
	for i, p := range pts {
		row := make([]any, 0, histNF+1)
		row = append(row, p.T)
		for _, x := range p.V {
			if math.IsNaN(float64(x)) {
				row = append(row, nil)
			} else {
				row = append(row, math.Round(float64(x)*10)/10)
			}
		}
		rows[i] = row
	}
	return map[string]any{"step": step, "fields": append([]string{"t"}, histFields...), "points": rows}, nil
}
