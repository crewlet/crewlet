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

// kimiMembers is a Kimi archive's main agent's wire.jsonl holding main and a
// subagent's holding sub.
func kimiMembers(main, sub []string) []boundedMember {
	members := []boundedMember{{name: "wd_a_0/session_x/agents/main/wire.jsonl", lines: main}}
	if sub != nil {
		members = append(members, boundedMember{name: "wd_a_0/session_x/agents/sub1/wire.jsonl", lines: sub})
	}
	return members
}

// kimiPersonVariants are wire.jsonl lines written every way a person's read on
// the walk reads apart from a line held whole, past a value bound of a
// kibibyte: a person's message joined from parts, opening with a heading
// within and past the bound, a part past it written in order and out of it, a
// model's part, a call's input of every kind and a tool's name past it, a
// result's output as a string, parts, a list of no parts and a value, each
// within and past it, a summary, members of another kind than their field's,
// a type written last, and lines that are no record.
func kimiPersonVariants() []string {
	h := boundedWords()
	q := boundedQuoted
	part := func(text string) string { return `{"type":"text","text":` + q(text) + `}` }
	msg := func(origin string, parts ...string) string {
		return `{"type":"context.append_message","time":1795325833665,"message":{"role":"user",` +
			`"origin":{"kind":"` + origin + `"},"content":[` + strings.Join(parts, ",") + `]}}`
	}
	event := func(ev string) string {
		return `{"type":"context.append_loop_event","time":1795325833700,"event":` + ev + `}`
	}
	call := func(name, args string) string {
		return event(`{"type":"tool.call","toolCallId":"c1","name":` + q(name) + `,"args":` + args + `}`)
	}
	result := func(output string) string {
		return event(`{"type":"tool.result","toolCallId":"c1","result":{"output":` + output + `,"isError":true}}`)
	}
	return append([]string{
		msg("user", part("the brief")),
		msg("user", part(h.note+"the note")),
		msg("user", part(h.note+h.words)),
		msg("user", part("one "), `{"type":"image","text":"not words"}`, `null`, part(h.words), part(h.words)),
		msg("user", `{"text":`+q(h.words)+`,"type":"text"}`),
		msg("user", `{"type":"text","text":`+q(h.words+h.secret)+`}`),
		msg("user", part(h.blank)),
		msg("injected", part("a reminder")),
		`{"time":5,"message":{"origin":{"kind":"user"},"content":[{"text":"x","type":"text"}],"role":"user"},` +
			`"type":"context.append_message"}`,
		event(`{"type":"content.part","part":{"type":"text","text":"the model's words"}}`),
		event(`{"type":"content.part","part":{"text":` + q(h.words) + `,"type":"text"}}`),
		event(`{"type":"content.part","part":{"type":"text","text":` + q(h.blank) + `}}`),
		event(`{"type":"content.part","part":{"type":"think","text":"thinking"}}`),
		call("Shell", `{"command":"ls -la","n":1}`),
		call("Shell", `{"command":`+q(h.words)+`}`),
		call(strings.Repeat("t", 300), `"a string"`),
		call(strings.Repeat("tool ", 300), `[1, 2]`),
		call("Shell", `null`),
		call("Shell", `12.5e3`),
		event(`{"type":"tool.call","toolCallId":"c2","name":"Read"}`),
		result(`"the output"`),
		result(q(h.words + h.secret)),
		result(`[{"type":"text","text":"part one"},{"type":"text","text":" two"},{"type":"image","data":"x"},null]`),
		result(`[{"type":"text","text":` + q(h.words) + `},{"type":"text","text":"after"}]`),
		result(`[{"text":` + q(h.words) + `,"type":"text"}]`),
		result(`[{"type":"text","text":"a part"},"no part"]`),
		result(`[{"type":"text","text":` + q(h.words) + `},7]`),
		result(`[{"type":5,"text":"x"}]`),
		result(`{"stdout":` + q(h.words) + `}`),
		result(`null`), result(`true`), result(`-0.5`),
		event(`{"type":"tool.result","toolCallId":"c2","result":{"isError":false}}`),
		`{"type":"context.apply_compaction","time":9,"summary":"the summary"}`,
		`{"type":"context.apply_compaction","time":9,"summary":` + q(h.words) + `}`,
		`{"type":"context.apply_compaction","summary":` + q(h.blank) + `}`,
		`{"type":"usage.record","usage":{"inputOther":1},"usageScope":"turn"}`,
		`{"type":"context.apply_compaction","time":"9","summary":"x"}`,
		`{"type":"context.apply_compaction","time":1.0,"summary":"x"}`,
		`{"type":"context.apply_compaction","time":1e3,"summary":"x"}`,
		`{"type":"context.apply_compaction","time":-12,"summary":"x"}`,
		`{"type":"context.apply_compaction","time":99999999999999999999,"summary":"x"}`,
		`{"type":"context.append_message","message":"x"}`,
		`{"type":"context.append_message","message":{"content":"x"}}`,
		`{"type":"context.append_message","message":{"content":[5]}}`,
		`{"type":"context.append_message","message":{"origin":7}}`,
		event(`{"type":"content.part","part":{"type":"text","text":5}}`),
		event(`{"type":"tool.result","result":{"isError":"yes"}}`),
		event(`{"type":"tool.call","name":7}`),
		`{"type":"context.apply_compaction","summary":5}`,
		`{"type":5}`,
		`{"summary":"no type"}`,
		`{"type":"","summary":"empty type"}`,
		`{"event":{"type":"content.part","part":{"type":"text","text":"typed last"}},"time":3,` +
			`"type":"context.append_loop_event"}`,
		`{"type":"x","extra":{"deep":[1,{"a":"b"}],"s":` + q(h.words) + `}}`,
	}, boundedLines()...)
}

