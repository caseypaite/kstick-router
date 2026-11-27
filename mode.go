package main

// Operating modes.
//
//   router (default): wlan0 is the hotspot LAN, with DHCP/DNS (dnsmasq), NAT, and LTE/USB uplinks.
//   client: wlan0 joins an existing Wi-Fi network (NetworkManager profile "wifi-client") as the primary
//     uplink; LTE is the backup. There is no hotspot, DHCP, DNS server, NAT or LAN, so the LAN-only
//     features (clients, DHCP, DNS, LAN IPv6, VPN routing for the LAN, Mesh -> LAN, the Wi-Fi off
//     schedule, port forwards and custom firewall rules) are off and their pages hidden. SMS, LTE, the
//     dashboards, Cloudflare Mesh, WhatsApp and the other forwarding targets keep working.
//
// Switching is a risky change (confirm.go): unless the new mode is confirmed in time, the old one comes
// back. Recovery over ssh or the console: `router-webui -set-mode router` (or `client`).

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const (
	modeRouter = "router"
	modeClient = "client"
	clientCon  = "wifi-client" // NetworkManager profile of the joined network
)

func isClient(c Config) bool { return c.Mode == modeClient }

var reHexPSK = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

// validClientWifi: the network client mode may join (PSK empty = open network).
func validClientWifi(ssid, psk string) error {
	if len(ssid) < 1 || len(ssid) > 32 || strings.IndexFunc(ssid, unicode.IsControl) >= 0 {
		return badRequest("network name (SSID) must be 1-32 bytes without control characters")
	}
	if psk == "" || reHexPSK.MatchString(psk) {
		return nil
	}
	if len(psk) < 8 || len(psk) > 63 || strings.IndexFunc(psk, func(r rune) bool { return r < 0x20 || r > 0x7e }) >= 0 {
		return badRequest("Wi-Fi password must be 8-63 printable ASCII characters (or empty for an open network)")
	}
	return nil
}

// validateMode is the mode part of validateConfig.
func validateMode(c *Config) error {
	switch c.Mode {
	case "", modeRouter:
		c.Mode = modeRouter
		return nil
	case modeClient:
	default:
		return badRequest("mode must be %q or %q", modeRouter, modeClient)
	}
	if lanIf != "wlan0" {
		return badRequest("Wi-Fi client mode needs the Wi-Fi interface, but it is a port of the wired LAN bridge (kstick-setup wired-lan off first)")
	}
	if err := validClientWifi(c.Client.SSID, c.Client.PSK); err != nil {
		return err
	}
	switch {
	case c.IPv6.Enabled:
		return badRequest("LAN IPv6 is not available in Wi-Fi client mode (there is no LAN)")
	case c.VPN.RouteLAN != "":
		return badRequest("routing the LAN through a VPN is not available in Wi-Fi client mode")
	case c.Firewall.MeshLAN:
		return badRequest("Mesh access to the LAN is not available in Wi-Fi client mode")
	case c.Schedule.WifiOff.Enabled:
		return badRequest("the Wi-Fi off schedule would cut the Wi-Fi uplink: not available in client mode")
	}
	return nil
}

// toClientMode switches c to client mode and turns off what only makes sense with a LAN; it returns
// what was turned off.
func toClientMode(c *Config, cl ClientCfg) []string {
	var off []string
	c.Mode, c.Client = modeClient, cl
	if c.IPv6.Enabled {
		c.IPv6.Enabled = false
		off = append(off, "LAN IPv6")
	}
	if c.VPN.RouteLAN != "" {
		c.VPN.RouteLAN, c.VPN.KillSwitch = "", false
		off = append(off, "VPN routing for the LAN")
	}
	if c.Firewall.MeshLAN {
		c.Firewall.MeshLAN = false
		off = append(off, "Mesh access to the LAN")
	}
	if c.Schedule.WifiOff.Enabled {
		c.Schedule.WifiOff.Enabled = false
		off = append(off, "the Wi-Fi off schedule")
	}
	return off
}

