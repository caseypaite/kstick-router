// kstick-smsgw: public SMS gateway. Listens on 127.0.0.1 only; Cloudflare Tunnel
// (cloudflared) publishes it. Sends through the LTE modem with ModemManager.
//
//	POST /api/v1/sms       {"to":"9876543210","message":"optional"}  Authorization: Bearer <key>
//	                       optional Idempotency-Key header; ?wait=0-90 seconds (default 60) before answering 202
//	GET  /api/v1/sms/{id}  status and delivery report of a message this key sent
//	GET  /api/v1/inbox     received SMS (?after_id=N, ?since=RFC3339, ?limit=1-100); keys with the "inbox" scope only
//	GET  /api/v1/status    Authorization: Bearer <key>
//	GET  /                 documentation + test form
//
// Sends go through a queue served by one worker, so a slow modem never holds an HTTP request past Cloudflare's
// 100 s limit: the request waits up to ?wait seconds and otherwise answers 202 with the message id to poll.
package main

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"kstick-router/internal/smscfg"
	"kstick-router/internal/smsdb"
	"kstick-router/internal/smssend"
)

//go:embed web
var webFS embed.FS

var (
	cfgPath = flag.String("config", smscfg.Path, "configuration file (managed by the router web UI)")
	dbPath  = flag.String("db", smsdb.Path, "SQLite SMS log (shared with the router web UI)")
	logPath = flag.String("log", smscfg.LogPath, "legacy JSON-lines send log, imported into the database once")
	db      *smsdb.DB
	listen  = flag.String("listen", "127.0.0.1:8080", "listen address")

	cfgMu     sync.RWMutex
	cfg       = smscfg.Default()
	cfgTime   time.Time
	cfgLoaded bool
)

func main() {
	flag.Parse()
	log.SetFlags(0)
	os.Setenv("PATH", "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	reloadConfig()
	go func() {
		for range time.Tick(3 * time.Second) {
			reloadConfig()
		}
	}()
	var err error
	if db, err = smsdb.Open(*dbPath); err != nil {
		log.Fatalf("database: %v", err)
	}
	importLegacyLog()
	if n, err := db.SweepInterrupted("api"); err != nil {
		log.Printf("database: %v", err)
	} else if n > 0 {
		log.Printf("%d API message(s) were interrupted by the last restart (marked failed or unknown)", n)
	}
	loadCounters()
	go sendWorker()

	srv := &http.Server{
		Addr: *listen, Handler: newHandler(),
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 20 * time.Second,
		WriteTimeout: 150 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10,
	}
	log.Printf("kstick-smsgw listening on %s", *listen)
	log.Fatal(srv.ListenAndServe())
}

func newHandler() http.Handler {
	mux := http.NewServeMux()
	sub, _ := fs.Sub(webFS, "web")
	static := http.FileServerFS(sub)
	mux.HandleFunc("POST /api/v1/sms", handleSend)
	mux.HandleFunc("GET /api/v1/sms/{id}", handleGetSMS)
	mux.HandleFunc("GET /api/v1/inbox", handleInbox)
	mux.HandleFunc("GET /api/v1/status", handleStatus)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			jsonErr(w, 404, "not found")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			jsonErr(w, 405, "method not allowed")
			return
		}
		static.ServeHTTP(w, r)
	})
	return secure(mux)
}

func reloadConfig() {
	st, err := os.Stat(*cfgPath)
	var mt time.Time
	if err == nil {
		mt = st.ModTime()
	}
	if cfgLoaded && mt.Equal(cfgTime) { // unchanged (or still missing)
		return
	}
	cfgLoaded, cfgTime = true, mt
	c, err := smscfg.Load(*cfgPath)
	if err != nil {
		log.Printf("config: %v (keeping previous settings)", err)
		return
	}
	cfgMu.Lock()
	cfg = c
	cfgMu.Unlock()
	log.Printf("config loaded: enabled=%v keys=%d", c.Enabled, len(c.Keys))
}

func current() smscfg.Config {
	cfgMu.RLock()
	defer cfgMu.RUnlock()
	return cfg
}

func secure(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; script-src 'self'; connect-src 'self'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("X-Frame-Options", "DENY")
		hd.Set("Referrer-Policy", "no-referrer")
		hd.Set("Strict-Transport-Security", "max-age=31536000")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			hd.Set("Cache-Control", "no-store")
		}
		h.ServeHTTP(w, r)
	})
}

