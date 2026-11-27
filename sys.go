package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"time"
)

// run executes a command (never through a shell) and returns stdout.
func run(timeout time.Duration, name string, args ...string) (string, error) {
	return runStdin(timeout, "", name, args...)
}

func runStdin(timeout time.Duration, stdin, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return out.String(), fmt.Errorf("%s: timed out after %s", name, timeout)
	}
	if err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = strings.TrimSpace(out.String())
		}
		if len(msg) > 600 {
			msg = msg[:600] + "…"
		}
		return out.String(), fmt.Errorf("%s: %s", name, firstNonEmpty(msg, err.Error()))
	}
	return out.String(), nil
}

// runCombined returns stdout+stderr together (for diagnostics tools).
func runCombined(timeout time.Duration, name string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	s := string(out)
	if ctx.Err() == context.DeadlineExceeded {
		s += "\n[timed out]"
	} else if err != nil && len(out) == 0 {
		s = err.Error()
	}
	return s
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

func readTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// writeAtomic writes a file via a temp file + rename so readers never see partial content. The data and the
// directory entry are fsynced: the stick can lose power or reset hard (watchdog, battery), and without the syncs
// ext4 may come back with an empty or old file.
func writeAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Chmod(mode) // OpenFile's mode only applies to new files (and is masked by umask)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}

// ---- HTTP helpers ----

type httpError struct {
	code int
	msg  string
}

func (e *httpError) Error() string { return e.msg }

func badRequest(format string, a ...any) error {
	return &httpError{http.StatusBadRequest, fmt.Sprintf(format, a...)}
}

type apiFunc func(r *http.Request) (any, error)

func jsonAPI(f apiFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v, err := f(r)
		if err != nil {
			code := http.StatusInternalServerError
			var he *httpError
			if errors.As(err, &he) {
				code = he.code
			}
			if code >= 500 {
				log.Printf("api %s %s: %v", r.Method, r.URL.Path, err)
			}
			writeJSON(w, code, map[string]string{"error": err.Error()})
			return
		}
		if v == nil {
			v = map[string]bool{"ok": true}
		} else if rv := reflect.ValueOf(v); rv.Kind() == reflect.Slice && rv.IsNil() {
			// A nil slice encodes as null; list endpoints must always return an array.
			v = reflect.MakeSlice(rv.Type(), 0, 0).Interface()
		}
		writeJSON(w, http.StatusOK, v)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(true)
	_ = enc.Encode(v)
}

func readJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	if err := dec.Decode(v); err != nil {
		return badRequest("invalid JSON body: %v", err)
	}
	return nil
}

func clientIP(r *http.Request) string {
	h, _, _ := net.SplitHostPort(r.RemoteAddr)
	return h
}

func audit(r *http.Request, format string, a ...any) {
	log.Printf("audit user=%s ip=%s: %s", currentUser(r), clientIP(r), fmt.Sprintf(format, a...))
}

func auditAnon(ip, format string, a ...any) {
	log.Printf("audit ip=%s: %s", ip, fmt.Sprintf(format, a...))
}
