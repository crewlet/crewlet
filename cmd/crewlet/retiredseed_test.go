package main

import (
	"bytes"
	"context"
	"flag"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/engine"
)

// legacyCompanyYAML is a company.yaml written before per-seat access, as a
// deployment upgraded to this build still carries it: the GitHub tier,
// Datadog's company-wide role and the single Mattermost channel, each retired
// and refused by name, and nothing else wrong with it.
const legacyCompanyYAML = `
name: Legacy
providers:
  sandbox: {fake: true}
integrations:
  datadog:
    enabled: true
    webhook_token: "${DATADOG_WEBHOOK_TOKEN}"
    route_to: none
    provisioning:
      site: datadoghq.com
      api_key: "${DATADOG_API_KEY}"
      app_key: "${DATADOG_APP_KEY}"
      role: r-1
units:
  - name: Platform
    roles:
      - name: SRE
        handle: sre
        integrations:
          github:
            enabled: false
            tier: full_access
          mattermost:
            enabled: false
            channel: town-square
`

// legacyKeys is each retired key legacyCompanyYAML carries, by its path and
// line, as the warning names it.
var legacyKeys = []string{
	"integrations.datadog.provisioning.role (line 14)",
	"units[0].roles[0].integrations.github.tier (line 23)",
	"units[0].roles[0].integrations.mattermost.channel (line 26)",
}

// legacyFile writes doc as the company.yaml in dir, and answers its path and
// the refusal every reader of it that holds the retired keys gives
// ([config.LoadCompanyToRun]), which is the message a refused import must
// carry unchanged.
func legacyFile(t *testing.T, dir, doc string) (string, error) {
	t.Helper()
	path := filepath.Join(dir, "company.yaml")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	_, refusal := config.LoadCompanyToRun(path, nil)
	if refusal == nil || !strings.Contains(refusal.Error(), "was removed") {
		t.Fatalf("the fixture is not refused for its retired keys: %v", refusal)
	}
	return path, refusal
}

// resolveRunSeed is the seed `crewlet run` resolves from args, and from
// -import-company naming importPath where it is set.
func resolveRunSeed(t *testing.T, importPath string, args ...string) (tierBSeed, error) {
	t.Helper()
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	cfg := addConfigFlags(flags)
	if err := flags.Parse(args); err != nil {
		t.Fatal(err)
	}
	return cfg.resolveSeed(flags, importPath, nil, nil)
}

// A -company FILE THAT STILL CARRIES KEYS PER-SEAT ACCESS RETIRED STARTS THE
// NODE ON THE COMPANY THAT EXISTS, AND IS REFUSED ONLY WHERE IT WOULD BE
// WRITTEN.
//
// A deployment upgraded to the build that retired them starts with the
// company.yaml it was written with, and its store already holds the company:
// the file is outranked and never imported, and the stored revision reads
// without the keys and is migrated by the engine it starts. Refusing the file
// at load refused every such boot. Now the node starts on the stored company,
// and one warning names each retired key by its path and line and says how to
// refresh the file. Where the file WOULD be written, an empty store with no
// fleet, it is refused with the very message it always was: no revision is
// written in a retired shape.
//
// Mutation: drop the refusal at the head of [importSeed], and the empty store
// imports the file read without its keys.
func TestALegacyCompanyFileStartsTheStoredCompanyAndIsRefusedOnlyOnImport(t *testing.T) {
	t.Parallel()
	file, refusal := legacyFile(t, t.TempDir(), legacyCompanyYAML)
	seed, err := resolveRunSeed(t, "", "-company", file)
	if err != nil {
		t.Fatalf("crewlet run refused a company file whose only fault is retired keys: %v", err)
	}
	if seed.Retired == nil || seed.Company == nil {
		t.Fatalf("the seed carries no set-aside refusal or no company: %+v", seed)
	}

	t.Run("a store holding the company starts on it and says the file is stale", func(t *testing.T) {
		cfg := bootstrapForStore(t, t.TempDir())
		boot := storedBoot(t, cfg)
		serveStored(t, cfg, liveRevisionDoc)
		var logged bytes.Buffer
		log := slog.New(slog.NewTextHandler(&logged, nil))

		e, plan, err := bootEngine(t.Context(), boot, seed, storeAt(cfg), engine.New, log)
		if err != nil {
			t.Fatalf("the node did not start: %v", err)
		}
		t.Cleanup(func() { e.Stop(context.WithoutCancel(t.Context())) })
		if _, err := startReconciler(t.Context(), e, boot, plan, e.Cipher(), log); err != nil {
			t.Fatalf("startReconciler: %v", err)
		}
		if name, _ := running(e); name != "Live" {
			t.Errorf("the node runs %q, want the stored Live", name)
		}
		if made := fileRevisionsOf(t, e); made != 0 {
			t.Errorf("%d revisions written from the file, want none", made)
		}
		assertStaleSaid(t, logged.String(), file)
	})

	t.Run("a fleet running a company outranks it as well", func(t *testing.T) {
		db := seedStore(t)
		fleet := coordmemory.NewFleet()
		live := activateAsPeer(t, fleet, "live-revision", []byte(`{"name":"Live"}`))
		var logged bytes.Buffer
		if err := bootSeedSaying(t, db, fleet, seed, fixtureCipher,
			slog.New(slog.NewTextHandler(&logged, nil))); err != nil {
			t.Fatalf("boot: %v", err)
		}
		assertFleetOn(t, fleet, live)
		if made := fileRevisions(t, db); len(made) != 0 {
			t.Errorf("%d revisions written from the file, want none", len(made))
		}
		assertStaleSaid(t, logged.String(), file)
	})

	t.Run("an empty store refuses to import it", func(t *testing.T) {
		cfg := bootstrapForStore(t, t.TempDir())
		boot := storedBoot(t, cfg)
		e, plan := bootDecided(t, cfg, boot, seed)
		_, err := startReconciler(t.Context(), e, boot, plan, e.Cipher(), quiet())
		if err == nil || err.Error() != refusal.Error() {
			t.Fatalf("the import into an empty store = %v,\nwant the refusal the file always earned: %v",
				err, refusal)
		}
		if revisions, err := e.Backends().Store.Configs().List(t.Context(), 0, 0); err != nil ||
			len(revisions) != 0 {
			t.Errorf("a refused import stored %d revisions (%v), want none", len(revisions), err)
		}
		if _, found, err := e.Backends().Fleet.Target(t.Context()); err != nil || found {
			t.Errorf("a refused import pointed the fleet at a revision (%v, %v)", found, err)
		}
	})
}

