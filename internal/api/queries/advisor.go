package queries

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/notice"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tokens"
	"github.com/crewlet/crewlet/internal/usage"
)

// advisorAnswer is the advisor's findings with the platform notices they and
// the fleet's alarms raise: what reaches the administrators' Inbox.
type advisorAnswer struct {
	advisor.Answer
	Notices []notice.Notice `json:"notices"`
}

// advisor answers `advisor`: every finding about the fleet's company at the
// revision it is, the revision each fix is sent under. One read of the fleet's
// pointer gives both, and the document is redacted before anything is read
// off it, so no change a fix carries can hold a value redaction hides.
//
// AN EMPTY LIST, not an error, when nothing is active: a deployment before
// its first import has nothing to advise about, and its alarms are still noticed.
func (s Sources) advisor(ctx context.Context, _ Params) (any, error) {
	now := s.clock()
	fleet, fleetKnown := s.fleetAlarms(ctx)
	held, heldKnown := s.heldConversations(ctx)
	answer, err := s.advisorRead(ctx, now)
	if err != nil {
		return nil, err
	}
	return noticedWith(answer, fleet, fleetKnown, held, heldKnown, now), nil
}

// advisorRead is the advisor's findings at now, typed: what `advisor` answers
// beside its notices, and what the insights sharer reads.
func (s Sources) advisorRead(ctx context.Context, now time.Time) (advisor.Answer, error) {
	company, revision, err := s.Config.FleetCompany(ctx)
	if errors.Is(err, configapi.ErrNoActiveRevision) {
		return advisor.Answer{Findings: []advisor.Finding{}, Unread: []string{}}, nil
	}
	if err != nil {
		return advisor.Answer{}, revisionRefusal("advisor", err)
	}
	in := advisor.Inputs{Company: company.Redact(), Revision: revision}
	// THE INTEGRATIONS' LAST RECONCILE, from the read the integrations answer
	// makes; nil where this node cannot say, which the answer names as unread.
	if states, known := s.reconcileStates(ctx); known {
		in.Integrations = slices.SortedFunc(maps.Values(states), func(a, b integration.State) int {
			return cmp.Compare(a.Kind, b.Kind)
		})
	}
	return advisor.ReadWithEvidence(in, s.advisorEvidence(ctx, company, now)), nil
}

// noticedWith is the answer with the notices its findings, the fleet's alarms
// and the held conversations raise. `alarms` is named unread once, whether this
// node's evaluation or the fleet's marks could not be read; `holds` where the
// held conversations could not be.
func noticedWith(a advisor.Answer, fleet []notice.NodeAlarms, fleetKnown bool, held []notice.Notice,
	heldKnown bool, now time.Time) advisorAnswer {

	if !fleetKnown && !slices.Contains(a.Unread, advisor.UnreadAlarms) {
		a.Unread = append(a.Unread, advisor.UnreadAlarms)
	}
	if !heldKnown {
		a.Unread = append(a.Unread, advisor.UnreadHolds)
	}
	return advisorAnswer{Answer: a, Notices: notice.Read(a.Findings, fleet, held, now)}
}

// heldConversations is a notice per conversation the engine holds, named by
// its seat's handle; false where the holds cannot be read.
func (s Sources) heldConversations(ctx context.Context) ([]notice.Notice, bool) {
	if s.Holds == nil {
		return nil, true
	}
	holds, err := s.Holds.ListConversationHolds(ctx)
	if err != nil {
		log.WarnContext(ctx, "advisor_conversation_holds_unread", "error", err.Error())
		return nil, false
	}
	handles := s.seatHandles()
	return notice.FromConversationHolds(holds, func(seat uuid.UUID) (string, bool) {
		handle, known := handles[seat]
		return handle, known
	}), true
}

