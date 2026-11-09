package types_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
)

// AN AGENT'S WRITE STATES WHOM IT WAS MADE FOR, AND NOBODY ELSE'S CARRIES
// ANYBODY (ADR-0109). An agent writes only in a turn, which works for somebody
// or for the seat itself, or as the engine writes for it, so an agent's actor
// that states none of the three came from a builder that forgot: refused,
// never read as a write made for nobody, which would wake whoever it reaches
// for the agent with every app. A person, a credential and the engine write
// for nobody but themselves: the zero value is their answer, and a carrier on
// their write is refused.
//
// Mutation: read an agent's unstated actor as nobody — the forgotten builder's
// write is answered as made for nobody; let a person's write carry somebody —
// jane's write is made for ada.
func TestAnAgentsWriteStatesWhomItWasMadeFor(t *testing.T) {
	t.Parallel()
	ada := &types.WorkingFor{Kind: types.WorkingForSeat, Seat: "ada"}
	for name, c := range map[string]struct {
		kind       iam.ActorKind
		writtenFor types.WrittenFor
		want       *types.WorkingFor
		cause      error
	}{
		"an agent that states nothing":         {iam.ActorAgent, types.WrittenFor{}, nil, types.ErrWrittenForUnstated},
		"an agent in a turn nobody woke":       {iam.ActorAgent, types.ForTurn(nil), nil, nil},
		"an agent in a turn ada woke":          {iam.ActorAgent, types.ForTurn(ada), ada, nil},
		"the engine's own write as an agent":   {iam.ActorAgent, types.ForNobody(), nil, nil},
		"a person, stating nothing":            {iam.ActorHuman, types.WrittenFor{}, nil, nil},
		"a person handed a carrier":            {iam.ActorHuman, types.ForTurn(ada), nil, types.ErrWrittenForNotAnAgents},
		"a credential, stating nobody":         {iam.ActorOperator, types.ForNobody(), nil, nil},
		"a credential handed a carrier":        {iam.ActorOperator, types.ForTurn(ada), nil, types.ErrWrittenForNotAnAgents},
		"the engine, stating nothing":          {iam.ActorSystem, types.WrittenFor{}, nil, nil},
		"a kind this build does not know, too": {"guest", types.ForTurn(ada), nil, types.ErrWrittenForNotAnAgents},
	} {
		got, err := c.writtenFor.For(c.kind)
		if !errors.Is(err, c.cause) || (c.cause == nil && err != nil) || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: written for %+v (%v), want %+v (%v)", name, got, err, c.want, c.cause)
		}
	}

	// THE CARRIER IS THE ACTOR'S OWN COPY: a turn's carrier mended after the
	// actor was built does not move what its write carries.
	carried := &types.WorkingFor{Kind: types.WorkingForCredential, Login: "token:ci",
		Apps: []iam.Grant{iam.GrantWorklet}}
	stated := types.ForTurn(carried)
	carried.Apps[0] = iam.GrantAdmin
	if got := stated.Carrier(); got.Apps[0] != iam.GrantWorklet {
		t.Errorf("the actor's carrier moved with its builder's: %+v", got)
	}
}

// WHOM A WRITE WAS MADE FOR CROSSES THE WIRE AS ITS BUILDER STATED IT. An actor
// carrying nobody goes out without the key, byte for byte as a build without
// the field sends it; one carrying somebody goes out with them. On the far
// side what arrived is stated: a carrier as itself, and an absent one as
// nobody once the actor's own decode says it arrived ([types.WrittenFor.Received]),
// which is also what an older node, knowing no carrier, sends.
//
// Mutation: drop `stated` from UnmarshalJSON — a carrier that crossed the wire
// is refused as unstated; have IsZero answer the zero value alone — an actor
// stated for nobody puts a null key on the wire.
func TestWhomAWriteWasMadeForCrossesTheWireAsStated(t *testing.T) {
	t.Parallel()
	type actor struct {
		Handle     string
		WrittenFor types.WrittenFor `json:",omitzero"`
	}
	ada := &types.WorkingFor{Kind: types.WorkingForSeat, Seat: "ada"}
	for name, c := range map[string]struct {
		sent types.WrittenFor
		wire string
	}{
		"nobody, in a turn nobody woke": {types.ForTurn(nil), `{"Handle":"dev"}`},
		"nobody, as the engine":         {types.ForNobody(), `{"Handle":"dev"}`},
		"ada":                           {types.ForTurn(ada), `{"Handle":"dev","WrittenFor":{"kind":"seat","seat":"ada"}}`},
	} {
		b, err := json.Marshal(actor{Handle: "dev", WrittenFor: c.sent})
		if err != nil || string(b) != c.wire {
			t.Errorf("%s goes out as %s (%v), want %s", name, b, err, c.wire)
			continue
		}
		var got actor
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("%s: decode: %v", name, err)
		}
		received := got.WrittenFor.Received()
		carrier, err := received.For(iam.ActorAgent)
		if err != nil || !reflect.DeepEqual(carrier, c.sent.Carrier()) {
			t.Errorf("%s arrives written for %+v (%v), want %+v", name, carrier, err, c.sent.Carrier())
		}
	}
	// A NULL IS NOBODY, AND STATED.
	var null types.WrittenFor
	if err := json.Unmarshal([]byte(`null`), &null); err != nil || !null.Stated() || null.Carrier() != nil {
		t.Errorf("a null carrier decoded as %+v (%v), want nobody, stated", null, err)
	}
}