// -import-company OF A LEGACY FILE IS REFUSED with the message it always was:
// the override is always written, so there is nothing for the refusal to wait
// for.
func TestALegacyCompanyFileIsRefusedAsAnOverride(t *testing.T) {
	t.Parallel()
	file, refusal := legacyFile(t, t.TempDir(), legacyCompanyYAML)
	if _, err := resolveRunSeed(t, file); err == nil || err.Error() != refusal.Error() {
		t.Fatalf("-import-company of a legacy file = %v,\nwant its refusal: %v", err, refusal)
	}
}

// A LEGACY FILE WITH A GENUINE FAULT BESIDE ITS RETIRED KEYS STILL FAILS, on
// that fault: only a file whose whole problem is retired keys is carried.
//
// Mutation: set every unknown field aside in config.setAsideRemoved, not only
// the retired ones, and this file starts the node.
func TestALegacyCompanyFileWithAGenuineFaultFailsOnIt(t *testing.T) {
	t.Parallel()
	typo := strings.Replace(legacyCompanyYAML, "        handle: sre\n",
		"        handle: sre\n        gaol: ship\n", 1)
	file, _ := legacyFile(t, t.TempDir(), typo)
	_, err := resolveRunSeed(t, "", "-company", file)
	if err == nil || !strings.Contains(err.Error(), `"gaol" isn't a setting`) {
		t.Fatalf("a legacy file with a typo = %v, want it refused naming the typo", err)
	}
}

// assertStaleSaid fails a case whose log does not carry exactly one
// company_file_stale warning naming every retired key and the command that
// refreshes the file, or that also reports it as an ordinary ignored seed.
func assertStaleSaid(t *testing.T, logged, file string) {
	t.Helper()
	if n := strings.Count(logged, "msg=company_file_stale"); n != 1 {
		t.Errorf("company_file_stale was said %d times, want once:\n%s", n, logged)
	}
	for _, want := range append([]string{"crewlet config export > " + file}, legacyKeys...) {
		if !strings.Contains(logged, want) {
			t.Errorf("the stale file was not said with %q:\n%s", want, logged)
		}
	}
	if strings.Contains(logged, "company_seed_ignored") {
		t.Errorf("the stale file was also reported as an ignored seed:\n%s", logged)
	}
}

// fileRevisionsOf is how many revisions a -company file made in e's store.
func fileRevisionsOf(t *testing.T, e *engine.Engine) int {
	t.Helper()
	revisions, err := e.Backends().Store.Configs().List(t.Context(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	made := 0
	for _, r := range revisions {
		if r.Source == "file" {
			made++
		}
	}
	return made
}
