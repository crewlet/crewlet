package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/gitlet/host"
	"github.com/crewlet/crewlet/internal/queue/topics"
	seatplacement "github.com/crewlet/crewlet/internal/seat/placement"
	"github.com/crewlet/crewlet/internal/statelog"
)

// GITLET ON THE API: smart HTTP under /git/ for a person's git client, two
// downloads beside it (a file's bytes, a commit's ZIP), the cache's verify under
// /gitlet/, and what every node answers when it serves none of Gitlet now.
//
// Both prefixes are GUARDED and never on api.public ([auth.Unguarded],
// [auth.Public]): a repository is the company's source, reached with a person's
// own bound token (ADR-0085), which on a git route alone may arrive as a Basic
// password, since a git client presents one no other way. /box/git/ and
// /box/gitlet/ ([auth.BoxGitPrefix], [auth.BoxGitletPrefix], boxcode.go) are a
// box's routes, admitted by the box's own token, never an operator's: mounted on
// the route table like /mcp/ and /otlp/, not through [authz.Router], whose
// refusal would come before the box's admission ran.

// GITLET'S ROWS ARE QUESTIONS, not routes of this file: `code_repos`,
// `code_repo`, `code_pushes` and `code_repo_readers` live in
// internal/api/queries and are served on their /gitlet/ paths through rest.go's
// named routes, so every reader is answered by one implementation. This file
// hands them the code domain's read, per question ([codeQuestions]).

// Whether the GIT routes exist is known at New: the node's Tier A roles name
// `gitlet`, else a git request is the mux's own `no_route`. The READS are
// questions on every API: a data node answers from its own copy, any other
// through the estate router.
//
// Whether a route or question is SERVED is asked of the engine on every request
// ([CodeSurface]), as native.go does: Gitlet is built at boot or at the first
// apply whose company sets code.backend: gitlet, possibly after the app was
// assembled. A miss answers WHY ([engine.Engine.CodeUnserved]): a company
// hosting no code is a 404, a node serving none now or not yet knowing a 503
// naming which of four ([codeUnserved]). A box is told only once it presents a
// run token this fleet signed ([App.boxGitRoute]).

// GitRoutes is Gitlet as the API mounts it; *host.Server satisfies it. Raw
// and Archive are a person's two downloads beside git (host/download.go).
// The Box handlers are the same three entry points behind a box's admission
// rather than a person's; AdmitCheck is that admission for a check's box's
// reads under /box/gitlet/ (boxcode.go), which answers a request it refuses
// itself.
type GitRoutes interface {
	InfoRefs() http.Handler
	UploadPack() http.Handler
	ReceivePack() http.Handler
	Raw() http.Handler
	Archive() http.Handler
	BoxInfoRefs() http.Handler
	BoxUploadPack() http.Handler
	BoxReceivePack() http.Handler
	AdmitCheck(ctx context.Context, w http.ResponseWriter, r *http.Request) (host.CheckAdmitted, bool)
	StartVerify(ctx context.Context, name string, repair bool, floor statelog.Position) (host.Verification, error)
	Verification(name string, id uuid.UUID) (host.Verification, bool)
	AwaitVerification(ctx context.Context, name string, id uuid.UUID) (host.Verification, bool)
}

