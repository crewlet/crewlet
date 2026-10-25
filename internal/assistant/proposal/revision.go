package proposal

import (
	"errors"
	"fmt"
	"slices"
	"strings"
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
)

// RevisionKinds are the seven, in declaration order.
var RevisionKinds = []RevisionKind{RevisionTask, RevisionPage, RevisionArtifact, RevisionPerson,
	RevisionCharter, RevisionEntity, RevisionResource}

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

// EntityTag is the If-Match a write bound to r is sent under, quoted, and
// whether r carries one: a charter's and an entity's version is the entity tag
// its route judges a write against, so the route refuses a change made in the
// moments between the approval's re-read and the write, which the re-read
// alone cannot. Every other kind's precondition travels in the write's own
// arguments (a task's if_match, a page's base_version), or is the re-read's
// alone (a resource's version field).
func (r Revision) EntityTag() (string, bool) {
	if (r.Kind != RevisionCharter && r.Kind != RevisionEntity) || r.Version == "" {
		return "", false
	}
	return `"` + r.Version + `"`, true
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
		return Revision{}, fmt.Errorf("%w: revision kind %q is not one of %v", ErrInvalid,
			kind, RevisionKinds)
	}
	return r, nil
}

// ErrGone is what a [Revisions] answers for an object the person can no longer
// read at all (deleted, or out of their reach since the preview): a revision
// that moved as far as a revision can.
var ErrGone = errors.New("proposal: the object the revision names is gone")
