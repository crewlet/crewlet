package estate

import (
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/rendezvous"
	"github.com/crewlet/crewlet/internal/worklet"
)

// A TRACKER ACTOR CROSSES THE WIRE AS IT WAS STATED (ADR-0109), under the
// wire's rule for what an operation takes (crewlet#910), inside itself as
// anywhere else. The actor a tracker write crosses in carries Worklet's
// provenance beside its kind: an agent's actor stated for nobody goes out
// with an empty provenance (being stated is no zero, and nothing of it
// crosses), and one stated for somebody carries them; decoded as the serving
// node decodes a request's inputs, each arrives stated, as it was sent, and a
// member a newer build added, beside the carrier or inside it, is named where
// it is rather than dropped.
//
// Mutation: give the actor a MarshalJSON over encoding/json, and the actor
// stated for nobody goes out with every zero field of its provenance; decode
// the carrier with an encoding/json of its own, and the member inside it is
// not named.
func TestATrackerActorCrossesTheWireAsItWasStated(t *testing.T) {
	t.Parallel()
	ada := &types.WorkingFor{Kind: types.WorkingForSeat, Seat: "ada"}
	for name, c := range map[string]struct {
		writtenFor types.WrittenFor
		wire       string
	}{
		"an agent stated for nobody": {types.ForTurn(nil), `{"handle":"swe","kind":"agent","provenance":{}}`},
		"an agent stated for ada": {types.ForTurn(ada),
			`{"handle":"swe","kind":"agent","provenance":{"WrittenFor":{"kind":"seat","seat":"ada"}}}`},
	} {
		raw, err := encodeInput(Actor{Handle: "swe", Kind: worklet.AuthorAgent,
			Provenance: worklet.Provenance{WrittenFor: c.writtenFor}})
		if err != nil || string(raw) != c.wire {
			t.Errorf("%s goes out as %s (%v), want %s", name, raw, err, c.wire)
			continue
		}
		got, unknown := arriving(t, raw)
		if len(unknown) != 0 || !got.Provenance.WrittenFor.Stated() ||
			!reflect.DeepEqual(got.Provenance.WrittenFor.Carrier(), c.writtenFor.Carrier()) {
			t.Errorf("%s arrives naming %q, written for %+v (stated: %v), want %+v, stated", name, unknown,
				got.Provenance.WrittenFor.Carrier(), got.Provenance.WrittenFor.Stated(), c.writtenFor.Carrier())
		}
	}

	for _, c := range []struct{ raw, key string }{
		{`{"handle":"swe","kind":"agent","provenance":{"later":1}}`, "actor.provenance.later"},
		{`{"handle":"swe","kind":"agent","provenance":{"WrittenFor":{"kind":"seat","seat":"ada","later":1}}}`,
			"actor.provenance.WrittenFor.later"},
	} {
		if _, unknown := arriving(t, json.RawMessage(c.raw)); !slices.Equal(unknown, []string{c.key}) {
			t.Errorf("%s arrives naming %q, want %s", c.raw, unknown, c.key)
		}
	}
}

// arriving is the actor raw is as a serving node decodes a request's inputs
// ([inputs]), or every member of it that node's build does not declare, each
// by its path.
func arriving(t *testing.T, raw json.RawMessage) (*Actor, []string) {
	t.Helper()
	_, actor, refused, err := inputs(opCreateTask.spec, request{Op: opCreateTask.spec.name, Actor: raw,
		Args: json.RawMessage(`{"OpID":"op-1","Task":{"Project":"ENG"}}`)})
	switch {
	case err != nil:
		t.Fatalf("the serving node does not decode %s: %v", raw, err)
	case refused != nil:
		return &Actor{}, refused.Keys
	}
	return actor, nil
}

// AN ACTOR THAT CANNOT WRITE IS REFUSED BEFORE ANYTHING IS ASKED (ADR-0109), on
// a data node answering its own seat's write from its own copy as much as on
// one that routes it, for the tracker, the knowledge base and chat alike: an
// agent's actor that states nobody it writes for, and a person's handed a
// carrier, may write nothing ([types.WrittenFor.For]). A node the write is
// routed to reads an absent carrier as stated nobody, and would run an agent's
// write that states nothing as the seat's own work; so the router refuses such
// an actor before it asks anything, in process as routed ([actorWrites]), and
// one answer comes from every node. Nothing runs on the node's own copy and no
// request leaves it, and tracker.view_prior, a read that runs as the actor, is
// held to the same. The same calls with their actors stated run in process.
//
// Mutation: drop the actor's check from callAs, and the tracker's write and
// its prior read run on the node's own copy; have pageActorWrites or
// chatActorWrites answer nil, and that family's write runs there.
func TestAnActorThatCannotWriteIsRefusedBeforeAnythingIsAsked(t *testing.T) {
	t.Parallel()
	ada := &types.WorkingFor{Kind: types.WorkingForSeat, Seat: "ada"}
	for name, c := range map[string]struct {
		agent           bool
		refused, stated types.WrittenFor
		cause           error
	}{
		"an agent that states nothing": {true, types.WrittenFor{}, types.ForTurn(ada),
			types.ErrWrittenForUnstated},
		"a person handed a carrier": {false, types.ForTurn(ada), types.WrittenFor{},
			types.ErrWrittenForNotAnAgents},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFleet(t, "data-a")
			node, desk := f.nodes["data-a"], withRooms(f)["data-a"]
			here := f.router(t, "data-a", node)
			ctx := t.Context()
			calls := func(writtenFor types.WrittenFor) map[string]error {
				tracker := Actor{Handle: "swe", Kind: worklet.AuthorAgent,
					Provenance: worklet.Provenance{WrittenFor: writtenFor}}
				page := doclet.Actor{Handle: "swe", Kind: doclet.AuthorAgent, WrittenFor: writtenFor}
				room := chatlet.Actor{Handle: "swe", Kind: chatlet.AuthorAgent, WrittenFor: writtenFor}
				if !c.agent {
					tracker.Handle, tracker.Kind = "maya", worklet.AuthorHuman
					page.Handle, page.Kind = "maya", doclet.AuthorHuman
					room.Handle, room.Kind = "maya", chatlet.AuthorHuman
				}
				_, createErr := here.WriterAs(tracker).CreateTask(ctx, "op-1", worklet.Task{Project: "ENG"}, nil)
				_, priorErr := here.WriterAs(tracker).ViewPrior(ctx, "v-1")
				_, _, pageErr := here.Pages().Comment(ctx, page, "p1", doclet.NewComment{Body: "is this current?"})
				_, roomErr := here.Chat().Post(ctx, room, "room-1", chatlet.NewMessage{Body: "is this current?"})
				return map[string]error{"tracker": createErr, "tracker's prior read": priorErr,
					"pages": pageErr, "chat": roomErr}
			}
			ran := func() []string {
				var families []string
				for _, op := range []string{"create", "view_prior", "comment"} {
					if node.askedFor(op) {
						families = append(families, op)
					}
				}
				if len(desk.asked()) != 0 {
					families = append(families, "post")
				}
				return families
			}

			for family, err := range calls(c.refused) {
				if !errors.Is(err, c.cause) {
					t.Errorf("the %s call answered %v, want it refused for %v", family, err, c.cause)
				}
				if errors.As(err, new(*ErrUnserved)) || errors.Is(err, ErrOutcomeUnknown) {
					t.Errorf("the %s call answered %v, which says no data node served it or that it "+
						"may have run: it was never asked", family, err)
				}
			}
			node.mu.Lock()
			actors, sent := len(node.actors), node.requests
			node.mu.Unlock()
			if got := ran(); len(got) != 0 || actors != 0 || sent != 0 {
				t.Fatalf("the data node ran %v as %d actors and was sent %d requests, want nothing asked",
					got, actors, sent)
			}

			// CONTROL: the same calls, their actors stated, run on the node's
			// own copy, and nothing crosses the wire.
			for family, err := range calls(c.stated) {
				if err != nil {
					t.Errorf("the %s call of a stated actor answered %v, want it run", family, err)
				}
			}
			node.mu.Lock()
			sent = node.requests
			node.mu.Unlock()
			if got := ran(); !slices.Equal(got, []string{"create", "view_prior", "comment", "post"}) || sent != 0 {
				t.Errorf("the stated calls ran %v here and sent %d requests, want every one run in process",
					got, sent)
			}
		})
	}
}

