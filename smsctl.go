package main

// SMS remote control: commands by SMS from allowed numbers, for when Mesh or LTE data is down and the web
// UI is out of reach. An SMS reads "<PIN> <COMMAND>", e.g. "4821 STATUS"; the reply goes back by SMS.
//
// Safety: only listed numbers count (SMS senders can be spoofed, so the PIN is required too); other
// senders get no reply. Three wrong PINs from a number lock it out for an hour. Commands older than
// 15 minutes (an SMS delivered late, e.g. after the stick was off) are ignored. At most 10 replies per
// number per hour. Command SMS are stored with the PIN masked and are never forwarded to WhatsApp.

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/caseypaite/kstick-router/internal/smscfg"
	"github.com/caseypaite/kstick-router/internal/smsdb"
)

type SMSCtlCfg struct {
	Enabled bool     `json:"enabled"`
	Numbers []string `json:"numbers"`  // allowed senders: 10-digit Indian or full international, digits only
	PINHash string   `json:"pin_hash"` // sha256, set through the API (write-only)
	Notify  bool     `json:"notify"`   // also send notifications (data cap) to these numbers
}

var (
	reCtlNumber = regexp.MustCompile(`^[0-9]{10,15}$`)
	reCtlPIN    = regexp.MustCompile(`^[0-9]{4,12}$`)
	reHashHex   = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

func validateSMSCtl(s *SMSCtlCfg) error {
	if len(s.Numbers) > 5 {
		return badRequest("SMS control: at most 5 numbers")
	}
	seen := map[string]bool{}
	for i, n := range s.Numbers {
		n = strings.TrimPrefix(strings.ReplaceAll(strings.TrimSpace(n), " ", ""), "+")
		if !reCtlNumber.MatchString(n) || seen[n] {
			return badRequest("SMS control: number %q must be 10 digits (India) or a full international number, digits only", s.Numbers[i])
		}
		seen[n], s.Numbers[i] = true, n
	}
	if s.PINHash != "" && !reHashHex.MatchString(s.PINHash) {
		return badRequest("SMS control: invalid PIN hash")
	}
	if s.Enabled && (len(s.Numbers) == 0 || s.PINHash == "") {
		return badRequest("SMS control needs at least one number and a PIN")
	}
	return nil
}

func hashPIN(pin string) string {
	h := sha256.Sum256([]byte("kstick-smsctl:" + pin))
	return hex.EncodeToString(h[:])
}

// ctlNumberMatches: does the SMS sender belong to an allowed number? 10-digit entries are Indian
// numbers; the sender may come as +91…, 91… or 0….
func ctlNumberMatches(allowed, sender string) bool {
	d := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, sender)
	if len(allowed) == 10 {
		return d == allowed || d == "91"+allowed || d == "0"+allowed
	}
	return d == allowed
}

func ctlAllowed(c SMSCtlCfg, sender string) bool {
	for _, n := range c.Numbers {
		if ctlNumberMatches(n, sender) {
			return true
		}
	}
	return false
}

var ctlCommands = map[string]string{
	"HELP":         "list the commands",
	"STATUS":       "uplink, signal, temperature, battery, data, Mesh",
	"DATA":         "data used today and this period",
	"LTE RESET":    "reconnect the LTE data connection",
	"MESH RESTART": "restart the Cloudflare Mesh client",
	"REBOOT":       "reboot the router",
}

type ctlCommand struct {
	pin, cmd string // cmd: normalized command, "" when the text is not a command
	masked   string // the text to store (PIN replaced)
}

// parseCtl recognises "<PIN> <COMMAND>" (or a bare command, which then has no PIN).
func parseCtl(text string) ctlCommand {
	f := strings.Fields(strings.ToUpper(strings.TrimSpace(text)))
	if len(f) == 0 || len(f) > 3 {
		return ctlCommand{masked: text}
	}
	pin, rest := "", f
	if _, isCmd := ctlCommands[strings.Join(f, " ")]; !isCmd {
		pin, rest = f[0], f[1:]
	}
	cmd := strings.Join(rest, " ")
	if _, ok := ctlCommands[cmd]; !ok {
		return ctlCommand{masked: text}
	}
	masked := cmd
	if pin != "" {
		masked = "•••• " + cmd
	}
	return ctlCommand{pin: pin, cmd: cmd, masked: masked}
}

// ctlTooOld: the SMS was sent more than 15 minutes ago (only judged when both times are trustworthy).
func ctlTooOld(ts string, now time.Time) bool {
	if !clockSane(now) {
		return false
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05-07"} {
		if t, err := time.Parse(layout, ts); err == nil {
			return now.Sub(t) > 15*time.Minute
		}
	}
	return false
}

type ctlSender struct {
	bad         []time.Time // wrong PINs
	lockedUntil time.Time
	replies     []time.Time
}

