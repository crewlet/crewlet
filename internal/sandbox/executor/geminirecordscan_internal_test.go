package executor

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// geminiMembers is a Gemini CLI archive's chat file holding lines.
func geminiMembers(lines []string) []boundedMember {
	return []boundedMember{{name: "chats/session-x.jsonl", lines: lines}}
}

// geminiSessionLine is a session line of typ under id whose content is content,
// more members after it.
func geminiSessionLine(typ, id, content, more string) string {
	return `{"id":"` + id + `","timestamp":"2026-11-21T18:48:00.568Z","type":"` + typ + `","content":` + content +
		more + `}`
}

// geminiPersonVariants are session lines written every way a person's read on
// the walk reads apart from a line held whole, past a value bound of a
// kibibyte: a response's words trimmed and past the bound, its copies under
// one id and another (ids past 256 bytes among them), a line said or of no
// entry between two copies, calls of every input and a tool's name past the
// bound beside the results Gemini CLI writes into them; a user entry's own
// context, brief and notes within and past the bound, its function responses,
// a compression's summary within and past the bound and the restatement after
// it, closed by `$patch` or `$rewindTo`; members of another kind, a type and
// an id written last, and lines that are no record. A variant of several
// lines is one string, its lines joined.
func geminiPersonVariants() []string {
	h := boundedWords()
	q := boundedQuoted
	g := func(id, content, more string) string { return geminiSessionLine("gemini", id, content, more) }
	u := func(content string) string { return geminiSessionLine("user", "u1", content, "") }
	parts := func(p ...string) string { return "[" + strings.Join(p, ",") + "]" }
	text := func(s string) string { return `{"text":` + q(s) + `}` }
	calls := func(c ...string) string { return `,"toolCalls":[` + strings.Join(c, ",") + `]` }
	call := func(id, name, args string) string {
		return `{"id":"` + id + `","name":` + q(name) + `,"args":` + args + `,"result":[{"functionResponse":` +
			`{"id":"` + id + `","name":"x","response":{"output":` + q(h.words) + `}}}],"status":"success"}`
	}
	resp := func(body string) string {
		return `{"functionResponse":{"id":"c1","name":"run_shell_command","response":` + body + `}}`
	}
	long := strings.Repeat("a", 300)
	snapshot := u(parts(text("<state_snapshot>\nthe summary\n</state_snapshot>")))
	return append([]string{
		g("m1", q("the model's words"), ""),
		g("m1", q("  padded words \n"), ""),
		g("m1", q(h.words), ""),
		g("m1", q(h.words+h.blank), ""),
		g("m1", q(h.blank), ""),
		g("m1", q(h.words+h.secret), calls(call("c1", "run_shell_command", `{"command":"ls"}`))),
		g("m1", q(""), calls(call("c1", "run_shell_command", `{"command":`+q(h.words)+`}`),
			call("c2", strings.Repeat("tool ", 300), `[1, 2]`), call("c3", strings.Repeat("t", 300), `"s"`))),
		g("m1", q("w"), calls(`5`, `null`, `{"id":"c4","name":"Read"}`, `{"id":"c5","name":"x","args":null}`,
			`{"id":"c6","name":7,"args":{ "a" : [ 1 , 2 ] }}`, `{"id":"c7","name":"x","args":{"a":1},"args":{"b":2}}`)),
		g("m1", parts(text("array words")), calls(call("c1", "x", `{}`))),
		g("m1", `5`, `,"toolCalls":{"id":"c1"}`),
		g("m1", q("first copy"), "") + "\n" + g("m1", q("first copy"), calls(call("c1", "Shell", `{}`))) + "\n" +
			g("m1", q("last copy"), calls(call("c1", "Shell", `{"run":1}`))),
		g("m1", q("one"), "") + "\n" + g("m2", q("two"), "") + "\n" + g("m2", q(""), calls(call("c1", "x", `{}`))),
		`{"type":"gemini","content":"no id"}` + "\n" + `{"type":"gemini","content":"no id again","id":5}`,
		g(long, q("a long id"), "") + "\n" + g(long, q("its copy"), "") + "\n" + g(long+"b", q("another"), ""),
		g("m1", q("before a line said"), "") + "\nnot a record\n" + g("m1", q("its copy"), ""),
		g("m1", q("before a set"), "") + "\n" + `{"$set":{"lastUpdated":"x"}}` + "\n\n" + g("m1", q("its copy"), ""),
		g("m1", q("words"), calls(call("c1", "run_shell_command", `{}`))) + "\n" +
			u(parts(resp(`{"output":"answered"}`), `{"functionResponse":{"id":"zz","name":"its own",`+
				`"response":{"output":"o"}}}`)),
		u(parts(text("<session_context>\nThis is the Gemini CLI.</session_context>"), text("the brief"))),
		u(q("the brief as a string")),
		u(q("")),
		u(parts(text(h.note + "the note"))),
		u(parts(text(h.note + h.words))),
		u(parts(text(h.blank), text("  "+h.words+h.secret+"  "))),
		u(parts(`{"text":"thinking","thought":true}`, `{"thought":true,"text":"<state_snapshot>x"}`, text("words"))),
		u(parts(resp(`{"output":`+q(h.words)+`}`), resp(`{"error":"it failed"}`), resp(`{"error":{"code":5}}`),
			resp(`{"error":{"m":`+q(h.words)+`}}`), resp(`"a string body"`), resp(`{"llmContent":`+q(h.words)+`}`))),
		u(parts(`{"functionResponse":{"id":"zz","name":` + q(strings.Repeat("tool ", 300)) +
			`,"response":{"output":"o"}}}`)),
		u(parts(`{"functionCall":{"id":"c1","name":"x","args":{}},"text":"a call in a user entry"}`)),
		snapshot + "\n" + u(parts(text("a restated entry"))) + "\n" + g("m9", q("a restated reply"), "") + "\n" +
			`{"$rewindTo":"x"}` + "\n" + u(parts(text("read again"))),
		u(q("<state_snapshot>"+h.words)) + "\n" + u(parts(text("restated"))) + "\n" + `{"$patch":null}`,
		u(parts(text("  \n<state_snapshot>padded</state_snapshot>  "))) + "\n" + `{"$patch":{},"type":"user",` +
			`"content":"a sync's end is no entry"}` + "\n" + u(parts(text("after"))),
		u(parts(text("  "), text("<state_snapshot>a blank first"))),
		u(parts(`{"text":5}`, text("<state_snapshot>no text first"))),
		snapshot + "\n" + u(parts(text("an unclosed restatement"))),
		`{"content":[{"text":"typed last"}],"timestamp":"2026-11-21T18:48:00Z","type":"user"}`,
		`{"content":"id last","type":"gemini","id":"m1"}` + "\n" + g("m1", q("copy"), ""),
		`{"type":"gemini","id":"m1","content":"one","content":"two"}`,
		`{"type":"user","content":[{"text":"one"}],"content":[{"text":"two"}]}`,
		`{"type":"gemini","timestamp":"2026-11-21T19:48:00+01:00","content":"offset"}`,
		`{"type":"user","timestamp":"10000-01-01T00:00:00Z","content":"far"}`,
		`{"sessionId":"s","projectHash":"p","kind":"main"}`,
		`{"type":5,"content":"x"}`,
		`{"type":"x","extra":{"deep":[1,{"a":"b"}],"s":` + q(h.words) + `}}`,
		`  {"type":"user","content":"blanks around"}  `,
	}, boundedLines()...)
}

