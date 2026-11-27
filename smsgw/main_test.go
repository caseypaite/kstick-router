package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caseypaite/kstick-router/internal/smscfg"
	"github.com/caseypaite/kstick-router/internal/smsdb"
)

const (
	testKey   = "ksms_testkey_0123456789abcdefghijklmnopqrstuvwxyz"
	readerKey = "ksms_reader_0123456789abcdefghijklmnopqrstuvwxyz"
)

// fakeModem puts a fake mmcli in PATH that records the SMS number and text.
func fakeModem(t *testing.T, state string, sendFails bool) string {
	return fakeModemSlow(t, state, sendFails, "")
}

func fakeModemSlow(t *testing.T, state string, sendFails bool, sendDelay string) string {
	dir := t.TempDir()
	rec := filepath.Join(dir, "rec")
	fail := "true"
	if sendFails {
		fail = "echo 'error: send failed' >&2; exit 1"
	}
	script := `#!/bin/sh
case "$*" in
  "-s "*" -J") echo '{"sms":{"properties":{"message-reference":"12"}}}';;
  *"-J"*) echo '{"modem":{"generic":{"state":"` + state + `","signal-quality":{"value":"80"}},"3gpp":{"operator-name":"Test"}}}';;
  *--messaging-create-sms-with-text=*)
    for a in "$@"; do case "$a" in
      --messaging-create-sms=*) echo "$a" > ` + rec + `.num;;
      --messaging-create-sms-with-text=*) cat "${a#*=}" > ` + rec + `.txt;;
    esac; done
    echo "Successfully created new SMS: /org/freedesktop/ModemManager1/SMS/7 (unknown)";;
  *--send*) ` + func() string {
		if sendDelay != "" {
			return "sleep " + sendDelay + "; "
		}
		return ""
	}() + fail + `; echo sent >> ` + rec + `.sent;;
  *) ;;
esac
`
	os.WriteFile(filepath.Join(dir, "mmcli"), []byte(script), 0755)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	return rec
}

var workerOnce sync.Once

func setup(t *testing.T, c smscfg.Config) {
	workerOnce.Do(func() { go sendWorker() })
	*logPath = filepath.Join(t.TempDir(), "sent.jsonl")
	d, err := smsdb.Open(filepath.Join(t.TempDir(), "sms.db"))
	if err != nil {
		t.Fatal(err)
	}
	db = d
	t.Cleanup(func() { d.Close() })
	c.Keys = []smscfg.Key{{Name: "app", Hash: smscfg.HashKey(testKey), Prefix: testKey[:9]},
		{Name: "reader", Hash: smscfg.HashKey(readerKey), Prefix: readerKey[:9], Scopes: []string{"inbox"}}}
	cfgMu.Lock()
	cfg = c
	cfgMu.Unlock()
	rlMu.Lock()
	keyMinute, numHour, keyDay, globalDay = map[string][]time.Time{}, map[string][]time.Time{}, map[string]int{}, map[string]int{}
	rlMu.Unlock()
	failMu.Lock()
	fails = map[string]*failRec{}
	failMu.Unlock()
}

func post(t *testing.T, key, body string) (int, map[string]any) { return postH(t, key, body, "", "") }

func postH(t *testing.T, key, body, idem, query string) (int, map[string]any) {
	req := httptest.NewRequest("POST", "/api/v1/sms"+query, strings.NewReader(body))
	if idem != "" {
		req.Header.Set("Idempotency-Key", idem)
	}
	req.RemoteAddr = "127.0.0.1:5555"
	req.Header.Set("CF-Connecting-IP", "203.0.113.9")
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	w := httptest.NewRecorder()
	handleSend(w, req)
	var m map[string]any
	json.Unmarshal(w.Body.Bytes(), &m)
	return w.Code, m
}

func enabled() smscfg.Config {
	c := smscfg.Default()
	c.Enabled = true
	c.DefaultMessage = "Default hello"
	return c
}

