package netstack

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/waiter"
)

// ErrPingTimeout marks a probe that got no reply within its deadline.
var ErrPingTimeout = errors.New("timeout")

// Ping payload size (bytes) — matches the 56-byte data of a classic
// ping(1) default, keeping packets far below the 1280-byte MTU.
const pingPayloadLen = 56

var pingPayload = bytes.Repeat([]byte("yggstack ping "), 4)[:pingPayloadLen]

// IsYggAddress reports whether ip is inside 200::/7, the Yggdrasil range.
func IsYggAddress(ip net.IP) bool {
	ip16 := ip.To16()
	if ip16 == nil {
		return false
	}
	return ip16[0] == 0x02 || ip16[0] == 0x03
}

// PingEvent is one reported event of a ping session: either a probe result
// (Seq > 0) or the session end (Done). Done events carry a Reason of
// "completed", "stopped" or "error".
type PingEvent struct {
	Seq    int
	RTT    time.Duration
	Err    error
	Done   bool
	Reason string
}

// pingEndpoint drives one ICMPv6 echo socket over the netstack: the gVisor
// ICMP transport endpoint does the ident matching, checksumming and reply
// filtering; this wrapper adds probe/deadline semantics.
type pingEndpoint struct {
	ep        tcpip.Endpoint
	wq        *waiter.Queue
	waitEntry waiter.Entry
	notifyCh  <-chan struct{}
}

// newPingEndpoint creates and connects the endpoint. The caller must not
// race netstack Close: create, use and close it within one run.
func (s *YggdrasilNetstack) newPingEndpoint(dst net.IP) (*pingEndpoint, error) {
	wq := waiter.Queue{}
	ep, err := s.stack.NewEndpoint(icmp.ProtocolNumber6, ipv6.ProtocolNumber, &wq)
	if err != nil {
		return nil, fmt.Errorf("NewEndpoint: %s", err.String())
	}
	if err := ep.Connect(tcpip.FullAddress{
		NIC:  1,
		Addr: tcpip.AddrFromSlice(dst.To16()),
	}); err != nil {
		ep.Close()
		return nil, fmt.Errorf("Connect: %s", err.String())
	}
	entry, ch := waiter.NewChannelEntry(waiter.EventIn)
	wq.EventRegister(&entry)
	return &pingEndpoint{ep: ep, wq: &wq, waitEntry: entry, notifyCh: ch}, nil
}

func (p *pingEndpoint) close() {
	p.wq.EventUnregister(&p.waitEntry)
	p.ep.Close()
}

// probe sends one echo request with the given sequence number and waits up
// to timeout for its reply, draining late replies to earlier probes.
func (p *pingEndpoint) probe(ctx context.Context, seq int, timeout time.Duration) (time.Duration, error) {
	pkt := make([]byte, header.ICMPv6EchoMinimumSize+len(pingPayload))
	pkt[0] = byte(header.ICMPv6EchoRequest)
	pkt[1] = 0 // code
	// checksum and ident are filled in by the stack on send
	binary.BigEndian.PutUint16(pkt[6:header.ICMPv6EchoMinimumSize], uint16(seq))
	copy(pkt[header.ICMPv6EchoMinimumSize:], pingPayload)

	if _, err := p.ep.Write(bytes.NewReader(pkt), tcpip.WriteOptions{}); err != nil {
		return 0, fmt.Errorf("Write: %s", err.String())
	}

	start := time.Now()
	for {
		remaining := timeout - time.Since(start)
		if remaining <= 0 {
			return 0, ErrPingTimeout
		}
		var buf bytes.Buffer
		if _, err := p.ep.Read(&buf, tcpip.ReadOptions{}); err != nil {
			switch err.(type) {
			case *tcpip.ErrWouldBlock:
				select {
				case <-p.notifyCh:
				case <-time.After(remaining):
				case <-ctx.Done():
					return 0, ctx.Err()
				}
				continue
			default:
				return 0, fmt.Errorf("Read: %s", err.String())
			}
		}
		data := buf.Bytes()
		if len(data) >= header.ICMPv6EchoMinimumSize &&
			header.ICMPv6(data).Type() == header.ICMPv6EchoReply &&
			binary.BigEndian.Uint16(data[6:header.ICMPv6EchoMinimumSize]) == uint16(seq) {
			return time.Since(start), nil
		}
		// A late reply to an earlier probe: drain it and keep waiting.
	}
}

// RunPing sends count ICMPv6 echo probes (0 = until ctx is cancelled) to
// dst, every interval, each waiting up to timeout for its reply, reporting
// every probe and the session end through onEvent. It returns when the
// session completes, is cancelled, or fails.
//
// The first probe to a never-contacted destination can be lost to the
// overlay's key lookup; it is reported as a plain timeout.
func (s *YggdrasilNetstack) RunPing(
	ctx context.Context,
	dst net.IP,
	count int,
	timeout, interval time.Duration,
	onEvent func(PingEvent),
) {
	endpoint, err := s.newPingEndpoint(dst)
	if err != nil {
		onEvent(PingEvent{Done: true, Reason: "error", Err: err})
		return
	}
	defer endpoint.close()

	seq := 0
	sent := 0
	for {
		if count != 0 && sent >= count {
			onEvent(PingEvent{Done: true, Reason: "completed"})
			return
		}

		seq++
		rtt, err := endpoint.probe(ctx, seq, timeout)
		switch {
		case errors.Is(err, context.Canceled):
			// The in-flight probe is cancelled, not lost: report no
			// result for it, just end the session.
			onEvent(PingEvent{Done: true, Reason: "stopped"})
			return
		case err != nil && !errors.Is(err, ErrPingTimeout):
			onEvent(PingEvent{Done: true, Reason: "error", Err: err})
			return
		}
		sent++
		onEvent(PingEvent{Seq: seq, RTT: rtt, Err: err})

		if count == 0 || sent < count {
			select {
			case <-time.After(interval):
			case <-ctx.Done():
				onEvent(PingEvent{Done: true, Reason: "stopped"})
				return
			}
		}
	}
}
