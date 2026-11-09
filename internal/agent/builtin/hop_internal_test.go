package builtin

import (
	"context"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/crewlet/crewlet/internal/a2a"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/org"
)

// WHO A HAND-ON WORKS FOR IS THE HANDING TURN'S OWN CEILING (ADR-0109), read
// off the call's binding and nothing else: nobody for a turn that works for its
// own seat; the seat of a person or a colleague seat; a credential with the
// apps its ceiling holds it to; a stranger for a stranger and for any ceiling
// this build cannot read.
//
// Mutation: carry a credential's grants as the directory states them rather
// than as the ceiling narrowed them, and the narrowed token's ask is wider.
func TestWhoAHandOnWorksForIsTheHandingTurnsOwnCeiling(t *testing.T) {
	t.Parallel()
	token := iam.Principal{Kind: iam.KindMachine, Login: "token:ci", Stage: iam.StageActive,
		Grants: []iam.Grant{iam.GrantWorklet}}
	for name, tc := range map[string]struct {
		ceiling authz.Ceiling
		want    *types.WorkingFor
	}{
		"its own seat's":   {authz.Ceiling{Kind: authz.CeilingNone}, nil},
		"a stranger's":     {authz.Ceiling{Kind: authz.CeilingStranger}, &types.WorkingFor{Kind: types.WorkingForStranger}},
		"a person's":       {authz.Ceiling{Kind: authz.CeilingRequester, Principal: lena}, &types.WorkingFor{Kind: types.WorkingForSeat, Seat: "lena"}},
		"a colleague seat": {authz.Ceiling{Kind: authz.CeilingRequester, Principal: iam.Principal{Kind: iam.KindSeat, Seat: "cto"}}, &types.WorkingFor{Kind: types.WorkingForSeat, Seat: "cto"}},
		"a credential's": {authz.Ceiling{Kind: authz.CeilingRequester, Principal: token},
			&types.WorkingFor{Kind: types.WorkingForCredential, Login: "token:ci", Apps: []iam.Grant{iam.GrantWorklet}}},
		"a person with no seat": {authz.Ceiling{Kind: authz.CeilingRequester, Principal: iam.Principal{Kind: iam.KindPerson}}, &types.WorkingFor{Kind: types.WorkingForStranger}},
		"a ceiling of no kind":  {authz.Ceiling{Kind: "lost"}, &types.WorkingFor{Kind: types.WorkingForStranger}},
	} {
		if got := workingForOf(tc.ceiling); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: the hand-on works for %+v, want %+v", name, got, tc.want)
		}
	}
}

