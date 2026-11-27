package main

import (
	"log"
	"strings"
)

// Optional modules, switched in /etc/router/modules.conf (one "name=on|off" per line, # comments).
// The installer writes this file. A missing file or line means off: modules that depend on outside
// services (WhatsApp forwarding needs a WAWAT server) are excluded unless installed explicitly.
const modulesPath = "/etc/router/modules.conf"

type moduleSet struct {
	WhatsApp bool `json:"whatsapp"`
}

var modules moduleSet

func loadModules() {
	modules = parseModules(readTrim(modulesPath))
	log.Printf("modules: whatsapp=%v", modules.WhatsApp)
}

func parseModules(txt string) moduleSet {
	var m moduleSet
	for _, l := range strings.Split(txt, "\n") {
		l, _, _ = strings.Cut(l, "#")
		k, v, ok := strings.Cut(l, "=")
		if !ok {
			continue
		}
		on := false
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "on", "1", "yes", "true":
			on = true
		}
		if strings.TrimSpace(strings.ToLower(k)) == "whatsapp" {
			m.WhatsApp = on
		}
	}
	return m
}
