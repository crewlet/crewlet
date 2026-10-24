package code

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/org"
)

// THE TYPED PAYLOADS, one per (kind, op), and the closed sets they spell.
//
// Every payload travels as FULL POST-STATE or as the complete list of what a
// record does: a repository's settings whole, a push's every command with its
// result. That is what lets a record rebuild its rows on a node that replays
// it from zero, with no lookup against anything but the rows a strict replay
// has already written.
//
// NO PAYLOAD CARRIES A VERSION FIELD: the RECORD's version gates decoding, the
// domain keeps no document version of its own, and a field nothing reads is
// not shipped. The eviction and the generation keep the framework's shape,
// version and all, which every domain shares.
//
// EVERY ENUM is a named string with typed constants, Valid, and a function
// answering the closed set in order — the one list a validator and a schema
// read.

// OwnerKind is what owns a repository.
type OwnerKind string

const (
	// OwnerUnit is a unit of the chart, by its key.
	OwnerUnit OwnerKind = "unit"
	// OwnerSeat is one seat, by its handle.
	OwnerSeat OwnerKind = "seat"
)

// OwnerKinds is the closed set.
func OwnerKinds() []OwnerKind { return []OwnerKind{OwnerUnit, OwnerSeat} }

// Valid reports a kind this build knows.
func (k OwnerKind) Valid() bool { return slices.Contains(OwnerKinds(), k) }

// ManagedBy is what owns a repository's settings and its lifecycle.
type ManagedBy string

// ManagedConfig is a repository code.repos declares: the configuration
// reconcile creates it, settles its settings and retires it, and nothing else
// may.
const ManagedConfig ManagedBy = "config"

// ManagedBys is the closed set.
func ManagedBys() []ManagedBy { return []ManagedBy{ManagedConfig} }

// Valid reports a value this build knows.
func (m ManagedBy) Valid() bool { return slices.Contains(ManagedBys(), m) }

// DirectPush is whether a protected ref takes a direct push.
type DirectPush string

const (
	// DirectPersons lets a person push to it directly.
	DirectPersons DirectPush = "persons"
	// DirectNone lets nobody push to it directly.
	DirectNone DirectPush = "none"
)

// DirectPushes is the closed set.
func DirectPushes() []DirectPush { return []DirectPush{DirectPersons, DirectNone} }

// Valid reports a value this build knows.
func (d DirectPush) Valid() bool { return slices.Contains(DirectPushes(), d) }

// Visibility is who may read a repository besides persons and the tokens of
// Tier A (ADR-0054): a SEAT'S standing narrows by it, and a person's never
// does — a credential presented by a person, or a token bound to nobody, reads
// every repository, because a CI token that could not read a unit repository
// breaks its builds and a Tier A token is the operator's own credential.
type Visibility string

const (
	// VisibilityCompany is read by every seat.
	VisibilityCompany Visibility = "company"
	// VisibilityUnit is read only by the seats the chart relates to the
	// owner: an owning unit's subtree and the leads above it, or an owning
	// seat and the leads of its unit and above — and by no other seat.
	VisibilityUnit Visibility = "unit"
)

// Visibilities is the closed set: the one list a validator and the
// configuration's own (config.CodeVisibilities, held to it by the engine's
// TestEveryDeclaredVisibilityIsOneTheDomainRecords) are read against.
func Visibilities() []Visibility { return []Visibility{VisibilityCompany, VisibilityUnit} }

// Valid reports a value this build knows. EMPTY IS NOT ONE: a record carries a
// repository's settings whole, and a settings record that named no visibility
// would be a reader's guess at which one it meant — the configuration applies
// its default (company) before the record is formed.
func (v Visibility) Valid() bool { return slices.Contains(Visibilities(), v) }

// PrincipalKind is who acts on a repository.
type PrincipalKind string

const (
	// PrincipalOperator is a credential — a person, through the token bound
	// to them, or a token bound to nobody — the author kind every operator
	// write is recorded under.
	PrincipalOperator PrincipalKind = "operator"
	// PrincipalAgent is a seat of the chart, by its handle, whose standing on
	// a repository the chart decides ([GrantOf]).
	PrincipalAgent PrincipalKind = "agent"
)

// PrincipalKinds is the closed set.
func PrincipalKinds() []PrincipalKind { return []PrincipalKind{PrincipalOperator, PrincipalAgent} }

