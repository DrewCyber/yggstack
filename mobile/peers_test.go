package mobile

import (
	"net"
	"strings"
	"testing"
	"time"
)

// recordingPeerCallback collects delivered snapshots in order.
type recordingPeerCallback struct {
	ch chan string
}

func (c *recordingPeerCallback) OnPeersChanged(peersJSON string) {
	c.ch <- peersJSON
}

func recvSnapshot(t *testing.T, ch chan string) string {
	t.Helper()
	select {
	case s := <-ch:
		return s
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for peer snapshot")
		return ""
	}
}

// TestPeerCallback verifies the snapshot callback contract: a baseline
// delivery right after Start, a re-delivery carrying a newly added peer
// (down, since its target port is dead), and a fresh baseline on the
// next run — proving the forwarder is owned by the run and Stop joins it.
func TestPeerCallback(t *testing.T) {
	y := offlineNode(t)

	cb := &recordingPeerCallback{ch: make(chan string, 16)}
	y.SetPeerCallback(cb)

	if err := y.Start("", "", ""); err != nil {
		t.Fatal(err)
	}
	if got := recvSnapshot(t, cb.ch); got != "[]" {
		t.Fatalf("baseline snapshot = %q, want []", got)
	}

	// A dead port: the peer entry appears (Up:false) but never connects.
	dead, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadAddr := dead.Addr().String()
	dead.Close()
	if err := y.AddLivePeer("tcp://" + deadAddr + "?maxbackoff=5s"); err != nil {
		t.Fatal(err)
	}

	got := recvSnapshot(t, cb.ch)
	if !strings.Contains(got, `"URI":"tcp://`+deadAddr) ||
		!strings.Contains(got, `"Up":false`) {
		t.Fatalf("snapshot after AddLivePeer = %q, want peer %s down", got, deadAddr)
	}

	stopNode(t, y)
	if err := y.Start("", "", ""); err != nil {
		t.Fatal(err)
	}
	if got := recvSnapshot(t, cb.ch); got != "[]" {
		t.Fatalf("second run baseline snapshot = %q, want []", got)
	}
	stopNode(t, y)
}
