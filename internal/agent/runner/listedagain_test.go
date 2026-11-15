package runner_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A HELD CALL A TOLD CALL COULD BE TAKEN FOR IS LISTED AGAIN. A brief tells a
// session that may already show some of its told calls, and every late call,
// that a call shown both in the session and in a list is ONE call, made once.
// Where a call the session holds and a told call name one tool with arguments
// that read alike in the told call's line, a model reading both can take the
// told call for the held one and lose it. So the held call is listed again,
// under words that say the session shows it and that the told call is one of
// its own (runner's calledAfter, prompts.AfterSave.Shown).

// listedAgainHeading heads the calls the session holds that a brief lists
// again (prompts.AfterSave.Shown).
const listedAgainHeading = "## Calls the session above shows, listed again"

// digested is call as its run's call log admitted it, digested by its whole
// arguments, as every call this build's log takes is: its arguments known
// whole, so they read alike to another call's only as its line shows them.
func digested(call runner.BridgedCall) runner.BridgedCall {
	call.Digest = turnctx.ArgsDigest(call.Args)
	return call
}

// openCodeKept is the turn's kept session of an OpenCode job, cut off, saved
// at the round's first call.
var openCodeKept = runner.SessionBasis{LaunchSeq: 1, CaptureSeq: 1, SessionID: "s-1", Harness: "opencode",
	Through: 1, NoteCursor: 1, Home: "/boxes/4f2a/home", Interrupted: true, ContextTokens: 40000}

// listedAgainSection is the section that lists again the one held call
// slack_post({"channel":"C1","text":"hi"}).
const listedAgainSection = listedAgainHeading +
	"\nThe session above shows each of these calls. Each is listed again because a call in another list" +
	" of this message names the same tool with arguments that read alike here: that one is a call of its" +
	" own, made as well as this one, never this one shown twice." +
	"\n- slack_post({\"channel\":\"C1\",\"text\":\"hi\"}) → success"

// lateHeadingLine heads the late calls (prompts' lateCallsHeader).
const lateHeadingLine = "## Calls the run that made them stopped waiting for, or whose outcome is unknown"

// cutOffKept is the turn's kept session, cut off, saved once its log had taken
// through calls.
func cutOffKept(through int) runner.SessionBasis {
	basis := keptBasis
	basis.Through, basis.Interrupted = through, true
	return basis
}

// sectionOf is the section of brief that heading opens, up to the next
// section's heading, and whether brief has one.
func sectionOf(brief, heading string) (string, bool) {
	at := strings.Index(brief, heading)
	if at < 0 {
		return "", false
	}
	section := brief[at:]
	if end := strings.Index(section[len(heading):], "\n## "); end >= 0 {
		section = section[:len(heading)+end]
	}
	return section, true
}

// post is a digested slack_post to C1 saying text, the seq'th call the bridge
// admitted and the log took.
func post(text string, seq int) runner.BridgedCall {
	return digested(numbered("slack_post", map[string]any{"channel": "C1", "text": text}, seq, seq))
}

// A HELD CALL A TOLD CALL COULD BE TAKEN FOR IS LISTED AGAIN. A cut-off
// capture saved at the round's first call holds that call; the call after its
// save names the same tool with the same arguments, and the brief tells it as a
// call the session may already show, under words that say a call shown both in
// the session and in the list is ONE call. So the held call is listed again,
// after the calls since the save and before the late calls, under words that
// say the session shows it; and no section of the brief is an empty one.
//
// Mutation: list nothing again, and the brief tells the call once.
func TestAHeldCallAToldCallCouldBeTakenForIsListedAgain(t *testing.T) {
	t.Parallel()
	late := digested(numbered("jira_create", map[string]any{"summary": "a late issue"}, 3, 3))
	late.Late = true
	brief := lostRelaunch(t, cutOffKept(1), runner.Resume{Bridged: []runner.BridgedCall{
		post("hi", 1), post("hi", 2), late,
	}}).Session.Brief
	line := `- slack_post({"channel":"C1","text":"hi"}) → success`
	sinceAt, againAt := strings.Index(brief, cutOffHeader), strings.Index(brief, listedAgainSection)
	lateAt := strings.Index(brief, lateHeadingLine)
	if sinceAt < 0 || againAt < sinceAt || lateAt < againAt {
		t.Fatalf("the brief is not told the calls since its save, the held call listed again, then the late "+
			"calls:\n%s", brief)
	}
	if told := brief[sinceAt:againAt]; strings.Count(told, line) != 1 {
		t.Errorf("the call after the save is not told once among the calls since the save:\n%s", brief)
	}
	if strings.Count(brief, line) != 2 {
		t.Errorf("the brief tells the call %d times, want once told and once listed again:\n%s",
			strings.Count(brief, line), brief)
	}
	if strings.Contains(brief, "(none)") {
		t.Errorf("the brief carries an empty section:\n%s", brief)
	}
}

