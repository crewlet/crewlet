package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A PROVISIONING RUN READS THE SECRET STORE, NOT JUST THE ENVIRONMENT.
//
// The chain used to be config.EnvOnly(), so every Tier B ${VAR} an operator
// had already put in the store resolved to the empty string. For a GitLab
// signing secret that is not merely a missing value — empty is the signal to
// MINT — so the run replaced a working webhook secret at the vendor with a
// fresh one and broke every delivery in flight until the config caught up.
func TestAProvisioningRunResolvesThroughTheSecretStore(t *testing.T) {
	cfg := bootstrapWithKeyring(t, "k1")
	if _, errs, err := secretsCmd(t, cfg, "set", "GITLAB_SIGNING_SECRET",
		"-value", "whsec_c3RvcmVkLXNpZ25pbmcta2V5LW9mLTMyLWJ5dGVzIQ=="); err != nil {
		t.Fatalf("seed the store: %v (%s)", err, errs)
	}

	var notes bytes.Buffer
	env, closeEnv, err := companyResolver(t.Context(), cfg, &notes)
	if err != nil {
		t.Fatalf("companyResolver: %v", err)
	}
	defer closeEnv()

	got := env.Value("${GITLAB_SIGNING_SECRET}")
	if got != "whsec_c3RvcmVkLXNpZ25pbmcta2V5LW9mLTMyLWJ5dGVzIQ==" {
		t.Fatalf("the stored secret did not reach the run: %q", got)
	}
	if notes.Len() > 0 {
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
	if _, errs, err := secretsCmd(t, cfg, "set", "PLANE_TOKEN",
		"-value", "the-rotated-one"); err != nil {
		t.Fatalf("seed the store: %v (%s)", err, errs)
	}
	t.Setenv("PLANE_TOKEN", "the-stale-export")

	env, closeEnv, err := companyResolver(t.Context(), cfg, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("companyResolver: %v", err)
	}
	defer closeEnv()

	if got := env.Lookup("PLANE_TOKEN"); got != "the-rotated-one" {
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
	env, closeEnv, err := companyResolver(t.Context(), missing, &notes)
	if err != nil {
		t.Fatalf("a missing bootstrap is a supported deployment: %v", err)
	}
	defer closeEnv()

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

// A KEYRINGLESS BOOTSTRAP IS ALSO ENVIRONMENT-ONLY, and also says so — the
// node has a store but nothing to decrypt it with, which is the same
// deployment from this command's point of view.
func TestABootstrapWithNoKeyringResolvesFromTheEnvironment(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("node:\n  id: cli-test\n"), 0o600); err != nil {
		t.Fatalf("write bootstrap: %v", err)
	}

	var notes bytes.Buffer
	if _, closeEnv, err := companyResolver(t.Context(), path, &notes); err != nil {
		t.Fatalf("a keyringless bootstrap is a supported deployment: %v", err)
	} else {
		defer closeEnv()
	}
	if !strings.Contains(notes.String(), "secrets.keys") {
		t.Errorf("the note does not say what is missing: %q", notes.String())
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
	if _, _, err := companyResolver(t.Context(), path, &notes); err == nil {
		t.Fatal("a bootstrap that cannot be read was treated as no bootstrap, " +
			"so a configured store silently became a stale environment")
	}
	if notes.Len() > 0 {
		t.Errorf("a failed run announced a fallback it did not take: %q", notes.String())
	}
}
