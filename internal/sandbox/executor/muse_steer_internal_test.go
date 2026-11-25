package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// museToken is the fixture's take n's token.
func museToken(n string) string { return claudeTakeToken("00000000-0000-4000-8000-00000000000" + n) }

// The digests Muse Code gave the notes plugin's package in each fixture's
// box, which its hooks' runs record ([museInstalledDigest]).
const (
	museHooksDigest      = "sha256:b0a00512373d797fa5b05952efbd9042c9ad3db342cb8b263b6cbcde239a9d9c"
	museLaunchedDigest   = "sha256:c84f032f91b068e193ec2f11ed87d845d3972d0a83cd0dcd83f4ed85336aea13"
	museRepositoryDigest = "sha256:50bb3cd3349b897a8b1b1c5f894dee7222fa24b66cad50bdbc767e01e1cdd12d"
	museFailedDigest     = "sha256:6b4afe21018a7cffb2a2fb25976e2bb2087f78619e713759b2a10e09ba1d1931"
)

// museInstallAnswer is Muse Code's answer to the plugin's install, as the
// script keeps it ([Paths.MuseNotesInstall]), for the package digest names.
func museInstallAnswer(digest string) string {
	return `{"installed":{"id":"crewlet-notes","package_sha256":"` + digest + `"}}`
}

// museHeldOf is the tokens record shows held of the plugin package digest
// names, every token it names asked, the case failing on a read error.
func museHeldOf(t *testing.T, record []byte, digest string) map[string]bool {
	t.Helper()
	held, err := museHeld(t.Context(), bytes.NewReader(record), digest, museTokensIn(record), nil)
	if err != nil {
		t.Fatal(err)
	}
	return held
}

// museTokensIn is every take token record names.
func museTokensIn(record []byte) map[string]bool {
	tokens := map[string]bool{}
	for _, token := range regexp.MustCompile(`\[crewlet take [^\]\\"\n]{36}\]`).FindAll(record, -1) {
		tokens[string(token)] = true
	}
	return tokens
}

// museLines is the fixture's lines, each with its break.
func museLines(t *testing.T, name string) [][]byte {
	t.Helper()
	return bytes.SplitAfter(museFixture(t, name), []byte("\n"))
}

// museRecordLine is one record line of payload event, as the record writes one.
func museRecordLine(t *testing.T, payloadType string, event map[string]any) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"payload_type": payloadType, "payload": map[string]any{"event": event}})
	if err != nil {
		t.Fatal(err)
	}
	return append(raw, '\n')
}

// insertAfter is lines with extra placed after the line whose text holds
// marker, first found.
func insertAfter(t *testing.T, lines [][]byte, marker string, extra []byte) []byte {
	t.Helper()
	var out []byte
	done := false
	for _, l := range lines {
		out = append(out, l...)
		if !done && bytes.Contains(l, []byte(marker)) {
			out, done = append(out, extra...), true
		}
	}
	if !done {
		t.Fatalf("no line holds %q", marker)
	}
	return out
}

// TWO HOOKS FIRING IN ONE STEP ARE EACH CONFIRMED ON THEIR OWN, and the finish's
// too, off a real 1.4.25 record (three PostToolUse hooks of the run's plugin
// over a two-call batch, the second hook timed out, and a Stop block): each
// take whose token its hook's stderr shows, on a run Muse took the answer of,
// followed by the next model request, is held; the timed-out hook's take is not.
//
// Mutation: read a hook's stderr whatever its status, or hold a take with no
// model request after it.
func TestTwoMuseHooksFiringInOneStepAreEachConfirmed(t *testing.T) {
	held := museHeldOf(t, museFixture(t, "hooks.session.jsonl"), museHooksDigest)
	for _, n := range []string{"1", "3", "9"} {
		if !held[museToken(n)] {
			t.Errorf("take %s is not held: %v", n, held)
		}
	}
	if held[museToken("2")] || len(held) != 3 {
		t.Errorf("held %v, want takes 1, 3 and 9 alone", held)
	}
}

