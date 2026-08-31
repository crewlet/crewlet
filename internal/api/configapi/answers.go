package configapi

import (
	"context"
	"errors"
	"fmt"

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
	sideTarget  = "not_found"
	sideAgainst = "against_not_found"
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
	side string
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
	if s == nil {
		return nil, store.Revision{}, fmt.Errorf("configapi: no store on this node")
	}
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
	return company.Redact(), revision, nil
}

// Revisions is the history, newest first, metadata only.
func (s *Service) Revisions(ctx context.Context, limit, offset int) ([]map[string]any, error) {
	if s == nil {
		return nil, fmt.Errorf("configapi: no store on this node")
	}
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
// Both sides redacted, so a rotated credential shows as a changed mask and
// never as either value.
func (s *Service) Diff(ctx context.Context, revisionID, against string) (map[string]any, error) {
	if s == nil {
		return nil, fmt.Errorf("configapi: no store on this node")
	}
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
	changes, err := Changes(from.Redact(), to.Redact())
	if err != nil {
		return nil, err
	}
	return map[string]any{"from": base.ID, "to": target.ID, "changes": changes}, nil
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
