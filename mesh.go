package main

// Cloudflare Mesh: the Cloudflare One client (warp-svc / warp-cli) registered as a Mesh node. The page shows
// its state and Mesh addresses, connects/disconnects/restarts it, enrolls it with a Mesh node token, and
// switches "management over Mesh" (the firewall's mesh_admin) on or off. Works in both modes.

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

const warpStateDir = "/var/lib/cloudflare-warp"

func warpCLI(timeout time.Duration, args ...string) (string, error) {
	return run(timeout, "warp-cli", append([]string{"--accept-tos"}, args...)...)
}

// parseColonLines turns "Key: value" lines into a map (warp-cli status / registration show).
func parseColonLines(out string) map[string]string {
	m := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(l, ":"); ok {
			m[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return m
}

var stMesh = ttlCache[map[string]any]{ttl: 5 * time.Second}

func meshInfo() map[string]any {
	m := map[string]any{"admin": snapshot().Firewall.MeshAdmin, "ipv4": ifaceIPv4(meshIf), "interface": meshIf}
	if _, err := exec.LookPath("warp-cli"); err != nil {
		m["installed"] = false
		return m
	}
	m["installed"] = true
	active, _ := run(5*time.Second, "systemctl", "is-active", "warp-svc")
	enabled, _ := run(5*time.Second, "systemctl", "is-enabled", "warp-svc")
	m["service"], m["enabled"] = strings.TrimSpace(active), strings.TrimSpace(enabled) == "enabled"
	if strings.TrimSpace(active) != "active" {
		return m
	}
	if out, err := warpCLI(10*time.Second, "status"); err == nil {
		st := parseColonLines(out)
		m["status"], m["network"], m["reason"] = st["Status update"], st["Network"], st["Reason"]
	}
	if out, err := warpCLI(10*time.Second, "registration", "show"); err == nil {
		reg := parseColonLines(out)
		m["registered"] = reg["Device ID"] != ""
		m["device_id"], m["organization"], m["account_type"] = reg["Device ID"], reg["Organization"], reg["Account type"]
	} else {
		m["registered"] = false
	}
	for _, l := range strings.Split(ipq("-6", "-o", "addr", "show", "dev", meshIf, "scope", "global"), "\n") {
		f := strings.Fields(l)
		for i := range f {
			if f[i] == "inet6" && i+1 < len(f) {
				if a := strings.Split(f[i+1], "/")[0]; strings.HasPrefix(a, "2606:4700:cf1:1000:") {
					m["ipv6"] = a
				}
			}
		}
	}
	return m
}

func apiGetMesh(r *http.Request) (any, error) { return stMesh.get(meshInfo), nil }

// apiPutMesh: {"admin": bool} — ping, SSH and the web UI over Mesh (a risky change: confirm or revert).
func apiPutMesh(r *http.Request) (any, error) {
	var b struct {
		Admin bool `json:"admin"`
	}
	if err := readJSON(r, &b); err != nil {
		return nil, err
	}
	defer stMesh.invalidate()
	return updateCfgConfirm(r, "Mesh management", func(c *Config) { c.Firewall.MeshAdmin = b.Admin }, applyFirewall)
}

func apiMeshAction(r *http.Request) (any, error) {
	if _, err := exec.LookPath("warp-cli"); err != nil {
		return nil, badRequest("the Cloudflare One client (cloudflare-warp) is not installed")
	}
	defer stMesh.invalidate()
	action := r.PathValue("action")
	var err error
	msg := ""
	switch action {
	case "connect":
		_, err = warpCLI(30*time.Second, "connect")
		msg = "Connecting to Cloudflare."
	case "disconnect":
		_, err = warpCLI(30*time.Second, "disconnect")
		msg = "Disconnected: the Mesh address is gone until you connect again."
	case "restart":
		later(func() { _, _ = run(60*time.Second, "systemctl", "restart", "warp-svc") })
		msg = "Restarting the Cloudflare One client: Mesh is back in about 20 seconds."
	case "enable":
		_, err = run(60*time.Second, "systemctl", "enable", "--now", "warp-svc")
		msg = "The Cloudflare One client now starts at boot and is running."
	case "disable":
		later(func() { _, _ = run(60*time.Second, "systemctl", "disable", "--now", "warp-svc") })
		msg = "The Cloudflare One client is stopped and no longer starts at boot."
	default:
		return nil, badRequest("unknown action %q", action)
	}
	if err != nil {
		return nil, err
	}
	audit(r, "mesh: %s", action)
	return map[string]string{"message": msg}, nil
}

// reMeshToken: a Mesh node token (base64 JSON); extractMeshToken also accepts the whole command from the
// dashboard ("warp-cli connector new <token> && warp-cli connect").
var reMeshToken = regexp.MustCompile(`^[A-Za-z0-9+/=_-]+$`)

func extractMeshToken(s string) string {
	for _, f := range strings.Fields(s) {
		if len(f) >= 40 && len(f) <= 4096 && reMeshToken.MatchString(f) {
			return f
		}
	}
	return ""
}

// apiMeshRegister: {"token": "...", "replace": bool}. Replacing an existing registration moves the old
// state aside (/var/lib/cloudflare-warp.replaced-<time>) instead of deleting it at Cloudflare: the same
// node token may be in use on another device.
func apiMeshRegister(r *http.Request) (any, error) {
	var b struct {
		Token   string `json:"token"`
		Replace bool   `json:"replace"`
	}
	if err := readJSON(r, &b); err != nil {
		return nil, err
	}
	token := extractMeshToken(b.Token)
	if token == "" {
		return nil, badRequest("paste the Mesh node token (or the whole \"warp-cli connector new …\" command)")
	}
	if _, err := exec.LookPath("warp-cli"); err != nil {
		return nil, badRequest("the Cloudflare One client (cloudflare-warp) is not installed")
	}
	defer stMesh.invalidate()
	if _, err := run(60*time.Second, "systemctl", "enable", "--now", "warp-svc"); err != nil {
		return nil, err
	}
	if err := waitWarp(); err != nil {
		return nil, err
	}
	if out, err := warpCLI(10*time.Second, "registration", "show"); err == nil && parseColonLines(out)["Device ID"] != "" {
		if !b.Replace {
			return nil, &httpError{http.StatusConflict, "this device is already registered; tick \"replace the current registration\" to enroll it with the new token"}
		}
		_, _ = run(60*time.Second, "systemctl", "stop", "warp-svc")
		old := warpStateDir + ".replaced-" + time.Now().Format("20060102-150405")
		if err := os.Rename(warpStateDir, old); err != nil {
			_, _ = run(60*time.Second, "systemctl", "start", "warp-svc")
			return nil, fmt.Errorf("move the old registration aside: %v", err)
		}
		if err := os.MkdirAll(warpStateDir, 0o755); err != nil {
			return nil, err
		}
		if _, err := run(60*time.Second, "systemctl", "start", "warp-svc"); err != nil {
			return nil, err
		}
		if err := waitWarp(); err != nil {
			return nil, err
		}
		audit(r, "mesh: old registration moved to %s", old)
	}
	if _, err := warpCLI(60*time.Second, "connector", "new", token); err != nil {
		return nil, fmt.Errorf("registration failed: %v", strings.ReplaceAll(err.Error(), token, "<token>"))
	}
	if _, err := warpCLI(30*time.Second, "connect"); err != nil {
		return nil, err
	}
	audit(r, "mesh: registered as a Mesh node (new token)")
	return map[string]string{"message": "Registered. The Mesh address appears once the client is connected (a few seconds)."}, nil
}

// waitWarp: warp-svc answers warp-cli (it takes a few seconds after a start).
func waitWarp() error {
	for i := 0; i < 20; i++ {
		if _, err := warpCLI(5*time.Second, "status"); err == nil {
			return nil
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("the Cloudflare One client (warp-svc) does not answer")
}
