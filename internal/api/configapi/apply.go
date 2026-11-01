package configapi

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
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
// reads the fleet's revision ([Service.current]), checks what the caller said
// it was building on,
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

	// Derived is the hierarchy the engine derives from the revision: every
	// seat's handle, unit and agent id, and every unit's key, as the
	// running company will see them.
	Derived config.Derived
}

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
	// Node is the node that stored it ([Options.Node]), set exactly when
	// Stored is: a revision that lost its activation is a row of that node's
	// history and no other's, so it is diffed and reverted there alone
	// ([KeptRevision]).
	Node string
	// Stated is a write whose request named Base in its If-Match header
	// ([ApplyRequest.Stated]): what it lost is a precondition it stated.
	// Never set by a write whose If-Match was another tag (the charter's, an
	// entity's), a write that reads none (a reload, a revert), a base sent as
	// a body field, or `If-Match: *`, which names no revision.
	Stated bool
}

// Status is the status a [RacedError] is refused with: 412 where the request
// stated the revision it lost in its If-Match header ([RacedError.Stated]),
// and 409 where it stated none.
//
// HTTP'S RULE, the one every surface here answers a moved version by (RFC
// 9110 §13.1.1): a condition the request stated as a header that turned out
// false is a FAILED PRECONDITION, whether it was false when the request
// arrived or became false before the activation's compare-and-set landed;
// either way the server did not perform what was conditioned on it. A write
// that named no revision lost a race it never declared a condition for,
// which is a conflict. The code is `revision_advanced` either way, because
// what moved is the company's revision; crewlet-operator and the dashboard
// read the code, and the status says only which kind of request lost.
//
// DECIDED BY WHAT WAS JUDGED, carried on the error from the precondition
// check, never by reading the request again: a route whose If-Match is
// another resource's tag, or that reads none, would otherwise answer a stray
// header's revision as a precondition it never evaluated.
func (e *RacedError) Status() int {
	if e.Stated {
		return http.StatusPreconditionFailed
	}
	return http.StatusConflict
}

func (e *RacedError) Error() string {
	return fmt.Sprintf("configapi: the active revision advanced from %q to %q", e.Base, e.Current)
}

// ActivationUnknownError reports a write that was STORED and whose activation
// this node cannot account for: the coordination store did not answer it
// ([coord.ErrUnavailable]), and a publish whose acknowledgement was lost may
// still have moved the pointer.
//
// Its own error, apart from a pointer that could not be read before anything
// was stored, because the two send an operator opposite ways: that one wrote
// nothing and is sent again; this one may be the company already, and sent
// again it makes a second revision and a second epoch over a write the operator
// was told never happened.
type ActivationUnknownError struct {
	// Base is the revision the write was built on, empty when it was built on
	// nothing.
	Base string
	// Stored is the revision the write stored.
	Stored string
	// Node is the node that stored it ([Options.Node]), whose history alone
	// holds Stored until an activation names it: a revision the pointer does
	// not name is no other node's to diff or revert, and answers 404 there.
	Node string
	Err  error
}

func (e *ActivationUnknownError) Error() string {
	return fmt.Sprintf("configapi: revision %s was stored on node %s and its activation "+
		"was not answered: %v", e.Stored, e.Node, e.Err)
}

func (e *ActivationUnknownError) Unwrap() error { return e.Err }

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
	// can be refused for one seat and still show a person where every
	// other seat lands, and the problems are placed on that hierarchy.
	Derived config.Derived
}

func (e *ValidationError) Error() string { return "configapi: " + e.Err.Error() }
func (e *ValidationError) Unwrap() error { return e.Err }

// RefusalFields is the structured half of a refused configuration document,
// for a surface answering the refusal in its own words: the problems it
// breaks, located and classified ([config.Problems]), and — for a document the
// engine parsed far enough to have one — the hierarchy it derived.
//
// ONE MAPPING for every surface a document refusal reaches, so a dashboard
// placing problems on the chart reads the same shape from /config and from
// /setup.
//
// EMPTY, NEVER NIL, for an error that is not about a document. Every caller
// adds its own words to what comes back (a detail, a hint, an id), and a nil
// map is the one value in Go that reads fine and panics on the write. The
// caller that needs to know whether there was a structured half reads its
// length.
func RefusalFields(err error) map[string]any {
	var invalid *ValidationError
	var patchErr *PatchError
	var docErr *DocumentError
	switch {
	case errors.As(err, &invalid):
		return map[string]any{"problems": config.Problems(invalid.Err),
			"derived": invalid.Derived}
	case errors.As(err, &patchErr):
		return map[string]any{"problems": config.Problems(patchErr.Err)}
	case errors.As(err, &docErr):
		return map[string]any{"problems": config.Problems(docErr.Err)}
	}
	return map[string]any{}
}

