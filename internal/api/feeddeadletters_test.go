package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/queue"
)

// fakeDeadLetters is the wake feeds' dead-letter seam: the listing it answers,
// the entries a discard can take, every discard it was asked for, and the
// refusal it answers them with. The zero value is feeds holding nothing.
type fakeDeadLetters struct {
	mu        sync.Mutex
	listing   engine.FeedDeadLetterListing
	held      map[uint64]engine.FeedDeadLetter
	refuse    error
	discarded []engine.FeedDeadLetterDiscard
}

func (f *fakeDeadLetters) List(context.Context) engine.FeedDeadLetterListing {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listing
}

func (f *fakeDeadLetters) Discard(_ context.Context,
	req engine.FeedDeadLetterDiscard) (engine.FeedDeadLetter, error) {

	f.mu.Lock()
	defer f.mu.Unlock()
	f.discarded = append(f.discarded, req)
	if f.refuse != nil {
		return engine.FeedDeadLetter{}, f.refuse
	}
	entry, held := f.held[req.ID]
	if !held {
		return engine.FeedDeadLetter{}, fmt.Errorf("%w: %d is on no wake feed's subject",
			engine.ErrNoDeadLetter, req.ID)
	}
	delete(f.held, req.ID)
	return entry, nil
}

// asked is every discard the engine was asked for so far.
func (f *fakeDeadLetters) asked() []engine.FeedDeadLetterDiscard {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.discarded)
}

// deadLetterAt is the instant the cases' entries are dated from.
var deadLetterAt = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

// readEntry is a dead letter this build read: every field the feed kept of
// its record.
var readEntry = engine.FeedDeadLetter{
	ID: 41, Source: "work", Stream: "CREWLET_TRACKER_LOG", Position: 1234, Generation: 1,
	Key: "crewlet.tracker.task.T-1", OpID: "op-41", Reason: "unknown_key",
	Detail:   "the frame is sealed under a key this node does not hold (it holds [k1])",
	StoredAt: deadLetterAt.Add(-3 * time.Hour), Delivered: 312,
	DeadLetteredAt: deadLetterAt.Add(-2 * time.Hour), KeptAt: deadLetterAt.Add(-2 * time.Hour),
	ExpiresAt: deadLetterAt.Add(30*24*time.Hour - 2*time.Hour),
}

// postAs runs one authenticated write and returns the recorder.
func postAs(t *testing.T, a *api.App, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, req)
	return rec
}

// envelope is a recorder's body, decoded.
func envelope(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("undecodable body %q: %v", rec.Body.String(), err)
	}
	return body
}

