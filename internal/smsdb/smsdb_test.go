package smsdb

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/caseypaite/kstick-router/internal/smssend"
)

func TestDB(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "x", "sms.db"))
	if err != nil {
		t.Fatal(err)
	}
	in1 := In{Sender: "VM-BANK", Text: "OTP 1", SMSC: "+91", SMSTime: "2026-11-21T10:00:00+05:30", Origin: "modem", WAStatus: "pending"}
	if _, ok, _ := d.AddIn("h1", in1, []string{"webhook"}); !ok {
		t.Fatal("insert")
	}
	if _, ok, _ := d.AddIn("h1", in1, nil); ok {
		t.Fatal("duplicate inserted")
	}
	p, _ := d.PendingIn(10)
	if len(p) != 1 {
		t.Fatalf("pending %v", p)
	}
	d.MarkAttempt(p[0].ID, "91x@c.us", "503", false, time.Now().Add(-time.Second))
	d.MarkForwarded(p[0].ID, "91x@c.us", "mid")
	l, _ := d.ListIn(Query{})
	if l[0].WAStatus != "forwarded" || l[0].WAAttempts != 2 || l[0].WAForwardedAt == "" || l[0].WAMessageID != "mid" {
		t.Fatalf("in %+v", l[0])
	}
	d.AddOut(Out{Ref: "r1", Recipient: "+919876543210", Text: "hi", Chars: 2, Channel: "api", APIKey: "app"})
	d.AddOut(Out{Ref: "r2", Recipient: "+919876543211", Text: "yo", Chars: 2, Channel: "api", APIKey: "other"})
	d.FinishOut("r1", nil)
	d.FinishOut("r2", errors.New("boom"))
	o, _ := d.ListOut(Query{Key: "app"})
	if len(o) != 1 || o[0].Status != "sent" || o[0].SentAt == "" {
		t.Fatalf("out %+v", o)
	}
	m, total, _ := d.APIUsageSince(DayStart(timeNow()))
	if total != 2 || m["app"] != 1 {
		t.Fatalf("usage %v %d", m, total)
	}
	s, err := d.Stats()
	if err != nil || s.InTotal != 1 || s.OutFailedDay != 1 || s.WAByStatus["forwarded"] != 1 {
		t.Fatalf("stats %+v %v", s, err)
	}
	if o, _ := d.ListOut(Query{Search: "boom"}); len(o) != 0 {
		t.Fatal("search should not match error column")
	}
}

func TestNewFeatures(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "sms.db"))
	if err != nil {
		t.Fatal(err)
	}
	// backoff: a failed WhatsApp attempt is not retried before its time
	id, _, _ := d.AddIn("a", In{Sender: "X", Text: "one", Origin: "modem", WAStatus: "pending"}, []string{"webhook", "ntfy"})
	d.MarkAttempt(id, "t", "503", false, time.Now().Add(time.Hour))
	if p, _ := d.PendingIn(10); len(p) != 0 {
		t.Fatalf("retried too early: %v", p)
	}
	// remote-control commands never reach the inbox; the inbox pages by id
	d.AddIn("b", In{Sender: "Y", Text: "•••• STATUS", Origin: "modem", WAStatus: "skipped", Ctl: true}, nil)
	id3, _, _ := d.AddIn("c", In{Sender: "Z", Text: "three", Origin: "modem", WAStatus: "off"}, nil)
	if l, _ := d.Inbox(0, "", 10); len(l) != 2 || l[0].ID != id || l[1].ID != id3 {
		t.Fatalf("inbox %+v", l)
	}
	if l, _ := d.Inbox(id, "", 10); len(l) != 1 || l[0].ID != id3 {
		t.Fatalf("inbox after %d: %+v", id, l)
	}
	// other forwarding targets
	ins, fs, _ := d.PendingFwd("webhook", 10)
	if len(ins) != 1 || ins[0].ID != id || fs[0].Attempts != 0 {
		t.Fatalf("pending webhook %v %v", ins, fs)
	}
	d.MarkFwd(id, "webhook", false, "HTTP 500", false, time.Now().Add(time.Hour))
	if ins, _, _ := d.PendingFwd("webhook", 10); len(ins) != 0 {
		t.Fatal("webhook retried too early")
	}
	d.MarkFwd(id, "ntfy", true, "", false, time.Time{})
	l, _ := d.ListIn(Query{})
	var got []Fwd
	for _, m := range l {
		if m.ID == id {
			got = m.Forwards
		}
	}
	if len(got) != 2 || got[0].Target != "ntfy" || got[0].Status != "forwarded" || got[1].Status != "pending" || got[1].Attempts != 1 {
		t.Fatalf("forwards %+v", got)
	}
	if err := d.RequeueFwd(id3, "telegram"); err != nil {
		t.Fatal(err)
	}
	if ins, _, _ := d.PendingFwd("telegram", 10); len(ins) != 1 {
		t.Fatal("requeue")
	}
	// idempotency keys are per API key
	o := Out{Ref: "r1", Recipient: "+919876543210", Text: "hi", Chars: 2, Channel: "api", APIKey: "app", IdemKey: "k1", Status: "queued", Delivery: "requested"}
	if err := d.AddOut(o); err != nil {
		t.Fatal(err)
	}
	o.Ref = "r2"
	if err := d.AddOut(o); err != ErrDuplicate {
		t.Fatalf("duplicate idempotency key: %v", err)
	}
	o.APIKey = "other"
	if err := d.AddOut(o); err != nil {
		t.Fatalf("same idempotency key, other API key: %v", err)
	}
	if g, ok, _ := d.ByIdemKey("app", "k1"); !ok || g.Ref != "r1" {
		t.Fatalf("by key %+v", g)
	}
	// delivery reports match on message reference + recipient
	d.SetOutStatus("r1", "queued", "sending")
	d.FinishOutRef("r1", nil, 42)
	if ok, _ := d.Delivery("+919876543210", 41, "completed-received"); ok {
		t.Fatal("wrong reference matched")
	}
	if ok, _ := d.Delivery("919876543210", 42, "completed-received"); !ok {
		t.Fatal("report not matched")
	}
	if g, _, _ := d.GetOut("r1"); g.Status != "sent" || g.Delivery != "delivered" || g.DeliveryAt == "" {
		t.Fatalf("delivery %+v", g)
	}
	// startup sweep and the "unknown" status
	d.AddOut(Out{Ref: "q", Recipient: "+91", Text: "x", Channel: "api", Status: "queued"})
	d.AddOut(Out{Ref: "s", Recipient: "+91", Text: "x", Channel: "api", Status: "sending"})
	d.AddOut(Out{Ref: "w", Recipient: "+91", Text: "x", Channel: "web-ui", Status: "sending"})
	if n, _ := d.SweepInterrupted("api"); n != 3 { // q, s and "r2" of key "other" (queued)
		t.Fatalf("swept %d", n)
	}
	for ref, want := range map[string]string{"q": "failed", "s": "unknown", "w": "sending"} {
		if g, _, _ := d.GetOut(ref); g.Status != want {
			t.Fatalf("%s: %s, want %s", ref, g.Status, want)
		}
	}
	d.FinishOut("w", fmt.Errorf("x: %w", smssend.ErrUnknown))
	if g, _, _ := d.GetOut("w"); g.Status != "unknown" {
		t.Fatalf("timeout should be unknown: %+v", g)
	}
}

