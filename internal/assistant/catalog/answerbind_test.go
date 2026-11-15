package catalog_test

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/proposal"
)

// taskID is the task the questions here are asked on.
const taskID = "7d0c5c8e-1a2b-4c3d-8e9f-0a1b2c3d4e5f"

// threadOf is a reader over one task's thread in pages, newest page first:
// `GET /worklet/comments` answers the page its cursor names (the first with
// none), narrowed to one comment's conversation where it names a `thread`, as
// the route narrows it, and `GET /worklet/decisions` names the person's own
// record, ana.
func threadOf(pages ...[]any) proposal.Read {
	return func(_ context.Context, r exec.Request) (exec.Response, error) {
		var body any
		switch r.Path {
		case "/worklet/decisions":
			body = map[string]any{"handle": "ana", "items": []any{}, "more": false}
		case "/worklet/comments":
			at := 0
			if c := r.Query.Get("cursor"); c != "" {
				at = int(c[0] - '0')
			}
			comments := pages[at]
			if root := r.Query.Get("thread"); root != "" {
				var narrowed []any
				for _, c := range comments {
					m := c.(map[string]any)
					if m["id"] == root || m["answers"] == root || m["reply_to"] == root {
						narrowed = append(narrowed, c)
					}
				}
				comments = narrowed
			}
			page := map[string]any{"item": taskID, "key": "ENG-41", "title": "Route the docs",
				"comments": comments}
			if at+1 < len(pages) {
				page["next_cursor"] = string(rune('0' + at + 1))
			}
			body = page
		default:
			return exec.Response{Status: http.StatusNotFound,
				Body: []byte(`{"error":"not_found","message":"There is no such record here."}`)}, nil
		}
		raw, _ := json.Marshal(body)
		return exec.Response{Status: http.StatusOK, Header: http.Header{}, Body: raw}, nil
	}
}

// asked is a comment asking ask a question with two options.
func asked(id, ask string, extra map[string]any) map[string]any {
	c := map[string]any{"id": id, "author": "agent-cto", "ask": ask, "body": "Which way?",
		"decision": map[string]any{"question": "Which way should the edge route it?",
			"options": []any{map[string]any{"id": "worker", "label": "A Worker route"},
				map[string]any{"id": "defer", "label": "Defer"}}}}
	maps.Copy(c, extra)
	return c
}

// remark is a comment that asks nothing.
func remark(id string) map[string]any {
	return map[string]any{"id": id, "author": "agent-swe", "body": "Noted."}
}

func previewAnswer(t *testing.T, read proposal.Read, args map[string]any) (proposal.Preview, error) {
	t.Helper()
	tool := mustTool(t, "answer_decision")
	return tool.Preview(t.Context(), read, wednesday, args)
}

