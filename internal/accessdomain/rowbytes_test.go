package accessdomain_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/accessdomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// AN AUDIT PAGE ENDS AT ITS BYTES AND ITS CURSOR RESUMES AFTER THE LAST ROW
// KEPT: five commands under a bound two of their rows fit come back in pages of
// two, two and one, every command once and newest first, as one unbounded page
// orders them.
//
// Mutations, each red: drop the Take check in Commands (one page of five); mint
// the cut page's cursor from the refused row (a command skipped per page);
// leave a cut page without a cursor (paging stops after two).
func TestACommandPageEndsAtItsBytesAndResumesAfterItsLastRow(t *testing.T) {
	t.Parallel()
	node := newWriteNode(t, openAccessLog(t), "data-1")
	for i := range 5 {
		ran := command("sre", uuid.NewString(), "/usr/bin/journalctl -u worker-"+strings.Repeat("x", 400)+string(rune('a'+i)))
		if _, err := node.writer.Ran(t.Context(), engineCall(), ran); err != nil {
			t.Fatal(err)
		}
	}
	fresh := statelog.Freshness{Level: statelog.ReadStale}
	whole, err := node.reader().Commands(t.Context(), accessdomain.AuditQuery{Handle: "sre"}, fresh)
	if err != nil || len(whole.Rows) != 5 || whole.Next != "" {
		t.Fatalf("the unbounded page answers %d rows and cursor %q, %v; want all five", len(whole.Rows),
			whole.Next, err)
	}
	heaviest := 0
	for _, row := range whole.Rows {
		raw, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		heaviest = max(heaviest, len(raw))
	}
	ctx := statelog.WithAnswerBytes(t.Context(), 2*heaviest+heaviest/2)
	var got []string
	var sizes []int
	q := accessdomain.AuditQuery{Handle: "sre"}
	for range 10 {
		page, err := node.reader().Commands(ctx, q, fresh)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range page.Rows {
			got = append(got, row.ID)
		}
		sizes = append(sizes, len(page.Rows))
		if q.Cursor = page.Next; q.Cursor == "" {
			break
		}
	}
	var want []string
	for _, row := range whole.Rows {
		want = append(want, row.ID)
	}
	if !slices.Equal(got, want) || !slices.Equal(sizes, []int{2, 2, 1}) {
		t.Errorf("paged %v in pages of %v, want %v in pages of [2 2 1]", got, sizes, want)
	}
}
