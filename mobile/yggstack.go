// Package mobile provides Android/iOS bindings for Yggstack
package mobile

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gologme/log"
	"github.com/hjson/hjson-go/v4"

	"github.com/yggdrasil-network/yggdrasil-go/src/address"
	"github.com/yggdrasil-network/yggdrasil-go/src/admin"
	"github.com/yggdrasil-network/yggdrasil-go/src/config"
	"github.com/yggdrasil-network/yggdrasil-go/src/core"
	"github.com/yggdrasil-network/yggdrasil-go/src/multicast"

	"github.com/yggdrasil-network/yggstack/src/netstack"
	"github.com/yggdrasil-network/yggstack/src/types"
)

// Yggstack is the main mobile binding object
type Yggstack struct {
	core      *core.Core
	multicast *multicast.Multicast
	admin     *admin.AdminSocket
	netstack  *netstack.YggdrasilNetstack
	socks5Tcp net.Listener
	logger    *log.Logger
	logWriter io.Writer
	logLevel  string
	config    *config.NodeConfig
	run       *workerScope
	mappings  map[string]*workerScope

	// Port mappings
	localTCPMappings  []types.TCPMapping
	localUDPMappings  []types.UDPMapping
	remoteTCPMappings []types.TCPMapping
	remoteUDPMappings []types.UDPMapping

	// Per-listener runtime stats (connection gauges, Yggdrasil-side RX/TX bytes),
	// keyed by mapping identity plus socksStatsKey for the SOCKS5 proxy
	listenerStats sync.Map // string key → *listenerStats

	// State
	isRunning bool
	mu        sync.RWMutex
}

// LogWriter implements io.Writer for Android logging
type LogWriter struct {
	callback LogCallback
	mu       sync.Mutex
}

// LogCallback is called when logs are generated
type LogCallback interface {
	OnLog(message string)
}

// Write is shared by every per-subsystem *log.Logger built in buildLogger, so
// without this lock the JNI callback could be entered concurrently from
// multiple goroutines/OS threads (each logger only serializes against
// itself, not against the others writing to the same callback).
func (w *LogWriter) Write(p []byte) (n int, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.callback != nil {
		w.callback.OnLog(string(p))
	}
	return len(p), nil
}

// NewYggstack creates a new Yggstack instance
func NewYggstack() *Yggstack {
	y := &Yggstack{
		isRunning: false,
		logWriter: os.Stdout,
	}
	y.logger = y.buildLogger()
	return y
}

// Mapping key helpers (used for per-mapping cancel / listener tracking)
func localTCPMappingKey(listenAddr, mappedAddr string) string {
	return "ltcp:" + listenAddr + "->" + mappedAddr
}
func localUDPMappingKey(listenAddr, mappedAddr string) string {
	return "ludp:" + listenAddr + "->" + mappedAddr
}
func remoteTCPMappingKey(remotePort int, localAddr string) string {
	return fmt.Sprintf("rtcp:%d->%s", remotePort, localAddr)
}
func remoteUDPMappingKey(remotePort int, localAddr string) string {
	return fmt.Sprintf("rudp:%d->%s", remotePort, localAddr)
}

// SetLogCallback sets custom log callback for Android
func (y *Yggstack) SetLogCallback(callback LogCallback) {
	if callback != nil {
		y.logWriter = &LogWriter{callback: callback}
		y.logger = y.buildLogger()
	}
}

// buildLogger returns a fresh golog Logger over the shared writer. Each
// subsystem gets its own instance because gologme/log mutates per-instance
// state (calldepth) without locking, so one instance must not be shared
// across goroutines.
func (y *Yggstack) buildLogger() *log.Logger {
	w := y.logWriter
	if w == nil {
		w = os.Stdout
	}
	l := log.New(w, "", log.Flags())
	y.applyLogLevel(l)
	// Prewarm calldepth so its unsynchronized first-write cannot race later.
	l.Infof("%s", "logger ready")
	return l
}

func (y *Yggstack) applyLogLevel(l *log.Logger) {
	switch strings.ToLower(y.logLevel) {
	case "error":
		l.EnableLevel("error")
	case "warn":
		l.EnableLevel("error")
		l.EnableLevel("warn")
	case "info":
		l.EnableLevel("error")
		l.EnableLevel("warn")
		l.EnableLevel("info")
	case "debug":
		l.EnableLevel("error")
		l.EnableLevel("warn")
		l.EnableLevel("info")
		l.EnableLevel("debug")
		l.EnableLevel("trace")
	}
}

// SetLogLevel sets the logging level (info, warn, error, debug)
func (y *Yggstack) SetLogLevel(level string) {
	y.logLevel = strings.ToLower(level)
	y.applyLogLevel(y.logger)
}

// GenerateConfig generates a new random configuration and returns it as JSON string
func GenerateConfig() (string, error) {
	cfg := config.GenerateConfig()
	cfg.AdminListen = "none"

	bs, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", fmt.Errorf("failed to marshal config: %w", err)
	}
	return string(bs), nil
}

