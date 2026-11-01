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
	"github.com/crewlet/crewlet/internal/toolname"
)

// ADMISSION: who is asking, as whom, and of which repository — every git
// request's first step. A PERSON is admitted by admit, here; a CODING BOX by
// admitBox (boxadmit.go), and a CHECK'S BOX by admitBox too, its token telling
// it to admitCheck (checkadmit.go). Nothing after admission asks which of the
// three ran but through the admission's own fields and its seven accessors —
// principal, share, refs, boxFloor, resyncKey, floorWords and engineCommitter — and a box's nil check
// where its pushes raise the run's floor and are left out of the operator
// audit (TestNothingAfterAdmissionAsksWhichAdmitterRan).

// admission is a request admitted to a repository.
type admission struct {
	// name is the repository's name, the URL's `.git` stripped.
	name string
	// actor is who the guard resolved, as every record names them
	// (iam.ActorFor): the author, its kind and the credential — the operator
	// id, `token:<id>` for a Tier A token or `pat:<id>` for a person's own
	// machine token. seat is the human seat the principal holds — a person's
	// own, or the one the identity directory binds a token to — "" for a
	// credential bound to nobody. All three empty for a box, which the guard
	// resolved to nobody.
	actor      iam.Actor
	operatorID string
	seat       string
	// repo is the repository as resolved: stale, or through the barrier
	// where a stale answer would refuse — a box's at its run's session.
	repo code.RepoRef
	// linearizable reports that repo was read through the barrier.
	linearizable bool
	// box is the coding run a /box/git/ request was admitted for; nil for a
	// person's and a check's.
	box *BoxRun
	// check is the attempt a check's box was admitted for, as its row stood
	// at admission; nil for a person's request and a coding box's.
	check *code.CheckRow
	// noRefs is a request shown NO REF — a check's box's: its advertisement
	// is protocol v2's, which names none, and its `ls-refs` is answered empty
	// (fetch.go), since it fetches its one commit by id.
	noRefs bool
	// grant is the principal's standing as admission judged it — a person's
	// GrantOf(…, nil), a box's the seat's from the chart held to the run's
	// (code.Grant.Within), and a check's box's none: it acts as nobody,
	// admitted by its attempt's row to fetch, and a fetch past its admission
	// judges no grant. Every later judgement of the request reads it, never a
	// GrantOf of its own (refusedAtAdvertisement, receivePack).
	grant code.Grant
	// fresh is what a read of this request's REFS syncs at ([admission.refs]):
	// LINEARIZABLE for a person, the run's SESSION at its floor for a box, and
	// a check's box's at the position its attempt was queued at.
	fresh statelog.Freshness
	// spool is the credential the request's memory and spool are charged to
	// (spool.go's per-credential share): [personSpool] of a person's or a
	// token's credential, and [boxSpool] for EVERY box. One key for every
	// box because a box runs generated code and the share is what stops one
	// credential HOLDING the node's spool room: keyed per run, sixty runs
	// were sixty quarters and boxes could hold all of it while a person's
	// spooled fetch waited out Timing.SpoolWait; keyed as one, boxes
	// together hold at most a quarter, and a person's never waits behind
	// them. What it costs is among boxes alone: one box's spooled requests
	// can make another box's wait, in the spool's first-come order, which is
	// the untrusted class sharing what it is given.
	spool string
}

// boxSpool is every coding box's ONE spool credential, and personSpool a
// person's or a token's: the credential behind the `cred:` prefix, which
// boxSpool lacks. THE PREFIX KEEPS THE TWO SPELLINGS DISJOINT STRUCTURALLY,
// whatever the identity grammars later admit — no credential the guard
// resolves spells `runs` today only because a person's login carries a dot and
// every credential name a colon (internal/iam), and a rule resting on that
// reading would break the day either grammar widened.
const boxSpool = "runs"

