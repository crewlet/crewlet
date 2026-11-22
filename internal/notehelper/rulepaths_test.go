package main

import "testing"

// A RULE'S `paths` MATCH AS CLAUDE CODE MATCHES THEM. Every case's verdict is
// Claude Code 2.1.330's own, its matcher run over the same value and path:
// the documented shapes (`**`, `*`, `?`, braces, `**/` at no depth, a
// directory's `/**`) and the gitignore ones beneath them (a slash-free
// pattern at any depth, bracket classes, `!`, escapes, case, a trailing
// slash, a matched directory matching all under it).
//
// Mutation: anchor a slash-free pattern, let `*` or `?` cross a slash, keep a
// trailing `/**`, drop case folding or the directories-above check, or let a
// negation re-include under a matched directory, and a case goes red.
func TestARulesPathsMatchAsClaudeCodeMatchesThem(t *testing.T) {
	t.Parallel()
	const (
		matched = iota
		unmatched
		unscoped
	)
	for _, tc := range []struct {
		paths []string
		rel   string
		want  int
	}{
		{[]string{`src/**`}, `src/a.ts`, matched},
		{[]string{`src/**`}, `src/x/y/z.ts`, matched},
		{[]string{`src/**`}, `srcx/a.ts`, unmatched},
		{[]string{`src/**`}, `lib/src/a.ts`, matched},
		{[]string{`src/**`}, `SRC/a.ts`, matched},
		{[]string{`**/*.ts`}, `a.ts`, matched},
		{[]string{`**/*.ts`}, `x/a.ts`, matched},
		{[]string{`**/*.ts`}, `x/y/a.ts`, matched},
		{[]string{`**/*.ts`}, `a.tsx`, unmatched},
		{[]string{`**/*.ts`}, `a.ts/b.go`, matched},
		{[]string{`*.md`}, `README.md`, matched},
		{[]string{`*.md`}, `docs/x.md`, matched},
		{[]string{`*.md`}, `x.mdx`, unmatched},
		{[]string{`src/*.ts`}, `src/a.ts`, matched},
		{[]string{`src/*.ts`}, `src/x/a.ts`, unmatched},
		{[]string{`src/*.ts`}, `a.ts`, unmatched},
		{[]string{`src/**/*.ts`}, `src/a.ts`, matched},
		{[]string{`src/**/*.ts`}, `src/x/a.ts`, matched},
		{[]string{`src/**/*.ts`}, `src/x/y/a.ts`, matched},
		{[]string{`src/**/*.ts`}, `lib/src/a.ts`, unmatched},
		{[]string{`src/**/test/*.ts`}, `src/test/a.ts`, matched},
		{[]string{`src/**/test/*.ts`}, `src/x/test/a.ts`, matched},
		{[]string{`src/**/test/*.ts`}, `src/test/x/a.ts`, unmatched},
		{[]string{`?.go`}, `a.go`, matched},
		{[]string{`?.go`}, `ab.go`, unmatched},
		{[]string{`?.go`}, `x/a.go`, matched},
		{[]string{`[abc].go`}, `a.go`, matched},
		{[]string{`[abc].go`}, `d.go`, unmatched},
		{[]string{`[abc].go`}, `x/b.go`, matched},
		{[]string{`[!abc].go`}, `a.go`, matched},
		{[]string{`[!abc].go`}, `d.go`, unmatched},
		{[]string{`[^abc].go`}, `a.go`, matched},
		{[]string{`[^abc].go`}, `d.go`, unmatched},
		{[]string{`[a-c]*.go`}, `apple.go`, matched},
		{[]string{`[a-c]*.go`}, `delta.go`, unmatched},
		{[]string{`[a-c]*.go`}, `Banana.go`, matched},
		{[]string{`[c-a].go`}, `a.go`, unmatched},
		{[]string{`[c-a].go`}, `b.go`, unmatched},
		{[]string{`[]a].go`}, `].go`, unmatched},
		{[]string{`[]a].go`}, `a.go`, unmatched},
		{[]string{`[]a].go`}, `b.go`, unmatched},
		{[]string{`[ab.go`}, `[ab.go`, unmatched},
		{[]string{`[ab.go`}, `a.go`, unmatched},
		{[]string{`{src,lib}/**/*.ts`}, `src/a.ts`, matched},
		{[]string{`{src,lib}/**/*.ts`}, `lib/x/a.ts`, matched},
		{[]string{`{src,lib}/**/*.ts`}, `test/a.ts`, unmatched},
		{[]string{`src/*.{ts,tsx}`}, `src/a.ts`, matched},
		{[]string{`src/*.{ts,tsx}`}, `src/a.tsx`, matched},
		{[]string{`src/*.{ts,tsx}`}, `src/a.js`, unmatched},
		{[]string{`src/{a,b}/c/**`}, `src/a/c/d`, matched},
		{[]string{`src/{a,b}/c/**`}, `src/b/c/x/y`, matched},
		{[]string{`src/{a,b}/c/**`}, `src/e/c/d`, unmatched},
		{[]string{`{a,b}{1,2}.md`}, `a1.md`, matched},
		{[]string{`{a,b}{1,2}.md`}, `b2.md`, matched},
		{[]string{`{a,b}{1,2}.md`}, `c1.md`, unmatched},
		{[]string{`/src/a.ts`}, `src/a.ts`, matched},
		{[]string{`/src/a.ts`}, `x/src/a.ts`, unmatched},
		{[]string{`a.ts`}, `a.ts`, matched},
		{[]string{`a.ts`}, `x/a.ts`, matched},
		{[]string{`a.ts`}, `x/a.ts/b`, matched},
		{[]string{`docs/`}, `docs/a.md`, matched},
		{[]string{`docs/`}, `x/docs/a.md`, matched},
		{[]string{`docs/`}, `docs`, unmatched},
		{[]string{`**`}, `a.ts`, unscoped},
		{[]string{`**/**`}, `a.ts`, unscoped},
		{[]string{`**/**`}, `x/y`, unscoped},
		{[]string{`src/**`, `!src/gen/**`}, `src/gen/a.ts`, matched},
		{[]string{`src/**`, `!src/gen/**`}, `src/a.ts`, matched},
		{[]string{`src/**/*.ts`, `!src/**/*.test.ts`}, `src/a.ts`, matched},
		{[]string{`src/**/*.ts`, `!src/**/*.test.ts`}, `src/a.test.ts`, unmatched},
		{[]string{`src/**/*.ts`, `!src/**/*.test.ts`}, `src/x/b.test.ts`, unmatched},
		{[]string{`*`}, `a`, matched},
		{[]string{`*`}, `x/a`, matched},
		{[]string{`src/*`}, `src/a`, matched},
		{[]string{`src/*`}, `src/x/a`, matched},
		{[]string{`src/*`}, `src`, unmatched},
		{[]string{`**/test/**`}, `test/a`, matched},
		{[]string{`**/test/**`}, `x/test/b/c`, matched},
		{[]string{`**/test/**`}, `test`, matched},
		{[]string{`a/**/b`}, `a/b`, matched},
		{[]string{`a/**/b`}, `a/x/b`, matched},
		{[]string{`a/**/b`}, `a/x/y/b`, matched},
		{[]string{`a/**/b`}, `a/b/c`, matched},
		{[]string{`a/**/b`}, `a/xb`, unmatched},
		{[]string{`\*.md`}, `*.md`, matched},
		{[]string{`\*.md`}, `a.md`, unmatched},
		{[]string{`src/**, lib/**`}, `src/a`, matched},
		{[]string{`src/**, lib/**`}, `lib/b`, matched},
		{[]string{`src/**, lib/**`}, `x/c`, unmatched},
		{[]string{`  src/a.ts  `}, `src/a.ts`, matched},
		{[]string{`.github/**`}, `.github/workflows/ci.yml`, matched},
		{[]string{`.github/**`}, `x/.github/a`, matched},
		{[]string{`*.test.ts`}, `a.test.ts`, matched},
		{[]string{`*.test.ts`}, `x/a.test.ts`, matched},
		{[]string{`*.test.ts`}, `a.ts`, unmatched},
		{[]string{`#comment`}, `#comment`, unmatched},
		{[]string{`\#x.md`}, `#x.md`, matched},
		{[]string{`!x.md`}, `x.md`, unmatched},
		{[]string{`\!x.md`}, `!x.md`, matched},
		{[]string{`src/a b.ts`}, `src/a b.ts`, matched},
		{[]string{`**/foo`}, `foo`, matched},
		{[]string{`**/foo`}, `x/foo`, matched},
		{[]string{`**/foo`}, `x/foo/y`, matched},
		{[]string{`src/**/*`}, `src/a`, matched},
		{[]string{`src/**/*`}, `src/x/a`, matched},
		{[]string{`*.{md,txt}`}, `a.md`, matched},
		{[]string{`*.{md,txt}`}, `b/c.txt`, matched},
		{[]string{`*.{md,txt}`}, `d.go`, unmatched},
		{[]string{`a/*/b`}, `a/x/b`, matched},
		{[]string{`a/*/b`}, `a/x/y/b`, unmatched},
		{[]string{`a**b`}, `ab`, matched},
		{[]string{`a**b`}, `axyb`, matched},
		{[]string{`a**b`}, `ax/yb`, unmatched},
		{[]string{`**/*.{ts,tsx}`}, `deep/er/c.tsx`, matched},
		{[]string{`src/a.ts/`}, `src/a.ts`, unmatched},
		{[]string{`lib\`}, `lib\`, unmatched},
		{[]string{`lib\`}, `lib`, unmatched},
		{[]string{`x?y`}, `x/y`, unmatched},
		{[]string{`x?y`}, `xay`, matched},
		{[]string{`a+b(c).md`}, `a+b(c).md`, matched},
		{[]string{`{a,{b,c}}.md`}, `a.md`, unmatched},
		{[]string{`{a,{b,c}}.md`}, `b.md`, matched},
		{[]string{`{a,{b,c}}.md`}, `c.md`, unmatched},
		{[]string{`{a,{b,c}}.md`}, `{b.md`, unmatched},
		{[]string{`a.{x}`}, `a.x`, matched},
	} {
		patterns := rulePatterns(tc.paths)
		got := unmatched
		switch {
		case patterns == nil:
			got = unscoped
		case newPathMatcher(patterns).matches(tc.rel):
			got = matched
		}
		if got != tc.want {
			t.Errorf("paths %q on %q: got %d, Claude Code's verdict is %d (0 matched, 1 not, 2 unscoped)",
				tc.paths, tc.rel, got, tc.want)
		}
	}
}

// A PATTERN WRITTEN TO EXPLODE IS USED AS IT STANDS: past Claude Code's own
// bound on brace expansion the pattern is one comparison, so a rule cannot
// stall the hook past its timeout.
//
// Mutation: drop the bound, and this expands 2^20 patterns.
func TestABraceExpansionPastItsBoundIsUsedUnexpanded(t *testing.T) {
	t.Parallel()
	p := ""
	for range 20 {
		p += "{a,b}"
	}
	if got := rulePatterns([]string{p}); len(got) != 1 || got[0] != p {
		t.Errorf("an exploding pattern became %d patterns, want itself alone", len(got))
	}
}
