package pages_test

import (
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A LISTING RESUMED FROM ITS CURSOR MISSES NOTHING, WHATEVER LEAVES THE PART
// ALREADY READ.
//
// The case an offset gets wrong: a caller reads the first page, a page on it
// is trashed out of the status filter, and the second read is asked to skip
// two rows of a set that now has one fewer in front. The row that moved up
// into the gap is never returned, and nothing on either answer says so. A
// cursor names the last row read rather than counting to it, so the second
// read starts exactly after it.
func TestAListingResumedFromItsCursorMissesNothing(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	written := map[string]string{}
	for _, title := range []string{"Alpha", "Beta", "Delta", "Epsilon", "Gamma"} {
		written[title] = r.write(author("jane"), pages.NewPage{
			Title: title, Body: "prose",
		}).Page.ID
	}
	published := []pages.Status{pages.StatusPublished}

	first := r.list(pages.Filter{Container: "ENG", Status: published, Limit: 2})
	if got := titles(first); len(got) != 2 || got[0] != "Alpha" || got[1] != "Beta" {
		t.Fatalf("the first page is %v, want Alpha and Beta", got)
	}
	if !first.Truncated || first.NextCursor == "" {
		t.Fatalf("a full first page says truncated=%v with cursor %q",
			first.Truncated, first.NextCursor)
	}
	if _, err := r.store.Trash(t.Context(), author("jane"), written["Alpha"]); err != nil {
		t.Fatalf("Trash: %v", err)
	}
	r.drain()

	var rest []string
	for after := first.NextCursor; after != ""; {
		page := r.list(pages.Filter{
			Container: "ENG", Status: published, Limit: 2, After: after,
		})
		rest = append(rest, titles(page)...)
		if page.Truncated != (page.NextCursor != "") {
			t.Fatalf("a page says truncated=%v with cursor %q — the two are "+
				"one fact", page.Truncated, page.NextCursor)
		}
		after = page.NextCursor
	}
	if want := []string{"Delta", "Epsilon", "Gamma"}; strings.Join(rest, ",") !=
		strings.Join(want, ",") {
		t.Errorf("resuming after Beta read %v, want %v — every page that "+
			"matched and did not move, once", rest, want)
	}
}

// THE CURSOR BREAKS A TIE ON THE TITLE BY ID.
//
// A title is its container's address, but that is held in `pages_titles` and
// nothing in `pages_heads` forbids two rows sharing one. A keyset over the
// pair alone would resume "after Runbook" and skip the second Runbook, so the
// id is part of the key. The second row is written straight into the table,
// because every write path here arbitrates a title before it writes one.
func TestTheCursorBreaksATieOnTheTitleByID(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	page := r.write(author("jane"), pages.NewPage{Title: "Runbook", Body: "prose"})
	if err := r.db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `
			INSERT INTO pages_heads
				(id, container, parent_id, title, title_norm, body, status,
				 author, edit_version, created_at, updated_at, trashed_at,
				 version, scoped_through, document)
			SELECT ?, container, parent_id, title, title_norm, body, status,
			       author, edit_version, created_at, updated_at, trashed_at,
			       version, scoped_through, document
			  FROM pages_heads WHERE id = ?`, page.Page.ID+"-twin", page.Page.ID)
		return err
	}); err != nil {
		t.Fatalf("write a second row under the same title: %v", err)
	}

	var seen []string
	after := ""
	for range 3 {
		got := r.list(pages.Filter{Container: "ENG", Limit: 1, After: after})
		for _, p := range got.Pages {
			seen = append(seen, p.ID)
		}
		if after = got.NextCursor; after == "" {
			break
		}
	}
	if len(seen) != 2 || seen[0] == seen[1] {
		t.Errorf("walking one row at a time read %v, want both rows titled "+
			"Runbook once each", seen)
	}
}

// A CURSOR THAT DOES NOT DECODE IS REFUSED NAMING THE FIELD, rather than read
// as some position in the listing: a mangled cursor read as "the start" would
// repeat a page, and one read as past the end would answer an empty listing
// that looks like the end of the set.
func TestACursorThatDoesNotDecodeIsRefused(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	r.write(author("jane"), pages.NewPage{Title: "Runbook", Body: "prose"})
	// A title, too few parts, parts that are not base64, and a well-formed
	// cursor with no id (ENG, Runbook, empty).
	for _, after := range []string{"Runbook", "a.b", "!!.!!.!!", "RU5H.UnVuYm9vaw."} {
		_, err := r.reader.List(t.Context(), pages.Filter{After: after},
			statelog.Freshness{Level: statelog.ReadSession})
		if !errors.Is(err, pages.ErrInvalid) || !strings.Contains(err.Error(), "after") {
			t.Errorf("the cursor %q answered %v, want a refusal naming after",
				after, err)
		}
	}
}
