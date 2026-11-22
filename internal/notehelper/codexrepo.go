package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"
)

// THE CODEX REPOSITORY ADAPTERS hand a Codex run the root instructions of each
// repository checked out directly under its working directory: its
// AGENTS.override.md, else AGENTS.md, what Codex reads started at that root.
// codex-launch runs once, before the app-server, and writes the checkouts
// there into the run's CODEX_HOME/AGENTS.md, each labelled with its root.
// codex-nested is the run's PostToolUse hook (internal/sandbox/executor's
// codexNestedHookFlags): after each tool call it hands each checkout cloned
// since, once, as the hook's additional context, in Codex's own "# AGENTS.md
// instructions for <dir>" shape. Both claim checkouts and read their files
// through [handCheckouts], within one budget for the whole run.

// launchConfig is what -config names for codex-launch: the run's repository
// state directory (the checkouts handed, the budget left, the lock), the
// CODEX_HOME/AGENTS.md it replaces, the run's whole budget in bytes, and the
// checkouts handed first, in order (the repositories the run pushes to). The
// working directory is the adapter's own.
type launchConfig struct {
	State  string   `json:"state"`
	Agents string   `json:"agents"`
	Budget int      `json:"budget"`
	First  []string `json:"first"`
}

// nestedConfig is what -config names for codex-nested, written by
// codex-launch: the run's repository state directory and the working
// directory its checkouts sit directly under.
type nestedConfig struct {
	State   string `json:"state"`
	Workdir string `json:"workdir"`
}

// maxHookInput bounds what the hook reads of its input, which it does not
// use: a tool call's response can run long.
const maxHookInput = 1 << 20

// runCodexLaunch hands the checkouts under its working directory their
// instructions as the run starts, and writes the state codex-nested reads.
// Exit 0 once written, 1 where it could not write them.
func runCodexLaunch(_ context.Context, f flags, _ io.Reader, _, stderr io.Writer, _ func(string) string) int {
	cwd, err := os.Getwd()
	if err == nil {
		err = codexLaunch(f.config, cwd)
	}
	if err != nil {
		fmt.Fprintf(stderr, "notehelper: handing the run's repositories their instructions: %v\n", err)
		return exitError
	}
	return exitDone
}

// codexLaunch is codex-launch run in the working directory cwd.
func codexLaunch(config, cwd string) error {
	raw, err := readBounded(config, maxConfig)
	if err != nil {
		return err
	}
	var c launchConfig
	if err = json.Unmarshal(raw, &c); err != nil {
		return fmt.Errorf("the launch config: %w", err)
	}
	if c.State == "" || c.Agents == "" || c.Budget < 0 {
		return errors.New("the launch config names no state, no AGENTS.md or a negative budget")
	}
	work, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(c.State, 0o700); err != nil {
		return err
	}
	for name, body := range map[string]string{"handed": "", "budget": strconv.Itoa(c.Budget) + "\n"} {
		if err = os.WriteFile(filepath.Join(c.State, name), []byte(body), 0o600); err != nil {
			return err
		}
	}
	handed, err := handCheckouts(c.State, work, c.First)
	if err != nil {
		return err
	}
	var sections []string
	for _, h := range handed {
		if h.text != "" {
			sections = append(sections, "for `"+h.name+"` with root "+h.root+"\n\n"+h.text)
		}
	}
	if err = os.Remove(c.Agents); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if len(sections) > 0 {
		if err = os.WriteFile(c.Agents, []byte(strings.Join(sections, "\n\n")), 0o600); err != nil {
			return err
		}
	}
	nested, err := json.Marshal(nestedConfig{State: c.State, Workdir: work})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(c.State, "nested.json"), append(nested, '\n'), 0o600)
}

// runCodexNested is one firing of the hook: exit 0 whatever happened, since a
// hook that fails changes nothing the run needs, its trouble on stderr.
func runCodexNested(_ context.Context, f flags, stdin io.Reader, stdout, stderr io.Writer,
	_ func(string) string) int {
	_, _ = io.Copy(io.Discard, io.LimitReader(stdin, maxHookInput))
	raw, err := readBounded(f.config, maxConfig)
	if err != nil {
		fmt.Fprintf(stderr, "notehelper: the nested config: %v\n", err)
		return exitDone
	}
	var c nestedConfig
	if err = json.Unmarshal(raw, &c); err != nil || c.State == "" || c.Workdir == "" {
		fmt.Fprintf(stderr, "notehelper: the nested config names no state and working directory (%v)\n", err)
		return exitDone
	}
	handed, err := handCheckouts(c.State, c.Workdir, nil)
	if err != nil {
		fmt.Fprintf(stderr, "notehelper: handing a checkout's instructions: %v\n", err)
	}
	var blocks []string
	for _, h := range handed {
		if h.text != "" {
			blocks = append(blocks, "# AGENTS.md instructions for "+h.root+"\n\n<INSTRUCTIONS>\n"+h.text+"\n</INSTRUCTIONS>")
		}
	}
	if len(blocks) == 0 {
		return exitDone
	}
	out, _ := json.Marshal(map[string]any{"hookSpecificOutput": map[string]string{
		"hookEventName": "PostToolUse", "additionalContext": strings.Join(blocks, "\n\n")}})
	_, _ = stdout.Write(append(out, '\n'))
	return exitDone
}

