package advisordomain

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/statelog"
)

// THE TYPED PAYLOADS, one per (kind, op), each the caller's input and the
// record's payload alike. A record carries its complete values, judged by the
// writer, so a replay from zero writes the rows the first apply did; a body is
// opaque JSON here (package doc, "The applier judges no text").

// episodeNamespace is the name space an episode's id is derived in, fixed for
// good as [keyNamespace] is.
var episodeNamespace = uuid.MustParse("2e7b94c1-5f3a-4d08-8c6e-b1a07d53f2e6")

// EpisodeID is the id of the episode of key first observed at observed: so a
// duty that observes the same condition twice derives the same episode.
func EpisodeID(key string, observed time.Time) string {
	return uuid.NewSHA1(episodeNamespace, []byte(key+"@"+observed.UTC().Format(time.RFC3339Nano))).String()
}

// Opening opens a new episode of Key (op `open`).
type Opening struct {
	// Episode is the new episode's id ([EpisodeID]).
	Episode string `json:"episode"`

	// Key is the class, `:` and the subject key; Class is the class alone.
	Key   string `json:"key"`
	Class string `json:"class"`

	Source      Source   `json:"source"`
	Actor       Actor    `json:"actor"`
	Area        string   `json:"area"`
	Severity    Severity `json:"severity"`
	SubjectKind string   `json:"subject_kind"`
	SubjectID   string   `json:"subject_id"`

	// Revision is 1: an episode opens at its first revision.
	Revision int `json:"revision"`

	// VerdictDigest and FixDigest are lowercase hex SHA-256 digests.
	VerdictDigest string `json:"verdict_digest"`
	FixDigest     string `json:"fix_digest"`

	// ImpactMinutes and ImpactTokens are per week; nil where the class cannot
	// measure them.
	ImpactMinutes *int64 `json:"impact_minutes,omitempty"`
	ImpactTokens  *int64 `json:"impact_tokens,omitempty"`

	// Body is the advisor's finding as a JSON object (title, why, evidence,
	// fix ...), opaque here.
	Body json.RawMessage `json:"body"`
}

// Revision records a moved verdict or fix (op `revise`): Revision is the
// stored revision plus one.
type Revision struct {
	Episode       string          `json:"episode"`
	Revision      int             `json:"revision"`
	Severity      Severity        `json:"severity"`
	VerdictDigest string          `json:"verdict_digest"`
	FixDigest     string          `json:"fix_digest"`
	ImpactMinutes *int64          `json:"impact_minutes,omitempty"`
	ImpactTokens  *int64          `json:"impact_tokens,omitempty"`
	Body          json.RawMessage `json:"body"`

	// Moved is what moved, `[{key,label,was,now}]`, kept in the history.
	Moved json.RawMessage `json:"moved,omitempty"`
}

// Refresh records facts that moved under the same verdict and fix (op
// `refresh`): no revision bump and no history row. Revision is the revision
// it refreshes; a newer stored one makes it write nothing.
type Refresh struct {
	Episode       string          `json:"episode"`
	Revision      int             `json:"revision"`
	ImpactMinutes *int64          `json:"impact_minutes,omitempty"`
	ImpactTokens  *int64          `json:"impact_tokens,omitempty"`
	Body          json.RawMessage `json:"body"`
}

// Move is one state transition of an episode (op `move`), legal as the package
// doc's "Legal moves" says.
type Move struct {
	Episode string `json:"episode"`

	// Event is one of [MoveEvents], and State the state after it.
	Event Event `json:"event"`
	State State `json:"state"`

	// Reason is the close reason where State is closed, else empty.
	Reason CloseReason `json:"reason,omitempty"`

	// FixDigest is the fix an `applied` or `ineffective` move is about.
	FixDigest string `json:"fix_digest,omitempty"`

	// EffectDueAt is when an `applied` episode's effect is checked, and
	// SnoozedUntil when a `snoozed` one opens again.
	EffectDueAt  time.Time `json:"effect_due_at,omitzero"`
	SnoozedUntil time.Time `json:"snoozed_until,omitzero"`

	// Hold holds the key quiet past a `dismissed` close.
	Hold *Hold `json:"hold,omitempty"`

	// Moved is the facts that show why, `[{key,label,was,now}]`.
	Moved json.RawMessage `json:"moved,omitempty"`
}

