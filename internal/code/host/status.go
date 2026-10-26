package host

import (
	"time"

	"github.com/crewlet/crewlet/internal/code/cache"
)

// WHAT THIS NODE'S CODE HOST SAYS OF ITSELF: its cache against its budget, how
// many repositories it holds a copy of, what the cache spent on its own
// account, and the repositories it served most over the last hour and the last
// day with each one's copy in service — the heat the host counts into the
// cache as it answers (cache.Heat). The engine answers its peers' status
// scatter with it (topics.CodeStatus); the host imports no queue, so what it
// hands the engine is a value.
//
// A REQUEST'S USE IS COUNTED ONCE, WHERE ITS ANSWER ENDS (served): an
// advertisement of upload-pack is a fetch — every git process that reads a
// repository opens with one — and a fetch's transfer that offered no `have`
// and was served to its end is a clone; a push is counted where it is
// recorded (audit), which every push the node admits reaches exactly once
// however its request ends (pushAudit) — refused or failed at its
// advertisement, or at its POST whatever became of it; and every answer adds
// the bytes git's process wrote its client (sink.Sent). A FETCH refused before
// git ran — its credential, its repository, its body — served nothing of the
// repository and counts nothing. A push refused or failed before git ran is
// counted all the same, at its record, as the push the node was asked for;
// only a request refused before its admission — a credential refused, a
// repository the node does not have, a host stopping — names no repository
// and nobody, and counts nothing.

// CodeStatus is what this node's code host says of itself, as of At.
type CodeStatus struct {
	// Node is this node's id.
	Node string `json:"node"`
	// At is when the status was read.
	At time.Time `json:"at"`
	// Cache is the cache as it stands.
	Cache cache.Status `json:"cache"`
	// Hot is the cache.HotRepos repositories the node served most in the
	// hour and in the day, in one list, those hottest in the hour first
	// (cache.Heat.Hot); empty when it served none within the day.
	Hot []RepoStatus `json:"hot"`
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
	// none — never synced here, evicted, purged — which beside a hot
	// repository's heat is a budget too small for what the node is asked.
	Copy *cache.Copy `json:"copy,omitempty"`
}

// Status is this node's code host as it stands now, and the repository whose
// id is repo beside it where repo is not "". It reads running totals and the
// heat's windows in memory, and no row and no disk, so it answers at once
// whatever the node is doing.
func (s *Server) Status(repo string) CodeStatus {
	c := s.o.Cache
	out := CodeStatus{Node: s.o.NodeID, At: s.now().UTC(), Cache: c.Status(), Hot: []RepoStatus{}}
	for _, h := range c.Heat().Hot(cache.HotRepos) {
		out.Hot = append(out.Hot, s.repoStatus(h))
	}
	if repo != "" {
		r := s.repoStatus(c.Heat().Of(repo))
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
