package main

import (
	"log"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// LAN IPv6 via a WireGuard tunnel (NAT66):
//   the LAN interface (wlan0, or br-lan with a wired LAN) gets <ULA>::1/64; dnsmasq advertises the prefix (RA + RDNSS, MTU 1280).
//   ip -6 rule 900: from <ULA> lookup main suppress_prefixlength 0
//   ip -6 rule 901: from <ULA> lookup 201  -> default dev <tunnel> | unreachable default
//   nft: oifname "wg*" ip6 saddr <ULA> masquerade
// While the tunnel is unhealthy the RA router lifetime is 0 and routes are
// "unreachable", so clients fall back to IPv4 at once.

const (
	v6Table     = "201"
	v6FailAfter = 3
)

var (
	v6TunnelUp   atomic.Bool
	v6Kick       = make(chan struct{}, 1)
	v6mu         sync.Mutex
	v6state      = map[string]any{"active": false}
	v6fails      = v6FailAfter
	v6ForceProbe atomic.Bool
	v6probe      probeGate
)

// probeGate decides when the IPv6 health probe must actually ping. Each ping wakes the LTE radio
// (full transmit power for ~10 s), so a healthy tunnel is not pinged while its counters show traffic in
// both directions (replies are coming back), and only every probeIdle while it carries nothing.
// A forced probe, a recent failure, or one-way traffic still probes, at most every probeMin.
type probeGate struct {
	dev       string
	rx, tx    uint64
	lastProbe time.Time
}

const (
	probeMin  = 30 * time.Second
	probeIdle = 5 * time.Minute
)

func (g *probeGate) need(dev string, rx, tx uint64, failing, force bool, now time.Time) bool {
	prx, ptx, same := g.rx, g.tx, g.dev == dev
	g.dev, g.rx, g.tx = dev, rx, tx
	since := now.Sub(g.lastProbe)
	switch {
	case force || !same:
		return true
	case since < probeMin:
		return false
	case failing:
		return true
	case rx > prx && tx > ptx:
		return false
	case rx == prx && tx == ptx:
		return since >= probeIdle
	}
	return true // one-way traffic: is the tunnel stalled?
}

// probed records a probe; the counters read after it, so its own packets don't count as traffic.
func (g *probeGate) probed(rx, tx uint64, now time.Time) { g.rx, g.tx, g.lastProbe = rx, tx, now }

func ifCounters(dev string) (rx, tx uint64) {
	rx, _ = strconv.ParseUint(readTrim("/sys/class/net/"+dev+"/statistics/rx_bytes"), 10, 64)
	tx, _ = strconv.ParseUint(readTrim("/sys/class/net/"+dev+"/statistics/tx_bytes"), 10, 64)
	return
}

func kickIPv6() {
	select {
	case v6Kick <- struct{}{}:
	default:
	}
}

func ipv6Loop() {
	t := time.NewTicker(time.Minute) // plus network events (netwatch.go)
	for {
		reconcileIPv6()
		waitNext(t, v6Kick, v6Net)
	}
}

func lanV6Addr(c Config) (netip.Prefix, netip.Addr) {
	p := netip.MustParsePrefix(c.IPv6.ULA)
	return p, p.Addr().Next()
}

func v6Global(dev string) string {
	out := ipq("-6", "-o", "addr", "show", "dev", dev, "scope", "global")
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		for i := range f {
			if f[i] == "inet6" && i+1 < len(f) {
				return strings.Split(f[i+1], "/")[0]
			}
		}
	}
	return ""
}

func ensureV6Rules(lan string) {
	rules := ipq("-6", "rule", "show")
	w900 := "900:\tfrom " + lan + " lookup main suppress_prefixlength 0"
	w901 := "901:\tfrom " + lan + " lookup " + v6Table
	if strings.Contains(rules, w900) && strings.Contains(rules, w901) {
		return
	}
	delV6Rules()
	ipq("-6", "rule", "add", "priority", "900", "from", lan, "lookup", "main", "suppress_prefixlength", "0")
	ipq("-6", "rule", "add", "priority", "901", "from", lan, "lookup", v6Table)
}