// LoadConfigJSON loads configuration from JSON string
func (y *Yggstack) LoadConfigJSON(configJSON string) error {
	y.mu.Lock()
	defer y.mu.Unlock()

	if y.isRunning {
		return fmt.Errorf("cannot load config while Yggstack is running")
	}

	cfg := &config.NodeConfig{}
	if err := json.Unmarshal([]byte(configJSON), cfg); err != nil {
		// Try HJSON format
		if err := hjson.Unmarshal([]byte(configJSON), cfg); err != nil {
			return fmt.Errorf("failed to parse config: %w", err)
		}
	}

	cfg.AdminListen = "none"
	y.config = cfg
	return nil
}

// GetAddress returns the IPv6 address for this node
func (y *Yggstack) GetAddress() (string, error) {
	y.mu.RLock()
	defer y.mu.RUnlock()

	if y.config == nil {
		return "", fmt.Errorf("config not loaded")
	}

	privateKey := ed25519.PrivateKey(y.config.PrivateKey)
	publicKey := privateKey.Public().(ed25519.PublicKey)
	addr := address.AddrForKey(publicKey)
	ip := net.IP(addr[:])
	return ip.String(), nil
}

// GetSubnet returns the IPv6 subnet for this node
func (y *Yggstack) GetSubnet() (string, error) {
	y.mu.RLock()
	defer y.mu.RUnlock()

	if y.config == nil {
		return "", fmt.Errorf("config not loaded")
	}

	privateKey := ed25519.PrivateKey(y.config.PrivateKey)
	publicKey := privateKey.Public().(ed25519.PublicKey)
	snet := address.SubnetForKey(publicKey)
	ipnet := net.IPNet{
		IP:   append(snet[:], 0, 0, 0, 0, 0, 0, 0, 0),
		Mask: net.CIDRMask(len(snet)*8, 128),
	}
	return ipnet.String(), nil
}

// GetPublicKey returns the public key for this node
func (y *Yggstack) GetPublicKey() (string, error) {
	y.mu.RLock()
	defer y.mu.RUnlock()

	if y.config == nil {
		return "", fmt.Errorf("config not loaded")
	}

	privateKey := ed25519.PrivateKey(y.config.PrivateKey)
	publicKey := privateKey.Public().(ed25519.PublicKey)
	return hex.EncodeToString(publicKey), nil
}

// AddPeer adds a peer to the configuration
func (y *Yggstack) AddPeer(peerURI string) error {
	y.mu.Lock()
	defer y.mu.Unlock()

	if y.config == nil {
		return fmt.Errorf("config not loaded")
	}

	// Check if peer already exists
	for _, peer := range y.config.Peers {
		if peer == peerURI {
			return nil // Already exists
		}
	}

	y.config.Peers = append(y.config.Peers, peerURI)
	return nil
}

// RemovePeer removes a peer from the configuration
func (y *Yggstack) RemovePeer(peerURI string) error {
	y.mu.Lock()
	defer y.mu.Unlock()

	if y.config == nil {
		return fmt.Errorf("config not loaded")
	}

	newPeers := []string{}
	for _, peer := range y.config.Peers {
		if peer != peerURI {
			newPeers = append(newPeers, peer)
		}
	}
	y.config.Peers = newPeers
	return nil
}

// GetPeers returns the list of configured peers as JSON string
func (y *Yggstack) GetPeers() (string, error) {
	y.mu.RLock()
	defer y.mu.RUnlock()

	if y.config == nil {
		return "", fmt.Errorf("config not loaded")
	}

	bs, err := json.Marshal(y.config.Peers)
	if err != nil {
		return "", fmt.Errorf("failed to marshal peers: %w", err)
	}
	return string(bs), nil
}

// GetPeersJSON returns detailed information about connected peers as JSON
func (y *Yggstack) GetPeersJSON() (string, error) {
	y.mu.RLock()
	defer y.mu.RUnlock()

	if y.core == nil {
		return "[]", fmt.Errorf("core not initialized")
	}

	peersInfo := y.core.GetPeers()
	bs, err := json.Marshal(peersInfo)
	if err != nil {
		return "[]", fmt.Errorf("failed to marshal peers info: %w", err)
	}
	return string(bs), nil
}

// AddLivePeer adds a peer to the running yggdrasil core without restarting.
// peerURI should be a full URI, e.g. "tcp://host:port?maxbackoff=5s".
// Returns an error if the node is not currently running.
func (y *Yggstack) AddLivePeer(peerURI string) error {
	y.mu.RLock()
	defer y.mu.RUnlock()

	if !y.isRunning || y.core == nil {
		return fmt.Errorf("Yggstack is not running")
	}

	u, err := url.Parse(peerURI)
	if err != nil {
		return fmt.Errorf("invalid peer URI %q: %w", peerURI, err)
	}
	return y.core.AddPeer(u, "")
}

