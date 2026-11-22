package executor

import (
	"context"
	"fmt"
	"strings"
	"unicode"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// runHome is what a harness that discovers its configuration by walking its
// home and its working directory (Gemini CLI, Qwen Code) may load besides what
// its launch writes, and so what the launch removes before every run: a reused
// box's home outlives its runs and its user can write all of it.
//
// State is the harness's directory under the home (`.gemini`, `.qwen`); Owned
// the entries under it that hold skills, commands, rules, memory or an
// environment file, removed whole; Memory the instruction files the harness
// reads from the home itself and from the working directory. The working
// directory is the box's own, never a checkout (a checkout is cloned below
// it), so its State directory goes too where it is not the home's.
type runHome struct {
	State  string
	Owned  []string
	Memory []string
}

// pruneScript is the shell command that removes h's files from home and from
// the run's working directory, and leaves an empty `<State>/.env` there. Both
// harnesses read the first environment file on the walk from that directory
// upward (Gemini CLI to `/`), so the walk ends where it starts and no file
// above the box's home is read or touched; Qwen Code also reads the home's
// `.env` files whatever the walk found, which go with the rest.
func (h runHome) pruneScript(home string) string {
	q := shellword.Quote
	state := home + "/" + h.State
	removed := []string{q(home + "/.env")}
	for _, entry := range h.Owned {
		removed = append(removed, q(state+"/"+entry))
	}
	for _, name := range h.Memory {
		removed = append(removed, q(home+"/"+name))
	}
	inWork := []string{`"$PWD/` + h.State + `"`, `"$PWD/.env"`}
	for _, name := range h.Memory {
		inWork = append(inWork, `"$PWD/`+name+`"`)
	}
	return "rm -rf -- " + strings.Join(removed, " ") +
		` && if [ "$PWD" != ` + q(home) + ` ]; then rm -rf -- ` + strings.Join(inWork, " ") + `; fi` +
		` && mkdir -p -- "$PWD/` + h.State + `" && printf '%s\n' ` + q(runEnvSeed) + ` > "$PWD/` + h.State + `/.env"`
}

// runEnvSeed is the empty environment file's one line.
const runEnvSeed = "# Written by Crewlet: this run's own environment file, so none above it is read."

// prune runs h's [runHome.pruneScript] in box, in the working directory the
// run's job starts in (both take the box's default).
func (h runHome) prune(ctx context.Context, box sandbox.Sandbox, home, harness string) error {
	res, err := box.Exec(ctx, h.pruneScript(home), sandbox.ExecOptions{})
	if err != nil {
		return fmt.Errorf("executor: clearing what an earlier run left for %s: %w", harness, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("executor: clearing what an earlier run left for %s exited %d: %s",
			harness, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

// briefNotACommand is a brief as a harness that runs a prompt beginning with
// `/` as one of its own commands is handed it (Gemini CLI headless, Qwen Code
// over ACP): both trim the prompt first, JavaScript's whitespace, and test its
// first character, so the leading slashes and the space around them go and the
// rest is the brief's words. Any other brief is handed byte for byte.
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
