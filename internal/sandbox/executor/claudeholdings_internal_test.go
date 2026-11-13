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

	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// THE CASES BELOW READ ARCHIVES TWO WAYS. Most build a main thread of the
// layer's own records (claudethread_test.go), shaped as Claude Code writes them,
// and a subagent's thread and metadata beside it where they read one, and tar
// them as the take's scrub writes an archive. The rest read sessions the
// probes measured, cut into testdata/claude/<release>/holdings/<name> as the
// members their archive held ([hsFixtures]).

// hsFixtures are the measured threads the cases read, each a member tree as its
// session's archive held it, by release and name, with where it was cut from.
// Each was cut as data, every line in its order with its keys in their order
// and of their types: the probes' call ids are their own scripted ones
// (`toolu_T1_a`), every other id is synthetic (the session in ctSession's
// family, the file named by it, a subagent's `a` and sixteen hex digits, every
// other uuid in ctUUID's form, so each parent still names its record), and a
// path under a home directory is /tmp/box/ and its last element. A line over
// 2 KiB that is no user record is ABRIDGED: each string in it over 256 bytes is
// "(abridged)", and in the harness's own bookkeeping (no assistant or system
// record) each list that long is empty, so its keys and their types stay. That
// is the harness's own bulky text, a request's shape, a prompt snapshot, a
// skill listing, and T4's three 2 MB padding blocks: what the reading reads of
// such a line is its head alone. A user record is never abridged.
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
	"2.1.310/s1":  "probe S, s1: the main thread just after /compact, untrimmed, its boundary at line 60",
	"2.1.322/s1":  "probe S, s1: the main thread just after /compact, untrimmed",
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
func hsBounds() scrubBounds { return boundsFor(executorkeep.SessionMaxBytes) }

// hsLogged is a call the log kept as a success that answered output.
func hsLogged(id, output string) sandbox.LoggedCall {
	return sandbox.LoggedCall{ID: id, Asked: true, Output: output}
}

// hsAsk asks about calls.
func hsAsk(calls ...sandbox.LoggedCall) sandbox.HoldingsAsk { return sandbox.HoldingsAsk{Calls: calls} }

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

// A MAIN-THREAD LINE THAT IS NO RECORD LEAVES THE SESSION UNREAD: a whole line
// that is not one JSON object, one whose head key is of another type, and a
// counted record whose message does not decode each end the reading with no
// reading, since a reading that passed one by would count a thread with a hole
// in it as whole, and a record that held a call as one that held none. The
// same thread without the line is read, and holds its call. A task
// notification whose origin does not decode costs only the fact it could have
// been: it is no notification, its subagent's leg is unanswered, and the
// reading stands.
//
// Mutation: pass a line that is no record by, or read a counted record whose
// message does not decode as one with no calls, and the reading stands; or
// take an origin that does not decode for a line that is no record, and the
// notification's session is unread.
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
		"a message no reader reads, on an attachment": {`{"parentUuid":"` + ctUUID(3) + `","isSidechain":false,` +
			`"type":"attachment","message":42,"uuid":"` + ctUUID(9) + `"}`, true},
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

