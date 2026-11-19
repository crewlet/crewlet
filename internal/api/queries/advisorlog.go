package queries

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/advisordomain"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE ADVISOR'S FINDINGS AS ITS DUTY KEEPS THEM (ADR-0164): read off the
// advisor log, every finding with the evidence it rests on, whether a run has
// confirmed it lately, and its fix. Atlas entries are named only to a holder
// of the atlas app (ADR-0141): stored whole, cut at the read.

// AdvisorLog is the advisor log's reads, this node's copy or a data node's.
type AdvisorLog interface {
	Findings(ctx context.Context, f advisordomain.Filter, fresh statelog.Freshness) ([]advisordomain.FindingRow, string, error)
	NonClosed(ctx context.Context, fresh statelog.Freshness) ([]advisordomain.FindingRow, error)
	Finding(ctx context.Context, id string, fresh statelog.Freshness) (advisordomain.FindingRow, []advisordomain.HistoryRow, error)
	Episodes(ctx context.Context, key string, fresh statelog.Freshness) ([]advisordomain.FindingRow, error)
	LatestRun(ctx context.Context, fresh statelog.Freshness) (advisordomain.RunRow, bool, error)
	PendingRequests(ctx context.Context, fresh statelog.Freshness) ([]advisordomain.RunRow, error)
	LastRunOfClasses(ctx context.Context, fresh statelog.Freshness) (map[string]time.Time, error)
}

// advisorFresh is what every advisor read reads at: this node's copy, which a
// person's own decision was written through.
var advisorFresh = statelog.Freshness{Level: statelog.ReadSession}

// AdvisorSubject is what a finding is about: its kind, its id within the kind
// and what a person calls it.
type AdvisorSubject struct {
	Kind advisor.SubjectKind `json:"kind"`
	ID   string              `json:"id"`
	Name string              `json:"name"`
	// Seat, Unit and Schedule name a seat's or a schedule's parts, as a fix's page reads them.
	Seat     string `json:"seat"`
	Unit     string `json:"unit"`
	Schedule string `json:"schedule"`
}

// AdvisorFact is one number a finding's evidence states.
type AdvisorFact struct {
	Key   string       `json:"key"`
	Label string       `json:"label"`
	Value int64        `json:"value"`
	Unit  advisor.Unit `json:"unit"`
}

// AdvisorRecordRef is one record behind a finding, named where the read can name it.
type AdvisorRecordRef struct {
	Kind advisor.RecordKind `json:"kind"`
	ID   string             `json:"id"`
	Name *string            `json:"name"`
}

// AdvisorEvidence is what a finding rests on, and the span it was read over.
type AdvisorEvidence struct {
	Window  advisor.Span       `json:"window"`
	Facts   []AdvisorFact      `json:"facts"`
	Records []AdvisorRecordRef `json:"records"`
}

// AdvisorImpact is what a finding costs a week, where its class can measure it.
type AdvisorImpact struct {
	HoursPerWeek  *float64 `json:"hours_per_week"`
	TokensPerWeek *int64   `json:"tokens_per_week"`
}

// AdvisorIneffective is a fix that was applied and did not clear the finding.
type AdvisorIneffective struct {
	FixDigest string    `json:"fix_digest"`
	AppliedAt time.Time `json:"applied_at"`
}

// Whether a run has confirmed a live finding lately.
const (
	ConfirmedFresh = "fresh"
	ConfirmedStale = "stale"
)

