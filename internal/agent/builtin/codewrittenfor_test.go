package builtin_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
)

// WHOM A CODE WRITE IS MADE FOR (ADR-0109): every request a code tool's call
// forms states it, composed from the call's own bound ceiling.

// codeWriteCalls is a call of every code tool that forms a request of a
// change's lifecycle, its conversation, an engine write or a dequeue — and the
// steps that form one of their own: the open's ask of its reviewers, a
// comment's mark of its thread, an apply's answer to its findings and a
// revert's merge — each against site!7 as [carryingHost] holds it.
func codeWriteCalls() []struct {
	tool string
	args map[string]any
} {
	const thread = "0192f6c1-6a7e-7c1b-9d4e-3f8a2b1c0d30"
	return []struct {
		tool string
		args map[string]any
	}{
		{builtin.CodeOpenChangeTool, map[string]any{"repo": "site", "branch": "feature", "title": "Retry the upload",
			"reviewers": []any{"agent-ceo"}}},
		{builtin.CodeUpdateChangeTool, map[string]any{"change": "site!7", "expected_version": 4,
			"title": "Retry twice"}},
		{builtin.CodeCloseChangeTool, map[string]any{"change": "site!7", "expected_version": 4,
			"reason": "superseded"}},
		{builtin.CodeCloseChangeTool, map[string]any{"change": "site!7", "expected_version": 4, "reopen": true}},
		{builtin.CodeRequestReviewTool, map[string]any{"change": "site!7", "reviewers": []any{"agent-ceo"}}},
		{builtin.CodeReviewChangeTool, map[string]any{"change": "site!7", "verdict": "approve",
			"summary": "Looks right."}},
		{builtin.CodeCommentOnChangeTool, map[string]any{"change": "site!7", "body": "Done, thanks.",
			"thread": thread, "resolve": true}},
		{builtin.CodeResolveFindingTool, map[string]any{"change": "site!7", "findings": []any{suggestedA},
			"resolution": "resolved"}},
		{builtin.CodeApplySuggestionsTool, applyArgs(suggestedA)},
		{builtin.CodeMergeChangeTool, map[string]any{"change": "site!7", "expected_head": revHead(3),
			"delete_branch": false}},
		{builtin.CodeDequeueChangeTool, map[string]any{"change": "site!7"}},
		{builtin.CodeRebaseChangeTool, map[string]any{"change": "site!7", "expected_head": revHead(3)}},
		{builtin.CodeRevertChangeTool, map[string]any{"change": "site!7", "merge": true}},
		{builtin.CodeCommitFilesTool, commitArgs("hello")},
		{builtin.CodeRevertTool, map[string]any{"repo": "site", "branch": "seats/agent-cto/fix",
			"expected_head": mainOID, "commit": "feature"}},
		{builtin.CodeCherryPickTool, map[string]any{"repo": "site", "branch": "seats/agent-cto/fix",
			"expected_head": mainOID, "commit": "feature"}},
		{builtin.CodeCreateBranchTool, map[string]any{"repo": "site", "branch": "seats/agent-cto/new",
			"at": mainOID}},
		{builtin.CodeDeleteBranchTool, map[string]any{"repo": "site", "branch": "seats/agent-cto/fix",
			"expected_head": mainOID}},
	}
}

// carryingHost is site and its change site!7 — queued, with findings that
// suggest — and a writer that lands every write, answering a revert of the
// change with the change its push opened, so the revert's merge is asked too.
func carryingHost() (*gitletHost, *codeWrites) {
	writer := &codeWrites{}
	writer.write = func(w gitlet.EngineWrite) (gitlet.EngineWriteResult, error) {
		res := gitlet.EngineWriteResult{Outcome: statelog.OutcomeApplied, Commit: writtenOID,
			PushID: gitlet.WritePushID(w.OpID), Position: codeAt(13),
			Command: gitlet.Command{Ref: w.Ref, Old: w.ExpectedHead, New: writtenOID, Result: gitlet.ResultOK}}
		switch w.Kind {
		case gitlet.WriteRevertChange:
			res.Change = &gitlet.ChangeAnswer{ChangeID: siteChangeID, Number: 8, Opened: true}
		case gitlet.WriteMergeChange:
			res.Merge = &gitlet.MergeAnswer{}
		}
		return res, nil
	}
	return newChangeHost(suggestedChange().queued()), writer
}

