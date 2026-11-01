package code

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/gitref"
)

// Refusal is why a command was refused, or why a push was not committed — a
// code a client sees as "<code>: <sentence>". A named string, so a code this
// build does not know is a value rather than a panic.
type Refusal string

// The codes. The first seventeen may be RECORDED on a command ([Refusal.Recorded]);
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
	// under the engine's own refs/crewlet/, or one of the two kinds of ref
	// under refs/heads/seats that are nobody's: the root itself as a branch,
	// and a seat's namespace as a branch ([gitref.ReservedSeatBranch], which
	// says why: either would block seats' namespaces under
	// [RefusalRefConflict]).
	RefusalReservedRef Refusal = "reserved_ref"
	// RefusalNotPermitted is a command the principal's grant on the
	// repository does not cover ([Grant.RefRule], ADR-0081).
	RefusalNotPermitted Refusal = "not_permitted"
	// RefusalBadRef is not a ref.
	RefusalBadRef Refusal = "bad_ref"
	// RefusalRepoArchived is a push to a read-only repository.
	RefusalRepoArchived Refusal = "repo_archived"
	// RefusalRepoDeleted is a push to a repository that is deleted, being
	// purged, purged or not there at all — four states with four remedies,
	// so its sentence is formed from the state ([RepoGone.Detail]).
	RefusalRepoDeleted Refusal = "repo_deleted"
	// RefusalRepoSize is a push past code.limits.repo_max_bytes.
	RefusalRepoSize Refusal = "repo_size"
	// RefusalRepoRefs is a create past MaxRefsPerRepo live refs.
	RefusalRepoRefs Refusal = "repo_refs"
	// RefusalRefConflict is a create beside a live ref of the directory it
	// would make, or with live refs under its own name: refs/heads/feature
	// and refs/heads/feature/x are never both live, because every client
	// keeps a ref as a file and a file is never also a directory.
	RefusalRefConflict Refusal = "ref_conflict"
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
	// that holds MaxPacksPerRepo live packs or more: its compaction has not
	// caught up with its pushes, nothing of the push was decided, and it
	// lands when it is run again once a compaction has merged the packs. A
	// push carrying no pack — deletions, refs moved onto commits the
	// repository holds — adds none, and is never refused for it.
	//
	// The push decide refuses it, from the rows, counting the packs of its
	// batch's earlier entries ([working.behind]), and the receiver before it
	// uploads the push's bundle, from the row it read again — one sentence
	// for both ([BehindDetail]).
	//
	// AN ANSWER ABOUT A PUSH, never recorded on a command ([Refusal.Recorded]):
	// the decide refuses the whole push and its record carries nothing of it,
	// so it is no field of the code domain's records and takes no
	// versionedFields row. The estate router carries it from a committer to
	// the receiver in [PushAnswer.Refused], where a build that does not know
	// the code says it by its name, beside the decide's sentence.
	RefusalCompactionBehind Refusal = "compaction_behind"
	// RefusalPushQuota is a SEAT'S push past the pushes its seat may make in
	// one company day, every repository together
	// (code.limits.pushes_per_seat_day, [Limits.PushesPerSeatDay]): nothing
	// of the push was decided, and it lands when it is run again once the
	// day has turned — the sentence names the instant ([quotaDetail]). A
	// person's push and a token's are never counted: the bound is on what a
	// RUNAWAY seat costs the code log, a record a push, which a person's
	// own hand already bounds.
	//
	// UNRECORDED, for that reason: a recorded refusal costs the log an entry
	// a refused push, so a seat pushing in a loop past its quota would still
	// cost the log a record a push — the very cost the quota exists to
	// bound. The push decide refuses the whole push, its record carries
	// nothing of it, and the batch's other entries go on.
	//
	// AN ANSWER ABOUT A PUSH, never recorded on a command ([Refusal.Recorded]),
	// as compaction_behind is: no field of the code domain's records and no
	// versionedFields row, carried by the estate router in
	// [PushAnswer.Refused].
	RefusalPushQuota Refusal = "push_quota"
	// RefusalExpunging is a push its receiver checked against the
	// repository at another drop epoch than the repository's
	// ([Proposal.DropEpoch]): an expunge began on it since ([ExpungeBegin]),
	// leaving dropping what the receiver's copy held live, so a command of
	// the push may name an object the expunge's commit retires. Nothing of
	// the push was decided, and it lands when it is run again, checked
	// against the repository as the begin left it — a deletion too, since
	// the refusal is of the copy the push was checked against, never of a
	// command.
	//
	// The push decide refuses it, from the epoch the proposal carries
	// against the row's, after the repository's status and before any rule
	// of the push's own ([ExpungingDetail]).
	//
	// AN ANSWER ABOUT A PUSH, never recorded on a command ([Refusal.Recorded]),
	// as compaction_behind is: no field of the code domain's records and no
	// versionedFields row, carried by the estate router in
	// [PushAnswer.Refused].
	RefusalExpunging Refusal = "expunging"
	// RefusalTooManyChanges is a push that moves more live changes than its
	// record can carry the rows of beside its own — each change's row, the
	// revision a head move writes and the author it makes, a merge's and a
	// close's row (revisions.go) — past [MaxRecordRows] even alone in its
	// record: a push of hundreds of branches each heading a live change, in
	// practice a bot's force-push of every change it rebased. Nothing of it
	// was decided, and it lands as several pushes of fewer branches.
	//
	// The push decide refuses it, from the change rows its snapshot holds,
	// counted as the record's apply will write them ([changeBook]); a push
	// whose changes fit alone but not beside the entries before it is
	// refused nothing — the committer carries it to lead its next record.
	//
	// AN ANSWER ABOUT A PUSH, never recorded on a command ([Refusal.Recorded]),
	// as compaction_behind is: no field of the code domain's records and no
	// versionedFields row, carried by the estate router in
	// [PushAnswer.Refused].
	RefusalTooManyChanges Refusal = "too_many_changes"
	// RefusalUnavailable is the code log or the object store refusing.
	RefusalUnavailable Refusal = "unavailable"
	// RefusalStoreFull is a push whose bundle the object store refused
	// because it has no room for it (objstore.ErrFull): nothing was stored
	// and nothing proposed.
	//
	// NOT unavailable, whose sentence says to run git push again: a full
	// store refuses the same push the same way until an operator makes the
	// room it names (objstore.Full), so that sentence sent a person — and a
	// CI runner on a loop — to push again into a store that could not take
	// it. The landing's own line names that room; this fixed sentence names
	// none, since the room is the store's to say.
	//
	// AN ANSWER ABOUT A PUSH, decided by the node that received it before
	// any proposal: never recorded on a command ([Refusal.Recorded]) and
	// never carried by the estate router, so it is no field of the code
	// domain's records and takes no versionedFields row.
	RefusalStoreFull Refusal = "store_full"
	// RefusalForgedCommitter is a CODING BOX'S push whose pack holds a commit
	// committed as the engine — `engine@<code.identity_domain>`, the address
	// only the host's own writes carry (ADR-0085), in whatever case it is
	// written — that the repository does not already hold, or more such
	// commits than one push is checked for: refused whole by the node that
	// received it before any byte of its bundle was uploaded, so nothing was
	// stored and nothing proposed. A box runs code nobody vouches for, and a
	// commit it stored under the engine's name would read as the host's own
	// write in every clone's history; a person's push and an import may carry
	// the engine's commits as git carries any commit — a branch pushed back
	// from a clone, an export imported again.
	//
	// AN ANSWER ABOUT A PUSH, decided by the node that received it before
	// any proposal, as store_full is: never recorded on a command
	// ([Refusal.Recorded]) and never carried by the estate router, so it is
	// no field of the code domain's records and takes no versionedFields
	// row.
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
		RefusalRepoArchived, RefusalRepoDeleted, RefusalRepoSize, RefusalRepoRefs, RefusalRefConflict,
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
	RefusalRepoSize:     "the repository would hold more than code.limits.repo_max_bytes",
	RefusalRepoRefs:     "the repository holds as many live refs as one repository may",
	RefusalRefConflict: "a ref and a directory of refs never share a name (feature beside feature/x): " +
		"every clone holding one could not fetch the other",
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
	RefusalForgedCommitter: "a commit the push sends is committed as the code host's engine, which only the " +
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
// as the rows that refuse it hold it: the one input of the sentence that
// refusal carries ([RepoGone.Detail]).
//
// ONE CODE, FOUR STATES, FOUR REMEDIES. A repository removed from code.repos
// is DELETED, and declaring it again brings it back as it was — until its
// purge begins, code.deleted_retention_days after the deletion at the
// earliest. Once its purge has begun it is PURGING, which has no undo: the
// name declared again waits for the purge (code_repo_awaits_purge) and is
// then a NEW, empty repository. A PURGED one has left only its deletion
// marker, and its name declared again is likewise a new repository. And an
// id no record ever created is no repository at all. The code stays one,
// because what a client does about all four is the same — it cannot push
// there — but one sentence for the four told a client whose repository was
// being purged, or was gone, that declaring it again would restore it: the
// one remedy that is false there.
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
	// remedy: a declaration restores a repository code.repos declares, and a
	// restore through the code tools one a tool made. "" — a repository with
	// no row, whose manager nothing this node holds says — is a
	// configuration's, the only kind a purged one's marker was written for
	// before tools made repositories.
	ManagedBy ManagedBy
}