// AN AGENT'S WRITE FROM A BUILD WITHOUT THE CARRIER RUNS AS ITS SEAT'S OWN
// (ADR-0109's roll). A node of the build before the carrier, with the wire's
// rule for what an operation takes (crewlet#910), leaves an agent's provenance
// that carries nothing off the request whole: the merge queue's close of a
// landed change's item is one such write. So the serving node states whom an
// actor's writes are for whether or not a provenance arrived, and an agent's
// write that arrives with none runs as the seat's own work, never refused for
// a key its sender left out because it carried nobody. (Its run on a real
// tracker is internal/engine's
// TestAnAgentsTrackerWriteFromABuildWithoutTheCarrierRunsForNobody.)
//
// Mutation: state whom the writes are for only where a provenance arrives (in
// a decode of worklet.Provenance rather than of the actor), and this write
// arrives stating nothing, which the tracker's writer refuses for good.
func TestAnAgentsWriteFromABuildWithoutTheCarrierRunsAsItsSeatsOwn(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	node := f.nodes["data-a"]
	older := request{Op: opCreateTask.spec.name, Actor: json.RawMessage(`{"handle":"swe","kind":"agent"}`),
		Args: json.RawMessage(`{"OpID":"op-1","Task":{"Project":"ENG"}}`)}
	if rep := askSigned(t, f.start(t), "data-a", older); rep.Err != nil || rep.Unserved != "" {
		t.Fatalf("the write was answered %+v, want it run", rep)
	}
	node.mu.Lock()
	wrote := slices.Clone(node.actors)
	node.mu.Unlock()
	if len(wrote) != 1 {
		t.Fatalf("the tracker was asked to write as %d actors, want one", len(wrote))
	}
	if carrier, err := wrote[0].Provenance.WrittenFor.For(iam.ActorAgent); err != nil || carrier != nil {
		t.Errorf("the write runs written for %+v (%v), want stated for nobody: the seat's own work",
			carrier, err)
	}
}

// AN UNKNOWN MEMBER INSIDE A ROUTED ACTOR IS REFUSED AT ITS PATH (crewlet#910's
// rule, held inside ADR-0109's actors). Each actor decodes itself, to state
// whom its writes are for, and does so through the serving node's own decoder,
// so the rule refuses a member a newer build added inside it as it refuses one
// anywhere else in what an operation takes: the serving node refuses the
// request, naming the member where it is (beside the tracker's carrier, inside
// it, in the actor of a page write's or a chat gesture's arguments, inside
// that actor's carrier), and runs nothing of it. An actor that decoded with an
// encoding/json of its own dropped every such member without a word, and the
// write ran without it.
//
// Mutation: give doclet.Actor back an UnmarshalJSON over encoding/json, and
// the page writes run with their member dropped; decode the carrier with
// json.Unmarshal, and the members inside the carriers are dropped.
func TestAnUnknownMemberInsideARoutedActorIsRefusedAtItsPath(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	node, desk := f.nodes["data-a"], withRooms(f)["data-a"]
	q := f.start(t)
	task := json.RawMessage(`{"OpID":"op-1","Task":{"Project":"ENG"}}`)
	for name, c := range map[string]struct {
		req request
		key string
	}{
		"beside the tracker's carrier": {request{Op: opCreateTask.spec.name, Args: task,
			Actor: json.RawMessage(`{"handle":"swe","kind":"agent","provenance":{"later":1}}`)},
			"actor.provenance.later"},
		"inside the tracker's carrier": {request{Op: opCreateTask.spec.name, Args: task,
			Actor: json.RawMessage(`{"handle":"swe","kind":"agent",` +
				`"provenance":{"WrittenFor":{"kind":"seat","seat":"ada","later":1}}}`)},
			"actor.provenance.WrittenFor.later"},
		"in a page comment's actor": {request{Op: opCommentPage.spec.name,
			Args: json.RawMessage(`{"Actor":{"Handle":"swe","Kind":"agent","Later":1},` +
				`"PageID":"p1","Comment":{"Body":"is this current?"}}`)},
			"args.Actor.Later"},
		"inside a page comment's carrier": {request{Op: opCommentPage.spec.name,
			Args: json.RawMessage(`{"Actor":{"Handle":"swe","Kind":"agent",` +
				`"WrittenFor":{"kind":"seat","seat":"ada","later":1}},` +
				`"PageID":"p1","Comment":{"Body":"is this current?"}}`)},
			"args.Actor.WrittenFor.later"},
		"in a chat post's actor": {request{Op: opChatPost.spec.name,
			Args: json.RawMessage(`{"Actor":{"Handle":"jane","Kind":"agent","Later":1},` +
				`"ChannelID":"room-1","Message":{"Body":"is this current?"}}`)},
			"args.Actor.Later"},
		"inside a chat post's carrier": {request{Op: opChatPost.spec.name,
			Args: json.RawMessage(`{"Actor":{"Handle":"jane","Kind":"agent",` +
				`"WrittenFor":{"kind":"seat","seat":"ada","later":1}},` +
				`"ChannelID":"room-1","Message":{"Body":"is this current?"}}`)},
			"args.Actor.WrittenFor.later"},
	} {
		rep := askSigned(t, q, "data-a", c.req)
		var refused *ErrUnknownKeys
		if err := decodeError(rep.Err); !errors.As(err, &refused) ||
			!slices.Equal(refused.Keys, []string{c.key}) || !slices.Equal(refused.Nodes, []string{"data-a"}) {
			t.Errorf("%s was answered %v, want refused by data-a naming %s", name, err, c.key)
		}
	}
	if node.askedFor("create") || node.askedFor("comment") || len(desk.asked()) != 0 {
		t.Fatalf("the data node ran writes holding members its build does not declare: tracker %v, "+
			"pages %v, chat %v", node.askedFor("create"), node.askedFor("comment"), desk.asked())
	}
}

// A WRITE FOR SOMEBODY MOVES OFF A DATA NODE WHOSE BUILD HAS NO CARRIER
// (ADR-0109's roll). A data node of the build before the carrier, with the
// wire's rule for what an operation takes (crewlet#910), does not declare it,
// so it refuses a write that carries one, naming it, and runs nothing of it:
// the asking router moves on to a data node of this build, where the write
// runs for whom it was made. A write that carries nobody sends no key for it
// and runs on the older node as it always did. And where no data node
// declares the carrier, the caller is told that nothing ran and a data node of
// the writer's build is needed, never that the write ran for nobody: so the
// data nodes are upgraded first.
//
// Mutation: have WrittenFor.IsZero answer the zero value alone, and the write
// for nobody carries a key the older node refuses; drop the refusal's arm from
// Router.settle, and the write for ada fails on the older node.
func TestAWriteForSomebodyMovesOffADataNodeWithoutTheCarrier(t *testing.T) {
	t.Parallel()
	ada := &types.WorkingFor{Kind: types.WorkingForSeat, Seat: "ada"}
	// THE BUILD BEFORE THE CARRIER, as its refusal names what it does not
	// declare: inside the tracker's actor, and inside the actor a page
	// write's or a chat gesture's arguments hold.
	older := func(n *fakeNode) {
		n.undeclared = []string{"actor.provenance.WrittenFor", "args.Actor.WrittenFor"}
	}
	keys := map[string]string{"tracker": "actor.provenance.WrittenFor", "pages": "args.Actor.WrittenFor",
		"chat": "args.Actor.WrittenFor"}
	every := []string{"tracker", "pages", "chat"}
	writes := func(t *testing.T, f *fleet, writtenFor types.WrittenFor) map[string]error {
		t.Helper()
		ctx := t.Context()
		_, trackerErr := f.client.WriterAs(Actor{Handle: "swe", Kind: worklet.AuthorAgent,
			Provenance: worklet.Provenance{WrittenFor: writtenFor}}).CreateTask(ctx, "op-1",
			worklet.Task{Project: "ENG"}, nil)
		_, _, pageErr := f.client.Pages().Comment(ctx, doclet.Actor{Handle: "swe", Kind: doclet.AuthorAgent,
			WrittenFor: writtenFor}, "p1", doclet.NewComment{Body: "is this current?"})
		_, roomErr := f.client.Chat().Post(ctx, chatlet.Actor{Handle: "jane", Kind: chatlet.AuthorAgent,
			WrittenFor: writtenFor}, "room-1", chatlet.NewMessage{Body: "is this current?"})
		return map[string]error{"tracker": trackerErr, "pages": pageErr, "chat": roomErr}
	}
	ran := func(n *fakeNode, desk *chatDesk) []string {
		var families []string
		if n.askedFor("create") {
			families = append(families, "tracker")
		}
		if n.askedFor("comment") {
			families = append(families, "pages")
		}
		if slices.Contains(desk.asked(), "post jane") {
			families = append(families, "chat")
		}
		return families
	}

	t.Run("a write for nobody runs there", func(t *testing.T) {
		t.Parallel()
		f := newFleet(t, "data-a", "data-b")
		desks := withRooms(f)
		_, first := f.first(t, f.client)
		first.set(older)
		for family, err := range writes(t, f, types.ForNobody()) {
			if err != nil {
				t.Errorf("the %s write for nobody answered %v, want it run", family, err)
			}
		}
		second := f.other(first)
		if got := ran(first, desks[first.name]); !slices.Equal(got, every) {
			t.Errorf("the node of the older build ran %v, want every write for nobody", got)
		}
		if got := ran(second, desks[second.name]); len(got) != 0 {
			t.Errorf("the writes for nobody moved off the older build to %s, which ran %v", second.name, got)
		}
	})

	t.Run("a write for ada runs on a node of this build", func(t *testing.T) {
		t.Parallel()
		f := newFleet(t, "data-a", "data-b")
		desks := withRooms(f)
		_, first := f.first(t, f.client)
		first.set(older)
		for family, err := range writes(t, f, types.ForTurn(ada)) {
			if err != nil {
				t.Errorf("the %s write for ada answered %v, want it run on a node of this build", family, err)
			}
		}
		second := f.other(first)
		if got := ran(first, desks[first.name]); len(got) != 0 {
			t.Errorf("the node of the older build ran %v of the writes for ada, without their carrier", got)
		}
		if got := ran(second, desks[second.name]); !slices.Equal(got, every) {
			t.Errorf("the node of this build ran %v, want every write for ada", got)
		}
		second.mu.Lock()
		wrote := slices.Clone(second.actors)
		second.mu.Unlock()
		if len(wrote) != 1 || !reflect.DeepEqual(wrote[0].Provenance.WrittenFor.Carrier(), ada) {
			t.Errorf("the tracker write ran as %+v, want one actor written for ada", wrote)
		}
	})

	t.Run("no node of this build", func(t *testing.T) {
		t.Parallel()
		f := newFleet(t, "data-a", "data-b")
		desks := withRooms(f)
		for _, node := range f.nodes {
			node.set(older)
		}
		for family, err := range writes(t, f, types.ForTurn(ada)) {
			var refused *ErrUnknownKeys
			if !errors.As(err, &refused) || !slices.Equal(refused.Keys, []string{keys[family]}) ||
				len(refused.Nodes) != len(f.nodes) {
				t.Errorf("the %s write for ada answered %v, want refused by every node, naming %s",
					family, err, keys[family])
				continue
			}
			if !errors.As(err, new(*ErrUnserved)) || errors.Is(err, ErrOutcomeUnknown) {
				t.Errorf("the %s write for ada answered %v, want an estate nobody served, in which "+
					"nothing ran", family, err)
			}
		}
		for name, node := range f.nodes {
			if got := ran(node, desks[name]); len(got) != 0 {
				t.Errorf("%s, of the older build, ran %v of the writes for ada", name, got)
			}
		}
	})
}

