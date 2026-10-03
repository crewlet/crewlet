package configapi

import (
	"context"
	"fmt"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
)

// Writing ONE entity: the draft PUT /config/{kind}/{id} applies.
//
// What that route knows is not small — splice into a copy of the active
// revision so everything the caller did not send stays exactly as stored,
// refuse a rename rather than coercing it, restore the masks a redacted read
// handed back, and validate the WHOLE document rather than the entity, because
// an entity that is valid on its own can still break the company. So it is a
// draft of its own, and [Service.prepare] and [Service.commit] carry it as they
// carry the whole-document write.
//
// A MERGE PATCH CANNOT DO THIS. RFC 7396 replaces an array wholesale, so a
// patch addressing `mcp_servers[2]` would delete every other server.
//
// AND THE SAME DRAFT FROM INSIDE THIS PROCESS ([Service.ApplyEntity]): the
// setup surface records a seat's own app, and the reconcile loop records an
// installation it discovered or removes a disconnected seat's credentials,
// each against ONE seat — and a second surface that reimplemented what the
// route knows would be a second idea of how a seat is written.

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

	Summary string

	// By is who the revision records as its author; see [ApplyRequest.By].
	By iam.Actor

	// Expect is the revision the caller built this edit on, empty for
	// unconditional. See [ApplyRequest.Expect].
	Expect string
}

// ApplyEntity splices one entity into the active revision and activates it.
func (s *Service) ApplyEntity(ctx context.Context, req ApplyEntityRequest) (Applied, error) {
	d, err := entityDraft(req.Kind, req.ID, asText(req.Body), req.Expect, false)
	if err != nil {
		return Applied{}, err
	}
	prepared, err := s.prepare(ctx, d)
	if err != nil {
		return Applied{}, err
	}
	return s.commit(ctx, prepared, req.Summary, req.By)
}

// EntityError reports an entity write the document refused: no such entity,
// an identity mismatch, or a body this kind cannot read.
type EntityError struct{ Err error }

func (e *EntityError) Error() string { return "configapi: " + e.Err.Error() }
func (e *EntityError) Unwrap() error { return e.Err }

// entityDraft replaces the entity of one kind under one id — or, with create,
// adds one under an id the collection does not carry yet.
//
// Nothing to splice into is refused rather than treated as an empty company:
// building the first revision out of one entity is not what this write is for.
func entityDraft(kind, id string, body submitted, expect string, create bool) (draft, error) {
	access, ok := entityKinds[kind]
	if !ok {
		return draft{}, &EntityError{Err: fmt.Errorf("%w: %q (want one of %v)",
			ErrUnknownEntityKind, kind, EntityKinds())}
	}
	return draft{
		expect: expect, requireActive: true,
		// VALIDATED WHOLE, not just the entity. A seat naming a provider
		// that no longer exists is valid on its own and breaks the
		// company, and a per-entity surface is exactly where that gets
		// introduced.
		rules: (*config.Company).Validate,
		build: func(b base) (*config.Company, []byte, error) {
			// A SECOND COPY, so the splice lands on a document that still
			// has everything the caller did not send, and the prior stays
			// the unmodified side the mask restore reads from.
			spliced, err := config.DecodeCompany(b.document)
			if err != nil {
				return nil, nil, fmt.Errorf("configapi: decode the active revision: %w", err)
			}
			write := access.replace
			if create {
				write = access.create
			}
			if write == nil {
				return nil, nil, &EntityError{Err: ErrNotCreatable}
			}
			if refused := write(spliced, id, body); refused != nil {
				return nil, nil, &EntityError{Err: refused}
			}
			// The masks the caller was shown come back as the values they
			// hide, against the revision they were shown FROM. A created
			// entity matches no member of that revision, so nothing in it
			// is restored: it was never shown, so it holds no mask.
			spliced.RestoreRedacted(b.prior)
			entity, _ := access.find(spliced, id)
			document, err := spliceStored(b.document, access, id, entity, create)
			if err != nil {
				return nil, nil, err
			}
			return spliced, document, nil
		},
	}, nil
}
