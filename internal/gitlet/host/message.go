package host

import (
	"strings"
)

// A REVERT'S AND A CHERRY-PICK'S DEFAULT MESSAGE IS GIT'S OWN — what `git
// revert --no-edit` and `git cherry-pick -x` write (git 2.56.0's sequencer.c,
// do_pick_commit) — so a history that mixes the engine's reverts with a
// person's reads as one (TestARevertIsGitsRevert, TestACherryPickIsGitsCherryPick
// hold both against git's own commands on every run):
//
//   - a revert: `Revert "<subject>"`, a blank line, `This reverts commit
//     <id>.` — and a revert of a revert `Reapply "<its subject>"` (git
//     2.43's rule, which leaves a revert of a reapply as it is) — VERBATIM;
//   - a cherry-pick: the commit's message from its subject on, a line feed
//     where it ends without one, a blank line unless it already ends in a
//     trailer block (sequencer.c's has_conforming_footer, whose trailer
//     reading is trailer.c's: conformingFooter), and `(cherry picked from
//     commit <id>)` — every line's trailing white space then taken off and
//     runs of blank lines made one (strbuf_stripspace, which git's pick
//     applies and its revert does not: measured on 2.56.0).
//
// A subject is the first line of the message once lines of nothing but
// white space are skipped (commit.c's find_commit_subject), and everything
// from a NUL on is no part of the message, as every C string git reads it as.
// The comment character is git's default `#` and no trailer is configured:
// the pool runs git with no configuration file of a person's.

// revertMessage is `git revert --no-edit`'s message for commit c, whose
// message is msg.
func revertMessage(c, msg string) string {
	subject, _ := commitSubject(msg)
	var b strings.Builder
	if orig, ok := strings.CutPrefix(subject, `Revert "`); ok && !strings.HasPrefix(orig, `Revert "`) {
		b.WriteString(`Reapply "`)
		b.WriteString(orig)
		b.WriteString("\n")
	} else {
		b.WriteString(`Revert "`)
		b.WriteString(subject)
		b.WriteString("\"\n")
	}
	b.WriteString("\nThis reverts commit ")
	b.WriteString(c)
	b.WriteString(".\n")
	return b.String()
}

// pickMessage is `git cherry-pick -x`'s message for commit c, whose message is
// msg.
func pickMessage(c, msg string) string {
	subject, rest := commitSubject(msg)
	m := ""
	if subject != "" {
		m = rest
	}
	if m != "" && !strings.HasSuffix(m, "\n") {
		m += "\n"
	}
	if !conformingFooter(m) {
		m += "\n"
	}
	m += "(cherry picked from commit " + c + ")\n"
	return stripspace(m)
}

// commitSubject is msg's subject — its first line once blank lines are
// skipped, without its line feed — and the message from the subject on.
// Everything from a NUL on is dropped first.
func commitSubject(msg string) (subject, rest string) {
	if i := strings.IndexByte(msg, 0); i >= 0 {
		msg = msg[:i]
	}
	// commit.c's skip_blank_lines: a line of nothing but git's white space
	// is blank.
	for msg != "" {
		line := msg
		if i := strings.IndexByte(msg, '\n'); i >= 0 {
			line = msg[:i+1]
		}
		if trimGitSpace(line) != "" {
			break
		}
		msg = msg[len(line):]
	}
	subject = msg
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		subject = msg[:i]
	}
	return subject, msg
}

// trimGitSpace is s without its trailing white space as git reads white space
// (gitSpace): never a vertical tab or a form feed.
func trimGitSpace(s string) string {
	for s != "" && gitSpace(s[len(s)-1]) {
		s = s[:len(s)-1]
	}
	return s
}

// stripspace is git's strbuf_stripspace with no comment character: every
// line's trailing white space taken off, a run of blank lines made one, none
// at the start or the end, and every line ended by a line feed.
func stripspace(s string) string {
	var b strings.Builder
	empties := 0
	for s != "" {
		line := s
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			line = s[:i+1]
		}
		s = s[len(line):]
		kept := trimGitSpace(line)
		if kept == "" {
			empties++
			continue
		}
		if empties > 0 && b.Len() > 0 {
			b.WriteByte('\n')
		}
		empties = 0
		b.WriteString(kept)
		b.WriteByte('\n')
	}
	return b.String()
}

// The comment line and the scissors git's default configuration reads a
// message with.
const (
	commentLine = "#"
	scissors    = commentLine + " ------------------------ >8 ------------------------\n"
)

// gitGeneratedPrefixes are the trailers git writes itself, which make a
// paragraph a trailer block at a quarter of its lines (trailer.c).
var gitGeneratedPrefixes = []string{"Signed-off-by: ", "(cherry picked from commit "}