// A WRITE WHOSE ARGUMENTS READ ALIKE IS LISTED AGAIN, DIGESTS APART. Two
// writes to one path whose contents differ past the ledger's budget have
// different digests, and different results; but the told write's line shows
// its content as a rewrite or omitted by its size, never as itself, and no
// result, so all a model can compare is its path, which is the held write's.
// It is listed again.
//
// Mutation: compare digests alone, or compare results too, and the held write
// is not listed again.
func TestAWriteWhoseArgumentsReadAlikeIsListedAgain(t *testing.T) {
	t.Parallel()
	held := digested(numbered("write_file", map[string]any{"path": "notes.md",
		"content": strings.Repeat("a", ledger.ValueLimit+50)}, 1, 1))
	held.Result = "wrote the a's"
	told := digested(numbered("write_file", map[string]any{"path": "notes.md",
		"content": strings.Repeat("b", ledger.ValueLimit+50)}, 2, 2))
	told.Result = "wrote the b's"
	if held.Digest == told.Digest {
		t.Fatal("the two writes share a digest; the case needs two")
	}
	brief := lostRelaunch(t, cutOffKept(1), runner.Resume{Bridged: []runner.BridgedCall{held, told}}).Session.Brief
	again, listed := sectionOf(brief, listedAgainHeading)
	if !listed || !strings.Contains(again, `"path":"notes.md"`) {
		t.Errorf("a held write a told write reads alike is not listed again:\n%s", brief)
	}
}

// A CALL WHOSE SHOWN ARGUMENTS DIFFER IS NOT LISTED AGAIN. A told write to
// another path shows that path whole, so a model reading it never takes it for
// the held write; and a told call with a key the held call lacks, or lacking
// one it has, shows that key, whatever its value. Neither is listed again.
//
// Mutation: read a value the line shows whole as one that may equal anything,
// or read a key one call has and the other lacks as alike, and a case lists
// the held call again.
func TestACallWhoseShownArgumentsDifferIsNotListedAgain(t *testing.T) {
	t.Parallel()
	write := func(path, fill string, seq int) runner.BridgedCall {
		return digested(numbered("write_file", map[string]any{"path": path,
			"content": strings.Repeat(fill, ledger.ValueLimit+50)}, seq, seq))
	}
	for name, calls := range map[string][]runner.BridgedCall{
		"another path, shown whole": {write("a.md", "a", 1), write("b.md", "b", 2)},
		"a key the told call has and the held one lacks": {
			digested(numbered("slack_post", map[string]any{"channel": "C1"}, 1, 1)),
			digested(numbered("slack_post", map[string]any{"channel": "C1", "text": "hi"}, 2, 2)),
		},
		"a key the held call has and the told one lacks": {
			digested(numbered("slack_post", map[string]any{"channel": "C1", "text": "hi"}, 1, 1)),
			digested(numbered("slack_post", map[string]any{"channel": "C1"}, 2, 2)),
		},
	} {
		brief := lostRelaunch(t, cutOffKept(1), runner.Resume{Bridged: calls}).Session.Brief
		if strings.Contains(brief, listedAgainHeading) {
			t.Errorf("%s: a held call no told call reads alike is listed again:\n%s", name, brief)
		}
	}
}