// ApplyRequest is one merge-patch write.
type ApplyRequest struct {
	// Patch is an RFC 7396 JSON Merge Patch over the ACTIVE document.
	Patch []byte

	// Summary is the audit sentence stored on the revision. Required: a
	// patch is the change least visible in a diff, so the sentence saying
	// what it was for matters most here.
	Summary string

	// By is who the revision records as its author: the name, the kind and
	// the credential it was written through, [iam.ActorFor]'s whole answer
	// for the party — or the engine's own name for a write a timer made.
	By iam.Actor

	// Engine marks the engine's own write ([draft].engine), which may change
	// a field only the engine writes.
	Engine bool

	// Expect is the revision the caller built this edit on. Empty states no
	// precondition, which is what a script that owns the config outright
	// wants: the edit is built on the fleet's revision as this call reads
	// it, and still lands only while the pointer names that. When set and
	// stale the write is refused with a [RacedError] BEFORE anything is
	// stored, so a caller working from a list it read a minute ago does not
	// silently overwrite what happened since.
	//
	// EITHER SPELLING. A bare revision id is what a programmatic caller
	// holds; a quoted entity-tag, a comma-separated list of them, or `*` is
	// what arrives in an `If-Match` header, and a caller that forwards one
	// is doing the standard thing rather than the wrong thing.
	Expect string

	// Stated is an Expect the caller's own request named in its If-Match
	// header (`/setup`'s forced disconnect forwards it, PATCH /config judged
	// it): a write that then loses its race is refused 412 rather than 409
	// ([RacedError.Status]). `*` names no revision and is never stated.
	Stated bool
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
	prepared, err := s.prepare(ctx, patchDraft(req, nil))
	if err != nil {
		return Applied{}, err
	}
	return s.commit(ctx, prepared, req.Summary)
}

// base is the active revision a write is built on, opened.
type base struct {
	revision store.Revision
	found    bool
	// document is the revision's stored bytes, opened; nil when nothing is
	// active, and when the revision is stored without a seal
	// ([base.unsealed]).
	document []byte
	// prior is document decoded, held to no rule: it is where the masks a
	// redacted read handed back are restored from. Nil when nothing is
	// active, and when the revision is stored without a seal
	// ([base.unsealed]).
	prior *config.Company
	// unsealed reports an active revision stored without a seal, which a
	// whole document replaces having read NOTHING from it
	// ([draft.wholeDocument]): document and prior stay nil, so no mask is
	// restored from it and no field carried from it, the write is admitted
	// and judged as one replacing nothing, and every seat somebody holds
	// that the new document drops is refused (seatheld.go). It is still the
	// base: the revision the write's preconditions and its activation's
	// compare-and-set are judged against. See [Service.prepare].
	unsealed bool
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
	// rules is what the proposed company is held to, beside the revision it
	// replaces (nil when nothing is active): [authoredRules] or
	// [runnableRules].
	rules func(next, prior *config.Company) error
	// source is what the revision records as where it came from: empty for
	// [revisionSource], or [importSource] for an import.
	source string
	// principal is who asked for the write, which is admitted on what it
	// changes (admission.go); nil for the engine's own writes, which no
	// request made.
	principal *iam.Principal
	// addressedCheck is a dry run of one seat or unit at an address
	// [Service.mayReach] admitted the caller to, which is answered even when
	// it changes nothing (admission.go).
	addressedCheck bool
	// charter is a write of the charter alone ([Service.putCharter]), which
	// is admitted on the principles' own verb while what it changes is the
	// charter's settings and nothing else (admission.go).
	charter bool
	// by is who the write is made as: the credential [Service.Authorize]
	// judges, and the author the revision records — one value for both, so
	// the party a write was admitted as is the party it is stored under.
	by iam.Actor
	// stated is an expect the request named in its If-Match header
	// ([RacedError.Stated]).
	stated bool
	// warnings derives warnings the write itself carries beside the
	// document's own ([Company.Warnings]): a removal's resolution moves.
	// Nil for none.
	warnings func(next, prior *config.Company) []config.Warning
	// authored is a write a caller composed (a patch, a whole document, an
	// entity, a removal), held to every rule an author is, and engine the
	// engine's own write of one: an installation it discovered, an app it
	// recorded, a block a disconnect removed. Only an authored write that is
	// not the engine's is refused a change to a field the engine alone
	// writes ([config.RefuseEngineWritten]).
	authored, engine bool
	// republishes is a write that re-publishes the active document's
	// stored bytes unchanged ([Service.Reload]). It changes nothing about
	// the company, so a managed document does not refuse it: see
	// [Service.Authorize].
	republishes bool
	// wholeDocument is a write that can be built without the active
	// revision's document: a whole document replacing it ([replaceDraft]),
	// which restores masks and carries unknown fields from that document
	// only when it opens. It alone goes on over an active revision stored
	// without a seal, reading nothing from it ([base.unsealed]). Every
	// other write is built from the active document (a patch merges onto
	// it, an entity write splices into it, the charter's write edits it, a
	// reload re-publishes it, a revert is judged on what it changes from
	// it), so over a revision nothing can read it is refused as every
	// reader of that revision is ([RefuseUnreadable]).
	wholeDocument bool
}