// clientIP: cloudflared connects from loopback and passes the visitor in CF-Connecting-IP.
func clientIP(r *http.Request) string {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		if cf := net.ParseIP(strings.TrimSpace(r.Header.Get("CF-Connecting-IP"))); cf != nil {
			return cf.String()
		}
	}
	return host
}

func jsonOut(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func jsonErr(w http.ResponseWriter, code int, msg string) {
	jsonOut(w, code, map[string]any{"ok": false, "error": msg})
}

// ---------------------------------------------------------------- authentication

type failRec struct {
	n     int
	first time.Time
	until time.Time
}

var (
	failMu sync.Mutex
	fails  = map[string]*failRec{}
)

const (
	failMax    = 10
	failWindow = 10 * time.Minute
	failBlock  = 15 * time.Minute
)

func blocked(ip string) bool {
	failMu.Lock()
	defer failMu.Unlock()
	f := fails[ip]
	return f != nil && time.Now().Before(f.until)
}

func noteFail(ip string) {
	failMu.Lock()
	defer failMu.Unlock()
	now := time.Now()
	f := fails[ip]
	if f == nil || now.Sub(f.first) > failWindow {
		f = &failRec{first: now}
		fails[ip] = f
	}
	f.n++
	if f.n >= failMax {
		f.until = now.Add(failBlock)
		log.Printf("auth: %s blocked for %s after %d bad API keys", ip, failBlock, f.n)
	}
	if len(fails) > 10000 { // bound memory
		for k, v := range fails {
			if now.Sub(v.first) > failWindow && now.After(v.until) {
				delete(fails, k)
			}
		}
	}
}

// authenticate returns the API key, or writes an error response. scope: what the request needs.
func authenticate(w http.ResponseWriter, r *http.Request, c smscfg.Config, scope string) (smscfg.Key, bool) {
	k, ok := authKey(w, r, c)
	if ok && scope != "" && !k.Can(scope) {
		jsonErr(w, 403, fmt.Sprintf("this API key may not use this endpoint (needs the %q permission; set it in the router web UI)", scope))
		return k, false
	}
	return k, ok
}

func authKey(w http.ResponseWriter, r *http.Request, c smscfg.Config) (smscfg.Key, bool) {
	ip := clientIP(r)
	if blocked(ip) {
		jsonErr(w, 429, "too many invalid API keys; try again later")
		return smscfg.Key{}, false
	}
	key := ""
	if h := r.Header.Get("Authorization"); len(h) > 7 && strings.EqualFold(h[:7], "Bearer ") {
		key = strings.TrimSpace(h[7:])
	} else {
		key = strings.TrimSpace(r.Header.Get("X-API-Key"))
	}
	if key == "" {
		w.Header().Set("WWW-Authenticate", `Bearer realm="smsgw"`)
		jsonErr(w, 401, "missing API key (Authorization: Bearer <key>)")
		return smscfg.Key{}, false
	}
	h := smscfg.HashKey(key)
	for _, k := range c.Keys {
		if k.Hash == h {
			return k, true
		}
	}
	noteFail(ip)
	w.Header().Set("WWW-Authenticate", `Bearer realm="smsgw", error="invalid_token"`)
	jsonErr(w, 401, "invalid API key")
	return smscfg.Key{}, false
}

// ---------------------------------------------------------------- rate limits

var (
	rlMu      sync.Mutex
	keyMinute = map[string][]time.Time{}
	numHour   = map[string][]time.Time{}
	keyDay    = map[string]int{} // "day|key"
	globalDay = map[string]int{} // day
)

func prune(ts []time.Time, win time.Duration, now time.Time) []time.Time {
	i := 0
	for i < len(ts) && now.Sub(ts[i]) >= win {
		i++
	}
	return ts[i:]
}

// reserve checks every limit and, if all pass, counts this attempt.
func reserve(c smscfg.Config, key, number string) error {
	rlMu.Lock()
	defer rlMu.Unlock()
	now := time.Now()
	day := smscfg.Today(now)
	km := prune(keyMinute[key], time.Minute, now)
	nh := prune(numHour[number], time.Hour, now)
	keyMinute[key], numHour[number] = km, nh
	l := c.Limits
	switch {
	case l.GlobalPerDay > 0 && globalDay[day] >= l.GlobalPerDay:
		return fmt.Errorf("daily gateway limit reached (%d SMS)", l.GlobalPerDay)
	case l.PerKeyPerDay > 0 && keyDay[day+"|"+key] >= l.PerKeyPerDay:
		return fmt.Errorf("daily limit for this API key reached (%d SMS)", l.PerKeyPerDay)
	case l.PerKeyPerMinute > 0 && len(km) >= l.PerKeyPerMinute:
		return fmt.Errorf("too many requests for this API key (%d per minute)", l.PerKeyPerMinute)
	case l.PerNumberPerHour > 0 && len(nh) >= l.PerNumberPerHour:
		return fmt.Errorf("too many messages to this number (%d per hour)", l.PerNumberPerHour)
	}
	keyMinute[key] = append(km, now)
	numHour[number] = append(nh, now)
	keyDay[day+"|"+key]++
	globalDay[day]++
	for k := range globalDay { // drop old days
		if k != day {
			delete(globalDay, k)
		}
	}
	for k := range keyDay {
		if !strings.HasPrefix(k, day+"|") {
			delete(keyDay, k)
		}
	}
	return nil
}

// loadCounters restores today's daily counters from the database after a restart.
func loadCounters() {
	day := smscfg.Today(time.Now())
	per, total, err := db.APIUsageSince(smsdb.DayStart(time.Now()))
	if err != nil {
		log.Printf("counters: %v", err)
		return
	}
	rlMu.Lock()
	defer rlMu.Unlock()
	globalDay[day] = total
	for k, n := range per {
		keyDay[day+"|"+k] = n
	}
}

// importLegacyLog moves the old sent.jsonl entries into the database (once).
func importLegacyLog() {
	if _, err := os.Stat(*logPath); err != nil {
		return
	}
	n := 0
	for _, e := range smscfg.ReadLog(*logPath, 0) {
		o := smsdb.Out{Ref: e.ID, CreatedAt: e.Time, Recipient: e.To, Text: e.Text, Chars: e.Chars, Channel: "api",
			APIKey: e.Key, ClientIP: e.IP, MessageSource: e.Source, Status: e.Status, Error: e.Error}
		if e.Status == "sent" {
			o.SentAt = e.Time
		}
		if db.AddOut(o) == nil {
			n++
		}
	}
	os.Rename(*logPath, *logPath+".imported")
	os.Rename(*logPath+".1", *logPath+".1.imported")
	log.Printf("imported %d entries from %s into the database", n, *logPath)
}

func usage(key string) (int, int) {
	rlMu.Lock()
	defer rlMu.Unlock()
	day := smscfg.Today(time.Now())
	return keyDay[day+"|"+key], globalDay[day]
}

// ---------------------------------------------------------------- modem

func mmcli(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	return smssend.Runner(ctx, timeout, args...)
}

type modemInfo struct {
	State    string `json:"state"`
	Operator string `json:"operator,omitempty"`
	Signal   string `json:"signal_quality,omitempty"`
}

func modemStatus(ctx context.Context) (modemInfo, error) {
	out, err := mmcli(ctx, 15*time.Second, "-m", "any", "-J")
	if err != nil {
		return modemInfo{State: "unavailable"}, err
	}
	var m struct {
		Modem struct {
			Generic struct {
				State         string `json:"state"`
				SignalQuality struct {
					Value string `json:"value"`
				} `json:"signal-quality"`
			} `json:"generic"`
			ThreeGPP struct {
				OperatorName string `json:"operator-name"`
			} `json:"3gpp"`
		} `json:"modem"`
	}
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		return modemInfo{State: "unknown"}, err
	}
	return modemInfo{State: m.Modem.Generic.State, Operator: m.Modem.ThreeGPP.OperatorName, Signal: m.Modem.Generic.SignalQuality.Value}, nil
}

