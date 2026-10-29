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
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A CODING BOX'S ADMISSION: admit for a request under /box/git/ — the same
// answers to the same requests, its credential the run's and its grant the run
// row's on top of the seat's.
//
// # In order, each refusal its own answer
//
//  1. NAME — as admit's: the URL's segment, one `.git` stripped, held to the
//     grammar.
//  2. CREDENTIAL — git's Basic, user name [BoxUser], the run's token as the
//     password, verified under [BoxKeyDomain] ([BoxTokens.Verify]). The run's
//     token and NOTHING ELSE admits a box: the guard's resolution of the
//     request (iam.From) is never read here, so neither an operator's bearer —
//     which the guard marks anonymous on this unguarded route — nor a
//     signed-in person's cookie, which it resolves there, admits anything, and
//     an operator token presented as the password fails the verify like any
//     other value. Each refusal is git's 401 challenge in the box's own words
//     (auth.GitChallengeSaying), never the person's, which names an API token
//     — the one credential a box must never hold. The step is [ChallengeBox],
//     which the API's box route on a node serving no code host runs too.
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
//     stop_running is the engine's emergency stop, and a detached coding run
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
//     the run was GIVEN. A name a rename left redirecting is answered for the
//     repository it redirects to, under its current name, A PUSH INCLUDED,
//     where admit refuses a person's push to tell them the new name: the
//     grant is the repository's id, and the box's remote is the name its
//     launch gave it, which nothing in the box changes. A name that now names
//     another repository is refused as not given.
//  7. THE SEAT'S GRANT — from the serving node's CURRENT chart (Options.Roster),
//     held to the run's (code.Grant.Within): a box does at most what both its
//     row and its seat's own grant allow. A node with no chart, or one
//     confirmed behind the company's configuration (Options.Current), judges
//     no box's grant and answers 503: the configuration it failed to apply may
//     be the one that revoked the seat, and the box's retry reaches another
//     node, since /ready already drains this one at the balancer. A fetch the
//     seat may not read — a `visibility: unit` repository outside its unit —
//     is 403. A push's commands are judged per ref on the same grant, at its
//     advertisement and its receive, as a person's are.
//  8. FRESHNESS — every read of the request's refs is at the run's SESSION:
//     its floor, which every ref answer the box is shown and every push it
//     lands raises (raiseFloor). A ZERO floor reads LINEARIZABLE.
//
// # Logged by who could have caused it
//
// A refusal reached WITHOUT a credential this fleet signed — step 2 — is
// unauthenticated traffic on a public route that anyone who can reach the
// listener produces at will, so it is DEBUG (code_box_challenged) and never a
// line an attacker can print at INFO, as the tool bridge's resolve decided for
// the same reason; every refusal from step 3 on takes a token the fleet
// signed, and is INFO (code_box_refused), and a 503 WARN (code_box_unavailable),
// each naming its reason, the run's turn and launch, the seat and the
// repository where known — and never the token.
//
// # A box's pushes are audited by nothing but their rows
//
// operator_acted is a PERSON'S record (pushAudit). A box's push is recorded by
// its push row, which names its run (code.RunProvenance.RunID), and a box's
// refusal by its log line. Its fetch of a `visibility: unit` repository is a
// read like any other, recorded under its seat (fetched).

// The box's three challenges, each a sentence of the box's own credential:
// what to present, never an API token.
const (
	boxNoneSentence = "authentication required: a coding box authenticates as `" + BoxUser +
		"` with its run token, which the engine's credential helper answers from $CREWLET_GIT_TOKEN"
	boxUserSentence  = "a coding box authenticates as `" + BoxUser + "` with its run token"
	boxTokenSentence = "this run token is not valid here: forged, expired, or signed under a key this " +
		"node's secrets.keys does not hold"
)

