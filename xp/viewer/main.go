// lofi-viewer — read-only public web front for a KStick router.
//
// Serves the KStick XP web UI in "viewer" mode with its own accounts and its own, read-only data
// collection (/proc, /sys, nmcli, mmcli). It holds no router-admin credential and talks to no
// admin API, so nothing reachable through it can change the router.
//
// Published through Cloudflare Tunnel: a cloudflared on the same device forwards the public hostname to
// this listener on loopback. Only allowlisted peers may connect (loopback by default).
//
//	lofi-viewer serve  [-listen 127.0.0.1:8090] [-web DIR] [-users FILE] [-allow IPs] [-trust IPs]
//	lofi-viewer passwd NAME [-users FILE]   (new password on stdin)
//	lofi-viewer deluser NAME [-users FILE]
package main

import (
	"bufio"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	sessIdle     = time.Hour
	sessMax      = 12 * time.Hour
	maxSessions  = 64
	pbkdfIter    = 60000 // ~0.5 s on the stick's Cortex-A53 (no SHA extensions)
	failWindow   = 15 * time.Minute
	failPerIP    = 5
	failGlobal   = 30
	cookieName   = "kxv"
	minPassword  = 6
	trafficKeep  = 100
	activeSample = 3 * time.Second
	idleSample   = 30 * time.Second
)

// ------------------------------------------------------------------------------------------ users

type userDB struct {
	path  string
	mu    sync.Mutex
	mtime time.Time
	users map[string]string // name -> "pbkdf2-sha256$iter$salt$hash"
}

var nameRe = regexp.MustCompile(`^[a-z][a-z0-9_.-]{1,31}$`)

func (u *userDB) load() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	st, err := os.Stat(u.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			u.users = map[string]string{}
			return nil
		}
		return err
	}
	if u.users != nil && st.ModTime().Equal(u.mtime) {
		return nil
	}
	f, err := os.Open(u.path)
	if err != nil {
		return err
	}
	defer f.Close()
	m := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, h, ok := strings.Cut(line, ":")
		if ok && nameRe.MatchString(name) {
			m[name] = h
		}
	}
	u.users, u.mtime = m, st.ModTime()
	return sc.Err()
}

func hashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	k, err := pbkdf2.Key(sha256.New, pw, salt, pbkdfIter, 32)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", pbkdfIter, base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(k)), nil
}

