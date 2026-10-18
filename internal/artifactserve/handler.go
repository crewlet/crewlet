package artifactserve

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/api/pagepolicy"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/artifactserve/ticket"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/objectives"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

var log = logging.Get("artifactserve")

// Routes are every route the artifact listener serves, exactly: one file of a
// ticket's version (the entry at the bare ticket path), one asset of a ticket's
// artifact, the embedded runtime and the embedded kit. GET answers HEAD too.
// Everything else is an empty 404: no API route, no probe, no redirect, no
// listing.
var Routes = []string{
	"GET /t/{ticket}/{path...}",
	"GET /t/{ticket}/_assets/{asset}",
	"GET /_crewlet/runtime/{contract}/{file...}",
	"GET /_crewlet/kit/{version}/{file...}",
}

// Artifacts is the artifacts estate as the origin reads it: through the estate
// router, so a node whose own copy is out of service or lagging decides through
// a data node whose copy answers. The router's method set, as narrow as this
// package uses it.
type Artifacts interface {
	Artifact(ctx context.Context, id artifacts.ID, fresh statelog.Freshness) (
		artifactsdomain.Head, statelog.Position, error)
	Version(ctx context.Context, id artifacts.ID, number int) (artifactsdomain.Version, error)
	Assets(ctx context.Context, id artifacts.ID) ([]artifactsdomain.Asset, error)
}

// Objects is the fleet's object store as the origin reads it: every read
// checked against the row ([objstore.Store.Open]).
type Objects interface {
	Open(ctx context.Context, o objstore.Object) (io.ReadCloser, error)
}

// Credentials answers who the credential a ticket names stands for NOW, from
// the identity rows alone (the origin never holds the credential itself):
// resolved, with the principal composed as the API's guard composes it; ended
// ([iam.Anonymous]: signed out, revoked, expired, suspended, or bound to a seat
// the chart no longer lets it act as); or [iam.Unknown], a node that cannot
// tell (an identity copy past the stall grace, a record it cannot read, a
// broker it cannot reach).
//
// CONSUMER-DEFINED here and implemented by internal/api/auth, which is where a
// principal is composed and nowhere else.
type Credentials interface {
	Principal(ctx context.Context, lineage string) (iam.Principal, iam.Resolution)
}

// LocalCopy reports whether this node's own artifacts copy answers right now,
// without waiting on the broker: what tells an authorization decided through a
// peer from one decided here, for the peer-check instrument.
type LocalCopy interface {
	ArtifactsCopyServes(ctx context.Context) bool
}

// Recorder is where the origin's measurements go: the engine's one recorder.
type Recorder interface {
	Add(name string, n uint64, attrs metrics.Attrs)
	Observe(name string, d time.Duration, attrs metrics.Attrs)
}

// unrecorded is the recorder of a handler handed none, which records nothing:
// the engine's own convention for a node built without a recorder (a test, an
// embedded engine), never the normal deployment.
type unrecorded struct{}

func (unrecorded) Add(string, uint64, metrics.Attrs)            {}
func (unrecorded) Observe(string, time.Duration, metrics.Attrs) {}

// Objectives is where the origin's service objective events go: the node's
// objective meter ([objectives.Meter]), which judges each against its bound.
type Objectives interface {
	Record(n objectives.Name, elapsed time.Duration, answered bool)
}

// unjudged is the objectives of a handler handed none, which count nothing.
type unjudged struct{}

func (unjudged) Record(objectives.Name, time.Duration, bool) {}

