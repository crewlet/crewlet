package storetest

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/store"
)

// The company_config REVISION table's own behaviour: the history, and this
// node's three facts about it — what it boots on, the fleet's revision as it
// last saw it, and an offline choice not yet published.
//
// What is NOT here: the activation pointer, the apply status and the
// peer-health read. Those are fleet-shared state in the coordination store —
// see internal/coord/fleet.go — where internal/coord/coordtest certifies them
// against every backend rather than against this one database.

// fleetRevision is a copy of a fleet revision as the reconciler holds one: the
// pointer's origin, its sealed body and the instant it was activated at.
func fleetRevision(id string, activated time.Time) store.Revision {
	return store.Revision{
		ID: id, CreatedBy: "maya", CreatedByKind: iam.ActorOperator, OperatorID: "pat:42",
		Source: "api", Summary: id, Payload: json.RawMessage(`{"name":"` + id + `"}`),
		CreatedAt: base, ActivatedAt: activated,
	}
}

// offline is a revision an offline command writes.
func offline(summary string) store.Revision {
	return store.Revision{
		CreatedBy: "ops", CreatedByKind: iam.ActorOperator, Source: "import",
		Summary: summary, Payload: json.RawMessage(`{"name":"` + summary + `"}`),
		CreatedAt: base,
	}
}

func mustBoot(t *testing.T, configs *store.Configs) store.Revision {
	t.Helper()
	boot, found, err := configs.Boot(t.Context())
	if err != nil || !found {
		t.Fatalf("boot row: found=%v err=%v", found, err)
	}
	return boot
}

func mustGet(t *testing.T, configs *store.Configs, id string) store.Revision {
	t.Helper()
	got, found, err := configs.Get(t.Context(), id)
	if err != nil || !found {
		t.Fatalf("get %s: found=%v err=%v", id, found, err)
	}
	return got
}

func testOneRowBootsAndOneIsTheFleets(t *testing.T, db *store.DB) {
	// Enforced by the database rather than by the application remembering
	// to. Two boot rows is a node whose start depends on which one a query
	// happened to return first, and two fleet rows a pointer restored from
	// whichever came back.
	configs := db.Configs()
	first, err := configs.Offer(t.Context(), store.Offer{}, offline("one"))
	if err != nil {
		t.Fatalf("first offer: %v", err)
	}
	second, err := configs.Offer(t.Context(), store.Offer{}, offline("two"))
	if err != nil {
		t.Fatalf("second offer: %v", err)
	}
	if boot := mustBoot(t, configs); boot.ID != second {
		t.Fatalf("boots on %s, want the newest offer %s", boot.ID, second)
	}
	if again := mustGet(t, configs, first); again.Boots || again.Offer != nil {
		t.Errorf("the replaced offer is still marked: boots=%v offer=%+v", again.Boots, again.Offer)
	}
	for _, id := range []string{"r-a", "r-b"} {
		if err = configs.SawFleet(t.Context(), fleetRevision(id, base)); err != nil {
			t.Fatalf("saw %s: %v", id, err)
		}
	}
	fleets, found, err := configs.Fleets(t.Context())
	if err != nil || !found || fleets.ID != "r-b" {
		t.Fatalf("fleet's revision = %s (found=%v err=%v), want r-b", fleets.ID, found, err)
	}
	if again := mustGet(t, configs, "r-a"); again.Fleets {
		t.Error("the fleet's previous revision is still marked as the fleet's")
	}
	all, err := configs.List(t.Context(), 0, 0)
	if err != nil || len(all) != 4 {
		t.Fatalf("%d revisions (err %v), want 4 — history is not overwritten", len(all), err)
	}
}

func testAnInsertedRevisionIsHistory(t *testing.T, db *store.DB) {
	// A write through a running node's API stores its revision before the
	// fleet has taken it, and even once the fleet has, this node has not run
	// it: what a node boots on is something its engine applied. So an
	// inserted revision marks nothing, with or without a boot row beside it.
	configs := db.Configs()
	lone, err := configs.Insert(t.Context(), offline("refused"))
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if boot, found, berr := configs.Boot(t.Context()); berr != nil || found {
		t.Fatalf("an inserted revision became the boot row: %+v found=%v err=%v", boot, found, berr)
	}
	if err = configs.Serve(t.Context(), fleetRevision("live", base)); err != nil {
		t.Fatalf("serve: %v", err)
	}
	pending := offline("pending")
	pending.ParentID = "live"
	id, err := configs.Insert(t.Context(), pending)
	if err != nil {
		t.Fatalf("insert the pending revision: %v", err)
	}
	if boot := mustBoot(t, configs); boot.ID != "live" {
		t.Fatalf("boots on %s, want the served revision kept", boot.ID)
	}
	stored := mustGet(t, configs, id)
	if stored.Boots || stored.Fleets || stored.Offer != nil || !stored.ActivatedAt.IsZero() ||
		!stored.ServedAt.IsZero() || stored.ParentID != "live" {
		t.Errorf("inserted = %+v, want unmarked, never activated, with its parent", stored)
	}
	if again := mustGet(t, configs, lone); again.Boots {
		t.Error("an unrelated inserted revision became the boot row")
	}
}

