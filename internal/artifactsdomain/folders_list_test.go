package artifactsdomain_test

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// THE LISTING IN S3's TERMS, AND EVERY COUNT BESIDE IT, against one company:
//
//	Marketing/          jane's, everyone's
//	  Q3/               jane's
//	Drafts/             jane's own
//
// with artifacts top (top level), inMarketing and hidden (jane's own, never
// shared) in Marketing, inQ3 in Q3, and inDrafts in Drafts; every one but
// hidden is shared with the company.
type gallery struct {
	n                                        *node
	marketing, q3, drafts                    artifactsdomain.Folder
	top, inMarketing, hidden, inQ3, inDrafts artifacts.ID
	jane, bob                                artifactsdomain.Viewer
	janeCall                                 artifactsdomain.Call
}

func newGallery(t *testing.T) *gallery {
	t.Helper()
	n := newHarness(t)
	jane := person("jane", artifacts.RoleOwner)
	g := &gallery{n: n, janeCall: jane, jane: artifactsdomain.Viewer{Record: "jane"},
		bob: artifactsdomain.Viewer{Record: "bob"}}
	chain := n.mkdir(jane, "", artifactsdomain.FolderCompany, "Marketing", "Q3")
	g.marketing, g.q3 = chain[0], chain[1]
	g.drafts = n.mkdir(jane, "", artifactsdomain.FolderPrivate, "Drafts")[0]
	g.top = n.createIn(jane, "Top", "")
	g.inMarketing = n.createIn(jane, "In Marketing", g.marketing.ID)
	g.hidden = n.createIn(jane, "Hidden", g.marketing.ID)
	g.inQ3 = n.createIn(jane, "In Q3", g.q3.ID)
	g.inDrafts = n.createIn(jane, "In Drafts", g.drafts.ID)
	for _, id := range []artifacts.ID{g.top, g.inMarketing, g.inQ3, g.inDrafts} {
		n.shareWithCompany(jane, id)
	}
	return g
}

// list is one page for v.
func (g *gallery) list(v artifactsdomain.Viewer, q artifactsdomain.ListQuery) artifactsdomain.ListPage {
	g.n.t.Helper()
	page, err := g.n.reader.List(g.n.t.Context(), v, q)
	if err != nil {
		g.n.t.Fatalf("list %+v for %s: %v", q, v.Record, err)
	}
	return page
}

// idsOf is a page's artifacts, sorted.
func idsOf(page artifactsdomain.ListPage) []artifacts.ID {
	out := make([]artifacts.ID, 0, len(page.Contents))
	for _, item := range page.Contents {
		out = append(out, item.ID)
	}
	slices.SortFunc(out, compareIDs)
	return out
}

// sortedIDs is ids sorted.
func sortedIDs(ids ...artifacts.ID) []artifacts.ID {
	slices.SortFunc(ids, compareIDs)
	return ids
}

// THE TOP LEVEL, ONE LEVEL AT A TIME: what is filed in no folder, or in one
// the reader cannot see (bob is shown jane's Drafts' artifact at his top level,
// never hidden by a folder he cannot see, and never told its folder, by its
// path or by its id), and the
// child folders as common prefixes, each counting only what the reader may
// open.
//
// Mutation: count a folder's artifacts without the visibility predicate and
// bob's Marketing counts jane's hidden artifact; place an artifact in an
// invisible folder nowhere and bob never sees inDrafts; keep a row's folder id
// unscrubbed and bob is told the id of jane's Drafts.
func TestTheTopLevelHoldsWhatNoVisibleFolderDoes(t *testing.T) {
	t.Parallel()
	g := newGallery(t)
	slash := artifactsdomain.ListQuery{Delimiter: artifactsdomain.ListDelimiter}
	bob := g.list(g.bob, slash)
	if got := idsOf(bob); !slices.Equal(got, sortedIDs(g.top, g.inDrafts)) {
		t.Errorf("bob's top level holds %v", got)
	}
	for _, item := range bob.Contents {
		if item.FolderPath != "" || item.Folder != "" {
			t.Errorf("bob is told %s is filed in %q (folder %q)", item.Title, item.FolderPath, item.Folder)
		}
	}
	if len(bob.CommonPrefixes) != 1 || bob.CommonPrefixes[0] != (artifactsdomain.CommonPrefix{
		Prefix: "Marketing/", Folder: g.marketing.ID, Name: "Marketing",
		Visibility: artifactsdomain.FolderCompany, Items: 1, Folders: 1,
	}) {
		t.Errorf("bob's common prefixes are %+v", bob.CommonPrefixes)
	}
	if bob.Total != 2 || bob.KeyCount != 3 || bob.Delimiter != "/" || bob.Prefix != "" {
		t.Errorf("bob's page: total %d, keys %d, delimiter %q, prefix %q", bob.Total, bob.KeyCount,
			bob.Delimiter, bob.Prefix)
	}
	jane := g.list(g.jane, slash)
	if got := idsOf(jane); !slices.Equal(got, sortedIDs(g.top)) {
		t.Errorf("jane's top level holds %v", got)
	}
	var names []string
	for _, p := range jane.CommonPrefixes {
		names = append(names, p.Name)
	}
	if !slices.Equal(names, []string{"Drafts", "Marketing"}) || jane.CommonPrefixes[1].Items != 2 {
		t.Errorf("jane's common prefixes are %+v", jane.CommonPrefixes)
	}
}

