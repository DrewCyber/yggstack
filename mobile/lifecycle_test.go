package mobile

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/things-go/go-socks5/statute"
	"github.com/yggdrasil-network/yggdrasil-go/src/config"
)

func offlineNode(t *testing.T) *Yggstack {
	t.Helper()
	y := NewYggstack()
	y.logWriter = io.Discard
	y.logger = y.buildLogger()
	y.config = config.GenerateConfig()
	y.config.Peers = nil
	y.config.Listen = nil
	y.config.InterfacePeers = nil
	y.config.MulticastInterfaces = nil
	t.Cleanup(func() {
		if y.IsRunning() {
			y.Stop()
		}
	})
	return y
}

func waitFor(t *testing.T, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatal("condition did not become true")
		}
		time.Sleep(time.Millisecond)
	}
}

func stopNode(t *testing.T, y *Yggstack) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- y.Stop() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not join workers")
	}
	if y.core != nil || y.netstack != nil || y.run != nil || y.IsRunning() {
		t.Fatal("run retained after Stop")
	}
}

func TestStartupRollbackAndRestart(t *testing.T) {
	y := offlineNode(t)
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	done := make(chan error, 1)
	go func() { done <- y.Start(occupied.Addr().String(), "") }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected bind failure")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("startup rollback deadlocked")
	}
	if y.core != nil || y.netstack != nil || y.run != nil || y.IsRunning() {
		t.Fatal("partial startup retained")
	}
	for range 3 {
		if err := y.Start("127.0.0.1:0", ""); err != nil {
			t.Fatal(err)
		}
		stopNode(t, y)
		if err := y.Stop(); err == nil {
			t.Fatal("repeated Stop should report not running")
		}
	}
}

func socksClient(t *testing.T, y *Yggstack, command byte, target *net.TCPAddr) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", y.socks5Tcp.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Write([]byte{5, 1, 0}); err != nil {
		t.Fatal(err)
	}
	greeting := make([]byte, 2)
	if _, err := io.ReadFull(c, greeting); err != nil {
		t.Fatal(err)
	}
	addr := statute.AddrSpec{AddrType: statute.ATYPIPv6, IP: target.IP, Port: target.Port}
	req := statute.Request{Version: 5, Command: command, DstAddr: addr}
	if _, err := c.Write(req.Bytes()); err != nil {
		t.Fatal(err)
	}
	// Reply header followed by bind address and port.
	h := make([]byte, 4)
	if _, err := io.ReadFull(c, h); err != nil {
		t.Fatal(err)
	}
	if h[1] != 0 {
		t.Fatalf("SOCKS reply %v", h)
	}
	n := 6
	if h[3] == 4 {
		n = 18
	}
	if _, err := io.ReadFull(c, make([]byte, n)); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestSOCKSConnectionsClosedOnStop(t *testing.T) {
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
		t.Fatal("local netstack accept stalled")
	}
	defer target.Close()
	target.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := client.Write([]byte("test")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(target, buf); err != nil || string(buf) != "test" {
		t.Fatalf("transfer: %q %v", buf, err)
	}
	// An idle handshake and an idle UDP association must also be owned.
	idle, err := net.Dial("tcp", y.socks5Tcp.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()
	association := socksClient(t, y, statute.CommandAssociate, &net.TCPAddr{IP: net.IPv6zero})
	stopNode(t, y)
	for _, c := range []net.Conn{client, target, idle, association} {
		c.SetReadDeadline(time.Now().Add(time.Second))
		if _, err := c.Read(buf); err == nil {
			t.Fatal("connection survived Stop")
		} else if e, ok := err.(net.Error); ok && e.Timeout() {
			t.Fatal("connection not closed")
		}
	}
}

func TestMappingRemoveReadd(t *testing.T) {
	y := offlineNode(t)
	if err := y.Start("", ""); err != nil {
		t.Fatal(err)
	}
	for _, udp := range []bool{false, true} {
		local, remote := "127.0.0.1:0", "[200::1]:1234"
		add, remove := y.AddLocalTCPMapping, y.RemoveLocalTCPMapping
		key := localTCPMappingKey(local, remote)
		if udp {
			add, remove = y.AddLocalUDPMapping, y.RemoveLocalUDPMapping
			key = localUDPMappingKey(local, remote)
		}
		for range 10 {
			if err := add(local, remote); err != nil {
				t.Fatal(err)
			}
			old := y.mappings[key]
			waitFor(t, func() bool { old.mu.Lock(); defer old.mu.Unlock(); return len(old.resources) > 0 })
			// A session/socket owned by this generation must close on remove.
			a, b := net.Pipe()
			old.conn(a)
			if err := remove(local, remote); err != nil {
				t.Fatal(err)
			}
			b.SetReadDeadline(time.Now().Add(time.Second))
			if _, err := b.Read(make([]byte, 1)); err != io.EOF {
				t.Fatalf("old session survived: %v", err)
			}
			b.Close()
			if err := add(local, remote); err != nil {
				t.Fatal(err)
			}
			fresh := y.mappings[key]
			if fresh == old {
				t.Fatal("reused generation")
			}
			waitFor(t, func() bool { _, ok := y.listenerStats.Load(key); return ok })
			if y.mappings[key] != fresh {
				t.Fatal("old cleanup deleted replacement")
			}
			if err := remove(local, remote); err != nil {
				t.Fatal(err)
			}
		}
	}
	stopNode(t, y)
}

func TestScopeLateRegistrationAndJoin(t *testing.T) {
	s := newWorkerScope(context.Background())
	a, b := net.Pipe()
	defer b.Close()
	s.Close()
	s.conn(a)
	if _, err := b.Read(make([]byte, 1)); err != io.EOF {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() { s.stop() })
	}
	wg.Wait()
}
