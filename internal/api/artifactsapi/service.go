package artifactsapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/canvasformat"
	"github.com/crewlet/crewlet/internal/artifacts/docindex"
	"github.com/crewlet/crewlet/internal/artifacts/slideformat"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/artifactserve"
	"github.com/crewlet/crewlet/internal/artifactupload"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/objectives"
	"github.com/crewlet/crewlet/internal/objstore"
)

// Reads is the artifacts estate's read side the routes ask: the estate
// router's, so a node whose own copy is out of service answers from a peer's.
type Reads interface {
	Heads
	Version(ctx context.Context, id artifacts.ID, number int) (artifactsdomain.Version, error)
}

// Writes is the artifacts estate's write side the routes that need no bytes
// call: the estate router's, so a node without `data` writes through one that
// holds it.
type Writes interface {
	Patch(ctx context.Context, c artifactsdomain.Call, id artifacts.ID, p artifactsdomain.Patch) (
		artifactsdomain.Written, error)
	Share(ctx context.Context, c artifactsdomain.Call, id artifacts.ID, s artifacts.Share) (
		artifactsdomain.Written, error)
	Unshare(ctx context.Context, c artifactsdomain.Call, id artifacts.ID, a artifacts.Audience) (
		artifactsdomain.Written, error)
	Transfer(ctx context.Context, c artifactsdomain.Call, id artifacts.ID, toSeat string) (
		artifactsdomain.Written, error)
	Trash(ctx context.Context, c artifactsdomain.Call, id artifacts.ID) (artifactsdomain.Written, error)
	Restore(ctx context.Context, c artifactsdomain.Call, id artifacts.ID) (artifactsdomain.Written, error)
	Purge(ctx context.Context, c artifactsdomain.Call, id artifacts.ID) (artifactsdomain.Written, error)
	RemoveAsset(ctx context.Context, c artifactsdomain.Call, id artifacts.ID, asset uuid.UUID) (
		artifactsdomain.Written, error)
	Pin(ctx context.Context, c artifactsdomain.Call, id artifacts.ID, pinned bool) (
		artifactsdomain.Written, error)
}

// Pipeline is the upload and publish pipeline (internal/artifactupload): every
// byte a version or an asset is made of passes through it, so the policy a
// publish is held to is the same whichever surface published.
type Pipeline interface {
	Intend(ctx context.Context, c artifactsdomain.Call, id artifacts.ID, req artifacts.UploadRequest) (
		artifacts.UploadIntent, error)
	Transfer(ctx context.Context, c artifactsdomain.Call, upload, path string, body io.Reader, size int64) error
	Lookup(ctx context.Context, c artifactsdomain.Call, upload string) (artifactupload.Upload, error)
	Create(ctx context.Context, c artifactsdomain.Call, req artifactupload.CreateRequest) (
		artifactsdomain.Published, artifactupload.Report, error)
	Publish(ctx context.Context, c artifactsdomain.Call, req artifactupload.PublishRequest) (
		artifactsdomain.Published, artifactupload.Report, error)
	AddAsset(ctx context.Context, c artifactsdomain.Call, id artifacts.ID, upload, name string) (
		artifactsdomain.Asset, error)
}

// Objects is the object store a version's bytes are read back through, each
// checked against the digest its manifest names ([objstore.Store.Open]).
type Objects interface {
	Open(ctx context.Context, o objstore.Object) (io.ReadCloser, error)
}

// Framer mints the frame the dashboard embeds a version in, on the artifact
// origin ([artifactserve.Frames]), and reads a minted frame's ticket back from
// its URL for the viewer's ready report.
type Framer interface {
	For(r artifactserve.FrameRequest) (artifacts.Frame, error)
	Claims(frameURL string) (artifacts.TicketClaims, error)
}

// Beacon admits the viewers' ready reports into the node's service objectives
// ([objectives.Beacon]).
type Beacon interface {
	Accept(r objectives.ReadyReport) error
}

