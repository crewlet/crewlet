package codingagent_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/codingagent"
)

// refsThroughEveryPath is what each of the three places a run's account is
// read from makes of text: the report the agent wrote (a whole collection),
// Claude Code's result message, and OpenCode's event stream and plain output.
// Each is one reading of the same rule, so every case runs through all four.
func refsThroughEveryPath(t *testing.T, text string) map[string][]string {
	t.Helper()
	runner := codingagent.NewClaudeCode()
	b := box(t, runner)
	b.Put(paths(b).Findings(), "Outcome: succeeded\n"+text)
	b.Put(paths(b).ExitCode(), "0")
	collected, err := runner.Collect(t.Context(), b, sandbox.RunHandle{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	result, err := json.Marshal(map[string]any{"type": "result", "subtype": "success", "result": text})
	if err != nil {
		t.Fatal(err)
	}
	event, err := json.Marshal(map[string]any{"type": "text", "part": map[string]any{"text": text}})
	if err != nil {
		t.Fatal(err)
	}
	return map[string][]string{
		"the report":              collected.DeliveredRefs,
		"claude-code's result":    codingagent.ClaudeCode{}.Parse(string(result)).DeliveredRefs,
		"opencode's event stream": codingagent.OpenCode{}.Parse(string(event)).DeliveredRefs,
		"opencode's plain output": codingagent.OpenCode{}.Parse(text).DeliveredRefs,
	}
}

// A PULL REQUEST IS RECOGNISED BY ITS SHAPE, ON ANY HOST. Which hosts a box
// can push to is set by the seat's own environment and setup steps, which the
// engine never names — so a GitHub Enterprise, a self-managed GitLab on a port
// of its own, or the walkthrough's own GitLab at gitlab.local:8929 delivered
// nothing at all while the pattern knew two hosts by name.
func TestADeliveredPullRequestIsRecognisedOnAnyCodeHost(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		url  string
		want bool
	}{
		"github.com":                 {"https://github.com/acme/api/pull/42", true},
		"gitlab.com":                 {"https://gitlab.com/acme/api/-/merge_requests/5", true},
		"github enterprise":          {"https://ghe.example.com/acme/api/pull/7", true},
		"self-managed gitlab, port":  {"http://gitlab.local:8929/nimbus-hq/nimbuscore/-/merge_requests/3", true},
		"self-managed gitlab, group": {"https://gitlab.example.com/g/sub/p/-/merge_requests/12", true},
		"gitea":                      {"https://gitea.example.com/acme/api/pulls/8", true},
		"bitbucket":                  {"https://bitbucket.org/acme/api/pull-requests/9", true},
		"an issue":                   {"https://ghe.example.com/acme/api/issues/7", false},
		"a pull path with no number": {"https://ghe.example.com/acme/api/pull/", false},
		"credentials in the host":    {"https://x-token:hunter2hunter2@ghe.example.com/acme/api/pull/7", false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var want []string
			if tc.want {
				want = []string{tc.url}
			}
			for path, got := range refsThroughEveryPath(t, "Opened "+tc.url+" for review.") {
				if !slices.Equal(got, want) {
					t.Errorf("%s: refs = %q, want %q", path, got, want)
				}
			}
		})
	}
}

// THE REPORT'S OWN `Delivered:` LINES ARE ITS REFS — a branch as well as a pull
// request on any host, in the order it names them — and a pull request the
// report only MENTIONS is not one of them: the scrape that cannot tell the two
// apart is for a report that names nothing.
func TestTheReportsDeliveredLinesAreItsRefs(t *testing.T) {
	t.Parallel()
	report := strings.Join([]string{
		"Followed the pattern in https://github.com/acme/api/pull/3.",
		"Delivered: wip/t1",
		"Delivered: https://ghe.example.com/a/b/pull/7",
	}, "\n")
	want := []string{"wip/t1", "https://ghe.example.com/a/b/pull/7"}
	for path, got := range refsThroughEveryPath(t, report) {
		if !slices.Equal(got, want) {
			t.Errorf("%s: refs = %q, want %q", path, got, want)
		}
	}
}

