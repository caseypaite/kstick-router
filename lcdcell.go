package main

// Serving cell tower for the MF800 status screen: ModemManager's cell info (operator, TAC, cell ID, PCI, EARFCN,
// timing advance, neighbours) and the tower's position from a cell database. Everything is read only while the
// screen shows. A position lookup sends the serving cell's IDs (not the device's own data) to BeaconDB
// (no key) or, if /etc/router/opencellid-key exists, OpenCelliD first. There is at most one lookup per new cell.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	openCellIDKeyPath = "/etc/router/opencellid-key"
	cellInfoEvery     = 5 * time.Second
)

type lteCell struct {
	Serving        bool
	OperatorID     string // MCC+MNC
	TAC, CI, PCI   int64  // -1 unknown
	EARFCN         int    // -1 unknown
	RSRP, RSRQ     float64
	TimingAdvanceU int // µs round trip, -1 unknown
}

type cellPos struct {
	Lat, Lon float64
	RangeM   int
	Src      string
	Found    bool
	Err      string
	At       time.Time
}

type lcdCellState struct {
	Have       bool
	Serving    lteCell
	Neighbours int
	Pos        cellPos
	Looking    bool
}

var (
	cellMu      sync.Mutex
	cellCur     lcdCellState
	cellReadAt  time.Time
	cellPosMemo = map[string]cellPos{}
	cellBusy    = map[string]bool{}
	cellHTTP    = &http.Client{Timeout: 20 * time.Second}
)

// cellTower returns the serving cell, re-read at most every 5 s, and starts a position lookup for a new cell.
func cellTower() lcdCellState {
	cellMu.Lock()
	stale := time.Since(cellReadAt) >= cellInfoEvery
	cellMu.Unlock()
	if stale {
		out, err := run(10*time.Second, "mmcli", "-m", "any", "--get-cell-info", "-K")
		cells := parseCellInfo(out)
		cellMu.Lock()
		cellReadAt = time.Now()
		cellCur = lcdCellState{}
		if err == nil {
			for _, c := range cells {
				if c.Serving && !cellCur.Have {
					cellCur.Have, cellCur.Serving = true, c
				} else {
					cellCur.Neighbours++
				}
			}
		}
		cellMu.Unlock()
	}
	cellMu.Lock()
	defer cellMu.Unlock()
	s := cellCur
	if !s.Have || s.Serving.CI < 0 || len(s.Serving.OperatorID) < 5 {
		return s
	}
	key := cellKey(s.Serving)
	p, ok := cellPosMemo[key]
	retry := 24 * time.Hour // not in the databases: they change slowly
	if p.Err != "" {
		retry = 5 * time.Minute
	}
	if (!ok || time.Since(p.At) >= retry) && !cellBusy[key] {
		cellBusy[key] = true
		go func(c lteCell) {
			p := lookupCellPos(c)
			cellMu.Lock()
			cellPosMemo[key] = p
			delete(cellBusy, key)
			cellMu.Unlock()
		}(s.Serving)
	}
	s.Pos, s.Looking = p, cellBusy[key] && !p.Found
	return s
}

func cellKey(c lteCell) string { return fmt.Sprintf("%s-%d-%d", c.OperatorID, c.TAC, c.CI) }

// parseCellInfo reads `mmcli --get-cell-info -K` lines:
// modem.generic.cell-info.value[3] : cell type: lte, serving: yes, operator id: 40410, tac: 87D, ci: E7F220C, ...
func parseCellInfo(out string) []lteCell {
	var cells []lteCell
	for _, ln := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(ln, " : ")
		if !ok || !strings.HasPrefix(strings.TrimSpace(k), "modem.generic.cell-info.value[") {
			continue
		}
		f := map[string]string{}
		for _, part := range strings.Split(v, ", ") {
			if a, b, ok := strings.Cut(part, ": "); ok {
				f[strings.TrimSpace(a)] = strings.TrimSpace(b)
			}
		}
		if f["cell type"] != "lte" {
			continue
		}
		hex := func(k string) int64 {
			n, err := strconv.ParseInt(f[k], 16, 64)
			if err != nil {
				return -1
			}
			return n
		}
		dec := func(k string) int {
			n, err := strconv.Atoi(f[k])
			if err != nil {
				return -1
			}
			return n
		}
		fl := func(k string) float64 {
			x, err := strconv.ParseFloat(f[k], 64)
			if err != nil {
				return math.NaN()
			}
			return x
		}
		cells = append(cells, lteCell{
			Serving: f["serving"] == "yes", OperatorID: f["operator id"],
			TAC: hex("tac"), CI: hex("ci"), PCI: hex("physical ci"), EARFCN: dec("earfcn"),
			RSRP: fl("rsrp"), RSRQ: fl("rsrq"), TimingAdvanceU: dec("timing advance"),
		})
	}
	return cells
}

