package configapi

import (
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/words"
)

// A MANAGED COMPANY DOCUMENT — ADR-0030.
//
// Tier A's `api.auth.company_writers` names the Tier A tokens that alone may
// change the company document, for a deployment where something other than a
// person is its source: a GitOps pipeline, a Kubernetes operator rendering it
// from custom resources. Such a system replaces the active revision with its
// own at every reconcile, so a person's edit made here lasted until then and
// nothing said so. The refusal is the saying.
//
// THE CREDENTIAL, NEVER THE AUTHOR. A write is judged by the credential it
// came through ([iam.Actor.OperatorID]), and a Tier A bearer's is its own
// login, `token:<id>`: a listed id is admitted by that and by nothing else
// ([config.APIAuth.MayWriteCompany], the one reading of the list). A person's
// session, a machine token acting as its owner and a session exchanged from a
// Tier A token each name another credential, so every one of them is refused
// while the document is managed — a unit's lead writing inside their own
// subtree included, because their edit is exactly the one the managing
// system's next reconcile overwrites.
//
// IT COMPOSES WITH THE AUTHORITY TABLE, in a fixed order: the route's policy
// decides first (admin, or `config.org.write` for whoever could lead a unit),
// so a caller who could never write is told the permission they lack;
// then this list, so a caller who could is told the document is managed; then
// the org-chart reach a lead's write is admitted on (admission.go); and only
// then is the body read. Neither replaces the other: the table says who may
// change the company, and this says that nobody but its source may change it
// HERE.
//
// ENFORCED HERE AND NOWHERE ELSE, because this package is the one write path
// onto the document: every route on this surface, `/setup` (through
// [Service.Apply] and [Service.ApplyEntity]), and the engine's own writes all
// reach [Service.prepare], which asks [Service.Authorize] before it reads
// anything. Each HTTP route that changes the document asks it FIRST, before
// it reads the body, its summary or the revision it names
// ([Service.refusedManaged]): the refusal depends on the credential alone, so
// a non-writer is answered it whatever it sent rather than first being told
// to fix a body no fix would let through. A surface that has a side effect
// BEFORE its write — `/setup` seals a credential first, creates a GitHub App
// first, queues a teardown first — asks Authorize itself, earlier, so the
// refusal comes before the side effect rather than after it; the check in
// prepare stays the backstop for every programmatic caller.
//
// WHAT COUNTS AS CHANGING THE DOCUMENT is every write that stores different
// bytes or re-points the fleet at another revision: PUT, PATCH, an entity
// write, a revert, a /setup connect or disconnect. A dry run of any of them is
// refused alike, since a check exists to answer what the write would. NOT a
// reload, which re-publishes the active revision's own bytes: it is the gesture
// that completes a credential rotation, and a rotation — a leaked key, now — is
// exactly the break-glass a person must keep when the managing system cannot
// know it is needed. The secret store is outside the rule for the same reason;
// both stay open to whoever the authority table admits to them. And NOT the
// engine's own writes ([iam.ActorSystem]): the rule is about which credentials
// may write, and the engine presents none.

// ManagedError reports a change to a managed company document through a
// credential the deployment does not let make one.
type ManagedError struct {
	// By is the refused party: the author, its kind and the credential the
	// write came through, which is what the list was asked about.
	By iam.Actor
	// Writers are the token ids that may change the document.
	Writers []string
}

func (e *ManagedError) Error() string {
	return fmt.Sprintf("configapi: the company config is managed by %s, so "+
		"%s can read it but not change it",
		strings.Join(e.Writers, ", "), e.party())
}

// party names the refused write the way a person reads it: who, and through
// which credential, because the credential is what the list judges — a person
// told only their own name could not see why a token of theirs is refused too.
func (e *ManagedError) party() string {
	switch {
	case e.By.OperatorID == "" || e.By.OperatorID == e.By.Name:
		return fmt.Sprintf("%q", e.By.Name)
	case e.By.Name == "":
		return fmt.Sprintf("the credential %q", e.By.OperatorID)
	}
	return fmt.Sprintf("%q, through the credential %q", e.By.Name, e.By.OperatorID)
}

// Hint is what to do instead, for every surface that answers the refusal.
//
// THE WRITERS ARE TOKEN IDS, not systems: the hint names them as the
// credential the managing system writes with, never as the system itself.
func (e *ManagedError) Hint() string {
	return "Change it in the system that manages it (" +
		tokensPhrase(e.Writers) + "). Edits here get overwritten. To rotate " +
		"a leaked credential, write it to /secrets and POST /config/reload. " +
		"To manage it here, remove `api.auth.company_writers` from every " +
		"node's config file and restart."
}

// tokensPhrase names token ids as tokens: `the token "gitops"`, `one of the
// tokens "gitops" or "ci"`.
func tokensPhrase(ids []string) string {
	if len(ids) == 1 {
		return "the token " + words.Or(ids)
	}
	return "one of the tokens " + words.Or(ids)
}

// Authorize reports whether by may change the company document, as a
// [*ManagedError] when it may not.
//
// THE ENGINE'S OWN WRITES PASS: an [iam.ActorSystem] write presents no
// credential, and the rule is which credentials may write. Every other party
// — whatever surface it came through — is judged by
// [config.APIAuth.MayWriteCompany], the one reading of the list.
func (s *Service) Authorize(by iam.Actor) error {
	// THROUGH A TYPED POINTER, never returned as the error itself: a nil
	// *ManagedError in an error interface is not nil.
	if managed := s.managedRefusal(by); managed != nil {
		return managed
	}
	return nil
}

// managedRefusal is [Service.Authorize]'s answer as the refusal itself, or
// nil when by may change the company document.
func (s *Service) managedRefusal(by iam.Actor) *ManagedError {
	if s.writers.MayWriteCompany(by) {
		return nil
	}
	return &ManagedError{By: by, Writers: slices.Clone(s.writers.CompanyWriters)}
}

// refusedManaged answers this request's credential the managed refusal when
// it may not change the company document, and reports whether it did. Every
// route that changes the document asks it before anything else its handler
// does; see the package's note on where this is enforced.
func (s *Service) refusedManaged(w http.ResponseWriter, r *http.Request) bool {
	managed := s.managedRefusal(attributionOf(r))
	if managed == nil {
		return false
	}
	RefuseManaged(w, managed)
	return true
}

// RefuseManaged answers a [*ManagedError] for any surface: 403
// [httpjson.CodeConfigManaged], who manages the document and what to do
// instead.
//
// ONE SHAPE for /config and /setup, so a client — the dashboard, `crewlet
// config import` — reads the same refusal wherever its write was refused.
func RefuseManaged(w http.ResponseWriter, managed *ManagedError) {
	httpjson.FailWithFields(w, http.StatusForbidden, httpjson.CodeConfigManaged, httpjson.Detail{
		"detail": "The company config is managed by " +
			strings.Join(managed.Writers, ", ") + ". " +
			httpjson.Sentence(managed.party()+" can read it but not change it"),
		"managed_by": managed.Writers,
		"hint":       managed.Hint(),
	})
}
