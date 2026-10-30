package estate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/signedask"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// fleetRing is the keyring every node of a test fleet holds.
var fleetRing = statelog.OneKey("k1", "the estate suite's fleet key material")

// fleetEnvelopes are a test fleet member's envelopes.
var fleetEnvelopes = mustEnvelopes(fleetRing)

func mustEnvelopes(ring statelog.Keyring) *Envelopes {
	env, err := NewEnvelopes(ring)
	if err != nil {
		panic(err)
	}
	return env
}

// signed is req as a fleet member sends it, with an id and a deadline of its
// own where the case named none.
func signed(t *testing.T, env *Envelopes, req request) (request, []byte) {
	t.Helper()
	if req.ID == "" {
		req.ID = signedask.NewID()
	}
	if req.Deadline.IsZero() {
		req.Deadline = time.Now().Add(time.Minute)
	}
	sealed, err := env.sealRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	return req, sealed
}

// askSigned sends req to one node as a fleet member does and reads the reply
// the way the router does: verified, and bound to the request.
func askSigned(t *testing.T, q *memory.Queue, node string, req request) reply {
	t.Helper()
	req, sealed := signed(t, fleetEnvelopes, req)
	replies, err := q.Ask(t.Context(), Subject(node), sealed, 1)
	if err != nil || len(replies) != 1 {
		t.Fatalf("ask %s = (%d replies, %v)", node, len(replies), err)
	}
	rep, why, err := fleetEnvelopes.openReply(replies[0], req.ID)
	if err != nil || why != "" {
		t.Fatalf("the reply from %s was not believed: %q, %v", node, why, err)
	}
	return rep
}

// rawReply is a reply as it arrives, verified but not yet bound to a request —
// which no asker ever reads one as, and which a refusal bound to no request
// can only be read as.
func rawReply(t *testing.T, sealed []byte) reply {
	t.Helper()
	_, domain := fleetEnvelopes.channel.Domains()
	verifier, err := statelog.NewVerifier(domain, fleetRing)
	if err != nil {
		t.Fatal(err)
	}
	body, verdict := verifier.Open(sealed)
	if verdict != statelog.Verified {
		t.Fatalf("the node's reply did not verify: %s", verdict)
	}
	var rep reply
	if err := json.Unmarshal(body, &rep); err != nil {
		t.Fatal(err)
	}
	return rep
}

// sealUnbound is req signed by the fleet with no check that it is bound — the
// request a build that forgot its id or its deadline would send, which this
// build's own [Envelopes.sealRequest] refuses to make.
func sealUnbound(t *testing.T, req request) []byte {
	t.Helper()
	domain, _ := fleetEnvelopes.channel.Domains()
	signer, err := statelog.NewSigner(domain, fleetRing)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	return signer.Seal(raw)
}

// opened is a request as a member of the test fleet reads it off the wire:
// verified and decoded, the zero request where it is not the fleet's.
func opened(sealed []byte) request {
	admitted := fleetEnvelopes.admit(sealed, signedask.NewGuard(), time.Now())
	if admitted.Disposition != signedask.Admitted {
		return request{}
	}
	return admitted.Request
}

// A REQUEST THE FLEET DID NOT SIGN RUNS NOTHING, AND NAMES NOBODY: a data node
// runs a write as the actor its request names, so a request anybody who can
// reach the broker could write is a write as any seat in the company. Three
// ways to try — no signature at all, a signature under the fleet's key id with
// somebody else's material, and the fleet's own signature over bytes altered
// after it was made — and each is answered "did not run", signed by the node
// and bound to no request, with nothing dispatched.
func TestARequestTheFleetDidNotSignRunsNothing(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	node := f.nodes["data-a"]
	write := request{ID: "r-forged", Op: opCreateTask.spec.name, Actor: &swe,
		Args:     json.RawMessage(`{"OpID":"op-forged","Task":{"Project":"ENG"}}`),
		Deadline: time.Now().Add(time.Minute)}

	unsigned, err := json.Marshal(write)
	if err != nil {
		t.Fatal(err)
	}
	_, outsider := signed(t, mustEnvelopes(statelog.OneKey("k1", "not the fleet's material")), write)
	_, altered := signed(t, fleetEnvelopes, write)
	altered = bytes.Replace(altered, []byte(`"handle":"swe"`), []byte(`"handle":"ceo"`), 1)
	if !bytes.Contains(altered, []byte(`"handle":"ceo"`)) {
		t.Fatal("the case did not alter the signed actor")
	}

	for name, sent := range map[string][]byte{
		"unsigned":                         unsigned,
		"signed under somebody else's key": outsider,
		"altered after it was signed":      altered,
	} {
		replies, err := f.start(t).Ask(t.Context(), Subject("data-a"), sent, 1)
		if err != nil || len(replies) != 1 {
			t.Fatalf("%s: ask = (%d replies, %v), want the node to answer", name, len(replies), err)
		}
		rep := rawReply(t, replies[0])
		if rep.Unserved != unservedUnauthenticated || rep.Result != nil || rep.Err != nil {
			t.Errorf("%s: answered %+v, want %q and nothing run", name, rep, unservedUnauthenticated)
		}
		if rep.Request != "" {
			t.Errorf("%s: the refusal names request %q, which nothing vouched for", name, rep.Request)
		}
	}
	if node.askedFor("create") {
		t.Fatal("a request the fleet did not sign was dispatched")
	}
	node.mu.Lock()
	defer node.mu.Unlock()
	if len(node.actors) != 0 {
		t.Fatalf("a writer was built for %+v from a request nobody signed", node.actors)
	}
}

