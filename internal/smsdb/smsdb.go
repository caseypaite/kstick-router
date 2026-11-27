// Package smsdb is the SQLite log of every SMS the KStick sends or receives.
// Written by router-webui (root: received SMS, WhatsApp forwarding, web-UI sends)
// and kstick-smsgw (user smsgw: API sends). The file is owned by smsgw.
package smsdb

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"kstick-router/internal/smssend"

	_ "modernc.org/sqlite"
)

const Path = "/var/lib/smsgw/sms.db"

const schema = `
CREATE TABLE IF NOT EXISTS sms_in (
  id              INTEGER PRIMARY KEY,
  hash            TEXT NOT NULL UNIQUE,   -- sender+time+text, de-duplicates
  sender          TEXT NOT NULL,
  text            TEXT NOT NULL,
  smsc            TEXT NOT NULL DEFAULT '',
  sms_time        TEXT NOT NULL DEFAULT '', -- network timestamp of the SMS
  stored_at       TEXT NOT NULL,            -- when it was saved here
  origin          TEXT NOT NULL,            -- modem | sim-import
  wa_status       TEXT NOT NULL,            -- pending | forwarded | failed | skipped | off
  wa_attempts     INTEGER NOT NULL DEFAULT 0,
  wa_target       TEXT NOT NULL DEFAULT '',
  wa_forwarded_at TEXT NOT NULL DEFAULT '',
  wa_message_id   TEXT NOT NULL DEFAULT '',
  wa_note         TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS sms_in_time ON sms_in(stored_at);
CREATE INDEX IF NOT EXISTS sms_in_wa ON sms_in(wa_status);
CREATE TABLE IF NOT EXISTS sms_out (
  id             INTEGER PRIMARY KEY,
  ref            TEXT NOT NULL UNIQUE,     -- id returned to the API caller
  created_at     TEXT NOT NULL,
  sent_at        TEXT NOT NULL DEFAULT '',
  recipient      TEXT NOT NULL,
  text           TEXT NOT NULL,
  chars          INTEGER NOT NULL,
  channel        TEXT NOT NULL,            -- api | web-ui
  api_key        TEXT NOT NULL DEFAULT '', -- API key name (api channel)
  client_ip      TEXT NOT NULL DEFAULT '',
  message_source TEXT NOT NULL DEFAULT '', -- request | default
  status         TEXT NOT NULL,            -- queued | sending | sent | failed | unknown
  error          TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS sms_out_time ON sms_out(created_at);
CREATE INDEX IF NOT EXISTS sms_out_key ON sms_out(api_key, created_at);
-- forwarding of received SMS to targets other than WhatsApp (which keeps its wa_* columns)
CREATE TABLE IF NOT EXISTS sms_fwd (
  sms_id   INTEGER NOT NULL,
  target   TEXT NOT NULL,                  -- webhook | telegram | ntfy
  status   TEXT NOT NULL,                  -- pending | forwarded | failed
  attempts INTEGER NOT NULL DEFAULT 0,
  next_at  TEXT NOT NULL DEFAULT '',       -- no retry before this time (RFC3339)
  note     TEXT NOT NULL DEFAULT '',
  done_at  TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (sms_id, target)
);
CREATE INDEX IF NOT EXISTS sms_fwd_pending ON sms_fwd(target, status);
`

// columns added after the first release: added to existing databases on Open
var migrations = []struct{ table, col, def string }{
	{"sms_in", "ctl", "INTEGER NOT NULL DEFAULT 0"},          // 1 = SMS remote control command (never exposed by the API)
	{"sms_in", "wa_next_at", "TEXT NOT NULL DEFAULT ''"},     // WhatsApp: no retry before this time
	{"sms_out", "idem_key", "TEXT NOT NULL DEFAULT ''"},      // API Idempotency-Key header
	{"sms_out", "msg_ref", "INTEGER NOT NULL DEFAULT -1"},    // network message reference, for delivery reports
	{"sms_out", "delivery", "TEXT NOT NULL DEFAULT ''"},      // '' (not requested) | requested | delivered | failed
	{"sms_out", "delivery_at", "TEXT NOT NULL DEFAULT ''"},   // when the report arrived
	{"sms_out", "delivery_note", "TEXT NOT NULL DEFAULT ''"}, // ModemManager delivery state, e.g. completed-received
}