// A PERSON'S READ OF A KIMI CODE SESSION ON THE WALK IS ITS WHOLE-LINE READ'S:
// over every variant, alone, between a brief and a call, and all together, in
// the main agent's wire.jsonl and a subagent's, and over every fixture, the
// read on the walk, whole and in pieces of seventeen bytes, answers the page
// and the error the whole-line oracle answers ([wholeKimi]) under every
// bounded read.
//
// Mutation: split a brief past the bound, read a part's text past it written
// out of order, keep a result's parts where the list holds no part, count a
// blank line of other white space as no record, or size a line with its
// carriage returns: a variant fails.
func TestAPersonsReadOfAKimiSessionIsItsWholeLineRead(t *testing.T) {
	t.Parallel()
	reads := boundedReads()
	check := func(name string, archive []byte) {
		for _, r := range reads {
			want := personReadOf(t, wholeKimi{}, archive, r)
			if got := personReadOf(t, Kimi{}, archive, r); got != want {
				t.Errorf("%s read %+v:\n%s", name, r, firstApart(got, want))
			}
			if got := personReadOf(t, piecesKimi{n: 17}, archive, r); got != want {
				t.Errorf("%s read %+v in pieces:\n%s", name, r, firstApart(got, want))
			}
		}
	}
	brief := `{"type":"context.append_message","time":1,"message":{"role":"user","origin":{"kind":"user"},` +
		`"content":[{"type":"text","text":"the brief"}]}}`
	call := `{"type":"context.append_loop_event","time":2,"event":{"type":"tool.call","toolCallId":"c1",` +
		`"name":"Shell","args":{"command":"ls"}}}`
	variants := kimiPersonVariants()
	for i, v := range variants {
		check(fmt.Sprintf("variant %d %.120q", i, v),
			boundedArchiveOf(t, kimiMembers([]string{brief, call, v}, []string{v, brief})...))
	}
	check("every variant", boundedArchiveOf(t, kimiMembers(variants, variants)...))
	torn := kimiMembers([]string{brief, call, variants[0]}, nil)
	torn[0].torn = true
	check("a torn last line", boundedArchiveOf(t, torn...))
	for _, fixture := range kimiWireFixtures(t) {
		check(fixture.name, boundedArchiveOf(t, kimiMembers(fixture.lines, nil)...))
	}
}

