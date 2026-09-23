package stream_test

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/crewlet/crewlet/internal/api/stream"
)

// countingData counts how many times the envelope carrying it is marshalled.
//
// COUNTING, NOT TIMING. "The fan-out got faster" is a benchmark's claim and it
// moves with the machine; "one envelope was marshalled once" is the property,
// and a MarshalJSON that increments is the only place in the process that can
// observe it without the hub having to keep a counter for a test's sake.
type countingData struct{ n *atomic.Int64 }

func (c countingData) MarshalJSON() ([]byte, error) {
	c.n.Add(1)
	return []byte(`{"counted":true}`), nil
}

// TestAFrameIsEncodedOncePerPosture is the whole point of the rewrite.
//
// A client's posture is the only thing that decides its bytes, so two clients
// in the same posture must receive the byte-identical frame from ONE encode.
// With N dashboards open this used to be N marshals of identical JSON, on N
// goroutines, for every push — the envelope travelled to each writer and each
// writer encoded it for itself.
func TestAFrameIsEncodedOncePerPosture(t *testing.T) {
	t.Parallel()
	h := stream.NewHub()
	const clients = 16
	live := make([]*stream.Client, 0, clients)
	for range clients {
		c := stream.NewClient(reader)
		h.Register(c)
		live = append(live, c)
	}

	var encodes atomic.Int64
	h.Broadcast(stream.Push(stream.KindEvent, countingData{n: &encodes}, clock))

	if got := encodes.Load(); got != 1 {
		t.Errorf("encodes = %d for %d clients in one posture, want 1: the "+
			"envelope is being marshalled per client again", got, clients)
	}
	// And they all got it, so the single encode is not a single DELIVERY.
	for i, c := range live {
		if got := drain(c); len(got) != 1 {
			t.Fatalf("client %d received %d frames, want 1", i, len(got))
		}
	}

	// THE CONTROL that makes the count above able to fail: a second posture
	// is a second set of bytes, so it costs a second encode. If the count
	// were measuring nothing, this would read 1 as well.
	degraded := stream.NewClient(reader)
	h.Register(degraded)
	if !degraded.SetPosture(stream.FrameDegraded) {
		t.Fatal("the degraded posture was refused")
	}
	// KindHealth, because it is the one fanned-out kind a degraded client
	// still receives — the keepalive that tells it why the others stopped.
	encodes.Store(0)
	h.Broadcast(stream.Push(stream.KindHealth, countingData{n: &encodes}, clock))
	if got := encodes.Load(); got != 2 {
		t.Errorf("encodes = %d across two postures, want 2: one per posture, "+
			"not one per client and not one for all of them", got)
	}
}

// TestTwoClientsInOnePostureShareTheExactBytes is the other half: "once" is
// worth nothing unless what they share is the same frame.
func TestTwoClientsInOnePostureShareTheExactBytes(t *testing.T) {
	t.Parallel()
	h := stream.NewHub()
	a, b := stream.NewClient(reader), stream.NewClient(reader)
	h.Register(a)
	h.Register(b)

	h.Broadcast(stream.Push(stream.KindEvent, map[string]any{"id": "e1"}, clock))

	first, second := drain(a), drain(b)
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("frames = %d and %d, want one each", len(first), len(second))
	}
	if first[0] != second[0] {
		t.Errorf("two clients in one posture received different frames (%p, %p); "+
			"the shared encode is not being shared", first[0], second[0])
	}
}

