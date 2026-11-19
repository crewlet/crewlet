package worklet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/crewlet/crewlet/internal/changefeed"
	"github.com/crewlet/crewlet/internal/decision"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/named"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/routing"
)

// The metadata keys the prompt reads back.
//
// NAMED CONSTANTS because the parser writes them and the prompt reads them: a
// typo in either silently renders an empty field, which looks exactly like a
// change that had nothing to say.
const (
	MetaTaskKey = "item_key"
	MetaTaskID  = "item_id"

	// MetaKeyCollision is "true" when [MetaTaskKey] opens ANOTHER task —
	// one that claimed the key first — so the task is reached by
	// [MetaTaskID] alone. Absent otherwise. Read through [metaAddress].
	MetaKeyCollision = "item_key_collision"
	MetaProject      = "project"
	MetaStatus       = "status"
	MetaAssignee     = "assignee"
	MetaRecordID     = "change_id"
	MetaChangeKind   = "change_kind"
	MetaTitle        = "title"
	MetaExcerpt      = "excerpt"
	MetaLate         = "late"

	// MetaDeltas is WHAT MOVED, already rendered by [changedText]: one
	// `field: from → to` line per value delta and one `field: added …; removed …`
	// line per set. Rendered here because the envelope is a string map and
	// [Notify.Fields] exists only on this side: a prompt is handed a
	// [notify.Inbound] with no route back to the record.
	MetaDeltas = "deltas"

	// MetaMoved is WHAT KINDS OF CHANGE the write made: every kind whose field
	// it moved ([Notify.movedKinds]), whatever kind its record ended with,
	// comma-separated in [ChangeKinds] order, absent where it moved none. The
	// prompt says what happened by it, as the router decides who hears by the
	// same deltas ([Notify.moved]): a hand-off written with a status is a
	// `status` record whose wake names `assignee` here too, so its new holder
	// reads that the task was assigned to them.
	MetaMoved = "moved"

	// MetaObject and MetaObjectID are WHAT the change was about, since one of
	// the two routable kinds is not a task. The prompt keys its opener on the
	// object, not the change kind: "somebody wrote your priority list" is not "a
	// task you are assigned to", and inferring the noun from the kind would take
	// a switch that grows with every kind.
	MetaObject   = "object"
	MetaObjectID = "object_id"

	// MetaVia is WHY this seat is being told: the prompt renders "you were
	// mentioned" as an ask and "you are watching this" as news.
	MetaVia = "routed_via"

	// MetaCommentID is the comment the change carried: for an ask, the id an
	// answer names in `answers`, so the prompt writes the answering call out
	// whole rather than leaving the seat to find it in a thread read.
	MetaCommentID = "comment_id"

	// MetaDecision is the [Decision] a new ask carries (question, options,
	// recommendation) as JSON: the envelope is a string map, a decision is a
	// structure, and the prompt is its one reader.
	MetaDecision = "decision"

	// MetaChoice, MetaQuestion and MetaInform are the [AnsweredDecision] an
	// answer closed: the chosen option (a [DecisionOption] as JSON, absent for a
	// prose answer), the question, and where the asker promised to report the
	// outcome (an [Inform] as JSON). The asker's wake renders them.
	MetaChoice   = "choice"
	MetaQuestion = "question"
	MetaInform   = "inform"

	// MetaDependents is the dependents a relations commit on a blocker names
	// ([Snapshot.Dependents]), by id, comma-separated in the record's order:
	// what a `blocking` wake is about beside its own item, so a wake whose
	// every dependent ended starts no turn. Read through [WakeDependents].
	MetaDependents = "dependents"
)

// WakeDependents is the dependents a wake's metadata names ([MetaDependents]),
// none for a wake that names none.
func WakeDependents(metadata map[string]string) []string {
	if metadata[MetaDependents] == "" {
		return nil
	}
	return strings.Split(metadata[MetaDependents], ",")
}

