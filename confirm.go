package main

// Confirm-or-revert for risky changes. A change that can cut the admin off (firewall, LAN address/DHCP,
// VPN routing, IPv6, blocking a client) is applied at once, but undone after Admin.ConfirmSecs unless it
// is confirmed in the web UI. Over Cloudflare Mesh a bad rule would otherwise mean a trip to the USB port.
//
// Further risky changes while one is pending extend it and keep the original "before" state, so one
// confirm or revert covers them all. The pending change is kept in a file: if router-webui restarts (or
// the stick reboots, e.g. power-cycled after a lockout) before it was confirmed, it is reverted at start.
// Only the risky sections (operating mode, firewall, LAN, VPN, IPv6, LTE bands) go back; other settings saved meanwhile stay.

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"
)

const (
	pendingPath   = "/var/lib/router/pending-change.json"
	lanChangeSecs = 180 // a new LAN address needs a reconnect and a new login before it can be confirmed
)

type pendingChange struct {
	ID       string    `json:"id"`
	What     []string  `json:"what"`
	Started  time.Time `json:"started"`
	Deadline time.Time `json:"deadline"`
	Prev     Config    `json:"prev"`
}

var (
	pendMu    sync.Mutex
	pending   *pendingChange
	pendTimer *time.Timer
)

func cloneConfig(c Config) Config {
	b, _ := json.Marshal(c)
	var n Config
	_ = json.Unmarshal(b, &n)
	return n
}

// pendingView is what the UI shows (no config inside).
func pendingView() map[string]any {
	pendMu.Lock()
	defer pendMu.Unlock()
	if pending == nil {
		return nil
	}
	return map[string]any{"id": pending.ID, "what": pending.What, "deadline": pending.Deadline.Format(time.RFC3339),
		"seconds_left": max(0, int(time.Until(pending.Deadline).Seconds()))}
}

// beginConfirm registers a risky change that was just applied; prev is the config before it.
// It returns the pending view, or nil when confirmation is off.
func beginConfirm(what string, prev Config, secs int, lanAddrChanged bool) map[string]any {
	if secs <= 0 {
		return nil
	}
	if lanAddrChanged {
		secs = max(secs, lanChangeSecs)
	}
	pendMu.Lock()
	if pending == nil {
		pending = &pendingChange{ID: randHex(8), Started: time.Now(), Prev: cloneConfig(prev)}
	}
	pending.What = append(pending.What, what)
	pending.Deadline = time.Now().Add(time.Duration(secs) * time.Second)
	if pendTimer != nil {
		pendTimer.Stop()
	}
	id := pending.ID
	pendTimer = time.AfterFunc(time.Until(pending.Deadline), func() { revertPending(id, "not confirmed in time") })
	b, _ := json.Marshal(pending)
	pendMu.Unlock()
	if err := writeAtomic(pendingPath, b, 0o600); err != nil {
		log.Printf("confirm: %v", err)
	}
	log.Printf("confirm: %s applied; reverted in %d s unless confirmed", what, secs)
	return pendingView()
}

func takePending(id string) (*pendingChange, error) {
	pendMu.Lock()
	defer pendMu.Unlock()
	if pending == nil || (id != "" && pending.ID != id) {
		return nil, &httpError{http.StatusNotFound, "no such pending change (already confirmed or reverted)"}
	}
	p := pending
	pending = nil
	if pendTimer != nil {
		pendTimer.Stop()
		pendTimer = nil
	}
	return p, nil
}

// revertPending puts the risky sections back as they were before the pending change.
func revertPending(id, why string) error {
	p, err := takePending(id)
	if err != nil {
		return err
	}
	err = revertTo(p.Prev, p.What, why)
	_ = os.Remove(pendingPath)
	return err
}

