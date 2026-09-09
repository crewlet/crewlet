package tracker

import (
	"context"
	"database/sql"
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
// Everything a project accumulates in use: its field catalogue, its sprint
// policy and pointer, its default assignee, its tags, its archived flag. Those
// are the operator's and the tools', and a reconcile that rewrote them would
// undo a person's work every time somebody edited the config.
//
// Nor does it REMOVE a project the chart no longer names. A project holds the
// company's tasks, and a unit renamed in a config edit would otherwise take
// every task filed under it out of reach — a data loss with a typo as its
// trigger. What the chart stops naming simply stops being reconciled.
//
// # Idempotence, and why it is per project
//
// Each project is its own append on its own subject, so N nodes running this
// at once contend per project and exactly one wins each; the losers see the
// value they wanted already there and write nothing. `epoch` is the config
// revision this chart came from: a project already stamped at or above it is
// left alone, which is what makes a reconcile on every apply cost nothing
// after the first node has done it.
//
// It returns the projects it actually wrote, so a caller can log the change
// rather than the attempt.
func (w *Writer) ApplyChart(ctx context.Context, epoch int64, chart []ChartProject) ([]string, error) {
	var wrote []string
	for _, p := range chart {
		if p.Key == "" {
			continue
		}
		changed, err := w.applyChartProject(ctx, epoch, p)
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
func (w *Writer) applyChartProject(ctx context.Context, epoch int64,
	p ChartProject) (bool, error) {

	subject := ProjectSubject(p.Key)
	scope := ScopeSet{Subject: true}
	at := w.Now()
	var changed bool

	_, err := w.published(ctx, statelog.Request{
		Subject:  wire(subject),
		Scope:    scope.Resolve(subject),
		OpID:     chartOpID(epoch, p.Key),
		MintedAt: at,
		Pattern:  statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx) (statelog.Decision, error) {
			current, held, err := readProject(ctx, tx, p.Key)
			if err != nil {
				return statelog.Decision{}, err
			}
			next := current
			if !held {
				next = Project{
					V: DocumentVersion, Key: p.Key, CreatedAt: at,
				}
			}
			// A LATER CHART ALREADY WON. Two nodes applying two
			// revisions is ordinary during a rollout, and the newer
			// one must not be walked back by the older node's own
			// reconcile arriving second.
			if held && current.ChartEpoch > epoch {
				return statelog.Decision{}, nil
			}
			if held && current.ChartEpoch == epoch &&
				next.Name == p.Name && next.Purpose == p.Purpose &&
				next.Unit == p.Unit {

				// NOTHING TO SAY. An empty decision is a legitimate
				// outcome rather than an error — see
				// [statelog.Decision.Payload] — and it is what makes
				// a reconcile on every apply free after the first.
				return statelog.Decision{}, nil
			}
			next.Name, next.Purpose, next.Unit = p.Name, p.Purpose, p.Unit
			next.ChartEpoch = epoch
			next.UpdatedAt = at
			changed = true

			op := OpPatch
			if !held {
				op = OpCreate
			}
			decision, err := w.decide(subject, op, scope,
				chartOpID(epoch, p.Key), next, nil, at)
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
// DERIVED FROM THE EPOCH AND THE KEY, so every node applying one revision
// mints the same id for one project: the ledger then collapses the N-1 that
// lost the broker's arbitration into a no-op rather than leaving each node to
// discover it separately. It is deliberately NOT time-based — a reconcile runs
// on every apply and on every boot, and a fresh id per run would make each of
// those a new operation to be deduped by nothing.
func chartOpID(epoch int64, key string) string {
	return fmt.Sprintf("chart:%d:%s", epoch, key)
}

// ChartEpochOf is the epoch a config revision's chart apply stamps.
//
// THE REVISION'S OWN TIMESTAMP in Unix seconds rather than a counter, because
// the value has to be comparable across nodes that learned about the revision
// separately and monotone in the order revisions were activated. A counter
// would need a home, and the one home it could have — the coordination
// pointer's revision — is not a number every reader of a project row has.
func ChartEpochOf(activatedAt time.Time) int64 { return activatedAt.UTC().Unix() }
