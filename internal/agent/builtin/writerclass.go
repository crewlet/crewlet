package builtin

import (
	"errors"
	"maps"
	"slices"

	"github.com/crewlet/crewlet/internal/chat"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
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
	tracker.Domain{}.Name(): {
		class: trackerClass,
		// A person's record is the tracker's, kept under the name a
		// `handle` resolves to.
		objects: []string{"item", "project", "handle"},
	},
	pages.Domain{}.Name(): {
		class: pageClass,
		// A new page has no id to name yet: write_page addresses it by its
		// `container` and its title, so the container is the object a
		// failed create points at. The title is not declared, because a
		// surface names these by argument name across every tool, and on
		// the tracker's tools `title` is a work item's text, not an object.
		objects: []string{"container", "page"},
	},
	chat.Domain{}.Name(): {
		class: chatClass,
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
// classes the tools' own failures carry, since [writeFailure] and
// [pageWriteFailure] fall back to it, and [chatWriteFailure] classes by
// [chatClass] itself.
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
		ambiguous       *tracker.ErrAmbiguous
		ambiguousAnswer *tracker.ErrAmbiguousAnswer
	)
	switch {
	case errors.Is(err, tracker.ErrNoTask), errors.Is(err, tracker.ErrNoComment),
		errors.Is(err, tracker.ErrNoProject), errors.Is(err, tracker.ErrNoFile):
		return tools.RefusalNotFound, true
	case errors.Is(err, tracker.ErrStaleVersion):
		return tools.RefusalStaleVersion, true
	case errors.Is(err, tracker.ErrForbidden), errors.Is(err, tracker.ErrNotAuthor):
		return tools.RefusalForbidden, true
	case errors.Is(err, tracker.ErrAlreadyAnswered):
		return tools.RefusalAlreadyAnswered, true
	case errors.Is(err, tracker.ErrInboxFull):
		return tools.RefusalInboxFull, true
	case errors.Is(err, tracker.ErrReassignmentBudget):
		return tools.RefusalReassignmentBudget, true
	case errors.Is(err, tracker.ErrNothingToRestore):
		return tools.RefusalConflict, true
	case errors.Is(err, tracker.ErrUploadStale), errors.Is(err, tracker.ErrIndexBuilding):
		return tools.RefusalUnavailable, true
	case errors.Is(err, tracker.ErrInvalid), errors.Is(err, tracker.ErrBadQuery),
		errors.Is(err, tracker.ErrTooBroad), errors.Is(err, tracker.ErrNoType),
		errors.Is(err, tracker.ErrReparentAcrossProjects),
		errors.As(err, &ambiguous), errors.As(err, &ambiguousAnswer):
		return tools.RefusalInvalid, true
	}
	return "", false
}

// pageClass is the class of a refusal the knowledge base's writer decided.
func pageClass(err error) (tools.Refusal, bool) {
	switch {
	case errors.Is(err, pages.ErrNotFound):
		return tools.RefusalNotFound, true
	case errors.Is(err, pages.ErrStaleVersion):
		return tools.RefusalStaleVersion, true
	case errors.Is(err, pages.ErrTitleTaken):
		return tools.RefusalExists, true
	case errors.Is(err, pages.ErrConflict):
		return tools.RefusalConflict, true
	case errors.Is(err, pages.ErrReserved):
		return tools.RefusalForbidden, true
	case errors.Is(err, pages.ErrInvalid), errors.Is(err, pages.ErrParent),
		errors.Is(err, pages.ErrBadCursor):
		return tools.RefusalInvalid, true
	}
	return "", false
}
