package mobile

import (
	"encoding/json"
	"net"
	"sort"
	"sync"
	"sync/atomic"
)

// socksStatsKey identifies the SOCKS5 proxy listener in the stats registry.
const socksStatsKey = "socks"

// listenerStatsWrappingEnabled toggles connection/byte instrumentation.
// The relay stress test (stress_local_test.go) exercises these wrappers
// under the race detector.
var listenerStatsWrappingEnabled = true

// wrapCountingConn counts bytes AND registers the connection in the
// listener's active/total gauges. Use for relays representing one connection.
func wrapCountingConn(conn net.Conn, stats *listenerStats) net.Conn {
	if !listenerStatsWrappingEnabled {
		return conn
	}
	stats.connOpened()
	return &countingConn{Conn: conn, stats: stats, trackGauges: true}
}

// wrapTrafficOnlyConn counts only bytes; used for secondary legs (e.g. the
// destination conn behind an already-counted SOCKS control connection).
func wrapTrafficOnlyConn(conn net.Conn, stats *listenerStats) net.Conn {
	if !listenerStatsWrappingEnabled {
		return conn
	}
	return &countingConn{Conn: conn, stats: stats}
}

// wrapCountingPacketConn instruments a shared UDP socket whose
// ReadFrom/WriteTo calls belong to several client sessions.
func wrapCountingPacketConn(conn net.PacketConn, stats *listenerStats) net.PacketConn {
	if !listenerStatsWrappingEnabled {
		return conn
	}
	return &countingPacketConn{PacketConn: conn, stats: stats}
}

// listenerStats holds runtime counters for a single listener (SOCKS proxy,
// forwarded port or exposed service). Bytes are counted on the
// Yggdrasil-facing leg of each relay: RX = received from the network,
// TX = sent to the network.
//
// For TCP listeners the active gauge tracks currently open relays; for UDP
// mappings each distinct remote client endpoint counts as a connection and
// the gauge is zeroed when the mapping handler exits.
type listenerStats struct {
	key         string
	kind        string // "socks", "local-tcp", "local-udp", "remote-tcp", "remote-udp"
	listen      string
	target      string
	activeConns atomic.Uint64
	totalConns  atomic.Uint64
	rxBytes     atomic.Uint64
	txBytes     atomic.Uint64
}

func (s *listenerStats) connOpened() {
	s.totalConns.Add(1)
	s.activeConns.Add(1)
}

// connClosed decrements the active gauge, clamping at zero so late Close
// calls after a forced shutdown cannot wrap the counter around.
func (s *listenerStats) connClosed() {
	for {
		cur := s.activeConns.Load()
		if cur == 0 {
			return
		}
		if s.activeConns.CompareAndSwap(cur, cur-1) {
			return
		}
	}
}

// sessionsEnded zeroes the active gauge; used on UDP handler exit since
// per-client sessions have no close hooks of their own.
func (s *listenerStats) sessionsEnded() {
	s.activeConns.Store(0)
}

func statsKindOrder(kind string) int {
	switch kind {
	case "socks":
		return 0
	case "local-tcp":
		return 1
	case "local-udp":
		return 2
	case "remote-tcp":
		return 3
	case "remote-udp":
		return 4
	default:
		return 5
	}
}

type listenerStatsJSON struct {
	Key         string `json:"Key"`
	Kind        string `json:"Kind"`
	Listen      string `json:"Listen"`
	Target      string `json:"Target"`
	ActiveConns uint64 `json:"ActiveConns"`
	TotalConns  uint64 `json:"TotalConns"`
	RXBytes     uint64 `json:"RXBytes"`
	TXBytes     uint64 `json:"TXBytes"`
}

func (y *Yggstack) getOrCreateListenerStats(key, kind, listen, target string) *listenerStats {
	if v, ok := y.listenerStats.Load(key); ok {
		return v.(*listenerStats)
	}
	s := &listenerStats{key: key, kind: kind, listen: listen, target: target}
	actual, _ := y.listenerStats.LoadOrStore(key, s)
	return actual.(*listenerStats)
}

func (y *Yggstack) removeListenerStats(key string) {
	y.listenerStats.Delete(key)
}

func (y *Yggstack) resetListenerStats() {
	y.listenerStats.Range(func(k, _ any) bool {
		y.listenerStats.Delete(k)
		return true
	})
}

// GetListenersJSON returns connection counts and traffic totals for every
// active listener as a JSON array, mirroring the GetPeersJSON convention.
func (y *Yggstack) GetListenersJSON() (string, error) {
	list := make([]listenerStatsJSON, 0)
	y.listenerStats.Range(func(_, v any) bool {
		s := v.(*listenerStats)
		list = append(list, listenerStatsJSON{
			Key:         s.key,
			Kind:        s.kind,
			Listen:      s.listen,
			Target:      s.target,
			ActiveConns: s.activeConns.Load(),
			TotalConns:  s.totalConns.Load(),
			RXBytes:     s.rxBytes.Load(),
			TXBytes:     s.txBytes.Load(),
		})
		return true
	})
	sort.Slice(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if ka, kb := statsKindOrder(a.Kind), statsKindOrder(b.Kind); ka != kb {
			return ka < kb
		}
		if a.Listen != b.Listen {
			return a.Listen < b.Listen
		}
		return a.Target < b.Target
	})
	b, err := json.Marshal(list)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// countingConn wraps a net.Conn on the Yggdrasil-facing leg of a relay and
// attributes its payload bytes to the owning listener's counters.
type countingConn struct {
	net.Conn
	stats       *listenerStats
	trackGauges bool // participate in the active/total connection gauges
	closeOnce   sync.Once
}

// newCountingConn counts bytes AND registers the connection in the listener's
// active/total gauges. Use for relays that represent one connection.
func newCountingConn(conn net.Conn, stats *listenerStats) *countingConn {
	stats.connOpened()
	return &countingConn{Conn: conn, stats: stats, trackGauges: true}
}

// newTrafficOnlyConn counts only bytes; used for secondary legs (e.g. the
// destination conn behind an already-counted SOCKS control connection).
func newTrafficOnlyConn(conn net.Conn, stats *listenerStats) *countingConn {
	return &countingConn{Conn: conn, stats: stats}
}

func (c *countingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.stats.rxBytes.Add(uint64(n))
	return n, err
}

func (c *countingConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.stats.txBytes.Add(uint64(n))
	return n, err
}

func (c *countingConn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		if c.trackGauges {
			c.stats.connClosed()
		}
		err = c.Conn.Close()
	})
	return err
}

// countingPacketConn is countingConn's counterpart for shared UDP sockets,
// where each ReadFrom/WriteTo belongs to one of several client sessions.
type countingPacketConn struct {
	net.PacketConn
	stats *listenerStats
}

func newCountingPacketConn(conn net.PacketConn, stats *listenerStats) *countingPacketConn {
	return &countingPacketConn{PacketConn: conn, stats: stats}
}

func (c *countingPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	n, addr, err := c.PacketConn.ReadFrom(p)
	c.stats.rxBytes.Add(uint64(n))
	return n, addr, err
}

func (c *countingPacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	n, err := c.PacketConn.WriteTo(p, addr)
	c.stats.txBytes.Add(uint64(n))
	return n, err
}

// countingListener counts connections accepted by the SOCKS5 proxy listener;
// the go-socks5 server closes every served conn, which clears the gauge.
type countingListener struct {
	net.Listener
	stats *listenerStats
}

func (l *countingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return newCountingConn(conn, l.stats), nil
}
