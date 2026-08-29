// Package jira is the Jira integration: the work tracker a company's issues
// live in, and the inbound edge that turns activity on one into a woken seat.
//
// INBOUND ONLY. An agent's own Jira work — transitioning an issue, posting a
// comment, setting an assignee — happens through its MCP server under its
// own credential, so nothing here ever writes on a seat's behalf. What the
// engine contributes is the half a tool call cannot: deciding which seats an
// event concerns, and telling them what is being asked.
package jira

import (
	"context"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/notify"
)

var log = logging.Get("jira")

// Backend is the transport name, and the party registry's namespace for a
// Jira account id.
const Backend = "jira"

// issueEvents are the issue lifecycle events that can name a recipient.
//
// An explicit set rather than a prefix match on "jira:issue". Jira emits a
// long tail of issue-adjacent events — properties set, links created,
// worklogs, sprints, versions, boards — that concern the workspace rather
// than a person, and routing one produces a turn spent triaging "somebody
// set a property".
var issueEvents = map[string]bool{
	"jira:issue_created": true,
	"jira:issue_updated": true,
	"jira:issue_deleted": true,
}

// Routing reasons, in the order a target is considered. The FIRST reason for
// a given person wins, so a mentioned assignee is woken as a mention — which
// is the stronger claim on their attention and the one the prompt renders
// differently.
const (
	// ViaMention: named in a comment. A directed ask.
	ViaMention = "mention"
	// ViaAssignee: the issue is theirs.
	ViaAssignee = "assignee"
	// ViaWatcher: following the issue. The weakest claim — a watcher
	// once interacted, and Jira adds them automatically for doing so.
	ViaWatcher = "watcher"
	// ViaLeadFallback: nobody here was named, so the owning unit's lead
	// gets it. A ticket must never vanish.
	ViaLeadFallback = "project_lead_fallback"
)

// RoutedViaField carries the reason on a notification, so the prompt can say
// WHY this seat is being told — "you were mentioned" and "you are watching
// this issue" ask for different things.
const RoutedViaField = "routed_via"

// Watchers lists the accounts following an issue.
//
// A seam because the lookup needs the org credential and the routing rules
// are worth testing without one. Watchers are the one routing input a Jira
// payload never carries: the webhook names the assignee and the actor, and
// everybody else who asked to hear about the issue is invisible to it.
type Watchers interface {
	Of(ctx context.Context, issueKey string) ([]string, error)
}

// ParserOptions configure a [Parser].
type ParserOptions struct {
	// URL is the human-readable instance base, for building links a
	// person can open. Empty omits the link, which is the honest answer
	// for a Cloud instance named only by its cloud id.
	URL string

	// Watchers is the org-credential lookup. Nil routes from what the
	// payload names, which is a documented degradation rather than an
	// error: an issue's assignee and its mentions still reach their
	// seats, and only the people merely following it go unheard.
	Watchers Watchers

	// Leads maps a project KEY to the handle of the unit lead who owns
	// it — the recipient when nobody else here was named.
	Leads map[string]string
}

// Parser turns one Jira webhook into the notifications it implies.
type Parser struct {
	url      string
	watchers Watchers
	leads    map[string]string
}

// NewParser builds the parser.
func NewParser(opts ParserOptions) *Parser {
	p := &Parser{
		url:      strings.TrimRight(strings.TrimSpace(opts.URL), "/"),
		watchers: opts.Watchers,
		leads:    make(map[string]string, len(opts.Leads)),
	}
	// Keys are compared UPPER, because Jira renders them upper and an
	// operator writes them however they were shown.
	for key, lead := range opts.Leads {
		if key != "" && lead != "" {
			p.leads[strings.ToUpper(strings.TrimSpace(key))] = lead
		}
	}
	return p
}

// Source implements [notify.Parser].
func (p *Parser) Source() string { return Backend }

// Parse implements [notify.Parser].
//
// # The order of the targets is the whole routing rule
//
// Mentions, then the assignee, then the watchers — strongest claim first,
// because [Parser.directed] keeps the FIRST reason it sees for a person. A
// naive fan-out that walked the watcher list first would tell a mentioned
// colleague they are "watching this issue", which is the one framing that
// makes a direct ask look like background noise. Jira makes that easy to get
// wrong: it adds a mentioned user to the watcher list, so both reasons are
// true for the same person on nearly every comment.
func (p *Parser) Parse(ctx context.Context, w types.RawWebhook, reg *notify.Registry) ([]notify.Routed, error) {
	event := str(w.Body, "webhookEvent")
	if !issueEvents[event] && !commentEvents[event] {
		return nil, nil
	}
	base, ok := p.base(w.Body, reg)
	if !ok {
		// A payload naming no issue. Every rule below rests on the issue
		// — the watcher lookup, the conversation key, the recon pointer
		// — so a copy without one is a wake with nowhere to look.
		log.Debug("jira_event_names_no_issue", "event_type", event)
		return nil, nil
	}
	meta := base.Metadata
	actor := meta[notify.ActorField]

	var targets []target
	if comment, ok := w.Body["comment"].(map[string]any); ok {
		targets = targetsOf(MentionIDs(comment["body"]), ViaMention)
	}
	if assignee := meta["assignee_account_id"]; assignee != "" {
		targets = append(targets, target{
			id: assignee, email: meta["assignee_email"], via: ViaAssignee,
		})
	}
	targets = append(targets, targetsOf(p.watching(ctx, meta["issue_key"]), ViaWatcher)...)

	if copies := p.directed(base, targets, actor, reg); len(copies) > 0 {
		return copies, nil
	}
	// NOBODY HERE WAS REACHED. The lead is woken unless the actor is the
	// issue's own assignee, which is the one case where the work is
	// demonstrably owned: somebody took it and is working on it in the
	// open. Everything else — an unassigned ticket filed into the
	// project, an issue handed entirely to people outside the org chart —
	// has landed nowhere, and a ticket that lands nowhere produces no
	// error anywhere and is discovered weeks later.
	//
	// Keyed on the ASSIGNEE rather than on "was every target the actor",
	// because Jira adds the reporter to the watcher list automatically:
	// the founder who files an unassigned ticket is its only watcher, so
	// the every-target-was-the-actor reading would call the most
	// important case in the integration self-service and stay silent.
	if meta["assignee_account_id"] != "" && meta["assignee_account_id"] == actor {
		log.Debug("jira_actor_owns_the_issue", "issue", meta["issue_key"],
			"event_type", event)
		return nil, nil
	}
	return p.leadCopy(base, meta["project"], actor, reg), nil
}

