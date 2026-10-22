package storetest

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/store"
)

// The company_config REVISION table's own behaviour.
//
// What is NOT here any more: the activation pointer, the apply status and the
// peer-health read. Those are fleet-shared state and moved to the coordination
// store — see internal/coord/fleet.go — where internal/coord/coordtest
// certifies them against every backend rather than against this one database.
// The revisions themselves stay: a payload is content, and the node reading
// it is the node running the seat.
func testOnlyOneRevisionIsActive(t *testing.T, db *store.DB) {
	// Enforced by the database rather than by the application remembering
	// to. Two active rows is a company whose configuration depends on which
	// one a query happened to return first.
	configs := db.Configs()
	first, err := configs.InsertActive(t.Context(), store.Revision{
		CreatedByKind: iam.ActorOperator,
		Summary:       "one", Payload: json.RawMessage(`{"n":1}`), CreatedAt: base,
	})
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := configs.InsertActive(t.Context(), store.Revision{
		CreatedByKind: iam.ActorOperator,
		ParentID:      first, Summary: "two",
		Payload: json.RawMessage(`{"n":2}`), CreatedAt: base.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	active, found, err := configs.Active(t.Context())
	if err != nil || !found {
		t.Fatalf("active: found=%v err=%v", found, err)
	}
	if active.ID != second {
		t.Fatalf("active is %s, want the newest (%s)", active.ID, second)
	}
	all, err := configs.List(t.Context(), 0, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("%d revisions, want 2 — history is not overwritten", len(all))
	}
	if all[0].ID != second {
		t.Fatalf("listing is not newest-first: %s came before %s", all[0].ID, second)
	}
}

func testAnInsertedRevisionIsHistoryUntilActivated(t *testing.T, db *store.DB) {
	// A write through a running node's API stores its revision before the
	// fleet has taken it, and one that loses the activation's compare-and-set
	// must stay exactly that: history. Made active on the way in, it was what
	// the node served and what the node published to the fleet at its next
	// start, so a refused write landed after all.
	configs := db.Configs()

	// With nothing active, an inserted revision leaves nothing active.
	lone, err := configs.Insert(t.Context(), store.Revision{
		CreatedByKind: iam.ActorOperator,
		Summary:       "refused", Payload: json.RawMessage(`{"n":0}`), CreatedAt: base,
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if active, found, aerr := configs.Active(t.Context()); aerr != nil || found {
		t.Fatalf("an inserted revision became active: %+v found=%v err=%v", active, found, aerr)
	}

	live, err := configs.InsertActive(t.Context(), store.Revision{
		CreatedByKind: iam.ActorOperator,
		Summary:       "live", Payload: json.RawMessage(`{"n":1}`), CreatedAt: base.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("insert the live revision: %v", err)
	}
	pending, err := configs.Insert(t.Context(), store.Revision{
		CreatedByKind: iam.ActorOperator,
		ParentID:      live, Summary: "pending",
		Payload: json.RawMessage(`{"n":2}`), CreatedAt: base.Add(2 * time.Minute),
	})
	if err != nil {
		t.Fatalf("insert the pending revision: %v", err)
	}
	if active, found, aerr := configs.Active(t.Context()); aerr != nil || !found || active.ID != live {
		t.Fatalf("active is %+v (found=%v err=%v), want the live revision %s kept", active, found, aerr, live)
	}
	stored, found, err := configs.Get(t.Context(), pending)
	if err != nil || !found {
		t.Fatalf("the inserted revision is not in the history: found=%v err=%v", found, err)
	}
	if stored.Active || !stored.ActivatedAt.IsZero() || stored.ParentID != live {
		t.Errorf("inserted = %+v, want inactive, never activated, with its parent", stored)
	}
	all, err := configs.List(t.Context(), 0, 0)
	if err != nil || len(all) != 3 {
		t.Fatalf("list: %d rows, err %v, want all three revisions", len(all), err)
	}

	// And activating it afterwards is the ordinary local activation.
	if _, err := configs.Activate(t.Context(), pending, base.Add(3*time.Minute)); err != nil {
		t.Fatalf("activate: %v", err)
	}
	if active, _, err := configs.Active(t.Context()); err != nil || active.ID != pending {
		t.Fatalf("active is %s (err %v), want the activated revision %s", active.ID, err, pending)
	}
	if again, _, _ := configs.Get(t.Context(), lone); again.Active {
		t.Error("an unrelated inserted revision became active")
	}
}

func testActivatingAMissingRevisionChangesNothing(t *testing.T, db *store.DB) {
	// The deactivate runs first, so a failure that committed anyway would
	// leave a company with NO active configuration — every node
	// unconfigured, every webhook refused, from one mistyped id.
	configs := db.Configs()
	id, err := configs.InsertActive(t.Context(), store.Revision{
		CreatedByKind: iam.ActorOperator,
		Summary:       "live", Payload: json.RawMessage(`{"n":1}`), CreatedAt: base,
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	if _, err := configs.Activate(t.Context(), "00000000-0000-0000-0000-000000000000", base); err == nil {
		t.Fatal("activating a revision that does not exist succeeded")
	} else if !errors.Is(err, store.ErrNoRevision) {
		t.Fatalf("error does not say the revision is missing: %v", err)
	}
	active, found, err := configs.Active(t.Context())
	if err != nil || !found {
		t.Fatalf("the company lost its active revision: found=%v err=%v", found, err)
	}
	if active.ID != id {
		t.Fatalf("active is %s, want %s", active.ID, id)
	}
}

func testALocalActivationNeverMovesTheCopyBack(t *testing.T, db *store.DB) {
	// THIS NODE'S ACTIVE REVISION IS ITS COPY OF THE FLEET'S POINTER, and the
	// pointer only moves forward. Two writers mark it on a running node: a
	// write once the fleet has taken it, and the reconciler once it has
	// applied an epoch. A reconciler finishing the apply of one epoch after
	// a write on this node had activated the next marked the older revision
	// active again, and the node answered GET /config with the revision the
	// write had replaced. So an activation older than the one held is
	// refused, inside the transaction, naming what is held.
	configs := db.Configs()
	older, err := configs.InsertActive(t.Context(), store.Revision{
		CreatedByKind: iam.ActorOperator,
		Summary:       "older", Payload: json.RawMessage(`{"n":1}`), CreatedAt: base,
	})
	if err != nil {
		t.Fatalf("insert the older revision: %v", err)
	}
	newer, err := configs.Insert(t.Context(), store.Revision{
		CreatedByKind: iam.ActorOperator, ParentID: older,
		Summary: "newer", Payload: json.RawMessage(`{"n":2}`), CreatedAt: base,
	})
	if err != nil {
		t.Fatalf("insert the newer revision: %v", err)
	}
	newerAt := base.Add(2 * time.Minute)
	if _, err = configs.Activate(t.Context(), newer, newerAt); err != nil {
		t.Fatalf("activate the newer revision: %v", err)
	}

	_, err = configs.Activate(t.Context(), older, base.Add(time.Minute))
	var superseded *store.SupersededError
	if !errors.As(err, &superseded) {
		t.Fatalf("activating an older activation answered %v, want a *store.SupersededError", err)
	}
	if superseded.Revision != older || superseded.Held != newer || !superseded.HeldAt.Equal(newerAt) {
		t.Errorf("superseded = %+v, want %s refused for %s held from %s",
			superseded, older, newer, newerAt)
	}
	active, found, err := configs.Active(t.Context())
	if err != nil || !found || active.ID != newer || !active.ActivatedAt.Equal(newerAt) {
		t.Fatalf("active = %s at %s (found=%v err=%v), want %s kept at %s",
			active.ID, active.ActivatedAt, found, err, newer, newerAt)
	}

	// The same revision at an OLDER instant is older too: re-activating an
	// unchanged revision is the credential-rotation gesture, and a copy
	// marked back to an earlier one describes an activation the fleet has
	// moved past.
	if _, err = configs.Activate(t.Context(), newer, base.Add(time.Minute)); !errors.As(err, &superseded) {
		t.Errorf("the held revision at an older instant answered %v, want superseded", err)
	}
	// The same activation again is not older, and changes nothing.
	if _, err = configs.Activate(t.Context(), newer, newerAt); err != nil {
		t.Errorf("marking the held activation again: %v", err)
	}
	// A later one moves the copy, whichever revision it names.
	laterAt := base.Add(3 * time.Minute)
	if _, err := configs.Activate(t.Context(), older, laterAt); err != nil {
		t.Fatalf("a later activation was refused: %v", err)
	}
	if active, _, err := configs.Active(t.Context()); err != nil || active.ID != older ||
		!active.ActivatedAt.Equal(laterAt) {
		t.Errorf("active = %s at %s (err %v), want %s at %s", active.ID, active.ActivatedAt,
			err, older, laterAt)
	}
	// And a missing revision is missing, whatever its instant says.
	if _, err := configs.Activate(t.Context(), "00000000-0000-0000-0000-000000000000",
		base); !errors.Is(err, store.ErrNoRevision) {
		t.Errorf("activating a missing revision at an older instant answered %v, want ErrNoRevision", err)
	}
}

func testAnAdoptionNeverMovesTheCopyBack(t *testing.T, db *store.DB) {
	// A PEER FETCHES THE REVISION OF THE EPOCH IT IS APPLYING, and an
	// activation of the next can land on it meanwhile. The fetched revision
	// is kept as history — this node met it — and the copy stays on the
	// later activation.
	configs := db.Configs()
	heldAt := base.Add(time.Hour)
	if err := configs.Adopt(t.Context(), store.Revision{
		ID: "rev-later", CreatedBy: "maya", CreatedByKind: iam.ActorOperator,
		Source: "api", Summary: "later", CreatedAt: base, ActivatedAt: heldAt,
	}); err != nil {
		t.Fatalf("adopt the later revision: %v", err)
	}
	err := configs.Adopt(t.Context(), store.Revision{
		ID: "rev-earlier", CreatedBy: "maya", CreatedByKind: iam.ActorOperator,
		Source: "api", Summary: "earlier", CreatedAt: base, ActivatedAt: base.Add(time.Minute),
	})
	var superseded *store.SupersededError
	if !errors.As(err, &superseded) || superseded.Held != "rev-later" {
		t.Fatalf("adopting an older activation answered %v, want superseded by rev-later", err)
	}
	active, found, err := configs.Active(t.Context())
	if err != nil || !found || active.ID != "rev-later" || !active.ActivatedAt.Equal(heldAt) {
		t.Fatalf("active = %s at %s (found=%v err=%v), want rev-later kept at %s",
			active.ID, active.ActivatedAt, found, err, heldAt)
	}
	kept, found, err := configs.Get(t.Context(), "rev-earlier")
	if err != nil || !found {
		t.Fatalf("the earlier revision is not in the history: found=%v err=%v", found, err)
	}
	if kept.Active || !kept.ActivatedAt.IsZero() || kept.Summary != "earlier" {
		t.Errorf("the earlier revision was kept as %+v, want inactive history", kept)
	}
}

func testPayloadRoundTrips(t *testing.T, db *store.DB) {
	// The payload is opaque: the sealed envelope every writer stores, under
	// the keyring every node holds. What comes back must be what went in,
	// byte for byte — a re-serialized document would not decrypt.
	sealed := json.RawMessage(`{"__encrypted__":"enc:v1:abc.def"}`)
	id, err := db.Configs().InsertActive(t.Context(), store.Revision{
		CreatedByKind: iam.ActorOperator,
		Summary:       "sealed", Payload: sealed, CreatedAt: base,
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	got, found, err := db.Configs().Get(t.Context(), id)
	if err != nil || !found {
		t.Fatalf("get: found=%v err=%v", found, err)
	}
	if string(got.Payload) != string(sealed) {
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
		id, err := configs.InsertActive(t.Context(), store.Revision{
			CreatedByKind: iam.ActorOperator,
			Summary:       "burst", CreatedAt: base,
			Payload: json.RawMessage(`{"n":` + string(rune('0'+i)) + `}`),
		})
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
		want := ids[len(ids)-1-i]
		if revision.ID != want {
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
	byOperator, err := configs.InsertActive(t.Context(), store.Revision{
		CreatedBy: "token:ops", CreatedByKind: iam.ActorOperator, OperatorID: "token:ops",
		Source: "api", Summary: "a credential's write", CreatedAt: base,
	})
	if err != nil {
		t.Fatalf("operator insert: %v", err)
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
		got, found, gerr := configs.Get(t.Context(), id)
		if gerr != nil || !found {
			t.Fatalf("get %s: found=%v err=%v", id, found, gerr)
		}
		if read(got) != w {
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
	active, _, err := configs.Active(t.Context())
	if err != nil || read(active) != want[byOperator] {
		t.Errorf("Active = %+v (err %v), want %+v", read(active), err, want[byOperator])
	}
}

func testAnUnattributedWriteIsRefused(t *testing.T, db *store.DB) {
	// A WRITER ALWAYS KNOWS WHAT IT IS. A revision stored without saying is
	// exactly the row the audit screen used to fill in with a guess, so the
	// store refuses it — including a kind that is not an [iam.ActorKind],
	// `node` among them: the engine's own write is `system`.
	configs := db.Configs()
	for _, kind := range []iam.ActorKind{"", "seat", "peer", "node"} {
		if _, err := configs.InsertActive(t.Context(), store.Revision{
			CreatedBy: "x", CreatedByKind: kind, Summary: "s", CreatedAt: base,
		}); err == nil {
			t.Errorf("InsertActive with kind %q was stored", kind)
		}
		if _, err := configs.Insert(t.Context(), store.Revision{
			CreatedBy: "x", CreatedByKind: kind, Summary: "s", CreatedAt: base,
		}); err == nil {
			t.Errorf("Insert with kind %q was stored", kind)
		}
	}
	if all, err := configs.List(t.Context(), 0, 0); err != nil || len(all) != 0 {
		t.Fatalf("a refused write left %d rows (err %v)", len(all), err)
	}
}

func testAnAdoptedRevisionKeepsItsOriginsAuthor(t *testing.T, db *store.DB) {
	// THE ORIGIN'S AUTHOR, AND ANY KIND IT CARRIES. A peer's copy of a
	// revision must read the way the origin's does — the credential
	// included — and a kind a newer build wrote is a value to keep rather
	// than a write to refuse: this node is running that revision either
	// way.
	configs := db.Configs()
	activated := base.Add(time.Hour)
	for id, kind := range map[string]iam.ActorKind{
		"rev-op": iam.ActorOperator, "rev-new": "seat",
	} {
		if err := configs.Adopt(t.Context(), store.Revision{
			ID: id, CreatedBy: "maya", CreatedByKind: kind, OperatorID: "pat:42",
			Source: "api", Summary: "adopted", CreatedAt: base, ActivatedAt: activated,
		}); err != nil {
			t.Fatalf("adopt %s: %v", id, err)
		}
		got, found, err := configs.Get(t.Context(), id)
		if err != nil || !found {
			t.Fatalf("get %s: found=%v err=%v", id, found, err)
		}
		if got.CreatedBy != "maya" || got.CreatedByKind != kind ||
			got.OperatorID != "pat:42" || got.Source != "api" {
			t.Errorf("%s adopted as (%q, %q, %q, %q), want (maya, %q, pat:42, api)",
				id, got.CreatedBy, got.CreatedByKind, got.OperatorID, got.Source, kind)
		}
		// ACTIVE FROM THE FLEET'S INSTANT, never from the revision's
		// creation: the two differ on every re-activation of an older
		// revision, and activated_at is what this node boots its chart with.
		if !got.Active || !got.ActivatedAt.Equal(activated) || !got.CreatedAt.Equal(base) {
			t.Errorf("%s adopted active=%v at %s, created %s; want active at %s, created %s",
				id, got.Active, got.ActivatedAt, got.CreatedAt, activated, base)
		}
	}
}

func testAnAdoptionRefusesARevisionThatNamesNobody(t *testing.T, db *store.DB) {
	// AN ADOPTION STORES WHAT THE POINTER SAYS, AND THE POINTER ALWAYS SAYS
	// IT: a row with no author kind, no source or no instant would be a
	// guess on the audit screen, so the store refuses it rather than
	// recording one, and nothing is left behind. The credential alone may
	// be absent — the engine's own write names none — and is not asked
	// for here.
	configs := db.Configs()
	whole := store.Revision{
		ID: "rev-1", CreatedBy: "maya", CreatedByKind: iam.ActorOperator,
		OperatorID: "pat:42", Source: "api", Summary: "s", CreatedAt: base,
		ActivatedAt: base,
	}
	for name, mutate := range map[string]func(*store.Revision){
		"no author kind": func(r *store.Revision) { r.CreatedByKind = "" },
		"no source":      func(r *store.Revision) { r.Source = "" },
		"no creation":    func(r *store.Revision) { r.CreatedAt = time.Time{} },
		"no activation":  func(r *store.Revision) { r.ActivatedAt = time.Time{} },
	} {
		r := whole
		mutate(&r)
		if err := configs.Adopt(t.Context(), r); err == nil {
			t.Errorf("Adopt with %s was stored", name)
		}
	}
	if all, err := configs.List(t.Context(), 0, 0); err != nil || len(all) != 0 {
		t.Fatalf("a refused adoption left %d rows (err %v)", len(all), err)
	}
}

func testAReAdoptionKeepsTheKnownAuthor(t *testing.T, db *store.DB) {
	// A row this node already holds keeps its author: the node that stored
	// its own write — or adopted it first — is the authority on it. Only
	// its activation moves.
	configs := db.Configs()
	own, err := configs.InsertActive(t.Context(), store.Revision{
		CreatedBy: "node-a", CreatedByKind: iam.ActorSystem, Source: "file",
		Summary: "mine", CreatedAt: base,
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	later := base.Add(2 * time.Hour)
	if err = configs.Adopt(t.Context(), store.Revision{
		ID: own, CreatedBy: "somebody", CreatedByKind: iam.ActorOperator,
		OperatorID: "token:somebody", Source: "api", Summary: "mine", CreatedAt: base,
		ActivatedAt: later,
	}); err != nil {
		t.Fatalf("adopt over a known author: %v", err)
	}
	got, _, err := configs.Get(t.Context(), own)
	if err != nil || got.CreatedBy != "node-a" || got.CreatedByKind != iam.ActorSystem ||
		got.OperatorID != "" || got.Source != "file" {
		t.Fatalf("a known author was overwritten: (%q, %q, %q, %q) err %v, want (node-a, system, none, file)",
			got.CreatedBy, got.CreatedByKind, got.OperatorID, got.Source, err)
	}
	if !got.Active || !got.ActivatedAt.Equal(later) {
		t.Fatalf("re-adopted active=%v at %s, want active at %s", got.Active, got.ActivatedAt, later)
	}
}