// prepared is a write built and checked, and not yet stored: everything a dry
// run answers, and everything [Service.commit] needs.
//
// Only [Service.prepare] makes one, so a revision cannot be stored that was
// not read, built, validated and checked for a control plane first.
type prepared struct {
	// base is the fleet's revision the write was built on, zero when
	// nothing was configured, and pointed whether the fleet's pointer named
	// it: what the compare-and-set expects is the base when it did, and no
	// pointer at all when it did not.
	base    store.Revision
	pointed bool
	// baseUnsealed reports a base stored without a seal ([base.unsealed]),
	// and baseFleet one read off the pointer that this node holds no row
	// for ([fleetRevision.fleet]): a base that is both is never kept in this
	// node's history ([Service.commit]).
	baseUnsealed bool
	baseFleet    bool
	document     []byte
	warnings     []config.Warning
	// held are the warnings the active revision already carries, by
	// [warningKey]: what a dry run tells apart from the ones this write
	// introduces ([checkedWarnings]).
	held    map[warningKey]bool
	derived config.Derived
	// by is the draft's party, carried rather than passed again to
	// [Service.commit], so the author stored is the one prepare judged.
	by iam.Actor
	// source is the draft's, resolved: what the revision records.
	source string
	// stated is the draft's: a race this write loses at the compare-and-set
	// is a precondition its request stated ([RacedError.Stated]).
	stated bool
	// company is the proposed document, decoded: what [Service.CheckEntity]
	// hands a caller testing a draft entry.
	company *config.Company
}

