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

// A BOX'S ADMISSION: admit for a request under /box/git/, its credential a
// box's own (package doc, "A coding box is admitted by its run"). The Basic
// user name tells the two boxes apart: [BoxUser], a seat's coding box,
// admitted by its run's row (admitRun), and [gitlet.CheckUser], a check's box,
// by its attempt's row (checkadmit.go). The steps run in order, each refusal
// its own answer: 1 NAME, as admit's; 2 CREDENTIAL; 2a IN FLIGHT; 3 RUN;
// 4 GRANT BY NAME; 5 STOP; 6 REPOSITORY, at 8 FRESHNESS; 7 THE SEAT'S GRANT,
// each explained where admitRun takes it. A box's push is no operator_acted
// (pushAudit): its push row names its run (gitlet.RunProvenance.RunID), and its
// fetch of a `visibility: unit` repository is recorded under its seat
// (fetched).

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

	// 2 THE CREDENTIAL: a box's token and nothing else admits a box. The
	// guard's resolution (iam.From) is never read here, so neither an
	// operator's bearer nor a person's cookie admits anything, and a run's
	// token presented as a check's, or a check's as a run's, verifies as
	// neither (boxCredential). Refusals are ChallengeBox's.
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

	// 2a ITS PLACE IN FLIGHT: at most MaxRunRequests of the run's requests at
	// once on this node, counted before anything is read for it (boxlimit.go).
	if !s.holdRun(ctx, ref.LaunchID) {
		b.unavailable("run_busy", busySentence("this run"))
		return admission{}, false
	}

	// 3 THE RUN, three-valued: a store that cannot be read is 503, never 401,
	// since the credential was good.
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

	// 4 THE RUN'S GRANT, by the name it was given, BEFORE the code log: step
	// 6's refusing branch appends a barrier to the log, so a box looping
	// `git ls-remote` over guessed names would spend the barrier budget until
	// the log refused every push of the company (log_full).
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

	// 5 THE STOP, a push only. A pause with stop_running is the engine's
	// emergency stop, which does not fence a detached executor, so without
	// this its box kept pushing as the seat. A pause without it stops new work
	// only, and no fetch is refused for a pause: a read changes nothing, and a
	// box winding down may need one. Unknown pauses are 503, never "not
	// stopped".
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

	// 8 THE FRESHNESS, which step 6 reads at: the run's SESSION, whose floor
	// every ref answer and landed push raises (raiseFloor). A row naming no
	// floor reads at the log's head, a re-sync its bucket pays for
	// (boxlimit.go); spent, it reads this node's copy as it stands.
	adm := admission{name: name, box: &run, spool: boxSpool, fresh: runSession(run.Floor)}
	var spent *costlimit.Refusal
	if adm.fresh.Level == statelog.ReadLinearizable {
		if spent = s.boxResync(adm); spent != nil {
			adm.fresh = statelog.Freshness{Level: statelog.ReadStale}
		}
	}
	adm.linearizable = adm.fresh.Level == statelog.ReadLinearizable

	// 6 THE REPOSITORY, given: judged again at the log's head where the
	// session's answer would refuse (admit's rule that no refusal rests on a
	// stale read), while the run's re-syncs last; spent, the session's answer
	// stands. A name that now names another repository is refused as not
	// given.
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
	// A REDIRECTED NAME is answered for the repository it redirects to, A
	// PUSH INCLUDED, where admit refuses a person's push to tell them the new
	// name: the grant is the repository's id, and nothing in the box changes
	// the remote its launch gave it. Under its current name, so what the
	// request logs, heats and records names it as every other read does.
	adm.name, adm.repo = repo.Name, repo

	// 7 THE SEAT'S GRANT, from this node's CURRENT chart. A node with none, or
	// confirmed behind the company's configuration, judges no box and answers
	// 503: the configuration it failed to apply may be the one that revoked
	// the seat, and /ready already drains it, so the retry reaches another
	// node.
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
	// WHOEVER THE RUN'S TURN WORKS FOR (runCeiling) is held first to the rule
	// every carrier is held to, the committer's for the box's pushes included,
	// never a reading of this package's own: a kind this build knows
	// (gitlet.KindUnknownToThisBuild), naming somebody a checkout writes
	// (gitlet.RunCarrierInvalid).
	switch carrier, invalid := run.WorkingFor, gitlet.RunCarrierInvalid(run.Seat, run.WorkingFor); {
	case gitlet.KindUnknownToThisBuild(carrier):
		// A NEWER BUILD'S RUN, in write.go's words for a value only a newer
		// build reads: retryable, since the next request may reach that build.
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
		// run to wait for: a stranger (ADR-0110), the zero kind, a handle or
		// login out of its grammar, a credential naming no app. An app this
		// build does not know passes: the bound is the credential's own
		// grant, which no app moves, and the push's committer answers the app
		// as a node of another build.
		b.refuse("not_permitted", fmt.Sprintf("run %s's row names whom its turn works for as no checkout writes "+
			"it, so its box reads and pushes nothing: %s", run.Ref.TurnID, sentence(invalid.Error())))
		return admission{}, false
	}
	// At most what the row, the seat and whoever the turn works for allow; a
	// push's commands are judged per ref on this grant, as a person's are.
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
// (gitlet.Grant.Under), WHOEVER ITS TURN WORKS FOR, or nil for a run its seat
// alone bounds:
//   - a COLLEAGUE SEAT whose ask woke the turn (BoxRun.Ceiling): its own grant
//     from this chart (M3-decisions U40 (a)), the seat's own work aside;
//   - a CREDENTIAL acting as no seat (BoxRun.WorkingFor): its own grant
//     (gitlet.CredentialPrincipal), which reads every repository and pushes
//     none, so the box pushes the seat's own work alone (ADR-0110);
//   - a person's seat, or nobody: none.
//
// admitRun's step 7 first answers the rows this must not bound: a kind this
// build does not know, and one no checkout writes, read here as nobody's.
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

