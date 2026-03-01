package types

import (
	"net"
)

// ForwardUDPResponse reads response packets from src and forwards them back to
// the shared listener dst at the client address dstAddr.
//
// This function:
//   - Does NOT close dst (the shared listener must stay open for other clients)
//   - Does NOT compete with the dispatch loop by reading from dst
//   - Calls onDone when src errors or closes, so the caller can remove the
//     session from its tracking map and close src.
//
// This is the correct helper to use inside per-client UDP forwarding loops
// where a single shared listener serves multiple remote clients.
func ForwardUDPResponse(mtu uint64, dst net.PacketConn, dstAddr net.Addr, src net.Conn, onDone func()) {
	defer onDone()
	buf := make([]byte, mtu)
	for {
		n, err := src.Read(buf)
		if err != nil {
			return
		}
		if n > 0 {
			// Ignore write errors: the shared listener may have lagged,
			// but we must not close it from here.
			_, _ = dst.WriteTo(buf[:n], dstAddr)
		}
	}
}
