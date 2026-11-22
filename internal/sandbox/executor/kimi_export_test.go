package executor

import (
	"context"
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
	box.Put(record, string(kimiFixture(t, "2.7.2", "steered.wire.jsonl")))
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
