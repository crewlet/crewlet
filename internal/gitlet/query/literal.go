package query

import (
	"slices"
	"unicode"
	"unicode/utf8"
)

// A LOG'S GREP is a LITERAL, matched here over each commit's whole message as
// git streams it, never handed to git, whose --grep takes a regular
// expression: a pattern from a model is a runtime nobody bounded, and a read's
// argument lists carry no client text. It is matched case-folded as
// strings.EqualFold folds (every rune stands for the least of its fold orbit,
// so `K`, `k` and the Kelvin sign are one), by Knuth–Morris–Pratt: one pass,
// never a step back, so a message of any length is searched in memory bounded
// by the literal. A byte that is not UTF-8 stands for itself alone, so it
// matches nothing in the literal, which is valid UTF-8.

// literal is one Grep, matched over one message at a time.
type literal struct {
	pat  []rune
	fail []int
	// at is how much of pat the message's tail matches, found whether all of
	// it has occurred, and carry the opening of a rune the last write ended
	// inside.
	at    int
	found bool
	carry []byte
}

// newLiteral is the matcher of s, valid UTF-8 and not empty.
func newLiteral(s string) *literal {
	pat := make([]rune, 0, len(s))
	for _, r := range s {
		pat = append(pat, fold(r))
	}
	fail := make([]int, len(pat))
	for i, k := 1, 0; i < len(pat); i++ {
		for k > 0 && pat[i] != pat[k] {
			k = fail[k-1]
		}
		if pat[i] == pat[k] {
			k++
		}
		fail[i] = k
	}
	return &literal{pat: pat, fail: fail, carry: make([]byte, 0, utf8.UTFMax)}
}

// fold is the rune r stands for, case-folded: the least of its fold orbit.
func fold(r rune) rune {
	if r < utf8.RuneSelf {
		if 'a' <= r && r <= 'z' {
			return r - 'a' + 'A'
		}
		return r
	}
	least := r
	for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
		least = min(least, f)
	}
	return least
}

// write takes the message's next bytes, and reads none once the literal is
// found: nothing later unfinds it, and step has no rune of pat left to compare.
func (l *literal) write(p []byte) {
	// A rune the last write ended inside, completed by this one's opening
	// bytes, or else its bytes each standing for themselves.
	for len(l.carry) > 0 && len(p) > 0 && !l.found {
		take := min(utf8.UTFMax-len(l.carry), len(p))
		buf := slices.Concat(l.carry, p[:take])
		if !utf8.FullRune(buf) {
			l.carry, p = append(l.carry[:0], buf...), p[take:]
			continue
		}
		r, size := utf8.DecodeRune(buf)
		l.step(r, size, buf[0])
		if size >= len(l.carry) {
			p, l.carry = p[size-len(l.carry):], l.carry[:0]
		} else {
			l.carry = append(l.carry[:0], l.carry[size:]...)
		}
	}
	for len(p) > 0 && !l.found {
		if !utf8.FullRune(p) {
			l.carry = append(l.carry[:0], p...)
			return
		}
		r, size := utf8.DecodeRune(p)
		l.step(r, size, p[0])
		p = p[size:]
	}
}

// step advances the match by one rune of the message, r, size bytes long,
// the first of them b. Never call it once found: pat has no rune past its last.
func (l *literal) step(r rune, size int, b byte) {
	if r == utf8.RuneError && size == 1 {
		// A byte that is not UTF-8: below every rune, so no rune of the
		// literal, and distinct for every byte.
		r = -1 - rune(b)
	} else {
		r = fold(r)
	}
	for l.at > 0 && r != l.pat[l.at] {
		l.at = l.fail[l.at-1]
	}
	if r == l.pat[l.at] {
		l.at++
	}
	if l.at == len(l.pat) {
		l.found = true
	}
}

// end ends the message (a rune it ended inside is bytes that are not UTF-8),
// answers whether the literal occurred, and readies the matcher for the next.
func (l *literal) end() bool {
	for _, b := range l.carry {
		if !l.found {
			l.step(utf8.RuneError, 1, b)
		}
	}
	found := l.found
	l.at, l.found, l.carry = 0, false, l.carry[:0]
	return found
}
