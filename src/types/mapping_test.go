package types

import "testing"

func TestLocalMappings(t *testing.T) {
	var tcpLocal TCPLocalMappings
	// Valid: local port mapped to a Yggdrasil IPv6 address and port
	if err := tcpLocal.Set("1234:[2000::1]:4321"); err != nil {
		t.Fatal(err)
	}
	// Invalid: local mappings must carry a Yggdrasil IPv6 mapped address
	if err := tcpLocal.Set("1234"); err == nil {
		t.Fatal("mapped Yggdrasil address is required")
	}
	if err := tcpLocal.Set("1234:localhost:4321"); err == nil {
		t.Fatal("mapped address must be an IPv6 literal")
	}
	if err := tcpLocal.Set("a"); err == nil {
		t.Fatal("'a' should be an invalid exposed port")
	}

	var udpLocal UDPLocalMappings
	if err := udpLocal.Set("1234:[2000::1]:4321"); err != nil {
		t.Fatal(err)
	}
	if err := udpLocal.Set("1234"); err == nil {
		t.Fatal("mapped Yggdrasil address is required")
	}
	if err := udpLocal.Set("1234:localhost:4321"); err == nil {
		t.Fatal("mapped address must be an IPv6 literal")
	}
}

func TestRemoteMappings(t *testing.T) {
	var tcpRemote TCPRemoteMappings
	// Valid: remote (Yggdrasil) port mapped to a plain IP address and port
	if err := tcpRemote.Set("1234:192.168.1.1:4321"); err != nil {
		t.Fatal(err)
	}
	// Invalid: remote mappings must not carry a listen address
	if err := tcpRemote.Set("192.168.1.2:1234:192.168.1.1:4321"); err == nil {
		t.Fatal("remote mapping must not include a listen address")
	}
	if err := tcpRemote.Set("1234:localhost:4321"); err == nil {
		t.Fatal("mapped address must be an IP literal")
	}

	var udpRemote UDPRemoteMappings
	if err := udpRemote.Set("1234:192.168.1.1:4321"); err != nil {
		t.Fatal(err)
	}
	if err := udpRemote.Set("192.168.1.2:1234:192.168.1.1:4321"); err == nil {
		t.Fatal("remote mapping must not include a listen address")
	}
}
