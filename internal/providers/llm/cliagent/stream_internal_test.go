package cliagent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent/cliprofile"
)

// claudeStreamFixture is a Claude Code 2.1.310 stream-json run of one text
// mode round, written by hand from the S3 measurement (system events, the
// stream_event deltas of a thinking block and a text block, the whole
// assistant message, a rate_limit_event and the result) with no model called.
// Its text block spells a fenced envelope, cut inside an escape, inside a
// `\u` escape and between the halves of a surrogate pair.
const claudeStreamFixture = "testdata/claude-code-stream-json.jsonl"

// fixtureMessage and fixtureQuery are what the fixture's envelope says.
const (
	fixtureMessage = "The sea stays salty because rivers carry dissolved minerals into it, and " +
		"evaporation leaves the salt behind \U0001f30a. Say \"more\" for the sources."
	fixtureQuery = `why is the sea "salty"`
)

// claudeStreamProvider is a fake CLI read through the SHIPPED claude-code
// profile's answer paths and streamed variant: it prints FAKE_STREAM_STDOUT
// when the call's argv carries the variant's arguments and FAKE_STDOUT when
// it does not.
func claudeStreamProvider(t *testing.T, stream, unary string) *Provider {
	t.Helper()
	builtin, ok := cliprofile.Builtin("claude-code")
	if !ok || builtin.Stream == nil {
		t.Fatal("the claude-code profile has no streamed variant")
	}
	p := fakeProvider(t, map[string]string{
		"FAKE_STREAM_ARG":    "--include-partial-messages",
		"FAKE_STREAM_STDOUT": stream,
		"FAKE_STDOUT":        unary,
	}, map[string]any{"output": "json", "text_paths": []any{[]any{"result"}}})
	p.profile.TextPaths = builtin.TextPaths
	p.profile.ErrorPaths = builtin.ErrorPaths
	p.profile.Usage = builtin.Usage
	p.profile.Stream = builtin.Stream
	if err := p.profile.Validate("claude-code"); err != nil {
		t.Fatal(err)
	}
	p.streamed = p.profile.Streamed()
	return p
}

func readFixture(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(claudeStreamFixture))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// toolRequest is a round that offers a tool, so the reply is an envelope.
func toolRequest(onDelta func(llm.Delta)) llm.Request {
	return llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "Why is the sea salty?"}},
		Tools:    []llm.ToolDef{{Name: "search_knowledge", Description: "Search the knowledge base."}},
		OnDelta:  onDelta,
	}
}

// recorder keeps every delta a call sent, and what a reader shown them would
// hold: appended, and emptied on a retraction.
type recorder struct {
	deltas  []llm.Delta
	shown   string
	retract int
}

func (r *recorder) on(d llm.Delta) {
	r.deltas = append(r.deltas, d)
	if d.Retract {
		r.retract++
		r.shown = ""
		return
	}
	r.shown += d.Content
}

// A WATCHED CLAUDE CODE ROUND SHOWS ITS PROSE AS IT IS WRITTEN, AND ONLY ITS
// PROSE. The fixture's fragments are envelope JSON; what reaches the watcher
// is the decoded message, in several deltas rather than one at the end, with
// nothing of the thinking block, the tool call or the envelope's syntax, and
// it ends as exactly the content the round returns, with nothing retracted.
func TestAWatchedClaudeCodeRoundShowsItsProseAsItIsWritten(t *testing.T) {
	t.Parallel()
	p := claudeStreamProvider(t, readFixture(t), `{"result":"the unary answer"}`)
	var rec recorder
	comp, err := ask(t, p, toolRequest(rec.on))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if comp.Content != fixtureMessage {
		t.Fatalf("content = %q, want the envelope's message", comp.Content)
	}
	if len(comp.ToolCalls) != 1 || comp.ToolCalls[0].Name != "search_knowledge" ||
		comp.ToolCalls[0].Arguments["query"] != fixtureQuery {
		t.Errorf("tool calls = %+v, want the one search the envelope asks for", comp.ToolCalls)
	}
	if comp.InputTokens != 12+14610 || comp.OutputTokens != 96 || comp.CacheRead != 14610 {
		t.Errorf("usage in %d, out %d, cache read %d: not read from the result event",
			comp.InputTokens, comp.OutputTokens, comp.CacheRead)
	}
	if rec.retract != 0 {
		t.Errorf("a well-formed round was retracted: %+v", rec.deltas)
	}
	if rec.shown != comp.Content {
		t.Errorf("the watcher was shown %q, want exactly the content %q", rec.shown, comp.Content)
	}
	prose := 0
	for _, d := range rec.deltas {
		if d.Content != "" {
			prose++
		}
		for _, leak := range []string{"tool_calls", "search_knowledge", "not this", "{", "```", "salt; I should"} {
			if strings.Contains(d.Content, leak) {
				t.Errorf("delta %q shows %q, which is not the message's prose", d.Content, leak)
			}
		}
	}
	if prose < 4 {
		t.Errorf("%d deltas carried prose; the answer arrived whole instead of as it was written", prose)
	}
}

