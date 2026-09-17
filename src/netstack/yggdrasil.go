package netstack

import (
	"log"
	"net"
	"sync"

	"github.com/yggdrasil-network/yggdrasil-go/src/core"
	"github.com/yggdrasil-network/yggdrasil-go/src/ipv6rwc"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
)

type YggdrasilNIC struct {
	interrupt  func() error
	closeOnce  sync.Once
	workers    sync.WaitGroup
	stateMu    sync.RWMutex
	closed     bool
	ipv6rwc    *ipv6rwc.ReadWriteCloser
	dispatcher stack.NetworkDispatcher
	rstPackets chan *stack.PacketBuffer

	// writeBuf is reused across packets; WritePackets is invoked
	// concurrently from several transport goroutines, so access is
	// serialized. (A shared unguarded buffer here caused heap corruption.)
	writeMu  sync.Mutex
	writeBuf []byte
}

func (s *YggdrasilNetstack) NewYggdrasilNIC(ygg *core.Core) tcpip.Error {
	rwc := ipv6rwc.NewReadWriteCloser(ygg)
	mtu := rwc.MTU()
	rxPool := &sync.Pool{New: func() interface{} {
		return make([]byte, mtu)
	}}
	nic := &YggdrasilNIC{
		ipv6rwc:    rwc,
		interrupt:  ygg.Close,
		writeBuf:   make([]byte, mtu),
		rstPackets: make(chan *stack.PacketBuffer, 100),
	}
	s.nic = nic
	if err := s.stack.CreateNIC(1, nic); err != nil {
		return err
	}
	nic.workers.Add(2)
	go func() {
		defer nic.workers.Done()
		for {
			// Scratch buffer is pooled; the delivered payload is a private
			// copy because gvisor endpoints may retain views beyond this
			// iteration, so the scratch memory must not be recycled early.
			buf := rxPool.Get().([]byte)
			rx, err := nic.ipv6rwc.Read(buf)
			if err != nil {
				rxPool.Put(buf)
				break
			}
			payload := make([]byte, rx)
			copy(payload, buf[:rx])
			rxPool.Put(buf)
			pkb := stack.NewPacketBuffer(stack.PacketBufferOptions{
				Payload: buffer.MakeWithData(payload),
			})
			nic.stateMu.RLock()
			dispatcher := nic.dispatcher
			closed := nic.closed
			nic.stateMu.RUnlock()
			if !closed && dispatcher != nil {
				dispatcher.DeliverNetworkPacket(ipv6.ProtocolNumber, pkb)
			}
			pkb.DecRef()
		}
	}()
	go func() {
		defer nic.workers.Done()
		for pkt := range nic.rstPackets {
			_ = nic.writePacket(pkt)
			pkt.DecRef()
		}
	}()
	_, snet, err := net.ParseCIDR("0200::/7")
	if err != nil {
		return &tcpip.ErrBadAddress{}
	}
	subnet, err := tcpip.NewSubnet(
		tcpip.AddrFromSlice(snet.IP.To16()),
		tcpip.MaskFrom(string(snet.Mask)),
	)
	if err != nil {
		return &tcpip.ErrBadAddress{}
	}
	s.stack.AddRoute(tcpip.Route{
		Destination: subnet,
		NIC:         1,
	})
	if s.stack.HandleLocal() {
		ip := ygg.Address()
		if err := s.stack.AddProtocolAddress(
			1,
			tcpip.ProtocolAddress{
				Protocol:          ipv6.ProtocolNumber,
				AddressWithPrefix: tcpip.AddrFromSlice(ip.To16()).WithPrefix(),
			},
			stack.AddressProperties{},
		); err != nil {
			return err
		}
	}
	return nil
}

func (e *YggdrasilNIC) Attach(dispatcher stack.NetworkDispatcher) {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	e.dispatcher = dispatcher
}

