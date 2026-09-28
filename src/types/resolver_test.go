package types

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/yggdrasil-network/yggdrasil-go/src/address"
)

const (
	testLookupName = "failover.example.invalid"
	testAnswerIP   = "200:1234:5678:9abc::1"
)

// fakeDNS stands in for the netstack dial: it records the server addresses
// dialed and forwards each one to a local UDP listener with canned DNS
// behavior ("answer" | "blackhole" | "nxdomain").
type fakeDNS struct {
	mu      sync.Mutex
	dialed  []string
	targets map[string]*net.UDPAddr // configured addr -> local listener
}

func (f *fakeDNS) dial(_ context.Context, network, address string) (net.Conn, error) {
	f.mu.Lock()
	f.dialed = append(f.dialed, address)
	target := f.targets[address]
	f.mu.Unlock()
	if target == nil {
		return nil, fmt.Errorf("no fake server for %q", address)
	}
	return net.DialUDP(network, nil, target)
}

func (f *fakeDNS) dialLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.dialed...)
}

// serveUDP answers queries on the listener according to mode; a blackhole
// server swallows queries so the resolver's per-attempt deadline is what
// fires.
func serveUDP(udp *net.UDPConn, mode string) {
	buf := make([]byte, 512)
	for {
		n, from, err := udp.ReadFromUDP(buf)
		if err != nil {
			return
		}
		switch mode {
		case "blackhole":
		case "nxdomain":
			_, _ = udp.WriteToUDP(dnsResponse(buf[:n], 3, nil), from)
		default:
			_, _ = udp.WriteToUDP(dnsResponse(buf[:n], 0, net.ParseIP(testAnswerIP)), from)
		}
	}
}

// dnsResponse builds a minimal single-question reply echoing the query's
// transaction ID and question, with one AAAA record when rcode is 0.
func dnsResponse(query []byte, rcode int, ip net.IP) []byte {
	if len(query) < 12 {
		return nil
	}
	qend := 12
	for qend < len(query) && query[qend] != 0 {
		qend += int(query[qend]) + 1
	}
	qend += 1 + 4 // zero label + QTYPE + QCLASS
	if qend > len(query) {
		return nil
	}

	resp := make([]byte, 0, qend+28)
	resp = append(resp, query[:2]...)          // ID
	resp = append(resp, 0x81, byte(rcode))     // QR=1 RD=1, RCODE
	resp = append(resp, 0, 1)                  // QDCOUNT
	resp = append(resp, 0, 0, 0, 0, 0, 0)      // ANCOUNT/NSCOUNT/ARCOUNT
	resp = append(resp, query[12:qend]...)
	if rcode == 0 && ip != nil {
		binary.BigEndian.PutUint16(resp[6:8], 1) // ANCOUNT
		resp = append(resp, 0xc0, 0x0c)          // NAME pointer to the question
		resp = append(resp, 0, 28)               // TYPE AAAA
		resp = append(resp, 0, 1)                // CLASS IN
		resp = append(resp, 0, 0, 0, 0)          // TTL
		resp = append(resp, 0, 16)               // RDLENGTH
		resp = append(resp, ip.To16()...)
	}
	return resp
}

// newTestResolver builds a failover resolver over the fake DNS: one local
// UDP listener per configured server address, with the given behavior.
func newTestResolver(t *testing.T, modes map[string]string, order []string, attemptTimeout, retryAfter time.Duration) (*NameResolver, *fakeDNS) {
	t.Helper()
	fake := &fakeDNS{targets: make(map[string]*net.UDPAddr)}
	for _, addr := range order {
		if _, ok := fake.targets[addr]; ok {
			continue
		}
		udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
		if err != nil {
			t.Fatalf("listen fake DNS: %v", err)
		}
		fake.targets[addr] = udp.LocalAddr().(*net.UDPAddr)
		t.Cleanup(func() { udp.Close() })
		go serveUDP(udp, modes[addr])
	}
	res := newNameResolver(fake.dial, joinComma(order), attemptTimeout, retryAfter)
	if res.resolver != nil {
		t.Fatal("expected the failover path, got the single-server resolver")
	}
	return res, fake
}

func joinComma(list []string) string {
	out := ""
	for i, s := range list {
		if i > 0 {
			out += ","
		}
		out += s
	}
	return out
}

func TestParseNameservers(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{"308:62:45:62::", []string{"[308:62:45:62::]:dns"}},
		{"1.2.3.4:5353", []string{"1.2.3.4:5353"}},
		{"308:1::, 308:2::", []string{"[308:1::]:dns", "[308:2::]:dns"}},
		{"[308:1::]:53,,308:2::", []string{"[308:1::]:53", "[308:2::]:dns"}},
		{"[308:1::]:53,308:1::", []string{"[308:1::]:53", "[308:1::]:dns"}},
	} {
		if got := parseNameservers(tt.in); len(got) != len(tt.want) {
			t.Errorf("parseNameservers(%q) = %v, want %v", tt.in, got, tt.want)
		} else {
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("parseNameservers(%q) = %v, want %v", tt.in, got, tt.want)
					break
				}
			}
		}
	}
}

func TestSingleServerKeepsLegacyPath(t *testing.T) {
	server := "[308:62:45:62::]:53"
	fake := &fakeDNS{targets: map[string]*net.UDPAddr{}}
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("listen fake DNS: %v", err)
	}
	fake.targets[server] = udp.LocalAddr().(*net.UDPAddr)
	t.Cleanup(func() { udp.Close() })
	go serveUDP(udp, "answer")

	res := newNameResolver(fake.dial, server, time.Second, time.Minute)
	if res.resolver == nil {
		t.Fatal("single server must use the legacy resolver path")
	}

	ctx := context.Background()
	_, ip, err := res.Resolve(ctx, testLookupName)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if ip.String() != testAnswerIP {
		t.Fatalf("Resolve = %s, want %s", ip, testAnswerIP)
	}
	if got := fake.dialLog(); len(got) != 1 || got[0] != server {
		t.Fatalf("dialed %v, want exactly [%s]", got, server)
	}
}

