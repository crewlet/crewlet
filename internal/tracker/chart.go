package tracker

import (
	"context"
	"database/sql"
	"fmt"

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
// at once contend per project and exactly one wins each; the losers see the
// value they wanted already there and write nothing.
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
// rather than the attempt.
func (w *Writer) ApplyChart(ctx context.Context, at int64, chart []ChartProject) ([]string, error) {
	var wrote []string
	for _, p := range chart {
		if p.Key == "" {
			continue
		}
		changed, err := w.applyChartProject(ctx, at, p)
		if err != nil {
			// EVERY PROJECT IS ATTEMPTED. One project's broker refusal
			// must not leave the rest of a company unable to file
			// anything, and the caller is a reconcile that runs again.
			return wrote, fmt.Errorf("tracker: reconcile project %s from the "+
				"org chart: %w", p.Key, err)
		}
		if changed {
			wrote = append(wrote, p.Key)
		}
	}
	return wrote, nil
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
	var changed bool

	_, err := w.published(ctx, statelog.Request{
		Subject:  wire(subject),
		Scope:    scope.Resolve(subject),
		OpID:     chartOpID(at, p.Key),
		MintedAt: now,
		Pattern:  statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx) (statelog.Decision, error) {
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

			op, kind := OpPatch, ChangeProjectUpdated
			if !held {
				op, kind = OpCreate, ChangeProjectCreated
			}
			decision, err := w.decide(subject, op, kind, scope,
				chartOpID(at, p.Key), next, nil, now)
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
	return changed, nil
}

// chartOpID is the operation id one project's chart apply writes under.
//
// DERIVED FROM THE CHART POSITION AND THE KEY, so two nodes reconciling the
// SAME chart state mint the same id for one project and the ledger collapses
// the one that lost the broker's arbitration into a no-op. It is deliberately
// not time-based: a reconcile runs on every apply, on every boot and on every
// chart write, and a fresh id per run would make each of those a new operation
// deduped by nothing.
//
// Two nodes at DIFFERENT cursors mint different ids, which is correct and
// costs nothing: the one that is behind derives the same three field values
// from the rows it has, so its decide finds the row already saying them and
// returns an empty decision before any append. The ledger is the collapse for
// the identical-position case; the row comparison is the collapse for every
// other case.
func chartOpID(at int64, key string) string {
	return fmt.Sprintf("chart:%d:%s", at, key)
}
