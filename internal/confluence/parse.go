package confluence

import (
	"context"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/org"
)

// Turning a page change into a woken seat.
//
// # A wiki has no assignees, and that shapes every rule here
//
// A tracker event names somebody: an assignee, a reporter, a watcher. A page
// event names only who edited it. So routing has three signals — a seat is
// SUBSCRIBED to the page, somebody was MENTIONED in the content, or the page
// lives in a space some unit owns — and the last is a fallback in the strict
// sense: it says "this concerns your team", never "this is yours".
//
// # The subscription list is the ENGINE'S, not Confluence's
//
// Confluence does keep watchers, and reading them is the obvious design. It
// is the wrong one here for three reasons that compound: the watcher list is
// mostly PEOPLE, who Confluence has already notified natively and who resolve
// to nothing the engine can route to; reading it costs a call per event on a
// path that must stay cheap; and a per-role token often cannot read another
// user's watch state at all, so the answer would be "no watchers" on exactly
// the deployments the feature is documented for.
//
// So the engine keeps its own list, of the only parties it can route to
// anyway. A seat is subscribed to a page when it TOUCHED it — edited it, or
// was mentioned on it — which is the same rule Confluence applies to people
// and needs no vendor call to evaluate. It lives on the coordination store,
// because a seat subscribed by a mention one node handled has to be found by
// whichever node handles the next event.
//
// The list is bounded by the coordination bucket's retention rather than by a
// per-page TTL: a page nobody has touched inside that window drops its
// subscribers, which is the right forgetting — a seat that edited a page a
// year ago is not waiting on it.

// Routing reasons, strongest first.
const (
	// ViaWatcher: the seat has touched this page before, so it is
	// subscribed to what happens next on it.
	ViaWatcher = "watcher"
	// ViaMention: named in the page or comment. A directed ask.
	ViaMention = "mention"
	// ViaSpaceLead: the page is in a space this seat's unit owns. Not a
	// claim on the seat's attention beyond "your team's documentation
	// moved".
	ViaSpaceLead = "space_lead"
)

// RoutedViaField carries the reason on a notification.
const RoutedViaField = "routed_via"

// contentEvents are the events that carry a page a seat could act on.
//
// An explicit set, because a Confluence instance emits a long tail about
// spaces, permissions, labels and attachments — none of which is addressed
// to anybody, and each of which would cost a turn spent triaging "somebody
// added a label".
var contentEvents = map[string]bool{
	"page_created":    true,
	"page_updated":    true,
	"page_trashed":    true,
	"page_removed":    true,
	"blog_created":    true,
	"blog_updated":    true,
	"comment_created": true,
	"comment_updated": true,
}

// edits are the events that constitute a claim on a page, as opposed to a
// remark about one. Only these subscribe their author — see the subscribe
// call in [Parser.Parse].
var edits = map[string]bool{
	"page_created": true,
	"page_updated": true,
	"blog_created": true,
	"blog_updated": true,
}

// ParserOptions configure a [Parser].
type ParserOptions struct {
	// SiteURL is the human base for links. Empty omits them, which is the
	// honest answer for a Cloud instance named only by its cloud id.
	SiteURL string

	// Leads maps a space KEY to the handle of the unit lead who owns it.
	Leads map[string]string

	// SkillsSpace is the engine-managed space holding tool skills. Its
	// pages are machinery: they are INDEXED and never routed, because a
	// seat woken by its own company's prompt fragment changing has
	// nothing to do about it.
	SkillsSpace string

	// OnPage re-indexes a changed page. It runs BEFORE every routing
	// filter, because the skill registry cares about every change —
	// including one in the space routing excludes.
	OnPage func(ctx context.Context, eventType, pageID string) error

	// Watchers is the engine's own page-subscription list. Nil routes by
	// mentions and space leads alone, which is the single-node case with
	// no coordination store and the honest degradation: a seat that
	// touched a page simply is not woken by later activity on it.
	Watchers Watchers

	// Now is the clock a subscription is stamped with. Nil takes the wall
	// clock.
	Now func() time.Time
}

