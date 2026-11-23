package executor

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// codexConfirmThread is the session every confirm case below runs.
const codexConfirmThread = "019a1f00-0000-7000-8000-0000000000c1"

// codexItemLine is a rollout line holding a response_item of payload.
func codexItemLine(t *testing.T, payload map[string]any, metadata map[string]any) string {
	t.Helper()
	line := map[string]any{"timestamp": "2026-11-23T09:00:00.000Z", "type": "response_item", "payload": payload}
	if metadata != nil {
		line["metadata"] = metadata
	}
	raw, err := json.Marshal(line)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw) + "\n"
}

// codexUserLine is a person's message: a brief or a steered note.
func codexUserLine(t *testing.T, text string) string {
	return codexItemLine(t, map[string]any{"type": "message", "role": "user",
		"content": []map[string]string{{"type": "input_text", "text": text}}}, nil)
}

// codexReplyLine is the model's words answering a request.
func codexReplyLine(t *testing.T) string {
	return codexItemLine(t, map[string]any{"type": "message", "role": "assistant",
		"content": []map[string]string{{"type": "output_text", "text": "Done."}}}, nil)
}

// codexConfirm is the takes the runner's main-thread read holds, of a box whose
// rollout of [codexConfirmThread] is rollout and whose record of where the
// run began is start ("" for none).
func codexConfirm(t *testing.T, rollout, start string, takes ...sandbox.AskedTake) sandbox.TakeReceipts {
	t.Helper()
	return codexConfirmAsk(t, rollout, start, sandbox.MainThreadAsk{Session: codexConfirmThread, Takes: takes})
}

// codexConfirmAsk is [codexConfirm] of a whole ask.
func codexConfirmAsk(t *testing.T, rollout, start string, ask sandbox.MainThreadAsk) sandbox.TakeReceipts {
	t.Helper()
	box := sandbox.NewFakeSandbox("codex-confirm")
	p := PathsFor(box)
	path := p.CodexSessions() + "/2026/11/23/rollout-2026-11-23T09-00-00-" + codexConfirmThread + ".jsonl"
	box.Put(path, rollout)
	if start != "" {
		box.Put(p.CodexRunStart(), start)
	}
	box.ExecFunc = func(context.Context, string, sandbox.ExecOptions) (sandbox.ExecResult, error) {
		return sandbox.ExecResult{Stdout: path + "\n"}, nil
	}
	return New(Codex{}).ReadMainThread(t.Context(), box, ask).Receipts
}

// codexTake is a take of text served under id.
func codexTake(id, text string) sandbox.AskedTake {
	return sandbox.AskedTake{Take: id, Digest: ItemDigest(text)}
}

// AN EARLIER LAUNCH'S NOTE CONFIRMS NOTHING IN THIS RUN: a continued session's
// rollout holds every launch's messages, so a note of the same text this run
// served and never steered is not held off the earlier launch's record and
// reply; only a record past where this run began counts.
//
// Mutation: read the rollout from its first line, or ignore the run's start,
// and the note reads as held.
func TestAnEarlierLaunchsNoteConfirmsNothingInThisRun(t *testing.T) {
	t.Parallel()
	const note = "## A note from ada, sent while this turn was running\n\nuse main"
	earlier := codexUserLine(t, "Fix the flake.") + codexUserLine(t, note) + codexReplyLine(t)
	rollout := earlier + codexUserLine(t, "Ship it.") + codexReplyLine(t)
	got := codexConfirm(t, rollout, codexConfirmThread+" 3\n", codexTake("n-2", note))
	if len(got.Held) != 0 {
		t.Errorf("a note only the earlier launch recorded reads %+v, want nothing held", got)
	}
	steered := rollout + codexUserLine(t, note) + codexReplyLine(t)
	if got := codexConfirm(t, steered, codexConfirmThread+" 3\n", codexTake("n-2", note)); !slices.Equal(got.Held, []string{"n-2"}) {
		t.Errorf("the note this run steered reads %+v, want it held", got)
	}
}

// TWO IDENTICAL NOTES NEED TWO RECORDS: takes of one text are matched in serve
// order, one record each, so where Codex recorded one, only the first take is
// held and the second is served again.
//
// Mutation: match takes against a set of digests, and both read as held.
func TestTwoIdenticalCodexNotesNeedTwoRecords(t *testing.T) {
	t.Parallel()
	const note = "## A note from ada, sent while this turn was running\n\nrun the tests"
	takes := []sandbox.AskedTake{codexTake("n-1", note), codexTake("n-2", note)}
	once := codexUserLine(t, "Fix the flake.") + codexUserLine(t, note) + codexReplyLine(t)
	if got := codexConfirm(t, once, codexConfirmThread+" 0\n", takes...); !slices.Equal(got.Held, []string{"n-1"}) {
		t.Errorf("one record of two identical takes reads %+v, want the first alone held", got)
	}
	twice := once + codexUserLine(t, note) + codexReplyLine(t)
	if got := codexConfirm(t, twice, codexConfirmThread+" 0\n", takes...); !slices.Equal(got.Held, []string{"n-1", "n-2"}) {
		t.Errorf("two records of two identical takes read %+v, want both held", got)
	}
}

