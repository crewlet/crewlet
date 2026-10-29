package query

import (
	"unicode"
	"unicode/utf8"
)

// A LOG'S GREP is a LITERAL, matched here over each commit's whole message as
// git streams it — never handed to git, whose --grep takes a regular
// expression: a pattern from a model is a runtime nobody bounded, and a
// read's argument lists carry no client text. It is matched CASE-FOLDED, as
// strings.EqualFold folds (Unicode's simple case folding: every rune stands
// for the least rune of its fold orbit, so `K`, `k` and the Kelvin sign are
// one), by Knuth–Morris–Pratt over those folded runes: one pass, never a step
// back, so a message of any length is searched whole in memory bounded by the
// literal — its runes and their failure table, and the opening of a rune a
// write ended inside. A byte that is not UTF-8 stands for itself alone, and
// so matches nothing a literal, which is valid UTF-8, holds.

// literal is one Grep, matched over one message at a time.
type literal struct {
	pat  []rune
	fail []int
	// at is how much of pat the message's tail matches, found whether the
	// whole of it has occurred, and carry the opening of a rune the last
	// write ended inside, completed by the next.
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

// write takes the message's next bytes — and, once the literal is found, reads
// none: nothing the message goes on to say unfinds it, and the match is
// whole, so no rune of the literal is left to step to.
func (l *literal) write(p []byte) {
	// A RUNE THE LAST WRITE ENDED INSIDE, completed by this one's opening
	// bytes — or, where they do not complete it, its bytes each standing for
	// themselves.
	for len(l.carry) > 0 && len(p) > 0 && !l.found {
		take := min(utf8.UTFMax-len(l.carry), len(p))
		buf := append(l.carry[:len(l.carry):len(l.carry)], p[:take]...)
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

// step advances the match by one rune of the message — r, size bytes long,
// the first of them b. Never once the literal is found: the match is then the
// whole literal, and the literal has no rune past its last to compare.
func (l *literal) step(r rune, size int, b byte) {
	if r == utf8.RuneError && size == 1 {
		// A BYTE THAT IS NOT UTF-8, which no rune of the literal is: below
		// every rune, and one of its own for every byte.
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

// end ends the message — a rune it ended inside is bytes that are not UTF-8
// — and answers whether the literal occurred in it, readying the matcher for
// the next.
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
