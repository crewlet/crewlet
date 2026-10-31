package usage

import (
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/statelog"
)

// The USAGE RECORD: one node's day for one seat, one schedule, one person or
// one unit repository's readers, whole.
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

// RecordVersion is the record shape this build reads: the highest
// [versionedFields] names, which the conformance suite holds it to.
//
// A record above it is RETAINED at its position rather than skipped, so a
// newer peer's shape survives a rolling upgrade in both directions. A seat's,
// a schedule's and a person's day are the base format, version 1; a unit
// repository's readers ([KindRepoRead]) are version 2; and a seat's day that
// carries its changes and reviews ([Record.Code]) is version 3.
//
// IT IS THE CEILING THIS BUILD READS, NOT WHAT IT WRITES: a record is stamped
// with the lowest version that reads what it carries ([Record.Encode], over
// [versionedFields]), so a field a later build adds holds back only the records
// that carry it. It moves only with the table, and exactly to the table's
// highest version.
const RecordVersion = 3

// baseRecordVersion is version 1, the base format: what a record carrying no
// versioned field is stamped at.
const baseRecordVersion = 1

// versionedFields is every field a usage record has gained since the base
// format, and the version a reader must be at to apply a record carrying it:
// the repo_read kind and the four fields only it carries, at version 2, and a
// seat's code day ([Record.Code]), at 3. A build at version 1 RETAINS a
// repository's readers — under the identity their writer stated
// ([RecordEnvelope.SubjectID]) — rather than faulting at a kind it has no
// applier for, and applies them once it is upgraded; a build at 1 or 2 retains
// a seat's day that carries its changes and reviews rather than applying it
// with them dropped, which would replace the day every upgraded node holds
// with one counting none; every other record stays at 1 and every build
// applies it.
//
// ADDING A FIELD TO A RECORD IS ADDING A ROW HERE, on the tracker's terms: an
// older build decodes a field it has no home for AROUND — it reads the
// version, finds it readable, applies what it understands and drops the rest
// — so its rows for that day differ from every upgraded node's for as long as
// the day is kept. The row is what makes the encoder stamp a version that build
// retains instead; the constant above moves with it, and the domain's
// statelogtest candidate gains a record carrying the field. A field no tag has
// shipped is still one two builds of a rolling upgrade disagree about, so
// "nothing has been released" exempts no field from its row.
//
// A NEW KIND IS A VALUE of a field every record carries, so its row names the
// key and the value (`subject.kind` equal to the new kind's name): a row
// naming only the key would stamp every record the domain writes.
//
// THE KIND'S OWN FIELDS HAVE ROWS BESIDE IT, though today only records of that
// kind carry them: a row states one FIELD's version, by the rule above, so a
// field that a later change lets another kind carry is already stamped by its
// own row rather than relying on a kind it no longer rides with.
var versionedFields = statelog.RecordFields{
	{Name: "Subject.Kind repo_read", Since: 2, Path: []string{"subject", "kind"}, Equals: string(KindRepoRead)},
	{Name: "Subject.Repo", Since: 2, Path: []string{"subject", "repo"}},
	{Name: "Record.RepoName", Since: 2, Path: []string{"repo_name"}},
	{Name: "Record.RepoReads", Since: 2, Path: []string{"repo_reads"}},
	{Name: "Record.RepoReadersElided", Since: 2, Path: []string{"repo_readers_elided"}},
	{Name: "Record.Code", Since: 3, Path: []string{"code"}},
}

// VersionedFields is the table, for the conformance suite.
func VersionedFields() statelog.RecordFields { return slices.Clone(versionedFields) }

// Kind is what a usage record is about.
//
// A NAMED STRING TYPE with a Valid method, so a kind a newer build publishes
// is a value this build defers rather than a panic. DEFERS, and the envelope is
// what makes that true rather than claimed: a kind this build does not know is
// admitted there only on a record stamped above [RecordVersion] — which is how
// a newer build stamps one — and the second pass then retains the record as
// [ErrFutureVersion]. See [Subject.validateFor].
type Kind string

