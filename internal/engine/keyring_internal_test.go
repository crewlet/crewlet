package engine

import (
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/secrets"
)

// testBootstrap is [config.DefaultBootstrap] plus the one thing a runnable
// Tier A has that the defaults deliberately do not: a keyring, which signs
// every state-log record and without which a node refuses to start. A default
// key would be one every deployment shares, defeating the signing, so it is
// minted per call through the minter `crewlet secrets keygen` uses, reading
// size, randomness and encoding off the production definition.
func testBootstrap(t testing.TB) config.Bootstrap {
	t.Helper()
	b := config.DefaultBootstrap()
	key, err := secrets.GenerateKey()
	if err != nil {
		t.Fatalf("no randomness for the test keyring: %v", err)
	}
	b.Secrets.ActiveKeyID = "test"
	b.Secrets.Keys = []config.SecretKey{{ID: "test", Material: secrets.EncodeKey(key)}}
	return b
}
