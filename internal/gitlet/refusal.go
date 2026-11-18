package gitlet

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/toolname"
)

// Refusal is why a command was refused, or why a push was not committed — a
// code a client sees as "<code>: <sentence>". A named string, so a code this
// build does not know is a value rather than a panic.
type Refusal string

// The codes. The first twenty may be RECORDED on a command ([Refusal.Recorded]);
// the last eleven are answers ABOUT a push, never part of one.
const (
	// RefusalStale is a ref that is not where the client thought.
	RefusalStale Refusal = "stale"
	// RefusalProtectedForce is a force-push a protection refuses.
	RefusalProtectedForce Refusal = "protected_force"
	// RefusalProtectedDelete is a deletion a protection refuses.
	RefusalProtectedDelete Refusal = "protected_delete"
	// RefusalProtectedDirect is a direct push to a ref no one pushes to.
	RefusalProtectedDirect Refusal = "protected_direct"
	// RefusalTagMoved is a tag pushed over another value.
	RefusalTagMoved Refusal = "tag_moved"
	// RefusalReservedRef is a ref outside refs/heads/ and refs/tags/, one
	// under the engine's refs/crewlet/, or refs/heads/seats itself or a seat's
	// namespace as a branch ([gitref.ReservedSeatBranch] says why).
	RefusalReservedRef Refusal = "reserved_ref"
	// RefusalNotPermitted is a command the principal's grant on the
	// repository does not cover ([Grant.RefRule], ADR-0088).
	RefusalNotPermitted Refusal = "not_permitted"
	// RefusalBadRef is not a ref.
	RefusalBadRef Refusal = "bad_ref"
	// RefusalRepoArchived is a push to a read-only repository.
	RefusalRepoArchived Refusal = "repo_archived"
	// RefusalRepoDeleted is a push to a repository that is deleted, being
	// purged, purged or not there at all — four states with four remedies,
	// so its sentence is formed from the state ([RepoGone.Detail]).
	RefusalRepoDeleted Refusal = "repo_deleted"
	// RefusalRepoImporting is a write to a repository being imported from
	// another host ([RepoStatusImporting]) — a push, a change's every write,
	// an expunge — which takes none until its import is done
	// ([ImportingDetail]). Recordable as repo_archived is, in the base format
	// ([RecordVersion]), though the push decide refuses the whole push for it.
	RefusalRepoImporting Refusal = "repo_importing"
	// RefusalRepoMirrored is a write to a PULL MIRROR ([RepoRef.Mirrored]) —
	// a push, a change's every write — which moves only by its syncs from
	// its source ([MirroredDetail]). Recordable as repo_importing is.
	RefusalRepoMirrored Refusal = "repo_mirrored"
	// RefusalRepoSize is a push past code.limits.repo_max_bytes.
	RefusalRepoSize Refusal = "repo_size"
	// RefusalRepoRefs is a create past MaxRefsPerRepo live refs.
	RefusalRepoRefs Refusal = "repo_refs"
	// RefusalRefConflict is a create beside a live ref of the directory it
	// would make, or with live refs under its own name: refs/heads/feature
	// and refs/heads/feature/x are never both live, because every client
	// keeps a ref as a file and a file is never also a directory.
	RefusalRefConflict Refusal = "ref_conflict"
	// RefusalDefaultBranch is a deletion of the repository's default branch
	// ([DefaultBranchRefusal]), refused whoever deletes it (a push, a write, a
	// merge deleting its source) until another branch is made the default.
	// Recorded, as a protection's refusal is: who tried is an audit fact. An
	// older build reads the code as a value and its detail as written.
	RefusalDefaultBranch Refusal = "default_branch"
	// RefusalUploadExpired is a pack uploaded longer ago than a write may
	// still name it (objstore.RecordWithin).
	RefusalUploadExpired Refusal = "upload_expired"
	// RefusalAtomic is a command refused with the rest of an --atomic push.
	RefusalAtomic Refusal = "atomic"
	// RefusalBlobTooLarge is a blob past code.limits.max_blob_bytes.
	RefusalBlobTooLarge Refusal = "blob_too_large"
	// RefusalPackTooLarge is a pack bundle past MaxPackBytes, refused by the
	// receiver before anything is uploaded.
	RefusalPackTooLarge Refusal = "pack_too_large"

	// RefusalBusy is a repository taking pushes faster than one node
	// commits them.
	RefusalBusy Refusal = "busy"
	// RefusalCompactionBehind is a push carrying a pack into a repository
	// holding MaxPacksPerRepo live packs or more: nothing was decided, and it
	// lands once compaction has merged the packs. A push carrying no pack is
	// never refused for it. The push decide refuses it from the rows, counting
	// its batch's earlier entries' packs ([working.behind]); the receiver,
	// before upload, from the row read again; one sentence ([BehindDetail]).
	//
	// An answer about a push, never recorded ([Refusal.Recorded]): the decide
	// refuses the whole push, so it is no field of the code domain's records
	// and takes no versionedFields row. The estate router carries it to the
	// receiver in [PushAnswer.Refused], where a build that does not know it
	// says its name.
	RefusalCompactionBehind Refusal = "compaction_behind"
	// RefusalPushQuota is a seat's push past code.limits.pushes_per_seat_day
	// ([Limits.PushesPerSeatDay]), every repository together: nothing was
	// decided, and it lands once the company day turns ([quotaDetail] names
	// the instant). A person's or a token's push is never counted: the bound
	// is on what a runaway seat costs the code log, a record a push.
	//
	// Unrecorded for that reason, since a recorded refusal would still cost
	// the log an entry a push. An answer about a push, as compaction_behind
	// is; the batch's other entries go on.
	RefusalPushQuota Refusal = "push_quota"
	// RefusalExpunging is a push its receiver checked at another drop epoch
	// than the repository's ([Proposal.DropEpoch]): an expunge began since
	// ([ExpungeBegin]), so a command may name an object its commit retires.
	// Nothing was decided; run again, it is checked against the repository as
	// the begin left it, a deletion too (the refusal is of the copy, not of a
	// command). The push decide refuses it after the repository's status and
	// before any rule of the push's own ([ExpungingDetail]). An answer about a
	// push, as compaction_behind is.
	RefusalExpunging Refusal = "expunging"
	// RefusalTooManyChanges is a push whose live changes' rows (each change's
	// row, a head move's revision and author, a merge's and a close's row:
	// revisions.go) and landings on protected branches (two rows each,
	// [PushEntry.Landings]) take its record past [MaxRecordRows] even alone,
	// in practice a bot force-pushing every change it rebased. Nothing was
	// decided; it lands as several smaller pushes. The push decide counts the
	// rows as the apply writes them ([changeBook], [entryRows]); a push that
	// fits alone but not beside earlier entries is not refused: the committer
	// carries it to lead its next record. An answer about a push, as
	// compaction_behind is.
	RefusalTooManyChanges Refusal = "too_many_changes"
	// RefusalUnavailable is the code log or the object store refusing.
	RefusalUnavailable Refusal = "unavailable"
	// RefusalStoreFull is a push whose bundle the object store refused for lack
	// of room (objstore.ErrFull): nothing was stored or proposed. Not
	// unavailable, whose sentence says to push again: a full store refuses the
	// same push until an operator makes the room (objstore.Full), which the
	// landing's own line names. An answer about a push, decided by its receiver
	// before any proposal: never recorded ([Refusal.Recorded]) nor carried by
	// the estate router, so no record field and no versionedFields row.
	RefusalStoreFull Refusal = "store_full"
	// RefusalForgedCommitter is a coding box's push whose pack holds a commit
	// the repository does not already hold committed as the engine
	// (`engine@<code.identity_domain>`, in any case; ADR-0092), or more such
	// commits than one push is checked for: refused whole by its receiver
	// before any upload. A box runs code nobody vouches for, and such a commit
	// would read as the host's own write in every clone; a person's push and an
	// import may carry the engine's commits as git carries any. An answer
	// about a push, as store_full is.
	RefusalForgedCommitter Refusal = "forged_committer"
	// RefusalNotApplied is a push durable on the log and applied nowhere.
	RefusalNotApplied Refusal = "not_applied"
	// RefusalUnknown is a push whose outcome could not be established.
	RefusalUnknown Refusal = "unknown"
	// RefusalPending is a push durable at a position this node has not
	// resolved.
	RefusalPending Refusal = "pending"
)

