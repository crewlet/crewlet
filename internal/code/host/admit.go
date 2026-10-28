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
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// ADMISSION: who is asking, as whom, and of which repository — every git
// request's first step.

// admission is a request admitted to a repository.
type admission struct {
	// name is the repository's name, the URL's `.git` stripped.
	name string
	// actor is who the guard resolved, as every record names them
	// (iam.ActorFor): the author, its kind and the credential — the operator
	// id, `token:<id>` for a Tier A token or `pat:<id>` for a person's own
	// machine token. seat is the human seat the principal holds — a person's
	// own, or the one the identity directory binds a token to — "" for a
	// credential bound to nobody.
	actor      iam.Actor
	operatorID string
	seat       string
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
//  2. PRINCIPAL — the guard's resolution (iam.From). Anonymous — nothing
//     presented, or a credential refused — is challenged, which is what makes
//     git ask its credential helper; a resolution this node could not make,
//     or none at all (the request reached the host past no guard), is 503 and
//     never a challenge, since a credential git would be asked for again is
//     not what is missing.
//  3. PERSON — the human seat the principal holds, if any: the guard's own
//     answer (the identity directory's binding, resolved against the running
//     org), never a second lookup here.
//  4. REPOSITORY — resolved STALE, and again LINEARIZABLE when the stale answer
//     would refuse: a repository not found, one whose status refuses op
//     (deleted or purging for any op, archived for a push), or an old name a
//     push named. A refusal is never decided on a stale read.
//  5. OLD NAME — a name a rename left redirecting is answered by renamed: a
//     fetch's advertisement redirected, its POST admitted to the repository
//     under its current name, a push refused.
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
	p, how := iam.From(ctx)
	switch how {
	case iam.Resolved:
	case iam.Anonymous:
		auth.GitChallenge(w)
		return admission{}, false
	default:
		s.identityUnknown(w, r, name, iam.Reason(ctx))
		return admission{}, false
	}
	adm := admitted(name, p)
	ref, err := s.o.Estate.Resolve(ctx, code.ResolveQuery{Name: name,
		Fresh: statelog.Freshness{Level: statelog.ReadStale}})
	if errors.Is(err, code.ErrRepoNotFound) ||
		(err == nil && (refuses(ref.Status, op) || (ref.Redirect != "" && op == auth.GitPush))) {
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
	case ref.Redirect != "":
		if !renamed(w, r, raw, op, ref) {
			return admission{}, false
		}
		// ADMITTED UNDER ITS CURRENT NAME, so what the fetch logs, heats
		// and records names the repository as every other read does.
		adm.name = ref.Name
	}
	adm.repo = ref
	return adm, true
}

// renamed answers a request that named a repository by an OLD name, which a
// rename left redirecting to it until ref.RedirectUntil (code.RedirectWindow),
// and reports whether the request goes on, admitted to the repository:
//
//   - A FETCH'S ADVERTISEMENT — the GET a clone and a fetch begin with — is
//     302 Found to the same path and query under the new name. git follows a
//     redirect of its first exchange (http.followRedirects' default,
//     `initial`), and REBASES onto the new name — printing `redirecting to`
//     and sending everything after to it — only where the redirect answered
//     the first request it made: a client presenting its credential up front
//     (http.extraHeader, a CI checkout's shape).
//   - A FETCH'S POST GOES ON, to the repository. Where a credential helper
//     answers git's challenge — a person's own git — its first request is
//     anonymous and challenged, so the redirect answers the retry that
//     carried the credential, and git follows it without rebasing: it reads
//     the advertisement under the new name and POSTs to the old one
//     (measured, TestARenamedRepositorysOldNameRedirectsAClone). Refusing
//     that POST, as a redirect-only old name would, fails every such clone
//     with git's bare `RPC failed; HTTP 404`.
//   - A PUSH IS REFUSED, at either step, naming the new name: at the
//     advertisement in the 404 git prints (`remote: <sentence>`), never a
//     redirect, which a credential-helper client follows to POST its push to
//     the old name, where git prints no reason for a refusal. So no push is
//     decided against a name the repository no longer holds, and a pusher is
//     told what to change — answered at admission like a name no repository
//     holds, so no push is recorded for it.
func renamed(w http.ResponseWriter, r *http.Request, raw string, op auth.GitOp, ref code.RepoRef) bool {
	switch {
	case op == auth.GitFetch && r.Method == http.MethodPost:
		return true
	case op == auth.GitFetch:
		target := *r.URL
		target.Path = strings.Replace(target.Path, "/git/"+raw+"/", "/git/"+ref.Redirect+".git/", 1)
		target.RawPath = ""
		w.Header().Set("Location", target.RequestURI())
		w.WriteHeader(http.StatusFound)
		return false
	}
	auth.GitText(w, http.StatusNotFound, renamedSentence(strings.TrimSuffix(raw, ".git"), ref))
	return false
}

