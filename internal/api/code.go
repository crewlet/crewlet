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
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/host"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/queue/topics"
	seatplacement "github.com/crewlet/crewlet/internal/seat/placement"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// THE ENGINE'S OWN GIT HOST, ON THE API: smart HTTP under /git/ for a person's
// git client, the cache's verify under /code/, and what every node answers
// when it serves none of the code host now.
//
// Both prefixes are GUARDED, as every route the exemption list
// ([auth.Unguarded]) does not name is, and neither is ever on api.public
// ([auth.Public] names neither): a repository is the company's
// source, and a person reaches it with their own bound token — the third person
// transport beside the act route and the operator's MCP (ADR-0055). On a git
// route, and on no other, that token may arrive as a Basic credential's
// password, because a git client presents one no other way.
//
// /box/git/ is: a coding box's routes, the code host's third prefix, are
// published beside the box's other two edges and admitted by the run, never
// by an operator token. The same three handlers serve them behind the code
// host's box admission, which reads the run's token from git's Basic password
// and the run's row on every request ([auth.BoxGitPrefix]); mounted on the
// route table itself, like /mcp/ and /otlp/, never through the
// [authz.Router] the person's routes take, since a box's request reaches the
// handler anonymous to the guard and a router would refuse it before its
// admission ran.
//
// THE CODE HOST'S ROWS ARE QUESTIONS, not routes of this file: `code_repos`,
// `code_repo` and `code_pushes`, registered in internal/api/queries and served
// on their /code/ paths through the named routes (rest.go), so a person, the
// socket and the dashboard reading its own writes back at a session floor are
// all answered by one implementation — and so is who read a unit's
// repository, `code_repo_readers`, an admin's question on the same terms. This
// file hands them the code domain's read and the usage domain's readers, per
// question, with why this node has none ([codeQuestions]).
//
// # Mounted from what is known at New, served from what the engine has now
//
// Whether the GIT routes exist is a fact about THIS NODE, known when the app is
// assembled: its Tier A roles name `git`, or name none. A node without the role
// mounts none of them, and a git request to it is the mux's own `no_route`,
// exactly as a route this node's configuration does not enable has always
// answered. The READS are questions on every API: a data node answers them
// from its own copy, and any other node through the estate router.
//
// Whether a route or a question is SERVED is asked of the engine on every
// request ([CodeSurface]), as the native halves are (native.go): the code host
// and its reads are built with the native runtime, at boot or at the first
// apply whose company sets code.native — on a node that booted with no
// company, after the app was assembled. One that finds nothing answers WHY, as
// the engine decides it from what the fleet activated
// ([engine.Engine.CodeUnserved]): a company that hosts no code is a 404, and a
// node that serves none of it now while the configuration the fleet activated
// hosts code — or before this node has read whether it does — a 503 that says
// which of four it is ([codeUnserved]). A coding box is told only once its
// request presents a run token this fleet signed, and challenged until then
// ([App.boxGitRoute]): its prefix is public, and the reason is the
// deployment's state.

// GitRoutes is the code host as the API mounts it; *host.Server satisfies it.
// The Box handlers are the same three entry points behind a coding box's
// admission rather than a person's.
type GitRoutes interface {
	InfoRefs() http.Handler
	UploadPack() http.Handler
	ReceivePack() http.Handler
	BoxInfoRefs() http.Handler
	BoxUploadPack() http.Handler
	BoxReceivePack() http.Handler
	StartVerify(ctx context.Context, name string, repair bool, floor statelog.Position) (host.Verification, error)
	Verification(name string, id uuid.UUID) (host.Verification, bool)
	AwaitVerification(ctx context.Context, name string, id uuid.UUID) (host.Verification, bool)
}

// CodeSurface is the code host and its reads ASKED ON EVERY REQUEST, each
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
	// GitRoutes is this node's code host, or why it serves none.
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

	// VerifyBox is whether token is a coding box's run token this fleet
	// signed: the engine's own box tokens (host.BoxTokens.Verify), over the
	// keyring Tier A derives on EVERY node whatever the company runs — so it
	// answers where GitRoutes finds no code host, which is the one place it
	// is asked: a box route there tells a box why only once its token
	// verifies ([App.boxGitRoute]).
	VerifyBox(token string) bool
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