// StateNotice reports a Worklet wake that tells its seat a state rather than
// words (ADR-0162), and the reason it was routed under: one under an
// [Reason.Absorbable] reason whose change is no talk on the task and carries
// no comment. Words are never one, whatever their reason.
func StateNotice(n *types.ExternalNotification) (Reason, bool) {
	if n == nil || n.NotificationSource != Source {
		return "", false
	}
	reason := Reason(n.Metadata[MetaVia])
	if !reason.Absorbable() || ChangeKind(n.SourceEventType).Conversation() ||
		n.Metadata[MetaCommentID] != "" {
		return "", false
	}
	return reason, true
}

// Parser turns a mutation record into the notifications it implies; it
// implements [notify.Parser].
//
// The record carries its own routing snapshot, so the node that wins a delivery
// routes without reading rows it may be behind on. The parser only decodes the
// record into [Candidates], asks the party registry whether each handle is
// still a seat, and renders the survivors in the spine's shape. Every rule
// about WHO lives where it is testable without a registry: the reasons and
// their precedence in recipients.go, the mute in wake.go's snapshot, and the
// one-reason-per-handle rule, fallback chain and post-filter in
// internal/routing, which every native domain shares.
type Parser struct {
	logger *slog.Logger
}

// ParserOptions configure a parser.
type ParserOptions struct {
	// Logger is where the parser reports. Nil is the package's own
	// component logger, never silence.
	Logger *slog.Logger
}

// NewParser builds the tracker's inbound parser.
func NewParser(opts ParserOptions) *Parser {
	logger := opts.Logger
	if logger == nil {
		logger = log
	}
	return &Parser{logger: logger}
}

// Source is the integration name, matching the delivery's own.
func (p *Parser) Source() string { return Source }

// Parse reports which seats a record concerns.
func (p *Parser) Parse(ctx context.Context, w types.RawWebhook, reg *notify.Registry) ([]notify.Routed, error) {
	record, err := recordFromBody(w.Body)
	if err != nil {
		return nil, err
	}
	if record.Notify == nil {
		// A RECORD NOBODY ANNOUNCED means the feed's decision was bypassed (a
		// replayed body, a test, a second producer). Answering "nobody" rather than
		// erroring keeps the layers independent: the feed decides what to relay,
		// this decides who it is for.
		return nil, nil
	}
	if !record.Subject.Kind.Routable() {
		// A PROJECT, A CATALOGUE, A VIEW OR A TAG SET is read from its own surface
		// rather than woken into an inbox, and a counter or an alias has no
		// audience. [ObjectKind.Routable] says why the set is closed.
		return nil, nil
	}

	base := p.inbound(record)
	// FROM THE RECORD, exactly as the applier reads it, so the wake and the
	// notification rows can never disagree about who a change concerns.
	candidates := Candidates(record.Notify)
	// THE AUTHOR IS THE ONE NAME TO LEAVE OUT: a person bound to a seat
	// writes AS that seat (iam.ActorFor), so the handle their own gestures
	// land on — the watch a create leaves, the watch a comment leaves — is
	// the author the record already carries.
	heard := Heard(candidates, registryHas(reg), record.Actor)
	routed := routing.Waking(heard)
	held := routing.Held(heard, routed)
	if len(routed) == 0 && len(held) == 0 {
		p.logger.DebugContext(ctx, "tracker_change_reaches_nobody",
			"task", record.Notify.Snapshot.Key, "kind", string(record.Notify.Kind))
		return nil, nil
	}

	out := make([]notify.Routed, 0, len(routed)+len(held))
	for _, c := range held {
		// TOLD AND NOT WOKEN: the notice row is the applier's; this is the
		// held wake's record, under the reason the seat was told.
		out = append(out, notify.Routed{Inbound: notify.Inbound{Source: base.Source,
			Origin: types.TriggerOrigin{Kind: types.OriginWorkItem, ID: record.Notify.Snapshot.Key,
				Reason: string(c.Reason)}},
			To: notify.Recipient{Handle: c.Handle}, Held: true,
			WakeID: changefeed.WakeID(record.OpID, c.Handle), WakeAt: record.CreatedAt})
	}
	for _, c := range routed {
		inbound := base
		inbound.Metadata = withVia(base.Metadata, string(c.Reason))
		// WHAT STARTS THE TURN THIS WAKES: the item, under this
		// recipient's own reason.
		inbound.Origin = types.TriggerOrigin{Kind: types.OriginWorkItem,
			ID: record.Notify.Snapshot.Key, Reason: string(c.Reason)}
		// PER RECIPIENT, because the promise was the asker's alone: the
		// same answer reaches the item's watchers under their own reasons
		// and obliges none of them to post anywhere.
		inbound.Owes = Owes(inbound.Metadata)
		out = append(out, notify.Routed{
			Inbound: inbound,
			To:      notify.Recipient{Handle: c.Handle},
			// DERIVED, so a redelivery is recognisable: the feed's claim fails
			// open (an unreachable coordination store must not stop
			// notifications), and this catches what slips through, in the inbox
			// and the completion ledger.
			WakeID: changefeed.WakeID(record.OpID, c.Handle),
			// AND STAMPED WITH THE RECORD'S OWN INSTANT, which every copy
			// of this wake carries — see [notify.Routed.WakeAt].
			WakeAt: record.CreatedAt,
		})
	}
	return out, nil
}

