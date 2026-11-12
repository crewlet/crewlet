package builtin_test

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools"
)

// joinFixture is one company's three request tools behind one holder whose
// desk keeps every held call as a running row of the seat that asked.
type joinFixture struct {
	t     *testing.T
	desk  *askDesk
	pulls *fakePulls
	reg   *tools.Registry
	hold  *builtin.CallHold
	turn  func(handle, run string) *turnctx.Turn
}

func newJoinFixture(t *testing.T) *joinFixture {
	t.Helper()
	cfg, o, chart := sshFixture(t)
	f := &joinFixture{t: t, desk: &askDesk{outcome: sandbox.AskRecorded, seat: "sre"},
		pulls: &fakePulls{facts: map[int]builtin.PullFacts{42: readyPull()}}, reg: tools.NewRegistry()}
	if _, err := builtin.Register(f.reg, builtin.Deps{Pulls: f.pulls, SSH: builtin.SSHDeps{
		Servers: &fakeServers{cfg: cfg}, Requesters: healthRequesters(), Chart: chart}}); err != nil {
		t.Fatal(err)
	}
	f.hold = builtin.NewCallHold(f.desk, callSeal(t), nil, nil)
	f.turn = func(handle, run string) *turnctx.Turn {
		turn := sshTurn(o, handle, "", nil)
		turn.RunID = run
		return turn
	}
	return f
}

// ask makes one call of tool as handle's run, over the surface a run calls.
func (f *joinFixture) ask(handle, run, tool string, args map[string]any) toolloop.ToolResult {
	f.t.Helper()
	exec := tools.NewSurface("execute", f.reg.Snapshot(), []string{tool}).WithHolder(f.hold).
		ForTurn(f.turn(handle, run))
	res, err := exec.Execute(turnctx.WithJob(asSeat(handle), "launch-"+run), llm.ToolCall{Name: tool, Arguments: args})
	if err != nil {
		f.t.Fatal(err)
	}
	return res
}

// held is how many calls the desk recorded.
func (f *joinFixture) held() int {
	f.desk.mu.Lock()
	defer f.desk.mu.Unlock()
	return len(f.desk.recorded)
}

// operateArgs is one ssh_operate call on the server sre operates.
func operateArgs(command, reason string) map[string]any {
	return map[string]any{"host": "api-1", "command": command, "reason": reason}
}

// A seat's repeat of a request still open joins it, for each delegable kind:
// the same merge, deploy or command asked again from another turn, in other
// words, holds nothing new and is answered with the request that waits, while
// it waits on a delegate and once it was put to people.
//
// Mutation: hold every call its tool lets through, and one merge is two
// requests, two delegate turns and two Inbox entries.
func TestARepeatAskJoinsTheRequestStillOpen(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		tool          string
		first, repeat map[string]any
	}{
		{builtin.AskToMergeTool, pullMergeArgs(pullHead), map[string]any{
			"change": "https://github.com/acme/web/pull/42", "head": strings.ToUpper(pullHead),
			"method": "merge", "reason": "QA approved it, please merge"}},
		{builtin.AskToDeployTool, deployArgs(), map[string]any{
			"repo": "https://github.com/acme/web", "commit": deployCommit, "changes": []any{"#42", "41", "42"},
			"environment": "production", "rollback": "Roll back to the last tag.", "reason": "asked again"}},
		{builtin.SSHOperateTool, operateArgs("sudo systemctl restart api", "the deploy needs it"),
			operateArgs("sudo systemctl restart api", "restart after the release")},
	} {
		f := newJoinFixture(t)
		first := f.ask("sre", "run-1", c.tool, c.first)
		if !first.Held || f.held() != 1 {
			t.Fatalf("%s: the first ask answered %+v and held %d calls, want it held", c.tool, first, f.held())
		}
		open := f.desk.rows[0].HeldCall
		if len(open.Subject) != 64 {
			t.Fatalf("%s: the held call names the subject %q, want a digest", c.tool, open.Subject)
		}
		for _, state := range []string{"waiting on its delegate", "put to people"} {
			again := f.ask("sre", "run-2", c.tool, c.repeat)
			if again.Held || again.Refusal() != tools.RefusalConflict || f.held() != 1 ||
				!strings.Contains(again.Output, open.ID) || !strings.Contains(again.Output, "did not ask again") {
				t.Fatalf("%s, %s: a repeat answered %+v and held %d calls, want it joined to %s", c.tool, state,
					again, f.held(), open.ID)
			}
			f.desk.rows[0].Status = sandbox.StatusAwaiting
			f.desk.rows[0].HeldCall.Delegation = &sandbox.HeldDelegation{Rule: 1, Delegates: []string{"dev"},
				Escalated: &sandbox.HeldEscalation{At: time.Now()}}
		}
	}
}

