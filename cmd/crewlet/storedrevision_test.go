package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/config"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/store"
)

// storedRevisionDoc is what a store can hold and this build could not have
// written: a field only a newer build knows, and a delegate template naming a
// model provider the company does not configure, which this build's validator
// refuses.
const storedRevisionDoc = `{"name":"Nimbus",` +
	`"a_setting_from_a_newer_build":{"depth":3},` +
	`"providers":{"sandbox":{"fake":true},"llm":{"main":{"model":"claude-sonnet-5"}}},` +
	`"workers":{"researcher":{"description":"reads sources and reports findings",` +
	`"system_prompt":"You research.","model":"nonexistent"}}}`

// serveStored stores raw bytes as the revision this node boots on, the way one
// its reconciler applied lands there, and returns its id.
//
// A REVISION THIS BUILD CANNOT RUN may be served this way too: online a node
// serves only what its engine applied, so a boot row this build refuses was
// served by another build — the binary was downgraded, or the store restored —
// and that is the state those cases stand for.
func serveStored(t *testing.T, cfg, payload string) string {
	t.Helper()
	return serveStoredAt(t, cfg, payload, time.Now().UTC())
}

// runnableRevisionDoc is a company document this build runs.
const runnableRevisionDoc = `{"name":"Nimbus",` +
	`"providers":{"sandbox":{"fake":true},"llm":{"main":{"model":"claude-sonnet-5"}}}}`

// serveStoredAt is [serveStored] at an activation instant the case chooses,
// which the store records as the activation this node applied.
func serveStoredAt(t *testing.T, cfg, payload string, activated time.Time) string {
	t.Helper()
	cs, closeStore, err := openConfigStore(t.Context(), cfg, "")
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	defer closeStore()
	// SEALED, as every revision a node holding a keyring keeps is: the node
	// this stands in for fetched it from its peers, and a plaintext one would
	// have been refused before it was ever applied.
	sealed, err := secrets.Seal(cs.cipher, []byte(payload))
	if err != nil {
		t.Fatalf("seal the revision: %v", err)
	}
	served := store.Revision{
		ID: uuid.NewString(), Source: "fleet", CreatedBy: "peer",
		CreatedByKind: iam.ActorOperator, Summary: "from a peer", Payload: sealed,
		CreatedAt: activated, ActivatedAt: activated,
	}
	// The reconciler's two verbs, in its order: the tick records the fleet's
	// revision it opened, and serves it once the engine applied it.
	if err := cs.configs.SawFleet(t.Context(), served); err != nil {
		t.Fatalf("record the fleet's revision: %v", err)
	}
	if err := cs.configs.Serve(t.Context(), served); err != nil {
		t.Fatalf("serve the revision: %v", err)
	}
	return served.ID
}

// bootActivation is the instant of the activation the revision this node boots
// on was applied from — [store.Revision.ServedAt], which is what the boot
// stamps the chart with, and NOT ActivatedAt, which a re-activation this node
// refused raises without having applied it here.
func bootActivation(t *testing.T, cfg string) time.Time {
	t.Helper()
	cs, closeStore, err := openConfigStore(t.Context(), cfg, "")
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	defer closeStore()
	boot, found, err := cs.configs.Boot(t.Context())
	if err != nil || !found {
		t.Fatalf("read the revision this node boots on: found=%v err=%v", found, err)
	}
	return boot.ServedAt
}

// companyLabel is what a case's failure calls a company: its name, or that
// there is none — never the whole document.
func companyLabel(c *config.Company) string {
	if c == nil {
		return "(no company)"
	}
	return fmt.Sprintf("%q", c.Name)
}

// bootOn is the decision `crewlet run` boots a data node on, over the store
// cfg names, with seed on its command line.
func bootOn(t *testing.T, cfg string, seed tierBSeed, log *slog.Logger) (seedPlan, error) {
	t.Helper()
	return decideOn(t, &config.Bootstrap{}, seed, cfg, log)
}

// decideOn is [decideSeed] over the store cfg names, for a case that starts no
// engine on the plan: the claim the decision takes on that store ([heldStore])
// goes back at once, as `crewlet run` gives it back once its engine has opened
// the file ([bootEngine]). Kept, it would outlast the case on a path a later
// step may hand another process — an offline command, a holder standing in
// for the engine ([holdTheStore]) — and refuse it in the name of a boot that
// never started.
func decideOn(t *testing.T, boot *config.Bootstrap, seed tierBSeed, cfg string,
	log *slog.Logger) (seedPlan, error) {

	t.Helper()
	plan, err := decideSeed(t.Context(), boot, seed, storeAt(cfg), log)
	plan.held.release()
	plan.held = heldStore{}
	return plan, err
}

