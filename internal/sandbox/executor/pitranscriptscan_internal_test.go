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

// piMembers is a Pi archive whose session file holds lines.
func piMembers(lines []string) []boundedMember {
	return []boundedMember{{name: ".pi/sessions/s1.jsonl", lines: lines}}
}

// piPersonVariants are session lines written every way a person's read on the
// walk reads apart from a line held whole, past a value bound of a kibibyte: a
// message's content as a string and as blocks, joined within and past it,
// blank, a block past it written in order and out of it, tool calls before and
// after the words, of no shape and naming tools past it, a result's, a note's
// content opening with a heading within and past it, a summary, members of
// another kind than their field's, and lines that are no record.
func piPersonVariants() []string {
	h := boundedWords()
	q := boundedQuoted
	const stamp = `"timestamp":"2026-01-01T00:00:00.000Z"`
	msg := func(role, content, more string) string {
		return `{"type":"message","id":"m1","parentId":"p",` + stamp + `,"message":{"role":"` + role +
			`","content":` + content + more + `}}`
	}
	text := func(s string) string { return `{"type":"text","text":` + q(s) + `}` }
	blocks := func(b ...string) string { return `[` + strings.Join(b, ",") + `]` }
	callOf := func(name, args string) string {
		return `{"type":"toolCall","id":"c1","name":` + q(name) + `,"arguments":` + args + `}`
	}
	note := func(content string) string {
		return `{"type":"custom_message","id":"n1",` + stamp + `,"customType":"` + piNoteType + `","content":` +
			content + `,"details":{"inbox":"i1"}}`
	}
	return append([]string{
		msg("user", q("the brief"), ""), msg("user", q(h.note+h.words), ""), msg("user", q(h.blank), ""),
		msg("user", blocks(text("one"), text(" "), text("two"), `null`, `{"type":"image","data":"x"}`), ""),
		msg("user", blocks(text(h.words), text(h.words+h.secret)), ""),
		msg("user", blocks(`{"text":`+q(h.words)+`,"type":"text"}`), ""),
		msg("user", blocks(text("a"), `5`), ""), msg("user", `{"a":1}`, ""), msg("user", `null`, ""),
		msg("assistant", blocks(text("the words"), callOf("bash", `{"command":"ls"}`)), `,"stopReason":"toolUse"`),
		msg("assistant", blocks(callOf("bash", `{"command":`+q(h.words)+`}`), text(h.words)), ""),
		msg("assistant", blocks(callOf("read", `"a string"`), text(" "), callOf("x", `null`)), ""),
		msg("assistant", blocks(`{"arguments":{"a":1},"name":"late","type":"toolCall"}`,
			`{"type":"thinking","thinking":"hmm"}`), ""),
		msg("assistant", blocks(callOf(strings.Repeat("t", 300), `[1]`), callOf(h.words, `{}`)), ""),
		msg("assistant", blocks(text(h.blank), text("after")), ""),
		msg("assistant", blocks(text(h.blank)), ""),
		msg("assistant", blocks(callOf("bash", `{}`), `{"type":"toolCall","id":5}`), ""),
		msg("assistant", q("a string's words"), `,"usage":{"input":1,"output":2}`),
		msg("toolResult", blocks(text("the result")), `,"toolCallId":"c1","isError":true`),
		msg("toolResult", blocks(text(h.words+h.secret)), `,"toolCallId":"c1"`),
		msg("toolResult", blocks(text(h.blank)), `,"toolCallId":"c1"`),
		msg("toolResult", blocks(text(" ")), `,"toolCallId":"c1"`),
		msg("toolResult", blocks(`{"text":`+q(h.words)+`,"type":"text"}`, text("x")), `,"toolCallId":"c1"`),
		msg("toolResult", `7`, `,"toolCallId":"c1"`),
		`{"type":"message","id":"m2","message":{"role":"toolResult","toolCallId":"c9"}}`,
		msg("system", q("not read"), ""), `{"type":"message","id":"m3","message":null}`,
		note(q("a note")), note(q(h.note + "the note")), note(q(h.note + h.words)), note(q(h.blank)),
		note(blocks(text(h.note+"in blocks"), text("more"))), note(blocks(text(h.words), `5`)),
		note(blocks(`{"text":` + q(h.words) + `,"type":"text"}`)), note(`null`), note(`{"a":1}`),
		`{"type":"custom_message","customType":"other","content":"not ours"}`,
		`{"type":"compaction",` + stamp + `,"summary":"the summary"}`,
		`{"type":"compaction","summary":` + q(h.words) + `}`, `{"type":"compaction","summary":` + q(h.blank) + `}`,
		`{"type":"compaction","summary":5}`, `{"type":"message","message":"x"}`,
		msg("assistant", q("x"), `,"isError":"no"`), msg("assistant", q("x"), `,"usage":{"input":"1"}`),
		msg("assistant", q("x"), `,"usage":{"input":1.5}`), `{"type":"message","id":7}`,
		`{"type":"message","timestamp":5}`, `{"type":5}`, `{"id":"x"}`, `{"type":""}`,
		`{"message":{"role":"user","content":"typed last"},"type":"message"}`,
		`{"type":"session","version":3,"id":"s","cwd":"/w"}`,
	}, boundedLines()...)
}

