package mobile

import (
	"net"
	"strings"

	"github.com/yggdrasil-network/yggdrasil-go/src/address"
	"github.com/yggdrasil-network/yggstack/src/netstack"
)

// WarmupAddress fires the overlay's rumor path lookup for a Yggdrasil
// address (within 200::/7) without sending any payload — the same lookup
// a cold packet triggers (see CheckTCPPort/CheckUDPPort): the signed
// pathNotify response caches the destination's path and teaches the
// keyStore the address→key mapping, so the next real packet skips both
// round trips and only pays the session handshake. Fire-and-forget:
// returns immediately and carries no verdict — the port checks that
// follow are the verdict. Lookup messages are throttled to one per
// destination per second (a fan-out over distinct destinations never
// throttles), and firing at an already-warm destination at most
// re-requests it. The port in addrPort, if present, is ignored. No-op
// when the node is not running or the address is not a Yggdrasil
// address.
func (y *Yggstack) WarmupAddress(addrPort string) {
	ip := parseYggIP(addrPort)
	if ip == nil {
		return
	}
	y.mu.RLock()
	running := y.isRunning
	core := y.core
	y.mu.RUnlock()
	if !running || core == nil {
		return
	}
	var addr address.Address
	copy(addr[:], ip.To16())
	core.SendLookup(addr.GetKey())
}

// parseYggIP accepts a bare, bracketed, or "[addr]:port" Yggdrasil
// address and returns the IP, or nil when it does not parse or lies
// outside 200::/7.
func parseYggIP(addrPort string) net.IP {
	s := strings.TrimSpace(addrPort)
	if strings.HasPrefix(s, "[") {
		if i := strings.IndexByte(s, ']'); i >= 0 {
			s = s[1:i]
		}
	}
	ip := net.ParseIP(s)
	if ip == nil || !netstack.IsYggAddress(ip) {
		return nil
	}
	return ip
}
