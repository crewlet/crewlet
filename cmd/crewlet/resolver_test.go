package main

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A PROVISIONING RUN READS THE FLEET'S SECRET STORE, THROUGH THE RUNNING NODE.
//
// A run that saw only the environment would resolve EMPTY for every Tier B
// ${VAR} an operator has put in the store — and for a webhook signing secret
// empty is the signal to MINT, which replaces a working secret at the
// third-party app. The node's store is read whole, in one revealing request,
// before anything resolves.
//
// Mutation: resolve from the environment alone once a node is reached, and
// the stored secret does not arrive.
func TestAProvisioningRunResolvesThroughTheRunningNode(t *testing.T) {
	t.Parallel()
	const stored = "whsec_c3RvcmVkLXNpZ25pbmcta2V5LW9mLTMyLWJ5dGVzIQ=="
	node := newFakeSecretsNode(t)
	node.body = `{"values":{"GITLAB_SIGNING_SECRET":"` + stored + `"}}`

	var notes bytes.Buffer
	env, closeEnv, err := resolveThrough(t.Context(), node.client(t), nil, &notes)
	if err != nil {
		t.Fatalf("resolveThrough: %v", err)
	}
	defer closeEnv()

	if got := env.Value("${GITLAB_SIGNING_SECRET}"); got != stored {
		t.Fatalf("the fleet's secret did not reach the run: %q", got)
	}
	if node.last.method != http.MethodGet || node.last.path != "/secrets" ||
		node.last.query != "reveal=true" {
		t.Errorf("the node was asked %s %s?%s, want the one revealing read of "+
			"every value", node.last.method, node.last.path, node.last.query)
	}
	if notes.Len() > 0 {
		t.Errorf("a run that DID read the fleet announced that it did not: %q",
			notes.String())
	}
}

// THE FLEET FIRST, ENVIRONMENT BEHIND — the same order the engine resolves in.
//
// A rotated secret must win over a stale export, which is the whole reason
// the store exists: rotation is an update of one row, and an environment
// that could shadow it would make the rotation appear to work and change
// nothing.
func TestAFleetSecretWinsOverAStaleExport(t *testing.T) {
	node := newFakeSecretsNode(t)
	node.body = `{"values":{"CONFLUENCE_TOKEN":"the-rotated-one"}}`
	t.Setenv("CONFLUENCE_TOKEN", "the-stale-export")

	env, closeEnv, err := resolveThrough(t.Context(), node.client(t), nil, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("resolveThrough: %v", err)
	}
	defer closeEnv()

	if got := env.Lookup("CONFLUENCE_TOKEN"); got != "the-rotated-one" {
		t.Fatalf("a stale export shadowed the rotated secret: %q", got)
	}
}

// A FLEET THAT WAS REACHED AND COULD NOT BE READ FAILS THE RUN.
//
// Resolving on from the environment would be the stale-export shadowing this
// chain exists to prevent, and every stored credential would read as unset —
// the case in which a run mints over the one that works.
//
// Mutation: fall back to the environment on a failed read, and the run
// resolves instead of failing.
func TestAFleetThatCannotBeReadFailsTheRun(t *testing.T) {
	t.Parallel()
	node := newFakeSecretsNode(t)
	node.status = http.StatusInternalServerError
	node.body = `{"error":"internal_error"}`

	var notes bytes.Buffer
	if _, _, err := resolveThrough(t.Context(), node.client(t), nil, &notes); err == nil {
		t.Fatal("a node that could not answer its secrets was resolved around")
	}
	if notes.Len() > 0 {
		t.Errorf("a failed run announced a fallback it did not take: %q", notes.String())
	}
}

// WITH NO ENGINE RUNNING, THIS NODE'S OWN TABLE IS NOT READ AS THE FLEET'S.
//
// It holds only rows written while the engine was stopped, and the engine
// moves them onto the fleet and deletes them at every start — so read as the
// store it answers "unset" for every credential the fleet holds. The run
// resolves from the environment and says why, and what to do.
//
// Mutation: open this node's table and resolve from it, and the locally
// written row resolves.
func TestWithNoEngineRunningTheLocalTableIsNotTheFleet(t *testing.T) {
	cfg := bootstrapWithKeyring(t, "k1")
	if _, errs, err := secretsCmd(t, cfg, "set", "GITLAB_SIGNING_SECRET",
		"-value", "whsec_bG9jYWwtb25seS1zaWduaW5nLWtleS0zMi1ieXRlcyE="); err != nil {
		t.Fatalf("write a local row: %v (%s)", err, errs)
	}
	t.Setenv("GITLAB_SIGNING_SECRET", "")

	var notes bytes.Buffer
	env, closeEnv, err := companyResolver(t.Context(), cfg, &notes)
	if err != nil {
		t.Fatalf("companyResolver: %v", err)
	}
	defer closeEnv()

	if got := env.Value("${GITLAB_SIGNING_SECRET}"); got != "" {
		t.Errorf("a row in this node's own table resolved as the fleet's: %q", got)
	}
	for _, want := range []string{
		"no engine is running", // why the fleet is out of reach
		"environment only",     // which chain ran
		"crewlet run",          // what to do about it
	} {
		if !strings.Contains(notes.String(), want) {
			t.Errorf("the note omits %q: %q", want, notes.String())
		}
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

// THE PREVIOUS ENGINE'S OPERATOR-CREDENTIAL NAMES STILL WORK.
//
// `--provision-token` / $GITLAB_PROVISION_TOKEN was renamed to
// `-admin-token` / $*_ADMIN_TOKEN with no alias and no migration note. An operator whose CI exports the old name got "no
// administrator token" from a pipeline that had worked the day before, and
// the error named only the new spelling — so the message actively pointed
// away from the cause.
//
// The new name WINS when both are set: it is the one this engine documents,
// and an operator who has migrated should not have a stale export silently
// override the value they just wrote.
func TestTheRenamedOperatorCredentialsStillRead(t *testing.T) {
	for _, tc := range []struct {
		name  string
		set   map[string]string
		names []string
		want  string
	}{
		{
			name:  "the current name",
			set:   map[string]string{"GITLAB_ADMIN_TOKEN": "current"},
			names: []string{"GITLAB_ADMIN_TOKEN", "GITLAB_PROVISION_TOKEN"},
			want:  "current",
		},
		{
			name:  "the previous engine's name alone",
			set:   map[string]string{"GITLAB_PROVISION_TOKEN": "legacy"},
			names: []string{"GITLAB_ADMIN_TOKEN", "GITLAB_PROVISION_TOKEN"},
			want:  "legacy",
		},
		{
			name: "both, and the current one wins",
			set: map[string]string{
				"GITLAB_ADMIN_TOKEN":     "current",
				"GITLAB_PROVISION_TOKEN": "legacy",
			},
			names: []string{"GITLAB_ADMIN_TOKEN", "GITLAB_PROVISION_TOKEN"},
			want:  "current",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.set {
				t.Setenv(k, v)
			}
			if got := operatorCredential(tc.names...); got != tc.want {
				t.Errorf("operatorCredential(%v) = %q, want %q", tc.names, got, tc.want)
			}
		})
	}
}
