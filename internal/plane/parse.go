// Package plane is the Plane integration: the work tracker agents are
// assigned from, and the page store they read shared knowledge out of.
package plane

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/notify"
)

var log = logging.Get("plane")

// Backend is the transport name.
const Backend = "plane"

// contentEvents are the only events that can name a recipient.
//
// Everything else a Plane workspace emits — a project created, a cycle
// closed, a module renamed, a member added — is bookkeeping about the
// workspace rather than a message to anybody, and routing it produces turns
// triaging "somebody made a cycle".
var contentEvents = []string{"issue", "issue_comment", "intake_issue", "page"}

// Routing reasons, in the order a target is considered. The FIRST reason for
// a given person wins, so a mentioned assignee is woken as a mention — which
// is the stronger claim on their attention and the one the prompt renders
// differently.
const (
	// ViaMention: named in a comment. A directed ask.
	ViaMention = "mention"
	// ViaAssignee: on the work item.
	ViaAssignee = "assignee"
	// ViaAssigneeAdded: just put on it, which is the moment it becomes
	// theirs.
	ViaAssigneeAdded = "assignee_added"
	// ViaSubscriber: following the thread.
	ViaSubscriber = "subscriber"
	// ViaLeadFallback: nobody was named, so the owning unit's lead gets
	// it. A new ticket must never vanish.
	ViaLeadFallback = "project_lead_fallback"
	// ViaIntake: Plane's unassigned-inbound surface. Triage is the lead's.
	ViaIntake = "intake_triage"
	// ViaPageLead: a page changed. CE pages have no subscription model
	// and no comments, so the lead is the only recipient there is.
	ViaPageLead = "page_project_lead"
)

// RoutedViaField carries the reason on a notification, so the prompt can say
// WHY this seat is being told — "you were mentioned" and "you are subscribed
// to this thread" ask for different things.
const RoutedViaField = "routed_via"

// Projects resolves a project's UUID to the identifier people use.
//
// A seam because the mapping needs the API and a cache, and the routing rules
// are worth testing without either. An unresolvable id yields "", which is
// FAIL CLOSED for every rule that reads it: a page whose project is unknown
// routes to nobody rather than to a guess.
type Projects interface {
	Identifier(ctx context.Context, projectID string) string
}

// Subscribers lists the users following a work item.
type Subscribers interface {
	Of(ctx context.Context, projectID, issueID string) ([]string, error)
}

// PageIndexer is notified of every page change, before any routing decision.
//
// The tool-skill registry is rebuilt from page content, and it cares about
// EVERY change — including a seat's own edit and a project whose
// notifications are excluded. Indexing must never be a casualty of a routing
// rule.
type PageIndexer func(ctx context.Context, eventType, pageID string) error

// ParserOptions configure a [Parser].
type ParserOptions struct {
	// URL is the workspace's base address, for building shareable links.
	URL string

	Projects    Projects
	Subscribers Subscribers

	// Leads maps a project IDENTIFIER to the handle of the unit lead who
	// owns it — the recipient when nobody was named.
	Leads map[string]string

	// Excluded are project identifiers whose webhooks must not produce
	// notifications at all.
	//
	// An engine-managed project — the one holding tool skills — has no
	// human or agent recipient by design, and without this its page edits
	// fall through to lead routing and surface as a stream of
	// undeliverable notifications. The page indexer still runs for them:
	// only routing stops.
	Excluded []string

	// OnPage re-indexes a changed page.
	OnPage PageIndexer
}

// Parser turns one Plane webhook into the notifications it implies.
type Parser struct {
	url      string
	projects Projects
	subs     Subscribers
	leads    map[string]string
	excluded map[string]bool
	onPage   PageIndexer
}

