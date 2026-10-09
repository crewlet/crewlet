package codingagent_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

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
// a ref line: prose after the label, nothing after it, or a URL that carries
// a credential.
func TestADeliveredLineIsReadWhateverItsMarkup(t *testing.T) {
	t.Parallel()
	for line, want := range map[string]string{
		"Delivered: wip/t1":                     "wip/t1",
		"- Delivered: wip/t1":                   "wip/t1",
		"* **Delivered:** `wip/t1`":             "wip/t1",
		"**Delivered**: <https://h/a/b/pull/1>": "https://h/a/b/pull/1",
		"2. delivered: feature/retry.":          "feature/retry",
		"  DELIVERED:   wip/t1  ":               "wip/t1",
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
	instruction := codingagent.FindingsInstruction("/tmp/findings.md")
	var lines, want []string
	for i, span := range strings.Split(instruction, "`") {
		ref, ok := strings.CutPrefix(span, "Delivered: ")
		if i%2 == 0 || !ok || strings.HasPrefix(ref, "<") {
			continue
		}
		lines = append(lines, span)
		want = append(want, ref)
	}
	if len(want) < 2 {
		t.Fatalf("the instruction shows %d examples of a Delivered line, want a branch and a URL:\n%s",
			len(want), instruction)
	}
	refs := refsThroughEveryPath(t, strings.Join(lines, "\n"))
	for path, got := range refs {
		if !slices.Equal(got, want) {
			t.Errorf("%s: the instruction's examples read as %q, want %q", path, got, want)
		}
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
