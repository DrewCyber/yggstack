package mobile

import (
	"context"
	"io"
	"net"
	"sync"
	"time"

	"github.com/things-go/go-socks5"
	"github.com/things-go/go-socks5/statute"
	"github.com/yggdrasil-network/yggstack/src/types"
)

type scopedResolver struct {
	scope    *workerScope
	resolver socks5.NameResolver
}

func (r scopedResolver) Resolve(_ context.Context, name string) (context.Context, net.IP, error) {
	ctx, cancel := context.WithTimeout(r.scope.ctx, 10*time.Second)
	defer cancel()
	_, ip, err := r.resolver.Resolve(ctx, name)
	return r.scope.ctx, ip, err
}

func (y *Yggstack) startSOCKS(listener net.Listener, nameserver string) {
	run, ns := y.run, y.netstack
	stats := y.getOrCreateListenerStats(socksStatsKey, "socks", listener.Addr().String(), "")
	var resolver socks5.NameResolver = socks5.DNSResolver{}
	if nameserver != "" {
		resolver = types.NewNameResolver(ns, nameserver)
	}
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
			conn = client.conn(wrapCountingConn(conn, stats))
			dial := func(_ context.Context, network, addr string) (net.Conn, error) {
				ctx, cancel := context.WithTimeout(client.ctx, 10*time.Second)
				defer cancel()
				c, err := ns.DialContext(ctx, network, addr)
				if err != nil {
					return nil, err
				}
				return client.conn(wrapTrafficOnlyConn(c, stats)), nil
			}
			server := socks5.NewServer(
				socks5.WithDial(dial),
				socks5.WithResolver(scopedResolver{client, resolver}),
				socks5.WithGPool(client),
				// The upstream associate handler's UDP socket is not exposed and
				// leaks when sending its initial reply fails. Own that socket here.
				socks5.WithAssociateHandle(func(_ context.Context, w io.Writer, req *socks5.Request) error {
					return serveAssociate(client, w, req, dial)
				}),
			)
			run.goWorker(func() {
				server.ServeConn(conn)
				client.stop()
				run.forget(client)
			})
		}
	})
}

func serveAssociate(client *workerScope, w io.Writer, req *socks5.Request, dial func(context.Context, string, string) (net.Conn, error)) error {
	addr := req.LocalAddr.(*net.TCPAddr)
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: addr.IP})
	if err != nil {
		socks5.SendReply(w, statute.RepServerFailure, nil)
		return err
	}
	if !client.own(udp) {
		return net.ErrClosed
	}
	defer udp.Close()
	defer client.forget(udp)
	if err := socks5.SendReply(w, statute.RepSuccess, udp.LocalAddr()); err != nil {
		return err
	}
	client.goWorker(func() {
		sessions := make(map[string]net.Conn)
		var sessionsMu sync.Mutex
		removeSession := func(key string, c net.Conn) {
			sessionsMu.Lock()
			if sessions[key] == c {
				delete(sessions, key)
			}
			sessionsMu.Unlock()
			c.Close()
		}
		defer func() {
			sessionsMu.Lock()
			defer sessionsMu.Unlock()
			for _, c := range sessions {
				c.Close()
			}
		}()
		buf := make([]byte, 65535)
		for {
			n, from, err := udp.ReadFromUDP(buf)
			if err != nil {
				return
			}
			packet, err := statute.ParseDatagram(buf[:n])
			if err != nil || packet.Frag != 0 {
				continue
			}
			if !req.DestAddr.IP.IsUnspecified() && !req.DestAddr.IP.Equal(from.IP) {
				continue
			}
			if req.DestAddr.Port != 0 && req.DestAddr.Port != from.Port {
				continue
			}
			key := from.String() + "->" + packet.DstAddr.String()
			sessionsMu.Lock()
			c := sessions[key]
			sessionsMu.Unlock()
			if c == nil {
				c, err = dial(client.ctx, "udp", packet.DstAddr.String())
				if err != nil {
					continue
				}
				sessionsMu.Lock()
				sessions[key] = c
				sessionsMu.Unlock()
				header := append([]byte(nil), packet.Header()...)
				client.goWorker(func() {
					defer removeSession(key, c)
					response := make([]byte, 65535)
					for {
						n, err := c.Read(response)
						if err != nil {
							return
						}
						if _, err := udp.WriteToUDP(append(header[:len(header):len(header)], response[:n]...), from); err != nil {
							return
						}
					}
				})
			}
			if _, err := c.Write(packet.Data); err != nil {
				removeSession(key, c)
			}
		}
	})
	_, err = io.Copy(io.Discard, req.Reader)
	return err
}
