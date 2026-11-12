package gitlet_test

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE READS OF CHANGES: a listing of them and one in detail, read through the
// shipped reader from the rows the shipped committer's records wrote, on a
// real embedded broker.

// readChange is change number of repo as who reads it, at limit revisions a
// page, after cursor.
func readChange(t *testing.T, n *node, who gitlet.Principal, ceiling, repo string, number int64, limit int,
	cursor string) (gitlet.ReadAnswer, error) {

	t.Helper()
	return n.reader.Read(t.Context(), gitlet.ReadQuery{Asker: who, CeilingSeat: ceiling, Kind: gitlet.ReadChange,
		Repo: repo, Change: number, Limit: limit, Cursor: cursor, Fresh: stale})
}

// listChanges is every change q lists, by reference (web!1) in the order its
// pages served them, read limit rows a page, and how many pages that took.
func listChanges(t *testing.T, n *node, q gitlet.ReadQuery, limit int) ([]string, int) {
	t.Helper()
	q.Kind, q.Limit, q.Fresh = gitlet.ReadChanges, limit, stale
	var refs []string
	for pages := 1; pages <= 100; pages++ {
		answer, err := n.reader.Read(t.Context(), q)
		if err != nil {
			t.Fatalf("Read(changes) as %+v: %v", q, err)
		}
		for _, c := range answer.Changes {
			refs = append(refs, c.Ref)
		}
		if answer.Next == "" {
			return refs, pages
		}
		q.Cursor = answer.Next
	}
	t.Fatalf("the listing %+v did not end in a hundred pages", q)
	return nil, 0
}

// A CHANGE IS FOUND BY ITS NUMBER ON ITS REPOSITORY: web!1 is the first change
// opened on web, whatever another repository numbers its own — its row, its
// description, its work item and its authors on a first page beside its
// revisions newest first, a page at a time, and a later page its revisions
// alone. A number the repository does not hold is not found, naming the
// change; a repository nobody declared is not found as a repository.
//
// Mutations: read the change by its number without its repository → app!1
// answers for web!1; serve the revisions oldest first → the first page opens
// at revision 1; carry the detail on a later page too → the second page holds
// it.
func TestAChangeIsFoundByItsNumber(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := committerOn(t, n, n.pub)
	ctx := t.Context()
	app := changeRepo(t, n, "app")
	web := changeRepo(t, n, "web")

	other, err := c.ProposeChange(ctx, opening(app, featureRef, mainRef, cto))
	requireChanged(t, other, err)
	first := opening(web, featureRef, mainRef, dev)
	first.Open.Body = "Retries the upload once a minute, five times."
	first.Open.WorkItem = &types.WorkItem{Backend: types.WorkWorklet, ID: "task-1", Key: "ENG-1", Project: "ENG"}
	opened, err := c.ProposeChange(ctx, first)
	requireChanged(t, opened, err)
	second, err := c.ProposeChange(ctx, opening(web, releaseRef, mainRef, ada))
	requireChanged(t, second, err)
	byDev := pushedBy(t, c, web, dev, update(featureRef, oidOf("B"), oidOf("B2")))
	byCTO := pushedBy(t, c, web, cto, update(featureRef, oidOf("B2"), oidOf("B3")))

	page, err := readChange(t, n, ada, "", "web", 1, 2, "")
	if err != nil || page.Change == nil {
		t.Fatalf("web!1's first page = %+v, %v; want its detail", page, err)
	}
	got := page.Change
	switch {
	case got.ID != opened.ChangeID || got.Ref != "web!1" || got.Repo != "web" || got.RepoID != web ||
		got.Number != 1:
		t.Fatalf("web!1 read as %s (%s, %s of %s), want %s", got.Ref, got.ID, got.Repo, got.RepoID, opened.ChangeID)
	case got.Title != "Retry the upload" || got.Body != first.Open.Body ||
		got.BodyBytes != int64(len(first.Open.Body)):
		t.Errorf("web!1 says %q with %q (%d bytes), want its title and its description", got.Title, got.Body,
			got.BodyBytes)
	case got.State != gitlet.ChangeStateOpen || got.SourceRef != featureRef || got.TargetRef != mainRef ||
		got.Head != oidOf("B3") || got.Revision != 3 || got.Version != 3:
		t.Errorf("web!1 is %s from %s into %s at %s, revision %d, version %d; want open, at B3, revision 3, "+
			"version 3", got.State, got.SourceRef, got.TargetRef, got.Head, got.Revision, got.Version)
	case got.Owner != "dev" || got.OwnerKind != gitlet.PartyAgent || got.OwnerOperatorID != "":
		t.Errorf("web!1 is owned by %s (%s, %q), want the seat dev", got.Owner, got.OwnerKind, got.OwnerOperatorID)
	case got.WorkItem == nil || *got.WorkItem != *first.Open.WorkItem:
		t.Errorf("web!1 carries %+v, want %+v", got.WorkItem, first.Open.WorkItem)
	case got.OpenedPosition != opened.Position || got.OpenedAt.IsZero() ||
		got.Position != byCTO.Position || got.Access != "maintain":
		t.Errorf("web!1 opened at %v (%v), last written at %v, read at %s; want its open's record, the "+
			"second push's, and a person's maintain", got.OpenedPosition, got.OpenedAt, got.Position, got.Access)
	}
	if want := []gitlet.ChangeAuthor{
		{Author: "cto", Kind: gitlet.PartyAgent, FirstRevision: 3, Position: byCTO.Position},
		{Author: "dev", Kind: gitlet.PartyAgent, FirstRevision: 1, Position: opened.Position},
	}; !slices.Equal(got.Authors, want) {
		t.Errorf("web!1's authors are %+v, want %+v", got.Authors, want)
	}
	revisionsOf := func(revisions []gitlet.RevisionRow) []string {
		var out []string
		for _, r := range revisions {
			out = append(out, fmt.Sprintf("%d %s %s %s", r.N, r.Head, r.Pusher, r.PushID))
		}
		return out
	}
	if want := []string{
		fmt.Sprintf("3 %s cto %s", oidOf("B3"), byCTO.Answer.PushID),
		fmt.Sprintf("2 %s dev %s", oidOf("B2"), byDev.Answer.PushID),
	}; !slices.Equal(revisionsOf(page.Revisions), want) || page.Next != "2" {
		t.Errorf("web!1's first page of two revisions is %q then %q, want %q then 2", revisionsOf(page.Revisions),
			page.Next, want)
	}
	later, err := readChange(t, n, ada, "", "web", 1, 2, page.Next)
	if want := []string{fmt.Sprintf("1 %s dev ", oidOf("B"))}; err != nil || later.Change != nil ||
		!slices.Equal(revisionsOf(later.Revisions), want) || later.Next != "" {
		t.Errorf("web!1's second page is %+v, %v; want revision 1 alone, made by its open, and no detail",
			later, err)
	}

	byPerson, err := readChange(t, n, dev, "", "web", 2, 0, "")
	if err != nil || byPerson.Change == nil || byPerson.Change.ID != second.ChangeID ||
		byPerson.Change.Owner != "founder" || byPerson.Change.OwnerKind != gitlet.PartyPerson ||
		byPerson.Change.OwnerOperatorID != ada.OperatorID || byPerson.Change.Access != "write" ||
		byPerson.Change.WorkItem != nil {
		t.Errorf("web!2 read by dev = %+v, %v; want the person's change, owned by their human seat and the "+
			"credential they opened it with, at dev's write", byPerson.Change, err)
	}
	elsewhere, err := readChange(t, n, ada, "", "app", 1, 0, "")
	if err != nil || elsewhere.Change == nil || elsewhere.Change.ID != other.ChangeID ||
		elsewhere.Change.Ref != "app!1" {
		t.Errorf("app!1 = %+v, %v; want app's own first change", elsewhere.Change, err)
	}

	if _, err := readChange(t, n, ada, "", "web", 3, 0, ""); !errors.Is(err, gitlet.ErrChangeNotFound) ||
		!strings.Contains(err.Error(), "web!3") {
		t.Errorf("web!3, which nobody opened = %v, want ErrChangeNotFound naming web!3", err)
	}
	if _, err := readChange(t, n, ada, "", "nope", 1, 0, ""); !errors.Is(err, gitlet.ErrRepoNotFound) {
		t.Errorf("a change of a repository nobody declared = %v, want ErrRepoNotFound", err)
	}
	if _, err := readChange(t, n, ada, "", "web", 1, 0, "two"); !errors.Is(err, gitlet.ErrBadCursor) {
		t.Errorf("a page of revisions after a cursor no read issued = %v, want ErrBadCursor", err)
	}
}