// RemoveLivePeer removes a peer from the running yggdrasil core without restarting.
// peerURI should be a full URI matching what was passed when the peer was added,
// e.g. "tcp://host:port?maxbackoff=5s".
// Returns an error if the node is not currently running.
func (y *Yggstack) RemoveLivePeer(peerURI string) error {
	y.mu.RLock()
	defer y.mu.RUnlock()

	if !y.isRunning || y.core == nil {
		return fmt.Errorf("Yggstack is not running")
	}

	u, err := url.Parse(peerURI)
	if err != nil {
		return fmt.Errorf("invalid peer URI %q: %w", peerURI, err)
	}
	return y.core.RemovePeer(u, "")
}

// RetryPeersNow forces immediate reconnection attempt for all peers
// This should be called when network connectivity changes (e.g., WiFi <-> Cellular)
func (y *Yggstack) RetryPeersNow() error {
	y.mu.RLock()
	defer y.mu.RUnlock()

	if !y.isRunning {
		return fmt.Errorf("Yggstack is not running")
	}

	if y.core == nil {
		return fmt.Errorf("core not initialized")
	}

	y.logger.Infof("Forcing immediate peer retry...")
	y.core.RetryPeersNow()
	y.logger.Infof("RetryPeersNow() completed - peers should reconnect immediately")
	return nil
}

// Start starts the Yggstack node with optional SOCKS listener and nameserver
func (y *Yggstack) Start(socksAddress string, nameserver string) (startErr error) {
	y.mu.Lock()
	defer y.mu.Unlock()

	if y.isRunning {
		return fmt.Errorf("Yggstack is already running")
	}

	if y.config == nil {
		return fmt.Errorf("config not loaded, call LoadConfigJSON first")
	}

	y.run = newWorkerScope(context.Background())
	y.mappings = make(map[string]*workerScope)
	defer func() {
		if startErr != nil {
			y.stopLocked()
		}
	}()

	// Stats always describe the current run only.
	y.resetListenerStats()

	// Generate self-signed certificate if not already present
	if y.config.Certificate == nil {
		if err := y.config.GenerateSelfSignedCertificate(); err != nil {
			return fmt.Errorf("failed to generate certificate: %w", err)
		}
	}

	// Setup the Yggdrasil core
	var err error
	privateKey := ed25519.PrivateKey(y.config.PrivateKey)
	options := []core.SetupOption{
		core.NodeInfo(y.config.NodeInfo),
		core.NodeInfoPrivacy(y.config.NodeInfoPrivacy),
	}

	for _, addr := range y.config.Listen {
		options = append(options, core.ListenAddress(addr))
	}

	for _, peer := range y.config.Peers {
		options = append(options, core.Peer{URI: peer})
	}

	for intf, peers := range y.config.InterfacePeers {
		for _, peer := range peers {
			options = append(options, core.Peer{URI: peer, SourceInterface: intf})
		}
	}

	for _, allowed := range y.config.AllowedPublicKeys {
		k, err := hex.DecodeString(allowed)
		if err != nil {
			return fmt.Errorf("invalid allowed public key: %w", err)
		}
		options = append(options, core.AllowedPublicKey(k[:]))
	}

	if y.config.GroupPassword != "" {
		options = append(options, core.GroupPassword(y.config.GroupPassword))
	}

	if y.core, err = core.New(y.config.Certificate, y.buildLogger(), options...); err != nil {
		return fmt.Errorf("failed to create core: %w", err)
	}

	address, subnet := y.core.Address(), y.core.Subnet()
	publicKey := privateKey.Public().(ed25519.PublicKey)
	publicstr := hex.EncodeToString(publicKey)
	y.logger.Infof("Your public key is %s", publicstr)
	y.logger.Infof("Your IPv6 address is %s", address.String())
	y.logger.Infof("Your IPv6 subnet is %s", subnet.String())
	y.logger.Infof("Your Yggstack resolver name is %s%s", publicstr, types.NameMappingSuffix)

	// Setup the admin socket (disabled for mobile)
	adminOptions := []admin.SetupOption{
		admin.ListenAddress("none"),
	}
	if y.admin, err = admin.New(y.core, y.buildLogger(), adminOptions...); err != nil {
		return fmt.Errorf("failed to create admin: %w", err)
	}

	// Setup the multicast module
	multicastOptions := []multicast.SetupOption{}
	for _, intf := range y.config.MulticastInterfaces {
		regex, err := regexp.Compile(intf.Regex)
		if err != nil {
			return fmt.Errorf("invalid multicast regex: %w", err)
		}
		multicastOptions = append(multicastOptions, multicast.MulticastInterface{
			Regex:    regex,
			Beacon:   intf.Beacon,
			Listen:   intf.Listen,
			Port:     intf.Port,
			Priority: uint8(intf.Priority),
			Password: intf.Password,
		})
	}

	if y.multicast, err = multicast.New(y.core, y.buildLogger(), multicastOptions...); err != nil {
		return fmt.Errorf("failed to create multicast: %w", err)
	}

	// Setup Yggdrasil netstack
	if y.netstack, err = netstack.CreateYggdrasilNetstack(y.core); err != nil {
		return fmt.Errorf("failed to create netstack: %w", err)
	}

	// Bind before starting any workers; failure uses the same rollback as Stop.
	if socksAddress != "" {
		if y.socks5Tcp, err = net.Listen("tcp", socksAddress); err != nil {
			return fmt.Errorf("failed to start SOCKS listener: %w", err)
		}
		y.startSOCKS(y.socks5Tcp, nameserver)
	}

	// Setup local TCP mappings
	for _, mapping := range y.localTCPMappings {
		key := localTCPMappingKey(mapping.Listen.String(), mapping.Mapped.String())
		y.startMapping(key, func(scope *workerScope) { y.handleLocalTCPMappingCtx(scope, key, mapping) })
	}

	// Setup local UDP mappings
	for _, mapping := range y.localUDPMappings {
		key := localUDPMappingKey(mapping.Listen.String(), mapping.Mapped.String())
		y.startMapping(key, func(scope *workerScope) { y.handleLocalUDPMappingCtx(scope, key, mapping) })
	}

	// Setup remote TCP mappings
	for _, mapping := range y.remoteTCPMappings {
		key := remoteTCPMappingKey(mapping.Listen.Port, mapping.Mapped.String())
		y.startMapping(key, func(scope *workerScope) { y.handleRemoteTCPMappingCtx(scope, key, mapping) })
	}

	// Setup remote UDP mappings
	for _, mapping := range y.remoteUDPMappings {
		key := remoteUDPMappingKey(mapping.Listen.Port, mapping.Mapped.String())
		y.startMapping(key, func(scope *workerScope) { y.handleRemoteUDPMappingCtx(scope, key, mapping) })
	}

	y.isRunning = true
	y.logger.Infof("Yggstack started successfully")
	return nil
}

