package host

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/statelog"
)

// ADMISSION: who is asking, as whom, and of which repository — every git
// request's first step.

// admission is a request admitted to a repository.
type admission struct {
	// name is the repository's name, the URL's `.git` stripped.
	name string
	// operatorID is the credential, seat the human seat it is bound to — ""
	// for a credential bound to nobody.
	operatorID, seat string
	// repo is the repository as resolved: stale, or through the barrier
	// where a stale answer would refuse.
	repo code.RepoRef
	// linearizable reports that repo was read through the barrier.
	linearizable bool
}

// admit answers the request itself and returns false when it must stop:
//
//  1. NAME — the URL's segment with one `.git` stripped, held to the grammar;
//     a name the grammar refuses is a repository that cannot exist (404).
//  2. OPERATOR — the guard's; absent, the request reached the host some other
//     way than through it, and is challenged rather than served.
//  3. PERSON — the human seat the credential is bound to, if any.
//  4. REPOSITORY — resolved STALE, and again LINEARIZABLE when the stale answer
//     would refuse: a repository not found, or one whose status refuses op
//     (deleted or purging for any op, archived for a push). A refusal is never
//     decided on a stale read.
//
// The repository's STATUS refusals are not answered here: each entry point
// answers them in its protocol's own form.
func (s *Server) admit(ctx context.Context, w http.ResponseWriter, r *http.Request, op auth.GitOp) (admission, bool) {
	raw := r.PathValue(PathRepo)
	name := strings.TrimSuffix(raw, ".git")
	if !gitref.ValidRepoName(name) {
		auth.GitText(w, http.StatusNotFound, notFoundSentence(raw))
		return admission{}, false
	}
	operatorID, ok := auth.OperatorFrom(ctx)
	if !ok {
		auth.GitChallenge(w)
		return admission{}, false
	}
	adm := admission{name: name, operatorID: operatorID, seat: s.o.Persons(operatorID)}
	ref, err := s.o.Estate.Resolve(ctx, code.ResolveQuery{Name: name,
		Fresh: statelog.Freshness{Level: statelog.ReadStale}})
	if errors.Is(err, code.ErrRepoNotFound) || (err == nil && refuses(ref.Status, op)) {
		adm.linearizable = true
		ref, err = s.o.Estate.Resolve(ctx, code.ResolveQuery{Name: name,
			Fresh: statelog.Freshness{Level: statelog.ReadLinearizable}})
	}
	switch {
	case errors.Is(err, code.ErrRepoNotFound):
		auth.GitText(w, http.StatusNotFound, notFoundSentence(name))
		return admission{}, false
	case err != nil:
		s.unavailable(w, r, name, err)
		return admission{}, false
	}
	adm.repo = ref
	return adm, true
}

// principal is who the admitted request acts as: the operator credential and
// the person it is bound to — "" for nobody. The ONE construction of it, so a
// push's proposal, its session and its grant can never name two different
// principals.
func (a admission) principal() code.Principal {
	return code.Principal{Kind: code.PrincipalOperator, OperatorID: a.operatorID, Seat: a.seat}
}

// grant is the admitted principal's standing on repo (ADR-0040): a PERSON'S,
// so it reads no chart — code.GrantOf consults the roster only for a seat — and
// a credential bound to a human seat maintains every repository while one
// bound to nobody reads it.
func (a admission) grant(repo code.RepoRef) code.Grant {
	return code.GrantOf(a.principal(), repo, nil)
}

// grantRefusal is the class a push its principal's grant refuses whole is
// recorded under: `unbound` for a token bound to nobody — the word the
// dashboard and crewlet read for it, since the remedy is a line of
// configuration rather than a grant — and the domain's not_permitted for any
// other principal.
func grantRefusal(p code.Principal) AuditRefusal {
	if p.Kind == code.PrincipalOperator && p.Seat == "" {
		return RefusalUnbound
	}
	return AuditRefusal(code.RefusalNotPermitted)
}

// refuses reports whether a repository in status refuses op.
func refuses(status code.RepoStatus, op auth.GitOp) bool {
	switch status {
	case code.RepoStatusDeleted, code.RepoStatusPurging:
		return true
	case code.RepoStatusArchived:
		return op == auth.GitPush
	}
	return false
}

// gone reports a deleted or purging repository: one that answers nothing.
func gone(status code.RepoStatus) bool {
	return status == code.RepoStatusDeleted || status == code.RepoStatusPurging
}

// unavailable answers a request the code log could not be read for here — a
// linearizable read refused (no quorum, a copy out of service), or no data
// node answered — 503 with Retry-After, logged.
func (s *Server) unavailable(w http.ResponseWriter, r *http.Request, name string, err error) {
	if s.ended(w, r, name, err) {
		return
	}
	s.log.Warn("code_git_unavailable", "repo", name, "path", r.URL.Path, "error", err)
	retry(w, UnavailableRetryAfter)
	auth.GitText(w, http.StatusServiceUnavailable, "the code log could not be read here: "+
		sentence(err.Error())+"; retry in "+seconds(UnavailableRetryAfter))
}

// retry sets a 503's Retry-After, in whole seconds.
func retry(w http.ResponseWriter, d time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(int(d/time.Second)))
}

// seconds is d as a person reads a wait: "10 s".
func seconds(d time.Duration) string { return strconv.Itoa(int(d/time.Second)) + " s" }

// THE SENTENCES of the refusals admission and the entry points make, each the
// one spelling of its refusal.

// notFoundSentence is a repository nobody declared, or a name that cannot be
// one.
func notFoundSentence(name string) string {
	return "repository " + strconv.Quote(name) + " not found"
}

// goneSentence is a deleted or purging repository's: the domain's own
// sentence for the state (code.RepoGone), so the advertisement, a fetch and a
// push say what the push decide says.
func (s *Server) goneSentence(ref code.RepoRef) string {
	return code.RepoGone{ID: ref.ID, Name: ref.Name, Status: ref.Status, DeletedAt: ref.DeletedAt,
		Retention: s.o.DeletedRetention()}.Detail()
}

// serviceFor is the git process an advertisement runs — read from the
// request's query only AFTER auth.GitAccess has classified it, and only ever
// the service GitAccess's class allows: git-upload-pack for a fetch,
// git-receive-pack for a push, and nothing else. A request whose query names
// another service, names one twice or does not parse is refused (ok false),
// and one naming no service is the dumb protocol's (dumb true). So a request
// the classifier calls a fetch never runs receive-pack — which is what makes
// the guard's and the drain's reading of it the one that holds (FuzzServiceFor).
func serviceFor(r *http.Request) (svc string, dumb, ok bool) {
	op := auth.GitAccess(r)
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return "", false, false
	}
	asked := query["service"]
	switch {
	case op == auth.GitFetch && (len(asked) == 0 || (len(asked) == 1 && asked[0] == "")):
		return "", true, false
	case op == auth.GitFetch && len(asked) == 1 && asked[0] == "git-upload-pack":
		return asked[0], false, true
	case op == auth.GitPush && len(asked) == 1 && asked[0] == "git-receive-pack":
		return asked[0], false, true
	}
	return "", false, false
}
