package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// cursorResultInline is the most bytes the MCP call's result may come to with
// the checkouts' item appended: Cursor Agent 2026.11.22-e9042b2 puts a result
// of 39,043 bytes in the model's next request whole, and moves one of 40,043
// bytes or more (to 3 MB) to a file, the call's own text with it, the model
// told only the file's path (measured). Counted as the hook's tool_output plus
// the item, past the text items Cursor joins, so the answer stays under it.
const cursorResultInline = 39_000

// cursorRoomFloor is the least room a firing hands in: below it, an MCP call's
// own result leaves too little beside it for one checkout's file and listing,
// so the firing claims nothing and the clone waits for the next MCP call,
// whose result may be shorter.
const cursorRoomFloor = 8 << 10

// cursorNestedInput is the part of a Cursor Agent postToolUse hook's input
// cursor-nested reads: the tool (`MCP:<tool>` for an MCP call), its result as
// the JSON text Cursor hands the hook, and the conversation the call is in:
// the run's session, or a subagent's own, which Cursor names nothing else by
// (measured on 2026.11.22-e9042b2: no parent field; conversation, session and
// generation ids all the subagent's).
type cursorNestedInput struct {
	ToolName     string `json:"tool_name"`
	ToolOutput   string `json:"tool_output"`
	Conversation string `json:"conversation_id"`
}

// cursorConversation is the file in the run's checkout state its script
// writes the run's session id to once it has settled it (internal/sandbox/
// executor's cursorScript): a firing claims only in that conversation, so a
// subagent's call, whose context ends with its answer, hands the main loop no
// clone it will never see; a firing with no such file claims nothing.
const cursorConversation = "conversation"

// runCursorNested is one firing of a Cursor Agent run's postToolUse hook
// (internal/sandbox/executor's cursorNestedCommand): at an MCP call of the
// main agent whose result leaves room ([cursorRoom]), it claims each checkout
// cloned since the last firing, once ([handNested]), by [cursorClaimWithin],
// and answers the call's result as Cursor handed it with one text item more
// ([cursorAppended]), the only part of a hook's answer that reaches the model
// mid-run (measured on 2026.11.22-e9042b2). Any other call, one outside the
// run's conversation ([cursorConversation]), a result it cannot keep whole,
// and too little room answer {} and claim nothing. Exit 0 whatever happened, its trouble on stderr.
func runCursorNested(_ context.Context, f flags, stdin io.Reader, stdout, stderr io.Writer, _ func(string) string) int {
	answer := cursorNestedAnswer(f, stdin, stderr, time.Now().Add(cursorClaimWithin))
	_, _ = stdout.Write(append(answer, '\n'))
	return exitDone
}

// cursorClaimWithin is how long a firing may take, from its start, to claim:
// past it, it claims nothing and the clone waits for the next MCP call, since
// Cursor drops an answer past its hook's timeout (measured) and a clone
// claimed into a dropped answer is never handed. A whole firing handing a
// clone of 200 skills and a 30 KB AGENTS.md took 0.63 s on the build VM (one
// with nothing new, 9 ms); the runner's timeout is twice this
// (internal/sandbox/executor's cursorHookTimeout), so what follows a claim has
// as long again.
const cursorClaimWithin = 10 * time.Second

// cursorNestedAnswer is the hook's answer, claiming only by claimBy: {} where
// it hands nothing.
func cursorNestedAnswer(f flags, stdin io.Reader, stderr io.Writer, claimBy time.Time) []byte {
	none := []byte("{}")
	raw, err := io.ReadAll(io.LimitReader(stdin, maxHookInput+1))
	if err != nil || len(raw) > maxHookInput {
		fmt.Fprintln(stderr, "notehelper: the hook's input does not read within its bound")
		return none
	}
	var in cursorNestedInput
	if err = json.Unmarshal(raw, &in); err != nil {
		fmt.Fprintf(stderr, "notehelper: the hook's input: %v\n", err)
		return none
	}
	if !strings.HasPrefix(in.ToolName, "MCP:") {
		return none
	}
	room := cursorRoom(in.ToolOutput)
	if room < cursorRoomFloor {
		return none
	}
	if _, kept := cursorAppended(in.ToolOutput, ""); !kept {
		return none
	}
	c, err := readNested(f.config)
	if err != nil {
		fmt.Fprintf(stderr, "notehelper: the nested config: %v\n", err)
		return none
	}
	run, err := readBounded(filepath.Join(c.State, cursorConversation), 256)
	if err != nil || in.Conversation == "" || strings.TrimSpace(string(run)) != in.Conversation {
		return none
	}
	c.claimBy = claimBy
	listings := room / 4
	firing := room - listings
	if c.FiringLimit != nil && *c.FiringLimit < firing {
		firing = *c.FiringLimit
	}
	// ITS NAMES ARE THE LISTINGS' SHARE: a checkout whose left-out files that
	// share cannot name waits for a call with room, or cursor-left names it.
	c.firing = &firingBound{left: firing, count: byteCount, unitBytes: 1,
		names: newListingBudgetWithin(listings).namesWithin(), namesCap: listings >= maxListing}
	handed, skipped, err := handNested(c, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "notehelper: handing a checkout: %v\n", err)
	}
	text := cursorNestedText(handed, skipped, listings)
	if text == "" {
		return none
	}
	answer, _ := cursorAppended(in.ToolOutput, text)
	return answer
}