// Refusals is every code, in this order.
func Refusals() []Refusal {
	return []Refusal{
		RefusalStale, RefusalProtectedForce, RefusalProtectedDelete,
		RefusalProtectedDirect, RefusalTagMoved, RefusalReservedRef, RefusalNotPermitted, RefusalBadRef,
		RefusalRepoArchived, RefusalRepoDeleted, RefusalRepoImporting, RefusalRepoMirrored, RefusalRepoSize,
		RefusalRepoRefs, RefusalRefConflict, RefusalDefaultBranch,
		RefusalUploadExpired, RefusalAtomic, RefusalBlobTooLarge, RefusalPackTooLarge,
		RefusalBusy, RefusalCompactionBehind, RefusalPushQuota, RefusalExpunging, RefusalTooManyChanges,
		RefusalUnavailable, RefusalStoreFull, RefusalForgedCommitter, RefusalNotApplied, RefusalUnknown,
		RefusalPending,
	}
}

// Valid reports a code this build knows.
func (r Refusal) Valid() bool { return slices.Contains(Refusals(), r) }

// Recorded reports a code that may appear on a recorded command: every one but
// the eleven that are answers ABOUT a push — whether it could be committed at
// all — rather than a verdict on one of its commands.
func (r Refusal) Recorded() bool {
	switch r {
	case RefusalBusy, RefusalCompactionBehind, RefusalPushQuota, RefusalExpunging, RefusalTooManyChanges,
		RefusalUnavailable, RefusalStoreFull, RefusalForgedCommitter, RefusalNotApplied, RefusalUnknown,
		RefusalPending:
		return false
	}
	return r.Valid()
}