// Stop stops the entire node, including during Android Power Save. Start and all
// mutations remain serialized until teardown finishes; a new run never inherits
// workers or resources from the previous one.
func (y *Yggstack) Stop() error {
	y.mu.Lock()
	defer y.mu.Unlock()
	if !y.isRunning {
		return fmt.Errorf("Yggstack is not running")
	}
	y.stopLocked()
	return nil
}

// stopLocked also rolls back partial startup, independently of isRunning.
// Workers must not acquire y.mu: teardown holds it until every worker is joined.
func (y *Yggstack) stopLocked() {
	y.isRunning = false
	if y.run != nil {
		y.run.Close()
	}
	// Cancel every mapping before joining any one of them.
	for _, scope := range y.mappings {
		scope.Close()
	}
	for key := range y.mappings {
		y.stopMapping(key)
	}
	if y.run != nil {
		y.run.stop()
	}
	if y.admin != nil {
		y.admin.Stop()
	}
	if y.multicast != nil {
		y.multicast.Stop()
	}
	if y.netstack != nil {
		y.netstack.Close()
	}
	if y.core != nil {
		y.core.Stop()
	}
	y.admin, y.multicast, y.core, y.netstack = nil, nil, nil, nil
	y.socks5Tcp = nil
	y.run, y.mappings = nil, nil
	y.resetListenerStats()
	y.logger.Infof("Yggstack stopped")
}

// IsRunning returns whether Yggstack is currently running
func (y *Yggstack) IsRunning() bool {
	y.mu.RLock()
	defer y.mu.RUnlock()
	return y.isRunning
}

// AddLocalTCPMapping adds a TCP mapping from local address to remote Yggdrasil address
// Format: localAddr="127.0.0.1:8080", remoteAddr="[200:1234::1]:8080"
func (y *Yggstack) AddLocalTCPMapping(localAddr, remoteAddr string) error {
	y.mu.Lock()
	defer y.mu.Unlock()

	localTCPAddr, err := net.ResolveTCPAddr("tcp", localAddr)
	if err != nil {
		return fmt.Errorf("invalid local TCP address %s: %w", localAddr, err)
	}

	remoteTCPAddr, err := net.ResolveTCPAddr("tcp", remoteAddr)
	if err != nil {
		return fmt.Errorf("invalid remote TCP address %s: %w", remoteAddr, err)
	}

	mapping := types.TCPMapping{
		Listen: localTCPAddr,
		Mapped: remoteTCPAddr,
	}

	y.localTCPMappings = append(y.localTCPMappings, mapping)

	// If already running, start the mapping handler
	if y.isRunning {
		key := localTCPMappingKey(localTCPAddr.String(), remoteTCPAddr.String())
		y.startMapping(key, func(scope *workerScope) { y.handleLocalTCPMappingCtx(scope, key, mapping) })
	}

	y.logger.Infof("Added local TCP mapping: %s -> %s", localAddr, remoteAddr)
	return nil
}