// A HOOK KILLED MID-TAKE LEAVES ITS TAKE UNCONFIRMED: a run Muse timed out or
// killed records no effect, whatever its stderr says, so the note is told
// again.
//
// Mutation: hold a token off any run of the plugin's hooks.
func TestAMuseHookKilledMidTakeLeavesItsTakeUnconfirmed(t *testing.T) {
	record := strings.ReplaceAll(string(museFixture(t, "hooks.session.jsonl")),
		`"status":"timed_out","stderr":""`, `"status":"timed_out","stderr":"`+museToken("2")+`\n"`)
	if !strings.Contains(record, museToken("2")) {
		t.Fatal("the fixture's timed-out run was not found")
	}
	if held := museHeldOf(t, []byte(record), museHooksDigest); held[museToken("2")] {
		t.Fatalf("a take on a timed-out hook's stderr is held: %v", held)
	}
}

// ONLY THE PLUGIN'S OWN HOOKS GIVE A RECEIPT: a repository's hook, recorded
// under its file's path, writing a token on its stderr confirms nothing.
//
// Mutation: hold a token off any hook's run.
func TestOnlyTheMusePluginsHooksGiveAReceipt(t *testing.T) {
	forged := museRecordLine(t, "runtime.session", map[string]any{"kind": "hook_run_terminal",
		"hook_key": "/work/repo/.muse/hooks.json:post_tool_use:def-1", "status": "completed",
		"effects": []string{"context"}, "stderr": museToken("7") + "\n"})
	record := insertAfter(t, museLines(t, "hooks.session.jsonl"), "posttooluse-3", forged)
	if held := museHeldOf(t, record, museHooksDigest); held[museToken("7")] {
		t.Fatalf("a repository hook's token is held: %v", held)
	}
}

// A COMPACTION BETWEEN A TAKE AND THE NEXT REQUEST LEAVES IT UNCONFIRMED,
// whether it comes before the run's block is committed or after: the record
// names what a compaction kept only by its own sequence, so the note is told
// again rather than counted off a summary.
//
// Mutation: drop the compaction's reset of the runs awaiting a block, or of
// the takes awaiting a request.
func TestACompactionBeforeTheNextMuseRequestLeavesTheTakeUnconfirmed(t *testing.T) {
	compaction := museRecordLine(t, "runtime.session", map[string]any{"kind": "context_compaction_installed"})
	for name, after := range map[string]string{
		"before the blocks": `"kind":"tool_result_batch_committed"`,
		"after the blocks":  `"id":"hook:post_tool_use:call_0032_1:1"`,
	} {
		record := insertAfter(t, museLines(t, "hooks.session.jsonl"), after, compaction)
		held := museHeldOf(t, record, museHooksDigest)
		if held[museToken("1")] || held[museToken("3")] || !held[museToken("9")] {
			t.Errorf("%s: held %v, want the batch's takes unconfirmed past the compaction, the finish's held",
				name, held)
		}
	}
}

// A RESTART BEFORE THE NEXT REQUEST LEAVES A FINISH'S TAKE UNCONFIRMED, its
// block sitting in the leading developer instructions, which a resumed
// session does not restore, whether the block was committed yet or not; and a
// step's take whose block the old session never committed, which the new one
// never will. A step's committed block stays in the session's history.
//
// Mutation: keep a finish's pending take, or a run awaiting its block, across
// a session's start.
func TestAMuseFinishTakeIsNotConfirmedAcrossARestart(t *testing.T) {
	restart := []byte(`{"payload_type":"session.started","payload":{}}` + "\n")
	for name, c := range map[string]struct {
		after   string
		unheld  []string
		stillOK []string
	}{
		// After the Stop hook's end, which alone records it `blocked`.
		"after the finish's run":   {`"status":"blocked"`, []string{"9"}, []string{"1", "3"}},
		"after the finish's block": {`"id":"hook:stop:stop:0"`, []string{"9"}, []string{"1", "3"}},
		"before the step's blocks": {`"kind":"tool_result_batch_committed"`, []string{"1", "3"}, []string{"9"}},
	} {
		held := museHeldOf(t, insertAfter(t, museLines(t, "hooks.session.jsonl"), c.after, restart), museHooksDigest)
		for _, n := range c.unheld {
			if held[museToken(n)] {
				t.Errorf("%s: take %s is held across the restart: %v", name, n, held)
			}
		}
		for _, n := range c.stillOK {
			if !held[museToken(n)] {
				t.Errorf("%s: take %s is not held: %v", name, n, held)
			}
		}
	}
}

