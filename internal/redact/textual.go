package redact

import (
	"bytes"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// textual is a text the rules read: a string, as [Secrets] reads a text whole,
// or bytes, as a [Stream]'s stage reads its window in place ([feed.window]),
// one generic body serving both. Bytes rather than an unsafe.String view of
// the same buffers: measured interleaved on two shared cores, this streams 3.2
// to 6.4 percent slower than the view, where either streams 24 to 51 percent
// faster than before, and Secrets runs as fast as before (BenchmarkSecrets, 187
// to 192 MB/s against 185); but a view kept past a slide of its buffer redacts
// or publishes the wrong bytes, and no test caught that (a mutant dropping a
// Clone survived its poison test). The helpers below are the strings
// package's, for either, each dispatching on the type once: the box it
// switches on stays on the stack, as does each literal [tIndex] converts to
// bytes, none longer than the 32 bytes the compiler converts there.
type textual interface{ string | []byte }

func tIndex[T textual](s T, sep string) int {
	switch v := any(s).(type) {
	case string:
		return strings.Index(v, sep)
	case []byte:
		return bytes.Index(v, []byte(sep))
	}
	panic("redact: unreachable")
}

func tIndexByte[T textual](s T, c byte) int {
	switch v := any(s).(type) {
	case string:
		return strings.IndexByte(v, c)
	case []byte:
		return bytes.IndexByte(v, c)
	}
	panic("redact: unreachable")
}

func tLastIndexByte[T textual](s T, c byte) int {
	switch v := any(s).(type) {
	case string:
		return strings.LastIndexByte(v, c)
	case []byte:
		return bytes.LastIndexByte(v, c)
	}
	panic("redact: unreachable")
}

func tIndexAny[T textual](s T, chars string) int {
	switch v := any(s).(type) {
	case string:
		return strings.IndexAny(v, chars)
	case []byte:
		return bytes.IndexAny(v, chars)
	}
	panic("redact: unreachable")
}

func tIndexFunc[T textual](s T, f func(rune) bool) int {
	switch v := any(s).(type) {
	case string:
		return strings.IndexFunc(v, f)
	case []byte:
		return bytes.IndexFunc(v, f)
	}
	panic("redact: unreachable")
}

func tContains[T textual](s T, sub string) bool { return tIndex(s, sub) >= 0 }

func tContainsAny[T textual](s T, chars string) bool { return tIndexAny(s, chars) >= 0 }

// tHasPrefix, tHasSuffix and tEqual compare byte by byte, converting nothing.
func tHasPrefix[T textual](s T, prefix string) bool {
	if len(s) < len(prefix) {
		return false
	}
	for i := range len(prefix) {
		if s[i] != prefix[i] {
			return false
		}
	}
	return true
}

func tHasSuffix[T textual](s T, suffix string) bool {
	if len(s) < len(suffix) {
		return false
	}
	return tHasPrefix(s[len(s)-len(suffix):], suffix)
}

func tEqual[T textual](s T, t string) bool { return len(s) == len(t) && tHasPrefix(s, t) }

// tHasPrefixOf is whether s begins with p, a text of the same kind.
func tHasPrefixOf[T textual](s, p T) bool {
	if len(s) < len(p) {
		return false
	}
	for i := range len(p) {
		if s[i] != p[i] {
			return false
		}
	}
	return true
}

func tHasSuffixOf[T textual](s, p T) bool {
	return len(s) >= len(p) && tHasPrefixOf(s[len(s)-len(p):], p)
}

func tContainsOf[T textual](s, sub T) bool {
	switch v := any(s).(type) {
	case string:
		return strings.Contains(v, any(sub).(string))
	case []byte:
		return bytes.Contains(v, any(sub).([]byte))
	}
	panic("redact: unreachable")
}

func tTrimLeft[T textual](s T, cutset string) T {
	i := 0
	for i < len(s) && strings.IndexByte(cutset, s[i]) >= 0 {
		i++
	}
	return s[i:]
}

func tTrimRight[T textual](s T, cutset string) T {
	i := len(s)
	for i > 0 && strings.IndexByte(cutset, s[i-1]) >= 0 {
		i--
	}
	return s[:i]
}

func tTrimSuffix[T textual](s T, suffix string) T {
	if tHasSuffix(s, suffix) {
		return s[:len(s)-len(suffix)]
	}
	return s
}

func tTrimSpace[T textual](s T) T {
	switch v := any(s).(type) {
	case string:
		t := strings.TrimLeftFunc(v, unicode.IsSpace)
		start := len(v) - len(t)
		return s[start : start+len(strings.TrimRightFunc(t, unicode.IsSpace))]
	case []byte:
		t := bytes.TrimLeftFunc(v, unicode.IsSpace)
		start := len(v) - len(t)
		return s[start : start+len(bytes.TrimRightFunc(t, unicode.IsSpace))]
	}
	panic("redact: unreachable")
}

func tDecodeRune[T textual](s T) (rune, int) {
	switch v := any(s).(type) {
	case string:
		return utf8.DecodeRuneInString(v)
	case []byte:
		return utf8.DecodeRune(v)
	}
	panic("redact: unreachable")
}

func tDecodeLastRune[T textual](s T) (rune, int) {
	switch v := any(s).(type) {
	case string:
		return utf8.DecodeLastRuneInString(v)
	case []byte:
		return utf8.DecodeLastRune(v)
	}
	panic("redact: unreachable")
}

// tString is s as a string: s itself, or a copy of the bytes.
func tString[T textual](s T) string {
	switch v := any(s).(type) {
	case string:
		return v
	case []byte:
		return string(v)
	}
	panic("redact: unreachable")
}

func reFindIndex[T textual](re *regexp.Regexp, s T) []int {
	switch v := any(s).(type) {
	case string:
		return re.FindStringIndex(v)
	case []byte:
		return re.FindIndex(v)
	}
	panic("redact: unreachable")
}

func reMatch[T textual](re *regexp.Regexp, s T) bool {
	switch v := any(s).(type) {
	case string:
		return re.MatchString(v)
	case []byte:
		return re.Match(v)
	}
	panic("redact: unreachable")
}

// valueFunc, readFunc, boundFunc and refusesFunc are a rule's functions of
// where a match stands ([rule]), each for a string and for bytes: one generic
// body instantiated twice, since a function value cannot be generic.
type valueFunc struct {
	s func(string, int, int, [][2]int) (int, int)
	b func([]byte, int, int, [][2]int) (int, int)
}

func callValue[T textual](f *valueFunc, text T, start, end int, prior [][2]int) (int, int) {
	switch v := any(text).(type) {
	case string:
		return f.s(v, start, end, prior)
	case []byte:
		return f.b(v, start, end, prior)
	}
	panic("redact: unreachable")
}

type readFunc struct {
	s func(string, int, int, [][2]int) aValue
	b func([]byte, int, int, [][2]int) aValue
}

func callRead[T textual](f *readFunc, text T, start, end int, prior [][2]int) aValue {
	switch v := any(text).(type) {
	case string:
		return f.s(v, start, end, prior)
	case []byte:
		return f.b(v, start, end, prior)
	}
	panic("redact: unreachable")
}

type boundFunc struct {
	s func(string, int, int) (bool, int)
	b func([]byte, int, int) (bool, int)
}

func callBound[T textual](f *boundFunc, text T, start, end int) (bool, int) {
	switch v := any(text).(type) {
	case string:
		return f.s(v, start, end)
	case []byte:
		return f.b(v, start, end)
	}
	panic("redact: unreachable")
}

type refusesFunc struct {
	s func(string, int) bool
	b func([]byte, int) bool
}

func callRefuses[T textual](f *refusesFunc, text T, at int) bool {
	switch v := any(text).(type) {
	case string:
		return f.s(v, at)
	case []byte:
		return f.b(v, at)
	}
	panic("redact: unreachable")
}
