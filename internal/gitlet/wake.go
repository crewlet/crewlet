package gitlet

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/crewlet/crewlet/internal/changefeed"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/routing"
)

// The metadata keys a code wake carries, which the prompt reads back
// ([Prompt]). Named, because the parser writes them and the prompt reads
// them, and a typo in either is a field that silently renders empty.
const (
	// MetaRecordID is the record the wake was derived from ([RecordID]).
	MetaRecordID = "record_id"
	// MetaChangeID and MetaChangeRef are the change the wake is about: its
	// id, which the conversation keys on, and its name (web!42), which a
	// seat passes to code_get_change.
	MetaChangeID  = "change_id"
	MetaChangeRef = "change_ref"
	// MetaRepo and MetaTitle are its repository's name and its title as the
	// record leaves them.
	MetaRepo  = "repo"
	MetaTitle = "title"
	// MetaVia is WHY this seat hears: one [Reason].
	MetaVia = "routed_via"
	// MetaVerdict is a review's verdict, on a wake a review sent.
	MetaVerdict = "verdict"
	// MetaNotClosed is why a merged change's work item was not closed, on
	// the wake a close_settled sent for a close that ended not_closed
	// ([ChangeRouting.NotClosed]).
	MetaNotClosed = "not_closed"
	// MetaWorkBackend, MetaWorkID, MetaWorkKey and MetaWorkProject are the
	// work item the change implements ([ChangeRouting.WorkItem]), each absent
	// where it carries none.
	MetaWorkBackend = "work_backend"
	MetaWorkID      = "work_id"
	MetaWorkKey     = "work_key"
	MetaWorkProject = "work_project"
)

// Parser turns a code record the feed relayed into the wakes it implies.
//
// It implements [notify.Parser]. EVERYTHING IT NEEDS IS IN THE RECORD: the
// routing snapshot its writer decided ([ChangeRouting]), read through the one
// rule every native domain routes by (internal/routing), so the node that wins
// a delivery routes without reading a row it may be behind on.
type Parser struct {
	logger *slog.Logger
}

// ParserOptions configure a parser.
type ParserOptions struct {
	// Logger is where the parser reports. Nil is the package's own feed
	// logger, never silence.
	Logger *slog.Logger
}

// NewParser builds Gitlet's inbound parser.
func NewParser(opts ParserOptions) *Parser {
	logger := opts.Logger
	if logger == nil {
		logger = logging.Get("gitlet.feed")
	}
	return &Parser{logger: logger}
}

// Source is the integration name, matching the delivery's own: [Source].
func (p *Parser) Source() string { return Source }

