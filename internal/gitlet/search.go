package gitlet

import (
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/codeindex"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A CODE SEARCH OF ONE REPOSITORY ([GitSearch], M6-D12, M6-D13) is a git
// read like any other: the keyed walk asks the repository's Gitlet nodes in
// their order, and the one that answers judges the asker's grant from its own
// chart. What differs is how it reads. A `search: indexed` repository's
// default branch is read through the trigram index its first [IndexHolders]
// Gitlet nodes keep of its tip (internal/gitlet/cache's DerivedCodeIndex): the
// plan's candidates among the tip's files, confirmed line by line with Go's
// regexp. Everything else — a `search: grep` repository, a `ref:` other than
// the indexed tip, a node that holds no index — is read with git grep over
// the strings of the plan's most selective clause (one `-E` alternation of
// them, escaped: codeindex.Plan.Grep), its lines confirmed with the same
// regexp, so both paths answer RE2's matches, and the same ones.
//
// THE QUERY CROSSES AS ITS PARTS — the text, the mode, the case and the
// filters (codeindex.Query) — never as a compiled pattern: the Gitlet node makes
// the expression and its plan by the one grammar the asker validated it
// with, so a node and its asker cannot read one search two ways, and a
// refusal names what the person or the seat wrote. An estate request, never
// a record, so it moves no RecordVersion and takes no versionedFields row; the
// domain landed on main with it.

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
	// MaxSearchContext is the most lines a search answers on each side of a
	// hit: the screen draws three, and each costs a hit MaxSearchLineBytes more.
	MaxSearchContext = 3
	// MaxSearchContextHits is the most hits a search asking context answers:
	// with six lines more each, 100 weigh at worst what MaxSearchHits bare hits do.
	MaxSearchContextHits = 100
	// MaxSearchHitMatches is the most matches a hit names: past that many in
	// MaxSearchLineBytes, a highlight reads as the whole line.
	MaxSearchHitMatches = 32
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
	// Context is how many lines of the file on each side of a hit it answers,
	// 0 to MaxSearchContext; 0 is none. Near hits share them ([SearchHit.Before]).
	Context int `json:"context,omitempty"`
	// PerFile is the most hits answered of one file, 0 to MaxSearchHits; 0 is
	// every one. Set, each hit names its file's total ([SearchHit.FileMatches]).
	PerFile int `json:"per_file,omitempty"`
	// After, where set, is the place in the search's order the answer
	// continues from: only lines AFTER it are answered, so Limit counts the
	// lines past it alone ([SearchAfter]). The domain landed on main with it,
	// as with the rest of this request.
	After *SearchAfter `json:"after,omitempty"`
}

// SearchAfter is a place in one repository's search order — its paths'
// bytes, then their numbers — named as the hit sent last names it: the path
// as itself or, where it is not UTF-8, by PathB64 alone ([GitPath]'s pair),
// and the line. A search continued after it reads no file whose path sorts
// before the path, and answers no line of the path itself at or before the
// line, so a page of a long search costs the lines from its place on and
// never again the lines an earlier page answered ([SearchAfterOf]).
type SearchAfter struct {
	Path    string `json:"path,omitempty"`
	PathB64 string `json:"path_b64,omitempty"`
	Line    int    `json:"line"`
}

// SearchAfterOf is the place of h: what a search continued after h names.
func SearchAfterOf(h SearchHit) SearchAfter {
	if h.PathB64 != "" {
		return SearchAfter{PathB64: h.PathB64, Line: h.Line}
	}
	return SearchAfter{Path: h.Path, Line: h.Line}
}

// Raw is a's path as its exact bytes, or [ErrInvalid] naming what is wrong
// with the place: a path no tree entry can be named by, or a line before a
// file's first.
func (a SearchAfter) Raw() (string, error) {
	if a.Line < 1 {
		return "", fmt.Errorf("%w: search.after.line: %d is before a file's first line, 1", ErrInvalid, a.Line)
	}
	return GitPath("search.after.path", a.Path, a.PathB64, false)
}

// Query is q as codeindex reads it.
func (q SearchQuery) Query() codeindex.Query {
	return codeindex.Query{Text: q.Text, Mode: q.Mode, IgnoreCase: q.IgnoreCase, Paths: q.Paths, Ref: q.Ref}
}

// Hits is the most hits q answers: its Limit, or where it names none,
// MaxSearchContextHits for a search asking context and MaxSearchHits for another.
func (q SearchQuery) Hits() int {
	switch {
	case q.Limit != 0:
		return q.Limit
	case q.Context > 0:
		return MaxSearchContextHits
	}
	return MaxSearchHits
}

// ValidateBounds refuses a limit, a context or a per-file bound past what a
// search answers ([ErrInvalid], naming the field): what every surface that
// builds a search checks first.
func (q SearchQuery) ValidateBounds() error {
	switch {
	case q.Limit < 0 || q.Limit > MaxSearchHits:
		return fmt.Errorf("%w: search.limit: %d is not 1 to %d (0 is %d)", ErrInvalid, q.Limit, MaxSearchHits,
			MaxSearchHits)
	case q.Context < 0 || q.Context > MaxSearchContext:
		return fmt.Errorf("%w: search.context: %d is not 0 to %d lines", ErrInvalid, q.Context, MaxSearchContext)
	case q.PerFile < 0 || q.PerFile > MaxSearchHits:
		return fmt.Errorf("%w: search.per_file: %d is not 0 to %d (0 is every matching line)", ErrInvalid,
			q.PerFile, MaxSearchHits)
	case q.Context > 0 && q.Limit > MaxSearchContextHits:
		return fmt.Errorf("%w: search.limit: %d is past the %d hits a search answering context lines may",
			ErrInvalid, q.Limit, MaxSearchContextHits)
	}
	return nil
}

// validate refuses a search the Gitlet node would refuse: a bound past what
// a search answers (ValidateBounds), codeindex's grammar and plan
// (Query.Validate, which names what to change), and a place to continue
// after that names no line.
func (q SearchQuery) validate() error {
	if err := q.ValidateBounds(); err != nil {
		return err
	}
	if err := q.Query().Validate(); err != nil {
		return fmt.Errorf("%w: search: %w", ErrInvalid, err)
	}
	if q.After != nil {
		if _, err := q.After.Raw(); err != nil {
			return err
		}
	}
	return nil
}

// SearchAnswer is a search's lines: every line of the commit searched the
// expression matches — after the query's After, where it names one — in the
// order of their paths' bytes and then their numbers, as many as the query's
// limit; each hit with where it matched, and the lines around it and its
// file's total where the query asked them. A search that stops early stops in
// that order, so its hits are always the FIRST lines of what it searched:
// every line before its last is among them.
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
	// Matches are where the expression matched in Text: byte offsets
	// [start, end) into Text as answered, in order, none empty or past Text's
	// cut, at most MaxSearchHitMatches. Absent is none answered.
	Matches [][2]int `json:"matches,omitempty"`
	// Before and After are the file's lines just before and after this one,
	// the query's Context of each at most, each cut as Text is. A line a nearer
	// hit of the same answer carries is not carried again, so Before starts
	// after the previous hit's After and After ends before the file's next hit.
	Before []string `json:"before,omitempty"`
	After  []string `json:"after,omitempty"`
	// FileMatches is how many lines of the file matched, where the query
	// asked PerFile and the search read the whole file; absent otherwise.
	FileMatches int `json:"file_matches,omitempty"`
}
