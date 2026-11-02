package codesearch

import (
	"cmp"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/signedask"
)

// Reads is a git node's reads by object id as the serving half asks them:
// internal/code/query's Server, which syncs the copy, JUDGES THE ASKER'S GRANT
// from this node's own chart and reads the commit through the index or with
// git grep. Declared here by its consumer; the engine hands it this node's
// git reads.
type Reads interface {
	Read(ctx context.Context, q code.GitQuery) (code.GitAnswer, error)
}

// ServeReads is the most of one request's repositories a node reads at once.
//
// queue.ConcurrentAnswers (64): the git each read runs is bounded by the
// node's seat git-work share — half its read lane, a core a slot — which no
// plausible host makes wider than this, so the bound never idles the share;
// what it bounds is the rest of a read, its sync of the copy and its wait for
// a unit, so a request assigning a node thousands of repositories holds 64
// goroutines and syncs at once rather than thousands. A read that waits past
// code.GitShareWait for a unit is answered unready share, and its runner-up
// is asked.
const ServeReads = queue.ConcurrentAnswers

// errNotAsked is a request that assigns this node nothing: answered with
// nothing, since a scatter is served by every git node and only the nodes it
// names answer it.
var errNotAsked = errors.New("codesearch: the request assigns this node no repository")

// Server is one git node's answerer of the scatter.
type Server struct {
	wire  *Wire
	taken *signedask.Guard
	reads Reads
	node  string
	now   func() time.Time
}

// NewServer is node's answerer over its reads, verifying requests with wire.
func NewServer(wire *Wire, node string, reads Reads) (*Server, error) {
	if wire == nil || reads == nil || node == "" {
		return nil, errors.New("codesearch: a server needs the scatter's envelopes, its node's id and its " +
			"reads")
	}
	return &Server{wire: wire, taken: signedask.NewGuard(), reads: reads, node: node, now: time.Now}, nil
}

// Answer answers one sealed request: queue.AnswerFunc's shape, the function
// the engine registers on topics.CodeSearch.
//
// IT CARRIES OUT ONLY WHAT IT ADMITS (signedask.Channel.Admit): a request the
// fleet did not sign, one past its deadline, one whose id it has taken and one
// this build cannot read are answered with nothing — the asker hears a node
// that did not answer — because each read is made AS THE ASKER THE REQUEST
// NAMES, judged by this node's chart, so an unsigned request would let any
// client of the broker read any repository as any seat in the company. One
// signed under a key this node does not hold is answered Unverified, naming
// the keys it holds, so the asker can say what to fix.
func (s *Server) Answer(ctx context.Context, sealed []byte) ([]byte, error) {
	adm := s.wire.Admit(sealed, s.taken, s.now())
	switch adm.Disposition {
	case signedask.Admitted:
	case signedask.KeyUnknown:
		if len(adm.Request.Assign[s.node]) == 0 {
			return nil, errNotAsked
		}
		return s.wire.SealReply(Reply{Version: Protocol, Request: adm.ID, Node: s.node, Unverified: adm.Why})
	default:
		return nil, fmt.Errorf("codesearch: %s request refused: %s", adm.Disposition, adm.Why)
	}
	req := adm.Request
	if req.Version != Protocol {
		return nil, fmt.Errorf("codesearch: a request at protocol %d, and this build speaks %d", req.Version,
			Protocol)
	}
	asked := req.Assign[s.node]
	if len(asked) == 0 {
		return nil, errNotAsked
	}
	rctx, cancel := code.AnswerBy(ctx, req.Deadline)
	defer cancel()
	return s.wire.SealReply(s.search(rctx, req, asked))
}

// search reads every repository asked, at most ServeReads at once, and keeps
// the first Limit hits of them all.
func (s *Server) search(ctx context.Context, req Request, asked []Assigned) Reply {
	results := make([]RepoResult, len(asked))
	hits := make([][]Hit, len(asked))
	slots := make(chan struct{}, ServeReads)
	var wg sync.WaitGroup
	for i, a := range asked {
		slots <- struct{}{}
		wg.Go(func() {
			defer func() { <-slots }()
			results[i], hits[i] = s.one(ctx, req, a)
		})
	}
	wg.Wait()
	all := slices.Concat(hits...)
	limit := req.Search.Hits()
	slices.SortFunc(all, CompareHits)
	out := Reply{Version: Protocol, Request: req.ID, Node: s.node, Repos: results}
	if len(all) > limit {
		all, out.Cut = all[:limit], true
	}
	out.Hits = all
	return out
}

// one reads one repository as the request's asker.
func (s *Server) one(ctx context.Context, req Request, a Assigned) (RepoResult, []Hit) {
	sq := req.Search
	got, err := s.reads.Read(ctx, code.GitQuery{Kind: code.GitSearch, RepoID: a.RepoID, Asker: req.Asker,
		CeilingSeat: req.CeilingSeat, Turn: req.Turn, Fresh: req.Fresh, Home: a.Home, Search: &sq})
	r := RepoResult{RepoID: a.RepoID}
	var unready *code.ErrGitUnready
	switch {
	case err == nil && got.Search != nil:
		ans := got.Search
		r.Outcome, r.Tip, r.Via, r.SkippedFiles, r.Truncated = OutcomeSearched, ans.Tip, ans.Via,
			ans.SkippedFiles, ans.Truncated
		hits := make([]Hit, len(ans.Hits))
		for i, h := range ans.Hits {
			hits[i] = Hit{Repo: a.Name, SearchHit: h}
		}
		return r, hits
	case err == nil:
		r.Outcome, r.Detail = OutcomeFailed, "the node answered a search with no search in its answer"
	case errors.As(err, &unready):
		r.Outcome, r.Unready, r.Detail = OutcomeUnready, unready.Reason, unready.Detail
	case errors.Is(err, code.ErrNotPermitted):
		r.Outcome, r.Detail = OutcomeNotPermitted, err.Error()
	case errors.Is(err, code.ErrRefNotFound), errors.Is(err, code.ErrObjectNotFound):
		r.Outcome, r.Detail = OutcomeNoRef, err.Error()
	default:
		r.Outcome, r.Detail = OutcomeFailed, err.Error()
	}
	return r, nil
}

// CompareHits is the merge's one order: repository name, path, line — the
// path by its BYTES, as a repository's own answer orders it (code.SearchAnswer),
// so a path that is not UTF-8, whose text carries U+FFFD where its bytes
// differ, is placed by what it is. Two hits of one repository, path and line
// are one line, which a repository answers once. Exported because a surface
// that pages an answer (builtin's `code_search` cursor) places its pages by
// this same order: a copy of it there would page by an order the search no
// longer answers in the day either one changes.
func CompareHits(a, b Hit) int {
	return cmp.Or(cmp.Compare(a.Repo, b.Repo), cmp.Compare(rawPath(a), rawPath(b)), cmp.Compare(a.Line, b.Line))
}

// rawPath is a hit's path as its bytes (code.WirePath's inverse).
func rawPath(h Hit) string {
	if h.PathB64 == "" {
		return h.Path
	}
	raw, err := base64.StdEncoding.DecodeString(h.PathB64)
	if err != nil {
		// A node's encoding of its own path never fails to decode; were one
		// to, its text still places it.
		return h.Path
	}
	return string(raw)
}