// notHosted is what a code route answers where the company hosts no code: ONE
// sentence, in git's text/plain form on a git route and as a `not_found`
// detail on a /code/ read.
const notHosted = "this company does not host code (code.native is not set)"

// codeApplyingRetryAfter is what a node applying the configuration the fleet
// activated tells a code request to wait: stream.HealthInterval (5 s). An
// apply that builds the code host is a handful of opens and a cache's boot —
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
// of it, so a sweep of every repository on one git node passes over the ones
// that node does not keep (nothing there can be damaged) and fails on a name
// nobody declared. A git route says each in git's one text/plain line, where
// nothing reads a reason: the sentence is all git prints.
const reasonNotHeld = "not_held"

// unserved is why a node serves none of the code host now, as a code route
// answers it ([codeUnserved]).
type unserved struct {
	// code is the answer's code: [httpjson.CodeNotFound] for a company that
	// hosts no code, [httpjson.CodeQueryFailed] for a reason the seam does
	// not name, and otherwise one of the code host's four 503s.
	code httpjson.Code

	// retryAfter is the wait a 503 carries, zero where waiting clears
	// nothing; detail the sentence that says which it is. The sentence
	// names no code, because each answer names it once: the envelope's
	// `error` on a /code/ route, and the brackets git's one line ends with.
	retryAfter time.Duration
	detail     string
}

// codeUnserved is how a code route answers a node that serves none of the
// code host now, by the engine's reason:
//
//   - not hosted: 404 `not_found`, [notHosted];
//   - applying: 503 `applying` with Retry-After [codeApplyingRetryAfter] —
//     the node is applying the configuration the fleet activated and answers
//     once that apply ends. It says nothing of what that configuration holds:
//     an apply is in flight before it has decoded the revision too, and a
//     company that never set code.native must not be told the code host is
//     coming;
//   - apply failed: 503 `config_apply_failed` and no Retry-After — the node's
//     apply of a configuration that hosts code failed, and nothing it waits
//     for clears that; another node serves it;
//   - unknown: 503 `code_unknown` and no Retry-After, saying instead that the
//     node could not read the configuration the fleet activated and so cannot
//     say whether the company hosts code — the apply failed before the
//     revision was decoded, and config_apply_failed's sentence that it hosts
//     code would state what the node never read;
//   - needs restart: 503 `native_backend_needs_restart` and no Retry-After —
//     the node's native runtime was running before code.native was set, and
//     only its restart clears it; another node serves it.
//
// Any other error breaks the seam's contract: 500, logged by whichever
// rendering answers it.
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
			detail: "this node's native runtime was running before code.native was set, and it serves the " +
				"code host once restarted: ask another node, or restart this one"}
	}
	return unserved{code: httpjson.CodeQueryFailed}
}

// question is u in the read surface's vocabulary, which the code questions
// answer and every /code/ route renders ([writeQueryError]): a company hosting
// no code is [queries.NotFound] (`not_hosted`); a node that will serve the code
// host once something clears is [queries.ErrUnavailable] under the 503's own
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
	return b != nil && b.Profile("").Roles.Has(seatplacement.RoleGit)
}

