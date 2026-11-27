package main

// Every SMS sent or received is kept in SQLite (internal/smsdb), not on the SIM:
// received messages are copied into the database and then deleted from the
// modem; the database also drives forwarding (WhatsApp, webhook, Telegram, ntfy).
//
// New SMS are noticed through ModemManager's D-Bus "Added" signal (dbus-monitor), with a slow poll as a safety
// net; without dbus-monitor the modem is polled every 10 s.

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/caseypaite/kstick-router/internal/smscfg"
	"github.com/caseypaite/kstick-router/internal/smsdb"
	"github.com/caseypaite/kstick-router/internal/smssend"
)

const simImportMarker = "/var/lib/router/sms-sim-imported"

var (
	smsDBMu  sync.Mutex
	smsDBVal *smsdb.DB
)

// smsDB opens the shared database on first use (nil while unavailable).
func smsDB() *smsdb.DB {
	smsDBMu.Lock()
	defer smsDBMu.Unlock()
	if smsDBVal == nil {
		d, err := smsdb.Open(smsdb.Path)
		if err != nil {
			log.Printf("sms database: %v", err)
			return nil
		}
		smsDBVal = d
	}
	return smsDBVal
}

var (
	smsKick     = make(chan struct{}, 1)
	smsWatching atomic.Bool // dbus-monitor is running: poll the modem rarely
)

func kickSMS() {
	select {
	case smsKick <- struct{}{}:
	default:
	}
}

func smsStoreLoop() {
	if d := smsDB(); d != nil { // sends this program had in flight when it last stopped
		if n, err := d.SweepInterrupted("web-ui", "sms-control", "notify"); err == nil && n > 0 {
			log.Printf("sms store: %d message(s) were interrupted by the last restart (marked failed or unknown)", n)
		}
	}
	go smsWatch()
	t := time.NewTicker(10 * time.Second) // forwarding retries; the modem is read when due
	var lastIngest time.Time
	ingest := true
	for {
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("sms store: %v", r)
				}
			}()
			every := 10 * time.Second
			if smsWatching.Load() {
				every = 2 * time.Minute
			}
			if ingest || time.Since(lastIngest) >= every {
				ingestSMS()
				lastIngest, ingest = time.Now(), false
			}
			forwardPending()
			forwardOthers()
		}()
		select {
		case <-t.C:
		case <-waKick:
		case <-smsKick:
			ingest = true
		}
	}
}

// smsWatch follows ModemManager's Messaging.Added signal (new SMS, including status reports) and wakes the loop.
func smsWatch() {
	if _, err := exec.LookPath("dbus-monitor"); err != nil {
		log.Printf("sms store: dbus-monitor not found; polling the modem every 10 s")
		return
	}
	for {
		cmd := exec.Command("dbus-monitor", "--system",
			"type='signal',interface='org.freedesktop.ModemManager1.Modem.Messaging',member='Added'")
		out, err := cmd.StdoutPipe()
		if err == nil {
			err = cmd.Start()
		}
		if err != nil {
			log.Printf("sms store: dbus-monitor: %v", err)
			time.Sleep(time.Minute)
			continue
		}
		smsWatching.Store(true)
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			if strings.Contains(sc.Text(), "member=Added") {
				time.Sleep(time.Second) // let ModemManager finish reading the message
				kickSMS()
			}
		}
		cmd.Wait()
		smsWatching.Store(false)
		kickSMS() // catch up on anything missed while it was down
		time.Sleep(30 * time.Second)
	}
}

// Multi-part SMS whose remaining parts never arrive stay in state "receiving"; after this long they are stored as
// they are (marked incomplete) and removed from the modem.
const partialSMSTimeout = 6 * time.Hour

var (
	partialMu   sync.Mutex
	partialSeen = map[string]time.Time{} // modem SMS path -> first seen receiving
)

// partialExpired notes a still-receiving SMS and reports whether it has waited too long.
func partialExpired(path string, now time.Time) bool {
	partialMu.Lock()
	defer partialMu.Unlock()
	t, ok := partialSeen[path]
	if !ok {
		partialSeen[path] = now
		return false
	}
	return now.Sub(t) > partialSMSTimeout
}

