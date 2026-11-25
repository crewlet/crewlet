package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
// runs: a lookup, a write that stops at its proposal, a declined injection, the
// quality cases (a two-part question answered with the failure's reason, names
// for a space and seat, an open request that asks rather than shares, spend by
// week, a change from an unrelated screen) and the brief cases (rows kept only
// for a record read and resolved, danger only from its own read, all-clear,
// injection, a record gone before the brief, over-long answers whose one retry
// lands (the #407 replay), exact caps, and an ask waiting in a chat room), and
// a task too big for a turn proposed for an agent seat. A change breaking a
// case fails here, and the example an operator copies is one the engine runs. Mutation: let a write tool execute instead of proposing,
// and file-a-task ends with no proposal; let no row cite a room, and both chat
// cases end with nothing needing the person.
func TestTheShippedAssistantEvalSuitePasses(t *testing.T) {
	t.Parallel()
	suite := filepath.Join(sourcetree.Root(t), "examples", "assistant-evals")
	var out bytes.Buffer
	if err := run([]string{"assistant", "eval", "-suite", suite, "-trials", "2"}, &out,
		&out); err != nil {
		t.Fatalf("the shipped suite failed: %v\n%s", err, out.String())
	}
	for _, want := range []string{"find-a-task", "file-a-task", "injected-instruction",
		"status-and-failure", "page-in-named-space", "ambiguous-brand",
		"week-over-week-spend", "change-from-another-screen",
		"brief-inbox", "brief-all-clear", "brief-injected-instruction", "brief-record-gone",
		"brief-retry", "brief-at-the-caps", "brief-every-cap", "brief-chatlet-mentions",
		"brief-chatlet-room", "agent-lacks-a-github-permission", "integration-set-up-for-an-agent",
		"two-access-clocks", "a-permission-the-connection-lacks", "approve-a-grant-request",
		"injected-agent-question", "answer-a-question-by-its-option", "what-a-task-waits-on",
		"answer-only-your-own-question", "decide-the-docs-route", "how-the-agent-gets-the-access",
		"instructions-planted-in-a-comment", "answer-in-your-own-words",
		"explain-how-a-grant-works", "who-changed-what", "read-one-config-section",
		"read-a-vendor-page", "a-page-cannot-steer-a-change", "search-a-vendors-docs",
		"big-task-becomes-a-work-item", "setup-button-reads-connect",
		"stepper-on-its-second-step", "a-field-shown-empty",
		"a-screen-cannot-steer-the-assistant", "2/2",
		"pass^2 43/43 cases",
		"model scripted"} {
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
  calls: [read_api]
  says: [ada]
`
	dir := t.TempDir()
	writeCase(t, filepath.Join(dir, "spend.yaml"), failing)
	var out bytes.Buffer
	err := run([]string{"assistant", "eval", "-suite", dir}, &out, &out)
	if err == nil || !strings.Contains(err.Error(), "1 of 1 cases failed") {
		t.Fatalf("a failing case answered %v:\n%s", err, out.String())
	}
	for _, want := range []string{"spend", "0/1", "spend, trial 1: calls: read_api not called",
		`spend, trial 1: says: "ada" not in the answer`} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the report does not say %q:\n%s", want, out.String())
		}
	}

	for name, tc := range map[string]struct{ body, want string }{
		"a misspelt member": {`question: q
script: [{say: x}]
expect: {state: completed, sayz: [x]}
`, "expect.sayz: unknown field"},
		"a tool the catalogue lacks": {`question: q
script: [{call: delete_everything}]
expect: {state: completed}
`, `"delete_everything" is not a catalogue tool`},
		"a case that cannot fail": {`question: q
script: [{say: x}]
`, "expects nothing"},
		"a round with both a call and prose": {`question: q
script: [{call: read_api, say: x}]
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

// A PAID MODEL IS NEVER CALLED WITHOUT ITS FLAG: -model alone is refused
// before any configuration is read, naming -paid, and -paid alone is refused
// as confirming nothing. No case here reaches a model. Mutation: drop the
// -paid check and the first run goes on to read a company file.
func TestAPaidModelIsAskedOnlyWithItsFlag(t *testing.T) {
	t.Parallel()
	suite := filepath.Join(sourcetree.Root(t), "examples", "assistant-evals")
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"-suite", suite, "-model", "assistant"}, "pass -paid to confirm"},
		{[]string{"-suite", suite, "-paid"}, "no -model is named"},
	} {
		var out bytes.Buffer
		err := run(append([]string{"assistant", "eval"}, tc.args...), &out, &out)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("assistant eval %v answered %v, want %q", tc.args, err, tc.want)
		}
	}
}

// THE BINARY LINKS NO TEST SCAFFOLDING: the crewlet binary's dependency graph,
// as the toolchain that builds it reads it (`go list -deps`), holds neither
// the `testing` package nor internal/assistant/assistanttest (nor
// net/http/httptest, which imports `testing`). Shipping code and test
// scaffolding answer to different review bars, and a helper added to a test
// package must never ship by being reachable from a production one: the
// assistant's evaluation harness is internal/assistant/evals, which the binary
// links, and assistanttest only wraps it for the suites. Mutation: import
// assistanttest (or net/http/httptest) from internal/assistant/evals and this
// names it.
func TestTheBinaryLinksNoTestScaffolding(t *testing.T) {
	t.Parallel()
	// The go command that runs this test puts its own GOROOT/bin first on the
	// PATH it hands a test, so the go found there is the toolchain building
	// this binary.
	gotool, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("the go tool is not on PATH: %v", err)
	}
	list := exec.Command(gotool, "list", "-deps", ".")
	list.Dir = filepath.Join(sourcetree.Root(t), "cmd", "crewlet")
	out, err := list.Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	deps := strings.Fields(string(out))
	if !slices.Contains(deps, "github.com/crewlet/crewlet/internal/assistant/evals") {
		t.Fatalf("the binary does not link internal/assistant/evals, so this walk is not "+
			"reading the graph it guards (%d packages)", len(deps))
	}
	for _, banned := range []string{"testing", "net/http/httptest",
		"github.com/crewlet/crewlet/internal/assistant/assistanttest"} {
		if slices.Contains(deps, banned) {
			t.Errorf("the crewlet binary links %s, which is test scaffolding", banned)
		}
	}
}
