package main

import (
	"context"
	"io"
	"strings"
)

// runQwenNested is one firing of a Qwen Code run's PostToolUse hook
// (internal/sandbox/executor's qwenCheckouts): Qwen Code 0.28.4 loads nothing
// of a checkout below its start directory, ever (measured), so it hands each
// new checkout's instruction files and rules and its skills
// ([qwenNestedContext]).
func runQwenNested(_ context.Context, f flags, stdin io.Reader, stdout, stderr io.Writer,
	_ func(string) string) int {
	return runHookNested(f, stdin, stdout, stderr, "PostToolUse", qwenNestedContext)
}

// qwenNestedContext is the checkouts handed in Qwen Code's own shape: each
// file framed as it frames a context file it loads, `--- Context from: <path>
// ---` to `--- End of Context from: <path> ---` (measured on 0.28.4), its path
// absolute; then each one's skills listed ([skillsListing]), then what the
// firing left out ([skippedListing]), the listings within one bound
// ([listingBudget.listings], the left out first).
func qwenNestedContext(handed []handedCheckout, skipped []string) string {
	var blocks []string
	skills, left := newListingBudget().listings(handed, skipped)
	for i, h := range handed {
		for _, p := range h.parts {
			blocks = append(blocks, "--- Context from: "+p.path+" ---\n"+p.text+"\n--- End of Context from: "+p.path+" ---")
		}
		blocks = appendNonEmpty(blocks, skills[i])
	}
	return strings.Join(appendNonEmpty(blocks, left), "\n\n")
}
