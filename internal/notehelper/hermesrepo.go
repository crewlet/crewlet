package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// HERMES AGENT'S CHECKOUTS: Hermes 0.23.3's own code reads a checkout (its
// project context, injection scan and cut; its skill trust and quarantine), so
// the run asks it first: hermes-peek names the checkouts a firing may claim,
// the run's plugin (or, at launch, the carry script) has Hermes render each,
// and checkout-launch or hermes-nested claims them through that gate
// ([checkoutGate]), handing each whole where the room left allows, else by
// the files to read, else not yet ([hermesRoom]). The package doc's
// "Checkouts below the working directory" has the contract.

// budgetHermes is the [launchBudget] form of Hermes Agent's ephemeral system
// prompt, the launch's channel.
const budgetHermes = "hermes"

// launchBudget is the bound the launch's sections share with the text the
// harness reads beside them: Form, whose count applies ([budgetHermes]); Own,
// the file of the run's own text the sections follow, apart by a blank line
// (none where it is not there); Bytes, the bound on both.
type launchBudget struct {
	Form  string `json:"form"`
	Own   string `json:"own"`
	Bytes int    `json:"bytes"`
}

// hermesRoom is what a Hermes answer has left, in its channel's count
// ([claimRoom]): the system prompt counts bytes, a tool result Python's
// characters. sep is what the next block's separator costs (a blank line, or
// nothing where no text precedes it); where names the channel for a checkout
// it names; whole holds the roots handed whole.
type hermesRoom struct {
	left, sep int
	count     func(string) int
	where     string
	whole     map[string]bool
}

// hermesLaunchRoom is the room the ephemeral prompt leaves the launch's
// sections: b's bytes less the run's own text, as the script's $(cat) reads
// it, trailing newlines dropped, the blank line after it charged to the first
// block.
func hermesLaunchRoom(b launchBudget) (*hermesRoom, error) {
	own, err := readBounded(b.Own, int64(b.Bytes)+1)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	r := &hermesRoom{left: b.Bytes, count: func(s string) int { return len(s) }, where: "the run's system prompt",
		whole: map[string]bool{}}
	if text := strings.TrimRight(string(own), "\n"); text != "" {
		r.left, r.sep = b.Bytes-len(text), len("\n\n")
	}
	return r, nil
}

// newNestedRoom is the room a tool's result has left, room characters, the
// answer following the result after a blank line.
func newNestedRoom(room int) *hermesRoom {
	return &hermesRoom{left: room, sep: len("\n\n"), count: utf8.RuneCountInString, where: "this tool result",
		whole: map[string]bool{}}
}

// hermesLabel opens a checkout's block, as every runner's sections do.
func hermesLabel(h handedCheckout) string { return "for `" + h.name + "` with root " + h.root + "\n\n" }

// hermesNamed is h's block where its instructions are not handed whole: the
// files to read for them.
func (r *hermesRoom) hermesNamed(h handedCheckout) string {
	files := "the instruction files at its root"
	if len(h.files) > 0 {
		files = strings.Join(h.files, ", ")
	}
	return hermesLabel(h) + "read " + files + " for its instructions, which whole pass what " + r.where +
		" has room for"
}

// take reserves h's block and the separator before it: whole where it
// fits, else named, said, else nothing, and h waits; a checkout with no
// instructions takes none, while any room is left.
func (r *hermesRoom) take(h handedCheckout, said *skips) bool {
	if r.left <= 0 {
		return false
	}
	if h.text == "" {
		return true
	}
	if cost := r.count(hermesLabel(h)+h.text) + r.sep; cost <= r.left {
		r.left, r.sep = r.left-cost, len("\n\n")
		r.whole[h.root] = true
		return true
	}
	if cost := r.count(r.hermesNamed(h)) + r.sep; cost <= r.left {
		r.left, r.sep = r.left-cost, len("\n\n")
		said.say("the instructions of %s are named, not handed: whole they pass what %s has room for", h.root, r.where)
		return true
	}
	return false
}