var (
	ctlMu      sync.Mutex
	ctlSenders = map[string]*ctlSender{}
	ctlLog     []map[string]string
	ctlRun     sync.Mutex // one command at a time
)

func ctlNote(from, cmd, result string) {
	log.Printf("sms control: %s from %s: %s", firstNonEmpty(cmd, "?"), from, result)
	ctlMu.Lock()
	defer ctlMu.Unlock()
	ctlLog = append(ctlLog, map[string]string{"time": time.Now().Format(time.RFC3339), "from": from, "command": cmd, "result": result})
	if len(ctlLog) > 50 {
		ctlLog = ctlLog[len(ctlLog)-50:]
	}
}

func recent(ts []time.Time, win time.Duration, now time.Time) []time.Time {
	out := ts[:0]
	for _, t := range ts {
		if now.Sub(t) < win {
			out = append(out, t)
		}
	}
	return out
}

// ctlCheck applies the lockout and reply limits; it returns whether to run the command, and the reply
// to send for a refusal ("" = stay silent).
func ctlCheck(c SMSCtlCfg, from, pin string, now time.Time) (ok bool, reply string) {
	ctlMu.Lock()
	defer ctlMu.Unlock()
	s := ctlSenders[from]
	if s == nil {
		s = &ctlSender{}
		ctlSenders[from] = s
	}
	if now.Before(s.lockedUntil) {
		return false, ""
	}
	s.replies = recent(s.replies, time.Hour, now)
	if len(s.replies) >= 10 {
		return false, ""
	}
	if pin == "" || subtle.ConstantTimeCompare([]byte(hashPIN(pin)), []byte(c.PINHash)) != 1 {
		s.bad = append(recent(s.bad, time.Hour, now), now)
		if len(s.bad) >= 3 {
			s.lockedUntil, s.bad = now.Add(time.Hour), nil
			s.replies = append(s.replies, now)
			return false, "KStick: wrong PIN 3 times, this number is locked out for 1 hour."
		}
		s.replies = append(s.replies, now)
		return false, "KStick: wrong or missing PIN. Send: <PIN> HELP"
	}
	s.bad = nil
	s.replies = append(s.replies, now)
	return true, ""
}

// ctlParse checks a received SMS: is it a command from an allowed number? (Nothing runs yet.)
func ctlParse(from, text string) (ctlCommand, bool) {
	c := snapshot().SMSCtl
	if !c.Enabled || !ctlAllowed(c, from) {
		return ctlCommand{}, false
	}
	cmd := parseCtl(text)
	return cmd, cmd.cmd != ""
}

// ctlExecute runs a command SMS once it is stored as new (so a re-read SMS never runs twice).
func ctlExecute(from, ts string, cmd ctlCommand) {
	c := snapshot().SMSCtl
	go func() {
		ctlRun.Lock()
		defer ctlRun.Unlock()
		now := time.Now()
		if ctlTooOld(ts, now) {
			ctlNote(from, cmd.cmd, "ignored: sent more than 15 minutes ago")
			return
		}
		ok, refusal := ctlCheck(c, from, cmd.pin, now)
		if !ok {
			ctlNote(from, cmd.cmd, firstNonEmpty(refusal, "ignored (locked out or reply limit)"))
			if refusal != "" {
				ctlReply(from, refusal)
			}
			return
		}
		runCtl(from, cmd.cmd)
	}()
}

func ctlReply(to, text string) {
	d := smsDB()
	ref := fmt.Sprintf("ctl%x", time.Now().UnixNano())
	if d != nil {
		_ = d.AddOut(smsdb.Out{Ref: ref, Recipient: to, Text: text, Chars: len([]rune(text)), Channel: "sms-control"})
	}
	err := modemSendSMS(to, text)
	if d != nil {
		d.FinishOut(ref, err)
	}
	if err != nil {
		log.Printf("sms control: reply to %s: %v", to, err)
	}
}

func runCtl(from, cmd string) {
	switch cmd {
	case "HELP":
		ctlReply(from, "KStick commands (send <PIN> <command>): STATUS, DATA, LTE RESET, MESH RESTART, REBOOT")
		ctlNote(from, cmd, "replied")
	case "STATUS":
		ctlReply(from, statusText())
		ctlNote(from, cmd, "replied")
	case "DATA":
		ctlReply(from, "KStick data: "+dataSummaryText())
		ctlNote(from, cmd, "replied")
	case "LTE RESET":
		ctlReply(from, "KStick: reconnecting LTE data now.")
		_, _ = run(30*time.Second, "nmcli", "connection", "down", lteCon)
		_, err := run(90*time.Second, "nmcli", "-w", "75", "connection", "up", lteCon)
		refreshModem()
		ctlNote(from, cmd, fmt.Sprintf("done (error: %v)", err))
	case "MESH RESTART":
		_, err := run(90*time.Second, "systemctl", "restart", "warp-svc")
		time.Sleep(20 * time.Second) // let it connect before reporting
		state := "Mesh " + firstNonEmpty(ifaceIPv4(meshIf), "not connected yet")
		if err != nil {
			state = "restart failed: " + truncRunes(err.Error(), 80)
		}
		ctlReply(from, "KStick: Mesh client restarted. "+state)
		ctlNote(from, cmd, state)
	case "REBOOT":
		ctlReply(from, "KStick: rebooting now, back in about a minute.")
		ctlNote(from, cmd, "rebooting")
		flushState()
		_, _ = run(10*time.Second, "systemctl", "reboot")
	}
}