// A CHANGE'S DETAIL IS READ ALONE WHEN IT IS ASKED ALONE (ReadQuery.
// DetailOnly): a change pushed three times, read for its detail alone, answers
// the very detail its first page of revisions carries — its row, its
// description, its authors and its readiness, which the chart judges — and
// not one revision beside it, nor a cursor to them: a merge's and a rebase's
// build read the change again and again and neither reads a revision, each
// of which carries its facts' touched paths.
//
// Mutation: read the revisions whatever the read asks → the detail comes
// with three revisions, red.
func TestAChangesDetailIsReadAloneWhenAskedAlone(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := committerOn(t, n, n.pub)
	ctx := t.Context()
	web := changeRepo(t, n, "web")
	opened, err := c.ProposeChange(ctx, opening(web, featureRef, mainRef, dev))
	requireChanged(t, opened, err)
	pushedBy(t, c, web, dev, update(featureRef, oidOf("B"), oidOf("B2")))
	pushedBy(t, c, web, cto, update(featureRef, oidOf("B2"), oidOf("B3")))

	page, err := readChange(t, n, ada, "", "web", 1, 0, "")
	if err != nil || page.Change == nil || len(page.Revisions) != 3 {
		t.Fatalf("web!1's first page = %+v, %v; want its detail and its three revisions", page, err)
	}
	alone, err := n.reader.Read(ctx, gitlet.ReadQuery{Asker: ada, Kind: gitlet.ReadChange, Repo: "web", Change: 1,
		DetailOnly: true, Fresh: stale})
	switch {
	case err != nil || alone.Change == nil:
		t.Fatalf("web!1's detail alone = %+v, %v; want its detail", alone, err)
	case len(alone.Revisions) != 0 || alone.Next != "":
		t.Errorf("web!1's detail alone came with %d revisions and cursor %q, want none", len(alone.Revisions),
			alone.Next)
	case alone.Change.Readiness == nil:
		t.Errorf("web!1's detail alone carries no readiness, which every detail carries")
	case !reflect.DeepEqual(alone.Change, page.Change):
		t.Errorf("web!1's detail alone is %+v, want the first page's %+v", alone.Change, page.Change)
	}
}