// A CALL WHOSE ARGUMENTS ARE NOT KNOWN IS LISTED BESIDE EVERY CALL OF ITS TOOL.
// Nothing says arguments not known whole read apart from any: a told call's
// withheld, in flight, kept less than whole by the run's record or recorded
// with no digest, or a held call's kept less than whole. So the held call is
// listed again beside a told call of its tool whose arguments, as far as they
// are known, differ.
//
// Mutation: read a call kept less than whole as one known whole, and its cases
// go red.
func TestACallWhoseArgumentsAreNotKnownIsListedBesideEveryCallOfItsTool(t *testing.T) {
	t.Parallel()
	withheld := runner.BridgedCall{Call: ledger.Call{Name: "slack_post", Result: "posted", Withheld: true},
		Seq: 2, Appended: 2}
	keptShort := post("told", 2)
	keptShort.ArgsBytes = 70_000
	heldShort := post("hi", 1)
	heldShort.ArgsBytes = 70_000
	undigested := numbered("slack_post", map[string]any{"channel": "C1", "text": "told"}, 2, 2)
	inFlight := []runner.BridgedCall{{Call: ledger.Call{Name: "slack_post", Result: runner.UnfinishedNote,
		Failed: true, Digest: post("told", 2).Digest, Unfinished: true}, Seq: 2}}
	for name, round := range map[string]runner.Resume{
		"a withheld told call":  {Bridged: []runner.BridgedCall{post("hi", 1), withheld}},
		"a told call in flight": {Bridged: []runner.BridgedCall{post("hi", 1)}, Unfinished: inFlight},
		"a told call its record kept less than whole": {
			Bridged: []runner.BridgedCall{post("hi", 1), keptShort}},
		"a held call its record kept less than whole": {
			Bridged: []runner.BridgedCall{heldShort, post("told", 2)}},
		"a told call with no digest": {Bridged: []runner.BridgedCall{post("hi", 1), undigested}},
	} {
		brief := lostRelaunch(t, cutOffKept(1), round).Session.Brief
		if again, listed := sectionOf(brief, listedAgainHeading); !listed || !strings.Contains(again, `"text":"hi"`) {
			t.Errorf("%s: the held call is not listed again:\n%s", name, brief)
		}
	}
}

// A HELD CALL LIKE A LATE CALL IS LISTED AGAIN, FOR EVERY HARNESS. The copy a
// job's box holds where its harness ended its own run holds every call of the
// round but the late ones, and is told those under words that say each is ONE
// call. Where one names the tool and arguments of a held call, the held call is
// listed again, for an OpenCode job's copy as for a Claude Code one's: and the
// brief is the one the same round gets where its held call reads apart, plus
// exactly that section, before the late calls.
//
// Mutation: leave the late calls out of the calls a held call is matched
// against, and nothing is listed again.
func TestAHeldCallLikeALateCallIsListedAgainForEveryHarness(t *testing.T) {
	t.Parallel()
	for harness, carry := range map[string]func(*runner.Resume){
		"opencode": func(r *runner.Resume) {
			r.HarnessEnd, r.Run.Harness, r.BasisRestart = sandbox.HarnessEndOwnRun, "opencode", new("")
		},
		sandbox.ClaudeCodeHarness: copyOf(sandbox.HarnessEndOwnRun, far),
	} {
		briefOf := func(held runner.BridgedCall) string {
			late := post("hi", 2)
			late.Late = true
			return inPlaceOf(t, guarded{basis: inBoxBasis, bridged: []runner.BridgedCall{held, late},
				carry: func(r *runner.Resume) {
					answered("use main")(r)
					carry(r)
				}})
		}
		alike, apart := briefOf(post("hi", 1)), briefOf(post("bye", 1))
		lateAt := strings.Index(apart, lateHeadingLine)
		if lateAt < 0 {
			t.Fatalf("%s: the copy is told no late call:\n%s", harness, apart)
		}
		if want := apart[:lateAt] + listedAgainSection + "\n\n" + apart[lateAt:]; alike != want {
			t.Errorf("%s: a copy whose held call reads alike its late call is told\n%s\nwant\n%s", harness,
				alike, want)
		}
	}
}

