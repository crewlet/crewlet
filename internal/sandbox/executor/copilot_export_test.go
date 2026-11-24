package executor

import (
	"bytes"
	"encoding/json"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// CopilotConfirmsATake is Copilot's case of the note contract's confirm path
// (TestANoteServedIsConfirmedOffItsJobsOwnRecord): the Runner's main-thread read
// answers a take asked from the session's own record, its traits stating the
// receipts it settles.
//
// Mutation: drop notesHeld (no recordConfirmer) — the run confirms nothing and
// its traits state no receipts.
func CopilotConfirmsATake(t *testing.T) {
	if !confirms(Copilot{}) || !NewCopilot().ThreadTraits().Receipts {
		t.Fatal("Copilot confirms no take")
	}
	box := sandbox.NewFakeSandbox("b")
	const session = "55555555-5555-4555-8555-555555555555"
	box.Put(PathsFor(box).CopilotSessionDir(session)+"/events.jsonl", string(copilotFixture(t, "steered.events.jsonl")))
	reading := NewCopilot().ReadMainThread(t.Context(), box, sandbox.MainThreadAsk{Session: session,
		Takes: []sandbox.AskedTake{{Take: "msg_inbox1", Digest: "sha256:not-read"}, {Take: "msg_never"}}})
	if !slices.Equal(reading.Receipts.Held, []string{"msg_inbox1"}) {
		t.Fatalf("receipts = %+v, want msg_inbox1 held", reading.Receipts)
	}
}

// CopilotWakeRead is a Copilot session handed wake by its extension, as a
// steering message in place of steered's note, read back as its transcript.
func CopilotWakeRead(t *testing.T, wake string) []TranscriptEntry {
	t.Helper()
	var events []byte
	for l := range bytes.Lines(copilotFixture(t, "steered.events.jsonl")) {
		if bytes.Contains(l, []byte(`"clientTurnId":"crewlet-note:msg_inbox1"`)) {
			var ev map[string]any
			if err := json.Unmarshal(l, &ev); err != nil {
				t.Fatal(err)
			}
			ev["data"].(map[string]any)["content"] = wake
			raw, err := json.Marshal(ev)
			if err != nil {
				t.Fatal(err)
			}
			l = append(raw, '\n')
		}
		events = append(events, l...)
	}
	archive := copilotArchive(t, map[string][]byte{
		"session-state/55555555-5555-4555-8555-555555555555/events.jsonl": events})
	got, err := newTranscriptBuilder(0, TranscriptReadBounds(), TranscriptFilter{}).decode(t.Context(), Copilot{},
		bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	return got.Entries
}

// CopilotArchiveFormat is the layout a Copilot run packs its session in.
const CopilotArchiveFormat = copilotArchiveFormat

// CopilotProbeSDK is the stand-in of Copilot's extension SDK the suite runs the
// rendered extension against (copilot_steer_probe.mjs).
const CopilotProbeSDK = "copilot_steer_probe.mjs"

// NewCopilotUnderPolicy is the runner whose guard reads the managed policy at
// file, held to owner's uid where the machine's is held to root's: the suite
// runs as no root and may not write /etc.
func NewCopilotUnderPolicy(file string, owner int) *Runner {
	return New(Copilot{policy: copilotPolicy{file: file, owner: owner}})
}