// ONLY A CHAINED RECORD OF ITS OWN THREAD HOLDS A CALL. In the main thread: a
// user or assistant record with a parentUuid, `isSidechain` false and no
// `agentId`. A request's blob repeating the result, a queue operation, a
// sidechain record and an agent's record in the main file, and a user record
// out of the chain each hold nothing, so the call they repeat holds its use
// alone and is told; a counted record's result holds it. In subagent A's
// thread: one with a parentUuid, A's `agentId` and `isSidechain` true. A
// result there naming no agent or another, or no sidechain, holds nothing, and
// leaves A's thread not whole; one out of the chain holds nothing either.
//
// Mutation: count a record that is not chained, or not its own thread's, and
// the case's result holds its call.
func TestOnlyAChainedRecordOfItsOwnThreadHoldsACall(t *testing.T) {
	t.Parallel()
	const id, out = "toolu_own", "own done"
	call := hsLogged(id, out)
	content := hsAnswer(id, hsBlock(t, out))
	result := ctResult(ctUUID(2), ctUUID(3), content, hsBlock(t, out))
	for name, tc := range map[string]struct {
		record string
		held   bool
	}{
		"a counted result": {result, true},
		"a request's blob": {`{"type":"api-request-blob","sessionId":"` + ctSession + `","hash":"b1",` +
			`"message":{"role":"user","content":` + content + `}}`, false},
		"a queue operation": {`{"type":"queue-operation","operation":"enqueue","sessionId":"` + ctSession +
			`","message":{"role":"user","content":` + content + `}}`, false},
		"a sidechain record": {ctReplace(t, result, `"isSidechain":false`, `"isSidechain":true`), false},
		"an agent's record": {ctReplace(t, result, `"isSidechain":false`,
			`"isSidechain":false,"agentId":"`+ctAgent+`"`), false},
		"a record out of the chain": {ctReplace(t, result, `"parentUuid":"`+ctUUID(2)+`",`, ``), false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			thread := hsThread(ctAssistant(ctUUID(1), ctUUID(2), hsUse(id)), tc.record)
			got, err := hsRead(t, hsArchive(t, thread), ctSession, hsAsk(call))
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
			records := got.Calls[hsSubCall]
			if held, _ := got.Holds(hsLogged(hsSubCall, hsSubOut)); held != tc.held || records.Uses != 1 ||
				(records.Results == 1) != tc.held || records.Thread != ctAgent {
				t.Errorf("Holds = %t, its records %+v; want held %t, with one use in its subagent's thread", held,
					records, tc.held)
			}
		})
	}
}