// Options configure the surface.
type Options struct {
	// Gate is the one admission every route takes. REQUIRED, with its
	// Heads, Chart, Limits, Marks and Roster.
	Gate Gate

	// Reads and Writes are the artifacts estate. REQUIRED.
	Reads  Reads
	Writes Writes

	// Uploads is the node's upload and publish pipeline, asked for per
	// request: the engine builds it on first use, and a node that cannot
	// (no object store, no coordination store) answers its routes
	// unavailable with the reason rather than refusing to boot. REQUIRED.
	Uploads func() (Pipeline, error)

	// Objects reads a version's bytes for the download and the source
	// routes. REQUIRED.
	Objects Objects

	// Frames mints view frames. NIL IS A DEPLOYMENT WITH NO ARTIFACT ORIGIN
	// (`api.artifacts.external_url` unset): a frame is refused naming the
	// key an operator sets, and everything else is served.
	Frames Framer

	// Ready admits the frames' ready reports (`POST /artifacts/ready`) into
	// the node's service objectives. REQUIRED: a report this node could not
	// count would be an outage the one indicator that sees it never heard
	// of.
	Ready Beacon

	// PaceClock is the clock a streamed body's pace is measured on; nil is
	// the wall clock. See internal/api/paced.
	PaceClock func() time.Time
}

// Service is the surface.
type Service struct {
	gate    Gate
	reads   Reads
	writes  Writes
	uploads func() (Pipeline, error)
	objects Objects
	frames  Framer
	ready   Beacon
	pace    func() time.Time

	// decks, docs and canvases are what this node read of each version for
	// the dashboard, through the kind's format.
	decks    *formatCache[slideformat.Deck]
	docs     *formatCache[docindex.Doc]
	canvases *formatCache[canvasformat.Canvas]
}

// New builds the surface.
func New(opts Options) (*Service, error) {
	if err := opts.Gate.validate(); err != nil {
		return nil, err
	}
	switch {
	case opts.Reads == nil:
		return nil, errors.New("artifactsapi: no artifacts reader")
	case opts.Writes == nil:
		return nil, errors.New("artifactsapi: no artifacts writer")
	case opts.Uploads == nil:
		return nil, errors.New("artifactsapi: no upload pipeline: every byte of a " +
			"version passes through it")
	case opts.Objects == nil:
		return nil, errors.New("artifactsapi: no object store: a version's files are " +
			"read back through it")
	case opts.Ready == nil:
		return nil, errors.New("artifactsapi: no ready beacon: a frame's ready report is " +
			"counted into the node's service objectives through it")
	}
	pace := opts.PaceClock
	if pace == nil {
		pace = time.Now
	}
	return &Service{gate: opts.Gate, reads: opts.Reads, writes: opts.Writes, uploads: opts.Uploads,
		objects: opts.Objects, frames: opts.Frames, ready: opts.Ready, pace: pace,
		decks: newDeckCache(opts.Objects), docs: newDocCache(opts.Objects),
		canvases: newCanvasCache(opts.Objects)}, nil
}

// Gate is the surface's admission, which the read questions take too.
func (s *Service) Gate() Gate { return s.gate }

// The patterns. Named, because the transfer route's shape is held to the path
// the pipeline hands a client ([artifactupload.TransferPath]).
const (
	patternTransfer = "PUT /artifacts/uploads/{upload}/files/{path...}"
	patternFiles    = "GET /artifacts/{id}/versions/{n}/files/{path...}"
)

