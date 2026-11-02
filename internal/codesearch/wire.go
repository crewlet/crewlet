package codesearch

import (
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/signedask"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE WIRE a scatter crosses: one [Request] on topics.CodeSearch, served by
// every git node, each answering only for the repositories the request
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
func NewWire(ring statelog.Keyring) (*Wire, error) {
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
	Search      code.SearchQuery   `json:"search"`

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
	// path, line); Cut says it left lines out past them.
	Hits []Hit `json:"hits,omitempty"`
	Cut  bool  `json:"cut,omitempty"`
}

// Answers is the id of the request this reply answers (internal/signedask).
func (r Reply) Answers() string { return r.Request }

// Outcome is what became of one repository on the node asked for it. A
// closed set ([Outcomes]).
type Outcome string

const (
	// OutcomeSearched: the node answered its lines.
	OutcomeSearched Outcome = "searched"
	// OutcomeUnready: the node could not answer it now, for a reason about
	// the node (code.ErrGitUnready); its runner-up is asked.
	OutcomeUnready Outcome = "unready"
	// OutcomeNotPermitted: the node judged the asker's grant and it does not
	// cover the repository (code.ErrNotPermitted) — a chart that moved since
	// the asker's listing.
	OutcomeNotPermitted Outcome = "not_permitted"
	// OutcomeNoRef: the repository holds no such ref or commit
	// (code.ErrRefNotFound, code.ErrObjectNotFound): nothing in it to search
	// at what the query names.
	OutcomeNoRef Outcome = "no_ref"
	// OutcomeFailed: the node refused it for any other reason — a request
	// this build reads otherwise than its asker's; its runner-up is asked.
	OutcomeFailed Outcome = "failed"
)

// Outcomes is the closed set.
func Outcomes() []Outcome {
	return []Outcome{OutcomeSearched, OutcomeUnready, OutcomeNotPermitted, OutcomeNoRef, OutcomeFailed}
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
