package gitref

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// MaxPathGlobBytes is the longest path glob [ParsePathGlob] reads: 128 bytes.
//
// PAST ANY GLOB A PERSON WRITES: what one owns below a directory is its **, so
// a glob is a few components (services/payments/**/*_test.go is thirty bytes)
// and one past a hundred is a list of files written as one path. Its parts
// read into a pattern's byte offsets, which hold it.
//
// A GLOB'S BOUND, NOT A LIST'S: a repository names up to 2 048 globs
// (internal/gitlet's MaxPathOwners × MaxPathOwnerPaths) and JSON may spell
// each byte as six (`<`, `>`, `&`), so this bound alone would let them weigh
// about 1.6 MB, past the code-domain record carrying them. Their total is
// bounded there (config's CodeMaxPathOwnersBytes).
const MaxPathGlobBytes = 128

// MaxPathGlobComponents is the most components a path glob holding a '*' may
// have: eight, [MaxPatternComponents]' value, bounding the same cost: a run of
// parts BETWEEN two **s searched for along the path, here SEVEN parts (six
// between a leading ** and a trailing one, plus the * a trailing ** is read as,
// [readGlob]), two more than a pattern's. Eight is deeper than any ownership.
//
// IT BOUNDS COST A BYTE OF PATH, NOT A MATCH'S: a path has no bound. The
// costliest glob costs 3.2 ms on 48 KiB, what a revision's touched paths may
// take (internal/gitlet's MaxTouchedBytes): about 65 ns a byte, a directory's
// ** 20 ns (the package doc's Measured section). So the home, in MatchOwners,
// pays at most about 6.5 s of a core a revision at MaxPathOwners ×
// MaxPathOwnerPaths such globs, and about 42 ms for directories' **.
const MaxPathGlobComponents = MaxPatternComponents

// PathGlob is a glob over a repository's TREE PATHS, checked and read once by
// [ParsePathGlob]: how a path owner or a check names tree paths. It is
// [Pattern]'s rule over a path rather than a ref, read by the same [readGlob]
// and held, by the tests, to the same rule written out plainly and to git's
// `:(glob)` pathspec.
//
// A path is what git's tree names a file by: components below the root,
// '/'-separated, no leading or trailing '/' (`git diff-tree -r --name-only`).
// Unlike a ref, it has no bound but the tree's: [PathGlob.Match] takes any
// length. The zero PathGlob matches no path: a glob nobody parsed owns nothing.
type PathGlob struct {
	// literal is a glob holding no '*', which matches only the identical path
	// — one comparison. Empty for a glob of wildcards and for the zero
	// PathGlob.
	literal string
	// parts, head and tail are a wildcard glob's, as [Pattern]'s are.
	parts      []part
	head, tail uint8
}

// ParsePathGlob checks glob as a path glob and reads it once, answering why it
// is not one: a sentence following "<glob> is not a path glob: ".
//
// Relative to the repository's ROOT: '/'-separated components, at most
// [MaxPathGlobBytes] of valid UTF-8, none empty, each exactly ** (zero or more
// whole components, one or more as the LAST: docs/** is never a file named
// docs) or one where * is any run of bytes but '/'. A glob with a * has at most
// [MaxPathGlobComponents] components; one without matches ONLY the identical
// path. Refused, as reading as something it does not do: . or ..; ** beside
// text (a**b), which git's wildmatch reads as one *; ?, [ and \ (read as
// themselves; name such a file through *); a leading !; a control character.
func ParsePathGlob(glob string) (PathGlob, error) {
	switch {
	case glob == "":
		return PathGlob{}, errors.New("it is empty; a glob names at least one path")
	case len(glob) > MaxPathGlobBytes:
		return PathGlob{}, fmt.Errorf("it is %d bytes, past the %d a path glob may hold — a "+
			"directory's ** names what lies beneath it in fewer", len(glob), MaxPathGlobBytes)
	case !utf8.ValidString(glob):
		return PathGlob{}, errors.New("it is not valid UTF-8")
	case glob[0] == '/':
		return PathGlob{}, errors.New("it begins with /; a glob is relative to the repository's " +
			"root, so drop the leading /")
	case glob[0] == '!':
		return PathGlob{}, errors.New("it begins with !, which gitignore reads as a negation and " +
			"this grammar has none of; list the paths that are owned")
	case glob[len(glob)-1] == '/':
		return PathGlob{}, errors.New("it ends with /; a glob names files, so every file inside a " +
			"directory is dir/**")
	}
	components, wild := 0, false
	for c := range strings.SplitSeq(glob, "/") {
		components++
		if why := pathGlobComponentFault(c); why != "" {
			return PathGlob{}, errors.New(why)
		}
		wild = wild || strings.IndexByte(c, '*') >= 0
	}
	if !wild {
		return PathGlob{literal: glob}, nil
	}
	if components > MaxPathGlobComponents {
		return PathGlob{}, fmt.Errorf("it has %d components, past the %d a glob holding a * may "+
			"have — a directory's ** reaches as deep as its tree runs", components, MaxPathGlobComponents)
	}
	parts, head, tail := readGlob(glob, components, nil)
	return PathGlob{parts: parts, head: head, tail: tail}, nil
}

