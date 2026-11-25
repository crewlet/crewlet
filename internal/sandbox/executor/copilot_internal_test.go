package executor

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/prereqtest"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// copilotFixture is testdata/copilot/1.0.114/<name>: a real Copilot 1.0.114
// run's stream or events.jsonl, against a stand-in model (its BYOK variables)
// and a stand-in bridge, with the vendor's own prompt text replaced
// (`[the vendor's text, not kept in this fixture]`) and every other byte as the
// harness wrote it.
func copilotFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "copilot", "1.0.114", name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// copilotScriptUsage is what a run's script adds to its stream once its harness
// exits ([copilotUsage]), run under sh over events, a session's events.jsonl,
// that held before lines when the run started.
func copilotScriptUsage(t *testing.T, events []byte, before int) []byte {
	t.Helper()
	file := filepath.Join(t.TempDir(), "events.jsonl")
	if err := os.WriteFile(file, events, 0o600); err != nil {
		t.Fatal(err)
	}
	sh := prereqtest.OnPath(t, "sh", "the case runs the box's script", "install a POSIX shell")
	script := "ev=" + shellword.Quote(file) + " el=" + strconv.Itoa(before) + "\n" + copilotUsage()
	out, err := exec.Command(sh, "-c", script).Output()
	if err != nil {
		t.Fatalf("the usage part of the script failed: %v", err)
	}
	return out
}

