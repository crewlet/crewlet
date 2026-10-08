package main

import (
	"bytes"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// managedBootstrap writes a Tier A naming a store in dir and `operator` as
// the company document's only writer.
func managedBootstrap(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "managed.yaml")
	body := "node:\n  id: cli-test\nstore:\n  path: " + filepath.Join(dir, "index.db") +
		"\napi:\n  auth:\n    tokens:\n      - {id: operator, token: op-token}\n" +
		"    company_writers: [operator]\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// AN OFFLINE WRITE PRESENTS NO CREDENTIAL, so a managed document refuses it —
// an import and an activation alike — naming who manages it and the two ways
// forward, and storing nothing. Reading it is untouched.
func TestAManagedDocumentRefusesTheOfflineWrites(t *testing.T) {
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

// A -company BOOTSTRAP SEED IS IGNORED ON A MANAGED DEPLOYMENT, loudly: an
// empty store there waits for the managing system's first write, and a file
// the node was never told about (the ./company.yaml default) must not become
// the company in the meantime.
func TestABootstrapSeedIsIgnoredOnAManagedDeployment(t *testing.T) {
	t.Parallel()
	company := parse(t, companyYAML)
	var logged bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logged, nil))

	unmanaged := config.DefaultBootstrap()
	if got := managedSeed(t.Context(), &unmanaged, seedOf(company), log); got.Company == nil {
		t.Fatal("an unmanaged deployment dropped its seed")
	}
	if logged.Len() != 0 {
		t.Errorf("an unmanaged seed warned: %s", logged.String())
	}

	managed := config.DefaultBootstrap()
	managed.API.Auth.CompanyWriters = []string{"operator"}
	got := managedSeed(t.Context(), &managed, seedOf(company), log)
	if got.Company != nil || got.Override {
		t.Errorf("a managed deployment kept its seed: %+v", got)
	}
	if !strings.Contains(logged.String(), "company_seed_ignored") ||
		!strings.Contains(logged.String(), "operator") {
		t.Errorf("the ignored seed was not said, naming the writer: %s", logged.String())
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
