package executor

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// THE CASES BELOW READ ARCHIVES TWO WAYS. Most build a main thread of the
// layer's own records (claudethread_test.go), shaped as Claude Code writes them,
// and a subagent's thread and metadata beside it where they read one, and tar
// them as the take's scrub writes an archive. The rest read sessions the
// probes measured, cut into testdata/claude/<release>/holdings/<name> as the
// members their archive held ([hsFixtures]).

// hsFixtures are the measured threads the cases read, each a member tree as its
// session's archive held it, by release and name. Each was cut as data, every
// line in its order with its keys in their order and types: the probes' call ids
// are their own scripted ones (`toolu_T1_a`), every other id is synthetic (the
// session in ctSession's family, a subagent's `a` plus sixteen hex digits, every
// other uuid in ctUUID's form so each parent still names its record), and a path
// under a home directory is /tmp/box/ and its last element. A line over 2 KiB
// that is no user record is ABRIDGED: each string over 256 bytes is "(abridged)"
// and, in the harness's own bookkeeping, each list that long is empty, so keys
// and types stay; the reading reads such a line's head alone. A user record is
// never abridged.
var hsFixtures = map[string]string{
	"2.1.310/T1":  "probe T, T1 (a tree, clean end): its archive",
	"2.1.322/T1":  "probe T, T1 (a tree, clean end): its archive",
	"2.1.310/T2":  "probe T, T2 (a tree, killed once both results were on disk): its archive",
	"2.1.322/T2":  "probe T, T2 (a tree, killed once both results were on disk): its archive",
	"2.1.310/T3":  "probe T, T3 (a tree with one call open at the kill): its archive",
	"2.1.322/T3":  "probe T, T3 (a tree with one call open at the kill): its archive",
	"2.1.310/T4":  "probe T, T4 (a tree past the 5 MiB load path, 6,232,051 bytes before abridging): its archive",
	"2.1.322/T4":  "probe T, T4 (a tree past the 5 MiB load path, 6,232,304 bytes before abridging): its archive",
	"2.1.310/T5":  "probe T, T5 (an Agent answer in a tree): its archive, its subagent's thread and metadata",
	"2.1.322/T5":  "probe T, T5 (an Agent answer in a tree): its archive, its subagent's thread and metadata",
	"2.1.310/s1":  "probe S, s1: the main thread just after /compact, untrimmed, its boundary at line 58",
	"2.1.322/s1":  "probe S, s1: the main thread just after /compact, untrimmed, its boundary at line 60",
	"2.1.310/s2":  "probe S, s2: the main thread just after /compact and its trim, the preserved head first",
	"2.1.322/s2":  "probe S, s2: the main thread just after /compact and its trim, the preserved head first",
	"2.1.310/EK":  "probe P2(e), EK: the main thread after a resume wrote its interrupted call's result",
	"2.1.310/ET0": "probe P2(e), ET0: the main thread of a harness stopped by SIGTERM in a shell call",
	"2.1.310/pBa": "probe pB, (a): a call the harness gave up on at 60 s, which the bridge logged a success " +
		"(`slow a finished its work`)",
	"2.1.310/pBc": "probe pB, (c): a call the bridge answered in time and logged a success " +
		"(`slow c finished its work`)",
	"2.1.310/T10": "probe T, T10 (a background Agent answered by its task notification): its archive, its " +
		"subagent's thread and metadata",
	"2.1.322/T10": "probe T, T10 (a background Agent answered by its task notification, which an isMeta " +
		"turnCompanion record precedes): its archive, its subagent's thread and metadata",
	"2.1.310/G": "probe P2(g): a foreground subagent answered by its call's own result (a...02) and a " +
		"background one by its task notification (a...01), each with its thread and metadata",
	"2.1.310/H": "probe P2(h): P2(g) with two calls at once in each subagent, which leave a tree in its thread, " +
		"and a ToolSearch whose result is a list of tool references",
	"2.1.310/C": "probe P2(c): a background subagent answered by its task notification, beside a shell call of " +
		"the main thread's",
	"2.1.310/DC0": "probe P2(dc): a foreground subagent answered by its call's own result",
	"2.1.310/DCC": "probe P2(dc), its box's copy: the subagent's thread cut back before its last reply, the " +
		"session resumed and the subagent sent a message, answered by its notification",
	"2.1.310/DW": "probe P2(d), its box's copy: a foreground subagent cut off in a call, the session resumed (the " +
		"Agent call's result the resume's own) and the subagent sent a message, answered by its notification",
	"2.1.310/DT": "probe P2(d), its box's copy: as DW, but the torn last piece of the subagent's thread was left " +
		"uncut, and its next record written onto it (line 25)",
	"2.1.330/n1": "row n1: a note handed at a batch and one at Stop, each answered, then resumed with both in " +
		"its first request: the main thread before the resume",
	"2.1.330/t1": "row t1: a note handed at a batch, the next request never answered and the harness stopped, " +
		"then resumed with the note in its first request: the main thread before the resume",
}

// hsFixture is the fixture of release and name ([hsFixtures]) as its archive
// held it, tarred and gzipped in name order, and the session it holds; edits
// are pairs of a text and its replacement, each found in its main thread.
func hsFixture(t *testing.T, release, name string, edits ...[2]string) (archive []byte, session string) {
	t.Helper()
	if _, cut := hsFixtures[release+"/"+name]; !cut {
		t.Fatalf("no fixture %s/%s is described in hsFixtures", release, name)
	}
	root := filepath.Join("testdata", "claude", release, "holdings", name)
	files := map[string]string{}
	err := filepath.WalkDir(root, func(file string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, file)
		if err != nil {
			return err
		}
		member := filepath.ToSlash(rel)
		files[member] = string(raw)
		if s, ok := strings.CutPrefix(member, claudeProjectPrefix); ok && !strings.Contains(s, "/") {
			session = strings.TrimSuffix(s, ".jsonl")
		}
		return nil
	})
	if err != nil || session == "" {
		t.Fatalf("fixture %s/%s: %v, its session %q", release, name, err, session)
	}
	main, _ := claudeMainThread(session)
	for _, edit := range edits {
		if !strings.Contains(files[main], edit[0]) {
			t.Fatalf("fixture %s/%s holds no %q", release, name, edit[0])
		}
		files[main] = strings.ReplaceAll(files[main], edit[0], edit[1])
	}
	return tarOf(t, files), session
}

// hsMain is ctSession's main thread, as its archive names it.
var hsMain = claudeProjectPrefix + ctSession + ".jsonl"

// hsBounds is what a reading of a kept session is held to.
func hsBounds() scrubBounds { return plainBounds(scrubPlainMax) }

// hsLogged is a call the log kept as a success that answered output.
func hsLogged(id, output string) sandbox.LoggedCall {
	return sandbox.LoggedCall{ID: id, Asked: true, Output: output}
}

// hsResumer is the release that continues every session here: one whose
// resume [threadChain] models ([claudeResumeRead]).
const hsResumer = "2.1.322"

// hsAsk asks about calls, for a session [hsResumer] continues.
func hsAsk(calls ...sandbox.LoggedCall) sandbox.HoldingsAsk {
	return sandbox.HoldingsAsk{Calls: calls, Resumer: hsResumer}
}

// hsRead is a reading of archive, session's, for ask.
func hsRead(t *testing.T, archive []byte, session string, ask sandbox.HoldingsAsk) (sandbox.SessionHoldings, error) {
	t.Helper()
	return hsReadFrom(t.Context(), bytes.NewReader(archive), session, ask, hsBounds())
}

// hsReadFrom is a reading of what body streams, session's archive, for ask,
// within bounds.
func hsReadFrom(ctx context.Context, body io.Reader, session string, ask sandbox.HoldingsAsk,
	bounds scrubBounds) (sandbox.SessionHoldings, error) {
	return ClaudeCode{}.readHoldings(ctx, body, sandbox.SessionArchive{Harness: ClaudeCodeName, SessionID: session,
		Format: claudeArchiveFormat, Version: holdingsWriterFloor}, ask, bounds)
}

// hsUse is a model's call of id, an assistant record's content.
func hsUse(id string) string {
	return `[{"type":"tool_use","id":"` + id + `","name":"mcp__crewlet__slow","input":{"tag":"` + id + `"}}]`
}

// hsBlock is text as one text block, as an MCP tool's result holds it.
func hsBlock(t *testing.T, text string) string {
	t.Helper()
	return `[{"type":"text","text":` + jsonString(t, text) + `}]`
}

// hsAnswer is the content of a user record answering id with content, a JSON
// value.
func hsAnswer(id, content string) string {
	return `[{"tool_use_id":"` + id + `","type":"tool_result","content":` + content + `}]`
}

// hsCall is a call of id chained to parent, uuid n, and its result, uuid n+1,
// whose content is the JSON value content.
func hsCall(parent string, n int, id, content string) []string {
	return []string{
		ctAssistant(parent, ctUUID(n), hsUse(id)),
		ctResult(ctUUID(n), ctUUID(n+1), hsAnswer(id, content), content),
	}
}

// hsThread is a main thread of its brief, uuid 1, and lines after it.
func hsThread(lines ...string) string {
	return ctThread(append([]string{ctBrief(ctUUID(1))}, lines...)...)
}

// hsArchive is ctSession's archive holding thread as its main thread.
func hsArchive(t *testing.T, thread string) []byte {
	t.Helper()
	return tarOf(t, map[string]string{hsMain: thread})
}

// hsNoReading fails t unless got and err are no reading, err the reading's own
// words.
func hsNoReading(t *testing.T, got sandbox.SessionHoldings, err error) {
	t.Helper()
	var own *holdingsError
	if got.Read || got.Kind != "" || got.Calls != nil || !errors.As(err, &own) {
		t.Fatalf("a reading = %+v, %v; want no reading, in the reading's own words", got, err)
	}
}

// hsReadWhole fails t unless got and err are a reading.
func hsReadWhole(t *testing.T, got sandbox.SessionHoldings, err error) {
	t.Helper()
	if err != nil || !got.Read || got.Kind != sandbox.HoldingsByID {
		t.Fatalf("a reading = %+v, %v; want a reading of the records filed by id", got, err)
	}
}

// A CALL IS HELD ONLY THROUGH A WHOLE RESULT LINE: a result written as a last
// piece with no break, or in the NULs GNU tar fills a member out with once it
// shrank as it was packed, is no line, so its call holds a use and no result,
// and is told. The same result ended by its break holds it.
//
// Mutation: hand the walk a member's last piece as a line, and the torn result
// holds its call.
func TestACallIsHeldOnlyThroughAWholeResultLine(t *testing.T) {
	t.Parallel()
	const id, out = "toolu_whole", "whole done"
	call := hsLogged(id, out)
	pair := hsCall(ctUUID(1), 2, id, hsBlock(t, out))
	use := hsThread(pair[0])
	for name, tc := range map[string]struct {
		thread string
		held   bool
	}{
		"its line whole":          {hsThread(pair...), true},
		"its line torn":           {use + pair[1], false},
		"its line in NUL padding": {use + pair[1] + strings.Repeat("\x00", 512), false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := hsRead(t, hsArchive(t, tc.thread), ctSession, hsAsk(call))
			hsReadWhole(t, got, err)
			if held, _ := got.Holds(call); held != tc.held {
				t.Errorf("Holds = %t, its records %+v; want %t", held, got.Calls[id], tc.held)
			}
		})
	}
}

// A MAIN-THREAD LINE THAT IS NO RECORD LEAVES THE SESSION UNREAD: a line that is
// not one JSON object, one whose head key is of another type, and a counted
// record whose message does not decode each end the reading with no reading,
// since passing one by would count a thread with a hole as whole. The same
// thread without the line is read and holds its call. A task notification whose
// origin does not decode is no notification: its subagent's leg is unanswered
// and the reading stands. A reply listing a batch's calls in no shape Claude
// Code writes is no reading either ([holdingsWalk.idsFold]).
//
// Mutation: pass a line that is no record by, or read an undecodable counted
// record or batch list past, and the reading stands; take an undecodable origin
// for a non-record line, and the notification's session is unread.
func TestAMainThreadLineThatIsNoRecordLeavesTheSessionUnread(t *testing.T) {
	t.Parallel()
	const id, out = "toolu_record", "record done"
	call := hsLogged(id, out)
	pair := hsCall(ctUUID(1), 2, id, hsBlock(t, out))
	for name, tc := range map[string]struct {
		line string
		read bool
	}{
		"no line between them": {"", true},
		"a line of half a record": {`{"parentUuid":"` + ctUUID(3) + `","isSidechain":false,"type":"user",` +
			`"message":{"role":"user","content":`, false},
		"a line that is a list":          {`[1,2,3]`, false},
		"a head key of another type":     {`{"parentUuid":"` + ctUUID(3) + `","isSidechain":"no","type":"user"}`, false},
		"a counted record's bad content": {ctResult(ctUUID(3), ctUUID(9), `42`, `42`), false},
		"a counted call naming no call": {ctAssistant(ctUUID(3), ctUUID(9),
			`[{"type":"tool_use","id":"","name":"Bash","input":{}}]`), false},
		"a batch's list in no shape Claude Code writes": {ctReplace(t, ctAssistant(ctUUID(3), ctUUID(9),
			`[{"type":"text","text":"done."}]`), `"type":"assistant",`, `"type":"assistant","batchToolUses":[7],`),
			false},
		"a message no reader reads, on an attachment": {`{"parentUuid":"` + ctUUID(3) + `","isSidechain":false,` +
			`"type":"attachment","message":42,"uuid":"` + ctUUID(9) + `","version":"2.1.310"}`, true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			lines := append([]string{}, pair...)
			if tc.line != "" {
				lines = append(lines, tc.line)
			}
			got, err := hsRead(t, hsArchive(t, hsThread(lines...)), ctSession, hsAsk(call))
			if !tc.read {
				hsNoReading(t, got, err)
				return
			}
			hsReadWhole(t, got, err)
			if held, _ := got.Holds(call); !held {
				t.Errorf("Holds = false, its records %+v; want its call held", got.Calls[id])
			}
		})
	}
	t.Run("a notification whose origin does not decode", func(t *testing.T) {
		t.Parallel()
		s := hsBackground(t)
		s.main[3] = ctReplace(t, s.main[3], ctNotificationOrigin, `"origin":"task-notification"`)
		hsReports(t, s, hsUnanswered)
	})
}

// EVERY CHAINED RECORD OF THE MAIN THREAD IS HELD TO THE WRITER'S FLOOR by the
// release it names, not the session's last writer alone: a box continues a
// session an older release wrote, so a session the floor's release last wrote
// can hold records no rule of the reading was measured on. A record of 2.1.305,
// one naming no release or one this code cannot read (two numbers, one past an
// int), or whose `version` is no string, makes the session no reading wherever
// it lies; so does a brief or call's use of 2.1.305 with no `parentUuid`, which
// Claude Code's resume reads as chained. A later writer reads, as does a
// bookkeeping record naming no release or an old one, which no rule reads.
// Mutation: skip the check, and each such session holds its call; read an absent
// `version` as the floor's, or hold only a record with a `parentUuid`, and the
// record naming none is read.
func TestARecordWrittenBelowTheFloorEndsTheReading(t *testing.T) {
	t.Parallel()
	const id, out = "toolu_floor", "floor done"
	call := hsLogged(id, out)
	release := `"version":"2.1.310"`
	brief, pair := ctBrief(ctUUID(1)), hsCall(ctUUID(1), 2, id, hsBlock(t, out))
	note := ctAttachment(ctUUID(3), ctUUID(4), ctTokens, ctTokensRendered)
	writtenBy := func(record, version string) string {
		if version == "" {
			return ctReplace(t, record, ","+release, "")
		}
		return ctReplace(t, record, release, `"version":`+version)
	}
	shape := ctReplace(t, ctRequestShape, release, `"version":"2.1.118"`)
	unparented := func(record, parent string) string { return ctReplace(t, record, parent, "") }
	for name, tc := range map[string]struct {
		lines []string
		read  bool
	}{
		"every record at the floor":       {[]string{brief, pair[0], pair[1], note}, true},
		"every record of a later release": {[]string{writtenBy(brief, `"2.1.322"`), pair[0], pair[1], note}, true},
		"bookkeeping naming no release or an old one": {[]string{ctAtisLatch, shape, brief, pair[0], pair[1]},
			true},
		"the brief written by 2.1.305":      {[]string{writtenBy(brief, `"2.1.305"`), pair[0], pair[1]}, false},
		"the call's use written by 2.1.305": {[]string{brief, writtenBy(pair[0], `"2.1.305"`), pair[1]}, false},
		"the brief written by 2.1.305, with no parentUuid": {[]string{
			unparented(writtenBy(brief, `"2.1.305"`), `"parentUuid":null,`), pair[0], pair[1]}, false},
		"the call's use written by 2.1.305, with no parentUuid": {[]string{brief,
			unparented(writtenBy(pair[0], `"2.1.305"`), `"parentUuid":"`+ctUUID(1)+`",`), pair[1]}, false},
		"its result naming no release": {[]string{brief, pair[0], writtenBy(pair[1], "")}, false},
		"an attachment after it naming no release": {[]string{brief, pair[0], pair[1], writtenBy(note, "")},
			false},
		"its result of two numbers": {[]string{brief, pair[0], writtenBy(pair[1], `"2.1"`)}, false},
		"its result past what an int holds": {[]string{brief, pair[0],
			writtenBy(pair[1], `"2.1.99999999999999999999"`)}, false},
		"its result naming it as a number": {[]string{brief, pair[0], writtenBy(pair[1], `2.1`)}, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := hsRead(t, hsArchive(t, ctThread(tc.lines...)), ctSession, hsAsk(call))
			if !tc.read {
				hsNoReading(t, got, err)
				if !strings.Contains(err.Error(), "of the session's main thread is a record") {
					t.Errorf("no reading, for %q; want it said of the record", err)
				}
				return
			}
			hsReadWhole(t, got, err)
			if held, _ := got.Holds(call); !held {
				t.Errorf("Holds = false, its records %+v; want its call held", got.Calls[id])
			}
		})
	}
}

// EVERY CHAINED RECORD OF A SUBAGENT'S THREAD IS HELD TO THE WRITER'S FLOOR by
// the release it names: one of 2.1.305, or naming none, anywhere in its thread
// (its prompt, its call's use, its call's result, its finished reply) leaves
// the subagent unreported, its thread not whole for that line, and its call
// told under it; the main thread is read, and the session's other facts stand.
// So does its prompt of 2.1.305 carrying no `parentUuid`, a root of its thread
// as Claude Code reads one. A thread whose every record is of a later release
// reports it.
//
// Mutation: hold only the main thread's records to the floor, and the
// subagent is reported and its call held; hold only a record that carries a
// `parentUuid`, and the prompt that carries none is read.
func TestASubagentRecordWrittenBelowTheFloorLeavesItUnreported(t *testing.T) {
	t.Parallel()
	release := `"version":"2.1.310"`
	for name, tc := range map[string]struct {
		line       int
		version    string
		why        string
		unparented bool
	}{
		"its prompt written by 2.1.305": {0, `"version":"2.1.305"`, "line 1 of its thread is a record of", false},
		"its prompt written by 2.1.305, with no parentUuid": {0, `"version":"2.1.305"`,
			"line 1 of its thread is a record of", true},
		"its call's use written by 2.1.305":     {1, `"version":"2.1.305"`, "line 2 of its thread is a record of", false},
		"its call's result naming no release":   {2, "", "line 3 of its thread is a record that names no writer", false},
		"its finished reply naming it oddly":    {3, `"version":7`, "line 4 of its thread is a record that names", false},
		"its finished reply of a later release": {3, `"version":"2.1.322"`, "", false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := hsForeground(t)
			from, to := release, tc.version
			if to == "" {
				from = "," + release
			}
			s.thread[tc.line] = ctReplace(t, s.thread[tc.line], from, to)
			if tc.unparented {
				s.thread[tc.line] = ctReplace(t, s.thread[tc.line], `"parentUuid":null,`, "")
			}
			if tc.why != "" {
				tc.why = hsNotWhole + ": " + tc.why
			}
			hsReports(t, s, tc.why)
		})
	}
}

// A CALL'S ID ON ANY RECORD OF ITS FILE IS ANOTHER CARRIER of it, counted beside
// the call's own: Claude Code's resume walks every user and assistant record
// whatever its `isSidechain` or `agentId`, and its request keeps the first use
// of an id and the first result after it, so a second carrier may take the
// call's place and the call is told. Carriers: a reply of no `isSidechain`, of
// an `agentId` or of no `parentUuid` (a root the walk reaches), and an earlier
// result of no `isSidechain` and another text; a result of an `agentId` that is
// the call's only one still holds it. A user record out of the chain whose call
// another reply made is no reading (tengu_foamy_spring). In A's thread rule 2
// reads only A's own chained records: another is counted and leaves A not
// whole; one out of the chain restarts it ([threadRestarts.See]).
// Mutation: count only own or only chained records, and a carrier leaves its call held.
func TestACallsIDOnAnyRecordOfItsFileIsAnotherCarrier(t *testing.T) {
	t.Parallel()
	const x = "toolu_carried_x"
	callX := hsLogged(x, "ok")
	said := rcSaid("done")
	reply := func(parent, n int, id, content string) string { return rcReply(parent, n, id, n*100, content) }
	results := func(parent, n int, ids ...string) string { return rcResults(parent, n, n*100, ids...) }
	// unsided is line with no `isSidechain`, agented it with subagent
	// ctAgent's `agentId`, and rooted it with no `parentUuid`.
	unsided := func(t *testing.T, line string) string {
		t.Helper()
		return ctReplace(t, line, `"isSidechain":false,`, "")
	}
	agented := func(t *testing.T, line string) string {
		t.Helper()
		return ctReplace(t, line, `"isSidechain":false,`, `"isSidechain":false,"agentId":"`+ctAgent+`",`)
	}
	rooted := func(t *testing.T, line string, parent int) string {
		t.Helper()
		return ctReplace(t, line, `"parentUuid":"`+rcUUID(parent)+`",`, "")
	}
	brief := rcPrompt(0, 1, 0)
	for name, tc := range map[string]struct {
		lines []string
		held  bool
	}{
		// [1, 2, 3, 4]
		"no other carrier": {lines: []string{brief, reply(1, 2, "msg_u", rcCalls(x)), results(2, 3, x),
			reply(3, 4, "msg_after", said)}, held: true},
		// [1, 2, 3, 4, 5]: the request keeps 2's use, and answers it.
		"a reply of no isSidechain calling it first": {lines: []string{brief,
			unsided(t, reply(1, 2, "msg_n", rcCalls(x))), reply(2, 3, "msg_u", rcCalls(x)), results(3, 4, x),
			reply(4, 5, "msg_after", said)}},
		"a reply of an agentId calling it first": {lines: []string{brief,
			agented(t, reply(1, 2, "msg_n", rcCalls(x))), reply(2, 3, "msg_u", rcCalls(x)), results(3, 4, x),
			reply(4, 5, "msg_after", said)}},
		// [2, 3, 4, 5]: 2 roots the walk, a restart.
		"a reply of no parentUuid calling it first": {lines: []string{brief,
			rooted(t, reply(1, 2, "msg_n", rcCalls(x)), 1), reply(2, 3, "msg_u", rcCalls(x)), results(3, 4, x),
			reply(4, 5, "msg_after", said)}},
		// [1, 2, 3, 4, 5]: 3 and 4 fold into one message, 3's block first.
		"a result of no isSidechain before its own, of another text": {lines: []string{brief,
			reply(1, 2, "msg_u", rcCalls(x)), ctReplace(t, unsided(t, results(2, 3, x)), `"content":"ok"`,
				`"content":"not the log's"`), results(2, 4, x), reply(4, 5, "msg_after", said)}},
		// [1, 2, 3, 4]
		"its only result of an agentId": {lines: []string{brief, reply(1, 2, "msg_u", rcCalls(x)),
			agented(t, results(2, 3, x)), reply(3, 4, "msg_after", said)}, held: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := hsRead(t, hsArchive(t, ctThread(tc.lines...)), ctSession, hsAsk(callX))
			hsReadWhole(t, got, err)
			if held, _ := got.Holds(callX); held != tc.held {
				t.Errorf("Holds = %t, its records %+v; want %t", held, got.Calls[x], tc.held)
			}
		})
	}
	const id, out = "toolu_own", "own done"
	call := hsLogged(id, out)
	content := hsAnswer(id, hsBlock(t, out))
	result := ctResult(ctUUID(2), ctUUID(3), content, hsBlock(t, out))
	for name, tc := range map[string]struct {
		record       string
		held, unread bool
	}{
		"a counted result": {record: result, held: true},
		"a request's blob": {record: `{"type":"api-request-blob","sessionId":"` + ctSession + `","hash":"b1",` +
			`"message":{"role":"user","content":` + content + `}}`},
		"a queue operation": {record: `{"type":"queue-operation","operation":"enqueue","sessionId":"` + ctSession +
			`","message":{"role":"user","content":` + content + `}}`},
		"a record out of the chain": {record: ctReplace(t, result, `"parentUuid":"`+ctUUID(2)+`",`, ``),
			unread: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			thread := hsThread(ctAssistant(ctUUID(1), ctUUID(2), hsUse(id)), tc.record)
			got, err := hsRead(t, hsArchive(t, thread), ctSession, hsAsk(call))
			if tc.unread {
				hsNoReading(t, got, err)
				return
			}
			hsReadWhole(t, got, err)
			records := got.Calls[id]
			if held, _ := got.Holds(call); held != tc.held || records.Uses != 1 || (records.Results == 1) != tc.held {
				t.Errorf("Holds = %t, its records %+v; want held %t, with one use", held, records, tc.held)
			}
		})
	}
	own := `"isSidechain":true,"agentId":"` + ctAgent + `"`
	for name, tc := range map[string]struct {
		from, to string
		held     bool
	}{
		"a subagent's own result":              {own, own, true},
		"a subagent's result of no agent":      {own, `"isSidechain":true`, false},
		"a subagent's result of another":       {own, `"isSidechain":true,"agentId":"` + hsOtherAgent + `"`, false},
		"a subagent's result of no sidechain":  {own, `"agentId":"` + ctAgent + `"`, false},
		"a subagent's result, not a sidechain": {own, `"isSidechain":false,"agentId":"` + ctAgent + `"`, false},
		"a subagent's result out of the chain": {`"parentUuid":"` + ctUUID(102) + `",`, ``, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := hsForeground(t)
			s.thread[2] = ctReplace(t, s.thread[2], tc.from, tc.to)
			got := hsSubagentRead(t, s)
			records, report := got.Calls[hsSubCall], got.Subagents[ctAgent]
			held, under := got.Holds(hsLogged(hsSubCall, hsSubOut))
			if held != tc.held || records.Uses != 1 || records.Results != 1 ||
				records.Thread != ctAgent || report.Reported != tc.held ||
				(!tc.held && (under != ctAgent || !strings.Contains(report.Why, hsNotWhole))) {
				t.Errorf("Holds = %t under %q, its records %+v, the subagent %+v; want held %t, with one use and "+
					"one result in its subagent's thread, the subagent reported only where its call is held, and "+
					"else not whole", held, under, records, report, tc.held)
			}
		})
	}
}

