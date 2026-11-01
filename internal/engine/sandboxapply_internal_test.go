package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/configplane"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
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
`

// companyWithoutSandboxDoc is the same company with nothing that runs code.
const companyWithoutSandboxDoc = `
name: Nimbus
roles:
  - name: SWE
    handle: swe
`

// A NODE THAT BOOTED WITH NO COMPANY BRINGS THE CODE SANDBOX UP WITH THE FIRST
// ONE THAT CONFIGURES IT — the coordinator, the completion poll, and the seat's
// own preparation — with no restart.
//
// The coordinator was built at boot or never, so a node started the way the
// quickstart starts one served a company with no executor runtime until its
// process restarted, and a run a peer had left on one of its seats was
// polled, collected and resumed by nobody here.
func TestAFirstCompanyWithASandboxBringsTheCoordinatorUpWithoutARestart(t *testing.T) {
	t.Parallel()
	e := sandboxNode(t, nil)
	if e.sandbox.Load() != nil {
		t.Fatal("the premise: a node with no company runs no sandbox")
	}
	// A RUN LEFT ON THE SEAT, still going, before this node has a company
	// at all — this node's own, from before a restart, since a box in the
	// direct cell is polled by the node that launched it: its recovery is
	// part of what the apply has to bring.
	seedRunningRun(t, e, "wk-before", "box-1")

	status, applied, err := e.Apply(t.Context(), parseCompany(t, sandboxDoc("")), time.Now())
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
	// TAKEN BACK BY A PASS RUN HERE rather than the sweep's next tick: the
	// release changed no seat set, so nothing asks for one, and what is
	// under test is the acquisition, not the sweep's cadence.
	e.node.Host().Sweep(t.Context())
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
	// only to finish what that catalogue started, so an executor pinned to
	// the revision without one is refused rather than given a box.
	current := e.Company()
	seat := current.Org.AgentSeatByHandle("swe")
	executor, _ := executorCompany(t)
	pinned := &Company{Config: current.Config, Org: current.Org, Models: executor.Models}
	seat.LLM = executor.Org.Roles[0].LLM
	l := launcherOn(e, pinned, seat, "t-after")
	if _, err := l.LaunchExecutor(t.Context(), launchRequest(l, nil)); err == nil ||
		!strings.Contains(err.Error(), "providers.sandbox") {
		t.Errorf("an executor launched for a company with no providers.sandbox (err %v), "+
			"on a catalogue it no longer configures", err)
	}
}

// A CATALOGUE THAT CANNOT BE BUILT IS REFUSED ON A NODE THAT RUNS NO SANDBOX
// YET, as on one that does — before anything changes.
//
// It was built only where a coordinator already existed, so on every other
// node a revision whose providers.sandbox could never mint a box was published,
// and every seat's executor launched into a catalogue that had none.
func TestABrokenSandboxCatalogueIsRefusedOnANodeWithoutOne(t *testing.T) {
	t.Parallel()
	e := sandboxNode(t, nil)
	broken := e2bExecutorDoc(t, "${CREWLET_TEST_E2B_KEY_NEVER_SET}")
	status, applied, err := e.Apply(t.Context(), parseCompany(t, broken), time.Now())
	if err == nil || status != configplane.StatusError {
		t.Fatalf("Apply = (%s, %v, %v), want a refusal", status, applied, err)
	}
	if e.Company() != nil || e.sandbox.Load() != nil {
		t.Error("the unbuildable catalogue was installed or started a runtime")
	}
	// THE TRAIL NAMES THE COMPANY IT BUILT and nothing past it, so a refusal
	// after the company is told from one of the company itself.
	if want := []string{"secrets", "company"}; !slices.Equal(applied, want) {
		t.Errorf("a refused catalogue reported %v, want %v", applied, want)
	}
}

// THE CHECK A WRITE ASKS BEFORE IT LANDS A REVISION IS THE APPLY'S OWN BUILD,
// over this node's own chain, and it changes nothing.
//
// `crewlet run` holds the Tier B file it imports to [Engine.Buildable] before
// the file is stored or activated, because nothing else builds it until an
// apply does, after the whole fleet has been pointed at it. A check that built
// less than an apply — the company, without the sandbox catalogue — passed
// exactly the file whose every apply is refused; one that resolved from the
// environment alone refused a key the node keeps in its secret store.
//
// Mutations: drop the catalogue from [buildRevision] and the unset key builds;
// resolve from the environment alone and the stored key does not.
func TestBuildableIsTheApplysBuildOverThisNodesChain(t *testing.T) {
	t.Parallel()
	company := parseCompany(t, e2bExecutorDoc(t, "${CREWLET_TEST_E2B_KEY}"))
	// AN EMPTY ENVIRONMENT, so the process's own can answer nothing here.
	e := &Engine{environ: config.MapSource{}}
	e.installSecrets(map[string]string{})
	if err := e.Buildable(company); err == nil {
		t.Error("a revision whose sandbox catalogue cannot be built was buildable")
	}
	e.installSecrets(map[string]string{"CREWLET_TEST_E2B_KEY": "e2b_test_key"})
	if err := e.Buildable(company); err != nil {
		t.Errorf("a revision whose key this node's secret store holds was refused: %v", err)
	}
	if e.Company() != nil || e.sandbox.Load() != nil {
		t.Error("checking a revision installed it or started a sandbox runtime")
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
	b := testBootstrap(t)
	dir := t.TempDir()
	b.Store.Path = filepath.Join(dir, "crewlet.db")
	brokertest.StoreIn(&b.Stream, filepath.Join(dir, "stream"))
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
	b := testBootstrap(t)
	dir := t.TempDir()
	b.Store.Path = filepath.Join(dir, "crewlet.db")
	brokertest.StoreIn(&b.Stream, filepath.Join(dir, "stream"))
	SeedStore(t, &b)
	e, err := New(t.Context(), Options{
		Bootstrap: &b, Company: company,
		SandboxPollInterval: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })
	return e
}

// e2bExecutorDoc is a one-seat company whose executor's entry runs its runs in
// the remote cell, keyed by key — the one cell whose backend a catalogue can
// fail to build for want of a credential.
func e2bExecutorDoc(t *testing.T, key string) string {
	t.Helper()
	return fmt.Sprintf(`
