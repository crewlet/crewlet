package code

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/statelog"
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

const (
	// ManagedConfig is a repository code.repos declares: the configuration
	// reconcile creates it, settles its settings and retires it, and nothing
	// else may.
	ManagedConfig ManagedBy = "config"

	// ManagedTools is a repository a tool made — a person's or a lead's
	// project, or a seat's scratch repository — whose lifecycle the tools'
	// requests decide ([Store.Lifecycle]) and no configuration reaches: the
	// reconcile refuses to adopt one ([ErrNotConfigManaged]), and the
	// activation fence never refuses one's record ([Applier.fenced]).
	//
	// NO versionedFields ROW, unlike the kind beside it ([RepoSettings.Kind]):
	// a build that knows only ManagedConfig applies a record carrying this
	// value as this build does — the applier stores a manager as written and
	// fences only a configuration's record — and its reconcile refuses to
	// adopt the repository as this one's does, so it reads nothing wrongly.
	ManagedTools ManagedBy = "tools"
)

// ManagedBys is the closed set.
func ManagedBys() []ManagedBy { return []ManagedBy{ManagedConfig, ManagedTools} }

// Valid reports a value this build knows.
func (m ManagedBy) Valid() bool { return slices.Contains(ManagedBys(), m) }

// RepoKind is what a repository is FOR, which decides its name's grammar, who
// may create it, whether it takes protections and whether it expires.
//
// THE KIND FOLLOWS THE NAME (gitref): a project's name is one label, a
// scratch repository's `<handle>.<slug>`, so a reader holding only a name
// knows which it is — which is why a rename keeps its kind's form for every
// actor ([lifecycleRefusal]).
type RepoKind string

const (
	// RepoProject is a repository the company works in: declared in
	// code.repos, or created by a person or by the owning unit's lead. It
	// never expires.
	RepoProject RepoKind = "project"
	// RepoScratch is a seat's own, named <handle>.<slug> after it, owned by
	// it, unprotected, and expiring once it has been idle
	// code.scratch.idle_days — never while a live turn holds it
	// (Retention.Holds).
	RepoScratch RepoKind = "scratch"
)

// RepoKinds is the closed set, in this order.
func RepoKinds() []RepoKind { return []RepoKind{RepoProject, RepoScratch} }

// Valid reports a kind this build knows. EMPTY IS NOT ONE, for [Visibility]'s
// reason: a record carries a repository's settings whole.
func (k RepoKind) Valid() bool { return slices.Contains(RepoKinds(), k) }

// KindOf is the kind a repository's name says it is: scratch for a seat's
// <handle>.<slug>, a project otherwise — what a create's settings must state
// ([RepoSettings.Validate] does not know the name; [LifecycleRequest.Validate]
// holds a tool's create to it, and a declaration is a project by the one
// constructor every reconcile's settings come from, [Declared.settingsAt]).
func KindOf(name string) RepoKind {
	if _, scratch := gitref.ScratchName(name); scratch {
		return RepoScratch
	}
	return RepoProject
}

// DirectPush is whether a protected ref takes a direct push.
type DirectPush string

const (
	// DirectPersons lets a person push to it directly, and NOBODY ELSE: a
	// seat's push to it is refused protected_direct ([ProtectionRefusal]),
	// so a seat's work reaches it when a person pushes it. Before seats
	// pushed it refused nothing, and read as "anyone who may push" it would
	// mean what a ref no rule matches already means.
	DirectPersons DirectPush = "persons"
	// DirectNone lets nobody push to it directly.
	DirectNone DirectPush = "none"
)

// DirectPushes is the closed set.
func DirectPushes() []DirectPush { return []DirectPush{DirectPersons, DirectNone} }

// Valid reports a value this build knows.
func (d DirectPush) Valid() bool { return slices.Contains(DirectPushes(), d) }

// Visibility is who may read a repository besides persons and the tokens of
// Tier A (ADR-0058): a SEAT'S standing narrows by it, and a person's never
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

// readsChart reports a principal whose push is judged from the CHART: every
// kind but an operator's. A person maintains every repository and a token
// bound to nobody reads every one — rules that need no chart (ADR-0055), so
// [GrantOf] never consults one for them — while a seat's standing is its
// relation on the chart, and so is everything the push decide holds a seat's
// push to beside it: its grant, the protections that take a person's push
// alone, its share of a repository outside its unit, its day's quota. A node
// that holds no chart, or one confirmed behind the company's configuration,
// decides none of such a push ([CommitterOptions.SeatRules],
// [CommitterOptions.Current]).
func (p PrincipalKind) readsChart() bool { return p != PrincipalOperator }

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

// PackKind is what added a pack row to code_packs: which record wrote it.
//
// READ BY NOTHING THAT DECIDES: a cache installs a pack by its [PackState],
// and a decide judges one by its state, its part and its size, so a row of a
// kind this build does not know — a newer build's — is installed and judged
// as any other. It is the repository's history of what rewrote its packs, for
// whoever reads the rows. Every value is in the base format, under the code
// domain's stated condition ([OpCompact], [OpExpungeBegin]).
type PackKind string

const (
	// PackPush is a push's pack: the bundle its receive-pack stored.
	PackPush PackKind = "push"
	// PackCompact is a compaction's output.
	PackCompact PackKind = "compact"
	// PackMerge is the pack of objects a change's merge record builds (M5):
	// no record of this build writes one.
	PackMerge PackKind = "merge"
	// PackCandidate is the pack a speculative queue's candidate is recorded
	// with (M5): no record of this build writes one.
	PackCandidate PackKind = "candidate"
	// PackLand is the pack a post-land lane's landing records (M5): no
	// record of this build writes one.
	PackLand PackKind = "land"
	// PackExpunge is a pack an expunge's begin keeps: what of its inputs a
	// ref reached, live.
	PackExpunge PackKind = "expunge"
	// PackCruft is a pack an expunge's begin leaves dropping: what of its
	// inputs no ref reached.
	PackCruft PackKind = "cruft"
	// PackRescue is a pack an expunge's commit keeps of its dropping packs:
	// what a push that landed before its begin made reachable again, live.
	PackRescue PackKind = "rescue"
)

// PackKinds is the closed set, in the order the records that write them were
// added — the merge's three among them, which the merge records extend, so
// one list names every kind code_packs may hold.
func PackKinds() []PackKind {
	return []PackKind{PackPush, PackCompact, PackMerge, PackCandidate, PackLand, PackExpunge, PackCruft,
		PackRescue}
}

// Valid reports a kind this build knows.
func (k PackKind) Valid() bool { return slices.Contains(PackKinds(), k) }

// PackState is where a pack row is in its repository's life.
type PackState string

const (
	// PackLive is a pack the repository holds: every cache installs it among
	// the repository's objects, every receiver lets a push rely on it, and
	// every rewrite may take it as an input.
	PackLive PackState = "live"
	// PackDropping is a pack an expunge's begin left holding what no ref
	// reached when it was planned: a cache still serves it, so a reader
	// already on one of its objects finishes, and no new push may rely on it
	// — until the expunge's commit retires it into code_packs_retired. Never
	// a compaction's input, a published layer's pack or an expunge's input,
	// and never counted among the repository's live packs, bytes or objects.
	PackDropping PackState = "dropping"
)

