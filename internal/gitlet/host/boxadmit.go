package host

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/costlimit"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/toolname"
)

// A BOX'S ADMISSION: admit for a request under /box/git/ — the same answers to
// the same requests, its credential a box's own. Two boxes reach Gitlet
// there, told apart by the Basic user name their credential presents: a seat's
// CODING BOX (BoxUser), admitted by its run's row with its grant the run's on
// top of the seat's — admitRun, the steps below — and a CHECK'S BOX
// (gitlet.CheckUser), admitted by its attempt's row to fetch one commit
// (checkadmit.go).
//
// # In order, each refusal its own answer
//
//  1. NAME — as admit's: the URL's segment, one `.git` stripped, held to the
//     grammar.
//  2. CREDENTIAL — git's Basic, user name [BoxUser] with the run's token as the
//     password ([BoxTokens.Verify]), or [gitlet.CheckUser] with the check's
//     ([CheckTokens.Verify]), each verified under [BoxKeyDomain] — and each
//     user name's own token alone: a run's token presented as a check's, or a
//     check's as a run's, verifies as neither ([boxCredential]). A box's token
//     and NOTHING ELSE admits a box: the guard's resolution of the request
//     (iam.From) is never read here, so neither an operator's bearer — which
//     the guard marks anonymous on this unguarded route — nor a signed-in
//     person's cookie, which it resolves there, admits anything, and an
//     operator token presented as the password fails the verify like any
//     other value. Each refusal is git's 401 challenge in the box's own words
//     (auth.GitChallengeSaying), never the person's, which names an API token
//     — the one credential a box must never hold. The step is [ChallengeBox],
//     which the API's box routes on a node not serving Gitlet run too. A
//     check's token goes on to admitCheck; a run's to admitRun, whose steps
//     follow.
//     2a. IN FLIGHT — at most MaxRunRequests of the run's requests at once
//     on this node, the token's launch counted before anything is read for
//     it; past them 503, reason run_busy (boxlimit.go).
//  3. RUN — the row its token names ([Runs.Run]), THREE-VALUED: a store that
//     cannot be read is 503 and never 401 (the credential was good, and
//     nothing about it is known to be wrong); an unknown run, a launch the row
//     has replaced and a run whose job has left its box are 403.
//  4. GRANT, BY THE NAME THE RUN WAS GIVEN — before any read of the code log.
//     A box is untrusted traffic, and step 6's refusing branch appends a
//     barrier to the code log: resolved first, a box probing names it was
//     never given — `git ls-remote` in a loop over guessed names — appended
//     one barrier a guess, outside the census the log's barrier budget was
//     sized by, until the log reached its ceiling and refused every push of
//     the company (log_full). Granted first, step 6 is reached only for the at
//     most a run's repositories the row names. A push of a repository the run
//     was given to read is refused here too.
//  5. STOP — a push only: a seat whose pause asked its running work to stop
//     ([Runs.Stopped]) pushes nothing from a box. A person's pause with
//     stop_running is the engine's emergency stop, and a detached executor
//     is deliberately not fenced by it — it outlives its turn — so without
//     this a box kept pushing to the company's repositories as the seat after
//     a person stopped it. A pause without stop_running stops new work only,
//     and a running job is work in flight, so its pushes land; and a fetch is
//     never refused for a pause: a read changes nothing, and a box winding
//     down may need one. Unknown — a node that has not read the pauses yet — is
//     503, never "not stopped".
//  6. REPOSITORY — resolved at the run's session (step 8), and again
//     LINEARIZABLE where that answer would refuse — not found, a status that
//     refuses op, or ANOTHER repository under the name: admit's rule that a
//     refusal is never decided on a stale read, reached now only for a name
//     the run was GIVEN — and only while the run's re-syncs last
//     (boxlimit.go): spent, the session's answer stands — a name not found
//     there, or naming another repository, refused saying it was not judged
//     again. A name a rename left redirecting is answered for the
//     repository it redirects to, under its current name, A PUSH INCLUDED,
//     where admit refuses a person's push to tell them the new name: the
//     grant is the repository's id, and the box's remote is the name its
//     launch gave it, which nothing in the box changes. A name that now names
//     another repository is refused as not given.
//  7. THE SEAT'S GRANT — from the serving node's CURRENT chart (Options.Roster),
//     held to the run's (gitlet.Grant.Within): a box does at most what both its
//     row and its seat's own grant allow. And in a run whose turn a colleague
//     seat woke (BoxRun.Ceiling), held to that colleague's own grant from the
//     same chart (gitlet.Grant.Under) — what a seat does for a colleague is
//     decided for the colleague too, as its code tools' reads and writes are
//     (M3-decisions U40 (a)): a fetch the colleague could not make, and a
//     command outside the seat's own work the colleague could not make, are
//     refused, and its own work — the branches under its own namespace, and
//     every branch of a repository it owns — stays its own. In a run whose
//     turn a credential acting as no seat woke (BoxRun.WorkingFor), held to
//     that credential's own grant the same way, which reads every repository
//     and pushes none: the box fetches what its run was given and pushes its
//     seat's own work alone, as the turn's own code writes for the credential
//     do (ADR-0110). Whom the row names is held first to the rule every
//     carrier is held to (gitlet.KindUnknownToThisBuild,
//     gitlet.RunCarrierInvalid): a run whose row names a stranger, or names
//     whom its turn works for as no checkout writes it — the zero kind, a
//     handle or a login out of its grammar, a credential naming no app — is
//     refused every request, the closed end; and one naming it by a kind
//     this build does not know, a newer build's, is answered 503
//     carrier_unknown, judged by no bound this node could only guess at
//     (runCeiling): a node of a newer build judges it, and the harness's
//     push or fetch again reaches one. A node with
//     no chart, or one confirmed behind the company's configuration
//     (Options.Current), judges no box's grant and answers 503: the
//     configuration it failed to apply may be the one that revoked the seat,
//     and the box's retry reaches another node, since /ready already drains
//     this one at the balancer. A fetch the seat — or the colleague — may not
//     read, a `visibility: unit` repository outside its unit, is 403. A push's
//     commands are judged per ref on the same grant, at its advertisement and
//     its receive, as a person's are.
//  8. FRESHNESS — every read of the request's refs is at the run's SESSION:
//     its floor, which every ref answer the box is shown and every push it
//     lands raises (raiseFloor). A ZERO floor reads LINEARIZABLE, a re-sync
//     the run's bucket pays for, and as this node's copy stands once it is
//     spent.
//
// # Logged by who could have caused it
//
// A refusal reached WITHOUT a credential this fleet signed — step 2 — is
// unauthenticated traffic on a public route that anyone who can reach the
// listener produces at will, so it is DEBUG (code_box_challenged) and never a
// line an attacker can print at INFO, as the tool bridge's resolve decided for
// the same reason; every refusal from step 3 on takes a token the fleet
// signed, and is INFO (code_box_refused), and a 503 WARN (code_box_unavailable),
// each naming its reason, the run's turn and launch and the seat — or the
// check's attempt — and the repository where known, and never the token.
//
// # A box's pushes are audited by nothing but their rows
//
// operator_acted is a PERSON'S record (pushAudit). A box's push is recorded by
// its push row, which names its run (gitlet.RunProvenance.RunID), and a box's
// refusal by its log line. Its fetch of a `visibility: unit` repository is a
// read like any other, recorded under its seat (fetched).
//
// # A box's push holds what it pushes
//
// Every seat's branch a box's push names, as judged, is held on its run's row
// before anything of the push is received, stored or proposed
// (admission.holdRefs): what the box pushed is what its parked run depends on.
// A row at its bound is 403 holds_full, and a run store that cannot be written
// 503 holds_unwritten — never the push sent unheld. A box's FETCH holds
// nothing: under protocol v2 its wants are object ids and `ls-refs` lists by
// prefix, so no request names a ref the box depends on; its checkout is its
// launch's grant, held at the launch.