// A BOOT FROM THE STORE CARRIES THE INSTANT ITS REVISION WAS ACTIVATED, which
// the engine stamps the company's chart with — and a company with no revision
// carries none.
func TestABootFromTheStoreCarriesItsRevisionsActivation(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := bootstrapForStore(t, dir)
	if plan, err := bootOn(t, cfg, tierBSeed{}, quiet()); err != nil ||
		plan.company != nil || !plan.activatedAt.IsZero() {
		t.Fatalf("an empty store booted (%v, %s, %v), want no company and no instant",
			companyLabel(plan.company), plan.activatedAt, err)
	}

	activated := time.Date(2026, 4, 1, 9, 30, 0, 123_456_000, time.UTC)
	serveStoredAt(t, cfg, duplicateKeysRevision, activated)

	plan, err := bootOn(t, cfg, tierBSeed{}, quiet())
	if err != nil || plan.company == nil {
		t.Fatalf("boot from the store: (%v, %v)", companyLabel(plan.company), err)
	}
	if !plan.activatedAt.Equal(activated) {
		t.Errorf("the boot carries activation %s, want the revision's %s", plan.activatedAt, activated)
	}
}

// THE ENGINE BOOTS ON THE REVISION ITS STORE ACTIVATED, or on nothing — and
// it is handed the instant that revision was activated at.
//
// An empty store is the UNCONFIGURED state, a company-less boot rather than a
// failure: the documented bootstrap is to start a node and push the first
// revision into it. A stored revision this build cannot run refuses the boot,
// naming it.
//
// AND THE ENGINE IS HANDED THE INSTANT ITS STORED COMPANY WAS ACTIVATED. The
// engine stamps the chart and the knowledge spaces with it at boot and writes
// neither without one, so a run that read the instant and did not pass it on
// booted a node on its store's company with no projects until the next apply.
func TestRunBootsTheEngineOnTheRevisionItsStoreActivated(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := bootstrapForStore(t, dir)
	boot := &config.Bootstrap{}

	plan, err := bootOn(t, cfg, tierBSeed{}, quiet())
	if err != nil {
		t.Fatal(err)
	}
	opts, err := bootOptions(boot, plan)
	if err != nil || opts.Company != nil || plan.act != seedNone {
		t.Fatalf("an empty store booted (%+v, %q, %v), want the unconfigured state: "+
			"no company, nothing to import and no error", companyLabel(opts.Company),
			plan.act, err)
	}
	if opts.Bootstrap != boot {
		t.Error("the engine is not handed the Tier A the run loaded")
	}

	serveStored(t, cfg, runnableRevisionDoc)
	if plan, err = bootOn(t, cfg, tierBSeed{}, quiet()); err != nil {
		t.Fatal(err)
	}
	if opts, err = bootOptions(boot, plan); err != nil ||
		opts.Company == nil || opts.Company.Name != "Nimbus" {
		t.Fatalf("the options from the store = (%+v, %v), want its company",
			companyLabel(opts.Company), err)
	}
	if stored := bootActivation(t, cfg); opts.ActivatedAt.IsZero() ||
		!opts.ActivatedAt.Equal(stored) {
		t.Errorf("the engine is handed activation %s, want the stored revision's %s",
			opts.ActivatedAt, stored)
	}

	id := serveStored(t, cfg, storedRevisionDoc)
	if _, err = bootOn(t, cfg, tierBSeed{}, quiet()); err == nil ||
		!strings.Contains(err.Error(), id) {
		t.Errorf("a store holding a revision this build cannot run booted "+
			"(%v), want a refusal naming %s", err, id)
	}
}