// A PREFIX LISTS ONE FOLDER, ONE LEVEL OR ALL BENEATH IT: names compared by
// their fold, a one-level page naming its children with their prefixes, a
// recursive one everything beneath, each row naming its folder's path; a
// folder addressed by id lists alike; and an unknown or invisible prefix is
// not found, never an empty page, one answer for both.
//
// Mutation: answer an invisible prefix with an empty page and bob learns
// Drafts exists by the difference.
func TestAPrefixListsTheFolderItNames(t *testing.T) {
	t.Parallel()
	g := newGallery(t)
	one := g.list(g.bob, artifactsdomain.ListQuery{Prefix: "marketing/", Delimiter: "/"})
	if got := idsOf(one); !slices.Equal(got, sortedIDs(g.inMarketing)) || one.Prefix != "Marketing/" {
		t.Errorf("bob's Marketing holds %v under the prefix %q", got, one.Prefix)
	}
	if len(one.CommonPrefixes) != 1 || one.CommonPrefixes[0].Prefix != "Marketing/Q3/" ||
		one.CommonPrefixes[0].Items != 1 || one.CommonPrefixes[0].Folders != 0 {
		t.Errorf("bob's Marketing's common prefixes are %+v", one.CommonPrefixes)
	}
	all := g.list(g.bob, artifactsdomain.ListQuery{Prefix: "Marketing/"})
	if got := idsOf(all); !slices.Equal(got, sortedIDs(g.inMarketing, g.inQ3)) || all.CommonPrefixes != nil {
		t.Errorf("everything beneath Marketing for bob is %v, prefixes %v", got, all.CommonPrefixes)
	}
	for _, item := range all.Contents {
		want := map[artifacts.ID]string{g.inMarketing: "Marketing/", g.inQ3: "Marketing/Q3/"}[item.ID]
		if item.FolderPath != want {
			t.Errorf("%s names its folder %q, want %q", item.Title, item.FolderPath, want)
		}
	}
	byID := g.list(g.bob, artifactsdomain.ListQuery{Folder: g.q3.ID, Delimiter: "/"})
	if got := idsOf(byID); !slices.Equal(got, sortedIDs(g.inQ3)) || byID.Prefix != "Marketing/Q3/" {
		t.Errorf("Q3 by id holds %v under %q", got, byID.Prefix)
	}
	for _, q := range []artifactsdomain.ListQuery{
		{Prefix: "Drafts/", Delimiter: "/"}, {Prefix: "Nothing/", Delimiter: "/"},
		{Folder: g.drafts.ID}, {Folder: artifactsdomain.FolderIDOf("none", 0).String()},
	} {
		if _, err := g.n.reader.List(t.Context(), g.bob, q); !errors.Is(err, artifactsdomain.ErrFolderNotFound) {
			t.Errorf("bob listing %+v: %v, want no such folder", q, err)
		}
	}
	for _, q := range []artifactsdomain.ListQuery{
		{Prefix: "Marketing"}, {Delimiter: "|"}, {Prefix: "Marketing/", Folder: g.marketing.ID},
	} {
		if _, err := g.n.reader.List(t.Context(), g.bob, q); !errors.Is(err, artifactsdomain.ErrInvalid) {
			t.Errorf("listing %+v: %v, want refused", q, err)
		}
	}
}

