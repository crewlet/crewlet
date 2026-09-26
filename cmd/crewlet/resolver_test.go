package main

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/provision"
)

// A PROVISIONING RUN READS THE FLEET'S SECRET STORE, THROUGH A RUNNING NODE.
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
	fleet, err := resolveThrough(t.Context(), "crewlet.yaml", node.client(t), nil, &notes)
	if err != nil {
		t.Fatalf("resolveThrough: %v", err)
	}

	if got := fleet.env.Value("${GITLAB_SIGNING_SECRET}"); got != stored {
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

	fleet, err := resolveThrough(t.Context(), "crewlet.yaml", node.client(t), nil, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("resolveThrough: %v", err)
	}

	if got := fleet.env.Lookup("CONFLUENCE_TOKEN"); got != "the-rotated-one" {
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
	if _, err := resolveThrough(t.Context(), "crewlet.yaml", node.client(t), nil, &notes); err == nil {
		t.Fatal("a node that could not answer its secrets was resolved around")
	}
	if notes.Len() > 0 {
		t.Errorf("a failed run announced a fallback it did not take: %q", notes.String())
	}
}

// bootstrapBesideANode writes a Tier A document with a keyring whose API
// names node, and the store file it declares.
func bootstrapBesideANode(t *testing.T, node *fakeSecretsNode) (cfg, storePath string) {
	t.Helper()
	dir := t.TempDir()
	storePath = filepath.Join(dir, "index.db")
	served, err := url.Parse(node.server.URL)
	if err != nil {
		t.Fatalf("parse the node's address: %v", err)
	}
	doc := fmt.Sprintf("node:\n  id: cli-test\nstore:\n  path: %s\n"+
		"api:\n  host: %s\n  port: %s\n  auth:\n    tokens:\n"+
		"      - id: ops\n        token: ops-token\n"+
		"secrets:\n  active_key_id: k1\n  keys:\n    - id: k1\n"+
		"      material: %q\n",
		storePath, served.Hostname(), served.Port(),
		base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32)))
	cfg = filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfg, []byte(doc), 0o600); err != nil {
		t.Fatalf("write bootstrap: %v", err)
	}
	if err := os.WriteFile(storePath, nil, 0o600); err != nil {
		t.Fatalf("write the store file: %v", err)
	}
	return cfg, storePath
}

// holdTheStore takes the lock a running engine holds on the store file at
// path, for the life of the test.
//
// ON A DESCRIPTOR OF ITS OWN, which is what makes a store open in this process
// refused as another process's would be: the lock is taken per open file, and
// the store shares its own handle's lock only with the opens it made itself.
func holdTheStore(t *testing.T, path string) {
	t.Helper()
	file, err := os.OpenFile(path+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatalf("open the lock file: %v", err)
	}
	t.Cleanup(func() { _ = file.Close() })
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatalf("hold the store's lock: %v", err)
	}
}

// A STORE THE ENGINE HOLDS IS READ THROUGH THAT ENGINE.
//
// The lock on the store file is how a command knows an engine runs on this
// host, and the engine's API is then the one way to the fleet's store: the
// resolver asks it for every value, with the bearer token Tier A names, and
// resolves the company from what it answers.
//
// Mutation: answer a locked store as no engine running here, or resolve from
// the environment once the node is found, and the fleet's value does not
// arrive.
func TestTheResolverReadsTheFleetThroughTheEngineHoldingTheStore(t *testing.T) {
	t.Parallel()
	node := newFakeSecretsNode(t)
	node.body = `{"values":{"JIRA_ORG_TOKEN":"from-the-fleet"}}`
	cfg, storePath := bootstrapBesideANode(t, node)
	holdTheStore(t, storePath)

	var notes bytes.Buffer
	fleet, err := companyResolver(t.Context(), cfg, "", &notes)
	if err != nil {
		t.Fatalf("companyResolver: %v", err)
	}
	if got := fleet.env.Value("${JIRA_ORG_TOKEN}"); got != "from-the-fleet" {
		t.Fatalf("the engine's value did not reach the run: %q (notes %q)",
			got, notes.String())
	}
	if node.last.method != http.MethodGet || node.last.path != "/secrets" ||
		node.last.query != "reveal=true" || node.last.auth != "Bearer ops-token" {
		t.Errorf("the engine was asked %+v, want GET /secrets?reveal=true with "+
			"Tier A's token", node.last)
	}
	if fleet.node == nil || fleet.unread != nil || fleet.absent != "" {
		t.Errorf("the run does not record the node it read through: %+v", fleet)
	}
	if notes.Len() > 0 {
		t.Errorf("a run that read the fleet announced that it did not: %q", notes.String())
	}
}