// A CHANGE READ IN BRIEF ANSWERS ITS ROW ALONE (ReadQuery.Brief): web!1's
// first page in brief carries the very row, repository and access its
// detail's summary carries, and its revisions as the full first page pages
// them — and nothing the detail reads besides: no detail at all, so no
// description, authors, conversation or mergeability, and no readiness, which
// a person's read judges against a chart. So a reader that holds no chart,
// which refuses a person's full first page about itself, answers that
// person's brief one. Asked in brief AND alone (DetailOnly), it answers the
// row and reads no revision.
//
// Mutations: answer the detail in brief as well → Change is set beside the
// brief row; judge the readiness in brief → the chartless reader refuses the
// brief read; put a readiness on the brief row → it carries one; read the
// revisions of a row asked alone → three revisions beside it, red.
func TestAChangeReadInBriefAnswersItsRowAlone(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := committerOn(t, n, n.pub)
	ctx := t.Context()
	web := changeRepo(t, n, "web")
	first := opening(web, featureRef, mainRef, dev)
	first.Open.Body = "Retries the upload once a minute."
	opened, err := c.ProposeChange(ctx, first)
	requireChanged(t, opened, err)
	pushedBy(t, c, web, dev, update(featureRef, oidOf("B"), oidOf("B2")))
	pushedBy(t, c, web, cto, update(featureRef, oidOf("B2"), oidOf("B3")))

	full, err := readChange(t, n, ada, "", "web", 1, 2, "")
	if err != nil || full.Change == nil || full.Change.Readiness == nil {
		t.Fatalf("web!1's first page = %+v, %v; want its detail, judged", full, err)
	}
	unrostered, err := gitlet.NewReader(gitlet.ReaderOptions{Log: n.authority})
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	brief := gitlet.ReadQuery{Asker: ada, Kind: gitlet.ReadChange, Repo: "web", Change: 1, Limit: 2, Brief: true,
		Fresh: stale}
	if _, err = unrostered.Read(ctx, gitlet.ReadQuery{Asker: ada, Kind: gitlet.ReadChange, Repo: "web", Change: 1,
		Fresh: stale}); !errors.Is(err, gitlet.ErrNotServing) {
		t.Fatalf("a chartless reader's full first page of web!1 = %v, want ErrNotServing about itself", err)
	}
	for name, r := range map[string]*gitlet.Reader{"the node's reader": n.reader, "a chartless reader": unrostered} {
		got, err := r.Read(ctx, brief)
		want := full.Change.ChangeSummary
		want.Readiness = nil
		switch {
		case err != nil || got.ChangeBrief == nil:
			t.Errorf("%s: web!1 in brief = %+v, %v; want its row", name, got, err)
		case got.Change != nil:
			t.Errorf("%s: web!1 in brief came with its detail %+v, which a brief read never reads", name,
				got.Change)
		case !reflect.DeepEqual(*got.ChangeBrief, want):
			t.Errorf("%s: web!1 in brief is %+v, want its detail's row without a readiness %+v", name,
				*got.ChangeBrief, want)
		case !reflect.DeepEqual(got.Revisions, full.Revisions) || got.Next != full.Next:
			t.Errorf("%s: web!1's revisions in brief are %+v (next %q), want the full page's %+v (next %q)",
				name, got.Revisions, got.Next, full.Revisions, full.Next)
		}

		// IN BRIEF AND ALONE (ReadQuery.DetailOnly beside Brief): the very
		// row, and no revision read with it — what an executor's launch on a
		// change reads.
		alone, err := r.Read(ctx, gitlet.ReadQuery{Asker: ada, Kind: gitlet.ReadChange, Repo: "web", Change: 1,
			Brief: true, DetailOnly: true, Fresh: stale})
		switch {
		case err != nil || alone.ChangeBrief == nil || alone.Change != nil:
			t.Errorf("%s: web!1's row alone = %+v, %v; want its row and no detail", name, alone, err)
		case !reflect.DeepEqual(*alone.ChangeBrief, want):
			t.Errorf("%s: web!1's row alone is %+v, want %+v", name, *alone.ChangeBrief, want)
		case len(alone.Revisions) != 0 || alone.Next != "":
			t.Errorf("%s: web!1's row alone came with %d revisions and cursor %q, want none", name,
				len(alone.Revisions), alone.Next)
		}
	}
}

// A READ THAT RESOLVES ITS REPOSITORY ANSWERS ITS ROW (ReadQuery.Resolve): a
// change read, a listing of a repository's changes and a listing of its checks,
// each asked to resolve the name it reads by, answer beside their pages the
// very row a `repo` read resolves the name to — and none where they are not
// asked; a resolving read is held to the grant as the read it rides on is, so
// a seat that may not read the repository is refused it, never handed its
// row.
//
// Mutations: answer no resolution → Resolved is nil; answer the grant's
// partial row (readableIn's) → it differs from the repo read's; resolve
// without judging the grant → vp-sales is answered plat-private's row.
func TestAReadThatResolvesItsRepositoryAnswersItsRow(t *testing.T) {
	t.Parallel()
	n, _ := grantedChanges(t)
	ctx := t.Context()
	dev := agent("dev")
	repo, err := n.reader.Read(ctx, gitlet.ReadQuery{Asker: dev, Kind: gitlet.ReadRepo, Repo: "plat-private",
		Fresh: stale})
	if err != nil || repo.Repo == nil {
		t.Fatalf("plat-private as dev reads it = %+v, %v; want its detail", repo, err)
	}
	for name, q := range map[string]gitlet.ReadQuery{
		"a change read":           {Kind: gitlet.ReadChange, Change: 1},
		"a part of a change":      {Kind: gitlet.ReadChange, Change: 1, Part: gitlet.PartComments},
		"a change in brief":       {Kind: gitlet.ReadChange, Change: 1, Brief: true},
		"a change's detail alone": {Kind: gitlet.ReadChange, Change: 1, DetailOnly: true},
		"a change's row alone":    {Kind: gitlet.ReadChange, Change: 1, Brief: true, DetailOnly: true},
		"a listing of changes":    {Kind: gitlet.ReadChanges},
		"a listing of checks":     {Kind: gitlet.ReadChecks, Checks: &gitlet.CheckQuery{Change: 1}},
	} {
		q.Asker, q.Repo, q.Fresh = dev, "plat-private", stale
		plain, err := n.reader.Read(ctx, q)
		if err != nil || plain.Resolved != nil {
			t.Errorf("%s not asked to resolve = %+v, %v; want no resolution", name, plain.Resolved, err)
		}
		q.Resolve = true
		got, err := n.reader.Read(ctx, q)
		switch {
		case err != nil || got.Resolved == nil:
			t.Errorf("%s asked to resolve = %+v, %v; want plat-private's row", name, got.Resolved, err)
		case !reflect.DeepEqual(*got.Resolved, repo.Repo.RepoRef):
			t.Errorf("%s resolved plat-private as %+v, want the repo read's %+v", name, *got.Resolved,
				repo.Repo.RepoRef)
		}
		q.Asker = agent("vp-sales")
		if got, err = n.reader.Read(ctx, q); !errors.Is(err, gitlet.ErrNotPermitted) || got.Resolved != nil {
			t.Errorf("%s resolved for vp-sales = %+v, %v; want ErrNotPermitted and no row", name, got.Resolved,
				err)
		}
	}
}