// mountCode registers the code host's routes — the git transport and the
// cache's verify, on a node whose roles name git and on no other — each
// through [authz.Router] on the verb it is decided by, and refuses a route
// mounted with no policy, as every guarded surface does. The code host's ROWS
// are not among them, nor who read a unit's repository: they are questions, on
// the named routes (rest.go).
//
// # Which verb, and why the guard alone was not enough
//
// They were mounted on the plain table, so they asked only what the guard in
// front of every route asks — whether somebody resolved — and a person holding
// nothing but the overview, or a machine token carrying one app, could start a
// verify, and its repair, of every repository a git node keeps. Now:
//
//   - THE GIT ROUTES admit everybody the guard resolved
//     ([authz.ActionCodeGit]), as the code questions do ([authz.ActionCodeRead]):
//     what each may fetch or push is the per-repository grant the serving node
//     derives from the chart once the repository is read (code.GrantOf,
//     ADR-0058), which no route can ask before it.
//   - A VERIFY AND ITS READ are the deployment's own controls, admin's as a
//     backup is, decided by [deploymentPolicy] — a check of THIS NODE's copy
//     that reads every byte of it and holds a maintenance slot, and with a
//     repair rewrites what every fetch on the node is served from: an
//     operation on the node, which no repository's grant decides.
//
// A GIT ROUTE'S REFUSAL IS IN GIT'S FORM ([gitRefusal]): git prints one line
// of text and nothing of the JSON envelope a REST route answers.
//
// A CODING BOX'S THREE ROUTES are mounted on the route table ITSELF, beside
// the person's, as /mcp/ and /otlp/ are — never through a router. A router
// decides every route it mounts on the request's iam principal
// ([authz.ContextGuard]), and a box's request reaches the handler anonymous:
// the guard never resolves a run's token, which is no credential of its, so
// behind a router every box request would be refused before the code host's
// box admission — the run's token, its row and the seat's grant, read on every
// request — ever ran. They are served through [App.boxGitRoute], resolved per
// request as the person's are, so a node whose code host is not serving tells
// a box why in the same words it tells a person — but only once the box's
// request presents a run token this fleet signed, since the route is public
// and the reason is the deployment's state; and mounted where the person's
// are, on a node whose roles name git. The table keeps their patterns, so the
// listener walk publishes them ([auth.BoxGitPrefix] is public).
//
// The git patterns are the host's own constants, so the path a handler reads
// its repository from is the path it was mounted at, and this file spells no
// git path a classifier gate would have to allow.
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
	mux.Handle(host.PatternBoxInfoRefs, a.boxGitRoute(GitRoutes.BoxInfoRefs))
	mux.Handle(host.PatternBoxUploadPack, a.boxGitRoute(GitRoutes.BoxUploadPack))
	mux.Handle(host.PatternBoxReceivePack, a.boxGitRoute(GitRoutes.BoxReceivePack))
	// A WRITE TO THE DRAIN GATE, refused while draining like every POST: a
	// check is new work on the node, a repair a write into its copy. Only
	// where git is served: it checks THIS node's cache, which a node without
	// the role does not keep — and so is the read of one, which a draining
	// node still answers.
	rest := authz.NewRouter(mux, guard)
	for pattern, h := range map[string]http.HandlerFunc{
		"POST /code/repos/{name}/verify":     a.serveCodeVerify,
		"GET /code/repos/{name}/verify/{id}": a.serveCodeVerification,
	} {
		mount(rest, pattern, deploymentPolicy(pattern), h)
	}
	return errors.Join(failures...)
}

// gitRefusal renders a request a git route's router did not admit in the one
// form git prints — a line of text, never the JSON envelope, which git shows
// nothing of — under the status and the code a JSON route answers the same
// decision with ([authz.RefusalCode]), so the two cannot drift apart. A
// decision this node could not make says when to come back, as the envelope's
// 503 does. Every verb a git route is mounted on admits everybody the guard
// resolved, so what reaches this is a principal whose stage may not act, or a
// node that could not decide.
func gitRefusal(w http.ResponseWriter, _ *http.Request, _ authz.Policy, d authz.Decision) {
	status, refused := authz.RefusalCode(d)
	sentence := strings.TrimSuffix(refused.Message(), ".") + " (" + string(refused) + ")"
	if d.Unknown() {
		auth.GitUnavailable(w, authz.RetryUndecidedSeconds, sentence)
		return
	}
	auth.GitText(w, status, sentence)
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

// boxGitRoute is [App.gitRoute] for a coding box's route, which tells WHY this
// node serves no code host now only to a request presenting a run token this
// fleet signed ([CodeSurface.VerifyBox]), and answers any other the code
// host's own challenge — the box admission's credential step,
// [host.ChallengeBox].
//
// # Why a box's route, and only a box's, waits on the token
//
// /box/git/ is PUBLIC: served on api.public, reached from wherever a sandbox
// runs, so anybody who can reach the listener can ask it. Answered before any
// credential, the reason told whoever asked the deployment's state — whether
// the company hosts code (404), that this node is applying the configuration
// the fleet activated, that its apply failed, that it could not read that
// configuration, that its runtime is owed a restart (503), or that the seam
// broke its contract (500). A box holding its run's token is told exactly
// what a person is, since what it says is the box's to act on: retry, or ask
// another node. A person's routes keep their answers: the guard stands in
// front of them, so nobody reaches a reason there without a credential.
//
// THE SAME CHALLENGE THE ADMISSION ANSWERS, by the one step both run, so a box
// whose credential helper has not answered yet — git's first request carries
// no credential — is challenged in the same words by a node that serves the
// code host and by one that serves none, and git asks its helper as it always
// does: the token it presents then is told the reason. The token is verified
// by the engine's own box tokens over the keyring every node holds, so a node
// with no code host built — every node this answers for — verifies it as a
// node serving one does.
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

// unservedGit answers a git route on a node that serves none of the code host
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
			"this node could not say why it serves no code host; its log says what it was told")
	default:
		auth.GitUnavailable(w, httpjson.RetrySeconds(u.retryAfter), u.detail+" ("+string(u.code)+")")
	}
}