const postMigration = `
-- remote-control commands stored before the ctl column existed (recognised by their note) stay out of the inbox API
UPDATE sms_in SET ctl=1 WHERE ctl=0 AND wa_note='SMS remote control command';
CREATE UNIQUE INDEX IF NOT EXISTS sms_out_idem ON sms_out(api_key, idem_key) WHERE idem_key != '';
CREATE INDEX IF NOT EXISTS sms_out_ref ON sms_out(msg_ref) WHERE msg_ref >= 0;
`

func migrate(d *sql.DB) error {
	for _, m := range migrations {
		var n int
		if err := d.QueryRow(`SELECT count(*) FROM pragma_table_info(?) WHERE name=?`, m.table, m.col).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			// router-webui and kstick-smsgw may both upgrade at the same moment: the other one winning is fine
			if _, err := d.Exec(`ALTER TABLE ` + m.table + ` ADD COLUMN ` + m.col + ` ` + m.def); err != nil &&
				!strings.Contains(err.Error(), "duplicate column name") {
				return err
			}
		}
	}
	_, err := d.Exec(postMigration)
	return err
}

type DB struct{ *sql.DB }

// Open creates the database if needed. When running as root the file is
// handed to user smsgw so the unprivileged gateway can write to it too.
func Open(path string) (*DB, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0750); err != nil {
		return nil, err
	}
	dsn := "file:" + path + "?_pragma=busy_timeout(10000)&_pragma=journal_mode(DELETE)&_pragma=synchronous(FULL)"
	d, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	d.SetMaxOpenConns(1)
	if _, err := d.Exec(schema); err != nil {
		d.Close()
		return nil, fmt.Errorf("sms database: %w", err)
	}
	if err := migrate(d); err != nil {
		d.Close()
		return nil, fmt.Errorf("sms database upgrade: %w", err)
	}
	if os.Geteuid() == 0 {
		if u, err := user.Lookup("smsgw"); err == nil {
			uid, _ := strconv.Atoi(u.Uid)
			gid, _ := strconv.Atoi(u.Gid)
			os.Chown(dir, uid, gid)
			os.Chown(path, uid, gid)
		}
	}
	os.Chmod(path, 0640)
	return &DB{d}, nil
}

func Now() string { return time.Now().Format(time.RFC3339) }

// ---------------------------------------------------------------- incoming

type In struct {
	ID            int64  `json:"id"`
	Sender        string `json:"sender"`
	Text          string `json:"text"`
	SMSC          string `json:"smsc"`
	SMSTime       string `json:"sms_time"`
	StoredAt      string `json:"stored_at"`
	Origin        string `json:"origin"`
	WAStatus      string `json:"wa_status"`
	WAAttempts    int    `json:"wa_attempts"`
	WATarget      string `json:"wa_target"`
	WAForwardedAt string `json:"wa_forwarded_at"`
	WAMessageID   string `json:"wa_message_id"`
	WANote        string `json:"wa_note"`
	WANextAt      string `json:"wa_next_at,omitempty"`
	Ctl           bool   `json:"ctl,omitempty"`
	Forwards      []Fwd  `json:"forwards,omitempty"` // webhook / Telegram / ntfy (ListIn only)
}

const inCols = `id,sender,text,smsc,sms_time,stored_at,origin,wa_status,wa_attempts,wa_target,wa_forwarded_at,wa_message_id,wa_note,wa_next_at,ctl`