// Hold keeps a key from opening again until Until (zero: for good), unless the
// new episode's severity ranks above Severity.
type Hold struct {
	Until    time.Time `json:"until,omitzero"`
	Reason   string    `json:"reason"`
	Severity Severity  `json:"severity"`
}

// Explanation is the model's words for one verdict (op `explain`), written only
// while the stored verdict digest is VerdictDigest.
type Explanation struct {
	Episode       string `json:"episode"`
	VerdictDigest string `json:"verdict_digest"`
	Text          string `json:"text"`
}

// Release lifts a key's hold (op `release`): a person's reopen. It carries
// nothing: the key is its subject.
type Release struct{}

// RunRequest is a person's press of Run now (op `request` on the run).
type RunRequest struct {
	Run string `json:"run"`

	// By is the person's handle.
	By string `json:"by"`
}

// RunRecord is a run's end (op `record` on the run).
type RunRecord struct {
	Run     string  `json:"run"`
	Trigger Trigger `json:"trigger"`

	// By is the person who pressed Run now, empty for another trigger.
	By string `json:"by,omitempty"`

	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`
	Status    RunStatus `json:"status"`

	// Classes is `[{class,status,observed,opened,resolved,revised}]`, each
	// status one of [ClassStatuses].
	Classes json.RawMessage `json:"classes"`

	// Model is the model pass's summary as a JSON object, or null.
	Model json.RawMessage `json:"model,omitempty"`
}

// Report writes one week's report, once (op `write`): the ISO week, the period
// it covers and its body as a JSON object.
type Report struct {
	Week string          `json:"week"`
	From time.Time       `json:"from"`
	To   time.Time       `json:"to"`
	Body json.RawMessage `json:"body"`
}

// AlarmRaise opens an episode of the series (op `raise`) unless one is open.
type AlarmRaise struct {
	Node     string    `json:"node"`
	Kind     string    `json:"kind"`
	Instance string    `json:"instance"`
	RaisedAt time.Time `json:"raised_at"`
}

// AlarmClear closes the series' open episode (op `clear`), else writes nothing.
type AlarmClear struct {
	ClearedAt time.Time `json:"cleared_at"`
}

// Expiry removes what is past each retention (op `expire`), once a day: closed
// episodes closed before ClosedBefore, runs ended before RunsBefore, reports
// written before ReportsBefore and alarm episodes cleared before AlarmsBefore.
type Expiry struct {
	ClosedBefore  time.Time `json:"closed_before"`
	RunsBefore    time.Time `json:"runs_before"`
	ReportsBefore time.Time `json:"reports_before"`
	AlarmsBefore  time.Time `json:"alarms_before"`
}

// ExpiryFor is the day now falls in and the expiry each retention gives at now.
func ExpiryFor(now time.Time) (time.Time, Expiry) {
	now = now.UTC()
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	return day, Expiry{
		ClosedBefore: now.Add(-ClosedRetention), RunsBefore: now.Add(-RunRetention),
		ReportsBefore: now.Add(-ReportRetention), AlarmsBefore: now.Add(-AlarmRetention),
	}
}

// Eviction is a node's eviction from this log, or its readmission: pinned at
// [GateRecordVersion] for ever.
type Eviction struct {
	V      int    `json:"v"`
	NodeID string `json:"node_id"`

	EvictedBy string    `json:"evicted_by,omitempty"`
	EvictedAt time.Time `json:"evicted_at"`

	// Readmitted makes this the inverse commit rather than a delete.
	Readmitted bool `json:"readmitted,omitempty"`
}

// Generation is a reanchor's record.
type Generation struct {
	V           int       `json:"v"`
	Generation  uint32    `json:"generation"`
	By          string    `json:"by,omitempty"`
	PrevHighest uint64    `json:"prev_highest,omitempty"`
	StreamAt    time.Time `json:"stream_created_at"`
}

// DecodeMutation reads the typed payload for one record. The dispatch is on
// (kind, op) alone, so an unknown pair is an error naming both.
func DecodeMutation(rec MutationRecord) (any, error) {
	k, op := rec.Subject.Kind, rec.Op
	switch {
	case k == KindFinding && op == OpOpen:
		return decodePayload[Opening](rec)
	case k == KindFinding && op == OpRevise:
		return decodePayload[Revision](rec)
	case k == KindFinding && op == OpRefresh:
		return decodePayload[Refresh](rec)
	case k == KindFinding && op == OpMove:
		return decodePayload[Move](rec)
	case k == KindFinding && op == OpExplain:
		return decodePayload[Explanation](rec)
	case k == KindFinding && op == OpRelease:
		return decodePayload[Release](rec)
	case k == KindRun && op == OpRequest:
		return decodePayload[RunRequest](rec)
	case k == KindRun && op == OpRecord:
		return decodePayload[RunRecord](rec)
	case k == KindReport && op == OpWrite:
		return decodePayload[Report](rec)
	case k == KindAlarm && op == OpRaise:
		return decodePayload[AlarmRaise](rec)
	case k == KindAlarm && op == OpClear:
		return decodePayload[AlarmClear](rec)
	case k == KindRetention && op == OpExpire:
		return decodePayload[Expiry](rec)
	case k == KindEviction && op == OpEviction:
		return decodePayload[Eviction](rec)
	case k == KindGeneration && op == OpGeneration:
		return decodePayload[Generation](rec)
	case k == KindBarrier && op == OpBarrier, k == KindTrimmed && op == OpTrimmed:
		// The two payload-free records: nil is their value.
		return nil, nil
	}
	return nil, fmt.Errorf("advisordomain: no payload shape for (%s, %s): a pair this build "+
		"does not know is a newer peer's record", k, op)
}

// decodePayload reads one typed payload.
func decodePayload[T any](rec MutationRecord) (T, error) {
	var out T
	if len(rec.Mutation) == 0 {
		return out, fmt.Errorf("advisordomain: the record on %s carries no payload, and "+
			"(%s, %s) needs one", rec.Subject, rec.Subject.Kind, rec.Op)
	}
	if err := json.Unmarshal(rec.Mutation, &out); err != nil {
		return out, fmt.Errorf("advisordomain: decode the %s payload on %s: %w", rec.Op, rec.Subject, err)
	}
	return out, nil
}

// EncodeBarrier renders the read index's payload-free append. An op id is
// refused: a repeat would be answered from the dedupe cache with no quorum.
func EncodeBarrier(env statelog.Envelope) ([]byte, error) {
	if env.Kind != statelog.BarrierKind {
		return nil, fmt.Errorf("advisordomain: %q is not a barrier envelope", env.Kind)
	}
	if env.OpID != "" {
		return nil, fmt.Errorf("advisordomain: a barrier carries no op id and this one has one")
	}
	return Encode(MutationRecord{
		RecordEnvelope: RecordEnvelope{
			Subject: BarrierSubject(), Op: OpBarrier, Scope: ScopeSet{Subject: true}, Gen: env.Gen,
		},
	})
}

// EncodeTrimmed renders the trim's trimmed record (ADR-0080). The writer
// travels, so the eviction gate can drop an evicted node's; no op id.
func EncodeTrimmed(env statelog.Envelope) ([]byte, error) {
	if env.Kind != statelog.TrimmedKind || env.Subject.Kind != statelog.TrimmedKind {
		return nil, fmt.Errorf("advisordomain: a %q envelope on %q is not a trimmed record",
			env.Kind, env.Subject)
	}
	if env.OpID != "" {
		return nil, fmt.Errorf("advisordomain: a trimmed record carries no op id and this one has one")
	}
	floor, err := statelog.TrimmedFloor(Domain{}.Stream().Name, env.Subject)
	if err != nil {
		return nil, fmt.Errorf("advisordomain: %w", err)
	}
	return Encode(MutationRecord{
		RecordEnvelope: RecordEnvelope{
			// Pinned at the base version for ever: every build must read it.
			V: baseRecordVersion, Subject: TrimmedSubject(floor), Op: OpTrimmed,
			Scope: ScopeSet{Subject: true}, Gen: env.Gen, Writer: env.Writer,
		},
	})
}
