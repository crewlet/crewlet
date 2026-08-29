package queries

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

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
	return map[string]any{
		"schedules":   s.ConfiguredSchedules(),
		"recent_runs": s.recentRuns(ctx),
	}, nil
}

// ConfiguredSchedules is the schedule rows a company's org declares, with no
// store read at all.
//
// Exported because the dashboard's handshake snapshot carries them too, and
// the two must be ONE implementation: the screen renders its rows from the
// snapshot slice and fetches only the dispatch ledger through the question
// above, so a second derivation here would be a screen whose contents changed
// depending on which of the two arrived last.
//
// The ledger half stays out. It is a store read, and the snapshot is built
// without one — see stream.Service.Snapshot.
func (s Sources) ConfiguredSchedules() []schedule.Row {
	// The FUNC, not just its answer: a process with no company source at
	// all is the state a node is in before its first revision activates,
	// and every other config-derived surface guards it the same way.
	if s.Company == nil {
		return []schedule.Row{}
	}
	company := s.Company()
	if company == nil {
		return []schedule.Row{}
	}
	organization, err := company.Organization()
	if err != nil {
		// A company that will not resolve into an org is one no node is
		// running. Empty is the honest answer and the screen says so.
		return []schedule.Row{}
	}
	rows := schedule.Describe(organization, schedule.DescribeOptions{
		DefaultTimezone: company.Scheduling.DefaultTimezone,
		Now:             s.clock(),
	})
	if rows == nil {
		return []schedule.Row{}
	}
	return rows
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
//
// # Configured is not routed
//
// `routes` is the one thing here that is a property of the BUILD rather than
// of the config, and it is the difference between an integration that works
// and one that only looks like it does. A vendor's webhook route verifies and
// stores its deliveries as soon as its block is present; whether one then
// wakes a seat needs a parser, and four vendors have the first half and not
// the second. Without this field they render identically — configured, secret
// present, deliveries arriving — so a company whose tracker is ingesting
// hundreds of events that reach nobody looks exactly like one that is working.
//
// THREE-VALUED, like secret_present and for the same reason: null is "this
// process cannot say", which is what a standalone API honestly answers, and
// is not the same claim as false.
func (s Sources) integrations(ctx context.Context, _ Params) (any, error) {
	company := s.Company()
	if company == nil {
		return map[string]any{"integrations": []any{}}, nil
	}
	seen := s.deliveryTraffic(ctx)
	in := company.Integrations

	// Nil when no engine is co-located; an empty-but-non-nil list is a real
	// answer meaning nothing routes, so the two must not collapse.
	var routed []string
	known := s.Routed != nil
	if known {
		routed = s.Routed()
		known = routed != nil
	}
	// The same shape for what could VERIFY a delivery, and read the same
	// way: nil is "cannot say", empty is "nothing here can verify".
	var verifiable []string
	verifiableKnown := s.Verifiable != nil
	if verifiableKnown {
		verifiable = s.Verifiable()
		verifiableKnown = verifiable != nil
	}

	out := []map[string]any{}
	// CONFIGURED and ENABLED are different facts and the answer sends both.
	// A block present with `enabled: false` is a deliberate pause an operator
	// can see; an absent block is an integration nobody set up. Folding them
	// left a paused integration looking unconfigured, which is the state most
	// likely to be mistaken for a mistake.
	add := func(kind string, enabled bool, secret *bool, detail map[string]any) {
		row := map[string]any{
			"key":        kind,
			"configured": true,
			"enabled":    enabled,
			"inbound":    seen.count[kind],
			// THREE-VALUED like the secret fields, and for the same
			// reason: null means this node could not read the outcome
			// events, and reporting that as 0 would say every delivery
			// woke a seat on a node that cannot tell.
			"skipped":      countOrNil(seen.skipped, kind),
			"coalesced":    countOrNil(seen.coalesced, kind),
			"inbound_kind": map[bool]string{true: "websocket", false: "webhook"}[kind == "mattermost"],
			"inbound_path": inboundPath(kind),
		}
		// Rendered as a relative time, so an absent one has to be absent
		// rather than the zero instant — which would print as 1970.
		if at, ok := seen.last[kind]; ok {
			row["last_at"] = at.UTC().Format(time.RFC3339)
		} else {
			row["last_at"] = nil
		}
		if known {
			row["routes"] = slices.Contains(routed, kind)
		} else {
			row["routes"] = nil
		}
		// THREE-VALUED, and the third value is the point: null means this
		// surface uses no secret at all, false means a route is refusing
		// every delivery, and only an operator can tell those apart.
		//
		// CONFIGURED, which is a claim about the DOCUMENT. A secret lives
		// there as a ${VAR}, so this being true says an operator wrote one
		// down — not that the route has anything to verify with.
		row["secret_present"] = secret
		// Which is the question secret_usable answers, from what this
		// process actually RESOLVED. The gap between the two is invisible
		// from every other surface: an unset variable renders as a secret
		// present, the vendor's settings page shows a healthy hook, and
		// every delivery is refused with nothing anywhere naming the
		// variable. Null when this process cannot say — a standalone API —
		// or when the surface has no secret to resolve, exactly as above.
		switch {
		case secret == nil || !verifiableKnown:
			row["secret_usable"] = nil
		default:
			row["secret_usable"] = boolPtr(slices.Contains(verifiable, kind))
		}
		// Every row carries seats, so the view never reads undefined.
		// An empty list is a real answer — nobody holds credentials of
		// their own for this surface — and it is not the same as absent.
		row["seats"] = seatsFor(company, kind)
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
		add("slack", in.Slack != nil, boolPtr(seats > 0), nil)
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
		return out[i]["key"].(string) < out[j]["key"].(string)
	})
	body := map[string]any{
		"integrations": out,
		// Whether the counts above are a MEASUREMENT. Without this a
		// store that could not be read reports every integration at zero
		// inbound, which reads as "nothing is arriving" — the alarming
		// answer — when the truth is "nobody looked".
		"traffic_known": seen.known,
		// The oldest delivery counted, so a count means something. The
		// page is capped rather than time-bounded, so there is no fixed
		// window to name; null when nothing was counted.
		"traffic_since": nil,
	}
	if !seen.since.IsZero() {
		body["traffic_since"] = seen.since.UTC().Format(time.RFC3339)
	}
	return body, nil
}

