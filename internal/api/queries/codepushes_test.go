package queries_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/gitlet"
)

// A PUSH HISTORY IS NARROWED BY ITS PARAMETERS: code_pushes asks a PushFilter of exactly `pusher_kind`, `pusher`,
// `since` and `until`, instants in UTC; none asks none; a bad instant is bad_params; another question reads none.
// Mutation: read `since` into Until, or build the filter on every kind.
func TestAPushHistoryIsNarrowedByItsParameters(t *testing.T) {
	t.Parallel()
	stub := &stubCode{}
	if _, err := askNative(t, queries.Sources{Code: stub}, "code_pushes", map[string]any{"name": "web",
		"pusher_kind": " agent ", "pusher": "dev", "since": "2031-04-02T10:00:00+01:00",
		"until": "2031-04-02T10:00:00.000001Z"}); err != nil {
		t.Fatal(err)
	}
	f := stub.last().PushFilter
	since, until := time.Date(2031, 4, 2, 9, 0, 0, 0, time.UTC), time.Date(2031, 4, 2, 10, 0, 0, 1000, time.UTC)
	if f == nil || f.PusherKind != gitlet.PusherAgent || f.Pusher != "dev" || !f.Since.Equal(since) ||
		f.Since.Location() != time.UTC || !f.Until.Equal(until) {
		t.Errorf("the narrowed history asked %+v, want dev's pushes from %s until %s in UTC", f, since, until)
	}
	none := &stubCode{}
	if _, err := askNative(t, queries.Sources{Code: none}, "code_pushes", map[string]any{"name": "web"}); err != nil {
		t.Fatal(err)
	}
	if f := none.last().PushFilter; f != nil {
		t.Errorf("a history naming no narrowing asked %+v", f)
	}
	refused := &stubCode{}
	_, err := askNative(t, queries.Sources{Code: refused}, "code_pushes", map[string]any{"name": "web",
		"since": "yesterday"})
	if !errors.Is(err, queries.ErrBadParams) || !strings.Contains(err.Error(), "since") || len(refused.asked) != 0 {
		t.Errorf("since=yesterday answered %v having read %d, want bad_params naming since", err, len(refused.asked))
	}
	repos := &stubCode{}
	if _, err = askNative(t, queries.Sources{Code: repos}, "code_repos", map[string]any{
		"since": "2031-04-02T10:00:00Z", "pusher_kind": "agent"}); err != nil {
		t.Fatal(err)
	}
	if f := repos.last().PushFilter; f != nil {
		t.Errorf("a listing of repositories asked the narrowing %+v", f)
	}
}
