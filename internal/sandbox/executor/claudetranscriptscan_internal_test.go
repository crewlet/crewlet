package executor

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/redact/redacttest"
)

// claudeArchiveOf is a gzipped tar of a Claude Code session whose transcript
// holds lines, each ended by a break, and a subagent's holding sub.
func claudeArchiveOf(t testing.TB, lines, sub []string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	member := func(name string, lines []string) {
		body := strings.Join(lines, "\n")
		if len(lines) > 0 {
			body += "\n"
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(body)),
			Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	prefix := "projects/" + claudeProjectDir + "/" + ctSession
	member(prefix+".jsonl", lines)
	if sub != nil {
		member(prefix+"/subagents/agent-a1.jsonl", sub)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// claudePersonVariants are records written every way a person's read on the
// scanner reads apart from a line held whole, past a value bound of a
// kibibyte: a brief opening with a heading past it and within it, a kind and a
// block's type written last, blank texts past it, a call's input of every kind,
// results joined, each part and as a string past it, parts written out of
// order past it, a tool's name past it, a hook line within and past it, notes
// in a record the harness wrote for itself (escaped, in a name, past it),
// messages of no shape and written twice, lines that are no record (one refused
// a read's chunk before its break), and a credential across the bound.
func claudePersonVariants() []string {
	q := func(s string) string {
		raw, err := json.Marshal(s)
		if err != nil {
			panic(err)
		}
		return string(raw)
	}
	w := strings.Repeat("words of the model, ", 100)
	note := noteHeadOpen + "ada" + noteHeadClose + "\nTarget main.\n"
	blank := strings.Repeat(" \n", 1000)
	secret := redacttest.Shapes()[0].Sample
	part := func(text string) string { return `{"type":"text","text":` + q(text) + `}` }
	reply := func(blocks ...string) string {
		return `{"type":"assistant","message":{"role":"assistant","content":[` + strings.Join(blocks, ",") + `]}}`
	}
	answer := func(id, content string) string {
		return `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"` + id +
			`","content":` + content + `}]}}`
	}
	call := func(id, input string) string {
		return `{"type":"tool_use","id":"` + id + `","name":"Bash","input":` + input + `}`
	}
	hook := "PostToolBatch says: crewlet: a note reached the run"
	escaped := strings.ReplaceAll(q(note), "#", `\u0023`)
	return []string{
		`{"type":"user","message":{"role":"user","content":` + q(note+"short") + `}}`,
		`{"type":"user","message":{"role":"user","content":` + q(note+w) + `}}`,
		`{"message":{"role":"user","content":` + q(note+"short") + `},"type":"user","timestamp":"2026-01-01T00:00:00Z"}`,
		`{"message":{"content":[{"type":"text","text":` + q(w) + `}]},"type":"assistant"}`,
		`{"type":"user","isCompactSummary":true,"message":{"content":` + q(w) + `}}`,
		`{"isCompactSummary":true,"message":{"content":"short"},"type":"user"}`,
		`{"type":"user","isMeta":true,"message":{"content":` + q(note) + `}}`,
		`{"type":"user","message":{"content":` + q(blank) + `}}`,
		reply(`{"type":"text","text":`+q(blank)+`}`, `{"type":"text","text":" "}`, `{"type":"thinking","thinking":`+q(note)+`}`),
		reply(`{"text":`+q(w)+`,"type":"text"}`, `{"input":{"a":`+q(w)+`},"name":"Bash","id":"toolu_9","type":"tool_use"}`),
		reply(call("toolu_1", `{"command":`+q(w)+` , "n": 1 ,"x":[ 1, 2 ], "s":"a b"}`)),
		reply(call("toolu_2", q(w)), call("toolu_3", `12.5e3`), call("toolu_4", `null`),
			`{"type":"tool_use","id":"toolu_5","name":"Bash"}`, call("toolu_6", `{ "s" : "x\ny" }`)),
		answer("toolu_1", q(w)),
		answer("toolu_2", `[`+part(strings.Repeat("x", 600))+`,`+part(strings.Repeat("y", 600))+`]`),
		answer("toolu_3", `[`+part(w)+`,`+part("after")+`]`),
		answer("toolu_4", `[`+part(blank)+`,`+part(" ")+`,{"type":"image","source":{}},`+part("ok")+`]`),
		answer("toolu_5", `[{"text":`+q(w)+`,"type":"text"}]`),
		answer("toolu_6", `[{"type":"text","text":`+q(w)+`,"type":"text"}]`),
		answer("toolu_7", `[{"type":"text","text":"x","text":`+q(w)+`}]`),
		answer("toolu_8", `[{"type":"text","text":`+q(w)+`,"text":"x"},`+part("on")+`]`),
		answer("toolu_9", `[{"type":"image","text":`+q(w)+`}]`),
		answer("toolu_x", `[{"type":"text","text":`+q(strings.Repeat("y", 1020)+secret+" and more")+`}]`),
		`{"type":"user","message":{"content":[{"content":"late","tool_use_id":"toolu_1","is_error":true,"type":"tool_result"}]}}`,
		reply(`{"type":"tool_use","id":"toolu_n","name":` + q(w) + `,"input":{}}`),
		answer("toolu_n", `"named past the bound"`),
		reply(`{"type":"tool_use","id":` + q(w) + `,"name":"Bash","input":{}}`),
		`{"type":"system","subtype":"informational","content":` + q(hook+"\n"+note) + `}`,
		`{"content":` + q(hook) + `,"level":"notice","type":"system","subtype":"informational"}`,
		`{"type":"system","subtype":"informational","content":` + q(hook+" "+w) + `}`,
		`{"type":"system","subtype":"informational","content":` + q(hook) + `,"content":5}`,
		`{"type":"system","subtype":"compact_boundary","content":` + q(note) + `}`,
		`{"type":"attachment","attachment":{"content":` + q(note) + `}}`,
		`{"type":"attachment","attachment":{"type":"hook_additional_context","content":[` + q(note) + `]}}`,
		`{"type":"attachment","attachment":{"content":[` + q(note) + `],"type":"hook_additional_context"}}`,
		`{"type":"attachment","attachment":{"content":[` + q(note) + `],"type":"hook_additional_context",` +
			`"type":"hook_success"}}`,
		`{"type":"attachment","attachment":{"type":"hook_additional_context","content":[` + q(note) + `],` +
			`"content":["x"]}}`,
		`{"type":"attachment","attachment":{"type":"hook_additional_context","content":[` + q(note) + `]},` +
			`"attachment":{"type":"hook_success","content":""}}`,
		`{"type":"attachment","attachment":{"type":"hook_additional_context","content":{` + q(note) + `:[` +
			q(note) + `]}},"rendered":[{"content":` + q(note) + `}]}`,
		`{"type":"attachment","x":` + escaped + `}`,
		`{"type":"progress",` + q(note) + `:1}`,
		`{"type":"x","big":` + q(note+w) + `}`,
		`{"type":"assistant","message":"just a string"}`,
		`{"type":"assistant","message":{"content":5}}`,
		`{"type":"assistant","message":{"content":"first"},"message":{"content":[{"type":"text","text":"second"}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"first"}],"content":"second"}}`,
		`[1,2]`, `"x"`, `{"type":`, "\u00a0" + `{"type":"user","message":{"content":"x"}}`, ` `, ``, `{} {}`,
		`{"type":"user","message":{"content":"x"}} trailing`, "\t{\"type\":\"user\",\"message\":{\"content\":\"tabbed\"}}\r",
		`{"type":"user","message":{"content":"x"}} ` + q(strings.Repeat("y", 2*lineChunkBytes)),
		reply(`{"type":"text","text":` + q(strings.Repeat("x", 1010)+secret+w) + `}`),
	}
}

// A PERSON'S READ OF A CLAUDE CODE SESSION ON THE SCANNER IS ITS WHOLE-LINE
// READ'S: over every variant, alone, after a brief and all together, in the
// session's transcript and a subagent's, the read on the scanner, whole and in
// pieces of seventeen bytes, answers the page and the error the whole-line
// oracle answers ([transcriptBuilder.claudeArchiveWhole]): from the start and
// later, at a person's bounds and a seat's, under filters by kind and by term,
// at the value bound and below, with every redaction refused, and past the
// lines bound inside one record's blocks and parts and at a line.
//
// Mutation: split a brief past the bound, read a hook line past it, stream a
// part out of order, show a refused redaction, read a line framed by other
// white space, leave a refused line's rest unread or drop the lines bound.
func TestAPersonsReadOfAClaudeCodeSessionIsItsWholeLineRead(t *testing.T) {
	t.Parallel()
	person, seat := TranscriptReadBounds(), SeatTranscriptBounds()
	calls := TranscriptFilter{Kinds: []EntryKind{EntryToolCall, EntryToolResult}}
	terms := TranscriptFilter{Terms: []string{"words"}}
	wide := []personRead{{bounds: person, valueMax: TranscriptValueMaxBytes}, {bounds: person, valueMax: 1 << 10},
		{bounds: seat, valueMax: 1 << 10, from: 2, filter: calls}}
	every := append(wide, personRead{bounds: person, valueMax: 1 << 10, filter: terms},
		personRead{bounds: seat, valueMax: 512, filter: terms, from: 1},
		personRead{bounds: person, valueMax: 1 << 10, refuse: true},
		personRead{bounds: person, valueMax: 1 << 10, from: 1000},
		personRead{bounds: person, valueMax: 1 << 10, filter: TranscriptFilter{Kinds: []EntryKind{EntryModel}}},
		personRead{bounds: person, valueMax: 1 << 10,
			filter: TranscriptFilter{Kinds: []EntryKind{EntryNote, EntryLateCall}}})
	check := func(name string, archive []byte, reads []personRead) {
		for _, r := range reads {
			want := personReadOf(t, wholeClaudeCode{}, archive, r)
			if got := personReadOf(t, ClaudeCode{}, archive, r); got != want {
				t.Errorf("%s read %+v:\n%s", name, r, firstApart(got, want))
			}
			if got := personReadOf(t, piecesClaudeCode{n: 17}, archive, r); got != want {
				t.Errorf("%s read %+v in pieces:\n%s", name, r, firstApart(got, want))
			}
		}
	}
	brief := `{"type":"user","message":{"role":"user","content":"the brief"}}`
	variants := claudePersonVariants()
	for i, v := range variants {
		check(fmt.Sprintf("variant %d %.120q", i, v), claudeArchiveOf(t, []string{brief, v}, []string{v, brief}),
			every)
	}
	check("every variant", claudeArchiveOf(t, variants, variants), every)
	_, raw := claudeCaptureOfInternal(t)
	check("the session record", raw, wide)
	// Past the lines bound inside one record's blocks, inside a result's parts,
	// and at the line that crosses it.
	empties := func(n int) string { return strings.Repeat(`{},`, n-1) + `{}` }
	reads := []personRead{{bounds: person, valueMax: 1 << 10}}
	check("past the lines bound in a message's blocks", claudeArchiveOf(t, []string{brief,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"before"},` + empties(TranscriptReadLines) +
			`,{"type":"text","text":"after"}]}}`, brief}, nil), reads)
	check("past the lines bound in a result's parts", claudeArchiveOf(t, []string{brief,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t","content":[` +
			empties(TranscriptReadLines) + `]}]}}`, brief}, nil), reads)
	for _, short := range []int{2, 3} {
		check(fmt.Sprintf("at the lines bound less %d", short), claudeArchiveOf(t, []string{brief,
			`{"type":"assistant","message":{"content":[` + empties(TranscriptReadLines-short) +
				`,{"type":"text","text":"x"}]}}`, `not a record`, brief}, nil), reads)
	}
}

