package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Speed test of the active uplink, run on the stick itself (curl against speed.cloudflare.com).
// 15 s download then 10 s upload; the speed is measured after a 2 s warm-up so the connection's
// ramp-up doesn't drag the result down. LTE transmission heats the modem fast, so it refuses to start
// at the thermal guard's start temperature or during an LTE cooldown, skips the upload if the
// download left the stick that hot, and stops a transfer early at speedAbortC.

const (
	speedDownBytes = 50_000_000 // Cloudflare refuses 100 MB (403); the URL is repeated so a fast link can't run dry
	speedDownReps  = 6
	speedDownSecs  = 15
	speedUpSecs    = 10
	speedWarmup    = 2 * time.Second
	speedAbortC    = 90 // below the emergency cooldown (92 °C)
	speedFile      = "/var/lib/router/speedtest.json"
	speedURL       = "https://speed.cloudflare.com"
)

type SpeedResult struct {
	Time      string  `json:"time"`
	Iface     string  `json:"iface"`
	PingMs    float64 `json:"ping_ms"`
	PingMin   float64 `json:"ping_min_ms"`
	PingMax   float64 `json:"ping_max_ms"`
	DownMbps  float64 `json:"down_mbps"`
	DownMB    float64 `json:"down_mb"`
	DownSecs  float64 `json:"down_s"`
	UpMbps    float64 `json:"up_mbps"`
	UpMB      float64 `json:"up_mb"`
	UpSecs    float64 `json:"up_s"`
	TempStart float64 `json:"temp_start_c"`
	TempEnd   float64 `json:"temp_end_c"`
	Note      string  `json:"note,omitempty"`
}

var (
	speedMu      sync.Mutex
	speedRunning bool
	speedPhase   string
	speedErr     string
	speedLast    *SpeedResult
)

func init() {
	if b, err := os.ReadFile(speedFile); err == nil {
		var r SpeedResult
		if json.Unmarshal(b, &r) == nil {
			speedLast = &r
		}
	}
}

func speedSet(phase string) { speedMu.Lock(); speedPhase = phase; speedMu.Unlock() }

func apiGetSpeedtest(r *http.Request) (any, error) {
	speedMu.Lock()
	defer speedMu.Unlock()
	return map[string]any{"running": speedRunning, "phase": speedPhase, "error": speedErr, "last": speedLast}, nil
}

// speedBlocked: why a test must not start now ("" = ok).
func speedBlocked(c float64, t ThermalCfg, cooling bool, iface string) string {
	switch {
	case iface == "" || iface == "none":
		return "no uplink is online"
	case cooling:
		return "the LTE radio is off for an emergency cooldown"
	case t.Enabled && c >= float64(t.StartC):
		return fmt.Sprintf("the stick is at %.0f °C (limit %d °C): a speed test would overheat the modem", c, t.StartC)
	}
	return ""
}

func apiRunSpeedtest(r *http.Request) (any, error) {
	var b struct {
		Upload *bool `json:"upload"`
	}
	_ = readJSON(r, &b)
	upload := b.Upload == nil || *b.Upload
	iface := readWANState()["active"]
	_, cooling := os.Stat(cooldownMarker)
	if why := speedBlocked(maxThermalC(), snapshot().Thermal, cooling == nil, iface); why != "" {
		return nil, badRequest("speed test not started: %s", why)
	}
	speedMu.Lock()
	if speedRunning {
		speedMu.Unlock()
		return nil, &httpError{http.StatusConflict, "a speed test is already running"}
	}
	speedRunning, speedErr, speedPhase = true, "", "starting"
	speedMu.Unlock()
	go func() {
		res, err := runSpeedtest(iface, upload)
		speedMu.Lock()
		defer speedMu.Unlock()
		speedRunning, speedPhase = false, ""
		if err != nil {
			speedErr = err.Error()
			log.Printf("speed test: %v", err)
			return
		}
		speedLast = res
		if j, err := json.Marshal(res); err == nil {
			_ = writeAtomic(speedFile, j, 0o644)
		}
		log.Printf("speed test via %s: down %.1f Mbit/s, up %.1f Mbit/s, ping %.0f ms, %.0f→%.0f °C",
			res.Iface, res.DownMbps, res.UpMbps, res.PingMs, res.TempStart, res.TempEnd)
	}()
	return map[string]any{"started": true, "iface": iface}, nil
}

var rePingSummary = regexp.MustCompile(`= ([\d.]+)/([\d.]+)/([\d.]+)/`)

// transferStats tracks a transfer's byte count over time. The steady speed is taken from the
// warm-up point (warmup after the first byte) to the end; a transfer too short for that falls
// back to the average from the first byte.
type transferStats struct {
	mu          sync.Mutex
	n           int64
	first, warm time.Time
	nWarm       int64
	warmup      time.Duration
}

func (t *transferStats) add(n int, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.first.IsZero() {
		t.first = now
	}
	if t.warm.IsZero() && now.Sub(t.first) >= t.warmup {
		t.warm, t.nWarm = now, t.n
	}
	t.n += int64(n)
}

