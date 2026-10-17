package builtin

import (
	"errors"
	"maps"
	"slices"

	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
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
		class: artifactClass,
		// An artifact is named by its `artifact` id on every tool that
		// changes one; create_artifact names none, because the artifact
		// it makes has no id until it lands.
		objects: []string{"artifact"},
	},
	artifactdata.Domain{}.Name(): {
		class: artifactDataClass,
		// A datastore is its artifact's, named by the same `artifact`.
		objects: []string{"artifact"},
	},
	worklet.Domain{}.Name(): {
		class: trackerClass,
		// A person's record is the tracker's, kept under the name a
		// `handle` resolves to.
		objects: []string{"item", "project", "handle"},
	},
	doclet.Domain{}.Name(): {
		class: pageClass,
		// A new page has no id to name yet: write_page addresses it by its
		// `container` and its title, so the container is the object a
		// failed create points at. The title is not declared, because a
		// surface names these by argument name across every tool, and on
		// the tracker's tools `title` is a work item's text, not an object.
		objects: []string{"container", "page"},
	},
}

// WriterClass is the refusal class a native domain writer's error is, and
// false for an error no domain decided: a condition of the node, which
// [Condition] answers, a fault, or an outcome that is not a refusal at all
// ([UnknownCause]).
//
// EXPORTED for the human write surface, whose tool-less writers are classed
// by this one rule (internal/api/operator's ClassifyWrite) — the same
// classes the tools' own failures carry, since [writeFailure] and
// [pageWriteFailure] fall back to it.
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
// AS THE TOOLS CLASS IT where a tool decides one alone: a stale upload is the
// same call again, a restore with nothing in the trash is the item's state
// rather than the argument, a type the project does not file is the argument
// to change.
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
	case errors.Is(err, worklet.ErrNothingToRestore):
		return tools.RefusalConflict, true
	case errors.Is(err, worklet.ErrUploadStale), errors.Is(err, worklet.ErrIndexBuilding):
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
		errors.Is(err, doclet.ErrBadCursor):
		return tools.RefusalInvalid, true
	}
	return "", false
}

// artifactClass is the class of a refusal the artifacts' writer decided.
//
// AS THE TOOLS CLASS IT ([publishFailure]): an artifact in the trash refuses a
// change until a person restores it, a label another version holds is an
// argument to change, and an upload past its term — or carried bytes the
// artifact no longer names — is the same call made again. A write refused
// because the artifact's purge has begun is the artifact's state, as a tracker
// restore with nothing in the trash is.
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
	case errors.Is(err, artifactsdomain.ErrTrashed), errors.Is(err, artifactsdomain.ErrAgentCompanyShare),
		errors.Is(err, artifactsdomain.ErrAboveOwnRole), errors.As(err, &quota):
		return tools.RefusalForbidden, true
	case errors.Is(err, artifactsdomain.ErrConflict), errors.Is(err, artifactsdomain.ErrPurging),
		errors.Is(err, artifactsdomain.ErrUploadStale), errors.As(err, &gone):
		return tools.RefusalConflict, true
	case errors.Is(err, artifactsdomain.ErrInvalid), errors.Is(err, artifactsdomain.ErrLabelTaken),
		errors.Is(err, artifactsdomain.ErrBadCursor):
		return tools.RefusalInvalid, true
	}
	return "", false
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
