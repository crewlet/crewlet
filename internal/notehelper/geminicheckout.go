package main

import (
	"context"
	"io"
)

// runGeminiNested is one firing of a Gemini CLI run's AfterTool hook
// (internal/sandbox/executor's geminiCheckouts): Gemini CLI 0.70.0 loads a
// checkout's context files itself on the first read under it, a clone made
// mid-run included, but lists no skill it did not start with (measured), so
// it hands each new checkout's skills alone, in Claude Code's listing
// ([claudeNestedContext]), its config naming no instruction file to read. The
// run's subagents are off, so every firing is the main loop's.
func runGeminiNested(_ context.Context, f flags, stdin io.Reader, stdout, stderr io.Writer,
	_ func(string) string) int {
	return runHookNested(f, stdin, stdout, stderr, "AfterTool", claudeNestedContext)
}