// prepare builds and checks a write, storing nothing.
//
// # In this order, and each step for a reason
//
//   - Whether the CREDENTIAL may change a managed document at all
//     (managed.go), before anything is read: it is a question about who
//     is asking and never about what they sent.
//   - The fleet's revision ([Service.current]), and what the caller built on.
//     A stale base is refused before any work, because nothing built on it
//     can be kept.
//   - The stored document, opened and held to no rule: it is the merge base
//     and the prior masks are restored from, and a revision this build
//     would refuse must stay replaceable by the write that corrects it. One
//     stored WITHOUT A SEAL is replaced by a whole document and read by
//     nothing ([base.unsealed]); every other write is refused over it.
//   - The proposal, built by the draft.
//   - Whether the caller may make it: admin, or a lead inside
//     their own subtree (admission.go). The engine's own writes are not asked.
//   - Its hierarchy, DERIVED BEFORE IT IS JUDGED, because a refusal carries
//     it too.
//   - Its rules, and what an answer reports about it.
//   - Whether it takes a human seat away from somebody bound to it or an
//     open invitation holding it, which is the one step that reads outside
//     this node (seatheld.go).
func (s *Service) prepare(ctx context.Context, d draft) (*prepared, error) {
	// WHO MAY CHANGE THE DOCUMENT, before this reads or stores anything: a
	// credential a managed deployment does not let write is refused, and a
	// dry run is held to it too, because a check that answered "valid" for a
	// write the save would refuse is the one thing a dry run must never do.
	// The HTTP routes have already asked, before they read the request
	// ([Service.refusedManaged]); this is the rule for every other caller.
	if !d.republishes {
		if err := s.Authorize(d.by); err != nil {
			return nil, err
		}
	}
	cur, found, err := s.current(ctx)
	if err != nil {
		return nil, err
	}
	active := cur.revision
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
		return nil, &RacedError{Base: d.expect, Current: active.ID, Stated: d.stated}
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
		b.document, b.prior, err = s.openDocument(cur)
		var unreadable *UnreadableRevisionError
		switch {
		case err == nil:
		case d.wholeDocument && errors.As(err, &unreadable) && unreadable.Unsealed():
			// AN UNSEALED ACTIVE REVISION IS REPLACED, NEVER READ, which is
			// the offline import's rule (cmd/crewlet's importConfig) held
			// online: the refusal every reader answers it with names a
			// whole document sent with PUT /config as its remedy
			// ([UnreadableRevisionError.Remedy]), and refusing that too
			// left a fleet whose active revision nothing could replace
			// without stopping a node. Nothing vouches for what its body
			// says, so nothing is taken from it: see [base.unsealed].
			//
			// ONLY UNSEALED. A revision SEALED is somebody's document
			// whether or not this node can open it: under a key this
			// keyring does not hold, which putting the key back recovers
			// whole, or in a form this build does not read, which the
			// newer build that sealed it reads. Replaced here, every
			// credential and field in it would be lost to a write that
			// could not read them.
			b.unsealed = true
		default:
			return nil, fmt.Errorf("configapi: open the active revision: %w", err)
		}
	}
	company, document, err := d.build(b)
	if err != nil {
		return nil, err
	}
	// WHO MAY MAKE IT, BEFORE ANYTHING IS SAID ABOUT IT: a refusal names what
	// the caller may not change, rather than what is wrong with parts of the
	// company they cannot read.
	//
	// AGAINST b.prior, which is nil over an unsealed base: a write judged
	// against no document changes every setting and adds every seat and
	// unit, so only admin may replace a revision nobody can read, and a
	// lead's write is refused for reaching outside what they lead.
	if d.principal != nil {
		if err := admit(ctx, *d.principal, b.prior, company, d.addressedCheck,
			d.charter); err != nil {
			return nil, err
		}
	}
	derived := config.Derive(company)
	invalid := d.rules(company, b.prior)
	if b.unsealed {
		if masked := company.UnresolvedMasks(); len(masked) > 0 {
			invalid = errors.Join(unrestorableMasks(masked),
				config.WithoutFaultsAt(invalid, masked))
		}
	}
	if invalid != nil {
		return nil, &ValidationError{Err: invalid, Derived: derived}
	}
	// A FIELD ONLY THE ENGINE WRITES, changed by anybody else, is refused:
	// a GitHub App id written by a person or a model points an agent at an
	// app that is not its own. A document the operator writes is exempt:
	// it renders what the engine reported back to it.
	if d.authored && !d.engine && !s.Managed() {
		if refused := config.RefuseEngineWritten(b.prior, company); refused != nil {
			return nil, &ValidationError{Err: refused, Derived: derived}
		}
	}
	// A RAISE IS A PERSON'S (raise.go), asked of a request's write once its
	// rules hold, since judging it may ask a vendor what it offers.
	if d.principal != nil && !s.Managed() && !authz.Checking(ctx) {
		if err := s.admitRaises(ctx, *d.principal, b, company); err != nil {
			return nil, err
		}
	}
	// AFTER THE RULES, because a document that is refused anyway takes no
	// seat away, and it is the one step here that reads outside this node.
	if err := s.checkHeldSeats(ctx, b, company); err != nil {
		return nil, err
	}
	// AND WHAT IT DOES BESIDE THIS NODE'S TIER A ([Options.TierWarnings]),
	// in the answer that applies it: a person turning code.native on reads
	// the cross-tier consequence where they made the change, rather than in
	// a node's log.
	warnings := s.warnings(company)
	if d.warnings != nil {
		warnings = append(warnings, d.warnings(company, b.prior)...)
	}
	p := &prepared{
		company:  company,
		document: document, warnings: warnings, derived: derived, by: d.by,
		source: cmp.Or(d.source, revisionSource), pointed: cur.pointed,
		held: s.heldWarnings(b.prior), stated: d.stated,
	}
	if found {
		p.base, p.baseUnsealed, p.baseFleet = active, b.unsealed, cur.fleet
	}
	return p, nil
}

// warnings is everything a write's answer reports as VALID and worth knowing:
// the company's own ([config.Company.Warnings]) and what it does beside this
// node's Tier A ([Options.TierWarnings]).
func (s *Service) warnings(company *config.Company) []config.Warning {
	out := company.Warnings()
	if s.tierWarnings != nil {
		out = append(out, s.tierWarnings(company)...)
	}
	return out
}

