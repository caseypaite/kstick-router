package main

// Signal and placement helper: while the Signal page (or the LCD signal screen) is open, the modem
// measures every 2 s instead of every 10 s and the readings are fetched as often, so moving the stick
// shows up at once. Measuring is local to the modem: it sends nothing over the air, so it adds no heat.
//
// LTE band lock: limit the modem to some LTE bands (e.g. the operator's least crowded one). A wrong
// choice can leave the stick without service, so it is a confirm-or-revert change; the lock is
// re-applied if the modem forgets it (after a modem reset).

import (
	"fmt"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

type RadioCfg struct {
	Bands []string `json:"bands"` // LTE bands, e.g. "eutran-3"; empty = all the modem supports
}

var reLTEBand = regexp.MustCompile(`^eutran-([0-9]{1,3})$`)

func validateRadio(r *RadioCfg) error {
	if len(r.Bands) > 32 {
		return badRequest("too many bands")
	}
	seen := map[string]bool{}
	for _, b := range r.Bands {
		if !reLTEBand.MatchString(b) || seen[b] {
			return badRequest("invalid or duplicate LTE band %q", b)
		}
		seen[b] = true
	}
	sortBands(r.Bands)
	return nil
}

func bandNum(b string) int {
	m := reLTEBand.FindStringSubmatch(b)
	if m == nil {
		return 1 << 20
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

func sortBands(bs []string) {
	sort.Slice(bs, func(i, j int) bool { return bandNum(bs[i]) < bandNum(bs[j]) })
}

// modemBands: the bands the modem supports and the ones it currently uses (LTE only, sorted).
func modemBands() (supported, current []string, ok bool) {
	m := modemDetails()
	mo, _ := m["modem"].(map[string]any)
	if mo == nil {
		return nil, nil, false
	}
	list := func(k string) []string {
		var out []string
		arr, _ := dig(mo, "modem", "generic", k).([]any)
		for _, x := range arr {
			if s := str(x); reLTEBand.MatchString(s) {
				out = append(out, s)
			}
		}
		sortBands(out)
		return out
	}
	return list("supported-bands"), list("current-bands"), true
}

// bandsWanted: what the modem's LTE band set should be for cfg (empty cfg = all supported).
func bandsWanted(cfg, supported []string) []string {
	if len(cfg) == 0 {
		return supported
	}
	return cfg
}

func sameBands(a, b []string) bool { return strings.Join(a, ",") == strings.Join(b, ",") }

// applyBands sets the modem's bands when they differ from the config.
func applyBands(r RadioCfg) error {
	sup, cur, ok := modemBands()
	if !ok {
		return nil // no modem now: applied when it appears
	}
	if sameBands(bandsWanted(r.Bands, sup), cur) {
		return nil
	}
	arg := "any"
	if len(r.Bands) > 0 {
		arg = strings.Join(r.Bands, "|")
	}
	log.Printf("radio: setting LTE bands to %s (were %s)", arg, strings.Join(cur, ","))
	if out, err := run(30*time.Second, "mmcli", "-m", "any", "--set-current-bands="+arg); err != nil {
		return fmt.Errorf("setting the LTE bands: %v %s", err, strings.TrimSpace(out))
	}
	refreshModem()
	return nil
}

// ---------------------------------------------------------------- fast signal while watched

var signalDemand atomic.Int64

func wantFastSignal() { signalDemand.Store(time.Now().UnixNano()) }

func signalLoop() {
	fast, lastBandCheck := false, time.Now()
	for {
		watched := time.Since(time.Unix(0, signalDemand.Load())) < 30*time.Second
		if watched != fast {
			rate := map[bool]string{true: "2", false: "10"}[watched]
			if _, err := run(10*time.Second, "mmcli", "-m", "any", "--signal-setup="+rate); err == nil {
				fast = watched
			}
		}
		if fast {
			if sg := mmJSON("-m", "any", "--signal-get"); sg != nil {
				modemMu.Lock()
				if modemCache != nil {
					n := make(map[string]any, len(modemCache))
					for k, v := range modemCache {
						n[k] = v
					}
					n["signal"] = sg
					modemCache = n
				}
				modemMu.Unlock()
			}
		}
		// the band lock: re-applied if the modem lost it (checked every 10 min, from the cache)
		if time.Since(lastBandCheck) > 10*time.Minute {
			lastBandCheck = time.Now()
			if r := snapshot().Radio; len(r.Bands) > 0 {
				if err := applyBands(r); err != nil {
					log.Printf("radio: %v", err)
				}
			}
		}
		if fast {
			time.Sleep(2 * time.Second)
		} else {
			time.Sleep(5 * time.Second)
		}
	}
}

// signalQuality rates an LTE reading: 3 excellent, 2 good, 1 fair, 0 poor, -1 unknown.
func signalQuality(kind string, v float64) int {
	var th [3]float64 // excellent, good, fair
	switch kind {
	case "rsrp":
		th = [3]float64{-80, -90, -100}
	case "snr":
		th = [3]float64{20, 13, 0}
	case "rsrq":
		th = [3]float64{-10, -15, -20}
	default:
		return -1
	}
	for i, t := range th {
		if v >= t {
			return 3 - i
		}
	}
	return 0
}

var qualityName = []string{"poor", "fair", "good", "excellent"}

func apiGetSignal(r *http.Request) (any, error) {
	wantFastSignal()
	m := modemSummary()
	res := map[string]any{"modem": m, "fast": true}
	q := map[string]any{}
	for _, k := range []string{"rsrp", "rsrq", "snr"} {
		if f, err := strconv.ParseFloat(str(m[k]), 64); err == nil {
			q[k] = qualityName[signalQuality(k, f)]
		}
	}
	res["quality"] = q
	sup, cur, ok := modemBands()
	res["supported_bands"], res["current_bands"], res["bands_known"] = sup, cur, ok
	res["config"] = snapshot().Radio
	return res, nil
}

func apiPutRadio(r *http.Request) (any, error) {
	var rc RadioCfg
	if err := readJSON(r, &rc); err != nil {
		return nil, err
	}
	if rc.Bands == nil {
		rc.Bands = []string{}
	}
	if sup, _, ok := modemBands(); ok && len(rc.Bands) > 0 {
		have := map[string]bool{}
		for _, b := range sup {
			have[b] = true
		}
		for _, b := range rc.Bands {
			if !have[b] {
				return nil, badRequest("the modem does not support LTE band %s", strings.TrimPrefix(b, "eutran-"))
			}
		}
		if sameBands(rc.Bands, sup) {
			rc.Bands = []string{} // all of them: no lock
		}
	}
	return updateCfgConfirm(r, "LTE bands", func(c *Config) { c.Radio = rc }, func(c Config) error { return applyBands(c.Radio) })
}