func TestSendCustomMessage(t *testing.T) {
	rec := fakeModem(t, "connected", false)
	setup(t, enabled())
	msg := `Hi 'there' "x" $(reboot); ठीक है ✓`
	code, m := post(t, testKey, `{"to":"9876543210","message":`+jsonStr(msg)+`}`)
	if code != 200 || m["ok"] != true || m["to"] != "+919876543210" || m["message_source"] != "request" {
		t.Fatalf("got %d %v", code, m)
	}
	num, _ := os.ReadFile(rec + ".num")
	txt, _ := os.ReadFile(rec + ".txt")
	if strings.TrimSpace(string(num)) != "--messaging-create-sms=number='+919876543210',delivery-report-request=yes" || string(txt) != msg {
		t.Fatalf("modem got number %q text %q", num, txt)
	}
	if l, _ := db.ListOut(smsdb.Query{}); len(l) != 1 || l[0].Status != "sent" || l[0].ClientIP != "203.0.113.9" || l[0].APIKey != "app" || l[0].Text != msg || l[0].SentAt == "" {
		t.Fatalf("log %+v", l)
	}
}

func TestSendDefaultMessage(t *testing.T) {
	rec := fakeModem(t, "registered", false)
	setup(t, enabled())
	code, m := post(t, testKey, `{"to":"6000000001"}`)
	txt, _ := os.ReadFile(rec + ".txt")
	if code != 200 || m["message_source"] != "default" || string(txt) != "Default hello" {
		t.Fatalf("got %d %v text %q", code, m, txt)
	}
}

func TestValidation(t *testing.T) {
	fakeModem(t, "connected", false)
	c := enabled()
	c.MaxLength = 10
	setup(t, c)
	for _, n := range []string{"98765", "+919876543210", "09876543210", "5876543210", "98765432a0", "98765 43210", "919876543210", ""} {
		if code, _ := post(t, testKey, `{"to":"`+n+`","message":"x"}`); code != 400 {
			t.Errorf("number %q: got %d, want 400", n, code)
		}
	}
	if code, _ := post(t, testKey, `{"to":"9876543210","message":"12345678901"}`); code != 400 {
		t.Errorf("too long: got %d", code)
	}
	if code, _ := post(t, testKey, `{"to":"9876543210","extra":1}`); code != 400 {
		t.Errorf("unknown field: got %d", code)
	}
	c.DefaultMessage = ""
	setup(t, c)
	if code, _ := post(t, testKey, `{"to":"9876543210"}`); code != 400 {
		t.Errorf("no message, no default: got %d", code)
	}
}

func TestAuthAndLockout(t *testing.T) {
	fakeModem(t, "connected", false)
	setup(t, enabled())
	if code, _ := post(t, "", `{"to":"9876543210"}`); code != 401 {
		t.Fatalf("no key: %d", code)
	}
	for i := 0; i < failMax; i++ {
		post(t, "ksms_wrong", `{"to":"9876543210"}`)
	}
	if code, _ := post(t, testKey, `{"to":"9876543210"}`); code != 429 {
		t.Fatalf("after %d bad keys even a good key should be blocked for that IP, got %d", failMax, code)
	}
}

func TestDisabledAndModemDown(t *testing.T) {
	fakeModem(t, "searching", false)
	c := enabled()
	c.Enabled = false
	setup(t, c)
	if code, _ := post(t, testKey, `{"to":"9876543210"}`); code != 503 {
		t.Fatalf("disabled: %d", code)
	}
	setup(t, enabled())
	if code, m := post(t, testKey, `{"to":"9876543210"}`); code != 503 || !strings.Contains(m["error"].(string), "not registered") {
		t.Fatalf("modem searching: %d %v", code, m)
	}
}

func TestSendFailureLogged(t *testing.T) {
	fakeModem(t, "connected", true)
	setup(t, enabled())
	if code, _ := post(t, testKey, `{"to":"9876543210"}`); code != 503 {
		t.Fatalf("send failure: %d", code)
	}
	if l, _ := db.ListOut(smsdb.Query{}); len(l) != 1 || l[0].Status != "failed" || l[0].Error == "" {
		t.Fatalf("log %+v", l)
	}
}