func newID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// ---------------------------------------------------------------- send queue

var (
	sendQueue = make(chan string, 200) // message refs, sent in order by sendWorker
	waitMu    sync.Mutex
	waiters   = map[string][]chan struct{}{}
)

// waitFor returns a channel closed when the worker has finished the message.
func waitFor(ref string) chan struct{} {
	ch := make(chan struct{})
	waitMu.Lock()
	waiters[ref] = append(waiters[ref], ch)
	waitMu.Unlock()
	return ch
}

func stopWaiting(ref string, ch chan struct{}) {
	waitMu.Lock()
	defer waitMu.Unlock()
	l := waiters[ref]
	for i, c := range l {
		if c == ch {
			waiters[ref] = append(l[:i], l[i+1:]...)
			break
		}
	}
	if len(waiters[ref]) == 0 {
		delete(waiters, ref)
	}
}

func finished(ref string) {
	waitMu.Lock()
	l := waiters[ref]
	delete(waiters, ref)
	waitMu.Unlock()
	for _, c := range l {
		close(c)
	}
}

// sendWorker sends queued messages one at a time (the modem handles one; smssend's file lock also keeps the
// router web UI's own sends out of the way).
func sendWorker() {
	for ref := range sendQueue {
		sendOne(ref)
	}
}