// personSpool is the spool credential of a person's or a token's request.
func personSpool(operatorID string) string { return "cred:" + operatorID }

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
	// A PERSON'S GRANT READS NO CHART (code.GrantOf asks no roster of an
	// operator) and no attribute of the repository but its id, name and
	// owner, so one judged here holds for every row the request later reads
	// of the same repository.
	adm.grant = code.GrantOf(adm.principal(), ref, nil)
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
	return admission{name: name, actor: actor, operatorID: actor.OperatorID, seat: p.Seat,
		fresh: statelog.Freshness{Level: statelog.ReadLinearizable}, spool: personSpool(actor.OperatorID)}
}

// principal is who the admitted request acts as: the operator credential and
// the person it is bound to — "" for nobody — or, for a coding box, its run's
// seat as an agent, its pushes stamped with the run that made them. The ONE
// construction of it, so a push's proposal, its session, its grant and its
// fetch's record can never name two different principals.
//
// A BOX'S RUN IS FITTED where it is stamped (code.FitProvenance): its work
// item, model and coding agent are other systems' labels copied onto the row
// as they arrived — a GitLab key may nest a project path past the bound — and
// one copied raw would have every push the box makes refused as an invalid
// proposal, the run stopped by a label it never chose. Its two ids are the
// engine's own and never fitted: a run's pushes are found by them whole.
//
// A CHECK'S BOX ACTS AS NOBODY — the zero principal: the engine running its
// repository's own checks file, which no seat or person asked of the host. So
// its fetch is no principal's read of a `visibility: unit` repository
// (fetched): the attempt's row, naming the change and who queued it, is its
// record. It pushes nothing (admitCheck), so no proposal or session ever
// carries it — and one that did would be refused as naming nobody.
func (a admission) principal() code.Principal {
	if a.check != nil {
		return code.Principal{}
	}
	if a.box != nil {
		run := a.box.Provenance
		run.WorkBackend, run.WorkKey = code.FitProvenance(run.WorkBackend), code.FitProvenance(run.WorkKey)
		run.WorkID = code.FitProvenance(run.WorkID)
		run.Model, run.CodingAgent = code.FitProvenance(run.Model), code.FitProvenance(run.CodingAgent)
		return code.Principal{Kind: code.PrincipalAgent, Seat: a.box.Seat, Run: run}
	}
	return code.Principal{Kind: code.PrincipalOperator, OperatorID: a.operatorID, Seat: a.seat}
}

// refs is the freshness every read of this request's REFS syncs at — a v0
// advertisement, a push's, an `ls-refs`, a push's receive — at the four call
// sites that name a ref: LINEARIZABLE for a person, through the code log's
// barrier, and the run's SESSION at its floor for a box, which pays no
// barrier because its floor already holds everything it was shown
// (boxadmit.go, "A box reads at its run's floor"). A check's box is shown no
// ref (admission.noRefs), so what it syncs at is its check's session, which
// holds the commit it fetches.
func (a admission) refs() statelog.Freshness { return a.fresh }

// boxFloor is the run a box's ref answers raise the floor of and the floor
// its admission read, and false for a person's request, which has none.
func (a admission) boxFloor() (BoxRunRef, statelog.Position, bool) {
	if a.box == nil {
		return BoxRunRef{}, statelog.Position{}, false
	}
	return a.box.Ref, a.box.Floor, true
}

