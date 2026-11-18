package gitlet

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/words"
)

// THE TYPED PAYLOADS, one per (kind, op), and the closed sets they spell.
//
// Every payload carries FULL POST-STATE or the complete list of what a record
// does (settings whole, a push's every command with its result), so a node
// replaying from zero rebuilds its rows from nothing but the rows a strict
// replay has already written.
//
// NO PAYLOAD CARRIES A VERSION FIELD: the RECORD's version gates decoding. The
// eviction and the generation keep the framework's shape, version and all.
//
// EVERY ENUM is a named string with typed constants, Valid, and a function
// answering the closed set in order: the one list a validator and a schema read.

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

	// ManagedTools is a repository a tool made (a person's or a lead's
	// project, or a seat's scratch repository), whose lifecycle the tools'
	// requests decide ([Store.Lifecycle]): the reconcile refuses to adopt one
	// ([ErrNotConfigManaged]), and the activation fence never refuses its
	// record ([Applier.fenced]).
	//
	// NO versionedFields ROW: a build knowing only ManagedConfig stores the
	// manager as written, fences only a configuration's record and refuses to
	// adopt the repository, so it reads nothing wrongly.
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
// scratch repository's `<handle>.<slug>`, so a rename keeps its kind's form
// for every actor ([lifecycleRefusal]).
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
// <handle>.<slug>, a project otherwise. [LifecycleRequest.Validate] holds a
// tool's create to it ([RepoSettings.Validate] does not know the name), and a
// declaration is a project by [Declared.settingsAt].
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
	// so a seat's work reaches it when a person pushes it. Read as "anyone
	// who may push" it would mean what a ref no rule matches already means.
	DirectPersons DirectPush = "persons"
	// DirectNone lets nobody push to it directly.
	DirectNone DirectPush = "none"
)

// DirectPushes is the closed set.
func DirectPushes() []DirectPush { return []DirectPush{DirectPersons, DirectNone} }

// Valid reports a value this build knows.
func (d DirectPush) Valid() bool { return slices.Contains(DirectPushes(), d) }

// Visibility is who may read a repository besides persons and Tier A tokens
// (ADR-0088): a SEAT'S standing narrows by it, a person's never does. A
// person's credential or a token bound to nobody reads every repository: a CI
// token that could not read a unit repository breaks its builds, and a Tier A
// token is the operator's own credential.
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

// Valid reports a value this build knows. EMPTY IS NOT ONE: a record carries
// settings whole, so an empty visibility would leave a reader guessing; the
// configuration applies its default (company) before forming the record.
func (v Visibility) Valid() bool { return slices.Contains(Visibilities(), v) }

// SearchMode is how code search reads a repository: from an index of its
// default branch's tip (indexed), or with git grep (grep), only by a search
// naming it. An index costs its holders disk and a build per tip; grep costs
// nothing until asked. Indexed is code.repos' default
// (config.CodeSearchMode.Resolved), grep a tool's. A SCRATCH REPOSITORY IS
// ALWAYS grep ([RepoSettings.Validate]): a seat makes up to
// code.scratch.max_per_seat, each deleted once idle, so indexing them would
// index every experiment.
//
// internal/gitlet/cache builds the index on the first [IndexHolders];
// [GitSearch] reads it or greps; internal/codesearch's scatter greps a grep
// repository only when a repo: filter names it.
type SearchMode string

const (
	// SearchIndexed is read from an index of its default branch's tip.
	SearchIndexed SearchMode = "indexed"
	// SearchGrep is read with git grep, only by a search that names it.
	SearchGrep SearchMode = "grep"
)

// SearchModes is the closed set: the one list a validator and the
// configuration's own (config.CodeSearchModes, held to it by the engine's
// TestEveryDeclaredSearchModeIsOneTheDomainRecords) are read against.
func SearchModes() []SearchMode { return []SearchMode{SearchIndexed, SearchGrep} }

// IndexHolders is how many Gitlet nodes keep a `search: indexed` repository's
// code index: the first IndexHolders of its rendezvous order over the live
// Gitlet nodes (estate.Router.GitOrder), so a search has a runner-up holding a
// built index when the first is gone or still building (M6-D12). Two, not
// one: a single holder's restart would send every search of its
// repositories to git grep until it rebuilt; not three, because every holder
// pays the index's bytes against its cache budget and its build's cat-file.
const IndexHolders = 2

// Valid reports a mode this build knows. EMPTY IS NOT ONE, for [Visibility]'s
// reason: whoever forms a record applies its default first — the
// configuration indexed, a tool grep.
func (m SearchMode) Valid() bool { return slices.Contains(SearchModes(), m) }

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
	// PrincipalSystem is the ENGINE ON ITS OWN ACCOUNT (no credential, seat
	// or run), acting only as the ref lifecycle's push deleting seats' idle
	// branches ([ViaExpiry], retention_expiry.go); [Proposal.Validate] pairs
	// the two both ways. [Principal.Validate] refuses it, so a read,
	// lifecycle request, change, conversation or merge naming it is refused
	// before any decide, and
	// [GrantOf] grants it only that deletion ([Grant.RefRule]). Spelled as
	// iam.ActorSystem, which its push row's actor_kind says.
	//
	// BASE FORMAT ([RecordVersion]): every build that meets a push record
	// reads it.
	PrincipalSystem PrincipalKind = "system"
)

// PrincipalKinds is the closed set: every kind a record's principal may name.
func PrincipalKinds() []PrincipalKind {
	return []PrincipalKind{PrincipalOperator, PrincipalAgent, PrincipalSystem}
}

// Valid reports a value this build knows.
func (p PrincipalKind) Valid() bool { return slices.Contains(PrincipalKinds(), p) }

// readsChart reports a principal whose push is judged from the CHART: a
// seat's alone. A person maintains every repository and a token bound to
// nobody reads every one (ADR-0085), and the engine's expiry has its own rule
// ([Grant.RefRule]); a seat's grant, protections, cross-unit share and daily
// quota all come from the chart. A node holding no chart, or one confirmed
// behind the configuration, decides no such push
// ([CommitterOptions.SeatRules], [CommitterOptions.Current]).
func (p PrincipalKind) readsChart() bool { return p == PrincipalAgent }

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
	// RepoStatusImporting is being filled from another host: an import, or a
	// pull mirror's first fetch, at its create ([CreateRepo.status]) or when a
	// settings record makes an active repository a mirror
	// ([repoRemotes.startsImport]). It takes no write until done: no push,
	// change, archive or expunge ([RefusalRepoImporting]). It is fetched,
	// listed, renamed, has its settings changed and is deleted as an active
	// one is; the compactor, the publisher and the expunge pass it by.
	//
	// BASE FORMAT ([RecordVersion]): every build that meets a record making
	// one reads it.
	RepoStatusImporting RepoStatus = "importing"
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

// RepoStatuses is the closed set, in the order a repository's life reaches
// them.
func RepoStatuses() []RepoStatus {
	return []RepoStatus{RepoStatusImporting, RepoStatusActive, RepoStatusArchived, RepoStatusDeleted,
		RepoStatusPurging}
}

// live reports a status a repository is in use in: importing, active or
// archived — listed, fetched and kept, where a deleted one waits for its
// purge and a purging one is going.
func (s RepoStatus) live() bool {
	return s == RepoStatusImporting || s == RepoStatusActive || s == RepoStatusArchived
}