func sendOne(ref string) {
	defer finished(ref)
	if ok, err := db.SetOutStatus(ref, "queued", "sending"); err != nil || !ok {
		if err != nil {
			log.Printf("database: %v", err)
		}
		return
	}
	o, _, err := db.GetOut(ref)
	if err != nil {
		log.Printf("database: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	res, err := smssend.Send(ctx, o.Recipient, o.Text, smssend.Options{CheckModem: true, DeliveryReport: o.Delivery == "requested"})
	cancel()
	if dbErr := db.FinishOutRef(ref, err, res.MessageRef); dbErr != nil {
		log.Printf("database: %v", dbErr)
	}
	if err != nil {
		log.Printf("send %s to %s by %q failed: %v", ref, o.Recipient, o.APIKey, err)
		return
	}
	log.Printf("sent %s to %s (%d chars, %s message) by %q", ref, o.Recipient, o.Chars, o.MessageSource, o.APIKey)
}

// enqueue hands a recorded message to the worker; a full queue fails it at once.
func enqueue(ref string) bool {
	select {
	case sendQueue <- ref:
		return true
	default:
		db.FinishOut(ref, errors.New("not sent: the send queue is full (try again later)"))
		return false
	}
}

// ---------------------------------------------------------------- handlers

type sendReq struct {
	To      string `json:"to"`
	Number  string `json:"number"` // alias of "to"
	Message string `json:"message"`
}

func readSend(r *http.Request) (sendReq, error) {
	var q sendReq
	body, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, 8<<10))
	if err != nil {
		return q, errors.New("request body too large (max 8 KB)")
	}
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "application/x-www-form-urlencoded") {
		v, err := url.ParseQuery(string(body))
		if err != nil {
			return q, errors.New("invalid form body")
		}
		q.To, q.Number, q.Message = v.Get("to"), v.Get("number"), v.Get("message")
	} else {
		dec := json.NewDecoder(strings.NewReader(string(body)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&q); err != nil {
			return q, errors.New(`invalid JSON body; expected {"to":"9876543210","message":"…"}`)
		}
	}
	if q.To == "" {
		q.To = q.Number
	}
	q.To = strings.TrimSpace(q.To)
	return q, nil
}

