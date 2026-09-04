package work_test

import (
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/work"
)

// A ROLLING UPGRADE PUTS TWO BUILDS ON ONE BUCKET, and an item is READ,
// MODIFIED and WRITTEN BACK by whichever node the request landed on. Without
// this, an older build's dashboard PATCH strips a newer build's field out of
// the head and the fleet loses it silently and permanently.
func TestAnUnknownFieldSurvivesAReadModifyWrite(t *testing.T) {
	t.Parallel()
	// What a newer build wrote: every field this build knows, plus two it
	// does not.
	raw := []byte(`{
		"v": 1, "id": "i1", "key": "ENG-1", "project": "ENG", "type": "task",
		"title": "old title", "status": "todo",
		"created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-01T00:00:00Z",
		"change_seq": 3,
		"estimate_points": 8,
		"sprint": {"id": "s-4", "name": "Q1 W3"}
	}`)

	item, err := work.DecodeItem(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if item.Title != "old title" || item.Status != work.StatusTodo {
		t.Fatalf("the known fields did not decode: %+v", item)
	}

	item.Title = "edited by the older build"
	out, err := work.EncodeItem(item)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	var got map[string]json.RawMessage
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if string(got["estimate_points"]) != "8" {
		t.Errorf("estimate_points came back as %q — the older build dropped a "+
			"field the newer one wrote", got["estimate_points"])
	}
	if !json.Valid(got["sprint"]) || string(got["sprint"]) == "null" {
		t.Errorf("sprint came back as %q", got["sprint"])
	}
	var title string
	if err := json.Unmarshal(got["title"], &title); err != nil || title != "edited by the older build" {
		t.Errorf("the edit did not land: %q", got["title"])
	}
}

// A VERSION ABOVE THIS BUILD'S IS REFUSED, not downgraded. Carrying unknown
// FIELDS covers an additive change; a version bump says the shape moved, and
// a build that cannot read it must not be the one rewriting it.
func TestANewerDocumentVersionIsRefused(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"v": 99, "id": "i1", "key": "ENG-1", "project": "ENG",
		"type": "task", "title": "t", "status": "todo",
		"created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-01T00:00:00Z"}`)
	_, err := work.DecodeItem(raw)
	var unknown work.ErrUnknownVersion
	if !asUnknownVersion(err, &unknown) {
		t.Fatalf("decode gave %v, want ErrUnknownVersion", err)
	}
	if unknown.Got != 99 || unknown.Want != work.DocumentVersion {
		t.Errorf("the error does not name both versions: %+v", unknown)
	}
}

func asUnknownVersion(err error, out *work.ErrUnknownVersion) bool {
	e, ok := err.(interface{ Unwrap() error })
	if ok {
		err = e.Unwrap()
	}
	v, ok := err.(work.ErrUnknownVersion)
	if ok {
		*out = v
	}
	return ok
}

// EVERY FIELD OF EVERY RECORD MUST BE KNOWN TO THE DECODER. A name the
// decoder does not recognise is put into Extra AND decoded into the struct —
// so the next encode writes the stale carried copy back over what the caller
// just set. Silent, permanent, and exactly the failure the extra map exists
// to prevent.
func TestEveryDeclaredFieldIsRecognisedByTheDecoder(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		encode func() ([]byte, error)
		decode func([]byte) (map[string]json.RawMessage, error)
	}{
		{"item", func() ([]byte, error) { return work.EncodeItem(populatedItem()) },
			func(b []byte) (map[string]json.RawMessage, error) {
				got, err := work.DecodeItem(b)
				return got.Extra, err
			}},
		{"comment", func() ([]byte, error) { return work.EncodeComment(populatedComment()) },
			func(b []byte) (map[string]json.RawMessage, error) {
				got, err := work.DecodeComment(b)
				return got.Extra, err
			}},
		{"change", func() ([]byte, error) { return work.EncodeChange(populatedChange()) },
			func(b []byte) (map[string]json.RawMessage, error) {
				got, err := work.DecodeChange(b)
				return got.Extra, err
			}},
		{"counter", func() ([]byte, error) {
			return work.EncodeCounter(work.Counter{V: 1, Project: "ENG", Last: 7})
		}, func(b []byte) (map[string]json.RawMessage, error) {
			got, err := work.DecodeCounter(b)
			return got.Extra, err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			data, err := tc.encode()
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			extra, err := tc.decode(data)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(extra) != 0 {
				t.Errorf("these declared fields were carried as unknown: %v — "+
					"add them to omittedNames, or the next write puts the stale "+
					"carried copy back over the caller's value",
					slices.Sorted(maps.Keys(extra)))
			}
		})
	}
}

// A CARRIED FIELD NEVER OVERWRITES A KNOWN ONE. The collision can only arise
// from a bug — a build putting a field it knows into the carry map — and
// letting the stale carried copy win would silently undo the write that set
// it, which is precisely the failure the carry map exists to prevent.
func TestACarriedFieldLosesToTheOneThisBuildSet(t *testing.T) {
	t.Parallel()
	item := populatedItem()
	item.Title = "what this write decided"
	item.Extra = map[string]json.RawMessage{
		"title":           json.RawMessage(`"a stale carried copy"`),
		"estimate_points": json.RawMessage(`8`),
	}
	data, err := work.EncodeItem(item)
	if err != nil {
		t.Fatal(err)
	}
	got, err := work.DecodeItem(data)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "what this write decided" {
		t.Errorf("title = %q — a carried copy overwrote the write", got.Title)
	}
	if string(got.Extra["estimate_points"]) != "8" {
		t.Errorf("the genuinely unknown field was dropped: %v", got.Extra)
	}
	if _, carried := got.Extra["title"]; carried {
		t.Error("a known field came back in the carry map")
	}
}

// A fully-populated record round-trips to an equal value, which is what makes
// a read-modify-write safe for the fields this build DOES know.
func TestAPopulatedRecordRoundTrips(t *testing.T) {
	t.Parallel()
	original := populatedItem()
	data, err := work.EncodeItem(original)
	if err != nil {
		t.Fatal(err)
	}
	got, err := work.DecodeItem(data)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, original) {
		t.Errorf("round trip changed the item:\n got %+v\nwant %+v", got, original)
	}
}

func populatedItem() work.Item {
	at := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	due := at.Add(72 * time.Hour)
	closed := at.Add(time.Hour)
	return work.Item{
		V: 1, ID: "i1", Key: "ENG-1", Project: "ENG", Type: work.TypeTask,
		ParentID: "i0", Title: "a title", Body: "a body",
		Status: work.StatusDone, CloseReason: work.CloseDuplicate, DuplicateOf: "i2",
		Priority: work.PriorityHigh, Reporter: "pm", Assignee: "eng",
		Watchers: []string{"pm", "cto"}, Muted: []string{"ops"},
		Labels: []string{"backend"},
		Links:  []work.Link{{Kind: work.LinkBlocks, To: "i3"}},
		Due:    &due, CreatedAt: at, UpdatedAt: at, ClosedAt: &closed,
		ChangeSeq: 4, Reassignments: 2,
		LastChange: &work.Change{V: 1, ID: "c1", ItemID: "i1", Kind: work.ChangeStatus},
	}
}

func populatedComment() work.Comment {
	at := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	return work.Comment{
		V: 1, ID: "m1", ItemID: "i1", Author: "eng", AuthorKind: work.AuthorAgent,
		Body: "a remark", Mentions: []string{"pm"}, ReplyTo: "m0",
		CreatedAt: at, UpdatedAt: at,
		LastChange: &work.Change{V: 1, ID: "c2", ItemID: "i1", Kind: work.ChangeComment},
	}
}

func populatedChange() work.Change {
	at := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	return work.Change{
		V: 1, ID: "c1", ItemID: "i1", Kind: work.ChangeStatus,
		Actor: "eng", ActorKind: work.AuthorAgent, OperatorID: "founder",
		Fields:    map[string]work.Delta{"status": {From: "todo", To: "in_progress"}},
		CommentID: "m1", Excerpt: "picked it up", Mentions: []string{"pm"},
		TurnID: "t1", Chain: []string{"pm", "eng"}, Quiet: true, HeadRevision: 42,
		Snapshot: work.Snapshot{
			Key: "ENG-1", Project: "ENG", Title: "a title",
			Status: work.StatusInProgress, Assignee: "eng", Reporter: "pm",
			Watchers: []string{"pm"},
		},
		CreatedAt: at,
	}
}
