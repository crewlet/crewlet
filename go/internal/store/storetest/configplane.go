package storetest

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/configplane"
	"github.com/crewlet/crewlet/internal/store"
)

// The control plane's cases. Two things here are exactly the kind of statement
// one driver accepts and the other does not — an AUTOINCREMENT primary key used
// as a monotonic counter, and a multi-statement transaction whose failure must
// leave no company active — so both are asserted against both drivers.

func testActivationEpochIsMonotonic(t *testing.T, db *store.DB) {
	// The epoch is a FENCE: every node compares it against the epoch it has
	// applied. A counter that went backwards, or one that reused a value,
	// would let a new activation look older than one a node already has —
	// so the node keeps serving the previous company and reports converged.
	plane := db.ControlPlane()
	var last int64
	for i := range 5 {
		epoch, err := plane.RecordActivation(t.Context(), "rev-a", "", base.Add(time.Duration(i)*time.Second))
		if err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
		if epoch <= last {
			t.Fatalf("epoch %d did not advance past %d", epoch, last)
		}
		last = epoch
	}
}

func testReactivatingTheSameRevisionMovesThePointer(t *testing.T, db *store.DB) {
	// THE reason the pointer is an append log. Re-activating an unchanged
	// revision is how an operator asks a running fleet to re-resolve its
	// ${VAR} references and pick up a rotated credential — a pointer keyed
	// on the revision id could not express it, so it would rebuild nothing
	// on exactly the operation performed to make it rebuild.
	configs := db.Configs()
	id, first, err := configs.InsertActive(t.Context(), store.Revision{
		Source: "import", CreatedBy: "founder", Summary: "initial",
		Payload: json.RawMessage(`{"name":"Acme"}`), CreatedAt: base,
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	second, err := configs.Activate(t.Context(), id, base.Add(time.Minute))
	if err != nil {
		t.Fatalf("re-activate: %v", err)
	}
	if second <= first {
		t.Fatalf("re-activating the same revision left the pointer at %d (was %d), "+
			"so nothing in the fleet reconciles", second, first)
	}
	target, found, err := db.ControlPlane().Target(t.Context())
	if err != nil || !found {
		t.Fatalf("target: found=%v err=%v", found, err)
	}
	if target.Epoch != second || target.RevisionID != id {
		t.Fatalf("target = %+v, want epoch %d of %s", target, second, id)
	}
}

func testNoActivationIsNotAnError(t *testing.T, db *store.DB) {
	// A fresh deployment has no activation, which is what "unconfigured"
	// means. Reporting that as an error would make a database outage and a
	// company nobody has configured yet look identical.
	_, found, err := db.ControlPlane().Target(t.Context())
	if err != nil {
		t.Fatalf("target on an empty plane: %v", err)
	}
	if found {
		t.Fatal("an empty control plane reported an activation")
	}
	if _, found, err := db.Configs().Active(t.Context()); err != nil || found {
		t.Fatalf("active on an empty store: found=%v err=%v", found, err)
	}
}

func testOnlyOneRevisionIsActive(t *testing.T, db *store.DB) {
	// Enforced by the database rather than by the application remembering
	// to. Two active rows is a company whose configuration depends on which
	// one a query happened to return first.
	configs := db.Configs()
	first, _, err := configs.InsertActive(t.Context(), store.Revision{
		Summary: "one", Payload: json.RawMessage(`{"n":1}`), CreatedAt: base,
	})
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, _, err := configs.InsertActive(t.Context(), store.Revision{
		ParentID: first, Summary: "two",
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

func testActivatingAMissingRevisionChangesNothing(t *testing.T, db *store.DB) {
	// The deactivate runs first, so a failure that committed anyway would
	// leave a company with NO active configuration — every node
	// unconfigured, every webhook refused, from one mistyped id.
	configs := db.Configs()
	id, _, err := configs.InsertActive(t.Context(), store.Revision{
		Summary: "live", Payload: json.RawMessage(`{"n":1}`), CreatedAt: base,
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
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
	// The payload is opaque: a sealed envelope when a keyring is
	// configured, the plaintext document when one is not. Either way what
	// comes back must be what went in, byte for byte — a re-serialized
	// document would not decrypt.
	sealed := json.RawMessage(`{"__encrypted__":"enc:v1:abc.def"}`)
	id, _, err := db.Configs().InsertActive(t.Context(), store.Revision{
		Summary: "sealed", Payload: sealed, CreatedAt: base,
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

func testApplyStatusIsALastWord(t *testing.T, db *store.DB) {
	// Keyed by node, so a second report REPLACES the first: this table
	// answers "where is each node now", and a history of every apply a
	// long-lived node ever made grows without bound to answer a question
	// nobody asks.
	plane := db.ControlPlane()
	for i, status := range []configplane.ApplyStatus{
		configplane.StatusError, configplane.StatusOK,
	} {
		if err := plane.RecordApply(t.Context(), store.NodeApply{
			NodeID: "node-a", Epoch: int64(i + 1), RevisionID: "rev-1",
			Status: status, UpdatedAt: base.Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
	}
	fleet, err := plane.Fleet(t.Context())
	if err != nil {
		t.Fatalf("fleet: %v", err)
	}
	if len(fleet) != 1 {
		t.Fatalf("%d rows for one node, want 1", len(fleet))
	}
	if fleet[0].Status != configplane.StatusOK || fleet[0].Epoch != 2 {
		t.Fatalf("fleet[0] = %+v, want the latest report", fleet[0])
	}
}

func testPeerHealthExcludesTheAskerAndTheStale(t *testing.T, db *store.DB) {
	// The freshness bound is what stops a scaled-in pod's ghost `ok` from
	// making a diverged survivor shed its seats to a node that no longer
	// exists — the company goes dark exactly where it should have gone
	// degraded and raised an alarm.
	plane := db.ControlPlane()
	seed := func(node string, status configplane.ApplyStatus, at time.Time) {
		t.Helper()
		if err := plane.RecordApply(t.Context(), store.NodeApply{
			NodeID: node, Epoch: 7, RevisionID: "rev-7",
			Status: status, UpdatedAt: at,
		}); err != nil {
			t.Fatalf("seed %s: %v", node, err)
		}
	}
	seed("self", configplane.StatusError, base)
	seed("fresh-ok", configplane.StatusOK, base)
	seed("fresh-degraded", configplane.StatusDegraded, base)
	seed("ghost", configplane.StatusOK, base.Add(-time.Hour))

	ok, reported, err := plane.PeerHealth(t.Context(), 7, "self", base.Add(-time.Minute))
	if err != nil {
		t.Fatalf("peer health: %v", err)
	}
	if ok != 1 {
		t.Errorf("peers ok = %d, want 1 — degraded is not somewhere work can go, "+
			"and a stale row is not evidence about now", ok)
	}
	if reported != 2 {
		t.Errorf("peers reported = %d, want 2 (the asker and the ghost excluded)", reported)
	}
}

func testPeerHealthIsPerEpoch(t *testing.T, db *store.DB) {
	// A peer that applied the PREVIOUS epoch cleanly is not evidence about
	// this one. Counting it would make every rollout look like a fleet that
	// had already converged.
	plane := db.ControlPlane()
	if err := plane.RecordApply(t.Context(), store.NodeApply{
		NodeID: "peer", Epoch: 6, Status: configplane.StatusOK, UpdatedAt: base,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	ok, reported, err := plane.PeerHealth(t.Context(), 7, "self", base.Add(-time.Minute))
	if err != nil {
		t.Fatalf("peer health: %v", err)
	}
	if ok != 0 || reported != 0 {
		t.Fatalf("a peer on epoch 6 counted toward epoch 7: ok=%d reported=%d", ok, reported)
	}
}

func testApplyStatusPurge(t *testing.T, db *store.DB) {
	plane := db.ControlPlane()
	for node, at := range map[string]time.Time{
		"gone": base.Add(-2 * time.Hour), "here": base,
	} {
		if err := plane.RecordApply(t.Context(), store.NodeApply{
			NodeID: node, Epoch: 1, Status: configplane.StatusOK, UpdatedAt: at,
		}); err != nil {
			t.Fatalf("seed %s: %v", node, err)
		}
	}
	n, err := plane.Purge(t.Context(), base.Add(-time.Hour))
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != 1 {
		t.Fatalf("purged %d rows, want the 1 older than the cutoff", n)
	}
	fleet, err := plane.Fleet(t.Context())
	if err != nil {
		t.Fatalf("fleet: %v", err)
	}
	if len(fleet) != 1 || fleet[0].NodeID != "here" {
		t.Fatalf("fleet = %+v, want only the node still reporting", fleet)
	}
}

func testApplyStatusRejectsAnIncompleteIdentity(t *testing.T, db *store.DB) {
	// A row with no node id upserts onto the empty-string key, so every
	// node that forgot the field shares one row — and the fleet view shows
	// one anonymous entry where a dozen nodes should be.
	plane := db.ControlPlane()
	if err := plane.RecordApply(t.Context(), store.NodeApply{
		Epoch: 1, Status: configplane.StatusOK, UpdatedAt: base,
	}); err == nil {
		t.Error("an apply status with no node id was written")
	}
	if err := plane.RecordApply(t.Context(), store.NodeApply{
		NodeID: "node-a", Epoch: 1, Status: configplane.StatusOK,
	}); err == nil {
		t.Error("an apply status with no timestamp was written, so it can never expire")
	}
}

func testApplyErrorIsBounded(t *testing.T, db *store.DB) {
	// Every peer reads this column on every reconcile tick, and the fleet
	// view renders it. One node's driver dump must not become a download
	// for the rest of the fleet.
	plane := db.ControlPlane()
	long := make([]byte, store.MaxApplyErrorLength*3)
	for i := range long {
		long[i] = 'x'
	}
	if err := plane.RecordApply(t.Context(), store.NodeApply{
		NodeID: "node-a", Epoch: 1, Status: configplane.StatusError,
		Error: string(long), UpdatedAt: base,
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	fleet, err := plane.Fleet(t.Context())
	if err != nil {
		t.Fatalf("fleet: %v", err)
	}
	if got := len(fleet[0].Error); got != store.MaxApplyErrorLength {
		t.Fatalf("stored %d characters of error, want it bounded to %d",
			got, store.MaxApplyErrorLength)
	}
}

func testUnknownApplyStatusIsRefused(t *testing.T, db *store.DB) {
	// The three outcomes are a closed set, and the third one — degraded —
	// is the one that decides whether a node counts as a healthy peer. A
	// fourth value nobody handles would be read as "not ok" by the peer
	// count and as converged by nothing, which is a state no reader agrees
	// about.
	err := db.ControlPlane().RecordApply(t.Context(), store.NodeApply{
		NodeID: "node-a", Epoch: 1, Status: "probably-fine", UpdatedAt: base,
	})
	if err == nil {
		t.Fatal("a status outside the closed set was stored")
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
		id, _, err := configs.InsertActive(t.Context(), store.Revision{
			Summary: "burst", CreatedAt: base,
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
