package accessmigrate_test

import (
	"encoding/json"
	"testing"

	"github.com/crewlet/crewlet/internal/accessmigrate"
	"github.com/crewlet/crewlet/internal/config"
)

// THE MIGRATED DOCUMENT IS ONE THE ENGINE RUNS AND ADMITS.
//
// The migration writes through the rules a running company depends on, and a
// later edit is judged against every rule a person's write is: a seat the
// migration turned on with no level, or a credential it left where the rules
// refuse one, would refuse every edit afterwards. Mutation: write a Mattermost
// bot on with no roles, and admission refuses it.
func TestTheMigratedDocumentIsRunAndAdmitted(t *testing.T) {
	t.Parallel()
	root := gophrase(t)
	if _, err := accessmigrate.Apply(root, plan(t, root, gophraseVendors())); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	c, err := config.DecodeCompany(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ValidateRunnable(); err != nil {
		t.Errorf("the engine would not run the migrated document: %v", err)
	}
	if err := c.ValidateAdmission(); err != nil {
		t.Errorf("the migrated document breaks an admission rule: %v", err)
	}
}
