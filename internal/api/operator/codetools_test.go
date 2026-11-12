package operator_test

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/iam"
	crewletmcp "github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/statelog"
)

// listingHost is a fake of Gitlet whose one listing is empty, recording who
// asked for it: what an operator's code tool reads through.
type listingHost struct {
	mu    sync.Mutex
	asked []gitlet.Principal
}

func (h *listingHost) Read(_ context.Context, q gitlet.ReadQuery) (gitlet.ReadAnswer, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.asked = append(h.asked, q.Asker)
	return gitlet.ReadAnswer{Level: q.Fresh.Level}, nil
}

func (h *listingHost) ResolveRef(context.Context, gitlet.RefQuery) (gitlet.RefAnswer, error) {
	return gitlet.RefAnswer{}, gitlet.ErrRefNotFound
}

func (h *listingHost) GitRead(context.Context, gitlet.GitQuery) (gitlet.GitAnswer, error) {
	return gitlet.GitAnswer{}, gitlet.ErrObjectNotFound
}

func (h *listingHost) FloorOf(string) statelog.Position { return statelog.Position{} }

// noRequesters composes nobody: the operator surface's calls are made in no
// turn, so no code read there is ever made for anybody but its caller — and
// the code tools are wired with the requester ceiling's halves or not at all.
type noRequesters struct{}

func (noRequesters) Requester(_ context.Context, handle string) (iam.Principal, error) {
	return iam.Principal{}, fmt.Errorf("the operator surface composes no requester, and was asked for %q", handle)
}

// Credential composes nobody either.
func (noRequesters) Credential(_ context.Context, login string) (iam.Principal, bool, error) {
	return iam.Principal{}, false, fmt.Errorf("the operator surface composes no requester, and was asked for %q", login)
}

func (h *listingHost) askers() []gitlet.Principal {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.asked)
}

// THE CODE TOOLS ARE SERVED PER CALL, AND A CALL TO ONE IS A READ. The
// catalogue holds the eleven exactly while the call's company hosts code — so a
// revision that turns Gitlet on serves them from the next call and one
// that turns it off withdraws them from the next — each advertised as the
// proven read it is; and a call to one is answered as the request's person,
// bound to their seat, and leaves no `operator_acted` record, as no read does.
//
// Mutation: catalogueFor dropping Halves.Code, or a code tool annotated as a
// write → red.
func TestTheCodeToolsAreServedPerCallAndAuditedAsReads(t *testing.T) {
	t.Parallel()
	var hosting atomic.Bool
	host := &listingHost{}
	audit := &auditLog{}
	s := newSurface(t, operator.Options{
		Halves: func() (operator.Halves, bool) {
			halves := operator.Halves{Work: builtin.WorkDeps{Reader: stubWorkReader{}, Writer: stubWorkWriter}}
			if hosting.Load() {
				halves.Code = builtin.CodeDeps{Code: host, Requesters: noRequesters{}, Chart: authz.NoChart{}}
			}
			return halves, true
		},
		Org: company, Audit: audit,
	})
	for _, name := range builtin.CodeToolNames() {
		if slices.Contains(s.Tools(), name) {
			t.Errorf("a company hosting no code is offered %s", name)
		}
	}

	hosting.Store(true)
	for _, name := range builtin.CodeToolNames() {
		if !slices.Contains(s.Tools(), name) {
			t.Errorf("the next call after the company hosts code is offered no %s", name)
		}
		if !crewletmcp.ReadOnlyProven(s.Annotations(name)) {
			t.Errorf("%s is advertised as %+v, not as a proven read", name, s.Annotations(name))
		}
	}
	got, err := s.Dispatch(iam.WithPrincipal(t.Context(), founder), operator.Call{
		Transport: types.TransportMCP, Tool: builtin.CodeListReposTool, Args: map[string]any{},
	})
	if err != nil || got.Failed {
		t.Fatalf("the founder's listing answered %v %q", err, got.Output)
	}
	want := gitlet.Principal{Kind: gitlet.PrincipalOperator, OperatorID: "jane.founder", Seat: "jane-founder"}
	if asked := host.askers(); len(asked) != 1 || asked[0] != want {
		t.Errorf("the listing asked as %+v, want the founder bound to their seat, %+v", asked, want)
	}
	if recorded := audit.published(); len(recorded) != 0 {
		t.Errorf("a code read left %d audit records, want none", len(recorded))
	}

	hosting.Store(false)
	if slices.Contains(s.Tools(), builtin.CodeListReposTool) {
		t.Error("the next call after the company stops hosting code is still offered the code tools")
	}
}

