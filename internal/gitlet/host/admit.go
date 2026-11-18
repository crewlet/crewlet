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
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/toolname"
)

// ADMISSION: who is asking, as whom, and of which repository — every git
// request's first step. A PERSON is admitted by admit; a CODING BOX by
// admitBox (boxadmit.go); a CHECK'S BOX by admitBox too, its token routing it
// to admitCheck (checkadmit.go). Nothing after admission asks which ran but
// through the admission's fields and accessors (principal, writtenFor, share,
// refs, objects, boxFloor, resyncKey, floorWords, engineCommitter), and a box's
// nil check where its pushes raise the run's floor and skip the operator audit
// (TestNothingAfterAdmissionAsksWhichAdmitterRan).

// admission is a request admitted to a repository.
type admission struct {
	// name is the repository's name, the URL's `.git` stripped.
	name string
	// actor is who the guard resolved, as every record names them
	// (iam.ActorFor). seat is the human seat the principal holds, its own or
	// the one the identity directory binds a token to; "" for a credential
	// bound to nobody. All three empty for a box, which the guard resolved to
	// nobody.
	actor      iam.Actor
	operatorID string
	seat       string
	// repo is the repository as resolved: stale, or through the barrier
	// where a stale answer would refuse — a box's at its run's session.
	repo gitlet.RepoRef
	// linearizable reports that repo was read through the barrier.
	linearizable bool
	// box is the executor a /box/git/ request was admitted for; nil otherwise.
	box *BoxRun
	// check is the attempt a check's box was admitted for, as its row stood
	// at admission; nil otherwise.
	check *gitlet.CheckRow
	// noRefs is a check's box's request: shown NO REF (protocol v2's
	// advertisement, `ls-refs` answered empty in fetch.go), since it fetches
	// its one commit by id.
	noRefs bool
	// grant is the principal's standing as admission judged it: a person's
	// GrantOf(…, nil), a box's seat grant held to the run's
	// (gitlet.Grant.Within), and none for a check's box, which acts as nobody
	// and only fetches. Every later judgement reads it, never a GrantOf of its
	// own (refusedAtAdvertisement, receivePack).
	grant gitlet.Grant
	// fresh is what a read of this request's REFS syncs at ([admission.refs]):
	// LINEARIZABLE for a person, the run's SESSION at its floor for a box, and
	// a check's box's at the position its attempt was queued at.
	fresh statelog.Freshness
	// spool is the credential the request's memory and spool are charged to
	// (spool.go's per-credential share): [personSpool] for a person or token,
	// [boxSpool] for EVERY box. Keyed per run, many boxes could together hold
	// the whole spool while a person's fetch waited out Timing.SpoolWait;
	// keyed as one, boxes hold at most a quarter. The cost stays among boxes:
	// one box's spooled requests can make another's wait.
	spool string
}

// boxSpool is every coding box's ONE spool credential; personSpool's `cred:`
// prefix keeps the two spellings disjoint STRUCTURALLY, rather than by today's
// identity grammars (a login carries a dot, a credential name a colon), which
// may widen.
const boxSpool = "runs"

// personSpool is the spool credential of a person's or a token's request.
func personSpool(operatorID string) string { return "cred:" + operatorID }