// statusText: a one-SMS summary (plain ASCII keeps it in one 160-character GSM-7 message).
func statusText() string {
	var b strings.Builder
	b.WriteString("KStick " + time.Now().Format("15:04"))
	up := strings.Fields(readTrim("/proc/uptime") + " 0")[0]
	var secs float64
	fmt.Sscan(up, &secs)
	b.WriteString(" up " + (time.Duration(secs) * time.Second).Round(time.Minute).String())
	wan := firstNonEmpty(readWANState()["active"], "none")
	b.WriteString(" | WAN " + wan)
	m := modemSummary()
	if p, _ := m["present"].(bool); p {
		fmt.Fprintf(&b, " %s %s%%", firstNonEmpty(fmt.Sprint(m["operator"]), "?"), firstNonEmpty(fmt.Sprint(m["signal"]), "?"))
		if r := fmt.Sprint(m["rsrp"]); r != "" && r != "<nil>" {
			b.WriteString(" " + r + "dBm")
		}
	}
	if ip := ifaceIPv4(wan); ip != "" {
		b.WriteString(" " + ip)
	}
	fmt.Fprintf(&b, " | %.0fC", maxThermalC())
	if bat := batteryInfo(); bat != nil {
		if p, ok := bat["percent"].(int); ok {
			fmt.Fprintf(&b, " bat %d%%", p)
			if ch, _ := bat["charging"].(bool); ch {
				b.WriteString("+")
			}
		}
	}
	b.WriteString(" | mesh " + firstNonEmpty(ifaceIPv4(meshIf), "down"))
	b.WriteString(" | " + dataSummaryText())
	return truncRunes(b.String(), 300)
}

// smsNotify sends a notification to the SMS control numbers (when enabled with notifications on).
func smsNotify(text string) {
	c := snapshot().SMSCtl
	if !c.Enabled || !c.Notify {
		return
	}
	for _, n := range c.Numbers {
		to := n
		if smscfg.ValidNumber(n) {
			to = smscfg.E164(n)
		} else {
			to = "+" + n
		}
		d := smsDB()
		ref := fmt.Sprintf("nt%x", time.Now().UnixNano())
		if d != nil {
			_ = d.AddOut(smsdb.Out{Ref: ref, Recipient: to, Text: text, Chars: len([]rune(text)), Channel: "notify"})
		}
		err := modemSendSMS(to, text)
		if d != nil {
			d.FinishOut(ref, err)
		}
		if err != nil {
			log.Printf("notify %s: %v", to, err)
		}
	}
}

func apiGetSMSCtl(r *http.Request) (any, error) {
	c := snapshot().SMSCtl
	ctlMu.Lock()
	logs := append([]map[string]string{}, ctlLog...)
	ctlMu.Unlock()
	cmds := []map[string]string{}
	for _, k := range []string{"STATUS", "DATA", "LTE RESET", "MESH RESTART", "REBOOT", "HELP"} {
		cmds = append(cmds, map[string]string{"command": k, "what": ctlCommands[k]})
	}
	return map[string]any{"enabled": c.Enabled, "numbers": c.Numbers, "pin_set": c.PINHash != "", "notify": c.Notify,
		"log": logs, "commands": cmds}, nil
}

func apiPutSMSCtl(r *http.Request) (any, error) {
	var b struct {
		Enabled bool     `json:"enabled"`
		Numbers []string `json:"numbers"`
		PIN     string   `json:"pin"` // empty = keep
		Notify  bool     `json:"notify"`
	}
	if err := readJSON(r, &b); err != nil {
		return nil, err
	}
	b.PIN = strings.TrimSpace(b.PIN)
	if b.PIN != "" && !reCtlPIN.MatchString(b.PIN) {
		return nil, badRequest("PIN must be 4-12 digits")
	}
	if b.Numbers == nil {
		b.Numbers = []string{}
	}
	res, err := updateCfg(r, "SMS control", func(c *Config) {
		c.SMSCtl.Enabled, c.SMSCtl.Numbers, c.SMSCtl.Notify = b.Enabled, b.Numbers, b.Notify
		if b.PIN != "" {
			c.SMSCtl.PINHash = hashPIN(b.PIN)
		}
	})
	if err == nil {
		ctlMu.Lock()
		ctlSenders = map[string]*ctlSender{} // a new PIN or list starts clean
		ctlMu.Unlock()
	}
	return res, err
}
