package main

import (
	"flag"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
)

// Two web UIs share the same API: "classic" (web/, the default) and "xp" (web-xp/, a Windows-XP-style
// desktop built from xp/src). Config.UIStyle picks the one the device serves; a browser can override it
// for itself with /?ui=classic|xp (a cookie; /?ui=default drops it) — the way back if one UI is broken.

var flagWebDirXP = flag.String("web-dir-xp", "", "directory with the XP-style UI files (default: -web-dir + \"-xp\")")

const uiCookie = "router_ui"

var uiStyles = []string{"classic", "xp"}

func validUIStyle(s string) bool { return s == "classic" || s == "xp" }

func uiDir(style string) string {
	if style == "xp" {
		if *flagWebDirXP != "" {
			return *flagWebDirXP
		}
		if *flagWebDir == "" {
			return ""
		}
		return filepath.Clean(*flagWebDir) + "-xp"
	}
	return *flagWebDir
}

// uiInstalled: the style's files are on disk (an index.html is enough to serve it).
func uiInstalled(style string) bool {
	d := uiDir(style)
	if d == "" {
		return false
	}
	st, err := os.Stat(filepath.Join(d, "index.html"))
	return err == nil && st.Mode().IsRegular()
}

// uiStyleFor: the browser's own choice if it is installed, else the device setting, else classic,
// else whatever is installed.
func uiStyleFor(r *http.Request) string {
	if c, err := r.Cookie(uiCookie); err == nil && validUIStyle(c.Value) && uiInstalled(c.Value) {
		return c.Value
	}
	if s := snapshot().UIStyle; validUIStyle(s) && uiInstalled(s) {
		return s
	}
	for _, s := range uiStyles {
		if uiInstalled(s) {
			return s
		}
	}
	return ""
}

var (
	uiServersMu sync.Mutex
	uiServers   = map[string]http.Handler{}
	uiLogged    = map[string]bool{}
)

func uiServer(style string) http.Handler {
	uiServersMu.Lock()
	defer uiServersMu.Unlock()
	if style == "" {
		if !uiLogged[""] {
			log.Printf("web UI: no index.html in %q or %q, serving the built-in \"UI files missing\" page", uiDir("classic"), uiDir("xp"))
			uiLogged[""] = true
		}
		static, _ := fs.Sub(webFS, "webfallback")
		return http.FileServerFS(static)
	}
	h, ok := uiServers[style]
	if !ok {
		log.Printf("web UI: %s style from %s", style, uiDir(style))
		h = http.FileServerFS(os.DirFS(uiDir(style)))
		uiServers[style] = h
	}
	return h
}

// webHandler serves the static UI files of the style chosen for this request.
func webHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if v := r.URL.Query().Get("ui"); v != "" && (r.URL.Path == "/" || r.URL.Path == "/index.html") {
			c := &http.Cookie{Name: uiCookie, Path: "/", HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode}
			switch {
			case validUIStyle(v) && uiInstalled(v):
				c.Value, c.MaxAge = v, 400*24*3600
			case v == "default":
				c.MaxAge = -1
			default:
				http.Error(w, "unknown or not installed UI style", http.StatusNotFound)
				return
			}
			http.SetCookie(w, c)
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		uiServer(uiStyleFor(r)).ServeHTTP(w, r)
	})
}

// GET /api/ui: the device's style, this browser's override and what is installed.
func apiGetUI(r *http.Request) (any, error) {
	var avail []string
	for _, s := range uiStyles {
		if uiInstalled(s) {
			avail = append(avail, s)
		}
	}
	cur := snapshot().UIStyle
	if !validUIStyle(cur) {
		cur = "classic"
	}
	override := ""
	if c, err := r.Cookie(uiCookie); err == nil && validUIStyle(c.Value) {
		override = c.Value
	}
	return map[string]any{"style": cur, "browser": override, "serving": uiStyleFor(r), "available": avail}, nil
}

// PUT /api/ui {"style": "classic"|"xp"}: the device-wide style. Takes effect on the next page load.
func apiPutUI(r *http.Request) (any, error) {
	var b struct{ Style string }
	if err := readJSON(r, &b); err != nil {
		return nil, err
	}
	if !validUIStyle(b.Style) {
		return nil, badRequest("style must be classic or xp")
	}
	if !uiInstalled(b.Style) {
		return nil, badRequest("the %s UI files are not installed (%s)", b.Style, uiDir(b.Style))
	}
	return updateCfg(r, "web UI style", func(c *Config) { c.UIStyle = b.Style })
}