// admitBox is admit for a coding box: the steps above, in order.
func (s *Server) admitBox(ctx context.Context, w http.ResponseWriter, r *http.Request, op auth.GitOp) (admission, bool) {
	raw := r.PathValue(PathRepo)
	name := strings.TrimSuffix(raw, ".git")
	if !gitref.ValidRepoName(name) {
		auth.GitText(w, http.StatusNotFound, notFoundSentence(raw))
		return admission{}, false
	}

	// 2 THE CREDENTIAL.
	var ref BoxRunRef
	if ChallengeBox(w, r, func(token string) (signed bool) {
		ref, signed = s.o.Boxes.Tokens.Verify(token)
		return signed
	}, s.log) {
		return admission{}, false
	}
	b := boxRefusal{s: s, w: w, r: r, ref: ref, name: name}

	// 3 THE RUN.
	run, found, err := s.o.Boxes.Runs.Run(ctx, ref.TurnID)
	switch {
	case err != nil:
		b.unavailable("run_unread", "the run store could not be read here: "+sentence(err.Error()))
		return admission{}, false
	case !found:
		b.refuse("run_unknown", fmt.Sprintf("coding run %s is unknown here: it ended and its record was "+
			"removed, or it never launched", ref.TurnID))
		return admission{}, false
	case run.Ref.LaunchID != ref.LaunchID:
		b.refuse("run_superseded", fmt.Sprintf("this token is a finished job's: run %s has launched again "+
			"since, and only its current job's token admits a box", ref.TurnID))
		return admission{}, false
	case !run.Live:
		b.refuse("run_ended", fmt.Sprintf("coding run %s has ended (%s): its box keeps no git access",
			ref.TurnID, run.Status))
		return admission{}, false
	}
	b.seat = run.Seat

	// 4 THE RUN'S GRANT, by the name it was given — before the code log.
	g, given := run.grantNamed(name)
	switch {
	case !given:
		b.refuse("not_granted", fmt.Sprintf("this run was not given %s: its run_sandbox call named %s",
			strconv.Quote(name), grantedNames(run)))
		return admission{}, false
	case op == auth.GitPush && g.Access < code.AccessWrite:
		b.refuse("read_only", fmt.Sprintf("this run was given %s to read: a push needs access: write in "+
			"its run_sandbox call", strconv.Quote(name)))
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
	adm := admission{name: name, box: &run, spool: boxSpool, fresh: runSession(run.Floor)}

	// 6 THE REPOSITORY, given.
	repo, err := s.o.Estate.Resolve(ctx, code.ResolveQuery{Name: name, Fresh: adm.fresh})
	if errors.Is(err, code.ErrRepoNotFound) ||
		(err == nil && (refuses(repo.Status, op) || repo.ID != g.RepoID)) {
		adm.linearizable = true
		repo, err = s.o.Estate.Resolve(ctx, code.ResolveQuery{Name: name,
			Fresh: statelog.Freshness{Level: statelog.ReadLinearizable}})
	}
	switch {
	case errors.Is(err, code.ErrRepoNotFound):
		auth.GitText(w, http.StatusNotFound, notFoundSentence(name))
		return admission{}, false
	case err != nil:
		s.unavailable(w, r, name, err)
		return admission{}, false
	case repo.ID != g.RepoID:
		b.refuse("not_granted", fmt.Sprintf("%s is no longer the repository this run was given (renamed "+
			"since the launch); call run_sandbox again", strconv.Quote(name)))
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
	adm.grant = code.GrantOf(adm.principal(), repo, roster).Within(g.Access)
	if op == auth.GitFetch && !adm.grant.MayRead() {
		b.refuse("not_permitted", adm.grant.ReadRefusal())
		return admission{}, false
	}
	return adm, true
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
// credential at all, a user name other than [BoxUser], or a password signed
// refuses — with git's 401 challenge in the box's own words, logs it on log
// at DEBUG (code_box_challenged) with why (`none`, `user` or `token`) and
// never the value presented, and reports true; a request presenting a run
// token signed accepts is left unanswered, and false.
//
// EXPORTED FOR THE ONE OTHER PLACE a box's credential is decided before an
// admission can run: the API's box route on a node that serves no code host
// now, which tells a box WHY only once it presents a token this fleet signed
// (internal/api; M3-decisions U8) — /box/git/ is public, and that reason is
// the deployment's state. ONE STEP FOR BOTH, so a box is challenged in the
// same words, at the same level, by a node serving the code host and by one
// that serves none, and nothing reads a box's credential by a grammar of its
// own (TestNothingAfterAdmissionAsksWhichAdmitterRan holds every other reader
// of git's Basic out of this package). signed is the caller's verify: the
// admission's keeps the run its token names.
func ChallengeBox(w http.ResponseWriter, r *http.Request, signed func(token string) bool, log *slog.Logger) bool {
	var why, words string
	switch user, password, presented := r.BasicAuth(); {
	case !presented:
		why, words = "none", boxNoneSentence
	case user != BoxUser:
		why, words = "user", boxUserSentence
	case !signed(password):
		why, words = "token", boxTokenSentence
	default:
		return false
	}
	log.Debug("code_box_challenged", "path", r.URL.Path, "reason", why)
	auth.GitChallengeSaying(w, words)
	return true
}

// boxRefusal answers one admitted token's refusal: what its run, its seat and
// its repository are, for the line it logs.
type boxRefusal struct {
	s    *Server
	w    http.ResponseWriter
	r    *http.Request
	ref  BoxRunRef
	seat string
	name string
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

// attrs is a refusal's log line: its reason, the run's turn and launch, the
// seat and the repository.
func (b boxRefusal) attrs(reason string) []any {
	return []any{"reason", reason, "turn_id", b.ref.TurnID, "launch_id", b.ref.LaunchID, "seat", b.seat,
		"repo", b.name, "path", b.r.URL.Path}
}

// raiseFloor raises the run a box's request was admitted for to pos — the
// copy a ref answer it is about to be served is read from, or an applied
// push's row — on the run's row (Runs.RaiseFloor), and answers a failure as a
// *floorUnraised. Nothing for a person's request, whose reads of refs are
// linearizable, and nothing where pos is at or below the floor the admission
// read: one compare-and-set on the row, taken only when the box is shown
// something newer than its floor already holds.
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
