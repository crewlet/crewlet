package gitref_test

import (
	"path"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/gitref"
)

func TestValidPattern(t *testing.T) {
	for _, tc := range []struct {
		pattern string
		want    bool
	}{
		{"refs/heads/main", true},
		{"refs/heads/release/*", true},
		{"refs/heads/**", true},
		{"refs/heads/**/hotfix", true},
		{"refs/heads/**/**", true},
		{"refs/heads/*", true},
		{"refs/heads/*/*", true},
		{"refs/heads/a.*", true},
		{"refs/heads/*.b", true},
		{"refs/heads/*a*", true},
		{"refs/tags/v*", true},
		{"refs/tags/**", true},
		{"refs/heads/" + strings.Repeat("a", 244), true}, // exactly MaxRefNameBytes
		{"refs/heads/a/b/c/d/e/*", true},                 // exactly MaxPatternComponents
		{"refs/heads/**/a/b/c/*/**", true},
		{"refs/heads/a/b/c/d/e/f/g/h/i/j/k/l/m/n/o", true}, // a literal is bounded only as a ref is

		{"refs/heads/*.lock", false}, // every ref it could match ends in .lock
		{"refs/heads/a..*", false},   // holds ..
		{"refs/heads/.*", false},     // begins with .
		{"refs/heads/*.", false},     // ends with .
		{"refs/heads/@{*", false},    // holds @{
		{"refs/heads/a**b", false},   // ** only as a whole component
		{"refs/heads/***", false},
		{"refs/heads/**.lock", false},
		{"refs/heads/a?", false}, // * is the one metacharacter
		{"refs/heads/[ab]", false},
		{`refs/heads/a\*`, false},
		{"refs/heads/a b", false},
		{"refs/tags/", false}, // nothing after the namespace
		{"refs/heads", false},
		{"refs/heads//a", false},
		{"refs/heads/a/", false},
		{"refs/remotes/*", false}, // a protection names a branch or a tag
		{"refs/notes/**", false},
		{"refs/*", false},
		{"*", false},
		{"", false},
		{"refs/heads/\xff", false},
		{"refs/heads/" + strings.Repeat("a", 245), false}, // past MaxRefNameBytes
		{"refs/heads/a/b/c/d/e/f/*", false},               // past MaxPatternComponents
		{"refs/heads/**/a/b/c/d/*/**", false},
		{"refs/tags/a/b/c/d/e/f/v*", false},
	} {
		if got := gitref.ValidPattern(tc.pattern); got != tc.want {
			t.Errorf("ValidPattern(%q) = %v, want %v", tc.pattern, got, tc.want)
		}
	}
}

// deep is a ref of 52 components below refs/heads, so the oracle holds a
// pattern's backtracking to git over far more than a branch's usual handful.
var deep = "refs/heads/d/" + strings.Repeat("a/", 50) + "b"

// patternRefs are the refs TestMatchPatternAgreesWithGitForEachRef creates.
var patternRefs = []string{
	"refs/heads/main",
	"refs/heads/v1",
	"refs/heads/z",
	"refs/heads/hotfix",
	"refs/heads/x/y",
	"refs/heads/release/2",
	"refs/heads/release/1/2",
	"refs/heads/release/1/3/2",
	"refs/heads/feat/a.b",
	"refs/heads/feat/a.c",
	"refs/heads/team/a/hotfix",
	"refs/heads/a/b/c/d",
	"refs/heads/a/x/a/b",
	deep,
	"refs/tags/v1.0",
	"refs/tags/v2.0-rc",
	"refs/tags/rc/v3",
}

