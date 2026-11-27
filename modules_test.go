package main

import "testing"

func TestParseModules(t *testing.T) {
	for in, want := range map[string]bool{
		"":                            false, // missing file: optional modules are excluded
		"whatsapp=on":                 true,
		"# x\n WhatsApp = yes # note": true,
		"whatsapp=off":                false,
		"whatsapp":                    false,
	} {
		if got := parseModules(in).WhatsApp; got != want {
			t.Errorf("parseModules(%q).WhatsApp = %v, want %v", in, got, want)
		}
	}
}