// result: total MB, steady Mbit/s, seconds since the first byte
func (t *transferStats) result(end time.Time) (mb, mbps, secs float64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.first.IsZero() {
		return 0, 0, 0
	}
	secs = end.Sub(t.first).Seconds()
	from, n := t.first, t.n
	if !t.warm.IsZero() && end.Sub(t.warm) >= time.Second {
		from, n = t.warm, t.n-t.nWarm
	}
	if d := end.Sub(from).Seconds(); d > 0 {
		mbps = float64(n) * 8 / 1e6 / d
	}
	return float64(t.n) / 1e6, mbps, secs
}

// countingReader feeds the upload: endless random bytes (so nothing on the path can compress them),
// counted as curl takes them.
type countingReader struct {
	st  *transferStats
	rng *rand.ChaCha8
}

func (c *countingReader) Read(p []byte) (int, error) {
	if len(p) > 64<<10 {
		p = p[:64<<10]
	}
	_, _ = c.rng.Read(p)
	c.st.add(len(p), time.Now())
	return len(p), nil
}

// timedTransfer runs curl for at most dur, measuring bytes as they flow, and stops early when the
// stick reaches speedAbortC. Returns the stats, why it stopped early ("" if it ran its course).
func timedTransfer(dur time.Duration, upload bool, args ...string) (*transferStats, string, error) {
	st := &transferStats{warmup: speedWarmup}
	ctx, cancel := context.WithTimeout(context.Background(), dur)
	defer cancel()
	cmd := exec.CommandContext(ctx, "curl", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	done := make(chan struct{})
	if upload {
		cmd.Stdin = &countingReader{st: st, rng: rand.NewChaCha8([32]byte{byte(time.Now().UnixNano())})}
		cmd.Stdout = io.Discard
	} else {
		out, err := cmd.StdoutPipe()
		if err != nil {
			return nil, "", err
		}
		go func() {
			defer close(done)
			buf := make([]byte, 64<<10)
			for {
				n, err := out.Read(buf)
				if n > 0 {
					st.add(n, time.Now())
				}
				if err != nil {
					return
				}
			}
		}()
	}
	if err := cmd.Start(); err != nil {
		return nil, "", err
	}
	waitErr := make(chan error, 1)
	go func() {
		if !upload {
			<-done
		}
		waitErr <- cmd.Wait()
	}()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	stopped := ""
	for {
		select {
		case err := <-waitErr:
			if ctx.Err() != nil { // our time limit: the normal end
				err = nil
			}
			if err != nil && st.n == 0 {
				return nil, stopped, fmt.Errorf("%v %s", err, strings.TrimSpace(stderr.String()))
			}
			return st, stopped, nil
		case <-tick.C:
			if c := maxThermalC(); c >= speedAbortC && stopped == "" {
				stopped = fmt.Sprintf("stopped early at %.0f °C", c)
				cancel()
			}
		}
	}
}

func runSpeedtest(iface string, upload bool) (*SpeedResult, error) {
	res := &SpeedResult{Time: time.Now().Format(time.RFC3339), Iface: iface, TempStart: maxThermalC()}

	speedSet("latency")
	out, _ := run(15*time.Second, "ping", "-4", "-n", "-c5", "-i0.3", "-W2", "-I", iface, "1.1.1.1")
	if m := rePingSummary.FindStringSubmatch(out); m != nil {
		res.PingMin, _ = strconv.ParseFloat(m[1], 64)
		res.PingMs, _ = strconv.ParseFloat(m[2], 64)
		res.PingMax, _ = strconv.ParseFloat(m[3], 64)
	}
	var notes []string

	speedSet("download")
	args := []string{"-4", "-s", "-f", "--interface", iface}
	for i := 0; i < speedDownReps; i++ { // curl fetches them one after another into the same stdout
		args = append(args, "-o", "-", fmt.Sprintf("%s/__down?bytes=%d", speedURL, speedDownBytes))
	}
	st, why, err := timedTransfer(speedDownSecs*time.Second, false, args...)
	if err != nil {
		return nil, fmt.Errorf("download failed: %v", err)
	}
	if res.DownMB, res.DownMbps, res.DownSecs = st.result(time.Now()); res.DownMB == 0 {
		return nil, fmt.Errorf("download failed: no data received")
	}
	if why != "" {
		notes = append(notes, "download "+why)
	}

	t := snapshot().Thermal
	if c := maxThermalC(); !upload {
		notes = append(notes, "upload skipped")
	} else if why != "" || (t.Enabled && c >= float64(t.StartC)) {
		notes = append(notes, fmt.Sprintf("upload skipped: %.0f °C after the download", c))
	} else {
		speedSet("upload")
		st, why, err = timedTransfer(speedUpSecs*time.Second, true, "-4", "-s", "-f", "--interface", iface, "-o", "/dev/null",
			"-X", "POST", "-H", "Content-Type: application/octet-stream", "-T", "-", speedURL+"/__up")
		switch {
		case err != nil:
			notes = append(notes, fmt.Sprintf("upload failed: %v", err))
		default:
			res.UpMB, res.UpMbps, res.UpSecs = st.result(time.Now())
			if why != "" {
				notes = append(notes, "upload "+why)
			}
		}
	}
	res.Note = strings.Join(notes, "; ")
	res.TempEnd = maxThermalC()
	return res, nil
}
