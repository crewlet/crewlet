package sandboxtest

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A LAUNCH RECORDS WHAT THE BRIEF IT HANDED LEFT OUT, AND EVERY WRITE OF THE
// JOB'S RECORD KEEPS IT. The reads of the round its brief counted without
// listing them, and those of the fresh brief its box falls back to, each with
// the late calls it left out apart ([sandbox.BriefTold.LateOmitted]), are
// written by the attach that names the job's command (never the one before
// it), and every later write of the record writes them back unchanged under
// their own keys. Only the next launch, replacing the record whole, says none.
//
// Mutation: drop any set from the attach, record them before the job, or leave
// them off the wire, and a check goes red.
func testALaunchRecordsWhatTheBriefItHandedLeftOut(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	const turn = "t-omitted"
	mustBeginLaunch(t, s, run(turn))
	if err := s.AttachSandbox(ctx, turn, sandbox.BoxRef{SandboxID: "box-omitted", BriefOmitted: []int{1},
		FreshOmitted: []int{2}}, sandbox.Fence{}); err != nil {
		t.Fatalf("the attach before the job: %v", err)
	}
	if got := mustGet(t, s, turn).Launch; len(got.BriefOmitted)+len(got.FreshOmitted) != 0 {
		t.Errorf("the attach before the job recorded %v and %v left out, want none: it names no command",
			got.BriefOmitted, got.FreshOmitted)
	}
	if err := s.AttachSandbox(ctx, turn, sandbox.BoxRef{SandboxID: "box-omitted", CommandID: "cmd-omitted",
		Brief: sandbox.BriefContinued, BriefOmitted: []int{13, 14}, FreshOmitted: []int{15},
		BriefLateOmitted: []int{3}, FreshLateOmitted: []int{4}}, sandbox.Fence{}); err != nil {
		t.Fatalf("the attach that names the job's command: %v", err)
	}
	check := func(step string) {
		t.Helper()
		got := mustGet(t, s, turn).Launch
		if !slices.Equal(got.BriefOmitted, []int{13, 14}) || !slices.Equal(got.FreshOmitted, []int{15}) {
			t.Errorf("after %s the job's record says its briefs left out %v and %v, want [13 14] and [15]",
				step, got.BriefOmitted, got.FreshOmitted)
		}
		if !slices.Equal(got.BriefLateOmitted, []int{3}) || !slices.Equal(got.FreshLateOmitted, []int{4}) {
			t.Errorf("after %s the job's record says its briefs left out late calls %v and %v, want [3] and [4]",
				step, got.BriefLateOmitted, got.FreshLateOmitted)
		}
	}
	check("the attach that names the job's command")
	if suspended, err := s.MarkSuspended(ctx, turn, suspension()); err != nil || !suspended {
		t.Fatalf("MarkSuspended = %v, %v", suspended, err)
	}
	check("the park of its launch")
	if err := s.SetStatus(ctx, turn, sandbox.StatusRunning, sandbox.Fence{}); err != nil {
		t.Fatal(err)
	}
	check("a status write")
	if _, err := s.ClaimOwnership(ctx, turn, "node-b", 2); err != nil {
		t.Fatal(err)
	}
	check("a lease")
	launch := mustGet(t, s, turn).LaunchID
	if _, err := s.AppendBridgeCall(ctx, turn, sandbox.BridgeAppend{Launch: launch,
		Call: sandbox.BridgeCall{Name: "read_page"}}); err != nil {
		t.Fatal(err)
	}
	check("a bridged call")
	claimed := mustClaim(t, s, turn)
	check("the claim")
	mustRelease(t, s, claimed)
	check("its release")
	park(t, s, turn)
	check("a park on a question")
	mustBeginLaunch(t, s, run(turn))
	if got := mustGet(t, s, turn).Launch; len(got.BriefOmitted)+len(got.FreshOmitted)+
		len(got.BriefLateOmitted)+len(got.FreshLateOmitted) != 0 {
		t.Errorf("the next launch's job reads %v, %v, %v and %v left out, the job before's, want none",
			got.BriefOmitted, got.FreshOmitted, got.BriefLateOmitted, got.FreshLateOmitted)
	}
}

