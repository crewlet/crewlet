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
//
// # Two halves: prepare, then commit
//
// Every write on this surface is the same two steps, and they are separate
// functions because a DRY RUN is the first without the second. [Service.prepare]
// reads the active revision, checks what the caller said it was building on,
// opens the stored document, builds the proposed one from it, validates it
// and derives what an answer reports: its warnings and its hierarchy.
// Nothing is stored. [Service.commit] seals, stores and activates exactly the
// bytes prepare produced. A check that ran a copy of the first half would
// validate a different write from the one a save sends, which is the one
// thing a dry run must never do.

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

	// Warnings is what the engine will run but a person should know about
	// the revision: references that resolve to nothing, and admission rules
	// a re-activated stored company still breaks. Never nil.
	Warnings []config.Warning

	// Derived is the hierarchy the engine derives from the revision.
	Derived config.Derived
}

// ErrNoControlPlane reports a process that can store a revision but cannot
// point the fleet at one, so a write here would take effect nowhere.
var ErrNoControlPlane = errors.New("configapi: no control plane on this process")

// errNoStore reports a call on a service a node without a store never built.
var errNoStore = errors.New("configapi: no store on this node")

// RacedError reports that the active revision moved under the caller.
//
// Carries both sides, because the recovery needs them: the caller re-reads
// Current and re-derives the edit. Stored names the revision that WAS written
// and left inert, which is the operator's work surviving as history rather
// than being unwound by a second write that can itself fail.
type RacedError struct {
	// Base is the revision the caller built on, empty when it built on
	// nothing because nothing was active.
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

// DocumentError reports a submitted document this build could not read: it
// is not YAML or JSON, or its shape is not a company's. Distinct from a
// validation failure, because there is no company to validate.
type DocumentError struct{ Err error }

func (e *DocumentError) Error() string { return "configapi: " + e.Err.Error() }
func (e *DocumentError) Unwrap() error { return e.Err }

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
type ValidationError struct {
	Err error

	// Derived is the hierarchy the refused document derives. A document
	// with problems still has one, and a person fixing those problems needs
	// to see it: a misspelled lead is easier to find in the chart it breaks.
	Derived config.Derived
}

func (e *ValidationError) Error() string { return "configapi: " + e.Err.Error() }
func (e *ValidationError) Unwrap() error { return e.Err }

// RefusalFields is the structured half of a refused configuration document,
// for a surface answering the refusal in its own words: the problems it
// breaks, located and classified ([config.Problems]), and the hierarchy the
// engine derived from it when it parsed far enough to have one.
//
// ONE MAPPING for every surface a document refusal reaches, so a dashboard
// placing problems on the chart reads the same shape from /config and from
// /setup. Nil for an error that is not about a document.
func RefusalFields(err error) map[string]any {
	var invalid *ValidationError
	var patchErr *PatchError
	var docErr *DocumentError
	switch {
	case errors.As(err, &invalid):
		return map[string]any{"problems": config.Problems(invalid.Err), "derived": invalid.Derived}
	case errors.As(err, &patchErr):
		return map[string]any{"problems": config.Problems(patchErr.Err)}
	case errors.As(err, &docErr):
		return map[string]any{"problems": config.Problems(docErr.Err)}
	}
	return nil
}

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
	//
	// EITHER SPELLING. A bare revision id is what a programmatic caller
	// holds; a quoted entity-tag, a comma-separated list of them, or `*` is
	// what arrives in an `If-Match` header, and a caller that forwards one
	// is doing the standard thing rather than the wrong thing.
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
	prepared, err := s.prepare(ctx, patchDraft(req))
	if err != nil {
		return Applied{}, err
	}
	return s.commit(ctx, prepared, req.Summary, req.Operator)
}

// base is the active revision a write is built on, opened.
type base struct {
	revision store.Revision
	found    bool
	// document is the revision's stored bytes, unsealed; nil when nothing
	// is active.
	document []byte
	// prior is document decoded, held to no rule: it is where the masks a
	// redacted read handed back are restored from. Nil when nothing is
	// active.
	prior *config.Company
}

