package tracker

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// ApplyChart makes the projects a company's org chart names EXIST, and keeps
// their chart-owned fields in step with it.
//
// # Why a project is not created on demand
//
// A create is a SEQUENCE: it takes the next number from its project's own
// counter and then writes the task, and the counter is arbitrated on the
// project's subject. So a project has to be an object before anything can be
// filed in it — and "create it if it is missing, inside the create" is exactly
// the shape that produces two projects, two counters and two ENG-1s when two
// nodes file at once.
//
// Deriving it from the CHART instead settles that, and settles more than that:
// a project's name, purpose and owning unit are facts a founder wrote in the
// config, so a tool that could write them would let a model rename the
// company's projects. The record marks those three CHART-OWNED for that
// reason, and this is the only writer of them.
//
// # What it does not touch
//
// Everything a project accumulates in use: its field catalogue, its default
// assignee, its tags, its archived flag. Those
// are the operator's and the tools', and a reconcile that rewrote them would
// undo a person's work every time somebody edited the config.
//
// Nor does it REMOVE a project the chart no longer names. A project holds the
// company's tasks, and a unit renamed in a config edit would otherwise take
// every task filed under it out of reach — a data loss with a typo as its
// trigger. What the chart stops naming simply stops being reconciled.
//
// # Idempotence, and why a matching row is the thing that decides it
//
// Each project is its own append on its own subject, so N nodes running this
// at once contend per project and exactly one wins each; the losers re-decide
// on the rows the winner wrote, see the value they wanted already there and
// write nothing.
//
// WHAT MAKES A RECONCILE FREE IS THE ROW MATCHING, never the stamp. This runs
// on every apply, on every boot and on every chart write, and `at` moves on
// each of those — so a guard that rewrote a project whose three fields already
// said what the chart says would rewrite EVERY project, on EVERY node, every
// time. Which is what a stamp keyed on the applying node's own clock did: two
// nodes seconds apart minted two epochs, each higher than the other's, and the
// pair rewrote the whole catalogue back and forth for the life of the
// deployment, once per apply and once per boot. So the fields are compared
// FIRST and the position is consulted only when they differ.
//
// It returns the projects it actually wrote, so a caller can log the change
// rather than the attempt; a write whose outcome is `unknown` is an error,
// because the next apply is what retries it and the caller is the one that
// says so.
//
// EVERY PROJECT IS ATTEMPTED, and the error names every one that failed. One
// project's broker refusal must not leave the rest of a company unable to file
// anything, and the caller is a reconcile that runs again. The loop used to
// say so and return at the first failure, so a chart whose first project hit a
// full stream or an unknown outcome reconciled none of the projects after it —
// on every apply, for as long as that one kept failing.
func (w *Writer) ApplyChart(ctx context.Context, at int64, chart []ChartProject) ([]string, error) {
	var (
		wrote  []string
		failed []error
	)
	for _, p := range chart {
		if p.Key == "" {
			continue
		}
		changed, err := w.applyChartProject(ctx, at, p)
		if err != nil {
			failed = append(failed, fmt.Errorf("tracker: reconcile project %s "+
				"from the org chart: %w", p.Key, err))
			continue
		}
		if changed {
			wrote = append(wrote, p.Key)
		}
	}
	return wrote, errors.Join(failed...)
}

// ChartProject is one project as the org chart declares it — the three
// chart-owned fields and nothing else, so a caller cannot reach past them.
type ChartProject struct {
	Key     string
	Name    string
	Purpose string
	Unit    string
}

// applyChartProject writes one, or decides there is nothing to write.
func (w *Writer) applyChartProject(ctx context.Context, at int64,
	p ChartProject) (bool, error) {

	subject := ProjectSubject(p.Key)
	scope := ScopeSet{Subject: true}
	now := w.Now()
	// THE WALL CLOCK for the mint and not the writer's own, which is the
	// clock AUTHORED instants are stamped from and a test pins: the mint
	// is compared with the ledger's watermark, which a sweep and a join
	// set off the wall.
	op := chartOpID(time.Now(), p.Key)
	var changed bool

	result, err := w.published(ctx, statelog.Request{
		Subject: wire(subject),
		Scope:   scope.Resolve(subject),
		OpID:    op,
		Pattern: statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			// RESET ON EVERY ROUND. A round that decided to write and
			// lost the broker's arbitration is followed by one that
			// finds the winner's value already there, and only the
			// last round says what this call did.
			changed = false
			current, held, err := readProject(ctx, tx, p.Key)
			if err != nil {
				return statelog.Decision{}, err
			}
			next := current
			if !held {
				next = Project{
					V: DocumentVersion, Key: p.Key, CreatedAt: now,
				}
			}
			// THE ROW ALREADY SAYS IT, whatever position said so.
			//
			// An empty decision is a legitimate outcome rather than
			// an error — see [statelog.Decision.Payload] — and this
			// is the arm that makes a reconcile on every apply, every
			// boot and every chart write cost one local read. It is
			// deliberately BEFORE the guard below: a row that already
			// holds these three values has nothing to walk back, so
			// there is nothing for a position to arbitrate.
			if held && next.Name == p.Name && next.Purpose == p.Purpose &&
				next.Unit == p.Unit {

				return statelog.Decision{}, nil
			}
			// A LATER CHART ALREADY WON, and only now is that a
			// question. Two nodes at different applier cursors is
			// ordinary — one is simply behind — and the view the
			// behind node derives must not walk back what the ahead
			// one wrote. The positions are comparable because both
			// are packed positions on the SAME log.
			if held && current.ChartPosition > at {
				return statelog.Decision{}, nil
			}
			next.Name, next.Purpose, next.Unit = p.Name, p.Purpose, p.Unit
			next.ChartPosition = at
			next.UpdatedAt = now
			changed = true

			verb, kind := OpPatch, ChangeProjectUpdated
			if !held {
				verb, kind = OpCreate, ChangeProjectCreated
			}
			decision, err := w.decide(stamp, subject, verb, kind, scope,
				op, next, nil, now)
			if err != nil {
				return statelog.Decision{}, err
			}
			decision.Version = int64(current.Version)
			return decision, nil
		},
	})
	if err != nil {
		return false, err
	}
	if result.Outcome == statelog.OutcomeUnknown {
		return false, fmt.Errorf("tracker: whether project %s's chart landed "+
			"is unknown (operation %s); the next apply decides it again", p.Key, op)
	}
	return changed, nil
}

// chartOpID is the operation id one project's chart apply writes under.
//
// FRESH PER APPLY, minted at the apply's own instant through the state log's
// grammar ([statelog.NewOpID]), because a chart apply is a RECONCILE rather
// than an operation a retry has to be matched to: it decides from the
// project's rows what, if anything, is left to write, so a second apply of one
// chart state — on another node, at the next boot, after a lost
// acknowledgement — finds its value there and writes nothing, and N nodes
// racing one chart state are settled by the broker's arbitration with the
// losers re-deciding on the winner's rows. None of that needs the ledger.
//
// An id DERIVED FROM THE POSITION would put the ledger in charge instead, and
// the instant the ledger vouches by would be one the id does not carry: this
// used to be `chart:<position>:<key>`, which is outside the op-id grammar, so
// the ledger read it as minted at the zero instant and could vouch for it on
// no node whose ledger ever lost a row. And the ledger would answer a SECOND
// apply of one chart state from the first's row rather than re-reading the
// project, so a project a behind node walked back could never be set right by
// the chart that is actually current.
func chartOpID(at time.Time, key string) string {
	return statelog.NewOpID(at, "chart-"+key)
}
