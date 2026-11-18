package sandbox

import "strings"

// ShellQuote is s as ONE word of a POSIX shell: single-quoted, each inner
// single quote closing the word, escaped, and reopening it, so the shell reads
// every byte literally (`$( )`, a backquote, `;`, a newline) as one argument.
// No byte needs a second rule: NUL is the only byte a quoted word cannot carry,
// and a command's text, a C string to every shell, cannot carry one either.
//
// It is the one quoting function for every value the engine writes into a
// command a box's shell reads (internal/sandbox/executor, internal/engine, the
// repository steps): a copy is a second chance to disagree about a quote
// (ADR-0008), and every such value is untrusted (a model's path, a branch
// spelled from a tracker's key, a box home under an operator's state_dir).
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
