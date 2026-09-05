package work

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/changefeed"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/notify"
)

// Routing reasons, in the order a target is considered.
//
// THE FIRST REASON FOR A GIVEN PERSON WINS, so a mentioned assignee is woken
// as a MENTION — the stronger claim on their attention, and the one the
// prompt renders as an ask rather than as news.
const (
	// ViaMention: named in a comment. A directed ask, and the one reason a
	// mute does not suppress.
	ViaMention = "mention"

	// ViaAssignee: the item is theirs.
	ViaAssignee = "assignee"

	// ViaWatcher: following the item. The weakest claim — most watchers
	// are there because the participants rule put them there.
	ViaWatcher = "watcher"

	// ViaLeadFallback: nobody here was named, so the lead of the unit that
	// owns the project gets it. AN ITEM MUST NEVER LAND NOWHERE: that
	// produces no error anywhere and is discovered weeks later.
	ViaLeadFallback = "project_lead_fallback"
)

// RoutedViaField carries the reason on a notification, so the prompt can say
// WHY this seat is being told — "you were mentioned" and "you are watching
// this" ask for different things.
const RoutedViaField = "routed_via"

// The metadata keys the prompt reads back. Named constants because the
// parser writes them and the prompt reads them, and a typo in either is a
// field that silently renders empty.
const (
	MetaItemKey    = "item_key"
	MetaItemID     = "item_id"
	MetaProject    = "project"
	MetaStatus     = "status"
	MetaAssignee   = "assignee"
	MetaChangeID   = "change_id"
	MetaChangeKind = "change_kind"
	MetaCommentID  = "comment_id"
	MetaRevision   = "head_revision"
	MetaTitle      = "title"
)

// Leads maps a project key to the handle that owns it.
type Leads map[string]string

// Parser turns a change record into the notifications it implies.
//
// It implements [notify.Parser]. EVERYTHING IT NEEDS IS IN THE PAYLOAD: the
// change carries its own routing snapshot, so the node that wins a delivery
// routes without reading a projection it may be behind on.
type Parser struct {
	leads Leads

	// baseURL is where a person's browser reaches this deployment, for
	// composing a link. Empty omits it, which is the honest answer for a
	// deployment that has not configured api.public_url — a link that
	// opens nothing costs a reader a click to discover.
	baseURL string
}

// ParserOptions configure a parser.
type ParserOptions struct {
	Leads   Leads
	BaseURL string
}

// NewParser builds the tracker's inbound parser.
func NewParser(opts ParserOptions) *Parser {
	leads := make(Leads, len(opts.Leads))
	for project, handle := range opts.Leads {
		leads[strings.ToUpper(strings.TrimSpace(project))] = handle
	}
	return &Parser{leads: leads, baseURL: strings.TrimRight(opts.BaseURL, "/")}
}

// Source is the integration name, matching the delivery's own.
func (p *Parser) Source() string { return Source }

// Parse reports which seats a change concerns.
func (p *Parser) Parse(ctx context.Context, w types.RawWebhook, reg *notify.Registry) ([]notify.Routed, error) {
	change, err := changeFromBody(w.Body)
	if err != nil {
		return nil, err
	}
	if change.Snapshot.Key == "" {
		// A change naming no item. Every rule below rests on the item —
		// the conversation key, the link, the lead lookup — so a record
		// without one is a wake with nowhere to look.
		log.DebugContext(ctx, "work_change_names_no_item", "change", change.ID)
		return nil, nil
	}
	base := p.inbound(change)
	actor := change.Actor

	targets := p.targets(change)
	if copies := p.directed(base, targets, actor, reg); len(copies) > 0 {
		return copies, nil
	}

	// NOBODY HERE WAS REACHED. The lead is woken unless the actor is the
	// item's own assignee, which is the one case where the work is
	// demonstrably owned: somebody took it and is working on it in the
	// open. Everything else — an unassigned item filed into the project,
	// an item handed entirely to people outside the org chart — has landed
	// nowhere.
	//
	// Keyed on the ASSIGNEE rather than on "was every target the actor",
	// because the participants rule adds the reporter automatically: a
	// founder who files an unassigned item is its only watcher, so the
	// every-target-was-the-actor reading would call the most important
	// case in the tracker self-service and stay silent.
	if change.Snapshot.Assignee != "" && change.Snapshot.Assignee == actor {
		log.DebugContext(ctx, "work_actor_owns_the_item",
			"item", change.Snapshot.Key, "kind", string(change.Kind))
		return nil, nil
	}
	return p.leadCopy(base, change.Snapshot.Project, actor, reg), nil
}

// target is one recipient and why.
type target struct{ handle, via string }

// targets is everyone this change concerns, strongest claim first.
func (p *Parser) targets(change Change) []target {
	var out []target
	for _, handle := range change.Mentions {
		out = append(out, target{handle: handle, via: ViaMention})
	}
	if change.Snapshot.Assignee != "" {
		out = append(out, target{handle: change.Snapshot.Assignee, via: ViaAssignee})
	}
	// THE SNAPSHOT'S WATCHERS ARE ALREADY MINUS THE MUTED. Subtracting
	// here would be a second implementation of the mute rule, and the one
	// that forgot would re-wake somebody who explicitly opted out.
	for _, handle := range change.Snapshot.Watchers {
		out = append(out, target{handle: handle, via: ViaWatcher})
	}
	return out
}