func partialDone(path string) {
	partialMu.Lock()
	delete(partialSeen, path)
	partialMu.Unlock()
}

// ingestSMS stores received SMS in the database and removes them from the SIM/modem.
func ingestSMS() {
	d := smsDB()
	if d == nil {
		return
	}
	l := mmJSON("-m", "any", "--messaging-list-sms")
	if l == nil {
		return
	}
	paths, _ := l["modem.messaging.sms"].([]any)
	_, err := os.Stat(simImportMarker)
	firstImport := err != nil // messages already on the SIM when this feature started
	c := loadWawat()
	fc := loadForward()
	now := time.Now()
	for _, p := range paths {
		ps := str(p)
		s := mmJSON("-s", ps)
		if s == nil {
			continue
		}
		pdu, state := str(dig(s, "sms", "properties", "pdu-type")), str(dig(s, "sms", "properties", "state"))
		if pdu == "status-report" { // delivery report for an SMS we sent
			ref, err := strconv.Atoi(str(dig(s, "sms", "properties", "message-reference")))
			if err != nil {
				ref = -1
			}
			num, ds := str(dig(s, "sms", "content", "number")), str(dig(s, "sms", "properties", "delivery-state"))
			if ok, err := d.Delivery(num, ref, ds); err != nil {
				log.Printf("sms store: delivery report: %v (kept on the modem)", err)
				continue
			} else if !ok {
				log.Printf("sms store: delivery report for %s (reference %d, %s) matched no sent SMS", num, ref, ds)
			}
			run(20*time.Second, "mmcli", "-m", "any", "--messaging-delete-sms="+ps)
			continue
		}
		incomplete := false
		switch {
		case pdu != "deliver":
			continue // outgoing: deleted by its sender
		case state == "receiving":
			if !partialExpired(ps, now) {
				continue // multi-part still arriving
			}
			incomplete = true
		case state != "received":
			continue
		}
		partialDone(ps)
		m := rxSMS{path: ps, from: str(dig(s, "sms", "content", "number")), ts: str(dig(s, "sms", "properties", "timestamp")),
			text: str(dig(s, "sms", "content", "text")), smsc: str(dig(s, "sms", "properties", "smsc"))}
		if incomplete {
			m.text = strings.TrimSpace(m.text + "\n[incomplete: some parts of this multi-part SMS never arrived]")
		}
		m.hash = smsHash(m.from, m.ts, m.text)
		origin, wa, note := "modem", "pending", ""
		switch {
		case firstImport:
			origin, wa, note = "sim-import", "skipped", "was on the SIM before the SMS database existed"
		case !modules.WhatsApp:
			wa, note = "off", "WhatsApp forward module not installed"
		case !c.Enabled || c.Target == "":
			wa, note = "off", "WhatsApp forwarding was off when this SMS arrived"
		case ignoredSender(c, m.from):
			wa, note = "skipped", "sender matches the ignore list"
		}
		text := m.text
		targets := fc.enabledTargets(m.from)
		if firstImport {
			targets = nil
		}
		cmd, isCmd := ctlParse(m.from, m.text)
		isCmd = isCmd && !incomplete
		if isCmd && !firstImport { // SMS remote control: store with the PIN masked, never forward
			text, wa, note, targets = cmd.masked, "skipped", "SMS remote control command", nil
		}
		_, inserted, err := d.AddIn(m.hash, smsdb.In{Sender: m.from, Text: text, SMSC: m.smsc, SMSTime: m.ts, Origin: origin,
			WAStatus: wa, WANote: note, Ctl: isCmd && !firstImport}, targets)
		if err != nil {
			log.Printf("sms store: %v (kept on the modem)", err)
			continue // never delete what we could not save
		}
		if isCmd && inserted && !firstImport {
			ctlExecute(m.from, m.ts, cmd)
		}
		if _, err := run(20*time.Second, "mmcli", "-m", "any", "--messaging-delete-sms="+ps); err != nil {
			log.Printf("sms store: delete %s: %v", ps, err)
		}
		if inserted {
			kickWawat() // forward right away
		}
	}
	if firstImport {
		os.MkdirAll("/var/lib/router", 0700)
		os.WriteFile(simImportMarker, []byte(smsdb.Now()+"\n"), 0600)
	}
}