// TestMatchPatternAgreesWithGitForEachRef holds MatchPattern to git
// for-each-ref over the refs of a real repository, each row's answer WRITTEN
// DOWN and re-measured on every run, so the table cannot drift from the git
// it describes. For a glob, git's listing IS the rule, so one column is both
// answers. For a literal, git lists the ref and every ref below it and
// MatchPattern the ref alone — the one place the package is stricter — and
// the test derives that difference from git's own listing rather than from a
// second column.
func TestMatchPatternAgreesWithGitForEachRef(t *testing.T) {
	c := gittest.NewClient(t)
	dir := c.Init(t)
	c.Commit(t, dir, nil, "t")
	for _, ref := range patternRefs {
		c.Run(t, dir, "update-ref", ref, "HEAD")
	}
	gitLists := func(pattern string) []string {
		var listed []string
		for line := range strings.Lines(c.Run(t, dir, "for-each-ref", "--format=%(refname)", pattern)) {
			listed = append(listed, strings.TrimSuffix(line, "\n"))
		}
		return listed
	}
	// matches is MatchPattern's set, held to the parsed form's on every ref:
	// the two are one rule, and the decide asks the parsed one.
	matches := func(pattern string) []string {
		parsed, ok := gitref.ParsePattern(pattern)
		if !ok {
			t.Fatalf("ParsePattern(%s) refused a pattern the table asks git about", pattern)
		}
		var matched []string
		for _, ref := range patternRefs {
			got := gitref.MatchPattern(pattern, ref)
			if parsed.Match(ref) != got {
				t.Errorf("%s against %s: Pattern.Match says %v, MatchPattern %v", pattern, ref, !got, got)
			}
			if got {
				matched = append(matched, ref)
			}
		}
		return matched
	}

	// Globs: one component per *; per **, zero or more — one or more as the
	// pattern's last component.
	for _, tc := range []struct {
		pattern string
		want    []string
	}{
		{"refs/heads/*", heads("main", "v1", "z", "hotfix")},
		{"refs/heads/v*", heads("v1")},
		{"refs/heads/*a*", heads("main")},
		{"refs/heads/*/y", heads("x/y")},
		{"refs/heads/*/*", heads("x/y", "release/2", "feat/a.b", "feat/a.c")},
		{"refs/heads/feat/*", heads("feat/a.b", "feat/a.c")},
		{"refs/heads/feat/*.b", heads("feat/a.b")},
		{"refs/heads/**", allHeads()},
		{"refs/heads/**/**", allHeads()},
		{"refs/heads/**/y", heads("x/y")},
		{"refs/heads/**/z", heads("z")}, // zero components
		{"refs/heads/**/hotfix", heads("hotfix", "team/a/hotfix")},
		{"refs/heads/release/**/2", heads("release/2", "release/1/2", "release/1/3/2")},
		{"refs/heads/x/**", heads("x/y")},
		{"refs/heads/x/y/**", nil},               // a trailing ** is one or more,
		{"refs/heads/**/b/**", heads("a/b/c/d")}, // so neither a/x/a/b nor deep
		{"refs/heads/**/c/*", heads("a/b/c/d")},
		{"refs/heads/**/*.b", heads("feat/a.b")},
		{"refs/heads/**/a/**/b", heads("a/x/a/b", deep[len("refs/heads/"):])},
		{"refs/heads/d/**/a/a/**/b", []string{deep}},
		{"refs/heads/d/**/a/b/**/b", nil}, // backtracks through every ** and finds nothing
		{"refs/tags/v*", tags("v1.0", "v2.0-rc")},
		{"refs/tags/**", tags("v1.0", "v2.0-rc", "rc/v3")},
	} {
		if listed := gitLists(tc.pattern); !sameSet(listed, tc.want) {
			t.Errorf("git for-each-ref %s lists %v, the table says %v", tc.pattern, listed, tc.want)
		}
		if matched := matches(tc.pattern); !sameSet(matched, tc.want) {
			t.Errorf("MatchPattern(%s) matches %v, git lists %v", tc.pattern, matched, tc.want)
		}
	}

	// Literals: git lists the ref and every ref below it up to a '/';
	// MatchPattern matches the identical ref alone.
	for _, tc := range []struct {
		pattern string
		git     []string
	}{
		{"refs/heads/main", heads("main")},
		{"refs/tags/v1.0", tags("v1.0")},
		{"refs/heads/x", heads("x/y")},
		{"refs/heads/release", heads("release/2", "release/1/2", "release/1/3/2")},
		{"refs/heads/release/1", heads("release/1/2", "release/1/3/2")},
	} {
		listed := gitLists(tc.pattern)
		if !sameSet(listed, tc.git) {
			t.Errorf("git for-each-ref %s lists %v, the table says %v", tc.pattern, listed, tc.git)
		}
		var exact []string
		for _, ref := range listed {
			switch {
			case ref == tc.pattern:
				exact = append(exact, ref)
			case strings.HasPrefix(ref, tc.pattern+"/"):
			default:
				t.Errorf("git for-each-ref %s lists %s, neither that ref nor one below it — "+
					"the literal rule this package is stricter than is not git's any more", tc.pattern, ref)
			}
		}
		if matched := matches(tc.pattern); !sameSet(matched, exact) {
			t.Errorf("MatchPattern(%s) matches %v, want only the identical ref: %v (git lists %v)",
				tc.pattern, matched, exact, listed)
		}
	}
}

