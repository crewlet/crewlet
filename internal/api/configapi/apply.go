package configapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/store"
)

// Writing a revision from inside this process rather than over HTTP.
//
// PATCH /config is the surface an operator drives, and it is the only place
// that knows how to merge onto the active document, restore the masks a
// redacted read handed back, validate the whole result and flip the
// activation pointer as a compare-and-set. Everything in this file is that
// same sequence, exported, so a second surface in this binary can perform a
// config write WITHOUT a second implementation of it.
//
// The alternative was a setup route that reached for the store and the plane
// itself. Two write paths onto one document is how one of them stops
// restoring redacted values, or stops validating, or starts overwriting a
// concurrent write — and the one that drifts is always the one nobody drives
// by hand.
//
// The HTTP handler is a caller like any other: it resolves its preconditions
// from headers, calls [Service.Apply], and maps the errors below onto the
// status codes it has always answered with.

// Applied is what a successful write produced.
type Applied struct {
	// RevisionID is the revision this write stored and activated.
	RevisionID string

	// Epoch is the coordination store's own version of the activation
	// pointer, monotonic across the fleet.
	Epoch int64

	// Parent is the revision the edit was built on, empty on a first
	// import.
	Parent string
}

// ErrNoControlPlane reports a process that can store a revision but cannot
// point the fleet at one, so a write here would take effect nowhere.
var ErrNoControlPlane = errors.New("configapi: no control plane on this process")

// RacedError reports that the active revision moved under the caller.
//
// Carries both sides, because the recovery needs them: the caller re-reads
// Current and re-derives the edit. Stored names the revision that WAS written
// and left inert, which is the operator's work surviving as history rather
// than being unwound by a second write that can itself fail.
type RacedError struct {
	// Base is the revision the caller built on.
	Base string
	// Current is the revision that won, empty when it could not be read.
	Current string
	// Stored is the inert revision this write left behind, empty when the
	// race was detected before anything was stored.
	Stored string
}

func (e *RacedError) Error() string {
	return fmt.Sprintf("configapi: the active revision advanced from %q to %q", e.Base, e.Current)
}

// PatchError reports a merge patch that could not be applied, or that
// produced a document the strict reader refuses. Distinct from a validation
// failure: the document never came into existence.
type PatchError struct{ Err error }

func (e *PatchError) Error() string { return "configapi: " + e.Err.Error() }
func (e *PatchError) Unwrap() error { return e.Err }

// ValidationError reports a document that parsed and is not a valid company.
//
// A patch is validated as the WHOLE document it produces, so a section that
// is fine on its own is still refused when it leaves the company invalid.
type ValidationError struct{ Err error }

func (e *ValidationError) Error() string { return "configapi: " + e.Err.Error() }
func (e *ValidationError) Unwrap() error { return e.Err }

// ApplyRequest is one merge-patch write.
type ApplyRequest struct {
	// Patch is an RFC 7396 JSON Merge Patch over the ACTIVE document.
	Patch []byte

	// Summary is the audit sentence stored on the revision. Required: a
	// patch is the change least visible in a diff, so the sentence saying
	// what it was for matters most here.
	Summary string

	// Operator is who the revision records as its author.
	Operator string

	// Expect is the revision the caller built this edit on. Empty is
	// unconditional, which is what a first import and a script that owns
	// the config outright both want. When set and stale the write is
	// refused with a [RacedError] BEFORE anything is stored, so a caller
	// working from a list it read a minute ago does not silently overwrite
	// what happened since.
	Expect string
}

