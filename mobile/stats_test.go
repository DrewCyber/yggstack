package mobile

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/things-go/go-socks5/statute"
)

func TestSOCKSCountsOnlyYggdrasilPayload(t *testing.T) {
	y := offlineNode(t)
	if err := y.Start("127.0.0.1:0", ""); err != nil {
		t.Fatal(err)
	}
	listener, err := y.netstack.ListenTCP(&net.TCPAddr{IP: y.core.Address()})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := listener.Accept()
		if err == nil {
			accepted <- c
		}
	}()
	client := socksClient(t, y, statute.CommandConnect, listener.Addr().(*net.TCPAddr))
	var target net.Conn
	select {
	case target = <-accepted:
	case <-time.After(3 * time.Second):
		t.Fatal("accept stalled")
	}
	defer target.Close()
	target.SetDeadline(time.Now().Add(3 * time.Second))
	value, _ := y.listenerStats.Load(socksStatsKey)
	stats := value.(*listenerStats)
	if stats.rxBytes.Load() != 0 || stats.txBytes.Load() != 0 {
		t.Fatal("SOCKS handshake counted as payload")
	}
	if _, err := client.Write([]byte("request")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(target, make([]byte, 7)); err != nil {
		t.Fatal(err)
	}
	if _, err := target.Write([]byte("reply")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(client, make([]byte, 5)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return stats.txBytes.Load() >= 7 && stats.rxBytes.Load() >= 5 })
	if stats.txBytes.Load() != 7 || stats.rxBytes.Load() != 5 {
		t.Fatalf("TX=%d RX=%d", stats.txBytes.Load(), stats.rxBytes.Load())
	}
	if stats.activeConns.Load() != 1 || stats.totalConns.Load() != 1 {
		t.Fatal("incorrect gauge")
	}
	client.Close()
	target.Close()
	waitFor(t, func() bool { y.run.mu.Lock(); defer y.run.mu.Unlock(); return len(y.run.resources) == 1 })
	if stats.activeConns.Load() != 0 {
		t.Fatal("gauge retained after close")
	}
}

func TestRemoteUDPCountsOnlyYggdrasilLeg(t *testing.T) {
	y := offlineNode(t)
	local, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	if err := y.AddRemoteUDPMapping(23456, local.LocalAddr().String()); err != nil {
		t.Fatal(err)
	}
	if err := y.Start("", ""); err != nil {
		t.Fatal(err)
	}
	key := remoteUDPMappingKey(23456, local.LocalAddr().String())
	scope := y.mappings[key]
	waitFor(t, func() bool { scope.mu.Lock(); defer scope.mu.Unlock(); return len(scope.resources) > 0 })
	c, err := y.netstack.DialUDP(&net.UDPAddr{IP: y.core.Address(), Port: 23456})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	local.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err = c.Write([]byte("request")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 32)
	n, from, err := local.ReadFromUDP(buf)
	if err != nil || string(buf[:n]) != "request" {
		t.Fatalf("request: %q %v", buf[:n], err)
	}
	if _, err = local.WriteToUDP([]byte("reply"), from); err != nil {
		t.Fatal(err)
	}
	n, err = c.Read(buf)
	if err != nil || string(buf[:n]) != "reply" {
		t.Fatalf("reply: %q %v", buf[:n], err)
	}
	value, _ := y.listenerStats.Load(key)
	stats := value.(*listenerStats)
	waitFor(t, func() bool { return stats.rxBytes.Load() >= 7 && stats.txBytes.Load() >= 5 })
	if stats.rxBytes.Load() != 7 || stats.txBytes.Load() != 5 {
		t.Fatalf("RX=%d TX=%d", stats.rxBytes.Load(), stats.txBytes.Load())
	}
	if stats.activeConns.Load() != 1 || stats.totalConns.Load() != 1 {
		t.Fatal("incorrect session gauge")
	}
	if err := y.RemoveRemoteUDPMapping(23456, local.LocalAddr().String()); err != nil {
		t.Fatal(err)
	}
	if stats.activeConns.Load() != 0 {
		t.Fatal("session survived mapping removal")
	}
}

func TestUDPSessionReaperIndependentOfReads(t *testing.T) {
	scope := newWorkerScope(context.Background())
	defer scope.stop()
	sessions := new(sync.Map)
	stats := new(listenerStats)
	a, b := net.Pipe()
	defer b.Close()
	idle := newUDPSession(scope.conn(wrapGaugeOnlyConn(a, stats)))
	idle.lastSeen = time.Now().Add(-2 * udpSessionIdleTimeout)
	sessions.Store("idle", idle)
	activeA, activeB := net.Pipe()
	defer activeB.Close()
	active := newUDPSession(scope.conn(wrapGaugeOnlyConn(activeA, stats)))
	sessions.Store("active", active)
	startUDPSessionReaper(scope, sessions, time.Millisecond)
	// No packet read loop or deadlines exist; continuously touch a different client.
	done := make(chan struct{})
	scope.goWorker(func() {
		for {
			select {
			case <-done:
				return
			case <-scope.ctx.Done():
				return
			default:
				active.touch()
			}
		}
	})
	waitFor(t, func() bool { _, ok := sessions.Load("idle"); return !ok && stats.activeConns.Load() == 1 })
	close(done)
	if idle.touch() {
		t.Fatal("expired session revived")
	}
	if _, ok := sessions.Load("active"); !ok {
		t.Fatal("active session expired")
	}
	scope.mu.Lock()
	count := len(scope.resources)
	scope.mu.Unlock()
	if count != 1 {
		t.Fatalf("closed resources retained: %d", count)
	}
	b.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := b.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("expired conn not closed: %v", err)
	}
	scope.stop()
	if stats.activeConns.Load() != 0 {
		t.Fatal("active session survived stop")
	}
}
