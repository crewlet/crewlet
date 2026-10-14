package host

import (
	"net/http"
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
