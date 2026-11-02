package code_test

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/inbox"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// codeSource is the code host's half of a composed inbox over n's reader,
// asked as ada — the shape internal/api/queries' codeInbox reads it in.
type codeSource struct{ n *node }

func (codeSource) Name() string { return "code" }

func (s codeSource) Notices(ctx context.Context, q inbox.SourceQuery) (inbox.SourcePage, error) {
	answer, err := s.n.reader.Read(ctx, code.ReadQuery{Kind: code.ReadInbox, Asker: ada, Cursor: q.Cursor,
		Limit: q.Limit, Inbox: &code.InboxQuery{Handle: q.Handle, Unread: q.Unread, PrimaryOnly: q.PrimaryOnly,
			Snoozed: q.Snoozed, Now: harnessAt}})
	if err != nil {
		return inbox.SourcePage{}, err
	}
	page := inbox.SourcePage{Next: answer.Next, Level: answer.Level, Complete: true}
	for _, n := range answer.Inbox.Notices {
		page.Notices = append(page.Notices, inbox.Notice{RecordID: n.RecordID, At: n.At,
			Position: inbox.Position{Stream: n.LogStream, Generation: n.LogGeneration, Seq: n.LogSeq}})
	}
	return page, nil
}

// A DELETED REPOSITORY'S NOTICES HOLD NO PAGE OF THE INBOX BACK: founder's
// four newest code notices are about a change on a repository deleted and not
// yet purged — which keeps its notices until its purge — and the fifth about
// one on a repository founder still reads. Composed in pages of three, the
// code host's first page scans three of the deleted repository's notices and
// answers none of them, its cursor past them; the next page resumes there and
// reaches the fifth notice, and the inbox ends.
//
// Mutation: keep the code host's cursor where a composed page took none of its
// notices (internal/inbox's Compose) — every page rescans the same three and
// the fifth notice is never reached, red.
func TestADeletedRepositorysNoticesHoldNoPageOfTheInboxBack(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	n.ensure("gone", described("one"), t1)
	n.ensure("kept", described("two"), t1)
	gone, kept := n.mustID("gone"), n.mustID("kept")
	n.deleteAt(gone, harnessAt.Add(-time.Hour))
	notice := func(tx *sql.Tx, record, repo string, seq uint64) error {
		at := statelog.Position{Stream: topics.CodeLogStream, Generation: 1, Seq: seq}
		_, err := tx.ExecContext(t.Context(), `INSERT INTO code_notifications (record_id, recipient,
			recipient_kind, repo_id, change_id, change_ref, reason, addressed, fallback_only, fallback_rank, excerpt,
			created_at, log_seq, log_stream, log_generation)
			VALUES (?, 'founder', 'person', ?, ?, ?, 'mention', 1, 0, 0, '', ?, ?, ?, 1)`,
			record, repo, repo+"-change", repo+"!1", store.EncodeTime(harnessAt), at.Packed(), at.Stream)
		return err
	}
	if err := n.db.Tx(t.Context(), func(tx *sql.Tx) error {
		for i := range 4 {
			if err := notice(tx, fmt.Sprintf("gone-record-%d", i), gone, uint64(9001+i)); err != nil {
				return err
			}
		}
		return notice(tx, "kept-record", kept, 9000)
	}); err != nil {
		t.Fatalf("seed founder's notices: %v", err)
	}
	var all []string
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > 4 {
			t.Fatalf("founder's inbox did not end after %d pages: read %v", pages, all)
		}
		page, err := inbox.Compose(t.Context(), inbox.Query{Handle: "founder", Snoozed: inbox.SnoozeExclude,
			Limit: 3, Cursor: cursor}, []inbox.Source{codeSource{n: n}})
		if err != nil {
			t.Fatalf("Compose at %q: %v", cursor, err)
		}
		for _, notice := range page.Notices {
			all = append(all, notice.RecordID)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if want := []string{code.NoticeID("kept-record", kept+"-change")}; !slices.Equal(all, want) {
		t.Errorf("founder's composed inbox reads %v, want the notice on the repository they still read, %v", all,
			want)
	}
}

// CODE NOTICES ARE HELD TO THE REPOSITORY GRANT: founder heard about a change
// on the company's `web` and one on Sales' `visibility: unit` repository; read
// as founder, the inbox holds both, each carrying what its change says; read
// by a seat whose grant does not reach Sales' repository, only web's — the
// page still a page of the notices it scanned, its cursor where the scan
// stopped.
//
// Mutation: keep every scanned notice whatever its repository — dev reads the
// Sales change's notice, red.
func TestCodeNoticesAreHeldToTheRepositoryGrant(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	storedBefore(n, time.Hour)
	c, _ := committerOn(t, n, n.pub)
	opened := map[string]string{}
	for _, r := range []struct {
		name, unit string
		visibility code.Visibility
		by         code.Principal
	}{
		{"web", "Engineering", code.VisibilityCompany, dev},
		{"sales-private", "Sales", code.VisibilityUnit, agent("vp-sales")},
	} {
		s := suiteSettings()
		s.Owner, s.Visibility = code.Owner{Kind: code.OwnerUnit, Ref: r.unit}, r.visibility
		repo := n.ensure(r.name, s, t1).RepoID
		n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set(mainRef, code.ZeroOID, oidOf("A")),
			set(featureRef, code.ZeroOID, oidOf("B"))))
		open := opening(repo, featureRef, mainRef, r.by)
		open.Open.Mentions = []string{"founder"}
		open.Open.Body = "Retry the upload once, not twice."
		res, err := c.ProposeChange(t.Context(), open)
		requireChanged(t, res, err)
		opened[r.name] = res.ChangeID
	}
	read := func(asker code.Principal) []string {
		t.Helper()
		answer, err := n.reader.Read(t.Context(), code.ReadQuery{Kind: code.ReadInbox, Asker: asker,
			Inbox: &code.InboxQuery{Handle: "founder", Snoozed: inbox.SnoozeExclude, Now: harnessAt}})
		if err != nil || answer.Inbox == nil {
			t.Fatalf("read founder's code inbox as %+v: %v", asker, err)
		}
		var out []string
		for _, notice := range answer.Inbox.Notices {
			out = append(out, notice.Repo)
			if notice.Change == nil || notice.Change.Body != "Retry the upload once, not twice." ||
				notice.ChangeID != opened[notice.Repo] {
				t.Errorf("the notice of %s carries %+v, want its change and its description", notice.Repo,
					notice.Change)
			}
		}
		slices.Sort(out)
		return out
	}
	if got := read(ada); !slices.Equal(got, []string{"sales-private", "web"}) {
		t.Errorf("founder reads notices of %v, want both repositories'", got)
	}
	if got := read(dev); !slices.Equal(got, []string{"web"}) {
		t.Errorf("dev reads founder's notices of %v, want web's alone — dev's grant does not reach Sales' "+
			"repository", got)
	}
}

