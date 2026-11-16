package artifactsdomain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/changefeed"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/routing"
)

// The metadata keys a comment wake carries, which the prompt reads back
// ([Prompt]). Named, because the parser writes them and the prompt reads them.
const (
	// MetaVia is why this seat hears: one [NotifyReason].
	MetaVia = "routed_via"
	// MetaAddressed is "true" where the remark speaks to this seat.
	MetaAddressed = "addressed"
	// MetaArtifact, MetaThread and MetaRemark are the artifact, the thread
	// and the remark the record wrote, each absent where it names none.
	MetaArtifact = "artifact"
	MetaThread   = "thread"
	MetaRemark   = "remark"
	// MetaKind is the artifact's kind, MetaVersion the version the thread was
	// made on and MetaHead the head when the record was written.
	MetaKind    = "artifact_kind"
	MetaVersion = "version"
	MetaHead    = "head"
	// MetaLabel is the thread's place as a person reads it, and MetaQuote
	// the page's text it quotes.
	MetaLabel = "label"
	MetaQuote = "quote"
	// MetaResolvedVersion is the version a resolve cites.
	MetaResolvedVersion = "resolved_version"
	// MetaRecordID is the record the wake was derived from.
	MetaRecordID = "record_id"
)

// Parser turns a comment record the feed relayed into its wakes, from the
// routing snapshot the record carries alone, so any node routes it without
// reading a row.
type Parser struct {
	logger *slog.Logger
}

// ParserOptions configure a parser.
type ParserOptions struct {
	// Logger is where the parser reports. Nil is the package's own feed
	// logger, never silence.
	Logger *slog.Logger
}

// NewParser builds the artifacts log's inbound parser.
func NewParser(opts ParserOptions) *Parser {
	logger := opts.Logger
	if logger == nil {
		logger = logging.Get("artifactsdomain.feed")
	}
	return &Parser{logger: logger}
}

// Source is the integration name, matching the delivery's own: [Source].
func (p *Parser) Source() string { return Source }

// Parse wakes each agent the snapshot names that the company still employs,
// never its author; the wake id is derived from the record and the recipient,
// so a redelivery collapses into the wake it made.
func (p *Parser) Parse(ctx context.Context, w types.RawWebhook, reg *notify.Registry) ([]notify.Routed, error) {
	record, err := recordFromBody(w.Body)
	if err != nil {
		return nil, err
	}
	if !wakesAnybody(record.Subject.Kind, record.Op) {
		return nil, nil
	}
	payload, err := DecodeMutation(record)
	if err != nil {
		return nil, err
	}
	snapshot, err := notifyOfRecord(record)
	if err != nil || snapshot == nil || len(snapshot.Agents) == 0 {
		// A RECORD THAT WAKES NOBODY reaching the parser means the feed's
		// own decision was bypassed: "nobody" keeps the two layers apart.
		return nil, err
	}
	recordID := record.OpID
	if recordID == "" {
		recordID = w.Trigger
	}
	var b routing.Builder[NotifyReason]
	for _, r := range snapshot.Agents {
		b.Add(r.Seat, r.Reason, r.Addressed)
	}
	var out []notify.Routed
	for _, c := range routing.Route(b.Candidates(), registryHas(reg), record.Actor, nil) {
		out = append(out, notify.Routed{
			Inbound: inboundOf(record, payload, *snapshot, recordID, c),
			To:      notify.Recipient{Handle: c.Handle},
			WakeID:  changefeed.WakeID(recordID, c.Handle),
			// THE RECORD'S OWN INSTANT, the same on every copy of the wake.
			WakeAt: record.CreatedAt,
		})
	}
	if len(out) == 0 {
		p.logger.DebugContext(ctx, "artifact_comment_wakes_nobody", "record", recordID,
			"op", string(record.Op))
		return nil, nil
	}
	return out, nil
}

// registryHas narrows the party registry to whether the company still employs
// a handle. A nil registry admits everybody, the honest answer for a
// deployment with no organization loaded yet.
func registryHas(reg *notify.Registry) func(string) bool {
	if reg == nil {
		return nil
	}
	return func(handle string) bool {
		_, ok := reg.ByHandle(handle)
		return ok
	}
}

// inboundOf is the wake one recipient is given: its own reason and
// addressing, the record's remark as the body, and what the snapshot says of
// where the comment was made.
func inboundOf(record MutationRecord, payload any, n CommentNotify, recordID string,
	c routing.Candidate[NotifyReason]) notify.Inbound {

	artifact, thread, _ := SplitCommentID(record.Subject.ID)
	meta := map[string]string{
		notify.ActorField: record.Actor,
		MetaRecordID:      recordID,
		MetaVia:           string(c.Reason),
		MetaArtifact:      artifact,
		MetaThread:        thread,
	}
	if c.Addressed {
		meta[MetaAddressed] = "true"
	}
	subject := "an artifact"
	if a := n.About; a != nil {
		if a.Title != "" {
			subject = a.Title
		}
		meta[MetaKind], meta[MetaLabel] = string(a.Kind), a.Label
		if a.Version > 0 {
			meta[MetaVersion] = strconv.Itoa(a.Version)
		}
		if a.Head > 0 {
			meta[MetaHead] = strconv.Itoa(a.Head)
		}
		if a.Quote != "" {
			meta[MetaQuote] = a.Quote
		}
	}
	var body string
	switch p := payload.(type) {
	case CommentOpenPayload:
		body, meta[MetaRemark] = p.Body, p.Remark
	case CommentReplyPayload:
		body, meta[MetaRemark] = p.Body, p.Remark
	case CommentStatusPayload:
		if p.Version > 0 {
			meta[MetaResolvedVersion] = strconv.Itoa(p.Version)
		}
	}
	body = strings.TrimSpace(body)
	if body != "" {
		// WHAT THE BODY IS AND WHO WROTE IT, for the fence the spine puts it
		// in (ADR-0040): a remark is every word its author's.
		meta[notify.BodySourceField] = string(provenance.SourceArtifactComment)
		meta[notify.BodyAuthorField] = record.Actor
		meta[notify.BodyAuthorKindField] = string(record.ActorKind)
	}
	return notify.Inbound{
		Source:    Source,
		EventType: string(record.Op),
		Sender:    record.Actor,
		Subject:   subject,
		Body:      body,
		Metadata:  meta,
		// WHO WROTE THE RECORD, by its own kind, and whom an agent wrote it
		// for, which the turn this wakes works for (ADR-0109).
		Writer: notify.FirstPartyWriter(record.ActorKind, record.Actor, nil, n.WorkingFor),
	}
}

// recordFromBody recovers the record the feed relayed, through the domain's
// own decoder. A record at a version a newer build writes is DEFERRED to a
// peer on that build ([notify.Defer]), never refused (crewlet#993).
func recordFromBody(body map[string]any) (MutationRecord, error) {
	if len(body) == 0 {
		return MutationRecord{}, errors.New("artifactsdomain: the delivery carries no record")
	}
	data, err := json.Marshal(body)
	if err != nil {
		return MutationRecord{}, fmt.Errorf("artifactsdomain: read the delivery: %w", err)
	}
	record, err := Decode(data)
	var future *ErrFutureVersion
	if errors.As(err, &future) {
		return MutationRecord{}, notify.Defer(notify.DeferNewerRecord, err)
	}
	return record, err
}
