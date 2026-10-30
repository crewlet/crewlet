package configapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
)

// Writing SEVERAL SEATS AS ONE REVISION, and rewriting the stored document
// itself: the two writes an in-process caller makes that neither a merge patch
// nor a one-entity write can.
//
// A SAVE OF AN INTEGRATION'S AGENTS TAB CHANGES MANY SEATS AT ONCE, and it is
// one decision: turning three agents on and one off is reviewed, reverted and
// attributed as one change. Written as one entity write per seat it was as many
// revisions, each its own activation, and a failure part way through left the
// company with half of what the person saved. So every seat is changed on one
// decoded copy of the fleet's revision, spliced into the stored bytes one after
// another, and the whole document is validated and activated once.
//
// A MIGRATION READS WHAT THIS BUILD CANNOT REPRESENT. A stored revision decodes
// leniently: a key this build no longer has is dropped from the struct and
// carried back on every write ([config.CarryUnknown]). A migration that reads a
// retired value has to read it from the stored tree, and one that retires a key
// has to remove it there, or the next write carries it back for ever.

// SeatEdit is one seat's change in a [Service.ApplySeats] write.
type SeatEdit struct {
	// Handle is the seat's handle, its address in the document.
	Handle string

	// Edit changes the seat in place. It is handed this build's decoded
	// seat, on a copy of the fleet's revision; an error refuses the whole
	// write and nothing is stored.
	Edit func(*config.Role) error
}

// ApplySeatsRequest is one write of several seats.
type ApplySeatsRequest struct {
	// Seats are the seats the write changes, each named once.
	Seats []SeatEdit

	// Summary is the audit sentence stored on the revision.
	Summary string

	// By is who the revision records as its author; see [ApplyRequest.By].
	By iam.Actor

	// Expect is the revision the caller built this edit on, empty for
	// unconditional. See [ApplyRequest.Expect].
	Expect string
}

// ErrSeatNamedTwice reports a write naming one seat in two edits, which would
// leave the second edit judging a seat the first already changed.
var ErrSeatNamedTwice = errors.New("configapi: a seat is named twice in one write")

// ApplySeats changes every seat the request names on one copy of the fleet's
// revision and activates the result as ONE revision.
//
// The refusals are the write's: a [*RacedError] for a stale Expect, an
// [*EntityError] for a seat the document does not carry, and a
// [*ValidationError] for a company the change leaves invalid.
func (s *Service) ApplySeats(ctx context.Context, req ApplySeatsRequest) (Applied, error) {
	prepared, err := s.prepareSeats(ctx, req)
	if err != nil {
		return Applied{}, err
	}
	return s.commit(ctx, prepared, req.Summary)
}

// SeatsCheck is a dry run of [Service.ApplySeats]: the company the write would
// store, the revision it was built on, and its warnings, each marked with
// whether the write introduces it. Nothing is stored.
type SeatsCheck struct {
	Company  *config.Company
	Base     string
	Warnings []CheckedWarning
}

// CheckedWarning is one warning a dry run answers, and whether the write
// introduces it rather than the active revision already carrying it.
type CheckedWarning = checkedWarning

// CheckSeats is [Service.ApplySeats] judged and not stored: the same draft, the
// same rules, in the same order, so a check cannot answer "valid" for a write
// the save would refuse.
func (s *Service) CheckSeats(ctx context.Context, req ApplySeatsRequest) (SeatsCheck, error) {
	prepared, err := s.prepareSeats(ctx, req)
	if err != nil {
		return SeatsCheck{}, err
	}
	return SeatsCheck{Company: prepared.company, Base: prepared.base.ID,
		Warnings: checkedWarnings(prepared)}, nil
}

// prepareSeats builds and checks a several-seat write.
func (s *Service) prepareSeats(ctx context.Context, req ApplySeatsRequest) (*prepared, error) {
	handles := make([]string, 0, len(req.Seats))
	for _, edit := range req.Seats {
		handle := strings.TrimSpace(edit.Handle)
		if handle == "" || edit.Edit == nil {
			return nil, &EntityError{Err: fmt.Errorf("%w: a seat edit names no seat "+
				"or changes nothing", ErrNoSuchEntity)}
		}
		if slices.Contains(handles, handle) {
			return nil, &EntityError{Err: fmt.Errorf("%w: %q", ErrSeatNamedTwice, handle)}
		}
		handles = append(handles, handle)
	}
	access := entityKinds[EntityRoles]
	d := draft{
		expect: req.Expect, requireActive: true, by: req.By,
		rules: authoredRules, authored: true,
		build: func(b base) (*config.Company, []byte, error) {
			// A SECOND COPY, as a one-entity write makes: the edits land on
			// a document that still holds everything they do not touch,
			// and the prior stays the unmodified side the mask restore
			// reads from.
			spliced, err := config.DecodeCompany(b.document)
			if err != nil {
				return nil, nil, fmt.Errorf("configapi: decode the active revision: %w", err)
			}
			for _, edit := range req.Seats {
				found, ok := access.find(spliced, strings.TrimSpace(edit.Handle))
				if !ok {
					return nil, nil, &EntityError{Err: fmt.Errorf("%w: no seat %q",
						ErrNoSuchEntity, edit.Handle)}
				}
				if editErr := edit.Edit(found.(*config.Role)); editErr != nil {
					return nil, nil, &EntityError{Err: editErr}
				}
			}
			spliced.RestoreRedacted(b.prior)
			document := b.document
			for _, handle := range handles {
				entity, _ := access.find(spliced, handle)
				if document, err = spliceStored(document, access, handle, entity,
					false); err != nil {
					return nil, nil, err
				}
			}
			return spliced, document, nil
		},
	}
	return s.prepare(ctx, d)
}

