package mobile

import (
	"net"
	"strconv"
	"testing"
)

func freeTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

func freeUDPPort(t *testing.T) int {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := pc.LocalAddr().(*net.UDPAddr).Port
	_ = pc.Close()
	return port
}

func TestAddRemoteTCPMappingRejectsDuplicateAndPortConflict(t *testing.T) {
	y, _ := startedOfflineNode(t)
	defer stopNode(t, y)

	port := freeTCPPort(t)
	local := "127.0.0.1:" + strconv.Itoa(freeTCPPort(t))
	if err := y.AddRemoteTCPMapping(port, local); err != nil {
		t.Fatalf("first add: %v", err)
	}
	if err := y.AddRemoteTCPMapping(port, local); err != nil {
		t.Fatalf("duplicate add should be idempotent: %v", err)
	}
	if got := len(y.remoteTCPMappings); got != 1 {
		t.Fatalf("duplicate add created %d mappings, want 1", got)
	}

	other := "127.0.0.1:" + strconv.Itoa(freeTCPPort(t))
	if err := y.AddRemoteTCPMapping(port, other); err == nil {
		t.Fatal("port conflict was accepted")
	}
	if got := len(y.remoteTCPMappings); got != 1 {
		t.Fatalf("conflict changed mapping count to %d, want 1", got)
	}
}

func TestAddRemoteTCPMappingReturnsListenError(t *testing.T) {
	y, addr := startedOfflineNode(t)
	defer stopNode(t, y)

	port := freeTCPPort(t)
	remote := &net.TCPAddr{IP: net.ParseIP(addr), Port: port}
	ln, err := y.netstack.ListenTCP(remote)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	local := "127.0.0.1:" + strconv.Itoa(freeTCPPort(t))
	if err := y.AddRemoteTCPMapping(port, local); err == nil {
		t.Fatal("add succeeded although the Ygg port is already bound")
	}
	if got := len(y.remoteTCPMappings); got != 0 {
		t.Fatalf("failed add left %d mappings, want 0", got)
	}
}

func TestAddRemoteUDPMappingRejectsDuplicateAndPortConflict(t *testing.T) {
	y, _ := startedOfflineNode(t)
	defer stopNode(t, y)

	port := freeUDPPort(t)
	local := "127.0.0.1:" + strconv.Itoa(freeUDPPort(t))
	if err := y.AddRemoteUDPMapping(port, local); err != nil {
		t.Fatalf("first add: %v", err)
	}
	if err := y.AddRemoteUDPMapping(port, local); err != nil {
		t.Fatalf("duplicate add should be idempotent: %v", err)
	}
	if got := len(y.remoteUDPMappings); got != 1 {
		t.Fatalf("duplicate add created %d mappings, want 1", got)
	}

	other := "127.0.0.1:" + strconv.Itoa(freeUDPPort(t))
	if err := y.AddRemoteUDPMapping(port, other); err == nil {
		t.Fatal("port conflict was accepted")
	}
	if got := len(y.remoteUDPMappings); got != 1 {
		t.Fatalf("conflict changed mapping count to %d, want 1", got)
	}
}

func TestAddRemoteMappingRejectsInvalidPort(t *testing.T) {
	y := offlineNode(t)
	for _, port := range []int{-1, 0, 65536} {
		if err := y.AddRemoteTCPMapping(port, "127.0.0.1:1"); err == nil {
			t.Fatalf("TCP accepted remote port %d", port)
		}
		if err := y.AddRemoteUDPMapping(port, "127.0.0.1:1"); err == nil {
			t.Fatalf("UDP accepted remote port %d", port)
		}
	}
}