// CodeSurface is Gitlet and its reads ASKED ON EVERY REQUEST, each
// answering, where this node has none NOW, why: one of
// [engine.ErrCodeNotHosted], [engine.ErrCodeApplying],
// [engine.ErrCodeApplyFailed], [engine.ErrCodeUnknown] and
// [engine.ErrCodeNeedsRestart], which [codeUnserved] answers each its own
// way.
//
// AN INTERFACE ANSWERING (value, error) — never a bool — because "absent"
// is five facts with five answers, and a typed nil inside an interface would
// pass a nil check and panic on the first request.
type CodeSurface interface {
	// GitRoutes is Gitlet as this node serves it, or why it serves none.
	GitRoutes() (GitRoutes, error)

	// CodeReads is the code domain's read (estate.CodeSeam), or why this
	// node serves none — which the code questions are answered from, the
	// reason put in their vocabulary by [codeQuestions].
	CodeReads() (queries.CodeReads, error)

	// RepoReaders is who read a `visibility: unit` repository — the usage
	// domain's readers, wherever its rows are served, and the company's
	// clock a range that names no end is cut on — or why this node serves
	// none, on CodeReads' terms: the readers stand with the code reads.
	RepoReaders() (builtin.ReadersDeps, error)

	// CheckLogs is the object store a failed check's log is read from —
	// the fleet's one, which Gitlet keeps its packs in — or why this
	// node serves none, on CodeReads' terms.
	CheckLogs() (queries.CheckLogs, error)

	// CodeSearcher is the code search over the fleet's Gitlet nodes
	// (engine.Engine.CodeSearch), or why this node serves none, on
	// CodeReads' terms.
	CodeSearcher() (queries.CodeSearcher, error)

	// GitReads is the code domain's reads of a repository by object id,
	// routed to its home Gitlet node (estate.CodeSeam's GitRead) — what a
	// check's box's reads of its commit are answered from (boxcode.go) — or
	// why this node serves none, on CodeReads' terms.
	GitReads() (GitReader, error)

	// VerifyBox is whether token is the credential user presents on a box
	// route, signed by this fleet — an executor's token under host.BoxUser,
	// a check's under gitlet.CheckUser (host.VerifyBox) — by the engine's own
	// tokens over the keyring Tier A derives on EVERY node whatever the
	// company runs: so it answers where GitRoutes finds Gitlet unserved, which
	// is the one place it is asked — a box route there tells a box why only
	// once its token verifies ([App.boxGitRoute]).
	VerifyBox(user, token string) bool
}

// GitReader is the code domain's reads of a repository by object id, as a
// check's box's reads of its commit ask them; estate.CodeSeam satisfies it.
type GitReader interface {
	GitRead(ctx context.Context, q gitlet.GitQuery) (gitlet.GitAnswer, error)
}

// codeQuestions is the code surface as the code questions find it on every
// question ([queries.CodeSource]): its read and its readers, or why this node
// serves none in the read surface's vocabulary — the reason [codeUnserved]
// decides for the git routes too, so a clone and a listing asked of one node
// say the same thing about why it serves neither.
type codeQuestions struct{ surface CodeSurface }

// CodeReads is the surface's read, or why there is none ([unserved.question]).
func (c codeQuestions) CodeReads() (queries.CodeReads, error) {
	reads, why := c.surface.CodeReads()
	if why != nil {
		return nil, codeUnserved(why).question(why)
	}
	return reads, nil
}

// RepoReaders is the surface's readers of a unit's repository, or why there
// are none, on CodeReads' terms ([unserved.question]).
func (c codeQuestions) RepoReaders() (builtin.ReadersDeps, error) {
	readers, why := c.surface.RepoReaders()
	if why != nil {
		return builtin.ReadersDeps{}, codeUnserved(why).question(why)
	}
	return readers, nil
}

// CheckLogs is the surface's object store for a check's log, or why there
// is none, on CodeReads' terms ([unserved.question]).
func (c codeQuestions) CheckLogs() (queries.CheckLogs, error) {
	logs, why := c.surface.CheckLogs()
	if why != nil {
		return nil, codeUnserved(why).question(why)
	}
	return logs, nil
}

// CodeSearcher is the surface's code search, or why there is none, on
// CodeReads' terms ([unserved.question]).
func (c codeQuestions) CodeSearcher() (queries.CodeSearcher, error) {
	searcher, why := c.surface.CodeSearcher()
	if why != nil {
		return nil, codeUnserved(why).question(why)
	}
	return searcher, nil
}

// notHosted is what a code route answers where the company hosts no code: ONE
// sentence, in git's text/plain form on a git route and as a `not_found`
// detail on a /gitlet/ read.
const notHosted = "this company does not host code (code.backend is not gitlet)"

// codeApplyingRetryAfter is what a node applying the configuration the fleet
// activated tells a code request to wait: stream.HealthInterval (5 s). An
// apply that builds Gitlet is a handful of opens and a cache's boot —
// seconds — and the health tick is the soonest a client watching the node
// could see it ended. The one absence waiting is sure to clear: a failed apply
// clears at a retry of the node's own only where its cause has gone away by
// itself, and otherwise when somebody acts, and a restart owed only when
// somebody acts — so neither carries a Retry-After.
const codeApplyingRetryAfter = stream.HealthInterval

