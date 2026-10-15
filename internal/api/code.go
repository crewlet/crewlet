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
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/host"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	seatplacement "github.com/crewlet/crewlet/internal/seat/placement"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE ENGINE'S OWN GIT HOST, ON THE API: smart HTTP under /git/ for a person's
// git client, and the code host's REST reads under /code/.
//
// Both prefixes are ALWAYS GUARDED ([auth.GuardedPrefixes]) and neither is ever
// on api.public ([auth.Public] names neither): a repository is the company's
// source, and a person reaches it with their own bound token — the third person
// transport beside the act route and the operator's MCP (ADR-0037). On a git
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
// ([CodeSurface]), for [NativeRoutes]' reason: the code host and its reads are
// built with the native runtime, at boot or at the first apply whose company
// sets code.native — on a node that booted with no company, after the app was
// assembled. A route that finds nothing answers WHY, as the engine decides it
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

// The reasons a 503 from a code route names, each the log line or the state
// an operator finds it under, so a script or a balancer can act on it rather
// than parse the sentence.
const (
	reasonApplying     = "applying"
	reasonApplyFailed  = "config_apply_failed"
	reasonNeedsRestart = "native_backend_needs_restart"
)

// reasonNotHeld is what a verify's 404 names when the repository is declared
// and THIS node holds no copy of it — a different fact from a repository
// nobody declared, which a client acts on the opposite way: a sweep of every
// repository on one git node passes over the ones that node does not keep
// (nothing there can be damaged) and fails on a name nobody declared. Both
// are `not_found`, since neither has anything here to check; the reason is
// what tells them apart without reading the sentence.
const reasonNotHeld = "not_held"

// notDeclared is the sentence a repository nobody declared is answered with,
// naming the setting that declares one.
func notDeclared(name string) string {
	return fmt.Sprintf("no repository named %q is declared in code.repos", name)
}

// codeUnserved is how a code route answers a node that serves none of the
// code host now, by the engine's reason:
//
//   - not hosted: 404, [notHosted];
//   - applying: 503 with Retry-After [codeApplyingRetryAfter] — the node is
//     applying the configuration the fleet activated and answers once that
//     apply ends. It says nothing of what that configuration holds: an apply
//     is in flight before it has decoded the revision too, and a company
//     that never set code.native must not be told the code host is coming;
//   - apply failed: 503 and no Retry-After — the node's apply of a
//     configuration that hosts code failed (config_apply_failed) and nothing
//     it waits for clears that; another node serves it;
//   - unknown: the same 503 under the same reason, saying instead that the
//     node could not read the configuration the fleet activated and so cannot
//     say whether the company hosts code — the apply failed before the
//     revision was decoded, and a sentence that it hosts code would state
//     what the node never read;
//   - needs restart: 503 and no Retry-After — the node's native runtime was
//     running before code.native was set (native_backend_needs_restart), and
//     only its restart clears it; another node serves it.
//
// Any other error breaks the seam's contract: 500, logged.
func codeUnserved(err error) (status int, retryAfter time.Duration, reason, detail string) {
	switch {
	case errors.Is(err, engine.ErrCodeNotHosted):
		return http.StatusNotFound, 0, "", notHosted
	case errors.Is(err, engine.ErrCodeApplying):
		return http.StatusServiceUnavailable, codeApplyingRetryAfter, reasonApplying,
			"this node is applying the configuration the fleet activated and answers once that apply " +
				"ends: retry in " + strconv.Itoa(int(codeApplyingRetryAfter.Seconds())) +
				" seconds, or against another node"
	case errors.Is(err, engine.ErrCodeApplyFailed):
		return http.StatusServiceUnavailable, 0, reasonApplyFailed,
			"this node could not apply the configuration the fleet activated, which hosts code " +
				"(config_apply_failed: GET /fleet and this node's log say why): ask another node, or " +
				"activate a corrected configuration"
	case errors.Is(err, engine.ErrCodeUnknown):
		return http.StatusServiceUnavailable, 0, reasonApplyFailed,
			"this node could not read the configuration the fleet activated, so it cannot say whether " +
				"this company hosts code (config_apply_failed: GET /fleet and this node's log say why): " +
				"ask another node"
	case errors.Is(err, engine.ErrCodeNeedsRestart):
		return http.StatusServiceUnavailable, 0, reasonNeedsRestart,
			"this node's native runtime was running before code.native was set, and it serves the code " +
				"host once restarted (native_backend_needs_restart): ask another node, or restart this one"
	}
	log.Warn("api_code_unserved_unexplained", "error", err)
	return http.StatusInternalServerError, 0, "", ""
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
// are mounted where.
//
// The git patterns are the host's own constants, so the path a handler reads
// its repository from is the path it was mounted at, and this file spells no
// git path a classifier gate would have to allow.
func (a *App) mountCode(mux httpjson.Router) {
	if a.servesGit {
		mux.Handle(host.PatternInfoRefs, a.gitRoute(GitRoutes.InfoRefs))
		mux.Handle(host.PatternUploadPack, a.gitRoute(GitRoutes.UploadPack))
		mux.Handle(host.PatternReceivePack, a.gitRoute(GitRoutes.ReceivePack))
		// A WRITE TO THE DRAIN GATE, refused while draining like every
		// POST: a check is new work on the node, a repair a rebuild. Only
		// where git is served: it checks THIS node's cache, which a node
		// without the role does not keep — and so is the read of one, which
		// a draining node still answers.
		mux.HandleFunc("POST /code/repos/{name}/verify", a.serveCodeVerify)
		mux.HandleFunc("GET /code/repos/{name}/verify/{id}", a.serveCodeVerification)
	}
	mux.Handle("GET /code/repos", a.serveCodeRead(code.ReadRepos, "code_repos"))
	mux.Handle("GET /code/repos/{name}", a.serveCodeRead(code.ReadRepo, "code_repo"))
	mux.Handle("GET /code/repos/{name}/pushes", a.serveCodeRead(code.ReadPushes, "code_pushes"))
}

// gitRoute serves one of the host's git handlers, resolved for this request,
// or answers in git's own form why this node serves none now.
func (a *App) gitRoute(pick func(GitRoutes) http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g, why := a.code.GitRoutes()
		if why != nil {
			status, after, reason, detail := codeUnserved(why)
			if after > 0 {
				w.Header().Set("Retry-After", strconv.Itoa(int(after.Seconds())))
			}
			if status == http.StatusInternalServerError {
				detail = "this node could not say why it serves no code host; its log says what it was told"
			}
			if reason != "" {
				detail += " (" + reason + ")"
			}
			auth.GitText(w, status, detail)
			return
		}
		pick(g).ServeHTTP(w, r)
	})
}

