package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

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

// anOrigin is the record every activation a case publishes carries, written
// an hour before pinnedNow: the backends refuse an activation without one, and
// a case about something else has no author to say.
var anOrigin = coord.RevisionOrigin{
	Author: "maya", AuthorKind: string(store.AuthorOperator), Source: "api",
	CreatedAt: pinnedNow.Add(-time.Hour),
}

// brokenRevision is well-formed JSON that cannot be built: a seat naming a
// provider the document does not configure. The provider block is non-empty
// deliberately: a company with no models at all is a supported authoring
// state, and an empty one would not be refused at all (see nomodels_test.go).
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
	return planeFor(t, newEngine(t, engine.Options{}), opts...)
}

// planeFor puts the reconciler in front of an engine the caller built, for a
// case whose subject needs a company or a dispatcher of its own.
func planeFor(t *testing.T, e *engine.Engine, opts ...func(*engine.ReconcilerOptions)) *plane {
	t.Helper()
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
// activate stores a revision and points the fleet at it.
//
// TAKES THE CALLER'S CONTEXT rather than reaching for t.Context(). Most cases
// pass t.Context() and nothing changes, but the concurrency case below drives
// this from a goroutine under a cancellable context it also owns — and a
// writer that kept activating revisions after its own cancel would be writing
// past the window the readers are watching.
func (p *plane) activate(ctx context.Context, t *testing.T, doc string) int64 {
	t.Helper()
	document := yamlToJSON(t, doc)
	id, err := p.store.Configs().InsertActive(ctx, store.Revision{
		CreatedByKind: store.AuthorOperator,
		Source:        "test", CreatedBy: "operator", Summary: "revision",
		Payload: document, CreatedAt: pinnedNow,
	})
	if err != nil {
		t.Fatalf("store the revision: %v", err)
	}
	published, err := p.fleet.Activate(ctx, coord.ActivationRequest{RevisionID: id, Summary: "revision", Payload: document, At: pinnedNow, Origin: anOrigin})
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
		CreatedByKind: store.AuthorOperator,
		Source:        "test", CreatedBy: "operator", Summary: summary,
		Payload: payload, CreatedAt: pinnedNow,
	})
	if err != nil {
		t.Fatalf("store the revision: %v", err)
	}
	published, err := p.fleet.Activate(t.Context(), coord.ActivationRequest{RevisionID: id, Summary: summary, Payload: payload, At: pinnedNow, Origin: anOrigin})
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
	epoch := p.activate(t.Context(), t, grownCompanyDoc)

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
	epoch := p.activate(t.Context(), t, grownCompanyDoc)
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

// A CONVERGED NODE KEEPS SAYING SO.
//
// The apply-status row ages out after four reconcile intervals by design, so
// a node that stops reporting vanishes from the fleet view. It was written on
// an apply alone, so a node that converged stopped reporting a minute later:
// the fleet view drew it as having applied nothing while its own /health
// served the current epoch, and every peer dropped it as stale evidence.
func TestAConvergedNodeKeepsItsApplyStatusFresh(t *testing.T) {
	t.Parallel()
	now := pinnedNow
	p := newPlane(t, func(o *engine.ReconcilerOptions) {
		o.Now = func() time.Time { return now }
	})
	epoch := p.activate(t.Context(), t, grownCompanyDoc)
	if err := p.recon.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Longer than the row's freshness: without a refresh this is the row
	// every peer skips and the fleet view drops.
	now = pinnedNow.Add(2 * coord.StatusFreshness)
	if err := p.recon.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	row := p.fleetRow(t)
	if !row.UpdatedAt.Equal(now) {
		t.Errorf("row written at %v, want this tick's %v — a converged node stopped reporting",
			row.UpdatedAt, now)
	}
	if row.Epoch != epoch || configplane.ApplyStatus(row.Status) != configplane.StatusOK {
		t.Errorf("row = %+v, want ok on epoch %d", row, epoch)
	}
	// A refresh is not an apply: the epoch is built once.
	if len(p.applies) != 1 {
		t.Errorf("%d applies, want the one", len(p.applies))
	}
}

// AND A NODE THAT GAVE UP KEEPS SAYING WHY. Out of retries on an epoch, the
// failure it recorded is what its peers and the fleet view need; aged out, the
// node read as one that never tried.
func TestANodeOutOfRetriesKeepsItsFailureFresh(t *testing.T) {
	t.Parallel()
	now := pinnedNow
	p := newPlane(t, func(o *engine.ReconcilerOptions) {
		o.Now = func() time.Time { return now }
	})
	p.activatePayload(t, "broken", brokenRevision)
	for range configplane.MaxApplyAttempts {
		_ = p.recon.Tick(t.Context())
	}
	first := p.fleetRow(t)
	now = pinnedNow.Add(2 * coord.StatusFreshness)
	_ = p.recon.Tick(t.Context())
	row := p.fleetRow(t)
	if !row.UpdatedAt.Equal(now) {
		t.Errorf("row written at %v, want this tick's %v", row.UpdatedAt, now)
	}
	if configplane.ApplyStatus(row.Status) != configplane.StatusError || row.Error != first.Error {
		t.Errorf("row = %+v, want the recorded failure %q restated", row, first.Error)
	}
	if len(p.applies) != configplane.MaxApplyAttempts {
		t.Errorf("%d applies, want the %d attempts and no more", len(p.applies),
			configplane.MaxApplyAttempts)
	}
}

