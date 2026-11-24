package executor

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// hermesMembers is a Hermes archive whose dumped record holds lines.
func hermesMembers(lines []string) []boundedMember {
	return []boundedMember{{name: hermesArchiveMember, lines: lines}}
}

// hermesPersonVariants are record lines written every way a person's read on
// the walk reads apart from a row held whole, past a value bound of a
// kibibyte: a person's words, steered and not, within and past it, a list's
// parts in and out of order and refused, the model's words and calls in every
// shape tool_calls takes, a tool's result and its name, a summary, flags,
// uids, times, members of another kind than their field's or in another case,
// a row written before its table, and lines that are no row.
func hermesPersonVariants() []string {
	h := boundedWords()
	q := boundedQuoted
	long := strings.Repeat("t", 300)
	row := func(fields string) string {
		return `{"t": "messages", "r": {"id": 7, ` + fields + `, "timestamp": 1795392102.5}}`
	}
	steer := func(s string) string { return hermesNoteOpen + s + hermesNoteClose }
	calls := func(list string) string {
		return row(`"role": "assistant", "content": "", "tool_calls": ` + list)
	}
	return append([]string{
		row(`"role": "user", "content": "the brief"`),
		row(`"role": "user", "content": ` + q(h.note+h.words)),
		row(`"role": "user", "content": ` + q(h.note+"short")),
		row(`"role": "user", "content": ` + q(steer("a steered note"))),
		row(`"role": "user", "content": ` + q(steer(h.note+"a steered heading"))),
		row(`"role": "user", "content": ` + q(steer(h.words))),
		row(`"role": "user", "content": ` + q(hermesNoteOpen+h.words)),
		row(`"role": "user", "content": ` + q(hermesNoteOpen+"short")),
		row(`"role": "user", "content": ` + q(steer(h.blank))),
		row(`"role": "user", "content": ` + q(steer(""))),
		row(`"role": "user", "content": ` + q(h.blank)),
		row(`"role": "user", "content": [{"type": "text", "text": "one"}, {"type": "image_url"}, {"type": "text", "text": "two"}]`),
		row(`"role": "user", "content": [{"type": "text", "text": ` + q(h.words) + `}, {"type": "text", "text": ` +
			q(h.words+h.secret) + `}]`),
		row(`"role": "user", "content": [{"text": ` + q(h.words) + `, "type": "text"}]`),
		row(`"role": "user", "content": [{"type": "text", "text": ` + q(steer(h.words)) + `}]`),
		row(`"role": "user", "content": [{"type": "text", "text": 5}]`), row(`"role": "user", "content": [5]`),
		row(`"role": "user", "content": [{"type": "text", "text": "words before"}, 5]`),
		row(`"role": "tool", "tool_call_id": "c1", "content": [{"type": "text", "text": "words before"}, {"text": 5}]`),
		row(`"role": "user", "content": [null, {"type": "text", "text": "after a null"}]`),
		row(`"role": "user", "content": {"a": 1}`), row(`"role": "user", "content": null`),
		row(`"role": "user", "content": 7`),
		row(`"role": "assistant", "content": "the words", "finish_reason": "stop"`),
		row(`"role": "assistant", "content": ` + q(h.words+h.secret)),
		row(`"role": "assistant", "content": ` + q(steer(h.words))),
		calls(q(`[{"id": "c1", "type": "function", "function": {"name": "terminal", "arguments": "{\"command\": \"ls\"}"}}]`)),
		row(`"role": "assistant", "content": "before", "tool_calls": [{"id": "c1", "name": "flat", "arguments": {"a": [1, 2]}}]`),
		calls(q(`[{"id": "c1", "function": {"name": "f", "arguments": ` + q(`{"command": `+q(h.words)+`}`) + `}}]`)),
		calls(`[{"id": "c1", "function": {"name": "f", "arguments": {"command": ` + q(h.words) + `}}}]`),
		calls(`[{"id": "c1", "name": "  ", "function": {"name": "fn", "arguments": "{}"}}]`),
		calls(`[{"id": "c1", "name": "  ", "function": {"name": " ", "arguments": "{}"}}]`),
		calls(`[{"id": "c1", "name": "own", "arguments": "own args", "function": {"name": "fn", "arguments": "fn"}}]`),
		calls(`[{"id": "c1", "function": {"name": "fn", "arguments": "fn"}, "arguments": "own later"}]`),
		calls(`[{"id": "c1", "function": {"name": "fn", "arguments": "fn"}, "function": null}]`),
		calls(`[{"id": "c1", "name": "x", "arguments": null}]`), calls(`[{"id": "c1", "name": "x"}]`),
		calls(`[null]`), calls(`[{"id": 5}]`), calls(`[{"function": 5}]`), calls(`[{"id": "c1", "name": "a"}, 5]`),
		calls(`[{"id": "c1", "name": "   ", "arguments": "{}"}]`),
		calls(q(`not json`)), calls(q(`[1] x`)), calls(q(``)), calls(q(`null`)), calls(q(`5`)), calls(q(` [ ] `)),
		calls(q(`[{"id": "c1", "name": "x"}`)), calls(`7`), calls(`null`), calls(`{"a": 1}`),
		calls(`[{"id": "c1", "name": ` + q(long) + `}]`), calls(`[{"id": "c1", "name": ` + q(h.words) + `}]`),
		calls(q(`[{"id": "c1", "name": ` + q(h.words) + `}]`)), calls(`[{"id": ` + q(long) + `, "name": "x"}]`),
		calls(`[{"ID": "c1", "Name": "cased"}]`),
		row(`"role": "tool", "tool_call_id": "c1", "tool_name": "terminal", "content": "the result"`),
		row(`"role": "tool", "tool_call_id": "c1", "content": ` + q(h.words+h.secret)),
		row(`"role": "tool", "tool_call_id": "c1", "content": ` + q(steer(h.words))),
		row(`"role": "tool", "tool_call_id": "c9", "tool_name": "named", "content": ""`),
		row(`"role": "tool", "tool_call_id": "c9", "tool_name": "  ", "content": "x"`),
		row(`"role": "tool", "tool_call_id": "c9", "tool_name": ` + q(h.words) + `, "content": "x"`),
		row(`"role": "tool", "tool_call_id": "c9", "tool_name": ` + q(long) + `, "content": "x"`),
		row(`"role": "tool", "tool_call_id": "c1", "content": [{"type": "text", "text": ` + q(h.words) + `}]`),
		row(`"role": "tool", "tool_call_id": "c1", "content": [{"text": ` + q(h.words) + `, "type": "text"}]`),
		row(`"role": "tool", "tool_call_id": "c1", "content": [5]`), row(`"role": "tool", "content": null`),
		row(`"role": "user", "content": "the summary", "_compressed_summary": 1`),
		row(`"role": "user", "content": ` + q(h.words) + `, "_compressed_summary": 1`),
		row(`"role": "user", "content": ` + q(h.blank) + `, "_compressed_summary": 1`),
		row(`"role": "user", "content": "inactive", "active": 0, "compacted": 1`),
		row(`"role": "system", "content": "not read"`),
		row(`"role": "user", "content": "x", "active": "1"`), row(`"role": "user", "content": "x", "id": 1.5`),
		row(`"role": "user", "content": "x", "message_uid": 5`), row(`"role": "user", "content": "x", "finish_reason": 5`),
		row(`"role": "user", "content": "x", "_compressed_summary": 1e2`),
		row(`"role": "user", "content": "uid one", "message_uid": "u1"`),
		row(`"role": "user", "content": "uid long", "message_uid": ` + q(long)),
		`{"r": {"role": "user", "content": "row before table"}, "t": "messages"}`,
		`{"r": {"role": "user", "content": "x"}, "t": "sessions"}`, `{"t": "sessions", "r": 5}`,
		`{"t": "messages", "r": 5}`, `{"t": "messages"}`, `{"t": "messages", "r": null}`, `{"t": 5, "r": {}}`,
		`{"t": "", "r": {"role": "user", "content": "x"}}`, `{"t": ` + q(long) + `, "r": {}}`,
		`{"ddl": "CREATE TABLE x", "kind": "table"}`, `{"t": "messages", "r": {"Role": "user", "content": "cased key"}}`,
		`{"t": "messages", "r": {"role": "user", "content": "x", "timestamp": "1795392102.5"}}`,
		`{"t": "messages", "r": {"role": "user", "content": "x", "timestamp": "x"}}`,
		`{"t": "messages", "r": {"role": "user", "content": "x", "timestamp": -1}}`,
		`{"t": "messages", "r": {"role": "user", "content": "x", "timestamp": 1e400}}`,
		`{"t": "messages", "r": {"role": "user", "content": "x", "timestamp": true}}`,
		`{"t": "messages", "r": {"role": "user", "content": "x", "timestamp": ` + q(long) + `}}`,
		`{"t": "messages", "r": {"role": ` + q(long) + `, "content": "long role"}}`,
		`{"t": "messages", "r": {"role": "tool", "tool_call_id": ` + q(long) + `, "content": "x"}}`,
	}, boundedLines()...)
}