// hermesAnswer is what a Hermes run is handed of the checkouts claimed: each
// one's instructions as its room took them ([hermesRoom.take]), then the
// skills copied ([skillsListing]) and what was left out ([skippedListing]),
// within what the room has left and [maxListing], cut as [listingBudget] cuts;
// "" for nothing.
func hermesAnswer(handed []handedCheckout, skipped []string, r *hermesRoom) string {
	var blocks []string
	for _, h := range handed {
		switch {
		case h.text == "":
		case r.whole[h.root]:
			blocks = append(blocks, hermesLabel(h)+h.text)
		default:
			blocks = append(blocks, r.hermesNamed(h))
		}
	}
	if r.left < len(listingCut)+len("\n\n") {
		return strings.Join(blocks, "\n\n") // no room even to say a listing is cut
	}
	// IN BYTES, which no character count passes, from what the blocks left.
	budget := newListingBudgetWithin(min(maxListing, r.left))
	for _, h := range handed {
		if listing := budget.take(skillsListing(h)); listing != "" {
			blocks = append(blocks, listing)
		}
	}
	if listing := budget.take(skippedListing(skipped)); listing != "" {
		blocks = append(blocks, listing)
	}
	return strings.Join(blocks, "\n\n")
}

// peekConfig is what hermes-peek reads of a launch's or a nested adapter's
// config: the state, the working directory (a launch's is its own) and the
// checkouts handed first.
type peekConfig struct {
	State   string   `json:"state"`
	Workdir string   `json:"workdir"`
	First   []string `json:"first"`
}

// runHermesPeek prints, as {"roots": [...]} (with -lines, a root a line),
// the root of each checkout a firing could claim now: finished
// ([cloneFinished]), not handed, in [checkoutNames]' order, none once the run
// is ended. It claims nothing, so the gate the run's Hermes then answers for
// them is read before any claim. Exit 1 where it cannot say.
func runHermesPeek(_ context.Context, f flags, _ io.Reader, stdout, stderr io.Writer, _ func(string) string) int {
	roots, err := hermesPeek(f.config)
	switch {
	case err != nil:
	case f.lines:
		for _, root := range roots {
			if _, err = fmt.Fprintln(stdout, root); err != nil {
				break
			}
		}
	default:
		err = json.NewEncoder(stdout).Encode(map[string][]string{"roots": roots})
	}
	if err != nil {
		fmt.Fprintf(stderr, "notehelper: naming the checkouts a firing may claim: %v\n", err)
		return exitError
	}
	return exitDone
}

// hermesPeek is [runHermesPeek]'s roots under the config at path.
func hermesPeek(path string) ([]string, error) {
	raw, err := readBounded(path, maxConfig)
	if err != nil {
		return nil, err
	}
	var c peekConfig
	if err = json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("the config: %w", err)
	}
	if c.State == "" {
		return nil, errors.New("the config names no state")
	}
	work := c.Workdir
	if work == "" {
		var cwd string
		if cwd, err = os.Getwd(); err != nil {
			return nil, err
		}
		if work, err = filepath.EvalSymlinks(cwd); err != nil {
			return nil, err
		}
	}
	roots := []string{}
	if _, err = os.Lstat(filepath.Join(c.State, endedMarker)); err == nil {
		return roots, nil
	}
	seen, err := readHanded(filepath.Join(c.State, "handed"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	was := map[string]bool{}
	for _, name := range seen {
		was[name] = true
	}
	names, err := checkoutNames(work, c.First)
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		if root := filepath.Join(work, name); !was[name] && cloneFinished(root) {
			roots = append(roots, root)
		}
	}
	return roots, nil
}

// runHermesNested is one firing of a Hermes run's nested hand, which the
// run's plugin runs after each of the main loop's tool calls (internal/
// sandbox/executor's hermes_plugin.py, its transform_tool_result) with the
// gate its Hermes answered for the checkouts hermes-peek named on stdin and
// -room, the characters the tool's result has left before Hermes would spill
// it to a file: it claims each checkout whose block fits ([hermesRoom]), the
// rest waiting for a later firing, and prints what it hands
// ([hermesAnswer]) for the plugin to add to the result. Exit 0 whatever
// happened, as [runCodexNested], its trouble on stderr.
func runHermesNested(_ context.Context, f flags, stdin io.Reader, stdout, stderr io.Writer, _ func(string) string) int {
	c, err := readNested(f.config)
	if err == nil && !c.Gated {
		err = errors.New("it names no gate, which a Hermes firing reads")
	}
	if err == nil {
		c.gate, err = readGate(stdin)
	}
	if err != nil {
		fmt.Fprintf(stderr, "notehelper: the nested config: %v\n", err)
		return exitDone
	}
	room := newNestedRoom(f.room)
	c.room = room
	handed, skipped, err := handNested(c, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "notehelper: handing a checkout's instructions: %v\n", err)
	}
	_, _ = io.WriteString(stdout, hermesAnswer(handed, skipped, room))
	return exitDone
}
