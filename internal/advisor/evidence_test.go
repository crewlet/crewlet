package advisor_test

import (
	"go/build"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/worklet"
)

// kindOf names owner a person, ghost no seat, and every other handle an agent.
func kindOf(handle string) advisor.SeatKind {
	switch handle {
	case "owner":
		return advisor.SeatPerson
	case "ghost":
		return advisor.SeatUnknown
	}
	return advisor.SeatAgent
}

func named(handle string, told, woken int) worklet.NamedWithoutAsk {
	return worklet.NamedWithoutAsk{Handle: handle, Told: told, Woken: woken,
		Comments: []string{"c-1"}, WokenComments: []string{"c-1"}}
}

// EACH DETECTOR FIRES AT ITS THRESHOLD AND IS SILENT ONE BELOW IT, so a
// threshold moved by accident is a red case rather than a quieter screen.
func TestEachDetectorFiresAtItsThresholdAndNotBelow(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		work worklet.WorkPatterns
		want []advisor.Class
	}{
		"5 wakes in 500 agent comments": {worklet.WorkPatterns{AgentComments: 500,
			Named: []worklet.NamedWithoutAsk{named("pm", 3, 3), named("qa", 2, 2)}},
			[]advisor.Class{advisor.ClassAgentMentionsWithoutAsk}},
		"4 wakes": {worklet.WorkPatterns{AgentComments: 10,
			Named: []worklet.NamedWithoutAsk{named("pm", 4, 4)}}, nil},
		"5 wakes in 501 agent comments": {worklet.WorkPatterns{AgentComments: 501,
			Named: []worklet.NamedWithoutAsk{named("pm", 5, 5)}}, nil},
		"5 references that woke nobody": {worklet.WorkPatterns{AgentComments: 5,
			Named: []worklet.NamedWithoutAsk{named("pm", 5, 0)}}, nil},
		"5 hand-offs that said they ask": {worklet.WorkPatterns{AgentComments: 5,
			Named: []worklet.NamedWithoutAsk{{Handle: "pm", Told: 5, Asked: 5}}}, nil},
		"a removed seat named 5 times": {worklet.WorkPatterns{AgentComments: 5,
			Named: []worklet.NamedWithoutAsk{named("ghost", 5, 5)}}, nil},
		"a person named 5 times": {worklet.WorkPatterns{AgentComments: 5,
			Named: []worklet.NamedWithoutAsk{named("owner", 5, 5)}},
			[]advisor.Class{advisor.ClassPersonRequestsWithoutAsk}},
		"a person named 4 times": {worklet.WorkPatterns{AgentComments: 4,
			Named: []worklet.NamedWithoutAsk{named("owner", 4, 4)}}, nil},
		"3 overtaken of 60 answers": {worklet.WorkPatterns{Answers: 60, Superseded: 1, Moot: 2},
			[]advisor.Class{advisor.ClassAskOvertaken}},
		"3 overtaken of 61 answers": {worklet.WorkPatterns{Answers: 61, Superseded: 1, Moot: 2}, nil},
		"2 overtaken of 2 answers":  {worklet.WorkPatterns{Answers: 2, Moot: 2}, nil},
		"one idle review": {worklet.WorkPatterns{IdleReviews: []worklet.IdleReview{{Key: "W-1", Holder: "qa"}}},
			[]advisor.Class{advisor.ClassReviewIdle}},
		"no idle review": {worklet.WorkPatterns{IdleReviews: []worklet.IdleReview{}}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var got []advisor.Class
			for _, f := range advisor.Detect(advisor.Evidence{Work: tc.work}, kindOf) {
				got = append(got, f.Class)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("raised %v, want %v", got, tc.want)
			}
		})
	}
}

