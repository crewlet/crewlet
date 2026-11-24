package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
)

// runClaudeNested is one firing of a Claude Code run's PostToolUse hook
// (internal/sandbox/executor's claudeCheckoutsPlugin): it claims each checkout
// cloned since the last firing, once ([handCheckouts]), and hands the listing
// of its skills ([skillsListing]) as the hook's additional context. Claude Code
// 2.1.330 loads a checkout's CLAUDE.md files and rules itself on the first read
// under it, but lists none of the skills copied during a session, nor a
// checkout's own where a read marked their directory before they existed
// (measured), so only skills are handed. A subagent's firing (its input names
// an agent_id) claims nothing: its context reaches that subagent alone. Exit 0
// whatever happened, its trouble on stderr.
func runClaudeNested(_ context.Context, f flags, stdin io.Reader, stdout, stderr io.Writer,
	_ func(string) string) int {
	var input struct {
		AgentID string `json:"agent_id"`
	}
	if err := json.NewDecoder(stdin).Decode(&input); err != nil {
		fmt.Fprintf(stderr, "notehelper: the hook's input: %v\n", err)
		return exitDone
	}
	if input.AgentID != "" {
		return exitDone
	}
	c, err := readNested(f.config)
	if err != nil {
		fmt.Fprintf(stderr, "notehelper: the nested config: %v\n", err)
		return exitDone
	}
	handed, err := handCheckouts(c.State, c.Workdir, nil, c.checkoutSpec, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "notehelper: handing a checkout's skills: %v\n", err)
	}
	var answer string
	for _, h := range handed {
		if listing := skillsListing(h); listing != "" {
			if answer != "" {
				answer += "\n\n"
			}
			answer += listing
		}
	}
	if answer == "" {
		return exitDone
	}
	out, _ := json.Marshal(map[string]any{"hookSpecificOutput": map[string]string{
		"hookEventName": "PostToolUse", "additionalContext": answer}})
	_, _ = stdout.Write(append(out, '\n'))
	return exitDone
}
