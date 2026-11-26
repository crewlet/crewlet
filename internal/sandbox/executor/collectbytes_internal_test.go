package executor

import (
	"encoding/json"
	"strings"
	"testing"
	"unsafe"

	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/redact/redacttest"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A COLLECTION DECODES A LONG TEXT ONCE, from the bytes as read ([wireAlias]):
// a result line's parse, the stream's assistant line; a result line on the
// stream that succeeded decodes none of it, unparseable output only its end
// ([unparsedFailure]). Each allocates what the text's own decode does, measured
// beside it; a copy of the line or of a raw value is more. BoxReadHeldBytes in
// internal/sandbox counts on it. Bytes allocated, so a copy shows however soon
// it is let go; not parallel, the counts being the process's.
//
// Mutations, each red: decode the result, the event, its message or a block
// with wireDecode; convert the result to a string and back before decoding it;
// keep a failed result's whole line as its event; decode a succeeded result's
// text on the stream; quote unparseable output whole.
func TestACollectionDecodesALongTextOnce(t *testing.T) {
	const size = 8 << 20
	text := strings.Repeat("the harness wrote down what it found\n", size/37)
	quoted, err := json.Marshal(text)
	if err != nil {
		t.Fatal(err)
	}
	result := func(failed string) []byte {
		return []byte(`{"type":"result","subtype":"success","is_error":` + failed + `,"result":` + string(quoted) +
			`,"session_id":"9a8b7c6d-0000-4000-8000-000000000002","usage":{"input_tokens":1,"output_tokens":1}}`)
	}
	succeeded, failed := result("false"), result("true")
	assistant := []byte(`{"type":"assistant","message":{"id":"msg_01","role":"assistant","content":[` +
		`{"type":"text","text":` + string(quoted) + `}],"usage":{"input_tokens":1,"output_tokens":1}},` +
		`"parent_tool_use_id":null}`)
	unparseable := []byte(strings.ReplaceAll(text, "\n", " ") + "\nError: token expired")
	// slack is what a reading allocates beside its text: a decoder, the
	// result's few strings, an unparseable output's end and the sentence
	// around it ([unparsedShownBytes] and its redaction's look back, twice).
	const slack = 4*unparsedShownBytes + 1<<20
	// decode is the text's own decode, in this build: under the race detector a
	// pool drops buffers, and the decode allocates more.
	_, decode := allocationsOf(t, func() { _ = looseString(quoted) })
	// holding is a decoder whose transcript holds the text as it is, so what
	// is counted is the line's decode, not its redaction's stream.
	holding := func() *claudeEvents {
		d := &claudeEvents{}
		d.transcript.hold = 2 * size
		return d
	}
	for _, c := range []struct {
		name    string
		read    func()
		decodes bool
	}{
		{"a result line's parse", func() { _ = ClaudeCode{}.Parse(succeeded) }, true},
		{"a failed result line's parse", func() { _ = ClaudeCode{}.Parse(failed) }, true},
		{"the stream's assistant line", func() { holding().Line(assistant) }, true},
		{"the stream's result line of a run that succeeded", func() { (&claudeEvents{}).Line(succeeded) }, false},
		{"unparseable output's parse", func() { _ = ClaudeCode{}.Parse(unparseable) }, false},
	} {
		limit := uint64(slack)
		if c.decodes {
			limit += decode
		}
		if _, allocated := allocationsOf(t, c.read); allocated > limit {
			t.Errorf("%s allocated %d bytes for a text of %d, past its decode (%d if it decodes it) and %d beside it",
				c.name, allocated, len(text), decode, slack)
		}
	}
}

// THE STREAM'S LAST LINE IS KEPT ONLY WHERE IT STANDS IN FOR THE RESULT FILE:
// an empty or refused file of its own beside the stream, for which
// [Runner.Collect] reads the stream again. Kept on every collection, a 32 MiB
// result line would be held a second time beside the file's own copy. A
// stream read without keeping keeps nothing.
//
// Mutations, each red: keep the line whatever the file held; keep it for a
// harness whose result is its stream; keep it on a read that did not ask.
func TestTheStreamsLastLineIsKeptOnlyWhereItStandsInForTheResultFile(t *testing.T) {
	t.Parallel()
	b := sandbox.NewFakeSandbox("box-1")
	p := PathsFor(b)
	claude, openCode := ClaudeCode{}.Output(p), OpenCode{}.Output(p)
	for _, c := range []struct {
		name    string
		out     Output
		refusal string
		raw     string
		want    bool
	}{
		{"a result file that reads", claude, "", `{"type":"result"}`, false},
		{"an empty result file", claude, "", "", true},
		{"a blank result file", claude, "", " \n", true},
		{"a refused result file", claude, "the result is past 32 MiB", "", true},
		{"a harness whose result is its stream", openCode, "", "", false},
		{"a result file that is the stream", Output{Stdout: p.Stream(), Result: p.Stream(), Events: true}, "", "", false},
		{"a harness that streams no events", Output{Stdout: p.Stream(), Result: p.Result()}, "", "", false},
	} {
		if got := c.out.lastLineStandsIn(c.refusal, []byte(c.raw)); got != c.want {
			t.Errorf("%s: the stream's last line stands in = %t, want %t", c.name, got, c.want)
		}
	}

	runner := NewClaudeCode()
	b.Put(p.Stream(), `{"type":"system"}`+"\n"+`{"type":"result","subtype":"success"}`+"\n")
	for _, keep := range []bool{false, true} {
		stream, err := openStream(t.Context(), b, p.Stream())
		if err != nil {
			t.Fatal(err)
		}
		read, _, err := runner.decodeStream(stream, keep)
		if err != nil {
			t.Fatalf("decodeStream: %v", err)
		}
		if read.last.said() != keep {
			t.Errorf("a stream read keeping its last line %t kept %q", keep, read.last.line)
		}
	}
}

// A FAILED RESULT'S TEXT IS REDACTED ONCE FOR ITS REPORT AND ITS ERROR: Claude
// Code's result text is both where the run failed ([ClaudeCode.Parse]), one
// string, so the collection hands back one redaction for both
// ([secretsOnce]), never a copy each.
//
// Mutation: redact the report and the failure each on its own, or keep the
// report untrimmed, and the two are two strings.
func TestAFailedResultsTextIsRedactedOnceForItsReportAndItsError(t *testing.T) {
	t.Parallel()
	var key string
	for _, s := range redacttest.Shapes() {
		if s.Name == "anthropic-key" {
			key = s.Sample
		}
	}
	runner := NewClaudeCode()
	b := sandbox.NewFakeSandbox("box-1")
	if err := runner.Install(t.Context(), b); err != nil {
		t.Fatalf("Install: %v", err)
	}
	p := PathsFor(b)
	line := `{"type":"result","subtype":"success","is_error":true,"result":"the deploy failed with ` + key + `\n"}`
	b.Put(p.Stream(), line+"\n")
	b.Put(p.Result(), line)
	b.Put(p.ExitCode(), "0")
	res, err := runner.Collect(t.Context(), b, sandbox.RunHandle{}, queue.NewAnswerBudget(nil))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	switch {
	case res.Success || res.Text == "" || res.Text != res.Error:
		t.Fatalf("the collection = success %t, text %q, error %q; want a failure whose report is its error",
			res.Success, res.Text, res.Error)
	case strings.Contains(res.Text, key):
		t.Errorf("the report kept the credential: %q", res.Text)
	case unsafe.StringData(res.Text) != unsafe.StringData(res.Error):
		t.Error("the report and the failure are two redactions of one text")
	}
}
