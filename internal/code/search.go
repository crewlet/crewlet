package code

import (
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/codeindex"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A CODE SEARCH OF ONE REPOSITORY ([GitSearch], M6-D12, M6-D13) is a git
// read like any other: the keyed walk asks the repository's git nodes in
// their order, and the one that answers judges the asker's grant from its own
// chart. What differs is how it reads. A `search: indexed` repository's
// default branch is read through the trigram index its first [IndexHolders]
// git nodes keep of its tip (internal/code/cache's DerivedCodeIndex): the
// plan's candidates among the tip's files, confirmed line by line with Go's
// regexp. Everything else — a `search: grep` repository, a `ref:` other than
// the indexed tip, a node that holds no index — is read with git grep over
// the strings of the plan's most selective clause (one `-E` alternation of
// them, escaped: codeindex.Plan.Grep), its lines confirmed with the same
// regexp, so both paths answer RE2's matches, and the same ones.
//
// THE QUERY CROSSES AS ITS PARTS — the text, the mode, the case and the
// filters (codeindex.Query) — never as a compiled pattern: the git node makes
// the expression and its plan by the one grammar the asker validated it
// with, so a node and its asker cannot read one search two ways, and a
// refusal names what the person or the seat wrote. An estate request, never
// a record, so it moves no RecordVersion and takes no versionedFields row; as
// a STATED CONDITION (M3-O84 §2 item 5), no build that carries the code
// domain has run without it, since none of the domain is on main.

const (
	// MaxSearchHits is the most lines one search answers: about 128 bytes a
	// hit — a path, a line number, MaxSearchLineBytes of text at the most and
	// a blob's id — so a full answer's 500 fill most of MaxGitAnswerBytes'
	// 56 KiB without passing it.
	MaxSearchHits = 500
	// MaxSearchLineBytes is the most of a matching line a hit carries, cut
	// on a rune's boundary: enough to read the match in its line, and what
	// keeps a minified file's one long line from filling an answer alone.
	MaxSearchLineBytes = 256
	// MaxIndexedFileBytes is the largest file either path reads: a file past
	// it is skipped by the index (codeindex.MaxFileBytes) and dropped from
	// git grep's answer by its size, so both paths answer the same lines.
	MaxIndexedFileBytes = codeindex.MaxFileBytes
	// SearchBudget is what one search may spend reading and confirming, its
	// git included: the state log's resolve budget, the time a person waits
	// on a write's answer. A search that runs it out answers the hits it
	// found by then, Truncated — never nothing, and never late: a search's
	// cost is the corpus's, which another node would read as slowly.
	SearchBudget = statelog.DefaultResolveBudget
)

// SearchVia is the path a search was answered by. A closed set
// ([SearchVias]).
type SearchVia string

const (
	// SearchViaIndex is the repository's code index of the commit searched.
	SearchViaIndex SearchVia = "index"
	// SearchViaGrep is git grep over the commit searched.
	SearchViaGrep SearchVia = "grep"
)

// SearchVias is the closed set.
func SearchVias() []SearchVia { return []SearchVia{SearchViaIndex, SearchViaGrep} }

// Valid reports a path this build knows.
func (v SearchVia) Valid() bool { return slices.Contains(SearchVias(), v) }

// SearchQuery is one repository's code search ([GitSearch]): codeindex.Query's
// parts but its repositories, which the read's RepoID names, and how many
// hits it wants.
type SearchQuery struct {
	// Text is what is searched for, as Mode reads it (codeindex.Mode).
	Text string         `json:"text"`
	Mode codeindex.Mode `json:"mode"`
	// IgnoreCase folds case as (?i) does.
	IgnoreCase bool `json:"ignore_case,omitempty"`
	// Paths are the files searched (codeindex.PathFilter): those any one
	// matches; none is every file.
	Paths []string `json:"paths,omitempty"`
	// Ref is what is searched at: a branch, a whole ref or a commit's whole
	// id; empty is the repository's default branch — the one commit its
	// index answers for.
	Ref string `json:"ref,omitempty"`
	// Limit is the most hits answered, 1 to MaxSearchHits; 0 is
	// MaxSearchHits.
	Limit int `json:"limit,omitempty"`
}

// Query is q as codeindex reads it.
func (q SearchQuery) Query() codeindex.Query {
	return codeindex.Query{Text: q.Text, Mode: q.Mode, IgnoreCase: q.IgnoreCase, Paths: q.Paths, Ref: q.Ref}
}

// Hits is the most hits q answers.
func (q SearchQuery) Hits() int {
	if q.Limit == 0 {
		return MaxSearchHits
	}
	return q.Limit
}

// validate refuses a search the git node would refuse: codeindex's grammar
// and plan (Query.Validate, which names what to change), and a limit past
// MaxSearchHits.
func (q SearchQuery) validate() error {
	if q.Limit < 0 || q.Limit > MaxSearchHits {
		return fmt.Errorf("%w: search.limit: %d is not 1 to %d (0 is %d)", ErrInvalid, q.Limit, MaxSearchHits,
			MaxSearchHits)
	}
	if err := q.Query().Validate(); err != nil {
		return fmt.Errorf("%w: search: %w", ErrInvalid, err)
	}
	return nil
}

// SearchAnswer is a search's lines: every line of the commit searched the
// expression matches, in the order of their paths' bytes and then their
// numbers, as many as the query's limit.
type SearchAnswer struct {
	Hits []SearchHit `json:"hits"`
	// Tip is the commit searched, whole; Ref the whole ref the query's Ref
	// (or the default branch) named it by, empty where it named a commit.
	Tip string `json:"tip"`
	Ref string `json:"ref,omitempty"`
	// Via is how the commit was read.
	Via SearchVia `json:"via"`
	// SkippedFiles is how many of the commit's files, of those the paths
	// admit, the search did not read for their size (past
	// MaxIndexedFileBytes) — and, on the index path, as binaries, which git
	// grep -I passes over unsaid — plus every file a line of which was past
	// what a search runs its expression on (codeindex.Plan.LineBytes).
	SkippedFiles int `json:"skipped_files,omitempty"`
	// Truncated says the search stopped before it had read every file that
	// could match: at its limit of hits, or at SearchBudget. Hits is then
	// what it found first, in order.
	Truncated bool `json:"truncated,omitempty"`
}

// SearchHit is one matching line.
type SearchHit struct {
	// Path is the file's path (PathB64 per [WirePath]).
	Path    string `json:"path"`
	PathB64 string `json:"path_b64,omitempty"`
	// Line is the line's number, a file's first being 1.
	Line int `json:"line"`
	// Text is the line, at most MaxSearchLineBytes of it, cut on a rune's
	// boundary; invalid UTF-8 is carried as U+FFFD.
	Text string `json:"text"`
	// Blob is the id of the file's object, whole: what a file read at it
	// names, and the same at every commit holding the same bytes.
	Blob string `json:"blob"`
}
