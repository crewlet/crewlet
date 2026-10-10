package queries

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tracker"
)

// storyFields is every field a story carries, by its wire path, and why a
// member may read it — the withholding rule (ADR-0031) applied field by field.
// A REVIEWED LIST: a field added to [TurnStory] fails the gate until somebody
// writes down why it is something the company published.
var storyFields = map[string]string{
	"turn_id": "the session's identity, which every change it made already " +
		"names in the change feed",
	"seat":                  "who did the work: the chart is published",
	"work_item.backend":     "where the work lives",
	"work_item.id":          "the work item, which is published",
	"work_item.key":         "the work item, which is published",
	"work_item.title":       "the work item, which is published",
	"work_item.ordinal":     "the task's own count of its sessions, which its record carries",
	"started_at":            "when it worked on the work",
	"duration_ms":           "how long it worked on the work — a duration, never a cost",
	"why.origin":            "the kind of published surface it started from",
	"why.summary":           "the published trigger's own label, quoted only for a published origin",
	"why.by":                "who set it going, by handle",
	"did[].kind":            "a typed action, never a tool name",
	"did[].at":              "when",
	"did[].pages[].backend": "which knowledge base holds a page it opened",
	"did[].pages[].id":      "a page it opened, which is published",
	"did[].pages[].title":   "a page it opened, which is published",
	"left_behind[].kind":    "a task or a page",
	"left_behind[].id":      "a task or page it changed, which is published",
	"left_behind[].key":     "a task it changed, which is published",
	"left_behind[].title":   "a task or page it changed, which is published",
	"left_behind[].change": "what the change was, in the vocabulary the item's " +
		"own published history uses",
	"left_behind[].status": "the status a task moved to, which its record shows",
	"left_behind[].at":     "when the change landed",
	"outcome":              "how the session ended, as a closed set of five",
	"helpers":              "how many helpers it used — a count, never their prompts",
	"consulted":            "which colleagues it asked, by handle — never what was said",
	"your_note.note":       "the caller's own words",
	"your_note.status":     "whether the session read the caller's note",
	"your_note.sent_at":    "when the caller sent it",
	"your_note.at":         "when it was read or expired",
}

// machineWords are fragments no story field may be named after: each is a
// thing the machine processed or how it is run, which ADR-0031 keeps for
// admins. A field whose wire name carries one is a field somebody is about to
// publish without deciding to.
var machineWords = []string{"prompt", "tool", "argument", "model", "node", "token",
	"cache", "cost", "spend", "trace", "span", "round", "phase", "query", "response",
	"narration", "diary", "episode", "budget", "provider", "sandbox", "box"}

// storyPaths walks a type's wire fields, nested structs, slices and pointers
// included, into the dotted paths [storyFields] keys on.
func storyPaths(t reflect.Type, prefix string, out map[string]bool) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	for field := range t.Fields() {
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		path := prefix + name
		inner := field.Type
		for inner.Kind() == reflect.Pointer {
			inner = inner.Elem()
		}
		switch {
		case inner.Kind() == reflect.Slice && inner.Elem().Kind() == reflect.Struct:
			storyPaths(inner.Elem(), path+"[].", out)
		case inner.Kind() == reflect.Struct && inner != reflect.TypeFor[time.Time]():
			storyPaths(inner, path+".", out)
		default:
			out[path] = true
		}
	}
}

// EVERY FIELD A STORY CARRIES IS CLASSIFIED, both ways, and none is named
// after what the machine processed.
//
// A story is the one answer about a turn a member may read, and the turn it is
// built from carries the prompts, the tools, the model, the node and the spend
// — so a field added here is the shortest route by which any of them reaches a
// teammate. The field is refused until this table says why it is published;
// an entry for a field that no longer exists is refused as stale; and a wire
// name carrying a machine word is refused whatever the table says.
func TestEveryTurnStoryFieldIsClassified(t *testing.T) {
	t.Parallel()
	paths := map[string]bool{}
	storyPaths(reflect.TypeFor[TurnStory](), "", paths)
	for path := range paths {
		why, ok := storyFields[path]
		switch {
		case !ok:
			t.Errorf("TurnStory carries %q, which is not classified — decide "+
				"whether the company published it (ADR-0031) and say why in "+
				"storyFields, or keep it off the story", path)
		case why == "":
			t.Errorf("%q is classified with no reason", path)
		}
		for _, word := range machineWords {
			if strings.Contains(path, word) {
				t.Errorf("TurnStory carries %q, named after %q — what the machine "+
					"processed or how it is run, which a member is never served", path, word)
			}
		}
	}
	for path := range storyFields {
		if !paths[path] {
			t.Errorf("storyFields classifies %q, which TurnStory no longer carries", path)
		}
	}
}