// sentences is each code's fixed sentence, with no parameters: what a client
// sees as "<code>: <sentence>" when no Detail was formed.
var sentences = map[Refusal]string{
	RefusalStale:           "the ref is at another commit; fetch first",
	RefusalProtectedForce:  "a protection on this ref refuses a force-push (allow_force)",
	RefusalProtectedDelete: "a protection on this ref refuses its deletion (allow_delete)",
	RefusalProtectedDirect: "a protection on this ref takes no direct push (direct_push: none), or a " +
		"person's alone (direct_push: persons)",
	RefusalTagMoved: "a tag does not move: delete it and push it again",
	RefusalReservedRef: "only refs/heads/ and refs/tags/ are pushed; refs/crewlet/ is the engine's, and " +
		"refs/heads/seats holds only each seat's own namespace, refs/heads/seats/<handle>/",
	RefusalNotPermitted: "the caller's grant on this repository does not cover this command",
	RefusalBadRef:       "not a ref: git's check-ref-format rules, at most 255 bytes of UTF-8 under refs/<namespace>/",
	RefusalRepoArchived: "the repository is archived and read-only",
	RefusalRepoDeleted:  "the repository is deleted, being purged or gone, and takes no push",
	RefusalRepoImporting: "the repository is being imported from another host, and takes no push until its " +
		"import is done",
	RefusalRepoMirrored: "the repository is a pull mirror, which moves only by its syncs from its source; push " +
		"to its source",
	RefusalRepoSize: "the repository would hold more than code.limits.repo_max_bytes",
	RefusalRepoRefs: "the repository holds as many live refs as one repository may",
	RefusalRefConflict: "a ref and a directory of refs never share a name (feature beside feature/x): " +
		"every clone holding one could not fetch the other",
	RefusalDefaultBranch: "the repository's default branch is not deleted: make another branch the default " +
		"first",
	RefusalUploadExpired: "the pack was uploaded too long ago to be committed; run git push again",
	RefusalAtomic:        "refused with the rest of this --atomic push",
	RefusalBlobTooLarge:  "a blob is larger than code.limits.max_blob_bytes; keep large binaries in Worklet's project files",
	RefusalPackTooLarge: "the pack this push stores — completed against the repository and " +
		"indexed — is larger than one push may store; push fewer commits at once",
	RefusalBusy: "the repository is taking pushes faster than they are committed; run git push again",
	RefusalCompactionBehind: "the repository holds as many live packs as one may until compaction catches up, " +
		"and nothing was decided; run git push again in a minute",
	RefusalPushQuota: "the seat has pushed as many times today as code.limits.pushes_per_seat_day allows, " +
		"and nothing was decided; the count starts again when the company's day turns",
	RefusalExpunging: "an expunge of the repository's unreachable objects began after the push was checked, " +
		"and nothing was decided; run git push again",
	RefusalTooManyChanges: "the push moves more live changes than one record can carry beside its own refs, " +
		"and nothing was decided; push their branches in fewer pushes",
	RefusalUnavailable: "the code log or the object store refused the write; run git push again",
	RefusalNotApplied:  "the push was durable and applied nowhere; run git push again",
	RefusalUnknown:     "the push's outcome is unknown; fetch, then run git push again",
	RefusalPending:     "the push is durable and unresolved here; run git push again (a push that landed reports up to date)",
	RefusalStoreFull:   "the object store has no room for the push, and nothing was stored; it lands only once an operator makes room",
	RefusalForgedCommitter: "a commit the push sends is committed as Gitlet's engine, which only the " +
		"host's own writes are, and a coding box sends none the repository does not already hold; nothing " +
		"was stored",
}

