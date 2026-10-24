package main

import (
	"bytes"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// resolveCompany is one run's resolution of the GitLab fixture company, as a
// provisioning command makes it, through the store the Tier A at cfg names.
func resolveCompany(t *testing.T, cfg string, notes io.Writer) (*companyValues, error) {
	t.Helper()
	fs := flag.NewFlagSet("provision", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	at := addStoreFlags(fs)
	if err := fs.Parse([]string{"-config", cfg}); err != nil {
		t.Fatal(err)
	}
	company, err := config.LoadCompanyToRun(gitlabCompanyFile(t))
	if err != nil {
		t.Fatal(err)
	}
	companySecrets, err := openCompanyStore(noEnviron, at, "crewlet gitlab provision")
	if err != nil {
		return nil, err
	}
	t.Cleanup(companySecrets.Close)
	return companySecrets.values(t.Context(), company, notes)
}

// A PROVISIONING RUN READS THE SECRET STORE, NOT JUST THE ENVIRONMENT.
//
// The chain used to be config.EnvOnly(), so every Tier B ${VAR} an operator
// had already put in the store resolved to the empty string. For a GitLab
// signing secret that is not merely a missing value. Empty is the signal to
// MINT, so the run replaced a working webhook secret at the third-party app
// with a fresh one and broke every delivery in flight until the config
// caught up.
func TestAProvisioningRunResolvesThroughTheSecretStore(t *testing.T) {
	t.Parallel()
	cfg := bootstrapWithKeyring(t, "k1")
	if _, errs, err := secretsCmd(t, cfg, "set", "GITLAB_SIGNING_SECRET",
		"-value", "whsec_c3RvcmVkLXNpZ25pbmcta2V5LW9mLTMyLWJ5dGVzIQ=="); err != nil {
		t.Fatalf("seed the store: %v (%s)", err, errs)
	}

	var notes bytes.Buffer
	env, err := resolveCompany(t, cfg, &notes)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	got := env.Value("${GITLAB_SIGNING_SECRET}")
	if got != "whsec_c3RvcmVkLXNpZ25pbmcta2V5LW9mLTMyLWJ5dGVzIQ==" {
		t.Fatalf("the stored secret did not reach the run: %q", got)
	}
	// A STOPPED NODE'S OWN ROWS ARE NOT THE FLEET'S, and the run says it
	// read the first: an operator reading the plan has to know a value
	// those rows lack is unknown rather than unset.
	if !strings.Contains(notes.String(), "own secret rows") {
		t.Errorf("a run that read one node's rows did not say so: %q", notes.String())
	}
	if strings.Contains(notes.String(), "environment only") {
		t.Errorf("a run that DID read the store announced that it did not: %q",
			notes.String())
	}
}

// STORE FIRST, ENVIRONMENT BEHIND — the same order the engine resolves in.
//
// A rotated secret must win over a stale export, which is the whole reason
// the store exists: rotation is an update of one row, and an environment
// that could shadow it would make the rotation appear to work and change
// nothing.
func TestAStoredSecretWinsOverAStaleExport(t *testing.T) {
	cfg := bootstrapWithKeyring(t, "k1")
	if _, errs, err := secretsCmd(t, cfg, "set", "GITLAB_TOKEN_SWE",
		"-value", "the-rotated-one"); err != nil {
		t.Fatalf("seed the store: %v (%s)", err, errs)
	}
	t.Setenv("GITLAB_TOKEN_SWE", "the-stale-export")

	env, err := resolveCompany(t, cfg, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if got := env.Lookup("GITLAB_TOKEN_SWE"); got != "the-rotated-one" {
		t.Fatalf("a stale export shadowed the rotated secret: %q", got)
	}
}

// THE OPERATOR'S OWN CREDENTIAL IS NOT THE COMPANY'S, and does not come
// from the store.
//
// A GitLab admin PAT carries `api` scope over the whole group, and the store
// is replicated to every node holding the keyring. Crewlet never persists
// one; reading one back would imply it may be kept there — which is how the
// most powerful credential in the deployment ends up in the shared table
// beside the seat tokens it exists to mint.
func TestAnOperatorCredentialIsNotReadFromTheStore(t *testing.T) {
	cfg := bootstrapWithKeyring(t, "k1")
	if _, errs, err := secretsCmd(t, cfg, "set", "GITLAB_ADMIN_TOKEN",
		"-value", "stored-admin-pat"); err != nil {
		t.Fatalf("seed the store: %v (%s)", err, errs)
	}
	if got := operatorCredential("GITLAB_ADMIN_TOKEN"); got != "" {
		t.Fatalf("an operator credential was read out of the secret store: %q", got)
	}
	t.Setenv("GITLAB_ADMIN_TOKEN", "  exported-admin-pat  ")
	if got := operatorCredential("GITLAB_ADMIN_TOKEN"); got != "exported-admin-pat" {
		t.Errorf("the exported credential did not reach the run: %q", got)
	}
}

// NO STORE IS A SUPPORTED DEPLOYMENT, AND IT SAYS SO.
//
// Environment-only resolution is the pre-store shape and must keep working.
// It must not be SILENT, though: a mistyped -config resolving nothing has
// exactly the destructive outcome above, and the only way an operator can
// tell the two apart is the run saying which chain it ran.
func TestWithoutAStoreTheRunSaysWhichChainItUsed(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not-here.yaml")

	var notes bytes.Buffer
	env, err := resolveCompany(t, missing, &notes)
	if err != nil {
		t.Fatalf("a missing bootstrap is a supported deployment: %v", err)
	}

	t.Setenv("SOME_TOKEN", "from-the-environment")
	if got := env.Lookup("SOME_TOKEN"); got != "from-the-environment" {
		t.Errorf("environment-only resolution stopped working: %q", got)
	}
	if !strings.Contains(notes.String(), missing) {
		t.Errorf("the note does not name the path that was not there: %q", notes.String())
	}
	if !strings.Contains(notes.String(), "environment only") {
		t.Errorf("the note does not say which chain ran: %q", notes.String())
	}
}

// A KEYRINGLESS BOOTSTRAP IS REFUSED, NOT RESOLVED FROM THE ENVIRONMENT.
//
// It used to be the second environment-only case, beside a missing file: a
// node with a store and nothing to decrypt it with. Every node needs a keyring
// now, so a Tier A without one is a file no node can run, and resolving a
// company's values from whatever this shell holds on the strength of it would
// be the stale-export shadowing this chain exists to prevent. The refusal
// names the field, and the environment note is never written.
func TestABootstrapWithNoKeyringIsRefused(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("node:\n  id: cli-test\n"), 0o600); err != nil {
		t.Fatalf("write bootstrap: %v", err)
	}

	var notes bytes.Buffer
	_, err := resolveCompany(t, path, &notes)
	if err == nil {
		t.Fatal("a keyringless bootstrap was resolved rather than refused")
	}
	if !strings.Contains(err.Error(), "secrets.keys") {
		t.Errorf("the refusal does not name the keyring: %v", err)
	}
	if strings.Contains(notes.String(), "environment only") {
		t.Errorf("a refused bootstrap still announced an environment-only run: %q",
			notes.String())
	}
}

// A BROKEN BOOTSTRAP FAILS THE RUN RATHER THAN FALLING BACK.
//
// Falling back is the stale-export shadowing this chain exists to prevent,
// and it would happen at the worst moment: an operator who configured a
// store and did not get it would have every secret resolved from whatever
// their shell happened to hold.
func TestABrokenBootstrapRefusesRatherThanResolvingFromTheEnvironment(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("store:\n  path: [not, a, string]\n"), 0o600); err != nil {
		t.Fatalf("write bootstrap: %v", err)
	}

	var notes bytes.Buffer
	if _, err := resolveCompany(t, path, &notes); err == nil {
		t.Fatal("a bootstrap that cannot be read was treated as no bootstrap, " +
			"so a configured store silently became a stale environment")
	}
	if notes.Len() > 0 {
		t.Errorf("a failed run announced a fallback it did not take: %q", notes.String())
	}
}