// The box's challenges, each a sentence of the box's own credential: what to
// present, never an API token.
const (
	boxNoneSentence = "authentication required: a coding box authenticates as `" + BoxUser +
		"` with its run token, and a check's box as `" + gitlet.CheckUser + "` with its check's token, each " +
		"answered by the engine's credential helper from $CREWLET_GIT_TOKEN"
	boxUserSentence = "a coding box authenticates as `" + BoxUser + "` with its run token, and a check's " +
		"box as `" + gitlet.CheckUser + "` with its check's token"
	boxTokenSentence = "this run token is not valid here: forged, expired, or signed under a key this " +
		"node's secrets.keys does not hold"
	checkTokenSentence = "this check token is not valid here: forged, expired with its check, or signed " +
		"under a key this node's secrets.keys does not hold"
)

// admitBox is admit for a box: the name, then the credential, whose user name
// says whose box it is — a check's, admitted by admitCheck, or an executor's,
// by admitRun.
func (s *Server) admitBox(ctx context.Context, w http.ResponseWriter, r *http.Request, op auth.GitOp) (admission, bool) {
	raw := r.PathValue(PathRepo)
	name := strings.TrimSuffix(raw, ".git")
	if !gitref.ValidRepoName(name) {
		auth.GitText(w, http.StatusNotFound, notFoundSentence(raw))
		return admission{}, false
	}

	// 2 THE CREDENTIAL.
	var (
		run   BoxRunRef
		check CheckRunRef
		user  string
	)
	if ChallengeBox(w, r, func(presented, token string) (signed bool) {
		user = presented
		run, check, signed = boxCredential(s.o.Boxes.Tokens, s.o.Boxes.Checks, presented, token)
		return signed
	}, s.log) {
		return admission{}, false
	}
	if user == gitlet.CheckUser {
		return s.admitCheck(ctx, w, r, op, name, check)
	}
	return s.admitRun(ctx, w, r, op, name, run)
}

