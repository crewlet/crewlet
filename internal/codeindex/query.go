package codeindex

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/gitref"
)

// MaxQueryBytes bounds a code search's text, in bytes, measured with the
// whitespace around it trimmed; a longer one is REFUSED, never cut. It is
// knowledge search's bound (knowledge.MaxQueryBytes, which a test holds it
// to): four hundred bytes is a long line of code and several names, and past
// it a query is a pasted file, which a search on part of would answer as a
// different question. Declared here rather than taken from there, because
// this package is pure and the knowledge seam is not.
const MaxQueryBytes = 400

// Mode is how a query's text is read.
type Mode string

const (
	// ModeLiteral is the text exactly, anywhere in a line.
	ModeLiteral Mode = "literal"
	// ModeRegex is the text as an RE2 expression (Go's regexp), each line
	// matched on its own: ^ and $ are a line's start and end.
	ModeRegex Mode = "regex"
	// ModeWord is the text exactly, not inside a longer word: where the text
	// begins or ends with a word character, no word character may stand
	// beside it there. A word character is RE2's, ASCII [0-9A-Za-z_].
	ModeWord Mode = "word"
)

// Modes is every Mode.
var Modes = []Mode{ModeLiteral, ModeRegex, ModeWord}

// Valid reports whether m is one of Modes.
func (m Mode) Valid() bool { return slices.Contains(Modes, m) }

// Query is a code search: the text, how it is read, and its filters.
//
// THE GRAMMAR IS ONE, whoever carries the parts: ParseQuery reads the filters
// a person or a seat writes before the text, and a surface that carries the
// mode, the case or a filter beside the text — a tool's argument, a URL's
// parameter — sets it on the Query and holds the whole to Validate, which
// judges each part by the rule ParseQuery reads it with.
type Query struct {
	// Text is what is searched for, as Mode reads it.
	Text string
	// Mode is how Text is read.
	Mode Mode
	// IgnoreCase folds case as (?i) does: Unicode's simple folding.
	IgnoreCase bool
	// Repos are the repositories searched (`repo:`): any of them; none is
	// every repository the asker may read.
	Repos []string
	// Paths are the files searched (`path:`, see PathFilter): those any one
	// matches; none is every file.
	Paths []string
	// Ref is what is searched at (`ref:`): a branch, a whole ref or a
	// commit's id; empty is the repository's default branch.
	Ref string
}

// The filters ParseQuery reads, each followed by its value with no space.
const (
	filterRepo = "repo:"
	filterPath = "path:"
	filterRef  = "ref:"
)

// ParseQuery reads a code search as a person or a seat writes it: FILTERS
// first — `repo:<name>`, `path:<pattern>` and `ref:<ref>`, each a word of its
// own — then the text searched for, which is everything after them, inner
// spaces and all. Repeated, repo: and path: each widen the search (any of
// them); ref: names one ref. A filter word with nothing after its colon is
// text, as is one after the text has begun: the filters LEAD, so what follows
// them is searched for exactly as written. A text that itself begins with
// one of the filter words is searched in regex mode, where a backslash before
// its colon keeps it text.
//
// The query is in ModeLiteral, case-sensitive; a surface that carries a mode
// or the case beside the text sets them and holds the query to Validate.
// ParseQuery refuses — each a *QueryError — a query past MaxQueryBytes, one
// that is not UTF-8, one with no text after its filters, and a filter its
// grammar refuses.
func ParseQuery(text string) (Query, error) {
	trimmed := strings.TrimSpace(text)
	if len(trimmed) > MaxQueryBytes {
		return Query{}, refuse(ErrQueryTooLong, "a code search takes at most %d bytes and this one is %d: "+
			"search for a line's distinctive part, not a pasted passage", MaxQueryBytes, len(trimmed))
	}
	q := Query{Mode: ModeLiteral}
	rest := trimmed
	for {
		word, after := rest, ""
		if i := strings.IndexFunc(rest, unicode.IsSpace); i >= 0 {
			word, after = rest[:i], rest[i:]
		}
		key, value, ok := filterOf(word)
		if !ok {
			break
		}
		switch key {
		case filterRepo:
			if !slices.Contains(q.Repos, value) {
				q.Repos = append(q.Repos, value)
			}
		case filterPath:
			if !slices.Contains(q.Paths, value) {
				q.Paths = append(q.Paths, value)
			}
		case filterRef:
			if q.Ref != "" && q.Ref != value {
				return Query{}, refuse(ErrBadQuery, "ref:%s and ref:%s: a search reads one ref", q.Ref, value)
			}
			q.Ref = value
		}
		rest = strings.TrimLeftFunc(after, unicode.IsSpace)
	}
	q.Text = rest
	if err := q.check(); err != nil {
		return Query{}, err
	}
	return q, nil
}

