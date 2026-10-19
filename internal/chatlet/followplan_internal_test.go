package chatlet

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// THE FOLLOW DERIVATION SEEKS BY KEY, and runs on every edit, delete and erase.
//
// Each name an edit drops asks the thread whether anything still names it and,
// if nothing does, removes one follow. Both are written for a seek, and both
// have a plan this planner prefers that is not one: the reply probe joined
// plainly drives on `chat_mentions_handle_idx` — every mention the handle has
// ever had in the company, to answer a question about one thread — and a
// delete with a term on the unindexed `reason` column leaves the primary key
// for `chat_follows_prune_idx`, every follow in the room. Neither is a
// failure; each is a transaction holding the estate's one writer for longer
// the older the company is.
//
// NEVER ANALYZEd, because no node is: a plan taken over statistics is a plan
// no deployment runs.
func TestTheFollowDerivationSeeksByKey(t *testing.T) {
	t.Parallel()
	node, db := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"),
		store.Options{}, 1)
	t.Cleanup(func() {
		if err := node.Close(); err != nil {
			t.Errorf("close the store: %v", err)
		}
	})

	for _, c := range []struct {
		name, statement string
		// want is every step the plan must take, in order: a table and
		// the index it is reached through.
		want []string
	}{
		{"the root's own mention", rootNamesStatement, []string{
			"SEARCH chat_mentions USING COVERING INDEX sqlite_autoindex_chat_mentions_1 (message_id=? AND handle=?)",
		}},
		{"the thread's replies", repliesNameStatement, []string{
			"SEARCH m USING INDEX chat_messages_thread_idx (channel_id=? AND thread_root=?)",
			"SEARCH n USING COVERING INDEX sqlite_autoindex_chat_mentions_1 (message_id=? AND handle=?)",
		}},
		{"the follow taken back", unfollowStatement, []string{
			"SEARCH chat_follows USING COVERING INDEX sqlite_autoindex_chat_follows_1 (channel_id=? AND thread_root=? AND handle=?)",
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var plan []string
			// A WRITE TRANSACTION, because one of the three is a delete
			// and a read transaction refuses to plan it.
			if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
				args := make([]any, strings.Count(c.statement, "?"))
				for i := range args {
					args[i] = "x"
				}
				rows, err := tx.QueryContext(t.Context(),
					"EXPLAIN QUERY PLAN "+c.statement, args...)
				if err != nil {
					return err
				}
				defer func() { _ = rows.Close() }()
				for rows.Next() {
					var id, parent, notUsed int
					var detail string
					if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
						return err
					}
					plan = append(plan, detail)
				}
				return rows.Err()
			}); err != nil {
				t.Fatalf("explain: %v", err)
			}
			if strings.Join(plan, "\n") != strings.Join(c.want, "\n") {
				t.Errorf("the plan is\n\t%s\nwant\n\t%s",
					strings.Join(plan, "\n\t"), strings.Join(c.want, "\n\t"))
			}
		})
	}
}