// admitRun is admit for a coding box whose run's token admitBox verified: the
// steps above from the run on, in order.
func (s *Server) admitRun(ctx context.Context, w http.ResponseWriter, r *http.Request, op auth.GitOp, name string,
	ref BoxRunRef) (admission, bool) {

	b := boxRefusal{s: s, w: w, r: r, ref: ref, name: name}

	// 2a ITS PLACE IN FLIGHT, before anything is read for it (boxlimit.go).
	if !s.holdRun(ctx, ref.LaunchID) {
		b.unavailable("run_busy", busySentence("this run"))
		return admission{}, false
	}

	// 3 THE RUN.
	run, found, err := s.o.Boxes.Runs.Run(ctx, ref.TurnID)
	switch {
	case err != nil:
		b.unavailable("run_unread", "the run store could not be read here: "+sentence(err.Error()))
		return admission{}, false
	case !found:
		b.refuse("run_unknown", fmt.Sprintf("executor %s is unknown here: it ended and its record was "+
			"removed, or it never launched", ref.TurnID))
		return admission{}, false
	case run.Ref.LaunchID != ref.LaunchID:
		b.refuse("run_superseded", fmt.Sprintf("this token is a finished job's: run %s has launched again "+
			"since, and only its current job's token admits a box", ref.TurnID))
		return admission{}, false
	case !run.Live:
		b.refuse("run_ended", fmt.Sprintf("executor %s has ended (%s): its box keeps no git access",
			ref.TurnID, run.Status))
		return admission{}, false
	}
	b.seat = run.Seat

	// 4 THE RUN'S GRANT, by the name it was given — before the code log.
	g, given := run.grantNamed(name)
	switch {
	case !given:
		b.refuse("not_granted", fmt.Sprintf("this run was not given %s: it has checked out %s; check a "+
			"repository out with "+toolname.CodeCheckout+" before you fetch or push it", strconv.Quote(name), grantedNames(run)))
		return admission{}, false
	case op == auth.GitPush && g.Access < gitlet.AccessWrite:
		b.refuse("read_only", fmt.Sprintf("this run was given %s to read: a push needs access: write in "+
			"its "+toolname.CodeCheckout+" call", strconv.Quote(name)))
		return admission{}, false
	}

	// 5 THE STOP, for a push.
	if op == auth.GitPush {
		by, serr := s.o.Boxes.Runs.Stopped(ctx, run.Seat)
		switch {
		case serr != nil:
			b.unavailable("pauses_unread", "this node has not yet read which seats are paused")
			return admission{}, false
		case by != "":
			b.refuse("seat_stopped", fmt.Sprintf("seat %s was paused by %s, who asked its running work to "+
				"stop: a box of its runs pushes nothing until the seat is resumed", run.Seat, by))
			return admission{}, false
		}
	}

	// 8 THE FRESHNESS, which step 6 reads at.
	// A RUN WHOSE ROW NAMES NO FLOOR reads its refs at the log's head, a
	// re-sync its bucket pays for like any other (boxlimit.go): spent, it
	// reads them as this node's copy holds them — the floor it has, which is
	// none.
	adm := admission{name: name, box: &run, spool: boxSpool, fresh: runSession(run.Floor)}
	var spent *costlimit.Refusal
	if adm.fresh.Level == statelog.ReadLinearizable {
		if spent = s.boxResync(adm); spent != nil {
			adm.fresh = statelog.Freshness{Level: statelog.ReadStale}
		}
	}
	adm.linearizable = adm.fresh.Level == statelog.ReadLinearizable

	// 6 THE REPOSITORY, given — judged again at the log's head where the
	// session's answer would refuse, while the run's re-syncs last.
	repo, err := s.o.Estate.Resolve(ctx, gitlet.ResolveQuery{Name: name, Fresh: adm.fresh})
	if !adm.linearizable && spent == nil && (errors.Is(err, gitlet.ErrRepoNotFound) ||
		(err == nil && (refuses(repo, op) || repo.ID != g.RepoID))) {
		if spent = s.boxResync(adm); spent == nil {
			adm.linearizable = true
			repo, err = s.o.Estate.Resolve(ctx, gitlet.ResolveQuery{Name: name,
				Fresh: statelog.Freshness{Level: statelog.ReadLinearizable}})
		}
	}
	// ANSWERED AT THE RUN'S FLOOR once its re-syncs are spent, saying so.
	floorNote := ""
	if spent != nil {
		floorNote = " " + s.notRejudged(adm, spent)
	}
	switch {
	case errors.Is(err, gitlet.ErrRepoNotFound):
		auth.GitText(w, http.StatusNotFound, notFoundSentence(name)+floorNote)
		return admission{}, false
	case err != nil:
		s.unavailable(w, r, name, err)
		return admission{}, false
	case repo.ID != g.RepoID:
		b.refuse("not_granted", fmt.Sprintf("%s is no longer the repository this run was given (renamed "+
			"since it was checked out); check it out again with "+toolname.CodeCheckout+" under its new name%s",
			strconv.Quote(name), floorNote))
		return admission{}, false
	}
	// ANSWERED UNDER ITS CURRENT NAME, as admit answers a fetch it lets
	// through a redirect: what the request logs, heats and records names
	// the repository as every other read does.
	adm.name, adm.repo = repo.Name, repo

	// 7 THE SEAT'S GRANT, from this node's current chart.
	roster := s.o.Roster()
	switch {
	case roster == nil:
		b.unavailable("no_chart", fmt.Sprintf("this node holds no chart yet to judge seat %s's grant by",
			run.Seat))
		return admission{}, false
	case s.o.Current != nil && !s.o.Current():
		b.unavailable("behind_config", "this node is behind the company's configuration and judges no "+
			"seat's grant")
		return admission{}, false
	}
	// WHOEVER THE RUN'S TURN WORKS FOR, which this node judges the box for too
	// (runCeiling), held first to the one rule every carrier is held to, never
	// to a reading of this package's own: a kind this build knows
	// (gitlet.KindUnknownToThisBuild), and somebody a checkout writes on a row
	// (gitlet.RunCarrierInvalid) — the rule the committer holds every push of
	// the box to, which states the same carrier.
	switch carrier, invalid := run.WorkingFor, gitlet.RunCarrierInvalid(run.Seat, run.WorkingFor); {
	case gitlet.KindUnknownToThisBuild(carrier):
		// A NEWER BUILD'S RUN, in the words this package gives every value
		// only a newer build reads (write.go's protection): retryable, since
		// the harness's next request may reach a node of that build.
		asked := "fetch"
		if op == auth.GitPush {
			asked = "push"
		}
		b.unavailable("carrier_unknown", fmt.Sprintf("this node's build does not know whom run %s's turn works "+
			"for, so it cannot judge this %s: a node of a newer build judges it, and the harness's %s again "+
			"reaches one", run.Ref.TurnID, asked, asked))
		return admission{}, false
	case invalid != nil && !gitlet.IsUnknownToThisBuild(invalid):
		// WHAT NO CHECKOUT WRITES is the closed end, never a newer build's
		// run to wait for: a stranger, for whom no checkout is made and
		// nothing is read or pushed (ADR-0110), and what no build composes
		// — the zero kind, a handle or a login out of its grammar, a
		// credential naming no app, which every reader of a carrier takes
		// for a stranger. An app this build does not know is a newer
		// build's, and passes: the box's bound is the credential's own
		// grant, which no app moves, and its push's committer answers the
		// app as a node of another build.
		b.refuse("not_permitted", fmt.Sprintf("run %s's row names whom its turn works for as no checkout writes "+
			"it, so its box reads and pushes nothing: %s", run.Ref.TurnID, sentence(invalid.Error())))
		return admission{}, false
	}
	adm.grant = gitlet.GrantOf(adm.principal(), repo, roster).Within(g.Access)
	if ceiling := runCeiling(run, repo, roster); ceiling != nil {
		adm.grant = adm.grant.Under(*ceiling)
	}
	if op == auth.GitFetch && !adm.grant.MayRead() {
		b.refuse("not_permitted", adm.grant.ReadRefusal())
		return admission{}, false
	}
	return adm, true
}