// Detail is the sentence of a push or a compaction refused repo_deleted for g:
// what became of the repository and what, if anything, brings it back — by
// who manages it: a declaration for a repository code.repos declares, a
// restore through the code tools for one a tool made.
//
// A deleted repository's sentence names the instant its purge may begin — the
// deletion plus the deciding node's retention: the earliest the code-retention
// duty begins it under that epoch, which runs on a tick and may begin it
// later. Named as when the purge MAY BEGIN, never as "restorable until", which
// would promise an instant nothing holds the purge to.
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
// repository, where it is archived — code.repos for a repository it declares,
// a person's or a seat's tool for one a tool made — and what it still serves.
// ONE SPELLING for the two places that refuse it (ADR-0008) — the push decide,
// from the rows, and the code host, at a push's advertisement or its request,
// where a person's git prints it as the reason the push was refused.
func ArchivedDetail(name string, managedBy ManagedBy) string {
	if managedBy == ManagedTools {
		return fmt.Sprintf("repository %s is archived and read-only: it is fetched, and takes no push "+
			"until it is unarchived through the code tools", name)
	}
	return fmt.Sprintf("repository %s is archived and read-only (code.repos archived: true): "+
		"it is fetched, and takes no push", name)
}

// BehindDetail is the sentence of a push refused compaction_behind: which
// repository, how many live packs it holds, the most a push may take it to
// ([MaxPacksPerRepo]), and that nothing was decided. ONE SPELLING for the two
// places that refuse it (ADR-0008) — the push decide, from the rows and the
// packs of its batch's earlier entries, and the code host before it uploads a
// push's bundle, from the row it read again.
//
// It names the count the repository holds beside the bound rather than
// calling the count the most it may hold, because a repository can hold more
// than this build's bound — a later build may raise it, and a fleet in the
// middle of its upgrade hands the repository back to a committer of this one
// — and "the most it may hold" beside a larger count says what is not so. It
// asks for the push again in a minute rather than at once, as busy does not:
// only a repository whose pushes outran its compaction meets the bound, and a
// retry within seconds meets it again — uploading nothing, but costing the
// receiver its read of the row and the client its whole pack's send.
func BehindDetail(name string, held int64) string {
	return fmt.Sprintf("%s holds %d live packs, and a push adds none past %d until compaction catches up: "+
		"nothing was decided; run git push again in a minute", name, held, MaxPacksPerRepo)
}