func TestRateLimits(t *testing.T) {
	fakeModem(t, "connected", false)
	c := enabled()
	c.Limits = smscfg.Limits{PerKeyPerMinute: 100, PerKeyPerDay: 100, PerNumberPerHour: 2, GlobalPerDay: 3}
	setup(t, c)
	codes := []int{}
	for _, n := range []string{"9876543210", "9876543210", "9876543210", "9876543211", "9876543212"} {
		code, _ := post(t, testKey, `{"to":"`+n+`"}`)
		codes = append(codes, code)
	}
	// 2 per number per hour, then global cap of 3 per day
	want := []int{200, 200, 429, 200, 429}
	for i := range want {
		if codes[i] != want[i] {
			t.Fatalf("codes %v, want %v", codes, want)
		}
	}
}

func TestFormBody(t *testing.T) {
	rec := fakeModem(t, "connected", false)
	setup(t, enabled())
	req := httptest.NewRequest("POST", "/api/v1/sms", strings.NewReader("to=9876543210&message=hello+form"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-API-Key", testKey)
	w := httptest.NewRecorder()
	handleSend(w, req)
	txt, _ := os.ReadFile(rec + ".txt")
	if w.Code != 200 || string(txt) != "hello form" {
		t.Fatalf("form: %d %s %q", w.Code, w.Body, txt)
	}
}

func jsonStr(s string) string { b, _ := json.Marshal(s); return string(b) }

var _ = http.StatusOK

func TestRouter(t *testing.T) {
	fakeModem(t, "connected", false)
	setup(t, enabled())
	h := newHandler()
	for _, c := range []struct {
		method, path string
		want         int
	}{{"GET", "/", 200}, {"GET", "/app.js", 200}, {"GET", "/api/nope", 404}, {"POST", "/", 405}, {"GET", "/api/v1/sms", 404}, {"POST", "/api/v1/sms", 401}, {"GET", "/api/v1/status", 401},
		{"GET", "/api/v1/sms/abc", 401}, {"GET", "/api/v1/inbox", 401}} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(c.method, c.path, nil))
		if w.Code != c.want {
			t.Errorf("%s %s = %d, want %d", c.method, c.path, w.Code, c.want)
		}
	}
}

func TestLegacyImportAndCounters(t *testing.T) {
	setup(t, enabled())
	now := time.Now().Format(time.RFC3339)
	os.WriteFile(*logPath, []byte(`{"time":"`+now+`","id":"aa11","key":"app","ip":"1.2.3.4","to":"+919876543210","source":"request","chars":2,"text":"hi","status":"sent"}`+"\n"), 0600)
	importLegacyLog()
	loadCounters()
	if l, _ := db.ListOut(smsdb.Query{}); len(l) != 1 || l[0].Ref != "aa11" || l[0].APIKey != "app" {
		t.Fatalf("import %+v", l)
	}
	if k, g := usage("app"); k != 1 || g != 1 {
		t.Fatalf("counters %d %d", k, g)
	}
	if _, err := os.Stat(*logPath); err == nil {
		t.Fatal("legacy log not renamed")
	}
}

func sends(rec string) int {
	b, _ := os.ReadFile(rec + ".sent")
	return strings.Count(string(b), "sent")
}

func get(t *testing.T, key, path string) (int, map[string]any) {
	req := httptest.NewRequest("GET", path, nil)
	req.RemoteAddr = "127.0.0.1:5555"
	req.Header.Set("Authorization", "Bearer "+key)
	w := httptest.NewRecorder()
	newHandler().ServeHTTP(w, req)
	var m map[string]any
	json.Unmarshal(w.Body.Bytes(), &m)
	return w.Code, m
}

