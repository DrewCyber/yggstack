package types

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/yggdrasil-network/yggdrasil-go/src/address"
	"github.com/yggdrasil-network/yggstack/src/netstack"
)

const NameMappingSuffix = ".pk.ygg"

// Multi-nameserver tuning: per-server query budget, and how long to stay on a
// fallback server before re-probing the preferred (first) one.
const (
	nameserverAttemptTimeout     = 4 * time.Second
	nameserverRetryPreferredAfter = 60 * time.Second
)

type dialFunc func(ctx context.Context, network, address string) (net.Conn, error)

// NameResolver resolves proxy hostnames. Configured with a single nameserver
// it behaves exactly like the historical resolver; with a comma-separated
// list it queries the servers in order, failing over to the next one when a
// server is unreachable or silent, and sticks with the last server that
// answered.
type NameResolver struct {
	// Single-nameserver path (set unless 2+ servers are configured).
	resolver *net.Resolver

	// Failover path.
	dial                dialFunc
	servers             []string
	attemptTimeout      time.Duration
	retryPreferredAfter time.Duration

	mu            sync.Mutex
	current       int       // server index the next lookup starts from
	preferredDown time.Time // when the preferred (first) server last failed
}

// NewNameResolver builds a resolver dialing through the netstack.
// nameservers is a single "host[:port]" server or a comma-separated list
// tried in order with failover; each entry defaults to the dns service port.
func NewNameResolver(stack *netstack.YggdrasilNetstack, nameservers string) *NameResolver {
	return newNameResolver(stack.DialContext, nameservers,
		nameserverAttemptTimeout, nameserverRetryPreferredAfter)
}

func newNameResolver(dial dialFunc, nameservers string, attemptTimeout, retryPreferredAfter time.Duration) *NameResolver {
	res := &NameResolver{
		dial:                dial,
		servers:             parseNameservers(nameservers),
		attemptTimeout:      attemptTimeout,
		retryPreferredAfter: retryPreferredAfter,
	}
	if len(res.servers) <= 1 {
		var server string
		if len(res.servers) == 1 {
			server = res.servers[0]
		}
		res.resolver = goResolver(dial, server)
	}
	return res
}

// goResolver returns a resolver whose queries all go to server (or to the
// system default when server is empty).
func goResolver(dial dialFunc, server string) *net.Resolver {
	res := &net.Resolver{
		PreferGo: true,
	}
	if server != "" {
		res.Dial = func(ctx context.Context, network, _ string) (net.Conn, error) { // nolint:staticcheck
			return dial(ctx, network, server)
		}
	}
	return res
}

// parseNameservers splits a comma-separated nameserver list, defaulting each
// entry to the dns service port the way the single-server form always did.
func parseNameservers(nameservers string) []string {
	var servers []string
	for _, entry := range strings.Split(nameservers, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		host, port, err := net.SplitHostPort(entry)
		if err != nil {
			// default to dns service when no port given.
			port = "dns"
			host = entry
		}
		servers = append(servers, net.JoinHostPort(host, port))
	}
	return servers
}

func (r *NameResolver) Resolve(ctx context.Context, name string) (context.Context, net.IP, error) {
	if strings.HasSuffix(name, NameMappingSuffix) {
		name = strings.TrimSuffix(name, NameMappingSuffix)
		// Check if remaining string contains a dot and
		// assume publickey is a rightmost token
		name = name[strings.LastIndex(name, ".")+1:]
		var pk [ed25519.PublicKeySize]byte
		if b, err := hex.DecodeString(name); err != nil {
			return nil, nil, fmt.Errorf("hex.DecodeString: %w", err)
		} else {
			copy(pk[:], b)
			return ctx, net.IP(address.AddrForKey(pk[:])[:]), nil
		}
	}
	ip := net.ParseIP(name)
	if ip != nil {
		return ctx, ip, nil
	}
	if r.resolver != nil {
		ip, err := lookupIP(ctx, r.resolver, name)
		if err != nil {
			return nil, nil, err
		}
		return ctx, ip, nil
	}
	return r.resolveFailover(ctx, name)
}

// lookupIP resolves name to its first address, printing the failure the way
// the resolver always has; the returned error is the raw lookup error so
// callers can classify it.
func lookupIP(ctx context.Context, resolver *net.Resolver, name string) (net.IP, error) {
	addrs, err := resolver.LookupIP(ctx, "ip6", name)
	if err != nil {
		fmt.Println("failed to lookup", name, "due to error:", err)
		return nil, err
	}
	if len(addrs) == 0 {
		fmt.Println("failed to lookup", name, "due to no addresses")
		return nil, fmt.Errorf("no addresses for %q", name)
	}
	return addrs[0], nil
}

// resolveFailover walks the server list starting at the last server that
// answered, giving each attempt its own deadline so a silent server cannot
// consume the caller's whole budget.
func (r *NameResolver) resolveFailover(ctx context.Context, name string) (context.Context, net.IP, error) {
	start := r.startIndex()
	var lastErr error
	for i := range r.servers {
		idx := (start + i) % len(r.servers)
		attemptCtx, cancel := context.WithTimeout(ctx, r.attemptTimeout)
		ip, err := lookupIP(attemptCtx, goResolver(r.dial, r.servers[idx]), name)
		cancel()
		if err == nil {
			r.noteSuccess(idx)
			return ctx, ip, nil
		}
		if isNotFound(err) {
			// The server answered authoritatively that the name does not
			// exist; failing over would only re-ask the same question.
			r.noteSuccess(idx)
			return nil, nil, err
		}
		r.noteFailure(idx)
		lastErr = err
	}
	return nil, nil, fmt.Errorf("all %d nameservers failed to lookup %q: %w", len(r.servers), name, lastErr)
}

// isNotFound reports whether err is a definitive "no such host" answer from a
// responsive server, as opposed to a timeout or unreachable server.
func isNotFound(err error) bool {
	var dnsErr *net.DNSError
	return errors.As(err, &dnsErr) &&
		dnsErr.IsNotFound && !dnsErr.IsTimeout && !dnsErr.IsTemporary
}

func (r *NameResolver) startIndex() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.current != 0 && !r.preferredDown.IsZero() &&
		time.Since(r.preferredDown) >= r.retryPreferredAfter {
		// Re-probe the preferred server; if it is still down the walk
		// fails over again within this same lookup.
		r.current = 0
	}
	return r.current
}

func (r *NameResolver) noteSuccess(idx int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.current = idx
	if idx == 0 {
		r.preferredDown = time.Time{}
	}
}

func (r *NameResolver) noteFailure(idx int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if idx == 0 && r.preferredDown.IsZero() {
		r.preferredDown = time.Now()
	}
	if r.current == idx {
		r.current = (idx + 1) % len(r.servers)
	}
}
