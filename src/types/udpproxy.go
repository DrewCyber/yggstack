package types

import (
	"net"
)

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
