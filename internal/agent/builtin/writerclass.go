package builtin

import (
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/artifactupload"
	"github.com/crewlet/crewlet/internal/atlasdomain"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/inbox"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/worklet"
)

// nativeDomain is what a surface outside a tool needs to know about one native
// domain these tools write: the class a refusal its writer decided is, and the
// arguments that name one of its objects.
type nativeDomain struct {
	// class is the refusal class a writer's error is, and false for one
	// this domain did not decide — a condition of the node, a fault, an
	// outcome that is not a refusal at all.
	class func(error) (tools.Refusal, bool)

	// sentence is a refusal this domain decided as its tools word it: the
	// writer's words without a Go error's package prefix ("worklet: invalid:
	// …"), which says nothing about what to change. It is the domain's own
	// Sentence; for the artifacts the publish pipeline's
	// (internal/artifactupload), since every artifact refusal passes through
	// it carrying every package's prefix on the way.
	sentence func(error) string

	// objects are the argument names that name one of the domain's objects,
	// as the tools take them.
	objects []string
}

// nativeDomains is every native domain these tools write, keyed by its
// state-log name: ONE PLACE A DOMAIN IS DECLARED TO THE SURFACES, since the
// person's write surface calls some writers without a tool (a purge, a rename,
// a trash), classes their errors (internal/api/operator's ClassifyWrite) and
// names a failed call's object from its arguments (About); separate lists
// drifted. Gates: [TestEveryRoutedDomainIsDeclaredToTheSurfaces]
// (every domain the estate router routes; not the executor log, the engine's
// own record), [TestEveryDomainRefusalIsClassed] (every sentinel and error
// type), and builtin_test's TestEveryObjectArgumentIsOneAToolTakes and
// TestEveryWriteNamesTheObjectItWrites (objects against the tools, both ways).
var nativeDomains = map[string]nativeDomain{
	artifactsdomain.Domain{}.Name(): {
		class:    artifactClass,
		sentence: artifactupload.Sentence,
		// `artifact` on every tool that changes one (create_artifact names
		// none: it has no id until it lands); `artifacts` is move_artifact's
		// list; `parent` is the folder create_artifact_folder makes in, where
		// to look for what it made, as write_page's container is.
		objects: []string{"artifact", "artifacts", "parent"},
	},
	artifactdata.Domain{}.Name(): {
		class:    artifactDataClass,
		sentence: artifactdata.Sentence,
		// A datastore is its artifact's, named by the same `artifact`.
		objects: []string{"artifact"},
	},
	worklet.Domain{}.Name(): {
		class:    trackerClass,
		sentence: worklet.Sentence,
		// A person's record is the tracker's, kept under the name a
		// `handle` resolves to.
		objects: []string{"item", "project", "handle"},
	},
	doclet.Domain{}.Name(): {
		class:    pageClass,
		sentence: doclet.Sentence,
		// A new page has no id yet: write_page addresses it by `container`
		// and title, so the container is what a failed create points at.
		// `title` is not declared: surfaces match argument names across
		// every tool, and the tracker's `title` is text, not an object.
		objects: []string{"container", "page"},
	},
	chatlet.Domain{}.Name(): {
		class:    chatClass,
		sentence: chatlet.Sentence,
		// `channel` is a room's id or name (its address, a new room's
		// included); `message_id` a message, never `message`, the knowledge
		// base's revision note; `to`, since a direct conversation IS its
		// participants.
		objects: []string{"channel", "message_id", "to"},
	},
	atlasdomain.Domain{}.Name(): {
		class:    atlasClass,
		sentence: atlasdomain.Sentence,
		// `atlas_entry` by id or kind and key, never `entry`, which on the
		// artifacts' tools is a file. A purged value is never named: that
		// would repeat what the purge erases.
		objects: []string{"atlas_entry"},
	},
	gitlet.Domain{}.Name(): {
		class:    codeClass,
		sentence: gitlet.Sentence,
		// `repo` is a repository's name, which a rename redirects; `change`
		// its number within it. code_create_repo's `name` is not declared:
		// on other domains' tools it is text, not an object (objectargs_test
		// names why).
		objects: []string{"repo", "change"},
	},
}

