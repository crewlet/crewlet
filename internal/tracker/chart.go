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
// # The epoch is the ACTIVATION's instant
//
// activatedAt is when the configuration this chart came from was activated —
// the instant on the fleet's activation pointer, which every node reads and a
// node keeps as its active revision's `activated_at` — and it is the project's
// epoch ([ChartEpochOf]). Never the apply's own clock: every node applies one
// activation separately, at its reconcile tick and again at every boot, so an
// instant read there differs on every one of them and both things the epoch is
// for stop working. A node that boots on a stale revision stamps NOW and walks
// the fleet's newer names back to its own old ones, and no second apply ever
// carries the epoch the first stamped, so every apply on every node is a
// record per project for a value nobody changed. The zero instant is refused
// ([ErrNoChartActivation]): there is no honest default, and a caller holding
// no activation has no chart to apply.
//
// # Idempotence, and why it is per project
//
// Each project is its own append on its own subject, so N nodes running this
// at once contend per project and exactly one wins each; the losers re-decide
// on the rows the winner wrote, see the value they wanted already there and
// write nothing. A project already stamped ABOVE the epoch is left alone, and
// one stamped AT it with the same three values is too — which is what makes a
// reconcile on every apply and every boot cost nothing after the first node
// has done it.
//
// It returns the projects it actually wrote, so a caller can log the change
// rather than the attempt; a write whose outcome is `unknown` is an error,
// because the next apply is what retries it and the caller is the one that
// says so.
func (w *Writer) ApplyChart(ctx context.Context, activatedAt time.Time,
	chart []ChartProject) ([]string, error) {

	if activatedAt.IsZero() {
		return nil, ErrNoChartActivation
	}
	epoch := ChartEpochOf(activatedAt)
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

// ErrNoChartActivation refuses a chart apply that does not name the activation
// it came from. See [Writer.ApplyChart].
var ErrNoChartActivation = errors.New("tracker: a chart apply must name the " +
	"instant its configuration was activated")

// applyChartProject writes one, or decides there is nothing to write.
func (w *Writer) applyChartProject(ctx context.Context, epoch int64,
	p ChartProject) (bool, error) {

	subject := ProjectSubject(p.Key)
	scope := ScopeSet{Subject: true}
	at := w.Now()
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

			verb, kind := OpPatch, ChangeProjectUpdated
			if !held {
				verb, kind = OpCreate, ChangeProjectCreated
			}
			decision, err := w.decide(stamp, subject, verb, kind, scope,
				op, next, nil, at)
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
// FRESH PER APPLY, minted at the apply's own instant, because a chart apply
// is a RECONCILE rather than an operation a retry has to be matched to: it
// decides from the project's rows what, if anything, is left to write, so a
// second apply of one activation — on another node, at the next boot, after a
// lost acknowledgement — finds its value there and writes nothing, and N nodes
// racing one activation are settled by the broker's arbitration with the
// losers re-deciding on the winner's rows. None of that needs the ledger.
//
// An id DERIVED FROM THE ACTIVATION would put the ledger in charge instead,
// and its instant would be the activation's. Once that is older than the
// ledger's retention — a company whose configuration has not changed for a
// month, which is most of them — the ledger can no longer vouch for it, and
// every boot of every node would have every project's apply answered
// `unknown` without being decided. And the ledger would answer a SECOND apply
// of one activation from the first's row rather than re-reading the project,
// so a project an older chart walked back at an equal epoch could never be
// set right by the activation that is actually current.
func chartOpID(at time.Time, key string) string {
	return statelog.NewOpID(at, "chart-"+key)
}

// ChartEpochOf is the epoch a chart applied from a configuration activated at
// activatedAt stamps: that instant in Unix MILLISECONDS.
//
// AN INSTANT rather than a counter, because the value has to survive the
// coordination store being recreated: the activation pointer's own revision is
// the one counter every node shares, and it restarts at 1 with a new store —
// after which every chart would be older than every project's stamp and none
// would ever be applied again. An instant carries on from where the last one
// was.
//
// MILLISECONDS and not seconds, because two activations inside one second are
// an ordinary thing for a script to do, and at a second's resolution they
// share an epoch: a node still applying the first can land after the second's
// record and walk its names back at an EQUAL epoch, which the guard lets
// through. Both sources of the instant keep at least that much — the pointer
// carries nanoseconds and the store microseconds — so the pointer's instant
// and a boot's read of the store agree on it.
func ChartEpochOf(activatedAt time.Time) int64 { return activatedAt.UTC().UnixMilli() }
