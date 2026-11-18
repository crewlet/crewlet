package activityline_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/activityline"
	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/redact/redacttest"
)

// A CALL READS BACK AS WRITTEN: its number, tool, preview and the count of its
// subject's lines past the first; its input lines and its result likewise.
func TestEveryLineReadsBackAsWritten(t *testing.T) {
	t.Parallel()
	script := "python3 - <<'EOF'\nimport json\n  print(json.dumps({}))\n\nEOF"
	lines := activityline.Call(3, "Bash", script)
	if len(lines) != 6 {
		t.Fatalf("lines = %q; want the header and the script's five lines", lines)
	}
	head := activityline.Parse(lines[0])
	if head.Tag != activityline.TagCall || head.Seq != 3 || head.Tool != "Bash" ||
		head.Text != "python3 - <<'EOF'" || head.More != 4 {
		t.Errorf("header %q reads as %+v", lines[0], head)
	}
	var input []string
	for _, l := range lines[1:] {
		in := activityline.Parse(l)
		if in.Tag != activityline.TagInput || in.Seq != 3 {
			t.Fatalf("input line %q reads as %+v", l, in)
		}
		input = append(input, in.Text)
	}
	if got := strings.Join(input, "\n"); got != script {
		t.Errorf("input reads back as %q, want the script whole, its indent kept: %q", got, script)
	}

	for line, want := range map[string]activityline.Line{
		activityline.OK(3, activityline.Took{D: 1250 * time.Millisecond, Known: true}): {Tag: activityline.TagOK,
			Seq: 3, Took: activityline.Took{D: 1250 * time.Millisecond, Known: true}},
		activityline.OK(3, activityline.Took{}): {Tag: activityline.TagOK, Seq: 3},
		activityline.Failed(4, "exit 1\nFAIL", activityline.Took{Known: true}): {Tag: activityline.TagFailed,
			Seq: 4, Text: "exit 1 (+1 more line(s))", Took: activityline.Took{Known: true}},
		activityline.Failed(0, "denied", activityline.Took{}): {Tag: activityline.TagFailed, Text: "denied"},
		activityline.Call(1, "mcp__crewlet__get_work_item", "")[0]: {Tag: activityline.TagCall, Seq: 1,
			Tool: "mcp__crewlet__get_work_item"},
		activityline.Words(activityline.TagNote, "3 line(s) left out"): {Tag: activityline.TagNote,
			Text: "3 line(s) left out"},
		activityline.Words(activityline.TagSteer, "use the release branch"): {Tag: activityline.TagSteer,
			Text: "use the release branch"},
	} {
		if got := activityline.Parse(line); got != want {
			t.Errorf("%q reads as %+v, want %+v", line, got, want)
		}
	}
}

// A LINE THAT IS NOT THE GRAMMAR IS THE AGENT'S WORDS, whole: a bracket the
// model wrote, a tag this build does not write, or a known tag malformed.
func TestAnythingElseIsTheAgentsWords(t *testing.T) {
	t.Parallel()
	for _, line := range []string{
		"All 363 pass.", "[x] done", "[tool] Bash: go test", "[tool 0] Bash", "[tool 2]", "[tool 2 5ms] Bash",
		"[input] a", "[input 2 5ms] a", "[ok -1]", "[ok 2 fastms]", "[ok 2 3ms 4ms]", "[note 2] a",
		"[steer 9ms] a", "[ok 2", "",
	} {
		if got := activityline.Parse(line); got != (activityline.Line{Tag: activityline.Say, Text: line}) {
			t.Errorf("%q reads as %+v, want the words whole", line, got)
		}
	}
}