// A NODE NAMED WITH -api IS READ, WHERE NO ENGINE RUNS HERE.
//
// The flag is how a command reaches the fleet from a machine that is not a
// node, and it is the node a -secret-store run writes through: a run that
// wrote through it and resolved from the environment would say the fleet
// could not be read while it held the node that could answer.
//
// Mutation: resolve without the named node, and this run resolves from the
// environment and says the fleet is out of reach.
func TestANodeNamedWithAPIIsReadWhereNoEngineRuns(t *testing.T) {
	t.Setenv(apiTokenEnv, "ops-token")
	node := newFakeSecretsNode(t)
	node.body = `{"values":{"JIRA_ORG_TOKEN":"from-the-named-node"}}`
	for name, cfg := range map[string]string{
		"a keyring and no engine here": bootstrapWithKeyring(t, "k1"),
		"no Tier A on this machine":    filepath.Join(t.TempDir(), "absent.yaml"),
	} {
		var notes bytes.Buffer
		fleet, err := companyResolver(t.Context(), cfg, node.server.URL, &notes)
		if err != nil {
			t.Fatalf("%s: companyResolver: %v", name, err)
		}
		if got := fleet.env.Value("${JIRA_ORG_TOKEN}"); got != "from-the-named-node" {
			t.Errorf("%s: the named node's value did not reach the run: %q", name, got)
		}
		if fleet.recordable() != nil || notes.Len() > 0 {
			t.Errorf("%s: a run that read the named node says it could not: "+
				"%v %q", name, fleet.recordable(), notes.String())
		}
	}
}

