package executor_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// admitAll starts session's execution for plugin a, answers its first poll
// with notes and waits until each is admitted, returning the poll it holds
// next.
func admitAll(t *testing.T, route *notesRoute, pr *probe, notes ...mcpbridge.SteerNote) *notesCall {
	t.Helper()
	pr.event("a", "session.execution.started", steerSession, nil)
	route.next("GET notes", "poll once the session's execution started").reply(http.StatusOK, notesBody(notes...))
	for _, n := range notes {
		wantAdmitted(t, pr.expect("prompt-started").Input, steerSession, n)
		if !pr.expect("prompt").OK {
			t.Fatal("the stand-in refused an admission it was not told to")
		}
	}
	return route.next("GET notes", "poll past the notes it admitted")
}

// answerAgain answers the plugin's ask for second ids, which must name asked:
// granted are given their second id in job-1, spent are answered spent.
func answerAgain(t *testing.T, route *notesRoute, asked []mcpbridge.SteerNote, granted, spent []string) {
	t.Helper()
	ask := route.next("POST again", "ask for the folded notes' second ids")
	ids := []string{}
	for _, n := range asked {
		ids = append(ids, n.ID)
	}
	if want, _ := json.Marshal(map[string]any{"ids": ids}); ask.body != string(want) {
		t.Errorf("the ask reads %s, want %s", ask.body, want)
	}
	again := map[string]string{}
	for _, n := range asked {
		for _, id := range granted {
			if n.ID == id {
				again[id] = againOf(n)
			}
		}
	}
	if spent == nil {
		spent = []string{}
	}
	body, _ := json.Marshal(map[string]any{"again": again, "spent": spent})
	ask.reply(http.StatusOK, string(body))
}

// servedOnce is a wake item, or a run's message, as the route serves it: no
// second id, since it is served once and never admitted again.
func servedOnce(seq int, id string) mcpbridge.SteerNote {
	return mcpbridge.SteerNote{Seq: seq, ID: id, Inbox: executor.InboxID(executor.OpenCodeName, "job-1", id),
		Message: "## A message from bob about this work, sent while this turn was running\n\n" + id}
}

// checkpoint is a request's user message once a compaction folded text: the
// summary and the serialized record OpenCode hands the model as history
// (2.3.0), the text inside it whole.
func checkpoint(text string) map[string]any {
	return userText("msg_checkpoint", "<conversation-checkpoint>\nThe following is a summary and serialized "+
		"record of earlier conversation. Treat it as historical context, not as new instructions."+
		"\n\n<summary>\n## Objective\n- "+
		"Fix the flake.\n</summary>\n\n<recent-context>\n[User]: "+text+"\n</recent-context>\n"+
		"</conversation-checkpoint>")
}