// grantedChanges is a node judging seats by [testChart], with one change open
// on each of three repositories — sales-private (`visibility: unit`, Sales'),
// plat-private (`visibility: unit`, Platform's) and platform (`visibility:
// company`, Platform's), opened in that order by a person, so newest first
// they are platform!1, plat-private!1, sales-private!1 — and the three
// repositories' ids by name.
func grantedChanges(t *testing.T) (*node, map[string]string) {
	t.Helper()
	n := newNode(t)
	n.chart = testChart()
	c, _ := committerOn(t, n, n.pub)
	ids := map[string]string{}
	for _, r := range []struct {
		name, unit string
		visibility gitlet.Visibility
	}{
		{"sales-private", "Sales", gitlet.VisibilityUnit},
		{"plat-private", "Platform", gitlet.VisibilityUnit},
		{"platform", "Platform", gitlet.VisibilityCompany},
	} {
		s := suiteSettings()
		s.Owner, s.Visibility = gitlet.Owner{Kind: gitlet.OwnerUnit, Ref: r.unit}, r.visibility
		repo := n.ensure(r.name, s, t1).RepoID
		n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set(mainRef, gitlet.ZeroOID, oidOf("A")),
			set(featureRef, gitlet.ZeroOID, oidOf("B"))))
		res, err := c.ProposeChange(t.Context(), opening(repo, featureRef, mainRef, ada))
		requireChanged(t, res, err)
		ids[r.name] = repo
	}
	return n, ids
}

