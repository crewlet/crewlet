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