// registryHas narrows the party registry to the one question routing asks.
//
// A HANDLE THAT IS NO LONGER A SEAT (a changed handle is a removed seat,
// ADR-0013) is dropped, since a notification addressed to nobody is one
// nothing reports. A nil registry admits everybody: no organization is loaded
// yet.
func registryHas(reg *notify.Registry) func(string) bool {
	if reg == nil {
		return nil
	}
	return func(handle string) bool {
		_, ok := reg.ByHandle(handle)
		return ok
	}
}

// inbound is the notification every recipient's copy is made from.
//
// The task fields come from the SNAPSHOT, not the subject: on the routable
// kind that is not a task the subject's id is a person's handle, which
// `get_work_item` cannot resolve, so the prompt would send the seat to read
// something that is not a task. The snapshot carries a task only when the wake
// is about one: a `prioritised` wake names the task that reached the top of
// somebody's list, and a person's own bookkeeping names none.
func (p *Parser) inbound(record MutationRecord) notify.Inbound {
	snapshot := record.Notify.Snapshot
	metadata := map[string]string{
		// WHO MADE THE CHANGE, under the actor key every parser stamps
		// ([notify.ActorField]), so the spine's self-action guard covers this
		// source too, agreeing with [Route] by asking the same reason
		// ([Prompt.WakesActor]).
		notify.ActorField: record.Actor,
		MetaTaskKey:       snapshot.Key,
		MetaTaskID:        snapshot.TaskID(record.Subject),
		MetaProject:       snapshot.Project,
		MetaStatus:        string(snapshot.Status),
		MetaAssignee:      snapshot.Assignee,
		MetaRecordID:      record.OpID,
		MetaChangeKind:    string(record.Notify.Kind),
		MetaTitle:         snapshot.Title,
		MetaObject:        string(record.Subject.Kind),
		MetaObjectID:      record.Subject.ID,
	}
	if record.Notify.Excerpt != "" {
		metadata[MetaExcerpt] = record.Notify.Excerpt
	}
	// WHAT MOVED, not only that something did: the side it moved FROM is not on
	// the task, so no tool round could recover it.
	if text := changedText(record.Notify.Fields, record.Notify.FieldsOmitted); text != "" {
		metadata[MetaDeltas] = text
	}
	if moved := record.Notify.movedKinds(); len(moved) > 0 {
		names := make([]string, len(moved))
		for i, kind := range moved {
			names[i] = string(kind)
		}
		metadata[MetaMoved] = strings.Join(names, ",")
	}
	if record.Notify.CommentID != "" {
		metadata[MetaCommentID] = record.Notify.CommentID
	}
	if len(snapshot.Dependents) > 0 {
		ids := make([]string, len(snapshot.Dependents))
		for i, party := range snapshot.Dependents {
			ids[i] = party.Task
		}
		metadata[MetaDependents] = strings.Join(ids, ",")
	}
	if decision := askedDecision(record); decision != nil {
		metadata[MetaDecision] = jsonText(decision)
	}
	if answered := record.Notify.Answered; answered != nil {
		if answered.Question != "" {
			metadata[MetaQuestion] = answered.Question
		}
		if answered.Choice != nil {
			metadata[MetaChoice] = jsonText(answered.Choice)
		}
		if answered.Inform != nil {
			metadata[MetaInform] = jsonText(answered.Inform)
		}
	}
	if record.Notify.Late {
		// THE FLAG A READER NEEDS to see why they hear of something hours old:
		// it is a repair, not the change itself.
		metadata[MetaLate] = "true"
	}
	if snapshot.KeyCollision && metadata[MetaTaskID] != "" {
		// THE KEY IS NOT THIS TASK'S ADDRESS: it opens the task that claimed it
		// first, so the prompt sends the seat to the id instead. Only beside an
		// id, or the flag would leave the prompt nothing to name.
		metadata[MetaKeyCollision] = "true"
	}
	body, source := wakeBody(record)
	if body != "" {
		// WHAT THE BODY IS AND WHO WROTE IT, for the spine's fence (ADR-0040):
		// every word is the author's, and the record's author kind is what names
		// them once the roster no longer holds the handle.
		metadata[notify.BodySourceField] = string(source)
		metadata[notify.BodyAuthorField] = record.Actor
		metadata[notify.BodyAuthorKindField] = string(record.ActorKind)
	}
	subject := subjectLine(snapshot, record.Subject, record.Notify.Kind)
	return notify.Inbound{
		Source:      Source,
		EventType:   string(record.Notify.Kind),
		Sender:      record.Actor,
		Subject:     subject.Text,
		SubjectRefs: subject.Refs,
		Body:        body,
		Metadata:    metadata,
		// WHO THE SENDER IS, by the record's own kind: the actor may be a seat's
		// handle, a credential's login or a node's name, and only the kind tells
		// the woken turn whether a colleague is asking or nobody is
		// (notify.FirstPartyWriter); and whom an agent wrote it for, which that
		// turn works for (ADR-0109).
		Writer: notify.FirstPartyWriter(iam.ActorKind(record.ActorKind), record.Actor, record.ActorApps,
			record.WorkingFor),
	}
}

