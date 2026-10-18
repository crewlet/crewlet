package engine

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
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
