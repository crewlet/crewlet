package main

import (
	"context"
	"fmt"
	"io"
	"strings"
)

// runOpenCodeNested is one firing of an OpenCode run's nested hand, which the
// run's checkouts plugin runs after each tool call of the run's own session
// (internal/sandbox/executor's opencode_checkouts.js): it hands each checkout
// cloned since the last firing, once ([handNested]), printed on stdout in
// OpenCode's own shape ([openCodeNestedText]) for the plugin to add to the
// tool's result, with what it left out. It reads no input. Exit 0 whatever
// happened, as [runCodexNested], its trouble on stderr.
func runOpenCodeNested(_ context.Context, f flags, _ io.Reader, stdout, stderr io.Writer, _ func(string) string) int {
	c, err := readNested(f.config)
	if err != nil {
		fmt.Fprintf(stderr, "notehelper: the nested config: %v\n", err)
		return exitDone
	}
	handed, skipped, err := handNested(c, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "notehelper: handing a checkout's instructions: %v\n", err)
	}
	_, _ = io.WriteString(stdout, openCodeNestedText(handed, skipped))
	return exitDone
}

// openCodeNestedText is the checkouts handed as OpenCode 2.3.2 hands a
// session started at a checkout's root its instruction file (measured): the
// file whole after "Instructions from: <file>", under "for `<name>` with root
// <path>", as the launch's sections are; then its skills listed by path
// ([skillsListing]), since OpenCode lists no skill added after it started
// (measured); then what the firing left out ([skippedListing]), the listings
// within one firing's bound ([listingBudget]). "" where they hand nothing.
func openCodeNestedText(handed []handedCheckout, skipped []string) string {
	var blocks []string
	budget := newListingBudget()
	for _, h := range handed {
		files := make([]string, 0, len(h.parts))
		for _, p := range h.parts {
			files = append(files, "Instructions from: "+p.path+"\n"+p.text)
		}
		if len(files) > 0 {
			blocks = append(blocks, "for `"+h.name+"` with root "+h.root+"\n\n"+strings.Join(files, "\n\n"))
		}
		if listing := budget.take(skillsListing(h)); listing != "" {
			blocks = append(blocks, listing)
		}
	}
	if listing := budget.take(skippedListing(skipped)); listing != "" {
		blocks = append(blocks, listing)
	}
	return strings.Join(blocks, "\n\n")
}