// nmNow: run the delayed NetworkManager steps at once (the -set-mode command exits right after).
var nmNow bool

// later runs fn shortly after the HTTP response was sent (it may cut the admin's own connection).
func later(fn func()) {
	if nmNow {
		fn()
		return
	}
	go func() {
		time.Sleep(1500 * time.Millisecond)
		fn()
	}()
}

// applyMode puts the system into c's mode. Router mode starts DHCP only after the hotspot is up, so
// dnsmasq never answers on the network the device was a client of.
func applyMode(c Config) error {
	if isClient(c) {
		changed, err := ensureClientProfile(c.Client)
		if err != nil {
			return err
		}
		if err := nmModify(hotspotCon, "connection.autoconnect", "no"); err != nil {
			log.Printf("mode: hotspot autoconnect: %v", err)
		}
		stopDnsmasq()
		if err := applyFirewall(c); err != nil {
			return err
		}
		if err := applyWAN(c); err != nil {
			return err
		}
		if changed || wlanConnection() != clientCon {
			later(func() { nmUp(clientCon) })
		}
		stHotspot.invalidate()
		return nil
	}
	if nmExists(clientCon) {
		if err := nmModify(clientCon, "connection.autoconnect", "no"); err != nil {
			log.Printf("mode: %s autoconnect: %v", clientCon, err)
		}
	}
	if err := nmModify(hotspotCon, "connection.autoconnect", "yes"); err != nil {
		return err
	}
	if err := writeDnsmasq(c); err != nil {
		return err
	}
	if err := applyFirewall(c); err != nil {
		return err
	}
	if err := applyWAN(c); err != nil {
		return err
	}
	later(func() {
		if wlanConnection() != hotspotCon {
			nmUp(hotspotCon)
		}
		if _, err := run(20*time.Second, "systemctl", "enable", "router-dnsmasq"); err != nil {
			log.Printf("mode: enable dnsmasq: %v", err)
		}
		if _, err := run(20*time.Second, "systemctl", "restart", "router-dnsmasq"); err != nil {
			log.Printf("mode: start dnsmasq: %v", err)
		}
		stHotspot.invalidate()
	})
	return nil
}

func nmUp(con string) {
	if _, err := run(60*time.Second, "nmcli", "-w", "45", "connection", "up", con); err != nil {
		log.Printf("activate %s: %v", con, err)
	}
}

func nmExists(con string) bool {
	_, err := run(10*time.Second, "nmcli", "-g", "connection.id", "connection", "show", con)
	return err == nil
}

// wlanConnection: the NetworkManager profile active on wlan0 ("" if none).
func wlanConnection() string {
	out, _ := run(10*time.Second, "nmcli", "-g", "GENERAL.CONNECTION", "device", "show", "wlan0")
	return strings.TrimSpace(out)
}

func stopDnsmasq() {
	if _, err := run(20*time.Second, "systemctl", "disable", "--now", "router-dnsmasq"); err != nil {
		log.Printf("mode: stop dnsmasq: %v", err)
	}
}

