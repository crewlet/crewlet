package code_test

import (
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/inbox"
)

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
