package gitref

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// CommitText is why git would not write s into a commit AS IT IS GIVEN (its
// message, or part of an identity), and "" when it would.
//
// GIT'S UTF-8 IS NOT GO'S. has_invalid_utf8 (commit.c) also refuses Unicode's
// noncharacters, which unicode/utf8 accepts, and ensure_utf8 then rewrites each
// byte of a refused sequence as Latin-1: a Go string ending in U+FFFE goes
// into history as the six bytes of "ï¿¾" while git warns and exits 0. The
// rule covers the WHOLE commit-tree input, identity and message alike, and
// lives below both readers (gitproc's identity check, internal/gitlet's
// EngineWrite) so they cannot disagree. A NUL is not this rule's: each reader
// refuses it itself (git cuts a message at it, an env variable cannot carry it).
func CommitText(s string) string {
	if !utf8.ValidString(s) {
		return "is not UTF-8: git would rewrite the commit's bytes as though they were Latin-1"
	}
	for _, r := range s {
		if noncharacter(r) {
			return fmt.Sprintf("holds the Unicode noncharacter %U, which git reads as bytes that are not UTF-8 "+
				"and rewrites as though they were Latin-1", r)
		}
	}
	return ""
}

// WrittenCommitText is s as git writes it into a commit: every byte at which
// git's UTF-8 (CommitText's) finds no character read as the Latin-1 character
// it is — ensure_utf8's rewrite, one byte at a time and the reading resumed
// after it, so a noncharacter's three bytes become six — and the rest as it
// is. It is s itself exactly where CommitText admits s.
//
// FOR TEXT A CALLER DERIVES FROM A COMMIT git once wrote rather than states:
// Gitlet's revert and cherry-pick messages are git's own, made from the
// commit they name, and git's own commands rewrite such a message so as they
// commit it — a builder that refused it instead would refuse a revert git
// makes, and one that wrote it unrewritten would be refused by the pool.
func WrittenCommitText(s string) string {
	if CommitText(s) == "" {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if (r != utf8.RuneError || size > 1) && !noncharacter(r) {
			b.WriteString(s[i : i+size])
			i += size
			continue
		}
		c := s[i]
		b.WriteByte(0xc0 | c>>6)
		b.WriteByte(0x80 | c&0x3f)
		i++
	}
	return b.String()
}

// noncharacter reports whether r is one of Unicode's 66 noncharacters, by
// has_invalid_utf8's own two tests: U+FDD0 to U+FDEF, and the last two code
// points of every plane, U+xxFFFE and U+xxFFFF, each a code point whose low
// sixteen bits are FFFE or FFFF. Unicode's stability policy fixes the set for
// good.
func noncharacter(r rune) bool {
	return (0xFDD0 <= r && r <= 0xFDEF) || r&0xFFFE == 0xFFFE
}