// A REQUEST UNDER A KEY THIS NODE DOES NOT HOLD IS "DID NOT RUN", and the
// router asks a node that holds it: the key is a fact about the node, which an
// operator changes by adding it, so the write runs where the key is — and where
// no data node holds it, the estate is unserved saying why.
func TestARequestUnderAKeyThisNodeDoesNotHoldIsAskedElsewhere(t *testing.T) {
	t.Parallel()
	rotated := statelog.Keyring{ActiveID: "k2", Keys: []statelog.Key{
		fleetRing.Keys[0], {ID: "k2", Material: "the key the fleet is rotating to"},
	}}
	f := newFleet(t)
	behind := &fakeNode{name: "data-a", units: chartOf("data-a")}
	ahead := &fakeNode{name: "data-b", units: chartOf("data-b")}
	serveAs(t, f, behind, fleetEnvelopes)
	serveAs(t, f, ahead, mustEnvelopes(rotated))
	r := routerAs(t, f, "agent-2", mustEnvelopes(rotated))

	f.placement.set(func(p *fakePlacement) { p.nodes = []string{"data-a"} })
	_, err := r.WriterAs(swe).CreateTask(t.Context(), "op-k", worklet.Task{Project: "ENG"}, nil)
	var unserved *ErrUnserved
	if !errors.As(err, &unserved) || !strings.Contains(unserved.Detail, "does not hold the key") {
		t.Fatalf("a write only a node without the key was asked = %v, want the estate "+
			"unserved naming the key", err)
	}
	if behind.askedFor("create") {
		t.Fatal("a node that could not verify the request ran it")
	}

	f.placement.set(func(p *fakePlacement) { p.nodes = []string{"data-a", "data-b"} })
	if _, err := r.WriterAs(swe).CreateTask(t.Context(), "op-k", worklet.Task{Project: "ENG"},
		nil); err != nil {
		t.Fatalf("a write with a node holding the key = %v, want it written there", err)
	}
	if behind.askedFor("create") || !slices.Equal(ahead.ops(), []string{"op-k"}) {
		t.Fatalf("the write ran on %v and %v, want only the node holding the key", behind.ops(),
			ahead.ops())
	}
}