// ExpungingDetail is the sentence of a push refused expunging: which
// repository, the drop epoch its receiver checked it at and the one the
// repository is at, and that nothing was decided. Formed by the push decide
// alone: the epoch a receiver carries is its own copy's, so it has nothing to
// compare it with.
//
// It asks for the push again at once, as busy does and compaction_behind does
// not: the next receive syncs its copy to the repository as the begin left it,
// and a second begin waits for the first expunge's commit, so a retry meets
// the refusal again only past another whole expunge. A push checked at an
// epoch PAST the repository's is no expunge begun since — the rows it was
// checked against are not the ones deciding it — and is said as it is.
func ExpungingDetail(name string, checked, holds int64) string {
	if checked < holds {
		return fmt.Sprintf("an expunge of %s's unreachable objects began after this push was checked "+
			"against it (drop epoch %d, now %d): nothing was decided; run git push again", name, checked, holds)
	}
	return fmt.Sprintf("this push was checked against %s at drop epoch %d, and it is at %d: nothing was "+
		"decided; run git push again", name, checked, holds)
}

// tooManyChangesDetail is the sentence of a push refused too_many_changes: how
// many live changes it moves, the rows a record of its own would hold with
// their rows beside its own, and the bound. Formed by the push decide alone,
// the one counter of a record's change rows.
func tooManyChangesDetail(changes, rows int) string {
	return fmt.Sprintf("this push moves %d live changes, and their rows beside its own would take its record "+
		"to %d of the %d rows one record may write: nothing was decided; push their branches in fewer pushes",
		changes, rows, MaxRecordRows)
}