// wakeBody is what a woken seat reads of a change: the WHOLE text the card's
// excerpt was cut from, wherever the record carries it, and what that text is.
//
// The excerpt, cut at [MaxExcerpt], previews a list of notices; the wake is the
// one message a seat acts on, and a comment cut there loses the "but" in its
// second paragraph. The whole text is used exactly when the record's excerpt is
// what [excerptOf] makes of it: a stated excerpt (a hand-off's reason, a
// purge's line) does not match and is shown as itself. Its source is the
// comment, the new task's description, or else a note its author wrote about
// the change ([provenance.SourceChangeNote]), the label its fence carries.
func wakeBody(record MutationRecord) (string, provenance.Source) {
	excerpt := record.Notify.Excerpt
	if excerpt == "" {
		return "", ""
	}
	var carried struct {
		Comment *Comment `json:"comment"`
		Body    string   `json:"body"`
	}
	if json.Unmarshal(record.Mutation, &carried) != nil {
		return excerpt, ExcerptSource(record.Notify.Kind)
	}
	type candidate struct {
		text   string
		source provenance.Source
	}
	candidates := []candidate{}
	if c := carried.Comment; c != nil {
		body := strings.TrimSpace(c.Body)
		if answered := record.Notify.Answered; answered != nil && answered.Choice != nil {
			body = choiceExcerpt(answered.Choice.Label, body)
		}
		candidates = append(candidates, candidate{body, provenance.SourceWorkComment})
	}
	if record.Notify.Kind == ChangeCreated {
		candidates = append(candidates, candidate{
			strings.TrimSpace(carried.Body), provenance.SourceWorkDescription,
		})
	}
	for _, whole := range candidates {
		if whole.text != "" && excerptOf(whole.text) == excerpt {
			return whole.text, whole.source
		}
	}
	return excerpt, ExcerptSource(record.Notify.Kind)
}

