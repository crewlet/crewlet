package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/memory"
)

// THE TURN'S LEADS BESIDE ITS ROW (leads.go, coordstore.go): the part that keeps
// them names the run it is of, the row names the run and counts the leads the
// part has taken, the attach takes a brief's own leads into it before it writes
// the row, and a resume reads them as the row expects them.

// stageLeads writes value as turnID's leads part whatever it holds, as a store
// another writer used, or a removal left, would hold it.
func stageLeads(t *testing.T, runs coord.SandboxRuns, turnID string, value []byte) {
	t.Helper()
	if _, ok, err := runs.MutateSandboxRunPart(t.Context(), turnID, coord.RunLeads,
		func(coord.Record, bool) ([]byte, error) { return value, nil }); err != nil || !ok {
		t.Fatalf("writing %s's leads part: %v, %v", turnID, ok, err)
	}
}

// encodedPart is part as the store keeps it.
func encodedPart(t *testing.T, part leadsPart) []byte {
	t.Helper()
	raw, err := json.Marshal(part)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// keptAnswer is an answer lead of job, as the part keeps it before a take
// numbers it.
func keptAnswer(job string) KeptLead {
	return KeptLead{Kind: LeadKindAnswer, Job: job, Seq: 1, Round: 1, Sealed: []byte(job + "'s answer")}
}

// THE RUN IS NAMED FOR ITS LEADS BY ITS CREATE, AND KEPT BY EVERY RESET. The
// leads part outlives every relaunch as the row does, and a turn's row can be
// removed and created again beside a part an earlier run left, so the row names
// the run its leads are kept for: the job whose launch created it, and for a
// row an older build created, the first job this build's relaunch opened on it.
// Nothing renames it, a reset keeps the count of the leads its part has taken
// too, and neither is ever the caller's to hand in.
//
// Mutation: let a reset name the run every time, the create name none, or the
// create take the caller's name, and a check goes red.
func TestTheRunIsNamedForItsLeadsByItsCreateAndKeptByEveryReset(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	runs := memory.NewFleet()
	store := NewCoordStore(runs)
	first, err := store.BeginLaunch(ctx, PendingRun{TurnID: "t1", AgentHandle: "swe", LeadsRun: "a-callers-run",
		LeadsAppended: 9}, Fence{})
	if err != nil {
		t.Fatal(err)
	}
	if row, _, _ := store.Get(ctx, "t1"); first.LeadsRun != first.LaunchID || row.LeadsRun != first.LaunchID ||
		first.LeadsAppended != 0 || row.LeadsAppended != 0 {
		t.Fatalf("the create names run %q and counts %d (the row %q, %d); want the job that created it, %s, and "+
			"none: never the caller's", first.LeadsRun, first.LeadsAppended, row.LeadsRun, row.LeadsAppended,
			first.LaunchID)
	}
	if err := store.AttachSandbox(ctx, "t1", BoxRef{SandboxID: "box-1", CommandID: "cmd-1",
		Brief: BriefContinued, Leads: BriefLeads{Run: first.LeadsRun, Job: first.LaunchID,
			Own: []KeptLead{keptAnswer(first.LaunchID)}}}, Fence{}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginLaunch(ctx, PendingRun{TurnID: "t1", AgentHandle: "swe", Continues: true,
		LeadsRun: "a-callers-run", LeadsAppended: 9}, Fence{}); err != nil {
		t.Fatal(err)
	}
	if row, _, _ := store.Get(ctx, "t1"); row.LeadsRun != first.LaunchID || row.LeadsAppended != 1 ||
		row.LaunchID == first.LaunchID {
		t.Errorf("a relaunch names run %q and counts %d; want the run the create named, %s, and the one lead its "+
			"part took", row.LeadsRun, row.LeadsAppended, first.LaunchID)
	}
	// A ROW AN OLDER BUILD CREATED, which names no run for its leads.
	if made, err := runs.CreateSandboxRun(ctx, "t2",
		[]byte(`{"turn_id":"t2","agent_handle":"swe","status":"running","launch_id":"an-older-job","launch_seq":1}`)); err != nil || !made {
		t.Fatalf("creating an older build's row: %v, %v", made, err)
	}
	named, err := store.BeginLaunch(ctx, PendingRun{TurnID: "t2", AgentHandle: "swe", Continues: true}, Fence{})
	if err != nil {
		t.Fatal(err)
	}
	if named.LeadsRun != named.LaunchID {
		t.Errorf("the first relaunch of an older build's row names run %q, want its own job, %s", named.LeadsRun,
			named.LaunchID)
	}
	if _, err := store.BeginLaunch(ctx, PendingRun{TurnID: "t2", AgentHandle: "swe", Continues: true}, Fence{}); err != nil {
		t.Fatal(err)
	}
	if row, _, _ := store.Get(ctx, "t2"); row.LeadsRun != named.LaunchID {
		t.Errorf("the next relaunch names run %q, want the one the first named, %s", row.LeadsRun, named.LaunchID)
	}
}

// A RESUME READS EVERY LEAD ITS ROW COUNTS, OR SAYS IT IS NOT KEPT. The row says
// what the part beside it should hold: the run the leads are of and how many
// the part has taken. So the read gives back the run's own part as it is, its
// cut the leads it took and no longer keeps, and counts against the row every
// lead the part cannot give back: none beside the row, a part short of the
// row's count, a part of another run. A part that took more than the row counts
// (its attach took the leads and never landed) is read whole. A row naming no
// run (an older build's, never relaunched here) reads no part as its own. And
// a part that does not decode is an error, never "none".
//
// Mutation: cut by the part's count alone, read a part whatever run it names,
// or read one that does not decode as none, and a case goes red.
func TestAResumeReadsEveryLeadItsRowCountsOrSaysItIsNotKept(t *testing.T) {
	t.Parallel()
	numbered := func(job string, n int) KeptLead {
		lead := keptAnswer(job)
		lead.Appended = n
		return lead
	}
	for name, tc := range map[string]struct {
		part    []byte
		run     string
		counted int
		kept    int
		cut     int
		fails   bool
	}{
		"its own part": {part: encodedPart(t, leadsPart{Run: "run-1", Appends: 4,
			Leads: []KeptLead{numbered("job-3", 3), numbered("job-4", 4)}}), run: "run-1", counted: 4, kept: 2,
			cut: 2},
		"no part where the row counts three": {run: "run-1", counted: 3, cut: 3},
		"a part short of the row": {part: encodedPart(t, leadsPart{Run: "run-1", Appends: 1,
			Leads: []KeptLead{numbered("job-1", 1)}}), run: "run-1", counted: 3, kept: 1, cut: 2},
		"a part past the row": {part: encodedPart(t, leadsPart{Run: "run-1", Appends: 2,
			Leads: []KeptLead{numbered("job-1", 1), numbered("job-2", 2)}}), run: "run-1", counted: 1, kept: 2},
		"another run's part": {part: encodedPart(t, leadsPart{Run: "run-0", Appends: 1,
			Leads: []KeptLead{numbered("job-0", 1)}}), run: "run-1", counted: 2, cut: 2},
		"a row naming no run": {part: encodedPart(t, leadsPart{Run: "run-1", Appends: 1,
			Leads: []KeptLead{numbered("job-1", 1)}})},
		"a part that does not decode": {part: []byte(`{"run":`), run: "run-1", counted: 2, fails: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runs := memory.NewFleet()
			store := NewCoordStore(runs)
			row, err := store.BeginLaunch(t.Context(), PendingRun{TurnID: "t1", AgentHandle: "swe"}, Fence{})
			if err != nil {
				t.Fatal(err)
			}
			if tc.part != nil {
				stageLeads(t, runs, "t1", tc.part)
			}
			row.LeadsRun, row.LeadsAppended = tc.run, tc.counted
			got, err := store.Leads(t.Context(), row)
			switch {
			case tc.fails:
				if err == nil {
					t.Errorf("a part that does not decode reads as %+v, want an error", got)
				}
			case err != nil || len(got.Kept) != tc.kept || got.Cut != tc.cut:
				t.Errorf("the read gives %d leads with %d cut, %v; want %d with %d cut", len(got.Kept), got.Cut,
					err, tc.kept, tc.cut)
			}
		})
	}
}

// A LEADS PART OF ANOTHER RUN IS REPLACED ONLY BY THE RUN ITS ROW NAMES. The
// store may leave a part of an earlier run of the turn beside the row, and a
// part may be one nothing can read: the take that finds either asks the row
// whose the part should be. Where the row names the take's run, the part is
// replaced and the take's own lead is its first. Where it names another, the
// take is a launch of a run that ended: it writes nothing, and its attach is
// refused, with the contract's two answers in its order, a lease the row
// outranks the launch's by first. The one thing such a launch can leave is a
// part where none stood, naming its ended run, which the row's next take
// replaces (and which no reader of the row reads as its own).
//
// Mutation: replace whatever part is there without asking the row, take into
// another run's part, refuse the row's own run, or answer an ending where the
// row's lease outranks, and a case goes red.
func TestALeadsPartOfAnotherRunIsReplacedOnlyByTheRunItsRowNames(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	// launched is t1's row on a store of its own, opened under fence.
	launched := func(t *testing.T, fence Fence) (*CoordStore, coord.SandboxRuns, PendingRun) {
		t.Helper()
		runs := memory.NewFleet()
		store := NewCoordStore(runs)
		row, err := store.BeginLaunch(ctx, PendingRun{TurnID: "t1", AgentHandle: "swe"}, fence)
		if err != nil {
			t.Fatal(err)
		}
		return store, runs, row
	}
	attach := func(store *CoordStore, b BriefLeads, fence Fence) error {
		return store.AttachSandbox(ctx, "t1", BoxRef{SandboxID: "box-1", CommandID: "cmd-" + b.Job,
			Brief: BriefContinued, Leads: b}, fence)
	}
	ownRead := func(t *testing.T, store *CoordStore, job string) {
		t.Helper()
		row, _, err := store.Get(ctx, "t1")
		if err != nil {
			t.Fatal(err)
		}
		got, err := store.Leads(ctx, row)
		if err != nil || len(got.Kept) != 1 || got.Kept[0].Job != job || got.Kept[0].Appended != 1 || got.Cut != 0 {
			t.Errorf("the run reads leads %+v with %d cut, %v; want %s's alone, numbered 1", got.Kept, got.Cut,
				err, job)
		}
	}
	ended := BriefLeads{Run: "an-ended-run", Job: "an-ended-job", Own: []KeptLead{keptAnswer("an-ended-job")}}
	t.Run("an earlier run's part beside the row's run", func(t *testing.T) {
		t.Parallel()
		store, runs, row := launched(t, Fence{})
		stageLeads(t, runs, "t1", encodedPart(t, leadsPart{Run: "an-earlier-run", Appends: 1,
			Leads: []KeptLead{{Kind: LeadKindAnswer, Job: "job-earlier", Seq: 1, Appended: 1, Sealed: []byte("x")}}}))
		if err := attach(store, BriefLeads{Run: row.LeadsRun, Job: row.LaunchID,
			Own: []KeptLead{keptAnswer(row.LaunchID)}}, Fence{}); err != nil {
			t.Fatalf("the row's own take beside an earlier run's part: %v", err)
		}
		ownRead(t, store, row.LaunchID)
	})
	t.Run("a part that does not decode beside the row's run", func(t *testing.T) {
		t.Parallel()
		store, runs, row := launched(t, Fence{})
		stageLeads(t, runs, "t1", []byte(`{"run":`))
		if err := attach(store, BriefLeads{Run: row.LeadsRun, Job: row.LaunchID,
			Own: []KeptLead{keptAnswer(row.LaunchID)}}, Fence{}); err != nil {
			t.Fatalf("the row's own take beside a part that does not decode: %v", err)
		}
		ownRead(t, store, row.LaunchID)
	})
	for name, tc := range map[string]struct {
		opened, stale Fence
		want, not     error
	}{
		"a launch of a run that ended": {want: ErrRunEnding, not: ErrSeatNotHeld},
		"a launch the row's lease outranks": {opened: Fence{Owner: "node-b", Epoch: 5},
			stale: Fence{Owner: "node-a", Epoch: 3}, want: ErrSeatNotHeld},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			store, runs, row := launched(t, tc.opened)
			if err := attach(store, BriefLeads{Run: row.LeadsRun, Job: row.LaunchID,
				Own: []KeptLead{keptAnswer(row.LaunchID)}}, tc.opened); err != nil {
				t.Fatal(err)
			}
			before, _, _ := runs.SandboxRunPart(ctx, "t1", coord.RunLeads)
			beforeRow, _, _ := runs.SandboxRun(ctx, "t1")
			err := attach(store, ended, tc.stale)
			if !errors.Is(err, tc.want) || (tc.not != nil && errors.Is(err, tc.not)) {
				t.Errorf("the ended run's attach answers %v, want %v", err, tc.want)
			}
			after, _, _ := runs.SandboxRunPart(ctx, "t1", coord.RunLeads)
			afterRow, _, _ := runs.SandboxRun(ctx, "t1")
			if !bytes.Equal(after.Value, before.Value) || afterRow.Version != beforeRow.Version {
				t.Errorf("the ended run's take moved the run: its leads %s, were %s; the row at %d, was at %d",
					after.Value, before.Value, afterRow.Version, beforeRow.Version)
			}
		})
	}
	t.Run("an ended run's take where no part stands", func(t *testing.T) {
		t.Parallel()
		store, runs, row := launched(t, Fence{Owner: "node-b", Epoch: 5})
		if err := attach(store, ended, Fence{Owner: "node-a", Epoch: 3}); !errors.Is(err, ErrSeatNotHeld) {
			t.Fatalf("the ended run's attach answers %v, want its row write refused, %v", err, ErrSeatNotHeld)
		}
		if left, found, _ := runs.SandboxRunPart(ctx, "t1", coord.RunLeads); !found ||
			!bytes.Contains(left.Value, []byte(`"run":"an-ended-run"`)) {
			t.Fatalf("the ended run's take left %s (%v); the case stages the part it lands where none stood",
				left.Value, found)
		}
		if got, err := store.Leads(ctx, row); err != nil || len(got.Kept) != 0 {
			t.Errorf("the row reads the ended run's leads as its own: %+v, %v", got.Kept, err)
		}
		if err := attach(store, BriefLeads{Run: row.LeadsRun, Job: row.LaunchID,
			Own: []KeptLead{keptAnswer(row.LaunchID)}}, Fence{Owner: "node-b", Epoch: 5}); err != nil {
			t.Fatalf("the row's own take beside the ended run's part: %v", err)
		}
		ownRead(t, store, row.LaunchID)
	})
}

