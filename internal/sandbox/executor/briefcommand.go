package executor

import (
	"strings"
	"unicode"
)

// briefNotACommand is a brief as a harness that runs a prompt beginning with
// `/` as one of its own commands is handed it (Gemini CLI headless, Qwen Code
// over ACP, Grok Build's `--prompt-file`): each trims the prompt first (the
// first two JavaScript's whitespace; Grok a space and a line break, measured)
// and tests its first character, so the leading slashes and the space around
// them go and the rest is the brief's words. Any other brief is handed byte
// for byte.
func briefNotACommand(brief string) string {
	if !strings.HasPrefix(strings.TrimLeftFunc(brief, jsSpace), "/") {
		return brief
	}
	return strings.TrimLeftFunc(brief, func(r rune) bool { return r == '/' || jsSpace(r) })
}

// jsSpace is what JavaScript's String.prototype.trim removes: Unicode white
// space, the line terminators and the byte-order mark.
func jsSpace(r rune) bool { return unicode.IsSpace(r) || r == '\ufeff' }

// commandReader is a harness that runs a brief beginning with `/` as a command
// of its own; its runner hands it the brief through [briefNotACommand].
type commandReader interface{ readsCommands() }