// AdvisorFinding is one finding as a person reads it.
type AdvisorFinding struct {
	ID       string               `json:"id"`
	Key      string               `json:"key"`
	Class    string               `json:"class"`
	Source   advisordomain.Source `json:"source"`
	Actor    advisor.Actor        `json:"actor"`
	Area     advisor.Area         `json:"area"`
	Severity advisor.Severity     `json:"severity"`
	Subject  AdvisorSubject       `json:"subject"`
	Title    string               `json:"title"`
	Why      string               `json:"why"`
	Item     string               `json:"item"`
	Names    []string             `json:"names"`
	Path     string               `json:"path"`
	// Entries is what an atlas fix acts on, for an asker who holds the atlas
	// app; null for any other fix or asker (ADR-0141).
	Entries         []advisor.AtlasEntry `json:"entries"`
	Explanation     *string              `json:"explanation"`
	State           advisor.State        `json:"state"`
	CloseReason     *advisor.CloseReason `json:"close_reason"`
	Ineffective     *AdvisorIneffective  `json:"ineffective"`
	SnoozedUntil    *time.Time           `json:"snoozed_until"`
	OpenedAt        time.Time            `json:"opened_at"`
	LastConfirmedAt time.Time            `json:"last_confirmed_at"`
	ClosedAt        *time.Time           `json:"closed_at"`
	EffectDueAt     *time.Time           `json:"effect_due_at"`
	Confirmed       string               `json:"confirmed"`
	Revision        int                  `json:"revision"`
	SeenBefore      int                  `json:"seen_before"`
	Impact          *AdvisorImpact       `json:"impact"`
	Evidence        AdvisorEvidence      `json:"evidence"`
	Fix             advisor.Fix          `json:"fix"`
}

// findingView is how one read shows findings: at now, against when each scope
// was last concluded, and for whether its asker holds the atlas app.
type findingView struct {
	now      time.Time
	lastDone map[string]time.Time
	atlas    bool
	// entries is each atlas finding's entries by key, as the asker reads the
	// atlas, read once a read first needs them; nil for an asker without the app.
	entries func() map[string][]advisor.AtlasEntry
}

// findingView is the read's view, for the question's asker.
func (s Sources) findingView(ctx context.Context, what string) (findingView, error) {
	principal, err := caller(ctx, what)
	if err != nil {
		return findingView{}, err
	}
	lastDone, err := s.Advisor.LastRunOfClasses(ctx, advisorFresh)
	if err != nil {
		return findingView{}, advisorUnreadable(err)
	}
	now := s.clock()
	v := findingView{now: now, lastDone: lastDone, atlas: principal.Can(iam.GrantAtlas)}
	if v.atlas {
		v.entries = sync.OnceValue(func() map[string][]advisor.AtlasEntry {
			return s.atlasEntriesFor(ctx, principal, now)
		})
	}
	return v, nil
}

// advisorUnreadable is a read of the advisor log that failed, said as this node's condition.
func advisorUnreadable(err error) error {
	return fmt.Errorf("%w: the advisor's findings could not be read: %w", ErrUnavailable, err)
}

