package executor

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// MuseArchiveFormat is the layout a Muse Code run packs its session in.
const MuseArchiveFormat = museArchiveFormat

// MuseHookAnswerBytes is the most bytes Muse Code reads of a hook's answer.
const MuseHookAnswerBytes = museHookAnswerBytes

// MuseHookEvents is the two boundaries a Muse Code hook answers at.
var MuseHookEvents = []string{claudeEventToolBatch, claudeEventFinish}

// MuseHanded is message as Muse Code is handed it in a hook's answer at
// event: the answer the runner writes, the token's field cut out as the note
// helper cuts it (internal/notehelper's splitTake), every other byte as it
// stands, so the same length whatever order its fields come in.
func MuseHanded(t *testing.T, event, message string) string {
	t.Helper()
	const take = "11111111-1111-4111-8111-111111111111"
	answer := (Muse{}).hookAnswer(event, take, []HookNote{{Message: message}})
	if !json.Valid([]byte(answer)) {
		t.Fatalf("%s: the answer is no JSON", event)
	}
	field := `"` + museTakeField + `":` + museJSONString(claudeTakeToken(take)) + `,`
	handed, cut := strings.CutPrefix(answer, "{"+field)
	if !cut {
		t.Fatalf("%s: the answer does not open with the token's field: %.80s", event, answer)
	}
	return "{" + handed
}

// MuseConfirmsATake is Muse Code's case of the note contract's confirm path
// (TestANoteServedIsConfirmedOffItsJobsOwnRecord): the runner's main-thread
// read answers a take asked off the session's own record, its hooks' runs,
// its traits stating the receipts it settles.
//
// Mutation: drop readMainThread or the receipts trait, and nothing is held.
func MuseConfirmsATake(t *testing.T) {
	if !confirms(Muse{}) || !NewMuse().ThreadTraits().Receipts {
		t.Fatal("Muse Code confirms no take")
	}
	const session = "0b5e0000-0000-4000-8000-0000000000f7"
	box := sandbox.NewFakeSandbox("b")
	path := PathsFor(box).MuseSessions() + "/2026/11/22/" + session + "/session.jsonl"
	box.Put(path, string(museFixture(t, "hooks.session.jsonl")))
	box.Put(PathsFor(box).MuseNotesInstall(), museInstallAnswer(museHooksDigest))
	box.ExecFunc = func(context.Context, string, sandbox.ExecOptions) (sandbox.ExecResult, error) {
		return sandbox.ExecResult{Stdout: path + "\n"}, nil
	}
	reading := NewMuse().ReadMainThread(t.Context(), box, sandbox.MainThreadAsk{Session: session,
		Takes: []sandbox.AskedTake{{Take: "00000000-0000-4000-8000-000000000003"}}})
	if len(reading.Receipts.Held) != 1 {
		t.Fatalf("receipts = %+v, want the take held", reading.Receipts)
	}
}

// MuseWakeRead is a Muse Code session handed wake by a hook of its plugin, as
// its record keeps a hook's context, read back as its transcript.
func MuseWakeRead(t *testing.T, wake string) []TranscriptEntry {
	t.Helper()
	block, err := json.Marshal(map[string]any{"payload_type": "runtime.session", "recorded_at": 1795336482096791,
		"payload": map[string]any{"event": map[string]any{"kind": "context_block_updated",
			"source": museHookSource, "text": wake}}})
	if err != nil {
		t.Fatal(err)
	}
	record := append(append([]byte(nil), museFixture(t, "subagent.session.jsonl")...), append(block, '\n')...)
	return museTranscriptOf(t, museArchive(t, map[string][]byte{"2026/11/22/s/session.jsonl": record}))
}
