# Code Review Findings: Memory Leaks & Race Conditions

This document contains the findings of an extensive code review performed on the **Yggstack** codebase (`cmd/yggstack`, `src/netstack`, `src/types`, and `mobile`).

---

## Executive Summary

The review identified several critical memory leaks, goroutine leaks, unclosed network socket resources, and potential race conditions. The primary issues stem from:
1. Unbounded UDP session tracking without cleanup or expiration mechanisms in `cmd/yggstack/main.go`.
2. Handoff of network connections into unmanaged infinite-loop proxy goroutines (`ReverseProxyUDP` / `ProxyTCP`).
3. Unsafe value copying of `net.UDPConn` / `gonet.UDPConn` structs causing data races and synchronization corruption.
4. Missing context cancellation or deadline management in listener accept loops on shutdown.

---

## Detailed Findings & Fix Suggestions

### 1. Unbounded UDP Session & Goroutine Leak in Port Forwarders

- **Location**: `cmd/yggstack/main.go` (lines 337–400 and 404–469)
- **Category**: Memory Leak / Goroutine Leak / Resource Leak
- **Severity**: Critical

#### Problem Description
In `cmd/yggstack/main.go`, `localUdpConnections` and `remoteUdpConnections` (both `sync.Map` instances) store UDP proxy sessions.
1. When a packet is received from a new remote address, a new UDP connection is dialed (`s.DialUDP` or `net.DialUDP`), stored in the map as a `*UDPSession`, and a `types.ReverseProxyUDP` goroutine is spawned.
2. The session map **never expires or removes idle sessions**. Entries remain in the map indefinitely.
3. If writing to a UDP forwarding connection fails, `localUdpConnections.Delete(remoteUdpAddrStr)` is called. However, the associated `udpFwdConn` socket is closed, but the corresponding `ReverseProxyUDP` goroutine waiting on `src.Read()` is **never unblocked or terminated**. It remains suspended in memory forever.

#### Impact
Every unique UDP client address or source port consumes memory, file descriptors, netstack endpoints, and a leaked goroutine. Over time or under high traffic, this causes host memory exhaustion and file descriptor limits to be exceeded.

#### Fix Suggestion
- Implement session idle timeouts and periodic sweeping of idle UDP sessions (similar to the pattern used in `mobile/stats.go`).
- Ensure that when a UDP session is deleted or fails, its underlying connections are explicitly closed to unblock the `ReverseProxyUDP` goroutine.
- Pass a `context.Context` or shutdown signal to proxy goroutines.

---

### 2. Race Condition & Data Corruption via Value Copying of `net.UDPConn` Structs

- **Location**: `cmd/yggstack/main.go` (lines 379–380 and 456–457)
- **Category**: Race Condition / Misuse of Net Standard Types
- **Severity**: High

#### Problem Description
In `main.go`, `UDPSession.conn` holds a pointer to a UDP connection stored as `interface{}`. During packet forwarding:

In `localudp`:
```go
udpFwdConnPtr := udpSession.conn.(*gonet.UDPConn)
udpFwdConn := *udpFwdConnPtr // Dereferences pointer and copies struct by value!
```

In `remoteudp`:
```go
udpFwdConnPtr := udpSession.conn.(*net.UDPConn)
udpFwdConn := *udpFwdConnPtr // Dereferences pointer and copies struct by value!
```

Both `net.UDPConn` and `gonet.UDPConn` contain internal mutexes, pointers, and state buffers. Dereferencing the pointer (`*udpFwdConnPtr`) creates a **copy by value** of the internal socket struct. Calling `.Write()` or `.Close()` on copies while another goroutine accesses the original pointer violates Go concurrency guarantees and leads to data races and internal state corruption.

#### Impact
Data race warnings under Go race detector (`-race`), potential panic or memory corruption inside standard `net` and `gvisor` transport packages during concurrent writes or socket teardown.

#### Fix Suggestion
Do not dereference the pointer. Use the pointer directly when invoking methods:
```go
udpFwdConn, ok := udpSession.conn.(*net.UDPConn)
if !ok {
    continue
}
_, err = udpFwdConn.Write(udpBuffer[:bytesRead])
```

---

### 3. Premature TCP Teardown & Goroutine Lifetime Issue in `ProxyTCP`

- **Location**: `src/types/tcpproxy.go` (lines 7–34)
- **Category**: Goroutine Leak / Resource Handling
- **Severity**: Medium

