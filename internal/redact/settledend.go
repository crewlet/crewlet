package redact

import (
	"strings"
	"unicode/utf8"
)

// SettledEnd is the redaction of text's end that its first context bytes
// settle, opening on a line, or "" where none is settled, and how much came
// before it: text's bytes up to the settled start ([settledStart]) and
// redacted bytes past it. text is a reading begun inside a longer text, its
// first context bytes ([LookBack]) read only to decide what follows; at 0 it is
// the whole, its whole redaction the end.
//
// WHAT IS SETTLED is shown as the WHOLE text redacts it: redacted alone, a key
// whose BEGIN is in the context reads as cut off, and a password whose key is
// there as no password. So only what both redactions agree on is kept, from
// the line after the last place they part: a line begun before it, read from
// a place that is not its start, is not read as the whole text reads it.
func SettledEnd(text string, context int) (end string, before int) {
	from, opensLine := settledStart(text, context)
	part := Secrets(text[from:])
	start := 0
	if from > 0 {
		start = len(part) - sharedSuffix(Secrets(text), part)
	}
	if start == len(part) {
		return "", from + start
	}
	onLine := start == 0 && opensLine || start > 0 && part[start-1] == '\n'
	if !onLine {
		i := strings.IndexByte(part[start:], '\n')
		if i < 0 {
			return "", from + len(part)
		}
		start += i + 1
	}
	return part[start:], from + start
}

// settledStart is where what text's first context bytes settle begins, and
// whether it opens a line: the first line starting context bytes in or later,
// else the first character there, inside the last line.
func settledStart(text string, context int) (int, bool) {
	switch {
	case context <= 0:
		return 0, true
	case context >= len(text):
		return len(text), true
	case text[context-1] == '\n':
		return context, true
	}
	if i := strings.IndexByte(text[context:], '\n'); i >= 0 && context+i+1 < len(text) {
		return context + i + 1, true
	}
	for context < len(text) && !utf8.RuneStart(text[context]) {
		context++
	}
	return context, false
}

// sharedSuffix is how many bytes a and b end with alike.
func sharedSuffix(a, b string) int {
	n := min(len(a), len(b))
	for i := range n {
		if a[len(a)-1-i] != b[len(b)-1-i] {
			return i
		}
	}
	return n
}
