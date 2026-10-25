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
	"github.com/crewlet/crewlet/internal/iam"
	seatplacement "github.com/crewlet/crewlet/internal/seat/placement"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE ENGINE'S OWN GIT HOST, ON THE API: smart HTTP under /git/ for a person's
// git client, and the code host's REST reads under /code/.
//
// Both prefixes are GUARDED, as every route the exemption list
// ([auth.Unguarded]) does not name is, and neither is ever on api.public
// ([auth.Public] names neither): a repository is the company's
// source, and a person reaches it with their own bound token — the third person
// transport beside the act route and the operator's MCP (ADR-0052). On a git
// route, and on no other, that token may arrive as a Basic credential's
// password, because a git client presents one no other way.
//
// # Mounted from what is known at New, served from what the engine has now
//
// Whether the GIT routes exist is a fact about THIS NODE, known when the app is
// assembled: its Tier A roles name `git`, or name none. A node without the role
// mounts none of them, and a git request to it is the mux's own `no_route`,
// exactly as a route this node's configuration does not enable has always
// answered. The READS are mounted on every API: a data node answers them from
// its own copy, and any other node through the estate router.
//
// Whether a route is SERVED is asked of the engine on every request
// ([CodeSurface]), as the native halves are (native.go): the code host and its
// reads are built with the native runtime, at boot or at the first apply whose
// company sets code.native — on a node that booted with no company, after the
// app was assembled. A route that finds nothing answers WHY, as the engine decides it
// from what the fleet activated ([engine.Engine.CodeUnserved]): a company that
// hosts no code is a 404, and a node that serves none of it now while the
// configuration the fleet activated hosts code — or before this node has read
// whether it does — a 503 that says which of four it is ([codeUnserved]).

// GitRoutes is the code host as the API mounts it; *host.Server satisfies it.
type GitRoutes interface {
	InfoRefs() http.Handler
	UploadPack() http.Handler
	ReceivePack() http.Handler
	StartVerify(ctx context.Context, name string, repair bool, floor statelog.Position) (host.Verification, error)
	Verification(name string, id uuid.UUID) (host.Verification, bool)
	AwaitVerification(ctx context.Context, name string, id uuid.UUID) (host.Verification, bool)
}

