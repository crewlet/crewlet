package code_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/routing"
)

// WHOM A CHANGE'S RECORDS REACHED (routedread.go) AND WHAT ONE REVIEWER GAVE
// (conversationread.go's reviewsGiven): the two reads the API's change
// questions add beside a change's own.

// routedOf is one page of whom change number of repo reached, read as asker.
func routedOf(t *testing.T, n *node, repo string, number int64, limit int, cursor string) (*code.RoutedPage,
	string) {

	t.Helper()
	answer, err := n.reader.Read(t.Context(), code.ReadQuery{Asker: asker, Kind: code.ReadChangeRouting, Repo: repo,
		Change: number, Limit: limit, Cursor: cursor})
	if err != nil || answer.Routed == nil {
		t.Fatalf("Read(change_routing %s!%d after %q) = (%+v, %v), want a page", repo, number, cursor,
			answer.Routed, err)
	}
	return answer.Routed, answer.Next
}

// recipientsOf is every recipient of r, as "handle:reason:kind", in the order
// the page gives them.
func recipientsOf(r code.RoutedRecord) []string {
	var out []string
	for _, c := range r.Recipients {
		out = append(out, c.Handle+":"+string(c.Reason)+":"+string(c.Kind))
	}
	return out
}

// THE WOKE ANSWER TELLS SWEPT FROM NOBODY. A change whose records list nobody
// is one of two facts with two remedies — its routings resolved to nobody, or
// its notices were swept past their horizon — and the answer tells them apart
// by the change's opening against the instant from which every notice is
// kept: a change opened ten days ago that concerned nobody but its opener
// reached NOBODY, every notice it could have written still kept; one opened
// forty days ago with no notice listed is SWEPT, a seat's notice of it gone
// with the turn detail it named. And a forty-day-old record that mentioned a
// person and a seat lists the person alone, older than the page's horizon —
// what that horizon tells a reader of the record.
//
// Mutation: date the answer by a person's horizon (the company's year) rather
// than a seat's thirty days → the forty-day change reached "nobody", red; drop
// the horizon from the evidence → both empty answers are "unknown", red.
func TestTheWokeAnswerTellsSweptFromNobody(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := committerOn(t, n, n.pub)
	web := changeRepo(t, n, "web")
	api := changeRepo(t, n, "api")

	open := func(p code.ChangeProposal) {
		t.Helper()
		res, err := c.ProposeChange(t.Context(), p)
		requireChanged(t, res, err)
	}
	storedBefore(n, 10*24*time.Hour)
	open(opening(web, featureRef, mainRef, dev))
	storedBefore(n, 40*24*time.Hour)
	open(opening(web, releaseRef, mainRef, dev))
	mentioned := opening(api, featureRef, mainRef, dev)
	mentioned.Open.Mentions = []string{"founder", "cto"}
	open(mentioned)

	horizon := harnessAt.Add(-code.SeatNoticeHorizon)
	for _, tc := range []struct {
		repo   string
		number int64
		want   routing.Delivery
		lists  [][]string
	}{
		{"web", 1, routing.DeliveryNobody, nil},
		{"web", 2, routing.DeliverySwept, nil},
		{"api", 1, routing.DeliveryReached, [][]string{{"founder:mention:person"}}},
	} {
		page, next := routedOf(t, n, tc.repo, tc.number, 0, "")
		var lists [][]string
		for _, r := range page.Records {
			lists = append(lists, recipientsOf(r))
			if !r.At.Before(page.RetainedFrom) {
				t.Errorf("%s!%d's record at %v is not older than the page's horizon %v", tc.repo, tc.number,
					r.At, page.RetainedFrom)
			}
		}
		switch {
		case page.Delivery != tc.want:
			t.Errorf("%s!%d's routing is %q, want %q", tc.repo, tc.number, page.Delivery, tc.want)
		case !page.RetainedFrom.Equal(horizon):
			t.Errorf("%s!%d's routing keeps every notice from %v, want %v", tc.repo, tc.number,
				page.RetainedFrom, horizon)
		case !slices.EqualFunc(lists, tc.lists, slices.Equal[[]string]):
			t.Errorf("%s!%d's routing lists %v, want %v", tc.repo, tc.number, lists, tc.lists)
		case next != "":
			t.Errorf("%s!%d's one page names a next one, %q", tc.repo, tc.number, next)
		case page.Change.Ref != code.ChangeRef(tc.repo, tc.number) || page.OpenedAt.IsZero():
			t.Errorf("%s!%d's routing names the change %+v opened at %v", tc.repo, tc.number, page.Change,
				page.OpenedAt)
		}
	}
}