// PackStates is the closed set, in this order.
func PackStates() []PackState { return []PackState{PackLive, PackDropping} }

// Valid reports a state this build knows.
func (s PackState) Valid() bool { return slices.Contains(PackStates(), s) }

// Owner is who owns a repository, as resolved at the activation that wrote it.
type Owner struct {
	Kind OwnerKind `json:"kind"`
	// Ref is a unit's key or a seat's handle.
	Ref string `json:"ref"`
}

// PathOwner is one owner of a repository's paths: a unit, by its key, or a
// seat, by its handle — and the globs over the repository's tree it owns
// (gitref.ParsePathGlob), sorted and each once. What a path owner is for is
// REVIEW: a change touching its paths asks it for one. It GRANTS NOTHING — a
// seat's standing on a repository is its grant ([GrantOf]), read off the chart
// and its tier, and no path owner is read there — and so it is never on a
// [RepoRef], which every read, push and fetch decides from.
type PathOwner struct {
	Owner Owner    `json:"owner"`
	Paths []string `json:"paths"`
}

// equal compares two path owners, their globs as slices.
func (p PathOwner) equal(o PathOwner) bool {
	return p.Owner == o.Owner && slices.Equal(p.Paths, o.Paths)
}

// comparePathOwners is the canonical order of a repository's path owners: by
// the owner's kind, then its reference — every unit and every seat once, the
// one order every node forms a record in and [RepoSettings.Validate] holds a
// record to.
func comparePathOwners(a, b PathOwner) int {
	if c := strings.Compare(string(a.Owner.Kind), string(b.Owner.Kind)); c != 0 {
		return c
	}
	return strings.Compare(a.Owner.Ref, b.Owner.Ref)
}

// CanonicalPathOwners is owners in the ONE FORM a record carries them in: by
// owner ([comparePathOwners]), each owner's globs sorted — a copy, leaving
// owners as it was. THE ONE PLACE the order is formed, read by the engine's
// reconcile for a declaration and by a lifecycle request's decide for a
// change, so every node deciding one declaration forms byte-identical records.
// It removes nothing: an owner or a glob named twice is the caller's to
// refuse, and [RepoSettings.Validate] refuses it.
func CanonicalPathOwners(owners []PathOwner) []PathOwner {
	if len(owners) == 0 {
		return nil
	}
	out := make([]PathOwner, len(owners))
	for i, po := range owners {
		out[i] = PathOwner{Owner: po.Owner, Paths: slices.Sorted(slices.Values(po.Paths))}
	}
	slices.SortStableFunc(out, comparePathOwners)
	return out
}

// pathOwnersInvalid is why owners are no repository's path owners in the form
// a record carries them, or "": at most [MaxPathOwners], each a unit or a seat
// naming itself, with 1 to [MaxPathOwnerPaths] globs gitref reads, sorted and
// each once, and the owners sorted and each once ([comparePathOwners]) — and
// the whole at most [MaxPathOwnersBytes] as a record encodes it, judged last,
// since owners refused for what one says are refused for that.
func pathOwnersInvalid(owners []PathOwner) string {
	if len(owners) > MaxPathOwners {
		return fmt.Sprintf("path_owners: %d owners, past the cap of %d", len(owners), MaxPathOwners)
	}
	for i, po := range owners {
		at := fmt.Sprintf("path_owners[%d]", i)
		switch {
		case !po.Owner.Kind.Valid():
			return fmt.Sprintf("%s.owner.kind: %q is not one of %v", at, po.Owner.Kind, OwnerKinds())
		case strings.TrimSpace(po.Owner.Ref) == "":
			return at + ".owner.ref: a path owner names a unit or a seat"
		case len(po.Paths) == 0:
			return at + ".paths: a path owner owns at least one glob"
		case len(po.Paths) > MaxPathOwnerPaths:
			return fmt.Sprintf("%s.paths: %d globs, past the cap of %d", at, len(po.Paths), MaxPathOwnerPaths)
		case i > 0 && comparePathOwners(owners[i-1], po) >= 0:
			return fmt.Sprintf("%s.owner: %s %s is not after %s %s — the owners are sorted by kind and "+
				"reference and each named once, so one declaration forms one record on every node", at,
				po.Owner.Kind, po.Owner.Ref, owners[i-1].Owner.Kind, owners[i-1].Owner.Ref)
		}
		for k, glob := range po.Paths {
			if _, err := gitref.ParsePathGlob(glob); err != nil {
				return fmt.Sprintf("%s.paths[%d]: %q is not a path glob: %v", at, k, glob, err)
			}
			if k > 0 && po.Paths[k-1] >= glob {
				return fmt.Sprintf("%s.paths[%d]: %q is not after %q — an owner's globs are sorted and "+
					"each named once", at, k, glob, po.Paths[k-1])
			}
		}
	}
	if n := pathOwnersBytes(owners); n > MaxPathOwnersBytes {
		return fmt.Sprintf("path_owners: %d bytes as a record carries them, past the %d a repository's path "+
			"owners may weigh: own a directory with dir/** rather than each of its files", n, MaxPathOwnersBytes)
	}
	return ""
}

// Protection is one rule over the refs a pattern matches: what a push to one
// of them may do, and — on a branch rule — what a merge into one needs.
//
// COMPARED BY [Protection.Equal], never ==: its gate is a pointer.
type Protection struct {
	// Pattern is refs/heads/<glob> or refs/tags/<glob> (gitref's grammar).
	Pattern     string     `json:"pattern"`
	AllowForce  bool       `json:"allow_force,omitempty"`
	AllowDelete bool       `json:"allow_delete,omitempty"`
	DirectPush  DirectPush `json:"direct_push"`
	// Gate is the rule's merge gate ([MergeGate]), nil for a rule with none
	// — and always nil on a tag rule: a tag never moves, so no merge reaches
	// one. A push decides nothing by it: a push is held to the fields above,
	// and the merge gate holds the merges a change makes.
	Gate *MergeGate `json:"gate,omitempty"`
}

// invalid is why p is no rule a record may carry, or "", naming the field: a
// protection pattern, a direct_push this build knows, no allow_force and no
// merge gate on a tag pattern, since a tag never moves, and a gate a record
// may carry ([MergeGate]'s own rules, named below gate). THE ONE RULE for a
// rule alone, read by [RepoSettings.Validate] for every rule a record carries
// and by a protect request's own validation for the one it sends
// ([LifecycleRequest.Validate]); a rule's place among the others — sorted by
// pattern, each once — is the list's to judge.
func (p Protection) invalid() string {
	switch {
	case !gitref.ValidPattern(p.Pattern):
		return fmt.Sprintf("pattern: %q is not a protection pattern", p.Pattern)
	case !p.DirectPush.Valid():
		return fmt.Sprintf("direct_push: %q is not one of %v", p.DirectPush, DirectPushes())
	case p.AllowForce && strings.HasPrefix(p.Pattern, "refs/tags/"):
		return fmt.Sprintf("allow_force: a tag never moves, so allow_force has nothing to allow on %s",
			p.Pattern)
	case p.Gate != nil && strings.HasPrefix(p.Pattern, "refs/tags/"):
		return fmt.Sprintf("gate: a tag never moves, so no merge reaches %s and a merge gate has nothing "+
			"to hold", p.Pattern)
	case p.Gate == nil:
		return ""
	}
	switch field, why := p.Gate.invalid(); {
	case why == "":
		return ""
	case field == "":
		return "gate: " + why
	default:
		return "gate." + field + ": " + why
	}
}

