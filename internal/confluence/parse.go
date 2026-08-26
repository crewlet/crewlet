package confluence

import (
	"context"
	"strings"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/org"
)

// Turning a page change into a woken seat.
//
// # A wiki has no assignees, and that shapes every rule here
//
// A tracker event names somebody: an assignee, a reporter, a watcher. A page
// event names only who edited it. So routing here has exactly two honest
// signals — somebody was MENTIONED in the content, or the page lives in a
// space some unit owns — and the second is a fallback in the strict sense:
// it says "this concerns your team", never "this is yours".

// Routing reasons, strongest first.
const (
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
}

// Parser turns one Confluence webhook into the notifications it implies.
type Parser struct {
	siteURL     string
	leads       map[string]string
	skillsSpace string
	onPage      func(ctx context.Context, eventType, pageID string) error
}

// NewParser builds the parser.
func NewParser(opts ParserOptions) *Parser {
	p := &Parser{
		siteURL:     strings.TrimRight(strings.TrimSpace(opts.SiteURL), "/"),
		leads:       make(map[string]string, len(opts.Leads)),
		skillsSpace: strings.ToUpper(strings.TrimSpace(opts.SkillsSpace)),
		onPage:      opts.OnPage,
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
			log.Warn("confluence_page_index_failed", "page", pageID,
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

	// MENTIONS FIRST, because being named is a directed ask and the space
	// fallback is not one.
	body := firstOf(storageOf(comment), storageOf(page))
	if copies := p.directed(base, MentionIDs(body), actor, reg); len(copies) > 0 {
		return copies, nil
	}
	return p.leadCopy(base, space, actor, reg), nil
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
	pageID := str(page, "id")
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
		Body:      Snippet(text, bodyLimit),
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
		OfUnit: func(u *org.Unit) string { return u.ConfluenceSpace },
		OfRole: func(r *org.Role) string { return r.ConfluenceSpace },
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

func str(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}
