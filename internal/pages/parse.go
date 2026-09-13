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
//
// EVERY ONE HAS A PRODUCER AND A READER. The set used to carry three more — a
// status, a comment id and a head revision — each read by nothing, and each
// written from a shape this domain stopped putting on the wire when it moved
// onto its log. A key nobody sets and nobody reads is indistinguishable from
// one whose producer broke, which is exactly what had happened to all three.
const (
	MetaPageID     = "page_id"
	MetaContainer  = "container"
	MetaTitle      = "title"
	MetaVersion    = "version"
	MetaChangeID   = "change_id"
	MetaChangeKind = "change_kind"
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
//
// IT READS THE RECORD THE FEED DELIVERS, which is a [MutationRecord] — the
// same shape [Translator.Translate] encodes and the same one the tracker's own
// parser reads. It used to decode the body as a [Change], the shape the
// coordination family this domain replaced put on the wire; the two share a
// version field and almost no key, so every delivery decoded cleanly into a
// change with no page id, and every page notification in the company was
// silently dropped one line later.
func (p *Parser) Parse(ctx context.Context, w types.RawWebhook, reg *notify.Registry) ([]notify.Routed, error) {
	record, err := recordFromBody(w.Body)
	if err != nil {
		return nil, err
	}
	if record.Notify == nil {
		// A RECORD NOBODY ANNOUNCED reaching the parser at all means the
		// feed's own decision was bypassed — a replayed body, a test, a
		// second producer. Answering "nobody" rather than erroring keeps
		// the two layers independent: the feed decides what to relay and
		// this decides who it is for.
		return nil, nil
	}
	if record.Notify.PageID == "" {
		log.DebugContext(ctx, "pages_change_names_no_page", "change", record.OpID)
		return nil, nil
	}
	base := p.inbound(record)
	actor := record.Actor

	var targets []target
	for _, handle := range record.Notify.Mentions {
		targets = append(targets, target{handle: handle, via: ViaMention})
	}
	// THE RECIPIENTS ARE THE WATCHERS MINUS THE MUTED, subtracted once at
	// write time — so this can never forget to, and a handle that unwatched
	// is not woken by a set that still remembers them.
	for _, handle := range record.Notify.Recipients {
		targets = append(targets, target{handle: handle, via: ViaWatcher})
	}
	if copies := p.directed(base, targets, actor, reg); len(copies) > 0 {
		return copies, nil
	}
	if !LeadWorthy(record.Notify.Kind) {
		// An ordinary save nobody follows. A wiki fills up with pages
		// nobody watches, and waking a lead for every one of them is how a
		// lead learns to ignore the knowledge base entirely.
		return nil, nil
	}
	return p.leadCopy(base, record.Notify.Container, actor, reg), nil
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

func (p *Parser) inbound(record MutationRecord) notify.Inbound {
	n := record.Notify
	meta := map[string]string{
		notify.ActorField: record.Actor,
		MetaPageID:        n.PageID,
		MetaContainer:     n.Container,
		MetaTitle:         n.Title,
		MetaVersion:       fmt.Sprint(n.Version),
		MetaChangeID:      record.OpID,
		MetaChangeKind:    string(n.Kind),
	}
	if link := p.link(n.PageID); link != "" {
		meta["url"] = link
	}
	return notify.Inbound{
		Source:    Source,
		EventType: string(n.Kind),
		Sender:    record.Actor,
		Subject:   n.Title,
		Body:      n.Excerpt,
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

// recordFromBody recovers the record the feed relayed.
//
// THROUGH THE DOMAIN'S OWN DECODER, so a record a newer build wrote reaches
// this parser with everything it carried: the body is exactly the record,
// unknown fields included, and re-encoding it later must not strip them.
func recordFromBody(body map[string]any) (MutationRecord, error) {
	if len(body) == 0 {
		return MutationRecord{}, fmt.Errorf("pages: the delivery carries no record")
	}
	data, err := json.Marshal(body)
	if err != nil {
		return MutationRecord{}, fmt.Errorf("pages: read the delivery: %w", err)
	}
	return Decode(data)
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