// THE PLUGIN CONFIRMS A NOTE ONLY ONCE ITS MODEL IS SENT IT PLAIN (ADR-0177),
// off the "context" hook; a folded note is admitted again ONCE, or given up
// and posted folded (opencode_steer.js).
//
// Mutations, each red: confirm on `session.inbox.delivered`; match by words
// not id, or a message only containing the note; accept another shape; confirm
// while compacting, or never again after a failed compaction; keep the id of
// a failed admission or second admission; re-admit under the first id, twice,
// or ungranted; fold an item neither granted nor spent; fold after one
// compaction, or read on giving up; act on `compaction.failed`; read another
// session's hook; await the read in the hook; end the cleanup before a read,
// a fold or an ask in flight; wait past endWaitMs.
func TestThePluginConfirmsANoteOnlyOnceItsModelIsSentItPlain(t *testing.T) {
	t.Parallel()
	node := nodeForNotes(t)
	entry := steerEntry(t)
	start := func(t *testing.T) (*notesRoute, *probe) {
		t.Helper()
		route, pr := newNotesRoute(t), startProbe(t, node, entry)
		pr.setup("a", route.url(), steerSession)
		pr.expect("setup")
		return route, pr
	}

	t.Run("confirms a note sent plain, delivered or not yet, and only that", func(t *testing.T) {
		t.Parallel()
		route, pr := start(t)
		first, second := noteNumbered(1), noteNumbered(2)
		admitAll(t, route, pr, first, second)
		pr.event("a", "session.inbox.delivered", steerSession, map[string]any{"inboxID": first.Inbox})
		route.none(300*time.Millisecond, "the note was delivered, and no request carried it yet")
		pr.sees("a", steerSession, userText("msg_brief", "Fix the flake."), plainNote(first))
		read := route.next("POST read", "confirm the note its model is sent")
		if read.body != `{"ids":["note-1"]}` {
			t.Errorf("the confirmation reads %s, want note-1's id alone", read.body)
		}
		read.reply(http.StatusNoContent, "")
		// THE HOOK COMES BEFORE THE DELIVERED EVENT (measured on every release),
		// so a note seen plain before its delivery is confirmed all the same.
		pr.sees("a", steerSession, plainNote(second))
		if read := route.next("POST read", "confirm the note seen plain before its delivery"); read.body !=
			`{"ids":["note-2"]}` {
			t.Errorf("the confirmation reads %s, want note-2's id alone", read.body)
		}
	})

	t.Run("admits a folded note again once and confirms it once sent plain", func(t *testing.T) {
		t.Parallel()
		route, pr := start(t)
		note := noteNumbered(1)
		admitAll(t, route, pr, note)
		pr.event("a", "session.inbox.delivered", steerSession, map[string]any{"inboxID": note.Inbox})
		pr.event("a", "session.compaction.started", steerSession, map[string]any{"reason": "auto"})
		pr.sees("a", steerSession, checkpoint(note.Message))
		route.none(300*time.Millisecond, "no compaction has ended, so no second id is asked for")
		pr.event("a", "session.compaction.ended", steerSession, map[string]any{"reason": "auto"})
		answerAgain(t, route, []mcpbridge.SteerNote{note}, []string{note.ID}, nil)
		wantReadmitted(t, pr.expect("prompt-started").Input, steerSession, note)
		pr.expect("prompt")
		route.none(300*time.Millisecond, "the note reached its model only inside the checkpoint")
		pr.sees("a", steerSession, checkpoint(note.Message), userText(againOf(note), note.Message))
		if read := route.next("POST read", "confirm the note admitted again once sent plain"); read.body !=
			`{"ids":["note-1"]}` {
			t.Errorf("the confirmation reads %s, want note-1's id alone", read.body)
		}
		pr.event("a", "session.compaction.ended", steerSession, map[string]any{"reason": "auto"})
		pr.quiet(500*time.Millisecond, "the note was confirmed, so no compaction admits it again")
	})

	t.Run("gives a note folded twice up: posted folded, never read", func(t *testing.T) {
		t.Parallel()
		route, pr := start(t)
		note := noteNumbered(1)
		admitAll(t, route, pr, note)
		pr.event("a", "session.inbox.delivered", steerSession, map[string]any{"inboxID": note.Inbox})
		pr.event("a", "session.compaction.ended", steerSession, nil)
		answerAgain(t, route, []mcpbridge.SteerNote{note}, []string{note.ID}, nil)
		wantReadmitted(t, pr.expect("prompt-started").Input, steerSession, note)
		pr.expect("prompt")
		pr.event("a", "session.inbox.delivered", steerSession, map[string]any{"inboxID": againOf(note)})
		pr.event("a", "session.compaction.ended", steerSession, nil)
		fold := route.next("POST folded", "post the note it gave up, asking no second id again")
		if fold.body != `{"ids":["note-1"]}` {
			t.Errorf("the fold reads %s, want note-1's id alone", fold.body)
		}
		fold.reply(http.StatusNoContent, "")
		pr.sees("a", steerSession, checkpoint(note.Message), userText(againOf(note), note.Message))
		pr.event("a", "session.compaction.ended", steerSession, nil)
		pr.quiet(500*time.Millisecond, "the note was given up")
		route.none(300*time.Millisecond, "a note given up is never confirmed nor posted again")
	})

	t.Run("gives a note up whose second admission is spent: posted folded, never read", func(t *testing.T) {
		t.Parallel()
		route, pr := start(t)
		note := noteNumbered(1)
		admitAll(t, route, pr, note)
		pr.event("a", "session.inbox.delivered", steerSession, map[string]any{"inboxID": note.Inbox})
		pr.event("a", "session.compaction.ended", steerSession, nil)
		answerAgain(t, route, []mcpbridge.SteerNote{note}, nil, []string{note.ID})
		fold := route.next("POST folded", "post the note the engine answered spent")
		if fold.body != `{"ids":["note-1"]}` {
			t.Errorf("the fold reads %s, want note-1's id alone", fold.body)
		}
		fold.reply(http.StatusNoContent, "")
		pr.sees("a", steerSession, checkpoint(note.Message), plainNote(note))
		pr.quiet(500*time.Millisecond, "a note spent is never admitted again")
		route.none(300*time.Millisecond, "a note given up is never confirmed")
	})

	t.Run("admits nothing again and posts nothing when a compaction fails", func(t *testing.T) {
		t.Parallel()
		route, pr := start(t)
		note := noteNumbered(1)
		admitAll(t, route, pr, note)
		pr.event("a", "session.inbox.delivered", steerSession, map[string]any{"inboxID": note.Inbox})
		pr.event("a", "session.compaction.started", steerSession, map[string]any{"reason": "auto"})
		pr.event("a", "session.compaction.failed", steerSession,
			map[string]any{"reason": "auto", "error": map[string]any{"type": "compaction.failed"}})
		pr.event("a", "session.execution.failed", steerSession, nil)
		pr.quiet(500*time.Millisecond, "the compaction failed")
		route.none(300*time.Millisecond, "the compaction failed, and the note never reached its model")
	})

	t.Run("confirms again once a compaction failed", func(t *testing.T) {
		t.Parallel()
		route, pr := start(t)
		note := noteNumbered(1)
		admitAll(t, route, pr, note)
		pr.event("a", "session.compaction.started", steerSession, map[string]any{"reason": "auto"})
		pr.event("a", "session.compaction.failed", steerSession,
			map[string]any{"reason": "auto", "error": map[string]any{"type": "compaction.failed"}})
		// THE SESSION COMPACTS NO MORE: its next request is one a note is
		// confirmed off, as before the compaction started.
		pr.sees("a", steerSession, plainNote(note))
		if read := route.next("POST read", "confirm the note sent plain after the compaction failed"); read.body !=
			`{"ids":["note-1"]}` {
			t.Errorf("the confirmation reads %s, want note-1's id alone", read.body)
		}
	})

	t.Run("confirms nothing under the id of an admission that failed", func(t *testing.T) {
		t.Parallel()
		route, pr := start(t)
		note := noteNumbered(1)
		pr.send(map[string]any{"op": "fail-prompts", "name": "a", "count": 1})
		pr.event("a", "session.execution.started", steerSession, nil)
		route.next("GET notes", "poll once the session's execution started").reply(http.StatusOK, notesBody(note))
		wantAdmitted(t, pr.expect("prompt-started").Input, steerSession, note)
		if pr.expect("prompt").OK {
			t.Fatal("the stand-in admitted what it was told to refuse")
		}
		again := route.next("GET notes", "ask again for the note it could not admit")
		// A MESSAGE UNDER THE FAILED ADMISSION'S ID, which OpenCode never made:
		// the plugin let the note go, so nothing confirms it.
		pr.sees("a", steerSession, plainNote(note))
		route.none(300*time.Millisecond, "the note's admission failed, so no message is its own")
		again.reply(http.StatusOK, notesBody(note))
		wantAdmitted(t, pr.expect("prompt-started").Input, steerSession, note)
		if !pr.expect("prompt").OK {
			t.Fatal("the stand-in refused an admission it was not told to")
		}
		route.next("GET notes", "poll past the admitted note")
		pr.sees("a", steerSession, plainNote(note))
		if read := route.next("POST read", "confirm the note once admitted"); read.body != `{"ids":["note-1"]}` {
			t.Errorf("the confirmation reads %s, want note-1's id alone", read.body)
		}
	})

	t.Run("confirms nothing under the second id of an admission again that failed", func(t *testing.T) {
		t.Parallel()
		route, pr := start(t)
		note := noteNumbered(1)
		admitAll(t, route, pr, note)
		pr.event("a", "session.inbox.delivered", steerSession, map[string]any{"inboxID": note.Inbox})
		// Ordered before the compaction's end, which leads to the admission.
		pr.send(map[string]any{"op": "fail-prompts", "name": "a", "count": 1})
		pr.event("a", "session.compaction.ended", steerSession, nil)
		answerAgain(t, route, []mcpbridge.SteerNote{note}, []string{note.ID}, nil)
		wantReadmitted(t, pr.expect("prompt-started").Input, steerSession, note)
		if pr.expect("prompt").OK {
			t.Fatal("the stand-in admitted again what it was told to refuse")
		}
		pr.sees("a", steerSession, checkpoint(note.Message), userText(againOf(note), note.Message))
		route.none(300*time.Millisecond, "the second admission failed, so no message is the note's own")
	})

	t.Run("never admits again nor confirms a folded wake or run's message", func(t *testing.T) {
		t.Parallel()
		route, pr := start(t)
		wake, message := servedOnce(1, "wake:job-1:w-1"), servedOnce(2, "wake:job-1:m-1")
		admitAll(t, route, pr, wake, message)
		for _, n := range []mcpbridge.SteerNote{wake, message} {
			pr.event("a", "session.inbox.delivered", steerSession, map[string]any{"inboxID": n.Inbox})
		}
		pr.event("a", "session.compaction.ended", steerSession, nil)
		answerAgain(t, route, []mcpbridge.SteerNote{wake, message}, nil, nil)
		pr.quiet(500*time.Millisecond, "an item served once is never admitted again")
		pr.sees("a", steerSession, checkpoint(wake.Message+"\n"+message.Message))
		route.none(300*time.Millisecond, "a folded item served once is neither confirmed nor posted folded")
	})

	t.Run("confirms nothing from another session's request or events", func(t *testing.T) {
		t.Parallel()
		route, pr := start(t)
		note := noteNumbered(1)
		admitAll(t, route, pr, note)
		pr.sees("a", otherSession, plainNote(note))
		pr.event("a", "session.inbox.delivered", otherSession, map[string]any{"inboxID": note.Inbox})
		pr.event("a", "session.compaction.ended", otherSession, nil)
		pr.quiet(500*time.Millisecond, "only another session compacted")
		route.none(300*time.Millisecond, "only another session's model was sent the note")
	})

	t.Run("never holds a request on a confirmation in flight", func(t *testing.T) {
		t.Parallel()
		route, pr := start(t)
		first, second := noteNumbered(1), noteNumbered(2)
		admitAll(t, route, pr, first, second)
		pr.sees("a", steerSession, plainNote(first))
		stalled := route.next("POST read", "confirm the first note")
		// A HOOK THAT AWAITED THE READ would never report `hooked` here.
		pr.sees("a", steerSession, plainNote(first), plainNote(second))
		stalled.reply(http.StatusNoContent, "")
		if read := route.next("POST read", "confirm the second note"); read.body != `{"ids":["note-2"]}` {
			t.Errorf("the next confirmation reads %s, want note-2's id alone", read.body)
		}
	})

	t.Run("confirms the note sent plain and admits again the one folded", func(t *testing.T) {
		t.Parallel()
		route, pr := start(t)
		first, second := noteNumbered(1), noteNumbered(2)
		admitAll(t, route, pr, first, second)
		for _, n := range []mcpbridge.SteerNote{first, second} {
			pr.event("a", "session.inbox.delivered", steerSession, map[string]any{"inboxID": n.Inbox})
		}
		pr.sees("a", steerSession, plainNote(first))
		route.next("POST read", "confirm the note sent plain").reply(http.StatusNoContent, "")
		pr.event("a", "session.compaction.ended", steerSession, nil)
		answerAgain(t, route, []mcpbridge.SteerNote{second}, []string{second.ID}, nil)
		wantReadmitted(t, pr.expect("prompt-started").Input, steerSession, second)
		pr.expect("prompt")
		pr.quiet(500*time.Millisecond, "only the folded note is admitted again")
	})

	t.Run("confirms no message of another shape", func(t *testing.T) {
		t.Parallel()
		route, pr := start(t)
		note := noteNumbered(1)
		admitAll(t, route, pr, note)
		// EACH UNDER THE NOTE'S OWN ADMITTED ID, so only the shape fails.
		pr.sees("a", steerSession,
			map[string]any{"id": note.Inbox, "role": "user", "content": note.Message},
			map[string]any{"id": note.Inbox, "role": "user", "content": []map[string]any{
				{"type": "text", "text": note.Message}, {"type": "file", "mime": "text/plain"}}},
			map[string]any{"id": note.Inbox, "role": "user", "content": []map[string]any{
				{"type": "text", "text": note.Message[:10]}, {"type": "text", "text": note.Message[10:]}}},
			map[string]any{"id": note.Inbox, "role": "assistant", "content": []map[string]any{
				{"type": "text", "text": note.Message}}},
		)
		route.none(500*time.Millisecond, "no request held the note as one user text part")
	})

	t.Run("confirms a note only on the message of its own admission", func(t *testing.T) {
		t.Parallel()
		route, pr := start(t)
		// TWO NOTES OF THE SAME WORDS, as one person's note sent twice is.
		first, second := noteNumbered(1), noteNumbered(2)
		second.Message = first.Message
		admitAll(t, route, pr, first, second)
		// THE SAME WORDS UNDER NO ID THIS RUN ADMITTED: an earlier run's message
		// of a continued session, or no id at all.
		pr.sees("a", steerSession, userText("msg_00000000000000000000000000000000", first.Message),
			map[string]any{"role": "user", "content": []map[string]any{{"type": "text", "text": first.Message}}})
		route.none(300*time.Millisecond, "no message was either note's own admission")
		pr.sees("a", steerSession, plainNote(first))
		read := route.next("POST read", "confirm the note its own message is")
		if read.body != `{"ids":["note-1"]}` {
			t.Errorf("the confirmation reads %s, want note-1 alone, never the note of the same words", read.body)
		}
		// ANSWERED, as the route answers: the plugin holds one confirmation in
		// flight, so an unanswered one would hold the second until its abort.
		read.reply(http.StatusNoContent, "")
		pr.sees("a", steerSession, plainNote(first), plainNote(second))
		if read := route.next("POST read", "confirm the second note off its own message"); read.body !=
			`{"ids":["note-2"]}` {
			t.Errorf("the confirmation reads %s, want note-2 alone", read.body)
		}
	})

	t.Run("confirms nothing while the session compacts", func(t *testing.T) {
		t.Parallel()
		route, pr := start(t)
		note := noteNumbered(1)
		admitAll(t, route, pr, note)
		pr.event("a", "session.compaction.started", steerSession, map[string]any{"reason": "auto"})
		pr.sees("a", steerSession, plainNote(note))
		route.none(300*time.Millisecond, "a request while the session compacts confirms nothing")
		pr.event("a", "session.compaction.ended", steerSession, map[string]any{"reason": "auto"})
		pr.sees("a", steerSession, plainNote(note))
		if read := route.next("POST read", "confirm the note once the compaction ended"); read.body !=
			`{"ids":["note-1"]}` {
			t.Errorf("the confirmation reads %s, want note-1", read.body)
		}
	})

	t.Run("lets a fold in flight finish before its cleanup ends", func(t *testing.T) {
		t.Parallel()
		route, pr := start(t)
		note := noteNumbered(1)
		admitAll(t, route, pr, note)
		pr.event("a", "session.inbox.delivered", steerSession, map[string]any{"inboxID": note.Inbox})
		pr.event("a", "session.compaction.ended", steerSession, nil)
		answerAgain(t, route, []mcpbridge.SteerNote{note}, nil, []string{note.ID})
		inFlight := route.next("POST folded", "post the note it gave up")
		pr.send(map[string]any{"op": "cleanup", "name": "a"})
		pr.quiet(500*time.Millisecond, "the fold is still in flight")
		inFlight.reply(http.StatusNoContent, "")
		pr.expect("cleaned")
	})

	t.Run("lets an ask for a second id in flight finish before its cleanup ends", func(t *testing.T) {
		t.Parallel()
		route, pr := start(t)
		note := noteNumbered(1)
		admitAll(t, route, pr, note)
		pr.event("a", "session.inbox.delivered", steerSession, map[string]any{"inboxID": note.Inbox})
		pr.event("a", "session.compaction.ended", steerSession, nil)
		inFlight := route.next("POST again", "ask for the folded note's second id")
		pr.send(map[string]any{"op": "cleanup", "name": "a"})
		pr.quiet(500*time.Millisecond, "the ask is still in flight")
		inFlight.reply(http.StatusOK, `{"again":{},"spent":[]}`)
		pr.expect("cleaned")
	})

	t.Run("lets a confirmation in flight finish before its cleanup ends", func(t *testing.T) {
		t.Parallel()
		route, pr := start(t)
		note := noteNumbered(1)
		admitAll(t, route, pr, note)
		pr.sees("a", steerSession, plainNote(note))
		inFlight := route.next("POST read", "confirm the note")
		pr.send(map[string]any{"op": "cleanup", "name": "a"})
		pr.quiet(500*time.Millisecond, "the confirmation is still in flight")
		inFlight.reply(http.StatusNoContent, "")
		pr.expect("cleaned")
	})

	t.Run("ends a confirmation that never answers within its cleanup's bound", func(t *testing.T) {
		t.Parallel()
		route, pr := start(t)
		note := noteNumbered(1)
		admitAll(t, route, pr, note)
		pr.sees("a", steerSession, plainNote(note))
		hung := route.next("POST read", "confirm the note")
		pr.send(map[string]any{"op": "cleanup", "name": "a"})
		pr.expect("cleaned")
		route.letGo(hung, "the cleanup's bound passed")
	})
}
