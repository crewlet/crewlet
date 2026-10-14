package host

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/statelog"
)

// The route patterns the API mounts and the suite's mux — ONE spelling, so the
// path a handler reads its repository from is the path it was mounted at.
const (
	// PathRepo is the wildcard the repository's segment is read from.
	PathRepo = "repo"
	// PatternInfoRefs is the advertisement; a GET pattern also matches HEAD.
	PatternInfoRefs = "GET /git/{repo}/info/refs"
	// PatternUploadPack is a fetch's RPC.
	PatternUploadPack = "POST /git/{repo}/git-upload-pack"
	// PatternReceivePack is a push's RPC.
	PatternReceivePack = "POST /git/{repo}/git-receive-pack"
)

// InfoRefs answers a repository's advertisement, a fetch's or a push's.
func (s *Server) InfoRefs() http.Handler { return http.HandlerFunc(s.infoRefs) }

// UploadPack answers a fetch's RPC.
func (s *Server) UploadPack() http.Handler { return http.HandlerFunc(s.uploadPack) }

// ReceivePack answers a push's RPC.
func (s *Server) ReceivePack() http.Handler { return http.HandlerFunc(s.receivePack) }

// VerifyRepo checks this node's cache of the named repository against its rows,
// rebuilding it when repair (cache.Verify). The repository is resolved by name
// — stale, and again through the barrier when not found, since a refusal is
// never a stale answer; code.ErrRepoNotFound when it is not declared, or when
// the name is one the grammar refuses (a repository that cannot exist), and
// ErrStopping on a stopping host.
//
// ErrStopping TOO FOR A VERIFY THE STOP CUT SHORT, wrapping what the cut left:
// Stop ends every verify in flight through the host's lifetime, so its error
// is then the context's own `canceled`, which reads as the caller giving up —
// and a caller still waiting is owed what the node told every other request it
// stopped serving (503, Retry-After StoppingRetryAfter), not a failure of the
// repository it asked about. A verify its own caller ended is not one the stop
// cut, and keeps its error.
func (s *Server) VerifyRepo(ctx context.Context, name string, repair bool) (cache.VerifyReport, error) {
	if !gitref.ValidRepoName(name) {
		return cache.VerifyReport{}, code.ErrRepoNotFound
	}
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return cache.VerifyReport{}, ErrStopping
	}
	s.inflight.Add(1)
	s.mu.Unlock()
	defer s.inflight.Done()
	work, cancel := context.WithCancel(ctx)
	defer cancel()
	//nolint:contextcheck // The host's lifetime ends a verify too: Stop must reach its git.
	defer context.AfterFunc(s.life, cancel)()

	report, err := s.verify(work, name, repair)
	if err != nil && s.life.Err() != nil && ctx.Err() == nil {
		return report, fmt.Errorf("%w: %w", ErrStopping, err)
	}
	return report, err
}

// verify is VerifyRepo's work under ctx: the name resolved, then the cache's
// check of what it names.
func (s *Server) verify(ctx context.Context, name string, repair bool) (cache.VerifyReport, error) {
	ref, err := s.o.Estate.Resolve(ctx, code.ResolveQuery{Name: name,
		Fresh: statelog.Freshness{Level: statelog.ReadStale}})
	if errors.Is(err, code.ErrRepoNotFound) {
		ref, err = s.o.Estate.Resolve(ctx, code.ResolveQuery{Name: name,
			Fresh: statelog.Freshness{Level: statelog.ReadLinearizable}})
	}
	if err != nil {
		return cache.VerifyReport{}, err
	}
	return s.o.Cache.Verify(ctx, ref.ID, repair)
}