// inboundPath is where a vendor's deliveries arrive, so an operator can check
// what they pasted into the vendor's settings page against what this engine
// actually serves. Static per vendor — these are the routes webhooks.go
// registers, and a disagreement between the two is a route nothing reaches.
func inboundPath(kind string) string {
	switch kind {
	case "mattermost":
		return "" // one outbound websocket per seat; nothing arrives here
	case "slack":
		return "/webhooks/slack/{handle}"
	case "forge":
		return "/webhooks/forge"
	default:
		return "/webhooks/" + kind
	}
}

// traffic is what the event store can say about inbound deliveries.
//
// Grouped BY THE STORE rather than by a live counter: a live counter resets
// with the process, and the question an operator asks — "is anything arriving
// from GitLab" — is about recent history rather than about this pod's uptime.
//
// `known` is the load-bearing field. A store that cannot be read and a store
// with nothing in it are opposite facts, and a count of 0 expresses both — so
// the counts are reported ONLY alongside a flag saying they were measured.
type traffic struct {
	known bool
	count map[string]int
	last  map[string]time.Time

	// skipped and coalesced are what BECAME of the deliveries, counted
	// from the engine's own events rather than from the inbound rows.
	//
	// The three numbers answer one question together and are useless
	// apart: "128 arrived, 30 the routing gate dropped, 2 merges" is a
	// working integration; "128 arrived" alone cannot tell that from an
	// integration whose every delivery reaches nobody. Both are counted
	// per INTEGRATION, from the notification_source the events carry, so
	// they line up with the inbound count beside them.
	skipped   map[string]int
	coalesced map[string]int

	// since is the timestamp of the OLDEST delivery counted, which is what
	// makes the counts mean something. The page is capped rather than time
	// bounded, so "42 inbound" alone could span an hour or a year; "42
	// since Tuesday" is a measurement. Zero when nothing was counted.
	since time.Time
}

func (s Sources) deliveryTraffic(ctx context.Context) traffic {
	if s.Events == nil {
		return traffic{}
	}
	rows, err := s.Events.List(ctx, store.ListQuery{
		Category: "webhook", Limit: MaxEventPage,
	})
	if err != nil {
		log.WarnContext(ctx, "integration_counts_unreadable", "error", err)
		return traffic{}
	}
	out := traffic{
		known: true, count: map[string]int{}, last: map[string]time.Time{},
		skipped: map[string]int{}, coalesced: map[string]int{},
	}
	for _, row := range rows {
		out.count[row.Source]++
		if row.Time.After(out.last[row.Source]) {
			out.last[row.Source] = row.Time
		}
		if out.since.IsZero() || row.Time.Before(out.since) {
			out.since = row.Time
		}
	}
	s.countOutcomes(ctx, &out)
	return out
}

