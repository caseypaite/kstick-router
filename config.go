package main

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/netip"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// Config is everything the web UI owns. Wi-Fi radio and LTE bearer settings live
// in NetworkManager and are edited there directly.
type Config struct {
	Mode      string      `json:"mode"`   // "router" (default: hotspot LAN) or "client" (Wi-Fi client uplink), mode.go
	Client    ClientCfg   `json:"client"` // the Wi-Fi network joined in client mode
	LAN       LANCfg      `json:"lan"`
	Firewall  FirewallCfg `json:"firewall"`
	DNS       DNSCfg      `json:"dns"`
	WAN       WANCfg      `json:"wan"`
	VPN       VPNCfg      `json:"vpn"`
	IPv6      IPv6Cfg     `json:"ipv6"`
	Thermal   ThermalCfg  `json:"thermal"`     // thermal guard (thermal.go)
	Data      DataCfg     `json:"data"`        // LTE data usage caps (datausage.go)
	Admin     AdminCfg    `json:"admin"`       // confirm-or-revert for risky changes (confirm.go)
	SMSCtl    SMSCtlCfg   `json:"sms_control"` // commands by SMS from allowed numbers (smsctl.go)
	Battery   BatteryCfg  `json:"battery"`     // MF800 low-battery protection (battery.go)
	Radio     RadioCfg    `json:"radio"`       // LTE band lock (signal.go)
	Schedule  SchedCfg    `json:"schedule"`    // LTE/Wi-Fi off times, weekly reboot, device internet times (schedule.go)
	Bluetooth BTCfg       `json:"bluetooth"`   // Bluetooth on/off (bluetooth.go)
	UIStyle   string      `json:"ui_style"`    // web UI served: "classic" (default) or "xp" (ui.go)
}

// ClientCfg: Wi-Fi client mode (mode.go). The device joins this network for internet (LTE is the backup);
// there is no hotspot, DHCP or LAN. PSK empty = an open network.
type ClientCfg struct {
	SSID     string `json:"ssid"`
	PSK      string `json:"psk"`
	LANAdmin bool   `json:"lan_admin"` // SSH and the web UI from the joined Wi-Fi network (Mesh and USB are separate)
}

// AdminCfg: risky changes (firewall, LAN, VPN routing, IPv6, blocking a client) are undone after
// ConfirmSecs unless confirmed in the web UI. 0 turns this off.
type AdminCfg struct {
	ConfirmSecs int `json:"confirm_secs"`
}

// IPv6Cfg: LAN clients get a ULA prefix; their IPv6 traffic leaves NAT66'd through a
// WireGuard tunnel that has IPv6 (e.g. Cloudflare WARP), because the LTE carrier
// provides no usable native IPv6.
type IPv6Cfg struct {
	Enabled bool   `json:"enabled"`
	Tunnel  string `json:"tunnel"` // WireGuard connection carrying LAN IPv6
	ULA     string `json:"ula"`    // LAN prefix, fdXX:XXXX:XXXX::/64
}

type LANCfg struct {
	Address     string        `json:"address"` // router address/prefix, e.g. 192.168.4.1/24
	DHCPEnabled bool          `json:"dhcp_enabled"`
	DHCPStart   string        `json:"dhcp_start"`
	DHCPEnd     string        `json:"dhcp_end"`
	LeaseTime   string        `json:"lease_time"` // 30m, 12h, 7d, infinite
	Static      []StaticLease `json:"static_leases"`
}

type StaticLease struct {
	MAC  string `json:"mac"`
	IP   string `json:"ip"`
	Name string `json:"name"`
}

type FirewallCfg struct {
	WANPing      bool          `json:"wan_ping"`
	LANSSH       bool          `json:"lan_ssh"`
	LogDropped   bool          `json:"log_dropped"`
	MeshAdmin    bool          `json:"mesh_admin"` // ping/SSH/web UI from Cloudflare Mesh (CloudflareWARP)
	MeshLAN      bool          `json:"mesh_lan"`   // Mesh devices may reach the Wi-Fi LAN
	Rules        []Rule        `json:"rules"`
	PortForwards []PortForward `json:"port_forwards"`
	Blocked      []Blocked     `json:"blocked"`
}

type Rule struct {
	Enabled bool   `json:"enabled"`
	Chain   string `json:"chain"`  // input | forward
	Action  string `json:"action"` // accept | drop | reject
	In      string `json:"in"`     // any | lan | wan | usb | vpn
	Out     string `json:"out"`    // any | lan | wan | usb | vpn (forward only)
	Proto   string `json:"proto"`  // any | tcp | udp | tcp/udp | icmp
	Src     string `json:"src"`
	Dst     string `json:"dst"`
	DPort   string `json:"dport"`
	Log     bool   `json:"log"`
	Comment string `json:"comment"`
}

