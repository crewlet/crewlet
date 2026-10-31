package assistant_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/assistant"
	"github.com/crewlet/crewlet/internal/assistant/assistanttest"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/evals"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/wire"
)

// itemPage is the screen of ENG-7, with ENG-7 open.
func itemPage() *wire.PageContext {
	return &wire.PageContext{Screen: "item", Title: "Tasks › ENG-7", Params: map[string]string{
		"id": "ENG-7"}, Query: map[string]string{}, Resources: []wire.Resource{},
		Selection:      &wire.ResourceRef{Kind: wire.ResourceTask, ID: "ENG-7", Noun: wire.NounTask},
		SourceRevision: "item@1", Accepts: []string{}}
}

// briefOf is an answer_brief call citing ENG-7 in a row of tone.
func briefOf(id string, tone wire.BriefTone) evals.Round {
	return evals.Call(id, catalog.AnswerBriefTool, map[string]any{
		"about": "ENG-7, Rotate keys, waits on your review.",
		"needs": []any{map[string]any{"ref_kind": "task", "ref_id": "ENG-7",
			"do": "Review ENG-7: Rotate keys", "tone": string(tone), "action": "open"}},
		"follow_ups": []any{"Who is on ENG-7?"},
	})
}

// A BRIEF IS ASKED OF A PAGE: one with no page, or a mode the engine does not
// know, is refused before anything is recorded or spent.
//
// Mutation: drop the mode check and a brief with no page runs an ordinary turn.
func TestABriefNeedsThePageItBriefs(t *testing.T) {
	t.Parallel()
	f := assistanttest.NewRig(t, evals.RigOptions{Tools: []catalog.Tool{getTask(t)}, Handler: task})
	if _, err := f.Brief("no-page", "Brief me on this page", nil); !errors.Is(err,
		assistant.ErrInvalid) {
		t.Errorf("a brief with no page answered %v, want ErrInvalid", err)
	}
	_, err := f.Svc.Start(t.Context(), assistant.TurnStart{Person: f.Person,
		Conversation: f.Conversation, Key: evals.Key("bad-mode"), Input: "Brief me",
		Page: itemPage(), Read: f.Grant("bad-mode"), Mode: "summary"})
	if !errors.Is(err, assistant.ErrInvalid) {
		t.Errorf("an unknown mode answered %v, want ErrInvalid", err)
	}
	if sent := f.Dispatcher.Sent(); len(sent) != 0 {
		t.Errorf("a refused brief sent %+v", sent)
	}
}

// A BRIEF'S ANSWER IS ONE BRIEF CARD: its reads draw no card, its prose is
// never drawn, its row opens the engine's own address for the record it read,
// and a danger the record's read does not carry is drawn warning.
//
// Mutation: emit read cards in brief mode, or trust the model's danger, and
// this fails.
func TestABriefAnswersWithOneBriefCard(t *testing.T) {
	t.Parallel()
	f := assistanttest.NewRig(t, evals.RigOptions{Tools: []catalog.Tool{getTask(t)},
		Handler: task, Rounds: []evals.Round{readTask(), evals.Say("Let me write it."),
			briefOf("c2", wire.BriefDanger)}})
	accepted, err := f.Brief("brief", "Brief me on this task", itemPage())
	if err != nil {
		t.Fatal(err)
	}
	frames := f.Follow(accepted.TurnID, 0)
	var shown []wire.Card
	for _, fr := range frames {
		switch fr.Kind {
		case wire.EventCard:
			shown = append(shown, fr.Card.Card)
		case wire.EventDelta:
			if fr.Delta.Text != "" {
				t.Errorf("a brief drew prose %q", fr.Delta.Text)
			}
		}
	}
	if len(shown) != 1 || shown[0].Type != wire.CardBrief {
		t.Fatalf("cards = %+v, want one brief card", shown)
	}
	b := shown[0].Brief
	if b.Scope.Record == nil || b.Scope.Record.ID != "ENG-7" {
		t.Errorf("scope = %+v, want the open ENG-7", b.Scope)
	}
	if len(b.Needs) != 1 {
		t.Fatalf("needs = %+v, want the one row", b.Needs)
	}
	need := b.Needs[0]
	if need.Tone != wire.BriefWarning {
		t.Errorf("tone = %s, want warning: the task's read carries no urgency", need.Tone)
	}
	if need.Action == nil || need.Action.Link == nil || need.Action.Link.Href != "#/worklet/ENG-7" {
		t.Errorf("action = %+v, want the link table's address of ENG-7", need.Action)
	}
	if done := evals.Last(frames, wire.EventDone); done == nil || done.Done.State != wire.TurnCompleted {
		t.Errorf("done = %+v, want completed", done)
	}
}