// runCeiling is the grant a run's box is held under beside its seat's own
// (gitlet.Grant.Under) — WHOEVER ITS TURN WORKS FOR, as the run's row names
// them — and nil for a run its seat alone bounds:
//
//   - A COLLEAGUE SEAT whose ask woke the turn (BoxRun.Ceiling): that seat's
//     own grant, from this node's chart, so the box fetches nothing the
//     colleague could not read and pushes nothing outside the seat's own work
//     the colleague could not push.
//   - A CREDENTIAL acting as no seat (BoxRun.WorkingFor): the credential's own
//     grant (gitlet.CredentialPrincipal), which reads every repository and
//     pushes to none, so the box fetches every repository its run was given,
//     as the credential itself reads, and pushes the seat's own work alone —
//     its own namespace and the repositories it owns — as the turn's own code
//     writes for that credential do (ADR-0110). Bounded by its seat alone, the
//     box pushed every branch the seat may, for a token that pushes to no
//     repository itself.
//   - A PERSON'S SEAT, and NOBODY — a turn nobody woke, the seat's own, and a
//     row a build before the carrier wrote: none, a person bounding nothing.
//
// The rows the admission answers before asking it (admitRun's step 7), by the
// rule every carrier is held to: a row naming whom its turn works for by a
// kind this build does not know, which only a newer build writes and whose
// bound is that build's to judge (503 carrier_unknown, saying a node of a
// newer build judges the request and the harness's next push or fetch reaches
// one — never held to a bound this build guessed, nor refused as a run no
// build launched); and a row naming a stranger, for whom no checkout is made
// and no repository is read or written, the seat's own work included
// (ADR-0110), or naming whom its turn works for as no checkout writes it —
// the zero kind among them, which this function would read as nobody's — whose
// box is refused every request.
func runCeiling(run BoxRun, repo gitlet.RepoRef, roster gitlet.Roster) *gitlet.Grant {
	var g gitlet.Grant
	switch w := run.WorkingFor; {
	case run.Ceiling != "":
		g = gitlet.GrantOf(gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: run.Ceiling}, repo, roster)
	case w != nil && w.Kind == types.WorkingForCredential:
		g = gitlet.GrantOf(gitlet.CredentialPrincipal(w.Login), repo, roster)
	default:
		return nil
	}
	return &g
}

