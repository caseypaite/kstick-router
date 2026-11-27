package main

import (
	"bufio"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// ---------------- background sampler: interface rates + CPU ----------------

const histLen = 90 // 90 samples x 2 s = 3 minutes

type ifSample struct {
	RX, TX uint64 // totals (bytes)
}

type ratePoint struct {
	T  int64   `json:"t"`
	RX float64 `json:"rx"` // bytes/s
	TX float64 `json:"tx"`
}

var (
	sampMu  sync.Mutex
	lastIf  = map[string]ifSample{}
	history = map[string][]ratePoint{}
	cpuPct  float64
)

func readIfStats() map[string]ifSample {
	out := map[string]ifSample{}
	ents, _ := os.ReadDir("/sys/class/net")
	for _, e := range ents {
		n := e.Name()
		if n == "lo" {
			continue
		}
		rx, _ := strconv.ParseUint(readTrim(filepath.Join("/sys/class/net", n, "statistics/rx_bytes")), 10, 64)
		tx, _ := strconv.ParseUint(readTrim(filepath.Join("/sys/class/net", n, "statistics/tx_bytes")), 10, 64)
		out[n] = ifSample{rx, tx}
	}
	return out
}

func readCPU() (idle, total uint64) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if sc.Scan() {
		fs := strings.Fields(sc.Text())
		for i, v := range fs[1:] {
			n, _ := strconv.ParseUint(v, 10, 64)
			total += n
			if i == 3 || i == 4 { // idle + iowait
				idle += n
			}
		}
	}
	return
}

// LTE rates: the modem driver keeps no byte counters for wwan0 (sysfs stays 0), so its bytes come from
// the router firewall's nft counters lte_rx/lte_tx. Reading them starts a process, so it is done only
// while someone looks at LTE rates (web dashboard/interfaces, or the LCD's mobile network page).
const (
	lteIf           = "wwan0"
	lteDemandWindow = 30 * time.Second
	sampleStep      = 2 * time.Second
)

var (
	lastIfAt  = map[string]time.Time{}
	lteDemand atomic.Int64 // unix nanoseconds of the last request for LTE rates
)

// wantLTERates keeps LTE sampling on for lteDemandWindow (and the modem details fresh).
func wantLTERates() { lteDemand.Store(time.Now().UnixNano()); wantModem() }

var modemDemand atomic.Int64

// wantModem: someone shows modem details (web UI, LCD); modemLoop then refreshes every 10 s instead of every minute.
func wantModem() { modemDemand.Store(time.Now().UnixNano()) }

func modemWatched(now time.Time) bool {
	return now.Sub(time.Unix(0, modemDemand.Load())) < lteDemandWindow
}

func lteWatched(now time.Time) bool {
	return now.Sub(time.Unix(0, lteDemand.Load())) < lteDemandWindow
}

func lteCounters() (ifSample, bool) {
	out, err := run(5*time.Second, "nft", "list", "counters", "table", "inet", "router")
	if err != nil {
		return ifSample{}, false
	}
	return parseLTECounters(out)
}

// parseLTECounters reads lte_rx/lte_tx from "nft list counters" output.
func parseLTECounters(txt string) (ifSample, bool) {
	var s ifSample
	cur, seen := "", 0
	for _, l := range strings.Split(txt, "\n") {
		f := strings.Fields(l)
		if len(f) >= 2 && f[0] == "counter" {
			cur = f[1]
			continue
		}
		for i := 0; i+1 < len(f); i++ {
			if f[i] != "bytes" {
				continue
			}
			v, err := strconv.ParseUint(f[i+1], 10, 64)
			if err != nil {
				break
			}
			switch cur {
			case "lte_rx":
				s.RX, seen = v, seen|1
			case "lte_tx":
				s.TX, seen = v, seen|2
			}
		}
	}
	return s, seen == 3
}