// AN ATTACH MARKS LEADS ONLY ON THE JOB THEY WERE KEPT FOR. The take comes first
// and keeps what it took, but the marks are the job's: an attach whose brief's
// leads name another job than the one the row names, or the row's job under
// another run than the one the row names, marks none of them on the row and
// moves the row's count of them none, so no job is marked with another's leads,
// nor with leads a part of another run keeps under numbers of its own.
//
// Mutation: mark whatever job the row names, or under whatever run, and a case
// goes red.
func TestAnAttachMarksLeadsOnlyOnTheJobTheyWereKeptFor(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	for name, other := range map[string]func(row PendingRun) BriefLeads{
		"another job": func(row PendingRun) BriefLeads {
			return BriefLeads{Run: row.LeadsRun, Job: "another-job", Own: []KeptLead{keptAnswer("another-job")}}
		},
		// WHERE NO PART STANDS, the take lands one under the run it names
		// (the residual a launch of an ended run can leave) and asks the row
		// nothing.
		"the row's job under another run": func(row PendingRun) BriefLeads {
			return BriefLeads{Run: "another-run", Job: row.LaunchID, Own: []KeptLead{keptAnswer(row.LaunchID)}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runs := memory.NewFleet()
			store := NewCoordStore(runs)
			row, err := store.BeginLaunch(ctx, PendingRun{TurnID: "t1", AgentHandle: "swe"}, Fence{})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.AttachSandbox(ctx, "t1", BoxRef{SandboxID: "box-1", CommandID: "cmd-1",
				Brief: BriefContinued, Leads: other(row)}, Fence{}); err != nil {
				t.Fatal(err)
			}
			after, _, err := store.Get(ctx, "t1")
			if err != nil {
				t.Fatal(err)
			}
			if part, found, _ := runs.SandboxRunPart(ctx, "t1", coord.RunLeads); !found ||
				!bytes.Contains(part.Value, []byte(`"appended":1`)) {
				t.Fatalf("the take kept %s (%v); the case stages the lead it takes before the row's write",
					part.Value, found)
			}
			if len(after.Launch.LeadMarks) != 0 || after.LeadsAppended != 0 {
				t.Errorf("the row marks %v and counts %d; want nothing marked or counted on the row's job",
					after.Launch.LeadMarks, after.LeadsAppended)
			}
		})
	}
}

