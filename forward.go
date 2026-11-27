package main

// Forward received SMS to targets other than WhatsApp: a generic webhook (JSON, optionally HMAC-signed), a Telegram
// bot, or an ntfy topic. Each SMS gets one row per target in the database's sms_fwd table when it arrives, and the
// SMS loop delivers them with the same retry policy as WhatsApp (30 s doubling to hourly, about a day in all).
// Secrets live only in /etc/router/forward.json (root, 0600); the browser never sees them after saving.

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/caseypaite/kstick-router/internal/smsdb"
)

var forwardPath = "/etc/router/forward.json" // a var for tests

// Forwarding targets, in the order they are shown and tried.
var fwdTargets = []string{"webhook", "telegram", "ntfy"}

type WebhookCfg struct {
	Enabled bool   `json:"enabled"`
	URL     string `json:"url"`
	Secret  string `json:"secret"` // HMAC-SHA256 key for X-KStick-Signature; optional
}

type TelegramCfg struct {
	Enabled  bool   `json:"enabled"`
	BotToken string `json:"bot_token"`
	ChatID   string `json:"chat_id"`
}

type NtfyCfg struct {
	Enabled bool   `json:"enabled"`
	Server  string `json:"server"` // default https://ntfy.sh
	Topic   string `json:"topic"`
	Token   string `json:"token"` // access token for protected topics; optional
}

type ForwardCfg struct {
	Webhook  WebhookCfg  `json:"webhook"`
	Telegram TelegramCfg `json:"telegram"`
	Ntfy     NtfyCfg     `json:"ntfy"`
	Ignore   []string    `json:"ignore"` // sender substrings to skip (case-insensitive), for all three
}

var (
	fwdMu   sync.Mutex
	fwdLogs = map[string][]waLog{} // per target, last 30 calls (same shape as the WhatsApp log)

	reTgToken   = regexp.MustCompile(`^[0-9]{5,12}:[A-Za-z0-9_-]{30,60}$`)
	reTgChat    = regexp.MustCompile(`^(-?[0-9]{1,20}|@[A-Za-z0-9_]{5,32})$`)
	reNtfyTop   = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	reHTTPURL   = regexp.MustCompile(`^https?://[A-Za-z0-9.-]+(:[0-9]{1,5})?(/[A-Za-z0-9._~/%-]*)?(\?[A-Za-z0-9._~=&%-]*)?$`)
	fwdHTTP     = &http.Client{Timeout: 20 * time.Second, CheckRedirect: noRedirect}
	telegramAPI = "https://api.telegram.org" // tests point this at a local server
)

// noRedirect: a redirect could carry a secret header to another host.
func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

func loadForward() ForwardCfg {
	c := ForwardCfg{Ignore: []string{}}
	if b, err := os.ReadFile(forwardPath); err == nil {
		json.Unmarshal(b, &c)
	}
	if c.Ntfy.Server == "" {
		c.Ntfy.Server = "https://ntfy.sh"
	}
	if c.Ignore == nil {
		c.Ignore = []string{}
	}
	return c
}

func saveForward(c ForwardCfg) error {
	b, _ := json.MarshalIndent(c, "", "  ")
	return writeAtomic(forwardPath, append(b, '\n'), 0600)
}

// enabledTargets: the targets a newly received SMS from this sender is queued for.
func (c ForwardCfg) enabledTargets(from string) []string {
	if ignoredBy(c.Ignore, from) {
		return nil
	}
	var t []string
	if c.Webhook.Enabled {
		t = append(t, "webhook")
	}
	if c.Telegram.Enabled {
		t = append(t, "telegram")
	}
	if c.Ntfy.Enabled {
		t = append(t, "ntfy")
	}
	return t
}

func (c ForwardCfg) enabled(target string) bool {
	switch target {
	case "webhook":
		return c.Webhook.Enabled
	case "telegram":
		return c.Telegram.Enabled
	case "ntfy":
		return c.Ntfy.Enabled
	}
	return false
}

func ignoredBy(list []string, from string) bool {
	f := strings.ToLower(from)
	for _, p := range list {
		if p = strings.ToLower(strings.TrimSpace(p)); p != "" && strings.Contains(f, p) {
			return true
		}
	}
	return false
}