// ensureClientProfile creates or updates the "wifi-client" profile. An existing profile for the same
// network (made by hand or by NetworkManager) is adopted under that name, so a working connection is not
// interrupted. It reports whether the network or password changed (the profile must be re-activated).
func ensureClientProfile(cl ClientCfg) (bool, error) {
	name := ""
	if nmExists(clientCon) {
		name = clientCon
	} else {
		name = findWifiProfile(cl.SSID)
	}
	changed := false
	switch {
	case name == "":
		if _, err := run(20*time.Second, "nmcli", "connection", "add", "type", "wifi", "ifname", "wlan0",
			"con-name", clientCon, "ssid", cl.SSID, "connection.autoconnect", "no"); err != nil {
			return false, fmt.Errorf("create %s: %v", clientCon, err)
		}
		changed = true
	case name != clientCon:
		if err := nmModify(name, "connection.id", clientCon); err != nil {
			return false, err
		}
		log.Printf("mode: Wi-Fi profile %q adopted as %s", name, clientCon)
	}
	cur, _ := nmGet(clientCon, "802-11-wireless.ssid", "802-11-wireless-security.psk", "802-11-wireless-security.key-mgmt")
	if cur["802-11-wireless.ssid"] != cl.SSID || cur["802-11-wireless-security.psk"] != cl.PSK {
		changed = true
	}
	args := []string{"802-11-wireless.ssid", cl.SSID, "802-11-wireless.mode", "infrastructure",
		"802-11-wireless.powersave", "2", // power saving made the KStick's link slow and laggy
		"connection.interface-name", "wlan0", "connection.autoconnect", "yes",
		"connection.autoconnect-priority", "10", "connection.autoconnect-retries", "0",
		"ipv4.method", "auto", "ipv6.method", "auto"}
	if cl.PSK != "" {
		args = append(args, "802-11-wireless-security.key-mgmt", "wpa-psk", "802-11-wireless-security.psk", cl.PSK)
	}
	if err := nmModify(clientCon, args...); err != nil {
		return false, err
	}
	if cl.PSK == "" && cur["802-11-wireless-security.key-mgmt"] != "" {
		if _, err := run(15*time.Second, "nmcli", "connection", "modify", clientCon, "remove", "802-11-wireless-security"); err != nil {
			return false, err
		}
	}
	return changed, nil
}

// findWifiProfile: a Wi-Fi client (infrastructure) profile for ssid, other than the hotspot.
func findWifiProfile(ssid string) string {
	out, err := run(10*time.Second, "nmcli", "-t", "-f", "NAME,TYPE", "connection", "show")
	if err != nil {
		return ""
	}
	for _, l := range strings.Split(out, "\n") {
		f := splitNMTerse(l)
		if len(f) != 2 || f[1] != "802-11-wireless" || f[0] == hotspotCon {
			continue
		}
		m, err := nmGet(f[0], "802-11-wireless.ssid", "802-11-wireless.mode")
		if err == nil && m["802-11-wireless.ssid"] == ssid && m["802-11-wireless.mode"] != "ap" {
			return f[0]
		}
	}
	return ""
}

// splitNMTerse splits one line of `nmcli -t` output (':' separated, '\:' and '\\' escaped).
func splitNMTerse(l string) []string {
	var out []string
	var cur strings.Builder
	for i := 0; i < len(l); i++ {
		switch {
		case l[i] == '\\' && i+1 < len(l):
			i++
			cur.WriteByte(l[i])
		case l[i] == ':':
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(l[i])
		}
	}
	return append(out, cur.String())
}

// ---------------- Wi-Fi uplink state ----------------

var stWifiLink = ttlCache[map[string]any]{ttl: 5 * time.Second}

// wifiLink: the client connection on wlan0 (iw dev wlan0 link) plus its addresses.
func wifiLink() map[string]any {
	m := map[string]any{"connected": false, "connection": wlanConnection()}
	out, _ := run(5*time.Second, "iw", "dev", "wlan0", "link")
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		k, v, ok := strings.Cut(l, ":")
		if strings.HasPrefix(l, "Connected to ") {
			m["connected"] = true
			m["bssid"] = strings.Fields(l)[2]
			continue
		}
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch k {
		case "SSID":
			m["ssid"] = v
		case "freq":
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				m["freq_mhz"] = int(f)
			}
		case "signal":
			if n, err := strconv.Atoi(strings.Fields(v)[0]); err == nil {
				m["signal_dbm"] = n
			}
		case "tx bitrate":
			m["tx_bitrate"] = strings.Join(strings.Fields(v)[:min(2, len(strings.Fields(v)))], " ")
		case "rx bitrate":
			m["rx_bitrate"] = strings.Join(strings.Fields(v)[:min(2, len(strings.Fields(v)))], " ")
		}
	}
	m["ipv4"] = ifaceIPv4("wlan0")
	if f := strings.Fields(ipq("-4", "route", "show", "default", "dev", "wlan0")); len(f) >= 3 && f[1] == "via" {
		m["gateway"] = f[2]
	}
	return m
}

