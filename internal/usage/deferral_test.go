package usage_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/jetstream/jetstreamtest"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/usage"
)

// A NEWER BUILD'S DAYS OF ONE KIND ARE ALL RETAINED, through the real applier.
//
// This domain replays compacted, so a record this build cannot read is
// retained under its subject with whatever was retained there before deleted
// first. Two teams' days of a kind a later build adds, published by one node
// on one day, are two objects — and filed under a subject composed from the
// fields this build can decode, they were one, so the second retained deleted
// the first and an upgrade applied only the last. Both are published as that
// build would publish them, on its own subjects, and both must be held.
//
// Mutation: file a newer kind under the subject its decodable fields compose,
// and one row is left.
func TestANewerBuildsDaysOfOneKindAreAllRetained(t *testing.T) {
	t.Parallel()
	c := jetstreamtest.StartCluster(t, 3, js.Config{})
	spec := usage.Domain{}.Stream()
	q := c.Client(t, 0)
	if err := q.EnsureDomainStream(t.Context(), js.DomainStream{
		Name: spec.Name, Subjects: spec.Subjects, MaxBytes: 16 << 20,
		MaxPerSubject: spec.MaxPerSubject, MaxAge: spec.MaxAge,
		Duplicates: spec.Duplicates,
	}); err != nil {
		t.Fatalf("provision the log: %v", err)
	}
	node := joinFleet(t, q, "node-a", time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC))
	log, err := q.DomainLog(t.Context(), spec.Name)
	if err != nil {
		t.Fatalf("open the log: %v", err)
	}
	for _, team := range []string{"payments", "search"} {
		payload := newerKindFor(t, usage.RecordVersion+1, team)
		// THE NEWER BUILD'S OWN SUBJECT, composed as it composes it — never
		// this build's reading of the record.
		subject := topics.LogSubject(spec.SubjectPrefix, "a_kind_from_a_later_build",
			coord.DocumentKey("node-b", "2026-09-23", team))
		if _, _, err := log.Append(t.Context(), subject, "op-"+team, nil, payload); err != nil {
			t.Fatalf("append %s: %v", team, err)
		}
	}

	deadline := time.Now().Add(30 * time.Second)
	for {
		var held int
		if err := node.db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
			return tx.QueryRowContext(t.Context(),
				`SELECT COUNT(*) FROM `+usage.Domain{}.DeferredTable()).Scan(&held)
		}); err != nil {
			t.Fatalf("count the deferred records: %v", err)
		}
		if held == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the node holds %d deferred record(s) after 30s, want both teams' days", held)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if deferred, ok := node.runner.Deferred(); !ok {
		t.Fatalf("the applier reports nothing deferred (%+v)", deferred)
	}
}