// runSession is what a box's reads of its refs sync at: SESSION at its run's
// floor, or LINEARIZABLE for a run whose row names none — no launch that gave
// it a repository leaves its floor zero (the launch resolves a base), and the
// barrier is the safe direction for a row that somehow does.
func runSession(floor statelog.Position) statelog.Freshness {
	if floor.IsZero() {
		return statelog.Freshness{Level: statelog.ReadLinearizable}
	}
	return statelog.FloorAt(statelog.ReadSession, floor)
}

// grantedNames is every repository a run was given, as a refusal names them.
func grantedNames(run BoxRun) string {
	if len(run.Repos) == 0 {
		return "no repository"
	}
	names := make([]string, len(run.Repos))
	for i, g := range run.Repos {
		names[i] = strconv.Quote(g.Name)
	}
	return strings.Join(names, ", ")
}

// ChallengeBox is a box's admission's CREDENTIAL step (step 2) on its own: it
// answers a request that presents no credential this fleet signed — no Basic
// credential at all, a user name other than [BoxUser] and [gitlet.CheckUser], or a
// password signed refuses for the user name that presented it — with git's
// 401 challenge in the box's own words, logs it on log at DEBUG
// (code_box_challenged) with why (`none`, `user` or `token`) and never the
// value presented, and reports true; a request presenting a token signed
// accepts is left unanswered, and false.
//
// EXPORTED FOR THE ONE OTHER PLACE a box's credential is decided before an
// admission can run: the API's box routes on a node that does not serve Gitlet
// now, which tell a box WHY only once it presents a token this fleet signed
// (internal/api; M3-decisions U8) — the box routes are public, and that
// reason is the deployment's state. ONE STEP FOR BOTH, so a box is challenged
// in the same words, at the same level, by a node serving Gitlet and
// by one that does not, and nothing reads a box's credential by a grammar
// of its own (TestNothingAfterAdmissionAsksWhichAdmitterRan holds every other
// reader of git's Basic out of this package). signed is the caller's verify,
// of the token the user name presented ([VerifyBox]): the admission's keeps
// the run or the check its token names.
func ChallengeBox(w http.ResponseWriter, r *http.Request, signed func(user, token string) bool,
	log *slog.Logger) bool {

	var why, words string
	switch user, password, presented := r.BasicAuth(); {
	case !presented:
		why, words = "none", boxNoneSentence
	case user != BoxUser && user != gitlet.CheckUser:
		why, words = "user", boxUserSentence
	case !signed(user, password):
		why, words = "token", boxTokenSentence
		if user == gitlet.CheckUser {
			words = checkTokenSentence
		}
	default:
		return false
	}
	log.Debug("code_box_challenged", "path", r.URL.Path, "reason", why)
	auth.GitChallengeSaying(w, words)
	return true
}