// A TOKEN CONTINUES THE FOLDER IT WAS MINTED IN, BY ID: a rename between two
// pages does not move the next one, the page echoes the folder's path as it is
// now, and a token never continues a listing of another shape.
func TestATokenBindsTheFolderNotItsPath(t *testing.T) {
	t.Parallel()
	g := newGallery(t)
	q := artifactsdomain.ListQuery{Prefix: "Marketing/", MaxKeys: 1, Sort: artifactsdomain.SortTitle}
	first := g.list(g.bob, q)
	if len(first.Contents) != 1 || !first.IsTruncated || first.NextContinuationToken == "" {
		t.Fatalf("the first page: %+v", first)
	}
	if _, err := g.n.writer.RenameFolder(t.Context(), g.janeCall, g.marketing.ID, "Brand"); err != nil {
		t.Fatal(err)
	}
	q.ContinuationToken = first.NextContinuationToken
	next := g.list(g.bob, q)
	if len(next.Contents) != 1 || next.Contents[0].ID == first.Contents[0].ID || next.Prefix != "Brand/" {
		t.Errorf("the page after the rename: %+v", next)
	}
	q.Delimiter = "/"
	if _, err := g.n.reader.List(t.Context(), g.bob, q); !errors.Is(err, artifactsdomain.ErrBadCursor) {
		t.Errorf("a recursive token continuing a one-level listing: %v", err)
	}
	// A TOKEN NEVER LISTS ITS FOLDER UNDER ANOTHER FOLDER'S WORDS: a query
	// naming a different folder beside it, by id or by a path that names
	// one now, is refused rather than answered as the token's folder.
	other := q
	other.Delimiter = ""
	other.Prefix, other.Folder = "", g.drafts.ID
	if _, err := g.n.reader.List(t.Context(), g.jane, other); !errors.Is(err, artifactsdomain.ErrBadCursor) ||
		!strings.Contains(err.Error(), "another folder") {
		t.Errorf("a token sent with another folder's id: %v", err)
	}
	other.Folder, other.Prefix = "", "Drafts/"
	if _, err := g.n.reader.List(t.Context(), g.jane, other); !errors.Is(err, artifactsdomain.ErrBadCursor) ||
		!strings.Contains(err.Error(), "another folder") {
		t.Errorf("a token sent with another folder's path: %v", err)
	}
}

// THE REQUESTER NARROWS EVERYTHING THE PAGE DISCLOSES: listing for jane on
// bob's behalf names no folder bob cannot see (Drafts is no common prefix and
// inDrafts names no path), and holds and counts nothing bob may not open.
func TestTheRequesterNarrowsEveryFolderAndRow(t *testing.T) {
	t.Parallel()
	g := newGallery(t)
	bob := g.bob
	page := g.list(g.jane, artifactsdomain.ListQuery{Delimiter: "/", Requester: &bob})
	if got := idsOf(page); !slices.Equal(got, sortedIDs(g.top, g.inDrafts)) {
		t.Errorf("jane's top level for bob holds %v", got)
	}
	if len(page.CommonPrefixes) != 1 || page.CommonPrefixes[0].Name != "Marketing" ||
		page.CommonPrefixes[0].Items != 1 {
		t.Errorf("jane's common prefixes for bob are %+v", page.CommonPrefixes)
	}
	if _, err := g.n.reader.List(t.Context(), g.jane, artifactsdomain.ListQuery{Prefix: "Drafts/",
		Requester: &bob}); !errors.Is(err, artifactsdomain.ErrFolderNotFound) {
		t.Errorf("jane listing her own Drafts for bob: %v", err)
	}
	if _, err := g.n.reader.ResolveFolder(t.Context(), "Drafts", []artifactsdomain.Viewer{g.jane, bob},
		statelog.Freshness{}); !errors.Is(err, artifactsdomain.ErrFolderNotFound) {
		t.Errorf("resolving Drafts for jane and bob: %v", err)
	}
}

