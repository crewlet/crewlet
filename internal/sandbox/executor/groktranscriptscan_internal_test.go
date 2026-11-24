package executor

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// grokMembers is a Grok Build archive whose main record holds lines.
func grokMembers(lines []string) []boundedMember {
	return []boundedMember{{name: "sessions/s1/chat_history.jsonl", lines: lines}}
}

// grokToken is the take token of the take numbered n.
func grokToken(n string) string { return claudeTakeToken("0f0e0d0c-0000-4000-8000-00000000000" + n) }

// grokHookLine is a user line handing the model a hook's answer: words under
// head, the token after them, then tail, of reason.
func grokHookLine(head, words, token, tail, reason string) string {
	return `{"type":"user","content":[{"type":"text","text":` + boundedQuoted(head+words+token+tail) +
		`}],"synthetic_reason":"` + reason + `"}`
}

// grokPersonVariants are chat_history.jsonl lines written every way a person's
// read on the walk reads apart from a line held whole, past a value bound of a
// kibibyte: a prompt within and past it, its tags cut where its type comes
// first and kept where it comes after, blank once cut; a content as a string
// and as parts of any type, joined within and past it, a part past it written
// in order and out of it; a hook's answer within and past it; calls before and
// after the words, use_tool's within and past it, of no shape and naming tools
// past it; a result's; members of another kind than their field's; and lines
// that are no record.
func grokPersonVariants() []string {
	h := boundedWords()
	q := boundedQuoted
	open, closing := grokQueryOpen, grokQueryClose
	prompt := func(content string) string { return `{"type":"user","content":` + content + `,"prompt_index":0}` }
	parts := func(texts ...string) string {
		var b strings.Builder
		for i, t := range texts {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(`{"type":"text","text":` + q(t) + `}`)
		}
		return "[" + b.String() + "]"
	}
	call := func(id, name, args string) string {
		return `{"id":"` + id + `","name":` + q(name) + `,"arguments":` + args + `}`
	}
	said := func(content, calls string) string {
		return `{"type":"assistant","content":` + content + `,"tool_calls":[` + calls + `],"model_id":"m"}`
	}
	result := func(content string) string {
		return `{"type":"tool_result","tool_call_id":"c1","content":` + content + `}`
	}
	use := func(input string) string { return q(`{"tool_name":"crewlet_ab__echo","tool_input":` + input + `}`) }
	return append([]string{
		prompt(q(open + "the brief" + closing)), prompt(q(open + h.note + h.words + closing)),
		`{"content":` + q(open+h.words+closing) + `,"type":"user","prompt_index":0}`,
		prompt(q(open + h.blank + closing)), `{"content":` + q(open+h.blank+closing) + `,"type":"user","prompt_index":1}`,
		prompt(parts(open+"part one ", h.words+h.secret+closing)), prompt(parts(open+h.words, " ", closing)),
		prompt(parts("<user_q", "uery>\n"+h.words+"\n</user_", "query>")), prompt(q(open + h.note + "within" + closing)),
		prompt(q(h.note + "no tags")), prompt(q(open + h.words)), prompt(q(h.words + closing)), prompt(q("<user_q")),
		prompt(`[{"text":"typed apart","type":5}]`), prompt(`[{"text":5}]`), prompt(`["x"]`), prompt(`{"a":1}`),
		prompt(`null`), prompt(`7`), prompt(`[{"text":` + q(h.words) + `,"text":"after"}]`),
		prompt(`[{"text":"before","text":` + q(h.words) + `}]`), prompt(`[null,{"text":null},{"text":"x"}]`),
		`{"type":"user","content":"x","prompt_index":null}`, `{"type":"user","content":"x","prompt_index":1.5}`,
		`{"type":"user","content":"x","prompt_index":"0"}`, `{"type":"user","content":"x","synthetic_reason":5}`,
		`{"type":"user","content":"x","prompt_index":0,"synthetic_reason":` + q(strings.Repeat("r", 300)) + `}`,
		grokHookLine(grokHookHeads[0], h.note+"Use main.\n\n", grokToken("1"), grokReminderTail, "system_reminder"),
		grokHookLine(grokHookHeads[1], h.note+"It failed.\n\n", grokToken("2"), grokReminderTail, "system_reminder"),
		grokHookLine(grokStopHead, h.note+"Add a test.\n\n", grokToken("3"), "\n", "stop_hook_feedback"),
		grokHookLine(grokHookHeads[0], h.note+h.words, grokToken("4"), grokReminderTail, "system_reminder"),
		grokHookLine(grokStopHead, h.note, grokToken("5"), "\n", "system_reminder"),
		grokHookLine(grokHookHeads[0], "no heading", "", grokReminderTail, "system_reminder"),
		grokHookLine(grokHookHeads[0], h.note+"  \n", grokToken("6")+" \n", grokReminderTail, "system_reminder"),
		said(q("the words"), call("c1", "bash", q(`{"command": "ls"}`))),
		`{"type":"assistant","tool_calls":[` + call("c2", "bash", q(`{}`)) + `],"content":` + q(h.words) + `}`,
		said(q(" "), call("c3", "use_tool", use(`{"text": "hi"}`))),
		said(`""`, call("c4", "use_tool", use(`{"text":`+q(h.words)+`}`))),
		said(`""`, call("c5", "use_tool", q(`{"tool_input":{}}`))+","+call("c6", "use_tool", q(`not json`))),
		said(`""`, call("c7", "bash", q(`{"command":`+q(h.words)+`}`))+","+call("c8", "read", q("not json"))),
		said(`""`, call("c9", "bash", `null`)), said(`""`, call("c9", "bash", `5`)), said(`""`, `"x"`),
		said(`""`, `null`), said(`""`, `{}`), `{"type":"assistant","content":"x","tool_calls":{}}`,
		said(`""`, call(strings.Repeat("i", 300), strings.Repeat("t", 300), q(`[1]`))+","+call("c1", h.words, q(`{}`))),
		said(parts("a ", "b"), ""), said(parts(h.words, h.words+h.secret), ""),
		said(`[{"text":`+q(h.words)+`,"text":"after"}]`, call("c1", "bash", q(`{}`))), said(q(h.blank), ""),
		said(parts(h.blank, h.blank), call("c1", "bash", q(`{}`))), said(`5`, call("c1", "bash", q(`{}`))),
		result(q("the result")), result(q(h.words + h.secret)), result(q(h.blank)), result(`null`), result(`5`),
		result(parts("one ", "two")), result(`[{"text":` + q(h.words) + `,"text":"x"}]`), result(`[5]`),
		`{"type":"tool_result","tool_call_id":5,"content":"x"}`, `{"type":"tool_result","content":"no call"}`,
		`{"type":"system","content":"not read"}`, `{}`, `{"type":5}`, `{"type":"user"}`,
	}, boundedLines()...)
}