// RepoSettings is everything about a repository its manager decides, carried
// whole by every record that sets it.
type RepoSettings struct {
	ManagedBy ManagedBy `json:"managed_by"`
	// Kind is what the repository is for, which its name says ([KindOf]):
	// every create states it, a declaration's always a project
	// ([Declared.settingsAt]), and no record changes it — a rename keeps its
	// kind's form.
	//
	// BASE FORMAT, AS A STATED CONDITION: no build that carries the code
	// domain has run without this field — none of the domain is on main, so
	// no image and no release holds it — and while that is so, every build
	// that meets a record carrying settings reads it. Once a build without it
	// has run, each op whose payload carries settings takes {Name:
	// "RepoSettings.Kind", Since: 2, Op: <the op: create, settings, restore>,
	// Path: []string{"mutation", "settings", "kind"}} in versionedFields, with
	// RecordVersion at 2, its line under "What each version added" and a
	// Carrying record in the statelogtest candidate — such a build would apply
	// a create without its kind, and the row it wrote would read as no kind
	// on that node for ever.
	Kind  RepoKind `json:"kind"`
	Owner Owner    `json:"owner"`
	// DefaultBranch is a branch name, not a ref ("main").
	DefaultBranch string `json:"default_branch"`
	Description   string `json:"description,omitempty"`
	Archived      bool   `json:"archived,omitempty"`
	// Visibility is who may read it besides persons and Tier A tokens.
	Visibility Visibility `json:"visibility"`
	// Protections are sorted by Pattern and unique — the canonical order,
	// so two nodes deciding one declaration form byte-identical records —
	// each one's gate in its one form too ([CanonicalGate]).
	Protections []Protection `json:"protections,omitempty"`
	// PathOwners are who owns which of its paths, in the one form a record
	// carries them in ([CanonicalPathOwners]): at most [MaxPathOwners], each
	// owner once and each one's globs sorted and once — none on a scratch
	// repository, which is its seat's own.
	//
	// BASE FORMAT, AS A STATED CONDITION: no build that carries the code
	// domain has run without this field — none of the domain is on main, so
	// no image and no release holds it — and while that is so, every build
	// that meets a record carrying settings reads it. Once a build without it
	// has run, each op whose payload carries settings takes {Name:
	// "RepoSettings.PathOwners", Since: 2, Op: <the op: create, settings,
	// restore>, Path: []string{"mutation", "settings", "path_owners"}} in
	// versionedFields, with RecordVersion at 2, its line under "What each
	// version added" and a Carrying record in the statelogtest candidate —
	// such a build would apply settings without their path owners, and a
	// node running it would hold none of the rows every other node holds.
	PathOwners []PathOwner `json:"path_owners,omitempty"`
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
	case !s.Kind.Valid():
		return fmt.Errorf("%w: kind: %q is not one of %v", ErrInvalid, s.Kind, RepoKinds())
	case s.ManagedBy == ManagedTools && s.ConfigEpoch != 0:
		return fmt.Errorf("%w: config_epoch: a tool's repository comes from no configuration "+
			"activation, so its settings name none", ErrInvalid)
	case s.ManagedBy == ManagedConfig && s.Kind != RepoProject:
		return fmt.Errorf("%w: kind: a repository code.repos declares is a project", ErrInvalid)
	case s.Kind == RepoScratch && s.Owner.Kind != OwnerSeat:
		return fmt.Errorf("%w: owner: a scratch repository belongs to the seat whose handle it carries",
			ErrInvalid)
	case s.Kind == RepoScratch && len(s.Protections) > 0:
		return fmt.Errorf("%w: protections: a scratch repository takes no protections — it is its "+
			"seat's own", ErrInvalid)
	case s.Kind == RepoScratch && len(s.PathOwners) > 0:
		return fmt.Errorf("%w: path_owners: a scratch repository takes no path owners — it is its "+
			"seat's own", ErrInvalid)
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
		if why := p.invalid(); why != "" {
			return fmt.Errorf("%w: protections[%d].%s", ErrInvalid, i, why)
		}
		if i > 0 && s.Protections[i-1].Pattern >= p.Pattern {
			return fmt.Errorf("%w: protections[%d].pattern: %q is not after %q — the "+
				"rules are sorted by pattern and unique, so one declaration forms one "+
				"record on every node", ErrInvalid, i, p.Pattern, s.Protections[i-1].Pattern)
		}
	}
	if why := pathOwnersInvalid(s.PathOwners); why != "" {
		return fmt.Errorf("%w: %s", ErrInvalid, why)
	}
	return nil
}