// handedCheckout is a checkout claimed: its name under the working directory,
// its root, and its instructions, "" for none.
type handedCheckout struct {
	name, root, text string
}

// handCheckouts claims, under state's lock, each checkout under work no launch
// or firing has handed yet ([checkoutNames]), and is each one's instructions
// within the budget left, both recorded in state.
func handCheckouts(state, work string, first []string) ([]handedCheckout, error) {
	lock, err := os.OpenFile(filepath.Join(state, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	defer func() { _ = lock.Close() }()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return nil, err
	}
	seen, err := readHanded(filepath.Join(state, "handed"))
	if err != nil {
		return nil, err
	}
	budget, err := readBudget(filepath.Join(state, "budget"))
	if err != nil {
		return nil, err
	}
	names, err := checkoutNames(work, first)
	if err != nil {
		return nil, err
	}
	var handed []handedCheckout
	var claimed []string
	for _, name := range names {
		if seen[name] {
			continue
		}
		root := filepath.Join(work, name)
		text, n := rootInstructions(root, budget)
		budget -= n
		claimed = append(claimed, name)
		handed = append(handed, handedCheckout{name: name, root: root, text: text})
	}
	if len(claimed) == 0 {
		return nil, nil
	}
	if err = appendHanded(filepath.Join(state, "handed"), claimed); err != nil {
		return nil, err
	}
	if err = os.WriteFile(filepath.Join(state, "budget"), []byte(strconv.Itoa(budget)+"\n"), 0o600); err != nil {
		return nil, err
	}
	return handed, nil
}

// checkoutNames is every checkout directly under work: the names in first
// that are one, in their order, then the rest by name. A name starting with
// a dot is never one, whether first names it or not: the run's own state
// sits under such names, and the launch and the hook hold to one rule.
func checkoutNames(work string, first []string) ([]string, error) {
	var names []string
	taken := map[string]bool{}
	for _, name := range first {
		if !taken[name] && checkoutName(work, name) {
			taken[name] = true
			names = append(names, name)
		}
	}
	entries, err := os.ReadDir(work)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if name := e.Name(); !taken[name] && checkoutName(work, name) {
			names = append(names, name)
		}
	}
	return names, nil
}

// checkoutName reports whether name is a checkout directly under work: one
// path element, no leading dot and no line break (the handed file holds a
// name a line), a directory or a link to one, holding .git.
func checkoutName(work, name string) bool {
	if name == "" || strings.HasPrefix(name, ".") || strings.ContainsAny(name, "\n"+string(filepath.Separator)) {
		return false
	}
	info, err := os.Stat(filepath.Join(work, name))
	if err != nil || !info.IsDir() {
		return false
	}
	_, err = os.Lstat(filepath.Join(work, name, ".git"))
	return err == nil
}

// rootInstructions is root's AGENTS.override.md, else AGENTS.md, as Codex reads
// it started at root, links followed, cut to budget bytes on a rune boundary,
// invalid UTF-8 replaced, and how many bytes it took; "" and 0 for none, for
// an empty budget, and for a file of whitespace alone, which Codex hands no
// section and charges nothing for.
func rootInstructions(root string, budget int) (string, int) {
	if budget <= 0 {
		return "", 0
	}
	for _, name := range []string{"AGENTS.override.md", "AGENTS.md"} {
		path := filepath.Join(root, name)
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		raw, err := readBounded(path, int64(budget))
		if err != nil {
			return "", 0
		}
		if info.Size() > int64(len(raw)) {
			raw = cutAtRune(raw)
		}
		if strings.TrimSpace(string(raw)) == "" {
			return "", 0
		}
		return strings.ToValidUTF8(string(raw), "\uFFFD"), len(raw)
	}
	return "", 0
}

// cutAtRune is b, cut where a byte limit fell, less the rune the limit split.
func cutAtRune(b []byte) []byte {
	for i := 1; i <= utf8.UTFMax && i <= len(b); i++ {
		if utf8.RuneStart(b[len(b)-i]) {
			if !utf8.FullRune(b[len(b)-i:]) {
				return b[:len(b)-i]
			}
			return b
		}
	}
	return b
}

// readBounded is the first n bytes of path, or the whole file where shorter.
func readBounded(path string, n int64) ([]byte, error) {
	file, err := os.Open(path) //nolint:gosec // the run's own state, or a checkout's file Codex itself would read
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	return io.ReadAll(io.LimitReader(file, n))
}

// readHanded is the checkouts the run has handed, one name a line.
func readHanded(path string) (map[string]bool, error) {
	file, err := os.Open(path) //nolint:gosec // the run's own state
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	handed := map[string]bool{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		handed[scanner.Text()] = true
	}
	return handed, scanner.Err()
}

// appendHanded adds names to the run's handed checkouts, in the order they
// were claimed: the run's script copies their skills in that order.
func appendHanded(path string, names []string) error {
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // the run's own state
	if err != nil {
		return err
	}
	_, err = file.WriteString(strings.Join(names, "\n") + "\n")
	if cerr := file.Close(); err == nil {
		err = cerr
	}
	return err
}

// readBudget is the bytes of instructions the run has left to hand.
func readBudget(path string) (int, error) {
	raw, err := readBounded(path, 32)
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return 0, fmt.Errorf("the budget file holds %q: %w", raw, err)
	}
	return n, nil
}
