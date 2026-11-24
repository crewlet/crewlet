package executor

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// qwenMembers is a Qwen Code archive's chat file holding lines.
func qwenMembers(lines []string) []boundedMember {
	return []boundedMember{{name: "projects/k/chats/s.jsonl", lines: lines}}
}

// qwenRecord is a chat-file record of typ whose message holds parts.
func qwenRecord(typ string, parts ...string) string {
	return `{"type":"` + typ + `","timestamp":"2026-11-22T16:47:26.547Z","message":{"role":"x","parts":[` +
		strings.Join(parts, ",") + `]}}`
}

// qwenCompression is a chat_compression record whose payload is payload.
func qwenCompression(payload string) string {
	return `{"type":"system","subtype":"chat_compression","timestamp":"2026-11-22T16:47:27.000Z",` +
		`"systemPayload":` + payload + `}`
}

// qwenPersonVariants are chat-file lines written every way a person's read on
// the walk reads apart from a line held whole, past a value bound of a
// kibibyte: a response's words trimmed, past the bound and ending in a run of
// blanks past it, a thought before and after its text, calls of every input
// and a tool's name past the bound; a user record's brief and notes within
// and past the bound, every body a function response is read by, its own
// tool's name past the bound; compressions of every shape, a summary past the
// bound in its prefix and out of it; members of another kind; a type written
// last; and lines that are no record.
func qwenPersonVariants() []string {
	h := boundedWords()
	q := boundedQuoted
	text := func(s string) string { return `{"text":` + q(s) + `}` }
	call := func(name, args string) string {
		return `{"functionCall":{"id":"c1","name":` + q(name) + `,"args":` + args + `}}`
	}
	resp := func(body string) string {
		return `{"functionResponse":{"id":"c1","name":"run_shell_command","response":` + body + `}}`
	}
	asst := func(parts ...string) string { return qwenRecord("assistant", parts...) }
	result := func(parts ...string) string { return qwenRecord("tool_result", parts...) }
	entry := func(role string, parts ...string) string {
		return `{"role":"` + role + `","parts":[` + strings.Join(parts, ",") + `]}`
	}
	history := func(spl string, entries ...string) string {
		return `{"compressedHistory":[` + strings.Join(entries, ",") + `],"summaryPrefixLength":` + spl + `}`
	}
	summary := entry("user", text("<state_snapshot>the summary</state_snapshot>\n\nResume."))
	ack := entry("model", text("Got it."), `{"functionCall":{"id":"c9","name":"x","args":{}}}`)
	tail := entry("user", text("\n"+qwenNoteFraming+"a note kept"))
	return append([]string{
		asst(text("the model's words")),
		asst(text("  padded words \n")),
		asst(text(h.words)),
		asst(text(h.words + h.blank)),
		asst(text(h.blank + h.words + "\n\n")),
		asst(text(h.blank)),
		asst(text(h.words+h.secret), text("after")),
		asst(`{"text":`+q(h.words)+`,"thought":true}`, `{"thought":true,"text":"thinking"}`),
		asst(`{"text":"kept","thought":false}`, `{"text":"kept too","thought":"true"}`, `{"text":"a","thought":true,"thought":false}`),
		asst(`{"text":"first","text":"second"}`, `{"text":"words","text":5}`),
		asst(call("run_shell_command", `{"command":"ls -la","n":1}`)),
		asst(call("run_shell_command", `{"command":`+q(h.words)+`}`), text("and words")),
		asst(text("before"), call(strings.Repeat("t", 300), `"a string"`), call(strings.Repeat("tool ", 300), `[1, 2]`)),
		asst(call("x", `null`), call("x", `12.5e3`), `{"functionCall":{"id":"c3","name":"Read"}}`),
		asst(`{"text":"words","functionCall":{"id":"c2","name":"x","args":{ "a" : [ 1 , 2 ] }}}`),
		asst(`{"functionCall":5,"text":"words"}`, `{"functionCall":null}`, `{"functionCall":{"name":7,"id":8}}`),
		asst(`{"functionCall":{"id":"c1","name":"a","args":{"x":1},"args":{"y":2}}}`),
		`{"type":"assistant","message":{"parts":"a string part"}}`,
		`{"type":"assistant","message":{"parts":""}}`,
		`{"type":"assistant","message":{"parts":5}}`,
		`{"type":"assistant","message":{"parts":[5,null,"x",{"text":5},[]]}}`,
		`{"type":"assistant","message":"x"}`,
		`{"type":"assistant","message":null}`,
		`{"type":"assistant","message":{"parts":[{"text":"one"}]},"message":{"parts":[{"text":"two"}]}}`,
		`{"message":{"parts":[{"text":"typed last"}]},"timestamp":"2026-11-22T16:47:26Z","type":"assistant"}`,
		`{"type":"assistant","timestamp":"2026-11-22T17:47:26+01:00","message":{"parts":[{"text":"offset"}]}}`,
		`{"type":"assistant","timestamp":"10000-01-01T00:00:00Z","message":{"parts":[{"text":"far"}]}}`,
		`{"type":"assistant","timestamp":"yesterday","message":{"parts":[{"text":"no time"}]}}`,
		`{"type":"assistant","timestamp":5,"message":{"parts":[{"text":"no time"}]}}`,
		qwenRecord("user", text("the brief")),
		qwenRecord("user", text(h.note+"the note")),
		qwenRecord("user", text(h.note+h.words)),
		qwenRecord("user", text(h.blank), text("  \n"+h.note+"after blanks")),
		`{"type":"user","subtype":"mid_turn_user_message","message":{"role":"user","parts":[` +
			text("\n"+qwenNoteFraming+h.note+"a handed note") + `]}}`,
		qwenRecord("user", `{"text":"thought","thought":true}`, text(h.words+h.secret)),
		result(resp(`{"output":"slept 3s"}`)),
		result(resp(`{"output":` + q(h.words+h.secret) + `}`)),
		result(resp(`{"output":""}`), resp(`{"output":5}`), resp(`{"output":null}`)),
		result(resp(`{"error":"it failed"}`), resp(`{"error":""}`), resp(`{"error":{"code":1,"message":"x"}}`)),
		result(resp(`{"error":{"m":` + q(h.words) + `}}`)),
		result(resp(`{"error":[` + strings.Repeat("1, ", 400) + `1]}`)),
		result(resp(`{"error":` + q(h.words) + `}`)),
		result(resp(`{"error":null,"output":"o"}`), resp(`{"output":"o","error":"e"}`), resp(`{"error":"e","error":null}`)),
		result(resp(`"a string body"`), resp(`null`), resp(`[1, 2]`), resp(`true`)),
		result(resp(`{ "x" : [ 1 , 2 ] }`), resp(`{"llmContent":`+q(h.words)+`}`)),
		result(`{"functionResponse":{"id":"c1","name":"x"}}`, `{"functionResponse":5}`,
			`{"functionResponse":{"id":"zz","name":"its own"}}`),
		result(`{"functionResponse":{"id":"zz","name":` + q(strings.Repeat("tool ", 300)) +
			`,"response":{"output":"o"}}}`),
		result(`{"functionResponse":{"id":"c1","name":"x","response":{"output":"a"},"response":{"output":"b"}}}`),
		result(resp(`{"output":"o"}`), text("and words")),
		qwenCompression(history("2", summary, ack, tail)),
		qwenCompression(`{"summaryPrefixLength":1,"compressedHistory":[` + summary + `,` + ack + `]}`),
		qwenCompression(history("null", summary, tail)),
		qwenCompression(history("-1", summary, tail)),
		qwenCompression(history("99", summary, tail)),
		qwenCompression(history("0", summary, tail)),
		qwenCompression(history(`"2"`, summary)),
		qwenCompression(history("1.5", summary)),
		qwenCompression(history("1e0", summary)),
		qwenCompression(history("99999999999999999999", summary)),
		qwenCompression(`{"compressedHistory":null,"summaryPrefixLength":2}`),
		qwenCompression(`{"compressedHistory":5}`),
		qwenCompression(history("2", `5`)),
		qwenCompression(history("2", `null`, summary, `"x"`)),
		qwenCompression(history("3", `null`, summary, entry("user", text("two")))),
		qwenCompression(history("2", `{"parts":[{"text":"role last"}],"role":"user"}`, entry("user"))),
		qwenCompression(history("2", entry("user", `{"text":"a thought","thought":true}`, text("  "),
			`{"thought":true,"text":"x"}`, text("words")))),
		qwenCompression(history("1", entry("user", text(h.words)), entry("user", text("after")))),
		qwenCompression(history("1", entry("user", text("small")), entry("user", text(h.words)))),
		qwenCompression(history("2", entry("user", text(h.words[:600])), entry("user", text(h.words[:600])))),
		qwenCompression(history("1", entry("user", text("x"+h.blank)))),
		qwenCompression(history("1", entry("user", `"a string part"`))),
		qwenCompression(`"x"`),
		qwenCompression(`null`),
		`{"type":"system","subtype":"chat_compression"}`,
		`{"type":"system","subtype":"other","systemPayload":` + history("2", summary) + `}`,
		`{"type":"x","message":{"parts":[{"text":"unknown type"}]}}`,
		`{"type":"user"}`,
		`{"type":5,"message":{"parts":[{"text":"x"}]}}`,
		`{"type":"x","extra":{"deep":[1,{"a":"b"}],"s":` + q(h.words) + `}}`,
		`  {"type":"assistant","message":{"parts":[{"text":"blanks around"}]}}  `,
	}, boundedLines()...)
}

