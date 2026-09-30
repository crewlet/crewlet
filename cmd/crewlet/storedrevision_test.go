package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/store"
)

// storedRevisionDoc is what a store can hold and this build could not have
// written: a field only a newer build knows, and a delegate template naming a
// model provider the company does not configure, which this build's validator
// refuses.
//
// A SETTINGS DOCUMENT, with no org chart in it: a revision this build APPLIES
// carries none, so a fixture that held one would exercise the chart refusal
// rather than the rule each case here is about.
const storedRevisionDoc = `{"name":"Nimbus",` +
	`"a_setting_from_a_newer_build":{"depth":3},` +
	`"providers":{"llm":{"main":{"type":"anthropic","model":"claude-sonnet-5",` +
	`"api_keys":["${ANTHROPIC_API_KEY}"]}}},` +
	`"workers":{"researcher":{"description":"reads sources and reports findings",` +
	`"system_prompt":"You research.","model":"nonexistent"}}}`

// activateStored marks raw bytes active in the node's store, the way a peer's
// revision lands there, and returns its id.
func activateStored(t *testing.T, cfg, payload string) string {
	t.Helper()
	cs, closeStore, err := openConfigStore(t.Context(), cfg, "")
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	defer closeStore()
	// SEALED, as every revision a node holding a keyring keeps is: the node
	// this stands in for fetched it from its peers, and a plaintext one would
	// have been refused before it was ever adopted.
	sealed, err := secrets.Seal(cs.cipher, []byte(payload))
	if err != nil {
		t.Fatalf("seal the revision: %v", err)
	}
	id, err := cs.configs.InsertActive(t.Context(), store.Revision{
		Source: "fleet", CreatedBy: "peer", Summary: "from a peer",
		Payload: sealed, CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("store the revision: %v", err)
	}
	return id
}

// runnableRevisionDoc is a settings document this build runs.
const runnableRevisionDoc = `{"name":"Nimbus",` +
	`"providers":{"llm":{"main":{"type":"anthropic","model":"claude-sonnet-5",` +
	`"api_keys":["${ANTHROPIC_API_KEY}"]}}}}`

// `crewlet run` BOOTS ON THE COMPANY IT WAS GIVEN: its store's when it was
// given no file, and the file's when it was — without reading the store.
//
// An empty store is the UNCONFIGURED state, a company-less boot rather than a
// failure: the documented bootstrap is to start a node and push the first
// revision into it. And a file wins without the store being read, which is
// what keeps `-import-company` a repair: a node whose stored revision this
// build refuses to run still starts on the file that replaces it, where
// reading the store first would refuse the boot the repair needs.
func TestRunBootsTheEngineOnTheCompanyItWasGiven(t *testing.T) {
	dir := t.TempDir()
	cfg := bootstrapForStore(t, dir)
	boot := &config.Bootstrap{}

	opts, err := bootOptions(t.Context(), cfg, boot, nil)
	if err != nil || opts.Company != nil {
		t.Fatalf("an empty store booted (%+v, %v), want the unconfigured state: "+
			"no company and no error", opts.Company, err)
	}
	if opts.Bootstrap != boot {
		t.Error("the engine is not handed the Tier A the run loaded")
	}

	activateStored(t, cfg, runnableRevisionDoc)
	if opts, err = bootOptions(t.Context(), cfg, boot, nil); err != nil ||
		opts.Company == nil || opts.Company.Name != "Nimbus" {
		t.Fatalf("the options from the store = (%+v, %v), want its company",
			opts.Company, err)
	}

	id := activateStored(t, cfg, storedRevisionDoc)
	if _, err = bootOptions(t.Context(), cfg, boot, nil); err == nil ||
		!strings.Contains(err.Error(), id) {
		t.Errorf("a store holding a revision this build cannot run booted "+
			"(%v), want a refusal naming %s", err, id)
	}
	file := &config.Company{Name: "From a file"}
	if opts, err = bootOptions(t.Context(), cfg, boot, file); err != nil || opts.Company != file {
		t.Errorf("a file's company boots as (%v, %v) over a stored revision this "+
			"build cannot run, want the file's — -import-company is how that "+
			"revision is replaced", opts.Company, err)
	}
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

// duplicateKeysRevision breaks an admission rule and no runnable one: two
// units called "Platform" declaring no id, so both are keyed `platform` — each
// with a seat called "Engineer" on its own handle, which breaks nothing, a
// name being prose. A build before the rule admitted it.
const duplicateKeysRevision = `{"name":"Nimbus",` +
	`"providers":{"llm":{"main":{"type":"anthropic","model":"claude-sonnet-5",` +
	`"api_keys":["${ANTHROPIC_API_KEY}"]}}},` +
	`"units":[` +
	`{"name":"Engineering","children":[{"name":"Platform",` +
	`"roles":[{"name":"Engineer","handle":"platform-engineer","llm":"main"}]}]},` +
	`{"name":"Product","children":[{"name":"Platform",` +
	`"roles":[{"name":"Engineer","handle":"product-engineer","llm":"main"}]}]}]}`

// duplicateKeysYAML is the same company as a person would write it.
const duplicateKeysYAML = `
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

// A REVISION STILL CARRYING AN ORG CHART IS SHOWN AND EXPORTED, REFUSED AT
// BOOT, AND A FILE WITH A DUPLICATE UNIT KEY IS NEITHER IMPORTED NOR
// VALIDATED.
//
// # Why the boot refuses it rather than running it
//
// The chart is a state-log domain of its own, and a node derives its seats
// from that log. A revision written before the split still holds the chart
// INSIDE it, so applying one would mean choosing between two wrong answers:
// run the chart from a document no other node reads, or drop it and serve a
// company with no seats at all from bytes that decoded cleanly. It is refused
// instead, naming the one-line repair.
//
// # And why every READ still answers
//
// That revision is exactly the one an operator has to look at in order to
// repair it. A read that refused it would leave them with a node that will not
// start and no way to see what it is refusing.
//
// The two doors then meet a FILE differently again, which is the rest of this
// case: the store holds what an older build admitted, and a file is somebody's
// new submission, held to every admission rule.
func TestAnOldChartIsShownExportedAndRefusedAtBoot(t *testing.T) {
	dir := t.TempDir()
	cfg := bootstrapForStore(t, dir)
	id := activateStored(t, cfg, duplicateKeysRevision)

	company, err := companyFromStore(t.Context(), cfg)
	if err == nil {
		t.Fatalf("a node booted onto a revision that still carries a chart: %+v", company)
	}
	for _, want := range []string{"org chart", "crewlet config import"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the boot refusal does not mention %q: %v", want, err)
		}
	}
	if !strings.Contains(err.Error(), id) && !strings.Contains(err.Error(), "active revision") {
		t.Errorf("the boot refusal names neither the revision nor which one it is: %v", err)
	}
	if out, errs, err := configCmd(t, cfg, "export"); err != nil {
		t.Errorf("export refused a revision carrying a chart: %v (%s)", err, errs)
	} else if !strings.Contains(out, "product-engineer") {
		t.Errorf("export did not print the stored company:\n%s", out)
	}

	file := filepath.Join(dir, "duplicates.yaml")
	if err := os.WriteFile(file, []byte(duplicateKeysYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := configCmd(t, cfg, "import", file); err == nil ||
		!strings.Contains(err.Error(), "duplicate unit key") {
		t.Errorf("import of a file with a duplicate unit key = %v, want a refusal naming the rule", err)
	}
	var out, errOut bytes.Buffer
	if err := run([]string{"validate", "-config", cfg, "-company", file}, &out, &errOut); err == nil {
		t.Errorf("validate accepted a file with a duplicate unit key:\n%s%s", out.String(), errOut.String())
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

// A COMPANY FILE WITH A DUPLICATE UNIT KEY STARTS THE NODE THAT RUNS IT, AND
// IS REFUSED ONLY WHERE IT WOULD BE WRITTEN.
//
// `crewlet run -company company.yaml` is the documented way to run a node, and
// most boots write nothing from that file: it is byte for byte the active
// revision, or a bootstrap seed the store's own company outranks. When the
// seed was loaded against every rule, a file that ran yesterday and carries a
// duplicate name stopped the node from starting after the upgrade that added
// the rule. The rule still holds where the file becomes a new revision: an
// empty store, or -import-company over a different company.
func TestACompanyFileWithADuplicateUnitKeyRunsAndIsRefusedOnlyOnImport(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "company.yaml")
	if err := os.WriteFile(file, []byte(duplicateKeysYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	cfg := addConfigFlags(flags)
	if err := flags.Parse([]string{"-company", file}); err != nil {
		t.Fatal(err)
	}
	seed, err := cfg.resolveSeed(flags, "")
	if err != nil {
		t.Fatalf("crewlet run refused a company file that breaks only an admission rule: %v", err)
	}
	if seed.Company == nil || len(seed.Company.Units) != 2 {
		t.Fatalf("the seed is not the file's company: %+v", seed)
	}
	document, err := json.Marshal(seed.Company)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("an empty store refuses to import it", func(t *testing.T) {
		db := seedStore(t)
		fleet := coordmemory.NewFleet()
		err := seedCompany(t.Context(), db, fleet, nil, seed, fixtureCipher, quiet())
		if err == nil || !strings.Contains(err.Error(), "duplicate unit key") ||
			!strings.Contains(err.Error(), file) {
			t.Fatalf("seed into an empty store = %v, want a refusal naming the file and the rule", err)
		}
		if _, found, _ := db.Configs().Active(t.Context()); found {
			t.Error("a refused seed stored a revision")
		}
		if _, found, _ := fleet.Target(t.Context()); found {
			t.Error("a refused seed moved the activation pointer")
		}
	})

	t.Run("a store already holding it boots on it", func(t *testing.T) {
		db := seedStore(t)
		sealed, err := secrets.Seal(fixtureCipher, document)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Configs().InsertActive(t.Context(), store.Revision{
			Source: "file", CreatedBy: "node", Summary: "seeded before the rule",
			Payload: sealed, CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
		if err := seedCompany(t.Context(), db, coordmemory.NewFleet(), nil, seed, fixtureCipher, quiet()); err != nil {
			t.Fatalf("a node whose store holds this very file refused to boot: %v", err)
		}
	})

	t.Run("a store holding another company ignores it as a bootstrap and refuses it as an import", func(t *testing.T) {
		db := seedStore(t)
		if err := seedCompany(t.Context(), db, coordmemory.NewFleet(), nil,
			seedOf(parse(t, companyYAML)), fixtureCipher, quiet()); err != nil {
			t.Fatal(err)
		}
		if err := seedCompany(t.Context(), db, coordmemory.NewFleet(), nil, seed, fixtureCipher, quiet()); err != nil {
			t.Errorf("a bootstrap seed the store outranks refused to boot: %v", err)
		}
		override := seed
		override.Override = true
		err := seedCompany(t.Context(), db, coordmemory.NewFleet(), nil, override, fixtureCipher, quiet())
		if err == nil || !strings.Contains(err.Error(), "duplicate unit key") {
			t.Errorf("-import-company of a file with a duplicate unit key = %v, want a refusal", err)
		}
		revisions, err := db.Configs().List(t.Context(), 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(revisions) != 1 {
			t.Errorf("%d revisions, want only the original: a refused import stores nothing", len(revisions))
		}
	})
}

// THE VENDOR COMMANDS ACT ON A COMPANY FILE WITH A DUPLICATE UNIT KEY.
//
// They read the company a node runs and write none of it as a revision, so the
// admission rules are not theirs to enforce: a deployment whose file carries a
// duplicate from before the rule must still be provisionable after the
// upgrade that added it. Each command is expected to get PAST the load and
// stop on its own business (this company enables no integration), which is
// what the absence of the loader's "company config" prefix shows.
func TestVendorCommandsReadACompanyFileWithADuplicateUnitKey(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "company.yaml")
	if err := os.WriteFile(file, []byte(duplicateKeysYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	noBootstrap := filepath.Join(dir, "absent.yaml")
	for _, args := range [][]string{
		{"jira", "provision", "-dry-run", "-config", noBootstrap, file},
		{"github", "provision", "-dry-run", "-config", noBootstrap, file},
		{"gitlab", "provision", "-dry-run", "-config", noBootstrap, file},
		{"mattermost", "provision", "-dry-run", "-config", noBootstrap, file},
		{"mattermost", "doctor", "-config", noBootstrap, file},
		{"confluence", "provision", "-dry-run", "-config", noBootstrap, file},
		{"confluence", "resync", "-config", noBootstrap, file},
		{"confluence", "import", "-dry-run", "-config", noBootstrap, file, dir},
		{"slack", "provision", "-dry-run", "-config", noBootstrap, file},
		{"llm", "status", "-config", noBootstrap, "-company", file},
	} {
		var out, errOut bytes.Buffer
		err := run(args, &out, &errOut)
		if err != nil && (strings.Contains(err.Error(), "company config "+file) ||
			strings.Contains(err.Error(), "duplicate")) {
			t.Errorf("crewlet %s refused a company file that breaks only an admission rule: %v",
				strings.Join(args[:2], " "), err)
		}
	}
}
