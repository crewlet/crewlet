package configapi

import (
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/store"
)

// A MANAGED COMPANY DOCUMENT — ADR-0030.
//
// Tier A's `api.auth.company_writers` names the credentials that alone may
// change the company document, for a deployment where something other than a
// person is its source: a GitOps pipeline, a Kubernetes operator rendering it
// from custom resources. Such a system replaces the active revision with its
// own at every reconcile, so a person's edit made here lasted until then and
// nothing said so. The refusal is the saying.
//
// ENFORCED HERE AND NOWHERE ELSE, because this package is the one write path
// onto the document: every route on this surface, `/setup` (through
// [Service.Apply] and [Service.ApplyEntity]), and the engine's own writes all
// reach [Service.prepare], which asks [Service.Authorize] before it reads
// anything. A surface that has a side effect BEFORE its write — `/setup`
// seals a credential first, creates a GitHub App first, queues a teardown
// first — asks Authorize itself, earlier, so the refusal comes before the side
// effect rather than after it; the check in prepare stays the backstop.
//
// WHAT COUNTS AS CHANGING THE DOCUMENT is every write that stores different
// bytes or re-points the fleet at another revision: PUT, PATCH, an entity
// write, a revert, a /setup connect or disconnect. A dry run of any of them is
// refused alike, since a check exists to answer what the write would. NOT a
// reload, which re-publishes the active revision's own bytes: it is the gesture
// that completes a credential rotation, and a rotation — a leaked key, now — is
// exactly the break-glass a person must keep when the managing system cannot
// know it is needed. The secret store is outside the rule for the same reason.
// And NOT the engine's own writes ([store.AuthorNode]): the rule is about
// which credentials may write, and the engine presents none.

// CodeConfigManaged is a write onto a managed company document by a
// credential that is not one of its writers. 403: the credential is valid and
// the request well formed, and no retry with them changes the answer.
const CodeConfigManaged = httpjson.Code("config_managed")

// ManagedError reports a change to a managed company document by a
// credential the deployment does not let make one.
type ManagedError struct {
	// Operator is the refused credential's token id.
	Operator string
	// Writers are the token ids that may change the document.
	Writers []string
}

func (e *ManagedError) Error() string {
	return fmt.Sprintf("configapi: the company document is managed by %s; "+
		"the credential %q may read it and may not change it",
		strings.Join(e.Writers, ", "), e.Operator)
}

// Hint is what to do instead, for every surface that answers the refusal.
func (e *ManagedError) Hint() string {
	return "change the company where it is managed — the source " +
		strings.Join(e.Writers, ", ") + " renders it from — and let that " +
		"system write it here: an edit made directly would be overwritten at " +
		"its next reconcile. A leaked credential can still be rotated: write " +
		"the new value with /secrets and POST /config/reload. To take the " +
		"document back, remove api.auth.company_writers from every node's " +
		"Tier A and restart"
}

// Authorize reports whether author may change the company document, as a
// [*ManagedError] when it may not.
//
// THE ENGINE'S OWN WRITES PASS: a [store.AuthorNode] presents no credential,
// and the rule is which credentials may write. Every operator — whatever
// surface it came through — is judged by [config.APIAuth.MayWriteCompany],
// the one reading of the list.
func (s *Service) Authorize(author store.Author) error {
	if author.Kind == store.AuthorNode {
		return nil
	}
	policy := &s.boot.API.Auth
	if policy.MayWriteCompany(author.Name) {
		return nil
	}
	return &ManagedError{Operator: author.Name, Writers: slices.Clone(policy.CompanyWriters)}
}

// RefuseManaged answers a [*ManagedError] for any surface: 403, the code, who
// manages the document and what to do instead.
//
// ONE SHAPE for /config and /setup, so a client — the dashboard, `crewlet
// config import` — reads the same refusal wherever its write was refused.
func RefuseManaged(w http.ResponseWriter, managed *ManagedError) {
	httpjson.FailWithFields(w, http.StatusForbidden, CodeConfigManaged, map[string]any{
		"detail": "the company document is managed by " +
			strings.Join(managed.Writers, ", ") + ", and the credential " +
			fmt.Sprintf("%q", managed.Operator) + " may read it but not change it",
		"managed_by": managed.Writers,
		"hint":       managed.Hint(),
	})
}
