package main

// Block OS/app update servers by DNS to save mobile data. The names are written to a dnsmasq
// servers-file as server=/domain/ (no address: answer NXDOMAIN locally for the domain and its
// subdomains; a servers-file silently ignores local= lines, only server= is read), so blocking can be
// switched on and off with a SIGHUP when the active uplink changes, without restarting dnsmasq.

import (
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"
)

const updateBlockPath = "/etc/router/dnsmasq-servers.conf"

type UpdateBlockCfg struct {
	Mode   string   `json:"mode"`   // off | mobile (only while LTE is the active uplink) | always
	Groups []string `json:"groups"` // keys of updateGroups
}

type updateGroup struct {
	Key, Label, Help string
	Block            []string // domain and all subdomains
	Allow            []string // more specific names that keep working (security-relevant, small)
}

// updateGroups is the curated list. Keep it to update/download hosts: blocking sign-in, activation or
// telemetry hosts breaks other things and saves little data.
var updateGroups = []updateGroup{
	{
		Key: "windows", Label: "Windows Update & Delivery Optimization",
		Help: "Windows Update, feature updates, driver updates, Delivery Optimization (peer-to-peer) and Microsoft Edge updates.",
		Block: []string{
			"windowsupdate.com", "update.microsoft.com", "windowsupdate.microsoft.com",
			"delivery.mp.microsoft.com", "do.dsp.mp.microsoft.com", "tsfe.trafficshaping.dsp.mp.microsoft.com",
			"emdl.ws.microsoft.com", "msedge.api.cdp.microsoft.com",
		},
		// Root certificate/CRL lists (tiny) and Defender definition updates stay allowed.
		Allow: []string{"ctldl.windowsupdate.com", "definitionupdates.microsoft.com"},
	},
	{
		Key: "office", Label: "Microsoft Office updates",
		Help:  "Microsoft 365 / Office Click-to-Run update downloads.",
		Block: []string{"officecdn.microsoft.com", "officecdn.microsoft.com.edgesuite.net"},
	},
	{
		Key: "store", Label: "Microsoft Store & Xbox game downloads",
		Help: "Large game and app package downloads from the Store and Xbox.",
		Block: []string{
			"assets1.xboxlive.com", "assets2.xboxlive.com", "dlassets.xboxlive.com", "dlassets2.xboxlive.com",
			"d1.xboxlive.com", "d2.xboxlive.com", "xvcf1.xboxlive.com", "xvcf2.xboxlive.com", "xbox-mbr.xboxlive.com",
		},
	},
	{
		Key: "apple", Label: "Apple software updates (macOS, iOS)",
		Help: "macOS and iOS/iPadOS system update downloads and update catalogs.",
		Block: []string{
			"swcdn.apple.com", "swdist.apple.com", "swscan.apple.com", "updates.cdn-apple.com",
			"updates-http.cdn-apple.com", "mesu.apple.com", "appldnld.apple.com", "gdmf.apple.com",
		},
	},
}

func defaultUpdateBlock() UpdateBlockCfg {
	return UpdateBlockCfg{Mode: "off", Groups: []string{"windows", "office"}}
}

func validateUpdateBlock(u *UpdateBlockCfg) error {
	if u.Mode == "" {
		u.Mode = "off"
	}
	switch u.Mode {
	case "off", "mobile", "always":
	default:
		return badRequest("update blocking: mode must be off, mobile or always")
	}
	known := map[string]bool{}
	for _, g := range updateGroups {
		known[g.Key] = true
	}
	seen := map[string]bool{}
	clean := []string{}
	for _, g := range u.Groups {
		if !known[g] {
			return badRequest("update blocking: unknown category %q", g)
		}
		if !seen[g] {
			seen[g] = true
			clean = append(clean, g)
		}
	}
	u.Groups = clean
	return nil
}

