package executor

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
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
// context, a developer instruction, reasoning or the event lines that repeat
// each item; and a record this read cannot read said where it was.
//
// Mutation: read event_msg items, read harness-written user messages as
// briefs, drop the namespace from an MCP tool, read the note as a brief, drop
// the summary, or skip an unknown item silently, and the kinds below differ.
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
		EntryNote, EntrySummary, EntryUnread, EntryUnread, EntryToolCall, EntryToolResult,
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
	if e[9].Tool != "apply_patch" || e[9].Text != "*** Begin Patch" || e[10].Tool != "apply_patch" || e[10].Text != "Done!" {
		t.Errorf("the custom call reads %q %q, answered %q %q", e[9].Tool, e[9].Text, e[10].Tool, e[10].Text)
	}
}

// A CODEX TRANSCRIPT'S TORN OR OVER-BOUND LINE IS SAID, AND THE READ GOES ON:
// one line the read cannot take is a gap a person is told of, never the end
// of a transcript whose other lines read.
//
// Mutation: fail the read on an over-bound line, or drop the torn tail
// silently, and the entries below differ.
func TestACodexTranscriptsTornOrOverBoundLineIsSaid(t *testing.T) {
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
	if want := []EntryKind{EntryUnread, EntryModel, EntryUnread}; !slices.Equal(kinds, want) {
		t.Errorf("the entries are %v, want %v", kinds, want)
	}
}