// A RESULT BEFORE THE LAST RESTART IS NOT LIVE: what a resumed session is handed
// starts over at a restart, so a result before the main thread's last one is
// told. Measured: s1's compaction (its boundary follows its call's result) and
// s2's trim (it kept none of the job's records); and as both releases' readers
// read them, a microcompaction's boundary, a span replaced, and a root after
// the first (a user record whose parentUuid is absent or empty, as the walk
// reads one). A result between two restarts is before the last; one after the
// last is live, and held.
//
// Mutation: compare a result's line with the thread's FIRST restart, and the
// result between two restarts is live.
func TestAResultBeforeTheLastRestartIsNotLive(t *testing.T) {
	t.Parallel()
	const id, out = "toolu_restart", "restart done"
	call := hsLogged(id, out)
	pair := hsCall(ctUUID(1), 2, id, hsBlock(t, out))
	restart := map[string]string{
		"a compaction's boundary": ctBoundary(ctUUID(4), ctUUID(5), ctUUID(3)) + "\n" +
			ctCompactSummary(ctUUID(4), ctUUID(5)),
		"a microcompaction's boundary":  ctMicrocompactBoundary(ctUUID(3), ctUUID(4)),
		"a span replaced":               ctSpan(ctUUID(3), ctUUID(4), "true"),
		"a later root":                  ctBrief(ctUUID(4)),
		"a later root of no parentUuid": ctReplace(t, ctBrief(ctUUID(4)), `"parentUuid":null,`, ""),
		"a later root whose parentUuid is the empty string": ctReplace(t, ctBrief(ctUUID(4)), `"parentUuid":null`,
			`"parentUuid":""`),
	}
	for name, line := range restart {
		t.Run("after it, "+name, func(t *testing.T) {
			t.Parallel()
			got, err := hsRead(t, hsArchive(t, hsThread(append(pair[:2:2], line)...)), ctSession, hsAsk(call))
			hsReadWhole(t, got, err)
			if records := got.Calls[id]; records.Results != 1 || records.Live {
				t.Errorf("its records %+v; want its one result, not live", records)
			}
		})
	}
	t.Run("between two restarts", func(t *testing.T) {
		t.Parallel()
		thread := hsThread(ctMicrocompactBoundary(ctUUID(1), ctUUID(10)), pair[0], pair[1],
			ctMicrocompactBoundary(ctUUID(3), ctUUID(11)))
		got, err := hsRead(t, hsArchive(t, thread), ctSession, hsAsk(call))
		hsReadWhole(t, got, err)
		if records := got.Calls[id]; records.Results != 1 || records.Live {
			t.Errorf("its records %+v; want its one result, not live", records)
		}
	})
	t.Run("after the last restart", func(t *testing.T) {
		t.Parallel()
		thread := hsThread(ctMicrocompactBoundary(ctUUID(1), ctUUID(10)), ctSpan(ctUUID(10), ctUUID(11), "true"),
			ctAssistant(ctUUID(11), ctUUID(2), hsUse(id)), pair[1])
		got, err := hsRead(t, hsArchive(t, thread), ctSession, hsAsk(call))
		hsReadWhole(t, got, err)
		if held, _ := got.Holds(call); !held {
			t.Errorf("Holds = false, its records %+v; want a result after the last restart held", got.Calls[id])
		}
	})
	for _, release := range []string{"2.1.310", "2.1.322"} {
		t.Run(release+", s1's compaction", func(t *testing.T) {
			t.Parallel()
			s1 := hsLogged("toolu_s1_b1", "s1-b1")
			archive, session := hsFixture(t, release, "s1")
			got, err := hsRead(t, archive, session, hsAsk(s1))
			hsReadWhole(t, got, err)
			records := got.Calls[s1.ID]
			if held, _ := got.Holds(s1); held || records.Uses != 1 || records.Results != 1 || records.Live {
				t.Errorf("Holds = %t, its records %+v; want its use and result, not live, and told", held, records)
			}
		})
		t.Run(release+", s2's trim", func(t *testing.T) {
			t.Parallel()
			s2 := hsLogged("toolu_s2_b1", "s2-b1")
			archive, session := hsFixture(t, release, "s2")
			got, err := hsRead(t, archive, session, hsAsk(s2))
			hsReadWhole(t, got, err)
			if records, named := got.Calls[s2.ID]; named {
				t.Errorf("its records %+v; want none, the trim having kept none of the job's records", records)
			}
		})
	}
}

// A RESULT ITS HARNESS REPLACED IS NOT LIVE: where Claude Code hands its model a
// preview in a result's place, it records the replacement beside the result's
// own record, which keeps the whole output, and a resume hands its model the
// preview again, so a call whose result such a record names is told, wherever
// the record lies; a record replacing another call's result, or naming the call
// in an entry of another kind, leaves it held. In a subagent's thread its own
// record tells its call the same way. A record listing its replacements in no
// shape Claude Code writes is no reading in the main thread, and leaves a
// subagent's thread not whole.
// Mutation: pass every content-replacement record over, or keep a replaced
// result live, and the call is held; read past a record of no shape, and the
// main thread's reading stands.
func TestAResultItsHarnessReplacedIsNotLive(t *testing.T) {
	t.Parallel()
	const id, out = "toolu_replaced", "replaced done"
	call := hsLogged(id, out)
	pair := hsCall(ctUUID(1), 2, id, hsBlock(t, out))
	after := func(record string) []string { return append(slices.Clone(pair), record) }
	for name, tc := range map[string]struct {
		lines []string
		held  bool
	}{
		"no record replaces it":           {pair, true},
		"a record replaces it, after it":  {after(ctReplacement("", ctReplaced(id))), false},
		"a record replaces it, before it": {append([]string{ctReplacement("", ctReplaced(id))}, pair...), false},
		"a record replaces another call":  {after(ctReplacement("", ctReplaced("toolu_other"))), true},
		"a record names it in an entry of another kind": {after(ctReplacement("",
			`{"kind":"image","toolUseId":"`+id+`","replacement":"x"}`)), true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := hsRead(t, hsArchive(t, hsThread(tc.lines...)), ctSession, hsAsk(call))
			hsReadWhole(t, got, err)
			records := got.Calls[id]
			if held, _ := got.Holds(call); held != tc.held || records.Results != 1 || records.Live != tc.held {
				t.Errorf("Holds = %t, its records %+v; want held %t, its one result live %t", held, records,
					tc.held, tc.held)
			}
		})
	}
	t.Run("a record in no shape Claude Code writes", func(t *testing.T) {
		t.Parallel()
		got, err := hsRead(t, hsArchive(t, hsThread(after(ctReplacement("", `"x"`))...)), ctSession, hsAsk(call))
		hsNoReading(t, got, err)
	})
	t.Run("a subagent's record replaces its call", func(t *testing.T) {
		t.Parallel()
		s := hsForeground(t)
		s.thread = append(s.thread, ctReplacement(ctAgent, ctReplaced(hsSubCall)))
		got := hsSubagentRead(t, s)
		held, under := got.Holds(hsLogged(hsSubCall, hsSubOut))
		if records := got.Calls[hsSubCall]; held || under != "" || records.Live || !got.Subagents[ctAgent].Reported {
			t.Errorf("Holds = %t under %q, its records %+v, the subagent %+v; want it told in the main list, its "+
				"result not live, its subagent reported", held, under, records, got.Subagents[ctAgent])
		}
	})
	t.Run("a subagent's record in no shape Claude Code writes", func(t *testing.T) {
		t.Parallel()
		s := hsForeground(t)
		s.thread = append(s.thread, ctReplacement(ctAgent, `"x"`))
		hsReports(t, s, hsNotWhole)
	})
}

// PARALLEL RESULTS AFTER THE LAST RESTART ARE LIVE, off the chain or on it.
// Measured (probe T, 2.1.310 and 2.1.322): parallel calls leave a tree, the
// thread's last record descending from the last result, so the first result
// lies off the chain, and a continuation hands its model each such result all
// the same, with its call, as the resume recovers it: a result whose parent is
// a record of a reply on the chain, its own call's. T1, T2, T4's siblings, T5's
// Agent answer and T4's linear calls past the 5 MiB load path are live, each
// call the bridge answered held; T3's answered call, a sibling the chain skips,
// is held, and its open call, whose result the file never held, is told.
//
// Mutation: hold a result only on the chain its thread's last record descends
// from, and every off-chain sibling is told.
func TestParallelResultsAfterTheLastRestartAreLive(t *testing.T) {
	t.Parallel()
	done := func(tag string) sandbox.LoggedCall { return hsLogged("toolu_"+tag, "crewlet "+tag+" done") }
	for _, release := range []string{"2.1.310", "2.1.322"} {
		for name, calls := range map[string][]sandbox.LoggedCall{
			"T1": {done("T1_a"), done("T1_b")},
			"T2": {done("T2_a"), done("T2_b")},
			"T4": {done("T4_a"), done("T4_b"), done("T4_f1"), done("T4_f2"), done("T4_f3")},
			"T5": {done("T5_m"), done("T5_s1")},
		} {
			t.Run(release+"/"+name, func(t *testing.T) {
				t.Parallel()
				archive, session := hsFixture(t, release, name)
				got, err := hsRead(t, archive, session, hsAsk(calls...))
				hsReadWhole(t, got, err)
				for _, call := range calls {
					if held, _ := got.Holds(call); !held {
						t.Errorf("%s: Holds = false, its records %+v; want it held", call.ID, got.Calls[call.ID])
					}
				}
				if name == "T5" {
					// The Agent answer is no call the bridge logs: what is held
					// to the rule is that its result, off the chain, answers
					// the subagent's leg, whose call is held above.
					if report := got.Subagents["a0000000000000001"]; !report.Reported {
						t.Errorf("the subagent %+v; want it reported, its Agent answer live", report)
					}
				}
			})
		}
		t.Run(release+"/T3", func(t *testing.T) {
			t.Parallel()
			answered, open := done("T3_a"), done("T3_b")
			archive, session := hsFixture(t, release, "T3")
			got, err := hsRead(t, archive, session, hsAsk(answered, open))
			hsReadWhole(t, got, err)
			if held, _ := got.Holds(answered); !held {
				t.Errorf("the answered call: Holds = false, its records %+v; want it held", got.Calls[answered.ID])
			}
			records := got.Calls[open.ID]
			if held, _ := got.Holds(open); held || records.Uses != 1 || records.Results != 0 {
				t.Errorf("the open call: Holds = %t, its records %+v; want its use alone, told", held, records)
			}
		})
	}
}

