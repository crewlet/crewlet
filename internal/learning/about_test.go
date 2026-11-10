package learning_test

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/version"
)

// A NOTE SAYS WHAT IT IS ABOUT AND WHICH BUILD WROTE IT. The class is one of a
// closed set, company where the writer named none; the build is the running
// engine's; both read back with the note. A class outside the set is refused,
// never stored as something else.
//
// Mutation: store an unknown class, leave the build unstamped, or drop either
// from the read, and this goes red.
func TestANoteSaysWhatItIsAboutAndWhichBuildWroteIt(t *testing.T) {
	t.Parallel()
	d := diary(t)
	ctx := context.Background()
	at := time.Unix(1_700_000_000, 0).UTC()
	capability := longEntry("n1", "a1", "the box has no browser", at)
	capability.About = learning.AboutCapability
	if err := d.Write(ctx, capability); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := d.Write(ctx, longEntry("n2", "a1", "deploys go out on thursdays", at.Add(time.Minute))); err != nil {
		t.Fatalf("write: %v", err)
	}
	bad := longEntry("n3", "a1", "a guess", at)
	bad.About = "rumour"
	if err := d.Write(ctx, bad); err == nil {
		t.Fatal("a note about a class outside the closed set was stored")
	}
	got, err := d.Recent(ctx, "a1", at.Add(time.Hour), 10)
	if err != nil || len(got) != 2 {
		t.Fatalf("Recent = %+v (%v), want the two notes kept", got, err)
	}
	want := map[string]learning.About{"n1": learning.AboutCapability, "n2": learning.AboutCompany}
	for _, e := range got {
		if e.About != want[e.ID] || e.EngineBuild != version.String() {
			t.Errorf("note %s reads about %q by build %q, want %q by %q", e.ID, e.About, e.EngineBuild,
				want[e.ID], version.String())
		}
	}
}

// A NOTE IS HELD BACK ONLY WHERE AN UPGRADE OR AN ACCESS CHANGE MAY HAVE MADE IT
// FALSE: a platform note another build wrote, and a capability note written
// before the seat's access last changed. A company or a preference note is
// never held back, and nothing is held back for a fact the turn does not know.
//
// Mutation: hold back by age, hold a company note, or hold a capability note
// written after the change, and a row goes red.
func TestANoteIsHeldBackOnlyWhereItMayHaveBecomeFalse(t *testing.T) {
	t.Parallel()
	changed := time.Unix(1_700_000_000, 0).UTC()
	now := learning.Currency{Build: "v2", AccessChangedAt: changed}
	for _, tc := range []struct {
		name string
		note learning.DiaryEntry
		c    learning.Currency
		held bool
	}{
		{"a platform note of another build", learning.DiaryEntry{About: learning.AboutPlatform, EngineBuild: "v1"}, now, true},
		{"a platform note of this build", learning.DiaryEntry{About: learning.AboutPlatform, EngineBuild: "v2"}, now, false},
		{"a platform note, the build unknown", learning.DiaryEntry{About: learning.AboutPlatform, EngineBuild: "v1"},
			learning.Currency{}, false},
		{"a capability note before the change", learning.DiaryEntry{About: learning.AboutCapability,
			CreatedAt: changed.Add(-time.Hour)}, now, true},
		{"a capability note after the change", learning.DiaryEntry{About: learning.AboutCapability,
			CreatedAt: changed.Add(time.Hour)}, now, false},
		{"a capability note, no change told", learning.DiaryEntry{About: learning.AboutCapability,
			CreatedAt: changed.Add(-time.Hour)}, learning.Currency{Build: "v2"}, false},
		{"a company note of another build", learning.DiaryEntry{About: learning.AboutCompany, EngineBuild: "v1",
			CreatedAt: changed.Add(-time.Hour)}, now, false},
		{"a preference note before the change", learning.DiaryEntry{About: learning.AboutPreference,
			CreatedAt: changed.Add(-time.Hour)}, now, false},
	} {
		if got := tc.note.HeldBack(tc.c); got != tc.held {
			t.Errorf("%s: held back %v, want %v", tc.name, got, tc.held)
		}
	}
}

// THE POST-TURN DECIDER SAYS WHAT A FACT IS ABOUT. Its class is stored with the
// note, company where it names none; a class the prompt does not offer keeps
// nothing, rather than a fact under the wrong class.
//
// Mutation: ignore the class, or store an unknown one as company, and a row
// goes red.
func TestTheDeciderSaysWhatAFactIsAbout(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		answer string
		want   learning.About
		kept   bool
	}{
		{`{"kind":"LONG","content":"no seat has a sandbox","about":"capability"}`, learning.AboutCapability, true},
		{`{"kind":"SHORT","content":"the tracker caps titles","ttl_days":5,"about":"Platform"}`, learning.AboutPlatform, true},
		{`{"kind":"LONG","content":"deploys go out on thursdays"}`, learning.AboutCompany, true},
		{`{"kind":"LONG","content":"a guess","about":"rumour"}`, "", false},
	} {
		store := &fakeDiary{}
		if _, err := decider(t, says(tc.answer), store).Reflect(context.Background(), pdTurn()); err != nil {
			t.Fatalf("Reflect %s: %v", tc.answer, err)
		}
		switch {
		case !tc.kept && len(store.writes) != 0:
			t.Errorf("%s kept %+v", tc.answer, store.writes)
		case tc.kept && (len(store.writes) != 1 || store.writes[0].About != tc.want):
			t.Errorf("%s kept %+v, want one note about %q", tc.answer, store.writes, tc.want)
		}
	}
}

