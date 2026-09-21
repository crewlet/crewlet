package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// THE REGISTER: every domain this build runs, and everything the engine has to
// know to run one, in ONE fixed-order table.
//
// # Why one table rather than a list plus four switches
//
// A domain used to be declared in five places: the list itself, the applier
// switch, the write-authority switch, the barrier switch and the Tier A
// ceiling switch. Three of those refuse a domain they do not know, naming it,
// so forgetting them is a boot failure with an explanation. THE BARRIER SWITCH
// DOES NOT: a domain missing from it silently gets no read index, and
// therefore no `linearizable`, which is a correct answer for one domain
// (the vectors are derived and compacted, so "as of a position" is not a
// question about them) and a silent wrong one for every other. A reviewer
// cannot tell the two apart by reading the switch, because absent and
// deliberately-absent look identical.
//
// So the barrier decision moves into the entry and becomes IMPOSSIBLE TO
// OMIT: an entry states an encoder or states [registration.NoBarrier], and
// [checkRegister] refuses one that states neither or both. That is the whole
// reason the table exists — the other four collapse into it because a domain
// declared in one place is read in one place.
//
// # The order is still load-bearing
//
// The register is a SLICE and its order is the order every operator surface
// reports in, the order streams are provisioned in and the order an offer's
// terms are compared in. A map would reorder all of them on a whim of the
// runtime, which is why this is a table rather than a registry each domain
// writes itself into from an init.
//
// # Why the constructors are functions on the engine rather than methods on
// the domain
//
// [statelog.Domain] is the DECLARATION — what a snapshot, a claim and a sweep
// read — and it must be answerable by a build that cannot construct the
// applier or the seams at all. An applier holds this node's id, its store
// handle and, for the knowledge base, the skill detector and its nudge; none
// of those belong to a value a manifest carries. So the constructors take the
// engine's own state log and live here, next to the declaration that names
// them, rather than inside it.

// registration is one domain's complete entry in the register.
//
// Every field but [registration.Barrier] and [registration.NoBarrier] is
// required, and [checkRegister] is what says so: an entry that omits one is a
// boot failure naming the domain and the field, never a node that runs a
// domain it cannot apply, cannot write to or cannot size a stream for.
type registration struct {
	// Domain is the declaration itself.
	Domain statelog.Domain

	// NewApplier builds the state machine that turns this domain's records
	// into rows. Required: a domain with no applier would have its records
	// consumed and produce nothing on this node.
	NewApplier func(s *stateLog) (statelog.Applier, error)

	// NewSeams builds the write authority's three seams and the domain's
	// eviction reader. Required: a domain with no write authority is one
	// nothing could ever append to.
	NewSeams func(s *stateLog, runner *statelog.Runner) (writeSeams, error)

	// Barrier encodes the framework's barrier append as one of this
	// domain's own records. A domain that declares one gets a read index
	// and therefore `linearizable`; one that declares [NoBarrier] instead
	// gets neither, and its reads make no freshness claim beyond `session`.
	//
	// EXACTLY ONE of Barrier and NoBarrier is stated, which is what makes
	// "this domain grants no linearizable read" a decision in the diff
	// rather than a line nobody wrote.
	Barrier func(statelog.Envelope) ([]byte, error)

	// NoBarrier is the explicit form of a nil Barrier. See Barrier.
	NoBarrier bool

	// Ceiling is where Tier A puts this domain's stream ceiling, and what
	// a refusal to reserve it names. Required: a domain Tier A declares no
	// ceiling for would reserve its own default outside the budget every
	// other state log is sized into.
	Ceiling func(stream config.Stream, free int64) domainCeiling
}

// writeSeams is what one domain's write authority is built from: the three
// seams [statelog.Deps] takes, and the eviction reader readiness asks through.
//
// EVICTED MAY BE NIL, and that is the domain rather than an omission — the
// vectors are derived and compacted, so there is no tombstone table to read
// and nothing an evicted node could serve that a re-embed would not replace.
// It is not paired with a "no eviction reader" flag the way the barrier is,
// because a nil one refuses nothing silently: readiness asks the fence it was
// built from, and a domain with no fence has no answer to give either way.
type writeSeams struct {
	Rows    statelog.Rows
	Fence   statelog.Fence
	Gates   statelog.Gates
	Evicted func(context.Context) (bool, error)
}

