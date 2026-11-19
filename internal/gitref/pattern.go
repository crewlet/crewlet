package gitref

import (
	"strings"
	"unicode/utf8"
)

// patternNamespaces are the namespaces a protection pattern may name: a
// protection guards a BRANCH or a TAG, and a pattern over any other namespace
// (refs/notes/, refs/remotes/) is not one a repository declares.
var patternNamespaces = [...]string{"refs/heads/", "refs/tags/"}

// MaxPatternComponents is the most components a GLOB pattern — one holding a
// '*' — may have, refs and its namespace included.
//
// It bounds the cost of asking a glob about a ref: a run of parts BETWEEN two
// **s is searched for along the ref at every place it could start, and a
// 255-byte ref offers about as many piece searches at any component width, so
// the worst case moves with the run's length. Eight is refs, the namespace and
// six more: a run of at most four parts (five with the * a trailing ** stands
// for). It is deeper than any branch convention (refs/heads/team/*/release/**
// is six) and holds the costliest rule to the package doc's worst case, 13 µs;
// sixteen let one cost 27 µs, the byte bound alone (fifty-nine globs) 85 µs.
const MaxPatternComponents = 8

// Pattern is a protection pattern, checked and read once by [ParsePattern] —
// the form a caller asking one rule about many refs holds, so each ref costs
// the walk and nothing else: the pattern is not checked again, its components
// are not cut again, and nothing is allocated. A push's decide asks every rule
// of its repository about every command, on the repository's one committer
// and again on every compare-and-set round, and the git-serving node asks the
// same before it uploads, so the pattern's own work belongs to the rule
// rather than to each ref it is asked about (the package doc's figures).
//
// The zero Pattern matches no ref: a rule nobody parsed protects nothing.
type Pattern struct {
	// literal is a pattern holding no '*', which matches only the identical
	// ref — one comparison. Empty for a glob and for the zero Pattern.
	literal string
	// parts are a glob's components, each read once, with a trailing ** read
	// already as the one * and the ** it means (see [Pattern.Match]).
	parts []part
	// head is how many parts come before the first ** — all of them when
	// there is none — and tail how many come after the last: [Pattern.Match]
	// compares those once each, at the ref's start and at its end.
	head, tail uint8
}

// part is one component of a glob pattern, with what it matches decided once.
type part struct {
	kind partKind
	// pre and suf are a glob's: how many bytes come before its first * and
	// after its last, so a comparison cuts nothing it can know in advance.
	pre, suf uint8
	text     string // a literal's text, or a glob's
	// mid is a glob's text between its first * and its last: empty for a
	// glob of one *, and pieces separated by single *s for more — never
	// empty then, since ParsePattern refused ** inside a component.
	mid string
}

type partKind uint8

const (
	partLiteral partKind = iota // exactly text
	partGlob                    // text with each * read as any run of bytes
	partOne                     // *: any one component
	partAny                     // **: zero or more whole components
)

// ParsePattern checks pattern as a protection pattern and reads it once,
// reporting false (and the zero Pattern) otherwise. A pattern is refs/heads/
// or refs/tags/ then components, at most MaxRefNameBytes in all,
// each ** or a glob where * is any run of bytes but '/'; one holding a * has
// at most MaxPatternComponents components.
//
// A glob component is valid when non-empty, holding no **, and a valid ref
// component with each * read as an ordinary byte, so every pattern matches
// some ref that can exist (refs/heads/*.lock, refs/heads/.* and
// refs/heads/a..* match none and are refused), and holding none of ? [ \.
// ** inside a component (a**b) is refused, not read as * as git's wildmatch
// would. The check is [refFormat] with * admitted, as ValidRef's is.
func ParsePattern(pattern string) (Pattern, bool) {
	return parsePattern(pattern, nil)
}

// parsePattern is ParsePattern reading a glob's parts into buf when they fit
// its capacity, and into a slice of their own when they do not. A glob has at
// most MaxPatternComponents parts and one more for its trailing ** to become
// two, so a buf of that capacity always holds them.
func parsePattern(pattern string, buf []part) (Pattern, bool) {
	if len(pattern) > MaxRefNameBytes || !utf8.ValidString(pattern) {
		return Pattern{}, false
	}
	for _, ns := range patternNamespaces {
		rest, ok := strings.CutPrefix(pattern, ns)
		if !ok {
			continue
		}
		// An empty rest is an empty last component, which refFormat refuses.
		components, ok := refFormat(pattern, true)
		if !ok {
			return Pattern{}, false
		}
		if strings.IndexByte(rest, '*') < 0 {
			return Pattern{literal: pattern}, true
		}
		if components > MaxPatternComponents {
			return Pattern{}, false
		}
		parts, head, tail := readGlob(pattern, components, buf)
		return Pattern{parts: parts, head: head, tail: tail}, true
	}
	return Pattern{}, false
}