// A CALL IS HELD ONLY WHERE ITS RESUMED REQUEST PAIRS ITS USE WITH ITS RESULT,
// by the pairing rule ([holdingsPairing]) over the order the resume's model
// hands the main thread's records ([threadChain]): from the use's reply's first
// record to the later of use and result every record is plain (that reply's or a
// results record), the result after that first record, none of the reply's other
// results before it, no compaction boundary at or after it; the use names a
// bridge tool, its id no other call's fold (no reply id:
// [TestACallWhoseUseCarriesNoReplyIDIsTold]). A subagent's thread reads its own
// load's order (newThreadChain(agent)). Told: another reply, prompt or boundary
// in the span, a non-bridge tool, batch or split ids, each mark making a record
// not plain; one the request pairs is an over-tell, knowingly.
// Mutation: tell every call, drop a test, or read a marked record as plain.
func TestACallIsHeldOnlyWhereItsResumedRequestPairsItsUseWithItsResult(t *testing.T) {
	t.Parallel()
	const x, y = "toolu_pair_x", "toolu_pair_y"
	callX, callY := hsLogged(x, "ok"), hsLogged(y, "ok")
	said := rcSaid("done")
	// Each record n is written at n*100 ms, but where a case states a time.
	reply := func(parent, n int, id, content string) string { return rcReply(parent, n, id, n*100, content) }
	results := func(parent, n int, ids ...string) string { return rcResults(parent, n, n*100, ids...) }
	prompt := func(parent, n int) string { return rcPrompt(parent, n, n*100) }
	micro := func(parent, n int) string { return rcSystem(parent, n, n*100, subtypeMicrocompactBoundary, "") }
	brief := rcPrompt(0, 1, 0)
	// use, result and after are the plain case's records ("the use, then its
	// result"); marked is a record carrying key, and synthetic a reply of the
	// model Claude Code writes its own replies under.
	use, result, after := reply(1, 2, "msg_m", rcCalls(x)), results(2, 3, x), reply(3, 4, "msg_after", said)
	marked := func(line, key string) string {
		return ctReplace(t, line, `"isSidechain":false,`, `"isSidechain":false,`+key+`,`)
	}
	synthetic := func(line string) string {
		return ctReplace(t, line, `"model":"claude-sonnet-4-5"`, `"model":"`+claudeSyntheticModel+`"`)
	}
	// calling is use, its call one of tool.
	calling := func(tool string) string {
		return ctReplace(t, use, `"name":"mcp__crewlet__slow"`, `"name":"`+tool+`"`)
	}
	for name, tc := range map[string]struct {
		lines []string
		// held is whether x is held, and y where pair; id is the call asked in
		// x's place, where it is not ""; unread is a thread the model cannot
		// place, its result hung under another record than its call's
		// (whyResultByCall), so no reading.
		held, pair, unread bool
		id                 string
	}{
		// [1, 2, 3, 4]
		"the use, then its result": {lines: []string{brief, reply(1, 2, "msg_m", rcCalls(x)),
			results(2, 3, x), reply(3, 4, "msg_after", said)}, held: true},
		// ADJ1: the result under the reply between, all on the chain.
		"another reply between the use and its result, the result under it": {lines: []string{brief,
			reply(1, 2, "msg_m", rcCalls(x)), reply(2, 3, "msg_c", said), results(3, 4, x),
			reply(4, 5, "msg_after", said)}, unread: true},
		// R1a: the result under the first record of the use's reply, written
		// before another reply, a prompt and the use.
		"the result under its reply's first record, another reply and a prompt before the use": {lines: []string{
			brief, reply(1, 2, "msg_m", said), results(2, 3, x), reply(2, 4, "msg_c", said), prompt(4, 5),
			reply(5, 6, "msg_m", rcCalls(x)), reply(6, 7, "msg_after", said)}, unread: true},
		// R1b, [1, 2, 3, 4, 5, 6, 7]: the use and its result recovered beside
		// 2, 4 of msg_c between them, 6 folding back into 2 past it.
		"another reply between the use and its result, its reply going on after it": {lines: []string{brief,
			reply(1, 2, "msg_m", said), reply(2, 3, "msg_m", rcCalls(x)), reply(2, 4, "msg_c", said),
			results(3, 5, x), reply(4, 6, "msg_m", said), reply(6, 7, "msg_after", said)}},
		// R1c: the result under the reply between, the use its reply's only
		// record.
		"another reply between the use and its result, the use's reply one record": {lines: []string{brief,
			reply(1, 2, "msg_m", rcCalls(x)), reply(2, 3, "msg_c", said), results(3, 4, x),
			reply(4, 5, "msg_after", said)}, unread: true},
		// [1, 2, 3, 4, 5, 6]: the result, recovered beside msg_m, before G.
		"another reply between the use and its result": {lines: []string{brief,
			reply(1, 2, "msg_m", rcCalls(x)), reply(2, 3, "msg_c", said), results(2, 4, x),
			reply(3, 5, "msg_m", said), reply(5, 6, "msg_after", said)}},
		// [1, 2, 3, 4, 5]
		"the use in its reply's second record": {lines: []string{brief,
			reply(1, 2, "msg_m", said), reply(2, 3, "msg_m", rcCalls(x)), results(3, 4, x),
			reply(4, 5, "msg_after", said)}, held: true},
		// [1, 2, 3, 4, 5, 6]: U folds back into F past C.
		"another reply between its reply's first record and the use": {lines: []string{brief,
			reply(1, 2, "msg_m", said), reply(2, 3, "msg_c", said), reply(3, 4, "msg_m", rcCalls(x)),
			results(4, 5, x), reply(5, 6, "msg_after", said)}},
		// [1, 2, 3, 4, 5, 6]: the result, recovered, at its reply's window's end.
		"a prompt between the use's reply's records": {lines: []string{brief,
			reply(1, 2, "msg_m", rcCalls(x)), prompt(2, 3), reply(3, 4, "msg_m", said), results(2, 5, x),
			reply(4, 6, "msg_after", said)}},
		// [1, 2, 3, 5, 6, 7, 8]: 4 retracted, the prompt left alone between F and G.
		"a prompt left between the reply's records by a retraction": {lines: []string{brief,
			reply(1, 2, "msg_m", rcCalls(x)), prompt(2, 3), results(3, 4, "toolu_pair_z"),
			reply(4, 5, "msg_m", said), results(2, 6, x), reply(5, 7, "msg_after", said),
			rcFallback(7, 8, 800, rcQuoted(4))}},
		// OVER-TELL: [1, 2, 3, 4, 5, 6, 7]; the prompt and the results record
		// merge, so the request pairs x.
		"a prompt and another call's result between the reply's records": {lines: []string{brief,
			reply(1, 2, "msg_m", rcCalls(x)), prompt(2, 3), results(3, 4, "toolu_pair_z"),
			reply(4, 5, "msg_m", said), results(2, 6, x), reply(5, 7, "msg_after", said)}},
		// [1, 2, 3, 4, 5, 6]
		"another call's result between the reply's records": {lines: []string{brief,
			reply(1, 2, "msg_m", rcCalls(x)), results(2, 3, "toolu_pair_z"), reply(3, 4, "msg_m", said),
			results(2, 5, x), reply(4, 6, "msg_after", said)}, held: true},
		// [1, 2, 3, 4, 5, 6, 7]
		"a microcompaction's boundary and another reply before the use": {lines: []string{brief,
			reply(1, 2, "msg_m", said), micro(2, 3), reply(3, 4, "msg_c", said), reply(4, 5, "msg_m", rcCalls(x)),
			results(5, 6, x), reply(6, 7, "msg_after", said)}},
		// OVER-TELL: [1, 2, 3, 4, 5, 6]; the fold skips the boundary.
		"a microcompaction's boundary before the use": {lines: []string{brief,
			reply(1, 2, "msg_m", said), micro(2, 3), reply(3, 4, "msg_m", rcCalls(x)), results(4, 5, x),
			reply(5, 6, "msg_after", said)}},
		// [1, 2, 3, 4, 5, 6]
		"a microcompaction's boundary before the use's reply": {lines: []string{brief, micro(1, 2),
			reply(2, 3, "msg_m", said), reply(3, 4, "msg_m", rcCalls(x)), results(4, 5, x),
			reply(5, 6, "msg_after", said)}, held: true},
		// OVER-TELL: [1, 2, 3, 4, 5, 6]; B folds into Dt across R and C.
		"another reply between its reply's first record and the use, the result before it": {lines: []string{
			brief, reply(1, 2, "msg_m", said), results(5, 3, x), reply(2, 4, "msg_c", said),
			reply(4, 5, "msg_m", rcCalls(x)), reply(5, 6, "msg_after", said)}},
		// OVER-TELL: [1, 2, 3, 4, 5, 6]
		"a prompt between its reply's first record and the use, the result before it": {lines: []string{
			brief, reply(1, 2, "msg_m", said), results(5, 3, x), prompt(2, 4),
			reply(4, 5, "msg_m", rcCalls(x)), reply(5, 6, "msg_after", said)}},
		// [1, 2, 3, 4, 5]
		"another record of the use's reply between it and its result": {lines: []string{brief,
			reply(1, 2, "msg_m", rcCalls(x)), reply(2, 3, "msg_m", said), results(2, 4, x),
			reply(3, 5, "msg_after", said)}, held: true},
		// [1, 2, 3, 4, 5]
		"the result before the use, after its reply's first record": {lines: []string{brief,
			reply(1, 2, "msg_m", said), results(4, 3, x), reply(2, 4, "msg_m", rcCalls(x)),
			reply(4, 5, "msg_after", said)}, held: true},
		// [1, 3, 2, 4]: a recovered record is placed after its reply's first
		// record on the walk, wherever it is written.
		"the result written before the use's reply": {lines: []string{brief, results(3, 2, x),
			reply(1, 3, "msg_m", rcCalls(x)), reply(3, 4, "msg_after", said)}, held: true},
		// [1, 2, 3, 4, 5]
		"a record of the use's reply after its result": {lines: []string{brief, reply(1, 2, "msg_m", rcCalls(x)),
			results(2, 3, x), reply(3, 4, "msg_m", said), reply(4, 5, "msg_after", said)}, held: true},
		// [1, 3, 2, 4]: 2's parent names no record, and the walk takes 3,
		// the latest record no later than it.
		"the result placed before the use by the walk's fallback": {lines: []string{brief,
			rcReply(rcNowhere, 2, "msg_m", 1000, rcCalls(x)), rcResults(2, 3, 500, x),
			rcReply(2, 4, "msg_after", 2000, said)}},
		// [1, 2, 3, 4]: the fallback takes 1, and 3 is recovered after 2.
		"the result stamped after the walk's fallback": {lines: []string{brief,
			rcReply(rcNowhere, 2, "msg_m", 1000, rcCalls(x)), rcResults(2, 3, 1500, x),
			rcReply(2, 4, "msg_after", 2000, said)}, held: true},
		// [1, 2, 3, 4]
		"its result marked not meta": {lines: []string{brief, reply(1, 2, "msg_m", rcCalls(x)),
			ctReplace(t, results(2, 3, x), `"sourceToolAssistantUUID"`, `"isMeta":false,"sourceToolAssistantUUID"`),
			reply(3, 4, "msg_after", said)}, held: true},
		// OVER-TELL: [1, 2, 3, 4]; a meta mark changes nothing in the fold.
		"its result marked meta": {lines: []string{brief, reply(1, 2, "msg_m", rcCalls(x)),
			ctReplace(t, results(2, 3, x), `"sourceToolAssistantUUID"`, `"isMeta":true,"sourceToolAssistantUUID"`),
			reply(3, 4, "msg_after", said)}},
		// [1, 2, 4, 3, 5, 6, 7]: 3 recovered after 4, the reply's first record.
		"a result of another of its reply's calls written before the reply": {lines: []string{brief,
			reply(1, 2, "msg_q", said), results(4, 3, y), reply(2, 4, "msg_m", rcCalls(y)),
			reply(4, 5, "msg_m", rcCalls(x)), results(5, 6, x), reply(6, 7, "msg_after", said)}, held: true},
		// [1, 3, 2, 4, 5, 6]: 2's parent names no record, and the walk takes 3,
		// the latest record no later than it, before the reply.
		"a result of another of its reply's calls placed before the reply by the walk's fallback": {lines: []string{
			brief, rcReply(rcNowhere, 2, "msg_m", 1000, rcCalls(y)), rcResults(2, 3, 500, y),
			rcReply(2, 4, "msg_m", 1100, rcCalls(x)), rcResults(4, 5, 1200, x),
			rcReply(5, 6, "msg_after", 2000, said)}},
		// [1, 2, 3, 4, 5]: the boundary under the use, the result recovered
		// beside it; the relaunch's first request cuts there.
		"a compaction's boundary after the use": {lines: []string{brief, reply(1, 2, "msg_m", rcCalls(x)),
			rcSystem(2, 3, 300, subtypeCompactBoundary, ""), results(2, 4, x), reply(3, 5, "msg_after", said)}},
		// [1, 2, 3, 4], each a call of no tool of the bridge's: Claude Code's
		// own, which a context hint clears; its memory server's, which it
		// withholds; its Poll, which its code mode folds; and a server's whose
		// name only begins with the bridge's.
		"its use a call of Claude Code's own tool": {lines: []string{brief, calling("Bash"), result, after}},
		"its use a call of Claude Code's memory server": {lines: []string{brief,
			calling("mcp__memory__memory_view"), result, after}},
		"its use a Poll call": {lines: []string{brief, calling("Poll"), result, after}},
		"its use a call of a server named past the bridge's": {lines: []string{brief,
			calling("mcp__crewletx__slow"), result, after}},
		"a batch naming the call": {lines: []string{brief, reply(1, 2, "msg_m", rcCalls(x)), results(2, 3, x),
			ctReplace(t, reply(3, 4, "msg_after", said), `"type":"assistant",`,
				`"type":"assistant","batchToolUses":[{"id":"`+x+`","name":"Bash"}],`)}},
		"its id split from another call's": {id: "toolu_pair_1", lines: []string{brief,
			reply(1, 2, "msg_q", rcCalls("toolu_pair")), results(2, 3, "toolu_pair"),
			reply(3, 4, "msg_m", rcCalls("toolu_pair_1")), results(4, 5, "toolu_pair_1"),
			reply(5, 6, "msg_after", said)}},
		"its id of a split one's form, no call of the id it reads as split from": {id: "toolu_pair_1",
			lines: []string{brief, reply(1, 2, "msg_m", rcCalls("toolu_pair_1")), results(2, 3, "toolu_pair_1"),
				reply(3, 4, "msg_after", said)}, held: true},
		"a result split from its id": {lines: []string{brief, reply(1, 2, "msg_m", rcCalls(x)), results(2, 3, x),
			results(3, 4, x+"_1"), reply(4, 5, "msg_after", said)}},
		// [1, 4, 5]: the attempt, 2 and 3, off the conversation.
		"a withdrawn attempt left in the file": {lines: []string{brief, reply(1, 2, "msg_m", rcCalls(x)),
			results(2, 3, x), reply(1, 4, "msg_after", said)}},
		// [1, 2, 4, 5, 6]: the retraction filter takes 3, the use, alone.
		"the use's record retracted, its reply's first record handed": {lines: []string{brief,
			reply(1, 2, "msg_m", said), reply(2, 3, "msg_m", rcCalls(x)), results(3, 4, x),
			reply(4, 5, "msg_after", said), rcFallback(5, 6, 600, rcQuoted(3))}},
		// [1, 2, 3, 4, 5]: 3 recovered beside msg_m.
		"two calls of one reply, each with its own result": {lines: []string{brief,
			reply(1, 2, "msg_m", rcCalls(x, y)), results(2, 3, x), results(2, 4, y),
			reply(4, 5, "msg_after", said)}, held: true, pair: true},
		// [1, 2, 3, 4], each mark on the use's record U or the result's R.
		"its use's record virtual": {lines: []string{brief, marked(use, `"isVirtual":true`), result,
			after}},
		"its use the harness's own reply to a failed request": {lines: []string{brief,
			marked(synthetic(use), `"isApiErrorMessage":true`), result, after}},
		// OVER-TELL: A8 skips an API error of the model "<synthetic>" alone.
		"its use marked an API error alone": {lines: []string{brief, marked(use, `"isApiErrorMessage":true`),
			result, after}},
		// OVER-TELL, as the case before.
		"its use of the model \"<synthetic>\" alone": {lines: []string{brief, synthetic(use), result, after}},
		// OVER-TELL: GWi folds only the batch's split ids, and x is none.
		"its use's record listing a batch of another call": {lines: []string{brief,
			marked(use, `"batchToolUses":[{"id":"toolu_pair_other","name":"Bash"}]`), result, after}},
		"its result's record virtual": {lines: []string{brief, use, marked(result, `"isVirtual":true`),
			after}},
		"its result's record stamped by a shutdown": {lines: []string{brief, use,
			marked(result, `"interruptedByShutdown":true`), after}},
		"its result's record a compaction's summary": {lines: []string{brief, use,
			marked(result, `"isCompactSummary":true`), after}},
		"its result's record delivering a task": {lines: []string{brief, use,
			marked(result, `"taskDelivery":{}`), after}},
		"its result's record of a voice call's origin": {lines: []string{brief, use,
			marked(result, `"origin":{"kind":"voice-call"}`), after}},
		"its result's record of a task notification's origin": {lines: []string{brief, use,
			marked(result, `"origin":{"kind":"task-notification"}`), after}},
		"its result's record of an origin of no kind": {lines: []string{brief, use,
			marked(result, `"origin":{"producer":"session-task"}`), after}, held: true},
		// OVER-TELL: the request reads `origin?.kind`, and a string has none.
		"its result's record of an origin that does not decode": {lines: []string{brief, use,
			marked(result, `"origin":"voice-call"`), after}},
		// OVER-TELL: the request reads `isMeta===!0`.
		"its result's record of a meta mark that does not decode": {lines: []string{brief, use,
			marked(result, `"isMeta":"true"`), after}},
		// [1, 2, 3, 4, 5, 6]: the record between answers a project's read.
		"a results record answering an id Claude Code writes between the reply's records": {lines: []string{
			brief, reply(1, 2, "msg_m", rcCalls(x)), results(2, 3, "projectread_1"), reply(3, 4, "msg_m", said),
			results(2, 5, x), reply(4, 6, "msg_after", said)}},
		// [1, 2, 3, 4]: the call under an id Claude Code writes of its own.
		"its id one Claude Code writes of its own": {id: "poll_pair", lines: []string{brief,
			reply(1, 2, "msg_m", rcCalls("poll_pair")), results(2, 3, "poll_pair"),
			reply(3, 4, "msg_after", said)}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ask := []sandbox.LoggedCall{callX}
			if tc.id != "" {
				ask = []sandbox.LoggedCall{hsLogged(tc.id, "ok")}
			}
			if tc.pair {
				ask = append(ask, callY)
			}
			got, err := hsRead(t, hsArchive(t, ctThread(tc.lines...)), ctSession, hsAsk(ask...))
			if tc.unread {
				hsNoReading(t, got, err)
				if !strings.Contains(err.Error(), string(whyResultByCall)) {
					t.Errorf("no reading for %v; want it the model's %s", err, whyResultByCall)
				}
				return
			}
			hsReadWhole(t, got, err)
			for _, call := range ask {
				if held, _ := got.Holds(call); held != tc.held {
					t.Errorf("%s: Holds = %t, its records %+v; want %t", call.ID, held, got.Calls[call.ID], tc.held)
				}
			}
		})
	}
	// ON A COMPACTED MAIN THREAD, by the same rule over the order the resume
	// hands once it relinked what the last compaction kept under its anchor
	// and took its leaf from the set of the relinked parents ([threadChain],
	// THE RELINK and THE LEAF SET), each thread one of the facility's own
	// cases, its call x1 asked, which its records answer "ok". Its call is
	// written after the boundary, so the last restart does not tell it.
	callK := hsLogged("x1", "ok")
	keeping := rcKeeping(4, 1, 2)
	// call is x1's use, its result and a reply after them, under the summary.
	call := []string{rcReply(4, 5, "msg_x", 3000, rcCalls("x1")), rcResults(5, 6, 4000, "x1"),
		rcReply(6, 7, "msg_after", 5000, rcSaid("after"))}
	for name, tc := range map[string]struct {
		lines []string
		// held is whether x1 is held; unread is no reading, the model unable
		// to place the thread (whyResultByCall).
		held, unread bool
	}{
		"compacted, the summary the anchor of the call after it": {lines: rcCompacted(keeping, call...),
			held: true},
		"compacted, F1n: the summary the anchor, an attachment its other child": {lines: rcCompacted(keeping,
			rcAttach(4, 5, 2500, rcNote), rcReply(5, 6, "msg_x", 3000, rcCalls("x1")), rcResults(6, 7, 4000, "x1"),
			rcReply(7, 8, "msg_after", 5000, rcSaid("after"))), held: true},
		"compacted, a boundary keeping records the thread does not hold": {lines: []string{
			rcBoundary(1, 0, rcKeeping(3, 98, 99)), rcSummary(1, 2, 100), rcReply(2, 3, "msg_x", 200, rcCalls("x1")),
			rcResults(3, 4, 300, "x1"), rcReply(4, 5, "msg_after", 400, rcSaid("after"))}, held: true},
		"compacted, F1a: the anchor makes the call, kept by a list and a segment": {lines: rcCompacted(
			rcKeeping(5, 1, 2), call...), unread: true},
		"compacted, F1b: the anchor makes the call, kept by a segment": {lines: rcCompacted(rcSegment(5, 1, 2),
			call...), unread: true},
		"compacted, F1c: the anchor makes the call and is the checkpoint": {lines: rcCompacted(rcKeeping(5, 1, 2),
			append(slices.Clone(call), rcCheckpoint(5))...), unread: true},
		"compacted, F1d: the anchor an attachment between the call and its result": {lines: rcCompacted(
			rcKeeping(6, 1, 2), rcReply(4, 5, "msg_x", 3000, rcCalls("x1")), rcAttach(5, 6, 3500, rcNote),
			rcResults(6, 7, 4000, "x1"), rcReply(7, 8, "msg_after", 5000, rcSaid("after"))), unread: true},
		"compacted, F1e: the anchor the first record of two calls at once": {lines: rcCompacted(
			rcKeeping(5, 1, 2), rcReply(4, 5, "msg_p", 3000, rcCalls("x1")), rcReply(5, 6, "msg_p", 3001,
				rcCalls("y1")), rcResults(5, 7, 4000, "x1"), rcResults(6, 8, 4100, "y1"),
			rcReply(8, 9, "msg_after", 5000, rcSaid("after"))), unread: true},
		"compacted, F1f: the anchor makes the call, its result under the reply's other record": {lines: rcCompacted(
			rcKeeping(5, 1, 2), rcReply(4, 5, "msg_p", 3000, rcCalls("x1")), rcReply(5, 6, "msg_p", 3001,
				rcCalls("y1")), rcResults(6, 7, 4000, "x1"), rcResults(6, 8, 4100, "y1"),
			rcReply(8, 9, "msg_after", 5000, rcSaid("after"))), unread: true},
		"compacted, I3a: a checkpoint no turn names, the call under its attachment": {lines: rcCompacted(keeping,
			rcReply(4, 5, "msg_before", 3000, rcSaid("before")), rcCheckpoint(5), rcAttach(5, 7, 4000, rcNote),
			rcPrompt(7, 8, 5000), rcReply(8, 9, "msg_x", 6000, rcCalls("x1")), rcResults(9, 10, 7000, "x1")),
			held: true},
		"compacted, I3b: a checkpoint a terminal attachment puts in the set": {lines: rcCompacted(keeping,
			rcReply(4, 5, "msg_before", 3000, rcSaid("before")), rcCheckpoint(5), rcAttach(5, 7, 4000, rcNote),
			rcAttach(5, 8, 4100, rcNote), rcPrompt(8, 9, 5000), rcReply(9, 10, "msg_x", 6000, rcCalls("x1")),
			rcResults(10, 11, 7000, "x1"))},
		"compacted, I3c: a checkpoint a terminal stop hook's summary puts in the set": {lines: rcCompacted(keeping,
			rcReply(4, 5, "msg_before", 3000, rcSaid("before")), rcCheckpoint(5), rcStop(5, 7, 4000),
			rcAttach(5, 8, 4100, rcNote), rcPrompt(8, 9, 5000), rcReply(9, 10, "msg_x", 6000, rcCalls("x1")),
			rcResults(10, 11, 7000, "x1"))},
		"compacted, R3a: I3b relinked by its segment alone": {lines: rcCompacted(rcSegment(4, 1, 2),
			rcReply(4, 5, "msg_before", 3000, rcSaid("before")), rcCheckpoint(5), rcAttach(5, 7, 4000, rcNote),
			rcAttach(5, 8, 4100, rcNote), rcPrompt(8, 9, 5000), rcReply(9, 10, "msg_x", 6000, rcCalls("x1")),
			rcResults(10, 11, 7000, "x1"))},
		"compacted, I3d: I3b where the resume relinks nothing": {lines: []string{
			rcBoundary(1, 0, rcKeeping(3, 98, 99)), rcSummary(1, 2, 100), rcReply(2, 3, "msg_before", 200,
				rcSaid("before")), rcCheckpoint(3), rcAttach(3, 5, 300, rcNote), rcAttach(3, 6, 400, rcNote),
			rcPrompt(6, 7, 500), rcReply(7, 8, "msg_x", 600, rcCalls("x1")), rcResults(8, 9, 700, "x1")},
			held: true},
		"compacted, I3e: the call before a checkpoint no turn names": {lines: rcCompacted(keeping,
			rcReply(4, 5, "msg_x", 3000, rcCalls("x1")), rcResults(5, 6, 4000, "x1"),
			rcReply(6, 7, "msg_after", 5000, rcSaid("after")), rcCheckpoint(7), rcAttach(7, 9, 6000, rcNote),
			rcPrompt(9, 10, 7000), rcReply(10, 11, "msg_last", 8000, rcSaid("last"))), held: true},
		"compacted, I3f: the call before a checkpoint in the set": {lines: rcCompacted(keeping,
			rcReply(4, 5, "msg_x", 3000, rcCalls("x1")), rcResults(5, 6, 4000, "x1"),
			rcReply(6, 7, "msg_after", 5000, rcSaid("after")), rcCheckpoint(7), rcAttach(7, 9, 6000, rcNote),
			rcAttach(7, 10, 6100, rcNote), rcPrompt(10, 11, 7000), rcReply(11, 12, "msg_last", 8000,
				rcSaid("last"))), held: true},
		"compacted, I3g: the anchor the checkpoint": {lines: rcCompacted(keeping, rcReply(4, 5, "msg_before", 3000,
			rcSaid("before")), rcCheckpoint(4), rcPrompt(5, 7, 4000), rcReply(7, 8, "msg_x", 5000, rcCalls("x1")),
			rcResults(8, 9, 6000, "x1")), held: true},
		"compacted, I3h: a call's result the checkpoint, the call under its reminder": {lines: rcCompacted(keeping,
			rcReply(4, 5, "msg_y", 3000, rcCalls("y1")), rcResults(5, 6, 4000, "y1"), rcCheckpoint(6),
			rcAttach(6, 8, 5000, rcNote), rcReply(8, 9, "msg_x", 6000, rcCalls("x1")), rcResults(9, 10, 7000, "x1")),
			held: true},
		"compacted, I3i: I3h under T9media's anchor": {lines: rcCompacted(keeping, rcAttach(4, 5, 2500, rcNote),
			rcReply(5, 6, "msg_y", 3000, rcCalls("y1")), rcResults(6, 7, 4000, "y1"), rcCheckpoint(7),
			rcAttach(7, 9, 5000, rcNote), rcReply(9, 10, "msg_x", 6000, rcCalls("x1")), rcResults(10, 11, 7000, "x1")),
			held: true},
		"compacted, R2a: a checkpoint a summary and an attachment put in the set": {lines: []string{
			rcReply(rcNowhere, 1, "msg_kept", 0, rcSaid("kept")), rcBoundary(2, 1000, rcKeeping(3, 1)),
			rcSummary(2, 3, 1100), rcReply(3, 4, "msg_before", 2000, rcSaid("before")), rcCheckpoint(4),
			rcStop(4, 6, 3000), rcAttach(4, 7, 3100, rcNote), rcPrompt(7, 8, 4000),
			rcReply(8, 9, "msg_x", 5000, rcCalls("x1")), rcResults(9, 10, 6000, "x1")}},
		"compacted, R2b: the call right under the checkpoint's summary": {lines: []string{
			rcReply(rcNowhere, 1, "msg_kept", 0, rcSaid("kept")), rcBoundary(2, 1000, rcKeeping(3, 1)),
			rcSummary(2, 3, 1100), rcReply(3, 4, "msg_before", 2000, rcSaid("before")), rcStop(4, 5, 3000),
			rcCheckpoint(5), rcReply(5, 7, "msg_x", 4000, rcCalls("x1")), rcResults(7, 8, 5000, "x1")}, held: true},
		"compacted, R2c: the call under an attachment under the checkpoint's summary": {lines: []string{
			rcReply(rcNowhere, 1, "msg_kept", 0, rcSaid("kept")), rcBoundary(2, 1000, rcKeeping(3, 1)),
			rcSummary(2, 3, 1100), rcReply(3, 4, "msg_before", 2000, rcSaid("before")), rcStop(4, 5, 3000),
			rcCheckpoint(5), rcAttach(5, 7, 3500, rcNote), rcPrompt(7, 8, 4000),
			rcReply(8, 9, "msg_x", 5000, rcCalls("x1")), rcResults(9, 10, 6000, "x1")}, held: true},
		"compacted, S1: the checkpoint a stop hook's summary the call hangs off": {lines: rcCompacted(keeping,
			rcReply(4, 5, "msg_before", 3000, rcSaid("before")), rcStop(5, 6, 3500), rcCheckpoint(6),
			rcPrompt(6, 8, 4000), rcReply(8, 9, "msg_x", 5000, rcCalls("x1")), rcResults(9, 10, 6000, "x1")),
			held: true},
		"compacted, S2: the checkpoint the boundary": {lines: rcCompacted(keeping, rcCheckpoint(3),
			rcReply(4, 6, "msg_x", 3000, rcCalls("x1")), rcResults(6, 7, 4000, "x1"),
			rcReply(7, 8, "msg_after", 5000, rcSaid("after"))), held: true},
		"compacted, S3: a stop hook's summary the last record": {lines: rcCompacted(keeping,
			rcReply(4, 5, "msg_x", 3000, rcCalls("x1")), rcResults(5, 6, 4000, "x1"), rcStop(6, 7, 5000)),
			held: true},
		"compacted, B1: what it kept named by an earlier boundary alone": {lines: []string{rcPrompt(0, 1, 0),
			rcReply(1, 2, "msg_kept", 1000, rcSaid("kept")), rcBoundary(3, 2000, rcKeeping(4, 1, 2)),
			rcSummary(3, 4, 2100), rcReply(4, 5, "msg_before", 3000, rcSaid("before")),
			rcBoundary(6, 4000, `{"trigger":"manual"}`), rcSummary(6, 7, 4100), rcReply(7, 8, "msg_b", 5000,
				rcSaid("b")), rcCheckpoint(8), rcAttach(8, 10, 6000, rcNote), rcAttach(8, 11, 6100, rcNote),
			rcPrompt(11, 12, 7000), rcReply(12, 13, "msg_x", 8000, rcCalls("x1")), rcResults(13, 14, 9000, "x1")},
			held: true},
		"compacted, B2: a list of none beside a segment": {lines: rcCompacted(ctReplace(t, keeping,
			`"uuids":[`+rcQuoted(1, 2)+`]`, `"uuids":[]`), rcReply(4, 5, "msg_before", 3000, rcSaid("before")),
			rcCheckpoint(5), rcAttach(5, 7, 4000, rcNote), rcAttach(5, 8, 4100, rcNote), rcPrompt(8, 9, 5000),
			rcReply(9, 10, "msg_x", 6000, rcCalls("x1")), rcResults(10, 11, 7000, "x1")), held: true},
		"compacted, B3: a relink by its segment alone, a lagging checkpoint a turn names": {lines: rcCompacted(
			rcSegment(4, 1, 2), rcReply(4, 5, "msg_before", 3000, rcSaid("before")), rcCheckpoint(5),
			rcPrompt(5, 7, 4000), rcReply(7, 8, "msg_x", 5000, rcCalls("x1")), rcResults(8, 9, 6000, "x1")),
			held: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := hsRead(t, hsArchive(t, ctThread(tc.lines...)), ctSession, hsAsk(callK))
			if tc.unread {
				hsNoReading(t, got, err)
				if !strings.Contains(err.Error(), string(whyResultByCall)) {
					t.Errorf("no reading for %v; want it the model's %s", err, whyResultByCall)
				}
				return
			}
			hsReadWhole(t, got, err)
			if held, _ := got.Holds(callK); held != tc.held {
				t.Errorf("Holds = %t, its records %+v; want %t", held, got.Calls[callK.ID], tc.held)
			}
		})
	}
	// IN A SUBAGENT'S THREAD, by the same rule over each order its own load
	// hands, its subagent reported in each: another reply between the first
	// record of the use's reply and the use, told; and held, the result
	// written before its use but handed after it, and the control. And C1's
	// folds there as in the main thread: a batch naming the call on a reply
	// after its result, a result of an id split from the call's, and the
	// call's id split from another call's, each told; id is the call asked in
	// hsSubCall's place, answered "ok", where it is not "".
	batching := func(line string) string {
		return ctReplace(t, line, `"type":"assistant",`,
			`"type":"assistant","batchToolUses":[{"id":"`+hsSubCall+`","name":"Bash"}],`)
	}
	for name, tc := range map[string]struct {
		lines []string
		held  bool
		id    string
	}{
		"in a subagent's thread, as written": {lines: []string{prompt(0, 1), reply(1, 2, "msg_u", rcCalls(hsSubCall)),
			hsSubResult(t, 2, 3, 300), reply(3, 4, "msg_f", said)}, held: true},
		// [1, 2, 3, 4, 5, 6]: another reply between the use's reply's first
		// record and the use.
		"in a subagent's thread, another reply inside the use's reply": {lines: []string{prompt(0, 1),
			reply(1, 2, "msg_u", said), reply(2, 3, "msg_c", said), reply(3, 4, "msg_u", rcCalls(hsSubCall)),
			hsSubResult(t, 4, 5, 500), reply(5, 6, "msg_f", said)}},
		// [1, 2, 4, 3]: the result written first and handed after the use.
		"in a subagent's thread, its result written first and chained after its use": {lines: []string{
			prompt(0, 1), hsSubResult(t, 3, 2, 300), reply(1, 3, "msg_u", rcCalls(hsSubCall)),
			reply(2, 4, "msg_f", said)}, held: true},
		// [1, 2, 3, 4]: the batch on the reply that finished, past the stretch.
		"in a subagent's thread, a batch naming the call": {lines: []string{prompt(0, 1),
			reply(1, 2, "msg_u", rcCalls(hsSubCall)), hsSubResult(t, 2, 3, 300),
			batching(reply(3, 4, "msg_f", said))}},
		// [1, 2, 3, 4, 5]
		"in a subagent's thread, a result split from its id": {lines: []string{prompt(0, 1),
			reply(1, 2, "msg_u", rcCalls(hsSubCall)), hsSubResult(t, 2, 3, 300),
			results(3, 4, hsSubCall+"_1"), reply(4, 5, "msg_f", said)}},
		// [1, 2, 3, 4, 5, 6]
		"in a subagent's thread, its id split from another call's": {id: hsSubCall + "_1", lines: []string{
			prompt(0, 1), reply(1, 2, "msg_q", rcCalls(hsSubCall)), results(2, 3, hsSubCall),
			reply(3, 4, "msg_u", rcCalls(hsSubCall+"_1")), results(4, 5, hsSubCall+"_1"),
			reply(5, 6, "msg_f", said)}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := hsForeground(t)
			s.thread = hsOwn(t, tc.lines...)
			call := hsLogged(hsSubCall, hsSubOut)
			if tc.id != "" {
				call = hsLogged(tc.id, "ok")
			}
			got, err := hsRead(t, tarOfMembers(t, s.members()), ctSession, hsAsk(call))
			hsReadWhole(t, got, err)
			held, under := got.Holds(call)
			if records := got.Calls[call.ID]; held != tc.held || under != "" || !got.Subagents[ctAgent].Reported {
				t.Errorf("Holds = %t under %q, its records %+v, the subagent %+v; want it held %t, its subagent "+
					"reported", held, under, records, got.Subagents[ctAgent], tc.held)
			}
		})
	}
}

// A CALL WHOSE USE CARRIES NO REPLY ID IS TOLD (the pairing rule's C0,
// [holdingsPairing]): the request's fold merges an assistant record into the
// latest earlier message of its `message.id` as written, absent ids included,
// and its deserializer stamps a fresh id only on a record with no `requestId`
// either (2.1.322's e_n @295266158, 2.1.310's TJt @248494944). So an id-less use
// with a requestId merges back across messages into another such reply, and one
// with none is a message apart: no shape the rule can place. Told: N1. Over-told
// knowingly, the request pairing each: N2, N3, an empty-string id. Held: N1 with
// every reply of its own id. N4 is told before C0: the resume recovers records
// beside a reply by its id (fWo @293749272, Rpo @247134223), so no model is
// handed a result under the id-less use (C2; whyResultByCall).
// Mutation: drop C0, and each over-tell is held; tell every call at C0, and N1's control is told.
func TestACallWhoseUseCarriesNoReplyIDIsTold(t *testing.T) {
	t.Parallel()
	const x, z = "toolu_idless_x", "toolu_idless_z"
	callX := hsLogged(x, "ok")
	said := rcSaid("done")
	reply := func(parent, n int, id, content string) string { return rcReply(parent, n, id, n*100, content) }
	results := func(parent, n int, ids ...string) string { return rcResults(parent, n, n*100, ids...) }
	// idless is a reply's record whose message carries no id, and a requestId
	// where requested.
	idless := func(t *testing.T, line string, requested bool) string {
		t.Helper()
		line = ctReplace(t, line, `"id":"","type":"message",`, `"type":"message",`)
		if requested {
			line = ctReplace(t, line, `"type":"assistant",`, `"type":"assistant","requestId":"req_idless",`)
		}
		return line
	}
	brief := rcPrompt(0, 1, 0)
	for name, tc := range map[string]struct {
		lines []string
		held  bool
	}{
		// N1, [1, 2, 3, 4, 5, 6, 7]: 5 merges back into 2 across 3 and 4.
		"a use of no reply id with a requestId, merged back across other messages": {lines: []string{brief,
			idless(t, reply(1, 2, "", rcCalls(z)), true), results(2, 3, z), reply(3, 4, "msg_q", said),
			idless(t, reply(4, 5, "", rcCalls(x)), true), results(5, 6, x), reply(6, 7, "msg_after", said)}},
		// N1's control.
		"every reply with its id": {lines: []string{brief, reply(1, 2, "msg_z", rcCalls(z)), results(2, 3, z),
			reply(3, 4, "msg_q", said), reply(4, 5, "msg_u", rcCalls(x)), results(5, 6, x),
			reply(6, 7, "msg_after", said)}, held: true},
		// N4, [1, 2, 3, 4, 5]: 2 and 3 two messages, 4 after 3's.
		"a use of no reply id and no requestId, another such record before its result": {lines: []string{brief,
			idless(t, reply(1, 2, "", rcCalls(x)), false), idless(t, reply(2, 3, "", said), false),
			results(2, 4, x), reply(3, 5, "msg_after", said)}},
		// OVER-TELL: [1, 2, 3, 4]; stamped as N2's is.
		"a use whose reply id is the empty string": {lines: []string{brief, reply(1, 2, "", rcCalls(x)),
			results(2, 3, x), reply(3, 4, "msg_after", said)}},
		// N2, OVER-TELL: [1, 2, 3, 4]; the deserializer stamps an id of its own.
		"a use of no reply id and no requestId": {lines: []string{brief, idless(t, reply(1, 2, "", rcCalls(x)), false),
			results(2, 3, x), reply(3, 4, "msg_after", said)}},
		// N3, OVER-TELL: [1, 2, 3, 4]; nothing to merge into.
		"a use of no reply id with a requestId": {lines: []string{brief, idless(t, reply(1, 2, "", rcCalls(x)), true),
			results(2, 3, x), reply(3, 4, "msg_after", said)}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := hsRead(t, hsArchive(t, ctThread(tc.lines...)), ctSession, hsAsk(callX))
			hsReadWhole(t, got, err)
			if held, _ := got.Holds(callX); held != tc.held {
				t.Errorf("Holds = %t, its records %+v; want %t", held, got.Calls[x], tc.held)
			}
		})
	}
}

// hsOwn is lines, records as [rcReply] and its kin write them, as ctAgent's
// own records of its thread ([rcOwn]).
func hsOwn(t *testing.T, lines ...string) []string {
	t.Helper()
	own := make([]string, len(lines))
	for i, line := range lines {
		own[i] = rcOwn(t, line)
	}
	return own
}

// hsSubResult is record n, written at ms, the result of ctAgent's call
// hsSubCall of the text its bridge answered, chained to parent.
func hsSubResult(t *testing.T, parent, n, ms int) string {
	t.Helper()
	return ctReplace(t, rcResults(parent, n, ms, hsSubCall), `"content":"ok"`, `"content":"`+hsSubOut+`"`)
}

// hsReplyOf is a model's reply of the id reply, chained to parent, whose
// content is content: ctAssistant's, whose own id every record of it shares.
func hsReplyOf(t *testing.T, parent, uuid, reply, content string) string {
	t.Helper()
	return ctReplace(t, ctAssistant(parent, uuid, content), `"id":"msg_test"`, `"id":"`+reply+`"`)
}

// hsCheckpoint is a last-prompt record naming leaf, its keys after it beside.
func hsCheckpoint(t *testing.T, leaf, beside string) string {
	t.Helper()
	return ctReplace(t, ctLastPrompt, `"leafUuid":"`+ctUUID(5)+`"`, `"leafUuid":`+leaf+beside)
}

// hsProgress is a progress record, uuid, chained to parent: a legacy shape
// none of the files the probes kept holds, as Claude Code's load knows one (a
// `type` "progress" and a string uuid), its other keys a shell call's.
func hsProgress(parent, uuid string) string {
	return `{"parentUuid":"` + parent + `","isSidechain":false,"type":"progress","data":{"type":"bash_progress"},` +
		`"toolUseID":"toolu_test_progress","parentToolUseID":"toolu_test_progress","uuid":"` + uuid +
		`","timestamp":"` + ctStamp + `",` + ctTail("2.1.322") + `}`
}

