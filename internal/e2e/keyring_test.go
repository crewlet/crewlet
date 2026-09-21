package e2e

import (
	"crypto/rand"
	"encoding/base64"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
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

func testKeyMaterial(t *testing.T) string {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("no randomness for the test keyring: %v", err)
	}
	return base64.StdEncoding.EncodeToString(key)
}