// THE MAIN-THREAD READ ANSWERS EACH TAKE ASKED off the box's record: held as
// shown, absent once the job is over and it is not, undecided before.
//
// Mutation: answer a take not shown as absent while the job runs.
func TestAMuseMainThreadReadAnswersItsTakes(t *testing.T) {
	const session = "0b5e0000-0000-4000-8000-0000000000f7"
	box := sandbox.NewFakeSandbox("b")
	p := PathsFor(box)
	path := p.MuseSessions() + "/2026/11/22/" + session + "/session.jsonl"
	box.Put(path, string(museFixture(t, "hooks.session.jsonl")))
	box.Put(p.MuseNotesInstall(), museInstallAnswer(museHooksDigest))
	box.ExecFunc = func(_ context.Context, cmd string, _ sandbox.ExecOptions) (sandbox.ExecResult, error) {
		return sandbox.ExecResult{Stdout: path + "\n"}, nil
	}
	ask := sandbox.MainThreadAsk{Session: session, Takes: []sandbox.AskedTake{
		{Take: "00000000-0000-4000-8000-000000000001"}, {Take: "00000000-0000-4000-8000-000000000002"}}}
	got := (Muse{}).readMainThread(t.Context(), box, p, ask).Receipts
	if len(got.Held) != 1 || got.Held[0] != ask.Takes[0].Take || len(got.Absent) != 0 {
		t.Fatalf("receipts %+v, want the first held and the second undecided", got)
	}
	ask.Ended = true
	got = (Muse{}).readMainThread(t.Context(), box, p, ask).Receipts
	if len(got.Absent) != 1 || got.Absent[0] != ask.Takes[1].Take {
		t.Fatalf("receipts %+v, want the second absent once the job is over", got)
	}
}

// ONE ITEM AN ANSWER, WITHIN 16,384 BYTES OF RAW UTF-8: the engine's widest
// item (4,505 UTF-16 units) with the answer's framing fits Muse Code's
// ceiling once the note helper takes the token out, whether it is all `<`
// (which encoding/json escapes unless told not to), all U+2028 (which it
// escapes always), all 3-byte characters, or all tabs or newlines (two bytes
// each, escaped), and a second item goes to the next hook's answer.
//
// Mutation: carry two items in one answer, or write it with encoding/json.
func TestAMuseHookAnswerCarriesOneItemWithinItsCeiling(t *testing.T) {
	if museItemUnits < 4505 {
		t.Fatalf("one answer carries %d units, under the engine's widest item", museItemUnits)
	}
	if got := (Muse{}).notesThatFit([]string{"a", "b", "c"}); got != 1 {
		t.Errorf("one answer carries %d messages, want one", got)
	}
	for _, unit := range []string{"<", "\u2028", "中", "\t", "\n"} {
		widest := strings.Repeat(unit, 4505)
		for _, event := range MuseHookEvents {
			handed := MuseHanded(t, event, widest)
			if len(handed) > museHookAnswerBytes || strings.Contains(handed, `\u`) {
				t.Errorf("%q %s: the answer Muse is handed is %d bytes (escaped %v), past %d", unit, event,
					len(handed), strings.Contains(handed, `\u`), museHookAnswerBytes)
			}
		}
	}
}