// Beside adds the filters a surface carries BESIDE the text — a tool's
// argument, a URL's parameter, a screen's control — by the rule ParseQuery
// reads the same filters in the text by, so a search is one search however
// its filters arrive: a repository or a path widens it as one more repo: or
// path: does, named once however often it is named; and a ref is the one ref
// it reads, so one beside a different ref: is refused, as two ref: filters
// are. An empty value adds nothing. Each refusal is a *QueryError; the
// filters' own grammar is Validate's, after.
//
// ONE RULE, ON THE GRAMMAR'S SIDE, because two surfaces carry these filters —
// the `code_search` question's parameters and the seat tool's arguments —
// and a rule each surface wrote for itself would let one search read two ways.
func (q *Query) Beside(repo, path, ref string) error {
	repo, path, ref = strings.TrimSpace(repo), strings.TrimSpace(path), strings.TrimSpace(ref)
	if ref != "" {
		if q.Ref != "" && q.Ref != ref {
			return refuse(ErrBadQuery, "ref %s beside ref:%s: a search reads one ref", ref, q.Ref)
		}
		q.Ref = ref
	}
	if repo != "" && !slices.Contains(q.Repos, repo) {
		q.Repos = append(q.Repos, repo)
	}
	if path != "" && !slices.Contains(q.Paths, path) {
		q.Paths = append(q.Paths, path)
	}
	return nil
}

// filterOf is word as a filter: its key and value, or false where it is none
// — not one of the three words, or nothing after the colon.
func filterOf(word string) (key, value string, ok bool) {
	for _, key := range []string{filterRepo, filterPath, filterRef} {
		if value, found := strings.CutPrefix(word, key); found && value != "" {
			return key, value, true
		}
	}
	return "", "", false
}

// Validate holds q to the grammar: what ParseQuery refuses, a mode that is
// none of Modes, a regex that does not compile — and a pattern no plan can be
// made of (NewPlan's refusals), so a search a serving node would refuse is
// refused where it is asked. Each refusal is a *QueryError.
func (q Query) Validate() error {
	_, err := q.Plan()
	return err
}

// check is Validate but for the expression: the text, the mode and the
// filters.
func (q Query) check() error {
	switch {
	case !utf8.ValidString(q.Text):
		return refuse(ErrBadQuery, "a code search is UTF-8 text")
	case strings.TrimSpace(q.Text) == "":
		return refuse(ErrBadQuery, "a code search needs text to look for after its filters")
	case len(q.Text) > MaxQueryBytes:
		return refuse(ErrQueryTooLong, "a code search takes at most %d bytes and this one's text is %d: "+
			"search for a line's distinctive part, not a pasted passage", MaxQueryBytes, len(q.Text))
	case !q.Mode.Valid():
		return refuse(ErrBadQuery, "mode %q is none of %v", q.Mode, Modes)
	}
	for _, repo := range q.Repos {
		if !gitref.ValidRepoName(repo) {
			return refuse(ErrBadQuery, "repo:%s is not a repository name: one is lowercase letters, digits "+
				"and hyphens, at most %d bytes", repo, gitref.MaxRepoNameBytes)
		}
	}
	if _, err := NewPathFilter(q.Paths); err != nil {
		return err
	}
	if q.Ref != "" && !gitref.ValidOID(q.Ref) && !gitref.ValidRef(q.Ref) && !gitref.ValidBranch(q.Ref) {
		return refuse(ErrBadQuery, "ref:%s is not a ref: name a branch (main), a whole ref (refs/tags/v1) "+
			"or a commit's %d-digit id", q.Ref, gitref.OIDHexLen)
	}
	return nil
}

