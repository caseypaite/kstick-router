package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

func registerAPI(m *http.ServeMux) {
	m.HandleFunc("POST /api/login", handleLogin)
	m.HandleFunc("POST /api/logout", handleLogout)
	m.HandleFunc("GET /api/session", handleSession)
	a := func(p string, f apiFunc) { m.Handle(p, auth(jsonAPI(f))) }

	a("GET /api/status", apiStatus)
	a("GET /api/traffic", apiTraffic)
	a("GET /api/interfaces", apiInterfaces)
	a("GET /api/clients", apiClients)
	a("POST /api/clients/{mac}/{action}", routerOnly(apiClientAction))
	a("GET /api/mode", apiGetMode)
	a("PUT /api/mode", apiPutMode)
	a("GET /api/wifi/scan", clientOnly(apiWifiScan))
	a("GET /api/mesh", apiGetMesh)
	a("PUT /api/mesh", apiPutMesh)
	a("POST /api/mesh/register", apiMeshRegister)
	a("POST /api/mesh/{action}", apiMeshAction)

	a("GET /api/wan", apiGetWAN)
	a("PUT /api/wan/failover", apiPutFailover)
	a("PUT /api/wan/lte", apiPutLTE)
	a("POST /api/wan/lte/{action}", apiLTEAction)
	a("GET /api/speedtest", apiGetSpeedtest)
	a("POST /api/speedtest", apiRunSpeedtest)

	a("GET /api/lan", apiGetLAN)
	a("PUT /api/lan", routerOnly(apiPutLAN))
	a("PUT /api/wifi", routerOnly(apiPutWifi))
	a("PUT /api/dhcp/static", routerOnly(apiPutStatic))

	a("GET /api/dns", func(r *http.Request) (any, error) {
		return struct {
			DNSCfg
			UBStatus map[string]any    `json:"update_block_status"`
			UBGroups []updateGroupView `json:"update_block_groups"`
		}{snapshot().DNS, updateBlockSnapshot(), updateGroupViews()}, nil
	})
	a("PUT /api/dns", routerOnly(apiPutDNS))

	a("GET /api/firewall", func(r *http.Request) (any, error) { return snapshot().Firewall, nil })
	a("PUT /api/firewall", routerOnly(apiPutFirewall))
	a("GET /api/firewall/ruleset", apiRuleset)

	a("GET /api/ipv6", apiGetIPv6)
	a("PUT /api/ipv6", routerOnly(apiPutIPv6))
	a("GET /api/vpn", apiGetVPN)
	a("POST /api/vpn/import", apiImportVPN)
	a("PUT /api/vpn/routing", routerOnly(apiPutVPNRouting))
	a("POST /api/vpn/{name}/{action}", apiVPNAction)

	a("GET /api/sms", apiListSMS)
	a("POST /api/sms", apiSendSMS)
	if modules.WhatsApp { // optional module (modules.conf); without it there is no WhatsApp API at all
		a("GET /api/wawat", apiGetWawat)
		a("PUT /api/wawat", apiPutWawat)
		a("POST /api/wawat/verify", apiVerifyWawat)
		a("POST /api/wawat/test", apiTestWawat)
	}
	a("GET /api/thermal", func(*http.Request) (any, error) {
		return map[string]any{"config": snapshot().Thermal, "status": thermalSnapshot()}, nil
	})
	a("PUT /api/thermal", apiPutThermal)
	a("GET /api/modules", func(*http.Request) (any, error) { return modules, nil })
	a("GET /api/smsgw", apiGetSMSGW)
	a("PUT /api/smsgw", apiPutSMSGW)
	a("POST /api/smsgw/keys", apiCreateSMSKey)
	a("GET /api/smsgw/keyname", apiSuggestSMSKeyName)
	a("DELETE /api/smsgw/keys/{name}", apiDeleteSMSKey)
	a("PUT /api/smsgw/keys/{name}", apiPutSMSKey)
	a("POST /api/sms/in/{id}/forward", apiReforwardSMS)
	a("POST /api/sms/in/{id}/forward/{target}", apiReforwardSMS)
	a("GET /api/forward", apiGetForward)
	a("PUT /api/forward", apiPutForward)
	a("POST /api/forward/test/{target}", apiTestForward)
	a("POST /api/ussd", apiUSSD)
	a("GET /api/celltower", apiGetCellTower)
	a("PUT /api/celltower", apiPutCellTower)
	a("POST /api/celltower/test", apiTestCellTower)

	a("POST /api/diag/{tool}", apiDiag)
	a("GET /api/diag/{view}", apiDiagView)
	a("GET /api/logs", apiLogs)

	a("GET /api/pending", apiGetPending)
	a("POST /api/pending/{id}/{action}", apiPendingAction)
	a("PUT /api/admin", apiPutAdmin)
	a("GET /api/data", apiGetData)
	a("PUT /api/data", apiPutData)
	a("GET /api/smsctl", apiGetSMSCtl)
	a("PUT /api/smsctl", apiPutSMSCtl)

	a("GET /api/history", apiHistory)
	a("GET /api/signal", apiGetSignal)
	a("PUT /api/radio", apiPutRadio)
	a("PUT /api/battery", apiPutBattery)
	a("GET /api/schedule", apiGetSchedule)
	a("PUT /api/schedule", apiPutSchedule)

	a("GET /api/bt", apiGetBT)
	a("PUT /api/bt", apiPutBT)
	a("PUT /api/bt/uplink", apiPutBTUplink)
	a("POST /api/bt/{action}", apiBTAction)

	a("GET /api/ui", apiGetUI)
	a("PUT /api/ui", apiPutUI)
	a("GET /api/system", apiGetSystem)
	a("PUT /api/system", apiPutSystem)
	a("GET /api/system/timezones", apiTimezones)
	a("POST /api/system/password", apiPassword)
	a("POST /api/system/reboot", apiReboot)
	a("GET /api/services", apiServices)
	a("POST /api/services/{name}/restart", apiServiceRestart)
	a("GET /api/updates", apiUpdates)
	a("POST /api/updates/{action}", apiUpdateAction)
	a("GET /api/backup", apiBackup)
	a("POST /api/restore", routerOnly(apiRestore))
}

