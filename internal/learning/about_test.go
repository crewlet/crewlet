package learning_test

import (
	"context"
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
