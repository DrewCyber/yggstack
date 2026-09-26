package types

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

// httpHeadLimit bounds the request head (request line + headers) the proxy
// is willing to parse; larger heads are dropped. Mirrors the ng engine.
const httpHeadLimit = 16 * 1024

var httpHeadEnd = []byte("\r\n\r\n")

// HTTPProxy implements the HTTP proxy protocol on one connection at a time:
// CONNECT tunnels (HTTPS and other TCP protocols) plus forwarding of
// plain-HTTP absolute-URI requests, rewritten to origin-form with
// `Connection: close` — one origin connection per request, the client
// connection closes when the response completes. Hosts are resolved through
// Resolver (IPv6/IPv4 literals and .pk.ygg pass through; everything else
// goes via the configured nameserver).
type HTTPProxy struct {
	// Dial opens the Yggdrasil-facing leg of a relay.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// Resolver resolves request hostnames; must be non-nil.
	Resolver *NameResolver
	// MTU sizes the relay buffers (the node's Yggdrasil MTU).
	MTU uint64
}

// ServeConn handles one accepted client connection to completion.
func (p *HTTPProxy) ServeConn(ctx context.Context, conn net.Conn) {
	reader := bufio.NewReaderSize(conn, 8192)
	head, err := readHTTPHead(reader)
	if err != nil {
		return // not a parseable HTTP request; just drop the connection
	}
	parsed, err := parseHTTPHead(head)
	if err != nil {
		writeHTTPResponse(conn, 400, "Bad Request")
		return
	}

	// 10s budgets, like the SOCKS dial path.
	resolve := func(host string) (net.IP, error) {
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		_, ip, err := p.Resolver.Resolve(rctx, host)
		return ip, err
	}
	dial := func(ip net.IP, port string) (net.Conn, error) {
		if ip == nil {
			return nil, fmt.Errorf("no address")
		}
		dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		return p.Dial(dctx, "tcp", net.JoinHostPort(ip.String(), port))
	}

	if parsed.method == "CONNECT" {
		p.serveConnect(conn, reader, parsed, resolve, dial)
		return
	}
	p.serveForward(conn, reader, parsed, resolve, dial)
}

func (p *HTTPProxy) serveConnect(conn net.Conn, reader *bufio.Reader, h *httpHead,
	resolve func(string) (net.IP, error), dial func(net.IP, string) (net.Conn, error)) {
	host, port, err := splitHTTPAuthority(h.target, "443")
	if err != nil {
		writeHTTPResponse(conn, 400, "Bad Request")
		return
	}
	ip, err := resolve(host)
	if err != nil {
		writeHTTPResponse(conn, 502, "Bad Gateway")
		return
	}
	origin, err := dial(ip, port)
	if err != nil {
		writeHTTPResponse(conn, 502, "Bad Gateway")
		return
	}
	if _, err := conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		origin.Close()
		return
	}
	// Bytes the client pipelined behind the CONNECT head belong to the tunnel.
	if err := flushBuffered(reader, origin); err != nil {
		origin.Close()
		return
	}
	ProxyTCP(p.MTU, conn, origin)
}

func (p *HTTPProxy) serveForward(conn net.Conn, reader *bufio.Reader, h *httpHead,
	resolve func(string) (net.IP, error), dial func(net.IP, string) (net.Conn, error)) {
	target, err := parseForwardTarget(h)
	if err != nil {
		writeHTTPResponse(conn, 400, "Bad Request")
		return
	}
	ip, err := resolve(target.host)
	if err != nil {
		writeHTTPResponse(conn, 502, "Bad Gateway")
		return
	}
	origin, err := dial(ip, target.port)
	if err != nil {
		writeHTTPResponse(conn, 502, "Bad Gateway")
		return
	}
	// Rewritten head is Connection: close, so the origin closes after the
	// response and ProxyTCP tears both legs down; the client sees the close.
	if _, err := origin.Write([]byte(rewriteForwardHead(h, target))); err != nil {
		origin.Close()
		return
	}
	if err := flushBuffered(reader, origin); err != nil {
		origin.Close()
		return
	}
	ProxyTCP(p.MTU, conn, origin)
}

// readHTTPHead reads through the blank line ending the request head. Bytes
// read past it stay buffered in r for flushBuffered.
func readHTTPHead(r *bufio.Reader) ([]byte, error) {
	var head []byte
	for {
		if bytes.HasSuffix(head, httpHeadEnd) {
			return head, nil
		}
		if len(head) > httpHeadLimit {
			return nil, fmt.Errorf("request head too large")
		}
		line, err := r.ReadBytes('\n')
		if err != nil {
			return nil, err
		}
		head = append(head, line...)
	}
}