// lteBand: LTE band and downlink frequency (MHz) for a downlink EARFCN (3GPP TS 36.101), bands used in India and
// a few common others.
func lteBand(earfcn int) (band int, mhz float64) {
	for _, b := range []struct {
		band       int
		fLow       float64
		nOff, nMax int
	}{
		{1, 2110, 0, 599}, {3, 1805, 1200, 1949}, {5, 869, 2400, 2649}, {8, 925, 3450, 3799}, {20, 791, 6150, 6449},
		{28, 758, 9210, 9659}, {38, 2570, 37750, 38249}, {40, 2300, 38650, 39649}, {41, 2496, 39650, 41589},
	} {
		if earfcn >= b.nOff && earfcn <= b.nMax {
			return b.band, b.fLow + 0.1*float64(earfcn-b.nOff)
		}
	}
	return 0, 0
}

// taDistanceM: distance to the tower from the LTE timing advance, a round-trip time in µs (as libqmi reports it).
func taDistanceM(taUs int) int {
	if taUs < 0 {
		return -1
	}
	return taUs * 150 // c × t / 2
}

func lookupCellPos(c lteCell) cellPos {
	mcc, _ := strconv.Atoi(c.OperatorID[:3])
	mnc, _ := strconv.Atoi(c.OperatorID[3:])
	var errs []string
	if key := readTrim(openCellIDKeyPath); key != "" {
		p, err := lookupOpenCellID(key, mcc, mnc, c)
		if err == nil {
			return p
		}
		errs = append(errs, "OpenCelliD: "+err.Error())
	}
	p, err := lookupBeaconDB(mcc, mnc, c)
	if err == nil {
		return p
	}
	errs = append(errs, "BeaconDB: "+err.Error())
	notFound := true
	for _, e := range errs {
		if !strings.HasSuffix(e, errCellNotFound.Error()) {
			notFound = false
		}
	}
	if notFound {
		return cellPos{At: time.Now()}
	}
	log.Printf("lcd: cell position: %s", strings.Join(errs, "; "))
	return cellPos{Err: strings.Join(errs, "; "), At: time.Now()}
}

var errCellNotFound = fmt.Errorf("not found")

func lookupBeaconDB(mcc, mnc int, c lteCell) (cellPos, error) {
	body, _ := json.Marshal(map[string]any{
		"considerIp": false,
		"cellTowers": []map[string]any{{"radioType": "lte", "mobileCountryCode": mcc, "mobileNetworkCode": mnc,
			"locationAreaCode": c.TAC, "cellId": c.CI}},
	})
	req, _ := http.NewRequest("POST", "https://api.beacondb.net/v1/geolocate", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "router-webui")
	b, code, err := cellGet(req)
	if err != nil {
		return cellPos{}, err
	}
	if code == 404 {
		return cellPos{}, errCellNotFound
	}
	if code != 200 {
		return cellPos{}, fmt.Errorf("HTTP %d", code)
	}
	var r struct {
		Location struct{ Lat, Lng float64 }
		Accuracy float64
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return cellPos{}, err
	}
	return cellPos{Lat: r.Location.Lat, Lon: r.Location.Lng, RangeM: int(r.Accuracy), Src: "BeaconDB", Found: true, At: time.Now()}, nil
}

func lookupOpenCellID(key string, mcc, mnc int, c lteCell) (cellPos, error) {
	q := url.Values{"key": {key}, "mcc": {strconv.Itoa(mcc)}, "mnc": {strconv.Itoa(mnc)}, "lac": {strconv.FormatInt(c.TAC, 10)},
		"cellid": {strconv.FormatInt(c.CI, 10)}, "radio": {"LTE"}, "format": {"json"}}
	req, _ := http.NewRequest("GET", "https://opencellid.org/cell/get?"+q.Encode(), nil)
	req.Header.Set("User-Agent", "router-webui")
	b, code, err := cellGet(req)
	if err != nil {
		return cellPos{}, err
	}
	var r struct {
		Lat, Lon *float64
		Range    float64
		Error    string
		Code     int
	}
	jerr := json.Unmarshal(b, &r)
	if r.Lat == nil && (r.Code == 1 || strings.Contains(strings.ToLower(r.Error), "not found")) {
		return cellPos{}, errCellNotFound // OpenCelliD error code 1: cell not found (with HTTP 200 or 404)
	}
	if jerr != nil || code != 200 {
		if code != 200 { // an unknown key comes back as an error status, usually with a JSON message
			return cellPos{}, fmt.Errorf("HTTP %d %s", code, truncRunes(firstNonEmpty(r.Error, strings.TrimSpace(string(b))), 120))
		}
		return cellPos{}, jerr
	}
	if r.Lat == nil || r.Lon == nil {
		return cellPos{}, fmt.Errorf("%s", firstNonEmpty(r.Error, "no position in reply"))
	}
	return cellPos{Lat: *r.Lat, Lon: *r.Lon, RangeM: int(r.Range), Src: "OpenCelliD", Found: true, At: time.Now()}, nil
}

