package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// museNestedInput is the part of a Muse Code hook's input muse-nested reads:
// session_id, the main session's id in a main-loop firing and a subagent's
// own thread id in its firing (measured on 1.4.25, row hooksub2).
type museNestedInput struct {
	Session string `json:"session_id"`
}

// museNestedListingCut ends a muse-nested answer's listings cut at its bound.
const museNestedListingCut = "(The rest of this listing passes what one hook answer holds and is not shown.)"

// runMuseNested is one firing of a Muse Code run's nested hook, a PostToolUse
// hook of its own in the run's plugin (internal/sandbox/executor's
// museNestedHook): after a call of the main loop, never a subagent's (its
// session_id is not MUSE_SESSION_ID), whose context is not the main agent's,
// it hands each checkout cloned since the last firing, once ([handNested]),
// as the hook's additional context ([museNestedAnswer]). Exit 0 whatever
// happened, as [runCodexNested].
func runMuseNested(_ context.Context, f flags, stdin io.Reader, stdout, stderr io.Writer,
	getenv func(string) string) int {

	raw, err := io.ReadAll(io.LimitReader(stdin, museInputBytes))
	var in museNestedInput
	if err != nil || json.Unmarshal(raw, &in) != nil {
		fmt.Fprintln(stderr, "notehelper: the hook's input is not the JSON object Muse Code hands a hook")
		return exitDone
	}
	if in.Session == "" || in.Session != getenv("MUSE_SESSION_ID") {
		return exitDone
	}
	c, err := readNested(f.config)
	if err != nil {
		fmt.Fprintf(stderr, "notehelper: the nested config: %v\n", err)
		return exitDone
	}
	handed, skipped, err := handNested(c, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "notehelper: handing a checkout's instructions: %v\n", err)
	}
	answer, dropped := museNestedAnswer(handed, skipped)
	keepSkipped(c.State, dropped, stderr)
	_, _ = io.WriteString(stdout, answer)
	return exitDone
}

// museNestedAnswer is the hook's answer, "" where it hands nothing: its
// context as a PostToolUse's additional context, every byte of it within
// [museAnswerBytes], past which Muse Code 1.4.25 delivers nothing at all
// (measured, row hook40). The context is each checkout's instructions under
// "for `<name>` with root <path>", each file wrapped and escaped as Muse puts a
// project's rules file in its reminder, whole while they fit, the checkouts first in order, and every
// other named by its files to read; then each one's skills listed by name and
// path ([skillsListing]), since Muse lists no skill copied after it started
// but reads one by either (measured, row mid); then what the firing left out
// ([skippedListing]), the listings cut where the bound falls; and a line for
// the run's skip log for each checkout the cut drops whole, claimed and so
// never handed again.
func museNestedAnswer(handed []handedCheckout, skipped []string) (string, []string) {
	room := museAnswerBytes - len(museNestedEncode(""))
	var whole, named, roots []string
	var dropped []string
	for _, h := range handed {
		if len(h.parts) == 0 {
			continue
		}
		files := make([]string, len(h.parts))
		paths := make([]string, len(h.parts))
		for i, p := range h.parts {
			text := p.text
			if !strings.HasSuffix(text, "\n") {
				text += "\n"
			}
			files[i] = museTag("project", p.path) + museEscape(text) + museFileClose
			paths[i] = p.path
		}
		whole = append(whole, "for `"+h.name+"` with root "+h.root+"\n\n"+strings.Join(files, "\n\n"))
		named = append(named, "for `"+h.name+"` with root "+h.root+": read "+strings.Join(paths, ", ")+
			" for its instructions, which pass what this answer holds")
		roots = append(roots, h.root)
	}
	// EVERY CHECKOUT NAMED FIRST, then made whole in order while the bound
	// holds: one too big for it never keeps a later one from being named.
	apart := museEscapedBytes("\n\n")
	used := apart * max(len(named)-1, 0)
	for _, n := range named {
		used += museEscapedBytes(n)
	}
	blocks := append([]string(nil), named...)
	for i := range whole {
		if grow := museEscapedBytes(whole[i]) - museEscapedBytes(named[i]); used+grow <= room {
			blocks[i], used = whole[i], used+grow
		}
	}
	budget := &escapedBudget{left: room - used, started: len(named) > 0}
	if used > room {
		budget.left, budget.started, blocks = room, false, nil
		for i, n := range named {
			b := budget.take(n)
			if b != n {
				dropped = append(dropped, fmt.Sprintf("the instructions of %s are left out of the hook's answer: "+
					"the names of the checkouts cloned since the last call pass what one answer holds", roots[i]))
			}
			if b != "" {
				blocks = append(blocks, b)
			}
		}
	}
	for _, h := range handed {
		listing := skillsListing(h)
		b := budget.take(listing)
		if b != listing {
			dropped = append(dropped, fmt.Sprintf("the skills of %s are copied but not all listed in the hook's "+
				"answer, which they pass", h.root))
		}
		if b != "" {
			blocks = append(blocks, b)
		}
	}
	if b := budget.take(skippedListing(skipped)); b != "" {
		blocks = append(blocks, b)
	}
	if len(blocks) == 0 {
		return "", dropped
	}
	return museNestedEncode(strings.Join(blocks, "\n\n")), dropped
}

// museNestedEncode is text as a PostToolUse hook's answer, with no line break
// after it, as the run's notes hooks answer (musehook.go).
func museNestedEncode(text string) string {
	var answer struct {
		Output struct {
			Event   string `json:"hookEventName"`
			Context string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	answer.Output.Event, answer.Output.Context = "PostToolUse", text
	return strings.TrimSuffix(museJSON(answer), "\n")
}

// museJSON is v encoded with `<`, `>` and `&` raw, as the answer is written:
// they then take a byte each, never six.
func museJSON(v any) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v) // strings and a struct of them cannot fail to encode
	return b.String()
}

// museEscapedBytes is the bytes s takes inside the answer's JSON string:
// JSON escapes one character at a time, so pieces joined take their sum.
func museEscapedBytes(s string) int { return len(museJSON(s)) - len(`""`+"\n") }

// escapedBudget is what is left of an answer's bound in escaped bytes, to the
// byte: Muse Code 1.4.25 delivers an answer of 16,384 bytes and nothing of
// one of 16,385 (measured, rows hc1 and hc2); whether a block was taken
// already, which the next is apart from, and whether one was cut.
type escapedBudget struct {
	left         int
	started, cut bool
}

// take is block within what is left of b, apart from the one before, if any,
// by a blank line: whole where it fits, else cut after its last line that does
// with [museNestedListingCut], once; "" for an empty block and every block
// after a cut.
func (b *escapedBudget) take(block string) string {
	if block == "" || b.cut {
		return ""
	}
	apart := 0
	if b.started {
		apart = museEscapedBytes("\n\n")
	}
	if n := museEscapedBytes(block) + apart; n <= b.left {
		b.left, b.started = b.left-n, true
		return block
	}
	b.cut = true
	room := b.left - apart - museEscapedBytes(museNestedListingCut)
	if room < 0 {
		return ""
	}
	var kept []string
	for _, line := range strings.Split(block, "\n") {
		n := museEscapedBytes(line + "\n")
		if n > room {
			break
		}
		room -= n
		kept = append(kept, line)
	}
	b.started = true
	return strings.Join(append(kept, museNestedListingCut), "\n")
}