// requestsCarried is whom every request writer was asked is made for, by what
// it is: the engine writes, a change's lifecycle and conversation, and the
// writes of a queue.
func requestsCarried(writer *codeWrites) map[string][]types.WrittenFor {
	out := map[string][]types.WrittenFor{}
	writes, _ := writer.asked()
	for _, w := range writes {
		out["engine write"] = append(out["engine write"], w.WrittenFor)
	}
	changes, converses := writer.changesAsked()
	for _, p := range changes {
		out["change's lifecycle"] = append(out["change's lifecycle"], p.WrittenFor)
	}
	for _, w := range converses {
		out["change's conversation"] = append(out["change's conversation"], w.WrittenFor)
	}
	for _, p := range writer.queuesAsked() {
		out["queue write"] = append(out["queue write"], p.WrittenFor)
	}
	return out
}

// EVERY CODE WRITE STATES WHOM IT IS MADE FOR (ADR-0109): every request a code
// tool's call forms — a change's lifecycle (an open, an edit, a close, a
// reopen), its conversation (the open's ask of its reviewers, an ask for
// review, a review, a comment and its thread's mark, an answer to findings,
// an apply's answer), an engine write (an apply's suggestions, a merge, a
// rebase, a revert of a change and its merge, a commit, a revert, a
// cherry-pick, a branch made and one deleted) and a dequeue — and the merge an
// approved ask_to_merge makes in the asking run's resumed turn, states whom it
// is made for: in a turn a colleague seat woke, that seat; a person, their
// seat; a credential acting as no seat, that credential with the apps this
// node holds it to — the narrower of what its write carried and what this
// node resolves it to; and a turn nobody woke, nobody, stated so. A person's
// own call states nothing, which every node reads as nobody
// (types.WrittenFor.For).
//
// Mutations: proposeChange, conversing.decided, the open's ask, the dequeue
// or the engine writes' helper leaving the carrier unstated — that request
// states nothing, red.
func TestEveryCodeWriteStatesWhomItIsMadeFor(t *testing.T) {
	t.Parallel()
	seat := func(handle string) *types.WorkingFor {
		return &types.WorkingFor{Kind: types.WorkingForSeat, Seat: handle}
	}
	speaker := func(handle string) []types.CanonicalIdentity {
		return []types.CanonicalIdentity{{Handle: handle}}
	}
	// THE COUNTS every row sends, so a call that sent nothing fails too: the
	// nine engine writes the tools build, the revert's merge and the approved
	// ask_to_merge's; four of a change's lifecycle; seven of its
	// conversation; one dequeue.
	want := map[string]int{"engine write": 11, "change's lifecycle": 4, "change's conversation": 7, "queue write": 1}
	for _, c := range []struct {
		name string
		// wake sets what woke the turn.
		wake    func(turn *turnctx.Turn)
		carrier *types.WorkingFor
	}{
		{"a turn a colleague seat woke", func(turn *turnctx.Turn) {
			turn.Requester, turn.Senders = "agent-ceo", speaker("agent-ceo")
		}, seat("agent-ceo")},
		{"a turn a person woke", func(turn *turnctx.Turn) {
			turn.Requester, turn.Senders = "founder", speaker("founder")
		}, seat("founder")},
		{"a turn a credential acting as no seat woke", func(turn *turnctx.Turn) {
			turn.Senders = []types.CanonicalIdentity{{ExternalID: "token:ci", Platform: "worklet", Login: "token:ci"}}
			turn.RequesterLogin, turn.RequesterApps = "token:ci", []iam.Grant{iam.GrantGitlet, iam.GrantDoclet}
		}, &types.WorkingFor{Kind: types.WorkingForCredential, Login: "token:ci",
			Apps: []iam.Grant{iam.GrantGitlet, iam.GrantDoclet}}},
		{"a turn nobody woke", func(*turnctx.Turn) {}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			host, writer := carryingHost()
			deps := writeDeps(host, writer)
			reg := codeRegistry(t, deps)
			turn := writerTurn(t)
			c.wake(turn)
			for _, call := range codeWriteCalls() {
				if got := callWrite(everyGrant(), t, reg, turn, call.tool, call.args); got.Failed {
					t.Fatalf("%s answered %q", call.tool, got.Output)
				}
			}
			// THE APPROVED ASK_TO_MERGE, carried on in the asking run's
			// resumed turn: the code tools' own merge, as the seat.
			merge := releaseTool(t, builtin.Deps{Code: deps}, builtin.AskToMergeTool)
			approved := tools.WithApproval(asSeat("agent-cto"), tools.Approval{By: "ops-lead", HeldID: "held-9"})
			if got, err := merge.CallForTurn(approved, turn, map[string]any{"change": "site!7", "head": revHead(3),
				"reason": "ready to ship"}); err != nil || got.Failed {
				t.Fatalf("the approved ask_to_merge answered %v %q", err, got.Output)
			}
			for what, carried := range requestsCarried(writer) {
				if len(carried) != want[what] {
					t.Errorf("%d %s requests were sent, want %d", len(carried), what, want[what])
				}
				for i, f := range carried {
					if !f.Stated() || !reflect.DeepEqual(f.Carrier(), c.carrier) {
						t.Errorf("%s %d is made for %+v (stated: %v), want %+v, stated", what, i, f.Carrier(),
							f.Stated(), c.carrier)
					}
				}
			}
		})
	}

	// A PERSON'S OWN CALL, through one of their tokens: no deputy, so it
	// states nothing, and every node reads that as nobody.
	t.Run("a person's call", func(t *testing.T) {
		t.Parallel()
		host, writer := carryingHost()
		deps := writeDeps(host, writer)
		deps.Actor = builtin.PrincipalActor
		for _, call := range codeWriteCalls() {
			tool := operatorToolOver(t, builtin.OperatorDeps{Code: deps, Authorize: builtin.Decide(chartLeads)},
				call.tool)
			if got, err := tool.CallForTurn(asBoundPerson(), nil, call.args); err != nil || got.Failed {
				t.Fatalf("the person's %s answered %v %q", call.tool, err, got.Output)
			}
		}
		person := map[string]int{"engine write": 10, "change's lifecycle": 4, "change's conversation": 7,
			"queue write": 1}
		for what, carried := range requestsCarried(writer) {
			if len(carried) != person[what] {
				t.Errorf("%d %s requests were sent for the person, want %d", len(carried), what, person[what])
			}
			for i, f := range carried {
				if carrier, err := f.For(iam.ActorHuman); f.Stated() || err != nil || carrier != nil {
					t.Errorf("the person's %s %d is made for %+v (stated: %v, %v), want it stating nothing",
						what, i, carrier, f.Stated(), err)
				}
			}
		}
	})
}