func sampler() {
	prevIdle, prevTotal := readCPU()
	for range time.Tick(sampleStep) {
		t := time.Now()
		now := readIfStats()
		if _, ok := now[lteIf]; ok {
			delete(now, lteIf) // sysfs zeros mean nothing for wwan0
			if lteWatched(t) {
				if c, ok := lteCounters(); ok {
					now[lteIf] = c
				}
			}
		}
		idle, total := readCPU()
		sampMu.Lock()
		for n, s := range now {
			p, ok := lastIf[n]
			at := lastIfAt[n]
			lastIf[n], lastIfAt[n] = s, t
			if !ok || s.RX < p.RX || s.TX < p.TX { // new interface, or counters restarted
				continue
			}
			dt := t.Sub(at).Seconds()
			if dt > 3*sampleStep.Seconds() { // a gap (LTE not watched): start the history afresh
				history[n] = nil
				continue
			}
			if dt <= 0 {
				continue
			}
			h := append(history[n], ratePoint{t.Unix(), float64(s.RX-p.RX) / dt, float64(s.TX-p.TX) / dt})
			if len(h) > histLen {
				h = h[len(h)-histLen:]
			}
			history[n] = h
		}
		if dt := total - prevTotal; dt > 0 {
			cpuPct = 100 * float64(dt-(idle-prevIdle)) / float64(dt)
		}
		prevIdle, prevTotal = idle, total
		sampMu.Unlock()
	}
}

// currentRate is the latest rate of an interface; 0 when it is stale (e.g. LTE not sampled lately).
func currentRate(n string) ratePoint {
	sampMu.Lock()
	defer sampMu.Unlock()
	h := history[n]
	if len(h) == 0 || time.Since(time.Unix(h[len(h)-1].T, 0)) > 3*sampleStep {
		return ratePoint{}
	}
	return h[len(h)-1]
}

// sampledTotals: the sampler's latest byte totals (for wwan0, the nft counters since the firewall loaded).
func sampledTotals(n string) (ifSample, bool) {
	sampMu.Lock()
	defer sampMu.Unlock()
	s, ok := lastIf[n]
	return s, ok
}

func apiTraffic(r *http.Request) (any, error) {
	wantLTERates()
	sampMu.Lock()
	defer sampMu.Unlock()
	out := map[string][]ratePoint{}
	for n, h := range history {
		if strings.HasPrefix(n, "veth") {
			continue
		}
		out[n] = append([]ratePoint(nil), h...)
	}
	return out, nil
}

// ---------------- system ----------------

func memInfo() map[string]uint64 {
	m := map[string]uint64{}
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return m
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		fs := strings.Fields(v)
		if len(fs) > 0 {
			n, _ := strconv.ParseUint(fs[0], 10, 64)
			m[k] = n * 1024
		}
	}
	return m
}

// thermalLabels: MSM8916 on-die sensors are named after the SoC block they sit next to. The stick has no camera
// and its GPU is disabled, but those sensors still measure the chip, so label them as SoC regions.
var thermalLabels = map[string]string{
	"modem": "Modem", "camera": "SoC (camera block)", "gpu": "SoC (GPU block)",
	"cpu0-1": "CPU 0–1", "cpu2-3": "CPU 2–3",
}

// temps lists the thermal zones with the lowest passive/hot trip point (where the kernel starts throttling), if any.
func temps() []map[string]any {
	out := []map[string]any{}
	zones, _ := filepath.Glob("/sys/class/thermal/thermal_zone*")
	for _, z := range zones {
		t, err := strconv.Atoi(readTrim(z + "/temp"))
		if err != nil {
			continue
		}
		name := strings.TrimSuffix(readTrim(z+"/type"), "-thermal")
		e := map[string]any{"name": name, "label": firstNonEmpty(thermalLabels[name], name), "c": float64(t) / 1000}
		trips, _ := filepath.Glob(z + "/trip_point_*_type")
		for _, tp := range trips {
			if typ := readTrim(tp); typ != "passive" && typ != "hot" {
				continue
			}
			if v, err := strconv.Atoi(readTrim(strings.TrimSuffix(tp, "_type") + "_temp")); err == nil {
				if cur, ok := e["trip"].(float64); !ok || float64(v)/1000 < cur {
					e["trip"] = float64(v) / 1000
				}
			}
		}
		out = append(out, e)
	}
	return out
}

