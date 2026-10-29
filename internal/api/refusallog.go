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
// `config_write_refused`: the route's pattern, the method, the status, the
// problem's code, who asked, the revision they wrote against and the request
// id.
//
// NEVER WHAT WAS WRITTEN. The route is the PATTERN the request was served on
// (`PUT /secrets/{name}`), never the path, which would carry a secret's name;
// the problem is the envelope's code, read off the answer, never its body or
// its detail, which can quote a document; and nothing of the request's body
// is read at all.
//
// AT INFO, NOT WARN: a refusal is the engine doing its job, answering a
// request it was right not to take, and a stale tab or an expired step-up
// refuses routinely. A warning is for something an operator should act on
// unprompted; this is the record they look for when somebody asks.

// refusalMux is the mux a guarded surface mounts on, wrapping each route so
// that a write it answers with a 4xx logs to log. It forwards the policy each route
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

// Handle mounts h wrapped, whatever its pattern: a surface may mount a
// prefix with no method (configapi mounts `/config` and `/config/` over a
// sub-mux of its own), so whether a request is a write is decided when it
// arrives, by its method, never by the pattern it was mounted on.
func (m refusalMux) Handle(pattern string, h http.Handler) {
	m.Mux.Handle(pattern, m.logRefusals(pattern, h))
}

// isWrite reports whether method writes.
func isWrite(method string) bool {
	switch method {
	case http.MethodPut, http.MethodPost, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

// requestIDHeader names the request: the caller's or a proxy's where it sent
// one in the shape this accepts, a UUID minted here where not, and answered
// back, so a refusal the caller reports can be found in the log by it.
const requestIDHeader = "X-Request-Id"

func (m refusalMux) logRefusals(pattern string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isWrite(r.Method) {
			h.ServeHTTP(w, r)
			return
		}
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
			"route", routeOf(r, pattern), "method", r.Method, "status", watched.status, "code", envelope.Error,
			"by", by.Name, "kind", string(by.Kind), "operator", by.OperatorID,
			"base_revision", baseRevision(r.Header.Get("If-Match")),
			"dry_run", r.URL.Query().Get("dry_run") == "true", "request_id", id)
	})
}

// routeOf is the pattern r was served on: the innermost mux's, which a
// [http.ServeMux] beneath the mount sets on this same request as it routes
// it (configapi's sub-mux names `PUT /config/{kind}/{id}` where the mount
// says only `/config/`), and the mount's own where nothing beneath set one.
// Both are patterns, never a path.
func routeOf(r *http.Request, mounted string) string {
	if r.Pattern != "" {
		return r.Pattern
	}
	return mounted
}

// invalidRevision is the base_revision of an If-Match that is not one
// revision tag: never a tag, which cannot hold `<`.
const invalidRevision = "<invalid>"

// baseRevision is the revision ifMatch names, as the log may carry it: empty
// where none was sent, `*` for whatever the target holds, the tag without its
// quotes (or a weak tag's `W/`) where it has a revision tag's shape, and
// [invalidRevision] for anything else. NEVER THE HEADER AS SENT: it is the
// caller's text, and a header of any length or content would otherwise be
// written into the log verbatim. Every tag this engine answers is a UUID or
// a kind and a hex digest, so a tag is at most 128 bytes of letters,
// digits, `.`, `_`, `:` and `-`; a list of several is not one revision.
func baseRevision(ifMatch string) string {
	ifMatch = strings.TrimSpace(ifMatch)
	switch {
	case ifMatch == "":
		return ""
	case ifMatch == "*":
		return "*"
	}
	tag := strings.TrimPrefix(ifMatch, "W/")
	if len(tag) >= 2 && tag[0] == '"' && tag[len(tag)-1] == '"' {
		tag = tag[1 : len(tag)-1]
	}
	if !isToken(tag, "._:-") {
		return invalidRevision
	}
	return tag
}

// isToken reports whether s is 1 to 128 bytes of ASCII letters, digits and
// the punctuation in extra.
func isToken(s, extra string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for _, c := range []byte(s) {
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		case strings.IndexByte(extra, c) >= 0:
		default:
			return false
		}
	}
	return true
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