// copilotScriptUsageOver is [copilotScriptUsage] over a session directory that
// held launch, files by name, when the run started ([copilotJournalAtLaunch]),
// and holds after once its harness exited.
func copilotScriptUsageOver(t *testing.T, launch, after map[string][]byte) []byte {
	t.Helper()
	sh := prereqtest.OnPath(t, "sh", "the case runs the box's script", "install a POSIX shell")
	run := func(files map[string][]byte, script string) []byte {
		dir := t.TempDir()
		for name, body := range files {
			if err := os.WriteFile(filepath.Join(dir, name), body, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		out, err := exec.Command(sh, "-c", "ev="+shellword.Quote(filepath.Join(dir, "events.jsonl"))+"\n"+
			script).Output()
		if err != nil {
			t.Fatalf("the script failed: %v", err)
		}
		return out
	}
	kept := strings.Fields(string(run(launch, copilotJournalAtLaunch()+`echo "$el" "$en"`)))
	if len(kept) != 2 {
		t.Fatalf("the launch kept %q, want a line count and a file number", kept)
	}
	return run(after, "el="+kept[0]+" en="+kept[1]+"\n"+copilotUsage())
}

// copilotCollected is what a run whose harness wrote stream and whose session
// is events leaves in its result file: the stream, then the script's usage.
func copilotCollected(t *testing.T, stream, events string, before int) []byte {
	t.Helper()
	return append(copilotFixture(t, stream), copilotScriptUsage(t, copilotFixture(t, events), before)...)
}

// The run's answer is its last root message, and its spend the usage records
// its own process wrote, read after the script's mark.
//
// Mutation: read the answer off the `result` line, or sum the records without
// the mark — the case fails.
func TestACopilotRunsAnswerAndSpendAreItsOwn(t *testing.T) {
	dec := &copilotEvents{}
	if err := eachLine(bytes.NewReader(copilotCollected(t, "tool.stream.jsonl", "tool.events.jsonl", 0)), dec,
		maxLineBytes); err != nil {
		t.Fatal(err)
	}
	res := dec.Result()
	if !res.Success || res.Text != "ANSWER after tool" {
		t.Fatalf("result = %+v, want the success the run's last root message said", res)
	}
	if res.SessionID != "11111111-1111-4111-8111-111111111111" {
		t.Errorf("session = %q, want the stream's own", res.SessionID)
	}
	// Two requests: 1001+1002 prompt tokens with 500 cached read and 7 written
	// each, 11+12 output — what the stand-in model answered and Copilot recorded.
	spent, stated := dec.runSpend()
	if !stated {
		t.Fatal("the run's spend was not stated, and the script marked its usage")
	}
	charged := spent.on(sandbox.Result{})
	if charged.InputTokens != 2003 || charged.CacheReadTokens != 1000 || charged.CacheWriteTokens != 14 ||
		charged.OutputTokens != 23 || charged.CostUSD != 0 {
		t.Errorf("charged %+v, want input 2003 (the cache inside it), cache 1000/14, output 23, no price", charged)
	}
	if res.Turns != 2 || dec.reportedWindow() != 128000 {
		t.Errorf("turns %d, window %d; want the two requests and their 128000-token prompt budget",
			res.Turns, dec.reportedWindow())
	}
	if !strings.Contains(res.Transcript, "crewlet-echo") {
		t.Errorf("the transcript names no call:\n%s", res.Transcript)
	}
}

// A run this runner launched end to end (the engine's own rendered script,
// settings, instructions and extension, in a box against the stand-ins, a note
// sent while its tool ran) reads as its answer and its own spend off the result
// file its script left, and the note as held off its session's record.
//
// Mutation: read the script's usage mark as no mark, or confirm a note off
// another clientTurnId — the case fails.
func TestALaunchedCopilotRunReadsAsItsAnswerSpendAndNote(t *testing.T) {
	res := (Copilot{}).Parse(copilotFixture(t, "launched.result.jsonl"))
	if !res.Success || res.Text != "ANSWER saw note" {
		t.Fatalf("result = %+v, want the answer to the note", res)
	}
	if res.InputTokens != 2005 || res.CacheReadTokens != 1000 || res.CacheWriteTokens != 14 || res.OutputTokens != 25 {
		t.Errorf("charged in %d (cache %d/%d), out %d; want 2005 (1000/14), 25", res.InputTokens,
			res.CacheReadTokens, res.CacheWriteTokens, res.OutputTokens)
	}
	if !strings.Contains(res.Transcript, "[steer]") {
		t.Errorf("the transcript does not show the note:\n%s", res.Transcript)
	}
	held, err := copilotNotesHeld(t.Context(), bytes.NewReader(copilotFixture(t, "launched.events.jsonl")),
		served("msg_inbox1"), nil)
	if err != nil || !slices.Equal(held, []string{"msg_inbox1"}) {
		t.Fatalf("held = %v (%v), want the note", held, err)
	}
}

// A usage record before the script's mark is not charged: one Copilot
// printed itself would be counted twice beside the script's. A stream with no
// mark states no spend.
//
// Mutation: count a record whatever came before it — the case fails.
func TestACopilotUsageRecordBeforeTheMarkIsNotCharged(t *testing.T) {
	events := copilotFixture(t, "tool.events.jsonl")
	unmarked := append(copilotFixture(t, "tool.stream.jsonl"), events...)
	dec := &copilotEvents{}
	_ = eachLine(bytes.NewReader(unmarked), dec, maxLineBytes)
	if spent, stated := dec.runSpend(); stated || spent != (runSpend{}) {
		t.Fatalf("spend %+v stated %v, want none stated with no mark", spent, stated)
	}
	dec = &copilotEvents{}
	_ = eachLine(bytes.NewReader(append(unmarked, copilotScriptUsage(t, events, 0)...)), dec, maxLineBytes)
	spent, _ := dec.runSpend()
	if got := spent.on(sandbox.Result{}); got.InputTokens != 2003 || got.OutputTokens != 23 {
		t.Fatalf("charged %+v, want the two records after the mark alone (2003 in, 23 out)", got)
	}
}

// A continued session's usage is the run's own: the records this process
// wrote, never the session's earlier processes'.
//
// Mutation: take every record of the file, and the case fails.
func TestACopilotContinuedRunIsChargedItsOwnRequestsAlone(t *testing.T) {
	events := copilotFixture(t, "resumed.events.jsonl")
	// The first process wrote 16 lines (its two requests); the resume one.
	out := copilotScriptUsage(t, events, 16)
	dec := &copilotEvents{}
	_ = eachLine(bytes.NewReader(out), dec, maxLineBytes)
	spent, _ := dec.runSpend()
	if got := spent.on(sandbox.Result{}); got.InputTokens != 1003 || got.OutputTokens != 13 || dec.requests != 1 {
		t.Fatalf("charged %+v over %d requests, want the resume's one request (1003 in, 13 out)", got, dec.requests)
	}
	// AND A PROCESS THAT WROTE NO START OF ITS OWN is charged nothing, never
	// the run before it.
	none := copilotScriptUsage(t, events, 23)
	dec = &copilotEvents{}
	_ = eachLine(bytes.NewReader(none), dec, maxLineBytes)
	if spent, stated := dec.runSpend(); !stated || spent != (runSpend{}) {
		t.Fatalf("a run that wrote no start was charged %+v (stated %v), want nothing, stated", spent, stated)
	}
}

// A run whose journal Copilot rotated (COPILOT_SESSION_EVENT_LOG_ROTATION_BYTES
// at 8192, 1.0.114) is charged every request it made: those the rotation moved
// to a numbered file, past what the file held at launch, and those after it in
// the new head, whose copies hold no usage record and may number fewer lines
// than the launch's head did. A file numbered no higher than the last at launch
// is an earlier run's and is not read again.
//
// Mutation: read events.jsonl alone, read the files numbered at launch too,
// keep no highest file at launch, skip the launch's line count in every file,
// or in the first file holding a line: a case fails.
func TestARotatedCopilotRunIsChargedEveryRequestItMade(t *testing.T) {
	fx := func(name string) []byte { return copilotFixture(t, name) }
	// The lines a file held when the run began: an archive opens with the
	// head it moved, byte for byte (measured).
	first := func(b []byte, n int) []byte {
		rest := b
		for range n {
			_, rest, _ = bytes.Cut(rest, []byte("\n"))
		}
		return b[:len(b)-len(rest)]
	}
	// A resume that compacted, which rotated the journal, then made 6
	// requests: the 88 lines the head held at launch open events.1.jsonl, and
	// the new head holds 2 of the 6 within its first 88 lines.
	compacted, compactedHead := fx("compacted-resume-rotated.events.1.jsonl"), fx("compacted-resume-rotated.events.jsonl")
	for _, tc := range []struct {
		name                string
		launch, after       map[string][]byte
		requests, in, cache int
		write, out          int
	}{
		// A new session rotated as its harness exited: all 9 requests are in
		// events.1.jsonl.
		{"rotated at its end", nil, map[string][]byte{"events.1.jsonl": fx("rotated-end.events.1.jsonl"),
			"events.jsonl": fx("rotated-end.events.jsonl")}, 9, 9045, 4500, 63, 135},
		// Rotated after a compaction, before its last request: 13 + 1.
		{"rotated mid-run", nil, map[string][]byte{"events.1.jsonl": fx("rotated-mid.events.1.jsonl"),
			"events.jsonl": fx("rotated-mid.events.jsonl")}, 14, 14106, 7000, 98, 246},
		// A resume of a rotated session, whose events.1.jsonl is the first run's.
		{"resumed after a rotation", map[string][]byte{"events.1.jsonl": fx("rotated-end.events.1.jsonl"),
			"events.jsonl": fx("rotated-end.events.jsonl")}, map[string][]byte{
			"events.1.jsonl": fx("rotated-end.events.1.jsonl"),
			"events.jsonl":   fx("rotated-end.resumed.events.jsonl")}, 9, 9126, 4500, 63, 216},
		// A resume rotated as it exited: the 58 lines its head held at launch,
		// the first run's, open events.1.jsonl, and its 9 requests follow.
		{"a resume rotated at its end", map[string][]byte{"events.jsonl": first(fx("resumed-rotated.events.1.jsonl"), 58)},
			map[string][]byte{"events.1.jsonl": fx("resumed-rotated.events.1.jsonl"),
				"events.jsonl": fx("resumed-rotated.events.jsonl")}, 9, 9126, 4500, 63, 216},
		{"a resume rotated after a compaction", map[string][]byte{"events.jsonl": first(compacted, 88)},
			map[string][]byte{"events.1.jsonl": compacted, "events.jsonl": compactedHead}, 6, 6111, 3000, 42, 171},
		// BUILT FROM THOSE LINES, as no 1.0.114 run rotated twice in one box
		// (the second was always deferred): the same resume in a box whose
		// events.1.jsonl an earlier rotation left, so its archive is
		// events.2.jsonl and the head links to it.
		{"a second rotation", map[string][]byte{"events.1.jsonl": fx("rotated-end.events.1.jsonl"),
			"events.jsonl": first(compacted, 88)}, map[string][]byte{
			"events.1.jsonl": fx("rotated-end.events.1.jsonl"), "events.2.jsonl": compacted,
			"events.jsonl": bytes.Replace(compactedHead, []byte(`"previousFile":"events.1.jsonl"`),
				[]byte(`"previousFile":"events.2.jsonl"`), 1)}, 6, 6111, 3000, 42, 171},
		// BUILT: the archive of the launch's head empty, the head read whole.
		{"an empty archive", map[string][]byte{"events.jsonl": first(compacted, 88)},
			map[string][]byte{"events.1.jsonl": nil, "events.jsonl": compactedHead}, 6, 6111, 3000, 42, 171},
	} {
		t.Run(tc.name, func(t *testing.T) {
			copilotCharged(t, copilotScriptUsageOver(t, tc.launch, tc.after),
				tc.requests, tc.in, tc.cache, tc.write, tc.out)
		})
	}
}

// copilotCharged fails t unless usage, a run's script's usage part, charges requests
// requests: in prompt tokens, cache read and written, and out.
func copilotCharged(t *testing.T, usage []byte, requests, in, cache, write, out int) {
	t.Helper()
	dec := &copilotEvents{}
	if err := eachLine(bytes.NewReader(usage), dec, maxLineBytes); err != nil {
		t.Fatal(err)
	}
	spent, stated := dec.runSpend()
	got := spent.on(sandbox.Result{})
	if !stated || dec.requests != requests || got.InputTokens != in || got.CacheReadTokens != cache ||
		got.CacheWriteTokens != write || got.OutputTokens != out {
		t.Fatalf("charged %+v over %d requests (stated %v), want %d requests: %d in, cache %d/%d, %d out",
			got, dec.requests, stated, requests, in, cache, write, out)
	}
}

// A rate limit is reported as the run's limit line and its terminal event, for
// the entry to sit its plan out.
//
// Mutation: keep no limit line for `rate_limit` — the case fails.
func TestACopilotRateLimitIsReportedAsALimit(t *testing.T) {
	dec := &copilotEvents{}
	_ = eachLine(bytes.NewReader(copilotFixture(t, "ratelimit.stream.jsonl")), dec, maxLineBytes)
	res := dec.Result()
	if res.Success || !strings.Contains(res.Error, "429") {
		t.Fatalf("result = %+v, want a failure saying the 429", res)
	}
	limit := dec.limitReport()
	if !strings.Contains(limit, `"type":"session.error"`) || !strings.Contains(limit, `"errorType":"rate_limit"`) ||
		res.Event != limit {
		t.Fatalf("limit %q, event %q: want the session.error line in both", limit, res.Event)
	}
}

// A run the extension stopped at its cap reads as capped: its line, the abort,
// the aborted idle and exit 1, all four. The same abort without the line (as
// Ctrl+C leaves one, `user_initiated` too) is an abort, not a cap.
//
// Mutation: read a cap off the abort without the line — the second half
// fails; drop the line's arm — the first does.
func TestACopilotRunStoppedAtItsCapReadsAsCapped(t *testing.T) {
	stream := copilotFixture(t, "capped.result.jsonl")
	if !bytes.Contains(stream, []byte(copilotCapLine(3))) || !bytes.Contains(stream, []byte(copilotArmedMessage)) {
		t.Fatal("the capped run's result holds no cap line or no extension line")
	}
	dec := &copilotEvents{}
	_ = eachLine(bytes.NewReader(stream), dec, maxLineBytes)
	res := dec.Result()
	if res.Success || !res.RoundCapped || !strings.Contains(res.Error, "turn cap of 3") {
		t.Fatalf("result = %+v, want a run capped at 3", res)
	}
	var unlabelled []byte
	for l := range bytes.Lines(stream) {
		if !bytes.Contains(l, []byte(copilotCapMessage)) {
			unlabelled = append(unlabelled, l...)
		}
	}
	dec = &copilotEvents{}
	_ = eachLine(bytes.NewReader(unlabelled), dec, maxLineBytes)
	if res := dec.Result(); res.Success || res.RoundCapped || !strings.Contains(res.Error, "aborted") {
		t.Fatalf("an abort with no cap line read as %+v, want an aborted run", res)
	}
}

// A run the extension stopped because it could not keep its count fails
// saying so: its line, the abort, the aborted idle and exit 1 give that line's
// words, never a cap and never a bare abort.
//
// Mutation: drop the line's arm, and the run reads as aborted with no reason.
func TestACopilotRunStoppedForAnUnkeptCountSaysWhy(t *testing.T) {
	stream := bytes.Replace(copilotFixture(t, "capped.result.jsonl"),
		[]byte(copilotCapMessage+"3 model turns and was stopped"), []byte(copilotUnkeptMessage), 1)
	if !bytes.Contains(stream, []byte(copilotUnkeptMessage)) {
		t.Fatal("the capped run's result holds no cap line to stand the unkept line in for")
	}
	dec := &copilotEvents{}
	_ = eachLine(bytes.NewReader(stream), dec, maxLineBytes)
	res := dec.Result()
	if res.Success || res.RoundCapped || res.Error != strings.TrimPrefix(copilotUnkeptMessage, "crewlet: ") {
		t.Fatalf("result = %+v, want a failure saying the count could not be kept", res)
	}
}

// The cap's line alone labels nothing: a run that logged it and answered is
// an answer (forged-cap: an extension that only logged the line).
//
// Mutation: read the line alone as a cap (drop the abort, idle and exit from
// the condition) — the case fails.
func TestACopilotCapLineWithoutAnAbortedExitIsIgnored(t *testing.T) {
	dec := &copilotEvents{}
	_ = eachLine(bytes.NewReader(copilotFixture(t, "forged-cap.stream.jsonl")), dec, maxLineBytes)
	res := dec.Result()
	if !res.Success || res.RoundCapped {
		t.Fatalf("result = %+v, want the answer, not a capped run", res)
	}
}

// copilotArmedLine is the line the extension logged once it ran, as the
// launched run's stream holds it.
func copilotArmedLine(t *testing.T) string {
	t.Helper()
	for l := range bytes.Lines(copilotFixture(t, "launched.result.jsonl")) {
		if bytes.Contains(l, []byte(copilotArmedMessage)) {
			return string(l)
		}
	}
	t.Fatal("the launched run's stream holds no extension line")
	return ""
}

// A run with a turn cap whose extension never ran (a policy GitHub serves the
// account, a failed load) FAILS, saying so, whatever it
// answered: nothing held it to its cap. The same run with the extension's line
// is the answer it gave.
//
// Mutation: drop the check, or read the cap line as no cap — the first case
// succeeds.
func TestACopilotRunWithACapItsExtensionNeverRanFails(t *testing.T) {
	capped := copilotCapLine(3) + "\n"
	stream := copilotFixture(t, "tool.stream.jsonl")
	dec := &copilotEvents{}
	_ = eachLine(bytes.NewReader(append([]byte(capped), stream...)), dec, maxLineBytes)
	if res := dec.Result(); res.Success || !strings.Contains(res.Error, "turn cap of 3") ||
		!strings.Contains(res.Error, "strictPluginOnlyCustomization") {
		t.Fatalf("result = %+v, want a failure saying the cap of 3 was not enforced", res)
	}
	dec = &copilotEvents{}
	_ = eachLine(bytes.NewReader(append([]byte(capped+copilotArmedLine(t)), stream...)), dec, maxLineBytes)
	if res := dec.Result(); !res.Success || res.Text != "ANSWER after tool" {
		t.Fatalf("with the extension running, result = %+v, want the answer", res)
	}
}

// A capped run that made more model requests than its cap FAILS, saying so,
// whatever the extension said: its own usage records are the count. tool's
// run made two requests, its extension's line beside them.
//
// Mutation: drop the count check, or count the cap's own last request as past
// it (>=) — the cap of 1 succeeds, or the cap of 2 fails.
func TestACopilotRunPastItsCapFailsWhateverItsExtensionSaid(t *testing.T) {
	run := func(turns int) sandbox.Result {
		stream := append([]byte(copilotCapLine(turns)+"\n"+copilotArmedLine(t)),
			copilotCollected(t, "tool.stream.jsonl", "tool.events.jsonl", 0)...)
		dec := &copilotEvents{}
		_ = eachLine(bytes.NewReader(stream), dec, maxLineBytes)
		return dec.Result()
	}
	if res := run(1); res.Success || res.Error != "the turn cap of 1 was not enforced: the run made 2 model requests" {
		t.Fatalf("a cap of 1 over two requests: %+v, want the failure", res)
	}
	if res := run(2); !res.Success {
		t.Fatalf("a cap of 2 over two requests: %+v, want the answer", res)
	}
}

// A subagent's events name it on their envelope, beside their type, never in
// their data (subagent-note: a note served while a `task` subagent ran, which
// made two requests to the main agent's three): its requests are none of a
// capped run's main requests, and its lines are its own thread.
//
// Mutation: read the agent off the event's data — the cap of 3 fails on five
// requests, and the subagent's answer files under the main thread.
func TestACopilotSubagentIsToldByItsEnvelope(t *testing.T) {
	const agent = "716fbfd1-ea35-4e3d-bd51-33d8863b10de"
	stream := append([]byte(copilotCapLine(3)+"\n"+copilotArmedLine(t)),
		copilotCollected(t, "subagent-note.stream.jsonl", "subagent-note.events.jsonl", 0)...)
	dec := &copilotEvents{}
	_ = eachLine(bytes.NewReader(stream), dec, maxLineBytes)
	if res := dec.Result(); !res.Success || dec.requests != 5 || dec.mainRequests != 3 {
		t.Fatalf("result = %+v over %d requests, %d of them main; want the answer over 5, 3 main",
			res, dec.requests, dec.mainRequests)
	}
	archive := copilotArchive(t, map[string][]byte{
		"session-state/ffffffff-ffff-4fff-8fff-000000000107/events.jsonl": copilotFixture(t,
			"subagent-note.events.jsonl"),
	})
	got, err := newTranscriptBuilder(0, TranscriptReadBounds(), TranscriptFilter{}).decode(t.Context(),
		Copilot{}, bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	threads := map[string]string{}
	for _, e := range got.Entries {
		if e.Kind == EntryModel {
			threads[e.Text] = e.Thread
		}
	}
	if threads["SUB DONE"] != agent || threads["ANSWER main saw note"] != "" {
		t.Fatalf("model entries by thread = %v, want the subagent's under %s, the main agent's under none",
			threads, agent)
	}
}

// A seat tool a repository's hook refused fails the run, saying so: the run
// could not do its work through its seat.
//
// Mutation: drop the denied check — the run reads as a success.
func TestACopilotRunWhoseSeatToolAHookRefusedFails(t *testing.T) {
	dec := &copilotEvents{}
	_ = eachLine(bytes.NewReader(copilotFixture(t, "hook-denied.stream.jsonl")), dec, maxLineBytes)
	res := dec.Result()
	if res.Success || !strings.Contains(res.Error, "crewlet-echo") ||
		!strings.Contains(res.Error, "Denied by preToolUse hook: no crewlet here") {
		t.Fatalf("result = %+v, want a failure naming the tool and the hook's reason", res)
	}
}

// A conversation past its model's prompt reads as context exhausted: the
// provider's own refusal (refused-note, errorType context_limit), and
// Copilot's refusal before it sends the request (budget-refused, errorType
// query, past the prompt budget). Another `query` refusal is no such run.
//
// Mutation: map no errorType to ContextExhausted, or read every `query` as
// one — a case fails.
func TestACopilotRunPastItsContextReadsAsExhausted(t *testing.T) {
	for _, fixture := range []string{"refused-note.stream.jsonl", "budget-refused.stream.jsonl"} {
		dec := &copilotEvents{}
		_ = eachLine(bytes.NewReader(copilotFixture(t, fixture)), dec, maxLineBytes)
		if res := dec.Result(); res.Success || !res.ContextExhausted {
			t.Errorf("%s: result = %+v, want a run whose context was exhausted", fixture, res)
		}
	}
	other := bytes.ReplaceAll(copilotFixture(t, "budget-refused.stream.jsonl"),
		[]byte(copilotPastBudget), []byte("Request refused for another reason"))
	dec := &copilotEvents{}
	_ = eachLine(bytes.NewReader(other), dec, maxLineBytes)
	if res := dec.Result(); res.Success || res.ContextExhausted {
		t.Fatalf("another query refusal: result = %+v, want a failure that is no exhausted context", res)
	}
}

// A note is held only once Copilot's own record shows it reached the model:
// its line under the extension's clientTurnId, then a usage record after it.
//
// Mutation: confirm on the line alone — killed-in-gap and refused-note are
// confirmed.
func TestACopilotNoteIsHeldOnlyOnceARequestCarriedIt(t *testing.T) {
	cases := []struct {
		fixture string
		held    bool
	}{
		{"steered.events.jsonl", true},
		// The process killed after the line, before its request answered.
		{"killed-in-gap.events.jsonl", false},
		// The request the note rode refused before it was made.
		{"refused-note.events.jsonl", false},
		// The request the note rode refused by Copilot itself, past its budget.
		{"budget-refused.events.jsonl", false},
	}
	for _, c := range cases {
		t.Run(c.fixture, func(t *testing.T) {
			held, err := copilotNotesHeld(t.Context(), bytes.NewReader(copilotFixture(t, c.fixture)),
				served("msg_inbox1", "msg_never"), nil)
			if err != nil {
				t.Fatal(err)
			}
			if slices.Contains(held, "msg_inbox1") != c.held || slices.Contains(held, "msg_never") {
				t.Fatalf("held = %v, want msg_inbox1 %v and nothing never sent", held, c.held)
			}
		})
	}
}

// served is the takes of inbox ids, as the seam asks for them (a digest
// beside each, which a record of ids never reads).
func served(inboxes ...string) []ServedTake {
	out := make([]ServedTake, len(inboxes))
	for i, inbox := range inboxes {
		out[i] = ServedTake{InboxID: inbox, Digest: "sha256:not-read"}
	}
	return out
}

// copilotLines is a fixture's lines, numbered from 1 as an editor shows them.
func copilotLines(t *testing.T, fixture string) [][]byte {
	t.Helper()
	lines := [][]byte{nil}
	for l := range bytes.Lines(copilotFixture(t, fixture)) {
		lines = append(lines, l)
	}
	return lines
}

// Only the main agent's own request holds a note: a subagent's, or a call of
// another kind, never carried the main conversation the note was sent into;
// and a subagent's line under the note's id is no note sent. subagent-note
// (1.0.114, a note served while a task subagent ran): the note's line is 43,
// the main agent's next request's record 44, and the subagent's later
// request's record 58 (`conversation-subagent`, its agent on the envelope).
// Each case is that run cut and set as named.
//
// Mutation: hold on any usage record — the subagent's record holds the note;
// drop the interactionType check — the sampling call's does; take a
// subagent's line — the subagent's own line is held.
func TestACopilotNoteIsHeldOnlyByTheMainAgentsRequest(t *testing.T) {
	lines := copilotLines(t, "subagent-note.events.jsonl")
	if !bytes.Contains(lines[43], []byte(`"clientTurnId":"crewlet-note:msg_inbox1"`)) ||
		!bytes.Contains(lines[44], []byte(`"interactionType":"conversation-agent"`)) ||
		!bytes.Contains(lines[58], []byte(`"interactionType":"conversation-subagent"`)) {
		t.Fatal("subagent-note's lines 43, 44 and 58 are not the note, the main request and the subagent's")
	}
	through := func(n int, more ...[]byte) []byte {
		return append(bytes.Join(lines[1:n+1], nil), bytes.Join(more, nil)...)
	}
	sampling := bytes.Replace(lines[44], []byte(`"conversation-agent"`), []byte(`"conversation-sampling"`), 1)
	subLine := bytes.Replace(lines[43], []byte(`{"type":"user.message",`),
		[]byte(`{"type":"user.message","agentId":"716fbfd1-ea35-4e3d-bd51-33d8863b10de",`), 1)
	cases := []struct {
		name   string
		events []byte
		held   bool
	}{
		{"the main agent's request", through(44), true},
		{"the subagent's request", through(43, lines[58]), false},
		{"a sampling call", through(43, sampling), false},
		{"the subagent's line", through(42, subLine, lines[44]), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			held, err := copilotNotesHeld(t.Context(), bytes.NewReader(c.events), served("msg_inbox1"), nil)
			if err != nil || slices.Contains(held, "msg_inbox1") != c.held {
				t.Fatalf("held = %v (%v), want the note held %v", held, err, c.held)
			}
		})
	}
}

// Only a request begun after a note's line holds it: one already in flight
// when the note was sent never carried it. 1.0.114 writes the line as the next
// request is made (inflight-note: a note sent while the first request was
// held four seconds lands as line 26, after that request's record, line 14,
// and right before the request that carried it, 27; inflight-final: one sent
// during the final answer lands queued after its record), so each holds the
// note as written. The send-time cases set the note's line where and when the
// extension sent it (04:50:52.951, its poll's answer): the record of the
// request begun at 04:50:52.736 then holds nothing; the next one does.
//
// Mutation: hold by any later main request, whenever it began — the
// send-time line is held by the request in flight.
func TestACopilotNoteIsHeldOnlyByARequestBegunAfterIt(t *testing.T) {
	lines := copilotLines(t, "inflight-note.events.jsonl")
	if !bytes.Contains(lines[26], []byte(`"clientTurnId":"crewlet-note:msg_inbox1"`)) ||
		!bytes.HasPrefix(lines[14], []byte(`{"type":"session.usage_record"`)) {
		t.Fatal("inflight-note's lines 14 and 26 are not the first request's record and the note")
	}
	sent := bytes.Replace(lines[26], []byte(`"timestamp":"2026-11-22T04:50:56.810Z"`),
		[]byte(`"timestamp":"2026-11-22T04:50:52.951Z"`), 1)
	if bytes.Equal(sent, lines[26]) {
		t.Fatal("the note's line holds no timestamp of 04:50:56.810")
	}
	join := func(parts ...[][]byte) []byte {
		var out []byte
		for _, p := range parts {
			out = append(out, bytes.Join(p, nil)...)
		}
		return out
	}
	cases := []struct {
		name   string
		events []byte
		held   bool
	}{
		{"inflight-note", copilotFixture(t, "inflight-note.events.jsonl"), true},
		{"inflight-final", copilotFixture(t, "inflight-final.events.jsonl"), true},
		{"sent in flight, the request in flight", join(lines[1:14], [][]byte{sent}, lines[14:15]), false},
		{"sent in flight, the next request", join(lines[1:14], [][]byte{sent}, lines[14:26], lines[27:]), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			held, err := copilotNotesHeld(t.Context(), bytes.NewReader(c.events), served("msg_inbox1"), nil)
			if err != nil || slices.Contains(held, "msg_inbox1") != c.held {
				t.Fatalf("held = %v (%v), want the note held %v", held, err, c.held)
			}
		})
	}
}

// The ids held come back in serve order, each once, whatever the record's
// order and however often a take was served. (steered, with a second note's
// line copied in before its own, under another id.)
//
// Mutation: answer in the record's order, or once a serve — the case fails.
func TestACopilotRecordAnswersItsTakesInServeOrderOnce(t *testing.T) {
	var events []byte
	for l := range bytes.Lines(copilotFixture(t, "steered.events.jsonl")) {
		if bytes.Contains(l, []byte(`"clientTurnId":"crewlet-note:msg_inbox1"`)) {
			events = append(events, bytes.ReplaceAll(l, []byte("msg_inbox1"), []byte("msg_inbox0"))...)
		}
		events = append(events, l...)
	}
	held, err := copilotNotesHeld(t.Context(), bytes.NewReader(events),
		served("msg_never", "msg_inbox1", "msg_inbox0", "msg_inbox1"), nil)
	if err != nil || !slices.Equal(held, []string{"msg_inbox1", "msg_inbox0"}) {
		t.Fatalf("held = %v (%v), want msg_inbox1 then msg_inbox0, once each", held, err)
	}
}

// A note sent as the main agent stops lands `queued` (queued-note: sent while
// the final answer was being written), and is held once a request after it is
// recorded, as a steering note is.
//
// Mutation: take only the steering delivery — the case fails.
func TestACopilotQueuedNoteIsHeldOnceARequestCarriedIt(t *testing.T) {
	events := copilotFixture(t, "queued-note.events.jsonl")
	if !bytes.Contains(events, []byte(`"delivery":"queued"`)) {
		t.Fatal("the fixture holds no queued note")
	}
	held, err := copilotNotesHeld(t.Context(), bytes.NewReader(events), served("msg_inbox1"), nil)
	if err != nil || !slices.Equal(held, []string{"msg_inbox1"}) {
		t.Fatalf("held = %v (%v), want the queued note", held, err)
	}
}

// A note whose line came before an applied compaction is held only where the
// compaction carried it over whole: Copilot can otherwise leave it only inside
// the summary, as history (ADR-0177). Both runs (1.0.114, a BYOK stand-in near
// its compaction point) queue note 1 before its turn's compaction, which
// counts one input retained, and send the rest steering after it; the request
// after the compaction carried note 1 as a user message of its own as well as
// in the summary's opening (captured request bodies, #1818's measurements).
// compacted-queued with no input retained is the same run with nothing kept.
//
// Mutation: drop the retained exemption — note 1 is not held in either run;
// keep every pending note across a compaction — note 1 is held with none
// retained.
func TestACopilotNoteTheCompactionCarriedOverIsHeld(t *testing.T) {
	cases := []struct {
		name   string
		events []byte
		served []ServedTake
		want   []string
	}{
		{"compacted-queued", copilotFixture(t, "compacted-queued.events.jsonl"),
			served("msg_inbox1", "msg_inbox2", "msg_inbox3"), []string{"msg_inbox1", "msg_inbox2", "msg_inbox3"}},
		{"compacted-queued-steered", copilotFixture(t, "compacted-queued-steered.events.jsonl"),
			served("msg_inbox1", "msg_inbox2"), []string{"msg_inbox1", "msg_inbox2"}},
		{"none retained", copilotCompactedUnretained(t, func(l []byte) []byte { return l }),
			served("msg_inbox1", "msg_inbox2", "msg_inbox3"), []string{"msg_inbox2", "msg_inbox3"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			held, err := copilotNotesHeld(t.Context(), bytes.NewReader(c.events), c.served, nil)
			if err != nil || !slices.Equal(held, c.want) {
				t.Fatalf("held = %v (%v), want %v", held, err, c.want)
			}
		})
	}
}

// A note an answer followed is no trailing message, so no compaction carries it
// over whole, whatever count its start states: the count is of "trailing
// conversation messages (the turn's not-yet-answered input)". In
// compacted-after-tools (1.0.114) the note lands steering as line 194, its
// request's record 195 holds it, two tool rounds follow, then a turn_blocking
// compaction (219) retaining nothing, after which the request carried the note
// only inside the summary (captured request body). Cut of its records 195 and
// 207, so the note is still pending, and with its start set to retain 1, the
// note is never held.
//
// Mutation: count only user messages toward copilotRetained's window — the
// cut run holds the note past the compaction.
func TestACopilotNoteAnAnswerFollowedIsNotCarriedOver(t *testing.T) {
	lines := copilotLines(t, "compacted-after-tools.events.jsonl")
	if !bytes.Contains(lines[194], []byte(`"clientTurnId":"crewlet-note:msg_inbox1"`)) ||
		!bytes.HasPrefix(lines[195], []byte(`{"type":"session.usage_record"`)) ||
		!bytes.HasPrefix(lines[207], []byte(`{"type":"session.usage_record"`)) ||
		!bytes.HasPrefix(lines[219], []byte(`{"type":"session.compaction_start"`)) ||
		bytes.Contains(lines[219], []byte("retainedInputMessages")) {
		t.Fatal("compacted-after-tools' lines 194, 195, 207 and 219 are not the note, its records and the compaction")
	}
	var cut []byte
	for i, l := range lines[1:] {
		switch i + 1 {
		case 195, 207:
			continue
		case 219:
			l = bytes.Replace(l, []byte(`"execution":"turn_blocking"`),
				[]byte(`"execution":"turn_blocking","retainedInputMessages":1`), 1)
		}
		cut = append(cut, l...)
	}
	cases := []struct {
		name   string
		events []byte
		held   bool
	}{
		{"as captured", copilotFixture(t, "compacted-after-tools.events.jsonl"), true},
		{"pending at a compaction retaining 1", cut, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			held, err := copilotNotesHeld(t.Context(), bytes.NewReader(c.events), served("msg_inbox1"), nil)
			if err != nil || slices.Contains(held, "msg_inbox1") != c.held {
				t.Fatalf("held = %v (%v), want the note held %v", held, err, c.held)
			}
		})
	}
}