// A ONE-LINE SUBJECT WITHIN THE PREVIEW IS ITS HEADER ALONE; a longer one is
// previewed, marked, and kept whole beneath it.
func TestAnInputIsWrittenOnlyWhereThePreviewCutsIt(t *testing.T) {
	t.Parallel()
	if got := activityline.Call(1, "Read", "/repo/main.go"); !slices.Equal(got, []string{"[tool 1] Read: /repo/main.go"}) {
		t.Errorf("a short subject = %q, want its header alone", got)
	}
	long := "gh pr view 131 --json " + strings.Repeat("state,mergedAt,", 20)
	got := activityline.Call(2, "Bash", long)
	head := activityline.Parse(got[0])
	if len(got) != 2 || !strings.HasSuffix(head.Text, "…") || len(got[0]) > len("[tool 2] Bash: ")+activityline.DetailLimit {
		t.Fatalf("a long line = %q; want a cut preview and the line whole beneath it", got)
	}
	if in := activityline.Parse(got[1]); in.Text != long {
		t.Errorf("the input = %q, want %q", in.Text, long)
	}
}

// THE INPUT IS HELD TO ITS BOUND IN WHOLE LINES, so a reader counts what was
// not kept against the header's count rather than reading a cut line as whole.
func TestAnInputPastItsBoundKeepsWholeLines(t *testing.T) {
	t.Parallel()
	var script []string
	for i := range 2000 {
		script = append(script, fmt.Sprintf("print(%d)", i))
	}
	got := activityline.Call(1, "Bash", strings.Join(script, "\n"))
	if head := activityline.Parse(got[0]); head.More != 1999 {
		t.Errorf("the header counts %d more lines, want 1999", head.More)
	}
	size := 0
	for i, l := range got[1:] {
		in := activityline.Parse(l)
		if in.Text != script[i] {
			t.Fatalf("input line %d = %q, want %q", i, in.Text, script[i])
		}
		size += len(in.Text) + 1
	}
	if kept := len(got) - 1; kept >= len(script) || size > activityline.InputLimit+1 {
		t.Errorf("kept %d lines, %d bytes; want fewer than %d lines within %d bytes", kept, size, len(script),
			activityline.InputLimit)
	}
}

// THE PREVIEW AND THE WHOLE INPUT ARE ONE REDACTION: no credential of any shape
// the pass knows reaches either, wherever in a long input it is written.
func TestNoCredentialReachesAPreviewOrAnInput(t *testing.T) {
	t.Parallel()
	for _, shape := range redacttest.Shapes() {
		for name, subject := range map[string]string{
			"on the first line": "curl -H 'Authorization: Bearer " + shape.Sample + "' https://example.com",
			"inside a heredoc":  "python3 - <<'EOF'\nimport os\nkey = '''" + shape.Sample + "'''\nEOF",
			"as an argument":    `{"token":"` + shape.Sample + `","item":"ENG-12"}`,
		} {
			got := strings.Join(activityline.Call(1, "Bash", subject), "\n")
			got += "\n" + activityline.Failed(1, "refused: "+shape.Sample, activityline.Took{})
			if strings.Contains(got, shape.Sample) || containsPiece(got, shape.Sample, redact.Secrets(shape.Sample)) {
				t.Errorf("%s, %s: the lines carry the credential:\n%s", shape.Name, name, got)
			}
		}
	}
}

// containsPiece reports whether text holds 24 bytes of the sample that its own
// redaction takes out: a key's name the pass keeps is no piece of it.
func containsPiece(text, sample, kept string) bool {
	for i := 0; i+24 <= len(sample); i++ {
		if piece := sample[i : i+24]; !strings.Contains(kept, piece) && strings.Contains(text, piece) {
			return true
		}
	}
	return false
}

// THE DASHBOARD KNOWS EVERY TAG, in this build's order: a tag it lacks reads
// as the agent's words on every screen.
func TestTheDashboardReadsEveryTag(t *testing.T) {
	t.Parallel()
	body, err := clientsource.Literal(clientsource.Tree(t), "ACTIVITY_TAGS")
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, tag := range activityline.Tags {
		want = append(want, string(tag))
	}
	if got := clientsource.Strings(body); !slices.Equal(got, want) {
		t.Errorf("ACTIVITY_TAGS = %q, want %q", got, want)
	}
}