// A FOLDER GRANTS NOTHING TO ITS CONTENTS: jane's unshared artifact filed in
// the company's Marketing is not shown to bob, however he reaches Marketing
// (its listing, everything beneath it, its count), and an artifact shared with
// bob filed in jane's own Drafts is still his to open, at his top level. Who
// may open an artifact is its owner and its own shares, and nothing else.
//
// Mutation: admit an artifact by its folder's audience in the listing's
// predicate and bob is shown jane's hidden artifact in Marketing.
func TestAFolderGrantsNothingToItsContents(t *testing.T) {
	t.Parallel()
	g := newGallery(t)
	for _, q := range []artifactsdomain.ListQuery{
		{Prefix: "Marketing/", Delimiter: "/"}, {Prefix: "Marketing/"}, {},
	} {
		if page := g.list(g.bob, q); slices.Contains(idsOf(page), g.hidden) {
			t.Errorf("bob listing %+v is shown jane's unshared artifact", q)
		}
	}
	if h := g.n.head(g.hidden); len(h.Shares) != 0 || h.Folder != g.marketing.ID {
		t.Errorf("filing it changed its shares to %v", h.Shares)
	}
	if page := g.list(g.bob, artifactsdomain.ListQuery{}); !slices.Contains(idsOf(page), g.inDrafts) {
		t.Error("an artifact shared with bob is hidden from him by the folder it is filed in")
	}
}