// AN ANSWER IS BOUND TO THE QUESTION IT PREVIEWED. The preview reads the
// question from the task's thread, page by page back to it, binds the
// question as the card shows it (whom it asks, its words and decision,
// whether it is open), and its request names the task and the question by
// their ids. Re-read unchanged it is the same revision; answered meanwhile,
// edited or removed it is another, so the approval refuses the change as
// stale; and a question no longer there is gone. Mutation: bind no revision,
// leave `resolved` out of it, or read only the newest page, and this fails.
func TestAnAnswerIsBoundToTheQuestionItPreviewed(t *testing.T) {
	t.Parallel()
	older := []any{asked("c-9", "ana", nil), remark("c-10")}
	newest := []any{remark("c-11"), remark("c-12")}
	read := threadOf(newest, older)
	p, err := previewAnswer(t, read, map[string]any{"item": "ENG-41", "answers": "c-9",
		"choice": "worker", "body": "Fail open."})
	if err != nil {
		t.Fatalf("an answer to a question on an older page of the thread: %v", err)
	}
	r, err := proposal.ParseRevision(p.SourceRevision)
	if err != nil || r.Kind != proposal.RevisionAsk || r.ID != taskID+"/c-9" {
		t.Fatalf("the answer binds %q (%v), want the question by its task's id and its own",
			p.SourceRevision, err)
	}
	var sent struct{ Args map[string]any }
	if err := json.Unmarshal(p.Request.Body, &sent); err != nil {
		t.Fatal(err)
	}
	if sent.Args["item"] != taskID || sent.Args["answers"] != "c-9" ||
		sent.Args["choice"] != "worker" {
		t.Errorf("the approval would send %v, want the task and the question by their ids",
			sent.Args)
	}
	if now, err := catalog.Revision(t.Context(), read, p.SourceRevision); err != nil ||
		now != p.SourceRevision {
		t.Errorf("an unchanged question re-reads as %q (%v), want %q", now, err, p.SourceRevision)
	}
	for name, changed := range map[string]map[string]any{
		"answered meanwhile": {"resolved": true},
		"edited":             {"body": "Which way, now that the release slipped?"},
		"removed":            {"removed": true, "body": ""},
	} {
		moved := threadOf(newest, []any{asked("c-9", "ana", changed), remark("c-10")})
		if now, err := catalog.Revision(t.Context(), moved, p.SourceRevision); err != nil ||
			now == p.SourceRevision {
			t.Errorf("a question %s re-reads as the revision it was previewed at (%q, %v)",
				name, now, err)
		}
	}
	gone := threadOf(newest, []any{remark("c-10")})
	if _, err := catalog.Revision(t.Context(), gone, p.SourceRevision); !errors.Is(err,
		proposal.ErrGone) {
		t.Errorf("a question no longer on the thread re-reads with %v, want it gone", err)
	}
}

// AN ANSWER ANSWERS ONLY AN OPEN QUESTION PUT TO THE PERSON. Named by its
// comment id, a comment that asks nothing, a question asked of somebody else
// and one already answered are each refused before any card, naming why: an
// answer the assistant prepares never answers what text it read asked it to
// answer for somebody else. Named by none, the answer is the one open
// question on the task asked of the person, found across the whole thread,
// and two are refused naming both. Mutation: drop the person check, or infer
// from the newest page alone, and this fails.
func TestAnAnswerAnswersOnlyAnOpenQuestionPutToThePerson(t *testing.T) {
	t.Parallel()
	read := threadOf(
		[]any{asked("c-5", "bo", nil), remark("c-6"),
			asked("c-7", "ana", map[string]any{"resolved": true})},
		[]any{asked("c-3", "ana", nil), remark("c-4")})
	for _, tc := range []struct {
		answers string
		want    string
	}{
		{"c-4", "asks no question"},
		{"c-5", "is asked of bo"},
		{"c-7", "already answered"},
		{"c-99", "holds no comment c-99"},
	} {
		_, err := previewAnswer(t, read, map[string]any{"item": "ENG-41",
			"answers": tc.answers, "body": "Yes."})
		if !errors.Is(err, catalog.ErrInvalidArguments) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("an answer to %s previewed with %v, want it refused saying %q", tc.answers,
				err, tc.want)
		}
	}
	p, err := previewAnswer(t, read, map[string]any{"item": "ENG-41", "body": "Yes."})
	if err != nil || !strings.Contains(p.SourceRevision, "/c-3@") {
		t.Errorf("an answer naming no question binds %q (%v), want the one open question "+
			"put to the person, on the thread's older page", p.SourceRevision, err)
	}
	two := threadOf([]any{asked("c-2", "ana", nil)}, []any{asked("c-1", "ana", nil)})
	_, err = previewAnswer(t, two, map[string]any{"item": "ENG-41", "body": "Yes."})
	if !errors.Is(err, catalog.ErrInvalidArguments) || !strings.Contains(err.Error(), "c-2, c-1") {
		t.Errorf("two open questions previewed with %v, want both named", err)
	}
}
