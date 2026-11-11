package builtin_test

import (
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/tools"
)

// writeFamily is one family's write tool, wired over its fake with whoever
// composes a turn's requester, and what the writer was handed for each write.
type writeFamily struct {
	tool string
	args map[string]any
	// wire registers the family's tools over a fresh fake and answers a
	// read of whom every write the fake took was made for.
	wire func(t *testing.T, who builtin.Requesters) (*tools.Registry, func() []types.WrittenFor)
}

// writeFamilies are Worklet's, Doclet's and Chatlet's writes that wake a
// colleague: an item filed, a page comment naming one, a message in a room.
func writeFamilies() []writeFamily {
	return []writeFamily{
		{tool: builtin.CreateWorkItemTool, args: map[string]any{"title": "Read the runbook", "project": "ENG"},
			wire: func(t *testing.T, who builtin.Requesters) (*tools.Registry, func() []types.WrittenFor) {
				trk := newFakeTracker()
				reg := tools.NewRegistry()
				if _, err := builtin.Register(reg, gated(builtin.Deps{
					Work: builtin.WorkDeps{Reader: trk, Writer: trk.as}, Requesters: who,
				})); err != nil {
					t.Fatalf("register: %v", err)
				}
				return reg, func() []types.WrittenFor {
					var out []types.WrittenFor
					for _, a := range trk.actors {
						out = append(out, a.WrittenFor)
					}
					return out
				}
			}},
		{tool: builtin.CommentOnPageTool, args: map[string]any{"page": "p1", "asks": true,
			"body": "@agent-ceo is this current?"},
			wire: func(t *testing.T, who builtin.Requesters) (*tools.Registry, func() []types.WrittenFor) {
				kb := newFakeKB()
				reg := tools.NewRegistry()
				if _, err := builtin.Register(reg, gated(builtin.Deps{
					Pages:      builtin.PageDeps{Reader: kb, Writer: kb, Mentions: fakeMentions{"agent-ceo"}},
					Requesters: who,
				})); err != nil {
					t.Fatalf("register: %v", err)
				}
				return reg, func() []types.WrittenFor {
					var out []types.WrittenFor
					for _, a := range kb.actors {
						out = append(out, a.WrittenFor)
					}
					return out
				}
			}},
		{tool: builtin.PostMessageTool, args: map[string]any{"channel": chatRoomID, "asks": true,
			"body": "@agent-ceo can you read the runbook?"},
			wire: func(t *testing.T, who builtin.Requesters) (*tools.Registry, func() []types.WrittenFor) {
				chat := newFakeChat()
				reg := tools.NewRegistry()
				if _, err := builtin.Register(reg, gated(builtin.Deps{Chat: chatDeps(chat), Requesters: who})); err != nil {
					t.Fatalf("register: %v", err)
				}
				return reg, func() []types.WrittenFor {
					var out []types.WrittenFor
					for _, p := range chat.posts {
						out = append(out, p.actor.WrittenFor)
					}
					return out
				}
			}},
	}
}

// A WRITE CARRIES WHOEVER THE WRITING TURN WORKS FOR (ADR-0109), composed from
// the call's own bound ceiling on every family's write alike: a person's turn
// writes for the person, a credential's for the credential at the apps this
// node holds it to, a turn a colleague's ask woke for whoever that carried (so
// a chain B to C to D carries the founder end to end), and a turn nobody woke
// for nobody beyond its seat, which still STATES so: the stores refuse an
// agent's write that states nothing.
//
// Mutation: drop the carrier from one family's actor (pageActor builds its
// actor without it), and that family's writes state nothing: this names it,
// and the store refuses them.
func TestAWriteCarriesWhoeverTheWritingTurnWorksFor(t *testing.T) {
	t.Parallel()
	every := []iam.Grant{iam.GrantWorklet, iam.GrantDoclet, iam.GrantChatlet}
	who := requesters{
		"founder":        founderPrincipal(every...),
		"login:token:ci": {Kind: iam.KindMachine, Login: "token:ci", Stage: iam.StageActive, Grants: every},
	}
	founder := &types.WorkingFor{Kind: types.WorkingForSeat, Seat: "founder"}
	turnOf := func(requester string) *turnctx.Turn {
		turn := turnFor(t, "agent-cto")
		turn.Requester = requester
		return turn
	}
	token := turnOf("")
	token.RequesterLogin = "token:ci"
	token.RequesterApps = append(slices.Clone(every), iam.GrantArtifacts)
	chained := turnOf("agent-ceo")
	chained.WorkingFor = founder
	for _, family := range writeFamilies() {
		for name, c := range map[string]struct {
			turn *turnctx.Turn
			want *types.WorkingFor
		}{
			"a person's turn": {turnOf("founder"), founder},
			// NARROWED HERE: the write carried artifacts, which this node
			// gives the token no longer.
			"a credential's turn": {token, &types.WorkingFor{Kind: types.WorkingForCredential, Login: "token:ci",
				Apps: iam.AppsOf(every)}},
			"a turn a colleague's ask woke for the founder": {chained, founder},
			"a turn nobody woke":                            {turnOf(""), nil},
		} {
			reg, written := family.wire(t, who)
			got := callAsSeat(t, mustLookup(t, reg, family.tool), c.turn, family.args)
			if got.Failed {
				t.Errorf("%s in %s failed: %s", family.tool, name, got.Output)
				continue
			}
			stated := written()
			if len(stated) != 1 || !stated[0].Stated() || !reflect.DeepEqual(stated[0].Carrier(), c.want) {
				t.Errorf("%s in %s wrote for %+v, want %+v, stated", family.tool, name, stated, c.want)
			}
		}
	}
}

