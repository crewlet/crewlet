package sandbox

import "strings"

// ShellQuote is s as ONE word of a POSIX shell: wrapped in single quotes,
// each single quote inside it written as four bytes — a quote that closes the
// word, a backslash-escaped quote, a quote that opens it again — so the shell
// reads every byte of s literally, a `$( )`, a backquote, a `;`, a space or a
// newline included, and hands it to the command as one argument.
//
// THE ONE QUOTING FUNCTION for every value the engine writes into a command a
// box's shell reads: a coding agent's invocation and the paths of its
// artefacts (internal/sandbox/codingagent), the engine's reads of a run's
// workspace (internal/engine), and the steps that give a box a repository.
// Each of those used to carry its own copy of these two lines, and a copy is a
// second chance to disagree about a quote — the one byte the rule exists for
// (ADR-0008). Every such value comes from somewhere the engine does not get to
// trust: a box home derived from a provider's id and an operator's state_dir,
// a path a model named, a branch a tracker's key was spelled into.
//
// No byte needs a second rule: a single-quoted word has no escapes but the
// closing quote, so NUL is the only byte it cannot carry, and a command's
// text — a C string to every shell — cannot carry one either.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