// admit answers the request itself and returns false when it must stop:
//
//  1. NAME — the URL's segment with one `.git` stripped, held to the grammar;
//     a name the grammar refuses is a repository that cannot exist (404).
//  2. PRINCIPAL — the guard's resolution (iam.From), its seat never looked up
//     again. Anonymous is challenged, so git asks its credential helper; an
//     unresolved one is 503: a credential is not what is missing.
//  3. REPOSITORY — resolved STALE, and again LINEARIZABLE when the stale answer
//     would refuse (not found, a status that refuses op, an old name a push
//     named): a refusal is never decided on a stale read.
//  4. OLD NAME — answered by renamed. STATUS refusals are each entry point's.
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
		if op == auth.GitDownload {
			// A download is never challenged: a browser would prompt.
			auth.DownloadUnauthorized(w)
		} else {
			auth.GitChallenge(w)
		}
		return admission{}, false
	default:
		s.identityUnknown(w, r, name, iam.Reason(ctx))
		return admission{}, false
	}
	adm := admitted(name, p)
	ref, err := s.o.Estate.Resolve(ctx, gitlet.ResolveQuery{Name: name,
		Fresh: statelog.Freshness{Level: statelog.ReadStale}})
	if errors.Is(err, gitlet.ErrRepoNotFound) ||
		(err == nil && (refuses(ref, op) || (ref.Redirect != "" && op == auth.GitPush))) {
		adm.linearizable = true
		ref, err = s.o.Estate.Resolve(ctx, gitlet.ResolveQuery{Name: name,
			Fresh: statelog.Freshness{Level: statelog.ReadLinearizable}})
	}
	switch {
	case errors.Is(err, gitlet.ErrRepoNotFound):
		auth.GitText(w, http.StatusNotFound, notFoundSentence(name))
		return admission{}, false
	case err != nil:
		s.unavailable(w, r, name, err)
		return admission{}, false
	case ref.Redirect != "":
		if !renamed(w, r, raw, op, ref) {
			return admission{}, false
		}
		// Admitted under its CURRENT name, so what the fetch logs, heats
		// and records names it as every other read does.
		adm.name = ref.Name
	}
	adm.repo = ref
	// A person's grant reads no chart and only the repository's id, name and
	// owner, so one judged here holds for every later row of it.
	adm.grant = gitlet.GrantOf(adm.principal(), ref, nil)
	return adm, true
}