// WriterClass is the refusal class a native domain writer's error is, and
// false for an error no domain decided: a condition of the node ([Condition]),
// a fault, or an outcome that is not a refusal ([UnknownCause]).
//
// EXPORTED for the human write surface's tool-less writers
// (internal/api/operator's ClassifyWrite), so they carry the tools' classes:
// [writeFailure], [pageWriteFailure], [publishFailure] and [dataFailure] fall
// back to it, and [chatWriteFailure] classes by [chatClass] itself.
func WriterClass(err error) (tools.Refusal, bool) {
	// AN OUTCOME NOBODY CAN VOUCH FOR IS NEVER A REFUSAL, whatever it wraps:
	// classed by its cause, it would tell a caller nothing was written.
	if err == nil || errors.Is(UnknownCause("", err), ErrOutcomeUnknown) {
		return "", false
	}
	for _, name := range slices.Sorted(maps.Keys(nativeDomains)) {
		if class, ok := nativeDomains[name].class(err); ok {
			return class, true
		}
	}
	// THE STATE LOG'S OWN REFUSALS, which a writer returns where it has no
	// word of its own: the three [statelog.Sentence] tells a person, each in
	// the class its words state. THE DELETION IS NOT FOUND, though stated as
	// [statelog.ErrUnavailable]: classed as the [Condition] that says, it
	// tells a model "try again shortly" about what nothing will ever write
	// again, on any node. Tools word it via [domainRefusal]; the person's
	// write surface answers it `404`.
	var refused *statelog.Unavailable
	switch {
	case errors.Is(err, statelog.ErrExists):
		return tools.RefusalExists, true
	case errors.Is(err, statelog.ErrConflict):
		return tools.RefusalConflict, true
	case errors.As(err, &refused) && refused.Reason == statelog.ReasonDeleted:
		return tools.RefusalNotFound, true
	}
	return "", false
}

// WriterSentence is a native domain writer's refusal in the sentence the
// deciding domain composed for its tools, and false for an error no domain
// decided: a condition, a fault, or an outcome nobody can vouch for.
//
// A lost race, a create of what is there and a write about what a deletion
// keeps deleted take [statelog.Sentence]'s words for a person on every domain:
// the framework's own text names a broker subject nobody can act on. The
// deletion, no domain's decision, is a fact about the object, answered `404`.
//
// These are A MODEL'S WORDS, never a person's (internal/api/operator's
// FailWrite, ADR-0060): EXPORTED for that surface's suite, which holds its
// answers clear of them.
func WriterSentence(err error) (string, bool) {
	if err == nil || errors.Is(UnknownCause("", err), ErrOutcomeUnknown) {
		return "", false
	}
	if said, ok := statelog.Sentence(err); ok {
		return said, true
	}
	for _, name := range slices.Sorted(maps.Keys(nativeDomains)) {
		d := nativeDomains[name]
		if _, ok := d.class(err); ok {
			return d.sentence(err), true
		}
	}
	return "", false
}

// domainRefusal is a native domain's refusal as a tool's fallback arm words
// it: the framework's own ([WriterClass]'s state-log arm) whole in
// [statelog.Sentence]'s words, and any other as "<tool> was refused:
// <sentence>." in the DECIDING domain's words ([WriterSentence]), since a
// fallback meets other domains' refusals, which its own domain's Sentence
// would word with their package prefixes.
func domainRefusal(tool string, err error) string {
	if said, ok := statelog.Sentence(err); ok {
		return said
	}
	said, _ := WriterSentence(err)
	return fmt.Sprintf("%s was refused: %s.", tool, said)
}

