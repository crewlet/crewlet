package estate

import (
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
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
