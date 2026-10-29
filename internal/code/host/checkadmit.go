package host

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A CHECK'S BOX'S ADMISSION: a box running one attempt of one check of one
// commit reaches the code host on two routes — its git under /box/git/,
// admitted by admitCheck once admitBox has read its check's token, to fetch
// its commit; and the API's /box/code/, whose reads of that commit by object
// id the API answers once AdmitCheck has admitted it — by the same steps, its
// credential its check's token and its authority the attempt's ROW.
//
// # In order, each refusal its own answer
//
//  1. NAME and 2. CREDENTIAL — admitBox's: git's Basic, user name
//     [CheckUser], the check's token ([CheckTokens]) as the password.
//  3. A FETCH, OVER PROTOCOL v2 — on its git: a push is refused (a check
//     measures its commit and changes nothing), and so is a request in
//     protocol v0, whose fetch serves only what an advertisement named,
//     where a check's box is shown no ref (below) — the box's git, 2.40 or
//     later, speaks v2 by default. On /box/code/, a coding run's token is
//     refused: the API answers a check's box alone, a coding box reading its
//     repositories through its clone and its tools.
//  4. THE NAME GIVEN — before any read of the code log: the URL's name must
//     be the one the token names, the name the box was given. A box is
//     untrusted traffic running a repository's own checks file, and a
//     refusal decided against the code log costs a barrier on it (step 6):
//     matched first, a box probing names costs the log nothing, as a coding
//     box's probe costs nothing past the names its run was given.
//  5. THE ATTEMPT — its row ([code.ReadCheckBox]), THE AUTHORITY: read STALE,
//     and again LINEARIZABLE where the stale copy holds no such attempt — a
//     copy behind the log may not have applied its queue yet — so no attempt
//     is called unknown on a stale read. A row that has ENDED is refused at
//     once, read however stale: a check's state only rises (the join of its
//     records), so an ending a copy holds is the attempt's for good, and a
//     box still running past its attempt's end — the runner lost, the box
//     orphaned — appends no barrier however often it asks. A queued or
//     running row admits; the token is only its backstop
//     (code.CheckBoxGrace). Unreadable is 503 and never 401.
//  6. THE REPOSITORY — resolved at the check's session (step 7), and again
//     LINEARIZABLE where that answer would refuse — not found, gone, or
//     ANOTHER repository under the name — as a coding box's is: a name a
//     rename left redirecting is answered for the repository it redirects to,
//     under its current name; one now naming another repository than the
//     attempt's is refused as not given.
//  7. FRESHNESS — every read of the request at the check's SESSION: the
//     position its attempt was queued at, which the commit's own push
//     precedes, so the copy serving the box holds its commit. LINEARIZABLE
//     for a row naming none.
//
// # What the box is shown, and charged as
//
// NO REF: its advertisement is protocol v2's, which names none, and an
// `ls-refs` is answered empty (fetch.go), so a check's box learns no branch
// and reads at no ref's level — it fetches its one commit BY ID, which
// protocol v2 serves of any object the repository holds, and a clone's
// recipe asks (sandbox's single-sha CloneRecipe). Its transfers are a RUN's
// (gitproc.ShareRun) — the class every box shares, at most half the lane —
// keyed by its REPOSITORY, `check:<id>`: one push fans out a check per name
// its file defines, and keyed per attempt a repository's dozen checks took a
// dozen turns of the class against one coding run's one. Its spool is every
// box's ONE credential (boxSpool). It acts as NOBODY (admission.principal):
// the engine runs its repository's own checks file, so its fetch of a
// `visibility: unit` repository is no read of a principal's — its attempt's
// row, naming the change and who queued it, is its record — and its API's
// reads are the engine's own (code.GitQuery.Check).
//
// Every refusal from step 3 on takes a token the fleet signed: logged
// code_box_refused at INFO, a 503 code_box_unavailable at WARN, each naming
// the attempt and the repository — never the token.