// A CHANGE READ IS HELD TO THE GRANT on the change's repository: a seat lists
// the changes of exactly the repositories its grant reads, each at its access,
// and a listing or a read naming a `visibility: unit` repository it may not
// read is refused not_permitted, naming the visibility — a number in it
// included, which is never answered "not found"; a seat reading for a
// colleague is held to both grants, refused about the colleague
// (ErrAboveCeiling) where only the colleague's shuts it out and about itself
// first; a person and a token read every repository's changes. A page is the
// rows it READ: one a seat may not read is left out and the listing goes on
// past it, an empty page with a cursor.
//
// Mutations: list across repositories without judging each row's repository
// → vp-sales lists plat-private!1; judge the asker alone → dev, reading for
// vp-sales, lists plat-private!1; take the cursor from the kept rows → the
// one-row listing ends at its second page, before sales-private!1; read a
// change before judging its repository → vp-sales is told plat-private!9 is
// not found.
func TestAChangeReadIsHeldToTheGrant(t *testing.T) {
	t.Parallel()
	n, _ := grantedChanges(t)
	unboundToken := gitlet.Principal{Kind: gitlet.PrincipalOperator, OperatorID: "token:ci"}
	open := gitlet.ReadQuery{State: gitlet.ChangeStateOpen}
	for _, tc := range []struct {
		who     gitlet.Principal
		ceiling string
		want    []string
	}{
		{agent("vp-sales"), "", []string{"platform!1", "sales-private!1"}},
		{agent("dev"), "", []string{"platform!1", "plat-private!1"}},
		{agent("dev"), "vp-sales", []string{"platform!1"}},
		{agent("dev"), "cto", []string{"platform!1", "plat-private!1"}},
		{agent("root-bot"), "", []string{"platform!1"}},
		{ada, "", []string{"platform!1", "plat-private!1", "sales-private!1"}},
		{unboundToken, "", []string{"platform!1", "plat-private!1", "sales-private!1"}},
	} {
		q := open
		q.Asker, q.CeilingSeat = tc.who, tc.ceiling
		if got, _ := listChanges(t, n, q, 0); !slices.Equal(got, tc.want) {
			t.Errorf("%+v, reading for %q, lists %v; want %v", tc.who, tc.ceiling, got, tc.want)
		}
	}

	// ONE ROW A PAGE: vp-sales's second page reads plat-private!1 and keeps
	// nothing, and the listing goes on past it.
	q := open
	q.Asker = agent("vp-sales")
	q.Kind, q.Limit, q.Fresh = gitlet.ReadChanges, 1, stale
	first, err := n.reader.Read(t.Context(), q)
	if err != nil || len(first.Changes) != 1 || first.Next == "" {
		t.Fatalf("vp-sales's first one-row page = %+v, %v; want platform!1 and a cursor", first, err)
	}
	q.Cursor = first.Next
	second, err := n.reader.Read(t.Context(), q)
	if err != nil || len(second.Changes) != 0 || second.Next == "" {
		t.Fatalf("vp-sales's second one-row page = %+v, %v; want no change and a cursor past plat-private!1",
			second, err)
	}
	if got, pages := listChanges(t, n, gitlet.ReadQuery{Asker: agent("vp-sales"), State: gitlet.ChangeStateOpen},
		1); !slices.Equal(got, []string{"platform!1", "sales-private!1"}) || pages != 4 {
		t.Errorf("vp-sales's one-row listing is %v in %d pages, want platform!1 and sales-private!1 in four",
			got, pages)
	}

	// THE ACCESS each change is listed at is the asker's on its repository.
	access := map[string]string{}
	answer, err := n.reader.Read(t.Context(), gitlet.ReadQuery{Asker: agent("dev"), Kind: gitlet.ReadChanges,
		State: gitlet.ChangeStateOpen, Fresh: stale})
	if err != nil {
		t.Fatalf("dev's listing: %v", err)
	}
	for _, c := range answer.Changes {
		access[c.Ref] = c.Access
	}
	if fmt.Sprint(access) != "map[plat-private!1:write platform!1:write]" {
		t.Errorf("dev lists its changes at %v, want write on both of Platform's", access)
	}

	// A READ NAMING THE REPOSITORY — its listing, one change, a number it
	// does not hold — is refused whole, about the asker first.
	reads := func(who gitlet.Principal, ceiling string) map[string]func() error {
		return map[string]func() error{
			"listing": func() error {
				_, err := n.reader.Read(t.Context(), gitlet.ReadQuery{Asker: who, CeilingSeat: ceiling,
					Kind: gitlet.ReadChanges, Repo: "plat-private", Fresh: stale})
				return err
			},
			"listing in a state": func() error {
				_, err := n.reader.Read(t.Context(), gitlet.ReadQuery{Asker: who, CeilingSeat: ceiling,
					Kind: gitlet.ReadChanges, Repo: "plat-private", State: gitlet.ChangeStateOpen, Fresh: stale})
				return err
			},
			"change": func() error {
				_, err := readChange(t, n, who, ceiling, "plat-private", 1, 0, "")
				return err
			},
			"a later page of revisions": func() error {
				_, err := readChange(t, n, who, ceiling, "plat-private", 1, 0, "1")
				return err
			},
			"a number it does not hold": func() error {
				_, err := readChange(t, n, who, ceiling, "plat-private", 9, 0, "")
				return err
			},
		}
	}
	for name, read := range reads(agent("vp-sales"), "") {
		err := read()
		if !errors.Is(err, gitlet.ErrNotPermitted) || !strings.Contains(err.Error(), "visibility: unit") ||
			!strings.Contains(err.Error(), `unit "Platform"`) {
			t.Errorf("vp-sales's %s of plat-private = %v, want ErrNotPermitted naming the visibility and who "+
				"reads it", name, err)
		}
	}
	for name, read := range reads(agent("dev"), "vp-sales") {
		err := read()
		if !errors.Is(err, gitlet.ErrAboveCeiling) || errors.Is(err, gitlet.ErrNotPermitted) ||
			!strings.Contains(err.Error(), `seat "vp-sales"`) {
			t.Errorf("dev's %s of plat-private for vp-sales = %v, want ErrAboveCeiling naming vp-sales", name, err)
		}
	}
	for name, read := range reads(agent("vp-sales"), "root-bot") {
		if err := read(); !errors.Is(err, gitlet.ErrNotPermitted) || !strings.Contains(err.Error(), `seat "vp-sales"`) {
			t.Errorf("vp-sales's %s of plat-private for root-bot = %v, want ErrNotPermitted about vp-sales "+
				"itself", name, err)
		}
	}
	for name, who := range map[string]gitlet.Principal{"dev": agent("dev"), "a person": ada, "a token": unboundToken} {
		if _, err := readChange(t, n, who, "", "plat-private", 1, 0, ""); err != nil {
			t.Errorf("%s's read of plat-private!1 = %v, want it served", name, err)
		}
		if _, err := readChange(t, n, who, "", "plat-private", 9, 0, ""); !errors.Is(err, gitlet.ErrChangeNotFound) {
			t.Errorf("%s's read of plat-private!9 = %v, want ErrChangeNotFound: it reads the repository", name, err)
		}
	}
}

// A LISTING ACROSS REPOSITORIES LEAVES A DELETED ONE'S CHANGES OUT, as a
// listing of repositories leaves the repository out — its changes are no work
// anybody can do until it is restored — while a listing naming the repository
// and a read of one of its changes are served: it was named.
//
// Mutation: list a deleted repository's changes across repositories →
// platform!1 is listed after platform's deletion.
func TestAListingAcrossRepositoriesLeavesADeletedOnesChangesOut(t *testing.T) {
	t.Parallel()
	n, ids := grantedChanges(t)
	if _, err := n.store.DeleteRepo(t.Context(), n.activate(t3), ids["platform"]); err != nil {
		t.Fatalf("delete platform: %v", err)
	}
	n.drain()
	if got, _ := listChanges(t, n, gitlet.ReadQuery{Asker: ada, State: gitlet.ChangeStateOpen}, 0); !slices.Equal(got,
		[]string{"plat-private!1", "sales-private!1"}) {
		t.Errorf("the open changes once platform is deleted are %v, want the two live repositories'", got)
	}
	if got, _ := listChanges(t, n, gitlet.ReadQuery{Asker: ada, Repo: "platform"}, 0); !slices.Equal(got,
		[]string{"platform!1"}) {
		t.Errorf("the deleted platform's own listing is %v, want platform!1", got)
	}
	if got, err := readChange(t, n, ada, "", "platform", 1, 0, ""); err != nil || got.Change == nil {
		t.Errorf("platform!1 once platform is deleted = %+v, %v; want it served", got, err)
	}
}