func scanIn(rows *sql.Rows) ([]In, error) {
	defer rows.Close()
	res := []In{}
	for rows.Next() {
		var m In
		if err := rows.Scan(&m.ID, &m.Sender, &m.Text, &m.SMSC, &m.SMSTime, &m.StoredAt, &m.Origin, &m.WAStatus, &m.WAAttempts, &m.WATarget, &m.WAForwardedAt, &m.WAMessageID, &m.WANote, &m.WANextAt, &m.Ctl); err != nil {
			return nil, err
		}
		res = append(res, m)
	}
	return res, rows.Err()
}

// AddIn stores a received SMS (Sender, Text, SMSC, SMSTime, Origin, WAStatus, WANote, Ctl) and queues it for the
// given forwarding targets. It returns the new id, or inserted=false if the SMS was already stored.
func (d *DB) AddIn(hash string, m In, targets []string) (id int64, inserted bool, err error) {
	tx, err := d.Begin()
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback()
	r, err := tx.Exec(`INSERT INTO sms_in(hash,sender,text,smsc,sms_time,stored_at,origin,wa_status,wa_note,ctl) VALUES(?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(hash) DO NOTHING`, hash, m.Sender, m.Text, m.SMSC, m.SMSTime, Now(), m.Origin, m.WAStatus, m.WANote, m.Ctl)
	if err != nil {
		return 0, false, err
	}
	if n, _ := r.RowsAffected(); n != 1 {
		return 0, false, nil
	}
	id, _ = r.LastInsertId()
	for _, t := range targets {
		if _, err := tx.Exec(`INSERT INTO sms_fwd(sms_id,target,status) VALUES(?,?,'pending') ON CONFLICT DO NOTHING`, id, t); err != nil {
			return 0, false, err
		}
	}
	return id, true, tx.Commit()
}

// PendingIn lists SMS waiting for WhatsApp forwarding whose retry time has come, oldest first.
func (d *DB) PendingIn(limit int) ([]In, error) {
	rows, err := d.Query(`SELECT `+inCols+` FROM sms_in WHERE wa_status='pending' AND wa_next_at <= ? ORDER BY id LIMIT ?`, Now(), limit)
	if err != nil {
		return nil, err
	}
	return scanIn(rows)
}

func (d *DB) MarkForwarded(id int64, target, msgID string) error {
	_, err := d.Exec(`UPDATE sms_in SET wa_status='forwarded', wa_attempts=wa_attempts+1, wa_target=?, wa_forwarded_at=?, wa_message_id=?, wa_note='' WHERE id=?`, target, Now(), msgID, id)
	return err
}

// MarkAttempt records a failed forwarding attempt; final=true stops retrying, otherwise the next try waits until next.
func (d *DB) MarkAttempt(id int64, target, note string, final bool, next time.Time) error {
	st := "pending"
	if final {
		st = "failed"
	}
	_, err := d.Exec(`UPDATE sms_in SET wa_status=?, wa_attempts=wa_attempts+1, wa_target=?, wa_note=?, wa_next_at=? WHERE id=?`,
		st, target, note, next.Format(time.RFC3339), id)
	return err
}

func (d *DB) SetInStatus(id int64, status, note string) error {
	r, err := d.Exec(`UPDATE sms_in SET wa_status=?, wa_note=?, wa_next_at='', wa_attempts=CASE WHEN ?='pending' THEN 0 ELSE wa_attempts END WHERE id=?`, status, note, status, id)
	if err == nil {
		if n, _ := r.RowsAffected(); n == 0 {
			err = errors.New("no such message")
		}
	}
	return err
}

// Retry policy for forwarding: 30 s, 1, 2, 4 … minutes, then hourly, for about a day (MaxAttempts).
const (
	RetryFirst = 30 * time.Second
	RetryMax   = time.Hour
)