// directed builds one copy per reachable recipient.
func (p *Parser) directed(base notify.Inbound, targets []target, actor string, reg *notify.Registry) []notify.Routed {
	var (
		out     []notify.Routed
		seen    = map[string]bool{}
		dropped int
	)
	for _, t := range targets {
		if t.handle == "" || t.handle == actor || seen[t.handle] {
			continue
		}
		seen[t.handle] = true
		if reg != nil {
			if _, ok := reg.ByHandle(t.handle); !ok {
				// A handle that is no longer a seat: somebody who left,
				// or a watcher recorded before a rename. Dropped rather
				// than routed, because a notification addressed to
				// nobody is one nothing reports.
				dropped++
				continue
			}
		}
		out = append(out, notify.Routed{
			Inbound: withVia(base, t.via),
			To:      notify.Recipient{Handle: t.handle},
			// DERIVED, so a redelivery is recognisable as one. The
			// feed's claim is the first dedupe layer and it FAILS
			// OPEN — a coordination store that cannot be reached
			// must not stop notifications — so this is what catches
			// what slips through. See [changefeed.WakeID].
			WakeID: changefeed.WakeID(base.Metadata[MetaChangeID], t.handle),
		})
	}
	if dropped > 0 {
		log.Debug("work_unknown_recipients_dropped", "count", dropped,
			"item", base.Metadata[MetaItemKey])
	}
	return out
}

// leadCopy is the fallback: the lead of the unit that owns the project.
//
// An unmapped project produces NO notification rather than a guess, and says
// so at boot instead: a wake sent to whoever happens to be first in a map is
// worse than one nobody receives, because it teaches that seat to ignore the
// tracker.
func (p *Parser) leadCopy(base notify.Inbound, project, actor string, reg *notify.Registry) []notify.Routed {
	if project == "" {
		return nil
	}
	lead := p.leads[strings.ToUpper(project)]
	if lead == "" {
		log.Debug("work_no_recipients", "project", project,
			"item", base.Metadata[MetaItemKey])
		return nil
	}
	// THE LEAD'S OWN ACTION MUST NOT RE-TRIGGER THE LEAD. Without this a
	// lead filing an item in their own project wakes themselves, answers,
	// and wakes themselves again — a self-notification loop costing a turn
	// per round for as long as nobody is watching.
	if lead == actor {
		log.Debug("work_lead_is_the_actor", "lead", lead,
			"item", base.Metadata[MetaItemKey])
		return nil
	}
	return []notify.Routed{{
		Inbound: withVia(base, ViaLeadFallback),
		To:      notify.Recipient{Handle: lead},
		WakeID:  changefeed.WakeID(base.Metadata[MetaChangeID], lead),
	}}
}

// inbound renders the change as the spine's flat notification.
func (p *Parser) inbound(change Change) notify.Inbound {
	meta := map[string]string{
		notify.ActorField: change.Actor,
		MetaItemKey:       change.Snapshot.Key,
		MetaItemID:        change.ItemID,
		MetaProject:       change.Snapshot.Project,
		MetaStatus:        string(change.Snapshot.Status),
		MetaAssignee:      change.Snapshot.Assignee,
		MetaChangeID:      change.ID,
		MetaChangeKind:    string(change.Kind),
		MetaTitle:         change.Snapshot.Title,
	}
	if change.CommentID != "" {
		meta[MetaCommentID] = change.CommentID
	}
	if change.HeadRevision != 0 {
		meta[MetaRevision] = fmt.Sprint(change.HeadRevision)
	}
	if link := p.link(change.Snapshot.Key); link != "" {
		// "url" is the key every vendor's parser already writes, so a
		// prompt renders a link the same way whichever source produced
		// the notification.
		meta["url"] = link
	}
	return notify.Inbound{
		Source:    Source,
		EventType: string(change.Kind),
		Sender:    change.Actor,
		Subject:   change.Snapshot.Key + " " + change.Snapshot.Title,
		Body:      change.Excerpt,
		Metadata:  meta,
	}
}

// link is where a person opens this item, or empty.
func (p *Parser) link(itemKey string) string {
	if p.baseURL == "" || itemKey == "" {
		return ""
	}
	return p.baseURL + "/work/" + itemKey
}

// withVia stamps the routing reason on a copy.
//
// A COPY of the metadata, because one change produces several notifications
// with different reasons — stamping the shared map would make every copy
// claim whichever reason was written last.
func withVia(base notify.Inbound, via string) notify.Inbound {
	meta := make(map[string]string, len(base.Metadata)+1)
	for k, v := range base.Metadata {
		meta[k] = v
	}
	meta[RoutedViaField] = via
	base.Metadata = meta
	return base
}

// changeFromBody decodes the record a feed relayed.
func changeFromBody(body map[string]any) (Change, error) {
	if len(body) == 0 {
		return Change{}, fmt.Errorf("work: the delivery carries no change record")
	}
	data, err := json.Marshal(body)
	if err != nil {
		return Change{}, fmt.Errorf("work: read the delivery: %w", err)
	}
	return DecodeChange(data)
}

// AddressedKinds are the routing reasons that mean somebody is WAITING on
// this seat.
//
// Only two, and the line is deliberate: an assignment and a mention are asks,
// while a watcher copy and a lead fallback are news. The turn engine reads
// this as the half of "did this turn deliver?" a model cannot get wrong — an
// addressed turn may not end in silence, because to the person who asked,
// silence is indistinguishable from a message that was lost. Marking a
// watcher copy addressed would make every seat post something on every change
// it merely observes.
func AddressedKinds() []string { return []string{ViaAssignee, ViaMention} }

// Addressed reports whether this notification is an ask.
func Addressed(meta map[string]string) bool {
	return slices.Contains(AddressedKinds(), meta[RoutedViaField])
}
