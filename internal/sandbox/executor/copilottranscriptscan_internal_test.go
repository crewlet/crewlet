package executor

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// copilotMembers is a Copilot archive whose session's events.jsonl holds lines.
func copilotMembers(lines []string) []boundedMember {
	return []boundedMember{{name: copilotSessionState + "/s1/events.jsonl", lines: lines}}
}

// copilotPersonVariants are events.jsonl lines written every way a person's
// read on the walk reads apart from a line held whole, past a value bound of
// a kibibyte: a user message within and past it, opening with a heading, a
// subagent's; an assistant message's words and tool requests in either order,
// past it, blank, of no shape and naming tools past it; a completion's result
// and failure of every kind, within and past it; a line read for its type
// alone where its data's content is of another kind (an assistant's beside
// tool requests, a completion's beside its result, a subagent's before a line
// that is no record, which takes the main thread); a compaction's summary read
// by its own decode beside members of another kind; and lines that are no
// record.
func copilotPersonVariants() []string {
	h := boundedWords()
	q := boundedQuoted
	const stamp = `"timestamp":"2026-01-01T00:00:00.000Z"`
	line := func(kind, data string) string { return `{"type":"` + kind + `",` + stamp + `,"data":{` + data + `}}` }
	um := func(content string) string { return line("user.message", `"content":`+content) }
	am := func(data string) string { return line("assistant.message", data) }
	tc := func(data string) string { return line("tool.execution_complete", `"toolCallId":"c1",`+data) }
	req := func(name, args string) string {
		return `{"toolCallId":"c1","name":` + q(name) + `,"arguments":` + args + `}`
	}
	return append([]string{
		um(q("the brief")), um(q(h.note + "the note")), um(q(h.note + h.words)), um(q(h.words + h.secret)),
		um(q(h.blank)), um(`null`), um(`5`),
		`{"type":"user.message","agentId":"agent-7",` + stamp + `,"data":{"content":"a subagent's brief"}}`,
		`{"type":"user.message","agentId":` + q(strings.Repeat("a", 300)) + `,"data":{"content":"a long agent"}}`,
		am(`"content":"the words","toolRequests":[` + req("bash", `{"command":"ls"}`) + `]`),
		am(`"toolRequests":[` + req("bash", `{"command":`+q(h.words)+`}`) + `,` + req("read", `"a string"`) +
			`],"content":` + q(h.words)),
		am(`"content":" \n ","toolRequests":[` + req("bash", `[1,2]`) + `,null]`),
		am(`"content":"words","toolRequests":null`), am(`"toolRequests":[]`),
		am(`"content":"before","toolRequests":"no list"`), am(`"toolRequests":{"a":1}`),
		am(`"toolRequests":[{"toolCallId":5}]`), am(`"toolRequests":[` + req("bash", `{}`) + `,7]`),
		am(`"toolRequests":[{"toolCallId":"c2"},{"name":"x","arguments":null}]`),
		am(`"toolRequests":[` + req(strings.Repeat("t", 300), `12`) + `,` + req(h.words, `{}`) + `]`),
		am(`"content":"fallback","turns":"x","toolRequests":[` + req("bash", `{}`) + `]`),
		tc(`"success":true,"result":{"content":"the result"}`),
		tc(`"success":true,"result":{"content":` + q(h.words+h.secret) + `}`),
		tc(`"success":false,"error":"failed words"`),
		tc(`"error":{"message":"the message","code":"denied"}`),
		tc(`"error":{"message":" ","code":"denied"}`),
		tc(`"error":{"code":"denied","detail":` + q(h.words) + `}`),
		tc(`"error":{"message":` + q(h.words) + `}`),
		tc(`"error":{"message":5,"code":"x"}`),
		tc(`"error":[1,"two"]`), tc(`"error":42`), tc(`"error":null`), tc(`"success":"true"`),
		tc(`"result":{"content":"ignored"},"success":false`),
		line("tool.execution_complete", `"toolCallId":"c1","success":true,"result":"x"`),
		am(`"content":5,"toolRequests":[` + req("bash", `{"a":1}`) + `]`),
		tc(`"content":[1],"success":true,"result":{"content":"the result"}`),
		`{"type":"user.message","agentId":"agent-7",` + stamp + `,"data":{"content":{"a":true}}}` + "\n" +
			`not a record`,
		line("session.compaction_complete", `"summaryContent":"the summary"`),
		line("session.compaction_complete", `"summaryContent":`+q(h.words)),
		line("session.compaction_complete", `"summaryContent":`+q(h.blank)),
		line("session.compaction_complete", `"summaryContent":5`),
		line("session.compaction_complete", `"turns":"x","summaryContent":"read by its own decode"`),
		`{"type":"session.compaction_complete","data":"x"}`,
		`{"type":"session.compaction_complete","agentId":5,"data":{"summaryContent":"no agent"}}`,
		line("session.start", `"sessionId":7`), line("hook.end", `"usage":{"inputTokens":"x"}`),
		line("assistant.message", `"usage":{"inputTokens":1,"maxPromptTokens":2},"aborted":true,"content":"x"`),
		`{"type":"user.message","timestamp":5,"data":{"content":"x"}}`,
		`{"data":{"content":"no type"}}`, `{"type":5}`, `{"type":""}`, `{"type":"result","exitCode":0}`,
		`{"data":{"content":"typed last"},"type":"user.message"}`,
	}, boundedLines()...)
}

