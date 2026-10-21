package builtin

import (
	"errors"
	"maps"
	"slices"

	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/doclet"
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
	// writer's own words, without the package prefix a Go error opens with
	// ("worklet: invalid: …"), which says where the error came from and
	// nothing about what to change. The function the domain's tools word
	// every refusal with: the domain's own Sentence, or for the artifacts
	// and their datastores this package's [domainSentence] and
	// [dataSentence], which strip the prefix of every package a publish or
	// a write passes through — a Sentence of the domain's own beside them
	// would be a second wording of the same refusal.
	sentence func(error) string

	// objects are the argument names that name one of the domain's objects,
	// as the tools take them.
	objects []string
}

// nativeDomains is every native domain these tools write, keyed by its
// state-log name.
//
// ONE PLACE A DOMAIN IS DECLARED TO THE SURFACES, because the person's write
// surface calls some writers WITHOUT a tool — a purge, a rename, a trash, a
// remark taken down — and answers their errors itself
// (internal/api/operator's ClassifyWrite), and it names the object a failed
// call was about from the call's arguments (its About). Each kept its own list
// of a domain's sentinels and argument names beside the tools' own, so the
// lists drifted: a tracker refusal the surface knew and a tool did not
// (`ErrNotAuthor`, `ErrNoComment`) was a tool's internal_error, and a domain
// added beside the two would have been a 500 for every content refusal its
// tool-less verbs met. A gate holds this table to every domain the estate
// router carries a write for ([TestEveryRoutedDomainIsDeclaredToTheSurfaces]),
// each domain's classes to every sentinel and error type the domain declares
// ([TestEveryDomainRefusalIsClassed]), and its objects to the catalogue's
// tools in both directions: every one is an argument some tool takes, and
// every write takes one of them (builtin_test's
// TestEveryObjectArgumentIsOneAToolTakes and
// TestEveryWriteNamesTheObjectItWrites).
var nativeDomains = map[string]nativeDomain{
	artifactsdomain.Domain{}.Name(): {
		class:    artifactClass,
		sentence: domainSentence,
		// An artifact is named by its `artifact` id on every tool that
		// changes one; create_artifact names none, because the artifact
		// it makes has no id until it lands.
		objects: []string{"artifact"},
	},
	artifactdata.Domain{}.Name(): {
		class:    artifactDataClass,
		sentence: dataSentence,
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
		// A new page has no id to name yet: write_page addresses it by its
		// `container` and its title, so the container is the object a
		// failed create points at. The title is not declared, because a
		// surface names these by argument name across every tool, and on
		// the tracker's tools `title` is a work item's text, not an object.
		objects: []string{"container", "page"},
	},
	chatlet.Domain{}.Name(): {
		class:    chatClass,
		sentence: chatlet.Sentence,
		// A room is named by `channel` — its id or its name, which is
		// its address, a new room's included — and a message by
		// `message_id`, never `message`, which on the knowledge base's
		// tools is a save's revision note rather than an object. A
		// direct conversation IS its participants, so the `to` that
		// names them is the object a failed send_dm or open_dm points at.
		objects: []string{"channel", "message_id", "to"},
	},
}

// WriterClass is the refusal class a native domain writer's error is, and
// false for an error no domain decided: a condition of the node, which
// [Condition] answers, a fault, or an outcome that is not a refusal at all
// ([UnknownCause]).
//
// EXPORTED for the human write surface, whose tool-less writers are classed
// by this one rule (internal/api/operator's ClassifyWrite) — the same
// classes the tools' own failures carry, since [writeFailure],
// [pageWriteFailure], [publishFailure] and [dataFailure] fall back to it, and
// [chatWriteFailure] classes by [chatClass] itself.
func WriterClass(err error) (tools.Refusal, bool) {
	// AN OUTCOME NOBODY CAN VOUCH FOR IS NEVER A REFUSAL, whatever its
	// cause carries beside it: a step whose outcome is unknown, a write no
	// data node answered. Classed by what it wraps, it would tell a caller
	// nothing was written.
	if err == nil || errors.Is(UnknownCause("", err), ErrOutcomeUnknown) {
		return "", false
	}
	for _, name := range slices.Sorted(maps.Keys(nativeDomains)) {
		if class, ok := nativeDomains[name].class(err); ok {
			return class, true
		}
	}
	// THE STATE LOG'S OWN REFUSALS, which every domain's writer returns as
	// the framework wrote them where it has no word of its own: an object
	// that already exists, and a write that lost its compare-and-set rounds.
	switch {
	case errors.Is(err, statelog.ErrExists):
		return tools.RefusalExists, true
	case errors.Is(err, statelog.ErrConflict):
		return tools.RefusalConflict, true
	}
	return "", false
}

