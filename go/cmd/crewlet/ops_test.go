package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

func cli(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	var out, errs bytes.Buffer
	err := run(args, &out, &errs)
	return out.String(), errs.String(), err
}

// -check REPORTS AND APPLIES NOTHING. A command that migrated while
// answering "what would you migrate" could never answer it.
func TestMigrateCheckReportsPendingWithoutApplying(t *testing.T) {
	dir := t.TempDir()
	cfg := bootstrapForStore(t, dir)

	out, _, err := cli(t, "migrate", "-config", cfg, "-check")
	if err == nil {
		// NON-ZERO IS THE POINT: this is what a deploy gate calls, and a
		// gate that reported pending work and exited 0 stops nothing.
		t.Fatal("a database with pending migrations exited 0")
	}
	if !strings.Contains(out, "pending") {
		t.Errorf("output = %q", out)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "index.db")); statErr == nil {
		// Reading may create the file, but it must not create the
		// schema — which the next assertion proves.
		applied, pending, perr := store.Pending(context.Background(),
			filepath.Join(dir, "index.db"), store.Options{})
		if perr != nil {
			t.Fatalf("Pending: %v", perr)
		}
		if len(applied) != 0 || len(pending) == 0 {
			t.Errorf("-check applied %d migration(s)", len(applied))
		}
	}
}

// MIGRATING IS WHAT OPENING DOES, so the command reports rather than
// reimplements — a second migrator is one that can disagree with the engine
// about what "applied" means.
func TestMigrateAppliesAndThenIsQuiet(t *testing.T) {
	dir := t.TempDir()
	cfg := bootstrapForStore(t, dir)

	out, _, err := cli(t, "migrate", "-config", cfg)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if !strings.Contains(out, "applied") {
		t.Errorf("output = %q", out)
	}

	// A SECOND RUN IS A NO-OP and says so, which is what makes it safe in
	// a deploy script that runs on every rollout.
	out, _, err = cli(t, "migrate", "-config", cfg)
	if err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if !strings.Contains(out, "up to date") {
		t.Errorf("second run = %q", out)
	}

	// And -check now passes, which is the gate a deploy actually reads.
	if _, _, err = cli(t, "migrate", "-config", cfg, "-check"); err != nil {
		t.Errorf("-check failed on a migrated database: %v", err)
	}
}

// AN EMPTY TABLE IS NOT ZERO SPEND. A company that has spent nothing and one
// whose counters were reset are the same row-less state, and printing "0"
// for scopes that do not exist would invent seats.
func TestBudgetsShowSaysNothingRatherThanZero(t *testing.T) {
	dir := t.TempDir()
	cfg := bootstrapForStore(t, dir)

	out, _, err := cli(t, "budgets", "show", "-config", cfg)
	if err != nil {
		t.Fatalf("budgets show: %v", err)
	}
	if !strings.Contains(out, "has spent anything") {
		t.Errorf("output = %q", out)
	}
	if strings.Contains(out, " 0 ") {
		t.Errorf("a scope that does not exist was printed as zero: %q", out)
	}
}

// spend charges a scope, the way a turn does.
func spend(t *testing.T, dir string, agent string, tokens int) {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(dir, "index.db"), store.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Budgets().Charge(context.Background(), agent, tokens, 0, 0); err != nil {
		t.Fatalf("charge: %v", err)
	}
}

func TestBudgetsShowListsWhatEachScopeSpent(t *testing.T) {
	dir := t.TempDir()
	cfg := bootstrapForStore(t, dir)
	spend(t, dir, "swe", 1200)

	out, _, err := cli(t, "budgets", "show", "-config", cfg)
	if err != nil {
		t.Fatalf("budgets show: %v", err)
	}
	if !strings.Contains(out, store.OrgScope) || !strings.Contains(out, "1200") {
		t.Errorf("output = %q", out)
	}
	if !strings.Contains(out, store.AgentScope("swe")) {
		t.Errorf("the seat's own scope is missing: %q", out)
	}
}

// A RESET NAMES WHAT IT CLEARED. A count alone leaves an operator unable to
// tell "reset the seat I meant" from "reset a scope that was already empty".
func TestBudgetsResetNamesTheScopesItCleared(t *testing.T) {
	dir := t.TempDir()
	cfg := bootstrapForStore(t, dir)
	spend(t, dir, "swe", 500)

	out, _, err := cli(t, "budgets", "reset", "-config", cfg,
		"-scope", store.AgentScope("swe"))
	if err != nil {
		t.Fatalf("budgets reset: %v", err)
	}
	if !strings.Contains(out, store.AgentScope("swe")) {
		t.Errorf("output = %q", out)
	}
	// SCOPED MEANS SCOPED, in what it clears AND in what it claims to
	// have cleared: the org counter is a different ceiling, and naming it
	// would tell an operator they had re-armed a company somebody had
	// stopped on purpose.
	if strings.Contains(out, store.OrgScope) {
		t.Errorf("a scoped reset named a scope it did not clear: %q", out)
	}
	shown, _, err := cli(t, "budgets", "show", "-config", cfg)
	if err != nil {
		t.Fatalf("budgets show: %v", err)
	}
	if !strings.Contains(shown, store.OrgScope) {
		t.Errorf("the org counter was cleared by a scoped reset: %q", shown)
	}
	if strings.Contains(shown, store.AgentScope("swe")) {
		t.Errorf("the seat's counter survived its own reset: %q", shown)
	}
}

// RESETTING NOTHING SAYS SO rather than reporting a success that did not
// happen.
func TestResettingAScopeThatIsNotThereSaysSo(t *testing.T) {
	dir := t.TempDir()
	cfg := bootstrapForStore(t, dir)

	out, _, err := cli(t, "budgets", "reset", "-config", cfg, "-scope", "agent:nobody")
	if err != nil {
		t.Fatalf("budgets reset: %v", err)
	}
	if !strings.Contains(out, "Nothing to reset") {
		t.Errorf("output = %q", out)
	}
}

func TestBudgetsRejectsAnUnknownSubcommand(t *testing.T) {
	dir := t.TempDir()
	cfg := bootstrapForStore(t, dir)
	if _, _, err := cli(t, "budgets", "explode", "-config", cfg); err == nil {
		t.Fatal("an unknown subcommand was accepted")
	}
}

var _ = time.Now