func TestReactivatingAnUnchangedRevisionAppliesAgain(t *testing.T) {
	t.Parallel()
	// THE credential-rotation gesture. The payload is identical and the
	// point is that its ${VAR} references now resolve differently, so a
	// no-op check on the payload would rebuild nothing on exactly the
	// operation an operator performs to make it rebuild.
	p := newPlane(t)
	first := p.activate(t.Context(), t, grownCompanyDoc)
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
	republished, err := p.fleet.Activate(t.Context(), coord.ActivationRequest{RevisionID: active.ID, Summary: summary, Payload: active.Payload, At: pinnedNow, Origin: anOrigin})
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
	p.activate(t.Context(), t, grownCompanyDoc)
	for range 4 {
		if err := p.recon.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if len(p.applies) != 1 {
		t.Fatalf("%d applies for one activation, want 1", len(p.applies))
	}
}

// RE-ACTIVATING THE REVISION THIS NODE HOLDS MOVES ITS ACTIVATION INSTANT.
//
// Re-activation is the credential-rotation gesture, and the local row's
// instant is the one this node boots its chart with and the one its config
// history shows. The early return for "already the fleet's revision" used to
// leave it at the first activation's.
func TestReactivatingTheHeldRevisionMovesItsLocalInstant(t *testing.T) {
	t.Parallel()
	p := newPlane(t)
	p.activate(t.Context(), t, grownCompanyDoc)
	if err := p.recon.Tick(t.Context()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	target, _, err := p.fleet.Target(t.Context())
	if err != nil {
		t.Fatalf("Target: %v", err)
	}
	later := pinnedNow.Add(time.Hour)
	if _, err := p.fleet.Activate(t.Context(), coord.ActivationRequest{
		RevisionID: target.RevisionID, Summary: "rotate", Payload: yamlToJSON(t, grownCompanyDoc),
		At:     later,
		Origin: anOrigin,
	}); err != nil {
		t.Fatalf("re-activate: %v", err)
	}
	if err := p.recon.Tick(t.Context()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	active, found, err := p.store.Configs().Active(t.Context())
	if err != nil || !found {
		t.Fatalf("active: found=%v err=%v", found, err)
	}
	if active.ID != target.RevisionID || !active.ActivatedAt.Equal(later) {
		t.Errorf("this node's active revision is %s activated at %s, want %s "+
			"activated at the re-activation's %s", active.ID, active.ActivatedAt,
			target.RevisionID, later)
	}
}

// THE FLEET'S REVISION IS THIS NODE'S ACTIVE ONE ONCE THE NODE RUNS IT.
//
// A node's active revision is what its GET /config serves, what it boots on,
// and what it offers the whole fleet at its next start whenever it is newer
// than the pointer. One that is not the fleet's, left there after the node
// applied the fleet's epoch, is a node serving a company the fleet is not
// running, and republishing it one restart later over the one that is.
func TestTheNodesActiveRevisionFollowsTheFleetOnceApplied(t *testing.T) {
	t.Parallel()
	activeID := func(t *testing.T, p *plane) string {
		t.Helper()
		active, found, err := p.store.Configs().Active(t.Context())
		if err != nil || !found {
			t.Fatalf("this node has no active revision (found=%v err=%v)", found, err)
		}
		return active.ID
	}

	// A revision marked active on this node AFTER it applied the fleet's:
	// a write that lost a race and marked itself anyway, or one whose
	// local activation landed after this node adopted a newer revision.
	t.Run("a stray revision marked active after the apply", func(t *testing.T) {
		t.Parallel()
		p := newPlane(t)
		p.activate(t.Context(), t, grownCompanyDoc)
		if err := p.recon.Tick(t.Context()); err != nil {
			t.Fatalf("tick: %v", err)
		}
		target, _, err := p.fleet.Target(t.Context())
		if err != nil {
			t.Fatalf("Target: %v", err)
		}
		stray, err := p.store.Configs().InsertActive(t.Context(), store.Revision{
			CreatedByKind: store.AuthorOperator,
			Source:        "test", CreatedBy: "operator", Summary: "stray",
			Payload: yamlToJSON(t, companyDoc), CreatedAt: pinnedNow.Add(time.Hour),
		})
		if err != nil {
			t.Fatalf("store the stray revision: %v", err)
		}
		if err := p.recon.Tick(t.Context()); err != nil {
			t.Fatalf("tick: %v", err)
		}
		if got := activeID(t, p); got != target.RevisionID {
			t.Errorf("this node's active revision is %s, want the fleet's %s rather than the stray %s",
				got, target.RevisionID, stray)
		}
		// Realigned, not re-applied: the epoch was already this node's.
		if len(p.applies) != 1 {
			t.Errorf("%d applies, want the one", len(p.applies))
		}
	})

	// A fleet revision this node HOLDS and never marked active: an API write
	// whose own local activation failed after the fleet took it.
	t.Run("a held revision the fleet took and this node never marked", func(t *testing.T) {
		t.Parallel()
		p := newPlane(t)
		previous, err := p.store.Configs().InsertActive(t.Context(), store.Revision{
			CreatedByKind: store.AuthorOperator,
			Source:        "test", CreatedBy: "operator", Summary: "before",
			Payload: yamlToJSON(t, companyDoc), CreatedAt: pinnedNow,
		})
		if err != nil {
			t.Fatalf("store the previous revision: %v", err)
		}
		document := yamlToJSON(t, grownCompanyDoc)
		held, err := p.store.Configs().Insert(t.Context(), store.Revision{
			CreatedByKind: store.AuthorOperator,
			ParentID:      previous, Source: "api", CreatedBy: "operator", Summary: "written",
			Payload: document, CreatedAt: pinnedNow,
		})
		if err != nil {
			t.Fatalf("store the written revision: %v", err)
		}
		if _, err := p.fleet.Activate(t.Context(), coord.ActivationRequest{
			RevisionID: held, Summary: "written", Payload: document, At: pinnedNow,
			Origin: anOrigin,
		}); err != nil {
			t.Fatalf("activate: %v", err)
		}
		if err := p.recon.Tick(t.Context()); err != nil {
			t.Fatalf("tick: %v", err)
		}
		if got := activeID(t, p); got != held {
			t.Errorf("this node's active revision is %s, want the one the fleet took, %s", got, held)
		}
	})
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
	// non-empty on purpose: a company with NO models is a documented
	// authoring state, which applies cleanly and so could not stand for
	// a revision that cannot be built.
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
		Summary:    "ghost", At: pinnedNow, Origin: anOrigin}); err != nil {
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
	p.activate(t.Context(), t, grownCompanyDoc)
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
	p.activate(t.Context(), t, grownCompanyDoc)
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
	p.activate(t.Context(), t, grownCompanyDoc)
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
	p.activate(t.Context(), t, grownCompanyDoc)
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
	p.activate(t.Context(), t, grownCompanyDoc)
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

	epoch := p.activate(t.Context(), t, companyDoc)
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
	// The background learning passes are handed over AFTER the swap: their
	// loops walk the current company's seats, and handed over before it a
	// later refusal would leave them on a revision this node never served.
	if at, swap := slices.Index(got.AppliedSubsystems, "learning_passes"),
		slices.Index(got.AppliedSubsystems, "epoch"); at < swap {
		t.Errorf("subsystems = %v, want learning_passes after the epoch swap",
			got.AppliedSubsystems)
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
	status, applied, err := e.Apply(t.Context(), &config.Company{}, time.Now())
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

	epoch := author.activate(t.Context(), t, grownCompanyDoc)
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
	epoch := p.activate(t.Context(), t, grownCompanyDoc)
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

// --- the admission gate ---------------------------------------------------

// A NODE THAT HAS DECIDED NOTHING ADMITS WORK.
//
// The gate is consulted from the first delivery a node could take, which is
// before its first reconcile pass has necessarily run. Refusing on "I have not
// looked yet" would make every node deaf for its first interval — a cold start
// that answers no webhooks is strictly worse than one that answers them
// against the config it booted with.
func TestANodeThatHasDecidedNothingAdmitsWork(t *testing.T) {
	t.Parallel()
	p := newPlane(t)
	if !p.recon.Admits() {
		t.Error("a reconciler that has never decided a posture refused work")
	}
}

// A CURRENT NODE ADMITS WORK, which is the ordinary case and the one a
// regression here would take out first.
func TestACurrentNodeAdmitsWork(t *testing.T) {
	t.Parallel()
	p := newPlane(t)
	p.activate(t.Context(), t, grownCompanyDoc)
	if err := p.recon.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := p.recon.Posture(t.Context()); got != configplane.PostureServe {
		t.Fatalf("posture = %q, want serve", got)
	}
	if !p.recon.Admits() {
		t.Error("a node serving the current epoch refused work")
	}
}

// LAG ALONE DOES NOT CLOSE THE GATE.
//
// Every successful rollout produces lag, so a `wait` that refused work would
// make the first node to apply the cause of a fleet-wide outage — the same
// reason wait stays ready at /ready.
func TestOrdinaryLagStillAdmitsWork(t *testing.T) {
	t.Parallel()
	p := newPlane(t)
	p.activate(t.Context(), t, grownCompanyDoc)
	if got := p.recon.Posture(t.Context()); got != configplane.PostureWait {
		t.Fatalf("posture = %q, want wait", got)
	}
	if !p.recon.Admits() {
		t.Error("a node with ordinary propagation lag refused work")
	}
}

// A SHEDDING NODE ACTUALLY REFUSES.
//
// The regression this exists for: the posture was computed, published on the
// presence heartbeat and reported by /ready, and NOTHING acted on it. The gate
// was an engine option no production call site could fill, so a node that had
// concluded it was shedding kept taking every delivery, firing every schedule
// and running every turn against a company it had already decided it did not
// have. configplane.Posture.ServesTraffic was written for this and had no
// caller at all.
func TestASheddingNodeRefusesNewWork(t *testing.T) {
	t.Parallel()
	p := newPlane(t)
	// Confirmed lag — this node tried the epoch and failed — plus a
	// healthy peer that has it, which together are what shed means.
	p.activatePayload(t, "broken", brokenRevision)
	_ = p.recon.Tick(t.Context())
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
		t.Fatalf("posture = %q, want shed", got)
	}
	if p.recon.Admits() {
		t.Error("a shedding node still admitted work")
	}
}

// AN UNREADABLE CONTROL PLANE REOPENS THE GATE.
//
// Posture fails OPEN — a plane it cannot read reports serve — and admission
// has to follow it there. Recording only the reads that succeeded would leave
// a node that shed once and then lost the plane refusing work for the whole
// outage, on evidence it could no longer check.
func TestAnUnreadableControlPlaneReopensTheGate(t *testing.T) {
	t.Parallel()
	var blind atomic.Bool
	p := newPlane(t, func(o *engine.ReconcilerOptions) {
		o.Fleet = &blindPlane{planeBackend: o.Fleet, blind: &blind}
	})
	p.activatePayload(t, "broken", brokenRevision)
	_ = p.recon.Tick(t.Context())
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
		t.Fatalf("posture = %q, want shed — the premise", got)
	}
	if p.recon.Admits() {
		t.Fatal("a shedding node still admitted work — the premise")
	}

	blind.Store(true)
	if got := p.recon.Posture(t.Context()); got != configplane.PostureServe {
		t.Fatalf("posture = %q, want serve", got)
	}
	if !p.recon.Admits() {
		t.Error("the gate stayed shut on a posture that had failed open to serve")
	}
}

// blindPlane is a control plane that stops answering on command — a broker
// that was reachable and is not any more, which is the state the fail-open
// rule exists for.
//
// EMBEDDED THROUGH AN ALIAS, because coord.Plane declares a Fleet() method of
// its own and embedding the interface directly names the field "Fleet".
type planeBackend = coord.Plane

type blindPlane struct {
	planeBackend
	blind *atomic.Bool
}

func (b *blindPlane) Target(ctx context.Context) (coord.Activation, bool, error) {
	if b.blind.Load() {
		return coord.Activation{}, false, errors.New("the control plane is unreachable")
	}
	return b.planeBackend.Target(ctx)
}

// THE PROGRESS TRIPLE IS READ FROM OTHER GOROUTINES THAN THE ONE THAT
// WRITES IT, and nothing in this suite used to do both at once.
//
// The tick writes applied, target and attempts. Posture reads all three —
// from the seat heartbeat via SetPosture, from the readiness probe, and from
// every runtime-state HTTP handler. Only `applied` was atomic, with a comment
// naming this exact hazard; the other two sat bare beside it and were a plain
// data race that -race could not see because no test drove the two paths
// together.
func TestPostureIsSafeToReadWhileTheReconcilerTicks(t *testing.T) {
	t.Parallel()
	p := newPlane(t)
	p.activate(t.Context(), t, grownCompanyDoc)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var wg sync.WaitGroup
	// Several readers, because one racing goroutine finds a race far less
	// reliably than a handful hammering the same fields.
	for range 4 {
		wg.Go(func() {
			for ctx.Err() == nil {
				p.recon.Posture(ctx)
				p.recon.Applied()
				p.recon.Admits()
			}
		})
	}
	// And the writer, ticking against a moving target so the attempt
	// counter and the target epoch both change under the readers.
	//
	// TEN CYCLES: the detector reports the first unordered pair it sees,
	// and four readers spinning without a yield hit every field of the
	// triple thousands of times between two ticks, so each cycle — every
	// field moved — is already a full chance to see it. Forty bought
	// nothing ten does not, and held four cores spinning for ten seconds
	// of a shared race job.
	wg.Go(func() {
		defer cancel()
		for range 10 {
			_ = p.recon.Tick(ctx)
			p.activate(ctx, t, grownCompanyDoc)
		}
	})
	wg.Wait()
}

// AND THE PROGRESS IS NEVER TORN. Separate atomics would silence the race
// detector and still let a reader see an applied epoch from one moment beside
// an attempt count from another — "converged, but still retrying", which is
// not a state this node was ever in.
func TestTheProgressIsReadAsOneMoment(t *testing.T) {
	t.Parallel()
	p := newPlane(t)
	epoch := p.activate(t.Context(), t, grownCompanyDoc)
	if err := p.recon.Tick(t.Context()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if p.recon.Applied() != epoch {
		t.Fatalf("applied = %d, want %d", p.recon.Applied(), epoch)
	}
	// A node that has reached its target reports ok, and a successful apply
	// resets the attempt budget — so the status derived from the counter
	// must agree with the epoch beside it.
	row := p.fleetRow(t)
	if row.Status != string(configplane.StatusOK) {
		t.Errorf("status = %q for a node that reached its target", row.Status)
	}
	if row.Epoch != epoch {
		t.Errorf("reported epoch = %d, want %d", row.Epoch, epoch)
	}
}

// --- posture during an apply ------------------------------------------------

// midApplyPlane is the control plane with a window INTO an attempt: the
// reconciler reads a revision's payload from it only while an attempt is
// fetching a revision this node does not hold, so [midApplyPlane.during] runs
// on the tick's own goroutine after the attempt has started and before the
// epoch is swapped — exactly where the probes and the heartbeat, on their own
// goroutines, used to read the attempt in flight as a failed one.
//
// A payload it is told to WITHHOLD it answers as absent — a pointer naming a
// revision whose body is nowhere — so every attempt at that revision fails
// and none of them keeps a local copy that would let the next one skip the
// window.
type midApplyPlane struct {
	planeBackend
	during   atomic.Pointer[func()]
	withheld atomic.Pointer[string]
}

func (m *midApplyPlane) Payload(ctx context.Context, revisionID string) ([]byte, bool, error) {
	if fn := m.during.Load(); fn != nil {
		(*fn)()
	}
	if id := m.withheld.Load(); id != nil && *id == revisionID {
		return nil, false, nil
	}
	return m.planeBackend.Payload(ctx, revisionID)
}

// observe makes fn the window's observer.
func (m *midApplyPlane) observe(fn func()) { m.during.Store(&fn) }

// sighting is one posture read, with where in the apply it was taken and
// whether the node had a company at that instant — the two facts /health
// derives its status from.
type sighting struct {
	where      string
	posture    configplane.Posture
	configured bool
}

// activateRemotely points the fleet at a revision this node's own store does
// not hold, which is how every peer but the writer meets a revision — and
// what makes the attempt fetch it through the window above.
func activateRemotely(t *testing.T, p *plane, doc string) int64 {
	t.Helper()
	document := yamlToJSON(t, doc)
	published, err := p.fleet.Activate(t.Context(), coord.ActivationRequest{
		RevisionID: newRevisionID(t), Summary: "revision", Payload: document,
		At: pinnedNow, Origin: anOrigin,
	})
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	return published.Epoch
}

// newRevisionID is a revision id no store holds.
func newRevisionID(t *testing.T) string {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatal(err)
	}
	return id.String()
}

// A SUCCESSFUL APPLY IS NEVER REPORTED AS A DIVERGENCE — the first one, on a
// node with no company at all, and a later one alike.
//
// The attempt counter used to be raised BEFORE the attempt was made, and the
// posture read any counted attempt at an epoch not yet reached as a failed
// one. So for the whole of every successful apply a lone node reported
// isolated, and its /health said `isolated` (or `unconfigured`, the first
// time), indistinguishable from a revision that genuinely does not apply.
//
// Observed three ways: inside the attempt before the epoch is swapped (the
// payload fetch), inside it after (the engine's applied hook, which runs
// before the outcome is recorded), and by a reader polling the posture from
// another goroutine for the whole run, as the probes and the heartbeat do.
// The two windows are what make the case able to fail: the poller alone could
// miss a hundred-millisecond apply entirely.
func TestASuccessfulApplyIsNeverReportedAsADivergence(t *testing.T) {
	t.Parallel()
	e := unconfiguredEngine(t)
	window := &midApplyPlane{planeBackend: e.Backends().Fleet}
	p := planeFor(t, e, func(o *engine.ReconcilerOptions) { o.Fleet = window })

	var (
		mu    sync.Mutex
		seen  []sighting
		where string
	)
	look := func(at string) {
		posture := p.recon.Posture(t.Context())
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, sighting{at + " (" + where + ")", posture, e.Company() != nil})
	}
	window.observe(func() { look("before the swap") })
	e.SetOnApplied(func(context.Context) { look("after the swap") })

	ctx, cancel := context.WithCancel(t.Context())
	var polled []configplane.Posture
	var wg sync.WaitGroup
	wg.Go(func() {
		for ctx.Err() == nil {
			polled = append(polled, p.recon.Posture(ctx))
		}
	})

	for _, step := range []struct {
		name, doc  string
		configured bool
	}{
		{"the first apply, on an unconfigured node", companyDoc, false},
		{"a later apply", grownCompanyDoc, true},
	} {
		mu.Lock()
		where, seen = step.name, nil
		mu.Unlock()
		epoch := activateRemotely(t, p, step.doc)
		if err := p.recon.Tick(t.Context()); err != nil {
			t.Fatalf("%s: tick: %v", step.name, err)
		}
		if p.recon.Applied() != epoch {
			t.Fatalf("%s: applied = %d, want %d", step.name, p.recon.Applied(), epoch)
		}
		mu.Lock()
		got := slices.Clone(seen)
		mu.Unlock()
		if len(got) != 2 {
			t.Fatalf("%s: %d sightings inside the apply, want one either side of the swap: %+v",
				step.name, len(got), got)
		}
		for _, s := range got {
			if s.posture != configplane.PostureWait {
				t.Errorf("%s: posture = %s, want wait — a node applying a revision is "+
					"propagating it, not diverged from it", s.where, s.posture)
			}
		}
		// What /health's status is derived from beside the posture: no
		// company until the swap on a node that had none, so the first
		// apply reads `unconfigured` before it and `ok` after — each true
		// at its instant — and never a divergence.
		if got[0].configured != step.configured || !got[1].configured {
			t.Errorf("%s: configured = %v before the swap and %v after, want %v and true",
				step.name, got[0].configured, got[1].configured, step.configured)
		}
		if posture := p.recon.Posture(t.Context()); posture != configplane.PostureServe {
			t.Errorf("%s: posture after the apply = %s, want serve", step.name, posture)
		}
	}
	cancel()
	wg.Wait()
	if len(polled) == 0 {
		t.Fatal("the poller read no posture at all, so it observed nothing")
	}
	for _, posture := range polled {
		if posture != configplane.PostureServe && posture != configplane.PostureWait {
			t.Fatalf("a reader polling through two successful applies saw %s", posture)
		}
	}
}

// AND A FAILED APPLY STILL SAYS SO — from the moment its attempt concludes,
// never before. The fix must not have bought the transient's silence with the
// divergence's: an attempt at a revision this node cannot reach reads wait only
// while it is running, then isolated on its own, shed once a peer has the
// epoch, and stuck only when the LAST attempt has failed — not while it runs.
func TestAFailedApplyIsReportedOnceItConcludes(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{})
	window := &midApplyPlane{planeBackend: e.Backends().Fleet}
	p := planeFor(t, e, func(o *engine.ReconcilerOptions) { o.Fleet = window })
	p.activate(t.Context(), t, grownCompanyDoc)
	if err := p.recon.Tick(t.Context()); err != nil {
		t.Fatalf("the starting revision did not apply: %v", err)
	}

	// A pointer naming a revision whose body is nowhere: every attempt
	// fetches it through the window and fails, so each one can be watched
	// while it runs.
	ghost := newRevisionID(t)
	window.withheld.Store(&ghost)
	published, err := p.fleet.Activate(t.Context(), coord.ActivationRequest{
		RevisionID: ghost, Summary: "ghost", At: pinnedNow, Origin: anOrigin,
	})
	if err != nil {
		t.Fatal(err)
	}
	var during configplane.Posture
	window.observe(func() { during = p.recon.Posture(t.Context()) })

	attempt := func(want, wantAfter configplane.Posture) {
		t.Helper()
		during = ""
		if err := p.recon.Tick(t.Context()); !errors.Is(err, store.ErrNoRevision) {
			t.Fatalf("tick = %v, want the revision reported missing", err)
		}
		if during != want {
			t.Errorf("posture while the attempt ran = %q, want %s", during, want)
		}
		if after := p.recon.Posture(t.Context()); after != wantAfter {
			t.Errorf("posture once it failed = %s, want %s", after, wantAfter)
		}
	}

	// Alone: nothing has concluded while the first attempt runs, and its
	// failure is then the only evidence there is.
	attempt(configplane.PostureWait, configplane.PostureIsolated)

	// A peer has the epoch: this node is the anomaly, and its retry runs
	// on the evidence of the failure before it.
	if err := p.fleet.RecordApply(t.Context(), coord.NodeApply{
		NodeID: "node-b", Epoch: published.Epoch, Status: string(configplane.StatusOK),
		UpdatedAt: pinnedNow,
	}); err != nil {
		t.Fatal(err)
	}
	if got := p.recon.Posture(t.Context()); got != configplane.PostureShed {
		t.Fatalf("posture beside a peer that has the epoch = %s, want shed", got)
	}
	for range configplane.MaxApplyAttempts - 2 {
		attempt(configplane.PostureShed, configplane.PostureShed)
	}
	// The last attempt in flight has not exhausted anything yet.
	attempt(configplane.PostureShed, configplane.PostureStuck)
}