// unrestorableMasks refuses the credentials of a document that still hold the
// redaction marker when the revision it replaces is stored without a seal, one
// fault per masked path.
//
// A mask is restored from the revision a write replaces, and that revision is
// read by nothing ([base.unsealed]), so every mask stands for a value nothing
// here holds. Said in these words IN PLACE OF the validator's own, which
// [Service.prepare] drops for the same paths ([config.WithoutFaultsAt]):
// [config.Company.Validate] refuses the same marker in words about members
// that are new or renamed, which sends whoever sent it looking for a rename
// they never made, when what it needs is the value itself. Every other fault
// is still reported beside these, as over a revision that opens.
func unrestorableMasks(masked []config.Path) error {
	faults := make([]error, 0, len(masked))
	for _, path := range masked {
		faults = append(faults, &config.Fault{
			Path: path, Kind: config.ErrUnknownValue,
			Detail: fmt.Sprintf("still holds the redaction marker %q. A masked "+
				"value is filled in from the active revision, and that one is "+
				"stored without a seal, so there's nothing to fill it from. Write "+
				"the real value or a ${VAR} reference here", config.Redacted),
		})
	}
	return errors.Join(faults...)
}

// commit seals, stores and points the fleet at a prepared write.
//
// Stored FIRST, then pointed at: a crash between the two leaves a revision
// nothing points at, which is inert and recoverable with `crewlet config
// activate <id>`, where the other order would point the fleet at a revision no
// node can read. Its base is kept first where this node holds no row for it — a
// fleet revision the reconciler has not ticked onto yet — because the parent is
// a row of this node's history ([store.Configs.InsertOn]); a base the pointer
// carried WITHOUT A SEAL is the one exception, never kept, and the write's row
// names no parent ([store.Configs.Insert]).
//
// STORED AS HISTORY, AND ONLY THAT. The pointer is the company, and what this
// node boots on is the reconciler's to move once its engine has applied the
// revision. A write that marked itself this node's own when the pointer moved
// raced the reconciler: one that had read the pointer a moment before walked it
// back, so a read after the write answered the revision before it, and the
// write's own `If-Match` was refused.
//
// THE COMPARE-AND-SET NAMES WHAT THE WRITE WAS BUILT ON: the base, while the
// pointer still names it; no pointer at all, when it read none. Never
// unconditional, so a write cannot land over one it never saw.
//
// The plane is there: [Service.prepare] refused the write otherwise, and a
// prepared write comes from nowhere else.
//
// BY IS THE AUTHOR AND THE CREDENTIAL BOTH, and the revision keeps both: a
// revision is kept for ever, and one that named only the credential — a
// person's machine token, `pat:<id>` — named something the identity sweep
// deletes a week after it lapses, and with it the only record of whose it was.
// It is the prepared write's own party rather than an argument, so the author
// a revision is stored under is the one [Service.Authorize] judged.
func (s *Service) commit(ctx context.Context, p *prepared, summary string) (Applied, error) {
	by := p.by
	payload, err := secrets.Seal(s.cipher, p.document)
	if err != nil {
		return Applied{}, fmt.Errorf("configapi: seal the config: %w", err)
	}
	at := s.now()
	r := store.Revision{
		ParentID: p.base.ID, Source: p.source, CreatedBy: by.Name,
		CreatedByKind: by.Kind, OperatorID: by.OperatorID,
		Summary: summary, Payload: payload, CreatedAt: at,
	}
	var id string
	if p.baseUnsealed && p.baseFleet {
		// A BODY THAT ARRIVED WITHOUT A SEAL IS NEVER KEPT, which is the
		// reconciler's rule too: it keeps a pointer's revision only once
		// this node's keyring opens it ([store.Configs.SawFleet]), because
		// the seal is what says a node of this fleet wrote it, and a write
		// replacing it is no reason to file a body from outside the fleet
		// in this node's history under the author its pointer claims. The
		// write is still built on it, so its activation is conditioned on
		// it and replaces exactly the revision that was read; only its row
		// here names no parent. This node's OWN unsealed copy is held
		// already and is named, as the offline import names it.
		r.ParentID = ""
		id, err = s.configs.Insert(ctx, r)
	} else {
		id, err = s.configs.InsertOn(ctx, p.base, r)
	}
	if err != nil {
		return Applied{}, fmt.Errorf("configapi: store the config: %w", err)
	}
	request := coord.ActivationRequest{
		RevisionID: id, Summary: summary, Payload: payload, At: at,
		// THE ORIGIN RIDES THE POINTER, so every node that adopts this
		// revision records who wrote it, through which credential, from
		// where and when — rather than itself, from `fleet`, at the
		// instant it happened to apply it.
		Origin: coord.RevisionOrigin{
			Author: by.Name, AuthorKind: by.Kind, OperatorID: by.OperatorID,
			Source: p.source, CreatedAt: at,
		},
	}
	if p.pointed {
		request.Expect = p.base.ID
	} else {
		// A WRITE BUILT WITH NO POINTER SAYS SO, whatever this node knew
		// the company to be: on a node that has not caught up with its
		// fleet, "no pointer as far as I know" is not "the first company",
		// and a write that replaced the one the fleet is running would
		// rename it and orphan every seat's memory. The create-only
		// compare-and-set refuses it; on a fleet that genuinely has no
		// pointer it lands.
		request.ExpectAbsent = true
	}
	published, err := s.plane.Activate(ctx, request)
	if errors.Is(err, coord.ErrActivationRaced) {
		// THE REVISION IS KEPT, not unwound: stored, valid and inert, so
		// the operator's work survives as history they can revert to,
		// while the fleet goes on running whichever revision won.
		log.InfoContext(ctx, "config_activation_raced",
			"revision", id, "expected", p.base.ID, "by", by.Name,
			"operator", by.OperatorID)
		raced := &RacedError{Base: p.base.ID, Stored: id, Node: s.node, Stated: p.stated}
		if current, _, terr := s.plane.Target(ctx); terr == nil {
			raced.Current = current.RevisionID
		}
		return Applied{}, raced
	}
	if errors.Is(err, coord.ErrUnavailable) {
		log.WarnContext(ctx, "config_activation_unknown", "revision", id,
			"expected", p.base.ID, "by", by.Name, "operator", by.OperatorID, "error", err)
		return Applied{}, &ActivationUnknownError{Base: p.base.ID, Stored: id, Node: s.node,
			Err: err}
	}
	if err != nil {
		return Applied{}, fmt.Errorf("configapi: activate the config: %w", err)
	}
	// THE HISTORY SHOWS THE ACTIVATION, at the pointer's instant, which may
	// be later than the one asked for ([coord.ActivationAt]); the retention
	// sweep dates the row by it. Best effort: the activation has landed and
	// cannot be taken back, and the reconciler records the same instant
	// when its tick meets the pointer.
	if nerr := s.configs.NoteActivation(ctx, id, published.At); nerr != nil {
		log.WarnContext(ctx, "config_activation_not_noted",
			"revision", id, "epoch", published.Epoch, "error", nerr,
			"detail", "the fleet is running this revision; this node's history "+
				"shows the activation once its reconciler meets the pointer")
	}
	s.nudge(ctx, id, summary, p.source, by)
	log.InfoContext(ctx, "config_revision_written",
		"revision", id, "epoch", published.Epoch, "by", by.Name,
		"operator", by.OperatorID, "summary", summary)
	return Applied{
		RevisionID: id, Epoch: published.Epoch, Parent: p.base.ID,
		Warnings: p.warnings, Derived: p.derived,
	}, nil
}