// An ask that differs in what it decides is its own request: a new head,
// another method, another environment, another command, and the same ask by
// another seat are each held beside the one that waits.
//
// Mutation: identify a request by its tool and change alone, and an approval
// read for one head is waited on by a turn that asked about another.
func TestAnAskThatDiffersInSubstanceIsItsOwnRequest(t *testing.T) {
	t.Parallel()
	f := newJoinFixture(t)
	with := func(base map[string]any, key string, value any) map[string]any {
		base[key] = value
		return base
	}
	asks := []struct {
		name, seat, tool string
		args             map[string]any
		before           func()
	}{
		{"a merge", "sre", builtin.AskToMergeTool, pullMergeArgs(pullHead), nil},
		{"the same merge squashed", "sre", builtin.AskToMergeTool, with(pullMergeArgs(pullHead), "method", "squash"), nil},
		{"the same merge by another seat", "dev", builtin.AskToMergeTool, pullMergeArgs(pullHead), nil},
		{"the change at a new head", "sre", builtin.AskToMergeTool, pullMergeArgs(movedHead), func() {
			moved := readyPull()
			moved.Head = movedHead
			f.pulls.facts[42] = moved
		}},
		{"a deploy", "sre", builtin.AskToDeployTool, deployArgs(), nil},
		{"the same deploy elsewhere", "sre", builtin.AskToDeployTool, with(deployArgs(), "environment", "staging"), nil},
		{"the deploy with one change fewer", "sre", builtin.AskToDeployTool, with(deployArgs(), "changes", []any{"41"}), nil},
		{"a command", "sre", builtin.SSHOperateTool, operateArgs("sudo systemctl restart api", "needed"), nil},
		{"another command", "sre", builtin.SSHOperateTool, operateArgs("sudo systemctl restart worker", "needed"), nil},
		{"the command with stdin", "sre", builtin.SSHOperateTool,
			with(operateArgs("sudo systemctl restart api", "needed"), "stdin", "yes\n"), nil},
	}
	for i, a := range asks {
		if a.before != nil {
			a.before()
		}
		f.desk.seat = a.seat
		if res := f.ask(a.seat, "run-"+a.name, a.tool, a.args); !res.Held || f.held() != i+1 {
			t.Fatalf("%s answered %+v with %d calls held, want it held as request %d", a.name, res, f.held(), i+1)
		}
	}
}

// A request already answered, being carried out, or whose run ended or moved
// on to another job is no longer open: the same ask after it is a new request.
//
// Mutation: join whatever the seat's rows still name, and an ask after a
// denial or a merge that failed waits on an answer that already came.
func TestAnAskAfterItsRequestWasDecidedIsANewRequest(t *testing.T) {
	t.Parallel()
	for name, decide := range map[string]func(*sandbox.PendingRun){
		"answered":            func(r *sandbox.PendingRun) { r.Status = sandbox.StatusAnswered },
		"being carried out":   func(r *sandbox.PendingRun) { r.HeldCall.CallingAt = time.Now() },
		"its run is ending":   func(r *sandbox.PendingRun) { r.Ending = &sandbox.RecordedEnding{} },
		"its run took a job":  func(r *sandbox.PendingRun) { r.LaunchID = "launch-next" },
		"its question is off": func(r *sandbox.PendingRun) { r.Question = "" },
	} {
		f := newJoinFixture(t)
		if res := f.ask("sre", "run-1", builtin.AskToMergeTool, pullMergeArgs(pullHead)); !res.Held {
			t.Fatalf("%s: the first ask answered %+v, want it held", name, res)
		}
		decide(&f.desk.rows[0])
		if res := f.ask("sre", "run-2", builtin.AskToMergeTool, pullMergeArgs(pullHead)); !res.Held || f.held() != 2 {
			t.Errorf("%s: the ask after it answered %+v with %d calls held, want a new request", name, res, f.held())
		}
	}
}

// A seat's open requests that cannot be read hold nothing: the call is
// refused, to be made again, never held as a possible second request.
func TestAnAskWhoseOpenRequestsCannotBeReadHoldsNothing(t *testing.T) {
	t.Parallel()
	f := newJoinFixture(t)
	f.desk.listErr = errors.New("the store did not answer")
	if res := f.ask("sre", "run-1", builtin.AskToMergeTool, pullMergeArgs(pullHead)); res.Held || !res.Failed ||
		f.held() != 0 {
		t.Fatalf("an ask over an unread store answered %+v and held %d calls, want it refused", res, f.held())
	}
}

// Two turns of one seat asking the same thing at the same moment come to one
// request: the seat's open requests are read, and the new one recorded, under
// one lock, so the second turn reads only once the first has recorded its own.
//
// Mutation: read or record outside the lock, and both turns read no open
// request and each holds its call.
//
// Not parallel: the lock is the process's, and another case's hold would pass
// for this one's.
func TestTwoTurnsAskingAtOnceComeToOneRequest(t *testing.T) {
	f := newJoinFixture(t)
	var read, recorded []bool
	f.desk.onList = func() { read = append(read, builtin.HoldingLocked()) }
	f.desk.onRecord = func() { recorded = append(recorded, builtin.HoldingLocked()) }
	first := f.ask("sre", "run-1", builtin.AskToMergeTool, pullMergeArgs(pullHead))
	if !first.Held || !slices.Equal(read, []bool{true}) || !slices.Equal(recorded, []bool{true}) {
		t.Fatalf("the first ask answered %+v, reading under the lock %v and recording under it %v, want both "+
			"under it", first, read, recorded)
	}
	other := f.ask("sre", "run-2", builtin.AskToMergeTool, pullMergeArgs(pullHead))
	if other.Held || other.Refusal() != tools.RefusalConflict || f.held() != 1 || !slices.Equal(read, []bool{true, true}) {
		t.Fatalf("the second ask answered %+v with %d calls held, reading under the lock %v, want it joined",
			other, f.held(), read)
	}
	if builtin.HoldingLocked() {
		t.Fatal("the lock is still taken once both asks answered")
	}
}
