package main

import (
	"strings"
	"testing"
)

func TestWantDNSTunnel(t *testing.T) {
	c := defaultConfig()
	if got := wantDNSTunnel(c, "wgx"); got != "" {
		t.Errorf("no LAN routing: %q", got)
	}
	c.VPN.RouteLAN = "wg-warp"
	if got := wantDNSTunnel(c, "wg-warp"); got != "wg-warp" {
		t.Errorf("tunnel up: %q", got)
	}
	if got := wantDNSTunnel(c, ""); got != "" {
		t.Errorf("tunnel down, no kill switch: DNS falls back with the LAN: %q", got)
	}
	c.VPN.KillSwitch = true
	if got := wantDNSTunnel(c, ""); got != "wg-warp" {
		t.Errorf("tunnel down + kill switch: DNS must stay on the (missing) tunnel: %q", got)
	}
}

func withDNSTunnel(t *testing.T, dev string) {
	old, _ := vpnDNSDev.Load().(string)
	vpnDNSDev.Store(dev)
	t.Cleanup(func() { vpnDNSDev.Store(old) })
}

func TestDNSUpstreams(t *testing.T) {
	c := defaultConfig()
	normalize(&c)
	withDNSTunnel(t, "wg-warp")
	// LAN not routed through a tunnel: a stale device is ignored
	if got := strings.Join(dnsUpstreams(c), " "); got != "127.0.0.1" {
		t.Errorf("WAN default: %q", got)
	}
	c.DNS.Upstreams = []string{"8.8.8.8", "2001:4860:4860::8888"}
	if got := strings.Join(dnsUpstreams(c), " "); got != "8.8.8.8 2001:4860:4860::8888" {
		t.Errorf("WAN configured: %q", got)
	}
	c.VPN.RouteLAN = "wg-warp"
	if got := strings.Join(dnsUpstreams(c), " "); got != "8.8.8.8@wg-warp" {
		t.Errorf("tunnel, configured (IPv4 only): %q", got)
	}
	c.DNS.Upstreams = []string{"2001:4860:4860::8888"}
	if got := strings.Join(dnsUpstreams(c), " "); got != "1.1.1.1@wg-warp 9.9.9.9@wg-warp" {
		t.Errorf("tunnel, only IPv6 configured: %q", got)
	}
	c.DNS.Upstreams = nil
	if got := strings.Join(dnsUpstreams(c), " "); got != "1.1.1.1@wg-warp 9.9.9.9@wg-warp" {
		t.Errorf("tunnel, none configured (never the carrier DNS): %q", got)
	}
}

func TestDnsmasqTunnelRender(t *testing.T) {
	c := defaultConfig()
	normalize(&c)
	c.VPN.RouteLAN = "wg-warp"
	c.DNS.UpdateBlock.Mode = "always"
	withDNSTunnel(t, "wg-warp")
	conf := renderDnsmasq(c)
	if strings.Contains(conf, "server=127.0.0.1") || !strings.Contains(conf, "server=1.1.1.1@wg-warp") {
		t.Errorf("dnsmasq.conf leaks or lacks the tunnel servers:\n%s", conf)
	}
	if got := dnsmasqBoundTo(conf); got != "wg-warp" {
		t.Errorf("dnsmasqBoundTo = %q", got)
	}
	ub := renderUpdateBlock(c, true)
	for _, l := range strings.Split(ub, "\n") {
		if l != "" && !strings.HasSuffix(l, "/") && !strings.HasSuffix(l, "@wg-warp") {
			t.Errorf("update-block allow line bypasses the tunnel: %q", l)
		}
	}
	if !strings.Contains(ub, "@wg-warp") {
		t.Errorf("expected allowed names bound to the tunnel:\n%s", ub)
	}
	vpnDNSDev.Store("")
	if got := dnsmasqBoundTo(renderDnsmasq(c)); got != "" {
		t.Errorf("unbound config reports %q", got)
	}
}
