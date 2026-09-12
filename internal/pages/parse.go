package pages

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

// Routing reasons, strongest claim first.
const (
	// ViaMention: named in a comment or an edit. A directed ask, and the
	// only reason a mute does not suppress.
	ViaMention = "mention"

	// ViaWatcher: following the page.
	ViaWatcher = "watcher"

	// ViaLeadFallback: nobody here was named, so the lead of the unit that
	// owns the container gets it.
	//
	// IT FIRES FOR FEWER THINGS THAN THE TRACKER'S. An unassigned work item
	// is work nobody owns and must reach somebody; an unwatched page is
	// ordinarily just a page. So the fallback here is for a page CREATED or
	// TRASHED in a team's container — the two changes a lead has a reason
	// to know about — and not for every save.
	ViaLeadFallback = "container_lead_fallback"
)

// RoutedViaField carries the reason on a notification.
const RoutedViaField = "routed_via"

// The metadata keys the prompt reads back.
const (
	MetaPageID     = "page_id"
	MetaContainer  = "container"
	MetaTitle      = "title"
	MetaStatus     = "status"
	MetaVersion    = "version"
	MetaChangeID   = "change_id"
	MetaChangeKind = "change_kind"
	MetaCommentID  = "comment_id"
	MetaRevision   = "head_revision"
)

// Leads maps a container key to the handle that owns it.
type Leads map[string]string

// Parser turns a page change into the notifications it implies.
type Parser struct {
	leads   Leads
	baseURL string
}

// ParserOptions configure a parser.
type ParserOptions struct {
	Leads   Leads
	BaseURL string
}

// NewParser builds the knowledge base's inbound parser.
func NewParser(opts ParserOptions) *Parser {
	leads := make(Leads, len(opts.Leads))
	for container, handle := range opts.Leads {
		leads[strings.ToUpper(strings.TrimSpace(container))] = handle
	}
	return &Parser{leads: leads, baseURL: strings.TrimRight(opts.BaseURL, "/")}
}

// Source is the integration name.
func (p *Parser) Source() string { return Source }

// Parse reports which seats a page change concerns.
func (p *Parser) Parse(ctx context.Context, w types.RawWebhook, reg *notify.Registry) ([]notify.Routed, error) {
	change, err := changeFromBody(w.Body)
	if err != nil {
		return nil, err
	}
	if change.PageID == "" {
		log.DebugContext(ctx, "pages_change_names_no_page", "change", change.ID)
		return nil, nil
	}
	base := p.inbound(change)
	actor := change.Actor

	var targets []target
	for _, handle := range change.Mentions {
		targets = append(targets, target{handle: handle, via: ViaMention})
	}
	for _, handle := range change.Snapshot.Watchers {
		targets = append(targets, target{handle: handle, via: ViaWatcher})
	}
	if copies := p.directed(base, targets, actor, reg); len(copies) > 0 {
		return copies, nil
	}
	if !LeadWorthy(change.Kind) {
		// An ordinary save nobody follows. A wiki fills up with pages
		// nobody watches, and waking a lead for every one of them is how a
		// lead learns to ignore the knowledge base entirely.
		return nil, nil
	}
	return p.leadCopy(base, change.Snapshot.Container, actor, reg), nil
}

// LeadWorthy reports whether a change reaches the container's lead when
// nobody else was named.
//
// TWO KINDS. A page appearing in a team's container and one being trashed are
// facts a lead has a reason to know; a save, a label change or a move is not.
func LeadWorthy(kind ChangeKind) bool {
	return kind == ChangeCreated || kind == ChangeRemoved || kind == ChangeStatus
}

type target struct{ handle, via string }

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
		log.Debug("pages_unknown_recipients_dropped", "count", dropped,
			"page", base.Metadata[MetaTitle])
	}
	return out
}

func (p *Parser) leadCopy(base notify.Inbound, container, actor string, reg *notify.Registry) []notify.Routed {
	if container == "" {
		return nil
	}
	lead := p.leads[strings.ToUpper(container)]
	if lead == "" {
		log.Debug("pages_no_recipients", "container", container,
			"page", base.Metadata[MetaTitle])
		return nil
	}
	// A lead writing in their own container must not wake themselves.
	if lead == actor {
		return nil
	}
	return []notify.Routed{{
		Inbound: withVia(base, ViaLeadFallback),
		To:      notify.Recipient{Handle: lead},
		WakeID:  changefeed.WakeID(base.Metadata[MetaChangeID], lead),
	}}
}

func (p *Parser) inbound(change Change) notify.Inbound {
	meta := map[string]string{
		notify.ActorField: change.Actor,
		MetaPageID:        change.PageID,
		MetaContainer:     change.Snapshot.Container,
		MetaTitle:         change.Snapshot.Title,
		MetaStatus:        string(change.Snapshot.Status),
		MetaVersion:       fmt.Sprint(change.Snapshot.Version),
		MetaChangeID:      change.ID,
		MetaChangeKind:    string(change.Kind),
	}
	if change.CommentID != "" {
		meta[MetaCommentID] = change.CommentID
	}
	if change.HeadRevision != 0 {
		meta[MetaRevision] = fmt.Sprint(change.HeadRevision)
	}
	if link := p.link(change.PageID); link != "" {
		meta["url"] = link
	}
	return notify.Inbound{
		Source:    Source,
		EventType: string(change.Kind),
		Sender:    change.Actor,
		Subject:   change.Snapshot.Title,
		Body:      change.Excerpt,
		Metadata:  meta,
	}
}

func (p *Parser) link(pageID string) string {
	if p.baseURL == "" || pageID == "" {
		return ""
	}
	return p.baseURL + "/pages/" + pageID
}

func withVia(base notify.Inbound, via string) notify.Inbound {
	meta := make(map[string]string, len(base.Metadata)+1)
	for k, v := range base.Metadata {
		meta[k] = v
	}
	meta[RoutedViaField] = via
	base.Metadata = meta
	return base
}

func changeFromBody(body map[string]any) (Change, error) {
	if len(body) == 0 {
		return Change{}, fmt.Errorf("pages: the delivery carries no change record")
	}
	data, err := json.Marshal(body)
	if err != nil {
		return Change{}, fmt.Errorf("pages: read the delivery: %w", err)
	}
	return DecodeChange(data)
}

// AddressedKinds are the routing reasons that mean somebody is waiting.
//
// ONE, and that is the difference from the tracker: a page has no assignee,
// so the only thing that constitutes an ask here is somebody naming you.
// Marking a watcher copy addressed would oblige a seat to answer every save
// on every page it follows.
func AddressedKinds() []string { return []string{ViaMention} }

// Addressed reports whether this notification is an ask.
func Addressed(meta map[string]string) bool {
	return slices.Contains(AddressedKinds(), meta[RoutedViaField])
}