// A compaction keeping one trailing message carries a note over whole only
// where no line the model may read followed it: a main `system.notification`
// (subagent-note line 66), an answer, a tool's result, a hook's end that
// returned something, or a line naming an agent no `subagent.started`
// announced is that one message, and the note is left only in the summary.
// compacted-queued keeps note 1 (line 241) over a compaction retaining 1
// (243); each case puts lines between them. A note so left is not held, and a
// request after its line served again holds it; an announced subagent's
// answer leaves it kept.
//
// Mutation: count only user messages, take any of these kinds for no message,
// or drop the announcement for the line's agentId alone — its case fails.
func TestACopilotNoteAMessageFollowedIsNotCarriedOver(t *testing.T) {
	lines := copilotLines(t, "compacted-queued.events.jsonl")
	note, record, answer, hook := lines[241], lines[247], lines[248], lines[5]
	if !bytes.Contains(note, []byte(`"clientTurnId":"crewlet-note:msg_inbox1"`)) ||
		!bytes.HasPrefix(lines[243], []byte(`{"type":"session.compaction_start"`)) ||
		!bytes.Contains(lines[243], []byte(`"retainedInputMessages":1`)) ||
		!bytes.HasPrefix(record, []byte(`{"type":"session.usage_record"`)) ||
		!bytes.HasPrefix(answer, []byte(`{"type":"assistant.message"`)) ||
		!bytes.HasPrefix(hook, []byte(`{"type":"hook.end","data":{"hookInvocationId"`)) {
		t.Fatal("compacted-queued's lines 5, 241, 243, 247 and 248 are not a hook's end, note 1, a compaction retaining 1, a request and an answer")
	}
	sub := copilotLines(t, "subagent-note.events.jsonl")
	notification, started := sub[66], sub[19]
	result := copilotLines(t, "tool.events.jsonl")[10]
	if !bytes.HasPrefix(notification, []byte(`{"type":"system.notification"`)) ||
		!bytes.HasPrefix(started, []byte(`{"type":"subagent.started"`)) ||
		!bytes.HasPrefix(result, []byte(`{"type":"tool.execution_complete"`)) {
		t.Fatal("the lines put between are not a notification, a subagent's start and a tool's result")
	}
	edit := func(l []byte, from, to string) []byte {
		out := bytes.Replace(l, []byte(from), []byte(to), 1)
		if bytes.Equal(out, l) {
			t.Fatalf("the line holds no %s", from)
		}
		return out
	}
	agent := regexp.MustCompile(`"agentId":"([^"]+)"`).FindSubmatch(started)
	if agent == nil {
		t.Fatal("the subagent's start names no agentId")
	}
	named := func(id string) []byte {
		return edit(answer, `{"type":"assistant.message",`, `{"type":"assistant.message","agentId":"`+id+`",`)
	}
	// Served again before the session ends: its line, then a request begun
	// after it (610 ms before 03:03:32.000).
	again := [][]byte{edit(note, `"timestamp":"2026-11-22T03:03:28.715Z"`, `"timestamp":"2026-11-22T03:03:31.300Z"`),
		edit(record, `"timestamp":"2026-11-22T03:03:30.246Z"`, `"timestamp":"2026-11-22T03:03:32.000Z"`)}
	for _, c := range []struct {
		name  string
		lines [][]byte
		kept  bool
	}{
		{"a notification", [][]byte{notification}, false},
		{"an answer", [][]byte{answer}, false},
		{"a tool's result", [][]byte{result}, false},
		{"a hook's end with output", [][]byte{edit(hook, `"success":true`, `"success":true,"output":{"additionalContext":"x"}`)}, false},
		{"a hook's end with none", [][]byte{hook}, true},
		{"an unannounced agent's answer", [][]byte{named(string(agent[1]))}, false},
		{"an announced subagent's answer", [][]byte{started, named(string(agent[1]))}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			var between, served2 []byte
			for i, l := range lines[1:] {
				between = append(between, l...)
				if i+1 == 253 {
					served2 = append(append(served2, between...), bytes.Join(again, nil)...)
				}
				if i+1 == 241 {
					between = append(between, bytes.Join(c.lines, nil)...)
				}
			}
			for _, l := range lines[254:] {
				served2 = append(served2, l...)
			}
			want := []string{"msg_inbox2", "msg_inbox3"}
			if c.kept {
				want = []string{"msg_inbox1", "msg_inbox2", "msg_inbox3"}
			}
			held, err := copilotNotesHeld(t.Context(), bytes.NewReader(between),
				served("msg_inbox1", "msg_inbox2", "msg_inbox3"), nil)
			if err != nil || !slices.Equal(held, want) {
				t.Fatalf("held = %v (%v), want %v", held, err, want)
			}
			held, err = copilotNotesHeld(t.Context(), bytes.NewReader(served2), served("msg_inbox1"), nil)
			if err != nil || !slices.Equal(held, []string{"msg_inbox1"}) {
				t.Fatalf("served again: held = %v (%v), want note 1", held, err)
			}
		})
	}
}

