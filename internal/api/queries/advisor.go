package queries

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlas/gather"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/notice"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tokens"
	"github.com/crewlet/crewlet/internal/usage"
)

// advisorRead is the rule detectors' findings at now, read off the redacted
// document and the week's records: what the duty observes beside the atlas,
// and what the insights sharer reads (ADR-0119). AN EMPTY LIST, not an error,
// before the company's first import: there is nothing to advise about yet.
func (s Sources) advisorRead(ctx context.Context, now time.Time) (advisor.Answer, error) {
	answer, _, err := s.advisorReading(ctx, now)
	return answer, err
}

// advisorReading is [Sources.advisorRead] and the document it was read off,
// nil before the first import.
func (s Sources) advisorReading(ctx context.Context, now time.Time) (advisor.Answer, *config.Company, error) {
	in, ev, company, err := s.advisorInputs(ctx, now)
	if err != nil || company == nil {
		return advisor.Answer{Findings: []advisor.Finding{}, Unread: []string{}}, nil, err
	}
	return advisor.ReadWithEvidence(in, ev), company, nil
}

// advisorInputs is what the rule detectors read at now: the redacted document
// at its revision, the integrations' last reconcile and the week's records,
// beside the document as it runs; a nil company before the first import, or on
// a node that serves no configuration.
func (s Sources) advisorInputs(ctx context.Context, now time.Time) (advisor.Inputs, advisor.Evidence,
	*config.Company, error) {

	if s.Config == nil {
		return advisor.Inputs{}, advisor.Evidence{}, nil, nil
	}
	company, revision, err := s.Config.FleetCompany(ctx)
	if errors.Is(err, configapi.ErrNoActiveRevision) {
		return advisor.Inputs{}, advisor.Evidence{}, nil, nil
	}
	if err != nil {
		return advisor.Inputs{}, advisor.Evidence{}, nil, revisionRefusal("advisor", err)
	}
	in := advisor.Inputs{Company: company.Redact(), Revision: revision}
	// THE INTEGRATIONS' LAST RECONCILE, from the read the integrations answer
	// makes; nil where this node cannot say, which the answer names as unread.
	if states, known := s.reconcileStates(ctx); known {
		in.Integrations = slices.SortedFunc(maps.Values(states), func(a, b integration.State) int {
			return cmp.Compare(a.Kind, b.Kind)
		})
	}
	return in, s.advisorEvidence(ctx, company, now), company, nil
}

// advisedOfAtlas is answer with what the company's atlas shows needs a
// person, read as the engine reads the atlas: every entry, Gitlet's every
// repository among them, since the duty keeps what is found for whoever reads
// it later. Never the insights sharer's: no entry of the atlas leaves the
// engine. A node that serves no atlas, or runs no company yet, has none.
func (s Sources) advisedOfAtlas(ctx context.Context, answer advisor.Answer, now time.Time) advisor.Answer {
	var reads gather.GitletReads
	why := ""
	if s.Code != nil {
		var err error
		if reads, err = s.Code.CodeReads(); err != nil {
			reads, why = nil, "Gitlet could not be read on this node right now: "+err.Error()
		}
	}
	// THE ENGINE READS GITLET AS AN UNBOUND CREDENTIAL does, every repository it declares.
	return s.atlasFindings(ctx, answer, now, reads, why, gitlet.Principal{Kind: gitlet.PrincipalOperator},
		atlas.Reader{Admin: true}, true)
}

// atlasEntriesFor is each atlas finding's entries by its key, as the asker ctx
// carries reads the atlas: only what they may be named (ADR-0141), and a
// repository only where they hold Gitlet (ADR-0110). Filled into a stored
// finding at its read, which keeps none.
func (s Sources) atlasEntriesFor(ctx context.Context, principal iam.Principal, now time.Time) map[string][]advisor.AtlasEntry {
	reads, why := s.atlasGitlet(principal)
	answer := s.atlasFindings(ctx, advisor.Answer{Findings: []advisor.Finding{}}, now, reads, why,
		codeAsker(principal), atlas.Reader{Admin: principal.Can(iam.GrantAdmin)}, principal.Can(iam.GrantAtlas))
	out := map[string][]advisor.AtlasEntry{}
	for _, f := range answer.Findings {
		if f.Source == advisor.SourceAtlas {
			out[advisor.Key(f)] = append([]advisor.AtlasEntry{}, f.Entries...)
		}
	}
	return out
}