// THE BLOCK RULES READ EVERY BLOCK'S KIND: a reply that its model ended ends
// a leg only where its blocks are texts and thinkings of either kind alone, at
// least one, so one holding a call or a server's call beside its text ends
// none; a user record starts a leg unless it is results alone, so a text, a
// list of none and a result beside a text each start one; a plain results
// record is results alone, one or two; and a record's own toolUseResult
// answers its call only where that result is the record's one block, never
// beside a text or a second result.
//
// Mutation: read a reply as finished by any block it holds of those kinds, a
// record as plain results by any result it holds, or a result as alone by its
// record's one call, and a record mixing kinds reads as one of a kind.
func TestTheBlockRulesReadEveryBlocksKind(t *testing.T) {
	t.Parallel()
	text := `{"type":"text","text":"done."}`
	thinking := `{"type":"thinking","thinking":"the batch","signature":"c2ln"},` +
		`{"type":"redacted_thinking","data":"ZGF0YQ=="}`
	server := `{"type":"server_tool_use","id":"srvtoolu_test","name":"web_search","input":{}}`
	use := ctUseContent[1 : len(ctUseContent)-1]
	result := ctResultBody[1 : len(ctResultBody)-1]
	other := `{"tool_use_id":"toolu_other","type":"tool_result","content":"b2"}`
	for name, tc := range map[string]struct {
		content  string
		finished bool
	}{
		"a text":                             {`[` + text + `]`, true},
		"a text and both thinkings":          {`[` + thinking + `,` + text + `]`, true},
		"a list of none":                     {`[]`, false},
		"a text and a call":                  {`[` + text + `,` + use + `]`, false},
		"a text and a server's call":         {`[` + text + `,` + server + `]`, false},
		"both thinkings and a server's call": {`[` + thinking + `,` + server + `]`, false},
	} {
		m, err := ctLine(t, ctAssistant(ctUUID(2), ctUUID(3), tc.content)).Message()
		if err != nil {
			t.Fatalf("a reply of %s reads no message: %v", name, err)
		}
		if got := finishedReply(m.Blocks(), claudeEndedTurn, "claude-sonnet-4-5", false); got != tc.finished {
			t.Errorf("a reply of %s ended by its model finishes its leg %t, want %t", name, got, tc.finished)
		}
	}
	for name, tc := range map[string]struct {
		record             string
		start, plain, lone bool
	}{
		"a result alone": {record: ctResult(ctUUID(4), ctUUID(5), `[`+result+`]`, ctResultOwn), plain: true,
			lone: true},
		"two results": {record: ctResult(ctUUID(4), ctUUID(5), `[`+result+`,`+other+`]`, ctResultOwn),
			plain: true},
		"a result beside a text": {record: ctResult(ctUUID(4), ctUUID(5), `[`+result+`,`+text+`]`, ctResultOwn),
			start: true},
		"a list of none": {record: ctResult(ctUUID(4), ctUUID(5), `[]`, ctResultOwn), start: true},
		"a text":         {record: ctBrief(ctUUID(1)), start: true},
	} {
		line := ctLine(t, tc.record)
		m, err := line.Message()
		if err != nil {
			t.Fatalf("%s reads no message: %v", name, err)
		}
		if start, plain, lone := legStart(m), plainResults(line, m), resultAlone(m); start != tc.start ||
			plain != tc.plain || lone != tc.lone {
			t.Errorf("%s starts a leg %t, is plain results %t, a result alone %t; want %t, %t and %t", name, start,
				plain, lone, tc.start, tc.plain, tc.lone)
		}
	}
}

// A CALL WHOSE RECORD A RETRACTION NAMES IS TOLD: Claude Code's resume drops
// every record whose uuid's first 24 characters are an entry of a retraction
// list, after walking the chain and recovering beside each reply ([threadChain],
// THE RETRACTION FILTER: 2.1.322 reads the list off a fallback, a retry and a
// reply's own, 2.1.310 off a fallback's alone). Each record has a uuid of its own
// first 24 characters ([rcUUID]). Told: a use or result a fallback names, a use
// a retry names, an entry of 24, 30 or 24-plus-another-tail. Held: none, a
// 23-character entry, an uppercase one, a use beside a chain record a fallback
// names. No reading: a list in no shape, a result under a retracted record whose
// call its sibling made (whyResultByCall); a subagent's list is
// [TestARetractionListInASubagentsThreadLeavesItNotWhole]'s.
// Mutation: retract nothing, or drop the retry, 24-character or case check.
func TestACallWhoseRecordARetractionNamesIsTold(t *testing.T) {
	t.Parallel()
	const x = "toolu_retracted"
	callX := hsLogged(x, "ok")
	said := rcSaid("done")
	// u is the use's record: a uuid holding letters, which an uppercase
	// spelling changes.
	const u = 0xab
	if !strings.ContainsAny(rcUUID(u), "abcdef") {
		t.Fatalf("record %d's uuid %s holds no letter", u, rcUUID(u))
	}
	// on is the use, its result and a reply after them, all on the chain.
	on := []string{rcPrompt(0, 1, 100), rcReply(1, u, "msg_x", 200, rcCalls(x)), rcResults(u, 3, 300, x),
		rcReply(3, 4, "msg_after", 400, said)}
	// with is on and a fallback under its last reply naming entries, and a
	// reply after it.
	with := func(entries string) []string {
		return append(slices.Clone(on), rcFallback(4, 5, 500, entries), rcReply(5, 6, "msg_last", 600, said))
	}
	quoted := func(entry string) string { return `"` + entry + `"` }
	for name, tc := range map[string]struct {
		lines        []string
		held, unread bool
	}{
		"no retraction": {lines: on, held: true},
		"the use's record named by a refused reply's fallback":    {lines: with(rcQuoted(u))},
		"the result's record named by a refused reply's fallback": {lines: with(rcQuoted(3))},
		"the use's record named by a malformed reply's retry": {lines: append(slices.Clone(on),
			rcRetry(4, 5, 500, rcQuoted(u)), rcReply(5, 6, "msg_last", 600, said))},
		"the use's record named by its first 24 characters": {lines: with(quoted(rcUUID(u)[:24]))},
		"the use's record named by its first 30 characters": {lines: with(quoted(rcUUID(u)[:30]))},
		"the use's record named by its first 24 characters and another tail": {lines: with(
			quoted(rcUUID(u)[:24] + "ffffffffffff"))},
		"an entry of the use's record's first 23 characters": {lines: with(quoted(rcUUID(u)[:23])), held: true},
		"an entry naming the use's record in uppercase": {lines: with(quoted(strings.ToUpper(rcUUID(u)))),
			held: true},
		// [1, u, 4, 5, 6, 7]: 2, msg_p's record on the chain, retracted; u,
		// recovered beside it, and its result stay.
		"the record of its reply on the chain named, the use beside it": {lines: []string{rcPrompt(0, 1, 100),
			rcReply(1, 2, "msg_p", 200, said), rcReply(1, u, "msg_p", 250, rcCalls(x)), rcResults(u, 4, 300, x),
			rcReply(2, 5, "msg_after", 400, said), rcFallback(5, 6, 500, rcQuoted(2)),
			rcReply(6, 7, "msg_last", 600, said)}, held: true},
		// [1, 2, 4, 5, 7]: u, recovered beside 2, retracted.
		"the use beside the record of its reply on the chain named": {lines: []string{rcPrompt(0, 1, 100),
			rcReply(1, 2, "msg_p", 200, said), rcReply(1, u, "msg_p", 250, rcCalls(x)), rcResults(u, 4, 300, x),
			rcReply(2, 5, "msg_after", 400, said), rcFallback(5, 6, 500, rcQuoted(u)),
			rcReply(6, 7, "msg_last", 600, said)}},
		"the result under the record of its reply on the chain, which a fallback names": {lines: []string{
			rcPrompt(0, 1, 100), rcReply(1, u, "msg_p", 150, rcCalls(x)), rcReply(1, 2, "msg_p", 200, said),
			rcResults(2, 4, 300, x), rcReply(4, 5, "msg_after", 400, said), rcFallback(5, 6, 500, rcQuoted(2)),
			rcReply(6, 7, "msg_last", 600, said)}, unread: true},
		"a fallback's list in no shape Claude Code writes": {lines: with(`1`), unread: true},
		"a retry's list in no shape Claude Code writes": {lines: append(slices.Clone(on),
			rcRetry(4, 5, 500, rcQuoted(u)+`,null`), rcReply(5, 6, "msg_last", 600, said)), unread: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := hsRead(t, hsArchive(t, ctThread(tc.lines...)), ctSession, hsAsk(callX))
			if tc.unread {
				hsNoReading(t, got, err)
				return
			}
			hsReadWhole(t, got, err)
			if held, _ := got.Holds(callX); held != tc.held {
				t.Errorf("Holds = %t, its records %+v; want %t", held, got.Calls[x], tc.held)
			}
		})
	}
}

// A MAIN THREAD THE RESUME'S MODEL CANNOT PLACE IS NO READING: where the model
// of Claude Code's resume cannot tell for the thread ([threadChain.Unknown]),
// every call is told and no line past the first unplaced one is read. Such: a
// checkpoint a rewind or withdrawal marked, or whose leaf is no string; a
// sidechain record, fork briefing or file snapshot; a reply of string content;
// an undecodable user or assistant message; a record of no, uppercase or shared
// uuid; a progress record; a result whose call another record made than its
// parent (tengu_foamy_spring); a compaction's metadata in no shape Claude Code
// writes. The thread without them is read, as beside a checkpoint its last
// record descends from; shapes the model does place are [threadChain]'s tests.
// Mutation: read past such a thread, or on past its first such line, and the
// case is read or the next non-record line ends it.
func TestAMainThreadTheResumeModelCannotPlaceIsUnread(t *testing.T) {
	t.Parallel()
	const x, out = "toolu_leaf", "leaf done"
	call := hsLogged(x, out)
	pair := hsCall(ctUUID(1), 2, x, hsBlock(t, out))
	with := func(lines ...string) []string { return append(slices.Clone(pair), lines...) }
	cleared := hsReplyOf(t, ctUUID(1), ctUUID(9), "msg_left", `[{"type":"text","text":"left."}]`)
	done := `[{"type":"text","text":"done."}]`
	// answered is x's result, uuid, chained to parent.
	answered := func(parent, uuid int) string {
		return ctResult(ctUUID(parent), ctUUID(uuid), hsAnswer(x, hsBlock(t, out)), hsBlock(t, out))
	}
	for name, tc := range map[string]struct {
		lines []string
		read  bool
	}{
		"the thread": {lines: pair, read: true},
		"a checkpoint its last record descends from": {lines: with(hsCheckpoint(t, `"`+ctUUID(2)+`"`, "")),
			read: true},
		"a checkpoint naming no record":         {lines: with(hsCheckpoint(t, `"`+ctUUID(99)+`"`, "")), read: true},
		"a checkpoint naming no canonical uuid": {lines: with(hsCheckpoint(t, `"x"`, "")), read: true},
		"a checkpoint a compaction's boundary cleared": {lines: []string{cleared, hsCheckpoint(t, `"`+ctUUID(9)+`"`, ""),
			ctBoundary(ctUUID(10), ctUUID(11), ctUUID(9)), ctCompactSummary(ctUUID(10), ctUUID(11)),
			ctAssistant(ctUUID(11), ctUUID(2), hsUse(x)), pair[1]}, read: true},
		"a checkpoint a rewind marked": {lines: with(hsCheckpoint(t, `"`+ctUUID(2)+`"`,
			`,"explicit":true,"rewound":true`))},
		"a checkpoint a withdrawal marked":    {lines: with(hsCheckpoint(t, `null`, `,"explicit":true`))},
		"a checkpoint whose leaf is a number": {lines: with(hsCheckpoint(t, `7`, ""))},
		"a sidechain's record": {lines: with(ctReplace(t, ctSpan(ctUUID(3), ctUUID(4), ""), `"isSidechain":false`,
			`"isSidechain":true`))},
		"a fork's briefing": {lines: with(ctAttachment(ctUUID(3), ctUUID(4),
			`{"type":"fork_briefing","text":"the fork's brief"}`, ""))},
		"a file's snapshot": {lines: with(ctReplace(t, ctMicrocompactBoundary(ctUUID(3), ctUUID(4)),
			`"subtype":"microcompact_boundary"`, `"subtype":"file_snapshot"`))},
		"a reply whose content is a string": {lines: with(hsReplyOf(t, ctUUID(3), ctUUID(4), "msg_said", `"done."`))},
		"a reply whose content is a blank string": {lines: with(hsReplyOf(t, ctUUID(3), ctUUID(4), "msg_said",
			`" "`))},
		"an agent's record whose message does not decode": {lines: with(ctReplace(t,
			ctResult(ctUUID(3), ctUUID(4), `42`, `42`), `"isSidechain":false`,
			`"isSidechain":false,"agentId":"`+ctAgent+`"`))},
		"a record of no uuid": {lines: with(ctReplace(t, ctAttachment(ctUUID(3), ctUUID(4), ctTokens,
			ctTokensRendered), `"uuid":"`+ctUUID(4)+`",`, ""))},
		"a record of an uppercase uuid": {lines: with(ctAttachment(ctUUID(3), "AAAAAAAA-0000-4000-8000-000000000004",
			ctTokens, ctTokensRendered))},
		"a record of a uuid another carries": {lines: with(ctAttachment(ctUUID(3), ctUUID(2), ctTokens,
			ctTokensRendered))},
		"a progress record": {lines: with(hsProgress(ctUUID(3), ctUUID(4)))},
		"a progress record of its call's uuid, before it": {lines: []string{hsProgress(ctUUID(1), ctUUID(2)), pair[0],
			pair[1]}},
		"a call whose parent, its own result, is written after it": {lines: []string{
			ctAssistant(ctUUID(3), ctUUID(2), hsUse(x)), ctResult(ctUUID(1), ctUUID(3), hsAnswer(x, hsBlock(t, out)),
				hsBlock(t, out)), ctAssistant(ctUUID(2), ctUUID(5), done)}},
		"an attachment on the walk whose parent, the call's result, is written after it": {lines: []string{
			ctAttachment(ctUUID(4), ctUUID(2), ctTokens, ctTokensRendered), ctAssistant(ctUUID(2), ctUUID(3), hsUse(x)),
			ctResult(ctUUID(1), ctUUID(4), hsAnswer(x, hsBlock(t, out)), hsBlock(t, out)),
			ctAssistant(ctUUID(3), ctUUID(5), done)}},
		"a compaction keeping a record written after its boundary beside one no thread holds": {lines: []string{
			ctBoundary(ctUUID(10), ctUUID(11), ctUUID(12), ctUUID(99)), ctCompactSummary(ctUUID(10), ctUUID(11)),
			ctAssistant(ctUUID(11), ctUUID(12), hsUse(x)), answered(12, 13)}, read: true},
		"a compaction naming what it kept in no shape Claude Code writes": {lines: []string{
			ctReplace(t, ctBoundary(ctUUID(10), ctUUID(11), ctUUID(9)), `"uuids":["`+ctUUID(9)+`"]`,
				`"uuids":"`+ctUUID(9)+`"`),
			ctCompactSummary(ctUUID(10), ctUUID(11)), ctAssistant(ctUUID(11), ctUUID(12), hsUse(x)), answered(12, 13)}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := hsRead(t, hsArchive(t, hsThread(tc.lines...)), ctSession, hsAsk(call))
			if !tc.read {
				hsNoReading(t, got, err)
				return
			}
			hsReadWhole(t, got, err)
			if held, _ := got.Holds(call); !held {
				t.Errorf("Holds = false, its records %+v; want its call held", got.Calls[x])
			}
		})
	}
	// AT ONCE: the line the model first cannot place ends the reading, which
	// reads no line after it, a line that is no record among them.
	t.Run("no line read past the first the model cannot place", func(t *testing.T) {
		t.Parallel()
		got, err := hsRead(t, hsArchive(t, hsThread(with(hsProgress(ctUUID(3), ctUUID(4)), `[1,2,3]`)...)),
			ctSession, hsAsk(call))
		hsNoReading(t, got, err)
		if !strings.Contains(err.Error(), "line 4 of the session's main thread holds a shape") {
			t.Errorf("no reading for %v; want it the progress record's, line 4", err)
		}
	})
}

// A MAIN THREAD PAST THE MODEL'S BOUND IS NO READING: past the bound of the
// model of Claude Code's resume ([threadChainMaxRecords]: here one whose
// refusal's fallback names more uuids than it files) the model cannot tell
// what the resume hands, and the reading says so. (A subagent's thread past
// it is read on: [TestASubagentPastTheChainsBoundIsCountedStill].)
//
// Mutation: read on as though the model placed a thread past its bound, and
// the reading stands.
func TestAThreadIsWalkedWithinItsRecordsBound(t *testing.T) {
	t.Parallel()
	named := make([]int, threadChainMaxRecords)
	for i := range named {
		named[i] = 1000 + i
	}
	pair := hsCall(ctUUID(1), 2, hsSubCall, hsBlock(t, hsSubOut))
	thread := hsThread(append(pair, rcFallback(3, 4, 0, rcQuoted(named...)))...)
	got, err := hsRead(t, hsArchive(t, thread), ctSession, hsAsk(hsLogged(hsSubCall, hsSubOut)))
	hsNoReading(t, got, err)
	if !strings.Contains(err.Error(), string(whyBound)) {
		t.Errorf("no reading for %v; want it the model's bound", err)
	}
}

// A RESULT'S TEXT IS A STRING OR ONE TEXT BLOCK: what a model is handed of a
// call's output is the content's string, or the text of a list of exactly one
// object of `type` "text" whose `text` is a string, other keys passed over, keys
// matched exactly, a repeated key read by its last value. Any other content
// (tool references, two blocks, an empty list, an image, a key in another case,
// a `text` that is no string) has no text, -1, which no log's output matches. A
// MiB list is passed over past its second element: at most 16 KiB more than a
// list of two, no allocation per element. Not parallel: the allocations are the
// process's, and a parallel case's would count.
// Mutation: copy each element past the first, and a long list costs an
// allocation each; take any list's first text block or any `text` kind, and
// two blocks match or a null reads as the empty text.
func TestAResultsTextIsAStringOrOneTextBlock(t *testing.T) {
	const id = "toolu_text"
	for name, tc := range map[string]struct {
		content string
		text    string
		bytes   int
	}{
		"a string":                     {`"a string"`, "a string", 8},
		"one text block":               {`[{"type":"text","text":"one block"}]`, "one block", 9},
		"one text block, keys beside":  {`[{"type":"text","text":"one block","cache_control":null}]`, "one block", 9},
		"one text block, text twice":   {`[{"type":"text","text":"first","text":"last"}]`, "last", 4},
		"ToolSearch's tool references": {`[{"type":"tool_reference","tool_name":"mcp__seat__echo"}]`, "", -1},
		"two text blocks":              {`[{"type":"text","text":"a"},{"type":"text","text":"b"}]`, "", -1},
		"an empty list":                {`[]`, "", -1},
		"an image":                     {`[{"type":"image","source":{"type":"base64","data":"AA=="}}]`, "", -1},
		"a type spelt in another case": {`[{"Type":"text","text":"x"}]`, "", -1},
		"a text spelt in another case": {`[{"type":"text","Text":"x"}]`, "", -1},
		"a text that is no string":     {`[{"type":"text","text":1}]`, "", -1},
		"a text that is null":          {`[{"type":"text","text":null}]`, "", -1},
		"a block that is no object":    {`["x"]`, "", -1},
		"null":                         {`null`, "", -1},
	} {
		t.Run(name, func(t *testing.T) {
			thread := hsThread(ctAssistant(ctUUID(1), ctUUID(2), hsUse(id)),
				ctResult(ctUUID(2), ctUUID(3), hsAnswer(id, tc.content), tc.content))
			got, err := hsRead(t, hsArchive(t, thread), ctSession, hsAsk(sandbox.LoggedCall{ID: id, Asked: true}))
			hsReadWhole(t, got, err)
			records := got.Calls[id]
			want := [32]byte{}
			if tc.bytes >= 0 {
				want = sha256.Sum256([]byte(tc.text))
			}
			if records.TextBytes != tc.bytes || records.Opening != want {
				t.Errorf("its text is %d bytes opening %x; want %d bytes opening %x", records.TextBytes,
					records.Opening, tc.bytes, want)
			}
		})
	}
	t.Run("absent", func(t *testing.T) {
		content := `[{"tool_use_id":"` + id + `","type":"tool_result"}]`
		thread := hsThread(ctAssistant(ctUUID(1), ctUUID(2), hsUse(id)),
			ctResult(ctUUID(2), ctUUID(3), content, `""`))
		got, err := hsRead(t, hsArchive(t, thread), ctSession, hsAsk(sandbox.LoggedCall{ID: id, Asked: true}))
		hsReadWhole(t, got, err)
		if records := got.Calls[id]; records.Results != 1 || records.TextBytes != -1 {
			t.Errorf("its records %+v; want one result with no text", records)
		}
	})
	t.Run("a list of a MiB", func(t *testing.T) {
		line := func(n int) []scanPart {
			return scanLine(scanLit(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result",`+
				`"tool_use_id":"`+id+`","content":[{"type":"text","text":"x"}`),
				[]scanPart{{",1", n}}, scanLit(`]}]}}`))
		}
		short, long := line(1), line(listBytes/2)
		shortCost, _, shortErr := scanAllocated(t.Context(), short, short, 3)
		longCost, read, err := scanAllocated(t.Context(), short, long, 2)
		m, refused := read.Message()
		if shortErr != nil || err != nil || refused != nil || len(m.Calls()) != 1 || m.Calls()[0].Text.Bytes() != -1 {
			t.Fatalf("a result of a text block and a MiB more reads as %v, %v, its calls %+v; want one result "+
				"of no text", err, refused, m.Calls())
		}
		if longCost > shortCost+16<<10 {
			t.Errorf("a result list of a MiB costs its line's reading %d bytes, where a list of two costs %d: it is "+
				"read past its second element", longCost, shortCost)
		}
	})
}

// A WHOLE LOGGED OUTPUT IS HELD TO EVERY BYTE OF ITS RESULT: where the log kept
// a call's output whole, its result's text is compared with all of it, so a
// result as long and alike for its opening's digest, and otherwise past it,
// is told, where the same text whole is held. A cut output is compared by its
// size and opening, the most of it the log keeps for certain, so a result
// alike that far is held whatever it holds past it; and a text longer than any
// output the log keeps whole keeps no whole digest.
//
// Mutation: compare a whole output by its opening alone, and the result
// otherwise past it holds its call; or keep no whole digest, and the whole
// one is told.
func TestAWholeLoggedOutputIsHeldToEveryByteOfItsResult(t *testing.T) {
	t.Parallel()
	const id = "toolu_whole_text"
	opening := strings.Repeat("o", sandbox.HoldingsOpeningBytes)
	long := opening + strings.Repeat("l", sandbox.MaxBridgeOutputBytes)
	for name, tc := range map[string]struct {
		text string
		call sandbox.LoggedCall
		held bool
	}{
		"a whole output, its result whole":              {opening + "tail", hsLogged(id, opening+"tail"), true},
		"a whole output, its result otherwise past it":  {opening + "TAIL", hsLogged(id, opening+"tail"), false},
		"a whole output, its result otherwise early on": {"O" + opening[1:] + "tail", hsLogged(id, opening+"tail"), false},
		"a cut output, its result alike to its opening's end": {long, sandbox.LoggedCall{ID: id, Asked: true,
			Output: opening + "…(marker)", OutputBytes: len(long)}, true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			thread := hsThread(hsCall(ctUUID(1), 2, id, hsBlock(t, tc.text))...)
			got, err := hsRead(t, hsArchive(t, thread), ctSession, hsAsk(tc.call))
			hsReadWhole(t, got, err)
			if held, _ := got.Holds(tc.call); held != tc.held {
				t.Errorf("Holds = %t, its records %+v; want %t", held, got.Calls[id], tc.held)
			}
			if records := got.Calls[id]; (records.Whole == [32]byte{}) != (len(tc.text) > sandbox.MaxBridgeOutputBytes) {
				t.Errorf("a text of %d bytes keeps the whole digest %x; want one only up to %d bytes", len(tc.text),
					records.Whole, sandbox.MaxBridgeOutputBytes)
			}
		})
	}
}

// THE RESUME'S MARK DECIDES NOTHING (rule 3, met by rule 4). Measured (EK): a

// resume that finds a call with no result writes one of its own, a failure in
// its own words, marked `toolDenialKind: interrupted`; read against the
// success the log took, its call is told. And it is told for its failure and
// its text, never its mark: told with the mark stripped, and told with its text
// reworded to the log's very output while it still says it failed; held only
// where the result is the log's, a success of the log's text, whatever mark it
// carries. Its call, a shell call as the probe made it, is renamed a call of a
// tool of the bridge's, as every call the log keeps is
// ([claudeBridgedToolPrefix]).
//
// Mutation: read a result's is_error as absent, and the reworded result holds
// its call.
func TestTheResumesMarkDecidesNothing(t *testing.T) {
	t.Parallel()
	const output = "e1-done\n"
	call := hsLogged("toolu_e1", output)
	const own = "[Tool call interrupted: the session ended before this call's result was recorded, so its " +
		"outcome is unknown. Check whether it took effect before relying on it or running it again.]"
	reworded := [2]string{own, "e1-done\\n"}
	succeeded := [2]string{`"is_error":true,"tool_use_id":"toolu_e1"`, `"is_error":false,"tool_use_id":"toolu_e1"`}
	for name, tc := range map[string]struct {
		edits [][2]string
		held  bool
	}{
		"as the resume wrote it":         {nil, false},
		"its mark stripped":              {[][2]string{{`"toolDenialKind":"interrupted",`, ``}}, false},
		"its text the log's, a failure":  {[][2]string{reworded}, false},
		"the log's result, mark and all": {[][2]string{reworded, succeeded}, true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			bridged := [2]string{`"id":"toolu_e1","name":"Bash"`, `"id":"toolu_e1","name":"mcp__crewlet__slow"`}
			archive, session := hsFixture(t, "2.1.310", "EK", append([][2]string{bridged}, tc.edits...)...)
			got, err := hsRead(t, archive, session, hsAsk(call))
			hsReadWhole(t, got, err)
			if held, _ := got.Holds(call); held != tc.held {
				t.Errorf("Holds = %t, its records %+v; want %t", held, got.Calls[call.ID], tc.held)
			}
		})
	}
}

