package executor

import (
	"context"
	"fmt"
	"strings"
	"unicode"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// runHome is what a harness may load from a reused box besides what its launch
// writes (Gemini CLI, Qwen Code, Codex), and so what the launch removes before
// every run: a reused box's home outlives its runs and its user can write all
// of it. The working directory is the box's own, never a checkout (a checkout
// is cloned below it), so its State directory goes too, unless it is the
// home's own State by another name ([runHome.pruneScript]).
type runHome struct {
	// State is the harness's directory under the home (`.gemini`, `.qwen`,
	// `.codex`), and Owned the entries under it that hold skills, commands,
	// rules, memory or an environment file, removed whole.
	State string
	Owned []string
	// Memory is the instruction files the harness reads from the home itself
	// and from the working directory.
	Memory []string
	// Outside is entries under the home but outside State that the harness
	// reads, removed whole (Codex's `.agents/skills`).
	Outside []string
	// WalksEnv says the harness reads the first `.env` on the walk from its
	// working directory upward (Gemini CLI to `/`, Qwen Code beside the home's
	// own): the home's and the working directory's go, and the working
	// directory is left an empty `<State>/.env`, so the walk ends where it
	// starts and no file above the box's home is read or touched. Where the
	// working directory is the home, that is the home's own `<State>/.env`,
	// the Owned entry just removed: the walk starts there. Codex reads its
	// State's `.env` alone, an Owned entry.
	WalksEnv bool
}

// pruneScript is the shell command that removes h's files from home and from
// the run's working directory, and leaves the walk's empty `<State>/.env`
// there where h walks for one.
func (h runHome) pruneScript(home string) string {
	q := shellword.Quote
	state := home + "/" + h.State
	var removed []string
	if h.WalksEnv {
		removed = append(removed, q(home+"/.env"), `"$PWD/.env"`)
	}
	for _, entry := range h.Owned {
		removed = append(removed, q(state+"/"+entry))
	}
	for _, name := range h.Memory {
		removed = append(removed, q(home+"/"+name), `"$PWD/`+name+`"`)
	}
	for _, entry := range h.Outside {
		removed = append(removed, q(home+"/"+entry))
	}
	// THE SAME DIRECTORY, NOT THE SAME STRING: the box can make its working
	// directory, or the State in either place, a link to the other, and a
	// string compare then removed the home's State, sessions and login with
	// it. `-ef` compares device and inode, whatever names reach the directory
	// (a bind mount too, which `pwd -P` strings miss); it is POSIX.1-2024 and
	// dash, busybox ash and bash all have it. The home's State, so reached, is
	// already pruned and stays. Only the State is compared: the home's twin of
	// every other working-directory entry is removed anyway, and `rm` removes
	// a link, never its target.
	inWork := `"$PWD/` + h.State + `"`
	script := "rm -rf -- " + strings.Join(removed, " ") +
		` && if ! [ ` + inWork + ` -ef ` + q(state) + ` ]; then rm -rf -- ` + inWork + `; fi`
	if h.WalksEnv {
		script += ` && mkdir -p -- ` + inWork + ` && printf '%s\n' ` + q(runEnvSeed) + ` > "$PWD/` + h.State + `/.env"`
	}
	return script
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