// A PERSON'S READ OF A HERMES SESSION ON THE WALK IS ITS WHOLE-ROW READ'S:
// over every variant, alone, between a brief and a call, and all together,
// and over every recorded record, the read on the walk, whole and in pieces
// of seventeen bytes, answers the page and the error the whole-row oracle
// answers ([wholeHermes]) under every bounded read.
//
// Mutation: read a copy's row twice, take a call's function's arguments over
// its own, unframe a held text that is no steered note, or read a list whose
// part is refused: a variant fails.
func TestAPersonsReadOfAHermesSessionIsItsWholeLineRead(t *testing.T) {
	t.Parallel()
	reads := boundedReads()
	check := func(name string, archive []byte) {
		for _, r := range reads {
			want := personReadOf(t, wholeHermes{}, archive, r)
			if got := personReadOf(t, Hermes{}, archive, r); got != want {
				t.Errorf("%s read %+v:\n%s", name, r, firstApart(got, want))
			}
			if got := personReadOf(t, piecesHermes{n: 17}, archive, r); got != want {
				t.Errorf("%s read %+v in pieces:\n%s", name, r, firstApart(got, want))
			}
		}
	}
	brief := `{"t": "messages", "r": {"id": 1, "role": "user", "content": "the brief"}}`
	call := `{"t": "messages", "r": {"id": 2, "role": "assistant", "content": "", "tool_calls": [{"id": "c1", ` +
		`"function": {"name": "bash", "arguments": "{}"}}]}}`
	variants := hermesPersonVariants()
	for i, v := range variants {
		check(fmt.Sprintf("variant %d %.120q", i, v), boundedArchiveOf(t, hermesMembers([]string{brief, call, v,
			brief})...))
	}
	check("every variant", boundedArchiveOf(t, hermesMembers(append(variants, variants...))...))
	for _, fixture := range hermesRecordFixtures(t) {
		raw := string(hermesFixture(t, "0.23.3", fixture))
		members := hermesMembers(strings.Split(strings.TrimSuffix(raw, "\n"), "\n"))
		members[0].torn = !strings.HasSuffix(raw, "\n")
		check(fixture, boundedArchiveOf(t, members...))
	}
}

