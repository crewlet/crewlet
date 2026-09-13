package main

import (
	"bytes"
	"os"
	"path/filepath"
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

// duplicateNamesRevision breaks both admission rules and no runnable one: two
// units called "Platform", each with a seat called "Engineer" on its own
// handle. A build before those rules admitted it.
const duplicateNamesRevision = `{"name":"Nimbus",` +
	`"providers":{"llm":{"main":{"type":"anthropic","model":"claude-sonnet-5",` +
	`"api_keys":["${ANTHROPIC_API_KEY}"]}}},` +
	`"units":[` +
	`{"name":"Engineering","children":[{"name":"Platform",` +
	`"roles":[{"name":"Engineer","handle":"platform-engineer","llm":"main"}]}]},` +
	`{"name":"Product","children":[{"name":"Platform",` +
	`"roles":[{"name":"Engineer","handle":"product-engineer","llm":"main"}]}]}]}`

// duplicateNamesYAML is the same company as a person would write it.
const duplicateNamesYAML = `
name: Nimbus
providers:
  llm:
    main: {type: anthropic, model: claude-sonnet-5, api_keys: ["${ANTHROPIC_API_KEY}"]}
units:
  - name: Engineering
    children:
      - name: Platform
        roles: [{name: Engineer, handle: platform-engineer, llm: main}]
  - name: Product
    children:
      - name: Platform
        roles: [{name: Engineer, handle: product-engineer, llm: main}]
`

// A STORED COMPANY WITH DUPLICATE NAMES BOOTS AND EXPORTS, AND A FILE WITH
// THEM IS NEITHER IMPORTED NOR VALIDATED.
//
// The two doors meet the same document differently on purpose: the store
// holds what an older build admitted, and a file is somebody's new submission.
func TestDuplicateNamesBootFromTheStoreAndAreRefusedFromAFile(t *testing.T) {
	dir := t.TempDir()
	cfg := bootstrapForStore(t, dir)
	activateStored(t, cfg, duplicateNamesRevision)

	company, err := companyFromStore(t.Context(), cfg)
	if err != nil {
		t.Fatalf("a node refused to boot on a stored company with duplicate names: %v", err)
	}
	if company == nil || len(company.Units) != 2 {
		t.Fatalf("the booted company is not the stored one: %+v", company)
	}
	if out, errs, err := configCmd(t, cfg, "export"); err != nil {
		t.Errorf("export refused a stored company with duplicate names: %v (%s)", err, errs)
	} else if !strings.Contains(out, "product-engineer") {
		t.Errorf("export did not print the stored company:\n%s", out)
	}

	file := filepath.Join(dir, "duplicates.yaml")
	if err := os.WriteFile(file, []byte(duplicateNamesYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := configCmd(t, cfg, "import", file); err == nil ||
		!strings.Contains(err.Error(), "duplicate unit name") {
		t.Errorf("import of a file with duplicate names = %v, want a refusal naming the rule", err)
	}
	var out, errOut bytes.Buffer
	if err := run([]string{"validate", "-config", cfg, "-company", file}, &out, &errOut); err == nil {
		t.Errorf("validate accepted a file with duplicate names:\n%s%s", out.String(), errOut.String())
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