// Deps are the handler's dependencies, every one required but the recorder.
type Deps struct {
	Tickets     *ticket.Codec
	Origins     Origins
	Artifacts   Artifacts
	Objects     Objects
	Credentials Credentials
	Chart       authz.Chart
	Local       LocalCopy

	// Metrics is the engine's recorder. Nil records nothing, the engine's
	// own reading of a node built without one.
	Metrics Recorder

	// Objectives is the node's objective meter, which every document's
	// first byte is counted into ([objectives.ViewFirstByte]). Nil counts
	// nothing, for the recorder's reason.
	Objectives Objectives

	// Embedded is the binary's artifact tree: `runtime/<contract>/` and
	// `kit/<version>/`. A tree with neither directory serves both routes
	// as absent, which is a binary built before the runtime was.
	Embedded fs.FS

	// Now is the clock. Nil takes UTC wall time.
	Now func() time.Time
}

// Handler is the api.artifacts listener's whole handler.
type Handler struct {
	tickets  *ticket.Codec
	origins  Origins
	reads    Artifacts
	objects  Objects
	creds    Credentials
	chart    authz.Chart
	local    LocalCopy
	metrics  Recorder
	judged   Objectives
	now      func() time.Time
	verdicts *verdicts
	versions *lru[versionKey, artifactsdomain.Version]
	bodies   *lru[string, []byte]
	public   *publicFiles
}

// New builds the handler, or names the dependency it is missing.
func New(d Deps) (*Handler, error) {
	switch {
	case d.Tickets == nil:
		return nil, errors.New("artifactserve: the handler needs the ticket codec")
	case !d.Origins.mode.Valid():
		return nil, errors.New("artifactserve: the handler needs the artifact origin")
	case d.Artifacts == nil:
		return nil, errors.New("artifactserve: the handler needs the artifacts estate")
	case d.Objects == nil:
		return nil, errors.New("artifactserve: the handler needs the object store")
	case d.Credentials == nil:
		return nil, errors.New("artifactserve: the handler needs the credential resolver; " +
			"without it no document could be re-authorized")
	case d.Chart == nil:
		return nil, errors.New("artifactserve: the handler needs the company chart; " +
			"a unit share and the lead arm are decided against it")
	case d.Local == nil:
		return nil, errors.New("artifactserve: the handler needs this node's copy health")
	case d.Embedded == nil:
		return nil, errors.New("artifactserve: the handler needs the embedded artifact tree")
	}
	now := d.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	if d.Metrics == nil {
		d.Metrics = unrecorded{}
	}
	if d.Objectives == nil {
		d.Objectives = unjudged{}
	}
	return &Handler{
		tickets: d.Tickets, origins: d.Origins, reads: d.Artifacts, objects: d.Objects,
		creds: d.Credentials, chart: d.Chart, local: d.Local, metrics: d.Metrics,
		judged: d.Objectives, now: now,
		verdicts: newVerdicts(now),
		versions: newLRU[versionKey, artifactsdomain.Version](versionCacheBytes, versionSize),
		bodies:   newLRU[string, []byte](bodyCacheBytes, func(b []byte) int64 { return int64(len(b)) }),
		public:   newPublicFiles(d.Embedded),
	}, nil
}

// The destinations a request is counted under ([metrics.ArtifactsServeRequests]).
const (
	destDocument    = "document"
	destSubresource = "subresource"
	destRuntime     = "runtime"
	destKit         = "kit"
)

// ServeHTTP answers one request on the artifact listener.
//
// IT NEVER READS A COOKIE AND NEVER WRITES ONE. Nothing on this path asks for
// the Cookie header, and every response is built here from nothing a client's
// credential could change: a request carrying the dashboard's session cookie
// (a same-site deployment's browser sends one) is answered byte for byte as
// one carrying none.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := h.now()
	rec := &recording{ResponseWriter: w, now: h.now}
	dest := h.route(rec, r)
	status := rec.status
	if status == 0 {
		status = http.StatusOK
	}
	if dest == destDocument {
		// A DOCUMENT'S FIRST BYTE IS THE VIEW OBJECTIVE'S EVENT, under
		// every status but a 5xx: a refusal the viewer caused (an expired
		// ticket, a revoked share) is an answer, and a stale node's 503 is
		// the failure the objective exists to count. A handler that wrote
		// nothing answered at its return.
		first := rec.first
		if first.IsZero() {
			first = h.now()
		}
		h.judged.Record(objectives.ViewFirstByte, first.Sub(start), status < http.StatusInternalServerError)
	}
	h.metrics.Add(metrics.ArtifactsServeRequests, 1, metrics.Attrs{
		"dest": dest, "status": strconv.Itoa(status/100) + "xx"})
	h.metrics.Observe(metrics.ArtifactsServeDuration, h.now().Sub(start),
		metrics.Attrs{"dest": dest})
	if rec.bytes > 0 {
		h.metrics.Add(metrics.ArtifactsServeBytes, uint64(rec.bytes), nil)
	}
}

