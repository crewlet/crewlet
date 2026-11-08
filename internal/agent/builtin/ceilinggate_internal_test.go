package builtin

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/tools"
)

// rowTool is a builtin standing for the authority row it is named after: it
// declares no argument and counts its runs, and — given asks — asks the
// authorizer for each of those actions from inside itself before it runs, as a
// tool deciding a stored row does.
type rowTool struct {
	name      string
	authorize Authorizer
	asks      []authz.Action
	ran       *atomic.Int64
}

func (r rowTool) Name() string        { return r.name }
func (r rowTool) Description() string { return "stands for its row" }
func (r rowTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

func (r rowTool) Call(ctx context.Context, _ map[string]any) (mcp.Result, error) {
	for _, a := range r.asks {
		if err := r.authorize(ctx, a, authz.Object{}); err != nil {
			return authorityRefusal(string(a), err), nil
		}
	}
	r.ran.Add(1)
	return mcp.Result{Output: "ran"}, nil
}

// countedRequesters composes each handle as the principal it maps to, counting
// every composition.
type countedRequesters struct {
	of    map[string]iam.Principal
	asked *atomic.Int64
}

func (r countedRequesters) Requester(_ context.Context, handle string) (iam.Principal, error) {
	r.asked.Add(1)
	p, ok := r.of[handle]
	if !ok {
		return iam.Principal{}, errors.New("no such seat")
	}
	return p, nil
}

// opsBotTurn is a turn of the agent seat ops-bot that requester woke ("" for a
// turn nobody woke).
func opsBotTurn(requester string) *turnctx.Turn {
	o := &org.Organization{Name: "Acme", Roles: []*org.Role{{Name: "Ops Bot", DeclaredHandle: "ops-bot"}}}
	o.Normalize()
	return &turnctx.Turn{Org: o, Seat: o.Roles[0], Requester: requester}
}

// mallory is a person who uses no app; lena uses Worklet alone.
var (
	mallory = iam.Principal{Kind: iam.KindPerson, Seat: "mallory", Stage: iam.StageActive}
	lena    = iam.Principal{Kind: iam.KindPerson, Seat: "lena", Stage: iam.StageActive,
		Grants: []iam.Grant{iam.GrantWorklet}}
)

// gateDecided is every builtin row the gate itself decides, on an object the
// call's arguments state, whose row names an app.
func gateDecided() []authz.Action {
	var out []authz.Action
	for _, a := range authz.Actions() {
		if strings.Contains(string(a), ".") || !authz.NamesApp(a) || subjectOf[string(a)].inTool {
			continue
		}
		out = append(out, a)
	}
	return out
}

// EVERY APP VERB A SEAT TAKES IN A TURN SOMEBODY WOKE IS HELD TO THE APPS THEY
// USE (ADR-0104), through the gate every builtin passes: a call the seat's own
// row admits is refused [authz.ReasonCeiling] when the person whose turn it is
// does not use the row's app, with the app named for them — and the same call
// in a turn nobody woke, or one a person using the app woke, runs. A
// read-only builtin is told its turn by [tools.Invoke] like a seat-scoped one
// ([tools.TurnBound]), so both shapes are walked. The walk is every row the
// gate decides that names an app, so an app's new verb is held the day it is
// written.
//
// Mutation: bind no ceiling in CallInTurn — the read-only builtins run for a
// requester who uses no app.
func TestEveryAppVerbIsHeldToTheRequestersApps(t *testing.T) {
	t.Parallel()
	authorize := Decide(authz.NoChart{})
	asked := &atomic.Int64{}
	people := countedRequesters{of: map[string]iam.Principal{"mallory": mallory, "lena": lena}, asked: asked}
	walked := 0
	for _, a := range gateDecided() {
		ran := &atomic.Int64{}
		tool := gate(rowTool{name: string(a), authorize: authorize, ran: ran}, authorize, people)
		run := func(turn *turnctx.Turn) mcp.Result {
			res, refused, err := tools.Invoke(t.Context(), tools.Invocation{Name: string(a), Tool: tool,
				Args: tools.ArgsOf(tool, false, map[string]any{}), AsSeat: true, Turn: turn})
			if err != nil || refused {
				t.Fatalf("%s: the invocation itself failed (%v, refused %v)", a, err, refused)
			}
			return res
		}
		if res := run(opsBotTurn("")); res.Failed {
			// The seat's own row refuses it — a person's gesture, a
			// human-only verb — and that is the answer whoever woke it.
			continue
		}
		walked++
		res := run(opsBotTurn("mallory"))
		var refused *DecisionRefused
		if !res.Failed || !errors.As(res.Cause, &refused) || refused.Decision.Reason != authz.ReasonCeiling ||
			len(refused.Decision.RequesterGrants) == 0 {
			t.Errorf("%s in a turn a person using no app woke answered %+v, want the ceiling naming their "+
				"missing app", a, res)
		}
		if app, _ := authz.AppOf(a); app == iam.GrantWorklet {
			if res := run(opsBotTurn("lena")); res.Failed {
				t.Errorf("%s in a turn a Worklet user woke answered %+v, want it run", a, res)
			}
		}
	}
	if walked == 0 {
		t.Fatal("no row the gate decides was walked: the case holds nothing")
	}
}

// A DECISION A TOOL TAKES INSIDE ITSELF IS HELD TO THE CEILING TOO: a row the
// gate leaves to the tool, decided once the tool has read what the call is
// about, reads the same ceiling the gate bound to the call — so a tool cannot
// do for a requester inside itself what its gate would have refused them.
//
// Mutation: decide in-tool actions with authz.Decide alone — the tool creates
// the item for a requester who uses no app.
func TestAnInToolDecisionIsHeldToTheCeiling(t *testing.T) {
	t.Parallel()
	authorize := Decide(authz.NoChart{})
	people := countedRequesters{of: map[string]iam.Principal{"mallory": mallory}, asked: &atomic.Int64{}}
	ran := &atomic.Int64{}
	// A row the gate leaves alone (no app), whose tool decides a Worklet verb
	// inside itself.
	tool := gate(rowTool{name: string(authz.ActionColleagueAsk), authorize: authorize,
		asks: []authz.Action{authz.ActionWorkCreate}, ran: ran}, authorize, people)
	res, _, err := tools.Invoke(t.Context(), tools.Invocation{Name: tool.Name(), Tool: tool,
		Args: tools.ArgsOf(tool, false, map[string]any{}), AsSeat: true, Turn: opsBotTurn("mallory")})
	var refused *DecisionRefused
	if err != nil || !res.Failed || !errors.As(res.Cause, &refused) ||
		refused.Decision.Reason != authz.ReasonCeiling || ran.Load() != 0 {
		t.Errorf("an in-tool Worklet decision for a requester using no app answered %+v (%v), ran %d times; "+
			"want the ceiling before it ran", res, err, ran.Load())
	}
}

// A BUILTIN CALLED OUTSIDE ANY TURN ANSWERS AS ITSELF — the operator's surface
// and the HTTP one call a read-only builtin with no turn — and asks nobody who
// woke anything: the ceiling is a turn's, and a principal acting through their
// own credential is no deputy.
//
// Mutation: make every gated tool seat-scoped — the plain call is refused for
// want of a seat.
func TestAPlainBuiltinStillAnswersOutsideATurn(t *testing.T) {
	t.Parallel()
	authorize := Decide(authz.NoChart{})
	asked := &atomic.Int64{}
	ran := &atomic.Int64{}
	tool := gate(rowTool{name: string(authz.ActionWorkCreate), authorize: authorize, ran: ran}, authorize,
		countedRequesters{of: map[string]iam.Principal{}, asked: asked})
	if _, seatScoped := tool.(tools.SeatCallable); seatScoped {
		t.Fatal("a read-only builtin's gate is seat-scoped, so it is refused outside a turn")
	}
	operator := iam.WithPrincipal(t.Context(), iam.Principal{Kind: iam.KindPerson, Login: "ana",
		Stage: iam.StageActive, Grants: []iam.Grant{iam.GrantWorklet}})
	res, _, err := tools.Invoke(operator, tools.Invocation{Name: tool.Name(), Tool: tool,
		Args: tools.ArgsOf(tool, false, map[string]any{})})
	if err != nil || res.Failed || ran.Load() != 1 || asked.Load() != 0 {
		t.Errorf("a person's call outside any turn answered %+v (%v), ran %d, composed %d requesters; "+
			"want it run asking nobody", res, err, ran.Load(), asked.Load())
	}
}

// ONE CALL COMPOSES ITS REQUESTER ONCE, AND ONLY WHEN IT NEEDS ONE: every
// decision the call makes reads the one composition its gate bound, and a call
// that decides no app verb reads nothing of the identity directory at all. A
// call is one routed read at most, never one per decision; the next call reads
// afresh, so a revocation applies at the turn's next call.
//
// Mutation: compose the ceiling at every decision — the call composes three
// times; compose it eagerly at the gate — the app-less call composes one.
func TestOneCallComposesItsRequesterOnce(t *testing.T) {
	t.Parallel()
	authorize := Decide(authz.NoChart{})
	asked := &atomic.Int64{}
	people := countedRequesters{of: map[string]iam.Principal{"lena": lena}, asked: asked}
	invoke := func(tool tools.Callable) {
		t.Helper()
		res, _, err := tools.Invoke(t.Context(), tools.Invocation{Name: tool.Name(), Tool: tool,
			Args: tools.ArgsOf(tool, false, map[string]any{}), AsSeat: true, Turn: opsBotTurn("lena")})
		if err != nil || res.Failed {
			t.Fatalf("%s answered %+v (%v), want it run", tool.Name(), res, err)
		}
	}
	thrice := gate(rowTool{name: string(authz.ActionColleagueAsk), authorize: authorize, ran: &atomic.Int64{},
		asks: []authz.Action{authz.ActionWorkCreate, authz.ActionWorkCreate, authz.ActionWorkCreate}},
		authorize, people)
	invoke(thrice)
	if n := asked.Load(); n != 1 {
		t.Errorf("one call deciding three Worklet verbs composed its requester %d times, want once", n)
	}
	invoke(thrice)
	if n := asked.Load(); n != 2 {
		t.Errorf("the next call composed %d requesters in all, want a fresh one (2)", n)
	}
	appless := gate(rowTool{name: string(authz.ActionColleagueAsk), authorize: authorize, ran: &atomic.Int64{}},
		authorize, people)
	invoke(appless)
	if n := asked.Load(); n != 2 {
		t.Errorf("a call deciding no app verb composed a requester (%d in all, want 2)", n)
	}
}
