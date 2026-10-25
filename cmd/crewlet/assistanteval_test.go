package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
)

// THE EVAL COMMAND IS IN USAGE, and what usage names is the command the switch
// runs: `crewlet assistant eval` is listed by `crewlet help`, and that
// spelling reaches the evaluation (its own usage on a missing -suite), never
// `unknown command`. TestUsageAdvertisesEveryDispatchedCommand holds the
// switch and the usage to each other in general; this pins the one this work
// added. Mutation: drop the usage line, or the switch's case, and this fails.
func TestTheEvalCommandIsInUsage(t *testing.T) {
	t.Parallel()
	var help bytes.Buffer
	if err := run([]string{"help"}, &help, &help); err == nil {
		t.Fatal("help returned no sentinel")
	}
	if !strings.Contains(help.String(), "crewlet assistant eval") {
		t.Fatalf("usage does not name `crewlet assistant eval`:\n%s", help.String())
	}
	var out bytes.Buffer
	err := run([]string{"assistant", "eval"}, &out, &out)
	if err == nil || !strings.Contains(err.Error(), "-suite") ||
		!strings.Contains(out.String(), "usage: crewlet assistant eval") {
		t.Fatalf("`crewlet assistant eval` with no suite answered %v:\n%s", err, out.String())
	}
}

// THE SHIPPED SUITE PASSES ON ITS OWN SCRIPTS, through the command an operator
// runs: a lookup, a write that stops at its proposal and an injection the
// script declines. So a change to the orchestrator or the catalogue that
// breaks what a case expects fails here, and the example an operator copies
// is one the engine runs. Mutation: let a write tool execute instead of
// proposing, and file-a-task ends with no proposal.
func TestTheShippedAssistantEvalSuitePasses(t *testing.T) {
	t.Parallel()
	suite := filepath.Join(sourcetree.Root(t), "examples", "assistant-evals")
	var out bytes.Buffer
	if err := run([]string{"assistant", "eval", "-suite", suite, "-trials", "2"}, &out,
		&out); err != nil {
		t.Fatalf("the shipped suite failed: %v\n%s", err, out.String())
	}
	for _, want := range []string{"find-a-task", "file-a-task", "injected-instruction",
		"2/2", "pass^2 3/3 cases", "model scripted"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the report does not say %q:\n%s", want, out.String())
		}
	}
}

// A CASE THAT FAILS FAILS THE COMMAND, saying which expectation and in which
// trial; and a case that could not be checked as written is refused before
// anything runs, naming its file and member. Mutation: drop the exit status,
// or the strict decode, and this fails.
func TestAnEvalCaseThatFailsFailsTheCommand(t *testing.T) {
	t.Parallel()
	failing := `question: Who spent the most?
script:
  - say: Nobody spent anything.
expect:
  calls: [get_spend]
  says: [ada]
`
	dir := t.TempDir()
	writeCase(t, filepath.Join(dir, "spend.yaml"), failing)
	var out bytes.Buffer
	err := run([]string{"assistant", "eval", "-suite", dir}, &out, &out)
	if err == nil || !strings.Contains(err.Error(), "1 of 1 cases failed") {
		t.Fatalf("a failing case answered %v:\n%s", err, out.String())
	}
	for _, want := range []string{"spend", "0/1", "spend, trial 1: calls: get_spend not called",
		`spend, trial 1: says: "ada" not in the answer`} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the report does not say %q:\n%s", want, out.String())
		}
	}

	for name, tc := range map[string]struct{ body, want string }{
		"a misspelt member": {`question: q
script: [{say: x}]
expect: {state: completed, sayz: [x]}
`, `unknown field "sayz"`},
		"a tool the catalogue lacks": {`question: q
script: [{call: delete_everything}]
expect: {state: completed}
`, `"delete_everything" is not a catalogue tool`},
		"a case that cannot fail": {`question: q
script: [{say: x}]
`, "expects nothing"},
		"a round with both a call and prose": {`question: q
script: [{call: list_agents, say: x}]
expect: {state: completed}
`, "exactly one of call and say"},
	} {
		dir := t.TempDir()
		writeCase(t, filepath.Join(dir, "bad.yaml"), tc.body)
		var out bytes.Buffer
		err := run([]string{"assistant", "eval", "-suite", dir}, &out, &out)
		if err == nil || !strings.Contains(err.Error(), tc.want) ||
			!strings.Contains(err.Error(), "bad.yaml") {
			t.Errorf("%s answered %v, want it refused naming the file and %q", name, err, tc.want)
		}
	}
}

// writeCase puts a case file's body at path.
func writeCase(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