// THE BACKUP FINDING IS THE OWNER'S AND BLOCKS, while the backup alarm stands
// and only then; it is a step outside the document, with nothing to send.
func TestTheBackupFindingBlocksWhileTheBackupAlarmStands(t *testing.T) {
	t.Parallel()
	if got := advisor.Detect(advisor.Evidence{}, kindOf); len(got) != 0 {
		t.Fatalf("no standing alarm raised %v", got)
	}
	got := advisor.Detect(advisor.Evidence{Backup: advisor.Backup{Standing: true, Logs: 12}}, kindOf)
	if len(got) != 1 {
		t.Fatalf("a standing backup alarm raised %d findings, want 1", len(got))
	}
	f := got[0]
	if f.Class != advisor.ClassNoVerifiedBackup || f.Severity != advisor.SeverityBlocks ||
		f.Actor != advisor.ActorOwner || f.Fix.Kind != advisor.FixExternalStep ||
		len(f.Fix.Options) != 0 || f.Fix.Sentence == "" {
		t.Errorf("the backup finding is %+v, want the owner's, blocking, an external step", f)
	}
	if len(f.Evidence) != 1 || f.Evidence[0] != (advisor.Fact{Key: "logs", Value: 12}) {
		t.Errorf("the backup finding's evidence is %v, want logs=12", f.Evidence)
	}
}

// AN EVIDENCE FINDING IS THE ENGINE'S, never the owner's to-do: it offers no
// change to send, and names at most five records behind it.
func TestAnEvidenceFindingIsTheEnginesAndOffersNothingToSend(t *testing.T) {
	t.Parallel()
	many := make([]string, 9)
	for i := range many {
		many[i] = "c-" + strconv.Itoa(i)
	}
	work := worklet.WorkPatterns{AgentComments: 9, Answers: 3, Moot: 3, Overtaken: many[:5],
		Named: []worklet.NamedWithoutAsk{{Handle: "pm", Told: 9, Woken: 9, WokenComments: many[:5]},
			{Handle: "qa", Told: 4, Woken: 4, WokenComments: many[5:]}, {Handle: "owner", Told: 6, Comments: many[:5]}},
		IdleReviews: []worklet.IdleReview{{Task: "t-1", Key: "W-1", Holder: "qa"}}}
	found := advisor.Detect(advisor.Evidence{Work: work}, kindOf)
	// Every work class but approval.rubber_stamp, the owner's, held on its own below.
	if len(found) != len(advisor.WorkClasses())-1 {
		t.Fatalf("raised %d findings, want one per engine class the work patterns raise", len(found))
	}
	for _, f := range found {
		if f.Actor != advisor.ActorEngine || f.Source != advisor.SourceEvidence ||
			f.Fix.Kind != advisor.FixNone || len(f.Fix.Options) != 0 {
			t.Errorf("%s is %s from %s with a %s fix of %d options, want the engine's, "+
				"from evidence, with nothing to send", f.Class, f.Actor, f.Source, f.Fix.Kind, len(f.Fix.Options))
		}
		if len(f.Records) > worklet.MaxPatternExamples || len(f.Records) == 0 {
			t.Errorf("%s names %d records, want 1 to %d", f.Class, len(f.Records), worklet.MaxPatternExamples)
		}
		if f.Title == "" || f.Why == "" || len(f.Evidence) == 0 {
			t.Errorf("%s has no words or no evidence: %+v", f.Class, f)
		}
	}
}

// NO DETECTOR CALLS A MODEL: a finding is a fixed reading of records, and a
// model choosing one would let words in a comment steer what the owner is told.
func TestNoDetectorCallsAModel(t *testing.T) {
	t.Parallel()
	pkg, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, imp := range pkg.Imports {
		for _, banned := range []string{"internal/providers", "internal/assistant", "internal/agent",
			"internal/learning"} {
			if strings.Contains(imp, banned) {
				t.Errorf("internal/advisor imports %s: a detector reads records and calls no model", imp)
			}
		}
	}
}

