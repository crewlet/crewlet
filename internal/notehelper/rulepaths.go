package main

import (
	"regexp"
	"strings"
)

// A RULE'S `paths`, matched as Claude Code matches them (measured on 2.1.330,
// the claude-nested adapter's table test holding every shape to its verdict):
// the value is a string or a list; each string is split at the commas outside
// braces, each piece trimmed and its braces expanded; a pattern ending in
// `/**` loses it; a list of `**` alone scopes nothing. The patterns are then
// gitignore patterns (gitignore(5)), matched case-insensitively against the
// path relative to the directory holding the rule's `.claude`: one without a
// slash but a trailing one matches at any depth, `**/`, `/**/` and `/**` cross
// directories, `!` re-includes, a bracket class never negates, and a match
// of a directory above the file matches the file.

// Brace expansion's bounds, Claude Code's own: past either the pattern is
// used unexpanded, so a pattern written to explode costs one comparison.
const (
	braceResults = 1000
	braceBytes   = 4 << 20
)

// rulePatterns is a `paths` value's patterns, nil where it scopes nothing.
func rulePatterns(values []string) []string {
	budget := braceBudget{results: braceResults, bytes: braceBytes}
	var out []string
	for _, v := range values {
		for _, piece := range splitTopLevel(v) {
			for _, p := range budget.expand(piece) {
				p = strings.TrimSuffix(p, "/**")
				if p != "" {
					out = append(out, p)
				}
			}
		}
	}
	for _, p := range out {
		if p != "**" {
			return out
		}
	}
	return nil
}