var reIdemKey = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,100}$`)

// waitSeconds: ?wait=N (0-90), default 60. Cloudflare drops a request after 100 s.
func waitSeconds(r *http.Request) (time.Duration, error) {
	v := r.URL.Query().Get("wait")
	if v == "" {
		return 60 * time.Second, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 || n > 90 {
		return 0, errors.New("wait must be 0-90 seconds")
	}
	return time.Duration(n) * time.Second, nil
}

func handleSend(w http.ResponseWriter, r *http.Request) {
	c := current()
	if !c.Enabled {
		jsonErr(w, 503, "the SMS gateway is disabled")
		return
	}
	k, ok := authenticate(w, r, c, smscfg.ScopeSend)
	if !ok {
		return
	}
	key := k.Name
	idem := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idem != "" && !reIdemKey.MatchString(idem) {
		jsonErr(w, 400, "Idempotency-Key: 1-100 characters, letters, digits and . _ : - only (a UUID works)")
		return
	}
	wait, err := waitSeconds(r)
	if err != nil {
		jsonErr(w, 400, err.Error())
		return
	}
	q, err := readSend(r)
	if err != nil {
		jsonErr(w, 400, err.Error())
		return
	}
	if !smscfg.ValidNumber(q.To) {
		jsonErr(w, 400, "\"to\" must be a 10-digit Indian mobile number (digits only, starting with 6-9, no +91 or 0 prefix)")
		return
	}
	text, source := smscfg.CleanMessage(q.Message), "request"
	if strings.TrimSpace(text) == "" {
		text, source = c.DefaultMessage, "default"
	}
	if strings.TrimSpace(text) == "" {
		jsonErr(w, 400, "no message given and no default message is configured")
		return
	}
	if n := utf8.RuneCountInString(text); n > c.MaxLength {
		jsonErr(w, 400, fmt.Sprintf("message is %d characters; the maximum is %d", n, c.MaxLength))
		return
	}
	to := smscfg.E164(q.To)
	if idem != "" {
		if prev, found, err := db.ByIdemKey(key, idem); err != nil {
			log.Printf("database: %v", err)
			jsonErr(w, 500, "database error; nothing was sent")
			return
		} else if found {
			replay(w, r, c, prev, to, text, wait)
			return
		}
	}
	if err := reserve(c, key, q.To); err != nil {
		w.Header().Set("Retry-After", "60")
		jsonErr(w, 429, err.Error())
		return
	}
	e := smsdb.Out{Ref: newID(), Recipient: to, Text: text, Chars: utf8.RuneCountInString(text), Channel: "api",
		APIKey: key, ClientIP: clientIP(r), MessageSource: source, Status: "queued", IdemKey: idem}
	if c.DeliveryReports {
		e.Delivery = "requested"
	}
	if err := db.AddOut(e); errors.Is(err, smsdb.ErrDuplicate) { // the same key arrived twice at once
		if prev, found, _ := db.ByIdemKey(key, idem); found {
			replay(w, r, c, prev, to, text, wait)
			return
		}
		jsonErr(w, 500, "database error; nothing was sent")
		return
	} else if err != nil { // never send without a record
		log.Printf("database: %v", err)
		jsonErr(w, 500, "could not record the message; nothing was sent")
		return
	}
	ch := waitFor(e.Ref)
	if !enqueue(e.Ref) { // already marked failed: answer at once
		stopWaiting(e.Ref, ch)
		ch = nil
		w.Header().Set("Retry-After", "60")
	}
	respond(w, r, e.Ref, ch, wait, false)
}

// replay answers a repeated Idempotency-Key: the same message is never sent twice. Only a message that definitely
// failed ("failed": not sent) is tried again, and that counts against the rate limits like a new send.
func replay(w http.ResponseWriter, r *http.Request, c smscfg.Config, prev smsdb.Out, to, text string, wait time.Duration) {
	w.Header().Set("Idempotent-Replayed", "true")
	if prev.Recipient != to || prev.Text != text {
		jsonErr(w, 422, "this Idempotency-Key was already used for a different message")
		return
	}
	if prev.Status == "failed" {
		if err := reserve(c, prev.APIKey, strings.TrimPrefix(to, "+91")); err != nil {
			w.Header().Set("Retry-After", "60")
			jsonErr(w, 429, err.Error())
			return
		}
		if ok, _ := db.SetOutStatus(prev.Ref, "failed", "queued"); ok {
			ch := waitFor(prev.Ref)
			if !enqueue(prev.Ref) {
				stopWaiting(prev.Ref, ch)
				ch = nil
			}
			respond(w, r, prev.Ref, ch, wait, true)
			return
		}
	}
	var ch chan struct{}
	if prev.Status == "queued" || prev.Status == "sending" {
		ch = waitFor(prev.Ref)
	}
	respond(w, r, prev.Ref, ch, wait, true)
}

// respond waits (up to wait) for the worker, then reports the message's current state.
func respond(w http.ResponseWriter, r *http.Request, ref string, done chan struct{}, wait time.Duration, replayed bool) {
	if done != nil { // finished between the caller's status check and waitFor? (the worker updates the row, then signals)
		if o, ok, _ := db.GetOut(ref); ok && o.Status != "queued" && o.Status != "sending" {
			stopWaiting(ref, done)
			done = nil
		}
	}
	if done != nil {
		t := time.NewTimer(wait)
		select {
		case <-done:
		case <-t.C:
		case <-r.Context().Done():
		}
		t.Stop()
		stopWaiting(ref, done)
	}
	o, found, err := db.GetOut(ref)
	if err != nil || !found {
		jsonErr(w, 500, "database error")
		return
	}
	v := outView(o)
	if replayed {
		v["idempotent_replay"] = true
	}
	switch o.Status {
	case "sent":
		jsonOut(w, 200, v)
	case "queued", "sending":
		w.Header().Set("Location", "/api/v1/sms/"+o.Ref)
		jsonOut(w, 202, v)
	default: // failed, unknown. 503, not 502: Cloudflare replaces 502/504 bodies with its own error page
		jsonOut(w, 503, v)
	}
}

// outView is the API's view of a message (no API key, client IP or text echo beyond the length).
func outView(o smsdb.Out) map[string]any {
	v := map[string]any{"ok": o.Status == "sent" || o.Status == "queued" || o.Status == "sending", "id": o.Ref, "to": o.Recipient,
		"status": o.Status, "message_source": o.MessageSource, "chars": o.Chars, "created_at": o.CreatedAt,
		"status_url": "/api/v1/sms/" + o.Ref}
	if o.SentAt != "" {
		v["sent_at"] = o.SentAt
	}
	switch o.Status {
	case "failed":
		v["error"] = publicError(o.Error)
		v["retry"] = "not sent: safe to retry (with the same Idempotency-Key, if you used one)"
	case "unknown":
		v["error"] = "the modem did not confirm the send in time: the SMS may or may not have been delivered. " +
			"Retrying with the same Idempotency-Key will not send it again."
	}
	if o.Delivery != "" {
		d := map[string]any{"status": o.Delivery}
		if o.DeliveryAt != "" {
			d["at"] = o.DeliveryAt
		}
		if o.DeliveryNote != "" {
			d["detail"] = o.DeliveryNote
		}
		v["delivery_report"] = d
	}
	return v
}

// publicError keeps modem internals out of API answers.
func publicError(e string) string {
	switch {
	case strings.Contains(e, smssend.ErrModem.Error()):
		return smssend.ErrModem.Error()
	case strings.Contains(e, "send queue is full"), strings.Contains(e, "restarted before sending"):
		return e
	}
	return "the mobile network did not accept the SMS; try again later"
}

func handleGetSMS(w http.ResponseWriter, r *http.Request) {
	c := current()
	k, ok := authenticate(w, r, c, smscfg.ScopeSend)
	if !ok {
		return
	}
	o, found, err := db.GetOut(r.PathValue("id"))
	if err != nil {
		jsonErr(w, 500, "database error")
		return
	}
	if !found || o.Channel != "api" || o.APIKey != k.Name {
		jsonErr(w, 404, "no such message for this API key")
		return
	}
	jsonOut(w, 200, outView(o))
}

func handleInbox(w http.ResponseWriter, r *http.Request) {
	c := current()
	if !c.Enabled {
		jsonErr(w, 503, "the SMS gateway is disabled")
		return
	}
	if _, ok := authenticate(w, r, c, smscfg.ScopeInbox); !ok {
		return
	}
	v := r.URL.Query()
	var after int64
	if s := v.Get("after_id"); s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n < 0 {
			jsonErr(w, 400, "after_id must be a message id (a non-negative number)")
			return
		}
		after = n
	}
	since := ""
	if s := v.Get("since"); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			jsonErr(w, 400, "since must be an RFC 3339 time, e.g. 2026-11-26T10:00:00+05:30")
			return
		}
		since = t.Format(time.RFC3339)
	}
	limit := 50
	if s := v.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > 100 {
			jsonErr(w, 400, "limit must be 1-100")
			return
		}
		limit = n
	}
	l, err := db.Inbox(after, since, limit)
	if err != nil {
		log.Printf("database: %v", err)
		jsonErr(w, 500, "database error")
		return
	}
	msgs := make([]map[string]any, 0, len(l))
	next := after
	for _, m := range l {
		msgs = append(msgs, map[string]any{"id": m.ID, "from": m.Sender, "text": m.Text, "sms_time": m.SMSTime, "received_at": m.StoredAt})
		next = m.ID
	}
	jsonOut(w, 200, map[string]any{"ok": true, "messages": msgs, "next_after_id": next, "more": len(l) == limit})
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	c := current()
	k, ok := authenticate(w, r, c, "")
	if !ok {
		return
	}
	mi, _ := modemStatus(r.Context())
	ku, g := usage(k.Name)
	jsonOut(w, 200, map[string]any{
		"ok": true, "enabled": c.Enabled, "key": k.Name, "scopes": k.EffectiveScopes(), "modem": mi,
		"max_length": c.MaxLength, "default_message_set": strings.TrimSpace(c.DefaultMessage) != "",
		"delivery_reports": c.DeliveryReports, "queued": len(sendQueue),
		"limits": c.Limits, "usage_today": map[string]int{"this_key": ku, "gateway": g},
	})
}

func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}