// reasonNotHeld is the reason a verify's 404 names beside the code questions'
// two ([queries.ReasonNotDeclared], [queries.ReasonNotHosted]), every one
// `not_found` and told apart by the reason, because a client acts on each a
// different way: a verify's repository is declared and THIS node holds no copy
// of it, so a sweep of every repository on one Gitlet node passes over the ones
// that node does not keep (nothing there can be damaged) and fails on a name
// nobody declared. A git route says each in git's one text/plain line, where
// nothing reads a reason: the sentence is all git prints.
const reasonNotHeld = "not_held"

// unserved is why a node serves none of Gitlet now, as a code route
// answers it ([codeUnserved]).
type unserved struct {
	// code is the answer's code: [httpjson.CodeNotFound] for a company that
	// hosts no code, [httpjson.CodeQueryFailed] for a reason the seam does
	// not name, and otherwise one of Gitlet's four 503s.
	code httpjson.Code

	// retryAfter is the wait a 503 carries, zero where waiting clears
	// nothing; detail the sentence that says which it is. The sentence
	// names no code, because each answer names it once: the envelope's
	// `error` on a /gitlet/ route, and the brackets git's one line ends with.
	retryAfter time.Duration
	detail     string
}

// codeUnserved is how a code route answers a node serving none of Gitlet now,
// by the engine's reason:
//   - not hosted: 404 `not_found`, [notHosted];
//   - applying: 503 `applying`, Retry-After [codeApplyingRetryAfter]; says
//     nothing of what the configuration holds (not yet decoded);
//   - apply failed: 503 `config_apply_failed`, no Retry-After; another node serves;
//   - unknown: 503 `code_unknown`, no Retry-After: the apply failed before the
//     revision was decoded, so it cannot say the company hosts code;
//   - needs restart: 503 `native_backend_needs_restart`, no Retry-After;
//     only a restart clears it.
//
// Any other error breaks the seam's contract: 500.
func codeUnserved(err error) unserved {
	switch {
	case errors.Is(err, engine.ErrCodeNotHosted):
		return unserved{code: httpjson.CodeNotFound, detail: notHosted}
	case errors.Is(err, engine.ErrCodeApplying):
		return unserved{code: httpjson.CodeApplying, retryAfter: codeApplyingRetryAfter,
			detail: "this node is applying the configuration the fleet activated and answers once that " +
				"apply ends: retry in " + strconv.Itoa(httpjson.RetrySeconds(codeApplyingRetryAfter)) +
				" seconds, or against another node"}
	case errors.Is(err, engine.ErrCodeApplyFailed):
		return unserved{code: httpjson.CodeConfigApplyFailed,
			detail: "this node could not apply the configuration the fleet activated, which hosts code " +
				"(GET /fleet and this node's log say why): ask another node, or activate a corrected " +
				"configuration"}
	case errors.Is(err, engine.ErrCodeUnknown):
		return unserved{code: httpjson.CodeCodeUnknown,
			detail: "this node could not read the configuration the fleet activated, so it cannot say " +
				"whether this company hosts code (GET /fleet and this node's log say why): ask another node"}
	case errors.Is(err, engine.ErrCodeNeedsRestart):
		return unserved{code: httpjson.CodeNativeBackendNeedsRestart,
			detail: httpjson.NeedsRestart("Gitlet")}
	}
	return unserved{code: httpjson.CodeQueryFailed}
}

// question is u in the read surface's vocabulary, which the code questions
// answer and every /gitlet/ route renders ([writeQueryError]): a company hosting
// no code is [queries.NotFound] (`not_hosted`); a node that will serve Gitlet
// once something clears is [queries.ErrUnavailable] under the 503's own
// code ([stream.Unserved]) — so the socket names the code as its frame's
// `refusal`, with the same words and the same wait; and a reason the seam does
// not name is why itself, a failure the surface answers 500 and logs.
func (u unserved) question(why error) error {
	switch u.code {
	case httpjson.CodeNotFound:
		return &queries.NotFound{Reason: queries.ReasonNotHosted, Detail: u.detail}
	case httpjson.CodeQueryFailed:
		return fmt.Errorf("api: the code surface answered a reason this build does not name: %w", why)
	}
	return fmt.Errorf("%w: %w", queries.ErrUnavailable,
		&stream.Unserved{Code: u.code, Detail: u.detail, RetryAfter: u.retryAfter})
}