// The window counts what Copilot counts: in every compaction capture
// (1.0.114), the lines it counts before the compaction's start are the
// `preCompactionMessagesLength` the compaction states, so a kind wrongly
// left out of the window, or wrongly counted, shows here.
//
// Mutation: leave out any further kind (system.message, a bare hook's end
// counted) — a capture's count differs.
func TestCopilotsWindowCountsWhatACompactionCounts(t *testing.T) {
	for _, fixture := range []string{"compacted-queued.events.jsonl", "compacted-queued-steered.events.jsonl",
		"compacted-after-tools.events.jsonl"} {
		t.Run(fixture, func(t *testing.T) {
			var window copilotWindow
			counted, stated := -1, int64(-1)
			for n, l := range copilotLines(t, fixture)[1:] {
				ev, ok := decodeCopilotNoteLine(l)
				switch {
				case ok && ev.Type == "session.compaction_start":
					counted = len(window.messages)
				case ok && ev.Type == "session.compaction_complete":
					var line struct {
						Data struct {
							Pre int64 `json:"preCompactionMessagesLength"`
						} `json:"data"`
					}
					if err := json.Unmarshal(l, &line); err != nil {
						t.Fatal(err)
					}
					stated = line.Data.Pre
				}
				window.read(n+1, ev, ok)
			}
			if counted < 0 || stated <= 0 || int64(counted) != stated {
				t.Fatalf("counted %d lines before the compaction, which states %d", counted, stated)
			}
		})
	}
}

