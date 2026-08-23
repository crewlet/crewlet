package queries

import (
	"context"
	"fmt"
	"sort"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/schedule"
	"github.com/crewlet/crewlet/internal/store"
)

// The questions answered from the epoch: what the company DECLARES, which is a
// different question from what it has done.

// RecentRunsLimit bounds the schedule history a listing carries.
//
// The dispatch ledger holds every fire inside its retention window, and the
// screen shows a recent-activity strip beside the schedule table. Fifty is a
// page of it; the ledger's own retention is what bounds the table.
const RecentRunsLimit = 50

// schedules answers the schedules question: what is configured, and what
// recently fired.
//
// The two halves come from different places on purpose. The configured
// schedules are a PROJECTION of the org — computed, never stored — so a
// schedule an operator just added shows immediately rather than after its
// first fire. The history is the dispatch ledger, which is the only thing that
// knows what actually happened.
func (s Sources) schedules(ctx context.Context, _ Params) (any, error) {
	company := s.Company()
	if company == nil {
		return map[string]any{"schedules": []any{}, "recent_runs": []any{}}, nil
	}
	organization, err := company.Organization()
	if err != nil {
		return nil, err
	}
	rows := schedule.Describe(organization, schedule.DescribeOptions{
		DefaultTimezone: company.Scheduling.DefaultTimezone,
		Now:             s.clock(),
	})
	if rows == nil {
		rows = []schedule.Row{}
	}
	return map[string]any{
		"schedules":   rows,
		"recent_runs": s.recentRuns(ctx),
	}, nil
}

// recentRuns is the dispatch history, or an empty list.
//
// DEGRADES rather than fails: the configured schedules are the half an
// operator opens this screen for, and refusing to show them because the
// history is unreadable would blank the page over its footnote.
func (s Sources) recentRuns(ctx context.Context) []map[string]any {
	if s.Runs == nil {
		return []map[string]any{}
	}
	runs, err := s.Runs.Recent(ctx, RecentRunsLimit)
	if err != nil {
		log.WarnContext(ctx, "schedule_history_unreadable", "error", err)
		return []map[string]any{}
	}
	out := make([]map[string]any, 0, len(runs))
	for _, run := range runs {
		out = append(out, map[string]any{
			"scope_type":    string(run.Scope),
			"scope_id":      run.ScopeID,
			"schedule_name": run.ScheduleName,
			"fire_label":    run.FireLabel,
			"target_handle": run.TargetHandle,
			"scheduled_at":  isoOrEmpty(run.ScheduledAt),
			"fired_at":      isoOrEmpty(run.FiredAt),
			"outcome":       string(run.Outcome),
			"trace_id":      run.TraceID,
		})
	}
	return out
}

// integrations answers how each external surface is wired, and what has come
// through it.
//
// IT NEVER INFERS HEALTH. An idle Slack and a 401-ing Slack are identical in
// the event store — verification runs before a row is written — so a green dot
// derived from "we saw traffic" would be reporting the weather. What this
// answers is what is configured, whether the credential this node needs is
// present, and how many deliveries arrived; a reader draws their own
// conclusion from those.
func (s Sources) integrations(ctx context.Context, _ Params) (any, error) {
	company := s.Company()
	if company == nil {
		return map[string]any{"integrations": []any{}}, nil
	}
	counts := s.deliveryCounts(ctx)
	in := company.Integrations

	out := []map[string]any{}
	add := func(kind string, configured bool, secret *bool, detail map[string]any) {
		row := map[string]any{
			"kind": kind, "configured": configured,
			"events": counts[kind],
		}
		// THREE-VALUED, and the third value is the point: null means this
		// surface uses no secret at all, false means a route is refusing
		// every delivery, and only an operator can tell those apart.
		row["secret_present"] = secret
		for key, value := range detail {
			row[key] = value
		}
		out = append(out, row)
	}

	// Slack is reported when EITHER half is present, because they turn on
	// different things and an operator needs to see the half they forgot.
	// The org block is the TRANSPORT marker; the per-seat apps are what
	// the inbound route verifies with. A company with seat apps and no
	// block answers webhooks and sends nothing; one with the block and no
	// apps refuses every delivery.
	if seats := slackSeats(company); in.Slack != nil || seats > 0 {
		add("slack", in.Slack != nil, boolPtr(seats > 0), map[string]any{"seats": seats})
	}
	if in.Mattermost != nil {
		add("mattermost", true, nil, map[string]any{"url": in.Mattermost.URL})
	}
	if in.GitHub != nil {
		add("github", in.GitHub.Enabled, boolPtr(in.GitHub.WebhookSecret != ""), nil)
	}
	if in.GitLab != nil {
		add("gitlab", in.GitLab.Enabled, boolPtr(in.GitLab.SigningSecret != ""),
			map[string]any{"url": in.GitLab.URL})
	}
	if in.Plane != nil {
		add("plane", in.Plane.Enabled, boolPtr(in.Plane.WebhookSecret != ""),
			map[string]any{"url": in.Plane.URL, "workspace": in.Plane.Workspace})
	}
	if in.Jira != nil {
		add("jira", true, boolPtr(in.Jira.WebhookSecret != ""),
			map[string]any{"url": in.Jira.BaseURL()})
	}
	if in.Confluence != nil {
		add("confluence", true, boolPtr(in.Confluence.WebhookSecret != ""),
			map[string]any{"url": in.Confluence.BaseURL()})
	}
	if in.ForgeAppID != "" {
		// The app id is the JWT AUDIENCE rather than a secret — it is in
		// every manifest the operator installs — so its presence is the
		// whole configuration and there is no separate credential.
		add("forge", true, nil, map[string]any{"app_id": in.ForgeAppID})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i]["kind"].(string) < out[j]["kind"].(string)
	})
	return map[string]any{"integrations": out}, nil
}