// equal compares two settings field by field, Protections and PathOwners as
// slices (both are sorted by Validate's rule, and each path owner's globs too)
// of values compared by value — a rule's gate is a pointer, which
// [Protection.Equal] follows — and IGNORES ConfigEpoch: the activation a value
// came from is not a difference in the value.
func (s RepoSettings) equal(o RepoSettings) bool {
	return s.ManagedBy == o.ManagedBy && s.Kind == o.Kind && s.Owner == o.Owner &&
		s.DefaultBranch == o.DefaultBranch && s.Description == o.Description &&
		s.Archived == o.Archived && s.Visibility == o.Visibility &&
		slices.EqualFunc(s.Protections, o.Protections, Protection.Equal) &&
		slices.EqualFunc(s.PathOwners, o.PathOwners, PathOwner.equal)
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
	// RepoID is chosen by the WRITER and carried, never derived by the
	// applier: a create that lost its race must not have written a
	// repository under an id the winner also chose. A configuration's create
	// mints it once per call; a tool's derives it from the request's own
	// operation id ([Store.Lifecycle]), so a retry whose ledger row was swept
	// names the same repository, while two requests are two operations and
	// never share one.
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

// Rename claims To for a tool-managed repository, on To's name subject, and
// leaves From redirecting to it for [RedirectWindow] ([Store.Lifecycle]).
//
// BASE FORMAT, AS A STATED CONDITION: no build that carries the code domain
// has run without this op — none of the domain is on main, so no image and no
// release holds it — and while that is so, every build that meets a record of
// it reads it. Once a build without it has run, it takes {Name:
// "RecordEnvelope.Op rename", Since: 2, Path: []string{"op"}, Equals:
// string(OpRename)} in versionedFields, with RecordVersion at 2, its line under
// "What each version added", a Carrying record in the statelogtest candidate,
// and the op excepted from the stamp test's base-format loop — an older build
// would otherwise FAIL on the record at DecodeMutation's default arm rather
// than retain it. It is no gate.
type Rename struct {
	RepoID string `json:"repo_id"`
	From   string `json:"from"`
	To     string `json:"to"`
}

// ReleaseName frees a renamed repository's old name once its redirect has
// ended, on that name's subject — the code-retention pass's
// ([Store.ReleaseName]).
//
// BASE FORMAT, AS A STATED CONDITION, as [Rename] states it; once a build
// without it has run, it takes {Name: "RecordEnvelope.Op release_name", Since:
// 2, Path: []string{"op"}, Equals: string(OpReleaseName)} with the same
// companions. It is no gate.
type ReleaseName struct {
	Name   string `json:"name"`
	RepoID string `json:"repo_id"`
}

// RedirectWindow is how long a renamed repository's old name keeps
// redirecting: a MONTH, so a clone's remote and a CI configuration naming the
// old URL keep working while their owners update them. A product judgment, not
// a measurement; the release follows the window's end within one pass of the
// code-retention duty ([RetentionInterval]), and until it lands the name stays
// the repository's — every node at one position answers one way, which a
// comparison with each node's clock would not.
const RedirectWindow = 30 * 24 * time.Hour

// Hold is a LIVE TURN'S HOLD on a scratch repository, recorded on its subject
// so that an expiry pass which read the live turns' holds before this one
// existed is ordered against it: the broker arbitrates the two on the one
// subject `repo.<id>`, so whichever lands second decides from the first
// ([Store.ExpireScratch] refuses a repository with a record past the page it
// listed; a hold decided after the delete answers repo_deleted). Its apply
// moves the repository's version and NOTHING else — never its idle clock, which
// only activity moves ([Retention]).
//
// BASE FORMAT, AS A STATED CONDITION, as [Rename] states it; once a build
// without it has run, it takes {Name: "RecordEnvelope.Op hold", Since: 2, Path:
// []string{"op"}, Equals: string(OpHold)} with the same companions. It is no
// gate.
type Hold struct {
	RepoID string `json:"repo_id"`
	// TurnID is the turn holding it (turnctx.Turn.RunID), at most
	// MaxProvenanceBytes, so a reader of the log can say whose hold ordered an
	// expiry.
	TurnID string `json:"turn_id"`
}

// HoldRecordAfter is how long a scratch repository must have been idle before
// a turn's hold on it is also RECORDED ([Hold]): one day — the floor of
// code.scratch.idle_days (config.CodeScratchIdleDaysFloor) — less two hours.
//
// WHY A RECORD AT ALL, and only past this: a pass fixes its cutoff BEFORE it
// reads the live turns' holds, so a hold it could not see was taken after its
// cutoff was fixed. The pass's cutoff and this threshold are each ONE node's
// clock compared with the same broker-stamped last_activity_at, so the broker's
// clock cancels and only the two nodes' disagreement matters: a repository
// idle less than this by the holding node's clock, read after the hold was
// taken, is past no in-flight pass's cutoff, whatever that pass's epoch says
// idle_days is, as long as the two clocks disagree by under two hours. Two
// hours covers twice the references gate's assumed skew between two nodes
// (objstore.AssumedClockSkew, an hour, which the collector's grace rests on
// too), and exactly two nodes each within an hour of the broker, which the
// `clock_skew` alarm watches. Only an older repository can be raced, and for
// it the record makes the broker decide the race. Held by
// references.TestAHoldRecordCoversEveryPassInFlight.
//
// WHAT IT COSTS IS A RECORD A TURN, not one a repository: a hold moves no
// clock, so every turn that names a repository idle this long records again,
// until a push moves its clock or the expiry takes it — at the default seven
// idle days, the turns of the six days and two hours before it falls due.
// That window is long because the threshold is idle_days' FLOOR rather than
// the idle_days in force, which a pass in flight may judge by another epoch's
// (above). One record is small (TestAHoldRecordCostsTheLogOneSmallRecord),
// and internal/tracker's codeLogNegligible weighs the turns' records against
// the code log's day.
const HoldRecordAfter = 22 * time.Hour

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
	// Via is how the push's pack was built — received by git, or built by
	// the engine itself — as its proposer stamped it ([Proposal.Via]), and
	// what its push row's via column says.
	//
	// BASE FORMAT, AS A STATED CONDITION: no build that carries the code
	// domain has run without this field — none of the domain is on main, so
	// no image and no release holds it — and while that is so, every build
	// that meets a push record reads it. Once a build without it has run, a
	// push the ENGINE built takes {Name: "PushEntry.Via engine", Since: 2,
	// Op: "push", Path: []string{"mutation", "pushes", "via"}, Equals:
	// string(ViaEngine)} in versionedFields, with RecordVersion at 2, its
	// line under "What each version added" and a Carrying record in the
	// statelogtest candidate — such a build reads every push as one git
	// received, which is true of a push git received and false of the
	// engine's, so only the engine's needs the newer reader, and the key
	// alone would stamp every push — and the migration that adds the column
	// writes `git` into the rows it finds there, every one a push such a
	// build received, since the column's default names no way.
	Via PushVia `json:"via"`
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
	// Day is the company day a SEAT'S entry was counted against by its
	// decide's quota (code.limits.pushes_per_seat_day): period.Day's label
	// on the deciding node's clock in the company's zone, STAMPED BY THE
	// COMMITTER on every agent entry its record carries and never sent by a
	// proposer — so the apply adds the entry to the seat's count of that day
	// (code_seat_days) with no clock and no zone of its own, identically on
	// every node. Empty on every other entry.
	//
	// BASE FORMAT, AS A STATED CONDITION: no build that carries the code
	// domain has run without this field — none of the domain is on main, so
	// no image and no release holds it — and while that is so, every build
	// that meets a push record reads it. Once a build without it has run, it
	// takes {Name: "PushEntry.Day", Since: 2, Op: "push", Path:
	// []string{"mutation", "pushes", "day"}} in versionedFields, with
	// RecordVersion at 2, its line under "What each version added" and a
	// Carrying record in the statelogtest candidate — such a build would
	// apply a seat's push without counting it, and every committer of it
	// would read the seat's day short by that push.
	Day string `json:"day,omitempty"`
	// Change is the change this push opens from its options
	// ([ChangeOption], pushchange.go): STAMPED BY ITS DECIDE, and only where
	// the decide opens one — its target resolved to the repository's default
	// branch where the options named none — so the record's apply opens it on
	// the entry's one branch command, at the head that command set, from the
	// record alone (revisions.go). A push whose options asked for no change,
	// whose change was refused, or whose source already heads a live change
	// carries none: nothing of a change a push did not open is state.
	//
	// BASE FORMAT, AS A STATED CONDITION: no build that carries the code
	// domain has run without this field — none of the domain is on main, so
	// no image and no release holds it — and while that is so, every build
	// that meets a push record reads it. Once a build without it has run, it
	// takes {Name: "PushEntry.Change", Since: 2, Op: "push", Path:
	// []string{"mutation", "pushes", "change"}} in versionedFields, with
	// RecordVersion at 2, its line under "What each version added" and a
	// Carrying record in the statelogtest candidate — such a build would
	// apply the push and open no change, and every data node of it would
	// hold the repository short of the change its pusher was told it opened.
	Change *ChangeOption `json:"change,omitempty"`
}

