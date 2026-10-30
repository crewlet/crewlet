package code_test

import (
	"slices"
	"strconv"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
)

// servedChange is one change a listing served, beside the query whose page
// served it — the cursor it was read after included.
type servedChange struct {
	s code.ChangeSummary
	q code.ReadQuery
}

// servedChanges is every change q lists, read limit rows a page, each beside
// the query of the page that served it.
func servedChanges(t *testing.T, n *node, q code.ReadQuery, limit int) []servedChange {
	t.Helper()
	q.Kind, q.Limit, q.Fresh = code.ReadChanges, limit, stale
	var out []servedChange
	for pages := 1; pages <= 100; pages++ {
		answer, err := n.reader.Read(t.Context(), q)
		if err != nil {
			t.Fatalf("Read(changes) as %+v: %v", q, err)
		}
		for _, s := range answer.Changes {
			out = append(out, servedChange{s: s, q: q})
		}
		if answer.Next == "" {
			return out
		}
		q.Cursor = answer.Next
	}
	t.Fatalf("the listing %+v did not end in a hundred pages", q)
	return nil
}

// refsOf is the references of served, in their order.
func refsOf(served []servedChange) []string {
	out := make([]string, len(served))
	for i, sv := range served {
		out[i] = sv.s.Ref
	}
	return out
}

// partRow is one row a part of a change served: what names it, and the cursor
// its row says resumes after it.
type partRow struct{ id, cursor string }

