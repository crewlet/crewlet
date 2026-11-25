package executor

import (
	"fmt"
	"strings"
	"testing"
)

// cursorMembers is a Cursor archive whose conversation holds lines.
func cursorMembers(lines []string) []boundedMember {
	return []boundedMember{{name: cursorArchiveDir + "/s1.jsonl", lines: lines}}
}

// cursorText is a text part of words s, written as JSON.
func cursorText(s string) string { return `{"text":{"text":` + boundedQuoted(s) + `}}` }

// cursorToolCall is a tool call part of tool name, under id, its arguments args
// as written in its argsJson string.
func cursorToolCall(id, name, args string) string {
	return `{"toolCall":{"toolCallId":` + boundedQuoted(id) + `,"toolName":` + boundedQuoted(name) +
		`,"argsJson":` + boundedQuoted(args) + `}}`
}

// cursorPersonVariants are conversation lines written every way a person's
// read on the walk reads apart from a line held whole, past a value bound of
// a kibibyte: each message's parts joined within and past it, blank, null and
// of no shape; a part past it holding blanks alone, a credential across it;
// calls before and after the words, their arguments compacted, past it, of no
// JSON and null, naming tools and ids past it; a result failed, of no call,
// past it and blank; members of another kind than their field's at every
// depth; lines holding two messages; and lines that are no record.
func cursorPersonVariants() []string {
	h := boundedWords()
	q := boundedQuoted
	parts := func(p ...string) string { return `[` + strings.Join(p, ",") + `]` }
	user := func(content string) string { return `{"user":{"content":` + content + `}}` }
	asst := func(content string) string { return `{"assistant":{"content":` + content + `}}` }
	tool := func(more string) string { return `{"tool":{"toolCallId":"c1","toolName":"Shell"` + more + `}}` }
	return append([]string{
		user(parts(cursorText("the brief"))), user(parts(cursorText(h.note + "the note"))),
		user(parts(cursorText("one"), cursorText(" "), cursorText("two"), `null`, `{"text":null}`,
			`{"text":{}}`, `{"text":{"text":null}}`, `{"other":1}`)),
		user(parts(cursorText(h.words), cursorText(h.words+h.secret))), user(parts(cursorText(h.blank))),
		user(parts(cursorText(h.blank), cursorText("after"))), user(parts(cursorText(h.words + h.blank))),
		user(parts(cursorToolCall("c9", "Shell", `{"a":1}`), cursorText("a user's words beside a call"))),
		user(parts(`{"toolCall":{"toolCallId":5}}`)), user(parts(`{"toolCall":{"argsJson":{"a":1}}}`)),
		user(`null`), user(`[]`), user(q("a string")), user(`{"a":1}`), user(parts(`5`)),
		user(parts(`{"text":"words"}`)), user(parts(`{"text":{"text":7}}`)), user(parts(`{"text":[]}`)),
		`{"user":{}}`, `{"user":{"content":[],"other":5}}`, `{"user":null}`, `{"user":5}`, `{"user":[]}`,
		asst(parts(cursorText("the words"), cursorToolCall("c1", "Shell", `{ "command" : "ls" }`))),
		asst(parts(cursorToolCall("c1", "Shell", `{"command":`+q(h.words)+`}`), cursorText(h.words))),
		asst(parts(cursorText(" \n "), cursorToolCall("c1", "Read", `not json`), cursorToolCall("c2", "Edit", ``))),
		asst(parts(`{"toolCall":{"toolCallId":"c1","toolName":"Shell","argsJson":null}}`, `{"toolCall":null}`,
			`{"toolCall":{}}`)),
		asst(parts(cursorToolCall("c1", strings.Repeat("t", 300), `[1]`), cursorToolCall("c2", h.words, `{}`))),
		asst(parts(cursorToolCall(strings.Repeat("i", 300), "Shell", `{}`))),
		asst(parts(cursorText("both"), `{"text":{"text":"in one part"},"toolCall":{"toolCallId":"c3",`+
			`"toolName":"Grep","argsJson":"{\"p\":1}"}}`)),
		asst(parts(cursorToolCall("c1", "Shell", strings.Repeat(" ", 2000)+`{"a":1}`))),
		asst(parts(cursorToolCall("c1", "Shell", `{"k":"`+h.secret+`"}`))),
		asst(parts(cursorToolCall("c1", "Shell", `{}`), `{"toolCall":{"toolCallId":"c2","toolName":5}}`)),
		asst(parts(cursorText("before"), `{"toolCall":{"argsJson":7}}`)), asst(`null`), asst(`"x"`),
		`{"assistant":{"content":[],"model":{"a":[1,2]}}}`, `{"assistant":null}`, `{"assistant":true}`,
		tool(`,"content":` + parts(cursorText("the result")) + `,"isError":true`),
		tool(`,"content":` + parts(cursorText(h.words+h.secret), cursorText("more"))),
		tool(`,"content":` + parts(cursorText(h.blank))), tool(`,"content":` + parts(cursorText(" "))),
		tool(`,"content":null,"isError":null`), tool(``), tool(`,"isError":"yes"`), tool(`,"isError":1`),
		tool(`,"content":` + parts(cursorToolCall("c5", "x", `{}`), cursorText("a result's call is no call"))),
		`{"tool":{"toolCallId":"c9","content":[{"text":{"text":"of no call"}}]}}`,
		`{"tool":{"toolCallId":` + q(strings.Repeat("i", 300)) + `,"content":[]}}`,
		`{"tool":{"toolCallId":5}}`, `{"tool":{"toolName":["x"]}}`, `{"tool":{}}`, `{"tool":null}`, `{"tool":0}`,
		`{"assistant":{"content":[{"text":{"text":"the assistant's"}}]},"user":{"content":[{"text":{"text":` +
			`"the user's wins"}}]}}`,
		`{"tool":{"toolCallId":"c1","content":[]},"assistant":{"content":[{"text":{"text":"the assistant's"}}]}}`,
		`{"user":null,"tool":{"toolCallId":"c1","content":[{"text":{"text":"the tool's"}}]}}`,
		`{"user":{"content":[{"text":{"text":"x"}}]},"tool":{"isError":"no"}}`,
		`{}`, `{"other":{"user":{}}}`, `{"user":{"content":[{"text":{"text":"x"}}]}} trailing`,
	}, boundedLines()...)
}