// boxRefusal answers one admitted token's refusal: what its run and its seat —
// or its check's attempt — and its repository are, for the line it logs.
type boxRefusal struct {
	s   *Server
	w   http.ResponseWriter
	r   *http.Request
	ref BoxRunRef
	// check is the attempt a check's box's token names; nil for a run's.
	check *CheckRunRef
	seat  string
	name  string
}

// refuse answers 403 in git's form, logged INFO code_box_refused.
func (b boxRefusal) refuse(reason, words string) {
	b.s.log.Info("code_box_refused", b.attrs(reason)...)
	auth.GitText(b.w, http.StatusForbidden, words)
}

// unavailable answers 503 with Retry-After UnavailableRetryAfter, the wait
// said in the sentence too since git reads no header, logged WARN
// code_box_unavailable: a node that could not judge the box, which the box's
// retry may find another node able to.
func (b boxRefusal) unavailable(reason, words string) {
	b.s.log.Warn("code_box_unavailable", b.attrs(reason)...)
	retry(b.w, UnavailableRetryAfter)
	auth.GitText(b.w, http.StatusServiceUnavailable, words+"; retry in "+seconds(UnavailableRetryAfter))
}

// attrs is a refusal's log line: its reason, the run's turn and launch and
// the seat — or the check's attempt: its repository's id, its commit, the
// check and the attempt's number — and the repository.
func (b boxRefusal) attrs(reason string) []any {
	if c := b.check; c != nil {
		return []any{"reason", reason, "repo_id", c.Attempt.RepoID, "oid", c.Attempt.OID,
			"check", c.Attempt.Name, "attempt", c.Attempt.Attempt, "repo", b.name, "path", b.r.URL.Path}
	}
	return []any{"reason", reason, "turn_id", b.ref.TurnID, "launch_id", b.ref.LaunchID, "seat", b.seat,
		"repo", b.name, "path", b.r.URL.Path}
}

