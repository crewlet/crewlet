package queries

import (
	"context"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/store"
)

// The config family, answered through the SAME functions the /config routes
// call. Registered as operator-only: reading the document exposes the whole
// company — its org chart, which integrations are wired, and every ${VAR}
// reference by name — which is what makes /config the one prefix never
// eligible for anonymous read.

// ConfigAuditLimit bounds the revision history a dashboard asks for.
const ConfigAuditLimit = 50

// configDocument answers the active company, redacted.
//
// NULL when nothing is active, not an error: a deployment before its first
// import has no configuration, and the config screen renders that as "nothing
// configured yet" rather than as a failed query.
func (s Sources) configDocument(ctx context.Context, _ Params) (any, error) {
	company, err := s.Config.Document(ctx)
	if errors.Is(err, configapi.ErrNoActiveRevision) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return company, nil
}

// configEntities answers one addressable collection of the active revision:
// the identities in it, or one entity out of it.
//
// The read half of the Config room's entity editor, whose write half is
// PUT /config/{kind}/{id}. Operator-only like every other config answer —
// this is a slice of the same document, and a per-entity read that was not
// gated would be a way to fetch the whole company one seat at a time.
func (s Sources) configEntities(ctx context.Context, p Params) (any, error) {
	kind := p.String("kind")
	if kind == "" {
		return nil, fmt.Errorf("%w: config_entities needs a kind (one of %v)",
			ErrBadParams, configapi.EntityKinds())
	}
	if id := p.String("id"); id != "" {
		entity, err := s.Config.Entity(ctx, kind, id)
		switch {
		case errors.Is(err, configapi.ErrNoActiveRevision):
			return nil, nil
		case errors.Is(err, configapi.ErrUnknownEntityKind),
			errors.Is(err, configapi.ErrNoSuchEntity):
			// A BAD REQUEST, not a server fault: both are things the
			// caller named, and reporting them as failures would have the
			// room show "the query failed" for a typo.
			return nil, fmt.Errorf("%w: %s", ErrBadParams, err)
		case err != nil:
			return nil, err
		}
		return map[string]any{"kind": kind, "id": id, "entity": entity}, nil
	}
	ids, err := s.Config.Entities(ctx, kind)
	switch {
	case errors.Is(err, configapi.ErrNoActiveRevision):
		return nil, nil
	case errors.Is(err, configapi.ErrUnknownEntityKind):
		return nil, fmt.Errorf("%w: %s", ErrBadParams, err)
	case err != nil:
		return nil, err
	}
	return map[string]any{"kind": kind, "ids": ids}, nil
}

// configAudit answers the revision history.
func (s Sources) configAudit(ctx context.Context, p Params) (any, error) {
	limit := Clamp(p.Int("limit", ConfigAuditLimit), ConfigAuditLimit, configapi.MaxPage)
	return s.Config.Revisions(ctx, limit, p.Int("offset", 0))
}

// configDiff compares one revision against the active one.
func (s Sources) configDiff(ctx context.Context, p Params) (any, error) {
	id := p.String("revision_id")
	if id == "" {
		return nil, fmt.Errorf("%w: config_diff needs a revision_id", ErrBadParams)
	}
	body, err := s.Config.Diff(ctx, id, p.String("against"))
	switch {
	case errors.Is(err, configapi.ErrNoActiveRevision),
		errors.Is(err, store.ErrNoRevision):
		// NOT an error to the caller. The config screen asks for a diff
		// per row as it renders them, and a revision that has aged out
		// between the listing and the diff must leave one cell empty
		// rather than fail the screen.
		return nil, nil
	case err != nil:
		return nil, err
	default:
		return body, nil
	}
}
