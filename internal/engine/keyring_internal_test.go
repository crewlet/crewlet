package engine

import (
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/secrets"
)

// testBootstrap is [config.DefaultBootstrap] plus the one thing a runnable
// Tier A has that the defaults deliberately do not: a keyring.
//
// Every record on every state log is signed under it and a node that has none
// refuses to start, so a case that boots an engine from the bare defaults
// fails on a configuration fault rather than on its own subject. The key is
// not in the defaults and must not be — a default key is a key every
// deployment in the world shares, which is the opposite of what signing a
// record buys — so it is minted here instead, per call, through the minter
// `crewlet secrets keygen` uses: the size, the randomness and the encoding are
// read off the production definition rather than restated in a fixture.
func testBootstrap(t *testing.T) config.Bootstrap {
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
