package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/configplane"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/seat"
)

// sandboxCompanyDoc is a one-seat company whose seat runs code on the in-process
// double. The %s is the rest of the providers.sandbox block, so a case can
// change the catalogue without changing the company.
const sandboxCompanyDoc = `
name: Nimbus
providers:
  sandbox:
    fake: true
%s
roles:
  - name: SWE
    handle: swe
    sandbox:
      enabled: true
`

// companyWithoutSandboxDoc is the same company with nothing that runs code.
const companyWithoutSandboxDoc = `
name: Nimbus
roles:
  - name: SWE
    handle: swe
`

// A NODE THAT BOOTED WITH NO COMPANY BRINGS THE CODE SANDBOX UP WITH THE FIRST
// ONE THAT CONFIGURES IT — the coordinator, the completion poll, run_sandbox,
// and the seat's own preparation — with no restart.
//
// The coordinator was built at boot or never, so a node started the way the
// quickstart starts one served a code-enabled company with no code tool until
// its process restarted, and a run a peer had left on one of its seats was
// polled, collected and resumed by nobody here.
func TestAFirstCompanyWithASandboxBringsTheCoordinatorUpWithoutARestart(t *testing.T) {
	t.Parallel()
	e := sandboxNode(t, nil)
	if e.sandbox.Load() != nil {
		t.Fatal("the premise: a node with no company runs no sandbox")
	}
	// A RUN A PEER LEFT ON THE SEAT, still going, before this node has a
	// company at all: its recovery is part of what the apply has to bring.
	seedRunningRun(t, e, "wk-before", "box-1")

	status, applied, err := e.Apply(t.Context(),
		parseCompany(t, sandboxDoc("")), time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC))
	if err != nil || status != configplane.StatusOK {
		t.Fatalf("Apply = (%s, %v, %v), want ok", status, applied, err)
	}
	for _, stage := range []string{"sandbox_runtime", "sandbox"} {
		if !slices.Contains(applied, stage) {
			t.Errorf("the apply got through %v, which does not name %q", applied, stage)
		}
	}
	rt := e.sandbox.Load()
	if rt == nil {
		t.Fatal("the first sandbox company's apply left the node with no coordinator")
	}
	if e.sandboxWaiter.Load() == nil {
		t.Error("the apply started no completion poll, so no detached run ever finishes")
	}
	if _, ok := e.Company().Tools.Lookup(builtin.RunSandboxTool); !ok {
		t.Error("the applied company has no run_sandbox — equip ran before the " +
			"coordinator it registers against")
	}

	// THE SEAT, once this node takes it: its control topic attached and
	// the running job recovered, so the seat is held by it and the
	// dispatcher's own screening says so.
	waitHeld(t, e, "swe")
	eventually(t, "the seat's running job to hold it", func() bool {
		return e.SeatHeldBySandbox("swe")
	})
	if !e.sandboxSeatAttached("swe") {
		t.Error("the seat took no control topic, so its runs' completions reach nobody here")
	}
	if !e.dispatch.Conditions("swe").SeatHeldBySandbox {
		t.Error("the inbox screening does not see the coordinator the apply brought up, " +
			"so a seat with a job still running takes new turns beside it")
	}

	// THE POLL: the job's box, reached through the catalogue the apply
	// built, is kept alive on every tick.
	box := createFakeBox(t, rt)
	if box.ID() != "box-1" {
		t.Fatalf("the double minted %q, want the id the run names", box.ID())
	}
	eventually(t, "the poll to keep the job's box alive", func() bool {
		return box.Keepalives() > 0
	})
}