// A FILE THE BOOT WILL IMPORT IS NOT WHAT THE ENGINE BOOTS ON.
//
// The engine used to boot on any Tier B file it was given, and what that built
// outlived the reconcile that replaced it: the file's credential bundles
// restored into empty logins every later revision then keeps, and the store
// opened at the file's embedding width. Where the file is to be imported the
// node now boots UNCONFIGURED, and the import and the first reconcile bring the
// company in before a seat is claimed — so the engine never runs a document
// the store and the fleet have not activated.
//
// -import-company READS NOTHING FROM THE STORE, which is what keeps it a
// repair: a node whose stored revision this build refuses to run still starts
// to import the file that replaces it, where reading the store first would
// refuse the boot the repair needs. A SCRATCH store is not read either: it is
// discarded as the engine opens it, and opening it here would create the
// replicated estate a node without `data` must not have.
func TestAFileTheBootWillImportIsNotWhatTheEngineBootsOn(t *testing.T) {
	t.Parallel()
	file := parse(t, companyYAML)

	t.Run("a -company file on an empty store", func(t *testing.T) {
		t.Parallel()
		cfg := bootstrapForStore(t, t.TempDir())
		plan, err := bootOn(t, cfg, seedOf(file), quiet())
		if err != nil {
			t.Fatal(err)
		}
		if plan.company != nil || plan.act != seedBootstrap || plan.file.Company != file {
			t.Errorf("an empty store with a -company file boots on %s to %q %s, want "+
				"no company and the file bootstrapped", companyLabel(plan.company),
				plan.act, companyLabel(plan.file.Company))
		}
	})

	t.Run("-import-company over a runnable revision", func(t *testing.T) {
		t.Parallel()
		cfg := bootstrapForStore(t, t.TempDir())
		serveStored(t, cfg, runnableRevisionDoc)
		plan, err := bootOn(t, cfg, overrideOf(file), quiet())
		if err != nil {
			t.Fatal(err)
		}
		if plan.company != nil || plan.act != seedImport || plan.file.Company != file {
			t.Errorf("-import-company boots on %v to %q, want no company and the file "+
				"imported", companyLabel(plan.company), plan.act)
		}
	})

	t.Run("-import-company over a revision this build cannot run", func(t *testing.T) {
		t.Parallel()
		cfg := bootstrapForStore(t, t.TempDir())
		serveStored(t, cfg, storedRevisionDoc)
		plan, err := bootOn(t, cfg, overrideOf(file), quiet())
		if err != nil || plan.company != nil || plan.act != seedImport {
			t.Errorf("-import-company over a revision this build refuses = (%v, %q, %v), "+
				"want the boot that imports its replacement", companyLabel(plan.company), plan.act, err)
		}
	})

	t.Run("a scratch store", func(t *testing.T) {
		t.Parallel()
		scratch := &config.Bootstrap{Store: config.Store{Scratch: true}}
		unread := func(context.Context) (storedState, heldStore, error) {
			t.Error("a scratch store was read to decide the boot")
			return storedState{}, heldStore{}, nil
		}
		for _, seed := range []tierBSeed{seedOf(file), {Path: "company.yaml"}} {
			plan, err := decideSeed(t.Context(), scratch, seed, unread, quiet())
			if err != nil {
				t.Fatal(err)
			}
			want := seedNone
			if seed.Company != nil {
				want = seedBootstrap
			}
			if plan.company != nil || plan.act != want {
				t.Errorf("a scratch node with seed %+v boots on %v to %q, want no "+
					"company and %q", seed, companyLabel(plan.company), plan.act, want)
			}
		}
	})
}