// A PERSON'S READ OF A GROK BUILD SESSION ON THE WALK IS ITS WHOLE-LINE
// READ'S: over every variant, alone, between a brief and a call, and all
// together, and over every fixture, a subagent's record beside its main one,
// the read on the walk, whole and in pieces of seventeen bytes, answers the
// page and the error the whole-line oracle answers ([wholeGrok]) under every
// bounded read.
//
// Mutation: frame a prompt whose type comes after it, keep a prompt's tags
// past the bound where its type comes first, read use_tool's arguments past
// the bound, or read a part past the bound written out of order: a variant
// fails.
func TestAPersonsReadOfAGrokSessionIsItsWholeLineRead(t *testing.T) {
	t.Parallel()
	reads := boundedReads()
	check := func(name string, archive []byte) {
		for _, r := range reads {
			want := personReadOf(t, wholeGrok{}, archive, r)
			if got := personReadOf(t, Grok{}, archive, r); got != want {
				t.Errorf("%s read %+v:\n%s", name, r, firstApart(got, want))
			}
			if got := personReadOf(t, piecesGrok{n: 17}, archive, r); got != want {
				t.Errorf("%s read %+v in pieces:\n%s", name, r, firstApart(got, want))
			}
		}
	}
	brief := `{"type":"user","content":"<user_query>\nthe brief\n</user_query>","prompt_index":0}`
	call := `{"type":"assistant","content":"","tool_calls":[{"id":"c1","name":"bash","arguments":"{}"}]}`
	variants := grokPersonVariants()
	for i, v := range variants {
		check(fmt.Sprintf("variant %d %.120q", i, v), boundedArchiveOf(t, grokMembers([]string{brief, call, v,
			brief})...))
	}
	check("every variant", boundedArchiveOf(t, grokMembers(variants)...))
	for _, fixture := range grokChatFixtures(t) {
		lines := strings.Split(strings.TrimSuffix(fixtureAt(t, fixture), "\n"), "\n")
		check(fixture, boundedArchiveOf(t, boundedMember{name: "k/main/chat_history.jsonl", lines: lines},
			boundedMember{name: "k/child/chat_history.jsonl", lines: lines}))
	}
}

