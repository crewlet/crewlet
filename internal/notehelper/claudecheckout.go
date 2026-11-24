package main

import (
	"context"
	"encoding/json"
	"errors"
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
// (measured), so only skills are handed, its config naming no instruction
// file to read (launchConfig.NestedSkillsOnly). A subagent's firing (its input
// names an agent_id) claims nothing: its context reaches that subagent alone,
// and so does a firing whose input does not read within [maxHookInput]. Exit 0
// whatever happened, its trouble on stderr.
func runClaudeNested(_ context.Context, f flags, stdin io.Reader, stdout, stderr io.Writer,
	_ func(string) string) int {
	agent, err := hookAgent(stdin)
	if err != nil {
		fmt.Fprintf(stderr, "notehelper: the hook's input: %v\n", err)
		return exitDone
	}
	if agent != "" {
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

// hookAgent is the agent_id a Claude Code hook's input names, "" for the main
// agent's, read within [maxHookInput]: its top-level keys in turn, each other
// value read past, so an agent_id ahead of a long tool_response ends the read
// at once. An input that does not read whole within the bound, or is no JSON
// object, is an error, which claims nothing.
func hookAgent(stdin io.Reader) (string, error) {
	r := io.LimitReader(stdin, maxHookInput)
	defer func() { _, _ = io.Copy(io.Discard, r) }()
	dec := json.NewDecoder(r)
	tok, err := dec.Token()
	if err != nil {
		return "", err
	}
	if tok != json.Delim('{') {
		return "", errors.New("it is not a JSON object")
	}
	for dec.More() {
		if tok, err = dec.Token(); err != nil {
			return "", err
		}
		if tok == "agent_id" {
			var agent string
			if err = dec.Decode(&agent); err != nil {
				return "", err
			}
			if agent != "" {
				return agent, nil
			}
			continue
		}
		var skip json.RawMessage
		if err = dec.Decode(&skip); err != nil {
			return "", err
		}
	}
	if _, err = dec.Token(); err != nil {
		return "", err
	}
	return "", nil
}