// A BOOTSTRAP SEED THE STORE OUTRANKS BOOTS THE STORED REVISION, and says so.
//
// The import ignored such a file while the engine booted on it: a restart with
// a month-old company.yaml built, equipped and installed that file's company
// until the first reconcile replaced it, and for good where it moved what a
// boot decides once. Now one decision says both: the engine boots on the stored
// revision with its instant, nothing is imported, and the file is named at
// warn. A file that IS the stored revision byte for byte is no news, and says
// nothing.
//
// A STORED REVISION THIS BUILD CANNOT RUN still refuses the boot, as it does
// with no file, and with the same remedy
// ([TestABootOnARevisionThisBuildCannotRunNamesNoOfflineActivation]).
func TestABootstrapSeedTheStoreOutranksBootsTheStoredRevision(t *testing.T) {
	t.Parallel()
	file := parse(t, companyYAML)

	t.Run("a different file", func(t *testing.T) {
		t.Parallel()
		cfg := bootstrapForStore(t, t.TempDir())
		id := serveStored(t, cfg, runnableRevisionDoc)
		var logged bytes.Buffer
		plan, err := bootOn(t, cfg, seedOf(file), slog.New(slog.NewTextHandler(&logged, nil)))
		if err != nil {
			t.Fatal(err)
		}
		if plan.company == nil || plan.company.Name != "Nimbus" || plan.act != seedNone {
			t.Fatalf("a stale -company file boots on %v to %q, want the stored "+
				"Nimbus and nothing imported", companyLabel(plan.company), plan.act)
		}
		if stored := bootActivation(t, cfg); !plan.activatedAt.Equal(stored) {
			t.Errorf("the boot carries activation %s, want the stored revision's %s",
				plan.activatedAt, stored)
		}
		for _, want := range []string{"company_seed_ignored", "company.yaml", id, "-import-company"} {
			if !strings.Contains(logged.String(), want) {
				t.Errorf("the ignored file was not said with %q: %s", want, logged.String())
			}
		}
	})

	t.Run("the stored revision itself", func(t *testing.T) {
		t.Parallel()
		cfg := bootstrapForStore(t, t.TempDir())
		document, err := json.Marshal(file)
		if err != nil {
			t.Fatal(err)
		}
		serveStored(t, cfg, string(document))
		var logged bytes.Buffer
		plan, err := bootOn(t, cfg, seedOf(file), slog.New(slog.NewTextHandler(&logged, nil)))
		if err != nil {
			t.Fatal(err)
		}
		if plan.company == nil || plan.company.Name != file.Name || plan.act != seedNone {
			t.Errorf("a file that is the stored revision boots on %v to %q, want the "+
				"stored revision and nothing imported", companyLabel(plan.company), plan.act)
		}
		if logged.Len() != 0 {
			t.Errorf("a file that is the stored revision was reported: %s", logged.String())
		}
	})

	t.Run("over a revision this build cannot run", func(t *testing.T) {
		t.Parallel()
		cfg := bootstrapForStore(t, t.TempDir())
		id := serveStored(t, cfg, storedRevisionDoc)
		_, err := bootOn(t, cfg, seedOf(file), quiet())
		if err == nil {
			t.Fatal("a boot over a stored revision this build cannot run was accepted")
		}
		for _, want := range []string{id, "crewlet config import"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal does not mention %q: %v", want, err)
			}
		}
	})
}

// A SEED NOBODY DECIDED IS REFUSED by both of its consumers, rather than read
// as one that imports nothing — which would boot a node unconfigured, publish
// nothing and say nothing.
func TestAnUndecidedSeedIsRefused(t *testing.T) {
	t.Parallel()
	if _, err := bootOptions(&config.Bootstrap{}, seedPlan{}); !errors.Is(err, errUndecidedSeed) {
		t.Errorf("the engine's options from an undecided seed = %v, want it refused", err)
	}
	db := seedStore(t)
	err := seedCompany(t.Context(), db, coordmemory.NewFleet(), nil, seedPlan{},
		fixtureCipher, testNode, builds, quiet())
	if !errors.Is(err, errUndecidedSeed) {
		t.Errorf("the import of an undecided seed = %v, want it refused", err)
	}
}

// SHOW, EXPORT AND DIFF READ A REVISION WITHOUT RUNNING IT.
//
// So none of them may refuse one this build would not run: those are exactly
// the revisions an operator has to look at. show and export decoded through a
// validating reader, and diff through the AUTHORED reader, which also refused
// the newer build's field.
func TestARevisionThisBuildRefusesIsStillShownExportedAndDiffed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := bootstrapForStore(t, dir)
	if _, errs, err := configCmd(t, cfg, "import", companyFile(t, dir, "company.yaml", nil)); err != nil {
		t.Fatalf("import: %v (%s)", err, errs)
	}
	firstID := bootRevisionID(t, cfg)
	serveStored(t, cfg, storedRevisionDoc)

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
// name being prose. A peer whose build admits under rules this one does not
// share may store it, and this build must still run it.
const duplicateKeysRevision = `{"name":"Nimbus",` +
	`"providers":{"sandbox":{"fake":true},"llm":{"main":{"model":"claude-sonnet-5"}}},` +
	`"units":[` +
	`{"name":"Engineering","children":[{"name":"Platform",` +
	`"roles":[{"name":"Engineer","handle":"platform-engineer","llm":"main"}]}]},` +
	`{"name":"Product","children":[{"name":"Platform",` +
	`"roles":[{"name":"Engineer","handle":"product-engineer","llm":"main"}]}]}]}`

