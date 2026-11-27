package main

// Forward received SMS to WhatsApp through a WAWAT gateway (/api/v1/*).
// The API token lives only in /etc/router/wawat.json (root, 0600); the browser
// never sees it after saving.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/caseypaite/kstick-router/internal/smsdb"
)

const (
	wawatPath         = "/etc/router/wawat.json"
	wawatState        = "/var/lib/router/wawat-state.json"
	defaultWATemplate = "📩 SMS received on KStick\nFrom: {from}\nTime: {time}\n\n{text}"
)

type WawatCfg struct {
	Enabled   bool     `json:"enabled"`
	ServerURL string   `json:"server_url"`
	APIKey    string   `json:"api_key"`
	Target    string   `json:"target"`
	Template  string   `json:"template"`
	Ignore    []string `json:"ignore"` // sender substrings to skip (case-insensitive)
}

type waLog struct {
	Time    string            `json:"time"`
	Kind    string            `json:"kind"` // forward | verify | test
	From    string            `json:"from,omitempty"`
	Text    string            `json:"text,omitempty"`
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Status  int               `json:"status"`
	Ms      int64             `json:"ms"`
	Result  string            `json:"result"` // ok | error | skipped
	Error   string            `json:"error,omitempty"`
	Resp    string            `json:"response,omitempty"`
	Headers map[string]string `json:"auth_headers,omitempty"`
}

var (
	waMu       sync.Mutex
	waLogs     []waLog
	waKick     = make(chan struct{}, 1)
	waLastErr  string
	reWATarget = regexp.MustCompile(`^(\d{10,15}|\d{10,15}@c\.us|[0-9-]{10,40}@g\.us)$`)
)

func loadWawat() WawatCfg {
	c := WawatCfg{Template: defaultWATemplate, Ignore: []string{}}
	if b, err := os.ReadFile(wawatPath); err == nil {
		json.Unmarshal(b, &c)
	}
	if c.Template == "" {
		c.Template = defaultWATemplate
	}
	if c.Ignore == nil {
		c.Ignore = []string{}
	}
	if !modules.WhatsApp {
		c.Enabled = false
	}
	return c
}

func saveWawat(c WawatCfg) error {
	b, _ := json.MarshalIndent(c, "", "  ")
	return writeAtomic(wawatPath, append(b, '\n'), 0600)
}

func waJID(t string) string {
	switch {
	case strings.Contains(t, "@"):
		return t
	case len(t) == 10:
		return "91" + t + "@c.us"
	default:
		return t + "@c.us"
	}
}

func addWALog(e waLog) {
	waMu.Lock()
	defer waMu.Unlock()
	waLogs = append(waLogs, e)
	if len(waLogs) > 100 {
		waLogs = waLogs[len(waLogs)-100:]
	}
}

var waHTTP = &http.Client{Timeout: 20 * time.Second}

// waCall performs one gateway request and records it in the debug log.
func waCall(c WawatCfg, kind, method, path string, body any, e waLog) (map[string]any, waLog, error) {
	e.Time, e.Kind, e.Method, e.Path = time.Now().Format(time.RFC3339), kind, method, path
	if c.ServerURL == "" || c.APIKey == "" {
		e.Result, e.Error = "error", "server address and API token are required"
		addWALog(e)
		return nil, e, errors.New(e.Error)
	}
	if err := checkSecretURL(c.ServerURL); err != nil { // e.g. an http:// address saved by an older version
		e.Result, e.Error = "error", "server address "+err.Error()
		addWALog(e)
		return nil, e, errors.New(e.Error)
	}
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, strings.TrimRight(c.ServerURL, "/")+path, rd)
	req.Header.Set("x-api-key", c.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "kstick-router")
	t0 := time.Now()
	resp, err := waHTTP.Do(req)
	e.Ms = time.Since(t0).Milliseconds()
	if err != nil {
		e.Result, e.Error = "error", err.Error()
		addWALog(e)
		return nil, e, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	e.Status, e.Resp = resp.StatusCode, truncRunes(string(raw), 600)
	e.Headers = map[string]string{}
	for k, v := range resp.Header {
		if lk := strings.ToLower(k); strings.HasPrefix(lk, "x-wawat-auth-") {
			e.Headers[strings.TrimPrefix(lk, "x-wawat-auth-")] = strings.Join(v, ", ")
		}
	}
	var m map[string]any
	json.Unmarshal(raw, &m)
	switch {
	case resp.StatusCode == 503:
		err = fmt.Errorf("WhatsApp client not ready (503, Retry-After %s)", resp.Header.Get("Retry-After"))
	case resp.StatusCode == 429:
		err = errors.New("rate limited by the WhatsApp server (429)")
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		err = fmt.Errorf("API token rejected (%d)", resp.StatusCode)
	case resp.StatusCode >= 300:
		msg := fmt.Sprint(m["error"])
		if m["error"] == nil {
			msg = truncRunes(strings.TrimSpace(string(raw)), 200)
		}
		err = fmt.Errorf("HTTP %d: %s", resp.StatusCode, msg)
	case kind != "verify" && m["success"] != true:
		err = fmt.Errorf("server did not confirm the send: %s", truncRunes(string(raw), 200))
	}
	if err != nil {
		e.Result, e.Error = "error", err.Error()
	} else {
		e.Result = "ok"
	}
	addWALog(e)
	return m, e, err
}

