package gitref

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// CommitText is why git would not write s into a commit AS IT IS GIVEN — its
// message, or a part of its author's or committer's identity — and "" when it
// would.
//
// GIT'S UTF-8 IS NOT GO'S. Its has_invalid_utf8 (commit.c) refuses, beside
// everything unicode/utf8 refuses, Unicode's noncharacters, which unicode/utf8
// takes as it takes any other rune, and its ensure_utf8 then rewrites every
// byte of a sequence it refused as though the byte were Latin-1 — so a valid
// Go string ending in U+FFFE goes into history ending in the six bytes of
// "ï¿¾", while git prints a warning and exits 0. The rule is the WHOLE
// commit's, since ensure_utf8 reads every byte commit-tree writes: what a
// commit's identity holds (internal/code/gitproc's Identity) and the message
// it reads from its standard input alike.
//
// One rule here, below both of its readers: the pool that runs commit-tree
// (internal/code/gitproc) holds an identity to it before any process starts,
// and the code domain holds an engine write's message to it as it validates
// the write (internal/code's EngineWrite) — the tool that asks a write, the
// wire it crosses and the git node that builds it, by one reading. Two copies
// of it would refuse one text in one place and rewrite it in the other.
//
// A NUL is not this rule's: git cuts a commit's message at its first NUL and
// an environment variable cannot carry one, so each reader refuses it in its
// own words.
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
// the code host's revert and cherry-pick messages are git's own, made from the
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