// finding is row as v shows it.
func (v findingView) finding(row advisordomain.FindingRow) (AdvisorFinding, error) {
	var o advisor.Observation
	if err := json.Unmarshal(row.Body, &o); err != nil {
		return AdvisorFinding{}, fmt.Errorf("queries: finding %s does not decode: %w", row.ID, err)
	}
	f := o.Finding
	if !v.atlas {
		f = advisor.Unnamed(f)
	}
	out := AdvisorFinding{ID: row.ID, Key: row.Key, Class: row.Class, Source: row.Source, Actor: f.Actor,
		Area: f.Area, Severity: f.Severity, Title: f.Title, Why: f.Why, Item: f.Item,
		Names: nonNil(f.Names), Path: f.Path, State: advisor.State(row.State),
		OpenedAt: row.OpenedAt, Revision: row.Revision, SeenBefore: row.SeenBefore, Fix: f.Fix,
		Subject: AdvisorSubject{Kind: f.Subject.Kind, ID: advisor.SubjectID(f.Subject), Name: f.Subject.Name,
			Seat: f.Subject.Seat, Unit: f.Subject.Unit, Schedule: f.Subject.Schedule},
		Evidence: AdvisorEvidence{Window: o.Window, Facts: []AdvisorFact{}, Records: []AdvisorRecordRef{}}}
	// THE ENTRIES AN ATLAS FIX ACTS ON, filled at the read for an asker who
	// holds the atlas app, from the atlas as they read it; null for any other.
	if f.Fix.Kind == advisor.FixAtlasAct && v.entries != nil {
		out.Entries = nonNil(v.entries()[row.Key])
	}
	for _, fact := range f.Evidence {
		out.Evidence.Facts = append(out.Evidence.Facts, AdvisorFact{Key: fact.Key, Label: advisor.FactLabel(fact.Key),
			Value: fact.Value, Unit: advisor.FactUnit(fact.Key)})
	}
	for _, r := range f.Records {
		out.Evidence.Records = append(out.Evidence.Records, AdvisorRecordRef{Kind: r.Kind, ID: r.ID})
	}
	if row.Explanation != "" && row.ExplainedDigest == row.VerdictDigest {
		out.Explanation = &row.Explanation
	}
	if row.ImpactMinutes != nil || row.ImpactTokens != nil {
		out.Impact = &AdvisorImpact{TokensPerWeek: row.ImpactTokens}
		if row.ImpactMinutes != nil {
			hours := float64(*row.ImpactMinutes) / 60
			out.Impact.HoursPerWeek = &hours
		}
	}
	if row.IneffectiveFix != "" {
		out.Ineffective = &AdvisorIneffective{FixDigest: row.IneffectiveFix, AppliedAt: row.AppliedAt}
	}
	out.SnoozedUntil = after(row.SnoozedUntil, v.now)
	out.EffectDueAt = instant(row.EffectDueAt)
	out.LastConfirmedAt, out.Confirmed = v.confirmed(row, f)
	if out.State.Closed() {
		reason := advisor.CloseReason(row.CloseReason)
		out.CloseReason, out.ClosedAt, out.Confirmed = &reason, instant(row.ClosedAt), ConfirmedFresh
	}
	return out, nil
}

// confirmed is when a run last confirmed a live finding, and whether that is
// recent enough for its group ([advisor.Stale]).
func (v findingView) confirmed(row advisordomain.FindingRow, f advisor.Finding) (time.Time, string) {
	at := latest(row.OpenedAt, row.RevisedAt, row.FactsAt)
	if done := v.lastDone[advisor.ScopeOf(f)]; done.After(at) {
		at = done
	}
	if advisor.Stale(advisor.GroupOf(f), at, v.now) {
		return at, ConfirmedStale
	}
	return at, ConfirmedFresh
}

func latest(ts ...time.Time) time.Time {
	var out time.Time
	for _, t := range ts {
		if t.After(out) {
			out = t
		}
	}
	return out
}

