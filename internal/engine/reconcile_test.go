package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
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
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// pinnedNow is the clock the reconcile tests run on, pinned because a peer
// status's freshness window is measured against it: on the real clock a case
// would assert about that window by sleeping through it.
var pinnedNow = time.Date(2026, 8, 23, 14, 0, 0, 0, time.UTC)

// anOrigin is the origin every activation a case publishes carries: the
// backends refuse an activation without one.
var anOrigin = coord.RevisionOrigin{
	Author: "maya", AuthorKind: iam.ActorHuman, Source: "api",
	CreatedAt: pinnedNow.Add(-time.Hour),
}

// brokenRevision is well-formed JSON that cannot be built: a delegate template
// naming a provider the document does not configure. The provider block is
// non-empty because a company with no models is a supported state and would
// apply (nomodels_test.go).
var brokenRevision = json.RawMessage(`{"name":"Acme",
  "providers":{"sandbox":{"fake":true},"llm":{"zulu":{"model":"m"}}},
  "workers":{"researcher":{"description":"reads sources and reports findings",
    "system_prompt":"You research.","model":"nonexistent"}}}`)

// unbuildableCompanyDoc meets every runnable rule and is refused by the
// engine's own build: its seat's model is a ${VAR} nothing on the node
// answers, and the build refuses a model that resolves to nothing.
const unbuildableCompanyDoc = `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    coder: {harness: claude-code, model: "${CREWLET_TEST_MODEL_NEVER_SET}"}
roles:
  - name: SWE
    handle: swe
    llm: coder
`

// grownCompanyDoc differs from companyDoc where a reconcile must be visible:
// one more seat, and a day ceiling (above the executor's launch floor, which a
// company whose seats run an executor may not go below).
const grownCompanyDoc = `
name: Acme
token_budget:
  day: 4242000
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
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

// grownDayCeiling is grownCompanyDoc's day ceiling, which tells it apart from
// companyDoc once applied.
const grownDayCeiling = 4242000

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
	r, err := e.NewReconciler(t.Context(), options)
	if err != nil {
		t.Fatalf("NewReconciler: %v", err)
	}
	p.recon = r
	return p
}

// fleetPeers is two nodes of ONE fleet: each its own engine and database,
// sharing the author's coordination store and ONE keyring, as every node's
// Tier A names the same keys. Engines built apart hold random keyrings, and a
// peer could open nothing its author sealed.
func fleetPeers(t *testing.T, peerOpts ...func(*engine.ReconcilerOptions)) (author, peer *plane) {
	t.Helper()
	material := testKeyMaterial(t)
	keyed := func() *engine.Engine {
		return newEngine(t, engine.Options{Bootstrap: bootstrap(t, func(b *config.Bootstrap) {
			brokertest.StoreIn(&b.Stream, filepath.Join(brokertest.TempDir(t), "stream"))
			b.Secrets.Keys = []config.SecretKey{{ID: b.Secrets.ActiveKeyID, Material: material}}
		})})
	}
	author = planeFor(t, keyed())
	peer = planeFor(t, keyed(), append([]func(*engine.ReconcilerOptions){
		func(o *engine.ReconcilerOptions) { o.Fleet = author.fleet },
	}, peerOpts...)...)
	return author, peer
}

// seal is a document as this node's writers store and publish it: sealed under
// the engine's own keyring, which is the one its reconciler opens with.
func (p *plane) seal(t *testing.T, document json.RawMessage) json.RawMessage {
	t.Helper()
	sealed, err := secrets.Seal(p.engine.Cipher(), document)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	return sealed
}

// activate stores a revision and points the fleet at it, returning its epoch.
// The payload goes in the node's database as history and the pointer in the
// coordination store, SEALED in both as every writer stores it; only the
// reconciler's apply makes it what the node boots on.
//
// It takes the caller's context because the concurrency case drives it from a
// goroutine under a context it cancels, and must not activate past it.
func (p *plane) activate(ctx context.Context, t *testing.T, doc string) int64 {
	t.Helper()
	document := p.seal(t, yamlToJSON(t, doc))
	id, err := p.store.Configs().Insert(ctx, store.Revision{
		Source: "test", CreatedBy: revisionAuthor, CreatedByKind: iam.ActorHuman,
		OperatorID: revisionCredential, Summary: "revision",
		Payload: document, CreatedAt: pinnedNow,
	})
	if err != nil {
		t.Fatalf("store the revision: %v", err)
	}
	// The pointer carries the whole origin, as the config surface publishes
	// it: the backends refuse an activation without one.
	published, err := p.fleet.Activate(ctx, coord.ActivationRequest{
		RevisionID: id, Summary: "revision", Payload: document, At: pinnedNow,
		Origin: coord.RevisionOrigin{
			Author: revisionAuthor, AuthorKind: iam.ActorHuman,
			OperatorID: revisionCredential, Source: "test", CreatedAt: pinnedNow,
		},
	})
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	return published.Epoch
}

// revisionAuthor and revisionCredential are who [plane.activate] writes a
// revision as: a person (kind human) through their own machine token.
const (
	revisionAuthor     = "jane.doe"
	revisionCredential = "pat:0192f00d-0000-7000-8000-00000000000a"
)

// activatePayload is activate for a payload that is not a company document,
// sealed and written to both stores like any other: a reconciler reads only a
// payload the fleet points at.
func (p *plane) activatePayload(t *testing.T, summary string, payload json.RawMessage) int64 {
	t.Helper()
	payload = p.seal(t, payload)
	id, err := p.store.Configs().Insert(t.Context(), store.Revision{
		CreatedByKind: iam.ActorOperator,
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

// yamlToJSON parses a company as an import does and returns its JSON form,
// which is what the payload column holds.
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
	// A fresh deployment before its first import: recording "error" for a
	// company nobody configured would show the node broken in the fleet view.
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
	if got := dayCeiling(before); got != 0 {
		t.Errorf("the previous epoch changed under the apply: budget %d", got)
	}
	if got := len(before.Seats()); got != 2 {
		t.Errorf("the previous epoch changed under the apply: %d seats", got)
	}
	if got := dayCeiling(p.engine.Company()); got != grownDayCeiling {
		t.Errorf("token budget = %d, want the new revision's", got)
	}
	// AND THE ORG IS THE REVISION'S: the seats are part of the document an
	// apply installs.
	if got := p.seats(t); !slices.Equal(got, []string{"ceo", "cto", "designer"}) {
		t.Errorf("seats = %v, want the three the new revision names", got)
	}
}

func TestTheOutcomeIsRecordedWhereEveryPeerReadsIt(t *testing.T) {
	t.Parallel()
	// The pointer says where the fleet should be, this row where THIS node
	// is: only both tell propagation lag from a node that cannot apply.
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

// A CONVERGED NODE KEEPS SAYING SO. The apply-status row ages out
// (coord.StatusFreshness), so a converged node writing it only on an apply
// would vanish from the fleet view and every peer's evidence while serving
// the current epoch.
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
	// The credential-rotation gesture: the payload is identical but its
	// ${VAR} references now resolve differently, so a no-op check on the
	// payload would defeat it.
	p := newPlane(t)
	first := p.activate(t.Context(), t, grownCompanyDoc)
	if err := p.recon.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	afterFirst := p.engine.Company()

	target, found, err := p.fleet.Target(t.Context())
	if err != nil || !found {
		t.Fatalf("Target: found=%v err=%v", found, err)
	}
	payload, found, err := p.fleet.Body(t.Context(), target)
	if err != nil || !found {
		t.Fatalf("Body: found=%v err=%v", found, err)
	}
	// The pointer is append-only, so re-activating the SAME revision mints a
	// new epoch every node watches: how a rotated secret reaches the fleet.
	republished, err := p.fleet.Activate(t.Context(), coord.ActivationRequest{
		RevisionID: target.RevisionID, Summary: target.Summary, Payload: payload,
		At: pinnedNow, Expect: target.RevisionID, Origin: anOrigin})
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
	// The tick runs every 15 s for the node's life; rebuilding the epoch on
	// each would restart every subsystem an apply touches.
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

// RE-ACTIVATING THE REVISION THIS NODE BOOTS ON MOVES BOTH ITS INSTANTS (the
// credential-rotation gesture): the served one the node boots its chart with,
// and the activation one its config history shows and the sweep dates it by.
//
// Mutation: compare the boot row with what the engine serves by id alone in
// Reconciler.serve, and the instant stays the first activation's.
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
	boot, found, err := p.store.Configs().Boot(t.Context())
	if err != nil || !found {
		t.Fatalf("boot: found=%v err=%v", found, err)
	}
	if boot.ID != target.RevisionID || !boot.ServedAt.Equal(later) || !boot.ActivatedAt.Equal(later) {
		t.Errorf("this node boots on %s applied at %s (activated %s), want %s applied at "+
			"the re-activation's %s", boot.ID, boot.ServedAt, boot.ActivatedAt, target.RevisionID, later)
	}
}

// rotatedVarDoc's one seat's model is a ${VAR}: it builds while the node's
// environment answers it and is refused once it does not, as a variable one
// node never received.
const rotatedVarDoc = `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    coder: {harness: claude-code, model: "${CREWLET_TEST_MODEL_ROTATED}"}
roles:
  - name: SWE
    handle: swe
    llm: coder