// NextRetry returns when to try again after the given number of failed attempts (1 = the first failure).
func NextRetry(attempts int, now time.Time) time.Time {
	d := RetryFirst
	for i := 1; i < attempts && d < RetryMax; i++ {
		d *= 2
	}
	return now.Add(min(d, RetryMax))
}

// MaxAttempts: with NextRetry's spacing, 30 attempts take about a day (7 within the first hour, then hourly).
// Counted per forwarding round, so re-queueing an old SMS from the web UI gets a full round again.
const MaxAttempts = 30

// Inbox lists received SMS for the API, oldest first: id > afterID, and stored at or after since when set.
// SMS remote control commands are never included.
func (d *DB) Inbox(afterID int64, since string, limit int) ([]In, error) {
	q := `SELECT ` + inCols + ` FROM sms_in WHERE ctl=0 AND id > ?`
	a := []any{afterID}
	if since != "" {
		q, a = q+` AND stored_at >= ?`, append(a, since)
	}
	rows, err := d.Query(q+` ORDER BY id LIMIT ?`, append(a, lim(limit))...)
	if err != nil {
		return nil, err
	}
	return scanIn(rows)
}

// ---------------------------------------------------------------- other forwarding targets

type Fwd struct {
	SMSID    int64  `json:"-"`
	Target   string `json:"target"`
	Status   string `json:"status"`
	Attempts int    `json:"attempts"`
	NextAt   string `json:"next_at,omitempty"`
	Note     string `json:"note,omitempty"`
	DoneAt   string `json:"done_at,omitempty"`
}

// PendingFwd lists the SMS due for forwarding to target, oldest first.
func (d *DB) PendingFwd(target string, limit int) ([]In, []Fwd, error) {
	rows, err := d.Query(`SELECT i.id,i.sender,i.text,i.smsc,i.sms_time,i.stored_at,i.origin,i.wa_status,i.wa_attempts,i.wa_target,
		i.wa_forwarded_at,i.wa_message_id,i.wa_note,i.wa_next_at,i.ctl, f.attempts
		FROM sms_fwd f JOIN sms_in i ON i.id=f.sms_id
		WHERE f.target=? AND f.status='pending' AND f.next_at <= ? ORDER BY i.id LIMIT ?`, target, Now(), limit)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var ins []In
	var fs []Fwd
	for rows.Next() {
		var m In
		f := Fwd{Target: target, Status: "pending"}
		if err := rows.Scan(&m.ID, &m.Sender, &m.Text, &m.SMSC, &m.SMSTime, &m.StoredAt, &m.Origin, &m.WAStatus, &m.WAAttempts, &m.WATarget,
			&m.WAForwardedAt, &m.WAMessageID, &m.WANote, &m.WANextAt, &m.Ctl, &f.Attempts); err != nil {
			return nil, nil, err
		}
		f.SMSID = m.ID
		ins, fs = append(ins, m), append(fs, f)
	}
	return ins, fs, rows.Err()
}

// MarkFwd records a forwarding result. ok: forwarded. Otherwise final stops retrying, or the next try waits until next.
func (d *DB) MarkFwd(id int64, target string, ok bool, note string, final bool, next time.Time) error {
	var err error
	switch {
	case ok:
		_, err = d.Exec(`UPDATE sms_fwd SET status='forwarded', attempts=attempts+1, note='', next_at='', done_at=? WHERE sms_id=? AND target=?`, Now(), id, target)
	case final:
		_, err = d.Exec(`UPDATE sms_fwd SET status='failed', attempts=attempts+1, note=?, next_at='' WHERE sms_id=? AND target=?`, truncate(note, 300), id, target)
	default:
		_, err = d.Exec(`UPDATE sms_fwd SET attempts=attempts+1, note=?, next_at=? WHERE sms_id=? AND target=?`,
			truncate(note, 300), next.Format(time.RFC3339), id, target)
	}
	return err
}

