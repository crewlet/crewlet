package gitref

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// MaxPathGlobBytes is the longest path glob [ParsePathGlob] reads: 128 bytes.
//
// PAST ANY GLOB A PERSON WRITES for a part of a tree: what one owns below a
// directory is the directory's **, however deep the tree runs beneath it, so a
// glob is a few components — services/payments/**/*_test.go is thirty bytes —
// and one past a hundred is a list of files written as one path. A glob's
// parts read into a pattern's byte offsets, which hold it.
//
// IT IS A GLOB'S BOUND, NOT A LIST'S: a repository names up to 2 048 globs
// (internal/code's MaxPathOwners owners of MaxPathOwnerPaths each), and JSON
// may spell each byte of a glob as six (`<`, `>` and `&` are a path's like any
// other's), so at this bound alone they would weigh about 1.6 MB — past the
// one record of the code domain every change of a repository's settings
// carries them in. What a repository's path owners weigh is bounded as a whole,
// as the record carries them (config's CodeMaxPathOwnersBytes).
const MaxPathGlobBytes = 128

// MaxPathGlobComponents is the most components a path glob holding a '*' may
// have: [MaxPatternComponents], for its reason. A glob is matched by the same
// walk a pattern is, so the one cost it can make large is the same — a run of
// parts BETWEEN two **s, searched for along the path — and eight bounds that
// run at six parts. Eight is deeper than any ownership a glob states: what one
// owns below a directory is the directory's **, however deep the tree runs
// under it. A literal glob is one comparison of the whole path, so it is
// bounded only by its bytes.
const MaxPathGlobComponents = MaxPatternComponents

// PathGlob is a glob over a repository's TREE PATHS, checked and read once by
// [ParsePathGlob]: the rule a path owner names its part of the tree by, and a
// check names the paths it runs for. It is [Pattern]'s rule over a path rather
// than a ref — '/'-separated components, * within one component, ** whole
// components — read by the same reading of a glob ([readGlob]) and held, by the
// tests, to the same rule written out plainly and to git's own glob pathspec,
// which is how git itself reads `:(glob)` against the paths of a tree.
//
// A path is what git's tree names a file by: its components below the
// repository's root, '/'-separated, with no leading or trailing '/' — what
// `git diff-tree -r --name-only` prints. Unlike a ref, a path has no bound but
// the tree's: [PathGlob.Match] answers one of any length.
//
// The zero PathGlob matches no path: a glob nobody parsed owns nothing.
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
// is not one otherwise — a sentence that follows "<glob> is not a path glob: ",
// which is how every caller reports it.
//
// A path glob is relative to the repository's ROOT: one or more '/'-separated
// components, at most [MaxPathGlobBytes] of valid UTF-8 in all, no leading or
// trailing '/' and no empty component, where every component is either exactly
// ** — whole components: zero or more, and one or more as the glob's LAST
// component, so docs/** is every path inside docs and never a file named docs —
// or a component in which * stands for any run of bytes but '/'. A glob holding
// a * has at most [MaxPathGlobComponents] components. A glob holding none
// matches ONLY the identical path. So *.go is the Go files at the root alone,
// and **/*.go every Go file of the tree.
//
// What it refuses beside that, each because the glob would read as something it
// does not do:
//
//   - a component . or .., which no tree path holds (git refuses both);
//   - ** beside other text in one component (a**b), which git's wildmatch reads
//     as a single * — a glob that looks as if it reaches across components and
//     does not owns less than its author meant;
//   - ?, [ and \, the other metacharacters of every glob dialect a reader
//     knows (fnmatch's, gitignore's, a CODEOWNERS file's), which this grammar
//     would read as themselves: * is its one metacharacter, and a file whose
//     name holds one of them is named through *;
//   - a leading !, which gitignore reads as a negation and this grammar has
//     none of: a path owner lists what it owns;
//   - a control character, which a glob written in a document holds only by
//     mistake.
//
// It allocates the glob's parts once, and nothing for a literal.
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

// Match reports whether path matches the glob: every glob component but **
// matches exactly one path component, * in it standing for any run of bytes,
// and ** matches whole components — zero or more of them, except as the LAST
// component of the glob, where it matches one or more. That is
// [Pattern.Match]'s rule, component for component, and a literal glob matches
// only the identical path, as a literal pattern does.
//
// THE PATH HAS NO BOUND, where a ref's is 255 bytes, so the walk is not a
// pattern's: what lies between the glob's first ** and its last is not cut
// onto the stack once, as a ref's is — a path past what that cut holds is one
// a tree may have — but read a component at a time as the walk reaches it,
// each one's end found where it begins. What can absorb no component is
// compared once, as for a pattern: the parts before the first ** at the
// path's start and after the last at its end, so a glob with at most one **
// costs one comparison a part however deep the path, and only a run of parts
// between two **s is searched for along it — a run [MaxPathGlobComponents]
// bounds. It allocates nothing.
//
// Match does not check path: it is for a caller holding a tree's path. Its
// answer for anything else — an empty component, a leading '/' — means
// nothing, but it is an answer and never a panic.
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
// between what its head and its tail took. end is one past where between's
// last component ends, len(between)+1, or 0 where it holds no component at
// all: the walk's offsets name where a component BEGINS, and the one past the
// last is where none does.
//
// [walk]'s walk, over offsets rather than a cut: the classic wildcard walk over
// components, with ONE backtrack point, the last ** seen — every other part
// takes exactly one component, so a run placed at its earliest match leaves the
// longest remainder for the runs after it. It takes at most (the components
// between) × (the longest run) comparisons, each finding its component's end
// as it reads it.
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