// codeNotServed answers a /code/ route this node serves none of the code host
// for now, in the read surface's vocabulary and through its one writer
// ([writeQueryError]), as the code questions answer the same node: a company
// hosting no code is `not_found` with the reason that tells it from a
// repository nobody declared, and every 503 is written by
// [httpjson.UnavailableWith], which sets its Retry-After — or, where waiting
// clears nothing, none — with the envelope.
func codeNotServed(w http.ResponseWriter, what string, why error) {
	writeQueryError(w, what, codeUnserved(why).question(why))
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
	return "/code/repos/" + v.Repo + "/verify/" + v.ID.String()
}

// serveCodeVerify begins a check of THIS node's cache of one repository
// against its rows — and with `?repair=true` a repair of whatever it finds
// out of place. The check reads every byte of the repository, which can
// outlive a balancer's read timeout, so it runs on the node's own lifetime
// rather than the request's (host.Server.StartVerify): the request waits for
// it up to host.VerifyHold and answers `200` with the verification ended — a
// small copy's check is answered where it was asked — or `202 Accepted` with
// it running, its address in `Location`, where its caller reads the outcome
// ([App.serveCodeVerification]).
//
// WHAT IS REFUSED IS REFUSED AT ONCE, having read nothing of the repository:
// not declared or not held here (404 — a node holding no copy has nothing to
// check, and building one is a whole download), a full maintenance lane or
// no room in the memory the background work shares (503, Retry-After
// host.BusyRetryAfter), a node whose memory cannot hold the check at all
// (500 with its sentence), a check of the other repair setting running
// (409, naming it), and a stopping host (503, Retry-After
// host.StoppingRetryAfter). A second asking for the check running is handed
// it.
func (a *App) serveCodeVerify(w http.ResponseWriter, r *http.Request) {
	g, why := a.code.GitRoutes()
	if why != nil {
		codeNotServed(w, "code_verify", why)
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
	// ([tracker.ParseFloors]: `min_position` names one floor per log), so a
	// floor on any log but the code log is refused naming it, as every read
	// of one log refuses it, rather than every floor but the first dropped.
	floors, err := tracker.ParseFloors(queries.FromQuery(r.URL.Query()))
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
	case errors.Is(err, code.ErrRepoNotFound):
		httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNotFound,
			map[string]string{"reason": queries.ReasonNotDeclared, "detail": queries.RepoNotDeclared(name)})
	case errors.Is(err, code.ErrInvalid):
		// A floor on another log than the repositories' rows.
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeBadParams,
			map[string]string{"detail": err.Error()})
	case errors.Is(err, cache.ErrNotHeld):
		httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNotFound,
			map[string]string{"reason": reasonNotHeld,
				"detail": fmt.Sprintf("this node holds no copy of %q, so it has "+
					"nothing to check: verify the git nodes that serve it", name)})
	case errors.As(err, &running):
		httpjson.FailWithFields(w, http.StatusConflict, httpjson.CodeVerifyRunning,
			httpjson.Detail{"detail": err.Error(), "running": viewOf(running.Running)})
	case errors.Is(err, host.ErrStopping):
		// THE STOP'S OWN WAIT, the one every git request a stopping host
		// refuses is told: what takes the node out of rotation.
		httpjson.UnavailableWith(w, httpjson.CodeUnavailable,
			httpjson.RetrySeconds(host.StoppingRetryAfter), httpjson.Detail{
				"detail": "this node's code host is stopping: verify against another node's cache, or " +
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
		writeQueryError(w, "code_verify", queries.ClassifyFailure(err))
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
		codeNotServed(w, "code_verification", why)
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