func delV6Rules() {
	for _, prio := range []string{"900", "901"} {
		for i := 0; i < 5 && strings.Contains(ipq("-6", "rule", "show", "priority", prio), prio+":"); i++ {
			ipq("-6", "rule", "del", "priority", prio)
		}
	}
}

func setV6State(m map[string]any) {
	m["updated"] = time.Now().Format(time.RFC3339)
	v6mu.Lock()
	v6state = m
	v6mu.Unlock()
}

func setTunnelUp(up bool, c Config, why string) {
	if v6TunnelUp.Swap(up) != up {
		log.Printf("ipv6: LAN IPv6 internet %s (%s)", map[bool]string{true: "UP", false: "DOWN"}[up], why)
		if err := applyDnsmasq(c); err != nil { // router lifetime in RA follows the tunnel
			log.Printf("ipv6: dnsmasq: %v", err)
		}
	}
}

func reconcileIPv6() {
	c := snapshot()
	lan, routerIP := lanV6Addr(c)
	if !c.IPv6.Enabled {
		if strings.Contains(ipq("-6", "addr", "show", "dev", lanIf), routerIP.String()) {
			ipq("-6", "addr", "del", routerIP.String()+"/64", "dev", lanIf)
		}
		delV6Rules()
		ipq("-6", "route", "flush", "table", v6Table)
		ensureTunnelOifRoute("")
		v6TunnelUp.Store(false)
		setV6State(map[string]any{"active": false, "note": "IPv6 for the LAN is off"})
		return
	}

	_, _ = run(5*time.Second, "sysctl", "-q", "-w", "net.ipv6.conf.all.forwarding=1", "net.ipv6.conf."+lanIf+".disable_ipv6=0")
	ensureHotspotIPv6()
	if !strings.Contains(ipq("-6", "addr", "show", "dev", lanIf), routerIP.String()+"/64") {
		ipq("-6", "addr", "replace", routerIP.String()+"/64", "dev", lanIf, "nodad")
	}
	ensureV6Rules(lan.String())

	dev := ""
	for _, w := range wgConnections() {
		if w.Name == c.IPv6.Tunnel && w.Active && w.Device != "" {
			dev = w.Device
		}
	}
	egress := ""
	if dev != "" {
		egress = v6Global(dev)
	}
	ensureTunnelOifRoute(dev)
	// probe the tunnel only when its traffic doesn't already prove it works (probeGate)
	needProbe := false
	if dev != "" && egress != "" {
		rx, tx := ifCounters(dev)
		needProbe = v6probe.need(dev, rx, tx, v6fails > 0, v6ForceProbe.Swap(false), time.Now())
	}
	if needProbe {
		ok := false
		for _, t := range []string{"2606:4700:4700::1111", "2001:4860:4860::8888"} {
			if _, err := run(8*time.Second, "ping", "-6", "-n", "-c1", "-W3", "-I", dev, t); err == nil {
				ok = true
				break
			}
		}
		rx, tx := ifCounters(dev)
		v6probe.probed(rx, tx, time.Now())
		if ok {
			v6fails = 0
		} else if v6fails < 100 {
			v6fails++
		}
	} else if dev == "" || egress == "" {
		v6fails = v6FailAfter
	}
	up := dev != "" && egress != "" && v6fails < v6FailAfter
	cur := strings.TrimSpace(ipq("-6", "route", "show", "table", v6Table))
	why := "tunnel healthy"
	switch {
	case up:
		if !strings.HasPrefix(cur, "default dev "+dev) {
			ipq("-6", "route", "replace", "default", "dev", dev, "table", v6Table)
		}
	default:
		if !strings.HasPrefix(cur, "unreachable default") {
			ipq("-6", "route", "replace", "unreachable", "default", "table", v6Table)
		}
		switch {
		case dev == "":
			why = "tunnel " + c.IPv6.Tunnel + " is not running"
		case egress == "":
			why = "tunnel " + dev + " has no IPv6 address"
		default:
			why = "no IPv6 reply through " + dev
		}
	}
	setTunnelUp(up, c, why)
	setV6State(map[string]any{
		"active": true, "internet": up, "note": why, "lan_prefix": lan.String(), "router_address": routerIP.String(),
		"tunnel": c.IPv6.Tunnel, "device": dev, "egress_address": egress, "probe_fails": v6fails,
	})
}

