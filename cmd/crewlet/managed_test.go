package main

import (
	"bytes"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// managedBootstrap writes a Tier A naming a store in dir and `operator` as
// the company document's only writer — on the keyring and the stream
// [bootstrapForStore] writes, which every node requires, so a case can open
// the same store under both.
func managedBootstrap(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "managed.yaml")
	body := "node:\n  id: cli-test\nstore:\n  path: " + filepath.Join(dir, "index.db") +
		"\napi:\n  auth:\n    tokens:\n      - {id: operator, token: " + cliFixtureToken +
		", grants: [admin]}\n" +
		"    company_writers: [operator]\n" + fixtureStream(dir) + fixtureKeyring
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// AN OFFLINE WRITE PRESENTS NO CREDENTIAL, so a managed document refuses it —
// an import and an activation alike — naming who manages it and the two ways
// forward, and storing nothing. Reading it is untouched.
func TestAManagedDocumentRefusesTheOfflineWrites(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	open := bootstrapForStore(t, dir)
	company := companyFile(t, dir, "company.yaml", nil)
	if _, errs, err := configCmd(t, open, "import", company); err != nil {
		t.Fatalf("an unmanaged import: %v (%s)", err, errs)
	}
	listing, _, err := configCmd(t, open, "revisions")
	if err != nil {
		t.Fatal(err)
	}
	managed := managedBootstrap(t, dir)

	edited := companyFile(t, dir, "edited.yaml", func(doc string) string {
		return strings.Replace(doc, "name: Nimbus", "name: Nimbus Two", 1)
	})
	_, _, err = configCmd(t, managed, "import", edited)
	if err == nil {
		t.Fatal("an offline import of a managed document was accepted")
	}
	for _, want := range []string{"company_writers", "operator", "config import -api", apiTokenEnv} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}
	if after, _, err := configCmd(t, managed, "revisions"); err != nil || after != listing {
		t.Errorf("a refused import changed the history:\n%s\n%s", listing, after)
	}

	// RE-ACTIVATING THE ACTIVE REVISION CHANGES NOTHING in the company — the
	// offline re-publish a rotated credential needs — and stays open.
	id := firstRevisionID(t, listing)
	if _, _, err := configCmd(t, managed, "activate", id); err != nil {
		t.Errorf("re-activating the active revision of a managed document = %v, want it to land", err)
	}

	// ANY OTHER REVISION IS A CHANGE OF DOCUMENT, and is refused.
	if _, _, err := configCmd(t, open, "import", edited); err != nil {
		t.Fatalf("an unmanaged import: %v", err)
	}
	before, _, err := configCmd(t, managed, "revisions")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := configCmd(t, managed, "activate", id); err == nil ||
		!strings.Contains(err.Error(), "company_writers") {
		t.Errorf("an offline activate of an older revision of a managed document = %v, want the refusal", err)
	}
	if after, _, err := configCmd(t, managed, "revisions"); err != nil || after != before {
		t.Errorf("a refused activate changed the history:\n%s\n%s", before, after)
	}
	// A MISSING OR UNKNOWN ID IS TOLD SO, not that the document is managed:
	// there is no revision to judge.
	if _, _, err := configCmd(t, managed, "activate"); err == nil ||
		!strings.Contains(err.Error(), "needs a revision id") {
		t.Errorf("activate with no id on a managed document = %v, want the missing id named", err)
	}
	if _, _, err := configCmd(t, managed, "activate", "no-such-revision"); err == nil ||
		!strings.Contains(err.Error(), "no revision no-such-revision") {
		t.Errorf("activate of an unknown id on a managed document = %v, want it named", err)
	}
	// AND READING IS UNTOUCHED.
	shown, _, err := configCmd(t, managed, "show")
	if err != nil || !strings.Contains(shown, "Nimbus Two") {
		t.Errorf("show on a managed document = %q, %v; want the active company", shown, err)
	}
}

// firstRevisionID reads the one revision id out of a `config revisions` table.
func firstRevisionID(t *testing.T, table string) string {
	t.Helper()
	for _, line := range strings.Split(table, "\n")[1:] {
		if fields := strings.Fields(strings.TrimPrefix(line, "*")); len(fields) > 0 {
			return fields[0]
		}
	}
	t.Fatalf("no revision in %q", table)
	return ""
}