// AdmitCheck admits a CHECK'S BOX'S request of the API's /box/code/ — a read of
// its commit by object id, which the API asks of the code domain's git reads
// once admitted — by the steps above: the repository's name from the route's
// {repo}, its check's token as git's Basic password, the attempt's row and the
// repository. A request it refuses it answers itself, in the one line of
// text the box's git is answered in — one admission, one set of words — and
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
	if user != CheckUser {
		boxRefusal{s: s, w: w, r: r, ref: run, name: name}.refuse("not_a_check", "this route answers a "+
			"check's box, which authenticates as `"+CheckUser+"` with its check's token; a coding box reads "+
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
	Attempt code.CheckRow
	Repo    code.RepoRef
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
	}
	return s.checkAdmitted(ctx, b, name, ref)
}

// checkAdmitted is steps 4 to 7, the two routes' own: the admission of a
// check's box whose token named ref, to the repository its URL names name.
func (s *Server) checkAdmitted(ctx context.Context, b boxRefusal, name string, ref CheckRunRef) (admission, bool) {
	// 4 THE NAME GIVEN, before the code log.
	if name != ref.Repo {
		b.refuse("not_granted", fmt.Sprintf("this check's box was given %s: its token admits it to no other "+
			"repository", strconv.Quote(ref.Repo)))
		return admission{}, false
	}

	// 5 THE ATTEMPT.
	key := ref.Attempt
	row, found, err := s.checkRow(ctx, key)
	switch {
	case err != nil:
		s.unavailable(b.w, b.r, name, err)
		return admission{}, false
	case !found:
		b.refuse("check_unknown", fmt.Sprintf("attempt %d of check %s of %s is unknown here: its "+
			"repository's checks were swept or purged with it, or it was never queued", key.Attempt, key.Name,
			key.OID))
		return admission{}, false
	case row.State.Ended():
		b.refuse("check_ended", fmt.Sprintf("attempt %d of check %s of %s has ended (%s): its box keeps no "+
			"git access", key.Attempt, key.Name, key.OID, row.State))
		return admission{}, false
	}

	// 7 THE FRESHNESS, which step 6 reads at.
	adm := admission{name: name, check: &row, spool: boxSpool, fresh: runSession(row.Queued), noRefs: true}

	// 6 THE REPOSITORY, the attempt's.
	repo, err := s.o.Estate.Resolve(ctx, code.ResolveQuery{Name: name, Fresh: adm.fresh})
	if errors.Is(err, code.ErrRepoNotFound) ||
		(err == nil && (refuses(repo.Status, auth.GitFetch) || repo.ID != key.RepoID)) {
		adm.linearizable = true
		repo, err = s.o.Estate.Resolve(ctx, code.ResolveQuery{Name: name,
			Fresh: statelog.Freshness{Level: statelog.ReadLinearizable}})
	}
	switch {
	case errors.Is(err, code.ErrRepoNotFound):
		auth.GitText(b.w, http.StatusNotFound, notFoundSentence(name))
		return admission{}, false
	case err != nil:
		s.unavailable(b.w, b.r, name, err)
		return admission{}, false
	case repo.ID != key.RepoID:
		b.refuse("not_granted", fmt.Sprintf("%s is no longer the repository this check was queued for "+
			"(renamed or removed since); its next attempt is given the repository by its current name",
			strconv.Quote(name)))
		return admission{}, false
	}
	// ANSWERED UNDER ITS CURRENT NAME, as a coding box's is.
	adm.name, adm.repo = repo.Name, repo
	return adm, true
}

// checkRow is the attempt key names as its row stands (step 5), read STALE
// and, where the stale copy holds no such attempt, LINEARIZABLE: found false
// only once the barrier's copy holds none either. An ending read stale is not
// read again — the caller refuses it, and an ending is for good.
func (s *Server) checkRow(ctx context.Context, key code.CheckKey) (code.CheckRow, bool, error) {
	for _, level := range []statelog.ReadLevel{statelog.ReadStale, statelog.ReadLinearizable} {
		a, err := s.o.Estate.Read(ctx, code.ReadQuery{Kind: code.ReadCheckBox, CheckBox: &key,
			Fresh: statelog.Freshness{Level: level}})
		if err != nil {
			return code.CheckRow{}, false, err
		}
		if len(a.Checks) == 1 {
			return a.Checks[0], true, nil
		}
	}
	return code.CheckRow{}, false, nil
}
