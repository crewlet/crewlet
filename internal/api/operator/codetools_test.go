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
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	crewletmcp "github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/statelog"
)

// listingHost is a code host whose one listing is empty, recording who asked
// for it: what an operator's code tool reads through.
type listingHost struct {
	mu    sync.Mutex
	asked []code.Principal
}

func (h *listingHost) Read(_ context.Context, q code.ReadQuery) (code.ReadAnswer, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.asked = append(h.asked, q.Asker)
	return code.ReadAnswer{Level: q.Fresh.Level}, nil
}

func (h *listingHost) ResolveRef(context.Context, code.RefQuery) (code.RefAnswer, error) {
	return code.RefAnswer{}, code.ErrRefNotFound
}

func (h *listingHost) GitRead(context.Context, code.GitQuery) (code.GitAnswer, error) {
	return code.GitAnswer{}, code.ErrObjectNotFound
}

func (h *listingHost) FloorOf(string) statelog.Position { return statelog.Position{} }

// noRequesters composes nobody: the operator surface's calls are made in no
// turn, so no code read there is ever made for anybody but its caller — and
// the code tools are wired with the requester ceiling's halves or not at all.
type noRequesters struct{}

func (noRequesters) Requester(_ context.Context, handle string) (iam.Principal, error) {
	return iam.Principal{}, fmt.Errorf("the operator surface composes no requester, and was asked for %q", handle)
}

func (h *listingHost) askers() []code.Principal {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.asked)
}

// THE CODE TOOLS ARE SERVED PER CALL, AND A CALL TO ONE IS A READ. The
// catalogue holds the nine exactly while the call's company hosts code — so a
// revision that turns `code.native` on serves them from the next call and one
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
	want := code.Principal{Kind: code.PrincipalOperator, OperatorID: "jane.founder", Seat: "jane-founder"}
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