// register is every domain this build runs, in a FIXED order.
func register() []registration {
	return []registration{
		{
			Domain: tracker.Domain{},
			NewApplier: func(s *stateLog) (statelog.Applier, error) {
				return tracker.NewApplier(s.nodeID), nil
			},
			NewSeams: func(s *stateLog, runner *statelog.Runner) (writeSeams, error) {
				rows, err := tracker.NewRows(s.db)
				if err != nil {
					return writeSeams{}, err
				}
				fence := tracker.NewFence(s.db, s.nodeID)
				fence.Cursor = runner.Committed
				fence.Floor = s.trimFloor(tracker.Domain{}.Name(),
					func() uint32 { return runner.Committed().Generation })
				return writeSeams{Rows: rows, Fence: fence,
					Gates: tracker.NewGates(s.db), Evicted: fence.Evicted}, nil
			},
			Barrier: tracker.EncodeBarrier,
			Ceiling: func(stream config.Stream, free int64) domainCeiling {
				bytes, derived := stream.LogMaxBytes(free)
				return domainCeiling{Bytes: bytes,
					Field: "stream.tracker_log_max_bytes", Explicit: !derived}
			},
		},
		{
			Domain: search.Domain{},
			NewApplier: func(*stateLog) (statelog.Applier, error) {
				return search.NewApplier(), nil
			},
			NewSeams: func(s *stateLog, _ *statelog.Runner) (writeSeams, error) {
				rows, err := search.NewRows(s.db)
				if err != nil {
					return writeSeams{}, err
				}
				return writeSeams{Rows: rows, Fence: search.NewFence(),
					Gates: search.NewGates()}, nil
			},
			// NO BARRIER, DECLARED: the vectors are derived from sources
			// another domain owns and compacted to one message per
			// source, so a read of them makes no claim about a position
			// and there is nothing a barrier could prove.
			NoBarrier: true,
			Ceiling: func(stream config.Stream, free int64) domainCeiling {
				bytes, _ := stream.VectorsMaxBytes(free)
				return domainCeiling{Bytes: bytes,
					Field:    "stream.tracker_vectors_max_bytes",
					Explicit: stream.TrackerVectorsMaxBytes > 0}
			},
		},
		{
			Domain: pages.Domain{},
			NewApplier: func(s *stateLog) (statelog.Applier, error) {
				// THE PARSER AND THE NUDGE COME FROM HERE, because the
				// apply is what notices a tool-skill page arriving or
				// leaving and there is no other delivery to hang the
				// resync off. The nudge is safe to take before the
				// native runtime exists: it is a non-blocking send that
				// returns when there is nothing to send to.
				return pages.NewApplier(s.nodeID, s.skills, s.nudgeSkills), nil
			},
			NewSeams: func(s *stateLog, runner *statelog.Runner) (writeSeams, error) {
				rows, err := pages.NewRows(s.db)
				if err != nil {
					return writeSeams{}, err
				}
				fence := pages.NewFence(s.db, s.nodeID)
				fence.Cursor = runner.Committed
				fence.Floor = s.trimFloor(pages.Domain{}.Name(),
					func() uint32 { return runner.Committed().Generation })
				return writeSeams{Rows: rows, Fence: fence,
					Gates: pages.NewGates(s.db), Evicted: fence.Evicted}, nil
			},
			Barrier: pages.EncodeBarrier,
			Ceiling: func(stream config.Stream, free int64) domainCeiling {
				bytes, derived := stream.PagesMaxBytes(free)
				return domainCeiling{Bytes: bytes,
					Field: "stream.pages_log_max_bytes", Explicit: !derived}
			},
		},
	}
}

// checkRegister refuses a register whose entries are not complete, BEFORE
// anything is provisioned or started.
//
// Three of these were already boot failures, each raised at the moment its
// switch was consulted and naming only itself. Taken together and taken first,
// they say what is actually wrong: this build declares a domain it cannot run.
// The fourth, the barrier, was not a failure at all.
func checkRegister(entries []registration) error {
	seen := map[string]bool{}
	for i, entry := range entries {
		if entry.Domain == nil {
			return fmt.Errorf("engine: the state-log register's entry %d names no domain", i)
		}
		name := entry.Domain.Name()
		if seen[name] {
			return fmt.Errorf("engine: the state-log register declares domain %q twice, "+
				"so which applier, write authority and ceiling it runs would depend "+
				"on which entry a lookup reached first", name)
		}
		seen[name] = true
		if entry.NewApplier == nil {
			return fmt.Errorf("engine: the state-log register's entry for %q declares no "+
				"applier, so its records would be consumed and produce no rows on "+
				"this node", name)
		}
		if entry.NewSeams == nil {
			return fmt.Errorf("engine: the state-log register's entry for %q declares no "+
				"write authority, so nothing could ever append to its log", name)
		}
		if entry.Ceiling == nil {
			return fmt.Errorf("engine: the state-log register's entry for %q declares no "+
				"Tier A ceiling for its stream, so it would reserve its own default "+
				"outside the budget every other state log is sized into", name)
		}
		if (entry.Barrier == nil) == !entry.NoBarrier {
			return fmt.Errorf("engine: the state-log register's entry for %q must state "+
				"either a barrier encoder or NoBarrier, and states %s — a domain "+
				"that grants no linearizable read says so, because an entry that "+
				"merely omits the encoder is indistinguishable from one that "+
				"forgot it", name, statedBarrier(entry))
		}
	}
	if len(entries) == 0 {
		return errors.New("engine: the state-log register is empty, so this node would " +
			"provision no log, apply no record and serve no domain's rows")
	}
	return nil
}

// statedBarrier is what an entry said about its barrier, for the refusal.
func statedBarrier(entry registration) string {
	if entry.Barrier != nil {
		return "both"
	}
	return "neither"
}

// registrationFor is one domain's entry.
//
// It cannot fail on an unknown name where the caller walked the register to
// get there, which every caller does; the boolean is for the one that did not.
func registrationFor(name string) (registration, bool) {
	for _, entry := range register() {
		if entry.Domain.Name() == name {
			return entry, true
		}
	}
	return registration{}, false
}

// registeredDomains is every domain this build runs, in the register's order.
//
// Kept as a derivation rather than folded into every caller, because most of
// them want exactly this — the list — and reading it off the table is what
// makes the table the single place a domain is declared.
func registeredDomains() []statelog.Domain {
	entries := register()
	domains := make([]statelog.Domain, 0, len(entries))
	for _, entry := range entries {
		domains = append(domains, entry.Domain)
	}
	return domains
}
