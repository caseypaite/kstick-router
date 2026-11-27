package main

// Fewer background processes: the VPN and IPv6 loops used to run their checks (ip, nmcli, nft) every 10 s. Now
// they run every minute, plus at once when the kernel reports a link or address change (`ip monitor`: a tunnel or
// the LTE/WARP interface coming up or going down), which also makes them react faster than before.

import (
	"bufio"
	"log"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
)

var (
	vpnNet = make(chan struct{}, 1)
	v6Net  = make(chan struct{}, 1)
)

func notify(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// netWatch runs `ip monitor link address` and wakes the loops on every event.
func netWatch() {
	for {
		cmd := exec.Command("ip", "monitor", "link", "address")
		out, err := cmd.StdoutPipe()
		if err == nil {
			err = cmd.Start()
		}
		if err != nil {
			log.Printf("netwatch: %v", err)
			time.Sleep(time.Minute)
			continue
		}
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			notify(vpnNet)
			notify(v6Net)
		}
		cmd.Wait()
		time.Sleep(10 * time.Second)
	}
}

// netQuiet: events this soon after either loop's run are most likely caused by that run (both add addresses and
// routes) and are ignored, so the loops cannot keep waking themselves or each other.
const netQuiet = 3 * time.Second

var lastReconcile atomic.Int64 // unix nanoseconds when a VPN or IPv6 run last finished

// waitNext records the end of a run, then blocks until the ticker fires, a kick arrives (a settings change), or a
// network event arrives at least netQuiet after the last run of either loop.
func waitNext(t *time.Ticker, kick, net <-chan struct{}) {
	lastReconcile.Store(time.Now().UnixNano())
	for {
		select {
		case <-t.C:
			return
		case <-kick:
			return
		case <-net:
			if time.Since(time.Unix(0, lastReconcile.Load())) >= netQuiet {
				time.Sleep(time.Second) // let a burst of events settle
				return
			}
		}
	}
}

// ttlCache keeps a value for ttl; the dashboard asks every 4 s, and several open pages share one result.
type ttlCache[T any] struct {
	mu  sync.Mutex
	ttl time.Duration
	at  time.Time
	v   T
}

func (c *ttlCache[T]) get(f func() T) T {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.at.IsZero() || time.Since(c.at) >= c.ttl {
		c.v, c.at = f(), time.Now()
	}
	return c.v
}

// invalidate drops the cached value (after a settings change).
func (c *ttlCache[T]) invalidate() {
	c.mu.Lock()
	c.at = time.Time{}
	c.mu.Unlock()
}
