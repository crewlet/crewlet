package gitlet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/crewlet/crewlet/internal/changefeed"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/named"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/routing"
)

// The metadata keys a code wake carries and [Prompt] reads back. Named, since
// a typo on either side is a field that silently renders empty.
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
	// MetaNotClosed is why a merged change's work item was not closed, on a
	// close_settled wake ([ChangeRouting.NotClosed]).
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
// It implements [notify.Parser]. Everything it needs is in the record (the
// routing its writer decided, [ChangeRouting], routed by internal/routing), so
// the node that wins a delivery reads no row it may be behind on.
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

// Parse reports which seats a code record wakes, change by change.
//
//  1. Whom it concerns is [Concerns] over its routings (the applier's notice
//     set), each dropping its own actor, so nobody hears of their own change.
//  2. [routing.Route] asks the roster about the rest, keeping the repository
//     lead's copy only where nobody ordinary survived, per change.
//  3. A person (who reads the inbox, never queue-woken) is dropped last, after
//     the roster's filter, so the lead gets no fallback copy of a person's ask.
//
// The wake id is [changefeed.WakeID] over [NoticeID] and the recipient, so a
// redelivery (the feed's claim fails open) collapses into the wake it made, and
// a seat two changes of one record concern is woken about each.
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
		// The feed's own decision was bypassed (a replayed body, a second
		// producer); answering nobody keeps the two layers independent.
		return nil, nil
	}
	// The record's position is [RecordID]'s fallback for a record with no
	// op id: the identity the notice row and the feed's claim name too.
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
		// No actor here: Concern.Hears already dropped each routing's own,
		// keeping only [Reason.WakesActor] (a failed check of their head).
		for _, c := range routing.Route(candidates, registryHas(reg), "", nil) {
			concern := byHandle[c.Handle]
			if concern.Change.Person(c.Handle) {
				continue
			}
			out = append(out, notify.Routed{
				Inbound: inboundOf(record, payload, recordID, concern),
				To:      notify.Recipient{Handle: c.Handle},
				WakeID:  changefeed.WakeID(NoticeID(recordID, concern.Change.ChangeID), c.Handle),
				// The record's own instant, the same on every copy
				// ([notify.Routed.WakeAt]).
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

// inboundOf is the wake one concern's seat is given, per recipient: the
// reason its metadata and origin carry is the recipient's own.
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
		// For the spine's fence (ADR-0040): a change's text is all its
		// writer's, and the record's actor is that writer.
		meta[notify.BodySourceField] = string(source)
		meta[notify.BodyAuthorField] = r.Actor
	}
	subject := named.Words("").Name(named.Ref{Kind: named.Change, ID: r.ChangeRef, Text: r.ChangeRef})
	if r.Title != "" {
		subject = subject.Words(": " + r.Title)
	}
	return notify.Inbound{
		Source:      Source,
		EventType:   string(record.Op),
		Sender:      r.Actor,
		Subject:     subject.Text,
		SubjectRefs: subject.Refs,
		Body:        body,
		Metadata:    meta,
		// By the routing's actor, never the record's, which is the deciding
		// node for every code record ([writerOf]).
		Writer: writerOf(record.Op, r),
		// The change under this recipient's reason, so the turn is found
		// from it (`GET /turns?origin=code_change:<repo>!<number>`).
		Origin: types.TriggerOrigin{Kind: types.OriginCodeChange, ID: r.ChangeRef, Reason: string(c.Reason)},
	}
}

// writerOf is who wrote the record r routes ([notify.FirstPartyWriter]): the
// kind the routing records, never the handle's spelling. Never apps: a code
// record is made as a seat, and a token bound to nobody asks nothing of this
// domain (LifecycleRequest's Actor).
//
// An agent carries whom it wrote for ([ChangeRouting.WorkingFor], ADR-0109),
// as an engine record naming an earlier actor keeps that act's (concerns.go's
// head, "Whom the actor acted for"); a person writes only for themselves. With
// no actor, only the queue's records about an enqueue ([carriesAnEnqueue])
// carry one, the engine writing for them (concerns.go's head, "The one
// exception"); any other actorless record is the engine's alone.
func writerOf(op OpKind, r *ChangeRouting) *types.NotificationWriter {
	switch {
	case r.Actor == "" && carriesAnEnqueue(op):
		return notify.FirstPartyWriter(iam.ActorSystem, "", nil, r.WorkingFor)
	case r.Actor == "":
		return notify.FirstPartyWriter(iam.ActorSystem, "", nil, nil)
	case r.Person(r.Actor):
		return notify.FirstPartyWriter(iam.ActorHuman, r.Actor, nil, nil)
	}
	return notify.FirstPartyWriter(iam.ActorAgent, r.Actor, nil, r.WorkingFor)
}

// carriesAnEnqueue reports whether op is a merge queue record that names no
// actor yet carries whom an entry's enqueue was made for (an ejection, a landed
// change's item close settled): the two whose decides hand [routingFor] that
// carrier ([concerning.enqueuedFor]).
func carriesAnEnqueue(op OpKind) bool { return op == OpEject || op == OpCloseSettled }

// wakeText is what a woken seat reads of what the record says about change,
// whole, where a notice row keeps an excerpt ([MaxNoticeExcerpt]), since the
// wake is the one message a seat acts on and a cut review reads as the whole.
// Nothing for a record with no text of its own (a push) or about another change.
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

// recordFromBody recovers the record the feed relayed through the domain's
// own decoder, so a newer build's record arrives with everything it carried.
//
// A record at a newer version is deferred to a peer on that build
// ([notify.Defer]), never refused: notify acks a refusal on the one group every
// node takes inbound mail from, so an older node winning the delivery during a
// roll would lose the wake fleet-wide. It is the feed's future-version arm
// ([Translator]) on this side of the relay.
func recordFromBody(body map[string]any) (MutationRecord, error) {
	if len(body) == 0 {
		return MutationRecord{}, fmt.Errorf("gitlet: the delivery carries no record")
	}
	data, err := json.Marshal(body)
	if err != nil {
		return MutationRecord{}, fmt.Errorf("gitlet: read the delivery: %w", err)
	}
	record, err := Decode(data)
	var future *ErrFutureVersion
	if errors.As(err, &future) {
		return MutationRecord{}, notify.Defer(notify.DeferNewerRecord, err)
	}
	return record, err
}
