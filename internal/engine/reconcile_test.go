package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/configplane"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/store"
)

// pinnedNow is the clock the reconcile tests run on. Pinned because the
// freshness window that decides whether a peer's status is evidence is
// measured against it, and a suite on the real clock would assert about that
// window by sleeping through it.
var pinnedNow = time.Date(2026, 8, 23, 14, 0, 0, 0, time.UTC)

// brokenRevision is well-formed JSON that cannot be built: a seat naming a
// provider the document does not configure. The provider block is non-empty
// deliberately — a company with no models at all is a supported authoring
// state, so an empty one would exercise a different refusal.
var brokenRevision = json.RawMessage(`{"name":"Acme",
  "providers":{"llm":{"zulu":{"type":"anthropic","model":"m","api_keys":["k"]}}},
  "roles":[{"name":"CEO","handle":"ceo","llm":"nonexistent"}]}`)

// A second company, differing from companyDoc in the one way a reconcile has
// to be visible through: its seat set.
const grownCompanyDoc = `
name: Acme
providers:
  llm:
    zulu:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["${K}"]
roles:
  - name: CEO
    handle: ceo
    llm: zulu
  - name: CTO
    handle: cto
    llm: zulu
  - name: Designer
    handle: designer
    llm: zulu
`

// plane is one engine, one store, and the reconciler between them.
type plane struct {
	engine  *engine.Engine
	store   *store.DB
	fleet   coord.Plane
	recon   *engine.Reconciler
	applies []applied
}

type applied struct {
	epoch  int64
	status configplane.ApplyStatus
}

func newPlane(t *testing.T, opts ...func(*engine.ReconcilerOptions)) *plane {
	t.Helper()
	e := newEngine(t, engine.Options{})
	p := &plane{engine: e, store: e.Backends().Store, fleet: e.Backends().Fleet}

	options := engine.ReconcilerOptions{
		Store:  p.store,
		Fleet:  p.fleet,
		Queue:  e.Backends().Queue,
		NodeID: "node-a",
		Now:    func() time.Time { return pinnedNow },
	}
	options.OnApply = func(epoch int64, status configplane.ApplyStatus) {
		p.applies = append(p.applies, applied{epoch, status})
	}
	for _, opt := range opts {
		opt(&options)
	}
	r, err := e.NewReconciler(options)
	if err != nil {
		t.Fatalf("NewReconciler: %v", err)
	}
	p.recon = r
	return p
}

// activate writes a revision and points the fleet at it, returning its epoch.
//
// TWO STORES, which is the shape the engine itself now has: the payload goes
// in the node's database, the pointer in the coordination store.
func (p *plane) activate(t *testing.T, doc string) int64 {
	t.Helper()
	document := yamlToJSON(t, doc)
	id, err := p.store.Configs().InsertActive(t.Context(), store.Revision{
		Source: "test", CreatedBy: "operator", Summary: "revision",
		Payload: document, CreatedAt: pinnedNow,
	})
	if err != nil {
		t.Fatalf("store the revision: %v", err)
	}
	published, err := p.fleet.Activate(t.Context(), coord.ActivationRequest{RevisionID: id, Summary: "revision", Payload: document, At: pinnedNow})
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	return published.Epoch
}

// activatePayload is activate for a document that is not a company — a broken
// one, or a sealed one. Same two writes, because a payload the fleet does not
// point at is a payload no reconciler will ever read.
func (p *plane) activatePayload(t *testing.T, summary string, payload json.RawMessage) int64 {
	t.Helper()
	id, err := p.store.Configs().InsertActive(t.Context(), store.Revision{
		Source: "test", CreatedBy: "operator", Summary: summary,
		Payload: payload, CreatedAt: pinnedNow,
	})
	if err != nil {
		t.Fatalf("store the revision: %v", err)
	}
	published, err := p.fleet.Activate(t.Context(), coord.ActivationRequest{RevisionID: id, Summary: summary, Payload: payload, At: pinnedNow})
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	return published.Epoch
}