type PortForward struct {
	Enabled bool   `json:"enabled"`
	Proto   string `json:"proto"` // tcp | udp | tcp/udp
	ExtPort string `json:"ext_port"`
	IntIP   string `json:"int_ip"`
	IntPort string `json:"int_port"` // empty = same as external
	Source  string `json:"source"`   // optional allowed source CIDR
	Comment string `json:"comment"`
}

type Blocked struct {
	MAC     string `json:"mac"`
	Comment string `json:"comment"`
}

type DNSCfg struct {
	Upstreams        []string       `json:"upstreams"` // empty = follow the active WAN's DNS
	Hosts            []HostEntry    `json:"hosts"`
	Blocklist        []string       `json:"blocklist"`
	UpdateBlock      UpdateBlockCfg `json:"update_block"` // block OS update servers (updateblock.go)
	RebindProtection bool           `json:"rebind_protection"`
}

type HostEntry struct {
	Name string `json:"name"`
	IP   string `json:"ip"`
}

type WANCfg struct {
	Order     []string `json:"order"`
	Targets   []string `json:"targets"`
	Interval  int      `json:"interval"`
	FailAfter int      `json:"fail_after"`
}

type VPNCfg struct {
	RouteLAN   string `json:"route_lan"` // WireGuard connection carrying all LAN traffic ("" = none)
	KillSwitch bool   `json:"kill_switch"`
}

var (
	cfgMu   sync.Mutex // serialises config changes + applies
	cfg     Config
	cfgFile string
)

func defaultConfig() Config {
	return Config{
		LAN: LANCfg{Address: "192.168.4.1/24", DHCPEnabled: true, DHCPStart: "192.168.4.10", DHCPEnd: "192.168.4.254", LeaseTime: "12h"},
		Firewall: FirewallCfg{
			LANSSH: true,
		},
		DNS:     DNSCfg{UpdateBlock: defaultUpdateBlock()},
		Thermal: defaultThermal(),
		Data:    defaultData(),
		Admin:   AdminCfg{ConfirmSecs: 90},
		Battery: defaultBattery(),
		WAN:     WANCfg{Order: []string{"wwan0", "usb0"}, Targets: []string{"1.1.1.1", "8.8.8.8", "9.9.9.9"}, Interval: 15, FailAfter: 3},
	}
}

func loadConfig(path string) error {
	cfgFile = path
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		cfg = defaultConfig()
		normalize(&cfg)
		log.Printf("no config at %s, using defaults", path)
		return saveConfig()
	}
	if err != nil {
		return err
	}
	c := defaultConfig()
	if err := json.Unmarshal(b, &c); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	hadULA := c.IPv6.ULA != ""
	if err := validateConfig(&c); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	cfg = c
	if !hadULA { // persist the newly generated IPv6 prefix so it stays stable
		return saveConfig()
	}
	return nil
}

func saveConfig() error {
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(cfgFile, append(b, '\n'), 0o600)
}

func snapshot() Config {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	b, _ := json.Marshal(cfg)
	var c Config
	_ = json.Unmarshal(b, &c)
	return c
}

// ---- validation ----

var (
	reMAC     = regexp.MustCompile(`^([0-9a-f]{2}:){5}[0-9a-f]{2}$`)
	reHost    = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]{0,62})(\.[a-zA-Z0-9]([a-zA-Z0-9-]{0,62}))*$`)
	reDomain  = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,62})\.)+[a-z][a-z0-9-]{0,62}$`)
	reComment = regexp.MustCompile(`^[\w .,:/()@#+=-]{0,64}$`)
	reLease   = regexp.MustCompile(`^([1-9][0-9]{0,4}[mhd]|infinite)$`)
)

// ifaceNames["lan"] becomes lanIf at startup (init in render.go).
var ifaceNames = map[string]string{"lan": "wlan0", "wan": "wwan*", "usb": "usb0", "vpn": "wg*"}