// Parse reports which seats a code record wakes.
//
// # The rule, in order, change by change
//
//  1. Whom the record concerns is [Concerns] over its routings — the same set
//     the applier writes a notice row each for — and the actor step is each
//     routing's own: a push record carries a routing per change it merges or
//     opens, each naming its own pusher, so nobody is told of a change they
//     made ([Concern.Hears]).
//  2. internal/routing's [routing.Route] asks the roster about the rest, and
//     keeps the repository lead's copy only where nobody ordinary survived —
//     PER CHANGE, as [Concerns]' add-once rule is: a record that tells of two
//     changes is two answers, and somebody hearing of one change does not
//     drop the lead's copy of the other.
//  3. A PERSON is dropped last: a person reads their code inbox, composed from
//     the notice the applier wrote them, and is never queue-woken. Dropped
//     AFTER the roster's filter, so a change a person hears about is one
//     somebody heard about, and the lead's fallback copy does not wake a seat
//     for a review a person was asked for.
//
// THE WAKE ID IS DERIVED from the record, the change and the recipient
// ([changefeed.WakeID] over [NoticeID]), so a redelivery — the feed's claim
// fails open — collapses into the wake it already made, and a seat two changes
// of one batched record concern is woken about each.
func (p *Parser) Parse(ctx context.Context, w types.RawWebhook, reg *notify.Registry) ([]notify.Routed, error) {
	record, err := recordFromBody(w.Body)
	if err != nil {
		return nil, err
	}
	payload, err := DecodeMutation(record)
	if err != nil {
		return nil, err
	}
	concerns := Concerns(RoutingsOf(payload))
	if len(concerns) == 0 {
		// A RECORD THAT CONCERNS NOBODY reaching the parser at all means the
		// feed's own decision was bypassed — a replayed body, a second
		// producer. "Nobody" keeps the two layers independent.
		return nil, nil
	}
	// THE TRIGGER IS THE RECORD'S POSITION, which is what [RecordID] falls
	// back to for a record with no operation id — the same identity the
	// applier's notice row and the feed's claim name.
	recordID := record.OpID
	if recordID == "" {
		recordID = w.Trigger
	}
	var out []notify.Routed
	for _, change := range byChange(concerns) {
		byHandle := make(map[string]Concern, len(change))
		candidates := make([]routing.Candidate[Reason], 0, len(change))
		for _, c := range change {
			if !c.Hears() {
				continue
			}
			byHandle[c.Handle] = c
			candidates = append(candidates, c.Candidate)
		}
		// NO ACTOR AND NO REASON THAT WAKES ONE: the actor step was each
		// routing's own above (Concern.Hears), which already kept the one
		// reason that is news to whoever made the record — a failed check of
		// the head they pushed ([Reason.WakesActor]).
		for _, c := range routing.Route(candidates, registryHas(reg), "", nil) {
			concern := byHandle[c.Handle]
			if concern.Change.Person(c.Handle) {
				continue
			}
			out = append(out, notify.Routed{
				Inbound: inboundOf(record, payload, recordID, concern),
				To:      notify.Recipient{Handle: c.Handle},
				WakeID:  changefeed.WakeID(NoticeID(recordID, concern.Change.ChangeID), c.Handle),
				// THE RECORD'S OWN INSTANT, the same on every copy of the
				// wake — see [notify.Routed.WakeAt].
				WakeAt: record.CreatedAt,
			})
		}
	}
	if len(out) == 0 {
		p.logger.DebugContext(ctx, "code_change_wakes_nobody", "record", recordID, "op", string(record.Op))
		return nil, nil
	}
	return out, nil
}

// byChange is concerns cut into each change's, in [Concerns]' order: one
// change's concerns are contiguous there, since it offers a change's reasons
// whole before the next change's.
func byChange(concerns []Concern) [][]Concern {
	var out [][]Concern
	for i, c := range concerns {
		if i == 0 || c.Change.ChangeID != concerns[i-1].Change.ChangeID {
			out = append(out, nil)
		}
		out[len(out)-1] = append(out[len(out)-1], c)
	}
	return out
}

// registryHas narrows the party registry to the one question routing asks:
// whether the company still employs a handle. A nil registry admits everybody,
// the honest answer for a deployment with no organization loaded yet.
func registryHas(reg *notify.Registry) func(string) bool {
	if reg == nil {
		return nil
	}
	return func(handle string) bool {
		_, ok := reg.ByHandle(handle)
		return ok
	}
}

// inboundOf is the wake one concern's seat is given, PER RECIPIENT: its
// reason, which the wake's metadata and its origin both carry, is the
// recipient's own.
func inboundOf(record MutationRecord, payload any, recordID string, c Concern) notify.Inbound {
	r := c.Change
	meta := map[string]string{
		notify.ActorField: r.Actor,
		MetaRecordID:      recordID,
		MetaChangeID:      r.ChangeID,
		MetaChangeRef:     r.ChangeRef,
		MetaRepo:          r.RepoName,
		MetaTitle:         r.Title,
		MetaVia:           string(c.Reason),
	}
	if r.Verdict != "" {
		meta[MetaVerdict] = string(r.Verdict)
	}
	if r.NotClosed != "" {
		meta[MetaNotClosed] = r.NotClosed
	}
	if item := r.WorkItem; item != nil && item.ID != "" {
		meta[MetaWorkBackend], meta[MetaWorkID] = string(item.Backend), item.ID
		meta[MetaWorkKey], meta[MetaWorkProject] = item.Key, item.Project
	}
	body, source := wakeText(payload, r.ChangeID)
	if body != "" {
		// WHAT THE BODY IS AND WHO WROTE IT, for the fence the spine puts it
		// in (ADR-0040): every word of a change's text is its writer's —
		// the description they filed, the reason they gave, their comment,
		// their review — and the record's actor is that writer.
		meta[notify.BodySourceField] = string(source)
		meta[notify.BodyAuthorField] = r.Actor
	}
	subject := r.ChangeRef
	if r.Title != "" {
		subject += ": " + r.Title
	}
	return notify.Inbound{
		Source:    Source,
		EventType: string(record.Op),
		Sender:    r.Actor,
		Subject:   subject,
		Body:      body,
		Metadata:  meta,
		// WHO WROTE WHAT THE WAKE ANNOUNCES, by the routing's own record of
		// it: a person as the seat they hold, a seat as itself — a revision's
		// facts and a landing, which the home records, as whoever pushed the
		// revision and the push the home found merging the change — and
		// nobody, the engine, for the home's other records. The record's ACTOR
		// is the deciding node for every code record, so the routing's actor
		// is the writer here.
		Writer: writerOf(r),
		// WHAT STARTS THE TURN THIS WAKES: the change, as the Gitlet screens
		// name it, under this recipient's own reason — so the turn a review
		// request or a failed check started is found from the change
		// (`GET /turns?origin=code_change:<repo>!<number>`), as a chat
		// message's turn is from the message.
		Origin: types.TriggerOrigin{Kind: types.OriginCodeChange, ID: r.ChangeRef, Reason: string(c.Reason)},
	}
}