// THE LISTING IS THE ENGINE'S, FIELD FOR FIELD, AND IT IS ADMIN'S: every feed
// in order, an entry by every field the feed kept of its record and never its
// frame, an entry this build could not read by its id and why, a feed holding
// nothing as an empty list, and a feed that could not be listed by its error
// rather than as an empty one. A caller with no credential is refused it —
// the posture matrix holds every other shape.
//
// Mutation: drop a field's tag, or answer an unread feed with an empty list,
// and the decoded answer no longer says it.
func TestTheDeadLetterListingIsTheEnginesFieldForField(t *testing.T) {
	t.Parallel()
	unreadable := engine.FeedDeadLetter{ID: 57, Source: "work", KeptAt: deadLetterAt.Add(-time.Hour),
		ExpiresAt: deadLetterAt.Add(30*24*time.Hour - time.Hour),
		DecodeError: "changefeed: the dead letter is version 2, and this build reads up to 1: " +
			"a newer build kept it"}
	seam := &fakeDeadLetters{listing: engine.FeedDeadLetterListing{At: deadLetterAt,
		Feeds: []engine.FeedDeadLetterFeed{
			{Source: "work", Group: "crewlet-tracker-feed", Subject: "dlq.changefeed.work.crewlet-tracker-feed.x",
				Entries: []engine.FeedDeadLetter{readEntry, unreadable}},
			{Source: "page", Group: "crewlet-pages-feed", Subject: "dlq.changefeed.page.crewlet-pages-feed.y",
				ListError: "nats: no responders available for request"},
			{Source: "chat", Group: "crewlet-chat-feed", Subject: "dlq.changefeed.chat.crewlet-chat-feed.z",
				Entries: []engine.FeedDeadLetter{}},
		}}}
	b := closedPosture()
	a := newApp(t, api.Options{Bootstrap: &b, DeadLetters: seam})
	anonymous := httptest.NewRecorder()
	a.ServeHTTP(anonymous, httptest.NewRequest(http.MethodGet, "/feeds/dead-letters", nil))
	if anonymous.Code != http.StatusUnauthorized {
		t.Fatalf("an anonymous read of the dead letters answered %d: they are the "+
			"deployment's, admin's like the fleet view", anonymous.Code)
	}

	status, body := getAs(t, a, "/feeds/dead-letters")
	if status != http.StatusOK || body["at"] != deadLetterAt.Format(time.RFC3339) {
		t.Fatalf("GET /feeds/dead-letters = %d %v", status, body)
	}
	feeds, _ := body["feeds"].([]any)
	if len(feeds) != 3 {
		t.Fatalf("the answer names %d feeds, want the three the engine listed: %v", len(feeds), body)
	}
	work := feeds[0].(map[string]any)
	entries, _ := work["entries"].([]any)
	if work["source"] != "work" || work["group"] != "crewlet-tracker-feed" ||
		work["subject"] == "" || len(entries) != 2 {
		t.Fatalf("the work feed reads %v", work)
	}
	read := entries[0].(map[string]any)
	for field, want := range map[string]any{
		"id": float64(41), "source": "work", "stream": "CREWLET_TRACKER_LOG",
		"position": float64(1234), "generation": float64(1), "key": "crewlet.tracker.task.T-1",
		"op_id": "op-41", "reason": "unknown_key", "detail": readEntry.Detail,
		"stored_at": readEntry.StoredAt.Format(time.RFC3339), "delivered": float64(312),
		"dead_lettered_at": readEntry.DeadLetteredAt.Format(time.RFC3339),
		"kept_at":          readEntry.KeptAt.Format(time.RFC3339),
		"expires_at":       readEntry.ExpiresAt.Format(time.RFC3339),
	} {
		if read[field] != want {
			t.Errorf("the read entry's %s is %v, want %v", field, read[field], want)
		}
	}
	if _, framed := read["frame"]; framed || len(read) != 14 {
		t.Errorf("the read entry carries %d fields, want the fourteen named and never a frame: %v",
			len(read), read)
	}
	undecoded := entries[1].(map[string]any)
	if undecoded["id"] != float64(57) || undecoded["decode_error"] != unreadable.DecodeError ||
		undecoded["kept_at"] == nil || undecoded["expires_at"] == nil || len(undecoded) != 5 {
		t.Errorf("the entry this build cannot read reads %v, want its id, source, instants "+
			"and why, and nothing it could not read", undecoded)
	}
	page := feeds[1].(map[string]any)
	if page["list_error"] != "nats: no responders available for request" || page["entries"] != nil {
		t.Errorf("the feed that could not be listed reads %v, want its error and no entries", page)
	}
	chat := feeds[2].(map[string]any)
	if empty, ok := chat["entries"].([]any); !ok || len(empty) != 0 || chat["list_error"] != nil {
		t.Errorf("the feed holding nothing reads %v, want an empty list and no error", chat)
	}
}

