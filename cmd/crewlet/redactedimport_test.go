package main

import (
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// inlineKey is the credential a company that has not adopted the secret store
// writes into its document, and every read of the document masks.
const inlineKey = "sk-ant-api03-inline-fixture"

// storeWithInlineKey is a node's store holding, active, a company whose
// model key is written inline, and the redacted read of it — what `crewlet
// config show`, `GET /config` and the dashboard's Copy as YAML hand out.
func storeWithInlineKey(t *testing.T, dir string) (cfg, redacted string) {
	t.Helper()
	cfg = bootstrapForStore(t, dir)
	literal := companyFile(t, dir, "literal.yaml", func(doc string) string {
		return strings.Replace(doc, `"${ANTHROPIC_API_KEY}"`, `"`+inlineKey+`"`, 1)
	})
	if _, errs, err := configCmd(t, cfg, "import", literal); err != nil {
		t.Fatalf("import: %v (%s)", err, errs)
	}
	shown, _, err := configCmd(t, cfg, "show")
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	if !strings.Contains(shown, config.Redacted) || strings.Contains(shown, inlineKey) {
		t.Fatalf("the read is not the redacted one this case is about:\n%s", shown)
	}
	return cfg, shown
}

// A REDACTED READ IMPORTS OVER THE COMPANY IT WAS READ FROM.
//
// A company.yaml kept in sync from Copy as YAML or `GET /config` carries a
// mask for every inline credential, and `crewlet config import` is how it is
// written back. The import's own check ran before anything could restore a
// mask, so every such copy was refused for the marker, in a field nobody had
// touched — the round trip `PUT /config` makes work, and the guide promised.
// The offline import restores each mask from the revision it parents on: an
// unchanged copy is the active revision again and imports nothing, and an
// edited one is stored with the credential it hid rather than the marker.
func TestARedactedReadImportsOverTheCompanyItWasReadFrom(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg, redacted := storeWithInlineKey(t, dir)

	unchanged := filepath.Join(dir, "copy.yaml")
	if err := os.WriteFile(unchanged, []byte(redacted), 0o600); err != nil {
		t.Fatal(err)
	}
	out, errs, err := configCmd(t, cfg, "import", unchanged)
	if err != nil {
		t.Fatalf("an unchanged redacted copy was refused: %v (%s)", err, errs)
	}
	if !strings.Contains(out, "nothing imported") {
		t.Errorf("an unchanged redacted copy is the active revision again, and the "+
			"import said %q", out)
	}

	edited := filepath.Join(dir, "edited.yaml")
	if err := os.WriteFile(edited, []byte(strings.Replace(redacted,
		"name: Nimbus", "name: Nimbus Two", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, errs, err := configCmd(t, cfg, "import", edited); err != nil {
		t.Fatalf("an edited redacted copy was refused: %v (%s)", err, errs)
	}
	stored, _, err := configCmd(t, cfg, "export")
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if !strings.Contains(stored, "Nimbus Two") {
		t.Fatalf("the edit was not stored:\n%s", stored)
	}
	if !strings.Contains(stored, inlineKey) || strings.Contains(stored, config.Redacted) {
		t.Errorf("the stored revision does not carry the credential the copy masked:\n%s", stored)
	}
}

// A MASK NO REVISION ANSWERS IS REFUSED, NAMING ITS FIELD, and nothing is
// stored. With no active revision there is nothing to restore from, and an
// entry renamed in the copy has no prior value of its own; storing either
// would hand the CLI the literal marker as its key, found hours later as a
// sign-in failure that names nothing about where it came from.
func TestAMaskNoRevisionAnswersIsRefusedNamingItsField(t *testing.T) {
	t.Parallel()

	t.Run("an empty store", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		cfg := bootstrapForStore(t, dir)
		masked := companyFile(t, dir, "masked.yaml", func(doc string) string {
			return strings.Replace(doc, `"${ANTHROPIC_API_KEY}"`, `"`+config.Redacted+`"`, 1)
		})
		_, _, err := configCmd(t, cfg, "import", masked)
		if err == nil || !strings.Contains(err.Error(), "providers.llm.main.auth.api_key") {
			t.Fatalf("a mask with nothing to restore it from = %v, want a refusal naming its field", err)
		}
		if out, _, err := configCmd(t, cfg, "revisions"); err != nil || !strings.Contains(out, "no revisions") {
			t.Fatalf("the refused import left something behind: %q (%v)", out, err)
		}
	})

	t.Run("an entry renamed in the copy", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		cfg, redacted := storeWithInlineKey(t, dir)
		before := bootRevisionID(t, cfg)
		renamed := strings.NewReplacer("main:", "primary:", "llm: main", "llm: primary",
			"- main", "- primary").Replace(redacted)
		path := filepath.Join(dir, "renamed.yaml")
		if err := os.WriteFile(path, []byte(renamed), 0o600); err != nil {
			t.Fatal(err)
		}
		_, _, err := configCmd(t, cfg, "import", path)
		if err == nil || !strings.Contains(err.Error(), "providers.llm.primary.auth.api_key") {
			t.Fatalf("a renamed entry's mask = %v, want a refusal naming its field", err)
		}
		if after := bootRevisionID(t, cfg); after != before {
			t.Errorf("the refused import moved the active revision from %s to %s", before, after)
		}
	})
}

// THROUGH A NODE THE NODE RESTORES IT. The API route is `PUT /config`, which
// restores each mask from the revision it replaces before it validates, so
// the redacted copy travels as it is: a check here first would hold it to the
// rules before any restore and refuse it for its masks.
func TestARedactedCopyIsSentToTheNodeToRestore(t *testing.T) {
	dir := t.TempDir()
	cfg := bootstrapForStore(t, dir)
	masked := companyFile(t, dir, "masked.yaml", func(doc string) string {
		return strings.Replace(doc, `"${ANTHROPIC_API_KEY}"`, `"`+config.Redacted+`"`, 1)
	})
	var (
		mu   sync.Mutex
		sent []byte
	)
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		sent = body
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"revision_id":"rev-restored","epoch":3}`)
	}))
	t.Cleanup(node.Close)
	t.Setenv(apiTokenEnv, "a-tier-a-token")

	if _, errs, err := configCmd(t, cfg, "import", masked, "-api", node.URL); err != nil {
		t.Fatalf("a redacted copy was refused before it reached the node: %v (%s)", err, errs)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(string(sent), config.Redacted) {
		t.Errorf("the node was not sent the copy as it is, masks and all:\n%s", sent)
	}
}

// A REDACTED COMPANY FILE STARTS THE NODE IT WAS READ FROM.
//
// `crewlet run` boots on its company file and may import it, so a
// company.yaml kept in sync from a redacted read is restored against this
// node's active revision as the import restores one — for the override and
// for the bootstrap seed alike — rather than refused for the masks, or, worse,
// booted with the marker as a credential.
func TestARedactedSeedIsRestoredFromThisNodesActiveRevision(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg, redacted := storeWithInlineKey(t, dir)
	copyPath := filepath.Join(dir, "copy.yaml")
	if err := os.WriteFile(copyPath, []byte(redacted), 0o600); err != nil {
		t.Fatal(err)
	}

	for name, tc := range map[string]struct {
		args       []string
		importPath string
	}{
		"-import-company": {args: []string{"-config", cfg}, importPath: copyPath},
		"-company":        {args: []string{"-config", cfg, "-company", copyPath}},
	} {
		t.Run(name, func(t *testing.T) {
			flags := flag.NewFlagSet("run", flag.ContinueOnError)
			flags.SetOutput(io.Discard)
			c := addConfigFlags(flags)
			if err := flags.Parse(tc.args); err != nil {
				t.Fatal(err)
			}
			_, seed, err := c.loadForRun(t.Context(), flags, tc.importPath)
			if err != nil {
				t.Fatalf("a redacted company file did not load: %v", err)
			}
			if got := seed.Company.Providers.LLM["main"].Auth.APIKey; got != inlineKey {
				t.Errorf("the seed's key is %q, not the credential the file masked", got)
			}
		})
	}
}

// AND WITH NOTHING TO RESTORE FROM, A NODE REFUSES TO START ON THE MASK rather
// than boot a CLI on the literal marker.
func TestARedactedSeedOnAnEmptyStoreIsRefusedNamingItsField(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := bootstrapForStore(t, dir)
	masked := companyFile(t, dir, "masked.yaml", func(doc string) string {
		return strings.Replace(doc, `"${ANTHROPIC_API_KEY}"`, `"`+config.Redacted+`"`, 1)
	})
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	c := addConfigFlags(flags)
	if err := flags.Parse([]string{"-config", cfg, "-company", masked}); err != nil {
		t.Fatal(err)
	}
	_, _, err := c.loadForRun(t.Context(), flags, "")
	if err == nil || !strings.Contains(err.Error(), "providers.llm.main.auth.api_key") {
		t.Fatalf("a masked seed over an empty store = %v, want a refusal naming its field", err)
	}
}
