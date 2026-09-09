package tracker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/crewlet/crewlet/internal/changefeed"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/notify"
)

// The metadata keys the prompt reads back.
//
// NAMED CONSTANTS because the parser writes them and the prompt reads them,
// and a typo in either is a field that silently renders empty — which looks
// exactly like a change that had nothing to say.
const (
	MetaTaskKey    = "item_key"
	MetaTaskID     = "item_id"
	MetaProject    = "project"
	MetaStatus     = "status"
	MetaAssignee   = "assignee"
	MetaRecordID   = "change_id"
	MetaChangeKind = "change_kind"
	MetaTitle      = "title"
	MetaExcerpt    = "excerpt"
	MetaLate       = "late"

	// MetaVia is WHY this seat is being told, and it is not decoration:
	// "you were mentioned" and "you are watching this" ask for different
	// things, and the prompt renders them as an ask and as news.
	MetaVia = "routed_via"
)

// Parser turns a mutation record into the notifications it implies.
//
// It implements [notify.Parser]. EVERYTHING IT NEEDS IS IN THE PAYLOAD: the
// record carries its own routing snapshot, so the node that wins a delivery
// routes without reading rows it may be behind on.
//
// # What it adds to [Candidates], which does the actual deciding
//
// Three things, and only three: it turns a decoded record into the candidate
// set, it asks the party registry whether each handle is still a seat, and it
// renders what survives as the spine's own shape. Every rule about WHO — the
// twenty reasons, their precedence, the mute, the fallback — lives in
// recipients.go, where it can be exercised without a registry, a webhook or an
// organization.
type Parser struct {
	logger *slog.Logger

	// batched says this delivery is one of a bulk gesture's, which is what
	// suppresses the fan-out to watchers: a bulk edit of sixty-four tasks
	// would otherwise wake everybody watching any of them, sixty-four
	// times, about one person's afternoon.
	batched bool
}

// ParserOptions configure a parser.
type ParserOptions struct {
	Logger *slog.Logger
}

// NewParser builds the tracker's inbound parser.
func NewParser(opts ParserOptions) *Parser {
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
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
		// A RECORD NOBODY ANNOUNCED reaching the parser at all means the
		// feed's own decision was bypassed — a replayed body, a test, a
		// second producer. Answering "nobody" rather than erroring keeps
		// the two layers independent: the feed decides what to relay and
		// this decides who it is for, and neither depends on the other
		// having decided the same way.
		return nil, nil
	}
	if record.Subject.Kind != KindTask {
		// EVERY ROUTING RULE HERE IS ABOUT A TASK — the assignee, the
		// watchers, the dependents, the lead. A project or catalogue
		// edit is announced through its own surfaces rather than woken
		// into somebody's inbox.
		return nil, nil
	}

	base := p.inbound(record)
	candidates := Candidates(record.Notify, p.batched)
	routed := Route(candidates, registryHas(reg), record.Actor)
	if len(routed) == 0 {
		p.logger.DebugContext(ctx, "tracker_change_reaches_nobody",
			"task", record.Notify.Snapshot.Key, "kind", string(record.Notify.Kind))
		return nil, nil
	}

	out := make([]notify.Routed, 0, len(routed))
	for _, c := range routed {
		inbound := base
		inbound.Metadata = withVia(base.Metadata, string(c.Reason))
		out = append(out, notify.Routed{
			Inbound: inbound,
			To:      notify.Recipient{Handle: c.Handle},
			// DERIVED, so a redelivery is recognisable as one. The
			// feed's claim is the first dedupe layer and it FAILS OPEN
			// — a coordination store that cannot be reached must not
			// stop notifications — so this is what catches what slips
			// through, in the inbox and in the completion ledger.
			WakeID: changefeed.WakeID(record.OpID, c.Handle),
		})
	}
	return out, nil
}

// registryHas narrows the party registry to the one question routing asks.
//
// A HANDLE THAT IS NO LONGER A SEAT is somebody who left, or a watcher
// recorded before a rename. It is dropped rather than routed, because a
// notification addressed to nobody is one nothing reports. A nil registry
// admits everybody, which is the honest answer for a deployment with no
// organization loaded yet.
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
func (p *Parser) inbound(record MutationRecord) notify.Inbound {
	snapshot := record.Notify.Snapshot
	metadata := map[string]string{
		MetaTaskKey:    snapshot.Key,
		MetaTaskID:     record.Subject.ID,
		MetaProject:    snapshot.Project,
		MetaStatus:     string(snapshot.Status),
		MetaAssignee:   snapshot.Assignee,
		MetaRecordID:   record.OpID,
		MetaChangeKind: string(record.Notify.Kind),
		MetaTitle:      snapshot.Title,
	}
	if record.Notify.Excerpt != "" {
		metadata[MetaExcerpt] = record.Notify.Excerpt
	}
	if record.Notify.Late {
		// THE FLAG A READER NEEDS to understand why they are hearing
		// about something that happened hours ago: it is a repair, not
		// the change itself.
		metadata[MetaLate] = "true"
	}
	return notify.Inbound{
		Source:    Source,
		EventType: string(record.Notify.Kind),
		Sender:    record.Actor,
		Subject:   subjectLine(snapshot, record.Notify.Kind),
		Body:      record.Notify.Excerpt,
		Metadata:  metadata,
	}
}

// subjectLine is the one line a recipient sees before they read anything.
func subjectLine(snapshot Snapshot, kind ChangeKind) string {
	key := snapshot.Key
	if key == "" {
		key = "a task"
	}
	if snapshot.Title == "" {
		return fmt.Sprintf("%s: %s", key, kind)
	}
	return fmt.Sprintf("%s %s: %s", key, kind, snapshot.Title)
}

// withVia copies the metadata with this recipient's own reason on it.
//
// A COPY, because every recipient of one record gets a different reason and
// they share the map otherwise — writing in place would give all of them
// whichever one was rendered last.
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
// THROUGH JSON AND THEN THE RECORD'S OWN DECODER. The body arrives as a map
// because that is what every other parser's webhook carries, and re-marshalling
// it is what lets the record's own two-pass decode run — which is what makes a
// body relayed by a build that did not understand every field still yield
// everything the writer wrote.
func recordFromBody(body map[string]any) (MutationRecord, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return MutationRecord{}, fmt.Errorf("tracker: render a delivery back "+
			"into a record: %w", err)
	}
	record, err := Decode(raw)
	if err != nil {
		return MutationRecord{}, fmt.Errorf("tracker: read the record out of a "+
			"delivery: %w", err)
	}
	return record, nil
}
