package sandboxtest

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/redact/redacttest"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// item is the work item a launching turn was charged to.
var item = types.WorkItem{Backend: types.WorkWorklet, ID: "task-7", Key: "ENG-7", Project: "ENG"}

func testWorkItemSurvivesParkAndResume(t *testing.T, s sandbox.PendingStore) {
	// THE RESUMED TURN READS ITS ITEM OFF THE ROW, because nothing else can
	// name it: the dispatch that resolved it is gone, and the answer that
	// resumes a parked run is a chat message naming no item. So the item has
	// to survive every write between the launch and the resume — the
	// suspension, the claim, the park, the answer's claim — or the second
	// half of the turn is charged to nothing.
	r := run("t1")
	r.WorkItem = &item
	mustLaunched(t, s, r)
	claimed := mustClaim(t, s, "t1")
	if err := s.MarkAwaiting(t.Context(), "t1", sandbox.Clarification{
		AskedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("park: %v", err)
	}
	resumed, won, err := s.ClaimForResume(t.Context(), "t1", answerTo(t, s, "t1"), sandbox.Fence{})
	if err != nil || !won {
		t.Fatalf("claim the answer: won=%v err=%v", won, err)
	}
	for name, got := range map[string]sandbox.PendingRun{
		"claimed": claimed, "resumed": resumed, "read back": mustGet(t, s, "t1"),
	} {
		if got.WorkItem == nil || *got.WorkItem != item {
			t.Errorf("%s: work item = %+v, want %+v", name, got.WorkItem, item)
		}
	}
}

func testAParkedRunRecordsWhoItsAudienceIs(t *testing.T, s sandbox.PendingStore) {
	// THE PARK WRITES WHOM THE QUESTION IS PUT TO, beside the label the ask
	// recorded: the label alone ("manager") answered nobody's "what is
	// waiting on me". A new job opened on the row takes both away with the
	// question, because a question that is gone waits on nobody.
	mustBeginLaunch(t, s, run("t1"))
	if ok, err := s.MarkSuspended(t.Context(), "t1", suspension()); err != nil || !ok {
		t.Fatalf("suspend: %v %v", ok, err)
	}
	Ask(t, s, "t1", Question("which base branch?", sandbox.AskToManager))
	mustClaim(t, s, "t1")
	if err := s.MarkAwaiting(t.Context(), "t1", sandbox.Clarification{
		Answerers: sandbox.Audience{Handles: []string{"founder", "cto"}, Fallback: true},
		AskedAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatalf("park: %v", err)
	}
	got := mustGet(t, s, "t1")
	if got.Audience != "manager" || len(got.AudienceHandles) != 2 ||
		got.AudienceHandles[0] != "founder" || got.AudienceHandles[1] != "cto" ||
		!got.AudienceFallback {
		t.Fatalf("the park recorded audience %q handles %v fallback %v, want "+
			"manager, [founder cto], true", got.Audience, got.AudienceHandles, got.AudienceFallback)
	}

	mustBeginLaunch(t, s, run("t1"))
	again := mustGet(t, s, "t1")
	if again.Audience != "" || len(again.AudienceHandles) != 0 || again.AudienceFallback {
		t.Errorf("a new job kept the last question's audience %q %v (fallback %v): "+
			"the run would read as waiting on people nobody is asking",
			again.Audience, again.AudienceHandles, again.AudienceFallback)
	}
}

func testARequesterSurvivesTheLaunchThatRecordsIt(t *testing.T, s sandbox.PendingStore) {
	// WHO WOKE THE TURN is written by the launch and read by the park,
	// which runs when the job finishes — so every write between the two
	// has to hand it on.
	r := run("t1")
	r.Requester = "ada"
	mustBeginLaunch(t, s, r)
	if ok, err := s.MarkSuspended(t.Context(), "t1", suspension()); err != nil || !ok {
		t.Fatalf("suspend: %v %v", ok, err)
	}
	claimed := mustClaim(t, s, "t1")
	if claimed.Requester != "ada" {
		t.Fatalf("the claim read requester %q, want ada", claimed.Requester)
	}
	if err := s.MarkAwaiting(t.Context(), "t1", sandbox.Clarification{AskedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("park: %v", err)
	}
	if got := mustGet(t, s, "t1"); got.Requester != "ada" {
		t.Errorf("the park left requester %q, want ada", got.Requester)
	}
}

func testAColleaguesAskSurvivesEveryWriteOfItsTurn(t *testing.T, s sandbox.PendingStore) {
	// THE ASK THE TURN ANSWERS is written by the turn's first launch and read
	// by the frame that ends the turn — a resume, or a stop's close — so
	// every write between the two hands it on, a relaunch's reset included:
	// a reviewer's send-back is the same turn answering the same ask, and
	// the relaunch's own row states none. The question holds a credential,
	// and every write hands it on AS ASKED, since the answer echoes it to the
	// asking seat's model: only the reads that serve it redact it.
	sample := map[string]string{}
	for _, shape := range redacttest.Shapes() {
		sample[shape.Name] = shape.Sample
	}
	key := sample["anthropic-key"]
	want := sandbox.ColleagueAsk{
		ChannelID: "a2a-1", Chain: []string{"ceo"},
		TraceID: "tr-ask", SpanID: "sp-ask", ParentSpanID: "pp-ask",
	}.WithQuestion("which branch, and may it use " + key + "?")
	r := run("t1")
	asked := want
	r.ColleagueAsk = &asked
	mustLaunched(t, s, r)
	check := func(step string, got sandbox.PendingRun) {
		t.Helper()
		if got.ColleagueAsk == nil || got.ColleagueAsk.ChannelID != want.ChannelID ||
			got.ColleagueAsk.RawQuestion() != want.RawQuestion() || len(got.ColleagueAsk.Chain) != 1 ||
			got.ColleagueAsk.Chain[0] != "ceo" || got.ColleagueAsk.TraceID != want.TraceID ||
			got.ColleagueAsk.SpanID != want.SpanID || got.ColleagueAsk.ParentSpanID != want.ParentSpanID {
			t.Fatalf("%s left ask %+v, want %+v", step, got.ColleagueAsk, want)
		}
		if shown := got.ColleagueAsk.Question(); strings.Contains(shown, key) || !strings.Contains(shown, redact.Marker) {
			t.Fatalf("%s left an ask whose question reads %q, want the key as a marker", step, shown)
		}
	}
	check("the launch", mustGet(t, s, "t1"))
	check("the claim", mustClaim(t, s, "t1"))
	if err := s.MarkAwaiting(t.Context(), "t1", sandbox.Clarification{AskedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("park: %v", err)
	}
	check("the park", mustGet(t, s, "t1"))
	mustBeginLaunch(t, s, run("t1"))
	check("a relaunch", mustGet(t, s, "t1"))
}

// audienceQuorum is a key no build has declared: the stand-in for whatever a
// newer build adds to the row next.
const audienceQuorum = "audience_quorum"

func testAudienceFieldsSurviveAStatusFlipByABuildThatDoesNotKnowThem(
	t *testing.T, s sandbox.PendingStore, runs coord.SandboxRuns,
) {
	// EVERY WRITE HERE IS A READ-MODIFY-WRITE OF THE WHOLE ROW, and a fleet
	// mid-upgrade has an older build doing them. The row is written the way
	// a NEWER build would write it — the audience fields this build knows,
	// plus one it has never heard of — and then this build, playing the
	// older half, drives every flip a parked run goes through. Each has to
	// hand the row back with all of it, or the first claim an older node
	// makes deletes what the newer one wrote.
	r := run("t1")
	r.WorkItem = &item
	r.AudienceHandles = []string{"ada", "grace"}
	r.AudienceFallback = true
	r.Status = sandbox.StatusLaunching
	body, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	fields := map[string]json.RawMessage{}
	if err = json.Unmarshal(body, &fields); err != nil {
		t.Fatal(err)
	}
	fields[audienceQuorum] = json.RawMessage(`{"min":2,"of":["ada","grace"]}`)
	body, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if created, err := runs.CreateSandboxRun(t.Context(), "t1", body, nil); err != nil || !created {
		t.Fatalf("seed the newer build's row: created=%v err=%v", created, err)
	}

	check := func(step string) {
		t.Helper()
		record, found, err := runs.SandboxRun(t.Context(), "t1")
		if err != nil || !found {
			t.Fatalf("%s: read the raw row: found=%v err=%v", step, found, err)
		}
		var got map[string]json.RawMessage
		if err := json.Unmarshal(record.Value, &got); err != nil {
			t.Fatalf("%s: %v", step, err)
		}
		if string(got[audienceQuorum]) != `{"min":2,"of":["ada","grace"]}` {
			t.Errorf("%s dropped a key this build does not know: %s = %s",
				step, audienceQuorum, got[audienceQuorum])
		}
		read := mustGet(t, s, "t1")
		if len(read.AudienceHandles) != 2 || !read.AudienceFallback ||
			read.WorkItem == nil || *read.WorkItem != item {
			t.Errorf("%s dropped the audience or the item: %+v %v %+v", step,
				read.AudienceHandles, read.AudienceFallback, read.WorkItem)
		}
	}

	if ok, err := s.MarkSuspended(t.Context(), "t1", suspension()); err != nil || !ok {
		t.Fatalf("suspend: %v %v", ok, err)
	}
	check("the suspension")
	claimed := mustClaim(t, s, "t1")
	check("the claim")
	mustRelease(t, s, claimed)
	check("the release")
	mustClaim(t, s, "t1")
	// THE PARK IS THE WRITE THAT OWNS THE AUDIENCE, so it states the same
	// resolution the newer build parked with; what it must not drop is the
	// key it has never heard of, and the item.
	if err := s.MarkAwaiting(t.Context(), "t1", sandbox.Clarification{
		Answerers: sandbox.Audience{Handles: []string{"ada", "grace"}, Fallback: true},
		AskedAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatalf("park: %v", err)
	}
	check("the park")
	if ok, err := s.ClaimOwnership(t.Context(), "t1", "node-b", 3); err != nil || !ok {
		t.Fatalf("move: %v %v", ok, err)
	}
	check("the ownership move")
}