// cpuFreq reports the CPU clock: current, the configured cap, the hardware maximum, and the thermal limit
// (the cpufreq cooling device's state indexes the frequency table from the top; state 0 = not throttled).
func cpuFreq() map[string]any {
	const p = "/sys/devices/system/cpu/cpufreq/policy0/"
	khz := func(f string) int { v, _ := strconv.Atoi(readTrim(p + f)); return v / 1000 }
	cur := khz("scaling_cur_freq")
	if cur == 0 {
		return nil
	}
	var steps []int // MHz, highest first
	for _, f := range strings.Fields(readTrim(p + "scaling_available_frequencies")) {
		if v, err := strconv.Atoi(f); err == nil {
			steps = append(steps, v/1000)
		}
	}
	sort.Sort(sort.Reverse(sort.IntSlice(steps)))
	out := map[string]any{
		"cur_mhz": cur, "max_mhz": khz("scaling_max_freq"), "hw_max_mhz": khz("cpuinfo_max_freq"),
		"governor": readTrim(p + "scaling_governor"), "thermal_mhz": 0,
	}
	devs, _ := filepath.Glob("/sys/class/thermal/cooling_device*")
	for _, d := range devs {
		if !strings.HasPrefix(readTrim(d+"/type"), "cpufreq") {
			continue
		}
		if st, err := strconv.Atoi(readTrim(d + "/cur_state")); err == nil && st > 0 && st < len(steps) {
			out["thermal_mhz"] = steps[st]
		}
	}
	// The configured cap is what router-cpufreq.service set; scaling_max_freq already includes any thermal limit.
	// later files win, as in systemd: the unit's default, then kstick-setup's per-stick drop-in
	for _, c := range []string{readTrim("/etc/systemd/system/router-cpufreq.service"), readTrim("/etc/systemd/system/router-cpufreq.service.d/10-cap.conf")} {
		for _, f := range strings.Fields(c) {
			if v, ok := strings.CutPrefix(f, "MAX="); ok {
				if n, err := strconv.Atoi(v); err == nil {
					out["cap_mhz"] = n / 1000
				}
			}
		}
	}
	if c, ok := out["cap_mhz"].(int); ok && c >= out["hw_max_mhz"].(int) {
		delete(out, "cap_mhz") // capped at the hardware maximum = no cap
	}
	return out
}

func systemInfo() map[string]any {
	up, _ := strconv.ParseFloat(strings.Fields(readTrim("/proc/uptime") + " 0")[0], 64)
	var st syscall.Statfs_t
	_ = syscall.Statfs("/", &st)
	mem := memInfo()
	host, _ := os.Hostname()
	osName := ""
	for _, l := range strings.Split(readTrim("/etc/os-release"), "\n") {
		if v, ok := strings.CutPrefix(l, "PRETTY_NAME="); ok {
			osName = strings.Trim(v, `"`)
		}
	}
	kernel := readTrim("/proc/sys/kernel/osrelease")
	sampMu.Lock()
	cpu := cpuPct
	sampMu.Unlock()
	ctCount, _ := strconv.Atoi(readTrim("/proc/sys/net/netfilter/nf_conntrack_count"))
	ctMax, _ := strconv.Atoi(readTrim("/proc/sys/net/netfilter/nf_conntrack_max"))
	return map[string]any{
		"hostname": host, "os": osName, "kernel": kernel,
		"model":  strings.TrimRight(readTrim("/proc/device-tree/model"), "\x00"),
		"uptime": int64(up), "load": strings.Fields(readTrim("/proc/loadavg")), "cpu": cpu,
		"cpus":      countCPUs(),
		"mem_total": mem["MemTotal"], "mem_available": mem["MemAvailable"],
		"disk_total": st.Blocks * uint64(st.Bsize), "disk_free": st.Bavail * uint64(st.Bsize),
		"temps": temps(), "cpufreq": cpuFreq(), "thermal_guard": thermalSnapshot(), "time": time.Now().Format(time.RFC3339),
		"conntrack": ctCount, "conntrack_max": ctMax,
	}
}

func countCPUs() int {
	n := 0
	ents, _ := filepath.Glob("/sys/devices/system/cpu/cpu[0-9]*")
	n = len(ents)
	return n
}

// ---------------- interfaces ----------------

type ipAddrInfo struct {
	Family    string `json:"family"`
	Local     string `json:"local"`
	Prefixlen int    `json:"prefixlen"`
	Scope     string `json:"scope"`
}