// A STOPPED OR GIVEN-UP HARNESS'S OWN RESULT DOES NOT HOLD ITS CALL. Measured:
// a harness stopped in a shell call records its exit code as the call's failed
// result (ET0; its shell call renamed a call of a bridge tool, as every logged
// call is), and one that gave up on a slow call records its own timeout text as
// a failure (pB, a) while the bridge logged success and output; each is told.
// The timeout text is as long as that output, so it is told for its text too. A
// call answered in time (pB, c) is held: its result is the log's, beside a
// request's blob repeating it with a reminder, which is no record of the thread.
//
// Mutation: compare a result's text with the log's output by length alone, and
// the timeout text read as a success holds its call.
func TestAStoppedOrGivenUpHarnesssOwnResultDoesNotHoldItsCall(t *testing.T) {
	t.Parallel()
	gaveUp := hsLogged("toolu_pb_a", "slow a finished its work")
	for name, tc := range map[string]struct {
		fixture string
		edits   [][2]string
		call    sandbox.LoggedCall
		held    bool
	}{
		"a harness stopped in its call": {"ET0", [][2]string{{`"id":"toolu_et1","name":"Bash"`,
			`"id":"toolu_et1","name":"mcp__crewlet__slow"`}}, hsLogged("toolu_et1", "et1-done\n"), false},
		"a call its harness gave up on": {"pBa", nil, gaveUp, false},
		"its timeout text, as a success": {"pBa", [][2]string{{`"is_error":true,"tool_use_id":"toolu_pb_a"`,
			`"is_error":false,"tool_use_id":"toolu_pb_a"`}}, gaveUp, false},
		"a call answered in time": {"pBc", nil, hsLogged("toolu_pb_c", "slow c finished its work"), true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			archive, session := hsFixture(t, "2.1.310", tc.fixture, tc.edits...)
			got, err := hsRead(t, archive, session, hsAsk(tc.call))
			hsReadWhole(t, got, err)
			records := got.Calls[tc.call.ID]
			if held, _ := got.Holds(tc.call); held != tc.held || records.Uses != 1 || records.Results != 1 ||
				!records.Live {
				t.Errorf("Holds = %t, its records %+v; want %t, from its one live result", held, records, tc.held)
			}
		})
	}
}

// AN ARCHIVE NAMING A MEMBER OUTSIDE IT IS NOT READ, whichever member it is: a
// name that is absolute, or holds a `..` element, can be restored over the
// file a resumed session reads, so the member read need not be that file. The
// rule is held on the RAW name: `projects/crewlet/x/../y` is clean of `..`
// once cleaned. A member under its own name beside the main thread leaves the
// reading.
//
// Mutation: hold the name to the rule only once it is cleaned, and the
// interior `..` reads.
func TestHoldingsReadNoArchiveNamingAMemberOutsideIt(t *testing.T) {
	t.Parallel()
	thread := hsThread(hsCall(ctUUID(1), 2, "toolu_outside", hsBlock(t, "outside done"))...)
	for name, tc := range map[string]struct {
		member string
		read   bool
	}{
		"an absolute name":  {"/" + hsMain, false},
		"an interior ..":    {claudeProjectPrefix + "x/../y", false},
		"a leading ..":      {"../y", false},
		"a name of its own": {claudeProjectPrefix + "x/y", true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			archive := tarOfMembers(t, []tarMember{
				{h: tar.Header{Name: hsMain, Mode: 0o600, Typeflag: tar.TypeReg}, body: thread},
				{h: tar.Header{Name: tc.member, Mode: 0o600, Typeflag: tar.TypeReg}, body: "a file\n"},
			})
			got, err := hsRead(t, archive, ctSession, hsAsk(hsLogged("toolu_outside", "outside done")))
			if tc.read {
				hsReadWhole(t, got, err)
				return
			}
			hsNoReading(t, got, err)
		})
	}
}

// A READ PATH HELD TWICE LEAVES THE SESSION UNREAD: tar restores the last of
// two members that clean to one name, so a reading of either could be of the
// file the resumed session does not hold, whichever comes first.
//
// Mutation: drop the check, and the archive is read as its first copy.
func TestAReadPathHeldTwiceLeavesTheSessionUnread(t *testing.T) {
	t.Parallel()
	thread := hsThread(hsCall(ctUUID(1), 2, "toolu_twice", hsBlock(t, "twice done"))...)
	main := tarMember{h: tar.Header{Name: hsMain, Mode: 0o600, Typeflag: tar.TypeReg}, body: thread}
	// Each copy is a thread of its own, so the case turns on the copy alone.
	copyAs := func(name string) tarMember {
		return tarMember{h: tar.Header{Name: name, Mode: 0o600, Typeflag: tar.TypeReg}, body: hsThread()}
	}
	for name, members := range map[string][]tarMember{
		"its copy after it":  {main, copyAs("./" + hsMain)},
		"its copy before it": {copyAs("./" + hsMain), main},
		"two slashes":        {main, copyAs(strings.Replace(hsMain, "/", "//", 1))},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := hsRead(t, tarOfMembers(t, members), ctSession, hsAsk(hsLogged("toolu_twice", "twice done")))
			hsNoReading(t, got, err)
		})
	}
	t.Run("once", func(t *testing.T) {
		t.Parallel()
		got, err := hsRead(t, tarOfMembers(t, []tarMember{main}), ctSession, hsAsk(hsLogged("toolu_twice", "twice done")))
		hsReadWhole(t, got, err)
	})
}

// AN UNREAD MEMBER IS HELD TO THE STREAM'S BOUND: tar passes over a member no
// read path names by reading it through the archive's meter, so one that
// inflates past the plaintext a reading reads ends the reading, the scrub's
// own bound named, and one inside it is passed over.
//
// Mutation: hand eachMember no plaintext bound, and the member past it passes.
func TestAnUnreadMemberIsHeldToTheStreamsBound(t *testing.T) {
	t.Parallel()
	bounds := scrubBounds{plain: 1 << 20}
	other := claudeProjectPrefix + ctOther + ".jsonl"
	for name, tc := range map[string]struct {
		size int
		read bool
	}{
		"inside the bound": {256 << 10, true},
		"past the bound":   {2 << 20, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			archive := tarOf(t, map[string]string{hsMain: hsThread(), other: strings.Repeat("x", tc.size)})
			got, err := hsReadFrom(t.Context(), bytes.NewReader(archive), ctSession,
				hsAsk(hsLogged("toolu_bound", "bound done")), bounds)
			if tc.read {
				hsReadWhole(t, got, err)
				return
			}
			hsNoReading(t, got, err)
			if !errors.Is(err, errScrubBound) {
				t.Errorf("the reading ended with %v; want the scrub's bound", err)
			}
		})
	}
}

// hsLatched is a session's stream as the store hands one out: its bytes, and
// then fault for every read past them, latched as the store's stream latches
// its error; io.EOF where fault is nil.
type hsLatched struct {
	r     io.Reader
	fault error
}

func (b *hsLatched) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if errors.Is(err, io.EOF) && b.fault != nil {
		return n, b.fault
	}
	return n, err
}

// A READING DECIDES ONLY ONCE ITS STREAM ENDED: what the store streams is
// provisional until io.EOF, and tar stops at its end of archive, before the
// stream does. A stream that fails after the archive's last byte, and one cut
// short of its gzip trailer, past tar's end, each fail at the read past what
// they handed out, and each is no reading; the same stream ending at io.EOF is
// read.
//
// Mutation: decide at tar's end, without draining the stream, and both
// readings stand.
func TestAReadingDecidesOnlyOnceItsStreamEnded(t *testing.T) {
	t.Parallel()
	call := hsLogged("toolu_stream", "stream done")
	archive := hsArchive(t, hsThread(hsCall(ctUUID(1), 2, call.ID, hsBlock(t, call.Output))...))
	fault := errors.New("the stream's last chunk does not authenticate")
	for name, tc := range map[string]struct {
		bytes []byte
		fault error
	}{
		"its whole stream":                         {archive, nil},
		"a fault after its last byte":              {archive, fault},
		"cut short of its trailer, past tar's end": {archive[:len(archive)-8], fault},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := hsReadFrom(t.Context(), &hsLatched{r: bytes.NewReader(tc.bytes), fault: tc.fault},
				ctSession, hsAsk(call), hsBounds())
			if tc.fault == nil {
				hsReadWhole(t, got, err)
				return
			}
			hsNoReading(t, got, err)
			if !errors.Is(err, fault) {
				t.Errorf("the reading ended with %v; want the stream's own fault", err)
			}
		})
	}
}

// hsCancelling is a stream that ends ctx's reading once it has handed out more
// than after bytes, and counts what it hands out.
type hsCancelling struct {
	r      io.Reader
	after  int
	cancel context.CancelFunc
	handed int
}

func (c *hsCancelling) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.handed += n
	if c.handed > c.after {
		c.cancel()
	}
	return n, err
}

// A READING STOPS BETWEEN LINES WHEN ITS CONTEXT ENDS, which the line reader
// asks before each line ([threadReader.Next]), since a line's read takes
// none: a caller that left while a thread was read, many mebibytes that
// gzip barely shrinks, the main thread's or a subagent's, ends the reading
// within a line, with the caller's own error and the rest of the stream never
// read. A walk that asked only between members would read the thread whole
// and drain the stream.
//
// Mutation: ask the context between members alone, never in the line
// reader's Next, and the reading reads that whole thread and stands.
func TestAReadingStopsBetweenLinesWhenItsContextEnds(t *testing.T) {
	t.Parallel()
	random := rand.NewChaCha8([32]byte{'h', 'o', 'l', 'd'})
	noise := make([]byte, 48<<10)
	lines := make([]string, 0, 128)
	for range cap(lines) {
		_, _ = random.Read(noise)
		lines = append(lines, `{"type":"queue-operation","operation":"enqueue","sessionId":"`+ctSession+
			`","content":"`+base64.StdEncoding.EncodeToString(noise)+`"}`)
	}
	member := func(name, body string) tarMember {
		return tarMember{h: tar.Header{Name: name, Mode: 0o600, Typeflag: tar.TypeReg}, body: body}
	}
	for name, members := range map[string][]tarMember{
		"in the main thread":     {member(hsMain, hsThread(lines...))},
		"in a subagent's thread": {member(hsMain, hsThread()), member(hsAgentThread, ctThread(lines...))},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			archive := tarOfMembers(t, members)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			stream := &hsCancelling{r: bytes.NewReader(archive), after: 1 << 20, cancel: cancel}
			got, err := hsReadFrom(ctx, stream, ctSession, hsAsk(hsLogged("toolu_ctx", "ctx done")), hsBounds())
			if got.Read || !errors.Is(err, context.Canceled) {
				t.Fatalf("a reading whose caller left = %+v, %v; want no reading, with its caller's error", got, err)
			}
			if stream.handed >= len(archive) {
				t.Errorf("the reading read all %d bytes of a stream whose caller left after %d", len(archive),
					stream.after)
			}
		})
	}
}

// HOLDINGS ARE HELD TO THEIR BOUNDS: a line holding a value it reads whole
// past [threadValueMax], or whose facts pass [threadLineMax], ends the
// reading, the layer's bound named, where a 9 MiB line of a value no reader
// reads is read; a main thread that is no regular file is not read, as the
// box restores none; an archive holding no main thread is no reading; and one
// naming more subagents than [holdingsMaxAgents], or more legs than
// [holdingsMaxLegs], notifications among them, is none, where one at each
// bound is read.
//
// Mutation: read a main thread that is no regular file, or no main thread, as
// a session of no calls; count no notification beside the legs; or read a line
// past a bound as a refused line: and the reading stands.
func TestHoldingsAreHeldToTheirBounds(t *testing.T) {
	t.Parallel()
	call := hsLogged("toolu_bounds", "bounds done")
	pad := func(n int) string { // a line of n bytes, its break included
		head, tail := `{"type":"queue-operation","content":"`, `"}`
		return head + strings.Repeat("x", n-1-len(head)-len(tail)) + tail
	}
	called := func(n int) string { // a reply of n calls, whose facts pass their bound before its bytes do
		return ctAssistant(ctUUID(1), ctUUID(2), "["+strings.Repeat(`{"type":"tool_use","id":"toolu_1",`+
			`"name":"Bash","input":{}},`, n-1)+`{"type":"tool_use","id":"toolu_1","name":"Bash","input":{}}]`)
	}
	main := func(lines ...string) tarMember {
		return tarMember{h: tar.Header{Name: hsMain, Mode: 0o600, Typeflag: tar.TypeReg}, body: hsThread(lines...)}
	}
	agents := func(n int) []tarMember { // the main thread, and n subagents' metadata
		members := []tarMember{main()}
		for i := range n {
			members = append(members, tarMember{h: tar.Header{Name: claudeProjectPrefix + ctSession +
				"/subagents/agent-" + fmt.Sprintf("a%016x", i) + ".meta.json", Mode: 0o644, Typeflag: tar.TypeReg},
				body: ctMeta})
		}
		return members
	}
	legs := func(n int, notified bool) []tarMember { // a main thread of n Agent calls, one notified
		calls := make([]string, n)
		for i := range n {
			calls[i] = `{"type":"tool_use","id":"toolu_leg_` + strconv.Itoa(i) + `","name":"Agent","input":{}}`
		}
		lines := []string{ctAssistant(ctUUID(1), ctUUID(2), "["+strings.Join(calls, ",")+"]")}
		if notified {
			lines = append(lines, hsNotified(t, ctUUID(2), ctUUID(3), "toolu_leg_0"))
		}
		return []tarMember{main(lines...)}
	}
	for name, tc := range map[string]struct {
		members []tarMember
		read    bool
		bound   bool
	}{
		"subagents at their bound":                    {agents(holdingsMaxAgents), true, false},
		"subagents past their bound":                  {agents(holdingsMaxAgents + 1), false, false},
		"legs at their bound":                         {legs(holdingsMaxLegs, false), true, false},
		"legs past their bound":                       {legs(holdingsMaxLegs+1, false), false, false},
		"legs at their bound, and one's notification": {legs(holdingsMaxLegs, true), false, false},
		"a 9 MiB line of a value no reader reads":     {[]tarMember{main(pad(9 << 20))}, true, false},
		"a value past its bound": {[]tarMember{main(ctBrief(strings.Repeat("u", threadValueMax-1)))}, false,
			true},
		"facts past their bound": {[]tarMember{main(called(200_000))}, false, true},
		"a link named as the main thread": {[]tarMember{{h: tar.Header{Name: hsMain, Typeflag: tar.TypeSymlink,
			Linkname: "elsewhere.jsonl"}}}, false, false},
		"a directory named as the main thread": {[]tarMember{{h: tar.Header{Name: hsMain + "/", Mode: 0o700,
			Typeflag: tar.TypeDir}}}, false, false},
		"no main thread": {[]tarMember{{h: tar.Header{Name: claudeProjectPrefix + ctOther + ".jsonl", Mode: 0o600,
			Typeflag: tar.TypeReg}, body: hsThread()}}, false, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := hsRead(t, tarOfMembers(t, tc.members), ctSession, hsAsk(call))
			if tc.read {
				hsReadWhole(t, got, err)
				return
			}
			hsNoReading(t, got, err)
			if tc.bound && !errors.Is(err, errThreadBound) {
				t.Errorf("the reading ended with %v; want a line past its bound", err)
			}
		})
	}
}

// HOLDINGS READ ONLY THE CONTINUED SESSION'S MEMBERS, as the layer names them:
// another session's main thread, a thread deeper than the session's subagents (a
// Workflow's), `agent-.jsonl`, and the saved compaction under either name are
// passed over, so a call only they record is absent. The saved compaction holds
// the call both as a main thread and as a sidechain of no agent writes it, so a
// walk reading it as either would count it. A depth-one subagent's thread and
// metadata are read; either held twice, under two spellings, is no reading,
// since a restore keeps the last copy.
//
// Mutation: take any `projects/crewlet/<x>.jsonl` for the main thread, or read
// the saved compaction, and that call is counted; drop the check of a member
// held twice, and its archive is read.
func TestHoldingsReadOnlyTheContinuedSessionsMembers(t *testing.T) {
	t.Parallel()
	const id, out = "toolu_member", "member done"
	call := hsLogged(id, out)
	pair := hsCall(ctUUID(1), 2, id, hsBlock(t, out))
	elsewhere := hsThread(pair...)
	sidechain := func(line string) string { return ctReplace(t, line, `"isSidechain":false`, `"isSidechain":true`) }
	compaction := hsThread(append(slices.Clone(pair), sidechain(pair[0]), sidechain(pair[1]))...)
	session := claudeProjectPrefix + ctSession
	for name, tc := range map[string]struct{ member, body string }{
		"another session's main thread":          {claudeProjectPrefix + ctOther + ".jsonl", elsewhere},
		"a Workflow's thread":                    {session + "/subagents/workflows/w-1/agent-" + ctAgent + ".jsonl", elsewhere},
		"agent-.jsonl":                           {session + "/subagents/agent-.jsonl", elsewhere},
		"the saved compaction":                   {session + "/" + claudePrecompactFile, compaction},
		"the saved compaction beside the thread": {session + "." + claudePrecompactFile, compaction},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := hsRead(t, tarOf(t, map[string]string{hsMain: hsThread(), tc.member: tc.body}), ctSession,
				hsAsk(call))
			hsReadWhole(t, got, err)
			if records, named := got.Calls[id]; named {
				t.Errorf("its records %+v; want none, %s being no member the reading reads", records, tc.member)
			}
		})
	}
	t.Run("a depth-one subagent's thread and metadata", func(t *testing.T) {
		t.Parallel()
		got := hsSubagentRead(t, hsForeground(t))
		if records := got.Calls[hsSubCall]; records.Thread != ctAgent || records.Results != 1 ||
			!got.Subagents[ctAgent].Reported {
			t.Errorf("its records %+v, its subagent %+v; want them read in its subagent's thread, the subagent "+
				"reported", records, got.Subagents[ctAgent])
		}
	})
	for name, member := range map[string]string{"its thread": hsAgentThread, "its metadata": hsAgentMeta} {
		t.Run("a subagent's member held twice, "+name, func(t *testing.T) {
			t.Parallel()
			members := hsForeground(t).members()
			for _, m := range members {
				if m.h.Name == member {
					m.h.Name = "./" + member
					members = append(members, m)
					break
				}
			}
			got, err := hsRead(t, tarOfMembers(t, members), ctSession, hsAsk(hsLogged(hsSubCall, hsSubOut)))
			hsNoReading(t, got, err)
		})
	}
}

// A SESSION HOLDING AN UNPAIRED SURROGATE IS STILL READ: Claude Code writes a
// surrogate that has no pair as a `\udXXX` escape, which a text it cut inside a
// pair holds, and a box can write a byte that is no UTF-8. Each is read as the
// harness's own JSON.parse reads it, U+FFFD in its place, so the reading
// stands: those results are not the log's output, and are told, and the
// calls beside them are held.
//
// Mutation: read a result whose text does not decode strictly as no record,
// and the session is unread, every call in it told.
func TestASessionHoldingAnUnpairedSurrogateIsStillRead(t *testing.T) {
	t.Parallel()
	cut, raw, plain := hsLogged("toolu_cut", "\U0001F600 tail"), hsLogged("toolu_raw", "a\xffb"),
		hsLogged("toolu_plain", "plain done")
	var lines []string
	lines = append(lines, hsCall(ctUUID(1), 2, cut.ID, `[{"type":"text","text":"\ud83d tail"}]`)...)
	lines = append(lines, hsCall(ctUUID(3), 4, raw.ID, "[{\"type\":\"text\",\"text\":\"a\xffb\"}]")...)
	lines = append(lines, hsCall(ctUUID(5), 6, plain.ID, hsBlock(t, plain.Output))...)
	got, err := hsRead(t, hsArchive(t, hsThread(lines...)), ctSession, hsAsk(cut, raw, plain))
	hsReadWhole(t, got, err)
	for _, tc := range []struct {
		call sandbox.LoggedCall
		held bool
	}{{cut, false}, {raw, false}, {plain, true}} {
		if held, _ := got.Holds(tc.call); held != tc.held {
			t.Errorf("%s: Holds = %t, its records %+v; want %t", tc.call.ID, held, got.Calls[tc.call.ID], tc.held)
		}
	}
}

// AN ERROR QUOTES A BOX-WRITTEN NAME ONLY CUT: an archive can name a member in
// most of a mebibyte, and the reading's error is a log line's detail, so a
// name it quotes is its first line, redacted and held to what a transcript
// line quotes of a path ([memberNamed]), and the whole error stays well under
// a kibibyte.
//
// Mutation: quote the member's name whole, and the error runs to a mebibyte.
func TestAnErrorQuotesABoxWrittenNameOnlyCut(t *testing.T) {
	t.Parallel()
	name := "/" + strings.Repeat("n", 1<<20-100)
	archive := tarOfMembers(t, []tarMember{
		{h: tar.Header{Name: hsMain, Mode: 0o600, Typeflag: tar.TypeReg}, body: hsThread()},
		{h: tar.Header{Name: name, Mode: 0o600, Typeflag: tar.TypeReg, Format: tar.FormatPAX}, body: "a file\n"},
	})
	got, err := hsRead(t, archive, ctSession, hsAsk(hsLogged("toolu_name", "name done")))
	hsNoReading(t, got, err)
	if words := err.Error(); len(words) >= 1<<10 || strings.Contains(words, strings.Repeat("n", transcriptDetailLimit+1)) {
		t.Errorf("the error runs to %d bytes, %.200q…; want the name cut to %d, the whole under a kibibyte",
			len(words), words, transcriptDetailLimit)
	}
}

// THE SUBAGENT CASES read a session of ctSession's main thread and subagent
// ctAgent's thread and metadata beside it ([hsSession]), shaped as 2.1.310
// writes them: the main thread's Agent call of hsStarted and its answer, and
// the subagent's prompt, its call of hsSubCall answered with hsSubOut, and its
// last reply. Each case changes what it is about and leaves the rest
// reportable, so it turns on that alone.

const (
	// hsStarted is the call that starts ctAgent: ctMeta's, and the one
	// ctNotificationText names.
	hsStarted = "toolu_test_agent"
	// hsSent is a SendMessage call to ctAgent.
	hsSent = "toolu_test_send"
	// hsSubCall is ctAgent's call, which the bridge answered with hsSubOut.
	hsSubCall = "toolu_test_sub"
	hsSubOut  = "sub call done"
	// hsOtherAgent is a subagent of no member the cases write.
	hsOtherAgent = "a0000000000000099"
)

// hsAgentThread and hsAgentMeta are ctAgent's thread and metadata, as
// ctSession's archive names them.
var (
	hsAgentThread = claudeProjectPrefix + ctSession + "/subagents/agent-" + ctAgent + ".jsonl"
	hsAgentMeta   = claudeProjectPrefix + ctSession + "/subagents/agent-" + ctAgent + ".meta.json"
)

// hsSession is a session as its archive holds it: its main thread's lines, and
// ctAgent's thread's lines and metadata.
type hsSession struct {
	main, thread []string
	meta         string
	// noMeta leaves the metadata out of the archive; reversed writes the
	// members in the reverse of their names' order, as a box may.
	noMeta, reversed bool
}

// members is s's members, in the order its archive holds them.
func (s hsSession) members() []tarMember {
	members := []tarMember{
		{h: tar.Header{Name: hsMain, Mode: 0o600, Typeflag: tar.TypeReg}, body: ctThread(s.main...)},
		{h: tar.Header{Name: hsAgentThread, Mode: 0o600, Typeflag: tar.TypeReg}, body: ctThread(s.thread...)},
	}
	if !s.noMeta {
		members = append(members, tarMember{h: tar.Header{Name: hsAgentMeta, Mode: 0o644, Typeflag: tar.TypeReg},
			body: s.meta})
	}
	if s.reversed {
		slices.Reverse(members)
	}
	return members
}

// hsSide is record, a main thread's, as subagent ctAgent's thread writes it:
// a sidechain record of its agent.
func hsSide(t *testing.T, record string) string {
	t.Helper()
	return ctReplace(t, record, `"isSidechain":false`, `"isSidechain":true,"agentId":"`+ctAgent+`"`)
}

// hsReply is a model's reply of content, chained to parent, its stop reason
// stop, a JSON value: ctAssistant's, whose own stops for its calls.
func hsReply(t *testing.T, parent, uuid, content, stop string) string {
	t.Helper()
	return ctReplace(t, ctAssistant(parent, uuid, content), `"stop_reason":"tool_use"`, `"stop_reason":`+stop)
}

// hsFinished is a reply of text that ended its turn, chained to parent: a leg
// finished.
func hsFinished(t *testing.T, parent, uuid string) string {
	t.Helper()
	return hsReply(t, parent, uuid, `[{"type":"text","text":"done."}]`, `"end_turn"`)
}

// hsAPIError is the reply Claude Code writes itself, chained to parent, where
// its request failed for good, as 2.1.310 and 2.1.322 write one, abridged: the
// resume's own reply's shape ([ctSynthetic]: its model "<synthetic>", stopped
// at "stop_sequence"), its text the error's and its mark of an API error
// true, and none of the error's own keys beside it.
func hsAPIError(t *testing.T, parent, uuid string) string {
	t.Helper()
	return ctReplace(t, ctReplace(t, ctSynthetic(parent, uuid), "No response requested.", "API Error: 529 Overloaded"),
		`"isApiErrorMessage":false`, `"isApiErrorMessage":true`)
}

// hsCallOf is a model's call id of tool with input, a JSON object, as an
// assistant record's content.
func hsCallOf(id, tool, input string) string {
	return `[{"type":"tool_use","id":"` + id + `","name":"` + tool + `","input":` + input + `}]`
}

// The inputs of an Agent call that starts ctAgent and of a SendMessage call to
// it.
const (
	hsAgentInput = `{"description":"the subagent's task","prompt":"the subagent's task",` +
		`"subagent_type":"general-purpose"}`
	hsSendInput = `{"to":"` + ctAgent + `","message":"continue"}`
)

// hsAnswerOf is the user record answering id with text, chained to parent, its
// own toolUseResult own.
func hsAnswerOf(t *testing.T, parent, uuid, id, text, own string) string {
	t.Helper()
	return ctResult(parent, uuid, hsAnswer(id, hsBlock(t, text)), own)
}

// hsCompleted is the toolUseResult of an Agent call whose foreground subagent
// agent completed, as 2.1.310 writes it, abridged.
func hsCompleted(agent string) string {
	return `{"status":"completed","prompt":"the subagent's task","agentId":"` + agent + `","content":` +
		`[{"type":"text","text":"the subagent's report"}],"totalDurationMs":10}`
}