func TestMatchPatternRefusesWhatIsInvalid(t *testing.T) {
	for _, tc := range []struct {
		pattern, ref string
	}{
		{"refs/heads/*.lock", "refs/heads/a.lock"}, // neither is valid
		{"refs/heads/a**b", "refs/heads/axb"},      // wildmatch would read a**b as a*b
		{"refs/heads/a?", "refs/heads/ab"},         // path.Match would read ?
		{"refs/remotes/*", "refs/remotes/origin"},  // a namespace no protection names
		{"refs/heads/*", "refs/heads/a..b"},        // * would match it as a component
		{"refs/heads/**", "refs/heads/a/"},
		{"refs/heads/**", "refs/heads/\xff"},
		{"refs/heads/**", "refs/heads/" + strings.Repeat("a", 245)},
		{"refs/heads/**", ""},
		{"", "refs/heads/main"},
	} {
		if gitref.MatchPattern(tc.pattern, tc.ref) {
			t.Errorf("MatchPattern(%q, %q) = true: an invalid pattern or ref matches nothing", tc.pattern, tc.ref)
		}
	}
}

// TestAskingARuleAllocatesNothing pins what the forms cost a ref in garbage:
// nothing, the parsed form on any ref and the one-off form on any pattern too,
// since its own stack holds the parts of the longest one ParsePattern admits
// — the decide asks every rule about every command, and an allocation there
// is a thousand times sixty-four a round.
func TestAskingARuleAllocatesNothing(t *testing.T) {
	for _, bc := range benchPatterns {
		parsed, ok := gitref.ParsePattern(bc.pattern)
		if !ok {
			t.Fatalf("ParsePattern(%s) refused it", bc.pattern)
		}
		if n := testing.AllocsPerRun(100, func() { parsed.Match(bc.ref) }); n != 0 {
			t.Errorf("%s: Pattern.Match allocates %v times a call", bc.name, n)
		}
		if n := testing.AllocsPerRun(100, func() { gitref.MatchPattern(bc.pattern, bc.ref) }); n != 0 {
			t.Errorf("%s: MatchPattern allocates %v times a call", bc.name, n)
		}
	}
}

func TestTheZeroPatternMatchesNothing(t *testing.T) {
	var zero gitref.Pattern
	for _, ref := range append([]string{"", deepest}, patternRefs...) {
		if zero.Match(ref) {
			t.Errorf("the zero Pattern matches %q: a pattern nobody parsed protects nothing", ref)
		}
	}
}