// A LATER REVISION THAT CHANGES providers.sandbox RECONFIGURES THE RUNTIME IN
// PLACE: the same coordinator, so the busy set survives, and a manager the
// completion poll reaches on its next tick.
//
// The poll captured the manager it was started with, so after a reload it kept
// reconnecting through the backends of the catalogue the node first ran — a job
// launched after the reload was on a backend the poll had never heard of.
func TestALaterSandboxRevisionReconfiguresTheRunningRuntime(t *testing.T) {
	t.Parallel()
	e := sandboxNode(t, nil)
	seedRunningRun(t, e, "wk-before", "box-1")
	applyOK(t, e, sandboxDoc(""))
	waitHeld(t, e, "swe")
	eventually(t, "the seat's running job to hold it", func() bool {
		return e.SeatHeldBySandbox("swe")
	})
	first := e.sandbox.Load()
	if first == nil {
		t.Fatal("the first sandbox revision brought no runtime up")
	}
	before := first.coordinator.Manager()

	applied := applyOK(t, e, sandboxDoc("    default_timeout_seconds: 7200"))
	if slices.Contains(applied, "sandbox_runtime") || !slices.Contains(applied, "sandbox") {
		t.Errorf("a second sandbox revision got through %v, want the swap and no second start",
			applied)
	}
	if e.sandbox.Load() != first {
		t.Fatal("a revision rebuilt the runtime, forgetting which seats are mid-run")
	}
	after := first.coordinator.Manager()
	if after == before || after.BoxTimeout() != 2*time.Hour {
		t.Fatalf("the coordinator serves a manager with box timeout %v, want the "+
			"revision's 2h", after.BoxTimeout())
	}
	if !e.SeatHeldBySandbox("swe") {
		t.Error("the reload forgot the job still holding the seat")
	}

	// A JOB LAUNCHED AFTER THE RELOAD, on the reloaded backend.
	box := createFakeBox(t, first)
	seedRunningRun(t, e, "wk-after", box.ID())
	eventually(t, "the poll to reach a box on the reloaded backend", func() bool {
		return box.Keepalives() > 0
	})
}

// A NODE ALREADY HOLDING SEATS WHEN A REVISION FIRST ADDS providers.sandbox
// PREPARES THOSE SEATS — their control topics, their runs — rather than only the
// seats it takes afterwards.
//
// Its seats were taken before there was anything to attach, so a run launched
// under the new revision published its completion to a topic nothing on this
// node consumed, and the turn suspended into it never resumed.
func TestASandboxAddedByALaterRevisionPreparesTheSeatsAlreadyHeld(t *testing.T) {
	t.Parallel()
	e := sandboxNode(t, parseCompany(t, companyWithoutSandboxDoc))
	waitHeld(t, e, "swe")
	if e.sandbox.Load() != nil || e.sandboxSeatAttached("swe") {
		t.Fatal("the premise: a company with no providers.sandbox runs no sandbox")
	}
	seedRunningRun(t, e, "wk-before", "box-1")

	applied := applyOK(t, e, sandboxDoc(""))
	if !slices.Contains(applied, "sandbox_runtime") {
		t.Fatalf("the revision that added providers.sandbox got through %v", applied)
	}
	if !slices.Contains(e.node.Host().Held(), "swe") {
		t.Fatal("the seat was handed back rather than prepared")
	}
	if !e.sandboxSeatAttached("swe") {
		t.Error("a seat held before the sandbox arrived has no control topic")
	}
	if !e.SeatHeldBySandbox("swe") {
		t.Error("a seat held before the sandbox arrived never recovered the job running on it")
	}
}

// A SEAT HANDED ON AND TAKEN BACK ATTACHES ITS CONTROL TOPIC AGAIN: a release
// forgets what it detached, so the next acquisition does not read an attach
// that no longer exists and skip its own.
func TestASeatTakenBackAttachesItsControlTopicAgain(t *testing.T) {
	t.Parallel()
	e := sandboxNode(t, nil)
	applyOK(t, e, sandboxDoc(""))
	waitHeld(t, e, "swe")
	eventually(t, "the seat's control topic to attach", func() bool {
		return e.sandboxSeatAttached("swe")
	})

	if !e.node.Host().Release(t.Context(), "swe", seat.ReasonDrain) {
		t.Fatal("the premise: the seat is handed back")
	}
	if e.sandboxSeatAttached("swe") {
		t.Error("a released seat still reads as attached, so taking it back attaches nothing")
	}
	waitHeld(t, e, "swe")
	if !e.sandboxSeatAttached("swe") {
		t.Error("a seat taken back has no control topic")
	}
}