// A HOOK ANSWER'S TEXT READS BACK AS IT WAS WRITTEN, whatever it holds: the
// quote, the backslash, every control character, U+2028 and characters past
// the basic plane, as JSON's own reader decodes them.
//
// Mutation: leave a control character unescaped, or the backslash.
func TestAMuseHookAnswersTextReadsBackWhole(t *testing.T) {
	var text strings.Builder
	for r := rune(0); r < 0x80; r++ {
		text.WriteRune(r)
	}
	text.WriteString("\u2028\u2029 ünï 🚀 \\\" end")
	answer := (Muse{}).hookAnswer(claudeEventFinish, "11111111-1111-4111-8111-111111111111",
		[]HookNote{{Message: text.String()}})
	var got struct {
		Take     string `json:"crewlet_take"`
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(answer), &got); err != nil {
		t.Fatalf("%v: %q", err, answer)
	}
	if got.Reason != text.String() || got.Decision != "block" ||
		got.Take != claudeTakeToken("11111111-1111-4111-8111-111111111111") {
		t.Fatalf("read back %+v, want the text, the block and the token as written", got)
	}
}

// A LAUNCHED RUN'S NOTES ARE EACH HELD (the engine's own script, plugin, take
// config and note helper, in a box against a stand-in bridge serving two
// notes between steps and one at the finish): every take its record shows
// delivered and sent is held, and the answer is the one given after the
// finish's note.
//
// Mutation: hold no finish's take, or read another hook key.
func TestALaunchedMuseRunsNotesAreEachHeld(t *testing.T) {
	held := museHeldOf(t, museFixture(t, "launched-notes.session.jsonl"), museLaunchedDigest)
	for _, n := range []string{"1", "2", "9"} {
		if token := claudeTakeToken("aaaaaaaa-0000-4000-8000-00000000000" + n); !held[token] {
			t.Errorf("take %s is not held: %v", n, held)
		}
	}
	if res := (Muse{}).Parse(museFixture(t, "launched-notes.result.jsonl")); !res.Success ||
		!strings.HasSuffix(res.Text, "ANSWER after the stop note.") {
		t.Errorf("result %+v, want the answer after the finish's note", res)
	}
}

// THE MODEL'S SHELL HOLDS NO CREWLET_ VARIABLE: the run's own shell, in the
// launched run, saw no CREWLET_ name; the bridge's token is in the box home's
// settings file, never a variable. (Its sign-in and an operator's exporter,
// present: TestEveryRunnersShellHoldsWhatItsHarnessIsHanded.)
//
// Mutation: re-capture with a CREWLET_ variable handed to Muse.
func TestTheMuseShellHoldsNoCrewletVariable(t *testing.T) {
	found := false
	for l := range bytes.Lines(museFixture(t, "launched-notes.result.jsonl")) {
		env, ok := decodeMuseEnvelope(l)
		if !ok || env.PayloadType != "tool.result" {
			continue
		}
		var p struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(env.Payload, &p)
		if !strings.Contains(p.Text, "MUSE_SESSION_ID=") {
			continue
		}
		found = true
		if strings.Contains(p.Text, "CREWLET_") {
			t.Errorf("the run's shell ran with a CREWLET_ variable:\n%s", p.Text)
		}
	}
	if !found {
		t.Fatal("the fixture holds no shell's environment")
	}
}

// NO REPOSITORY FILE DISPLACES OR IMPERSONATES THE RUN'S CHANNELS (a launched
// run whose checkout declares a `crewlet` server and a hook writing a
// take-shaped line on its stderr, in a home holding a decoy plugin of the
// notes plugin's name): the model reached the bridge under the run's own
// name, the repository's take token confirms nothing, and the run's own take
// is held.
//
// Mutation: name the bridge `crewlet`, or hold a token off any hook's run.
func TestARepositoryCannotDisplaceOrImpersonateTheMuseChannels(t *testing.T) {
	held := museHeldOf(t, museFixture(t, "launched-repository.session.jsonl"), museRepositoryDigest)
	if !held[claudeTakeToken("aaaaaaaa-0000-4000-8000-000000000011")] ||
		held[claudeTakeToken("ffffffff-0000-4000-8000-000000000001")] ||
		held[claudeTakeToken("ffffffff-0000-4000-8000-000000000002")] {
		t.Fatalf("held %v: want the run's own take alone", held)
	}
	reached := false
	for l := range bytes.Lines(museFixture(t, "launched-repository.result.jsonl")) {
		var p musePayload
		env, ok := decodeMuseEnvelope(l)
		if !ok || env.PayloadType != "tool.result" || json.Unmarshal(env.Payload, &p) != nil {
			continue
		}
		name, _ := strings.CutPrefix(p.Facts.ToolName, "mcp__crewlet_")
		suffix, tool, _ := strings.Cut(name, "__")
		if suffix != "" && strings.Trim(suffix, "0123456789abcdef") == "" && tool == "echo" &&
			p.Facts.Outcome == "success" {
			reached = true
		}
	}
	if !reached {
		t.Fatal("the model reached no bridge tool under the run's own server name")
	}
}

