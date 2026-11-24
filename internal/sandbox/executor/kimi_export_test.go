package executor

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"maps"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// KimiConfirmsATake is Kimi Code's case of the note contract's confirm path
// (TestANoteServedIsConfirmedOffItsJobsOwnRecord): the Runner's main-thread read
// answers a take asked from the session's own record, its traits stating the
// receipts it settles.
//
// Mutation: drop notesHeld (no recordConfirmer) — the run confirms nothing and
// its traits state no receipts.
func KimiConfirmsATake(t *testing.T) {
	if !confirms(Kimi{}) || !NewKimi().ThreadTraits().Receipts {
		t.Fatal("Kimi confirms no take")
	}
	box := sandbox.NewFakeSandbox("b")
	const session = "session_6752b286-da65-4fa6-b177-7c428c2282fe"
	record := PathsFor(box).KimiSessions() + "/wd_wk_221ce7ca232a/" + session + "/agents/main/wire.jsonl"
	box.Put(record, string(kimiFixture(t, kimiSupported, "steered.wire.jsonl")))
	box.ExecFunc = func(context.Context, string, sandbox.ExecOptions) (sandbox.ExecResult, error) {
		return sandbox.ExecResult{Stdout: record + "\n"}, nil
	}
	reading := NewKimi().ReadMainThread(t.Context(), box, sandbox.MainThreadAsk{Session: session,
		Takes: []sandbox.AskedTake{{Take: kimiSteeredInbox}, {Take: "never"}}})
	if !slices.Equal(reading.Receipts.Held, []string{kimiSteeredInbox}) {
		t.Fatalf("receipts = %+v, want the steered note held", reading.Receipts)
	}
}

// KimiSteersOffRoute runs the run's driver against a stand-in of `kimi web`
// whose turn ends once a note is steered, its notes route notes, and returns
// each steer it made as `<prompt_id> <text>`, and whether it posted anything
// but polls to the route.
func KimiSteersOffRoute(t *testing.T, notes string) []string {
	t.Helper()
	probe := &kimiProbe{t: t, idle: make(chan struct{}, 4)}
	cmd, stdout := kimiDriverRun(t, probe, notes)
	if code := kimiDriverEnds(t, cmd); code != 0 {
		t.Fatalf("the driver exited %d, want 0:\n%s", code, stdout)
	}
	probe.mu.Lock()
	defer probe.mu.Unlock()
	return slices.Clone(probe.steers)
}

// KimiArchiveFormat is the layout a Kimi run packs its session in.
const KimiArchiveFormat = kimiArchiveFormat

// kimiArchive is a gzipped tar of files, each named as a Kimi pack names it.
func kimiArchive(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, name := range slices.Sorted(maps.Keys(files)) {
		body := files[name]
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// kimiTranscriptOf reads archive, as a pack makes it, as a Kimi session's
// transcript entries.
func kimiTranscriptOf(t *testing.T, archive []byte) []TranscriptEntry {
	t.Helper()
	got, err := newTranscriptBuilder(0, TranscriptReadBounds(), TranscriptFilter{}).decode(t.Context(), Kimi{},
		bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	return got.Entries
}

// kimiFixtureSession is the fixtures' session, and kimiMainMember where a pack
// names its main record.
const (
	kimiFixtureSession = "session_6752b286-da65-4fa6-b177-7c428c2282fe"
	kimiMainMember     = "wd_wk_221ce7ca232a/" + kimiFixtureSession + "/agents/main/wire.jsonl"
)

// KimiWakeRead is a Kimi session handed wake by its driver, as a steered
// message in place of steered's note, read back as its transcript.
func KimiWakeRead(t *testing.T, wake string) []TranscriptEntry {
	t.Helper()
	var wire []byte
	for l := range bytes.Lines(kimiFixture(t, kimiSupported, "steered.wire.jsonl")) {
		if bytes.HasPrefix(l, []byte(`{"type":"context.append_message"`)) &&
			bytes.Contains(l, []byte(`"id":"`+KimiNoteID(kimiSteeredInbox)+`"`)) {
			var ev map[string]any
			if err := json.Unmarshal(l, &ev); err != nil {
				t.Fatal(err)
			}
			ev["message"].(map[string]any)["content"] = []map[string]any{{"type": "text", "text": wake}}
			raw, err := json.Marshal(ev)
			if err != nil {
				t.Fatal(err)
			}
			l = append(raw, '\n')
		}
		wire = append(wire, l...)
	}
	return kimiTranscriptOf(t, kimiArchive(t, map[string][]byte{kimiMainMember: wire}))
}
