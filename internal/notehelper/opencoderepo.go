package main

import (
	"context"
	"fmt"
	"io"
	"strings"
)

// listedOpenCode is the [checkoutSpec.ListedAs] of a harness that lists skills
// as OpenCode 2.3.2 does ([openCodeListing]).
const listedOpenCode = "opencode"

// openCodeListing is OpenCode 2.3.2's listing rule ([listingRule]): a skill is
// listed and keyed by its frontmatter's name as written, else its
// directory's, and of two of one name, in any of its skill directories or
// the home's, it loads neither (measured, row clashact), so a copy a home
// skill's name takes is left out. A home walked as OpenCode walks a skill
// directory: 6 deep, dot directories and node_modules skipped.
var openCodeListing = listingRule{harness: "OpenCode", walk: skillWalkOptions{Depth: 6},
	key: func(name string) string { return name }, shown: func(name string) string { return name },
	takes: func(d skillDir) (string, []string, bool) {
		n, named := frontName(entryFile(d))
		if !named || n == "" {
			n = d.name
		}
		return n, []string{n}, true
	}}

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
// within one firing's bound ([listingBudget.listings], the left out first).
// "" where they hand nothing.
func openCodeNestedText(handed []handedCheckout, skipped []string) string {
	var blocks []string
	skills, left := newListingBudget().listings(handed, skipped)
	for i, h := range handed {
		files := make([]string, 0, len(h.parts))
		for _, p := range h.parts {
			files = append(files, "Instructions from: "+p.path+"\n"+p.text)
		}
		if len(files) > 0 {
			blocks = append(blocks, "for `"+h.name+"` with root "+h.root+"\n\n"+strings.Join(files, "\n\n"))
		}
		blocks = appendNonEmpty(blocks, skills[i])
	}
	return strings.Join(appendNonEmpty(blocks, left), "\n\n")
}