// THE SCRIPT REMOVES A PLUGIN OF THE NOTES PLUGIN'S NAME BEFORE IT INSTALLS AND
// APPROVES ITS OWN, and adds the servers after the approval, which rewrites
// the settings: an earlier plugin of that name never runs as the run's, and
// the servers keep `required` and their wait. (The last run's servers go at
// the clear before: TestAKilledRunsEntriesLeaveTheHomeAtAnotherHarnesssStart.)
//
// Mutation: install before removing, or add the servers first.
func TestTheMuseScriptWiresItsPluginBeforeItsServers(t *testing.T) {
	script := museWiring(Paths{Home: "/h"})
	order := []string{"muse plugins remove " + museNotesPlugin, "muse plugins install ",
		"muse plugins approve " + museNotesPlugin, "muse mcp add-json "}
	last := -1
	for _, step := range order {
		at := strings.Index(script, step)
		if at <= last {
			t.Fatalf("%q is not after the step before it in:\n%s", step, script)
		}
		last = at
	}
}

// AN UNREADABLE LINE WITH A LATER LINE AFTER IT CONFIRMS NOTHING: a line that
// is no JSON, or a runtime record whose event is no object, may be the
// compaction or the restart that leaves a take unconfirmed, so the read is
// undecided, saying which line, and holds no take, nor answers one absent once
// the job is over. The same line last is one Muse is still writing: the takes
// before it hold.
//
// Mutation: skip an unreadable line, or hold the takes read before it.
func TestAnUnreadableMuseRecordConfirmsNothing(t *testing.T) {
	const session = "0b5e0000-0000-4000-8000-0000000000f7"
	torn := []byte(`{"payload_type":"runtime.session","payload":{"ev` + "\n")
	for name, bad := range map[string][]byte{
		"no JSON":         torn,
		"event no object": []byte(`{"payload_type":"runtime.session","payload":{"event":"compacted"}}` + "\n"),
	} {
		record := insertAfter(t, museLines(t, "hooks.session.jsonl"), "plugin:crewlet-notes:posttooluse-1", bad)
		if held, err := museHeld(t.Context(), bytes.NewReader(record), museHooksDigest,
			museTokensIn(record), nil); err == nil || len(held) != 0 {
			t.Errorf("%s: held %v, err %v; want none held and the line named", name, held, err)
		}
		box := sandbox.NewFakeSandbox("b")
		p := PathsFor(box)
		path := p.MuseSessions() + "/2026/11/22/" + session + "/session.jsonl"
		box.Put(path, string(record))
		box.Put(p.MuseNotesInstall(), museInstallAnswer(museHooksDigest))
		box.ExecFunc = func(_ context.Context, _ string, _ sandbox.ExecOptions) (sandbox.ExecResult, error) {
			return sandbox.ExecResult{Stdout: path + "\n"}, nil
		}
		got := (Muse{}).readMainThread(t.Context(), box, p, sandbox.MainThreadAsk{Session: session, Ended: true,
			Takes: []sandbox.AskedTake{{Take: "00000000-0000-4000-8000-000000000001"}}}).Receipts
		if len(got.Held) != 0 || len(got.Absent) != 0 || !strings.Contains(got.Why, "line") {
			t.Errorf("%s: receipts %+v, want undecided with the line named", name, got)
		}
	}
	tail := append(museFixture(t, "hooks.session.jsonl"), torn[:len(torn)-1]...)
	if held := museHeldOf(t, tail, museHooksDigest); !held[museToken("1")] || !held[museToken("9")] {
		t.Errorf("held %v with a torn last line: want the takes before it", held)
	}
}