// RequeueFwd queues an SMS for target again (from the web UI), adding the row if needed.
func (d *DB) RequeueFwd(id int64, target string) error {
	var n int
	if err := d.QueryRow(`SELECT count(*) FROM sms_in WHERE id=?`, id).Scan(&n); err != nil || n == 0 {
		return errors.New("no such message")
	}
	_, err := d.Exec(`INSERT INTO sms_fwd(sms_id,target,status,note) VALUES(?,?,'pending','queued again from the web UI')
		ON CONFLICT(sms_id,target) DO UPDATE SET status='pending', attempts=0, next_at='', note='queued again from the web UI'`, id, target)
	return err
}

// fwdFor fills m.Forwards for a page of received SMS.
func (d *DB) fwdFor(list []In) error {
	if len(list) == 0 {
		return nil
	}
	idx := map[int64]int{}
	ph := make([]string, len(list))
	a := make([]any, len(list))
	for i, m := range list {
		idx[m.ID], ph[i], a[i] = i, "?", m.ID
	}
	rows, err := d.Query(`SELECT sms_id,target,status,attempts,next_at,note,done_at FROM sms_fwd WHERE sms_id IN (`+strings.Join(ph, ",")+`) ORDER BY target`, a...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var f Fwd
		if err := rows.Scan(&f.SMSID, &f.Target, &f.Status, &f.Attempts, &f.NextAt, &f.Note, &f.DoneAt); err != nil {
			return err
		}
		i := idx[f.SMSID]
		list[i].Forwards = append(list[i].Forwards, f)
	}
	return rows.Err()
}

// ---------------------------------------------------------------- outgoing

type Out struct {
	ID            int64  `json:"id"`
	Ref           string `json:"ref"`
	CreatedAt     string `json:"created_at"`
	SentAt        string `json:"sent_at"`
	Recipient     string `json:"recipient"`
	Text          string `json:"text"`
	Chars         int    `json:"chars"`
	Channel       string `json:"channel"`
	APIKey        string `json:"api_key"`
	ClientIP      string `json:"client_ip"`
	MessageSource string `json:"message_source"`
	Status        string `json:"status"`
	Error         string `json:"error"`
	IdemKey       string `json:"idempotency_key,omitempty"`
	MsgRef        int    `json:"-"`
	Delivery      string `json:"delivery"` // '' (not requested) | requested | delivered | failed
	DeliveryAt    string `json:"delivery_at,omitempty"`
	DeliveryNote  string `json:"delivery_note,omitempty"`
}

const outCols = `id,ref,created_at,sent_at,recipient,text,chars,channel,api_key,client_ip,message_source,status,error,idem_key,msg_ref,delivery,delivery_at,delivery_note`

func scanOut(rows *sql.Rows) ([]Out, error) {
	defer rows.Close()
	res := []Out{}
	for rows.Next() {
		var m Out
		if err := rows.Scan(&m.ID, &m.Ref, &m.CreatedAt, &m.SentAt, &m.Recipient, &m.Text, &m.Chars, &m.Channel, &m.APIKey, &m.ClientIP, &m.MessageSource, &m.Status, &m.Error,
			&m.IdemKey, &m.MsgRef, &m.Delivery, &m.DeliveryAt, &m.DeliveryNote); err != nil {
			return nil, err
		}
		res = append(res, m)
	}
	return res, rows.Err()
}

// ErrDuplicate: AddOut found an existing row with the same API key and idempotency key.
var ErrDuplicate = errors.New("duplicate idempotency key")