// A REPLY THE ROUTER CANNOT BELIEVE IS NO ANSWER: one not signed by the fleet,
// one the fleet signed for another request, and one altered after it was
// signed. Read, it moves on; a once-write, which the node may have run
// whatever it said, is an unknown outcome. And the node is not suspected — it
// answered, and what it answered with is not its health.
func TestAReplyTheRouterCannotBelieveIsNoAnswer(t *testing.T) {
	t.Parallel()
	outsiders := mustEnvelopes(statelog.OneKey("k1", "not the fleet's material"))
	_, replyDomain := fleetEnvelopes.channel.Domains()
	fleetReplies, err := statelog.NewSigner(replyDomain, fleetRing)
	if err != nil {
		t.Fatal(err)
	}
	for name, forge := range map[string]func(sealed []byte) []byte{
		"the fleet's, in a shape this build cannot decode": func([]byte) []byte {
			return fleetReplies.Seal([]byte(`{"node":"data-x","request":7}`))
		},
		"not signed by the fleet": func(sealed []byte) []byte {
			return outsiders.sealReply(reply{Node: "data-x", Request: opened(sealed).ID,
				Result: json.RawMessage(`{"TotalHint":99}`)})
		},
		"the fleet's answer to another request": func([]byte) []byte {
			return fleetEnvelopes.sealReply(reply{Node: "data-x", Request: "somebody-elses",
				Result: json.RawMessage(`{"TotalHint":99}`)})
		},
		"altered after it was signed": func(sealed []byte) []byte {
			out := fleetEnvelopes.sealReply(reply{Node: "data-x", Request: opened(sealed).ID,
				Result: json.RawMessage(`{"TotalHint":11}`)})
			return bytes.Replace(out, []byte(`"TotalHint":11`), []byte(`"TotalHint":99`), 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFleet(t)
			impostor := f.start(t)
			stop, err := impostor.Serve(t.Context(), Subject("data-x"),
				func(_ context.Context, raw []byte) ([]byte, error) { return forge(raw), nil })
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = stop(context.Background()) })
			f.placement.set(func(p *fakePlacement) { p.nodes = []string{"data-x"} })

			answer, err := f.client.Work().Tasks(t.Context(), worklet.Query{}, time.Now())
			var unserved *ErrUnserved
			if !errors.As(err, &unserved) || answer.TotalHint == 99 {
				t.Fatalf("a read answered only by a reply nobody can believe = (%+v, %v), "+
					"want the estate unserved", answer, err)
			}
			if !strings.Contains(unserved.Detail, "data-x: its reply") {
				t.Errorf("the refusal does not say why the reply was not believed: %s",
					unserved.Detail)
			}
			_, _, err = f.client.Pages().Comment(t.Context(), doclet.Actor{}, "p1",
				doclet.NewComment{Body: "hi"})
			if !errors.Is(err, ErrOutcomeUnknown) {
				t.Fatalf("a once-write answered by a reply nobody can believe = %v, want "+
					"ErrOutcomeUnknown", err)
			}
			// NOR IS A REPEATABLE WRITE'S "did not run": the node took the
			// request and may have run it, so with no other node to ask
			// the write is unknown rather than unserved.
			_, err = f.client.WriterAs(swe).CreateTask(t.Context(), "op-x",
				worklet.Task{Project: "ENG"}, nil)
			if !errors.Is(err, ErrOutcomeUnknown) || errors.As(err, &unserved) {
				t.Fatalf("a repeatable write answered by a reply nobody can believe = "+
					"%v, want ErrOutcomeUnknown", err)
			}
			f.placement.mu.Lock()
			defer f.placement.mu.Unlock()
			if len(f.placement.unanswered) != 0 {
				t.Errorf("a node that answered was reported silent: %v", f.placement.unanswered)
			}
		})
	}
}

