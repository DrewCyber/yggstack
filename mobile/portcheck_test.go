package mobile

import (
	"net"
	"strconv"
	"testing"
	"time"
)

// The port checks must verify a real service on the remote side, dialed
// through the node's own netstack (no SOCKS, no local mapping). An
// offline node plus its own remote (exposed) mappings give us a
// self-contained remote side: a dial to the node's Yggdrasil address
// lands on the exposed port and is forwarded to the loopback service.

func yggAddrPort(addr, port string) string {
	return "[" + addr + "]:" + port
}

func TestCheckTCPPortOutcomes(t *testing.T) {
	y, addr := startedOfflineNode(t)

	// A real remote service: loopback listener exposed via the node
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	localPort := ln.Addr().(*net.TCPAddr).Port
	if err := y.AddRemoteTCPMapping(localPort, "127.0.0.1:"+strconv.Itoa(localPort)); err != nil {
		t.Fatal(err)
	}

	port := strconv.Itoa(localPort)
	waitFor(t, func() bool {
		return y.CheckTCPPort(yggAddrPort(addr, port), 1500) == "connected"
	})

	// Closed port on the node's own address: the stack answers RST
	closed := yggAddrPort(addr, "1") // nothing listens on port 1
	if got := y.CheckTCPPort(closed, 1500); got != "refused" {
		t.Fatalf("closed port = %q, want refused", got)
	}

	stopNode(t, y)
	if got := y.CheckTCPPort(yggAddrPort(addr, port), 500); got != "error" {
		t.Fatalf("after Stop = %q, want error", got)
	}
}

func TestCheckTCPPortRejectsNonYggAddress(t *testing.T) {
	y, _ := startedOfflineNode(t)
	if got := y.CheckTCPPort("127.0.0.1:80", 500); got != "error" {
		t.Fatalf("non-Yggdrasil address = %q, want error", got)
	}
	if got := y.CheckTCPPort("not-an-address", 500); got != "error" {
		t.Fatalf("malformed address = %q, want error", got)
	}
	stopNode(t, y)
}

func TestCheckUDPPortOutcomes(t *testing.T) {
	y, addr := startedOfflineNode(t)

	// A replying UDP service behind an exposed port
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	go func() {
		buf := make([]byte, 512)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			if _, err := pc.WriteTo(buf[:n], from); err != nil {
				return
			}
		}
	}()
	localPort := pc.LocalAddr().(*net.UDPAddr).Port
	if err := y.AddRemoteUDPMapping(localPort, "127.0.0.1:"+strconv.Itoa(localPort)); err != nil {
		t.Fatal(err)
	}

	port := strconv.Itoa(localPort)
	deadline := time.Now().Add(5 * time.Second)
	for y.CheckUDPPort(yggAddrPort(addr, port), 1000) != "replied" {
		if time.Now().After(deadline) {
			t.Fatal("exposed UDP port never replied")
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Closed UDP port: no answer, but that is inconclusive — "silent",
	// not dead (the caller renders this as "no answer", never red)
	if got := y.CheckUDPPort(yggAddrPort(addr, "1"), 300); got != "silent" {
		t.Fatalf("closed UDP port = %q, want silent", got)
	}

	stopNode(t, y)
	if got := y.CheckUDPPort(yggAddrPort(addr, port), 500); got != "error" {
		t.Fatalf("after Stop = %q, want error", got)
	}
}

func TestUdpProbePayload(t *testing.T) {
	dns := udpProbePayload(53)
	if len(dns) != 17 || dns[0] != 0x12 || dns[2] != 0x01 || dns[12] != 0x00 {
		t.Fatalf("DNS probe = % x, want 17-byte root NS query", dns)
	}
	if got := udpProbePayload(1234); len(got) != 8 {
		t.Fatalf("generic probe len = %d, want 8", len(got))
	}
}
