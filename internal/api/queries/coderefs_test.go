package queries_test

import (
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/gitlet"
)

// A REFS LISTING ASKS ITS ORDER, WINDOW, FIND AND DETAIL in one refs read, none
// where the caller asked today's listing, and refuses a bad instant before the reader.
// Mutation: drop a parameter, or send an empty or name-only listing query.
func TestARefsListingAsksItsOrderWindowFindAndDetail(t *testing.T) {
	t.Parallel()
	stub := &stubCode{}
	if _, err := askNative(t, queries.Sources{Code: stub}, "code_refs", map[string]any{"name": "web",
		"order": "moved", "moved_since": "2026-11-01T00:00:00Z", "moved_before": " 2026-11-08T00:00:00.5Z ",
		"contains": "FiX", "detail": "true", "active_since": "2026-11-14T00:00:00+01:00"}); err != nil {
		t.Fatal(err)
	}
	got := stub.last()
	want := gitlet.RefListingQuery{Order: gitlet.RefOrderMoved,
		MovedSince:  time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
		MovedBefore: time.Date(2026, 11, 8, 0, 0, 0, 500_000_000, time.UTC), Contains: "FiX", Detail: true,
		ActiveSince: time.Date(2026, 11, 13, 23, 0, 0, 0, time.UTC)}
	if got.Kind != gitlet.ReadRefs || got.RefListing == nil || got.RefListing.Order != want.Order ||
		!got.RefListing.MovedSince.Equal(want.MovedSince) || !got.RefListing.MovedBefore.Equal(want.MovedBefore) ||
		got.RefListing.Contains != want.Contains || !got.RefListing.Detail ||
		!got.RefListing.ActiveSince.Equal(want.ActiveSince) {
		t.Fatalf("code_refs asked %s with %+v, want refs with %+v", got.Kind, got.RefListing, want)
	}

	for _, today := range []map[string]any{{"name": "web", "detail": "false"}, {"name": "web", "order": "name"}} {
		stub = &stubCode{}
		if _, err := askNative(t, queries.Sources{Code: stub}, "code_refs", today); err != nil {
			t.Fatal(err)
		}
		if got := stub.last(); got.RefListing != nil {
			t.Fatalf("code_refs asking today's listing (%v) asked %+v, want no ref_listing at all", today,
				got.RefListing)
		}
	}

	for key, bad := range map[string]string{"moved_since": "yesterday", "moved_before": "2026-11-08",
		"active_since": "soon", "detail": "yes"} {
		stub := &stubCode{}
		_, err := askNative(t, queries.Sources{Code: stub}, "code_refs", map[string]any{"name": "web", key: bad})
		if !errors.Is(err, queries.ErrBadParams) || len(stub.asked) != 0 {
			t.Errorf("%s=%s: answered %v having asked the reader %d times, want bad_params before it", key, bad,
				err, len(stub.asked))
		}
	}
}