// servesGit reports whether a node of this Tier A serves git: its roles name
// git, or name none (an undeclared node has every role) — the node's answer,
// never the company's. No Tier A is no git: such an app is a test's, and its
// guard refuses every write. (The placement package is imported under another
// name here because this package declares a function called placement, in
// roster.go.)
func servesGit(b *config.Bootstrap) bool {
	return b != nil && b.Profile("").Roles.Has(seatplacement.RoleGitlet)
}

// mountCode registers Gitlet's routes on a node whose roles name gitlet, each
// through [authz.Router] on the verb it is decided by. Its ROWS are questions.
//   - THE GIT ROUTES and the two downloads admit every holder of the gitlet app
//     ([authz.ActionCodeGit]) before any repository is read (ADR-0110); the
//     per-repository grant is derived later (gitlet.GrantOf, ADR-0088).
//   - A VERIFY AND ITS READ are admin's ([deploymentPolicy]): an operation on
//     this node's copy, which no repository's grant decides.
//
// A git refusal is git's one line of text ([gitRefusal]). A BOX'S ROUTES
// ([App.boxGitRoute], [App.boxCodeRoute]) go on the route table ITSELF, never a
// router: it decides on the iam principal ([authz.ContextGuard]) and a box's
// request arrives anonymous, so it would be refused before the box's admission.
func (a *App) mountCode(mux authz.Mux) error {
	if !a.servesGit {
		return nil
	}
	guard := authz.ContextGuard(authz.NoChart{})
	var failures []error
	mount := func(router *authz.Router, pattern string, p authz.Policy, h http.Handler) {
		if err := router.Handle(pattern, p, h); err != nil {
			failures = append(failures, err)
		}
	}
	git := authz.NewRouter(mux, guard).Refusing(gitRefusal)
	transport := authz.Policy{Action: authz.ActionCodeGit}
	mount(git, host.PatternInfoRefs, transport, a.gitRoute(GitRoutes.InfoRefs))
	mount(git, host.PatternUploadPack, transport, a.gitRoute(GitRoutes.UploadPack))
	mount(git, host.PatternReceivePack, transport, a.gitRoute(GitRoutes.ReceivePack))
	mount(git, host.PatternRaw, transport, a.gitRoute(GitRoutes.Raw))
	mount(git, host.PatternArchive, transport, a.gitRoute(GitRoutes.Archive))
	mux.Handle(host.PatternBoxInfoRefs, a.boxGitRoute(GitRoutes.BoxInfoRefs))
	mux.Handle(host.PatternBoxUploadPack, a.boxGitRoute(GitRoutes.BoxUploadPack))
	mux.Handle(host.PatternBoxReceivePack, a.boxGitRoute(GitRoutes.BoxReceivePack))
	mux.Handle(host.PatternBoxGitletTree, a.boxCodeRoute(gitlet.GitTree))
	mux.Handle(host.PatternBoxGitletBlob, a.boxCodeRoute(gitlet.GitBlob))
	mux.Handle(host.PatternBoxGitletDiff, a.boxCodeRoute(gitlet.GitDiff))
	// A WRITE TO THE DRAIN GATE, refused while draining like every POST: a
	// check is new work on the node, a repair a write into its copy. Only
	// where git is served: it checks THIS node's cache, which a node without
	// the role does not keep — and so is the read of one, which a draining
	// node still answers.
	rest := authz.NewRouter(mux, guard)
	for pattern, h := range map[string]http.HandlerFunc{
		"POST /gitlet/repos/{name}/verify":     a.serveCodeVerify,
		"GET /gitlet/repos/{name}/verify/{id}": a.serveCodeVerification,
	} {
		policy := deploymentPolicy(pattern)
		if strings.HasPrefix(pattern, http.MethodPost+" ") {
			// WHAT IT DOES, as every POST says ([authz.Effect]): it starts a
			// verification of this node's copy, which the read beside it
			// names by its id — a new record, whatever it finds or repairs.
			policy.Effect = authz.EffectCreates
		}
		mount(rest, pattern, policy, h)
	}
	return errors.Join(failures...)
}

