// Package smssend sends one SMS through ModemManager. Both router-webui (root) and kstick-smsgw (user smsgw) use
// it, and a file lock (flock) on LockPath makes the two processes take turns: the modem handles one send at a time,
// and two mmcli sends at once can fail or time out.
package smssend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// LockPath is in the SMS database directory, which both processes can write (root, and smsgw as its owner).
const LockPath = "/var/lib/smsgw/send.lock"

// Every error from Send means the SMS was not sent (safe to try again), except ErrUnknown.

// ErrModem: the modem is missing or not registered on the network.
var ErrModem = errors.New("the LTE modem is not registered on the network")

// ErrUnknown: the send step itself timed out; the network may or may not have accepted the message.
var ErrUnknown = errors.New("the modem did not answer in time; the SMS may or may not have been sent")

// Result of a successful send.
type Result struct {
	// MessageRef is the network's message reference (0-255), used to match delivery reports; -1 when unknown.
	MessageRef int
}

// Runner runs mmcli; tests replace it.
var Runner = func(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "mmcli", args...).CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return string(out), fmt.Errorf("mmcli %s: timed out after %s: %w", args[0], timeout, context.DeadlineExceeded)
	}
	if err != nil {
		return string(out), fmt.Errorf("mmcli %s: %v: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// lock takes the cross-process send lock, waiting for ctx. Without the lock file (e.g. in tests, or when the
// directory is missing) it carries on unlocked rather than refusing to send.
func lock(ctx context.Context, path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0660)
	if err != nil {
		return func() {}, nil
	}
	// Created by root (router-webui)? Hand it to the directory's owner (smsgw), so the gateway can open it too.
	if os.Geteuid() == 0 {
		if st, err := os.Stat(filepath.Dir(path)); err == nil {
			if sys, ok := st.Sys().(*syscall.Stat_t); ok {
				f.Chown(int(sys.Uid), int(sys.Gid))
			}
		}
		f.Chmod(0660)
	}
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) { // e.g. refused by a seccomp filter: send unlocked rather than never
			f.Close()
			return func() {}, nil
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, fmt.Errorf("waiting for another SMS send: %w", ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// Options for Send.
type Options struct {
	DeliveryReport bool   // ask the network for a delivery report
	CheckModem     bool   // refuse quickly when the modem is not registered
	LockPath       string // default LockPath
}

// Send sends text to number (validated by the caller: '+' and digits only). The text goes through a file, never the
// command line. The SMS object is deleted from the modem afterwards (the database is the record).
func Send(ctx context.Context, number, text string, o Options) (Result, error) {
	res := Result{MessageRef: -1}
	if o.LockPath == "" {
		o.LockPath = LockPath
	}
	unlock, err := lock(ctx, o.LockPath)
	if err != nil {
		return res, err
	}
	defer unlock()
	if o.CheckModem {
		if err := modemReady(ctx); err != nil {
			return res, err
		}
	}
	f, err := os.CreateTemp("", "sms-*.txt")
	if err != nil {
		return res, err
	}
	defer os.Remove(f.Name())
	_, werr := f.WriteString(text)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return res, werr
	}
	spec := "number='" + number + "'"
	if o.DeliveryReport {
		spec += ",delivery-report-request=yes"
	}
	out, err := Runner(ctx, 30*time.Second, "-m", "any", "--messaging-create-sms="+spec, "--messaging-create-sms-with-text="+f.Name())
	if err != nil {
		return res, err
	}
	i := strings.Index(out, "/org/freedesktop/ModemManager1/SMS/")
	if i < 0 {
		return res, fmt.Errorf("could not create SMS: %s", strings.TrimSpace(out))
	}
	path := strings.Fields(out[i:])[0]
	defer Runner(context.Background(), 20*time.Second, "-m", "any", "--messaging-delete-sms="+path)
	if _, err := Runner(ctx, 90*time.Second, "-s", path, "--send"); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return res, fmt.Errorf("%w (%v)", ErrUnknown, err)
		}
		return res, err // the modem or network refused it
	}
	if o.DeliveryReport {
		if js, err := Runner(context.Background(), 15*time.Second, "-s", path, "-J"); err == nil {
			res.MessageRef = ParseMessageRef(js)
		}
	}
	return res, nil
}

// ParseMessageRef reads "message-reference" from `mmcli -s PATH -J` output; -1 when missing.
func ParseMessageRef(js string) int {
	const k = `"message-reference":"`
	i := strings.Index(js, k)
	if i < 0 {
		return -1
	}
	rest := js[i+len(k):]
	j := strings.IndexByte(rest, '"')
	if j < 0 {
		return -1
	}
	n, err := strconv.Atoi(rest[:j])
	if err != nil || n < 0 || n > 255 {
		return -1
	}
	return n
}

func modemReady(ctx context.Context) error {
	out, err := Runner(ctx, 15*time.Second, "-m", "any", "-J")
	if err != nil {
		return fmt.Errorf("%w (%v)", ErrModem, err)
	}
	st := State(out)
	switch st {
	case "registered", "connected", "connecting", "disconnecting":
		return nil
	}
	return fmt.Errorf("%w (state %s)", ErrModem, st)
}

// State extracts modem.generic.state from `mmcli -m any -J` output.
func State(js string) string {
	var m struct {
		Modem struct {
			Generic struct {
				State string `json:"state"`
			} `json:"generic"`
		} `json:"modem"`
	}
	if json.Unmarshal([]byte(js), &m) != nil || m.Modem.Generic.State == "" {
		return "unknown"
	}
	return m.Modem.Generic.State
}