// A COLLECTION'S READ OF A KIMI CODE SESSION ON THE WALK IS ITS WHOLE-LINE
// READ'S: over every fixture, every person's variant and the lines that pick a
// note's receipt apart (typed in part, torn, a line of no record between), read
// whole and in pieces of seventeen bytes, it holds the notes the whole-line
// read holds, asked of every note the record names and one it never does.
//
// Mutation: read a line one of whose members is of another kind for its
// message's id, or take a torn last line's receipt: a case fails.
func TestACollectionsReadOfAKimiSessionIsItsWholeLineRead(t *testing.T) {
	t.Parallel()
	note := func(inbox, role string) string {
		return `{"type":"context.append_message","message":{"role":"` + role + `","content":[],"id":"` +
			KimiNoteID(inbox) + `"}}`
	}
	turn := `{"type":"usage.record","usageScope":"turn"}`
	records := map[string]string{}
	for _, fixture := range kimiWireFixtures(t) {
		records[fixture.name] = strings.Join(fixture.lines, "\n") + "\n"
	}
	lines := append(kimiPersonVariants(), note("one", "user"), note("two", "assistant"), turn,
		`{"type":"usage.record","usageScope":["turn"]}`, `{"type":"context.apply_compaction","message":7}`,
		`{"type":"context.undo"}`, `{"type":"context.clear"}`, `{"message":{"id":"`+KimiNoteID("one")+`"}}`,
		`{"type":"context.append_message","message":{"role":"user","id":"`+KimiNoteID("two")+`"},"usageScope":5}`,
		`{"type":"context.append_message","message":{"role":"user","id":5}}`)
	for i, l := range lines {
		records[fmt.Sprintf("line %d %.80q", i, l)] = note("one", "user") + "\n" + l + "\n" + turn + "\n"
		records[fmt.Sprintf("torn after line %d", i)] = note("one", "user") + "\n" + turn + "\n" + l
	}
	records["every line"] = strings.Join(lines, "\n") + "\n"
	records["a torn receipt"] = note("one", "user") + "\n" + turn
	for name, record := range records {
		takes := kimiServed(append(kimiInboxesIn(record), "never")...)
		want, wantErr := kimiNotesHeldWhole(t.Context(), strings.NewReader(record), takes)
		got, err := kimiNotesHeld(t.Context(), strings.NewReader(record), takes)
		if !slices.Equal(got, want) || (err != nil) != (wantErr != nil) {
			t.Errorf("%s: held %v (%v), want %v (%v)", name, got, err, want, wantErr)
		}
		got, err = kimiNotesHeld(t.Context(), &exportPieces{r: strings.NewReader(record), n: 17}, takes)
		if !slices.Equal(got, want) || (err != nil) != (wantErr != nil) {
			t.Errorf("%s in pieces: held %v (%v), want %v (%v)", name, got, err, want, wantErr)
		}
	}
}

// kimiFixture is one recorded wire.jsonl, by its lines.
type kimiWireFixture struct {
	name  string
	lines []string
}

// kimiWireFixtures is every release's recorded wire.jsonl.
func kimiWireFixtures(t *testing.T) []kimiWireFixture {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("testdata", "kimi", "*", "*.wire.jsonl"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no wire.jsonl fixtures (%v)", err)
	}
	var out []kimiWireFixture
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, kimiWireFixture{name: path,
			lines: strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")})
	}
	return out
}

// kimiInboxesIn is every inbox id a record names a note under.
func kimiInboxesIn(record string) []string {
	var out []string
	for _, m := range regexp.MustCompile(regexp.QuoteMeta(kimiNotePrefix)+`([A-Za-z0-9_-]+)`).
		FindAllStringSubmatch(record, -1) {
		if !slices.Contains(out, m[1]) {
			out = append(out, m[1])
		}
	}
	return out
}