// Regexp is q's text as the expression its mode makes of it, its case folded
// where IgnoreCase says so — what every path confirms a line with.
func (q Query) Regexp() (*regexp.Regexp, error) {
	if err := q.check(); err != nil {
		return nil, err
	}
	re, err := regexp.Compile(q.pattern())
	if err != nil {
		return nil, refuse(ErrBadQuery, "the regex %q does not compile: %v", q.Text, err)
	}
	return re, nil
}

// Plan is q's plan over the index (NewPlan), its refusals naming q's text.
func (q Query) Plan() (*Plan, error) {
	re, err := q.Regexp()
	if err != nil {
		return nil, err
	}
	what := fmt.Sprintf("the %s %q", map[Mode]string{ModeLiteral: "text", ModeWord: "word",
		ModeRegex: "regex"}[q.Mode], q.Text)
	return newPlan(re, what)
}

// pattern is the RE2 expression q's mode makes of its text — under (?m), so
// that ^ and $ are a line's edges as a code search reads them, which is what
// each line matched on its own makes them anyway; the flag says so to
// regexlit.LineLocal, which judges a pattern by the whole text it could be
// run on.
func (q Query) pattern() string {
	flags := "(?m)"
	if q.IgnoreCase {
		flags = "(?mi)"
	}
	switch q.Mode {
	case ModeRegex:
		return flags + q.Text
	case ModeWord:
		body := regexp.QuoteMeta(q.Text)
		if r, _ := utf8.DecodeRuneInString(q.Text); wordRune(r) {
			body = `\b` + body
		}
		if r, _ := utf8.DecodeLastRuneInString(q.Text); wordRune(r) {
			body += `\b`
		}
		return flags + body
	}
	return flags + regexp.QuoteMeta(q.Text)
}

// wordRune reports whether r is a word character as RE2's \b reads one.
func wordRune(r rune) bool {
	return r == '_' || 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9'
}

// PathFilter is a search's path: filters, matched against a file's path from
// the repository's root as git names it ("internal/code/doc.go"). Its zero
// value matches every path. A pattern reads as a .gitignore line does,
// without its negation, escapes and classes:
//
//   - a component is matched by a glob in which * is any run of characters
//     and ? one character, neither ever a /; every other character is itself;
//   - ** as a whole component is any number of components, none included;
//   - a pattern that matches a directory matches every file below it, so
//     internal/code is that directory's every file (and a file of that name);
//   - a pattern with no / but at its end matches at any depth: *.go is every
//     Go file, testdata every file under any directory of that name;
//   - a leading / anchors such a pattern at the root (/Makefile), and any /
//     inside a pattern anchors it there too (cmd/crewlet);
//   - a trailing / matches directories only (docs/).
//
// A file matches the filter when any one pattern matches it.
type PathFilter struct {
	patterns []pathPattern
}

// pathPattern is one parsed path: pattern.
type pathPattern struct {
	// parts are its components; "**" is any number of path components.
	parts []string
	// dirOnly is a trailing /: it matches a directory, so a file below
	// one, and never a file of its own name.
	dirOnly bool
}

// NewPathFilter parses patterns, refusing — a *QueryError — one the grammar
// does not read: empty, not UTF-8, holding a NUL, past MaxQueryBytes, with an
// empty component (a//b), a . or .. component, or ** inside a component.
func NewPathFilter(patterns []string) (PathFilter, error) {
	var f PathFilter
	for _, p := range patterns {
		pp, err := parsePathPattern(p)
		if err != nil {
			return PathFilter{}, err
		}
		f.patterns = append(f.patterns, pp)
	}
	return f, nil
}