// EVERY ROW THE BAND RULE PLACES HAS A STORY READING, used or withheld with a
// reason — so a new kind of row reaches a story because somebody decided it
// should, never because it fell through, and a row the rule stopped placing is
// not still read by a story that thinks it is.
func TestEveryBandedRowHasAStoryReading(t *testing.T) {
	t.Parallel()
	registered := map[string]bool{}
	for _, name := range events.RegisteredTypes() {
		registered[name] = true
		if types.BandOf(name, false) == types.BandRest {
			continue
		}
		reading, ok := storyReadings[name]
		switch {
		case !ok:
			t.Errorf("%q is filed in the %s band and the story has no reading for "+
				"it — read it into the story or withhold it, with the reason",
				name, types.BandOf(name, false))
		case reading.Why == "":
			t.Errorf("the story's reading of %q says nothing about why", name)
		}
	}
	for name := range storyReadings {
		if !registered[name] || types.BandOf(name, false) == types.BandRest {
			t.Errorf("the story reads %q, which no band places — a stale reading "+
				"of a row the turn answer no longer files", name)
		}
	}
	for _, name := range []string{"episode_written", "reflection_completed",
		"counterparty_profile_updated", "skill_synthesized"} {
		if storyReadings[name].Read != nil {
			t.Errorf("the story reads %q — a reflection pass is what the seat "+
				"learned, which is never a story", name)
		}
	}
}

// storyRow is one stored row of a turn: its type, its payload as the event
// carries it, and its instant.
func storyRow(t *testing.T, at time.Time, payload events.Payload) store.EventRecord {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal %s: %v", payload.EventType(), err)
	}
	return store.EventRecord{
		ID: payload.EventType() + "-" + at.Format(time.RFC3339Nano), Type: payload.EventType(),
		Time: at, Payload: raw,
	}
}

var storyBase = time.Date(2026, 10, 15, 7, 0, 0, 0, time.UTC)

// at is the n-th minute of the session.
func at(n int) time.Time { return storyBase.Add(time.Duration(n) * time.Minute) }

var eng412 = &types.WorkItem{Backend: types.WorkNative, ID: "t-412", Key: "ENG-412", Project: "ENG"}

// opened is a session's opening record.
func opened(trigger types.Trigger, item *types.WorkItem) types.AgentTurnStarted {
	return types.AgentTurnStarted{
		Agent: "a-swe", AgentHandle: "swe", RoleName: "SWE", TurnID: "run-1",
		Trigger: trigger, WorkItem: item, StartedAt: at(0),
	}
}

// taskWake is a tracker wake from a person.
var taskWake = types.Trigger{Type: "external_notification", Integration: tracker.Source,
	Summary: "jane assigned ENG-412", Actor: "jane"}

// finished is a completed, undecorated session: the dashboard's record and the
// learning record.
func finished(t *testing.T, decision string, mutate func(*types.AgentTurnCompleted, *types.TurnCompleted)) []store.EventRecord {
	t.Helper()
	end := types.AgentTurnCompleted{Agent: "a-swe", RoleName: "SWE", TurnID: "run-1",
		Decision: decision, Model: "claude-secret-model", TotalTokens: 98765,
		Prompt: "SYSTEM PROMPT TEXT", Response: "THE RESPONSE TEXT"}
	learned := types.TurnCompleted{Agent: "a-swe", AgentHandle: "swe", TurnID: "run-1",
		ReviewOutcome: decision, DurationMS: 60_000, PlanSummary: "THE PLAN SUMMARY",
		AllToolNames: []string{"update_work_item_secret_tool"}}
	if mutate != nil {
		mutate(&end, &learned)
	}
	return []store.EventRecord{storyRow(t, at(10), end), storyRow(t, at(10), learned)}
}

