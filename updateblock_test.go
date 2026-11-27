package main

import (
	"strings"
	"testing"
)

func TestUpdateBlockOn(t *testing.T) {
	for _, tc := range []struct {
		mode, wan string
		want      bool
	}{
		{"off", "wwan0", false}, {"always", "usb0", true},
		{"mobile", "wwan0", true}, {"mobile", "usb0", false}, {"mobile", "none", true}, {"mobile", "", true},
	} {
		if got := updateBlockOn(UpdateBlockCfg{Mode: tc.mode}, tc.wan); got != tc.want {
			t.Errorf("mode %s uplink %q: got %v, want %v", tc.mode, tc.wan, got, tc.want)
		}
	}
}

func TestRenderUpdateBlock(t *testing.T) {
	c := defaultConfig() // windows + office
	if s := renderUpdateBlock(c, false); s != "" {
		t.Fatalf("off must render an empty servers-file, got %q", s)
	}
	s := renderUpdateBlock(c, true)
	if strings.Contains(s, "local=") {
		t.Errorf("dnsmasq ignores local= in a servers-file:\n%s", s)
	}
	for _, want := range []string{"server=/windowsupdate.com/\n", "server=/delivery.mp.microsoft.com/\n", "server=/officecdn.microsoft.com/\n",
		"server=/ctldl.windowsupdate.com/127.0.0.1\n", "server=/definitionupdates.microsoft.com/127.0.0.1\n"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	if strings.Contains(s, "xboxlive") || strings.Contains(s, "apple") {
		t.Errorf("unselected categories rendered:\n%s", s)
	}
	c.DNS.Upstreams = []string{"1.1.1.1", "9.9.9.9"}
	s = renderUpdateBlock(c, true)
	if !strings.Contains(s, "server=/ctldl.windowsupdate.com/1.1.1.1\n") || !strings.Contains(s, "server=/ctldl.windowsupdate.com/9.9.9.9\n") {
		t.Errorf("allowed names must use the configured upstreams:\n%s", s)
	}
}

func TestValidateUpdateBlock(t *testing.T) {
	u := UpdateBlockCfg{Groups: []string{"windows", "windows", "apple"}}
	if err := validateUpdateBlock(&u); err != nil || u.Mode != "off" || len(u.Groups) != 2 {
		t.Fatalf("got %+v, %v", u, err)
	}
	if validateUpdateBlock(&UpdateBlockCfg{Mode: "sometimes"}) == nil {
		t.Error("bad mode accepted")
	}
	if validateUpdateBlock(&UpdateBlockCfg{Mode: "off", Groups: []string{"linux"}}) == nil {
		t.Error("unknown category accepted")
	}
}
