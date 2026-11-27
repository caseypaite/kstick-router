package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWAJIDAndRender(t *testing.T) {
	for in, want := range map[string]string{"9876543210": "919876543210@c.us", "447911123456": "447911123456@c.us", "120363000000000000@g.us": "120363000000000000@g.us"} {
		if g := waJID(in); g != want {
			t.Errorf("waJID(%s)=%s want %s", in, g, want)
		}
	}
	m := rxSMS{from: "VM-HDFCBK", ts: "2026-11-21T19:59:25+05:30", text: "OTP 123456"}
	got := renderWA("From {from} at {time}: {text} ({device})", m)
	if !strings.Contains(got, "From VM-HDFCBK at 21 Nov 2026") || !strings.HasSuffix(got, ": OTP 123456 (KStick)") {
		t.Fatalf("render: %q", got)
	}
	if !ignoredSender(WawatCfg{Ignore: []string{"ad-"}}, "AD-650025-P") || ignoredSender(WawatCfg{Ignore: []string{"ad-"}}, "VM-HDFCBK") {
		t.Fatal("ignore list")
	}
}

func TestWACall(t *testing.T) {
	var got map[string]any
	var key, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, path = r.Header.Get("x-api-key"), r.URL.Path
		json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("X-Wawat-Auth-Api-Key-Name", "kstick")
		if r.URL.Path == "/api/v1/status" {
			w.Write([]byte(`{"status":"CONNECTED","ready":true,"me":{"wid":{"user":"919999999999"}}}`))
			return
		}
		if got["message"] == "busy" {
			w.Header().Set("Retry-After", "5")
			w.WriteHeader(503)
			w.Write([]byte(`{"error":"not ready"}`))
			return
		}
		w.Write([]byte(`{"success":true,"messageId":"x"}`))
	}))
	defer srv.Close()
	c := WawatCfg{ServerURL: srv.URL, APIKey: "tok", Target: "9876543210"}
	if id, err := waSend(c, "test", "hello", waLog{}); err != nil || id != "x" || key != "tok" || path != "/api/v1/message/single" || got["number"] != "9876543210" || got["message"] != "hello" {
		t.Fatalf("send: %v key=%s path=%s body=%v", err, key, path, got)
	}
	c.Target = "120363000000000000@g.us"
	if _, err := waSend(c, "test", "hi", waLog{}); err != nil || path != "/api/v1/message/group" || got["groupId"] != c.Target {
		t.Fatalf("group: %v %s %v", err, path, got)
	}
	if _, err := waSend(c, "test", "busy", waLog{}); err == nil || !strings.Contains(err.Error(), "not ready") {
		t.Fatalf("503: %v", err)
	}
	m, e, err := waCall(c, "verify", "GET", "/api/v1/status", nil, waLog{})
	if err != nil || m["ready"] != true || e.Headers["api-key-name"] != "kstick" {
		t.Fatalf("verify: %v %v %v", err, m, e.Headers)
	}
	for _, l := range waLogs {
		if strings.Contains(l.Resp+l.Error, "tok") {
			t.Fatal("token leaked into the debug log")
		}
	}
}
