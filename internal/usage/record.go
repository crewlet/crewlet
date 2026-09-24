package usage

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/statelog"
)

// The USAGE RECORD: one node's day for one seat or one schedule, whole.
//
// # A record is the object's current value, never a delta
//
// Every record is the CUMULATIVE value of its (node, day, object) — re-derived
// from the owning node's own event log and republished whole — and an apply
// REPLACES what the object held. That is what makes the stream compactable: the
// newest message on a subject is everything anybody needs, so the broker keeps
// one and a node that joins replays one per object. A delta would need every
// message ever published on the subject, which is a log rather than a table.
// It is also what makes a lost or repeated publish harmless: the next flush
// carries the whole day again.

// RecordVersion is the record shape this build reads.
//
// A record above it is RETAINED at its position rather than skipped, so a
// newer peer's shape survives a rolling upgrade in both directions.
const RecordVersion = 1

// Kind is what a usage record is about.
//
// A NAMED STRING TYPE with a Valid method, so a kind a newer build publishes
// is a value this build defers rather than a panic.
type Kind string

const (
	// KindSeat is one seat's spend, turns and reads.
	KindSeat Kind = "seat"

	// KindSchedule is one schedule's fires.
	KindSchedule Kind = "schedule"
)

// Kinds is every kind this build writes.
var Kinds = []Kind{KindSeat, KindSchedule}

// Valid reports whether a kind off the wire is one this build knows.
func (k Kind) Valid() bool { return slices.Contains(Kinds, k) }

// Subject is the object a record is about: one kind of thing, on one node, on
// one company day.
//
// THE NODE IS PART OF THE IDENTITY, and that is the whole design. Each node
// derives only what its own event log holds, so two nodes that both ran a seat
// on one day publish two records rather than racing for one — no arbitration,
// no merge at write time, and a reader sums across nodes. A seat that moved
// at noon is two rows for that day, both true.
type Subject struct {
	Kind Kind `json:"kind"`

	// Node is the publishing node's id, which is also the only node that
	// may ever publish on this subject.
	Node string `json:"node"`

	// Day is the company-local date label, `2026-09-23`.
	Day string `json:"day"`

	// Seat is the seat's agent id, on a seat record.
	Seat string `json:"seat,omitempty"`

	// ScopeType, ScopeID and Schedule name a schedule, on a schedule
	// record — the three halves of a schedule's identity the dispatch
	// ledger keys on.
	ScopeType string `json:"scope_type,omitempty"`
	ScopeID   string `json:"scope_id,omitempty"`
	Schedule  string `json:"schedule,omitempty"`
}

// Validate refuses a subject that cannot address an object.
func (s Subject) Validate() error {
	if !s.Kind.Valid() {
		return fmt.Errorf("usage: %q is not a kind this build writes", s.Kind)
	}
	if strings.TrimSpace(s.Node) == "" {
		return fmt.Errorf("usage: a %s record names no node — the node is half "+
			"of the object's identity, and the only writer allowed on it", s.Kind)
	}
	if _, err := period.Parse(period.Day, s.Day, nil); err != nil {
		return fmt.Errorf("usage: a %s record's day: %w", s.Kind, err)
	}
	switch s.Kind {
	case KindSeat:
		if strings.TrimSpace(s.Seat) == "" {
			return fmt.Errorf("usage: a seat record names no seat")
		}
		if s.ScopeType != "" || s.ScopeID != "" || s.Schedule != "" {
			return fmt.Errorf("usage: a seat record names a schedule")
		}
	case KindSchedule:
		if strings.TrimSpace(s.ScopeType) == "" || strings.TrimSpace(s.ScopeID) == "" ||
			strings.TrimSpace(s.Schedule) == "" {
			return fmt.Errorf("usage: a schedule record must name its scope type, "+
				"scope id and schedule, and names %q/%q/%q",
				s.ScopeType, s.ScopeID, s.Schedule)
		}
		if s.Seat != "" {
			return fmt.Errorf("usage: a schedule record names a seat")
		}
	}
	return nil
}

// segments is the object's identity below its kind, in the order the subject
// is built: node, day, then the object.
func (s Subject) segments() []string {
	if s.Kind == KindSchedule {
		return []string{s.Node, s.Day, s.ScopeType, s.ScopeID, s.Schedule}
	}
	return []string{s.Node, s.Day, s.Seat}
}

// ID is the subject's identity as the framework carries it: the coordination
// key grammar over the node, the day and the object.
//
// ESCAPED SEGMENT BY SEGMENT, because the wire subject is a token path and a
// node id, a schedule name or a scope id can carry a dot, a space or a
// wildcard. [coord.DocumentKey] is the one place that escaping is written.
func (s Subject) ID() string { return coord.DocumentKey(s.segments()...) }

// Wire is the subject as the framework publishes it.
func (s Subject) Wire() statelog.Subject {
	return statelog.Subject{Kind: string(s.Kind), ID: s.ID()}
}

// String renders a subject for a log line.
func (s Subject) String() string { return string(s.Kind) + "." + s.ID() }

