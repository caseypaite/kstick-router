package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
)

// WireGuard connections are NetworkManager "wireguard" profiles named wg*. Their
// interface carries the same name. The router itself always stays on the WAN;
// only LAN traffic is steered into the tunnel, by policy routing:
//   rule 899: from <LAN> lookup main suppress_prefixlength 0   (local/connected routes)
//   rule 900: from <LAN> lookup 200                            (default via the tunnel / blackhole)

const vpnTable = "200"

var reWGName = regexp.MustCompile(`^wg[a-z0-9_-]{1,13}$`)

type wgConn struct {
	Name        string `json:"name"`
	UUID        string `json:"uuid"`
	Device      string `json:"device"`
	Active      bool   `json:"active"`
	Autoconnect bool   `json:"autoconnect"`
}

func wgConnections() []wgConn {
	out, err := run(10*time.Second, "nmcli", "-t", "-f", "NAME,UUID,TYPE,DEVICE,AUTOCONNECT,ACTIVE", "connection", "show")
	if err != nil {
		return nil
	}
	var res []wgConn
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		f := splitTerse(l)
		if len(f) < 6 || f[2] != "wireguard" {
			continue
		}
		res = append(res, wgConn{Name: f[0], UUID: f[1], Device: f[3], Autoconnect: f[4] == "yes", Active: f[5] == "yes"})
	}
	return res
}