func TestIdempotency(t *testing.T) {
	rec := fakeModem(t, "connected", false)
	setup(t, enabled())
	code, m := postH(t, testKey, `{"to":"9876543210","message":"otp 1"}`, "order-1", "")
	if code != 200 || m["status"] != "sent" {
		t.Fatalf("first: %d %v", code, m)
	}
	code, m2 := postH(t, testKey, `{"to":"9876543210","message":"otp 1"}`, "order-1", "")
	if code != 200 || m2["id"] != m["id"] || m2["idempotent_replay"] != true || sends(rec) != 1 {
		t.Fatalf("replay: %d %v, %d sends", code, m2, sends(rec))
	}
	if code, _ := postH(t, testKey, `{"to":"9876543210","message":"otp 2"}`, "order-1", ""); code != 422 {
		t.Fatalf("same key, other message: %d", code)
	}
	if code, _ := postH(t, testKey, `{"to":"9876543210","message":"otp 1"}`, "bad key with spaces", ""); code != 400 {
		t.Fatalf("invalid key: %d", code)
	}
	if _, k, _ := usageOf("app"); k != 1 {
		t.Fatalf("a replay must not count against the limits (%d)", k)
	}
}

func usageOf(k string) (int, int, int) { a, b := usage(k); return 0, a, b }

func TestFailedThenRetrySameKey(t *testing.T) {
	fakeModem(t, "searching", false)
	setup(t, enabled())
	if code, m := postH(t, testKey, `{"to":"9876543210"}`, "k2", ""); code != 503 || m["status"] != "failed" {
		t.Fatalf("modem down: %d %v", code, m)
	}
	rec := fakeModem(t, "connected", false)
	if code, m := postH(t, testKey, `{"to":"9876543210"}`, "k2", ""); code != 200 || m["status"] != "sent" || sends(rec) != 1 {
		t.Fatalf("retry after a definite failure should send: %d %v", code, m)
	}
	if l, _ := db.ListOut(smsdb.Query{}); len(l) != 1 {
		t.Fatalf("one row expected: %+v", l)
	}
}

func TestAcceptedThenPoll(t *testing.T) {
	rec := fakeModemSlow(t, "connected", false, "1")
	setup(t, enabled())
	code, m := postH(t, testKey, `{"to":"9876543210"}`, "", "?wait=0")
	if code != 202 || (m["status"] != "queued" && m["status"] != "sending") || m["status_url"] == nil {
		t.Fatalf("wait=0: %d %v", code, m)
	}
	id := m["id"].(string)
	for i := 0; i < 50 && sends(rec) == 0; i++ {
		time.Sleep(100 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	code, m = get(t, testKey, "/api/v1/sms/"+id)
	if code != 200 || m["status"] != "sent" || m["delivery_report"].(map[string]any)["status"] != "requested" {
		t.Fatalf("poll: %d %v", code, m)
	}
	if code, _ := get(t, readerKey, "/api/v1/sms/"+id); code != 403 {
		t.Fatalf("an inbox-only key cannot read sends: %d", code)
	}
}

func TestInbox(t *testing.T) {
	setup(t, enabled())
	db.AddIn("a", smsdb.In{Sender: "VM-BANK", Text: "OTP 123456", Origin: "modem", WAStatus: "off"}, nil)
	db.AddIn("b", smsdb.In{Sender: "+919000000001", Text: "•••• STATUS", Origin: "modem", WAStatus: "skipped", Ctl: true}, nil)
	db.AddIn("c", smsdb.In{Sender: "AD-SHOP", Text: "sale", Origin: "modem", WAStatus: "off"}, nil)
	if code, _ := get(t, testKey, "/api/v1/inbox"); code != 403 {
		t.Fatalf("send-only key: %d", code)
	}
	code, m := get(t, readerKey, "/api/v1/inbox?limit=1")
	l, _ := m["messages"].([]any)
	if code != 200 || len(l) != 1 || l[0].(map[string]any)["text"] != "OTP 123456" || m["more"] != true {
		t.Fatalf("inbox: %d %v", code, m)
	}
	next := m["next_after_id"].(float64)
	_, m = get(t, readerKey, "/api/v1/inbox?after_id="+strconv.Itoa(int(next)))
	l, _ = m["messages"].([]any)
	if len(l) != 1 || l[0].(map[string]any)["from"] != "AD-SHOP" {
		t.Fatalf("next page should skip the remote-control command: %v", m)
	}
	for _, q := range []string{"?limit=0", "?after_id=x", "?since=yesterday"} {
		if code, _ := get(t, readerKey, "/api/v1/inbox"+q); code != 400 {
			t.Errorf("%s: %d", q, code)
		}
	}
}
