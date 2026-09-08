package configapi

import (
	"context"
	"fmt"
)

// Writing ONE entity from inside this process.
//
// The document-wide sibling of [Service.Apply], and it exists for the same
// reason: a second surface in this binary has to be able to change a seat
// without reimplementing what PUT /config/roles/{handle} knows. What that
// route knows is not small — splice into a copy of the active revision so
// everything the caller did not send stays exactly as stored, refuse a rename
// rather than coercing it, restore the masks a redacted read handed back, and
// validate the WHOLE document rather than the entity, because a seat naming a
// provider that no longer exists is valid on its own and breaks the company.
//
// A MERGE PATCH CANNOT DO THIS. RFC 7396 replaces an array wholesale, so a
// patch addressing `roles[2]` would delete every other seat, which is why the
// setup surface routes a per-seat write here instead.

// ApplyEntityRequest is one entity write.
type ApplyEntityRequest struct {
	// Kind is the collection: roles, units, llm-providers, mcp-servers.
	Kind string

	// ID is the entity's own identity within it, and the address: a
	// mismatch between this and the body's identity is refused rather than
	// coerced, because silently keeping the old one would land every other
	// edit and leave the caller believing a rename took.
	ID string

	// Body is the entity's JSON, whole.
	Body []byte

	Summary  string
	Operator string

	// Expect is the revision the caller built this edit on, empty for
	// unconditional. See [ApplyRequest.Expect].
	Expect string
}

// EntityError reports an entity write the document refused: no such entity,
// an identity mismatch, or a body this kind cannot read.
type EntityError struct{ Err error }

func (e *EntityError) Error() string { return "configapi: " + e.Err.Error() }
func (e *EntityError) Unwrap() error { return e.Err }

// ApplyEntity splices one entity into the active revision and activates it.
func (s *Service) ApplyEntity(ctx context.Context, req ApplyEntityRequest) (Applied, error) {
	if s == nil {
		return Applied{}, fmt.Errorf("configapi: no store on this node")
	}
	access, ok := entityKinds[req.Kind]
	if !ok {
		return Applied{}, &EntityError{Err: ErrUnknownEntityKind}
	}
	active, found, err := s.configs.Active(ctx)
	if err != nil {
		return Applied{}, fmt.Errorf("configapi: read the active revision: %w", err)
	}
	if !found {
		// Nothing to splice into. Refused rather than treated as an empty
		// company: building the first revision out of one seat is not what
		// this route is for.
		return Applied{}, ErrNoActiveRevision
	}
	if req.Expect != "" && req.Expect != active.ID {
		return Applied{}, &RacedError{Base: req.Expect, Current: active.ID}
	}
	prior, err := s.open(active)
	if err != nil {
		return Applied{}, fmt.Errorf("configapi: open the active revision: %w", err)
	}
	// A SECOND COPY, so the splice lands on a document that still has
	// everything the caller did not send, and `prior` stays the unmodified
	// side the mask restore reads from.
	spliced, err := s.open(active)
	if err != nil {
		return Applied{}, fmt.Errorf("configapi: open the active revision: %w", err)
	}
	if err := access.replace(spliced, req.ID, req.Body); err != nil {
		return Applied{}, &EntityError{Err: err}
	}
	spliced.RestoreRedacted(prior)
	if err := spliced.Validate(); err != nil {
		return Applied{}, &ValidationError{Err: err}
	}
	return s.activate(ctx, spliced, active.ID, req.Summary, req.Operator)
}