// readGlob reads glob — checked already, of components components, holding a
// * — into its parts, in buf where they fit its capacity: each component once
// ([partOf]), a trailing ** read as the one * and the ** it means, and how many
// parts come before the first ** (head; all of them where there is none) and
// after the last (tail).
//
// THE ONE READING of a glob over '/'-separated names, a protection pattern's
// over a ref and a path glob's over a tree's path ([ParsePathGlob]), so the two
// cannot read one glob two ways — above all the trailing **, which matches one
// component or more, never the prefix before it.
func readGlob(glob string, components int, buf []part) (parts []part, head, tail uint8) {
	parts = buf[:0]
	if cap(parts) < components+1 {
		parts = make([]part, 0, components+1)
	}
	for c := range strings.SplitSeq(glob, "/") {
		parts = append(parts, partOf(c))
	}
	if last := len(parts) - 1; parts[last].kind == partAny {
		parts = append(parts[:last], part{kind: partOne}, part{kind: partAny})
	}
	head = uint8(len(parts))
	for k, pt := range parts {
		if pt.kind != partAny {
			continue
		}
		if int(head) == len(parts) {
			head = uint8(k)
		}
		tail = uint8(len(parts) - 1 - k)
	}
	return parts, head, tail
}

// partOf reads one component of a valid pattern.
func partOf(c string) part {
	switch {
	case c == "**":
		return part{kind: partAny}
	case c == "*":
		return part{kind: partOne}
	}
	first := strings.IndexByte(c, '*')
	if first < 0 {
		return part{kind: partLiteral, text: c}
	}
	last := strings.LastIndexByte(c, '*')
	pt := part{kind: partGlob, text: c, pre: uint8(first), suf: uint8(len(c) - 1 - last)}
	if last > first {
		pt.mid = c[first+1 : last]
	}
	return pt
}

// ValidPattern reports whether pattern is a protection pattern, as
// [ParsePattern] states the rule.
func ValidPattern(pattern string) bool {
	_, ok := ParsePattern(pattern)
	return ok
}

// MatchPattern reports whether ref matches the protection pattern. It is
// false unless ValidPattern(pattern) and ValidRef(ref): a rule that is not a
// pattern protects nothing, and a ref that is not a ref is refused before any
// rule is asked about it. It is the one-off form, checking both and parsing
// the pattern on every call — on its own stack, which holds any pattern's
// parts, so it allocates nothing; a caller asking one rule about many refs
// parses it once and asks [Pattern.Match], which is this rule exactly.
func MatchPattern(pattern, ref string) bool {
	var buf [MaxPatternComponents + 1]part
	p, ok := parsePattern(pattern, buf[:0])
	return ok && ValidRef(ref) && p.Match(ref)
}

// Match reports whether ref matches the pattern, both read as '/'-separated
// components. Every pattern component but ** matches exactly one ref
// component, * standing for any run of bytes; ** matches zero or more whole
// components, except as the pattern's LAST, where it matches one or more:
// everything INSIDE the prefix, never the prefix itself. So refs/heads/*
// matches refs/heads/main but not refs/heads/x/y, and refs/heads/release/**
// matches refs/heads/release/1.0 but not a branch named release. That is git's
// for-each-ref, which the tests hold it to, except that a LITERAL pattern
// matches only the identical ref (for-each-ref also matches a prefix up to
// '/'): a protection on refs/heads/main must not protect refs/heads/main/x.
// Cost: the package doc. Match does not check ref ([MatchPattern] does).
func (p Pattern) Match(ref string) bool {
	if p.parts == nil {
		return p.literal != "" && ref == p.literal
	}
	// For a caller holding a ValidRef: the answer for any other string means
	// nothing but is never a panic, and one longer than MaxRefNameBytes matches
	// nothing, since the cut between the first ** and the last has room for no
	// more (TestMatchAnswersFalseForWhatIsNoRef).
	if len(ref) > MaxRefNameBytes {
		return false
	}
	parts, head, tail := p.parts, int(p.head), int(p.tail)
	// The head, from the ref's start: lo is where the component after the
	// last one it took begins — past the ref's end once it took the last.
	lo := 0
	for k := range head {
		if lo > len(ref) {
			return false
		}
		hi := len(ref)
		if i := strings.IndexByte(ref[lo:], '/'); i >= 0 {
			hi = lo + i
		}
		if !parts[k].match(ref[lo:hi]) {
			return false
		}
		lo = hi + 1
	}
	if head == len(parts) {
		return lo > len(ref) // no **: the parts took every component, one each
	}
	// The tail, from the ref's end and never into what the head took: hi is
	// where the component before the first one it took ends.
	hi := len(ref)
	for k := len(parts) - 1; k >= len(parts)-tail; k-- {
		if hi < lo {
			return false
		}
		start := lo + strings.LastIndexByte(ref[lo:hi], '/') + 1
		if !parts[k].match(ref[start:hi]) {
			return false
		}
		hi = start - 1
	}
	middle := parts[head : len(parts)-tail]
	if len(middle) == 1 {
		return true // one **, which takes whatever lies between
	}
	// Runs of parts lie between two **s: what lies between the head and the
	// tail is cut once, on the stack — it is shorter than a ref of
	// MaxRefNameBytes, so it has at most that many components and one more
	// (all of them empty, which no ValidRef has, but Match must not fail on
	// it), and every offset fits in a byte.
	var ends [MaxRefNameBytes + 1]uint8
	n := 0
	between := ""
	if hi >= lo {
		between = ref[lo:hi]
		for i := 0; i < len(between); i++ {
			if between[i] == '/' {
				ends[n] = uint8(i)
				n++
			}
		}
		ends[n] = uint8(len(between))
		n++
	}
	return walk(middle, between, ends[:n])
}