type ipLink struct {
	Ifname    string       `json:"ifname"`
	Operstate string       `json:"operstate"`
	Address   string       `json:"address"`
	MTU       int          `json:"mtu"`
	Flags     []string     `json:"flags"`
	AddrInfo  []ipAddrInfo `json:"addr_info"`
}

func roleOf(n string) string {
	switch {
	case n == "wlan0":
		return "LAN (Wi-Fi)"
	case n == lanBridgeIf:
		return "LAN (bridge: Wi-Fi + wired)"
	case strings.HasPrefix(n, "enx"), strings.HasPrefix(n, "eth"):
		return "LAN (wired)"
	case n == "usb0":
		return "MGMT / uplink (USB)"
	case strings.HasPrefix(n, "wwan"):
		return "WAN (LTE)"
	case strings.HasPrefix(n, "bnep"):
		return "WAN backup (phone over Bluetooth)"
	case strings.HasPrefix(n, "wg"):
		return "VPN (WireGuard)"
	case n == "lo":
		return "loopback"
	}
	return ""
}

func apiInterfaces(r *http.Request) (any, error) {
	out, err := run(5*time.Second, "ip", "-j", "addr", "show")
	if err != nil {
		return nil, err
	}
	var links []ipLink
	if err := json.Unmarshal([]byte(out), &links); err != nil {
		return nil, err
	}
	wantLTERates()
	stats := readIfStats()
	if s, ok := sampledTotals(lteIf); ok {
		stats[lteIf] = s
	}
	var res []map[string]any
	for _, l := range links {
		if l.Ifname == "lo" {
			continue
		}
		// hide idle extra modem channels
		if strings.HasPrefix(l.Ifname, "wwan") && l.Ifname != "wwan0" && l.Operstate == "DOWN" && len(l.AddrInfo) == 0 {
			continue
		}
		var addrs []string
		for _, a := range l.AddrInfo {
			addrs = append(addrs, a.Local+"/"+strconv.Itoa(a.Prefixlen))
		}
		up := false
		for _, f := range l.Flags {
			if f == "UP" {
				up = true
			}
		}
		rate := currentRate(l.Ifname)
		res = append(res, map[string]any{
			"name": l.Ifname, "role": roleOf(l.Ifname), "state": l.Operstate, "up": up, "mac": l.Address,
			"mtu": l.MTU, "addresses": addrs, "rx_bytes": stats[l.Ifname].RX, "tx_bytes": stats[l.Ifname].TX,
			"rx_rate": rate.RX, "tx_rate": rate.TX,
		})
	}
	return res, nil
}

// ---------------- clients ----------------

type client struct {
	MAC       string   `json:"mac"`
	IP        string   `json:"ip"`
	Name      string   `json:"name"`
	Static    bool     `json:"static"`
	Blocked   bool     `json:"blocked"`
	Wifi      bool     `json:"wifi"`
	Signal    string   `json:"signal"`
	Connected int      `json:"connected_s"`
	Inactive  int      `json:"inactive_ms"`
	TxRate    string   `json:"bitrate"`
	LeaseEnds int64    `json:"lease_expires"`
	Upload    uint64   `json:"upload"`
	Download  uint64   `json:"download"`
	Seen      string   `json:"arp_state"`
	IPv6      []string `json:"ipv6"`
}