// A WRITE WHOSE ACTOR CANNOT WRITE IS REFUSED BEFORE ANY PEER RUNS IT
// (ADR-0109). This router holds no data, so every write it makes is decided on
// another data node, which reads an absent carrier as nobody and would run an
// agent's write that states nothing as the writing seat's own work. So the
// node that built the actor refuses it before asking anything ([actorWrites]),
// for the tracker, the knowledge base and chat alike: no request leaves it,
// and the caller is told why rather than that no data node served it or that
// its write may have run. The same writes with their actors stated run there,
// each arriving as it was stated.
//
// Mutation: drop the actor's check from callAs, and the tracker's write and
// its prior read run on the data node; have pageActorWrites or
// chatActorWrites answer nil, and that family's write runs there.
func TestAWriteWhoseActorCannotWriteIsRefusedBeforeAnyPeerRunsIt(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	node, desk := f.nodes["data-a"], withRooms(f)["data-a"]
	ctx := t.Context()
	ada := &types.WorkingFor{Kind: types.WorkingForSeat, Seat: "ada"}
	writes := func(tracker Actor, page doclet.Actor, room chatlet.Actor) map[string]error {
		_, trackerErr := f.client.WriterAs(tracker).CreateTask(ctx, "op-"+tracker.Handle,
			worklet.Task{Project: "ENG"}, nil)
		_, priorErr := f.client.WriterAs(tracker).ViewPrior(ctx, "v-1")
		_, _, pageErr := f.client.Pages().Comment(ctx, page, "p1", doclet.NewComment{Body: "is this current?"})
		_, roomErr := f.client.Chat().Post(ctx, room, "room-1", chatlet.NewMessage{Body: "is this current?"})
		return map[string]error{"tracker": trackerErr, "tracker's prior read": priorErr,
			"pages": pageErr, "chat": roomErr}
	}

	refused := writes(Actor{Handle: "swe", Kind: worklet.AuthorAgent},
		doclet.Actor{Handle: "swe", Kind: doclet.AuthorAgent},
		chatlet.Actor{Handle: "jane", Kind: chatlet.AuthorAgent})
	for family, err := range refused {
		if !errors.Is(err, types.ErrWrittenForUnstated) {
			t.Errorf("the %s call of an agent that states nothing answered %v, want it refused "+
				"for stating nobody", family, err)
		}
		if errors.As(err, new(*ErrUnserved)) || errors.Is(err, ErrOutcomeUnknown) {
			t.Errorf("the %s call of an agent that states nothing answered %v, which says no data "+
				"node served it or that it may have run: it was never sent", family, err)
		}
	}
	if _, _, err := f.client.Pages().Comment(ctx, doclet.Actor{Handle: "jane", Kind: doclet.AuthorHuman,
		WrittenFor: types.ForTurn(ada)}, "p1", doclet.NewComment{Body: "for ada"}); !errors.Is(err,
		types.ErrWrittenForNotAnAgents) {
		t.Errorf("a person's page write handed a carrier answered %v, want it refused", err)
	}
	node.mu.Lock()
	sent := node.requests
	node.mu.Unlock()
	if sent != 0 {
		t.Errorf("the data node was sent %d requests no actor may make, want none", sent)
	}
	if node.askedFor("create") || node.askedFor("view_prior") || node.askedFor("comment") ||
		len(desk.asked()) != 0 {
		t.Fatalf("the data node ran calls no actor may make: tracker %v, prior read %v, pages %v, chat %v",
			node.askedFor("create"), node.askedFor("view_prior"), node.askedFor("comment"), desk.asked())
	}

	// CONTROL: the same calls, their actors stated, run on the data node.
	ran := writes(Actor{Handle: "swe", Kind: worklet.AuthorAgent,
		Provenance: worklet.Provenance{WrittenFor: types.ForTurn(ada)}},
		doclet.Actor{Handle: "swe", Kind: doclet.AuthorAgent, WrittenFor: types.ForTurn(nil)},
		chatlet.Actor{Handle: "jane", Kind: chatlet.AuthorAgent, WrittenFor: types.ForTurn(ada)})
	for family, err := range ran {
		if err != nil {
			t.Errorf("the %s call of a stated agent answered %v, want it run", family, err)
		}
	}
	if !node.askedFor("create") || !node.askedFor("view_prior") || !node.askedFor("comment") ||
		!slices.Contains(desk.asked(), "post jane") {
		t.Errorf("the data node did not run the stated calls: tracker %v, prior read %v, pages %v, chat %v",
			node.askedFor("create"), node.askedFor("view_prior"), node.askedFor("comment"), desk.asked())
	}
	node.mu.Lock()
	wrote := slices.Clone(node.actors)
	node.mu.Unlock()
	if len(wrote) != 2 || !wrote[0].Provenance.WrittenFor.Stated() ||
		!reflect.DeepEqual(wrote[0].Provenance.WrittenFor.Carrier(), ada) {
		t.Errorf("the tracker calls ran as %+v, want two actors, the first stated, written for ada", wrote)
	}
}

// askedWhateverItsActor is every operation whose arguments hold a page or chat
// actor and which is asked whatever that actor may write, and why that is
// right.
//
// TWO-SIDED, like [carriedByServer]: an operation listed that is no longer
// declared, or whose arguments no longer hold such an actor, fails too.
var askedWhateverItsActor = map[string]string{
	"chat.reach": "a read: a post's dry run decides on the serving node's snapshot and publishes " +
		"nothing, so an actor that may write nothing writes nothing through it either",
}

var (
	pageActorType = reflect.TypeFor[doclet.Actor]()
	chatActorType = reflect.TypeFor[chatlet.Actor]()
)