// heldFixture is one seat's notes of every class, build and age around one
// access change, each with the same vector, and the instant of the change.
func heldFixture(t *testing.T, d *learning.Diary) time.Time {
	t.Helper()
	changed := time.Unix(1_700_000_000, 0).UTC()
	for i, n := range []struct {
		about learning.About
		build string
		at    time.Time
	}{
		{learning.AboutPlatform, "v1", changed.Add(time.Hour)},
		{learning.AboutPlatform, "v2", changed.Add(-time.Hour)},
		{learning.AboutCapability, "v2", changed.Add(-time.Hour)},
		{learning.AboutCapability, "v2", changed.Add(time.Hour)},
		{learning.AboutCompany, "v1", changed.Add(-time.Hour)},
		{learning.AboutPreference, "v1", changed.Add(-time.Hour)},
	} {
		e := longEntry(fmt.Sprintf("n%d", i), "a1", "a note", n.at)
		e.About, e.EngineBuild, e.Embedding = n.about, n.build, []float32{1, 0, 0, 0}
		if err := d.Write(context.Background(), e); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	return changed
}

// THE READS LEAVE OUT EXACTLY WHAT HELDBACK HOLDS, in their query, so a held
// note never takes a current one's place under a read's limit: the recency
// read, the similarity read and the count agree with the rule for every
// currency, and a limit of the current notes' number returns all of them.
//
// Mutation: let the SQL predicate drift from HeldBack, or hold after the
// limit, and a row goes red.
func TestTheHeldPredicateIsHeldBack(t *testing.T) {
	t.Parallel()
	d := diary(t)
	ctx := context.Background()
	changed := heldFixture(t, d)
	now := changed.Add(24 * time.Hour)
	all, err := d.Recent(ctx, "a1", now, 50)
	if err != nil || len(all) != 6 {
		t.Fatalf("Recent = %d notes (%v), want all six", len(all), err)
	}
	for _, c := range []learning.Currency{{}, {Build: "v2"}, {AccessChangedAt: changed},
		{Build: "v2", AccessChangedAt: changed}, {Build: "v1", AccessChangedAt: changed.Add(-2 * time.Hour)}} {
		var want []string
		held := 0
		for _, e := range all {
			if e.HeldBack(c) {
				held++
			} else {
				want = append(want, e.ID)
			}
		}
		recent, err := d.RecentHeldTo(ctx, "a1", c, now, len(want))
		if err != nil || !sameIDs(diaryIDs(recent), want) {
			t.Errorf("%+v: RecentHeldTo = %v (%v), want %v", c, diaryIDs(recent), err, want)
		}
		hits, err := d.Recall(ctx, "a1", learning.RecallQuery{Embedding: []float32{1, 0, 0, 0},
			Model: testModel, Limit: len(want), Held: c}, now)
		var recalled []string
		for _, h := range hits {
			recalled = append(recalled, h.Entry.ID)
		}
		if err != nil || !sameIDs(recalled, want) {
			t.Errorf("%+v: Recall = %v (%v), want %v", c, recalled, err, want)
		}
		if n, err := d.Hold(ctx, "a1", c, now); err != nil || n != held {
			t.Errorf("%+v: Hold = %d (%v), want %d", c, n, err, held)
		}
	}
}

// A HELD NOTE IS RETIRED A HELD NOTE'S LIFE AFTER IT WAS FIRST HELD: the first
// read that holds it marks the instant, a later one does not move it, and the
// sweep deletes it once the life has passed; a current note is never retired.
//
// Mutation: retire on the note's age, re-mark on every read, or retire a
// current note, and this goes red.
func TestAHeldNoteIsRetiredAfterItsLife(t *testing.T) {
	t.Parallel()
	d := diary(t)
	ctx := context.Background()
	changed := heldFixture(t, d)
	c := learning.Currency{Build: "v2", AccessChangedAt: changed}
	first := changed.Add(24 * time.Hour)
	if n, err := d.Hold(ctx, "a1", c, first); err != nil || n != 2 {
		t.Fatalf("Hold = %d (%v), want the two held notes", n, err)
	}
	if _, err := d.Hold(ctx, "a1", c, first.Add(20*24*time.Hour)); err != nil {
		t.Fatalf("Hold again: %v", err)
	}
	if n, err := d.RetireHeld(ctx, first.Add(learning.HeldNoteLife-time.Minute)); err != nil || n != 0 {
		t.Fatalf("before the life passed the sweep retired %d (%v)", n, err)
	}
	if n, err := d.RetireHeld(ctx, first.Add(learning.HeldNoteLife)); err != nil || n != 2 {
		t.Fatalf("after the life passed the sweep retired %d (%v), want the two held notes", n, err)
	}
	left, err := d.Recent(ctx, "a1", first.Add(learning.HeldNoteLife), 50)
	if err != nil || len(left) != 4 {
		t.Fatalf("after the sweep the diary holds %v (%v), want the four current notes", diaryIDs(left), err)
	}
	for _, e := range left {
		if e.HeldBack(c) {
			t.Errorf("the sweep left the held note %s", e.ID)
		}
	}
}

func diaryIDs(entries []learning.DiaryEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.ID)
	}
	return out
}

func sameIDs(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}