// holdRefs holds, on a CODING BOX'S run's row, every seat's branch its push
// names that the run does not hold yet — cmds, the push's commands as the
// grant and the protections judged them, those refused left out (pre), since
// they move nothing and are proposed nowhere — in ONE write (Runs.Hold),
// BEFORE receive-pack runs, anything is stored or the push is proposed. A box
// pushes as its RUN, not as a turn: what it pushed is what its parked run
// depends on, so the branch is held for as long as the run's row exists, as a
// branch its turn read or wrote is (code.RefHoldKey). Nothing for a person's
// push, nor for a ref no seat's branch ([code.Holdable]), nor for one the
// row already holds — so every later push of a branch its run holds costs the
// run store nothing — and the keys it holds join the admission's copy of the
// row, so a second run of the push over a rebuilt incarnation holds nothing
// again.
//
// Why at the push and not from its rows: every box push row names its run,
// but holding from them would be a second mechanism — a read of every unended
// run's pushes by whoever reads the holds — beside the row's union the
// launch's grants need anyway.
func (a admission) holdRefs(ctx context.Context, runs Runs, cmds []parsedCommand,
	pre map[string]answered) error {

	if a.box == nil {
		return nil
	}
	var (
		keys []string
		want = map[string]bool{}
	)
	for _, c := range cmds {
		if _, refused := pre[c.Ref]; refused || !code.Holdable(c.Ref) {
			continue
		}
		// A REF NAMED TWICE — which only a hand-built client sends, and the
		// proposal refuses — is one key.
		if key := code.RefHoldKey(a.repo.ID, c.Ref); !a.box.Holds[key] && !want[key] {
			want[key] = true
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 {
		return nil
	}
	if err := runs.Hold(ctx, a.box.Ref, keys); err != nil {
		return err
	}
	if a.box.Holds == nil {
		a.box.Holds = make(map[string]bool, len(keys))
	}
	for _, key := range keys {
		a.box.Holds[key] = true
	}
	return nil
}

// resyncKey is whose re-syncs at the log's head the request's linearizable
// reads draw on (boxlimit.go), as the node's re-sync buckets key them: a
// CODING BOX's run, by its launch, on its seat (code.RunResyncs); a CHECK'S
// BOX's attempt on its repository's checks (code.CheckKey.Resyncs) — its
// repository's checks file is code nobody vouches for as much as a seat's;
// and false for a person's request, whose reads of refs are linearizable by
// right and whose barriers are the census's clients' term.
func (a admission) resyncKey() (scope, principal string, ok bool) {
	switch {
	case a.check != nil:
		scope, principal = a.check.Resyncs()
		return scope, principal, true
	case a.box != nil:
		scope, principal = code.RunResyncs(a.box.Seat, a.box.Ref.LaunchID)
		return scope, principal, true
	}
	return "", "", false
}

// floorWords is how an answer at a box's floor names it — what it has been
// shown, what it may do instead, and whose bucket ran out, the principal's
// and the scope's (code.NotRejudged): a coding run's, or a check's.
func (a admission) floorWords() (shown, instead, principal, scope string) {
	if a.check != nil {
		return "this check was queued at", "a check's box fetches the one commit it checks", checkResyncs,
			checksResyncs
	}
	return "this run has been shown", "it has not been pushed yet, or ask " + toolname.CodeResolve, "this run's",
		"this seat's"
}

// engineCommitter is the address a commit in this request's pack may be
// committed as only where the repository already holds it
// (landing.EngineCommitter): for a CODING BOX the engine's own, as s names it
// off the serving epoch (Signer.Committer), and "" for a person, whose push
// may carry the engine's commits as git carries any. A box runs code nobody
// vouches for, and the committer is what a cooperating client's history
// marks the host's own writes by (ADR-0085), so a box's push claiming it is
// refused forged_committer. An error is a node holding no epoch, which judges
// no box's push.
func (a admission) engineCommitter(s Signer) (string, error) {
	if a.box == nil {
		return "", nil
	}
	return s.Committer()
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
// credential too. A CODING BOX is a RUN, keyed by its launch: its class holds
// at most half the lane (gitproc.ShareClass.Cap), so a person's push or clone
// is never queued behind generated code's client-paced clones. A CHECK'S BOX
// is a run too — a repository's own checks file is code nobody vouches for as
// much as a seat's — keyed by its REPOSITORY (`check:<id>`), never its
// attempt: a push fans out a check per name its file defines, and keyed per
// attempt one repository's dozen checks took a dozen turns of the class where
// a coding run takes one.
func (a admission) share() gitproc.Share {
	switch {
	case a.check != nil:
		return gitproc.Share{Class: gitproc.ShareRun, Principal: "check:" + a.check.RepoID}
	case a.box != nil:
		return gitproc.Share{Class: gitproc.ShareRun, Principal: a.box.Ref.LaunchID}
	case a.seat != "":
		return gitproc.Share{Class: gitproc.SharePerson, Principal: a.seat}
	}
	return gitproc.Share{Class: gitproc.ShareMachine, Principal: a.operatorID}
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