// Routes mounts the surface.
//
// EVERY ROUTE STATES ITS AUTHORITY HERE. A verb the table decides without the
// artifact (creating one, which asks the artifacts app; a purge, which is
// admin's, with a fresh proof of identity and a person present) is the route's
// own policy. A verb that needs the artifact's owner and shares, which only its
// head can say, is mounted on its weakest honest precondition, that the caller
// resolved and may act ([authz.ActionArtifactList]), and the handler reads the
// head and decides the verb on it through the one [Gate] before anything else
// is read or written. A caller with no relation is answered exactly as an
// unknown artifact.
func (s *Service) Routes(mux authz.Mux) error {
	router := authz.NewRouter(mux, authz.ContextGuard(s.gate.Chart))
	pre := authz.Policy{Action: authz.ActionArtifactList, Object: func(*http.Request) authz.Object {
		return authz.Object{Kind: authz.KindArtifact}
	}}
	create := authz.Policy{Action: authz.ActionArtifactCreate, Object: func(*http.Request) authz.Object {
		return authz.Object{Kind: authz.KindArtifact}
	}}
	purge := authz.Policy{Action: authz.ActionArtifactPurge, Object: func(*http.Request) authz.Object {
		return authz.Object{Kind: authz.KindArtifact}
	}}
	ready := authz.Policy{Action: authz.ActionArtifactReady, Object: func(*http.Request) authz.Object {
		return authz.Object{Kind: authz.KindArtifact}
	}}
	var failures []error
	for _, route := range []struct {
		pattern string
		policy  authz.Policy
		handler http.HandlerFunc
	}{
		{"POST /artifacts/uploads", create, s.intendCreate},
		{"POST /artifacts/{id}/uploads", pre, s.intendPublish},
		{patternTransfer, pre, s.transfer},
		{"POST /artifacts", create, s.create},
		{"POST /artifacts/{id}/versions", pre, s.publish},
		{"POST /artifacts/{id}/versions/{n}/restore", pre, s.restoreVersion},
		{"PATCH /artifacts/{id}", pre, s.patch},
		{"PATCH /artifacts/{id}/versions/{n}", pre, s.label},
		{"PUT /artifacts/{id}/shares", pre, s.share},
		{"DELETE /artifacts/{id}/shares/{audience...}", pre, s.unshare},
		{"POST /artifacts/{id}/transfer", pre, s.transferOwnership},
		{"DELETE /artifacts/{id}", pre, s.trash},
		{"POST /artifacts/{id}/restore", pre, s.restore},
		{"POST /artifacts/{id}/purge", purge, s.purge},
		{"POST /artifacts/{id}/assets", pre, s.addAsset},
		{"DELETE /artifacts/{id}/assets/{asset}", pre, s.removeAsset},
		{"PUT /artifacts/{id}/pin", pre, s.pin},
		{"DELETE /artifacts/{id}/pin", pre, s.unpin},
		{"POST /artifacts/{id}/tickets", pre, s.ticket},
		{patternFiles, pre, s.file},
		{"GET /artifacts/{id}/versions/{n}/source", pre, s.source},
		{"GET /artifacts/{id}/versions/{n}/deck", pre, s.deck},
		{"GET /artifacts/{id}/versions/{n}/doc", pre, s.doc},
		{"GET /artifacts/{id}/versions/{n}/outline", pre, s.outline},
		{"POST /artifacts/ready", ready, s.reportReady},
	} {
		if err := router.Handle(route.pattern, route.policy, route.handler); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// pipeline is the node's upload pipeline, or the refusal of a node that cannot
// build one, rendered.
func (s *Service) pipeline(w http.ResponseWriter, r *http.Request) (Pipeline, bool) {
	p, err := s.uploads()
	if err != nil {
		log.WarnContext(r.Context(), "artifacts_uploads_unavailable", "error", err.Error())
		unavailable(w, err, "this node takes no artifact uploads: "+err.Error())
		return nil, false
	}
	return p, true
}

// pathID is the artifact the path names, refusing a path naming none as an
// unknown artifact.
func pathID(w http.ResponseWriter, r *http.Request) (artifacts.ID, bool) {
	id, err := artifacts.ParseID(r.PathValue("id"))
	if err != nil {
		notFound(w)
		return artifacts.ID{}, false
	}
	return id, true
}

// pathVersion is the version number the path names.
func pathVersion(w http.ResponseWriter, r *http.Request) (int, bool) {
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || n < 1 || strconv.Itoa(n) != r.PathValue("n") {
		// A PATH NAMING NO VERSION NUMBER names no version.
		notFound(w)
		return 0, false
	}
	return n, true
}

// workItemField bounds the work item a write names: a tracker key, which
// confers nothing and is kept for the link alone.
const maxWorkItemBytes = 64

// checkWorkItem refuses a work item that could not be a tracker key: one
// longer than any key, or one carrying a byte outside visible ASCII.
func checkWorkItem(item string) (string, error) {
	item = strings.TrimSpace(item)
	if len(item) > maxWorkItemBytes {
		return "", errors.New("work_item is a tracker key, at most 64 bytes")
	}
	for i := 0; i < len(item); i++ {
		if c := item[i]; c <= ' ' || c > '~' {
			return "", errors.New("work_item is a tracker key: visible ASCII, no space")
		}
	}
	return item, nil
}
