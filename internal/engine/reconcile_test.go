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

// pinnedNow is the clock the reconcile tests run on. Pinned because the
// freshness window that decides whether a peer's status is evidence is
// measured against it, and a suite on the real clock would assert about that
// window by sleeping through it.
var pinnedNow = time.Date(2026, 8, 23, 14, 0, 0, 0, time.UTC)

// anOrigin is the record every activation a case publishes carries, written
// an hour before pinnedNow: the backends refuse an activation without one, and
// a case about something else has no author to say.
var anOrigin = coord.RevisionOrigin{
	Author: "maya", AuthorKind: iam.ActorHuman, Source: "api",
	CreatedAt: pinnedNow.Add(-time.Hour),
}

// brokenRevision is well-formed JSON that cannot be built: a delegate template
// naming a provider the document does not configure. The provider block is
// non-empty deliberately: a company with no models at all is a supported
// authoring state, and an empty one would not be refused at all (see
// nomodels_test.go).
var brokenRevision = json.RawMessage(`{"name":"Acme",
  "providers":{"sandbox":{"fake":true},"llm":{"zulu":{"model":"m"}}},
  "workers":{"researcher":{"description":"reads sources and reports findings",
    "system_prompt":"You research.","model":"nonexistent"}}}`)

// unbuildableCompanyDoc meets every runnable rule and is refused by the
// engine's own build: its one seat's executor runs in an E2B box, whose key is
// a ${VAR} nothing on the node answers, and the E2B backend refuses an empty
// key.
const unbuildableCompanyDoc = `
name: Acme
providers:
  sandbox:
    e2b:
      api_key: "${CREWLET_TEST_E2B_KEY_NEVER_SET}"
  llm:
    coder: {agent: claude-code, model: sonnet, run_in: e2b}
roles:
  - name: SWE
    handle: swe
    llm: coder
`

// A second company, differing from companyDoc in the ways a reconcile has to
// be visible through: one more seat, and a token budget — a day ceiling above
// the executor's launch floor, which a company with seats that run an
// executor may not go below.
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

