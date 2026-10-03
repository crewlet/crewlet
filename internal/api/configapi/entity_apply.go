package configapi

import (
	"fmt"

	"github.com/crewlet/crewlet/internal/config"
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
// THERE IS NO PROGRAMMATIC DOOR BESIDE THE ROUTE. There was one, for "a
// second surface in this binary" to change a seat through, and it outlived its
// one caller: a seat left the configuration for the org chart, whose per-seat
// write is the engine's own ([engine.Engine.SetSeatDocument]), so it was a
// write path nothing reached, documented as the one the setup surface used.

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
		// VALIDATED WHOLE, not just the entity. A provider that an entity
		// write reshapes is valid on its own and may break every worker
		// template naming it, and a per-entity surface is exactly where
		// that gets introduced. A seat naming it is the org chart's, which
		// no revision holds — see [ErrIdentityMismatch] for the one write
		// that checks the two halves together.
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