// conformingFooter reports whether msg ends in a trailer block as git's
// trailer iterator reads one with no divider (sequencer.c's
// has_conforming_footer over trailer.c's trailer_block_get): the end of the
// log message found past its trailing comments and scissors
// (ignoredLogMessageBytes), and a block found before it (trailerBlockStart).
func conformingFooter(msg string) bool {
	end := len(msg) - ignoredLogMessageBytes(msg)
	return trailerBlockStart(msg, end) < end
}

// ignoredLogMessageBytes is commit.c's ignored_log_message_bytes: how much of
// msg's end is comments, blank lines, an old "Conflicts:" block and
// everything from the scissors on.
func ignoredLogMessageBytes(msg string) int {
	cutoff := len(msg)
	switch {
	case strings.HasPrefix(msg, scissors):
		cutoff = 0
	default:
		if i := strings.Index(msg, "\n"+scissors); i >= 0 {
			cutoff = i + 1
		}
	}
	boc, bol, inConflicts := 0, 0, false
	for bol < cutoff {
		next := len(msg)
		if i := strings.IndexByte(msg[bol:cutoff], '\n'); i >= 0 {
			next = bol + i + 1
		}
		switch {
		case strings.HasPrefix(msg[bol:cutoff], commentLine) || msg[bol] == '\n':
			if boc == 0 {
				boc = bol
			}
		case strings.HasPrefix(msg[bol:], "Conflicts:\n"):
			inConflicts = true
			if boc == 0 {
				boc = bol
			}
		case inConflicts && msg[bol] == '\t':
		case boc != 0:
			boc, inConflicts = 0, false
		}
		bol = next
	}
	if boc != 0 {
		return len(msg) - boc
	}
	return len(msg) - cutoff
}

// trailerBlockStart is trailer.c's find_trailer_block_start over msg[:end]:
// where the trailer block begins — the last paragraph, past the title, whose
// lines are all trailers, or that holds a trailer git writes itself and is a
// quarter trailers — or end where there is none.
func trailerBlockStart(msg string, end int) int {
	// The first paragraph is the title and cannot be trailers.
	s := 0
	for s < end {
		if !strings.HasPrefix(msg[s:end], commentLine) && blankLine(msg[s:]) {
			break
		}
		s = nextLine(msg, s)
	}
	endOfTitle := s
	onlySpaces := true
	recognized := false
	trailers, nonTrailers, continuations := 0, 0, 0
	for l := lastLine(msg, end); l >= endOfTitle; l = lastLine(msg, l) {
		bol := msg[l:]
		if strings.HasPrefix(msg[l:end], commentLine) {
			nonTrailers += continuations
			continuations = 0
			continue
		}
		if blankLine(bol) {
			if onlySpaces {
				continue
			}
			nonTrailers += continuations
			switch {
			case recognized && trailers*3 >= nonTrailers:
				return nextLine(msg, l)
			case trailers > 0 && nonTrailers == 0:
				return nextLine(msg, l)
			}
			return end
		}
		onlySpaces = false
		generated := false
		for _, p := range gitGeneratedPrefixes {
			if strings.HasPrefix(bol, p) {
				generated = true
				break
			}
		}
		switch {
		case generated:
			trailers++
			continuations = 0
			recognized = true
		case findSeparator(bol) >= 1 && !gitSpace(bol[0]):
			trailers++
			continuations = 0
		case gitSpace(bol[0]):
			continuations++
		default:
			nonTrailers += 1 + continuations
			continuations = 0
		}
	}
	return end
}

// findSeparator is trailer.c's find_separator with the default separators,
// ":": where a trailer's colon is — after a token of letters, digits and
// hyphens, white space allowed only after it — or -1.
func findSeparator(line string) int {
	space := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case c == ':':
			return i
		case !space && (isAlnum(c) || c == '-'):
		case i > 0 && (c == ' ' || c == '\t'):
			space = true
		default:
			return -1
		}
	}
	return -1
}

// isAlnum is git's isalnum: an ASCII letter or digit.
func isAlnum(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9'
}

// blankLine is trailer.c's is_blank_line: the line at the start of s holds
// nothing but git's white space up to its line feed or s's end.
func blankLine(s string) bool {
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '\n':
			return true
		case !gitSpace(s[i]):
			return false
		}
	}
	return true
}

// nextLine is the offset of the line after the one at i: past its line feed,
// or msg's end.
func nextLine(msg string, i int) int {
	if j := strings.IndexByte(msg[i:], '\n'); j >= 0 {
		return i + j + 1
	}
	return len(msg)
}

// lastLine is trailer.c's last_line: where the last line of msg[:n] begins, its
// final line feed its own, or -1 for none.
func lastLine(msg string, n int) int {
	switch n {
	case 0:
		return -1
	case 1:
		return 0
	}
	if i := strings.LastIndexByte(msg[:n-1], '\n'); i >= 0 {
		return i + 1
	}
	return 0
}