// CodeReads is the code domain's read, wherever its rows are served;
// estate.CodeSeam satisfies it.
type CodeReads interface {
	Read(ctx context.Context, q code.ReadQuery) (code.ReadAnswer, error)
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

	// CodeReads is the code domain's read, or why this node serves none.
	CodeReads() (CodeReads, error)
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

// THE REASONS a code route's 404 can name: every one is `not_found`, since none
// has anything here to answer, and the reason is what tells them apart without
// reading the sentence, because a client acts on each a different way.
//
//   - reasonNotDeclared — nobody declared a repository of that name: declaring
//     one in code.repos, and waiting for the activation, is what makes it;
//   - reasonNotHosted — the company hosts no code at all (code.native is not
//     set): declaring a repository changes nothing until it does;
//   - reasonNotHeld — a verify's repository is declared and THIS node holds no
//     copy of it: a sweep of every repository on one git node passes over the
//     ones that node does not keep (nothing there can be damaged) and fails on
//     a name nobody declared.
//
// A git route says the same in git's one text/plain line, where nothing reads
// a reason: the sentence is all git prints.
const (
	reasonNotDeclared = "not_declared"
	reasonNotHosted   = "not_hosted"
	reasonNotHeld     = "not_held"
)

// notDeclared is the sentence a repository nobody declared is answered with,
// naming the setting that declares one.
func notDeclared(name string) string {
	return fmt.Sprintf("no repository named %q is declared in code.repos", name)
}

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
// Any other error breaks the seam's contract: 500, logged.
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
	log.Warn("api_code_unserved_unexplained", "error", err)
	return unserved{code: httpjson.CodeQueryFailed}
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

// mountCode registers the code host's routes — see the file's doc for which
// are mounted where — each through [authz.Router] on the verb it is decided
// by, and refuses a route mounted with no policy, as every guarded surface
// does.
//
// # Which verb, and why the guard alone was not enough
//
// They were mounted on the plain table, so they asked only what the guard in
// front of every route asks — whether somebody resolved — and a person holding
// nothing but the overview, or a machine token carrying one app, could start a
// verify, and its repair, of every repository a git node keeps. Now:
//
//   - THE READS AND THE GIT ROUTES admit everybody the guard resolved
//     ([authz.ActionCodeRead], [authz.ActionCodeGit]): what each may read,
//     fetch or push is the per-repository grant the serving node derives from
//     the chart once the repository is read (code.GrantOf, ADR-0055), which
//     no route can ask before it.
//   - A VERIFY AND ITS READ are the deployment's own controls, admin's as a
//     backup is, decided by [deploymentPolicy] — a check of THIS NODE's copy
//     that reads every byte of it and holds a maintenance slot, and with a
//     repair rewrites what every fetch on the node is served from: an
//     operation on the node, which no repository's grant decides.
//
// A GIT ROUTE'S REFUSAL IS IN GIT'S FORM ([gitRefusal]): git prints one line
// of text and nothing of the JSON envelope a REST route answers.
//
// The git patterns are the host's own constants, so the path a handler reads
// its repository from is the path it was mounted at, and this file spells no
// git path a classifier gate would have to allow.
func (a *App) mountCode(mux authz.Mux) error {
	guard := authz.ContextGuard(authz.NoChart{})
	rest := authz.NewRouter(mux, guard)
	var failures []error
	mount := func(router *authz.Router, pattern string, p authz.Policy, h http.Handler) {
		if err := router.Handle(pattern, p, h); err != nil {
			failures = append(failures, err)
		}
	}
	if a.servesGit {
		git := authz.NewRouter(mux, guard).Refusing(gitRefusal)
		transport := authz.Policy{Action: authz.ActionCodeGit}
		mount(git, host.PatternInfoRefs, transport, a.gitRoute(GitRoutes.InfoRefs))
		mount(git, host.PatternUploadPack, transport, a.gitRoute(GitRoutes.UploadPack))
		mount(git, host.PatternReceivePack, transport, a.gitRoute(GitRoutes.ReceivePack))
		// A WRITE TO THE DRAIN GATE, refused while draining like every
		// POST: a check is new work on the node, a repair a write into its
		// copy. Only where git is served: it checks THIS node's cache, which
		// a node without the role does not keep — and so is the read of one,
		// which a draining node still answers.
		for pattern, h := range map[string]http.HandlerFunc{
			"POST /code/repos/{name}/verify":     a.serveCodeVerify,
			"GET /code/repos/{name}/verify/{id}": a.serveCodeVerification,
		} {
			mount(rest, pattern, deploymentPolicy(pattern), h)
		}
	}
	read := authz.Policy{Action: authz.ActionCodeRead}
	mount(rest, "GET /code/repos", read, a.serveCodeRead(code.ReadRepos, "code_repos"))
	mount(rest, "GET /code/repos/{name}", read, a.serveCodeRead(code.ReadRepo, "code_repo"))
	mount(rest, "GET /code/repos/{name}/pushes", read, a.serveCodeRead(code.ReadPushes, "code_pushes"))
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
// or answers in git's own form why this node serves none now.
func (a *App) gitRoute(pick func(GitRoutes) http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g, why := a.code.GitRoutes()
		if why != nil {
			// IN GIT'S FORM, git printing nothing of the envelope: the
			// sentence, and the code in brackets after it for a script
			// that greps what git printed.
			u := codeUnserved(why)
			switch u.code {
			case httpjson.CodeNotFound:
				auth.GitText(w, http.StatusNotFound, u.detail)
			case httpjson.CodeQueryFailed:
				auth.GitText(w, http.StatusInternalServerError,
					"this node could not say why it serves no code host; its log says what it was told")
			default:
				auth.GitUnavailable(w, httpjson.RetrySeconds(u.retryAfter),
					u.detail+" ("+string(u.code)+")")
			}
			return
		}
		pick(g).ServeHTTP(w, r)
	})
}