// parsePathPattern is one pattern of a path filter.
func parsePathPattern(p string) (pathPattern, error) {
	bad := func(why string) (pathPattern, error) {
		return pathPattern{}, refuse(ErrBadQuery, "path:%s is not a path pattern: %s", p, why)
	}
	switch {
	case p == "":
		return bad("it is empty")
	case len(p) > MaxQueryBytes:
		return bad(fmt.Sprintf("it is %d bytes, past the %d a search takes", len(p), MaxQueryBytes))
	case !utf8.ValidString(p) || strings.IndexByte(p, 0) >= 0:
		return bad("a pattern is UTF-8 text with no NUL")
	}
	anchored := strings.HasPrefix(p, "/")
	body := strings.TrimPrefix(p, "/")
	dirOnly := strings.HasSuffix(body, "/")
	body = strings.TrimSuffix(body, "/")
	if body == "" {
		return bad("it names no file or directory")
	}
	parts := strings.Split(body, "/")
	for _, part := range parts {
		switch {
		case part == "":
			return bad("it has an empty component (//)")
		case part == "." || part == "..":
			return bad("a path is named from the repository's root, with no . or .. in it")
		case part != "**" && strings.Contains(part, "**"):
			return bad("** stands for whole directories: write it as a component of its own, as in a/**/b")
		}
	}
	if !anchored && len(parts) == 1 && parts[0] != "**" {
		parts = []string{"**", parts[0]}
	}
	return pathPattern{parts: parts, dirOnly: dirOnly}, nil
}

// PathFilter is q's path: filters, parsed.
func (q Query) PathFilter() (PathFilter, error) { return NewPathFilter(q.Paths) }

// Match reports whether path is a file the filter keeps: any pattern's, or
// every file where it has none.
func (f PathFilter) Match(path string) bool {
	if len(f.patterns) == 0 {
		return true
	}
	comps := strings.Split(path, "/")
	for _, p := range f.patterns {
		if p.match(comps) {
			return true
		}
	}
	return false
}

// match reports whether p matches the path of components comps: the whole of
// it, or — p naming a directory — a leading run of its components, and only
// such a run where p is dirOnly.
func (p pathPattern) match(comps []string) bool {
	// reach[j]: the parts so far can match comps[:j].
	reach := make([]bool, len(comps)+1)
	next := make([]bool, len(comps)+1)
	reach[0] = true
	for _, part := range p.parts {
		clear(next)
		if part == "**" {
			seen := false
			for j := range reach {
				seen = seen || reach[j]
				next[j] = seen
			}
		} else {
			for j, c := range comps {
				if reach[j] && globMatch(part, c) {
					next[j+1] = true
				}
			}
		}
		reach, next = next, reach
	}
	last := len(comps)
	if p.dirOnly {
		last--
	}
	for k := 1; k <= last; k++ {
		if reach[k] {
			return true
		}
	}
	return false
}

// globMatch reports whether name — one component — matches pattern, in which
// * is any run of characters and ? one character, rune by rune.
func globMatch(pattern, name string) bool {
	px, nx := 0, 0
	// Where to resume after the last *: the pattern past it, and the name
	// one character on from where it last stood.
	starPx, starNx := -1, -1
	for px < len(pattern) || nx < len(name) {
		if px < len(pattern) {
			switch c := pattern[px]; c {
			case '?':
				if nx < len(name) {
					_, w := utf8.DecodeRuneInString(name[nx:])
					px, nx = px+1, nx+w
					continue
				}
			case '*':
				starPx, starNx = px, nx
				if nx < len(name) {
					_, w := utf8.DecodeRuneInString(name[nx:])
					starNx = nx + w
				} else {
					starNx = len(name) + 1
				}
				px++
				continue
			default:
				if nx < len(name) && name[nx] == c {
					px, nx = px+1, nx+1
					continue
				}
			}
		}
		if starPx >= 0 && starNx <= len(name) {
			px, nx = starPx, starNx
			continue
		}
		return false
	}
	return true
}
