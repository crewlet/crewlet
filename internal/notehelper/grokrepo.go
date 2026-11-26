package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"slices"
	"strings"
	"unicode/utf8"
)

// grokNestedChars is the most characters of a hook's answer Grok Build 1.0.92
// hands its model: past 10,000 it hands the first 10,000 and "… [+N chars]",
// each hook's answer its own reminder cut on its own (measured, rows k and
// g4k2), counting code points (row g4k3: 8,003 code points of 12,003 UTF-16
// units and 24,003 bytes reached the model whole).
const grokNestedChars = 10000

// grokListingCut ends a grok-nested answer's listings cut at
// [grokNestedChars].
const grokListingCut = "(The rest of this listing passes what one hook answer holds and is not shown.)"

// grokNestedInput is the part of a Grok Build hook's input grok-nested reads:
// subagentType, which 1.0.92 sets on a subagent's calls alone (measured,
// row k).
type grokNestedInput struct {
	SubagentType *string `json:"subagentType"`
}

// runGrokNested is one firing of a Grok Build run's nested hook, its own
// PostToolUse entry in the run's settings (internal/sandbox/executor's
// grokHooks): after a call of the main loop, never a subagent's, whose
// context is not the main agent's, it hands each checkout cloned since the
// last firing, once ([handNested]), as the hook's additional context
// ([grokNestedContext]). Exit 0 whatever happened, as [runCodexNested].
func runGrokNested(_ context.Context, f flags, stdin io.Reader, stdout, stderr io.Writer, _ func(string) string) int {
	raw, err := io.ReadAll(io.LimitReader(stdin, grokInputBytes))
	var in grokNestedInput
	if err != nil || json.Unmarshal(raw, &in) != nil {
		fmt.Fprintln(stderr, "notehelper: the hook's input is not the JSON object Grok Build hands a hook")
		return exitDone
	}
	if in.SubagentType != nil {
		return exitDone
	}
	c, err := readNested(f.config)
	if err != nil {
		fmt.Fprintf(stderr, "notehelper: the nested config: %v\n", err)
		return exitDone
	}
	c.firing = grokFiring()
	handed, skipped, err := handNested(c, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "notehelper: handing a checkout's instructions: %v\n", err)
	}
	answer := grokNestedContext(handed, skipped)
	if answer == "" {
		return exitDone
	}
	out, _ := json.Marshal(map[string]any{"hookSpecificOutput": map[string]string{
		"hookEventName": "PostToolUse", "additionalContext": answer}})
	_, _ = stdout.Write(append(out, '\n'))
	return exitDone
}

// grokNestedContext is the checkouts handed within [grokNestedChars]: each
// one's instructions under "for `<name>` with root <path>", each file as
// Grok's own `## From: <path>` (measured), whole while they fit, the
// checkouts first in order, and every other named by its files' paths to
// read; then each one's skills listed by path ([skillsListing]), since Grok
// lists a copy only some 2 s after it lands (measured, rows w and w2); then
// what the firing left out ([skippedListing]), taken first, the skills'
// listings cut where the bound falls. "" where it hands nothing.
func grokNestedContext(handed []handedCheckout, skipped []string) string {
	var whole, named []string
	for _, h := range handed {
		if len(h.parts) == 0 {
			continue
		}
		files := make([]string, len(h.parts))
		for i, p := range h.parts {
			files[i] = "## From: " + p.path + "\n" + p.text
		}
		named = append(named, grokNamed(h.name, h.root, h.parts))
		if slices.ContainsFunc(h.parts, func(p instructionPart) bool { return p.unread }) {
			whole = append(whole, named[len(named)-1]) // a file past the answer was never read
			continue
		}
		whole = append(whole, "for `"+h.name+"` with root "+h.root+"\n\n"+strings.Join(files, "\n\n"))
	}
	// EVERY CHECKOUT NAMED FIRST, then what the firing left out held room
	// for, then each made whole in order while the bound holds: one too big
	// for it never keeps a later one from being named, nor a left-out line.
	used := 0
	for _, n := range named {
		used += units(n) + 2
	}
	held := 0
	if leftOut := skippedListing(skipped); leftOut != "" {
		held = units(leftOut) + 2
	}
	blocks := make([]string, len(named))
	copy(blocks, named)
	for i := range whole {
		if grow := units(whole[i]) - units(named[i]); used+grow+held <= grokNestedChars {
			blocks[i], used = whole[i], used+grow
		}
	}
	room := &unitBudget{left: grokNestedChars - used}
	if used > grokNestedChars {
		room.left, blocks = grokNestedChars, nil
		for _, n := range named {
			if b := room.take(n); b != "" {
				blocks = append(blocks, b)
			}
		}
	}
	left := room.take(skippedListing(skipped)) // first, as [listingBudget.listings] takes it
	for _, h := range handed {
		blocks = appendNonEmpty(blocks, room.take(skillsListing(h)))
	}
	return strings.Join(appendNonEmpty(blocks, left), "\n\n")
}

// grokNamed is a claimed checkout's block where its files do not fit whole:
// the paths to read them at.
func grokNamed(name, root string, parts []instructionPart) string {
	paths := make([]string, len(parts))
	for i, p := range parts {
		paths[i] = p.path
	}
	return "for `" + name + "` with root " + root + ": read " + strings.Join(paths, ", ") +
		" for its instructions, which pass what this answer holds"
}

// grokFiring is a Grok firing's bound: no bound on its files, which
// [grokNestedContext] hands whole while they fit and names past that, a file
// past the answer's code points never read but named, and a
// names room in code points from [grokNestedChars] that every claimed
// checkout's named block, and the lines naming what was left out, are
// charged to at claim, so a checkout the answer cannot name waits for a
// later firing rather than be claimed for good unnamed.
func grokFiring() *firingBound {
	return &firingBound{left: math.MaxInt, count: units, unitBytes: utf8.UTFMax, readCap: grokNestedChars,
		nameCount: units,
		names:     grokNestedChars - units(skippedListing([]string{""})) - len("\n\n"),
		nameCost:  func(name, root string, parts []instructionPart) int { return units(grokNamed(name, root, parts)) + 2 },
		namesCap:  true}
}

// units is s's length as Grok counts it: code points.
func units(s string) int { return utf8.RuneCountInString(s) }

// unitBudget is what is left of an answer's bound in code points, and
// whether a block was cut.
type unitBudget struct {
	left int
	cut  bool
}

// take is block within what is left of b, apart from the one before by a
// blank line: whole where it fits, else cut after its last line that does
// with [grokListingCut], once; "" for an empty block and every block after a
// cut.
func (b *unitBudget) take(block string) string {
	if block == "" || b.cut {
		return ""
	}
	if n := units(block) + 2; n <= b.left {
		b.left -= n
		return block
	}
	b.cut = true
	room := b.left - 2 - units(grokListingCut)
	if room < 0 {
		return ""
	}
	var kept []string
	for _, line := range strings.Split(block, "\n") {
		n := units(line) + 1
		if n > room {
			break
		}
		room -= n
		kept = append(kept, line)
	}
	return strings.Join(append(kept, grokListingCut), "\n")
}
