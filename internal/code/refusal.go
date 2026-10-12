package code

import (
	"slices"
	"strings"
)

// Refusal is why a command was refused, or why a push was not committed — a
// code a client sees as "<code>: <sentence>". A named string, so a code this
// build does not know is a value rather than a panic.
type Refusal string

// The codes. The first fifteen may be RECORDED on a command ([Refusal.Recorded]);
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
	// RefusalReservedRef is a ref outside refs/heads/ and refs/tags/, or
	// under the engine's own refs/crewlet/.
	RefusalReservedRef Refusal = "reserved_ref"
	// RefusalBadRef is not a ref.
	RefusalBadRef Refusal = "bad_ref"
	// RefusalRepoArchived is a push to a read-only repository.
	RefusalRepoArchived Refusal = "repo_archived"
	// RefusalRepoDeleted is a push to a deleted repository.
	RefusalRepoDeleted Refusal = "repo_deleted"
	// RefusalRepoSize is a push past code.limits.repo_max_bytes.
	RefusalRepoSize Refusal = "repo_size"
	// RefusalRepoRefs is a create past MaxRefsPerRepo.
	RefusalRepoRefs Refusal = "repo_refs"
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
		RefusalProtectedDirect, RefusalTagMoved, RefusalReservedRef, RefusalBadRef,
		RefusalRepoArchived, RefusalRepoDeleted, RefusalRepoSize, RefusalRepoRefs,
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
	RefusalReservedRef:     "only refs/heads/ and refs/tags/ are pushed; refs/crewlet/ is the engine's",
	RefusalBadRef:          "not a ref: git's check-ref-format rules, at most 255 bytes of UTF-8 under refs/<namespace>/",
	RefusalRepoArchived:    "the repository is archived and read-only",
	RefusalRepoDeleted:     "the repository is deleted; declaring it again restores it until its purge",
	RefusalRepoSize:        "the repository would hold more than code.limits.repo_max_bytes",
	RefusalRepoRefs:        "the repository holds as many refs as one repository may",
	RefusalUploadExpired:   "the pack was uploaded too long ago to be committed; run git push again",
	RefusalAtomic:          "refused with the rest of this --atomic push",
	RefusalBlobTooLarge:    "a blob is larger than code.limits.max_blob_bytes; keep large binaries in tracker files",
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

// StatelessRefusal is the rules of the push decide that no state decides —
// or "" when none fires, checked in this order:
//
//  1. a ref [ValidRef] refuses is bad_ref — before anything else, because
//     no other rule can reason about something that is not a ref;
//  2. a ref outside refs/heads/ and refs/tags/, or under refs/crewlet/, is
//     reserved_ref;
//  3. a tag whose old and new values are both commits is tag_moved — a tag
//     may be created and deleted, never moved.
//
// ONE FUNCTION the decide and the receiver both call (ADR-0008): the decide is
// authoritative, and the receiver may refuse early, before it uploads anything.
func StatelessRefusal(c Command) Refusal {
	switch {
	case !ValidRef(c.Ref):
		return RefusalBadRef
	case strings.HasPrefix(c.Ref, reservedPrefix),
		!strings.HasPrefix(c.Ref, branchPrefix) && !strings.HasPrefix(c.Ref, tagPrefix):
		return RefusalReservedRef
	case strings.HasPrefix(c.Ref, tagPrefix) && c.Old != ZeroOID && c.New != ZeroOID:
		return RefusalTagMoved
	}
	return ""
}

// ProtectionRefusal is the protection rule of the push decide over EVERY rule
// whose pattern matches the command's ref — or "" when none fires. Every
// matching rule is consulted, because two rules written for two reasons both
// hold: any rule with direct_push none refuses protected_direct; a deletion any
// rule does not allow is protected_delete; a force-push any rule does not allow
// is protected_force — in that order of precedence.
func ProtectionRefusal(c Command, rules []Protection) Refusal {
	var direct, deletion, force bool
	for _, rule := range rules {
		if !MatchPattern(rule.Pattern, c.Ref) {
			continue
		}
		direct = direct || rule.DirectPush == DirectNone
		deletion = deletion || !rule.AllowDelete
		force = force || !rule.AllowForce
	}
	switch {
	case direct:
		return RefusalProtectedDirect
	case c.New == ZeroOID && deletion:
		return RefusalProtectedDelete
	case c.Forced && force:
		return RefusalProtectedForce
	}
	return ""
}

// The ref namespaces the stateless rules read.
const (
	branchPrefix   = "refs/heads/"
	tagPrefix      = "refs/tags/"
	reservedPrefix = "refs/crewlet/"
)
