package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// cursorSpec is the spec a Cursor Agent run's launch gives the core
// (internal/sandbox/executor's cursorCheckouts), its firings' skills in skills.
func cursorSpec(skills string) checkoutSpec {
	return checkoutSpec{Instructions: names([]string{"AGENTS.md"}, []string{"CLAUDE.md"}, []string{".cursorrules"}),
		SkillDirs: []skillDirSlot{{Dir: ".cursor/skills"}, {Dir: ".claude/skills"}, {Dir: ".agents/skills"}},
		Skills:    skills, Copied: filepath.Join(filepath.Dir(skills), "copied"), Reserved: []string{"crewlet"}}
}

// cursorLaunch runs checkout-launch in r's working directory under Cursor's
// spec, the launch copying no skill.
func (r repoRun) cursorLaunch(t *testing.T) {
	t.Helper()
	raw, _ := json.Marshal(launchConfig{State: r.state, Agents: r.agents, LaunchSkillsLoaded: true,
		checkoutSpec: cursorSpec(r.skills)})
	config := filepath.Join(t.TempDir(), "launch.json")
	if err := os.WriteFile(config, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	var errs bytes.Buffer
	if err := checkoutLaunch(config, r.work, strings.NewReader(""), &errs); err != nil || errs.Len() > 0 {
		t.Fatalf("the launch failed: %v\n%s", err, errs.String())
	}
	if err := os.WriteFile(filepath.Join(r.state, cursorConversation), []byte(cursorRunConversation+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// cursorRunConversation is the run's session id, its script's
// [cursorConversation]; a subagent's call is in a conversation of its own.
const cursorRunConversation = "11111111-2222-4333-8444-555555555555"

// cursorInput is a postToolUse hook's input as Cursor writes it: the tool,
// its result as JSON text, and the conversation the call is in, its session
// and generation ids the same (measured), the run's where conversation is "".
func cursorInput(tool, output, conversation string) string {
	if conversation == "" {
		conversation = cursorRunConversation
	}
	in := map[string]any{"conversation_id": conversation, "session_id": conversation,
		"generation_id": conversation, "tool_name": tool, "tool_input": map[string]any{}, "tool_output": output,
		"duration": 12.5, "tool_use_id": "u", "hook_event_name": "postToolUse"}
	raw, _ := json.Marshal(in)
	return string(raw)
}

// cursorFire runs one cursor-nested firing on input: its answer and what it
// said.
func (r repoRun) cursorFire(t *testing.T, input string) (answer, said string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := runCursorNested(context.Background(), flags{config: filepath.Join(r.state, "nested.json")},
		strings.NewReader(input), &stdout, &stderr, os.Getenv); code != exitDone {
		t.Fatalf("the firing exited %d: %s", code, stderr.String())
	}
	return strings.TrimSuffix(stdout.String(), "\n"), stderr.String()
}

// cursorResult is an MCP result as Cursor hands the hook one: two text items,
// written with the escapes and spacing a server may send, and isError.
const cursorResult = `{"content":[{"type":"text","text":"ORIG \"quoted\"\nline2 é"},` +
	`{"text": "second",  "type":"text","annotations":{"audience":["user"]}}],"isError":false,"_meta":{"k":[1, 2]}}`

// cursorUpdated is answer's updated_mcp_tool_output, by key, its content by
// item, each as the bytes the answer holds.
func cursorUpdated(t *testing.T, answer string) (map[string]json.RawMessage, []json.RawMessage) {
	t.Helper()
	var outer map[string]json.RawMessage
	if err := json.Unmarshal([]byte(answer), &outer); err != nil {
		t.Fatalf("the answer %q is no JSON object: %v", answer, err)
	}
	var updated map[string]json.RawMessage
	if err := json.Unmarshal(outer["updated_mcp_tool_output"], &updated); err != nil || len(outer) != 1 {
		t.Fatalf("the answer %q holds no updated_mcp_tool_output alone: %v", answer, err)
	}
	var items []json.RawMessage
	if err := json.Unmarshal(updated["content"], &items); err != nil {
		t.Fatalf("the answer's content does not read: %v", err)
	}
	return updated, items
}

// A CLONE IS HANDED AT THE NEXT MCP CALL, APPENDED TO ITS RESULT, ONCE: the
// first firing at an MCP call after a checkout is cloned answers the call's
// result as Cursor handed it, every key and every content item the same
// bytes, with one text item more: the clone's files as the launch's section
// ([checkoutSection]) and its skills listed by their copies' paths. A
// checkout present at launch is never in it, and a later call is handed
// nothing.
//
// Mutation: re-encode an item or key, drop one, put the item first, hand the
// launch's checkout, or claim nothing, and this goes red.
func TestACursorCloneIsAppendedToTheNextMCPResultOnce(t *testing.T) {
	t.Parallel()
	r := newRepoRun(t, map[string]string{"first/.git/": "", "first/AGENTS.md": "FIRST\n"})
	r.cursorLaunch(t)
	if answer, said := r.cursorFire(t, cursorInput("MCP:t_tool", cursorResult, "")); answer != "{}" || said != "" {
		t.Errorf("a firing with no new checkout answered %q, said %q", answer, said)
	}
	r.add(t, map[string]string{"late/.git/": "", "late/AGENTS.md": "LATE-AGENTS\n", "late/CLAUDE.md": "LATE-CLAUDE\n",
		"late/.cursor/skills/sk/SKILL.md": "---\nname: sk\ndescription: Dsk.\n---\nBody.\n"})
	answer, said := r.cursorFire(t, cursorInput("MCP:t_tool", cursorResult, ""))
	if said != "" {
		t.Errorf("the firing said %q", said)
	}
	updated, items := cursorUpdated(t, answer)
	var original map[string]json.RawMessage
	var originals []json.RawMessage
	if err := json.Unmarshal([]byte(cursorResult), &original); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(original["content"], &originals); err != nil {
		t.Fatal(err)
	}
	for key, raw := range original {
		if key != "content" && !bytes.Equal(updated[key], raw) {
			t.Errorf("the result's %s came back as %s, want the bytes %s", key, updated[key], raw)
		}
	}
	if len(updated) != len(original) {
		t.Errorf("the answer holds keys %v, the result %v", keysOf(updated), keysOf(original))
	}
	if len(items) != len(originals)+1 {
		t.Fatalf("the answer holds %d items, want the result's %d and one more", len(items), len(originals))
	}
	for i, raw := range originals {
		if !bytes.Equal(items[i], raw) {
			t.Errorf("item %d came back as %s, want the bytes %s", i, items[i], raw)
		}
	}
	root := filepath.Join(r.workdir(t), "late")
	want := checkoutHeader("late", root) + "\n\nLATE-AGENTS\n\n\nLATE-CLAUDE\n\n\n" + "# Skills of " + root +
		"\n\nEach is a set of local instructions to follow, stored in its `SKILL.md` file; to use one, read that file." +
		"\n- late:sk: Dsk. (file: " + r.skills + "/late:sk/SKILL.md)"
	var added struct{ Type, Text string }
	if err := json.Unmarshal(items[len(items)-1], &added); err != nil || added.Type != "text" {
		t.Fatalf("the last item %s is no text item: %v", items[len(items)-1], err)
	}
	if added.Text != want {
		t.Errorf("the item hands\n%q\nwant\n%q", added.Text, want)
	}
	if strings.Contains(added.Text, "FIRST") {
		t.Errorf("the launch's checkout is handed again: %q", added.Text)
	}
	if answer, _ := r.cursorFire(t, cursorInput("MCP:t_tool", cursorResult, "")); answer != "{}" {
		t.Errorf("a later MCP call is handed the clone again: %q", answer)
	}
}

// keysOf is m's keys, for a message.
func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// A FIRING PAST ITS DEADLINE CLAIMS NOTHING: a firing slowed past
// [cursorClaimWithin] (here started with it spent) answers {} and leaves the
// clone unclaimed, its lines unsaid, so the next MCP call hands it rather
// than Cursor dropping a late answer that claimed it.
//
// Mutation: drop the deadline's check, or keep its lines, and this goes red.
func TestACursorFiringPastItsDeadlineClaimsNothing(t *testing.T) {
	t.Parallel()
	r := newRepoRun(t, nil)
	r.cursorLaunch(t)
	r.add(t, map[string]string{"late/.git/": "", "late/AGENTS.md": "LATE\n",
		"late/.cursor/skills/s/SKILL.md": "---\nname: s\ndescription: S.\n---\n",
		"late/.cursor/skills/out/":       ""})
	// A SKILL LEADING OUT OF ITS CHECKOUT, which a claim says is left out.
	outside := filepath.Join(t.TempDir(), "SKILL.md")
	if err := os.WriteFile(outside, []byte("---\nname: out\ndescription: Out.\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(r.work, "late/.cursor/skills/out/SKILL.md")); err != nil {
		t.Fatal(err)
	}
	var errs bytes.Buffer
	spent := time.Now().Add(-time.Second)
	if answer := cursorNestedAnswer(flags{config: filepath.Join(r.state, "nested.json")},
		strings.NewReader(cursorInput("MCP:t_tool", cursorResult, "")), &errs, spent); string(answer) != "{}" {
		t.Errorf("a firing past its deadline answered %q, want {}", answer)
	}
	if kept, _ := os.ReadFile(filepath.Join(r.state, skippedLog)); len(kept) > 0 {
		t.Errorf("a firing past its deadline kept lines for a clone it did not claim: %q", kept)
	}
	answer, _ := r.cursorFire(t, cursorInput("MCP:t_tool", cursorResult, ""))
	if _, items := cursorUpdated(t, answer); !strings.Contains(string(items[len(items)-1]), "LATE") ||
		strings.Count(string(items[len(items)-1]), "/out is left out") != 1 {
		t.Errorf("the next MCP call is not handed the clone, its left-out skill said once: %q", answer)
	}
	if kept, _ := os.ReadFile(filepath.Join(r.state, skippedLog)); strings.Count(string(kept), "/out is left out") != 1 {
		t.Errorf("the skip log says the left-out skill other than once: %q", kept)
	}
}

// A FIRING THAT CANNOT APPEND CLAIMS NOTHING: a call that is no MCP call (its
// result Cursor never takes back, however it reads), a subagent's (in a
// conversation not the run's, as Cursor marks one, measured), a result
// holding an item that is no text (Cursor would read it back as its JSON
// text), one that does not read, and one that leaves less room than
// [cursorRoomFloor] each answer {} and leave the clone for the next MCP call
// of the run's own conversation, which hands it.
//
// Mutation: claim before any of these checks, or drop one, and its row goes
// red.
func TestACursorFiringThatCannotAppendClaimsNothing(t *testing.T) {
	t.Parallel()
	long := `{"content":[{"type":"text","text":"` + strings.Repeat("x", cursorResultInline-cursorRoomFloor) + `"}]}`
	for name, input := range map[string]string{
		"a shell call":    cursorInput("Shell", cursorResult, ""),
		"a subagent's":    cursorInput("MCP:t_tool", cursorResult, "ed80bdf2-8b7c-4f18-ace4-80baa047c84e"),
		"an image item":   cursorInput("MCP:t_tool", `{"content":[{"type":"image","data":"AA==","mimeType":"image/png"}]}`, ""),
		"no object":       cursorInput("MCP:t_tool", `"just text"`, ""),
		"no content":      cursorInput("MCP:t_tool", `{"isError":true}`, ""),
		"too little room": cursorInput("MCP:t_tool", long, ""),
		"no JSON input":   "{not json",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newRepoRun(t, nil)
			r.cursorLaunch(t)
			r.add(t, map[string]string{"late/.git/": "", "late/AGENTS.md": "LATE\n"})
			if answer, _ := r.cursorFire(t, input); answer != "{}" {
				t.Errorf("the firing answered %q, want {}", answer)
			}
			answer, _ := r.cursorFire(t, cursorInput("MCP:t_tool", cursorResult, ""))
			if _, items := cursorUpdated(t, answer); !strings.Contains(string(items[len(items)-1]), "LATE") {
				t.Errorf("the next MCP call is not handed the clone: %q", answer)
			}
		})
	}
}

// THE ANSWER STAYS UNDER THE SIZE CURSOR MOVES TO A FILE: whatever the clone
// holds (an instruction file past the room, many skills) and however long the
// call's own result, the result Cursor hands the model with the item appended
// (its text items joined by a line, the item's text last) stays within
// [cursorResultInline], itself under the 39,043 bytes Cursor put in a request
// whole (measured), so the call's own text is never moved to a file for it.
//
// Mutation: raise the bound past 39,043, or count the room from nothing,
// and this goes red.
func TestACursorAnswerStaysUnderTheSizeCursorMovesToAFile(t *testing.T) {
	t.Parallel()
	if cursorResultInline >= 39_043 {
		t.Fatalf("the bound %d is not under the 39,043 bytes measured to reach the model whole", cursorResultInline)
	}
	files := map[string]string{"late/.git/": "", "late/AGENTS.md": strings.Repeat("A", 50_000) + "\n",
		"late/CLAUDE.md": strings.Repeat("C", 9_000) + "\n"}
	for i := range 400 {
		files[fmt.Sprintf("late/.agents/skills/s%03d/SKILL.md", i)] = fmt.Sprintf(
			"---\nname: s%03d\ndescription: %s\n---\n", i, strings.Repeat("d", 200))
	}
	for _, size := range []int{0, 5_000, 20_000, cursorResultInline - cursorRoomFloor - 64} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			t.Parallel()
			r := newRepoRun(t, nil)
			r.cursorLaunch(t)
			r.add(t, files)
			result := `{"content":[{"type":"text","text":"` + strings.Repeat("o", size) + `"}]}`
			answer, _ := r.cursorFire(t, cursorInput("MCP:t_tool", result, ""))
			_, items := cursorUpdated(t, answer)
			var texts []string
			for _, item := range items {
				var part struct{ Text string }
				if err := json.Unmarshal(item, &part); err != nil {
					t.Fatal(err)
				}
				texts = append(texts, part.Text)
			}
			if joined := len(strings.Join(texts, "\n")); joined > cursorResultInline {
				t.Errorf("the result Cursor would hand the model is %d bytes, past %d", joined, cursorResultInline)
			}
			if len(result)+1+len(texts[len(texts)-1]) > cursorResultInline {
				t.Errorf("the result as handed (%d) and the item (%d) pass %d", len(result),
					len(texts[len(texts)-1]), cursorResultInline)
			}
		})
	}
}