// flushBuffered writes bytes the bufio reader consumed past the request
// head (start of a request body or tunnel payload) to dst.
func flushBuffered(r *bufio.Reader, dst io.Writer) error {
	n := r.Buffered()
	if n == 0 {
		return nil
	}
	buf, err := r.Peek(n)
	if err != nil {
		return err
	}
	_, err = dst.Write(buf)
	return err
}

// httpHead is one parsed request head.
type httpHead struct {
	method  string
	target  string // authority for CONNECT, absolute-URI or origin-form otherwise
	version string
	headers []string // verbatim header lines, no CRLF
}

func parseHTTPHead(head []byte) (*httpHead, error) {
	text := strings.TrimSuffix(string(head), "\r\n\r\n")
	lines := strings.Split(text, "\r\n")
	parts := strings.Fields(lines[0])
	if len(parts) != 3 {
		return nil, fmt.Errorf("malformed request line %q", lines[0])
	}
	return &httpHead{method: parts[0], target: parts[1], version: parts[2], headers: lines[1:]}, nil
}

// splitHTTPAuthority splits "host:port" / "[v6]:port" / "host" into host
// and port, falling back to defaultPort when none is given.
func splitHTTPAuthority(authority, defaultPort string) (string, string, error) {
	if host, port, err := net.SplitHostPort(authority); err == nil {
		if _, err := strconv.Atoi(port); err != nil {
			return "", "", fmt.Errorf("invalid port in %q", authority)
		}
		return host, port, nil
	}
	return authority, defaultPort, nil
}

// httpForward is where a forwarded (non-CONNECT) request goes.
type httpForward struct {
	host       string
	port       string
	originForm string // path (+query) for the rewritten request line
	authority  string // for a synthesized Host header when the client sent none
}

// parseForwardTarget accepts proxy-style absolute-URI targets and, for
// leniency, origin-form targets with a Host header.
func parseForwardTarget(h *httpHead) (*httpForward, error) {
	if rest, ok := strings.CutPrefix(h.target, "http://"); ok {
		authority, path := rest, "/"
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			authority, path = rest[:i], rest[i:]
		}
		host, port, err := splitHTTPAuthority(authority, "80")
		if err != nil {
			return nil, err
		}
		return &httpForward{host: host, port: port, originForm: path, authority: authority}, nil
	}
	if strings.HasPrefix(h.target, "/") {
		hostHeader := findHTTPHeader(h.headers, "Host")
		if hostHeader == "" {
			return nil, fmt.Errorf("origin-form request without Host header")
		}
		host, port, err := splitHTTPAuthority(hostHeader, "80")
		if err != nil {
			return nil, err
		}
		return &httpForward{host: host, port: port, originForm: h.target, authority: hostHeader}, nil
	}
	return nil, fmt.Errorf("unsupported request target %q", h.target)
}

func findHTTPHeader(headers []string, name string) string {
	for _, line := range headers {
		if k, v, ok := strings.Cut(line, ":"); ok && strings.EqualFold(strings.TrimSpace(k), name) {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// rewriteForwardHead re-serializes a forwarded request head: origin-form
// request line, hop-by-hop proxy headers dropped, Connection: close so the
// origin closes after the response (we close the client side when it
// completes).
func rewriteForwardHead(h *httpHead, target *httpForward) string {
	var b strings.Builder
	b.WriteString(h.method)
	b.WriteByte(' ')
	b.WriteString(target.originForm)
	b.WriteByte(' ')
	b.WriteString(h.version)
	b.WriteString("\r\n")

	haveConnection, haveHost := false, false
	for _, line := range h.headers {
		name, _, _ := strings.Cut(line, ":")
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "proxy-connection", "proxy-authorization", "proxy-authentication", "keep-alive":
			continue
		case "connection":
			if !haveConnection {
				b.WriteString("Connection: close\r\n")
				haveConnection = true
			}
			continue
		case "host":
			haveHost = true
		}
		b.WriteString(line)
		b.WriteString("\r\n")
	}
	if !haveConnection {
		b.WriteString("Connection: close\r\n")
	}
	if !haveHost {
		b.WriteString("Host: ")
		b.WriteString(target.authority)
		b.WriteString("\r\n")
	}
	// The blank line terminating the header block.
	b.WriteString("\r\n")
	return b.String()
}

// writeHTTPResponse sends a minimal bodyless error response, then the
// caller closes the connection.
func writeHTTPResponse(w io.Writer, code int, reason string) {
	fmt.Fprintf(w, "HTTP/1.1 %d %s\r\nContent-Length: 0\r\nConnection: close\r\n\r\n", code, reason)
}