func testAWriteCannotArriveMarked(t *testing.T, db *store.DB) {
	// EACH MARK IS ITS VERB'S. A revision handed to a write with a mark set
	// is a caller that thinks the write sets it, and storing it unmarked
	// would let that caller believe the node now boots on a revision it
	// never applied.
	configs := db.Configs()
	for name, mutate := range map[string]func(*store.Revision){
		"boots":     func(r *store.Revision) { r.Boots = true },
		"fleets":    func(r *store.Revision) { r.Fleets = true },
		"offer":     func(r *store.Revision) { r.Offer = &store.Offer{} },
		"activated": func(r *store.Revision) { r.ActivatedAt = base },
		"served":    func(r *store.Revision) { r.ServedAt = base },
	} {
		r := offline(name)
		mutate(&r)
		if _, err := configs.Insert(t.Context(), r); err == nil {
			t.Errorf("Insert of a revision marked %s was stored", name)
		}
		if _, err := configs.Offer(t.Context(), store.Offer{}, r); err == nil {
			t.Errorf("Offer of a revision marked %s was stored", name)
		}
		if _, err := configs.InsertOn(t.Context(), store.Revision{}, r); err == nil {
			t.Errorf("InsertOn of a revision marked %s was stored", name)
		}
	}
	// AN OFFLINE OFFER IS NEVER PLANNED: only the boot publish plans one, and
	// a planned instant on a fresh offer reads as a publish that may have
	// landed. Nor is its base its own unconfirmed predecessor.
	//
	// A REAL REVISION TO REFUSE, so the refusal comes from the offer's shape
	// and not from a missing row: the [store.OfferStored] half aims at "p",
	// and with no such revision its own RowsAffected check would answer
	// [store.ErrNoRevision] whether or not the shape was ever looked at,
	// leaving the guard untested. So "p" is served first, and each refusal is
	// checked for an error that is NOT ErrNoRevision and for a boot row the
	// refusal left exactly as it was.
	if err := configs.Serve(t.Context(), fleetRevision("p", base)); err != nil {
		t.Fatalf("serve p: %v", err)
	}
	for name, over := range map[string]store.Offer{
		"planned":              {Base: "p", PlannedAt: base},
		"base and unconfirmed": {Base: "p", Unconfirmed: "p"},
	} {
		if _, err := configs.Offer(t.Context(), over, offline(name)); err == nil {
			t.Errorf("an offer %s was stored", name)
		}
		err := configs.OfferStored(t.Context(), "p", over)
		if err == nil {
			t.Errorf("a stored revision offered %s was accepted", name)
		} else if errors.Is(err, store.ErrNoRevision) {
			t.Errorf("a stored revision offered %s was refused as missing (%v), not "+
				"for its shape: the guard it stands for was never reached", name, err)
		}
		if boot := mustBoot(t, configs); boot.ID != "p" || boot.Offer != nil ||
			!boot.ServedAt.Equal(base) {
			t.Errorf("after a refused offer %s the node boots on %+v, want p served "+
				"at %s with no offer: the refusal wrote nothing", name, boot, base)
		}
	}
	if all, err := configs.List(t.Context(), 0, 0); err != nil || len(all) != 1 ||
		all[0].ID != "p" {
		t.Fatalf("a refused write left %d rows (err %v), want only the served p", len(all), err)
	}
}

