package mobile

import (
	"context"
	"io"
	"net"
	"sync"
)

// workerScope owns one run, mapping, or SOCKS client. Closing seals resource
// registration before closing sockets, so an accept/dial racing cancellation
// cannot escape cleanup. Only an existing worker (or the serialized launcher)
// may spawn another worker: the WaitGroup cannot become zero before the last Add.
// Workers never take Yggstack.mu. Public lifecycle/mapping operations hold it
// through stop, preventing replacement until the old scope has been joined.
type workerScope struct {
	ctx       context.Context
	cancel    context.CancelFunc
	mu        sync.Mutex
	closed    bool
	resources map[io.Closer]struct{}
	wg        sync.WaitGroup
	closeOnce sync.Once
}

func newWorkerScope(parent context.Context) *workerScope {
	ctx, cancel := context.WithCancel(parent)
	return &workerScope{ctx: ctx, cancel: cancel, resources: make(map[io.Closer]struct{})}
}

func (s *workerScope) own(c io.Closer) bool {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		c.Close()
		return false
	}
	s.resources[c] = struct{}{}
	s.mu.Unlock()
	return true
}

func (s *workerScope) forget(c io.Closer) {
	s.mu.Lock()
	delete(s.resources, c)
	s.mu.Unlock()
}

func (s *workerScope) conn(c net.Conn) net.Conn {
	owned := &ownedConn{Conn: c, scope: s}
	s.own(owned)
	return owned
}

func (s *workerScope) goWorker(f func()) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		f()
	}()
}

// Submit implements go-socks5's GPool, including its internal relay workers.
// Always return nil: returning an error makes the library spawn an unowned worker.
func (s *workerScope) Submit(f func()) error {
	s.goWorker(f)
	return nil
}

func (s *workerScope) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.cancel()
		resources := s.resources
		s.resources = make(map[io.Closer]struct{})
		s.mu.Unlock()
		for c := range resources {
			c.Close()
		}
	})
	return nil
}

func (s *workerScope) stop() {
	s.Close()
	s.wg.Wait()
}

type ownedConn struct {
	net.Conn
	scope *workerScope
	once  sync.Once
	err   error
}

func (c *ownedConn) Close() error {
	c.once.Do(func() {
		c.err = c.Conn.Close()
		c.scope.forget(c)
	})
	return c.err
}

// Called only under y.mu. Registry entries belong to instances, not handlers:
// old workers never delete a shared key that could identify a replacement.
func (y *Yggstack) startMapping(key string, handler func(*workerScope)) {
	y.stopMapping(key)
	s := newWorkerScope(y.run.ctx)
	y.mappings[key] = s
	s.goWorker(func() {
		defer s.Close()
		handler(s)
	})
}

func (y *Yggstack) stopMapping(key string) {
	if s := y.mappings[key]; s != nil {
		s.stop()
		delete(y.mappings, key)
	}
	y.removeListenerStats(key)
}