// Valid reports a value this build knows.
func (p PrincipalKind) Valid() bool { return slices.Contains(PrincipalKinds(), p) }

// CommandResult is what became of one ref update.
type CommandResult string

const (
	// ResultOK moved the ref.
	ResultOK CommandResult = "ok"
	// ResultNoop asked for the value the ref already had.
	ResultNoop CommandResult = "noop"
	// ResultNG was refused, for the reason its Refusal names.
	ResultNG CommandResult = "ng"
)

// CommandResults is the closed set.
func CommandResults() []CommandResult { return []CommandResult{ResultOK, ResultNoop, ResultNG} }

// Valid reports a value this build knows.
func (r CommandResult) Valid() bool { return slices.Contains(CommandResults(), r) }

// RepoStatus is where a repository is in its life.
type RepoStatus string

const (
	// RepoStatusActive takes pushes.
	RepoStatusActive RepoStatus = "active"
	// RepoStatusArchived is read-only.
	RepoStatusArchived RepoStatus = "archived"
	// RepoStatusDeleted is gone from every listing and restorable by
	// declaring it again until its purge.
	RepoStatusDeleted RepoStatus = "deleted"
	// RepoStatusPurging is being removed, part by part; its name returns
	// when the purge completes.
	RepoStatusPurging RepoStatus = "purging"
)

// RepoStatuses is the closed set.
func RepoStatuses() []RepoStatus {
	return []RepoStatus{RepoStatusActive, RepoStatusArchived, RepoStatusDeleted, RepoStatusPurging}
}

// Valid reports a value this build knows.
func (s RepoStatus) Valid() bool { return slices.Contains(RepoStatuses(), s) }

// Owner is who owns a repository, as resolved at the activation that wrote it.
type Owner struct {
	Kind OwnerKind `json:"kind"`
	// Ref is a unit's key or a seat's handle.
	Ref string `json:"ref"`
}

// Protection is one rule over the refs a pattern matches.
type Protection struct {
	// Pattern is refs/heads/<glob> or refs/tags/<glob> (gitref's grammar).
	Pattern     string     `json:"pattern"`
	AllowForce  bool       `json:"allow_force,omitempty"`
	AllowDelete bool       `json:"allow_delete,omitempty"`
	DirectPush  DirectPush `json:"direct_push"`
}

// RepoSettings is everything about a repository its manager decides, carried
// whole by every record that sets it.
type RepoSettings struct {
	ManagedBy ManagedBy `json:"managed_by"`
	Owner     Owner     `json:"owner"`
	// DefaultBranch is a branch name, not a ref ("main").
	DefaultBranch string `json:"default_branch"`
	Description   string `json:"description,omitempty"`
	Archived      bool   `json:"archived,omitempty"`
	// Visibility is who may read it besides persons and Tier A tokens.
	Visibility Visibility `json:"visibility"`
	// Protections are sorted by Pattern and unique — the canonical order,
	// so two nodes deciding one declaration form byte-identical records.
	Protections []Protection `json:"protections,omitempty"`
	// ConfigEpoch is the activation these settings came from
	// (configplane.ActivationStamp), which the activation fence and the
	// reconcile's lifecycle rules compare.
	ConfigEpoch int64 `json:"config_epoch"`
}

