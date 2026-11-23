package engine

import (
	"bytes"
	"context"
	"go/ast"
	"go/token"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/configplane"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/providers/llm/harness"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
)

// BUILDING AN EPOCH WRITES NO LOGIN; RUNNING ONE RESTORES IT. [NewCompany] is
// what `crewlet validate` calls, so a restore there would write a login onto
// whatever host validated the document. The restore is
// [Company.restoreBundles], made by a node's boot and every apply before the
// epoch is published: the cases below hold those call sites, this the helper.
func TestAnEpochRestoresItsBundlesOnlyWhenItIsRun(t *testing.T) {
	t.Setenv(config.HarnessHomeEnv, t.TempDir())
	profile, err := harnessprofile.Load("claude-code", nil)
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Base(profile.CredentialPaths[0])
	t.Setenv(harnessprofile.BundleVarName("subscription"), testBundle(t, "claude-code", name, `{"token":"configured"}`))

	c, err := config.ParseCompany([]byte(`
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    subscription:
      model: sonnet
      harness: claude-code
roles:
  - name: CEO
    handle: ceo
    llm: subscription
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	company, err := NewCompany(c)
	if err != nil {
		t.Fatalf("NewCompany: %v", err)
	}
	entry := c.Providers.LLM["subscription"]
	stateDir, err := entry.ResolvedStateDir("subscription")
	if err != nil {
		t.Fatal(err)
	}
	login := filepath.Join(stateDir, "claude-code", "credentials", name)
	if _, err := os.Stat(login); !os.IsNotExist(err) {
		t.Fatalf("building the epoch wrote the bundle's login (%v) — validating a document "+
			"would write onto the host", err)
	}

	company.restoreBundles(t.Context())
	got, err := os.ReadFile(login) //nolint:gosec // the test's own state directory
	if err != nil || string(got) != `{"token":"configured"}` {
		t.Fatalf("restoreBundles left %q (%v), want the configured bundle's login", got, err)
	}
}

// A BOOT RESTORES THE CONFIGURED BUNDLE, through [New] itself: a container
// rebuilt on every deploy comes up on an empty state volume, and the bundle
// (here the conventional variable `crewlet llm export -secret-store` writes)
// signs it in before its first turn. A boot that lost the call comes up
// signed out with every test of the helper still green.
//
// NOT PARALLEL: it sets the variable the bundle is read from.
//
// Mutation: drop the restore from New, and this is red.
func TestABootRestoresTheConfiguredBundle(t *testing.T) {
	login := bundledLogin(t)
	e := bundleNode(t, parseBundled(t, bundledCompanyDoc))
	if e.Company() == nil {
		t.Fatal("the premise: the node booted with no company")
	}
	if got, err := os.ReadFile(login); err != nil || string(got) != `{"token":"configured"}` { //nolint:gosec // the test's own state directory
		t.Errorf("the boot left %q (%v), want the configured bundle's login", got, err)
	}
}

// AN APPLY RESTORES THE CONFIGURED BUNDLE into a credentials directory found
// empty, and a revision the apply REFUSES restores nothing: the directory is
// the running epoch's too, so a bundle a refused revision wrote would sign its
// seats in as an account no published revision names — and outlive every
// retry, since the next revision's restore finds a login present and keeps it.
//
// NOT PARALLEL: it sets the variable the bundle is read from, and PATH.
//
// Mutation: drop the restore from Apply, and the first half is red; make it
// before the stages that can still refuse the revision, and the second is.
func TestAnApplyRestoresTheBundleOnlyForARevisionItPublishes(t *testing.T) {
	login := bundledLogin(t)
	e := bundleNode(t, nil)

	path := withNoContainerRuntime(t)
	status, stages, err := e.Apply(t.Context(), parseBundled(t, refusedBundledCompanyDoc), time.Now())
	if err == nil || status != configplane.StatusError {
		t.Fatalf("the premise: Apply = (%s, %v, %v), want the revision refused", status, stages, err)
	}
	if _, statErr := os.Stat(login); !os.IsNotExist(statErr) {
		t.Fatalf("a refused revision restored its bundle (%v) into the directory the running "+
			"epoch's seats share", statErr)
	}
	t.Setenv("PATH", path)

	status, stages, err = e.Apply(t.Context(), parseBundled(t, bundledCompanyDoc), time.Now())
	if err != nil || status != configplane.StatusOK {
		t.Fatalf("Apply = (%s, %v, %v), want ok", status, stages, err)
	}
	if got, err := os.ReadFile(login); err != nil || string(got) != `{"token":"configured"}` { //nolint:gosec // the test's own state directory
		t.Fatalf("the apply left %q (%v), want the configured bundle's login", got, err)
	}

	// Emptied under a running node, and the next apply restores it again.
	if err := os.Remove(login); err != nil {
		t.Fatal(err)
	}
	if status, stages, err := e.Apply(t.Context(), parseBundled(t, bundledCompanyDoc), time.Now()); err != nil || status != configplane.StatusOK {
		t.Fatalf("Apply = (%s, %v, %v), want ok", status, stages, err)
	}
	if got, err := os.ReadFile(login); err != nil || string(got) != `{"token":"configured"}` { //nolint:gosec // the test's own state directory
		t.Errorf("the next apply left %q (%v), want the bundle restored into the emptied directory", got, err)
	}
}

// THE BUNDLE IS IN PLACE BEFORE THE EPOCH IS CURRENT, and on an apply only once
// nothing can refuse the revision. A turn can start the moment the pointer
// moves, so a later restore lets the epoch's first call run signed out; one
// before a stage that can still refuse writes a login for a revision never
// published. Apply's reflection workers, serving the running seats from their
// swap, are swapped in only after the restore too. No order is visible from
// outside a running node, so the calls are read out of the source.
//
// Mutation: move the restore below installEpoch in New or in Apply, or above
// any `return configplane.StatusError` in Apply, or move Apply's
// installReflection above the restore or below installEpoch, and this is red.
func TestTheBundleIsRestoredBeforeTheEpochIsCurrent(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"New", "Apply"} {
		fn := engineFunction(t, name)
		restore, install := firstCall(fn, "restoreBundles"), firstCall(fn, "installEpoch")
		if restore == token.NoPos || install == token.NoPos {
			t.Fatalf("%s: found restoreBundles at %v and installEpoch at %v — both must be called", name, restore, install)
		}
		if restore > install {
			t.Errorf("%s restores the bundles after the epoch is current, so its first call runs signed out", name)
		}
		if name != "Apply" {
			continue
		}
		switch swap := firstCall(fn, "installReflection"); {
		case swap == token.NoPos:
			t.Error("Apply never swaps the revision's reflection workers in")
		case swap < restore:
			t.Error("Apply swaps the revision's reflection workers in before its bundles are restored, " +
				"so a pass on the revision's models runs signed out")
		case swap > install:
			t.Error("Apply swaps the revision's reflection workers in after the epoch is current, so " +
				"the epoch's seats reflect on the previous revision's org and models meanwhile")
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			ret, ok := n.(*ast.ReturnStmt)
			if !ok || len(ret.Results) == 0 {
				return true
			}
			if sel, ok := ret.Results[0].(*ast.SelectorExpr); ok && sel.Sel.Name == "StatusError" && ret.Pos() > restore {
				t.Errorf("Apply can still refuse the revision after restoring its bundles (a StatusError " +
					"return follows the restore), so a refused revision leaves a login behind")
			}
			return true
		})
	}
}

// engineFunction is the function or *Engine method of this package with the
// given name.
func engineFunction(t *testing.T, name string) *ast.FuncDecl {
	t.Helper()
	for _, file := range enginePackage(t) {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if ok && fn.Name.Name == name && (fn.Recv == nil || receiverIsEngine(fn)) {
				return fn
			}
		}
	}
	t.Fatalf("no function %s in the engine package", name)
	return nil
}

// firstCall is where fn first calls a method, or a function value, of the
// given name, or NoPos.
func firstCall(fn *ast.FuncDecl, name string) token.Pos {
	at := token.NoPos
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || at != token.NoPos {
			return at == token.NoPos
		}
		switch callee := call.Fun.(type) {
		case *ast.SelectorExpr:
			if callee.Sel.Name == name {
				at = call.Pos()
			}
		case *ast.Ident:
			if callee.Name == name {
				at = call.Pos()
			}
		}
		return true
	})
	return at
}

// bundledLogin points the harness state directories at a fresh one, sets the
// conventional bundle variable of the "subscription" entry to a bundle holding
// one claude-code login, and returns where that login lands.
func bundledLogin(t *testing.T) string {
	t.Helper()
	t.Setenv(config.HarnessHomeEnv, t.TempDir())
	profile, err := harnessprofile.Load("claude-code", nil)
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Base(profile.CredentialPaths[0])
	t.Setenv(harnessprofile.BundleVarName("subscription"), testBundle(t, "claude-code", name, `{"token":"configured"}`))
	c := parseBundled(t, bundledCompanyDoc)
	entry := c.Providers.LLM["subscription"]
	stateDir, err := entry.ResolvedStateDir("subscription")
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(stateDir, "claude-code", "credentials", name)
}

// bundleNode boots an engine over a fresh store on company, nil for none.
func bundleNode(t *testing.T, company *config.Company) *Engine {
	t.Helper()
	b := testBootstrap(t)
	dir := brokertest.TempDir(t)
	b.Store.Path = filepath.Join(dir, "crewlet.db")
	brokertest.StoreIn(&b.Stream, filepath.Join(dir, "stream"))
	SeedStore(t, &b)
	e, err := New(t.Context(), Options{Bootstrap: &b, Company: company})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })
	return e
}

func parseBundled(t *testing.T, doc string) *config.Company {
	t.Helper()
	c, err := config.ParseCompany([]byte(doc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return c
}

// refusedBundledCompanyDoc is [bundledCompanyDoc]'s bundled entry with a
// sandbox catalogue that builds no backend under [withNoContainerRuntime] — its
// container cell's runtime is on no PATH — which the apply refuses after the
// company itself has been built. The seat runs on an OpenCode entry, which
// runs as root too, so the refusal is the catalogue's under any user.
const refusedBundledCompanyDoc = `
name: Acme
providers:
  llm:
    subscription:
      model: sonnet
      harness: claude-code
    boxed:
      model: anthropic/claude-sonnet-5
      harness: opencode
      run_in: container
  sandbox:
    local: {image: example.invalid/box:1, runtime: docker}
roles:
  - name: CEO
    handle: ceo
    llm: boxed
`

// AN EPOCH DOES NOT UNDO A LOGOUT. `crewlet llm logout` empties the directory
// a configured bundle restores into, so the restore declines while the logout
// stands rather than put back the login the operator removed; the entry runs
// signed out, warned as the logout with the way back, not a failed restore.
//
// NOT PARALLEL: it sets the conventional variable and PATH, and swaps the
// process's log. A `claude` that only exits 0 goes first on PATH for the
// vendor's `claude auth logout`: the engine's rule is under test, not the
// machine's binary.
//
// Mutation: treat harness.ErrLoggedOut as a bundle to restore anyway, and the
// login is back here; warn it as a failed restore, and the warning is wrong.
func TestAnEpochDoesNotUndoALogout(t *testing.T) {
	logs := &bundleLog{}
	logging.Configure(slog.LevelInfo, logging.FormatJSON, logs)
	t.Cleanup(func() { logging.Configure(slog.LevelInfo, logging.FormatConsole, os.Stderr) })
	t.Setenv(config.HarnessHomeEnv, t.TempDir())
	stubVendorHarness(t, "claude")
	profile, err := harnessprofile.Load("claude-code", nil)
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Base(profile.CredentialPaths[0])
	t.Setenv(harnessprofile.BundleVarName("subscription"), testBundle(t, "claude-code", name, `{"token":"configured"}`))
	c, err := config.ParseCompany([]byte(bundledCompanyDoc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	company, err := NewCompany(c)
	if err != nil {
		t.Fatalf("NewCompany: %v", err)
	}
	company.restoreBundles(t.Context())
	var provider *harness.Provider
	for key, p := range company.Models.All() {
		if cli, ok := p.(*harness.Provider); ok && key == "subscription" {
			provider = cli
		}
	}
	if provider == nil {
		t.Fatal("the premise: the epoch holds no harness provider for the entry")
	}
	if has, err := provider.Workspace().HasLogin(t.Context()); err != nil || !has {
		t.Fatalf("the premise: the epoch restored no login (%v)", err)
	}
	if err := provider.Logout(t.Context(), io.Discard, io.Discard); err != nil {
		t.Fatalf("Logout: %v", err)
	}

	next, err := NewCompany(c)
	if err != nil {
		t.Fatalf("NewCompany: %v", err)
	}
	next.restoreBundles(t.Context())
	if login, err := provider.Workspace().Login(t.Context()); err != nil || len(login) != 0 {
		t.Errorf("the next epoch restored the login a logout removed: %d files (%v)", len(login), err)
	}
	got := logs.String()
	for _, want := range []string{"harness_bundle_withheld", "crewlet llm logout", "crewlet llm login subscription"} {
		if !strings.Contains(got, want) {
			t.Errorf("the epoch's warning does not carry %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "harness_bundle_restore_failed") {
		t.Errorf("a logout was warned as a bundle that failed to restore:\n%s", got)
	}
}

// AN ENTRY MOVED TO ANOTHER HARNESS DOES NOT RESTORE THE OLD HARNESS'S BUNDLE,
// even where both keep their login under one file name (pi's and codex's
// `auth.json`). The bundle is keyed by the entry, so after the move codex's
// empty login would get pi's credential, which codex cannot read and which,
// as a login present, keeps every later restore out. The epoch warns it as a
// failed bundle naming the harness it was exported from, and writes nothing.
//
// NOT PARALLEL: it sets the conventional variable and swaps the process's log.
//
// Mutation: drop the manifest's check from the restore, and codex's login is
// pi's file here.
func TestAnEntryMovedToAnotherHarnessDoesNotRestoreTheOldHarnessesBundle(t *testing.T) {
	logs := &bundleLog{}
	logging.Configure(slog.LevelInfo, logging.FormatJSON, logs)
	t.Cleanup(func() { logging.Configure(slog.LevelInfo, logging.FormatConsole, os.Stderr) })
	t.Setenv(config.HarnessHomeEnv, t.TempDir())
	t.Setenv(harnessprofile.BundleVarName("subscription"), testBundle(t, "pi", "auth.json", `{"token":"pi"}`))
	c := parseBundled(t, `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    subscription:
      model: gpt-5
      harness: codex
roles:
  # A person's seat: the bundle restored is the entry's whoever runs on it.
  - name: CEO
    handle: ceo
    kind: human
`)
	company, err := NewCompany(c)
	if err != nil {
		t.Fatalf("NewCompany: %v", err)
	}
	company.restoreBundles(t.Context())

	entry := c.Providers.LLM["subscription"]
	stateDir, err := entry.ResolvedStateDir("subscription")
	if err != nil {
		t.Fatal(err)
	}
	credentials := filepath.Join(stateDir, "codex", "credentials")
	if entries, err := os.ReadDir(credentials); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	} else if len(entries) != 0 {
		t.Errorf("the epoch restored pi's bundle into codex's login: %v", entries)
	}
	got := logs.String()
	for _, want := range []string{"harness_bundle_restore_failed", `it names \"pi\"`,
		"crewlet llm export subscription"} {
		if !strings.Contains(got, want) {
			t.Errorf("the epoch's warning does not carry %q:\n%s", want, got)
		}
	}
}

// stubVendorHarness puts an executable named name, which only exits 0, first on
// PATH for the rest of the test.
func stubVendorHarness(t *testing.T, name string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil { //nolint:gosec // the test's own stub
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// withNoContainerRuntime points PATH at an empty directory, so a container
// cell's runtime resolves nowhere and its catalogue cannot be built
// ([sandbox.ResolveContainerRuntime]), and answers the PATH it replaced. Not
// for a parallel test.
func withNoContainerRuntime(t *testing.T) string {
	t.Helper()
	path := os.Getenv("PATH")
	t.Setenv("PATH", t.TempDir())
	return path
}

// bundleLog is a log sink the logger may write to from any goroutine.
type bundleLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *bundleLog) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *bundleLog) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// bundledCompanyDoc is a company whose one seat runs on a claude-code entry
// with no bundle named, so the conventional variable is the one it reads.
const bundledCompanyDoc = `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    subscription:
      model: sonnet
      harness: claude-code
roles:
  - name: CEO
    handle: ceo
    llm: subscription
`

// testBundle is a bundle of agent's holding one credential file, exported by
// `crewlet llm export`'s own writer from a state directory of its own — so it
// names the harness it came from exactly as an export does.
func testBundle(t *testing.T, agent, name, body string) string {
	t.Helper()
	p, err := harness.New(harness.Config{
		Key: "export", Harness: agent, StateDir: t.TempDir(), Timeout: time.Second, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatalf("building the exporting %s entry: %v", agent, err)
	}
	login := filepath.Join(p.Workspace().CredentialsDir(), name)
	if err := os.MkdirAll(filepath.Dir(login), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(login, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	blob, err := p.ExportBundle(t.Context())
	if err != nil {
		t.Fatalf("ExportBundle: %v", err)
	}
	return blob
}