// NewParser builds the parser.
func NewParser(opts ParserOptions) (*Parser, error) {
	if strings.TrimSpace(opts.URL) == "" {
		return nil, fmt.Errorf("plane: the parser needs a workspace url")
	}
	p := &Parser{
		url:      strings.TrimRight(strings.TrimSpace(opts.URL), "/"),
		projects: opts.Projects,
		subs:     opts.Subscribers,
		leads:    make(map[string]string, len(opts.Leads)),
		excluded: make(map[string]bool, len(opts.Excluded)),
		onPage:   opts.OnPage,
	}
	// Identifiers are compared UPPER, because Plane accepts them in any
	// case and an operator writes them however they were shown.
	for identifier, lead := range opts.Leads {
		if identifier != "" && lead != "" {
			p.leads[strings.ToUpper(identifier)] = lead
		}
	}
	for _, identifier := range opts.Excluded {
		if identifier != "" {
			p.excluded[strings.ToUpper(identifier)] = true
		}
	}
	return p, nil
}

// Source implements [notify.Parser].
func (p *Parser) Source() string { return Backend }

// Parse implements [notify.Parser].
func (p *Parser) Parse(ctx context.Context, w types.RawWebhook, reg *notify.Registry) ([]notify.Routed, error) {
	event := str(w.Body, "event")
	action := str(w.Body, "action")
	if !slices.Contains(contentEvents, event) {
		return nil, nil
	}
	data, ok := w.Body["data"].(map[string]any)
	if !ok {
		return nil, nil
	}

	switch event {
	case "page":
		return p.routePage(ctx, w.Body, data, action, reg)
	case "issue":
		return p.routeIssue(ctx, w.Body, data, action, reg)
	case "issue_comment":
		return p.routeComment(ctx, w.Body, data, action, reg)
	default:
		return p.routeIntake(ctx, w.Body, data, reg)
	}
}

// routeIssue handles a work item's own lifecycle.
func (p *Parser) routeIssue(ctx context.Context, body, data map[string]any, action string, reg *notify.Registry) ([]notify.Routed, error) {
	activity := object(body, "activity")
	actor := str(activity, "actor_id")
	projectID := str(data, "project")
	identifier := p.identifier(ctx, projectID)
	base, ok := p.base(body, data, identifier, reg)
	if !ok {
		return nil, nil
	}
	issueID := base.Metadata["issue_id"]

	switch action {
	case "created":
		assignees := ids(data["assignees"])
		if len(assignees) == 0 {
			// The new-ticket-wakes-the-lead flow.
			return p.leadCopy(base, identifier, actor, ViaLeadFallback, reg), nil
		}
		copies := p.directed(base, targetsOf(assignees, ViaAssignee), actor, reg)
		if len(copies) == 0 && namesSomebodyElse(assignees, actor) {
			// Every non-actor assignee was somebody the engine cannot
			// route to — the ticket was handed entirely to people
			// outside the org chart. It still wakes the owning lead,
			// because a new ticket must never vanish. A purely
			// SELF-assigned create stays silent: the actor claimed
			// the work and knows they have it.
			return p.leadCopy(base, identifier, actor, ViaLeadFallback, reg), nil
		}
		return copies, nil

	case "updated":
		if str(activity, "field") == "assignees" {
			// Directed at the newly ADDED assignee, which is the
			// moment the work becomes theirs. A REMOVAL carries no
			// new identifier and wakes nobody: being taken off
			// something is not a task.
			// A REMOVAL carries no new identifier, and an empty target
			// wakes nobody — that rule lives in directed, once, rather
			// than being re-checked by each caller that could produce
			// an empty id.
			added := strings.ToLower(str(activity, "new_identifier"))
			return p.directed(base, targetsOf([]string{added}, ViaAssigneeAdded), actor, reg), nil
		}
		return p.directed(base, p.thread(ctx, projectID, issueID, data), actor, reg), nil

	case "deleted":
		// The subscribers endpoint cannot serve a deleted work item, so
		// this goes straight to the payload's assignees with no call.
		return p.directed(base, targetsOf(ids(data["assignees"]), ViaAssignee), actor, reg), nil
	}
	return nil, nil
}