// advisorEvidence is what the company did in the week before now. A SOURCE THAT
// FAILS IS NAMED AS UNREAD, never the whole answer lost: the document's
// findings and the integrations' need no tracker read.
func (s Sources) advisorEvidence(ctx context.Context, company *config.Company, now time.Time) advisor.Evidence {
	ev := advisor.Evidence{Until: now}
	window := advisor.PatternsWindow(now, company)
	window.Level = statelog.ReadStale
	work, err := s.Work.WorkPatterns(ctx, window)
	if err != nil {
		log.WarnContext(ctx, "advisor_work_patterns_unreadable", "error", err.Error())
		ev.WorkUnread = true
	}
	ev.Work = work
	if ev.Ledger, err = s.advisorLedger(ctx, now); err != nil {
		log.WarnContext(ctx, "advisor_turn_ledger_unreadable", "error", err.Error())
		ev.LedgerUnread = true
	}
	if s.AlarmsEvaluated == nil || !s.AlarmsEvaluated() {
		ev.AlarmsUnread = true
	} else {
		ev.Backup = backupEvidence(s.latestAlarms())
	}
	if ev.Paused, err = s.pausedSeats(ctx, company); err != nil {
		log.WarnContext(ctx, "advisor_seat_pauses_unreadable", "error", err.Error())
		ev.PausesUnread = true
	}
	if ev.MemoryKills, err = s.memoryKills(ctx, now); err != nil {
		log.WarnContext(ctx, "advisor_runs_unreadable", "error", err.Error())
		ev.RunsUnread = true
	}
	if ev.Fires, err = s.scheduleFires(ctx, now); err != nil {
		log.WarnContext(ctx, "advisor_schedule_fires_unreadable", "error", err.Error())
		ev.FiresUnread = true
	}
	return ev
}

// failedRunsPage is how many failed runs one read of the week takes, and
// maxFailedRunPages how many reads it takes before it names the source read
// in part: a week holding more failed runs than that is not read as whole.
const (
	failedRunsPage    = 200
	maxFailedRunPages = 10
)

// memoryKills is every executor run in the window before now the kernel stopped
// for memory, by its agent's handle, read a page at a time back to the window's
// start. A fleet that did not answer whole, or a week past the bound, is unread.
func (s Sources) memoryKills(ctx context.Context, now time.Time) ([]advisor.MemoryKill, error) {
	if s.Events == nil {
		return nil, errors.New("queries: this node reads no event log")
	}
	failed := true
	q := store.ListQuery{
		Type: types.ExecutorCollected{}.EventType(), Failed: &failed,
		Since: now.Add(-advisor.Window), Until: now, Limit: failedRunsPage, At: now,
	}
	var rows []store.EventRecord
	for page := 0; ; page++ {
		if page == maxFailedRunPages {
			return nil, fmt.Errorf("queries: the week holds more than %d failed runs",
				failedRunsPage*maxFailedRunPages)
		}
		listing, coverage, err := s.Events.List(ctx, q)
		if err != nil {
			return nil, fmt.Errorf("queries: list the week's failed runs: %w", err)
		}
		if !coverage.Complete {
			return nil, errors.New("queries: not every node answered for its runs")
		}
		rows = append(rows, listing.Rows...)
		if !listing.More || len(listing.Rows) == 0 {
			break
		}
		oldest := listing.Rows[len(listing.Rows)-1]
		q.Before = &store.Cursor{Time: oldest.Time, ID: oldest.ID}
	}
	chart := s.chartHandles()
	var out []advisor.MemoryKill
	for _, rec := range rows {
		var run types.ExecutorCollected
		seat, charted := chart[rec.AgentID]
		if !charted || json.Unmarshal(rec.Payload, &run) != nil ||
			run.ErrorKind != types.ExecutorErrorOutOfMemory {
			continue
		}
		out = append(out, advisor.MemoryKill{Seat: seat.handle, MemoryBytes: run.MemoryBytes})
	}
	return out, nil
}

// SeatPauseLister is the read of every seat's pause ([coord.SeatPauses]').
type SeatPauseLister interface {
	ListSeatPauses(ctx context.Context) ([]coord.SeatPause, error)
}

// pausedSeats is every agent seat of company the engine paused for a failure,
// named by handle: the pause record is keyed by the seat's agent id. A node with
// no pause store has none, since the engine pauses through that store alone.
func (s Sources) pausedSeats(ctx context.Context, company *config.Company) ([]advisor.PausedSeat, error) {
	var out []advisor.PausedSeat
	if s.Pauses == nil {
		return nil, nil
	}
	pauses, err := s.Pauses.ListSeatPauses(ctx)
	if err != nil {
		return nil, fmt.Errorf("queries: list the seats' pauses: %w", err)
	}
	failed := map[uuid.UUID]string{}
	for _, p := range pauses {
		if p.Failure != nil {
			failed[p.Seat] = p.Failure.Kind
		}
	}
	for role := range company.EachRole() {
		id, ok := org.DeriveAgentID(company.Name, role.Seat().Handle())
		if kind, paused := failed[id]; ok && role.Kind != org.KindHuman && paused {
			out = append(out, advisor.PausedSeat{Handle: role.Seat().Handle(), Name: role.Name, Failure: kind})
		}
	}
	return out, nil
}