// TestMatchAnswersFalseForWhatIsNoRef holds Pattern.Match's promise to a
// caller that hands it something no ref is: a string longer than
// MaxRefNameBytes matches nothing, and nothing it is handed is a panic. The
// cut of what lies between a pattern's first ** and its last has room on the
// stack for a ref's components and no more, so the length is checked before
// it: without that check a run of slashes indexes past the cut and a long ref
// wraps its byte offsets, and refs/heads/** — which never reaches the cut —
// takes a string past the bound for a branch.
func TestMatchAnswersFalseForWhatIsNoRef(t *testing.T) {
	long := []string{
		"refs/heads/" + strings.Repeat("a", gitref.MaxRefNameBytes+1-len("refs/heads/")),
		"refs/heads/" + strings.Repeat("a/", 144) + "a", // 300 bytes of short components
		strings.Repeat("/", 400),
		"refs/heads/" + strings.Repeat("/", 400),
		"refs/heads/" + strings.Repeat("a/", 200) + "x",
	}
	malformed := []string{"", "/", "//", "refs", "refs/", "refs/heads", "refs/heads/",
		"refs/heads//", "refs//heads/a", "refs/heads/a//x", "refs/heads/a/"}
	for _, pattern := range []string{
		"refs/heads/main",
		"refs/heads/*",
		"refs/heads/**",
		"refs/heads/**/x",
		"refs/heads/**/a/**",
		"refs/heads/**/a/**/x",
		"refs/heads/a/**/a/a/**/x",
	} {
		parsed, ok := gitref.ParsePattern(pattern)
		if !ok {
			t.Fatalf("ParsePattern(%s) refused it", pattern)
		}
		for _, ref := range long {
			if matchOrPanic(t, parsed, pattern, ref) {
				t.Errorf("%s matches a string of %d bytes: past MaxRefNameBytes, no ref", pattern, len(ref))
			}
		}
		for _, ref := range malformed {
			matchOrPanic(t, parsed, pattern, ref) // any answer, but an answer
		}
	}
}

// matchOrPanic is parsed.Match(ref), with a panic reported as the failure it
// is rather than ending the test binary.
func matchOrPanic(t *testing.T, parsed gitref.Pattern, pattern, ref string) (matched bool) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("%s against a string of %d bytes panics: %v", pattern, len(ref), r)
		}
	}()
	return parsed.Match(ref)
}

// TestPatternMatchAgreesWithItsRule holds the walk to MatchPattern's
// documented rule, written here as plainly as it can be ([rule]), over every
// pattern of up to four components drawn from seven — literals, a * at each
// place a component can hold one, and ** — against every ref of up to four
// components drawn from four: 952 000 pairs, every placement of * and **
// against every split of a short ref. The git table holds the rule to git on
// sampled shapes; this holds the walk, which is written for speed, to the
// rule written for reading, on all of them.
func TestPatternMatchAgreesWithItsRule(t *testing.T) {
	refs := sequences([]string{"a", "b", "ab", "ba"}, 4)
	for _, p := range sequences([]string{"a", "b", "*", "a*", "*b", "*a*", "**"}, 4) {
		pattern := "refs/heads/" + strings.Join(p, "/")
		parsed, ok := gitref.ParsePattern(pattern)
		if !ok {
			t.Fatalf("ParsePattern(%s) refused it", pattern)
		}
		for _, r := range refs {
			ref := "refs/heads/" + strings.Join(r, "/")
			want := rule(p, r)
			if got := parsed.Match(ref); got != want {
				t.Errorf("Pattern.Match: %s against %s is %v, the rule says %v", pattern, ref, got, want)
			}
			if got := gitref.MatchPattern(pattern, ref); got != want {
				t.Errorf("MatchPattern: %s against %s is %v, the rule says %v", pattern, ref, got, want)
			}
		}
	}
}

