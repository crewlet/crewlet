package codesearch

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"golang.org/x/sync/semaphore"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/signedask"
)

// Reads is a Gitlet node's reads by object id as the serving half asks them:
// internal/gitlet/query's Server, which syncs the copy, JUDGES THE ASKER'S GRANT
// from this node's own chart and reads the commit through the index or with
// git grep. Declared here by its consumer; the engine hands it this node's
// git reads.
type Reads interface {
	Read(ctx context.Context, q gitlet.GitQuery) (gitlet.GitAnswer, error)
}

// ScatterReads is how many of a Gitlet node's repositories its scatter reads at
// once, ACROSS EVERY REQUEST it is answering, where its seat git-work share
// (gitlet.GitShare) holds units units: ALL BUT ONE of them, and one where the
// share holds only one (a read lane of three cores or fewer: gitlet.ShareOf),
// where there is no unit to keep apart: the scatter waits for it FIFO beside
// the seats' reads, one at a time, so a seat waits behind at most one search.
// Why it is tied to the share is on [Server]'s scatter field.
func ScatterReads(units int) int { return max(1, units-1) }

// errNotAsked is a request that assigns this node nothing: answered with
// nothing, since a scatter is served by every Gitlet node and only the nodes it
// names answer it.
var errNotAsked = errors.New("codesearch: the request assigns this node no repository")

// Server is one Gitlet node's answerer of the scatter.
type Server struct {
	wire  *Wire
	taken *signedask.Guard
	reads Reads
	node  string
	now   func() time.Time
	// units is the node's seat git-work share, and scatter its
	// ScatterReads(units) slots, taken by every request's reads alike, FIFO.
	// TIED TO THE SHARE, because every scatter read waits on it: each search
	// takes a unit (gitlet.GitShare.Take), FIFO behind every seat's read.
	// Bounded by the requests' answer slots instead (queue.ConcurrentAnswers,
	// 64 a request, 64 requests a node), one unfiltered search queued 64
	// waiters on every node's share and a few thousands, so every seat's read
	// waited past gitlet.GitShareWait and was answered unready everywhere.
	// Here a scatter holds at most units-1 units, so a seat's read always finds
	// one, and one search on an idle node still uses all but that one. A read
	// holds its unit around its search alone, never its sync of the copy.
	units   int
	scatter *semaphore.Weighted
	// answers is the node's answer budget each answer holds [answerWeight] of.
	answers *queue.AnswerBudget
}

// ReplyHitBytes bounds the lines one node's reply carries, by their JSON: a
// quarter of [queue.MaxPayloadBytes], so the lines, every assigned
// repository's outcome and the seal's base64 (4/3) fit one message. The Limit
// alone bounds the count, never the bytes: a hit is up to one repository's
// 56 KiB git answer (gitlet.MaxGitAnswerBytes), so 500 of them passed the
// broker's ceiling and the reply was lost. Past it the reply is Cut after its
// last line kept, which the asker honours as the end of what it may trust.
const ReplyHitBytes = queue.MaxPayloadBytes / 4

// answerWeight is what an answer holds of the node's answer budget
// ([queue.AnswerBudget]) from before its reads until its reply is written:
// [ReplyHitBytes] four times, from the code: the lines held (1), their
// encoding in the reply (1), the seal's base64 (4/3) and the encoder's buffer
// beside them, with the reads in flight (at most [ScatterReads] git answers
// of 56 KiB) and the repositories' outcomes inside the slack.
const answerWeight = 4 * ReplyHitBytes