// checkSecretURL: a URL that a secret (token, signature key) is sent to must be https, unless it points at a
// private address by IP (a server on the LAN, the USB link or Cloudflare Mesh), where plain http is allowed.
func checkSecretURL(raw string) error {
	if !reHTTPURL.MatchString(raw) {
		return errors.New("must look like https://example.com/path")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme == "https" {
		return nil
	}
	if ip, err := netip.ParseAddr(u.Hostname()); err == nil && privateAddr(ip) {
		return nil
	}
	return errors.New("must use https:// (plain http:// would send the token unencrypted; it is only allowed for private IP addresses such as 192.168.x.x)")
}

func privateAddr(ip netip.Addr) bool {
	ip = ip.Unmap()
	cgnat := netip.MustParsePrefix("100.64.0.0/10") // Cloudflare Mesh / Tailscale-style overlays
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || cgnat.Contains(ip)
}

func addFwdLog(target string, e waLog) {
	fwdMu.Lock()
	defer fwdMu.Unlock()
	l := append(fwdLogs[target], e)
	if len(l) > 30 {
		l = l[len(l)-30:]
	}
	fwdLogs[target] = l
}

// permanentErr: a 4xx answer other than 408/429 will not get better by retrying (bad token, unknown chat).
type permanentErr struct{ error }

// fwdPost performs one request and records it. ok: 2xx.
func fwdPost(target, kind, u string, body []byte, hdr map[string]string, e waLog) ([]byte, error) {
	e.Time, e.Kind, e.Method = time.Now().Format(time.RFC3339), kind, "POST"
	if pu, err := url.Parse(u); err == nil {
		e.Path = pu.Host + redactPath(pu.Path)
	}
	req, err := http.NewRequest("POST", u, bytes.NewReader(body))
	if err != nil {
		e.Result, e.Error = "error", err.Error()
		addFwdLog(target, e)
		return nil, err
	}
	req.Header.Set("User-Agent", "kstick-router")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	t0 := time.Now()
	resp, err := fwdHTTP.Do(req)
	e.Ms = time.Since(t0).Milliseconds()
	if err != nil {
		err = errors.New(redactToken(err.Error()))
		e.Result, e.Error = "error", err.Error()
		addFwdLog(target, e)
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<10))
	e.Status, e.Resp = resp.StatusCode, truncRunes(string(raw), 400)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		err = fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncRunes(strings.TrimSpace(string(raw)), 200))
		if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != 408 && resp.StatusCode != 429 {
			err = permanentErr{err}
		}
		e.Result, e.Error = "error", err.Error()
	} else {
		e.Result = "ok"
	}
	addFwdLog(target, e)
	return raw, err
}

// The Telegram bot token is part of the URL path: never log or show it.
func redactPath(p string) string {
	if strings.HasPrefix(p, "/bot") {
		if i := strings.Index(p[1:], "/"); i > 0 {
			return "/bot…" + p[1+i:]
		}
	}
	return p
}

var reBotInURL = regexp.MustCompile(`/bot[0-9]+:[A-Za-z0-9_-]+`)

func redactToken(s string) string { return reBotInURL.ReplaceAllString(s, "/bot…") }

func fwdText(m rxSMS) string {
	return renderWA("📩 SMS from {from}\n{time}\n\n{text}", m)
}

