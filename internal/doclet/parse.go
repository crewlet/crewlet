package doclet

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/changefeed"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/routing"
)

// Routing reasons, strongest claim first — which is the order [Parser.Parse]
// offers them in, and so their precedence (internal/routing: the first reason
// that names a handle is the one it hears under).
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
	// ordinarily just a page. So the fallback here is for a page CREATED,
	// TRASHED or changing STATUS in a team's container ([LeadWorthy]) — the
	// changes a lead has a reason to know about — and not for every save.
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
		leads[ContainerKey(container)] = handle
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

	// THE TRACKER'S RULE AT ITS SMALLEST (internal/routing): two reasons in
	// precedence order and a chain of one lead to fall back on. A mention
	// outranks a watch, so a watcher who is also named hears that they were
	// named, and hears it once.
	var b routing.Builder[string]
	b.AddAll(record.Notify.Mentions, ViaMention, true)
	// THE RECIPIENTS ARE THE WATCHERS MINUS THE MUTED, subtracted once at
	// write time — so this can never forget to, and a handle that unwatched
	// is not woken by a set that still remembers them.
	b.AddAll(record.Notify.Recipients, ViaWatcher, false)
	// AN ORDINARY SAVE NOBODY FOLLOWS falls back on nobody. A wiki fills up
	// with pages nobody watches, and waking a lead for every one of them is
	// how a lead learns to ignore the knowledge base entirely — so only a
	// change [LeadWorthy] names offers the container's lead, and Route keeps
	// that copy only when nobody named survived.
	container := record.Notify.Container
	offered := container != "" && LeadWorthy(record.Notify.Kind)
	lead := ""
	if offered {
		lead = p.leads[ContainerKey(container)]
		b.Fallback(ViaLeadFallback, lead)
	}

	// THE LEAD IS CHECKED AGAINST THE ROSTER LIKE EVERY OTHER RECIPIENT. The
	// lead map is built from one epoch's org and this parser outlives that
	// epoch, so a lead whose seat was removed or whose handle was changed is
	// a handle the company no longer employs — and routing to one produces a
	// delivery that resolves to nobody at the far end of the notification
	// service, reported there as an undeliverable recipient rather than here,
	// where the container that reached nobody is named. A mention or a
	// watcher who left is dropped for the same reason.
	//
	// AND NO REASON HERE WAKES THE ACTOR (a nil wakesActor): a lead writing
	// in their own container, like anybody saving a page they watch, has
	// nothing to learn from the change they just made.
	refused := 0
	routed := routing.Route(b.Candidates(), employs(reg, &refused), actor, nil)
	if refused > 0 {
		log.Debug("pages_unknown_recipients_dropped", "count", refused,
			"page", base.Metadata[MetaTitle])
	}
	if len(routed) == 0 {
		if offered {
			// A CHANGE ITS LEAD HAD A REASON TO KNOW ABOUT reached nobody:
			// the container has no lead (an empty `lead`), or its lead is
			// the actor, or is no longer a seat (counted above).
			log.Debug("pages_change_reaches_nobody", "container", container,
				"lead", lead, "actor", actor, "page", base.Metadata[MetaTitle])
		}
		return nil, nil
	}

	// THE RECORD'S OWN INSTANT stamps every wake it produces — see
	// [notify.Routed.WakeAt].
	at := record.CreatedAt
	out := make([]notify.Routed, 0, len(routed))
	for _, c := range routed {
		out = append(out, notify.Routed{
			Inbound: withVia(base, c.Reason),
			To:      notify.Recipient{Handle: c.Handle},
			// DERIVED, so a redelivery is recognisable as one. The feed's
			// claim is the first dedupe layer and it FAILS OPEN — a
			// coordination store that cannot be reached must not stop
			// notifications — so this is what catches what slips through.
			// See [changefeed.WakeID].
			WakeID: changefeed.WakeID(base.Metadata[MetaChangeID], c.Handle),
			WakeAt: at,
		})
	}
	return out, nil
}

// employs narrows the party registry to the one question routing asks —
// whether the company still employs a handle — and counts the handles it
// refuses, for the debug line a dropped recipient leaves.
//
// A NIL REGISTRY admits everybody, which is the honest answer for a deployment
// with no organization loaded yet.
func employs(reg *notify.Registry, refused *int) func(string) bool {
	if reg == nil {
		return nil
	}
	return func(handle string) bool {
		if _, ok := reg.ByHandle(handle); ok {
			return true
		}
		*refused++
		return false
	}
}