// A PERSON'S READ OF A GEMINI CLI SESSION ON THE WALK IS ITS WHOLE-LINE READ'S:
// over every variant, alone, between a brief and a response and after one, and
// all together, and over every recorded chat file, the read on the walk, whole
// and in pieces of seventeen bytes, answers the page and the error the
// whole-line oracle answers ([wholeGemini]) under every bounded read.
//
// Mutation: read a copy of a response as a response after it, keep a response
// held past a line said in its place, read a restated line, or take a summary
// from a text other than the first: a variant fails.
func TestAPersonsReadOfAGeminiSessionIsItsWholeLineRead(t *testing.T) {
	t.Parallel()
	reads := boundedReads()
	check := func(name string, archive []byte) {
		for _, r := range reads {
			want := personReadOf(t, wholeGemini{}, archive, r)
			if got := personReadOf(t, GeminiCLI{}, archive, r); got != want {
				t.Errorf("%s read %+v:\n%s", name, r, firstApart(got, want))
			}
			if got := personReadOf(t, piecesGemini{n: 17}, archive, r); got != want {
				t.Errorf("%s read %+v in pieces:\n%s", name, r, firstApart(got, want))
			}
		}
	}
	brief := geminiSessionLine("user", "u0", `[{"text":"the brief"}]`, "")
	call := geminiSessionLine("gemini", "m0", `"calling"`,
		`,"toolCalls":[{"id":"c1","name":"Shell","args":{"command":"ls"}}]`)
	variants := geminiPersonVariants()
	for i, v := range variants {
		check(fmt.Sprintf("variant %d %.120q", i, v), boundedArchiveOf(t, geminiMembers([]string{brief, call, v, v})...))
	}
	check("every variant", boundedArchiveOf(t, geminiMembers(variants)...))
	torn := geminiMembers([]string{brief, call})
	torn[0].torn = true
	check("a torn last line", boundedArchiveOf(t, torn...))
	for _, fixture := range chatFixtures(t, "gemini/0.70.0", "session-*.jsonl") {
		check(fixture.name, boundedArchiveOf(t, geminiMembers(fixture.lines)...))
	}
}

