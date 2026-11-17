package gitlet

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// A STANDING CURSOR IS ITS PLACE: spelled after a row and cut again it names the row's phase and
// place, for both orders, an attempt not yet ended at 0 among them; a phase the listing does not
// walk, an instant that does not fit the phase, a short id, a non-hex commit, a bad branch and a
// name the listing is not narrowed to are each refused.
//
// Mutation: spell an ended row's phase as its state in the recent order and its cursor names a
// phase that order does not walk; drop the instant's test against the phase and a running cursor
// at an ending is taken.
func TestAStandingCursorIsItsPlace(t *testing.T) {
	t.Parallel()
	const repo = "0190f3c4-0000-7000-8000-0000000000aa"
	oid := strings.Repeat("ab", 20)
	ended := time.Date(2031, 4, 2, 3, 15, 0, 123000, time.UTC)
	failed := CheckRow{CheckKey: CheckKey{RepoID: repo, OID: oid, Target: "refs/heads/release/1.x", Name: "unit",
		Attempt: 3}, State: CheckStateFailed, FinishedAt: ended}
	running := CheckRow{CheckKey: CheckKey{RepoID: repo, OID: oid, Target: "refs/heads/main", Name: "lint",
		Attempt: 1}, State: CheckStateRunning}
	for _, tc := range []struct {
		name  string
		c     CheckQuery
		row   CheckRow
		phase CheckState
		head  string
	}{
		{"recent, an ending", CheckQuery{Standing: StandingRecent}, failed, "", "ended/"},
		{"recent, running", CheckQuery{Standing: StandingRecent}, running, CheckStateRunning, "running/0/"},
		{"failing, an ending", CheckQuery{Standing: StandingFailing}, failed, CheckStateFailed, "failed/"},
		{"failing, running", CheckQuery{Standing: StandingFailing}, running, CheckStateRunning, "running/0/"},
		{"recent narrowed to failed", CheckQuery{Standing: StandingRecent, State: CheckStateFailed}, failed,
			CheckStateFailed, "failed/"},
	} {
		s := standingCursorOf(tc.c)(tc.row)
		if !strings.HasPrefix(s, tc.head) {
			t.Errorf("%s spelled %q, want it to start %q", tc.name, s, tc.head)
		}
		phase, place, ok := cutStandingCursor(tc.c, s)
		at := int64(0)
		if !tc.row.FinishedAt.IsZero() {
			at = store.EncodeTime(tc.row.FinishedAt)
		}
		if !ok || phase != tc.phase || place.at != at || place.key != tc.row.CheckKey {
			t.Errorf("%s cut %q to (%q, %+v, %v), want (%q, %d, %+v)", tc.name, s, phase, place, ok, tc.phase, at,
				tc.row.CheckKey)
		}
	}

	recent, failing := CheckQuery{Standing: StandingRecent}, CheckQuery{Standing: StandingFailing}
	good := standingCursorOf(failing)(failed)
	for name, tc := range map[string]struct {
		c CheckQuery
		s string
	}{
		"a state's phase in the recent order":   {recent, good},
		"the merged phase in the failing order": {failing, standingCursorOf(recent)(failed)},
		"a phase narrowed away":                 {CheckQuery{Standing: StandingFailing, State: CheckStatePassed}, good},
		"a name narrowed away":                  {CheckQuery{Standing: StandingFailing, Name: "lint"}, good},
		"no phase":                              {failing, strings.TrimPrefix(good, "failed")},
		"a negative instant":                    {failing, strings.Replace(good, "failed/", "failed/-", 1)},
		"an ending at 0": {failing, "failed/0/" + repo + "/" + oid + "/" +
			cursorTarget.EncodeToString([]byte("refs/heads/main")) + "/unit/1"},
		"running at an instant": {failing, strings.Replace(standingCursorOf(failing)(running), "running/0/",
			"running/5/", 1)},
		"a short id":       {failing, strings.Replace(good, repo, "0190f3c4", 1)},
		"a non-hex commit": {failing, strings.Replace(good, oid, strings.Repeat("zz", 20), 1)},
		"a bad target":     {failing, strings.Replace(good, cursorTarget.EncodeToString([]byte(failed.Target)), "!!", 1)},
		"no attempt":       {failing, strings.TrimSuffix(good, "/3")},
		"an empty cursor":  {failing, ""},
		"the first page's": {failing, strings.Replace(good, strings.Split(good, "/")[1], "9223372036854775807", 1)},
	} {
		if _, _, ok := cutStandingCursor(tc.c, tc.s); ok {
			t.Errorf("%s (%q) was cut, want it refused", name, tc.s)
		}
	}
}
