package configapi

import (
	"context"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/configplane"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/store"
)

// The three questions this surface answers, as functions rather than as route
// handlers.
//
// The REST route and the dashboard's socket query both call these, for the
// reason the whole read surface exists: two implementations of "what is the
// config" can disagree, and the moment they do, which answer an operator gets
// depends on which transport their browser happened to use.

// The two halves of a diff, named so a handler can say WHICH one is missing.
//
// "The revision you asked about" and "the one you asked to compare it with"
// are different mistakes, and answering a single not_found makes the caller
// check both.
const (
	sideTarget  = httpjson.CodeNotFound
	sideAgainst = httpjson.CodeAgainstNotFound
)

// missingRevision is a revision lookup that found nothing, carrying which
// side of the diff it was.
//
// A TYPED ERROR rather than a message the handler re-reads. The handler used
// to substring-match the error text against the URL's path value, which is
// wrong whenever one id CONTAINS the other: `?against=r-7-typo` on revision
// `r-7` matched, so the caller was told the revision they asked about was
// missing when it exists and the one they named does not. `against` is
// unvalidated, so producing that pair takes nothing but a typo.
//
// It unwraps to [store.ErrNoRevision], so every existing errors.Is check
// keeps working and only the handler that needs the side asks for it.
type missingRevision struct {
	side httpjson.Code
	id   string
}

func (e *missingRevision) Error() string {
	return fmt.Sprintf("%s: %s", store.ErrNoRevision, e.id)
}

func (e *missingRevision) Unwrap() error { return store.ErrNoRevision }

// ErrNoActiveRevision reports a company nobody has configured yet.
//
// A real state and distinct from a failure: a deployment before its first
// import has no configuration, and reporting that as an error would make a
// working new install look broken.
var ErrNoActiveRevision = errors.New("configapi: no active revision")

// fleetRevision is the company as this surface answers it, and whether the
// fleet's activation pointer named it.
type fleetRevision struct {
	revision store.Revision
	// pointed is whether the pointer names revision, which is what a write
	// built on it expects at the compare-and-set; false for the company this
	// node knows when there is no pointer.
	pointed bool
	// fleet is a body read off the pointer that this node holds no row for,
	// which the reconciler keeps only once it opens: a refusal to open it is
	// the fleet's (a key this node was never given, a body somebody put on
	// the pointer unsealed), never this node's history's
	// ([UnreadableRevisionError.Fleet]).
	fleet bool
}

// current is the company this surface answers for: the FLEET's revision, the
// one its activation pointer names, read on every call.
//
// # The pointer, never the revision this node boots on
//
// Every answer here — GET /config and its entity-tag, the revision list's
// `is_active`, a diff against `active`, and the base every write is built on —
// is about the company, and the company is the pointer. What this node boots on
// is another fact (configplane's package doc): it trails the pointer by an
// apply, and stays behind it for good on a node that refused the fleet's
// revision. Answered from it, a write landing between a tick's read of the
// pointer and its apply read back as the revision before it, so the next
// `If-Match` naming the write was refused; and on a node that refused the
// fleet's revision, every write was built on a revision the pointer no longer
// named and lost the compare-and-set — a 409 nothing could clear, on the one
// surface that writes the correction. Every node answers the same, whatever it
// runs.
//
// FROM THIS NODE'S HISTORY WHERE IT HOLDS THE REVISION, and otherwise from the
// body the pointer carries, as the reconciler meets it ([configplane.RevisionOf]):
// a node that has not ticked since the pointer moved holds no row for it yet.
// It is not kept here — A READ NEVER WRITES — and the reconciler's next tick
// keeps it. At the pointer's instant either way.
//
// WITH NO POINTER, the company this node knows ([store.Configs.Restorable]):
// what the fleet was on when this node last saw it, or the change an offline
// command built on that. A write built on it is create-only at the pointer.
//
// A coordination store that cannot be read is an error that stays
// [coord.ErrUnavailable], answered 503: "the fleet has no company" and "I could
// not ask" send a write to opposite outcomes, and guessing the first is the one
// that replaces a running company.
func (s *Service) current(ctx context.Context) (fleetRevision, bool, error) {
	var bodiless *coord.Activation
	for {
		target, pointed, err := s.plane.Target(ctx)
		if err != nil {
			return fleetRevision{}, false, fmt.Errorf("configapi: read the fleet's activation: %w", err)
		}
		if !pointed {
			known, found, readErr := s.configs.Restorable(ctx)
			if readErr != nil {
				return fleetRevision{}, false, fmt.Errorf("configapi: read the revision this "+
					"node knows the company to be: %w", readErr)
			}
			return fleetRevision{revision: known}, found, nil
		}
		held, found, err := s.configs.Get(ctx, target.RevisionID)
		if err != nil {
			return fleetRevision{}, false, fmt.Errorf("configapi: read the fleet's revision %s: %w",
				target.RevisionID, err)
		}
		if found {
			held.ActivatedAt = target.At
			return fleetRevision{revision: held, pointed: true}, true, nil
		}
		payload, found, err := s.plane.Payload(ctx, target.RevisionID)
		if err != nil {
			return fleetRevision{}, false, fmt.Errorf("configapi: read the fleet's revision %s: %w",
				target.RevisionID, err)
		}
		if found {
			return fleetRevision{revision: configplane.RevisionOf(target, payload), pointed: true,
				fleet: true}, true, nil
		}
		// THE POINTER MOVED between the two reads, taking its body with
		// it: read it again. Each round that finds it moved is another
		// write landing, so this ends when they stop. A pointer found
		// UNMOVED with no body is not a race to wait out — every
		// activation carries its body, so it is a pointer nothing this
		// build writes — and is answered as the fault it is.
		if bodiless != nil && bodiless.Epoch == target.Epoch &&
			bodiless.RevisionID == target.RevisionID {
			return fleetRevision{}, false, fmt.Errorf("configapi: the fleet's activation "+
				"names revision %s and carries no body for it", target.RevisionID)
		}
		bodiless = &target
	}
}