func checkPassword(stored, pw string) bool {
	p := strings.Split(stored, "$")
	if len(p) != 4 || p[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err := strconv.Atoi(p[1])
	if err != nil || iter < 1000 || iter > 10_000_000 {
		return false
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(p[2])
	want, err2 := base64.RawStdEncoding.DecodeString(p[3])
	if err1 != nil || err2 != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, pw, salt, iter, len(want))
	return err == nil && subtle.ConstantTimeCompare(got, want) == 1
}

// a fixed dummy hash so unknown user names cost the same time as wrong passwords
var dummyHash, _ = hashPassword("dummy-password-for-timing")

func (u *userDB) verify(name, pw string) bool {
	if err := u.load(); err != nil {
		log.Printf("users: %v", err)
	}
	u.mu.Lock()
	h, ok := u.users[name]
	u.mu.Unlock()
	if !ok {
		checkPassword(dummyHash, pw)
		return false
	}
	return checkPassword(h, pw)
}

func editUsers(path, name string, newHash string) error { // newHash "" deletes
	var lines []string
	if b, err := os.ReadFile(path); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if n, _, ok := strings.Cut(l, ":"); ok && n == name {
				continue
			}
			if strings.TrimSpace(l) != "" {
				lines = append(lines, l)
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if newHash != "" {
		lines = append(lines, name+":"+newHash)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(lines, "\n")+"\n"), 0o640); err != nil {
		return err
	}
	// the service user reads the file through its group: give it the directory's group (root:lofiview),
	// or a password change as root leaves it root:root and every sign-in fails
	if st, err := os.Stat(filepath.Dir(path)); err == nil {
		if sys, ok := st.Sys().(*syscall.Stat_t); ok {
			if err := os.Chown(tmp, -1, int(sys.Gid)); err != nil {
				return err
			}
		}
	}
	return os.Rename(tmp, path)
}

// ------------------------------------------------------------------------------------------ sessions & rate limiting

type session struct {
	user, csrf       string
	created, touched time.Time
}

type app struct {
	web     string
	users   *userDB
	allow   []*net.IPNet
	trust   map[string]bool // peers whose CF-Connecting-IP header is believed
	secure  bool
	coll    *collector
	mu      sync.Mutex
	sess    map[string]*session
	fails   map[string][]time.Time
	allFail []time.Time
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func (a *app) getSession(r *http.Request) (string, *session) {
	c, err := r.Cookie(cookieName)
	if err != nil || len(c.Value) != 64 {
		return "", nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.sess[c.Value]
	if s == nil {
		return "", nil
	}
	now := time.Now()
	if now.Sub(s.touched) > sessIdle || now.Sub(s.created) > sessMax {
		delete(a.sess, c.Value)
		return "", nil
	}
	s.touched = now
	return c.Value, s
}

func (a *app) newSession(user string) (string, *session) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	for k, s := range a.sess { // expire, then evict the oldest if still full
		if now.Sub(s.touched) > sessIdle || now.Sub(s.created) > sessMax {
			delete(a.sess, k)
		}
	}
	if len(a.sess) >= maxSessions {
		var oldest string
		for k, s := range a.sess {
			if oldest == "" || s.touched.Before(a.sess[oldest].touched) {
				oldest = k
			}
		}
		delete(a.sess, oldest)
	}
	tok := randHex(32)
	s := &session{user: user, csrf: randHex(16), created: now, touched: now}
	a.sess[tok] = s
	return tok, s
}

func prune(ts []time.Time, now time.Time) []time.Time {
	i := 0
	for _, t := range ts {
		if now.Sub(t) < failWindow {
			ts[i] = t
			i++
		}
	}
	return ts[:i]
}

func (a *app) limited(ip string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	a.allFail = prune(a.allFail, now)
	a.fails[ip] = prune(a.fails[ip], now)
	if len(a.fails[ip]) == 0 {
		delete(a.fails, ip)
	}
	return len(a.fails[ip]) >= failPerIP || len(a.allFail) >= failGlobal
}

func (a *app) recordFail(ip string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	a.fails[ip] = append(a.fails[ip], now)
	a.allFail = append(a.allFail, now)
	if len(a.fails) > 4096 { // memory bound
		a.fails = map[string][]time.Time{}
	}
}

// ------------------------------------------------------------------------------------------ HTTP

func secHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; style-src-attr 'unsafe-inline'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	h.Set("X-Frame-Options", "DENY")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Robots-Tag", "noindex, nofollow")
	h.Set("Strict-Transport-Security", "max-age=31536000")
	h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func errJSON(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": msg})
}

func (a *app) peerAllowed(ip net.IP) bool {
	for _, n := range a.allow {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

func (a *app) clientIP(r *http.Request, peer string) string {
	if a.trust[peer] {
		if c := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); net.ParseIP(c) != nil {
			return c
		}
	}
	return peer
}

func (a *app) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	peer, _, _ := net.SplitHostPort(r.RemoteAddr)
	pip := net.ParseIP(peer)
	if pip == nil || !a.peerAllowed(pip) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	secHeaders(w)
	if strings.HasPrefix(r.URL.Path, "/api/") {
		a.api(w, r, a.clientIP(r, pip.String()))
		return
	}
	a.static(w, r)
}

var staticFiles = map[string]string{"/": "index.html", "/app.js": "app.js", "/style.css": "style.css"}

func (a *app) static(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name, ok := staticFiles[r.URL.Path]
	if !ok {
		if r.URL.Path == "/index.html" {
			http.Redirect(w, r, "./", http.StatusMovedPermanently)
			return
		}
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(filepath.Join(a.web, name))
	if err != nil {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	ct := map[string]string{".html": "text/html; charset=utf-8", ".js": "text/javascript; charset=utf-8", ".css": "text/css; charset=utf-8"}[filepath.Ext(name)]
	w.Header().Set("Content-Type", ct)
	http.ServeContent(w, r, name, st.ModTime(), f)
}

func (a *app) setCookie(w http.ResponseWriter, val string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: val, Path: "/", MaxAge: maxAge, HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteStrictMode})
}

func (a *app) api(w http.ResponseWriter, r *http.Request, ip string) {
	p := r.URL.Path
	if r.Method != http.MethodGet && p != "/api/login" && p != "/api/logout" {
		errJSON(w, http.StatusForbidden, "This is a read-only account: changes are not possible.")
		return
	}
	if r.Method == http.MethodPost && r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		errJSON(w, http.StatusForbidden, "cross-site request refused")
		return
	}
	switch p {
	case "/api/session":
		if _, s := a.getSession(r); s != nil {
			writeJSON(w, 200, map[string]any{"csrf": s.csrf, "user": s.user, "role": "viewer"})
		} else {
			writeJSON(w, 401, map[string]any{"error": "not signed in", "viewer": true})
		}
		return
	case "/api/login":
		if r.Method != http.MethodPost {
			errJSON(w, 405, "method not allowed")
			return
		}
		var in struct{ Username, Password string }
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in); err != nil {
			errJSON(w, 400, "bad request")
			return
		}
		if a.limited(ip) {
			log.Printf("login rate-limited ip=%s", ip)
			errJSON(w, 429, "Too many failed sign-ins. Please try again in 15 minutes.")
			return
		}
		user := strings.ToLower(strings.TrimSpace(in.Username))
		if len(in.Password) > 256 || !nameRe.MatchString(user) || !a.users.verify(user, in.Password) {
			a.recordFail(ip)
			log.Printf("login FAILED user=%q ip=%s", user, ip)
			time.Sleep(400 * time.Millisecond)
			errJSON(w, 401, "invalid username or password")
			return
		}
		tok, s := a.newSession(user)
		a.setCookie(w, tok, int(sessMax.Seconds()))
		log.Printf("login ok user=%q ip=%s", user, ip)
		writeJSON(w, 200, map[string]any{"csrf": s.csrf, "user": user, "role": "viewer"})
		return
	case "/api/logout":
		if r.Method != http.MethodPost {
			errJSON(w, 405, "method not allowed")
			return
		}
		tok, s := a.getSession(r)
		if s != nil && subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(s.csrf)) == 1 {
			a.mu.Lock()
			delete(a.sess, tok)
			a.mu.Unlock()
		}
		a.setCookie(w, "", -1)
		writeJSON(w, 200, map[string]any{})
		return
	}
	if _, s := a.getSession(r); s == nil {
		errJSON(w, 401, "not signed in")
		return
	}
	a.coll.touch()
	switch p {
	case "/api/status":
		writeJSON(w, 200, a.coll.status())
	case "/api/traffic":
		writeJSON(w, 200, a.coll.traffic())
	case "/api/mode":
		writeJSON(w, 200, map[string]any{"mode": a.coll.nm().mode})
	case "/api/modules":
		writeJSON(w, 200, map[string]any{})
	case "/api/speedtest":
		writeJSON(w, 200, map[string]any{"running": false, "last": nil})
	case "/api/pending":
		writeJSON(w, 200, map[string]any{})
	default:
		errJSON(w, http.StatusForbidden, "Not available with the read-only account.")
	}
}