func TestFailoverOnSilentServer(t *testing.T) {
	dead, live := "[308:1::]:53", "[308:2::]:53"
	res, fake := newTestResolver(t,
		map[string]string{dead: "blackhole", live: "answer"},
		[]string{dead, live}, 150*time.Millisecond, time.Hour)

	_, ip, err := res.Resolve(context.Background(), testLookupName)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if ip.String() != testAnswerIP {
		t.Fatalf("Resolve = %s, want %s", ip, testAnswerIP)
	}
	if got := fake.dialLog(); len(got) != 2 || got[0] != dead || got[1] != live {
		t.Fatalf("dialed %v, want [%s %s]", got, dead, live)
	}
}

func TestFailoverSticksToAnsweringServer(t *testing.T) {
	dead, live := "[308:1::]:53", "[308:2::]:53"
	res, fake := newTestResolver(t,
		map[string]string{dead: "blackhole", live: "answer"},
		[]string{dead, live}, 150*time.Millisecond, time.Hour)

	if _, _, err := res.Resolve(context.Background(), testLookupName); err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	if _, _, err := res.Resolve(context.Background(), testLookupName); err != nil {
		t.Fatalf("second Resolve: %v", err)
	}
	// Sticky: the second lookup must start at the server that answered.
	if got := fake.dialLog(); len(got) != 3 || got[2] != live {
		t.Fatalf("dialed %v, want the second lookup to start at %s", got, live)
	}
}

func TestPreferredServerReprobedAfterCooldown(t *testing.T) {
	dead, live := "[308:1::]:53", "[308:2::]:53"
	res, fake := newTestResolver(t,
		map[string]string{dead: "blackhole", live: "answer"},
		[]string{dead, live}, 150*time.Millisecond, 100*time.Millisecond)

	if _, _, err := res.Resolve(context.Background(), testLookupName); err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	time.Sleep(150 * time.Millisecond)
	if _, _, err := res.Resolve(context.Background(), testLookupName); err != nil {
		t.Fatalf("second Resolve: %v", err)
	}
	got := fake.dialLog()
	if len(got) != 4 || got[2] != dead || got[3] != live {
		t.Fatalf("dialed %v, want the second lookup to re-probe %s first", got, dead)
	}
}

func TestAuthoritativeNotFoundDoesNotFailOver(t *testing.T) {
	nx, live := "[308:1::]:53", "[308:2::]:53"
	res, fake := newTestResolver(t,
		map[string]string{nx: "nxdomain", live: "answer"},
		[]string{nx, live}, time.Second, time.Hour)

	_, _, err := res.Resolve(context.Background(), testLookupName)
	if err == nil {
		t.Fatal("Resolve should fail for a name the server says does not exist")
	}
	var dnsErr *net.DNSError
	if !errors.As(err, &dnsErr) || !dnsErr.IsNotFound {
		t.Fatalf("error = %v, want a not-found DNSError", err)
	}
	if got := fake.dialLog(); len(got) != 1 || got[0] != nx {
		t.Fatalf("dialed %v, want exactly [%s] — an answer must not fail over", got, nx)
	}
}

func TestFailoverReturnsErrorWhenAllServersFail(t *testing.T) {
	dead1, dead2 := "[308:1::]:53", "[308:2::]:53"
	res, _ := newTestResolver(t,
		map[string]string{dead1: "blackhole", dead2: "blackhole"},
		[]string{dead1, dead2}, 100*time.Millisecond, time.Hour)

	_, _, err := res.Resolve(context.Background(), testLookupName)
	if err == nil {
		t.Fatal("Resolve should fail when every server is silent")
	}
	if want := "all 2 nameservers failed"; len(err.Error()) < len(want) || err.Error()[:len(want)] != want {
		t.Fatalf("error = %q, want prefix %q", err.Error(), want)
	}
}

func TestFailoverResolverSkipsDNSForSpecialNames(t *testing.T) {
	dead, live := "[308:1::]:53", "[308:2::]:53"
	res, fake := newTestResolver(t,
		map[string]string{dead: "blackhole", live: "answer"},
		[]string{dead, live}, time.Second, time.Hour)

	// IP literals resolve without any server.
	_, ip, err := res.Resolve(context.Background(), "300::1")
	if err != nil || ip.String() != "300::1" {
		t.Fatalf("IP literal Resolve = %v, %v; want 300::1, nil", ip, err)
	}

	// .pk.ygg names derive from the embedded public key.
	_, ip, err = res.Resolve(context.Background(), "alias."+pubkeyForTest()+".pk.ygg")
	if err != nil {
		t.Fatalf(".pk.ygg Resolve: %v", err)
	}
	var want [16]byte
	copy(want[:], address.AddrForKey(pubkeyBytesForTest())[:])
	if !ip.Equal(net.IP(want[:])) {
		t.Fatalf(".pk.ygg Resolve = %s, want %s", ip, net.IP(want[:]))
	}

	if got := fake.dialLog(); len(got) != 0 {
		t.Fatalf("dialed %v, want no DNS traffic", got)
	}
}

func pubkeyBytesForTest() []byte {
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i + 1)
	}
	return raw
}

func pubkeyForTest() string {
	const hexDigits = "0123456789abcdef"
	out := make([]byte, 0, 64)
	for _, b := range pubkeyBytesForTest() {
		out = append(out, hexDigits[b>>4], hexDigits[b&0xf])
	}
	return string(out)
}
