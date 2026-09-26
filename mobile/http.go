package mobile

import (
	"context"
	"net"

	"github.com/yggdrasil-network/yggstack/src/types"
)

// startHTTP serves an HTTP proxy (CONNECT tunnels for HTTPS plus
// plain-HTTP forwarding) on the listener, mirroring startSOCKS's ownership
// model: the run scope owns the listener, each client gets its own scope
// joined on close, and bytes are counted on the Yggdrasil-facing leg only.
// Protocol handling lives in types.HTTPProxy, shared with the desktop CLI.
func (y *Yggstack) startHTTP(listener net.Listener, nameserver string) {
	run, ns := y.run, y.netstack
	mtu := y.core.MTU()
	stats := y.getOrCreateListenerStats(httpStatsKey, "http", listener.Addr().String(), "")
	resolver := types.NewNameResolver(ns, nameserver)
	run.own(listener)
	run.goWorker(func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			client := newWorkerScope(run.ctx)
			if !run.own(client) {
				conn.Close()
				return
			}
			conn = client.conn(wrapGaugeOnlyConn(conn, stats))
			proxy := &types.HTTPProxy{
				Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
					c, err := ns.DialContext(ctx, network, addr)
					if err != nil {
						return nil, err
					}
					return client.conn(wrapTrafficOnlyConn(c, stats)), nil
				},
				Resolver: resolver,
				MTU:      mtu,
			}
			run.goWorker(func() {
				proxy.ServeConn(client.ctx, conn)
				client.stop()
				run.forget(client)
			})
		}
	})
}
