package queries_test

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/gitlet"
)

// A DAY BOUNDS QUESTION ASKS THE ASKER'S DAYS: `day_bounds`, comma-separated or a list off a socket
// frame, reaches the reader as instants on a listing, a detail and a push history; one that is no
// RFC 3339 instant, or no list, is bad_params in words with nothing read. Mutation: read another key.
func TestADayBoundsQuestionAsksTheAskersDays(t *testing.T) {
	t.Parallel()
	a, b := "2031-04-02T18:15:00Z", "2031-04-03T18:15:00+00:00"
	want := []time.Time{time.Date(2031, 4, 2, 18, 15, 0, 0, time.UTC), time.Date(2031, 4, 3, 18, 15, 0, 0, time.UTC)}
	for _, q := range []struct {
		what   string
		params map[string]any
		kind   gitlet.ReadKind
	}{
		{"code_repos", map[string]any{"repo_facts": "true", "day_bounds": a + ", " + b}, gitlet.ReadRepos},
		{"code_repos", map[string]any{"repo_facts": "true", "day_bounds": []any{a, b}}, gitlet.ReadRepos},
		{"code_repo", map[string]any{"name": "web", "repo_facts": "true", "day_bounds": a + "," + b}, gitlet.ReadRepo},
		{"code_pushes", map[string]any{"name": "web", "day_bounds": a + "," + b}, gitlet.ReadPushes},
	} {
		stub := &stubCode{}
		if _, err := askNative(t, queries.Sources{Code: stub}, q.what, q.params); err != nil {
			t.Fatalf("%s with day bounds: %v", q.what, err)
		}
		asked := stub.last()
		if asked.Kind != q.kind || !slices.EqualFunc(asked.DayBounds, want, time.Time.Equal) {
			t.Errorf("%s with day bounds %v asked %+v, want a %s read of %v", q.what, q.params["day_bounds"],
				asked.DayBounds, q.kind, want)
		}
	}
	stub := &stubCode{}
	if _, err := askNative(t, queries.Sources{Code: stub}, "code_pushes", map[string]any{"name": "web"}); err != nil {
		t.Fatal(err)
	}
	if asked := stub.last(); asked.DayBounds != nil {
		t.Errorf("a history naming no days asked %v", asked.DayBounds)
	}
	for _, bounds := range []any{"yesterday", a + ",tomorrow", 7} {
		none := &stubCode{}
		_, err := askNative(t, queries.Sources{Code: none}, "code_pushes", map[string]any{"name": "web",
			"day_bounds": bounds})
		if !errors.Is(err, queries.ErrBadParams) || len(none.asked) != 0 || strings.Contains(err.Error(), "parsing time") {
			t.Errorf("day_bounds %v answered %v having read %d, want bad_params in words", bounds, err, len(none.asked))
		}
	}
}