func testARevisionSeenFromTheFleetMovesNoBootRow(t *testing.T, db *store.DB) {
	// THE FLEET'S REVISION IS NOT WHAT THIS NODE BOOTS ON until this node has
	// run it. The reconciler records the fleet's revision before it has
	// validated or built it, and a node that then refused it booted on it at
	// its next start — and was refused before the engine ran — because one
	// flag said both.
	configs := db.Configs()
	if err := configs.Serve(t.Context(), fleetRevision("p", base)); err != nil {
		t.Fatalf("serve p: %v", err)
	}
	if err := configs.SawFleet(t.Context(), fleetRevision("r", base.Add(time.Hour))); err != nil {
		t.Fatalf("saw r: %v", err)
	}
	if boot := mustBoot(t, configs); boot.ID != "p" || !boot.ServedAt.Equal(base) {
		t.Errorf("boots on %s served at %s, want p at %s", boot.ID, boot.ServedAt, base)
	}
	r := mustGet(t, configs, "r")
	if r.Boots || !r.Fleets || !r.ActivatedAt.Equal(base.Add(time.Hour)) || !r.ServedAt.IsZero() {
		t.Errorf("the fleet's revision reads boots=%v fleets=%v at %s, want only the "+
			"fleet's, at the fleet's instant", r.Boots, r.Fleets, r.ActivatedAt)
	}
}

func testTheFleetsInstantRisesAndTheServedOneStays(t *testing.T, db *store.DB) {
	// A ROW'S ACTIVATION INSTANT IS THE LATEST THIS NODE KNOWS OF, so the
	// retention sweep dates an old revision the fleet re-activated by the
	// re-activation; moved back by a late report, the row ages out under a
	// fleet running it. That holds on the BOOT row too: a re-activation of
	// the revision this node runs that it then refused is still the fleet's,
	// and with one column standing for both, the boot row kept the instant it
	// applied, the fleet's newer one was lost, and the reconciler wrote the
	// fleet's row again on every tick. What the boot row APPLIED is its own
	// instant, which a sighting must not move.
	configs := db.Configs()
	r, err := configs.Insert(t.Context(), offline("r"))
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	later, earlier := base.Add(2*time.Hour), base.Add(time.Hour)
	if err = configs.NoteActivation(t.Context(), r, later); err != nil {
		t.Fatalf("note: %v", err)
	}
	if err = configs.NoteActivation(t.Context(), r, earlier); err != nil {
		t.Fatalf("note an earlier activation: %v", err)
	}
	if got := mustGet(t, configs, r); !got.ActivatedAt.Equal(later) {
		t.Errorf("the instant is %s, want the later %s kept", got.ActivatedAt, later)
	}
	if err = configs.Serve(t.Context(), fleetRevision("p", base)); err != nil {
		t.Fatalf("serve p: %v", err)
	}
	if err = configs.SawFleet(t.Context(), fleetRevision("p", base.Add(9*time.Hour))); err != nil {
		t.Fatalf("saw p again: %v", err)
	}
	if err = configs.NoteActivation(t.Context(), "p", base.Add(9*time.Hour)); err != nil {
		t.Fatalf("note p: %v", err)
	}
	boot := mustBoot(t, configs)
	if !boot.ServedAt.Equal(base) {
		t.Errorf("the boot row's applied instant moved to %s, want %s", boot.ServedAt, base)
	}
	if fleets, found, ferr := configs.Fleets(t.Context()); ferr != nil || !found ||
		fleets.ID != "p" || !fleets.ActivatedAt.Equal(base.Add(9*time.Hour)) {
		t.Errorf("the fleet's row = %s at %s (found=%v err=%v), want p at the "+
			"re-activation's %s", fleets.ID, fleets.ActivatedAt, found, ferr, base.Add(9*time.Hour))
	}
	// AN APPLY RAISES IT TOO: the reconciler's record of the fleet's row is
	// best effort, and a node that served an activation it never recorded
	// seeing still knows the fleet activated it then.
	applied := base.Add(10 * time.Hour)
	if err = configs.Serve(t.Context(), fleetRevision("p", applied)); err != nil {
		t.Fatalf("serve p at a later activation: %v", err)
	}
	if boot = mustBoot(t, configs); !boot.ServedAt.Equal(applied) || !boot.ActivatedAt.Equal(applied) {
		t.Errorf("served p reads served %s activated %s, want both at %s",
			boot.ServedAt, boot.ActivatedAt, applied)
	}
	if err = configs.NoteActivation(t.Context(), "missing", base); !errors.Is(err, store.ErrNoRevision) {
		t.Errorf("noting a revision nobody holds = %v, want ErrNoRevision", err)
	}
}

