package host

import (
	"context"
	"net/http"

	"github.com/crewlet/crewlet/internal/api/auth"
)

// The route patterns the API mounts and the suite's mux — ONE spelling, so the
// path a handler reads its repository from is the path it was mounted at.
const (
	// PathRepo is the wildcard the repository's segment is read from.
	PathRepo = "repo"
	// PatternInfoRefs is the advertisement; a GET pattern also matches HEAD.
	PatternInfoRefs = "GET /git/{repo}/info/refs"
	// PatternUploadPack is a fetch's RPC.
	PatternUploadPack = "POST /git/{repo}" + UploadPackSuffix
	// PatternReceivePack is a push's RPC.
	PatternReceivePack = "POST /git/{repo}" + ReceivePackSuffix
)

// A BOX'S THREE ROUTES: the same three entry points under auth.BoxGitPrefix,
// behind the box's admission (boxadmit.go) rather than a person's — a coding
// run's box, or a check's, which fetches alone.
const (
	PatternBoxInfoRefs    = "GET /box/git/{repo}/info/refs"
	PatternBoxUploadPack  = "POST /box/git/{repo}" + UploadPackSuffix
	PatternBoxReceivePack = "POST /box/git/{repo}" + ReceivePackSuffix
)

// A CHECK'S BOX'S THREE READS of its commit by object id — its tree, a file of
// it, its diff — under auth.BoxCodePrefix: the API's routes, whose handlers
// admit the box by AdmitCheck, which reads the repository from the same
// wildcard ({repo}, PathRepo), so a route reads its repository from the path
// it was mounted at.
const (
	PatternBoxCodeTree = "GET /box/code/{repo}/tree"
	PatternBoxCodeBlob = "GET /box/code/{repo}/blob"
	PatternBoxCodeDiff = "GET /box/code/{repo}/diff"
)

// What a client that speaks the protocol itself, rather than through git,
// posts to a repository's two RPCs: the path after the repository's own
// (`/git/{repo}`), and the content type each handler requires, refusing any
// other `415`. ONE spelling with the patterns above and the handlers' checks,
// so such a client — the suite holding a fetch's reader still while a
// compaction runs, say — posts where a route is mounted and what the host
// takes, and spells no git service of its own: outside this package and
// GitAccess, a spelled service is what internal/api/auth's
// TestGitAccessIsTheOneClassifier fails as a second classifier.
const (
	UploadPackSuffix       = "/git-upload-pack"
	UploadPackRequestType  = "application/x-git-upload-pack-request"
	ReceivePackSuffix      = "/git-receive-pack"
	ReceivePackRequestType = "application/x-git-receive-pack-request"
)

// admitter is how an entry point admits its request: admit for a person's,
// admitBox for a box's — a coding run's or a check's. Everything after it
// reads the admission alone.
type admitter func(ctx context.Context, w http.ResponseWriter, r *http.Request, op auth.GitOp) (admission, bool)

// InfoRefs answers a repository's advertisement, a fetch's or a push's.
func (s *Server) InfoRefs() http.Handler { return s.route(s.infoRefs, s.admit) }

// UploadPack answers a fetch's RPC.
func (s *Server) UploadPack() http.Handler { return s.route(s.uploadPack, s.admit) }

// ReceivePack answers a push's RPC.
func (s *Server) ReceivePack() http.Handler { return s.route(s.receivePack, s.admit) }

// BoxInfoRefs answers a box's advertisement, a fetch's or a push's.
func (s *Server) BoxInfoRefs() http.Handler { return s.route(s.infoRefs, s.admitBox) }

// BoxUploadPack answers a box's fetch RPC.
func (s *Server) BoxUploadPack() http.Handler { return s.route(s.uploadPack, s.admitBox) }

// BoxReceivePack answers a coding box's push RPC: a check's box pushes nothing.
func (s *Server) BoxReceivePack() http.Handler { return s.route(s.receivePack, s.admitBox) }

// route is entry, admitting its requests with admit.
func (s *Server) route(entry func(http.ResponseWriter, *http.Request, admitter), admit admitter) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { entry(w, r, admit) })
}
