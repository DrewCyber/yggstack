package types

import (
	"net"
)

func tcpProxyFunc(mtu uint64, dst, src net.Conn) error {
	buf := make([]byte, mtu)
	for {
		n, err := src.Read(buf[:])
		if err != nil {
			return err
		}
		if n > 0 {
			n, err = dst.Write(buf[:n])
			if err != nil {
				return err
			}
		}
	}
}

func ProxyTCP(mtu uint64, c1, c2 net.Conn) error {
	// Start proxying
	errCh := make(chan error, 2)
	go func() { errCh <- tcpProxyFunc(mtu, c1, c2) }()
	go func() { errCh <- tcpProxyFunc(mtu, c2, c1) }()

	// Close both legs after the first pump exits, then join the other pump.
	// Returning early here would let a worker outlive its mapping/run owner.
	err := <-errCh
	c1.Close()
	c2.Close()
	<-errCh
	return err
}