// AddOut records an SMS about to be sent (status "sending" unless set, e.g. "queued").
func (d *DB) AddOut(o Out) error {
	if o.CreatedAt == "" {
		o.CreatedAt = Now()
	}
	if o.Status == "" {
		o.Status = "sending"
	}
	r, err := d.Exec(`INSERT INTO sms_out(ref,created_at,sent_at,recipient,text,chars,channel,api_key,client_ip,message_source,status,error,idem_key,delivery)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`,
		o.Ref, o.CreatedAt, o.SentAt, o.Recipient, o.Text, o.Chars, o.Channel, o.APIKey, o.ClientIP, o.MessageSource, o.Status, o.Error,
		o.IdemKey, o.Delivery)
	if err == nil && o.IdemKey != "" {
		if n, _ := r.RowsAffected(); n == 0 {
			return ErrDuplicate
		}
	}
	return err
}

// GetOut returns one outgoing SMS by its ref.
func (d *DB) GetOut(ref string) (Out, bool, error) {
	l, err := d.queryOut(`SELECT `+outCols+` FROM sms_out WHERE ref=?`, ref)
	if err != nil || len(l) == 0 {
		return Out{}, false, err
	}
	return l[0], true, nil
}

// ByIdemKey finds the SMS an API key sent with this Idempotency-Key.
func (d *DB) ByIdemKey(apiKey, idem string) (Out, bool, error) {
	l, err := d.queryOut(`SELECT `+outCols+` FROM sms_out WHERE api_key=? AND idem_key=?`, apiKey, idem)
	if err != nil || len(l) == 0 {
		return Out{}, false, err
	}
	return l[0], true, nil
}

func (d *DB) queryOut(q string, a ...any) ([]Out, error) {
	rows, err := d.Query(q, a...)
	if err != nil {
		return nil, err
	}
	return scanOut(rows)
}

// SetOutStatus moves an SMS to another status (queued -> sending; failed -> queued for a retry), clearing the error.
func (d *DB) SetOutStatus(ref, from, to string) (bool, error) {
	r, err := d.Exec(`UPDATE sms_out SET status=?, error='' WHERE ref=? AND status=?`, to, ref, from)
	if err != nil {
		return false, err
	}
	n, _ := r.RowsAffected()
	return n == 1, nil
}

// FinishOut records the result of a send: sent, failed, or unknown (smssend.ErrUnknown: the modem timed out).
func (d *DB) FinishOut(ref string, sendErr error) error { return d.FinishOutRef(ref, sendErr, -1) }

// FinishOutRef is FinishOut that also stores the network message reference (for delivery reports).
func (d *DB) FinishOutRef(ref string, sendErr error, msgRef int) error {
	if sendErr != nil {
		st := "failed"
		if errors.Is(sendErr, smssend.ErrUnknown) {
			st = "unknown"
		}
		_, err := d.Exec(`UPDATE sms_out SET status=?, error=?, delivery=CASE WHEN delivery='requested' THEN '' ELSE delivery END WHERE ref=?`,
			st, truncate(sendErr.Error(), 300), ref)
		return err
	}
	// without a message reference a report could never be matched: stop showing "awaiting report"
	_, err := d.Exec(`UPDATE sms_out SET status='sent', sent_at=?, error='', msg_ref=?,
		delivery=CASE WHEN ? < 0 AND delivery='requested' THEN '' ELSE delivery END WHERE ref=?`, Now(), msgRef, msgRef, ref)
	return err
}

// SweepInterrupted runs at startup: SMS of these channels left "queued" were never handed to the modem (failed,
// safe to retry); ones left "sending" may or may not have gone out (unknown). It returns how many rows changed.
func (d *DB) SweepInterrupted(channels ...string) (int, error) {
	if len(channels) == 0 {
		return 0, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(channels)), ",")
	a := make([]any, len(channels))
	for i, c := range channels {
		a[i] = c
	}
	n := 0
	for _, u := range []struct{ from, to, msg string }{
		{"queued", "failed", "not sent: the program restarted before sending (safe to retry)"},
		{"sending", "unknown", "interrupted by a restart while sending: it may or may not have been sent"},
	} {
		r, err := d.Exec(`UPDATE sms_out SET status=?, error=?, delivery=CASE WHEN delivery='requested' THEN '' ELSE delivery END
			WHERE status=? AND channel IN (`+ph+`)`, append([]any{u.to, u.msg, u.from}, a...)...)
		if err != nil {
			return n, err
		}
		k, _ := r.RowsAffected()
		n += int(k)
	}
	return n, nil
}