// Validate refuses settings this domain will not record. The configuration
// validator refuses the same document earlier with the same rules, from the
// same gitref grammar; this is the domain's own guard, since the applier
// salvages rather than refuses.
func (s RepoSettings) Validate() error {
	switch {
	case !s.ManagedBy.Valid():
		return fmt.Errorf("%w: managed_by: %q is not one of %v", ErrInvalid, s.ManagedBy, ManagedBys())
	case !s.Owner.Kind.Valid():
		return fmt.Errorf("%w: owner.kind: %q is not one of %v", ErrInvalid, s.Owner.Kind, OwnerKinds())
	case strings.TrimSpace(s.Owner.Ref) == "":
		return fmt.Errorf("%w: owner.ref: a repository's owner names a unit or a seat", ErrInvalid)
	case !s.Visibility.Valid():
		return fmt.Errorf("%w: visibility: %q is not one of %v", ErrInvalid, s.Visibility, Visibilities())
	case !gitref.ValidBranch(s.DefaultBranch):
		return fmt.Errorf("%w: default_branch: %q is not a branch name", ErrInvalid, s.DefaultBranch)
	case gitref.ReservedSeatBranch(s.DefaultBranch):
		return fmt.Errorf("%w: default_branch: %q is reserved: refs/heads/seats holds only each seat's "+
			"own namespace, and no push creates it", ErrInvalid, s.DefaultBranch)
	case len(s.Description) > MaxDescriptionBytes:
		return fmt.Errorf("%w: description: %d bytes, past the %d-byte cap", ErrInvalid,
			len(s.Description), MaxDescriptionBytes)
	case !utf8.ValidString(s.Description):
		return fmt.Errorf("%w: description: not valid UTF-8", ErrInvalid)
	case len(s.Protections) > MaxProtectionsPerRepo:
		return fmt.Errorf("%w: protections: %d rules, past the cap of %d", ErrInvalid,
			len(s.Protections), MaxProtectionsPerRepo)
	}
	for i, p := range s.Protections {
		switch {
		case !gitref.ValidPattern(p.Pattern):
			return fmt.Errorf("%w: protections[%d].pattern: %q is not a protection pattern",
				ErrInvalid, i, p.Pattern)
		case !p.DirectPush.Valid():
			return fmt.Errorf("%w: protections[%d].direct_push: %q is not one of %v",
				ErrInvalid, i, p.DirectPush, DirectPushes())
		case p.AllowForce && strings.HasPrefix(p.Pattern, "refs/tags/"):
			return fmt.Errorf("%w: protections[%d].allow_force: a tag never moves, so "+
				"allow_force has nothing to allow on %s", ErrInvalid, i, p.Pattern)
		case i > 0 && s.Protections[i-1].Pattern >= p.Pattern:
			return fmt.Errorf("%w: protections[%d].pattern: %q is not after %q — the "+
				"rules are sorted by pattern and unique, so one declaration forms one "+
				"record on every node", ErrInvalid, i, p.Pattern, s.Protections[i-1].Pattern)
		}
	}
	return nil
}

// equal compares two settings field by field, Protections as slices (both are
// sorted by Validate's rule), and IGNORES ConfigEpoch — the activation a value
// came from is not a difference in the value.
func (s RepoSettings) equal(o RepoSettings) bool {
	return s.ManagedBy == o.ManagedBy && s.Owner == o.Owner &&
		s.DefaultBranch == o.DefaultBranch && s.Description == o.Description &&
		s.Archived == o.Archived && s.Visibility == o.Visibility &&
		slices.Equal(s.Protections, o.Protections)
}

// status is the status settings put an active repository in.
func (s RepoSettings) status() RepoStatus {
	if s.Archived {
		return RepoStatusArchived
	}
	return RepoStatusActive
}

// CreateRepo makes a repository. Its subject is the NAME.
type CreateRepo struct {
	// RepoID is minted by the WRITER and carried, never derived: a create
	// that lost its race must not have written a repository under an id
	// the winner also chose.
	RepoID   string       `json:"repo_id"`
	Name     string       `json:"name"`
	Settings RepoSettings `json:"settings"`
}

// SettingsRecord replaces a repository's settings whole.
type SettingsRecord struct {
	RepoID   string       `json:"repo_id"`
	Settings RepoSettings `json:"settings"`
}

// LifecycleRecord is a delete or a restore.
type LifecycleRecord struct {
	RepoID      string `json:"repo_id"`
	ConfigEpoch int64  `json:"config_epoch"`
	// Reason is a delete's why ("removed from code.repos").
	Reason string `json:"reason,omitempty"`
	// Settings is what a restore brings the repository back with; nil on a
	// delete.
	Settings *RepoSettings `json:"settings,omitempty"`
}

// PushBatch is one or more pushes to one repository, in the order they were
// decided — group commit's one record.
type PushBatch struct {
	RepoID string      `json:"repo_id"`
	Pushes []PushEntry `json:"pushes"`
}

