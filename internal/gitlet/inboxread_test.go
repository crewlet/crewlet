package gitlet_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/inbox"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// codeSource is Gitlet's half of a composed inbox over n's reader,
// asked as ada — the shape internal/api/queries' codeInbox reads it in.
type codeSource struct{ n *node }

func (codeSource) Name() string { return "code" }

func (s codeSource) Notices(ctx context.Context, q inbox.SourceQuery) (inbox.SourcePage, error) {
	answer, err := s.n.reader.Read(ctx, gitlet.ReadQuery{Kind: gitlet.ReadInbox, Asker: ada, Cursor: q.Cursor,
		Limit: q.Limit, Inbox: &gitlet.InboxQuery{Handle: q.Handle, Unread: q.Unread, PrimaryOnly: q.PrimaryOnly,
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
// one on a repository founder still reads. Composed in pages of three,
// Gitlet's first page scans three of the deleted repository's notices and
// answers none of them, its cursor past them; the next page resumes there and
// reaches the fifth notice, and the inbox ends.
//
// Mutation: keep Gitlet's cursor where a composed page took none of its
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
	if want := []string{gitlet.NoticeID("kept-record", kept+"-change")}; !slices.Equal(all, want) {
		t.Errorf("founder's composed inbox reads %v, want the notice on the repository they still read, %v", all,
			want)
	}
}

// CODE NOTICES ARE HELD TO THE REPOSITORY GRANT, AND SO ARE THEIR CARDS:
// founder heard about a change on the company's `web` and one on Sales'
// `visibility: unit` repository; read as founder, the inbox holds both, each
// carrying its change's title and no card, and the cards of both changes are
// answered, each with its description; read by a seat whose grant does not
// reach Sales' repository, only web's notice and web's card — the page still a
// page of the notices it scanned, its cursor where the scan stopped.
//
// Mutations: keep every scanned notice whatever its repository — dev reads the
// Sales change's notice, red; answer every card a row holds whatever its
// repository — dev reads the Sales change's card, red.
func TestCodeNoticesAreHeldToTheRepositoryGrant(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	storedBefore(n, time.Hour)
	c, _ := committerOn(t, n, n.pub)
	opened := map[string]string{}
	for _, r := range []struct {
		name, unit string
		visibility gitlet.Visibility
		by         gitlet.Principal
	}{
		{"web", "Engineering", gitlet.VisibilityCompany, dev},
		{"sales-private", "Sales", gitlet.VisibilityUnit, agent("vp-sales")},
	} {
		s := suiteSettings()
		s.Owner, s.Visibility = gitlet.Owner{Kind: gitlet.OwnerUnit, Ref: r.unit}, r.visibility
		repo := n.ensure(r.name, s, t1).RepoID
		n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set(mainRef, gitlet.ZeroOID, oidOf("A")),
			set(featureRef, gitlet.ZeroOID, oidOf("B"))))
		open := opening(repo, featureRef, mainRef, r.by)
		open.Open.Mentions = []string{"founder"}
		open.Open.Body = "Retry the upload once, not twice."
		res, err := c.ProposeChange(t.Context(), open)
		requireChanged(t, res, err)
		opened[r.name] = res.ChangeID
	}
	read := func(asker gitlet.Principal) []string {
		t.Helper()
		answer, err := n.reader.Read(t.Context(), gitlet.ReadQuery{Kind: gitlet.ReadInbox, Asker: asker,
			Inbox: &gitlet.InboxQuery{Handle: "founder", Snoozed: inbox.SnoozeExclude, Now: harnessAt}})
		if err != nil || answer.Inbox == nil {
			t.Fatalf("read founder's code inbox as %+v: %v", asker, err)
		}
		var out []string
		for _, notice := range answer.Inbox.Notices {
			out = append(out, notice.Repo)
			if notice.Change == nil || notice.Change.Title != "Retry the upload" || notice.Change.Body != "" ||
				notice.ChangeID != opened[notice.Repo] {
				t.Errorf("the notice of %s carries %+v, want its change's title and no card", notice.Repo,
					notice.Change)
			}
		}
		slices.Sort(out)
		return out
	}
	cards := func(asker gitlet.Principal) []string {
		t.Helper()
		answer, err := n.reader.Read(t.Context(), gitlet.ReadQuery{Kind: gitlet.ReadNoticeCards, Asker: asker,
			NoticeCards: []string{opened["web"], opened["sales-private"]}})
		if err != nil {
			t.Fatalf("read the cards of both changes as %+v: %v", asker, err)
		}
		var out []string
		for repo, id := range opened {
			card, found := answer.NoticeCards[id]
			if !found {
				continue
			}
			out = append(out, repo)
			if card == nil || card.Body != "Retry the upload once, not twice." {
				t.Errorf("the card of %s's change carries %+v, want its description", repo, card)
			}
		}
		slices.Sort(out)
		return out
	}
	if got := read(ada); !slices.Equal(got, []string{"sales-private", "web"}) {
		t.Errorf("founder reads notices of %v, want both repositories'", got)
	}
	if got := cards(ada); !slices.Equal(got, []string{"sales-private", "web"}) {
		t.Errorf("founder reads the cards of %v, want both repositories' changes'", got)
	}
	if got := read(dev); !slices.Equal(got, []string{"web"}) {
		t.Errorf("dev reads founder's notices of %v, want web's alone — dev's grant does not reach Sales' "+
			"repository", got)
	}
	if got := cards(dev); !slices.Equal(got, []string{"web"}) {
		t.Errorf("dev reads the cards of %v, want web's change's alone — dev's grant does not reach Sales' "+
			"repository", got)
	}
}

// ONLY A CARD READ JUDGES WHERE A NOTICE'S CHANGE STANDS: founder heard about
// web!1 when dev opened it. The inbox read's notice carries the change's title
// and state and no readiness — the count of what waits and every list row are
// drawn from it, and judge nothing. The change's card carries the readiness
// the change's own read answers ada — its counts and its first sentence —
// until the change is closed, when a change that merges nothing carries none.
//
// Mutations: judge the readiness on the inbox read's notices — the bare
// notice carries one, red; carry no readiness on a card — red; judge every
// change a card read names, the closed one too — red.
func TestOnlyACardReadJudgesWhereANoticesChangeStands(t *testing.T) {
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
	notice := func() *gitlet.NoticeChange {
		t.Helper()
		answer, err := n.reader.Read(t.Context(), gitlet.ReadQuery{Kind: gitlet.ReadInbox, Asker: ada,
			Inbox: &gitlet.InboxQuery{Handle: "founder", Snoozed: inbox.SnoozeExclude, Now: harnessAt}})
		if err != nil || answer.Inbox == nil || len(answer.Inbox.Notices) == 0 {
			t.Fatalf("read founder's code inbox: %+v, %v", answer.Inbox, err)
		}
		bare := answer.Inbox.Notices[0].Change
		if bare == nil || bare.Title != "Retry the upload" || bare.Readiness != nil {
			t.Fatalf("the inbox read's notice carries %+v, want the change's title and no readiness", bare)
		}
		cards, err := n.reader.Read(t.Context(), gitlet.ReadQuery{Kind: gitlet.ReadNoticeCards, Asker: ada,
			NoticeCards: []string{answer.Inbox.Notices[0].ChangeID}})
		if err != nil {
			t.Fatalf("read the card of founder's notice: %v", err)
		}
		return cards.NoticeCards[answer.Inbox.Notices[0].ChangeID]
	}
	detail, err := readChange(t, n, ada, "", "web", opened.Number, 0, "")
	if err != nil || detail.Change == nil || detail.Change.Readiness == nil {
		t.Fatalf("read web!%d as ada: %+v, %v", opened.Number, detail.Change, err)
	}
	judged := detail.Change.Readiness
	want := gitlet.NoticeReadiness{Met: judged.Met, Total: judged.Total, First: judged.First}
	if want.First == "" || want.Total == 0 {
		t.Fatalf("the change's read judges %+v, want a first sentence over its preconditions", want)
	}
	if got := notice(); got == nil || got.Readiness == nil || *got.Readiness != want {
		t.Fatalf("an open change's card carries %+v, want the readiness its change's read judges, %+v", got,
			want)
	}
	closed, err := c.ProposeChange(t.Context(), closing(repo, opened.ChangeID, dev, opened.Version))
	requireChanged(t, closed, err)
	if got := notice(); got == nil || got.State != gitlet.ChangeStateClosed || got.Readiness != nil {
		t.Fatalf("a closed change's card carries %+v, want it closed and with no readiness — it merges "+
			"nothing", got)
	}
}

// A NOTICE_CARDS READ NAMES WHAT A PAGE CAN NAME, EACH CHANGE ONCE: none, more
// than one page of an inbox can name, a change twice, an id in no change's
// spelling, a cursor, a limit and a repository are each refused naming what
// was wrong — the changes named are the read's rows, read across every
// repository — and a read of a change no row holds answers no card.
//
// Mutation: drop the bound on how many changes a read names — a read of one
// more than a page can name is answered, red.
func TestANoticeCardsReadNamesWhatAPageCanNameEachChangeOnce(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	id := uuid.Must(uuid.NewV7()).String()
	many := make([]string, gitlet.MaxNoticeCards+1)
	for i := range many {
		many[i] = uuid.Must(uuid.NewV7()).String()
	}
	for _, tc := range []struct {
		name, field string
		q           gitlet.ReadQuery
	}{
		{"no change", "notice_cards", gitlet.ReadQuery{}},
		{"past a page", "notice_cards", gitlet.ReadQuery{NoticeCards: many}},
		{"a change twice", "notice_cards", gitlet.ReadQuery{NoticeCards: []string{id, id}}},
		{"no change's id", "notice_cards", gitlet.ReadQuery{NoticeCards: []string{"web!1"}}},
		{"a cursor", "cursor", gitlet.ReadQuery{NoticeCards: []string{id}, Cursor: "x"}},
		{"a limit", "limit", gitlet.ReadQuery{NoticeCards: []string{id}, Limit: 1}},
		{"a repository", "repo", gitlet.ReadQuery{NoticeCards: []string{id}, Repo: "web"}},
	} {
		tc.q.Kind, tc.q.Asker = gitlet.ReadNoticeCards, ada
		if _, err := n.reader.Read(t.Context(), tc.q); !errors.Is(err, gitlet.ErrInvalid) ||
			!strings.Contains(err.Error(), tc.field) {
			t.Errorf("a notice_cards read naming %s = %v, want ErrInvalid naming %s", tc.name, err, tc.field)
		}
	}
	answer, err := n.reader.Read(t.Context(), gitlet.ReadQuery{Kind: gitlet.ReadNoticeCards, Asker: ada,
		NoticeCards: many[:gitlet.MaxNoticeCards]})
	if err != nil || len(answer.NoticeCards) != 0 {
		t.Errorf("a notice_cards read of a page's worth of changes no row holds = (%+v, %v), want no card",
			answer.NoticeCards, err)
	}
}