func tell(t *testing.T, in storyInput) (TurnStory, bool) {
	t.Helper()
	if in.TurnID == "" {
		in.TurnID = "run-1"
	}
	return storyOf(in)
}

// A STORY CARRIES NOTHING THE MACHINE PROCESSED, whatever the turn held.
//
// Every row a turn can carry is filled with a value only it could produce — a
// prompt, a response, a tool and its arguments, a model, a token count, a
// search's query, a diary entry — and the story's wire form may contain none of
// them. The published facts are filled too and MUST appear, which is what
// stops this passing on an empty story.
func TestAStoryCarriesNothingTheMachineProcessed(t *testing.T) {
	t.Parallel()
	rows := []store.EventRecord{
		storyRow(t, at(0), opened(taskWake, eng412)),
		storyRow(t, at(1), types.PrefetchSummary{TurnID: "run-1"}),
		storyRow(t, at(1), types.PromptSize{TurnID: "run-1", SystemBytes: 31337}),
		storyRow(t, at(2), types.AgentPhaseCompleted{TurnID: "run-1", Model: "claude-secret-model",
			SystemPrompt: "SYSTEM PROMPT TEXT", Response: "THE RESPONSE TEXT",
			ToolExecutions: []types.ToolExecution{{"name": "update_work_item_secret_tool",
				"arguments": `{"secret":"ARGUMENT VALUE"}`}}}),
		storyRow(t, at(3), types.KnowledgeRead{TurnID: "run-1", Via: types.ReadViaSearch,
			Query: "THE SEARCH QUERY", Pages: []types.KnowledgeReadPage{{ID: "p-hidden", Title: "A RANKED HIT"}}}),
		storyRow(t, at(4), types.KnowledgeRead{TurnID: "run-1", Via: types.ReadViaGetPage,
			Backend: "native", Pages: []types.KnowledgeReadPage{{ID: "p-runbook", Title: "Billing runbook"}}}),
		storyRow(t, at(5), types.SkillUsed{TurnID: "run-1", SkillName: "SECRET SKILL NAME"}),
		storyRow(t, at(6), types.A2AMessageSent{TurnID: "run-1", Sender: "swe", Recipient: "cto",
			Content: "WHAT AGENTS SAID"}),
		storyRow(t, at(7), types.ProviderFallback{TurnID: "run-1", FromProviderKey: "SECRET PROVIDER"}),
		storyRow(t, at(11), types.EpisodeWritten{TurnID: "run-1", ReviewOutcome: "THE DIARY ENTRY"}),
	}
	rows = append(rows, finished(t, "done", nil)...)
	story, told := tell(t, storyInput{Rows: rows,
		Tasks: []tracker.TurnWrite{{TaskID: "t-412", Key: "ENG-412", Title: "Fix billing webhook",
			Kind: tracker.ChangeComment, At: at(8)}}})
	if !told {
		t.Fatal("a session charged to a task was not told")
	}
	raw, err := json.Marshal(story)
	if err != nil {
		t.Fatal(err)
	}
	wire := string(raw)
	for _, secret := range []string{"SYSTEM PROMPT TEXT", "THE RESPONSE TEXT",
		"update_work_item_secret_tool", "ARGUMENT VALUE", "claude-secret-model", "98765",
		"31337", "THE SEARCH QUERY", "A RANKED HIT", "SECRET SKILL NAME",
		"WHAT AGENTS SAID", "SECRET PROVIDER", "THE DIARY ENTRY", "THE PLAN SUMMARY"} {
		if strings.Contains(wire, secret) {
			t.Errorf("the story carries %q, which the machine processed:\n%s", secret, wire)
		}
	}
	for _, published := range []string{"ENG-412", "Billing runbook", "jane assigned ENG-412",
		"Fix billing webhook", `"outcome":"approved"`, `"seat":"swe"`} {
		if !strings.Contains(wire, published) {
			t.Errorf("the story does not carry %q, which it should tell:\n%s", published, wire)
		}
	}
}

