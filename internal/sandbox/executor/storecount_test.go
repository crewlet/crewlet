package executor_test

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/prereqtest"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// storeBox is a box for a Hermes run whose store answers each count from
// answers in turn, the last again once they run out, counting the asks.
func storeBox(t *testing.T, runner *executor.Runner, answers ...int) (*sandbox.FakeSandbox, *int) {
	t.Helper()
	b := box(t, runner)
	asked := new(int)
	b.ExecFunc = func(_ context.Context, cmd string, _ sandbox.ExecOptions) (sandbox.ExecResult, error) {
		if !strings.Contains(cmd, "hcarry requests") {
			return sandbox.ExecResult{}, nil
		}
		n := answers[min(*asked, len(answers)-1)]
		*asked++
		return sandbox.ExecResult{Stdout: fmt.Sprintf("crewlet-carry ok %d\n", n)}, nil
	}
	return b, asked
}

// A HERMES RUN IS COUNTED OFF ITS SESSION STORE, live and at its collection:
// nothing is asked before the run records where it began in the store; each
// read asks the store again, the count never going down; and the collection
// asks it too, so a capped run whose summary the plugin's record leaves out
// collects as past its cap all the same.
//
// Mutation: count off the plugin's record, let a later answer lower the count,
// ask before the run's start is recorded, or collect off the stream's count,
// and a check goes red.
func TestAHermesRunIsCountedOffItsSessionStore(t *testing.T) {
	t.Parallel()
	runner := executor.New(executor.Hermes{})
	b, asked := storeBox(t, runner, 3, 5, 4)
	c := runner.Count(sandbox.RunHandle{TurnCap: 3})
	if got, err := c.Read(t.Context(), b); err != nil || got.Requests != 0 || *asked != 0 {
		t.Fatalf("before the run's start is recorded: %+v (%v), %d asks; want nothing, none asked", got, err, *asked)
	}
	b.Put(executor.PathsFor(b).HermesRunStart(), fixtureSession+" 7\n")
	for i, want := range []int{3, 5, 5} {
		if got, err := c.Read(t.Context(), b); err != nil || got.Requests != want {
			t.Fatalf("read %d: %+v (%v), want %d requests", i+1, got, err, want)
		}
	}

	collected, _ := storeBox(t, runner, 4)
	p := executor.PathsFor(collected)
	collected.Put(p.HermesRunStart(), fixtureSession+" 0\n")
	collected.Put(executor.Hermes{}.Output(p).Stdout, streamOf(
		`{"type": "system", "subtype": "init", "model": "", "session_id": ""}`,
		`{"type": "result", "session_id": "local-1", "exit_code": 0, "text": "done"}`,
		each(3, `{"type":"crewlet.hermes_request","line":{"turn_id":"t_%d","finish_reason":"stop"}}`)))
	collected.Put(p.Findings(), "Outcome: succeeded")
	collected.Put(p.ExitCode(), "0")
	res, err := runner.Collect(t.Context(), collected, sandbox.RunHandle{TurnCap: 3}, queue.NewAnswerBudget(nil))
	if err != nil || res.Success || !res.TurnCapExceeded {
		t.Fatalf("3 recorded by the plugin and 4 by the store under a cap of 3: success %v, exceeded %v (%v); "+
			"want past the cap", res.Success, res.TurnCapExceeded, err)
	}
}

// hermesStore is a Python script that writes a Hermes 0.23.3 session store at
// argv[1]: the sessions and messages columns the count reads, the run's session
// titled "t", rotated once by a compaction into a child, another session
// beside it, and the rows its count is held to.
const hermesStore = `
import sqlite3, sys
db = sqlite3.connect(sys.argv[1])
db.execute("create table sessions (id text, title text, started_at real, parent_session_id text, source text, " +
           "model_config text, end_reason text)")
db.execute("create table messages (id integer primary key, session_id text, role text, content text, " +
           "display_kind text, display_metadata text)")
db.executemany("insert into sessions values (?, ?, ?, ?, ?, ?, ?)", [
    ("root", None, 1, None, "cli", None, "compression"), ("child", "t", 2, "root", "cli", None, None),
    ("other", "u", 3, None, "cli", None, None)])
db.executemany("insert into messages (id, session_id, role, content, display_kind, display_metadata) " +
               "values (?, ?, ?, ?, ?, ?)", [
    (1, "root", "assistant", "an earlier launch's answer", None, None),
    (2, "root", "user", "go", None, None),
    (3, "root", "assistant", "", None, None),
    (4, "child", "assistant", "a summary", None, None),
    (5, "child", "assistant", "Operation interrupted: waiting", None, '{"interrupted": true}'),
    (6, "child", "assistant", "Gateway shutting down", "failed_turn", None),
    (7, "other", "assistant", "another session's", None, None),
    (8, "child", "tool", "{}", None, None)])
db.commit()
`

// HERMES'S STORE COUNTS A RUN'S ANSWERS, NOT ITS NOTES (hermes_carry.py's
// requests): the assistant rows of the run's session and the sessions a
// compaction rotated it through, past where the run began, but the notes
// Hermes writes as one, an interrupt's and a failed turn's, which no answer
// carries the marks of (measured on 0.23.3); never another session's rows, and
// none of a store holding no messages.
//
// Mutation: count every assistant row, count the run's newest session alone,
// or count from the store's first row, and the count is not 2.
func TestHermesStoreCountsARunsAnswersNotItsNotes(t *testing.T) {
	t.Parallel()
	python := prereqtest.OnPath(t, "python3", "the case runs Hermes's store count", "install python3")
	db := filepath.Join(t.TempDir(), "state.db")
	if out, err := exec.CommandContext(t.Context(), python, "-I", "-c", hermesStore, db).CombinedOutput(); err != nil {
		t.Fatalf("writing the store: %v: %s", err, out)
	}
	count := func(dbPath, after string) string {
		t.Helper()
		out, err := exec.CommandContext(t.Context(), python, "-I", "hermes_carry.py", "requests", dbPath, "t",
			after).CombinedOutput()
		if err != nil {
			t.Fatalf("hermes_carry.py requests: %v: %s", err, out)
		}
		return strings.TrimSpace(string(out))
	}
	if got := count(db, "1"); got != "crewlet-carry ok 2" {
		t.Errorf("the run's answers past row 1 read %q, want its 2", got)
	}
	if got := count(filepath.Join(t.TempDir(), "none.db"), "0"); got != "crewlet-carry ok 0" {
		t.Errorf("a store not there reads %q, want none", got)
	}
}