// A BRIEF THE ENGINE CANNOT ACCEPT IS WRITTEN AGAIN ONCE, and a second refusal
// ends the turn brief_invalid, with nothing drawn.
//
// Mutation: allow more answers, or end on the first, and this fails.
func TestABriefIsWrittenAgainOnceThenFails(t *testing.T) {
	t.Parallel()
	bad := func(id string) evals.Round {
		return evals.Call(id, catalog.AnswerBriefTool, map[string]any{"about": "",
			"needs": []any{}})
	}
	f := assistanttest.NewRig(t, evals.RigOptions{Tools: []catalog.Tool{getTask(t)},
		Handler: task, Rounds: []evals.Round{bad("c1"), bad("c2"), evals.Say("Sorry.")}})
	accepted, err := f.Brief("bad", "Brief me on this task", itemPage())
	if err != nil {
		t.Fatal(err)
	}
	frames := f.Follow(accepted.TurnID, 0)
	if card := evals.Last(frames, wire.EventCard); card != nil {
		t.Errorf("a refused brief drew %+v", card.Card.Card)
	}
	failure := evals.Last(frames, wire.EventError)
	if failure == nil || failure.Error.Code != wire.ErrorBriefInvalid {
		t.Errorf("error = %+v, want brief_invalid", failure)
	}
}

// A ROW ABOUT A RECORD THE TURN NEVER READ IS DROPPED, never drawn, and the
// brief still stands without it.
//
// Mutation: skip the citation check and ENG-9 is drawn.
func TestABriefRowAboutAnUnreadRecordIsDropped(t *testing.T) {
	t.Parallel()
	unread := evals.Call("c1", catalog.AnswerBriefTool, map[string]any{
		"about": "ENG-7, Rotate keys, is in review.",
		"needs": []any{map[string]any{"ref_kind": "task", "ref_id": "ENG-9",
			"do": "Chase ENG-9: it is overdue", "tone": "warning", "action": "open"}},
	})
	handler := assistanttest.Routes(map[string]exec.Response{
		"GET /worklet/ENG-9": assistanttest.Task("ENG-9", "Ghost", "Never read."),
	})
	f := assistanttest.NewRig(t, evals.RigOptions{Tools: []catalog.Tool{getTask(t)},
		Handler: handler, Rounds: []evals.Round{unread}})
	accepted, err := f.Brief("unread", "Brief me on this page", itemPage())
	if err != nil {
		t.Fatal(err)
	}
	card := evals.Last(f.Follow(accepted.TurnID, 0), wire.EventCard)
	if card == nil || card.Card.Card.Brief == nil {
		t.Fatal("no brief card was drawn")
	}
	if needs := card.Card.Card.Brief.Needs; len(needs) != 0 {
		t.Errorf("needs = %+v, want the unread ENG-9 dropped", needs)
	}
	if sent := f.Dispatcher.Sent(); len(sent) != 0 {
		t.Errorf("an uncited record was read: %+v", sent)
	}
}

// overLong is an answer_brief call with two faults the model could not see
// one at a time: an about line of 366 characters (the length of the live
// case's summary) and a row's do of 130.
func overLong(id string) evals.Round {
	return evals.Call(id, catalog.AnswerBriefTool, map[string]any{
		"about": strings.Repeat("ENG-7 waits. ", 29)[:366],
		"needs": []any{map[string]any{"ref_kind": "task", "ref_id": "ENG-7",
			"do": strings.Repeat("r", 130), "tone": "warning", "action": "open"}},
	})
}

// reads is n rounds each reading ENG-7, called r1 to rn.
func reads(n int) []evals.Round {
	out := make([]evals.Round, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, evals.Call(fmt.Sprintf("r%d", i), "get_task",
			map[string]any{"item": "ENG-7"}))
	}
	return out
}

// logLines is a logger whose JSON lines a suite reads back.
type logLines struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logLines) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

// named is every logged line with message msg, decoded.
func (l *logLines) named(t *testing.T, msg string) []map[string]any {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(l.buf.String()), "\n") {
		var rec map[string]any
		if line == "" {
			continue
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("a log line is not JSON: %q", line)
		}
		if rec["msg"] == msg {
			out = append(out, rec)
		}
	}
	return out
}