// Watchers is the engine's page-subscription list, as this parser needs it.
//
// TWO METHODS, and the read one is a MEMBERSHIP TEST rather than an
// enumeration: the parser already knows every handle it could route to, so
// asking "which of these is subscribed" is one call whatever the page's
// history, while "who watches this page" would return identifiers the
// registry then has to resolve — including every human, who resolve to
// nothing.
type Watchers interface {
	// Watching returns the subset of handles subscribed to the page.
	//
	// FAILS OPEN AS EMPTY, deliberately: not knowing who is subscribed
	// must fall through to the space lead, which is where the event went
	// before subscriptions existed. The other direction would wake every
	// seat on a store blip.
	Watching(ctx context.Context, pageID string, handles []string) (map[string]bool, error)

	// Watch subscribes one handle to a page. Best effort: a subscription
	// that did not land costs one seat one missed follow-up, which is the
	// pre-subscription behaviour.
	Watch(ctx context.Context, pageID, handle string, at time.Time) error
}

// Parser turns one Confluence webhook into the notifications it implies.
type Parser struct {
	siteURL     string
	leads       map[string]string
	skillsSpace string
	onPage      func(ctx context.Context, eventType, pageID string) error
	watchers    Watchers
	now         func() time.Time
}

// NewParser builds the parser.
func NewParser(opts ParserOptions) *Parser {
	p := &Parser{
		siteURL:     strings.TrimRight(strings.TrimSpace(opts.SiteURL), "/"),
		leads:       make(map[string]string, len(opts.Leads)),
		skillsSpace: strings.ToUpper(strings.TrimSpace(opts.SkillsSpace)),
		onPage:      opts.OnPage,
		watchers:    opts.Watchers,
		now:         opts.Now,
	}
	if p.now == nil {
		p.now = time.Now
	}
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
func (p *Parser) Parse(ctx context.Context, w types.RawWebhook, reg *notify.Registry) ([]notify.Routed, error) {
	event := firstOf(str(w.Body, "event"), str(w.Body, "webhookEvent"))
	if !contentEvents[event] {
		return nil, nil
	}
	page, _ := w.Body["page"].(map[string]any)
	comment, _ := w.Body["comment"].(map[string]any)
	if len(page) == 0 && len(comment) == 0 {
		return nil, nil
	}

	pageID := str(page, "id")
	// THE INDEXER RUNS FIRST, before every filter below. The tool-skill
	// registry is rebuilt from page content and cares about EVERY change —
	// including one in the space routing excludes and one a seat made
	// itself — so indexing must never be a casualty of a routing rule.
	if p.onPage != nil && pageID != "" {
		if err := p.onPage(ctx, event, pageID); err != nil {
			log.WarnContext(ctx, "confluence_page_index_failed", "page", pageID,
				"event_type", event, "error", err.Error())
		}
	}

	space := strings.ToUpper(spaceOf(page, comment, w.Body))
	if p.skillsSpace != "" && space == p.skillsSpace {
		// Machinery, not knowledge. A seat woken because its own
		// company's prompt fragment changed has nothing to do about it.
		return nil, nil
	}

	base, ok := p.base(w.Body, page, comment, event, space)
	if !ok {
		return nil, nil
	}
	actor := base.Metadata[notify.ActorField]

	// SUBSCRIBERS AND MENTIONS TOGETHER, as one tier above the space
	// fallback. Ordering them against each other would be the wrong
	// question: a mention is a directed ask and a subscription is a
	// declared interest, and suppressing either in favour of the other
	// loses a recipient who genuinely wanted the event. Only the LEAD
	// fallback is exclusive — it exists for the case where nobody was
	// found at all.
	body := firstOf(storageOf(comment), storageOf(page))
	mentioned := MentionIDs(body)
	copies := p.directed(base, mentioned, actor, reg)
	copies = append(copies, p.subscribed(ctx, base, pageID, actor, copies, reg)...)

	// SUBSCRIBING COMES AFTER ROUTING, so this event's own recipients are
	// decided before the list changes.
	//
	// AN EDIT SUBSCRIBES ITS AUTHOR; A COMMENT DOES NOT. That asymmetry is
	// the whole delegation loop: a lead answering a page by commenting
	// "@teammate, yours" must NOT thereby subscribe itself, or every later
	// event on the page comes straight back and the delegation achieved
	// nothing. Editing a page is a claim on it; commenting on one is
	// often the opposite — handing it over. It is also the rule Confluence
	// itself applies to people, which is what keeps agents and humans
	// behaving alike on a shared page.
	author := ""
	if edits[event] {
		author = actor
	}
	p.subscribe(ctx, pageID, mentioned, author, reg)

	if len(copies) > 0 {
		return copies, nil
	}
	return p.leadCopy(base, space, actor, reg), nil
}

// subscribed builds one copy per seat that has touched this page before.
//
// The `already` list is this event's mention copies: a seat both mentioned
// and subscribed gets ONE notification, under the stronger reason. Two copies
// would be two turns for one page change.
func (p *Parser) subscribed(ctx context.Context, base notify.Inbound, pageID, actor string,
	already []notify.Routed, reg *notify.Registry,
) []notify.Routed {
	if p.watchers == nil || pageID == "" || reg == nil {
		return nil
	}
	handles := agentHandles(reg)
	if len(handles) == 0 {
		return nil
	}
	watching, err := p.watchers.Watching(ctx, pageID, handles)
	if err != nil {
		// EMPTY, so the event falls through to the space lead — where it
		// went before subscriptions existed. Waking every seat instead
		// would turn a store blip into a company-wide interrupt.
		log.WarnContext(ctx, "confluence_watchers_unreadable", "page", pageID,
			"error", err.Error(),
			"detail", "routing by mention and space lead alone for this event")
		return nil
	}
	// THE TWO TIERS ADDRESS DIFFERENTLY — a mention copy carries the
	// Confluence account id, a subscription copy carries the handle — so
	// the dedupe has to resolve one into the other. Comparing them raw
	// finds no overlap ever, and a seat both mentioned and subscribed then
	// gets two copies and runs two turns for one page change.
	seen := map[string]bool{}
	for _, copy := range already {
		if copy.To.Handle != "" {
			seen[copy.To.Handle] = true
		}
		for _, id := range copy.To.ExternalIDs {
			if party, known := reg.ByExternalID(Backend, id); known {
				seen[party.Handle] = true
			}
		}
	}
	out := make([]notify.Routed, 0, len(watching))
	// SORTED, because a map walk would order one page's recipients
	// differently on every delivery — and the order is what a reader of
	// the feed compares two events by.
	for _, handle := range slices.Sorted(maps.Keys(watching)) {
		if !watching[handle] || seen[handle] {
			continue
		}
		// The actor already knows what it just did. Its own external id
		// is what the webhook carries, so the handle is resolved back.
		if party, known := reg.ByExternalID(Backend, actor); known && party.Handle == handle {
			continue
		}
		out = append(out, notify.Routed{
			Inbound: withVia(base, ViaWatcher),
			To:      notify.Recipient{Handle: handle},
		})
	}
	return out
}

// agentHandles is every seat a subscription could wake.
//
// AGENTS ONLY. A person who edits a page is already watching it in Confluence
// and has already been notified natively; counting one here would spend a
// membership test on a party the engine cannot wake, and — worse — a human
// found "subscribed" would suppress the space-lead fallback in favour of a
// notification the service then skips.
func agentHandles(reg *notify.Registry) []string {
	out := make([]string, 0, reg.Len())
	for party := range reg.All() {
		if !party.Human && party.Handle != "" {
			out = append(out, party.Handle)
		}
	}
	slices.Sort(out)
	return out
}

// subscribe records who has now touched this page.
//
// BEST EFFORT and after the routing decision: a subscription that did not
// land costs one seat one missed follow-up, while failing the delivery over
// it would cost the event itself.
func (p *Parser) subscribe(ctx context.Context, pageID string, mentioned []string,
	actor string, reg *notify.Registry,
) {
	if p.watchers == nil || pageID == "" || reg == nil {
		return
	}
	at := p.now()
	seen := map[string]bool{}
	for _, id := range append(slices.Clone(mentioned), actor) {
		if id == "" {
			continue
		}
		party, known := reg.ByExternalID(Backend, id)
		// A PERSON IS NOT SUBSCRIBED. Confluence already watches for
		// them, and a human handle in this list would be tested on every
		// later event and route to nobody.
		if !known || party.Human || party.Handle == "" || seen[party.Handle] {
			continue
		}
		seen[party.Handle] = true
		if err := p.watchers.Watch(ctx, pageID, party.Handle, at); err != nil {
			log.WarnContext(ctx, "confluence_watch_not_recorded", "page", pageID,
				"seat", party.Handle, "error", err.Error(),
				"detail", "this seat will not be woken by later activity on the page")
		}
	}
}

// directed builds one copy per mentioned colleague.
//
// THE SINGLE CHOKE POINT where the mention list is intersected with the
// parties the engine can route to: an ordinary wiki user must never receive
// a copy the service cannot resolve, because each one surfaces as an
// undeliverable warning and a skip record.
func (p *Parser) directed(base notify.Inbound, ids []string, actor string, reg *notify.Registry) []notify.Routed {
	var (
		out  []notify.Routed
		seen = map[string]bool{}
	)
	for _, id := range ids {
		if id == "" || id == actor || seen[id] {
			continue
		}
		seen[id] = true
		if reg != nil {
			if _, known := reg.ByExternalID(Backend, id); !known {
				continue
			}
		}
		out = append(out, notify.Routed{
			Inbound: withVia(base, ViaMention),
			To:      notify.Recipient{ExternalIDs: []string{id}},
		})
	}
	return out
}

// leadCopy is the fallback: the lead of the unit that owns the space.
//
// A wiki page nobody was named in is not URGENT, and the prompt says so —
// but it must not vanish either: a space's documentation changing under a
// team is something its lead is the only person positioned to notice.
func (p *Parser) leadCopy(base notify.Inbound, space, actor string, reg *notify.Registry) []notify.Routed {
	if space == "" {
		return nil
	}
	lead := p.leads[space]
	if lead == "" {
		log.Debug("confluence_no_recipients", "event_type", base.EventType, "space", space)
		return nil
	}
	// THE LEAD'S OWN EDIT MUST NOT RE-TRIGGER THE LEAD, or a lead writing
	// in their own team's space wakes themselves, answers, and wakes
	// themselves again.
	if reg != nil && actor != "" {
		if party, ok := reg.ByExternalID(Backend, actor); ok && party.Handle == lead {
			return nil
		}
	}
	return []notify.Routed{{
		Inbound: withVia(base, ViaSpaceLead),
		To:      notify.Recipient{Handle: lead},
	}}
}

// base assembles the notification every recipient's copy is made from.
func (p *Parser) base(body, page, comment map[string]any, event, space string) (notify.Inbound, bool) {
	title := firstOf(str(page, "title"), str(comment, "title"))
	// THE COMMENT'S OWN CONTAINER IS THE SECOND PLACE THE PAGE IS NAMED.
	//
	// Both inbound shapes tolerate a comment payload that carries no
	// top-level `page` — the Forge relay only lifts a container it was given,
	// and Parse itself accepts comment-without-page — and the page id is the
	// whole conversation key here. Reading only the top-level object left
	// such a comment with no key at all, so it fell back to its own event id
	// and coalesced with nothing: three comments on one page while the seat
	// was busy ran three turns, which is precisely the case this key exists
	// to collapse. The container is the same object the relay would have
	// lifted, so this reads the page from where it actually is rather than
	// depending on whether an upstream copied it.
	pageID := firstOf(str(page, "id"), str(container(comment), "id"))
	if title == "" && pageID == "" {
		return notify.Inbound{}, false
	}
	actor, _ := body["user"].(map[string]any)
	if len(actor) == 0 {
		actor, _ = body["userAccountId"].(map[string]any)
	}
	account := firstOf(str(actor, "accountId"), str(actor, "username"),
		str(actor, "userKey"), str(body, "userAccountId"))

	meta := map[string]string{
		notify.ActorField: account,
		"actor_name":      firstOf(str(actor, "displayName"), str(actor, "username")),
		"event_type":      event,
		"space":           space,
		"page_id":         pageID,
		"page_title":      title,
	}
	if link := p.link(space, pageID); link != "" {
		meta["url"] = link
	}
	text := Flatten(firstOf(storageOf(comment), storageOf(page)))
	if len(comment) > 0 {
		meta["comment_id"] = str(comment, "id")
	}
	subject := title
	if subject == "" {
		subject = "Confluence " + event
	}
	return notify.Inbound{
		Source:    Backend,
		EventType: event,
		Sender:    firstOf(meta["actor_name"], account),
		Subject:   subject,
		Body:      excerpt(text),
		Metadata:  meta,
	}, true
}

// bodyLimit caps the page text carried on a notification.
//
// A wiki page can be tens of kilobytes, and the trigger's job is to say WHAT
// CHANGED and where — the seat fetches the page if it needs to act. Carrying
// the whole thing would put a document into every recipient's prompt for an
// event most of them will read and drop.
const bodyLimit = 600

// excerpt is the page text a notification carries.
//
// A POINTER THAT SAYS SO. The bound above is one of the few cuts here that
// earns its place — the page is re-readable through the seat's own tools and
// most recipients will read this and drop it — but it used to be applied by a
// helper that cut at the first newline or ". " BEFORE any limit, so a page was
// decapitated to its opening sentence whatever the budget, silently and
// mid-rune. A seat that could not tell an excerpt from the whole page acts on
// the opening line as though nothing followed it.
func excerpt(text string) string {
	out := knowledge.Snippet(text, bodyLimit)
	if out == "" || out == strings.Join(strings.Fields(text), " ") {
		return out
	}
	return out + "\n\n(Excerpt — read the page in full with your knowledge-base tools.)"
}

// link is the address a person opens, or empty when there is no human base.
func (p *Parser) link(space, pageID string) string {
	if p.siteURL == "" || space == "" || pageID == "" {
		return ""
	}
	return p.siteURL + "/wiki/spaces/" + space + "/pages/" + pageID
}

// withVia stamps the routing reason on a copy, on a COPY of the metadata —
// one payload produces several notifications and they carry different
// reasons.
func withVia(base notify.Inbound, via string) notify.Inbound {
	meta := make(map[string]string, len(base.Metadata)+1)
	for k, v := range base.Metadata {
		meta[k] = v
	}
	meta[RoutedViaField] = via
	base.Metadata = meta
	return base
}

// spaceOf finds the space key, which the deployments and the Forge relay
// each put somewhere different.
//
// A COMMENT's space may be stated on the comment, on its container page, or
// on the envelope — the relay copies it onto the page where it can, and a
// reader that knew one place would route nothing on the other two.
func spaceOf(page, comment, body map[string]any) string {
	for _, holder := range []map[string]any{page, comment, body} {
		if space, ok := holder["space"].(map[string]any); ok {
			if key := str(space, "key"); key != "" {
				return key
			}
		}
		if key := str(holder, "spaceKey"); key != "" {
			return key
		}
	}
	return ""
}

// storageOf reads a content object's storage-format body.
func storageOf(content map[string]any) string {
	body, ok := content["body"].(map[string]any)
	if !ok {
		return ""
	}
	storage, ok := body["storage"].(map[string]any)
	if !ok {
		return ""
	}
	return str(storage, "value")
}

// LeadsFrom maps each Confluence space key to the handle that owns it.
//
// The walk itself is [org.Organization.LeadsBy] — which seat owns a scope is
// a question about the org chart, and this vendor's only contribution is
// naming the field and reporting what the walk found in its own vocabulary.
func LeadsFrom(o *org.Organization) map[string]string {
	leads, report := o.LeadsBy(org.Scope{
		OfUnit: func(u *org.Unit) string { return u.Space },
		OfRole: func(r *org.Role) string { return r.Space },
	})
	for _, unled := range report.Unled {
		log.Warn("confluence_space_has_no_lead", "unit", unled.Unit, "space", unled.Scope)
	}
	for _, conflict := range report.Ambiguous {
		log.Warn("confluence_space_lead_ambiguous", "space", conflict.Scope,
			"declared_by", conflict.DeclaredBy, "chose", conflict.Chose,
			"candidates", conflict.Candidates)
	}
	return leads
}

// container is a comment's parent object, where a payload states one.
func container(comment map[string]any) map[string]any {
	if c, ok := comment["container"].(map[string]any); ok {
		return c
	}
	// Some shapes name it `parent` instead; both are the page the comment
	// hangs off, and which one arrives is the relay's choice, not a fact
	// about the comment.
	if c, ok := comment["parent"].(map[string]any); ok {
		return c
	}
	return nil
}

func str(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}