// waSend posts one message and returns the WhatsApp message id.
func waSend(c WawatCfg, kind, text string, e waLog) (string, error) {
	path, body := "/api/v1/message/single", map[string]any{"number": c.Target, "message": text}
	if strings.HasSuffix(c.Target, "@g.us") {
		path, body = "/api/v1/message/group", map[string]any{"groupId": c.Target, "message": text}
	}
	m, _, err := waCall(c, kind, "POST", path, body, e)
	if err != nil {
		return "", err
	}
	return fmt.Sprint(m["messageId"]), nil
}

func truncRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

func smsHash(from, ts, text string) string {
	h := sha256.Sum256([]byte(from + "\x00" + ts + "\x00" + text))
	return hex.EncodeToString(h[:12])
}

func kickWawat() {
	select {
	case waKick <- struct{}{}:
	default:
	}
}

type rxSMS struct{ path, from, ts, text, smsc, hash string }

// forwardPending sends stored SMS with wa_status=pending whose retry time has come, oldest first.
// A failure stops the batch (order is kept); the SMS is retried with growing gaps (smsdb.NextRetry: 30 s doubling
// to hourly) and given up after smsdb.MaxAttempts, about a day.
func forwardPending() {
	c := loadWawat()
	d := smsDB()
	if !c.Enabled || d == nil {
		return
	}
	list, err := d.PendingIn(20)
	if err != nil {
		log.Printf("wawat: %v", err)
		return
	}
	for _, m := range list {
		e := waLog{From: m.Sender, Text: truncRunes(m.Text, 160)}
		id, err := waSend(c, "forward", renderWA(c.Template, rxSMS{from: m.Sender, ts: m.SMSTime, text: m.Text}), e)
		jid := waJID(c.Target)
		if err == nil {
			waMu.Lock()
			waLastErr = ""
			waMu.Unlock()
			if err := d.MarkForwarded(m.ID, jid, id); err != nil {
				log.Printf("wawat: %v", err)
			}
			continue
		}
		waMu.Lock()
		waLastErr = err.Error()
		waMu.Unlock()
		attempts := m.WAAttempts + 1
		final := attempts >= smsdb.MaxAttempts
		note := err.Error()
		if final {
			note = fmt.Sprintf("gave up after %d attempts: %s", attempts, note)
		}
		d.MarkAttempt(m.ID, jid, truncRunes(note, 300), final, smsdb.NextRetry(attempts, time.Now()))
		if !final {
			return
		}
	}
}

func ignoredSender(c WawatCfg, from string) bool {
	f := strings.ToLower(from)
	for _, p := range c.Ignore {
		if p = strings.ToLower(strings.TrimSpace(p)); p != "" && strings.Contains(f, p) {
			return true
		}
	}
	return false
}

func renderWA(tpl string, m rxSMS) string {
	ts := m.ts
	if t, err := time.Parse(time.RFC3339, ts); err == nil {
		ts = t.Local().Format("02 Jan 2006 15:04:05")
	} else if t, err := time.Parse("2006-01-02T15:04:05-07", ts); err == nil {
		ts = t.Local().Format("02 Jan 2006 15:04:05")
	}
	return strings.NewReplacer("{from}", m.from, "{time}", ts, "{text}", m.text, "{device}", "KStick").Replace(tpl)
}