func testAnOfferReplacesTheBootRowAndKeepsItsBase(t *testing.T, db *store.DB) {
	// AN OFFLINE CHANGE CARRIES WHAT IT WAS BUILT ON, because the next boot
	// publishes it only over that: a change made on a node behind its fleet,
	// published anyway, rolled the fleet back to the document underneath it.
	// It has no instant — nothing has activated it — and the row it replaces
	// keeps the one it was applied at.
	configs := db.Configs()
	if err := configs.Serve(t.Context(), fleetRevision("p", base)); err != nil {
		t.Fatalf("serve p: %v", err)
	}
	id, err := configs.Offer(t.Context(), store.Offer{Base: "p"}, offline("change"))
	if err != nil {
		t.Fatalf("offer: %v", err)
	}
	boot := mustBoot(t, configs)
	if boot.ID != id || boot.Offer == nil || boot.Offer.Base != "p" || !boot.ActivatedAt.IsZero() ||
		!boot.ServedAt.IsZero() || !boot.Offer.PlannedAt.IsZero() {
		t.Fatalf("boots on %+v, want the offer over p, unplanned and never activated", boot)
	}
	if p := mustGet(t, configs, "p"); p.Boots || p.Offer != nil || !p.ActivatedAt.Equal(base) ||
		!p.ServedAt.IsZero() {
		t.Errorf("the replaced row reads boots=%v offer=%+v at %s served %s, want history "+
			"at %s that no longer says it was applied", p.Boots, p.Offer, p.ActivatedAt, p.ServedAt, base)
	}
	none, err := configs.Offer(t.Context(), store.Offer{}, offline("first"))
	if err != nil {
		t.Fatalf("offer on nothing: %v", err)
	}
	if boot = mustBoot(t, configs); boot.ID != none || boot.Offer == nil || boot.Offer.Base != "" {
		t.Errorf("boots on %+v, want an offer built on no company", boot)
	}
}