// fwdSend delivers one SMS (or a test) to target.
func fwdSend(c ForwardCfg, target, kind string, id int64, m rxSMS, storedAt string) error {
	e := waLog{From: m.from, Text: truncRunes(m.text, 160)}
	switch target {
	case "webhook":
		body, _ := json.Marshal(map[string]any{"event": "sms.received", "id": id, "from": m.from, "text": m.text,
			"sms_time": m.ts, "received_at": storedAt, "device": hostname(), "test": kind == "test"})
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		hdr := map[string]string{"Content-Type": "application/json", "X-KStick-Event": "sms.received",
			"X-KStick-Delivery": strconv.FormatInt(id, 10), "X-KStick-Timestamp": ts}
		if c.Webhook.Secret != "" {
			hdr["X-KStick-Signature"] = "sha256=" + webhookSignature(c.Webhook.Secret, ts, body)
		}
		_, err := fwdPost(target, kind, c.Webhook.URL, body, hdr, e)
		return err
	case "telegram":
		body, _ := json.Marshal(map[string]any{"chat_id": c.Telegram.ChatID, "text": fwdText(m), "disable_web_page_preview": true})
		raw, err := fwdPost(target, kind, telegramAPI+"/bot"+c.Telegram.BotToken+"/sendMessage", body,
			map[string]string{"Content-Type": "application/json"}, e)
		if err == nil {
			var r struct {
				OK bool `json:"ok"`
			}
			if json.Unmarshal(raw, &r) != nil || !r.OK {
				err = errors.New("Telegram did not confirm the message")
			}
		}
		return err
	case "ntfy":
		hdr := map[string]string{"Title": "SMS from " + asciiHeader(m.from), "Tags": "envelope", "Content-Type": "text/plain; charset=utf-8"}
		if c.Ntfy.Token != "" {
			hdr["Authorization"] = "Bearer " + c.Ntfy.Token
		}
		_, err := fwdPost(target, kind, strings.TrimRight(c.Ntfy.Server, "/")+"/"+c.Ntfy.Topic, []byte(fwdText(m)), hdr, e)
		return err
	}
	return fmt.Errorf("unknown target %q", target)
}

// webhookSignature = hex(HMAC-SHA256(secret, timestamp + "." + body)); receivers should also reject old timestamps.
func webhookSignature(secret, ts string, body []byte) string {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(ts + "."))
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

// asciiHeader keeps HTTP header values plain ASCII.
func asciiHeader(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e {
			return -1
		}
		return r
	}, s)
}

// forwardOthers delivers pending SMS to each enabled target, oldest first. A failure stops that target's batch
// (order is kept) until its retry time; disabled targets keep their queue until switched on again.
func forwardOthers() {
	d := smsDB()
	if d == nil {
		return
	}
	c := loadForward()
	for _, target := range fwdTargets {
		if !c.enabled(target) {
			continue
		}
		ins, fs, err := d.PendingFwd(target, 20)
		if err != nil {
			log.Printf("forward %s: %v", target, err)
			continue
		}
		for i, m := range ins {
			err := fwdSend(c, target, "forward", m.ID, rxSMS{from: m.Sender, ts: m.SMSTime, text: m.Text}, m.StoredAt)
			if err == nil {
				d.MarkFwd(m.ID, target, true, "", false, time.Time{})
				continue
			}
			attempts := fs[i].Attempts + 1
			var perm permanentErr
			final := attempts >= smsdb.MaxAttempts || errors.As(err, &perm)
			note := err.Error()
			if final {
				note = fmt.Sprintf("gave up after %d attempt(s): %s", attempts, note)
			}
			d.MarkFwd(m.ID, target, false, note, final, smsdb.NextRetry(attempts, time.Now()))
			if !final {
				break
			}
		}
	}
}

// ---------------------------------------------------------------- API

func secretHint(s string) string {
	if n := len(s); n > 4 {
		return "…" + s[n-4:]
	}
	if s != "" {
		return "set"
	}
	return ""
}

func apiGetForward(r *http.Request) (any, error) {
	c := loadForward()
	fwdMu.Lock()
	logs := map[string][]waLog{}
	for k, v := range fwdLogs {
		logs[k] = append([]waLog{}, v...)
	}
	fwdMu.Unlock()
	secrets := map[string]string{"webhook_secret": secretHint(c.Webhook.Secret), "telegram_token": secretHint(c.Telegram.BotToken),
		"ntfy_token": secretHint(c.Ntfy.Token)}
	c.Webhook.Secret, c.Telegram.BotToken, c.Ntfy.Token = "", "", ""
	return map[string]any{"config": c, "secrets": secrets, "log": logs, "telegram_api": telegramAPI}, nil
}

