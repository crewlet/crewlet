package proposal

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/words"
)

// RevisionKind is what a source revision is a revision of.
type RevisionKind string

const (
	// RevisionTask is a work item by key, at its version.
	RevisionTask RevisionKind = "task"
	// RevisionPage is a knowledge page by id, at its base version.
	RevisionPage RevisionKind = "page"
	// RevisionArtifact is an artifact by id, at its head.
	RevisionArtifact RevisionKind = "artifact"
	// RevisionPerson is a person's record by seat handle, at its version.
	RevisionPerson RevisionKind = "person"
	// RevisionRoom is a chat room by id, at a digest of the room as a card
	// shows it: its settings and its whole membership, which a change of
	// either moves and a message said in the room does not. NOT the room's
	// own `revision`, which every message moves, so a change bound to it
	// would go stale whenever anybody spoke.
	RevisionRoom RevisionKind = "room"
	// RevisionMessage is a chat message by `<room id>/<message id>`, each
	// part path-escaped, at the position of the record that last changed it
	// (`<generation>:<sequence>`), which an edit and a delete move and a
	// reaction does not.
	RevisionMessage RevisionKind = "message"
	// RevisionCharter is the company's principles (`GET /config/charter`),
	// at the entity tag the route answers, unquoted: the one revision whose
	// version is an HTTP entity tag, which the approved write is sent under
	// as its If-Match ([Revision.EntityTag]).
	RevisionCharter RevisionKind = "charter"
	// RevisionEntity is any resource a generic API change was previewed
	// against (internal/assistant/catalog's propose_api_change), by the path
	// it was read at, at the strong entity tag that read answered, unquoted:
	// the charter's kind generalised, so the approved write is sent under the
	// tag as its If-Match and the route refuses a change made meanwhile.
	RevisionEntity RevisionKind = "entity"
	// RevisionResource is a resource a generic API change was previewed
	// against whose read answers no entity tag but states its own version in
	// a top-level field: by its path, at `<field>=<value>`. The approval
	// reads the field again, and a moved value is a stale proposal.
	RevisionResource RevisionKind = "resource"
	// RevisionCreate is a resource a generic API change creates: by the path
	// its preview read answered 404 at, at [VersionAbsent]. The approved
	// write is sent under `If-None-Match: *`, so the route refuses it once
	// something holds the path, and the approval's re-read answers
	// [VersionPresent] for a path somebody filled since the preview.
	RevisionCreate RevisionKind = "create"
	// RevisionAtlasEntry is an atlas entry by id, at a digest of the entry as
	// a card shows it: its state, and each fact and relation with the source
	// holding it, which a change moves and a new sighting of the same value
	// does not.
	RevisionAtlasEntry RevisionKind = "atlas_entry"
	// RevisionAsk is a question on a task, `<task id>/<comment id>`, at a digest of it
	// as shown, which an answer, an edit or a removal moves.
	RevisionAsk RevisionKind = "ask"
)

// The two versions a [RevisionCreate] is at.
const (
	// VersionAbsent is nothing at the path: what the preview read, and what
	// the approval's re-read must still find.
	VersionAbsent = "absent"
	// VersionPresent is something at the path, which a create bound to its
	// absence has lost to.
	VersionPresent = "present"
)

// RevisionKinds are the twelve, in declaration order.
var RevisionKinds = []RevisionKind{RevisionTask, RevisionPage, RevisionArtifact, RevisionPerson,
	RevisionRoom, RevisionMessage, RevisionCharter, RevisionEntity, RevisionResource,
	RevisionCreate, RevisionAtlasEntry, RevisionAsk}

// CharterID is the one charter's id in a revision: a company has one.
const CharterID = "company"

// Valid reports whether k is a kind this build knows.
func (k RevisionKind) Valid() bool { return slices.Contains(RevisionKinds, k) }

// Revision is the object a preview read and the version it read it at:
// `<kind>:<id>@<version>`, the one spelling every proposal's source revision
// takes, so an approval compares the string a re-read answers with the string
// the preview recorded.
type Revision struct {
	Kind    RevisionKind
	ID      string
	Version string
}

// String is the revision's spelling.
func (r Revision) String() string { return string(r.Kind) + ":" + r.ID + "@" + r.Version }

// Precondition is the conditional request a write bound to r is sent under:
// THE ONE RULE from what a preview read to the headers the route judges.
//   - A charter's and an entity's version is its read's strong entity tag,
//     sent as a quoted If-Match, so the route refuses a change made between
//     the approval's re-read and the write.
//   - A create's is the absence its read found, sent as `If-None-Match: *`
//     ([exec.AnyRepresentation]); the route refuses it once something holds
//     the path (`412 entity_exists`).
//   - Every other kind's travels in the write's arguments (a task's if_match,
//     a page's base_version) or is the re-read's alone: none is sent.
//
// Never both (the API's `conflicting_preconditions`).
func (r Revision) Precondition() (ifMatch, ifNoneMatch string) {
	switch {
	case r.Version == "":
		return "", ""
	case r.Kind == RevisionCharter || r.Kind == RevisionEntity:
		return `"` + r.Version + `"`, ""
	case r.Kind == RevisionCreate && r.Version == VersionAbsent:
		return "", exec.AnyRepresentation
	}
	return "", ""
}

// ParseRevision reads a revision back, refusing a spelling [Revision.String]
// does not write.
func ParseRevision(s string) (Revision, error) {
	kind, rest, ok := strings.Cut(s, ":")
	if !ok {
		return Revision{}, fmt.Errorf("%w: revision %q is not <kind>:<id>@<version>", ErrInvalid, s)
	}
	at := strings.LastIndexByte(rest, '@')
	if at <= 0 || at == len(rest)-1 {
		return Revision{}, fmt.Errorf("%w: revision %q is not <kind>:<id>@<version>", ErrInvalid, s)
	}
	r := Revision{Kind: RevisionKind(kind), ID: rest[:at], Version: rest[at+1:]}
	if !r.Kind.Valid() {
		return Revision{}, fmt.Errorf("%w: revision kind %q is not one of %s", ErrInvalid,
			kind, words.Or(RevisionKinds))
	}
	return r, nil
}

// ErrGone is what a [Revisions] answers for an object the person can no longer
// read at all (deleted, or out of their reach since the preview): a revision
// that moved as far as a revision can.
var ErrGone = errors.New("proposal: the object the revision names is gone")