// A LISTING IS THE CHANGES ITS FILTERS NAME, newest first, whichever index it
// walks: every combination of a repository, a state, an owner and a work item
// a listing may name, read a row a page and fifty at once, lists exactly the
// changes that match all of them — the filters its index seeks and the ones it
// tests on the rows it read alike — in the order the records that opened them
// landed, newest first; and one naming no repository, work item or state is
// refused.
//
// Mutations: drop the owner test from a repository's walk → web's listing for
// dev lists web!2; drop the state test from a work item's walk → task-1's
// drafts list its open changes; order a repository's walk oldest first →
// web's listing opens at web!1.
func TestAListingIsTheChangesItsFiltersName(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	// THE CHART every listing judges its changes' readiness by, whoever
	// asks.
	n.chart = testChart()
	c, _ := committerOn(t, n, n.pub)
	ctx := t.Context()
	web := changeRepo(t, n, "web")
	app := changeRepo(t, n, "app")
	const topicRef = "refs/heads/topic"
	for _, repo := range []string{web, app} {
		pushedBy(t, c, repo, ada, update(topicRef, gitlet.ZeroOID, oidOf("T")))
	}
	task := func(id string) *types.WorkItem {
		return &types.WorkItem{Backend: types.WorkWorklet, ID: id, Key: strings.ToUpper(id), Project: "ENG"}
	}
	type opened struct {
		ref, repo, owner, work string
		state                  gitlet.ChangeState
	}
	var every []opened
	open := func(repo, name, source string, who gitlet.Principal, work *types.WorkItem, draft bool) gitlet.ChangeResult {
		t.Helper()
		p := opening(repo, source, mainRef, who)
		p.Open.WorkItem, p.Open.Draft = work, draft
		res, err := c.ProposeChange(ctx, p)
		requireChanged(t, res, err)
		owner := who.Seat
		state := gitlet.ChangeStateOpen
		if draft {
			state = gitlet.ChangeStateDraft
		}
		o := opened{ref: gitlet.ChangeRef(name, res.Number), repo: name, owner: owner, state: state}
		if work != nil {
			o.work = work.Ref()
		}
		every = append(every, o)
		return res
	}
	setState := func(ref string, state gitlet.ChangeState) {
		for i := range every {
			if every[i].ref == ref {
				every[i].state = state
			}
		}
	}
	open(web, "web", featureRef, dev, task("task-1"), false)
	open(web, "web", releaseRef, ada, nil, true)
	closeMe := open(web, "web", topicRef, dev, nil, false)
	closed, err := c.ProposeChange(ctx, closing(web, closeMe.ChangeID, dev, closeMe.Version))
	requireChanged(t, closed, err)
	setState("web!3", gitlet.ChangeStateClosed)
	open(app, "app", featureRef, dev, task("task-1"), false)
	open(app, "app", releaseRef, cto, nil, false)
	pushedBy(t, c, app, ada, update(mainRef, oidOf("A"), oidOf("R")))
	setState("app!2", gitlet.ChangeStateMerged)
	open(app, "app", topicRef, ada, task("task-2"), false)

	// NEWEST FIRST: every change is its own record, so the order the records
	// that opened them landed is the order they were opened in.
	newestFirst := slices.Clone(every)
	slices.Reverse(newestFirst)
	matching := func(repo, owner, work string, state gitlet.ChangeState) []string {
		var out []string
		for _, o := range newestFirst {
			if (repo == "" || o.repo == repo) && (owner == "" || o.owner == owner) &&
				(work == "" || o.work == work) && (state == "" || o.state == state) {
				out = append(out, o.ref)
			}
		}
		return out
	}
	cases := 0
	for _, repo := range []string{"", "web", "app"} {
		for _, state := range append([]gitlet.ChangeState{""}, gitlet.ChangeStates()...) {
			for _, owner := range []string{"", "dev", "founder"} {
				for _, work := range []string{"", "native:task-1", "native:task-2"} {
					q := gitlet.ReadQuery{Asker: ada, Repo: repo, State: state, Owner: owner, Work: work}
					if repo == "" && state == "" && work == "" {
						q.Kind, q.Fresh = gitlet.ReadChanges, stale
						if _, err := n.reader.Read(ctx, q); !errors.Is(err, gitlet.ErrInvalid) ||
							!strings.Contains(err.Error(), "state:") {
							t.Errorf("a listing naming no repository, work item or state (owner %q) = %v, want "+
								"ErrInvalid naming what it must name", owner, err)
						}
						continue
					}
					cases++
					want := matching(repo, owner, work, state)
					for _, limit := range []int{1, 50} {
						if got, _ := listChanges(t, n, q, limit); !slices.Equal(got, want) {
							t.Errorf("repo %q, state %q, owner %q, work %q, %d a page: listed %v, want %v", repo,
								state, owner, work, limit, got, want)
						}
					}
				}
			}
		}
	}
	if cases == 0 {
		t.Fatal("no combination was listed, so this measured nothing")
	}
}