// A PERSON'S READ OF A QWEN CODE SESSION ON THE WALK IS ITS WHOLE-LINE READ'S:
// over every variant, alone, between a brief and a call and the call's result,
// and all together, and over every recorded chat file, the read on the walk,
// whole and in pieces of seventeen bytes, answers the page and the error the
// whole-line oracle answers ([wholeQwen]) under every bounded read.
//
// Mutation: split a brief past the bound, drop a run of blanks past the bound
// that ends a text, read a thought's text, join a compaction entry's texts
// before its role is read, or name a result by its own tool where its call
// named one: a variant fails.
func TestAPersonsReadOfAQwenSessionIsItsWholeLineRead(t *testing.T) {
	t.Parallel()
	reads := boundedReads()
	check := func(name string, archive []byte) {
		for _, r := range reads {
			want := personReadOf(t, wholeQwen{}, archive, r)
			if got := personReadOf(t, QwenCode{}, archive, r); got != want {
				t.Errorf("%s read %+v:\n%s", name, r, firstApart(got, want))
			}
			if got := personReadOf(t, piecesQwen{n: 17}, archive, r); got != want {
				t.Errorf("%s read %+v in pieces:\n%s", name, r, firstApart(got, want))
			}
		}
	}
	brief := qwenRecord("user", `{"text":"the brief"}`)
	call := qwenRecord("assistant", `{"functionCall":{"id":"c1","name":"Shell","args":{"command":"ls"}}}`)
	variants := qwenPersonVariants()
	for i, v := range variants {
		check(fmt.Sprintf("variant %d %.120q", i, v), boundedArchiveOf(t, qwenMembers([]string{brief, call, v, v})...))
	}
	check("every variant", boundedArchiveOf(t, qwenMembers(variants)...))
	torn := qwenMembers([]string{brief, call, variants[0]})
	torn[0].torn = true
	check("a torn last line", boundedArchiveOf(t, torn...))
	for _, fixture := range chatFixtures(t, "qwen/0.28.4", "chat-*.jsonl") {
		check(fixture.name, boundedArchiveOf(t, qwenMembers(fixture.lines)...))
	}
}

