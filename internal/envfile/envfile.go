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
// # A whole file, as a dotenv tool reads it
//
// A harness that keeps its keys in a .env (Hermes Agent's ~/.hermes/.env) is
// a dotenv reader of its own, and the engine seeds and compares that file. So
// [ReadFile] states that reader's grammar for a whole file, here beside the
// line's: what Hermes Agent 0.23.4's one tokenizer (agent/secret_scope.py)
// takes, comments, `export`, quotes and inline comments, and refuses what two
// readers could take two ways. It reads quoting and comments unlike
// [ParseAssignment] because its reader differs: ParseAssignment answers to
// `source` and the engine's loader, ReadFile to a dotenv tokenizer. Writing
// such a file is [FormatAssignment], which both take.
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
// A single-quoted shell string is literal: no expansion or escapes. A
// double-quoted one expands $NAME and ${NAME} in both the shell AND the dotenv
// loader, so a token containing a literal `$` (as base64 and many vendor
// tokens do) would reach the engine partly replaced. A single-quoted string cannot contain a single quote. The usual '\” splice
// is handled by `source` but not by several dotenv readers, so such a value is
// REFUSED: a provisioner can mint a different one, where a value one reader
// mangles makes a credential differ between the two.
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

// ReadFile is the variables a whole .env assigns, read as a dotenv tool reads
// them (Hermes Agent 0.23.4's tokenizer): a byte-order mark set aside, blank
// lines and lines opening with `#` skipped, an `export` prefix, a value in
// double quotes taking `\"` and `\\` escapes, one in single quotes taken as
// written, and a `#` comment after a quoted value's closing quote, or after a
// space in a bare one, dropped.
//
// A name is what comes before the line's first `=`, its spaces trimmed, as
// that reader takes it, so `MY-KEY=x` is MY-KEY. What two readers could take
// two ways is refused: a line holding no `=` or no name, or a name assigned
// twice, which one reader takes the first of and another the last.
func ReadFile(content []byte) (map[string]string, error) {
	vars := map[string]string{}
	text := strings.TrimPrefix(string(content), "\ufeff")
	for n, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if rest, ok := strings.CutPrefix(line, "export "); ok {
			line = strings.TrimLeft(rest, " \t")
		}
		name, value, found := strings.Cut(line, "=")
		if name = strings.TrimSpace(name); !found || name == "" {
			return nil, fmt.Errorf("envfile: line %d is no assignment", n+1)
		}
		if _, set := vars[name]; set {
			return nil, fmt.Errorf("envfile: %s is assigned twice", name)
		}
		vars[name] = dotenvValue(withoutComment(value))
	}
	return vars, nil
}

// withoutComment is value less a trailing `# ...`: after a quoted value's
// closing quote, or, bare, from a `#` that follows a space.
func withoutComment(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return value
	}
	if quote := value[0]; quote == '"' || quote == '\'' {
		for i := 1; i < len(value); i++ {
			switch {
			case quote == '"' && value[i] == '\\':
				i++
			case value[i] == quote:
				if strings.HasPrefix(strings.TrimLeft(value[i+1:], " \t"), "#") {
					return value[:i+1]
				}
				return value
			}
		}
		return value
	}
	for i := 1; i < len(value); i++ {
		if value[i] == '#' && (value[i-1] == ' ' || value[i-1] == '\t') {
			return strings.TrimSpace(value[:i])
		}
	}
	return value
}

// dotenvValue is a value with its quotes, and a double-quoted one's escapes,
// taken off.
func dotenvValue(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		var out strings.Builder
		inner := value[1 : len(value)-1]
		for i := 0; i < len(inner); i++ {
			if inner[i] == '\\' && i+1 < len(inner) && (inner[i+1] == '"' || inner[i+1] == '\\') {
				i++
			}
			out.WriteByte(inner[i])
		}
		return out.String()
	}
	if len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\'' {
		return value[1 : len(value)-1]
	}
	return value
}