// TestADegradedClientLosesThePushesAndKeepsTheHealthFrame pins what the
// posture actually changes about a fan-out.
func TestADegradedClientLosesThePushesAndKeepsTheHealthFrame(t *testing.T) {
	t.Parallel()
	h := stream.NewHub()
	live, degraded := stream.NewClient(reader), stream.NewClient(reader)
	h.Register(live)
	h.Register(degraded)
	degraded.SetPosture(stream.FrameDegraded)

	h.Broadcast(stream.Push(stream.KindEvent, nil, clock))
	h.Broadcast(stream.Push(stream.KindAgents, nil, clock))
	h.Broadcast(stream.Push(stream.KindHealth, nil, clock))

	if got := len(drain(live)); got != 3 {
		t.Errorf("a live client received %d frames, want all 3", got)
	}
	got := drain(degraded)
	if len(got) != 1 || got[0].Kind() != stream.KindHealth {
		t.Errorf("a degraded client received %v, want the health frame alone: "+
			"it is the keepalive AND the explanation", kindsIn(got))
	}
}

// TestTheHubPostureReachesEveryClient: the NODE is what degrades, so one call
// has to move every open socket — a per-client decision made at connect would
// leave every tab that was already open rendering a copy the fleet abandoned.
func TestTheHubPostureReachesEveryClient(t *testing.T) {
	t.Parallel()
	h := stream.NewHub()
	before := stream.NewClient(reader)
	h.Register(before)

	h.SetPosture(stream.FrameDegraded)
	after := stream.NewClient(reader)
	h.Register(after)

	for name, c := range map[string]*stream.Client{"already open": before, "new": after} {
		if got := c.Posture(); got != stream.FrameDegraded {
			t.Errorf("the %s client is %q, want degraded", name, got)
		}
	}
	// The control: an INVALID posture is refused and the current one kept,
	// because the zero value is what a caller reaches by forgetting to
	// decide and the two ways to read it are opposite.
	h.SetPosture(stream.FramePosture(""))
	if got := h.Posture(); got != stream.FrameDegraded {
		t.Errorf("the hub took an invalid posture: %q", got)
	}
	if stream.FramePosture("").Valid() {
		t.Error("the zero FramePosture reports itself valid")
	}
}

// --- the route table ------------------------------------------------------ //

// TestEveryPushKindHasARoute walks the frozen kind list and holds the route
// table total over it.
//
// A kind with no route has no safe default: the branch a switch would fall to
// is "everyone", and for a per-seat frame that is the whole company reading
// one seat's mail. The list is read from this package's own SOURCE so a kind
// added in hub.go is covered without being listed here — the same idiom
// TestTheSocketAndRestShareOneTable uses for the error codes.
func TestEveryPushKindHasARoute(t *testing.T) {
	t.Parallel()
	kinds := declaredKinds(t)
	// The first seat-routed kind, named explicitly: a walk that silently
	// stopped finding it would look exactly like a pass — and it is the
	// one kind whose route decides WHO receives it rather than whether
	// anybody does.
	if !slicesContains(kinds, stream.KindInboxChanged) {
		t.Fatalf("the walk found %v and not %q, so it is not reading the "+
			"frozen list", kinds, stream.KindInboxChanged)
	}
	if got := stream.RouteOf(stream.KindInboxChanged); got != stream.RouteSeat {
		t.Errorf("%q routes as %q, want %q", stream.KindInboxChanged, got, stream.RouteSeat)
	}
	if unrouted := withoutARoute(kinds); len(unrouted) > 0 {
		t.Errorf("these push kinds have no route: %v — add each to the route "+
			"table in internal/api/stream/hub.go, because a kind with no "+
			"entry is dropped rather than guessed at", unrouted)
	}
	// THE CONTROL: mount a kind with no route and the walk goes red.
	if got := withoutARoute([]string{"from_the_future"}); len(got) != 1 {
		t.Errorf("a kind with no route passed the walk (%v), so this test is "+
			"asserting nothing", got)
	}
}