// boxHoldFailed answers a coding box's push whose run's hold on the seat
// branches it names could not be written (admission.holdRefs), with nothing
// received, stored or proposed: 403 holds_full where the run's row holds the
// most keys one run may — the push would land a branch its parked run
// depends on unheld — and otherwise 503 holds_unwritten, the run store
// unreachable and the hold UNKNOWN, never "held": the box's retry may reach a
// node that can write it.
func (s *Server) boxHoldFailed(w http.ResponseWriter, r *http.Request, adm admission, err error) {
	ref, _, _ := adm.boxFloor()
	b := boxRefusal{s: s, w: w, r: r, ref: ref, seat: adm.principal().Seat, name: adm.name}
	if errors.Is(err, sandbox.ErrCodeHoldsFull) {
		b.refuse("holds_full", fmt.Sprintf("this run already holds the most repositories and branches one "+
			"run may (%d); nothing of this push was stored", sandbox.MaxCodeHolds))
		return
	}
	b.unavailable("holds_unwritten", "the run store could not be written here: "+sentence(err.Error()))
}

// raiseFloor raises the run a box's request was admitted for to pos — the
// copy a ref answer it is about to be served is read from, or an applied
// push's row — on the run's row (Runs.RaiseFloor), and answers a failure as a
// *floorUnraised. Nothing for a person's request, whose reads of refs are
// linearizable, and nothing where pos is at or below the floor the admission
// read: one compare-and-set on the row, taken only when the box is shown
// something newer than its floor already holds.
//
// WHAT IT MOVES is the run's whole row and never its job's bridged tool log,
// which is kept beside it (sandbox.BridgeLog), so a raise reads and writes back
// the row's facts alone — 1.29 ms at the median on three members with the log
// full, a row of 37 931 bytes (BenchmarkABoxRequestOnItsRun,
// internal/coord/kv), against 11.1 ms on 516 173 bytes when the row carried
// the log — as the read of the run every request of the box begins with moves
// it once (Runs.Run: 0.43 ms, against 5.8).
//
// UNDER context.WithoutCancel, bounded by statelog.ReadBudget: a box that
// hangs up once it has been shown refs still holds what it was shown, and the
// raise is one write to the coordination store.
//
// WHY EVERY REF ANSWER, and before its first byte (M3-decisions M3-O56): git
// builds a push's thin pack against the refs that push's OWN advertisement
// named, and a push is two HTTP requests a balancer may send to two nodes. An
// advertisement served by a node ahead of the box's floor and a receive-pack
// by a node held at it leaves the second without the pack's bases ("missing
// necessary objects"); raised at the advertisement, the floor makes the
// receive-pack's session sync reach them. The raises at an `ls-refs` and a
// v0 advertisement keep the box's reads MONOTONIC: it never sees a branch go
// back between two nodes. A v2 clone takes at most one raise, a push at most
// two. A per-node cache of floors written behind would reopen the split-push
// hazard, which is why the row is written before the answer.
func (s *Server) raiseFloor(ctx context.Context, adm admission, pos statelog.Position) error {
	ref, floor, box := adm.boxFloor()
	if !box || pos.IsZero() {
		return nil
	}
	if !floor.IsZero() {
		newer, err := floor.Before(pos)
		switch {
		case err != nil:
			return &floorUnraised{ref: ref, err: err}
		case !newer:
			return nil
		}
	}
	raise, cancel := context.WithTimeout(context.WithoutCancel(ctx), statelog.ReadBudget)
	defer cancel()
	if err := s.o.Boxes.Runs.RaiseFloor(raise, ref, pos); err != nil {
		return &floorUnraised{ref: ref, err: err}
	}
	return nil
}

// floorUnraised is a box's ref answer refused, before its first byte, because
// its run's floor could not be raised to it: a box shown refs its floor does
// not cover is a box whose next push to a node held at that floor fails with
// git's "missing necessary objects", so the request is refused and the box
// retries — 503 with Retry-After (syncFailed), reason floor_unraised.
type floorUnraised struct {
	ref BoxRunRef
	err error
}

func (e *floorUnraised) Error() string {
	return "the run store could not be written here: " + e.err.Error()
}
func (e *floorUnraised) Unwrap() error { return e.err }