// draft is one kind of write: what the caller built it on, how it turns the
// base into the document it proposes, and which rules that document answers
// to.
type draft struct {
	// expect is the revision the caller said it built on, in any spelling
	// [matchesTag] reads; empty for none.
	expect string
	// requireActive refuses the write with [ErrNoActiveRevision] when
	// nothing is active: an edit is defined against a document.
	requireActive bool
	// expectAbsent refuses the write with a [RacedError] when a revision is
	// active: the caller built it on nothing.
	expectAbsent bool
	// build returns the company the write proposes and the bytes that
	// carry it, which are what is stored.
	build func(base) (*config.Company, []byte, error)
	// rules is what the proposed company is held to.
	rules func(*config.Company) error
}

// prepared is a write built and checked, and not yet stored: everything a dry
// run answers, and everything [Service.commit] needs.
//
// Only [Service.prepare] makes one, so a revision cannot be stored that was
// not read, built, validated and checked for a control plane first.
type prepared struct {
	// base is the active revision the write was built on, empty when
	// nothing was active.
	base     string
	document []byte
	warnings []config.Warning
	derived  config.Derived
}

// prepare builds and checks a write, storing nothing.
//
// # In this order, and each step for a reason
//
//   - The active revision, and what the caller built on. A stale base is
//     refused before any work, because nothing built on it can be kept.
//   - The control plane. A process that cannot activate refuses BEFORE it
//     validates, so a dry run on it answers exactly what the write would,
//     503 rather than a clean check for a write that cannot land.
//   - The stored document, opened and held to no rule: it is the merge base
//     and the prior masks are restored from, and a revision this build
//     would refuse must stay replaceable by the write that corrects it.
//   - The proposal, built by the draft.
//   - Its rules, and what an answer reports about it.
func (s *Service) prepare(ctx context.Context, d draft) (*prepared, error) {
	if s == nil {
		return nil, errNoStore
	}
	active, found, err := s.configs.Active(ctx)
	if err != nil {
		return nil, fmt.Errorf("configapi: read the active revision: %w", err)
	}
	switch {
	case d.requireActive && !found:
		return nil, ErrNoActiveRevision
	case d.expectAbsent && found:
		return nil, &RacedError{Current: active.ID}
	case d.expect != "" && (!found || !matchesTag(d.expect, etagOf(active))):
		// THE SAME RULE THE HTTP GUARD USES, because callers hand this the
		// same strings. `/setup`'s force-disconnect passes the raw
		// `If-Match` header straight through, and a correctly-formed
		// entity-tag (quoted, which is the only legal spelling) never
		// equals a bare revision id: a caller doing the standard thing was
		// answered 409 with their own current revision named as the
		// conflict.
		return nil, &RacedError{Base: d.expect, Current: active.ID}
	}
	if s.plane == nil {
		// HERE RATHER THAN IN EACH CALLER: this is the one step every
		// write on this surface passes through, so a new caller cannot
		// forget it. Storing a revision this process cannot point the fleet
		// at would report success for a change that takes effect nowhere.
		return nil, ErrNoControlPlane
	}

	b := base{revision: active, found: found}
	if found {
		// THE STORED BYTES, never a re-marshal of them. A rolling upgrade
		// puts two builds on one coordination store, and the activation
		// pointer carries the payload, so an older node holds, byte for
		// byte, a document a NEWER peer wrote. Decoding that into this
		// build's struct and marshalling it back silently drops every field
		// this build does not know, and the older node then publishes the
		// result as the fleet's configuration.
		if b.document, b.prior, err = s.openDocument(active); err != nil {
			return nil, fmt.Errorf("configapi: open the active revision: %w", err)
		}
	}
	company, document, err := d.build(b)
	if err != nil {
		return nil, err
	}
	// DERIVED BEFORE IT IS JUDGED, because a refusal carries it too: a
	// document with problems still has a hierarchy.
	derived := config.Derive(company)
	if invalid := d.rules(company); invalid != nil {
		return nil, &ValidationError{Err: invalid, Derived: derived}
	}
	p := &prepared{document: document, warnings: company.Warnings(), derived: derived}
	if found {
		p.base = active.ID
	}
	return p, nil
}