// A REPLAYED REQUEST RUNS ONCE: a signature says the fleet wrote the bytes, not
// that it wrote them now, so a request captured off the wire and sent again is
// refused as a replay — signed, and bound to the request it repeats — while
// the first copy ran exactly once.
func TestAReplayedRequestRunsOnce(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	capture := &capturingAsker{Asker: f.start(t)}
	r, err := NewRouter(RouterOptions{Self: "agent-2", Queue: capture, Placement: f.placement,
		Session: NewSession(), Envelopes: fleetEnvelopes})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.WriterAs(swe).CreateTask(t.Context(), "op-once", worklet.Task{Project: "ENG"},
		nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	sent := capture.payloads()
	if len(sent) != 1 {
		t.Fatalf("captured %d requests, want the one", len(sent))
	}
	first := opened(sent[0])
	if first.ID == "" {
		t.Fatal("the captured request does not open as the fleet's")
	}
	replies, err := f.start(t).Ask(t.Context(), Subject("data-a"), sent[0], 1)
	if err != nil || len(replies) != 1 {
		t.Fatalf("replay = (%d replies, %v)", len(replies), err)
	}
	rep, why, err := fleetEnvelopes.openReply(replies[0], first.ID)
	if err != nil || why != "" {
		t.Fatalf("the replay's refusal was not believed: %q, %v", why, err)
	}
	if rep.Unserved != unservedStale || !strings.Contains(rep.Detail, "replay") {
		t.Fatalf("a replayed request was answered %+v, want it refused as a replay", rep)
	}
	if got := f.nodes["data-a"].ops(); !slices.Equal(got, []string{"op-once"}) {
		t.Fatalf("the node ran %v, want the write once", got)
	}
}

// A REQUEST NOBODY IS WAITING FOR RUNS NOTHING: one whose deadline had passed
// when it arrived, and one that names no deadline or no id — none of which the
// fleet's own router sends, and each of which is how a replay would outlive the
// window a node remembers ids for.
func TestARequestNobodyIsWaitingForRunsNothing(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	write := request{Op: opCreateTask.spec.name, Actor: &swe,
		Args: json.RawMessage(`{"OpID":"op-late","Task":{"Project":"ENG"}}`)}
	for name, req := range map[string]request{
		"past its deadline": {ID: "r-late", Op: write.Op, Actor: write.Actor, Args: write.Args,
			Deadline: time.Now().Add(-time.Second)},
		"no deadline": {ID: "r-open", Op: write.Op, Actor: write.Actor, Args: write.Args},
		"no id": {Op: write.Op, Actor: write.Actor, Args: write.Args,
			Deadline: time.Now().Add(time.Minute)},
	} {
		replies, err := f.start(t).Ask(t.Context(), Subject("data-a"), sealUnbound(t, req), 1)
		if err != nil || len(replies) != 1 {
			t.Fatalf("%s: ask = (%d replies, %v)", name, len(replies), err)
		}
		if rep := rawReply(t, replies[0]); rep.Unserved != unservedStale || rep.Request != req.ID {
			t.Errorf("%s: answered %+v, want %q bound to %q", name, rep, unservedStale, req.ID)
		}
	}
	if f.nodes["data-a"].askedFor("create") {
		t.Fatal("a request nobody was waiting for ran")
	}
}

// THE TWO HALVES NEVER VERIFY AS EACH OTHER, NOR AS A STATE-LOG RECORD: the
// estate signs in domains of its own, and the domain is in the key and in the
// MAC, so a reply captured off the wire cannot be sent to a data node as a
// request, and no record of any domain's log is an envelope.
func TestEnvelopeDomainsNeverVerifyAsEachOther(t *testing.T) {
	t.Parallel()
	if req, rep := fleetEnvelopes.channel.Domains(); req != "estate.request" || rep != "estate.reply" {
		t.Fatalf("the estate signs in (%q, %q), want domains of its own", req, rep)
	}
	sent, req := signed(t, fleetEnvelopes, request{Op: opTasks.spec.name})
	rep := fleetEnvelopes.sealReply(reply{Node: "data-a", Request: sent.ID})
	record, err := statelog.NewSigner(trackerDomain, fleetRing)
	if err != nil {
		t.Fatal(err)
	}
	asRecord := record.Seal([]byte(`{"op":"tracker.tasks"}`))

	if _, why, _ := fleetEnvelopes.openReply(req, sent.ID); why == "" {
		t.Error("a request opened as a reply")
	}
	for name, sealed := range map[string][]byte{"a reply": rep, "a tracker record": asRecord} {
		if got := fleetEnvelopes.admit(sealed, signedask.NewGuard(), time.Now()); got.Disposition != signedask.Tampered {
			t.Errorf("%s opened as a request: %s", name, got.Disposition)
		}
	}
}

// A NODE THAT COULD NEITHER SIGN NOR VERIFY IS NOT BUILT: a router or a server
// with no envelopes, and envelopes over a keyring that names no active key,
// are refused where they are built rather than one request at a time on every
// peer.
func TestNothingIsBuiltWithoutEnvelopes(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	if _, err := NewRouter(RouterOptions{Self: "agent-1", Queue: f.start(t),
		Placement: f.placement, Session: NewSession()}); !errors.Is(err, errNoEnvelopes) {
		t.Errorf("a router with no envelopes = %v, want it refused", err)
	}
	if _, err := Serve(t.Context(), f.start(t), ServerOptions{Self: "data-a",
		Local: &fakeNode{}}); !errors.Is(err, errNoEnvelopes) {
		t.Errorf("a server with no envelopes = %v, want it refused", err)
	}
	if _, err := NewEnvelopes(statelog.Keyring{}); !errors.Is(err, statelog.ErrUnsigned) {
		t.Errorf("envelopes over an empty keyring = %v, want %v", err, statelog.ErrUnsigned)
	}
}

// serveAs makes node answer on the fleet's broker with env as its envelopes.
func serveAs(t *testing.T, f *fleet, node *fakeNode, env *Envelopes) {
	t.Helper()
	f.nodes[node.name] = node
	stop, err := Serve(t.Context(), silencer{q: f.start(t), node: node}, ServerOptions{
		Self: node.name, Local: node, Seams: node.seams(), Envelopes: env,
	})
	if err != nil {
		t.Fatalf("serve %s: %v", node.name, err)
	}
	t.Cleanup(func() { _ = stop(context.Background()) })
}

// routerAs is a router on node self over the fleet, with env as its envelopes.
func routerAs(t *testing.T, f *fleet, self string, env *Envelopes) *Router {
	t.Helper()
	r, err := NewRouter(RouterOptions{Queue: f.start(t), Self: self, Placement: f.placement,
		Session: NewSession(), Envelopes: env})
	if err != nil {
		t.Fatal(err)
	}
	r.readBudget, r.writeBudget = 300*time.Millisecond, 300*time.Millisecond
	return r
}

// capturingAsker forwards every request and keeps the bytes it sent.
type capturingAsker struct {
	Asker

	mu   sync.Mutex
	sent [][]byte
}

func (c *capturingAsker) Ask(ctx context.Context, subject string, request []byte,
	want int) ([][]byte, error) {
	c.mu.Lock()
	c.sent = append(c.sent, slices.Clone(request))
	c.mu.Unlock()
	return c.Asker.Ask(ctx, subject, request, want)
}

func (c *capturingAsker) payloads() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.sent)
}
