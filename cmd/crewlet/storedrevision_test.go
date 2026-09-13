package main

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// storedRevisionDoc is what a store can hold and this build could not have
// written: a field only a newer build knows, and a seat naming a model
// provider the company does not configure, which this build's validator
// refuses.
const storedRevisionDoc = `{"name":"Nimbus",` +
	`"a_setting_from_a_newer_build":{"depth":3},` +
	`"providers":{"llm":{"main":{"type":"anthropic","model":"claude-sonnet-5",` +
	`"api_keys":["${ANTHROPIC_API_KEY}"]}}},` +
	`"roles":[{"name":"CEO","handle":"ceo","llm":"nonexistent"}]}`

// activateStored marks raw bytes active in the node's store, the way a peer's
// revision lands there, and returns its id.
func activateStored(t *testing.T, cfg, payload string) string {
	t.Helper()
	cs, closeStore, err := openConfigStore(t.Context(), cfg)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	defer closeStore()
	id, err := cs.configs.InsertActive(t.Context(), store.Revision{
		Source: "fleet", CreatedBy: "peer", Summary: "from a peer",
		Payload: []byte(payload), CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("store the revision: %v", err)
	}
	return id
}

// SHOW, EXPORT AND DIFF READ A REVISION WITHOUT RUNNING IT.
//
// So none of them may refuse one this build would not run: those are exactly
// the revisions an operator has to look at. show and export decoded through a
// validating reader, and diff through the AUTHORED reader, which also refused
// the newer build's field.
func TestARevisionThisBuildRefusesIsStillShownExportedAndDiffed(t *testing.T) {
	dir := t.TempDir()
	cfg := bootstrapForStore(t, dir)
	if _, errs, err := configCmd(t, cfg, "import", companyFile(t, dir, "company.yaml", nil)); err != nil {
		t.Fatalf("import: %v (%s)", err, errs)
	}
	firstID := activeRevisionID(t, cfg)
	activateStored(t, cfg, storedRevisionDoc)

	for _, args := range [][]string{
		{"show"},
		{"export"},
		{"diff", firstID},
	} {
		out, errs, err := configCmd(t, cfg, args...)
		if err != nil {
			t.Errorf("config %s refused a stored revision: %v (%s)",
				strings.Join(args, " "), err, errs)
			continue
		}
		if !strings.Contains(out, "nonexistent") {
			t.Errorf("config %s did not show the stored document:\n%s",
				strings.Join(args, " "), out)
		}
	}
}

// BOOTING IS APPLYING, AND THE REFUSAL NAMES THE REVISION AND THE WAY OUT.
//
// The node is not serving its API yet, so an offline import is the repair,
// and an error that did not say which revision failed would leave the
// operator reading the store by hand.
func TestBootingOnARevisionThisBuildCannotRunNamesTheRevision(t *testing.T) {
	dir := t.TempDir()
	cfg := bootstrapForStore(t, dir)
	id := activateStored(t, cfg, storedRevisionDoc)

	company, err := companyFromStore(t.Context(), cfg)
	if err == nil {
		t.Fatalf("a node booted onto a revision this build cannot run: %+v", company)
	}
	for _, want := range []string{id, "crewlet config import", "nonexistent"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the boot refusal does not mention %q: %v", want, err)
		}
	}
}