// A CALL THE WORDS SAY THE SESSION LACKS BRINGS NO LISTING. A session whose
// harness ended its own run is told the calls after its save under words that
// say they are not in it, so no call there is shown twice and none is taken for
// a held one; and an owed read is told under words that say an earlier message
// counted it, a read the session may run again. Neither lists a held call
// again, alike or not.
//
// Mutation: match a held call against the calls after the save whatever their
// words, and the first case lists it again.
func TestACallTheWordsSayTheSessionLacksBringsNoListing(t *testing.T) {
	t.Parallel()
	saved := keptBasis
	saved.Through = 1
	owing := cutOffKept(2)
	owing.Owed = []int{2}
	read := func(seq int) runner.BridgedCall {
		return digested(numbered("slack_history", map[string]any{"channel": "C1"}, seq, seq))
	}
	for name, tc := range map[string]struct {
		basis   runner.SessionBasis
		bridged []runner.BridgedCall
		told    string
	}{
		"calls after a save the session ends at": {basis: saved,
			bridged: []runner.BridgedCall{post("hi", 1), post("hi", 2)},
			told:    "## Calls made after the session above was saved"},
		"an owed read": {basis: owing, bridged: []runner.BridgedCall{read(1), read(2)},
			told: "## Reads an earlier message counted"},
	} {
		brief := lostRelaunch(t, tc.basis, runner.Resume{Bridged: tc.bridged}).Session.Brief
		if !strings.Contains(brief, tc.told) {
			t.Fatalf("%s: the told call is not told under %q:\n%s", name, tc.told, brief)
		}
		if strings.Contains(brief, listedAgainHeading) {
			t.Errorf("%s: a held call is listed again:\n%s", name, brief)
		}
	}
}

// AN OPENCODE RELAUNCH WITH NO CALLS ALIKE IS TOLD AS MAIN TELLS IT. Listing a
// held call again changes a brief only where a held call and a told call read
// alike: of a relaunch whose held and told calls name one tool with arguments
// that read apart, every byte of the brief is what main handed before the
// listing existed, for a cut-off kept capture (its call after the save point
// and its late call beside the held one) and for the copy whose harness ended
// its own run (its late call beside the held one). The literals were rendered
// by main's own telling, before a call was ever listed again, and no change of
// this package's telling may move them without moving this test.
//
// Mutation: list a held call again beside every told call of its tool, read
// alike or not, or render the group of calls listed again where it is empty,
// and both cases go red.
func TestAnOpenCodeRelaunchWithNoCallsAlikeIsToldAsMainTellsIt(t *testing.T) {
	t.Parallel()
	late := digested(numbered("slack_post", map[string]any{"channel": "C1", "text": "a late post"}, 3, 3))
	late.Late = true
	kept := lostRelaunch(t, openCodeKept, runner.Resume{Bridged: []runner.BridgedCall{
		digested(numbered("slack_post", map[string]any{"channel": "C1", "text": "the held post"}, 1, 1)),
		digested(numbered("slack_post", map[string]any{"channel": "C1", "text": "a later post"}, 2, 2)),
		late,
	}}).Session.Brief
	if kept != mainKeptOpenCode {
		t.Errorf("a cut-off OpenCode capture with no calls alike is told\n%s\nwant main's\n%s", kept,
			mainKeptOpenCode)
	}
	lateInPlace := digested(numbered("slack_post", map[string]any{"channel": "C1", "text": "a late post"}, 2, 2))
	lateInPlace.Late = true
	inPlace := inPlaceOf(t, guarded{basis: inBoxBasis, bridged: []runner.BridgedCall{
		digested(numbered("slack_post", map[string]any{"channel": "C1", "text": "the held post"}, 1, 1)),
		lateInPlace,
	}, carry: func(r *runner.Resume) {
		answered("use main")(r)
		r.HarnessEnd, r.Run.Harness, r.BasisRestart = sandbox.HarnessEndOwnRun, "opencode", new("")
	}})
	if inPlace != mainInPlaceOpenCode {
		t.Errorf("an OpenCode copy whose harness ended its own run, with no calls alike, is told\n%s\n"+
			"want main's\n%s", inPlace, mainInPlaceOpenCode)
	}
}

