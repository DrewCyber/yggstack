package mobile

import (
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/yggdrasil-network/yggdrasil-go/src/config"
)

func TestConfigValidationAtomic(t *testing.T) {
	y := offlineNode(t)
	original := y.config
	for _, n := range []int{0, 1, 31, 32, 63, 65} {
		for _, text := range []string{
			fmt.Sprintf(`{"PrivateKey":%q}`, strings.Repeat("01", n)),
			fmt.Sprintf("{\nPrivateKey: %q\n}", strings.Repeat("01", n)),
		} {
			if err := y.LoadConfigJSON(text); err == nil {
				t.Fatalf("accepted key length %d", n)
			}
			if y.config != original {
				t.Fatal("failed load replaced config")
			}
		}
	}
	for _, fields := range []map[string]any{
		{"AllowedPublicKeys": []string{"01"}},
		{"AllowedPublicKeys": []string{strings.Repeat("00", 33)}},
		{"AllowedPublicKeys": []string{"zz"}},
		{"MulticastInterfaces": []map[string]any{{"Regex": "["}}},
	} {
		fields["PrivateKey"] = strings.Repeat("01", ed25519.PrivateKeySize)
		data, _ := json.Marshal(fields)
		if err := y.LoadConfigJSON(string(data)); err == nil {
			t.Fatalf("accepted invalid config %s", data)
		}
		if y.config != original {
			t.Fatal("failed load replaced config")
		}
	}
	good, err := GenerateConfig()
	if err != nil {
		t.Fatal(err)
	}
	if err := y.LoadConfigJSON(good); err != nil {
		t.Fatal(err)
	}
	for _, getter := range []func() (string, error){y.GetAddress, y.GetSubnet, y.GetPublicKey} {
		if value, err := getter(); err != nil || value == "" {
			t.Fatalf("getter: %q %v", value, err)
		}
	}
}

func TestInvalidConfigGettersAndStartup(t *testing.T) {
	y := offlineNode(t)
	for _, n := range []int{0, 1, 31, 32, 63, 65} {
		y.config = &config.NodeConfig{PrivateKey: make(config.KeyBytes, n)}
		for _, getter := range []func() (string, error){y.GetAddress, y.GetSubnet, y.GetPublicKey} {
			if _, err := getter(); err == nil {
				t.Fatalf("getter accepted length %d", n)
			}
		}
		if err := y.AddRemoteTCPMapping(1234, "127.0.0.1:1234"); err == nil {
			t.Fatal("TCP accepted invalid key")
		}
		if err := y.AddRemoteUDPMapping(1234, "127.0.0.1:1234"); err == nil {
			t.Fatal("UDP accepted invalid key")
		}
		if err := y.Start("", ""); err == nil {
			t.Fatal("Start accepted invalid key")
		}
		if y.run != nil || y.core != nil || y.netstack != nil {
			t.Fatal("allocated runtime for invalid config")
		}
	}
	y.config = config.GenerateConfig()
	y.config.MulticastInterfaces[0].Regex = "["
	// Stats survive preflight failure: even resetListenerStats must not run.
	stats := y.getOrCreateListenerStats("sentinel", "socks", "", "")
	stats.txBytes.Add(7)
	if err := y.Start("", ""); err == nil {
		t.Fatal("Start accepted invalid regex")
	}
	if y.run != nil || y.core != nil || y.netstack != nil {
		t.Fatal("allocated runtime before regex validation")
	}
	if value, ok := y.listenerStats.Load("sentinel"); !ok || value != stats {
		t.Fatal("preflight changed runtime")
	}
}