// ExcerptSource is what a change's excerpt is, by the kind of change that
// carried it: the cut of a comment on a comment's change, of a description on
// a create, and otherwise the note its author wrote about the change (a
// hand-off's reason, a purge's line, a re-prioritisation's line).
//
// ONE RULE for every reader of an excerpt (this wake, and the activity feed, a
// task's history and a person's inbox, which hold no payload), so a change's
// text is labelled alike wherever a seat reads it.
func ExcerptSource(kind ChangeKind) provenance.Source {
	switch kind {
	case ChangeComment, ChangeCommentEdited:
		return provenance.SourceWorkComment
	case ChangeCreated:
		return provenance.SourceWorkDescription
	}
	return provenance.SourceChangeNote
}

// askedDecision is the decision a record's NEW ask carries, or nil.
//
// READ FROM THE PAYLOAD, where the comment travels whole: a patch's `comment`
// or the question a create was filed as ([TaskCreate]), under the same key.
// Only on the kinds that WRITE an ask: an edit re-sends the whole comment, and
// rendering its options again would ask the question twice.
func askedDecision(record MutationRecord) *decision.Decision {
	switch record.Notify.Kind {
	case ChangeComment, ChangeCreated:
	default:
		return nil
	}
	var carried struct {
		Comment *Comment `json:"comment"`
	}
	if json.Unmarshal(record.Mutation, &carried) != nil || carried.Comment == nil ||
		carried.Comment.Ask == "" {
		return nil
	}
	return carried.Comment.Decision
}

// jsonText is a value as the compact JSON a metadata string carries. Every
// value handed here is one of this package's own plain structs, which cannot
// fail to encode.
func jsonText(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(raw)
}

// subjectLine is the one line a recipient sees before they read anything.
//
// IT NAMES THE OBJECT, since "a task" for a person's priority list would be
// false, and digests coalesce on the subject. AND THE TASK BY ITS ADDRESS
// ([Snapshot.Address]), the id where another task claimed its key first: it
// is the first reference a seat is shown, and a key there lands it on the
// claimant. The task is named as one (package named), by its id.
func subjectLine(snapshot Snapshot, subject Subject, kind ChangeKind) named.Line {
	line := named.Words(objectLabel(subject))
	if key := snapshot.Address(subject); key != "" {
		line = named.Words("").Name(named.Item(snapshot.TaskID(subject), key))
	}
	if snapshot.Title == "" {
		return line.Words(fmt.Sprintf(": %s", kind))
	}
	return line.Words(fmt.Sprintf(" %s: %s", kind, snapshot.Title))
}

// objectLabel is what a non-task subject is CALLED in one line.
//
// What it is FOR rather than its id: a recipient reading
// "alice: prioritised" learns nothing they can act on.
func objectLabel(subject Subject) string {
	if subject.Kind == KindPerson {
		return "your priorities"
	}
	return "a task"
}

// withVia copies the metadata with this recipient's own reason on it.
//
// A COPY: recipients of one record share the map, so writing in place would
// give all of them the last reason rendered.
func withVia(base map[string]string, via string) map[string]string {
	out := make(map[string]string, len(base)+1)
	for k, v := range base {
		out[k] = v
	}
	out[MetaVia] = via
	return out
}

// recordFromBody reads the record back out of a delivery.
//
// THROUGH JSON AND THEN [Decode]: the body arrives as a map, as every parser's
// webhook does, and re-marshalling lets the record's two-pass decode keep what
// a relaying build did not understand.
//
// A RECORD AT A NEWER VERSION IS DEFERRED to a peer on that build
// ([notify.Defer]), never refused: the notify service acks a refusal on the
// ONE group every node takes inbound mail from, so the wake would be lost to
// the fleet during a roll. It is the feed's future-version arm ([Translator])
// on this side of the relay.
func recordFromBody(body map[string]any) (MutationRecord, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return MutationRecord{}, fmt.Errorf("worklet: render a delivery back "+
			"into a record: %w", err)
	}
	record, err := Decode(raw)
	if err != nil {
		var future *ErrFutureVersion
		if errors.As(err, &future) {
			return MutationRecord{}, notify.Defer(notify.DeferNewerRecord, err)
		}
		return MutationRecord{}, fmt.Errorf("worklet: read the record out of a "+
			"delivery: %w", err)
	}
	return record, nil
}