// Valid reports a value this build knows.
func (s RepoStatus) Valid() bool { return slices.Contains(RepoStatuses(), s) }

// PackKind is what added a pack row to code_packs: which record wrote it.
//
// READ BY NOTHING THAT DECIDES: a cache installs by [PackState] and a decide
// judges state, part and size, so a newer build's kind is installed and judged
// as any other. It records what rewrote the packs, for whoever reads the rows.
// Every value is in the base format, as [OpCompact] and [OpExpungeBegin] are.
type PackKind string

const (
	// PackPush is a push's pack: the bundle its receive-pack stored.
	PackPush PackKind = "push"
	// PackCompact is a compaction's output.
	PackCompact PackKind = "compact"
	// PackMerge is the pack of objects a change's merge built — its merge or
	// squashed commit and the trees and blobs the merge wrote — which its
	// merge record adds ([Merge]).
	PackMerge PackKind = "merge"
	// PackCandidate is the pack a queue's train is recorded with —
	// the commits its candidates built of their entries, and the trees and
	// blobs their merges wrote — which its candidate record adds
	// ([Candidate]).
	PackCandidate PackKind = "candidate"
	// PackExpunge is a pack an expunge's begin keeps: what of its inputs a
	// ref reached, live.
	PackExpunge PackKind = "expunge"
	// PackCruft is a pack an expunge's begin leaves dropping: what of its
	// inputs no ref reached.
	PackCruft PackKind = "cruft"
	// PackRescue is a pack an expunge's commit keeps of its dropping packs:
	// what a push that landed before its begin made reachable again, live.
	PackRescue PackKind = "rescue"
	// PackImport is a pack an import's chunk lands ([ImportChunk]): objects
	// the repository's home fetched from the host it is imported from.
	PackImport PackKind = "import"
	// PackMirror is a pack a pull mirror's sync lands ([MirrorSync]):
	// objects its home fetched from the repository it follows.
	PackMirror PackKind = "mirror"
)