// Principal is who acts on a repository: an operator credential and the
// person it is bound to ("" for nobody), or a seat — and on a seat's push, the
// run behind it.
type Principal struct {
	Kind       PrincipalKind `json:"kind"`
	OperatorID string        `json:"operator_id,omitempty"`
	Seat       string        `json:"seat,omitempty"`
	// Run is what made a SEAT'S PUSH — the turn, the work item it was charged
	// to and the model, and for a coding box's push its run and its coding
	// agent ([RunProvenance.RunID]) — and nothing on any other principal: an
	// operator acts with a credential and no run, and a seat that only READS
	// names none (a read's asker is judged by who it is, never by what it is
	// doing).
	// THE ZERO VALUE IS "NO RUN", and a value, not a pointer: one spelling of
	// the absence, omitted from the encoding whole, and a principal stays a
	// value the cases and the index of pushes compare with ==.
	//
	// BASE FORMAT, AS A STATED CONDITION: no build that carries the code
	// domain has run without this field — none of the domain is on main, so
	// no image and no release holds it — and while that is so, every build
	// that meets a push record reads it. Once a build without it has run, it
	// takes {Name: "Principal.Run", Since: 2, Op: "push", Path:
	// []string{"mutation", "pushes", "principal", "run"}} in
	// versionedFields, with RecordVersion at 2, its line under "What each
	// version added" and a Carrying record in the statelogtest candidate — an
	// older build would otherwise apply a seat's push as a row naming no turn
	// and no actor at all, since it writes a push's credential as its actor
	// and a seat holds none; every seat's push names its run, so the row
	// holds the actor's rule too.
	Run RunProvenance `json:"run,omitzero"`
}

// Validate refuses a principal no record or question may carry ([ErrInvalid],
// naming the field): an operator names its credential, and the person it is
// bound to by their handle or nobody, and no run; a seat names its handle and
// no credential, and a run only as [RunProvenance.Validate] admits one. A kind
// this build does not know is refused, since nothing here could judge what
// one may do.
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
		case !p.Run.IsZero():
			return "principal.run: an operator acts with a credential, and only a seat's push names the run " +
				"behind it"
		}
	case PrincipalAgent:
		switch {
		case !org.ValidHandle(p.Seat):
			return fmt.Sprintf("principal.seat: %q is not a seat's handle — a seat acts by its handle", p.Seat)
		case p.OperatorID != "":
			return "principal.operator_id: a seat acts as itself, with no operator credential"
		case !p.Run.IsZero():
			if why := p.Run.invalid(); why != "" {
				return "principal.run." + why
			}
		}
	default:
		return fmt.Sprintf("principal.kind: %q is not one of %v", p.Kind, PrincipalKinds())
	}
	return ""
}

// Actor is who a record names as p's actor: the credential an operator acted
// with — the person's attribution, as every record a write leaves names it
// (iam.ActorFor), the seat they hold beside it — and a seat's own handle,
// since a seat acts as itself with no credential. THE ONE RULE for every
// record of a principal: a push row's actor, and the reader every read of a
// `visibility: unit` repository is recorded as, which the usage domain
// refuses empty.
func (p Principal) Actor() string {
	if p.Kind == PrincipalAgent {
		return p.Seat
	}
	return p.OperatorID
}

// RunProvenance is what made a seat's push, as the SERVER stamps it from the
// call that made it — the tool call's turn, its work item and its model, and
// for a coding box's push the run that pushed and its coding agent — and
// never as a commit says it: a commit's headers and trailers are written by
// whoever made the commit, a seat or the coding agent in its box, and the
// engine trusts nothing it did not see itself. Every field is bounded
// ([MaxProvenanceBytes]) and spelled as the push row stores it.
type RunProvenance struct {
	// TurnID is the turn that made the write (turnctx.Turn.RunID) — for a
	// box's push, the turn that launched its run — the one field every run
	// names.
	TurnID string `json:"turn_id"`
	// WorkBackend and WorkKey are the work item the turn is charged to
	// (ADR-0022) — its tracker and the key that tracker shows — empty for a
	// turn charged to none.
	WorkBackend string `json:"work_backend,omitempty"`
	WorkKey     string `json:"work_key,omitempty"`
	// Model is the executor model of the round that called the tool, empty
	// where the call named none.
	Model string `json:"model,omitempty"`
	// RunID is the CODING RUN behind a box's push — its launch, the id the
	// engine minted when it launched the box (sandbox.PendingRun.LaunchID), a
	// UUID in its canonical spelling — and CodingAgent the coding agent the
	// box ran ("claude-code", "opencode"). The code host stamps both from the
	// run's row when it admits the box: what the engine's account of a run's
	// delivery reads its pushes by (the `run_pushes` read). BOTH OR NEITHER,
	// and both only on a push git received from a box ([Proposal.Validate]):
	// an engine-built write's run is the turn that called the tool, and a
	// person acts with no run at all.
	//
	// BASE FORMAT, AS A STATED CONDITION: no build that carries the code
	// domain has run without these fields — none of the domain is on main, so
	// no image and no release holds them — and while that is so, every build
	// that meets a push record reads them. Once a build without them has run,
	// they take {Name: "RunProvenance.RunID", Since: 3, Op: "push", Path:
	// []string{"mutation", "pushes", "principal", "run", "run_id"}} and
	// {Name: "RunProvenance.CodingAgent", Since: 3, Op: "push", Path:
	// []string{"mutation", "pushes", "principal", "run", "coding_agent"}} in
	// versionedFields, with RecordVersion at 3, its line under "What each
	// version added" and a Carrying record in the statelogtest candidate. A
	// version of their own, after Principal.Run's 2: a record carrying them
	// carries the turn's run too, so a reader at version 2 would apply a box's
	// push as one no coding run made — its row without run_id and coding_agent
	// on that node alone, which no run_pushes read of that node finds.
	RunID       string `json:"run_id,omitempty"`
	CodingAgent string `json:"coding_agent,omitempty"`
}

// IsZero reports a principal's run that names nothing: no run at all, the
// spelling every principal but a seat's push carries.
func (r RunProvenance) IsZero() bool { return r == RunProvenance{} }

// MaxProvenanceBytes bounds each [RunProvenance] field: a turn id and a run id
// are UUIDs (36 bytes), a work key a tracker's or a vendor's (Jira's are under
// 32), a model id a provider's (the longest Bedrock ARN form is about 120), a
// coding agent a name the sandbox configuration chose ("claude-code"). 256 is
// past every one of them, and what the bound buys is a push entry whose size is
// known before it is measured: six fields at it are about nine kibibytes of a
// record's mebibyte even where JSON spells every byte in six.
const MaxProvenanceBytes = 256

// Validate refuses a run no push may carry ([ErrInvalid], naming the field):
// one that names no turn, a run id that is not a UUID in its canonical
// spelling, a coding run without its coding agent or an agent without its run,
// or a field past [MaxProvenanceBytes], not valid UTF-8, or holding a control
// character. A stamping site passes every field it copies from outside the
// code domain through [FitProvenance] first, so a field this refuses is the
// engine's own bug, never a seat's.
func (r RunProvenance) Validate() error {
	if why := r.invalid(); why != "" {
		return fmt.Errorf("%w: run.%s", ErrInvalid, why)
	}
	return nil
}