// writingHost is listingHost with Gitlet's writes: never asked here,
// since what is under test is which tools are offered.
type writingHost struct{ listingHost }

func (*writingHost) GitWrite(context.Context, gitlet.EngineWrite) (gitlet.EngineWriteResult, error) {
	return gitlet.EngineWriteResult{}, gitlet.ErrNotServing
}

func (*writingHost) Lifecycle(context.Context, gitlet.LifecycleRequest) (gitlet.LifecycleResult, error) {
	return gitlet.LifecycleResult{}, gitlet.ErrNotServing
}

func (*writingHost) Change(context.Context, gitlet.ChangeProposal) (gitlet.ChangeResult, error) {
	return gitlet.ChangeResult{}, gitlet.ErrNotServing
}

func (*writingHost) Converse(context.Context, gitlet.ConversationWrite) (gitlet.ConversationResult, error) {
	return gitlet.ConversationResult{}, gitlet.ErrNotServing
}

func (*writingHost) Mark(context.Context, gitlet.MarkWrite) (gitlet.MarkResult, error) {
	return gitlet.MarkResult{}, gitlet.ErrNotServing
}

func (*writingHost) Checks(context.Context, gitlet.ChecksRequest) (gitlet.ChecksResult, error) {
	return gitlet.ChecksResult{}, gitlet.ErrNotServing
}

func (*writingHost) Queue(context.Context, gitlet.QueueProposal) (gitlet.QueueResult, error) {
	return gitlet.QueueResult{}, gitlet.ErrNotServing
}

func (*writingHost) Expunge(context.Context, gitlet.ExpungeProposal) (gitlet.ExpungeResult, error) {
	return gitlet.ExpungeResult{}, gitlet.ErrNotServing
}

// THE CODE WRITES ARE SERVED WHERE GITLET'S WRITES ARE, AND A PERSON MAY
// PRESS THEM: the catalogue holds the code writes beside the reads exactly
// while the call's code half carries Gitlet's writes, advertises each
// as the write it is, and the act transport's account of what a person may
// press (Acts) names every one — admitted for every person, and decided per
// repository by the grant; the purge too, which asks a step-up the founder
// here has not made, since the press is what asks for the fresher proof
// (builtin.Admits). A code half with no writer offers the reads alone.
//
// Mutation: the code writes annotated as reads → absent from Acts, red; and
// Admits refusing a verb on the step-up it asks → the purge absent, red.
func TestTheCodeWritesAreServedAndActable(t *testing.T) {
	t.Parallel()
	var writing atomic.Bool
	host := &writingHost{}
	s := newSurface(t, operator.Options{
		Halves: func() (operator.Halves, bool) {
			halves := operator.Halves{Work: builtin.WorkDeps{Reader: stubWorkReader{}, Writer: stubWorkWriter}}
			halves.Code = builtin.CodeDeps{Code: host, Requesters: noRequesters{}, Chart: authz.NoChart{}}
			if writing.Load() {
				halves.Code.Writer, halves.Code.Holder = host, host
			}
			return halves, true
		},
		// A chart that leads nobody, as every surface whose Acts is read
		// has: Acts decides answer_executor by the run's lead, which NoChart
		// cannot answer.
		Org: company, Audit: &auditLog{}, Authorize: builtin.Decide(leadsNobody{}),
	})
	for _, name := range builtin.CodeWrites() {
		if slices.Contains(s.Tools(), name) {
			t.Errorf("a code half with no writer offers %s", name)
		}
	}
	writing.Store(true)
	acts, err := s.Acts(t.Context(), founder)
	if err != nil {
		t.Fatalf("Acts: %v", err)
	}
	for _, name := range builtin.CodeWrites() {
		if !slices.Contains(s.Tools(), name) {
			t.Errorf("a code half with Gitlet's writes offers no %s", name)
		}
		if crewletmcp.ReadOnlyProven(s.Annotations(name)) {
			t.Errorf("%s is advertised as a read", name)
		}
		if !slices.Contains(acts, name) {
			t.Errorf("the act transport would not serve %s to a person: acts %v", name, acts)
		}
	}
	for _, name := range builtin.CodeToolNames() {
		if slices.Contains(acts, name) {
			t.Errorf("the act transport would serve the read %s", name)
		}
	}
}