// gitRefusal renders a request a git route's router did not admit in the one
// form git prints — a line of text, never the JSON envelope, which git shows
// nothing of — under the status and the code a JSON route answers the same
// decision with: the authority table's one mapping ([authz.RefusalFields]),
// and for a decision this node could not make the 503 [authz.EnvelopeRefusal]
// answers it with, saying when to come back — so the two cannot drift apart.
// Every verb a git route is mounted on admits every holder of the gitlet app
// the guard resolved, so what reaches this is a principal without the app, one
// whose stage may not act, or a node that could not decide.
func gitRefusal(w http.ResponseWriter, _ *http.Request, _ authz.Policy, d authz.Decision) {
	if d.Unknown() {
		auth.GitUnavailable(w, authz.RetryUndecidedSeconds, gitSentence(httpjson.CodeUnavailable, nil))
		return
	}
	status, refused, detail := authz.RefusalFields(d)
	auth.GitText(w, status, gitSentence(refused, detail))
}

// gitSentence is a refusal as git prints it: the sentence the JSON envelope
// carries for the same refusal — the deciding rule's own, naming the
// permission where one is the remedy ([httpjson.RefusalMessage]) — with the
// code a client branches on beside it.
//
// THE RULE'S SENTENCE AND NEVER THE CODE'S, because git is the one place a
// person without the gitlet app meets the refusal: worded by its code alone,
// `unauthorized` named every permission there is, and a person who lacked
// one app was sent to ask for all of them, admin among them.
func gitSentence(refused httpjson.Code, detail httpjson.Detail) string {
	reason, _ := detail[authz.DetailReason].(string)
	grants, _ := detail[authz.DetailGrants].([]string)
	return strings.TrimSuffix(httpjson.RefusalMessage(refused, reason, grants), ".") +
		" (" + string(refused) + ")"
}

// gitRoute serves one of the host's git handlers, resolved for this request,
// or answers in git's own form why this node serves none now
// ([unservedGit]).
func (a *App) gitRoute(pick func(GitRoutes) http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g, why := a.code.GitRoutes()
		if why != nil {
			unservedGit(w, why)
			return
		}
		pick(g).ServeHTTP(w, r)
	})
}

// boxGitRoute is [App.gitRoute] for a box's route, which tells WHY this node
// serves none of Gitlet now only to a request presenting a token this fleet
// signed ([CodeSurface.VerifyBox]) and answers any other with the box
// admission's challenge, [host.ChallengeBox].
//
// Only a box's route waits on the token: /box/git/ is PUBLIC (api.public), so a
// reason answered before any credential would tell anybody who reaches the
// listener the deployment's state. A person's routes keep their answers: the
// guard stands in front of them. The challenge is the admission's own step, so a
// box whose helper has not answered yet (git's first request carries no
// credential) is challenged in the same words whether or not the node serves
// Gitlet; the keyring every node holds verifies the token either way.
func (a *App) boxGitRoute(pick func(GitRoutes) http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g, why := a.code.GitRoutes()
		if why == nil {
			pick(g).ServeHTTP(w, r)
			return
		}
		if host.ChallengeBox(w, r, a.code.VerifyBox, log) {
			return
		}
		unservedGit(w, why)
	})
}

// unservedGit answers a git route on a node that serves none of Gitlet
// now, by the engine's reason ([codeUnserved]) — IN GIT'S FORM, git printing
// nothing of the envelope: the sentence, and the code in brackets after it
// for a script that greps what git printed.
func unservedGit(w http.ResponseWriter, why error) {
	u := codeUnserved(why)
	switch u.code {
	case httpjson.CodeNotFound:
		auth.GitText(w, http.StatusNotFound, u.detail)
	case httpjson.CodeQueryFailed:
		log.Warn("api_code_unserved_unexplained", "error", why)
		auth.GitText(w, http.StatusInternalServerError,
			"this node could not say why it does not serve Gitlet; its log says what it was told")
	default:
		auth.GitUnavailable(w, httpjson.RetrySeconds(u.retryAfter), u.detail+" ("+string(u.code)+")")
	}
}

// codeNotServed answers a /gitlet/ route this node serves none of Gitlet
// for now, in the read surface's vocabulary and through its one writer
// ([writeQueryError]), as the code questions answer the same node: a company
// hosting no code is `not_found` with the reason that tells it from a
// repository nobody declared, and every 503 is written by
// [httpjson.UnavailableWith], which sets its Retry-After — or, where waiting
// clears nothing, none — with the envelope.
func codeNotServed(w http.ResponseWriter, r *http.Request, what string, why error) {
	writeQueryError(w, r, what, codeUnserved(why).question(why))
}