func testServingClearsEveryOffer(t *testing.T, db *store.DB) {
	// AN OFFER THE FLEET NEVER TOOK STAYS HISTORY once the node runs the
	// fleet's revision: carried into history with its base, a later offline
	// activation of the same row would find a planned instant nobody planned.
	configs := db.Configs()
	id, err := configs.Offer(t.Context(), store.Offer{Base: "p", Unconfirmed: "q"}, offline("change"))
	if err != nil {
		t.Fatalf("offer: %v", err)
	}
	if err = configs.PlanOffer(t.Context(), id, base, "p"); err != nil {
		t.Fatalf("plan: %v", err)
	}
	if err = configs.Serve(t.Context(), fleetRevision("r", base.Add(time.Hour))); err != nil {
		t.Fatalf("serve r: %v", err)
	}
	all, err := configs.List(t.Context(), 0, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, r := range all {
		if r.Offer != nil {
			t.Errorf("%s still carries an offer %+v after the node served r", r.ID, r.Offer)
		}
	}
	if boot := mustBoot(t, configs); boot.ID != "r" || !boot.ServedAt.Equal(base.Add(time.Hour)) ||
		!boot.ActivatedAt.Equal(base.Add(time.Hour)) {
		t.Errorf("boots on %s served at %s (activated %s), want r at its applied instant",
			boot.ID, boot.ServedAt, boot.ActivatedAt)
	}
}

func testPlanAndConfirmNameTheBootOfferOrNothing(t *testing.T, db *store.DB) {
	// THE BOOT PUBLISH ACTS ON WHAT IT READ OR ON NOTHING. A plan or a
	// confirmation names the offer by id, and one that landed on whatever row
	// holds that id — a revision in history, or the boot row once it is no
	// offer — would record a publish of a revision nobody offered. The store
	// refuses it whatever its caller holds: `crewlet run` keeps the file from
	// the read it decides on until its engine has opened it, so it meets the
	// refusal only as a defect, and the store cannot know that.
	configs := db.Configs()
	history, err := configs.Insert(t.Context(), offline("history"))
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err = configs.PlanOffer(t.Context(), history, base, ""); !errors.Is(err, store.ErrNotOffered) {
		t.Errorf("planning a history row = %v, want ErrNotOffered", err)
	}
	id, err := configs.Offer(t.Context(), store.Offer{Base: history, Unconfirmed: "earlier"},
		offline("change"))
	if err != nil {
		t.Fatalf("offer: %v", err)
	}
	// THE PLAN SETTLES THE BASE: the pointer named the unconfirmed offer, so
	// that one landed, and the change is published over it alone.
	planned := base.Add(time.Minute)
	if err = configs.PlanOffer(t.Context(), id, planned, "earlier"); err != nil {
		t.Fatalf("plan: %v", err)
	}
	if boot := mustBoot(t, configs); boot.Offer == nil || !boot.Offer.PlannedAt.Equal(planned) ||
		boot.Offer.Base != "earlier" || boot.Offer.Unconfirmed != "" {
		t.Fatalf("offer = %+v, want planned at %s over the landed earlier offer alone", boot.Offer, planned)
	}
	if err = configs.ConfirmOffer(t.Context(), id); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	boot := mustBoot(t, configs)
	if boot.ID != id || boot.Offer != nil || !boot.ServedAt.IsZero() {
		t.Errorf("confirmed boot row = %+v, want the revision, no offer and no applied "+
			"instant until an apply serves it", boot)
	}
	if err = configs.ConfirmOffer(t.Context(), id); !errors.Is(err, store.ErrNotOffered) {
		t.Errorf("confirming twice = %v, want ErrNotOffered", err)
	}
}

func testOfferingAStoredRevision(t *testing.T, db *store.DB) {
	// The boot row is cleared first, so a missing id that committed anyway
	// would leave a node with NOTHING to start on, from one mistyped id. A
	// stored revision offered keeps the instant the fleet last gave it.
	configs := db.Configs()
	if err := configs.Serve(t.Context(), fleetRevision("p", base)); err != nil {
		t.Fatalf("serve: %v", err)
	}
	if err := configs.OfferStored(t.Context(), "missing", store.Offer{Base: "p"}); !errors.Is(err, store.ErrNoRevision) {
		t.Fatalf("offering a revision that does not exist = %v, want ErrNoRevision", err)
	}
	if boot := mustBoot(t, configs); boot.ID != "p" || boot.Offer != nil {
		t.Fatalf("the node lost its boot row: %+v", boot)
	}
	if err := configs.SawFleet(t.Context(), fleetRevision("old", base.Add(-time.Hour))); err != nil {
		t.Fatalf("saw: %v", err)
	}
	if err := configs.OfferStored(t.Context(), "old", store.Offer{Base: "p", Unconfirmed: "q"}); err != nil {
		t.Fatalf("offer stored: %v", err)
	}
	boot := mustBoot(t, configs)
	if boot.ID != "old" || boot.Offer == nil || boot.Offer.Base != "p" || boot.Offer.Unconfirmed != "q" ||
		!boot.ActivatedAt.Equal(base.Add(-time.Hour)) || !boot.ServedAt.IsZero() {
		t.Errorf("boots on %+v, want old offered over p or q at its last instant, applied never", boot)
	}
	// RE-OFFERING THE REVISION IT RUNS — the credential-rotation gesture —
	// drops the instant it was applied at: nothing has applied the choice.
	if err := configs.Serve(t.Context(), fleetRevision("p", base)); err != nil {
		t.Fatalf("serve p again: %v", err)
	}
	if err := configs.OfferStored(t.Context(), "p", store.Offer{Base: "p"}); err != nil {
		t.Fatalf("offer the running revision: %v", err)
	}
	if boot = mustBoot(t, configs); boot.ID != "p" || boot.Offer == nil || !boot.ServedAt.IsZero() {
		t.Errorf("boots on %+v, want p offered over itself with no applied instant", boot)
	}
}

func testRestorableIsTheFleetsRevisionUnlessAnOfferIsBuiltOnIt(t *testing.T, db *store.DB) {
	// WHAT A NODE KNOWS THE COMPANY TO BE, with no pointer to ask. A node that
	// refused R runs P, and restoring P would roll back every node that runs
	// R; an offline change built on R is the company somebody made here; one
	// built on something the fleet moved past is not.
	configs := db.Configs()
	restorable := func() string {
		t.Helper()
		r, found, err := configs.Restorable(t.Context())
		if err != nil {
			t.Fatalf("restorable: %v", err)
		}
		if !found {
			return ""
		}
		return r.ID
	}
	if got := restorable(); got != "" {
		t.Fatalf("an empty store restores %s, want nothing", got)
	}
	if err := configs.Serve(t.Context(), fleetRevision("p", base)); err != nil {
		t.Fatalf("serve: %v", err)
	}
	if got := restorable(); got != "p" {
		t.Errorf("a node that never saw a pointer restores %s, want its boot row p", got)
	}
	if err := configs.SawFleet(t.Context(), fleetRevision("r", base.Add(time.Hour))); err != nil {
		t.Fatalf("saw: %v", err)
	}
	if got := restorable(); got != "r" {
		t.Errorf("a node that refused r restores %s, want the fleet's r", got)
	}
	onFleets, err := configs.Offer(t.Context(), store.Offer{Base: "r"}, offline("on r"))
	if err != nil {
		t.Fatalf("offer: %v", err)
	}
	if got := restorable(); got != onFleets {
		t.Errorf("an offer built on the fleet's revision restores %s, want the offer %s", got, onFleets)
	}
	if _, err = configs.Offer(t.Context(), store.Offer{Base: "p"}, offline("on p")); err != nil {
		t.Fatalf("offer: %v", err)
	}
	if got := restorable(); got != "r" {
		t.Errorf("an offer built on a revision the fleet moved past restores %s, want r", got)
	}
	// AN OFFER BUILT ON AN UNCONFIRMED ONE THE FLEET WAS SEEN TO TAKE is built
	// on the fleet's revision too: that publish landed.
	if err = configs.SawFleet(t.Context(), fleetRevision("landed", base.Add(2*time.Hour))); err != nil {
		t.Fatalf("saw: %v", err)
	}
	onLanded, err := configs.Offer(t.Context(), store.Offer{Base: "r", Unconfirmed: "landed"},
		offline("on landed"))
	if err != nil {
		t.Fatalf("offer: %v", err)
	}
	if got := restorable(); got != onLanded {
		t.Errorf("an offer built on the unconfirmed offer the fleet took restores %s, want %s",
			got, onLanded)
	}
}

func testPayloadRoundTrips(t *testing.T, db *store.DB) {
	// The payload is opaque: the sealed envelope every writer stores, under
	// the keyring every node holds. What comes back must be what went in,
	// byte for byte — a re-serialized document would not decrypt.
	sealed := json.RawMessage(`{"__encrypted__":"enc:v1:abc.def"}`)
	r := offline("sealed")
	r.Payload = sealed
	id, err := db.Configs().Insert(t.Context(), r)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if got := mustGet(t, db.Configs(), id); string(got.Payload) != string(sealed) {
		t.Fatalf("payload = %s, want %s", got.Payload, sealed)
	}
}

func testRevisionsListInInsertionOrder(t *testing.T, db *store.DB) {
	// Two revisions written in one burst share a microsecond, so the
	// tiebreak decides what a reader sees first. A random uuid is stable
	// without being truthful — it can put the older one at the top — and a
	// history read in the wrong order is worse than one read slowly.
	configs := db.Configs()
	var ids []string
	for i := range 5 {
		r := offline("burst")
		r.Payload = json.RawMessage(`{"n":` + string(rune('0'+i)) + `}`)
		id, err := configs.Insert(t.Context(), r)
		if err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
		ids = append(ids, id)
	}
	listed, err := configs.List(t.Context(), 0, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != len(ids) {
		t.Fatalf("%d rows, want %d", len(listed), len(ids))
	}
	for i, revision := range listed {
		if want := ids[len(ids)-1-i]; revision.ID != want {
			t.Fatalf("position %d is %s, want %s — the listing is not newest-first",
				i, revision.ID, want)
		}
	}
}

func testARevisionRecordsWhatWroteIt(t *testing.T, db *store.DB) {
	// THE KIND IS STORED, NOT INFERRED. `created_by` is a label, and a
	// label cannot say whether a person, a credential or the engine wrote
	// — so a revision the node seeded, one a person bound to a seat wrote
	// and one an unbound credential wrote must read back as what they are,
	// through every reader the history has, each beside the credential it
	// went through.
	configs := db.Configs()
	type author struct {
		by       string
		kind     iam.ActorKind
		operator string
	}
	byOperator, err := configs.Offer(t.Context(), store.Offer{}, store.Revision{
		CreatedBy: "token:ops", CreatedByKind: iam.ActorOperator, OperatorID: "token:ops",
		Source: "api", Summary: "a credential's write", CreatedAt: base,
	})
	if err != nil {
		t.Fatalf("operator offer: %v", err)
	}
	byPerson, err := configs.Insert(t.Context(), store.Revision{
		CreatedBy: "maya", CreatedByKind: iam.ActorHuman,
		OperatorID: "session:0192f00d-0000-7000-8000-0000000000b0",
		Source:     "api", Summary: "a person's write", CreatedAt: base.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("human insert: %v", err)
	}
	byNode, err := configs.Insert(t.Context(), store.Revision{
		CreatedBy: "node-a", CreatedByKind: iam.ActorSystem,
		Source: "file", Summary: "a node's seed", CreatedAt: base.Add(2 * time.Minute),
	})
	if err != nil {
		t.Fatalf("node insert: %v", err)
	}
	want := map[string]author{
		byOperator: {"token:ops", iam.ActorOperator, "token:ops"},
		byPerson:   {"maya", iam.ActorHuman, "session:0192f00d-0000-7000-8000-0000000000b0"},
		byNode:     {"node-a", iam.ActorSystem, ""},
	}
	read := func(r store.Revision) author { return author{r.CreatedBy, r.CreatedByKind, r.OperatorID} }
	for id, w := range want {
		if got := mustGet(t, configs, id); read(got) != w {
			t.Errorf("Get(%s) = %+v, want %+v", id, read(got), w)
		}
	}
	listed, err := configs.List(t.Context(), 0, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != len(want) {
		t.Fatalf("List = %d revisions, want %d", len(listed), len(want))
	}
	for _, r := range listed {
		if read(r) != want[r.ID] {
			t.Errorf("List: %s = %+v, want %+v", r.ID, read(r), want[r.ID])
		}
	}
	if boot := mustBoot(t, configs); read(boot) != want[byOperator] {
		t.Errorf("Boot = %+v, want %+v", read(boot), want[byOperator])
	}
}

func testAnUnattributedWriteIsRefused(t *testing.T, db *store.DB) {
	// A WRITER ALWAYS KNOWS WHAT IT IS. A revision stored without saying is
	// exactly the row the audit screen used to fill in with a guess, so the
	// store refuses it — including a kind that is not an [iam.ActorKind],
	// `node` among them: the engine's own write is `system`.
	configs := db.Configs()
	for _, kind := range []iam.ActorKind{"", "seat", "peer", "node"} {
		r := store.Revision{CreatedBy: "x", CreatedByKind: kind, Summary: "s", CreatedAt: base}
		if _, err := configs.Offer(t.Context(), store.Offer{}, r); err == nil {
			t.Errorf("Offer with kind %q was stored", kind)
		}
		if _, err := configs.Insert(t.Context(), r); err == nil {
			t.Errorf("Insert with kind %q was stored", kind)
		}
		if _, err := configs.InsertOn(t.Context(), store.Revision{}, r); err == nil {
			t.Errorf("InsertOn with kind %q was stored", kind)
		}
	}
	if all, err := configs.List(t.Context(), 0, 0); err != nil || len(all) != 0 {
		t.Fatalf("a refused write left %d rows (err %v)", len(all), err)
	}
}

func testAFleetCopyKeepsItsOriginsAuthor(t *testing.T, db *store.DB) {
	// THE ORIGIN'S AUTHOR, AND ANY KIND IT CARRIES. A peer's copy of a
	// revision must read the way the origin's does — the credential
	// included — and a kind a newer build wrote is a value to keep rather
	// than a write to refuse: this node's fleet is on that revision either
	// way. Both copies a reconciler makes keep it: the one it records as the
	// fleet's, and the one it serves.
	configs := db.Configs()
	activated := base.Add(time.Hour)
	copies := map[string]func(store.Revision) error{
		"saw":   func(r store.Revision) error { return configs.SawFleet(t.Context(), r) },
		"serve": func(r store.Revision) error { return configs.Serve(t.Context(), r) },
	}
	for verb, copyOf := range copies {
		for id, kind := range map[string]iam.ActorKind{
			verb + "-op": iam.ActorOperator, verb + "-new": "seat",
		} {
			r := fleetRevision(id, activated)
			r.CreatedByKind = kind
			if err := copyOf(r); err != nil {
				t.Fatalf("%s %s: %v", verb, id, err)
			}
			got := mustGet(t, configs, id)
			if got.CreatedBy != "maya" || got.CreatedByKind != kind ||
				got.OperatorID != "pat:42" || got.Source != "api" {
				t.Errorf("%s copied as (%q, %q, %q, %q), want (maya, %q, pat:42, api)",
					id, got.CreatedBy, got.CreatedByKind, got.OperatorID, got.Source, kind)
			}
			// FROM THE FLEET'S INSTANT, never from the revision's
			// creation: the two differ on every re-activation of an
			// older revision.
			if !got.ActivatedAt.Equal(activated) || !got.CreatedAt.Equal(base) {
				t.Errorf("%s copied at %s, created %s; want at %s, created %s",
					id, got.ActivatedAt, got.CreatedAt, activated, base)
			}
		}
	}
}

func testAFleetCopyRefusesARevisionThatNamesNobody(t *testing.T, db *store.DB) {
	// A COPY STORES WHAT THE POINTER SAYS, AND THE POINTER ALWAYS SAYS IT: a
	// row with no author kind, no source or no instant would be a guess on
	// the audit screen, so the store refuses it rather than recording one,
	// and nothing is left behind. The credential alone may be absent — the
	// engine's own write names none — and is not asked for here.
	configs := db.Configs()
	for name, mutate := range map[string]func(*store.Revision){
		"no id":          func(r *store.Revision) { r.ID = "" },
		"no author kind": func(r *store.Revision) { r.CreatedByKind = "" },
		"no source":      func(r *store.Revision) { r.Source = "" },
		"no creation":    func(r *store.Revision) { r.CreatedAt = time.Time{} },
		"no activation":  func(r *store.Revision) { r.ActivatedAt = time.Time{} },
	} {
		r := fleetRevision("rev-1", base)
		mutate(&r)
		if err := configs.SawFleet(t.Context(), r); err == nil {
			t.Errorf("SawFleet with %s was stored", name)
		}
		if err := configs.Serve(t.Context(), r); err == nil {
			t.Errorf("Serve with %s was stored", name)
		}
	}
	if all, err := configs.List(t.Context(), 0, 0); err != nil || len(all) != 0 {
		t.Fatalf("a refused copy left %d rows (err %v)", len(all), err)
	}
}

func testAFleetCopyKeepsTheKnownAuthor(t *testing.T, db *store.DB) {
	// A row this node already holds keeps its author: the node that stored
	// its own write — or copied it first — is the authority on it. Only its
	// marks and its instant move.
	configs := db.Configs()
	own, err := configs.Insert(t.Context(), store.Revision{
		CreatedBy: "node-a", CreatedByKind: iam.ActorSystem, Source: "file",
		Summary: "mine", CreatedAt: base,
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	later := base.Add(2 * time.Hour)
	other := fleetRevision(own, later)
	other.CreatedBy, other.OperatorID = "somebody", "token:somebody"
	if err = configs.SawFleet(t.Context(), other); err != nil {
		t.Fatalf("saw over a known author: %v", err)
	}
	if err = configs.Serve(t.Context(), other); err != nil {
		t.Fatalf("serve over a known author: %v", err)
	}
	got := mustGet(t, configs, own)
	if got.CreatedBy != "node-a" || got.CreatedByKind != iam.ActorSystem ||
		got.OperatorID != "" || got.Source != "file" {
		t.Fatalf("a known author was overwritten: (%q, %q, %q, %q), want (node-a, system, none, file)",
			got.CreatedBy, got.CreatedByKind, got.OperatorID, got.Source)
	}
	if !got.Boots || !got.Fleets || !got.ActivatedAt.Equal(later) {
		t.Fatalf("copied boots=%v fleets=%v at %s, want both at %s",
			got.Boots, got.Fleets, got.ActivatedAt, later)
	}
}

func testAWriteOnABaseThisNodeNeverHeldKeepsTheBaseFirst(t *testing.T, db *store.DB) {
	// AN API WRITE IS BUILT ON THE FLEET'S REVISION, which a peer that has not
	// ticked since the pointer moved holds no row of — and the write's parent
	// is a foreign key into this table. So the base is kept first, with its
	// origin, and a base already held is left as it is.
	configs := db.Configs()
	fleets := fleetRevision("r", base)
	edit := offline("edit")
	edit.ParentID = "r"
	id, err := configs.InsertOn(t.Context(), fleets, edit)
	if err != nil {
		t.Fatalf("insert on an unheld base: %v", err)
	}
	if got := mustGet(t, configs, id); got.ParentID != "r" || got.Boots {
		t.Errorf("the write reads parent %q boots=%v, want parent r and unmarked", got.ParentID, got.Boots)
	}
	if got := mustGet(t, configs, "r"); got.CreatedBy != "maya" || got.Boots || got.Fleets {
		t.Errorf("the kept base reads %+v, want the origin's history row", got)
	}
	again := offline("again")
	again.ParentID = "r"
	held := fleetRevision("r", base)
	held.CreatedBy = "somebody else"
	if _, err = configs.InsertOn(t.Context(), held, again); err != nil {
		t.Fatalf("insert on a held base: %v", err)
	}
	if got := mustGet(t, configs, "r"); got.CreatedBy != "maya" {
		t.Errorf("a held base was rewritten: author %q", got.CreatedBy)
	}
	stray := offline("stray")
	stray.ParentID = "elsewhere"
	if _, err = configs.InsertOn(t.Context(), fleets, stray); err == nil {
		t.Error("a write naming a parent other than its base was stored")
	}
	if _, err = configs.InsertOn(t.Context(), store.Revision{}, offline("first")); err != nil {
		t.Errorf("a write built on no company: %v", err)
	}
}