// A BREADCRUMB NEVER NAMES A HIDDEN ANCESTOR: a folder is seen through its
// top-level folder, so every folder a reader is answered comes after its
// parent, every one of its ancestors is answered too, and every path a row or
// a prefix names is the whole path from the top level. Bob is answered none of
// jane's own tree, whatever he owns under the company's.
//
// Mutation: decide a subfolder's visibility on its own owner and bob, who owns
// a folder moved into jane's own tree, is answered it under a parent he is
// not.
func TestABreadcrumbNeverNamesAHiddenAncestor(t *testing.T) {
	t.Parallel()
	g := newGallery(t)
	bobsCall := person("bob", artifacts.RoleOwner)
	bobs := g.n.mkdir(bobsCall, g.q3.ID, "", "Bob's notes")[0]
	if _, err := g.n.writer.MoveFolder(t.Context(), g.janeCall, bobs.ID, g.drafts.ID); err != nil {
		t.Fatal(err)
	}
	for _, v := range []artifactsdomain.Viewer{g.bob, g.jane} {
		tree, err := g.n.reader.Folders(t.Context(), v, statelog.Freshness{})
		if err != nil {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		for _, f := range tree.Folders {
			if f.Parent != "" && !seen[f.Parent] {
				t.Errorf("%s is answered %s before its parent, or without it", v.Record, f.Name)
			}
			seen[f.ID] = true
			if v.Record == "bob" && (f.ID == bobs.ID || f.ID == g.drafts.ID) {
				t.Errorf("bob is answered %s, in jane's own tree", f.Name)
			}
		}
	}
	page := g.list(g.jane, artifactsdomain.ListQuery{Prefix: "Drafts/", Delimiter: "/"})
	if len(page.CommonPrefixes) != 1 || page.CommonPrefixes[0].Prefix != "Drafts/Bob's notes/" {
		t.Errorf("jane's Drafts answers %+v", page.CommonPrefixes)
	}
}

// A FOLDER'S COUNT HOLDS ONLY WHAT THE READER MAY SEE: the tree read counts
// only what bob may open, by the listing's own predicate, and puts inDrafts
// among the artifacts in no folder he sees; jane is answered her own tree and
// her own count.
//
// Mutation: count a folder's artifacts from the rows alone and bob's
// Marketing counts the hidden artifact.
func TestAFolderCountHoldsOnlyWhatTheReaderMaySee(t *testing.T) {
	t.Parallel()
	g := newGallery(t)
	tree, err := g.n.reader.Folders(t.Context(), g.bob, statelog.Freshness{})
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, f := range tree.Folders {
		counts[f.Name] = f.Artifacts
	}
	if len(tree.Folders) != 2 || counts["Marketing"] != 1 || counts["Q3"] != 1 || tree.TopLevel != 2 {
		t.Errorf("bob's tree is %+v, top level %d", tree.Folders, tree.TopLevel)
	}
	mine, err := g.n.reader.Folders(t.Context(), g.jane, statelog.Freshness{})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range mine.Folders {
		names = append(names, f.Name)
	}
	if !slices.Equal(names, []string{"Drafts", "Marketing", "Q3"}) || mine.TopLevel != 1 {
		t.Errorf("jane's tree is %v, top level %d", names, mine.TopLevel)
	}
}

// THE TOTAL EQUALS THE ROWS THE VIEWER MAY SEE: the listing's one predicate
// and the authority table agree on every artifact of a company that holds
// every kind of relation, a share at a role this build does not know among
// them (which the table grants nothing), so the total never counts a row the
// table withholds.
//
// Mutation: drop the role restriction from sharedClause and the total counts
// the share written at a role off the ladder.
func TestTheTotalEqualsTheRowsTheViewerMaySee(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	jane, bob := person("jane", artifacts.RoleOwner), person("bob", artifacts.RoleOwner)
	scribe := agent("agent-scribe", artifacts.RoleOwner)
	share := func(c artifactsdomain.Call, id artifacts.ID, a artifacts.Audience) {
		t.Helper()
		if _, err := n.writer.Share(t.Context(), c, id, artifacts.Share{Audience: a, Role: artifacts.RoleViewer}); err != nil {
			t.Fatal(err)
		}
	}
	own := n.createIn(bob, "Bob's own", "")
	toBob := n.createIn(jane, "To bob", "")
	share(jane, toBob, artifacts.Audience{Kind: artifacts.AudienceSeat, ID: "bob"})
	toEng := n.createIn(jane, "To ENG", "")
	share(jane, toEng, artifacts.Audience{Kind: artifacts.AudienceUnit, ID: "ENG"})
	toOps := n.createIn(jane, "To OPS", "")
	share(jane, toOps, artifacts.Audience{Kind: artifacts.AudienceUnit, ID: "OPS"})
	toAll := n.createIn(jane, "To everyone", "")
	share(jane, toAll, artifacts.Audience{Kind: artifacts.AudienceCompany})
	toSam := n.createIn(jane, "To sam", "")
	share(jane, toSam, artifacts.Audience{Kind: artifacts.AudienceSeat, ID: "sam"})
	led := n.createIn(scribe, "Scribe's", "")
	unknown := n.createIn(jane, "At a role from the future", "")
	// A SHARE A NEWER PEER WROTE, at a role this build does not know: the
	// writer refuses to form one, so it is written straight into the
	// estate, as an applier of that newer build would have.
	if err := n.db.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `
			INSERT INTO artifact_shares (artifact_id, audience_kind, audience_id, role, granted_by,
				granted_by_kind, granted_at) VALUES (?, 'company', '', 'steward', 'jane', 'human', ?)`,
			unknown.String(), store.EncodeTime(monday))
		return err
	}); err != nil {
		t.Fatal(err)
	}

	viewer := artifactsdomain.Viewer{Record: "bob", Units: []string{"ENG"}, LedOwners: []string{"agent-scribe"}}
	page, err := n.reader.List(t.Context(), viewer, artifactsdomain.ListQuery{MaxKeys: artifactsdomain.MaxListLimit})
	if err != nil {
		t.Fatal(err)
	}
	p := iam.Principal{Login: "bob.person", Kind: iam.KindPerson, Seat: "bob", Stage: iam.StageActive,
		Grants: []iam.Grant{iam.GrantArtifacts}}
	shown := 0
	for _, id := range []artifacts.ID{own, toBob, toEng, toOps, toAll, toSam, led, unknown} {
		h := n.head(id)
		visible, err := authz.Visible(t.Context(), p, authz.Object{Kind: authz.KindArtifact, ID: id.String(),
			Owner: h.Owner, Shares: h.Shares, OwnerIsAgent: h.OwnerKind == iam.ActorAgent},
			listingChart{}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if listed := slices.Contains(idsOf(page), id); listed != visible {
			t.Errorf("%s: listed %v, and the authority table shows it: %v", h.Title, listed, visible)
		}
		if visible {
			shown++
		}
	}
	if page.Total != shown || len(page.Contents) != shown || shown != 5 {
		t.Errorf("the listing totals %d over %d rows, and the table shows %d of the artifacts", page.Total,
			len(page.Contents), shown)
	}
}

// listingChart seats bob in ENG and makes him scribe's lead, and says nothing
// else.
type listingChart struct{}

func (listingChart) Leads(_ context.Context, actor, subject string) (bool, error) {
	return actor == "bob" && subject == "agent-scribe", nil
}
func (listingChart) LeadsProject(context.Context, string, string) (bool, error)   { return false, nil }
func (listingChart) LeadsUnit(context.Context, string, string) (bool, error)      { return false, nil }
func (listingChart) LeadsContainer(context.Context, string, string) (bool, error) { return false, nil }
func (listingChart) LeadsAnyone(_ context.Context, actor string) (bool, error) {
	return actor == "bob", nil
}
func (listingChart) MemberOf(_ context.Context, actor, unit string) (bool, error) {
	return actor == "bob" && unit == "ENG", nil
}
func (listingChart) SeatGranted(context.Context, string, authz.SeatGrant) (bool, error) {
	return false, nil
}