// A REVISION THAT REMOVES providers.sandbox KEEPS THE RUNTIME AND ITS LAST
// MANAGER, for the runs still in flight: they cannot be launched any more and
// still have to be finished through the backends that made them.
func TestARevisionWithoutASandboxKeepsTheRuntimeForTheRunsInFlight(t *testing.T) {
	t.Parallel()
	e := sandboxNode(t, nil)
	applyOK(t, e, sandboxDoc(""))
	rt := e.sandbox.Load()
	if rt == nil {
		t.Fatal("the premise: the sandbox revision brought a runtime up")
	}
	manager := rt.coordinator.Manager()

	applyOK(t, e, companyWithoutSandboxDoc)
	if e.sandbox.Load() != rt || rt.coordinator.Manager() != manager {
		t.Error("removing providers.sandbox took down the runtime or the manager the " +
			"runs in flight are finished through")
	}
	// ...AND LAUNCHES NOTHING NEW on it: the runtime outlives the catalogue
	// only to finish what that catalogue started.
	if _, ok := e.Company().Tools.Lookup(builtin.RunSandboxTool); ok {
		t.Error("a company with no providers.sandbox still registers run_sandbox, " +
			"launching on a catalogue it no longer configures")
	}
}

// A CATALOGUE THAT CANNOT BE BUILT IS REFUSED ON A NODE THAT RUNS NO SANDBOX
// YET, as on one that does — before anything changes.
//
// It was built only where a coordinator already existed, so on every other
// node a revision whose providers.sandbox could never mint a box was published,
// and its code-enabled seats planned around one.
func TestABrokenSandboxCatalogueIsRefusedOnANodeWithoutOne(t *testing.T) {
	t.Parallel()
	e := sandboxNode(t, nil)
	broken := `
name: Nimbus
providers:
  sandbox:
    e2b:
      api_key: "${CREWLET_TEST_E2B_KEY_NEVER_SET}"
roles:
  - name: SWE
    handle: swe
    sandbox:
      enabled: true
      run_in: e2b
`
	status, applied, err := e.Apply(t.Context(), parseCompany(t, broken),
		time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC))
	if err == nil || status != configplane.StatusError {
		t.Fatalf("Apply = (%s, %v, %v), want a refusal", status, applied, err)
	}
	if e.Company() != nil || e.sandbox.Load() != nil {
		t.Error("the unbuildable catalogue was installed or started a runtime")
	}
}

// A POLL INTERVAL NO DUTY CAN BE GRANTED FOR IS REFUSED AT BOOT, on a node with
// no sandbox, as on one that has one.
//
// It was checked only where the poll started, which was boot or never. Once an
// apply can start it, a node that booted empty would carry the bad interval
// until its first sandbox company — and there it would refuse a revision that
// was itself fine, with the runtime already published and no poll behind it.
func TestAPollIntervalNoDutyAdmitsIsRefusedAtBootWithoutASandbox(t *testing.T) {
	t.Parallel()
	b := config.DefaultBootstrap()
	dir := t.TempDir()
	b.Store.Path = filepath.Join(dir, "crewlet.db")
	b.Stream.StoreDir = filepath.Join(dir, "stream")
	e, err := New(t.Context(), Options{
		Bootstrap:           &b,
		SandboxPollInterval: coord.MaxDutyTTL/dutyTTLTicks + time.Second,
	})
	if err == nil {
		e.Stop(context.Background())
		t.Fatal("New accepted a poll interval whose waiter duty no backend grants")
	}
	if !errors.Is(err, coord.ErrTTLTooLong) {
		t.Fatalf("New = %v, want an error wrapping coord.ErrTTLTooLong", err)
	}
}