// EVERY SEAT-ROUTED KIND HAS A PRODUCER.
//
// A seat-routed kind is a promise to a screen — "you will be told when this
// seat's inbox moves" — and a promise with a route and an index and nothing
// that ever builds the frame is a screen waiting on a push that cannot come,
// indistinguishable from a quiet company. `inbox_changed` shipped in exactly
// that state, declared and routed with its publisher to follow. So the walk
// reads this package's own source for a PushSeat call naming each such kind,
// which is the one constructor a seat-routed frame can be built with.
func TestEverySeatRoutedKindHasAProducer(t *testing.T) {
	t.Parallel()
	consts := declaredKindConsts(t)
	produced := seatProducers(t)
	for name, kind := range consts {
		if stream.RouteOf(kind) != stream.RouteSeat {
			continue
		}
		if !produced[name] {
			t.Errorf("%s (%q) is routed by seat and nothing in this package "+
				"builds one with PushSeat: a screen watching for it waits on a "+
				"push that cannot come", name, kind)
		}
	}
	// THE CONTROL: the walk is worth having only if it can find a producer,
	// and only if an unproduced name would fail it.
	if !produced["KindInboxChanged"] {
		t.Fatalf("the walk found producers for %v and not KindInboxChanged, so "+
			"it is not reading Service.InboxChanged", produced)
	}
	if produced["KindFromTheFuture"] {
		t.Error("the walk reports a producer for a kind nothing declares")
	}
}

// seatProducers is every Kind… constant this package's non-test source passes
// as the first argument of PushSeat.
func seatProducers(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, file := range parsedSource(t) {
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			if fn, ok := call.Fun.(*ast.Ident); !ok || fn.Name != "PushSeat" {
				return true
			}
			if kind, ok := call.Args[0].(*ast.Ident); ok {
				out[kind.Name] = true
			}
			return true
		})
	}
	return out
}

// declaredKindConsts is every Kind… string constant in this package's source,
// by name.
func declaredKindConsts(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, file := range parsedSource(t) {
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				value, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, ident := range value.Names {
					if !strings.HasPrefix(ident.Name, "Kind") || i >= len(value.Values) {
						continue
					}
					lit, ok := value.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					kind, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatalf("%s: %v", ident.Name, err)
					}
					out[ident.Name] = kind
				}
			}
		}
	}
	return out
}

// parsedSource is this package's non-test files, parsed.
func parsedSource(t *testing.T) []*ast.File {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}
	fset := token.NewFileSet()
	var out []*ast.File
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		out = append(out, file)
	}
	return out
}

// withoutARoute is the kinds with no entry on the route table.
func withoutARoute(kinds []string) []string {
	var out []string
	for _, kind := range kinds {
		if !stream.RouteOf(kind).Valid() {
			out = append(out, kind)
		}
	}
	return out
}

// declaredKinds is every string constant named Kind… in this package's source.
func declaredKinds(t *testing.T) []string {
	t.Helper()
	kinds := slices.Sorted(maps.Values(declaredKindConsts(t)))
	if len(kinds) == 0 {
		t.Fatal("no Kind constant found in this package, so this test could not fail")
	}
	return kinds
}

// --- routing by seat ------------------------------------------------------ //

// TestASeatFrameReachesOnlyThatSeatsWatchers is the routing index doing its
// job: a per-seat frame must not be a broadcast with a filter on the client.
func TestASeatFrameReachesOnlyThatSeatsWatchers(t *testing.T) {
	t.Parallel()
	h := stream.NewHub()
	watching, other, idle := stream.NewClient(reader), stream.NewClient(reader), stream.NewClient(reader)
	for _, c := range []*stream.Client{watching, other, idle} {
		h.Register(c)
	}
	h.Watch(watching, "lead")
	h.Watch(other, "coder")

	h.Broadcast(stream.PushSeat(stream.KindInboxChanged, "lead",
		map[string]any{"pending": 2}, clock))

	got := drain(watching)
	if len(got) != 1 || got[0].Kind() != stream.KindInboxChanged {
		t.Fatalf("the watcher received %v", kindsIn(got))
	}
	var body map[string]any
	decodeData(t, got[0], &body)
	if body["pending"] != float64(2) {
		t.Errorf("payload = %v", body)
	}
	var frame map[string]any
	if err := json.Unmarshal(got[0].Raw(), &frame); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if frame["seat"] != "lead" {
		t.Errorf("the frame does not name its seat: %v; a routing key the "+
			"receiver cannot read is one only the sender can debug", frame)
	}
	for name, c := range map[string]*stream.Client{"another seat's": other, "no seat's": idle} {
		if received := drain(c); len(received) != 0 {
			t.Errorf("%s watcher received %v", name, kindsIn(received))
		}
	}
}

