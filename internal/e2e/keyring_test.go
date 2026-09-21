package e2e

import (
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/secrets"
)

// EVERY NODE IN THIS SUITE CARRIES A KEYRING, because every record on every
// state log is signed under it and a node without one refuses to start.
//
// The harness had none anywhere, which was survivable only while nothing
// authenticated a record. It is a fixture rather than a fixed constant so that
// two suites running at once cannot accidentally share one, and it is the same
// across a fleet's members because that is precisely what the fleet cases are
// for: a member keyed differently would refuse every peer's records, which is
// the failure the signature exists to make impossible to reach by accident.
func withKeyring(t *testing.T, boot *config.Bootstrap) {
	t.Helper()
	boot.Secrets.ActiveKeyID = "e2e"
	boot.Secrets.Keys = []config.SecretKey{{ID: "e2e", Material: testKeyMaterial(t)}}
}

// fleetKeyring is withKeyring for a mesh: one ring, handed to every member.
func fleetKeyring(t *testing.T) (string, string) {
	t.Helper()
	return "e2e", testKeyMaterial(t)
}

// testKeyMaterial mints one THROUGH THE MINTER `crewlet secrets keygen` uses,
// so a suite's key is a key rather than a fixture's idea of one: the size, the
// randomness and the encoding are all read off the production definition, and
// a change to any of them reaches this suite in the same commit.
func testKeyMaterial(t *testing.T) string {
	t.Helper()
	key, err := secrets.GenerateKey()
	if err != nil {
		t.Fatalf("no randomness for the test keyring: %v", err)
	}
	return secrets.EncodeKey(key)
}