// A SESSION IS A STORY WHEN IT IS CHARGED TO WORK OR STARTED FROM A PUBLISHED
// SURFACE, and never otherwise — not even as an empty shell.
func TestWhichSessionsAreStories(t *testing.T) {
	t.Parallel()
	dm := []types.InboundInteraction{{ChannelKind: types.ChannelDM}}
	channel := []types.InboundInteraction{{ChannelKind: types.ChannelPublic}}
	room := []types.InboundInteraction{{ChannelKind: types.ChannelPublic}, {ChannelKind: types.ChannelGroup}}
	chat := types.Trigger{Type: "external_notification", Integration: "slack",
		Summary: "Message from Ana: the numbers", Actor: "Ana"}
	ask := types.Trigger{Type: types.A2ARequestType, Summary: "cto asked a colleague", Actor: "cto"}
	alert := types.Trigger{Type: "external_notification", Integration: "datadog", Summary: "CPU high"}
	fire := types.Trigger{Type: "task_assigned", Summary: "Daily standup"}
	page := types.Trigger{Type: "external_notification", Integration: pages.Source,
		Summary: "jane commented on Runbook", Actor: "jane"}
	withInteractions := func(in []types.InboundInteraction) func(*types.AgentTurnCompleted, *types.TurnCompleted) {
		return func(_ *types.AgentTurnCompleted, learned *types.TurnCompleted) {
			learned.Interactions = in
		}
	}
	cases := []struct {
		why     string
		rows    []store.EventRecord
		told    bool
		origin  StoryOrigin
		summary string
	}{
		{"a task woke it", append([]store.EventRecord{storyRow(t, at(0), opened(taskWake, nil))},
			finished(t, "done", nil)...), true, OriginTask, "jane assigned ENG-412"},
		{"a page woke it", append([]store.EventRecord{storyRow(t, at(0), opened(page, nil))},
			finished(t, "done", nil)...), true, OriginPage, "jane commented on Runbook"},
		{"a channel message woke it", append([]store.EventRecord{storyRow(t, at(0), opened(chat, nil))},
			finished(t, "done", withInteractions(channel))...), true, OriginChat, "Message from Ana: the numbers"},
		{"a direct message is the two people's own", append([]store.EventRecord{storyRow(t, at(0), opened(chat, nil))},
			finished(t, "done", withInteractions(dm))...), false, "", ""},
		{"a private channel is its members' own, even beside a public one",
			append([]store.EventRecord{storyRow(t, at(0), opened(chat, nil))},
				finished(t, "done", withInteractions(room))...), false, "", ""},
		{"a chat session still running cannot yet say it was in a channel",
			[]store.EventRecord{storyRow(t, at(0), opened(chat, nil))}, false, "", ""},
		{"a routine the company declares", append([]store.EventRecord{storyRow(t, at(0), opened(fire, nil))},
			finished(t, "done", nil)...), true, OriginSchedule, "Daily standup"},
		{"an agent asking an agent outside any work is a 1:1",
			append([]store.EventRecord{storyRow(t, at(0), opened(ask, nil))},
				finished(t, "done", nil)...), false, "", ""},
		{"help asked on a work item is work on it — told without what was asked",
			append([]store.EventRecord{storyRow(t, at(0), opened(ask, eng412))},
				finished(t, "done", nil)...), true, OriginColleague, ""},
		{"a direct message that wrote one task is charged to it — told without the message",
			append([]store.EventRecord{storyRow(t, at(0), opened(chat, nil))},
				finished(t, "done", func(end *types.AgentTurnCompleted, learned *types.TurnCompleted) {
					end.WorkItem, end.WorkItemBasis = eng412, types.BasisSoleWrite
					learned.Interactions = dm
				})...), true, OriginPrivate, ""},
		{"an alert is the machine's monitoring", append([]store.EventRecord{storyRow(t, at(0), opened(alert, nil))},
			finished(t, "done", nil)...), false, "", ""},
		{"a session that decided nobody was asking it did nothing to tell",
			append([]store.EventRecord{storyRow(t, at(0), opened(taskWake, eng412))},
				finished(t, "skipped", nil)...), false, "", ""},
	}
	for _, c := range cases {
		story, told := tell(t, storyInput{Rows: c.rows})
		if told != c.told {
			t.Errorf("%s: told = %v, want %v", c.why, told, c.told)
			continue
		}
		if !told {
			continue
		}
		if story.Why.Origin != c.origin || story.Why.Summary != c.summary {
			t.Errorf("%s: why = %+v, want origin %q summary %q", c.why, story.Why, c.origin, c.summary)
		}
		if !story.Why.Origin.Valid() {
			t.Errorf("%s: origin %q is not one this build names", c.why, story.Why.Origin)
		}
	}
}