// codeNotServed answers a /code/ route this node serves none of the code host
// for now, in the read surface's vocabulary, with why ([codeUnserved]).
func codeNotServed(w http.ResponseWriter, why error) {
	status, after, reason, detail := codeUnserved(why)
	switch status {
	case http.StatusNotFound:
		writeJSON(w, status, map[string]string{"error": stream.CodeNotFound, "detail": detail})
	case http.StatusServiceUnavailable:
		if after > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int(after.Seconds())))
		}
		writeJSON(w, status, map[string]string{"error": stream.CodeUnavailable, "reason": reason, "detail": detail})
	default:
		writeJSON(w, status, map[string]string{"error": stream.CodeQueryFailed})
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
		q, err := codeReadQuery(kind, r)
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

// codeReadQuery is a read's request, refused [queries.ErrBadParams] naming the
// parameter that is not one this read takes.
func codeReadQuery(kind code.ReadKind, r *http.Request) (code.ReadQuery, error) {
	p := queries.FromQuery(r.URL.Query())
	fresh, err := queries.Freshness(p)
	if err != nil {
		return code.ReadQuery{}, err
	}
	q := code.ReadQuery{Kind: kind, Repo: r.PathValue("name"), Cursor: p.String("cursor"), Fresh: fresh}
	for v := range strings.SplitSeq(p.String("status"), ",") {
		if v = strings.TrimSpace(v); v == "" {
			continue
		}
		status := code.RepoStatus(v)
		if !status.Valid() {
			return code.ReadQuery{}, fmt.Errorf("%w: status: %q is not one of %v", queries.ErrBadParams,
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
			return code.ReadQuery{}, fmt.Errorf("%w: limit: %q is not a whole number of zero or more "+
				"(0 is the read's own page size)", queries.ErrBadParams, p.String("limit"))
		}
		q.Limit = int(n)
	}
	return q, nil
}

// codeRefusal answers a code read's failure in the read surface's ONE
// vocabulary ([writeQueryError]): a repository that is not declared is
// `not_found`, naming it; a level, a cursor or a parameter the reader refuses
// is `bad_params` with the reader's own sentence; and anything that clears by
// waiting — a read refusal that is retryable, no data node serving — is
// `unavailable` with its own Retry-After, by [queries.ClassifyUnavailable], the
// registry's own classification rather than a second list here.
func codeRefusal(w http.ResponseWriter, what, repo string, err error) {
	switch {
	case errors.Is(err, code.ErrRepoNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": stream.CodeNotFound, "detail": notDeclared(repo),
		})
	case errors.Is(err, code.ErrBadLevel), errors.Is(err, code.ErrBadCursor),
		errors.Is(err, code.ErrInvalid):
		writeQueryError(w, what, fmt.Errorf("%w: %w", queries.ErrBadParams, err))
	default:
		writeQueryError(w, what, queries.ClassifyUnavailable(err))
	}
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
			errors.Is(queries.ClassifyUnavailable(v.Err), queries.ErrUnavailable) {
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
// against its rows — and with `?repair=true` a rebuild of what it finds
// damaged. The check reads every byte of the repository, which can outlive a
// balancer's read timeout, so it runs on the node's own lifetime rather than
// the request's (host.Server.StartVerify): the request waits for it up to
// host.VerifyHold and answers `200` with the verification ended — a small
// copy's check is answered where it was asked — or `202 Accepted` with it
// running, its address in `Location`, where its caller reads the outcome
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
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": stream.CodeBadParams, "detail": "repair is true or false",
		})
		return
	}
	// THE CALLER'S FLOOR, where it names one: a position on the code log it
	// read at, which the copy is brought to with no barrier (see
	// host.Server.StartVerify).
	var floor statelog.Position
	if raw := strings.TrimSpace(r.URL.Query().Get("min_position")); raw != "" {
		at, err := statelog.ParsePosition(raw)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": stream.CodeBadParams, "detail": "min_position: " + err.Error(),
			})
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
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": stream.CodeNotFound, "detail": notDeclared(name),
		})
	case errors.Is(err, code.ErrInvalid):
		// A floor on another log than the repositories' rows.
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": stream.CodeBadParams, "detail": err.Error(),
		})
	case errors.Is(err, cache.ErrNotHeld):
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": stream.CodeNotFound, "reason": reasonNotHeld,
			"detail": fmt.Sprintf("this node holds no copy of %q, so it has "+
				"nothing to check: verify the git nodes that serve it", name),
		})
	case errors.As(err, &running):
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "verify_running", "detail": err.Error(), "running": viewOf(running.Running),
		})
	case errors.Is(err, host.ErrStopping):
		// THE STOP'S OWN WAIT, the one every git request a stopping host
		// refuses is told: what takes the node out of rotation.
		w.Header().Set("Retry-After", strconv.Itoa(int(host.StoppingRetryAfter.Seconds())))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": stream.CodeUnavailable, "detail": "this node's code host is stopping: verify against " +
				"another node's cache, or this one's once it has restarted",
		})
	case errors.Is(err, gitproc.ErrBusy):
		// REFUSED BEFORE IT READ ANYTHING — the check takes its slot and
		// its memory first, never waits for either — so the retry costs
		// what this refusal did: nothing. At the wait a busy git request on
		// this node is told.
		w.Header().Set("Retry-After", strconv.Itoa(int(host.BusyRetryAfter.Seconds())))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": stream.CodeUnavailable, "detail": err.Error(),
		})
	case errors.Is(queries.ClassifyUnavailable(err), queries.ErrUnavailable):
		// What clears by waiting — the code log unreadable here, no data
		// node serving the resolve — with the refusal's own Retry-After.
		writeQueryError(w, "code_verify", queries.ClassifyUnavailable(err))
	default:
		// The sentence too, for viewOf's reason: a node whose memory
		// cannot hold the check names the node to run it on.
		log.Warn("api_code_verify_refused", "repo", name, "repair", repair, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": stream.CodeQueryFailed, "detail": err.Error(),
		})
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
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": stream.CodeNotFound, "detail": fmt.Sprintf("this node keeps no verify %s of %q: one is "+
				"kept for an hour after it ends, until the repository's next verify begins, and never across "+
				"a restart", r.PathValue("id"), name),
		})
		return
	}
	writeJSON(w, http.StatusOK, viewOf(v))
}
