package mobile

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/yggdrasil-network/yggstack/src/netstack"
)

// PingCallback receives a ping session's events as JSON strings — the same
// protocol the Rust engine emits:
//
//	{"Type":"probe","Seq":1,"Success":true,"RttMs":12.34}
//	{"Type":"probe","Seq":2,"Success":false,"Error":"timeout"}
//	{"Type":"done","Reason":"completed"|"stopped"|"error"}
type PingCallback interface {
	OnResult(result string)
}

// pingSession tracks the running session: cancel stops it and done lets
// StopPing join the goroutine so no callback event can race a newly
// started session or the engine teardown.
type pingSession struct {
	cancel context.CancelFunc
	done   sync.WaitGroup
}

// StartPing starts an ICMPv6 echo session to a Yggdrasil address (within
// 200::/7), sending count probes (0 = until stopped) every intervalMs ms,
// each waiting up to timeoutMs for its reply. Every probe result and the
// session end are reported through cb. Replaces any running session;
// requires a running node. The first probe to an unknown destination may
// be delayed by the overlay's key lookup and time out — it is reported
// as-is.
func (y *Yggstack) StartPing(address string, count int, timeoutMs int64, intervalMs int64, cb PingCallback) error {
	ip := net.ParseIP(strings.TrimSpace(strings.Trim(address, "[]")))
	if ip == nil || ip.To16() == nil {
		return fmt.Errorf("invalid IPv6 address: %s", address)
	}
	if !netstack.IsYggAddress(ip) {
		return fmt.Errorf("not a Yggdrasil address (must be within 200::/7)")
	}
	if count < 0 {
		count = 0
	}
	timeout := time.Duration(clampInt64(timeoutMs, 50, 60000)) * time.Millisecond
	interval := time.Duration(clampInt64(intervalMs, 0, 60000)) * time.Millisecond

	y.mu.Lock()
	defer y.mu.Unlock()
	if !y.isRunning || y.netstack == nil {
		return fmt.Errorf("Yggstack is not running")
	}
	// Replace a running session: join it so its final events are delivered
	// before this session starts probing.
	y.stopPingLocked()

	ctx, cancel := context.WithCancel(y.run.ctx)
	session := &pingSession{cancel: cancel}
	session.done.Add(1)
	y.ping = session
	ns := y.netstack

	go func() {
		defer session.done.Done()
		ns.RunPing(ctx, ip, count, timeout, interval, func(ev netstack.PingEvent) {
			if cb != nil {
				cb.OnResult(pingEventJSON(ev))
			}
		})
	}()
	return nil
}

// StopPing stops the running ping session (if any). Its callback receives
// a final {"Type":"done","Reason":"stopped"} event before this returns.
func (y *Yggstack) StopPing() {
	y.mu.Lock()
	defer y.mu.Unlock()
	y.stopPingLocked()
}

// stopPingLocked cancels and joins the running ping session; y.mu must be
// held. Called from Stop before the netstack is closed so the session's
// endpoint is never used against a torn-down stack.
func (y *Yggstack) stopPingLocked() {
	if y.ping == nil {
		return
	}
	y.ping.cancel()
	y.ping.done.Wait()
	y.ping = nil
}

func pingEventJSON(ev netstack.PingEvent) string {
	if ev.Done {
		return fmt.Sprintf(`{"Type":"done","Reason":%q}`, ev.Reason)
	}
	if ev.Err == nil {
		return fmt.Sprintf(`{"Type":"probe","Seq":%d,"Success":true,"RttMs":%.2f}`,
			ev.Seq, ev.RTT.Seconds()*1000.0)
	}
	errStr := "timeout"
	if !errors.Is(ev.Err, netstack.ErrPingTimeout) {
		errStr = ev.Err.Error()
	}
	return fmt.Sprintf(`{"Type":"probe","Seq":%d,"Success":false,"Error":%q}`, ev.Seq, errStr)
}

func clampInt64(v, lo, hi int64) int64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