// THE ENGINE'S PATTERNS SIT BESIDE THE DOCUMENT'S FINDINGS, a seat named as
// the chart names it, and the document's own findings unchanged.
func TestReadWithEvidenceAddsTheEnginesPatternsToTheDocuments(t *testing.T) {
	t.Parallel()
	c := liveShape(t)
	in := advisor.Inputs{Company: c, Revision: "rev-1", Integrations: []integration.State{}}
	static := advisor.Read(in)
	work := worklet.WorkPatterns{AgentComments: 20,
		Named: []worklet.NamedWithoutAsk{named("pm", 6, 6), named("owner", 5, 5)}}
	got := advisor.ReadWithEvidence(in, advisor.Evidence{Work: work})
	var engine []string
	for _, f := range got.Findings {
		if f.Actor == advisor.ActorEngine {
			engine = append(engine, string(f.Class)+":"+f.Subject.Name)
		}
	}
	slices.Sort(engine)
	want := []string{"person.requests_without_ask:Owner", "wake.agent_mentions_without_ask:"}
	if !slices.Equal(engine, want) {
		t.Errorf("engine findings %v, want %v", engine, want)
	}
	if len(got.Findings) != len(static.Findings)+len(want) || got.Revision != "rev-1" {
		t.Errorf("%d findings at %s, want the document's %d and %d more at rev-1",
			len(got.Findings), got.Revision, len(static.Findings), len(want))
	}
}

// THE TRACKER READ COVERS THE WEEK BEFORE NOW and calls a review idle after a
// day untouched, over every agent seat and no person.
func TestThePatternsWindowIsAWeekAndAReviewIsIdleAfterADay(t *testing.T) {
	t.Parallel()
	now := time.Date(2030, 1, 9, 12, 0, 0, 0, time.UTC)
	q := advisor.PatternsWindow(now, liveShape(t))
	if !q.Until.Equal(now) || !q.Since.Equal(now.Add(-7*24*time.Hour)) ||
		!q.QuietSince.Equal(now.Add(-24*time.Hour)) {
		t.Errorf("the window is %s to %s, quiet since %s; want the week before %s and a day", q.Since,
			q.Until, q.QuietSince, now)
	}
	if !slices.Equal(q.Reviewers, []string{"pm", "designer", "app", "backend", "qa", "sre"}) {
		t.Errorf("the reviewers are %v, want every agent seat in the chart's order", q.Reviewers)
	}
}

// A PERSON WHO TAKES ONE AGENT'S RECOMMENDATION NEARLY EVERY TIME is told so,
// as the owner's call: the fix is a draft Ask Crewlet writes for review, never
// a change, and below the threshold nothing is said.
func TestDecisionsTakenAsRecommendedAreTheOwnersToWeigh(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                 string
		decided, recommended int
		want                 bool
	}{
		{"nine in ten of twenty", 20, 18, true},
		{"every one of thirty", 30, 30, true},
		{"too few to say", 19, 19, false},
		{"often declined", 20, 17, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			work := worklet.WorkPatterns{Decisions: []worklet.DecisionTally{
				{Person: "owner", Asker: "pm", Decided: tc.decided, Recommended: tc.recommended}}}
			found := advisor.Detect(advisor.Evidence{Work: work}, kindOf)
			if (len(found) == 1) != tc.want || len(found) > 1 {
				t.Fatalf("raised %+v, want a finding %v", found, tc.want)
			}
			if !tc.want {
				return
			}
			f := found[0]
			if f.Class != advisor.ClassApprovalRubberStamp || f.Actor != advisor.ActorOwner ||
				f.Severity != advisor.SeverityNote || f.Subject.Seat != "pm" ||
				f.Fix.Kind != advisor.FixDraftWithAssistant || len(f.Fix.Options) != 0 {
				t.Errorf("finding = %+v, want the owner's note about pm with a draft to review", f)
			}
			for _, want := range []string{"owner", "pm", strconv.Itoa(tc.decided), "Show me the change"} {
				if !strings.Contains(f.Fix.Prompt, want) {
					t.Errorf("the draft's prompt %q does not say %q", f.Fix.Prompt, want)
				}
			}
		})
	}
}