// firstApart is got and want around where they first differ.
func firstApart(got, want string) string {
	at := 0
	for at < len(got) && at < len(want) && got[at] == want[at] {
		at++
	}
	from := max(at-300, 0)
	return fmt.Sprintf("apart at byte %d:\nscan   ...%.500s\nwant   ...%.500s", at, got[from:], want[from:])
}

// claudeCaptureOfInternal is the transcript test's session record and
// sidecar as an archive, read here for the oracle.
func claudeCaptureOfInternal(t *testing.T) ([]string, []byte) {
	t.Helper()
	lines := []string{
		`{"type":"user","message":{"role":"user","content":"Fix the flaky test in ENG-7."},"timestamp":"2026-10-30T12:00:00.000Z"}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Running the tests first."}]},"timestamp":"2026-10-30T12:00:02.000Z"}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"go test ./cache/...","description":"run the tests"}}]},"timestamp":"2026-10-30T12:00:03.000Z"}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"FAIL cache_test.go:40","is_error":true}]},"timestamp":"2026-10-30T12:00:04.000Z"}`,
		`{"type":"file-history-snapshot","messageId":"m1","snapshot":{}}`,
		`{"type":"user","isCompactSummary":true,"message":{"role":"user","content":"The test races on the cache."},"timestamp":"2026-10-30T12:00:06.000Z"}`,
		`not a record`,
	}
	return lines, claudeArchiveOf(t, lines, lines[:2])
}