// holds reports whether a value of ty holds a target at any depth the encoder
// reaches one: through a pointer, a slice, an array, a map's values and every
// field it carries.
func holds(ty, target reflect.Type, seen map[reflect.Type]bool) bool {
	if ty == target {
		return true
	}
	if seen[ty] {
		return false
	}
	seen[ty] = true
	switch ty.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Map:
		return holds(ty.Elem(), target, seen)
	case reflect.Struct:
		for field := range ty.Fields() {
			if (field.IsExported() || field.Anonymous) && holds(field.Type, target, seen) {
				return true
			}
		}
	}
	return false
}

// holdsAnActor reports whether a value of ty holds a page or a chat actor.
func holdsAnActor(ty reflect.Type) bool {
	return holds(ty, pageActorType, map[reflect.Type]bool{}) || holds(ty, chatActorType, map[reflect.Type]bool{})
}

// actorsIn sets every page or chat actor v holds to an agent's actor whose
// writes are for writtenFor, allocating each pointer on the way to one, and
// answers the path of each it set, and of each it cannot reach: one in a list,
// an array or a map, which a value this builds holds none of, and one in a
// field reflection cannot set.
func actorsIn(v reflect.Value, path string, writtenFor types.WrittenFor) (set, unreached []string) {
	switch v.Type() {
	case pageActorType:
		v.Set(reflect.ValueOf(doclet.Actor{Handle: "swe", Kind: doclet.AuthorAgent, WrittenFor: writtenFor}))
		return []string{path}, nil
	case chatActorType:
		v.Set(reflect.ValueOf(chatlet.Actor{Handle: "swe", Kind: chatlet.AuthorAgent, WrittenFor: writtenFor}))
		return []string{path}, nil
	}
	switch v.Kind() {
	case reflect.Pointer:
		if holdsAnActor(v.Type().Elem()) {
			v.Set(reflect.New(v.Type().Elem()))
			return actorsIn(v.Elem(), path, writtenFor)
		}
	case reflect.Slice, reflect.Array, reflect.Map:
		if holdsAnActor(v.Type().Elem()) {
			return nil, []string{path + "[]"}
		}
	case reflect.Struct:
		for i, field := range slices.Collect(v.Type().Fields()) {
			if !holdsAnActor(field.Type) {
				continue
			}
			if !v.Field(i).CanSet() {
				unreached = append(unreached, path+"."+field.Name)
				continue
			}
			s, u := actorsIn(v.Field(i), path+"."+field.Name, writtenFor)
			set, unreached = append(set, s...), append(unreached, u...)
		}
	}
	return set, unreached
}

// EVERY OPERATION WHOSE ARGUMENTS HOLD A PAGE OR CHAT ACTOR REFUSES ONE THAT
// CANNOT WRITE BEFORE IT IS ASKED (ADR-0109). Such an operation runs as the
// actor its arguments hold, which the router never sees as the actor an
// operation runs as ([callAs] holds only that one), so each declares the
// refusal itself ([op.refusedBeforeAsking] over [actorWrites]). Found from the
// registry, at any depth an actor can be held, and asked of each declaration
// with its arguments built around an agent's actor that states nothing —
// refused for it — and around one that is stated, which is not: so a new page
// or chat write cannot be declared without the refusal, nor a refusal pass for
// something other than the actor. A read that holds an actor and writes
// nothing is named with why ([askedWhateverItsActor]). And a walk that finds no
// holder of either actor fails, rather than hold nothing to anything.
//
// Mutation: drop refusedBeforeAsking from any page or chat write, and the gate
// names it; have the walk find no actor, and the gate fails.
func TestEveryOperationHoldingAPageOrChatActorRefusesOneThatCannotWrite(t *testing.T) {
	t.Parallel()
	found := map[reflect.Type][]string{}
	for _, name := range slices.Sorted(maps.Keys(registry)) {
		spec := registry[name]
		_, listed := askedWhateverItsActor[name]
		held := false
		for _, ty := range []reflect.Type{pageActorType, chatActorType} {
			if holds(spec.args, ty, map[reflect.Type]bool{}) {
				found[ty], held = append(found[ty], name), true
			}
		}
		switch {
		case !held && listed:
			t.Errorf("askedWhateverItsActor lists %s, whose arguments hold no page or chat actor", name)
			continue
		case !held:
			continue
		}
		unstated := reflect.New(spec.args).Elem()
		at, unreached := actorsIn(unstated, "args", types.WrittenFor{})
		switch {
		case len(unreached) > 0:
			t.Errorf("%s holds a page or chat actor at %v, where this gate builds none: teach it to", name, unreached)
			continue
		case listed:
			continue
		case spec.asked == nil:
			t.Errorf("%s holds an actor at %v and declares no refusal before it is asked: declare "+
				"refusedBeforeAsking over actorWrites, or name it in askedWhateverItsActor with why", name, at)
			continue
		}
		if err := spec.asked(unstated.Interface()); !errors.Is(err, types.ErrWrittenForUnstated) {
			t.Errorf("%s, asked with an agent's actor that states nothing at %v, answered %v; want it refused "+
				"for stating nobody", name, at, err)
		}
		stated := reflect.New(spec.args).Elem()
		actorsIn(stated, "args", types.ForTurn(nil))
		if err := spec.asked(stated.Interface()); errors.Is(err, types.ErrWrittenForUnstated) {
			t.Errorf("%s, asked with a stated agent's actor at %v, answered %v; want its actor admitted",
				name, at, err)
		}
	}
	for _, ty := range []reflect.Type{pageActorType, chatActorType} {
		if len(found[ty]) == 0 {
			t.Errorf("no operation's arguments hold a %s: the walk found nothing, and holds nothing to anything", ty)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(askedWhateverItsActor)) {
		if _, declared := registry[name]; !declared {
			t.Errorf("askedWhateverItsActor lists %s, which no operation is declared as", name)
		}
	}
}

// EVERY CODE WRITE REFUSES AN AGENT'S REQUEST THAT STATES NOBODY, BEFORE IT IS
// ASKED (ADR-0109). A code operation runs as no actor: its asker travels inside
// its request, beside whom the write is made for, so a serving node reads a
// carrier that never arrived as stated nobody — whichever build left it off —
// and would decide an agent's write that states nothing as the seat's own
// work. So each operation whose request holds an asker and a carrier declares
// the refusal itself ([op.refusedBeforeAsking] over [codeWrites]). Found from
// the registry — every operation whose arguments hold a gitlet.Principal and a
// types.WrittenFor — each is asked with its request built around an agent
// asker that states nothing, which it refuses, and around one stated for
// nobody, which it admits. A walk that finds fewer than the six code writes
// fails, rather than hold nothing to anything; one holding the two deeper than
// the request itself is named, for this gate to be taught.
//
// Mutation: drop refusedBeforeAsking from code.conversation, and the gate
// names it; have the walk find no request, and the gate fails.
func TestEveryCodeWriteRefusesAnAgentsRequestThatStatesNobody(t *testing.T) {
	t.Parallel()
	principal, carrier := reflect.TypeFor[gitlet.Principal](), reflect.TypeFor[types.WrittenFor]()
	agent := gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: "swe"}
	var found []string
	for _, name := range slices.Sorted(maps.Keys(registry)) {
		spec := registry[name]
		if !holds(spec.args, principal, map[reflect.Type]bool{}) || !holds(spec.args, carrier, map[reflect.Type]bool{}) {
			continue
		}
		var askers, carriers []int
		if spec.args.Kind() == reflect.Struct {
			for i, field := range slices.Collect(spec.args.Fields()) {
				switch field.Type {
				case principal:
					askers = append(askers, i)
				case carrier:
					carriers = append(carriers, i)
				}
			}
		}
		if len(askers) != 1 || len(carriers) != 1 {
			t.Errorf("%s holds an asker and whom its write is made for beyond its request's own fields, where "+
				"this gate builds neither: teach it to", name)
			continue
		}
		found = append(found, name)
		if spec.asked == nil {
			t.Errorf("%s holds an asker and whom its write is made for and declares no refusal before it is "+
				"asked: declare refusedBeforeAsking over codeWrites", name)
			continue
		}
		request := func(writtenFor types.WrittenFor) any {
			v := reflect.New(spec.args).Elem()
			v.Field(askers[0]).Set(reflect.ValueOf(agent))
			v.Field(carriers[0]).Set(reflect.ValueOf(writtenFor))
			return v.Interface()
		}
		if err := spec.asked(request(types.WrittenFor{})); !errors.Is(err, types.ErrWrittenForUnstated) {
			t.Errorf("%s, asked with an agent's request that states nothing, answered %v; want it refused for "+
				"stating nobody", name, err)
		}
		if err := spec.asked(request(types.ForNobody())); err != nil {
			t.Errorf("%s, asked with an agent's request stated for nobody, answered %v; want it admitted", name, err)
		}
	}
	if len(found) < 6 {
		t.Fatalf("the walk found %d operations holding an asker and whom its write is made for (%v), want the six "+
			"code writes at least", len(found), found)
	}
}