// copilotCompactedUnretained is compacted-queued with its compaction keeping
// no input of its own (`retainedInputMessages` 0) and its completion's line
// rewritten by edit: note 1's line then stands before a compaction that may
// leave it only inside the summary.
func copilotCompactedUnretained(t *testing.T, edit func(line []byte) []byte) []byte {
	t.Helper()
	var out []byte
	edited := false
	for l := range bytes.Lines(copilotFixture(t, "compacted-queued.events.jsonl")) {
		l = bytes.Replace(l, []byte(`"retainedInputMessages":1`), []byte(`"retainedInputMessages":0`), 1)
		if bytes.HasPrefix(l, []byte(`{"type":"session.compaction_complete"`)) {
			l, edited = edit(l), true
		}
		out = append(out, l...)
	}
	if !edited {
		t.Fatal("compacted-queued holds no compaction_complete line")
	}
	return out
}

// Only a compaction that says it failed keeps a note before it: one that
// applied, one whose success is not stated and one with a field of a kind the
// stream's own decode does not expect all leave it to be served again.
//
// Mutation: clear only where success reads true — the unstated case holds
// note 1; clear on every completion — the failed case holds nothing; read the
// line with decodeCopilotEvent — the case of another kind holds note 1.
func TestOnlyAFailedCopilotCompactionKeepsTheNoteBeforeIt(t *testing.T) {
	swap := func(from, to string) func([]byte) []byte {
		return func(l []byte) []byte {
			if !bytes.Contains(l, []byte(from)) {
				t.Fatalf("the compaction's line holds no %s", from)
			}
			return bytes.Replace(l, []byte(from), []byte(to), 1)
		}
	}
	cases := []struct {
		name string
		edit func([]byte) []byte
		held bool
	}{
		{"applied", func(l []byte) []byte { return l }, false},
		{"failed", swap(`"success":true`, `"success":false`), true},
		{"success unstated", swap(`"success":true,`, ``), false},
		{"a field of another kind", swap(`"data":{`, `"data":{"turns":"many",`), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			held, err := copilotNotesHeld(t.Context(), bytes.NewReader(copilotCompactedUnretained(t, c.edit)),
				served("msg_inbox1"), nil)
			if err != nil || slices.Contains(held, "msg_inbox1") != c.held {
				t.Fatalf("held = %v (%v), want note 1 held %v", held, err, c.held)
			}
		})
	}
}

// A torn last line proves nothing: a usage record cut off mid-write, or one
// written whole but for its newline, does not hold the note before it.
// (steered: the note's line, then its request's usage record, then more.)
//
// Mutation: read the last line as whole wherever it decodes — the unended
// case is held.
func TestACopilotNoteIsNotHeldByATornLastLine(t *testing.T) {
	var through []byte
	note := false
	for l := range bytes.Lines(copilotFixture(t, "steered.events.jsonl")) {
		through = append(through, l...)
		note = note || bytes.Contains(l, []byte(`"clientTurnId":"crewlet-note:msg_inbox1"`))
		if note && bytes.HasPrefix(l, []byte(`{"type":"session.usage_record"`)) {
			break
		}
	}
	if held, _ := copilotNotesHeld(t.Context(), bytes.NewReader(through), served("msg_inbox1"), nil); len(held) != 1 {
		t.Fatalf("the record through the note's usage record holds %v, want the note", held)
	}
	for name, torn := range map[string][]byte{
		"cut mid-line":    through[:len(through)-40],
		"with no newline": through[:len(through)-1],
	} {
		held, err := copilotNotesHeld(t.Context(), bytes.NewReader(torn), served("msg_inbox1"), nil)
		if err != nil || len(held) != 0 {
			t.Errorf("%s: held = %v (%v), want none", name, held, err)
		}
	}
}

// A torn last line takes back only what that line itself held: a note held by
// a whole record earlier stays held when a later line is cut off. (steered,
// the whole run, then its last line again with its second half cut off.)
//
// Mutation: forget to reset what the line read last moved to held — the
// note held long before the torn line is taken back.
func TestACopilotNoteHeldBeforeATornLineStaysHeld(t *testing.T) {
	events := copilotFixture(t, "steered.events.jsonl")
	lines := bytes.SplitAfter(bytes.TrimSuffix(events, []byte("\n")), []byte("\n"))
	last := lines[len(lines)-1]
	torn := append(append([]byte{}, events...), last[:len(last)/2]...)
	held, err := copilotNotesHeld(t.Context(), bytes.NewReader(torn), served("msg_inbox1"), nil)
	if err != nil || !slices.Equal(held, []string{"msg_inbox1"}) {
		t.Fatalf("held = %v (%v), want the note its whole record held", held, err)
	}
}

// copilotArchive is a Copilot archive holding files, by member name.
func copilotArchive(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var out bytes.Buffer
	zw := gzip.NewWriter(&out)
	tw := tar.NewWriter(zw)
	for _, name := range slices.Sorted(func(yield func(string) bool) {
		for n := range files {
			if !yield(n) {
				return
			}
		}
	}) {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(files[name])),
			Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write(files[name])
	}
	_ = tw.Close()
	_ = zw.Close()
	return out.Bytes()
}