// Sentence is the code's fixed sentence, or the code itself for one this build
// does not know.
func (r Refusal) Sentence() string {
	if s, ok := sentences[r]; ok {
		return s
	}
	return string(r)
}

// RepoGone is what became of a repository a push is refused repo_deleted for,
// as the rows hold it: the one input of [RepoGone.Detail].
//
// One code, four states, four remedies: DELETED is restored by declaring it
// again until its purge begins (code.deleted_retention_days on); PURGING has
// no undo, and its name declared again waits for the purge
// (code_repo_awaits_purge), then is a new, empty repository; PURGED left only
// its deletion marker, and its name makes a new repository; an id no record
// created is none. One code, since a client pushes to none of them; one
// sentence for all four would promise a false restore.
type RepoGone struct {
	// ID is the repository the push named.
	ID string
	// Name is its name — its row's, or its deletion marker's once purged —
	// and "" where nothing this node holds names it, or the marker that does
	// could not be read: the id names it then.
	Name string
	// Status is its row's, deleted or purging, and "" where it has no row.
	Status RepoStatus
	// DeletedAt is when a repository with a row was deleted, and Retention
	// code.deleted_retention_days on the deciding node's epoch: how long
	// after DeletedAt its purge may begin. Either zero is not known, and no
	// instant is stated.
	DeletedAt time.Time
	Retention time.Duration
	// Purged is a repository with no row whose deletion marker this node
	// holds: its purge has completed.
	Purged bool
	// ManagedBy is who manages a repository with a row, which decides its
	// remedy: a declaration for code.repos', a code-tools restore for a
	// tool's. "" (no row) is a configuration's, the only kind a purged one's
	// marker was written for before tools made repositories.
	ManagedBy ManagedBy
}

// Detail is the sentence of a push or a compaction refused repo_deleted for g:
// what became of the repository and what, by who manages it, brings it back.
// A deleted one's names when its purge MAY BEGIN (the deletion plus the
// deciding node's retention; the duty runs on a tick and may begin later),
// never "restorable until", which nothing holds the purge to.
func (g RepoGone) Detail() string {
	named := cmp.Or(g.Name, g.ID)
	restores, recreates := "declaring it again in code.repos restores it", "declaring its name in code.repos"
	if g.ManagedBy == ManagedTools {
		restores, recreates = "a restore through the code tools brings it back", "creating it again"
	}
	switch {
	case g.Status == RepoStatusDeleted && !g.DeletedAt.IsZero() && g.Retention > 0:
		return fmt.Sprintf("repository %s is deleted; %s as it was, until its purge, which may begin from "+
			"%s (code.deleted_retention_days after its deletion)", named, restores,
			g.DeletedAt.Add(g.Retention).UTC().Format(time.RFC3339))
	case g.Status == RepoStatusDeleted:
		return fmt.Sprintf("repository %s is deleted; %s as it was, until its purge", named, restores)
	case g.Status == RepoStatusPurging:
		return fmt.Sprintf("repository %s is being purged, which cannot be undone; once the purge "+
			"completes, %s creates a new, empty repository", named, recreates)
	case g.Purged:
		return fmt.Sprintf("repository %s was purged and holds nothing; %s creates a new, empty "+
			"repository", named, recreates)
	}
	return fmt.Sprintf("repository %s does not exist", g.ID)
}

