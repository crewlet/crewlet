package main

import (
	"context"
	"fmt"
	"io"
	"strings"
)

// listedKimi is the [checkoutSpec.ListedAs] of a harness that lists skills as
// Kimi Code 2.7.2 does ([kimiListing]).
const listedKimi = "kimi"

// kimiListing is Kimi Code 2.7.2's listing rule ([listingRule]): a skill is
// listed by its frontmatter's name, a loose `.md` at a root's top by its
// file's less `.md` where it names none, a SKILL.md naming none not at all;
// names are keyed lower-cased, and of two alike the first root's wins, the
// run's skills directory before ~/.agents/skills (C 2.7.2: normalizeSkillName,
// parseSkillText, userRoots). A home walked as Kimi walks one: dot directories
// holding SKILL.md are skills (walked into, more than Kimi does, which only
// keeps a copy out), 8 deep at most, nothing refused past that.
var kimiListing = listingRule{harness: "Kimi Code", walk: skillWalkOptions{Every: true, Depth: 8}, loose: true,
	key: strings.ToLower, shown: func(name string) string { return name },
	takes: func(d skillDir) (string, []string, bool) {
		n, named := frontName(entryFile(d))
		if !named || n == "" {
			if !d.command {
				return "", nil, true
			}
			n = d.name
		}
		return n, []string{strings.ToLower(n)}, true
	}}

// runKimiNested is one firing of a Kimi Code run's nested hand, which the
// run's driver runs after each of the main loop's tool calls ends
// (internal/sandbox/executor's kimi_driver.js, on `item.ended`): it hands each
// checkout cloned since the last firing, once ([handNested]), printed on
// stdout in Kimi's own shape ([kimiNestedText]) for the driver to steer into
// the session, with what it left out. It reads no input. Exit 0 whatever
// happened, as [runCodexNested], its trouble on stderr.
func runKimiNested(_ context.Context, f flags, _ io.Reader, stdout, stderr io.Writer, _ func(string) string) int {
	c, err := readNested(f.config)
	if err != nil {
		fmt.Fprintf(stderr, "notehelper: the nested config: %v\n", err)
		return exitDone
	}
	handed, skipped, err := handNested(c, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "notehelper: handing a checkout's instructions: %v\n", err)
	}
	_, _ = io.WriteString(stdout, kimiNestedText(handed, skipped))
	return exitDone
}

// kimiNestedText is the checkouts handed as Kimi Code 2.7.2 renders the
// instruction files of a session started at a checkout's root (measured):
// each file's text trimmed, after `<!-- From: <file> -->`, under "for
// `<name>` with root <path>", as the launch's sections are; then its skills
// listed by path ([skillsListing]), since Kimi lists no skill added after it
// started (measured); then what the firing left out ([skippedListing]), the
// listings within one firing's bound ([listingBudget]). "" where they hand
// nothing.
func kimiNestedText(handed []handedCheckout, skipped []string) string {
	var blocks []string
	budget := newListingBudget()
	for _, h := range handed {
		files := make([]string, 0, len(h.parts))
		for _, p := range h.parts {
			files = append(files, "<!-- From: "+p.path+" -->\n"+strings.TrimSpace(p.text))
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
