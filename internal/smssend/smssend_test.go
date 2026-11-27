package smssend

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fake replaces mmcli: create returns an SMS path, -J returns a message reference; send runs onSend.
func fake(t *testing.T, state string, onSend func(ctx context.Context) error) *[]string {
	var mu sync.Mutex
	calls := &[]string{}
	old := Runner
	Runner = func(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
		mu.Lock()
		*calls = append(*calls, strings.Join(args, " "))
		mu.Unlock()
		j := strings.Join(args, " ")
		switch {
		case j == "-m any -J":
			return `{"modem":{"generic":{"state":"` + state + `"}}}`, nil
		case strings.Contains(j, "--messaging-create-sms="):
			return "Successfully created new SMS: /org/freedesktop/ModemManager1/SMS/3 (unknown)", nil
		case strings.HasSuffix(j, "--send"):
			ctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			return "", onSend(ctx)
		case strings.HasSuffix(j, " -J"):
			return `{"sms":{"properties":{"message-reference":"57","state":"sent"}}}`, nil
		}
		return "", nil
	}
	t.Cleanup(func() { Runner = old })
	return calls
}

func TestSendOK(t *testing.T) {
	calls := fake(t, "connected", func(context.Context) error { return nil })
	res, err := Send(context.Background(), "+919876543210", "hi", Options{DeliveryReport: true, CheckModem: true, LockPath: filepath.Join(t.TempDir(), "l")})
	if err != nil || res.MessageRef != 57 {
		t.Fatalf("%v %+v", err, res)
	}
	all := strings.Join(*calls, "\n")
	if !strings.Contains(all, "number='+919876543210',delivery-report-request=yes") || !strings.Contains(all, "--messaging-delete-sms=/org/freedesktop/ModemManager1/SMS/3") {
		t.Fatal(all)
	}
}

func TestModemNotRegistered(t *testing.T) {
	calls := fake(t, "searching", func(context.Context) error { return nil })
	_, err := Send(context.Background(), "+91", "x", Options{CheckModem: true, LockPath: filepath.Join(t.TempDir(), "l")})
	if !errors.Is(err, ErrModem) || errors.Is(err, ErrUnknown) || len(*calls) != 1 {
		t.Fatalf("%v %v", err, *calls)
	}
}

func TestSendTimeoutIsUnknown(t *testing.T) {
	fake(t, "connected", func(ctx context.Context) error {
		<-ctx.Done()
		return fmt.Errorf("mmcli -s: timed out: %w", context.DeadlineExceeded)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := Send(ctx, "+91", "x", Options{LockPath: filepath.Join(t.TempDir(), "l")})
	if !errors.Is(err, ErrUnknown) {
		t.Fatalf("a timed-out send must be unknown, got %v", err)
	}
	fake(t, "connected", func(context.Context) error { return errors.New("CMS ERROR 38") })
	if _, err := Send(context.Background(), "+91", "x", Options{LockPath: filepath.Join(t.TempDir(), "l")}); err == nil || errors.Is(err, ErrUnknown) {
		t.Fatalf("a refused send is a plain failure, got %v", err)
	}
}

// Sends holding the same lock file never overlap (the lock is a flock, so it also works across processes).
func TestLockSerialises(t *testing.T) {
	var in, maxIn atomic.Int32
	fake(t, "connected", func(context.Context) error {
		n := in.Add(1)
		for {
			m := maxIn.Load()
			if n <= m || maxIn.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		in.Add(-1)
		return nil
	})
	lp := filepath.Join(t.TempDir(), "send.lock")
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); Send(context.Background(), "+91", "x", Options{LockPath: lp}) }()
	}
	wg.Wait()
	if maxIn.Load() != 1 {
		t.Fatalf("%d sends ran at once", maxIn.Load())
	}
}

func TestParse(t *testing.T) {
	if ParseMessageRef(`{"sms":{"properties":{"message-reference":"--"}}}`) != -1 || ParseMessageRef(`"message-reference":"255"`) != 255 {
		t.Fatal("message reference")
	}
	if State(`{"modem":{"generic":{"state-failed-reason":"--","state":"registered"}}}`) != "registered" || State("junk") != "unknown" {
		t.Fatal("state")
	}
}