// hsLaunched is the toolUseResult of an Agent call that started ctAgent in the
// background.
const hsLaunched = `{"isAsync":true,"status":"async_launched","agentId":"` + ctAgent +
	`","description":"the subagent's task","prompt":"the subagent's task"}`

// hsSendResult is a SendMessage call's toolUseResult, its success the JSON
// value success.
func hsSendResult(success string) string {
	return `{"success":` + success + `,"message":"Resuming agent","resumedAgentId":"` + ctAgent + `"}`
}

// hsNotified is the main thread's task notification of ctAgent's leg leg,
// chained to parent.
func hsNotified(t *testing.T, parent, uuid, leg string) string {
	t.Helper()
	return ctReplace(t, ctNotification(parent, uuid), "<tool-use-id>"+hsStarted+"</tool-use-id>",
		"<tool-use-id>"+leg+"</tool-use-id>")
}

// hsBackgroundMeta is ctMeta of a subagent started in the background.
func hsBackgroundMeta(t *testing.T) string {
	t.Helper()
	return ctReplace(t, ctMeta, `"requestShape":"foreground"`, `"requestShape":"background"`)
}

// hsForeground is a session whose subagent's work is done and answered by its
// Agent call's own result: reportable.
func hsForeground(t *testing.T) hsSession {
	t.Helper()
	return hsSession{
		main: []string{
			ctBrief(ctUUID(1)),
			ctAssistant(ctUUID(1), ctUUID(2), hsCallOf(hsStarted, claudeToolAgent, hsAgentInput)),
			hsAnswerOf(t, ctUUID(2), ctUUID(3), hsStarted, "the subagent's report", hsCompleted(ctAgent)),
		},
		thread: []string{
			ctSubagentPrompt(ctUUID(101)),
			hsSide(t, ctAssistant(ctUUID(101), ctUUID(102), hsUse(hsSubCall))),
			hsSide(t, hsAnswerOf(t, ctUUID(102), ctUUID(103), hsSubCall, hsSubOut, hsBlock(t, hsSubOut))),
			hsSide(t, hsFinished(t, ctUUID(103), ctUUID(104))),
		},
		meta: ctMeta,
	}
}

// hsBackground is hsForeground's session with its subagent started in the
// background: its call answered at once with its launch, and its work by its
// task notification. Reportable.
func hsBackground(t *testing.T) hsSession {
	t.Helper()
	s := hsForeground(t)
	s.main = []string{
		ctBrief(ctUUID(1)),
		ctAssistant(ctUUID(1), ctUUID(2), hsCallOf(hsStarted, claudeToolAgent, hsAgentInput)),
		hsAnswerOf(t, ctUUID(2), ctUUID(3), hsStarted, "Async agent launched successfully.", hsLaunched),
		hsNotified(t, ctUUID(3), ctUUID(4), hsStarted),
	}
	s.meta = hsBackgroundMeta(t)
	return s
}

// hsResumed is hsForeground's session whose subagent the main thread then sent
// a message, answered by its task notification, which its thread holds as a
// second leg it finished. Reportable.
func hsResumed(t *testing.T) hsSession {
	t.Helper()
	s := hsForeground(t)
	s.main = append(s.main,
		ctAssistant(ctUUID(3), ctUUID(5), hsCallOf(hsSent, claudeToolSendMessage, hsSendInput)),
		hsAnswerOf(t, ctUUID(5), ctUUID(6), hsSent, `{"success":true}`, hsSendResult("true")),
		hsNotified(t, ctUUID(6), ctUUID(7), hsSent))
	s.thread = append(s.thread, ctCoordinator(ctUUID(104), ctUUID(105)),
		hsSide(t, hsFinished(t, ctUUID(105), ctUUID(106))))
	return s
}

// hsSubagentRead is a reading of s for ctAgent's call, which the bridge
// answered with hsSubOut, failing t unless the reading stands.
func hsSubagentRead(t *testing.T, s hsSession) sandbox.SessionHoldings {
	t.Helper()
	got, err := hsRead(t, tarOfMembers(t, s.members()), ctSession, hsAsk(hsLogged(hsSubCall, hsSubOut)))
	hsReadWhole(t, got, err)
	return got
}

// hsReports fails t unless reading s reports ctAgent and holds its call, or,
// where why is not "", leaves ctAgent unreported, saying why, and tells its
// call under its heading.
func hsReports(t *testing.T, s hsSession, why string) {
	t.Helper()
	got := hsSubagentRead(t, s)
	held, under := got.Holds(hsLogged(hsSubCall, hsSubOut))
	report := got.Subagents[ctAgent]
	if why == "" {
		if !held || under != "" || !report.Reported || report.Why != "" {
			t.Errorf("Holds = %t under %q, the subagent %+v, its call %+v; want it reported and its call held",
				held, under, report, got.Calls[hsSubCall])
		}
		return
	}
	if held || under != ctAgent || report.Reported || !strings.Contains(report.Why, why) {
		t.Errorf("Holds = %t under %q, the subagent %+v; want it unreported for %q, its call told under it",
			held, under, report, why)
	}
}

// The words each condition of rule 2 is reported by.
const (
	hsUnlinked   = "it is linked to no call"
	hsNotWhole   = "its thread is not whole"
	hsUnanswered = "no live answer in the main thread"
	hsUnseenLeg  = "legs of work, and the main thread holds"
	hsUnfinished = "ends on no finished reply"
)

// hsMeasured is, by agent, the calls of a measured session's subagents its
// bridge answered, as the log keeps them.
type hsMeasured map[string][]sandbox.LoggedCall

// A SUBAGENT IS REPORTED ONLY WHERE ITS ONE CONVERSATION ANSWERED AND FINISHED
// EVERY LEG (rule 2), the conversation the subagent's own load hands
// ([threadChain], newThreadChain(agent)): a call its thread holds is held only
// then, else it is told under the subagent's heading, the reading saying which
// condition failed first. Measured and reported: P2(c), P2(dc), P2(g), P2(h), T5
// and T10; not: P2(dc)'s box copy (condition 5) and P2(d) (condition 3, then the
// strict rule: its thread is not whole). The built cases each fail one condition
// of an otherwise reportable subagent (metadata, thread record, finish, leg
// start, Agent answer, notification, message) or pass one Claude Code's walk
// reads as it does (R1i/R1j, R2a-R2g, V1, V2: branches, legs, answer order).
// Mutation: drop a condition or the strict rule, and the subagent its case
// built is reported; tell every call, and R1j's and R2g's go unanswered.
func TestASubagentIsReportedOnlyWhereItsOneConversationAnsweredAndFinishedEveryLeg(t *testing.T) {
	t.Parallel()
	// conversation is hsForeground's session whose subagent's thread is what
	// lines writes, records as rcReply and its kin write them, each of its own
	// ([hsOwn]), each written at the time it states.
	conversation := func(lines func(*testing.T) []string) func(*testing.T) hsSession {
		return func(t *testing.T) hsSession {
			s := hsForeground(t)
			s.thread = hsOwn(t, lines(t)...)
			return s
		}
	}
	said, x := rcSaid("done"), rcCalls(hsSubCall)
	// asked is the call, its result and the reply that finished its leg.
	asked := func(t *testing.T) []string {
		return []string{rcPrompt(0, 1, 100), rcReply(1, 2, "msg_u", 200, x), hsSubResult(t, 2, 3, 300),
			rcReply(3, 4, "msg_f", 400, said)}
	}
	with := func(base func(*testing.T) hsSession, edit func(*testing.T, *hsSession)) func(*testing.T) hsSession {
		return func(t *testing.T) hsSession {
			s := base(t)
			edit(t, &s)
			return s
		}
	}
	// legs is hsResumed's session, two legs of ctAgent's work, whose
	// subagent's thread is what lines writes, each line ctAgent's own as it
	// stands ([rcOwn] makes one of a record rcReply and its kin write).
	legs := func(lines func(*testing.T) []string) func(*testing.T) hsSession {
		return func(t *testing.T) hsSession {
			s := hsResumed(t)
			s.thread = lines(t)
			return s
		}
	}
	own := func(t *testing.T, line string) string {
		t.Helper()
		return rcOwn(t, line)
	}
	sent := func(result string) func(*testing.T, *hsSession) {
		return func(t *testing.T, s *hsSession) {
			s.main = append(s.main, ctAssistant(ctUUID(3), ctUUID(5), hsCallOf(hsSent, claudeToolSendMessage,
				hsSendInput)))
			if result != "" {
				s.main = append(s.main, hsAnswerOf(t, ctUUID(5), ctUUID(6), hsSent, `{"success":`+result+`}`,
					hsSendResult(result)))
			}
		}
	}
	for name, tc := range map[string]struct {
		session func(*testing.T) hsSession
		why     string
	}{
		"answered by its Agent call's own result": {hsForeground, ""},
		"answered by its task notification":       {hsBackground, ""},
		"its members in the box's other order": {with(hsForeground, func(_ *testing.T, s *hsSession) {
			s.reversed = true
		}), ""},
		"sent a message its own result refused with false": {with(hsForeground, sent("false")), ""},
		"sent a message answered by its task notification": {hsResumed, ""},
		"no metadata": {with(hsForeground, func(_ *testing.T, s *hsSession) { s.noMeta = true }), hsUnlinked},
		"started by a call of another tool": {with(hsForeground, func(_ *testing.T, s *hsSession) {
			s.main[1] = ctAssistant(ctUUID(1), ctUUID(2), hsCallOf(hsStarted, "Bash", `{"command":"true"}`))
		}), hsUnlinked},
		"a restart in its thread": {with(hsForeground, func(t *testing.T, s *hsSession) {
			s.thread = append(s.thread, hsSide(t, ctMicrocompactBoundary(ctUUID(104), ctUUID(105))))
		}), hsNotWhole},
		"a progress record in its thread": {with(hsForeground, func(t *testing.T, s *hsSession) {
			s.thread = append(s.thread, hsSide(t, hsProgress(ctUUID(104), ctUUID(105))))
		}), hsNotWhole},
		"a batch's list in no shape in its thread": {with(hsForeground, func(t *testing.T, s *hsSession) {
			s.thread[3] = ctReplace(t, s.thread[3], `"type":"assistant",`,
				`"type":"assistant","batchToolUses":[7],`)
		}), hsNotWhole},
		"its prompt's parentUuid the empty string": {with(hsForeground, func(t *testing.T, s *hsSession) {
			s.thread[0] = ctReplace(t, s.thread[0], `"parentUuid":null`, `"parentUuid":""`)
		}), ""},
		"its prompt with no parentUuid": {with(hsForeground, func(t *testing.T, s *hsSession) {
			s.thread[0] = ctReplace(t, s.thread[0], `"parentUuid":null,`, "")
		}), ""},
		"its Agent call and its answer off the conversation": {with(hsForeground, func(t *testing.T,
			s *hsSession) {
			s.main = append(s.main, hsReplyOf(t, ctUUID(1), ctUUID(5), "msg_after", `[{"type":"text","text":"done."}]`))
		}), hsUnanswered},
		// R1i, [1, 2, 4, 3, 5]: the answer, recovered beside the call's reply
		// (msg_a), placed by its place in the file after another reply written
		// before it inside that reply's records (msg_a's 5 after it), so the
		// request hands it after that reply's message, not the call's.
		"its Agent call's answer behind another reply": {with(hsForeground, func(t *testing.T, s *hsSession) {
			s.main = []string{rcPrompt(0, 1, 100), rcReply(1, 2, "msg_a", 200, hsAgentCall),
				rcReply(2, 4, "msg_between", 300, said), hsAgentAnswer(t, 400), rcReply(4, 5, "msg_a", 500, said)}
		}), hsUnanswered},
		// R1j, [1, 2, 3, 4]: its control.
		"its Agent call's answer right after it": {with(hsForeground, func(t *testing.T, s *hsSession) {
			s.main = []string{rcPrompt(0, 1, 100), rcReply(1, 2, "msg_a", 200, hsAgentCall),
				hsAgentAnswer(t, 300), rcReply(3, 4, "msg_after", 400, said)}
		}), ""},
		// The request keeps the first use of an id: a call of another tool under
		// the Agent call's id before it takes the answer's place.
		"its Agent call's id a call of another tool carries first": {with(hsForeground, func(t *testing.T,
			s *hsSession) {
			s.main = []string{s.main[0],
				hsReplyOf(t, ctUUID(1), ctUUID(4), "msg_bash", hsCallOf(hsStarted, "Bash", `{"command":"true"}`)),
				ctAssistant(ctUUID(4), ctUUID(2), hsCallOf(hsStarted, claudeToolAgent, hsAgentInput)), s.main[2]}
		}), hsUnanswered},
		"a batch's calls listed in the main thread": {with(hsForeground, func(t *testing.T, s *hsSession) {
			s.main = append(s.main, ctReplace(t, hsReplyOf(t, ctUUID(3), ctUUID(5), "msg_batch",
				`[{"type":"text","text":"done."}]`), `"type":"assistant",`,
				`"type":"assistant","batchToolUses":[{"id":"toolu_test_batch","name":"Bash"}],`))
		}), hsUnanswered},
		"a result of a batch's split id in the main thread": {with(hsForeground, func(t *testing.T, s *hsSession) {
			s.main = append(s.main, hsAnswerOf(t, ctUUID(3), ctUUID(5), "toolu_test_batch_1", "split",
				hsBlock(t, "split")))
		}), hsUnanswered},
		"its notification off the conversation": {with(hsBackground, func(t *testing.T, s *hsSession) {
			s.main = append(s.main, hsReplyOf(t, ctUUID(3), ctUUID(5), "msg_after", `[{"type":"text","text":"done."}]`))
		}), hsUnanswered},
		"its first chained record roots no chain": {with(hsForeground, func(t *testing.T, s *hsSession) {
			s.thread[0] = ctReplace(t, s.thread[0], `"parentUuid":null`, `"parentUuid":"`+ctUUID(100)+`"`)
		}), hsNotWhole},
		"its last reply an empty list": {with(hsForeground, func(t *testing.T, s *hsSession) {
			s.thread[3] = hsSide(t, hsReply(t, ctUUID(103), ctUUID(104), `[]`, `"end_turn"`))
		}), hsUnfinished},
		"its last reply the text of a reply cut before its call": {with(hsForeground, func(t *testing.T, s *hsSession) {
			s.thread[3] = hsSide(t, hsReply(t, ctUUID(103), ctUUID(104),
				`[{"type":"text","text":"Now the next call."}]`, `"tool_use"`))
		}), hsUnfinished},
		"its last reply its harness's own, for a request that failed": {with(hsForeground, func(t *testing.T, s *hsSession) {
			s.thread[3] = hsSide(t, hsAPIError(t, ctUUID(103), ctUUID(104)))
		}), hsUnfinished},
		"its last reply its harness's own, a resume's": {with(hsForeground, func(t *testing.T, s *hsSession) {
			s.thread[3] = hsSide(t, ctSynthetic(ctUUID(103), ctUUID(104)))
		}), hsUnfinished},
		"its last reply ended its turn, marked an API error": {with(hsForeground, func(t *testing.T, s *hsSession) {
			s.thread[3] = hsSide(t, ctReplace(t, hsFinished(t, ctUUID(103), ctUUID(104)), `"type":"assistant"`,
				`"isApiErrorMessage":true,"type":"assistant"`))
		}), hsUnfinished},
		"its last reply cut at its token bound": {with(hsForeground, func(t *testing.T, s *hsSession) {
			s.thread[3] = hsSide(t, hsReply(t, ctUUID(103), ctUUID(104), `[{"type":"text","text":"done."}]`,
				`"max_tokens"`))
		}), hsUnfinished},
		"its last reply's mark of an API error no boolean": {with(hsForeground, func(t *testing.T, s *hsSession) {
			s.thread[3] = hsSide(t, ctReplace(t, hsFinished(t, ctUUID(103), ctUUID(104)), `"type":"assistant"`,
				`"isApiErrorMessage":"false","type":"assistant"`))
		}), hsNotWhole},
		"its last reply ended its turn, of the model <synthetic>": {with(hsForeground, func(t *testing.T, s *hsSession) {
			s.thread[3] = hsSide(t, ctReplace(t, hsFinished(t, ctUUID(103), ctUUID(104)), `"model":"claude-sonnet-4-5"`,
				`"model":"<synthetic>"`))
		}), hsUnfinished},
		"its last reply's model no string": {with(hsForeground, func(t *testing.T, s *hsSession) {
			s.thread[3] = hsSide(t, ctReplace(t, hsFinished(t, ctUUID(103), ctUUID(104)), `"model":"claude-sonnet-4-5"`,
				`"model":null`))
		}), hsNotWhole},
		"a skill's text in its thread (d4's record)": {with(hsForeground, func(t *testing.T, s *hsSession) {
			s.thread = []string{s.thread[0], s.thread[1], s.thread[2],
				hsSide(t, ctSkillText(ctUUID(103), ctUUID(110))), hsSide(t, hsFinished(t, ctUUID(110), ctUUID(104)))}
		}), hsUnseenLeg},
		"a second leg it finished, no call of it in the main thread": {with(hsForeground, func(t *testing.T, s *hsSession) {
			s.thread = append(s.thread, ctCoordinator(ctUUID(104), ctUUID(105)),
				hsSide(t, hsFinished(t, ctUUID(105), ctUUID(106))))
		}), hsUnseenLeg},
		"its Agent call's answer naming another agent": {with(hsForeground, func(t *testing.T, s *hsSession) {
			s.main[2] = hsAnswerOf(t, ctUUID(2), ctUUID(3), hsStarted, "the subagent's report", hsCompleted(hsOtherAgent))
		}), hsUnanswered},
		"its Agent call's answer, of a background subagent": {with(hsForeground, func(t *testing.T, s *hsSession) {
			s.meta = hsBackgroundMeta(t)
		}), hsUnanswered},
		"its Agent call's answer before a restart of the main thread": {with(hsForeground, func(t *testing.T, s *hsSession) {
			s.main = append(s.main, ctMicrocompactBoundary(ctUUID(3), ctUUID(4)))
		}), hsUnanswered},
		"its Agent call's answer a failure": {with(hsForeground, func(t *testing.T, s *hsSession) {
			s.main[2] = ctReplace(t, s.main[2], `"type":"tool_result"`, `"type":"tool_result","is_error":true`)
		}), hsUnanswered},
		"its Agent call answered twice": {with(hsForeground, func(t *testing.T, s *hsSession) {
			s.main = append(s.main, hsAnswerOf(t, ctUUID(2), ctUUID(5), hsStarted, "the subagent's report",
				hsCompleted(ctAgent)))
		}), hsUnanswered},
		"a second Agent call under its call's id": {with(hsForeground, func(t *testing.T, s *hsSession) {
			s.main = append(s.main, ctAssistant(ctUUID(3), ctUUID(5), hsCallOf(hsStarted, claudeToolAgent,
				hsAgentInput)))
		}), hsUnlinked},
		"started by a Task call": {with(hsForeground, func(_ *testing.T, s *hsSession) {
			s.main[1] = ctAssistant(ctUUID(1), ctUUID(2), hsCallOf(hsStarted, claudeToolTask, hsAgentInput))
		}), ""},
		"its notification of a subagent that did not complete": {with(hsBackground, func(t *testing.T, s *hsSession) {
			s.main[3] = ctReplace(t, s.main[3], "<status>completed</status>", "<status>killed</status>")
		}), hsUnanswered},
		"its notification naming another agent": {with(hsBackground, func(t *testing.T, s *hsSession) {
			s.main[3] = ctReplace(t, s.main[3], "<task-id>"+ctAgent+"</task-id>", "<task-id>"+hsOtherAgent+"</task-id>")
		}), hsUnanswered},
		"a record of another agent's in its thread": {with(hsForeground, func(t *testing.T, s *hsSession) {
			s.thread = slices.Insert(s.thread, 3, ctReplace(t, ctAttachment(ctUUID(103), ctUUID(110), ctTokens,
				ctTokensRendered), `"isSidechain":false`, `"isSidechain":true,"agentId":"`+hsOtherAgent+`"`))
		}), hsNotWhole},
		"sent a message its own result refused with null": {with(hsForeground, sent("null")), hsUnanswered},
		"sent a message with no result":                   {with(hsForeground, sent("")), hsUnanswered},
		"its conversation of its load's own records":      {conversation(asked), ""},
		// [1, 2, 3, 4, 5, 6]: 4 recovered beside msg_u, before 5.
		"two calls at once, the first's result recovered beside their reply": {conversation(func(t *testing.T) []string {
			return []string{rcPrompt(0, 1, 100), rcReply(1, 2, "msg_u", 200, x),
				rcReply(2, 3, "msg_u", 250, rcCalls("toolu_sub_y")), hsSubResult(t, 2, 4, 300),
				rcResults(3, 5, 350, "toolu_sub_y"), rcReply(5, 6, "msg_f", 400, said)}
		}), ""},
		// [1, 2, 4, 3]: 3 written before 4, its parent.
		"the reply that finished its leg written before the result it follows": {conversation(func(t *testing.T) []string {
			return []string{rcPrompt(0, 1, 100), rcReply(1, 2, "msg_u", 200, x), rcReply(4, 3, "msg_f", 400, said),
				hsSubResult(t, 2, 4, 300)}
		}), ""},
		// [1, 2, 3, 4]: 5, written last, stamped before 4, the leaf.
		"a reply that finished on a branch its call is not on": {conversation(func(t *testing.T) []string {
			return append(asked(t), rcReply(1, 5, "msg_g", 350, said))
		}), hsNotWhole},
		// [1, 5]: 5 the leaf, the call off the conversation.
		"its load's leaf a reply on a branch its call is not on": {conversation(func(t *testing.T) []string {
			return append(asked(t), rcReply(1, 5, "msg_g", 500, said))
		}), hsNotWhole},
		// [1, 2, 5, 3, 4]: 5, of msg_p, recovered beside 2; the leg ends on 4.
		"the reply that finished its leg written last, handed before its call": {conversation(func(t *testing.T) []string {
			return []string{rcPrompt(0, 1, 100), rcReply(1, 2, "msg_p", 200, rcSaid("first")),
				rcReply(2, 3, "msg_u", 300, x), hsSubResult(t, 3, 4, 400), rcReply(2, 5, "msg_p", 250, said)}
		}), hsUnfinished},
		// THE SHAPES BUILT AGAINST RULE 2, each record of its own id and time.
		// R2a, [1, 4]: 4, the latest own leaf, the leaf; 5, an own attachment
		// under the call's result, an own leaf off the conversation.
		"R2a: the reply that finished on a branch without the call, an own attachment on the call's": {
			conversation(func(t *testing.T) []string {
				return []string{rcPrompt(0, 1, 100), rcReply(1, 2, "msg_u", 200, x), hsSubResult(t, 2, 3, 300),
					rcReply(1, 4, "msg_f", 500, said), rcAttach(3, 5, 450, rcNote)}
			}), hsNotWhole},
		// R2b: R2a with an own stop hook's summary in the attachment's place.
		"R2b: as R2a, an own system record on the call's branch": {conversation(func(t *testing.T) []string {
			return []string{rcPrompt(0, 1, 100), rcReply(1, 2, "msg_u", 200, x), hsSubResult(t, 2, 3, 300),
				rcReply(1, 4, "msg_f", 500, said), rcStop(3, 5, 450)}
		}), hsNotWhole},
		// V1, [1, 2, 3, 5]: R2a with its attachment the latest own leaf.
		"V1: as R2a, the attachment the leaf": {conversation(func(t *testing.T) []string {
			return []string{rcPrompt(0, 1, 100), rcReply(1, 2, "msg_u", 200, x), hsSubResult(t, 2, 3, 300),
				rcReply(1, 4, "msg_f", 400, said), rcAttach(3, 5, 500, rcNote)}
		}), hsNotWhole},
		// V2, [1, 5]: two finished branches off the prompt, the one without
		// the call written first and the latest.
		"V2: two finished branches, the leaf the one without the call, written first": {conversation(
			func(t *testing.T) []string {
				return []string{rcPrompt(0, 1, 100), rcReply(1, 5, "msg_fb", 900, said), rcReply(1, 2, "msg_u", 200, x),
					hsSubResult(t, 2, 3, 300), rcReply(3, 4, "msg_fa", 400, said)}
			}), hsNotWhole},
		// V2b, [1, 5]: V2 with both leaves of one time, the first in the
		// file the leaf on the tie.
		"V2b: two finished branches of one time, the leaf the one without the call": {conversation(
			func(t *testing.T) []string {
				return []string{rcPrompt(0, 1, 100), rcReply(1, 5, "msg_fb", 400, said), rcReply(1, 2, "msg_u", 200, x),
					hsSubResult(t, 2, 3, 300), rcReply(3, 4, "msg_fa", 400, said)}
			}), hsNotWhole},
		// [1, 2, 3, 4, 5, 6]: the call in the first of two legs.
		"two legs of the load's own records, the call in the first": {legs(func(t *testing.T) []string {
			return []string{own(t, rcPrompt(0, 1, 100)), own(t, rcReply(1, 2, "msg_u", 200, x)),
				own(t, hsSubResult(t, 2, 3, 300)), own(t, rcReply(3, 4, "msg_f1", 400, said)),
				hsCoordinatorAt(t, 500), own(t, rcReply(5, 6, "msg_f2", 600, said))}
		}), ""},
		// [1, 2, 3, 4, 5, 7, 6]: 2.1.310's resume, the second leg's reply
		// naming the attachment its harness writes after it.
		"two legs, the second's reply before the attachment it names (2.1.310's resume)": {legs(
			func(t *testing.T) []string {
				return []string{own(t, rcPrompt(0, 1, 100)), own(t, rcReply(1, 2, "msg_u", 200, x)),
					own(t, hsSubResult(t, 2, 3, 300)), own(t, rcReply(3, 4, "msg_f1", 400, said)),
					hsCoordinatorAt(t, 500), own(t, rcReply(7, 6, "msg_f2", 600, said)),
					own(t, rcAttach(5, 7, 700, rcNote))}
			}), ""},
		// R2c, [1, 2, 3, 7]: both finished replies on a branch without the
		// call, the attachment under its result the leaf.
		"R2c: two legs, both finishing replies on a branch without the call": {legs(func(t *testing.T) []string {
			return []string{own(t, rcPrompt(0, 1, 100)), own(t, rcReply(1, 2, "msg_u", 200, x)),
				own(t, hsSubResult(t, 2, 3, 300)), own(t, rcReply(1, 4, "msg_f1", 400, said)),
				hsCoordinatorAt(t, 500), own(t, rcReply(5, 6, "msg_f2", 600, said)),
				own(t, rcAttach(3, 7, 700, rcNote))}
		}), hsNotWhole},
		// R2d, [1, 2, 3, 7]: R2c with the result written in the second leg's
		// stretch.
		"R2d: interleaved legs, the result written in the second leg's stretch": {legs(func(t *testing.T) []string {
			return []string{own(t, rcPrompt(0, 1, 100)), own(t, rcReply(1, 2, "msg_u", 200, x)),
				own(t, rcReply(1, 4, "msg_f1", 300, said)), hsCoordinatorAt(t, 400),
				own(t, hsSubResult(t, 2, 3, 500)), own(t, rcReply(5, 6, "msg_f2", 600, said)),
				own(t, rcAttach(3, 7, 700, rcNote))}
		}), hsNotWhole},
		// R2g, [1, 4, 5, 7, 2, 3, 6]: the call in the second leg of 2.1.310's
		// resume, its use under the attachment written last.
		"R2g: the call in a 2.1.310-resumed leg, the attachment its use names written last": {legs(
			func(t *testing.T) []string {
				return []string{own(t, rcPrompt(0, 1, 100)), own(t, rcReply(1, 4, "msg_f1", 200, said)),
					hsCoordinatorAt(t, 300), own(t, rcReply(7, 2, "msg_u", 400, x)),
					own(t, hsSubResult(t, 2, 3, 500)), own(t, rcReply(3, 6, "msg_f2", 600, said)),
					own(t, rcAttach(5, 7, 700, rcNote))}
			}), ""},
		// [1, 2, 3, 5, 6]: 5, of msg_f, recovered beside 6; 4, a leg's start, off it.
		"a leg's start off its conversation, under a reply its load recovers": {conversation(func(t *testing.T) []string {
			return []string{rcPrompt(0, 1, 100), rcReply(1, 2, "msg_u", 200, x), hsSubResult(t, 2, 3, 300),
				rcPrompt(1, 4, 350), rcReply(4, 5, "msg_f", 400, rcSaid("aside")), rcReply(3, 6, "msg_f", 500, said)}
		}), hsUnseenLeg},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			hsReports(t, tc.session(t), tc.why)
		})
	}
	done := func(id, output string) sandbox.LoggedCall { return hsLogged(id, output) }
	// unbridged are the measured calls of no tool of the bridge's: P2(c)'s
	// and P2(dc)'s shell calls, and P2(h)'s calls of a server it named seat.
	// Each is told, in the main list where its subagent is reported.
	unbridged := map[string]bool{"toolu_c1a": true, "toolu_c1b": true, "toolu_d2a": true, "toolu_d2b": true,
		"toolu_h_bg_echo": true, "toolu_h_fg_echo": true}
	for name, tc := range map[string]struct {
		calls hsMeasured
		why   string
	}{
		"2.1.310/G": {hsMeasured{"a0000000000000001": {done("toolu_g_bg1", "crewlet g_bg1 done")},
			"a0000000000000002": {done("toolu_g_fg1", "crewlet g_fg1 done")}}, ""},
		"2.1.310/H": {hsMeasured{
			"a0000000000000001": {done("toolu_h_bg1", "crewlet h_bg1 done"),
				done("toolu_h_bg_later", "crewlet h_bg_later done"), done("toolu_h_bg_echo", "seat h_bg_echo done")},
			"a0000000000000002": {done("toolu_h_fg1", "crewlet h_fg1 done"),
				done("toolu_h_fg_later", "crewlet h_fg_later done"), done("toolu_h_fg_echo", "seat h_fg_echo done")},
		}, ""},
		"2.1.310/C":   {hsMeasured{"a0000000000000001": {done("toolu_c1a", "c1a-done"), done("toolu_c1b", "c1b")}}, ""},
		"2.1.310/DC0": {hsMeasured{"a0000000000000001": {done("toolu_d2a", "d2a"), done("toolu_d2b", "d2b")}}, ""},
		"2.1.310/T5":  {hsMeasured{"a0000000000000001": {done("toolu_T5_s1", "crewlet T5_s1 done")}}, ""},
		"2.1.322/T5":  {hsMeasured{"a0000000000000001": {done("toolu_T5_s1", "crewlet T5_s1 done")}}, ""},
		"2.1.310/T10": {hsMeasured{"a0000000000000001": {done("toolu_T10_s1", "crewlet T10_s1 done")}}, ""},
		"2.1.322/T10": {hsMeasured{"a0000000000000001": {done("toolu_T10_s1", "crewlet T10_s1 done")}}, ""},
		"2.1.310/DCC": {hsMeasured{"a0000000000000001": {done("toolu_d2a", "d2a"), done("toolu_d2b", "d2b")}},
			hsUnfinished},
		"2.1.310/DW": {hsMeasured{"a0000000000000001": {done("toolu_d1a", "d1a"), done("toolu_d1b", "d1b")}},
			hsNotWhole},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			release, fixture, _ := strings.Cut(name, "/")
			archive, session := hsFixture(t, release, fixture)
			var ask []sandbox.LoggedCall
			for _, calls := range tc.calls {
				ask = append(ask, calls...)
			}
			got, err := hsRead(t, archive, session, hsAsk(ask...))
			hsReadWhole(t, got, err)
			for agent, calls := range tc.calls {
				report := got.Subagents[agent]
				if report.Reported != (tc.why == "") || !strings.Contains(report.Why, tc.why) {
					t.Errorf("%s: %+v; want it reported %t, saying %q", agent, report, tc.why == "", tc.why)
				}
				for _, call := range calls {
					held, under := got.Holds(call)
					want, heading := tc.why == "" && !unbridged[call.ID], agent
					if tc.why == "" {
						heading = ""
					}
					if records := got.Calls[call.ID]; held != want || records.Thread != agent ||
						(!held && under != heading) {
						t.Errorf("%s: Holds = %t under %q, its records %+v; want it held %t, in %s's thread",
							call.ID, held, under, records, want, agent)
					}
				}
			}
		})
	}
	t.Run("2.1.310/H, a reported subagent's call whose result is no text", func(t *testing.T) {
		t.Parallel()
		// THE TOOL SEARCH'S RESULT is a list of tool references, no text a
		// log's output matches: told in the main list, its subagent reported.
		archive, session := hsFixture(t, "2.1.310", "H")
		call := hsLogged("toolu_h_fg_ts", "mcp__crewlet__later")
		got, err := hsRead(t, archive, session, hsAsk(call))
		hsReadWhole(t, got, err)
		if held, under := got.Holds(call); held || under != "" || !got.Subagents["a0000000000000002"].Reported {
			t.Errorf("Holds = %t under %q, the subagent %+v; want it told in the main list, its subagent reported",
				held, under, got.Subagents["a0000000000000002"])
		}
	})
}