func TestNextRetry(t *testing.T) {
	now := time.Now()
	for n, want := range map[int]time.Duration{1: 30 * time.Second, 2: time.Minute, 3: 2 * time.Minute, 9: time.Hour, 50: time.Hour} {
		if got := NextRetry(n, now).Sub(now); got != want {
			t.Errorf("attempt %d: %s, want %s", n, got, want)
		}
	}
	total := time.Duration(0)
	for n := 1; n < MaxAttempts; n++ {
		total += NextRetry(n, now).Sub(now)
	}
	if total < 20*time.Hour || total > 28*time.Hour {
		t.Errorf("all retries span %s, want about a day", total)
	}
}

var timeNow = func() time.Time { return time.Now() }

// A database created by the previous release (no ctl/idem_key/msg_ref/... columns) is upgraded on Open.
func TestMigrateOldDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	old, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = old.Exec(`CREATE TABLE sms_in (id INTEGER PRIMARY KEY, hash TEXT NOT NULL UNIQUE, sender TEXT NOT NULL, text TEXT NOT NULL,
  smsc TEXT NOT NULL DEFAULT '', sms_time TEXT NOT NULL DEFAULT '', stored_at TEXT NOT NULL, origin TEXT NOT NULL, wa_status TEXT NOT NULL,
  wa_attempts INTEGER NOT NULL DEFAULT 0, wa_target TEXT NOT NULL DEFAULT '', wa_forwarded_at TEXT NOT NULL DEFAULT '',
  wa_message_id TEXT NOT NULL DEFAULT '', wa_note TEXT NOT NULL DEFAULT '');
CREATE TABLE sms_out (id INTEGER PRIMARY KEY, ref TEXT NOT NULL UNIQUE, created_at TEXT NOT NULL, sent_at TEXT NOT NULL DEFAULT '',
  recipient TEXT NOT NULL, text TEXT NOT NULL, chars INTEGER NOT NULL, channel TEXT NOT NULL, api_key TEXT NOT NULL DEFAULT '',
  client_ip TEXT NOT NULL DEFAULT '', message_source TEXT NOT NULL DEFAULT '', status TEXT NOT NULL, error TEXT NOT NULL DEFAULT '');
INSERT INTO sms_in(hash,sender,text,stored_at,origin,wa_status) VALUES('h','A','old','2026-11-01T00:00:00Z','modem','forwarded');
INSERT INTO sms_in(hash,sender,text,stored_at,origin,wa_status,wa_note) VALUES('c','+91','•••• STATUS','2026-11-01T00:00:00Z','modem','skipped','SMS remote control command');
INSERT INTO sms_out(ref,created_at,recipient,text,chars,channel,status) VALUES('r','2026-11-01T00:00:00Z','+91','old',3,'api','sent');`)
	old.Close()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ { // and opening again is a no-op
		d, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		if l, err := d.Inbox(0, "", 10); err != nil || len(l) != 1 || l[0].Text != "old" {
			t.Fatalf("inbox %v %v", l, err)
		}
		if o, ok, err := d.GetOut("r"); err != nil || !ok || o.MsgRef != -1 || o.Delivery != "" {
			t.Fatalf("out %+v %v", o, err)
		}
		d.Close()
	}
}
