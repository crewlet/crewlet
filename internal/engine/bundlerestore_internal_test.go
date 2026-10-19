package engine

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent/cliprofile"
)

// BUILDING AN EPOCH WRITES NO LOGIN; RUNNING ONE RESTORES IT. [NewCompany] is
// what `crewlet validate` calls, so a configured credential bundle restored
// while the providers were constructed wrote a login onto whatever host
// validated the document. The restore is [Company.restoreBundles], which a
// node's boot and every apply make before the epoch is published.
func TestAnEpochRestoresItsBundlesOnlyWhenItIsRun(t *testing.T) {
	t.Setenv(config.CLIHomeEnv, t.TempDir())
	profile, err := cliprofile.Load("claude-code", nil)
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Base(profile.CredentialPaths[0])
	t.Setenv(cliagent.BundleVarName("subscription"), testBundle(t, name, `{"token":"configured"}`))

	c, err := config.ParseCompany([]byte(`
name: Acme
providers:
  llm:
    subscription:
      type: cli-agent
      model: sonnet
      cli:
        agent: claude-code
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
	stateDir, err := c.Providers.LLM["subscription"].CLI.ResolvedStateDir("subscription")
	if err != nil {
		t.Fatal(err)
	}
	login := filepath.Join(stateDir, "credentials", name)
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

// AN EPOCH DOES NOT UNDO A LOGOUT. The directory `crewlet llm logout` empties
// is exactly what a configured bundle restores into, so every apply and every
// boot after a logout put back the very login the operator had just removed —
// a compromised one, typically. The restore declines while the logout stands,
// and the epoch is still built and run: the entry runs signed out, as the
// operator asked — and says so as what it is, naming the logout and the way
// back, rather than as a bundle that failed to restore.
//
// NOT PARALLEL: it sets the conventional variable and swaps the process's log.
//
// Mutation: treat cliagent.ErrLoggedOut as a bundle to restore anyway, and the
// login is back here; warn it as a failed restore, and the warning is wrong.
func TestAnEpochDoesNotUndoALogout(t *testing.T) {
	logs := &bundleLog{}
	logging.Configure(slog.LevelInfo, logging.FormatJSON, logs)
	t.Cleanup(func() { logging.Configure(slog.LevelInfo, logging.FormatConsole, os.Stderr) })
	t.Setenv(config.CLIHomeEnv, t.TempDir())
	profile, err := cliprofile.Load("claude-code", nil)
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Base(profile.CredentialPaths[0])
	t.Setenv(cliagent.BundleVarName("subscription"), testBundle(t, name, `{"token":"configured"}`))
	c, err := config.ParseCompany([]byte(bundledCompanyDoc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	company, err := NewCompany(c)
	if err != nil {
		t.Fatalf("NewCompany: %v", err)
	}
	company.restoreBundles(t.Context())
	var provider *cliagent.Provider
	for key, p := range company.Models.All() {
		if cli, ok := p.(*cliagent.Provider); ok && key == "subscription" {
			provider = cli
		}
	}
	if provider == nil {
		t.Fatal("the premise: the epoch holds no cli-agent provider for the entry")
	}
	if !provider.Workspace().HasLogin() {
		t.Fatal("the premise: the epoch restored no login")
	}
	if err := provider.Logout(t.Context(), io.Discard, io.Discard); err != nil {
		t.Fatalf("Logout: %v", err)
	}

	next, err := NewCompany(c)
	if err != nil {
		t.Fatalf("NewCompany: %v", err)
	}
	next.restoreBundles(t.Context())
	if files := provider.Workspace().LoginFiles(); len(files) != 0 {
		t.Errorf("the next epoch restored the login a logout removed: %v", files)
	}
	got := logs.String()
	for _, want := range []string{"cli_agent_bundle_withheld", "crewlet llm logout", "crewlet llm login subscription"} {
		if !strings.Contains(got, want) {
			t.Errorf("the epoch's warning does not carry %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "cli_agent_bundle_restore_failed") {
		t.Errorf("a logout was warned as a bundle that failed to restore:\n%s", got)
	}
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
  llm:
    subscription:
      type: cli-agent
      model: sonnet
      cli:
        agent: claude-code
roles:
  - name: CEO
    handle: ceo
    llm: subscription
`

// testBundle packs one credential file the way `crewlet llm export` does.
func testBundle(t *testing.T, name, body string) string {
	t.Helper()
	var raw bytes.Buffer
	zw := gzip.NewWriter(&raw)
	tw := tar.NewWriter(zw)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(raw.Bytes())
}