// A STORY ENDS THE WAY ITS NEWEST SEGMENT DID: a parked session is not over,
// one whose coding job asked a question waits on a person until an answer
// reaches it, and one whose job was lost after it parked did not finish.
func TestAStoryEndsTheWayItsNewestSegmentDid(t *testing.T) {
	t.Parallel()
	start := storyRow(t, at(0), opened(taskWake, eng412))
	parked := storyRow(t, at(5), types.AgentTurnCompleted{TurnID: "run-1", Suspended: true})
	asked := storyRow(t, at(6), types.SandboxClarificationRequested{TurnID: "run-1", Question: "THE QUESTION"})
	answered := storyRow(t, at(7), types.SandboxRunAnswered{TurnID: "run-1", Outcome: types.AnswerResumed})
	declined := storyRow(t, at(7), types.SandboxRunAnswered{TurnID: "run-1", Outcome: types.AnswerDeclined})
	resumed := storyRow(t, at(8), types.AgentTurnStarted{TurnID: "run-1", Resumed: true,
		Trigger: types.Trigger{Type: "sandbox_run_completed"}})
	lost := storyRow(t, at(9), types.SandboxRunFailed{TurnID: "run-1"})
	cases := []struct {
		why  string
		rows []store.EventRecord
		want StoryOutcome
	}{
		{"no segment has ended", []store.EventRecord{start}, OutcomeRunning},
		{"parked on a coding job", []store.EventRecord{start, parked}, OutcomeRunning},
		{"its job asked people a question", []store.EventRecord{start, parked, asked}, OutcomeWaitingOnYou},
		{"an answer reached the job", []store.EventRecord{start, parked, asked, answered}, OutcomeRunning},
		{"an answer the job let go of leaves it waiting", []store.EventRecord{start, parked, asked, declined},
			OutcomeWaitingOnYou},
		{"resumed after the park", []store.EventRecord{start, parked, resumed}, OutcomeRunning},
		// THE RESUMED SEGMENT IS THE EVIDENCE: the answer's own row can sit
		// on a node that did not answer this read, and a session working
		// again is not waiting on anybody, whatever the rows before it said.
		{"resumed with the answer's row unread", []store.EventRecord{start, parked, asked, resumed},
			OutcomeRunning},
		{"its job was lost while it waited", []store.EventRecord{start, parked, lost}, OutcomeFailed},
		{"its check passed the work", append([]store.EventRecord{start}, finished(t, "done", nil)...),
			OutcomeApproved},
		{"it ended with its check still asking for another pass",
			append([]store.EventRecord{start}, finished(t, "self_iterate", nil)...), OutcomeSentBack},
		{"it broke", append([]store.EventRecord{start}, finished(t, "failed",
			func(end *types.AgentTurnCompleted, _ *types.TurnCompleted) { end.Failed = true })...),
			OutcomeFailed},
		{"a person stopped it", append([]store.EventRecord{start}, finished(t, "done",
			func(end *types.AgentTurnCompleted, _ *types.TurnCompleted) { end.Stopped = true })...),
			OutcomeFailed},
		{"a parked session that then finished", append([]store.EventRecord{start, parked, resumed},
			finished(t, "done", nil)...), OutcomeApproved},
	}
	for _, c := range cases {
		story, told := tell(t, storyInput{Rows: c.rows})
		if !told {
			t.Errorf("%s: a session on a task was not told", c.why)
			continue
		}
		if story.Outcome != c.want {
			t.Errorf("%s: outcome = %q, want %q", c.why, story.Outcome, c.want)
		}
		if !story.Outcome.Valid() {
			t.Errorf("%s: outcome %q is not one this build names", c.why, story.Outcome)
		}
	}
}

