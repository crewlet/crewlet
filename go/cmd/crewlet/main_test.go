package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const companyYAML = `
name: Acme
providers:
  llm:
    primary:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["${ANTHROPIC_API_KEY}"]
roles:
  - name: CEO
    handle: ceo
    llm: primary
  - name: CTO
    handle: cto
    llm: primary
`

// configPair writes both tiers into a temp directory and returns the flags
// that point at them.
func configPair(t *testing.T, bootstrapYAML, company string) []string {
	t.Helper()
	dir := t.TempDir()
	boot := filepath.Join(dir, "crewlet.yaml")
	comp := filepath.Join(dir, "company.yaml")
	if bootstrapYAML == "" {
		bootstrapYAML = "store:\n  path: " + filepath.Join(dir, "crewlet.db") + "\n" +
			"stream:\n  store_dir: " + filepath.Join(dir, "stream") + "\n"
	}
	if err := os.WriteFile(boot, []byte(bootstrapYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(comp, []byte(company), 0o600); err != nil {
		t.Fatal(err)
	}
	return []string{"-config", boot, "-company", comp}
}

func TestValidateReportsWhatTheConfigDescribes(t *testing.T) {
	t.Parallel()
	// Validate exists so a config can be checked without starting
	// anything, which means it must reach nothing: no broker, no store, no
	// provider. What it prints is the summary an operator uses to confirm
	// they edited the file they meant to.
	var out, errOut bytes.Buffer
	args := append([]string{"validate"}, configPair(t, "", companyYAML)...)
	if err := run(args, &out, &errOut); err != nil {
		t.Fatalf("validate: %v (stderr %s)", err, errOut.String())
	}
	got := out.String()
	for _, want := range []string{"Acme", "2 agent seats", "1 LLM providers", "embedded", "local"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary %q does not mention %q", got, want)
		}
	}
}

func TestValidateCatchesWhatASchemaCannot(t *testing.T) {
	t.Parallel()
	// The reason validate builds the epoch rather than just parsing: a
	// seat whose llm names no configured provider is well-formed YAML and
	// a valid document. It fails at the first turn, which is the worst
	// place to learn it.
	bad := strings.Replace(companyYAML, "llm: primary\n", "llm: nonexistent\n", 1)
	var out, errOut bytes.Buffer
	args := append([]string{"validate"}, configPair(t, "", bad)...)
	err := run(args, &out, &errOut)
	if err == nil {
		t.Fatal("a role naming an unconfigured provider validated")
	}
	if !strings.Contains(err.Error(), "nonexistent") {
		t.Errorf("the error does not name the provider: %v", err)
	}
}

func TestBothTiersAreReportedTogether(t *testing.T) {
	t.Parallel()
	// An operator fixing a broker URL only to be told about their org
	// chart on the next boot has been made to pay twice for one edit. It
	// is the rule each tier's own validator already follows internally.
	dir := t.TempDir()
	boot := filepath.Join(dir, "crewlet.yaml")
	comp := filepath.Join(dir, "company.yaml")
	if err := os.WriteFile(boot, []byte("stream:\n  type: kafka\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(comp, []byte("name: \"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	err := run([]string{"validate", "-config", boot, "-company", comp}, &out, &errOut)
	if err == nil {
		t.Fatal("two broken tiers validated")
	}
	msg := err.Error()
	if !strings.Contains(msg, "kafka") {
		t.Errorf("the bootstrap problem is missing: %v", err)
	}
	if !strings.Contains(msg, "name") {
		t.Errorf("the company problem is missing, so only the first tier was read: %v", err)
	}
}

func TestAMissingConfigNamesTheFileNotTheField(t *testing.T) {
	t.Parallel()
	// The default paths are relative, so a first run in the wrong
	// directory is the ordinary way to reach this. It must say which file
	// it could not find.
	var out, errOut bytes.Buffer
	err := run([]string{"validate",
		"-config", filepath.Join(t.TempDir(), "nope.yaml"),
		"-company", filepath.Join(t.TempDir(), "alsonope.yaml"),
	}, &out, &errOut)
	if err == nil {
		t.Fatal("a missing config validated")
	}
	if !strings.Contains(err.Error(), "nope.yaml") {
		t.Errorf("the error does not name the file: %v", err)
	}
}

func TestUsageNamesBothTiersAndTheirDefaults(t *testing.T) {
	t.Parallel()
	// The two tiers are the one thing a new operator has to understand
	// before anything else works, and the flag names alone do not say
	// which is which.
	var out bytes.Buffer
	if err := run([]string{"help"}, &out, &out); err == nil {
		t.Fatal("help returned no sentinel, so main would not treat it as help")
	}
	got := out.String()
	for _, want := range []string{"-config", "-company", "crewlet.yaml", "company.yaml", "validate"} {
		if !strings.Contains(got, want) {
			t.Errorf("usage does not mention %q:\n%s", want, got)
		}
	}
}

func TestAnUnknownCommandIsRefusedWithUsage(t *testing.T) {
	t.Parallel()
	var out, errOut bytes.Buffer
	err := run([]string{"strt"}, &out, &errOut)
	if err == nil {
		t.Fatal("an unknown command was accepted")
	}
	if !strings.Contains(err.Error(), "strt") {
		t.Errorf("the error does not name the command: %v", err)
	}
	if !strings.Contains(errOut.String(), "Usage:") {
		t.Error("an unknown command printed no usage")
	}
}

func TestNoCommandIsRefused(t *testing.T) {
	t.Parallel()
	var out, errOut bytes.Buffer
	if err := run(nil, &out, &errOut); err == nil {
		t.Error("an empty command line was accepted")
	}
}

func TestVersionPrints(t *testing.T) {
	t.Parallel()
	var out, errOut bytes.Buffer
	if err := run([]string{"version"}, &out, &errOut); err != nil {
		t.Fatalf("version: %v", err)
	}
	if !strings.HasPrefix(out.String(), "crewlet ") {
		t.Errorf("version output = %q", out.String())
	}
}

func TestRunRefusesABadConfigBeforeStartingAnything(t *testing.T) {
	t.Parallel()
	// A node that boots on a bad config and discovers it at the first turn
	// has already told its peers it owns seats. This is the same check
	// validate makes, on the path that matters.
	bad := strings.Replace(companyYAML, "llm: primary\n", "llm: nonexistent\n", 1)
	var errOut bytes.Buffer
	args := append([]string{"run"}, configPair(t, "", bad)...)
	if err := run(args, &bytes.Buffer{}, &errOut); err == nil {
		t.Fatal("run started on a company whose seat names no provider")
	}
}
