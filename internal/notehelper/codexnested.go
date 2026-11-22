package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

// THE CODEX NESTED ADAPTER: a Codex run's PostToolUse hook, declared and
// trusted by the run alone (internal/sandbox/executor's codexNestedHookFlags),
// runs the helper after each tool call. It finds, by the file system, each
// repository checked out directly under the run's working directory since the
// run began, and hands its root AGENTS.override.md, else AGENTS.md, once a run,
// as the hook's additional context, within what the launch left of the run's
// budget: what Codex reads started at that checkout's root, in its own
// "# AGENTS.md instructions for <dir>" shape.

// nestedConfig is what -config names for the adapter: the run's repository
// state directory (the checkouts handed, the budget left, the lock) and the
// working directory its checkouts sit directly under.
type nestedConfig struct {
	State   string `json:"state"`
	Workdir string `json:"workdir"`
}

// maxHookInput bounds what the adapter reads of the hook's input, which it
// does not use: a tool call's response can run long.
const maxHookInput = 1 << 20

// runCodexNested is one firing of the adapter: exit 0 whatever happened, since
// a hook that fails changes nothing the run needs, its trouble on stderr.
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
	added, err := handNewCheckouts(c)
	if err != nil {
		fmt.Fprintf(stderr, "notehelper: handing a checkout's instructions: %v\n", err)
	}
	if added == "" {
		return exitDone
	}
	out, _ := json.Marshal(map[string]any{"hookSpecificOutput": map[string]string{
		"hookEventName": "PostToolUse", "additionalContext": added}})
	_, _ = stdout.Write(append(out, '\n'))
	return exitDone
}

// handNewCheckouts claims, under the state's lock, each checkout under
// c.Workdir no firing has handed yet, and is the instructions of those it
// could read within the budget left, in name order.
func handNewCheckouts(c nestedConfig) (string, error) {
	lock, err := os.OpenFile(filepath.Join(c.State, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return "", err
	}
	defer func() { _ = lock.Close() }()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return "", err
	}
	handed, err := readHanded(filepath.Join(c.State, "handed"))
	if err != nil {
		return "", err
	}
	budget, err := readBudget(filepath.Join(c.State, "budget"))
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir(c.Workdir)
	if err != nil {
		return "", err
	}
	var blocks []string
	var claimed []string
	for _, e := range entries {
		name := e.Name()
		if handed[name] || !isCheckout(filepath.Join(c.Workdir, name)) {
			continue
		}
		claimed = append(claimed, name)
		text, n := rootInstructions(filepath.Join(c.Workdir, name), budget)
		budget -= n
		if text != "" {
			blocks = append(blocks, "# AGENTS.md instructions for "+filepath.Join(c.Workdir, name)+
				"\n\n<INSTRUCTIONS>\n"+text+"\n</INSTRUCTIONS>")
		}
	}
	if len(claimed) == 0 {
		return "", nil
	}
	if err = appendHanded(filepath.Join(c.State, "handed"), claimed); err != nil {
		return "", err
	}
	if err = os.WriteFile(filepath.Join(c.State, "budget"), []byte(strconv.Itoa(budget)+"\n"), 0o600); err != nil {
		return "", err
	}
	return strings.Join(blocks, "\n\n"), nil
}

// isCheckout reports whether dir is a repository checked out there: a
// directory, never a link to one, holding .git.
func isCheckout(dir string) bool {
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() {
		return false
	}
	_, err = os.Lstat(filepath.Join(dir, ".git"))
	return err == nil
}

// rootInstructions is root's AGENTS.override.md, else AGENTS.md, as Codex reads
// it started at root, cut to budget bytes, and how many bytes it took; "" and
// 0 for none, for an empty budget, or for a file that resolves outside root.
func rootInstructions(root string, budget int) (string, int) {
	if budget <= 0 {
		return "", 0
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", 0
	}
	for _, name := range []string{"AGENTS.override.md", "AGENTS.md"} {
		path, err := filepath.EvalSymlinks(filepath.Join(root, name))
		if err != nil {
			continue
		}
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if !strings.HasPrefix(path, realRoot+string(filepath.Separator)) {
			return "", 0
		}
		raw, err := readBounded(path, int64(budget))
		if err != nil {
			return "", 0
		}
		return string(raw), len(raw)
	}
	return "", 0
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

// appendHanded adds names to the run's handed checkouts, in name order.
func appendHanded(path string, names []string) error {
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // the run's own state
	if err != nil {
		return err
	}
	slices.Sort(names)
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