// A CODEX NOTE NO REPLY FOLLOWED IS NOT HELD: Codex records a steered message
// before the request that carries it, and a resume handing a trailing message
// back was not measured on 0.171.0, so the record is settled only by the
// model's next output. A run killed, or a request refused, after the record
// leaves the note unheld; a compaction before the reply drops it, and the
// model's answer to the compaction is no reply.
//
// Mutation: hold a note off its record alone, keep it across a compaction, or
// take a compaction's answer for a reply, and a case reads as held.
func TestACodexNoteNoReplyFollowedIsNotHeld(t *testing.T) {
	t.Parallel()
	const note = "## A note from ada, sent while this turn was running\n\nstop"
	brief := codexUserLine(t, "Fix the flake.") + codexReplyLine(t)
	compacted := `{"timestamp":"2026-11-23T09:00:01.000Z","type":"compacted","payload":{"message":"a summary"}}` + "\n"
	compactionAnswer := codexItemLine(t, map[string]any{"type": "message", "role": "assistant",
		"content": []map[string]string{{"type": "output_text", "text": "a summary"}}},
		map[string]any{"compaction_output": true})
	call := codexItemLine(t, map[string]any{"type": "function_call", "name": "exec_command", "arguments": "{}",
		"call_id": "c-1"}, nil)
	for name, tc := range map[string]struct {
		rollout string
		held    bool
	}{
		"the record last":               {brief + codexUserLine(t, note), false},
		"a compaction before the reply": {brief + codexUserLine(t, note) + compacted + codexReplyLine(t), false},
		"the compaction's answer alone": {brief + codexUserLine(t, note) + compactionAnswer, false},
		"the model's words after":       {brief + codexUserLine(t, note) + codexReplyLine(t), true},
		"a call the model made after":   {brief + codexUserLine(t, note) + call, true},
		"a torn reply after the record": {brief + codexUserLine(t, note) + strings.TrimSuffix(codexReplyLine(t), "\n"), false},
	} {
		got := codexConfirm(t, tc.rollout, codexConfirmThread+" 0\n", codexTake("n-1", note))
		if held := slices.Equal(got.Held, []string{"n-1"}); held != tc.held {
			t.Errorf("%s: the note reads %+v, want held %v", name, got, tc.held)
		}
	}
}

// WHERE THE RUN BEGAN DECIDES WHAT COUNTS, so a box without that record, or
// with another session's, decides nothing: the take stays undecided, saying
// why, never held off an earlier launch nor called absent.
//
// Mutation: read a missing or foreign start as 0, and the note reads as held.
func TestACodexRunWithoutItsStartDecidesNothing(t *testing.T) {
	t.Parallel()
	const note = "## A note from ada, sent while this turn was running\n\nuse main"
	rollout := codexUserLine(t, note) + codexReplyLine(t)
	for name, start := range map[string]string{
		"none":            "",
		"another session": "019a1f00-0000-7000-8000-0000000000ff 0\n",
		"no count":        codexConfirmThread + "\n",
	} {
		got := codexConfirm(t, rollout, start, codexTake("n-1", note))
		if len(got.Held) != 0 || len(got.Absent) != 0 || got.Why == "" {
			t.Errorf("%s: the note reads %+v, want undecided with why", name, got)
		}
	}
}

// A TAKE CODEX NEVER RECORDED LEAVES THE LATER ONES TO MATCH: a steer Codex
// accepted and did not record (the note helper takes an accepted steer as
// delivered) is unheld, and each later message matches the first take after
// the last one matched with its words, as Gemini CLI's do.
//
// Mutation: match a message against the next take alone, and C reads unheld.
func TestATakeCodexNeverRecordedLeavesTheLaterOnesToMatch(t *testing.T) {
	t.Parallel()
	const (
		a    = "## A note from ada, sent while this turn was running\n\nuse main"
		lost = "## A note from ada, sent while this turn was running\n\nnever recorded"
		c    = "## A note from ada, sent while this turn was running\n\nrun the tests"
	)
	rollout := codexUserLine(t, "Fix the flake.") + codexReplyLine(t) + codexUserLine(t, a) + codexReplyLine(t) +
		codexUserLine(t, c) + codexReplyLine(t)
	got := codexConfirm(t, rollout, codexConfirmThread+" 0\n", codexTake("n-a", a), codexTake("n-lost", lost),
		codexTake("n-c", c))
	if !slices.Equal(got.Held, []string{"n-a", "n-c"}) {
		t.Errorf("the takes read %+v, want A and C held and the lost one not", got)
	}
}

// A READ CODEX NOTE KEEPS ITS PLACE: the run's read notes are matched in their
// places among its serves, so a read note's message settles no later take of
// the same words, which a run killed before its reply leaves unheld.
//
// Mutation: match the record against the asked takes alone, and B reads held
// off A's message and reply.
func TestAReadCodexNoteKeepsItsPlace(t *testing.T) {
	t.Parallel()
	const note = "## A note from ada, sent while this turn was running\n\nuse main"
	a, b := codexTake("n-a", note), codexTake("n-b", note)
	ask := sandbox.MainThreadAsk{Session: codexConfirmThread, Takes: []sandbox.AskedTake{b},
		Served: []sandbox.AskedTake{a, b}}
	killed := codexUserLine(t, "Fix the flake.") + codexReplyLine(t) + codexUserLine(t, note) + codexReplyLine(t) +
		codexUserLine(t, note)
	if got := codexConfirmAsk(t, killed, codexConfirmThread+" 0\n", ask); len(got.Held) != 0 {
		t.Errorf("B, killed before its reply, reads %+v off the read A's record, want nothing held", got)
	}
	if got := codexConfirmAsk(t, killed+codexReplyLine(t), codexConfirmThread+" 0\n", ask); !slices.Equal(got.Held,
		[]string{"n-b"}) {
		t.Errorf("B, answered, reads %+v, want it held", got)
	}
}
