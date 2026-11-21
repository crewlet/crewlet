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

// A MANAGED COMPANY DOCUMENT (ADR-0030). Tier A's `api.auth.company_writers`
// names the Tier A tokens that alone may change the document, for a deployment
// whose source (GitOps, an operator) replaces the active revision at every
// reconcile, so a person's edit here would silently vanish.
//
// THE CREDENTIAL, NEVER THE AUTHOR: a write is judged by its credential
// ([iam.Actor.OperatorID]; a Tier A bearer's is `token:<id>`, read by
// [config.APIAuth.MayWriteCompany]). Sessions and owner-acting machine tokens
// name other credentials, so all are refused, a unit's lead included.
//
// ORDER: the route's policy first (a caller who could never write is told the
// permission they lack), then this list, then a lead's reach (admission.go).

// ManagedError reports a change to a managed company document through a
// credential the deployment does not let make one.
//
// A CHANGE is every write that stores different bytes or re-points the fleet:
// PUT, PATCH, an entity write, a revert, a /setup connect or disconnect, and a
// dry run of any of them. NOT a reload, which re-publishes the active bytes: it
// completes a credential rotation, the break-glass a person must keep when the
// managing system cannot know it is needed (the secret store is outside the
// rule likewise). NOT the engine's own writes ([iam.ActorSystem]).
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
// credential. Every other party is judged by [config.APIAuth.MayWriteCompany].
//
// ENFORCED HERE AND NOWHERE ELSE: this package is the one write path onto the
// document, and [Service.prepare] asks it before reading anything. A surface
// with a side effect BEFORE its write (/setup seals a credential, creates a
// GitHub App) asks it earlier, so the refusal precedes the effect; prepare's
// check stays the backstop for programmatic callers.
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
// does: the refusal depends on the credential alone, so a non-writer is not
// first told to fix a body no fix would let through.
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