// A HANDED-ON TURN IS HELD TO WHOEVER IT CARRIED, read before the seat that
// asked or answered, and composed again on this node: a person as the
// directory holds them now, a credential narrowed by this node's resolution and
// never widened, the seat itself (carried back to it) as its own work, a
// stranger as one, and a kind this build does not know as a stranger.
//
// Mutation: read the turn's requester before its carrier, and the turn a
// colleague's ask woke for mallory uses the asking agent's every app.
func TestAHandedOnTurnIsHeldToWhoeverItCarried(t *testing.T) {
	t.Parallel()
	asked := &atomic.Int64{}
	r := countedRequesters{asked: asked, of: map[string]iam.Principal{
		"mallory": mallory, "lena": lena,
		"agent-asker":     {Kind: iam.KindSeat, Seat: "agent-asker", Grants: iam.Apps},
		"login:token:ci":  {Kind: iam.KindMachine, Login: "token:ci", Stage: iam.StageActive, Grants: []iam.Grant{iam.GrantWorklet}},
		"login:token:off": {},
	}}
	turn := func(w *types.WorkingFor) *turnctx.Turn {
		t := opsBotTurn("agent-asker")
		t.WorkingFor = w
		return t
	}
	for name, tc := range map[string]struct {
		w      *types.WorkingFor
		kind   authz.CeilingKind
		who    string
		grants []iam.Grant
	}{
		"a person who uses no app": {&types.WorkingFor{Kind: types.WorkingForSeat, Seat: "mallory"},
			authz.CeilingRequester, "mallory", nil},
		"a credential, narrowed here": {&types.WorkingFor{Kind: types.WorkingForCredential, Login: "token:ci",
			Apps: []iam.Grant{iam.GrantWorklet, iam.GrantDoclet}}, authz.CeilingRequester, "token:ci",
			[]iam.Grant{iam.GrantWorklet}},
		"the seat itself, carried back": {&types.WorkingFor{Kind: types.WorkingForSeat, Seat: "ops-bot"},
			authz.CeilingNone, "", nil},
		"a seat with no handle":   {&types.WorkingFor{Kind: types.WorkingForSeat}, authz.CeilingStranger, "", nil},
		"a stranger":              {&types.WorkingFor{Kind: types.WorkingForStranger}, authz.CeilingStranger, "", nil},
		"a kind this build lacks": {&types.WorkingFor{Kind: "guest"}, authz.CeilingStranger, "", nil},
	} {
		c, err := composeCeiling(t.Context(), turn(tc.w), "ops-bot", r)
		who := c.Principal.Seat
		if c.Principal.Kind == iam.KindMachine {
			who = c.Principal.Login
		}
		if err != nil || c.Kind != tc.kind || who != tc.who || !reflect.DeepEqual(iam.AppsOf(c.Principal.Grants),
			iam.AppsOf(tc.grants)) {
			t.Errorf("%s: the turn is held to %s %q using %v (%v), want %s %q using %v", name, c.Kind, who,
				c.Principal.Grants, err, tc.kind, tc.who, tc.grants)
		}
	}
	// A TURN NO HAND-ON WOKE reads its requester as before: the asking agent.
	if c, err := composeCeiling(t.Context(), turn(nil), "ops-bot", r); err != nil ||
		c.Kind != authz.CeilingRequester || c.Principal.Seat != "agent-asker" {
		t.Errorf("a turn no hand-on woke is held to %+v (%v), want the requester it names", c, err)
	}
}

// recordingAsker records every ask it is handed.
type recordingAsker struct {
	mu   sync.Mutex
	asks []a2a.Ask
}

func (a *recordingAsker) Open(_ context.Context, ask a2a.Ask) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.asks = append(a.asks, ask)
	return "c1", nil
}

// A COLLEAGUE'S ASK CARRIES WHOEVER THE ASKING TURN WORKS FOR, from the
// call's own ceiling: a person's turn asks for the person, a credential's for
// the credential at the apps it is held to here, a turn a hand-on woke for
// whoever that carried (so a chain never widens), and a turn nobody woke for
// nobody beyond its seat. NEVER FROM THE CALL'S ARGUMENTS: an ask whose
// arguments name somebody to work for is no wider than the turn's own.
//
// Mutation: read a `working_for` off the arguments, and the turn nobody woke
// asks its colleague to work for the CEO.
func TestAColleaguesAskCarriesWhoeverTheAskingTurnWorksFor(t *testing.T) {
	t.Parallel()
	o := &org.Organization{Name: "Acme", Roles: []*org.Role{
		{Name: "Ops Bot", DeclaredHandle: "ops-bot"},
		{Name: "CTO", DeclaredHandle: "cto"},
		{Name: "CEO", DeclaredHandle: "ceo"},
		{Name: "Lena", DeclaredHandle: "lena", Kind: org.KindHuman},
	}}
	o.Normalize()
	r := countedRequesters{asked: &atomic.Int64{}, of: map[string]iam.Principal{
		"lena":           lena,
		"login:token:ci": {Kind: iam.KindMachine, Login: "token:ci", Stage: iam.StageActive, Grants: []iam.Grant{iam.GrantWorklet}},
	}}
	turnOf := func(requester string) *turnctx.Turn {
		return &turnctx.Turn{Org: o, Seat: o.SeatByHandle("ops-bot"), Requester: requester}
	}
	token := turnOf("")
	token.RequesterLogin, token.RequesterApps = "token:ci", []iam.Grant{iam.GrantWorklet, iam.GrantDoclet}
	chained := turnOf("cto")
	chained.WorkingFor = &types.WorkingFor{Kind: types.WorkingForSeat, Seat: "lena"}
	for name, tc := range map[string]struct {
		turn *turnctx.Turn
		args map[string]any
		want *types.WorkingFor
	}{
		"a person's turn": {turnOf("lena"), nil, &types.WorkingFor{Kind: types.WorkingForSeat, Seat: "lena"}},
		"a credential's turn": {token, nil, &types.WorkingFor{Kind: types.WorkingForCredential,
			Login: "token:ci", Apps: []iam.Grant{iam.GrantWorklet}}},
		"a turn a hand-on woke for lena": {chained, nil, &types.WorkingFor{Kind: types.WorkingForSeat, Seat: "lena"}},
		"a turn nobody woke":             {turnOf(""), nil, nil},
		"a turn nobody woke, arguments naming the CEO": {turnOf(""), map[string]any{
			"working_for": map[string]any{"kind": "seat", "seat": "ceo"}}, nil},
	} {
		svc := &recordingAsker{}
		tool := &a2aAsk{svc: svc, requesters: r}
		args := map[string]any{"target": "cto", "brief": "Tail the api log."}
		for k, v := range tc.args {
			args[k] = v
		}
		if tc.turn == chained {
			args["target"] = "ceo"
		}
		res, err := tool.CallForTurn(t.Context(), tc.turn, args)
		if err != nil || res.Failed || len(svc.asks) != 1 {
			t.Errorf("%s: the ask answered %+v (%v) and opened %d channels", name, res, err, len(svc.asks))
			continue
		}
		if got := svc.asks[0].WorkingFor; !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: the ask carries %+v, want %+v", name, got, tc.want)
		}
	}
}

