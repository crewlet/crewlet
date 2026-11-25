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
// since the last firing, once ([handNested]), printed on stdout in Pi's own
// shape ([piNestedText]) for the extension to add to the tool's result, with
// what it left out, all of it within -room, the characters (UTF-16 units, as
// Pi counts them) the extension found the result has left before Pi would
// compact the run. It reads no input. Exit 0 whatever happened, as
// [runCodexNested], its trouble on stderr.
func runPiNested(_ context.Context, f flags, _ io.Reader, stdout, stderr io.Writer, _ func(string) string) int {
	c, err := readNested(f.config)
	if err != nil {
		fmt.Fprintf(stderr, "notehelper: the nested config: %v\n", err)
		return exitDone
	}
	room := max(f.room, 0)
	listings := min(maxListing, room)
	c.room = &firingBound{left: room - listings, count: utf16Count}
	handed, skipped, err := handNested(c, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "notehelper: handing a checkout's instructions: %v\n", err)
	}
	_, _ = io.WriteString(stdout, piNestedText(handed, skipped, listings))
	return exitDone
}

// piNestedText is the checkouts handed as Pi 1.6.0 hands a session started at
// a checkout's root its instructions (measured): each file whole in
// `<project_instructions path="<file>">`, as its project context wraps one,
// under "for `<name>` with root <path>", as the launch's sections are; then
// its skills listed by path ([skillsListing]), since Pi lists no skill added
// after it started (measured); then what the firing left out
// ([skippedListing]), the listings within listings bytes, which no count of
// their characters passes ([listingBudget]), none where that cannot even say
// they are cut. The files take what the room leaves the listings, so the
// answer stays within the room. "" where they hand nothing.
func piNestedText(handed []handedCheckout, skipped []string, listings int) string {
	var blocks []string
	budget := newListingBudgetWithin(listings)
	budget.cut = listings < len(listingCut)+len("\n\n")
	for _, h := range handed {
		files := make([]string, 0, len(h.parts))
		for _, p := range h.parts {
			files = append(files, "<project_instructions path=\""+p.path+"\">\n"+p.text+"\n</project_instructions>")
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
