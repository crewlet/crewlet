package api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"unicode"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/authz"
)

// A REFUSED WRITE TO THE COMPANY LEAVES ONE LOG LINE. A write to the config,
// the secrets or a model that the engine answers with a 4xx (a stale
// If-Match, a validation problem, a missing step-up, a grant the caller does
// not hold, a guard refusing it before the route ran) used to leave nothing in
// the engine's log, so an operator asking why a save "did nothing" had the
// dashboard's sentence and no record of it. Each now logs
// `config_write_refused`: the route as mounted, the method, the status, the
// problem's code, who asked, the revision they wrote against and the request
// id.
//
// NEVER WHAT WAS WRITTEN. The route is the mounted PATTERN
// (`PUT /secrets/{name}`), never the path, which would carry a secret's name;
// the problem is the envelope's code, read off the answer, never its body or
// its detail, which can quote a document; and nothing of the request's body
// is read at all.
//
// AT INFO, NOT WARN: a refusal is the engine doing its job, answering a
// request it was right not to take, and a stale tab or an expired step-up
// refuses routinely. A warning is for something an operator should act on
// unprompted; this is the record they look for when somebody asks.

// refusalMux is the mux a guarded surface mounts on, wrapping each write
// route so that a 4xx answer logs to log. It forwards the policy each route
// is mounted with ([authz.PolicyMux]) to the mux beneath, and every guard's
// refusal passes through it, since the wrap is outside the route's [authz.Router].
type refusalMux struct {
	authz.Mux
	log *slog.Logger
}

// MountedWith passes the route's policy on, where the mux beneath keeps it.
func (m refusalMux) MountedWith(pattern string, p authz.Policy) {
	if keeper, ok := m.Mux.(authz.PolicyMux); ok {
		keeper.MountedWith(pattern, p)
	}
}

// Handle mounts h, wrapped when pattern is a write.
func (m refusalMux) Handle(pattern string, h http.Handler) {
	method, _, _ := strings.Cut(pattern, " ")
	switch method {
	case http.MethodPut, http.MethodPost, http.MethodPatch, http.MethodDelete:
		m.Mux.Handle(pattern, m.logRefusals(pattern, h))
	default:
		m.Mux.Handle(pattern, h)
	}
}

// requestIDHeader names the request: the caller's or a proxy's where it sent
// one in the shape this accepts, a UUID minted here where not, and answered
// back, so a refusal the caller reports can be found in the log by it.
const requestIDHeader = "X-Request-Id"

func (m refusalMux) logRefusals(pattern string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := requestID(r.Header.Get(requestIDHeader))
		w.Header().Set(requestIDHeader, id)
		watched := &refusalWatcher{ResponseWriter: w}
		h.ServeHTTP(watched, r)
		if watched.status < 400 || watched.status >= 500 {
			return
		}
		var envelope struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(watched.head.Bytes(), &envelope)
		by := auth.AttributionOf(r.Context())
		m.log.InfoContext(r.Context(), "config_write_refused",
			"route", pattern, "method", r.Method, "status", watched.status, "code", envelope.Error,
			"by", by.Name, "kind", string(by.Kind), "operator", by.OperatorID,
			"base_revision", strings.Trim(strings.TrimSpace(r.Header.Get("If-Match")), `"`),
			"dry_run", r.URL.Query().Get("dry_run") == "true", "request_id", id)
	})
}

// requestID is the request's id: sent, where it is one printable token of at
// most 128 bytes, or minted.
func requestID(sent string) string {
	if sent != "" && len(sent) <= 128 && strings.IndexFunc(sent, func(r rune) bool {
		return r > unicode.MaxASCII || !unicode.IsPrint(r) || unicode.IsSpace(r)
	}) < 0 {
		return sent
	}
	return uuid.NewString()
}

// refusalWatcher is a ResponseWriter that keeps the status written and the
// head of an error answer, where its envelope's code is.
type refusalWatcher struct {
	http.ResponseWriter
	status int
	head   bytes.Buffer
}

func (w *refusalWatcher) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *refusalWatcher) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.status >= 400 && w.head.Len() < 64<<10 {
		w.head.Write(b)
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap lets [http.ResponseController] reach the writer beneath.
func (w *refusalWatcher) Unwrap() http.ResponseWriter { return w.ResponseWriter }