// ensureHotspotIPv6 stops NetworkManager from disabling IPv6 on the AP; the
// addresses are managed here. Restarts Wi-Fi once when it changes the profile.
var hotspotV6Done bool

func ensureHotspotIPv6() {
	if hotspotV6Done {
		return
	}
	m, err := nmGet(lanCon(), "ipv6.method")
	if err != nil {
		return
	}
	if m["ipv6.method"] != "ignore" {
		if err := nmModify(lanCon(), "ipv6.method", "ignore"); err != nil {
			log.Printf("ipv6: hotspot: %v", err)
			return
		}
		log.Printf("ipv6: %s profile set to ipv6.method=ignore, restarting it once", lanCon())
		nmUpLater(lanCon())
	}
	hotspotV6Done = true
}

func v6Snapshot() map[string]any {
	v6mu.Lock()
	defer v6mu.Unlock()
	out := map[string]any{}
	for k, v := range v6state {
		out[k] = v
	}
	return out
}

func apiGetIPv6(r *http.Request) (any, error) {
	var tunnels []string
	for _, w := range wgConnections() {
		tunnels = append(tunnels, w.Name)
	}
	if tunnels == nil {
		tunnels = []string{}
	}
	return map[string]any{"config": snapshot().IPv6, "status": v6Snapshot(), "tunnels": tunnels}, nil
}

func apiPutIPv6(r *http.Request) (any, error) {
	var b struct {
		Enabled bool   `json:"enabled"`
		Tunnel  string `json:"tunnel"`
	}
	if err := readJSON(r, &b); err != nil {
		return nil, err
	}
	if b.Tunnel != "" {
		found := false
		for _, w := range wgConnections() {
			found = found || w.Name == b.Tunnel
		}
		if !found {
			return nil, badRequest("unknown WireGuard connection %q", b.Tunnel)
		}
	}
	res, err := updateCfgConfirm(r, "IPv6", func(c *Config) { c.IPv6.Enabled, c.IPv6.Tunnel = b.Enabled, b.Tunnel }, applyFirewall, applyDnsmasq)
	v6ForceProbe.Store(true)
	kickIPv6()
	return res, err
}

// ensureTunnelOifRoute lets sockets bound to the tunnel (our health probe) send IPv6
// through it: rule 902 "oif <dev> lookup 202", table 202 "default dev <dev>".
// IPv6, unlike IPv4, will not send on a bound device without a matching route.
const v6OifTable = "202"

func ensureTunnelOifRoute(dev string) {
	rule := ipq("-6", "rule", "show", "priority", "902")
	if dev == "" {
		if strings.Contains(rule, "902:") {
			ipq("-6", "rule", "del", "priority", "902")
			ipq("-6", "route", "flush", "table", v6OifTable)
		}
		return
	}
	if !strings.Contains(rule, "oif "+dev+" lookup "+v6OifTable) {
		for i := 0; i < 5 && strings.Contains(ipq("-6", "rule", "show", "priority", "902"), "902:"); i++ {
			ipq("-6", "rule", "del", "priority", "902")
		}
		ipq("-6", "rule", "add", "priority", "902", "oif", dev, "lookup", v6OifTable)
	}
	if !strings.HasPrefix(strings.TrimSpace(ipq("-6", "route", "show", "table", v6OifTable)), "default dev "+dev) {
		ipq("-6", "route", "replace", "default", "dev", dev, "table", v6OifTable)
	}
}