// ---------------------------------------------------------------- web UI API

func apiListSMS(r *http.Request) (any, error) {
	d := smsDB()
	if d == nil {
		return nil, fmt.Errorf("the SMS database is not available")
	}
	v := r.URL.Query()
	q := smsdb.Query{Search: v.Get("q"), Status: v.Get("status"), Key: v.Get("key"), Channel: v.Get("channel")}
	q.Limit, _ = strconv.Atoi(v.Get("limit"))
	q.Before, _ = strconv.ParseInt(v.Get("before"), 10, 64)
	st, err := d.Stats()
	if err != nil {
		return nil, err
	}
	res := map[string]any{"stats": st}
	if v.Get("dir") != "out" {
		if res["in"], err = d.ListIn(q); err != nil {
			return nil, err
		}
	}
	if v.Get("dir") != "in" {
		if res["out"], err = d.ListOut(q); err != nil {
			return nil, err
		}
	}
	return res, nil
}

func apiSendSMS(r *http.Request) (any, error) {
	var b struct{ Number, Text string }
	if err := readJSON(r, &b); err != nil {
		return nil, err
	}
	b.Number = strings.ReplaceAll(strings.TrimSpace(b.Number), " ", "")
	if smscfg.ValidNumber(b.Number) {
		b.Number = smscfg.E164(b.Number)
	}
	if !rePhone.MatchString(b.Number) {
		return nil, badRequest("invalid phone number")
	}
	text := smscfg.CleanMessage(b.Text)
	if strings.TrimSpace(text) == "" || utf8.RuneCountInString(text) > 480 {
		return nil, badRequest("message must be 1-480 characters")
	}
	d := smsDB()
	if d == nil {
		return nil, fmt.Errorf("the SMS database is not available; nothing was sent")
	}
	ref := fmt.Sprintf("ui%x", time.Now().UnixNano())
	if err := d.AddOut(smsdb.Out{Ref: ref, Recipient: b.Number, Text: text, Chars: utf8.RuneCountInString(text), Channel: "web-ui",
		ClientIP: clientIP(r), Delivery: "requested"}); err != nil {
		return nil, err
	}
	res, err := modemSend(b.Number, text, true)
	d.FinishOutRef(ref, err, res.MessageRef)
	if err != nil {
		return nil, err
	}
	audit(r, "SMS sent to %s", b.Number)
	return map[string]string{"message": "SMS sent."}, nil
}

// modemSendSMS sends one SMS through the shared send path (internal/smssend: text passed as a file, one send at a
// time across router-webui and the public gateway) and removes it from the modem afterwards.
func modemSendSMS(number, text string) error {
	_, err := modemSend(number, text, false)
	return err
}

func modemSend(number, text string, deliveryReport bool) (smssend.Result, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	return smssend.Send(ctx, number, text, smssend.Options{DeliveryReport: deliveryReport})
}

func apiReforwardSMS(r *http.Request) (any, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return nil, badRequest("invalid id")
	}
	d := smsDB()
	if d == nil {
		return nil, fmt.Errorf("the SMS database is not available")
	}
	if t := r.PathValue("target"); t != "" && t != "whatsapp" {
		if !loadForward().enabled(t) {
			return nil, badRequest("switch %s forwarding on first", t)
		}
		if err := d.RequeueFwd(id, t); err != nil {
			return nil, badRequest("%s", err.Error())
		}
		kickWawat()
		audit(r, "SMS %d queued for %s forwarding", id, t)
		return map[string]string{"message": "Queued for " + t + "."}, nil
	}
	if !modules.WhatsApp {
		return nil, badRequest("the WhatsApp forward module is not installed")
	}
	c := loadWawat()
	if !c.Enabled || c.Target == "" {
		return nil, badRequest("enable WhatsApp forwarding first")
	}
	if err := d.SetInStatus(id, "pending", "queued again from the web UI"); err != nil {
		return nil, badRequest("%s", err.Error())
	}
	kickWawat()
	audit(r, "SMS %d queued for WhatsApp forwarding", id)
	return map[string]string{"message": "Queued for WhatsApp."}, nil
}