// A CODE WRITE FOR SOMEBODY MOVES OFF A NODE WHOSE BUILD HAS NO CARRIER
// (ADR-0109's roll). A data node or a Gitlet node of the build before the
// carrier, with the wire's rule for what an operation takes (crewlet#910),
// does not declare whom a code write is made for, so it refuses a request
// carrying somebody, naming args.written_for, and runs nothing of it: the walk
// asks the repository's next node, of this build, where the write is decided
// for whom it was made. A request made for nobody sends no member, and runs on
// the older node as it always did. And a request from a seat of the older
// build, which sends its ceiling and no carrier at all, is served by a node of
// this build as stated nobody beside the ceiling it sent, which its decide
// holds it to as before the carrier. For a change's write and a push, asked
// of the repository's committing data nodes, and an engine write, asked of its
// Gitlet nodes.
//
// Mutation: have WrittenFor.IsZero answer the zero value alone, and the writes
// for nobody carry a key the older node refuses; have a request's decoder
// state nothing it received, and the older seat's writes arrive stating
// nothing.
func TestACodeWriteForSomebodyMovesOffANodeWithoutTheCarrier(t *testing.T) {
	t.Parallel()
	ada := &types.WorkingFor{Kind: types.WorkingForSeat, Seat: "ada"}
	older := func(n *fakeNode) { n.undeclared = []string{"args.written_for"} }
	seat := gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: "swe"}
	change := func(repo string, writtenFor types.WrittenFor) gitlet.ChangeProposal {
		return gitlet.ChangeProposal{OpID: "019a0c2e-0000-7000-8000-0000000000d0.code_close_change", Asker: seat,
			ExpectedVersion: 3, WrittenFor: writtenFor,
			Close: &gitlet.ChangeClose{RepoID: repo, ChangeID: "019a0c2e-0000-7000-8000-0000000000d1"}}
	}
	push := func(repo string, writtenFor types.WrittenFor) gitlet.Proposal {
		p := proposalTo(repo, "019a0c2e-0000-7000-8000-00000000a11d")
		p.Principal = gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: "swe",
			Run: gitlet.RunProvenance{TurnID: "019a0c2e-0000-7000-8000-00000000d00d"}}
		p.Via, p.WrittenFor = gitlet.ViaEngine, writtenFor
		return p
	}

	for _, tc := range []struct {
		name       string
		writtenFor types.WrittenFor
		// onOlder is whether the older node runs the writes.
		onOlder bool
	}{
		{"made for nobody, on the older node", types.ForTurn(nil), true},
		{"made for ada, on a node of this build", types.ForTurn(ada), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// A DATA NODE'S: a change's write and a push.
			holders := []string{"data-a", "data-b"}
			f := newFleet(t, holders...)
			const repo = "repo-written-for"
			o := rendezvous.Order(repo, holders)
			committer, runnerUp := f.nodes[o[0]], f.nodes[o[1]]
			committer.set(older)
			if _, err := f.client.Code().Change(t.Context(), change(repo, tc.writtenFor)); err != nil {
				t.Fatalf("the change's write %s: %v", tc.name, err)
			}
			if _, err := f.client.Code().Propose(t.Context(), push(repo, tc.writtenFor)); err != nil {
				t.Fatalf("the push %s: %v", tc.name, err)
			}
			ran, idle := runnerUp, committer
			if tc.onOlder {
				ran, idle = committer, runnerUp
			}
			idle.mu.Lock()
			idleChanges, idleProposals := len(idle.changes), len(idle.proposals)
			idle.mu.Unlock()
			ran.mu.Lock()
			changes, proposals := slices.Clone(ran.changes), slices.Clone(ran.proposals)
			ran.mu.Unlock()
			if idleChanges+idleProposals != 0 || len(changes) != 1 || len(proposals) != 1 {
				t.Fatalf("%s ran %d changes and %d pushes and %s ran %d and %d, want the one of each on %s alone",
					ran.name, len(changes), len(proposals), idle.name, idleChanges, idleProposals, ran.name)
			}
			for what, got := range map[string]types.WrittenFor{"change's write": changes[0].WrittenFor,
				"push": proposals[0].WrittenFor} {
				if !got.Stated() || !reflect.DeepEqual(got.Carrier(), tc.writtenFor.Carrier()) {
					t.Errorf("the %s ran made for %+v (stated: %v), want %+v", what, got.Carrier(), got.Stated(),
						tc.writtenFor.Carrier())
				}
			}

			// A GITLET NODE'S: an engine write.
			gits := []string{"git-a", "git-b"}
			halves := f.gitletNodes(t, gits)
			w := seatWriteOf(repoID(41))
			order := rendezvous.Order(w.RepoID, gits)
			f.nodes[order[0]].set(older)
			w.WrittenFor = tc.writtenFor
			if _, err := f.client.Code().GitWrite(t.Context(), w); err != nil {
				t.Fatalf("the engine write %s: %v", tc.name, err)
			}
			built, spared := halves[order[1]], halves[order[0]]
			if tc.onOlder {
				built, spared = halves[order[0]], halves[order[1]]
			}
			asked := built.writes()
			if len(spared.writes()) != 0 || len(asked) != 1 || !asked[0].w.WrittenFor.Stated() ||
				!reflect.DeepEqual(asked[0].w.WrittenFor.Carrier(), tc.writtenFor.Carrier()) {
				t.Errorf("the engine write %s was built %+v and %d times elsewhere, want once, by %s, made for %+v",
					tc.name, asked, len(spared.writes()), built.name, tc.writtenFor.Carrier())
			}
		})
	}

	// A SEAT OF THE OLDER BUILD sends its ceiling and no carrier.
	t.Run("an older seat's write", func(t *testing.T) {
		t.Parallel()
		f := newFleet(t, "data-a")
		halves := f.gitletNodes(t, []string{"git-a"})
		q := f.start(t)
		ceiling := `"ceiling":{"kind":"seat","seat":"lead"}`
		for _, c := range []struct {
			op, subject, args string
		}{
			{opCodeChange.spec.name, laneProposals.subject("data-a"),
				`{"op_id":"019a0c2e-0000-7000-8000-0000000000d0.code_close_change","asker":{"kind":"agent","seat":"swe"},` +
					`"expected_version":3,` + ceiling + `,"close":{"repo_id":"repo-older",` +
					`"change_id":"019a0c2e-0000-7000-8000-0000000000d1"}}`},
			{opCodeGitWrite.spec.name, laneGitWrites.subject("git-a"),
				`{"kind":"create_branch","repo_id":"` + repoID(42) + `","principal":{"kind":"agent","seat":"swe",` +
					`"run":{"turn_id":"019a0c2e-0000-7000-8000-00000000d00d"}},` +
					`"op_id":"019a0c2e-0000-7000-8000-0000000000f4","fresh":{"level":"linearizable"},` +
					`"ref":"refs/heads/seats/swe/a","commit":"7777777777777777777777777777777777777777",` + ceiling + `}`},
		} {
			rep := askSignedOn(t, q, c.subject, request{Op: c.op, Args: json.RawMessage(c.args)})
			if rep.Err != nil || rep.Unserved != "" {
				t.Fatalf("%s from an older seat was answered %+v, want it served", c.op, rep)
			}
		}
		node := f.nodes["data-a"]
		node.mu.Lock()
		changes := slices.Clone(node.changes)
		node.mu.Unlock()
		lead := &gitlet.WriteCeiling{Kind: gitlet.WriteCeilingSeat, Seat: "lead"}
		if len(changes) != 1 || !changes[0].WrittenFor.Stated() || changes[0].WrittenFor.Carrier() != nil ||
			!reflect.DeepEqual(changes[0].Ceiling, lead) {
			t.Errorf("the older seat's change arrived as %+v, want stated nobody beside the ceiling it sent", changes)
		}
		writes := halves["git-a"].writes()
		if len(writes) != 1 || !writes[0].w.WrittenFor.Stated() || writes[0].w.WrittenFor.Carrier() != nil ||
			!reflect.DeepEqual(writes[0].w.Ceiling, lead) {
			t.Errorf("the older seat's engine write arrived as %+v, want stated nobody beside the ceiling it sent",
				writes)
		}
	})
}