// splitTerse splits nmcli -t output on unescaped colons.
func splitTerse(l string) []string {
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

func vpnBrief() map[string]any {
	c := snapshot()
	active := []string{}
	for _, w := range wgConnections() {
		if w.Active {
			active = append(active, w.Name)
		}
	}
	return map[string]any{"route_lan": c.VPN.RouteLAN, "kill_switch": c.VPN.KillSwitch, "active": active}
}

var vpnKick = make(chan struct{}, 1)

func kickVPN() {
	stVPN.invalidate()
	select {
	case vpnKick <- struct{}{}:
	default:
	}
}

func vpnLoop() {
	t := time.NewTicker(time.Minute) // plus network events (netwatch.go)
	for {
		reconcileVPN()
		reconcileMeshRule()
		waitNext(t, vpnKick, vpnNet)
	}
}

func ipq(args ...string) string {
	out, _ := run(5*time.Second, "ip", args...)
	return out
}

func reconcileVPN() {
	c := snapshot()
	syncVPNDNS(c, reconcileVPNRoutes(c))
}

// reconcileVPNRoutes keeps the LAN policy routing in line with the config and returns the running
// tunnel device that carries the LAN ("" if none).
func reconcileVPNRoutes(c Config) string {
	lan := c.LAN.prefix().Masked().String()
	rules := ipq("-4", "rule", "show")
	want899 := fmt.Sprintf("899:\tfrom %s lookup main suppress_prefixlength 0", lan)
	want900 := fmt.Sprintf("900:\tfrom %s lookup %s", lan, vpnTable)

	if c.VPN.RouteLAN == "" {
		if strings.Contains(rules, "899:") || strings.Contains(rules, "900:") {
			ipq("-4", "rule", "del", "priority", "899")
			ipq("-4", "rule", "del", "priority", "900")
			ipq("-4", "route", "flush", "table", vpnTable)
			log.Printf("vpn: LAN routing via tunnel disabled")
		}
		return ""
	}
	if !strings.Contains(rules, want899) || !strings.Contains(rules, want900) {
		for strings.Contains(ipq("-4", "rule", "show", "priority", "899"), "899") {
			if _, err := run(5*time.Second, "ip", "-4", "rule", "del", "priority", "899"); err != nil {
				break
			}
		}
		for strings.Contains(ipq("-4", "rule", "show", "priority", "900"), "900") {
			if _, err := run(5*time.Second, "ip", "-4", "rule", "del", "priority", "900"); err != nil {
				break
			}
		}
		ipq("-4", "rule", "add", "priority", "899", "from", lan, "lookup", "main", "suppress_prefixlength", "0")
		ipq("-4", "rule", "add", "priority", "900", "from", lan, "lookup", vpnTable)
	}
	dev := ""
	for _, w := range wgConnections() {
		if w.Name == c.VPN.RouteLAN && w.Active && w.Device != "" {
			dev = w.Device
		}
	}
	cur := strings.TrimSpace(ipq("-4", "route", "show", "table", vpnTable))
	switch {
	case dev != "":
		if cur != "default dev "+dev+" scope link" {
			ipq("-4", "route", "replace", "default", "dev", dev, "table", vpnTable)
			log.Printf("vpn: LAN traffic -> %s", dev)
		}
	case c.VPN.KillSwitch:
		if !strings.HasPrefix(cur, "blackhole default") {
			ipq("-4", "route", "replace", "blackhole", "default", "table", vpnTable)
			log.Printf("vpn: tunnel %s down, kill switch blocks LAN internet", c.VPN.RouteLAN)
		}
	default:
		if cur != "" {
			ipq("-4", "route", "flush", "table", vpnTable)
			log.Printf("vpn: tunnel %s down, LAN falls back to WAN", c.VPN.RouteLAN)
		}
	}
	return dev
}

// ---- DNS follows the LAN into the tunnel ----
//
// LAN clients resolve through dnsmasq on the router, and the router's own traffic stays on the WAN, so
// with "Route LAN through" a tunnel the queries would still leave over LTE: the carrier would see every
// name the LAN looks up. While the LAN is routed through a tunnel, dnsmasq's upstream servers are bound
// to the tunnel device (server=IP@dev, SO_BINDTODEVICE; IPv4 then sends on the device even though the
// router's routes point at the WAN). With the kill switch on and the tunnel down, they stay bound to the
// missing tunnel, so lookups fail rather than leak; without it, DNS falls back to the WAN with the LAN.

var vpnDNSDev atomic.Value // string: device dnsmasq's upstreams are bound to ("" = normal WAN DNS)

// defaultTunnelDNS: used through a tunnel when no IPv4 DNS server is configured (the carrier's DNS,
// which the router follows otherwise, is usually only reachable from the carrier's network).
var defaultTunnelDNS = []string{"1.1.1.1", "9.9.9.9"}

// dnsTunnel: the device DNS must go through for this config ("" = WAN).
func dnsTunnel(c Config) string {
	if c.VPN.RouteLAN == "" {
		return ""
	}
	d, _ := vpnDNSDev.Load().(string)
	return d
}

// wantDNSTunnel: which device DNS should be bound to, given the running tunnel device.
func wantDNSTunnel(c Config, dev string) string {
	switch {
	case c.VPN.RouteLAN == "":
		return ""
	case dev != "":
		return dev
	case c.VPN.KillSwitch:
		return c.VPN.RouteLAN // NetworkManager names a WireGuard interface after its connection
	}
	return ""
}

// dnsmasqBoundTo: the device the dnsmasq.conf on disk binds its upstreams to ("" = none).
func dnsmasqBoundTo(conf string) string {
	for _, l := range strings.Split(conf, "\n") {
		if v, ok := strings.CutPrefix(l, "server="); ok {
			if _, dev, ok := strings.Cut(v, "@"); ok {
				return dev
			}
		}
	}
	return ""
}

func syncVPNDNS(c Config, dev string) {
	want := wantDNSTunnel(c, dev)
	vpnDNSDev.Store(want)
	conf, err := os.ReadFile(dnsmasqPath)
	if err != nil || dnsmasqBoundTo(string(conf)) == want {
		return
	}
	if want != "" {
		log.Printf("vpn: LAN DNS now goes through %s", want)
	} else {
		log.Printf("vpn: LAN DNS back on the WAN")
	}
	if err := applyDnsmasq(c); err != nil {
		log.Printf("vpn: dnsmasq: %v", err)
	}
}

func apiGetVPN(r *http.Request) (any, error) {
	wgShow := runCombined(10*time.Second, "wg", "show")
	return map[string]any{
		"connections": wgConnections(),
		"routing":     snapshot().VPN,
		"dns_via":     dnsTunnel(snapshot()),
		"wg_show":     wgShow,
		"table":       ipq("-4", "route", "show", "table", vpnTable),
	}, nil
}

func apiImportVPN(r *http.Request) (any, error) {
	var b struct {
		Name   string `json:"name"`
		Config string `json:"config"`
	}
	if err := readJSON(r, &b); err != nil {
		return nil, err
	}
	b.Name = strings.TrimSpace(b.Name)
	if !reWGName.MatchString(b.Name) {
		return nil, badRequest("name must start with \"wg\" and use 3-15 lowercase letters, digits, _ or - (e.g. wg-warp)")
	}
	for _, w := range wgConnections() {
		if w.Name == b.Name {
			return nil, badRequest("a connection named %s already exists", b.Name)
		}
	}
	if len(b.Config) > 16<<10 || !strings.Contains(b.Config, "[Interface]") || !strings.Contains(b.Config, "PrivateKey") || !strings.Contains(b.Config, "[Peer]") {
		return nil, badRequest("paste a complete WireGuard config with [Interface] (PrivateKey) and [Peer] sections")
	}
	// wg-quick hook directives would run shell commands; NetworkManager ignores them, but refuse to be explicit.
	for _, l := range strings.Split(b.Config, "\n") {
		k := strings.ToLower(strings.TrimSpace(strings.SplitN(l, "=", 2)[0]))
		if k == "preup" || k == "postup" || k == "predown" || k == "postdown" {
			return nil, badRequest("PreUp/PostUp/PreDown/PostDown lines are not supported; remove them")
		}
	}
	dir, err := os.MkdirTemp("", "wgimport")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, b.Name+".conf")
	if err := os.WriteFile(path, []byte(b.Config), 0o600); err != nil {
		return nil, err
	}
	if _, err := run(20*time.Second, "nmcli", "connection", "import", "type", "wireguard", "file", path); err != nil {
		return nil, err
	}
	// The router keeps its own routing; LAN steering is done by policy rules (see top).
	if err := nmModify(b.Name, "connection.autoconnect", "no", "wireguard.peer-routes", "no",
		"wireguard.ip4-auto-default-route", "no", "wireguard.ip6-auto-default-route", "no",
		"ipv4.never-default", "yes", "ipv6.never-default", "yes",
		// the router keeps resolving through its WAN; DNS= lines are meant for full-tunnel clients
		"ipv4.dns", "", "ipv6.dns", "", "ipv4.ignore-auto-dns", "yes", "ipv6.ignore-auto-dns", "yes"); err != nil {
		return nil, err
	}
	_, _ = run(20*time.Second, "nmcli", "connection", "down", b.Name)
	audit(r, "WireGuard connection %s imported", b.Name)
	return map[string]string{"message": "Imported " + b.Name + ". Bring it up, then choose it under \"Route LAN through\"."}, nil
}