// Document is the active company, redacted.
func (s *Service) Document(ctx context.Context) (*config.Company, error) {
	company, _, err := s.documentOf(ctx)
	return company, err
}

// documentOf is [Service.Document] plus the revision it came from, which is
// what an entity-tag needs: the document changes exactly when the active
// revision does, so the revision id IS the validator.
func (s *Service) documentOf(ctx context.Context) (*config.Company, store.Revision, error) {
	company, revision, err := s.activeCompany(ctx)
	if err != nil {
		return nil, store.Revision{}, err
	}
	return company.Redact(), revision, nil
}

// activeCompany opens the fleet's revision ([Service.current]) UNREDACTED.
//
// Never answered to a caller as it stands. It exists for the one read that
// needs what redaction hides without publishing any of it: see
// [Service.References].
func (s *Service) activeCompany(ctx context.Context) (*config.Company, store.Revision, error) {
	cur, found, err := s.current(ctx)
	if err != nil {
		return nil, store.Revision{}, err
	}
	if !found {
		return nil, store.Revision{}, ErrNoActiveRevision
	}
	company, err := s.open(cur)
	if err != nil {
		return nil, store.Revision{}, err
	}
	return company, cur.revision, nil
}

// FleetCompany is the fleet's company UNREDACTED and the id of the revision it
// is, from ONE read of the pointer ([Service.current]).
//
// For a surface built on this one that both describes the document its writes
// change and names the revision they are checked against — the setup surface,
// whose answers say what each field holds and hand back the revision to send
// as `if_match`. Read apart, the two described different revisions on a node
// whose engine runs another than the fleet's — one that refused it, or one
// mid-apply — and a write decided on one document landed on the other. Never
// answered to a caller as it stands: its literals are what redaction hides.
//
// [ErrNoActiveRevision] where the fleet has no company, an
// [*UnreadableRevisionError] where this node's keyring does not open it, and
// [coord.ErrUnavailable] where the pointer could not be read.
func (s *Service) FleetCompany(ctx context.Context) (*config.Company, string, error) {
	company, revision, err := s.activeCompany(ctx)
	if err != nil {
		return nil, "", err
	}
	return company, revision.ID, nil
}

// References is every ${VAR} the active document names, paired with the path
// of the field that names it.
//
// THE QUESTION IS "WHAT BREAKS IF I REMOVE THIS", and it is asked from the
// Secrets screen rather than from here: deleting or renaming a credential the
// document still points at leaves that pointer resolving to nothing, and the
// surfaces holding it start refusing deliveries with no error naming the row
// that went away. The answer has to carry PATHS — a name on its own says a
// reference exists somewhere, which nobody can act on.
//
// It lives on the config surface because it is a fact about the config
// document, not about the secret store: the store holds the values, and the
// only thing that knows a value is spoken for is the document that names it.
// /secrets could not answer this without a second reader of the company
// revision.
//
// Derived from the UNREDACTED document, and it answers only names and paths,
// never a value. The redacted one GET /config serves would hide exactly the
// references this question is about: redaction shows a value only when it is
// one whole ${VAR}, so a credential written as "Bearer ${TOKEN}" reads as a
// mask there, and an index built from that mask would tell an operator that
// nothing points at TOKEN just before they delete it.
//
// And it counts a variable the company reads WITHOUT naming it — an entry's
// empty credential field, filled from a conventional variable
// ([config.Company.VariablesRead]) — because removing that row signs the
// entry out just as surely.
//
// The revision travels with the answer because the index is derived from it
// and changes exactly when it does, which is what the route's entity-tag
// needs and what a caller comparing two answers has to key on.
func (s *Service) References(ctx context.Context) ([]config.Reference, store.Revision, error) {
	company, revision, err := s.activeCompany(ctx)
	if err != nil {
		return nil, store.Revision{}, err
	}
	return company.VariablesRead(), revision, nil
}

