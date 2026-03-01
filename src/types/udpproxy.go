package types

import (
	"net"
)

// ForwardUDPResponse reads response packets from src and forwards them back to
// the shared listener dst at the client address dstAddr.
//
// Unlike ReverseProxyUDP, this function:
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

func udpProxyFunc(mtu uint64, dst net.PacketConn, dstAddr net.Addr, src net.Conn) error {
	buf := make([]byte, mtu)
	for {
		n, err := src.Read(buf[:])
		if err != nil {
			return err
		}
		if n > 0 {
			n, err = dst.WriteTo(buf[:n], dstAddr)
			if err != nil {
				return err
			}
		}
	}
}

func ReverseProxyUDP(mtu uint64, dst net.PacketConn, dstAddr net.Addr, src net.Conn) error {
	// Start bidirectional proxying
	errCh := make(chan error, 2)

	// Forward: src -> dst (with dstAddr)
	go func() {
		errCh <- udpProxyFunc(mtu, dst, dstAddr, src)
	}()

	// Backward: dst -> src (read from dst, write to src)
	go func() {
		buf := make([]byte, mtu)
		for {
			n, addr, err := dst.ReadFrom(buf[:])
			if err != nil {
				errCh <- err
				return
			}
			// Only forward packets from the expected address
			if addr.String() == dstAddr.String() {
				if n > 0 {
					_, err = src.Write(buf[:n])
					if err != nil {
						errCh <- err
						return
					}
				}
			}
		}
	}()

	// Wait for first error from either direction
	err := <-errCh

	// Close connections
	dst.Close()
	src.Close()

	return err
}