// A CODE WRITE WHOSE ARGUMENTS NAME SOMEBODY GETS NOTHING WIDER (ADR-0109): whom
// a code write is made for is composed from the call's own ceiling and never
// from what a model writes, so a call whose arguments carry a `written_for`
// and a `working_for` naming another seat is refused at the gate as an
// argument no tool declares, sending nothing — and past the gate, its ceiling
// composed from the turn as the gate would bind it, the request each tool forms
// is made for whoever the turn works for, the colleague seat whose ask woke
// it, never the seat the arguments named.
//
// Mutation: an engine write, a change's lifecycle, its conversation or a
// dequeue made for the seat its arguments name → red.
func TestACodeWriteWhoseArgumentsNameSomebodyGetsNothingWider(t *testing.T) {
	t.Parallel()
	forged := map[string]any{"kind": "seat", "seat": "agent-cfo"}
	ceo := &types.WorkingFor{Kind: types.WorkingForSeat, Seat: "agent-ceo"}
	for _, call := range codeWriteCalls() {
		args := map[string]any{"written_for": forged, "working_for": forged}
		for k, v := range call.args {
			args[k] = v
		}
		host, writer := carryingHost()
		reg := codeRegistry(t, writeDeps(host, writer))
		turn := writerTurn(t)
		turn.Requester, turn.Senders = "agent-ceo", []types.CanonicalIdentity{{Handle: "agent-ceo"}}
		entry, ok := reg.Lookup(call.tool)
		if !ok {
			t.Fatalf("%s is not registered", call.tool)
		}
		gated, ok := entry.Tool.(tools.SeatCallable)
		if !ok {
			t.Fatalf("%s is not seat-callable", call.tool)
		}
		refused, err := gated.CallForTurn(everyGrant(), turn, args)
		sent := 0
		for _, carried := range requestsCarried(writer) {
			sent += len(carried)
		}
		if err != nil || !refused.Failed || !errors.Is(refused.Cause, builtin.ErrUndeclaredArgument) || sent != 0 {
			t.Errorf("%s naming another seat in its arguments answered %q (%v, cause %v) and sent %d requests, "+
				"want the arguments refused and nothing sent", call.tool, refused.Output, err, refused.Cause, sent)
		}

		inner, ok := entry.Tool.(interface{ Unwrap() tools.Callable })
		if !ok {
			t.Fatalf("%s is not gated", call.tool)
		}
		seat, ok := inner.Unwrap().(tools.SeatCallable)
		if !ok {
			t.Fatalf("%s past its gate is not seat-callable", call.tool)
		}
		if got, err := seat.CallForTurn(asSeat("agent-cto"), turn, args); err != nil || got.Failed {
			t.Errorf("%s past the gate answered %v %q", call.tool, err, got.Output)
			continue
		}
		for what, carried := range requestsCarried(writer) {
			for i, f := range carried {
				if !f.Stated() || !reflect.DeepEqual(f.Carrier(), ceo) {
					t.Errorf("%s's %s %d, its arguments naming agent-cfo, is made for %+v, want the colleague "+
						"whose ask woke the turn, %+v", call.tool, what, i, f.Carrier(), ceo)
				}
			}
		}
	}
}