// duplicateKeysYAML is the same company as a person would write it.
const duplicateKeysYAML = `
name: Nimbus
providers:
  sandbox: {fake: true}
  llm:
    main: {model: claude-sonnet-5}
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

// A STORED COMPANY WITH A DUPLICATE UNIT KEY BOOTS AND EXPORTS, AND A FILE
// WITH ONE IS NEITHER IMPORTED NOR VALIDATED.
//
// The two doors meet the same document differently on purpose: the store
// holds what a build admitted under its own rules — a newer peer's may differ
// from this one's — and a file is somebody's new submission, held to every
// admission rule. The rule is an admission rule and not a runnable one, so a
// node whose fleet is already on such a revision boots on it rather than
// refusing the company it was running yesterday.
func TestADuplicateUnitKeyBootsFromTheStoreAndIsRefusedFromAFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := bootstrapForStore(t, dir)
	serveStored(t, cfg, duplicateKeysRevision)

	plan, err := bootOn(t, cfg, tierBSeed{}, quiet())
	if err != nil {
		t.Fatalf("a node refused to boot on a stored company with a duplicate unit key: %v", err)
	}
	company := plan.company
	if company == nil || len(company.Units) != 2 {
		t.Fatalf("the booted company is not the stored one: %+v", company)
	}
	if out, errs, err := configCmd(t, cfg, "export"); err != nil {
		t.Errorf("export refused a stored company with a duplicate unit key: %v (%s)", err, errs)
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

// A NODE BOOTING FROM ITS STORE BOOTS ON THE REVISION'S OWN ACTIVATION.
//
// The engine stamps the org chart's projects with the instant the revision
// was activated, and a restart must stamp the SAME instant or it republishes
// every project for a chart nobody changed. That instant is the one the store
// recorded with its active copy — never zero (which would apply no chart at
// boot) and never the moment of this boot.
func TestAStoredCompanyBootsWithTheInstantItWasActivated(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := bootstrapForStore(t, dir)
	serveStored(t, cfg, duplicateKeysRevision)
	stored := bootActivation(t, cfg)

	plan, err := bootOn(t, cfg, tierBSeed{}, quiet())
	if err != nil {
		t.Fatalf("decide the boot: %v", err)
	}
	if activatedAt := plan.activatedAt; activatedAt.IsZero() || !activatedAt.Equal(stored) {
		t.Errorf("the node boots with activation %v; the store recorded %v — a "+
			"restart would stamp the chart with something other than the "+
			"activation it is re-applying", activatedAt, stored)
	}
}

// undecodableRevisionDoc is a document no build of this one's decodes: a name
// that is not a string.
const undecodableRevisionDoc = `{"name":["not","a","name"]}`

// BOOTING IS APPLYING, AND THE REFUSAL NAMES THE REVISION AND THE WAY OUT —
// and no way out that is not one.
//
// A node's store names only a revision it applied or one an offline command of
// this build held to the same rules, so a boot row this build cannot run was
// written by another build or another store, and the first way out is that
// build. The other is an offline import, which the next start publishes only
// over the revision it is built on. The refusal used to offer `crewlet config
// activate`, which boots on a stored revision this same check refuses, and
// -import-company, which replaces whatever the fleet runs — a newer company
// included — to start one node. A document that does not decode is refused the
// same way as one that does not validate.
//
// Mutation: restore either old remedy, or leave the base out of the publish
// clause, and the case fails.
func TestABootOnARevisionThisBuildCannotRunNamesNoOfflineActivation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, document, cause string
		seed                  tierBSeed
	}{
		{"a revision that does not validate", storedRevisionDoc, "nonexistent", tierBSeed{}},
		{"a revision that does not decode", undecodableRevisionDoc, "cannot unmarshal", tierBSeed{}},
		{"with a -company file beside it", storedRevisionDoc, "nonexistent",
			seedOf(parse(t, companyYAML))},
	} {
		cfg := bootstrapForStore(t, t.TempDir())
		id := serveStored(t, cfg, tc.document)

		plan, err := bootOn(t, cfg, tc.seed, quiet())
		if err == nil {
			t.Errorf("%s: a node booted onto a revision this build cannot run: %s",
				tc.name, companyLabel(plan.company))
			continue
		}
		for _, want := range []string{id, tc.cause, "start the build that wrote it",
			"crewlet config import", "only while the fleet still runs " + id} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: the boot refusal does not mention %q: %v", tc.name, want, err)
			}
		}
		for _, never := range []string{"-import-company", "config activate"} {
			if strings.Contains(err.Error(), never) {
				t.Errorf("%s: the boot refusal offers %q: %v", tc.name, never, err)
			}
		}
	}

	// AN OFFER BUILT ON NO COMPANY — a newer build's offline import on a node
	// that knew none — is published only where the fleet has none.
	cfg := bootstrapForStore(t, t.TempDir())
	cs, closeStore, err := openConfigStore(t.Context(), cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := secrets.Seal(cs.cipher, []byte(storedRevisionDoc))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cs.configs.Offer(t.Context(), store.Offer{}, store.Revision{
		Source: "file", CreatedBy: "ops", CreatedByKind: iam.ActorOperator,
		Summary: "a newer build's import", Payload: sealed,
	}); err != nil {
		t.Fatal(err)
	}
	closeStore()
	if _, err := bootOn(t, cfg, tierBSeed{}, quiet()); err == nil ||
		!strings.Contains(err.Error(), "only if the fleet has no company yet") {
		t.Errorf("a boot over an offer built on no company = %v, want the publish "+
			"clause for a fleet with none", err)
	}
}