// ------------------------------------------------------------------------------------------ data collection (read-only)

type pt struct {
	Rx float64 `json:"rx"`
	Tx float64 `json:"tx"`
}

type nmInfo struct {
	at              time.Time
	mode, wlanState string
	connected       bool
	signalPct, freq int
	rate            string
}

type collector struct {
	mu         sync.Mutex
	lastActive time.Time
	cpuTot     uint64
	cpuIdle    uint64
	cpuPct     float64
	prevNet    map[string][2]uint64
	prevT      time.Time
	hist       map[string][]pt
	nmc        nmInfo
	modemAt    time.Time
	modemV     map[string]any
}

func (c *collector) touch() { c.mu.Lock(); c.lastActive = time.Now(); c.mu.Unlock() }

var ifRe = regexp.MustCompile(`^(wwan0|usb0|wlan0|wg)`)
var ifaceNameRe = regexp.MustCompile(`^[a-z0-9]{1,15}$`)

func (c *collector) run() {
	for {
		c.sample()
		c.mu.Lock()
		active := time.Since(c.lastActive) < 10*time.Minute
		c.mu.Unlock()
		if active {
			time.Sleep(activeSample)
		} else {
			time.Sleep(idleSample)
		}
	}
}

func (c *collector) sample() {
	now := time.Now()
	var tot, idle uint64
	if b, err := os.ReadFile("/proc/stat"); err == nil {
		line, _, _ := strings.Cut(string(b), "\n")
		f := strings.Fields(line)
		for i, v := range f[1:] {
			n, _ := strconv.ParseUint(v, 10, 64)
			tot += n
			if i == 3 || i == 4 { // idle + iowait
				idle += n
			}
		}
	}
	cur := map[string][2]uint64{}
	if b, err := os.ReadFile("/proc/net/dev"); err == nil {
		for _, l := range strings.Split(string(b), "\n")[2:] {
			name, rest, ok := strings.Cut(l, ":")
			name = strings.TrimSpace(name)
			if !ok || !ifRe.MatchString(name) {
				continue
			}
			f := strings.Fields(rest)
			if len(f) >= 9 {
				rx, _ := strconv.ParseUint(f[0], 10, 64)
				tx, _ := strconv.ParseUint(f[8], 10, 64)
				cur[name] = [2]uint64{rx, tx}
			}
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cpuTot > 0 && tot > c.cpuTot {
		c.cpuPct = 100 * (1 - float64(idle-c.cpuIdle)/float64(tot-c.cpuTot))
	}
	c.cpuTot, c.cpuIdle = tot, idle
	if !c.prevT.IsZero() {
		dt := now.Sub(c.prevT).Seconds()
		for n, v := range cur {
			p, ok := c.prevNet[n]
			if !ok || v[0] < p[0] || v[1] < p[1] || dt <= 0 {
				continue
			}
			h := append(c.hist[n], pt{float64(v[0]-p[0]) / dt, float64(v[1]-p[1]) / dt})
			if len(h) > trafficKeep {
				h = h[len(h)-trafficKeep:]
			}
			c.hist[n] = h
		}
		for n := range c.hist {
			if _, ok := cur[n]; !ok {
				delete(c.hist, n)
			}
		}
	}
	c.prevNet, c.prevT = cur, now
}

func (c *collector) traffic() map[string][]pt {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string][]pt{}
	for n, h := range c.hist {
		out[n] = append([]pt(nil), h...)
	}
	return out
}

func readStr(p string) string {
	b, _ := os.ReadFile(p)
	return strings.TrimSpace(strings.TrimRight(string(b), "\x00"))
}
func readInt(p string) int64 { n, _ := strconv.ParseInt(readStr(p), 10, 64); return n }

func run(timeout time.Duration, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C"}
	done := make(chan struct{})
	var out []byte
	var err error
	go func() { out, err = cmd.Output(); close(done) }()
	select {
	case <-done:
		return string(out), err
	case <-time.After(timeout):
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		<-done
		return "", errors.New("timeout")
	}
}

func (c *collector) nm() nmInfo {
	c.mu.Lock()
	if time.Since(c.nmc.at) < 15*time.Second {
		v := c.nmc
		c.mu.Unlock()
		return v
	}
	c.mu.Unlock()
	v := nmInfo{at: time.Now(), mode: "router"}
	conn := ""
	if out, err := run(8*time.Second, "nmcli", "-t", "-f", "DEVICE,TYPE,STATE,CONNECTION", "device"); err == nil {
		for _, l := range strings.Split(out, "\n") {
			f := strings.SplitN(l, ":", 4)
			if len(f) == 4 && f[0] == "wlan0" {
				v.wlanState, conn = f[2], strings.ReplaceAll(f[3], `\:`, ":")
			}
		}
	}
	if conn != "" {
		if out, err := run(8*time.Second, "nmcli", "-t", "-g", "802-11-wireless.mode", "connection", "show", conn); err == nil && strings.TrimSpace(out) == "infrastructure" {
			v.mode = "client"
		}
	}
	if v.mode == "client" && strings.HasPrefix(v.wlanState, "connected") {
		if out, err := run(8*time.Second, "nmcli", "-t", "-f", "IN-USE,SIGNAL,FREQ,RATE", "device", "wifi", "list", "ifname", "wlan0", "--rescan", "no"); err == nil {
			for _, l := range strings.Split(out, "\n") {
				f := strings.Split(l, ":")
				if len(f) == 4 && f[0] == "*" {
					v.connected = true
					v.signalPct, _ = strconv.Atoi(f[1])
					v.freq, _ = strconv.Atoi(strings.Fields(f[2] + " 0")[0])
					v.rate = f[3]
				}
			}
		}
	}
	c.mu.Lock()
	c.nmc = v
	c.mu.Unlock()
	return v
}

func dig(m any, path ...string) any {
	for _, k := range path {
		mm, ok := m.(map[string]any)
		if !ok {
			return nil
		}
		m = mm[k]
	}
	return m
}

func clean(v any) string { // mmcli uses "--" for "unknown"
	switch x := v.(type) {
	case string:
		if x == "--" {
			return ""
		}
		return x
	case []any:
		var s []string
		for _, e := range x {
			if t := clean(e); t != "" {
				s = append(s, t)
			}
		}
		return strings.Join(s, ", ")
	}
	return ""
}

// modem summary from an allowlist of fields: IMEI / IMSI / ICCID / numbers / APN / IP addresses are never copied
func (c *collector) modem() map[string]any {
	c.mu.Lock()
	if c.modemV != nil && time.Since(c.modemAt) < 30*time.Second {
		v := c.modemV
		c.mu.Unlock()
		return v
	}
	c.mu.Unlock()
	v := map[string]any{"present": false}
	if out, err := run(10*time.Second, "mmcli", "-m", "any", "-J"); err == nil {
		var j map[string]any
		if json.Unmarshal([]byte(out), &j) == nil {
			g, t := dig(j, "modem", "generic"), dig(j, "modem", "3gpp")
			sig, _ := strconv.Atoi(clean(dig(g, "signal-quality", "value")))
			v = map[string]any{
				"present": true, "state": clean(dig(g, "state")), "power_state": clean(dig(g, "power-state")),
				"operator": clean(dig(t, "operator-name")), "operator_code": clean(dig(t, "operator-code")),
				"registration": clean(dig(t, "registration-state")), "packet_service": clean(dig(t, "packet-service-state")),
				"signal": sig, "tech": clean(dig(g, "access-technologies")), "modes": clean(dig(g, "current-modes")),
				"bands":    strings.ReplaceAll(clean(dig(g, "current-bands")), ", ", ","),
				"firmware": clean(dig(g, "revision")), "hw_revision": clean(dig(g, "hardware-revision")),
			}
		}
	}
	c.mu.Lock()
	c.modemV, c.modemAt = v, time.Now()
	c.mu.Unlock()
	return v
}

func temps() []map[string]any {
	zones, _ := filepath.Glob("/sys/class/thermal/thermal_zone*")
	var out []map[string]any
	for _, z := range zones {
		t := readInt(z + "/temp")
		if t == 0 {
			continue
		}
		name := readStr(z + "/type")
		e := map[string]any{"name": name, "label": strings.TrimSuffix(name, "-thermal"), "c": float64(t) / 1000}
		trip := int64(0)
		tps, _ := filepath.Glob(z + "/trip_point_*_type")
		for _, tp := range tps {
			if readStr(tp) == "passive" {
				if v := readInt(strings.TrimSuffix(tp, "_type")+"_temp") / 1000; v > 0 && (trip == 0 || v < trip) {
					trip = v
				}
			}
		}
		if trip > 0 {
			e["trip"] = trip
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["c"].(float64) > out[j]["c"].(float64) })
	return out
}

func meminfo() (total, avail int64) {
	b, _ := os.ReadFile("/proc/meminfo")
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		if len(f) >= 2 {
			n, _ := strconv.ParseInt(f[1], 10, 64)
			switch f[0] {
			case "MemTotal:":
				total = n * 1024
			case "MemAvailable:":
				avail = n * 1024
			}
		}
	}
	return
}

