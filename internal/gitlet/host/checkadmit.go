package host

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/costlimit"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A CHECK'S BOX'S ADMISSION, on its git under /box/git/ (admitCheck, its token
// read by admitBox) and the API's /box/gitlet/ (AdmitCheck), by the same steps,
// each refusal its own answer (package doc, "A check's box is admitted by its
// attempt's row"):
//  1. name and 2. credential: git's Basic, [gitlet.CheckUser], [CheckTokens];
//  3. on git, a fetch over protocol v2 holding a place in flight; on the API,
//     no executor's token;
//  4. the URL's name is the token's, matched before any code-log read;
//  5. the attempt's row, the authority (checkRow); 6. the repository; and
//  7. freshness, every read at the check's session (checkAdmitted).
// From step 3 on, refusals log code_box_refused (INFO) and 503s
// code_box_unavailable (WARN), naming attempt and repository, never the token.

// AdmitCheck admits a CHECK'S BOX'S request of the API's /box/gitlet/, a read
// of its commit by object id, by the steps above: the name from {repo}, the
// check's token as git's Basic password, the attempt's row and the repository.
// It holds no place in flight: the read runs on the repository's home Gitlet
// node, whose look at the log's head draws on the same attempt's re-syncs. A
// request it refuses it answers itself, in the words the box's git gets, and
// reports false.
func (s *Server) AdmitCheck(ctx context.Context, w http.ResponseWriter, r *http.Request) (CheckAdmitted, bool) {
	ctx, end, ok := s.begin(ctx, w)
	if !ok {
		return CheckAdmitted{}, false
	}
	defer end()
	name := r.PathValue(PathRepo)
	if !gitref.ValidRepoName(name) {
		auth.GitText(w, http.StatusNotFound, notFoundSentence(name))
		return CheckAdmitted{}, false
	}
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
		return CheckAdmitted{}, false
	}
	if user != gitlet.CheckUser {
		boxRefusal{s: s, w: w, r: r, ref: run, name: name}.refuse("not_a_check", "this route answers a "+
			"check's box, which authenticates as `"+gitlet.CheckUser+"` with its check's token; a coding box reads "+
			"its repositories through its clone and its tools")
		return CheckAdmitted{}, false
	}
	adm, ok := s.checkAdmitted(ctx, boxRefusal{s: s, w: w, r: r, check: &check, name: name}, name, check)
	if !ok {
		return CheckAdmitted{}, false
	}
	if gone(adm.repo.Status) {
		auth.GitText(w, http.StatusNotFound, s.goneSentence(adm.repo))
		return CheckAdmitted{}, false
	}
	return CheckAdmitted{Attempt: *adm.check, Repo: adm.repo, Fresh: adm.fresh}, true
}

// CheckAdmitted is a check's box's request of the API admitted (AdmitCheck):
// the attempt as its row stood, the repository under its current name, and
// the check's session, which every read the request makes is answered at.
type CheckAdmitted struct {
	Attempt gitlet.CheckRow
	Repo    gitlet.RepoRef
	Fresh   statelog.Freshness
}

// admitCheck is admit for a check's box on its git, its token verified by
// admitBox: a fetch over protocol v2 (step 3), then [Server.checkAdmitted].
func (s *Server) admitCheck(ctx context.Context, w http.ResponseWriter, r *http.Request, op auth.GitOp,
	name string, ref CheckRunRef) (admission, bool) {

	b := boxRefusal{s: s, w: w, r: r, check: &ref, name: name}
	switch {
	case op == auth.GitPush:
		b.refuse("read_only", "a check's box fetches the commit it checks and pushes nothing")
		return admission{}, false
	case protocolVersion(r.Header.Get("Git-Protocol")) != 2:
		b.refuse("protocol_v0", "a check's box fetches over git's protocol v2, which its git speaks by "+
			"default from 2.26 on: it is shown no ref, and protocol v0 fetches only what a ref names")
		return admission{}, false
	case !s.holdRun(ctx, checkHold(ref.Attempt)):
		// ITS PLACE IN FLIGHT, as an executor's (boxlimit.go): a check's
		// file runs code nobody vouches for, and its partial clone's lazy
		// fetches are its git's own burst.
		b.unavailable("run_busy", busySentence("this check"))
		return admission{}, false
	}
	return s.checkAdmitted(ctx, b, name, ref)
}