// A COLLECTION'S READ OF A QWEN CODE SESSION ON THE WALK IS ITS WHOLE-LINE
// READ'S: over every recorded chat file, every person's variant and the lines
// that pick a note's receipt apart (compactions keeping and summarizing it,
// members of another kind, a note in another part, null and torn lines),
// between a note and its reply and torn after them, read whole and in pieces
// of seventeen bytes, from the start and past a line, it holds the notes the
// whole-line read holds.
//
// Mutation: keep a note a compaction's prefix covers, take a note's text from
// a part past the first, or keep pending notes past a line of another kind:
// a case fails.
func TestACollectionsReadOfAQwenSessionIsItsWholeLineRead(t *testing.T) {
	t.Parallel()
	q := boundedQuoted
	note := func(text string) string {
		return `{"type":"user","subtype":"mid_turn_user_message","message":{"role":"user","parts":[{"text":` +
			q("\n"+qwenNoteFraming+text) + `}]}}`
	}
	reply := qwenRecord("assistant", `{"text":"ok"}`)
	keeps := func(spl, cpl string, texts ...string) string {
		entries := []string{`{"role":"user","parts":[{"text":"<state_snapshot>s</state_snapshot>"}]}`,
			`{"role":"model","parts":[{"text":"Got it."}]}`}
		for _, text := range texts {
			entries = append(entries, `{"role":"user","parts":[{"text":`+q(text)+`}]}`)
		}
		return qwenCompression(`{"compressedHistory":[` + strings.Join(entries, ",") + `],"summaryPrefixLength":` +
			spl + `,"contextPrefixLength":` + cpl + `}`)
	}
	records := map[string]string{}
	for _, fixture := range chatFixtures(t, "qwen/0.28.4", "chat-*.jsonl") {
		records[fixture.name] = strings.Join(fixture.lines, "\n") + "\n"
	}
	lines := append(qwenPersonVariants(), note("one"), note("two"), reply,
		keeps("2", "2", "\n"+qwenNoteFraming+"one"), keeps("2", "3", "\n"+qwenNoteFraming+"one"),
		keeps("2", "null", "one", "one"), keeps("3", "-1", "one", "one"), keeps("-1", "2", "one"),
		keeps(`"2"`, "2", "one"), keeps("2", "1.5", "one"), keeps("2", "2", "one")+"\n"+keeps("2", "2", "two"),
		qwenCompression(`{"summaryPrefixLength":0,"compressedHistory":[{"role":5,"parts":[{"text":"one"}]}]}`),
		qwenCompression(`{"summaryPrefixLength":0,"compressedHistory":[{"role":"user","parts":[{"text":5}]}]}`),
		qwenCompression(`{"summaryPrefixLength":0,"compressedHistory":[null,{"role":"user","parts":[null,`+
			`{"text":null},{"text":"one"}]}]}`),
		qwenCompression(`{"summaryPrefixLength":0,"compressedHistory":[{"role":"model","parts":[{"text":"one"}]}]}`),
		`{"type":"assistant","subtype":5}`, `{"type":"user","message":{"parts":[{"text":5}]}}`,
		`{"type":"user","message":{"parts":{}}}`, `{"type":"user","message":[]}`, `null`, ` null `,
		`{"type":"user","subtype":"mid_turn_user_message","message":{"parts":[null,{"text":`+
			q("\n"+qwenNoteFraming+"two")+`}]}}`,
		`{"type":"user","subtype":"mid_turn_user_message","message":{"parts":[{"text":`+
			q("\n"+qwenNoteFraming+"one")+`,"text":null}]}}`,
		`{"type":"user","subtype":"mid_turn_user_message","message":{"parts":[{"text":`+q(qwenNoteFraming+"two")+`}]}}`,
		`{"type":"user","subtype":"mid_turn_user_message","message":{"parts":[{"text":`+
			q("\n"+qwenNoteFraming)+`}]}}`)
	for i, l := range lines {
		records[fmt.Sprintf("line %d %.80q", i, l)] = note("one") + "\n" + l + "\n" + note("two") + "\n" + reply + "\n"
		records[fmt.Sprintf("torn after line %d", i)] = note("one") + "\n" + reply + "\n" + l
	}
	records["every line"] = strings.Join(lines, "\n") + "\n"
	for name, record := range records {
		takes := qwenNotesIn(record)
		for _, from := range []int{0, 2} {
			want, wantErr := qwenNotesHeldWhole(t.Context(), strings.NewReader(record), takes, from, maxLineBytes)
			got, err := qwenNotesHeld(t.Context(), strings.NewReader(record), takes, from, maxLineBytes)
			if !slices.Equal(got, want) || (err != nil) != (wantErr != nil) {
				t.Errorf("%s from %d: held %v (%v), want %v (%v)", name, from, got, err, want, wantErr)
			}
			got, err = qwenNotesHeld(t.Context(), &exportPieces{r: strings.NewReader(record), n: 17}, takes, from,
				maxLineBytes)
			if !slices.Equal(got, want) || (err != nil) != (wantErr != nil) {
				t.Errorf("%s from %d in pieces: held %v (%v), want %v (%v)", name, from, got, err, want, wantErr)
			}
		}
	}
}

