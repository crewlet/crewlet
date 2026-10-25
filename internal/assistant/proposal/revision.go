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
)

// RevisionKinds are the five, in declaration order.
var RevisionKinds = []RevisionKind{RevisionTask, RevisionPage, RevisionArtifact, RevisionPerson,
	RevisionCharter}

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
// whether r carries one: a charter revision's version is the entity tag its
// route judges a write against, so the route refuses a change made in the
// moments between the approval's re-read and the write, which the re-read
// alone cannot. Every other kind's precondition travels in the write's own
// arguments (a task's if_match, a page's base_version).
func (r Revision) EntityTag() (string, bool) {
	if r.Kind != RevisionCharter || r.Version == "" {
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