// ScopeRoot is the domain's own level of a scope path.
const ScopeRoot = "u"

// ScopePath is the one path a record's apply touches: the day, the node, the
// kind, the object.
//
// DAY FIRST, because the one read that could ever want a whole level is "this
// day, every node"; and every segment escaped by the same grammar the subject
// uses, which is what keeps a separator inside a schedule name from splitting
// one level into two.
func (s Subject) ScopePath() string {
	parts := []string{ScopeRoot, coord.DocumentKey(s.Day), coord.DocumentKey(s.Node),
		string(s.Kind), coord.DocumentKey(s.segments()[2:]...)}
	return strings.Join(parts, statelog.ScopeSeparator)
}

// RecordEnvelope is the half EVERY build can read, for ever.
//
// Its SEVEN keys are reserved at the top level of the format for its life: a
// later version may add fields beside them and may never repurpose one.
type RecordEnvelope struct {
	// V is the record version.
	V int `json:"v"`

	// OpID is the broker's duplicate-window key, derived from the
	// record's own content (see [Record.opID]).
	OpID string `json:"op_id,omitempty"`

	// Subject is the object this record is about.
	Subject Subject `json:"subject"`

	// CreatedAt is the writer's own clock. Reported, never ordered on.
	CreatedAt time.Time `json:"created_at,omitzero"`

	// Gen is the generation the writer published in.
	Gen uint32 `json:"gen,omitempty"`

	// Writer is the publishing node — always the subject's own node.
	Writer string `json:"writer,omitempty"`

	// Scope is the object's path, carried rather than derived so a newer
	// build's wider scope is readable by this one.
	Scope statelog.ScopeSet `json:"scope"`
}

// Record is one node's day for one object.
type Record struct {
	RecordEnvelope

	// Handle and Role name a seat as its day knew it. A seat's id is
	// derived and outlives both, so a reader that has only the id — a seat
	// removed since — still has a name to show.
	Handle string `json:"handle,omitempty"`
	Role   string `json:"role,omitempty"`

	// Tokens are a seat's spend, one entry per (phase, worker, model,
	// provider key).
	Tokens []Tokens `json:"tokens,omitempty"`

	// Turns are a seat's turn statistics. Present on every seat record,
	// because its row is the head the apply's guard reads.
	Turns *Turns `json:"turns,omitempty"`

	// Reads are the pages a seat reached, capped at [ReadsPerSeatDay];
	// ReadsElided is how many (page, via) entries the cap dropped.
	Reads       []Read `json:"reads,omitempty"`
	ReadsElided int    `json:"reads_elided,omitempty"`

	// Fires are a schedule's dispatches.
	Fires []Fire `json:"fires,omitempty"`

	// Extra carries fields a newer build wrote, so a record round-trips
	// losslessly through a node that cannot interpret them.
	Extra map[string]json.RawMessage `json:"-"`
}

// Tokens is one spend cell. The cache counts are a BREAKDOWN of Input, never
// an addition: Total is Input plus Output.
type Tokens struct {
	Phase       string `json:"phase,omitempty"`
	Worker      string `json:"worker,omitempty"`
	Model       string `json:"model,omitempty"`
	ProviderKey string `json:"provider_key,omitempty"`
	Input       int64  `json:"input"`
	Output      int64  `json:"output"`
	CacheRead   int64  `json:"cache_read"`
	CacheWrite  int64  `json:"cache_write"`
	Total       int64  `json:"total"`
	Calls       int64  `json:"calls"`
}

// Turns is a seat's turns that ended on its day, on its node.
type Turns struct {
	Count     int64 `json:"count"`
	Failed    int64 `json:"failed"`
	Reviewed  int64 `json:"reviewed"`
	FirstPass int64 `json:"first_pass"`

	// SentBack counts REVIEWS that sent work back, not turns.
	SentBack int64 `json:"sent_back"`

	// Durations is every ended turn's duration, as a histogram.
	Durations Hist `json:"duration_hist"`

	LastEndedAt time.Time `json:"last_ended_at,omitzero"`
}

// Read is one (backend, page, via) a seat reached, with its newest read.
type Read struct {
	PageID      string    `json:"page_id"`
	Backend     string    `json:"backend,omitempty"`
	Via         string    `json:"via"`
	Count       int64     `json:"count"`
	LastAt      time.Time `json:"last_at,omitzero"`
	LastTurnID  string    `json:"last_turn_id,omitempty"`
	LastWorkKey string    `json:"last_work_key,omitempty"`
	LastQuery   string    `json:"last_query,omitempty"`
}

// Fire is one dispatch a schedule made.
type Fire struct {
	At      time.Time `json:"at"`
	Target  string    `json:"target,omitempty"`
	Outcome string    `json:"outcome,omitempty"`
	TraceID string    `json:"trace_id,omitempty"`
	TurnID  string    `json:"turn_id,omitempty"`
}

// knownKeys are the top-level keys this build writes, so Extra holds exactly
// what it does not. LISTED RATHER THAN REFLECTED, for the vector record's
// reason: the list is the format's own reserved set.
var knownKeys = []string{
	"v", "op_id", "subject", "created_at", "gen", "writer", "scope",
	"handle", "role", "tokens", "turns", "reads", "reads_elided", "fires",
}