// A CODE NOTICE CARRIES WHERE ITS CHANGE'S MERGE STANDS, judged at the read
// as the change's own read judges it: founder heard about web!1 when dev
// opened it, and the notice carries the readiness the change's read answers
// ada — its counts and its first sentence — until the change is closed, when
// a change that merges nothing carries none.
//
// Mutations: carry no readiness on a notice — red; judge every change the
// page names, the closed one too — red.
func TestACodeNoticeCarriesItsChangesReadinessLine(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	storedBefore(n, time.Hour)
	c, _ := committerOn(t, n, n.pub)
	repo := changeRepo(t, n, "web")
	open := opening(repo, featureRef, mainRef, dev)
	open.Open.Mentions = []string{"founder"}
	opened, err := c.ProposeChange(t.Context(), open)
	requireChanged(t, opened, err)
	notice := func() *code.NoticeChange {
		t.Helper()
		answer, err := n.reader.Read(t.Context(), code.ReadQuery{Kind: code.ReadInbox, Asker: ada,
			Inbox: &code.InboxQuery{Handle: "founder", Snoozed: inbox.SnoozeExclude, Now: harnessAt}})
		if err != nil || answer.Inbox == nil || len(answer.Inbox.Notices) == 0 {
			t.Fatalf("read founder's code inbox: %+v, %v", answer.Inbox, err)
		}
		return answer.Inbox.Notices[0].Change
	}
	detail, err := readChange(t, n, ada, "", "web", opened.Number, 0, "")
	if err != nil || detail.Change == nil || detail.Change.Readiness == nil {
		t.Fatalf("read web!%d as ada: %+v, %v", opened.Number, detail.Change, err)
	}
	judged := detail.Change.Readiness
	want := code.NoticeReadiness{Met: judged.Met, Total: judged.Total, First: judged.First}
	if want.First == "" || want.Total == 0 {
		t.Fatalf("the change's read judges %+v, want a first sentence over its preconditions", want)
	}
	if got := notice(); got == nil || got.Readiness == nil || *got.Readiness != want {
		t.Fatalf("an open change's notice carries %+v, want the readiness its change's read judges, %+v", got,
			want)
	}
	closed, err := c.ProposeChange(t.Context(), closing(repo, opened.ChangeID, dev, opened.Version))
	requireChanged(t, closed, err)
	if got := notice(); got == nil || got.State != code.ChangeStateClosed || got.Readiness != nil {
		t.Fatalf("a closed change's notice carries %+v, want it closed and with no readiness — it merges "+
			"nothing", got)
	}
}