// A PERSON'S READ OF A PI SESSION ON THE WALK IS ITS WHOLE-LINE READ'S: over
// every variant, alone, between a brief and a call, and all together, and over
// every fixture, the read on the walk, whole and in pieces of seventeen bytes,
// answers the page and the error the whole-line oracle answers ([wholePi])
// under every bounded read.
//
// Mutation: keep a blank block, place a call before the words' entry, read a
// block past the bound written out of order, or keep a message's words where
// a block is of no shape: a variant fails.
func TestAPersonsReadOfAPiSessionIsItsWholeLineRead(t *testing.T) {
	t.Parallel()
	reads := boundedReads()
	check := func(name string, archive []byte) {
		for _, r := range reads {
			want := personReadOf(t, wholePi{}, archive, r)
			if got := personReadOf(t, Pi{}, archive, r); got != want {
				t.Errorf("%s read %+v:\n%s", name, r, firstApart(got, want))
			}
			if got := personReadOf(t, piecesPi{n: 17}, archive, r); got != want {
				t.Errorf("%s read %+v in pieces:\n%s", name, r, firstApart(got, want))
			}
		}
	}
	brief := `{"type":"message","message":{"role":"user","content":"the brief"}}`
	call := `{"type":"message","message":{"role":"assistant","content":[{"type":"toolCall","id":"c1","name":"bash"}]}}`
	variants := piPersonVariants()
	for i, v := range variants {
		check(fmt.Sprintf("variant %d %.120q", i, v), boundedArchiveOf(t, piMembers([]string{brief, call, v,
			brief})...))
	}
	check("every variant", boundedArchiveOf(t, piMembers(variants)...))
	for _, fixture := range piSessionFixtures(t) {
		check(fixture, boundedArchiveOf(t, piMembers(strings.Split(strings.TrimSuffix(
			piFixtureAt(t, fixture), "\n"), "\n"))...))
	}
}

// A COLLECTION'S READ OF A PI SESSION ON THE WALK IS ITS WHOLE-LINE READ'S:
// over every fixture, every person's variant and the lines that pick a note's
// receipt apart (a branch, an edit, a compaction, ids past the word bound,
// members of another kind), each between a note and its answer and torn after
// them, read whole and in pieces of seventeen bytes, it holds the notes the
// whole-line read holds.
//
// Mutation: read a line one of whose members is of another kind as an entry,
// or tell two ids apart by their first bytes alone: a case fails.
func TestACollectionsReadOfAPiSessionIsItsWholeLineRead(t *testing.T) {
	t.Parallel()
	long := func(c string) string { return strings.Repeat("x", 300) + c }
	note := `{"type":"custom_message","id":"n1","parentId":"","customType":"` + piNoteType +
		`","content":"x","details":{"inbox":"one"}}`
	answer := func(id, parent string) string {
		return `{"type":"message","id":"` + id + `","parentId":"` + parent + `","message":{"role":"assistant",` +
			`"stopReason":"stop"}}`
	}
	records := map[string]string{}
	for _, fixture := range piSessionFixtures(t) {
		records[fixture] = piFixtureAt(t, fixture)
	}
	lines := append(piPersonVariants(), answer("a0", "n1"), answer("a1", "zz"),
		`{"type":"compaction","id":"c1","parentId":"n1"}`,
		`{"type":"context_edit","id":"e1","parentId":"n1","targetId":"n1"}`,
		`{"type":"context_edit","id":"e1","parentId":"n1","targetId":5}`,
		`{"type":"message","id":"m1","parentId":"n1","message":{"role":"assistant","stopReason":"error"}}`,
		`{"type":"message","id":"m1","parentId":"n1","message":{"role":"assistant","stopReason":"stop",`+
			`"usage":{"input":"x"}}}`,
		`{"type":"custom_message","id":"n2","parentId":"n1","customType":"`+piNoteType+`","details":"x"}`,
		`{"type":"custom_message","id":"n2","parentId":"n1","customType":"`+piNoteType+
			`","details":{"inbox":5}}`,
		`{"type":"message","id":"`+long("a")+`","parentId":"n1","message":{"role":"user"}}`,
		answer(long("b"), long("a")))
	for i, l := range lines {
		records[fmt.Sprintf("line %d %.80q", i, l)] = note + "\n" + l + "\n" + answer("a9", "n1") + "\n"
		records[fmt.Sprintf("torn after line %d", i)] = note + "\n" + answer("a9", "n1") + "\n" + l
		records[fmt.Sprintf("line %d last", i)] = note + "\n" + answer("a9", "n1") + "\n" + l + "\n"
	}
	records["every line"] = note + "\n" + strings.Join(lines, "\n") + "\n"
	for name, record := range records {
		takes := served(append(piInboxesIn(record), "one", "never")...)
		want, wantErr := piNotesHeldWhole(t.Context(), strings.NewReader(record), takes)
		got, err := piNotesHeld(t.Context(), strings.NewReader(record), takes)
		if !slices.Equal(got, want) || (err != nil) != (wantErr != nil) {
			t.Errorf("%s: held %v (%v), want %v (%v)", name, got, err, want, wantErr)
		}
		got, err = piNotesHeld(t.Context(), &exportPieces{r: strings.NewReader(record), n: 17}, takes)
		if !slices.Equal(got, want) || (err != nil) != (wantErr != nil) {
			t.Errorf("%s in pieces: held %v (%v), want %v (%v)", name, got, err, want, wantErr)
		}
	}
}

// piSessionFixtures is every recorded session file's path.
func piSessionFixtures(t *testing.T) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("testdata", "pi", "*", "*.session.jsonl"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no session fixtures (%v)", err)
	}
	return paths
}

// piFixtureAt is the fixture at path.
func piFixtureAt(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// piInboxesIn is every inbox id a record's details name.
func piInboxesIn(record string) []string {
	var out []string
	for _, m := range regexp.MustCompile(`"inbox":"([^"]+)"`).FindAllStringSubmatch(record, -1) {
		if !slices.Contains(out, m[1]) {
			out = append(out, m[1])
		}
	}
	return out
}
