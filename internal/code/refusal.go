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
// the last five are answers ABOUT a push, never part of one.
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
	// repository does not cover ([Grant.RefRule], ADR-0040).
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
	// RefusalUnavailable is the code log or the object store refusing.
	RefusalUnavailable Refusal = "unavailable"
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
		RefusalBusy, RefusalUnavailable, RefusalNotApplied, RefusalUnknown,
		RefusalPending,
	}
}

// Valid reports a code this build knows.
func (r Refusal) Valid() bool { return slices.Contains(Refusals(), r) }

// Recorded reports a code that may appear on a recorded command: every one but
// the five that are answers ABOUT a push — whether it could be committed at
// all — rather than a verdict on one of its commands.
func (r Refusal) Recorded() bool {
	switch r {
	case RefusalBusy, RefusalUnavailable, RefusalNotApplied, RefusalUnknown, RefusalPending:
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
	RefusalProtectedDirect: "a protection on this ref takes no direct push (direct_push: none)",
	RefusalTagMoved:        "a tag does not move: delete it and push it again",
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
	RefusalBlobTooLarge:  "a blob is larger than code.limits.max_blob_bytes; keep large binaries in tracker files",
	RefusalPackTooLarge: "the pack this push stores — completed against the repository and " +
		"indexed — is larger than one push may store; push fewer commits at once",
	RefusalBusy:        "the repository is taking pushes faster than they are committed; run git push again",
	RefusalUnavailable: "the code log or the object store refused the write; run git push again",
	RefusalNotApplied:  "the push was durable and applied nowhere; run git push again",
	RefusalUnknown:     "the push's outcome is unknown; fetch, then run git push again",
	RefusalPending:     "the push is durable and unresolved here; run git push again (a push that landed reports up to date)",
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
}

// Detail is the sentence of a push refused repo_deleted for g: what became of
// the repository and what, if anything, brings it back.
//
// A deleted repository's sentence names the instant its purge may begin — the
// deletion plus the deciding node's retention: the earliest the code-retention
// duty begins it under that epoch, which runs on a tick and may begin it
// later. Named as when the purge MAY BEGIN, never as "restorable until", which
// would promise an instant nothing holds the purge to.
func (g RepoGone) Detail() string {
	named := cmp.Or(g.Name, g.ID)
	switch {
	case g.Status == RepoStatusDeleted && !g.DeletedAt.IsZero() && g.Retention > 0:
		return fmt.Sprintf("repository %s is deleted; declaring it again in code.repos restores it "+
			"as it was, until its purge, which may begin from %s (code.deleted_retention_days after "+
			"its deletion)", named, g.DeletedAt.Add(g.Retention).UTC().Format(time.RFC3339))
	case g.Status == RepoStatusDeleted:
		return fmt.Sprintf("repository %s is deleted; declaring it again in code.repos restores it "+
			"as it was, until its purge", named)
	case g.Status == RepoStatusPurging:
		return fmt.Sprintf("repository %s is being purged, which cannot be undone; once the purge "+
			"completes, declaring its name in code.repos creates a new, empty repository", named)
	case g.Purged:
		return fmt.Sprintf("repository %s was purged and holds nothing; declaring its name in "+
			"code.repos creates a new, empty repository", named)
	}
	return fmt.Sprintf("repository %s does not exist", g.ID)
}

// ArchivedDetail is the sentence of a push refused repo_archived: which
// repository, where it is archived, and what it still serves. ONE SPELLING
// for the two places that refuse it (ADR-0008) — the push decide, from the
// rows, and the code host, at a push's advertisement or its request, where a
// person's git prints it as the reason the push was refused.
func ArchivedDetail(name string) string {
	return fmt.Sprintf("repository %s is archived and read-only (code.repos archived: true): "+
		"it is fetched, and takes no push", name)
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
	rules    []Protection
	patterns []gitref.Pattern
}

// ParseProtections reads rules for asking about many refs, refusing — rather
// than reading as a rule that protects nothing — one whose pattern is not a
// protection pattern: every stored rule passed the configuration's validation
// and [RepoSettings.Validate] before any record carried it, so one that does
// not parse is an estate that holds what no writer of this build wrote, and a
// protection read as absent is a protected ref open to every push.
func ParseProtections(rules []Protection) (Protections, error) {
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
// whose pattern matches the command's ref — the refusal and the rule that
// refuses it, the first in the rules' order to refuse it for that reason; ""
// and the zero Protection when none fires. Every matching rule is consulted,
// because two rules written for two reasons both hold: any rule with
// direct_push none refuses protected_direct; a deletion any rule does not
// allow is protected_delete; a force-push any rule does not allow is
// protected_force — in that order of precedence.
//
// ONE WALK, answering the rule with the refusal. The sentence a refusal
// carries names that rule, and finding it with a second walk over the rules
// asked every rule before it about the ref again: twice the cost of the
// costliest round a repository's rules can make (3.4 s against 1.8 s,
// doc.go's measured section), every command of it refused. A rule with
// direct_push none ends the walk, since nothing after it outranks it.
//
// c.Ref is a ref: [StatelessRefusal] refuses anything else first, in the
// decide and in the receiver alike, so the ref is not checked again for every
// rule — for anything else the answer means nothing (and is never a panic).
func ProtectionRefusal(c Command, rules Protections) (Refusal, Protection) {
	deletion, force := -1, -1
	for i, p := range rules.patterns {
		if !p.Match(c.Ref) {
			continue
		}
		rule := rules.rules[i]
		if rule.DirectPush == DirectNone {
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
	return "", Protection{}
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
