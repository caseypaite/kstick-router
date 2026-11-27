package main

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
)

func TestClientModeValidation(t *testing.T) {
	c := sampleConfig() // router mode with LAN IPv6, Mesh -> LAN, …
	if err := validateConfig(&c); err != nil || c.Mode != modeRouter {
		t.Fatalf("router default: %v %q", err, c.Mode)
	}
	bad := cloneConfig(c)
	bad.Mode, bad.Client = modeClient, ClientCfg{SSID: "Home", PSK: "secret123"}
	if validateConfig(&bad) == nil {
		t.Fatal("client mode with LAN IPv6 / Mesh -> LAN on must be refused")
	}
	c.Schedule.WifiOff = Window{true, "01:00", "06:00"}
	off := toClientMode(&c, ClientCfg{SSID: "Home", PSK: "secret123", LANAdmin: true})
	if err := validateConfig(&c); err != nil {
		t.Fatalf("after toClientMode: %v", err)
	}
	if len(off) != 3 || c.IPv6.Enabled || c.Firewall.MeshLAN || c.Schedule.WifiOff.Enabled {
		t.Fatalf("turned off %v", off)
	}
	for _, tc := range []ClientCfg{{SSID: ""}, {SSID: "x", PSK: "short"}, {SSID: strings.Repeat("s", 33)}} {
		c.Client = tc
		if validateConfig(&c) == nil {
			t.Errorf("accepted %+v", tc)
		}
	}
	for _, tc := range []ClientCfg{{SSID: "open"}, {SSID: "hex", PSK: strings.Repeat("ab", 32)}} {
		c.Client = tc
		if err := validateConfig(&c); err != nil {
			t.Errorf("%+v: %v", tc, err)
		}
	}
	c.Mode = "bridge"
	if validateConfig(&c) == nil {
		t.Error("unknown mode accepted")
	}
}

func TestClientFirewallAndWAN(t *testing.T) {
	c := sampleConfig()
	toClientMode(&c, ClientCfg{SSID: `My "Net"`, PSK: "secret123", LANAdmin: true})
	if err := validateConfig(&c); err != nil {
		t.Fatal(err)
	}
	fw := renderFirewall(c)
	for _, want := range []string{`counter lte_rx`, `set lan_tx`, `iifname "wlan0" udp dport 68 accept`,
		`iifname "wlan0" tcp dport { 22, 80, 443 } accept`, `iifname "CloudflareWARP" tcp dport { 22, 80, 443 }`,
		`comment "dropped input"`, `comment "dropped forward"`, `policy drop`} {
		if !strings.Contains(fw, want) {
			t.Errorf("client firewall lacks %q", want)
		}
	}
	for _, bad := range []string{"masquerade", "dnat", "lan_input", "udp dport { 53, 67, 547 }"} {
		if strings.Contains(fw, bad) {
			t.Errorf("client firewall has %q", bad)
		}
	}
	if !strings.Contains(fw, `"My \"Net\""`) { // the SSID in the comment line is quoted (%q)
		t.Error("SSID comment not quoted")
	}
	c.Client.LANAdmin = false
	if strings.Contains(renderFirewall(c), `iifname "wlan0" tcp dport`) {
		t.Error("lan_admin off still opens the web UI to the joined network")
	}
	c.WAN.Order = []string{"wwan0", "wlan0", "usb0"}
	if got := wanList(c); !slices.Equal(got, []string{"wlan0", "wwan0", "usb0"}) {
		t.Errorf("wanList %v", got)
	}
	c.Mode = modeRouter
	if got := wanList(c); slices.Contains(got, "wlan0") {
		t.Errorf("router mode wanList %v", got)
	}
}

func TestRouterOnly(t *testing.T) {
	cfgMu.Lock()
	saved := cfg
	cfg = defaultConfig()
	toClientMode(&cfg, ClientCfg{SSID: "Home"})
	cfgMu.Unlock()
	defer func() { cfgMu.Lock(); cfg = saved; cfgMu.Unlock() }()
	called := false
	f := routerOnly(func(*http.Request) (any, error) { called = true; return nil, nil })
	_, err := f(nil)
	var he *httpError
	if called || !errors.As(err, &he) || he.code != http.StatusConflict {
		t.Fatalf("routerOnly in client mode: called=%v err=%v", called, err)
	}
	if _, err := clientOnly(func(*http.Request) (any, error) { return nil, nil })(nil); err != nil {
		t.Fatalf("clientOnly in client mode: %v", err)
	}
}

func TestMeshHelpers(t *testing.T) {
	tok := "eyJhIjoiMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAiLCJ0IjoiYzc5M2NjMDgifQ=="
	for _, in := range []string{tok, "  " + tok + "\n", "warp-cli connector new " + tok + " && warp-cli connect"} {
		if got := extractMeshToken(in); got != tok {
			t.Errorf("extract(%q) = %q", in, got)
		}
	}
	if extractMeshToken("warp-cli connect") != "" || extractMeshToken("short") != "" {
		t.Error("extracted a token from nothing")
	}
	m := parseColonLines("Status update: Connected\nNetwork: healthy\nOrganization: example-org\n")
	if m["Status update"] != "Connected" || m["Network"] != "healthy" || m["Organization"] != "example-org" {
		t.Errorf("parse %v", m)
	}
	if f := splitNMTerse(`*:My\:Net:78:WPA2:2462 MHz`); !slices.Equal(f, []string{"*", "My:Net", "78", "WPA2", "2462 MHz"}) {
		t.Errorf("split %q", f)
	}
}