// yamlToJSON stores a company the way an import does: parse the authored
// document once, and store its JSON form, which is what the payload column
// holds and what every node reads from then on.
func yamlToJSON(t *testing.T, doc string) json.RawMessage {
	t.Helper()
	cfg, err := config.ParseCompany([]byte(doc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

func (p *plane) seats(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, s := range p.engine.Company().Seats() {
		out = append(out, s.Handle)
	}
	return out
}

func (p *plane) fleetRow(t *testing.T) coord.NodeApply {
	t.Helper()
	rows, err := p.fleet.Fleet(t.Context())
	if err != nil {
		t.Fatalf("fleet: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("%d rows in the fleet view, want this node's one", len(rows))
	}
	return rows[0]
}

func TestANodeWithNoActivationAppliesNothing(t *testing.T) {
	t.Parallel()
	// A fresh deployment sits here until the first import. Not an error and
	// not a state to report: a node that recorded "error" for a company
	// nobody has configured would look broken on the operator's first look
	// at the fleet view.
	p := newPlane(t)
	if err := p.recon.Tick(t.Context()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if p.recon.Applied() != 0 {
		t.Errorf("applied epoch = %d, want 0", p.recon.Applied())
	}
	rows, err := p.fleet.Fleet(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("a node with nothing to apply reported %+v", rows)
	}
}

func TestANewRevisionReplacesTheEpoch(t *testing.T) {
	t.Parallel()
	p := newPlane(t)
	before := p.engine.Company()
	epoch := p.activate(t, grownCompanyDoc)

	if err := p.recon.Tick(t.Context()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if p.recon.Applied() != epoch {
		t.Fatalf("applied = %d, want %d", p.recon.Applied(), epoch)
	}
	// PUBLISHED, not mutated: the previous epoch is still intact, which is
	// what makes rollback a re-publish rather than an un-apply.
	if p.engine.Company() == before {
		t.Fatal("the epoch was mutated in place")
	}
	if got := len(before.Seats()); got != 2 {
		t.Errorf("the previous epoch changed under the apply: %d seats", got)
	}
	if got := p.seats(t); len(got) != 3 || got[0] != "ceo" {
		t.Errorf("seats = %v, want the three the new revision names", got)
	}
}

func TestTheOutcomeIsRecordedWhereEveryPeerReadsIt(t *testing.T) {
	t.Parallel()
	// Reading the pointer says where the fleet should be; this row says
	// where THIS node actually is. Only the two together tell "behind
	// because propagation takes a moment" from "behind because I cannot
	// apply this".
	p := newPlane(t)
	epoch := p.activate(t, grownCompanyDoc)
	if err := p.recon.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	row := p.fleetRow(t)
	if row.NodeID != "node-a" || row.Epoch != epoch {
		t.Fatalf("row = %+v, want node-a on epoch %d", row, epoch)
	}
	if configplane.ApplyStatus(row.Status) != configplane.StatusOK {
		t.Errorf("status = %q, want ok", row.Status)
	}
	if row.Error != "" {
		t.Errorf("a clean apply recorded an error: %q", row.Error)
	}
	if len(p.applies) != 1 || p.applies[0].status != configplane.StatusOK {
		t.Errorf("observers saw %+v", p.applies)
	}
}

func TestReactivatingAnUnchangedRevisionAppliesAgain(t *testing.T) {
	t.Parallel()
	// THE credential-rotation gesture. The payload is identical and the
	// point is that its ${VAR} references now resolve differently, so a
	// no-op check on the payload would rebuild nothing on exactly the
	// operation an operator performs to make it rebuild.
	p := newPlane(t)
	first := p.activate(t, grownCompanyDoc)
	if err := p.recon.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	afterFirst := p.engine.Company()

	active, found, err := p.store.Configs().Active(t.Context())
	if err != nil || !found {
		t.Fatalf("active: found=%v err=%v", found, err)
	}
	// RE-ACTIVATING the SAME revision, which is the case that matters: the
	// pointer is append-only, so it mints a new epoch every node is
	// watching — that is how a rotated secret reaches a running fleet.
	summary, err := p.store.Configs().Activate(t.Context(), active.ID, pinnedNow)
	if err != nil {
		t.Fatalf("re-activate: %v", err)
	}
	republished, err := p.fleet.Activate(t.Context(), coord.ActivationRequest{RevisionID: active.ID, Summary: summary, Payload: active.Payload, At: pinnedNow})
	if err != nil {
		t.Fatalf("re-publish: %v", err)
	}
	second := republished.Epoch
	if second <= first {
		t.Fatalf("the pointer did not move: %d then %d", first, second)
	}
	if err := p.recon.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if p.recon.Applied() != second {
		t.Fatalf("applied = %d, want the re-activation's epoch %d", p.recon.Applied(), second)
	}
	if p.engine.Company() == afterFirst {
		t.Fatal("re-activation reused the previous epoch, so nothing re-resolved")
	}
}

func TestAnAlreadyAppliedEpochIsNotReapplied(t *testing.T) {
	t.Parallel()
	// The tick runs every 15 seconds for the life of the node. Rebuilding
	// the epoch on each one would restart every subsystem an apply touches,
	// four times a minute, forever.
	p := newPlane(t)
	p.activate(t, grownCompanyDoc)
	for range 4 {
		if err := p.recon.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if len(p.applies) != 1 {
		t.Fatalf("%d applies for one activation, want 1", len(p.applies))
	}
}

func TestARevisionThatCannotBeBuiltLeavesTheNodeServing(t *testing.T) {
	t.Parallel()
	// error, not degraded: the build touches nothing, so this node still
	// serves the PRIOR epoch correctly. That is a legitimate
	// degraded-but-correct state and safe to route work to, which is
	// exactly what the distinction is for.
	p := newPlane(t)
	before := p.engine.Company()
	// A seat naming a provider the document does not configure:
	// well-formed JSON, and refused at build. The provider block is
	// non-empty on purpose — a company with NO models is a documented
	// authoring state, so an empty one would be a different fault from
	// the one under test.
	p.activatePayload(t, "broken", brokenRevision)
	err := p.recon.Tick(t.Context())
	if err == nil {
		t.Fatal("a revision that cannot be built applied cleanly")
	}
	if p.engine.Company() != before {
		t.Fatal("a refused revision still replaced the epoch")
	}
	if p.recon.Applied() != 0 {
		t.Errorf("applied = %d, want 0 — nothing was applied", p.recon.Applied())
	}
	row := p.fleetRow(t)
	if configplane.ApplyStatus(row.Status) != configplane.StatusError {
		t.Errorf("status = %q, want error", row.Status)
	}
	if !strings.Contains(row.Error, "nonexistent") {
		t.Errorf("the recorded reason does not name the fault: %q", row.Error)
	}
}

func TestAPointerNamingAMissingRevisionIsReported(t *testing.T) {
	t.Parallel()
	// A fleet where every node quietly ignores an unreadable pointer
	// converges on nothing while reporting convergence.
	p := newPlane(t)
	// No payload with it either: a pointer whose revision is in neither
	// store is exactly the ghost this reports.
	if _, err := p.fleet.Activate(t.Context(), coord.ActivationRequest{
		RevisionID: "00000000-0000-0000-0000-000000000000",
		Summary:    "ghost", At: pinnedNow}); err != nil {
		t.Fatal(err)
	}
	if err := p.recon.Tick(t.Context()); err == nil {
		t.Fatal("a pointer naming nothing applied cleanly")
	}
	if row := p.fleetRow(t); configplane.ApplyStatus(row.Status) != configplane.StatusError {
		t.Errorf("status = %q, want error", row.Status)
	}
}

func TestOneEpochIsRetriedABoundedNumberOfTimes(t *testing.T) {
	t.Parallel()
	// Per epoch, not per node lifetime — so re-activating a FIXED revision
	// resets the budget and the runbook's fix actually works. Without the
	// bound a bad revision has this node rebuilding its subsystems every
	// fifteen seconds until somebody notices.
	p := newPlane(t)
	p.activatePayload(t, "broken", brokenRevision)
	for range 10 {
		_ = p.recon.Tick(t.Context())
	}
	if len(p.applies) != configplane.MaxApplyAttempts {
		t.Fatalf("%d attempts at one bad epoch, want %d",
			len(p.applies), configplane.MaxApplyAttempts)
	}

	// The fix: activate a revision that works. The budget resets because
	// the target moved.
	p.activate(t, grownCompanyDoc)
	if err := p.recon.Tick(t.Context()); err != nil {
		t.Fatalf("the fixed revision was refused: %v", err)
	}
	if got := p.seats(t); len(got) != 3 {
		t.Errorf("seats = %v, want the fixed revision's three", got)
	}
}

func TestASealedRevisionNeedsItsKeyring(t *testing.T) {
	t.Parallel()
	cipher, err := secrets.NewCipher(secrets.Keyring{
		ActiveID: "k1", Keys: map[string][]byte{"k1": testKey(t)},
	})
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := secrets.Seal(cipher, yamlToJSON(t, grownCompanyDoc))
	if err != nil {
		t.Fatal(err)
	}

	// Without the keyring: an ERROR, never an empty company. Booting onto
	// nothing reads on every surface as an operator who configured nothing,
	// where the actual fault is a deployment that lost its root of trust.
	blind := newPlane(t)
	blind.activatePayload(t, "sealed", sealed)
	err = blind.recon.Tick(t.Context())
	if !errors.Is(err, secrets.ErrSealedWithoutKey) {
		t.Fatalf("err = %v, want the sealed-without-key error", err)
	}

	// With it: applied.
	keyed := newPlane(t, func(o *engine.ReconcilerOptions) { o.Cipher = cipher })
	keyed.activatePayload(t, "sealed", sealed)
	if err := keyed.recon.Tick(t.Context()); err != nil {
		t.Fatalf("a sealed revision with its keyring: %v", err)
	}
	if got := keyed.seats(t); len(got) != 3 {
		t.Errorf("seats = %v, want the sealed revision's three", got)
	}
}

func testKey(t *testing.T) []byte {
	t.Helper()
	key, err := secrets.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func TestAReconcilerNeedsAStoreAndAnIdentity(t *testing.T) {
	t.Parallel()
	// The apply-status table upserts on node id, so an empty one makes
	// every node that forgot it share a row — and the fleet view shows one
	// anonymous entry where a dozen nodes should be.
	e := newEngine(t, engine.Options{})
	if _, err := e.NewReconciler(engine.ReconcilerOptions{NodeID: "n"}); !errors.Is(err, engine.ErrNoStore) {
		t.Errorf("err = %v, want ErrNoStore", err)
	}
	if _, err := e.NewReconciler(engine.ReconcilerOptions{
		Store: e.Backends().Store, Fleet: e.Backends().Fleet,
	}); !errors.Is(err, engine.ErrNoPublisher) {
		t.Errorf("err = %v, want ErrNoPublisher", err)
	}
	if _, err := e.NewReconciler(engine.ReconcilerOptions{
		Store: e.Backends().Store, Fleet: e.Backends().Fleet, Queue: e.Backends().Queue,
	}); err == nil {
		t.Error("a reconciler with no node id was built")
	}
}

// --- posture ---------------------------------------------------------------

func TestACurrentNodeServes(t *testing.T) {
	t.Parallel()
	p := newPlane(t)
	p.activate(t, grownCompanyDoc)
	if err := p.recon.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := p.recon.Posture(t.Context()); got != configplane.PostureServe {
		t.Errorf("posture = %q, want serve", got)
	}
}

func TestAnUnconfiguredNodeIsNotDiverged(t *testing.T) {
	t.Parallel()
	// No activation at all means nothing to converge on. Reporting a
	// posture other than serve would take every node of a brand-new
	// deployment out of rotation before its first import.
	p := newPlane(t)
	if got := p.recon.Posture(t.Context()); got != configplane.PostureServe {
		t.Errorf("posture = %q, want serve", got)
	}
}

func TestOrdinaryPropagationLagIsNotDivergence(t *testing.T) {
	t.Parallel()
	// EVERY successful rollout produces lag: the first node to apply
	// advances the pointer and every peer is behind until it polls.
	// Shedding on that makes the fastest node the cause of a fleet-wide
	// outage, and the faster it is the longer the outage.
	p := newPlane(t)
	p.activate(t, grownCompanyDoc)
	// The pointer has moved and this node has not ticked.
	if got := p.recon.Posture(t.Context()); got != configplane.PostureWait {
		t.Errorf("posture = %q, want wait", got)
	}
}

func TestAConfirmedLaggardShedsOnlyWhenAPeerHasTheEpoch(t *testing.T) {
	t.Parallel()
	// Shedding exists to move work to a healthy peer. With no healthy peer
	// it is not shedding, it is stopping — so the same lag reads as
	// isolated, and the node keeps serving what it has.
	p := newPlane(t)
	// The node tries and fails, which is what makes its lag CONFIRMED
	// rather than ordinary propagation — a distinction the posture rule
	// rests on, because every successful rollout produces lag.
	p.activatePayload(t, "broken", brokenRevision)
	_ = p.recon.Tick(t.Context())

	if got := p.recon.Posture(t.Context()); got != configplane.PostureIsolated {
		t.Errorf("posture = %q, want isolated — nobody applied this epoch", got)
	}

	// A peer reports the epoch applied cleanly, recently.
	target, _, err := p.fleet.Target(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := p.fleet.RecordApply(t.Context(), coord.NodeApply{
		NodeID: "node-b", Epoch: target.Epoch, Status: string(configplane.StatusOK),
		UpdatedAt: pinnedNow,
	}); err != nil {
		t.Fatal(err)
	}
	if got := p.recon.Posture(t.Context()); got != configplane.PostureShed {
		t.Errorf("posture = %q, want shed — a healthy peer has the epoch", got)
	}
}

func TestAStalePeerIsNotEvidence(t *testing.T) {
	t.Parallel()
	// A node that was scaled in, redeployed or crashed leaves its `ok`
	// behind forever. Counting that ghost makes a diverged survivor shed
	// its seats to a node that no longer exists — the company goes dark
	// exactly where it should have gone degraded and raised an alarm.
	p := newPlane(t)
	p.activatePayload(t, "broken", brokenRevision)
	_ = p.recon.Tick(t.Context())
	target, _, err := p.fleet.Target(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := p.fleet.RecordApply(t.Context(), coord.NodeApply{
		NodeID: "ghost", Epoch: target.Epoch, Status: string(configplane.StatusOK),
		UpdatedAt: pinnedNow.Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if got := p.recon.Posture(t.Context()); got != configplane.PostureIsolated {
		t.Errorf("posture = %q, want isolated — the only healthy peer is a ghost row", got)
	}
}

func TestAnUnreadableControlPlaneKeepsTheNodeServing(t *testing.T) {
	t.Parallel()
	// The safe answer to "am I behind?" is the one that keeps a working
	// company working: the alternative takes every node out of rotation on
	// a database blip.
	p := newPlane(t)
	p.activate(t, grownCompanyDoc)
	if err := p.recon.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := p.store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := p.recon.Posture(t.Context()); got != configplane.PostureServe {
		t.Errorf("posture = %q, want serve", got)
	}
}

func TestTheLoopStopsWithItsContext(t *testing.T) {
	t.Parallel()
	p := newPlane(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); p.recon.Run(ctx) }()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the reconcile loop outlived its context")
	}
}

func TestTheNodeSeesTheNewEpochsSeats(t *testing.T) {
	t.Parallel()
	// The seat set the HOST reads must follow the epoch, not the company
	// this engine booted on. A method value captured at construction keeps
	// claiming seats a deleted role no longer has and never claims a new
	// one — and the failure is invisible, because a node reading a stale
	// seat set looks exactly like a node losing every race.
	p := newPlane(t)
	host := p.engine.Node().Host()
	if got := len(host.CompanySeats()); got != 2 {
		t.Fatalf("the host starts with %d seats, want the boot company's 2", got)
	}
	p.activate(t, grownCompanyDoc)
	if err := p.recon.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	var handles []string
	for _, seat := range host.CompanySeats() {
		handles = append(handles, seat.Handle)
	}
	if len(handles) != 3 {
		t.Fatalf("the host sees %v, want the new epoch's three seats", handles)
	}
	if !slices.Equal(handles, []string{"ceo", "cto", "designer"}) {
		t.Errorf("seats = %v, want the new epoch's three, sorted", handles)
	}
}

// The post-mortem trail. The coordination record this accompanies is one key
// per node in a bucket whose age is sixty seconds, by design — so the epoch,
// status and error text of a node that crashed or was scaled in during a bad
// rollout are gone a minute later, which is exactly the node an incident
// review is looking for. The event is what outlives it, and until this it was
// a registered type with a topic and NO PUBLISHER anywhere in the tree.
func TestAnApplyLeavesADurableTrail(t *testing.T) {
	t.Parallel()
	p := newPlane(t)
	applies := subscribeApplied(t, p)

	epoch := p.activate(t, companyDoc)
	if err := p.recon.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := p.recon.Applied(); got != epoch {
		t.Fatalf("applied epoch = %d, want %d", got, epoch)
	}

	ev := waitForApplied(t, applies)
	if ev.Source != "node-a" {
		t.Errorf("source = %q, want the node id — the payload deliberately does not repeat it", ev.Source)
	}
	got := appliedPayload(t, ev)
	if got.Status != types.ApplyOK {
		t.Errorf("status = %q, want %q", got.Status, types.ApplyOK)
	}
	if got.Error != "" {
		t.Errorf("error = %q on a successful apply", got.Error)
	}
	// The subsystems this apply got through, in the order it went through
	// them. On a failure it is what was already mutated when the refusal
	// happened, which is the whole of what makes a degraded apply
	// diagnosable after the fact.
	if len(got.AppliedSubsystems) == 0 {
		t.Fatal("applied_subsystems is empty on a successful apply")
	}
	if got.AppliedSubsystems[0] != "secrets" ||
		got.AppliedSubsystems[len(got.AppliedSubsystems)-1] != "scheduler" {
		t.Errorf("subsystems = %v, want the apply's own order from secrets to scheduler",
			got.AppliedSubsystems)
	}
	if !slices.Contains(got.AppliedSubsystems, "epoch") {
		t.Errorf("subsystems = %v, want the epoch publish among them", got.AppliedSubsystems)
	}
}

// A refused revision is the case the trail exists for, and the subsystem list
// is what says how far it got before the refusal.
func TestARefusedApplySaysHowFarItGot(t *testing.T) {
	t.Parallel()
	p := newPlane(t)
	applies := subscribeApplied(t, p)

	// A payload that DECODES as a company — the stored-form reader is
	// lenient, so a rolling upgrade can boot on a newer peer's revision —
	// and is refused when the company is BUILT, because the seat's model
	// names a provider the revision does not configure.
	p.activatePayload(t, "broken", brokenRevision)
	if err := p.recon.Tick(t.Context()); err == nil {
		t.Fatal("a revision naming an unconfigured provider was applied")
	}

	got := appliedPayload(t, waitForApplied(t, applies))
	if got.Status != types.ApplyError {
		t.Errorf("status = %q, want %q", got.Status, types.ApplyError)
	}
	if got.Error == "" {
		t.Error("a failed apply published no error text — there is nothing to review")
	}
	// Refused before the engine was asked to apply anything, so NOTHING on
	// this node was mutated — which is the difference between a node that
	// is serving its previous epoch correctly and one that is degraded.
	if len(got.AppliedSubsystems) != 0 {
		t.Errorf("subsystems = %v, want none — the revision never reached Apply",
			got.AppliedSubsystems)
	}
}

// And the partial list, at its own seam. An apply that fails PART WAY has
// already mutated everything before the failure, and the list is the only
// record of how much — the coordination status carries a status and an error
// string and nothing else.
func TestApplyReportsHowFarItGotBeforeARefusal(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{})
	status, applied, err := e.Apply(t.Context(), &config.Company{})
	if err == nil {
		t.Fatal("a company with no name was applied")
	}
	if status != configplane.StatusError {
		t.Errorf("status = %q, want %q", status, configplane.StatusError)
	}
	// The secret snapshot is taken FIRST — it is what makes re-activating
	// an unchanged revision pick up a rotated credential — so it is the one
	// step that ran before the company itself was refused.
	if !slices.Equal(applied, []string{"secrets"}) {
		t.Errorf("applied = %v, want just the step that ran before the refusal", applied)
	}
}

// appliedPayload reads the typed payload back off the envelope.
func appliedPayload(t *testing.T, ev *events.Event) types.ConfigRevisionApplied {
	t.Helper()
	got, ok := ev.Data.(*types.ConfigRevisionApplied)
	if !ok {
		t.Fatalf("payload is %T, want *types.ConfigRevisionApplied", ev.Data)
	}
	return *got
}

// subscribeApplied attaches to the apply topic before anything is activated.
func subscribeApplied(t *testing.T, p *plane) <-chan *events.Event {
	t.Helper()
	got := make(chan *events.Event, 4)
	if err := p.engine.Backends().Queue.Subscribe(t.Context(),
		topics.ConfigRevisionApplied, "apply-trail",
		func(_ context.Context, e *events.Event) queue.Result {
			select {
			case got <- e:
			default:
			}
			return queue.Ack()
		}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	return got
}

func waitForApplied(t *testing.T, ch <-chan *events.Event) *events.Event {
	t.Helper()
	select {
	case ev := <-ch:
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("no config_revision_applied event was published")
		return nil
	}
}

// THE PAYLOAD HAS TO TRAVEL WITH THE POINTER. A revision is written to the
// database of whichever node served the write, and every other node meets it
// for the first time when the pointer names it. While the body lived only in
// that one node's file, a peer read nothing and reported "no such revision"
// once per reconcile tick — for the life of the deployment. A live config
// change reached exactly the node it was posted to, which on a fleet is the
// whole feature.
func TestAPeerConvergesOnARevisionItHasNeverSeen(t *testing.T) {
	t.Parallel()
	author := newPlane(t)
	// A second node: its own database, the same coordination store. That is
	// the shape of a fleet, and the only thing the two share.
	peer := newPlane(t, func(o *engine.ReconcilerOptions) { o.Fleet = author.fleet })

	epoch := author.activate(t, grownCompanyDoc)
	target, found, err := author.fleet.Target(t.Context())
	if err != nil || !found {
		t.Fatalf("Target: found=%v err=%v", found, err)
	}
	if _, held, err := peer.store.Configs().Get(t.Context(), target.RevisionID); err != nil || held {
		t.Fatalf("the peer already holds the revision (held=%v err=%v) — this test proves nothing", held, err)
	}

	if err := peer.recon.Tick(t.Context()); err != nil {
		t.Fatalf("the peer could not converge on a revision it had never seen: %v", err)
	}
	if got := peer.recon.Applied(); got != epoch {
		t.Errorf("the peer applied epoch %d, want %d", got, epoch)
	}
	// And it KEPT the revision. company_config is where this node's own
	// history, diffs and revert targets are read from, so a node that
	// applied without adopting would serve an epoch its operator surface
	// cannot show.
	adopted, held, err := peer.store.Configs().Get(t.Context(), target.RevisionID)
	if err != nil || !held {
		t.Fatalf("the peer did not keep its own copy (held=%v err=%v)", held, err)
	}
	if !adopted.Active {
		t.Error("the adopted revision is not the peer's active one")
	}
	// The seats of the revision it converged on, not the one it booted with.
	if seats := peer.seats(t); !slices.Contains(seats, "designer") {
		t.Errorf("the peer's seats are %v, want the revision it converged on", seats)
	}
}

// The nudge, end to end. The reconcile interval is fifteen seconds; an
// operator's config change has to land in milliseconds, and the only thing
// that makes that true is a node hearing the activation and running its tick
// early. Until this the event type was registered with a topic and NOTHING
// anywhere published or consumed it, so every node waited out its poll.
func TestAnActivationNudgeWakesTheLoop(t *testing.T) {
	t.Parallel()
	p := newPlane(t)
	if err := p.engine.Backends().Queue.Start(t.Context()); err != nil {
		t.Fatalf("queue start: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); p.recon.Run(ctx) }()

	// Activated AFTER the loop is running, so the only thing that can make
	// it converge inside the window below is the nudge — a poll is a full
	// interval away.
	epoch := p.activate(t, grownCompanyDoc)
	ev := events.New(types.ConfigRevisionActivated{RevisionID: "any", RevisionSummary: "x"},
		events.NewTrace())
	// Bounded well under the reconcile interval: only the nudge can get the
	// node there in this window.
	deadline := time.Now().Add(configplane.ReconcileInterval / 2)
	for p.recon.Applied() != epoch {
		if time.Now().After(deadline) {
			t.Fatal("the node did not converge before its next poll could have " +
				"run — the nudge is not waking the loop")
		}
		// Republished each round: the loop subscribes inside Run, so a
		// single publish racing the attach would flake.
		_ = p.engine.Backends().Queue.Publish(ctx, topics.ConfigRevisionActivated, ev)
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	<-done
}