// invalid is why r is no run, or "" — the field named as its JSON does.
//
// A RUN ID IS THE LAUNCH'S UUID, CANONICAL, never a label fitted to the bound:
// the engine minted it, and a `run_pushes` read finds the run's pushes by it
// whole, so a run id spelled any other way would land a push no read of its
// run ever names.
func (r RunProvenance) invalid() string {
	switch {
	case r.TurnID == "":
		return "turn_id: a seat's push names the turn that made it"
	case r.RunID != "" && !canonicalUUID(r.RunID):
		return fmt.Sprintf("run_id: %q is not a UUID in its canonical lowercase spelling — a coding run is "+
			"named by the launch id the engine minted", r.RunID)
	case r.RunID != "" && r.CodingAgent == "":
		return "coding_agent: a coding run's push names the coding agent its box ran"
	case r.RunID == "" && r.CodingAgent != "":
		return "run_id: a coding agent's push names the coding run it pushed for"
	}
	for _, f := range []struct{ name, value string }{
		{"turn_id", r.TurnID}, {"work_backend", r.WorkBackend}, {"work_key", r.WorkKey}, {"model", r.Model},
		{"coding_agent", r.CodingAgent},
	} {
		switch {
		case len(f.value) > MaxProvenanceBytes:
			return fmt.Sprintf("%s: %d bytes, past the %d one field may hold (MaxProvenanceBytes)", f.name,
				len(f.value), MaxProvenanceBytes)
		case !utf8.ValidString(f.value):
			return f.name + ": not valid UTF-8"
		case strings.ContainsFunc(f.value, unicode.IsControl):
			return f.name + ": holds a control character"
		}
	}
	return ""
}