// EVERY SEAT A NODE TAKES IS ANNOUNCED, on a node that runs no sandbox as on
// one that does.
//
// The announcement sat behind the sandbox half of the seat's preparation, whose
// first line returned on a node with no coordinator — so on the ordinary node
// no seat was ever announced, and each stayed on whatever state its last owner
// left behind (`terminated`, `offline`) until it happened to do some work.
func TestASeatIsAnnouncedOnANodeThatRunsNoSandbox(t *testing.T) {
	t.Parallel()
	e := newSandboxNode(t, parseCompany(t, companyWithoutSandboxDoc))
	spawned := make(chan string, 8)
	if err := e.backends.Queue.Subscribe(t.Context(),
		topics.Event(types.AgentSpawned{}.EventType()), "spawn-probe",
		func(_ context.Context, ev *events.Event) queue.Result {
			spawned <- ev.Source
			return queue.Ack()
		}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := e.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitHeld(t, e, "swe")
	if e.sandbox.Load() != nil {
		t.Fatal("the premise: this node runs no sandbox")
	}
	select {
	case role := <-spawned:
		if role != "SWE" {
			t.Errorf("the announcement names role %q, want the seat's SWE", role)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the seat was taken and never announced")
	}
}

// sandboxNode is a started engine over a fresh store, on company (nil for none),
// polling its runs quickly.
func sandboxNode(t *testing.T, company *config.Company) *Engine {
	t.Helper()
	e := newSandboxNode(t, company)
	if err := e.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return e
}

// newSandboxNode is [sandboxNode] before its Start, for a case that has to be
// listening before the node takes a seat.
func newSandboxNode(t *testing.T, company *config.Company) *Engine {
	t.Helper()
	b := config.DefaultBootstrap()
	dir := t.TempDir()
	b.Store.Path = filepath.Join(dir, "crewlet.db")
	b.Stream.StoreDir = filepath.Join(dir, "stream")
	e, err := New(t.Context(), Options{
		Bootstrap: &b, Company: company,
		ActivatedAt:         time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC),
		SandboxPollInterval: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })
	return e
}

// sandboxDoc is [sandboxCompanyDoc] with extra lines in its providers.sandbox
// block.
func sandboxDoc(extra string) string {
	return fmt.Sprintf(sandboxCompanyDoc, extra)
}

func parseCompany(t *testing.T, doc string) *config.Company {
	t.Helper()
	c, err := config.ParseCompany([]byte(doc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return c
}

// applyOK applies a revision that must be served, and returns the stages it
// went through.
func applyOK(t *testing.T, e *Engine, doc string) []string {
	t.Helper()
	status, applied, err := e.Apply(t.Context(), parseCompany(t, doc),
		time.Now().UTC())
	if err != nil || status != configplane.StatusOK {
		t.Fatalf("Apply = (%s, %v, %v), want ok", status, applied, err)
	}
	return applied
}

// seedRunningRun records a running coding job on the swe seat, as the node that
// launched it would: no command id, so the job never reports itself done and
// every tick of the poll is a keepalive.
func seedRunningRun(t *testing.T, e *Engine, turnID, sandboxID string) {
	t.Helper()
	store := sandbox.NewCoordStore(e.backends.Fleet)
	ctx := t.Context()
	if err := store.BeginLaunch(ctx, sandbox.PendingRun{
		TurnID: turnID, AgentHandle: "swe", Role: "SWE", CodingAgent: "claude-code",
		CreatedAt: time.Now().UTC(),
	}, sandbox.Fence{}); err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	if err := store.AttachSandbox(ctx, turnID, sandbox.BoxRef{
		SandboxID: sandboxID, CodingAgent: "claude-code",
	}, sandbox.Fence{}); err != nil {
		t.Fatalf("AttachSandbox: %v", err)
	}
	suspended, err := store.MarkSuspended(ctx, turnID, map[string]any{
		"version": float64(1), "pending_tool_call_id": "call-1",
		"pending_tool_name": builtin.RunSandboxTool,
	})
	if err != nil || !suspended {
		t.Fatalf("MarkSuspended = (%v, %v), want the run open to the poll", suspended, err)
	}
}

// createFakeBox mints a box on the runtime's current direct backend, which is
// the in-process double.
func createFakeBox(t *testing.T, rt *sandboxRuntime) *sandbox.FakeSandbox {
	t.Helper()
	provider, err := rt.coordinator.Manager().Provider(sandbox.Direct)
	if err != nil {
		t.Fatalf("the direct cell: %v", err)
	}
	fake, ok := provider.(*sandbox.FakeProvider)
	if !ok {
		t.Fatalf("the direct cell is %T, not the double", provider)
	}
	box, err := fake.Create(t.Context(), sandbox.Spec{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return fake.Box(box.ID())
}

// waitHeld waits for this node to take a seat and finish establishing it —
// not merely to win its lease, which [seat.Host.Held] reports while the
// acquisition hook is still running.
func waitHeld(t *testing.T, e *Engine, handle string) {
	t.Helper()
	eventually(t, "the seat "+handle+" to be established here", func() bool {
		_, ok := e.node.Host().MayStart(handle)
		return ok
	})
}

// eventually polls a condition for up to twenty seconds.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
