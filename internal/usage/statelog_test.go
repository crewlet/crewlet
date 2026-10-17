package usage_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
	"github.com/crewlet/crewlet/internal/store"
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
			// Migrate is nil: the tables ship in the replicated
			// migration `a_nodes_day_is_replicated`, so a fresh store
			// already has them — a schema created from test code would
			// be one the suite proved and the migration did not.
			Encode: encodeSuiteRecord,
			// THE VERSIONED-FIELD TABLE. It is empty — every kind this
			// domain writes, a person's day included, is in the base
			// format — and the record carrying each field fails on the
			// first row somebody adds without a case; a kind this build's
			// successor adds is a row keyed on the kind's value, and brings
			// its record the same way.
			Fields:   usage.VersionedFields(),
			Carrying: carryingSuiteField,
			Kinds: []string{string(usage.KindSeat), string(usage.KindSchedule),
				string(usage.KindPerson)},
			Rows:  usage.NewRows,
			Write: suiteWrite,
		}
	})
}

// carryingSuiteField builds a valid record carrying exactly one versioned
// field, with its version left for the encoder to stamp.
//
// THE TABLE IS EMPTY, so nothing reaches this today. A row added to it without
// a case here fails the suite rather than passing unexamined: a record that did
// not carry the field would be stamped at 1 and reported as the path being
// wrong, which is a fixture fault dressed as a domain one.
func carryingSuiteField(field statelog.VersionedField) ([]byte, error) {
	return nil, fmt.Errorf("no suite record carries %s — add a case that sets it "+
		"and nothing else versioned", field.Name)
}

// suiteWrite is one flush of the domain's own [usage.Publisher] — the only
// writer this domain has — over a node whose store holds one seat's day, run
// as the suite's own node so the subject's node and the stamp's writer agree
// as they do in production.
func suiteWrite(ctx context.Context, pub *statelog.Publisher, estate store.ReplicatedReader) error {
	p, err := usage.NewPublisher(usage.PublisherDeps{
		Store: oneSeatDay{}, Log: pub, Estate: estate, NodeID: statelogtest.SuiteWriter,
		Caught:      caught,
		Generations: unseen,
		Zone:        func() *time.Location { return time.UTC },
		Now:         func() time.Time { return time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC) },
		FireHistory: fireHistory,
	})
	if err != nil {
		return err
	}
	return p.Flush(ctx)
}

// oneSeatDay is a node's store holding one seat's spend, the same on every day
// it is asked about.
type oneSeatDay struct{}

func (oneSeatDay) UsageMark(context.Context, store.UsageWindow) (store.UsageMark, error) {
	return store.UsageMark{Events: 1, LastEvent: 1}, nil
}

// UsageRederive answers that no settle has changed an older day, and
// UsageWatermark that the node owes nothing older than the two days a tick
// fingerprints: the suite's write is one tick of those two days.
func (oneSeatDay) UsageRederive(context.Context) (store.UsageRederive, bool, error) {
	return store.UsageRederive{}, false, nil
}

func (oneSeatDay) ClearUsageRederive(context.Context, int64) (bool, error) { return false, nil }

func (oneSeatDay) UsageWatermark(context.Context) (time.Time, bool, error) {
	return time.Time{}, false, nil
}

func (oneSeatDay) SetUsageWatermark(context.Context, time.Time) error { return nil }

// UsageRepublished answers that the node never republished every day, and the
// suite's publisher never sees the fleet's generations, so it starts no pass.
func (oneSeatDay) UsageRepublished(context.Context) (string, bool, error) {
	return "", false, nil
}

func (oneSeatDay) SetUsageRepublished(context.Context, string) error { return nil }

// UsageDayWindow, UsageDayContaining and PinUsageDay record no cut: every day
// is the company clock's own, which never moves here.
func (oneSeatDay) UsageDayWindow(context.Context, string) (store.UsageWindow, bool, error) {
	return store.UsageWindow{}, false, nil
}

func (oneSeatDay) UsageDayContaining(context.Context, time.Time) (string, store.UsageWindow, bool, error) {
	return "", store.UsageWindow{}, false, nil
}

func (oneSeatDay) PinUsageDay(_ context.Context, _ string, w store.UsageWindow) (store.UsageWindow, error) {
	return w, nil
}

func (oneSeatDay) UsageForDay(context.Context, store.UsageWindow) (store.UsageDay, error) {
	return store.UsageDay{Seats: []store.UsageSeat{{
		AgentID: "suite-seat", Handle: "dev", Role: "Dev",
		Tokens: []store.UsageTokens{{Phase: "execute", Model: "m", Input: 10,
			Output: 5, Total: 15, Calls: 1}},
		Turns: store.UsageTurns{Count: 1, Durations: []time.Duration{40 * time.Second}},
	}}}, nil
}

// encodeSuiteRecord builds one valid record for an object of either kind, at
// any version — including one above this build's, which is what a newer peer
// publishes and this build has to retain — and, at version zero, at whatever
// the domain's own encoder stamps, which is the writer's path.
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
	case usage.KindPerson:
		subject.Person = id
		rec.Role = "Founder"
		rec.Tokens = []usage.Tokens{{Phase: "auxiliary", Worker: "answer_knowledge",
			Model: "m", Input: 40, Output: 2, Total: 42, Calls: 1}}
	}
	rec.Subject = subject
	return rec.Encode()
}