// deliveryCounts is how many webhook rows each source wrote, grouped BY THE
// STORE rather than by a live counter.
//
// A live counter resets with the process, and the question an operator asks —
// "is anything arriving from GitLab" — is about the last day rather than since
// this pod started.
func (s Sources) deliveryCounts(ctx context.Context) map[string]int {
	if s.Events == nil {
		return nil
	}
	rows, err := s.Events.List(ctx, store.ListQuery{
		Category: "webhook", Limit: MaxEventPage,
	})
	if err != nil {
		log.WarnContext(ctx, "integration_counts_unreadable", "error", err)
		return nil
	}
	counts := map[string]int{}
	for _, row := range rows {
		counts[row.Source]++
	}
	return counts
}

// slackSeats counts the per-seat Slack apps this company configures.
//
// Counted rather than listed: the number is what says whether the route can
// verify anything at all, and the handles are already on the org surface.
func slackSeats(company *config.Company) int {
	n := 0
	for i := range company.Roles {
		if slack := company.Roles[i].Integrations.Slack; slack != nil && slack.SigningSecret != "" {
			n++
		}
	}
	return n
}

func boolPtr(v bool) *bool { return &v }

// conversations answers what a seat already said — the threads it holds
// context for, or one thread's entries.
//
// Served because a context source an operator cannot read is an invisible
// second memory. This one is engine-owned, so it can be shown instead.
func (s Sources) conversations(ctx context.Context, p Params) (any, error) {
	handle := p.String("handle")
	if handle == "" {
		return nil, fmt.Errorf("%w: conversations needs a handle", ErrBadParams)
	}
	if key := p.String("key"); key != "" {
		entries, err := s.Conversations.History(ctx, handle, key, ConversationEntryLimit)
		if err != nil {
			return nil, err
		}
		if entries == nil {
			entries = []ledger.Session{}
		}
		return map[string]any{
			"handle": handle, "conversation_key": key,
			"conversations": []any{}, "entries": entries,
		}, nil
	}
	threads, err := s.Conversations.Threads(ctx, handle, ConversationListLimit)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(threads))
	for _, thread := range threads {
		out = append(out, map[string]any{
			"conversation_key": thread.Key,
			"entries":          thread.Entries,
			"last_at":          isoOrEmpty(thread.LastAt),
		})
	}
	return map[string]any{
		"handle": handle, "conversations": out, "entries": []any{},
	}, nil
}

// The two conversation windows.
const (
	// ConversationListLimit bounds the threads one seat's page lists. A
	// seat working a busy chat surface accumulates a thread per channel,
	// and the page shows them as a sidebar.
	ConversationListLimit = 50

	// ConversationEntryLimit bounds one thread's entries. It matches the
	// window a turn itself is given, so what an operator reads is what the
	// next turn will read.
	ConversationEntryLimit = 50
)

// agentMemory answers a seat's memory: its diary and its episodes.
//
// Both halves, because they answer different questions. The diary is what this
// seat chose to remember; the episodes are what it did, summarised. A page
// showing one without the other reads as a seat with half a history.
func (s Sources) agentMemory(ctx context.Context, p Params) (any, error) {
	id := p.String("id")
	if id == "" {
		return nil, fmt.Errorf("%w: agent_memory needs an id", ErrBadParams)
	}
	out := map[string]any{"id": id, "diary": []any{}, "episodes": []any{}}
	now := s.clock()
	if s.Diary != nil {
		entries, err := s.Diary.Recent(ctx, id, now, MemoryPageLimit)
		if err != nil {
			return nil, err
		}
		if entries != nil {
			out["diary"] = entries
		}
	}
	if s.Episodes != nil {
		// Episodes are keyed by HANDLE and the diary by agent id. The
		// dashboard has one identifier for a seat, so both are asked with
		// it and the one that does not recognise it answers nothing —
		// which is correct rather than an error, and is what a seat with
		// no episodes yet looks like anyway.
		episodes, err := s.Episodes.Recent(ctx, id, MemoryPageLimit)
		if err != nil {
			return nil, err
		}
		if episodes != nil {
			out["episodes"] = episodes
		}
	}
	return out, nil
}

// MemoryPageLimit bounds each half of a seat's memory page.
const MemoryPageLimit = 50
