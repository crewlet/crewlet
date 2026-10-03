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
	for id, kind := range map[string]iam.ActorKind{
		"rev-op": iam.ActorOperator, "rev-new": "seat", "rev-unknown": "",
	} {
		if err := configs.Adopt(t.Context(), store.Revision{
			ID: id, CreatedBy: "maya", CreatedByKind: kind, OperatorID: "pat:42",
			Source: "api", Summary: "adopted", CreatedAt: base,
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
	}
}

func testAnAdoptionLearnsAnUnknownAuthor(t *testing.T, db *store.DB) {
	// A ROW ADOPTED BEFORE THE FLEET SAID WHO WROTE IT IS FILLED IN LATER —
	// the author, its kind and the credential together — and a row that
	// knows its author is never overwritten: the node that stored its own
	// write is the authority on it.
	configs := db.Configs()
	if err := configs.Adopt(t.Context(), store.Revision{
		ID: "rev-1", Source: "fleet", Summary: "s", CreatedAt: base,
	}); err != nil {
		t.Fatalf("adopt with no author: %v", err)
	}
	if err := configs.Adopt(t.Context(), store.Revision{
		ID: "rev-1", CreatedBy: "maya", CreatedByKind: iam.ActorHuman,
		OperatorID: "pat:42", Source: "api", Summary: "s", CreatedAt: base,
	}); err != nil {
		t.Fatalf("adopt again with the author: %v", err)
	}
	got, _, err := configs.Get(t.Context(), "rev-1")
	if err != nil || got.CreatedBy != "maya" || got.CreatedByKind != iam.ActorHuman ||
		got.OperatorID != "pat:42" {
		t.Fatalf("after the fleet named the author: (%q, %q, %q) err %v, want (maya, human, pat:42)",
			got.CreatedBy, got.CreatedByKind, got.OperatorID, err)
	}

	own, err := configs.InsertActive(t.Context(), store.Revision{
		CreatedBy: "node-a", CreatedByKind: iam.ActorSystem, Source: "file",
		Summary: "mine", CreatedAt: base,
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err = configs.Adopt(t.Context(), store.Revision{
		ID: own, CreatedBy: "somebody", CreatedByKind: iam.ActorOperator,
		OperatorID: "token:somebody", Source: "api", Summary: "mine", CreatedAt: base,
	}); err != nil {
		t.Fatalf("adopt over a known author: %v", err)
	}
	got, _, err = configs.Get(t.Context(), own)
	if err != nil || got.CreatedBy != "node-a" || got.CreatedByKind != iam.ActorSystem ||
		got.OperatorID != "" {
		t.Fatalf("a known author was overwritten: (%q, %q, %q) err %v, want (node-a, system, none)",
			got.CreatedBy, got.CreatedByKind, got.OperatorID, err)
	}
}
