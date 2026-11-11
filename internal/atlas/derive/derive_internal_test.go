package derive

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
)

// EVERY INTEGRATION KIND HAS AN ATLAS ROW, and every row is an integration's:
// a kind the engine learns to connect is an account the atlas must show, and
// nothing fails silently when it does not.
func TestEveryIntegrationKindHasAnAtlasRow(t *testing.T) {
	t.Parallel()
	for _, k := range integration.Kinds {
		if integrationRows[k] == nil {
			t.Errorf("integration %s has no atlas row", k)
		}
	}
	for k := range integrationRows {
		if !k.Valid() {
			t.Errorf("an atlas row for %q, which is no integration", k)
		}
	}
}

// EVERY HARNESS REACHES A VENDOR the atlas knows, and every sign-in mode reads
// as one of the atlas's words, both ways.
func TestEveryHarnessReachesAVendor(t *testing.T) {
	t.Parallel()
	for _, h := range config.Harnesses {
		if !harnessVendors[h].Valid() {
			t.Errorf("harness %s reaches no vendor", h)
		}
	}
	if len(harnessVendors) != len(config.Harnesses) {
		t.Errorf("%d harness rows for %d harnesses", len(harnessVendors), len(config.Harnesses))
	}
	for _, m := range config.HarnessAuthModes {
		if !slices.Contains(atlas.FactSignIn.Spec().Words, signIns[m]) {
			t.Errorf("sign-in mode %s reads as no word of the atlas", m)
		}
	}
	if len(signIns) != len(config.HarnessAuthModes) {
		t.Errorf("%d sign-in rows for %d modes", len(signIns), len(config.HarnessAuthModes))
	}
}

// THE SETTINGS THE ATLAS READS HOLD NO SECRET: the only way to make them masks
// every credential, so a derivation that read the wrong field would still read
// none. This is the test that goes red when the redacting copy is bypassed.
func TestTheSettingsTheAtlasReadsHoldNoSecret(t *testing.T) {
	t.Parallel()
	c := AcmeCompany()
	if planted := PlantSecrets(c); planted < 40 {
		t.Fatalf("planted %d secrets: the walk no longer reaches the document's secret positions", planted)
	}
	held, err := json.Marshal(FromSettings(c, "r1", AcmeDeployment).company)
	if err != nil {
		t.Fatal(err)
	}
	if i := strings.Index(string(held), Canary); i >= 0 {
		end := min(len(held), i+120)
		t.Fatalf("the settings the atlas reads hold a credential: %s", held[i:end])
	}
	original, _ := json.Marshal(c)
	if !strings.Contains(string(original), Canary) {
		t.Fatal("the planted document holds no canary: the test proves nothing")
	}
}