// AN APPLY THAT OUTLASTS PROPAGATION IS CONFIRMED LAG, while it is still
// running. With attempts counted only when they conclude, a hung apply would
// otherwise read as ordinary propagation for as long as it hung — a tick
// blocked in it never comes round to count anything — so the lag is measured
// in time, from the moment this node first saw the epoch.
func TestAnApplyThatOutlastsTheGraceIsConfirmedLag(t *testing.T) {
	t.Parallel()
	var clock atomic.Int64
	clock.Store(pinnedNow.UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()).UTC() }

	e := newEngine(t, engine.Options{})
	window := &midApplyPlane{planeBackend: e.Backends().Fleet}
	p := planeFor(t, e, func(o *engine.ReconcilerOptions) {
		o.Fleet = window
		o.Now = now
	})
	epoch := activateRemotely(t, p, grownCompanyDoc)

	var early, late, healthyPeer configplane.Posture
	window.observe(func() {
		early = p.recon.Posture(t.Context())
		clock.Add(int64(configplane.LagGrace))
		late = p.recon.Posture(t.Context())
		// A peer that has the epoch, fresh on the clock as it now reads.
		if err := p.fleet.RecordApply(t.Context(), coord.NodeApply{
			NodeID: "node-b", Epoch: epoch, Status: string(configplane.StatusOK),
			UpdatedAt: now(),
		}); err != nil {
			t.Error(err)
		}
		healthyPeer = p.recon.Posture(t.Context())
	})
	if err := p.recon.Tick(t.Context()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if early != configplane.PostureWait {
		t.Errorf("posture as the apply began = %s, want wait", early)
	}
	// Behind past the grace with no peer reporting: silence, not evidence.
	if late != configplane.PostureWait {
		t.Errorf("posture past the grace with no peer = %s, want wait", late)
	}
	if healthyPeer != configplane.PostureShed {
		t.Errorf("posture past the grace beside a peer that has the epoch = %s, want shed",
			healthyPeer)
	}
	// And the apply that did finish takes the node back to serving, with
	// the lag clock stopped: nothing it waited out counts against the next
	// epoch.
	if got := p.recon.Posture(t.Context()); got != configplane.PostureServe {
		t.Errorf("posture after the apply = %s, want serve", got)
	}
	window.observe(func() {})
	clock.Add(int64(time.Hour))
	next := activateRemotely(t, p, companyDoc)
	if err := p.fleet.RecordApply(t.Context(), coord.NodeApply{
		NodeID: "node-b", Epoch: next, Status: string(configplane.StatusOK), UpdatedAt: now(),
	}); err != nil {
		t.Fatal(err)
	}
	if got := p.recon.Posture(t.Context()); got != configplane.PostureWait {
		t.Errorf("posture on a new epoch an hour after the last apply = %s, want wait — "+
			"the lag the last epoch waited out was counted against this one", got)
	}
}

