package netstack

import (
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/gologme/log"
	"github.com/yggdrasil-network/yggdrasil-go/src/config"
	"github.com/yggdrasil-network/yggdrasil-go/src/core"
)

func TestCloseJoinsNICAndEndpoints(t *testing.T) {
	cfg := config.GenerateConfig()
	if err := cfg.GenerateSelfSignedCertificate(); err != nil {
		t.Fatal(err)
	}
	c, err := core.New(cfg.Certificate, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Stop()
	s, err := CreateYggdrasilNetstack(c)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := s.ListenTCP(&net.TCPAddr{IP: c.Address()})
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan error, 1)
	go func() { _, err := listener.Accept(); accepted <- err }()
	done := make(chan struct{})
	go func() {
		var wg sync.WaitGroup
		for range 4 {
			wg.Go(s.Close)
		}
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not join NIC workers")
	}
	select {
	case err := <-accepted:
		if err == nil {
			t.Fatal("accept survived close")
		}
	case <-time.After(time.Second):
		t.Fatal("accept did not unblock")
	}
	if s.nic.IsAttached() {
		t.Fatal("NIC still attached")
	}
	s.nic.Wait()
}
