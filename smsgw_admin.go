package main

// Administration of the public SMS gateway (kstick-smsgw): the router web UI
// (LAN/USB/Mesh only) owns /etc/smsgw/config.json; the gateway reloads it.

import (
	"encoding/json"
	"net/http"
	"os"
	"os/user"
	"strconv"
	"strings"
	"sync"
	"time"

	"kstick-router/internal/smscfg"
	"kstick-router/internal/smsdb"
)

var smsMu sync.Mutex

func saveSMSCfg(c smscfg.Config) error {
	if err := c.Validate(); err != nil {
		return badRequest("%s", err.Error())
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	if err := os.MkdirAll("/etc/smsgw", 0750); err != nil {
		return err
	}
	if err := writeAtomic(smscfg.Path, append(b, '\n'), 0640); err != nil {
		return err
	}
	if g, err := user.LookupGroup("smsgw"); err == nil { // gateway reads it as group smsgw
		if gid, err := strconv.Atoi(g.Gid); err == nil {
			os.Chown(smscfg.Path, 0, gid)
			os.Chown("/etc/smsgw", 0, gid)
		}
	}
	return nil
}

type smsKeyView struct {
	Name    string   `json:"name"`
	Prefix  string   `json:"prefix"`
	Created string   `json:"created"`
	Scopes  []string `json:"scopes"`
}

// cleanScopes validates the permissions chosen for a key (at least one).
func cleanScopes(in []string) ([]string, error) {
	var out []string
	for _, sc := range []string{smscfg.ScopeSend, smscfg.ScopeInbox} {
		for _, v := range in {
			if v == sc {
				out = append(out, sc)
				break
			}
		}
	}
	if len(out) == 0 || len(out) != len(in) {
		return nil, badRequest("permissions: choose send and/or inbox")
	}
	return out, nil
}

func apiGetSMSGW(r *http.Request) (any, error) {
	c, err := smscfg.Load(smscfg.Path)
	if err != nil {
		return nil, err
	}
	keys := []smsKeyView{}
	for _, k := range c.Keys {
		keys = append(keys, smsKeyView{k.Name, k.Prefix, k.Created, k.EffectiveScopes()})
	}
	recent, today := []smsdb.Out{}, map[string]any{"sent": 0, "failed": 0, "per_key": map[string]int{}}
	if d := smsDB(); d != nil {
		if l, err := d.ListOut(smsdb.Query{Channel: "api", Limit: 100}); err == nil {
			recent = l
		}
		if st, err := d.Stats(); err == nil {
			today = map[string]any{"sent": st.SentTodayAPI, "failed": st.FailedTodayAP, "per_key": st.APIToday}
		}
	}
	svc := func(u string) string {
		out, _ := run(5*time.Second, "systemctl", "is-active", u)
		return strings.TrimSpace(out)
	}
	c.Keys = nil
	return map[string]any{
		"config": c, "keys": keys, "log": recent,
		"today":    today,
		"services": map[string]string{"smsgw": svc("kstick-smsgw"), "cloudflared": svc("cloudflared")},
	}, nil
}

func apiPutSMSGW(r *http.Request) (any, error) {
	var b struct {
		Enabled        bool          `json:"enabled"`
		DefaultMessage string        `json:"default_message"`
		MaxLength      int           `json:"max_length"`
		PublicURL      string        `json:"public_url"`
		Limits         smscfg.Limits `json:"limits"`
		DeliveryReport *bool         `json:"delivery_reports"` // nil: keep (older web UI)
	}
	if err := readJSON(r, &b); err != nil {
		return nil, err
	}
	smsMu.Lock()
	defer smsMu.Unlock()
	c, err := smscfg.Load(smscfg.Path)
	if err != nil {
		return nil, err
	}
	c.Enabled, c.DefaultMessage, c.MaxLength, c.PublicURL, c.Limits =
		b.Enabled, smscfg.CleanMessage(b.DefaultMessage), b.MaxLength, strings.TrimSpace(b.PublicURL), b.Limits
	if b.DeliveryReport != nil {
		c.DeliveryReports = *b.DeliveryReport
	}
	if err := saveSMSCfg(c); err != nil {
		return nil, err
	}
	audit(r, "SMS gateway settings updated (enabled=%v)", c.Enabled)
	return map[string]string{"message": "SMS gateway settings saved."}, nil
}

func apiCreateSMSKey(r *http.Request) (any, error) {
	var b struct {
		Name   string   `json:"name"`
		Scopes []string `json:"scopes"` // default: send only
	}
	if err := readJSON(r, &b); err != nil {
		return nil, err
	}
	if len(b.Scopes) == 0 {
		b.Scopes = []string{smscfg.ScopeSend}
	}
	scopes, err := cleanScopes(b.Scopes)
	if err != nil {
		return nil, err
	}
	b.Name = strings.TrimSpace(b.Name)
	if b.Name == "" { // generated below once the existing names are known
		b.Name = "-"
	}
	if b.Name != "-" && !smscfg.ValidKeyName(b.Name) {
		return nil, badRequest("key name: 1-32 letters, digits, spaces, '.', '_' or '-'")
	}
	smsMu.Lock()
	defer smsMu.Unlock()
	c, err := smscfg.Load(smscfg.Path)
	if err != nil {
		return nil, err
	}
	taken := func(n string) bool {
		for _, k := range c.Keys {
			if strings.EqualFold(k.Name, n) {
				return true
			}
		}
		return false
	}
	if b.Name == "-" {
		b.Name = smscfg.RandomKeyName(taken)
	} else if taken(b.Name) {
		return nil, badRequest("a key named %q already exists", b.Name)
	}
	if len(c.Keys) >= 50 {
		return nil, badRequest("too many API keys (50)")
	}
	key := smscfg.NewKey()
	c.Keys = append(c.Keys, smscfg.Key{Name: b.Name, Hash: smscfg.HashKey(key), Prefix: key[:9], Created: time.Now().Format(time.RFC3339),
		Scopes: scopes})
	if err := saveSMSCfg(c); err != nil {
		return nil, err
	}
	audit(r, "SMS gateway API key %q created (%s)", b.Name, strings.Join(scopes, ", "))
	return map[string]string{"name": b.Name, "key": key}, nil
}

func apiDeleteSMSKey(r *http.Request) (any, error) {
	name := r.PathValue("name")
	smsMu.Lock()
	defer smsMu.Unlock()
	c, err := smscfg.Load(smscfg.Path)
	if err != nil {
		return nil, err
	}
	keep := []smscfg.Key{}
	for _, k := range c.Keys {
		if k.Name != name {
			keep = append(keep, k)
		}
	}
	if len(keep) == len(c.Keys) {
		return nil, &httpError{404, "no such key"}
	}
	c.Keys = keep
	if err := saveSMSCfg(c); err != nil {
		return nil, err
	}
	audit(r, "SMS gateway API key %q revoked", name)
	return map[string]string{"message": "Key revoked."}, nil
}

// apiPutSMSKey changes a key's permissions (send, inbox).
func apiPutSMSKey(r *http.Request) (any, error) {
	var b struct {
		Scopes []string `json:"scopes"`
	}
	if err := readJSON(r, &b); err != nil {
		return nil, err
	}
	scopes, err := cleanScopes(b.Scopes)
	if err != nil {
		return nil, err
	}
	name := r.PathValue("name")
	smsMu.Lock()
	defer smsMu.Unlock()
	c, err := smscfg.Load(smscfg.Path)
	if err != nil {
		return nil, err
	}
	found := false
	for i := range c.Keys {
		if c.Keys[i].Name == name {
			c.Keys[i].Scopes, found = scopes, true
		}
	}
	if !found {
		return nil, &httpError{404, "no such key"}
	}
	if err := saveSMSCfg(c); err != nil {
		return nil, err
	}
	audit(r, "SMS gateway API key %q permissions: %s", name, strings.Join(scopes, ", "))
	return map[string]string{"message": "Permissions saved."}, nil
}

// apiSuggestSMSKeyName proposes a random three-word name for a new key.
func apiSuggestSMSKeyName(r *http.Request) (any, error) {
	c, err := smscfg.Load(smscfg.Path)
	if err != nil {
		return nil, err
	}
	return map[string]string{"name": smscfg.RandomKeyName(func(n string) bool {
		for _, k := range c.Keys {
			if strings.EqualFold(k.Name, n) {
				return true
			}
		}
		return false
	})}, nil
}