// ArchivedDetail is the sentence of a push refused repo_archived: which
// repository, where it is archived (code.repos, or a tool) and what it still
// serves. One spelling for the push decide and Gitlet's check at a push's
// advertisement or request (ADR-0008).
func ArchivedDetail(name string, managedBy ManagedBy) string {
	if managedBy == ManagedTools {
		return fmt.Sprintf("repository %s is archived and read-only: it is fetched, and takes no push "+
			"until it is unarchived through the code tools", name)
	}
	return fmt.Sprintf("repository %s is archived and read-only (code.repos archived: true): "+
		"it is fetched, and takes no push", name)
}

// BehindDetail is the sentence of a push refused compaction_behind: the
// repository, its live packs, the bound ([MaxPacksPerRepo]) and that nothing
// was decided; one spelling for the push decide and the receiver (ADR-0008).
//
// It never calls the bound the most a repository may hold: a later build may
// raise it, and mid-upgrade a repository can hold more. It asks for the push
// again in a minute, unlike busy: only pushes outrunning compaction meet the
// bound, and a retry within seconds meets it again, costing the receiver its
// row read and the client its whole pack's send.
func BehindDetail(name string, held int64) string {
	return fmt.Sprintf("%s holds %d live packs, and a push adds none past %d until compaction catches up: "+
		"nothing was decided; run git push again in a minute", name, held, MaxPacksPerRepo)
}

// ExpungingDetail is the sentence of a push refused expunging: the
// repository, the drop epoch its receiver checked and the repository's, and
// that nothing was decided. Formed by the push decide alone: a receiver has
// only its own copy's epoch. It asks for the push again at once, unlike
// compaction_behind: the next receive syncs its copy, and a second begin
// waits for the first expunge's commit. An epoch PAST the repository's is no
// expunge begun since, and is said as it is.
func ExpungingDetail(name string, checked, holds int64) string {
	if checked < holds {
		return fmt.Sprintf("an expunge of %s's unreachable objects began after this push was checked "+
			"against it (drop epoch %d, now %d): nothing was decided; run git push again", name, checked, holds)
	}
	return fmt.Sprintf("this push was checked against %s at drop epoch %d, and it is at %d: nothing was "+
		"decided; run git push again", name, checked, holds)
}

// ExpungingBuiltDetail is the sentence of a merge, a train or a landing refused
// expunging ([MergeRefusalExpunging]): what was built (what), who builds it
// again (builder), the epoch its build read (built) and the repository's
// (holds) — and that nothing of it was decided. A push's sentence
// ([ExpungingDetail]) tells its client to push again; this one names the
// builder, which builds the record again against the repository as the begin
// left it.
func ExpungingBuiltDetail(name, what, builder string, built, holds int64) string {
	if built < holds {
		return fmt.Sprintf("an expunge of %s's unreachable objects began after this %s was built (drop epoch "+
			"%d, now %d): nothing was decided; %s builds it again", name, what, built, holds, builder)
	}
	return fmt.Sprintf("this %s was built against %s at drop epoch %d, and it is at %d: nothing was decided; "+
		"%s builds it again", what, name, built, holds, builder)
}

// tooManyChangesDetail is the sentence of a push refused too_many_changes: how
// many live changes it moves, how many protected branches it lands on where it
// lands on any, the rows a record of its own would hold with their rows beside
// its own, and the bound. Formed by the push decide alone, the one counter of
// a record's change and landing rows.
func tooManyChangesDetail(changes, landings, rows int) string {
	moves := fmt.Sprintf("this push moves %d live changes", changes)
	if landings > 0 {
		moves += fmt.Sprintf(" and lands on %d protected branches", landings)
	}
	return fmt.Sprintf("%s, and their rows beside its own would take its record to %d of the %d rows one record "+
		"may write: nothing was decided; push their branches in fewer pushes", moves, rows, MaxRecordRows)
}

