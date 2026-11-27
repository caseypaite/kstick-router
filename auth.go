package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const (
	cookieName    = "osr_session"
	sessionIdle   = 30 * time.Minute
	sessionMax    = 12 * time.Hour
	maxFails      = 5
	lockoutPeriod = 5 * time.Minute
)

type session struct {
	user     string
	csrf     string
	created  time.Time
	lastSeen time.Time
}

type failInfo struct {
	count int
	until time.Time
}

var (
	sessMu   sync.Mutex
	sessions = map[string]*session{}
	fails    = map[string]*failInfo{}
)

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// checkPassword verifies a system account password through PAM's unix_chkpwd helper,
// so the shadow hash (yescrypt) is handled by libxcrypt exactly as for console login.
func checkPassword(user, pw string) bool {
	if pw == "" || len(pw) > 512 || strings.ContainsAny(pw, "\x00\n") {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/sbin/unix_chkpwd", user, "nonull")
	cmd.Stdin = strings.NewReader(pw + "\x00")
	return cmd.Run() == nil
}

func lockedOut(ip string) (bool, time.Duration) {
	sessMu.Lock()
	defer sessMu.Unlock()
	f := fails[ip]
	if f != nil && time.Now().Before(f.until) {
		return true, time.Until(f.until).Round(time.Second)
	}
	return false, 0
}

func recordFail(ip string) {
	sessMu.Lock()
	defer sessMu.Unlock()
	f := fails[ip]
	if f == nil || (f.count >= maxFails && time.Now().After(f.until)) {
		f = &failInfo{}
		fails[ip] = f
	}
	f.count++
	if f.count >= maxFails {
		f.until = time.Now().Add(lockoutPeriod)
	}
}

func handleLogin(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if locked, left := lockedOut(ip); locked {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many failed attempts, try again in " + left.String()})
		return
	}
	var body struct{ Username, Password string }
	if err := readJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	user := strings.TrimSpace(body.Username)
	if user != *flagUser || !checkPassword(user, body.Password) {
		recordFail(ip)
		time.Sleep(700 * time.Millisecond) // slow down guessing
		auditAnon(ip, "login FAILED for %q", user)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid username or password"})
		return
	}
	sessMu.Lock()
	delete(fails, ip)
	tok := randHex(32)
	s := &session{user: user, csrf: randHex(32), created: time.Now(), lastSeen: time.Now()}
	sessions[tok] = s
	sessMu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: tok, Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	})
	auditAnon(ip, "login ok for %q", user)
	writeJSON(w, http.StatusOK, map[string]string{"user": user, "csrf": s.csrf})
}

func handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil {
		sessMu.Lock()
		delete(sessions, c.Value)
		sessMu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func lookupSession(r *http.Request) *session {
	c, err := r.Cookie(cookieName)
	if err != nil || len(c.Value) != 64 {
		return nil
	}
	sessMu.Lock()
	defer sessMu.Unlock()
	s := sessions[c.Value]
	if s == nil {
		return nil
	}
	now := time.Now()
	if now.Sub(s.lastSeen) > sessionIdle || now.Sub(s.created) > sessionMax {
		delete(sessions, c.Value)
		return nil
	}
	s.lastSeen = now
	return s
}

func handleSession(w http.ResponseWriter, r *http.Request) {
	s := lookupSession(r)
	if s == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "not logged in"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"user": s.user, "csrf": s.csrf})
}

type ctxKey struct{}

// auth requires a valid session; state-changing requests also need the CSRF token header.
func auth(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := lookupSession(r)
		if s == nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "not logged in"})
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			tok := r.Header.Get("X-CSRF-Token")
			if subtle.ConstantTimeCompare([]byte(tok), []byte(s.csrf)) != 1 {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "missing or invalid CSRF token"})
				return
			}
		}
		h(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, s.user)))
	})
}

func currentUser(r *http.Request) string {
	if u, ok := r.Context().Value(ctxKey{}).(string); ok {
		return u
	}
	return "-"
}

// dropOtherSessions logs out everyone except the given request's session (after a password change).
func dropOtherSessions(r *http.Request) {
	c, _ := r.Cookie(cookieName)
	sessMu.Lock()
	defer sessMu.Unlock()
	for k := range sessions {
		if c == nil || k != c.Value {
			delete(sessions, k)
		}
	}
}

// janitor periodically forgets expired sessions and stale failure records.
func init() {
	go func() {
		for range time.Tick(5 * time.Minute) {
			now := time.Now()
			sessMu.Lock()
			for k, s := range sessions {
				if now.Sub(s.lastSeen) > sessionIdle || now.Sub(s.created) > sessionMax {
					delete(sessions, k)
				}
			}
			for k, f := range fails {
				if now.After(f.until) && now.Sub(f.until) > time.Hour {
					delete(fails, k)
				}
			}
			sessMu.Unlock()
		}
	}()
}