// PushEntry is one push: in a record iff at least one of its commands is ok.
type PushEntry struct {
	// PushID is minted by the receiving node and stable across a failover,
	// which is what collapses a push decided twice.
	PushID    string    `json:"push_id"`
	Principal Principal `json:"principal"`
	// Node is the node that received the push.
	Node   string `json:"node"`
	Atomic bool   `json:"atomic,omitempty"`
	// Commands is every command the client sent, each with its result.
	Commands []Command `json:"commands"`
	// Pack is present iff the push carried new objects.
	Pack            *PackRef  `json:"pack,omitempty"`
	Introduced      []string  `json:"introduced,omitempty"`
	IntroducedCount int       `json:"introduced_count,omitempty"`
	ReceivedAt      time.Time `json:"received_at"`
}

// Principal is who acts on a repository: an operator credential and the
// person it is bound to ("" for nobody), or a seat.
type Principal struct {
	Kind       PrincipalKind `json:"kind"`
	OperatorID string        `json:"operator_id,omitempty"`
	Seat       string        `json:"seat,omitempty"`
}

// Validate refuses a principal no record or question may carry ([ErrInvalid],
// naming the field): an operator names its credential, and the person it is
// bound to by their handle or nobody; a seat names its handle and no
// credential. A kind this build does not know is refused, since nothing here
// could judge what one may do.
func (p Principal) Validate() error {
	if why := p.invalid(); why != "" {
		return fmt.Errorf("%w: %s", ErrInvalid, why)
	}
	return nil
}

// invalid is why p is no principal, or "" — the one rule [Principal.Validate]
// and a proposal's validation both state.
func (p Principal) invalid() string {
	switch p.Kind {
	case PrincipalOperator:
		switch {
		case strings.TrimSpace(p.OperatorID) == "":
			return "principal.operator_id: an operator names the credential it acts with"
		case p.Seat != "" && !org.ValidHandle(p.Seat):
			return fmt.Sprintf("principal.seat: %q is not a seat's handle", p.Seat)
		}
	case PrincipalAgent:
		switch {
		case !org.ValidHandle(p.Seat):
			return fmt.Sprintf("principal.seat: %q is not a seat's handle — a seat acts by its handle", p.Seat)
		case p.OperatorID != "":
			return "principal.operator_id: a seat acts as itself, with no operator credential"
		}
	default:
		return fmt.Sprintf("principal.kind: %q is not one of %v", p.Kind, PrincipalKinds())
	}
	return ""
}

// Command is one ref update and what became of it.
type Command struct {
	Ref    string        `json:"ref"`
	Old    string        `json:"old"`
	New    string        `json:"new"`
	Forced bool          `json:"forced,omitempty"`
	Result CommandResult `json:"result"`
	Reason Refusal       `json:"reason,omitempty"`
	// Detail is an answer's one-sentence specifics for an ng command
	// ("refs/heads/main is at 1a2b3c4; fetch first"). It travels in
	// proposals and answers and is NEVER in a record or a row: a sentence
	// is an answer, not state.
	Detail string `json:"detail,omitempty"`
}

// PackRef names ONE push's stored object, its PACK BUNDLE (bundle.go): the
// pack receive-pack stored, then its .idx, then its .rev, back to back — so a
// git node installs a push with one GET and three renames and runs no
// index-pack. Size, Hash and Object are the WHOLE object's (what the store
// verifies and the collector audits); the sections are cut by IndexBytes and
// ReverseBytes from the end.
type PackRef struct {
	// PackID is the push's id, for a push's pack.
	PackID string        `json:"pack_id"`
	Object objstore.Key  `json:"object"`
	Hash   objstore.Hash `json:"hash"`
	// Size is the whole bundle: PackBytes() + IndexBytes + ReverseBytes.
	Size         int64 `json:"size"`
	IndexBytes   int64 `json:"index_bytes"`
	ReverseBytes int64 `json:"reverse_bytes"`
	// GitChecksum is the pack's trailer — and each index's pack-checksum
	// field — in 40 hex digits.
	GitChecksum string `json:"git_checksum"`
	// Objects is the object count, from the index's fan-out.
	Objects int64 `json:"objects"`
}

// Content is the object a read of the bundle is checked against.
func (p PackRef) Content() objstore.Object {
	return objstore.Object{Key: p.Object, Hash: p.Hash, Size: p.Size}
}

// PackBytes is the pack section's size.
func (p PackRef) PackBytes() int64 { return p.Size - p.IndexBytes - p.ReverseBytes }

