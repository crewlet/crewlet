package executor

import (
	"bytes"
	"encoding/json"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// PiConfirmsATake is Pi's case of the note contract's confirm path
// (TestANoteServedIsConfirmedOffItsJobsOwnRecord): the Runner's main-thread read
// answers a take asked from the session's own record, its traits stating the
// receipts it settles.
//
// Mutation: drop notesHeld (no recordConfirmer): the run confirms nothing and
// its traits state no receipts.
func PiConfirmsATake(t *testing.T) {
	if !confirms(Pi{}) || !NewPi().ThreadTraits().Receipts {
		t.Fatal("Pi confirms no take")
	}
	box := sandbox.NewFakeSandbox("b")
	const session = "22222222-2222-4222-8222-222222222222"
	box.Put(PathsFor(box).PiSessionFile(session), string(piFixture(t, "steered.session.jsonl")))
	reading := NewPi().ReadMainThread(t.Context(), box, sandbox.MainThreadAsk{Session: session,
		Takes: []sandbox.AskedTake{{Take: "inbox-steer-1", Digest: "sha256:not-read"}, {Take: "inbox-never"}}})
	if !slices.Equal(reading.Receipts.Held, []string{"inbox-steer-1"}) {
		t.Fatalf("receipts = %+v, want inbox-steer-1 held", reading.Receipts)
	}
}

// PiWakeRead is a Pi session handed wake by its extension, as the custom
// message in place of steered's note, read back as its transcript.
func PiWakeRead(t *testing.T, wake string) []TranscriptEntry {
	t.Helper()
	var record []byte
	for l := range bytes.Lines(piFixture(t, "steered.session.jsonl")) {
		if bytes.Contains(l, []byte(`"customType":"crewlet-note"`)) {
			var e map[string]any
			if err := json.Unmarshal(l, &e); err != nil {
				t.Fatal(err)
			}
			e["content"] = wake
			raw, err := json.Marshal(e)
			if err != nil {
				t.Fatal(err)
			}
			l = append(raw, '\n')
		}
		record = append(record, l...)
	}
	const session = "22222222-2222-4222-8222-222222222222"
	got, err := newTranscriptBuilder(0, TranscriptReadBounds(), TranscriptFilter{}).decode(t.Context(), Pi{},
		bytes.NewReader(piArchive(t, session, record)))
	if err != nil {
		t.Fatal(err)
	}
	return got.Entries
}

// PiArchiveFormat is the layout a Pi run packs its session in.
const PiArchiveFormat = piArchiveFormat

// PiProbe is the stand-in of pi's extension API the suite runs the rendered
// extension against (pi_extension_probe.mjs).
const PiProbe = "pi_extension_probe.mjs"