// THE TURN'S LEADS ARE KEPT BESIDE ITS ROW AND OUTLIVE EVERY RELAUNCH. What
// carried each session of the turn on is kept beside the run's record, in a
// part of its own (sandbox.PendingStore.Leads): the attach that names a job's
// command takes into it the leads its brief opened with of its own, each once,
// numbered as taken, held to their share with the oldest dropped first and
// still counted, and marks on the job's record, by those numbers, every lead
// its brief told (sandbox.LaunchRecord.LeadMarks). The record keeps the marks
// and the count, never a lead's bytes. The reset opening the next job clears
// the marks and leaves the leads and their count as they are, every key a later
// build added included. Mutation: clear the part at a reset, keep the job
// before's marks, write a lead back without its carried keys, lose the cut.
func testTheTurnsLeadsAreKeptBesideItsRowAndOutliveEveryRelaunch(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	const turn = "t-leads"
	// A THIRD OF THE SHARE EACH, so the third job's own lead drops the first.
	lead := func(job string) sandbox.KeptLead {
		return sandbox.KeptLead{Kind: sandbox.LeadKindAnswer, Job: job, Seq: 1, Round: 1,
			Sealed: bytes.Repeat([]byte("s"), sandbox.MaxKeptLeadBytes/3),
			Extra:  map[string]json.RawMessage{"later": json.RawMessage(`"a later build's key"`)}}
	}
	// attach is the attach that names the running job's command, its brief's
	// leads named for the run and the job as the row names them.
	attach := func(own []sandbox.KeptLead, told []sandbox.LeadRef) sandbox.PendingRun {
		t.Helper()
		row := mustGetIn(ctx, t, s, turn)
		if err := s.AttachSandbox(ctx, turn, sandbox.BoxRef{SandboxID: "box-leads", CommandID: "cmd-" + row.LaunchID,
			Brief: sandbox.BriefContinued, Leads: sandbox.BriefLeads{Run: row.LeadsRun, Job: row.LaunchID,
				Counted: row.LeadsAppended, Own: own, Told: told}}, sandbox.Fence{}); err != nil {
			t.Fatalf("the attach that names %s's command: %v", row.LaunchID, err)
		}
		return mustGetIn(ctx, t, s, turn)
	}
	read := func(row sandbox.PendingRun) sandbox.TurnLeads {
		t.Helper()
		leads, err := s.Leads(ctx, row)
		if err != nil {
			t.Fatalf("Leads: %v", err)
		}
		return leads
	}
	jobs := func(leads []sandbox.KeptLead) (names []string, numbers []int) {
		for _, kept := range leads {
			names, numbers = append(names, kept.Job), append(numbers, kept.Appended)
		}
		return names, numbers
	}
	asWritten := func(t *testing.T, kept sandbox.KeptLead, job string) {
		t.Helper()
		if !bytes.Equal(kept.Sealed, lead(job).Sealed) || string(kept.Extra["later"]) != `"a later build's key"` {
			t.Errorf("%s's lead is kept re-made: %d bytes sealed, carried keys %v", job, len(kept.Sealed),
				kept.Extra)
		}
	}
	mustBeginLaunch(t, s, run(turn))
	before := mustGet(t, s, turn)
	if err := s.AttachSandbox(ctx, turn, sandbox.BoxRef{SandboxID: "box-leads", Leads: sandbox.BriefLeads{
		Run: before.LeadsRun, Job: before.LaunchID, Own: []sandbox.KeptLead{lead("job-0")}}},
		sandbox.Fence{}); err != nil {
		t.Fatalf("the attach before the job: %v", err)
	}
	if got := read(mustGet(t, s, turn)); len(got.Kept) != 0 || got.Cut != 0 {
		t.Errorf("the attach before the job kept leads %+v with %d cut, want none: it names no command", got.Kept,
			got.Cut)
	}
	first := attach([]sandbox.KeptLead{lead("job-1")}, nil)
	if names, numbers := jobs(read(first).Kept); !slices.Equal(names, []string{"job-1"}) ||
		!slices.Equal(numbers, []int{1}) || !slices.Equal(first.Launch.LeadMarks, []int{1}) ||
		first.LeadsAppended != 1 {
		t.Fatalf("the first job keeps leads %v numbered %v, its record marks %v and counts %d; want job-1's, "+
			"numbered 1, marked and counted", names, numbers, first.Launch.LeadMarks, first.LeadsAppended)
	}
	sealed := base64.StdEncoding.EncodeToString(lead("job-1").Sealed)
	if raw, err := json.Marshal(first); err != nil || bytes.Contains(raw, []byte(sealed)) {
		t.Errorf("the job's record holds the bytes of the lead it marks (%v): it keeps the marks and the count "+
			"alone", err)
	}
	next := run(turn)
	next.Continues = true
	mustBeginLaunch(t, s, next)
	reset := mustGet(t, s, turn)
	if len(reset.Launch.LeadMarks) != 0 || reset.LeadsAppended != 1 || reset.LeadsRun != first.LeadsRun {
		t.Fatalf("the next job's record marks %v, counts %d and names run %q for its leads; want none of the "+
			"job before's marks, the count and the run kept (%q)", reset.Launch.LeadMarks, reset.LeadsAppended,
			reset.LeadsRun, first.LeadsRun)
	}
	got := read(reset)
	if names, _ := jobs(got.Kept); !slices.Equal(names, []string{"job-1"}) || got.Cut != 0 {
		t.Fatalf("after the reset the turn's leads read %v with %d cut, want job-1's, kept as they were", names,
			got.Cut)
	}
	asWritten(t, got.Kept[0], "job-1")
	second := attach([]sandbox.KeptLead{lead("job-2")}, []sandbox.LeadRef{{Job: "job-1", Kind: sandbox.LeadKindAnswer}})
	if names, numbers := jobs(read(second).Kept); !slices.Equal(names, []string{"job-1", "job-2"}) ||
		!slices.Equal(numbers, []int{1, 2}) || !slices.Equal(second.Launch.LeadMarks, []int{1, 2}) ||
		second.LeadsAppended != 2 {
		t.Errorf("the second job keeps leads %v numbered %v, its record marks %v and counts %d; want job-1's "+
			"told again and its own, both marked", names, numbers, second.Launch.LeadMarks, second.LeadsAppended)
	}
	// A SEND-BACK OPENS A NEW ROUND AND KEEPS THE TURN'S LEADS ALL THE SAME.
	mustBeginLaunch(t, s, run(turn))
	third := attach([]sandbox.KeptLead{lead("job-3")}, nil)
	got = read(third)
	if names, numbers := jobs(got.Kept); !slices.Equal(names, []string{"job-2", "job-3"}) ||
		!slices.Equal(numbers, []int{2, 3}) || got.Cut != 1 || !slices.Equal(third.Launch.LeadMarks, []int{3}) ||
		third.LeadsAppended != 3 {
		t.Errorf("the third job keeps leads %v numbered %v with %d cut, its record marks %v and counts %d; want "+
			"job-1 dropped and counted, job-2 told by no brief of its, and its own marked", names, numbers,
			got.Cut, third.Launch.LeadMarks, third.LeadsAppended)
	}
	for i, job := range []string{"job-2", "job-3"} {
		if i < len(got.Kept) {
			asWritten(t, got.Kept[i], job)
		}
	}
	mustBeginLaunch(t, s, next)
	fourth := mustGet(t, s, turn)
	if got := read(fourth); len(got.Kept) != 2 || got.Cut != 1 || fourth.LeadsAppended != 3 ||
		len(fourth.Launch.LeadMarks) != 0 {
		t.Errorf("the fourth job reads %d leads with %d cut, its record counting %d and marking %v; want the two "+
			"and the cut kept, the count kept and no mark", len(got.Kept), got.Cut, fourth.LeadsAppended,
			fourth.Launch.LeadMarks)
	}
}