func (e *YggdrasilNIC) IsAttached() bool {
	e.stateMu.RLock()
	defer e.stateMu.RUnlock()
	return e.dispatcher != nil
}

func (e *YggdrasilNIC) MTU() uint32 { return uint32(e.ipv6rwc.MTU()) }

func (e *YggdrasilNIC) SetMTU(uint32) {}

func (*YggdrasilNIC) Capabilities() stack.LinkEndpointCapabilities { return stack.CapabilityNone }

func (*YggdrasilNIC) MaxHeaderLength() uint16 { return 40 }

func (*YggdrasilNIC) LinkAddress() tcpip.LinkAddress { return "" }

func (*YggdrasilNIC) SetLinkAddress(tcpip.LinkAddress) {}

func (e *YggdrasilNIC) Wait() { e.workers.Wait() }

func (e *YggdrasilNIC) writePacket(
	pkt *stack.PacketBuffer,
) tcpip.Error {
	// We need to recover from panic() here because
	// parser in ToView() gets confused on some packets
	// without payload and panics
	defer func() {
		r := recover()
		if r != nil {
		}
	}()
	// Serialize access to the reused write buffer; WritePackets is invoked
	// concurrently from several transport goroutines.
	e.writeMu.Lock()
	defer e.writeMu.Unlock()
	e.stateMu.RLock()
	closed := e.closed
	e.stateMu.RUnlock()
	if closed {
		return &tcpip.ErrClosedForSend{}
	}
	vv := pkt.ToView()
	defer vv.Release()
	if vv.Size() > len(e.writeBuf) {
		e.writeBuf = make([]byte, vv.Size())
	}
	n, err := vv.Read(e.writeBuf)
	if err != nil {
		return &tcpip.ErrAborted{}
	}
	if _, err := e.ipv6rwc.Write(e.writeBuf[:n]); err != nil {
		return &tcpip.ErrAborted{}
	}
	return nil
}

func (e *YggdrasilNIC) WritePackets(
	list stack.PacketBufferList,
) (int, tcpip.Error) {
	var i int = 0
	var err tcpip.Error = nil
	for i, pkt := range list.AsSlice() {
		if pkt.Data().Size() == 0 {
			if pkt.Network().TransportProtocol() == tcp.ProtocolNumber {
				tcpHeader := header.TCP(pkt.TransportHeader().Slice())
				if (tcpHeader.Flags() & header.TCPFlagRst) == header.TCPFlagRst {
					e.stateMu.RLock()
					if e.closed {
						e.stateMu.RUnlock()
						return i, &tcpip.ErrClosedForSend{}
					}
					pkt.IncRef()
					select {
					case e.rstPackets <- pkt:
						// Packet queued successfully
					default:
						// Channel full, drop packet and release ref
						pkt.DecRef()
					}
					e.stateMu.RUnlock()
					continue
				}
			}
		}
		err = e.writePacket(pkt)
		if err != nil {
			log.Println(err)
			return i - 1, err
		}
	}

	return i, nil
}

func (e *YggdrasilNIC) WriteRawPacket(*stack.PacketBuffer) tcpip.Error {
	panic("not implemented")
}

func (*YggdrasilNIC) ARPHardwareType() header.ARPHardwareType {
	return header.ARPHardwareNone
}

func (e *YggdrasilNIC) AddHeader(*stack.PacketBuffer) {
}

func (e *YggdrasilNIC) ParseHeader(*stack.PacketBuffer) bool {
	return true
}

func (e *YggdrasilNIC) Close() {
	e.closeOnce.Do(func() {
		e.stateMu.Lock()
		e.closed = true
		close(e.rstPackets)
		e.stateMu.Unlock()
		// Closing the packet transport unblocks Read and any in-flight Write.
		// Do not recursively remove the NIC here: gVisor calls Close itself.
		if e.interrupt != nil {
			_ = e.interrupt()
		}
	})
}

func (e *YggdrasilNIC) SetOnCloseAction(func()) {}