// commit seals, stores and points the fleet at a prepared write.
//
// Stored FIRST, then pointed at: a crash between the two leaves a revision
// nothing points at, which is inert and recoverable with `crewlet config
// activate <id>`, where the other order would point the fleet at a revision no
// node can read.
//
// The plane is there: [Service.prepare] refused the write otherwise, and a
// prepared write comes from nowhere else.
func (s *Service) commit(ctx context.Context, p *prepared, summary, operator string) (Applied, error) {
	payload, err := secrets.Seal(s.cipher, p.document)
	if err != nil {
		return Applied{}, fmt.Errorf("configapi: seal the config: %w", err)
	}
	at := s.now()
	id, err := s.configs.InsertActive(ctx, store.Revision{
		ParentID: p.base, Source: "api", CreatedBy: operator,
		Summary: summary, Payload: payload, CreatedAt: at,
	})
	if err != nil {
		return Applied{}, fmt.Errorf("configapi: store the config: %w", err)
	}
	published, err := s.plane.Activate(ctx, coord.ActivationRequest{
		RevisionID: id, Summary: summary, Payload: payload, At: at, Expect: p.base,
		// A WRITE BUILT ON NOTHING SAYS SO. Every write here names what it
		// was built on as the activation's expectation, and a write with no
		// base was built on an empty store: on a node that has not caught up
		// with its fleet that is not "the first company", it is a company
		// nobody has seen replacing the one the fleet is running. The
		// create-only compare-and-set refuses it; on a genuinely
		// unconfigured fleet there is no pointer and it lands.
		ExpectAbsent: p.base == "",
	})
	if errors.Is(err, coord.ErrActivationRaced) {
		// THE REVISION IS KEPT, not unwound: stored, valid and inert, so
		// the operator's work survives as history they can revert to,
		// and this node's reconciler adopts whichever revision won.
		log.InfoContext(ctx, "config_activation_raced",
			"revision", id, "expected", p.base, "by", operator)
		raced := &RacedError{Base: p.base, Stored: id}
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
	return Applied{
		RevisionID: id, Epoch: published.Epoch, Parent: p.base,
		Warnings: p.warnings, Derived: p.derived,
	}, nil
}

// patchDraft is a JSON Merge Patch over the active document.
func patchDraft(req ApplyRequest) draft {
	return draft{
		expect: req.Expect, requireActive: true,
		rules: (*config.Company).Validate,
		build: func(b base) (*config.Company, []byte, error) {
			if len(req.Patch) == 0 {
				return nil, nil, &PatchError{Err: errEmptyPatch}
			}
			merged, err := applyMergePatch(b.document, req.Patch)
			if err != nil {
				return nil, nil, &PatchError{Err: err}
			}
			incoming, err := readPatched(req.Patch, merged)
			if err != nil {
				return nil, nil, &PatchError{Err: err}
			}
			incoming.RestoreRedacted(b.prior)
			// AND THE BYTES ARE WHAT IS STORED, for the same reason the
			// merge base is. Everything above worked on the full document;
			// encoding `incoming` alone would undo it at the last step.
			// Restoring the redacted values is the only thing that changed
			// the struct after the merge, so its own encoding is merged
			// BACK OVER the document, which writes the fields this build
			// knows and leaves untouched the ones it does not.
			restored, err := json.Marshal(incoming)
			if err != nil {
				return nil, nil, fmt.Errorf("configapi: encode the merged config: %w", err)
			}
			final, err := applyMergePatch(merged, restored)
			if err != nil {
				return nil, nil, fmt.Errorf("configapi: restore the merged config: %w", err)
			}
			// AND THE ARRAYS THAT MERGE REPLACED keep the fields this build
			// cannot represent: see unknown.go.
			if final, err = carryUnknown(b.document, final); err != nil {
				return nil, nil, err
			}
			return incoming, final, nil
		},
	}
}

// replaceDraft is a whole document replacing whatever is active.
//
// built says what the caller read before sending it: the active revision's id,
// or empty for nothing, which refuses the write if a revision has appeared
// since. A full replacement built on nothing and landing on something would
// discard a company its author never saw.
func replaceDraft(incoming *config.Company, built string) draft {
	return draft{
		expect: built, expectAbsent: built == "",
		rules: (*config.Company).Validate,
		build: func(b base) (*config.Company, []byte, error) {
			if b.found {
				// The masks the caller was shown come back as the values
				// they hide. Without this, a reader who fetched the config,
				// changed one line and sent it back would replace every
				// credential in the company with the mask. VALIDATED AFTER
				// the restore, so a masked credential is judged as the value
				// it stands for.
				incoming.RestoreRedacted(b.prior)
			}
			document, err := json.Marshal(incoming)
			if err != nil {
				return nil, nil, fmt.Errorf("configapi: encode the config: %w", err)
			}
			if b.found {
				// A full replacement still keeps what this build cannot
				// represent: nobody sending it through this build could have
				// named such a field, so nobody meant to remove one. See
				// unknown.go.
				if document, err = carryUnknown(b.document, document); err != nil {
					return nil, nil, err
				}
			}
			return incoming, document, nil
		},
	}
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
		// merged document itself and is unchanged by any of this. It names
		// no line: the text a line would count is the engine's own merge,
		// which the caller never saw, rather than anything they sent.
		return nil, withoutLines(err)
	}
	// The strict reader found a key it does not know. Whose is it? Ask the
	// patch alone, and ONLY about its keys: it is a fragment, so nothing
	// else the authored reader decides about it is meaningful yet — a
	// redaction marker is restored after the merge, and a shape is judged
	// by Validate once it has been.
	if patchErr := onlyUnknownField(parseDocument(patch)); patchErr != nil {
		return nil, patchErr
	}
	// The stored-form reader, which holds the merged document to NO rule.
	// Validation happens exactly once, in prepare, after the masks are
	// restored. This line used to validate here as well, before the
	// restore, so on a document a newer peer had extended every PATCH that
	// carried a masked credential (any roles or units array read from GET
	// /config) was refused as an invalid patch naming the masks.
	return config.DecodeCompany(merged)
}