// TestASeatFrameWithNoSeatIsDroppedRatherThanFannedOut is the failure the
// route exists to make impossible: a targeted frame that lost its address must
// not become a broadcast.
func TestASeatFrameWithNoSeatIsDroppedRatherThanFannedOut(t *testing.T) {
	t.Parallel()
	h := stream.NewHub()
	c := stream.NewClient(reader)
	h.Register(c)
	h.Watch(c, "lead")

	h.Broadcast(stream.Push(stream.KindInboxChanged, nil, clock))
	if got := drain(c); len(got) != 0 {
		t.Errorf("a seat frame with no seat reached %v", kindsIn(got))
	}
	// The counterfactual, so the drop above is the ADDRESS and not the
	// kind: the same kind WITH a seat arrives.
	h.Broadcast(stream.PushSeat(stream.KindInboxChanged, "lead", nil, clock))
	if got := drain(c); len(got) != 1 {
		t.Errorf("an addressed seat frame did not arrive: %v", kindsIn(got))
	}
}

// TestAnAnswerIsNeverBroadcast: a result carries one client's correlation id,
// so fanning it out answers every other tab's question with it.
func TestAnAnswerIsNeverBroadcast(t *testing.T) {
	t.Parallel()
	h := stream.NewHub()
	c := stream.NewClient(reader)
	h.Register(c)

	for _, kind := range []string{stream.KindResult, stream.KindError, stream.KindPong, "from_the_future"} {
		h.Broadcast(stream.Push(kind, nil, clock))
	}
	if got := drain(c); len(got) != 0 {
		t.Errorf("broadcast delivered %v; direct and unrouted kinds must not "+
			"fan out", kindsIn(got))
	}
	// And the direct path DOES deliver them, so the refusal above is about
	// the route rather than about the kind being unserveable.
	c.Reply(stream.Envelope{Kind: stream.KindResult, ID: 7, What: "agent"})
	if got := drain(c); len(got) != 1 || got[0].Kind() != stream.KindResult {
		t.Errorf("Reply delivered %v, want the result", kindsIn(got))
	}
}

// TestUnregisteringLeavesTheSeatIndex: a bucket holding a client the hub has
// let go of keeps it alive and sends to its closed queue.
func TestUnregisteringLeavesTheSeatIndex(t *testing.T) {
	t.Parallel()
	h := stream.NewHub()
	c := stream.NewClient(reader)
	h.Register(c)
	h.Watch(c, "lead")
	if got := h.Watchers("lead"); got != 1 {
		t.Fatalf("watchers = %d, want 1", got)
	}

	h.Unregister(c)
	if got := h.Watchers("lead"); got != 0 {
		t.Errorf("watchers = %d after an unregister, want 0", got)
	}
	// A client that was never registered is never indexed either.
	stray := stream.NewClient(reader)
	h.Watch(stray, "lead")
	if got := h.Watchers("lead"); got != 0 {
		t.Errorf("an unregistered client entered the index (%d watchers)", got)
	}
}

// kindsIn is the kinds of a frame slice, for a failure message.
func kindsIn(frames []*stream.Frame) []string {
	out := make([]string, 0, len(frames))
	for _, f := range frames {
		out = append(out, f.Kind())
	}
	return out
}

func slicesContains(haystack []string, needle string) bool {
	for _, item := range haystack {
		if item == needle {
			return true
		}
	}
	return false
}