func stationDump() map[string]map[string]string {
	out := map[string]map[string]string{}
	txt, err := run(5*time.Second, "iw", "dev", "wlan0", "station", "dump")
	if err != nil {
		return out
	}
	var cur map[string]string
	for _, l := range strings.Split(txt, "\n") {
		if strings.HasPrefix(l, "Station ") {
			f := strings.Fields(l)
			if len(f) >= 2 {
				cur = map[string]string{}
				out[strings.ToLower(f[1])] = cur
			}
			continue
		}
		if cur == nil {
			continue
		}
		k, v, ok := strings.Cut(strings.TrimSpace(l), ":")
		if ok {
			cur[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return out
}

func readLeases() []map[string]string {
	var out []map[string]string
	b, err := os.ReadFile(leasePath)
	if err != nil {
		return out
	}
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		if len(f) >= 4 {
			name := f[3]
			if name == "*" {
				name = ""
			}
			out = append(out, map[string]string{"expires": f[0], "mac": strings.ToLower(f[1]), "ip": f[2], "name": name})
		}
	}
	return out
}

// setCounters reads per-IP byte counters from the nft dynamic sets.
func setCounters(set string) map[string]uint64 {
	out := map[string]uint64{}
	txt, err := run(5*time.Second, "nft", "-j", "list", "set", "inet", "router", set)
	if err != nil {
		return out
	}
	var doc struct {
		Nftables []map[string]json.RawMessage `json:"nftables"`
	}
	if json.Unmarshal([]byte(txt), &doc) != nil {
		return out
	}
	for _, item := range doc.Nftables {
		raw, ok := item["set"]
		if !ok {
			continue
		}
		var s struct {
			Elem []json.RawMessage `json:"elem"`
		}
		_ = json.Unmarshal(raw, &s)
		for _, e := range s.Elem {
			var el struct {
				Elem struct {
					Val     any `json:"val"`
					Counter struct {
						Bytes uint64 `json:"bytes"`
					} `json:"counter"`
				} `json:"elem"`
			}
			if json.Unmarshal(e, &el) == nil {
				if ip, ok := el.Elem.Val.(string); ok {
					out[ip] = el.Elem.Counter.Bytes
				}
			}
		}
	}
	return out
}

func listClients() []*client {
	c := snapshot()
	byMAC := map[string]*client{}
	get := func(mac string) *client {
		mac = strings.ToLower(mac)
		if byMAC[mac] == nil {
			byMAC[mac] = &client{MAC: mac}
		}
		return byMAC[mac]
	}
	for _, l := range readLeases() {
		cl := get(l["mac"])
		cl.IP, cl.Name = l["ip"], l["name"]
		cl.LeaseEnds, _ = strconv.ParseInt(l["expires"], 10, 64)
	}
	for mac, st := range stationDump() {
		cl := get(mac)
		cl.Wifi = true
		cl.Signal = st["signal"]
		cl.TxRate = st["tx bitrate"]
		cl.Connected, _ = strconv.Atoi(strings.Fields(st["connected time"] + " 0")[0])
		cl.Inactive, _ = strconv.Atoi(strings.Fields(st["inactive time"] + " 0")[0])
	}
	if txt, err := run(5*time.Second, "ip", "-j", "neigh", "show", "dev", "wlan0"); err == nil {
		var ns []struct {
			Dst    string   `json:"dst"`
			Lladdr string   `json:"lladdr"`
			State  []string `json:"state"`
		}
		if json.Unmarshal([]byte(txt), &ns) == nil {
			for _, n := range ns {
				if n.Lladdr == "" || strings.Contains(n.Dst, ":") {
					continue
				}
				cl := get(n.Lladdr)
				if cl.IP == "" {
					cl.IP = n.Dst
				}
				if len(n.State) > 0 {
					cl.Seen = n.State[0]
				}
			}
		}
	}
	if txt, err := run(5*time.Second, "ip", "-j", "-6", "neigh", "show", "dev", "wlan0"); err == nil {
		var ns []struct {
			Dst    string `json:"dst"`
			Lladdr string `json:"lladdr"`
		}
		if json.Unmarshal([]byte(txt), &ns) == nil {
			for _, n := range ns {
				if n.Lladdr != "" && !strings.HasPrefix(n.Dst, "fe80:") {
					cl := get(n.Lladdr)
					cl.IPv6 = append(cl.IPv6, n.Dst)
				}
			}
		}
	}
	for _, s := range c.LAN.Static {
		cl, ok := byMAC[s.MAC]
		if !ok {
			continue
		}
		cl.Static = true
		if cl.Name == "" {
			cl.Name = s.Name
		}
	}
	for _, b := range c.Firewall.Blocked {
		get(b.MAC).Blocked = true
	}
	up, down := setCounters("lan_tx"), setCounters("lan_rx")
	out := make([]*client, 0, len(byMAC))
	for _, cl := range byMAC {
		cl.Upload, cl.Download = up[cl.IP], down[cl.IP]
		out = append(out, cl)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].IP < out[j].IP })
	return out
}

