package usage_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
	"github.com/crewlet/crewlet/internal/usage"
)

// THE USAGE DOMAIN IS CERTIFIED BY THE FRAMEWORK'S OWN SUITE, as every domain
// is — and it is the second compacted one, so every case the vector domain
// found written for the strict domains' shape is already fixed, and anything
// left is this domain's own.
func TestTheUsageDomainIsACertifiedDomain(t *testing.T) {
	t.Parallel()
	statelogtest.Run(t, func(t *testing.T) statelogtest.Candidate {
		return statelogtest.Candidate{
			Domain:  usage.Domain{},
			Applier: usage.NewApplier(),
			// Migrate is nil: the tables ship in replicated/0017, so a
			// fresh store already has them — a schema created from test
			// code would be one the suite proved and the migration did not.
			Encode: encodeSuiteRecord,
			Kinds:  []string{string(usage.KindSeat), string(usage.KindSchedule)},
		}
	})
}

// encodeSuiteRecord builds one valid record for an object of either kind, at
// any version — including one above this build's, which is what a newer peer
// publishes and this build has to retain.
func encodeSuiteRecord(kind, id, opID string, version int) ([]byte, error) {
	subject := usage.Subject{Kind: usage.Kind(kind), Node: "suite-node", Day: "2026-09-23"}
	rec := usage.Record{
		RecordEnvelope: usage.RecordEnvelope{V: version, OpID: opID, Writer: "suite-node"},
	}
	switch usage.Kind(kind) {
	case usage.KindSeat:
		subject.Seat = id
		rec.Handle, rec.Role = "dev-"+id, "Dev"
		rec.Tokens = []usage.Tokens{{Phase: "execute", Model: "m", Input: 10,
			Output: 5, Total: 15, Calls: 1}}
		rec.Turns = &usage.Turns{Count: 1}
		rec.Turns.Durations.Add(40 * time.Second)
		rec.Reads = []usage.Read{{PageID: "p-" + id, Backend: "native", Via: "search", Count: 2}}
	case usage.KindSchedule:
		subject.ScopeType, subject.ScopeID, subject.Schedule = "role", "Dev", id
		rec.Fires = []usage.Fire{{At: time.Date(2026, 9, 23, 9, 0, 0, 0, time.UTC),
			Target: "dev", Outcome: "fired"}}
	}
	rec.Subject = subject
	return rec.Encode()
}
