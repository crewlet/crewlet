package queries

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/learning"
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
// and one that only looks like it does. A third-party app's webhook route verifies and
// stores its deliveries as soon as its block is present; whether one then
// wakes a seat needs a parser, and four third-party apps have the first half and not
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
	// ONE READ for the whole answer. Every row asks the same question of
	// the same coordination bucket, and a per-row read would put seven
	// round trips on a screen refresh.
	//
	// Nil when this process cannot say, which a standalone API honestly
	// is, and which is NOT the same claim as "nothing has been
	// reconciled". A row then carries a null reconcile rather than one
	// asserting that nobody has ever checked.
	reconciled, reconcileKnown := s.reconcileStates(ctx)

	// Nil when no engine is co-located; an empty-but-non-nil list is a real
	// answer meaning nothing routes, so the two must not collapse.
	var routed []string
	known := s.Routed != nil
	if known {
		routed = s.Routed(ctx)
		known = routed != nil
	}
	// The same shape for what could VERIFY a delivery, and read the same
	// way: nil is "cannot say", empty is "nothing here can verify".
	var verifiable []string
	verifiableKnown := s.Verifiable != nil
	if verifiableKnown {
		verifiable = s.Verifiable(ctx)
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
			"skipped":   countOrNil(seen.skipped, kind),
			"coalesced": countOrNil(seen.coalesced, kind),
		}
		// WHERE A DELIVERY ARRIVES, and null where none ever does.
		//
		// The default used to be `webhook` at `/webhooks/<kind>`, which is
		// the route for five of the eight and a fabrication for the rest.
		// Atlassian is the one it fabricated: the organization receives
		// nothing at all, so the row advertised `/webhooks/atlassian` — a
		// path webhooks.go does not register and never has — for an
		// operator to check their settings page against. See
		// [integration.Kind.Ingests].
		row["inbound_kind"], row["inbound_path"] = nil, nil
		if integration.Kind(kind).Ingests() {
			row["inbound_kind"] = map[bool]string{
				true: "websocket", false: "webhook"}[kind == "mattermost"]
			row["inbound_path"] = inboundPath(kind)
		}
		// Rendered as a relative time, so an absent one has to be absent
		// rather than the zero instant — which would print as 1970.
		if at, ok := seen.last[kind]; ok {
			row["last_at"] = at.UTC().Format(time.RFC3339)
		} else {
			row["last_at"] = nil
		}
		if sources := deliversAs(kind); known && len(sources) > 0 {
			row["routes"] = slices.ContainsFunc(sources, func(source string) bool {
				return slices.Contains(routed, source)
			})
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
		// present, the third-party app's settings page shows a healthy hook, and
		// every delivery is refused with nothing anywhere naming the
		// variable. Null when this process cannot say — a standalone API —
		// or when the surface has no secret to resolve, exactly as above.
		switch {
		case secret == nil || !verifiableKnown:
			row["secret_usable"] = nil
		default:
			row["secret_usable"] = boolPtr(slices.Contains(verifiable, kind))
		}
		// THREE-VALUED again, and the third value is the one that took a
		// subsystem to be able to say at all: null means nothing is
		// checking this surface from here, an absent entry means the loop
		// has not reached it yet, and a present one is a real finding.
		// Before the reconcile loop existed this answer had no honest
		// form, which is why the doc comment above still says it never
		// infers health: it does not, and now it does not have to.
		//
		// AND A ROW IS NOT A REPORT. The setup write stamps an address on
		// a surface no pass converges, which leaves a row with no phase in
		// it; rendered as a report, that empty phase became the card's
		// status and drew Slack with no state at all while its address had
		// moved. [integration.State.Observed] is the test.
		if !reconcileKnown {
			row["reconcile"] = nil
		} else if state, checked := reconciled[kind]; checked && state.Observed() {
			row["reconcile"] = reconcileRow(state)
		} else {
			row["reconcile"] = nil
		}
		// WHETHER THIS SURFACE'S REGISTRATION STILL POINTS HERE.
		//
		// A company's public base moves, and a registration made against
		// the old one keeps pointing at an address that no longer answers.
		// Where a pass registers the hook, the next tick moves it and this
		// is true again within a tick; where nothing does, it stays false
		// until a person goes and changes it at the third-party app, which
		// is the whole reason the field exists.
		//
		// THREE-VALUED like the rest: null is "nothing has recorded an
		// address for this surface", which is not the same claim as "the
		// address moved". A row that has never been set up says null, and
		// a screen must not report that as a fault.
		//
		// COMPARED AGAINST THE RESOLVED BASE, which is why it reads
		// [Sources.PublicBase] rather than the document: the address a
		// surface registered is what a `${VAR}` public base resolved to,
		// and comparing that against the reference itself would report
		// every such company as moved for ever. A process that cannot
		// resolve says null rather than false, for the reason above.
		row["endpoint"], row["endpoint_current"] = nil, nil
		if state, checked := reconciled[kind]; checked && state.Endpoint != "" {
			row["endpoint"] = state.Endpoint
			if s.PublicBase != nil {
				row["endpoint_current"] = state.Endpoint == s.PublicBase()
			}
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
	if seats := seatSecrets(company, "slack"); in.Slack != nil || seats > 0 {
		add("slack", in.Slack != nil, boolPtr(seats > 0), nil)
	}
	if in.Mattermost != nil {
		add("mattermost", true, nil, map[string]any{"url": in.Mattermost.URL})
	}
	// GitHub, on the same terms as Slack and for the same reason: a
	// per-agent app is an app of its own, with its own signing secret, and
	// a company can hold nothing but those. Reported on the org block
	// alone, such a company had no GitHub row at all while five agents
	// were receiving deliveries.
	if seats := seatSecrets(company, "github"); in.GitHub != nil || seats > 0 {
		org := in.GitHub != nil
		add("github", org && in.GitHub.Enabled,
			boolPtr(seats > 0 || (org && in.GitHub.WebhookSecret != "")), nil)
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
		// TWO ROUTES, so the row names both. Data Center signs on
		// /webhooks/confluence; Cloud carries a token on
		// /webhooks/confluence/{event}, one hook per event. secret_present
		// answers for EITHER credential, because the question is whether a
		// delivery from this surface could be verified at all.
		add("confluence", true,
			boolPtr(in.Confluence.WebhookSecret != "" || in.Confluence.WebhookToken != ""),
			map[string]any{
				"url":        in.Confluence.BaseURL(),
				"cloud_path": "/webhooks/confluence/{event}",
			})
	}
	if in.Datadog != nil {
		// The one row whose secret is not a signing key, and the detail
		// says so rather than leaving a reader to assume the header is
		// verified like every other surface's. route_to is reported
		// because it is where an alert naming no owner goes, which is
		// the single most consequential thing about this integration
		// that is invisible from the traffic counts beside it.
		add("datadog", in.Datadog.Enabled, boolPtr(in.Datadog.WebhookToken != ""),
			map[string]any{
				"handle_tag": in.Datadog.HandleTagOrDefault(),
				"route_to":   in.Datadog.RouteTo,
			})
	}
	if in.Atlassian != nil {
		// THE ORGANIZATION, not a third product. Jira and Confluence are
		// sites this engine reads as an account; this is where the account
		// itself is created, which no site API can do — so it reconciles
		// identities, ingests nothing, and has no inbound address of its
		// own. The loop writes a status row for it like any other surface,
		// and without a row here that status was invisible: the one screen
		// an operator watches said nothing at all about whether their
		// agents' Atlassian accounts could still be created.
		// PRESENCE IS THE CONFIGURATION, the same as jira and confluence
		// beside it: the block carries no `enabled` switch, because an
		// organization key is either there to create accounts with or it
		// is not.
		// NO DELIVERY SECRET, AND NOTHING TO ROUTE. The organization key is
		// a PROVISIONING credential — it creates accounts — and `secret`
		// here means the value an inbound delivery is verified with, which
		// this surface has because it receives no delivery at all. Passed as
		// the key, the row reported `secret_usable: false` (nothing verifies
		// atlassian, because nothing needs to) and `routes: false` (no parser
		// routes it, because nothing arrives), which the dashboard drew as
		// "secret unresolved — every delivery is refused" and "routes
		// nowhere" over an organization whose key had just created every
		// agent's account. Mattermost, the other surface with no inbound
		// address, has passed nil here all along.
		add("atlassian", true, nil,
			map[string]any{
				"deployment": in.Atlassian.DeploymentOrDefault(),
				"org_id":     in.Atlassian.OrgID,
			})
	}
	if in.ForgeAppID != "" {
		// The app id is the JWT AUDIENCE rather than a secret — it is in
		// every manifest the operator installs — so its presence is the
		// whole configuration and there is no separate credential.
		add("forge", true, nil, map[string]any{"app_id": in.ForgeAppID})
	}
	slices.SortFunc(out, func(a, b map[string]any) int {
		return cmp.Compare(a["key"].(string), b["key"].(string))
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

// deliversAs names the sources a verified delivery at this surface arrives
// under. For six of the nine rows it is the surface itself, and the other
// three are the whole reason it exists rather than a `slices.Contains`.
//
// The question `routes` answers is whether a verified delivery would WAKE A
// SEAT, and only a registered parser can. A surface is therefore asked about
// the source its deliveries are published as, which is not always its own
// name.
func deliversAs(kind string) []string {
	switch {
	case kind == "forge":
		// AN INGRESS PATH, NOT A SOURCE. A Cloud event relayed by the
		// Forge app is republished as the PRODUCT it belongs to — jira or
		// confluence, from forgeEvents — and parsed by that product's
		// parser. So no parser is ever registered under "forge", and the
		// relay answered `routes: false` on every Cloud deployment for
		// ever. The dashboard groups it under the Atlassian row, so a
		// tenant whose relay was feeding both products correctly carried a
		// permanent "Forge relay — routes nowhere" beside them.
		return []string{"jira", "confluence"}
	case !integration.Kind(kind).Ingests():
		// Nothing arrives, so there is nothing to route and `false` —
		// "verified deliveries reach nobody" — is a fault report about a
		// question this surface is not asked. See [integration.Kind.Ingests].
		return nil
	default:
		return []string{kind}
	}
}

// inboundPath is where a third-party app's deliveries arrive, so an operator can check
// what they pasted into the third-party app's settings page against what this engine
// actually serves. Static per integration: these are the routes webhooks.go
// registers, and a disagreement between the two is a route nothing reaches.
//
// Asked only of a surface that INGESTS, which is what lets the default stand:
// a kind reaching it has a `/webhooks/<kind>` route unless it is named above.
func inboundPath(kind string) string {
	switch kind {
	case "mattermost":
		return "" // one outbound websocket per seat; nothing arrives here
	case "slack":
		return "/webhooks/slack/{handle}"
	case "github":
		// Both forms are served. The seat one is what a per-agent app
		// points at, and naming it here is what tells an operator the
		// path is meant to carry a handle.
		return "/webhooks/github/{handle}"
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
		// to line up with is the inbound count for one third-party app.
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

// integrationOf reads the third-party app an outcome event concerns.
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
//
// THE WALK IS company.EachRole, not a loop over company.Roles: that field is
// the seats belonging to NO unit, and a company whose agents all sit in units
// answered an empty list from every caller here. EachRole is exported for
// this exact reason, and its own doc records the first time a top-level-only
// lookup shipped.
func seatsFor(company *config.Company, kind string) []string {
	out := []string{}
	for r := range company.EachRole() {
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
		case "github":
			carries = r.Integrations.GitHub != nil
		}
		if carries {
			out = append(out, r.Name)
		}
	}
	slices.Sort(out)
	return out
}

// seatSecrets counts the per-seat apps of one kind that carry a verification
// credential.
//
// Counted as well as listed by [seatsFor]: the COUNT is what says whether the
// route can verify anything at all, since an app with no signing secret
// cannot, and an app without one is exactly the half-finished state an
// operator needs to see.
func seatSecrets(company *config.Company, kind string) int {
	n := 0
	for r := range company.EachRole() {
		var secret string
		switch kind {
		case "slack":
			if slack := r.Integrations.Slack; slack != nil {
				secret = slack.SigningSecret
			}
		case "github":
			// AND AN INSTALLATION, which is what makes the secret mean
			// anything.
			//
			// A seat app that is not installed on the organization sees no
			// repository, mints no token and receives no delivery — so
			// counting its signing secret reported a surface that is
			// receiving events on the strength of an app that reaches
			// nothing. It survived a disconnect for exactly that reason:
			// the org block went, every installation was removed at
			// GitHub, and the row stayed alive on two sealed values,
			// rendering the card as Connecting with a `routes nowhere`
			// badge permanently. The clause this counter exists for — a
			// company whose agents have their own apps and no org block —
			// is unaffected, because such a company installed them.
			if app := r.Integrations.GitHub; app != nil && app.InstallationID != 0 {
				secret = app.WebhookSecret
			}
		}
		if secret != "" {
			n++
		}
	}
	return n
}

func boolPtr(v bool) *bool { return &v }

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
	// EVERY key is present on every answer, as an empty list rather than an
	// absent one. A client cannot tell "this seat has learned nothing" from
	// "this node does not keep that half" if the key simply is not there, and
	// both are ordinary states.
	out := map[string]any{
		"id":             id,
		"diary":          []map[string]any{},
		"episodes":       []map[string]any{},
		"skills":         []map[string]any{},
		"counterparties": []map[string]any{},
		"onboarded_at":   "",
	}
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
		rows := make([]map[string]any, 0, len(entries))
		for _, e := range entries {
			rows = append(rows, diaryRow(e))
		}
		out["diary"] = rows
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
		rows := make([]map[string]any, 0, len(episodes))
		for _, e := range episodes {
			rows = append(rows, episodeRow(e))
		}
		out["episodes"] = rows
	}
	if s.Skills != nil {
		// The half that had no query at all. A seat drafts these from its
		// own repeated work, versions them, and loads them mid-turn — and
		// until now the operator paying for that could not see one.
		// The zero ListOptions is the operator's view as much as the
		// agent's: archived hidden, stale shown — a stale skill still
		// works and still revives on use, so hiding it would misreport
		// what the seat can actually load.
		skills, err := s.Skills.List(ctx, id, learning.ListOptions{})
		if err != nil {
			return nil, err
		}
		if len(skills) > MemoryPageLimit {
			skills = skills[:MemoryPageLimit]
		}
		rows := make([]map[string]any, 0, len(skills))
		for _, sk := range skills {
			rows = append(rows, skillRow(sk))
		}
		out["skills"] = rows
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

// reconcileStates reads what the loop last found, keyed by surface.
//
// The second result is whether this process could say at all, kept apart from
// an empty map for the same reason [Sources.Routed] keeps them apart: a
// standalone API has nothing to ask, and reporting that as "no surface has
// been reconciled" would put an alarming claim on a screen that had simply
// asked the wrong node.
func (s Sources) reconcileStates(ctx context.Context) (map[string]integration.State, bool) {
	if s.Reconciles == nil {
		return nil, false
	}
	states := s.Reconciles(ctx)
	if states == nil {
		return nil, false
	}
	out := make(map[string]integration.State, len(states))
	for _, state := range states {
		out[state.Kind.String()] = state
	}
	return out, true
}

// reconcileRow renders one surface's status for the wire.
//
// The FINDINGS travel as well as the report, because the two answer different
// questions: the report says what to do next, and the findings say what is
// actually wrong. A company with a broken webhook and four under-granted
// seats reports the webhook, and an operator who fixes it should not have to
// wait a full pass to discover there were four more things behind it.
func reconcileRow(state integration.State) map[string]any {
	// THE REPORT A READER SHOULD SEE, which is the stored one except in the
	// window between somebody pressing Disconnect and the first teardown
	// pass: the stored phase is then still whatever the last reconcile
	// concluded, and showing it reports a connected integration somebody
	// has already asked to remove.
	report := state.Reported()
	row := map[string]any{
		"phase": string(report.Phase),
		// The phase in a reader's words, decided HERE rather than on the
		// client. A screen that mapped six phase values to five labels
		// would be a second place that has to know what they mean, and it
		// could not label a phase a newer node wrote at all.
		"phase_label": report.Phase.Label(),
		"actor":       string(report.Actor),
		"detail":      report.Detail,
		"action_url":  report.ActionURL,
		// The INTENT, separately from the phase it produces. A reader
		// needs to know a disconnect was asked for even on a build whose
		// phase vocabulary it does not share.
		"disconnecting": state.TearingDown(),
		"outcome":       string(state.Outcome),
		"attempts":      state.Attempts,
		"last_error":    state.LastError,
		"findings":      reconcileFindings(state.Findings),
	}
	// Rendered as instants, so an absent one is absent rather than the
	// zero time, which prints as 1970 and reads as a real answer.
	row["last_attempt_at"] = instantOrNil(state.LastAttemptAt)
	row["settled_at"] = instantOrNil(state.SettledAt)
	row["next_attempt_at"] = instantOrNil(state.NextAttemptAt)
	return row
}

// reconcileFindings renders the findings list, never nil so a view does not
// have to read undefined.
func reconcileFindings(findings []integration.Finding) []map[string]any {
	out := make([]map[string]any, 0, len(findings))
	for _, f := range findings {
		// THE VERDICT TRAVELS WITH THE FINDING, from
		// [integration.FindingKind.Verdict] — the one table in the tree
		// that says what a kind MEANS.
		//
		// Without it a reader has only the kind string, and the only way to
		// tell a real problem from an advisory is to keep a second copy of
		// the closed set wherever the question is asked. The dashboard did
		// exactly that by accident: it treated any finding naming an agent
		// as that agent not working, which is wrong for the two kinds whose
		// verdict is PhaseReady — grant_excess and registration_orphaned
		// both describe something that is working — so one spare permission
		// on a GitHub app badged a healthy agent amber underneath a card
		// reading Connected.
		//
		// A kind this build does not know still gets a verdict here,
		// because Verdict's default arm answers for one: a peer on a newer
		// build can write a kind into the shared row, and rendering it as
		// an advisory would let it hide a real problem.
		phase, actor := f.Kind.Verdict()
		row := map[string]any{
			"kind":       string(f.Kind),
			"subject":    f.Subject,
			"detail":     f.Detail,
			"action_url": f.ActionURL,
			"phase":      string(phase),
			"actor":      string(actor),
		}
		// WHAT TO DO, SEPARATE FROM WHAT IS WRONG, so a card can lay the
		// two out rather than render one paragraph carrying both. Absent
		// where a surface honestly has no instruction to give.
		if f.Remedy != "" {
			row["remedy"] = f.Remedy
		}
		// THE WHOLE LIST, where a finding is about many things and its own
		// sentence gives only the count. See [integration.Finding.Subjects]:
		// the detail is the card's status line and is capped, so a finding
		// that listed thirty-six addresses inline was a wall cut off
		// mid-address. Absent rather than an empty array for the ordinary
		// finding about one thing, so a view can ask whether there is a
		// list at all.
		if len(f.Subjects) > 0 {
			row["subjects"] = f.Subjects
		}
		out = append(out, row)
	}
	return out
}

// instantOrNil renders a timestamp, or null for one that was never set.
func instantOrNil(at time.Time) any {
	if at.IsZero() {
		return nil
	}
	return at.UTC().Format(time.RFC3339)
}