// walk reports whether the components of between, cut at ends, match parts,
// which begin and end with ** — the pattern between its first ** and its
// last, the ref between what its head and its tail took. It is the classic
// wildcard walk over components: two indexes and ONE backtrack point, the
// last ** seen. One is enough because every other part takes exactly one
// component, so placing a run of parts at its earliest match leaves the
// longest remainder for the runs after it — any match that gave an earlier **
// more is also found by giving the later one more. A run is tried at each
// place it could start, so the walk takes at most (the components between) ×
// (the longest run) comparisons, which is what MaxPatternComponents bounds.
func walk(parts []part, between string, ends []uint8) bool {
	i, j := 1, 0       // parts[0] is **, which takes as few components as it can
	star, mark := 1, 0 // after the last ** seen: the part after it, and the component it took up to
	for j < len(ends) {
		if i < len(parts) {
			switch pt := &parts[i]; {
			case pt.kind == partAny && i == len(parts)-1:
				return true // the last **, which takes whatever is left
			case pt.kind == partAny:
				star, mark = i+1, j
				i++
				continue
			case pt.match(componentAt(between, ends, j)):
				i++
				j++
				continue
			}
		}
		mark++ // the last ** takes one component more
		i, j = star, mark
	}
	for i < len(parts) && parts[i].kind == partAny {
		i++
	}
	return i == len(parts)
}

// PatternReachesSeats reports whether pattern (false if no [ParsePattern]
// pattern) matches some ref a seat's namespace can hold:
// refs/heads/seats/<handle>/<branch> ([SeatsRoot]). A push mirror's patterns
// are held to it (the code domain's MirrorTarget): a mirror publishes what the
// company merged, never a seat's own branches.
//
// EXACT, NEVER A PREFIX TEST: every component below seats/ is its seat's to
// name, so a pattern reaches it exactly where its part over the component
// after refs/heads/ admits `seats` and the later parts take two components or
// more, or that part is **: refs/heads/** and refs/heads/*/*/* do;
// refs/heads/*, refs/heads/*/* and refs/heads/seats/x do not (seats and
// seats/<one name> are reserved, [ReservedSeatBranch]).
func PatternReachesSeats(pattern string) bool {
	var buf [MaxPatternComponents + 1]part
	p, ok := parsePattern(pattern, buf[:0])
	return ok && p.reachesSeats()
}

// reachesSeats is [PatternReachesSeats] over a parsed pattern.
func (p Pattern) reachesSeats() bool {
	if p.parts == nil {
		rest, under := strings.CutPrefix(p.literal, SeatsRoot)
		return under && strings.Contains(rest, "/")
	}
	// A glob's first two parts are refs and its namespace, each a literal
	// (parsePattern cut one of patternNamespaces off it).
	if p.parts[1].text != "heads" {
		return false
	}
	rest := p.parts[2:]
	if rest[0].kind == partAny {
		return true
	}
	if !rest[0].match("seats") {
		return false
	}
	below := 0
	for i := range rest[1:] {
		if rest[1+i].kind == partAny {
			return true
		}
		below++
	}
	return below >= 2
}

// componentAt is component j of s, cut at ends.
func componentAt(s string, ends []uint8, j int) string {
	start := 0
	if j > 0 {
		start = int(ends[j-1]) + 1
	}
	return s[start:ends[j]]
}

// match reports whether one ref component matches a part that is not **.
func (pt *part) match(name string) bool {
	switch pt.kind {
	case partLiteral:
		return name == pt.text
	case partOne:
		return true
	}
	return pt.globMatch(name)
}

// globMatch reports whether name matches a glob part, one component each, in
// which * stands for any run of bytes, the empty one included. The glob's text
// before its first * is a prefix of name and its text after its last * a
// suffix, not overlapping, and every piece between is found leftmost, in
// order, in what lies between: a piece placed as early as it can go leaves the
// most room for the ones after it, so a match that exists is never missed.
// That is path.Match's reading of a pattern whose only metacharacter is * —
// the only one ParsePattern admits — without its general machinery.
func (pt *part) globMatch(name string) bool {
	pre, suf, text := int(pt.pre), int(pt.suf), pt.text
	if len(name) < pre+suf ||
		pre > 0 && name[:pre] != text[:pre] ||
		suf > 0 && name[len(name)-suf:] != text[len(text)-suf:] {
		return false
	}
	if pt.mid == "" {
		return true
	}
	name, mid := name[pre:len(name)-suf], pt.mid
	for {
		k := strings.IndexByte(mid, '*')
		if k < 0 {
			return strings.Contains(name, mid)
		}
		at := strings.Index(name, mid[:k])
		if at < 0 {
			return false
		}
		name, mid = name[at+k:], mid[k+1:]
	}
}