func apiPutForward(r *http.Request) (any, error) {
	var b ForwardCfg
	if err := readJSON(r, &b); err != nil {
		return nil, err
	}
	old := loadForward()
	// write-only secrets: empty keeps the stored one; "-" clears it
	keep := func(nv, ov string) string {
		switch strings.TrimSpace(nv) {
		case "":
			return ov
		case "-":
			return ""
		}
		return strings.TrimSpace(nv)
	}
	b.Webhook.Secret = keep(b.Webhook.Secret, old.Webhook.Secret)
	b.Telegram.BotToken = keep(b.Telegram.BotToken, old.Telegram.BotToken)
	b.Ntfy.Token = keep(b.Ntfy.Token, old.Ntfy.Token)
	b.Webhook.URL = strings.TrimSpace(b.Webhook.URL)
	b.Telegram.ChatID = strings.TrimSpace(b.Telegram.ChatID)
	b.Ntfy.Server = strings.TrimRight(strings.TrimSpace(b.Ntfy.Server), "/")
	if b.Ntfy.Server == "" {
		b.Ntfy.Server = "https://ntfy.sh"
	}
	b.Ntfy.Topic = strings.TrimSpace(b.Ntfy.Topic)
	if err := validateForward(&b); err != nil {
		return nil, err
	}
	if err := saveForward(b); err != nil {
		return nil, err
	}
	audit(r, "SMS forwarding updated (webhook=%v telegram=%v ntfy=%v)", b.Webhook.Enabled, b.Telegram.Enabled, b.Ntfy.Enabled)
	kickWawat()
	return map[string]string{"message": "Forwarding saved."}, nil
}

func validateForward(b *ForwardCfg) error {
	if b.Webhook.URL != "" {
		if err := checkSecretURL(b.Webhook.URL); err != nil {
			return badRequest("webhook URL %s", err.Error())
		}
	}
	if len(b.Webhook.Secret) > 200 || strings.ContainsAny(b.Webhook.Secret, " \r\n") {
		return badRequest("webhook secret: at most 200 characters, no spaces")
	}
	if b.Webhook.Enabled && b.Webhook.URL == "" {
		return badRequest("set the webhook URL before enabling it")
	}
	if b.Telegram.BotToken != "" && !reTgToken.MatchString(b.Telegram.BotToken) {
		return badRequest("Telegram bot token must look like 123456789:AA… (from @BotFather)")
	}
	if b.Telegram.ChatID != "" && !reTgChat.MatchString(b.Telegram.ChatID) {
		return badRequest("Telegram chat id: a number (negative for groups) or @channelname")
	}
	if b.Telegram.Enabled && (b.Telegram.BotToken == "" || b.Telegram.ChatID == "") {
		return badRequest("set the Telegram bot token and chat id before enabling it")
	}
	if err := checkSecretURL(b.Ntfy.Server); err != nil {
		return badRequest("ntfy server %s", err.Error())
	}
	if b.Ntfy.Topic != "" && !reNtfyTop.MatchString(b.Ntfy.Topic) {
		return badRequest("ntfy topic: 1-64 letters, digits, '-' or '_'")
	}
	if len(b.Ntfy.Token) > 200 || strings.ContainsAny(b.Ntfy.Token, " \r\n") {
		return badRequest("ntfy token: at most 200 characters, no spaces")
	}
	if b.Ntfy.Enabled && b.Ntfy.Topic == "" {
		return badRequest("set the ntfy topic before enabling it")
	}
	var ign []string
	for _, s := range b.Ignore {
		if s = strings.TrimSpace(s); s != "" && len(ign) < 50 {
			ign = append(ign, truncRunes(s, 40))
		}
	}
	if ign == nil {
		ign = []string{}
	}
	b.Ignore = ign
	return nil
}

func apiTestForward(r *http.Request) (any, error) {
	target := r.PathValue("target")
	c := loadForward()
	if !c.enabled(target) {
		return nil, badRequest("switch %s forwarding on and save first", target)
	}
	m := rxSMS{from: "KStick (test)", ts: time.Now().Format(time.RFC3339), text: "Test message: SMS forwarding from your router is configured."}
	if err := fwdSend(c, target, "test", 0, m, time.Now().Format(time.RFC3339)); err != nil {
		return nil, badRequest("test failed: %s", err.Error())
	}
	audit(r, "SMS forwarding test sent to %s", target)
	return map[string]string{"message": "Test message sent to " + target + "."}, nil
}
