package chatlet

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// THE FOLLOW DERIVATION SEEKS BY KEY, and runs on every edit, delete and erase.
//
// Each dropped name asks the thread whether anything still names it, and if not
// removes one follow. The planner prefers a non-seek for each: the plainly
// joined reply probe drives on `chat_mentions_handle_idx` (every mention the
// handle ever had), and a delete with a term on unindexed `reason` leaves the
// primary key for `chat_follows_prune_idx` (every follow in the room), holding
// the estate's one writer longer as the company ages. NEVER ANALYZEd, because
// no node is; plans are read through storetest ([storetest.Decorrelated]).
//
// Mutation: join the replies plainly rather than CROSS JOIN and the plan
// drives on chat_mentions_handle_idx.
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
			args := make([]any, strings.Count(c.statement, "?"))
			for i := range args {
				args[i] = "x"
			}
			// THE DELETE IS PLANNED IN A WRITE TRANSACTION, by Plan
			// itself: a read transaction refuses to plan it.
			plan := storetest.Plan(t, db, c.statement, args)
			if line, ok := storetest.Decorrelated(plan); ok {
				t.Errorf("the planner unnested a correlated aggregate into a "+
					"read of the whole table: %s", line)
			}
			if strings.Join(plan, "\n") != strings.Join(c.want, "\n") {
				t.Errorf("the plan is\n\t%s\nwant\n\t%s",
					strings.Join(plan, "\n\t"), strings.Join(c.want, "\n\t"))
			}
		})
	}
}