// A CODE WRITE FROM A BUILD WITHOUT THE CARRIER IS MADE FOR NOBODY (ADR-0109's
// roll). A seat or a Gitlet node of the build before the carrier sends a code
// write's request with no member for whom it is made at all, beside its
// ceiling where the request takes one. Decoded as a serving node decodes a
// request's inputs, each of the six arrives stated nobody, and the rule every
// node holds one to admits it: with no ceiling, with a colleague's and with
// nobody's — the two its decide still holds it to as before the carrier, a
// colleague's grant bounding it and nobody's sparing only the seat's own work
// (gitlet's TestACodeWriteWhoseCarrierAndCeilingDisagreeIsRefused, its older
// sender's rows).
//
// Mutations: a request's decoder stating nothing it received — that request
// arrives stating nothing, red; the rule refusing a ceiling that comes with no
// carrier — every ceilinged request refused, red.
func TestACodeWriteFromABuildWithoutTheCarrierIsMadeForNobody(t *testing.T) {
	t.Parallel()
	seat := gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: "swe"}
	op := "019a0c2e-0000-7000-8000-0000000000f0.code_merge_change"
	repo, changeID := repoID(43), "019a0c2e-0000-7000-8000-0000000000d1"
	head := "7777777777777777777777777777777777777777"
	enqueue := gitlet.QueueProposal{OpID: op, Asker: seat, Enqueue: &gitlet.Enqueue{RepoID: repo,
		EntryID: gitlet.QueueEntryID(op), ChangeID: changeID, TargetRef: "refs/heads/main", HeadOID: head,
		Method: gitlet.MergeMethodMerge}}
	merge := gitlet.MergeProposal{OpID: op, Asker: seat, Merge: gitlet.Merge{RepoID: repo,
		MergeID: gitlet.WritePushID(op), ChangeID: changeID, Method: gitlet.MergeMethodFastForward,
		TargetRef: "refs/heads/main", BaseOID: "6666666666666666666666666666666666666666", HeadOID: head,
		CommitOID: head, Node: "git-a", ReceivedAt: envelopeInstant}}
	push := proposalTo(repo, "019a0c2e-0000-7000-8000-00000000a11d")
	push.Principal = gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: "swe",
		Run: gitlet.RunProvenance{TurnID: "019a0c2e-0000-7000-8000-00000000d00d"}}
	push.Via = gitlet.ViaEngine
	for _, ceiling := range []*gitlet.WriteCeiling{nil, {Kind: gitlet.WriteCeilingSeat, Seat: "lead"},
		{Kind: gitlet.WriteCeilingNobody}} {
		write := seatWriteOf(repo)
		write.Ceiling = ceiling
		requests := map[*opSpec]any{
			opCodeChange.spec: gitlet.ChangeProposal{OpID: op, Asker: seat, ExpectedVersion: 3, Ceiling: ceiling,
				Close: &gitlet.ChangeClose{RepoID: repo, ChangeID: changeID}},
			opCodeConversation.spec: gitlet.ConversationWrite{OpID: op, Asker: seat, Ceiling: ceiling,
				Comment: &gitlet.Comment{RepoID: repo, ChangeID: changeID, Body: "Why two retries?"}},
			opCodeGitWrite.spec: write,
			opCodeQueue.spec: gitlet.QueueProposal{OpID: op, Asker: seat, Ceiling: ceiling,
				Dequeue: &gitlet.Dequeue{RepoID: repo, EntryID: gitlet.QueueEntryID(op)}},
		}
		if ceiling == nil {
			// The three that take no ceiling: their builder held the write
			// to its own.
			requests[opCodePropose.spec], requests[opCodeMerge.spec] = push, merge
			requests[&opSpec{name: opCodeQueue.spec.name + " (an enqueue)", decode: opCodeQueue.spec.decode}] =
				enqueue
		}
		for spec, sent := range requests {
			// AS THE OLDER BUILD SENDS IT: no member for whom it is made.
			v := reflect.New(reflect.TypeOf(sent)).Elem()
			v.Set(reflect.ValueOf(sent))
			v.FieldByName("WrittenFor").Set(reflect.ValueOf(types.WrittenFor{}))
			raw, err := encodeInput(v.Interface())
			if err != nil {
				t.Fatalf("encode %s: %v", spec.name, err)
			}
			if strings.Contains(string(raw), "written_for") {
				t.Fatalf("%s as an older build sends it holds a carrier: %s", spec.name, raw)
			}
			args, _, refused, err := inputs(spec, request{Op: spec.name, Args: raw})
			if err != nil || refused != nil {
				t.Fatalf("%s from an older build does not decode: %v %v", spec.name, refused, err)
			}
			arrived := reflect.ValueOf(args)
			writtenFor := arrived.FieldByName("WrittenFor").Interface().(types.WrittenFor)
			if carrier, err := writtenFor.For(iam.ActorAgent); err != nil || carrier != nil {
				t.Errorf("%s from an older build under the ceiling %+v arrives made for %+v (%v), want stated "+
					"nobody", spec.name, ceiling, carrier, err)
			}
			if err := args.(interface{ Validate() error }).Validate(); err != nil {
				t.Errorf("%s from an older build under the ceiling %+v is refused: %v, want it admitted, decided "+
					"by its ceiling", spec.name, ceiling, err)
			}
		}
	}
}

// laterApp is a name this build does not know as an app: one a newer build
// adds, as #1020 added atlas. laterKind is a name this build does not know as
// a kind of carrier: one a newer build adds beside the seat and the
// credential. The cases that read either check first that this build still
// does not know it.
const (
	laterApp  iam.Grant            = "ledger"
	laterKind types.WorkingForKind = "group"
)

// olderCarrier is a data node or a Gitlet node of a build that does not know a
// value a carrier names, as the router meets one: every request it is sent
// holds, wherever it carries a carrier, what rename makes of that carrier — a
// name this build does not know in the place of a value the asker's build
// knows and this node's does not.
func olderCarrier(rename func(carrier map[string]any)) func(request) request {
	return func(req request) request {
		doc, err := looseDoc(req.Args)
		if err != nil {
			panic("the arguments of " + req.Op + " do not decode: " + err.Error())
		}
		if args, held := doc.(map[string]any); held {
			if carrier, held := args["written_for"].(map[string]any); held {
				rename(carrier)
			}
		}
		raw, err := json.Marshal(doc)
		if err != nil {
			panic(err)
		}
		req.Args = raw
		return req
	}
}

// knowsNoApp is a node of a build that does not know app, as atlas was to every
// build before it became one: a credential's carrier naming app names laterApp
// in its place.
func knowsNoApp(app iam.Grant) func(request) request {
	return olderCarrier(func(carrier map[string]any) {
		apps, _ := carrier["apps"].([]any)
		for i, named := range apps {
			if named == string(app) {
				apps[i] = string(laterApp)
			}
		}
	})
}

// knowsNoKind is a node of a build that does not know kind, as every build is
// before a kind of carrier is added: a carrier of that kind names laterKind in
// its place.
func knowsNoKind(kind types.WorkingForKind) func(request) request {
	return olderCarrier(func(carrier map[string]any) {
		if carrier["kind"] == string(kind) {
			carrier["kind"] = string(laterKind)
		}
	})
}

// carrierWalk is one code write of a case about a node whose build does not
// know a value a carrier names: whom it is made for and under which ceiling,
// and where it runs — on the node that does not know the value (onLacking), on
// the one after it, or, where every node lacks it (nowhere), on none.
type carrierWalk struct {
	name               string
	writtenFor         types.WrittenFor
	ceiling            *gitlet.WriteCeiling
	onLacking, nowhere bool
}