// -import-company IS AN INSTRUCTION TO REPLACE THE DOCUMENT, and on a managed
// deployment it is refused before the node starts rather than ignored — a
// node that started without doing it would read as one that had.
func TestImportCompanyIsRefusedOnAManagedDeployment(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	managed := managedBootstrap(t, dir)
	company := companyFile(t, dir, "company.yaml", nil)
	var out, errOut bytes.Buffer
	err := run([]string{"run", "-config", managed, "-import-company", company}, &out, &errOut)
	if err == nil {
		t.Fatal("-import-company on a managed deployment was accepted")
	}
	if !strings.Contains(err.Error(), "company_writers") || !strings.Contains(err.Error(), "-import-company") {
		t.Errorf("the refusal does not name the flag and the setting: %v", err)
	}
}

// A -company BOOTSTRAP SEED IS NEITHER IMPORTED NOR RUN ON A MANAGED
// DEPLOYMENT, and is not even read: an empty store there waits for the
// managing system's first write, and a file the node was never told about (the
// ./company.yaml default) must not become the company in the meantime — nor
// fail the boot of a node that ignores it because it no longer parses.
//
// It went through the import alone: the managed check dropped the seed for the
// import, after the engine had already booted on the file, so a managed node
// with an empty store ran the file's company and claimed its seats while
// logging that the file is never imported. Resolved as the run resolves it and
// decided as the run decides it, the engine is handed no company, nothing is
// imported, and the ignored file is said, naming the writer.
func TestABootstrapSeedIsNeitherImportedNorRunOnAManagedDeployment(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	managed := managedBootstrap(t, dir)
	// A FILE THAT DOES NOT PARSE, at the default path: read, it would refuse
	// the boot of a node that ignores it.
	stray := filepath.Join(dir, "company.yaml")
	if err := os.WriteFile(stray, []byte("name: [unterminated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resolve := func(t *testing.T, path string, named bool) (*config.Bootstrap, tierBSeed) {
		t.Helper()
		flags := flag.NewFlagSet("run", flag.ContinueOnError)
		cfg := addConfigFlags(flags)
		args := []string{"-config", managed}
		if named {
			args = append(args, "-company", path)
		}
		if err := flags.Parse(args); err != nil {
			t.Fatal(err)
		}
		// THE DEFAULT, standing where the working directory's would.
		*cfg.company = path
		boot, seed, err := cfg.loadForRun(t.Context(), flags, "")
		if err != nil {
			t.Fatalf("a managed node refused a -company file it ignores: %v", err)
		}
		return boot, seed
	}

	for _, named := range []bool{false, true} {
		boot, seed := resolve(t, stray, named)
		if seed.Company != nil || !seed.Managed || seed.Path != stray {
			t.Fatalf("a managed node's seed (named %v) resolved to %+v, want the file "+
				"named and never read", named, seed)
		}
		var logged bytes.Buffer
		plan, err := decideOn(t, boot, seed, managed,
			slog.New(slog.NewTextHandler(&logged, nil)))
		if err != nil {
			t.Fatal(err)
		}
		opts, err := bootOptions(boot, plan)
		if err != nil {
			t.Fatal(err)
		}
		if opts.Company != nil || plan.act != seedNone {
			t.Errorf("a managed node with an empty store boots on %v to %q, want no "+
				"company and nothing imported", companyLabel(opts.Company), plan.act)
		}
		if !strings.Contains(logged.String(), "company_seed_ignored") ||
			!strings.Contains(logged.String(), "operator") ||
			!strings.Contains(logged.String(), stray) {
			t.Errorf("the ignored seed was not said, naming the file and the writer: %s",
				logged.String())
		}
	}

	// AN ABSENT DEFAULT IS NO SEED, and nothing to say.
	boot, seed := resolve(t, filepath.Join(dir, "absent.yaml"), false)
	if seed.Managed || seed.Company != nil {
		t.Errorf("an absent default on a managed node resolved to %+v, want no seed", seed)
	}
	var logged bytes.Buffer
	if _, err := decideOn(t, boot, seed, managed,
		slog.New(slog.NewTextHandler(&logged, nil))); err != nil {
		t.Fatal(err)
	}
	if logged.Len() != 0 {
		t.Errorf("a managed node with no seed warned: %s", logged.String())
	}

	// AND A STORE THAT HOLDS A COMPANY BOOTS ON IT, as with no file at all.
	serveStored(t, managed, runnableRevisionDoc)
	boot, seed = resolve(t, stray, false)
	plan, err := decideOn(t, boot, seed, managed, quiet())
	if err != nil {
		t.Fatal(err)
	}
	if plan.company == nil || plan.company.Name != "Nimbus" || plan.activatedAt.IsZero() {
		t.Errorf("a managed node over a stored company boots on %v at %s, want the "+
			"stored revision with its instant", companyLabel(plan.company), plan.activatedAt)
	}
}

// A MANAGED NODE OVER A REVISION THIS BUILD CANNOT RUN NAMES THE ONE WAY OUT
// THAT CLEARS THE REFUSAL, and says nothing else.
//
// The refusal first named an offline `crewlet config import`, which a managed
// deployment refuses, so the operator was sent to a second refusal; then the
// managing system's write through a serving node, which moves the fleet and
// never this node's store — the only thing this boot reads — so every restart
// was refused the same way. And the ignored-seed warning had already been said
// before the stored revision was read, so it stood beside the refusal that
// stopped the boot.
//
// So the case FOLLOWS THE REMEDY the refusal names — the list removed from
// this node's Tier A, a corrected document imported offline, the node started,
// the list restored — and holds the boot to deciding on the corrected company
// at both starts.
//
// Mutations: name the offline import alone, or the write through a serving
// node, and the remedy case fails; warn before reading the store and the
// silence case does; refuse the offline import whatever Tier A says and the
// remedy is never followed.
func TestAManagedNodeOverARevisionItCannotRunNamesTheManagedRemedy(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	managed := managedBootstrap(t, dir)
	id := serveStored(t, managed, storedRevisionDoc)
	boot, err := config.LoadBootstrap(managed, config.EnvOnly())
	if err != nil {
		t.Fatal(err)
	}
	seed, err := managedSeed(boot, companyFile(t, dir, "company.yaml", nil), false, "")
	if err != nil {
		t.Fatal(err)
	}

	var logged bytes.Buffer
	_, err = decideOn(t, boot, seed, managed,
		slog.New(slog.NewTextHandler(&logged, nil)))
	if err == nil {
		t.Fatal("a managed node booted onto a revision this build cannot run")
	}
	for _, want := range []string{id, "company_writers", "operator",
		"start the build that wrote it", "remove api.auth.company_writers",
		"`crewlet config import`", "only while the fleet still runs " + id,
		"restore api.auth.company_writers", "nonexistent"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}
	for _, dead := range []string{"-import-company", "-api <node>", "config activate"} {
		if strings.Contains(err.Error(), dead) {
			t.Errorf("the refusal names %s, which does not clear it: %v", dead, err)
		}
	}
	if strings.Contains(logged.String(), "company_seed_ignored") {
		t.Errorf("the ignored seed was said beside the refusal: %s", logged.String())
	}

	// THE REMEDY, as the refusal states it. The same store under a Tier A
	// that names no writers takes an offline import ...
	taken := bootstrapForStore(t, dir)
	corrected := companyFile(t, dir, "corrected.yaml", func(doc string) string {
		return strings.Replace(doc, "name: Nimbus", "name: Nimbus Corrected", 1)
	})
	if _, errs, err := configCmd(t, taken, "import", corrected); err != nil {
		t.Fatalf("the offline import the refusal names: %v (%s)", err, errs)
	}
	// ... the node starts on it ...
	takenBoot, err := config.LoadBootstrap(taken, config.EnvOnly())
	if err != nil {
		t.Fatal(err)
	}
	plan, err := decideOn(t, takenBoot, tierBSeed{}, taken, quiet())
	if err != nil || plan.company == nil || plan.company.Name != "Nimbus Corrected" {
		t.Fatalf("a start after the remedy decided (%v, %v), want the corrected company",
			companyLabel(plan.company), err)
	}
	// ... and with the writers restored it starts on it again.
	plan, err = decideOn(t, boot, seed, managed, quiet())
	if err != nil || plan.company == nil || plan.company.Name != "Nimbus Corrected" {
		t.Errorf("a managed restart after the remedy decided (%v, %v), want the "+
			"corrected company", companyLabel(plan.company), err)
	}
}

// AND AN IMPORT THROUGH A NODE THAT REFUSES THE TOKEN AS NOT A WRITER says
// who manages the document, what to do, and which token this command sent.
func TestAManagedRefusalThroughTheAPISaysWhichTokenWasSent(t *testing.T) {
	t.Parallel()
	client := answering(t, http.StatusForbidden, []byte(`{"error":"config_managed",`+
		`"detail":"the company document is managed by operator","managed_by":["operator"],`+
		`"hint":"change the company where it is managed"}`))
	_, _, err := client.Import(t.Context(), []byte("name: Acme\n"), "import")
	if err == nil {
		t.Fatal("a managed refusal was reported as a write")
	}
	for _, want := range []string{"managed by operator", "where it is managed", apiTokenEnv} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q does not say %q", err, want)
		}
	}
}