// renamed answers a request naming a repository by an OLD name, redirecting
// until ref.RedirectUntil, and reports whether it goes on, admitted:
//
//   - A FETCH'S ADVERTISEMENT is 302 Found to the same path and query under
//     the new name. git rebases onto it only when the redirect answered its
//     very first request (a credential sent up front, http.extraHeader).
//   - A FETCH'S POST GOES ON: behind a credential helper the redirect answers
//     the challenged retry; git follows it without rebasing and POSTs to the
//     old name (TestARenamedRepositorysOldNameRedirectsAClone).
//   - A DOWNLOAD GOES ON, under the current name.
//   - A PUSH IS REFUSED, 404 naming the new name, never redirected (git prints
//     no reason for a refused POST), and not recorded.
func renamed(w http.ResponseWriter, r *http.Request, raw string, op auth.GitOp, ref gitlet.RepoRef) bool {
	switch {
	case op == auth.GitDownload:
		// Served as the repository, never redirected: a Location built from
		// the request's path misses a path-routing proxy's prefix.
		return true
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
func renamedSentence(oldName string, ref gitlet.RepoRef) string {
	since := ref.RedirectUntil.Add(-gitlet.RedirectWindow).UTC().Format(time.RFC3339)
	return strconv.Quote(oldName) + " was renamed to " + strconv.Quote(ref.Redirect) + " on " + since +
		"; set your remote to /git/" + ref.Redirect + ".git at the same address (the old name serves " +
		"clones and fetches until " + ref.RedirectUntil.UTC().Format(time.RFC3339) + ")"
}

// admitted is the admission of a request for repository name by the resolved
// principal p, before its repository is resolved. The ONE derivation of an
// admission's identity, so a push's record, its grant and its transfers' turn
// read the same fields.
func admitted(name string, p iam.Principal) admission {
	actor := iam.ActorFor(p)
	return admission{name: name, actor: actor, operatorID: actor.OperatorID, seat: p.Seat,
		fresh: statelog.Freshness{Level: statelog.ReadLinearizable}, spool: personSpool(actor.OperatorID)}
}

// principal is who the admitted request acts as: the operator credential and
// its bound person, or a coding box's run's seat as an agent. The ONE
// construction, so a push's proposal, session, grant and fetch record agree.
//
// A BOX'S RUN IS FITTED (gitlet.FitProvenance): its work item, model and
// harness are other systems' labels, and one past the bound would have every
// push refused as an invalid proposal. Its two ids are the engine's own and
// never fitted: a run's pushes are found by them whole.
//
// A CHECK'S BOX ACTS AS NOBODY (the zero principal): its fetch is no
// principal's read of a `visibility: unit` repository (fetched), the attempt's
// row is its record. It pushes nothing (admitCheck).
func (a admission) principal() gitlet.Principal {
	if a.check != nil {
		return gitlet.Principal{}
	}
	if a.box != nil {
		run := a.box.Provenance
		run.WorkBackend, run.WorkKey = gitlet.FitProvenance(run.WorkBackend), gitlet.FitProvenance(run.WorkKey)
		run.WorkID, run.WorkProject = gitlet.FitProvenance(run.WorkID), gitlet.FitProvenance(run.WorkProject)
		run.Model, run.Harness = gitlet.FitProvenance(run.Model), gitlet.FitProvenance(run.Harness)
		return gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: a.box.Seat, Run: run}
	}
	return gitlet.Principal{Kind: gitlet.PrincipalOperator, OperatorID: a.operatorID, Seat: a.seat}
}

// writtenFor is whom a push this request lands is made for
// (gitlet.Proposal.WrittenFor). A CODING BOX's push is its run's turn's write
// made late, so it is made for whom that turn works for (BoxRun.WorkingFor),
// carried as the row holds it, a newer build's value included: the committer
// holds it to its rule, and admission already refused what no checkout
// writes (boxadmit.go's step 7). A person's push states nothing; a check's
// box pushes nothing. ONE answer for the landing and for the measure before
// receive-pack (commandsRefusal), so a push is measured carrying what it will.
func (a admission) writtenFor() types.WrittenFor {
	if a.box == nil {
		return types.WrittenFor{}
	}
	return types.ForTurn(a.box.WorkingFor)
}

// refs is the freshness every read naming this request's REFS syncs at (v0
// and push advertisements, `ls-refs`, a push's receive): LINEARIZABLE for a
// person, and the run's SESSION at its floor for a box, which already holds
// all it was shown (boxadmit.go, "A box reads at its run's floor"). A check's
// box reads no ref (admission.noRefs); its fetch syncs at [admission.objects].
func (a admission) refs() statelog.Freshness { return a.fresh }

// objects is the freshness a fetch's wants are first looked for at (fetch.go).
// STALE for a person and a coding box: an object id has no level, and a
// missing want is what the one sync at the log's head is for. A CHECK'S BOX's
// is its check's SESSION, which its commit's push precedes, so the copy holds
// the commit and its tree. Stale, most nodes' caches on a fleet would lack the
// commit and pay a code-log barrier per box, one internal/worklet's codeLogDay
// does not price.
func (a admission) objects() statelog.Freshness {
	if a.check != nil {
		return a.fresh
	}
	return statelog.Freshness{Level: statelog.ReadStale}
}

// boxFloor is the run a box's ref answers raise the floor of and the floor
// its admission read, and false for a person's request, which has none.
func (a admission) boxFloor() (BoxRunRef, statelog.Position, bool) {
	if a.box == nil {
		return BoxRunRef{}, statelog.Position{}, false
	}
	return a.box.Ref, a.box.Floor, true
}

// holdRefs holds, on a CODING BOX'S run's row, every seat's branch its push
// names ([gitlet.Holdable]) that the run does not hold yet, the commands pre
// refused left out, in ONE write (Runs.Hold) BEFORE receive-pack runs or
// anything is stored or proposed. Its parked run depends on what it pushed, so
// the branch is held as long as the run's row exists (gitlet.RefHoldKey). The
// keys join the admission's copy of the row, so a held branch costs the run
// store nothing again, a rerun over a rebuilt incarnation included. Nothing for
// a person's push.
//
// Held at the push, not derived from push rows: that would be a second
// mechanism, reading every unended run's pushes, beside the row's union the
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
		if _, refused := pre[c.Ref]; refused || !gitlet.Holdable(c.Ref) {
			continue
		}
		// A ref named twice (the proposal refuses it) is one key.
		if key := gitlet.RefHoldKey(a.repo.ID, c.Ref); !a.box.Holds[key] && !want[key] {
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

// resyncKey is whose re-sync bucket the request's linearizable reads draw on
// (boxlimit.go): a CODING BOX's run, by launch, on its seat
// (gitlet.RunResyncs); a CHECK'S BOX's repository's checks
// (gitlet.CheckKey.Resyncs), code nobody vouches for; false for a person,
// whose ref reads are linearizable by right and priced in the census's
// clients' term.
func (a admission) resyncKey() (scope, principal string, ok bool) {
	switch {
	case a.check != nil:
		scope, principal = a.check.Resyncs()
		return scope, principal, true
	case a.box != nil:
		scope, principal = gitlet.RunResyncs(a.box.Seat, a.box.Ref.LaunchID)
		return scope, principal, true
	}
	return "", "", false
}

// floorWords is how an answer at a box's floor names it — what it has been
// shown, what it may do instead, and whose bucket ran out, the principal's
// and the scope's (gitlet.NotRejudged): an executor's, or a check's.
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
// (landing.EngineCommitter): for a CODING BOX the engine's own
// (Signer.Committer), so a box claiming it is refused forged_committer
// (package doc, "A coding box is admitted by its run"); "" for a person. An
// error is a node holding no epoch, which judges no box's push.
func (a admission) engineCommitter(s Signer) (string, error) {
	if a.box == nil {
		return "", nil
	}
	return s.Committer()
}

// share is whose transfer the admitted request is (gitproc.Cmd.Share), what the
// transfer lane decides a contended slot by and charges spend to.
//   - A PERSON, or a token bound to a human seat, is keyed by the seat's
//     handle: all their credentials are one principal, as the grant reads them.
//   - A MACHINE (a credential acting as nobody) is keyed by the credential,
//     never its owning account (iam.RecordOwner): each CI runner's token gets
//     its own turn, as the fetch spool keys it.
//   - A CODING BOX is a RUN, keyed by launch; its class holds at most half the
//     lane (gitproc.ShareClass.Cap), so a person never queues behind it.
//   - A CHECK'S BOX is a run keyed by REPOSITORY (`check:<id>`), never attempt:
//     a push fans out a check per name, and one repository's checks take one
//     turn of the class, as an executor does.
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
// recorded under: `unbound` for a token bound to nobody, whose remedy is a
// binding in the identity directory rather than a grant, and the domain's
// not_permitted for any other principal.
func grantRefusal(p gitlet.Principal) AuditRefusal {
	if p.Kind == gitlet.PrincipalOperator && p.Seat == "" {
		return RefusalUnbound
	}
	return AuditRefusal(gitlet.RefusalNotPermitted)
}

// refuses reports whether repo, as a read found it, refuses op: a deleted or
// purging one every op; an archived, importing or pull-mirror one a push. A
// stale read's refusal is confirmed linearizable before it is answered.
func refuses(repo gitlet.RepoRef, op auth.GitOp) bool {
	switch repo.Status {
	case gitlet.RepoStatusDeleted, gitlet.RepoStatusPurging:
		return true
	case gitlet.RepoStatusArchived, gitlet.RepoStatusImporting:
		return op == auth.GitPush
	}
	return repo.Mirrored && op == auth.GitPush
}

// gone reports a deleted or purging repository: one that answers nothing.
func gone(status gitlet.RepoStatus) bool {
	return status == gitlet.RepoStatusDeleted || status == gitlet.RepoStatusPurging
}

// identityUnknown answers a request whose principal this node could not tell
// (the guard's unknown arm, or no guard at all: iam.ErrUnresolved) with 503
// and the identity estate's own Retry-After, logged.
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

// unavailable answers a request the code log could not be read for here (a
// linearizable read refused, or no data node answered): 503 with Retry-After,
// logged.
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
// (gitlet.RepoGone), so every entry point says what the push decide says.
func (s *Server) goneSentence(ref gitlet.RepoRef) string {
	return gitlet.RepoGone{ID: ref.ID, Name: ref.Name, Status: ref.Status, DeletedAt: ref.DeletedAt,
		Retention: s.o.DeletedRetention(), ManagedBy: ref.ManagedBy}.Detail()
}

// serviceFor is the git process an advertisement runs: only the service
// auth.GitAccess's class allows (upload-pack for a fetch, receive-pack for a
// push), so a request classified a fetch never runs receive-pack and the
// guard's and drain's reading holds (FuzzServiceFor). Another service, one
// named twice or an unparsable query is refused (ok false); none named is the
// dumb protocol's (dumb true).
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