// failEverywhere activates a revision whose payload is withheld from the
// window, so every attempt at it fails, and runs `attempts` of them.
func failEverywhere(t *testing.T, p *plane, window *midApplyPlane, attempts int) int64 {
	t.Helper()
	ghost := newRevisionID(t)
	window.withheld.Store(&ghost)
	published, err := p.fleet.Activate(t.Context(), coord.ActivationRequest{
		RevisionID: ghost, Summary: "ghost", At: pinnedNow, Origin: anOrigin,
	})
	if err != nil {
		t.Fatal(err)
	}
	for range attempts {
		if err := p.recon.Tick(t.Context()); !errors.Is(err, store.ErrNoRevision) {
			t.Fatalf("tick at the withheld revision = %v, want the revision reported missing", err)
		}
	}
	return published.Epoch
}

// peerApplied records a fresh ok row for epoch from another node.
func peerApplied(t *testing.T, p *plane, epoch int64, at time.Time) {
	t.Helper()
	if err := p.fleet.RecordApply(t.Context(), coord.NodeApply{
		NodeID: "node-b", Epoch: epoch, Status: string(configplane.StatusOK), UpdatedAt: at,
	}); err != nil {
		t.Fatal(err)
	}
}

// THE RECOVERY GESTURE IS AN ORDINARY APPLY. A revision failed everywhere, the
// node has been isolated on it for ten minutes, and the operator activates the
// corrected one: a faster peer records it first, and this node — mid-apply of
// a fix it will apply — is propagating, not late. The lag clock used to carry
// across the new target, so the ten minutes waited out on the bad revision
// read as confirmed lag on the fix and the node shed for the whole of an apply
// that succeeded, on every node but the fastest.
func TestTheRecoveryApplyIsJudgedOnTheCorrectedEpochAlone(t *testing.T) {
	t.Parallel()
	var clock atomic.Int64
	clock.Store(pinnedNow.UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	e := newEngine(t, engine.Options{})
	window := &midApplyPlane{planeBackend: e.Backends().Fleet}
	p := planeFor(t, e, func(o *engine.ReconcilerOptions) {
		o.Fleet = window
		o.Now = now
	})
	p.activate(t.Context(), t, grownCompanyDoc)
	if err := p.recon.Tick(t.Context()); err != nil {
		t.Fatalf("the starting revision did not apply: %v", err)
	}
	failEverywhere(t, p, window, 1)
	if got := p.recon.Posture(t.Context()); got != configplane.PostureIsolated {
		t.Fatalf("posture after a revision that failed alone = %s, want isolated", got)
	}

	clock.Add(int64(10 * time.Minute))
	fixed := activateRemotely(t, p, companyDoc)
	var during configplane.Posture
	window.observe(func() {
		peerApplied(t, p, fixed, now())
		during = p.recon.Posture(t.Context())
	})
	if err := p.recon.Tick(t.Context()); err != nil {
		t.Fatalf("the corrected revision did not apply: %v", err)
	}
	if p.recon.Applied() != fixed {
		t.Fatalf("applied = %d, want the corrected epoch %d", p.recon.Applied(), fixed)
	}
	if during != configplane.PostureWait {
		t.Errorf("posture while applying the corrected revision beside a peer that has it = %s, "+
			"want wait — the lag waited out on the bad revision was counted against the fix", during)
	}
	if got := p.recon.Posture(t.Context()); got != configplane.PostureServe {
		t.Errorf("posture once the fix applied = %s, want serve", got)
	}
}

// AN EPOCH THE TICK HAS NOT MET YET HAS NO ATTEMPTS AND NO LAG. Between an
// activation and the tick that first sees it — a nudge's latency, or a whole
// poll interval when the nudge is lost — the posture reads the fresh pointer
// beside progress that describes the previous target. Paired that way, a node
// that exhausted its retries on a bad revision long ago read as STUCK on the
// corrected one the moment a peer applied it, out of rotation before it had
// made any attempt at it.
func TestAnEpochNotYetMetCarriesNothingFromTheLastOne(t *testing.T) {
	t.Parallel()
	var clock atomic.Int64
	clock.Store(pinnedNow.UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	e := newEngine(t, engine.Options{})
	window := &midApplyPlane{planeBackend: e.Backends().Fleet}
	p := planeFor(t, e, func(o *engine.ReconcilerOptions) {
		o.Fleet = window
		o.Now = now
	})
	p.activate(t.Context(), t, grownCompanyDoc)
	if err := p.recon.Tick(t.Context()); err != nil {
		t.Fatalf("the starting revision did not apply: %v", err)
	}
	bad := failEverywhere(t, p, window, configplane.MaxApplyAttempts)
	peerApplied(t, p, bad, now())
	if got := p.recon.Posture(t.Context()); got != configplane.PostureStuck {
		t.Fatalf("posture out of retries beside a peer that has the epoch = %s, want stuck", got)
	}

	clock.Add(int64(10 * time.Minute))
	fixed := activateRemotely(t, p, companyDoc)
	peerApplied(t, p, fixed, now())
	// No tick yet: this node has not attempted the corrected epoch at all.
	if got := p.recon.Posture(t.Context()); got != configplane.PostureWait {
		t.Errorf("posture before any attempt at the corrected epoch = %s, want wait", got)
	}
	if !p.recon.Admits() {
		t.Error("a node that has not yet attempted the corrected epoch refuses work")
	}
}

// AND A HUNG APPLY STAYS LATE ACROSS A NEW ACTIVATION. Restarting the lag
// clock with each epoch must not hand a wedged node a fresh 45 s every time
// the operator activates something: its tick is held in the attempt at the
// older epoch and cannot begin on the new one, so it is late on the new epoch
// by as long as that attempt has held it — wait while that is short, shed
// once it outlasts propagation beside a peer that has the new epoch.
func TestAHungApplyStaysLateAcrossANewActivation(t *testing.T) {
	t.Parallel()
	var clock atomic.Int64
	clock.Store(pinnedNow.UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	e := newEngine(t, engine.Options{})
	p := planeFor(t, e, func(o *engine.ReconcilerOptions) { o.Now = now })
	activateRemotely(t, p, grownCompanyDoc)

	// From inside the attempt at the older epoch — after its epoch is
	// swapped and before its outcome is recorded, where a hung apply would
	// sit — the fleet moves on and a peer applies the newer epoch.
	var (
		next        int64
		early, hung configplane.Posture
		once        sync.Once
	)
	moveOn := func() {
		next = activateRemotely(t, p, companyDoc)
		peerApplied(t, p, next, now())
		early = p.recon.Posture(t.Context())
		clock.Add(int64(configplane.LagGrace))
		peerApplied(t, p, next, now())
		hung = p.recon.Posture(t.Context())
	}
	e.SetOnApplied(func(context.Context) { once.Do(moveOn) })
	if err := p.recon.Tick(t.Context()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if early != configplane.PostureWait {
		t.Errorf("posture moments into an attempt at the older epoch = %s, want wait", early)
	}
	if hung != configplane.PostureShed {
		t.Errorf("posture with an attempt at the older epoch held past the grace, beside a peer "+
			"on the newer one = %s, want shed", hung)
	}
	// The attempt ended, so the tick is free to meet the newer epoch: the
	// time it was held no longer counts against it.
	if got := p.recon.Posture(t.Context()); got != configplane.PostureWait {
		t.Errorf("posture once the attempt ended, before the tick meets the newer epoch = %s, "+
			"want wait", got)
	}
	if err := p.recon.Tick(t.Context()); err != nil {
		t.Fatalf("tick at the newer epoch: %v", err)
	}
	if p.recon.Applied() != next {
		t.Fatalf("applied = %d, want %d", p.recon.Applied(), next)
	}
}