// MigrateRequest rewrites the stored document of the fleet's revision.
type MigrateRequest struct {
	// Transform rewrites the stored document, decoded as a tree (objects,
	// lists and scalars, every number as the text it was stored as), in
	// place. It reports whether it changed anything: a migration that has
	// already run reports false, and nothing is written.
	Transform func(root map[string]any) (changed bool, err error)

	// Summary is the audit sentence stored on the revision.
	Summary string

	// By is the revision's author: the engine's own name for a migration
	// the engine runs.
	By iam.Actor

	// Expect is the revision the migration was planned on
	// ([Service.StoredDocument]), so what it moved before writing is what
	// the document it writes over names. A later revision is refused
	// [*RacedError], and the migration plans again.
	Expect string
}

// ErrNothingToMigrate reports a migration whose transform changed nothing,
// which is the answer every start after the first gets.
var ErrNothingToMigrate = errors.New("configapi: the document needs no migration")

// Migrate rewrites the fleet's revision through req.Transform and activates
// the result as one revision, held to req.Expect and built on the revision it
// read (a compare-and-set, so two nodes migrating at once store one revision
// and the other is refused [*RacedError]). [ErrNothingToMigrate] when the transform changed nothing, and
// [ErrNoActiveRevision] when the fleet runs no company.
//
// HELD TO THE RULES A RUNNING COMPANY DEPENDS ON, not to every rule an author
// is: the document is the company's own, migrated for it, and a migration
// refused over an unrelated advisory the company already carried would leave it
// on the shape this build no longer reads at all.
func (s *Service) Migrate(ctx context.Context, req MigrateRequest) (Applied, error) {
	if req.Transform == nil {
		return Applied{}, fmt.Errorf("configapi: a migration needs a transform")
	}
	d := draft{
		expect: req.Expect, requireActive: true, by: req.By,
		rules: runnableRules,
		build: func(b base) (*config.Company, []byte, error) {
			tree, err := decodeTree(b.document)
			if err != nil {
				return nil, nil, fmt.Errorf("configapi: decode the active revision: %w", err)
			}
			root, _ := tree.(map[string]any)
			if root == nil {
				return nil, nil, fmt.Errorf("configapi: the active revision is not a document")
			}
			changed, err := req.Transform(root)
			if err != nil {
				return nil, nil, err
			}
			if !changed {
				return nil, nil, ErrNothingToMigrate
			}
			document, err := json.Marshal(root)
			if err != nil {
				return nil, nil, fmt.Errorf("configapi: encode the migrated config: %w", err)
			}
			company, err := config.DecodeCompany(document)
			if err != nil {
				return nil, nil, fmt.Errorf("configapi: decode the migrated config: %w", err)
			}
			return company, document, nil
		},
	}
	// THE BASE IS THE REVISION THE TRANSFORM READ, whatever the pointer
	// names by the time it is stored: [Service.prepare] reads it, and
	// [Service.commit] names it in the compare-and-set.
	prepared, err := s.prepare(ctx, d)
	if err != nil {
		return Applied{}, err
	}
	return s.commit(ctx, prepared, req.Summary)
}

// StoredDocument is the fleet's revision as stored, decoded as a tree (every
// number as the text it was stored as), UNREDACTED, and the revision it is:
// what a migration plans from before it writes ([Service.Migrate]).
// [ErrNoActiveRevision] when the fleet runs no company. For an in-process
// caller; never answered to a request.
func (s *Service) StoredDocument(ctx context.Context) (map[string]any, string, error) {
	cur, found, err := s.current(ctx)
	if err != nil {
		return nil, "", err
	}
	if !found {
		return nil, "", ErrNoActiveRevision
	}
	document, _, err := s.openDocument(cur)
	if err != nil {
		return nil, "", fmt.Errorf("configapi: open the active revision: %w", err)
	}
	tree, err := decodeTree(document)
	if err != nil {
		return nil, "", fmt.Errorf("configapi: decode the active revision: %w", err)
	}
	root, _ := tree.(map[string]any)
	if root == nil {
		return nil, "", fmt.Errorf("configapi: the active revision is not a document")
	}
	return root, cur.revision.ID, nil
}

// Managed reports a deployment whose company document only named systems
// write (`api.auth.company_writers`): an operator rendering it from its
// custom resources, typically. Every surface that writes the document answers
// such a deployment read-only, and says where a change is made instead.
func (s *Service) Managed() bool { return len(s.writers.CompanyWriters) > 0 }
