package mobile

import (
	"context"
	"errors"
	"net"
	"strings"
	"syscall"
	"time"

	"github.com/yggdrasil-network/yggstack/src/netstack"
)

// CheckTCPPort dials "[ygg-addr]:port" through the node's own netstack —
// no proxy or mapping involved — and reports whether a real service
// answered on the remote side:
//
//	"connected" — TCP handshake completed: a listener accepted the dial
//	"refused"   — the remote side answered RST: nothing listens on that port
//	"timeout"   — no answer within timeoutMs (unreachable or silently dropped)
//	"error"     — node not running, or addrPort is not a Yggdrasil address
//
// The dial is attempted twice: the first attempt uses a short window —
// on a cold start (or first contact) the overlay's key lookup is still
// in flight and its only role is to trigger that lookup; a warm
// destination answers well within it. The retry then dials with the
// full window through the now-cached key. A genuine refusal answers
// fast on both attempts. Blocks for up to ~timeoutMs + 400ms + 1s;
// safe to call concurrently with Stop (a dial interrupted by teardown
// reports "timeout").
func (y *Yggstack) CheckTCPPort(addrPort string, timeoutMs int64) string {
	addr, err := resolveYggTCPAddr(addrPort)
	if err != nil {
		return "error"
	}
	ns, runCtx := y.checkNetstack()
	if ns == nil {
		return "error"
	}
	timeout := time.Duration(clampInt64(timeoutMs, 100, 15000)) * time.Millisecond
	firstTimeout := timeout
	if firstTimeout > time.Second {
		firstTimeout = time.Second
	}

	var last string
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			select {
			case <-runCtx.Done():
				return "error"
			case <-time.After(400 * time.Millisecond):
			}
		}
		window := timeout
		if attempt == 0 {
			window = firstTimeout
		}
		ctx, cancel := context.WithTimeout(runCtx, window)
		conn, err := ns.DialContext(ctx, "tcp", addr.String())
		cancel()
		if err == nil {
			conn.Close()
			return "connected"
		}
		if isRefused(err) {
			last = "refused"
		} else {
			last = "timeout"
		}
	}
	return last
}

// CheckUDPPort probes "[ygg-addr]:port" through the node's own netstack
// and listens for any reply. UDP has no universal handshake, so silence is
// inconclusive rather than proof of death:
//
//	"replied" — something answered: a service is listening and responding
//	"silent"  — no reply within timeoutMs (service may still be alive but
//	            ignoring junk datagrams)
//	"error"   — node not running, send failed, or malformed address
//
// The probe datagram is chosen by port (see udpProbePayload): port 53 gets
// a real DNS query, where a zero-filled datagram is silently dropped by
// most servers. Datagrams are re-sent across the window (see probeSendAt):
// on a cold start the first ones are consumed by the overlay's key lookup
// (TCP rides this out with SYN retransmission; UDP has none), so only a
// later send gets through.
func (y *Yggstack) CheckUDPPort(addrPort string, timeoutMs int64) string {
	addr, err := resolveYggUDPAddr(addrPort)
	if err != nil {
		return "error"
	}
	ns, _ := y.checkNetstack()
	if ns == nil {
		return "error"
	}
	timeout := time.Duration(clampInt64(timeoutMs, 100, 15000)) * time.Millisecond
	probe := udpProbePayload(addr.Port)

	conn, err := ns.DialUDP(addr)
	if err != nil {
		return "error"
	}
	defer conn.Close()
	buf := make([]byte, 4096)
	start := time.Now()
	sent := 0
	for {
		if sent < len(probeSendAt) && time.Since(start) >= probeSendAt[sent] {
			if _, err := conn.Write(probe); err != nil {
				return "error"
			}
			sent++
		}
		nextSend := timeout
		if sent < len(probeSendAt) && probeSendAt[sent] < nextSend {
			nextSend = probeSendAt[sent]
		}
		if err := conn.SetReadDeadline(start.Add(nextSend)); err != nil {
			return "error"
		}
		if _, err := conn.Read(buf); err == nil {
			return "replied"
		}
		if time.Since(start) >= timeout {
			return "silent"
		}
	}
}

// probeSendAt spaces probe datagram sends across the check window: the
// overlay's cold-start key lookup swallows the earliest ones, so the
// cadence keeps re-sending until past the typical lookup latency.
var probeSendAt = []time.Duration{
	0,
	400 * time.Millisecond,
	1000 * time.Millisecond,
	1800 * time.Millisecond,
}

// udpProbePayload picks the probe datagram for a UDP port. Port 53 is DNS
// by assignment: a minimal well-formed root NS query — any DNS reply
// (NOERROR, REFUSED, FORMERR…) proves a live service, where a zero-filled
// datagram is silently dropped by most servers. Other ports get the
// generic zero probe, which reply-happy protocols still answer.
func udpProbePayload(port int) []byte {
	if port == 53 {
		return []byte{
			0x12, 0x34, // ID
			0x01, 0x00, // flags: RD
			0x00, 0x01, // QDCOUNT: 1
			0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // counts: 0
			0x00,       // QNAME: root
			0x00, 0x02, // QTYPE: NS
			0x00, 0x01, // QCLASS: IN
		}
	}
	return []byte{0, 0, 0, 0, 0, 0, 0, 0}
}

// checkNetstack returns the live netstack and the run context to bind
// probe deadlines to, or nils when the node is not running.
func (y *Yggstack) checkNetstack() (*netstack.YggdrasilNetstack, context.Context) {
	y.mu.RLock()
	defer y.mu.RUnlock()
	if !y.isRunning || y.netstack == nil || y.run == nil {
		return nil, nil
	}
	return y.netstack, y.run.ctx
}

func isRefused(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) ||
		strings.Contains(strings.ToLower(err.Error()), "refused")
}

func resolveYggTCPAddr(addrPort string) (*net.TCPAddr, error) {
	addr, err := net.ResolveTCPAddr("tcp", addrPort)
	if err != nil {
		return nil, err
	}
	if addr.IP == nil || !netstack.IsYggAddress(addr.IP) {
		return nil, errors.New("not a Yggdrasil address (must be within 200::/7)")
	}
	return addr, nil
}

func resolveYggUDPAddr(addrPort string) (*net.UDPAddr, error) {
	addr, err := net.ResolveUDPAddr("udp", addrPort)
	if err != nil {
		return nil, err
	}
	if addr.IP == nil || !netstack.IsYggAddress(addr.IP) {
		return nil, errors.New("not a Yggdrasil address (must be within 200::/7)")
	}
	return addr, nil
}