// ---------------- firewall / DNS ----------------

func apiPutFirewall(r *http.Request) (any, error) {
	var f FirewallCfg
	if err := readJSON(r, &f); err != nil {
		return nil, err
	}
	res, err := updateCfgConfirm(r, "firewall", func(c *Config) { c.Firewall = f }, applyFirewall)
	kickVPN() // applies the Mesh return-route rule now
	return res, err
}

func apiRuleset(r *http.Request) (any, error) {
	out, err := run(10*time.Second, "nft", "list", "ruleset")
	if err != nil {
		return nil, err
	}
	return map[string]string{"ruleset": out, "generated": readTrim(fwPath)}, nil
}

func apiPutDNS(r *http.Request) (any, error) {
	var d DNSCfg
	if err := readJSON(r, &d); err != nil {
		return nil, err
	}
	return updateCfg(r, "DNS", func(c *Config) { c.DNS = d }, applyDnsmasq)
}

// ---------------- clients ----------------

func apiClientAction(r *http.Request) (any, error) {
	mac, act := normMAC(r.PathValue("mac")), r.PathValue("action")
	if !reMAC.MatchString(mac) {
		return nil, badRequest("invalid MAC")
	}
	switch act {
	case "block":
		return updateCfgConfirm(r, "block "+mac, func(c *Config) {
			for _, b := range c.Firewall.Blocked {
				if b.MAC == mac {
					return
				}
			}
			c.Firewall.Blocked = append(c.Firewall.Blocked, Blocked{MAC: mac, Comment: "blocked from clients page"})
		}, applyFirewall)
	case "unblock":
		return updateCfg(r, "unblock "+mac, func(c *Config) {
			out := c.Firewall.Blocked[:0]
			for _, b := range c.Firewall.Blocked {
				if b.MAC != mac {
					out = append(out, b)
				}
			}
			c.Firewall.Blocked = out
		}, applyFirewall)
	case "disconnect":
		if _, err := run(5*time.Second, "iw", "dev", "wlan0", "station", "del", mac); err != nil {
			return nil, err
		}
		audit(r, "disconnected Wi-Fi client %s", mac)
		return nil, nil
	case "reserve":
		var ip, name string
		for _, c := range listClients() {
			if c.MAC == mac {
				ip, name = c.IP, c.Name
			}
		}
		if ip == "" {
			return nil, badRequest("client has no known IP address")
		}
		if name != "" && (!reHost.MatchString(name) || strings.Contains(name, ".")) {
			name = ""
		}
		return updateCfg(r, "reserve "+mac, func(c *Config) {
			for _, s := range c.LAN.Static {
				if s.MAC == mac {
					return
				}
			}
			c.LAN.Static = append(c.LAN.Static, StaticLease{MAC: mac, IP: ip, Name: name})
		}, applyDnsmasq)
	}
	return nil, badRequest("unknown action")
}