// WITH NO ENGINE RUNNING, THIS NODE'S OWN TABLE IS NOT READ AS THE FLEET'S.
//
// It holds only rows written while the engine was stopped, which the engine
// moves onto the fleet at its next start — so read as the store it answers
// "unset" for every credential the fleet holds. The run resolves from the
// environment and says why, and what to do; and it knows the fleet was not
// read, which is what refuses it anything it would record.
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
	fleet, err := companyResolver(t.Context(), cfg, "", &notes)
	if err != nil {
		t.Fatalf("companyResolver: %v", err)
	}

	if got := fleet.env.Value("${GITLAB_SIGNING_SECRET}"); got != "" {
		t.Errorf("a row in this node's own table resolved as the fleet's: %q", got)
	}
	for _, want := range []string{
		"no engine is running", // why the fleet is out of reach
		"environment only",     // which chain ran
		"resolves empty",       // what that does to a value the fleet holds
		"crewlet run",          // what to do about it
		"-api",                 // the other way to reach it
	} {
		if !strings.Contains(notes.String(), want) {
			t.Errorf("the note omits %q: %q", want, notes.String())
		}
	}
	if !errors.Is(fleet.recordable(), errFleetUnread) {
		t.Errorf("a run that could not read the fleet may record: %v", fleet.recordable())
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
	fleet, err := companyResolver(t.Context(), missing, "", &notes)
	if err != nil {
		t.Fatalf("a missing bootstrap is a supported deployment: %v", err)
	}

	t.Setenv("SOME_TOKEN", "from-the-environment")
	if got := fleet.env.Lookup("SOME_TOKEN"); got != "from-the-environment" {
		t.Errorf("environment-only resolution stopped working: %q", got)
	}
	if !strings.Contains(notes.String(), missing) {
		t.Errorf("the note does not name the path that was not there: %q", notes.String())
	}
	if !strings.Contains(notes.String(), "environment only") {
		t.Errorf("the note does not say which chain ran: %q", notes.String())
	}
	if fleet.recordable() != nil {
		t.Errorf("a deployment with no store may not record: %v", fleet.recordable())
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
	if _, err := companyResolver(t.Context(), path, "", &notes); err != nil {
		t.Fatalf("a keyringless bootstrap is a supported deployment: %v", err)
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
	if _, err := companyResolver(t.Context(), path, "", &notes); err == nil {
		t.Fatal("a bootstrap that cannot be read was treated as no bootstrap, " +
			"so a configured store silently became a stale environment")
	}
	if notes.Len() > 0 {
		t.Errorf("a failed run announced a fallback it did not take: %q", notes.String())
	}
}

// openEnvFileSink opens a -env-file sink over what a run read of the fleet.
func openEnvFileSink(t *testing.T, path string, fleet *fleetRead) provision.TokenSink {
	t.Helper()
	sinks := sinkFlags{
		secretStore: new(bool), envFile: &path, print: new(bool),
		bootstrap: new(string), api: new(string),
	}
	sink, err := sinks.open(&bytes.Buffer{}, fleet)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return sink
}

// A FILE IS NOT THE FLEET'S STORE, AND EVERY NODE READS THE STORE FIRST.
//
// So a file sink on a run that read the fleet answers the fleet's value for a
// name it holds — a pass that asked the file alone would find nothing, mint,
// and replace the working credential at the third-party app — and refuses to
// record under such a name, because no node would ever read what it wrote.
// A name the fleet does not hold is the file's, both ways.
//
// Mutation: hand back the file sink itself, or answer the file before the
// fleet, and the held credential reads as absent and records.
func TestAFileSinkAnswersTheFleetFirstAndRefusesWhatItHolds(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "minted.env")
	sink := openEnvFileSink(t, path, &fleetRead{
		env:  config.EnvOnly(),
		held: map[string]string{"GITLAB_TOKEN_SWE": " glpat-the-fleets \n"},
	})

	value, held, err := sink.Value(t.Context(), "GITLAB_TOKEN_SWE")
	if err != nil || !held || value != "glpat-the-fleets" {
		t.Fatalf("Value = %q, %v, %v; want the fleet's credential, trimmed", value, held, err)
	}
	err = sink.Record(t.Context(), "GITLAB_TOKEN_SWE", "glpat-minted")
	if !errors.Is(err, errFleetHolds) || !strings.Contains(err.Error(), "-secret-store") {
		t.Fatalf("a name the fleet holds recorded into a file: %v", err)
	}
	if written, _ := os.ReadFile(path); strings.Contains(string(written), "glpat-minted") {
		t.Errorf("the refused credential reached the file: %q", written)
	}

	if _, held, err := sink.Value(t.Context(), "GITLAB_TOKEN_PM"); err != nil || held {
		t.Errorf("a name the fleet does not hold read as held: %v %v", held, err)
	}
	if err := sink.Record(t.Context(), "GITLAB_TOKEN_PM", "glpat-new"); err != nil {
		t.Fatalf("a name the fleet does not hold was refused: %v", err)
	}
	if value, held, _ := sink.Value(t.Context(), "GITLAB_TOKEN_PM"); !held || value != "glpat-new" {
		t.Errorf("the file lost what it recorded: %q %v", value, held)
	}

	// AND A TEARDOWN IS TOLD WHAT IT LEFT BEHIND: the file forgets both,
	// and the fleet's row is still sealed and still resolving.
	err = sink.Forget(t.Context(), "GITLAB_TOKEN_SWE", "GITLAB_TOKEN_PM")
	if !errors.Is(err, errFleetHolds) || !strings.Contains(err.Error(), "GITLAB_TOKEN_SWE") ||
		strings.Contains(err.Error(), "GITLAB_TOKEN_PM") {
		t.Errorf("Forget answered %v, want the one name the fleet still holds", err)
	}
}

// A RUN THAT COULD NOT READ THE FLEET IS REFUSED BEFORE A VALUE IT MAY HOLD
// IS CHECKED — AND A RUN THAT RECORDS NOTHING IS TOLD THAT CAUSE.
//
// With no engine to read through, a token only the fleet holds resolves
// empty, and a check of it refused the run as though the operator had never
// set it: they went looking for a value that was already in the store. A real
// run is refused for the store before any such check; a dry run, which
// records nothing, still refuses over the empty token, and says the store may
// hold it.
//
// Mutation: drop the refusal before the checks, or the unread clause from the
// token's refusal, and one of the two fails.
func TestARefusalOverAnEmptyValueNamesTheUnreadFleet(t *testing.T) {
	t.Setenv("JIRA_ORG_TOKEN", "")
	company := filepath.Join(t.TempDir(), "company.yaml")
	if err := os.WriteFile(company, []byte(jiraCompanyDoc), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg := bootstrapWithKeyring(t, "k1")
	jira := func(args ...string) error {
		t.Helper()
		var out, errs bytes.Buffer
		return run(append([]string{"jira", "provision", company, "-config", cfg},
			args...), &out, &errs)
	}

	err := jira("-print")
	if !errors.Is(err, errFleetUnread) || strings.Contains(err.Error(), "resolved empty") {
		t.Errorf("a real run answered %v, want the unread store and nothing about "+
			"an empty token", err)
	}
	err = jira("-dry-run")
	if err == nil || !strings.Contains(err.Error(), "resolved empty") ||
		!strings.Contains(err.Error(), errFleetUnread.Error()) {
		t.Errorf("a dry run answered %v, want the empty token and the store that "+
			"may hold it", err)
	}
}

// jiraCompanyDoc is a company whose Jira org token only a secret store holds.
const jiraCompanyDoc = `
name: Nimbus
providers:
  llm:
    main:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["${ANTHROPIC_API_KEY}"]
integrations:
  jira:
    url: https://jira.example.com
    token: "${JIRA_ORG_TOKEN}"
    webhook_secret: "${JIRA_WEBHOOK_SECRET}"
roles:
  - name: SWE
    handle: swe
    llm: main
`
