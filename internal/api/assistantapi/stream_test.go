package assistantapi_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/crewlet/crewlet/internal/assistant/assistanttest"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/evals"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/wire"
)

// parkedSite is a turn that read once, then proposed a task and parked on it,
// its stream read to the end as the asking tab reads it.
func parkedSite(t *testing.T) (*site, wire.TurnAccepted, []wire.Frame) {
	t.Helper()
	s := newSite(t, evals.RigOptions{
		Tools: []catalog.Tool{assistanttest.Tool(t, "get_task"),
			assistanttest.Tool(t, "create_task")},
		Acts: []string{"create_work_item"},
		Handler: assistanttest.Routes(map[string]exec.Response{
			"GET /worklet/ENG-7": assistanttest.Task("ENG-7", "Rotate keys",
				"Rotate the signing keys before Friday."),
		}),
		Rounds: []evals.Round{
			evals.Call("c1", "get_task", map[string]any{"item": "ENG-7"}),
			evals.Call("c2", "create_task", map[string]any{"title": "Rotate keys"}),
			evals.Say("I prepared the task.")}})
	accepted := s.ask(t, "File a task to rotate the keys")
	drawn := frames(t, s.stream(t, accepted, ""))
	done := evals.Last(drawn, wire.EventDone)
	if done == nil || done.Done.State != wire.TurnWaitingForApproval {
		t.Fatalf("the turn ended %+v, want parked on its proposal", done)
	}
	return s, accepted, drawn
}

// transcriptOf reads the conversation as a reloaded tab does.
func transcriptOf(t *testing.T, s *site) wire.Transcript {
	t.Helper()
	rec := s.do(s.rig.Person, http.MethodGet, "/assistant/conversations/"+
		s.rig.Conversation.String(), nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("transcript = %d %s", rec.Code, rec.Body)
	}
	var out wire.Transcript
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// A PARKED TURN REOPENED FROM HISTORY IS NOT A DROPPED STREAM: the transcript
// names the resume point as the segment's last frame (its `done`), and the
// stream resumed there answers 204 with no body, the event-stream answer a
// client does not dial again after. An empty 200 there read as a drop to the
// drawer, which redialled it every second until the log expired, with
// "Reconnecting" and "Offline" on screen the whole time.
//
// Mutation: drop the caught-up answer in Service.follow and the resume answers
// an empty 200, which this refuses.
func TestAParkedTurnResumedAtItsEndAnswersNoContent(t *testing.T) {
	t.Parallel()
	s, accepted, drawn := parkedSite(t)
	open := transcriptOf(t, s).OpenTurn
	if open == nil || open.TurnID != accepted.TurnID || open.State != wire.TurnWaitingForApproval {
		t.Fatalf("the transcript's open turn = %+v, want the parked one", open)
	}
	if last := drawn[len(drawn)-1].Seq; open.AfterSeq != last {
		t.Fatalf("the transcript resumes after %d, want the segment's last frame %d",
			open.AfterSeq, last)
	}
	rec := s.do(s.rig.Person, http.MethodGet, open.StreamURL, nil,
		map[string]string{"Last-Event-ID": wire.EventID(open.TurnID, open.AfterSeq)})
	if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("the resume at the segment's end = %d %q, want 204 with no body",
			rec.Code, rec.Body)
	}
	// A resume from inside the segment still replays what follows, to its end.
	rest := frames(t, s.stream(t, accepted, wire.EventID(accepted.TurnID, open.AfterSeq-1)))
	if len(rest) != 1 || rest[0].Done == nil {
		t.Fatalf("a resume one frame short = %+v, want the done frame alone", rest)
	}
}