// LeadWorthy reports whether a change reaches the container's lead when
// nobody else was named.
//
// THREE KINDS, each a fact a lead has a reason to know: a page appearing in a
// team's container, one being trashed, and one changing status — published,
// or restored from the trash. A save, a label change or a move is not, and the
// lead's prompt has an opener for exactly these three.
func LeadWorthy(kind ChangeKind) bool {
	return kind == ChangeCreated || kind == ChangeRemoved || kind == ChangeStatus
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
	body := wakeBody(record)
	if body != "" {
		// WHO WROTE THE BODY, for the fence the spine puts it in
		// (ADR-0040): every word of a change's text is its author's — the
		// remark, the new page's opening line, the note they saved with —
		// and the record states their kind, which names them when the
		// roster no longer holds the handle. WHAT it is follows from the
		// kind of change alone ([Prompt.Provenance]), so it is not
		// stamped beside it.
		meta[notify.BodyAuthorField] = record.Actor
		meta[notify.BodyAuthorKindField] = string(record.ActorKind)
	}
	return notify.Inbound{
		Source:    Source,
		EventType: string(n.Kind),
		Sender:    record.Actor,
		Subject:   n.Title,
		Body:      body,
		Metadata:  meta,
		// AND WHO THE SENDER IS, by the record's own kind: a seat's handle
		// and a credential's login are spelled alike, and only the kind
		// tells a turn this wakes whether a colleague is asking
		// (notify.FirstPartyWriter).
		Writer: notify.FirstPartyWriter(iam.ActorKind(record.ActorKind), record.Actor),
	}
}

// wakeBody is what a woken seat reads of a change: a comment WHOLE, from the
// record that carries it, where the card's excerpt was cut from it.
//
// The card excerpt is a preview for a list of notices, whitespace collapsed
// and cut at [MaxExcerpt]; the wake prompt is the one message a seat ACTS on,
// and a remark cut at six hundred bytes there reads as the whole of what
// somebody wrote. RECOGNISED BY DERIVATION, never guessed: the comment is
// used exactly when the record's excerpt is what [excerpt] makes of it.
//
// A NEW PAGE'S wake keeps its excerpt — the page's first line — because the
// page is not a message: it can run to half a megabyte, and the wake is a
// pointer to it that its recipient opens with the knowledge-base tools.
func wakeBody(record MutationRecord) string {
	n := record.Notify
	if n == nil || n.Excerpt == "" {
		return ""
	}
	var patch PagePatch
	if json.Unmarshal(record.Mutation, &patch) != nil || patch.Comment == nil || patch.Comment.Body == nil {
		return n.Excerpt
	}
	if body := *patch.Comment.Body; excerpt(body) == n.Excerpt {
		return strings.TrimSpace(body)
	}
	return n.Excerpt
}

// excerptSource is what a change's excerpt is, by the kind of change that
// carried it — which on this surface decides it, since only a comment's change
// carries a remark: a remark on a comment, the page's opening line on a create,
// and the author's note about the change on every other kind (a save's
// message).
func excerptSource(kind ChangeKind) provenance.Source {
	switch kind {
	case ChangeComment, ChangeCommentEdited:
		return provenance.SourcePageComment
	case ChangeCreated:
		return provenance.SourcePageBody
	}
	return provenance.SourceChangeNote
}

// link is where the person a change woke opens the page: the DASHBOARD's
// address of it ([AddressPrefix]), on the dashboard's base.
//
// Never the REST route /doclet/pages/<id>, which it used to be: that answers JSON to a
// browser, and behind the guard when anonymous reads are off, so a seat that
// forwarded the link — as the shipped tool skill teaches it to forward links —
// handed a person an endpoint rather than a page. And never a base of the
// parser's own choosing: the dashboard address is the one the backlinks read
// ([Links]) and the one the dashboard's router resolves, so the three agree by
// construction.
func (p *Parser) link(pageID string) string {
	if p.baseURL == "" || pageID == "" {
		return ""
	}
	return p.baseURL + "/" + AddressPrefix + pageID
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
		return MutationRecord{}, fmt.Errorf("doclet: the delivery carries no record")
	}
	data, err := json.Marshal(body)
	if err != nil {
		return MutationRecord{}, fmt.Errorf("doclet: read the delivery: %w", err)
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
