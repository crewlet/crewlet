package engine

import (
	"testing"

	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// A LAUNCH HANDS A SESSION ON ONLY UNDER THE ENTRY IT RAN UNDER, where the
// member it picked runs a harness not measured to resume on its launch's
// route: under that member's own entry it hands the continuation on, under
// another or one the turn's row never recorded it starts a session of its own.
// A launch-routed member's harness continues it under any entry.
//
// Mutation: drop the entry from the launch's check, compare it against the
// session's own, or skip the harness's trait, and a row goes red.
func TestALaunchHandsASessionOnOnlyUnderTheEntryItRanUnder(t *testing.T) {
	t.Parallel()
	seat := &org.Role{Name: "Engineer", DeclaredHandle: "swe"}
	l := &agentLauncher{engine: &Engine{}, seat: seat, turn: &turnctx.Turn{RunID: "run-1", Seat: seat}}
	for name, tc := range map[string]struct {
		member    chainMember
		own       bool
		recorded  string
		continues bool
	}{
		"an unmeasured harness, under its own entry": {member: chainMember{key: "copilot",
			agent: executor.CopilotName, model: "claude-sonnet-4-5", fleet: true}, own: true, continues: true},
		"an unmeasured harness, under another entry": {member: chainMember{key: "copilot",
			agent: executor.CopilotName, model: "claude-sonnet-4-5", fleet: true}, recorded: "another-entry"},
		"an unmeasured harness, no entry recorded": {member: chainMember{key: "copilot",
			agent: executor.CopilotName, model: "claude-sonnet-4-5", fleet: true}},
		"a launch-routed harness, under another entry": {member: chainMember{key: "head",
			agent: executor.ClaudeCodeName, model: "claude-opus-5-5", fleet: true}, recorded: "another-entry", continues: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			member := harnessProvider(t, tc.member)
			entry := tc.recorded
			if tc.own {
				entry = member.EntryID()
			}
			req := runner.AgentRunRequest{Session: &runner.SessionContinuation{Brief: "carry on",
				Basis: runner.SessionBasis{LaunchSeq: 1, CaptureSeq: 1, SessionID: "s-1",
					Harness: member.Harness(), Entry: entry, ContextTokens: 1000}}}
			got, _ := l.continuedFloor(t.Context(), req, member, true)
			if (got.Session != nil) != tc.continues {
				t.Errorf("the launch hands the session on: %v, want %v", got.Session != nil, tc.continues)
			}
		})
	}
}
