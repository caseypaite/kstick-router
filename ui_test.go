package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The served UI follows the device setting, a browser's ?ui= cookie overrides it, and a style whose
// files are missing is never served.
func TestWebHandlerStyles(t *testing.T) {
	root := t.TempDir()
	classic, xp := filepath.Join(root, "web"), filepath.Join(root, "web-xp")
	for d, body := range map[string]string{classic: "CLASSIC", xp: "XP"} {
		os.MkdirAll(d, 0o755)
		os.WriteFile(filepath.Join(d, "index.html"), []byte(body), 0o644)
	}
	oldDir, oldXP := *flagWebDir, *flagWebDirXP
	defer func() { *flagWebDir, *flagWebDirXP = oldDir, oldXP }()
	*flagWebDir, *flagWebDirXP = classic, ""
	cfgMu.Lock()
	prev := cfg
	cfgMu.Unlock()
	defer func() { cfgMu.Lock(); cfg = prev; cfgMu.Unlock() }()
	setStyle := func(s string) { cfgMu.Lock(); cfg.UIStyle = s; cfgMu.Unlock() }

	h := webHandler()
	get := func(path string, cookie string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		if cookie != "" {
			r.AddCookie(&http.Cookie{Name: uiCookie, Value: cookie})
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	body := func(w *httptest.ResponseRecorder) string { return strings.TrimSpace(w.Body.String()) }

	setStyle("")
	if b := body(get("/", "")); b != "CLASSIC" {
		t.Fatalf("default: got %q", b)
	}
	setStyle("xp")
	if b := body(get("/", "")); b != "XP" {
		t.Fatalf("device xp: got %q", b)
	}
	if b := body(get("/", "classic")); b != "CLASSIC" {
		t.Fatalf("cookie classic over device xp: got %q", b)
	}
	if b := body(get("/", "bogus")); b != "XP" {
		t.Fatalf("bad cookie ignored: got %q", b)
	}

	w := get("/?ui=classic", "")
	if w.Code != http.StatusSeeOther || !strings.Contains(w.Header().Get("Set-Cookie"), uiCookie+"=classic") {
		t.Fatalf("?ui=classic: %d %q", w.Code, w.Header().Get("Set-Cookie"))
	}
	w = get("/?ui=default", "classic")
	if w.Code != http.StatusSeeOther || !strings.Contains(w.Header().Get("Set-Cookie"), "Max-Age=0") {
		t.Fatalf("?ui=default: %d %q", w.Code, w.Header().Get("Set-Cookie"))
	}
	if w = get("/?ui=../etc", ""); w.Code != http.StatusNotFound {
		t.Fatalf("?ui=../etc: %d", w.Code)
	}

	// XP files gone: the device setting falls back to classic, and ?ui=xp is refused
	os.Remove(filepath.Join(xp, "index.html"))
	if b := body(get("/", "xp")); b != "CLASSIC" {
		t.Fatalf("xp missing: got %q", b)
	}
	if w = get("/?ui=xp", ""); w.Code != http.StatusNotFound {
		t.Fatalf("?ui=xp without files: %d", w.Code)
	}
	if _, err := apiPutUI(httptest.NewRequest("PUT", "/api/ui", strings.NewReader(`{"style":"xp"}`))); err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("PUT xp without files: %v", err)
	}
}