// /run/router/wan.state (written by wan-failover): only up/down per uplink and the active one; no gateways
func wanState() map[string]any {
	w := map[string]any{"active": "none"}
	b, _ := os.ReadFile("/run/router/wan.state")
	for _, l := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(l), "=")
		if !ok {
			continue
		}
		switch {
		case k == "active":
			if ifaceNameRe.MatchString(v) {
				w["active"] = v
			}
		case k == "updated":
			w["updated"] = v
		case ifRe.MatchString(k) || k == "bnep0":
			st := strings.Fields(v + " ")[0]
			if st == "up" || st == "down" {
				w[k] = st
			}
		}
	}
	return w
}

func (c *collector) status() map[string]any {
	host, _ := os.Hostname()
	up, _ := strconv.ParseFloat(strings.Fields(readStr("/proc/uptime") + " 0")[0], 64)
	var load []float64
	for _, f := range strings.Fields(readStr("/proc/loadavg")) {
		if x, err := strconv.ParseFloat(f, 64); err == nil && len(load) < 3 {
			load = append(load, x)
		}
	}
	mt, ma := meminfo()
	var fs syscall.Statfs_t
	_ = syscall.Statfs("/", &fs)
	c.mu.Lock()
	cpu := c.cpuPct
	c.mu.Unlock()
	cf := "/sys/devices/system/cpu/cpu0/cpufreq/"
	sys := map[string]any{
		"hostname": host, "model": readStr("/proc/device-tree/model"), "uptime": up, "cpu": cpu, "load": load,
		"mem_total": mt, "mem_available": ma,
		"disk_total": int64(fs.Blocks) * int64(fs.Bsize), "disk_free": int64(fs.Bavail) * int64(fs.Bsize),
		"temps": temps(), "conntrack": readInt("/proc/sys/net/netfilter/nf_conntrack_count"), "conntrack_max": readInt("/proc/sys/net/netfilter/nf_conntrack_max"),
	}
	if cur := readInt(cf + "scaling_cur_freq"); cur > 0 {
		sys["cpufreq"] = map[string]any{"cur_mhz": cur / 1000, "max_mhz": readInt(cf+"scaling_max_freq") / 1000, "hw_max_mhz": readInt(cf+"cpuinfo_max_freq") / 1000, "governor": readStr(cf + "scaling_governor")}
	}
	n := c.nm()
	out := map[string]any{"system": sys, "wan": wanState(), "mode": n.mode, "modem": c.modem()}
	if n.mode == "client" {
		wl := map[string]any{"connected": n.connected}
		if n.connected {
			wl["signal_dbm"] = n.signalPct/2 - 100 // nmcli reports %, the UI expects dBm
			wl["freq_mhz"] = n.freq
			wl["tx_bitrate"] = n.rate
		}
		out["wifi_uplink"] = wl
	} else {
		st := "off"
		if strings.HasPrefix(n.wlanState, "connected") {
			st = "activated"
		}
		out["wifi"] = map[string]any{"state": st}
	}
	return out
}

