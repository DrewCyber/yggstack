package mobile

import (
	"strings"
	"sync"
	"testing"
	"time"
)

type pingCollector struct {
	mu     sync.Mutex
	events []string
}

func (c *pingCollector) OnResult(result string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, result)
}

func (c *pingCollector) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.events...)
}

func (c *pingCollector) count(contains string) int {
	n := 0
	for _, e := range c.snapshot() {
		if strings.Contains(e, contains) {
			n++
		}
	}
	return n
}

// doneReason returns the reason of the session's done event, or "" if none
// arrived yet.
func (c *pingCollector) doneReason() string {
	var reason string
	for _, e := range c.snapshot() {
		if !strings.Contains(e, `"Type":"done"`) {
			continue
		}
		const marker = `"Reason":"`
		if i := strings.Index(e, marker); i >= 0 {
			rest := e[i+len(marker):]
			reason = rest[:strings.IndexByte(rest, '"')]
		}
	}
	return reason
}

func awaitPingDone(t *testing.T, c *pingCollector) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for c.doneReason() == "" {
		if time.Now().After(deadline) {
			t.Fatalf("ping session never finished; events: %v", c.snapshot())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func startedOfflineNode(t *testing.T) (*Yggstack, string) {
	t.Helper()
	y := offlineNode(t)
	if err := y.Start("", "", ""); err != nil {
		t.Fatal(err)
	}
	addr, err := y.GetAddress()
	if err != nil {
		t.Fatal(err)
	}
	return y, addr
}

func TestSelfPingThroughTheStack(t *testing.T) {
	y, addr := startedOfflineNode(t)
	c := &pingCollector{}

	if err := y.StartPing(addr, 3, 2000, 200, c); err != nil {
		t.Fatal(err)
	}
	awaitPingDone(t, c)
	if got := c.doneReason(); got != "completed" {
		t.Fatalf("done reason = %q, want completed", got)
	}
	// The very first probe to any destination can be lost to the overlay's
	// key lookup (by design: it is reported as a plain timeout). Every
	// probe after the first must be answered in-stack.
	if got := c.count(`"Success":true`); got < 2 {
		t.Fatalf("successful probes = %d, want >= 2; events: %v", got, c.snapshot())
	}
	y.StopPing()
	stopNode(t, y)
}

func TestStopPingEndsTheSession(t *testing.T) {
	y, addr := startedOfflineNode(t)
	c := &pingCollector{}

	if err := y.StartPing(addr, 0, 2000, 200, c); err != nil {
		t.Fatal(err)
	}
	time.Sleep(700 * time.Millisecond)
	y.StopPing()
	if got := c.doneReason(); got != "stopped" {
		t.Fatalf("done reason = %q, want stopped", got)
	}
	stopNode(t, y)
}

func TestStartPingValidatesInputAndState(t *testing.T) {
	y, addr := startedOfflineNode(t)

	if err := y.StartPing("not-an-address", 1, 2000, 200, &pingCollector{}); err == nil {
		t.Fatal("expected error for malformed address")
	}
	if err := y.StartPing("2001:db8::1", 1, 2000, 200, &pingCollector{}); err == nil {
		t.Fatal("expected error for non-Yggdrasil address")
	}

	stopNode(t, y)
	if err := y.StartPing(addr, 1, 2000, 200, &pingCollector{}); err == nil {
		t.Fatal("expected error when not running")
	}
}

func TestNewPingSessionReplacesTheRunningOne(t *testing.T) {
	y, addr := startedOfflineNode(t)
	first := &pingCollector{}
	second := &pingCollector{}

	if err := y.StartPing(addr, 0, 2000, 200, first); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if err := y.StartPing(addr, 1, 2000, 200, second); err != nil {
		t.Fatal(err)
	}

	if got := first.doneReason(); got != "stopped" {
		t.Fatalf("replaced session done reason = %q, want stopped", got)
	}
	awaitPingDone(t, second)
	if got := second.doneReason(); got != "completed" {
		t.Fatalf("new session done reason = %q, want completed", got)
	}
	stopNode(t, y)
}

func TestStopJoinsThePingSession(t *testing.T) {
	y, addr := startedOfflineNode(t)
	c := &pingCollector{}

	if err := y.StartPing(addr, 0, 2000, 200, c); err != nil {
		t.Fatal(err)
	}
	// Stop must join the session: no callback events may arrive after.
	stopNode(t, y)
	time.Sleep(300 * time.Millisecond)
	n := len(c.snapshot())
	time.Sleep(300 * time.Millisecond)
	if got := len(c.snapshot()); got != n {
		t.Fatalf("events arrived after Stop: %d -> %d", n, got)
	}
}