// ---------------- diagnostics ----------------

var reTarget = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9.:-]{0,252})$`)

func apiDiag(r *http.Request) (any, error) {
	var b struct {
		Target string `json:"target"`
		Iface  string `json:"iface"`
		Count  int    `json:"count"`
		Server string `json:"server"`
	}
	if err := readJSON(r, &b); err != nil {
		return nil, err
	}
	b.Target = strings.TrimSpace(b.Target)
	if !reTarget.MatchString(b.Target) {
		return nil, badRequest("enter a host name or IP address")
	}
	if b.Iface != "" && b.Iface != "wwan0" && b.Iface != "usb0" && b.Iface != "wlan0" && !reWGName.MatchString(b.Iface) {
		return nil, badRequest("invalid interface")
	}
	switch r.PathValue("tool") {
	case "ping":
		if b.Count < 1 || b.Count > 10 {
			b.Count = 4
		}
		args := []string{"-n", "-c", strconv.Itoa(b.Count), "-W", "2"}
		if b.Iface != "" {
			args = append(args, "-I", b.Iface)
		}
		return map[string]string{"output": runCombined(40*time.Second, "ping", append(args, b.Target)...)}, nil
	case "traceroute":
		args := []string{"-n", "-w", "2", "-q", "1", "-m", "20"}
		if b.Iface != "" {
			args = append(args, "-i", b.Iface)
		}
		return map[string]string{"output": runCombined(90*time.Second, "traceroute", append(args, b.Target)...)}, nil
	case "dns":
		res := &net.Resolver{}
		if b.Server != "" {
			ip := net.ParseIP(strings.TrimSpace(b.Server))
			if ip == nil {
				return nil, badRequest("DNS server must be an IP address")
			}
			res = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{Timeout: 4 * time.Second}).DialContext(ctx, network, net.JoinHostPort(ip.String(), "53"))
			}}
		}
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()
		start := time.Now()
		addrs, err := res.LookupHost(ctx, b.Target)
		took := time.Since(start).Round(time.Millisecond)
		if err != nil {
			return map[string]string{"output": fmt.Sprintf("lookup failed after %s: %v", took, err)}, nil
		}
		cname, _ := res.LookupCNAME(ctx, b.Target)
		return map[string]string{"output": fmt.Sprintf("%s (%s)\nCNAME: %s\n%s", b.Target, took, cname, strings.Join(addrs, "\n"))}, nil
	}
	return nil, badRequest("unknown tool")
}

func apiDiagView(r *http.Request) (any, error) {
	switch r.PathValue("view") {
	case "routes":
		s := "### ip rule\n" + runCombined(5*time.Second, "ip", "rule") +
			"\n### main table\n" + runCombined(5*time.Second, "ip", "route") +
			"\n### table 100 (WAN failover)\n" + runCombined(5*time.Second, "ip", "route", "show", "table", "100") +
			"\n### table 200 (VPN for LAN)\n" + runCombined(5*time.Second, "ip", "route", "show", "table", "200") +
			"\n### IPv6\n" + runCombined(5*time.Second, "ip", "-6", "route")
		return map[string]string{"output": s}, nil
	case "arp":
		return map[string]string{"output": runCombined(5*time.Second, "ip", "neigh")}, nil
	case "conntrack":
		out := runCombined(15*time.Second, "conntrack", "-L", "-o", "extended")
		lines := strings.Split(out, "\n")
		if len(lines) > 400 {
			lines = append(lines[:400], fmt.Sprintf("… %d more", len(lines)-400))
		}
		return map[string]string{"output": strings.Join(lines, "\n")}, nil
	case "sockets":
		return map[string]string{"output": runCombined(5*time.Second, "ss", "-tulpn")}, nil
	}
	return nil, badRequest("unknown view")
}

var logSources = map[string][]string{
	"system":   {},
	"firewall": {"-k", "--grep", "router-(drop|rule)"},
	"network":  {"-u", "NetworkManager"},
	"modem":    {"-u", "ModemManager"},
	"wan":      {"-t", "wan-failover"},
	"dhcp":     {"-u", "router-dnsmasq"},
	"webui":    {"-u", "router-webui"},
	"auth":     {"-t", "sshd", "-t", "sshd-session", "-t", "sudo", "-u", "router-webui", "--grep", "login|Accepted|Failed|Invalid|sudo|session"},
	"kernel":   {"-k"},
}

func apiLogs(r *http.Request) (any, error) {
	src := r.URL.Query().Get("source")
	filter, ok := logSources[src]
	if !ok {
		return nil, badRequest("unknown log source")
	}
	n, _ := strconv.Atoi(r.URL.Query().Get("lines"))
	if n < 10 || n > 1000 {
		n = 200
	}
	args := append([]string{"--no-pager", "-o", "short-iso", "-n", strconv.Itoa(n)}, filter...)
	if src == "webui" {
		args = append(args, "-b")
	}
	return map[string]string{"output": runCombined(20*time.Second, "journalctl", args...)}, nil
}

// ---------------- system ----------------

func apiGetSystem(r *http.Request) (any, error) {
	info := systemInfo()
	tz, _ := run(5*time.Second, "timedatectl", "show", "-p", "Timezone", "--value")
	ntp, _ := run(5*time.Second, "timedatectl", "show", "-p", "NTPSynchronized", "--value")
	info["timezone"] = strings.TrimSpace(tz)
	info["ntp_synced"] = strings.TrimSpace(ntp) == "yes"
	info["admin_user"] = *flagUser
	info["confirm_secs"] = snapshot().Admin.ConfirmSecs
	info["watchdog"] = watchdogInfo()
	if batteryInfo() != nil {
		info["battery_cfg"] = snapshot().Battery
		info["battery_state"] = batterySnapshot()
	}
	return info, nil
}

func apiTimezones(r *http.Request) (any, error) {
	out, err := run(10*time.Second, "timedatectl", "list-timezones")
	if err != nil {
		return nil, err
	}
	return strings.Fields(out), nil
}

var reHostname = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$`)

