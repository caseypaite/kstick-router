package main

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

func sampleConfig() Config {
	c := defaultConfig()
	c.Firewall.WANPing = true
	c.Firewall.LogDropped = true
	c.Firewall.Rules = []Rule{
		{Enabled: true, Chain: "forward", Action: "drop", In: "lan", Out: "wan", Proto: "tcp/udp", DPort: "25,465,587", Log: true, Comment: "no smtp"},
		{Enabled: true, Chain: "input", Action: "reject", In: "lan", Proto: "tcp", Src: "192.168.4.0/25", DPort: "22", Comment: "ssh lower half"},
		{Enabled: true, Chain: "forward", Action: "accept", In: "lan", Out: "vpn", Proto: "any", Dst: "2606:4700::/32", Comment: "v6 test"},
		{Enabled: true, Chain: "forward", Action: "drop", In: "any", Out: "any", Proto: "icmp", Comment: ""},
	}
	c.Firewall.PortForwards = []PortForward{
		{Enabled: true, Proto: "tcp", ExtPort: "8080", IntIP: "192.168.4.50", IntPort: "80", Comment: "web"},
		{Enabled: true, Proto: "tcp/udp", ExtPort: "10000-10010", IntIP: "192.168.4.51", Source: "100.64.0.0/10", Comment: "range"},
	}
	c.Firewall.Blocked = []Blocked{{MAC: "AA:BB:CC:DD:EE:FF", Comment: "kid tablet"}}
	c.LAN.Static = []StaticLease{{MAC: "32:0c:9c:0b:01:20", IP: "192.168.4.18", Name: "redmi"}}
	c.Firewall.MeshAdmin, c.Firewall.MeshLAN = true, true
	c.IPv6 = IPv6Cfg{Enabled: true, Tunnel: "wg-warp", ULA: "fd12:3456:789a::/64"}
	c.DNS = DNSCfg{Upstreams: []string{"1.1.1.1"}, Hosts: []HostEntry{{Name: "nas.lan", IP: "192.168.4.50"}}, Blocklist: []string{"Doubleclick.net"}, RebindProtection: true}
	return c
}

func TestRender(t *testing.T) {
	c := sampleConfig()
	if err := validateConfig(&c); err != nil {
		t.Fatal(err)
	}
	os.WriteFile("/tmp/claude-1000/-home-ck-AI/89728961-8a4e-46a9-93fc-ff13022714c4/scratchpad/test-fw.nft", []byte(renderFirewall(c)), 0o644)
	os.WriteFile("/tmp/claude-1000/-home-ck-AI/89728961-8a4e-46a9-93fc-ff13022714c4/scratchpad/test-dnsmasq.conf", []byte(renderDnsmasq(c)), 0o644)
}

func TestValidateRejects(t *testing.T) {
	bad := []func(*Config){
		func(c *Config) {
			c.Firewall.Rules = []Rule{{Chain: "input", Action: "accept", Proto: "tcp", DPort: "22; flush ruleset"}}
		},
		func(c *Config) {
			c.Firewall.Rules = []Rule{{Chain: "input", Action: "accept", Proto: "any", Comment: `x" ; drop`}}
		},
		func(c *Config) {
			c.Firewall.PortForwards = []PortForward{{Proto: "tcp", ExtPort: "80", IntIP: "8.8.8.8"}}
		},
		func(c *Config) { c.DNS.Blocklist = []string{"evil.com\nserver=6.6.6.6"} },
		func(c *Config) { c.DNS.Hosts = []HostEntry{{Name: "a,b", IP: "1.1.1.1"}} },
		func(c *Config) { c.LAN.Address = "10.42.0.1/24" },
		func(c *Config) { c.LAN.LeaseTime = "12h\ndhcp-option=6,6.6.6.6" },
		func(c *Config) { c.WAN.Order = []string{"wwan0; reboot"} },
		func(c *Config) { c.IPv6 = IPv6Cfg{Enabled: true, Tunnel: "wg-warp", ULA: "2001:db8::/64"} },
		func(c *Config) { c.IPv6 = IPv6Cfg{Enabled: true, Tunnel: "", ULA: "fd00:1::/64"} },
		func(c *Config) { c.IPv6 = IPv6Cfg{Enabled: true, Tunnel: "wg-x\" ; drop", ULA: "fd00:1::/64"} },
	}
	for i, f := range bad {
		c := defaultConfig()
		f(&c)
		if err := validateConfig(&c); err == nil {
			t.Errorf("case %d: expected validation error", i)
		}
	}
}

func TestNoNullLists(t *testing.T) {
	c := defaultConfig()
	if err := validateConfig(&c); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(c)
	if bytes.Contains(b, []byte("null")) {
		t.Fatalf("config JSON contains null: %s", b)
	}
}