// PurgePart deletes one bounded part of a deleted repository's rows.
type PurgePart struct {
	RepoID string `json:"repo_id"`
	// Part is 1-based and must be the row's purge_parts + 1 — the guard that
	// makes a redelivered or stale part delete nothing more.
	Part int `json:"part"`
}

// Purge removes what remains of a repository and writes its deletion marker.
// It STATES the name whose claim it releases, because a release the record
// did not state would be a row deleted on one node's authority.
type Purge struct {
	RepoID string `json:"repo_id"`
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// Activated raises the activation fence to a configuration activation.
//
// PINNED AT [GateRecordVersion] FOR EVER, as every gate is: a node that
// deferred it would apply what the fence refuses.
type Activated struct {
	// ConfigEpoch is the activation (configplane.ActivationStamp).
	ConfigEpoch int64 `json:"config_epoch"`
}

// Eviction is a node's eviction from this log, or its readmission. PINNED AT
// [GateRecordVersion] FOR EVER.
type Eviction struct {
	V      int    `json:"v"`
	NodeID string `json:"node_id"`

	EvictedBy string    `json:"evicted_by,omitempty"`
	EvictedAt time.Time `json:"evicted_at"`

	// Readmitted makes this the INVERSE COMMIT rather than a delete, so an
	// eviction's whole history survives a replay.
	Readmitted bool `json:"readmitted,omitempty"`
}

// Generation is a reanchor's record: this log was recreated, and every
// position below this number is comparable and safely stale.
type Generation struct {
	V          int    `json:"v"`
	Generation uint32 `json:"generation"`

	By string `json:"by,omitempty"`

	// PrevHighest is the highest sequence the fleet had seen on the OLD
	// stream. Provenance only.
	PrevHighest uint64 `json:"prev_highest,omitempty"`

	// StreamCreatedAt is the new stream's own creation instant.
	StreamCreatedAt time.Time `json:"stream_created_at"`
}

// DecodeMutation reads the typed payload for one record.
//
// THE DISPATCH IS ON (kind, op) AND NOTHING ELSE, so a record whose pair this
// build does not know is an error naming both rather than a nil payload the
// applier would treat as an empty one: a pair this build does not know is a
// newer peer's record rather than an empty one.
func DecodeMutation(rec MutationRecord) (any, error) {
	kind, op := rec.Subject.Kind, rec.Op
	switch {
	case kind == KindName && op == OpCreate:
		return decodePayload[CreateRepo](rec)
	case kind == KindRepo && op == OpPush:
		return decodePayload[PushBatch](rec)
	case kind == KindRepo && op == OpSettings:
		return decodePayload[SettingsRecord](rec)
	case kind == KindRepo && (op == OpDelete || op == OpRestore):
		return decodePayload[LifecycleRecord](rec)
	case kind == KindRepo && op == OpPurgePart:
		return decodePayload[PurgePart](rec)
	case kind == KindRepo && op == OpPurge:
		return decodePayload[Purge](rec)
	case kind == KindCompany && op == OpActivate:
		return decodePayload[Activated](rec)
	case kind == KindEviction && op == OpEviction:
		return decodePayload[Eviction](rec)
	case kind == KindGeneration && op == OpGeneration:
		return decodePayload[Generation](rec)
	case kind == KindBarrier && op == OpBarrier, kind == KindTrimmed && op == OpTrimmed:
		// THE TWO PAYLOAD-FREE RECORDS, and nil is their value rather than
		// an absence: the applier's switch has a case for each that writes
		// nothing of this domain's.
		return nil, nil
	}
	return nil, fmt.Errorf("code: no payload shape for (%s, %s) — a record's "+
		"kind and op together name its payload, and a pair this build does not "+
		"know is a newer peer's record rather than an empty one", kind, op)
}

// decodePayload reads one typed payload.
func decodePayload[T any](rec MutationRecord) (T, error) {
	var out T
	if len(rec.Mutation) == 0 {
		return out, fmt.Errorf("code: the record on %s carries no payload, and "+
			"(%s, %s) needs one", rec.Subject, rec.Subject.Kind, rec.Op)
	}
	if err := json.Unmarshal(rec.Mutation, &out); err != nil {
		return out, fmt.Errorf("code: decode the %s payload on %s: %w", rec.Op, rec.Subject, err)
	}
	return out, nil
}