const (
	// KindSeat is one seat's spend, turns and reads.
	KindSeat Kind = "seat"

	// KindSchedule is one schedule's fires.
	KindSchedule Kind = "schedule"

	// KindPerson is one PERSON's auxiliary spend: what the seats' cheap
	// model spent for a human seat — a question answered on the operator
	// surface, a background pass on a unit a person leads. A person is no
	// seat: no agent id to key a seat's record on, no turns and no reads,
	// so its spend was in no seat's day and therefore in no named spend
	// window.
	KindPerson Kind = "person"

	// KindRepoRead is one `visibility: unit` repository's READERS on one
	// node's day: every credential and seat that read its contents there —
	// a git fetch that carried a pack, a code tool's answer — and how much
	// (types.CodeFetched). ONE SUBJECT PER (node, day, repository), carrying
	// every reader, rather than one per reader: the compacted stream keeps a
	// subject index every broker member holds in memory, so its subjects are
	// counted in repositories × nodes × days and never in readers × that.
	KindRepoRead Kind = "repo_read"
)

// Kinds is every kind this build writes.
var Kinds = []Kind{KindSeat, KindSchedule, KindPerson, KindRepoRead}

// Valid reports whether a kind off the wire is one this build knows.
func (k Kind) Valid() bool { return slices.Contains(Kinds, k) }

// Subject is the object a record is about: one kind of thing, on one node, on
// one company day.
//
// THE NODE IS PART OF THE IDENTITY, and that is the whole design. Each data
// node derives only what its own event log holds, so two nodes that both ran a
// seat on one day publish two records rather than racing for one — no
// arbitration, no merge at write time, and a reader sums across nodes. A seat
// that moved at noon is two rows for that day, both true.
type Subject struct {
	Kind Kind `json:"kind"`

	// Node is the publishing node's id, which is also the only node that
	// may ever publish on this subject.
	Node string `json:"node"`

	// Day is the company-local date label, `2026-09-23`.
	Day string `json:"day"`

	// Seat is the seat's agent id, on a seat record.
	Seat string `json:"seat,omitempty"`

	// Person is the person's seat HANDLE, on a person record — the human
	// seat the spending principal holds ([iam.RecordOwner]). A handle
	// rather than an id, because a human seat has none: identity is derived
	// for agent seats alone. And never the identity estate's person id,
	// which names who holds the seat now rather than the seat that spent.
	Person string `json:"person,omitempty"`

	// ScopeType, ScopeID and Schedule name a schedule, on a schedule
	// record — the three halves of a schedule's identity the dispatch
	// ledger keys on.
	ScopeType string `json:"scope_type,omitempty"`
	ScopeID   string `json:"scope_id,omitempty"`
	Schedule  string `json:"schedule,omitempty"`

	// Repo is the repository's id, on a repo_read record: the identity that
	// outlives its name, which a rename moves and a deletion frees.
	Repo string `json:"repo,omitempty"`
}

// Validate refuses a subject that cannot address an object.
func (s Subject) Validate() error {
	if !s.Kind.Valid() {
		return fmt.Errorf("usage: %q is not a kind this build writes", s.Kind)
	}
	if err := s.validateShared(); err != nil {
		return err
	}
	switch s.Kind {
	case KindSeat:
		if strings.TrimSpace(s.Seat) == "" {
			return fmt.Errorf("usage: a seat record names no seat")
		}
		if s.ScopeType != "" || s.ScopeID != "" || s.Schedule != "" || s.Person != "" || s.Repo != "" {
			return fmt.Errorf("usage: a seat record names a schedule, a person or a repository")
		}
	case KindPerson:
		if strings.TrimSpace(s.Person) == "" {
			return fmt.Errorf("usage: a person record names no person")
		}
		if s.Seat != "" || s.ScopeType != "" || s.ScopeID != "" || s.Schedule != "" || s.Repo != "" {
			return fmt.Errorf("usage: a person record names a seat, a schedule or a repository")
		}
	case KindSchedule:
		if strings.TrimSpace(s.ScopeType) == "" || strings.TrimSpace(s.ScopeID) == "" ||
			strings.TrimSpace(s.Schedule) == "" {
			return fmt.Errorf("usage: a schedule record must name its scope type, "+
				"scope id and schedule, and names %q/%q/%q",
				s.ScopeType, s.ScopeID, s.Schedule)
		}
		if s.Seat != "" || s.Person != "" || s.Repo != "" {
			return fmt.Errorf("usage: a schedule record names a seat, a person or a repository")
		}
	case KindRepoRead:
		if strings.TrimSpace(s.Repo) == "" {
			return fmt.Errorf("usage: a repo_read record names no repository")
		}
		if s.Seat != "" || s.Person != "" || s.ScopeType != "" || s.ScopeID != "" || s.Schedule != "" {
			return fmt.Errorf("usage: a repo_read record names a seat, a person or a schedule — " +
				"its readers are its body, and its object is the repository alone")
		}
	}
	return nil
}