// A CHANGE'S ROUTING IS READ A WHOLE RECORD AT A TIME, newest first: every
// recipient of one record on one page, in the order a reader scans them —
// addressed first, each group by handle — a page closing only between two
// records, and every later page `reached`, since its first listed one.
//
// Mutation: close a page by rows rather than records → a page of one splits
// the open's two recipients, red; order the records oldest first → the first
// page is the open, red.
func TestAChangesRoutingIsReadAWholeRecordAtATime(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	storedBefore(n, time.Hour)
	c, _ := committerOn(t, n, n.pub)
	repo := changeRepo(t, n, "web")
	open := opening(repo, featureRef, mainRef, dev)
	open.Open.Mentions = []string{"founder", "cto"}
	opened, err := c.ProposeChange(t.Context(), open)
	requireChanged(t, opened, err)
	w, _ := writerOn(t, n, n.pub)
	converse(t, w, asking(repo, opened.ChangeID, dev, "plat-lead"))
	converse(t, w, reviewing(repo, opened.ChangeID, platLead, 1, code.VerdictApprove, nil))

	want := [][]string{
		{"dev:review_submitted:agent"},
		{"plat-lead:review_requested:agent"},
		{"cto:mention:agent", "founder:mention:person"},
	}
	var got [][]string
	cursor := ""
	for pages := 1; ; pages++ {
		page, next := routedOf(t, n, "web", 1, 1, cursor)
		if pages > len(want)+1 {
			t.Fatalf("the routing took %d pages of one record, want %d", pages, len(want))
		}
		if page.Delivery != routing.DeliveryReached {
			t.Errorf("page %d of the routing is %q, want reached", pages, page.Delivery)
		}
		for _, r := range page.Records {
			got = append(got, recipientsOf(r))
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if !slices.EqualFunc(got, want, slices.Equal[[]string]) {
		t.Fatalf("the routing read a record a page lists %v, want %v", got, want)
	}

	whole, _ := routedOf(t, n, "web", 1, 0, "")
	if len(whole.Records) != 3 || whole.Records[2].Excerpt != "" || whole.Records[1].Excerpt != "the retry loop" {
		t.Errorf("one page of the routing lists %+v, want the three records with what each said", whole.Records)
	}
	if _, err := n.reader.Read(t.Context(), code.ReadQuery{Asker: asker, Kind: code.ReadChangeRouting, Repo: "web",
		Change: 9}); !errors.Is(err, code.ErrChangeNotFound) {
		t.Errorf("the routing of a change web does not hold = %v, want ErrChangeNotFound", err)
	}
}

// A REVIEWER FINDS THE REVIEWS THEY GAVE, newest first, across repositories,
// each naming the change it judged — and only those of repositories the asker
// may read: a seat outside a `visibility: unit` repository's unit is listed
// the review of the company's repository alone, an empty page standing for the
// review it may not read, never the listing's end; a person is listed both.
//
// Mutation: list every review the index holds, unjudged → vp-sales is listed
// the unit repository's review, red; take the cursor from the kept rows → the
// first page of one ends the listing, red.
func TestAReviewerFindsTheReviewsTheyGave(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := committerOn(t, n, n.pub)
	w, _ := writerOn(t, n, n.pub)
	web := changeRepo(t, n, "web")
	s := suiteSettings()
	s.Owner, s.Visibility = code.Owner{Kind: code.OwnerUnit, Ref: "Engineering"}, code.VisibilityUnit
	private := n.ensure("eng-private", s, t1).RepoID
	n.push(private, entry(uuid.Must(uuid.NewV7()).String(), set(mainRef, code.ZeroOID, oidOf("A")),
		set(featureRef, code.ZeroOID, oidOf("B"))))

	for _, repo := range []string{web, private} {
		opened, err := c.ProposeChange(t.Context(), opening(repo, featureRef, mainRef, dev))
		requireChanged(t, opened, err)
		converse(t, w, reviewing(repo, opened.ChangeID, platLead, 1, code.VerdictComment, nil))
	}

	listing := func(who code.Principal, limit int) ([]string, int) {
		t.Helper()
		var refs []string
		q := code.ReadQuery{Asker: who, Kind: code.ReadReviews, Reviewer: "plat-lead", Limit: limit}
		for pages := 1; pages <= 5; pages++ {
			answer, err := n.reader.Read(t.Context(), q)
			if err != nil {
				t.Fatalf("Read(reviews) as %+v: %v", who, err)
			}
			for _, r := range answer.Reviews {
				if r.Change == nil || r.Reviewer != "plat-lead" || r.Change.Title != "Retry the upload" {
					t.Fatalf("a listed review is %+v, want plat-lead's naming its change", r)
				}
				refs = append(refs, r.Change.Ref)
			}
			if answer.Next == "" {
				return refs, pages
			}
			q.Cursor = answer.Next
		}
		t.Fatalf("the listing as %+v did not end in five pages", who)
		return nil, 0
	}
	if got, _ := listing(ada, 0); !slices.Equal(got, []string{"eng-private!1", "web!1"}) {
		t.Errorf("a person is listed %v, want both reviews newest first", got)
	}
	if got, pages := listing(vpSales, 1); !slices.Equal(got, []string{"web!1"}) || pages != 3 {
		t.Errorf("a seat outside Engineering is listed %v in %d pages of one, want web!1's alone in three", got,
			pages)
	}
	for _, q := range []code.ReadQuery{
		{Asker: asker, Kind: code.ReadReviews},
		{Asker: asker, Kind: code.ReadReviews, Reviewer: "Not A Handle"},
		{Asker: asker, Kind: code.ReadReviews, Reviewer: "plat-lead", Repo: "web"},
	} {
		if _, err := n.reader.Read(t.Context(), q); !errors.Is(err, code.ErrInvalid) {
			t.Errorf("Read(%+v) = %v, want ErrInvalid", q, err)
		}
	}
}

// A PERSON'S NOTICE IS KEPT AT LEAST AS LONG AS A SEAT'S, which is what lets a
// change's routing date every notice by one horizon: the shortest inbox the
// company may keep is no shorter than a seat's notice is kept.
//
// Mutation: lower config.MinInboxRetentionDays below thirty → red.
func TestEveryNoticeIsKeptAtLeastAsLongAsASeats(t *testing.T) {
	t.Parallel()
	if shortest := time.Duration(config.MinInboxRetentionDays) * 24 * time.Hour; shortest < code.SeatNoticeHorizon {
		t.Fatalf("a company may keep a person's notice %v, shorter than a seat's %v: a change's routing could "+
			"not date its notices by the seats' horizon", shortest, code.SeatNoticeHorizon)
	}
}
