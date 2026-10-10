package configapi

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/store"
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
// The route itself is a caller of the same draft, so the two cannot differ.
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

	Summary string
	// Author is who the revision records as its writer; see
	// [ApplyRequest.Author].
	Author store.Author

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
	d, err := entityDraft(req.Kind, req.ID, entityWrite{body: asText(req.Body)}, req.Expect)
	if err != nil {
		return Applied{}, err
	}
	d.author = req.Author
	prepared, err := s.prepare(ctx, d)
	if err != nil {
		return Applied{}, err
	}
	return s.commit(ctx, prepared, req.Summary)
}

// entityWrite is one write to one entity: what was sent, and how it is read.
type entityWrite struct {
	// body is the entity whole — or, with patch, a JSON Merge Patch over it.
	body submitted
	// patch reads body as a merge patch over the entity as it stands.
	//
	// OVER THIS BUILD'S ENCODING of the entity, which is the shape a PUT
	// body takes: the merged object is then read exactly as a PUT's body is
	// — strictly, so a key the patch misspelt is refused at its place — and
	// spliced the same way, so what the stored element carries that this
	// build cannot represent is kept ([spliceStored]) whichever verb wrote.
	patch bool
	// create adds the entity rather than replacing one (`If-None-Match: *`).
	create bool
	// self holds a caller below admin to the availability of their own
	// seat (selfservice.go); nil for an admin and for the engine's writes.
	self *selfService
}

// entityDraft replaces the entity of one kind under one id — or, with
// create, adds one under an id the collection does not carry yet.
//
// Nothing to splice into is refused rather than treated as an empty company:
// building the first revision out of one seat is not what this write is for.
func entityDraft(kind, id string, write entityWrite, expect string) (draft, error) {
	access, ok := entityKinds[kind]
	if !ok {
		return draft{}, &EntityError{Err: fmt.Errorf("%w: %q (want one of %v)",
			ErrUnknownEntityKind, kind, EntityKinds())}
	}
	if write.create && access.create == nil {
		return draft{}, &EntityError{Err: fmt.Errorf("%w: %s", ErrNotCreatable, kind)}
	}
	return draft{
		expect: expect, requireActive: true,
		// VALIDATED WHOLE, not just the entity. A seat naming a provider
		// that no longer exists is valid on its own and breaks the company,
		// and a per-entity surface is exactly where that gets introduced.
		rules: (*config.Company).Validate,
		build: func(b base) (*config.Company, []byte, error) {
			// A SECOND COPY, so the splice lands on a document that still
			// has everything the caller did not send, and the prior stays
			// the unmodified side the mask restore reads from.
			spliced, err := config.DecodeCompany(b.document)
			if err != nil {
				return nil, nil, fmt.Errorf("configapi: decode the active revision: %w", err)
			}
			// WHOSE SEAT, judged on the document the write is built on and
			// will land on — see selfservice.go.
			if write.self != nil {
				if refused := write.self.owns(spliced, kind, id); refused != nil {
					return nil, nil, &EntityError{Err: refused}
				}
			}
			body := write.body
			// THE ENTITY AS IT STANDS, encoded before the splice replaces
			// it: what a patch merges over, and what a member's write is
			// compared against.
			var found []byte
			if write.patch || write.self != nil {
				current, held := access.find(spliced, id)
				if !held {
					return nil, nil, &EntityError{Err: fmt.Errorf("%w: %s/%s",
						ErrNoSuchEntity, kind, id)}
				}
				if found, err = json.Marshal(current); err != nil {
					return nil, nil, fmt.Errorf("configapi: encode %s/%s: %w", kind, id, err)
				}
			}
			if write.patch {
				merged, refused := patchedEntity(found, write.body.text)
				if refused != nil {
					return nil, nil, &PatchError{Err: refused}
				}
				body = asText(merged)
			}
			splice := access.replace
			if write.create {
				splice = access.create
			}
			if refused := splice(spliced, id, body); refused != nil {
				return nil, nil, &EntityError{Err: refused}
			}
			// The masks the caller was shown come back as the values they
			// hide, against the revision they were shown FROM. A created
			// entity matches no member of that revision, so nothing in it
			// is restored: it was never shown, so it holds no mask.
			spliced.RestoreRedacted(b.prior)
			entity, _ := access.find(spliced, id)
			// AND WHAT IT CHANGED, judged after the restore, so a mask
			// sent back as it was shown is the value it stands for rather
			// than a change.
			if write.self != nil {
				if refused := onlyAvailability(found, entity); refused != nil {
					return nil, nil, &EntityError{Err: refused}
				}
			}
			document, err := spliceStored(b.document, access, id, entity, write.create)
			if err != nil {
				return nil, nil, err
			}
			return spliced, document, nil
		},
	}, nil
}

// patchedEntity merges a JSON Merge Patch (RFC 7396) over one entity's
// encoding, and answers the merged entity's.
//
// THE PATCH IS READ AS THE DOCUMENT PATCH IS ([readPatch]): an object, never
// empty, and `null` removes a field. An array in it replaces the entity's
// array whole, which on one entity is the only reading there is — a seat's
// `manages` list has no members a patch could address.
func patchedEntity(current, patch []byte) ([]byte, error) {
	target, err := decodeTree(current)
	if err != nil {
		return nil, fmt.Errorf("configapi: decode the entity: %w", err)
	}
	overlay, err := readPatch(patch)
	if err != nil {
		return nil, err
	}
	merged, err := json.Marshal(mergePatch(target, overlay))
	if err != nil {
		return nil, fmt.Errorf("configapi: encode the patched entity: %w", err)
	}
	return merged, nil
}