// splitTopLevel is v cut at each comma outside braces, each piece trimmed,
// the empty dropped.
func splitTopLevel(v string) []string {
	var out []string
	depth, start := 0, 0
	keep := func(s string) {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	for i := 0; i < len(v); i++ {
		switch v[i] {
		case '{':
			depth++
		case '}':
			depth--
		case ',':
			if depth == 0 {
				keep(v[start:i])
				start = i + 1
			}
		}
	}
	keep(v[start:])
	return out
}

// braceBudget is what is left of the expansion's bounds, shared by every
// pattern of one rule.
type braceBudget struct{ results, bytes int }

// braceGroup is a pattern's first brace group: what precedes it, its
// alternatives and what follows.
var braceGroup = regexp.MustCompile(`^([^{]*)\{([^}]+)\}(.*)$`)

// expand is p with its brace groups expanded, first group first, or p as it
// stands where the expansion would pass the budget.
func (b *braceBudget) expand(p string) []string {
	if !strings.Contains(p, "{") {
		return []string{p}
	}
	var out []string
	stack := []string{p}
	for len(stack) > 0 {
		h := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		m := braceGroup.FindStringSubmatch(h)
		if m == nil {
			out = append(out, h)
			continue
		}
		alts := strings.Split(m[2], ",")
		b.bytes -= len(h)
		n := len(out) + len(stack) + len(alts)
		if b.bytes < 0 || n > b.results || n*len(p) > b.bytes {
			return []string{p}
		}
		for i := len(alts) - 1; i >= 0; i-- {
			stack = append(stack, m[1]+strings.TrimSpace(alts[i])+m[3])
		}
	}
	b.results -= len(out)
	b.bytes -= len(out) * len(p)
	return out
}

// pathMatcher is a rule's patterns compiled ([compileRulePattern]), in order.
type pathMatcher []rulePattern

// rulePattern is one pattern: its expression, and whether it re-includes.
type rulePattern struct {
	re       *regexp.Regexp
	negative bool
}

// newPathMatcher compiles patterns, leaving out a comment, a blank and one
// ending in a lone backslash, as gitignore does.
func newPathMatcher(patterns []string) pathMatcher {
	var m pathMatcher
	for _, p := range patterns {
		if strings.HasPrefix(p, "#") || strings.TrimSpace(p) == "" ||
			strings.HasSuffix(p, `\`) && !strings.HasSuffix(p, `\\`) {
			continue
		}
		negative := strings.HasPrefix(p, "!")
		p = strings.TrimPrefix(p, "!")
		if strings.HasPrefix(p, `\!`) || strings.HasPrefix(p, `\#`) {
			p = p[1:]
		}
		if re := compileRulePattern(p); re != nil {
			m = append(m, rulePattern{re: re, negative: negative})
		}
	}
	return m
}

// matches reports whether rel, a slash-separated path relative to the rule's
// base, is matched: the directories above it first, each with a trailing
// slash, since a matched directory matches all under it and nothing under it
// is re-included.
func (m pathMatcher) matches(rel string) bool {
	for i := 0; i < len(rel); i++ {
		if rel[i] == '/' && m.verdict(rel[:i+1]) {
			return true
		}
	}
	return m.verdict(rel)
}

// verdict is p's own: the last pattern that applies decides, a pattern
// applying where it would change the answer.
func (m pathMatcher) verdict(p string) bool {
	in := false
	for _, r := range m {
		if r.negative == in && r.re.MatchString(p) {
			in = !r.negative
		}
	}
	return in
}

// compileRulePattern is one gitignore pattern as a case-insensitive
// expression over a relative path (a directory's with a trailing slash), nil
// for one that can match nothing (an unclosed bracket).
func compileRulePattern(p string) *regexp.Regexp {
	p = trimUnescapedSpace(p)
	if p == "" {
		return nil
	}
	var b strings.Builder
	b.WriteString("(?i)")
	switch {
	case strings.HasPrefix(p, "**/"):
		b.WriteString("^(?:.*/)?")
		p = p[3:]
	case strings.HasPrefix(p, "/"):
		b.WriteString("^")
		p = p[1:]
	case strings.Contains(strings.TrimSuffix(p, "/"), "/"):
		b.WriteString("^")
	default:
		b.WriteString("(?:^|/)")
	}
	dirOnly := strings.HasSuffix(p, "/") && !strings.HasSuffix(p, `\/`)
	p = strings.TrimSuffix(p, "/")
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch {
		case c == '\\' && i+1 < len(p):
			i++
			b.WriteString(regexp.QuoteMeta(p[i : i+1]))
		case strings.HasPrefix(p[i:], "/**/"):
			b.WriteString("(?:/[^/]+)*/")
			i += 3
		case p[i:] == "/**":
			b.WriteString("/.+")
			i += 2
		case c == '*':
			for i+1 < len(p) && p[i+1] == '*' {
				i++
			}
			// A trailing star after a slash names an entry of that directory,
			// never the directory itself.
			if i == len(p)-1 && i > 0 && p[i-1] == '/' {
				b.WriteString("[^/]+")
			} else {
				b.WriteString("[^/]*")
			}
		case c == '?':
			b.WriteString("[^/]")
		case c == '[':
			class, n := bracketClass(p[i:])
			if n == 0 {
				return nil
			}
			b.WriteString(class)
			i += n - 1
		default:
			b.WriteString(regexp.QuoteMeta(p[i : i+1]))
		}
	}
	if dirOnly {
		b.WriteString("/$")
	} else {
		b.WriteString("/?$")
	}
	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil
	}
	return re
}

// bracketClass is the bracket expression s opens as an RE2 class, and the
// bytes it spans; 0 where it is never closed or holds no member, which
// matches nothing. As in Claude Code, a leading `!` or `^` is a member, not a
// negation; a range written backwards is dropped, and a backslash escapes the
// member after it.
func bracketClass(s string) (string, int) {
	var members []string
	for i := 1; i < len(s); i++ {
		c := s[i]
		if c == ']' {
			var b strings.Builder
			for j := 0; j < len(members); j++ {
				if j+2 < len(members) && members[j+1] == "-" {
					if members[j] <= members[j+2] {
						b.WriteString(regexp.QuoteMeta(members[j]) + "-" + regexp.QuoteMeta(members[j+2]))
					}
					j += 2
					continue
				}
				b.WriteString(regexp.QuoteMeta(members[j]))
			}
			if b.Len() == 0 {
				return "", 0
			}
			return "[" + b.String() + "]", i + 1
		}
		if c == '\\' && i+1 < len(s) {
			i++
			c = s[i]
		}
		members = append(members, string(c))
	}
	return "", 0
}

// trimUnescapedSpace is p without its trailing spaces, but one a backslash
// escapes.
func trimUnescapedSpace(p string) string {
	t := strings.TrimRight(p, " \t")
	if len(t) < len(p) && strings.HasSuffix(t, `\`) && !strings.HasSuffix(t, `\\`) {
		return t + " "
	}
	return t
}