// validateShared checks the two segments every kind's identity begins with —
// the node and the day — which is all a build can check of a kind it does not
// know.
func (s Subject) validateShared() error {
	if strings.TrimSpace(s.Node) == "" {
		return fmt.Errorf("usage: a %s record names no node — the node is half "+
			"of the object's identity, and the only writer allowed on it", s.Kind)
	}
	if _, err := period.Parse(period.Day, s.Day, nil); err != nil {
		return fmt.Errorf("usage: a %s record's day: %w", s.Kind, err)
	}
	return nil
}

// validateFor is [Subject.Validate] as the envelope applies it to a record of
// version v.
//
// A KIND THIS BUILD DOES NOT KNOW, ON A RECORD ABOVE [RecordVersion], IS A
// NEWER BUILD'S, and the envelope admits it with only the node and the day
// checked, so the second pass retains it as [ErrFutureVersion]. The envelope
// is the half every build must read, and refused here the record was an
// unreadable envelope — which the framework treats as a STOP of the whole
// applier rather than a deferral of one record. So every kind a later build
// added stopped every node still on this one, which is the opposite of what
// [Kind] promised. Below the day this build cannot read the subject's fields at
// all, so the record is filed under the identity its writer stated
// ([RecordEnvelope.SubjectID]) — which is NOT a label: on this compacted
// domain a deferred record supersedes whatever was retained under its
// subject, so a subject composed from the fields this build does know would
// make every object of the new kind on one day one object. The deferred
// record keeps its whole payload, and the build that can read it applies it.
//
// AT OR BELOW [RecordVersion] AN UNKNOWN KIND IS A WRITER FAULT rather than a
// newer build, because a version this build reads is one whose kinds it knows.
func (s Subject) validateFor(v int) error {
	if !s.Kind.Valid() && v > RecordVersion {
		return s.validateShared()
	}
	return s.Validate()
}