// codeNotServed answers a /code/ route this node serves none of the code host
// for now, in the refusal envelope, with why ([codeUnserved]): a company
// hosting no code is `not_found` with the reason that tells it from a
// repository nobody declared, and every 503 is written by
// [httpjson.UnavailableWith], which sets its Retry-After — or, where waiting
// clears nothing, none — with the envelope.
func codeNotServed(w http.ResponseWriter, why error) {
	u := codeUnserved(why)
	switch u.code {
	case httpjson.CodeNotFound:
		httpjson.FailWith(w, http.StatusNotFound, u.code,
			map[string]string{"reason": reasonNotHosted, "detail": u.detail})
	case httpjson.CodeQueryFailed:
		httpjson.Fail(w, http.StatusInternalServerError, u.code)
	default:
		httpjson.UnavailableWith(w, u.code, httpjson.RetrySeconds(u.retryAfter),
			httpjson.Detail{"detail": u.detail})
	}
}

// serveCodeRead answers one of the code domain's reads: a listing of
// repositories, one repository in detail, or one repository's push history.
//
// AT THE CALLER'S LEVEL, read through the tree's ONE freshness grammar
// ([queries.Freshness]) — `read_level`, `max_lag_seconds`, `max_lag_seq`,
// `min_position` — at the REST read path's surface, so a stale listing is the
// default and a client that must not conclude "not declared" from an old copy
// asks for linearizable. The answer carries the position and the level it was
// served at. A level the reader does not serve is the reader's to refuse,
// naming the levels it serves ([code.ReadLevels]): `session` among them, since
// it holds no position of a caller's on the code log.
func (a *App) serveCodeRead(kind code.ReadKind, what string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads, why := a.code.CodeReads()
		if why != nil {
			codeNotServed(w, why)
			return
		}
		// A CODE READ IS HELD TO WHO ASKS, so one asked for nobody is
		// refused rather than judged as somebody. The guard answers every
		// request it admits to this route with a resolved principal, and
		// [auth.Caller] writes the refusal for anything else in the
		// engine's three arms: 401 for a caller who presented nothing it
		// accepts, 503 for a node that could not tell — and a request that
		// reached the route past no guard at all is that 503 too, logged
		// as the wiring fault it is, never a read.
		p, ok := auth.Caller(w, r)
		if !ok {
			return
		}
		q, err := codeReadQuery(kind, codeAsker(p), r)
		if err != nil {
			writeQueryError(w, what, err)
			return
		}
		answer, err := reads.Read(r.Context(), q)
		if err != nil {
			codeRefusal(w, what, q.Repo, err)
			return
		}
		writeJSON(w, http.StatusOK, answer)
	})
}

// codeAsker is who a REST code read asks as: the principal the guard resolved
// for the request, under the credential it came through ([iam.ActorFor]), and
// the human seat it holds — a person's own, or the one the identity directory
// binds a token to — or none. Every principal a person presents reads every
// repository (ADR-0055), so the answer a person gets is the whole listing; what
// the asker buys here is that the reader judges every read by who asked it, so
// a seat's read, when one asks, cannot be answered as a person's.
func codeAsker(p iam.Principal) code.Principal {
	return code.Principal{Kind: code.PrincipalOperator,
		OperatorID: iam.ActorFor(p).OperatorID, Seat: p.Seat}
}