// Reads is, for each name, the paths of the fleet's revision that read it
// ([config.Company.VariablesRead]): what a secret write asks before it decides
// whether the revision has to be re-activated for the value to reach anything
// (ADR-0070). A name nothing reads is absent from the answer, and with nothing
// active nothing reads anything, which is an empty answer rather than an
// error: a value sealed before the first import is read by the revision that
// import activates, which resolves it then.
//
// ONE OPENING OF THE REVISION for every name, because a batch of sixty-four
// names asked one at a time would decrypt the company sixty-four times to
// answer one question.
func (s *Service) Reads(ctx context.Context, names []string) (map[string][]string, error) {
	refs, _, err := s.References(ctx)
	if errors.Is(err, ErrNoActiveRevision) {
		return map[string][]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	asked := make(map[string]bool, len(names))
	for _, name := range names {
		asked[name] = true
	}
	out := map[string][]string{}
	for _, ref := range refs {
		if asked[ref.Name] {
			out[ref.Name] = append(out[ref.Name], ref.Path)
		}
	}
	return out, nil
}

// ActiveRevision is the id of the fleet's revision ([Service.current]),
// whichever one this node runs.
//
// The token every conditional write is built against: a caller reads state,
// derives an edit, and names what it read so a concurrent write is refused
// rather than silently overwritten.
func (s *Service) ActiveRevision(ctx context.Context) (string, error) {
	cur, found, err := s.current(ctx)
	if err != nil {
		return "", err
	}
	if !found {
		return "", ErrNoActiveRevision
	}
	return cur.revision.ID, nil
}

// Revisions is this node's history, newest first, metadata only, each marked
// `is_active` when it is the fleet's revision ([Service.current]).
func (s *Service) Revisions(ctx context.Context, limit, offset int) ([]map[string]any, error) {
	cur, _, err := s.current(ctx)
	if err != nil {
		return nil, err
	}
	revisions, err := s.configs.List(ctx, limit, offset)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(revisions))
	for _, revision := range revisions {
		out = append(out, meta(revision, cur.revision.ID))
	}
	return out, nil
}

// Diff compares one revision against another, or against the active one.
//
// Both sides are compared as stored and reported redacted (see [Changes]), so
// a rotated credential shows as a changed mask and never as either value. The
// listing is cut at [MaxChanges] because this is the side answering over a
// wire, and `changes_total` is always present so a reader renders how many
// changed rather than how many arrived.
func (s *Service) Diff(ctx context.Context, revisionID, against string) (map[string]any, error) {
	target, found, err := s.revision(ctx, revisionID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, &missingRevision{side: sideTarget, id: revisionID}
	}
	base, err := s.baseFor(ctx, against)
	if err != nil {
		return nil, err
	}
	from, err := s.open(base)
	if err != nil {
		return nil, err
	}
	to, err := s.open(target)
	if err != nil {
		return nil, err
	}
	changes, err := Changes(from, to)
	if err != nil {
		return nil, err
	}
	// THE CUT, AND THE TOTAL BESIDE IT. A response body and a socket frame
	// are the side with a size budget, so the bound is taken here rather
	// than inside the walk — `crewlet config diff` writes to a terminal and
	// prints the same comparison whole.
	//
	// Reported as a key rather than as an entry: the marker used to ride
	// along IN the listing as a pathless Change whose To was a sentence,
	// which every client then had to know was not a change. The dashboard
	// did not, and drew it as a blank path turning undefined into prose.
	total := len(changes)
	if total > MaxChanges {
		changes = changes[:MaxChanges:MaxChanges]
	}
	if changes == nil {
		// An empty LIST for identical revisions, like every other listing
		// this surface answers — `null` is a shape a typed client has to
		// guard for a second time to learn nothing changed.
		changes = []Change{}
	}
	return map[string]any{
		"from": base.revision.ID, "to": target.revision.ID,
		"changes": changes, "changes_total": total,
	}, nil
}

// baseFor resolves the side a diff compares against: `active` is the fleet's
// revision ([Service.current]).
func (s *Service) baseFor(ctx context.Context, against string) (fleetRevision, error) {
	if against == "" || against == "active" {
		cur, found, err := s.current(ctx)
		if err != nil {
			return fleetRevision{}, err
		}
		if !found {
			return fleetRevision{}, ErrNoActiveRevision
		}
		return cur, nil
	}
	base, found, err := s.revision(ctx, against)
	if err != nil {
		return fleetRevision{}, err
	}
	if !found {
		return fleetRevision{}, &missingRevision{side: sideAgainst, id: against}
	}
	return base, nil
}

// revision is one revision by id: from this node's history, or — for the
// fleet's revision on a node that has not ticked since the pointer named it —
// from the pointer, so a revision every other answer here calls the company is
// never one a read by its id cannot find. Which of the two it came from rides
// with it ([fleetRevision.fleet]), so a revision that does not open is told by
// where it came from.
func (s *Service) revision(ctx context.Context, revisionID string) (fleetRevision, bool, error) {
	held, found, err := s.configs.Get(ctx, revisionID)
	if err != nil || found {
		return fleetRevision{revision: held}, found, err
	}
	cur, found, err := s.current(ctx)
	if err != nil || !found || cur.revision.ID != revisionID {
		return fleetRevision{}, false, err
	}
	return cur, true, nil
}