// segments is the object's identity below its kind, in the order the subject
// is built: node, day, then the object.
func (s Subject) segments() []string {
	switch s.Kind {
	case KindSchedule:
		return []string{s.Node, s.Day, s.ScopeType, s.ScopeID, s.Schedule}
	case KindPerson:
		return []string{s.Node, s.Day, s.Person}
	case KindRepoRead:
		return []string{s.Node, s.Day, s.Repo}
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
// Its EIGHT keys are reserved at the top level of the format for its life: a
// later version may add fields beside them and may never repurpose one.
type RecordEnvelope struct {
	// V is the record version.
	V int `json:"v"`

	// OpID is the broker's duplicate-window key, derived from the
	// record's own content (see [Record.opID]).
	OpID string `json:"op_id,omitempty"`

	// Subject is the object this record is about.
	Subject Subject `json:"subject"`

	// SubjectID is the object's identity below its kind as its WRITER
	// composed it — [Subject.ID] — written on every record, and the identity
	// a build reads for a kind it does not know ([RecordEnvelope.Wire]).
	//
	// THE SUBJECT IS THE SUPERSEDE KEY, not a label: this domain replays
	// compacted, and a deferred record is retained under its subject with
	// whatever an earlier one held there deleted first. A kind a later build
	// adds is keyed on fields this one cannot decode, so composed here from
	// what it can, every object of that kind on one (node, day) was ONE
	// subject — and the second retained deleted the first, a day nothing
	// re-derives once yesterday has passed. The writer's own composition is
	// the one every build can read verbatim.
	SubjectID string `json:"subject_id,omitempty"`

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

// Wire is the subject the framework files this record under: the one its
// kind's own fields compose when this build knows the kind, and the one its
// writer composed ([RecordEnvelope.SubjectID]) when it does not.
func (e RecordEnvelope) Wire() statelog.Subject {
	if !e.Subject.Kind.Valid() {
		return statelog.Subject{Kind: string(e.Subject.Kind), ID: e.SubjectID}
	}
	return e.Subject.Wire()
}

// validateSubjectID holds the writer's identity to the subject it names.
//
// FOR A KIND THIS BUILD KNOWS, the two must be one: a record whose stated
// identity names another object than its fields is a writer fault, and filed
// under either it would supersede the wrong row. FOR ONE IT DOES NOT, the
// identity is required — every build that can write a kind this one lacks
// writes it — and must begin with the subject's own node and day, which is
// all this build can check and what keeps a record in its writer's own
// namespace.
func (e RecordEnvelope) validateSubjectID() error {
	if e.Subject.Kind.Valid() {
		if e.SubjectID != "" && e.SubjectID != e.Subject.ID() {
			return fmt.Errorf("usage: the record on %s states its identity as %q", e.Subject, e.SubjectID)
		}
		return nil
	}
	prefix := coord.DocumentKey(e.Subject.Node, e.Subject.Day) + coord.KeySeparator
	if !strings.HasPrefix(e.SubjectID, prefix) || len(e.SubjectID) == len(prefix) {
		return fmt.Errorf("usage: the %s record of a newer build states its identity as %q, "+
			"which does not name an object under its own node and day — every build "+
			"that writes a kind this one lacks states it", e.Subject.Kind, e.SubjectID)
	}
	return nil
}

// Record is one node's day for one object.
type Record struct {
	RecordEnvelope

	// Handle and Role name a seat as its day knew it. A seat's id is
	// derived and outlives both, so a reader that has only the id — a seat
	// removed since — still has a name to show. On a person's record Role
	// is the person's seat's role, and the handle is the subject's own.
	Handle string `json:"handle,omitempty"`
	Role   string `json:"role,omitempty"`

	// Tokens are a seat's or a person's spend, one entry per (phase,
	// worker, model, provider key).
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

	// RepoName names a repo_read record's repository as its day's newest
	// read did: a label beside the id, which outlives it.
	RepoName string `json:"repo_name,omitempty"`

	// RepoReads are a unit repository's readers on its day, the most-read
	// [ReadersPerRepoDay] of them, in reader order ([RepoRead.Key]);
	// RepoReadersElided counts the rest, so an answer that lost readers says
	// so.
	RepoReads         []RepoRead `json:"repo_reads,omitempty"`
	RepoReadersElided int        `json:"repo_readers_elided,omitempty"`

	// Code is a seat's changes and reviews on the code host that day, nil
	// for a day with none of them — so a seat's day that has none stays the
	// base format every build reads (record version 3 only where it is set).
	Code *CodeDay `json:"code,omitempty"`

	// Extra carries fields a newer build wrote, so a record round-trips
	// losslessly through a node that cannot interpret them.
	Extra map[string]json.RawMessage `json:"-"`
}

// CodeDay is what a seat did on the code host on its day, on its node: the
// changes it opened, the reviews it gave, and the findings the reviews of its
// own changes raised — counted on the OWNER, since a finding is work its
// change received (store.UsageCode, counted off the events the landings
// announced). Every figure only grows within a day, so it joins by max.
type CodeDay struct {
	ChangesOpened    int64 `json:"changes_opened,omitempty"`
	ReviewsGiven     int64 `json:"reviews_given,omitempty"`
	FindingsReceived int64 `json:"findings_received,omitempty"`
}

// codeDayOf is c as a record carries it: nil for a day with none.
func codeDayOf(c CodeDay) *CodeDay {
	if c == (CodeDay{}) {
		return nil
	}
	return &c
}

// RepoRead is one reader's reads of a unit repository on a day, by one way.
//
// THE READER IS ITS KIND, ITS PRINCIPAL AND THE SEAT IT ACTED AS: a person reads
// through several credentials of theirs, each its own reader naming their one
// seat, and a token bound to one seat and then another is two readers — never
// one whose earlier reads are filed under the seat it holds now.
type RepoRead struct {
	// PrincipalKind is `operator` (a credential) or `agent` (a seat), the
	// code host's spelling (types.CodeReaderKind).
	PrincipalKind string `json:"principal_kind"`

	// Principal is the credential as every record names it (iam.ActorFor),
	// or the agent's handle.
	Principal string `json:"principal"`

	// Seat is the human seat the credential is bound to, or the agent's own
	// handle; empty for a token bound to nobody.
	Seat string `json:"seat,omitempty"`

	// Via is how it was read: `git` or `tool` (types.CodeFetchedVia).
	Via string `json:"via"`

	// Reads is how many reads, Bytes what they were sent, LastAt the newest.
	Reads  int64     `json:"reads"`
	Bytes  int64     `json:"bytes"`
	LastAt time.Time `json:"last_at,omitzero"`
}

// RepoReaderKey is a reader's identity within its repository's day.
type RepoReaderKey struct{ PrincipalKind, Principal, Via, Seat string }

// Key is r's reader.
func (r RepoRead) Key() RepoReaderKey {
	return RepoReaderKey{PrincipalKind: r.PrincipalKind, Principal: r.Principal, Via: r.Via, Seat: r.Seat}
}

// compareRepoReader is the reader order a record's readers and every read of
// them are in: the principal kind, the principal, the way, the seat — the
// estate's own key below the repository's day.
func compareRepoReader(a, b RepoRead) int {
	return cmp.Or(cmp.Compare(a.PrincipalKind, b.PrincipalKind), cmp.Compare(a.Principal, b.Principal),
		cmp.Compare(a.Via, b.Via), cmp.Compare(a.Seat, b.Seat))
}

// maxReaderBytes is the longest principal a reader may name — a credential's
// name or a handle, both far shorter — and maxSeatBytes the longest seat, a
// handle's own bound ([iam.MaxLogin]): the figures the record's bound
// ([ReadersPerRepoDay]) is measured at.
const (
	maxReaderBytes = 255
	maxSeatBytes   = iam.MaxLogin
)

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
	"v", "op_id", "subject", "subject_id", "created_at", "gen", "writer", "scope",
	"handle", "role", "tokens", "turns", "reads", "reads_elided", "fires",
	"repo_name", "repo_reads", "repo_readers_elided", "code",
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
	if err := env.Subject.validateFor(env.V); err != nil {
		return RecordEnvelope{}, err
	}
	if err := env.validateSubjectID(); err != nil {
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
	if r.Subject.Kind != KindRepoRead && (r.RepoName != "" || len(r.RepoReads) > 0 || r.RepoReadersElided != 0) {
		return fmt.Errorf("usage: the %s record on %s carries a repository's readers", r.Subject.Kind, r.Subject)
	}
	if err := r.validateCode(); err != nil {
		return err
	}
	switch r.Subject.Kind {
	case KindRepoRead:
		return r.validateRepoReads()
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
	case KindPerson:
		if r.Turns != nil || len(r.Reads) > 0 || r.ReadsElided != 0 || len(r.Fires) > 0 ||
			r.Handle != "" {
			return fmt.Errorf("usage: the person record on %s carries a seat's turns, "+
				"reads or handle, or a schedule's fires — a person's day is its spend "+
				"alone, named by its subject", r.Subject)
		}
	}
	return nil
}

// validateCode refuses a code day a record cannot carry: on any kind but a
// seat's — a person's changes and reviews are counted on no seat, and a
// schedule or a repository opens none — a negative figure, or every figure
// zero, which a writer leaves out ([codeDayOf]): carried, it would stamp the
// record version 3 and hold back every older build from a day that counts
// nothing they cannot read.
func (r Record) validateCode() error {
	if r.Code == nil {
		return nil
	}
	c := *r.Code
	switch {
	case r.Subject.Kind != KindSeat:
		return fmt.Errorf("usage: the %s record on %s carries a seat's changes and reviews", r.Subject.Kind,
			r.Subject)
	case c.ChangesOpened < 0 || c.ReviewsGiven < 0 || c.FindingsReceived < 0:
		return fmt.Errorf("usage: the seat record on %s counts %+v of its changes and reviews — no figure of "+
			"a day is below zero", r.Subject, c)
	case c == CodeDay{}:
		return fmt.Errorf("usage: the seat record on %s carries a code day of zeros — a day with no change "+
			"or review carries none, and stays readable by every build", r.Subject)
	}
	return nil
}

// validateRepoReads refuses a repo_read body its kind cannot carry: no reader,
// more than a day holds ([ReadersPerRepoDay]), a reader this build cannot name
// — an unknown principal kind or way, a principal past [maxReaderBytes] or a
// seat past [maxSeatBytes], no read — one named twice, or a seat's, a
// schedule's or a person's day beside it. A reader named twice is refused rather than summed:
// the publisher folds each reader once, so a second entry is a writer fault,
// and the apply would fail on the readers' key at whatever node applied it.
func (r Record) validateRepoReads() error {
	if r.Turns != nil || len(r.Tokens) > 0 || len(r.Reads) > 0 || r.ReadsElided != 0 ||
		len(r.Fires) > 0 || r.Handle != "" || r.Role != "" {
		return fmt.Errorf("usage: the repo_read record on %s carries a seat's spend, turns or reads, "+
			"or a schedule's fires — a repository's day is its readers alone", r.Subject)
	}
	switch {
	case len(r.RepoReads) == 0:
		return fmt.Errorf("usage: the repo_read record on %s names no reader — a day nobody read the "+
			"repository on is no record", r.Subject)
	case len(r.RepoReads) > ReadersPerRepoDay:
		return fmt.Errorf("usage: the repo_read record on %s carries %d readers and a repository's day "+
			"holds at most %d — the writer elides past the cap and counts what it dropped",
			r.Subject, len(r.RepoReads), ReadersPerRepoDay)
	case r.RepoReadersElided < 0:
		return fmt.Errorf("usage: the repo_read record on %s elides %d readers", r.Subject, r.RepoReadersElided)
	}
	seen := make(map[RepoReaderKey]bool, len(r.RepoReads))
	for _, rd := range r.RepoReads {
		switch {
		case !types.CodeReaderKind(rd.PrincipalKind).Valid():
			return fmt.Errorf("usage: a reader on %s is a %q, which is not a principal kind this build "+
				"reads (%v)", r.Subject, rd.PrincipalKind, types.CodeReaderKinds())
		case !types.CodeFetchedVia(rd.Via).Valid():
			return fmt.Errorf("usage: a reader on %s read it by %q, which is not a way this build reads "+
				"(%v)", r.Subject, rd.Via, types.CodeFetchedVias())
		case rd.Principal == "" || len(rd.Principal) > maxReaderBytes || len(rd.Seat) > maxSeatBytes:
			return fmt.Errorf("usage: a reader on %s names principal %q and seat %q — a principal is 1 to "+
				"%d bytes and a seat at most %d", r.Subject, rd.Principal, rd.Seat, maxReaderBytes,
				maxSeatBytes)
		case rd.Reads < 1 || rd.Bytes < 0:
			return fmt.Errorf("usage: reader %s on %s read it %d times for %d bytes — a reader read it "+
				"at least once", rd.Principal, r.Subject, rd.Reads, rd.Bytes)
		case seen[rd.Key()]:
			return fmt.Errorf("usage: the repo_read record on %s names reader %+v twice", r.Subject, rd.Key())
		}
		seen[rd.Key()] = true
	}
	return nil
}

// Encode writes a record, stamping it with the lowest version that reads it.
//
// A ZERO VERSION IS "STAMP IT": the publisher leaves V unset and this computes
// the minimum over [versionedFields] from the bytes it is about to publish —
// carried fields included — so the stamp cannot drift from what the record
// carries. A version the caller DID set is kept — a relay re-encodes a record
// at the version its writer gave it — but is refused when it is below what the
// record's own fields need, because that record would be applied, lossily, by
// exactly the builds the stamp exists to hold it back from.
func (r Record) Encode() ([]byte, error) {
	stamp := r.V == 0
	if stamp {
		// THE LOWEST, never [RecordVersion]: the table raises it to what
		// the record's own fields need.
		r.V = baseRecordVersion
	}
	if err := r.Subject.Validate(); err != nil {
		return nil, err
	}
	// THE IDENTITY, written on every record — see [RecordEnvelope.SubjectID].
	r.SubjectID = r.Subject.ID()
	if r.Scope.Empty() {
		r.Scope = statelog.ScopeSet{Paths: []string{r.Subject.ScopePath()}}
	}
	if r.V <= RecordVersion {
		if err := r.validateBody(); err != nil {
			return nil, err
		}
	}
	body, err := r.marshal()
	if err != nil {
		return nil, err
	}
	// NO OP: a usage record has none, so every row of the table applies to
	// every record.
	minimum, err := versionedFields.Minimum("", body)
	if err != nil {
		return nil, fmt.Errorf("usage: stamp the record on %s: %w", r.Subject, err)
	}
	switch {
	case r.V >= minimum:
	case stamp:
		r.V = minimum
		return r.marshal()
	default:
		return nil, fmt.Errorf("usage: the record on %s is stamped version %d and "+
			"carries %s — a build reading %d would decode it, drop what it has no "+
			"field for and apply the rest; leave the version unset and the "+
			"encoder stamps the lowest one that reads it", r.Subject, r.V,
			strings.Join(versionedFields.Carried("", body), ", "), r.V)
	}
	return body, nil
}

// marshal renders the record with whatever a newer build wrote carried back.
func (r Record) marshal() ([]byte, error) {
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