// countOutcomes adds the two per-integration outcome counts.
//
// A SECOND QUERY, on the notification category, because the outcomes are
// engine events and the deliveries are edge rows: they live under different
// categories and no single listing holds both. Its failure leaves the
// outcome counts absent rather than zero — see [traffic] — because a zero
// that means "unreadable" is the number an operator would act on.
func (s Sources) countOutcomes(ctx context.Context, out *traffic) {
	rows, err := s.Events.List(ctx, store.ListQuery{
		Category: "notification", Limit: MaxEventPage,
	})
	if err != nil {
		log.WarnContext(ctx, "integration_outcomes_unreadable", "error", err)
		out.skipped, out.coalesced = nil, nil
		return
	}
	for _, row := range rows {
		// THE INTEGRATION, not the event's Source: the source of an
		// engine-published event names the engine, and what the row has
		// to line up with is the inbound count for one vendor.
		source := integrationOf(row)
		if source == "" {
			continue
		}
		switch row.Type {
		case "notification_skipped":
			out.skipped[source]++
		case "notifications_coalesced":
			out.coalesced[source]++
		}
	}
}

// integrationOf reads the vendor an outcome event concerns.
//
// FROM THE TAG, not the payload: a listing deliberately never selects the
// payload column, so the tag is all a historical row carries — see
// [store.RecordFor]. A row written before that tag existed carries none and
// is skipped rather than guessed at.
func integrationOf(row store.EventRecord) string {
	return strings.TrimSpace(row.Tags["notification_source"])
}

// seatsFor lists the handles that carry this surface in their own config.
//
// LISTED rather than counted, because the number alone answers a question
// nobody asks. "Which seats reach GitLab" is followed immediately by "which
// ones", and a count sends the reader to the org page to work it out — while
// the answer is already in the config this function is reading.
//
// What counts as carrying a surface is that seat's OWN field for it: a Slack
// app, a Mattermost identity, a per-seat project or space. A seat with none
// of them is served by the company-wide account, not by one of its own.
func seatsFor(company *config.Company, kind string) []string {
	out := []string{}
	for i := range company.Roles {
		r := &company.Roles[i]
		var carries bool
		switch kind {
		case "slack":
			carries = r.Integrations.Slack != nil
		case "mattermost":
			carries = r.Integrations.Mattermost != nil
		case "jira":
			carries = r.Integrations.Jira != nil
		case "confluence":
			carries = r.Integrations.Confluence != nil
		}
		if carries {
			out = append(out, r.Name)
		}
	}
	sort.Strings(out)
	return out
}

// slackSeats counts the per-seat Slack apps this company configures.
//
// Counted as well as listed: the COUNT is what says whether the route can
// verify anything at all, since a Slack app with no signing secret cannot.
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
// agentIDOf resolves a seat handle to the derived agent id the diary is keyed
// by, passing anything else through — a caller that already holds an id is
// unaffected.
func (s Sources) agentIDOf(handle string) string {
	if handle == "" || s.Company == nil {
		return handle
	}
	company := s.Company()
	if company == nil {
		return handle
	}
	organization, err := company.Organization()
	if err != nil {
		return handle
	}
	role := organization.AgentSeatByHandle(handle)
	if role == nil {
		return handle
	}
	if id, ok := organization.AgentIDFor(role); ok {
		return id.String()
	}
	return handle
}

func (s Sources) agentMemory(ctx context.Context, p Params) (any, error) {
	id := p.String("id")
	if id == "" {
		return nil, fmt.Errorf("%w: agent_memory needs an id", ErrBadParams)
	}
	out := map[string]any{"id": id, "diary": []any{}, "episodes": []any{}}
	now := s.clock()
	if s.Diary != nil {
		// RESOLVED, not passed through. The diary is keyed by the derived
		// AGENT ID and the dashboard's one identifier for a seat is its
		// handle, so handing the handle straight to the diary asked it
		// about a seat it has no rows for — and answered an empty memory
		// rather than the seat's, which reads identically to a seat that
		// has not learned anything yet.
		entries, err := s.Diary.Recent(ctx, s.agentIDOf(id), now, MemoryPageLimit)
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

// countOrNil renders an outcome count, or null when nothing was counted.
func countOrNil(counts map[string]int, kind string) any {
	if counts == nil {
		return nil
	}
	return counts[kind]
}