func apiVPNAction(r *http.Request) (any, error) {
	name, act := r.PathValue("name"), r.PathValue("action")
	found := false
	for _, w := range wgConnections() {
		if w.Name == name {
			found = true
		}
	}
	if !found || !reWGName.MatchString(name) {
		return nil, badRequest("unknown WireGuard connection")
	}
	var err error
	switch act {
	case "up":
		_, err = run(40*time.Second, "nmcli", "-w", "30", "connection", "up", name)
	case "down":
		_, err = run(20*time.Second, "nmcli", "connection", "down", name)
	case "autoconnect-on":
		err = nmModify(name, "connection.autoconnect", "yes")
	case "autoconnect-off":
		err = nmModify(name, "connection.autoconnect", "no")
	case "delete":
		if snapshot().VPN.RouteLAN == name {
			return nil, badRequest("stop routing the LAN through %s first", name)
		}
		if c := snapshot(); c.IPv6.Enabled && c.IPv6.Tunnel == name {
			return nil, badRequest("%s carries LAN IPv6; turn IPv6 off or pick another tunnel first", name)
		}
		_, err = run(20*time.Second, "nmcli", "connection", "delete", name)
	default:
		return nil, badRequest("unknown action")
	}
	if err != nil {
		return nil, err
	}
	audit(r, "WireGuard %s: %s", name, act)
	kickVPN()
	v6ForceProbe.Store(true)
	kickIPv6()
	return nil, nil
}

func apiPutVPNRouting(r *http.Request) (any, error) {
	var v VPNCfg
	if err := readJSON(r, &v); err != nil {
		return nil, err
	}
	if v.RouteLAN != "" {
		ok := false
		for _, w := range wgConnections() {
			if w.Name == v.RouteLAN {
				ok = true
			}
		}
		if !ok {
			return nil, badRequest("unknown WireGuard connection %q", v.RouteLAN)
		}
	}
	res, err := updateCfgConfirm(r, "VPN routing", func(c *Config) { c.VPN = v })
	kickVPN()
	return res, err
}