// route dispatches one request and answers the destination it was counted as.
func (h *Handler) route(w http.ResponseWriter, r *http.Request) string {
	p := r.URL.Path
	dest := destSubresource
	switch {
	case strings.HasPrefix(p, "/_crewlet/runtime/"):
		dest = destRuntime
	case strings.HasPrefix(p, "/_crewlet/kit/"):
		dest = destKit
	case fetchDest(r) == "iframe" || fetchDest(r) == "document":
		dest = destDocument
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		h.refuse(w, http.StatusMethodNotAllowed, 0)
		return dest
	}
	if dest == destRuntime || dest == destKit {
		if !h.public.serve(w, r, dest, strings.TrimPrefix(p, "/_crewlet/"+dest+"/"),
			h.origins.Secure()) {
			h.refuse(w, http.StatusNotFound, 0)
		}
		return dest
	}
	rest, ok := strings.CutPrefix(p, ticketPath)
	if !ok {
		h.refuse(w, http.StatusNotFound, 0)
		return dest
	}
	token, file, ok := strings.Cut(rest, "/")
	if !ok || token == "" {
		h.refuse(w, http.StatusNotFound, 0)
		return dest
	}
	served := h.ticketed(w, r, token, file)
	if served != "" {
		return served
	}
	return dest
}

// fetchDest is the request's Sec-Fetch-Dest, lower case, or empty for a
// browser that sends no Fetch Metadata.
func fetchDest(r *http.Request) string {
	return strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Dest")))
}

// refuse answers an empty refusal: no body, no hint whether the artifact
// exists, inert headers, never cached. retry is a 503's Retry-After, zero for
// none.
//
// EMPTY FOR EVERY STATUS. A JSON error here would be a document an artifact
// page could read about other artifacts, and the origin serves bytes and
// nothing else.
func (h *Handler) refuse(w http.ResponseWriter, status int, retry time.Duration) {
	header := w.Header()
	for k := range header {
		if k != "Allow" {
			header.Del(k)
		}
	}
	pagepolicy.ArtifactSubresource(header, h.origins.refusalPolicy())
	header.Set("Cache-Control", "no-store")
	if retry > 0 {
		header.Set("Retry-After", strconv.Itoa(int((retry+time.Second-1)/time.Second)))
	}
	header.Set("Content-Length", "0")
	w.WriteHeader(status)
}

// recording is the response writer the handler writes through: it remembers
// the status, when the response was committed to and the body's size for the
// instruments, and passes deadlines and flushes through to the connection.
type recording struct {
	http.ResponseWriter
	now    func() time.Time
	status int
	bytes  int64

	// first is when the status was set (a WriteHeader, or the first Write
	// that implies one): the instant the handler committed to its answer,
	// which is the first byte as this process can see it.
	first time.Time
}

func (r *recording) WriteHeader(status int) {
	if r.status == 0 {
		r.status, r.first = status, r.now()
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *recording) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status, r.first = http.StatusOK, r.now()
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

// Unwrap is what lets http.ResponseController reach the connection's
// deadlines through this writer.
func (r *recording) Unwrap() http.ResponseWriter { return r.ResponseWriter }