// StatelessRefusal is the rules of the push decide that no state decides —
// or "" when none fires, checked in this order:
//
//  1. a ref [ValidRef] refuses is bad_ref — before anything else, because
//     no other rule can reason about something that is not a ref;
//  2. a ref outside refs/heads/ and refs/tags/, one under refs/crewlet/, and
//     refs/heads/seats or refs/heads/seats/<one component> as a branch, is
//     reserved_ref ([reservedRef]);
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

// reservedRef reports a ref no principal pushes: one outside refs/heads/ and
// refs/tags/, one under the engine's own refs/crewlet/, and the two kinds of
// ref under refs/heads/seats that are nobody's ([reservedSeatRef]: a branch
// there beside a directory of its name would block seats' namespaces, which
// ref_conflict refuses) — so every judge of a push, the decide, the receiver
// and every engine-built write, refuses them before any grant is read, a
// person's push included.
func reservedRef(ref string) bool {
	return strings.HasPrefix(ref, reservedPrefix) ||
		(!strings.HasPrefix(ref, branchPrefix) && !strings.HasPrefix(ref, tagPrefix)) ||
		reservedSeatRef(ref)
}

// Protections is a repository's protection rules with each pattern PARSED
// ONCE ([ParseProtections]) — the form [ProtectionRefusal] asks, because the
// push decide asks every rule about every command of a batch on the
// repository's one committer (once a batch, before its publish and outside
// every snapshot, and again only inside a round whose rules differ —
// decide.go), and the receiver asks every rule about every command of a
// push. Parsed per question, a maximal batch's round re-read each pattern and
// re-checked each ref for every (command, rule) pair — 192 000 of them at
// three thousand commands and [MaxProtectionsPerRepo] rules, most of the
// round's cost (doc.go's measured section).
//
// The zero Protections holds no rule.
type Protections struct {
	rules    []PushRule
	patterns []gitref.Pattern
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
			return Protections{}, fmt.Errorf("code: protection %q is not a protection pattern "+
				"this build reads, so no push to its repository can be judged against it", rule.Pattern)
		}
		out.patterns[i] = p
	}
	return out, nil
}

// ProtectionRefusal is the protection rule of the push decide over EVERY rule
// whose pattern matches the command's ref, for a push made by a principal of
// kind by — the refusal and the rule that refuses it, the first in the rules'
// order to refuse it for that reason; "" and the zero Protection when none
// fires. Every matching rule is consulted, because two rules written for two
// reasons both hold: any rule with direct_push none, and for anybody but a
// person any rule with direct_push persons, refuses protected_direct; a
// deletion any rule does not allow is protected_delete; a force-push any rule
// does not allow is protected_force — in that order of precedence.
//
// DIRECT_PUSH PERSONS IS A PERSON'S AND NOBODY ELSE'S. Its own words are "a
// person may push to it", and once seats push it must refuse a seat, or
// `persons` would mean what a ref no rule matches already means — anyone who
// may push. So every protection rule refuses every seat's direct push (unset
// reads persons, and none refuses everybody): a seat's work reaches a
// protected ref when a person pushes it. by is an operator for a person's
// push — a credential bound to nobody is refused by its grant before any
// protection is asked — so only a person passes it.
//
// ONE WALK, answering the rule with the refusal. The sentence a refusal
// carries names that rule, and finding it with a second walk over the rules
// asked every rule before it about the ref again: twice the cost of the
// costliest round a repository's rules can make (3.4 s against 1.8 s,
// doc.go's measured section), every command of it refused. A rule that
// refuses protected_direct ends the walk, since nothing after it outranks it.
//
// c.Ref is a ref: [StatelessRefusal] refuses anything else first, in the
// decide and in the receiver alike, so the ref is not checked again for every
// rule — for anything else the answer means nothing (and is never a panic).
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
// every command that is ok or not yet decided becomes ng atomic, and a noop
// stays one — it asked for what the ref already holds, which no refusal of
// its neighbours makes untrue. A push with no refusal is returned unchanged;
// otherwise the answer is a new slice and cmds is left as it was.
//
// ONE FUNCTION the decide and the receiver both call (ADR-0008): the receiver
// applies it to the refusals it fills in before proposing, so a push it
// refuses in part never uploads a pack for the rest, and the decide applies it
// to what it decided — never a second spelling of the rule.
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