// verificationView is a verification as the API answers it: the operation's
// id, its repository and repair, where it is and when — and, while it checks
// the copy, when the node ends that check by — the report — what the check
// found, everything once it is done and what it found before it failed once
// it failed — and, failed, why, in the read surface's vocabulary.
type verificationView struct {
	ID       string              `json:"id"`
	Repo     string              `json:"repo"`
	Repair   bool                `json:"repair"`
	State    host.VerifyState    `json:"state"`
	Started  time.Time           `json:"started"`
	Deadline time.Time           `json:"deadline,omitzero"`
	Ended    time.Time           `json:"ended,omitzero"`
	Report   *cache.VerifyReport `json:"report,omitempty"`
	Error    string              `json:"error,omitempty"`
	Detail   string              `json:"detail,omitempty"`
}

// viewOf is v as the API answers it. A failure's sentence is the caller's to
// read, unlike a read's 500: the route is always guarded, its reader an
// operator who asked a node to check itself, and its failures are written for
// exactly that reader — the node whose memory cannot hold the check, the pack
// the object store lacks, the stop that cut it short.
func viewOf(v host.Verification) verificationView {
	out := verificationView{ID: v.ID.String(), Repo: v.Repo, Repair: v.Repair, State: v.State,
		Started: v.Started.UTC(), Ended: v.Ended.UTC()}
	if v.State == host.VerifyRunning {
		out.Deadline = v.Deadline.UTC()
	}
	if v.State != host.VerifyRunning && (v.Report.Repo != "" || v.Report.Name != "") {
		report := v.Report
		out.Report = &report
	}
	if v.State == host.VerifyFailed {
		out.Error, out.Detail = stream.CodeQueryFailed, v.Err.Error()
		if errors.Is(v.Err, host.ErrStopping) || errors.Is(v.Err, gitproc.ErrBusy) ||
			errors.Is(queries.ClassifyFailure(v.Err), queries.ErrUnavailable) {
			out.Error = stream.CodeUnavailable
		}
	}
	return out
}

// verificationPath is where a verification is read.
func verificationPath(v host.Verification) string {
	return "/gitlet/repos/" + v.Repo + "/verify/" + v.ID.String()
}