// A CLONE NO MCP CALL WITH ROOM FOLLOWED IS NAMED ONCE AS THE RUN ENDS: once
// the run's state is ended, cursor-left adds one skip line for each finished
// clone under the working directory that no launch or firing claimed, and
// none for a launch checkout, a handed clone, or one cloned half way; before
// the end it says nothing, and a second end says nothing again.
//
// Mutation: name a claimed checkout, a half clone, or before the end, or say
// it twice, and this goes red.
func TestACursorCloneNeverHandedIsNamedOnceAsTheRunEnds(t *testing.T) {
	t.Parallel()
	r := newRepoRun(t, map[string]string{"first/.git/": "", "first/AGENTS.md": "FIRST\n"})
	r.cursorLaunch(t)
	r.add(t, map[string]string{"handed/.git/": "", "handed/AGENTS.md": "HANDED\n"})
	if answer, _ := r.cursorFire(t, cursorInput("MCP:t_tool", cursorResult, "")); !strings.Contains(answer, "HANDED") {
		t.Fatalf("the clone before the MCP call was not handed: %q", answer)
	}
	r.add(t, map[string]string{"quiet/.git/": "", "quiet/AGENTS.md": "QUIET\n",
		"half/.git/HEAD": "ref: refs/heads/main\n", "half/AGENTS.md": "HALF\n"})
	left := func() string {
		t.Helper()
		var stderr bytes.Buffer
		if code := runCursorLeft(context.Background(), flags{config: filepath.Join(r.state, "nested.json")},
			strings.NewReader(""), io.Discard, &stderr, os.Getenv); code != exitDone || stderr.Len() > 0 {
			t.Fatalf("cursor-left exited %d: %s", code, stderr.String())
		}
		raw, _ := os.ReadFile(filepath.Join(r.state, skippedLog))
		return string(raw)
	}
	if got := left(); got != "" {
		t.Errorf("before the run ended, cursor-left said %q", got)
	}
	if err := os.WriteFile(filepath.Join(r.state, endedMarker), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	want := "notehelper: the checkout " + filepath.Join(r.workdir(t), "quiet") + " was not handed: its clone " +
		"finished after the run's launch, and no MCP call with room for it came after that\n"
	if got := left(); got != want {
		t.Errorf("once the run ended, the skip log is\n%q\nwant\n%q", got, want)
	}
	if got := left(); got != want {
		t.Errorf("a second end changed the skip log to %q", got)
	}
}

// A FIRING CLAIMS NOTHING BEFORE THE RUN NAMES ITS CONVERSATION: with no
// [cursorConversation] in the state (a script that has not yet settled its
// session), no call can be told from a subagent's, so it answers {} and the
// clone waits.
//
// Mutation: hand where the file is missing, and this goes red.
func TestACursorFiringClaimsNothingBeforeTheRunNamesItsConversation(t *testing.T) {
	t.Parallel()
	r := newRepoRun(t, nil)
	r.cursorLaunch(t)
	if err := os.Remove(filepath.Join(r.state, cursorConversation)); err != nil {
		t.Fatal(err)
	}
	r.add(t, map[string]string{"late/.git/": "", "late/AGENTS.md": "LATE\n"})
	if answer, _ := r.cursorFire(t, cursorInput("MCP:t_tool", cursorResult, "")); answer != "{}" {
		t.Errorf("a firing with no run conversation answered %q, want {}", answer)
	}
}

// A PASS OF CURSOR-LEFT THAT FAILS IS TRIED AGAIN: its once-only mark is made
// only after its lines are written, so an end whose skip log cannot take them
// (here a directory in its place) leaves the mark unmade, and the end's next
// path writes the line.
//
// Mutation: make the mark before the lines, and this goes red.
func TestACursorLeftPassThatFailsIsTriedAgain(t *testing.T) {
	t.Parallel()
	r := newRepoRun(t, nil)
	r.cursorLaunch(t)
	r.add(t, map[string]string{"quiet/.git/": "", "quiet/AGENTS.md": "QUIET\n"})
	log := filepath.Join(r.state, skippedLog)
	if err := os.WriteFile(filepath.Join(r.state, endedMarker), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(log, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := cursorLeft(r.state, r.workdir(t)); err == nil {
		t.Fatal("a pass whose skip log is a directory reported no trouble")
	}
	if err := os.Remove(log); err != nil {
		t.Fatal(err)
	}
	if err := cursorLeft(r.state, r.workdir(t)); err != nil {
		t.Fatalf("the second pass: %v", err)
	}
	if raw, _ := os.ReadFile(log); !strings.Contains(string(raw), filepath.Join(r.workdir(t), "quiet")+" was not handed") {
		t.Errorf("after a failed pass, the next one did not name ./quiet: %q", raw)
	}
}
