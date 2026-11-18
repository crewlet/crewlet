package worklet

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/configplane"
	"github.com/crewlet/crewlet/internal/statelog"
)

// ApplyChart makes the projects a company's org chart names EXIST and keeps
// their CHART-OWNED name, purpose and unit in step with it, as their only
// writer: a founder wrote them, so no tool may let a model rename them, and a
// project made on demand inside a create would let two nodes filing at once
// make two counters and two ENG-1s. What a project gathers in use (fields,
// default assignee, tags, archived flag) it never touches, and a project the
// chart stops naming is left, not removed: a renamed unit would otherwise take
// its tasks out of reach. activatedAt, the configuration's activation instant,
// is the epoch ([configplane.ActivationStamp]); zero is [ErrNoChartActivation].
// It attempts EVERY project, joining the failures, so one broker refusal
// cannot stall the rest, and returns those it wrote; an `unknown` outcome is
// an error, which the next apply retries.
func (w *Writer) ApplyChart(ctx context.Context, activatedAt time.Time,
	chart []ChartProject) ([]string, error) {

	if activatedAt.IsZero() {
		return nil, ErrNoChartActivation
	}
	epoch := configplane.ActivationStamp(activatedAt)
	var (
		wrote  []string
		failed []error
	)
	for _, p := range chart {
		if p.Key == "" {
			continue
		}
		changed, err := w.applyChartProject(ctx, epoch, p)
		if err != nil {
			failed = append(failed, fmt.Errorf("worklet: reconcile project %s "+
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

// ErrNoChartActivation refuses a chart apply that does not name the activation
// it came from. See [Writer.ApplyChart].
var ErrNoChartActivation = errors.New("worklet: a chart apply must name the " +
	"instant its configuration was activated")

// applyChartProject writes one project, or decides there is nothing to write.
//
// The epoch is the activation's, never the apply's clock: every node applies
// one activation separately, at each reconcile tick and boot, so a clock stamp
// would let a node booting a stale revision walk newer names back, and make
// every apply a record. Each project is its own subject, so racing nodes
// contend per project and the losers re-decide on the winner's rows. A row
// stamped ABOVE the epoch is left alone, and one AT it with the same values
// too. The stamp is compared BEFORE the fields: a matching row with an OLDER
// stamp is re-stamped, or a slow node applying an activation between the two
// late would walk it back. One record per project per activation, never per
// apply.
func (w *Writer) applyChartProject(ctx context.Context, epoch int64,
	p ChartProject) (bool, error) {

	subject := ProjectSubject(p.Key)
	scope := ScopeSet{Subject: true}
	now := w.Now()
	// THE WALL CLOCK for the mint, not w.Now (authored instants, pinned by
	// tests): the mint is compared with the ledger's watermark, which a
	// sweep and a join set off the wall.
	op := chartOpID(time.Now(), p.Key)
	var changed bool

	result, err := w.published(ctx, statelog.Request{
		Subject: wire(subject),
		Scope:   scope.Resolve(subject),
		OpID:    op,
		Pattern: statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			// RESET ON EVERY ROUND: a round that lost the broker's
			// arbitration is followed by one finding the winner's value,
			// and only the last says what this call did.
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
			// A LATER CHART ALREADY WON: during a rollout an older
			// node's reconcile arriving second must not walk it back.
			if held && current.ChartEpoch > epoch {
				return statelog.Decision{}, nil
			}
			if held && current.ChartEpoch == epoch &&
				next.Name == p.Name && next.Purpose == p.Purpose &&
				next.Unit == p.Unit {

				// NOTHING TO SAY: an empty decision is an outcome, not an
				// error ([statelog.Decision.Payload]).
				return statelog.Decision{}, nil
			}
			next.Name, next.Purpose, next.Unit = p.Name, p.Purpose, p.Unit
			next.ChartEpoch = epoch
			next.UpdatedAt = now
			changed = true

			verb, kind := OpPatch, ChangeProjectUpdated
			if !held {
				verb, kind = OpCreate, ChangeProjectCreated
			}
			decision, err := w.decide(ctx, tx, stamp, subject, verb, kind, scope,
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
		return false, fmt.Errorf("worklet: whether project %s's chart landed "+
			"is unknown (operation %s); the next apply decides it again", p.Key, op)
	}
	return changed, nil
}

// ApplyRoster reconciles the org chart's seat order ([Roster]) from every
// seat's name by handle, stamped with the activation the chart came from, and
// reports whether it wrote. A project's epoch rules hold, for the reasons on
// [Writer.applyChartProject].
//
// ONE RECORD FOR THE WHOLE CHART, because a place is relative: a seat added or
// renamed moves every place after it, which per-seat records would write with
// the order half moved in between.
func (w *Writer) ApplyRoster(ctx context.Context, activatedAt time.Time,
	names map[string]string) (bool, error) {

	if activatedAt.IsZero() {
		return false, ErrNoChartActivation
	}
	epoch := configplane.ActivationStamp(activatedAt)
	seats := seatsByName(names)
	subject := RosterSubject()
	scope := ScopeSet{Subject: true}
	now := w.Now()
	// THE WALL CLOCK for the mint, for [Writer.applyChartProject]'s reason.
	op := chartOpID(time.Now(), "roster")
	var changed bool

	result, err := w.published(ctx, statelog.Request{
		Subject: wire(subject),
		Scope:   scope.Resolve(subject),
		OpID:    op,
		Pattern: statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			changed = false
			current, held, err := readRoster(ctx, tx)
			if err != nil {
				return statelog.Decision{}, err
			}
			if held && (current.ChartEpoch > epoch ||
				current.ChartEpoch == epoch && slices.Equal(current.Seats, seats)) {
				return statelog.Decision{}, nil
			}
			changed = true
			decision, err := w.decide(ctx, tx, stamp, subject, OpPatch, "", scope, op,
				Roster{V: DocumentVersion, Seats: seats, ChartEpoch: epoch, UpdatedAt: now},
				nil, now)
			if err != nil {
				return statelog.Decision{}, err
			}
			decision.Version = int64(current.Version)
			return decision, nil
		},
	})
	if err != nil {
		return false, fmt.Errorf("worklet: reconcile the seats' order from the "+
			"org chart: %w", err)
	}
	if result.Outcome == statelog.OutcomeUnknown {
		return false, fmt.Errorf("worklet: whether the seats' order landed is "+
			"unknown (operation %s); the next apply decides it again", op)
	}
	return changed, nil
}

// chartOpID is the operation id one chart apply writes under: FRESH PER APPLY
// ([statelog.NewOpID]), because a reconcile decides from the rows what is left
// to write, so a repeat on any node finds its value and writes nothing, and
// racing nodes are settled by arbitration. None of that needs the ledger.
//
// An id DERIVED FROM THE ACTIVATION would put the ledger in charge: once the
// activation outlives the ledger's retention (a config unchanged for a month,
// as most are), every boot's apply would be answered `unknown` undecided, and
// a second apply would be answered from the first's row, so a project an older
// chart walked back at an equal epoch could never be set right.
func chartOpID(at time.Time, key string) string {
	return statelog.NewOpID(at, "chart-"+key)
}