// AN EARLIER RUN'S LEADS ARE NONE OF THE TURN'S NEXT RUN'S. A turn's record can
// be removed and created again, and the store may leave a part of the earlier
// run beside the new record (coord's removeRunPart leaves a part beside a
// record created again): the leads part names the run it is of, and the record
// the run its leads are kept for (sandbox.PendingRun.LeadsRun), so a part of
// another run reads as none of this one's, every lead the record counts said as
// not kept; and the record's next attach that takes a lead replaces it, its
// own lead numbered past every lead the record counted, so the leads the
// replaced part took with it are still counted.
//
// Mutation: read a part whatever run it names, take into another run's part,
// or start the replacing part's numbers at none, and a check goes red.
func testAnEarlierRunsLeadsAreNoneOfTheTurnsNextRuns(t *testing.T, s sandbox.PendingStore, runs coord.SandboxRuns) {
	ctx := t.Context()
	const turn = "t-leads-earlier"
	attach := func(job string) sandbox.PendingRun {
		t.Helper()
		row := mustGetIn(ctx, t, s, turn)
		own := sandbox.KeptLead{Kind: sandbox.LeadKindAnswer, Job: row.LaunchID, Seq: row.LaunchSeq, Round: 1,
			Sealed: []byte(job + "'s answer")}
		if err := s.AttachSandbox(ctx, turn, sandbox.BoxRef{SandboxID: "box-earlier", CommandID: "cmd-" + job,
			Brief: sandbox.BriefContinued, Leads: sandbox.BriefLeads{Run: row.LeadsRun, Job: row.LaunchID,
				Counted: row.LeadsAppended, Own: []sandbox.KeptLead{own}}}, sandbox.Fence{}); err != nil {
			t.Fatalf("the attach that names %s's command: %v", job, err)
		}
		return mustGetIn(ctx, t, s, turn)
	}
	mustBeginLaunch(t, s, run(turn))
	first := attach("job-1")
	// A PART OF ANOTHER RUN OF THE TURN, as a record removed and created again
	// can find beside it.
	earlier, err := json.Marshal(map[string]any{"run": "an-earlier-run", "appends": 1,
		"leads": []sandbox.KeptLead{{Kind: sandbox.LeadKindAnswer, Job: "job-earlier", Seq: 1, Round: 1,
			Appended: 1, Sealed: []byte("an earlier run's answer")}}})
	if err != nil {
		t.Fatal(err)
	}
	_, ok, err := runs.MutateSandboxRunPart(ctx, turn, coord.RunLeads,
		func(coord.Record, bool) ([]byte, error) { return earlier, nil })
	if err != nil || !ok {
		t.Fatalf("staging another run's leads beside the record: %v, %v", ok, err)
	}
	got, err := s.Leads(ctx, first)
	if err != nil || len(got.Kept) != 0 || got.Cut != 1 {
		t.Errorf("beside a part of another run the record reads leads %+v with %d cut, %v; want none of them, "+
			"and the one lead it counts said as not kept", got.Kept, got.Cut, err)
	}
	next := run(turn)
	next.Continues = true
	mustBeginLaunch(t, s, next)
	second := attach("job-2")
	got, err = s.Leads(ctx, second)
	if err != nil || len(got.Kept) != 1 || got.Kept[0].Job != second.LaunchID || got.Kept[0].Appended != 2 ||
		got.Cut != 1 || !slices.Equal(second.Launch.LeadMarks, []int{2}) || second.LeadsAppended != 2 {
		t.Errorf("the next attach reads leads %+v with %d cut, its record marking %v and counting %d, %v; want "+
			"its own lead alone, numbered 2 past the one the replaced part took with it, which is counted",
			got.Kept, got.Cut, second.Launch.LeadMarks, second.LeadsAppended, err)
	}
}