// pathGlobComponentFault is why c is no component of a path glob, or "" for one
// that is: the rules [ParsePathGlob] states, one component at a time.
func pathGlobComponentFault(c string) string {
	switch {
	case c == "":
		return "it holds an empty component (//); every component names something"
	case c == "." || c == "..":
		return fmt.Sprintf("it holds a %s component, which no path of a tree holds", c)
	case c == "**":
		return ""
	case strings.Contains(c, "**"):
		return fmt.Sprintf("its component %q holds ** beside other text, which matches as a single * "+
			"would; ** is a whole component (a/**/b), and * is enough within one", c)
	}
	for i := 0; i < len(c); i++ {
		switch b := c[i]; {
		case b < 0x20 || b == 0x7f:
			return fmt.Sprintf("its component %q holds a control character", c)
		case b == '?' || b == '[' || b == '\\':
			return fmt.Sprintf("its component %q holds %q, which another glob dialect reads as a "+
				"metacharacter and this one as itself; * is the one metacharacter, and names a file "+
				"holding %q too", c, string(b), string(b))
		}
	}
	return ""
}

// Match reports whether path matches the glob: every component but ** matches
// exactly one path component, * standing for any run of bytes; ** matches zero
// or more whole components, except as the glob's LAST, where it matches one or
// more. That is [Pattern.Match]'s rule; a literal glob matches only the
// identical path.
//
// THE PATH HAS NO BOUND, so what lies between the first ** and the last is
// read a component at a time, not cut onto the stack. Parts before the first
// ** and after the last are compared once at the path's ends; only a run
// between two **s is searched for along it, a run [MaxPathGlobComponents]
// bounds. It allocates nothing and does not check path: an empty component or
// leading '/' gets a meaningless answer, never a panic.
func (g PathGlob) Match(path string) bool {
	if g.parts == nil {
		return g.literal != "" && path == g.literal
	}
	parts, head, tail := g.parts, int(g.head), int(g.tail)
	// The head, from the path's start: lo is where the component after the
	// last one it took begins — past the path's end once it took the last.
	lo := 0
	for k := range head {
		if lo > len(path) {
			return false
		}
		hi := len(path)
		if i := strings.IndexByte(path[lo:], '/'); i >= 0 {
			hi = lo + i
		}
		if !parts[k].match(path[lo:hi]) {
			return false
		}
		lo = hi + 1
	}
	if head == len(parts) {
		return lo > len(path) // no **: the parts took every component, one each
	}
	// The tail, from the path's end and never into what the head took: hi is
	// where the component before the first one it took ends.
	hi := len(path)
	for k := len(parts) - 1; k >= len(parts)-tail; k-- {
		if hi < lo {
			return false
		}
		start := lo + strings.LastIndexByte(path[lo:hi], '/') + 1
		if !parts[k].match(path[start:hi]) {
			return false
		}
		hi = start - 1
	}
	middle := parts[head : len(parts)-tail]
	if len(middle) == 1 {
		return true // one **, which takes whatever lies between
	}
	if hi < lo {
		// The head and the tail took every component: nothing lies between,
		// which is no component at all rather than one empty one.
		return walkPath(middle, "", 0)
	}
	between := path[lo:hi]
	return walkPath(middle, between, len(between)+1)
}

// walkPath reports whether the components of between match parts, which begin
// and end with ** — the glob between its first ** and its last, the path
// between what its head and tail took. end is len(between)+1, or 0 where it
// holds no component: offsets name where a component BEGINS, and the one past
// the last is where none does.
//
// [walk]'s walk over offsets rather than a cut: the classic wildcard walk with
// ONE backtrack point, the last ** seen, since every other part takes exactly
// one component and a run placed at its earliest match leaves the longest
// remainder for the runs after it. At most (the components between) × (the
// longest run) comparisons.
func walkPath(parts []part, between string, end int) bool {
	i, j := 1, 0       // parts[0] is **, which takes as few components as it can
	star, mark := 1, 0 // after the last ** seen: the part after it, and where the component it took up to begins
	for j < end {
		if i < len(parts) {
			pt := &parts[i]
			switch {
			case pt.kind == partAny && i == len(parts)-1:
				return true // the last **, which takes whatever is left
			case pt.kind == partAny:
				star, mark = i+1, j
				i++
				continue
			}
			if c, next := componentFrom(between, j); pt.match(c) {
				i, j = i+1, next
				continue
			}
		}
		_, mark = componentFrom(between, mark) // the last ** takes one component more
		i, j = star, mark
	}
	for i < len(parts) && parts[i].kind == partAny {
		i++
	}
	return i == len(parts)
}

// componentFrom is the component of s that begins at offset j — at most
// len(s), where an empty last component begins — and where the one after it
// begins: len(s)+1 after the last.
func componentFrom(s string, j int) (string, int) {
	if k := strings.IndexByte(s[j:], '/'); k >= 0 {
		return s[j : j+k], j + k + 1
	}
	return s[j:], len(s) + 1
}