// NewServer is node's answerer over its reads, verifying requests with wire,
// its reads bounded by a seat git-work share of units units
// (gitlet.GitShare.Units).
func NewServer(wire *Wire, node string, reads Reads, units int, answers *queue.AnswerBudget) (*Server, error) {
	if wire == nil || reads == nil || node == "" || units < 1 || answers == nil {
		return nil, errors.New("codesearch: a server needs the scatter's envelopes, its node's id, its reads, " +
			"its node's seat git-work share and its node's answer budget")
	}
	return &Server{wire: wire, taken: signedask.NewGuard(), reads: reads, node: node, now: time.Now,
		units: units, scatter: semaphore.NewWeighted(int64(ScatterReads(units))), answers: answers}, nil
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
	// THE REQUEST'S ARRIVAL, on both clocks at once: the runtime's, on which
	// its reads are counted, and the node's own, which admits it.
	arrived, now := time.Now(), s.now()
	adm := s.wire.Admit(sealed, s.taken, now)
	switch adm.Disposition {
	case signedask.Admitted:
	case signedask.KeyUnknown:
		if len(adm.Request.Assign[s.node]) == 0 {
			return nil, errNotAsked
		}
		return s.wire.SealReply(Reply{Version: replyProtocol(adm.Request.Version), Request: adm.ID, Node: s.node,
			Unverified: adm.Why})
	default:
		return nil, fmt.Errorf("codesearch: %s request refused: %s", adm.Disposition, adm.Why)
	}
	req := adm.Request
	if req.Version < baseProtocol || req.Version > Protocol {
		return nil, fmt.Errorf("codesearch: a request at protocol %d, and this build speaks %d to %d", req.Version,
			baseProtocol, Protocol)
	}
	if req.Version < ContextProtocol && requestProtocol(req.Search) >= ContextProtocol {
		return nil, fmt.Errorf("codesearch: a request at protocol %d carries context or per_file, which "+
			"protocol %d introduced", req.Version, ContextProtocol)
	}
	asked := req.Assign[s.node]
	if len(asked) == 0 {
		return nil, errNotAsked
	}
	due, nobody := signedask.Due(arrived, req.BudgetMS)
	if nobody != "" {
		return nil, fmt.Errorf("codesearch: %s request refused: %s", signedask.Stale, nobody)
	}
	rctx, cancel := gitlet.AnswerBy(ctx, due)
	defer cancel()
	// HELD FROM BEFORE THE READS until the reply is written, waited for no
	// longer than the answer is due: one the budget cannot admit by then
	// answers nothing, and the asker asks the repositories' runners-up.
	release, err := s.answers.Acquire(rctx, queue.AnswerCodeSearch, answerWeight)
	if err != nil {
		return nil, fmt.Errorf("codesearch: %w", err)
	}
	defer release()
	return s.wire.SealReply(s.search(rctx, req, asked))
}

// replyProtocol is the protocol a reply to a request stamped v carries: v,
// where this build speaks it, so its asker reads the reply; Protocol otherwise.
func replyProtocol(v int) int {
	if v < baseProtocol || v > Protocol {
		return Protocol
	}
	return v
}

// search reads the repositories asked IN THE ORDER OF THEIR NAMES, in the
// node's ScatterReads slots, folding each one's lines into the request's
// merge as its read returns, so it holds the first Limit lines and the reads
// in flight, never every answer, and PASSES OVER a repository the merge has
// passed: once the first Limit lines are all of repositories named before it,
// none of its lines can be in the reply. Every repository named after the last
// line kept is past the limit ([OutcomePastLimit]), read or not, decided by
// name once every read has returned so the reply is the same however the
// reads interleaved. A line past the last of a repository that stopped early
// is the asker's to leave out ([prefixEnd]); the node cuts by its limit alone.
func (s *Server) search(ctx context.Context, req Request, asked []Assigned) Reply {
	results := make([]RepoResult, len(asked))
	order := make([]int, len(asked))
	for i := range order {
		order[i] = i
	}
	slices.SortFunc(order, func(x, y int) int { return cmp.Compare(asked[x].Name, asked[y].Name) })
	m := newMerge(req.Search.Hits())
	var wg sync.WaitGroup
	for _, i := range order {
		a := asked[i]
		if err := s.scatter.Acquire(ctx, 1); err != nil {
			// THE ANSWER FELL DUE, or its asker went away, before a slot came
			// free: unready for this node's share, so its runner-up is asked.
			results[i] = RepoResult{RepoID: a.RepoID, Outcome: OutcomeUnready, Unready: gitlet.UnreadyShare,
				Detail: fmt.Sprintf("%s's code searches held their part of its seat git-work share — %d of its "+
					"%d units — until the search's answer fell due; another Gitlet node answers meanwhile", s.node,
					ScatterReads(s.units), s.units)}
			continue
		}
		// Asked once a slot is free: the reads that held it may have filled
		// the limit meanwhile.
		if m.passed(a.Name) {
			s.scatter.Release(1)
			continue
		}
		wg.Go(func() {
			defer s.scatter.Release(1)
			var hits []Hit
			results[i], hits = s.one(ctx, req, a)
			m.fold(hits)
		})
	}
	wg.Wait()
	out := Reply{Version: req.Version, Request: req.ID, Node: s.node, Repos: results}
	out.Hits, out.Cut = m.lines()
	for i, a := range asked {
		if m.passed(a.Name) {
			results[i], out.Cut = RepoResult{RepoID: a.RepoID, Outcome: OutcomePastLimit}, true
		}
	}
	var cut bool
	if out.Hits, cut = hitsWithin(out.Hits, ReplyHitBytes); cut && len(out.Hits) > 0 {
		// CUT BY BYTES, as the limit cuts: a repository named after the last
		// line kept is past it, whatever it answered.
		out.Cut = true
		last := out.Hits[len(out.Hits)-1].Repo
		for i, a := range asked {
			if cmp.Less(last, a.Name) && results[i].Outcome == OutcomeSearched {
				results[i] = RepoResult{RepoID: a.RepoID, Outcome: OutcomePastLimit}
			}
		}
	}
	return out
}

