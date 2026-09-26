package types

import (
	"bufio"
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// fakeHTTPProxy builds an HTTPProxy whose Dial reaches a local TCP listener
// (the "origin") instead of the Yggdrasil netstack. IP-literal targets skip
// DNS, so the empty nameserver resolver is enough.
func fakeHTTPProxy(t *testing.T) *HTTPProxy {
	t.Helper()
	return &HTTPProxy{
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
		Resolver: NewNameResolver(nil, ""),
		MTU:      65535,
	}
}

func TestHTTPProxyConnect(t *testing.T) {
	origin, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer origin.Close()
	originAddr := origin.Addr().String()

	originDone := make(chan string, 1)
	go func() {
		c, err := origin.Accept()
		if err != nil {
			originDone <- err.Error()
			return
		}
		defer c.Close()
		buf := make([]byte, 5)
		if _, err := io.ReadFull(c, buf); err != nil {
			originDone <- err.Error()
			return
		}
		if _, err := c.Write([]byte("world")); err != nil {
			originDone <- err.Error()
			return
		}
		originDone <- string(buf)
	}()

	client, server := net.Pipe()
	proxy := fakeHTTPProxy(t)
	go proxy.ServeConn(context.Background(), server)

	go func() {
		io.WriteString(client, "CONNECT "+originAddr+" HTTP/1.1\r\nHost: "+originAddr+"\r\n\r\nhello")
	}()

	br := bufio.NewReader(client)
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(line, "HTTP/1.1 200") {
		t.Fatalf("CONNECT reply = %q", line)
	}
	// Consume the rest of the (empty) response head.
	for {
		l, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if l == "\r\n" {
			break
		}
	}
	echoed, err := io.ReadAll(br)
	if err != nil {
		t.Fatal(err)
	}
	if string(echoed) != "world" {
		t.Fatalf("tunnel payload = %q", echoed)
	}
	client.Close()

	select {
	case got := <-originDone:
		if got != "hello" {
			t.Fatalf("origin received %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("origin handler did not finish")
	}
}

func TestHTTPProxyForward(t *testing.T) {
	origin, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer origin.Close()
	originAddr := origin.Addr().String()

	gotRequest := make(chan string, 1)
	go func() {
		c, err := origin.Accept()
		if err != nil {
			gotRequest <- err.Error()
			return
		}
		br := bufio.NewReader(c)
		head, err := readHTTPHead(br)
		if err != nil {
			gotRequest <- err.Error()
			return
		}
		io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nhi")
		c.Close()
		gotRequest <- string(head)
	}()

	client, server := net.Pipe()
	proxy := fakeHTTPProxy(t)
	go proxy.ServeConn(context.Background(), server)

	go func() {
		io.WriteString(client, "GET http://"+originAddr+"/path?q=1 HTTP/1.1\r\nHost: "+originAddr+"\r\nProxy-Connection: keep-alive\r\n\r\n")
	}()

	resp, err := io.ReadAll(client)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(resp), "200 OK") || !strings.HasSuffix(string(resp), "hi") {
		t.Fatalf("response = %q", resp)
	}

	select {
	case head := <-gotRequest:
		if !strings.HasPrefix(head, "GET /path?q=1 HTTP/1.1\r\n") {
			t.Fatalf("rewritten request line in %q", head)
		}
		if !strings.Contains(head, "Host: "+originAddr) {
			t.Fatalf("Host header missing in %q", head)
		}
		if !strings.Contains(head, "Connection: close") {
			t.Fatalf("Connection: close missing in %q", head)
		}
		if strings.Contains(head, "Proxy-Connection") {
			t.Fatalf("Proxy-Connection not dropped in %q", head)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("origin handler did not finish")
	}
}

func TestParseForwardTargetOriginForm(t *testing.T) {
	h := &httpHead{
		method:  "GET",
		target:  "/x/y?z",
		version: "HTTP/1.1",
		headers: []string{"Host: example.com:8080"},
	}
	target, err := parseForwardTarget(h)
	if err != nil {
		t.Fatal(err)
	}
	if target.host != "example.com" || target.port != "8080" || target.originForm != "/x/y?z" {
		t.Fatalf("target = %+v", target)
	}
}

func TestRewriteForwardHeadSynthesizesHost(t *testing.T) {
	h := &httpHead{
		method:  "GET",
		target:  "http://example.com/",
		version: "HTTP/1.0",
		headers: []string{"User-Agent: test"},
	}
	out := rewriteForwardHead(h, &httpForward{originForm: "/", authority: "example.com"})
	if !strings.HasPrefix(out, "GET / HTTP/1.0\r\n") {
		t.Fatalf("request line in %q", out)
	}
	if !strings.Contains(out, "Host: example.com\r\n") || !strings.Contains(out, "Connection: close\r\n") {
		t.Fatalf("synthesized headers missing in %q", out)
	}
}
