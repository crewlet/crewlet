package harnessprofile

import (
	"bytes"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// A `.env` credential file, read and written in the grammar Hermes Agent
// 0.23.3 reads it in (agent/secret_scope.py's load_env_file, the one tokenizer
// every reader of its .env goes through): a line per variable, blank lines and
// lines opening with `#` skipped, an optional `export ` prefix, the name
// before the first `=` with its spaces trimmed, a later line winning. A value
// in double quotes takes `\"` and `\\` escapes, one in single quotes is taken
// as written, and an unquoted one ends at a `#` that follows a space.

// parseDotenv is content's variables.
func parseDotenv(content []byte) map[string]string {
	vars := map[string]string{}
	content = bytes.TrimPrefix(content, []byte("\xef\xbb\xbf"))
	for raw := range strings.Lines(string(content)) {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if rest, ok := strings.CutPrefix(line, "export "); ok {
			line = strings.TrimLeft(rest, " \t")
		}
		key, value, found := strings.Cut(line, "=")
		if key = strings.TrimSpace(key); found && key != "" {
			vars[key] = dotenvValue(dotenvWithoutComment(value))
		}
	}
	return vars
}

// dotenvWithoutComment is value less a trailing `# ...`: after a quoted value's
// closing quote, or, unquoted, from a `#` that follows a space.
func dotenvWithoutComment(value string) string {
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
				if strings.HasPrefix(strings.TrimLeft(value[i+1:], " \t\r\n\v\f"), "#") {
					return value[:i+1]
				}
				return value
			}
		}
		return value
	}
	for i := 1; i < len(value); i++ {
		if value[i] == '#' && isSpace(value[i-1]) {
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

// encodeDotenv writes vars a line each, sorted, a value quoted as Hermes's own
// writer quotes it (hermes_cli/config.py's _quote_env_value): in double quotes,
// escaped, wherever it holds a `#`, a quote or a space.
func encodeDotenv(vars map[string]any) []byte {
	var out bytes.Buffer
	for _, key := range slices.Sorted(maps.Keys(vars)) {
		value := fmt.Sprint(vars[key])
		if strings.ContainsAny(value, "#\"'") || strings.IndexFunc(value, func(r rune) bool {
			return r < 128 && isSpace(byte(r))
		}) >= 0 {
			value = `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value) + `"`
		}
		fmt.Fprintf(&out, "%s=%s\n", key, value)
	}
	return out.Bytes()
}

// isSpace reports whether b is ASCII white space.
func isSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\v' || b == '\f'
}