// checkAdmitted is steps 4 to 7, the two routes' own: the admission of a
// check's box whose token named ref, to the repository its URL names name.
func (s *Server) checkAdmitted(ctx context.Context, b boxRefusal, name string, ref CheckRunRef) (admission, bool) {
	// 4 THE NAME GIVEN, before the code log: a box runs untrusted code, and a
	// refusal decided against the log can cost a barrier, so probing names
	// costs the log nothing.
	if name != ref.Repo {
		b.refuse("not_granted", fmt.Sprintf("this check's box was given %s: its token admits it to no other "+
			"repository", strconv.Quote(ref.Repo)))
		return admission{}, false
	}

	// 5 THE ATTEMPT, the authority, its token only a backstop
	// (gitlet.CheckBoxGrace). An ended row is refused however stale, since a
	// check's state only rises, so an orphaned box appends no barrier.
	// Unreadable is 503, never 401.
	key := ref.Attempt
	row, found, spent, err := s.checkRow(ctx, key)
	switch {
	case err != nil:
		s.unavailable(b.w, b.r, name, err)
		return admission{}, false
	case !found:
		b.refuse("check_unknown", fmt.Sprintf("attempt %d of check %s of %s is unknown here: its "+
			"repository's checks were swept or purged with it, or it was never queued%s", key.Attempt, key.Name,
			key.OID, s.checkNotRejudged(spent)))
		return admission{}, false
	case row.State.Ended():
		b.refuse("check_ended", fmt.Sprintf("attempt %d of check %s of %s has ended (%s): its box keeps no "+
			"git access", key.Attempt, key.Name, key.OID, row.State))
		return admission{}, false
	}

	// 7 THE FRESHNESS, which step 6 reads at.
	// A ROW THAT NAMES NO QUEUED POSITION reads at the log's head, a re-sync
	// the check's bucket pays for as a floorless run's does (boxlimit.go).
	adm := admission{name: name, check: &row, spool: boxSpool, fresh: runSession(row.Queued), noRefs: true}
	if adm.fresh.Level == statelog.ReadLinearizable {
		if spent = s.boxResync(adm); spent != nil {
			adm.fresh = statelog.Freshness{Level: statelog.ReadStale}
		}
	}
	adm.linearizable = adm.fresh.Level == statelog.ReadLinearizable

	// 6 THE REPOSITORY, the attempt's — judged again at the log's head where
	// the session's answer would refuse, while the check's re-syncs last.
	repo, err := s.o.Estate.Resolve(ctx, gitlet.ResolveQuery{Name: name, Fresh: adm.fresh})
	if !adm.linearizable && spent == nil && (errors.Is(err, gitlet.ErrRepoNotFound) ||
		(err == nil && (refuses(repo, auth.GitFetch) || repo.ID != key.RepoID))) {
		if spent = s.boxResync(adm); spent == nil {
			adm.linearizable = true
			repo, err = s.o.Estate.Resolve(ctx, gitlet.ResolveQuery{Name: name,
				Fresh: statelog.Freshness{Level: statelog.ReadLinearizable}})
		}
	}
	switch {
	case errors.Is(err, gitlet.ErrRepoNotFound):
		auth.GitText(b.w, http.StatusNotFound, notFoundSentence(name)+s.checkNotRejudged(spent))
		return admission{}, false
	case err != nil:
		s.unavailable(b.w, b.r, name, err)
		return admission{}, false
	case repo.ID != key.RepoID:
		b.refuse("not_granted", fmt.Sprintf("%s is no longer the repository this check was queued for "+
			"(renamed or removed since); its next attempt is given the repository by its current name%s",
			strconv.Quote(name), s.checkNotRejudged(spent)))
		return admission{}, false
	}
	// ANSWERED UNDER ITS CURRENT NAME, as a coding box's is.
	adm.name, adm.repo = repo.Name, repo
	return adm, true
}

// checkRow is the attempt key names as its row stands (step 5), read STALE
// and, where that copy holds no such attempt (it may not have applied the
// queue yet), LINEARIZABLE while the check's re-syncs last (boxlimit.go), so a
// swept attempt's box asking in a loop pays bounded barriers. found is false
// once the barrier's copy holds none, or, re-syncs spent, the stale copy's
// none with the refusal that spent them. An ending read stale is not read
// again: an ending is for good.
func (s *Server) checkRow(ctx context.Context, key gitlet.CheckKey) (gitlet.CheckRow, bool, *costlimit.Refusal,
	error) {

	for _, level := range []statelog.ReadLevel{statelog.ReadStale, statelog.ReadLinearizable} {
		if level == statelog.ReadLinearizable {
			if spent := s.o.Resyncs.Box(key.Resyncs()); spent != nil {
				return gitlet.CheckRow{}, false, spent, nil
			}
		}
		a, err := s.o.Estate.Read(ctx, gitlet.ReadQuery{Kind: gitlet.ReadCheckBox, CheckBox: &key,
			Fresh: statelog.Freshness{Level: level}})
		if err != nil {
			return gitlet.CheckRow{}, false, nil, err
		}
		if len(a.Checks) == 1 {
			return a.Checks[0], true, nil, nil
		}
	}
	return gitlet.CheckRow{}, false, nil, nil
}

// checkNotRejudged is the clause a check's box's refusal answered on a copy
// not read at the log's head carries — its re-syncs spent — or "" where it
// was read there.
func (s *Server) checkNotRejudged(spent *costlimit.Refusal) string {
	if spent == nil {
		return ""
	}
	return " " + gitlet.NotRejudged(spent, checkResyncs, checksResyncs, s.now())
}