// A TAKE IS HELD ONLY ONCE ITS OWN CONTEXT BLOCK IS COMMITTED: a hook run whose
// answer Muse took, a request after it, but no block of that call and firing
// (`hook:post_tool_use:<call>:<n>`) in the record, confirms nothing; the takes
// whose blocks it shows still hold.
//
// Mutation: hold a take off its hook's run and the next request alone.
func TestAMuseTakeWithoutItsContextBlockIsNotHeld(t *testing.T) {
	var record []byte
	dropped := 0
	for _, l := range museLines(t, "hooks.session.jsonl") {
		if bytes.Contains(l, []byte(`"id":"hook:post_tool_use:call_0032_0:0"`)) ||
			bytes.Contains(l, []byte(`"id":"hook:post_tool_use:call_0032_1:0"`)) {
			dropped++
			continue
		}
		record = append(record, l...)
	}
	if dropped != 2 {
		t.Fatalf("dropped %d blocks, want slot 1's two", dropped)
	}
	held := museHeldOf(t, record, museHooksDigest)
	if held[museToken("1")] || !held[museToken("3")] || !held[museToken("9")] {
		t.Fatalf("held %v: want slot 1's take unconfirmed, slot 3's and the finish's held", held)
	}
}

// A HOOK RUN OF ANOTHER PACKAGE CONFIRMS NOTHING: only a run whose
// `source_digest` is the package the script installed counts, so a plugin of
// the notes plugin's id and other content gives no receipt, whatever Muse
// approved.
//
// Mutation: drop the digest check, or compare it to the plugin's id alone.
func TestAMuseHookOfAnotherPackageConfirmsNothing(t *testing.T) {
	if held := museHeldOf(t, museFixture(t, "hooks.session.jsonl"), "sha256:"+strings.Repeat("0", 64)); len(held) != 0 {
		t.Errorf("held %v under another package's digest, want none", held)
	}
	record := strings.ReplaceAll(string(museFixture(t, "hooks.session.jsonl")),
		`"capability_id":"posttooluse-3","plugin_id":"crewlet-notes","source_digest":"`+museHooksDigest,
		`"capability_id":"posttooluse-3","plugin_id":"crewlet-notes","source_digest":"sha256:`+strings.Repeat("f", 64))
	if !strings.Contains(record, strings.Repeat("f", 64)) {
		t.Fatal("the fixture's slot 3 runs were not found")
	}
	if held := museHeldOf(t, []byte(record), museHooksDigest); held[museToken("3")] || !held[museToken("1")] {
		t.Errorf("held %v: want slot 3's take, from another package, unconfirmed", held)
	}
}

// TWO IDENTICAL NOTES IN ONE CALL ARE EACH CONFIRMED ON THEIR OWN: two hooks
// answering one call with the same text get a block each (`:0`, `:1`,
// measured on 1.4.25), each answering its own run's take.
//
// Mutation: key a take's block by its text, and the second take is never held.
func TestTwoIdenticalMuseNotesInOneCallAreEachConfirmed(t *testing.T) {
	record := museFixture(t, "failed-request.session.jsonl")
	if bytes.Count(record, []byte(`"text":"SAME-NOTE"`)) != 2 {
		t.Fatal("the fixture holds no two blocks of one text")
	}
	held := museHeldOf(t, record, museFailedDigest)
	if !held[museToken("1")] || !held[museToken("2")] {
		t.Fatalf("held %v: want both takes", held)
	}
}