// StatelessRefusal is the rules of the push decide that no state decides —
// or "" when none fires, checked in this order:
//
//  1. a ref [ValidRef] refuses is bad_ref — before anything else, because
//     no other rule can reason about something that is not a ref;
//  2. a ref [reservedRef] reserves is reserved_ref;
//  3. a tag whose old and new values are both commits is tag_moved — a tag
//     may be created and deleted, never moved.
//
// ONE FUNCTION the decide and the receiver both call (ADR-0008): the decide is
// authoritative, and the receiver may refuse early, before it uploads anything.
func StatelessRefusal(c Command) Refusal {
	switch {
	case !ValidRef(c.Ref):
		return RefusalBadRef
	case reservedRef(c.Ref):
		return RefusalReservedRef
	case strings.HasPrefix(c.Ref, tagPrefix) && c.Old != ZeroOID && c.New != ZeroOID:
		return RefusalTagMoved
	}
	return ""
}

// Attempted reports a push every command of which is refused, one at least by a rule on who may move
// which ref ([attemptRefusals]): what is recorded of a push git received nothing of which lands, an
// ATTEMPT that moves nothing ([PushEntry]).
func Attempted(cmds []Command) bool {
	ruled := false
	for _, c := range cmds {
		if c.Result != ResultNG {
			return false
		}
		ruled = ruled || slices.Contains(attemptRefusals, c.Reason)
	}
	return ruled
}

// attemptRefusals are the refusals a rule makes of the pusher's own update, each a rule a reader can
// open; a refusal of the client's view, the content's size, the repository's state or the fleet's is
// no attempt.
var attemptRefusals = []Refusal{RefusalProtectedDirect, RefusalProtectedForce, RefusalProtectedDelete,
	RefusalTagMoved, RefusalReservedRef, RefusalNotPermitted, RefusalDefaultBranch}

// DefaultBranchRefusal refuses default_branch, with its sentence, a deletion of
// repository repo's default branch defaultBranch ("main"); "" for every other
// command.
//
// The host's rule, not a protection's: with HEAD's branch deleted, clones check
// out nothing and a change with no target proposes into nothing. No setting
// wants that, so it is refused whoever asks, a person included; the remedy is
// another branch made the default first (code_update_repo). One function every
// judge of a deletion asks (ADR-0008): the push decide (after the protections,
// so a protected default branch is refused by the rule that names it), an
// engine write's judge, and a merge deleting its source.
func DefaultBranchRefusal(c Command, repo, defaultBranch string) (Refusal, string) {
	if c.New != ZeroOID || defaultBranch == "" || c.Ref != branchPrefix+defaultBranch {
		return "", ""
	}
	return RefusalDefaultBranch, DefaultBranchDetail(repo, defaultBranch)
}

// DefaultBranchDetail is the sentence of a deletion refused default_branch:
// which branch, of which repository, and the change that lets it go.
func DefaultBranchDetail(repo, defaultBranch string) string {
	return fmt.Sprintf("%s is %s's default branch, which every clone checks out, so it is not deleted: "+
		"make another branch the default first ("+toolname.CodeUpdateRepo+" default_branch)", defaultBranch, repo)
}

// RepoStateRefusal refuses every command on an archived, importing or mirrored
// repository, with its sentence, and "" for one that takes them; a deleted one is
// the caller's. One rule for the push decide, an engine write and a refs listing.
func RepoStateRefusal(repo RepoRef) (Refusal, string) {
	switch {
	case repo.Status == RepoStatusArchived:
		return RefusalRepoArchived, ArchivedDetail(repo.Name, repo.ManagedBy)
	case repo.Status == RepoStatusImporting:
		return RefusalRepoImporting, ImportingDetail(repo.Name)
	case repo.Mirrored:
		return RefusalRepoMirrored, MirroredDetail(repo.Name, repo.ManagedBy)
	}
	return "", ""
}

// reservedRef reports a ref no principal pushes: outside refs/heads/ and
// refs/tags/, under refs/crewlet/, or a seat ref [reservedSeatRef] reserves.
// Every judge of a push refuses it before any grant is read, a person's push
// included.
func reservedRef(ref string) bool {
	return strings.HasPrefix(ref, reservedPrefix) ||
		(!strings.HasPrefix(ref, branchPrefix) && !strings.HasPrefix(ref, tagPrefix)) ||
		reservedSeatRef(ref)
}