// AddLocalUDPMapping adds a UDP mapping from local address to remote Yggdrasil address
// Format: localAddr="127.0.0.1:5353", remoteAddr="[200:1234::1]:53"
func (y *Yggstack) AddLocalUDPMapping(localAddr, remoteAddr string) error {
	y.mu.Lock()
	defer y.mu.Unlock()

	localUDPAddr, err := net.ResolveUDPAddr("udp", localAddr)
	if err != nil {
		return fmt.Errorf("invalid local UDP address %s: %w", localAddr, err)
	}

	remoteUDPAddr, err := net.ResolveUDPAddr("udp", remoteAddr)
	if err != nil {
		return fmt.Errorf("invalid remote UDP address %s: %w", remoteAddr, err)
	}

	mapping := types.UDPMapping{
		Listen: localUDPAddr,
		Mapped: remoteUDPAddr,
	}

	y.localUDPMappings = append(y.localUDPMappings, mapping)

	// If already running, start the mapping handler
	if y.isRunning {
		key := localUDPMappingKey(localUDPAddr.String(), remoteUDPAddr.String())
		y.startMapping(key, func(scope *workerScope) { y.handleLocalUDPMappingCtx(scope, key, mapping) })
	}

	y.logger.Infof("Added local UDP mapping: %s -> %s", localAddr, remoteAddr)
	return nil
}

// AddRemoteTCPMapping adds a TCP mapping to expose local port on Yggdrasil network
// Format: remotePort=8080, localAddr="127.0.0.1:80"
func (y *Yggstack) AddRemoteTCPMapping(remotePort int, localAddr string) error {
	y.mu.Lock()
	defer y.mu.Unlock()

	if y.config == nil {
		return fmt.Errorf("config not loaded")
	}

	localTCPAddr, err := net.ResolveTCPAddr("tcp", localAddr)
	if err != nil {
		return fmt.Errorf("invalid local TCP address %s: %w", localAddr, err)
	}

	// Get our Yggdrasil address
	privateKey := ed25519.PrivateKey(y.config.PrivateKey)
	publicKey := privateKey.Public().(ed25519.PublicKey)
	addr := address.AddrForKey(publicKey)
	ip := net.IP(addr[:])

	remoteTCPAddr := &net.TCPAddr{
		IP:   ip,
		Port: remotePort,
	}

	mapping := types.TCPMapping{
		Listen: remoteTCPAddr,
		Mapped: localTCPAddr,
	}

	y.remoteTCPMappings = append(y.remoteTCPMappings, mapping)

	// If already running, start the mapping handler
	if y.isRunning {
		key := remoteTCPMappingKey(mapping.Listen.Port, mapping.Mapped.String())
		y.startMapping(key, func(scope *workerScope) { y.handleRemoteTCPMappingCtx(scope, key, mapping) })
	}

	y.logger.Infof("Added remote TCP mapping: [%s]:%d -> %s", ip, remotePort, localAddr)
	return nil
}

// AddRemoteUDPMapping adds a UDP mapping to expose local port on Yggdrasil network
// Format: remotePort=53, localAddr="127.0.0.1:53"
func (y *Yggstack) AddRemoteUDPMapping(remotePort int, localAddr string) error {
	y.mu.Lock()
	defer y.mu.Unlock()

	if y.config == nil {
		return fmt.Errorf("config not loaded")
	}

	localUDPAddr, err := net.ResolveUDPAddr("udp", localAddr)
	if err != nil {
		return fmt.Errorf("invalid local UDP address %s: %w", localAddr, err)
	}

	// Get our Yggdrasil address
	privateKey := ed25519.PrivateKey(y.config.PrivateKey)
	publicKey := privateKey.Public().(ed25519.PublicKey)
	addr := address.AddrForKey(publicKey)
	ip := net.IP(addr[:])

	remoteUDPAddr := &net.UDPAddr{
		IP:   ip,
		Port: remotePort,
	}

	mapping := types.UDPMapping{
		Listen: remoteUDPAddr,
		Mapped: localUDPAddr,
	}

	y.remoteUDPMappings = append(y.remoteUDPMappings, mapping)

	// If already running, start the mapping handler
	if y.isRunning {
		key := remoteUDPMappingKey(mapping.Listen.Port, mapping.Mapped.String())
		y.startMapping(key, func(scope *workerScope) { y.handleRemoteUDPMappingCtx(scope, key, mapping) })
	}

	y.logger.Infof("Added remote UDP mapping: [%s]:%d -> %s", ip, remotePort, localAddr)
	return nil
}

// ClearLocalMappings clears all local (forward) port mappings
func (y *Yggstack) ClearLocalMappings() error {
	y.mu.Lock()
	defer y.mu.Unlock()
	for key := range y.mappings {
		if strings.HasPrefix(key, "ltcp:") || strings.HasPrefix(key, "ludp:") {
			y.stopMapping(key)
		}
	}

	y.localTCPMappings = nil
	y.localUDPMappings = nil

	y.logger.Infof("Cleared all local port mappings")
	return nil
}

