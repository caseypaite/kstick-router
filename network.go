package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	hotspotCon = "hotspot"
	// Wired LAN (kstick-setup wired-lan on): bridge br-lan holds the LAN address; the hotspot and any USB
	// Ethernet adapter are its ports.
	lanBridgeCon  = "lan-bridge"
	lanBridgeIf   = "br-lan"
	lanBridgeFile = "/etc/NetworkManager/system-connections/lan-bridge.nmconnection"
	lteCon        = "lte"
)

// nmGet returns selected properties of a NetworkManager connection (secrets included).
func nmGet(con string, fields ...string) (map[string]string, error) {
	out, err := run(10*time.Second, "nmcli", "-s", "-t", "-f", strings.Join(fields, ","), "connection", "show", con)
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	for _, l := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		k, v, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		v = strings.NewReplacer(`\:`, ":", `\\`, `\`).Replace(v)
		m[k] = v
	}
	return m, nil
}

func nmModify(con string, kv ...string) error {
	args := append([]string{"connection", "modify", con}, kv...)
	_, err := run(15*time.Second, "nmcli", args...)
	return err
}

// nmUpLater re-activates a connection shortly after the HTTP response was sent,
// because doing so may cut the admin's own Wi-Fi connection.
func nmUpLater(con string) {
	go func() {
		time.Sleep(1500 * time.Millisecond)
		if con == hotspotCon && isClient(snapshot()) { // the hotspot would take wlan0 off the joined network
			log.Printf("activate %s: skipped (Wi-Fi client mode)", con)
			return
		}
		if _, err := run(60*time.Second, "nmcli", "-w", "45", "connection", "up", con); err != nil {
			log.Printf("activate %s: %v", con, err)
		}
	}()
}

// ---------------- Wi-Fi / LAN ----------------

func hotspotBrief() map[string]any {
	m, err := nmGet(hotspotCon, "802-11-wireless.ssid", "GENERAL.STATE", "802-11-wireless.channel")
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	return map[string]any{"ssid": m["802-11-wireless.ssid"], "state": m["GENERAL.STATE"], "channel": m["802-11-wireless.channel"]}
}

func apiGetLAN(r *http.Request) (any, error) {
	m, err := nmGet(hotspotCon, "802-11-wireless.ssid", "802-11-wireless.hidden", "802-11-wireless.channel",
		"802-11-wireless.band", "802-11-wireless-security.psk", "802-11-wireless-security.key-mgmt", "GENERAL.STATE", "connection.autoconnect")
	if err != nil {
		return nil, err
	}
	ch, _ := strconv.Atoi(m["802-11-wireless.channel"])
	return map[string]any{
		"wifi": map[string]any{
			"ssid": m["802-11-wireless.ssid"], "psk": m["802-11-wireless-security.psk"], "hidden": m["802-11-wireless.hidden"] == "yes",
			"channel": ch, "band": m["802-11-wireless.band"], "security": m["802-11-wireless-security.key-mgmt"],
			"state": m["GENERAL.STATE"],
		},
		"lan": snapshot().LAN,
	}, nil
}

func apiPutWifi(r *http.Request) (any, error) {
	var b struct {
		SSID    string `json:"ssid"`
		PSK     string `json:"psk"`
		Hidden  bool   `json:"hidden"`
		Channel int    `json:"channel"`
	}
	if err := readJSON(r, &b); err != nil {
		return nil, err
	}
	if err := validWifi(b.SSID, b.PSK, b.Channel); err != nil {
		return nil, err
	}
	hidden := "no"
	if b.Hidden {
		hidden = "yes"
	}
	if err := nmModify(hotspotCon, "802-11-wireless.ssid", b.SSID, "802-11-wireless-security.psk", b.PSK,
		"802-11-wireless.hidden", hidden, "802-11-wireless.channel", wifiChannelArg(b.Channel)); err != nil {
		return nil, err
	}
	audit(r, "wifi settings changed (ssid=%q hidden=%v channel=%d)", b.SSID, b.Hidden, b.Channel)
	stHotspot.invalidate()
	lcdWifiChanged()
	nmUpLater(hotspotCon)
	return map[string]string{"message": "Saved. Wi-Fi restarts now; clients reconnect in a few seconds."}, nil
}

// validWifi: the hotspot settings the Wi-Fi page and a backup restore accept.
func validWifi(ssid, psk string, channel int) error {
	if len(ssid) < 1 || len(ssid) > 32 || strings.IndexFunc(ssid, unicode.IsControl) >= 0 {
		return badRequest("SSID must be 1-32 bytes without control characters")
	}
	if len(psk) < 8 || len(psk) > 63 || strings.IndexFunc(psk, func(r rune) bool { return r < 0x20 || r > 0x7e }) >= 0 {
		return badRequest("Wi-Fi password must be 8-63 printable ASCII characters")
	}
	if channel < 0 || channel > 13 {
		return badRequest("channel must be 0 (auto) or 1-13")
	}
	return nil
}

func apiPutLAN(r *http.Request) (any, error) {
	var l LANCfg
	if err := readJSON(r, &l); err != nil {
		return nil, err
	}
	prev, next, err := putLAN(r, l)
	if err != nil {
		return nil, err
	}
	kickVPN()
	addrChanged := next.LAN.Address != prev.LAN.Address
	msg := map[string]string{"message": "Saved."}
	if addrChanged {
		nmUpLater(lanCon())
		msg["message"] = "Saved. The LAN address changed: reconnect to Wi-Fi and open https://" + next.LAN.prefix().Addr().String()
	}
	res := withPending(msg, "LAN settings", prev, addrChanged)
	if m, ok := res.(map[string]any); ok && addrChanged {
		m["message"] = msg["message"] + ", sign in there and confirm, or the old address comes back."
	}
	return res, nil
}

// putLAN validates, applies and saves new LAN settings; it returns the config before and after.
func putLAN(r *http.Request, l LANCfg) (Config, Config, error) {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	prev := cloneConfig(cfg)
	next := cloneConfig(cfg)
	l.Static = cfg.LAN.Static
	next.LAN = l
	if err := validateConfig(&next); err != nil {
		return prev, next, err
	}
	if next.LAN.Address != prev.LAN.Address {
		if err := nmModify(lanCon(), "ipv4.method", "manual", "ipv4.addresses", next.LAN.Address); err != nil {
			return prev, next, err
		}
	}
	if err := applyDnsmasq(next); err != nil {
		return prev, next, err
	}
	if err := applyFirewall(next); err != nil {
		return prev, next, err
	}
	cfg = next
	if err := saveConfig(); err != nil {
		return prev, next, err
	}
	audit(r, "LAN settings changed (%s, dhcp=%v %s-%s %s)", l.Address, l.DHCPEnabled, l.DHCPStart, l.DHCPEnd, l.LeaseTime)
	return prev, next, nil
}

func apiPutStatic(r *http.Request) (any, error) {
	var leases []StaticLease
	if err := readJSON(r, &leases); err != nil {
		return nil, err
	}
	return updateCfg(r, "static DHCP leases", func(c *Config) { c.LAN.Static = leases }, applyDnsmasq)
}

// updateCfg validates a modified copy of the config, applies it, then persists it.
func updateCfg(r *http.Request, what string, mutate func(*Config), apply ...func(Config) error) (any, error) {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	b, _ := json.Marshal(cfg)
	var next Config
	_ = json.Unmarshal(b, &next)
	mutate(&next)
	if err := validateConfig(&next); err != nil {
		return nil, err
	}
	for _, a := range apply {
		if err := a(next); err != nil {
			return nil, err
		}
	}
	cfg = next
	if err := saveConfig(); err != nil {
		return nil, err
	}
	audit(r, "%s updated", what)
	return map[string]string{"message": "Saved and applied."}, nil
}

// ---------------- WAN: failover + LTE ----------------

var (
	reAPN   = regexp.MustCompile(`^[A-Za-z0-9._-]{0,63}$`)
	rePrint = regexp.MustCompile(`^[\x20-\x7e]{0,64}$`)
)

func apiGetWAN(r *http.Request) (any, error) {
	m, _ := nmGet(lteCon, "gsm.apn", "gsm.username", "gsm.password", "gsm.home-only", "connection.autoconnect", "GENERAL.STATE")
	return map[string]any{
		"state":    readWANState(),
		"failover": snapshot().WAN,
		"lte": map[string]any{
			"apn": m["gsm.apn"], "username": m["gsm.username"], "has_password": m["gsm.password"] != "",
			"roaming": m["gsm.home-only"] != "yes", "autoconnect": m["connection.autoconnect"] == "yes", "state": m["GENERAL.STATE"],
		},
		"modem":         modemDetails(),
		"modem_summary": modemSummary(),
	}, nil
}

func apiPutFailover(r *http.Request) (any, error) {
	var w WANCfg
	if err := readJSON(r, &w); err != nil {
		return nil, err
	}
	return updateCfg(r, "WAN failover", func(c *Config) { c.WAN = w }, applyWAN)
}

func apiPutLTE(r *http.Request) (any, error) {
	var b struct {
		APN         string  `json:"apn"`
		Username    string  `json:"username"`
		Password    *string `json:"password"` // nil = keep
		Roaming     bool    `json:"roaming"`
		Autoconnect bool    `json:"autoconnect"`
	}
	if err := readJSON(r, &b); err != nil {
		return nil, err
	}
	if !reAPN.MatchString(b.APN) || !rePrint.MatchString(b.Username) || (b.Password != nil && !rePrint.MatchString(*b.Password)) {
		return nil, badRequest("APN may contain letters, digits . _ -; user/password printable ASCII up to 64 chars")
	}
	yn := func(v bool) string {
		if v {
			return "yes"
		}
		return "no"
	}
	kv := []string{"gsm.apn", b.APN, "gsm.username", b.Username, "gsm.home-only", yn(!b.Roaming), "connection.autoconnect", yn(b.Autoconnect)}
	if b.Password != nil {
		kv = append(kv, "gsm.password", *b.Password)
	}
	if err := nmModify(lteCon, kv...); err != nil {
		return nil, err
	}
	audit(r, "LTE settings changed (apn=%q roaming=%v autoconnect=%v)", b.APN, b.Roaming, b.Autoconnect)
	if b.Autoconnect {
		nmUpLater(lteCon)
	}
	return map[string]string{"message": "Saved. LTE reconnects with the new settings."}, nil
}

func apiLTEAction(r *http.Request) (any, error) {
	act := r.PathValue("action")
	var args []string
	switch act {
	case "connect":
		args = []string{"-w", "60", "connection", "up", lteCon}
	case "disconnect":
		args = []string{"connection", "down", lteCon}
	default:
		return nil, badRequest("unknown action")
	}
	audit(r, "LTE %s", act)
	if _, err := run(70*time.Second, "nmcli", args...); err != nil {
		return nil, err
	}
	refreshModem()
	return nil, nil
}

// ---------------- modem (ModemManager) ----------------

var (
	modemMu    sync.Mutex
	modemCache map[string]any
)

func mmJSON(args ...string) map[string]any {
	out, err := run(15*time.Second, "mmcli", append([]string{"-J"}, args...)...)
	if err != nil {
		return nil
	}
	var m map[string]any
	if json.Unmarshal([]byte(out), &m) != nil {
		return nil
	}
	return m
}

func dig(m map[string]any, path ...string) any {
	var cur any = m
	for _, p := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[p]
	}
	return cur
}

func str(v any) string {
	switch t := v.(type) {
	case string:
		if t == "--" {
			return ""
		}
		return t
	case []any:
		var s []string
		for _, x := range t {
			s = append(s, str(x))
		}
		return strings.Join(s, ", ")
	case nil:
		return ""
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func refreshModem() {
	res := map[string]any{"present": false}
	modem := mmJSON("-m", "any")
	if modem != nil {
		res["present"] = true
		res["modem"] = modem
		if sim := str(dig(modem, "modem", "generic", "sim")); sim != "" {
			res["sim"] = mmJSON("-i", sim)
		}
		res["signal"] = mmJSON("-m", "any", "--signal-get")
		if bs, ok := dig(modem, "modem", "generic", "bearers").([]any); ok {
			var bearers []any
			for _, b := range bs {
				if p := str(b); p != "" {
					if bj := mmJSON("-b", p); bj != nil {
						bearers = append(bearers, bj)
					}
				}
			}
			res["bearers"] = bearers
		}
	}
	modemMu.Lock()
	modemCache = res
	modemMu.Unlock()
}

func modemLoop() {
	time.Sleep(2 * time.Second)
	_, _ = run(10*time.Second, "mmcli", "-m", "any", "--signal-setup=10")
	// every 10 s while someone watches (web UI, awake LCD: wantModem), otherwise every minute
	var last time.Time
	for {
		now := time.Now()
		every := time.Minute
		if modemWatched(now) {
			every = 10 * time.Second
		}
		if now.Sub(last) >= every {
			refreshModem()
			last = now
		}
		time.Sleep(time.Second)
	}
}

func modemDetails() map[string]any {
	modemMu.Lock()
	defer modemMu.Unlock()
	return modemCache
}

func modemSummary() map[string]any {
	m := modemDetails()
	if m == nil {
		return map[string]any{"present": false}
	}
	mo, _ := m["modem"].(map[string]any)
	if mo == nil {
		return map[string]any{"present": false}
	}
	g := func(p ...string) string { return str(dig(mo, append([]string{"modem", "generic"}, p...)...)) }
	p3 := func(p ...string) string { return str(dig(mo, append([]string{"modem", "3gpp"}, p...)...)) }
	sim, _ := m["sim"].(map[string]any)
	sp := func(k string) string { return str(dig(sim, "sim", "properties", k)) }
	sg, _ := m["signal"].(map[string]any)
	lte := func(k string) string { return str(dig(sg, "modem", "signal", "lte", k)) }
	imei := p3("imei")
	if imei == "" {
		imei = g("equipment-identifier")
	}
	res := map[string]any{
		"present":        true,
		"state":          g("state"),
		"power_state":    g("power-state"),
		"operator":       p3("operator-name"),
		"operator_code":  p3("operator-code"),
		"registration":   p3("registration-state"),
		"packet_service": p3("packet-service-state"),
		"tech":           g("access-technologies"),
		"signal":         g("signal-quality", "value"),
		"rsrp":           lte("rsrp"),
		"rsrq":           lte("rsrq"),
		"snr":            lte("snr"),
		"rssi":           lte("rssi"),
		"modes":          g("current-modes"),
		"bands":          g("current-bands"),
		"own_number":     firstNonEmpty(g("own-numbers"), ownNumberOverride()),
		"imei":           imei,
		"imsi":           sp("imsi"),
		"iccid":          sp("iccid"),
		"sim_operator":   sp("operator-name"),
		"firmware":       strings.Join(strings.Fields(g("revision")), " "),
		"hw_revision":    g("hardware-revision"),
		"pin_retries":    g("unlock-retries"),
	}
	if bs, ok := m["bearers"].([]any); ok {
		for _, b := range bs {
			bm, _ := b.(map[string]any)
			if str(dig(bm, "bearer", "status", "connected")) != "yes" {
				continue
			}
			res["apn"] = str(dig(bm, "bearer", "properties", "apn"))
			if a := str(dig(bm, "bearer", "ipv4-config", "address")); a != "" {
				res["ipv4"] = a + "/" + str(dig(bm, "bearer", "ipv4-config", "prefix"))
			}
			res["ipv6"] = str(dig(bm, "bearer", "ipv6-config", "address"))
			res["session_s"] = str(dig(bm, "bearer", "stats", "duration"))
			break
		}
	}
	return res
}

// ownNumberOverride: most SIMs don't store their own number (MSISDN), so ModemManager reports none.
// /etc/router/own-number (one line, set by the admin) fills in for it.
const ownNumberPath = "/etc/router/own-number"

func ownNumberOverride() string { return readTrim(ownNumberPath) }

// lanIf: the interface that holds the LAN address (fixed at startup, so rules can name it before it exists).
var lanIf = func() string {
	if _, err := os.Stat(lanBridgeFile); err == nil {
		return lanBridgeIf
	}
	return "wlan0"
}()

// lanCon: the NetworkManager profile that holds the LAN address.
func lanCon() string {
	if lanIf == lanBridgeIf {
		return lanBridgeCon
	}
	return hotspotCon
}

// reapplyIfLANIfChanged: the rendered DHCP/firewall files are only rewritten on settings changes, so after
// kstick-setup wired-lan on/off (which restarts router-webui) they would still name the old LAN interface.
func reapplyIfLANIfChanged() {
	dns, _ := os.ReadFile(dnsmasqPath)
	fw, _ := os.ReadFile(fwPath)
	c := snapshot()
	if isClient(c) { // no dnsmasq; only the firewall and failover files can be out of date
		if len(fw) > 0 && withoutStamp(string(fw)) != withoutStamp(renderFirewall(c)) {
			log.Printf("firewall: rules changed by an update: re-applying")
			if err := applyFirewall(c); err != nil {
				log.Printf("firewall: %v", err)
			}
		}
		if wc, err := os.ReadFile(wanConfPath); err == nil && string(wc) != renderWANConf(c) {
			if err := applyWAN(c); err != nil {
				log.Printf("wan-failover: %v", err)
			}
		}
		return
	}
	if len(dns) == 0 {
		return
	}
	if strings.Contains(string(dns), "\ninterface="+lanIf+"\n") && strings.Contains(string(fw), `iifname "`+lanIf+`"`) {
		// an update changed what the firewall should contain (e.g. new counters or uplinks): re-apply it
		if len(fw) > 0 && withoutStamp(string(fw)) != withoutStamp(renderFirewall(c)) {
			log.Printf("firewall: rules changed by an update: re-applying")
			if err := applyFirewall(c); err != nil {
				log.Printf("firewall: %v", err)
			}
		}
		if wc, err := os.ReadFile(wanConfPath); err == nil && string(wc) != renderWANConf(c) {
			log.Printf("wan-failover: settings changed by an update: re-applying")
			if err := applyWAN(c); err != nil {
				log.Printf("wan-failover: %v", err)
			}
		}
		return
	}
	log.Printf("LAN interface is now %s: re-applying DHCP and firewall", lanIf)
	if err := applyDnsmasq(c); err != nil {
		log.Printf("LAN: dnsmasq: %v", err)
	}
	if err := applyFirewall(c); err != nil {
		log.Printf("LAN: firewall: %v", err)
	}
}

// wifiChannelArg: nmcli value for the hotspot channel. 0 (Automatic) must be sent as "" (reset to default):
// NetworkManager 1.52 rejects "0" as an invalid channel.
func wifiChannelArg(ch int) string {
	if ch <= 0 {
		return ""
	}
	return strconv.Itoa(ch)
}

// withoutStamp drops the "Generated by router-webui at <time>" line, so generated files can be compared.
func withoutStamp(s string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if !strings.HasPrefix(l, "# Generated by router-webui at ") {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}
