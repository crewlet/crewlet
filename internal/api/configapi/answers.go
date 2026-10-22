package configapi

import (
	"context"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/config"
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
//
// # The active revision is the fleet's, read where the fleet keeps it
//
// "Active" means the revision the fleet's activation pointer names
// (internal/configplane), and every reader here asks the pointer
// ([Service.active]) rather than this node's own copy of it. The copy is what
// the node boots on and what its reconciler keeps in step once it has applied
// an epoch, so it trails the pointer by however long an apply takes, and
// longer where this node's own mark of a write failed after the fleet took it.
// Read from the copy, a 201 from this surface was followed by a GET /config
// naming the revision that write replaced, and a write built on what a
// lagging node answered lost its compare-and-set to a revision the caller had
// never been shown. The pointer is what every write here is compared against,
// so it is what a read that a write is built on has to answer.
//
// Read from the coordination store's leader, so a read on any node after a
// write's 2xx on any node names that write's revision or a later one. A node
// that cannot read the pointer answers 503 rather than its own copy: an
// answer it cannot vouch for is no answer to "what is the fleet running".
// Whether THIS node has applied the revision yet is a different question,
// answered by /health's applied_epoch.

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

// activeCompany opens the active revision UNREDACTED.
//
// Never answered to a caller as it stands. It exists for the reads that need
// what redaction hides without publishing any of it: see [Service.References]
// and [Service.ActiveCompany].
func (s *Service) activeCompany(ctx context.Context) (*config.Company, store.Revision, error) {
	revision, found, err := s.active(ctx)
	if err != nil {
		return nil, store.Revision{}, err
	}
	if !found {
		return nil, store.Revision{}, ErrNoActiveRevision
	}
	company, err := s.open(ctx, revision)
	if err != nil {
		return nil, store.Revision{}, err
	}
	return company, revision, nil
}

// ActiveCompany is the fleet's active revision and the document it holds,
// UNREDACTED, read once: the company /setup describes and builds every
// submission's requirements from.
//
// THE POINTER'S, as every read here is, and ONE READ giving both: /setup
// answered the integrations of the company this node had applied beside the
// revision id the pointer named, so on a node behind the pointer a client
// sending that id back as `if_match` passed the stale-base check with
// requirements read from the older revision, and its write replaced what the
// newer one had changed. Its ${VAR} references are what a submission writes
// through, which is why it is unredacted; nothing it holds is answered to a
// caller as it stands.
func (s *Service) ActiveCompany(ctx context.Context) (*config.Company, store.Revision, error) {
	return s.activeCompany(ctx)
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
// The revision travels with the answer because the index is derived from it
// and changes exactly when it does, which is what the route's entity-tag
// needs and what a caller comparing two answers has to key on.
func (s *Service) References(ctx context.Context) ([]config.Reference, store.Revision, error) {
	company, revision, err := s.activeCompany(ctx)
	if err != nil {
		return nil, store.Revision{}, err
	}
	return config.References(company), revision, nil
}

// ActiveRevision is the id of the revision the fleet is running: the one its
// activation pointer names.
//
// The token every conditional write is built against: a caller reads state,
// derives an edit, and names what it read so a concurrent write is refused
// rather than silently overwritten.
func (s *Service) ActiveRevision(ctx context.Context) (string, error) {
	target, found, err := s.plane.Target(ctx)
	if err != nil {
		return "", fmt.Errorf("configapi: read the fleet's activation: %w", err)
	}
	if !found {
		return "", ErrNoActiveRevision
	}
	return target.RevisionID, nil
}

// active is the revision the fleet's activation pointer names, with its sealed
// body, and whether there is one.
//
// THE BODY IS THIS NODE'S COPY where it holds one, and the pointer's own where
// it does not yet: every node meets a peer's revision first through the
// pointer, and its reconciler keeps it a moment later. Read in two steps — the
// pointer, then the body the pointer carries — the pointer can move between
// them, which the second read answers as "not this revision". The read is
// made again then, from the pointer, because it moved to a later activation
// and that is the one to answer; it is made again ONLY while the pointer
// keeps moving, so a pointer that names a revision whose body it does not
// carry is reported as that, never asked about for ever.
//
// The revision answers active from the pointer's instant, whatever this
// node's copy says of it.
func (s *Service) active(ctx context.Context) (store.Revision, bool, error) {
	revision, found, _, err := s.activeHeld(ctx)
	return revision, found, err
}

// activeHeld is [Service.active] and whether this node holds the revision in
// its own history: the one listing that has to say so, [Service.Revisions],
// puts a revision it does not hold yet into the page itself.
func (s *Service) activeHeld(ctx context.Context) (store.Revision, bool, bool, error) {
	moved := int64(-1)
	for {
		target, found, err := s.plane.Target(ctx)
		if err != nil {
			return store.Revision{}, false, false,
				fmt.Errorf("configapi: read the fleet's activation: %w", err)
		}
		if !found {
			return store.Revision{}, false, false, nil
		}
		revision, held, err := s.configs.Get(ctx, target.RevisionID)
		if err != nil {
			return store.Revision{}, false, false, err
		}
		if !held {
			payload, current, err := s.plane.Payload(ctx, target.RevisionID)
			if err != nil {
				return store.Revision{}, false, false,
					fmt.Errorf("configapi: read the fleet's revision: %w", err)
			}
			if !current {
				if target.Epoch == moved {
					// A FAULT, never "no such revision": the pointer
					// names one and carries another's body, which no
					// request caused. Wrapping store.ErrNoRevision told a
					// diff that the revision ASKED ABOUT was missing,
					// and the socket's diff that it had aged out.
					return store.Revision{}, false, false, fmt.Errorf("configapi: "+
						"the fleet's activation names revision %s and carries no "+
						"body for it", target.RevisionID)
				}
				moved = target.Epoch
				continue
			}
			revision = fleetRevision(target, payload)
		}
		revision.Active, revision.ActivatedAt = true, target.At
		return revision, true, held, nil
	}
}

// fleetRevision is a revision this node meets only through the pointer, as the
// pointer describes it: its author and its origin are the ones every node
// that keeps it records ([coord.RevisionOrigin]).
func fleetRevision(target coord.Activation, payload []byte) store.Revision {
	origin := target.Origin
	return store.Revision{
		ID: target.RevisionID, Source: origin.Source, CreatedBy: origin.Author,
		CreatedByKind: origin.AuthorKind, OperatorID: origin.OperatorID,
		Summary: target.Summary, Payload: payload, CreatedAt: origin.CreatedAt,
	}
}

// revision is one revision by id: this node's copy, or the pointer's body when
// it names one this node does not hold yet — the revision GET /config on this
// node may already have answered.
func (s *Service) revision(ctx context.Context, id string) (store.Revision, bool, error) {
	revision, held, err := s.configs.Get(ctx, id)
	if err != nil || held {
		return revision, held, err
	}
	active, found, err := s.active(ctx)
	if err != nil || !found || active.ID != id {
		return store.Revision{}, false, err
	}
	return active, true, nil
}

// Revisions is this node's history, newest first, metadata only, with the
// revision the fleet's pointer names marked active.
//
// THE ACTIVE REVISION IS ALWAYS IN IT, where its place in the order puts it,
// held here or not: a revision a peer activated reaches this node's history
// only when the reconciler keeps it, and a node that cannot open its body
// never keeps it. Listed from the history alone, no row was active and the
// active revision was missing while GET /config on the same node answered it,
// so a screen read "none active" beside a document it had just shown.
func (s *Service) Revisions(ctx context.Context, limit, offset int) ([]map[string]any, error) {
	active, found, held, err := s.activeHeld(ctx)
	if err != nil {
		return nil, err
	}
	activeID := ""
	if found {
		activeID = active.ID
	}
	var revisions []store.Revision
	if found && !held {
		revisions, err = s.pageWith(ctx, active, limit, offset)
	} else {
		revisions, err = s.configs.List(ctx, limit, offset)
	}
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(revisions))
	for _, revision := range revisions {
		out = append(out, meta(revision, activeID))
	}
	return out, nil
}

// pageWith is one page of this node's history with extra, a revision it does
// not hold, placed where the history's own order puts it: newest first by
// creation, and before the rows it ties with, since the row it becomes when
// the reconciler keeps it is inserted after them.
//
// EXACT ACROSS PAGES: a page past the one extra falls on starts one row
// earlier in the history, so it is read from the row before, and the row the
// extra pushed off the previous page opens this one.
func (s *Service) pageWith(ctx context.Context, extra store.Revision,
	limit, offset int) ([]store.Revision, error) {

	if limit <= 0 {
		limit = store.DefaultRevisionPage
	}
	if offset < 0 {
		offset = 0
	}
	newer := func(r store.Revision) bool { return !extra.CreatedAt.Before(r.CreatedAt) }
	if offset > 0 {
		rows, err := s.configs.List(ctx, limit+1, offset-1)
		if err != nil || len(rows) == 0 {
			// Past the end of the history and the extra both.
			return nil, err
		}
		if newer(rows[0]) {
			// The extra sits before this page, so every row here moved
			// one place down.
			return rows[:min(limit, len(rows))], nil
		}
		return placed(rows[1:], extra, newer, limit), nil
	}
	rows, err := s.configs.List(ctx, limit, 0)
	if err != nil {
		return nil, err
	}
	return placed(rows, extra, newer, limit), nil
}

// placed is rows with extra inserted before the first row it is newer than,
// or after the last when the page has room for it, cut to limit.
func placed(rows []store.Revision, extra store.Revision,
	newer func(store.Revision) bool, limit int) []store.Revision {

	at := len(rows)
	for i, r := range rows {
		if newer(r) {
			at = i
			break
		}
	}
	if at == len(rows) && len(rows) >= limit {
		return rows
	}
	out := make([]store.Revision, 0, len(rows)+1)
	out = append(out, rows[:at]...)
	out = append(out, extra)
	out = append(out, rows[at:]...)
	return out[:min(limit, len(out))]
}

// activeID is the revision the fleet's pointer names, empty when nothing has
// been activated: the one revision a listing marks active.
func (s *Service) activeID(ctx context.Context) (string, error) {
	id, err := s.ActiveRevision(ctx)
	if errors.Is(err, ErrNoActiveRevision) {
		return "", nil
	}
	return id, err
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
	from, err := s.open(ctx, base)
	if err != nil {
		return nil, err
	}
	to, err := s.open(ctx, target)
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
		"from": base.ID, "to": target.ID,
		"changes": changes, "changes_total": total,
	}, nil
}

// baseFor resolves the side a diff compares against.
func (s *Service) baseFor(ctx context.Context, against string) (store.Revision, error) {
	if against == "" || against == "active" {
		active, found, err := s.active(ctx)
		if err != nil {
			return store.Revision{}, err
		}
		if !found {
			return store.Revision{}, ErrNoActiveRevision
		}
		return active, nil
	}
	base, found, err := s.revision(ctx, against)
	if err != nil {
		return store.Revision{}, err
	}
	if !found {
		return store.Revision{}, &missingRevision{side: sideAgainst, id: against}
	}
	return base, nil
}
