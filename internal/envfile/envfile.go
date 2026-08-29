// Package envfile is THE grammar of one `.env` assignment.
//
// # Why a package for one line of text
//
// A credential file has TWO readers and they do not agree, so a line that
// satisfies one can silently break the other:
//
//   - an OPERATOR reads it with `source .env`, where a bare space ends the
//     assignment and the rest of the line becomes a command — and the shell
//     then abandons every credential BELOW it in the file;
//   - a DOTENV TOOL reads it — direnv, docker compose's env_file, a CI
//     step's loader — and expands ${...} inside unquoted and double-quoted
//     values, so a token containing one is silently rewritten.
//
// The first failure is the one that hurts: it is silent, it is positional,
// and the symptom is a company where the seats defined after some particular
// token stop authenticating.
//
// # The ENGINE is not one of the readers
//
// It reads `${VAR}` from this node's secret store and then from the PROCESS
// environment, and from nowhere else — so a file written here has to be
// sourced, or fed to the process some other way, before the engine starts.
// A dotenv loader in the engine would be a third source of truth for
// secrets, discovered by filename, able to override the Tier A keyring that
// opens the store: exactly the inversion the two-tier design refuses. The
// path that needs no file at all is `-secret-store`.
//
// Every provisioner that mints a credential writes one of these lines. When
// each built its own, whether a minted token survived depended on which CLI
// minted it — one reasoned the quoting through and refused what it could not
// represent, another wrote a bare fmt.Sprintf("%s=%s"). This package is the
// one answer, and the suite fails the build on a new hand-built assignment.
//
// It imports nothing from crewlet, so even the config loader can use it.
package envfile

import (
	"fmt"
	"regexp"
	"strings"
)

// AssignmentRE matches one assignment line, with or without `export`.
//
// The `export` prefix is accepted because an operator's file usually has it —
// it is what makes `source .env` put the values into the CHILD processes an
// operator then starts — and a parser that rejected it would rewrite a file
// it could not read.
var AssignmentRE = regexp.MustCompile(
	`^\s*(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*=(.*)$`)

// ErrUnrepresentable reports a value no assignment line can carry safely.
//
// Its own error because the caller's answer is specific: a provisioner that
// minted a token it cannot write must REVOKE it rather than leave it
// standing, since a credential that exists and is not recorded is one nobody
// can use and nobody will remember to remove.
var ErrUnrepresentable = fmt.Errorf("envfile: the value cannot be written safely")

// FormatAssignment renders NAME=value so that BOTH readers agree.
//
// # Single quotes, and why not double
//
// A single-quoted shell string is literal: no expansion, no escapes, no
// interpretation of $ or ` or \. A double-quoted one expands $NAME and
// ${NAME} in both the shell AND the dotenv loader — so a token containing a
// literal `$` (which base64 and many vendor tokens do) would reach the
// engine with a chunk of itself replaced by an environment variable, or by
// nothing.
//
// The only thing a single-quoted string cannot contain is a single quote.
// Rather than the usual '\” splice — which `source` handles and several
// dotenv readers do not — such a value is REFUSED. A provisioner can then
// mint a different one; silently writing something one reader mangles is
// how a credential comes to differ between the two.
func FormatAssignment(name, value string) (string, error) {
	if !NameOK(name) {
		return "", fmt.Errorf("envfile: %q is not a variable name", name)
	}
	if strings.ContainsAny(value, "'\n\r") {
		// A NEWLINE IS AS FATAL AS A QUOTE and for the same reason: the
		// second line is not an assignment, so `source` runs it.
		return "", fmt.Errorf("%w: %s contains a single quote or a newline",
			ErrUnrepresentable, name)
	}
	return "export " + name + "='" + value + "'", nil
}

// NameOK reports whether a name is a usable variable name.
func NameOK(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		switch {
		case r == '_' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z'):
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

// ParseAssignment reads one line back, reporting whether it was one.
//
// It UNDERSTANDS all three quoting forms rather than only the one
// FormatAssignment writes, because the file it reads was usually written by
// a person: an operator's hand-written `TOKEN=abc` must be recognised as the
// same variable, or a rotation appends a second assignment and which one
// wins depends on the reader.
func ParseAssignment(line string) (name, value string, ok bool) {
	m := AssignmentRE.FindStringSubmatch(line)
	if m == nil {
		return "", "", false
	}
	return m[1], unquote(strings.TrimSpace(m[2])), true
}

// unquote strips one layer of matching quotes.
//
// A BARE value is trimmed of trailing comment-like text only when it is
// quoted, never when it is not: `TOKEN=abc # note` is a shell assignment of
// `abc` with a comment, but a dotenv reader disagrees about it — so this
// takes the whole remainder, which is what the engine's loader does, and
// leaves the ambiguity visible rather than resolving it two ways.
func unquote(value string) string {
	if len(value) < 2 {
		return value
	}
	first, last := value[0], value[len(value)-1]
	if first == last && (first == '\'' || first == '"') {
		return value[1 : len(value)-1]
	}
	return value
}
