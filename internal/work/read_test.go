package work_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/projection"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/work"
)

func reader(t *testing.T, db *store.DB) *work.Reader {
	t.Helper()
	r, err := work.NewReader(work.ReaderOptions{DB: db})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func keysOf(items []work.Summary) []string {
	out := make([]string, len(items))
	for i, s := range items {
		out[i] = s.Key
	}
	return out
}

// A READ BEFORE THE PROJECTION HAS CAUGHT UP IS REFUSED, never answered
// empty: "this company has no work" is an answer a seat acts on — it files a
// duplicate, it tells a person their link is dead.
func TestAReadBeforeHydrationRaisesRatherThanAnsweringEmpty(t *testing.T) {
	t.Parallel()
	_, db := projected(t)
	r, err := work.NewReader(work.ReaderOptions{
		DB: db, Hydrated: func() bool { return false },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.List(t.Context(), work.Filter{}); !errors.Is(err, projection.ErrNotHydrated) {
		t.Errorf("List gave %v, want ErrNotHydrated", err)
	}
	if _, err := r.Get(t.Context(), "ENG-1"); !errors.Is(err, projection.ErrNotHydrated) {
		t.Errorf("Get gave %v, want ErrNotHydrated", err)
	}
}

// A BOARD IS A LOCAL QUERY, which is the whole reason the projection exists.
func TestTheBoardFiltersTheWayAScreenAsks(t *testing.T) {
	t.Parallel()
	s, db := projected(t)
	mine := file(t, s, human("jane"), work.NewItem{
		Title: "mine", Assignee: "eng", Labels: []string{"backend"},
	})
	theirs := file(t, s, human("jane"), work.NewItem{Title: "theirs", Assignee: "ops"})
	closed := file(t, s, human("jane"), work.NewItem{Title: "done", Assignee: "eng"})
	if _, err := s.Update(t.Context(), human("jane"), closed.Item.ID, 0,
		work.Edit{Status: ptr(work.StatusDone)}); err != nil {
		t.Fatal(err)
	}
	settle(t, func() bool { return rowCount(t, db, `SELECT COUNT(*) FROM work_items`) == 3 },
		"the items never projected")
	settle(t, func() bool {
		return rowCount(t, db, `SELECT COUNT(*) FROM work_items WHERE status = 'done'`) == 1
	}, "the close never projected")

	r := reader(t, db)
	open := true
	for _, tc := range []struct {
		name string
		f    work.Filter
		want []string
	}{
		{"everything", work.Filter{}, []string{closed.Item.Key, theirs.Item.Key, mine.Item.Key}},
		{"a seat's own open work", work.Filter{Assignee: "eng", Open: &open}, []string{mine.Item.Key}},
		{"one project", work.Filter{Project: "ENG"}, []string{closed.Item.Key, theirs.Item.Key, mine.Item.Key}},
		{"a label", work.Filter{Label: "backend"}, []string{mine.Item.Key}},
		{"a status", work.Filter{Status: []work.Status{work.StatusDone}}, []string{closed.Item.Key}},
		{"a text match", work.Filter{Text: "their"}, []string{theirs.Item.Key}},
		{"a key match", work.Filter{Text: mine.Item.Key}, []string{mine.Item.Key}},
		{"nothing matching", work.Filter{Assignee: "nobody"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := r.List(t.Context(), tc.f)
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			gotKeys := keysOf(got)
			slices.Sort(gotKeys)
			want := slices.Clone(tc.want)
			slices.Sort(want)
			if !slices.Equal(gotKeys, want) {
				t.Errorf("listed %v, want %v", gotKeys, want)
			}
		})
	}

	// LABELS COME BACK IN ONE QUERY, not one per item: a fifty-item board
	// would otherwise be fifty-one round trips to a single-writer store.
	got, err := r.List(t.Context(), work.Filter{Label: "backend"})
	if err != nil || len(got) != 1 {
		t.Fatalf("list = %v, %v", got, err)
	}
	if !slices.Equal(got[0].Labels, []string{"backend"}) {
		t.Errorf("labels = %v", got[0].Labels)
	}
}

// A MUTED WATCHER IS OUT OF THEIR OWN LIST, or the unwatch looks like it did
// nothing.
func TestAWatcherListExcludesWhatTheyMuted(t *testing.T) {
	t.Parallel()
	s, db := projected(t)
	watched := file(t, s, human("jane"), work.NewItem{Title: "following", Assignee: "eng"})
	muted := file(t, s, human("jane"), work.NewItem{Title: "not any more", Assignee: "eng"})
	if _, err := s.Update(t.Context(), human("eng"), muted.Item.ID, 0,
		work.Edit{Watch: ptr(false)}); err != nil {
		t.Fatal(err)
	}
	settle(t, func() bool {
		return rowCount(t, db, `SELECT COUNT(*) FROM work_watchers WHERE muted = 1`) == 1
	}, "the mute never projected")

	got, err := reader(t, db).List(t.Context(), work.Filter{Watcher: "eng"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(keysOf(got), []string{watched.Item.Key}) {
		t.Errorf("watching %v, want only the item they still follow", keysOf(got))
	}
}

// OPENING AN ITEM GIVES BOTH DIRECTIONS OF EVERY LINK, so a reader sees
// "blocked by" without a second query and without knowing which end authored
// which.
func TestOpeningAnItemShowsBothEndsOfItsLinks(t *testing.T) {
	t.Parallel()
	s, db := projected(t)
	blocker := file(t, s, human("jane"), work.NewItem{Title: "the blocker"})
	blocked := file(t, s, human("jane"), work.NewItem{Title: "the blocked"})
	if _, err := s.Update(t.Context(), human("jane"), blocker.Item.ID, 0, work.Edit{
		AddLinks: []work.Link{{Kind: work.LinkBlocks, To: blocked.Item.ID}},
	}); err != nil {
		t.Fatal(err)
	}
	settle(t, func() bool { return rowCount(t, db, `SELECT COUNT(*) FROM work_links`) == 2 },
		"the links never projected")

	r := reader(t, db)
	from, err := r.Get(t.Context(), blocker.Item.Key)
	if err != nil {
		t.Fatal(err)
	}
	if len(from.Links) != 1 || from.Links[0].Kind != work.LinkBlocks || from.Links[0].Derived {
		t.Fatalf("the authoring end sees %+v", from.Links)
	}
	if from.Links[0].Key != blocked.Item.Key {
		t.Errorf("the link does not resolve the other item: %+v", from.Links[0])
	}
	to, err := r.Get(t.Context(), blocked.Item.Key)
	if err != nil {
		t.Fatal(err)
	}
	if len(to.Links) != 1 || to.Links[0].Kind != "blocked_by" || !to.Links[0].Derived {
		t.Errorf("the other end sees %+v, want a derived blocked_by", to.Links)
	}
}

// AN ITEM IS FOUND BY EITHER IDENTITY, because a person and a model hold the
// key while every internal reference holds the id, and neither should have to
// translate.
func TestAnItemIsFoundByKeyOrByID(t *testing.T) {
	t.Parallel()
	s, db := projected(t)
	got := file(t, s, human("jane"), work.NewItem{Title: "either way", Assignee: "eng"})
	settle(t, func() bool { return rowCount(t, db, `SELECT COUNT(*) FROM work_items`) == 1 },
		"the item never projected")

	r := reader(t, db)
	// The key is UPPER-CASED on the way in, so a person who typed "eng-1"
	// into a search box or a model that lower-cased it in a tool argument
	// finds the item rather than being told it does not exist.
	for _, id := range []string{got.Item.Key, got.Item.ID, strings.ToLower(got.Item.Key)} {
		detail, err := r.Get(t.Context(), id)
		if err != nil {
			t.Fatalf("Get(%q): %v", id, err)
		}
		if detail.Item.Key != got.Item.Key {
			t.Errorf("Get(%q) found %q", id, detail.Item.Key)
		}
		if detail.Revision != got.Revision {
			t.Errorf("Get(%q) reports revision %d, the write reported %d",
				id, detail.Revision, got.Revision)
		}
	}
	if _, err := r.Get(t.Context(), "ENG-999"); !errors.Is(err, work.ErrNotFound) {
		t.Errorf("a missing item gave %v, want ErrNotFound", err)
	}
}
