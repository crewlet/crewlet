package engine

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/configplane"
	"github.com/crewlet/crewlet/internal/logging"
)

// cursorCompanyDoc is a company whose one seat runs on a cursor-agent entry.
const cursorCompanyDoc = `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    cursor:
      model: gpt-5
      harness: cursor-agent
roles:
  - name: CEO
    handle: ceo
    llm: cursor
`

// AN EPOCH WARNS OF INSTRUCTIONS ABOVE A CURSOR ENTRY'S STATE DIRECTORY, in
// the words each refused call uses, naming the file: Cursor reads every
// AGENTS.md above its call into the request, so no call of the entry runs
// there. Warned, not refused: the company boots, as it does for an entry with
// no credentials.
//
// NOT PARALLEL: it sets the harness home and swaps the process's log.
//
// Mutation: warn nothing, or warn without the file: this goes red.
func TestAnEpochWarnsOfInstructionsAboveACursorEntry(t *testing.T) {
	logs := &bundleLog{}
	logging.Configure(slog.LevelInfo, logging.FormatJSON, logs)
	t.Cleanup(func() { logging.Configure(slog.LevelInfo, logging.FormatConsole, os.Stderr) })
	above := t.TempDir()
	planted := filepath.Join(above, "AGENTS.md")
	if err := os.WriteFile(planted, []byte("INSTRUCTIONS FROM ABOVE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.HarnessHomeEnv, filepath.Join(above, "harness-home"))
	c, err := config.ParseCompany([]byte(cursorCompanyDoc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	company, err := NewCompany(c)
	if err != nil {
		t.Fatalf("NewCompany refused the company: %v", err)
	}
	company.warnAncestorFiles(t.Context())
	got := logs.String()
	if !strings.Contains(got, "harness_ancestor_files") || !strings.Contains(got, planted) ||
		!strings.Contains(got, "set this entry's state_dir") {
		t.Errorf("the epoch warned %q, want harness_ancestor_files naming %s", got, planted)
	}
}

// A BOOT AND AN APPLY EACH WARN OF INSTRUCTIONS ABOVE A CURSOR ENTRY, through
// [New] and [Engine.Apply] themselves, naming the file: a node that lost
// either call would come up, or take a revision, with every Cursor call
// refused and nothing said until the first one.
//
// NOT PARALLEL: it sets the harness home and swaps the process's log.
//
// Mutation: drop the warning's call from New, and the boot half is red; from
// Apply, and the apply half is.
func TestABootAndAnApplyWarnOfInstructionsAboveACursorEntry(t *testing.T) {
	logs := &bundleLog{}
	logging.Configure(slog.LevelInfo, logging.FormatJSON, logs)
	t.Cleanup(func() { logging.Configure(slog.LevelInfo, logging.FormatConsole, os.Stderr) })
	above := t.TempDir()
	planted := filepath.Join(above, "AGENTS.md")
	if err := os.WriteFile(planted, []byte("INSTRUCTIONS FROM ABOVE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.HarnessHomeEnv, filepath.Join(above, "harness-home"))
	warned := func() bool {
		got := logs.String()
		return strings.Contains(got, "harness_ancestor_files") && strings.Contains(got, planted)
	}

	e := bundleNode(t, parseBundled(t, cursorCompanyDoc))
	if !warned() {
		t.Errorf("the boot did not warn of %s:\n%s", planted, logs.String())
	}
	logs.mu.Lock()
	logs.buf.Reset()
	logs.mu.Unlock()
	if status, stages, err := e.Apply(t.Context(), parseBundled(t, cursorCompanyDoc), time.Now()); err != nil ||
		status != configplane.StatusOK {
		t.Fatalf("Apply = (%s, %v, %v), want ok", status, stages, err)
	}
	if !warned() {
		t.Errorf("the apply did not warn of %s:\n%s", planted, logs.String())
	}
}