// A CALL NOBODY WATCHES RUNS THE PROFILE AS WRITTEN. The variant's argv is for
// a streamed call only, so the summariser and the judges keep the single
// document they always read.
func TestAnUnwatchedCallRunsTheProfileAsWritten(t *testing.T) {
	t.Parallel()
	p := claudeStreamProvider(t, readFixture(t), `{"result":"the unary answer","is_error":false}`)
	comp, err := ask(t, p, llm.Request{})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if comp.Content != "the unary answer" {
		t.Errorf("content = %q: an unwatched call ran the streamed argv", comp.Content)
	}
}

// A VIEW THE PARSER OVERRULES IS RETRACTED. The stream follows the first
// message synonym it meets; the parser prefers `message` over `content`. When
// they disagree the watcher is told to discard what it was shown, and what
// follows is the answer whole, so the view still ends as the content exactly.
func TestAViewTheParserOverrulesIsRetracted(t *testing.T) {
	t.Parallel()
	reply := `{"content": "the first reading", "message": "the answer", "tool_calls": []}`
	p := claudeStreamProvider(t, claudeStream(t, reply, 7), "")
	var rec recorder
	comp, err := ask(t, p, toolRequest(rec.on))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if comp.Content != "the answer" {
		t.Fatalf("content = %q, want the parser's reading", comp.Content)
	}
	if rec.retract != 1 {
		t.Errorf("retractions = %d in %+v, want the one that discards the first reading", rec.retract, rec.deltas)
	}
	if rec.shown != comp.Content {
		t.Errorf("after the retraction the watcher holds %q, want %q", rec.shown, comp.Content)
	}
}

// A REPLY THAT IS NOT AN ENVELOPE SHOWS NOTHING UNTIL IT IS READ. Prose before
// the object is a shape the stream does not follow, so nothing is guessed at
// while it arrives, and the parser's answer arrives in one delta at the end.
func TestAReplyThatIsNotAnEnvelopeArrivesWholeAtTheEnd(t *testing.T) {
	t.Parallel()
	reply := "Here you are:\n```json\n{\"message\": \"in the fence\", \"tool_calls\": []}\n```"
	p := claudeStreamProvider(t, claudeStream(t, reply, 5), "")
	var rec recorder
	comp, err := ask(t, p, toolRequest(rec.on))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if comp.Content != "in the fence" || rec.shown != comp.Content || rec.retract != 0 {
		t.Errorf("content %q, shown %q, retractions %d", comp.Content, rec.shown, rec.retract)
	}
	if len(rec.deltas) != 1 {
		t.Errorf("deltas = %+v, want the answer in one at the end", rec.deltas)
	}
}

