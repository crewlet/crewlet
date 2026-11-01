package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/providers/llm/harness"
	queuememory "github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/seat/placement"
)

// fakeDeviceLogin is a harness's device login a case drives: it prints its code at
// once and waits for the case to end it or for its context to.
type fakeDeviceLogin struct {
	finish chan error
}

func (f *fakeDeviceLogin) login(ctx context.Context, _ *harness.Provider, out func(harness.DeviceCode)) error {
	out(harness.DeviceCode{URL: "https://auth.example.com/codex/device", Code: "QXJM-7KPD",
		ExpiresAt: time.Now().Add(15 * time.Minute)})
	select {
	case err := <-f.finish:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// signInNode is one node of a fleet sharing fleet, named node, running a codex
// entry signed in by its harness's login, with the sign-in watch running.
func signInNode(t *testing.T, fleet coord.Fleet, leases *coordmemory.Backend, node string,
	login *fakeDeviceLogin) *Engine {
	t.Helper()
	if _, _, err := leases.TryAcquire(t.Context(), coord.NodeResource(node), coord.AcquireOptions{
		Owner: node + ":boot-1", TTL: time.Minute, Meta: placement.NodeProfile{ID: node}.Meta()}); err != nil {
		t.Fatal(err)
	}
	c, _ := chainCompany(t, chainMember{key: "codex-review", agent: "codex", model: "gpt-6.1-sol", subscription: true})
	e := epochEngine(c)
	e.backends = &Backends{Fleet: fleet, Coord: leases}
	e.signIns.login = login.login
	e.testNode = node
	e.startDeviceSignIns(t.Context())
	t.Cleanup(e.stopDeviceSignIns)
	return e
}

// statusOn polls a session's status as node e reads it until it is want.
func statusOn(t *testing.T, e *Engine, id uuid.UUID, want coord.DeviceSignInStatus) coord.DeviceSignIn {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, err := e.DeviceSignIn(t.Context(), id)
		if err == nil && got.Status == want {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("the session reads %+v (%v), want %s", got, err, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A DEVICE SIGN-IN IS ONE SESSION PER MODEL ACROSS THE FLEET, READ AND
// CANCELLED THROUGH ANY NODE. The node that started it relays the harness's code;
// another node serves the status read, refuses a second start for the same
// model naming the running session, and forwards a cancel through the record to
// the node running the process, which ends it and says so.
//
// Mutation: let a second start replace a running session, or cancel only where
// the request lands, and this fails.
func TestADeviceSignInIsOneSessionReadAndCancelledThroughAnyNode(t *testing.T) {
	t.Parallel()
	fleet, leases := coordmemory.NewFleet(), coordmemory.New()
	login := &fakeDeviceLogin{finish: make(chan error)}
	one := signInNode(t, fleet, leases, "node-1", login)
	two := signInNode(t, fleet, leases, "node-2", login)

	started, err := one.StartDeviceSignIn(t.Context(), "codex-review", person)
	if err != nil {
		t.Fatalf("StartDeviceSignIn: %v", err)
	}
	if started.Status != coord.DevicePending || started.Code != "QXJM-7KPD" || started.Node != "node-1" {
		t.Fatalf("the start answered %+v, want node-1's pending session with the harness's code", started)
	}
	if read := statusOn(t, two, started.ID, coord.DevicePending); read.URL != started.URL {
		t.Errorf("node-2 reads %+v, want the session node-1 runs", read)
	}
	var running *SignInInProgressError
	if _, err := two.StartDeviceSignIn(t.Context(), "codex-review", person); !errors.As(err, &running) ||
		running.Running.ID != started.ID {
		t.Fatalf("a second start through node-2 = %v, want the running session refused by name", err)
	}

	if _, err := two.CancelDeviceSignIn(t.Context(), started.ID, person); err != nil {
		t.Fatalf("CancelDeviceSignIn through node-2: %v", err)
	}
	statusOn(t, two, started.ID, coord.DeviceCancelled)

	// AND A NEW SESSION CAN START once the last one ended.
	again, err := two.StartDeviceSignIn(t.Context(), "codex-review", person)
	if err != nil || again.Node != "node-2" || again.ID == started.ID {
		t.Fatalf("a start after the cancel = %+v, %v; want a new session on node-2", again, err)
	}
	if _, err := two.CancelDeviceSignIn(t.Context(), again.ID, person); err != nil {
		t.Fatalf("CancelDeviceSignIn on its own node: %v", err)
	}
	statusOn(t, one, again.ID, coord.DeviceCancelled)
}

// A NODE THAT STOPS ENDS ITS SESSIONS AS FAILED, saying to start again, and a
// session whose node is gone is failed by every other node's sweep: nobody
// waits out a code no process is reading, and the model can be signed in again.
//
// Mutation: leave a stopping node's sessions pending, or sweep only lapsed
// records, and this fails.
func TestADeviceSignInEndsWithItsNode(t *testing.T) {
	t.Parallel()
	fleet, leases := coordmemory.NewFleet(), coordmemory.New()
	login := &fakeDeviceLogin{finish: make(chan error)}
	one := signInNode(t, fleet, leases, "node-1", login)
	two := signInNode(t, fleet, leases, "node-2", login)

	started, err := one.StartDeviceSignIn(t.Context(), "codex-review", person)
	if err != nil {
		t.Fatalf("StartDeviceSignIn: %v", err)
	}
	one.stopDeviceSignIns()
	ended := statusOn(t, two, started.ID, coord.DeviceFailed)
	if !strings.Contains(ended.Detail, "start again") {
		t.Errorf("the stopped node's session says %q, want it to say to start again", ended.Detail)
	}

	// A NODE THAT DIED mid-session left a pending record nobody runs.
	orphan := coord.DeviceSignIn{ID: uuid.New(), Entry: "other", Node: "node-gone",
		Status: coord.DevicePending, StartedAt: time.Now(), ExpiresAt: time.Now().Add(15 * time.Minute),
		By: person.Name, ByKind: person.Kind}
	if _, _, err := fleet.CreateDeviceSignIn(t.Context(), orphan); err != nil {
		t.Fatal(err)
	}
	two.sweepDeviceSignIns(t.Context())
	if got := statusOn(t, two, orphan.ID, coord.DeviceFailed); got.Detail == "" {
		t.Errorf("the orphaned session = %+v, want it failed, saying why", got)
	}

	// AND A RECORD PAST ITS LIFETIME is removed.
	lapsed := orphan
	lapsed.ID, lapsed.Entry, lapsed.Status = uuid.New(), "lapsed", coord.DeviceExpired
	lapsed.ExpiresAt = time.Now().Add(-coord.DeviceSignInLinger - time.Minute)
	if _, _, err := fleet.CreateDeviceSignIn(t.Context(), lapsed); err != nil {
		t.Fatal(err)
	}
	two.sweepDeviceSignIns(t.Context())
	if _, found, _ := fleet.DeviceSignIn(t.Context(), "lapsed"); found {
		t.Error("a record past its lifetime was kept")
	}
}

// AN ENTRY THAT DOES NOT SIGN IN WITH A LOGIN IS REFUSED BEFORE ANY SESSION: a
// device sign-in would store a login none of its calls reads.
func TestADeviceSignInIsRefusedForAKeyEntry(t *testing.T) {
	t.Parallel()
	c, _ := chainCompany(t, chainMember{key: "keyed", agent: "codex", model: "gpt-6.1-sol"})
	spec := c.Config.Providers.LLM["keyed"]
	spec.Auth.Mode = "api-key"
	c.Config.Providers.LLM["keyed"] = spec
	e := epochEngine(c)
	e.backends = &Backends{Fleet: coordmemory.NewFleet()}
	if _, err := e.StartDeviceSignIn(t.Context(), "keyed", person); !errors.Is(err, ErrSignsInWithoutLogin) {
		t.Fatalf("a device sign-in for an api-key entry = %v, want ErrSignsInWithoutLogin", err)
	}
	if all, _ := e.backends.Fleet.ListDeviceSignIns(t.Context()); len(all) != 0 {
		t.Errorf("a refused start left %+v", all)
	}
}

// A NODE THAT CANNOT SEE THE FLEET DOES NOT JUDGE ANOTHER NODE'S SESSION GONE:
// with no lease view, a running session on another node is refused as running,
// never replaced while a person is typing its code.
//
// Mutation: read "no lease view" as "no node is live", and this fails.
func TestUnknownLivenessIsNotADeadSession(t *testing.T) {
	t.Parallel()
	fleet := coordmemory.NewFleet()
	c, _ := chainCompany(t, chainMember{key: "codex-review", agent: "codex", model: "gpt-6.1-sol", subscription: true})
	e := epochEngine(c)
	e.backends, e.testNode = &Backends{Fleet: fleet}, "node-2"
	e.startDeviceSignIns(t.Context())
	t.Cleanup(e.stopDeviceSignIns)
	running := coord.DeviceSignIn{ID: uuid.New(), Entry: "codex-review", Node: "node-1",
		Status: coord.DevicePending, StartedAt: time.Now(), ExpiresAt: time.Now().Add(15 * time.Minute),
		By: person.Name, ByKind: person.Kind}
	if _, _, err := fleet.CreateDeviceSignIn(t.Context(), running); err != nil {
		t.Fatal(err)
	}
	var busy *SignInInProgressError
	if _, err := e.StartDeviceSignIn(t.Context(), "codex-review", person); !errors.As(err, &busy) {
		t.Fatalf("a start beside another node's session, with no lease view = %v, want it refused", err)
	}
}

// A LAPSED SESSION BLOCKS NOTHING: a record past its lifetime (its code expired
// longer ago than the linger) reads as absent by its id, and a new start
// replaces it at the version read, even while the node that ran it is alive
// and no sweep has removed it yet.
//
// Mutation: drop the lapse from what makes a session over, or answer a lapsed
// record by its id, and this fails.
func TestALapsedSessionBlocksNoNewOne(t *testing.T) {
	t.Parallel()
	fleet, leases := coordmemory.NewFleet(), coordmemory.New()
	login := &fakeDeviceLogin{finish: make(chan error)}
	one := signInNode(t, fleet, leases, "node-1", login)
	two := signInNode(t, fleet, leases, "node-2", login)
	stale := coord.DeviceSignIn{ID: uuid.New(), Entry: "codex-review", Node: "node-1",
		Status: coord.DevicePending, StartedAt: time.Now().Add(-time.Hour),
		ExpiresAt: time.Now().Add(-coord.DeviceSignInLinger - time.Minute),
		By:        person.Name, ByKind: person.Kind}
	if _, _, err := fleet.CreateDeviceSignIn(t.Context(), stale); err != nil {
		t.Fatal(err)
	}
	if _, err := one.DeviceSignIn(t.Context(), stale.ID); !errors.Is(err, ErrNoSuchSignIn) {
		t.Errorf("reading a lapsed session = %v, want it absent", err)
	}
	started, err := two.StartDeviceSignIn(t.Context(), "codex-review", person)
	if err != nil || started.ID == stale.ID || started.Node != "node-2" {
		t.Fatalf("a start over a lapsed session = %+v, %v; want a new session on node-2", started, err)
	}
	if _, err := two.CancelDeviceSignIn(t.Context(), started.ID, person); err != nil {
		t.Fatalf("CancelDeviceSignIn: %v", err)
	}
}

// endsAudited is every device sign-in end q carries, as `model_acted`.
func endsAudited(q *queuememory.Queue) []types.ModelActed {
	var out []types.ModelActed
	for _, ev := range q.History() {
		if row, ok := ev.Data.(*types.ModelActed); ok && row.Action == types.ModelDeviceSignInEnded {
			out = append(out, *row)
		}
	}
	return out
}

// HOW A DEVICE SIGN-IN ENDED IS IN THE AUDIT TRAIL, as the person's who
// started it.
//
// The route that started it answered with a pending code; whether a login was
// stored is decided minutes later on the node running the harness, and no route
// call marks it. The node that ran the session publishes its end, and a
// session whose node died is published by the sweep that fails it.
//
// Mutation: publish nothing from endDeviceSignIn, or from the sweep, and
// this fails.
func TestADeviceSignInsEndIsAudited(t *testing.T) {
	t.Parallel()
	fleet, leases := coordmemory.NewFleet(), coordmemory.New()
	login := &fakeDeviceLogin{finish: make(chan error, 1)}
	one := signInNode(t, fleet, leases, "node-1", login)
	q := queuememory.New()
	if err := q.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = q.Stop(context.Background()) })
	one.backends.Queue = q

	started, err := one.StartDeviceSignIn(t.Context(), "codex-review", person)
	if err != nil {
		t.Fatalf("StartDeviceSignIn: %v", err)
	}
	login.finish <- harness.ErrDeviceCodeExpired
	statusOn(t, one, started.ID, coord.DeviceExpired)
	deadline := time.Now().Add(5 * time.Second)
	for len(endsAudited(q)) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	ends := endsAudited(q)
	if len(ends) != 1 || ends[0].Status != "expired" || ends[0].ProviderKey != "codex-review" ||
		ends[0].ActorName != person.Name || ends[0].OperatorID != person.OperatorID {
		t.Fatalf("the expiry is audited as %+v, want one row of jane-doe's session", ends)
	}

	// A SESSION WHOSE NODE IS GONE is audited by the sweep that fails it.
	orphan := coord.DeviceSignIn{ID: uuid.New(), Entry: "other", Node: "node-gone",
		Status: coord.DevicePending, StartedAt: time.Now(), ExpiresAt: time.Now().Add(15 * time.Minute),
		By: person.Name, ByKind: person.Kind}
	if _, _, err := fleet.CreateDeviceSignIn(t.Context(), orphan); err != nil {
		t.Fatal(err)
	}
	one.sweepDeviceSignIns(t.Context())
	ends = endsAudited(q)
	if len(ends) != 2 || ends[1].Status != "failed" || ends[1].Outcome != types.AuditFailed ||
		ends[1].ProviderKey != "other" {
		t.Errorf("the orphan's end is audited as %+v", ends)
	}
}