// ClearRemoteMappings clears all remote (expose) port mappings
func (y *Yggstack) ClearRemoteMappings() error {
	y.mu.Lock()
	defer y.mu.Unlock()
	for key := range y.mappings {
		if strings.HasPrefix(key, "rtcp:") || strings.HasPrefix(key, "rudp:") {
			y.stopMapping(key)
		}
	}

	y.remoteTCPMappings = nil
	y.remoteUDPMappings = nil

	y.logger.Infof("Cleared all remote port mappings")
	return nil
}

// RemoveLocalTCPMapping stops and removes a specific local TCP forward mapping.
// Cancels the handler goroutine and closes its listener immediately.
func (y *Yggstack) RemoveLocalTCPMapping(localAddr, remoteAddr string) error {
	y.mu.Lock()
	defer y.mu.Unlock()

	localTCPAddr, err := net.ResolveTCPAddr("tcp", localAddr)
	if err != nil {
		return fmt.Errorf("invalid local TCP address %s: %w", localAddr, err)
	}
	remoteTCPAddr, err := net.ResolveTCPAddr("tcp", remoteAddr)
	if err != nil {
		return fmt.Errorf("invalid remote TCP address %s: %w", remoteAddr, err)
	}

	key := localTCPMappingKey(localTCPAddr.String(), remoteTCPAddr.String())
	y.stopMapping(key)

	newMappings := make([]types.TCPMapping, 0, len(y.localTCPMappings))
	for _, m := range y.localTCPMappings {
		if m.Listen.String() != localTCPAddr.String() || m.Mapped.String() != remoteTCPAddr.String() {
			newMappings = append(newMappings, m)
		}
	}
	y.localTCPMappings = newMappings

	y.logger.Infof("Removed local TCP mapping: %s -> %s", localAddr, remoteAddr)
	return nil
}

// RemoveLocalUDPMapping stops and removes a specific local UDP forward mapping.
func (y *Yggstack) RemoveLocalUDPMapping(localAddr, remoteAddr string) error {
	y.mu.Lock()
	defer y.mu.Unlock()

	localUDPAddr, err := net.ResolveUDPAddr("udp", localAddr)
	if err != nil {
		return fmt.Errorf("invalid local UDP address %s: %w", localAddr, err)
	}
	remoteUDPAddr, err := net.ResolveUDPAddr("udp", remoteAddr)
	if err != nil {
		return fmt.Errorf("invalid remote UDP address %s: %w", remoteAddr, err)
	}

	key := localUDPMappingKey(localUDPAddr.String(), remoteUDPAddr.String())
	y.stopMapping(key)

	newMappings := make([]types.UDPMapping, 0, len(y.localUDPMappings))
	for _, m := range y.localUDPMappings {
		if m.Listen.String() != localUDPAddr.String() || m.Mapped.String() != remoteUDPAddr.String() {
			newMappings = append(newMappings, m)
		}
	}
	y.localUDPMappings = newMappings

	y.logger.Infof("Removed local UDP mapping: %s -> %s", localAddr, remoteAddr)
	return nil
}

// RemoveRemoteTCPMapping stops and removes a specific remote TCP expose mapping.
func (y *Yggstack) RemoveRemoteTCPMapping(remotePort int, localAddr string) error {
	y.mu.Lock()
	defer y.mu.Unlock()

	localTCPAddr, err := net.ResolveTCPAddr("tcp", localAddr)
	if err != nil {
		return fmt.Errorf("invalid local TCP address %s: %w", localAddr, err)
	}

	key := remoteTCPMappingKey(remotePort, localTCPAddr.String())
	y.stopMapping(key)

	newMappings := make([]types.TCPMapping, 0, len(y.remoteTCPMappings))
	for _, m := range y.remoteTCPMappings {
		if m.Listen.Port != remotePort || m.Mapped.String() != localTCPAddr.String() {
			newMappings = append(newMappings, m)
		}
	}
	y.remoteTCPMappings = newMappings

	y.logger.Infof("Removed remote TCP mapping: port %d -> %s", remotePort, localAddr)
	return nil
}

// RemoveRemoteUDPMapping stops and removes a specific remote UDP expose mapping.
func (y *Yggstack) RemoveRemoteUDPMapping(remotePort int, localAddr string) error {
	y.mu.Lock()
	defer y.mu.Unlock()

	localUDPAddr, err := net.ResolveUDPAddr("udp", localAddr)
	if err != nil {
		return fmt.Errorf("invalid local UDP address %s: %w", localAddr, err)
	}

	key := remoteUDPMappingKey(remotePort, localUDPAddr.String())
	y.stopMapping(key)

	newMappings := make([]types.UDPMapping, 0, len(y.remoteUDPMappings))
	for _, m := range y.remoteUDPMappings {
		if m.Listen.Port != remotePort || m.Mapped.String() != localUDPAddr.String() {
			newMappings = append(newMappings, m)
		}
	}
	y.remoteUDPMappings = newMappings

	y.logger.Infof("Removed remote UDP mapping: port %d -> %s", remotePort, localAddr)
	return nil
}

