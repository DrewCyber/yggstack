# Fix: Port Binding Issue During Rapid Start/Stop

## Problem

When the Yggstack service was stopped and restarted quickly (especially during rapid start/stop cycles), TCP/UDP listeners could remain bound to ports even after the service reported it had stopped. This caused "address already in use" errors on subsequent starts.

### Root Cause

The existing shutdown sequence had a critical flaw:

1. Context was cancelled, signaling handlers to stop
2. Active proxy **connections** were closed
3. Wait for handlers to complete (5-second timeout)
4. If handlers didn't finish, force shutdown was executed

**The problem:** TCP/UDP **listeners** were only closed via `defer listener.Close()` when handler functions returned. If a handler was stuck waiting for active connections to finish, the listener remained bound to the port even after the timeout forced shutdown.

### Example from Logs

```
[00:25:33] Stopping TCP mapping handler for port 8448  ✓
[00:25:33] Stopping TCP mapping handler for port 8443  ✓
[00:25:33] Stopping TCP mapping handler for port 8123  ✓
[00:25:33] (port 8843 handler MISSING - never stopped)
[00:25:38] Timeout waiting for handlers to stop, forcing shutdown
[00:25:38] Yggstack stopped

[00:25:43] Starting Yggstack...
[00:25:43] Failed to listen on local TCP 127.0.0.1:8843: bind: address already in use
```

Port 8843 had an active connection that prevented the handler from exiting cleanly, leaving the port bound.

## Solution

Added explicit listener tracking and force-close mechanism:

### Changes to `mobile/yggstack.go`

1. **Added listener tracking** (struct fields):
   ```go
   activeListeners   []io.Closer
   activeListenersMu sync.Mutex
   ```

2. **New functions**:
   - `trackListener(listener io.Closer)` - Register all TCP/UDP listeners
   - `closeAllListeners()` - Forcefully close all tracked listeners

3. **Updated Stop() sequence**:
   ```go
   // Close all active listeners first to stop accepting new connections
   y.closeAllListeners()
   
   // Close all active proxy connections to unblock handlers
   y.closeAllConnections()
   
   // Wait for handlers to finish (now they can exit quickly)
   y.handlersWg.Wait()
   ```

4. **Track all listener types**:
   - Local TCP listeners (`handleLocalTCPMapping`)
   - Local UDP listeners (`handleLocalUDPMapping`)
   - Remote TCP listeners (`handleRemoteTCPMapping`)
   - Remote UDP listeners (`handleRemoteUDPMapping`)

## Benefits

- **Immediate port release**: Listeners are closed before waiting for handlers
- **Fast cleanup**: `Accept()` and `ReadFrom()` calls fail immediately when listeners close
- **No timeout issues**: Handlers can exit cleanly within the 5-second window
- **Prevents port conflicts**: All ports properly released before next start attempt

## Testing

Tested with rapid start/stop cycles (spamming start/stop button). Before the fix, ports would become stuck with "address already in use" errors. After the fix, all ports are properly released and can be rebound immediately.

## Files Modified

- `mobile/yggstack.go`
  - Added `io` import
  - Added `activeListeners` tracking fields
  - Added `trackListener()` and `closeAllListeners()` functions
  - Updated `Stop()` to close listeners before waiting
  - Added `trackListener()` calls in all 4 handler functions