// updateBlockOn: "mobile" blocks unless the USB host (a PC sharing its connection) is the active
// uplink; with no healthy uplink it blocks too, so updates don't start the moment LTE returns.
func updateBlockOn(u UpdateBlockCfg, activeWAN string) bool {
	switch u.Mode {
	case "always":
		return true
	case "mobile":
		return activeWAN != "usb0"
	}
	return false
}

func renderUpdateBlock(c Config, on bool) string {
	var b strings.Builder
	if !on {
		return ""
	}
	upstreams := dnsUpstreams(c) // the same servers as dnsmasq.conf (bound to the VPN tunnel if the LAN uses one)
	sel := map[string]bool{}
	for _, g := range c.DNS.UpdateBlock.Groups {
		sel[g] = true
	}
	for _, g := range updateGroups {
		if !sel[g.Key] {
			continue
		}
		for _, d := range g.Block {
			fmt.Fprintf(&b, "server=/%s/\n", d)
		}
		for _, d := range g.Allow { // a more specific domain wins over the blocked parent
			for _, u := range upstreams {
				fmt.Fprintf(&b, "server=/%s/%s\n", d, u)
			}
		}
	}
	return b.String()
}

var (
	ubMu     sync.Mutex
	ubState  = map[string]any{"active": false}
	ubLastOn = -1 // -1 unknown, 0 off, 1 on (what the servers-file currently says)
)

func activeWAN() string { return readWANState()["active"] }

// writeUpdateBlock writes the servers-file for the current uplink. It reports whether dnsmasq
// needs a reload (the content changed).
func writeUpdateBlock(c Config) (bool, error) {
	ubMu.Lock()
	defer ubMu.Unlock()
	wan := activeWAN()
	on := updateBlockOn(c.DNS.UpdateBlock, wan)
	data := renderUpdateBlock(c, on)
	n := strings.Count(data, "/\n") // blocked lines end in "/"; allowed ones end in an address
	ubState = map[string]any{"active": on, "uplink": wan, "mode": c.DNS.UpdateBlock.Mode, "domains": n, "since": time.Now().Format(time.RFC3339)}
	old, err := os.ReadFile(updateBlockPath)
	if err == nil && string(old) == data {
		return false, nil
	}
	if err := writeAtomic(updateBlockPath, []byte(data), 0o644); err != nil {
		return false, err
	}
	state := 0
	if on {
		state = 1
	}
	if ubLastOn != state {
		log.Printf("update blocking %s (mode %s, uplink %s, %d domains)", map[bool]string{true: "ON", false: "off"}[on], c.DNS.UpdateBlock.Mode, firstNonEmpty(wan, "none"), n)
	}
	ubLastOn = state
	return true, nil
}

func updateBlockSnapshot() map[string]any {
	ubMu.Lock()
	defer ubMu.Unlock()
	out := map[string]any{}
	for k, v := range ubState {
		out[k] = v
	}
	return out
}

// reloadDnsmasqServers makes dnsmasq re-read the servers-file (and drop its cache) without a restart.
func reloadDnsmasqServers() {
	if _, err := run(10*time.Second, "systemctl", "kill", "--signal=HUP", "router-dnsmasq"); err != nil {
		log.Printf("update blocking: reload dnsmasq: %v", err)
	}
}

// updateBlockLoop follows uplink changes (LTE <-> USB host) for the "mobile" mode.
func updateBlockLoop() {
	for {
		if changed, err := writeUpdateBlock(snapshot()); err != nil {
			log.Printf("update blocking: %v", err)
		} else if changed {
			reloadDnsmasqServers()
		}
		time.Sleep(5 * time.Second)
	}
}

type updateGroupView struct {
	Key     string   `json:"key"`
	Label   string   `json:"label"`
	Help    string   `json:"help"`
	Domains []string `json:"domains"`
	Allowed []string `json:"allowed"`
}

func updateGroupViews() []updateGroupView {
	out := []updateGroupView{}
	for _, g := range updateGroups {
		out = append(out, updateGroupView{g.Key, g.Label, g.Help, g.Block, append([]string{}, g.Allow...)})
	}
	return out
}
