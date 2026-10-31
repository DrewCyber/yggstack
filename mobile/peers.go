package mobile

import (
	"time"

	"github.com/yggdrasil-network/yggdrasil-go/src/core"
)

// PeerCallback is invoked with a full peer snapshot — the same JSON
// array GetPeersJSON returns — whenever the connected-peer set changes,
// plus once right after a successful Start as a baseline.
type PeerCallback interface {
	OnPeersChanged(peersJSON string)
}

// peerDebounce coalesces bursts of link transitions (e.g. a network
// flap dropping several peers at once) into a single snapshot delivery.
const peerDebounce = 250 * time.Millisecond

// SetPeerCallback registers a callback receiving the peer snapshot on
// every change of the connected-peer set. The registration takes effect
// at the next Start; snapshots are delivered from a worker goroutine
// owned by that run.
func (y *Yggstack) SetPeerCallback(cb PeerCallback) {
	y.mu.Lock()
	defer y.mu.Unlock()
	y.peerCallback = cb
}

// startPeerForwarder wires the core's peer listener to the registered
// callback. Called under y.mu with y.core and y.run freshly created, so
// the worker below never touches Yggstack fields after this returns: it
// captures the core and callback of this run and stops with the run's
// context.
func (y *Yggstack) startPeerForwarder() {
	cb := y.peerCallback
	if cb == nil {
		return
	}
	c := y.core
	ticks := make(chan struct{}, 16)
	c.SetPeerListener(func(core.PeerEvent) {
		// Non-blocking: a pending tick already guarantees a fresh
		// snapshot, and the listener must not stall the links actor.
		select {
		case ticks <- struct{}{}:
		default:
		}
	})
	ctx := y.run.ctx
	y.run.goWorker(func() {
		// Baseline: the consumer starts from a known state without polling.
		cb.OnPeersChanged(buildPeersJSON(c))
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticks:
			}
			timer := time.NewTimer(peerDebounce)
		debounce:
			for {
				select {
				case <-ticks:
					timer.Reset(peerDebounce)
				case <-timer.C:
					break debounce
				case <-ctx.Done():
					timer.Stop()
					return
				}
			}
			cb.OnPeersChanged(buildPeersJSON(c))
		}
	})
}