// atlasFindings is answer with the atlas's findings, its view read through
// reads as asker and folded for reader, named where names says so.
func (s Sources) atlasFindings(ctx context.Context, answer advisor.Answer, now time.Time, reads gather.GitletReads,
	why string, asker gitlet.Principal, reader atlas.Reader, names bool) advisor.Answer {

	chart := s.organization()
	if s.Atlas == nil || chart == nil {
		return answer
	}
	ev := advisor.AtlasEvidence{Until: now, Names: names,
		Holds: func(owner atlas.Owner) bool {
			if owner.Kind == atlas.ExternalUnit {
				return chart.UnitByRef(owner.Name) != nil
			}
			return chart.Role(owner.Name) != nil
		}}
	read, running := s.Atlas.AtlasRead(ctx, reads, why, asker, atlasFresh, now)
	if running {
		ev.View = atlas.For(read.View, reader)
	} else {
		ev.ViewUnread = true
	}
	report, running, err := s.Atlas.AtlasSources(ctx, now)
	switch {
	case err != nil:
		log.WarnContext(ctx, "advisor_atlas_sources_unread", "error", err.Error())
		ev.SourcesUnread = true
	case !running:
		ev.SourcesUnread = true
	}
	ev.Interval = time.Duration(report.RefreshIntervalSeconds) * time.Second
	for _, source := range report.Sources {
		ev.Sources = append(ev.Sources, advisor.AtlasSource{Kind: string(source.Kind),
			Name: advisor.SurfaceName(source.Kind), LastCompleteAt: source.LastCompleteAt})
	}
	return advisor.WithAtlas(answer, ev)
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
	if ev.Runs, err = s.endedRuns(ctx, now); err != nil {
		log.WarnContext(ctx, "advisor_runs_unreadable", "error", err.Error())
		ev.RunsUnread = true
	}
	if ev.Stops, err = s.budgetStops(ctx, now); err != nil {
		log.WarnContext(ctx, "advisor_budget_stops_unreadable", "error", err.Error())
		ev.StopsUnread = true
	}
	if ev.Merges, err = s.uncheckedMerges(ctx, now); err != nil {
		log.WarnContext(ctx, "advisor_merges_unreadable", "error", err.Error())
		ev.MergesUnread = true
	}
	ev.Waiting = s.waitingWakes()
	if ev.Fires, err = s.scheduleFires(ctx, now); err != nil {
		log.WarnContext(ctx, "advisor_schedule_fires_unreadable", "error", err.Error())
		ev.FiresUnread = true
	}
	if ev.Episodes, err = s.alarmEpisodes(ctx, now); err != nil {
		log.WarnContext(ctx, "advisor_alarm_episodes_unreadable", "error", err.Error())
		ev.EpisodesUnread = true
	}
	return ev
}

// alarmEpisodes is every alarm episode on any node that stood in the window
// before now, as the advisor log keeps them.
func (s Sources) alarmEpisodes(ctx context.Context, now time.Time) ([]advisor.AlarmEpisode, error) {
	if s.Advisor == nil {
		return nil, errors.New("queries: this node reaches no advisor log to read the alarms' history from")
	}
	rows, err := s.Advisor.AlarmEpisodes(ctx, now.Add(-advisor.Window), advisorFresh)
	if err != nil {
		return nil, err
	}
	out := make([]advisor.AlarmEpisode, 0, len(rows))
	for _, r := range rows {
		out = append(out, advisor.AlarmEpisode{Node: r.Node, Kind: r.Kind, Instance: r.Instance,
			RaisedAt: r.RaisedAt, ClearedAt: r.ClearedAt})
	}
	return out, nil
}

// failedRunsPage is how many records one read of the week takes, and
// maxFailedRunPages how many reads it takes before it names the source read
// in part: a week holding more failed runs than that is not read as whole.
const (
	failedRunsPage    = 200
	maxFailedRunPages = 10
)

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