// ---------------- API ----------------

func apiGetMode(r *http.Request) (any, error) {
	c := snapshot()
	out := map[string]any{
		"mode":       c.Mode,
		"client":     map[string]any{"ssid": c.Client.SSID, "has_psk": c.Client.PSK != "", "lan_admin": c.Client.LANAdmin},
		"can_client": lanIf == "wlan0",
		"lan":        c.LAN.prefix().Addr().String(),
	}
	if isClient(c) {
		out["link"] = stWifiLink.get(wifiLink)
	} else {
		out["hotspot"] = stHotspot.get(hotspotBrief)
	}
	return out, nil
}

func apiPutMode(r *http.Request) (any, error) {
	var b struct {
		Mode     string `json:"mode"`
		SSID     string `json:"ssid"`
		PSK      string `json:"psk"`  // empty: keep the saved password when the network is the same
		Open     bool   `json:"open"` // an open network (no password)
		LANAdmin bool   `json:"lan_admin"`
	}
	if err := readJSON(r, &b); err != nil {
		return nil, err
	}
	prev := snapshot()
	var off []string
	mutate := func(c *Config) { c.Mode = b.Mode }
	switch b.Mode {
	case modeRouter:
	case modeClient:
		cl := ClientCfg{SSID: b.SSID, PSK: b.PSK, LANAdmin: b.LANAdmin}
		if b.Open {
			cl.PSK = ""
		} else if cl.PSK == "" {
			if b.SSID != prev.Client.SSID || prev.Client.PSK == "" {
				return nil, badRequest("enter the Wi-Fi password (or tick \"open network\")")
			}
			cl.PSK = prev.Client.PSK
		}
		mutate = func(c *Config) { off = toClientMode(c, cl) }
	default:
		return nil, badRequest("mode must be %q or %q", modeRouter, modeClient)
	}
	res, err := updateCfg(r, "operating mode", mutate, applyMode)
	if err != nil {
		return nil, err
	}
	stWifiLink.invalidate()
	kickVPN()
	kickIPv6()
	var msg string
	switch {
	case b.Mode == modeRouter && prev.Mode != modeRouter:
		msg = "Switching to router mode: the Wi-Fi hotspot comes back. Connect to it (https://" + prev.LAN.prefix().Addr().String() + ") or use Cloudflare Mesh over LTE"
	case b.Mode == modeClient && prev.Mode != modeClient:
		msg = fmt.Sprintf("Switching to Wi-Fi client mode: the hotspot turns off and the device joins %q. Reach it over Cloudflare Mesh", b.SSID)
		if b.LANAdmin {
			msg += " or from that network"
		}
	default:
		msg = "Saved"
	}
	if len(off) > 0 {
		msg += ". Turned off: " + strings.Join(off, ", ")
	}
	audit(r, "operating mode: %s (ssid %q, admin from that network %v)", b.Mode, b.SSID, b.LANAdmin)
	out := withPending(res, "operating mode", prev, true)
	if m, ok := out.(map[string]any); ok {
		m["message"] = msg + ", then confirm, or the previous mode comes back."
		return m, nil
	}
	return map[string]string{"message": msg + "."}, nil
}