// endLogWait bounds how long a suite waits for a line a turn writes AFTER its
// stream has ended. A turn tells the stream it is done first, so a person is
// answered at once, and only then publishes its audit record and logs its end
// (`assistant_turn_ended`), on the same goroutine, within milliseconds. A
// suite that read the log the moment the stream ended raced that write and
// failed about one run in two under the race detector. Ten seconds is a turn
// that never logged its end, not a slow runner.
const endLogWait = 10 * time.Second

// awaited is [logLines.named] once at least want lines with message msg have
// been logged, or what has been logged when [endLogWait] runs out.
func (l *logLines) awaited(t *testing.T, msg string, want int) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(endLogWait)
	for {
		got := l.named(t, msg)
		if len(got) >= want || time.Now().After(deadline) {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// captured replaces the rig's logger with one the suite reads.
func captured(f *assistanttest.Rig) *logLines {
	l := &logLines{}
	f.Svc.SetLogger(slog.New(slog.NewJSONHandler(l, &slog.HandlerOptions{Level: slog.LevelDebug})))
	return l
}

// A REFUSED BRIEF NAMES EVERY FAULT AT ONCE, so its one retry fixes them all
// and lands: the live failure was a refusal naming the first line alone, a
// retry that fixed it, and a second refusal for the row it had not named.
// The engine's own log says why the brief was refused, and never its words.
//
// Mutation: stop at the first fault, or drop the refusal's log line, and
// this fails.
func TestABriefRefusalNamesEveryFaultAndItsRetryLands(t *testing.T) {
	t.Parallel()
	f := assistanttest.NewRig(t, evals.RigOptions{Tools: []catalog.Tool{getTask(t)},
		Handler: task, Rounds: []evals.Round{readTask(), overLong("a1"),
			briefOf("a2", wire.BriefWarning)}})
	logs := captured(f)
	accepted, err := f.Brief("retry", "Brief me on this task", itemPage())
	if err != nil {
		t.Fatal(err)
	}
	frames := f.Follow(accepted.TurnID, 0)
	if done := evals.Last(frames, wire.EventDone); done == nil || done.Done.State != wire.TurnCompleted {
		t.Fatalf("done = %+v, want completed on the retry", done)
	}
	if card := evals.Last(frames, wire.EventCard); card == nil || card.Card.Card.Brief == nil {
		t.Fatal("the retry drew no brief card")
	}
	refusal, ok := f.Model.Answer("a1")
	if !ok || !refusal.Failed {
		t.Fatalf("the first answer was answered %+v, want refused", refusal)
	}
	for _, want := range []string{
		fmt.Sprintf("about is 366 characters and the most it takes is %d", wire.BriefAboutMaxRunes),
		fmt.Sprintf("needs[0].do is 130 characters and the most it takes is %d",
			wire.BriefTextMaxRunes), "last attempt"} {
		if !strings.Contains(refusal.Output, want) {
			t.Errorf("the refusal does not say %q: %s", want, refusal.Output)
		}
	}
	lines := logs.named(t, "assistant_brief_refused")
	if len(lines) != 1 {
		t.Fatalf("logged %d refusals, want 1", len(lines))
	}
	line := lines[0]
	if line["turn"] != accepted.TurnID.String() || line["attempt"] != float64(1) ||
		line["final"] != false {
		t.Errorf("refusal logged as %v, want turn %s, attempt 1, not final", line, accepted.TurnID)
	}
	if reason, _ := line["reason"].(string); !strings.Contains(reason, "about is 366") ||
		strings.Contains(reason, "ENG-7 waits") {
		t.Errorf("reason = %q, want the fault and none of the brief's words", reason)
	}
}

// THE LAST ROUND THAT MAY READ LEAVES ROOM FOR THE ANSWER AND ITS RETRY: a
// brief that reads in every round it may is told so in that round's answer,
// and an answer refused in the round after it is written again, and drawn.
//
// Mutation: let reads run in every round of the turn, or drop the rounds kept
// for the answer, and the retry is cut off as a step limit.
func TestABriefsLastReadRoundLeavesRoomForItsRetry(t *testing.T) {
	t.Parallel()
	script := append(reads(assistant.BriefReadRounds), overLong("a1"),
		briefOf("a2", wire.BriefWarning))
	f := assistanttest.NewRig(t, evals.RigOptions{Tools: []catalog.Tool{getTask(t)},
		Handler: task, Rounds: script})
	accepted, err := f.Brief("last-read", "Brief me on this task", itemPage())
	if err != nil {
		t.Fatal(err)
	}
	frames := f.Follow(accepted.TurnID, 0)
	if done := evals.Last(frames, wire.EventDone); done == nil || done.Done.State != wire.TurnCompleted {
		t.Fatalf("done = %+v, want completed on the retry", done)
	}
	last := fmt.Sprintf("r%d", assistant.BriefReadRounds)
	before := fmt.Sprintf("r%d", assistant.BriefReadRounds-1)
	if res, _ := f.Model.Answer(last); !strings.Contains(res.Output, "last round of reads") {
		t.Errorf("the last read round was answered %q, want it to say the reads are spent", res.Output)
	}
	if res, _ := f.Model.Answer(before); strings.Contains(res.Output, "last round of reads") {
		t.Errorf("an earlier read round was told the reads are spent: %q", res.Output)
	}
	if res, ok := f.Model.Answer("a2"); !ok || res.Failed {
		t.Errorf("the retry was answered %+v, want it accepted", res)
	}
}

// A READ IN THE ROUNDS KEPT FOR THE ANSWER IS NOT SENT: the model is told to
// answer, and the answer that follows is drawn.
//
// Mutation: drop the read gate and the read runs in the answer's round.
func TestABriefReadInTheAnswersRoundsIsNotSent(t *testing.T) {
	t.Parallel()
	extra := evals.Call("late", "get_task", map[string]any{"item": "ENG-8"})
	script := append(reads(assistant.BriefReadRounds), extra, briefOf("a1", wire.BriefWarning))
	f := assistanttest.NewRig(t, evals.RigOptions{Tools: []catalog.Tool{getTask(t)},
		Handler: task, Rounds: script})
	accepted, err := f.Brief("late-read", "Brief me on this task", itemPage())
	if err != nil {
		t.Fatal(err)
	}
	frames := f.Follow(accepted.TurnID, 0)
	if done := evals.Last(frames, wire.EventDone); done == nil || done.Done.State != wire.TurnCompleted {
		t.Fatalf("done = %+v, want completed", done)
	}
	if res, _ := f.Model.Answer("late"); !res.Failed || !strings.Contains(res.Output, "Call answer_brief now") {
		t.Errorf("a read in the answer's rounds was answered %+v, want it refused unsent", res)
	}
	for _, s := range f.Dispatcher.Sent() {
		if s.Request.Path == "/worklet/ENG-8" {
			t.Errorf("a read in the answer's rounds was sent: %+v", s.Request)
		}
	}
}

// AN ANSWER REFUSED IN THE TURN'S LAST ROUND IS FINAL: no round is left for
// a retry, so the turn ends brief_invalid with the reason, in the refusal's
// log line and the turn's end, rather than as a step limit.
//
// Mutation: keep offering a retry in the last round and the turn ends
// step_limit with no reason logged.
func TestABriefRefusedInTheTurnsLastRoundIsFinal(t *testing.T) {
	t.Parallel()
	extra := evals.Call("late", "get_task", map[string]any{"item": "ENG-7"})
	script := append(reads(assistant.BriefReadRounds), extra, overLong("a1"),
		briefOf("a2", wire.BriefWarning))
	f := assistanttest.NewRig(t, evals.RigOptions{Tools: []catalog.Tool{getTask(t)},
		Handler: task, Rounds: script})
	logs := captured(f)
	accepted, err := f.Brief("last-round", "Brief me on this task", itemPage())
	if err != nil {
		t.Fatal(err)
	}
	frames := f.Follow(accepted.TurnID, 0)
	if failure := evals.Last(frames, wire.EventError); failure == nil ||
		failure.Error.Code != wire.ErrorBriefInvalid {
		t.Fatalf("error = %+v, want brief_invalid", failure)
	}
	refused := logs.named(t, "assistant_brief_refused")
	if len(refused) != 1 || refused[0]["final"] != true {
		t.Errorf("refusals logged %v, want one, final", refused)
	}
	// THE END IS LOGGED AFTER THE STREAM HEARS IT ([endLogWait]).
	ended := logs.awaited(t, "assistant_turn_ended", 1)
	if len(ended) != 1 || ended[0]["error_code"] != string(wire.ErrorBriefInvalid) {
		t.Fatalf("turn ends logged %v, want one brief_invalid", ended)
	}
	if reason, _ := ended[0]["reason"].(string); !strings.Contains(reason, "about is 366") {
		t.Errorf("the turn's end gives reason %q, want the last refusal's", reason)
	}
}

// A BRIEF ROW MAY CITE A CHAT ROOM, the record a question asked of the person
// in chat waits in: by its id, or by its name with its "#", which the engine
// resolves to the id through the room's own read, and each row opens the
// room's screen. A room the person may not read is dropped as any record
// they may not open is. Before a room was a citable kind, a brief on
// Chatlet's mentions could cite neither room, and said nothing needed the
// person while two asks waited on them.
//
// Mutation: drop the room from the citable kinds, or resolve no room name,
// and a row is refused or dropped.
func TestABriefRowMayCiteAChatRoom(t *testing.T) {
	t.Parallel()
	const launch, release, secret = "0b9d2a52-6d1c-4f0e-9a51-2a7f3c1d9e31",
		"0b9d2a52-6d1c-4f0e-9a51-2a7f3c1d9e32", "0b9d2a52-6d1c-4f0e-9a51-2a7f3c1d9e33"
	room := func(id, name string) exec.Response {
		body, _ := json.Marshal(map[string]any{"channel": map[string]any{"id": id,
			"kind": "public", "name": name}})
		return exec.Response{Status: http.StatusOK, Body: body}
	}
	mentions, _ := json.Marshal(map[string]any{"items": []any{
		map[string]any{"key": "mention:m1", "kind": "mention", "channel_id": launch,
			"channel_name": "launch", "channel_kind": "public", "actor": "ada",
			"excerpt": "@grace can you approve the launch copy?", "message_id": "m1",
			"at": "2026-10-26T09:00:00Z"},
		map[string]any{"key": "mention:m2", "kind": "mention", "channel_id": release,
			"channel_name": "release", "channel_kind": "public", "actor": "ada",
			"excerpt": "@grace ship Friday or hold?", "message_id": "m2",
			"at": "2026-10-26T09:05:00Z"},
		map[string]any{"key": "mention:m3", "kind": "mention", "channel_id": secret,
			"channel_name": "secret", "channel_kind": "private", "actor": "ada",
			"excerpt": "@grace a word?", "message_id": "m3", "at": "2026-10-26T09:10:00Z"}}})
	handler := assistanttest.Routes(map[string]exec.Response{
		"GET /chatlet/activity":            {Status: http.StatusOK, Body: mentions},
		"GET /chatlet/channels/" + launch:  room(launch, "launch"),
		"GET /chatlet/channels/%23release": room(release, "release"),
		"GET /chatlet/channels/" + release: room(release, "release"),
	})
	need := func(id, do string) map[string]any {
		return map[string]any{"ref_kind": "room", "ref_id": id, "do": do, "tone": "warning",
			"action": "open"}
	}
	f := assistanttest.NewRig(t, evals.RigOptions{
		Tools:   []catalog.Tool{assistanttest.Tool(t, "list_chat_mentions")},
		Handler: handler, Rounds: []evals.Round{
			evals.Call("c1", "list_chat_mentions", map[string]any{}),
			evals.Call("c2", catalog.AnswerBriefTool, map[string]any{
				"about": "Two asks wait on you in chat.",
				"needs": []any{need(launch, "Answer Ada in #launch: approve the launch copy?"),
					need("#release", "Answer Ada in #release: ship Friday or hold?"),
					need(secret, "Answer Ada in #secret")}})}})
	mentionsPage := &wire.PageContext{Screen: "chatlet-activity", Title: "Chatlet › Activity",
		Params: map[string]string{}, Query: map[string]string{}, Resources: []wire.Resource{},
		SourceRevision: "mentions@1", Accepts: []string{}}
	accepted, err := f.Brief("rooms", "Brief me on this page", mentionsPage)
	if err != nil {
		t.Fatal(err)
	}
	card := evals.Last(f.Follow(accepted.TurnID, 0), wire.EventCard)
	if card == nil || card.Card.Card.Brief == nil {
		t.Fatal("no brief card was drawn")
	}
	brief := card.Card.Card.Brief
	var opened []string
	for _, n := range brief.Needs {
		if n.Ref.Kind != wire.ResourceRoom || n.Action == nil || n.Action.Link == nil ||
			n.Action.Label != "Open the room" {
			t.Errorf("a room's row is %+v", n)
			continue
		}
		opened = append(opened, n.Ref.ID+" "+n.Action.Link.Href)
	}
	want := []string{launch + " #/chatlet/" + launch, release + " #/chatlet/" + release}
	if fmt.Sprint(opened) != fmt.Sprint(want) {
		t.Errorf("the rows open %q, want %q: the room by its id, the room by its name, and "+
			"not the room the person may not read", opened, want)
	}
	if got := brief.Verdict(); got != "2 things need you" {
		t.Errorf("the brief says %q", got)
	}
}

// A ROOM'S BRIEF IS ITS CATCH UP: points of what was said, each kept only for
// a message this turn read and the person may read now, with the source the
// engine read back (the room, the message and its thread). A point citing a
// message no read named, or one the person may not read, is dropped.
//
// Mutation: drop the citation check, or the read back, and a point survives
// that the person was never shown or may not open.
func TestARoomsBriefSummarisesWithPointsCitingMessages(t *testing.T) {
	t.Parallel()
	const room = "0b9d2a52-6d1c-4f0e-9a51-2a7f3c1d9e41"
	transcript, _ := json.Marshal(map[string]any{"channel_id": room, "messages": []any{
		map[string]any{"message": map[string]any{"id": "m-reply", "channel_id": room,
			"thread_root": "m-root", "author": "ada", "body": "GP-3 passed 13 of 14"}},
		map[string]any{"message": map[string]any{"id": "m-root", "channel_id": room,
			"author": "sam", "body": "Should 2.4.0 wait for GP-12?"}},
		map[string]any{"message": map[string]any{"id": "m-gone", "channel_id": room,
			"author": "sam", "body": "taken back since"}}}})
	one := func(id, root string) exec.Response {
		body, _ := json.Marshal(map[string]any{"channel_kind": "public", "view": map[string]any{
			"message": map[string]any{"id": id, "channel_id": room, "thread_root": root}}})
		return exec.Response{Status: http.StatusOK, Body: body}
	}
	handler := assistanttest.Routes(map[string]exec.Response{
		"GET /chatlet/channels/" + room + "/messages":         {Status: http.StatusOK, Body: transcript},
		"GET /chatlet/channels/" + room + "/messages/m-root":  one("m-root", ""),
		"GET /chatlet/channels/" + room + "/messages/m-reply": one("m-reply", "m-root"),
		"GET /chatlet/channels/" + room + "/messages/m-gone":  {Status: http.StatusNotFound},
	})
	point := func(say, id string) map[string]any {
		return map[string]any{"say": say, "message_id": id}
	}
	f := assistanttest.NewRig(t, evals.RigOptions{
		Tools:   []catalog.Tool{assistanttest.Tool(t, "read_chat_messages")},
		Handler: handler, Rounds: []evals.Round{
			evals.Call("c1", "read_chat_messages", map[string]any{"channel": room}),
			evals.Call("c2", catalog.AnswerBriefTool, map[string]any{
				"about": "The 2.4.0 release waits on GP-12.",
				"needs": []any{},
				"points": []any{
					point("Sam asks whether 2.4.0 waits for GP-12.", "m-root"),
					point("Ada reports GP-3 passed 13 of 14 scenarios.", "m-reply"),
					point("Sam said something since taken back.", "m-gone"),
					point("Somebody said something nobody read.", "m-never")}})}})
	roomPage := &wire.PageContext{Screen: "chatlet", Title: "Chatlet › #engineering",
		Params: map[string]string{"id": room}, Query: map[string]string{},
		Resources: []wire.Resource{}, SourceRevision: "room@1", Accepts: []string{},
		Selection: &wire.ResourceRef{Kind: wire.ResourceRoom, ID: room, Noun: wire.NounRoom}}
	accepted, err := f.Brief("catch up", "Catch me up", roomPage)
	if err != nil {
		t.Fatal(err)
	}
	card := evals.Last(f.Follow(accepted.TurnID, 0), wire.EventCard)
	if card == nil || card.Card.Card.Brief == nil {
		t.Fatal("no brief card was drawn")
	}
	var got []string
	for _, p := range card.Card.Card.Brief.Points {
		got = append(got, p.Source.MessageID+"/"+p.Source.ThreadRoot)
	}
	if want := []string{"m-root/", "m-reply/m-root"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("the points cite %v, want %v: each read back as the person, the reply "+
			"with its thread, and none the person was not shown or may not open", got, want)
	}
}
