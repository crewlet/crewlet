package store_test

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/store"
)

// A WILDCARD IN THE NEEDLE MATCHES ITSELF.
//
// The bug this closes is not an injection — the value is still a bound
// parameter — it is a WRONG ANSWER, which is the harder kind to notice: a
// person filtering a list for "100%" gets every row, and nothing anywhere
// says the filter did not apply.
func TestALikeSearchTreatsWildcardsAsText(t *testing.T) {
	t.Parallel()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "like.db"), store.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx := t.Context()
	if err := db.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`CREATE TABLE crewlet_like_probe (title TEXT NOT NULL)`); err != nil {
			return err
		}
		for _, title := range []string{
			"100% coverage", "ordinary title", "a_b", "axb",
			`back\slash`, "100 coverage",
		} {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO crewlet_like_probe (title) VALUES (?)`, title); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	search := func(needle string) []string {
		t.Helper()
		var out []string
		err := db.Read(ctx, func(tx *sql.Tx) error {
			rows, err := tx.QueryContext(ctx,
				`SELECT title FROM crewlet_like_probe WHERE title LIKE ? ESCAPE '\' ORDER BY title`,
				store.LikeContains(needle))
			if err != nil {
				return err
			}
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var title string
				if err := rows.Scan(&title); err != nil {
					return err
				}
				out = append(out, title)
			}
			return rows.Err()
		})
		if err != nil {
			t.Fatalf("search %q: %v", needle, err)
		}
		return out
	}

	for name, tc := range map[string]struct {
		needle string
		want   []string
	}{
		"a percent sign is a percent sign": {"100%", []string{"100% coverage"}},
		"an underscore is an underscore":   {"a_b", []string{"a_b"}},
		"a backslash is a backslash":       {`back\`, []string{`back\slash`}},
		"ordinary text still matches":      {"ordinary", []string{"ordinary title"}},
		"and matches by containment":       {"coverage", []string{"100 coverage", "100% coverage"}},
	} {
		t.Run(name, func(t *testing.T) {
			got := search(tc.needle)
			if len(got) != len(tc.want) {
				t.Fatalf("search %q = %v, want %v", tc.needle, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("search %q = %v, want %v", tc.needle, got, tc.want)
					break
				}
			}
		})
	}

	// AN EMPTY NEEDLE STILL MATCHES EVERYTHING, which is the caller's
	// business: every caller here refuses an empty filter before building
	// a pattern, and a function that silently matched nothing instead
	// would be a different kind of surprise.
	if got := len(search("")); got != 6 {
		t.Errorf("an empty needle matched %d rows, want all 6", got)
	}
}

// THE PREFIX FORM ESCAPES TOO, and anchors.
func TestALikePrefixIsAnchoredAndEscaped(t *testing.T) {
	t.Parallel()
	if got := store.LikePrefix("100%"); got != `100\%%` {
		t.Errorf("LikePrefix(%q) = %q", "100%", got)
	}
	if got := store.LikeContains(`a\b`); got != `%a\\b%` {
		t.Errorf("LikeContains(%q) = %q", `a\b`, got)
	}
}