// writerOf is who wrote the record r routes, as a first-party notice states
// them ([notify.FirstPartyWriter]): the kind the routing records, never the
// spelling of the handle.
//
// NO APPS, because no record here is a credential's own: a code record is
// made as a seat — an agent's, or a person's through the token bound to them —
// and a token bound to nobody asks nothing of this domain (LifecycleRequest's
// Actor), so a writer here is never one a turn is worked for by its apps.
//
// AND NOBODY AN AGENT WROTE FOR, yet: a change's routing carries no carrier,
// so the turn a seat's code record wakes works for that seat (ADR-0109's edge
// until crewlet#798's piece 4c carries one on code's records).
func writerOf(r *ChangeRouting) *types.NotificationWriter {
	switch {
	case r.Actor == "":
		return notify.FirstPartyWriter(iam.ActorSystem, "", nil, nil)
	case r.Person(r.Actor):
		return notify.FirstPartyWriter(iam.ActorHuman, r.Actor, nil, nil)
	}
	return notify.FirstPartyWriter(iam.ActorAgent, r.Actor, nil, nil)
}

// wakeText is what a woken seat reads of what the record says about change:
// a comment, a review's summary, an ask's note, an open's or an edit's
// description, a close's reason — WHOLE, where a notice row keeps an excerpt
// ([MaxNoticeExcerpt]): the wake is the one message a seat acts on, and a
// review cut at six hundred bytes reads as the whole of it. Nothing for a
// record that says nothing of its own, a push's among them, and nothing for a
// payload about another change.
func wakeText(payload any, change string) (string, provenance.Source) {
	var (
		text, about string
		source      provenance.Source
	)
	switch p := payload.(type) {
	case ChangeOpen:
		text, about, source = p.Body, p.ChangeID, provenance.SourceChangeDescription
	case ChangeEdit:
		if p.Body != nil {
			text = *p.Body
		}
		about, source = p.ChangeID, provenance.SourceChangeDescription
	case ChangeClose:
		text, about, source = p.Reason, p.ChangeID, provenance.SourceChangeNote
	case ReviewerRequest:
		text, about, source = p.Note, p.ChangeID, provenance.SourceChangeNote
	case Comment:
		text, about, source = p.Body, p.ChangeID, provenance.SourceChangeComment
	case Review:
		text, about, source = p.Summary, p.ChangeID, provenance.SourceChangeReview
	}
	if about != change {
		return "", ""
	}
	return strings.TrimSpace(text), source
}

// recordFromBody recovers the record the feed relayed, through the domain's
// own decoder, so a record a newer build wrote reaches the parser with
// everything it carried.
func recordFromBody(body map[string]any) (MutationRecord, error) {
	if len(body) == 0 {
		return MutationRecord{}, fmt.Errorf("gitlet: the delivery carries no record")
	}
	data, err := json.Marshal(body)
	if err != nil {
		return MutationRecord{}, fmt.Errorf("gitlet: read the delivery: %w", err)
	}
	return Decode(data)
}