// AN ATTACH MARKS THE LEADS ITS BRIEF COULD NOT READ. A relaunch whose resume
// could not read the run's leads hands on the ones its job's record marks by
// their numbers alone, and its brief may open with no lead of its own (a
// replaced sign-in's carries none): the attach that names the command still
// takes, so the job it opens marks each of them the part keeps, for a
// continuation of its session to tell once a read answers, and counts them.
// Numbers alone write nothing beside the row.
//
// Mutation: take only where the brief opened with or told a lead by name, and
// the job marks none.
func TestAnAttachMarksTheLeadsItsBriefCouldNotRead(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	runs := memory.NewFleet()
	store := NewCoordStore(runs)
	first, err := store.BeginLaunch(ctx, PendingRun{TurnID: "t1", AgentHandle: "swe"}, Fence{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AttachSandbox(ctx, "t1", BoxRef{SandboxID: "box-1", CommandID: "cmd-1", Brief: BriefContinued,
		Leads: BriefLeads{Run: first.LeadsRun, Job: first.LaunchID, Own: []KeptLead{keptAnswer(first.LaunchID)}}},
		Fence{}); err != nil {
		t.Fatal(err)
	}
	next, err := store.BeginLaunch(ctx, PendingRun{TurnID: "t1", AgentHandle: "swe", Continues: true}, Fence{})
	if err != nil {
		t.Fatal(err)
	}
	before, _, _ := runs.SandboxRunPart(ctx, "t1", coord.RunLeads)
	if err := store.AttachSandbox(ctx, "t1", BoxRef{SandboxID: "box-1", CommandID: "cmd-2", Brief: BriefContinued,
		Leads: BriefLeads{Run: next.LeadsRun, Job: next.LaunchID, Counted: next.LeadsAppended,
			ToldUnread: []int{1, 7}}}, Fence{}); err != nil {
		t.Fatal(err)
	}
	after, _, err := store.Get(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(after.Launch.LeadMarks, []int{1}) || after.LeadsAppended != 1 {
		t.Errorf("the job whose brief could not read the leads marks %v and counts %d; want 1, the first job's "+
			"answer, which the part keeps, and not 7, which it never took", after.Launch.LeadMarks,
			after.LeadsAppended)
	}
	if part, _, _ := runs.SandboxRunPart(ctx, "t1", coord.RunLeads); part.Version != before.Version {
		t.Errorf("numbers alone wrote the run's leads: at %d, were at %d", part.Version, before.Version)
	}
}

// leadsRefused is a store whose every write of a run's leads part fails, as an
// unreachable coordination store's would, and whose every other call is the
// store's.
type leadsRefused struct{ leadsStore }

// leadsStore names the store leadsRefused embeds, whose own name is one of its
// methods.
type leadsStore = coord.SandboxRuns

func (r leadsRefused) MutateSandboxRunPart(ctx context.Context, turnID string, part coord.RunPart,
	decide func(coord.Record, bool) ([]byte, error)) (coord.Record, bool, error) {
	if part == coord.RunLeads {
		return coord.Record{}, false, errRefusedCall
	}
	return r.leadsStore.MutateSandboxRunPart(ctx, turnID, part, decide)
}

// A TAKE THE STORE CANNOT WRITE REFUSES THE ATTACH. The take comes before the
// row's write, so refusing it starts nothing the row names, as a refused row
// write does: the launch abandons its box. Gone on with instead, the job would
// run with a brief that told leads the run does not keep, and a later brief to
// a session that may lack it would tell less with nothing saying so. An attach
// whose brief opened with no lead of its own takes nothing, and lands.
//
// Mutation: log the take's failure and go on, and the row names the command.
func TestATakeTheStoreCannotWriteRefusesTheAttach(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := NewCoordStore(leadsRefused{memory.NewFleet()})
	row, err := store.BeginLaunch(ctx, PendingRun{TurnID: "t1", AgentHandle: "swe"}, Fence{})
	if err != nil {
		t.Fatal(err)
	}
	err = store.AttachSandbox(ctx, "t1", BoxRef{SandboxID: "box-1", CommandID: "cmd-1", Brief: BriefContinued,
		Leads: BriefLeads{Run: row.LeadsRun, Job: row.LaunchID, Own: []KeptLead{keptAnswer(row.LaunchID)}}}, Fence{})
	if !errors.Is(err, errRefusedCall) {
		t.Errorf("an attach whose take the store refused answers %v, want the store's refusal", err)
	}
	if after, _, _ := store.Get(ctx, "t1"); after.CommandID != "" || len(after.Launch.LeadMarks) != 0 {
		t.Errorf("the refused attach wrote the row: command %q, marks %v", after.CommandID, after.Launch.LeadMarks)
	}
	if err := store.AttachSandbox(ctx, "t1", BoxRef{SandboxID: "box-1", CommandID: "cmd-1",
		Brief: BriefContinued}, Fence{}); err != nil {
		t.Errorf("an attach with no lead to take answers %v, want it landed", err)
	}
	if after, _, _ := store.Get(ctx, "t1"); after.CommandID != "cmd-1" {
		t.Errorf("the attach with no lead names command %q, want cmd-1", after.CommandID)
	}
}

// A LEADS TAKE THAT NAMES NO RUN OR NO JOB IS REFUSED. A launch names both from
// the row it opened, so leads that name neither are a caller's fault: taken,
// they would land in a part no row reads as its own and mark no job, and the
// job would run on a brief whose leads nothing keeps. The attach is refused
// before anything is written, the part and the row as they were, and the
// launch abandons its box as for any refused attach.
//
// Mutation: take leads that name no run or no job, and a case goes red.
func TestALeadsTakeThatNamesNoRunOrNoJobIsRefused(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	for name, unnamed := range map[string]func(row PendingRun) BriefLeads{
		"no run": func(row PendingRun) BriefLeads {
			return BriefLeads{Job: row.LaunchID, Own: []KeptLead{keptAnswer(row.LaunchID)}}
		},
		"no job": func(row PendingRun) BriefLeads {
			return BriefLeads{Run: row.LeadsRun, Own: []KeptLead{keptAnswer(row.LaunchID)}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runs := memory.NewFleet()
			store := NewCoordStore(runs)
			row, err := store.BeginLaunch(ctx, PendingRun{TurnID: "t1", AgentHandle: "swe"}, Fence{})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.AttachSandbox(ctx, "t1", BoxRef{SandboxID: "box-1", CommandID: "cmd-1",
				Brief: BriefContinued, Leads: unnamed(row)}, Fence{}); err == nil {
				t.Errorf("an attach whose leads name %s landed, want it refused", name)
			}
			if part, found, _ := runs.SandboxRunPart(ctx, "t1", coord.RunLeads); found {
				t.Errorf("the refused attach wrote the run's leads: %s", part.Value)
			}
			if after, _, _ := store.Get(ctx, "t1"); after.CommandID != "" || len(after.Launch.LeadMarks) != 0 {
				t.Errorf("the refused attach wrote the row: command %q, marks %v", after.CommandID,
					after.Launch.LeadMarks)
			}
		})
	}
}

// A RESUME IS HANDED THE LEADS BESIDE ITS ROW, AND EVERY ONE IT CANNOT READ. The
// coordinator reads the turn's leads inside the resume's frame, beside the log,
// and hands them in the one request it builds; where the store cannot give them
// back, the resume still runs, told that every lead the row counts is not kept,
// never that there is none, and that the read failed, so the leads its job's
// brief told are handed on by their numbers for a later read to tell: a lead
// told again is a message the session may already show, and the brief says how
// many it cannot tell.
//
// Mutation: leave the leads off the request, hand an unread part on as none or
// as read, or refuse the resume for it, and a case goes red.
func TestAResumeIsHandedTheLeadsBesideItsRowAndEveryOneItCannotRead(t *testing.T) {
	t.Parallel()
	for name, unread := range map[string]bool{"read": false, "unread": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			rig := newCoordRig(t)
			row := rig.launch("t1")
			lead := sealedFor(t, rigLeads(), answerLead("use main"), row.LaunchID, row.LaunchSeq)
			if took, err := rig.pending.takeLeads(ctx, "t1", BriefLeads{Run: row.LeadsRun, Job: row.LaunchID,
				Own: []KeptLead{lead}}, Fence{}); err != nil || !took.landed {
				t.Fatalf("taking the job's lead: %+v, %v", took, err)
			}
			if _, ok, err := rig.pending.mutate(ctx, "t1", func(run *PendingRun) bool {
				run.LeadsAppended, run.Launch.LeadMarks = 1, []int{1}
				return true
			}); err != nil || !ok {
				t.Fatalf("counting the job's lead on its row: %v, %v", ok, err)
			}
			if unread {
				rig.coordinator.pending = &refusingStore{inner: rig.coordinator.pending, refuse: []string{"Leads"}}
			}
			rig.runner.Finish(Result{Success: true})
			payload, ev := rig.completion("t1")
			if err := rig.coordinator.OnCompleted(ctx, payload, ev); err != nil {
				t.Fatalf("OnCompleted: %v", err)
			}
			calls := rig.resumer.calls()
			if len(calls) != 1 {
				t.Fatalf("resumed %d times, want once whether or not the leads could be read", len(calls))
			}
			got := calls[0].Leads
			switch {
			case unread && (len(got.Kept) != 0 || got.Cut != 1 || !got.Unread):
				t.Errorf("the resume is handed leads %+v with %d cut (unread %v), want none, the one the row "+
					"counts said as not kept, and the read said unread", got.Kept, got.Cut, got.Unread)
			case !unread && (len(got.Kept) != 1 || !bytes.Equal(got.Kept[0].Sealed, lead.Sealed) || got.Cut != 0 ||
				got.Unread):
				t.Errorf("the resume is handed leads %+v with %d cut (unread %v), want the job's one lead, read",
					got.Kept, got.Cut, got.Unread)
			}
		})
	}
}