// cursorRoom is what is left for the checkouts' item beside an MCP call's
// result, output as Cursor hands it: [cursorResultInline] less the result and
// the line Cursor joins the item to it with.
func cursorRoom(output string) int { return cursorResultInline - len(output) - 1 }

// cursorAppended is the answer handing text as one more text item of the MCP
// call's result output, and whether output can be kept whole: an object whose
// content is an array of text items. Every key and every item goes back as the
// bytes Cursor handed, never decoded and encoded again, the item last; a
// non-text item would not, since Cursor reads an answer's other items back as
// their JSON text (read from the 2026.11.22-e9042b2 bundle), so a result
// holding one is left alone.
func cursorAppended(output, text string) ([]byte, bool) {
	var result map[string]json.RawMessage
	if err := json.Unmarshal([]byte(output), &result); err != nil || result == nil {
		return nil, false
	}
	var items []json.RawMessage
	if err := json.Unmarshal(result["content"], &items); err != nil || items == nil {
		return nil, false
	}
	for _, item := range items {
		var part struct {
			Type string  `json:"type"`
			Text *string `json:"text"`
		}
		if err := json.Unmarshal(item, &part); err != nil || part.Type != "text" || part.Text == nil {
			return nil, false
		}
	}
	added, _ := json.Marshal(map[string]string{"type": "text", "text": text})
	var b bytes.Buffer
	b.WriteString(`{"updated_mcp_tool_output":{`)
	for i, key := range slices.Sorted(maps.Keys(result)) {
		if i > 0 {
			b.WriteByte(',')
		}
		name, _ := json.Marshal(key)
		b.Write(name)
		b.WriteByte(':')
		if key != "content" {
			b.Write(result[key])
			continue
		}
		b.WriteByte('[')
		for _, item := range items {
			b.Write(item)
			b.WriteByte(',')
		}
		b.Write(added)
		b.WriteByte(']')
	}
	b.WriteString("}}")
	return b.Bytes(), true
}

// cursorNestedText is the checkouts handed as a Cursor run's launch hands its
// checkouts present then ([checkoutSection], the rule the runner writes them
// in), each one's skills listed ([skillsListing]), since Cursor lists no skill
// added after it starts (measured), then what the firing left out
// ([skippedListing]), the listings within listings bytes, the left out taken
// first ([listingBudget.listings]). "" where it hands nothing.
func cursorNestedText(handed []handedCheckout, skipped []string, listings int) string {
	var blocks []string
	skills, left := newListingBudgetWithin(listings).listings(handed, skipped)
	for i, h := range handed {
		if h.text != "" {
			blocks = append(blocks, checkoutSection(h))
		}
		blocks = appendNonEmpty(blocks, skills[i])
	}
	return strings.Join(appendNonEmpty(blocks, left), "\n\n")
}

// cursorLeftSaid marks, in the run's checkout state, that cursor-left has said
// the clones the run never handed, so the end's every path says them once.
const cursorLeftSaid = "left-said"

// runCursorLeft names in the skip log ([skippedLog]) each finished clone under
// the working directory no launch or firing claimed, a line each, once the
// run has ended ([endedMarker]), so a clone no MCP call with room followed is
// said rather than silently never handed: on Cursor a firing comes only at an
// MCP call ([runCursorNested]), where the other runners fire after every
// tool. Once per run: [cursorLeftSaid] is made only once its lines are
// written, so a pass that fails is tried again at the end's next path. Exit 0
// whatever happened.
func runCursorLeft(_ context.Context, f flags, _ io.Reader, _, stderr io.Writer, _ func(string) string) int {
	c, err := readNested(f.config)
	if err != nil {
		fmt.Fprintf(stderr, "notehelper: the nested config: %v\n", err)
		return exitDone
	}
	if err = cursorLeft(c.State, c.Workdir); err != nil {
		fmt.Fprintf(stderr, "notehelper: naming the clones the run did not hand: %v\n", err)
	}
	return exitDone
}

// cursorLeft adds to the skip log a line for each finished clone under work no
// launch or firing claimed, by name, then marks it said ([cursorLeftSaid]):
// nothing before a run's state is ended, as a firing may still claim, nor
// once it is said. A checkout present but unfinished at launch reads as one
// that finished after it.
func cursorLeft(state, work string) error {
	for _, marker := range []string{endedMarker, cursorLeftSaid} {
		_, err := os.Lstat(filepath.Join(state, marker))
		if marker == endedMarker && errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if marker == cursorLeftSaid && err == nil {
			return nil
		}
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	handed, err := readHanded(filepath.Join(state, "handed"))
	if err != nil {
		return err
	}
	names, err := checkoutNames(work, nil)
	if err != nil {
		return err
	}
	var lines []string
	for _, name := range names {
		if !slices.Contains(handed, name) && cloneFinished(filepath.Join(work, name)) {
			lines = append(lines, fmt.Sprintf("the checkout %s was not handed: its clone finished after the "+
				"run's launch, and no MCP call with room for it came after that", filepath.Join(work, name)))
		}
	}
	if len(lines) > 0 {
		if err = appendSkipped(filepath.Join(state, skippedLog), lines); err != nil {
			return err
		}
	}
	return os.WriteFile(filepath.Join(state, cursorLeftSaid), nil, 0o600)
}