// walksPastANodeLacking holds a code write's walk to each case, on fleets
// whose first node is older (a build that does not know lacks, a value a
// carrier names): a change's write and a push, asked of the repository's
// committing data nodes, and an engine write, asked of its Gitlet nodes. Each
// runs where its case says, once, made for whom it was sent for; and where no
// node serves it, the caller is told that every node refused it, naming key,
// and that nothing ran — an estate nobody served, which the tool answers as
// fleet_builds_differ — never that the request is malformed.
func walksPastANodeLacking(t *testing.T, older func(request) request, lacks, key string, cases []carrierWalk) {
	t.Helper()
	lacking := func(n *fakeNode) { n.olderBuild = older }
	seat := gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: "swe"}
	change := func(repo string, tc carrierWalk) gitlet.ChangeProposal {
		return gitlet.ChangeProposal{OpID: "019a0c2e-0000-7000-8000-0000000000d0.code_close_change", Asker: seat,
			ExpectedVersion: 3, Ceiling: tc.ceiling, WrittenFor: tc.writtenFor,
			Close: &gitlet.ChangeClose{RepoID: repo, ChangeID: "019a0c2e-0000-7000-8000-0000000000d1"}}
	}
	push := func(repo string, writtenFor types.WrittenFor) gitlet.Proposal {
		p := proposalTo(repo, "019a0c2e-0000-7000-8000-00000000a11d")
		p.Principal = gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: "swe",
			Run: gitlet.RunProvenance{TurnID: "019a0c2e-0000-7000-8000-00000000d00d"}}
		p.Via, p.WrittenFor = gitlet.ViaEngine, writtenFor
		return p
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// A DATA NODE'S: a change's write and a push.
			holders := []string{"data-a", "data-b"}
			f := newFleet(t, holders...)
			const repo = "repo-older-carrier"
			o := rendezvous.Order(repo, holders)
			committer, runnerUp := f.nodes[o[0]], f.nodes[o[1]]
			committer.set(lacking)
			if tc.nowhere {
				runnerUp.set(lacking)
			}
			_, changeErr := f.client.Code().Change(t.Context(), change(repo, tc))
			_, pushErr := f.client.Code().Propose(t.Context(), push(repo, tc.writtenFor))

			// A GITLET NODE'S: an engine write.
			gits := []string{"git-a", "git-b"}
			halves := f.gitletNodes(t, gits)
			w := seatWriteOf(repoID(46))
			order := rendezvous.Order(w.RepoID, gits)
			f.nodes[order[0]].set(lacking)
			if tc.nowhere {
				f.nodes[order[1]].set(lacking)
			}
			w.Ceiling, w.WrittenFor = tc.ceiling, tc.writtenFor
			_, writeErr := f.client.Code().GitWrite(t.Context(), w)

			for what, err := range map[string]error{"change's write": changeErr, "push": pushErr,
				"engine write": writeErr} {
				if !tc.nowhere {
					if err != nil {
						t.Errorf("the %s %s: %v, want it run", what, tc.name, err)
					}
					continue
				}
				var refused *ErrUnknownKeys
				switch {
				case !errors.As(err, &refused) || !slices.Equal(refused.Keys, []string{key}) ||
					len(refused.Nodes) != 2:
					t.Errorf("the %s %s answered %v, want it refused by both nodes, naming %s", what, tc.name,
						err, key)
				case !errors.As(err, new(*ErrUnserved)) || errors.Is(err, ErrOutcomeUnknown):
					t.Errorf("the %s %s answered %v, want an estate nobody served, in which nothing ran", what,
						tc.name, err)
				}
			}
			if tc.nowhere {
				for _, node := range []*fakeNode{committer, runnerUp} {
					node.mu.Lock()
					ran := len(node.changes) + len(node.proposals)
					node.mu.Unlock()
					if ran != 0 {
						t.Errorf("%s, which does not know %s, ran %d writes carrying it", node.name, lacks, ran)
					}
				}
				for name, half := range halves {
					if built := len(half.writes()); built != 0 {
						t.Errorf("%s, which does not know %s, built %d engine writes carrying it", name, lacks,
							built)
					}
				}
				return
			}

			ran, idle := runnerUp, committer
			built, spared := halves[order[1]], halves[order[0]]
			if tc.onLacking {
				ran, idle = committer, runnerUp
				built, spared = halves[order[0]], halves[order[1]]
			}
			idle.mu.Lock()
			idleRan := len(idle.changes) + len(idle.proposals)
			idle.mu.Unlock()
			ran.mu.Lock()
			changes, proposals := slices.Clone(ran.changes), slices.Clone(ran.proposals)
			ran.mu.Unlock()
			if idleRan != 0 || len(changes) != 1 || len(proposals) != 1 {
				t.Fatalf("%s ran %d changes and %d pushes and %s ran %d writes, want the one of each on %s alone",
					ran.name, len(changes), len(proposals), idle.name, idleRan, ran.name)
			}
			asked := built.writes()
			if len(spared.writes()) != 0 || len(asked) != 1 {
				t.Fatalf("the engine write was built %d times by %s and %d by %s, want once, by %s",
					len(asked), built.name, len(spared.writes()), spared.name, built.name)
			}
			for what, got := range map[string]types.WrittenFor{"change's write": changes[0].WrittenFor,
				"push": proposals[0].WrittenFor, "engine write": asked[0].w.WrittenFor} {
				if !reflect.DeepEqual(got.Carrier(), tc.writtenFor.Carrier()) {
					t.Errorf("the %s ran made for %+v, want %+v", what, got.Carrier(), tc.writtenFor.Carrier())
				}
			}
		})
	}
}

// A CODE WRITE FOR A CREDENTIAL MOVES OFF A NODE WHOSE BUILD DOES NOT KNOW ITS
// APP (ADR-0109's roll). A credential's carrier names the apps its turn was
// held to, and no sender composes one its own build does not know, so a node
// whose build does not know one is an older build's, in the roll that adds the
// app — as atlas was to every build before it became one. That node runs
// nothing of a code write made for such a credential: the request's decoder
// refuses the app as the wire refuses a member the build does not declare,
// naming args.written_for.apps[], so the walk asks the repository's next node,
// which knows it and decides the write carrying it. A write made for a
// credential holding only apps the older node knows runs there as ever. And
// where no node knows the app, the caller is told that nothing ran and that a
// node of the writer's build is needed — the tool's fleet_builds_differ —
// never that the request is malformed, which would end the walk and tell a
// newer build's valid request it is. For a change's write and a push, asked of
// the repository's committing data nodes, and an engine write, asked of its
// Gitlet nodes.
//
// Mutation: refuse the app this build does not know as invalid, and the writes
// for the credential holding atlas end at the node that does not know it.
func TestACodeWriteForACredentialMovesOffANodeThatDoesNotKnowItsApp(t *testing.T) {
	t.Parallel()
	if laterApp.Valid() {
		t.Fatalf("this build knows %q, and the case needs a name it does not", laterApp)
	}
	nobody := &gitlet.WriteCeiling{Kind: gitlet.WriteCeilingNobody}
	holding := func(apps ...iam.Grant) types.WrittenFor {
		return types.ForTurn(&types.WorkingFor{Kind: types.WorkingForCredential, Login: "token:ci", Apps: apps})
	}
	walksPastANodeLacking(t, knowsNoApp(iam.GrantAtlas), "atlas", "args.written_for.apps[]", []carrierWalk{
		{"holding gitlet, on the node that does not know atlas", holding(iam.GrantGitlet), nobody, true, false},
		{"holding atlas, on a node that knows it", holding(iam.GrantGitlet, iam.GrantAtlas), nobody, false, false},
		{"holding atlas, where no node knows it", holding(iam.GrantGitlet, iam.GrantAtlas), nobody, false, true},
	})
}

// A CODE WRITE MOVES OFF A NODE WHOSE BUILD DOES NOT KNOW ITS CARRIER'S KIND
// (ADR-0109's roll). A carrier names by its kind whom a write is made for — a
// seat, a credential — and no sender composes a kind its own build does not
// know, so a node whose build does not know one is an older build's, in the
// roll that adds the kind. That node runs nothing of a code write carrying
// one: the request's decoder refuses the kind as the wire refuses a member the
// build does not declare, naming args.written_for.kind, so the walk asks the
// repository's next node, which knows it and decides the write carrying it. A
// write made for a kind the older node knows runs there as ever. And where no
// node knows the kind, the caller is told that nothing ran and that a node of
// the writer's build is needed — the tool's fleet_builds_differ — never that
// the request is malformed, which would end the walk and tell a newer build's
// valid request it is. The older node here knows no credential's carrier, as
// a build before that kind would not; for a change's write and a push, asked
// of the repository's committing data nodes, and an engine write, asked of
// its Gitlet nodes.
//
// Mutation: refuse the kind this build does not know as invalid, and the
// writes made for the credential end at the node that does not know its kind.
func TestACodeWriteMovesOffANodeThatDoesNotKnowItsCarriersKind(t *testing.T) {
	t.Parallel()
	if laterKind.Valid() {
		t.Fatalf("this build knows %q, and the case needs a kind it does not", laterKind)
	}
	lead := &gitlet.WriteCeiling{Kind: gitlet.WriteCeilingSeat, Seat: "lead"}
	nobody := &gitlet.WriteCeiling{Kind: gitlet.WriteCeilingNobody}
	forLead := types.ForTurn(&types.WorkingFor{Kind: types.WorkingForSeat, Seat: "lead"})
	forCI := types.ForTurn(&types.WorkingFor{Kind: types.WorkingForCredential, Login: "token:ci",
		Apps: []iam.Grant{iam.GrantGitlet}})
	walksPastANodeLacking(t, knowsNoKind(types.WorkingForCredential), "a credential's carrier",
		"args.written_for.kind", []carrierWalk{
			{"made for a seat, on the node that does not know a credential's carrier", forLead, lead, true, false},
			{"made for a credential, on a node that knows its carrier", forCI, nobody, false, false},
			{"made for a credential, where no node knows its carrier", forCI, nobody, false, true},
		})
}