// A PERSON'S READ OF A CURSOR CONVERSATION ON THE WALK IS ITS WHOLE-LINE
// READ'S: over every variant, alone, between a brief and a call, all together
// and all together with the last line torn, and over the recorded store's
// export, the read on the walk, whole and in pieces of seventeen bytes,
// answers the page and the error the whole-line oracle answers
// ([wholeCursor]) under every bounded read.
//
// Mutation: hold an entry's text a byte short, drop a call's arguments, lose
// a result's failure, keep a blank part, place a call before the words'
// entry, read a line of a member of another kind as a record, or leave a
// torn last line unread: a variant fails.
func TestAPersonsReadOfACursorConversationIsItsWholeLineRead(t *testing.T) {
	t.Parallel()
	reads := boundedReads()
	check := func(name string, archive []byte) {
		for _, r := range reads {
			want := personReadOf(t, wholeCursor{}, archive, r)
			if got := personReadOf(t, CursorAgent{}, archive, r); got != want {
				t.Errorf("%s read %+v:\n%s", name, r, firstApart(got, want))
			}
			if got := personReadOf(t, piecesCursor{n: 17}, archive, r); got != want {
				t.Errorf("%s read %+v in pieces:\n%s", name, r, firstApart(got, want))
			}
		}
	}
	brief := `{"user":{"content":[{"text":{"text":"the brief"}}]}}`
	call := `{"assistant":{"content":[` + cursorToolCall("c1", "Shell", `{"command":"ls"}`) + `]}}`
	variants := cursorPersonVariants()
	for i, v := range variants {
		check(fmt.Sprintf("variant %d %.120q", i, v), boundedArchiveOf(t, cursorMembers([]string{brief, call, v,
			brief})...))
		torn := cursorMembers([]string{brief, call, v})
		torn[0].torn = true
		check(fmt.Sprintf("variant %d torn %.120q", i, v), boundedArchiveOf(t, torn...))
	}
	check("every variant", boundedArchiveOf(t, cursorMembers(variants)...))
	torn := cursorMembers(variants)
	torn[0].torn = true
	check("every variant, torn", boundedArchiveOf(t, torn...))
	lines, code, stderr := cursorExport(t, "plain")
	if code != 0 {
		t.Fatalf("the export refused a store of the measured release (exit %d): %s", code, stderr)
	}
	check("the plain store's export", boundedArchiveOf(t, cursorMembers(strings.Split(strings.TrimSuffix(
		string(lines), "\n"), "\n"))...))
}