// apiWifiScan lists the networks wlan0 can see (client mode only: a hotspot does not scan).
func apiWifiScan(r *http.Request) (any, error) {
	out, err := run(30*time.Second, "nmcli", "-t", "-f", "IN-USE,SSID,SIGNAL,SECURITY,FREQ", "device", "wifi", "list", "ifname", "wlan0", "--rescan", "yes")
	if err != nil {
		return nil, err
	}
	type net struct {
		SSID     string `json:"ssid"`
		Signal   int    `json:"signal"`
		Security string `json:"security"`
		FreqMHz  int    `json:"freq_mhz"`
		InUse    bool   `json:"in_use"`
	}
	best := map[string]net{}
	for _, l := range strings.Split(out, "\n") {
		f := splitNMTerse(l)
		if len(f) != 5 || f[1] == "" {
			continue
		}
		sig, _ := strconv.Atoi(f[2])
		freq, _ := strconv.Atoi(strings.Fields(f[4] + " 0")[0])
		n := net{SSID: f[1], Signal: sig, Security: f[3], FreqMHz: freq, InUse: f[0] == "*"}
		if o, ok := best[n.SSID]; !ok || n.Signal > o.Signal || n.InUse {
			n.InUse = n.InUse || o.InUse
			best[n.SSID] = n
		}
	}
	list := make([]net, 0, len(best))
	for _, n := range best {
		list = append(list, n)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Signal > list[j].Signal })
	return list, nil
}

// routerOnly refuses an API call in client mode (the feature needs the hotspot LAN).
func routerOnly(f apiFunc) apiFunc {
	return func(r *http.Request) (any, error) {
		if isClient(snapshot()) {
			return nil, &httpError{http.StatusConflict, "not available in Wi-Fi client mode (there is no LAN); switch to router mode first (Network > Operating mode)"}
		}
		return f(r)
	}
}

// clientOnly is the opposite of routerOnly.
func clientOnly(f apiFunc) apiFunc {
	return func(r *http.Request) (any, error) {
		if !isClient(snapshot()) {
			return nil, &httpError{http.StatusConflict, "only available in Wi-Fi client mode"}
		}
		return f(r)
	}
}

// ---------------- command line: router-webui -set-mode router|client ----------------

// cliSetMode switches the mode without the confirm step (recovery over ssh or the console), then
// restarts the running web UI so it picks up the new config. Client mode needs a network: the one in
// the config, or else the Wi-Fi network wlan0 is connected to right now.
func cliSetMode(mode string) error {
	cfgMu.Lock()
	next := cloneConfig(cfg)
	cfgMu.Unlock()
	switch mode {
	case modeRouter:
		next.Mode = modeRouter
	case modeClient:
		cl := next.Client
		if cl.SSID == "" {
			con := wlanConnection()
			if con == "" || con == hotspotCon {
				return errors.New("no Wi-Fi network configured: connect wlan0 to one first (nmcli device wifi connect <SSID> password <password>) or use the web UI")
			}
			m, err := nmGet(con, "802-11-wireless.ssid", "802-11-wireless-security.psk")
			if err != nil {
				return err
			}
			cl = ClientCfg{SSID: m["802-11-wireless.ssid"], PSK: m["802-11-wireless-security.psk"], LANAdmin: true}
			fmt.Printf("using the network wlan0 is connected to: %q (profile %q)\n", cl.SSID, con)
		}
		for _, o := range toClientMode(&next, cl) {
			fmt.Printf("turned off: %s\n", o)
		}
	default:
		return fmt.Errorf("mode must be %q or %q", modeRouter, modeClient)
	}
	if err := validateConfig(&next); err != nil {
		return err
	}
	nmNow = true
	if err := applyMode(next); err != nil {
		return err
	}
	cfgMu.Lock()
	cfg = next
	err := saveConfig()
	cfgMu.Unlock()
	if err != nil {
		return err
	}
	_ = os.Remove(pendingPath) // a change pending from before must not undo this at the next start
	if _, err := run(60*time.Second, "systemctl", "try-restart", "router-webui"); err != nil {
		log.Printf("restart router-webui: %v", err)
	}
	fmt.Printf("mode: %s\n", next.Mode)
	return nil
}