// A COLLECTION'S READ OF A GEMINI CLI SESSION ON THE WALK IS ITS WHOLE-LINE
// READ'S: over every recorded chat file, every person's variant and the lines
// that pick a take's delivery apart (a summary and its restatement, a type of
// another kind, thoughts, text parts in and out of a take's run), around and
// torn after a delivery, read whole and in pieces of seventeen bytes, with
// takes from the start and from a later line, it confirms the takes the
// whole-line read confirms.
//
// Mutation: confirm from a summary's own line or its restatement, or take a
// type written after the content as read before it: a case fails.
func TestACollectionsReadOfAGeminiSessionIsItsWholeLineRead(t *testing.T) {
	t.Parallel()
	q := boundedQuoted
	note := func(text string) string { return geminiSessionLine("user", "n", `[{"text":`+q(text)+`}]`, "") }
	records := map[string]string{}
	for _, fixture := range chatFixtures(t, "gemini/0.70.0", "session-*.jsonl") {
		records[fixture.name] = strings.Join(fixture.lines, "\n") + "\n"
	}
	lines := append(geminiPersonVariants(), note("one"), note("two"),
		geminiSessionLine("user", "s", `[{"text":"<state_snapshot>s"},{"text":"one"}]`, ""),
		geminiSessionLine("user", "s", `[{"text":"one","thought":true},{"text":" <state_snapshot>s"}]`, ""),
		geminiSessionLine("user", "s", `[{"text":"one","thought":true},{"text":"two"}]`, ""),
		`{"content":[{"text":"one"}],"type":"user"}`, `{"content":[{"text":"one"}],"type":5}`,
		`{"content":"one","type":"user"}`, `{"content":"","type":"user"}`, `{"content":[{"text":""}],"type":"user"}`,
		`{"content":[{"text":"one"},{"text":"one"},{"text":"two"}],"type":"user","$patch":5}`,
		`{"content":[{"text":"one"}],"type":"gemini"}`, `{"type":"user","content":[{"text":5}]}`, `null`)
	for i, l := range lines {
		records[fmt.Sprintf("line %d %.80q", i, l)] = l + "\n" + note("one") + "\n" + l + "\n" + note("two") + "\n"
		records[fmt.Sprintf("torn after line %d", i)] = note("one") + "\n" + l
	}
	records["every line"] = strings.Join(lines, "\n") + "\n"
	for name, record := range records {
		texts := geminiTextsIn(record)
		for _, from := range []int{0, 2} {
			takes := make([]geminiAskedTake, len(texts))
			for i, text := range texts {
				takes[i] = geminiAskedTake{digest: ItemDigest(text), from: from, until: -1}
				if i == 1 {
					takes[i].until = from + 3
				}
			}
			want, wantErr := geminiConfirmedWhole(t.Context(), strings.NewReader(record), takes, maxLineBytes)
			got, err := geminiConfirmed(t.Context(), strings.NewReader(record), takes, maxLineBytes)
			if !slices.Equal(got, want) || (err != nil) != (wantErr != nil) {
				t.Errorf("%s from %d: confirmed %v (%v), want %v (%v)", name, from, got, err, want, wantErr)
			}
			got, err = geminiConfirmed(t.Context(), &exportPieces{r: strings.NewReader(record), n: 17}, takes,
				maxLineBytes)
			if !slices.Equal(got, want) || (err != nil) != (wantErr != nil) {
				t.Errorf("%s from %d in pieces: confirmed %v (%v), want %v (%v)", name, from, got, err, want, wantErr)
			}
		}
	}
}

// geminiTextsIn is every user text part a record holds, in order, then one it
// never does: the takes a differential asks of it.
func geminiTextsIn(record string) []string {
	var texts []string
	for line := range strings.Lines(record) {
		var e struct {
			Type    string          `json:"type"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal([]byte(line), &e) != nil || e.Type != "user" {
			continue
		}
		for _, part := range geminiParts(e.Content) {
			if part.Text != nil {
				texts = append(texts, *part.Text)
			}
		}
	}
	return append(texts, "never")
}