// authoredRules is what a document a caller WROTE is held to: every rule
// [config.Company.Validate] holds, and the charter's on what the write
// changes, judged against the revision it replaces
// ([config.Company.ValidateCharterChange]), so a charter fault the write did
// not touch is a warning on the answer rather than a refusal of the write.
func authoredRules(next, prior *config.Company) error {
	return errors.Join(next.Validate(), next.ValidateCharterChange(prior))
}

// runnableRules is what a stored document re-activated as it is (a reload, a
// revert) is held to: the rules a running company depends on, and none a
// build is free to change, since a newer peer may have admitted it.
func runnableRules(next, _ *config.Company) error {
	return next.ValidateRunnable()
}

// importRules is what an import is held to ([Service.mayImport]): every rule
// [config.Company.Validate] holds, and the charter's as warnings only, which
// the answer carries, because a restored file must import whatever its charter
// says.
func importRules(next, _ *config.Company) error {
	return next.Validate()
}

// patchDraft is a JSON Merge Patch over the active document.
//
// sent is the patch as parsed from what an HTTP caller sent, so a key it
// names that this build does not know is reported at the line it was written
// on; nil reads the patch from req.Patch, which is all a programmatic caller
// has.
func patchDraft(req ApplyRequest, sent *yaml.Node) draft {
	return draft{
		expect: req.Expect, stated: statedPrecondition(req.Stated, req.Expect),
		requireActive: true, by: req.By, engine: req.Engine,
		rules: authoredRules, authored: true,
		build: func(b base) (*config.Company, []byte, error) {
			if len(req.Patch) == 0 {
				return nil, nil, &PatchError{Err: errEmptyPatch}
			}
			merged, err := applyMergePatch(b.document, req.Patch)
			if err != nil {
				return nil, nil, &PatchError{Err: err}
			}
			incoming, err := readPatched(req.Patch, sent, merged)
			if err != nil {
				return nil, nil, &PatchError{Err: err}
			}
			incoming.RestoreRedacted(b.prior)
			// AND THE BYTES ARE WHAT IS STORED, for the same reason the
			// merge base is. Everything above worked on the full document;
			// encoding `incoming` alone would undo it at the last step.
			// Restoring the redacted values is the only thing that changed
			// the struct after the merge, so its own encoding is written
			// BACK OVER the document where the patch named something,
			// which is the only place a mask can be: see [writeBackNamed].
			restored, err := json.Marshal(incoming)
			if err != nil {
				return nil, nil, fmt.Errorf("configapi: encode the merged config: %w", err)
			}
			named, err := readPatch(req.Patch)
			if err != nil {
				return nil, nil, &PatchError{Err: err}
			}
			final, err := writeBackNamed(merged, restored, named)
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
//
// It is the one write that goes on over an active revision stored without a
// seal ([draft.wholeDocument]), and it reads nothing from one: what it
// restores and carries below comes only from a revision that opened.
func replaceDraft(incoming *config.Company, built string, by iam.Actor) draft {
	return draft{
		expect: built, expectAbsent: built == "", by: by,
		rules: authoredRules, authored: true, wholeDocument: true,
		build: func(b base) (*config.Company, []byte, error) {
			if b.prior != nil {
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
			if b.document != nil {
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
func readPatched(patch []byte, sent *yaml.Node, merged []byte) (*config.Company, error) {
	cfg, err := parseDocument(merged)
	if err != nil && !errors.Is(err, config.ErrUnknownField) {
		// Every other refusal the authored reader makes is about the
		// merged document itself and is unchanged by any of this. It names
		// no line: the text a line would count is the engine's own merge,
		// which the caller never saw, rather than anything they sent.
		return nil, withoutLines(err)
	}
	// ASKED WHETHER OR NOT THE MERGE PRODUCED A CLEAN DOCUMENT, because a
	// `null` in a merge patch DELETES the key it names (RFC 7396): a patch
	// naming a key this build does not know leaves no trace of it in the
	// merged document, so the merged document cannot be what decides. Asked
	// only when the merged document already showed the key, the null form
	// answered 201 and changed nothing, since the write carries back every
	// key it cannot represent that the write does not name. It was the one
	// silent no-op on a surface whose whole rule is that an unknown key in a
	// patch is refused rather than ignored.
	named := submitted{text: patch, doc: sent}
	if patchErr := onlyUnknownField(named.company()); patchErr != nil {
		return nil, patchErr
	}
	if err == nil {
		return cfg, nil
	}
	// The strict reader found a key it does not know and the patch did not
	// name it, so it is a PEER's. The stored-form reader holds the merged
	// document to NO rule. Validation happens exactly once, in prepare,
	// after the masks are restored. This line used to validate here as
	// well, before the restore, so on a document a newer peer had extended
	// every PATCH that carried a masked credential (any mcp_servers list
	// read from GET /config) was refused as an invalid patch naming the
	// masks.
	//
	// AND IT MINTS NOTHING, where the authored reader writes down every
	// unit's key and every seat's handle: a seat the patch added would be
	// stored without its handle, and take another from its name the first
	// time that name was corrected.
	cfg, err = config.DecodeCompany(merged)
	if err != nil {
		return nil, err
	}
	config.MintIdentities(cfg)
	return cfg, nil
}

// withoutLines clears the line from every fault in err, for a failure found in
// text the caller did not write. Each fault's rendered message loses its
// trailing line with it, so a refusal's detail and its problems still agree.
func withoutLines(err error) error {
	eachFault(err, func(f *config.Fault) { f.Line = 0 })
	return err
}

// eachFault changes every fault in err's tree in place, before anything has
// rendered it: a fault's message is built from its fields when it is read, so
// the detail and the problems of one refusal both carry the change.
func eachFault(err error, change func(*config.Fault)) {
	switch e := err.(type) { //nolint:errorlint // Every fault in the tree, not the first one errors.As finds.
	case nil:
	case *config.Fault:
		change(e)
	case interface{ Unwrap() []error }:
		for _, part := range e.Unwrap() {
			eachFault(part, change)
		}
	case interface{ Unwrap() error }:
		eachFault(e.Unwrap(), change)
	}
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
//
// EXPECT IS THE REVISION THE CALLER DECIDED ON, in either spelling
// [ApplyRequest.Expect] takes, and empty for none — `POST /config/reload`,
// which takes no `If-Match`, and the engine's own reload after a pass sealed a
// credential, both of which mean "whatever the fleet runs". A `/setup`
// rotation is the caller that sends one: it wrote through the `${VAR}` the
// revision it read holds, and a reload of a document a write in between
// changed — a literal in that slot, or a pointer at another name — would
// republish a company that never reads the value it just sealed and answer
// that the rotation landed. Held as every write's is: refused [RacedError] in
// [Service.prepare] when the pointer has moved, and the compare-and-set
// expects it.
func (s *Service) Reload(ctx context.Context, summary string, by iam.Actor,
	expect string) (Applied, error) {

	prepared, err := s.prepare(ctx, draft{
		expect: expect, requireActive: true, by: by,
		// NOT A CHANGE TO THE COMPANY, so a managed document does not
		// refuse it: the bytes below are the active revision's own, and
		// what a reload exists for is the credential rotation a person
		// may have to make while the managing system cannot — see
		// [Service.Authorize].
		republishes: true,
		// VALIDATED, because a reload is an apply: every node rebuilds its
		// epoch from what this activates, and re-publishing a company this
		// build cannot run would move every node onto a refusal. The answer
		// names the field, and PUT or PATCH is how it is corrected.
		//
		// The RUNNABLE rules only, like every apply. A reload is the
		// credential-rotation gesture, and refusing it for an admission
		// rule — which an apply does not enforce, since a newer peer may
		// have admitted the document under rules this build does not
		// share — would make a rotation impossible until somebody
		// restructured the org. The answer's warnings name each one.
		rules: runnableRules,
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
	return s.commit(ctx, prepared, summary)
}

// warningKey is what one warning is, wherever it sits: its kind, the seat,
// unit and reference it is about, and its words. NOT ITS PATH, which is a
// list index (`units[0].roles[2]`) a write moves by adding or removing a seat
// before it, so the same advisory about the same seat would read as new.
type warningKey struct {
	kind, ref, seat, unit, from, to, message string
}

func keyOf(w config.Warning) warningKey {
	return warningKey{kind: w.Kind, ref: w.Ref, seat: w.Seat, unit: w.Unit, from: w.From,
		to: w.To, message: w.Message}
}

// heldWarnings are the warnings the active document carries, nil when nothing
// is active (every warning of a first write is its own).
//
// READ AS A WRITE'S OWN ARE ([Service.warnings]), the cross-tier ones
// included: read off the document alone, a cross-tier warning the active
// revision already carries would be answered as one every write introduces.
func (s *Service) heldWarnings(prior *config.Company) map[warningKey]bool {
	if prior == nil {
		return nil
	}
	held := map[warningKey]bool{}
	for _, w := range s.warnings(prior) {
		held[keyOf(w)] = true
	}
	return held
}

// checkedWarning is one warning as a dry run answers it: the warning, and
// whether the write introduces it rather than the active revision already
// carrying it.
type checkedWarning struct {
	config.Warning
	Introduced bool `json:"introduced"`
}

// checkedWarnings are a prepared write's warnings, each marked with whether
// the write introduces it. A client showing the person what THIS change means
// (the Dashboard Assistant's proposal card) shows the introduced ones: a
// human seat that had no chat contact before the change is not news about a
// change that added somebody else.
func checkedWarnings(p *prepared) []checkedWarning {
	out := make([]checkedWarning, 0, len(p.warnings))
	for _, w := range p.warnings {
		out = append(out, checkedWarning{Warning: w, Introduced: !p.held[keyOf(w)]})
	}
	return out
}

// statedPrecondition is whether expect is a precondition its request stated:
// one its If-Match header named, never `*` or nothing, whose lost race is
// answered 412 ([RacedError.Stated]).
func statedPrecondition(stated bool, expect string) bool {
	expect = strings.TrimSpace(expect)
	return stated && expect != "" && expect != "*"
}