// renamedSentence is what a push to oldName, which a rename left redirecting
// to ref, is told: the new name, when the rename landed and until when the
// old name still serves a clone.
func renamedSentence(oldName string, ref code.RepoRef) string {
	since := ref.RedirectUntil.Add(-code.RedirectWindow).UTC().Format(time.RFC3339)
	return strconv.Quote(oldName) + " was renamed to " + strconv.Quote(ref.Redirect) + " on " + since +
		"; set your remote to /git/" + ref.Redirect + ".git at the same address (the old name serves " +
		"clones and fetches until " + ref.RedirectUntil.UTC().Format(time.RFC3339) + ")"
}

// admitted is the admission of a request for repository name by the resolved
// principal p, before its repository is resolved: who it is as every record
// names it (iam.ActorFor), the credential it came through and the human seat
// it acts as. The ONE derivation of an admission's identity from the guard's
// principal, so what a push records, what its grant reads and whose turn its
// transfers take are read off the same fields.
func admitted(name string, p iam.Principal) admission {
	actor := iam.ActorFor(p)
	return admission{name: name, actor: actor, operatorID: actor.OperatorID, seat: p.Seat}
}

// principal is who the admitted request acts as: the operator credential and
// the person it is bound to — "" for nobody. The ONE construction of it, so a
// push's proposal, its session and its grant can never name two different
// principals.
func (a admission) principal() code.Principal {
	return code.Principal{Kind: code.PrincipalOperator, OperatorID: a.operatorID, Seat: a.seat}
}

// share is whose transfer the admitted request is (gitproc.Cmd.Share), what
// the transfer lane decides a contended slot by and charges the run's spend
// to. A PERSON is the holder of a human seat, keyed by the seat's handle:
// a person through any credential of theirs — their own machine tokens and
// their sessions all name their seat — so their laptop and their scripts are
// one principal, as the grant reads them as one; or a Tier A token or a
// service account the identity directory binds to a human seat nobody else
// holds, since a bind is a claim on the seat itself and that seat therefore
// has one holder. A MACHINE is a credential acting as nobody — a Tier A
// token bound to no seat (`token:<id>`) or a service account's machine token
// (`pat:<id>`) — keyed by that credential's name, never by the account that
// owns it (iam.RecordOwner): a service account with a token per CI runner
// gives each runner its own turn, as the fetch spool keys a request by the
// credential too.
func (a admission) share() gitproc.Share {
	if a.seat != "" {
		return gitproc.Share{Class: gitproc.SharePerson, Principal: a.seat}
	}
	return gitproc.Share{Class: gitproc.ShareMachine, Principal: a.operatorID}
}

// grant is the admitted principal's standing on repo (ADR-0058): a PERSON'S,
// so it reads no chart — code.GrantOf consults the roster only for a seat — and
// a credential bound to a human seat maintains every repository while one
// bound to nobody reads it.
func (a admission) grant(repo code.RepoRef) code.Grant {
	return code.GrantOf(a.principal(), repo, nil)
}

// grantRefusal is the class a push its principal's grant refuses whole is
// recorded under: `unbound` for a token bound to nobody — the word the
// dashboard and crewlet read for it, since the remedy is a binding in the
// identity directory rather than a grant — and the domain's not_permitted for
// any other principal.
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

// identityUnknown answers a request whose principal this node could not tell —
// the guard's unknown arm, or a request that reached the host past no guard at
// all (iam.ErrUnresolved, a wiring fault) — 503 with the identity estate's own
// Retry-After, logged.
func (s *Server) identityUnknown(w http.ResponseWriter, r *http.Request, name string, why error) {
	s.log.Warn("code_git_identity_unknown", "repo", name, "path", r.URL.Path, "reason", why)
	wait := time.Duration(auth.RetryIdentity(why)) * time.Second
	if wait > 0 {
		retry(w, wait)
	}
	sentence := "who is asking could not be told here"
	if wait > 0 {
		sentence += "; retry in " + seconds(wait)
	}
	auth.GitText(w, http.StatusServiceUnavailable, sentence)
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
		Retention: s.o.DeletedRetention(), ManagedBy: ref.ManagedBy}.Detail()
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
