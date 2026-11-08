package codesearch

import (
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/recordseal"
	"github.com/crewlet/crewlet/internal/signedask"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE WIRE a scatter crosses: one [Request] on topics.CodeSearch, served by
// every Gitlet node, each answering only for the repositories the request
// assigns it, and one [Reply] from each of those.
//
// # Evolution
//
// Additive only, [Protocol] stamped on both halves: an answerer that does not
// speak a request's protocol answers nothing, which its asker reads as a node
// that did not answer and asks the runner-up of; an asker drops a reply at a
// protocol it does not speak. An estate request's shape and no record's, so the
// code domain's RecordVersion does not move for it — as a STATED CONDITION
// (M3-O84 §2 item 5), no build that carries the code domain has run without
// it, since none of the domain is on main.

// Protocol is the version of the request and reply this build writes and
// reads.
const Protocol = 1

// Channel is the scatter's name among the fleet's signed channels, and so the
// stem of the two key domains a request and a reply are signed under — its
// own, so no estate envelope, steer note or state-log record verifies as one.
const Channel = "code.search"

// Wire is a node's two halves of the scatter's envelopes.
type Wire = signedask.Channel[Request, Reply]

// NewWire builds a node's envelopes over the fleet keyring every state-log
// record is signed under ([signedask.New]).
func NewWire(ring recordseal.Keyring) (*Wire, error) {
	return signedask.New[Request, Reply](Channel, ring)
}

// Request is one round of a scatter: the search, WHO ASKS IT — judged by
// every answering node as a read of each repository is ([code.GitQuery]'s
// Asker, CeilingSeat and Turn) — and which node answers for which
// repository.
type Request struct {
	Version int `json:"version"`
	// ID and Deadline bind the request ([Request.Binding]): minted per round,
	// so a round and its runner-up round are two requests.
	ID       string    `json:"id"`
	Deadline time.Time `json:"deadline"`

	Asker       code.Principal     `json:"asker"`
	CeilingSeat string             `json:"ceiling_seat,omitempty"`
	Turn        string             `json:"turn,omitempty"`
	Fresh       statelog.Freshness `json:"fresh"`
	// Head is the code log's head as the asker's linearizable listing of
	// the repositories read it (code.GitQuery.Head), zero where it listed
	// them at another level: each repository's one second look syncs to it
	// rather than taking a barrier of its own, so a search naming a ref most
	// repositories lack costs the fleet the one barrier its listing took.
	Head   statelog.Position `json:"head,omitzero"`
	Search code.SearchQuery  `json:"search"`

	// Assign is every node asked this round and the repositories it answers
	// for, each repository named under exactly one node.
	Assign map[string][]Assigned `json:"assign"`
}

// Binding is the request's id and deadline (internal/signedask).
func (r Request) Binding() (string, time.Time) { return r.ID, r.Deadline }

// Assigned is one repository a node answers for.
type Assigned struct {
	RepoID string `json:"repo_id"`
	// Name orders the node's hits as the asker merges them (by name, path,
	// line), so the node can keep only the first Limit of them: what no other
	// node's hits can come before.
	Name string `json:"name"`
	// Home lets the node fill a copy it does not hold ([code.GitQuery].Home):
	// set for the first node of the repository's order, and for its runner-up
	// where that node passed the fill on ([code.UnreadyReason.PassesFill]) or
	// did not answer at all.
	Home bool `json:"home,omitempty"`
	// After is the place the search continues from ([Key]), set on the one
	// repository it falls in: the node reads it at the key's commit, past
	// the key's line. Additive, under the STATED CONDITION above.
	After *Key `json:"after,omitempty"`
}

// Key is a place in a search's order, the one its pages are cut at: a
// repository, the commit it was searched at, and a line of it — the last
// line a page answered ([KeyOf]). A search continued after it ([Query.After])
// asks no repository whose name sorts before the key's, and reads the key's
// own repository AT THE KEY'S COMMIT, past the key's line: the lines a page
// answers continue the very file the page before was cut in, whatever was
// pushed between the two, so no line of it is answered twice or skipped
// because a push moved it; every repository after it is read at the commit
// it is searched at now, none of its lines answered yet.
type Key struct {
	Repo string `json:"repo"`
	Tip  string `json:"tip"`
	code.SearchAfter
}

// KeyOf is h's place, h read at tip.
func KeyOf(h Hit, tip string) Key {
	return Key{Repo: h.Repo, Tip: tip, SearchAfter: code.SearchAfterOf(h.SearchHit)}
}

// of is k where repo is k's repository, and nil otherwise or where k is.
func (k *Key) of(repo string) *Key {
	if k == nil || k.Repo != repo {
		return nil
	}
	return k
}

// continueIn makes sq, a search of repo, the search continued after k where
// repo is k's repository — at k's commit, past k's line — and leaves it as it
// is otherwise.
func (k *Key) continueIn(repo string, sq *code.SearchQuery) {
	if k = k.of(repo); k == nil {
		return
	}
	after := k.SearchAfter
	sq.Ref, sq.After = k.Tip, &after
}

// Reply is one node's answer for the repositories a request assigned it.
type Reply struct {
	Version int `json:"version"`
	// Request is the id of the request this answers ([Reply.Answers]).
	Request string `json:"request"`
	// Node is the node that answers: what the asker names it by in the
	// coverage, and whose silence it can then tell from its answer.
	Node string `json:"node"`
	// Unverified is set, and nothing else, where the node took nothing
	// because the request is signed under a key it does not hold: the
	// sentence names the keys it holds.
	Unverified string `json:"unverified,omitempty"`
	// Repos is what became of each repository assigned it.
	Repos []RepoResult `json:"repos,omitempty"`
	// Hits are the first Limit lines of its repositories' answers by (name,
	// path, line); Cut says it left lines out past them, or named a
	// repository past the limit ([OutcomePastLimit]).
	Hits []Hit `json:"hits,omitempty"`
	Cut  bool  `json:"cut,omitempty"`
}

// Answers is the id of the request this reply answers (internal/signedask).
func (r Reply) Answers() string { return r.Request }

// Outcome is what became of one repository on the node asked for it. A
// closed set ([Outcomes]).
type Outcome string

const (
	// OutcomeSearched means the node answered its lines.
	OutcomeSearched Outcome = "searched"
	// OutcomeUnready means the node could not answer it now, for a reason about
	// the node (code.ErrGitUnready); its runner-up is asked.
	OutcomeUnready Outcome = "unready"
	// OutcomeNotPermitted means the node judged the asker's grant and it does not
	// cover the repository (code.ErrNotPermitted) — a chart that moved since
	// the asker's listing.
	OutcomeNotPermitted Outcome = "not_permitted"
	// OutcomeNoRef means the repository holds no such ref or commit
	// (code.ErrRefNotFound, code.ErrObjectNotFound): nothing in it to search
	// at what the query names.
	OutcomeNoRef Outcome = "no_ref"
	// OutcomeFailed means the node refused it for any other reason — a request
	// this build reads otherwise than its asker's; its runner-up is asked.
	OutcomeFailed Outcome = "failed"
	// OutcomePastLimit means no line of it can be in the answer: the node's
	// first Limit lines are all of repositories named before it, so every line
	// it holds sorts after them. Read or passed over unread — which turns on
	// how the node's reads interleaved, so the node names it by its name alone
	// once every read has returned. Final: its runner-up is never asked.
	OutcomePastLimit Outcome = "past_limit"
)

// Outcomes is the closed set.
func Outcomes() []Outcome {
	return []Outcome{OutcomeSearched, OutcomeUnready, OutcomeNotPermitted, OutcomeNoRef, OutcomeFailed,
		OutcomePastLimit}
}

// RepoResult is one repository's outcome.
type RepoResult struct {
	RepoID  string  `json:"repo_id"`
	Outcome Outcome `json:"outcome"`
	// Tip, Via, SkippedFiles and Truncated are its answer's, where searched.
	Tip          string         `json:"tip,omitempty"`
	Via          code.SearchVia `json:"via,omitempty"`
	SkippedFiles int            `json:"skipped_files,omitempty"`
	Truncated    bool           `json:"truncated,omitempty"`
	// Unready is the node's reason, where unready.
	Unready code.UnreadyReason `json:"unready,omitempty"`
	// Detail is the node's sentence, where not searched.
	Detail string `json:"detail,omitempty"`
}

// Hit is one matching line of one repository.
type Hit struct {
	// Repo is the repository's name.
	Repo string `json:"repo"`
	code.SearchHit
}