// hsAgentCall is the content of a reply making the Agent call hsStarted,
// which starts ctAgent.
const hsAgentCall = `[{"type":"tool_use","id":"` + hsStarted + `","name":"` + claudeToolAgent + `","input":` +
	hsAgentInput + `}]`

// hsAgentAnswer is record 3, written at ms, the result answering hsStarted
// with ctAgent's report, its completion its own toolUseResult, chained to
// record 2, which makes the call: [rcResults]'s, of the load's own records.
func hsAgentAnswer(t *testing.T, ms int) string {
	t.Helper()
	return ctReplace(t, ctReplace(t, rcResults(2, 3, ms, hsStarted), `"content":"ok"`,
		`"content":`+hsBlock(t, "the subagent's report")), `"sourceToolAssistantUUID"`,
		`"toolUseResult":`+hsCompleted(ctAgent)+`,"sourceToolAssistantUUID"`)
}

// hsCoordinatorAt is record 5, written at ms, the message ctAgent's
// coordinator sent it, which starts a leg of its work, chained to record 4,
// the reply that finished the leg before: as ctCoordinator writes it, of the
// load's own records.
func hsCoordinatorAt(t *testing.T, ms int) string {
	t.Helper()
	return ctReplace(t, ctCoordinator(rcUUID(4), rcUUID(5)), `"timestamp":"`+ctStamp+`"`,
		`"timestamp":"`+rcAt(ms)+`"`)
}

// hsAsked is a subagent's conversation of the load's own records: its
// prompt, its call of hsSubCall and the call's result, and the reply that
// finished its leg, each of its own id and time, followed by more.
func hsAsked(t *testing.T, more ...string) hsSession {
	t.Helper()
	s := hsForeground(t)
	s.thread = append(hsOwn(t, rcPrompt(0, 1, 100), rcReply(1, 2, "msg_u", 200, rcCalls(hsSubCall)),
		hsSubResult(t, 2, 3, 300), rcReply(3, 4, "msg_f", 400, rcSaid("done"))), hsOwn(t, more...)...)
	return s
}

// A REPLY ITS MODEL REFUSED LEAVES ITS SUBAGENT NOT WHOLE (rule 2): where a
// model refuses a reply, Claude Code moves the request to another model and
// takes the refused attempt's records back out of the conversation it goes on
// with, and a subagent's file records no fallback of it, so an attempt that
// stopped "refusal" lies on what its own load hands though its model went on
// without it. Its subagent is unreported, its call told under its heading;
// the same reply stopped "end_turn" leaves it reported.
//
// Mutation: read past a reply's refusal, and the refused one's subagent is
// reported.
func TestARefusedReplyLeavesItsSubagentNotWhole(t *testing.T) {
	t.Parallel()
	for stop, why := range map[string]string{"refusal": hsNotWhole, "end_turn": ""} {
		t.Run(stop, func(t *testing.T) {
			t.Parallel()
			s := hsAsked(t, ctReplace(t, rcReply(4, 5, "msg_r", 500, rcSaid("I cannot help with that.")),
				`"stop_reason":"end_turn"`, `"stop_reason":"`+stop+`"`), rcReply(5, 6, "msg_g", 600, rcSaid("done")))
			hsReports(t, s, why)
		})
	}
}

// A RETRACTION LIST IN A SUBAGENT'S THREAD LEAVES IT NOT WHOLE (rule 2): no
// load of a subagent's thread filters by one ([threadLine.Retracted]), so
// where its file names records taken back, what its model was handed is not
// what the file holds. A refused reply's fallback naming the call's record,
// or naming no record of the thread, a malformed reply's retry naming one,
// and a list in no shape Claude Code writes each leave the subagent
// unreported, its call told under its heading; the conversation without one
// is reported.
//
// Mutation: read past a subagent's retraction list, and each subagent whose
// list names a record of its own, or none, is reported.
func TestARetractionListInASubagentsThreadLeavesItNotWhole(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		more []string
		why  string
	}{
		"none": {nil, ""},
		"a refusal's fallback naming its call's record": {[]string{rcFallback(4, 5, 450, rcQuoted(2))}, hsNotWhole},
		"a refusal's fallback naming no record of its thread": {[]string{rcFallback(4, 5, 450,
			`"77777777-0000-4000-8000-000000000001"`)}, hsNotWhole},
		"a malformed reply's retry naming its call's record": {[]string{rcRetry(4, 5, 450, rcQuoted(2))},
			hsNotWhole},
		"a list in no shape Claude Code writes": {[]string{rcFallback(4, 5, 450, `1`)}, hsNotWhole},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			hsReports(t, hsAsked(t, tc.more...), tc.why)
		})
	}
}

// A CALL A SUBAGENT'S LIST NAMES BEFORE ITS FAULT IS TOLD: where a subagent's
// content-replacement record lists an asked call's result and then an entry that
// is no object, or its reply's batch lists that call and then such an entry, the
// subagent is not whole and the call, whole in the main thread, is told all the
// same. Every entry before the fault is read, as in a whole list: the harness
// reads the entries it can, so dropping them would hold a call whose result its
// model was not handed whole. A batch's entry reaches a main-thread call where
// the reading meets it before the main thread is decided, so that archive holds
// the subagent's members first, an order a box may write. A list naming no such
// entry holds the call and reports its subagent, in either order.
//
// Mutation: walk a list whole before handing an entry, and its call is held.
func TestACallASubagentsListNamesBeforeItsFaultIsTold(t *testing.T) {
	t.Parallel()
	const id, out = "toolu_test_listed", "listed done"
	call := hsLogged(id, out)
	for name, tc := range map[string]struct {
		edit func(*testing.T, *hsSession)
		held bool
	}{
		"no list names it": {func(*testing.T, *hsSession) {}, true},
		"no list names it, its subagent's members first": {func(_ *testing.T, s *hsSession) { s.reversed = true },
			true},
		"a replacement names it, then an entry that is no object": {func(_ *testing.T, s *hsSession) {
			s.thread = append(s.thread, ctReplacement(ctAgent, `{"kind":"tool-result","toolUseId":"`+id+`"}`, `7`))
		}, false},
		"a batch names it, then an entry that is no object": {func(t *testing.T, s *hsSession) {
			s.thread[1] = ctReplace(t, s.thread[1], `"apiBlockIndex"`, `"batchToolUses":[{"id":"`+id+`"},7],`+
				`"apiBlockIndex"`)
			s.reversed = true
		}, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := hsForeground(t)
			s.main = append(s.main, hsCall(ctUUID(3), 5, id, hsBlock(t, out))...)
			tc.edit(t, &s)
			got, err := hsRead(t, tarOfMembers(t, s.members()), ctSession, hsAsk(call, hsLogged(hsSubCall, hsSubOut)))
			hsReadWhole(t, got, err)
			held, under := got.Holds(call)
			report := got.Subagents[ctAgent]
			if held != tc.held || under != "" || report.Reported != tc.held {
				t.Errorf("Holds = %t under %q, the subagent %+v, its records %+v; want it held %t in the main list, "+
					"the subagent reported %t", held, under, report, got.Calls[id], tc.held, tc.held)
			}
		})
	}
}

// A LEG FINISHED WITHOUT TEXT IS UNFINISHED (rule 2, condition 5): a subagent
// answers its caller with the text of its last reply whose text says
// something, as JavaScript's trim() reads it (2.1.322's pkn through t5t,
// 2.1.310's Owr through POt), so a leg whose last reply ended its turn with
// thinking alone, or text of white space or a byte order mark alone, answers
// with an earlier reply's text, written before its call's result was. Each
// leaves its subagent unreported; text beside thinking, and a text of U+0085
// alone, which trim() keeps, finish it.
//
// Mutation: take a reply of thinking or blank text alone for finished, and
// each such leg's subagent is reported.
func TestALegFinishedWithoutTextIsUnfinished(t *testing.T) {
	t.Parallel()
	const thought = `{"type":"thinking","thinking":"done","signature":"c2ln"}`
	for name, tc := range map[string]struct {
		content, why string
	}{
		"text":                      {rcSaid("done"), ""},
		"thinking and text":         {`[` + thought + `,{"type":"text","text":"done"}]`, ""},
		"text of U+0085 alone":      {`[{"type":"text","text":"\u0085"}]`, ""},
		"thinking alone":            {`[` + thought + `]`, hsUnfinished},
		"text of white space alone": {`[{"type":"text","text":" \n\t"}]`, hsUnfinished},
		"text of U+FEFF alone":      {`[{"type":"text","text":"\ufeff"}]`, hsUnfinished},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := hsAsked(t)
			s.thread[3] = rcOwn(t, rcReply(3, 4, "msg_f", 400, tc.content))
			hsReports(t, s, tc.why)
		})
	}
}

// A TORN SUBAGENT THREAD IS NOT WHOLE (rule 2): the box cuts a subagent's torn
// last piece only where it has find, and the subagent's own load can read a
// whole value with no break after it as one more record, which the reading
// never reads. So a thread that ends in a piece with no break leaves its
// subagent unreported, its call told under its heading, though the piece is a
// whole record; the same record with its break leaves it reported.
//
// Mutation: read past a member's torn end, and the torn thread's subagent is
// reported.
func TestATornSubagentThreadIsNotWhole(t *testing.T) {
	t.Parallel()
	piece := hsSide(t, ctAttachment(ctUUID(104), ctUUID(105), ctTokens, ctTokensRendered))
	for name, tc := range map[string]struct {
		end, why string
	}{"its last record whole": {"\n", ""}, "its last record torn": {"", hsNotWhole}} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			members := hsForeground(t).members()
			members[1].body += piece + tc.end
			got, err := hsRead(t, tarOfMembers(t, members), ctSession, hsAsk(hsLogged(hsSubCall, hsSubOut)))
			hsReadWhole(t, got, err)
			held, under := got.Holds(hsLogged(hsSubCall, hsSubOut))
			report := got.Subagents[ctAgent]
			if tc.why == "" && (!held || !report.Reported) {
				t.Errorf("Holds = %t under %q, the subagent %+v; want it reported and its call held", held, under, report)
			}
			if tc.why != "" && (held || under != ctAgent || report.Reported || !strings.Contains(report.Why, tc.why)) {
				t.Errorf("Holds = %t under %q, the subagent %+v; want it unreported for %q, its call told under it",
					held, under, report, tc.why)
			}
		})
	}
}

// A SUBAGENT PAST THE CHAIN'S BOUND IS COUNTED STILL: past the bound of the
// model of its own load ([threadChainMaxRecords]: here a refusal's fallback
// naming more uuids than it files) its thread is not whole, and the reading
// reads on, so a second carrier of its call's id after that record is
// counted, and tells the call though the subagent's every other condition
// held.
//
// Mutation: stop reading a subagent's thread at the model's bound, and the
// second carrier is not counted.
func TestASubagentPastTheChainsBoundIsCountedStill(t *testing.T) {
	t.Parallel()
	named := make([]int, threadChainMaxRecords)
	for i := range named {
		named[i] = 1000 + i
	}
	s := hsAsked(t, rcFallback(4, 5, 450, rcQuoted(named...)), rcReply(5, 6, "msg_again", 500, rcCalls(hsSubCall)))
	got := hsSubagentRead(t, s)
	held, under := got.Holds(hsLogged(hsSubCall, hsSubOut))
	records, report := got.Calls[hsSubCall], got.Subagents[ctAgent]
	if held || records.Uses != 2 || records.Thread != ctAgent || report.Reported ||
		!strings.Contains(report.Why, hsNotWhole) {
		t.Errorf("Holds = %t under %q, its records %+v, the subagent %+v; want both its uses counted in its "+
			"thread past the bound, and it told under a subagent not whole", held, under, records, report)
	}
}

// AN ANSWER CARRYING A HANDBACK ANSWERS NO LEG (rule 2, condition 3). Under a
// handback contract a subagent delivers its report through its own call,
// SubagentHandback, and its completion answers its caller with the harness's
// words (2.1.322's pkn, 2.1.310's Owr): a call's answer carries a `handback`
// key, and a notification's result names the tool, in each of the three forms
// both releases write (report delivered, not reported yet, none delivered). So
// an Agent answer carrying `handback` or `handbackReport`, and a notification
// in any form, leave the subagent unreported, its call told; a leg one of
// whose notifications carried one is answered by no other.
//
// Mutation: read past a call's answer's handback or a notification's, and the
// subagent is reported; keep only the last notification's, and the one notified twice is.
func TestAnAnswerCarryingAHandbackAnswersNoLeg(t *testing.T) {
	t.Parallel()
	forms := map[string]string{
		"delivered": `This agent's report was delivered to you as a message from \"` + ctAgent +
			`\" (its SubagentHandback call). Read it there; it is not repeated here.`,
		"not reported yet": "This agent has not reported yet: it is waiting on its own background work and " +
			"will deliver its report through SubagentHandback when that finishes.",
		"none delivered": "The subagent ended without delivering a report through SubagentHandback, so no " +
			"report was delivered. Its unsent text is not shown.",
	}
	// carrying is a task notification, line, whose result is text.
	carrying := func(t *testing.T, line, text string) string {
		t.Helper()
		return ctReplace(t, line, `<status>completed</status>\n</task-notification>`,
			`<status>completed</status>\n<result>`+text+`</result>\n</task-notification>`)
	}
	answered := func(key string) func(*testing.T) hsSession {
		return func(t *testing.T) hsSession {
			s := hsForeground(t)
			s.main[2] = hsAnswerOf(t, ctUUID(2), ctUUID(3), hsStarted, "the subagent's report",
				strings.Replace(hsCompleted(ctAgent), `"status":"completed",`, `"status":"completed",`+key+`,`, 1))
			return s
		}
	}
	cases := map[string]struct {
		session func(*testing.T) hsSession
		why     string
	}{
		"its call's own answer":                   {hsForeground, ""},
		"its call's own answer carrying handback": {answered(`"handback":"withheld"`), hsUnanswered},
		"its call's own answer carrying handbackReport": {answered(`"handbackReport":"the report"`),
			hsUnanswered},
		"its notification":             {hsBackground, ""},
		"a SendMessage's notification": {hsResumed, ""},
		"its notification carrying none, after one carrying one": {func(t *testing.T) hsSession {
			s := hsBackground(t)
			s.main = append(s.main[:3], carrying(t, hsNotified(t, ctUUID(3), ctUUID(5), hsStarted), forms["delivered"]),
				hsNotified(t, ctUUID(5), ctUUID(4), hsStarted))
			return s
		}, hsUnanswered},
	}
	for form, text := range forms {
		cases["its notification carrying one, "+form] = struct {
			session func(*testing.T) hsSession
			why     string
		}{func(t *testing.T) hsSession {
			s := hsBackground(t)
			s.main[3] = carrying(t, s.main[3], text)
			return s
		}, hsUnanswered}
		cases["a SendMessage's notification carrying one, "+form] = struct {
			session func(*testing.T) hsSession
			why     string
		}{func(t *testing.T) hsSession {
			s := hsResumed(t)
			s.main[5] = carrying(t, s.main[5], text)
			return s
		}, hsUnanswered}
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			hsReports(t, tc.session(t), tc.why)
		})
	}
}

// A NOTICE BEFORE THE LAST RESTART ANSWERS NO LEG (rule 2, condition 3): what a
// resumed session is handed starts over at a restart, so a task notification
// counts only where its line follows the main thread's last one, as a result
// does ([TestAResultBeforeTheLastRestartIsNotLive]). This is the reading's own
// forward guard: a release after 2.1.322 that cut at a microcompaction's
// boundary would hand less. A notification before a boundary, or between two,
// leaves its subagent unreported, its call told; one after the last answers.
//
// Mutation: hold a notification to no restart, and the one before the boundary
// answers its leg; compare with the first restart, and the one between two does.
func TestANoticeBeforeTheLastRestartAnswersNoLeg(t *testing.T) {
	t.Parallel()
	// restarted is hsBackground's session whose main thread goes on from its
	// launch (3) with lines.
	restarted := func(lines ...string) func(*testing.T) hsSession {
		return func(t *testing.T) hsSession {
			s := hsBackground(t)
			s.main = append(s.main[:3:3], lines...)
			return s
		}
	}
	for name, tc := range map[string]struct {
		session func(*testing.T) hsSession
		why     string
	}{
		"its notification": {hsBackground, ""},
		"its notification before a restart": {restarted(hsNotified(t, ctUUID(3), ctUUID(4), hsStarted),
			ctMicrocompactBoundary(ctUUID(4), ctUUID(5))), hsUnanswered},
		"its notification between two restarts": {restarted(ctMicrocompactBoundary(ctUUID(3), ctUUID(10)),
			hsNotified(t, ctUUID(10), ctUUID(4), hsStarted), ctMicrocompactBoundary(ctUUID(4), ctUUID(11))),
			hsUnanswered},
		"its notification after the last restart": {restarted(ctMicrocompactBoundary(ctUUID(3), ctUUID(10)),
			hsNotified(t, ctUUID(10), ctUUID(4), hsStarted)), ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			hsReports(t, tc.session(t), tc.why)
		})
	}
}

// A CALL WHOSE RECORDS LIE IN TWO THREADS IS TOLD, in the main list, wherever
// its use lies and wherever its result: a thread's result pairs with a call of
// its own thread alone, so a use in the main thread and its result in a
// subagent's, or the reverse, is no call a model was handed whole with its
// result, and a whole pair in each, as a gateway that repeats ids would
// leave, is two calls' records under one id. Each is counted in both threads,
// two of them and no thread named, though every condition of its subagent
// holds.
//
// Mutation: count a call's threads as one however many hold its records, and
// each reads as the records of one thread.
func TestACallsRecordsInTwoThreadsAreTold(t *testing.T) {
	t.Parallel()
	for name, edit := range map[string]func(*testing.T, *hsSession){
		"its use in the main thread, its result in a subagent's": func(t *testing.T, s *hsSession) {
			s.main = append(s.main, ctAssistant(ctUUID(3), ctUUID(5), hsUse(hsSubCall)))
			s.thread = []string{s.thread[0], ctReplace(t, s.thread[2], `"parentUuid":"`+ctUUID(102)+`"`,
				`"parentUuid":"`+ctUUID(101)+`"`), s.thread[3]}
		},
		"its use in a subagent's thread, its result in the main thread": func(t *testing.T, s *hsSession) {
			s.main = append(s.main, hsAnswerOf(t, ctUUID(3), ctUUID(5), hsSubCall, hsSubOut, hsBlock(t, hsSubOut)))
			s.thread = []string{s.thread[0], s.thread[1], ctReplace(t, s.thread[3], `"parentUuid":"`+ctUUID(103)+`"`,
				`"parentUuid":"`+ctUUID(102)+`"`)}
		},
		"a whole pair in each": func(t *testing.T, s *hsSession) {
			s.main = append(s.main, hsCall(ctUUID(3), 5, hsSubCall, hsBlock(t, hsSubOut))...)
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := hsForeground(t)
			edit(t, &s)
			got := hsSubagentRead(t, s)
			records := got.Calls[hsSubCall]
			held, under := got.Holds(hsLogged(hsSubCall, hsSubOut))
			if held || under != "" || records.Threads != 2 || records.Thread != "" || !got.Subagents[ctAgent].Reported {
				t.Errorf("Holds = %t under %q, its records %+v, the subagent %+v; want its records counted in two "+
					"threads, no thread named, and it told in the main list, its subagent reported", held, under,
					records, got.Subagents[ctAgent])
			}
		})
	}
}

