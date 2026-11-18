package host

import (
	"time"

	"github.com/crewlet/crewlet/internal/gitlet/cache"
)

// Gitlet's status on this node: its cache against its budget and its hottest
// repositories of the hour and the day (cache.Heat). The engine answers its
// peers' status scatter with it (topics.CodeStatus); the host imports no
// queue, so it hands the engine a value. What a request adds to the heat:
// package doc, "What a request served is counted where its answer ends". A
// fetch refused before git ran counts nothing; a push refused or failed
// before git ran still counts, at its record; a request refused before its
// admission names no repository and counts nothing.

// CodeStatus is what Gitlet on this node says of itself, as of At.
type CodeStatus struct {
	// Node is this node's id.
	Node string `json:"node"`
	// At is when the status was read.
	At time.Time `json:"at"`
	// Cache is the cache as it stands.
	Cache cache.Status `json:"cache"`
	// Hot is the cache.HotRepos repositories served most in the hour and in
	// the day, in one list, the hour's hottest first (cache.Heat.Hot); empty
	// when none was served within the day, nil when not asked for
	// ([StatusAsk.Hot]). Both go on the wire as no `hot`: either names no hot
	// repository.
	Hot []RepoStatus `json:"hot,omitempty"`
	// Repo is the repository the status was asked about: its heat here and
	// its copy in service, whether or not it is among the hottest; nil when
	// none was asked about.
	Repo *RepoStatus `json:"repo,omitempty"`
}

// RepoStatus is one repository on this node: what the node served of it in
// each window, and its copy in service.
type RepoStatus struct {
	cache.RepoHeat
	// Copy is the repository's copy in service here; nil where the node holds
	// none (never synced, evicted, purged), which beside a hot repository's
	// heat means a budget too small for what the node is asked.
	Copy *cache.Copy `json:"copy,omitempty"`
}

// StatusAsk is what a status is asked to carry beside the node's cache.
type StatusAsk struct {
	// Repo is the id of a repository to report, hot or not; "" asks about
	// none.
	Repo string
	// Hot asks for the node's hot list, up to 2 × cache.HotRepos repositories
	// with their copies and the bulk of a status; code_status reads it only on
	// the fleet's first page, so every other answer leaves it out.
	Hot bool
}

// Status is Gitlet on this node now, with what ask asks for. It reads only
// in-memory totals and heat windows, no row and no disk, so it answers at
// once whatever the node is doing.
func (s *Server) Status(ask StatusAsk) CodeStatus {
	c := s.o.Cache
	out := CodeStatus{Node: s.o.NodeID, At: s.now().UTC(), Cache: c.Status()}
	if ask.Hot {
		out.Hot = []RepoStatus{}
		for _, h := range c.Heat().Hot(cache.HotRepos) {
			out.Hot = append(out.Hot, s.repoStatus(h))
		}
	}
	if ask.Repo != "" {
		r := s.repoStatus(c.Heat().Of(ask.Repo))
		out.Repo = &r
	}
	return out
}

// repoStatus is h with the repository's copy in service.
func (s *Server) repoStatus(h cache.RepoHeat) RepoStatus {
	r := RepoStatus{RepoHeat: h}
	if cp, ok := s.o.Cache.Copy(h.ID); ok {
		r.Copy = &cp
	}
	return r
}

// served counts what one request of adm's served of its repository into the
// cache's heat: u, and every byte of git's answer out wrote the client.
func (s *Server) served(adm admission, out *sink, u cache.Use) {
	u.BytesOut = out.Sent()
	s.o.Cache.Heat().Add(adm.repo.ID, adm.name, u)
}