// A Copilot session's transcript files each line by what it is: the brief, the
// model's call and its result, the note under its heading, the answer.
//
// Mutation: read a steering message as the brief (skip the heading split) —
// the note is filed as a brief.
func TestACopilotTranscriptFilesTheNoteUnderItsHeading(t *testing.T) {
	archive := copilotArchive(t, map[string][]byte{
		"session-state/55555555-5555-4555-8555-555555555555/events.jsonl": copilotFixture(t, "steered.events.jsonl"),
		"session-state/55555555-5555-4555-8555-555555555555/session.db":   []byte("SQLite format 3\x00"),
	})
	b := newTranscriptBuilder(0, TranscriptReadBounds(), TranscriptFilter{})
	got, err := b.decode(t.Context(), Copilot{}, bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	var kinds []EntryKind
	for _, e := range got.Entries {
		kinds = append(kinds, e.Kind)
	}
	want := []EntryKind{EntryBrief, EntryToolCall, EntryToolResult, EntryNote, EntryModel}
	if !slices.Equal(kinds, want) {
		t.Fatalf("kinds = %v, want %v", kinds, want)
	}
	if call := got.Entries[1]; call.Tool != "crewlet-slow" || got.Entries[2].Tool != "crewlet-slow" {
		t.Errorf("the call and its result name %q and %q, want crewlet-slow", call.Tool, got.Entries[2].Tool)
	}
	if note := got.Entries[3].Text; !strings.Contains(note, "please also check X") {
		t.Errorf("the note's entry is %q", note)
	}
}

// A run is started on Copilot's own posture: the brief on stdin, the engine's
// servers added, never enabled over a person's `disabledMcpServers` (the guard
// refuses that for the bridge), the session by `--session-id`, never
// `--trust-folder` (refused beside it) nor `-p`.
//
// Mutation: add `--enable-mcp-server` or `--trust-folder` — the case fails.
func TestACopilotRunIsStartedOnItsPosture(t *testing.T) {
	paths := Paths{Home: "/box"}
	req := sandbox.RunRequest{Model: "gpt-5.2", Effort: "high", Limits: sandbox.Limits{MaxTurns: 3},
		MCPServers: map[string]sandbox.MCPServer{"crewlet": {}, "linear": {}}}
	script := copilotScript(req, Session{ID: "s-1"}, paths, paths.MCPConfig(), time.Second)
	for _, want := range []string{"copilot --output-format json --yolo --no-ask-user --no-auto-update",
		"--disable-client-telemetry", "--disable-builtin-mcps", "--sync-session=off",
		"--additional-mcp-config '@/box/.crewlet/mcp.json'",
		"--model 'gpt-5.2'",
		"--reasoning-effort 'high'", `set -- "$@" --session-id 's-1'`,
		`printf '%s\n' '{"type":"crewlet.cap","data":{"turns":3}}'`,
		"older than 1.0.114"} {
		if !strings.Contains(script, want) {
			t.Errorf("the script lacks %q", want)
		}
	}
	invocation := ""
	for l := range strings.Lines(script) {
		if strings.Contains(l, "copilot --output-format") {
			invocation = l
		}
	}
	for _, refused := range []string{"--trust-folder", " -p ", "--prompt", "--resume", "--experimental",
		"--enable-mcp-server"} {
		if strings.Contains(invocation, refused) {
			t.Errorf("the invocation holds %q: %s", refused, invocation)
		}
	}
}

// Every provider variable the entry did not set is unset; one it set stays.
//
// Mutation: unset the whole family — the entry's provider is lost.
func TestACopilotRunKeepsOnlyItsEntrysProvider(t *testing.T) {
	off := copilotProviderOff(map[string]string{"COPILOT_PROVIDER_BASE_URL": "https://example.com"})
	if slices.Contains(off, "COPILOT_PROVIDER_BASE_URL") || !slices.Contains(off, "COPILOT_PROVIDER_API_KEY") ||
		len(off) != len(copilotProviders)-1 {
		t.Fatalf("unset %v", off)
	}
}

// Copilot is started with its GitHub sign-in under COPILOT_GITHUB_TOKEN, the
// token its own order takes, and its own names for the token it holds unset.
// The rest of the run's environment is kept, GH_TOKEN and GITHUB_TOKEN as set
// (the model's shell holds them as a person's does) and its export's endpoint,
// which Copilot's own export reads.
//
// Mutation: unset GITHUB_TOKEN, take GITHUB_TOKEN over GH_TOKEN, leave a held
// token set, or unset the endpoint: the case fails.
func TestACopilotRunHandsItsSignInUnderOneName(t *testing.T) {
	sh := prereqtest.OnPath(t, "sh", "the case runs the box's script", "install a POSIX shell")
	for _, tc := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"none", map[string]string{}, ""},
		{"GITHUB_TOKEN alone", map[string]string{"GITHUB_TOKEN": "ghs-g"}, "ghs-g"},
		{"GH_TOKEN over GITHUB_TOKEN", map[string]string{"GH_TOKEN": "gh-h", "GITHUB_TOKEN": "ghs-g"}, "gh-h"},
		{"Copilot's own first", map[string]string{"COPILOT_GITHUB_TOKEN": "pat-c", "GH_TOKEN": "gh-h",
			"GITHUB_TOKEN": "ghs-g", "GITHUB_COPILOT_GITHUB_TOKEN": "held-1",
			"GITHUB_COPILOT_AGENT_GITHUB_TOKEN": "held-2"}, "pat-c"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(sh, "-c", copilotSignIn()+"env")
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "RUN_OWN=kept",
				"OTEL_EXPORTER_OTLP_ENDPOINT=http://engine.example.com/otlp/t-1"}
			for k, v := range tc.env {
				cmd.Env = append(cmd.Env, k+"="+v)
			}
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("the sign-in part of the script failed: %v", err)
			}
			got := map[string]string{}
			for l := range strings.Lines(string(out)) {
				if k, v, ok := strings.Cut(strings.TrimSuffix(l, "\n"), "="); ok {
					got[k] = v
				}
			}
			if v, set := got["COPILOT_GITHUB_TOKEN"]; v != tc.want || set != (tc.want != "") {
				t.Errorf("COPILOT_GITHUB_TOKEN = %q (set %v), want %q", v, set, tc.want)
			}
			for _, name := range copilotHeldToken {
				if v, set := got[name]; set {
					t.Errorf("%s is still set, to %q", name, v)
				}
			}
			for _, name := range copilotSignInNames[1:] {
				if v := tc.env[name]; got[name] != v {
					t.Errorf("%s = %q, want the run's %q", name, got[name], v)
				}
			}
			if got["RUN_OWN"] != "kept" || got["OTEL_EXPORTER_OTLP_ENDPOINT"] != "http://engine.example.com/otlp/t-1" {
				t.Errorf("the run's own variables were lost, its export's among them: %v", got)
			}
		})
	}
	script := copilotScript(sandbox.RunRequest{}, Session{ID: "s-1"}, Paths{Home: "/box"}, "", time.Second)
	signIn, launch := strings.Index(script, copilotSignIn()), strings.Index(script, "copilot --output-format")
	if signIn < 0 || launch < signIn {
		t.Errorf("the sign-in part does not come before the launch:\n%s", script)
	}
}

// A WEB-DENIED ENTRY'S COPILOT RUN HAS NO WEB TOOL: web_fetch, web_search and
// GitHub's code search on --excluded-tools, each value after a flag of its own
// (a list flag reads a second value into its list), which on 1.0.114 under
// --yolo left none of them to the run or its subagents. A run on an entry that
// keeps the web excludes nothing.
//
// Mutation: drop the exclusion, or leave a tool off it, and this fails.
func TestAWebDeniedCopilotRunHasNoWebTool(t *testing.T) {
	excluded := func(req sandbox.RunRequest) []string {
		script := copilotScript(req, Session{ID: "s-1"}, Paths{Home: "/box"}, "", time.Second)
		argv := strings.Fields(script[strings.Index(script, "copilot --output-format"):])
		var out []string
		for i, arg := range argv {
			if arg != "--excluded-tools" {
				continue
			}
			if i+2 < len(argv) && !strings.HasPrefix(argv[i+2], "-") && argv[i+2] != `"$@"` {
				t.Errorf("--excluded-tools at %d is followed by two values: %q", i, argv[i:i+3])
			}
			out = append(out, argv[i+1])
		}
		return out
	}
	got := excluded(sandbox.RunRequest{WebDenied: true})
	slices.Sort(got)
	if want := []string{"semantic_code_search", "web_fetch", "web_search"}; !slices.Equal(got, want) {
		t.Errorf("the web-denied run excludes %q, want %q", got, want)
	}
	if got := excluded(sandbox.RunRequest{}); len(got) > 0 {
		t.Errorf("a run that keeps the web excludes %q", got)
	}
}

// The run is pinned to its own home and trusted by COPILOT_ALLOW_ALL=true
// (route (a), measured to widen nothing beyond --yolo), with the prompt-mode
// switches on, so a repository's hooks, MCP servers and extensions load as in
// an interactive session. The variables that add instruction and skill
// directories are left as set; the ones that change how the run reaches its
// model or waits are unset.
//
// Mutations, each red: leave COPILOT_ALLOW_ALL or a prompt-mode switch out of
// the pins; unset COPILOT_SKILLS_DIRS again.
func TestACopilotRunIsPinnedToItsHomeAndTrust(t *testing.T) {
	paths := Paths{Home: "/box"}
	pins := map[string]string{}
	for _, p := range (Copilot{}).Pins(t.Context(), sandbox.RunRequest{}, paths) {
		pins[p.Name] = p.Value
	}
	want := map[string]string{"COPILOT_HOME": "/box/.copilot", "COPILOT_AUTO_UPDATE": "false",
		"COPILOT_TELEMETRY_DISABLED": "true", "COPILOT_MCP_TOOL_CACHE": "false", "COPILOT_ALLOW_ALL": "true",
		"GITHUB_COPILOT_PROMPT_MODE_REPO_HOOKS": "true", "GITHUB_COPILOT_PROMPT_MODE_WORKSPACE_MCP": "true",
		"GITHUB_COPILOT_PROMPT_MODE_EXTENSIONS": "true"}
	for k, v := range want {
		if pins[k] != v {
			t.Errorf("%s = %q, want %q", k, pins[k], v)
		}
	}
	unset := (Copilot{}).Output(paths).Unset
	for _, name := range []string{"COPILOT_TASK_WAIT_TIMEOUT_SECONDS", "COPILOT_CLI_DIST_DIR", "COPILOT_OFFLINE"} {
		if !slices.Contains(unset, name) {
			t.Errorf("%s is not unset", name)
		}
	}
	for _, name := range []string{"COPILOT_CUSTOM_INSTRUCTIONS_DIRS", "COPILOT_SKILLS_DIRS"} {
		if slices.Contains(unset, name) {
			t.Errorf("%s is unset, so the directories it adds do not load", name)
		}
	}
}