// ChallengeBox is a box's admission's CREDENTIAL step on its own: a request
// presenting no credential this fleet signed (no Basic credential, a user name
// other than [BoxUser] and [gitlet.CheckUser], or a token signed refuses for
// that user name) gets git's 401 challenge in the box's own words, never the
// person's, which name an API token a box must never hold; it is logged at
// DEBUG (code_box_challenged) with why and never the value, and reported true;
// a signed token is left unanswered, and false. Exported for the API's box routes on a node not serving Gitlet, which tell a
// box why only once it presents a signed token (M3-decisions U8): one step, so
// both challenge alike and nothing else reads a box's credential
// (TestNothingAfterAdmissionAsksWhichAdmitterRan). signed is the caller's
// verify ([VerifyBox]).
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

// boxRefusal answers one admitted token's refusal. Past the credential step a
// refusal takes a token this fleet signed, so it logs INFO (code_box_refused)
// or, for a 503, WARN (code_box_unavailable); before it, the traffic is
// anyone's on a public route, so ChallengeBox logs it at DEBUG, never a line an
// attacker can print at INFO. Neither logs the token.
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

// attrs is a refusal's log line: the run's turn, launch and seat, or the
// check's attempt, and the repository.
func (b boxRefusal) attrs(reason string) []any {
	if c := b.check; c != nil {
		return []any{"reason", reason, "repo_id", c.Attempt.RepoID, "oid", c.Attempt.OID,
			"check", c.Attempt.Name, "attempt", c.Attempt.Attempt, "repo", b.name, "path", b.r.URL.Path}
	}
	return []any{"reason", reason, "turn_id", b.ref.TurnID, "launch_id", b.ref.LaunchID, "seat", b.seat,
		"repo", b.name, "path", b.r.URL.Path}
}

// boxHoldFailed answers a coding box's push whose run's hold on the seat
// branches it names (admission.holdRefs) could not be written, with nothing
// received, stored or proposed, since its parked run depends on what it
// pushed: 403 holds_full at the row's bound, else 503 holds_unwritten, the
// hold UNKNOWN and never "held", for a retry that may reach a node that can
// write it. A box's fetch holds nothing: its v2 wants are object ids and
// `ls-refs` lists by prefix, so no request names a ref it depends on; its
// checkout is held at its launch.
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

// raiseFloor raises the run's floor on its row (Runs.RaiseFloor) to pos, the
// copy a ref answer is about to be served from or an applied push's row, and
// answers a failure as a *floorUnraised; nothing for a person's request, or
// where pos is not past the admission's floor. Every ref answer raises it
// before its first byte (M3-decisions M3-O56): git builds a push's thin pack
// against its own advertisement's refs, and a balancer may send the
// receive-pack to another node, which must sync to them; it also keeps the
// box's reads MONOTONIC. A floor cache written behind would reopen that. Under
// context.WithoutCancel, bounded by statelog.ReadBudget: a box that hangs up
// still holds what it was shown. The row keeps the bridged tool log beside it
// (sandbox.BridgeLog), so a raise moves its facts alone
// (BenchmarkABoxRequestOnItsRun).
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
