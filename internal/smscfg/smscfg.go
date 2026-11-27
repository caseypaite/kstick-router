// Package smscfg is the configuration and send log shared by the public SMS
// gateway (kstick-smsgw) and the router web UI that administers it.
package smscfg

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	Path    = "/etc/smsgw/config.json" // root:smsgw 0640, written by router-webui
	LogPath = "/var/lib/smsgw/sent.jsonl"
	LogMax  = 1 << 20 // rotate to .1 above 1 MiB
)

type Key struct {
	Name    string   `json:"name"`
	Hash    string   `json:"hash"`   // sha256 of the full key, hex
	Prefix  string   `json:"prefix"` // first characters, for recognition only
	Created string   `json:"created"`
	Scopes  []string `json:"scopes,omitempty"` // "send", "inbox"; empty = send only (keys from before scopes existed)
}

// Scopes an API key can have. Reading the inbox exposes received SMS (one-time codes): opt-in per key.
const (
	ScopeSend  = "send"
	ScopeInbox = "inbox"
)

// Can reports whether the key has the scope.
func (k Key) Can(scope string) bool {
	if len(k.Scopes) == 0 {
		return scope == ScopeSend
	}
	for _, s := range k.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// EffectiveScopes is Scopes with the default applied.
func (k Key) EffectiveScopes() []string {
	if len(k.Scopes) == 0 {
		return []string{ScopeSend}
	}
	return k.Scopes
}

// Limits: 0 means "no limit".
type Limits struct {
	PerKeyPerMinute  int `json:"per_key_per_minute"`
	PerKeyPerDay     int `json:"per_key_per_day"`
	PerNumberPerHour int `json:"per_number_per_hour"`
	GlobalPerDay     int `json:"global_per_day"`
}

type Config struct {
	Enabled        bool   `json:"enabled"`
	DefaultMessage string `json:"default_message"`
	MaxLength      int    `json:"max_length"`
	PublicURL      string `json:"public_url"`
	Limits         Limits `json:"limits"`
	Keys           []Key  `json:"keys"`
	// DeliveryReports asks the network for a delivery report for every API send (shown by GET /api/v1/sms/{id}).
	DeliveryReports bool `json:"delivery_reports"`
}

func Default() Config {
	return Config{
		Enabled:   false,
		MaxLength: 480,
		PublicURL: "",
		Limits:    Limits{PerKeyPerMinute: 5, PerKeyPerDay: 100, PerNumberPerHour: 3, GlobalPerDay: 200},
		Keys:      []Key{},

		DeliveryReports: true,
	}
}

// Load returns the defaults when the file does not exist yet.
func Load(path string) (Config, error) {
	c := Default()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return Default(), fmt.Errorf("%s: %w", path, err)
	}
	if c.Keys == nil {
		c.Keys = []Key{}
	}
	return c, c.Validate()
}

var (
	reKeyName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 _.-]{0,31}$`)
	reHash    = regexp.MustCompile(`^[0-9a-f]{64}$`)
	reNumber  = regexp.MustCompile(`^[6-9][0-9]{9}$`)
	reURL     = regexp.MustCompile(`^https://[a-z0-9.-]{1,253}(/[A-Za-z0-9._~/-]*)?$`)
)

func (c *Config) Validate() error {
	if c.MaxLength < 1 || c.MaxLength > 1000 {
		return errors.New("max length must be 1-1000 characters")
	}
	if n := utf8.RuneCountInString(c.DefaultMessage); n > c.MaxLength {
		return fmt.Errorf("default message is %d characters; the maximum is %d", n, c.MaxLength)
	}
	if c.DefaultMessage != CleanMessage(c.DefaultMessage) {
		return errors.New("default message contains control characters")
	}
	if c.PublicURL != "" && !reURL.MatchString(c.PublicURL) {
		return errors.New("public URL must look like https://smsgw.example.com")
	}
	for _, v := range []int{c.Limits.PerKeyPerMinute, c.Limits.PerKeyPerDay, c.Limits.PerNumberPerHour, c.Limits.GlobalPerDay} {
		if v < 0 || v > 100000 {
			return errors.New("limits must be between 0 (no limit) and 100000")
		}
	}
	seen := map[string]bool{}
	for _, k := range c.Keys {
		if !reKeyName.MatchString(k.Name) || !reHash.MatchString(k.Hash) {
			return fmt.Errorf("invalid API key entry %q", k.Name)
		}
		if len(k.Scopes) > 2 {
			return fmt.Errorf("API key %q: too many scopes", k.Name)
		}
		for _, sc := range k.Scopes {
			if sc != ScopeSend && sc != ScopeInbox {
				return fmt.Errorf("API key %q: unknown scope %q", k.Name, sc)
			}
		}
		if seen[strings.ToLower(k.Name)] {
			return fmt.Errorf("duplicate API key name %q", k.Name)
		}
		seen[strings.ToLower(k.Name)] = true
	}
	return nil
}

func ValidKeyName(s string) bool { return reKeyName.MatchString(s) }

// ValidNumber: exactly 10 digits, an Indian mobile number (starts with 6-9).
func ValidNumber(s string) bool { return reNumber.MatchString(s) }

// E164 turns a validated 10-digit Indian mobile number into +91XXXXXXXXXX.
func E164(s string) string { return "+91" + s }

func HashKey(k string) string {
	h := sha256.Sum256([]byte(k))
	return hex.EncodeToString(h[:])
}

// NewKey returns a random API key ("ksms_" + 43 base64url chars, 256 bits).
func NewKey() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return "ksms_" + base64.RawURLEncoding.EncodeToString(b)
}

// CleanMessage drops carriage returns and control characters other than newline/tab.
func CleanMessage(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) || r == utf8.RuneError {
			return -1
		}
		return r
	}, s)
}

type LogEntry struct {
	Time   string `json:"time"`
	ID     string `json:"id"`
	Key    string `json:"key"`
	IP     string `json:"ip"`
	To     string `json:"to"`
	Source string `json:"source"` // request | default
	Chars  int    `json:"chars"`
	Text   string `json:"text"`   // first 160 characters
	Status string `json:"status"` // sent | failed
	Error  string `json:"error,omitempty"`
}

// ReadLog returns up to n most recent entries (newest last), including the rotated file.
func ReadLog(path string, n int) []LogEntry {
	var all []LogEntry
	for _, p := range []string{path + ".1", path} {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64<<10), 64<<10)
		for sc.Scan() {
			var e LogEntry
			if json.Unmarshal(sc.Bytes(), &e) == nil {
				all = append(all, e)
			}
		}
		f.Close()
	}
	if n > 0 && len(all) > n {
		all = all[len(all)-n:]
	}
	return all
}

// Today is the local calendar day used for daily limits, e.g. "2026-11-21".
func Today(t time.Time) string { return t.Local().Format("2006-01-02") }