`

// A RE-ACTIVATION OF THE REVISION A NODE RUNS, WHICH THE NODE THEN REFUSES, IS
// STILL THE FLEET'S. The node keeps running that revision at its earlier
// instant, so its boot row is its fleet's row, carrying both instants; were
// the fleet's not the re-activation's, a lost pointer would be restored at the
// old instant and every tick out of retries would rewrite the row, decrypting
// the revision each time. The apply records it or, where that write failed,
// the tick out of retries, comparing the instant: the revision already matches.
// Not parallel: the variable is the process environment.
//
// Mutations: put `is_active = 0` back on the store's raise, and the fleet's
// instant stays the applied one; compare the revision alone in
// Reconciler.note, and the second case keeps the applied instant.
func TestARefusedReactivationOfTheRunningRevisionIsStillTheFleets(t *testing.T) {
	for _, tc := range []struct {
		name string
		// applyKeepFails refuses the store's write of the fleet's instant
		// for every attempt, as a write that failed during the apply.
		applyKeepFails bool
	}{
		{name: "recorded by the apply"},
		{name: "repaired out of retries", applyKeepFails: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CREWLET_TEST_MODEL_ROTATED", "sonnet")
			p := newPlane(t)
			p.activate(t.Context(), t, rotatedVarDoc)
			if err := p.recon.Tick(t.Context()); err != nil {
				t.Fatalf("apply with the variable set: %v", err)
			}
			target, _, err := p.fleet.Target(t.Context())
			if err != nil {
				t.Fatalf("Target: %v", err)
			}
			payload, _, err := p.fleet.Body(t.Context(), target)
			if err != nil {
				t.Fatalf("Payload: %v", err)
			}
			if err = os.Unsetenv("CREWLET_TEST_MODEL_ROTATED"); err != nil {
				t.Fatal(err)
			}
			later := pinnedNow.Add(time.Hour)
			if _, err = p.fleet.Activate(t.Context(), coord.ActivationRequest{
				RevisionID: target.RevisionID, Summary: "rotate", Payload: payload, At: later,
				Expect: target.RevisionID, Origin: anOrigin,
			}); err != nil {
				t.Fatalf("re-activate: %v", err)
			}
			if tc.applyKeepFails {
				p.refuseInstants(t)
			}
			for range configplane.MaxApplyAttempts {
				_ = p.recon.Tick(t.Context())
			}
			if tc.applyKeepFails {
				p.allowInstants(t)
				if fleets, _, _ := p.store.Configs().Fleets(t.Context()); fleets.ActivatedAt.Equal(later) {
					t.Fatalf("the apply recorded the re-activation; the case needs its write refused")
				}
			}
			_ = p.recon.Tick(t.Context())
			if row := p.fleetRow(t); configplane.ApplyStatus(row.Status) != configplane.StatusError {
				t.Fatalf("the re-activation applied (%+v); the case needs it refused", row)
			}
			fleets, found, err := p.store.Configs().Fleets(t.Context())
			if err != nil || !found || fleets.ID != target.RevisionID || !fleets.ActivatedAt.Equal(later) {
				t.Errorf("the fleet's row is %s at %s (found=%v err=%v), want %s at the "+
					"re-activation's %s", fleets.ID, fleets.ActivatedAt, found, err, target.RevisionID, later)
			}
			boot, _, err := p.store.Configs().Boot(t.Context())
			if err != nil || boot.ID != target.RevisionID || !boot.ServedAt.Equal(pinnedNow) {
				t.Errorf("this node boots on %s applied at %s (err %v), want %s at the instant "+
					"it applied, %s", boot.ID, boot.ServedAt, err, target.RevisionID, pinnedNow)
			}
		})
	}
}

// refuseInstants makes this node's store refuse every write that moves a
// revision's activation instant, and nothing else; allowInstants lifts it.
func (p *plane) refuseInstants(t *testing.T) {
	t.Helper()
	if _, err := p.store.SQL().ExecContext(t.Context(), `CREATE TRIGGER refuse_the_instant
		BEFORE UPDATE ON company_config
		WHEN NEW.activated_at IS NOT OLD.activated_at
		BEGIN SELECT RAISE(ABORT, 'the node store refused the write'); END`); err != nil {
		t.Fatalf("install the trigger: %v", err)
	}
}

func (p *plane) allowInstants(t *testing.T) {
	t.Helper()
	if _, err := p.store.SQL().ExecContext(t.Context(), `DROP TRIGGER refuse_the_instant`); err != nil {
		t.Fatalf("drop the trigger: %v", err)
	}
}

// refuseBootRow makes this node's store refuse every write that moves what
// the node boots on, as a full disk would; allowBootRow lifts it. It trips on
// the served instant, which [store.Configs.Serve] moves on both rows it
// touches, so either half rolls the whole transaction back.
func (p *plane) refuseBootRow(t *testing.T) {
	t.Helper()
	if _, err := p.store.SQL().ExecContext(t.Context(), `CREATE TRIGGER refuse_the_boot_row
		BEFORE UPDATE ON company_config
		WHEN NEW.served_at IS NOT OLD.served_at
		BEGIN SELECT RAISE(ABORT, 'the node store refused the write'); END`); err != nil {
		t.Fatalf("install the trigger: %v", err)
	}
}

func (p *plane) allowBootRow(t *testing.T) {
	t.Helper()
	if _, err := p.store.SQL().ExecContext(t.Context(), `DROP TRIGGER refuse_the_boot_row`); err != nil {
		t.Fatalf("drop the trigger: %v", err)
	}
}

// THE FLEET'S ROW IS REPAIRED BY THE TICKS THAT FOLLOW AN APPLY. The apply
// records it best effort, and the node restores it when the pointer is gone,
// so a write the apply could not make is repaired by the next tick: the
// converged one, or one out of retries on a revision this node refuses. A row
// left behind restores a company the fleet has moved past.
//
// Mutation: drop Reconciler.note from either branch of Tick, or break its
// comparison with the pointer, and that branch's case keeps the stale row.
func TestTheFleetsRowIsRepairedAfterAnApply(t *testing.T) {
	t.Parallel()
	// behind moves the fleet's row off the pointer, as a keep that failed
	// during the apply leaves it.
	behind := func(t *testing.T, p *plane) {
		t.Helper()
		stale := store.Revision{
			ID: "0192f00d-0000-7000-8000-00000000057a", CreatedBy: "maya",
			CreatedByKind: iam.ActorHuman, Source: "api", Summary: "stale",
			Payload: p.seal(t, yamlToJSON(t, companyDoc)), CreatedAt: pinnedNow.Add(-time.Hour),
			ActivatedAt: pinnedNow.Add(-time.Hour),
		}
		if err := p.store.Configs().SawFleet(t.Context(), stale); err != nil {
			t.Fatalf("move the fleet's row back: %v", err)
		}
	}
	repaired := func(t *testing.T, p *plane) {
		t.Helper()
		target, _, err := p.fleet.Target(t.Context())
		if err != nil {
			t.Fatalf("Target: %v", err)
		}
		fleets, found, err := p.store.Configs().Fleets(t.Context())
		if err != nil || !found || fleets.ID != target.RevisionID || !fleets.ActivatedAt.Equal(target.At) {
			t.Errorf("the fleet's row is %s at %s (found=%v err=%v), want the pointer's %s at %s",
				fleets.ID, fleets.ActivatedAt, found, err, target.RevisionID, target.At)
		}
	}
	t.Run("converged", func(t *testing.T) {
		t.Parallel()
		p := newPlane(t)
		p.activate(t.Context(), t, grownCompanyDoc)
		if err := p.recon.Tick(t.Context()); err != nil {
			t.Fatalf("apply: %v", err)
		}
		behind(t, p)
		if err := p.recon.Tick(t.Context()); err != nil {
			t.Fatalf("converged tick: %v", err)
		}
		repaired(t, p)
	})
	t.Run("out of retries", func(t *testing.T) {
		t.Parallel()
		p := newPlane(t)
		p.activatePayload(t, "broken", brokenRevision)
		for range configplane.MaxApplyAttempts {
			_ = p.recon.Tick(t.Context())
		}
		behind(t, p)
		_ = p.recon.Tick(t.Context())
		if len(p.applies) != configplane.MaxApplyAttempts {
			t.Fatalf("%d applies, want the tick after them out of retries", len(p.applies))
		}
		repaired(t, p)
	})
}

// WHAT A NODE BOOTS ON IS WHAT ITS ENGINE APPLIED. The reconciler is the boot
// row's only online writer, and a converged tick puts the row back on the
// applied revision if anything moved it (a failed write, an offline choice the
// fleet never took): a row left elsewhere starts the node, one restart later,
// on a company the fleet is not running.
//
// Mutation: drop Reconciler.serve from the converged branch of Tick and the
// first case keeps the offer.
func TestTheBootRowFollowsWhatTheEngineApplied(t *testing.T) {
	t.Parallel()
	bootID := func(t *testing.T, p *plane) store.Revision {
		t.Helper()
		boot, found, err := p.store.Configs().Boot(t.Context())
		if err != nil || !found {
			t.Fatalf("this node boots on nothing (found=%v err=%v)", found, err)
		}
		return boot
	}

	// An offline choice moved the boot row under the running node, and the
	// boot publish withheld it because the fleet had moved on: the node boots
	// on the fleet's revision it runs, and logs that the offline change was
	// not published so its operator learns of it.
	t.Run("a boot row moved under the running node is put back", func(t *testing.T) {
		t.Parallel()
		logged := &logBuffer{}
		p := newPlane(t, func(o *engine.ReconcilerOptions) {
			o.Log = slog.New(slog.NewJSONHandler(logged, nil))
		})
		p.activate(t.Context(), t, grownCompanyDoc)
		if err := p.recon.Tick(t.Context()); err != nil {
			t.Fatalf("tick: %v", err)
		}
		target, _, err := p.fleet.Target(t.Context())
		if err != nil {
			t.Fatalf("Target: %v", err)
		}
		offered, err := p.store.Configs().Offer(t.Context(), store.Offer{Base: "an-older-revision"}, store.Revision{
			CreatedByKind: iam.ActorOperator,
			Source:        "test", CreatedBy: "operator", Summary: "offline",
			Payload: yamlToJSON(t, companyDoc), CreatedAt: pinnedNow.Add(time.Hour),
		})
		if err != nil {
			t.Fatalf("offer: %v", err)
		}
		if err := p.recon.Tick(t.Context()); err != nil {
			t.Fatalf("tick: %v", err)
		}
		if got := bootID(t, p); got.ID != target.RevisionID || got.Offer != nil {
			t.Errorf("this node boots on %s (offer %+v), want the revision it runs, %s, "+
				"rather than the offline %s", got.ID, got.Offer, target.RevisionID, offered)
		}
		// Put back, not re-applied: the epoch was already this node's.
		if len(p.applies) != 1 {
			t.Errorf("%d applies, want the one", len(p.applies))
		}
		said := logged.records(t, "offline_revision_not_published")
		if len(said) != 1 || said[0]["revision"] != offered ||
			said[0]["built_on"] != "an-older-revision" ||
			said[0]["fleet_runs"] != target.RevisionID ||
			said[0]["hint"] != configplane.WithheldOfferRemedy(offered, "an-older-revision") {
			t.Errorf("the replaced offline change was said as %v, want once, naming %s "+
				"built on an-older-revision and the fleet's %s", said, offered,
				target.RevisionID)
		}
	})

	// The boot published this node's offer and died before recording that
	// the fleet took it: serving the revision the engine runs clears the
	// offer, and nothing was withheld, so nothing is logged.
	t.Run("an offer the fleet took is served without a warning", func(t *testing.T) {
		t.Parallel()
		logged := &logBuffer{}
		p := newPlane(t, func(o *engine.ReconcilerOptions) {
			o.Log = slog.New(slog.NewJSONHandler(logged, nil))
		})
		document := p.seal(t, yamlToJSON(t, grownCompanyDoc))
		offered, err := p.store.Configs().Offer(t.Context(), store.Offer{}, store.Revision{
			CreatedByKind: iam.ActorOperator, Source: "file", CreatedBy: "operator",
			Summary: "offline", Payload: document, CreatedAt: pinnedNow,
		})
		if err != nil {
			t.Fatalf("offer: %v", err)
		}
		if _, err := p.fleet.Activate(t.Context(), coord.ActivationRequest{
			RevisionID: offered, Summary: "offline", Payload: document, At: pinnedNow,
			ExpectAbsent: true, Origin: anOrigin,
		}); err != nil {
			t.Fatalf("the boot's publish: %v", err)
		}
		if err := p.recon.Tick(t.Context()); err != nil {
			t.Fatalf("tick: %v", err)
		}
		if got := bootID(t, p); got.ID != offered || got.Offer != nil {
			t.Errorf("this node boots on %s (offer %+v), want %s served", got.ID,
				got.Offer, offered)
		}
		if said := logged.records(t, "offline_revision_not_published"); len(said) != 0 {
			t.Errorf("an offer the fleet took was said to be withheld: %v", said)
		}
	})

	// A revision the fleet took that this node holds only as history, as an
	// API write leaves it: stored, and the pointer moved.
	t.Run("a held revision the fleet took becomes the boot row once applied", func(t *testing.T) {
		t.Parallel()
		p := newPlane(t)
		previous, err := p.store.Configs().Insert(t.Context(), store.Revision{
			CreatedByKind: iam.ActorOperator,
			Source:        "test", CreatedBy: "operator", Summary: "before",
			Payload: yamlToJSON(t, companyDoc), CreatedAt: pinnedNow,
		})
		if err != nil {
			t.Fatalf("store the previous revision: %v", err)
		}
		document := p.seal(t, yamlToJSON(t, grownCompanyDoc))
		held, err := p.store.Configs().Insert(t.Context(), store.Revision{
			CreatedByKind: iam.ActorOperator,
			ParentID:      previous, Source: "api", CreatedBy: "operator", Summary: "written",
			Payload: document, CreatedAt: pinnedNow,
		})
		if err != nil {
			t.Fatalf("store the written revision: %v", err)
		}
		if _, err := p.fleet.Activate(t.Context(), coord.ActivationRequest{
			RevisionID: held, Summary: "written", Payload: document, At: pinnedNow,
			ExpectAbsent: true, Origin: anOrigin,
		}); err != nil {
			t.Fatalf("activate: %v", err)
		}
		if _, found, err := p.store.Configs().Boot(t.Context()); err != nil || found {
			t.Fatalf("the write is the boot row before any apply (found=%v err=%v)", found, err)
		}
		if err := p.recon.Tick(t.Context()); err != nil {
			t.Fatalf("tick: %v", err)
		}
		if got := bootID(t, p); got.ID != held {
			t.Errorf("this node boots on %s, want the one the fleet took, %s", got.ID, held)
		}
	})
}

// THE FLEET ROW SAYS WHAT THIS NODE WOULD COME BACK ON, which is not what it
// is running. The boot row moves only after a successful apply, by a local
// best-effort write, so a refused write leaves the node running one revision
// and booting on another. A reader that keeps what a node needs to start (the
// Kubernetes operator keeps a revision's sealed credentials while a node names
// it) must read the boot revision, or the node's next restart crash-loops on a
// revision it can no longer build. A node refusing the fleet's next revision
// never converges, so the tick out of retries is what repairs the row.
//
// Mutations: read Reconciler.Boots off served rather than the boot row and the
// first assertion fails; drop Reconciler.serve from the out-of-retries branch
// of Tick and the repair never lands.
func TestTheFleetRowSaysWhatThisNodeBootsOn(t *testing.T) {
	t.Parallel()
	p := newPlane(t)
	p.activate(t.Context(), t, companyDoc)
	if err := p.recon.Tick(t.Context()); err != nil {
		t.Fatalf("apply the revision this node boots on: %v", err)
	}
	boots := p.recon.Serving()

	// From here the store refuses boot-row writes, so the apply below leaves
	// this node running one revision and booting on another.
	p.refuseBootRow(t)
	p.activate(t.Context(), t, grownCompanyDoc)
	if err := p.recon.Tick(t.Context()); err != nil {
		t.Fatalf("apply the revision this node runs: %v", err)
	}
	runs := p.recon.Serving()
	if runs == boots || runs == "" {
		t.Fatalf("this node runs and boots on %q; the case needs two revisions", runs)
	}

	// Then the pointer moves to a revision this node cannot build: every
	// tick from here is a retry or one out of retries.
	refused := p.activatePayload(t, "broken", brokenRevision)
	for range configplane.MaxApplyAttempts {
		_ = p.recon.Tick(t.Context())
	}
	boot, found, err := p.store.Configs().Boot(t.Context())
	if err != nil || !found || boot.ID != boots {
		t.Fatalf("this node boots on %s (found=%v err=%v); the case needs the store's "+
			"refusal to have left it on %s", boot.ID, found, err, boots)
	}
	row := p.fleetRow(t)
	if row.Epoch != refused || configplane.ApplyStatus(row.Status) != configplane.StatusError {
		t.Fatalf("the fleet row is %+v, want the refusal at epoch %d", row, refused)
	}
	if row.Serving != runs || row.Boots != boots {
		t.Errorf("the fleet says this node serves %q and boots on %q, want %s and %s — "+
			"a reader keeping what this node needs in order to start reads the second",
			row.Serving, row.Boots, runs, boots)
	}

	// The store takes the write again: the next tick, out of retries on a
	// revision this node still refuses, repairs the boot row and says so.
	p.allowBootRow(t)
	_ = p.recon.Tick(t.Context())
	if boot, _, err = p.store.Configs().Boot(t.Context()); err != nil || boot.ID != runs {
		t.Errorf("this node boots on %s (err %v) once the store took the write, want the "+
			"revision it runs, %s", boot.ID, err, runs)
	}
	if row = p.fleetRow(t); row.Serving != runs || row.Boots != runs {
		t.Errorf("the fleet says this node serves %q and boots on %q, want both %s once "+
			"the boot row was repaired", row.Serving, row.Boots, runs)
	}
}

// A TICK THAT APPLIED NOTHING WRITES NOTHING TO THE BOOT ROW, repair ticks
// included. The row only moves to a revision this engine APPLIED; before the
// first apply the node serves what it BOOTED on, which the row already names
// at the instant this node applied it. On a node that refused a re-activation
// of the revision it runs, the row also carries the fleet's later instant, and
// a tick that wrote the row would stamp that as this node's applied instant,
// which the next boot stamps the chart with. (The same guard leaves an
// offline change offered until an apply replaces it.)
//
// Mutation: drop the applied-epoch guard from Reconciler.serve and the tick
// that refuses the fleet's revision restamps the boot row.
func TestATickThatAppliedNothingWritesNoBootRow(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{})
	configs := e.Backends().Store.Configs()
	applied := store.Revision{
		ID: "0192f00d-0000-7000-8000-00000000b007", CreatedBy: "maya",
		CreatedByKind: iam.ActorHuman, Source: "api", Summary: "what this node booted on",
		Payload:   sealed(t, e, companyDoc),
		CreatedAt: pinnedNow.Add(-time.Hour), ActivatedAt: pinnedNow.Add(-time.Hour),
	}
	if err := configs.Serve(t.Context(), applied); err != nil {
		t.Fatalf("serve the revision this node booted on: %v", err)
	}
	// The re-activation this node refused before it restarted: the row
	// carries the fleet's later instant beside the one this node applied.
	reactivated := applied
	reactivated.ActivatedAt = pinnedNow
	if err := configs.SawFleet(t.Context(), reactivated); err != nil {
		t.Fatalf("record the fleet's re-activation: %v", err)
	}
	booted, found, err := configs.Boot(t.Context())
	if err != nil || !found || !booted.ServedAt.Equal(applied.ActivatedAt) ||
		!booted.ActivatedAt.Equal(reactivated.ActivatedAt) {
		t.Fatalf("this node boots on %s applied at %s (activated %s, found=%v err=%v); "+
			"the case needs the two instants apart", booted.ID, booted.ServedAt,
			booted.ActivatedAt, found, err)
	}
	p := planeFor(t, e, func(o *engine.ReconcilerOptions) { o.Booted = booted })

	p.activatePayload(t, "broken", brokenRevision)
	if err := p.recon.Tick(t.Context()); err == nil {
		t.Fatal("a revision that cannot be built applied cleanly")
	}
	boot, _, err := configs.Boot(t.Context())
	if err != nil || boot.ID != applied.ID || !boot.ServedAt.Equal(applied.ActivatedAt) {
		t.Errorf("this node boots on %s applied at %s (err %v), want %s at the instant it "+
			"applied, %s: no apply of this engine's moved the row", boot.ID, boot.ServedAt,
			err, applied.ID, applied.ActivatedAt)
	}
	if row := p.fleetRow(t); row.Serving != applied.ID || row.Boots != applied.ID {
		t.Errorf("the fleet says this node serves %q and boots on %q, want both to be the "+
			"revision it booted on, %s", row.Serving, row.Boots, applied.ID)
	}
}

// sealed is a document as a writer of e's fleet stores it, for a case that
// writes a revision into the store without a reconciler of its own.
func sealed(t *testing.T, e *engine.Engine, doc string) json.RawMessage {
	t.Helper()
	payload, err := secrets.Seal(e.Cipher(), yamlToJSON(t, doc))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	return payload
}

// A NODE WHOSE DATABASE IS DISCARDED AT EVERY START BOOTS ON NOTHING, however
// many revisions it applies. Its boot row serves its own readers while it
// runs (a config write's base, an offer's) and nobody after, since the next
// start deletes the file: naming it in the fleet view would have a reader keep
// what this node will never use. The reconciler learns scratch from the store
// handle, the one reading that cannot disagree with the open; an option
// restating it is what a harness forgets.
//
// Mutations: drop the scratch branch of Reconciler.Boots, or build the
// reconciler's scratch from anything but the handle (false), and the row
// names the revision the next start deletes.
func TestAScratchNodeBootsOnNothing(t *testing.T) {
	t.Parallel()
	scratch := storetest.OpenNode(t, filepath.Join(t.TempDir(), "node.db"),
		store.Options{Scratch: true})
	t.Cleanup(func() { _ = scratch.Close() })
	if !scratch.Scratch() {
		t.Fatal("a store opened scratch says it keeps what it writes")
	}
	// The ENGINE's store holds this case's history; the scratch one is the
	// node's own, as without `data`, meeting every revision via the pointer.
	p := newPlane(t, func(o *engine.ReconcilerOptions) { o.Store = scratch })
	p.activate(t.Context(), t, grownCompanyDoc)
	if err := p.recon.Tick(t.Context()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	row := p.fleetRow(t)
	if row.Serving == "" || row.Boots != "" {
		t.Errorf("this node serves %q and reports booting on %q, want the revision it "+
			"applied and nothing at all", row.Serving, row.Boots)
	}
	boot, found, err := scratch.Configs().Boot(t.Context())
	if err != nil || !found || boot.ID != row.Serving {
		t.Errorf("the boot row holds %s (found=%v err=%v), want the revision this node "+
			"applied, %s: it is written for this node's own readers and reported to "+
			"nobody", boot.ID, found, err, row.Serving)
	}
}

// THE FLEET ROW NAMES THE BOOT REVISION ON THE TICK THAT MOVES IT. Its boot
// name never runs AHEAD of the store (it moves only on a successful read or
// write of the boot row), so writing the boot row before the fleet's record
// only makes the row say it sooner: a clean apply's record names the new boot
// revision, and a converged tick that repairs a failed write says so on that
// tick. Otherwise a reader watching a rollout sees two names differ for a
// tick, which reads as a boot row that failed to move.
//
// Mutations: record before serve in Reconciler.apply and the first half reads
// the previous boot revision; serve after refresh in Tick's converged branch
// and the second half reads the unrepaired one.
func TestTheFleetRowNamesTheBootRevisionOnTheTickThatMovesIt(t *testing.T) {
	t.Parallel()
	// A NODE THAT ALREADY BOOTS ON SOMETHING, so that the name the row
	// would carry one tick late is a revision rather than none.
	e := newEngine(t, engine.Options{})
	booted := bootOn(t, e, companyDoc)
	p := planeFor(t, e, func(o *engine.ReconcilerOptions) { o.Booted = booted })

	p.activate(t.Context(), t, grownCompanyDoc)
	if err := p.recon.Tick(t.Context()); err != nil {
		t.Fatalf("apply the fleet's revision: %v", err)
	}
	second := p.recon.Serving()
	if second == booted.ID {
		t.Fatalf("this node still runs %s; the case needs an apply past it", second)
	}
	if row := p.fleetRow(t); row.Serving != second || row.Boots != second {
		t.Errorf("after a clean apply the fleet says this node serves %q and boots on "+
			"%q, want both %s on the tick that applied it rather than %s", row.Serving,
			row.Boots, second, booted.ID)
	}

	// A write to the boot row that fails, and the converged tick that
	// repairs it once the store takes writes again.
	p.refuseBootRow(t)
	p.activate(t.Context(), t, companyDoc)
	if err := p.recon.Tick(t.Context()); err != nil {
		t.Fatalf("apply the third revision: %v", err)
	}
	third := p.recon.Serving()
	if row := p.fleetRow(t); row.Serving != third || row.Boots != second {
		t.Fatalf("the fleet says this node serves %q and boots on %q, want %s and %s; "+
			"the case needs the boot row's write to have failed", row.Serving, row.Boots,
			third, second)
	}
	p.allowBootRow(t)
	if err := p.recon.Tick(t.Context()); err != nil {
		t.Fatalf("converged tick: %v", err)
	}
	if row := p.fleetRow(t); row.Boots != third {
		t.Errorf("on the converged tick that repaired the boot row the fleet says this "+
			"node boots on %q, want %s", row.Boots, third)
	}
}

// A BOOT ROW THIS NODE COULD NOT READ IS NEVER PUBLISHED AS NONE. An empty
// boot name says the node boots on nothing, a claim a reader acts on (the
// Kubernetes operator keeps a revision's sealed credentials while a node names
// it), so it comes only from a read that found the row empty, never one that
// failed. The row is read as the reconciler is built and a later failed read
// leaves the last answer standing; learned on the first tick instead, a read
// failing there published an empty boot revision, and the node's next start
// could not build what it booted on.
//
// Mutations: drop the read from Engine.NewReconciler and the record names no
// boot revision; record an empty one when Reconciler.serve's read fails and so
// does it.
func TestABootRowThisNodeCouldNotReadIsNeverPublishedAsNone(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{})
	booted := bootOn(t, e, companyDoc)
	p := planeFor(t, e, func(o *engine.ReconcilerOptions) { o.Booted = booted })

	// From here every read of the boot row fails, the reads the apply
	// itself makes do not, and the store still holds booted there.
	restore := unreadableBootRow(t, p.store)
	p.activate(t.Context(), t, grownCompanyDoc)
	if err := p.recon.Tick(t.Context()); err != nil {
		t.Fatalf("apply the fleet's revision: %v", err)
	}
	runs := p.recon.Serving()
	if runs == booted.ID || runs == "" {
		t.Fatalf("this node runs %q; the case needs an apply past %s", runs, booted.ID)
	}
	if held := bootRowID(t, p.store); held != booted.ID {
		t.Fatalf("the boot row holds %s; the case needs the failed reads to have kept "+
			"the apply from moving it off %s", held, booted.ID)
	}
	if row := p.fleetRow(t); row.Serving != runs || row.Boots != booted.ID {
		t.Errorf("the fleet says this node serves %q and boots on %q, want %s and the "+
			"revision its boot row still holds, %s", row.Serving, row.Boots, runs, booted.ID)
	}

	// The row reads again: the converged tick moves it, and says so.
	restore()
	if err := p.recon.Tick(t.Context()); err != nil {
		t.Fatalf("converged tick: %v", err)
	}
	if row := p.fleetRow(t); row.Boots != runs || bootRowID(t, p.store) != runs {
		t.Errorf("the fleet says this node boots on %q once the row reads again, want %s",
			row.Boots, runs)
	}
}

// A RECONCILER THAT CANNOT READ WHAT ITS NODE BOOTS ON IS NOT BUILT, as the boot
// fails on a store it cannot read: built anyway, its first record would have
// to name a boot revision it does not know.
//
// Mutation: ignore the read's error in Engine.NewReconciler and the reconciler
// is built.
func TestAReconcilerThatCannotReadWhatItsNodeBootsOnIsNotBuilt(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{})
	bootOn(t, e, companyDoc)
	unreadableBootRow(t, e.Backends().Store)
	r, err := e.NewReconciler(t.Context(), engine.ReconcilerOptions{
		Store: e.Backends().Store, Fleet: e.Backends().Fleet, Queue: e.Backends().Queue,
		NodeID: "node-a",
	})
	if err == nil {
		t.Fatalf("a reconciler was built over a boot row it could not read (boots on %q)",
			r.Boots())
	}
}

// THE FLEET ROW SAYS WHICH EPOCH THIS NODE LAST APPLIED, beside the one it last
// attempted. A refusal says only that the node did not reach the attempted
// epoch, so without this a writer that saved epoch 6 could not tell whether a
// refusing node runs 5 or nothing; the fleet view compares it per node, as the
// health push does with its live epoch. 0 before the first apply succeeds,
// which the view leaves out.
//
// Mutation: record the attempted epoch as the applied one and the refusal
// reads as epoch 6 applied.
func TestTheFleetRowSaysWhichEpochThisNodeLastApplied(t *testing.T) {
	t.Parallel()
	p := newPlane(t)
	refusedFirst := p.activatePayload(t, "broken", brokenRevision)
	if err := p.recon.Tick(t.Context()); err == nil {
		t.Fatal("a revision that cannot be built applied cleanly")
	}
	if row := p.fleetRow(t); row.Epoch != refusedFirst || row.Applied != 0 {
		t.Errorf("before any apply the fleet says this node attempted epoch %d and "+
			"applied %d, want %d and 0", row.Epoch, row.Applied, refusedFirst)
	}

	// Four activations move the pointer with no tick between them, so the one
	// apply below is of the last of them.
	var applied int64
	for range 4 {
		applied = p.activate(t.Context(), t, companyDoc)
	}
	if err := p.recon.Tick(t.Context()); err != nil {
		t.Fatalf("apply epoch %d: %v", applied, err)
	}
	refused := p.activatePayload(t, "broken", brokenRevision)
	if err := p.recon.Tick(t.Context()); err == nil {
		t.Fatal("a revision that cannot be built applied cleanly")
	}
	row := p.fleetRow(t)
	if row.Epoch != refused || row.Applied != applied ||
		configplane.ApplyStatus(row.Status) != configplane.StatusError {
		t.Errorf("the fleet says this node attempted epoch %d (%s) and applied %d, want "+
			"the refusal of %d beside the %d it runs", row.Epoch, row.Status, row.Applied,
			refused, applied)
	}
}

// bootOn makes doc what e's node boots on, as an earlier run of the node left
// its store — applied at an instant an hour before [pinnedNow] — and returns
// the row as the boot reads it.
func bootOn(t *testing.T, e *engine.Engine, doc string) store.Revision {
	t.Helper()
	configs := e.Backends().Store.Configs()
	if err := configs.Serve(t.Context(), store.Revision{
		ID: uuid.NewString(), CreatedBy: "maya", CreatedByKind: iam.ActorHuman,
		Source: "api", Summary: "what this node booted on", Payload: sealed(t, e, doc),
		CreatedAt: pinnedNow.Add(-time.Hour), ActivatedAt: pinnedNow.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("serve the revision this node boots on: %v", err)
	}
	booted, found, err := configs.Boot(t.Context())
	if err != nil || !found {
		t.Fatalf("this node boots on nothing (found=%v err=%v)", found, err)
	}
	return booted
}

// unreadableBootRow makes every read of db's boot row fail, leaving the row in
// place and every other row readable, and returns what puts it back. It writes
// the served instant as text, so the read fails scanning THIS row (a corrupt
// page, a column a newer build wrote differently) rather than losing the whole
// file, which the apply would trip over too.
func unreadableBootRow(t *testing.T, db *store.DB) (restore func()) {
	t.Helper()
	var served int64
	if err := db.SQL().QueryRowContext(t.Context(),
		`SELECT served_at FROM company_config WHERE is_active <> 0`).Scan(&served); err != nil {
		t.Fatalf("read the boot row's served instant: %v", err)
	}
	set := func(value any) {
		t.Helper()
		if _, err := db.SQL().ExecContext(t.Context(),
			`UPDATE company_config SET served_at = ? WHERE is_active <> 0`, value); err != nil {
			t.Fatalf("write the boot row's served instant: %v", err)
		}
	}
	set("not an instant")
	if _, _, err := db.Configs().Boot(t.Context()); err == nil {
		t.Fatal("the boot row still reads; the case needs a read of it to fail")
	}
	return func() { set(served) }
}

// bootRowID is the revision db's boot row holds, read past the store's own
// reader so that a case can see it while that reader fails.
func bootRowID(t *testing.T, db *store.DB) string {
	t.Helper()
	var id string
	if err := db.SQL().QueryRowContext(t.Context(),
		`SELECT revision_id FROM company_config WHERE is_active <> 0`).Scan(&id); err != nil {
		t.Fatalf("read the boot row: %v", err)
	}
	return id
}

// A REVISION THIS NODE REFUSES NEVER BECOMES WHAT IT BOOTS ON, or a revision
// one node's build cannot run takes that node down at its next restart,
// however healthy it was. What the node RECORDS of it is that the fleet is on
// it: the company it restores if the pointer is lost, and what its fleet-view
// row names beside the one it serves.
//
// Mutation: make Reconciler.keep serve the revision (the old adopt), or call
// Reconciler.serve on a refused apply, and the boot row is the refused one.
func TestARevisionThisNodeRefusesNeverBecomesWhatItBootsOn(t *testing.T) {
	t.Parallel()
	author, node := fleetPeers(t)
	author.activate(t.Context(), t, grownCompanyDoc)
	served, _, err := author.fleet.Target(t.Context())
	if err != nil {
		t.Fatalf("Target: %v", err)
	}
	if err := node.recon.Tick(t.Context()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	for _, refusal := range []struct {
		name     string
		document json.RawMessage
		// runnable is whether the runnable rules refuse it, before the
		// engine sees it, rather than the engine's own build.
		runnable bool
	}{
		{"invalid", json.RawMessage(`{"name":"Acme","roles":[{"name":"CEO","handle":"ceo"},` +
			`{"name":"Again","handle":"ceo"}]}`), true},
		{"unbuildable", yamlToJSON(t, unbuildableCompanyDoc), false},
	} {
		name := refusal.name
		epoch := author.activatePayload(t, name, refusal.document)
		target, _, err := author.fleet.Target(t.Context())
		if err != nil {
			t.Fatalf("Target: %v", err)
		}
		err = node.recon.Tick(t.Context())
		if err == nil {
			t.Fatalf("%s: the refused revision applied cleanly", name)
		}
		if got := strings.Contains(err.Error(), "is not a runnable company"); got != refusal.runnable {
			t.Fatalf("%s: refused by the runnable rules = %v, want %v: %v",
				name, got, refusal.runnable, err)
		}
		boot, found, err := node.store.Configs().Boot(t.Context())
		if err != nil || !found || boot.ID != served.RevisionID {
			t.Errorf("%s: this node boots on %s (found=%v err=%v), want the revision it "+
				"serves, %s", name, boot.ID, found, err, served.RevisionID)
		}
		fleets, found, err := node.store.Configs().Fleets(t.Context())
		if err != nil || !found || fleets.ID != target.RevisionID {
			t.Errorf("%s: the fleet's revision is recorded as %s (found=%v err=%v), "+
				"want the refused %s", name, fleets.ID, found, err, target.RevisionID)
		}
		if got := node.recon.Serving(); got != served.RevisionID {
			t.Errorf("%s: Serving = %s, want %s", name, got, served.RevisionID)
		}
		row := author.fleetRow(t)
		if row.Epoch != epoch || row.RevisionID != target.RevisionID ||
			row.Serving != served.RevisionID ||
			configplane.ApplyStatus(row.Status) != configplane.StatusError {
			t.Errorf("%s: the fleet view says %+v, want an error at epoch %d on %s, "+
				"serving %s", name, row, epoch, target.RevisionID, served.RevisionID)
		}
	}
}

// THIS NODE'S OWN READERS ARE TOLD WHAT ITS LAST ATTEMPT MADE OF A REVISION.
// A refusal leaves the applied epoch where it was, which read alone looks like
// a node still applying; the health push carries [engine.Reconciler.Attempt]
// beside it, so a reader who may not read the fleet (a unit lead following
// their save) learns of the refusal. Nothing before the first concluded
// attempt, which a zero epoch would misstate.
//
// Mutation: report the attempt only once one succeeded, and the refusal reads
// as the earlier success.
func TestTheLastAttemptSaysARefusalTheAppliedEpochCannot(t *testing.T) {
	t.Parallel()
	p := newPlane(t)
	if attempt, ok := p.recon.Attempt(); ok {
		t.Fatalf("an attempt %+v before any tick", attempt)
	}
	first := p.activate(t.Context(), t, grownCompanyDoc)
	if err := p.recon.Tick(t.Context()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	refused := p.activatePayload(t, "broken", brokenRevision)
	target, _, err := p.fleet.Target(t.Context())
	if err != nil {
		t.Fatalf("Target: %v", err)
	}
	if err := p.recon.Tick(t.Context()); err == nil {
		t.Fatal("a revision that cannot be built applied cleanly")
	}
	if p.recon.Applied() != first {
		t.Fatalf("applied = %d after the refusal, want %d", p.recon.Applied(), first)
	}
	attempt, ok := p.recon.Attempt()
	if !ok || attempt.Epoch != refused || attempt.RevisionID != target.RevisionID ||
		configplane.ApplyStatus(attempt.Status) != configplane.StatusError {
		t.Errorf("the last attempt is %+v (reported %v), want an error at epoch %d on %s",
			attempt, ok, refused, target.RevisionID)
	}
}

// A NODE SAYS WHAT IT BOOTED ON UNTIL IT APPLIES SOMETHING ELSE: one that
// refuses the fleet's revision still runs its boot row's company, which its
// fleet-view row must show, and before its first apply the reconciler knows
// that only from what it was told the engine started on.
//
// Mutation: ignore ReconcilerOptions.Booted and the row serves nothing.
func TestANodeServesWhatItBootedOnUntilAnApplySucceeds(t *testing.T) {
	t.Parallel()
	booted := store.Revision{ID: "0192f00d-0000-7000-8000-0000000b0070", ActivatedAt: pinnedNow}
	p := newPlane(t, func(o *engine.ReconcilerOptions) { o.Booted = booted })
	if got := p.recon.Serving(); got != booted.ID {
		t.Fatalf("Serving = %q before any tick, want what the engine booted on, %s", got, booted.ID)
	}
	p.activatePayload(t, "broken", brokenRevision)
	if err := p.recon.Tick(t.Context()); err == nil {
		t.Fatal("a revision that cannot be built applied cleanly")
	}
	if row := p.fleetRow(t); row.Serving != booted.ID {
		t.Errorf("the fleet view says this node serves %q, want %s", row.Serving, booted.ID)
	}
	p.activate(t.Context(), t, grownCompanyDoc)
	if err := p.recon.Tick(t.Context()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	target, _, err := p.fleet.Target(t.Context())
	if err != nil {
		t.Fatalf("Target: %v", err)
	}
	if got := p.recon.Serving(); got != target.RevisionID {
		t.Errorf("Serving = %q after the apply, want %s", got, target.RevisionID)
	}
}

// AN EPOCH IS NOT A REVISION: a pointer recreated under a running node can
// name another revision at the epoch this node already applied. Convergence is
// by epoch (the pointer's contract), so the node does not apply it, and the
// boot row must not name it either: the next boot would start on a revision
// the engine never validated.
//
// Mutation: serve the converged tick's target rather than what the engine
// applied and the boot row is the recreated pointer's revision.
func TestAReusedEpochNeverMarksAnUnappliedRevisionAsTheBootRow(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{})
	fleet := &swappablePlane{}
	fleet.current.Store(&planeBox{e.Backends().Fleet})
	p := planeFor(t, e, func(o *engine.ReconcilerOptions) { o.Fleet = fleet })
	p.fleet = e.Backends().Fleet
	epoch := p.activate(t.Context(), t, grownCompanyDoc)
	applied, _, err := p.fleet.Target(t.Context())
	if err != nil {
		t.Fatalf("Target: %v", err)
	}
	if err := p.recon.Tick(t.Context()); err != nil {
		t.Fatalf("tick: %v", err)
	}

	// A FRESH COORDINATION STORE whose activation at the same epoch names
	// another revision this node holds: the stores count epochs apart (the
	// KV's body takes a revision of its own), so it activates up to it.
	recreated := memory.NewFleet()
	document := p.seal(t, yamlToJSON(t, companyDoc))
	other, err := p.store.Configs().Insert(t.Context(), store.Revision{
		CreatedByKind: iam.ActorOperator, Source: "test", CreatedBy: "operator",
		Summary: "elsewhere", Payload: document, CreatedAt: pinnedNow,
	})
	if err != nil {
		t.Fatalf("store the other revision: %v", err)
	}
	var published coord.Activation
	for published.Epoch < epoch {
		if published, err = recreated.Activate(t.Context(), coord.ActivationRequest{
			RevisionID: other, Summary: "elsewhere", Payload: document, At: pinnedNow.Add(time.Hour),
			Origin: anOrigin,
		}); err != nil {
			t.Fatalf("activate on the recreated store: %v", err)
		}
	}
	if published.Epoch != epoch {
		t.Fatalf("the recreated store is at epoch %d, want the reused %d", published.Epoch, epoch)
	}
	fleet.current.Store(&planeBox{recreated})
	if err := p.recon.Tick(t.Context()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	boot, found, err := p.store.Configs().Boot(t.Context())
	if err != nil || !found || boot.ID != applied.RevisionID {
		t.Errorf("this node boots on %s (found=%v err=%v), want the revision its "+
			"engine applied, %s", boot.ID, found, err, applied.RevisionID)
	}
}

// swappablePlane is a coordination plane a case can replace under a running
// reconciler, as a rebuilt coordination store is replaced under a node.
type swappablePlane struct{ current atomic.Pointer[planeBox] }

// planeBox holds a plane in an atomic pointer, which takes a concrete type.
type planeBox struct{ coord.Plane }

func (s *swappablePlane) plane() coord.Plane { return s.current.Load().Plane }

func (s *swappablePlane) Activate(ctx context.Context, req coord.ActivationRequest) (coord.Activation, error) {
	return s.plane().Activate(ctx, req)
}

func (s *swappablePlane) Target(ctx context.Context) (coord.Activation, bool, error) {
	return s.plane().Target(ctx)
}

func (s *swappablePlane) Body(ctx context.Context, a coord.Activation) (json.RawMessage, bool, error) {
	return s.plane().Body(ctx, a)
}

func (s *swappablePlane) BodyCrossing(a coord.Activation) time.Duration {
	return s.plane().BodyCrossing(a)
}

func (s *swappablePlane) RecordApply(ctx context.Context, status coord.NodeApply) error {
	return s.plane().RecordApply(ctx, status)
}

func (s *swappablePlane) Fleet(ctx context.Context) ([]coord.NodeApply, error) {
	return s.plane().Fleet(ctx)
}

// TWO TICKS ON ONE ENGINE FOLLOW THE POINTER IN ITS ORDER: interleaved, the
// tick that read the OLDER epoch could apply last and walk the boot row back
// to it. The first tick here is held inside its read of the older revision's
// body until the pointer has moved on and, unless something orders them, a
// second tick has read and applied the newer one.
//
// Mutation: drop Engine.following from Tick and the second tick runs inside
// the first, which then applies the older revision last.
func TestTwoTicksOnOneEngineServeInPointerOrder(t *testing.T) {
	t.Parallel()
	author, node := fleetPeers(t)
	author.activate(t.Context(), t, companyDoc)
	older, _, err := author.fleet.Target(t.Context())
	if err != nil {
		t.Fatalf("Target: %v", err)
	}

	secondRead := make(chan struct{}, 1)
	secondApplied := make(chan struct{}, 1)
	second := planeFor(t, node.engine, func(o *engine.ReconcilerOptions) {
		o.Fleet = &readSignal{Plane: author.fleet, read: secondRead}
		o.NodeID = "node-a-second"
		o.OnApply = func(int64, configplane.ApplyStatus) {
			select {
			case secondApplied <- struct{}{}:
			default:
			}
		}
	})
	during := func() {
		author.activate(t.Context(), t, grownCompanyDoc)
		// Long enough for an unordered second tick to read the newer
		// pointer and apply it; an ordered one is waiting on the first.
		select {
		case <-secondRead:
			<-secondApplied
		case <-time.After(200 * time.Millisecond):
		}
	}
	held := &heldPayload{Plane: author.fleet, revision: older.RevisionID,
		entered: make(chan struct{}), during: during}
	first := planeFor(t, node.engine, func(o *engine.ReconcilerOptions) { o.Fleet = held })

	var wg sync.WaitGroup
	wg.Go(func() { _ = first.recon.Tick(t.Context()) })
	// The second tick starts once the first is inside the older body.
	<-held.entered
	wg.Go(func() { _ = second.recon.Tick(t.Context()) })
	wg.Wait()

	newer, _, err := author.fleet.Target(t.Context())
	if err != nil {
		t.Fatalf("Target: %v", err)
	}
	boot, found, err := node.store.Configs().Boot(t.Context())
	if err != nil || !found || boot.ID != newer.RevisionID {
		t.Errorf("this node boots on %s (found=%v err=%v), want the newer revision %s "+
			"rather than the older %s", boot.ID, found, err, newer.RevisionID, older.RevisionID)
	}
}

// heldPayload runs during inside the first read of one revision's body, after
// the body has been read: the window between a tick's read of the pointer and
// its apply.
type heldPayload struct {
	coord.Plane
	revision string
	during   func()
	once     sync.Once
	entered  chan struct{}
}

func (h *heldPayload) Body(ctx context.Context, a coord.Activation) (json.RawMessage, bool, error) {
	payload, found, err := h.Plane.Body(ctx, a)
	if a.RevisionID == h.revision {
		h.once.Do(func() {
			close(h.entered)
			h.during()
		})
	}
	return payload, found, err
}

// readSignal says when a tick has read the pointer.
type readSignal struct {
	coord.Plane
	read chan struct{}
}

func (r *readSignal) Target(ctx context.Context) (coord.Activation, bool, error) {
	select {
	case r.read <- struct{}{}:
	default:
	}
	return r.Plane.Target(ctx)
}

func TestARevisionThatCannotBeBuiltLeavesTheNodeServing(t *testing.T) {
	t.Parallel()
	// error, not degraded: the build touches nothing, so this node still
	// serves the PRIOR epoch correctly and is safe to route work to.
	p := newPlane(t)
	before := p.engine.Company()
	// Refused at build, not by the runnable rules: see brokenRevision.
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

// bodiless is a plane whose pointer names a body it no longer holds: what a
// node sees when the pointer moved on between its reads of the target and the
// body, and the sweep took the body.
type bodiless struct{ coord.Plane }

func (bodiless) Body(context.Context, coord.Activation) (json.RawMessage, bool, error) {
	return nil, false, nil
}

func TestAPointerNamingAMissingRevisionIsReported(t *testing.T) {
	t.Parallel()
	// A fleet where every node quietly ignores an unreadable pointer
	// converges on nothing while reporting convergence.
	e := newEngine(t, engine.Options{})
	p := planeFor(t, e, func(o *engine.ReconcilerOptions) {
		o.Fleet = bodiless{o.Fleet}
	})
	// A revision in neither store: the node's database has never seen it,
	// and the plane has no body for it.
	if _, err := p.fleet.Activate(t.Context(), coord.ActivationRequest{
		RevisionID: "00000000-0000-0000-0000-000000000000",
		Summary:    "ghost", Payload: []byte("{}"), At: pinnedNow, Origin: anOrigin}); err != nil {
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
	// Per epoch, not per node lifetime, so activating a FIXED revision resets
	// the budget; unbounded, a bad revision rebuilds this node's subsystems
	// every tick.
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
	// The fixed revision's settings AND its seats: a revision carries both.
	if got := dayCeiling(p.engine.Company()); got != grownDayCeiling {
		t.Errorf("token budget = %d, want the fixed revision's", got)
	}
	if got := p.seats(t); len(got) != 3 {
		t.Errorf("seats = %v, want the fixed revision's three", got)
	}
}

// A REVISION SEALED UNDER A KEY THIS NODE DOES NOT HOLD IS AN ERROR, NEVER AN
// EMPTY COMPANY, which would read on every surface as an operator who
// configured nothing, where the fault is the keyring: another fleet's Tier A,
// or a key dropped before every revision was re-sealed off it. The control is
// the same document sealed under this node's own keyring.
func TestASealedRevisionNeedsTheKeyItWasSealedUnder(t *testing.T) {
	t.Parallel()
	foreign, err := secrets.NewCipher(secrets.Keyring{
		ActiveID: "elsewhere", Keys: map[string][]byte{"elsewhere": testKey(t)},
	})
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := secrets.Seal(foreign, yamlToJSON(t, grownCompanyDoc))
	if err != nil {
		t.Fatal(err)
	}

	p := newPlane(t)
	if _, err := p.fleet.Activate(t.Context(), coord.ActivationRequest{
		RevisionID: "sealed-elsewhere", Summary: "sealed", Payload: sealed, At: pinnedNow,
		Origin: anOrigin,
	}); err != nil {
		t.Fatalf("activate: %v", err)
	}
	if err := p.recon.Tick(t.Context()); !errors.Is(err, secrets.ErrDecrypt) {
		t.Fatalf("err = %v, want the revision refused as undecryptable", err)
	}
	if got := dayCeiling(p.engine.Company()); got == grownDayCeiling {
		t.Error("the node runs a revision it could not open")
	}

	// THE CONTROL: its own keyring opens it.
	p.activatePayload(t, "sealed", yamlToJSON(t, grownCompanyDoc))
	if err := p.recon.Tick(t.Context()); err != nil {
		t.Fatalf("a revision sealed under this node's keyring: %v", err)
	}
	if got := dayCeiling(p.engine.Company()); got != grownDayCeiling {
		t.Errorf("token budget = %d, want the sealed revision's", got)
	}
}

// A PEER'S REVISION THAT IS NOT SEALED IS NEITHER APPLIED NOR KEPT. A peer
// reads the payload off the coordination store, which anything the broker
// admits can write, so the seal under the fleet's keyring is what says a node
// of this fleet wrote it. An unsealed body is recorded as an error, its epoch
// is not reached, and it never lands in this node's config history, where it
// would be shown, diffed and offered as a revert target. The forgery goes
// straight onto the plane, since every writer this build has seals. The
// control, the same document sealed by a node of the fleet, is applied and kept.
//
// Mutation: let secrets.Open hand back a plaintext payload with a keyring in
// hand, and the forged revision applies; keep it before the open in
// Reconciler.applyRevision and it is kept although it is refused.
func TestAnUnsealedRevisionFromThePeersIsNeitherAppliedNorKept(t *testing.T) {
	t.Parallel()
	author, node := fleetPeers(t)

	forged, err := author.fleet.Activate(t.Context(), coord.ActivationRequest{
		RevisionID: "0192f00d-0000-7000-8000-00000000f0f0", Summary: "forged",
		Payload: yamlToJSON(t, grownCompanyDoc), At: pinnedNow, Origin: anOrigin,
	})
	if err != nil {
		t.Fatalf("publish the forgery: %v", err)
	}
	err = node.recon.Tick(t.Context())
	if !errors.Is(err, secrets.ErrUnsealedWithKey) {
		t.Fatalf("Tick = %v, want the unsealed-with-a-keyring refusal", err)
	}
	// It says where the body came from (the coordination store, which
	// anything reaching the broker can write) and what replaces it: an
	// admin's whole document.
	for _, says := range []string{"coordination store", "without a seal",
		"anything that reaches the broker", "An admin", "whole document", "PUT /config"} {
		if !strings.Contains(err.Error(), says) {
			t.Errorf("the refusal does not say %q: %v", says, err)
		}
	}
	if got := node.recon.Applied(); got == forged.Epoch {
		t.Error("the node applied the epoch an unsealed revision was published at")
	}
	if got := dayCeiling(node.engine.Company()); got == grownDayCeiling {
		t.Error("the node runs the forged document's settings")
	}
	// The node reports into the fleet view it reads, the author's; the
	// author never ticked, so the one row is the node's.
	if row := author.fleetRow(t); configplane.ApplyStatus(row.Status) != configplane.StatusError ||
		row.Epoch != forged.Epoch {
		t.Errorf("the node recorded %+v, want an error against epoch %d", row, forged.Epoch)
	}
	if _, held, err := node.store.Configs().Get(t.Context(), forged.RevisionID); err != nil || held {
		t.Errorf("the forged revision is in the node's own history (held=%v err=%v)", held, err)
	}

	// THE CONTROL: the same document, sealed by a node of the fleet.
	sealedEpoch := author.activate(t.Context(), t, grownCompanyDoc)
	sealedTarget, _, err := author.fleet.Target(t.Context())
	if err != nil {
		t.Fatalf("Target: %v", err)
	}
	if err := node.recon.Tick(t.Context()); err != nil {
		t.Fatalf("a sealed revision from the peers: %v", err)
	}
	if got := node.recon.Applied(); got != sealedEpoch {
		t.Errorf("the node applied epoch %d, want the sealed revision's %d", got, sealedEpoch)
	}
	if _, held, err := node.store.Configs().Get(t.Context(), sealedTarget.RevisionID); err != nil || !held {
		t.Errorf("the sealed revision was not kept (held=%v err=%v)", held, err)
	}
}

// THIS NODE'S OWN UNSEALED COPY IS NAMED AS ITS OWN: the fleet can point at a
// revision this node holds in the clear, which nobody else published, so the
// refusal names the remedy, an import HERE, which stores it sealed.
//
// Mutation: answer the local copy with the peer's wording and the case fails.
func TestThisNodesOwnUnsealedCopyIsNamedAsItsOwn(t *testing.T) {
	t.Parallel()
	p := newPlane(t)
	document := yamlToJSON(t, grownCompanyDoc)
	id, err := p.store.Configs().Insert(t.Context(), store.Revision{
		Source: "test", CreatedBy: "operator", CreatedByKind: iam.ActorOperator,
		Summary: "unsealed", Payload: document, CreatedAt: pinnedNow,
	})
	if err != nil {
		t.Fatalf("store the revision: %v", err)
	}
	if _, err := p.fleet.Activate(t.Context(), coord.ActivationRequest{
		RevisionID: id, Summary: "unsealed", Payload: document, At: pinnedNow,
		Origin: anOrigin,
	}); err != nil {
		t.Fatalf("activate: %v", err)
	}
	err = p.recon.Tick(t.Context())
	if !errors.Is(err, secrets.ErrUnsealedWithKey) {
		t.Fatalf("Tick = %v, want the unsealed-with-a-keyring refusal", err)
	}
	for _, says := range []string{"this node's own copy", id, "An admin",
		"whole document", "crewlet config import"} {
		if !strings.Contains(err.Error(), says) {
			t.Errorf("the refusal does not say %q: %v", says, err)
		}
	}
	if strings.Contains(err.Error(), "broker") {
		t.Errorf("this node's own copy was answered as a forgery: %v", err)
	}
}

// A PEER'S REVISION SEALED IN A FORM THIS BUILD DOES NOT READ IS NOT UNSEALED.
// A newer build's seal (here `enc:v2:`) is wrapped as this build's is; read as
// no seal, the error would say the body came from outside the fleet and send
// the operator to replace a whole document, discarding what the newer peer
// wrote. It is refused, neither applied nor kept, as every body that does not
// open is, and the error names the remedy: the build that sealed it.
//
// Mutation: answer every seal secrets cannot parse as ErrUnsealedWithKey, or
// drop openRefusal's arm for it, and the refusal speaks of a missing seal.
func TestAPeersRevisionSealedInAFormThisBuildDoesNotReadIsNotUnsealed(t *testing.T) {
	t.Parallel()
	author, node := fleetPeers(t)

	newer, err := author.fleet.Activate(t.Context(), coord.ActivationRequest{
		RevisionID: "0192f00d-0000-7000-8000-00000000f0f1", Summary: "from a newer build",
		Payload: []byte(`{"__encrypted__":"enc:v2:k2:QUJDREVGR0hJSktMTU5PUA=="}`),
		At:      pinnedNow, Origin: anOrigin,
	})
	if err != nil {
		t.Fatalf("publish the newer build's revision: %v", err)
	}
	err = node.recon.Tick(t.Context())
	if !errors.Is(err, secrets.ErrUnknownEnvelope) || errors.Is(err, secrets.ErrUnsealedWithKey) {
		t.Fatalf("Tick = %v, want a seal this build does not read, never an unsealed body", err)
	}
	for _, says := range []string{newer.RevisionID, "form this build doesn't read",
		"newer build", "upgrade this node"} {
		if !strings.Contains(err.Error(), says) {
			t.Errorf("the refusal does not say %q: %v", says, err)
		}
	}
	for _, never := range []string{"without a seal", "PUT /config"} {
		if strings.Contains(err.Error(), never) {
			t.Errorf("the refusal says %q, which sends the operator to replace "+
				"a document somebody sealed: %v", never, err)
		}
	}
	if got := node.recon.Applied(); got == newer.Epoch {
		t.Error("the node applied the epoch a revision it cannot read was published at")
	}
	if _, held, err := node.store.Configs().Get(t.Context(), newer.RevisionID); err != nil || held {
		t.Errorf("the revision is in the node's own history (held=%v err=%v)", held, err)
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
	// The apply-status table upserts on node id, so nodes with an empty one
	// would share one anonymous row in the fleet view.
	e := newEngine(t, engine.Options{})
	if _, err := e.NewReconciler(t.Context(), engine.ReconcilerOptions{NodeID: "n"}); !errors.Is(err, engine.ErrNoStore) {
		t.Errorf("err = %v, want ErrNoStore", err)
	}
	if _, err := e.NewReconciler(t.Context(), engine.ReconcilerOptions{
		Store: e.Backends().Store, Fleet: e.Backends().Fleet,
	}); !errors.Is(err, engine.ErrNoPublisher) {
		t.Errorf("err = %v, want ErrNoPublisher", err)
	}
	if _, err := e.NewReconciler(t.Context(), engine.ReconcilerOptions{
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
	// Nothing to converge on: any posture but serve would take every node of
	// a brand-new deployment out of rotation before its first import.
	p := newPlane(t)
	if got := p.recon.Posture(t.Context()); got != configplane.PostureServe {
		t.Errorf("posture = %q, want serve", got)
	}
}

func TestOrdinaryPropagationLagIsNotDivergence(t *testing.T) {
	t.Parallel()
	// EVERY successful rollout leaves peers behind until they poll; shedding
	// on that makes the fastest node the cause of a fleet-wide outage.
	p := newPlane(t)
	p.activate(t.Context(), t, grownCompanyDoc)
	// The pointer has moved and this node has not ticked.
	if got := p.recon.Posture(t.Context()); got != configplane.PostureWait {
		t.Errorf("posture = %q, want wait", got)
	}
}

func TestAConfirmedLaggardShedsOnlyWhenAPeerHasTheEpoch(t *testing.T) {
	t.Parallel()
	// Shedding moves work to a healthy peer; with none it would be stopping,
	// so the same lag reads isolated and the node keeps serving what it has.
	p := newPlane(t)
	// The node tries and fails: that makes its lag CONFIRMED rather than
	// ordinary propagation.
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
	// A node scaled in, redeployed or crashed leaves its `ok` behind;
	// counting it would have a diverged survivor shed its seats to a node
	// that no longer exists, taking the company dark instead of degraded.
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
	// Fail open: answering "behind" would take every node out of rotation
	// on a database blip.
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
	// The HOST's seat set must follow the epoch, not the boot company: a
	// method value captured at construction keeps a stale set, which looks
	// exactly like a node losing every race.
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

// The post-mortem trail. The coordination record is one key per node in a
// bucket aged at sixty seconds, so a node that crashed or was scaled in during
// a bad rollout, the one an incident review looks for, is gone a minute
// later; this event is what outlives it.
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
	// The subsystems this apply got through, in order: on a failure, what
	// was already mutated, which makes a degraded apply diagnosable.
	if len(got.AppliedSubsystems) == 0 {
		t.Fatal("applied_subsystems is empty on a successful apply")
	}
	// `published` is last: it tells every open socket the company changed,
	// so it follows everything a socket reads.
	if got.AppliedSubsystems[0] != "secrets" ||
		got.AppliedSubsystems[len(got.AppliedSubsystems)-1] != "published" {
		t.Errorf("subsystems = %v, want the apply's own order from secrets to published",
			got.AppliedSubsystems)
	}
	// Each is named once: a stage appearing twice reads as a retry.
	seen := map[string]bool{}
	for _, name := range got.AppliedSubsystems {
		if seen[name] {
			t.Errorf("subsystems = %v, want %q named once", got.AppliedSubsystems, name)
		}
		seen[name] = true
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

	// A payload that DECODES as a company (the stored-form reader is
	// lenient, so a rolling upgrade can boot on a newer peer's revision)
	// and is refused when BUILT.
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
	// Refused before Apply, so NOTHING was mutated: the node serves its
	// previous epoch correctly rather than degraded.
	if len(got.AppliedSubsystems) != 0 {
		t.Errorf("subsystems = %v, want none — the revision never reached Apply",
			got.AppliedSubsystems)
	}
}

// And the partial list, at its own seam: an apply failing PART WAY has
// mutated everything before the failure, and the list is the only record of
// how much (the coordination status carries just a status and error text).
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
	// The secret snapshot runs FIRST (it lets re-activating an unchanged
	// revision pick up a rotated credential), so it ran before the refusal.
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
// database of the node that served the write, and every other node first
// meets it when the pointer names it: with the body in that one file, a live
// config change would reach only the node it was posted to.
func TestAPeerConvergesOnARevisionItHasNeverSeen(t *testing.T) {
	t.Parallel()
	// A second node: its own database, sharing only the coordination store
	// and the keyring, as a fleet does.
	author, peer := fleetPeers(t)

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
	// And it KEPT the revision: company_config is where this node's history,
	// diffs and revert targets are read from.
	adopted, held, err := peer.store.Configs().Get(t.Context(), target.RevisionID)
	if err != nil || !held {
		t.Fatalf("the peer did not keep its own copy (held=%v err=%v)", held, err)
	}
	if !adopted.Boots || !adopted.Fleets {
		t.Errorf("the applied revision boots=%v fleets=%v on the peer, want both",
			adopted.Boots, adopted.Fleets)
	}
	// AND IT NAMES WHO WROTE IT, AND HOW, from the pointer's complete origin
	// ([coord.RevisionOrigin.Check]): the history an operator reads must not
	// depend on which node answered. Mutation: adopt as `peer` from `fleet`
	// and this fails.
	if adopted.CreatedBy != revisionAuthor || adopted.CreatedByKind != iam.ActorHuman ||
		adopted.OperatorID != revisionCredential || adopted.Source != "test" {
		t.Errorf("the peer's copy records %q/%q/%q from %q, want the writer's "+
			"%s/human/%s from %q", adopted.CreatedBy, adopted.CreatedByKind,
			adopted.OperatorID, adopted.Source, revisionAuthor, revisionCredential, "test")
	}
	// The settings and the seats of the revision it converged on, not the
	// one it booted with.
	if got := dayCeiling(peer.engine.Company()); got != grownDayCeiling {
		t.Errorf("the peer's token budget is %d, want the revision it "+
			"converged on", got)
	}
	if seats := peer.seats(t); !slices.Contains(seats, "designer") {
		t.Errorf("the peer's seats are %v, want the revision it converged on", seats)
	}
}

// The nudge, end to end. The reconcile interval is fifteen seconds and a
// config change must land in milliseconds: only a node hearing the activation
// and ticking early makes that true.
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

	// Activated AFTER the loop runs, so only the nudge can make it converge
	// in the window below; a poll is a full interval away.
	epoch := p.activate(t.Context(), t, grownCompanyDoc)
	ev := events.New(types.ConfigRevisionActivated{RevisionID: "any", RevisionSummary: "x"},
		events.NewTrace())
	// Half the reconcile interval: no poll lands inside it.
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

// A NODE THAT HAS DECIDED NOTHING ADMITS WORK. The gate is consulted from a
// node's first delivery, possibly before its first reconcile pass; refusing
// then would leave every cold start deaf for an interval, which is worse than
// answering against the config it booted with.
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

// LAG ALONE DOES NOT CLOSE THE GATE: refusing on `wait` would make the first
// node to apply the cause of a fleet-wide outage, the reason wait stays ready
// at /ready.
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

// A SHEDDING NODE ACTUALLY REFUSES (configplane.Posture.ServesTraffic): a
// posture computed, heartbeated and reported at /ready but acted on by nothing
// leaves a node taking deliveries, schedules and turns against a company it
// has decided it does not have.
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

// AN UNREADABLE CONTROL PLANE REOPENS THE GATE. Posture fails OPEN (an
// unreadable plane reports serve) and admission follows: recording only
// successful reads would leave a node that shed once refusing work for the
// whole outage, on evidence it can no longer check.
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

// THE HEARTBEAT'S READ NAMES THE PLANE IT COULD NOT READ instead of failing
// open, and shuts nothing the probe's read would not: it reopens the gate
// exactly as [engine.Reconciler.Posture] does, so the admission a node
// applies cannot depend on which of the two read last.
func TestReadPostureReportsAnUnreadablePlaneAndStillReopensTheGate(t *testing.T) {
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
	if got, err := p.recon.ReadPosture(t.Context()); err != nil || got != configplane.PostureShed {
		t.Fatalf("posture = %q, %v; want shed — the premise", got, err)
	}
	if p.recon.Admits() {
		t.Fatal("a shedding node still admitted work — the premise")
	}

	blind.Store(true)
	got, err := p.recon.ReadPosture(t.Context())
	if err == nil {
		t.Fatalf("an unreadable control plane answered %q, want an error", got)
	}
	if got != "" {
		t.Errorf("posture = %q beside the error, want none", got)
	}
	if !p.recon.Admits() {
		t.Error("the gate stayed shut on a plane the heartbeat could not read")
	}
}

// blindPlane is a control plane that stops answering on command, as a broker
// that became unreachable: the state the fail-open rule exists for. Embedded
// through an alias, because coord.Plane declares a Fleet() method of its own
// and embedding the interface directly names the field "Fleet".
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

// THE PROGRESS TRIPLE IS READ FROM OTHER GOROUTINES THAN THE ONE THAT WRITES
// IT: the tick writes applied, target and attempts, and Posture reads all
// three from the seat heartbeat (SetPosture), the readiness probe and every
// runtime-state HTTP handler. Only a test driving both paths at once lets
// -race see a field left unsynchronised.
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
	// counter and the target epoch both change under the readers. TEN
	// CYCLES: the detector reports the first unordered pair, and four
	// spinning readers hit every field thousands of times between two
	// ticks, so each cycle is a full chance; more only hold four cores of a
	// shared race job.
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

// dayCeiling is a company's daily token ceiling, 0 where it declares none — the
// one window [grownCompanyDoc] sets, so a revision's settings can be told from
// the one before it.
func dayCeiling(c *engine.Company) int {
	if c == nil || c.Config == nil || c.Config.TokenBudget.Day == nil {
		return 0
	}
	return *c.Config.TokenBudget.Day
}

// --- posture during an apply ------------------------------------------------

// midApplyPlane is the control plane with a window INTO an attempt: the
// reconciler reads a payload from it only while an attempt fetches a revision
// this node does not hold, so [midApplyPlane.during] runs on the tick's own
// goroutine after the attempt starts and before the epoch is swapped, where
// the probes and the heartbeat, on their own goroutines, must not read the
// attempt in flight as failed. A WITHHELD payload it answers as absent, so
// every attempt at that revision fails and none keeps a local copy that would
// skip the window.
type midApplyPlane struct {
	planeBackend
	during   atomic.Pointer[func()]
	withheld atomic.Pointer[string]
}

func (m *midApplyPlane) Body(ctx context.Context, a coord.Activation) (json.RawMessage, bool, error) {
	if fn := m.during.Load(); fn != nil {
		(*fn)()
	}
	if id := m.withheld.Load(); id != nil && *id == a.RevisionID {
		return nil, false, nil
	}
	return m.planeBackend.Body(ctx, a)
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

// activateRemotely points the fleet at a revision this node's store does not
// hold, as every peer but the writer meets one, so the attempt fetches it
// through the window above. SEALED, as every writer seals: a peer refuses a
// body that arrives through the coordination store in the clear.
func activateRemotely(t *testing.T, p *plane, doc string) int64 {
	t.Helper()
	document := p.seal(t, yamlToJSON(t, doc))
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

// A SUCCESSFUL APPLY IS NEVER REPORTED AS A DIVERGENCE, the first one on a
// node with no company and a later one alike: an attempt in flight is not a
// failed one, or a lone node's /health would say `isolated` (or
// `unconfigured`) through every apply, like a revision that does not apply.
//
// Observed three ways: inside the attempt before the swap (the payload fetch),
// inside it after (the engine's applied hook, before the outcome is recorded),
// and by a reader polling from another goroutine as the probes and heartbeat
// do. The two windows let the case fail: the poller alone could miss a
// hundred-millisecond apply.
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
	e.SetOnCompanyPublished(func(context.Context) { look("after the swap") })

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
		// /health derives its status from this beside the posture: the
		// first apply reads `unconfigured` before the swap and `ok` after,
		// never a divergence.
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

// AND A FAILED APPLY STILL SAYS SO, from the moment its attempt concludes and
// never before: an attempt at a revision this node cannot reach reads wait
// while it runs, then isolated alone, shed once a peer has the epoch, and
// stuck only once the LAST attempt has failed.
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
		RevisionID: ghost, Summary: "ghost", Payload: []byte("{}"), At: pinnedNow,
		Origin: anOrigin,
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

// AN APPLY THAT OUTLASTS PROPAGATION IS CONFIRMED LAG, while still running.
// Attempts count only when they conclude, and a tick hung in one never does,
// so the lag is measured in time, from when this node first saw the epoch.
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
	// The finished apply returns the node to serving and stops the lag
	// clock: nothing it waited out counts against the next epoch.
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
		RevisionID: ghost, Summary: "ghost", Payload: []byte("{}"), At: pinnedNow,
		Origin: anOrigin,
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

// THE RECOVERY GESTURE IS AN ORDINARY APPLY. After a revision failed
// everywhere and the node sat isolated on it for ten minutes, the operator
// activates the corrected one and a faster peer records it first: this node,
// mid-apply of the fix, is propagating, not late. The lag clock restarts with
// the target, or every node but the fastest sheds through a successful apply.
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

// AN EPOCH THE TICK HAS NOT MET YET HAS NO ATTEMPTS AND NO LAG. Until the tick
// first sees an activation (a nudge's latency, or a poll interval when the
// nudge is lost), the posture reads the fresh pointer beside progress about
// the previous target; paired that way, a node long out of retries on a bad
// revision would read STUCK on the corrected one as soon as a peer applied it.
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

// AND A HUNG APPLY STAYS LATE ACROSS A NEW ACTIVATION, rather than getting a
// fresh configplane.LagGrace with each: its tick is held in the attempt at the
// older epoch and cannot begin on the new one, so it is late on the new epoch
// by as long as that attempt has held it (wait while short, shed once it
// outlasts propagation beside a peer that has the new epoch).
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
	e.SetOnCompanyPublished(func(context.Context) { once.Do(moveOn) })
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