// A COLLECTION'S READ OF A GROK BUILD SESSION ON THE WALK IS ITS WHOLE-LINE
// READ'S: over every fixture, every person's variant and the lines that pick a
// hook's answer apart (heads and tails across parts, a reason after the
// words, white space and a run of it longer than a read's piece around the
// token, members of another kind), each between a hook's answer and the
// model's and torn after them, read whole and in pieces of seventeen bytes, it
// holds the takes the whole-line read holds and fails where it fails, in its
// words.
//
// Mutation: read the token before the tail is cut, cut white space other
// than spaces and breaks, or tell a head by fewer bytes than it holds: a case
// fails.
func TestACollectionsReadOfAGrokSessionIsItsWholeLineRead(t *testing.T) {
	t.Parallel()
	head, tail := grokHookHeads[0], grokReminderTail
	note := grokHookLine(head, "## A note from ada\n\nUse main.\n\n", grokToken("1"), tail, "system_reminder")
	answer := `{"type":"assistant","content":"ok"}`
	q := boundedQuoted
	run := strings.Repeat(" \n", 40000)
	records := map[string]string{}
	for _, fixture := range grokChatFixtures(t) {
		records[fixture] = fixtureAt(t, fixture)
	}
	lines := append(grokPersonVariants(),
		`{"type":"user","content":[{"text":`+q(head[:20])+`},{"text":`+q(head[20:]+"x"+grokToken("2"))+
			`},{"text":`+q(tail[:5])+`},{"text":`+q(tail[5:])+`}],"synthetic_reason":"system_reminder"}`,
		`{"type":"user","content":`+q(head+grokToken("3")+tail)+`}`,
		`{"content":`+q(grokStopHead+grokToken("4")+"\n")+`,"synthetic_reason":"stop_hook_feedback","type":"user"}`,
		grokHookLine(head, "x", grokToken("5")+run, tail, "system_reminder"),
		grokHookLine(head, strings.Repeat("words ", 20000), grokToken("6")+"\t", tail, "system_reminder"),
		grokHookLine(head, "", grokToken("7")[1:], tail, "system_reminder"),
		grokHookLine(head, "", grokToken("8"), tail+tail, "system_reminder"),
		grokHookLine(head, "", grokToken("9"), "", "system_reminder"),
		grokHookLine(grokStopHead, "", grokToken("8"), "\n\n \n", "stop_hook_feedback"),
		grokHookLine(head[:len(head)-1], "", grokToken("9"), tail, "system_reminder"),
		grokHookLine(head[:len(head)-1]+"x", "", grokToken("9"), tail, "system_reminder"),
		grokHookLine("", "", "", head[:30], "system_reminder"),
		`{"type":"user","content":[{"text":`+q(head+grokToken("2")+tail)+`},5],"synthetic_reason":"system_reminder"}`,
		`{"type":"user","content":`+q(head+grokToken("2")+tail)+`,"synthetic_reason":"system_reminder",`+
			`"tool_calls":[{"id":5}]}`, `{"type":"assistant","content":"x","prompt_index":"1"}`, `true`)
	for i, l := range lines {
		records[fmt.Sprintf("line %d %.80q", i, l)] = note + "\n" + l + "\n" + answer + "\n"
		records[fmt.Sprintf("line %d before its answer", i)] = l + "\n" + answer + "\n"
		records[fmt.Sprintf("torn after line %d", i)] = note + "\n" + answer + "\n" + l
		records[fmt.Sprintf("line %d last", i)] = note + "\n" + answer + "\n" + l + "\n"
	}
	records["every line"] = note + "\n" + strings.Join(lines, "\n") + "\n" + answer + "\n"
	for name, record := range records {
		want, wantErr := grokHeldWhole(strings.NewReader(record))
		got, err := grokHeld(t.Context(), strings.NewReader(record))
		if !maps.Equal(got, want) || fmt.Sprint(err) != fmt.Sprint(wantErr) {
			t.Errorf("%s: held %v (%v), want %v (%v)", name, got, err, want, wantErr)
		}
		got, err = grokHeld(t.Context(), &exportPieces{r: strings.NewReader(record), n: 17})
		if !maps.Equal(got, want) || fmt.Sprint(err) != fmt.Sprint(wantErr) {
			t.Errorf("%s in pieces: held %v (%v), want %v (%v)", name, got, err, want, wantErr)
		}
	}
}

// grokChatFixtures is every recorded chat_history.jsonl's path.
func grokChatFixtures(t *testing.T) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("testdata", "grok", "*", "*.chat.jsonl"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no chat fixtures (%v)", err)
	}
	return paths
}

// fixtureAt is the fixture at path.
func fixtureAt(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