// A PERSON'S READ OF A COPILOT SESSION ON THE WALK IS ITS WHOLE-LINE READ'S:
// over every variant, alone, between a brief and a call, and all together, and
// over every fixture, the read on the walk, whole and in pieces of seventeen
// bytes, answers the page and the error the whole-line oracle answers
// ([wholeCopilot]) under every bounded read.
//
// Mutation: read a line of another kind's member as whole (its content's
// among them), place the calls before the words' entry, take a failure's
// blank message, or split a brief past the bound: a variant fails.
func TestAPersonsReadOfACopilotSessionIsItsWholeLineRead(t *testing.T) {
	t.Parallel()
	reads := boundedReads()
	check := func(name string, archive []byte) {
		for _, r := range reads {
			want := personReadOf(t, wholeCopilot{}, archive, r)
			if got := personReadOf(t, Copilot{}, archive, r); got != want {
				t.Errorf("%s read %+v:\n%s", name, r, firstApart(got, want))
			}
			if got := personReadOf(t, piecesCopilot{n: 17}, archive, r); got != want {
				t.Errorf("%s read %+v in pieces:\n%s", name, r, firstApart(got, want))
			}
		}
	}
	brief := `{"type":"user.message","data":{"content":"the brief"}}`
	call := `{"type":"assistant.message","data":{"toolRequests":[{"toolCallId":"c1","name":"bash"}]}}`
	variants := copilotPersonVariants()
	for i, v := range variants {
		check(fmt.Sprintf("variant %d %.120q", i, v), boundedArchiveOf(t, copilotMembers([]string{brief, call, v,
			brief})...))
	}
	check("every variant", boundedArchiveOf(t, copilotMembers(variants)...))
	for _, fixture := range copilotEventsFixtures(t) {
		check(fixture, boundedArchiveOf(t, copilotMembers(strings.Split(strings.TrimSuffix(
			copilotFixtureAt(t, fixture), "\n"), "\n"))...))
	}
}