// A TORN LAST CURSOR LINE IS READ AS A LINE: the take keeps a conversation as
// it was first read, so a last line the export left with no break, whole or
// cut inside its JSON, is read as one, the whole one an entry and the cut one
// said as no record ([lineWalk.member]).
//
// Mutation: leave a torn last line unread, or read a cut one as a record: a
// case fails.
func TestATornLastCursorLineIsReadAsALine(t *testing.T) {
	t.Parallel()
	r := personRead{bounds: TranscriptReadBounds(), valueMax: TranscriptValueMaxBytes}
	for _, c := range []struct {
		last, holds string
	}{
		{last: `{"user":{"content":[{"text":{"text":"the torn brief"}}]}}`, holds: `"text":"the torn brief"`},
		{last: `{"user":{"content":[{"text":{"text":"cut`, holds: "is not a record"},
	} {
		members := cursorMembers([]string{`{"user":{"content":[{"text":{"text":"first"}}]}}`, c.last})
		members[0].torn = true
		if got := personReadOf(t, CursorAgent{}, boundedArchiveOf(t, members...), r); !strings.Contains(got, c.holds) {
			t.Errorf("a torn last line %.60q: read %.400s, want it holding %q", c.last, got, c.holds)
		}
	}
}

// A CURSOR KEY WRITTEN TWICE IS READ BY ITS LAST, AND ONE IN ANOTHER CASE IS
// NONE: the walk reads a key by its exact name, a value written again
// replacing the first whole, as Cursor's own JSON.parse reads the history it
// is handed ([fieldKind]), where the whole-line decode matched any case and
// merged an object written twice. A part's text written again past the value
// bound loses the join, said in its place (R2). The export writes neither.
//
// Mutation: merge a message, a part or a call written twice, keep a text
// written before null, match a key in another case, or keep a part past the
// bound written twice: a case fails.
func TestACursorKeyWrittenTwiceIsReadByItsLast(t *testing.T) {
	t.Parallel()
	h := boundedWords()
	r := personRead{bounds: TranscriptReadBounds(), valueMax: 1 << 10}
	user := func(s string) string { return `{"user":{"content":[{"text":{"text":"` + s + `"}}]}}` }
	asst := func(parts string) string { return `{"assistant":{"content":[` + parts + `]}}` }
	for _, c := range []struct {
		name, line   string
		holds, never []string
	}{
		{"a message written twice", `{"user":{"content":[{"text":{"text":"first"}}]},"user":{"content":[{"text":` +
			`{"text":"last"}}]}}`, []string{`"kind":"brief","text":"last"`}, []string{"first"}},
		{"a message written again as null", strings.TrimSuffix(user("first"), "}") + `,"user":null,"tool":` +
			`{"toolCallId":"c1","content":[{"text":{"text":"the result"}}]}}`,
			[]string{`"kind":"tool_result"`, `"text":"the result"`}, []string{"first"}},
		{"a message written again empty", strings.TrimSuffix(user("first"), "}") + `,"user":{}}`,
			[]string{`"Entries":[]`}, []string{"first", "not a record"}},
		{"a content written twice", `{"assistant":{"content":[` + cursorToolCall("c1", "First", `{}`) + `],"content":[` +
			cursorText("last") + `]}}`, []string{`"kind":"model","text":"last"`}, []string{"First", "tool_call"}},
		{"a part's text written twice", asst(`{"text":{"text":"first"},"text":{"text":"last"}}`),
			[]string{`"text":"last"`}, []string{"first"}},
		{"a part's text written again as null", asst(`{"text":{"text":"first"},"text":null}`),
			[]string{`"Entries":[]`}, []string{"first"}},
		{"a text's words written twice", asst(`{"text":{"text":"first","text":"last"}}`),
			[]string{`"text":"last"`}, []string{"first"}},
		{"a call written twice", asst(`{"toolCall":{"toolCallId":"c1","toolName":"First","argsJson":"{}"},` +
			`"toolCall":{"toolCallId":"c2","toolName":"Last"}}`),
			[]string{`"kind":"tool_call","tool":"Last","text":""`}, []string{"First"}},
		{"a call's arguments written twice", asst(`{"toolCall":{"toolCallId":"c1","toolName":"Shell",` +
			`"argsJson":"{\"first\":1}","argsJson":"{\"last\":2}"}}`), []string{`last`}, []string{"first"}},
		{"a failure written again as null", `{"tool":{"toolCallId":"c1","isError":true,"isError":null,` +
			`"content":[]}}`, []string{`"kind":"tool_result"`}, []string{`"failed"`}},
		{"a message in another case", `{"User":{"content":[{"text":{"text":"cased"}}]}}`,
			[]string{"not a record"}, []string{"cased"}},
		{"a content in another case", `{"user":{"Content":[{"text":{"text":"cased"}}]}}`,
			[]string{`"Entries":[]`}, []string{"cased", "not a record"}},
		{"a part's text past the bound written twice", asst(`{"text":{"text":` + boundedQuoted(h.words) +
			`},"text":{"text":"last"}}`), []string{"in a part written out of order"}, []string{"words of the model"}},
	} {
		got := personReadOf(t, CursorAgent{}, boundedArchiveOf(t, cursorMembers([]string{c.line})...), r)
		for _, s := range c.holds {
			if !strings.Contains(got, s) {
				t.Errorf("%s: read %.500s, want it holding %q", c.name, got, s)
			}
		}
		for _, s := range c.never {
			if strings.Contains(got, s) {
				t.Errorf("%s: read %.500s, want no %q", c.name, got, s)
			}
		}
	}
}

