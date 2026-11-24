package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// Each runner's case of a TRAILING NOTE, one its harness recorded or sent and
// no reply of the model followed (a run killed, or its request refused, after
// the hand-over): whether the runner's own confirm holds it, for
// TestATrailingNoteNoReplyFollowedIsHeldOnlyWhereAResumeHandsItBack, which
// holds each answer to what its fixture states of its harness's resume.

// ClaudeTrailingNoteHeld is whether a batch hook's pair, its token and its
// context, holds its take with no reply after it.
func ClaudeTrailingNoteHeld(t *testing.T) bool {
	got := rhRead(t, "2.1.322", true, rhChained(rhCall("x1"), rhPair(rhBatch, rhBatchFire, rhBatchTake)), rhAskBatch)
	return slices.Contains(got.Held, rhBatchTake)
}

// CodexTrailingNoteHeld is whether a note the run's rollout records last, no
// model output after it, is held.
func CodexTrailingNoteHeld(t *testing.T) bool {
	const note = "## A note from ada, sent while this turn was running\n\nstop"
	rollout := codexUserLine(t, "Fix the flake.") + codexReplyLine(t) + codexUserLine(t, note)
	return slices.Contains(codexConfirm(t, rollout, codexConfirmThread+" 0\n", codexTake("n-1", note)).Held, "n-1")
}

// GeminiTrailingNoteHeld is whether a note the main session records last as a
// user message, under a take its script recorded, is held.
func GeminiTrailingNoteHeld(t *testing.T) bool {
	const session = "11111111-2222-4333-8444-555555555555"
	const text = "## A note from Lead, sent while this turn was running: stop"
	box := sandbox.NewFakeSandbox("gemini-trailing")
	p := PathsFor(box)
	const name = "session-2026-11-23T09-00-11111111.jsonl"
	box.Put(p.GeminiChats()+"/"+name, `{"sessionId":"`+session+`"}`+"\n"+
		`{"type":"user","content":[{"text":"Fix the flake."}]}`+"\n"+`{"type":"user","content":[{"text":"`+text+`"}]}`+"\n")
	box.Put(p.GeminiTakes(session), `{"from":0}`+"\n"+`{"take":"take-a","digest":"`+ItemDigest(text)+`"}`+"\n")
	box.ExecFunc = func(_ context.Context, cmd string, _ sandbox.ExecOptions) (sandbox.ExecResult, error) {
		return sandbox.ExecResult{Stdout: name + "\n"}, nil
	}
	got := GeminiCLI{}.readMainThread(t.Context(), box, p, sandbox.MainThreadAsk{Session: session,
		Takes: []sandbox.AskedTake{{Take: "take-a"}}}).Receipts
	return slices.Contains(got.Held, "take-a")
}

// QwenTrailingNoteHeld is whether a mid-turn message the chat file records
// last, no assistant record after it, is held.
func QwenTrailingNoteHeld(t *testing.T) bool {
	const note = "## A note from ada, sent while this turn was running\n\nstop"
	mid, err := json.Marshal(map[string]any{"type": "user", "subtype": "mid_turn_user_message",
		"message": map[string]any{"parts": []map[string]string{{"text": "\n" + qwenNoteFraming + note}}}})
	if err != nil {
		t.Fatal(err)
	}
	record := `{"type":"assistant","message":{"parts":[{"text":"On it."}]}}` + "\n" + string(mid) + "\n"
	held, err := qwenNotesHeld(t.Context(), bytes.NewReader([]byte(record)), qwenTakes(note), 0, qwenTestBound)
	if err != nil {
		t.Fatal(err)
	}
	return slices.Contains(held, "i-1")
}

// CopilotTrailingNoteHeld is whether a note 1.0.114 recorded before the
// process was killed, before its request answered (killed-in-gap), is held.
func CopilotTrailingNoteHeld(t *testing.T) bool {
	held, err := copilotNotesHeld(t.Context(), bytes.NewReader(copilotFixture(t, "killed-in-gap.events.jsonl")),
		served("msg_inbox1"))
	if err != nil {
		t.Fatal(err)
	}
	return slices.Contains(held, "msg_inbox1")
}

// KimiTrailingNoteHeld is whether a note 2.7.2 recorded before the run was
// killed, before its request (killed-in-gap), is held.
func KimiTrailingNoteHeld(t *testing.T) bool {
	held, err := kimiNotesHeld(t.Context(), bytes.NewReader(kimiFixture(t, "2.7.2", "killed-in-gap.wire.jsonl")),
		kimiServed(kimiKilledInbox))
	if err != nil {
		t.Fatal(err)
	}
	return slices.Contains(held, kimiKilledInbox)
}

// MuseTrailingNoteHeld is whether a take whose hook's block 1.4.25 committed,
// no main-session request completing after it, is held: the hooks fixture cut
// after its last committed block.
func MuseTrailingNoteHeld(t *testing.T) bool {
	var record []byte
	for _, l := range museLines(t, "hooks.session.jsonl") {
		record = append(record, l...)
		if bytes.Contains(l, []byte(`"hook:post_tool_use:call_0032_1:1"`)) {
			return museHeldOf(t, record, museHooksDigest)[museToken("1")]
		}
	}
	t.Fatal("the hooks fixture holds no committed block to cut after")
	return false
}

// GrokTrailingNoteHeld is whether a take whose Stop block 1.0.92 recorded, no
// model's answer after it, is held: the notes fixture cut after that block.
func GrokTrailingNoteHeld(t *testing.T) bool {
	record := grokFixture(t, "notes.chat.jsonl")
	lines := bytes.SplitAfter(record, []byte("\n"))
	stop := slices.IndexFunc(lines, func(l []byte) bool { return bytes.Contains(l, []byte("stop_hook_feedback")) })
	if stop < 0 {
		t.Fatal("the notes fixture holds no stop block to cut after")
	}
	held, err := grokHeld(t.Context(), bytes.NewReader(bytes.Join(lines[:stop+1], nil)))
	if err != nil {
		t.Fatal(err)
	}
	return held[claudeTakeToken("0f0e0d0c-0000-4000-8000-000000000002")]
}

// PiTrailingNoteHeld is whether a note 1.6.0's session holds last on its
// branch, no answer after it, is held.
func PiTrailingNoteHeld(t *testing.T) bool {
	steered := piSessionLines(t, "steered.session.jsonl")
	at := piNoteAt(t, steered, "inbox-steer-1")
	held, err := piNotesHeld(t.Context(), bytes.NewReader(piJoin(t, steered[:at+1])),
		[]ServedTake{{InboxID: "inbox-steer-1"}})
	if err != nil {
		t.Fatal(err)
	}
	return slices.Contains(held, "inbox-steer-1")
}

// HermesTrailingNoteHeld is whether a note this launch's record holds as its
// last row, no reply of the model stored after it, is held.
func HermesTrailingNoteHeld(t *testing.T) bool {
	dump := hermesNoteRows(t, func(id int64, r map[string]any) {
		if id == 4 {
			r["id"] = 6
		}
	})
	held, err := hermesNotesHeld(t.Context(), bytes.NewReader(dump), []ServedTake{{InboxID: "n-1", Digest: ItemDigest(hermesSteeredNote)}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	return slices.Contains(held, "n-1")
}
