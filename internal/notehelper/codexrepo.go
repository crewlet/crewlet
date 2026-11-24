package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// maxHookInput bounds what a nested hook reads of its input, which it does not
// use: a tool call's response can run long.
const maxHookInput = 1 << 20

// runCodexNested is one firing of a Codex run's PostToolUse hook
// (internal/sandbox/executor's codexNestedHookFlags): it hands each checkout
// cloned since the last firing, once ([handCheckouts]), as the hook's
// additional context ([codexNestedContext]). Exit 0 whatever happened, since
// a hook that fails changes nothing the run needs, its trouble on stderr.
func runCodexNested(_ context.Context, f flags, stdin io.Reader, stdout, stderr io.Writer,
	_ func(string) string) int {
	_, _ = io.Copy(io.Discard, io.LimitReader(stdin, maxHookInput))
	c, err := readNested(f.config)
	if err != nil {
		fmt.Fprintf(stderr, "notehelper: the nested config: %v\n", err)
		return exitDone
	}
	handed, err := handCheckouts(c.State, c.Workdir, nil, c.checkoutSpec, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "notehelper: handing a checkout's instructions: %v\n", err)
	}
	answer := codexNestedContext(handed)
	if answer == "" {
		return exitDone
	}
	out, _ := json.Marshal(map[string]any{"hookSpecificOutput": map[string]string{
		"hookEventName": "PostToolUse", "additionalContext": answer}})
	_, _ = stdout.Write(append(out, '\n'))
	return exitDone
}

// codexNestedContext is the checkouts handed, in Codex's own shapes: each
// one's instructions as "# AGENTS.md instructions for <dir>", and its skills
// listed as Codex lists a skill (`- <name>: <description> (file: <path>)`),
// since Codex 0.171.0 lists a skill copied during a turn only from the next
// turn on (measured) and a listed skill is one the model reads by its path.
func codexNestedContext(handed []handedCheckout) string {
	var blocks []string
	for _, h := range handed {
		if h.text != "" {
			blocks = append(blocks, "# AGENTS.md instructions for "+h.root+"\n\n<INSTRUCTIONS>\n"+h.text+"\n</INSTRUCTIONS>")
		}
		if len(h.skills) == 0 {
			continue
		}
		var b strings.Builder
		b.WriteString("# Skills of " + h.root + "\n\nEach is a set of local instructions to follow, stored in " +
			"its `SKILL.md` file, to use as the other skills listed are.\n")
		for _, s := range h.skills {
			if s.description == "" {
				fmt.Fprintf(&b, "- %s (file: %s)\n", s.name, s.path)
				continue
			}
			fmt.Fprintf(&b, "- %s: %s (file: %s)\n", s.name, s.description, s.path)
		}
		blocks = append(blocks, strings.TrimSuffix(b.String(), "\n"))
	}
	return strings.Join(blocks, "\n\n")
}