// qwenNotesIn is a take of every note text a record hands behind Qwen Code's
// framing, in order, then of "two", which a line may hand in another part, and
// of one it never does.
func qwenNotesIn(record string) []ServedTake {
	var takes []ServedTake
	for line := range strings.Lines(record) {
		var r struct {
			Message struct {
				Parts []struct {
					Text *string `json:"text"`
				} `json:"parts"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(line), &r) != nil || len(r.Message.Parts) == 0 || r.Message.Parts[0].Text == nil {
			continue
		}
		if handed, ok := strings.CutPrefix(*r.Message.Parts[0].Text, "\n"+qwenNoteFraming); ok {
			takes = append(takes, ServedTake{InboxID: fmt.Sprintf("i-%d", len(takes)), Digest: ItemDigest(handed)})
		}
	}
	return append(takes, ServedTake{InboxID: "two", Digest: ItemDigest("two")},
		ServedTake{InboxID: "never", Digest: ItemDigest("never")})
}

// A RESPONSE'S ANSWER KEEPS ONLY THE TEXT THAT CLAIMS IT ([chatAnswer]): a
// member written twice that picks a text let go of past the value bound is
// said in its place (an output past it replaced by one of no words, before or
// after a body past it a scanner's buffer long, or an output past it after an
// error past it replaced by null), and one that picks a text kept, or written
// again, is read (an output within the bound after such an error, an output
// replaced by another, an error after an output, a body's own text of members
// twice, and an output after a body past the bound).
//
// Mutation: keep a lower text past the bound once one claims the answer, keep
// a text let go of as lost once written again, or keep one written under a
// higher claim past the bound: a case fails.
func TestAResponsesAnswerLetGoOfPastTheBoundIsSaid(t *testing.T) {
	t.Parallel()
	h := boundedWords()
	q := boundedQuoted
	r := personRead{bounds: TranscriptReadBounds(), valueMax: 1 << 10}
	const twice = "names its output or its error twice"
	for _, c := range []struct {
		body  string
		said  bool
		holds string
	}{
		{body: `{"output":` + q(h.words) + `,"output":5}`, said: true},
		{body: `{"pad":` + q(strings.Repeat(h.words, 40)) + `,"output":"x","output":5}`, said: true},
		{body: `{"error":"e","error":null}`, holds: `error\":null`},
		{body: `{"error":` + q(h.words) + `,"output":` + q(h.words) + `,"error":null}`, said: true},
		{body: `{"error":` + q(h.words) + `,"error":null,"output":"o"}`, holds: `"text":"o"`},
		{body: `{"error":` + q(h.words) + `,"output":` + q(h.words) + `,"output":"o","error":null}`,
			holds: `"text":"o"`},
		{body: `{"output":` + q(h.words) + `,"output":"the last"}`, holds: "the last"},
		{body: `{"pad":` + q(h.words) + `,"output":` + q("the output "+h.words) + `}`, holds: "the output"},
		{body: `{"output":` + q(h.words) + `,"error":"it failed"}`, holds: "it failed"},
	} {
		line := qwenRecord("tool_result", `{"functionResponse":{"id":"c1","name":"x","response":`+c.body+`}}`)
		got := personReadOf(t, QwenCode{}, boundedArchiveOf(t, qwenMembers([]string{line})...), r)
		if strings.Contains(got, twice) != c.said || !strings.Contains(got, c.holds) {
			t.Errorf("%.80q: read %.300s; want said %v, holding %q", c.body, got, c.said, c.holds)
		}
	}
}