// Helper functions for port mapping handlers
func (y *Yggstack) handleLocalTCPMappingCtx(scope *workerScope, key string, mapping types.TCPMapping) {
	ctx := scope.ctx

	select {
	case <-ctx.Done():
		return
	default:
	}

	listener, err := net.ListenTCP("tcp", mapping.Listen)
	if err != nil {
		y.logger.Errorf("Failed to listen on local TCP %s: %s", mapping.Listen, err)
		return
	}
	defer listener.Close()

	if !scope.own(listener) {
		return
	}

	stats := y.getOrCreateListenerStats(key, "local-tcp", mapping.Listen.String(), mapping.Mapped.String())

	y.logger.Infof("Mapping local TCP port %d to Yggdrasil %s", mapping.Listen.Port, mapping.Mapped)

	for {
		listener.SetDeadline(time.Now().Add(100 * time.Millisecond))

		select {
		case <-ctx.Done():
			y.logger.Infof("Stopping TCP mapping handler for port %d", mapping.Listen.Port)
			return
		default:
			c, err := listener.Accept()
			if err != nil {
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					continue
				}
				select {
				case <-ctx.Done():
					return
				default:
					return
				}
			}

			c = scope.conn(c)

			// Count the connection from the moment it is accepted, not after
			// the dial completes: the client is already parked on this socket,
			// and an in-flight dial must register as activity (Power Save's
			// idle detection watches ActiveConns) so the node is not powered
			// down mid-handshake.
			stats.activeConns.Add(1)

			// Dial with a timeout bound to the handler context: without it an
			// unreachable target left the dial blocking on the stack's ~2min
			// internal timeout, swallowing the accepted connection in silence
			// and outliving the handler's shutdown.
			dialCtx, cancelDial := context.WithTimeout(ctx, 10*time.Second)
			r, err := y.netstack.DialContext(dialCtx, "tcp", mapping.Mapped.String())
			cancelDial()
			if err != nil {
				y.logger.Errorf("Failed to connect to %s: %s", mapping.Mapped, err)
				stats.connClosed()
				c.Close()
				continue
			}

			// Hand the pre-dial gauge entry over to the wrapper, which owns
			// the count for the rest of the connection's lifetime
			stats.connClosed()
			rc := wrapCountingConn(r, stats)
			rc = scope.conn(rc)
			scope.goWorker(func() { types.ProxyTCP(y.core.MTU(), c, rc) })
		}
	}
}

func (y *Yggstack) handleLocalUDPMappingCtx(scope *workerScope, key string, mapping types.UDPMapping) {
	ctx := scope.ctx

	select {
	case <-ctx.Done():
		return
	default:
	}

	mtu := y.core.MTU()
	udpListenConn, err := net.ListenUDP("udp", mapping.Listen)
	if err != nil {
		y.logger.Errorf("Failed to listen on local UDP %s: %s", mapping.Listen, err)
		return
	}
	defer udpListenConn.Close()

	if !scope.own(udpListenConn) {
		return
	}

	stats := y.getOrCreateListenerStats(key, "local-udp", mapping.Listen.String(), mapping.Mapped.String())
	defer stats.sessionsEnded()

	y.logger.Infof("Mapping local UDP port %d to Yggdrasil %s", mapping.Listen.Port, mapping.Mapped)

	localUdpConnections := new(sync.Map)
	udpBuffer := make([]byte, mtu)
	nextSweep := time.Now().Add(udpSweepInterval)

	for {
		udpListenConn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))

		select {
		case <-ctx.Done():
			y.logger.Infof("Stopping UDP mapping handler for port %d", mapping.Listen.Port)
			return
		default:
			bytesRead, remoteUdpAddr, err := udpListenConn.ReadFrom(udpBuffer)
			if err != nil {
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					if now := time.Now(); now.After(nextSweep) {
						sweepIdleUDPSessions(localUdpConnections, now)
						nextSweep = now.Add(udpSweepInterval)
					}
					continue
				}
				select {
				case <-ctx.Done():
					return
				default:
					return
				}
			}

			connKey := remoteUdpAddr.String()
			var sess *udpSession

			if v, ok := localUdpConnections.Load(connKey); ok {
				sess = v.(*udpSession)
				sess.touch()
			} else {
				raw, dialErr := y.netstack.DialUDP(mapping.Mapped)
				if dialErr != nil {
					y.logger.Errorf("Failed to dial UDP %s: %s", mapping.Mapped, dialErr)
					continue
				}
				// Each remote client endpoint is one counted connection; the
				// wrapper feeds both the reverse pump and the inline writes
				wrapped := scope.conn(wrapCountingConn(raw, stats))
				sess = newUDPSession(wrapped)
				localUdpConnections.Store(connKey, sess)

				scope.goWorker(func() {
					defer wrapped.Close()
					defer localUdpConnections.CompareAndDelete(connKey, sess)
					types.ReverseProxyUDP(mtu, udpListenConn, remoteUdpAddr, wrapped)
				})
			}

			if _, err := sess.conn.Write(udpBuffer[:bytesRead]); err != nil {
				y.logger.Errorf("Failed to write to UDP connection: %s", err)
			}
		}
	}
}