// A FAILED REQUEST AFTER THE BLOCK CONFIRMS NOTHING: the run whose next
// request failed (`model_failure_terminal_intent`, no `model_completed`)
// holds no take; the session continued and completed a request carrying both
// blocks (measured on 1.4.25), which holds them.
//
// Mutation: hold a take at any request, failed or not.
func TestAFailedMuseRequestAfterTheBlockConfirmsNothing(t *testing.T) {
	lines := museLines(t, "failed-request.session.jsonl")
	if !bytes.Contains(lines[77], []byte(`"payload_type":"session.started"`)) {
		t.Fatal("the fixture's continued run does not start at its line 78")
	}
	if held := museHeldOf(t, bytes.Join(lines[:77], nil), museFailedDigest); len(held) != 0 {
		t.Errorf("held %v off a failed request, want none", held)
	}
	if held := museHeldOf(t, bytes.Join(lines, nil), museFailedDigest); len(held) != 2 {
		t.Errorf("held %v once the continued run's request completed, want both takes", held)
	}
}

// THE PACKAGE'S DIGEST IS READ OFF THE INSTALL'S ANSWER HOWEVER IT IS LAID OUT:
// a real 1.4.25 answer to `muse plugins install --json`, as Muse prints it
// (indented) and compacted to one line, reads its `installed.package_sha256`;
// a failed install's answer, or none, reads none.
//
// Mutation: read the digest by a pattern of the indented layout alone.
func TestMuseReadsItsPackageDigestWhateverItsLayout(t *testing.T) {
	const want = "sha256:bf6b9b8c173702da61ca05d5b607aec0e1946cc50e6ae1294c31fec64ca0574c"
	pretty := museFixture(t, "plugin-install.json")
	var compact bytes.Buffer
	if err := json.Compact(&compact, pretty); err != nil {
		t.Fatal(err)
	}
	if bytes.Count(compact.Bytes(), []byte("\n")) > 0 || !bytes.Contains(pretty, []byte("\n  ")) {
		t.Fatal("the two layouts are not one indented and one on a single line")
	}
	for name, answer := range map[string][]byte{"indented": pretty, "compact": compact.Bytes()} {
		if got := museInstalledDigest(answer); got != want {
			t.Errorf("%s: read %q, want %q", name, got, want)
		}
	}
	for name, answer := range map[string]string{
		"a failed install": `{"error":{"code":"invalid-plugin-package","message":"x"}}`,
		"none":             "",
	} {
		if got := museInstalledDigest([]byte(answer)); got != "" {
			t.Errorf("%s: read %q, want none", name, got)
		}
	}
}

// A RUN WHOSE INSTALL LEFT NO DIGEST CONFIRMS NOTHING AND DECIDES NOTHING: an
// empty answer, its install never having printed one, reads undecided, saying
// why, even once the job is over, rather than every take absent.
//
// Mutation: read a take held, or absent, with no digest to tell the plugin's
// runs by.
func TestAMuseRunWithNoPackageDigestIsUndecided(t *testing.T) {
	const session = "0b5e0000-0000-4000-8000-0000000000f7"
	box := sandbox.NewFakeSandbox("b")
	p := PathsFor(box)
	path := p.MuseSessions() + "/2026/11/22/" + session + "/session.jsonl"
	box.Put(path, string(museFixture(t, "hooks.session.jsonl")))
	box.Put(p.MuseNotesInstall(), "")
	box.ExecFunc = func(_ context.Context, _ string, _ sandbox.ExecOptions) (sandbox.ExecResult, error) {
		return sandbox.ExecResult{Stdout: path + "\n"}, nil
	}
	got := (Muse{}).readMainThread(t.Context(), box, p, sandbox.MainThreadAsk{Session: session, Ended: true,
		Takes: []sandbox.AskedTake{{Take: "00000000-0000-4000-8000-000000000001"}}}).Receipts
	if len(got.Held) != 0 || len(got.Absent) != 0 || !strings.Contains(got.Why, "digest") {
		t.Fatalf("receipts %+v, want undecided, naming the missing digest", got)
	}
}