// A `Delivered:` line is read however a report lays it out — a list marker,
// emphasis, a ref in backticks — and one that does not hold a ref alone is not
// a ref line: prose after the label, nothing after it, a branch longer than a
// ref can be, or a URL that carries a credential.
func TestADeliveredLineIsReadWhateverItsMarkup(t *testing.T) {
	t.Parallel()
	longest := strings.Repeat("b", gitref.MaxBranchBytes)
	for line, want := range map[string]string{
		"Delivered: wip/t1":                     "wip/t1",
		"- Delivered: wip/t1":                   "wip/t1",
		"* **Delivered:** `wip/t1`":             "wip/t1",
		"**Delivered**: <https://h/a/b/pull/1>": "https://h/a/b/pull/1",
		"2. delivered: feature/retry.":          "feature/retry",
		"  DELIVERED:   wip/t1  ":               "wip/t1",
		"Delivered: " + longest:                 longest,
		"Delivered: " + longest + "b":           "",
		"Delivered: the fix for the flake":      "",
		"Delivered:":                            "",
		"Delivered: wip..t1":                    "",
		"Delivered: -wip":                       "",
		"Delivered: https://u:p@h/a/b/pull/1":   "",
		"Not delivered: wip/t1":                 "",
	} {
		var expected []string
		if want != "" {
			expected = []string{want}
		}
		if got := (codingagent.ClaudeCode{}).Parse(`{"type":"result","subtype":"success","result":` +
			quote(t, line) + `}`).DeliveredRefs; !slices.Equal(got, expected) {
			t.Errorf("%q: refs = %q, want %q", line, got, expected)
		}
	}
}

// A REF IS HANDED ON AS THE ACCOUNT WROTE IT, a URL that is not UTF-8
// included — on a `Delivered:` line, or in the host a scrape reads — because
// whether the run's record can carry a ref is the coordinator's to judge, for
// every runner alike, and it can judge only the bytes it is handed: mended
// into UTF-8 here, as a decode mends one, the ref would reach the record as a
// URL nobody delivered, with nothing left to tell it from one somebody did.
//
// The two paths that read an account raw: the report, and OpenCode's plain
// output. Claude Code's result and OpenCode's event stream are JSON decoded
// before any ref is read from them, and JSON is UTF-8 throughout.
func TestADeliveredRefIsHandedOnAsTheAccountWroteIt(t *testing.T) {
	t.Parallel()
	for text, want := range map[string]string{
		"Delivered: https://h/a/b/pull/1\xff":         "https://h/a/b/pull/1\xff",
		"Opened https://h\xff/a/b/pull/2 for review.": "https://h\xff/a/b/pull/2",
	} {
		runner := codingagent.NewClaudeCode()
		b := box(t, runner)
		b.Put(paths(b).Findings(), "Outcome: succeeded\n"+text)
		b.Put(paths(b).ExitCode(), "0")
		collected, err := runner.Collect(t.Context(), b, sandbox.RunHandle{})
		if err != nil {
			t.Fatalf("Collect: %v", err)
		}
		for path, got := range map[string][]string{
			"the report":              collected.DeliveredRefs,
			"opencode's plain output": codingagent.OpenCode{}.Parse(text).DeliveredRefs,
		} {
			if !slices.Equal(got, []string{want}) {
				t.Errorf("%s: %q: refs = %q, want %q", path, text, got, []string{want})
			}
		}
	}
}

// The report is the account of record, so ITS named refs win over the result
// message's — and a message that names refs on lines of its own beats a
// report that only mentions a pull request, since a named ref is the run's
// word and a scraped one is a guess.
func TestACollectionPrefersANamedRefWhereverItIs(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		report, message string
		want            []string
	}{
		"the report names its refs": {
			report:  "Delivered: wip/report",
			message: "Delivered: wip/message",
			want:    []string{"wip/report"},
		},
		"only the message names them": {
			report:  "Looked at https://github.com/acme/api/pull/3 first.",
			message: "Delivered: wip/message",
			want:    []string{"wip/message"},
		},
		"neither names them": {
			report:  "Opened https://ghe.example.com/a/b/pull/7.",
			message: "Opened https://ghe.example.com/a/b/pull/8.",
			want:    []string{"https://ghe.example.com/a/b/pull/7"},
		},
		"only the message has one": {
			report:  "Outcome: succeeded",
			message: "Opened https://ghe.example.com/a/b/pull/8.",
			want:    []string{"https://ghe.example.com/a/b/pull/8"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runner := codingagent.NewClaudeCode()
			b := box(t, runner)
			p := paths(b)
			b.Put(p.Result(), `{"type":"result","subtype":"success","result":`+quote(t, tc.message)+`}`)
			b.Put(p.Findings(), tc.report)
			b.Put(p.ExitCode(), "0")
			res, err := runner.Collect(t.Context(), b, sandbox.RunHandle{})
			if err != nil {
				t.Fatalf("Collect: %v", err)
			}
			if !slices.Equal(res.DeliveredRefs, tc.want) {
				t.Errorf("refs = %q, want %q", res.DeliveredRefs, tc.want)
			}
		})
	}
}