func (y *Yggstack) handleRemoteTCPMappingCtx(scope *workerScope, key string, mapping types.TCPMapping) {
	ctx := scope.ctx

	select {
	case <-ctx.Done():
		return
	default:
	}

	listener, err := y.netstack.ListenTCP(mapping.Listen)
	if err != nil {
		y.logger.Errorf("Failed to listen on remote TCP %s: %s", mapping.Listen, err)
		return
	}
	defer listener.Close()

	if !scope.own(listener) {
		return
	}

	stats := y.getOrCreateListenerStats(key, "remote-tcp", mapping.Listen.String(), mapping.Mapped.String())

	y.logger.Infof("Exposing local TCP %s on Yggdrasil port %d", mapping.Mapped, mapping.Listen.Port)

	for {
		select {
		case <-ctx.Done():
			return
		default:
			c, err := listener.Accept()
			if err != nil {
				select {
				case <-ctx.Done():
					return
				default:
					return
				}
			}

			c = scope.conn(c)

			// Same treatment as the forward handler: count from accept, and
			// bound the local dial instead of blocking on the OS default
			stats.activeConns.Add(1)
			dialer := net.Dialer{Timeout: 10 * time.Second}
			r, err := dialer.DialContext(ctx, "tcp", mapping.Mapped.String())
			if err != nil {
				y.logger.Errorf("Failed to connect to local %s: %s", mapping.Mapped, err)
				stats.connClosed()
				c.Close()
				continue
			}
			stats.connClosed()

			cc := wrapCountingConn(c, stats)
			cc = scope.conn(cc)
			r = scope.conn(r)
			scope.goWorker(func() { types.ProxyTCP(y.core.MTU(), cc, r) })
		}
	}
}

func (y *Yggstack) handleRemoteUDPMappingCtx(scope *workerScope, key string, mapping types.UDPMapping) {
	ctx := scope.ctx

	select {
	case <-ctx.Done():
		return
	default:
	}

	mtu := y.core.MTU()
	stats := y.getOrCreateListenerStats(key, "remote-udp", mapping.Listen.String(), mapping.Mapped.String())
	defer stats.sessionsEnded()

	rawListenConn, err := y.netstack.ListenUDP(mapping.Listen)
	if err != nil {
		y.logger.Errorf("Failed to listen on remote UDP %s: %s", mapping.Listen, err)
		return
	}
	// The socket is shared by all client sessions; the wrapper attributes
	// every ReadFrom/WriteTo to this mapping's byte counters
	udpListenConn := wrapCountingPacketConn(rawListenConn, stats)
	defer udpListenConn.Close()

	if !scope.own(udpListenConn) {
		return
	}

	y.logger.Infof("Exposing local UDP %s on Yggdrasil port %d", mapping.Mapped, mapping.Listen.Port)

	localUdpConnections := new(sync.Map)
	udpBuffer := make([]byte, mtu)
	nextSweep := time.Now().Add(udpSweepInterval)

	for {
		udpListenConn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))

		select {
		case <-ctx.Done():
			return
		default:
			bytesRead, remoteUdpAddr, err := udpListenConn.ReadFrom(udpBuffer)
			if err != nil {
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					if now := time.Now(); now.After(nextSweep) {
						sweepIdleUDPSessions(localUdpConnections, now)
						nextSweep = now.Add(udpSweepInterval)
					}
					continue
				}
				select {
				case <-ctx.Done():
					return
				default:
					return
				}
			}

			connKey := remoteUdpAddr.String()
			var sess *udpSession

			if v, ok := localUdpConnections.Load(connKey); ok {
				sess = v.(*udpSession)
				sess.touch()
			} else {
				localConn, dialErr := net.DialUDP("udp", nil, mapping.Mapped)
				if dialErr != nil {
					y.logger.Errorf("Failed to dial local UDP %s: %s", mapping.Mapped, dialErr)
					continue
				}
				// Each remote client endpoint is one counted connection; the
				// wrapper feeds both the reverse pump and the inline writes
				wrapped := scope.conn(wrapCountingConn(localConn, stats))
				sess = newUDPSession(wrapped)
				localUdpConnections.Store(connKey, sess)

				scope.goWorker(func() {
					defer wrapped.Close()
					defer localUdpConnections.CompareAndDelete(connKey, sess)
					types.ReverseProxyUDP(mtu, udpListenConn, remoteUdpAddr, wrapped)
				})
			}

			if _, err := sess.conn.Write(udpBuffer[:bytesRead]); err != nil {
				y.logger.Errorf("Failed to write to local UDP connection: %s", err)
			}
		}
	}
}