// canonicalUUID reports s as a UUID in its canonical lowercase spelling — the
// one spelling an id compared whole, as a row's key or a read's argument, is
// found by.
func canonicalUUID(s string) bool {
	id, err := uuid.Parse(s)
	return err == nil && id.String() == s
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

// Compaction is one compaction of one repository's packs: the packs it
// replaces and the bundles that replace them, decided by the repository's
// committer ([Committer.ProposeCompaction]) and applied as ONE record that adds
// the outputs' rows and moves the inputs' into code_packs_retired.
//
// A MERGE MAKES NO REACHABILITY JUDGEMENT (ADR-0060): its outputs hold every
// object its inputs held, so no push decided against the inputs can land a ref
// whose closure a compaction removed, and no fence is needed beside the decide's own
// check that every input is still live. The outputs are built over the inputs
// with no pack excluded — an object an input shares with a pack that is not
// an input is written again rather than left to that pack, because two
// compactions decided from different snapshots may each retire the pack the
// other relied on. An expunge, which drops objects, is the one rewrite that
// needs a fence — the drop epoch its begin moves, which every push is held to
// ([ExpungeBegin]) — and it is not this record.
//
// Every field is in the base format, under the condition [OpCompact] states.
type Compaction struct {
	RepoID string `json:"repo_id"`
	// CompactionID is minted ONCE by the node that builds the compaction — a
	// bare UUIDv7 (statelog.NewOpID with no name) — and is the record's
	// operation id: a proposal asked again of the repository's runner-up
	// after a committer went silent is the same compaction, collapsed by the
	// operation ledger, and its instant is what the ledger judges a late
	// retry by and what each output's id is derived at
	// ([DeriveOutputPackID]).
	CompactionID string `json:"compaction_id"`
	// Node is the git node that built it, for whoever reads the log.
	Node string `json:"node"`
	// Seen is the code log's position the builder planned at — the sync
	// its cache answered the inputs from. The committer decides at or past
	// it (the publish's session floor), and the record keeps it, so a
	// compaction says what its author read without a lookup. ONE FIELD: the
	// proposal carries no second copy, and nothing stamps it again.
	Seen statelog.Position `json:"seen"`
	// Retire are the pack ids the outputs replace, sorted and distinct,
	// each live and not full ([PackRow.Full]) when the committer decides.
	Retire []string `json:"retire"`
	// Add are the outputs, each one bundle in the stored format at most
	// MaxPackBytes WHOLE — the pack and both its indexes ([PackRef.Check])
	// — the n-th's pack id [DeriveOutputPackID] of CompactionID and n,
	// in the order the build wrote them.
	Add []PackRef `json:"add"`
	// Layer is the published layer this record adds to the repository's
	// chain, or nil. A compaction carrying one is a PUBLICATION: its Retire
	// and Add, when present, are the repack of the non-full packs the new
	// layer gathers, and every Add is one of the layer's packs — or both are
	// empty, a record that publishes a layer over the packs as they stand.
	//
	// BASE FORMAT, AS A STATED CONDITION: no build that carries the code
	// domain has run without this field — none of the domain is on main, so
	// no image and no release holds it — and while that is so, every build
	// that meets a publication reads its layer. Once a build without it has
	// run, a new field is a versionedFields row, and this one takes {Name:
	// "Compaction.Layer", Since: <the version after the table's highest>,
	// Op: string(OpCompact), Path: []string{"mutation", "layer"}}, with
	// RecordVersion moved to it and its line under "What each version
	// added", a Carrying record — a compaction carrying a Layer — in the
	// statelogtest candidate, and a publication and a plain compaction in
	// the stamp test. omitempty, so only a publication carries the key: an
	// older build would apply every plain compaction and RETAIN every
	// publication rather than move its packs without its layer.
	Layer *LayerRef `json:"layer,omitempty"`
}

// LayerRef is ONE published layer of a repository's multi-pack-index chain:
// git's own incremental multi-pack-index layer, its bitmap, and the
// commit-graph layer over the same packs, written ONCE by the repository's
// maintainer and installed by every git node (ADR-0051's Derived standing).
// Its bytes are the three files back to back — the multi-pack-index, its
// bitmap, the commit-graph layer — cut into Parts, each one stored object.
//
// A LAYER IS CLOSED TOGETHER WITH ITS BASE: it covers every pack live at its
// plan's position (its compaction's Seen) that no layer below it covers,
// because git writes a bitmap only over a chain whose packs hold everything
// their commits reach — measured on git 2.56.0: a layer over all of a
// history's packs but one is refused ("Packfile doesn't have full closure")
// and writes no bitmap, and so is a layer over a cut's full packs alone. A
// pack that lands after the plan stays outside the chain until the next
// publication, which git reads beside it. The decide holds both halves
// (compaction.go): every pack the layer names live, and every pack live at
// the plan that its base does not name in it.
type LayerRef struct {
	// LayerID is the multi-pack-index layer's own trailing checksum, 40
	// lowercase hex digits: the name git gives the layer's files
	// (multi-pack-index-<id>.midx and .bitmap) and its line of the chain
	// file.
	LayerID string `json:"layer_id"`
	// BaseID is the layer directly below it in the chain the record leaves,
	// "" for the chain's root. A layer file does not name its base — the
	// chain's order is the only thing that does, which is why the rows'
	// order is the chain's authority.
	BaseID string `json:"base_id,omitempty"`
	// Replaces are the ids of the TOP layers this one replaces, in chain
	// order, lowest first: the publisher's geometric merge of the layers
	// above BaseID. Empty for a layer placed on the whole chain.
	Replaces []string `json:"replaces,omitempty"`
	// GraphID is the commit-graph layer's trailing checksum, 40 lowercase
	// hex digits — its file's name (graph-<id>.graph) and its line of
	// commit-graph-chain — or "" where the layer's packs hold no commit the
	// base's graph layers lack (GraphBytes and Commits are then 0).
	GraphID string `json:"graph_id,omitempty"`
	// Packs are the git checksums of every pack the layer covers, sorted and
	// distinct.
	Packs []string `json:"packs"`
	// Parts are the stored objects holding the layer's bytes, in byte order:
	// every part but the last exactly LayerPartBytes, the last at most that.
	Parts []LayerPart `json:"parts"`
	// MidxBytes, BitmapBytes and GraphBytes are the three sections, which
	// the parts cut: their sum is the parts' sizes'. There is no reverse
	// index section: a git of the node floor writes a multi-pack-index's
	// reverse index as its RIDX chunk, and a .rev file only under a test
	// variable.
	MidxBytes   int64 `json:"midx_bytes"`
	BitmapBytes int64 `json:"bitmap_bytes"`
	GraphBytes  int64 `json:"graph_bytes"`
	// Objects is how many objects the multi-pack-index layer holds (the last
	// entry of its OIDF fanout), and Commits how many commits the graph
	// layer holds: what a git node's per-object charges for the layer read.
	Objects int64 `json:"objects"`
	Commits int64 `json:"commits"`
	// Git is the release of git that wrote it, as git's own version line
	// spells its first three numbers ("2.56.0"): a git node whose git is
	// older installs none of it.
	Git string `json:"git"`
	// TookMs is how long its build took, in milliseconds — the repack, the
	// multi-pack-index and the graph together: the duty cycle the next
	// publication of the repository is paced by.
	TookMs int64 `json:"took_ms"`
}

// LayerPart is one stored object of a layer's bytes.
type LayerPart struct {
	Object objstore.Key  `json:"object"`
	Hash   objstore.Hash `json:"hash"`
	Size   int64         `json:"size"`
}

// Content is the object a read of the part is checked against.
func (p LayerPart) Content() objstore.Object {
	return objstore.Object{Key: p.Object, Hash: p.Hash, Size: p.Size}
}

// Size is the layer's bytes: its three sections, which its parts cut.
func (l LayerRef) Size() int64 { return l.MidxBytes + l.BitmapBytes + l.GraphBytes }

// Sections are the layer's three files' sizes, in the order its bytes hold
// them: the multi-pack-index, its bitmap, and the commit-graph layer (0 where
// it carries none) — the cut its parts are made by ([StoreLayer]) and laid
// back by ([FetchLayerPart]).
func (l LayerRef) Sections() [3]int64 { return [3]int64{l.MidxBytes, l.BitmapBytes, l.GraphBytes} }

// RetireSweep removes a repository's retired pack rows at or below Through,
// oldest first — the code-retention duty's sweep of what a record retired,
// once its grace has passed ([Store.RetireSweep]).
//
// PURE IN THE RECORD: the duty decides Through by its own clock against each
// row's retired_at — the broker's instant — and the apply removes the rows at
// or below it and reads no clock, so every node removes the same rows, and a
// suite can drive the grace with the duty's clock where the broker's cannot
// be moved. The skew between the two clocks is at most twice
// objstore.AssumedClockSkew while no node raises `clock_skew`, which
// [RetireGrace] outlasts past the longest read.
type RetireSweep struct {
	RepoID string `json:"repo_id"`
	// Through is the highest retired_position whose rows the duty's clock
	// put past RetireGrace: every row at or below it retired at or before
	// the decide's now less the grace, and it names whole records'
	// retirements, never part of one. The apply removes no row above it.
	Through statelog.Position `json:"through"`
}

// ExpungeRequest asks for an expunge of a repository's unreachable objects — a
// person's purge. Its record writes the request and nothing else: the
// repository's maintainer serves it, with no grace for the objects a ref drop
// left, through as many begins and commits as the repository's live packs
// need, and it is SERVED — the repository holds it no more — once no pack of
// the repository dates its content from before it ([PackRow.Content]): at its
// own apply, for a repository holding no pack, and at a commit's otherwise. A
// request asked while one is open WIDENS it: recorded, it moves the
// repository's open request to its own id and position, since it asks about
// everything the open one did and what landed since — so the repository's
// purge is served only once nothing from before the latest request is left.
//
// Every field is in the base format, under the condition [OpExpungeRequest]
// states.
type ExpungeRequest struct {
	RepoID string `json:"repo_id"`
	// RequestID is the record's operation id, minted once by whoever asks —
	// an operation id this engine accepts (statelog.CheckCallerOpID) — so a
	// request asked again after its answer was lost is the same request,
	// collapsed by the operation ledger.
	RequestID string `json:"request_id"`
	// By is the person who asked, by the credential they asked with and the
	// human seat it is bound to: the request's attribution, which its
	// record's actor — the deciding node, as for every expunge record — does
	// not say.
	By Principal `json:"by"`
}

// ExpungeBegin begins an expunge of a repository's packs: the packs it
// rewrites (Retire) leave code_packs for code_packs_retired, and the packs
// that replace them join it — Add, holding every object a ref reached when it
// was planned, LIVE; Dropping, holding the rest, DROPPING ([PackDropping]) —
// while the repository's drop epoch moves to DropEpoch and ExpungeID is its
// open expunge until its commit ([ExpungeCommit]). Decided by the
// repository's committer ([Committer.ProposeExpunge]) and applied as ONE
// record, which also removes the published layers over its inputs.
//
// IT KEEPS EVERY OBJECT: Add and Dropping hold every object of the inputs,
// and a cache serves a dropping pack beside the live ones, so no read loses
// an object to it and it judges no reachability any record needs a fence
// for. What it decides is which objects a new push may rely on: none in a
// dropping pack, which a receiver's view of the repository leaves out, and
// none at all of a push whose receiver read the repository at an earlier drop
// epoch, which its committer refuses `expunging`.
//
// Every field is in the base format, under the condition [OpExpungeBegin]
// states.
type ExpungeBegin struct {
	RepoID string `json:"repo_id"`
	// ExpungeID is minted ONCE by the node that builds the expunge — a bare
	// UUIDv7 (statelog.NewOpID with no name) — and is the record's
	// operation id, the repository's open expunge until its commit, and
	// what each output's pack id is derived from ([DeriveOutputPackID]):
	// a begin asked again of the repository's runner-up after a committer
	// went silent is the same begin, collapsed by the operation ledger.
	ExpungeID string `json:"expunge_id"`
	// RequestID is the open request this expunge serves, "" for one on the
	// maintainer's own schedule. PROVENANCE: a commit serves a request by
	// what each pack holds ([PackRow.Content]), never by this.
	RequestID string `json:"request_id,omitempty"`
	// Node is the git node that built it, for whoever reads the log.
	Node string `json:"node"`
	// Seen is the code log's position the builder's scan read the
	// repository's refs and ref drops at — the tips its outputs were cut by.
	// The committer decides at or past it (the publish's session floor), and
	// a purge's outputs date their content from it ([PackRow.Content]).
	Seen statelog.Position `json:"seen"`
	// DropEpoch is the repository's drop epoch once the begin applies: the
	// one its row held when the builder planned, plus one.
	DropEpoch int64 `json:"drop_epoch"`
	// Grace is how long before the scan a ref drop's object was still kept
	// reachable: PruneGrace for an expunge on the maintainer's own schedule,
	// and 0 for one serving a request — a purge drops what a force-push left
	// at once. The decide reads no ref; the APPLY dates the begin's outputs
	// by it ([PackRow.Content]): from Seen where it is 0, a purge having
	// judged everything it kept there, and from the oldest input otherwise,
	// since what a grace keeps is what a purge still asks to drop.
	Grace time.Duration `json:"grace"`
	// Retire are the pack ids the outputs replace, sorted and distinct, each
	// live when the committer decides — a full pack among them: an expunge is
	// the one rewrite that takes a full pack, since what it drops may be in
	// any pack.
	Retire []string `json:"retire"`
	// Add are the packs holding what a ref reached and Dropping the packs
	// holding the rest, each one bundle in the stored format at most
	// MaxPackBytes WHOLE ([PackRef.Check]); the n-th of Add then Dropping,
	// counted together from 0 in that order, has pack id
	// [DeriveOutputPackID] of ExpungeID and n. Either may be empty, never
	// both.
	Add      []PackRef `json:"add,omitempty"`
	Dropping []PackRef `json:"dropping,omitempty"`
	// DropLayers are the published layers the begin removes, in chain order:
	// the chain's top from the lowest layer naming an input — a layer over a
	// pack the begin retires is one no git node can load, and a layer above
	// it one whose base is gone — and none where no layer names an input.
	// The repository's maintainer publishes again once the expunge commits.
	DropLayers []string `json:"drop_layers,omitempty"`
}

// ExpungeCommit ends a repository's open expunge: its dropping packs (Retire)
// leave code_packs for code_packs_retired, where each keeps its object from
// the collector for [RetireGrace] — so a reader already on one finishes — and
// its RESCUE (Add) joins code_packs live: what of the dropping packs a ref
// reached at the rescue's sync, which a push that landed between the
// expunge's scan and its begin made reachable again, its content dated from
// the oldest dropping pack it was cut from ([PackRow.Content]). The
// repository's open expunge is cleared, and its open request with it once no
// pack of the repository dates its content from before the request.
//
// ITS ONE PRECONDITION IS ITS EXPUNGE: still open at its epoch, with exactly
// the dropping packs the commit retires. No ref and no push is read, so a
// repository pushed to all day still commits — a push checked after the begin
// relies on no dropping object, and one checked before it is refused
// `expunging`.
//
// Every field is in the base format, under the condition [OpExpungeCommit]
// states.
type ExpungeCommit struct {
	RepoID string `json:"repo_id"`
	// ExpungeID is the expunge this commit ends: the repository's open one.
	ExpungeID string `json:"expunge_id"`
	// CommitID is minted ONCE by the node that builds the commit — a bare
	// UUIDv7 — and is the record's operation id and what each rescue pack's
	// id is derived from ([DeriveOutputPackID]). Not ExpungeID, the begin's
	// own: the ledger would answer the commit with the begin's record. And
	// minted a build, never derived from the expunge, so a commit asked again
	// of the repository's runner-up is the same commit, collapsed by the
	// ledger, while one its builder built again over a newer sync is another
	// — answered stale if the first landed, rather than collapsed onto a
	// record that names other rescue packs.
	CommitID string `json:"commit_id"`
	// RequestID is the request the expunge serves, "" for none: provenance,
	// as on the begin.
	RequestID string `json:"request_id,omitempty"`
	// Node is the git node that built it, for whoever reads the log.
	Node string `json:"node"`
	// Seen is the code log's position the rescue's tips were read at, at or
	// past the begin: the committer decides at or past it, so one that has
	// not applied the begin yet refuses `behind`, about itself, and the
	// runner-up decides — rather than refuse the commit stale.
	Seen statelog.Position `json:"seen"`
	// DropEpoch is the epoch the expunge's begin moved the repository to.
	DropEpoch int64 `json:"drop_epoch"`
	// Retire are the repository's dropping packs — every one — sorted and
	// distinct: none where the begin left none.
	Retire []string `json:"retire,omitempty"`
	// Add is the rescue, each one bundle in the stored format at most
	// MaxPackBytes WHOLE, the n-th's pack id [DeriveOutputPackID] of CommitID
	// and n; none where nothing dropping was reached again.
	Add []PackRef `json:"add,omitempty"`
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
	case kind == KindName && op == OpRename:
		return decodePayload[Rename](rec)
	case kind == KindName && op == OpReleaseName:
		return decodePayload[ReleaseName](rec)
	case kind == KindRepo && op == OpHold:
		return decodePayload[Hold](rec)
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
	case kind == KindRepo && op == OpCompact:
		return decodePayload[Compaction](rec)
	case kind == KindRepo && op == OpRetireSweep:
		return decodePayload[RetireSweep](rec)
	case kind == KindRepo && op == OpExpungeRequest:
		return decodePayload[ExpungeRequest](rec)
	case kind == KindRepo && op == OpExpungeBegin:
		return decodePayload[ExpungeBegin](rec)
	case kind == KindRepo && op == OpExpungeCommit:
		return decodePayload[ExpungeCommit](rec)
	case kind == KindRepo && op == OpChangeOpen:
		return decodePayload[ChangeOpen](rec)
	case kind == KindRepo && op == OpChangeEdit:
		return decodePayload[ChangeEdit](rec)
	case kind == KindRepo && op == OpChangeDraft:
		return decodePayload[ChangeDraft](rec)
	case kind == KindRepo && op == OpChangeRetarget:
		return decodePayload[ChangeRetarget](rec)
	case kind == KindRepo && op == OpChangeClose:
		return decodePayload[ChangeClose](rec)
	case kind == KindRepo && op == OpChangeLanded:
		return decodePayload[ChangeLanded](rec)
	case kind == KindRepo && op == OpChangeReopen:
		return decodePayload[ChangeReopen](rec)
	case kind == KindChange && op == OpReviewers:
		return decodePayload[ReviewerRequest](rec)
	case kind == KindChange && op == OpComment:
		return decodePayload[Comment](rec)
	case kind == KindChange && op == OpThreadResolve:
		return decodePayload[ThreadResolve](rec)
	case kind == KindChange && op == OpReview:
		return decodePayload[Review](rec)
	case kind == KindChange && op == OpFindingResolve:
		return decodePayload[FindingResolve](rec)
	case kind == KindChange && op == OpRevise:
		return decodePayload[Revise](rec)
	case kind == KindChange && op == OpMergeability:
		return decodePayload[Mergeability](rec)
	case kind == KindChecks && op == OpCheckQueued:
		return decodePayload[CheckQueued](rec)
	case kind == KindChecks && op == OpCheckStarted:
		return decodePayload[CheckStarted](rec)
	case kind == KindChecks && op == OpCheckFinished:
		return decodePayload[CheckFinished](rec)
	case kind == KindChecks && op == OpChecksSweep:
		return decodePayload[ChecksSweep](rec)
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