// PackKinds is the closed set, in the order their records were added: one
// list of every kind code_packs may hold.
func PackKinds() []PackKind {
	return []PackKind{PackPush, PackCompact, PackMerge, PackCandidate, PackExpunge, PackCruft, PackRescue,
		PackImport, PackMirror}
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

// PathOwner is one owner of a repository's paths, a unit by key or a seat by
// handle, and the globs it owns (gitref.ParsePathGlob), sorted and each once.
// It is for REVIEW: a change touching its paths asks it for one. It GRANTS
// NOTHING (standing is [GrantOf]'s), so it is never on a [RepoRef], which
// every read, push and fetch decides from.
type PathOwner struct {
	Owner Owner    `json:"owner"`
	Paths []string `json:"paths"`
}

// equal compares two path owners, their globs as slices.
func (p PathOwner) equal(o PathOwner) bool {
	return p.Owner == o.Owner && slices.Equal(p.Paths, o.Paths)
}

// comparePathOwners is the canonical order of a repository's path owners: by
// the owner's kind, then its reference, the one order every node forms a
// record in and [RepoSettings.Validate] holds a record to.
func comparePathOwners(a, b PathOwner) int {
	if c := strings.Compare(string(a.Owner.Kind), string(b.Owner.Kind)); c != 0 {
		return c
	}
	return strings.Compare(a.Owner.Ref, b.Owner.Ref)
}

// CanonicalPathOwners is owners in the ONE FORM a record carries them: sorted
// by owner ([comparePathOwners]), each owner's globs sorted, as a copy. THE
// ONE PLACE the order is formed (the reconcile and a lifecycle decide read
// it), so every node forms byte-identical records. It removes no duplicate:
// [RepoSettings.Validate] refuses one.
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

// pathOwnersInvalid is why owners are not in the form a record carries, or "":
// at most [MaxPathOwners], each with 1 to [MaxPathOwnerPaths] globs, all sorted
// and once ([comparePathOwners]), and the whole within [MaxPathOwnersBytes],
// judged last so a malformed owner is refused for what it says.
func pathOwnersInvalid(owners []PathOwner) string {
	if len(owners) > MaxPathOwners {
		return fmt.Sprintf("path_owners: %d owners, past the cap of %d", len(owners), MaxPathOwners)
	}
	for i, po := range owners {
		at := fmt.Sprintf("path_owners[%d]", i)
		switch {
		case !po.Owner.Kind.Valid():
			return fmt.Sprintf("%s.owner.kind: %q is not one of %s", at, po.Owner.Kind, words.Or(OwnerKinds()))
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

// PushRule is a protection rule as a PUSH is judged by it: its pattern and
// what it lets a push do, never the merge gate. It is what every read of a
// repository's row answers ([RepoRef.Protections]), none of which reads a
// gate. Typed apart from [Protection] so no consumer takes a gate nobody read
// for a rule that has none.
//
// Comparable with ==: it holds no pointer.
type PushRule struct {
	// Pattern is refs/heads/<glob> or refs/tags/<glob> (gitref's grammar).
	Pattern     string     `json:"pattern"`
	AllowForce  bool       `json:"allow_force,omitempty"`
	AllowDelete bool       `json:"allow_delete,omitempty"`
	DirectPush  DirectPush `json:"direct_push"`
}

// Protection is one rule over the refs a pattern matches WHOLE: its [PushRule]
// (embedded, so a record spells a rule as one object) and, on a branch rule,
// what a merge into one needs. Settings carry it, and so do the reads that
// present a gate or rewrite settings ([ReadQuery.Gates], the lifecycle
// decides, the reconcile).
//
// COMPARED BY [Protection.Equal], never ==: its gate is a pointer.
type Protection struct {
	PushRule
	// Gate is the rule's merge gate ([MergeGate]), nil for none and always
	// nil on a tag rule, which no merge reaches. A push is held to the
	// [PushRule] alone.
	Gate *MergeGate `json:"gate,omitempty"`
}

// pushRules is the push half of each of rules, in their order — the rules a
// [RepoRef] answers — and nil for none.
func pushRules(rules []Protection) []PushRule {
	if rules == nil {
		return nil
	}
	out := make([]PushRule, len(rules))
	for i, p := range rules {
		out[i] = p.PushRule
	}
	return out
}

// invalid is why p is no rule a record may carry, or "", naming the field. THE
// ONE RULE for a rule alone, read by [RepoSettings.Validate] and
// [LifecycleRequest.Validate]; its place among the others is the list's to
// judge.
func (p Protection) invalid() string {
	switch {
	case !gitref.ValidPattern(p.Pattern):
		return fmt.Sprintf("pattern: %q is not a protection pattern", p.Pattern)
	case !p.DirectPush.Valid():
		return fmt.Sprintf("direct_push: %q is not one of %s", p.DirectPush, words.Or(DirectPushes()))
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
	// Kind is what the repository is for, as its name says ([KindOf]); a
	// declaration's is always a project ([Declared.settingsAt]), and no
	// record changes it.
	//
	// BASE FORMAT ([RecordVersion]): every build that meets settings reads it.
	Kind  RepoKind `json:"kind"`
	Owner Owner    `json:"owner"`
	// DefaultBranch is a branch name, not a ref ("main").
	DefaultBranch string `json:"default_branch"`
	Description   string `json:"description,omitempty"`
	Archived      bool   `json:"archived,omitempty"`
	// Visibility is who may read it besides persons and Tier A tokens.
	Visibility Visibility `json:"visibility"`
	// Search is how code search reads it ([SearchMode]): never indexed on a
	// scratch repository. BASE FORMAT, as Kind is.
	Search SearchMode `json:"search"`
	// Protections are sorted by Pattern and unique — the canonical order,
	// so two nodes deciding one declaration form byte-identical records —
	// each one's gate in its one form too ([CanonicalGate]).
	Protections []Protection `json:"protections,omitempty"`
	// PathOwners are who owns which of its paths, in [CanonicalPathOwners]'
	// form; none on a scratch repository, which is its seat's own. BASE
	// FORMAT, as Kind is.
	PathOwners []PathOwner `json:"path_owners,omitempty"`
	// MirrorFrom makes it a PULL MIRROR of a repository at another host, and
	// MirrorTo a PUSH MIRROR to one; nil for none, and neither on a scratch
	// repository. Only a person or the configuration sets either (mirror.go,
	// whose head states the record format's condition for both).
	MirrorFrom *MirrorSource `json:"mirror_from,omitempty"`
	MirrorTo   *MirrorTarget `json:"mirror_to,omitempty"`
	// ConfigEpoch is the activation these settings came from
	// (configplane.ActivationStamp), which the activation fence and the
	// reconcile's lifecycle rules compare.
	ConfigEpoch int64 `json:"config_epoch"`
}

// Validate refuses settings this domain will not record: the domain's own
// guard beside the configuration validator's same rules, since the applier
// salvages rather than refuses.
func (s RepoSettings) Validate() error {
	switch {
	case !s.ManagedBy.Valid():
		return fmt.Errorf("%w: managed_by: %q is not one of %s", ErrInvalid, s.ManagedBy, words.Or(ManagedBys()))
	case !s.Kind.Valid():
		return fmt.Errorf("%w: kind: %q is not one of %s", ErrInvalid, s.Kind, words.Or(RepoKinds()))
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
		return fmt.Errorf("%w: owner.kind: %q is not one of %s", ErrInvalid, s.Owner.Kind, words.Or(OwnerKinds()))
	case strings.TrimSpace(s.Owner.Ref) == "":
		return fmt.Errorf("%w: owner.ref: a repository's owner names a unit or a seat", ErrInvalid)
	case !s.Visibility.Valid():
		return fmt.Errorf("%w: visibility: %q is not one of %s", ErrInvalid, s.Visibility, words.Or(Visibilities()))
	case !s.Search.Valid():
		return fmt.Errorf("%w: search: %q is not one of %s", ErrInvalid, s.Search, words.Or(SearchModes()))
	case s.Kind == RepoScratch && s.Search == SearchIndexed:
		return fmt.Errorf("%w: search: a scratch repository is searched with %s — it is its seat's own "+
			"experiment, and no index is kept for one", ErrInvalid, SearchGrep)
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
	// A SCRATCH REPOSITORY TAKES NO MIRROR, which would dial or publish to
	// another host for a seat's own experiment. An ARCHIVED one is no pull
	// mirror: it takes no sync, so its source would never move it.
	switch {
	case s.Kind == RepoScratch && (s.MirrorFrom != nil || s.MirrorTo != nil):
		return fmt.Errorf("%w: mirror: a scratch repository takes no mirror — it is its seat's own", ErrInvalid)
	case s.MirrorFrom != nil && s.Archived:
		return fmt.Errorf("%w: mirror_from: an archived repository takes no sync, so a pull mirror is "+
			"never archived — remove mirror_from to keep it as it stands", ErrInvalid)
	}
	if s.MirrorFrom != nil {
		if why := s.MirrorFrom.invalid(); why != "" {
			return fmt.Errorf("%w: mirror_from.%s", ErrInvalid, why)
		}
	}
	if s.MirrorTo != nil {
		if why := s.MirrorTo.invalid(); why != "" {
			return fmt.Errorf("%w: mirror_to.%s", ErrInvalid, why)
		}
	}
	return nil
}

// equal compares two settings by value, Protections and PathOwners in order
// (Validate holds them sorted), a rule's gate through [Protection.Equal], and
// IGNORES ConfigEpoch: the activation a value came from is no difference in
// the value.
func (s RepoSettings) equal(o RepoSettings) bool {
	return s.ManagedBy == o.ManagedBy && s.Kind == o.Kind && s.Owner == o.Owner &&
		s.DefaultBranch == o.DefaultBranch && s.Description == o.Description &&
		s.Archived == o.Archived && s.Visibility == o.Visibility && s.Search == o.Search &&
		slices.EqualFunc(s.Protections, o.Protections, Protection.Equal) &&
		slices.EqualFunc(s.PathOwners, o.PathOwners, PathOwner.equal) &&
		equalPointed(s.MirrorFrom, o.MirrorFrom, func(a, b MirrorSource) bool { return a == b }) &&
		equalPointed(s.MirrorTo, o.MirrorTo, MirrorTarget.equal)
}

// equalPointed compares two optional values: both absent, or both present and
// equal by eq.
func equalPointed[T any](a, b *T, eq func(T, T) bool) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return eq(*a, *b)
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
	// RepoID is chosen by the WRITER, never derived by the applier, so a
	// create that lost its race wrote nothing under the winner's id. A
	// configuration mints it per call; a tool derives it from the request's
	// operation id ([Store.Lifecycle]), so a retry whose ledger row was swept
	// names the same repository and two requests never share one.
	RepoID   string       `json:"repo_id"`
	Name     string       `json:"name"`
	Settings RepoSettings `json:"settings"`
	// Import fills the repository from another host at its create
	// ([ImportSource]), nil for none; read by the create alone, and never
	// beside a pull mirror, which imports from its own source. Only a person
	// or the configuration names one (mirror.go's head has the record
	// format's condition).
	Import *ImportSource `json:"import,omitempty"`
}

// importInvalid is why the create's import is none a record carries, or "".
func (p CreateRepo) importInvalid() string {
	switch i := p.Import; {
	case i == nil:
		return ""
	case p.Settings.Kind == RepoScratch:
		return "import: a scratch repository takes no import — it is its seat's own"
	case p.Settings.MirrorFrom != nil:
		return "import: a pull mirror is imported from its mirror_from when it is created, so an import " +
			"beside it is a second source"
	case p.Settings.Archived:
		return "import: an import fills an active repository, and an archived one takes nothing — archive " +
			"it once its import is done"
	default:
		if why := i.invalid(); why != "" {
			return "import." + why
		}
	}
	return ""
}

// status is the status the create makes its repository: IMPORTING where it
// imports — its own import, or a pull mirror's first fetch from its source —
// and otherwise what its settings say.
func (p CreateRepo) status() RepoStatus {
	if p.Import != nil || p.Settings.MirrorFrom != nil {
		return RepoStatusImporting
	}
	return p.Settings.status()
}

// importSource is what the create's import fetches, as code_repos.import_source
// records it: its import, or its pull mirror's source FIXED AT THE CREATE (a
// mirror_from moved meanwhile is the next sync's to follow), nil for none.
func (p CreateRepo) importSource() *ImportSource {
	switch {
	case p.Import != nil:
		i := *p.Import
		return &i
	case p.Settings.MirrorFrom != nil:
		return &ImportSource{URL: p.Settings.MirrorFrom.URL, Credential: p.Settings.MirrorFrom.Credential}
	}
	return nil
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
// BASE FORMAT ([RecordVersion]), and no gate.
type Rename struct {
	RepoID string `json:"repo_id"`
	From   string `json:"from"`
	To     string `json:"to"`
}

// ReleaseName frees a renamed repository's old name once its redirect has
// ended, on that name's subject — the code-retention pass's
// ([Store.ReleaseName]).
//
// BASE FORMAT, as [Rename] is. It is no gate.
type ReleaseName struct {
	Name   string `json:"name"`
	RepoID string `json:"repo_id"`
}

// RedirectWindow is how long a renamed repository's old name keeps
// redirecting: a MONTH, a product judgment, so a clone's remote or a CI
// configuration naming the old URL keeps working while its owner updates it.
// The release lands within one code-retention pass ([RetentionInterval]);
// until then the name stays the repository's, so every node at one position
// answers alike, which comparing each node's clock would not.
const RedirectWindow = 30 * 24 * time.Hour

// Hold is a LIVE TURN'S HOLD on a scratch repository, or on one seat's branch
// (Ref), recorded on `repo.<id>` so the broker orders it against a deletion
// listed before the turn's read: whichever lands second decides from the
// first ([Store.ExpireScratch] refuses a repository with a record past its
// listed page; a hold after the deletion answers repo_deleted, or
// ref_deleted). Its apply moves only the repository's version and a ref
// hold's held_position: never an idle clock ([Retention]), nor a ref's `at`,
// `position`, seat or target ([RefHoldKey]'s file).
//
// BASE FORMAT, as [Rename] is. It is no gate.
type Hold struct {
	RepoID string `json:"repo_id"`
	// TurnID is the turn holding it (turnctx.Turn.RunID), at most
	// MaxProvenanceBytes, so a reader of the log can say whose hold ordered an
	// expiry.
	TurnID string `json:"turn_id"`
	// Ref is the seat's branch the turn holds — a ref [Holdable] admits — or
	// "" for a hold on the scratch repository itself. BASE FORMAT, as the op
	// is.
	Ref string `json:"ref,omitempty"`
}

// HoldRecordAfter is how long a scratch repository must have been idle before
// a turn's hold on it is also RECORDED ([Hold]): idle_days' floor of one day
// (config.CodeScratchIdleDaysFloor) less two hours. A pass fixes its cutoff
// BEFORE reading live holds, so only a hold taken after it is unseen; cutoff
// and threshold each compare one node's clock with the same broker-stamped
// last_activity_at, so a repository idle less than this is past no in-flight
// pass's cutoff, under any epoch's idle_days, while two nodes disagree by under
// two hours (twice objstore.AssumedClockSkew; `clock_skew` watches it); for an
// older one the broker decides the race
// (references.TestAHoldRecordCoversEveryPassInFlight). It costs a record per
// TURN, as a hold moves no clock: small (TestAHoldRecordCostsTheLogOneSmallRecord),
// weighed by internal/worklet's codeLogNegligible.
const HoldRecordAfter = 22 * time.Hour

// PushBatch is one or more pushes to one repository, in the order they were
// decided — group commit's one record.
type PushBatch struct {
	RepoID string      `json:"repo_id"`
	Pushes []PushEntry `json:"pushes"`
}

// PushEntry is one push: in a record iff at least one of its commands is ok,
// or it is an ATTEMPT ([Attempted]): every command refused, one by a rule,
// recorded moving nothing, with no pack, introduced commit, change or routing.
type PushEntry struct {
	// PushID is minted by the receiving node and stable across a failover,
	// which is what collapses a push decided twice.
	PushID    string    `json:"push_id"`
	Principal Principal `json:"principal"`
	// Via is how the push's pack was built — received by git, or built by
	// the engine itself — as its proposer stamped it ([Proposal.Via]), and
	// what its push row's via column says.
	//
	// BASE FORMAT ([RecordVersion]): every build that meets a push record
	// reads it.
	Via PushVia `json:"via"`
	// Node is the node that received the push.
	Node   string `json:"node"`
	Atomic bool   `json:"atomic,omitempty"`
	// Commands is every command the client sent, each with its result.
	Commands []Command `json:"commands"`
	// Pack is present iff the push carried new objects.
	Pack            *PackRef `json:"pack,omitempty"`
	Introduced      []string `json:"introduced,omitempty"`
	IntroducedCount int      `json:"introduced_count,omitempty"`
	// Offered is how many commits an attempt's refused commands offered that
	// the repository did not hold, counted by its receiver in the push's
	// scratch; 0 where it offered none or the count was not made. VERSION 5
	// ([OfferedVersion]): an entry carrying it stamps its record there.
	Offered    int       `json:"offered,omitempty"`
	ReceivedAt time.Time `json:"received_at"`
	// Day is the company day a SEAT'S entry was counted against by its
	// decide's quota (code.limits.pushes_per_seat_day): period.Day's label
	// on the deciding node's clock in the company's zone, STAMPED BY THE
	// COMMITTER on every agent entry and never sent by a proposer, so the
	// apply counts it (code_seat_days) with no clock or zone of its own,
	// identically on every node. Empty on every other entry. BASE FORMAT, as
	// Via is.
	Day string `json:"day,omitempty"`
	// WorkingFor is whom a SEAT'S push was made for (ADR-0109): the carrier
	// its proposal states ([Proposal.WrittenFor]), STAMPED BY THE COMMITTER
	// on an agent's entry as Day is, never sent by a proposer; nil for a
	// seat's own work and on every entry no seat pushed. The apply keeps it on
	// code_pushes.working_for and on each revision the entry adds
	// (code_revisions.working_for), which later records read back
	// ([revisionPusher], [landingPusher]); routings carry it too
	// ([ChangeRouting.WorkingFor]).
	//
	// VERSION 2 ([versionedFields]): an entry carrying it stamps its record
	// there.
	WorkingFor *types.WorkingFor `json:"working_for,omitempty"`
	// Change is the change this push opens from its options
	// ([ChangeOption], pushchange.go), STAMPED BY ITS DECIDE only where it
	// opens one, its target resolved to the default branch where unnamed, so
	// the apply opens it on the entry's one branch command from the record
	// alone (revisions.go). A push opening no change carries none: nothing of
	// a change a push did not open is state. BASE FORMAT, as Via is.
	Change *ChangeOption `json:"change,omitempty"`
	// Routing is whom the entry concerns ([ChangeRouting]): one per change
	// its commands MERGE, in number order, then the change it OPENS, each
	// naming the pusher as actor. STAMPED BY ITS DECIDE; none where it merges
	// and opens nothing (concerns.go). Bounded with the record's bytes
	// ([routedEntry]). BASE FORMAT, as concerns.go's head says.
	Routing []ChangeRouting `json:"routing,omitempty"`
	// Landings is every LANDING on a protected branch: each ok command
	// moving a branch a protection rule matches (a person's direct_push
	// persons push or recorded override, ADR-0096; no seat's push moves one),
	// in command order. STAMPED BY ITS DECIDE from its snapshot's rules, so
	// the apply writes the landing row, and the green row where the gate
	// requires no check, without matching a rule itself.
	//
	// BASE FORMAT, as queuerecords.go's head says.
	Landings []PushLanding `json:"landings,omitempty"`
}

// PushLanding is one landing a push entry makes on a protected branch: the
// place of its command among the entry's, and whether it is green as it lands —
// the branch's gate requiring no check, so the branch's green row moves to it.
type PushLanding struct {
	Command int  `json:"command"`
	Green   bool `json:"green,omitempty"`
}

// Principal is who acts on a repository: an operator credential and the
// person it is bound to ("" for nobody), or a seat — and on a seat's push, the
// run behind it.
type Principal struct {
	Kind       PrincipalKind `json:"kind"`
	OperatorID string        `json:"operator_id,omitempty"`
	Seat       string        `json:"seat,omitempty"`
	// Run is what made a SEAT'S PUSH ([RunProvenance]), and nothing on any
	// other principal: an operator has no run, and a seat that only READS
	// names none, since a read's asker is judged by who it is. THE ZERO VALUE
	// IS "NO RUN", a value rather than a pointer, so the absence has one
	// spelling and a principal stays comparable with ==.
	//
	// BASE FORMAT ([RecordVersion]): every build that meets a push record
	// reads it.
	Run RunProvenance `json:"run,omitzero"`
}

// Validate refuses a principal no record or question may carry ([ErrInvalid],
// naming the field): an operator names its credential, its bound person's
// handle or nobody, and no run; a seat names its handle, no credential, and a
// run only as [RunProvenance.Validate] admits. An unknown kind is refused, and
// so is [PrincipalSystem]: only its expiry's push carries it, which
// [Proposal.Validate] judges ([Principal.systemInvalid]).
func (p Principal) Validate() error {
	if why := p.invalid(); why != "" {
		return fmt.Errorf("%w: %s", ErrInvalid, why)
	}
	return nil
}

// systemInvalid is why p is not the engine's own account as an expiry's push
// carries it, or "": the kind and NOTHING beside it, since the engine acts for
// nobody when it deletes an idle branch.
func (p Principal) systemInvalid() string {
	switch {
	case p.Kind != PrincipalSystem:
		return fmt.Sprintf("principal.kind: %q — the ref lifecycle's expiry is the engine's own push, kind %q",
			p.Kind, PrincipalSystem)
	case p.OperatorID != "":
		return "principal.operator_id: the engine acts on its own account, with no credential"
	case p.Seat != "":
		return "principal.seat: the engine acts on its own account, for no seat"
	case !p.Run.IsZero():
		return "principal.run: the engine's own push is no turn's and no executor's"
	}
	return ""
}

// invalid is why p is no principal, or "": the one rule [Principal.Validate]
// and a proposal's validation share. The engine's own account is none here.
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
	case PrincipalSystem:
		return fmt.Sprintf("principal.kind: %q is the engine's own account, which only the ref lifecycle's "+
			"expiry push carries — nobody asks, reads or writes as it", p.Kind)
	default:
		return fmt.Sprintf("principal.kind: %q is not one of %s", p.Kind, words.Or(PrincipalKinds()))
	}
	return ""
}

// Actor is who a record names as p's actor: an operator's credential (the
// person's attribution, iam.ActorFor), a seat's own handle, and nobody for the
// engine's expiry. THE ONE RULE for every record of a principal: a push row's
// actor, and the reader a `visibility: unit` repository's read is recorded as,
// which the usage domain refuses empty.
func (p Principal) Actor() string {
	if p.Kind == PrincipalAgent {
		return p.Seat
	}
	return p.OperatorID
}

// WriterKind is p's writer kind in the identity directory's words, as
// iam.ActorFor classes it: a seat is an agent, a person's bound credential
// human, a credential bound to nobody an operator, and the engine and a home's
// zero principal the system. [types.WrittenFor.For] is held to it, so
// [writtenForInvalid] and the estate's pre-check read one answer.
func (p Principal) WriterKind() iam.ActorKind {
	switch {
	case p.Kind == PrincipalAgent:
		return iam.ActorAgent
	case p.Kind == PrincipalOperator && p.Seat != "":
		return iam.ActorHuman
	case p.Kind == PrincipalOperator:
		return iam.ActorOperator
	}
	return iam.ActorSystem
}

// RunProvenance is what made a seat's push as the SERVER stamps it from the
// call (turn, work item, model, and for a coding box's push its run and
// harness), never as a commit says it: a commit's headers and trailers are its
// author's, and the engine trusts nothing it did not see. Every field is
// bounded ([MaxProvenanceBytes]) and spelled as the push row stores it.
type RunProvenance struct {
	// TurnID is the turn that made the write (turnctx.Turn.RunID), for a
	// box's push the turn that launched its run: the one field every run
	// names.
	TurnID string `json:"turn_id"`
	// WorkBackend and WorkKey are the work item the turn is charged to
	// (ADR-0022) — its tracker and the key that tracker shows — empty for a
	// turn charged to none.
	WorkBackend string `json:"work_backend,omitempty"`
	WorkKey     string `json:"work_key,omitempty"`
	// WorkID is that work item's IDENTITY in its tracker
	// (types.WorkItem.ID), since a key is a label the tracker may reuse:
	// stamped on a coding box's push from its run's row at admission, empty
	// on every other write, so a box's `git push -o change` opens a change
	// ([PushEntry.Change]) carrying its run's item, as a code_open_change of
	// the same turn does. Set, it names a WorkBackend. A RECORD'S FIELD AND
	// NO ROW'S: code_pushes keeps tracker and key, so [PushRow.Run] carries
	// none; the opened change keeps the identity on its own row.
	//
	// BASE FORMAT, as RunID is.
	WorkID string `json:"work_id,omitempty"`
	// WorkProject is that work item's PROJECT (types.WorkItem.Project),
	// carried exactly as WorkID is and kept by the change the push opens
	// (code_changes.work_project), so the change carries its item in the one
	// shape every open gives it and every wake built from it names the
	// project (MetaWorkProject). BASE FORMAT, as RunID is.
	WorkProject string `json:"work_project,omitempty"`
	// Model is the executor model of the round that called the tool, empty
	// where the call named none.
	Model string `json:"model,omitempty"`
	// RunID is the LAUNCH behind a box's push (sandbox.PendingRun.LaunchID,
	// a canonical UUID) and Harness the harness the box ran ("claude-code",
	// "opencode"), both stamped from the run's row at the box's admission:
	// the `run_pushes` read finds a run's pushes by them. BOTH OR NEITHER, and
	// only on a push git received from a box ([Proposal.Validate]): an
	// engine-built write's run is the calling turn, and a person has none.
	//
	// BASE FORMAT ([RecordVersion]): every build that meets a push record
	// reads them.
	RunID   string `json:"run_id,omitempty"`
	Harness string `json:"harness,omitempty"`
}

// IsZero reports a principal's run that names nothing: no run at all, the
// spelling every principal but a seat's push carries.
func (r RunProvenance) IsZero() bool { return r == RunProvenance{} }

// MaxProvenanceBytes bounds each [RunProvenance] field: ids are UUIDs (36
// bytes) or `<repository id>#<number>`, a Jira key is under 32, the longest
// Bedrock model ARN about 120, a harness a short name, and a GitLab group path
// as a project is fitted where a nested one runs longer. 256 is past each but
// that path's deepest, and buys an entry whose size is known before it is
// measured: eight fields at it are 12 KiB of a record's MiB even where JSON
// spells every byte in six.
const MaxProvenanceBytes = 256

// Validate refuses a run no push may carry ([ErrInvalid], naming the field).
// A stamping site passes every field copied from outside the code domain
// through [FitProvenance] first, so a refusal here is the engine's own bug,
// never a seat's.
func (r RunProvenance) Validate() error {
	if why := r.invalid(); why != "" {
		return fmt.Errorf("%w: run.%s", ErrInvalid, why)
	}
	return nil
}

// invalid is why r is no run, or "", naming the field as its JSON does. A RUN
// ID IS THE LAUNCH'S CANONICAL UUID, never a fitted label: `run_pushes` finds
// a run's pushes by it whole, so any other spelling lands a push no read of
// its run names.
func (r RunProvenance) invalid() string {
	switch {
	case r.TurnID == "":
		return "turn_id: a seat's push names the turn that made it"
	case r.RunID != "" && !canonicalUUID(r.RunID):
		return fmt.Sprintf("run_id: %q is not a UUID in its canonical lowercase spelling — an executor's "+
			"launch is named by the launch id the engine minted", r.RunID)
	case r.RunID != "" && r.Harness == "":
		return "harness: an executor's push names the harness its box ran"
	case r.RunID == "" && r.Harness != "":
		return "run_id: a push naming its harness names the launch it pushed for"
	case r.WorkID != "" && r.WorkBackend == "":
		return "work_backend: a work item's id is its identity in one tracker, and names that tracker"
	case r.WorkProject != "" && r.WorkBackend == "":
		return "work_backend: a work item's project is a project in one tracker, and names that tracker"
	}
	for _, f := range []struct{ name, value string }{
		{"turn_id", r.TurnID}, {"work_backend", r.WorkBackend}, {"work_id", r.WorkID}, {"work_key", r.WorkKey},
		{"work_project", r.WorkProject}, {"model", r.Model}, {"harness", r.Harness},
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
	// Detail is an ng command's one-sentence specifics ("refs/heads/main is
	// at 1a2b3c4; fetch first"), in proposals and answers and NEVER in a
	// record or a row: a sentence is an answer, not state.
	Detail string `json:"detail,omitempty"`
}

// PackRef names ONE push's stored object, its PACK BUNDLE (bundle.go): the
// pack receive-pack stored, then its .idx, then its .rev, back to back — so a
// Gitlet node installs a push with one GET and three renames and runs no
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
// A MERGE MAKES NO REACHABILITY JUDGEMENT (ADR-0090): its outputs hold every
// object its inputs held, so no push decided against them loses a ref's
// closure and no fence is needed beyond the decide's check that each input is
// live. An object an input shares with a non-input pack is written again, as
// two compactions from different snapshots may each retire the pack the other
// relied on. Only an expunge needs a fence ([ExpungeBegin]). Every field is in
// the base format, as [OpCompact] is.
type Compaction struct {
	RepoID string `json:"repo_id"`
	// CompactionID is minted ONCE by the building node, a bare UUIDv7
	// (statelog.NewOpID with no name), and is the record's operation id: a
	// proposal re-asked of the runner-up after a committer went silent
	// collapses in the operation ledger, which judges a late retry by its
	// instant; each output's id derives from it ([DeriveOutputPackID]).
	CompactionID string `json:"compaction_id"`
	// Node is the Gitlet node that built it, for whoever reads the log.
	Node string `json:"node"`
	// Seen is the code log's position the builder planned at; the committer
	// decides at or past it (the publish's session floor). ONE FIELD: the
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
	// BASE FORMAT ([RecordVersion]): every build that meets a publication
	// reads its layer.
	Layer *LayerRef `json:"layer,omitempty"`
}

// LayerRef is ONE published layer of a repository's multi-pack-index chain:
// git's incremental multi-pack-index layer, its bitmap, and the commit-graph
// layer over the same packs, written ONCE by the maintainer and installed by
// every Gitlet node (ADR-0081's Derived standing): the three files back to
// back, cut into Parts.
//
// A LAYER IS CLOSED TOGETHER WITH ITS BASE: it covers every pack live at its
// plan (its compaction's Seen) no lower layer covers, as git bitmaps only a
// chain holding all its commits reach (git 2.56.0 refuses one short a pack:
// "Packfile doesn't have full closure"); a later pack is read beside it until
// the next publication. compaction.go's decide holds both halves: every named
// pack live, every pack live at the plan in it or its base.
type LayerRef struct {
	// LayerID is the multi-pack-index layer's own trailing checksum, 40
	// lowercase hex digits: the name git gives the layer's files
	// (multi-pack-index-<id>.midx and .bitmap) and its line of the chain
	// file.
	LayerID string `json:"layer_id"`
	// BaseID is the layer directly below it in the chain the record leaves,
	// "" for the root. A layer file does not name its base, so the rows'
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
	// layer holds: what a Gitlet node's per-object charges for the layer read.
	Objects int64 `json:"objects"`
	Commits int64 `json:"commits"`
	// Git is the release of git that wrote it, as git's own version line
	// spells its first three numbers ("2.56.0"): a Gitlet node whose git is
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
// oldest first, once their grace has passed ([Store.RetireSweep]).
//
// PURE IN THE RECORD: the duty picks Through by its clock against each row's
// broker-stamped retired_at, and the apply reads no clock, so every node
// removes the same rows and a suite drives the grace with the duty's clock.
// The two clocks differ by at most twice objstore.AssumedClockSkew while no
// node raises `clock_skew`, which [RetireGrace] outlasts past the longest read.
type RetireSweep struct {
	RepoID string `json:"repo_id"`
	// Through is the highest retired_position whose rows the duty's clock
	// put past RetireGrace, naming whole records' retirements, never part of
	// one. The apply removes no row above it.
	Through statelog.Position `json:"through"`
}

// ExpungeRequest asks for an expunge of a repository's unreachable objects, a
// person's purge; its record writes the request alone. The maintainer serves
// it with no grace for what a ref drop left, through as many begins and
// commits as the live packs need, and it is SERVED once no pack dates its
// content from before it ([PackRow.Content]). A request asked while one is
// open WIDENS it, moving the open request to its own id and position, so the
// purge is served only once nothing from before the latest request is left.
//
// Every field is in the base format, as [OpExpungeRequest] is.
type ExpungeRequest struct {
	RepoID string `json:"repo_id"`
	// RequestID is the record's operation id, minted once by whoever asks —
	// an operation id this engine accepts (statelog.CheckCallerOpID) — so a
	// request asked again after its answer was lost is the same request,
	// collapsed by the operation ledger.
	RequestID string `json:"request_id"`
	// By is the person who asked, by credential and bound human seat: the
	// request's attribution, which its record's actor (the deciding node)
	// does not say.
	By Principal `json:"by"`
}

// ExpungeBegin begins an expunge: the packs it rewrites (Retire) move to
// code_packs_retired, Add (every object a ref reached at planning) joins LIVE
// and Dropping (the rest) DROPPING ([PackDropping]), the drop epoch moves to
// DropEpoch, and ExpungeID is open until its [ExpungeCommit]. Decided by the
// committer ([Committer.ProposeExpunge]) as ONE record, which also removes
// the published layers over its inputs.
//
// IT KEEPS EVERY OBJECT, and a cache serves dropping packs, so no read loses
// one. It decides what a new push may rely on: nothing dropping, and nothing
// at all for a receiver that read an earlier drop epoch, which its committer
// refuses `expunging`. Every field is in the base format, as
// [OpExpungeBegin] is.
type ExpungeBegin struct {
	RepoID string `json:"repo_id"`
	// ExpungeID is minted ONCE by the building node, a bare UUIDv7, and is
	// the record's operation id (a begin re-asked of the runner-up collapses
	// in the ledger), the open expunge until its commit, and what each
	// output's pack id derives from ([DeriveOutputPackID]).
	ExpungeID string `json:"expunge_id"`
	// RequestID is the open request this expunge serves, "" for one on the
	// maintainer's own schedule. PROVENANCE: a commit serves a request by
	// what each pack holds ([PackRow.Content]), never by this.
	RequestID string `json:"request_id,omitempty"`
	// Node is the Gitlet node that built it, for whoever reads the log.
	Node string `json:"node"`
	// Seen is the code log's position the builder's scan read the
	// repository's refs and ref drops at — the tips its outputs were cut by.
	// The committer decides at or past it (the publish's session floor), and
	// a purge's outputs date their content from it ([PackRow.Content]).
	Seen statelog.Position `json:"seen"`
	// DropEpoch is the repository's drop epoch once the begin applies: the
	// one its row held when the builder planned, plus one.
	DropEpoch int64 `json:"drop_epoch"`
	// Grace is how long before the scan a ref drop's object stayed
	// reachable: PruneGrace on the maintainer's schedule, 0 serving a request
	// (a purge drops what a force-push left at once). The APPLY dates the
	// outputs by it ([PackRow.Content]): from Seen at 0, else from the oldest
	// input, since what a grace keeps a purge still asks to drop.
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
	// pack the begin retires is one no Gitlet node can load, and a layer above
	// it one whose base is gone — and none where no layer names an input.
	// The repository's maintainer publishes again once the expunge commits.
	DropLayers []string `json:"drop_layers,omitempty"`
}

// ExpungeCommit ends the open expunge: its dropping packs (Retire) move to
// code_packs_retired, keeping their objects for [RetireGrace] so a reader on
// one finishes, and its RESCUE (Add) joins live: what of them a ref reached at
// the rescue's sync (a push landing between scan and begin), dated from the
// oldest dropping pack ([PackRow.Content]). The open expunge clears, and its
// open request once no pack dates its content from before it.
//
// ITS ONE PRECONDITION IS ITS EXPUNGE, open at its epoch with exactly the
// dropping packs it retires; no ref or push is read, so a busy repository
// still commits (a push checked after the begin relies on no dropping object,
// one checked before it is refused `expunging`). Every field is in the base
// format, as [OpExpungeCommit] is.
type ExpungeCommit struct {
	RepoID string `json:"repo_id"`
	// ExpungeID is the expunge this commit ends: the repository's open one.
	ExpungeID string `json:"expunge_id"`
	// CommitID is minted ONCE per build, a bare UUIDv7, and is the record's
	// operation id and what each rescue pack's id derives from
	// ([DeriveOutputPackID]). Not ExpungeID: the ledger would answer with the
	// begin's record. Not derived from the expunge: a commit re-asked of the
	// runner-up collapses, while a rebuild over a newer sync is another,
	// answered stale rather than collapsed onto a record naming other rescue
	// packs.
	CommitID string `json:"commit_id"`
	// RequestID is the request the expunge serves, "" for none: provenance,
	// as on the begin.
	RequestID string `json:"request_id,omitempty"`
	// Node is the Gitlet node that built it, for whoever reads the log.
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

// ExpungeScan records that a scan on the maintainer's schedule found NO
// expunge due: expunge_scanned_at moves to the record's broker instant, so
// `expunge_due`'s weekly arm leaves the repository out for [ExpungeInterval]
// ON EVERY NODE ([ReadQuery.ScannedBefore]). Whichever node maintains it next,
// after a restart, an upgrade or a membership change, reads the verdict from
// the rows rather than walking the repository again; a node-local verdict
// would not survive those.
//
// Decided by the committer only while no expunge is open (an open one is
// finished, never judged). At most one a repository an ExpungeInterval, and
// none where an expunge committed, whose commit moves the same instant. Every
// field is in the base format, as [OpExpungeScan] is.
type ExpungeScan struct {
	RepoID string `json:"repo_id"`
	// ScanID is minted ONCE by the scanning node, a bare UUIDv7, and is the
	// record's operation id, so a record re-asked of the runner-up collapses
	// in the operation ledger.
	ScanID string `json:"scan_id"`
	// Node is the Gitlet node that scanned, for whoever reads the log.
	Node string `json:"node"`
	// Seen is the code log's position the scan read the repository at — its
	// sync's, or the listing row's version where the repository was judged
	// from its row alone (its live bytes under ExpungeMinBytes) — the
	// publish's session floor.
	Seen statelog.Position `json:"seen"`
}

// ImportChunk lands one chunk of a repository's import ([CreateRepo.Import],
// or a pull mirror's first fill): packs its home fetched and the refs they
// set (each command's Old the ref after the chunks before), as ONE push row
// `via import` ([ViaImport]) by the engine's own account, a code_ref_drops row
// per command leaving an object ([leavesObject]), and import_seq moved to Seq.
// Decided by the committer ([Committer.ProposeImport]) ONLY IN ITS TURN, the
// repository importing at import_seq one below Seq, so a chunk asked twice or
// late is refused stale rather than landed again.
//
// Every field is in the base format, as [OpImportChunk] is.
type ImportChunk struct {
	RepoID string `json:"repo_id"`
	// ChunkID is minted afresh for every attempt, a bare UUIDv7, and is the
	// record's operation id, its push row's push id and what each pack's id
	// derives from ([DeriveOutputPackID]). Idempotence is Seq's
	// precondition, never the id: a rebuilt chunk is another attempt with
	// its own packs.
	ChunkID string `json:"chunk_id"`
	// Node is the Gitlet node that fetched it, for whoever reads the log.
	Node string `json:"node"`
	// Seq is the chunk's place in the import, from 1: the repository's
	// import_seq once it lands.
	Seq int64 `json:"seq"`
	// Packs are the bundles it lands, kind import, each at most MaxPackBytes
	// whole, at most [MaxImportPacks] — none for a chunk whose refs the
	// packs of the chunks before it already hold.
	Packs []PackRef `json:"packs,omitempty"`
	// Commands are the refs it sets, at most [MaxImportRefs], each decided
	// ok by its committer or the chunk refused whole. A command's Forced is
	// its builder's, which read the history it fetched.
	Commands []Command `json:"commands,omitempty"`
}

// ImportDone ends a repository's import: it is active from this record on,
// with imported_at the record's instant, import_error why the import ended
// short ("" for none) and, where DefaultBranch is set, its default branch moved
// to the source's HEAD branch, named only when the declared default is absent
// from what it imported (M6.md §2.3). Decided by the committer
// ([Committer.ProposeImport]) only while it is importing at import_seq Seq.
//
// Every field is in the base format, as [OpImportDone] is.
type ImportDone struct {
	RepoID string `json:"repo_id"`
	// Node is the Gitlet node that ended it.
	Node string `json:"node"`
	// Seq is how many chunks landed — the repository's import_seq — 0 for an
	// import that landed none.
	Seq int64 `json:"seq"`
	// DefaultBranch is the branch, by name ("main"), the repository's
	// default moves to, "" to keep the one it declares.
	DefaultBranch string `json:"default_branch,omitempty"`
	// Error is the one-sentence permanent refusal that ended the import
	// short (a blob past code.limits.max_blob_bytes, the repository past
	// code.limits.repo_max_bytes), "" for one that landed whole. The
	// repository keeps what landed.
	Error string `json:"error,omitempty"`
}

// MirrorSync moves a PULL MIRROR ([RepoSettings.MirrorFrom]) onto its source
// as its home last fetched it: packs of what it lacked and refs moved to the
// source's (each command's Old the home copy's), as ONE push row `via mirror`
// ([ViaMirror]) by the engine, a code_ref_drops row per command leaving an
// object, and mirror_synced_at moved to the record's instant. Decided by the
// committer ([Committer.ProposeMirrorSync]): stale where a named ref moved
// since the home read it, and `expunging`, recorded nowhere, where the drop
// epoch differs from the one the home copy's packs were cut against (M6-D4).
//
// A sync finding the source level carries no pack or command
// ([MirrorSync.Current]) and moves mirror_synced_at alone, which `mirrors_due`
// orders by. Base format, as [OpMirrorSync] is; no versionedFields row.
type MirrorSync struct {
	RepoID string `json:"repo_id"`
	// SyncID is minted afresh for every round — a bare UUIDv7 — and is the
	// record's operation id, its push row's push id and what each pack's id
	// is derived from ([DeriveOutputPackID]).
	SyncID string `json:"sync_id"`
	// Node is the Gitlet node that fetched it.
	Node string `json:"node"`
	// DropEpoch is the repository's drop epoch as its home's copy held it
	// when it cut the packs (cache.Synced.DropEpoch).
	DropEpoch int64 `json:"drop_epoch"`
	// Packs are the bundles it lands, kind mirror, at most [MaxImportPacks].
	Packs []PackRef `json:"packs,omitempty"`
	// Commands are the refs it moves, at most [MaxImportRefs]; Forced is its
	// builder's, as an import chunk's is.
	Commands []Command `json:"commands,omitempty"`
	// FetchedAt is when its home fetched the source, by the home's clock.
	FetchedAt time.Time `json:"fetched_at"`
}

// Current reports whether s moves nothing: its home found the mirror level
// with its source, so it lands no pack and no push row, and records only
// when it was found so.
func (s MirrorSync) Current() bool { return len(s.Commands) == 0 && len(s.Packs) == 0 }

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
// THE DISPATCH IS ON (kind, op) AND NOTHING ELSE, and an unknown pair is an
// error naming both rather than a nil payload the applier would take for an
// empty one: it is a newer peer's record.
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
	case kind == KindRepo && op == OpExpungeScan:
		return decodePayload[ExpungeScan](rec)
	case kind == KindRepo && op == OpImportChunk:
		return decodePayload[ImportChunk](rec)
	case kind == KindRepo && op == OpImportDone:
		return decodePayload[ImportDone](rec)
	case kind == KindRepo && op == OpMirrorSync:
		return decodePayload[MirrorSync](rec)
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
	case kind == KindRepo && op == OpMerge:
		return decodePayload[Merge](rec)
	case kind == KindRepo && op == OpEnqueue:
		return decodePayload[Enqueue](rec)
	case kind == KindRepo && op == OpDequeue:
		return decodePayload[Dequeue](rec)
	case kind == KindRepo && op == OpCandidate:
		return decodePayload[Candidate](rec)
	case kind == KindRepo && op == OpEject:
		return decodePayload[Eject](rec)
	case kind == KindRepo && op == OpLand:
		return decodePayload[Land](rec)
	case kind == KindRepo && op == OpGreen:
		return decodePayload[Green](rec)
	case kind == KindRepo && op == OpBroken:
		return decodePayload[Broken](rec)
	case kind == KindRepo && op == OpCloseSettled:
		return decodePayload[CloseSettled](rec)
	case kind == KindRepo && op == OpQueueSweep:
		return decodePayload[QueueSweep](rec)
	case kind == KindRepo && op == OpViewedKey:
		return decodePayload[ViewedKeyRecord](rec)
	case kind == KindChange && op == OpReviewers:
		return decodePayload[ReviewerRequest](rec)
	case kind == KindChange && op == OpComment:
		return decodePayload[Comment](rec)
	case kind == KindChange && op == OpThreadResolve:
		return decodePayload[ThreadResolve](rec)
	case kind == KindChange && op == OpCommentEdit:
		return decodePayload[CommentEdit](rec)
	case kind == KindChange && op == OpCommentDelete:
		return decodePayload[CommentDelete](rec)
	case kind == KindChange && op == OpReview:
		return decodePayload[Review](rec)
	case kind == KindChange && op == OpFindingResolve:
		return decodePayload[FindingResolve](rec)
	case kind == KindChange && op == OpRevise:
		return decodePayload[Revise](rec)
	case kind == KindChange && op == OpMergeability:
		return decodePayload[Mergeability](rec)
	case kind == KindChange && op == OpMergeabilityKept:
		return decodePayload[MergeabilityKept](rec)
	case kind == KindChange && op == OpAsks:
		return decodePayload[AskSettle](rec)
	case kind == KindChecks && op == OpCheckQueued:
		return decodePayload[CheckQueued](rec)
	case kind == KindChecks && op == OpCheckStarted:
		return decodePayload[CheckStarted](rec)
	case kind == KindChecks && op == OpCheckFinished:
		return decodePayload[CheckFinished](rec)
	case kind == KindChecks && op == OpChecksSweep:
		return decodePayload[ChecksSweep](rec)
	case kind == KindChecks && op == OpCheckDispatched:
		return decodePayload[CheckDispatched](rec)
	case kind == KindPerson && op == OpMarks:
		return decodePayload[PersonMarks](rec)
	case kind == KindPerson && op == OpViewed:
		return decodePayload[ViewedMarks](rec)
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
	return nil, fmt.Errorf("gitlet: no payload shape for (%s, %s) — a record's "+
		"kind and op together name its payload, and a pair this build does not "+
		"know is a newer peer's record rather than an empty one", kind, op)
}

// decodePayload reads one typed payload.
func decodePayload[T any](rec MutationRecord) (T, error) {
	var out T
	if len(rec.Mutation) == 0 {
		return out, fmt.Errorf("gitlet: the record on %s carries no payload, and "+
			"(%s, %s) needs one", rec.Subject, rec.Subject.Kind, rec.Op)
	}
	if err := json.Unmarshal(rec.Mutation, &out); err != nil {
		return out, fmt.Errorf("gitlet: decode the %s payload on %s: %w", rec.Op, rec.Subject, err)
	}
	return out, nil
}
