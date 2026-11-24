package executor

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// GrokRouting is every variable measured to move or sign a Grok run's requests.
var GrokRouting = routeNames(grokRouting)

// GrokArchiveFormat is the layout a Grok Build run packs its session in.
const GrokArchiveFormat = grokArchiveFormat

// grokFixture is testdata/grok/1.0.92/<name>: a real Grok Build 1.0.92 run's
// stream, record or archive, captured by the runner's own script, settings
// and agent file against a stand-in model and bridge; rules.chat.jsonl alone
// is a bare `grok --rules` run's record, a session no runner's launch began,
// cancelled.stream.jsonl a run on the grok profile's argv whose MCP call
// asked for an approval, and subagent-* bare runs' whose main agent spawned a
// subagent, their records without Grok's list of workflows.
func grokFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "grok", "1.0.92", name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// GrokConfirmsATake is Grok Build's case of the note contract's confirm path
// (TestANoteServedIsConfirmedOffItsJobsOwnRecord): the runner's main-thread
// read answers a take asked off the session's own record, its traits stating
// the receipts it settles.
//
// Mutation: drop readMainThread or the receipts trait, and nothing is held.
func GrokConfirmsATake(t *testing.T) {
	if !confirms(Grok{}) || !NewGrok().ThreadTraits().Receipts {
		t.Fatal("Grok Build confirms no take")
	}
	const session = "11111111-2222-4333-8444-0000000000a2"
	box := sandbox.NewFakeSandbox("b")
	box.Put(PathsFor(box).GrokSessionsKey()+"/"+session+"/chat_history.jsonl", string(grokFixture(t, "notes.chat.jsonl")))
	reading := NewGrok().ReadMainThread(t.Context(), box, sandbox.MainThreadAsk{Session: session,
		Takes: []sandbox.AskedTake{{Take: "0f0e0d0c-0000-4000-8000-000000000001"}}})
	if len(reading.Receipts.Held) != 1 {
		t.Fatalf("receipts = %+v, want the take held", reading.Receipts)
	}
}

// GrokWakeRead is a Grok Build session handed wake by a hook of its settings,
// as its record keeps a hook's answer, read back as its transcript.
func GrokWakeRead(t *testing.T, wake string) []TranscriptEntry {
	t.Helper()
	line, err := json.Marshal(map[string]any{"type": "user", "synthetic_reason": "system_reminder",
		"content": []map[string]string{{"type": "text", "text": grokHookHeads[0] + wake + "\n\n" +
			claudeTakeToken("0f0e0d0c-0000-4000-8000-0000000000aa") + grokReminderTail}}})
	if err != nil {
		t.Fatal(err)
	}
	record := append(append([]byte(nil), grokFixture(t, "plain.chat.jsonl")...), append(line, '\n')...)
	return grokTranscriptOf(t, museArchive(t, map[string][]byte{"k/s/chat_history.jsonl": record}))
}

// grokTranscriptOf is archive read as a Grok Build session's transcript.
func grokTranscriptOf(t *testing.T, archive []byte) []TranscriptEntry {
	t.Helper()
	got, err := newTranscriptBuilder(0, TranscriptReadBounds(), TranscriptFilter{}).decode(t.Context(), Grok{},
		bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	return got.Entries
}
