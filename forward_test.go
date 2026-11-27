package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"kstick-router/internal/smsdb"
)

func TestCheckSecretURL(t *testing.T) {
	for u, ok := range map[string]bool{
		"https://wa.example.com":          true,
		"https://hooks.example.com/a?b=c": true,
		"http://192.168.4.20:3000/hook":   true,
		"http://100.96.0.5/x":             true, // Mesh
		"http://127.0.0.1:8080":           true,
		"http://wa.example.com":           false, // token in clear text over the internet
		"http://8.8.8.8/x":                false,
		"ftp://x.example.com":             false,
		"https://user:pw@example.com":     false,
		"":                                false,
	} {
		if err := checkSecretURL(u); (err == nil) != ok {
			t.Errorf("%q: %v", u, err)
		}
	}
}

func TestRedactToken(t *testing.T) {
	s := redactToken(`Post "https://api.telegram.org/bot123456:AAAbbbCCCdddEEEfffGGGhhhIIIjjjKKK/sendMessage": dial tcp`)
	if strings.Contains(s, "AAAbbb") {
		t.Fatal(s)
	}
	if p := redactPath("/bot123456:AAAbbb/sendMessage"); p != "/bot…/sendMessage" {
		t.Fatal(p)
	}
}

// useTestForwarding points the forward config and the SMS database at temporary files.
func useTestForwarding(t *testing.T, c ForwardCfg) *smsdb.DB {
	dir := t.TempDir()
	forwardPath = filepath.Join(dir, "forward.json")
	if err := saveForward(c); err != nil {
		t.Fatal(err)
	}
	d, err := smsdb.Open(filepath.Join(dir, "sms.db"))
	if err != nil {
		t.Fatal(err)
	}
	smsDBMu.Lock()
	old := smsDBVal
	smsDBVal = d
	smsDBMu.Unlock()
	t.Cleanup(func() {
		smsDBMu.Lock()
		smsDBVal = old
		smsDBMu.Unlock()
		d.Close()
		forwardPath = "/etc/router/forward.json"
	})
	return d
}

func TestForwardTargets(t *testing.T) {
	var mu sync.Mutex
	got := map[string]*http.Request{}
	bodies := map[string]string{}
	fail := map[string]int{} // status to answer with
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		key := "webhook"
		switch {
		case strings.Contains(r.URL.Path, "/sendMessage"):
			key = "telegram"
		case strings.HasPrefix(r.URL.Path, "/topic"):
			key = "ntfy"
		}
		mu.Lock()
		got[key], bodies[key] = r, string(b)
		code := fail[key]
		mu.Unlock()
		if code != 0 {
			w.WriteHeader(code)
			return
		}
		if key == "telegram" {
			w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer srv.Close()
	old := telegramAPI
	telegramAPI = srv.URL
	defer func() { telegramAPI = old }()

	c := ForwardCfg{
		Webhook:  WebhookCfg{Enabled: true, URL: srv.URL + "/hook", Secret: "s3cret"},
		Telegram: TelegramCfg{Enabled: true, BotToken: "123456:AAAbbbCCCdddEEEfffGGGhhhIIIjjjKKK", ChatID: "-100123"},
		Ntfy:     NtfyCfg{Enabled: true, Server: srv.URL, Topic: "topic1", Token: "tk"},
		Ignore:   []string{"ad-"},
	}
	d := useTestForwarding(t, c)
	if got := c.enabledTargets("AD-SHOP"); len(got) != 0 {
		t.Fatalf("ignored sender queued for %v", got)
	}
	id, _, _ := d.AddIn("h", smsdb.In{Sender: "VM-BANK", Text: "OTP 123456 ✓", SMSTime: "2026-11-26T10:00:00+05:30", Origin: "modem", WAStatus: "off"},
		c.enabledTargets("VM-BANK"))
	fail["telegram"] = 401 // a bad token is permanent: give up at once
	fail["ntfy"] = 503     // temporary: retry later
	forwardOthers()

	wh := got["webhook"]
	if wh == nil {
		t.Fatal("webhook not called")
	}
	var p map[string]any
	json.Unmarshal([]byte(bodies["webhook"]), &p)
	if p["event"] != "sms.received" || p["text"] != "OTP 123456 ✓" || p["from"] != "VM-BANK" || p["id"] != float64(id) {
		t.Fatalf("webhook body %v", p)
	}
	want := "sha256=" + webhookSignature("s3cret", wh.Header.Get("X-KStick-Timestamp"), []byte(bodies["webhook"]))
	if wh.Header.Get("X-KStick-Signature") != want {
		t.Fatalf("signature %q, want %q", wh.Header.Get("X-KStick-Signature"), want)
	}
	if n := got["ntfy"]; n == nil || n.Header.Get("Authorization") != "Bearer tk" || n.Header.Get("Title") != "SMS from VM-BANK" ||
		!strings.Contains(bodies["ntfy"], "OTP 123456") {
		t.Fatalf("ntfy request %v %q", n, bodies["ntfy"])
	}
	if !strings.Contains(bodies["telegram"], `"chat_id":"-100123"`) {
		t.Fatalf("telegram body %q", bodies["telegram"])
	}
	l, _ := d.ListIn(smsdb.Query{})
	st := map[string]smsdb.Fwd{}
	for _, f := range l[0].Forwards {
		st[f.Target] = f
	}
	if st["webhook"].Status != "forwarded" || st["telegram"].Status != "failed" || st["ntfy"].Status != "pending" || st["ntfy"].NextAt == "" {
		t.Fatalf("forward states %+v", st)
	}
	for _, e := range fwdLogs["telegram"] {
		if strings.Contains(e.Path, "AAAbbb") {
			t.Fatalf("bot token in the log: %q", e.Path)
		}
	}
}

func TestValidateForward(t *testing.T) {
	ok := ForwardCfg{Ntfy: NtfyCfg{Server: "https://ntfy.sh"}}
	if err := validateForward(&ok); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]ForwardCfg{
		"webhook on without URL": {Webhook: WebhookCfg{Enabled: true}, Ntfy: NtfyCfg{Server: "https://ntfy.sh"}},
		"plain http webhook":     {Webhook: WebhookCfg{URL: "http://hooks.example.com"}, Ntfy: NtfyCfg{Server: "https://ntfy.sh"}},
		"bad bot token":          {Telegram: TelegramCfg{BotToken: "nope"}, Ntfy: NtfyCfg{Server: "https://ntfy.sh"}},
		"telegram on, no chat":   {Telegram: TelegramCfg{Enabled: true, BotToken: "123456:AAAbbbCCCdddEEEfffGGGhhhIIIjjjKKK"}, Ntfy: NtfyCfg{Server: "https://ntfy.sh"}},
		"bad ntfy topic":         {Ntfy: NtfyCfg{Server: "https://ntfy.sh", Topic: "a/b"}},
		"ntfy on, no topic":      {Ntfy: NtfyCfg{Enabled: true, Server: "https://ntfy.sh"}},
	} {
		if err := validateForward(&c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
