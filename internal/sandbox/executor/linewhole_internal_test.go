package executor

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/redact/redacttest"
)

// THE WHOLE-LINE READS OF A BOUNDED WALK'S RECORDS (Kimi Code's, Copilot's,
// Pi's, Grok Build's and Muse Code's) are the oracles a person's read and a
// collection's read on the walk are held to: each line held and decoded whole,
// as the reads were before they moved onto the walk, with what the walk
// decides applied where it decides it, past the value bound: R1, a person's
// words are one entry, and a hook's carry none; R2, a joined part is kept
// whatever it holds where written in order, and a join holding one written
// otherwise is said in its place ([textJoin]); R3, a call's arguments written
// as a string are shown as written; R6, a tool's name whose redaction the
// stream refuses makes its call said in its place.

// boundedMember is one member of a bounded walk's archive: its name, and its
// lines, each ended by a break unless torn ends the last without one.
type boundedMember struct {
	name  string
	lines []string
	torn  bool
}

// boundedArchiveOf is a gzipped tar of members.
func boundedArchiveOf(t testing.TB, members ...boundedMember) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, m := range members {
		body := strings.Join(m.lines, "\n")
		if len(m.lines) > 0 && !m.torn {
			body += "\n"
		}
		if err := tw.WriteHeader(&tar.Header{Name: m.name, Mode: 0o600, Size: int64(len(body)),
			Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// joinedWhole is raw, a list's parts that decode as the join's struct, joined
// by rule at valueMax as the walk joins them, and whether a part whose text
// runs past valueMax is written out of order, which loses the join (R2).
func joinedWhole(raw json.RawMessage, rule joinRule, valueMax int) (string, bool) {
	var parts []json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return "", false
	}
	var kept []string
	for _, part := range parts {
		if absent(part) {
			continue
		}
		var kind, text string
		types, texts, typeFirst, past := 0, 0, false, false
		_ = eachKey(part, func(name []byte, value json.RawMessage, _ int) error {
			switch string(name) {
			case "type":
				types++
				typeFirst = typeFirst || texts == 0
				kind = ""
				_ = json.Unmarshal(value, &kind)
			case "text":
				texts++
				text = ""
				_ = json.Unmarshal(value, &text)
				past = past || len(text) > valueMax
			}
			return nil
		})
		switch {
		case past && (texts != 1 || !rule.anyKind && (types != 1 || !typeFirst)):
			return "", true
		case !rule.anyKind && kind != rule.kind:
		case past, !rule.dropBlank || strings.TrimSpace(text) != "":
			kept = append(kept, text)
		}
	}
	return strings.Join(kept, rule.sep), false
}

// lostJoin says, as the walk does, a join a part past the bound written out of
// order lost.
func (b *transcriptBuilder) lostJoin() error {
	return b.add(TranscriptEntry{Kind: EntryUnread, Thread: b.thread}, fmt.Sprintf("a text of the "+
		"harness's record runs past %s in a part written out of order, and was not read",
		humanSize(int64(b.valueMax))))
}

// refusedName reports whether a tool's name the walk streams past the bound is
// one whose redaction the read's stream refuses (R6).
func (b *transcriptBuilder) refusedName(name string) bool {
	return b.redactor != nil && len(name) > b.valueMax
}

// boundedReads are the reads every bounded walk's differential takes: a
// person's and a seat's, from the start and later, unfiltered and filtered by
// kind and by term, at the value bound and below, and with every redaction
// refused.
func boundedReads() []personRead {
	person, seat := TranscriptReadBounds(), SeatTranscriptBounds()
	calls := TranscriptFilter{Kinds: []EntryKind{EntryToolCall, EntryToolResult}}
	terms := TranscriptFilter{Terms: []string{"words"}}
	return []personRead{{bounds: person, valueMax: TranscriptValueMaxBytes}, {bounds: person, valueMax: 1 << 10},
		{bounds: seat, valueMax: 1 << 10, from: 2, filter: calls},
		{bounds: person, valueMax: 1 << 10, filter: terms},
		{bounds: seat, valueMax: 512, filter: terms, from: 1},
		{bounds: person, valueMax: 1 << 10, refuse: true},
		{bounds: person, valueMax: 1 << 10, from: 3},
		{bounds: person, valueMax: 1 << 10, filter: TranscriptFilter{Kinds: []EntryKind{EntryModel, EntryBrief}}},
		{bounds: person, valueMax: 1 << 10, filter: TranscriptFilter{Kinds: []EntryKind{EntryNote, EntryLateCall}}},
		{bounds: TranscriptBounds{Entries: 3, Bytes: 4 << 10, EntryBytes: 1 << 10, ReadBytes: 1 << 20,
			ReadLines: 5}, valueMax: 1 << 10}}
}

// boundedHeld is the words the variants every bounded walk's differential
// writes hold: words past a kibibyte, a heading's note, a run of blanks past
// it, and a credential.
type boundedHeld struct {
	words, note, blank, secret string
}

func boundedWords() boundedHeld {
	return boundedHeld{words: strings.Repeat("words of the model, ", 100),
		note:  noteHeadOpen + "ada" + noteHeadClose + "\nTarget main.\n",
		blank: strings.Repeat(" \n", 1000), secret: redacttest.Shapes()[0].Sample}
}

// boundedQuoted is s as one JSON string.
func boundedQuoted(s string) string {
	raw, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

// boundedLines are lines every bounded walk reads as a line of no record, or
// none: blank by JSON and by Unicode (one past a read's chunk, a character
// across its edge), no object, not JSON, ended by carriage returns.
func boundedLines() []string {
	return []string{"", "   ", "\t\r", "\u00a0", "\v", "\u2028 ", strings.Repeat("\u2028", 30000),
		strings.Repeat("\u2028", 30000) + "x", `[1]`, `null`, `"x"`, `7`, `{`, `{}x`, "not a record\r\r",
		`{"type":"x"}` + "\r", "\xff", "{\"type\":\"a\xffb\"}", "\xe2\x80"}
}

// A LINE AT THE LINE BOUND IS READ AND ONE PAST IT SAID, ITS BREAK AMONG IT,
// as the whole-line read bounds a line, on every bounded walk: a line of the
// bound less its break and a last line of the bound are read; a line of the
// bound and its break, and a last line one byte longer, are said, sized with
// the break and counted toward no lines bound; and a collection's read passes
// each over as the whole-line one does, a torn one and one ended by blanks
// past the bound among them, failing in the same words where it fails.
//
// Mutation: count the break apart from the bound or a line's size, count a
// line past it toward the lines bound, or take one past it as a line: a case
// fails.
func TestABoundedLineAtTheLineBoundIsReadAndOnePastItSaid(t *testing.T) {
	t.Parallel()
	type harness struct {
		name          string
		walk, whole   carrier
		members       func(lines []string) []boundedMember
		note, receipt string
		head          string
		held, oracle  func(context.Context, io.Reader, []ServedTake) ([]string, error)
	}
	const at = `"timestamp":"2026-01-01T00:00:00.000Z"`
	for _, h := range []harness{
		{name: KimiName, walk: Kimi{}, whole: wholeKimi{}, held: kimiNotesHeld, oracle: kimiNotesHeldWhole,
			members: func(lines []string) []boundedMember { return kimiMembers(lines, nil) },
			note:    `{"type":"context.append_message","message":{"role":"user","id":"` + KimiNoteID("one") + `"}}`,
			receipt: `{"type":"usage.record","usageScope":"turn"}`,
			head:    `{"type":"context.apply_compaction","summary":"at the bound","pad":"`},
		{name: CopilotName, walk: Copilot{}, whole: wholeCopilot{}, held: copilotNotesHeld,
			oracle: copilotNotesHeldWhole, members: copilotMembers,
			note: `{"type":"user.message",` + at + `,"data":{"clientTurnId":"` + CopilotClientTurnID("one") +
				`","delivery":"steering"}}`,
			receipt: `{"type":"session.usage_record","timestamp":"2026-01-01T00:00:01.000Z","data":{"usage":` +
				`{"interactionType":"` + copilotMainRequest + `","duration":500}}}`,
			head: `{"type":"session.compaction_complete","data":{"summaryContent":"at the bound"},"pad":"`},
		{name: PiName, walk: Pi{}, whole: wholePi{}, held: piNotesHeld, oracle: piNotesHeldWhole,
			members: piMembers,
			note:    `{"type":"custom_message","id":"n1","customType":"` + piNoteType + `","details":{"inbox":"one"}}`,
			receipt: `{"type":"message","id":"a1","parentId":"n1","message":{"role":"assistant",` +
				`"stopReason":"stop"}}`,
			head: `{"type":"compaction","id":"c1","parentId":"n1","summary":"at the bound","pad":"`},
		{name: GrokName, walk: Grok{}, whole: wholeGrok{}, members: grokMembers,
			held: func(ctx context.Context, r io.Reader, _ []ServedTake) ([]string, error) {
				return notesBenchTokens(grokHeld(ctx, r))
			},
			oracle: func(_ context.Context, r io.Reader, _ []ServedTake) ([]string, error) {
				return notesBenchTokens(grokHeldWhole(r))
			},
			note: grokHookLine(grokHookHeads[0], "## A note from ada\n\nUse main.\n\n", claudeTakeToken(notesBenchTake),
				grokReminderTail, "system_reminder"),
			receipt: `{"type":"assistant","content":"ok"}`,
			head:    `{"type":"tool_result","tool_call_id":"c1","content":"at the bound","pad":"`},
		{name: MuseName, walk: Muse{}, whole: wholeMuse{}, members: museMembers,
			held: func(ctx context.Context, r io.Reader, _ []ServedTake) ([]string, error) {
				return notesBenchTokens(museHeld(ctx, r, museHooksDigest, map[string]bool{
					claudeTakeToken(notesBenchTake): true}))
			},
			oracle: func(_ context.Context, r io.Reader, _ []ServedTake) ([]string, error) {
				return notesBenchTokens(museHeldWhole(r, museHooksDigest, map[string]bool{
					claudeTakeToken(notesBenchTake): true}))
			},
			note:    museNoteRun("c1", claudeTakeToken(notesBenchTake)),
			receipt: museEventLine(`{"kind":"model_completed"}`),
			head: `{"payload_type":"runtime.session","recorded_at":1,"payload":{"event":{"kind":` +
				`"context_compaction_installed","replacement":{"text":"at the bound"}}},"pad":"`},
	} {
		t.Run(h.name, func(t *testing.T) {
			t.Parallel()
			line := func(n int) string { return h.head + strings.Repeat("x", n-len(h.head)-2) + `"}` }
			blanks := func(n int) string { return h.receipt + strings.Repeat(" ", n-len(h.receipt)) }
			r := personRead{bounds: TranscriptReadBounds(), valueMax: TranscriptValueMaxBytes}
			r.bounds.ReadLines = 3 + strings.Count(h.note, "\n")
			for _, c := range []struct {
				name  string
				lines []string
				torn  bool
			}{
				{"a line of the bound less its break", []string{h.note, line(maxLineBytes - 1), h.receipt}, false},
				{"a line of the bound and its break", []string{h.note, line(maxLineBytes), h.receipt, h.receipt},
					false},
				{"a last line of the bound", []string{h.note, h.receipt, line(maxLineBytes)}, true},
				{"a last line past the bound", []string{h.note, h.receipt, line(maxLineBytes + 1)}, true},
				{"a receipt past the bound in its blanks", []string{h.note, blanks(maxLineBytes)}, false},
				{"a line past the bound whose break its size shows", []string{h.note, line(pastBreakShown),
					h.receipt}, false},
			} {
				members := h.members(c.lines)
				members[0].torn = c.torn
				archive := boundedArchiveOf(t, members...)
				if got, want := personReadOf(t, h.walk, archive, r), personReadOf(t, h.whole, archive, r); got != want {
					t.Errorf("%s:\n%s", c.name, firstApart(got, want))
				}
				record := strings.Join(c.lines, "\n")
				if !c.torn {
					record += "\n"
				}
				takes := served("one")
				want, wantErr := h.oracle(t.Context(), strings.NewReader(record), takes)
				if got, err := h.held(t.Context(), strings.NewReader(record), takes); fmt.Sprint(err) != fmt.Sprint(wantErr) ||
					!slices.Equal(got, want) {
					t.Errorf("%s: a collection holds %v (%v), want %v (%v)", c.name, got, err, want, wantErr)
				}
			}
		})
	}
}

// pastBreakShown is a line past the line bound whose size said with its break
// is a tenth of a MiB more, as [humanSize] rounds it, than without.
const pastBreakShown = 33606860
