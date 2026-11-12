package gitlet

import (
	"database/sql"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/inbox"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// AN INBOX CARD READS NO SUPERSEDED FINDING: web!1 carries forty agent
// findings its raisers' later reviews superseded, three dismissed, a person's
// open one and two agents' open ones — a blocker and a nit — and its card
// counts the two, by one COVERING seek of code_findings_open_agent_idx a
// change, which is declared partial on exactly the open agent findings
// (openAgentFindings, spelled of FindingOpen and PartyAgent): no finding of
// an earlier revision is an entry of it, and no row is fetched to drop one.
//
// Mutations: read the findings through code_findings_raiser_idx again — the
// plan seeks every finding of the change and fetches each row, red; declare
// the index without its predicate (replicated 0137) — every superseded
// finding is an entry of it, red.
func TestAnInboxCardReadsNoSupersededFinding(t *testing.T) {
	t.Parallel()
	if want := fmt.Sprintf("state = '%s' AND raiser_kind = '%s'", FindingOpen, PartyAgent); openAgentFindings !=
		want {
		t.Fatalf("openAgentFindings is %q, and the open agent findings spell %q", openAgentFindings, want)
	}
	node, db := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"), store.Options{}, 1)
	t.Cleanup(func() { _ = node.Close() })
	var declared string
	if err := db.Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(), `SELECT sql FROM sqlite_master WHERE type = 'index' AND name = ?`,
			openAgentFindingsKey).Scan(&declared)
	}); err != nil {
		t.Fatalf("read %s's declaration: %v", openAgentFindingsKey, err)
	}
	if got := strings.Join(strings.Fields(declared), " "); !strings.HasSuffix(got, " WHERE "+openAgentFindings) {
		t.Errorf("%s is declared\n\t%s\nwant it partial on\n\t%s\n— the open agent findings alone, spelled as "+
			"its reader spells them", openAgentFindingsKey, got, openAgentFindings)
	}
	type finding struct {
		state    FindingState
		kind     PartyKind
		severity string
	}
	var findings []finding
	for range 40 {
		findings = append(findings, finding{FindingSuperseded, PartyAgent, "blocker"})
	}
	for range 3 {
		findings = append(findings, finding{FindingDismissed, PartyAgent, "major"})
	}
	findings = append(findings, finding{FindingOpen, PartyPerson, "major"}, finding{FindingOpen, PartyAgent,
		"blocker"}, finding{FindingOpen, PartyAgent, "nit"})
	if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
		for i, f := range findings {
			if _, err := tx.ExecContext(t.Context(), `INSERT INTO code_findings (id, change_id, review_id, raiser,
				raiser_kind, revision, commit_oid, path, blob_oid, side, line_start, line_end, severity, category,
				state, position) VALUES (?, 'change-1', 'review-1', 'seat-1', ?, ?, '', 'a.go', '', 'new', 1, 1, ?,
				'bug', ?, ?)`, fmt.Sprintf("finding-%03d", i), string(f.kind), 1+i/16, f.severity, string(f.state),
				i+1); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed web!1's findings: %v", err)
	}
	plan := storetest.Plan(t, db, noticeFindingsSQL, []any{`["change-1"]`})
	if !slices.ContainsFunc(plan, func(line string) bool {
		return strings.Contains(line, "COVERING INDEX "+openAgentFindingsKey+" (change_id=?)")
	}) {
		t.Errorf("the card's findings are not one covering seek of %s:\n%s", openAgentFindingsKey,
			strings.Join(plan, "\n"))
	}
	counted := map[string]int{}
	if err := db.Read(t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(t.Context(), noticeFindingsSQL, `["change-1"]`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var change, severity string
			if err := rows.Scan(&change, &severity); err != nil {
				return err
			}
			counted[severity]++
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("count web!1's findings: %v", err)
	}
	if want := map[string]int{"blocker": 1, "nit": 1}; !maps.Equal(counted, want) {
		t.Errorf("web!1's card counts %v, want the two open agent findings %v", counted, want)
	}
}

// AN ALL-READ INBOX SCANS NO ROW BELOW THE SEEN-THROUGH POSITION: founder has
// read through code@1:9000 and marked nothing unread, so an `unread` read's
// range starts at that position and seeks no notice below it; an unread mark
// below it is the one notice sought there, by its key. A `since` above the
// position bounds the range by itself; a read that is not `unread` keeps every
// notice, read or not, so its range starts at `since` alone; and a position
// on another stream covers no code notice, so it bounds nothing.
//
// Mutation: start the range at `since` whatever the read asks — the all-read
// inbox walks every notice founder holds, red.
func TestAnAllReadInboxScansNoRowBelowTheSeenThroughPosition(t *testing.T) {
	t.Parallel()
	seen := inbox.Position{Stream: topics.CodeLogStream, Generation: 1, Seq: 9000}
	since := statelog.Position{Stream: topics.CodeLogStream, Generation: 1, Seq: 9500}
	elsewhere := inbox.Position{Stream: topics.TrackerLogStream, Generation: 1, Seq: 9000}
	unreadAgain := inbox.Entry{RecordID: NoticeID("record-1", "change-1"), Position: 4000}
	for name, tc := range map[string]struct {
		seen   inbox.Position
		unread []inbox.Entry
		q      InboxQuery
		floor  int64
		marked string
	}{
		"an all-read inbox": {seen: seen, q: InboxQuery{Unread: true}, floor: int64(seen.Packed()),
			marked: "[]"},
		"an unread mark below the position": {seen: seen, unread: []inbox.Entry{unreadAgain},
			q: InboxQuery{Unread: true}, floor: int64(seen.Packed()), marked: `[["record-1","change-1"]]`},
		"since above the position": {seen: seen, q: InboxQuery{Unread: true, Since: since},
			floor: since.Packed(), marked: "[]"},
		"every notice": {seen: seen, q: InboxQuery{}, floor: 0, marked: "[]"},
		"a position on another stream": {seen: elsewhere, q: InboxQuery{Unread: true}, floor: 0,
			marked: "[]"},
	} {
		marks := inbox.Marks[Reason]{SeenThrough: tc.seen, Unread: tc.unread}
		tc.q.Handle, tc.q.Snoozed = "founder", inbox.SnoozeInclude
		scan, selects, err := inboxScanOf(ReadQuery{Kind: ReadInbox, Inbox: &tc.q},
			marks.Classifier(DefaultPrimaryReasons), time.Now())
		if err != nil || !selects {
			t.Fatalf("%s: inboxScanOf = %v, %v", name, selects, err)
		}
		if got := scan.floor(); got != tc.floor {
			t.Errorf("%s: the range starts above %d, want %d", name, got, tc.floor)
		}
		if scan.marked != tc.marked {
			t.Errorf("%s: the scan seeks the unread marks %s, want %s", name, scan.marked, tc.marked)
		}
	}
}