#### Problem Description
`ProxyTCP` starts two goroutines:
```go
go func() { errCh <- tcpProxyFunc(mtu, c1, c2) }()
go func() { errCh <- tcpProxyFunc(mtu, c2, c1) }()
```
1. `tcpProxyFunc` returns `err` immediately when `src.Read()` returns `io.EOF`.
2. `ProxyTCP` receives `io.EOF` on `errCh`, treats it as a non-nil error (`if e != nil`), and immediately closes both `c1` and `c2`.
3. Closing both connections abruptly prevents half-closed TCP streams (where one side shuts down writes but expects to read remaining response data) from completing naturally.
4. If `c1.Close()` or `c2.Close()` blocks or takes time, the second goroutine trying to send to `errCh` could block if `errCh` buffer is exhausted (though `make(chan error, 2)` prevents blocking on write, the second goroutine might block in `src.Read()` if the connection object isn't unblocked cleanly).

#### Impact
Abrupt termination of TCP connections during normal EOF closure; potential loss of in-flight buffered data for half-closed TCP connections.

#### Fix Suggestion
- Differentiate `io.EOF` from actual network read/write errors.
- Support half-close (e.g. `CloseWrite()`) if supported by the connection interfaces, or wait for both directions to return `io.EOF` before closing the underlying sockets.

---

### 4. Unblocked Goroutine Leaks in `ReverseProxyUDP`

- **Location**: `src/types/udpproxy.go` (lines 7–20)
- **Category**: Goroutine Leak
- **Severity**: Medium

#### Problem Description
`ReverseProxyUDP` runs a `for` loop reading from `src net.Conn` and writing to `dst net.PacketConn`:
```go
func ReverseProxyUDP(mtu uint64, dst net.PacketConn, dstAddr net.Addr, src net.Conn) error {
    buf := make([]byte, mtu)
    for {
        n, err := src.Read(buf[:])
        if err != nil {
            return err
        }
        // ...
    }
}
```
If `dst.WriteTo` fails, `ReverseProxyUDP` does not close `src`. More importantly, if `src` is never closed by an external caller, `src.Read(buf[:])` will block indefinitely.

#### Impact
Leaked goroutines running `ReverseProxyUDP` that remain blocked in `Read()` forever if connection tracking fails to close `src`.

#### Fix Suggestion
- Ensure that `src` and `dst` references are reliably closed when session management drops the connection.
- Use read deadlines or `context.Context` cancellation to break out of blocking `Read()` calls.

---

### 5. Unhandled Goroutines and Unclosed Listeners on Graceful Shutdown in CLI

- **Location**: `cmd/yggstack/main.go` (lines 318–492)
- **Category**: Resource Leak / Graceful Shutdown Failure
- **Severity**: Medium

#### Problem Description
When `main.go` receives a SIGINT or SIGTERM signal (`<-ctx.Done()`), it shuts down `admin`, `multicast`, `socks5` sockets, and `core`. However:
1. The listener goroutines created for `-local-tcp`, `-local-udp`, `-remote-tcp`, and `-remote-udp` run infinite loops (`for { listener.Accept() }` or `for { udpListenConn.ReadFrom() }`).
2. These listeners are never stored in a central structure in `main.go` and are **never closed** during shutdown.
3. `Accept()` and `ReadFrom()` remain blocked indefinitely on the listening sockets, keeping the goroutines alive until OS process termination.

#### Impact
Incomplete shutdown sequence; inability to reuse bound ports immediately if `yggstack` logic is embedded or integrated into other Go applications.

#### Fix Suggestion
Store created `net.Listener` and `net.PacketConn` objects in a slice or struct. On `ctx.Done()`, close all active listeners and wait for all mapping goroutines to exit using a `sync.WaitGroup`. (Note: `mobile/yggstack.go` already implements this cleanup pattern correctly; `cmd/yggstack/main.go` should follow suit).

---

### 6. Slice Mutation Concurrency Hazard in Flag Parsing Types

- **Location**: `src/types/mapping.go` (lines 201, 255, 309, 363)
- **Category**: Potential Race Condition / Defensive Coding
- **Severity**: Low

#### Problem Description
The `Set` methods for `TCPLocalMappings`, `TCPRemoteMappings`, `UDPLocalMappings`, and `UDPRemoteMappings` append directly to the receiver slice:
```go
*m = append(*m, mapping)
```
While `flag.Parse()` is currently invoked serially during application startup, exporting these types for external use without mutex protection means concurrent modifications will lead to race conditions and slice corruption.

#### Fix Suggestion
Document thread-safety expectations or add mutex protection if these types are intended to be modified dynamically at runtime outside of initial CLI parsing.

---

## Summary Table of Issues

| Issue ID | File Location | Description | Severity | Type |
| :--- | :--- | :--- | :--- | :--- |
| **YGG-01** | `cmd/yggstack/main.go` | Unbounded UDP session map growth & goroutine leak | **Critical** | Memory / Resource Leak |
| **YGG-02** | `cmd/yggstack/main.go` | Unsafe struct value copying of `net.UDPConn` / `gonet.UDPConn` | **High** | Race Condition |
| **YGG-03** | `src/types/tcpproxy.go` | Premature TCP socket closure & half-close handling in `ProxyTCP` | **Medium** | Resource / Logic Bug |
| **YGG-04** | `src/types/udpproxy.go` | Unblocked `Read()` goroutine leaks in `ReverseProxyUDP` | **Medium** | Goroutine Leak |
| **YGG-05** | `cmd/yggstack/main.go` | Unclosed listener sockets and hanging goroutines on shutdown | **Medium** | Resource Leak |
| **YGG-06** | `src/types/mapping.go` | Slice append operations without concurrency safety | **Low** | Race Hazard |