// codeReadQuery is a read's request, asked as asker, refused
// [queries.ErrBadParams] naming the parameter that is not one this read takes.
func codeReadQuery(kind code.ReadKind, asker code.Principal, r *http.Request) (code.ReadQuery, error) {
	p := queries.FromQuery(r.URL.Query())
	fresh, err := queries.Freshness(p)
	if err != nil {
		return code.ReadQuery{}, err
	}
	q := code.ReadQuery{Asker: asker, Kind: kind, Repo: r.PathValue("name"), Cursor: p.String("cursor"),
		Fresh: fresh}
	for v := range strings.SplitSeq(p.String("status"), ",") {
		if v = strings.TrimSpace(v); v == "" {
			continue
		}
		status := code.RepoStatus(v)
		if !status.Valid() {
			return code.ReadQuery{}, fmt.Errorf("%w: the status %q is not one of %v", queries.ErrBadParams,
				v, code.RepoStatuses())
		}
		q.Status = append(q.Status, status)
	}
	if p.Has("limit") {
		n, whole := p.WholeInt("limit")
		// int is int64 on every platform the store builds for
		// (internal/store/platform.go), so the conversion below is exact,
		// and a limit past a page's bound is the reader's to refuse, naming
		// its bound.
		if !whole || n < 0 {
			return code.ReadQuery{}, fmt.Errorf("%w: the limit %q is not a whole number of zero or more "+
				"(0 is the read's own page size)", queries.ErrBadParams, p.String("limit"))
		}
		q.Limit = int(n)
	}
	return q, nil
}

// codeRefusal answers a code read's failure in the refusal envelope, in the
// read surface's ONE vocabulary ([writeQueryError]): a repository that is not
// declared is `not_found`, naming it; a repository the asker's grant does not
// cover is `not_permitted` with the reader's own sentence; a level, a cursor
// or a parameter the reader refuses is `bad_params` with the reader's own
// sentence; and a read this node could not serve — a state-log refusal, no
// data node serving — is `unavailable` with its own Retry-After (none where
// waiting cannot clear it), by [queries.ClassifyFailure], the registry's own
// classification rather than a second list here.
//
// NOT PERMITTED IS 403 AND NEVER THE 500 everything unnamed falls into: it is
// a refusal of the CALLER ([code.ErrNotPermitted]) that every node judging the
// same grant from the same chart makes alike, so no retry and no other node
// answers it otherwise, and its sentence — who asked, which repository, the
// visibility that shut the asker out and who does read it — is the whole of
// what the caller can act on. [httpjson.CodeNotPermitted] is spelled as the
// domain's own refusal class, the word a refused push prints
// ([code.RefusalNotPermitted], held equal by test), so one refusal has one
// spelling on every surface that names it.
func codeRefusal(w http.ResponseWriter, what, repo string, err error) {
	switch {
	case errors.Is(err, code.ErrRepoNotFound):
		httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNotFound,
			map[string]string{"reason": reasonNotDeclared, "detail": notDeclared(repo)})
	case errors.Is(err, code.ErrNotPermitted):
		httpjson.FailWith(w, http.StatusForbidden, httpjson.CodeNotPermitted,
			map[string]string{"detail": refusalSentence(err, code.ErrNotPermitted)})
	case errors.Is(err, code.ErrBadLevel), errors.Is(err, code.ErrBadCursor),
		errors.Is(err, code.ErrInvalid):
		writeQueryError(w, what, fmt.Errorf("%w: %w", queries.ErrBadParams, err))
	default:
		writeQueryError(w, what, queries.ClassifyFailure(err))
	}
}

// refusalSentence is the sentence err carries past its sentinel's own Go text
// — `code: the caller's grant does not cover this: <sentence>` read as the
// sentence — wherever in the chain the sentinel was wrapped, since a refusal
// answered by another data node arrives wrapped by the estate router as well.
// A sentinel carrying no sentence of its own is answered with err's text whole.
func refusalSentence(err, sentinel error) string {
	if _, sentence, ok := strings.Cut(err.Error(), sentinel.Error()+": "); ok && sentence != "" {
		return sentence
	}
	return err.Error()
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
		codeNotServed(w, why)
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
	// host.Server.StartVerify).
	var floor statelog.Position
	if raw := strings.TrimSpace(r.URL.Query().Get("min_position")); raw != "" {
		at, err := statelog.ParsePosition(raw)
		if err != nil {
			httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeBadParams,
				map[string]string{"detail": "min_position: " + err.Error()})
			return
		}
		floor = at
	}
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
			map[string]string{"reason": reasonNotDeclared, "detail": notDeclared(name)})
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
		codeNotServed(w, why)
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