// ---------------------------------------------------------------- API

func wawatView(c WawatCfg) map[string]any {
	hint := ""
	if n := len(c.APIKey); n > 4 {
		hint = "…" + c.APIKey[n-4:]
	}
	waMu.Lock()
	logs := append([]waLog{}, waLogs...)
	lastErr := waLastErr
	waMu.Unlock()
	var forwarded map[string]int
	if d := smsDB(); d != nil {
		if st, err := d.Stats(); err == nil {
			forwarded = st.WAByStatus
		}
	}
	c.APIKey = ""
	jid := ""
	if c.Target != "" {
		jid = waJID(c.Target)
	}
	return map[string]any{"config": c, "key_set": hint != "", "key_hint": hint, "resolved_target": jid,
		"last_error": lastErr, "handled": forwarded, "log": logs, "default_template": defaultWATemplate}
}

func apiGetWawat(r *http.Request) (any, error) { return wawatView(loadWawat()), nil }

func apiPutWawat(r *http.Request) (any, error) {
	var b WawatCfg
	if err := readJSON(r, &b); err != nil {
		return nil, err
	}
	b.ServerURL = strings.TrimRight(strings.TrimSpace(b.ServerURL), "/")
	b.Target = strings.ReplaceAll(strings.TrimPrefix(strings.TrimSpace(b.Target), "+"), " ", "")
	if b.ServerURL != "" {
		if err := checkSecretURL(b.ServerURL); err != nil {
			return nil, badRequest("server address %s", err.Error())
		}
	}
	if b.Target != "" && !reWATarget.MatchString(b.Target) {
		return nil, badRequest("WhatsApp number: 10 digits (India) or full international number, digits only; or a group id ending in @g.us")
	}
	if strings.TrimSpace(b.Template) == "" {
		b.Template = defaultWATemplate
	}
	if len([]rune(b.Template)) > 1000 || !strings.Contains(b.Template, "{text}") {
		return nil, badRequest("message format must contain {text} and be at most 1000 characters")
	}
	if len(b.APIKey) > 512 || strings.ContainsAny(b.APIKey, "\r\n ") {
		return nil, badRequest("invalid API token")
	}
	var ign []string
	for _, s := range b.Ignore {
		if s = strings.TrimSpace(s); s != "" && len(ign) < 50 {
			ign = append(ign, truncRunes(s, 40))
		}
	}
	b.Ignore = ign
	old := loadWawat()
	if b.APIKey == "" { // write-only field: empty keeps the stored token
		b.APIKey = old.APIKey
	}
	if b.Enabled && (b.ServerURL == "" || b.APIKey == "" || b.Target == "") {
		return nil, badRequest("set the server address, API token and WhatsApp number before enabling forwarding")
	}
	if err := saveWawat(b); err != nil {
		return nil, err
	}
	audit(r, "WhatsApp SMS forwarding updated (enabled=%v, target=%s)", b.Enabled, b.Target)
	kickWawat()
	return map[string]any{"message": "WhatsApp forwarding saved."}, nil
}

func apiVerifyWawat(r *http.Request) (any, error) {
	c := loadWawat()
	m, e, err := waCall(c, "verify", "GET", "/api/v1/status", nil, waLog{})
	res := map[string]any{"ok": err == nil && m["ready"] == true, "http_status": e.Status, "auth_headers": e.Headers}
	if err != nil {
		res["error"] = err.Error()
		return res, nil
	}
	res["status"], res["ready"] = m["status"], m["ready"]
	if u := dig(m, "me", "wid", "user"); u != nil {
		res["linked_number"] = u
	}
	return res, nil
}

func apiTestWawat(r *http.Request) (any, error) {
	c := loadWawat()
	if c.Target == "" {
		return nil, badRequest("set the WhatsApp number first")
	}
	m := rxSMS{from: "KStick (test)", ts: time.Now().Format(time.RFC3339), text: "Test message: SMS forwarding from your KStick is configured."}
	if _, err := waSend(c, "test", renderWA(c.Template, m), waLog{From: m.from, Text: m.text}); err != nil {
		return nil, badRequest("test failed: %s", err.Error())
	}
	audit(r, "WhatsApp test message sent to %s", c.Target)
	return map[string]any{"message": "Test message sent to " + waJID(c.Target) + "."}, nil
}