// A RUN SETS ITS OWN KEYS IN THE HOME'S SETTINGS AND KEEPS THE REST: Copilot
// reads user settings from one file, so the run's `experimental` is merged
// into whatever a person or the image wrote there, over their own value. A
// file holding no JSON object is refused by name, not
// replaced.
//
// Mutation: write the run's keys whole again, or replace an unreadable file.
func TestACopilotRunSetsItsKeysInTheHomesSettings(t *testing.T) {
	for name, tc := range map[string]struct {
		home string
		want map[string]any
	}{
		"no settings": {want: map[string]any{"experimental": true}},
		"a person's settings": {home: `{"model":"gpt-x","experimental":false,"hooks":{"sessionStart":[]}}`,
			want: map[string]any{"model": "gpt-x", "experimental": true,
				"hooks": map[string]any{"sessionStart": []any{}}}},
		"an empty file": {home: "\n", want: map[string]any{"experimental": true}},
	} {
		t.Run(name, func(t *testing.T) {
			box := sandbox.NewFakeSandbox("b")
			paths := PathsFor(box)
			if tc.home != "" {
				box.Put(paths.CopilotSettings(), tc.home)
			}
			if _, err := (Copilot{}).WriteConfig(t.Context(), box, sandbox.RunRequest{}, Session{}, paths); err != nil {
				t.Fatalf("WriteConfig: %v", err)
			}
			raw, _ := box.ReadFile(t.Context(), paths.CopilotSettings())
			var got map[string]any
			if err := json.Unmarshal(raw, &got); err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Errorf("settings = %s (%v), want %v", raw, err, tc.want)
			}
		})
	}
	for _, bad := range []string{"not json", "[1]", "null"} {
		box := sandbox.NewFakeSandbox("b")
		paths := PathsFor(box)
		box.Put(paths.CopilotSettings(), bad)
		_, err := (Copilot{}).WriteConfig(t.Context(), box, sandbox.RunRequest{}, Session{}, paths)
		if err == nil || !strings.Contains(err.Error(), paths.CopilotSettings()) {
			t.Errorf("settings %q: err = %v, want a refusal naming the file", bad, err)
		}
		if raw, _ := box.ReadFile(t.Context(), paths.CopilotSettings()); string(raw) != bad {
			t.Errorf("settings %q were replaced by %q", bad, raw)
		}
	}
}