// serveCodeVerify begins a check of THIS node's cache of one repository
// against its rows, and with `?repair=true` a repair of what it finds out of
// place. It reads every byte, which can outlive a balancer's read timeout, so
// it runs on the node's own lifetime (host.Server.StartVerify): the request
// waits up to host.VerifyHold and answers `200` with the verification ended, or
// `202 Accepted` with it running, its address in `Location`
// ([App.serveCodeVerification]).
// REFUSED AT ONCE, having read nothing: not declared or not held here (404; a
// node with no copy has nothing to check), a full maintenance lane or no memory
// for it (503, host.BusyRetryAfter), memory that cannot hold it at all (500),
// the other repair setting running (409), a stopping host (503,
// host.StoppingRetryAfter). A second asking is handed the running check.
func (a *App) serveCodeVerify(w http.ResponseWriter, r *http.Request) {
	g, why := a.code.GitRoutes()
	if why != nil {
		codeNotServed(w, r, "code_verify", why)
		return
	}
	repair := false
	switch values := r.URL.Query()["repair"]; {
	case len(values) == 0:
	case len(values) == 1 && values[0] == "false":
	case len(values) == 1 && values[0] == "true":
		repair = true
	default:
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeBadParams,
			map[string]string{"detail": "repair is true or false"})
		return
	}
	// THE CALLER'S FLOOR, where it names one: a position on the code log it
	// read at, which the copy is brought to with no barrier (see
	// host.Server.StartVerify). Read by the read grammar's own rule
	// ([statelog.ParseFloors]: `min_position` names one floor per log), so a
	// floor on any log but the code log is refused naming it, as every read
	// of one log refuses it, rather than every floor but the first dropped.
	floors, err := statelog.ParseFloors(queries.FromQuery(r.URL.Query()))
	asked := statelog.Freshness{Floors: floors}
	if err == nil {
		err = asked.OnlyOn(topics.CodeLogStream)
	}
	if err != nil {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeBadParams,
			map[string]string{"detail": err.Error()})
		return
	}
	floor := asked.Floor(topics.CodeLogStream)
	name := r.PathValue("name")
	v, err := g.StartVerify(r.Context(), name, repair, floor)
	var running *host.VerifyRunningError
	switch {
	case err == nil:
		w.Header().Set("Location", verificationPath(v))
		hold, cancel := context.WithTimeout(r.Context(), host.VerifyHold)
		now, kept := g.AwaitVerification(hold, name, v.ID)
		cancel()
		if kept {
			v = now
		}
		status := http.StatusAccepted
		if v.State != host.VerifyRunning {
			status = http.StatusOK
		}
		writeJSON(w, status, viewOf(v))
	case errors.Is(err, gitlet.ErrRepoNotFound):
		httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNotFound,
			map[string]string{"reason": queries.ReasonNotDeclared, "detail": queries.RepoNotDeclared(name)})
	case errors.Is(err, gitlet.ErrInvalid):
		// A floor on another log than the repositories' rows.
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeBadParams,
			map[string]string{"detail": err.Error()})
	case errors.Is(err, cache.ErrNotHeld):
		httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNotFound,
			map[string]string{"reason": reasonNotHeld,
				"detail": fmt.Sprintf("this node holds no copy of %q, so it has "+
					"nothing to check: verify the Gitlet nodes that serve it", name)})
	case errors.As(err, &running):
		httpjson.FailWithFields(w, http.StatusConflict, httpjson.CodeVerifyRunning,
			httpjson.Detail{"detail": err.Error(), "running": viewOf(running.Running)})
	case errors.Is(err, host.ErrStopping):
		// THE STOP'S OWN WAIT, the one every git request a stopping host
		// refuses is told: what takes the node out of rotation.
		httpjson.UnavailableWith(w, httpjson.CodeUnavailable,
			httpjson.RetrySeconds(host.StoppingRetryAfter), httpjson.Detail{
				"detail": "this node's Gitlet is stopping: verify against another node's cache, or " +
					"this one's once it has restarted"})
	case errors.Is(err, gitproc.ErrBusy):
		// REFUSED BEFORE IT READ ANYTHING — the check takes its slot and
		// its memory first, never waits for either — so the retry costs
		// what this refusal did: nothing. At the wait a busy git request on
		// this node is told.
		httpjson.UnavailableWith(w, httpjson.CodeUnavailable,
			httpjson.RetrySeconds(host.BusyRetryAfter), httpjson.Detail{"detail": err.Error()})
	case errors.Is(queries.ClassifyFailure(err), queries.ErrUnavailable):
		// What clears by waiting — the code log unreadable here, no data
		// node serving the resolve — with the refusal's own Retry-After.
		writeQueryError(w, r, "code_verify", queries.ClassifyFailure(err))
	default:
		// The sentence too, for viewOf's reason: a node whose memory
		// cannot hold the check names the node to run it on.
		log.Warn("api_code_verify_refused", "repo", name, "repair", repair, "error", err)
		httpjson.FailWith(w, http.StatusInternalServerError, httpjson.CodeQueryFailed,
			map[string]string{"detail": err.Error()})
	}
}

// serveCodeVerification answers one verification of this node's cache —
// running, done with its report, or failed with why and what it found before
// — while the node keeps it: an hour after it ended (host.VerifyKept), until
// the repository's next verify begins, and never across a restart. A READ,
// so a draining node still answers it.
func (a *App) serveCodeVerification(w http.ResponseWriter, r *http.Request) {
	g, why := a.code.GitRoutes()
	if why != nil {
		codeNotServed(w, r, "code_verification", why)
		return
	}
	name := r.PathValue("name")
	id, err := uuid.Parse(r.PathValue("id"))
	var v host.Verification
	found := err == nil
	if found {
		v, found = g.Verification(name, id)
	}
	if !found {
		httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNotFound, map[string]string{
			"detail": fmt.Sprintf("this node keeps no verify %s of %q: one is kept for an hour after it "+
				"ends, until the repository's next verify begins, and never across a restart",
				r.PathValue("id"), name)})
		return
	}
	writeJSON(w, http.StatusOK, viewOf(v))
}