// A COMPANY FILE WITH A DUPLICATE UNIT KEY STARTS THE NODE THAT RUNS IT, AND
// IS REFUSED ONLY WHERE IT WOULD BE WRITTEN.
//
// `crewlet run -company company.yaml` is the documented way to run a node, and
// most boots write nothing from that file: it is byte for byte the active
// revision, or a bootstrap seed a company that exists outranks — the store's
// own, or the fleet's. When the seed was loaded against every rule, a file
// that ran yesterday and carries a duplicate unit key stopped the node from
// starting after the upgrade that added the rule. The rule still holds where
// the file becomes a new revision: where no company exists yet — none in this
// node's store and none the fleet runs — or -import-company over a different
// company.
func TestACompanyFileWithADuplicateUnitKeyRunsAndIsRefusedOnlyOnImport(t *testing.T) {
	t.Parallel()
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
	seed, err := cfg.resolveSeed(flags, "", nil, nil)
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
		err := bootSeed(t, db, fleet, seed, fixtureCipher)
		if err == nil || !strings.Contains(err.Error(), "duplicate unit key") ||
			!strings.Contains(err.Error(), file) {
			t.Fatalf("seed into an empty store = %v, want a refusal naming the file and the rule", err)
		}
		if revisions, err := db.Configs().List(t.Context(), 0, 0); err != nil || len(revisions) != 0 {
			t.Errorf("a refused seed stored %d revisions (%v), want none", len(revisions), err)
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
		applied := time.Now().UTC()
		if err := db.Configs().Serve(t.Context(), store.Revision{
			ID: uuid.NewString(), Source: "file", CreatedBy: testNode,
			CreatedByKind: iam.ActorSystem, Summary: "admitted under other rules",
			Payload: sealed, CreatedAt: applied, ActivatedAt: applied,
		}); err != nil {
			t.Fatal(err)
		}
		if err := bootSeed(t, db, coordmemory.NewFleet(), seed, fixtureCipher); err != nil {
			t.Fatalf("a node whose store holds this very file refused to boot: %v", err)
		}
	})

	t.Run("a store holding another company ignores it as a bootstrap and refuses it as an import", func(t *testing.T) {
		db := seedStore(t)
		if err := bootSeed(t, db, coordmemory.NewFleet(), seedOf(parse(t, companyYAML)), fixtureCipher); err != nil {
			t.Fatal(err)
		}
		if err := bootSeed(t, db, coordmemory.NewFleet(), seed, fixtureCipher); err != nil {
			t.Errorf("a bootstrap seed the store outranks refused to boot: %v", err)
		}
		override := seed
		override.Override = true
		err := bootSeed(t, db, coordmemory.NewFleet(), override, fixtureCipher)
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
// admission rules are not theirs to enforce: running a company depends on none
// of them, so a deployment whose file carries a duplicate must still be
// provisionable. Each command is expected to get PAST the load and
// stop on its own business (this company enables no integration), which is
// what the absence of the loader's "company config" prefix shows.
func TestVendorCommandsReadACompanyFileWithADuplicateUnitKey(t *testing.T) {
	t.Parallel()
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