// instant is t, nil where it was never set.
func instant(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// after is t where it is still ahead of now, else nil.
func after(t, now time.Time) *time.Time {
	if !t.After(now) {
		return nil
	}
	return &t
}

// AdvisorFindingList is a page of findings, the live ones counted by severity,
// and the latest run.
type AdvisorFindingList struct {
	Findings   []AdvisorFinding         `json:"findings"`
	NextCursor *string                  `json:"next_cursor"`
	Counts     map[advisor.Severity]int `json:"counts"`
	LastRun    *AdvisorRunSummary       `json:"last_run"`
}

// The bounds of a page of findings.
const (
	maxFindingsPage     = 100
	defaultFindingsPage = 50
)

// advisorFindings answers `advisor_findings`: `state=open` (the default) is
// every live finding, the most costly first, then the most impact, then the
// longest standing; `state=closed` the closed ones, the newest first. Both
// narrow by `severity=`, `area=` and `class=`, a page of `limit=` at `cursor=`.
func (s Sources) advisorFindings(ctx context.Context, p Params) (any, error) {
	v, err := s.findingView(ctx, "advisor_findings")
	if err != nil {
		return nil, err
	}
	limit := defaultFindingsPage
	if raw := p.String("limit"); raw != "" {
		if limit, err = strconv.Atoi(raw); err != nil || limit < 1 || limit > maxFindingsPage {
			return nil, badParams("limit", raw, []string{"1 to " + strconv.Itoa(maxFindingsPage)})
		}
	}
	severity := advisor.Severity(p.String("severity"))
	if severity != "" && !slices.Contains([]advisor.Severity{advisor.SeverityBlocks, advisor.SeverityCosts,
		advisor.SeverityNote}, severity) {
		return nil, badParams("severity", string(severity), []string{"blocks", "costs", "note"})
	}
	out := AdvisorFindingList{Findings: []AdvisorFinding{}}
	live, err := s.Advisor.NonClosed(ctx, advisorFresh)
	if err != nil {
		return nil, advisorUnreadable(err)
	}
	out.Counts = countsOf(live, v.now)
	if out.LastRun, err = s.latestRun(ctx); err != nil {
		return nil, err
	}
	var rows []advisordomain.FindingRow
	var next string
	switch state := p.String("state"); state {
	case "", "open":
		rows, next, err = livePage(live, severity, p.String("area"), p.String("class"), p.String("cursor"), limit)
	case "closed":
		rows, next, err = s.Advisor.Findings(ctx, advisordomain.Filter{Closed: true,
			Severity: advisordomain.Severity(severity), Area: p.String("area"), Class: p.String("class"),
			Cursor: p.String("cursor"), Limit: limit}, advisorFresh)
		if err != nil && !errors.Is(err, advisordomain.ErrInvalid) {
			err = advisorUnreadable(err)
		}
	default:
		return nil, badParams("state", state, []string{"open", "closed"})
	}
	if errors.Is(err, advisordomain.ErrInvalid) {
		return nil, fmt.Errorf("%w: %s", ErrBadParams, advisordomain.Sentence(err))
	}
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		f, err := v.finding(row)
		if err != nil {
			return nil, err
		}
		out.Findings = append(out.Findings, f)
	}
	if next != "" {
		out.NextCursor = &next
	}
	return out, nil
}

// countsOf is the live findings a person is to look at, by severity: neither
// snoozed at now nor applied and waiting on its effect.
func countsOf(rows []advisordomain.FindingRow, now time.Time) map[advisor.Severity]int {
	out := map[advisor.Severity]int{advisor.SeverityBlocks: 0, advisor.SeverityCosts: 0, advisor.SeverityNote: 0}
	for _, r := range rows {
		if advisor.State(r.State) == advisor.StateOpen && !r.SnoozedUntil.After(now) {
			out[advisor.Severity(r.Severity)]++
		}
	}
	return out
}

// livePage is one page of the live findings, ordered the most costly first,
// then the most impact, then the longest standing. The cursor is the offset.
func livePage(rows []advisordomain.FindingRow, severity advisor.Severity, area, class, cursor string,
	limit int) ([]advisordomain.FindingRow, string, error) {

	offset := 0
	if cursor != "" {
		n, err := strconv.Atoi(cursor)
		if err != nil || n < 0 {
			return nil, "", fmt.Errorf("%w: cursor %q is not one a page answered", ErrBadParams, cursor)
		}
		offset = n
	}
	rows = slices.DeleteFunc(slices.Clone(rows), func(r advisordomain.FindingRow) bool {
		return (severity != "" && advisor.Severity(r.Severity) != severity) || (area != "" && r.Area != area) ||
			(class != "" && r.Class != class)
	})
	slices.SortStableFunc(rows, func(a, b advisordomain.FindingRow) int {
		return cmp.Or(cmp.Compare(severityRank(a.Severity), severityRank(b.Severity)),
			cmp.Compare(impactOf(b), impactOf(a)), a.OpenedAt.Compare(b.OpenedAt), cmp.Compare(a.ID, b.ID))
	})
	if offset >= len(rows) {
		return nil, "", nil
	}
	end := min(offset+limit, len(rows))
	next := ""
	if end < len(rows) {
		next = strconv.Itoa(end)
	}
	return rows[offset:end], next, nil
}

// severityRank orders severities the most costly first.
func severityRank(s advisordomain.Severity) int {
	return slices.Index([]advisordomain.Severity{advisordomain.Severity(advisor.SeverityBlocks),
		advisordomain.Severity(advisor.SeverityCosts), advisordomain.Severity(advisor.SeverityNote)}, s)
}