// A COLLECTION'S READ OF A COPILOT SESSION ON THE WALK IS ITS WHOLE-LINE
// READ'S: over every fixture, every person's variant and the lines that pick a
// note's receipt apart (a subagent's, a hook's end bare and not, a request's
// duration and a compaction's count of every kind, members of another kind),
// each between a note and its request and torn after them, read whole and in
// pieces of seventeen bytes, it holds the notes the whole-line read holds.
//
// Mutation: read a hook's end with more than its id, kind and success as
// bare, or count a line past the bound out of the window: a case fails.
func TestACollectionsReadOfACopilotSessionIsItsWholeLineRead(t *testing.T) {
	t.Parallel()
	const at = `"timestamp":"2026-01-01T00:00:00.000Z"`
	note := `{"type":"user.message",` + at + `,"data":{"clientTurnId":"` + CopilotClientTurnID("one") +
		`","delivery":"steering","content":"x"}}`
	usage := func(data string) string {
		return `{"type":"session.usage_record","timestamp":"2026-01-01T00:00:01.000Z","data":{"usage":` + data + `}}`
	}
	request := usage(`{"interactionType":"` + copilotMainRequest + `","duration":500}`)
	records := map[string]string{}
	for _, fixture := range copilotEventsFixtures(t) {
		records[fixture] = copilotFixtureAt(t, fixture)
	}
	lines := append(copilotPersonVariants(),
		`{"type":"hook.end",`+at+`,"data":{"hookInvocationId":"h","hookType":"x","success":true}}`,
		`{"type":"hook.end",`+at+`,"data":{"hookInvocationId":"h","output":"handed"}}`,
		`{"type":"hook.end",`+at+`,"data":null}`, `{"type":"hook.end",`+at+`}`,
		`{"type":"hook.end",`+at+`,"data":{"hookInvocationId":5}}`,
		`{"type":"subagent.started","agentId":"agent-1",`+at+`}`,
		`{"type":"assistant.message","agentId":"agent-1",`+at+`,"data":{"content":"x"}}`,
		`{"type":"session.compaction_start",`+at+`,"data":{"retainedInputMessages":1}}`,
		`{"type":"session.compaction_start",`+at+`,"data":{"retainedInputMessages":"1"}}`,
		`{"type":"session.compaction_start",`+at+`,"data":{"retainedInputMessages":1.5}}`,
		`{"type":"session.compaction_complete",`+at+`,"data":{"success":false}}`,
		`{"type":"session.compaction_complete",`+at+`,"data":{"success":"false"}}`,
		`{"type":"session.compaction_complete",`+at+`,"data":{"success":true}}`,
		usage(`{"interactionType":"`+copilotMainRequest+`","duration":"500"}`),
		usage(`{"interactionType":"`+copilotMainRequest+`","duration":-5}`),
		usage(`{"interactionType":"`+copilotMainRequest+`","duration":2.5e3}`),
		usage(`{"interactionType":"conversation-subagent","duration":500}`),
		usage(`{"interactionType":5}`), usage(`7`),
		`{"type":"user.message",`+at+`,"data":{"clientTurnId":"`+CopilotClientTurnID("one")+`","delivery":5}}`,
		`{"type":"user.message","timestamp":"no time","data":{"clientTurnId":"`+CopilotClientTurnID("one")+
			`","delivery":"queued"}}`)
	for i, l := range lines {
		records[fmt.Sprintf("line %d %.80q", i, l)] = note + "\n" + l + "\n" + request + "\n"
		records[fmt.Sprintf("torn after line %d", i)] = note + "\n" + request + "\n" + l
	}
	records["every line"] = note + "\n" + strings.Join(lines, "\n") + "\n" + request + "\n"
	// A hook's end counts in a compaction's window only where it handed the
	// model something.
	for _, hook := range []string{`{"hookInvocationId":"h","hookType":"x","success":true}`,
		`{"hookInvocationId":"h","output":"handed"}`} {
		records["a compaction after a hook's end "+hook] = note + "\n" + `{"type":"hook.end",` + at + `,"data":` +
			hook + "}\n" + `{"type":"session.compaction_start",` + at + `,"data":{"retainedInputMessages":1}}` +
			"\n" + `{"type":"session.compaction_complete",` + at + `,"data":{"success":true}}` + "\n" + request + "\n"
	}
	for name, record := range records {
		takes := served(append(copilotInboxesIn(record), "never")...)
		want, wantErr := copilotNotesHeldWhole(t.Context(), strings.NewReader(record), takes)
		got, err := copilotNotesHeld(t.Context(), strings.NewReader(record), takes)
		if !slices.Equal(got, want) || (err != nil) != (wantErr != nil) {
			t.Errorf("%s: held %v (%v), want %v (%v)", name, got, err, want, wantErr)
		}
		got, err = copilotNotesHeld(t.Context(), &exportPieces{r: strings.NewReader(record), n: 17}, takes)
		if !slices.Equal(got, want) || (err != nil) != (wantErr != nil) {
			t.Errorf("%s in pieces: held %v (%v), want %v (%v)", name, got, err, want, wantErr)
		}
	}
}

// copilotEventsFixtures is every recorded events.jsonl's name.
func copilotEventsFixtures(t *testing.T) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("testdata", "copilot", "*", "*.events.jsonl"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no events.jsonl fixtures (%v)", err)
	}
	return paths
}

// copilotFixtureAt is the fixture at path.
func copilotFixtureAt(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// copilotInboxesIn is every inbox id a record names a note under.
func copilotInboxesIn(record string) []string {
	var out []string
	for _, m := range regexp.MustCompile(regexp.QuoteMeta(copilotTurnPrefix)+`([A-Za-z0-9_-]+)`).
		FindAllStringSubmatch(record, -1) {
		if !slices.Contains(out, m[1]) {
			out = append(out, m[1])
		}
	}
	return out
}