// A RESUMED TURN'S CARRY IS ITS CLAIM'S OWN. What the turn resuming from a
// collected job does with the job's round — collects it, or carries it on — is
// recorded on the claim of that job as the turn begins, and only there: never on
// a row that is not claimed, nor for another job, nor under a lease a newer one
// outranks, nor once the run's ending is decided; a carry that is none is
// refused. The release that hands the claim back takes it with it — the next
// claim's turn says it again — and the next launch's job starts with nothing
// said.
//
// Mutation: write the carry on a row that is not the claim, keep it across a
// release or a relaunch, or take it under an outranked fence, and a check goes
// red.
func testAResumedTurnsCarryIsItsClaimsOwn(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	launch := mustLaunched(t, s, run("t-carry"))
	fence := sandbox.Fence{}
	if ok, err := s.MarkResumeBegun(ctx, "t-carry", launch, fence, sandbox.ResumeCollects); err != nil || ok {
		t.Errorf("a carry on a running row nobody claimed = %v, %v, want nothing written", ok, err)
	}
	claimed := mustClaim(t, s, "t-carry")
	if _, err := s.MarkResumeBegun(ctx, "t-carry", launch, fence, "collected"); err == nil {
		t.Error("a carry that is none was taken")
	}
	if ok, err := s.MarkResumeBegun(ctx, "t-carry", "another-job", fence, sandbox.ResumeCollects); err != nil || ok {
		t.Errorf("a carry for a job the claim does not hold = %v, %v", ok, err)
	}
	if ok, err := s.MarkResumeBegun(ctx, "t-carry", launch, sandbox.Fence{Owner: "node-x", Epoch: 7},
		sandbox.ResumeCollects); err != nil || !ok {
		t.Fatalf("MarkResumeBegun under the claim = %v, %v", ok, err)
	}
	if got := mustGet(t, s, "t-carry").Launch.ResumeCarry; got != sandbox.ResumeCollects {
		t.Fatalf("the claim's job says %q, want %q", got, sandbox.ResumeCollects)
	}
	if _, err := s.ClaimOwnership(ctx, "t-carry", "node-y", 9); err != nil {
		t.Fatalf("ClaimOwnership: %v", err)
	}
	if ok, err := s.MarkResumeBegun(ctx, "t-carry", launch, sandbox.Fence{Owner: "node-x", Epoch: 7},
		sandbox.ResumeCarriesOn); err != nil || ok {
		t.Errorf("a carry under a lease the row's newer one outranks = %v, %v", ok, err)
	}
	claimed.Owner, claimed.OwnerEpoch = "node-y", 9
	mustRelease(t, s, claimed)
	if got := mustGet(t, s, "t-carry").Launch.ResumeCarry; got != "" {
		t.Errorf("the released claim's job still says %q, want its next claim's turn to say it again", got)
	}
	mustClaim(t, s, "t-carry")
	if ok, err := s.MarkResumeBegun(ctx, "t-carry", launch, sandbox.Fence{Owner: "node-y", Epoch: 9},
		sandbox.ResumeCarriesOn); err != nil || !ok {
		t.Fatalf("MarkResumeBegun under the next claim = %v, %v", ok, err)
	}
	mustBeginLaunch(t, s, run("t-carry"))
	if got := mustGet(t, s, "t-carry").Launch.ResumeCarry; got != "" {
		t.Errorf("the next job's record says %q, want nothing said", got)
	}
	if ok, err := s.MarkResumeBegun(ctx, "t-none", "job-1", fence, sandbox.ResumeCollects); err != nil || ok {
		t.Errorf("a carry for a run with no record = %v, %v", ok, err)
	}
}
