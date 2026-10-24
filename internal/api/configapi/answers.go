package configapi

import (
	"context"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/store"
)

// The three questions this surface answers, as functions rather than as route
// handlers.
//
// The REST route and the dashboard's socket query both call these, for the
// reason the whole read surface exists: two implementations of "what is the
// config" can disagree, and the moment they do, which answer an operator gets
// depends on which transport their browser happened to use.

// The two halves of a diff, named so a handler can say WHICH one is missing.
//
// "The revision you asked about" and "the one you asked to compare it with"
// are different mistakes, and answering a single not_found makes the caller
// check both.
const (
	sideTarget  = httpjson.CodeNotFound
	sideAgainst = httpjson.CodeAgainstNotFound
)

// missingRevision is a revision lookup that found nothing, carrying which
// side of the diff it was.
//
// A TYPED ERROR rather than a message the handler re-reads. The handler used
// to substring-match the error text against the URL's path value, which is
// wrong whenever one id CONTAINS the other: `?against=r-7-typo` on revision
// `r-7` matched, so the caller was told the revision they asked about was
// missing when it exists and the one they named does not. `against` is
// unvalidated, so producing that pair takes nothing but a typo.
//
// It unwraps to [store.ErrNoRevision], so every existing errors.Is check
// keeps working and only the handler that needs the side asks for it.
type missingRevision struct {
	side httpjson.Code
	id   string
}

func (e *missingRevision) Error() string {
	return fmt.Sprintf("%s: %s", store.ErrNoRevision, e.id)
}

func (e *missingRevision) Unwrap() error { return store.ErrNoRevision }

// ErrNoActiveRevision reports a company nobody has configured yet.
//
// A real state and distinct from a failure: a deployment before its first
// import has no configuration, and reporting that as an error would make a
// working new install look broken.
var ErrNoActiveRevision = errors.New("configapi: no active revision")

// Document is the active company, redacted.
func (s *Service) Document(ctx context.Context) (*config.Company, error) {
	company, _, err := s.documentOf(ctx)
	return company, err
}

// documentOf is [Service.Document] plus the revision it came from, which is
// what an entity-tag needs: the document changes exactly when the active
// revision does, so the revision id IS the validator.
func (s *Service) documentOf(ctx context.Context) (*config.Company, store.Revision, error) {
	company, revision, err := s.activeCompany(ctx)
	if err != nil {
		return nil, store.Revision{}, err
	}
	return company.Redact(), revision, nil
}

// activeCompany opens the active revision UNREDACTED.
//
// Never answered to a caller as it stands. It exists for the one read that
// needs what redaction hides without publishing any of it: see
// [Service.References].
func (s *Service) activeCompany(ctx context.Context) (*config.Company, store.Revision, error) {
	revision, found, err := s.configs.Active(ctx)
	if err != nil {
		return nil, store.Revision{}, err
	}
	if !found {
		return nil, store.Revision{}, ErrNoActiveRevision
	}
	company, err := s.open(revision)
	if err != nil {
		return nil, store.Revision{}, err
	}
	return company, revision, nil
}

// References is every ${VAR} the active document names, paired with the path
// of the field that names it.
//
// THE QUESTION IS "WHAT BREAKS IF I REMOVE THIS", and it is asked from the
// Secrets screen rather than from here: deleting or renaming a credential the
// document still points at leaves that pointer resolving to nothing, and the
// surfaces holding it start refusing deliveries with no error naming the row
// that went away. The answer has to carry PATHS — a name on its own says a
// reference exists somewhere, which nobody can act on.
//
// It lives on the config surface because it is a fact about the config
// document, not about the secret store: the store holds the values, and the
// only thing that knows a value is spoken for is the document that names it.
// /secrets could not answer this without a second reader of the company
// revision.
//
// Derived from the UNREDACTED document, and it answers only names and paths,
// never a value. The redacted one GET /config serves would hide exactly the
// references this question is about: redaction shows a value only when it is
// one whole ${VAR}, so a credential written as "Bearer ${TOKEN}" reads as a
// mask there, and an index built from that mask would tell an operator that
// nothing points at TOKEN just before they delete it.
//
// The revision travels with the answer because the index is derived from it
// and changes exactly when it does, which is what the route's entity-tag
// needs and what a caller comparing two answers has to key on.
func (s *Service) References(ctx context.Context) ([]config.Reference, store.Revision, error) {
	company, revision, err := s.activeCompany(ctx)
	if err != nil {
		return nil, store.Revision{}, err
	}
	return config.References(company), revision, nil
}