// target is one recipient and why.
//
// The email rides along because the assignee is the one target a payload
// names twice, and the two identities resolve against different halves of
// the registry: an agent seat is known by the account id its credential
// authenticates as, a human colleague by the address in their contact block.
// Carrying only the id would leave every human seat unreachable until
// somebody registered a Jira account for them.
type target struct{ id, email, via string }

func targetsOf(ids []string, via string) []target {
	out := make([]target, 0, len(ids))
	for _, id := range ids {
		out = append(out, target{id: id, via: via})
	}
	return out
}

// watching is the watcher fan-out, DEGRADING to nothing.
//
// A failed lookup is logged and yields no watcher targets — it never fails
// the delivery. The assignee and the mentions are in the payload and still
// route; what is lost is the people merely following the issue, which is the
// smallest possible casualty of an instance that is briefly unreachable.
func (p *Parser) watching(ctx context.Context, issueKey string) []string {
	if p.watchers == nil || issueKey == "" {
		return nil
	}
	ids, err := p.watchers.Of(ctx, issueKey)
	if err != nil {
		log.Warn("jira_watchers_unavailable", "issue", issueKey,
			"error", err.Error(),
			"detail", "this event reaches the assignee and anyone mentioned; "+
				"seats merely watching the issue do not hear about it")
		return nil
	}
	return ids
}

// directed builds one copy per target.
//
// THE SINGLE CHOKE POINT where every fan-out is intersected with the parties
// the engine can actually route to. An ordinary Jira user must never receive
// a copy the service cannot resolve: each one surfaces as an undeliverable
// warning and a skip record, and a busy project produces them by the dozen.
//
// The ACTOR is excluded, and the FIRST reason for a person wins.
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
		if reg != nil && !known(reg, t) {
			// Somebody in the instance who is not a seat here.
			dropped++
			continue
		}
		out = append(out, notify.Routed{
			Inbound: withVia(base, t.via),
			To: notify.Recipient{
				Email:       t.email,
				ExternalIDs: []string{t.id},
			},
		})
	}
	if dropped > 0 {
		log.Debug("jira_outsiders_dropped", "count", dropped,
			"event_type", base.EventType)
	}
	return out
}

// known reports whether either identity on a target names a colleague.
func known(reg *notify.Registry, t target) bool {
	if _, ok := reg.ByExternalID(Backend, t.id); ok {
		return true
	}
	if t.email == "" {
		return false
	}
	_, ok := reg.ByEmail(t.email)
	return ok
}

// leadCopy is the fallback: the unit lead who owns the project.
//
// An unmapped project produces NO notification rather than a guess — a
// misroute is worse than a miss, because it teaches a seat that work it does
// not own is its problem.
func (p *Parser) leadCopy(base notify.Inbound, projectKey, actor string, reg *notify.Registry) []notify.Routed {
	if projectKey == "" {
		return nil
	}
	lead := p.leads[strings.ToUpper(projectKey)]
	if lead == "" {
		log.Debug("jira_no_recipients", "event_type", base.EventType,
			"project", projectKey)
		return nil
	}
	// THE LEAD'S OWN ACTION MUST NOT RE-TRIGGER THE LEAD. Without this a
	// lead filing a ticket in their own project wakes themselves, answers,
	// and wakes themselves again — the self-notification loop that costs a
	// turn per round for as long as nobody is watching.
	if reg != nil && actor != "" {
		if party, ok := reg.ByExternalID(Backend, actor); ok && party.Handle == lead {
			log.Debug("jira_lead_is_the_actor", "lead", lead,
				"event_type", base.EventType)
			return nil
		}
	}
	return []notify.Routed{{
		Inbound: withVia(base, ViaLeadFallback),
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

// Lookup adapts a client to the parser's [Watchers] seam.
//
// A nil Client REPORTS an error rather than dereferencing: a company with no
// org credential still routes from what its payloads name, and the parser
// already treats a failed lookup as "no watchers". A panic here would turn
// that documented degradation into a dead inbound consumer.
type Lookup struct{ Client *Client }

// Of implements [Watchers].
func (l Lookup) Of(ctx context.Context, issueKey string) ([]string, error) {
	if l.Client == nil {
		return nil, fmt.Errorf("jira: no client")
	}
	return l.Client.WatchersOf(ctx, issueKey)
}