// withoutLines clears the line from every fault in err, for a failure found in
// text the caller did not write. Each fault's rendered message loses its
// trailing line with it, so a refusal's detail and its problems still agree.
func withoutLines(err error) error {
	var walk func(error)
	walk = func(e error) {
		switch e := e.(type) { //nolint:errorlint // Every fault in the tree, not the first one errors.As finds.
		case nil:
		case *config.Fault:
			e.Line = 0
		case interface{ Unwrap() []error }:
			for _, part := range e.Unwrap() {
				walk(part)
			}
		case interface{ Unwrap() error }:
			walk(e.Unwrap())
		}
	}
	walk(err)
	return err
}

// onlyUnknownField keeps an unknown-key refusal and discards every other
// complaint about a fragment.
func onlyUnknownField(_ *config.Company, err error) error {
	if err != nil && errors.Is(err, config.ErrUnknownField) {
		return err
	}
	return nil
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
	prepared, err := s.prepare(ctx, draft{
		requireActive: true,
		// VALIDATED, because a reload is an apply: every node rebuilds its
		// epoch from what this activates, and re-publishing a company this
		// build cannot run would move every node onto a refusal. The answer
		// names the field, and PUT or PATCH is how it is corrected.
		//
		// The RUNNABLE rules only. A reload is the credential-rotation
		// gesture, and refusing it for an admission rule the stored
		// document predates would make a rotation impossible until somebody
		// restructured the org. The answer's warnings name each one.
		rules: (*config.Company).ValidateRunnable,
		// THE STORED BYTES, UNCHANGED, which is what re-publishing the
		// active document means. Encoding this build's struct of it would
		// drop every field a newer peer wrote, on the gesture an operator
		// makes precisely because they changed nothing in the document.
		build: func(b base) (*config.Company, []byte, error) {
			return b.prior, b.document, nil
		},
	})
	if err != nil {
		return Applied{}, err
	}
	if summary == "" {
		summary = "reload configuration"
	}
	return s.commit(ctx, prepared, summary, operator)
}
