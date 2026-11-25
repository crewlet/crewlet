package engine

import (
	"crypto/sha256"
	"fmt"
	"log/slog"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// heldByReading is what a reading counts of a call whose one live result in the
// main thread is output, the log's: [sandbox.SessionHoldings.Holds] answers it
// held for a logged call whose output is output.
func heldByReading(output string) sandbox.CallRecords {
	sum := sha256.Sum256([]byte(output))
	return sandbox.CallRecords{Threads: 1, Uses: 1, Results: 1, Live: true, TextBytes: len(output),
		Opening: sum, Whole: sum}
}

// scriptedReading is a reader's answer as a case scripts it: read whole, of the
// kind this build reads, with calls and subagents.
func scriptedReading(calls map[string]sandbox.CallRecords,
	subagents map[string]sandbox.SubagentThread) func(sandbox.SessionArchive, sandbox.HoldingsAsk) (
	sandbox.SessionHoldings, error) {
	return func(sandbox.SessionArchive, sandbox.HoldingsAsk) (sandbox.SessionHoldings, error) {
		return sandbox.SessionHoldings{Read: true, Kind: sandbox.HoldingsByID, Calls: calls,
			Subagents: subagents}, nil
	}
}

// readingAsk is an ask of calls for the release that wrote the kept session.
func readingAsk(calls ...sandbox.LoggedCall) sandbox.HoldingsAsk {
	return sandbox.HoldingsAsk{Calls: calls, Resumer: "2.1.310"}
}

// captureLogs points the process's logger at a buffer for the rest of t, as
// JSON, and back at its boot default after. A case that calls it is not
// parallel.
func captureLogs(t *testing.T) *stopLineBuffer {
	t.Helper()
	logs := &stopLineBuffer{}
	logging.Configure(slog.LevelInfo, logging.FormatJSON, logs)
	t.Cleanup(func() { logging.Configure(slog.LevelInfo, logging.FormatConsole, os.Stderr) })
	return logs
}

// readingLines is every line logs holds from index from on whose msg is msg and
// whose turn_id is turn.
func readingLines(t *testing.T, logs *stopLineBuffer, from int, msg, turn string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, record := range logs.records(t)[from:] {
		if record["msg"] == msg && record["turn_id"] == turn {
			out = append(out, record)
		}
	}
	return out
}

// A RELAUNCH READS ITS SESSION THROUGH THE MEMBER THAT WROTE IT. What a cut-off
// kept session holds is read off the capture the probe answered, by the runner
// of the chain's member driving the harness that wrote the session, which is
// the runner that still continues the turn's row: in a chain whose first
// member drives another harness, that member's runner is asked nothing, and
// the reading the session's own runner answers reaches the relaunch.
//
// Mutation: ask the first member's runner whatever its harness, and the
// session's own runner is asked nothing and the relaunch gets no reading.
func TestARelaunchReadsItsSessionThroughTheMemberThatWroteIt(t *testing.T) {
	t.Parallel()
	c, seat := chainCompany(t,
		chainMember{key: "oc", agent: "opencode", model: "anthropic/claude-sonnet-5"},
		chainMember{key: "api", agent: "claude-code", model: "claude-golden"})
	k := newKeptLaunchOn(t, c, seat, 40000, 0, 0, nil, keptCutOff)
	reading := scriptedReading(map[string]sandbox.CallRecords{"toolu_01": heldByReading("ok")}, nil)
	k.runners["claude-code"].Holdings = reading
	k.runners["opencode"].Holdings = reading
	basis := k.l.ProbeSession(k.ctx)
	if !basis.Continues() || basis.Harness != "claude-code" {
		t.Fatalf("the probe answered %+v, want the kept Claude Code session", basis)
	}
	called := sandbox.LoggedCall{ID: "toolu_01", Asked: true, Output: "ok"}
	ask := readingAsk(called)
	holdings := k.l.SessionHoldings(k.ctx, basis, ask)
	if held, _ := holdings.Holds(called); !held {
		t.Fatalf("the relaunch got %+v, want the reading the session's own runner answered", holdings)
	}
	if asks := k.runners["claude-code"].HoldingsAsks(); len(asks) != 1 || !reflect.DeepEqual(asks[0], ask) {
		t.Errorf("the session's own runner was asked %+v, want the relaunch's ask once", asks)
	}
	if asks := k.runners["opencode"].HoldingsAsks(); len(asks) != 0 {
		t.Errorf("the first member's runner, of another harness, was asked %+v, want nothing", asks)
	}
}

// A READING THAT FAILS IS LOGGED AND HOLDS NOTHING. Every way a reading reads
// none — a runner that reads no holdings, a reader's own refusal, a harness no
// member of the chain drives, a capture other than the one the run's record
// keeps, and a reading that did not read the archive whole — answers the zero
// value, which holds no call, and says why on one line, whose detail the
// log's root redacts on its way out (internal/logging), so a reader's words
// never carry a credential into the log; and no line says a reading was read.
//
// NOT PARALLEL: it swaps the process-wide logger.
//
// Mutation: answer the reader's value where it failed, answer a reading not
// read whole, or log nothing, and a case goes red.
func TestAReadingThatFailsIsLoggedAndHoldsNothing(t *testing.T) {
	logs := captureLogs(t)
	secret := "ghp_" + strings.Repeat("c", 36)
	called := sandbox.LoggedCall{ID: "toolu_01", Asked: true, Output: "ok"}
	for name, tc := range map[string]struct {
		reading func(sandbox.SessionArchive, sandbox.HoldingsAsk) (sandbox.SessionHoldings, error)
		basis   func(runner.SessionBasis) runner.SessionBasis
		says    string
	}{
		"a runner that reads no holdings": {says: "reads no session's holdings"},
		"the reader's own refusal": {
			reading: func(sandbox.SessionArchive, sandbox.HoldingsAsk) (sandbox.SessionHoldings, error) {
				return sandbox.SessionHoldings{}, &readRefused{why: "the archive names " + secret}
			},
			says: "the archive names"},
		"a harness no member drives": {
			reading: scriptedReading(map[string]sandbox.CallRecords{"toolu_01": heldByReading("ok")}, nil),
			basis: func(b runner.SessionBasis) runner.SessionBasis {
				b.Harness = "opencode"
				return b
			},
			says: "no runner of this build reads"},
		"another capture than the record keeps": {
			reading: scriptedReading(map[string]sandbox.CallRecords{"toolu_01": heldByReading("ok")}, nil),
			basis: func(b runner.SessionBasis) runner.SessionBasis {
				b.CaptureSeq++
				return b
			},
			says: "does not keep the capture"},
		"a reading not read whole": {
			reading: func(sandbox.SessionArchive, sandbox.HoldingsAsk) (sandbox.SessionHoldings, error) {
				return sandbox.SessionHoldings{Kind: sandbox.HoldingsByID,
					Calls: map[string]sandbox.CallRecords{"toolu_01": heldByReading("ok")}}, nil
			},
			says: "did not read the session's archive whole"},
	} {
		t.Run(name, func(t *testing.T) {
			k := newKeptLaunch(t, 40000, 0, 0)
			k.jobs.Holdings = tc.reading
			basis := k.l.ProbeSession(k.ctx)
			if tc.basis != nil {
				basis = tc.basis(basis)
			}
			from := len(logs.records(t))
			if got := k.l.SessionHoldings(k.ctx, basis, readingAsk(called)); !reflect.DeepEqual(got,
				sandbox.SessionHoldings{}) {
				t.Fatalf("a reading that failed answered %+v, want no reading", got)
			}
			unread := readingLines(t, logs, from, "executor_session_holdings_unread", k.l.turn.RunID)
			if len(unread) != 1 {
				t.Fatalf("the reading said %d executor_session_holdings_unread lines, want 1", len(unread))
			}
			detail, _ := unread[0]["detail"].(string)
			if !strings.Contains(detail, tc.says) || strings.Contains(detail, secret) {
				t.Errorf("the line's detail is %q, want it to say %q, redacted", detail, tc.says)
			}
			if outcome, _ := unread[0]["outcome"].(string); !strings.Contains(outcome, "every call after its save point") {
				t.Errorf("the line's outcome is %q, want it to say the relaunch is told every call after its "+
					"save point", outcome)
			}
			if read := readingLines(t, logs, from, "executor_session_holdings_read", k.l.turn.RunID); len(read) != 0 {
				t.Errorf("a reading that failed also said it was read: %v", read)
			}
		})
	}
}

// readRefused is a reader's refusal in its own words.
type readRefused struct{ why string }

func (r *readRefused) Error() string { return r.why }

// A READING IS LOGGED WITH WHAT IT HELD AND TOLD. Its line counts the calls the
// relaunch asked about, never one it did not: how many the session holds, how
// many the telling tells, how many distinct subagents a told call is under, and
// the release it was read for; beside it one line per such subagent, never per
// call, says why the session does not hold its report. Its outcome says the
// brief told by the reading, or why not, where it holds or moves a call; else
// the relaunch is told every call after its save point and no line follows.
// NOT PARALLEL: it swaps the process-wide logger.
//
// Mutation: count told as asked or a call nobody asked about, say a line per
// call under a subagent, name subagents by anything but what a told call is
// under, or promise a brief for a reading that holds and moves nothing: red.
func TestAReadingIsLoggedWithWhatItHeldAndTold(t *testing.T) {
	logs := captureLogs(t)
	k := newKeptLaunch(t, 40000, 0, 0)
	under := func(agent, output string) sandbox.CallRecords {
		r := heldByReading(output)
		r.Thread = agent
		return r
	}
	k.jobs.Holdings = scriptedReading(map[string]sandbox.CallRecords{
		"toolu_held":    heldByReading("ok"),
		"toolu_a1_one":  under("a1", "one"),
		"toolu_a1_two":  under("a1", "two"),
		"toolu_a2":      under("a2", "three"),
		"toolu_unasked": heldByReading("unasked"),
	}, map[string]sandbox.SubagentThread{
		"a1": {Why: "the session holds no report of subagent a1"},
		"a2": {Why: "the session holds no report of subagent a2"},
	})
	ask := readingAsk(
		sandbox.LoggedCall{ID: "toolu_held", Asked: true, Output: "ok"},
		sandbox.LoggedCall{ID: "toolu_a1_one", Asked: true, Output: "one"},
		sandbox.LoggedCall{ID: "toolu_a1_two", Asked: true, Output: "two"},
		sandbox.LoggedCall{ID: "toolu_a2", Asked: true, Output: "three"},
		sandbox.LoggedCall{ID: "toolu_unnamed", Asked: true, Output: "four"},
		sandbox.LoggedCall{ID: "toolu_unasked", Output: "unasked"},
	)
	from := len(logs.records(t))
	if got := k.l.SessionHoldings(k.ctx, k.l.ProbeSession(k.ctx), ask); !got.Read {
		t.Fatalf("the relaunch got %+v, want the reading", got)
	}
	read := readingLines(t, logs, from, "executor_session_holdings_read", k.l.turn.RunID)
	if len(read) != 1 {
		t.Fatalf("the reading said %d executor_session_holdings_read lines, want 1", len(read))
	}
	for key, want := range map[string]float64{"asked": 5, "held": 1, "told": 4, "unreported": 2} {
		if got, _ := read[0][key].(float64); got != want {
			t.Errorf("the line says %s=%v, want %v", key, read[0][key], want)
		}
	}
	if writer, _ := read[0]["writer"].(string); writer != "2.1.310" {
		t.Errorf("the line says writer=%q, want the release the reading was read for", writer)
	}
	if outcome, _ := read[0]["outcome"].(string); !strings.Contains(outcome, "executor_session_holdings_unused") ||
		!strings.Contains(outcome, "executor_session_not_continued") {
		t.Errorf("the line's outcome is %q, want the brief handed or the line that says why not", outcome)
	}
	subagents := readingLines(t, logs, from, "executor_session_subagent_unreported", k.l.turn.RunID)
	got := map[string]string{}
	for _, line := range subagents {
		agent, _ := line["agent"].(string)
		got[agent], _ = line["detail"].(string)
	}
	want := map[string]string{"a1": "the session holds no report of subagent a1",
		"a2": "the session holds no report of subagent a2"}
	if len(subagents) != 2 || !reflect.DeepEqual(got, want) {
		t.Errorf("the reading said %d subagent lines, %v; want one per unreported subagent, %v", len(subagents),
			got, want)
	}
	if unread := readingLines(t, logs, from, "executor_session_holdings_unread", k.l.turn.RunID); len(unread) != 0 {
		t.Errorf("a reading read also said it read none: %v", unread)
	}
	// A READING THAT HOLDS AND MOVES NOTHING: no brief is told by it, so its
	// line promises none.
	from = len(logs.records(t))
	none := readingAsk(sandbox.LoggedCall{ID: "toolu_unnamed", Asked: true, Output: "four"})
	if got := k.l.SessionHoldings(k.ctx, k.l.ProbeSession(k.ctx), none); !got.Read {
		t.Fatalf("the relaunch got %+v, want the reading", got)
	}
	read = readingLines(t, logs, from, "executor_session_holdings_read", k.l.turn.RunID)
	if len(read) != 1 {
		t.Fatalf("the reading that holds nothing said %d executor_session_holdings_read lines, want 1", len(read))
	}
	if held, _ := read[0]["held"].(float64); held != 0 {
		t.Fatalf("the reading that holds nothing says held=%v", read[0]["held"])
	}
	outcome, _ := read[0]["outcome"].(string)
	if !strings.Contains(outcome, "told every call after its save point") ||
		strings.Contains(outcome, "executor_session_holdings_unused") {
		t.Errorf("the line of a reading that holds and moves nothing says %q, want the relaunch told every call "+
			"after its save point and no line to follow", outcome)
	}
}

// THE READ LINE SAYS WHETHER THE SESSION COMPACTED after its run began, as
// the reading found it: true where it did, false where not.
//
// NOT PARALLEL: it swaps the process-wide logger.
//
// Mutation: fix the field false, and this goes red.
func TestTheReadLineSaysWhetherTheSessionCompacted(t *testing.T) {
	logs := captureLogs(t)
	k := newKeptLaunch(t, 40000, 0, 0)
	for _, compacted := range []bool{true, false} {
		k.jobs.Holdings = func(sandbox.SessionArchive, sandbox.HoldingsAsk) (sandbox.SessionHoldings, error) {
			return sandbox.SessionHoldings{Read: true, Kind: sandbox.HoldingsByContent, Compacted: compacted}, nil
		}
		from := len(logs.records(t))
		k.l.SessionHoldings(k.ctx, k.l.ProbeSession(k.ctx), readingAsk(sandbox.LoggedCall{ID: "1", Asked: true}))
		read := readingLines(t, logs, from, "executor_session_holdings_read", k.l.turn.RunID)
		if len(read) != 1 {
			t.Fatalf("the reading said %d read lines, want 1", len(read))
		}
		if got, _ := read[0]["compacted"].(bool); got != compacted {
			t.Errorf("the line says compacted=%v, want %t", read[0]["compacted"], compacted)
		}
	}
}

// A PROBED BASIS CARRIES THE RELEASE THAT WROTE THE SESSION, as the turn's row
// on the executor log records it: what a reading of what the session holds is
// read for, and so the release a brief told by it is handed to. Empty where the
// row records none, which no reading is read for.
//
// Mutation: leave the release empty, or answer anything but the row's, and a
// case goes red.
func TestAProbedBasisCarriesTheReleaseThatWroteTheSession(t *testing.T) {
	t.Parallel()
	for _, release := range []string{"2.1.310", "2.1.322", ""} {
		t.Run("release "+release, func(t *testing.T) {
			t.Parallel()
			k := newKeptLaunch(t, 40000, 0, 0)
			k.sessions.mu.Lock()
			row := k.sessions.rows["t1"]
			row.HarnessVersion = release
			k.sessions.rows["t1"] = row
			k.sessions.mu.Unlock()
			basis := k.l.ProbeSession(k.ctx)
			if !basis.Continues() || basis.Release != release {
				t.Fatalf("the probe answered %+v, want the kept session written by release %q", basis, release)
			}
		})
	}
}

// THE BRIEFS TOLD BY A READING REACH THE SANDBOX BESIDE THE OTHER TWO. A kept
// capture's continuation carries, beside its briefs told by the save point,
// the two told by a reading of what it holds, the release that reading was
// read for and what they left out, each as the runner wrote it; a
// continuation without them carries none; and a base never carries them,
// whatever it was handed, since no reading is read of one.
//
// Mutation: drop the briefs told by a reading, swap their two, lose their
// release or their told set, or carry them beside a base, and a case goes red.
func TestTheBriefToldByAReadingReachesTheSandboxBesideTheOtherTwo(t *testing.T) {
	t.Parallel()
	exact := &runner.ExactTelling{Resumer: "2.1.310", Brief: "the exact brief in the box you left",
		Replaced: "the exact brief in a box that replaced yours", Told: sandbox.BriefTold{Omitted: []int{3}}}
	kept := runner.SessionBasis{LaunchSeq: 1, CaptureSeq: 2, SessionID: "s-1", Harness: "claude-code",
		Release: "2.1.310", Interrupted: true}
	told := sandbox.BriefTold{Omitted: []int{1, 3}}

	got := sandboxSession(&runner.SessionContinuation{Basis: kept, Brief: "carry on in the box you left",
		Replaced: "carry on in a box that replaced yours", Told: told, Exact: exact})
	want := &sandbox.SessionContinuation{LaunchSeq: 1, CaptureSeq: 2, Brief: "carry on in the box you left",
		Replaced: "carry on in a box that replaced yours", Told: told,
		Exact: &sandbox.ExactTelling{Resumer: "2.1.310", Brief: "the exact brief in the box you left",
			Replaced: "the exact brief in a box that replaced yours", Told: sandbox.BriefTold{Omitted: []int{3}}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("a kept capture told by a reading reaches the sandbox as %+v (exact %+v), want %+v (exact %+v)",
			got, got.Exact, want, want.Exact)
	}

	if got := sandboxSession(&runner.SessionContinuation{Basis: kept, Brief: "b", Replaced: "r"}); got == nil ||
		got.Exact != nil {
		t.Errorf("a kept capture with no reading reaches the sandbox as %+v, want no briefs told by one", got)
	}

	base := runner.SessionBasis{SessionID: "s-0", Harness: "claude-code", Base: &runner.BaseBasis{Turn: "t0"}}
	if got := sandboxSession(&runner.SessionContinuation{Basis: base, Replaced: "carry on", Exact: exact}); got == nil ||
		!got.Base || got.Exact != nil {
		t.Errorf("a base reaches the sandbox as %+v, want it with no briefs told by a reading", got)
	}
}

// A BRIEF TOLD BY A READING THE FLOOR CANNOT CARRY IS DROPPED ALONE. A kept
// capture's floor counts the briefs a reading tells, since its box may be handed
// them. Where theirs is a floor no window can leave room for and the session's
// own briefs' is not, the launch offers the session without them, on a copy
// (the runner still holds the caller's), with a line saying why. Where the
// session's own floor cannot be carried either, it is dropped whole, its line
// naming its own briefs' floor, never the larger, and no line says the
// reading's briefs were dropped alone. Where nothing caps the seat, the launch
// is held to their floor, the longest. NOT PARALLEL: it swaps the process logger.
// Mutation: leave a reading's briefs out of the floor, drop the whole session
// where only their floor fails, drop them on the caller's session, log nothing,
// or name the larger floor where both fail: red.
func TestABriefToldByAReadingTheFloorCannotCarryIsDroppedAlone(t *testing.T) {
	logs := captureLogs(t)
	// ABOUT 70,000 TOKENS OF BRIEF, beside a 70,000-token session: 140,000,
	// past a 100,000-token day, where the session's own briefs are not.
	long := strings.Repeat("x", 4*70000)
	member := harnessProvider(t, chainMember{key: "head", agent: executor.ClaudeCodeName, model: "claude-opus-5-5"})
	for name, tc := range map[string]struct {
		ceiling     int
		context     int64
		offered     bool
		exactKept   bool
		floor       int
		droppedLine bool
	}{
		"the reading's briefs past the room, the session's own within it": {ceiling: 100000, context: 70000,
			offered: true, floor: 70000 + 6, droppedLine: true},
		"both past the room": {ceiling: 100000, context: 150000},
		"no window capped":   {context: 70000, offered: true, exactKept: true, floor: 70000 + 70000},
	} {
		t.Run(name, func(t *testing.T) {
			ceilings := org.TokenCeilings(nil)
			if tc.ceiling > 0 {
				ceilings = org.TokenCeilings{period.Day: tc.ceiling}
			}
			_, l, _ := cappedFloorRig(t, ceilings, nil, 0, nil)
			l.turn.RunID = "exact-floor " + name
			session := &runner.SessionContinuation{Brief: "carry on", Replaced: "carry on in a new box",
				Basis: runner.SessionBasis{LaunchSeq: 1, CaptureSeq: 1, SessionID: "s-1",
					Harness: executor.ClaudeCodeName, ContextTokens: tc.context, Interrupted: true},
				Exact: &runner.ExactTelling{Resumer: "2.1.310", Brief: "carry on", Replaced: long}}
			from := len(logs.records(t))
			got, floor := l.continuedFloor(t.Context(), runner.AgentRunRequest{Brief: "the fresh brief",
				Session: session}, member, true)
			if (got.Session != nil) != tc.offered {
				t.Fatalf("the launch offers the session: %v, want %v", got.Session != nil, tc.offered)
			}
			if session.Exact == nil {
				t.Errorf("the caller's continuation lost its briefs told by a reading")
			}
			if tc.offered {
				if kept := got.Session.Exact != nil; kept != tc.exactKept {
					t.Errorf("the launch offers the briefs told by a reading: %v, want %v", kept, tc.exactKept)
				}
				if got.Session.Brief != "carry on" || got.Session.Replaced != "carry on in a new box" {
					t.Errorf("the offered session's briefs are %q and %q, want its own", got.Session.Brief,
						got.Session.Replaced)
				}
				if floor.tokens() != tc.floor {
					t.Errorf("the launch is held to %d, want %d", floor.tokens(), tc.floor)
				}
			}
			unused := readingLines(t, logs, from, "executor_session_holdings_unused", l.turn.RunID)
			if (len(unused) == 1) != tc.droppedLine || len(unused) > 1 {
				t.Fatalf("the launch said %d executor_session_holdings_unused lines, want one: %v", len(unused),
					tc.droppedLine)
			}
			if tc.droppedLine {
				if detail, _ := unused[0]["detail"].(string); !strings.Contains(detail, "past") {
					t.Errorf("the line's detail is %q, want the floor's why", detail)
				}
				if outcome, _ := unused[0]["outcome"].(string); outcome != floorUnusedOutcome {
					t.Errorf("the line's outcome is %q, want the session offered without the reading: %q",
						outcome, floorUnusedOutcome)
				}
			}
			dropped := readingLines(t, logs, from, "executor_session_not_continued", l.turn.RunID)
			if (len(dropped) != 0) == tc.offered {
				t.Errorf("the launch said %d executor_session_not_continued lines, offering the session: %v",
					len(dropped), tc.offered)
			}
			if !tc.offered && len(dropped) == 1 {
				without := *session
				without.Exact = nil
				own := fmt.Sprintf("needs %d tokens", keptFloor(staticFloor(l.company), &without, false).tokens())
				if detail, _ := dropped[0]["detail"].(string); !strings.Contains(detail, own) {
					t.Errorf("the line's detail is %q, want the session's own briefs' floor (%s)", detail, own)
				}
			}
		})
	}
}

// THE LAUNCH'S DECISION SAYS WHETHER ITS BRIEF WAS TOLD BY A READING. A
// relaunch handed briefs told by a reading of what its kept session holds,
// beside the briefs told by its save point, starts its run on one of them only
// where its box's harness answers the release the reading was read for, and
// tells the runner so before the run starts, so the run's guard and its
// published prompt follow the brief it read; a box of another release starts
// on the brief told by the save point, and the runner is told that.
//
// Mutation: carry the briefs told by a reading nowhere, or leave the launch's
// word on them out of what the runner is told, and a case goes red.
func TestTheLaunchsDecisionSaysWhetherItsBriefWasToldByAReading(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		answers string
		exact   bool
	}{
		"a box of the release read for": {answers: "2.1.310", exact: true},
		"a box of another release":      {answers: "2.1.322"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			k := newKeptLaunch(t, 40000, 0, 0)
			k.jobs.CarryVersion = tc.answers
			req := launchRequest(k.l, nil)
			req.Brief = "the fresh brief"
			req.Session = &runner.SessionContinuation{Basis: k.l.ProbeSession(k.ctx),
				Brief: "carry on in the box you left", Replaced: "carry on in a box that replaced yours",
				Exact: &runner.ExactTelling{Resumer: "2.1.310", Brief: "the exact brief in the box you left",
					Replaced: "the exact brief in a box that replaced yours"}}
			var decided []runner.SessionDecision
			req.Decided = func(d runner.SessionDecision) { decided = append(decided, d) }
			if _, err := k.l.LaunchExecutor(k.ctx, req); err != nil {
				t.Fatalf("relaunch: %v", err)
			}
			if len(decided) != 1 || !decided[0].Continues || !decided[0].Kept || decided[0].Exact != tc.exact {
				t.Fatalf("the runner was told %+v, want once: the turn's kept session, told by a reading: %v",
					decided, tc.exact)
			}
			want := "carry on in the box you left"
			if decided[0].NewBox {
				want = "carry on in a box that replaced yours"
			}
			if tc.exact {
				want = strings.Replace(want, "carry on", "the exact brief", 1)
			}
			started := k.jobs.Started()
			if last := started[len(started)-1]; last.Resume != "s-1" || last.Brief != want {
				t.Fatalf("the relaunched job started with session %q and brief %q, want the kept session and %q",
					last.Resume, last.Brief, want)
			}
		})
	}
}