// ------------------------------------------------------------------------------------------ main

func parseNets(s string) ([]*net.IPNet, error) {
	var out []*net.IPNet
	for _, f := range strings.Split(s, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if !strings.Contains(f, "/") {
			if strings.Contains(f, ":") {
				f += "/128"
			} else {
				f += "/32"
			}
		}
		_, n, err := net.ParseCIDR(f)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: lofi-viewer serve|passwd NAME|deluser NAME [flags]")
		os.Exit(2)
	}
	cmd := os.Args[1]
	fl := flag.NewFlagSet(cmd, flag.ExitOnError)
	usersPath := fl.String("users", "/etc/lofi-viewer/users", "viewer accounts file")
	switch cmd {
	case "passwd", "deluser":
		if len(os.Args) < 3 || !nameRe.MatchString(os.Args[2]) {
			log.Fatal("give a user name: lower-case letters, digits, . _ - (2-32 chars, starting with a letter)")
		}
		name := os.Args[2]
		_ = fl.Parse(os.Args[3:])
		if cmd == "deluser" {
			if err := editUsers(*usersPath, name, ""); err != nil {
				log.Fatal(err)
			}
			fmt.Println("removed", name)
			return
		}
		pw, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && pw == "" {
			log.Fatal("no password on stdin")
		}
		pw = strings.TrimRight(pw, "\r\n")
		if len(pw) < minPassword {
			log.Fatalf("password too short (min %d characters)", minPassword)
		}
		h, err := hashPassword(pw)
		if err != nil {
			log.Fatal(err)
		}
		if err := editUsers(*usersPath, name, h); err != nil {
			log.Fatal(err)
		}
		fmt.Println("password set for", name)
	case "serve":
		listen := fl.String("listen", "127.0.0.1:8090", "listen address")
		web := fl.String("web", "/usr/local/share/router-webui/web-xp", "KStick XP web files")
		allow := fl.String("allow", "127.0.0.1,::1", "peers allowed to connect (IPs/CIDRs)")
		trust := fl.String("trust", "127.0.0.1,::1", "peers whose CF-Connecting-IP header is trusted")
		insecure := fl.Bool("insecure-cookie", false, "omit the Secure cookie flag (local testing over plain http)")
		_ = fl.Parse(os.Args[2:])
		nets, err := parseNets(*allow)
		if err != nil {
			log.Fatal(err)
		}
		tr := map[string]bool{}
		for _, t := range strings.Split(*trust, ",") {
			if ip := net.ParseIP(strings.TrimSpace(t)); ip != nil {
				tr[ip.String()] = true
			}
		}
		u := &userDB{path: *usersPath}
		if err := u.load(); err != nil {
			log.Fatalf("users file: %v", err)
		}
		if len(u.users) == 0 {
			log.Printf("warning: no viewer accounts in %s (add one with: lofi-viewer passwd NAME)", *usersPath)
		}
		coll := &collector{hist: map[string][]pt{}, prevNet: map[string][2]uint64{}}
		go coll.run()
		a := &app{web: *web, users: u, allow: nets, trust: tr, secure: !*insecure, coll: coll, sess: map[string]*session{}, fails: map[string][]time.Time{}}
		srv := &http.Server{Addr: *listen, Handler: a, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second, MaxHeaderBytes: 16 << 10}
		log.Printf("lofi-viewer listening on %s (allowed peers: %s)", *listen, *allow)
		log.Fatal(srv.ListenAndServe())
	default:
		log.Fatalf("unknown command %q", cmd)
	}
}