// ActiveRevision is the id of the revision the fleet is running.
//
// The token every conditional write is built against: a caller reads state,
// derives an edit, and names what it read so a concurrent write is refused
// rather than silently overwritten.
func (s *Service) ActiveRevision(ctx context.Context) (string, error) {
	revision, found, err := s.configs.Active(ctx)
	if err != nil {
		return "", err
	}
	if !found {
		return "", ErrNoActiveRevision
	}
	return revision.ID, nil
}

// Revisions is the history, newest first, metadata only.
func (s *Service) Revisions(ctx context.Context, limit, offset int) ([]map[string]any, error) {
	revisions, err := s.configs.List(ctx, limit, offset)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(revisions))
	for _, revision := range revisions {
		out = append(out, meta(revision))
	}
	return out, nil
}

// Diff compares one revision against another, or against the active one.
//
// Both sides are compared as stored and reported redacted (see [Changes]), so
// a rotated credential shows as a changed mask and never as either value. The
// listing is cut at [MaxChanges] because this is the side answering over a
// wire, and `changes_total` is always present so a reader renders how many
// changed rather than how many arrived.
func (s *Service) Diff(ctx context.Context, revisionID, against string) (map[string]any, error) {
	target, found, err := s.configs.Get(ctx, revisionID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, &missingRevision{side: sideTarget, id: revisionID}
	}
	base, err := s.baseFor(ctx, against)
	if err != nil {
		return nil, err
	}
	from, err := s.open(base)
	if err != nil {
		return nil, err
	}
	to, err := s.open(target)
	if err != nil {
		return nil, err
	}
	changes, err := Changes(from, to)
	if err != nil {
		return nil, err
	}
	// THE CUT, AND THE TOTAL BESIDE IT. A response body and a socket frame
	// are the side with a size budget, so the bound is taken here rather
	// than inside the walk — `crewlet config diff` writes to a terminal and
	// prints the same comparison whole.
	//
	// Reported as a key rather than as an entry: the marker used to ride
	// along IN the listing as a pathless Change whose To was a sentence,
	// which every client then had to know was not a change. The dashboard
	// did not, and drew it as a blank path turning undefined into prose.
	total := len(changes)
	if total > MaxChanges {
		changes = changes[:MaxChanges:MaxChanges]
	}
	if changes == nil {
		// An empty LIST for identical revisions, like every other listing
		// this surface answers — `null` is a shape a typed client has to
		// guard for a second time to learn nothing changed.
		changes = []Change{}
	}
	return map[string]any{
		"from": base.ID, "to": target.ID,
		"changes": changes, "changes_total": total,
	}, nil
}

// baseFor resolves the side a diff compares against.
func (s *Service) baseFor(ctx context.Context, against string) (store.Revision, error) {
	if against == "" || against == "active" {
		active, found, err := s.configs.Active(ctx)
		if err != nil {
			return store.Revision{}, err
		}
		if !found {
			return store.Revision{}, ErrNoActiveRevision
		}
		return active, nil
	}
	base, found, err := s.configs.Get(ctx, against)
	if err != nil {
		return store.Revision{}, err
	}
	if !found {
		return store.Revision{}, &missingRevision{side: sideAgainst, id: against}
	}
	return base, nil
}