// THE BRIEF AND THE READER AGREE: every example of the line the report
// instruction shows the agent is a line the collection reads as the ref it
// names. Two halves of one protocol written in two places drift, and a drift
// here is silent — the agent writes what it was shown and the run delivers
// nothing.
func TestTheReportInstructionsExamplesAreReadAsTheirRefs(t *testing.T) {
	t.Parallel()
	// With and without the native half, whose own words about the line must
	// read as no example of one.
	for _, native := range [][]string{nil, {"web", "api"}} {
		instruction := codingagent.FindingsInstruction("/tmp/findings.md", native)
		var lines, want []string
		for i, span := range strings.Split(instruction, "`") {
			ref, ok := strings.CutPrefix(span, "Delivered: ")
			if i%2 == 0 || !ok || strings.HasPrefix(ref, "<") {
				continue
			}
			lines = append(lines, span)
			want = append(want, ref)
		}
		if len(want) != 2 {
			t.Fatalf("native %q: the instruction shows %d examples of a Delivered line, want a branch and "+
				"a URL:\n%s", native, len(want), instruction)
		}
		refs := refsThroughEveryPath(t, strings.Join(lines, "\n"))
		for path, got := range refs {
			if !slices.Equal(got, want) {
				t.Errorf("native %q, %s: the instruction's examples read as %q, want %q", native, path, got,
					want)
			}
		}
	}
}

// WHAT THE ENGINE REPORTS FOR A RUN, THE RUN IS TOLD: its pushes to the
// repositories it was given on the engine's own code host are read from the
// push record and handed to the executor, so the report is asked for lines
// only for what it delivered elsewhere — a line for each such push is the
// same delivery said twice, once by the record and once by the box. A run
// given none of them is told nothing of it.
func TestTheFindingsInstructionSaysWhatIsReported(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		native []string
		names  string
	}{
		{[]string{"web"}, `Your pushes to "web" on this company's code host`},
		{[]string{"web", "api"}, `Your pushes to "web" and "api" on this company's code host`},
		{[]string{"web", "api", "docs"}, `Your pushes to "web", "api" and "docs" on this company's code host`},
	} {
		instruction := codingagent.FindingsInstruction("/tmp/findings.md", tc.native)
		if !strings.Contains(instruction, tc.names+" are recorded and reported for you: write a "+
			"`Delivered:` line only for what you delivered somewhere else.") {
			t.Errorf("native %q: the instruction does not say the pushes are reported:\n%s", tc.native,
				instruction)
		}
	}
	if none := codingagent.FindingsInstruction("/tmp/findings.md", nil); strings.Contains(none, "code host") {
		t.Errorf("a run given no repository on the code host is told of its pushes there:\n%s", none)
	}

	// AND A RUNNER PUTS IT IN THE TEXT ITS BOX IS GIVEN, from the run's own
	// request. Mutation: finalSystem passing no repositories — red.
	_, system, _ := systemOfRun(t, codingagent.NewClaudeCode(), sandbox.RunRequest{
		Brief: "fix the flake", ReportedRepos: []string{"web", "api"},
	})
	if !strings.Contains(system, `Your pushes to "web" and "api" on this company's code host are recorded`) {
		t.Errorf("the run's standing instructions do not say its pushes are reported:\n%s", system)
	}
}

func quote(t *testing.T, s string) string {
	t.Helper()
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