// routeComment handles a comment on a work item.
func (p *Parser) routeComment(ctx context.Context, body, data map[string]any, action string, reg *notify.Registry) ([]notify.Routed, error) {
	// A DELETION carries no body, so routing it is pure noise: there is
	// nothing left for a seat to act on.
	if action != "created" && action != "updated" {
		return nil, nil
	}
	activity := object(body, "activity")
	actor := str(activity, "actor_id")
	projectID := str(data, "project")
	identifier := p.identifier(ctx, projectID)
	base, ok := p.base(body, data, identifier, reg)
	if !ok {
		return nil, nil
	}
	issueID := base.Metadata["issue_id"]

	// MENTIONS FIRST, because being named is a directed ask and the first
	// reason for a person wins — a mentioned subscriber should be told
	// they were mentioned, not that they are subscribed.
	targets := targetsOf(MentionIDs(str(data, "comment_html")), ViaMention)
	targets = append(targets, p.thread(ctx, projectID, issueID, data)...)
	return p.directed(base, targets, actor, reg), nil
}

// routeIntake handles Plane's unassigned-inbound surface.
//
// Triage is the lead's for any action: intake exists precisely because
// nobody has decided who owns the thing yet.
func (p *Parser) routeIntake(ctx context.Context, body, data map[string]any, reg *notify.Registry) ([]notify.Routed, error) {
	activity := object(body, "activity")
	identifier := p.identifier(ctx, str(data, "project"))
	base, ok := p.base(body, data, identifier, reg)
	if !ok {
		return nil, nil
	}
	return p.leadCopy(base, identifier, str(activity, "actor_id"), ViaIntake, reg), nil
}

// routePage handles a knowledge page.
func (p *Parser) routePage(ctx context.Context, body, data map[string]any, action string, reg *notify.Registry) ([]notify.Routed, error) {
	eventType := "page"
	if action != "" {
		eventType = "page." + action
	}
	pageID := str(data, "id")

	// THE INDEXER ALWAYS RUNS FIRST, before every filter below. The tool-
	// skill registry is rebuilt from page content and cares about every
	// change — a seat's own edit, a project whose notifications are
	// excluded — so indexing must never be a casualty of a routing rule.
	if p.onPage != nil && pageID != "" {
		if err := p.onPage(ctx, eventType, pageID); err != nil {
			log.Warn("plane_page_index_failed", "page", pageID,
				"event_type", eventType, "error", err.Error())
		}
	}

	// FAIL CLOSED on an unresolvable project. The lead lookup below would
	// refuse this anyway, so this is not a second behavioural guard — it
	// is the DIAGNOSTIC split. "This build's page serializer carries no
	// project" is a fork-compatibility problem an operator has to fix,
	// and "this project has no lead" is a config gap; collapsing them
	// into one silent debug line sends whoever reads it to the wrong one.
	projectID := pageProjectID(data)
	if projectID == "" {
		log.Warn("plane_page_project_unresolved", "page", pageID, "event_type", eventType)
		return nil, nil
	}
	identifier := p.identifier(ctx, projectID)
	if identifier != "" && p.excluded[strings.ToUpper(identifier)] {
		return nil, nil
	}

	// CE pages have no subscription model and no comments, so the lead is
	// the only recipient there is. That is an accepted degradation rather
	// than an oversight: page discussion happens on work items, where the
	// full routing applies.
	base, ok := p.base(body, data, identifier, reg)
	if !ok {
		return nil, nil
	}
	activity := object(body, "activity")
	return p.leadCopy(base, identifier, str(activity, "actor_id"), ViaPageLead, reg), nil
}

// target is one recipient and why.
type target struct{ id, via string }

func targetsOf(ids []string, via string) []target {
	out := make([]target, 0, len(ids))
	for _, id := range ids {
		out = append(out, target{id: id, via: via})
	}
	return out
}

// thread is the subscriber fan-out, DEGRADING to the payload's assignees.
//
// It degrades when there is no client, when the lookup fails, when the
// response is empty, and when a non-empty response yields no usable ids — an
// unrecognised row shape must not silently suppress the fallback, because
// the alternative is a thread nobody hears about.
func (p *Parser) thread(ctx context.Context, projectID, issueID string, data map[string]any) []target {
	if p.subs != nil && projectID != "" && issueID != "" {
		rows, err := p.subs.Of(ctx, projectID, issueID)
		if err != nil {
			log.Warn("plane_subscribers_unavailable", "project", projectID,
				"issue", issueID, "error", err.Error())
		}
		// USABLE ids, not row count. A response that came back non-empty
		// and yielded nothing readable — a shape this build does not
		// recognise — must not count as "there are subscribers" and
		// suppress the fallback, or the thread is heard by nobody.
		var usable []string
		for _, id := range rows {
			if id = strings.ToLower(strings.TrimSpace(id)); id != "" {
				usable = append(usable, id)
			}
		}
		if len(usable) > 0 {
			return targetsOf(usable, ViaSubscriber)
		}
	}
	return targetsOf(ids(data["assignees"]), ViaAssignee)
}