// A PLAIN STREAMED CALL IS SHOWN AS ITS ANSWER IS TRIMMED. With no tools there
// is no envelope, and the whitespace the answer is trimmed of is never shown,
// so nothing has to be retracted at the end.
func TestAPlainStreamedCallIsShownAsItsAnswerIsTrimmed(t *testing.T) {
	t.Parallel()
	p := claudeStreamProvider(t, claudeStream(t, "\n  The sea\n stays salty. \n\n", 4), "")
	var rec recorder
	comp, err := ask(t, p, llm.Request{OnDelta: rec.on})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if comp.Content != "The sea\n stays salty." || rec.shown != comp.Content || rec.retract != 0 {
		t.Errorf("content %q, shown %q, retractions %d", comp.Content, rec.shown, rec.retract)
	}
}

// A JSONL CLI SHOWS THE ENVELOPE'S PROSE, NOT ITS JSON. Codex delivers its
// reply as whole item events, so a tool round's envelope arrived on the live
// view as raw JSON; read through the same stream it is the message, in the one
// delta its event carries.
func TestAJSONLRoundShowsTheEnvelopesProseNotItsJSON(t *testing.T) {
	t.Parallel()
	envelope := `{"message": "Looking it up.", "tool_calls": [{"name": "search_knowledge", "arguments": {"query": "salt"}}]}`
	line, err := jsonLine(map[string]any{"type": "item.completed",
		"item": map[string]any{"type": "agent_message", "text": envelope}})
	if err != nil {
		t.Fatal(err)
	}
	p := fakeProvider(t, map[string]string{"FAKE_STDOUT": line + "\n"}, map[string]any{
		"output": "jsonl", "text_paths": []any{[]any{"item", "text"}},
	})
	var rec recorder
	comp, err := ask(t, p, toolRequest(rec.on))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if comp.Content != "Looking it up." || len(comp.ToolCalls) != 1 {
		t.Fatalf("content %q, calls %+v", comp.Content, comp.ToolCalls)
	}
	if rec.shown != comp.Content || rec.retract != 0 {
		t.Errorf("shown %q with %d retractions, want the message alone", rec.shown, rec.retract)
	}
	for _, d := range rec.deltas {
		if strings.Contains(d.Content, "tool_calls") {
			t.Errorf("delta %q is envelope JSON", d.Content)
		}
	}
}

// claudeStream renders reply as a minimal Claude Code stream-json run: its
// text cut into `parts` fragments as text_delta events, then the result event
// carrying the whole of it.
func claudeStream(t *testing.T, reply string, parts int) string {
	t.Helper()
	var lines []string
	add := func(doc map[string]any) {
		line, err := jsonLine(doc)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, line)
	}
	add(map[string]any{"type": "system", "subtype": "init"})
	size := max(1, len(reply)/parts)
	for from := 0; from < len(reply); from += size {
		to := min(len(reply), from+size)
		add(map[string]any{"type": "stream_event", "event": map[string]any{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "text_delta", "text": reply[from:to]}}})
	}
	add(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": reply,
		"usage": map[string]any{"input_tokens": 10, "output_tokens": 5}})
	return strings.Join(lines, "\n") + "\n"
}

// jsonLine encodes one stream event.
func jsonLine(doc map[string]any) (string, error) {
	raw, err := json.Marshal(doc)
	return string(raw), err
}

// THE WEB PROBE RUNS THE ARGV THE ASSISTANT RUNS. Every assistant answer is
// streamed, so on a profile with a streamed variant the probe must prove the
// web denied on that variant's argv. Here only the streamed argv reaches the
// web: a probe that ran the profile as written would answer NO-WEB and pass a
// denial no assistant call runs under.
func TestTheWebProbeRunsTheStreamedArgv(t *testing.T) {
	t.Parallel()
	p := claudeStreamProvider(t, claudeStream(t, "ts="+nowEpoch(), 2), `{"result":"NO-WEB"}`)
	p.profile.WebTools = cliprofile.WebToolsDenied
	p.streamed.WebTools = cliprofile.WebToolsDenied
	verdict, problem := p.webProbe(t.Context())
	if !strings.Contains(verdict, "FETCHED") || problem == "" {
		t.Errorf("verdict %q, problem %q: the probe did not run the streamed argv", verdict, problem)
	}
}