// A PATTERN REACHES A SEAT'S NAMESPACE EXACTLY WHERE IT MATCHES A REF THERE:
// PatternReachesSeats is held to MatchPattern itself over every pattern of up
// to four components below refs/heads/ drawn from seats, a, *, s* (a glob
// admitting seats) and ** — and the tags namespace, which reaches none —
// against every ref a seat's namespace can hold of up to four components below
// refs/heads/seats/, each a or seats: a pattern reaches it iff one of those
// refs matches. Four components suffice for every pattern here, since a
// pattern's non-** parts take one component each and ** may take none. So
// refs/heads/** and refs/heads/*/*/* reach it, refs/heads/* and
// refs/heads/seats/* — which match only reserved names no ref holds — do not.
//
// Mutation: answer true for any pattern whose part after refs/heads/ admits
// seats, whatever follows — refs/heads/* reaches, red.
func TestAPatternReachesASeatsNamespaceExactlyWhereItMatchesARefThere(t *testing.T) {
	t.Parallel()
	var seatRefs []string
	for _, below := range sequences([]string{"a", "seats"}, 4) {
		if len(below) >= 2 {
			seatRefs = append(seatRefs, gitref.SeatsRoot+strings.Join(below, "/"))
		}
	}
	for _, ns := range []string{"refs/heads/", "refs/tags/"} {
		for _, p := range sequences([]string{"seats", "a", "*", "s*", "**"}, 4) {
			pattern := ns + strings.Join(p, "/")
			if !gitref.ValidPattern(pattern) {
				t.Fatalf("%s is no pattern", pattern)
			}
			want := slices.ContainsFunc(seatRefs, func(ref string) bool { return gitref.MatchPattern(pattern, ref) })
			if got := gitref.PatternReachesSeats(pattern); got != want {
				t.Errorf("PatternReachesSeats(%s) = %v, and a ref of a seat's namespace matching it is %v",
					pattern, got, want)
			}
		}
	}
	for pattern, want := range map[string]bool{
		"refs/heads/main": false, "refs/heads/seats": false, "refs/heads/seats/ana": false,
		"refs/heads/seats/ana/x": true, "refs/heads/release/*": false, "refs/tags/seats/a/b": false,
		"not a pattern": false,
	} {
		if got := gitref.PatternReachesSeats(pattern); got != want {
			t.Errorf("PatternReachesSeats(%s) = %v, want %v", pattern, got, want)
		}
	}
}

// TestAGlobComponentIsPathMatch holds one component's glob to path.Match,
// which is how the rule reads it, over every glob of up to six bytes drawn
// from a, b and * (none holding **, which ParsePattern refuses) against every
// name of up to six bytes drawn from a and b: every arrangement of prefix,
// pieces and suffix in that space, so a piece placed anywhere but leftmost —
// the one choice of the matcher a short alphabet of whole components never
// tests — finds a name that tells.
func TestAGlobComponentIsPathMatch(t *testing.T) {
	names := sequences([]string{"a", "b"}, 6)
	for _, g := range sequences([]string{"a", "b", "*"}, 6) {
		glob := strings.Join(g, "")
		if strings.Contains(glob, "**") {
			continue
		}
		parsed, ok := gitref.ParsePattern("refs/heads/" + glob)
		if !ok {
			t.Fatalf("ParsePattern(refs/heads/%s) refused it", glob)
		}
		for _, n := range names {
			name := strings.Join(n, "")
			want, err := path.Match(glob, name)
			if err != nil {
				t.Fatalf("path.Match(%q, %q): %v", glob, name, err)
			}
			if got := parsed.Match("refs/heads/" + name); got != want {
				t.Errorf("glob %s against %s is %v, path.Match says %v", glob, name, got, want)
			}
		}
	}
}

// rule is MatchPattern's documented rule over the components below the
// namespace, which both sides share: ** matches zero or more whole
// components — one or more as the pattern's last — and any other pattern
// component exactly one ref component, as path.Match reads it.
func rule(pattern, ref []string) bool {
	switch {
	case len(pattern) == 0:
		return len(ref) == 0
	case pattern[0] == "**" && len(pattern) == 1:
		return len(ref) > 0
	case pattern[0] == "**":
		for k := range len(ref) + 1 {
			if rule(pattern[1:], ref[k:]) {
				return true
			}
		}
		return false
	case len(ref) == 0:
		return false
	}
	ok, err := path.Match(pattern[0], ref[0])
	return err == nil && ok && rule(pattern[1:], ref[1:])
}

// sequences is every sequence of one to most elements of alphabet.
func sequences(alphabet []string, most int) [][]string {
	var all [][]string
	last := [][]string{nil}
	for range most {
		var next [][]string
		for _, s := range last {
			for _, a := range alphabet {
				next = append(next, append(slices.Clip(s), a))
			}
		}
		all = append(all, next...)
		last = next
	}
	return all
}

func heads(names ...string) []string { return prefixed("refs/heads/", names) }