// fleetPeers is two nodes of ONE fleet: each its own engine and database, the
// author's coordination store for both, and ONE keyring between them — the
// shape a deployment has, where every node's Tier A names the same keys. Two
// engines built apart hold two random keyrings, and a peer could open nothing
// its author sealed.
func fleetPeers(t *testing.T, peerOpts ...func(*engine.ReconcilerOptions)) (author, peer *plane) {
	t.Helper()
	material := testKeyMaterial(t)
	keyed := func() *engine.Engine {
		return newEngine(t, engine.Options{Bootstrap: bootstrap(t, func(b *config.Bootstrap) {
			brokertest.StoreIn(&b.Stream, filepath.Join(t.TempDir(), "stream"))
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
//
// TWO STORES, which is the shape the engine itself now has: the payload goes
// in the node's database as history, the pointer in the coordination store —
// SEALED in both, as every writer stores it. Nothing here makes it what the
// node boots on: only the reconciler's apply does.
//
// TAKES THE CALLER'S CONTEXT rather than reaching for t.Context(). Most cases
// pass t.Context() and nothing changes, but the concurrency case below drives
// this from a goroutine under a cancellable context it also owns — and a
// writer that kept activating revisions after its own cancel would be writing
// past the window the readers are watching.
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
	// THE AUTHOR RIDES THE POINTER, as the config surface publishes it —
	// the whole origin, which the backends refuse an activation without.
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

// activatePayload is activate for a document that is not a company — a broken
// one, sealed like any other. Same two writes, because a payload the fleet
// does not point at is a payload no reconciler will ever read.
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

	target, found, err := p.fleet.Target(t.Context())
	if err != nil || !found {
		t.Fatalf("Target: found=%v err=%v", found, err)
	}
	payload, found, err := p.fleet.Payload(t.Context(), target.RevisionID)
	if err != nil || !found {
		t.Fatalf("Payload: found=%v err=%v", found, err)
	}
	// RE-ACTIVATING the SAME revision, which is the case that matters: the
	// pointer is append-only, so it mints a new epoch every node is
	// watching — that is how a rotated secret reaches a running fleet.
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

// RE-ACTIVATING THE REVISION THIS NODE BOOTS ON MOVES BOTH ITS INSTANTS.
//
// Re-activation is the credential-rotation gesture. The boot row's served
// instant is the one this node boots its chart with, and its activation
// instant the one its config history shows and the sweep dates it by. The
// early return for "already the fleet's revision" used to leave them at the
// first activation's.
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

// rotatedKeyDoc is a company whose one seat's executor runs in an E2B box keyed
// by a ${VAR}: it builds while the node's environment answers it, and the E2B
// backend refuses it once it does not — the shape of a rotated credential one
// node never received.
const rotatedKeyDoc = `
name: Acme
providers:
  sandbox:
    e2b:
      api_key: "${CREWLET_TEST_E2B_KEY_ROTATED}"
  llm:
    coder: {agent: claude-code, model: sonnet, run_in: e2b}
roles:
  - name: SWE
    handle: swe
    llm: coder
`

// A RE-ACTIVATION OF THE REVISION A NODE RUNS, WHICH THE NODE THEN REFUSES, IS
// STILL THE FLEET'S.
//
// The rotation gesture re-activates the revision every node runs, and a node
// whose environment lost the credential refuses it — so it keeps running that
// revision, applied at the earlier instant, and its boot row and its fleet's row
// are one row. With one instant per row, the boot row kept the one it applied
// and the fleet's newer one was lost: a lost pointer was restored at the old
// instant, and every tick out of retries found the fleet's row behind the
// pointer and wrote it again, decrypting the revision each time.
//
// The re-activation is recorded by the apply, and where that write failed, by
// the tick out of retries: the row names the pointer's revision either way, so
// a repair that compared the revision alone would leave its instant behind.
//
// Not parallel: the credential is the process environment.
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
			t.Setenv("CREWLET_TEST_E2B_KEY_ROTATED", "e2b_test_key")
			p := newPlane(t)
			p.activate(t.Context(), t, rotatedKeyDoc)
			if err := p.recon.Tick(t.Context()); err != nil {
				t.Fatalf("apply with the credential: %v", err)
			}
			target, _, err := p.fleet.Target(t.Context())
			if err != nil {
				t.Fatalf("Target: %v", err)
			}
			payload, _, err := p.fleet.Payload(t.Context(), target.RevisionID)
			if err != nil {
				t.Fatalf("Payload: %v", err)
			}
			if err = os.Unsetenv("CREWLET_TEST_E2B_KEY_ROTATED"); err != nil {
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
// revision's activation instant, which is the store failing a write it was asked
// for and nothing else; allowInstants takes the refusal away.
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

// refuseBootRow makes this node's store refuse every write that moves what the
// node boots on — the store failing a local write it was asked for, a full
// disk or a file somebody took away — and allowBootRow takes the refusal away.
//
// ON THE SERVED INSTANT, which [store.Configs.Serve] moves on both rows it
// touches: it clears the previous boot row's and stamps the new one's, so
// either half of the transaction trips this and the whole of it is rolled back,
// leaving the node booting on what it booted on before.
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

// THE FLEET'S ROW IS REPAIRED BY THE TICKS THAT FOLLOW AN APPLY.
//
// The row is recorded during the apply, best effort, and it is what the node
// restores when the pointer is gone; a write the apply could not make is left
// for the next tick to repair — the converged one, or one out of retries on a
// revision this node refuses, which is still the fleet's. A row left behind
// restores a company the fleet has moved past.
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

// WHAT A NODE BOOTS ON IS WHAT ITS ENGINE APPLIED.
//
// The boot row is the revision this node starts on, and the reconciler is its
// only online writer: once the engine has applied the fleet's revision, the
// boot row names it, and a converged tick puts it back there if anything moved
// it — a write the apply could not make, or an offline choice the fleet never
// took. A boot row left elsewhere is a node that starts, one restart later, on
// a company the fleet is not running.
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

	// An offline choice made the boot row under the running node, which
	// the boot publish withheld because the fleet had moved on: the
	// engine runs the fleet's revision, so that is what it boots on — and
	// it says the offline change was not published, which the operator who
	// made it would otherwise find out by accident.
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
	// the fleet took it: the boot row is still an offer, of the very revision
	// the engine runs. Serving it clears the offer, and nothing was withheld,
	// so nothing is said.
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

	// A revision the fleet took that this node holds only as history: an
	// API write, which stores its revision and moves the pointer and
	// nothing else.
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
// is running.
//
// The boot row moves only once an apply has succeeded, and that write is local
// and best effort: a store that refuses it leaves the node running a revision
// it would not start on, for as long as the repair takes. With only the revision
// it serves on the row, a reader that keeps what a node needs in order to start
// — the Kubernetes operator keeps a revision's sealed credentials while a node
// still names it — dropped exactly what the node would have booted on, and the
// node's next restart crash-looped on a revision it could no longer build.
//
// And the repair cannot wait for convergence: this node refuses the fleet's
// next revision, so it never takes a converged tick again, and the tick out of
// retries is the one that has to move the row.
//
// Mutations: publish the served revision as what the node boots on (take
// Reconciler.Boots off the boot row and read it off served) and the first
// assertion fails; drop Reconciler.serve from the out-of-retries branch of Tick
// and the repair never lands.
func TestTheFleetRowSaysWhatThisNodeBootsOn(t *testing.T) {
	t.Parallel()
	p := newPlane(t)
	p.activate(t.Context(), t, companyDoc)
	if err := p.recon.Tick(t.Context()); err != nil {
		t.Fatalf("apply the revision this node boots on: %v", err)
	}
	boots := p.recon.Serving()

	// From here the store takes no write that moves the boot row, so the
	// apply below leaves this node running one revision and booting on
	// another.
	p.refuseBootRow(t)
	p.activate(t.Context(), t, grownCompanyDoc)
	if err := p.recon.Tick(t.Context()); err != nil {
		t.Fatalf("apply the revision this node runs: %v", err)
	}
	runs := p.recon.Serving()
	if runs == boots || runs == "" {
		t.Fatalf("this node runs and boots on %q; the case needs two revisions", runs)
	}

	// And then the pointer moves to a revision this node cannot build: it
	// never converges again, and every tick from here is a retry or a tick
	// out of retries.
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

	// The store takes the write again. The next tick is out of retries on a
	// revision this node still refuses, and it is the one that repairs the
	// boot row and says so.
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
// included.
//
// The row is only ever moved to a revision this engine APPLIED, and until the
// first apply succeeds what the node serves is what it BOOTED on — which the
// row already names, at the instant this node applied it. The two instants on
// that row differ exactly on the node this matters for: one that refused a
// re-activation of the revision it runs carries the fleet's later activation
// beside its own earlier one, and a tick that wrote the row would stamp the
// later instant as the one this node applied, which is what the next boot
// stamps the chart with. (The same guard is what leaves an offline change in
// the row offered until an apply replaces it.)
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
	// THE RE-ACTIVATION THIS NODE REFUSED before it was restarted: the row
	// carries the fleet's later activation of the same revision, and the
	// instant this node applied it stays where it was.
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
// many revisions it applies.
//
// Its boot row is written while it runs — its own readers use it, a config
// write built on what this node knows and an offer's base among them — and
// read by nobody afterwards: the engine deletes the file as the next start
// opens it. Naming that revision in the fleet view would have a reader keep
// what this node has no use for, and say nothing about what it will run.
//
// ON A STORE OPENED SCRATCH, which is how the reconciler learns it: from the
// handle the node runs on, the one reading that cannot disagree with what the
// open did. An option saying it again was left out by every harness that
// built a reconciler on a scratch node, which then published a boot revision.
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
	// The ENGINE's store holds the history this case writes; the scratch
	// one is the node's own, as on a node without `data`, and meets every
	// revision through the pointer, as such a node does.
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

// THE FLEET ROW NAMES THE BOOT REVISION ON THE TICK THAT MOVES IT.
//
// What the row says this node boots on can never be AHEAD of the store — it
// moves only on a read of the boot row or a write to it that succeeded — so
// the order of the boot-row write and the fleet's record decides only how
// soon the row says it. Both orders are the right one here: a clean apply's
// record names the new boot revision beside the one it runs, rather than the
// previous one until the next tick; and the converged tick that repairs a
// write that failed says so on that tick, rather than one interval later. A
// reader watching the row through a rollout reads a node whose two names
// differ as one whose boot row failed to move, which these two are not.
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

// A BOOT ROW THIS NODE COULD NOT READ IS NEVER PUBLISHED AS NONE.
//
// What a node boots on reaches the fleet view as a name, and an empty one says
// it boots on nothing. That is a claim a reader acts on — the Kubernetes
// operator keeps a revision's sealed credentials while a node still names it —
// so it is made only from a read that found the row empty, never from one that
// failed. A reconciler that learned the row on its first tick published, for a
// node whose reads failed on that tick while its apply succeeded, the revision
// it runs and an empty boot revision: the operator kept the one, deleted what
// only the revision in the row needed, and the node's next start could not
// build what it booted on. The row is read as the reconciler is built, and a
// read that fails later leaves the last answer standing.
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
// attempted.
//
// A refusal says the node did not reach the epoch it attempted, never which one
// it did reach — so a writer that saved epoch 6 could not tell from the row
// whether a node that refused it runs 5, or nothing at all. That is the
// comparison a node's own health push makes with its live epoch, and the fleet
// view makes it per node from this. 0 before the first apply succeeds, which
// the view leaves out rather than offer as an epoch.
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

	// Epochs up to 5 move the pointer with no tick between them, so the one
	// apply below is of epoch 5.
	var applied int64
	for range 4 {
		applied = p.activate(t.Context(), t, companyDoc)
	}
	if applied != 5 {
		t.Fatalf("the pointer is at epoch %d; the case needs 5", applied)
	}
	if err := p.recon.Tick(t.Context()); err != nil {
		t.Fatalf("apply epoch 5: %v", err)
	}
	refused := p.activatePayload(t, "broken", brokenRevision)
	if err := p.recon.Tick(t.Context()); err == nil {
		t.Fatal("a revision that cannot be built applied cleanly")
	}
	row := p.fleetRow(t)
	if row.Epoch != 6 || refused != 6 || row.Applied != 5 ||
		configplane.ApplyStatus(row.Status) != configplane.StatusError {
		t.Errorf("the fleet says this node attempted epoch %d (%s) and applied %d, want "+
			"the refusal of 6 beside the 5 it runs", row.Epoch, row.Status, row.Applied)
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

// unreadableBootRow makes every read of db's boot row fail while the row stays
// where it is, and returns what puts it back. The other rows read as they did,
// so an apply that reads only those succeeds.
//
// A VALUE THE COLUMN CANNOT HOLD, the row's served instant written as text: the
// read fails as it scans the row, which is a read of THIS row failing — a
// corrupt page, a column a newer build wrote differently — rather than the
// whole file going away, which the apply would trip over too.
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

// A REVISION THIS NODE REFUSES NEVER BECOMES WHAT IT BOOTS ON.
//
// The fleet's revision used to be marked as this node's own the moment the
// tick fetched it, before it was validated: a node that refused it kept
// running the previous company, and booted on the refused one at its next
// start — where the boot refused it again, before the engine ran. So a
// revision one node's build could not run took that node down on its next
// restart, however healthy it had been. What the node RECORDS of the refused
// revision is that the fleet is on it: that is the company it restores if the
// pointer is lost, and what its row in the fleet view names beside the one it
// serves.
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
//
// A refusal leaves the applied epoch where it was, so read alone it is a node
// still applying. The health push carries [engine.Reconciler.Attempt] beside
// it, so a reader who may not read the fleet — a unit lead following their
// save — is told this node refused the revision rather than that it is
// applying it for good. Nothing before the first concluded attempt, which a
// zero epoch would misstate.
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

// A NODE SAYS WHAT IT BOOTED ON UNTIL IT APPLIES SOMETHING ELSE.
//
// A node that starts on its boot row and then refuses the fleet's revision is
// still running the boot row's company, and its row in the fleet view is where
// an operator sees that. Before its first successful apply, the reconciler
// knows that only because it was told what the engine started on.
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
// name another revision at the epoch this node already applied.
//
// The node does not apply it — convergence is by epoch, and that is the
// pointer's own contract — but nothing may then treat it as what the node
// runs. A boot row moved to "the pointer's revision at the applied epoch" would
// name a revision the engine never applied, and the next boot would start on
// it unvalidated.
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

	// A FRESH COORDINATION STORE whose first activation — the same epoch —
	// names another revision this node holds.
	recreated := memory.NewFleet()
	document := p.seal(t, yamlToJSON(t, companyDoc))
	other, err := p.store.Configs().Insert(t.Context(), store.Revision{
		CreatedByKind: iam.ActorOperator, Source: "test", CreatedBy: "operator",
		Summary: "elsewhere", Payload: document, CreatedAt: pinnedNow,
	})
	if err != nil {
		t.Fatalf("store the other revision: %v", err)
	}
	published, err := recreated.Activate(t.Context(), coord.ActivationRequest{
		RevisionID: other, Summary: "elsewhere", Payload: document, At: pinnedNow.Add(time.Hour),
		ExpectAbsent: true, Origin: anOrigin,
	})
	if err != nil {
		t.Fatalf("activate on the recreated store: %v", err)
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

func (s *swappablePlane) Payload(ctx context.Context, revisionID string) (json.RawMessage, bool, error) {
	return s.plane().Payload(ctx, revisionID)
}

func (s *swappablePlane) RecordApply(ctx context.Context, status coord.NodeApply) error {
	return s.plane().RecordApply(ctx, status)
}

func (s *swappablePlane) Fleet(ctx context.Context) ([]coord.NodeApply, error) {
	return s.plane().Fleet(ctx)
}

// TWO TICKS ON ONE ENGINE FOLLOW THE POINTER IN ITS ORDER.
//
// The boot row moves only to what the engine applied, so an engine that let
// two ticks interleave — each reading the pointer, then applying — could let
// the one that read the OLDER epoch apply last, and walk the boot row back to
// it. The first tick here is held inside its read of the older revision's
// body until the pointer has moved on and, unless something stops it, a
// second tick has read the newer one and applied it.
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

func (h *heldPayload) Payload(ctx context.Context, revisionID string) (json.RawMessage, bool, error) {
	payload, found, err := h.Plane.Payload(ctx, revisionID)
	if revisionID == h.revision {
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

// bodiless is a plane whose pointer names a revision it holds no body for.
//
// The store cannot be made to produce one — the body is written inside the
// pointer's own record — so the case needs a plane that says so: it is what a
// node sees when the pointer moved between its read of the target and its read
// of the body, and what a pointer an older build wrote beside a body a racing
// writer had replaced looks like.
type bodiless struct{ coord.Plane }

func (bodiless) Payload(context.Context, string) (json.RawMessage, bool, error) {
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
	// The fixed revision's settings AND its seats: a revision carries both.
	if got := dayCeiling(p.engine.Company()); got != grownDayCeiling {
		t.Errorf("token budget = %d, want the fixed revision's", got)
	}
	if got := p.seats(t); len(got) != 3 {
		t.Errorf("seats = %v, want the fixed revision's three", got)
	}
}

// A REVISION SEALED UNDER A KEY THIS NODE DOES NOT HOLD IS AN ERROR, NEVER AN
// EMPTY COMPANY.
//
// Booting onto nothing would read on every surface as an operator who
// configured nothing, where the actual fault is a keyring that does not name
// the key the revision was sealed under — a node deployed with another fleet's
// Tier A, or a key dropped before every revision was re-sealed off it.
//
// The control is the same document sealed under this node's own keyring.
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

// A PEER'S REVISION THAT IS NOT SEALED IS NEITHER APPLIED NOR KEPT.
//
// The payload a peer converges on is read off the coordination store, and
// anything that can reach the broker can write that bucket — so the seal under
// the fleet's keyring is what says a node of this fleet wrote it. A plaintext
// body used to be applied by a node holding a keyring exactly as its own
// would have been: a forged company document, running on every node, with
// nothing in the log saying where it came from. Now the apply is recorded as
// an error, the epoch is not reached, and the body never lands in this node's
// own config history — where it would have been shown, diffed and offered as a
// revert target.
//
// THE FORGERY IS PUBLISHED STRAIGHT ONTO THE PLANE, as anything reaching the
// broker could, rather than through a writer: every writer this build has
// seals under the node's keyring, and none can be built without it.
//
// The control is the same document sealed by a node of the fleet: applied and
// kept.
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
	// AND IT SAYS WHERE THE BODY CAME FROM: the coordination store, where
	// anything that reaches the broker could have written it; and what
	// replaces it, an admin's whole document.
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
	// THE FLEET VIEW the node reports into is the one it reads, the
	// author's: the author itself never ticked, so the one row is the
	// node's.
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

// THIS NODE'S OWN UNSEALED COPY IS NAMED AS ITS OWN.
//
// The fleet can point at a revision this node already holds in the clear. That
// is not a body somebody else published: the remedy is an import HERE, which
// stores the document sealed, so the refusal says it is this node's own copy.
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
//
// A newer build's seal (here a successor's `enc:v2:`) is wrapped exactly as
// this build's is, and the reader took every seal it could not parse for no
// seal at all: the recorded error told the operator the body came from
// outside the fleet and that a whole document replaces it, which would
// discard what the newer peer wrote. It is refused, neither applied nor kept,
// as every body that does not open is, and the error names the remedy, the
// build that sealed it.
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
	// The apply-status table upserts on node id, so an empty one makes
	// every node that forgot it share a row — and the fleet view shows one
	// anonymous entry where a dozen nodes should be.
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
	// `published` is last because it is what tells every open socket the
	// company changed, and it must not say so until everything a socket
	// reads has been rebuilt.
	if got.AppliedSubsystems[0] != "secrets" ||
		got.AppliedSubsystems[len(got.AppliedSubsystems)-1] != "published" {
		t.Errorf("subsystems = %v, want the apply's own order from secrets to published",
			got.AppliedSubsystems)
	}
	// AND EACH IS NAMED ONCE. The trail is what a degraded apply is read
	// back from, and a stage appearing twice reads as a retry that
	// happened rather than as one list stated in two places.
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
	// A second node: its own database, the same coordination store and
	// the same keyring. That is the shape of a fleet, and the only things
	// the two share.
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
	// And it KEPT the revision. company_config is where this node's own
	// history, diffs and revert targets are read from, so a node that
	// applied without adopting would serve an epoch its operator surface
	// cannot show.
	adopted, held, err := peer.store.Configs().Get(t.Context(), target.RevisionID)
	if err != nil || !held {
		t.Fatalf("the peer did not keep its own copy (held=%v err=%v)", held, err)
	}
	if !adopted.Boots || !adopted.Fleets {
		t.Errorf("the applied revision boots=%v fleets=%v on the peer, want both",
			adopted.Boots, adopted.Fleets)
	}
	// AND IT NAMES WHO WROTE IT, AND HOW. The copy said `peer` from `fleet`,
	// so the author was on the one node the write reached and nobody on every
	// other — the history an operator reads depended on which node answered.
	// The source is the writer's too: every pointer carries a complete origin
	// ([coord.RevisionOrigin.Check]), so there is no `fleet` stand-in left to
	// fall back to. Mutation: adopt as `peer` from `fleet` and this fails.
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

func (m *midApplyPlane) Payload(ctx context.Context, revisionID string) (json.RawMessage, bool, error) {
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
// what makes the attempt fetch it through the window above. It is SEALED, as
// every writer of the fleet seals what it publishes: a peer refuses a body
// that arrives through the coordination store in the clear.
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
