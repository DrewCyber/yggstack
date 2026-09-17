package types

import (
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"regexp"

	"github.com/yggdrasil-network/yggdrasil-go/src/config"
	"github.com/yggdrasil-network/yggdrasil-go/src/multicast"
)

// ConfigPublicKey checks the key before calling Public, which panics on short keys.
func ConfigPublicKey(cfg *config.NodeConfig) (ed25519.PublicKey, error) {
	if cfg == nil {
		return nil, fmt.Errorf("config not loaded")
	}
	if len(cfg.PrivateKey) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("invalid private key length: got %d, want %d", len(cfg.PrivateKey), ed25519.PrivateKeySize)
	}
	return ed25519.PrivateKey(cfg.PrivateKey).Public().(ed25519.PublicKey), nil
}

// ValidateConfig prepares multicast options before allocating any node resources.
func ValidateConfig(cfg *config.NodeConfig) ([]multicast.SetupOption, error) {
	if _, err := ConfigPublicKey(cfg); err != nil {
		return nil, err
	}
	for i, allowed := range cfg.AllowedPublicKeys {
		key, err := hex.DecodeString(allowed)
		if err != nil {
			return nil, fmt.Errorf("invalid allowed public key %d: %w", i, err)
		}
		if len(key) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("invalid allowed public key %d length: got %d, want %d", i, len(key), ed25519.PublicKeySize)
		}
	}
	options := make([]multicast.SetupOption, 0, len(cfg.MulticastInterfaces))
	for i, intf := range cfg.MulticastInterfaces {
		regex, err := regexp.Compile(intf.Regex)
		if err != nil {
			return nil, fmt.Errorf("invalid multicast regex %d: %w", i, err)
		}
		options = append(options, multicast.MulticastInterface{
			Regex: regex, Beacon: intf.Beacon, Listen: intf.Listen,
			Port: intf.Port, Priority: uint8(intf.Priority), Password: intf.Password,
		})
	}
	return options, nil
}