// YOUR NOTE IS THE CALLER'S OWN, under either of their names, and the newest
// of them: somebody else's note to the same session is theirs to read.
func TestYourNoteIsTheCallersOwn(t *testing.T) {
	t.Parallel()
	note := func(n int, by, bySeat, text string, outcome types.SteerOutcome) store.EventRecord {
		return storyRow(t, at(n), types.AgentTurnSteered{TurnID: "run-1", Note: text,
			SteeredBy: by, SteeredBySeat: bySeat, Outcome: outcome, SentAt: at(n - 1)})
	}
	rows := []store.EventRecord{
		storyRow(t, at(0), opened(taskWake, eng412)),
		note(2, "ops-1", "jane", "keep the route", types.SteerDelivered),
		note(3, "ops-9", "cto", "SOMEBODY ELSE'S NOTE", types.SteerDelivered),
		note(4, "jane", "", "and the signature", types.SteerExpired),
	}
	for _, c := range []struct {
		why    string
		viewer storyViewer
		want   string
		status types.SteerOutcome
	}{
		{"the person's newest, sent as their seat", storyViewer{Handle: "jane", OperatorID: "ops-1"},
			"and the signature", types.SteerExpired},
		{"through the key bound to them", storyViewer{OperatorID: "ops-1"},
			"keep the route", types.SteerDelivered},
	} {
		story, _ := tell(t, storyInput{Rows: rows, Viewer: c.viewer})
		if story.YourNote == nil || story.YourNote.Note != c.want || story.YourNote.Status != c.status {
			t.Errorf("%s: your note = %+v, want %q %s", c.why, story.YourNote, c.want, c.status)
		}
	}
	if story, _ := tell(t, storyInput{Rows: rows, Viewer: storyViewer{Handle: "bo"}}); story.YourNote != nil {
		t.Errorf("a person who sent no note is shown %+v", story.YourNote)
	}
	if story, _ := tell(t, storyInput{Rows: rows}); story.YourNote != nil {
		t.Errorf("a caller with no seat and no key is shown %+v", story.YourNote)
	}
}

// A STORY NAMES ITS WORK AS THE TRACKER KNOWS IT: the item a completion charged
// it to — a sole write the dispatch could not know — with the task's title and
// its number among the task's sessions; and a session still running, not yet
// counted, as the task's next.
func TestAStoryNamesItsWorkAndItsPlace(t *testing.T) {
	t.Parallel()
	start := storyRow(t, at(0), opened(taskWake, nil))
	soleWrite := finished(t, "done", func(end *types.AgentTurnCompleted, _ *types.TurnCompleted) {
		end.WorkItem, end.WorkItemBasis = eng412, types.BasisSoleWrite
	})
	place := &tracker.TurnPlace{TaskID: "t-412", Key: "ENG-412", Title: "Fix billing webhook", Ordinal: 3}
	story, _ := tell(t, storyInput{Rows: append([]store.EventRecord{start}, soleWrite...), Place: place})
	if item := story.WorkItem; item == nil || item.ID != "t-412" || item.Title != "Fix billing webhook" ||
		item.Ordinal != 3 {
		t.Errorf("work item = %+v, want ENG-412, titled, session 3", story.WorkItem)
	}
	other := &tracker.TurnPlace{TaskID: "t-999", Key: "OPS-9", Title: "elsewhere", Ordinal: 7}
	if story, _ := tell(t, storyInput{Rows: append([]store.EventRecord{start}, soleWrite...),
		Place: other}); story.WorkItem.Title != "" || story.WorkItem.Ordinal != 0 {
		t.Errorf("a place on another task named this one: %+v", story.WorkItem)
	}
	running := storyRow(t, at(0), opened(taskWake, eng412))
	if story, _ := tell(t, storyInput{Rows: []store.EventRecord{running}, NextOrdinal: 4}); story.WorkItem.Ordinal != 4 {
		t.Errorf("a running session is session %d, want the task's next, 4", story.WorkItem.Ordinal)
	}
	if story, _ := tell(t, storyInput{Rows: append([]store.EventRecord{running},
		finished(t, "done", nil)...), NextOrdinal: 4}); story.WorkItem.Ordinal != 0 {
		t.Errorf("a finished session with no place took the running one's number: %+v", story.WorkItem)
	}
	if story.StartedAt != at(0) || story.DurationMS != 60_000 {
		t.Errorf("started %v for %dms, want the opening's instant and the segment's measure",
			story.StartedAt, story.DurationMS)
	}
}

