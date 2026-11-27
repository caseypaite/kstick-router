package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// List endpoints must return [] rather than null, or the UI's .length/.map calls throw.
func TestJSONAPINilSliceIsEmptyArray(t *testing.T) {
	h := jsonAPI(func(*http.Request) (any, error) { var cs []*client; return cs, nil })
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest("GET", "/api/clients", nil))
	if got := strings.TrimSpace(rec.Body.String()); got != "[]" {
		t.Fatalf("nil slice encoded as %q, want []", got)
	}
}

func TestUpdateHostsFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "hosts")
	os.WriteFile(p, []byte("127.0.0.1\tlocalhost openstick-debian\n::1\t\tlocalhost ip6-localhost openstick-debian\nff02::1\t\tip6-allnodes\n\n127.0.1.1 openstick-debian\n"), 0o644)
	if err := updateHostsFile(p, "openstick-debian", "router1"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	want := "127.0.0.1\tlocalhost\trouter1\n::1\tlocalhost\tip6-localhost\trouter1\nff02::1\t\tip6-allnodes\n\n127.0.1.1\trouter1\n"
	if string(b) != want {
		t.Errorf("got:\n%s\nwant:\n%s", b, want)
	}
	// a stale name (renamed outside the UI) still gets 127.0.1.1 fixed
	updateHostsFile(p, "other", "new-name")
	if b, _ := os.ReadFile(p); !strings.Contains(string(b), "127.0.1.1\tnew-name") {
		t.Errorf("127.0.1.1 not updated: %s", b)
	}
}

func TestParseUSSDStatus(t *testing.T) {
	in := "modem.dbus-path                                 : /org/freedesktop/ModemManager1/Modem/0\nmodem.3gpp.ussd.status                          : active\nmodem.3gpp.ussd.network-request                 : --\n"
	if got := parseUSSDStatus(in); got != "active" {
		t.Errorf("got %q", got)
	}
	if got := parseUSSDStatus(""); got != "" {
		t.Errorf("empty: got %q", got)
	}
}

// Every rule renders cleanly with the LAN on wlan0 and on the wired-LAN bridge (no fmt errors, no stray wlan0).
func TestRenderLANInterfaces(t *testing.T) {
	c := defaultConfig()
	c.Firewall.MeshLAN, c.Firewall.MeshAdmin, c.IPv6.Enabled = true, true, true
	defer func() { lanIf, ifaceNames["lan"] = "wlan0", "wlan0" }()
	for _, ifc := range []string{"wlan0", "br-lan"} {
		lanIf, ifaceNames["lan"] = ifc, ifc
		out := renderFirewall(c) + renderDnsmasq(c)
		if strings.Contains(out, "%!") {
			t.Fatalf("%s: format error in rendered rules", ifc)
		}
		if ifc == "br-lan" && strings.Contains(out, "wlan0") {
			t.Fatalf("br-lan: rules still name wlan0")
		}
		if !strings.Contains(out, `oifname "`+ifc+`" ip daddr 192.168.4.0/24 accept`) || !strings.Contains(out, "interface="+ifc) {
			t.Fatalf("%s: LAN rules missing", ifc)
		}
	}
}

func TestWifiChannelArg(t *testing.T) {
	for ch, want := range map[int]string{0: "", -1: "", 1: "1", 11: "11"} {
		if got := wifiChannelArg(ch); got != want {
			t.Errorf("wifiChannelArg(%d) = %q, want %q", ch, got, want)
		}
	}
}

func TestParseAptSim(t *testing.T) {
	sim := "Reading package lists...\nInst libc6 [2.36-9] (2.36-9+deb12u9 Debian:12.9/stable [arm64])\nConf libc6 (2.36-9+deb12u9)\nInst curl [7.88] (7.88.1-10+deb12u8 Debian-Security [arm64])\n"
	got := parseAptSim(sim)
	if len(got) != 2 || !strings.HasPrefix(got[0], "libc6 ") || !strings.HasPrefix(got[1], "curl ") {
		t.Errorf("parseAptSim: %q", got)
	}
	if got := parseAptSim(""); got == nil || len(got) != 0 {
		t.Errorf("empty simulation must give an empty list, got %#v", got)
	}
}