// A PERSON'S READ OF ANY CLAUDE CODE LINE IS ITS WHOLE-LINE READ'S: whatever
// line a session's transcript holds, at value bounds of 64 bytes and a
// kibibyte, unfiltered and filtered by a term and by kind, the read on the
// scanner reads it without failing on its own, and where both it and the
// oracle read it to a page, the page is the oracle's. Seeded with every
// variant, alone and all together.
//
// Mutation: read a block's text into its call's entry, and a seed with a
// block's type written last fails.
func FuzzAPersonsReadOfAnyClaudeCodeLineIsItsWholeLineRead(f *testing.F) {
	variants := claudePersonVariants()
	for _, v := range variants {
		f.Add(v)
	}
	f.Add(strings.Join(variants, "\n"))
	reads := []personRead{{bounds: TranscriptReadBounds(), valueMax: 64},
		{bounds: TranscriptReadBounds(), valueMax: 1 << 10, filter: TranscriptFilter{Terms: []string{"words"}}},
		{bounds: SeatTranscriptBounds(), valueMax: 1 << 10, filter: TranscriptFilter{Kinds: []EntryKind{EntryToolResult}}}}
	f.Fuzz(func(t *testing.T, line string) {
		archive := claudeArchiveOf(t, strings.Split(line, "\n"), nil)
		for _, r := range reads {
			want := personReadOf(t, wholeClaudeCode{}, archive, r)
			if got := personReadOf(t, ClaudeCode{}, archive, r); got != want {
				t.Errorf("read %+v:\n%s", r, firstApart(got, want))
			}
		}
	})
}