// directed builds one copy per target.
//
// THE SINGLE CHOKE POINT where every directed fan-out is intersected with
// the parties the engine can actually route to. An ordinary workspace human
// must never receive a copy the service cannot resolve: each one surfaces as
// an undeliverable warning and a skip record, and a busy project produces
// them by the dozen.
//
// The ACTOR is excluded, and the FIRST reason for a person wins — a
// mentioned assignee is woken once, as a mention.
func (p *Parser) directed(base notify.Inbound, targets []target, actor string, reg *notify.Registry) []notify.Routed {
	var (
		out     []notify.Routed
		seen    = map[string]bool{}
		dropped int
	)
	for _, t := range targets {
		if t.id == "" || t.id == actor || seen[t.id] {
			continue
		}
		seen[t.id] = true
		if reg != nil {
			if _, known := reg.ByExternalID(Backend, t.id); !known {
				// Somebody in the workspace who is not a seat here.
				dropped++
				continue
			}
		}
		out = append(out, notify.Routed{
			Inbound: withVia(base, t.via),
			// The vendor's own id, resolved by the service's cascade —
			// a parser cannot know which seat a workspace UUID is.
			To: notify.Recipient{ExternalIDs: []string{t.id}},
		})
	}
	if dropped > 0 {
		log.Debug("plane_outsiders_dropped", "count", dropped,
			"event_type", base.EventType)
	}
	return out
}

// leadCopy is the fallback: the unit lead who owns the project.
//
// An unresolvable identifier or an unmapped project produces NO notification
// rather than a guess — a misroute is worse than a miss, because it teaches
// a seat that work it does not own is its problem.
func (p *Parser) leadCopy(base notify.Inbound, identifier, actor, via string, reg *notify.Registry) []notify.Routed {
	if identifier == "" {
		return nil
	}
	lead := p.leads[strings.ToUpper(identifier)]
	if lead == "" {
		log.Debug("plane_no_recipients", "event_type", base.EventType,
			"project", identifier)
		return nil
	}
	// THE LEAD'S OWN ACTION MUST NOT RE-TRIGGER THE LEAD. Without this a
	// lead filing a ticket in their own project wakes themselves, answers,
	// and wakes themselves again — the self-notification loop that costs a
	// turn per round for as long as nobody is watching.
	if reg != nil && actor != "" {
		if party, ok := reg.ByExternalID(Backend, actor); ok && party.Handle == lead {
			log.Debug("plane_lead_is_the_actor", "lead", lead,
				"event_type", base.EventType)
			return nil
		}
	}
	return []notify.Routed{{
		Inbound: withVia(base, via),
		To:      notify.Recipient{Handle: lead},
	}}
}

// withVia stamps the routing reason on a copy.
//
// A COPY of the metadata, because one payload produces several notifications
// and they carry different reasons — stamping the shared map would make every
// copy claim whichever reason was written last.
func withVia(base notify.Inbound, via string) notify.Inbound {
	meta := make(map[string]string, len(base.Metadata)+1)
	for k, v := range base.Metadata {
		meta[k] = v
	}
	meta[RoutedViaField] = via
	base.Metadata = meta
	return base
}

func (p *Parser) identifier(ctx context.Context, projectID string) string {
	if p.projects == nil || projectID == "" {
		return ""
	}
	return p.projects.Identifier(ctx, projectID)
}

func namesSomebodyElse(assignees []string, actor string) bool {
	for _, id := range assignees {
		if id != "" && id != actor {
			return true
		}
	}
	return false
}
