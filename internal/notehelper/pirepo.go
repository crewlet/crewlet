package main

import (
	"context"
	"fmt"
	"io"
	"strings"
)

// runPiNested is one firing of a Pi run's nested hand, which the run's
// extension runs after each tool call (internal/sandbox/executor's
// pi_extension.mjs, its tool_result handler): it hands each checkout cloned
// since the last firing, once ([handCheckouts]), printed on stdout in Pi's own
// shape ([piNestedText]) for the extension to add to the tool's result. It
// reads no input. Exit 0 whatever happened, as [runCodexNested], its trouble
// on stderr.
func runPiNested(_ context.Context, f flags, _ io.Reader, stdout, stderr io.Writer, _ func(string) string) int {
	c, err := readNested(f.config)
	if err != nil {
		fmt.Fprintf(stderr, "notehelper: the nested config: %v\n", err)
		return exitDone
	}
	handed, err := handCheckouts(c.State, c.Workdir, nil, c.checkoutSpec, &skips{stderr: stderr})
	if err != nil {
		fmt.Fprintf(stderr, "notehelper: handing a checkout's instructions: %v\n", err)
	}
	_, _ = io.WriteString(stdout, piNestedText(handed))
	return exitDone
}

// piNestedText is the checkouts handed as Pi 1.6.0 hands a session started at
// a checkout's root its instructions (measured): each file whole in
// `<project_instructions path="<file>">`, as its project context wraps one,
// under "for `<name>` with root <path>", as the launch's sections are; then
// its skills listed by path ([skillsListing]), since Pi lists no skill added
// after it started (measured). "" where they hand nothing.
func piNestedText(handed []handedCheckout) string {
	var blocks []string
	for _, h := range handed {
		files := make([]string, 0, len(h.parts))
		for _, p := range h.parts {
			files = append(files, "<project_instructions path=\""+p.path+"\">\n"+p.text+"\n</project_instructions>")
		}
		if len(files) > 0 {
			blocks = append(blocks, "for `"+h.name+"` with root "+h.root+"\n\n"+strings.Join(files, "\n\n"))
		}
		if listing := skillsListing(h); listing != "" {
			blocks = append(blocks, listing)
		}
	}
	return strings.Join(blocks, "\n\n")
}