// A RESULT BEFORE THE LAST RESTART IS NOT LIVE: what a resumed session is
// handed starts over at a restart, so a result before the main thread's last
// one is told. Measured: s1's compaction, whose boundary follows its call's
// result, and s2's trim, which kept none of the job's records, so its call is
// not in the file at all; and, as both releases' readers read them, a
// microcompaction's boundary, a span replaced and a root after the first. A
// result between two restarts is before the last. A result after the last
// restart is live, and held.
//
// Mutation: compare a result's line with the thread's FIRST restart, and the
// result between two restarts is live.
func TestAResultBeforeTheLastRestartIsNotLive(t *testing.T) {
	t.Parallel()
	const id, out = "toolu_restart", "restart done"
	call := hsLogged(id, out)
	pair := hsCall(ctUUID(1), 2, id, hsBlock(t, out))
	restart := map[string]string{
		"a compaction's boundary":      ctBoundary(ctUUID(4), ctUUID(5), ctUUID(3)),
		"a microcompaction's boundary": ctMicrocompactBoundary(ctUUID(3), ctUUID(4)),
		"a span replaced":              ctSpan(ctUUID(3), ctUUID(4), "true"),
		"a later root":                 ctBrief(ctUUID(4)),
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

// A RESULT ITS HARNESS REPLACED IS NOT LIVE: where Claude Code hands its model
// a preview in a result's place, it records the replacement beside the
// result's own record, which keeps the whole output, and a resume hands its
// model the preview again, so a call whose result such a record names is
// told, whatever its result's own record holds and wherever the record lies;
// a record replacing another call's result, or naming the call in an entry of
// another kind, leaves it held. In a subagent's thread its own record tells
// its call the same way, the subagent still reported. A record listing its
// replacements in no shape Claude Code writes is no reading in the main
// thread, and leaves a subagent's thread not whole.
//
// Mutation: pass every content-replacement record over, or keep a replaced
// result live, and the replaced call is held; or read past a record of no
// shape, and the main thread's reading stands.
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
// lies off the chain, and a continuation hands its model every whole-line
// result after the last restart all the same, with its call: T1's, T2's and
// T4's siblings, T5's Agent answer beside its sibling, and T4's linear calls
// past the 5 MiB load path. Each is live, and each call the bridge answered is
// held; T3's answered call is held, and its open call, whose result the file
// never held, holds its use alone and is told.
//
// Mutation: hold a result only on the chain its thread's last record
// descends from, and every off-chain sibling is told.
func TestParallelResultsAfterTheLastRestartAreLive(t *testing.T) {
	t.Parallel()
	done := func(tag string) sandbox.LoggedCall { return hsLogged("toolu_"+tag, "crewlet "+tag+" done") }
	for _, release := range []string{"2.1.310", "2.1.322"} {
		for name, calls := range map[string][]sandbox.LoggedCall{
			"T1": {done("T1_a"), done("T1_b")},
			"T2": {done("T2_a"), done("T2_b")},
			"T4": {done("T4_a"), done("T4_b"), done("T4_f1"), done("T4_f2"), done("T4_f3")},
			"T5": {done("T5_m")},
		} {
			t.Run(release+"/"+name, func(t *testing.T) {
				t.Parallel()
				archive, session := hsFixture(t, release, name)
				ask := append([]sandbox.LoggedCall{}, calls...)
				if name == "T5" {
					ask = append(ask, sandbox.LoggedCall{ID: "toolu_T5_agent", Asked: true})
				}
				got, err := hsRead(t, archive, session, hsAsk(ask...))
				hsReadWhole(t, got, err)
				for _, call := range calls {
					if held, _ := got.Holds(call); !held {
						t.Errorf("%s: Holds = false, its records %+v; want it held", call.ID, got.Calls[call.ID])
					}
				}
				if name == "T5" {
					// The Agent answer is no call the bridge logs: what is held
					// to the rule is that its result, off the chain, is live.
					if records := got.Calls["toolu_T5_agent"]; records.Uses != 1 || records.Results != 1 ||
						!records.Live || records.TextBytes <= 0 {
						t.Errorf("the Agent answer's records %+v; want its use and one live text result", records)
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

// A RESULT'S TEXT IS A STRING OR ONE TEXT BLOCK: what a model is handed of a
// call's output, as its harness recorded it, is the content's string, or the
// text of a list of exactly one object of `type` "text" whose `text` is a
// string, its other keys passed over, every key matched exactly and a key
// written twice read by its last value. Every other content, a ToolSearch's
// list of tool references, two text blocks, an empty list, an image, a key
// spelt in another case, absent or null, has no text, -1, which no log's
// output matches. And a list a line long is read no further than its second
// element: it costs no more than a list of two.
//
// Not parallel: the allocations are the process's, and a parallel case's
// would be counted with this one's.
//
// Mutation: decode a result's content whole, its list before its first
// element is held to a type, and the long list costs an allocation an element;
// or take any list's first text block for its text, and two blocks match.
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
	t.Run("a list a line long", func(t *testing.T) {
		head := `[{"type":"text","text":"x"},`
		short := []byte(head + `1]`)
		long := []byte(head + strings.Repeat(`1,`, (listBytes-len(head))/2) + `1]`)
		for _, list := range [][]byte{short, long} {
			if text, ok := resultText(list); ok {
				t.Fatalf("a list of a text block and more reads as the text %q; want none", text)
			}
		}
		shortObjects, _ := allocationsOf(func() { _, _ = resultText(short) })
		longObjects, longBytes := allocationsOf(func() { _, _ = resultText(long) })
		if longObjects > shortObjects+listSlack || longBytes >= uint64(len(long)) {
			t.Errorf("a list of %d bytes costs %d allocations and %d bytes, where a list of two costs %d: "+
				"it is read past its second element", len(long), longObjects, longBytes, shortObjects)
		}
	})
}

// THE RESUME'S MARK DECIDES NOTHING (rule 3, met by rule 4). Measured (EK): a
// resume that finds a call with no result writes one of its own, a failure in
// its own words, marked `toolDenialKind: interrupted`; read against the
// success the log took, its call is told. And it is told for its failure and
// its text, never its mark: told with the mark stripped, and told with its text
// reworded to the log's very output while it still says it failed; held only
// where the result is the log's, a success of the log's text, whatever mark it
// carries.
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
			archive, session := hsFixture(t, "2.1.310", "EK", tc.edits...)
			got, err := hsRead(t, archive, session, hsAsk(call))
			hsReadWhole(t, got, err)
			if held, _ := got.Holds(call); held != tc.held {
				t.Errorf("Holds = %t, its records %+v; want %t", held, got.Calls[call.ID], tc.held)
			}
		})
	}
}

// A STOPPED OR GIVEN-UP HARNESS'S OWN RESULT DOES NOT HOLD ITS CALL. Measured: a
// harness stopped in a shell call records its exit code as the call's failed
// result (ET0), and one that gave up on a slow call records its own timeout
// text as a failure (pB, a) while the bridge logged the call's success and its
// output; each is told. The timeout text is as long as that output, so it is
// told for its text as well as its failure: told still as a success. A call
// the bridge answered in time (pB, c) is held: its result is the log's,
// beside a request's blob that repeats it with a reminder appended, which is
// no record of the thread.
//
// Mutation: compare a result's text with the log's output by its length
// alone, and the timeout text read as a success holds its call.
func TestAStoppedOrGivenUpHarnesssOwnResultDoesNotHoldItsCall(t *testing.T) {
	t.Parallel()
	gaveUp := hsLogged("toolu_pb_a", "slow a finished its work")
	for name, tc := range map[string]struct {
		fixture string
		edits   [][2]string
		call    sandbox.LoggedCall
		held    bool
	}{
		"a harness stopped in its call": {"ET0", nil, hsLogged("toolu_et1", "et1-done\n"), false},
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
	bounds := scrubBounds{plain: 1 << 20, piece: scrubPieceMaxBytes}
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

// A READING STOPS BETWEEN LINES WHEN ITS CONTEXT ENDS, since a line's read
// takes none: a caller that left while the main thread was read, many
// mebibytes that gzip barely shrinks, ends the reading within a line, with the
// caller's own error and the rest of the stream never read. A walk that asked
// only between members would read the thread whole and drain the stream.
//
// Mutation: ask the context between members alone, and the reading reads the
// whole thread and stands.
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
	archive := hsArchive(t, hsThread(lines...))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stream := &hsCancelling{r: bytes.NewReader(archive), after: 1 << 20, cancel: cancel}
	got, err := hsReadFrom(ctx, stream, ctSession, hsAsk(hsLogged("toolu_ctx", "ctx done")), hsBounds())
	if got.Read || !errors.Is(err, context.Canceled) {
		t.Fatalf("a reading whose caller left = %+v, %v; want no reading, with its caller's error", got, err)
	}
	if stream.handed >= len(archive) {
		t.Errorf("the reading read all %d bytes of a stream whose caller left after %d", len(archive), stream.after)
	}
}

// HOLDINGS ARE HELD TO THEIR BOUNDS: a line one byte past the longest a thread
// is read with ends the reading, the layer's bound named, where one at it is
// read; a main thread that is no regular file, a link or a directory, is not
// read, as the box restores none; an archive holding no main thread is no
// reading, never one of a session that holds nothing; and an archive naming
// more subagents than [holdingsMaxAgents], or a main thread holding more legs
// of their work than [holdingsMaxLegs], its notifications among them, is no
// reading, where one at each bound is read.
//
// Mutation: read a main thread that is no regular file as one with no lines,
// or an archive with no main thread as a session holding none of its calls,
// and the reading stands; or keep a notification beside the legs uncounted,
// and the notification past the bound is read.
func TestHoldingsAreHeldToTheirBounds(t *testing.T) {
	t.Parallel()
	call := hsLogged("toolu_bounds", "bounds done")
	pad := func(n int) string { // a line of n bytes, its break included
		head, tail := `{"type":"queue-operation","content":"`, `"}`
		return head + strings.Repeat("x", n-1-len(head)-len(tail)) + tail
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
		"a line at its bound": {[]tarMember{{h: tar.Header{Name: hsMain, Mode: 0o600, Typeflag: tar.TypeReg},
			body: hsThread(pad(threadLineMax))}}, true, false},
		"a line past its bound": {[]tarMember{{h: tar.Header{Name: hsMain, Mode: 0o600, Typeflag: tar.TypeReg},
			body: hsThread(pad(threadLineMax + 1))}}, false, true},
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
// another session's main thread, a thread deeper than the session's
// subagents (a Workflow's), `agent-.jsonl`, and the session's saved compaction
// under either of its names are passed over, so a call only they record is
// absent, and the reading of the session's own main thread stands. A
// depth-one subagent's thread and metadata are read, its call counted in its
// thread; and either held twice, under two spellings, is no reading, since a
// restore keeps the last copy.
//
// Mutation: take any `projects/crewlet/<x>.jsonl` for the main thread, as a
// person's transcript does, and another session's call is counted; or drop
// the check of a subagent's member held twice, and its archive is read.
func TestHoldingsReadOnlyTheContinuedSessionsMembers(t *testing.T) {
	t.Parallel()
	const id, out = "toolu_member", "member done"
	call := hsLogged(id, out)
	elsewhere := hsThread(hsCall(ctUUID(1), 2, id, hsBlock(t, out))...)
	session := claudeProjectPrefix + ctSession
	for name, member := range map[string]string{
		"another session's main thread":          claudeProjectPrefix + ctOther + ".jsonl",
		"a Workflow's thread":                    session + "/subagents/workflows/w-1/agent-" + ctAgent + ".jsonl",
		"agent-.jsonl":                           session + "/subagents/agent-.jsonl",
		"the saved compaction":                   session + "/" + claudePrecompactFile,
		"the saved compaction beside the thread": session + "." + claudePrecompactFile,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := hsRead(t, tarOf(t, map[string]string{hsMain: hsThread(), member: elsewhere}), ctSession,
				hsAsk(call))
			hsReadWhole(t, got, err)
			if records, named := got.Calls[id]; named {
				t.Errorf("its records %+v; want none, %s being no member the reading reads", records, member)
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

// A SUBAGENT IS REPORTED ONLY WHERE EVERY LEG WAS ANSWERED AND FINISHED (rule
// 2), so a call its thread holds is held only then; otherwise it is told under
// the subagent's heading, the reading saying which condition failed first.
// Measured: P2(g)'s two subagents, one answered by its Agent call's own
// result and one by its task notification, P2(h)'s with a tree of two calls
// at once in each thread, P2(c)'s, P2(dc)'s, T5's on both releases and T10's
// on both, the last on 2.1.322 past the turnCompanion record before its
// notification, are reported; P2(dc)'s box copy, its subagent's thread cut
// back before its last reply (condition 5), and P2(d)'s, its Agent call
// answered by the resume's own result (condition 3), are not. Built, each
// otherwise reportable: a subagent whose metadata is missing or names a call
// of another tool; whose thread restarts, opens on a record that roots no
// chain, ends on an empty reply, on the text of a reply cut before its call, on
// a reply its harness wrote itself (a failed request's error, which ends the
// subagent and answers its call completed, or a resume's "No response
// requested."), on one marked an API error though it ended its turn, or on one
// cut at its token bound, or holds a skill's text, a leg the main thread holds
// no call of (d4's
// record); whose thread starts a second leg it finished, the main thread
// holding no call of it (condition 4 alone); whose Agent call's answer names
// another agent or is a background subagent's; whose notification precedes a
// restart of the main thread; or that was sent a message whose result refused
// it with null, or left none, and no notification answered. A message its own
// result refused with the literal false is no leg, and one answered by a
// notification is answered; and the members' order is the box's to choose.
//
// Mutation: ignore a restart in a subagent's thread, and the restarted one is
// reported; drop condition 4, and the second leg's subagent is reported; take
// a reply of text for finished whatever it stopped for, and the cut pair's is;
// take one for finished whatever it stopped for but a call, and the resume's
// own reply's is; or read past a reply's mark of an API error, and the marked
// reply's is.
func TestASubagentIsReportedOnlyWhereEveryLegWasAnsweredAndFinished(t *testing.T) {
	t.Parallel()
	with := func(base func(*testing.T) hsSession, edit func(*testing.T, *hsSession)) func(*testing.T) hsSession {
		return func(t *testing.T) hsSession {
			s := base(t)
			edit(t, &s)
			return s
		}
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
		"its notification before a restart of the main thread": {with(hsBackground, func(t *testing.T, s *hsSession) {
			s.main = append(s.main, ctMicrocompactBoundary(ctUUID(4), ctUUID(5)))
		}), hsUnanswered},
		"sent a message its own result refused with null": {with(hsForeground, sent("null")), hsUnanswered},
		"sent a message with no result":                   {with(hsForeground, sent("")), hsUnanswered},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			hsReports(t, tc.session(t), tc.why)
		})
	}
	done := func(id, output string) sandbox.LoggedCall { return hsLogged(id, output) }
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
			hsUnanswered},
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
					if records := got.Calls[call.ID]; held != (tc.why == "") || records.Thread != agent ||
						(!held && under != agent) {
						t.Errorf("%s: Holds = %t under %q, its records %+v; want it held %t, in %s's thread",
							call.ID, held, under, records, tc.why == "", agent)
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
				hsAnswerOf(t, ctUUID(1), ctUUID(3), hsStarted, "the subagent's report", hsCompleted(ctAgent)),
				ctAssistant(ctUUID(3), ctUUID(2), hsCallOf(hsStarted, claudeToolAgent, hsAgentInput))}
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
		{Kind: blockToolUse, ID: "toolu_" + strings.Repeat("l", 1<<20), Name: claudeToolAgent,
			Input: json.RawMessage(hsAgentInput)},
		{Kind: blockToolUse, ID: "toolu test task", Name: claudeToolTask, Input: json.RawMessage(hsAgentInput)},
		{Kind: blockToolUse, ID: strings.Repeat("s", sandbox.MaxBridgeCallIDBytes+1), Name: claudeToolSendMessage,
			Input: json.RawMessage(hsSendInput)},
		{Kind: blockToolUse, ID: hsStarted, Name: claudeToolAgent, Input: json.RawMessage(hsAgentInput)},
	} {
		if err := w.leg(call); err != nil {
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
// holding a space and one a mebibyte long each leave a few words of the
// reading's own.
//
// Mutation: read such a send as no leg at all, and its subagent is reported;
// or file it under its id, and the subagent's why quotes the mebibyte.
func TestASubagentSentAMessageUnderAnIDTheLogKeepsNoneOfIsNotReported(t *testing.T) {
	t.Parallel()
	for name, id := range map[string]string{
		"an id past the rule's bound": strings.Repeat("s", sandbox.MaxBridgeCallIDBytes+1),
		"an id holding a space":       "toolu test send",
		"an id a mebibyte long":       "toolu_" + strings.Repeat("m", 1<<20),
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

// WHAT MEMBERS HAND OUT IS HELD TO THE ARCHIVE'S BUDGET, the one the archive's
// plaintext is held to, apart from what its stream inflates to: a sparse
// member hands out holes its stream does not hold. Under a plaintext bound of
// a mebibyte, a subagent's thread of a whole first line, a hole of 600 KiB and
// a break, whose lines each stay under the line's bound and whose stream is a
// few kibibytes, is read; two of them hand out past the budget, and the
// second is no reading, the layer's bound named.
//
// Mutation: hand each member the whole plaintext bound, and both are read.
func TestWhatMembersHandOutIsHeldToTheArchivesBudget(t *testing.T) {
	t.Parallel()
	bounds := scrubBounds{plain: 1 << 20, piece: scrubPieceMaxBytes}
	const hole = 600 << 10
	thread := func(agent string) []byte {
		first := ctSubagentPrompt(ctUUID(101)) + "\n"
		return sparseMember(claudeProjectPrefix+ctSession+"/subagents/agent-"+agent+".jsonl",
			int64(len(first)+hole+1), sparseSegment{0, first}, sparseSegment{int64(len(first) + hole), "\n"})
	}
	main := plainMember(hsMain, hsThread())
	for name, tc := range map[string]struct {
		members [][]byte
		read    bool
	}{
		"one such thread":  {[][]byte{main, thread(ctAgent)}, true},
		"two such threads": {[][]byte{main, thread(ctAgent), thread("a0000000000000002")}, false},
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
			if !errors.Is(err, errThreadBound) {
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