// Delivery applies a delivery report (status-report SMS) to the latest matching sent SMS: same message reference,
// same recipient (last 10 digits), a report still awaited, sent in the last 3 days. state is ModemManager's
// delivery-state. It returns false when nothing matched.
func (d *DB) Delivery(recipient string, msgRef int, state string) (bool, error) {
	digits := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, recipient)
	if len(digits) < 6 || msgRef < 0 {
		return false, nil
	}
	if len(digits) > 10 {
		digits = digits[len(digits)-10:]
	}
	result := DeliveryResult(state)
	if result == "" { // temporary error: the network keeps trying, keep waiting
		_, err := d.Exec(`UPDATE sms_out SET delivery_note=? WHERE id=(SELECT id FROM sms_out WHERE msg_ref=? AND delivery='requested'
			AND substr(recipient,-10)=? AND created_at >= ? ORDER BY id DESC LIMIT 1)`, state, msgRef, digits, threeDaysAgo())
		return err == nil, err
	}
	r, err := d.Exec(`UPDATE sms_out SET delivery=?, delivery_at=?, delivery_note=? WHERE id=(SELECT id FROM sms_out WHERE msg_ref=?
		AND delivery='requested' AND substr(recipient,-10)=? AND created_at >= ? ORDER BY id DESC LIMIT 1)`,
		result, Now(), state, msgRef, digits, threeDaysAgo())
	if err != nil {
		return false, err
	}
	n, _ := r.RowsAffected()
	return n == 1, nil
}

func threeDaysAgo() string { return time.Now().Add(-72 * time.Hour).Format(time.RFC3339) }

// DeliveryResult maps ModemManager's delivery-state: completed-* = delivered, temporary-error-* = still trying (""),
// anything else (error-*, temporary-fatal-error-*) = failed.
func DeliveryResult(state string) string {
	switch {
	case strings.HasPrefix(state, "completed"):
		return "delivered"
	case strings.HasPrefix(state, "temporary-error"):
		return ""
	}
	return "failed"
}

// ---------------------------------------------------------------- queries

type Query struct {
	Limit   int
	Before  int64  // id < before (paging)
	Search  string // sender/recipient/text/api key substring
	Status  string
	Key     string // outgoing: API key name
	Since   string // RFC3339 lower bound
	Channel string // outgoing: api | web-ui
}

func where(q Query, in bool) (string, []any) {
	var c []string
	var a []any
	if q.Before > 0 {
		c, a = append(c, "id < ?"), append(a, q.Before)
	}
	if s := strings.TrimSpace(q.Search); s != "" {
		s = "%" + s + "%"
		if in {
			c, a = append(c, "(sender LIKE ? OR text LIKE ?)"), append(a, s, s)
		} else {
			c, a = append(c, "(recipient LIKE ? OR text LIKE ? OR api_key LIKE ?)"), append(a, s, s, s)
		}
	}
	if q.Status != "" {
		col := "status"
		if in {
			col = "wa_status"
		}
		c, a = append(c, col+" = ?"), append(a, q.Status)
	}
	if q.Key != "" && !in {
		c, a = append(c, "api_key = ?"), append(a, q.Key)
	}
	if q.Channel != "" && !in {
		c, a = append(c, "channel = ?"), append(a, q.Channel)
	}
	if q.Since != "" {
		col := "created_at"
		if in {
			col = "stored_at"
		}
		c, a = append(c, col+" >= ?"), append(a, q.Since)
	}
	if len(c) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(c, " AND "), a
}

func lim(n int) int {
	if n <= 0 || n > 500 {
		return 100
	}
	return n
}