// A PAGE KEPT SHORTER THAN IT WAS SERVED RESUMES AFTER ITS LAST ROW: every
// row a read of changes serves names the cursor that resumes after it — a
// listed change by the walk its listing takes (code.ReadQuery.ChangesCursor),
// a revision, a reviewer, a comment, a review and a finding by its own — so a
// reader that keeps any first rows of a page and resumes after the last it
// kept reads every row after it once, in order: whichever index a listing
// walks, and whichever half of what waits on the asker the row came from; and
// a page's own cursor is its last row's.
//
// Mutations: ChangesCursor spelled by another walk than the reader's (a
// repository's listing by its opening), the second half of needs=me resumed
// without its prefix, a comment's cursor without its thread, a review's by
// its id → red.
func TestAPageResumesAfterAnyRowItServed(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := committerOn(t, n, n.pub)
	w, _ := writerOn(t, n, n.pub)
	ctx := t.Context()
	web := changeRepo(t, n, "web")
	app := changeRepo(t, n, "app")
	n.push(web, entry(uuid.Must(uuid.NewV7()).String(), set("refs/heads/fix", code.ZeroOID, oidOf("F"))))
	open := func(repo, source string, owner code.Principal) code.ChangeResult {
		t.Helper()
		res, err := c.ProposeChange(ctx, opening(repo, source, mainRef, owner))
		requireChanged(t, res, err)
		return res
	}
	converse := func(cw code.ConversationWrite) code.ConversationResult {
		t.Helper()
		res, err := w.Converse(ctx, cw)
		requireConversed(t, res, err)
		return res
	}
	web1, web2 := open(web, featureRef, dev), open(web, releaseRef, cto)
	open(web, "refs/heads/fix", dev)
	app1, app2 := open(app, featureRef, dev), open(app, releaseRef, cto)
	pushedBy(t, c, web, dev, update(featureRef, oidOf("B"), oidOf("B2")))
	pushedBy(t, c, web, dev, update(featureRef, oidOf("B2"), oidOf("B3")))
	// WHAT WAITS ON DEV, both halves: cto's changes asking dev's review, and
	// dev's own whose reviewer asked for changes.
	converse(asking(web, web2.ChangeID, cto, "dev", "plat-lead"))
	converse(asking(app, app2.ChangeID, cto, "dev", "plat-lead"))
	converse(asking(web, web1.ChangeID, cto, "plat-lead", "founder"))
	converse(asking(app, app1.ChangeID, cto, "plat-lead"))
	converse(reviewing(web, web1.ChangeID, platLead, 3, code.VerdictRequestChanges, nil,
		raising(code.SeverityBlocker, 1, "Retries forever"), raising(code.SeverityMinor, 2, "A long line"),
		raising(code.SeverityNit, 3, "A typo")))
	converse(reviewing(web, web1.ChangeID, ada, 3, code.VerdictComment, nil))
	converse(reviewing(app, app1.ChangeID, platLead, 1, code.VerdictRequestChanges, nil))
	first := converse(saying(web, web1.ChangeID, platLead, "", "Why a minute?")).ID
	converse(saying(web, web1.ChangeID, cto, "", "Ship it."))
	converse(saying(web, web1.ChangeID, dev, first, "The upstream limit."))

	for name, q := range map[string]code.ReadQuery{
		"a repository's":                    {Asker: cto, Repo: "web"},
		"a repository's in a state":         {Asker: cto, Repo: "web", State: code.ChangeStateOpen},
		"the company's in a state":          {Asker: cto, State: code.ChangeStateOpen},
		"an owner's in a state":             {Asker: cto, Owner: "dev", State: code.ChangeStateOpen},
		"a reviewer's asks":                 {Asker: cto, Reviewer: "plat-lead"},
		"a reviewer's asks on a repository": {Asker: cto, Reviewer: "plat-lead", Repo: "app"},
		"what waits on dev's fix":           {Asker: dev, Needs: code.NeedFix},
		"what waits on dev":                 {Asker: dev, Needs: code.NeedMe},
	} {
		served := servedChanges(t, n, q, 50)
		if len(served) < 2 {
			t.Fatalf("%s lists %v; the case needs two changes to resume between", name, refsOf(served))
		}
		for i, sv := range served {
			resumed := sv.q
			resumed.Cursor = sv.q.ChangesCursor(sv.s)
			if got, want := refsOf(servedChanges(t, n, resumed, 50)), refsOf(served[i+1:]); !slices.Equal(got, want) {
				t.Errorf("%s resumed after %s (cursor %q) listed %v, want %v", name, sv.s.Ref, resumed.Cursor, got,
					want)
			}
		}
	}

	for _, part := range []struct {
		part code.ChangePart
		rows func(a code.ReadAnswer) []partRow
	}{
		{code.PartRevisions, func(a code.ReadAnswer) []partRow {
			var out []partRow
			for _, r := range a.Revisions {
				out = append(out, partRow{strconv.FormatInt(r.N, 10), r.Cursor()})
			}
			return out
		}},
		{code.PartReviewers, func(a code.ReadAnswer) []partRow {
			var out []partRow
			for _, r := range a.Reviewers {
				out = append(out, partRow{r.Reviewer, r.Cursor()})
			}
			return out
		}},
		{code.PartComments, func(a code.ReadAnswer) []partRow {
			var out []partRow
			for _, r := range a.Comments {
				out = append(out, partRow{r.ID, r.Cursor()})
			}
			return out
		}},
		{code.PartReviews, func(a code.ReadAnswer) []partRow {
			var out []partRow
			for _, r := range a.Reviews {
				out = append(out, partRow{r.ID, r.Cursor()})
			}
			return out
		}},
		{code.PartFindings, func(a code.ReadAnswer) []partRow {
			var out []partRow
			for _, r := range a.Findings {
				out = append(out, partRow{r.ID, r.Cursor()})
			}
			return out
		}},
	} {
		read := func(cursor string, limit int) ([]partRow, string) {
			t.Helper()
			answer, err := n.reader.Read(ctx, code.ReadQuery{Asker: cto, Kind: code.ReadChange, Repo: "web",
				Change: 1, Part: part.part, Cursor: cursor, Limit: limit, Fresh: stale})
			if err != nil {
				t.Fatalf("read web!1's %s after %q: %v", part.part, cursor, err)
			}
			return part.rows(answer), answer.Next
		}
		// EVERY ROW, a page of one at a time: each page's cursor is its
		// one row's own.
		var all []partRow
		for cursor := ""; ; {
			rows, next := read(cursor, 1)
			all = append(all, rows...)
			if next == "" {
				break
			}
			if len(rows) != 1 || next != rows[0].cursor {
				t.Fatalf("a page of %s holding %v resumes at %q, want its row's own cursor", part.part, rows, next)
			}
			cursor = next
		}
		if len(all) < 2 {
			t.Fatalf("web!1 has %d %s; the case needs two to resume between", len(all), part.part)
		}
		for i, row := range all {
			rest, _ := read(row.cursor, 50)
			if !slices.Equal(rest, all[i+1:]) {
				t.Errorf("web!1's %s resumed after %s read %v, want %v", part.part, row.id, rest, all[i+1:])
			}
		}
	}
}