// WriterSentence is a native domain writer's refusal in the sentence the
// domain that decided it composed, which its own tools word the same refusal
// with for a model — and false for an error no domain decided: a
// condition, a fault, an outcome nobody can vouch for, or one of the state
// log's own refusals, which [WriterClass] classes and which keeps the words
// the framework wrote it in, as the tools keep them.
//
// A MODEL'S WORDS, and the human write surface's tool-less verbs do not
// answer them: a purge's or a rename's refusal is told to a person in the
// words its writer put beside it, or in its code's own sentence, never in a
// tool's or the error's own, which opens with the package it came from
// (internal/api/operator's FailWrite, ADR-0060). EXPORTED for that surface's
// suite, which holds its answers clear of these words.
func WriterSentence(err error) (string, bool) {
	if err == nil || errors.Is(UnknownCause("", err), ErrOutcomeUnknown) {
		return "", false
	}
	for _, name := range slices.Sorted(maps.Keys(nativeDomains)) {
		d := nativeDomains[name]
		if _, ok := d.class(err); ok {
			return d.sentence(err), true
		}
	}
	return "", false
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

// trackerClass is the class of a refusal the tracker's writer decided.
//
// AS THE TOOLS CLASS IT where a tool decides one alone, by the one rule
// (internal/mcp's [tools.Refusal]): what the item or project the write is
// about refuses by its STATE — frozen in the trash, archived, its tags or pins
// at their cap, nothing in the trash to restore, an upload too old to name —
// is a conflict, as an artifact's trash is; a type the project does not file,
// or a parent in the trash, is the argument to change.
func trackerClass(err error) (tools.Refusal, bool) {
	var (
		ambiguous       *worklet.ErrAmbiguous
		ambiguousAnswer *worklet.ErrAmbiguousAnswer
	)
	switch {
	case errors.Is(err, worklet.ErrNoTask), errors.Is(err, worklet.ErrNoComment),
		errors.Is(err, worklet.ErrNoProject), errors.Is(err, worklet.ErrNoFile):
		return tools.RefusalNotFound, true
	case errors.Is(err, worklet.ErrStaleVersion):
		return tools.RefusalStaleVersion, true
	case errors.Is(err, worklet.ErrForbidden), errors.Is(err, worklet.ErrNotAuthor):
		return tools.RefusalForbidden, true
	case errors.Is(err, worklet.ErrAlreadyAnswered):
		return tools.RefusalAlreadyAnswered, true
	case errors.Is(err, worklet.ErrInboxFull):
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

// artifactClass is the class of a refusal the artifacts' writer decided.
//
// AS THE TOOLS CLASS IT ([publishFailure]) AND AS THE ARTIFACTS API ANSWERS IT
// (internal/api/artifactsapi's writeFailed): one class per refusal, whose
// status is the API's, which internal/api/artifactsapi's cross-surface case
// holds. An artifact in the trash, one whose purge has begun and a label
// another version holds (or the labels it keeps, all used) are each the
// ARTIFACT'S STATE, which the gesture collided with — a conflict, as a
// tracker restore with nothing in the trash is, and never a permission
// (`forbidden` told a model to go and ask for one) or an argument (`invalid`
// told it to send another, which a label somebody holds is only by luck). A
// quota is what the company holds at its plan's limit: `quota_exceeded`,
// which nobody's permission lifts. An upload past its term, or carried bytes
// the artifact no longer names, is the same call made again.
func artifactClass(err error) (tools.Refusal, bool) {
	var (
		moved *artifactsdomain.VersionMoved
		gone  *artifactsdomain.CarriedObjectGone
		quota *artifactsdomain.QuotaExceeded
	)
	switch {
	case errors.Is(err, artifactsdomain.ErrNotFound), errors.Is(err, artifactsdomain.ErrPurged):
		return tools.RefusalNotFound, true
	case errors.As(err, &moved):
		return tools.RefusalStaleVersion, true
	case errors.Is(err, artifactsdomain.ErrSourceTaken):
		return tools.RefusalExists, true
	case errors.As(err, &quota):
		return tools.RefusalQuotaExceeded, true
	case errors.Is(err, artifactsdomain.ErrAgentCompanyShare),
		errors.Is(err, artifactsdomain.ErrAboveOwnRole):
		return tools.RefusalForbidden, true
	case errors.Is(err, artifactsdomain.ErrConflict), errors.Is(err, artifactsdomain.ErrPurging),
		errors.Is(err, artifactsdomain.ErrTrashed), errors.Is(err, artifactsdomain.ErrLabelTaken),
		errors.Is(err, artifactsdomain.ErrUploadStale), errors.As(err, &gone):
		return tools.RefusalConflict, true
	case errors.Is(err, artifactsdomain.ErrInvalid), errors.Is(err, artifactsdomain.ErrBadCursor):
		return tools.RefusalInvalid, true
	}
	return "", false
}

// DataVerdictClass is the class of a datastore write the APPLIER refused, by
// the verdict it wrote — the class whose status internal/api/artifactdataapi
// answers the same verdict in, which that package's cross-surface case holds,
// and the reason this is exported. A quota is `quota_exceeded`, never `forbidden`: no permission
// lifts it. A verdict this build does not know is a newer peer's decision
// about a record that landed, which this build cannot describe: a conflict,
// as the API's 409 says, rather than an argument the caller should change.
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

// artifactDataClass is the class of a refusal a datastore's writer decided.
//
// AS THE TOOLS CLASS IT ([dataFailure]): a purged datastore is not there, and
// a write past this node's budget is answered once the budget refills. An undo
// of a write that cannot be undone is the history's state rather than the
// argument, as the API's 409 says.
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