// ObjectArgs are the argument names that name an object of a native domain,
// sorted: what a surface answering a failed call reads to say which object the
// call was about (internal/api/operator's About).
func ObjectArgs() []string {
	var out []string
	for _, d := range nativeDomains {
		out = append(out, d.objects...)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// trackerClass is the class of a refusal the tracker's writer decided, by the
// one rule (internal/mcp's [tools.Refusal]): a refusal by the item's or
// project's STATE (trashed, archived, tags or pins at their cap, nothing to
// restore, an upload too old) is `conflict`; a label another tag carries is
// `exists`; a type the project does not file, or a parent in the trash, is
// the argument to change.
func trackerClass(err error) (tools.Refusal, bool) {
	var (
		ambiguous       *worklet.ErrAmbiguous
		ambiguousAnswer *worklet.ErrAmbiguousAnswer
	)
	switch {
	case errors.Is(err, worklet.ErrNoTask), errors.Is(err, worklet.ErrNoComment),
		errors.Is(err, worklet.ErrNoProject), errors.Is(err, worklet.ErrNoFile),
		errors.Is(err, worklet.ErrNoView):
		return tools.RefusalNotFound, true
	case errors.Is(err, worklet.ErrStaleVersion):
		return tools.RefusalStaleVersion, true
	case errors.Is(err, worklet.ErrForbidden), errors.Is(err, worklet.ErrNotAuthor):
		return tools.RefusalForbidden, true
	case errors.Is(err, worklet.ErrAlreadyAnswered):
		return tools.RefusalAlreadyAnswered, true
	case errors.Is(err, worklet.ErrTaken):
		return tools.RefusalExists, true
	case errors.Is(err, inbox.ErrFull):
		return tools.RefusalInboxFull, true
	case errors.Is(err, worklet.ErrReassignmentBudget):
		return tools.RefusalReassignmentBudget, true
	case errors.Is(err, worklet.ErrNothingToRestore), errors.Is(err, worklet.ErrState),
		errors.Is(err, worklet.ErrUploadStale):
		return tools.RefusalConflict, true
	case errors.Is(err, worklet.ErrIndexBuilding):
		return tools.RefusalUnavailable, true
	case errors.Is(err, worklet.ErrInvalid), errors.Is(err, worklet.ErrBadQuery),
		errors.Is(err, worklet.ErrTooBroad), errors.Is(err, worklet.ErrNoType),
		errors.Is(err, worklet.ErrReparentAcrossProjects),
		errors.As(err, &ambiguous), errors.As(err, &ambiguousAnswer):
		return tools.RefusalInvalid, true
	}
	return "", false
}

// pageClass is the class of a refusal the knowledge base's writer decided.
func pageClass(err error) (tools.Refusal, bool) {
	switch {
	case errors.Is(err, doclet.ErrNotFound):
		return tools.RefusalNotFound, true
	case errors.Is(err, doclet.ErrStaleVersion):
		return tools.RefusalStaleVersion, true
	case errors.Is(err, doclet.ErrTitleTaken):
		return tools.RefusalExists, true
	case errors.Is(err, doclet.ErrConflict):
		return tools.RefusalConflict, true
	case errors.Is(err, doclet.ErrReserved):
		return tools.RefusalForbidden, true
	case errors.Is(err, doclet.ErrInvalid), errors.Is(err, doclet.ErrParent),
		errors.Is(err, doclet.ErrBadCursor), errors.Is(err, doclet.ErrUnservedOrder):
		return tools.RefusalInvalid, true
	}
	return "", false
}

// artifactClass is the class of a refusal the artifacts' writer decided, one
// class per refusal as [publishFailure] and internal/api/artifactsapi's
// writeFailed answer it (that package's cross-surface case holds them). The
// STATE of an artifact, folder or comment (trashed, purging, labels, folders or
// comments at their bound, an expired upload, carried bytes no longer named)
// is `conflict`, never `forbidden`, which sends a model to ask for permission.
// A label, source address or folder name another holds is `exists`: it lands
// once freed. A plan's quota is `quota_exceeded`, which no permission lifts. A
// missing artifact, folder, thread or remark is `not_found`; somebody else's
// remark `forbidden`; a credential in a comment, or a folder destination
// inside itself, too deep or ambiguous, `invalid`.
func artifactClass(err error) (tools.Refusal, bool) {
	var (
		moved     *artifactsdomain.VersionMoved
		gone      *artifactsdomain.CarriedObjectGone
		quota     *artifactsdomain.QuotaExceeded
		ambiguous *artifactsdomain.FolderAmbiguous
	)
	switch {
	case errors.Is(err, artifactsdomain.ErrNotFound), errors.Is(err, artifactsdomain.ErrPurged),
		errors.Is(err, artifactsdomain.ErrFolderNotFound), errors.Is(err, artifactsdomain.ErrCommentNotFound):
		return tools.RefusalNotFound, true
	case errors.As(err, &moved):
		return tools.RefusalStaleVersion, true
	case errors.Is(err, artifactsdomain.ErrSourceTaken), errors.Is(err, artifactsdomain.ErrLabelTaken),
		errors.Is(err, artifactsdomain.ErrExists), errors.Is(err, artifactsdomain.ErrFolderNameTaken):
		return tools.RefusalExists, true
	case errors.As(err, &quota):
		return tools.RefusalQuotaExceeded, true
	case errors.Is(err, artifactsdomain.ErrAgentCompanyShare),
		errors.Is(err, artifactsdomain.ErrAboveOwnRole), errors.Is(err, artifactsdomain.ErrNotTheAuthor):
		return tools.RefusalForbidden, true
	case errors.Is(err, artifactsdomain.ErrConflict), errors.Is(err, artifactsdomain.ErrPurging),
		errors.Is(err, artifactsdomain.ErrTrashed), errors.Is(err, artifactsdomain.ErrLabelsFull),
		errors.Is(err, artifactsdomain.ErrUploadStale), errors.As(err, &gone),
		errors.Is(err, artifactsdomain.ErrFolderLimit), errors.Is(err, artifactsdomain.ErrCommentLimit):
		return tools.RefusalConflict, true
	case errors.Is(err, artifactsdomain.ErrInvalid), errors.Is(err, artifactsdomain.ErrBadCursor),
		errors.Is(err, artifactsdomain.ErrCommentCredential),
		errors.Is(err, artifactsdomain.ErrFolderCycle), errors.Is(err, artifactsdomain.ErrFolderDepth),
		errors.As(err, &ambiguous):
		return tools.RefusalInvalid, true
	}
	return "", false
}

// DataVerdictClass is the class of a datastore write the APPLIER refused, by
// its verdict: exported because internal/api/artifactdataapi answers the same
// verdict in its status, which that package's cross-surface case holds. A
// quota is `quota_exceeded`, never `forbidden`: no permission lifts it. An
// unknown verdict is a newer peer's about a record that landed: a conflict, as
// the API's 409 says, not an argument to change.
func DataVerdictClass(v artifacts.DataVerdict) tools.Refusal {
	switch v {
	case artifacts.DataInvalid:
		return tools.RefusalInvalid
	case artifacts.DataForbidden:
		return tools.RefusalForbidden
	case artifacts.DataQuota:
		return tools.RefusalQuotaExceeded
	case artifacts.DataGone:
		return tools.RefusalNotFound
	}
	return tools.RefusalConflict
}

// artifactDataClass is the class of a refusal a datastore's writer decided, as
// [dataFailure] classes it: a purged datastore is not there, a write past this
// node's budget is answered once it refills, and an undo that cannot be done
// is the history's state, as the API's 409 says.
func artifactDataClass(err error) (tools.Refusal, bool) {
	var limited *artifactdata.RateLimited
	switch {
	case errors.Is(err, artifactdata.ErrNotFound), errors.Is(err, artifactdata.ErrGone):
		return tools.RefusalNotFound, true
	case errors.Is(err, artifactdata.ErrForbidden):
		return tools.RefusalForbidden, true
	case errors.Is(err, artifactdata.ErrConflict), errors.Is(err, artifactdata.ErrNotUndoable):
		return tools.RefusalConflict, true
	case errors.As(err, &limited):
		return tools.RefusalUnavailable, true
	case errors.Is(err, artifactdata.ErrInvalid), errors.Is(err, artifactdata.ErrBadCursor),
		errors.Is(err, artifactdata.ErrMigrationTooLarge):
		return tools.RefusalInvalid, true
	}
	return "", false
}