// hitsWithin is the first of hits whose JSON, a comma between each, fits room
// bytes, the first always kept, and whether it left any out.
func hitsWithin(hits []Hit, room int) ([]Hit, bool) {
	used := 0
	for i, h := range hits {
		raw, err := json.Marshal(h)
		if err != nil {
			return hits[:i], true
		}
		if used += len(raw) + 1; used > room && i > 0 {
			return hits[:i], true
		}
	}
	return hits, false
}

// one reads one repository as the request's asker.
func (s *Server) one(ctx context.Context, req Request, a Assigned) (RepoResult, []Hit) {
	sq := req.Search
	a.After.continueIn(a.Name, &sq)
	got, err := s.reads.Read(ctx, gitlet.GitQuery{Kind: gitlet.GitSearch, RepoID: a.RepoID, Asker: req.Asker,
		CeilingSeat: req.CeilingSeat, Turn: req.Turn, Fresh: req.Fresh, Head: req.Head, Home: a.Home, Search: &sq})
	r := RepoResult{RepoID: a.RepoID}
	var unready *gitlet.ErrGitUnready
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
	case errors.Is(err, gitlet.ErrNotPermitted):
		r.Outcome, r.Detail = OutcomeNotPermitted, err.Error()
	case errors.Is(err, gitlet.ErrRefNotFound), errors.Is(err, gitlet.ErrObjectNotFound):
		r.Outcome, r.Detail = OutcomeNoRef, err.Error()
	default:
		r.Outcome, r.Detail = OutcomeFailed, err.Error()
	}
	return r, nil
}

// CompareHits is the merge's one order: repository name, path, line — the
// path by its BYTES, as a repository's own answer orders it (gitlet.SearchAnswer),
// so a path that is not UTF-8, whose text carries U+FFFD where its bytes
// differ, is placed by what it is. Two hits of one repository, path and line
// are one line, which a repository answers once. Exported because a surface
// that pages an answer (builtin's `code_search` cursor) places its pages by
// this same order: a copy of it there would page by an order the search no
// longer answers in the day either one changes.
func CompareHits(a, b Hit) int {
	return cmp.Or(cmp.Compare(a.Repo, b.Repo), cmp.Compare(rawPath(a), rawPath(b)), cmp.Compare(a.Line, b.Line))
}

// rawPath is a hit's path as its bytes (gitlet.WirePath's inverse).
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

// prefixEnd is where the lines a merge holds stop being the FIRST lines of
// the search: the earliest last line of a repository that stopped early (at
// its limit, its answer's bound or its budget) and of a node that cut its
// lines at the limit. Each answered its first lines in the search's order up
// to its last (gitlet.SearchAnswer), so a line of another repository after
// that may follow lines it never read; the asker's merge keeps none. That is
// what lets a page be asked after the last line of the page before ([Key]): a
// line kept past a repository's stop would carry the next page's place past
// its unread lines, answered on no page. A repository that stopped with no
// line marks no end: a page continued from before it would stop as early, so
// an end there would never move; Truncated says its lines are missing, and the
// merge goes on past it.
type prefixEnd struct{ end *Hit }

// at lowers the end to h.
func (p *prefixEnd) at(h Hit) {
	if p.end == nil || CompareHits(h, *p.end) < 0 {
		p.end = &h
	}
}

// stopped lowers the end to the last of hits of each repository repos names:
// the repositories of one reply that stopped early.
func (p *prefixEnd) stopped(hits []Hit, repos map[string]bool) {
	last := map[string]Hit{}
	for _, h := range hits {
		if l, ok := last[h.Repo]; repos[h.Repo] && (!ok || CompareHits(h, l) > 0) {
			last[h.Repo] = h
		}
	}
	for _, h := range last {
		p.at(h)
	}
}

// keep is sorted hits up to the end, and whether it left any out.
func (p *prefixEnd) keep(hits []Hit) ([]Hit, bool) {
	if p.end == nil {
		return hits, false
	}
	n := 0
	for n < len(hits) && CompareHits(hits[n], *p.end) <= 0 {
		n++
	}
	return hits[:n], n < len(hits)
}