// DecodeEnvelope is the FIRST pass, and it never fails on version.
func DecodeEnvelope(payload []byte) (RecordEnvelope, error) {
	var env RecordEnvelope
	if err := json.Unmarshal(payload, &env); err != nil {
		return RecordEnvelope{}, fmt.Errorf("usage: decode the record envelope: %w", err)
	}
	if env.V <= 0 {
		return RecordEnvelope{}, fmt.Errorf("usage: a record carries version %d — "+
			"every record states its version, and one that does not cannot be "+
			"told apart from a newer build's", env.V)
	}
	if err := env.Subject.Validate(); err != nil {
		return RecordEnvelope{}, err
	}
	if env.Scope.Empty() {
		return RecordEnvelope{}, fmt.Errorf("usage: the record on %s declares no "+
			"scope — an empty scope says the record makes nothing stale, which is "+
			"the one claim a record a build cannot read may not make", env.Subject)
	}
	return env, nil
}

// ErrFutureVersion reports a record a newer build wrote.
type ErrFutureVersion struct {
	Got, Want int
	Subject   Subject
}

func (e *ErrFutureVersion) Error() string {
	return fmt.Sprintf("usage: the record on %s is version %d and this build "+
		"reads %d — it is RETAINED rather than skipped, so a build that can read "+
		"it applies it later", e.Subject, e.Got, e.Want)
}

// Decode is the SECOND pass, and the one that may refuse on version.
func Decode(payload []byte) (Record, error) {
	env, err := DecodeEnvelope(payload)
	if err != nil {
		return Record{}, err
	}
	if env.V > RecordVersion {
		return Record{RecordEnvelope: env}, &ErrFutureVersion{
			Got: env.V, Want: RecordVersion, Subject: env.Subject,
		}
	}
	var rec Record
	if err := json.Unmarshal(payload, &rec); err != nil {
		return Record{RecordEnvelope: env}, fmt.Errorf("usage: decode the record "+
			"on %s: %w", env.Subject, err)
	}
	if err := rec.validateBody(); err != nil {
		return Record{RecordEnvelope: env}, err
	}
	var extra map[string]json.RawMessage
	if err := json.Unmarshal(payload, &extra); err == nil {
		for _, known := range knownKeys {
			delete(extra, known)
		}
		if len(extra) > 0 {
			rec.Extra = extra
		}
	}
	return rec, nil
}

// validateBody refuses a body its kind cannot carry.
//
// A seat record with fires, or a schedule record with tokens, is a writer
// fault rather than a newer build — the version gate already let it through as
// one this build reads in full — and applying it would write rows under an
// object that does not own them.
func (r Record) validateBody() error {
	switch r.Subject.Kind {
	case KindSeat:
		if r.Turns == nil {
			return fmt.Errorf("usage: the seat record on %s carries no turns — "+
				"its row is the head every apply's guard reads", r.Subject)
		}
		if len(r.Fires) > 0 {
			return fmt.Errorf("usage: the seat record on %s carries fires", r.Subject)
		}
		if len(r.Reads) > ReadsPerSeatDay {
			return fmt.Errorf("usage: the seat record on %s carries %d reads and a "+
				"seat-day holds at most %d — the writer elides past the cap and "+
				"counts what it dropped", r.Subject, len(r.Reads), ReadsPerSeatDay)
		}
		if r.ReadsElided < 0 {
			return fmt.Errorf("usage: the seat record on %s elides %d reads",
				r.Subject, r.ReadsElided)
		}
	case KindSchedule:
		if r.Turns != nil || len(r.Tokens) > 0 || len(r.Reads) > 0 || r.ReadsElided != 0 {
			return fmt.Errorf("usage: the schedule record on %s carries a seat's "+
				"spend, turns or reads", r.Subject)
		}
	}
	return nil
}

// Encode writes a record.
func (r Record) Encode() ([]byte, error) {
	if r.V == 0 {
		r.V = RecordVersion
	}
	if err := r.Subject.Validate(); err != nil {
		return nil, err
	}
	if r.Scope.Empty() {
		r.Scope = statelog.ScopeSet{Paths: []string{r.Subject.ScopePath()}}
	}
	if r.V <= RecordVersion {
		if err := r.validateBody(); err != nil {
			return nil, err
		}
	}
	body, err := json.Marshal(r)
	if err != nil || len(r.Extra) == 0 {
		return body, err
	}
	// A RECORD FROM A NEWER BUILD RE-ENCODES WITH ITS UNKNOWN FIELDS, so a
	// relayed record loses nothing its writer wrote.
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(body, &merged); err != nil {
		return nil, fmt.Errorf("usage: re-encode a record carrying %d field(s) "+
			"this build does not know: %w", len(r.Extra), err)
	}
	for key, value := range r.Extra {
		if _, taken := merged[key]; !taken {
			merged[key] = value
		}
	}
	return json.Marshal(merged)
}
