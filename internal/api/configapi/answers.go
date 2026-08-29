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

// ErrNoActiveRevision reports a company nobody has configured yet.
//
// A real state and distinct from a failure: a deployment before its first
// import has no configuration, and reporting that as an error would make a
// working new install look broken.
var ErrNoActiveRevision = errors.New("configapi: no active revision")

// Document is the active company, redacted.
func (s *Service) Document(ctx context.Context) (*config.Company, error) {
	if s == nil {
		return nil, fmt.Errorf("configapi: no store on this node")
	}
	revision, found, err := s.configs.Active(ctx)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, ErrNoActiveRevision
	}
	company, err := s.open(revision)
	if err != nil {
		return nil, err
	}
	return company.Redact(), nil
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
		return nil, fmt.Errorf("%w: %s", store.ErrNoRevision, revisionID)
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
		return store.Revision{}, fmt.Errorf("%w: %s", store.ErrNoRevision, against)
	}
	return base, nil
}