// A CURSOR CALL'S ARGUMENTS ARE COMPACTED WITHIN THE BOUND AND SHOWN AS
// WRITTEN PAST IT (R3): a call's argsJson string of spaced JSON is its
// compaction within the value bound, as the whole-line read compacted it, and
// past the bound its text as written, never held to be compacted.
//
// Mutation: compact past the bound, or leave the arguments as written within
// it: a case fails.
func TestACursorCallsArgumentsAreCompactedWithinTheBound(t *testing.T) {
	t.Parallel()
	r := personRead{bounds: TranscriptReadBounds(), valueMax: 1 << 10}
	for _, c := range []struct {
		args, holds string
	}{
		{args: `{ "command" : "ls" }`, holds: `"text":"{\"command\":\"ls\"}"`},
		{args: `{ "pad" : "` + strings.Repeat("x", 1100) + `" }`, holds: `"text":"{ \"pad\" : \"xxx`},
	} {
		line := `{"assistant":{"content":[` + cursorToolCall("c1", "Shell", c.args) + `]}}`
		got := personReadOf(t, CursorAgent{}, boundedArchiveOf(t, cursorMembers([]string{line})...), r)
		if !strings.Contains(got, c.holds) || !strings.Contains(got, `"kind":"tool_call"`) {
			t.Errorf("arguments %.40q: read %.400s, want a call holding %q", c.args, got, c.holds)
		}
	}
}