func cellGet(req *http.Request) ([]byte, int, error) {
	resp, err := cellHTTP.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return b, resp.StatusCode, err
}

// fmtDist: 850m, 3.0km, 12km.
func fmtDist(m int) string {
	switch {
	case m < 1000:
		return fmt.Sprintf("%dm", m)
	case m < 10000:
		return fmt.Sprintf("%.1fkm", float64(m)/1000)
	}
	return fmt.Sprintf("%dkm", (m+500)/1000)
}

// ---------------------------------------------------------------- web UI: Network → Signal & bands → Cell tower

// reOpenCellIDKey: OpenCelliD access tokens look like "pk.0123abcd..."; accept any plain token without spaces.
var reOpenCellIDKey = regexp.MustCompile(`^[A-Za-z0-9._-]{8,128}$`)

func openCellIDHint() (set bool, hint string) {
	k := readTrim(openCellIDKeyPath)
	if k == "" {
		return false, ""
	}
	if len(k) > 4 {
		hint = "…" + k[len(k)-4:]
	}
	return true, hint
}

func cellView(s lcdCellState) map[string]any {
	set, hint := openCellIDHint()
	res := map[string]any{"have": s.Have, "neighbours": s.Neighbours, "key_set": set, "key_hint": hint, "looking": s.Looking}
	if s.Have {
		sv := s.Serving
		c := map[string]any{"operator_id": sv.OperatorID, "tac": sv.TAC, "ci": sv.CI, "pci": sv.PCI, "earfcn": sv.EARFCN,
			"timing_advance_us": sv.TimingAdvanceU, "distance_m": taDistanceM(sv.TimingAdvanceU)}
		if !math.IsNaN(sv.RSRP) {
			c["rsrp"] = sv.RSRP
		}
		if sv.CI >= 0 {
			c["enb"], c["sector"] = sv.CI>>8, sv.CI&0xFF
		}
		if b, f := lteBand(sv.EARFCN); b > 0 {
			c["band"], c["freq_mhz"] = b, math.Round(f*10)/10
		}
		res["cell"] = c
	}
	if p := s.Pos; !p.At.IsZero() {
		res["position"] = map[string]any{"found": p.Found, "lat": p.Lat, "lon": p.Lon, "range_m": p.RangeM, "source": p.Src,
			"error": p.Err, "checked": p.At.Format(time.RFC3339)}
	}
	return res
}

func apiGetCellTower(r *http.Request) (any, error) { return cellView(cellTower()), nil }

func apiPutCellTower(r *http.Request) (any, error) {
	var b struct {
		Key   string `json:"opencellid_key"`
		Clear bool   `json:"clear"`
	}
	if err := readJSON(r, &b); err != nil {
		return nil, err
	}
	b.Key = strings.TrimSpace(b.Key)
	switch {
	case b.Clear:
		if err := os.Remove(openCellIDKeyPath); err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		audit(r, "OpenCelliD API key removed")
	case b.Key == "": // write-only field: empty keeps the stored key
		return map[string]any{"message": "Nothing changed."}, nil
	case !reOpenCellIDKey.MatchString(b.Key):
		return nil, badRequest("invalid API key: 8-128 letters, digits, '.', '_' or '-'")
	default:
		if err := writeAtomic(openCellIDKeyPath, []byte(b.Key+"\n"), 0o600); err != nil {
			return nil, err
		}
		audit(r, "OpenCelliD API key saved")
	}
	cellMu.Lock()
	clear(cellPosMemo) // look the tower up again with the new setting
	cellMu.Unlock()
	if b.Clear {
		return map[string]any{"message": "OpenCelliD key removed; only BeaconDB is used."}, nil
	}
	return map[string]any{"message": "OpenCelliD key saved."}, nil
}

// apiTestCellTower looks the serving cell up in OpenCelliD now (checks the key) and keeps a found position.
func apiTestCellTower(r *http.Request) (any, error) {
	key := readTrim(openCellIDKeyPath)
	if key == "" {
		return nil, badRequest("save an OpenCelliD API key first")
	}
	s := cellTower()
	if !s.Have || s.Serving.CI < 0 || len(s.Serving.OperatorID) < 5 {
		return nil, badRequest("no serving LTE cell right now")
	}
	c := s.Serving
	mcc, _ := strconv.Atoi(c.OperatorID[:3])
	mnc, _ := strconv.Atoi(c.OperatorID[3:])
	p, err := lookupOpenCellID(key, mcc, mnc, c)
	switch {
	case err == errCellNotFound:
		return map[string]any{"ok": true, "found": false, "message": "The key works, but OpenCelliD doesn't know this cell yet."}, nil
	case err != nil:
		return map[string]any{"ok": false, "error": err.Error()}, nil
	}
	cellMu.Lock()
	cellPosMemo[cellKey(c)] = p
	cellMu.Unlock()
	return map[string]any{"ok": true, "found": true, "lat": p.Lat, "lon": p.Lon, "range_m": p.RangeM}, nil
}