// A LISTING OF CHANGES AND A CHANGE READ TAKE THEIR OWN FIELDS AND NO OTHER
// KIND'S, each refused naming it rather than ignored — a filter ignored is a
// listing of more than its caller asked for, which nothing in the answer says.
// A page of either is bounded as every page is, and a cursor of another order
// is no cursor of this one.
//
// Mutation: drop the refusal of a state outside a listing of changes → a
// repository listing narrowed to a change's state is served whole.
func TestOnlyAReadOfChangesTakesItsFields(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	// THE CHART a read of changes judges their readiness by, whoever asks:
	// a cursor is judged once it is.
	n.chart = testChart()
	changeRepo(t, n, "web")
	for name, tc := range map[string]struct {
		q     gitlet.ReadQuery
		field string
	}{
		"a listing in no state":         {gitlet.ReadQuery{Kind: gitlet.ReadChanges, State: "pending"}, "state:"},
		"a listing of nobody's":         {gitlet.ReadQuery{Kind: gitlet.ReadChanges, Owner: "Not A Handle", State: "open"}, "owner:"},
		"a listing of no work item":     {gitlet.ReadQuery{Kind: gitlet.ReadChanges, Work: "task-1"}, "work:"},
		"a work item of no tracker":     {gitlet.ReadQuery{Kind: gitlet.ReadChanges, Work: "nowhere:task-1"}, "work:"},
		"a work item of no id":          {gitlet.ReadQuery{Kind: gitlet.ReadChanges, Work: "native:"}, "work:"},
		"a listing past its page":       {gitlet.ReadQuery{Kind: gitlet.ReadChanges, Repo: "web", Limit: gitlet.MaxChangesPerPage + 1}, "limit:"},
		"a listing of no repository":    {gitlet.ReadQuery{Kind: gitlet.ReadChanges, Repo: "Not A Name"}, "repo:"},
		"a change of no number":         {gitlet.ReadQuery{Kind: gitlet.ReadChange, Repo: "web"}, "change:"},
		"a change of no repository":     {gitlet.ReadQuery{Kind: gitlet.ReadChange, Change: 1}, "repo:"},
		"revisions past their page":     {gitlet.ReadQuery{Kind: gitlet.ReadChange, Repo: "web", Change: 1, Limit: gitlet.MaxRevisionsPerPage + 1}, "limit:"},
		"a change in a state":           {gitlet.ReadQuery{Kind: gitlet.ReadChange, Repo: "web", Change: 1, State: "open"}, "state:"},
		"a listing naming a number":     {gitlet.ReadQuery{Kind: gitlet.ReadChanges, Repo: "web", Change: 1}, "change:"},
		"repositories in a state":       {gitlet.ReadQuery{Kind: gitlet.ReadRepos, State: "open"}, "state:"},
		"a history of an owner":         {gitlet.ReadQuery{Kind: gitlet.ReadPushes, Repo: "web", Owner: "dev"}, "owner:"},
		"refs of a work item":           {gitlet.ReadQuery{Kind: gitlet.ReadRefs, Repo: "web", Work: "native:task-1"}, "work:"},
		"a detail of a change's number": {gitlet.ReadQuery{Kind: gitlet.ReadRepo, Repo: "web", Change: 1}, "change:"},
		"a changes listing in brief":    {gitlet.ReadQuery{Kind: gitlet.ReadChanges, Repo: "web", Brief: true}, "brief:"},
		"a listing of nobody's review":  {gitlet.ReadQuery{Kind: gitlet.ReadChanges, Reviewer: "Not A Handle"}, "reviewer:"},
		"a listing of no need":          {gitlet.ReadQuery{Kind: gitlet.ReadChanges, Needs: "lunch"}, "needs:"},
		"a listing in no window":        {gitlet.ReadQuery{Kind: gitlet.ReadChanges, Window: "1y"}, "window:"},
		"a change in a window":          {gitlet.ReadQuery{Kind: gitlet.ReadChange, Repo: "web", Change: 1, Window: gitlet.WindowDay}, "window:"},
		"a history in a window":         {gitlet.ReadQuery{Kind: gitlet.ReadPushes, Repo: "web", Window: gitlet.WindowWeek}, "window:"},
		"a listing of a part":           {gitlet.ReadQuery{Kind: gitlet.ReadChanges, Repo: "web", Part: gitlet.PartComments}, "part:"},
		"a change of no part":           {gitlet.ReadQuery{Kind: gitlet.ReadChange, Repo: "web", Change: 1, Part: "diff"}, "part:"},
		"a change for a reviewer":       {gitlet.ReadQuery{Kind: gitlet.ReadChange, Repo: "web", Change: 1, Reviewer: "dev"}, "reviewer:"},
		"a change that needs":           {gitlet.ReadQuery{Kind: gitlet.ReadChange, Repo: "web", Change: 1, Needs: gitlet.NeedReview}, "needs:"},
		"a listing's detail alone":      {gitlet.ReadQuery{Kind: gitlet.ReadChanges, Repo: "web", DetailOnly: true}, "detail_only:"},
		"a part's detail alone":         {gitlet.ReadQuery{Kind: gitlet.ReadChange, Repo: "web", Change: 1, DetailOnly: true, Part: gitlet.PartRevisions}, "detail_only:"},
		"a later page's detail alone":   {gitlet.ReadQuery{Kind: gitlet.ReadChange, Repo: "web", Change: 1, DetailOnly: true, Cursor: "2"}, "detail_only:"},
		"a detail alone of a limit":     {gitlet.ReadQuery{Kind: gitlet.ReadChange, Repo: "web", Change: 1, DetailOnly: true, Limit: 1}, "detail_only:"},
		"a history of a part":           {gitlet.ReadQuery{Kind: gitlet.ReadPushes, Repo: "web", Part: gitlet.PartReviewers}, "part:"},
		"a part in brief":               {gitlet.ReadQuery{Kind: gitlet.ReadChange, Repo: "web", Change: 1, Brief: true, Part: gitlet.PartRevisions}, "brief:"},
		"a later page in brief":         {gitlet.ReadQuery{Kind: gitlet.ReadChange, Repo: "web", Change: 1, Brief: true, Cursor: "2"}, "brief:"},
		"a repository in brief":         {gitlet.ReadQuery{Kind: gitlet.ReadRepo, Repo: "web", Brief: true}, "brief:"},
		"a listing resolving nothing":   {gitlet.ReadQuery{Kind: gitlet.ReadChanges, State: "open", Resolve: true}, "resolve:"},
		"a repository resolving itself": {gitlet.ReadQuery{Kind: gitlet.ReadRepo, Repo: "web", Resolve: true}, "resolve:"},
		"a history resolving its name":  {gitlet.ReadQuery{Kind: gitlet.ReadPushes, Repo: "web", Resolve: true}, "resolve:"},
		"a duty listing resolving":      {gitlet.ReadQuery{Kind: gitlet.ReadCompactionDue, MinPacks: 1, Resolve: true}, "resolve:"},
		"comments past their page": {gitlet.ReadQuery{Kind: gitlet.ReadChange, Repo: "web", Change: 1,
			Part: gitlet.PartComments, Limit: gitlet.MaxCommentsPerPage + 1}, "limit:"},
		"reviewers past their page": {gitlet.ReadQuery{Kind: gitlet.ReadChange, Repo: "web", Change: 1,
			Part: gitlet.PartReviewers, Limit: gitlet.MaxReviewersPerPage + 1}, "limit:"},
	} {
		tc.q.Asker, tc.q.Fresh = asker, stale
		if _, err := n.reader.Read(t.Context(), tc.q); !errors.Is(err, gitlet.ErrInvalid) ||
			!strings.Contains(err.Error(), tc.field) {
			t.Errorf("%s: Read = %v, want ErrInvalid naming %s", name, err, tc.field)
		}
	}
	for name, q := range map[string]gitlet.ReadQuery{
		"a duty listing naming a state":   {Kind: gitlet.ReadCompactionDue, MinPacks: 1, State: "open"},
		"a publication listing of a work": {Kind: gitlet.ReadPublicationDue, Work: "native:task-1"},
		"an expunge listing of an owner":  {Kind: gitlet.ReadExpungeDue, Owner: "dev"},
		"an introduced page of a change":  {Kind: gitlet.ReadIntroduced, Change: 1, Introduced: &gitlet.IntroducedQuery{RepoID: "id"}},
		"an introduced page in a state":   {Kind: gitlet.ReadIntroduced, State: "open", Introduced: &gitlet.IntroducedQuery{RepoID: "id"}},
		"an introduced page of a work item": {Kind: gitlet.ReadIntroduced, Work: "native:task-1",
			Introduced: &gitlet.IntroducedQuery{RepoID: "id"}},
		"an introduced page of a reviewer": {Kind: gitlet.ReadIntroduced, Reviewer: "dev",
			Introduced: &gitlet.IntroducedQuery{RepoID: "id"}},
		"an introduced page of a target": {Kind: gitlet.ReadIntroduced, Target: "refs/heads/main",
			Introduced: &gitlet.IntroducedQuery{RepoID: "id"}},
		"an introduced page of a queue": {Kind: gitlet.ReadIntroduced, QueueDue: &gitlet.QueueDueQuery{},
			Introduced: &gitlet.IntroducedQuery{RepoID: "id"}},
		"an introduced page of running checks": {Kind: gitlet.ReadIntroduced,
			ChecksRunning: &gitlet.ChecksRunningQuery{}, Introduced: &gitlet.IntroducedQuery{RepoID: "id"}},
		"a duty listing that needs":        {Kind: gitlet.ReadCompactionDue, MinPacks: 1, Needs: gitlet.NeedReview},
		"a publication listing's part":     {Kind: gitlet.ReadPublicationDue, Part: gitlet.PartComments},
		"an expunge listing of a reviewer": {Kind: gitlet.ReadExpungeDue, Reviewer: "dev"},
	} {
		q.Fresh = stale
		if _, err := n.reader.Read(t.Context(), q); !errors.Is(err, gitlet.ErrInvalid) {
			t.Errorf("%s: Read = %v, want ErrInvalid", name, err)
		}
	}
	for name, q := range map[string]gitlet.ReadQuery{
		"a repository's listing after a position": {Repo: "web", Cursor: "123/0190f3c4-0000-7000-8000-000000000001"},
		"a state's listing after a number":        {State: "open", Cursor: "12"},
		"a repository's listing after nothing":    {Repo: "web", Cursor: "0"},
		"a reviewer's listing after a number":     {Reviewer: "dev", Cursor: "12"},
	} {
		q.Kind, q.Asker, q.Fresh = gitlet.ReadChanges, asker, stale
		if _, err := n.reader.Read(t.Context(), q); !errors.Is(err, gitlet.ErrBadCursor) {
			t.Errorf("%s: Read = %v, want ErrBadCursor", name, err)
		}
	}
	if _, err := n.reader.Read(t.Context(), gitlet.ReadQuery{Asker: asker, Kind: gitlet.ReadChanges, Repo: "web",
		Fresh: statelog.Freshness{Level: statelog.ReadSession}}); !errors.Is(err, gitlet.ErrBadLevel) {
		t.Errorf("a listing of changes at a bare session = %v, want ErrBadLevel", err)
	}
	// A NEED IS SOMEBODY'S: the asker's own where it names no reviewer, so a
	// credential bound to no seat names nobody it could be.
	if _, err := n.reader.Read(t.Context(), gitlet.ReadQuery{Asker: unbound, Kind: gitlet.ReadChanges,
		Needs: gitlet.NeedReview, Fresh: stale}); !errors.Is(err, gitlet.ErrInvalid) ||
		!strings.Contains(err.Error(), "needs:") {
		t.Errorf("a need of a credential bound to nobody = %v, want ErrInvalid naming needs", err)
	}
}
