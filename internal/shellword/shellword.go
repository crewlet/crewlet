// Package shellword is the one quoting of a value as a word of a POSIX shell,
// for every command the engine writes for a shell to read:
//   - a box's: the harness's invocations and the paths of a run's files
//     (internal/sandbox/executor), a repository's checkout and the backends'
//     own commands (internal/sandbox), the engine's reads of a run's workspace
//     and its removal of an artifact's earlier shots (internal/engine), a
//     repository's checks (internal/gitlet/checkrun), and the box helper's
//     ensure (internal/browser/boxhelper);
//   - git's: the hook every push's receive-pack runs (internal/gitlet/cache),
//     and the credential helper `crewlet gitlet` hands a git that talks to a
//     node (cmd/crewlet);
//   - a person's: a line crewlet prints to be pasted into a shell, such as
//     `crewlet iam`'s next-page line.
//
// The suite's stand-ins and scripts quote through it too, so a test writes a
// word as the engine does.
//
// Each of them once kept a private copy of the same two lines, and a copy is
// a second chance to disagree about a quote (ADR-0008). A value may hold any
// byte, and many are untrusted besides (a model's path, a branch spelled from
// a tracker's key, a box home under an operator's state_dir). It is a leaf,
// importing nothing of the engine's, so that any package writing such a
// command can import it: one that internal/sandbox imports could never import
// internal/sandbox for it.
package shellword

import "strings"

// Quote is s as ONE word of a POSIX shell: single-quoted, each inner single
// quote closing the word, escaped, and reopening it, so the shell reads every
// byte literally (`$( )`, a backquote, `;`, a newline) as one argument. No
// byte needs a second rule: NUL is the only byte a quoted word cannot carry,
// and a command's text, a C string to every shell, cannot carry one either.
func Quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