func (d *DB) ListIn(q Query) ([]In, error) {
	w, a := where(q, true)
	rows, err := d.Query(`SELECT `+inCols+` FROM sms_in`+w+` ORDER BY id DESC LIMIT ?`, append(a, lim(q.Limit))...)
	if err != nil {
		return nil, err
	}
	l, err := scanIn(rows)
	if err == nil {
		err = d.fwdFor(l)
	}
	return l, err
}

func (d *DB) ListOut(q Query) ([]Out, error) {
	w, a := where(q, false)
	rows, err := d.Query(`SELECT `+outCols+` FROM sms_out`+w+` ORDER BY id DESC LIMIT ?`, append(a, lim(q.Limit))...)
	if err != nil {
		return nil, err
	}
	return scanOut(rows)
}

type Stats struct {
	InTotal       int            `json:"in_total"`
	InToday       int            `json:"in_today"`
	OutTotal      int            `json:"out_total"`
	OutToday      int            `json:"out_today"`
	OutFailedDay  int            `json:"out_failed_today"`
	WAByStatus    map[string]int `json:"wa_by_status"`
	APIToday      map[string]int `json:"api_today"` // per key, today
	SentTodayAPI  int            `json:"api_sent_today"`
	FailedTodayAP int            `json:"api_failed_today"`
}

// DayStart is local midnight in RFC3339, for "today" counters.
func DayStart(t time.Time) string {
	y, m, dd := t.Local().Date()
	return time.Date(y, m, dd, 0, 0, 0, 0, time.Local).Format(time.RFC3339)
}

func (d *DB) Stats() (Stats, error) {
	s := Stats{WAByStatus: map[string]int{}, APIToday: map[string]int{}}
	day := DayStart(time.Now())
	q := func(dst *int, sql string, a ...any) error { return d.QueryRow(sql, a...).Scan(dst) }
	for _, e := range []error{
		q(&s.InTotal, `SELECT count(*) FROM sms_in`),
		q(&s.InToday, `SELECT count(*) FROM sms_in WHERE stored_at >= ?`, day),
		q(&s.OutTotal, `SELECT count(*) FROM sms_out`),
		q(&s.OutToday, `SELECT count(*) FROM sms_out WHERE created_at >= ?`, day),
		q(&s.OutFailedDay, `SELECT count(*) FROM sms_out WHERE created_at >= ? AND status='failed'`, day),
		q(&s.SentTodayAPI, `SELECT count(*) FROM sms_out WHERE created_at >= ? AND channel='api' AND status='sent'`, day),
		q(&s.FailedTodayAP, `SELECT count(*) FROM sms_out WHERE created_at >= ? AND channel='api' AND status='failed'`, day),
	} {
		if e != nil {
			return s, e
		}
	}
	rows, err := d.Query(`SELECT wa_status, count(*) FROM sms_in GROUP BY wa_status`)
	if err != nil {
		return s, err
	}
	for rows.Next() {
		var k string
		var n int
		rows.Scan(&k, &n)
		s.WAByStatus[k] = n
	}
	rows.Close()
	rows, err = d.Query(`SELECT api_key, count(*) FROM sms_out WHERE channel='api' AND created_at >= ? GROUP BY api_key`, day)
	if err != nil {
		return s, err
	}
	for rows.Next() {
		var k string
		var n int
		rows.Scan(&k, &n)
		s.APIToday[k] = n
	}
	rows.Close()
	return s, nil
}

// APIUsageSince counts API sends (any status) since t, per key and in total.
func (d *DB) APIUsageSince(since string) (map[string]int, int, error) {
	rows, err := d.Query(`SELECT api_key, count(*) FROM sms_out WHERE channel='api' AND created_at >= ? GROUP BY api_key`, since)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	m, total := map[string]int{}, 0
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			return nil, 0, err
		}
		m[k] = n
		total += n
	}
	return m, total, rows.Err()
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