// A DISCARD REPEATS ITS ID, NAMES WHO PRESSED, AND ANSWERS WHAT IT REMOVED: a
// discard whose confirmation does not repeat the id, and one naming no id the
// broker could hold, reach no engine at all; a confirmed one reaches it once,
// carrying the caller's author and credential for the engine's line, and
// answers the entry; and the same discard again finds nothing.
//
// Mutation: take the id from the confirmation alone, skip the spelling check,
// or drop the caller from the request, and a case goes red.
func TestADiscardConfirmsItsIDNamesWhoPressedAndAnswersTheEntry(t *testing.T) {
	t.Parallel()
	seam := &fakeDeadLetters{held: map[uint64]engine.FeedDeadLetter{41: readEntry}}
	b := closedPosture()
	a := newApp(t, api.Options{Bootstrap: &b, DeadLetters: seam})

	for _, path := range []string{
		"/feeds/dead-letters/41/discard",
		"/feeds/dead-letters/41/discard?confirm=42",
	} {
		if rec := postAs(t, a, path); rec.Code != http.StatusBadRequest ||
			envelope(t, rec)["error"] != "confirm_required" {
			t.Errorf("%s answered %d %s, want 400 confirm_required", path, rec.Code, rec.Body)
		}
	}
	for _, path := range []string{
		"/feeds/dead-letters/0/discard?confirm=0",
		"/feeds/dead-letters/x41/discard?confirm=x41",
		"/feeds/dead-letters/-41/discard?confirm=-41",
	} {
		rec := postAs(t, a, path)
		if body := envelope(t, rec); rec.Code != http.StatusBadRequest ||
			body["error"] != "dead_letter_invalid" || body["hint"] == nil {
			t.Errorf("%s answered %d %v, want 400 dead_letter_invalid with a hint", path, rec.Code, body)
		}
	}
	if asked := seam.asked(); len(asked) != 0 {
		t.Fatalf("a refused discard reached the engine: %+v", asked)
	}

	rec := postAs(t, a, "/feeds/dead-letters/41/discard?confirm=41")
	body := envelope(t, rec)
	if rec.Code != http.StatusOK || body["id"] != float64(41) || body["source"] != "work" ||
		body["stream"] != "CREWLET_TRACKER_LOG" || body["position"] != float64(1234) {
		t.Fatalf("a confirmed discard answered %d %v, want the entry it removed", rec.Code, body)
	}
	asked := seam.asked()
	if len(asked) != 1 || asked[0].ID != 41 || asked[0].By.Kind != iam.ActorOperator ||
		asked[0].By.OperatorID != iam.TokenLogin("founder") || asked[0].By.Name == "" {
		t.Fatalf("the engine was asked %+v, want entry 41 discarded as the founder token", asked)
	}

	rec = postAs(t, a, "/feeds/dead-letters/41/discard?confirm=41")
	if again := envelope(t, rec); rec.Code != http.StatusNotFound ||
		again["error"] != "no_dead_letter" || again["hint"] == nil {
		t.Errorf("discarding it again answered %d %v, want 404 no_dead_letter with a hint",
			rec.Code, again)
	}
}

// EVERY REFUSAL OF A DISCARD SENDS THE OPERATOR SOMEWHERE OF ITS OWN: an id no
// feed holds is a 404, never a 503; a feed that could not be listed is a 503,
// never a 404 a feed that did not answer could contradict — with a Retry-After
// only where waiting may clear it; a drop the broker did not answer is a 503
// to read the listing after; anything else a 500.
//
// Mutation: answer an unread feed as `no_dead_letter`, or put a Retry-After on
// a failure waiting does not clear, and a row goes red.
func TestEachDiscardRefusalSendsTheOperatorSomewhereOfItsOwn(t *testing.T) {
	t.Parallel()
	seam := &fakeDeadLetters{}
	b := closedPosture()
	a := newApp(t, api.Options{Bootstrap: &b, DeadLetters: seam})
	unanswered := fmt.Errorf("jetstream: list dead letters: %w", queue.ErrUnavailable)
	for _, tc := range []struct {
		name   string
		err    error
		status int
		code   string
		retry  string
	}{
		{"no feed holds it", fmt.Errorf("%w: 41 went before this discard reached it",
			engine.ErrNoDeadLetter), http.StatusNotFound, "no_dead_letter", ""},
		{"a feed the broker did not list", &engine.DeadLettersUnread{ID: 41,
			Feeds: []engine.UnreadFeed{{Source: "page", Err: unanswered}}},
			http.StatusServiceUnavailable, "unavailable", "4"},
		{"a feed that could not be listed for good", &engine.DeadLettersUnread{ID: 41,
			Feeds: []engine.UnreadFeed{{Source: "page",
				Err: errors.New("jetstream: not a dead-letter subject")}}},
			http.StatusServiceUnavailable, "unavailable", ""},
		{"a drop the broker did not answer", fmt.Errorf("engine: discard dead letter 41: %w",
			queue.ErrUnavailable), http.StatusServiceUnavailable, "unavailable", "4"},
		{"anything else", errors.New("engine: discard dead letter 41: refused"),
			http.StatusInternalServerError, "dead_letter_discard_failed", ""},
	} {
		seam.mu.Lock()
		seam.refuse = tc.err
		seam.mu.Unlock()
		rec := postAs(t, a, "/feeds/dead-letters/41/discard?confirm=41")
		body := envelope(t, rec)
		if rec.Code != tc.status || body["error"] != tc.code || body["hint"] == nil ||
			rec.Header().Get("Retry-After") != tc.retry {
			t.Errorf("%s answered %d %v (Retry-After %q), want %d %s (Retry-After %q) with a hint",
				tc.name, rec.Code, body, rec.Header().Get("Retry-After"), tc.status, tc.code, tc.retry)
		}
	}
}