func normMAC(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

func checkComment(s, what string) error {
	if !reComment.MatchString(s) {
		return badRequest("%s: comment may only contain letters, digits and . , : / ( ) @ # + = - (max 64)", what)
	}
	return nil
}

func parseIPv4(s string) (netip.Addr, error) {
	a, err := netip.ParseAddr(strings.TrimSpace(s))
	if err != nil || !a.Is4() {
		return netip.Addr{}, fmt.Errorf("%q is not an IPv4 address", s)
	}
	return a, nil
}

// checkAddrOrNet accepts an IPv4/IPv6 address or prefix; empty is allowed.
func checkAddrOrNet(s string) error {
	if s == "" {
		return nil
	}
	if _, err := netip.ParseAddr(s); err == nil {
		return nil
	}
	if _, err := netip.ParsePrefix(s); err == nil {
		return nil
	}
	return fmt.Errorf("%q is not an IP address or CIDR network", s)
}

// checkPorts validates "80", "1000-2000" or "80,443,8000-8010".
func checkPorts(s string, allowList bool) error {
	if s == "" {
		return nil
	}
	items := strings.Split(s, ",")
	if len(items) > 16 || (!allowList && len(items) > 1) {
		return fmt.Errorf("too many ports in %q", s)
	}
	for _, it := range items {
		lo, hi, isRange := strings.Cut(strings.TrimSpace(it), "-")
		a, err1 := strconv.Atoi(lo)
		b := a
		var err2 error
		if isRange {
			b, err2 = strconv.Atoi(hi)
		}
		if err1 != nil || err2 != nil || a < 1 || b > 65535 || a > b {
			return fmt.Errorf("invalid port or range %q", it)
		}
	}
	return nil
}

func normPorts(s string) string { return strings.ReplaceAll(strings.TrimSpace(s), " ", "") }

func (l LANCfg) prefix() netip.Prefix {
	p, _ := netip.ParsePrefix(l.Address)
	return p
}

// normalize replaces nil lists with empty ones so the API never sends JSON null.
func normalize(c *Config) {
	if c.LAN.Static == nil {
		c.LAN.Static = []StaticLease{}
	}
	if c.Firewall.Rules == nil {
		c.Firewall.Rules = []Rule{}
	}
	if c.Firewall.PortForwards == nil {
		c.Firewall.PortForwards = []PortForward{}
	}
	if c.Firewall.Blocked == nil {
		c.Firewall.Blocked = []Blocked{}
	}
	if c.DNS.Upstreams == nil {
		c.DNS.Upstreams = []string{}
	}
	if c.DNS.Hosts == nil {
		c.DNS.Hosts = []HostEntry{}
	}
	if c.DNS.Blocklist == nil {
		c.DNS.Blocklist = []string{}
	}
	if c.Radio.Bands == nil {
		c.Radio.Bands = []string{}
	}
	if c.Schedule.Devices == nil {
		c.Schedule.Devices = []DevSched{}
	}
	if c.SMSCtl.Numbers == nil {
		c.SMSCtl.Numbers = []string{}
	}
	if c.IPv6.ULA == "" {
		c.IPv6.ULA = randomULA()
	}
}

// randomULA returns an RFC 4193 prefix with a random 40-bit global ID.
func randomULA() string {
	b := make([]byte, 16)
	b[0] = 0xfd
	if _, err := rand.Read(b[1:6]); err != nil {
		panic(err)
	}
	return netip.PrefixFrom(netip.AddrFrom16([16]byte(b)), 64).String()
}

func validateConfig(c *Config) error {
	normalize(c)
	// LAN
	p, err := netip.ParsePrefix(strings.TrimSpace(c.LAN.Address))
	if err != nil || !p.Addr().Is4() || p.Bits() < 16 || p.Bits() > 30 || !p.Addr().IsPrivate() {
		return badRequest("LAN address must be a private IPv4 address with prefix /16../30, e.g. 192.168.4.1/24")
	}
	if p.Addr() == p.Masked().Addr() {
		return badRequest("LAN address must be a host address, not the network address")
	}
	for _, other := range []string{"10.42.0.0/24", "192.168.5.0/24"} {
		if op := netip.MustParsePrefix(other); op.Overlaps(p) {
			return badRequest("LAN network must not overlap the USB management networks (%s)", other)
		}
	}
	c.LAN.Address = p.String()
	if !reLease.MatchString(c.LAN.LeaseTime) {
		return badRequest("lease time must look like 30m, 12h, 7d or infinite")
	}
	net := p.Masked()
	s, err1 := parseIPv4(c.LAN.DHCPStart)
	e, err2 := parseIPv4(c.LAN.DHCPEnd)
	if err1 != nil || err2 != nil || !net.Contains(s) || !net.Contains(e) || e.Less(s) {
		return badRequest("DHCP range must be two addresses inside %s, start <= end", net)
	}
	seenMAC, seenIP := map[string]bool{}, map[string]bool{}
	for i := range c.LAN.Static {
		l := &c.LAN.Static[i]
		l.MAC, l.Name = normMAC(l.MAC), strings.TrimSpace(l.Name)
		if !reMAC.MatchString(l.MAC) {
			return badRequest("static lease %d: invalid MAC %q", i+1, l.MAC)
		}
		a, err := parseIPv4(l.IP)
		if err != nil || !net.Contains(a) || a == p.Addr() {
			return badRequest("static lease %d: IP must be inside %s and not the router address", i+1, net)
		}
		l.IP = a.String()
		if l.Name != "" && (!reHost.MatchString(l.Name) || strings.Contains(l.Name, ".")) {
			return badRequest("static lease %d: invalid host name %q", i+1, l.Name)
		}
		if seenMAC[l.MAC] || seenIP[l.IP] {
			return badRequest("static lease %d: duplicate MAC or IP", i+1)
		}
		seenMAC[l.MAC], seenIP[l.IP] = true, true
	}

	// Firewall
	f := &c.Firewall
	if len(f.Rules) > 200 || len(f.PortForwards) > 100 || len(f.Blocked) > 256 {
		return badRequest("too many firewall entries")
	}
	for i := range f.Rules {
		r := &f.Rules[i]
		where := fmt.Sprintf("rule %d", i+1)
		r.Src, r.Dst, r.DPort = strings.TrimSpace(r.Src), strings.TrimSpace(r.Dst), normPorts(r.DPort)
		if r.Chain != "input" && r.Chain != "forward" {
			return badRequest("%s: chain must be input or forward", where)
		}
		if r.Action != "accept" && r.Action != "drop" && r.Action != "reject" {
			return badRequest("%s: action must be accept, drop or reject", where)
		}
		for _, v := range []string{r.In, r.Out} {
			if _, ok := ifaceNames[v]; !ok && v != "any" && v != "" {
				return badRequest("%s: unknown interface %q", where, v)
			}
		}
		if r.In == "" {
			r.In = "any"
		}
		if r.Out == "" || r.Chain == "input" {
			r.Out = "any"
		}
		switch r.Proto {
		case "any", "icmp":
			if r.DPort != "" {
				return badRequest("%s: ports need protocol tcp, udp or tcp/udp", where)
			}
		case "tcp", "udp", "tcp/udp":
		default:
			return badRequest("%s: invalid protocol %q", where, r.Proto)
		}
		if err := checkAddrOrNet(r.Src); err != nil {
			return badRequest("%s: source: %v", where, err)
		}
		if err := checkAddrOrNet(r.Dst); err != nil {
			return badRequest("%s: destination: %v", where, err)
		}
		if r.Src != "" && r.Dst != "" && isV4(r.Src) != isV4(r.Dst) {
			return badRequest("%s: source and destination must be the same IP family", where)
		}
		if err := checkPorts(r.DPort, true); err != nil {
			return badRequest("%s: %v", where, err)
		}
		if err := checkComment(r.Comment, where); err != nil {
			return err
		}
	}
	for i := range f.PortForwards {
		pf := &f.PortForwards[i]
		where := fmt.Sprintf("port forward %d", i+1)
		pf.ExtPort, pf.IntPort, pf.Source = normPorts(pf.ExtPort), normPorts(pf.IntPort), strings.TrimSpace(pf.Source)
		if pf.Proto != "tcp" && pf.Proto != "udp" && pf.Proto != "tcp/udp" {
			return badRequest("%s: protocol must be tcp, udp or tcp/udp", where)
		}
		if pf.ExtPort == "" {
			return badRequest("%s: external port is required", where)
		}
		if err := checkPorts(pf.ExtPort, false); err != nil {
			return badRequest("%s: %v", where, err)
		}
		if err := checkPorts(pf.IntPort, false); err != nil || strings.Contains(pf.IntPort, "-") {
			return badRequest("%s: internal port must be a single port or empty", where)
		}
		if strings.Contains(pf.ExtPort, "-") && pf.IntPort != "" {
			return badRequest("%s: a port range keeps the same ports; leave internal port empty", where)
		}
		a, err := parseIPv4(pf.IntIP)
		if err != nil || !net.Contains(a) {
			return badRequest("%s: internal IP must be inside the LAN %s", where, net)
		}
		pf.IntIP = a.String()
		if pf.Source != "" {
			if err := checkAddrOrNet(pf.Source); err != nil || !isV4(pf.Source) {
				return badRequest("%s: source must be an IPv4 address or network", where)
			}
		}
		if err := checkComment(pf.Comment, where); err != nil {
			return err
		}
	}
	seen := map[string]bool{}
	for i := range f.Blocked {
		b := &f.Blocked[i]
		b.MAC = normMAC(b.MAC)
		if !reMAC.MatchString(b.MAC) || seen[b.MAC] {
			return badRequest("blocked client %d: invalid or duplicate MAC %q", i+1, b.MAC)
		}
		seen[b.MAC] = true
		if err := checkComment(b.Comment, fmt.Sprintf("blocked client %d", i+1)); err != nil {
			return err
		}
	}

	// DNS
	d := &c.DNS
	if len(d.Upstreams) > 8 || len(d.Hosts) > 200 || len(d.Blocklist) > 5000 {
		return badRequest("too many DNS entries")
	}
	for i, u := range d.Upstreams {
		a, err := netip.ParseAddr(strings.TrimSpace(u))
		if err != nil {
			return badRequest("DNS server %q is not an IP address", u)
		}
		d.Upstreams[i] = a.String()
	}
	for i := range d.Hosts {
		h := &d.Hosts[i]
		h.Name = strings.ToLower(strings.TrimSpace(h.Name))
		if !reHost.MatchString(h.Name) || len(h.Name) > 253 {
			return badRequest("host record %d: invalid name %q", i+1, h.Name)
		}
		a, err := netip.ParseAddr(strings.TrimSpace(h.IP))
		if err != nil {
			return badRequest("host record %d: invalid IP %q", i+1, h.IP)
		}
		h.IP = a.String()
	}
	clean := d.Blocklist[:0]
	dup := map[string]bool{}
	for _, b := range d.Blocklist {
		b = strings.ToLower(strings.TrimSpace(b))
		if b == "" || dup[b] {
			continue
		}
		if !reDomain.MatchString(b) || len(b) > 253 {
			return badRequest("blocklist: invalid domain %q", b)
		}
		dup[b] = true
		clean = append(clean, b)
	}
	d.Blocklist = clean
	if err := validateUpdateBlock(&d.UpdateBlock); err != nil {
		return err
	}
	if err := validateThermal(&c.Thermal); err != nil {
		return err
	}
	if err := validateData(&c.Data); err != nil {
		return err
	}
	if err := validateSMSCtl(&c.SMSCtl); err != nil {
		return err
	}
	if err := validateBattery(&c.Battery); err != nil {
		return err
	}
	if err := validateRadio(&c.Radio); err != nil {
		return err
	}
	if err := validateSchedule(&c.Schedule); err != nil {
		return err
	}
	if err := validateBTCfg(&c.Bluetooth); err != nil {
		return err
	}
	if a := c.Admin.ConfirmSecs; a != 0 && (a < 30 || a > 600) {
		return badRequest("confirm time must be 0 (off) or 30-600 seconds")
	}

	// WAN failover
	w := &c.WAN
	if len(w.Order) == 0 || len(w.Order) > 2 {
		return badRequest("WAN order must list wwan0 and/or usb0")
	}
	seenW := map[string]bool{}
	for _, o := range w.Order {
		if (o != "wwan0" && o != "usb0") || seenW[o] {
			return badRequest("WAN order: invalid or duplicate uplink %q", o)
		}
		seenW[o] = true
	}
	if len(w.Targets) == 0 || len(w.Targets) > 5 {
		return badRequest("give 1-5 health-check targets")
	}
	for i, t := range w.Targets {
		a, err := parseIPv4(t)
		if err != nil {
			return badRequest("health-check target: %v", err)
		}
		w.Targets[i] = a.String()
	}
	if w.Interval < 5 || w.Interval > 300 || w.FailAfter < 1 || w.FailAfter > 20 {
		return badRequest("interval must be 5-300 s and fail-after 1-20 checks")
	}

	// IPv6
	up, err := netip.ParsePrefix(c.IPv6.ULA)
	if err != nil || up.Bits() != 64 || up.Masked() != up || !netip.MustParsePrefix("fd00::/8").Contains(up.Addr()) {
		return badRequest("IPv6 LAN prefix must be a ULA /64 inside fd00::/8")
	}
	if c.IPv6.Tunnel != "" && !reWGName.MatchString(c.IPv6.Tunnel) {
		return badRequest("invalid IPv6 tunnel name %q", c.IPv6.Tunnel)
	}
	if c.IPv6.Enabled && c.IPv6.Tunnel == "" {
		return badRequest("choose the WireGuard tunnel that carries IPv6")
	}

	// VPN
	if c.VPN.RouteLAN != "" && !reWGName.MatchString(c.VPN.RouteLAN) {
		return badRequest("invalid VPN connection name %q", c.VPN.RouteLAN)
	}
	return validateMode(c)
}

func isV4(s string) bool {
	if a, err := netip.ParseAddr(s); err == nil {
		return a.Is4()
	}
	if p, err := netip.ParsePrefix(s); err == nil {
		return p.Addr().Is4()
	}
	return false
}