// Apply merges a patch onto the active revision and activates the result.
//
// The whole sequence in one call, in the one order that is safe: read the
// active revision, merge in its STORED shape (normalised JSON, so the result
// does not depend on how the document was originally written), parse
// strictly so a typo is refused rather than ignored, restore the masks a
// redacted read handed back, validate the whole document, seal it, store it,
// then flip the pointer as a compare-and-set naming the parent.
func (s *Service) Apply(ctx context.Context, req ApplyRequest) (Applied, error) {
	if s == nil {
		return Applied{}, fmt.Errorf("configapi: no store on this node")
	}
	if len(req.Patch) == 0 {
		return Applied{}, &PatchError{Err: errEmptyPatch}
	}

	active, found, err := s.configs.Active(ctx)
	if err != nil {
		return Applied{}, fmt.Errorf("configapi: read the active revision: %w", err)
	}
	if !found {
		return Applied{}, ErrNoActiveRevision
	}
	if req.Expect != "" && req.Expect != active.ID {
		return Applied{}, &RacedError{Base: req.Expect, Current: active.ID}
	}

	// THE STORED BYTES ARE THE MERGE BASE, never a re-marshal of them.
	//
	// A rolling upgrade puts two builds on one coordination store, and the
	// activation pointer carries the payload — so an older node holds, byte
	// for byte, a document a NEWER peer wrote. Decoding that into this
	// build's struct and marshalling it back silently drops every field this
	// build does not know, and the older node then publishes the result as
	// the fleet's configuration. The newer peers reconcile onto it and lose
	// settings nobody edited.
	document, err := secrets.Open(s.cipher, active.Payload)
	if err != nil {
		return Applied{}, fmt.Errorf("configapi: open the active revision: %w", err)
	}
	prior, err := config.DecodeCompany(document)
	if err != nil {
		return Applied{}, fmt.Errorf("configapi: decode the active revision: %w", err)
	}
	merged, err := applyMergePatch(document, req.Patch)
	if err != nil {
		return Applied{}, &PatchError{Err: err}
	}
	incoming, err := readPatched(req.Patch, merged)
	if err != nil {
		return Applied{}, &PatchError{Err: err}
	}
	incoming.RestoreRedacted(prior)
	if err := incoming.Validate(); err != nil {
		return Applied{}, &ValidationError{Err: err}
	}
	// AND THE BYTES ARE WHAT IS STORED, for the same reason. Everything
	// above worked on the full document; encoding `incoming` alone would
	// undo it at the last step. Restoring the redacted values is the only
	// thing that changed the struct after the merge, so its own encoding is
	// merged BACK OVER the document — which writes the fields this build
	// knows and leaves untouched the ones it does not.
	restored, err := json.Marshal(incoming)
	if err != nil {
		return Applied{}, fmt.Errorf("configapi: encode the merged config: %w", err)
	}
	final, err := applyMergePatch(merged, restored)
	if err != nil {
		return Applied{}, fmt.Errorf("configapi: restore the merged config: %w", err)
	}
	return s.activateDocument(ctx, final, active.ID, req.Summary, req.Operator)
}

// readPatched reads a patched document, catching a typo the PATCH invented
// while tolerating a field a PEER wrote.
//
// Both readers already state this rule and this call had it backwards.
// [parseDocument]'s doc says its strictness "belongs here and not on the
// stored form: this is the door a person's document comes through, and a typo
// is a mistake to catch rather than a peer running a newer build."
// [config.DecodeCompany]'s says rejecting an unrecognised key in a stored
// revision "makes a mixed-version fleet an outage in the older direction."
//
// A merged document is BOTH at once: a person's words over bytes a peer may
// have written. Reading it strictly refused every config write on an older
// node the moment a newer one added a field; reading it leniently would
// swallow the typo the strict reader exists to catch. So the two questions are
// asked of the two inputs separately.
//
// THE PATCH IS ASKED ONLY ABOUT ITS KEYS. It is a fragment, so nothing else
// the authored reader decides about it is meaningful yet: a redaction marker
// is restored after the merge, and a shape is judged by Validate once it has
// been. Only [config.ErrUnknownField] is a fact about the patch alone.
func readPatched(patch, merged []byte) (*config.Company, error) {
	cfg, err := parseDocument(merged)
	switch {
	case err == nil:
		return cfg, nil
	case !errors.Is(err, config.ErrUnknownField):
		// Every other refusal the authored reader makes is about the
		// merged document itself and is unchanged by any of this.
		return nil, err
	}
	// The strict reader found a key it does not know. Whose is it? Ask the
	// patch alone, and ONLY about its keys: it is a fragment, so nothing
	// else the authored reader decides about it is meaningful yet — a
	// redaction marker is restored after the merge, and a shape is judged
	// by Validate once it has been.
	if patchErr := onlyUnknownField(parseDocument(patch)); patchErr != nil {
		return nil, patchErr
	}
	return config.DecodeCompany(merged)
}

// onlyUnknownField keeps an unknown-key refusal and discards every other
// complaint about a fragment.
func onlyUnknownField(_ *config.Company, err error) error {
	if err != nil && errors.Is(err, config.ErrUnknownField) {
		return err
	}
	return nil
}