// A TASK NOTIFICATION IS READ LINE BY LINE, as Claude Code writes one and as
// probe T's T10 measured it on both releases: header; the subagent's id (an
// agent's by the layer's rule); the leg's (one the call-id rule holds); any
// number of output files; a status of completed, nothing read after it. Any
// other text names no leg: another header, an id outside its rule or in the
// other order, a stray line before the status, another status, or more on a
// line. Read as the walk reads one: a user record's content, through
// [threadReader.Next] and [threadMessage.Notification].
//
// Mutation: read any status as completed, a non-output-file line past, or a leg
// outside the call-id rule, and another shape answers a leg; leave the last
// line undecided, and a notification ending at its status names none.
func TestATaskNotificationIsReadLineByLine(t *testing.T) {
	t.Parallel()
	const head, done = "<task-notification>\n", "<status>completed</status>"
	ids := "<task-id>" + ctAgent + "</task-id>\n<tool-use-id>" + hsStarted + "</tool-use-id>\n"
	files := "<output-file>/tmp/box/" + ctAgent + ".output</output-file>\n<output-file>/tmp/box/b.output</output-file>\n"
	swapped := "<tool-use-id>" + hsStarted + "</tool-use-id>\n<task-id>" + ctAgent + "</task-id>\n"
	long := strings.Repeat("t", sandbox.MaxBridgeCallIDBytes+1)
	for name, tc := range map[string]struct {
		text string
		ok   bool
	}{
		"as written":                      {head + ids + done + "\n</task-notification>", true},
		"with its output files, as T10":   {head + ids + files + done + "\n<summary>Agent finished</summary>", true},
		"ending at its status":            {head + ids + done, true},
		"no header":                       {ids + done, false},
		"a header with more on its line":  {"<task-notification> \n" + ids + done, false},
		"an agent outside the agent rule": {head + strings.Replace(ids, ctAgent, "a b", 1) + done, false},
		"a leg outside the call-id rule":  {head + strings.Replace(ids, hsStarted, long, 1) + done, false},
		"its ids in the other order":      {head + swapped + done, false},
		"a line between ids and status":   {head + ids + "<summary>Agent finished</summary>\n" + done, false},
		"a status other than completed":   {head + ids + "<status>killed</status>", false},
		"a status with more on its line":  {head + ids + done + " ", false},
	} {
		record := ctReplace(t, ctNotification(ctUUID(1), ctUUID(3)),
			`"content":"`+strings.ReplaceAll(ctNotificationText, "\n", `\n`)+`"`, `"content":`+jsonString(t, tc.text))
		m, err := ctLine(t, record).Message()
		if err != nil {
			t.Errorf("%s reads no message: %v", name, err)
			continue
		}
		agent, leg, ok := m.Notification()
		if ok != tc.ok || (ok && (agent != ctAgent || leg != hsStarted)) {
			t.Errorf("%s reads as %t, naming %q and %q; want %t, naming %q and %q", name, ok, agent, leg, tc.ok,
				ctAgent, hsStarted)
		}
	}
}

// A SUBAGENT LINE THAT IS NO RECORD LEAVES IT UNREPORTED, AND THE READING
// STANDS: a whole line that is not one record, a counted record whose message
// does not decode, and a reply whose stop reason is null each leave its thread
// not whole, so its calls are told under its heading; and its thread is read on
// past such a line, so a call recorded after it is still counted, in that
// thread. Measured: P2(d)'s box copy whose subagent's torn last piece was left
// uncut, and its next record written onto it (line 25).
//
// Mutation: stop reading a subagent's thread at a line that is no record, and
// the call after it is absent; or read past a stop reason that does not
// decode, and the null one's subagent is reported.
func TestASubagentLineThatIsNoRecordLeavesItUnreported(t *testing.T) {
	t.Parallel()
	for name, edit := range map[string]func(*testing.T, *hsSession){
		"a line of half a record before its call": func(_ *testing.T, s *hsSession) {
			s.thread = slices.Insert(s.thread, 1, `{"parentUuid":"`+ctUUID(101)+`","isSidechain":true,"agentId":"`+
				ctAgent+`","type":"assistant","message":{"role":`)
		},
		"a record whose message does not decode, before its call": func(t *testing.T, s *hsSession) {
			s.thread = slices.Insert(s.thread, 1, hsSide(t, ctResult(ctUUID(101), ctUUID(109), `42`, `42`)))
		},
		"its last reply's stop reason null": func(t *testing.T, s *hsSession) {
			s.thread[3] = hsSide(t, hsReply(t, ctUUID(103), ctUUID(104), `[{"type":"text","text":"done."}]`, `null`))
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := hsForeground(t)
			edit(t, &s)
			got := hsSubagentRead(t, s)
			records, report := got.Calls[hsSubCall], got.Subagents[ctAgent]
			held, under := got.Holds(hsLogged(hsSubCall, hsSubOut))
			if records.Thread != ctAgent || records.Uses != 1 || records.Results != 1 || held || under != ctAgent ||
				report.Reported || !strings.Contains(report.Why, hsNotWhole) {
				t.Errorf("Holds = %t under %q, its records %+v, the subagent %+v; want its records counted in its "+
					"thread, and it told under a subagent not whole", held, under, records, report)
			}
		})
	}
	t.Run("2.1.310/DT, a torn piece written onto", func(t *testing.T) {
		t.Parallel()
		archive, session := hsFixture(t, "2.1.310", "DT")
		call := hsLogged("toolu_d1a", "d1a")
		got, err := hsRead(t, archive, session, hsAsk(call))
		hsReadWhole(t, got, err)
		report := got.Subagents["a0000000000000001"]
		if held, under := got.Holds(call); held || under != "a0000000000000001" || report.Reported ||
			!strings.Contains(report.Why, "line 25 of its thread is no record") {
			t.Errorf("Holds = %t under %q, the subagent %+v; want it told under a subagent whose line 25 is no "+
				"record", held, under, report)
		}
	})
}

// A RESULT ANSWERS ONLY A LEG ALREADY READ: a leg's fact exists once the main
// thread's call of it was read, and an answer read before it, its call's
// result or a task notification of it, answers nothing and is kept nowhere.
// Every measured thread writes an answer after its call; the same answer after
// it reports its subagent, and before it leaves the subagent unreported, the
// safe direction.
//
// Mutation: file an answer under a leg it creates itself, whatever was read
// before it, and the answer before its call reports its subagent.
func TestAResultAnswersOnlyALegAlreadyRead(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		session func(*testing.T) hsSession
		why     string
	}{
		"its call's answer after its call": {hsForeground, ""},
		"its call's answer before its call": {func(t *testing.T) hsSession {
			s := hsForeground(t)
			s.main = []string{s.main[0],
				hsAnswerOf(t, ctUUID(2), ctUUID(3), hsStarted, "the subagent's report", hsCompleted(ctAgent)),
				ctAssistant(ctUUID(1), ctUUID(2), hsCallOf(hsStarted, claudeToolAgent, hsAgentInput))}
			return s
		}, hsUnanswered},
		"its notification after its call": {hsBackground, ""},
		"its notification before its call": {func(t *testing.T) hsSession {
			s := hsBackground(t)
			s.main = []string{s.main[0], hsNotified(t, ctUUID(1), ctUUID(4), hsStarted),
				ctAssistant(ctUUID(4), ctUUID(2), hsCallOf(hsStarted, claudeToolAgent, hsAgentInput)),
				hsAnswerOf(t, ctUUID(2), ctUUID(3), hsStarted, "Async agent launched successfully.", hsLaunched)}
			return s
		}, hsUnanswered},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			hsReports(t, tc.session(t), tc.why)
		})
	}
}

// AN ANSWER IS READ ONLY OFF ITS OWN RESULT RECORD: a record's toolUseResult is
// the record's, and is one call's only where the record holds that one result
// alone, as every record measured that carries one does. An Agent call's
// completed answer on a record of its result alone reports its subagent; the
// same answer on a record holding another call's result too answers nothing.
//
// Mutation: take a toolUseResult for every call its record names, and the
// shared record's answer reports the subagent.
func TestAnAnswerIsReadOnlyOffItsOwnResultRecord(t *testing.T) {
	t.Parallel()
	t.Run("its record holding its result alone", func(t *testing.T) {
		t.Parallel()
		hsReports(t, hsForeground(t), "")
	})
	t.Run("its record holding another call's result too", func(t *testing.T) {
		t.Parallel()
		s := hsForeground(t)
		both := `[{"type":"tool_use","id":"` + hsStarted + `","name":"Agent","input":` + hsAgentInput + `},` +
			`{"type":"tool_use","id":"toolu_test_other","name":"mcp__crewlet__slow","input":{}}]`
		results := `[{"tool_use_id":"` + hsStarted + `","type":"tool_result","content":` +
			hsBlock(t, "the subagent's report") + `},{"tool_use_id":"toolu_test_other","type":"tool_result",` +
			`"content":` + hsBlock(t, "other done") + `}]`
		s.main = []string{s.main[0], ctAssistant(ctUUID(1), ctUUID(2), both),
			ctResult(ctUUID(2), ctUUID(3), results, hsCompleted(ctAgent))}
		hsReports(t, s, hsUnanswered)
	})
}

// A METADATA LINKS ITS SUBAGENT ONLY THROUGH ITS EXACT KEYS: one JSON object,
// each key matched as written and a key written twice read by its last value,
// `toolUseId` an id the log keeps, `spawnDepth` the integer 1 as JSON writes
// it, `requestShape` a string and `description` a string or absent, within
// [holdingsMetaMaxBytes]. Any other is linked to no call, and its subagent's
// calls are told under its heading.
//
// Mutation: match the metadata's keys by case, or read its depth by its value
// whatever its type, and a variant links its subagent.
func TestAMetaLinksOnlyThroughItsExactKeys(t *testing.T) {
	t.Parallel()
	meta := func(t *testing.T, from, to string) string {
		t.Helper()
		return ctReplace(t, ctMeta, from, to)
	}
	for name, tc := range map[string]struct {
		meta func(*testing.T) string
		why  string
	}{
		"as written": {func(*testing.T) string { return ctMeta }, ""},
		"its call named twice, its own last": {func(*testing.T) string {
			return `{"toolUseId":"toolu_test_other",` + ctMeta[1:]
		}, ""},
		"its call named twice, its own first": {func(*testing.T) string {
			return ctMeta[:len(ctMeta)-1] + `,"toolUseId":"toolu_test_other"}`
		}, hsUnlinked},
		"its call's key in another case": {func(t *testing.T) string {
			return meta(t, `"toolUseId"`, `"TOOLUSEID"`)
		}, hsUnlinked},
		"its call's id past the log's bound": {func(t *testing.T) string {
			return meta(t, `"toolUseId":"`+hsStarted+`"`, `"toolUseId":"`+strings.Repeat("t", 65)+`"`)
		}, hsUnlinked},
		"its call's id outside the log's rule": {func(t *testing.T) string {
			return meta(t, `"toolUseId":"`+hsStarted+`"`, `"toolUseId":"toolu test agent"`)
		}, hsUnlinked},
		"its depth as a string": {func(t *testing.T) string { return meta(t, `"spawnDepth":1`, `"spawnDepth":"1"`) }, hsUnlinked},
		"a depth of 2":          {func(t *testing.T) string { return meta(t, `"spawnDepth":1`, `"spawnDepth":2`) }, hsUnlinked},
		"a depth of 1.0":        {func(t *testing.T) string { return meta(t, `"spawnDepth":1`, `"spawnDepth":1.0`) }, hsUnlinked},
		"a depth of null":       {func(t *testing.T) string { return meta(t, `"spawnDepth":1`, `"spawnDepth":null`) }, hsUnlinked},
		"no request shape":      {func(t *testing.T) string { return meta(t, `"requestShape":"foreground",`, ``) }, hsUnlinked},
		"a description of null": {func(t *testing.T) string { return meta(t, `"description":"the subagent's task"`, `"description":null`) }, hsUnlinked},
		"two values":            {func(*testing.T) string { return ctMeta + ctMeta }, hsUnlinked},
		"text the scrub kept":   {func(*testing.T) string { return "the subagent's task" }, hsUnlinked},
		"null":                  {func(*testing.T) string { return "null" }, hsUnlinked},
		"past its bound": {func(t *testing.T) string {
			return meta(t, `"finished":true`, `"finished":true,"pad":"`+strings.Repeat("p", holdingsMetaMaxBytes)+`"`)
		}, hsUnlinked},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := hsForeground(t)
			s.meta = tc.meta(t)
			hsReports(t, s, tc.why)
		})
	}
}

// EVERY KEY THE READING DECODES IS EXACT, AND A KEY WRITTEN TWICE IS READ BY
// ITS LAST VALUE, as Claude Code's own JSON.parse reads them: an answer's
// `status` and `agentId` and a refusal's `success` written in another case are
// passed over, beside the key or alone; a `status` written twice is read as
// its last; and a SendMessage's `to` in another case sends to no subagent, so
// the leg it started in its thread is one the main thread holds no call of,
// while a `to` written twice sends to its last.
//
// Mutation: match a key by case, as encoding/json matches a struct's fields,
// and each variant is read as the key.
func TestEveryKeyTheReadingDecodesIsExactAndLastWins(t *testing.T) {
	t.Parallel()
	answered := func(own string) func(*testing.T) hsSession {
		return func(t *testing.T) hsSession {
			s := hsForeground(t)
			s.main[2] = hsAnswerOf(t, ctUUID(2), ctUUID(3), hsStarted, "the subagent's report", own)
			return s
		}
	}
	sentTo := func(input, success string) func(*testing.T) hsSession {
		return func(t *testing.T) hsSession {
			s := hsResumed(t)
			s.main[3] = ctAssistant(ctUUID(3), ctUUID(5), hsCallOf(hsSent, claudeToolSendMessage, input))
			s.main[4] = hsAnswerOf(t, ctUUID(5), ctUUID(6), hsSent, `{"success":true}`, success)
			return s
		}
	}
	refused := func(success string) func(*testing.T) hsSession {
		return func(t *testing.T) hsSession {
			s := hsForeground(t)
			s.main = append(s.main,
				ctAssistant(ctUUID(3), ctUUID(5), hsCallOf(hsSent, claudeToolSendMessage, hsSendInput)),
				hsAnswerOf(t, ctUUID(5), ctUUID(6), hsSent, `{"success":false}`, success))
			return s
		}
	}
	for name, tc := range map[string]struct {
		session func(*testing.T) hsSession
		why     string
	}{
		"an answer's status and agent beside the same keys in another case": {answered(`{"status":"completed",` +
			`"Status":"async_launched","agentId":"` + ctAgent + `","AgentId":"` + hsOtherAgent + `"}`), ""},
		"an answer's status and agent in another case alone": {answered(`{"Status":"completed","AgentId":"` +
			ctAgent + `"}`), hsUnanswered},
		"an answer's status written twice, completed last": {answered(`{"status":"async_launched",` +
			`"status":"completed","agentId":"` + ctAgent + `"}`), ""},
		"an answer's status written twice, completed first": {answered(`{"status":"completed",` +
			`"status":"async_launched","agentId":"` + ctAgent + `"}`), hsUnanswered},
		"a refusal's success in another case":           {refused(`{"Success":false}`), hsUnanswered},
		"a refusal's success written twice, false last": {refused(`{"success":true,"success":false}`), ""},
		"a send's to in another case": {sentTo(`{"To":"`+ctAgent+`","message":"continue"}`,
			hsSendResult("true")), hsUnseenLeg},
		"a send's to written twice, the subagent last": {sentTo(`{"to":"`+hsOtherAgent+`","to":"`+ctAgent+
			`","message":"continue"}`, hsSendResult("true")), ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			hsReports(t, tc.session(t), tc.why)
		})
	}
}

// EVERY THREAD OF A SESSION IS READ THROUGH ONE READER ([holdingsWalk.reader]):
// the walk reads its main thread and each subagent's through the one line
// reader it made for the first, reset for each, so a session of a thousand
// subagents costs the walk one reader's buffers, never one a member; and each
// thread is still read whole, the reading standing.
//
// Mutation: make a reader for each member, and the walk reads its two threads
// through two.
func TestEveryThreadOfASessionIsReadThroughOneReader(t *testing.T) {
	t.Parallel()
	w := newHoldingsWalk(ctSession, hsAsk(hsLogged(hsSubCall, hsSubOut)), hsBounds())
	var readers []*threadReader
	if err := eachMember(bytes.NewReader(tarOfMembers(t, hsResumed(t).members())), hsBounds(),
		func(h *tar.Header, member io.Reader) error {
			err := w.member(t.Context(), h, member)
			if w.lines != nil && !slices.Contains(readers, w.lines) {
				readers = append(readers, w.lines)
			}
			return err
		}); err != nil {
		t.Fatalf("reading the session: %v", err)
	}
	if !w.read[hsMain] || !w.read[hsAgentThread] || len(readers) != 1 {
		t.Errorf("the walk read its main thread %t and its subagent's %t through %d readers; want both read "+
			"through one", w.read[hsMain], w.read[hsAgentThread], len(readers))
	}
}

// THE LINES' BOUND IS THE READING'S, SHARED BY ITS THREADS
// ([holdingsWalk.reader]): each thread member the walk reads takes what the
// ones before it left of [threadLinesMax], as their bytes share
// [threadPlainMax], so a session of many subagents reads one bound's lines,
// never one a thread. A session whose main thread and subagent's thread hold
// the bound between them reads whole; with one line fewer left, the
// subagent's thread ends the reading with errThreadBound.
//
// Mutation: hand each member the whole bound, and the session past it reads
// whole.
func TestEveryThreadOfASessionSharesTheLinesBound(t *testing.T) {
	t.Parallel()
	s := hsResumed(t)
	lines := len(s.main) + len(s.thread)
	for name, tc := range map[string]struct {
		left int
		past bool
	}{
		"the bound its threads hold between them": {left: lines},
		"one line fewer left":                     {left: lines - 1, past: true},
	} {
		w := newHoldingsWalk(ctSession, hsAsk(hsLogged(hsSubCall, hsSubOut)), hsBounds())
		w.linesLeft = tc.left
		err := eachMember(bytes.NewReader(tarOfMembers(t, s.members())), hsBounds(),
			func(h *tar.Header, member io.Reader) error { return w.member(t.Context(), h, member) })
		switch {
		case tc.past && !errors.Is(err, errThreadBound):
			t.Errorf("%s: the walk ended with %v; want the lines' bound", name, err)
		case !tc.past && (err != nil || !w.read[hsMain] || !w.read[hsAgentThread]):
			t.Errorf("%s: the walk ended with %v, its main thread read %t and its subagent's %t; want both read "+
				"whole", name, err, w.read[hsMain], w.read[hsAgentThread])
		}
	}
}

// NO ID THE CALL-ID RULE REFUSES IS KEPT: an id is the box's word, as long as a
// line, and nothing can link, answer or notice a call under one, so a leg filed
// under it would hold the box's bytes until the walk ends, a hundred mebibytes
// of them in a hundred lines. An Agent call under an id a mebibyte long and a
// Task call under one holding a space are kept as nothing; a SendMessage under
// an id one byte past the rule's bound keeps the subagent it was sent to alone;
// a call under an id the rule holds is kept as its leg; and a metadata naming
// the call that started its subagent by an id most of the metadata's bound
// long keeps no id at all.
//
// Mutation: file a leg under its call's id whatever it is, or keep the id of a
// metadata that links nothing, and the walk holds the box's id.
func TestNoIDTheCallIDRuleRefusesIsKept(t *testing.T) {
	t.Parallel()
	w := newHoldingsWalk(ctSession, hsAsk(hsLogged(hsSubCall, hsSubOut)), hsBounds())
	for _, call := range []threadCall{
		{Kind: blockToolUse, ID: "toolu_" + strings.Repeat("l", 1<<20), Name: claudeToolAgent},
		{Kind: blockToolUse, ID: "toolu test task", Name: claudeToolTask},
		{Kind: blockToolUse, ID: strings.Repeat("s", sandbox.MaxBridgeCallIDBytes+1), Name: claudeToolSendMessage,
			To: sendTarget(json.RawMessage(hsSendInput))},
		{Kind: blockToolUse, ID: hsStarted, Name: claudeToolAgent},
	} {
		if err := w.leg(call, 0); err != nil {
			t.Fatalf("filing a call of %s: %v", call.Name, err)
		}
	}
	longest := 0
	for id := range w.legs {
		longest = max(longest, len(id))
	}
	if len(w.legs) != 1 || w.legs[hsStarted] == nil || len(w.unnamed) != 1 || !w.unnamed[ctAgent] || w.kept != 2 ||
		longest > sandbox.MaxBridgeCallIDBytes {
		t.Errorf("the walk keeps %d legs, the longest id %d bytes, %t of them %s's, and %d subagents sent a message "+
			"under an id the rule refuses, %t of them %s, %d in all; want the one leg of %s, and %s alone, 2 in all",
			len(w.legs), longest, w.legs[hsStarted] != nil, hsStarted, len(w.unnamed), w.unnamed[ctAgent], ctAgent,
			w.kept, hsStarted, ctAgent)
	}
	long := strings.Repeat("t", holdingsMetaMaxBytes-1<<10)
	meta, why := subagentMetaOf([]byte(ctReplace(t, ctMeta, `"toolUseId":"`+hsStarted+`"`, `"toolUseId":"`+long+`"`)))
	if meta.call != "" || meta.description != "the subagent's task" || !strings.Contains(why, "no id the log keeps") {
		t.Errorf("a metadata naming its call by %d bytes keeps a call of %d bytes and the description %q, saying "+
			"%.80q; want no call, its description, and that it names no id the log keeps", len(long), len(meta.call),
			meta.description, why)
	}
}

// A SUBAGENT SENT A MESSAGE UNDER AN ID THE LOG KEEPS NONE OF IS NOT REPORTED:
// no notification can name such a leg, so nothing in the main thread can
// answer it, and its result is filed under that id too, so not even a refusal
// of it is read. Its subagent's calls are told under its heading, whatever that
// result says, and its thread recording no second leg; and what it says why
// quotes no such id: a send under an id one byte past the rule's bound, one
// holding a space and one as long as a value the layer reads whole
// ([threadValueMax]) each leave a few words of the reading's own.
//
// Mutation: read such a send as no leg at all, and its subagent is reported;
// or file it under its id, and the subagent's why quotes the long id.
func TestASubagentSentAMessageUnderAnIDTheLogKeepsNoneOfIsNotReported(t *testing.T) {
	t.Parallel()
	for name, id := range map[string]string{
		"an id past the rule's bound": strings.Repeat("s", sandbox.MaxBridgeCallIDBytes+1),
		"an id holding a space":       "toolu test send",
		"an id at the value bound":    "toolu_" + strings.Repeat("m", threadValueMax-len(`"toolu_"`)),
	} {
		for outcome, success := range map[string]string{"that its result took": "true", "that its result refused": "false"} {
			t.Run(name+", "+outcome, func(t *testing.T) {
				t.Parallel()
				s := hsForeground(t)
				s.main = append(s.main,
					ctAssistant(ctUUID(3), ctUUID(5), hsCallOf(id, claudeToolSendMessage, hsSendInput)),
					hsAnswerOf(t, ctUUID(5), ctUUID(6), id, `{"success":`+success+`}`, hsSendResult(success)))
				hsReports(t, s, hsUnanswered)
				if why := hsSubagentRead(t, s).Subagents[ctAgent].Why; len(why) > 1<<8 || strings.Contains(why, id) {
					t.Errorf("its subagent's why is %d bytes, %.80q, quoting the send's id %t; want a few words of "+
						"the reading's own", len(why), why, strings.Contains(why, id))
				}
			})
		}
	}
}

// WHAT MEMBERS HAND OUT IS HELD TO THE ARCHIVE'S BUDGET, the one its plaintext
// is held to, apart from what its stream inflates to: a sparse member hands out
// holes its stream does not hold. Under a one-MiB bound, a subagent's thread (a
// whole first line, a 600 KiB hole, a break; each line under the line bound, the
// stream a few KiB) is read; two hand out past the budget and the second is no
// reading, the layer's bound named. A metadata of holes at its own bound, after
// a thread whose 1000 KiB hole left less than that, is no reading either.
//
// Mutation: hand each member the whole bound, and both threads are read; or
// read a metadata past what is left, and the one after the long thread is read.
func TestWhatMembersHandOutIsHeldToTheArchivesBudget(t *testing.T) {
	t.Parallel()
	bounds := scrubBounds{plain: 1 << 20}
	thread := func(agent string, hole int) []byte {
		first := ctSubagentPrompt(ctUUID(101)) + "\n"
		return sparseMember(claudeProjectPrefix+ctSession+"/subagents/agent-"+agent+".jsonl",
			int64(len(first)+hole+1), sparseSegment{0, first}, sparseSegment{int64(len(first) + hole), "\n"})
	}
	main := plainMember(hsMain, hsThread())
	meta := sparseMember(hsAgentMeta, holdingsMetaMaxBytes, sparseSegment{0, "{"})
	for name, tc := range map[string]struct {
		members [][]byte
		read    bool
		bound   error
	}{
		"one such thread":                    {[][]byte{main, thread(ctAgent, 600<<10)}, true, nil},
		"two such threads":                   {[][]byte{main, thread(ctAgent, 600<<10), thread("a0000000000000002", 600<<10)}, false, errThreadBound},
		"a metadata past what a thread left": {[][]byte{main, thread(ctAgent, 1000<<10), meta}, false, nil},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := hsReadFrom(t.Context(), bytes.NewReader(sparseTar(t, tc.members...)), ctSession,
				hsAsk(hsLogged("toolu_budget", "budget done")), bounds)
			if tc.read {
				hsReadWhole(t, got, err)
				return
			}
			hsNoReading(t, got, err)
			if tc.bound != nil && !errors.Is(err, tc.bound) {
				t.Errorf("the reading ended with %v; want a member past what is left of the budget", err)
			}
		})
	}
}

// A DESCRIPTION IS KEPT ONLY AS FAR AS A BRIEF CAN SHOW IT: the telling that
// names a subagent shows at most ledger.ValueLimit runes of its description,
// so a reading keeps [holdingsDescriptionBytes] of it, cut on a rune boundary
// as it is read, and copied, so the rest is not held until the walk ends. A
// description at the bound is kept whole; one past it is cut at the bound;
// one of three-byte runes, most of a metadata's bound long, is cut at the last
// whole rune inside it.
//
// Mutation: keep the description whole, or cut it at the byte bound inside a
// rune, and the long one is kept past the bound or as invalid UTF-8.
func TestADescriptionIsKeptOnlyAsFarAsABriefCanShowIt(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct{ description, kept string }{
		"a short one":       {"the subagent's task", "the subagent's task"},
		"one at the bound":  {strings.Repeat("d", holdingsDescriptionBytes), strings.Repeat("d", holdingsDescriptionBytes)},
		"one past it":       {strings.Repeat("d", holdingsDescriptionBytes+1), strings.Repeat("d", holdingsDescriptionBytes)},
		"60 KiB of € signs": {strings.Repeat("€", 20<<10), strings.Repeat("€", holdingsDescriptionBytes/len("€"))},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := hsForeground(t)
			s.meta = ctReplace(t, ctMeta, `"description":"the subagent's task"`,
				`"description":`+jsonString(t, tc.description))
			report := hsSubagentRead(t, s).Subagents[ctAgent]
			if !report.Reported || report.Description != tc.kept || !utf8.ValidString(report.Description) {
				t.Errorf("the subagent reads as reported %t, its description %d bytes, %.40q…; want it reported, "+
					"its description %d bytes, %.40q…", report.Reported, len(report.Description), report.Description,
					len(tc.kept), tc.kept)
			}
		})
	}
}
