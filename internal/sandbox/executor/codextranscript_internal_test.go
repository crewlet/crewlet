package executor

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// codexArchive is rollout packed as a Codex run packs it: a gzipped tar of
// one member ([codexArchiveMember]).
func codexArchive(t *testing.T, rollout []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	zw := gzip.NewWriter(&out)
	tw := tar.NewWriter(zw)
	h := &tar.Header{Name: codexArchiveMember(codexThread), Mode: 0o600, Size: int64(len(rollout)),
		Typeflag: tar.TypeReg}
	if err := tw.WriteHeader(h); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(rollout); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// A CODEX TRANSCRIPT IS THE SESSION AS A PERSON READS IT, off its archive: the
// brief, the model's words, each call with the tool that answered it, a note
// a person sent as its own entry, a compaction's summary; never Codex's own
// context, a developer instruction, reasoning, the event lines that repeat
// each item or a fragment Codex left where it was cut off; and an item of a
// kind this read does not know said where it was.
//
// Mutation: read event_msg items, read harness-written user messages as
// briefs, drop the namespace from an MCP tool, read the note as a brief, drop
// the summary, say the fragment, or skip an unknown item silently, and the
// kinds below differ.
func TestACodexTranscriptIsTheSessionAsAPersonReadsIt(t *testing.T) {
	t.Parallel()
	more := strings.Join([]string{
		`{"timestamp":"2026-11-21T18:34:01.000Z","type":"response_item","payload":{"type":"message","role":"user",` +
			`"content":[{"type":"input_text","text":"## A note from Ada, sent while this turn was running\n\nalso add a test"}]}}`,
		`{"timestamp":"2026-11-21T18:34:02.000Z","type":"compacted","payload":{"message":"the turn so far",` +
			`"replacement_history":[{"type":"message","role":"user","content":[{"type":"input_text","text":"kept"}]}]}}`,
		`{"timestamp":"2026-11-21T18:34:03.000Z","type":"response_item","payload":{"type":"a_later_kind"}}`,
		`not a record`,
		`{"type":"response_item","payload":{"type":"custom_tool_call","name":"apply_patch","call_id":"c2","input":"*** Begin Patch"}}`,
		`{"type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"c2","output":"Done!"}}`,
		"",
	}, "\n")
	archive := codexArchive(t, append(codexRolloutFixture(t), more...))
	got, err := decodeTranscript(context.Background(), Codex{}, bytes.NewReader(archive), 0,
		TranscriptReadBounds(), TranscriptFilter{})
	if err != nil {
		t.Fatalf("read the transcript: %v", err)
	}
	var kinds []EntryKind
	for _, e := range got.Entries {
		kinds = append(kinds, e.Kind)
	}
	want := []EntryKind{
		EntryBrief, EntryToolCall, EntryToolResult, EntryModel, EntryModel,
		EntryNote, EntrySummary, EntryUnread, EntryToolCall, EntryToolResult,
	}
	if !slices.Equal(kinds, want) {
		t.Fatalf("the entries are %v, want %v", kinds, want)
	}
	e := got.Entries
	if e[0].Text != "SCENARIO:slowmcp:1 go" || e[0].At.IsZero() {
		t.Errorf("the brief reads %q at %v", e[0].Text, e[0].At)
	}
	if e[1].Tool != "mcp__stub__echo" || e[1].Text != `{"text":"slow0"}` || e[2].Tool != "mcp__stub__echo" {
		t.Errorf("the call reads %q %q, answered by %q", e[1].Tool, e[1].Text, e[2].Tool)
	}
	if !strings.Contains(e[5].Text, "also add a test") || e[6].Text != "the turn so far" {
		t.Errorf("the note reads %q and the summary %q", e[5].Text, e[6].Text)
	}
	if e[8].Tool != "apply_patch" || e[8].Text != "*** Begin Patch" || e[9].Tool != "apply_patch" || e[9].Text != "Done!" {
		t.Errorf("the custom call reads %q %q, answered %q %q", e[8].Tool, e[8].Text, e[9].Tool, e[9].Text)
	}
}

// A CODEX TRANSCRIPT'S OVER-BOUND LINE IS SAID AND ITS TORN TAIL PASSED OVER:
// a record past the line bound is a gap a person is told of, never the end of
// a transcript whose other lines read; a tail Codex never finished is no
// record, and Codex itself passes it over.
//
// Mutation: fail the read on an over-bound line, or say the torn tail, and
// the entries below differ.
func TestACodexTranscriptsOverBoundLineIsSaidAndItsTornTailPassedOver(t *testing.T) {
	t.Parallel()
	long := `{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"` +
		strings.Repeat("a", codexRolloutLineMaxBytes) + `"}]}}` + "\n"
	rollout := long + `{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"after"}]}}` +
		"\n" + `{"type":"response_item","payload":{"type":"mess`
	got, err := decodeTranscript(context.Background(), Codex{}, bytes.NewReader(codexArchive(t, []byte(rollout))), 0,
		TranscriptReadBounds(), TranscriptFilter{})
	if err != nil {
		t.Fatalf("read the transcript: %v", err)
	}
	var kinds []EntryKind
	for _, e := range got.Entries {
		kinds = append(kinds, e.Kind)
	}
	if want := []EntryKind{EntryUnread, EntryModel}; !slices.Equal(kinds, want) {
		t.Errorf("the entries are %v, want %v", kinds, want)
	}
}

// A COMPACTED CODEX SESSION'S TRANSCRIPT SAYS THE SUMMARY ONCE AND THE NOTE
// ONCE: the model's answer to the compaction request and the compacted line
// carry the same summary, which reads once, from the compacted line; the note
// the history keeps reads once, from its own item.
//
// Mutation: read the compaction's answer as the model's words, or read the
// compacted line's history, and the summary or the note reads twice.
func TestACompactedCodexSessionsTranscriptSaysTheSummaryOnceAndTheNoteOnce(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("testdata", "codex", "0.171.0", "compaction-note-before.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeTranscript(context.Background(), Codex{}, bytes.NewReader(codexArchive(t, raw)), 0,
		TranscriptReadBounds(), TranscriptFilter{})
	if err != nil {
		t.Fatalf("read the transcript: %v", err)
	}
	notes, summaries := 0, 0
	for _, e := range got.Entries {
		switch {
		case strings.HasPrefix(e.Text, "NOTE-CMPA:"):
			notes++
		case strings.Contains(e.Text, "SUMMARY-FROM-FAKE"):
			if e.Kind != EntrySummary {
				t.Errorf("the summary reads as %s", e.Kind)
			}
			summaries++
		}
	}
	if notes != 1 || summaries != 1 {
		t.Errorf("the note reads %d times and the summary %d, want each once", notes, summaries)
	}
}