func apiClients(r *http.Request) (any, error) { return listClients(), nil }

func readWANState() map[string]string {
	st := map[string]string{}
	for _, l := range strings.Split(readTrim("/run/router/wan.state"), "\n") {
		if k, v, ok := strings.Cut(l, "="); ok {
			st[k] = v
		}
	}
	return st
}

// The slow parts of /api/status (each starts processes) are cached; the dashboard polls every 4 s.
var (
	stClients  = ttlCache[map[string]int]{ttl: 8 * time.Second}
	stFirewall = ttlCache[map[string]string]{ttl: 8 * time.Second}
	stHotspot  = ttlCache[map[string]any]{ttl: 10 * time.Second}
	stVPN      = ttlCache[map[string]any]{ttl: 10 * time.Second}
)

func apiStatus(r *http.Request) (any, error) {
	wantLTERates()
	return map[string]any{
		"system":  systemInfo(),
		"wan":     readWANState(),
		"modem":   modemSummary(),
		"clients": stClients.get(clientCounts),
		"wifi":    stHotspot.get(hotspotBrief),
		"vpn":     stVPN.get(vpnBrief),
		"ipv6":    v6Snapshot(),
		"firewall": map[string]any{
			"dropped": stFirewall.get(firewallDrops), "rules": len(snapshot().Firewall.Rules), "forwards": len(snapshot().Firewall.PortForwards),
		},
		"rates":           map[string]ratePoint{"wwan0": currentRate("wwan0"), "usb0": currentRate("usb0"), "wlan0": currentRate("wlan0")},
		"battery":         batteryInfo(),
		"data":            dataSummary(),
		"pending":         pendingView(),
		"battery_protect": batterySnapshot(),
		"bt_uplink":       btUplinkSnapshot(),
		"mode":            snapshot().Mode,
		"wifi_uplink":     wifiUplinkIfClient(),
	}, nil
}

func clientCounts() map[string]int {
	clients := listClients()
	online := 0
	for _, c := range clients {
		if c.Wifi {
			online++
		}
	}
	return map[string]int{"wifi": online, "known": len(clients)}
}

func firewallDrops() map[string]string {
	fwDrop := map[string]string{}
	if txt, err := run(5*time.Second, "nft", "list", "table", "inet", "router"); err == nil {
		for _, l := range strings.Split(txt, "\n") {
			if strings.Contains(l, `comment "dropped input"`) || strings.Contains(l, `comment "dropped forward"`) {
				f := strings.Fields(l)
				for i := range f {
					if f[i] == "packets" && i+1 < len(f) {
						if strings.Contains(l, "input") {
							fwDrop["input"] = f[i+1]
						} else {
							fwDrop["forward"] = f[i+1]
						}
					}
				}
			}
		}
	}
	return fwDrop
}

// batteryInfo reads the power-supply class (MF800: pm8916-bms-vm battery, pm8916-lbc-chgr USB charger).
// nil on devices without a battery (the KStick dongle).
func batteryInfo() map[string]any {
	var bat map[string]any
	usb := false
	dirs, _ := filepath.Glob("/sys/class/power_supply/*")
	for _, d := range dirs {
		switch readTrim(d + "/type") {
		case "Battery":
			if readTrim(d+"/present") == "0" {
				continue
			}
			bat = map[string]any{"status": readTrim(d + "/status"), "health": readTrim(d + "/health")}
			if v, err := strconv.Atoi(readTrim(d + "/capacity")); err == nil {
				bat["percent"] = v
			}
			if v, err := strconv.Atoi(readTrim(d + "/voltage_now")); err == nil {
				bat["voltage_mv"] = v / 1000
			}
		case "USB", "Mains":
			if readTrim(d+"/online") == "1" {
				usb = true
			}
		}
	}
	if bat != nil {
		bat["charging"] = bat["status"] == "Charging"
		bat["external_power"] = usb
	}
	return bat
}

// wifiUplinkIfClient: the joined Wi-Fi network in client mode (nil in router mode).
func wifiUplinkIfClient() map[string]any {
	if !isClient(snapshot()) {
		return nil
	}
	return stWifiLink.get(wifiLink)
}