func tags(names ...string) []string { return prefixed("refs/tags/", names) }

func prefixed(ns string, names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = ns + n
	}
	return out
}

// allHeads is every branch patternRefs creates.
func allHeads() []string {
	var out []string
	for _, ref := range patternRefs {
		if strings.HasPrefix(ref, "refs/heads/") {
			out = append(out, ref)
		}
	}
	return out
}

func sameSet(a, b []string) bool {
	return slices.Equal(slices.Sorted(slices.Values(a)), slices.Sorted(slices.Values(b)))
}

// deepest is the ref with the most components a ref may have under
// refs/heads/ — 122 one-byte ones, 254 bytes in all — which no pattern below
// matches.
var deepest = "refs/heads/" + strings.Repeat("a/", 121) + "b"

// widest is a ref of six-byte components, 250 bytes in all: the ref the worst
// arm's globs each match, a piece a byte, at every place they are tried.
var widest = "refs/heads/" + strings.Repeat("aaaaaa/", 34) + "b"

// benchPatterns are the shapes a protection is asked about, each against the
// ref its arm names: a literal rule on the branch it names; a release glob on
// a 34-byte release branch, the typical rule and ref; a ** with parts on both
// sides, which are compared once at the two ends of the deepest ref and never
// slid along it; a ** on either side of one component, which IS searched for
// along the deepest ref; and the costliest rule found within
// MaxPatternComponents — a run of four globs between two **s, three that
// match every component of widest searching six pieces each and a fourth that
// fails, so the run is tried at every place and fails at its last part every
// time. The same shape at any other width of component, one byte to
// twenty-four, costs within a fifth of it: narrow components are more places
// and wide ones more pieces.
var benchPatterns = []struct {
	name, pattern, ref string
	match              bool
}{
	{"literal", "refs/heads/main", "refs/heads/main", true},
	{"typical", "refs/heads/release/**", "refs/heads/release/2026.10/fix-the-login-flow", true},
	{"deepest", "refs/heads/**/hotfix", deepest, false},
	{"between", "refs/heads/**/hotfix/**", deepest, false},
	{"worst", "refs/heads/**/" + strings.Repeat("*a*a*a*a*a*a*/", 3) + "*a*a*a*a*a*a*c*/**", widest, false},
}

// BenchmarkMatchPattern is the one-off form: the pattern and the ref checked
// and the pattern parsed on every call.
func BenchmarkMatchPattern(b *testing.B) {
	for _, bc := range benchPatterns {
		if !gitref.ValidPattern(bc.pattern) || !gitref.ValidRef(bc.ref) ||
			gitref.MatchPattern(bc.pattern, bc.ref) != bc.match {
			b.Fatalf("%s: %s against %s is not the case the arm measures", bc.name, bc.pattern, bc.ref)
		}
		b.Run(bc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				gitref.MatchPattern(bc.pattern, bc.ref)
			}
		})
	}
}

// BenchmarkParsePattern is what reading a rule once costs: each arm's
// pattern, checked and read.
func BenchmarkParsePattern(b *testing.B) {
	for _, bc := range benchPatterns {
		b.Run(bc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				gitref.ParsePattern(bc.pattern)
			}
		})
	}
}

// BenchmarkValidRef is the check every command's ref passes once, before
// any rule is asked about it: each arm's ref.
func BenchmarkValidRef(b *testing.B) {
	for _, bc := range benchPatterns {
		b.Run(bc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				gitref.ValidRef(bc.ref)
			}
		})
	}
}

// BenchmarkPatternMatch is the parsed form, what a caller asking one rule
// about many refs pays a ref: the pattern parsed once, outside the loop.
func BenchmarkPatternMatch(b *testing.B) {
	for _, bc := range benchPatterns {
		parsed, ok := gitref.ParsePattern(bc.pattern)
		if !ok || parsed.Match(bc.ref) != bc.match {
			b.Fatalf("%s: %s against %s is not the case the arm measures", bc.name, bc.pattern, bc.ref)
		}
		b.Run(bc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				parsed.Match(bc.ref)
			}
		})
	}
}