// mainKeptOpenCode is the brief main hands a cut-off OpenCode capture whose
// held call, call after its save point and late call name one tool with
// arguments that read apart, as main told it before a held call was ever
// listed again.
const mainKeptOpenCode = "## Your previous segment was cut off\n" +
	"\n" +
	"You were working on this task when its host was drained, so you stopped mid-step, before you " +
	"finished, and did not get to report. What you did through your tools before you were cut off — a " +
	"comment posted, a work item updated, a file published — did happen: check before doing any of it " +
	"again. Then finish the task as it asks.\n" +
	"\n" +
	"## The session above was cut off\n" +
	"It was saved while its harness was still working, or as or after it was stopped, so it may end in " +
	"the middle of a call — a tool call with no result, which may or may not have run. Check what such a " +
	"call was meant to do before doing it again.\n" +
	"\n" +
	"## Calls this run made that the session above may not show\n" +
	"The session above may show some of these calls and not others: they are every call the run working " +
	"in it made through your tools since the last point the engine knows it reached. A call shown both " +
	"above and here is ONE call, made once, not two. They happened, and their outcomes stand: a call " +
	"marked `→ success` ALREADY RAN, so do NOT issue a write among them again; one marked `→ error` did " +
	"not take effect.\n" +
	"- slack_post({\"channel\":\"C1\",\"text\":\"a later post\"}) → success\n" +
	"\n" +
	"## Calls the run that made them stopped waiting for, or whose outcome is unknown\n" +
	"A run of this work made these calls through your tools, and each is one that run stopped waiting for " +
	"before it finished (it gave up on it, or it was cut off or replaced) or one whose outcome is " +
	"unknown. So the session above may show one with no outcome, as still running, as interrupted or as " +
	"failed, and an earlier message may have told it. Each is ONE call, made once, not two, and the " +
	"outcome told here stands: a call marked `→ success` ALREADY RAN, so do NOT issue a write among them " +
	"again; one marked `→ error` did not take effect, unless it says its outcome is unknown: that one may " +
	"have, so check whether it did before making it again.\n" +
	"- slack_post({\"channel\":\"C1\",\"text\":\"a late post\"}) → success"

// mainInPlaceOpenCode is the brief main hands the copy an OpenCode job's box
// holds where its harness ended its own run, whose held call and late call
// name one tool with arguments that read apart, as main told it before a held
// call was ever listed again.
const mainInPlaceOpenCode = "## Your question was answered\n" +
	"\n" +
	"You stopped to ask: which branch?\n" +
	"\n" +
	"founder answered: use main\n" +
	"\n" +
	"Carry on with the task from where you stopped, with this answer incorporated, and finish it as the " +
	"task asks.\n" +
	"\n" +
	"## Calls the run that made them stopped waiting for, or whose outcome is unknown\n" +
	"A run of this work made these calls through your tools, and each is one that run stopped waiting for " +
	"before it finished (it gave up on it, or it was cut off or replaced) or one whose outcome is " +
	"unknown. So the session above may show one with no outcome, as still running, as interrupted or as " +
	"failed, and an earlier message may have told it. Each is ONE call, made once, not two, and the " +
	"outcome told here stands: a call marked `→ success` ALREADY RAN, so do NOT issue a write among them " +
	"again; one marked `→ error` did not take effect, unless it says its outcome is unknown: that one may " +
	"have, so check whether it did before making it again.\n" +
	"- slack_post({\"channel\":\"C1\",\"text\":\"a late post\"}) → success"