// Protections is a repository's protection rules with each pattern parsed
// once ([ParseProtections]): the push decide asks every rule about every
// command of a batch, and the receiver about every command of a push, and
// parsing per question cost a maximal round 192 000 re-parses (doc.go's
// measured section).
//
// The zero Protections holds no rule.
type Protections struct {
	rules    []PushRule
	patterns []gitref.Pattern
}

// protects reports a ref some rule of ps matches: one a push to lands on a
// protected branch, where it is a branch ([PushEntry.Landings]).
func (ps Protections) protects(ref string) bool {
	return slices.ContainsFunc(ps.patterns, func(p gitref.Pattern) bool { return p.Match(ref) })
}

// ParseProtections reads rules for asking about many refs, refusing — rather
// than reading as a rule that protects nothing — one whose pattern is not a
// protection pattern: every stored rule passed the configuration's validation
// and [RepoSettings.Validate] before any record carried it, so one that does
// not parse is an estate that holds what no writer of this build wrote, and a
// protection read as absent is a protected ref open to every push.
func ParseProtections(rules []PushRule) (Protections, error) {
	out := Protections{rules: rules, patterns: make([]gitref.Pattern, len(rules))}
	for i, rule := range rules {
		p, ok := gitref.ParsePattern(rule.Pattern)
		if !ok {
			return Protections{}, fmt.Errorf("gitlet: protection %q is not a protection pattern "+
				"this build reads, so no push to its repository can be judged against it", rule.Pattern)
		}
		out.patterns[i] = p
	}
	return out, nil
}

// ProtectionRefusal is the push decide's protection rule over EVERY rule
// matching c.Ref (two rules written for two reasons both hold), for a push by
// a principal of kind by: the refusal and the first rule, in order, refusing
// for that reason; "" and the zero PushRule when none fires. Precedence:
// protected_direct (direct_push none, or persons for anybody but a person),
// then protected_delete, then protected_force.
//
// direct_push persons is a person's alone: every rule refuses a seat's direct
// push (unset reads persons), and by is an operator only for a person's (an
// unbound credential's grant refuses it first). One walk also answers the
// rule: a second doubled the costliest round (doc.go's measured section).
// c.Ref must be a ref: [StatelessRefusal] refuses anything else first.
func ProtectionRefusal(c Command, rules Protections, by PrincipalKind) (Refusal, PushRule) {
	deletion, force := -1, -1
	for i, p := range rules.patterns {
		if !p.Match(c.Ref) {
			continue
		}
		rule := rules.rules[i]
		if rule.DirectPush == DirectNone || (rule.DirectPush == DirectPersons && by != PrincipalOperator) {
			return RefusalProtectedDirect, rule
		}
		if deletion < 0 && !rule.AllowDelete {
			deletion = i
		}
		if force < 0 && !rule.AllowForce {
			force = i
		}
	}
	switch {
	case c.New == ZeroOID && deletion >= 0:
		return RefusalProtectedDelete, rules.rules[deletion]
	case c.Forced && force >= 0:
		return RefusalProtectedForce, rules.rules[force]
	}
	return "", PushRule{}
}

// Atomic is an --atomic push's all-or-nothing rule: when any command is ng,
// every command ok or undecided becomes ng atomic, and a noop stays one (no
// refusal makes what the ref already holds untrue). A push with no refusal is
// returned as it is; otherwise a new slice, cmds untouched. One function for
// the decide and the receiver (ADR-0008): the receiver applies it before
// proposing, so a part-refused push never uploads a pack for the rest.
func Atomic(cmds []Command) []Command {
	if !slices.ContainsFunc(cmds, func(c Command) bool { return c.Result == ResultNG }) {
		return cmds
	}
	out := slices.Clone(cmds)
	for i, c := range out {
		if c.Result == ResultNG || c.Result == ResultNoop {
			continue
		}
		out[i].Result, out[i].Reason = ResultNG, RefusalAtomic
		out[i].Detail = atomicDetail
	}
	return out
}

// atomicDetail is what a command refused with the rest of its push says.
const atomicDetail = "refused with the rest of this --atomic push"

// The ref namespaces the stateless rules read.
const (
	branchPrefix   = "refs/heads/"
	tagPrefix      = "refs/tags/"
	reservedPrefix = "refs/crewlet/"
)