// fleetAlarms is every live node's alarm marks as its last heartbeat carried
// them, from the nodes whose table has run; false where the fleet cannot be
// read, or a live node has not evaluated yet and may hold one unsaid.
func (s Sources) fleetAlarms(ctx context.Context) ([]notice.NodeAlarms, bool) {
	if s.Coord == nil {
		return nil, false
	}
	leases, err := s.Coord.ListLive(ctx, coord.ClassNode)
	if err != nil {
		log.WarnContext(ctx, "advisor_fleet_alarms_unread", "error", err.Error())
		return nil, false
	}
	var out []notice.NodeAlarms
	complete := true
	for _, lease := range leases {
		status, ok := coord.StatusFromMeta(lease.Meta)
		// A NODE SAYING NOTHING YET (no status, or none evaluated) may hold an
		// alarm unsaid, as a node just started does.
		if !ok || !status.AlarmsEvaluated {
			complete = false
			continue
		}
		out = append(out, notice.NodeAlarms{Node: nameIn(coord.ClassNode, lease.Resource), Marks: status.Alarms})
	}
	return out, complete
}

// backupEvidence is whether the latest evaluation holds the backup alarm, and
// over how many logs.
func backupEvidence(alarms []statelog.Alarm) advisor.Backup {
	for _, a := range alarms {
		if a.Kind != statelog.KindBackupAge {
			continue
		}
		logs := 0
		for _, d := range a.Domains {
			logs += len(d.Domains)
		}
		return advisor.Backup{Standing: true, Logs: logs}
	}
	return advisor.Backup{}
}

// AccessFixAnswer is `advisor_access`: the change that grants an agent one
// access it asked for, built from the fleet's company, never applied here.
type AccessFixAnswer struct {
	Seat     string            `json:"seat"`
	Access   advisor.AccessKey `json:"access"`
	Revision string            `json:"revision"`
	Fix      advisor.Fix       `json:"fix"`
}

// advisorAccess answers `advisor_access`: the grant change for `seat` and
// `access`, read off the redacted document. A key the catalogue does not
// name, a seat that holds it or cannot take it, or no such agent is refused
// in its own words.
func (s Sources) advisorAccess(ctx context.Context, p Params) (any, error) {
	seat, key := strings.TrimSpace(p.String("seat")), advisor.AccessKey(strings.TrimSpace(p.String("access")))
	if !key.Valid() {
		return nil, badParams("access", string(key), accessKeyNames())
	}
	company, revision, err := s.Config.FleetCompany(ctx)
	if err != nil {
		return nil, revisionRefusal("advisor_access", err)
	}
	fix, err := advisor.AccessFix(company.Redact(), seat, key)
	switch {
	case errors.Is(err, advisor.ErrAccessHeld):
		return nil, fmt.Errorf("%w: %s can already %s", ErrBadParams, seat, key.Does())
	case errors.Is(err, advisor.ErrAccessNotTakable):
		return nil, fmt.Errorf("%w: %s has nothing this access could be added to", ErrBadParams, seat)
	case err != nil:
		return nil, fmt.Errorf("%w: %s is not an agent seat of this company", ErrBadParams, seat)
	}
	return AccessFixAnswer{Seat: seat, Access: key, Revision: revision, Fix: fix}, nil
}

func accessKeyNames() []string {
	out := []string{}
	for _, k := range advisor.AccessKeys() {
		out = append(out, string(k))
	}
	return out
}

// advisorLedger is the turn ledger over [advisor.LedgerDays], each seat named
// by the chart's handle while the chart holds it.
func (s Sources) advisorLedger(ctx context.Context, now time.Time) ([]usage.TurnLedgerRow, error) {
	if s.Usage == nil {
		return nil, errors.New("queries: no usage half to read the turn ledger from")
	}
	window := tokens.LastDays(advisor.LedgerDays, now, s.zone())
	rows, err := s.Usage.TurnLedger(ctx, usage.SpendQuery{From: window.First.Label, To: window.Last.Label})
	if err != nil {
		return nil, err
	}
	chart := s.chartHandles()
	for i := range rows {
		if seat, ok := chart[rows[i].AgentID]; ok && seat.handle != "" {
			rows[i].Handle = seat.handle
		}
	}
	return rows, nil
}