// activate seals, stores and points the fleet at a document.
//
// The shared tail of every write on this surface. Stored FIRST, then pointed
// at: a crash between the two leaves a revision nothing points at, which is
// inert and recoverable with `crewlet config activate <id>`, where the other
// order would point the fleet at a revision no node can read.
func (s *Service) activate(
	ctx context.Context, company *config.Company, parent, summary, operator string,
) (Applied, error) {
	document, err := json.Marshal(company)
	if err != nil {
		return Applied{}, fmt.Errorf("configapi: encode the config: %w", err)
	}
	return s.activateDocument(ctx, document, parent, summary, operator)
}

// activateDocument is [Service.activate] for a caller that already holds the
// bytes to store.
//
// The two are not the same thing, and the difference is a rolling upgrade. A
// caller that built the document itself — a revert, a reload, a bootstrap —
// has bytes and a struct that agree by construction. A caller that MERGED one
// has bytes carrying fields this build cannot represent, and re-encoding its
// struct would drop exactly those.
func (s *Service) activateDocument(
	ctx context.Context, document []byte, parent, summary, operator string,
) (Applied, error) {
	if s.plane == nil {
		// REFUSED BEFORE ANYTHING IS STORED, and here rather than in each
		// caller: this is the one tail every write on this surface passes
		// through, so a new caller cannot forget it. Storing a revision
		// this process cannot point the fleet at would report success for
		// a change that takes effect nowhere.
		return Applied{}, ErrNoControlPlane
	}
	payload, err := secrets.Seal(s.cipher, document)
	if err != nil {
		return Applied{}, fmt.Errorf("configapi: seal the config: %w", err)
	}
	at := s.now()
	id, err := s.configs.InsertActive(ctx, store.Revision{
		ParentID: parent, Source: "api", CreatedBy: operator,
		Summary: summary, Payload: payload, CreatedAt: at,
	})
	if err != nil {
		return Applied{}, fmt.Errorf("configapi: store the config: %w", err)
	}
	published, err := s.plane.Activate(ctx, coord.ActivationRequest{
		RevisionID: id, Summary: summary, Payload: payload, At: at, Expect: parent,
	})
	if errors.Is(err, coord.ErrActivationRaced) {
		// THE REVISION IS KEPT, not unwound: stored, valid and inert, so
		// the operator's work survives as history they can revert to,
		// and this node's reconciler adopts whichever revision won.
		log.InfoContext(ctx, "config_activation_raced",
			"revision", id, "expected", parent, "by", operator)
		raced := &RacedError{Base: parent, Stored: id}
		if current, _, terr := s.plane.Target(ctx); terr == nil {
			raced.Current = current.RevisionID
		}
		return Applied{}, raced
	}
	if err != nil {
		return Applied{}, fmt.Errorf("configapi: activate the config: %w", err)
	}
	s.nudge(ctx, id, summary, operator)
	log.InfoContext(ctx, "config_revision_written",
		"revision", id, "epoch", published.Epoch, "by", operator, "summary", summary)
	return Applied{RevisionID: id, Epoch: published.Epoch, Parent: parent}, nil
}

// Reload re-publishes the ACTIVE document unchanged, as a new revision.
//
// The gesture a rotated SECRET needs. A `${VAR}` in the company config is
// resolved when a provider or a transport is constructed, from a snapshot
// taken at apply time, so writing a new value into the secret store changes
// nothing in a running process: the pointer in the document is already
// correct, so there is no patch to make, and with no activation there is no
// apply and no refreshed snapshot.
//
// Re-activating an UNCHANGED revision is what the control plane calls the
// credential-rotation gesture, and it is exactly why the activation pointer
// is append-only rather than keyed on a revision id: a pointer that
// deduplicated would rebuild nothing on precisely this operation.
//
// A NEW revision rather than a re-pointed old one, for the same reason revert
// writes one: the history stays append-only, so "the credentials were
// reloaded at 04:12" is a fact somebody can find later.
func (s *Service) Reload(ctx context.Context, summary, operator string) (Applied, error) {
	if s == nil {
		return Applied{}, fmt.Errorf("configapi: no store on this node")
	}
	active, found, err := s.configs.Active(ctx)
	if err != nil {
		return Applied{}, fmt.Errorf("configapi: read the active revision: %w", err)
	}
	if !found {
		return Applied{}, ErrNoActiveRevision
	}
	// OPENED, not copied: a revision sealed under a key no longer in the
	// keyring cannot be reloaded, and finding that out now beats
	// activating a document every node will fail to read.
	company, err := s.open(active)
	if err != nil {
		return Applied{}, fmt.Errorf("configapi: open the active revision: %w", err)
	}
	if summary == "" {
		summary = "reload configuration"
	}
	return s.activate(ctx, company, active.ID, summary, operator)
}