// updateHostsFile renames old to name in the hosts file and makes sure 127.0.1.1 maps to name.
func updateHostsFile(path, old, name string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	found := false
	for i, l := range lines {
		body, comment, hasComment := strings.Cut(l, "#")
		f := strings.Fields(body)
		if len(f) < 2 {
			continue
		}
		changed := false
		for j := 1; j < len(f); j++ {
			if old != "" && f[j] == old {
				f[j], changed = name, true
			}
		}
		if f[0] == "127.0.1.1" {
			f, changed, found = []string{"127.0.1.1", name}, true, true
		}
		if changed {
			lines[i] = strings.Join(f, "\t")
			if hasComment {
				lines[i] += " #" + comment
			}
		}
	}
	if !found {
		lines = append(lines, "127.0.1.1\t"+name)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func apiPutSystem(r *http.Request) (any, error) {
	var b struct{ Hostname, Timezone string }
	if err := readJSON(r, &b); err != nil {
		return nil, err
	}
	if b.Hostname != "" {
		if !reHostname.MatchString(b.Hostname) {
			return nil, badRequest("invalid hostname")
		}
		old, _ := os.Hostname()
		if _, err := run(10*time.Second, "hostnamectl", "set-hostname", b.Hostname); err != nil {
			return nil, err
		}
		// sudo and other tools resolve the hostname through /etc/hosts; hostnamectl doesn't touch it
		if err := updateHostsFile("/etc/hosts", old, b.Hostname); err != nil {
			log.Printf("hostname: /etc/hosts: %v", err)
		}
	}
	if b.Timezone != "" {
		zones, _ := apiTimezones(r)
		valid := false
		for _, z := range zones.([]string) {
			if z == b.Timezone {
				valid = true
			}
		}
		if !valid {
			return nil, badRequest("unknown timezone")
		}
		if _, err := run(10*time.Second, "timedatectl", "set-timezone", b.Timezone); err != nil {
			return nil, err
		}
		if loc, err := time.LoadLocation(b.Timezone); err == nil {
			time.Local = loc // schedules, data usage days and history use local time
		}
	}
	audit(r, "system settings: hostname=%q timezone=%q", b.Hostname, b.Timezone)
	return map[string]string{"message": "Saved."}, nil
}

func apiPassword(r *http.Request) (any, error) {
	var b struct{ Current, New string }
	if err := readJSON(r, &b); err != nil {
		return nil, err
	}
	if !checkPassword(*flagUser, b.Current) {
		time.Sleep(700 * time.Millisecond)
		audit(r, "password change FAILED (wrong current password)")
		return nil, badRequest("current password is wrong")
	}
	if len(b.New) < 10 || len(b.New) > 128 || strings.ContainsAny(b.New, ":\n\r\x00") || strings.IndexFunc(b.New, unicode.IsControl) >= 0 {
		return nil, badRequest("new password must be 10-128 characters and may not contain ':' or control characters")
	}
	if b.New == b.Current {
		return nil, badRequest("new password must differ from the current one")
	}
	if _, err := runStdin(10*time.Second, *flagUser+":"+b.New+"\n", "chpasswd"); err != nil {
		return nil, err
	}
	dropOtherSessions(r)
	audit(r, "password for %s changed", *flagUser)
	return map[string]string{"message": "Password changed. Other sessions were logged out."}, nil
}

func apiReboot(r *http.Request) (any, error) {
	audit(r, "reboot requested")
	flushState()
	go func() {
		time.Sleep(time.Second)
		_, _ = run(10*time.Second, "systemctl", "reboot")
	}()
	return map[string]string{"message": "Rebooting. The router is back in about a minute."}, nil
}

var managedServices = []string{"NetworkManager", "ModemManager", "router-firewall", "wan-failover", "router-dnsmasq", "router-webui", "ssh", "systemd-timesyncd", "wpa_supplicant"}

func apiServices(r *http.Request) (any, error) {
	var res []map[string]string
	for _, s := range managedServices {
		out, _ := run(5*time.Second, "systemctl", "show", s, "-p", "ActiveState,SubState,UnitFileState,ActiveEnterTimestamp,Description")
		m := map[string]string{"name": s}
		for _, l := range strings.Split(out, "\n") {
			if k, v, ok := strings.Cut(l, "="); ok {
				m[k] = v
			}
		}
		res = append(res, m)
	}
	return res, nil
}

func apiServiceRestart(r *http.Request) (any, error) {
	name := r.PathValue("name")
	ok := false
	for _, s := range managedServices {
		if s == name {
			ok = true
		}
	}
	if !ok {
		return nil, badRequest("unknown service")
	}
	audit(r, "restart service %s", name)
	if name == "router-webui" || name == "NetworkManager" || name == "ssh" {
		_, err := run(10*time.Second, "systemd-run", "--on-active=2", "--collect", "systemctl", "restart", name)
		return map[string]string{"message": name + " restarts in 2 seconds."}, err
	}
	if _, err := run(60*time.Second, "systemctl", "restart", name); err != nil {
		return nil, err
	}
	return map[string]string{"message": name + " restarted."}, nil
}

// ---------------- updates ----------------

const upgradeLog = "/var/log/router-upgrade.log"

// The upgradable list comes from "apt-get -s dist-upgrade", which takes tens of seconds and tens of MB on
// this stick. It runs at most once per change of the package lists or the dpkg database, never while an
// update job runs, one at a time, and in its own transient unit: a child of router-webui would count
// against router-webui's MemoryMax, and the OOM killer could take the web UI down with it.
var (
	updSimMu sync.Mutex // held while a simulation runs
	updMu    sync.Mutex // guards the cache below
	updKey   string     // mtimes of the apt lists and dpkg status the cache was computed from
	updPkgs  []string
	updKnown bool
)

func aptStateKey() string {
	k := ""
	for _, p := range []string{"/var/lib/apt/lists", "/var/lib/dpkg/status"} {
		if st, err := os.Stat(p); err == nil {
			k += st.ModTime().UTC().Format(time.RFC3339Nano) + "|"
		}
	}
	return k
}

func parseAptSim(sim string) []string {
	pkgs := []string{}
	for _, l := range strings.Split(sim, "\n") {
		if strings.HasPrefix(l, "Inst ") {
			pkgs = append(pkgs, strings.TrimPrefix(l, "Inst "))
		}
	}
	return pkgs
}

// upgradable returns the cached list, refreshing it first when apt's state changed and no update job
// (or other simulation) is running. known is false until a simulation has completed.
func upgradable(jobRunning bool) (pkgs []string, known bool) {
	key := aptStateKey()
	updMu.Lock()
	fresh := updKnown && updKey == key
	pkgs, known = updPkgs, updKnown
	updMu.Unlock()
	if fresh || jobRunning || !updSimMu.TryLock() {
		return pkgs, known
	}
	defer updSimMu.Unlock()
	sim, err := run(90*time.Second, "systemd-run", "--quiet", "--wait", "--pipe", "--collect",
		"--unit=router-upgrade-sim", "-p", "Nice=10", "-p", "OOMScoreAdjust=800",
		"apt-get", "-s", "-o", "Debug::NoLocking=1", "dist-upgrade")
	if err != nil {
		log.Printf("updates: simulation: %v", err)
		return pkgs, known
	}
	pkgs = parseAptSim(sim)
	updMu.Lock()
	updKey, updPkgs, updKnown = key, pkgs, true
	updMu.Unlock()
	return pkgs, true
}

func apiUpdates(r *http.Request) (any, error) {
	state, _ := run(5*time.Second, "systemctl", "show", "router-upgrade", "-p", "ActiveState", "--value")
	running := strings.TrimSpace(state) == "active"
	logTxt := readTrim(upgradeLog)
	if len(logTxt) > 20000 {
		logTxt = "…" + logTxt[len(logTxt)-20000:]
	}
	resp := map[string]any{"running": running, "log": logTxt}
	if r.URL.Query().Get("log_only") == "1" { // progress polling during a job
		return resp, nil
	}
	pkgs, known := upgradable(running)
	if pkgs == nil {
		pkgs = []string{}
	}
	st, _ := os.Stat("/var/lib/apt/lists")
	lastCheck := ""
	if st != nil {
		lastCheck = st.ModTime().Format(time.RFC3339)
	}
	resp["upgradable"], resp["upgradable_known"], resp["last_check"] = pkgs, known, lastCheck
	return resp, nil
}

func apiUpdateAction(r *http.Request) (any, error) {
	var cmd string
	switch r.PathValue("action") {
	case "check":
		cmd = "apt-get update"
	case "upgrade":
		cmd = "apt-get update && DEBIAN_FRONTEND=noninteractive apt-get -y -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold dist-upgrade"
	default:
		return nil, badRequest("unknown action")
	}
	if st, _ := run(5*time.Second, "systemctl", "is-active", "router-upgrade"); strings.TrimSpace(st) == "active" {
		return nil, badRequest("an update job is already running")
	}
	_ = os.WriteFile(upgradeLog, []byte(fmt.Sprintf("== %s started %s ==\n", r.PathValue("action"), time.Now().Format(time.RFC3339))), 0o644)
	// Runs as its own transient unit so it survives a restart of the web UI.
	_, err := run(15*time.Second, "systemd-run", "--unit=router-upgrade", "--collect",
		"-p", "StandardOutput=append:"+upgradeLog, "-p", "StandardError=append:"+upgradeLog,
		"/bin/sh", "-c", cmd)
	if err != nil {
		return nil, err
	}
	audit(r, "apt %s started", r.PathValue("action"))
	return map[string]string{"message": "Started. Progress appears below."}, nil
}

// ---------------- backup / restore ----------------

type backupDoc struct {
	Kind    string         `json:"kind"`
	Version int            `json:"version"`
	Created string         `json:"created"`
	Router  Config         `json:"router"`
	Wifi    map[string]any `json:"wifi"`
	LTE     map[string]any `json:"lte"`
}

func apiBackup(r *http.Request) (any, error) {
	wifi, _ := nmGet(hotspotCon, "802-11-wireless.ssid", "802-11-wireless-security.psk", "802-11-wireless.hidden", "802-11-wireless.channel")
	lte, _ := nmGet(lteCon, "gsm.apn", "gsm.username", "gsm.home-only")
	ch, _ := strconv.Atoi(wifi["802-11-wireless.channel"])
	audit(r, "configuration backup downloaded")
	return backupDoc{
		Kind: "kstick-router-backup", Version: 1, Created: time.Now().Format(time.RFC3339), Router: snapshot(),
		Wifi: map[string]any{"ssid": wifi["802-11-wireless.ssid"], "psk": wifi["802-11-wireless-security.psk"], "hidden": wifi["802-11-wireless.hidden"] == "yes", "channel": ch},
		LTE:  map[string]any{"apn": lte["gsm.apn"], "username": lte["gsm.username"], "roaming": lte["gsm.home-only"] != "yes"},
	}, nil
}

func apiRestore(r *http.Request) (any, error) {
	var b backupDoc
	if err := readJSON(r, &b); err != nil {
		return nil, err
	}
	if (b.Kind != "kstick-router-backup" && b.Kind != "openstick-router-backup") || b.Version != 1 {
		return nil, badRequest("not an KStick router backup file")
	}
	next := b.Router
	if err := validateConfig(&next); err != nil {
		return nil, err
	}
	// Check the Wi-Fi part as strictly as the Wi-Fi page before changing anything.
	var wifi struct {
		ssid, psk string
		hidden    bool
		ch        int
	}
	if b.Wifi != nil {
		wifi.ssid, _ = b.Wifi["ssid"].(string)
		wifi.psk, _ = b.Wifi["psk"].(string)
		wifi.hidden, _ = b.Wifi["hidden"].(bool)
		ch, _ := b.Wifi["channel"].(float64)
		if ch != float64(int(ch)) {
			return nil, badRequest("backup: invalid Wi-Fi channel")
		}
		wifi.ch = int(ch)
		if err := validWifi(wifi.ssid, wifi.psk, wifi.ch); err != nil {
			return nil, badRequest("backup: %s", err.Error())
		}
	}
	// The LAN address goes to NetworkManager only as one of updateCfg's apply steps, after the whole restored
	// config has passed validation; if a later step fails, the old address is put back.
	oldAddr := snapshot().LAN.Address
	lanChanged := next.LAN.Address != oldAddr
	setLAN := func(c Config) error {
		if !lanChanged {
			return nil
		}
		return nmModify(lanCon(), "ipv4.method", "manual", "ipv4.addresses", c.LAN.Address)
	}
	undoLAN := func(err error) error {
		if lanChanged {
			_ = nmModify(lanCon(), "ipv4.method", "manual", "ipv4.addresses", oldAddr)
		}
		return err
	}
	res, err := updateCfg(r, "restore from backup", func(c *Config) { *c = next }, setLAN,
		func(c Config) error { return undoLAN(applyFirewall(c)) },
		func(c Config) error { return undoLAN(applyDnsmasq(c)) },
		func(c Config) error { return undoLAN(applyWAN(c)) })
	if err != nil {
		return nil, err
	}
	if lanChanged {
		nmUpLater(lanCon())
	}
	kickVPN()
	if b.LTE != nil {
		if apn, _ := b.LTE["apn"].(string); reAPN.MatchString(apn) {
			roam, _ := b.LTE["roaming"].(bool)
			user, _ := b.LTE["username"].(string)
			if rePrint.MatchString(user) {
				home := "yes"
				if roam {
					home = "no"
				}
				_ = nmModify(lteCon, "gsm.apn", apn, "gsm.username", user, "gsm.home-only", home)
			}
		}
	}
	if b.Wifi != nil {
		h := "no"
		if wifi.hidden {
			h = "yes"
		}
		if err := nmModify(hotspotCon, "802-11-wireless.ssid", wifi.ssid, "802-11-wireless-security.psk", wifi.psk,
			"802-11-wireless.hidden", h, "802-11-wireless.channel", wifiChannelArg(wifi.ch)); err != nil {
			return nil, fmt.Errorf("settings restored, but the Wi-Fi settings could not be applied: %w", err)
		}
		lcdWifiChanged()
		nmUpLater(hotspotCon)
	}
	_ = res
	return map[string]string{"message": "Backup restored. Wi-Fi restarts if its settings changed."}, nil
}