// WHAT IT DID, WHOM IT ASKED, HOW MANY HELPED AND WHAT IT LEFT BEHIND are each
// read once, in order: a page opened whole is a read and a search's list is
// not; a colleague it asked is named once however many times; the channel of
// an ask it ANSWERED names nobody it consulted; and two comments on one task
// are one thing it left behind.
func TestAStoryTellsWhatItDidAndLeftBehind(t *testing.T) {
	t.Parallel()
	rows := []store.EventRecord{
		storyRow(t, at(0), opened(taskWake, eng412)),
		storyRow(t, at(1), types.KnowledgeRead{TurnID: "run-1", Via: types.ReadViaPrefetch,
			Pages: []types.KnowledgeReadPage{{ID: "p-pre"}}}),
		storyRow(t, at(2), types.KnowledgeRead{TurnID: "run-1", Via: types.ReadViaGetPage,
			Backend: "native", Pages: []types.KnowledgeReadPage{{ID: "p-1", Title: "Runbook"}}}),
		storyRow(t, at(3), types.A2AChannelOpened{TurnID: "run-1", Requester: "swe", Target: "cto"}),
		storyRow(t, at(4), types.A2AChannelOpened{TurnID: "run-1", Requester: "swe", Target: "cto"}),
		storyRow(t, at(4), types.A2AChannelOpened{TurnID: "run-1", Requester: "pm", Target: "swe"}),
		storyRow(t, at(5), types.SubagentBatched{TurnID: "run-1", TaskCount: 2}),
		storyRow(t, at(6), types.SubagentBatched{TurnID: "run-1", TaskCount: 1}),
		storyRow(t, at(7), types.SandboxRunStarted{TurnID: "run-1"}),
	}
	rows = append(rows, finished(t, "done", nil)...)
	story, _ := tell(t, storyInput{Rows: rows,
		Tasks: []tracker.TurnWrite{
			{TaskID: "t-412", Key: "ENG-412", Kind: tracker.ChangeComment, At: at(3)},
			{TaskID: "t-412", Key: "ENG-412", Kind: tracker.ChangeStatus, Status: "in_progress", At: at(4)},
			{TaskID: "t-412", Key: "ENG-412", Kind: tracker.ChangeComment, At: at(6)},
		},
		Pages: []pages.TurnWrite{{PageID: "p-1", Title: "Runbook", Kind: pages.ChangeSaved, At: at(5)}},
	})
	if len(story.Did) != 2 || story.Did[0].Kind != ActionReadPage || story.Did[0].Pages[0].ID != "p-1" ||
		story.Did[1].Kind != ActionCodingStarted {
		t.Errorf("did = %+v, want the page it opened and the coding job, in order", story.Did)
	}
	if !slices.Equal(story.Consulted, []string{"cto"}) {
		t.Errorf("consulted = %v, want cto once — and not the colleague whose ask it answered",
			story.Consulted)
	}
	if story.Helpers != 3 {
		t.Errorf("helpers = %d, want both batches' three", story.Helpers)
	}
	var left []string
	for _, link := range story.LeftBehind {
		left = append(left, string(link.Kind)+":"+link.ID+":"+link.Change+":"+link.Status)
	}
	want := []string{"task:t-412:comment:", "task:t-412:status:in_progress", "page:p-1:saved:"}
	if !slices.Equal(left, want) {
		t.Errorf("left behind = %v, want %v — in the order they landed, each once", left, want)
	}
	for _, a := range story.Did {
		if !a.Kind.Valid() {
			t.Errorf("action %q is not one this build names", a.Kind)
		}
	}
	for _, l := range story.LeftBehind {
		if !l.Kind.Valid() {
			t.Errorf("link kind %q is not one this build names", l.Kind)
		}
	}
}