// impactOf is a finding's weekly hours, in minutes, for ordering.
func impactOf(r advisordomain.FindingRow) int64 {
	if r.ImpactMinutes == nil {
		return 0
	}
	return *r.ImpactMinutes
}

// AdvisorHistoryEntry is one line of a finding's history.
type AdvisorHistoryEntry struct {
	At    time.Time       `json:"at"`
	Event string          `json:"event"`
	By    AdvisorActor    `json:"by"`
	Moved []advisor.Moved `json:"moved"`
}

// AdvisorActor is who moved a finding: the engine, or a person by handle.
type AdvisorActor struct {
	Kind   string `json:"kind"`
	Handle string `json:"handle"`
}

// AdvisorEpisode is an earlier episode of the same key.
type AdvisorEpisode struct {
	ID          string              `json:"id"`
	OpenedAt    time.Time           `json:"opened_at"`
	ClosedAt    *time.Time          `json:"closed_at"`
	CloseReason advisor.CloseReason `json:"close_reason"`
}

// AdvisorFindingDetail is one finding with its history and earlier episodes.
type AdvisorFindingDetail struct {
	AdvisorFinding
	History  []AdvisorHistoryEntry `json:"history"`
	Episodes []AdvisorEpisode      `json:"episodes"`
}

// advisorFinding answers `advisor_finding`: the finding `id` names, with its
// history and the key's earlier episodes.
func (s Sources) advisorFinding(ctx context.Context, p Params) (any, error) {
	id := strings.TrimSpace(p.String("id"))
	v, err := s.findingView(ctx, "advisor_finding")
	if err != nil {
		return nil, err
	}
	row, history, err := s.Advisor.Finding(ctx, id, advisorFresh)
	switch {
	case errors.Is(err, advisordomain.ErrNotFound):
		return nil, &NotFound{Reason: "advisor_finding", Detail: "No finding of the Advisor's has the id " + id + "."}
	case err != nil:
		return nil, advisorUnreadable(err)
	}
	f, err := v.finding(row)
	if err != nil {
		return nil, err
	}
	out := AdvisorFindingDetail{AdvisorFinding: f, History: []AdvisorHistoryEntry{}, Episodes: []AdvisorEpisode{}}
	for _, h := range history {
		entry := AdvisorHistoryEntry{At: h.At, Event: string(h.Event), By: AdvisorActor{Kind: "engine"},
			Moved: []advisor.Moved{}}
		if h.ByKind == iam.ActorHuman {
			entry.By = AdvisorActor{Kind: "person", Handle: h.ByName}
		}
		if len(h.Moved) > 0 {
			if err := json.Unmarshal(h.Moved, &entry.Moved); err != nil {
				return nil, fmt.Errorf("queries: finding %s's history does not decode: %w", id, err)
			}
		}
		out.History = append(out.History, entry)
	}
	episodes, err := s.Advisor.Episodes(ctx, row.Key, advisorFresh)
	if err != nil {
		return nil, advisorUnreadable(err)
	}
	for _, e := range episodes {
		if e.ID != row.ID {
			out.Episodes = append(out.Episodes, AdvisorEpisode{ID: e.ID, OpenedAt: e.OpenedAt,
				ClosedAt: instant(e.ClosedAt), CloseReason: advisor.CloseReason(e.CloseReason)})
		}
	}
	return out, nil
}

// AdvisorFindingOf is the finding id names, as the caller ctx carries reads it:
// what a person's decision on it answers with.
func (s Sources) AdvisorFindingOf(ctx context.Context, id string) (AdvisorFinding, error) {
	v, err := s.findingView(ctx, "advisor_finding")
	if err != nil {
		return AdvisorFinding{}, err
	}
	row, _, err := s.Advisor.Finding(ctx, id, advisorFresh)
	if err != nil {
		return AdvisorFinding{}, err
	}
	return v.finding(row)
}