name: Nimbus
providers:
  llm:
    cc:
      agent: claude-code
      model: sonnet
      run_in: e2b
      state_dir: %q
      auth: {mode: api-key, api_key: "sk-ant-not-real"}
  sandbox:
    e2b:
      api_key: %q
roles:
  - name: SWE
    handle: swe
    llm: cc
`, t.TempDir(), key)
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
	status, applied, err := e.Apply(t.Context(), parseCompany(t, doc), time.Now())
	if err != nil || status != configplane.StatusOK {
		t.Fatalf("Apply = (%s, %v, %v), want ok", status, applied, err)
	}
	return applied
}

// seedRunningRun records a running executor on the swe seat, as e itself
// launched it — before a restart, say — in the direct cell: no command id, so
// the job never reports itself done and every tick of the poll is a keepalive.
//
// e's OWN, because a box in the direct cell is on the host of the node that
// launched it and is polled there alone (internal/sandbox, boxhost.go).
func seedRunningRun(t *testing.T, e *Engine, turnID, sandboxID string) {
	t.Helper()
	store := sandbox.NewCoordStore(e.backends.Fleet)
	ctx := t.Context()
	// THE SEAT'S ID, as every launch records it: what a reply handed back
	// is published to, the seat's inbox being named by it (ADR-0013).
	agentID, _ := org.DeriveAgentID("Nimbus", "swe")
	if _, err := store.BeginLaunch(ctx, sandbox.PendingRun{
		TurnID: turnID, AgentID: agentID.String(), AgentHandle: "swe", Role: "SWE",
		CodingAgent: "claude-code", CreatedAt: time.Now().UTC(), LaunchNode: e.id,
	}, sandbox.Fence{}); err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	if err := store.AttachSandbox(ctx, turnID, sandbox.BoxRef{
		SandboxID: sandboxID, CodingAgent: "claude-code", Placement: string(sandbox.Direct),
	}, sandbox.Fence{}); err != nil {
		t.Fatalf("AttachSandbox: %v", err)
	}
	suspended, err := store.MarkSuspended(ctx, turnID, sandbox.Suspension{State: json.RawMessage(
		`{"version":4,"iteration":1}`)})
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