// A COLLECTION'S READ OF A HERMES SESSION ON THE WALK IS ITS WHOLE-ROW READ'S:
// over every recorded record, every person's variant and a steered row's
// words in every shape (notes joined, a note's own newlines, empty parts, a
// part past a note's bound, a list's parts in and out of order, a framing
// split across parts, unclosed or empty), the flags that take a row out of the
// conversation, and replies unfinished, between a note and its reply and
// torn after them, read whole and in pieces of seventeen bytes, from the start
// and past the note, it holds the notes the whole-row read holds.
//
// Mutation: start a part at every newline, read a part past a note's bound,
// take a part written before its type, or read a row's copy again: a case
// fails.
func TestACollectionsReadOfAHermesSessionIsItsWholeLineRead(t *testing.T) {
	t.Parallel()
	q := boundedQuoted
	steer := func(s string) string { return hermesNoteOpen + s + hermesNoteClose }
	note := func(id int, content string) string {
		return fmt.Sprintf(`{"t": "messages", "r": {"id": %d, "role": "user", "content": %s, "display_kind": "steer", `+
			`"message_uid": "u%d"}}`, id, content, id)
	}
	reply := func(id int) string {
		return fmt.Sprintf(`{"t": "messages", "r": {"id": %d, "role": "assistant", "content": "ok", `+
			`"finish_reason": "stop", "message_uid": "r%d"}}`, id, id)
	}
	past := strings.Repeat("x", hermesNoteMaxBytes+1)
	contents := []string{q(steer("one")), q(steer("one\ntwo")), q(steer("one\ntwo\nthree")), q(steer("two\none")),
		q(steer("one\n\ntwo")), q(steer("\n")), q(steer("")), q(steer("one\n" + past + "\ntwo")), q(steer(past)),
		q(hermesNoteOpen + "one"), q("one" + hermesNoteClose), q(steer("one\nnever\ntwo")), q(steer("one\none")),
		`[{"type": "text", "text": ` + q(steer("one")) + `}]`, `[{"text": ` + q(steer("one")) + `, "type": "text"}]`,
		`[{"type": "text", "text": ` + q(hermesNoteOpen+"on") + `}, {"type": "text", "text": ` + q("e"+hermesNoteClose) + `}]`,
		`[{"type": "image", "text": "x"}, {"type": "text", "text": ` + q(steer("one")) + `}]`,
		`[{"type": "text", "text": ` + q(steer("one")) + `, "type": "image"}]`,
		`[{"type": "text", "text": ` + q(steer("one")) + `, "text": "x"}]`, `[{"type": "text", "text": 5}]`,
		`[{"type": "text", "text": null}, {"type": "text", "text": ` + q(steer("one")) + `}]`, `5`, `null`}
	records := map[string]string{}
	for _, fixture := range hermesRecordFixtures(t) {
		records[fixture] = string(hermesFixture(t, "0.23.3", fixture))
	}
	for i, c := range contents {
		records[fmt.Sprintf("content %d %.80q", i, c)] = note(5, c) + "\n" + reply(6) + "\n"
		records[fmt.Sprintf("content %d copied", i)] = note(5, c) + "\n" + strings.Replace(note(7, c), `"u7"`, `"u5"`, 1) +
			"\n" + reply(8) + "\n"
	}
	flagged := func(flags string) string {
		return `{"t": "messages", "r": {"id": 5, "role": "user", "content": ` + q(steer("one")) +
			`, "display_kind": "steer", ` + flags + `}}` + "\n" + reply(6) + "\n"
	}
	records["an inactive note"] = flagged(`"active": 0`)
	records["a note of no flags"] = flagged(`"active": null, "compacted": null, "_compressed_summary": null`)
	records["a compacted note"] = flagged(`"compacted": 1`)
	records["a summary's note"] = flagged(`"_compressed_summary": 1`)
	records["a note shown otherwise"] = strings.Replace(flagged(`"active": 1`), `"steer"`, `"other"`, 1)
	records["a reply unfinished"] = note(5, q(steer("one"))) + "\n" + strings.Replace(reply(6), `"stop"`, `""`, 1) + "\n"
	records["a reply of no reason"] = note(5, q(steer("one"))) + "\n" + strings.Replace(reply(6), `"stop"`, `null`, 1) + "\n"
	lines := hermesPersonVariants()
	first := note(5, q(steer("one\ntwo")))
	for i, l := range lines {
		records[fmt.Sprintf("line %d %.80q", i, l)] = first + "\n" + l + "\n" + reply(9) + "\n"
		records[fmt.Sprintf("torn after line %d", i)] = first + "\n" + reply(9) + "\n" + l
	}
	records["every line"] = first + "\n" + strings.Join(lines, "\n") + "\n" + reply(9) + "\n"
	takes := []ServedTake{{InboxID: "one", Digest: ItemDigest("one")}, {InboxID: "two", Digest: ItemDigest("two")},
		{InboxID: "one-two", Digest: ItemDigest("one\ntwo")}, {InboxID: "empty", Digest: ItemDigest("")},
		{InboxID: "past", Digest: ItemDigest(past)}, {InboxID: "one again", Digest: ItemDigest("one")},
		{InboxID: "three", Digest: ItemDigest("three")}}
	for name, record := range records {
		for _, from := range []int64{0, 5} {
			for _, asked := range [][]ServedTake{takes, takes[2:], takes[:1]} {
				want, wantErr := hermesNotesHeldWhole(strings.NewReader(record), asked, from)
				got, err := hermesNotesHeld(t.Context(), strings.NewReader(record), asked, from)
				if !slices.Equal(got, want) || fmt.Sprint(err) != fmt.Sprint(wantErr) {
					t.Errorf("%s from %d of %d: held %v (%v), want %v (%v)", name, from, len(asked), got, err, want,
						wantErr)
				}
				got, err = hermesNotesHeld(t.Context(), &exportPieces{r: strings.NewReader(record), n: 17}, asked, from)
				if !slices.Equal(got, want) || fmt.Sprint(err) != fmt.Sprint(wantErr) {
					t.Errorf("%s from %d of %d in pieces: held %v (%v), want %v (%v)", name, from, len(asked), got,
						err, want, wantErr)
				}
			}
		}
	}
}

// hermesRecordFixtures is every recorded 0.23.3 record's name.
func hermesRecordFixtures(t *testing.T) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("testdata", "hermes", "0.23.3", "*.jsonl"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no record fixtures (%v)", err)
	}
	names := make([]string, len(paths))
	for i, p := range paths {
		names[i] = filepath.Base(p)
		if _, err := os.Stat(p); err != nil {
			t.Fatal(err)
		}
	}
	return names
}
