package builtin

import (
	"context"
	"errors"
	"slices"
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

// Credential places a credential by the entry keyed "login:" and its login,
// counted as a seat's composition is.
func (r countedRequesters) Credential(_ context.Context, login string) (iam.Principal, bool, error) {
	r.asked.Add(1)
	p, ok := r.of["login:"+login]
	return p, ok, nil
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
// USE (ADR-0109), through the gate every builtin passes: a call the seat's own
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

// A REQUESTER WHO MAY NOT ACT IS TOLD SO, AND NEVER SENT FOR AN APP.
//
// A person whose stage may not act — suspended, or removed — holds every app
// they were given and may still use none, so an app verb in a turn they woke
// is refused for their stage: the agent is told they may not act in the
// company now, and never that an administrator could give them the app, which
// they hold and which would change nothing. The CONTROL is a requester who
// may act and lacks the app, who is told the app.
//
// Mutation: refuse a requester who may not act naming the row's app again —
// the suspended requester is sent for an app they hold.
func TestARequesterWhoMayNotActIsToldSoNotSentForAnApp(t *testing.T) {
	t.Parallel()
	authorize := Decide(authz.NoChart{})
	suspended := iam.Principal{Kind: iam.KindPerson, Seat: "sam", Stage: iam.StageSuspended,
		Grants: slices.Clone(iam.Apps)}
	people := countedRequesters{of: map[string]iam.Principal{"sam": suspended, "mallory": mallory},
		asked: &atomic.Int64{}}
	const verb = authz.ActionWorkList
	tool := gate(rowTool{name: string(verb), authorize: authorize, ran: &atomic.Int64{}}, authorize, people)
	run := func(turn *turnctx.Turn) mcp.Result {
		res, refused, err := tools.Invoke(t.Context(), tools.Invocation{Name: string(verb), Tool: tool,
			Args: tools.ArgsOf(tool, false, map[string]any{}), AsSeat: true, Turn: turn})
		if err != nil || refused {
			t.Fatalf("the invocation itself failed (%v, refused %v)", err, refused)
		}
		return res
	}
	res := run(opsBotTurn("sam"))
	var refused *DecisionRefused
	if !res.Failed || !errors.As(res.Cause, &refused) ||
		refused.Decision.RequesterReason != authz.ReasonStage ||
		!strings.Contains(res.Output, "may not act in the company now") ||
		strings.Contains(res.Output, "administrator can give them") {
		t.Errorf("a turn a suspended person woke answered %+v, want refused for their stage, sending "+
			"nobody for an app", res)
	}
	res = run(opsBotTurn("mallory"))
	if !res.Failed || !strings.Contains(res.Output, "administrator can give them Worklet") {
		t.Errorf("a turn a person without Worklet woke answered %q, want told the app", res.Output)
	}
}

// tokenTurn is a turn of the agent seat ops-bot that a credential acting as no
// seat woke, by its login, its writes carrying apps.
func tokenTurn(login string, apps ...iam.Grant) *turnctx.Turn {
	turn := opsBotTurn("")
	turn.RequesterLogin, turn.RequesterApps = login, apps
	return turn
}

// machine is a credential acting as no seat, as this node resolves it.
func machine(login string, grants ...iam.Grant) iam.Principal {
	return iam.Principal{Kind: iam.KindMachine, Login: login, Stage: iam.StageActive, Grants: grants}
}

// A TOKEN'S WORK USES THE APPS IT HOLDS, AND NONE IT DOES NOT (ADR-0109): a
// credential acting as no seat that woke a turn is that turn's requester, by
// its own apps and never admin. A pipeline's `[worklet]` token that filed an
// item has the agent comment on it and refuses it the pages, naming the
// credential and the app; an `[admin]` token's work uses every app; a login
// this node cannot place, and a write that carried no apps, are a stranger's.
//
// Mutation: compose a turn's RequesterLogin as nobody (CeilingNone) — the
// Worklet token's turn reads the pages; or as a stranger — its comment is
// refused.
func TestATokensWorkUsesTheAppsItHolds(t *testing.T) {
	t.Parallel()
	authorize := Decide(authz.NoChart{})
	people := countedRequesters{asked: &atomic.Int64{}, of: map[string]iam.Principal{
		"login:token:ci":  machine("token:ci", iam.GrantWorklet),
		"login:token:ops": machine("token:ops", iam.GrantAdmin),
	}}
	run := func(verb authz.Action, turn *turnctx.Turn) mcp.Result {
		t.Helper()
		row := rowTool{name: string(verb), authorize: authorize, ran: &atomic.Int64{}}
		if subjectOf[string(verb)].inTool {
			// A ROW ITS TOOL DECIDES, asked from inside a tool whose own row
			// names no app — as the comment tool asks it.
			row = rowTool{name: string(authz.ActionColleagueAsk), authorize: authorize,
				asks: []authz.Action{verb}, ran: &atomic.Int64{}}
		}
		tool := gate(row, authorize, people)
		res, refused, err := tools.Invoke(t.Context(), tools.Invocation{Name: tool.Name(), Tool: tool,
			Args: tools.ArgsOf(tool, false, map[string]any{}), AsSeat: true, Turn: turn})
		if err != nil || refused {
			t.Fatalf("%s: the invocation itself failed (%v, refused %v)", verb, err, refused)
		}
		return res
	}

	ci := tokenTurn("token:ci", iam.GrantWorklet)
	if res := run(authz.ActionWorkComment, ci); res.Failed {
		t.Errorf("a comment in a turn the Worklet token woke answered %q, want it run", res.Output)
	}
	res := run(authz.ActionPageList, ci)
	var refused *DecisionRefused
	if !res.Failed || !errors.As(res.Cause, &refused) || refused.Decision.Reason != authz.ReasonCeiling ||
		refused.Decision.Ceiling != authz.CeilingRequester ||
		!slices.Equal(refused.Decision.RequesterGrants, []iam.Grant{iam.GrantDoclet}) ||
		!strings.Contains(res.Output, "the credential this turn is working for does not use Doclet") {
		t.Errorf("the pages in a turn the Worklet token woke answered %+v, want the ceiling naming "+
			"the credential and Doclet", res)
	}

	ops := tokenTurn("token:ops", iam.AppsOf([]iam.Grant{iam.GrantAdmin})...)
	walked := 0
	for _, a := range gateDecided() {
		if res := run(a, opsBotTurn("")); res.Failed {
			continue
		}
		walked++
		if res := run(a, ops); res.Failed {
			t.Errorf("%s in a turn the admin token woke answered %q, want it run: admin "+
				"includes every app", a, res.Output)
		}
	}
	if walked == 0 {
		t.Fatal("no row the gate decides was walked: the admin token's case holds nothing")
	}

	for name, turn := range map[string]*turnctx.Turn{
		"a login this node cannot place": tokenTurn("token:gone", iam.GrantWorklet),
		"a write that carried no apps":   tokenTurn("token:ci"),
	} {
		res := run(authz.ActionWorkComment, turn)
		if !res.Failed || !errors.As(res.Cause, &refused) || refused.Decision.Ceiling != authz.CeilingStranger {
			t.Errorf("a comment in a turn %s woke answered %+v, want a stranger's", name, res)
		}
	}
}

// A CREDENTIAL IS BOUND BY THE NARROWER OF ITS WRITE AND THIS NODE (ADR-0109).
// Tier A is per node: the apps a token's write carried are what the node that
// took it resolved, and this node's resolution is what the token is here, now.
// Each alone widens it somewhere — a token one node narrowed acting on
// another's wider grants, or a write recorded before the token was narrowed
// acting on the old ones — so a call uses only an app both name.
//
// Mutation: bound the credential by its carried apps alone — the token this
// node narrowed to Worklet reads the pages; by this node's alone — the write
// that carried Worklet reads them.
func TestACredentialIsBoundByTheNarrowerOfItsWriteAndThisNode(t *testing.T) {
	t.Parallel()
	authorize := Decide(authz.NoChart{})
	people := countedRequesters{asked: &atomic.Int64{}, of: map[string]iam.Principal{
		"login:token:narrowed-here":  machine("token:narrowed-here", iam.GrantWorklet),
		"login:token:narrowed-there": machine("token:narrowed-there", iam.GrantAdmin),
	}}
	tool := gate(rowTool{name: string(authz.ActionPageList), authorize: authorize, ran: &atomic.Int64{}},
		authorize, people)
	for name, turn := range map[string]*turnctx.Turn{
		"narrowed on this node":   tokenTurn("token:narrowed-here", iam.GrantWorklet, iam.GrantDoclet),
		"narrowed where it wrote": tokenTurn("token:narrowed-there", iam.GrantWorklet),
	} {
		res, _, err := tools.Invoke(t.Context(), tools.Invocation{Name: tool.Name(), Tool: tool,
			Args: tools.ArgsOf(tool, false, map[string]any{}), AsSeat: true, Turn: turn})
		var refused *DecisionRefused
		if err != nil || !res.Failed || !errors.As(res.Cause, &refused) ||
			refused.Decision.Reason != authz.ReasonCeiling || refused.Decision.RequesterKind != iam.KindMachine {
			t.Errorf("the pages in a turn a token %s woke answered %+v (%v), want the ceiling",
				name, res, err)
		}
	}
}