// A PICKUP TURN IS HELD AS THE ASSIGNMENT TURN IS (ADR-0109, ADR-0112): the
// turn the pickup starts on an item is held to the person or the credential
// the item is for exactly as the turn the item's assignment woke, so work a
// person without an app filed is refused that app in both.
//
// Mutation: compose the pickup turn as a turn nobody woke, and it uses the
// seat's every app.
func TestAPickupTurnIsHeldAsTheAssignmentTurnIs(t *testing.T) {
	t.Parallel()
	r := countedRequesters{asked: &atomic.Int64{}, of: map[string]iam.Principal{
		"lena": lena,
		"login:token:ci": {Kind: iam.KindMachine, Login: "token:ci", Stage: iam.StageActive,
			Grants: []iam.Grant{iam.GrantWorklet}},
	}}
	assignedBy := opsBotTurn("lena")
	pickedUp := opsBotTurn("")
	pickedUp.WorkingFor = &types.WorkingFor{Kind: types.WorkingForSeat, Seat: "lena"}
	byToken := opsBotTurn("")
	byToken.RequesterLogin, byToken.RequesterApps = "token:ci", []iam.Grant{iam.GrantWorklet}
	pickedForToken := opsBotTurn("")
	pickedForToken.WorkingFor = &types.WorkingFor{Kind: types.WorkingForCredential,
		Login: "token:ci", Apps: []iam.Grant{iam.GrantWorklet}}
	for name, pair := range map[string][2]*turnctx.Turn{
		"a person's item":     {assignedBy, pickedUp},
		"a credential's item": {byToken, pickedForToken},
	} {
		assignment, err := composeCeiling(t.Context(), pair[0], "ops-bot", r)
		if err != nil {
			t.Fatalf("%s: compose the assignment turn: %v", name, err)
		}
		picked, err := composeCeiling(t.Context(), pair[1], "ops-bot", r)
		if err != nil {
			t.Fatalf("%s: compose the pickup turn: %v", name, err)
		}
		if picked.Kind != authz.CeilingRequester || !reflect.DeepEqual(picked, assignment) {
			t.Errorf("%s: the pickup turn is held to %+v and the assignment turn to %+v",
				name, picked, assignment)
		}
		if slices.Contains(picked.Principal.Grants, iam.GrantDoclet) {
			t.Errorf("%s: the pickup turn may use Doclet, which the item's requester may not", name)
		}
	}
}