// A WRITE WHOSE ARGUMENTS NAME SOMEBODY TO WORK FOR GETS NOTHING WIDER (ADR-0109):
// the carrier is the engine's, composed from the call's bound ceiling, and
// nothing a model writes reaches it. At the gate such a call is refused for
// the argument nobody declared, so nothing is written; and the tool itself,
// handed the argument past the gate, writes for whoever its turn works for,
// here nobody, never for the CEO the arguments name.
//
// Mutation: compose the carrier from the arguments instead of the ceiling (a
// `working_for` read off the call), and the turn nobody woke writes for the
// CEO.
func TestAWriteWhoseArgumentsNameSomebodyGetsNothingWider(t *testing.T) {
	t.Parallel()
	forged := map[string]any{"kind": "seat", "seat": "agent-ceo"}
	for _, family := range writeFamilies() {
		args := map[string]any{"working_for": forged}
		for k, v := range family.args {
			args[k] = v
		}
		reg, written := family.wire(t, requesters{})
		tool := mustLookup(t, reg, family.tool)
		turn := turnFor(t, "agent-cto")

		refused := callAsSeat(t, tool, turn, args)
		if !refused.Failed || !errors.Is(refused.Cause, builtin.ErrUndeclaredArgument) || len(written()) != 0 {
			t.Errorf("%s with a working_for answered %q (cause %v) and wrote %d times, want the "+
				"argument refused and nothing written", family.tool, refused.Output, refused.Cause, len(written()))
		}

		inner, ok := tool.(interface{ Unwrap() tools.Callable })
		if !ok {
			t.Fatalf("%s is not gated", family.tool)
		}
		seat, ok := inner.Unwrap().(tools.SeatCallable)
		if !ok {
			t.Fatalf("%s is not seat-callable", family.tool)
		}
		ctx := builtin.OutsideAnyTurn(turnctx.WithPrincipal(t.Context(), turn))
		got, err := seat.CallForTurn(ctx, turn, args)
		if err != nil || got.Failed {
			t.Errorf("%s past the gate failed: %v %s", family.tool, err, got.Output)
			continue
		}
		stated := written()
		if len(stated) != 1 || !stated[0].Stated() || stated[0].Carrier() != nil {
			t.Errorf("%s whose arguments name the CEO wrote for %+v, want nobody beyond the seat",
				family.tool, stated)
		}
	}
}

// A WRITE WHOSE TURN'S REQUESTER CANNOT BE COMPOSED WRITES NOTHING (ADR-0109):
// it is refused as a node that could not tell, to be tried again, and never
// written as a turn nobody woke, which would wake whoever it reaches for the
// seat with every app. Reached here past a gate that bound nothing, with no
// composer to ask, as a wiring that skipped one would.
//
// Mutation: answer a requester that could not be composed as a turn nobody
// woke (writtenForTurn's error dropped), and each family writes for nobody.
func TestAWriteWhoseRequesterCannotBeComposedWritesNothing(t *testing.T) {
	t.Parallel()
	for _, family := range writeFamilies() {
		reg, written := family.wire(t, requesters{})
		inner, ok := mustLookup(t, reg, family.tool).(interface{ Unwrap() tools.Callable })
		if !ok {
			t.Fatalf("%s is not gated", family.tool)
		}
		turn := turnFor(t, "agent-cto")
		turn.Requester = "founder"
		got, err := inner.Unwrap().(tools.SeatCallable).CallForTurn(turnctx.WithPrincipal(t.Context(), turn),
			turn, family.args)
		if err != nil || !got.Failed || tools.RefusalOf(got) != tools.RefusalUnavailable ||
			!errors.Is(got.Cause, builtin.ErrUndecidable) || len(written()) != 0 {
			t.Errorf("%s with no requester to compose answered %q (%s, cause %v, err %v) and wrote %d "+
				"times, want it unavailable and nothing written", family.tool, got.Output, tools.RefusalOf(got),
				got.Cause, err, len(written()))
		}
	}
}

// mustLookup is the registered tool named name.
func mustLookup(t *testing.T, reg *tools.Registry, name string) tools.Callable {
	t.Helper()
	entry, ok := reg.Lookup(name)
	if !ok {
		t.Fatalf("%s is not registered", name)
	}
	return entry.Tool
}

// callAsSeat is one seat-callable call in turn, its principal the turn's own seat
// as every surface derives it.
func callAsSeat(t *testing.T, tool tools.Callable, turn *turnctx.Turn, args map[string]any) tools.Result {
	t.Helper()
	seat, ok := tool.(tools.SeatCallable)
	if !ok {
		t.Fatalf("%s is not seat-callable", tool.Name())
	}
	got, err := seat.CallForTurn(t.Context(), turn, args)
	if err != nil {
		t.Fatalf("%s: %v", tool.Name(), err)
	}
	return got
}