// carrierRead is a carrier a case about decoding a code write's request
// carries, and what becomes of the request: refused unread, naming unread; or
// read whole, and then admitted by the rule every node holds it to (says "")
// or refused by that rule, naming says.
type carrierRead struct {
	name         string
	carrier      types.WorkingFor
	unread, says string
}

// readsEachCodeWrite decodes each of the six requests a code write forms on
// repo, carrying each case's carrier — under the ceiling of nobody, where the
// request takes a ceiling — as a serving node decodes a request's inputs, and
// holds it to the case.
func readsEachCodeWrite(t *testing.T, repo string, cases []carrierRead) {
	t.Helper()
	seat := gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: "swe"}
	nobody := &gitlet.WriteCeiling{Kind: gitlet.WriteCeilingNobody}
	op := "019a0c2e-0000-7000-8000-0000000000f0.code_merge_change"
	changeID := "019a0c2e-0000-7000-8000-0000000000d1"
	head := "7777777777777777777777777777777777777777"
	write := seatWriteOf(repo)
	write.Ceiling = nobody
	push := proposalTo(repo, "019a0c2e-0000-7000-8000-00000000a11d")
	push.Principal = gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: "swe",
		Run: gitlet.RunProvenance{TurnID: "019a0c2e-0000-7000-8000-00000000d00d"}}
	push.Via = gitlet.ViaEngine
	requests := map[*opSpec]any{
		opCodeChange.spec: gitlet.ChangeProposal{OpID: op, Asker: seat, ExpectedVersion: 3, Ceiling: nobody,
			Close: &gitlet.ChangeClose{RepoID: repo, ChangeID: changeID}},
		opCodeConversation.spec: gitlet.ConversationWrite{OpID: op, Asker: seat, Ceiling: nobody,
			Comment: &gitlet.Comment{RepoID: repo, ChangeID: changeID, Body: "Why two retries?"}},
		opCodeGitWrite.spec: write,
		opCodeQueue.spec: gitlet.QueueProposal{OpID: op, Asker: seat, Ceiling: nobody,
			Dequeue: &gitlet.Dequeue{RepoID: repo, EntryID: gitlet.QueueEntryID(op)}},
		opCodeMerge.spec: gitlet.MergeProposal{OpID: op, Asker: seat, Merge: gitlet.Merge{RepoID: repo,
			MergeID: gitlet.WritePushID(op), ChangeID: changeID, Method: gitlet.MergeMethodFastForward,
			TargetRef: "refs/heads/main", BaseOID: "6666666666666666666666666666666666666666", HeadOID: head,
			CommitOID: head, Node: "git-a", ReceivedAt: envelopeInstant}},
		opCodePropose.spec: push,
	}
	for _, tc := range cases {
		carrier := types.ForTurn(&tc.carrier)
		for spec, sent := range requests {
			v := reflect.New(reflect.TypeOf(sent)).Elem()
			v.Set(reflect.ValueOf(sent))
			v.FieldByName("WrittenFor").Set(reflect.ValueOf(carrier))
			raw, err := encodeInput(v.Interface())
			if err != nil {
				t.Fatalf("encode %s: %v", spec.name, err)
			}
			args, _, refused, err := inputs(spec, request{Op: spec.name, Args: raw})
			if err != nil {
				t.Errorf("%s carrying %s failed to decode: %v, want it refused unread or read whole", spec.name,
					tc.name, err)
				continue
			}
			if tc.unread != "" {
				if refused == nil || !slices.Equal(refused.Keys, []string{tc.unread}) {
					t.Errorf("%s carrying %s was refused %v, want it refused unread, naming %s", spec.name,
						tc.name, refused, tc.unread)
				}
				continue
			}
			if refused != nil {
				t.Errorf("%s carrying %s was refused unread, naming %v, want it read whole", spec.name, tc.name,
					refused.Keys)
				continue
			}
			err = args.(interface{ Validate() error }).Validate()
			switch {
			case tc.says == "" && err != nil:
				t.Errorf("%s carrying %s: %v, want it admitted", spec.name, tc.name, err)
			case tc.says != "" && (err == nil || !strings.Contains(err.Error(), tc.says)):
				t.Errorf("%s carrying %s: %v, want it refused naming %s", spec.name, tc.name, err, tc.says)
			}
		}
	}
}

// A CODE WRITE NAMING AN APP ITS BUILD DOES NOT KNOW IS REFUSED UNREAD, by
// every request a code write forms (ADR-0109's roll). Decoded as a serving node
// decodes a request's inputs, each of the six, its credential's carrier naming
// an app this build does not know, is refused as one the build cannot read
// whole — named args.written_for.apps[], nothing of it to run — and never read
// to be refused as malformed. A name this build knows is the rule's to judge:
// admin, which it knows as no app, and the zero, which names none, are read
// whole and refused by the rule every node holds the request to, since no
// build composes either; and a credential holding apps this build knows is
// read whole and admitted.
//
// Mutations: the merge's decoder skipping the apps — code.merge's request read
// whole, red; every name that is no app read as one the build does not know —
// admin and the zero refused unread, red.
func TestACodeWriteNamingAnAppItsBuildDoesNotKnowIsRefusedUnread(t *testing.T) {
	t.Parallel()
	if laterApp.Valid() {
		t.Fatalf("this build knows %q, and the case needs a name it does not", laterApp)
	}
	holding := func(apps ...iam.Grant) types.WorkingFor {
		return types.WorkingFor{Kind: types.WorkingForCredential, Login: "token:ci", Apps: apps}
	}
	readsEachCodeWrite(t, repoID(47), []carrierRead{
		{"a credential naming an app this build does not know", holding(iam.GrantGitlet, laterApp),
			"args.written_for.apps[]", ""},
		{"a credential naming admin, which this build knows as no app", holding(iam.GrantGitlet, iam.GrantAdmin),
			"", "written_for.apps"},
		{"a credential naming the zero, which names no app", holding(iam.GrantGitlet, ""), "", "written_for.apps"},
		{"a credential holding apps this build knows", holding(iam.GrantGitlet, iam.GrantAtlas), "", ""},
	})
}

// A CODE WRITE NAMING A KIND ITS BUILD DOES NOT KNOW IS REFUSED UNREAD, by
// every request a code write forms (ADR-0109's roll). Decoded as a serving node
// decodes a request's inputs, each of the six, its carrier of a kind this
// build does not know, is refused as one the build cannot read whole — named
// args.written_for.kind, nothing of it to run — and never read to be refused
// as malformed: whether this build knows a carrier's kind is decided where it
// decides whether it knows its apps, in one place every request's decoder
// asks. A kind this build knows is the rule's to judge, and so is the zero:
// the zero, which names none, is read whole and refused by the rule every node
// holds the request to, since no build composes it, and so is a stranger's,
// which this build knows and no code write is made for; and a credential's is
// read whole and admitted.
//
// Mutations: the kind this build does not know read whole — every request
// refused by the rule as malformed, red; the zero kind read as one the build
// does not know — refused unread, red; a stranger's read as one the build does
// not know — refused unread, red; the conversation's decoder skipping the one
// place — code.conversation's request read whole, red.
func TestACodeWriteNamingAKindItsBuildDoesNotKnowIsRefusedUnread(t *testing.T) {
	t.Parallel()
	if laterKind.Valid() {
		t.Fatalf("this build knows %q, and the case needs a kind it does not", laterKind)
	}
	of := func(kind types.WorkingForKind) types.WorkingFor {
		return types.WorkingFor{Kind: kind, Login: "token:ci", Apps: []iam.Grant{iam.GrantGitlet}}
	}
	readsEachCodeWrite(t, repoID(48), []carrierRead{
		{"a carrier of a kind this build does not know", of(laterKind), "args.written_for.kind", ""},
		{"a carrier of the zero kind, which names none", of(""), "", "written_for.kind"},
		{"a stranger's carrier, of a kind this build knows", types.WorkingFor{Kind: types.WorkingForStranger}, "",
			"stranger"},
		{"a credential's carrier, of a kind this build knows", of(types.WorkingForCredential), "", ""},
	})
}