func revertTo(prev Config, what []string, why string) error {
	cfgMu.Lock()
	next := cloneConfig(cfg)
	next.Firewall, next.VPN, next.IPv6, next.Radio = prev.Firewall, prev.VPN, prev.IPv6, prev.Radio
	modeChanged := next.Mode != prev.Mode || next.Client != prev.Client
	next.Mode, next.Client = prev.Mode, prev.Client
	if modeChanged { // the Wi-Fi off schedule may only be on in router mode: take it back too
		next.Schedule.WifiOff = prev.Schedule.WifiOff
	}
	static := next.LAN.Static // DHCP reservations are not risky: keep the ones saved meanwhile
	next.LAN = prev.LAN
	next.LAN.Static = static
	if validateConfig(&next) != nil {
		next.LAN.Static = prev.LAN.Static // they no longer fit the old LAN
		if err := validateConfig(&next); err != nil {
			cfgMu.Unlock()
			log.Printf("confirm: cannot revert %v: %v", what, err)
			return err
		}
	}
	addrChanged := next.LAN.Address != cfg.LAN.Address
	var errs []error
	if addrChanged {
		errs = append(errs, nmModify(lanCon(), "ipv4.method", "manual", "ipv4.addresses", next.LAN.Address))
	}
	if modeChanged {
		errs = append(errs, applyMode(next)) // also the firewall and DHCP, in the safe order
	} else {
		errs = append(errs, applyDnsmasq(next), applyFirewall(next))
	}
	cfg = next
	errs = append(errs, saveConfig())
	cfgMu.Unlock()
	errs = append(errs, applyBands(next.Radio))
	kickVPN()
	v6ForceProbe.Store(true)
	kickIPv6()
	kickUsage()
	if addrChanged {
		nmUpLater(lanCon())
	}
	err := errors.Join(errs...)
	log.Printf("confirm: REVERTED %v (%s); errors: %v", what, why, err)
	return err
}

func confirmPending(id string) error {
	p, err := takePending(id)
	if err != nil {
		return err
	}
	_ = os.Remove(pendingPath)
	log.Printf("confirm: %v confirmed", p.What)
	return nil
}

// revertPendingAtStart: a change still pending when router-webui stopped was never confirmed.
func revertPendingAtStart() {
	b, err := os.ReadFile(pendingPath)
	if err != nil {
		return
	}
	var p pendingChange
	if json.Unmarshal(b, &p) != nil {
		_ = os.Remove(pendingPath)
		return
	}
	_ = revertTo(p.Prev, p.What, "router-webui restarted before the change was confirmed")
	_ = os.Remove(pendingPath)
}

// updateCfgConfirm is updateCfg for risky changes: it also starts the confirm countdown.
func updateCfgConfirm(r *http.Request, what string, mutate func(*Config), apply ...func(Config) error) (any, error) {
	prev := snapshot()
	res, err := updateCfg(r, what, mutate, apply...)
	if err != nil {
		return nil, err
	}
	return withPending(res, what, prev, false), nil
}

func withPending(res any, what string, prev Config, lanAddrChanged bool) any {
	p := beginConfirm(what, prev, snapshot().Admin.ConfirmSecs, lanAddrChanged)
	if p == nil {
		return res
	}
	out := map[string]any{"pending": p}
	if m, ok := res.(map[string]string); ok {
		for k, v := range m {
			out[k] = v
		}
	}
	out["message"] = fmt.Sprintf("Applied. Confirm within %d s, or it is undone.", p["seconds_left"])
	return out
}

func apiGetPending(r *http.Request) (any, error) {
	if v := pendingView(); v != nil {
		return v, nil
	}
	return map[string]any{}, nil
}

func apiPendingAction(r *http.Request) (any, error) {
	id := r.PathValue("id")
	switch r.PathValue("action") {
	case "confirm":
		if err := confirmPending(id); err != nil {
			return nil, err
		}
		audit(r, "pending change confirmed")
		return map[string]string{"message": "Changes kept."}, nil
	case "revert":
		audit(r, "pending change reverted by the admin")
		if err := revertPending(id, "reverted by the admin"); err != nil {
			return nil, err
		}
		return map[string]string{"message": "Changes undone."}, nil
	}
	return nil, badRequest("unknown action")
}

func apiPutAdmin(r *http.Request) (any, error) {
	var a AdminCfg
	if err := readJSON(r, &a); err != nil {
		return nil, err
	}
	return updateCfg(r, "admin settings", func(c *Config) { c.Admin = a })
}