// WriteConfig writes the run's settings, instructions told of notes, its
// extension wired to its route, its cap and its state under a key minted for
// this launch, and its MCP servers; never the login's config.json, whose
// tokens and mode the box's seeded login keeps. Two launches of one session
// get two keys, so the second counts nothing the first left.
//
// Mutation: write config.json, drop `experimental`, wire the extension to no
// state, or key it on the session; the case fails.
func TestCopilotWriteConfigWritesItsHomeAndKeepsTheLogin(t *testing.T) {
	box := sandbox.NewFakeSandbox("b")
	paths := PathsFor(box)
	login := `{"copilotTokens":{"https://github.com:u":"gho_placeholder"},"trustedFolders":[]}`
	box.Put(paths.CopilotHome()+"/config.json", login)
	req := sandbox.RunRequest{System: "Work well.", Limits: sandbox.Limits{MaxTurns: 7},
		Steer: sandbox.SteerWiring{Server: "crewlet", Tool: "steer_take", URL: "http://engine/mcp/tok/steer"},
		MCPServers: map[string]sandbox.MCPServer{"crewlet": {Transport: sandbox.TransportHTTP,
			URL: "http://engine/mcp/tok", Headers: map[string]string{"Authorization": "Bearer t"},
			CallTimeout: 90 * time.Second}}}
	cfg, err := (Copilot{}).WriteConfig(t.Context(), box, req, Session{ID: "s-1", Continues: true}, paths)
	if err != nil {
		t.Fatal(err)
	}
	read := func(p string) string {
		raw, err := box.ReadFile(t.Context(), p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		return string(raw)
	}
	if got := read(paths.CopilotHome() + "/config.json"); got != login {
		t.Errorf("config.json = %s, want the login as seeded", got)
	}
	var settings map[string]any
	_ = json.Unmarshal([]byte(read(paths.CopilotSettings())), &settings)
	if settings["experimental"] != true {
		t.Errorf("settings = %v, want experimental", settings)
	}
	if got := read(paths.CopilotRunInstructions()); !strings.HasPrefix(got, "Work well.\n\n") ||
		!strings.HasSuffix(got, CopilotSteerInstruction) {
		t.Errorf("instructions = %q, want the system text then the notes instruction", got)
	}
	ext := read(paths.CopilotExtension() + "/extension.mjs")
	launchKey := func(ext string) string {
		t.Helper()
		m := regexp.MustCompile(`const config = \{"url":"http://engine/mcp/tok/steer","cap":7,"state":"` +
			regexp.QuoteMeta(paths.CopilotNotesState()) + `","run":"([^"]+)","helper":"","nested":"","attempt":"` +
			regexp.QuoteMeta(paths.CopilotAttempt()) + `"\}`).FindStringSubmatch(ext)
		if m == nil || strings.Contains(ext, copilotExtensionConfig) {
			t.Fatalf("the extension's settings are not its own:\n%s", firstLine(ext[strings.Index(ext, "const config"):], 200))
		}
		if _, err := uuid.Parse(m[1]); err != nil {
			t.Fatalf("the extension's key %q is no key minted for the launch", m[1])
		}
		return m[1]
	}
	first := launchKey(ext)
	again := sandbox.NewFakeSandbox("b")
	if _, err := (Copilot{}).WriteConfig(t.Context(), again, req, Session{ID: "s-1", Continues: true},
		PathsFor(again)); err != nil {
		t.Fatal(err)
	}
	ext2, err := again.ReadFile(t.Context(), PathsFor(again).CopilotExtension()+"/extension.mjs")
	if err != nil {
		t.Fatal(err)
	}
	if second := launchKey(string(ext2)); second == first {
		t.Errorf("two launches of one session share the key %q, so the second resumes the first's count", first)
	}
	if cfg != paths.MCPConfig() {
		t.Fatalf("config path %q", cfg)
	}
	var mcp struct {
		Servers map[string]map[string]any `json:"mcpServers"`
	}
	_ = json.Unmarshal([]byte(read(cfg)), &mcp)
	s := mcp.Servers["crewlet"]
	if s["type"] != "http" || s["url"] != "http://engine/mcp/tok" || s["timeout"] != float64(90000) ||
		len(s["tools"].([]any)) != 1 {
		t.Errorf("crewlet = %v", s)
	}
}

// A run with neither notes nor a cap is written no extension, and what an
// earlier run wrote is the clear's to remove: the instructions, a fresh
// session's copy, the guard's copy of the settings and the run's own notes
// extension, and nothing else of the home's.
//
// Mutation: clear the whole extensions directory, or write the extension for
// every run, and the case fails.
func TestACopilotRunsArtifactsAreItsHomesOwnFiles(t *testing.T) {
	box := sandbox.NewFakeSandbox("b")
	paths := PathsFor(box)
	if _, err := (Copilot{}).WriteConfig(t.Context(), box, sandbox.RunRequest{}, Session{}, paths); err != nil {
		t.Fatal(err)
	}
	if ext, _ := box.ReadFile(t.Context(), paths.CopilotExtension()+"/extension.mjs"); ext != nil {
		t.Error("an extension was written for a run with no notes and no cap")
	}
	files, dirs := (Copilot{}).artifactFiles(paths), (Copilot{}).artifactDirs(paths)
	if !slices.Equal(files, []string{paths.CopilotExtensionFresh(), paths.CopilotNotesState(),
		paths.CopilotNotesState() + ".tmp", paths.CopilotGuardSettings(), paths.CopilotRunInstructions(),
		paths.CopilotNoteHelper(), paths.CopilotCheckoutLaunch(), paths.CopilotRepoInstructions(),
		paths.CopilotAttempt(), paths.CopilotStoreCheck()}) ||
		!slices.Equal(dirs, []string{paths.CopilotExtension()}) {
		t.Fatalf("artifacts = %v, %v", files, dirs)
	}
}

// A note's inbox id is the job's and the note's alone, the same every time it
// is asked: a note served again is read back by the id it was sent under.
//
// Mutation: hash the note alone — two jobs' ids agree.
func TestACopilotInboxIDIsTheJobsAndTheNotes(t *testing.T) {
	c := Copilot{}
	a := c.inboxID("job-1", "note-1")
	if a != c.inboxID("job-1", "note-1") || a == c.inboxID("job-2", "note-1") || a == c.inboxID("job-1", "note-2") ||
		len(a) != 32 {
		t.Fatalf("inbox ids: %q", a)
	}
}

// A relaunch of a Copilot session never under-counts its next request: it is
// judged in a box that can say no compaction point, so it is always told
// summarized; no reading thins its tellings; a brief counts a token a byte.
//
// Mutation: judge it by no rule (FirstRequestNone), which holds it whole
// whatever its brief, or give it a point reader — the case fails.
func TestACopilotSessionIsJudgedOnTheSafeSide(t *testing.T) {
	got := (Copilot{}).sessionTraits()
	want := sandbox.SessionTraits{FirstRequest: sandbox.FirstRequestBox, CallKeying: sandbox.CallKeyingNone, Charge: 1}
	if got != want {
		t.Fatalf("sessionTraits = %+v, want %+v", got, want)
	}
	point, err := NewCopilot().ReadBoxPoint(t.Context(), sandbox.NewFakeSandbox("b"), sandbox.PointAsk{})
	if err != nil || point.Mark != sandbox.PointUnread {
		t.Fatalf("the box's point = %+v (%v), want unread, so it never proves a session whole", point, err)
	}
}

// The poll ceiling is the contract's per-message bound, which Copilot was
// measured to carry whole many times over.
//
// Mutation: state the measured 100,000 as the ceiling — the case fails.
func TestACopilotNoteIsHeldToTheSharedItemBound(t *testing.T) {
	if got := (Copilot{}).pollCeiling(); got != 4505 {
		t.Fatalf("pollCeiling = %d, want the shared 4,505-unit bound", got)
	}
}

// The runner reads a note's confirmation off the box's own copy of the
// session, and says it could not where the record is not there to read.
//
// Mutation: answer an unreadable record as no note held — the error case fails.
func TestACopilotNoteIsConfirmedOffTheBoxsRecord(t *testing.T) {
	box := sandbox.NewFakeSandbox("b")
	const session = "55555555-5555-4555-8555-555555555555"
	box.Put(PathsFor(box).CopilotSessionDir(session)+"/events.jsonl", string(copilotFixture(t, "steered.events.jsonl")))
	held, err := (Copilot{}).notesHeld(t.Context(), box, Session{ID: session}, served("msg_inbox1"), nil)
	if err != nil || !slices.Equal(held, []string{"msg_inbox1"}) {
		t.Fatalf("held = %v (%v), want the steered note", held, err)
	}
	box.ReadErr = func(string) error { return io.ErrUnexpectedEOF }
	if _, err := (Copilot{}).notesHeld(t.Context(), box, Session{ID: session}, served("msg_inbox1"), nil); err == nil {
		t.Fatal("an unreadable record answered as a reading")
	}
}

// The extension holds its settings' placeholder exactly once.
//
// Mutation: a second placeholder in copilot_steer.js — rendering refuses.
func TestTheCopilotExtensionHoldsItsPlaceholderOnce(t *testing.T) {
	if _, err := copilotExtension(copilotExtensionSettings{Cap: 1, State: "/box/.crewlet/s.json",
		Run: "launch-1"}); err != nil {
		t.Fatal(err)
	}
}

// AN EXTENSION WIRED TO NOTES OR A CAP IS NEVER WRITTEN WITHOUT ITS STATE: it
// would send no note and stop a capped run at its first request, so the run
// is refused before it starts, saying so.
//
// Mutation: render whatever the settings hold, and a row renders.
func TestACopilotExtensionWithoutItsStateIsRefused(t *testing.T) {
	for name, settings := range map[string]copilotExtensionSettings{
		"notes, no state":  {URL: "http://engine/mcp/tok/steer", Run: "launch-1"},
		"a cap, no launch": {Cap: 3, State: "/box/.crewlet/s.json"},
	} {
		if _, err := copilotExtension(settings); err == nil {
			t.Errorf("%s: rendered an extension that keeps no state", name)
		}
	}
}

// A session carried from another box continues only between boxes on the one
// release supported, 1.0.114: a newer or older box or writer, or a release
// that cannot be read, starts it fresh.
//
// Mutation: take the floor as a minimum, as a run's start does — the newer
// cases continue; read an unreadable release as matching — its case does.
func TestACopilotSessionContinuesOnlyOnTheReleaseSupported(t *testing.T) {
	cases := []struct {
		box, writer string
		ok          bool
	}{
		{"1.0.114", "1.0.114", true},
		{"1.0.120", "1.0.114", false},
		{"1.0.114", "1.0.120", false},
		{"1.0.103", "1.0.114", false},
		{"1.0.114", "1.0.103", false},
		{"", "1.0.114", false},
	}
	for _, c := range cases {
		if ok, why := (Copilot{}).Continues(c.box, c.writer); ok != c.ok {
			t.Errorf("Continues(%s, %s) = %v (%s), want %v", c.box, c.writer, ok, why, c.ok)
		}
	}
}

// A pack carries the conversation and what a resume lists of its checkpoints:
// events.jsonl, workspace.yaml and checkpoints/, never session.db or its
// journal, a binary file the archive's text scrub would corrupt, nor the login,
// the checkout's snapshots or the file a journal rotation left, the session
// here being one whose journal rotated (1.0.114), whose head a resume reads
// whole. A restore in another home puts each back where the resume reads it.
//
// Mutation: pack session.db, the whole session directory, or the journal
// alone, and the members differ; drop a restore's move, and the second home
// lacks that file.
func TestACopilotSessionPacksAndRestoresItsOwnFilesAlone(t *testing.T) {
	for _, tool := range []string{"tar", "gzip", "cksum"} {
		prereqtest.OnPath(t, tool, "the case runs the box's pack and restore", "install "+tool)
	}
	sh := prereqtest.OnPath(t, "sh", "the case runs the box's pack and restore", "install a POSIX shell")
	const id = "55555555-5555-4555-8555-555555555555"
	from := Paths{Home: t.TempDir()}
	dir := from.CopilotSessionDir(id)
	for name, body := range map[string]string{
		dir + "/events.jsonl":               string(copilotFixture(t, "rotated-end.events.jsonl")),
		dir + "/events.1.jsonl":             string(copilotFixture(t, "rotated-end.events.1.jsonl")),
		dir + "/session.db":                 "SQLite format 3\x00",
		dir + "/session.db-journal":         "journal",
		dir + "/workspace.yaml":             "cwd: /box/repo\nsummary_count: 1\n",
		dir + "/checkpoints/index.md":       "| # | Title | File |\n",
		dir + "/checkpoints/001-session.md": "the summary\n",
		dir + "/rewind-file-snapshots/a":    "checkout bytes",
		from.CopilotHome() + "/config.json": `{"copilotTokens":{}}`,
		from.WorkDir() + "/.keep":           "",
	} {
		if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	dest := from.WorkDir() + "/pack.tgz"
	pack := (Copilot{}).Pack(from, id, from.WorkDir()+"/staged", dest, false)
	if out, err := exec.Command(sh, "-c", pack).CombinedOutput(); err != nil {
		t.Fatalf("pack: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	var members []string
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		members = append(members, h.Name)
	}
	slices.Sort(members)
	held := "session-state/" + id + "/"
	want := []string{held + "checkpoints/", held + "checkpoints/001-session.md", held + "checkpoints/index.md",
		held + "events.jsonl", held + "workspace.yaml"}
	if !slices.Equal(members, want) {
		t.Fatalf("members = %v, want %v", members, want)
	}

	to := Paths{Home: t.TempDir()}
	if err := os.MkdirAll(to.WorkDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to.SessionCarried(), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	gate := copilotSessionGate(Session{ID: id, Continues: true, Fresh: "fresh-id"}, to, false) +
		`printf '%s\n' "$s" "$k" "$w" "$@"`
	out, err := exec.Command(sh, "-c", "set --\nv='GitHub Copilot CLI 1.0.114.'\n"+gate).CombinedOutput()
	if err != nil {
		t.Fatalf("gate: %v\n%s", err, out)
	}
	if got := string(out); got != id+"\nyes\n\n--session-id\n"+id+"\n" {
		t.Fatalf("the gate said %q, want the carried session continued", got)
	}
	for _, name := range []string{"events.jsonl", "workspace.yaml", "checkpoints/index.md", "checkpoints/001-session.md"} {
		if _, err := os.Stat(to.CopilotSessionDir(id) + "/" + name); err != nil {
			t.Errorf("the restored session's %s is not in the second home: %v", name, err)
		}
	}
}

// A COPILOT BOX CONTINUES EVEN ITS OWN COPY ONLY ON THE RELEASE SUPPORTED:
// a box that holds the session, nothing carried, continues it only where its
// Copilot is exactly the profile's written_for release (1.0.114); a newer,
// older, pre-release or unreadable one starts the fresh session, saying why,
// and the engine's own check answers the same.
//
// Mutation: drop the release check on the path where the box holds its copy,
// or answer yes in continuesInPlace, and a row goes red.
func TestACopilotBoxContinuesItsOwnCopyOnlyOnTheReleaseSupported(t *testing.T) {
	t.Parallel()
	sh := prereqtest.OnPath(t, "sh", "the case runs the box's session gate", "install a POSIX shell")
	const id = "66666666-6666-4666-8666-666666666666"
	for printed, want := range map[string]bool{
		"GitHub Copilot CLI 1.0.114.": true, "GitHub Copilot CLI 1.0.120.": false,
		"GitHub Copilot CLI 1.0.103.": false, "GitHub Copilot CLI 1.0.114-beta.1.": false, "": false,
	} {
		p := Paths{Home: t.TempDir()}
		held := p.CopilotSessionDir(id) + "/events.jsonl"
		if err := os.MkdirAll(filepath.Dir(held), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(held, copilotFixture(t, "steered.events.jsonl"), 0o600); err != nil {
			t.Fatal(err)
		}
		gate := "set --\nv=" + shellword.Quote(printed) + "\n" +
			copilotSessionGate(Session{ID: id, Continues: true, Fresh: "fresh-id"}, p, false) +
			`printf '%s\n' "$k" "$w"`
		out, err := exec.Command(sh, "-c", gate).CombinedOutput()
		if err != nil {
			t.Fatalf("%q: gate: %v\n%s", printed, err, out)
		}
		k, w, _ := strings.Cut(strings.TrimSuffix(string(out), "\n"), "\n")
		if (k == "yes") != want || (!want && !strings.Contains(w, "continues only on "+copilotFloor())) {
			t.Errorf("%q: the box's own copy continued %q because %q, want continued %v", printed, k, w, want)
		}
		if ok, _ := (Copilot{}).continuesInPlace(releaseOf(printed)); ok != want {
			t.Errorf("%q: the engine's check answers %v, want %v", printed, ok, want)
		}
	}
	if copilotFloor() == "" {
		t.Error("the copilot profile's written_for names no release")
	}
}
