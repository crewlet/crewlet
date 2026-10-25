package proposal

import (
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
)

// RevisionKinds are the four, in declaration order.
var RevisionKinds = []RevisionKind{RevisionTask, RevisionPage, RevisionArtifact, RevisionPerson}

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