// ---------------- SMS / USSD ----------------

var (
	rePhone = regexp.MustCompile(`^\+?[0-9]{3,15}$`)
	reUSSD  = regexp.MustCompile(`^[*#0-9]{2,32}$`)
	reSMSID = regexp.MustCompile(`^[0-9]{1,6}$`)
)

func apiUSSD(r *http.Request) (any, error) {
	var b struct {
		Code   string `json:"code"`
		Action string `json:"action"` // initiate | respond | cancel
	}
	if err := readJSON(r, &b); err != nil {
		return nil, err
	}
	var arg string
	switch b.Action {
	case "cancel":
		arg = "--3gpp-ussd-cancel"
	case "respond", "initiate", "":
		if !reUSSD.MatchString(b.Code) {
			return nil, badRequest("USSD code may contain only digits, * and #")
		}
		if b.Action == "respond" {
			arg = "--3gpp-ussd-respond=" + b.Code
		} else {
			arg = "--3gpp-ussd-initiate=" + b.Code
		}
	default:
		return nil, badRequest("unknown action")
	}
	if b.Action == "initiate" || b.Action == "" {
		// A session the network never closed (or a menu left unanswered) blocks every new code with
		// "WrongState: a session is already active": a new code means the old session is abandoned.
		if st := ussdStatus(); st != "" && st != "idle" {
			runCombined(15*time.Second, "mmcli", "-m", "any", "--3gpp-ussd-cancel")
		}
	}
	out := runCombined(45*time.Second, "mmcli", "-m", "any", arg)
	if arg != "--3gpp-ussd-cancel" && strings.Contains(out, "session is already active") {
		runCombined(15*time.Second, "mmcli", "-m", "any", "--3gpp-ussd-cancel")
		if b.Action == "respond" {
			out = "The USSD session had already ended; start again with the code."
		} else {
			out = runCombined(45*time.Second, "mmcli", "-m", "any", arg)
		}
	}
	if strings.Contains(out, "NoNetworkFound") || strings.Contains(out, "NoNetwork") {
		// The failed request leaves ModemManager's session "active"; clear it so the next try isn't refused.
		runCombined(15*time.Second, "mmcli", "-m", "any", "--3gpp-ussd-cancel")
		out = strings.TrimSpace(out) + "\n\nThe network did not take the USSD request. On LTE, USSD needs a fallback to 2G/3G, " +
			"and this modem's firmware is limited to 4G, so USSD codes cannot reach the operator. " +
			"Check balances by SMS or the operator's app instead."
	}
	audit(r, "USSD %s %s", firstNonEmpty(b.Action, "initiate"), b.Code)
	return map[string]string{"output": strings.TrimSpace(out)}, nil
}

// reconcileMeshRule: rule 898 "to 100.96.0.0/12 lookup 65743" sends traffic to Cloudflare Mesh devices
// into the Cloudflare One client's table, ahead of wan-failover's rules 1000/1001 (whose table 100
// default route would send it out of the uplink instead: the client's own rule is only at 32765) and the
// VPN steering rules 899/900 (which would push LAN replies into a WireGuard tunnel). Always kept: without
// the Cloudflare client the table is empty and lookups fall through to the next rule.
const warpTable = "65743"

func reconcileMeshRule() {
	want := true
	have := strings.Contains(ipq("-4", "rule", "show", "priority", "898"), "898:")
	switch {
	case want && !have:
		ipq("-4", "rule", "add", "priority", "898", "to", "100.96.0.0/12", "lookup", warpTable)
	case !want && have:
		ipq("-4", "rule", "del", "priority", "898")
	}
}

// ussdStatus: ModemManager's 3GPP USSD state (idle | active | user-response), "" if unknown.
func ussdStatus() string {
	out, err := run(10*time.Second, "mmcli", "-m", "any", "--3gpp-ussd-status", "-K")
	if err != nil {
		return ""
	}
	return parseUSSDStatus(out)
}

func parseUSSDStatus(kv string) string {
	for _, l := range strings.Split(kv, "\n") {
		if k, v, ok := strings.Cut(l, ":"); ok && strings.TrimSpace(k) == "modem.3gpp.ussd.status" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
