package artifactsdomain_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// listedFor is the artifacts the admin path lists on work item item, by id.
func (n *node) listedFor(item string) []artifacts.ID {
	n.t.Helper()
	got, err := n.reader.List(n.t.Context(), artifactsdomain.Viewer{Admin: true},
		artifactsdomain.ListQuery{WorkItem: item})
	if err != nil {
		n.t.Fatalf("list for %s: %v", item, err)
	}
	var out []artifacts.ID
	for _, it := range got.Items {
		out = append(out, it.ID)
	}
	return out
}

// links is the history rows artifact id files under the link op.
func (n *node) links(id artifacts.ID) []artifactsdomain.HistoryEntry {
	n.t.Helper()
	page, err := n.reader.History(n.t.Context(), id, "", artifactsdomain.MaxListLimit)
	if err != nil {
		n.t.Fatalf("read %s's history: %v", id, err)
	}
	var out []artifactsdomain.HistoryEntry
	for _, e := range page.Entries {
		if e.Op == string(artifactsdomain.OpLinkWorkItem) {
			out = append(out, e)
		}
	}
	return out
}

// A LINK PUTS AN ARTIFACT THAT ALREADY EXISTS ON A WORK ITEM'S LISTING, ONCE.
// It writes one history row filed under the item and moves nothing else; a
// retry under the same key is the same operation, and a second link to an item
// the history already names (by a link, or by a create made for it) answers
// success and writes no second row.
//
// Mutation: drop linkedTo's check from the decide and the second link writes a
// second row; write the history row without the authority and the listing
// finds nothing.
func TestALinkListsAnExistingArtifactOnItsWorkItemOnce(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	jane := person("jane", artifacts.RoleOwner)
	id := n.create(jane, "Launch design", upload(t, "index.html", "<p>design</p>")).Artifact
	before := n.head(id)
	if got := n.listedFor("ENG-12"); len(got) != 0 {
		t.Fatalf("ENG-12 lists %v before any link", got)
	}

	keyed := jane
	keyed.OpKey = statelog.NewOpID(monday, "")
	if _, err := n.writer.LinkWorkItem(t.Context(), keyed, id, " ENG-12 "); err != nil {
		t.Fatalf("link: %v", err)
	}
	if got := n.listedFor("ENG-12"); !slices.Equal(got, []artifacts.ID{id}) {
		t.Fatalf("ENG-12 lists %v after the link, want %v", got, id)
	}
	rows := n.links(id)
	if len(rows) != 1 || rows[0].WorkItem != "ENG-12" || rows[0].Outcome != artifactsdomain.OutcomeApplied ||
		rows[0].Actor.Name != "jane" || rows[0].Origin != string(artifacts.OriginDashboard) {
		t.Fatalf("the link's history rows are %+v, want one applied row of jane's filed under ENG-12", rows)
	}
	after := n.head(id)
	if after.HeadNumber != before.HeadNumber || after.AccessEpoch != before.AccessEpoch ||
		!after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Errorf("a link moved the artifact: %+v, was %+v", after, before)
	}

	if _, err := n.writer.LinkWorkItem(t.Context(), keyed, id, "ENG-12"); err != nil {
		t.Errorf("the retry under the same key answered %v", err)
	}
	if _, err := n.writer.LinkWorkItem(t.Context(), jane, id, "ENG-12"); err != nil {
		t.Errorf("a second link to the same item answered %v", err)
	}
	if rows := n.links(id); len(rows) != 1 {
		t.Errorf("the item is linked by %d rows after a retry and a repeat, want 1", len(rows))
	}

	made := jane
	made.Authority.WorkItem = "ENG-13"
	madeFor := n.create(made, "Made for ENG-13", upload(t, "index.html", "<p>made</p>")).Artifact
	if _, err := n.writer.LinkWorkItem(t.Context(), jane, madeFor, "ENG-13"); err != nil {
		t.Fatalf("linking an artifact made for the item: %v", err)
	}
	if rows := n.links(madeFor); len(rows) != 0 {
		t.Errorf("a link to the item the artifact was made for wrote %+v", rows)
	}
}

// A LINK IS REFUSED BEFORE ANYTHING IS APPENDED where the writer can tell: an
// artifact that is not there, one in the trash, a work item that is no tracker
// key, and a call already made for another item, which is never silently
// relinked.
func TestALinkIsRefusedWhereItCouldNotList(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	jane := person("jane", artifacts.RoleOwner)
	id := n.create(jane, "Linkable", upload(t, "index.html", "<p>l</p>")).Artifact
	binned := n.create(jane, "Binned", upload(t, "index.html", "<p>b</p>")).Artifact
	if _, err := n.writer.Trash(t.Context(), jane, binned); err != nil {
		t.Fatalf("trash: %v", err)
	}
	charged := jane
	charged.Authority.WorkItem = "ENG-1"
	for name, c := range map[string]struct {
		call artifactsdomain.Call
		id   artifacts.ID
		item string
		want error
	}{
		"missing":      {jane, artifacts.IDFor("nothing"), "ENG-12", artifactsdomain.ErrNotFound},
		"trashed":      {jane, binned, "ENG-12", artifactsdomain.ErrTrashed},
		"no item":      {jane, id, "  ", artifactsdomain.ErrInvalid},
		"too long":     {jane, id, string(make([]byte, artifacts.MaxWorkItemBytes+1)), artifactsdomain.ErrInvalid},
		"another item": {charged, id, "ENG-2", artifactsdomain.ErrInvalid},
	} {
		if _, err := n.writer.LinkWorkItem(t.Context(), c.call, c.id, c.item); !errors.Is(err, c.want) {
			t.Errorf("%s: answered %v, want %v", name, err, c.want)
		}
	}
	if rows := n.links(id); len(rows) != 0 {
		t.Errorf("refused links wrote %+v", rows)
	}
}

// A LINK THE ROWS REFUSED LISTS THE ARTIFACT UNDER NOTHING. A link decided on a
// live artifact that a trash reached first is refused at the apply, and its
// history row names the item so the record says what was tried; once the
// artifact is restored, that refused row must not put it on the item's listing.
//
// Mutation: drop the outcome from the listing's work item clause and the
// restored artifact is listed on an item it was never linked to.
func TestALinkTheRowsRefusedListsTheArtifactUnderNothing(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	jane := person("jane", artifacts.RoleOwner)
	id := n.create(jane, "Raced", upload(t, "index.html", "<p>r</p>")).Artifact
	if _, err := n.writer.Trash(t.Context(), jane, id); err != nil {
		t.Fatalf("trash: %v", err)
	}
	n.drain()
	subject := artifactsdomain.ArtifactSubject(id)
	scope := artifactsdomain.ScopeSet{Subject: true}
	authority := suiteAuthority()
	authority.WorkItem = "ENG-12"
	spec := artifactsdomain.Domain{}.Stream()
	n.applyBody(spec, n.consumed+1, handRecord(t, subject, artifactsdomain.OpLinkWorkItem, "op-link",
		scope, artifactsdomain.LinkPayload{V: 1, Authority: authority}), monday)
	n.applyBody(spec, n.consumed+1, handRecord(t, subject, artifactsdomain.OpRestore, "op-restore",
		scope, artifactsdomain.StatusPayload{V: 1}), monday)

	rows := n.links(id)
	if len(rows) != 1 || rows[0].Outcome != artifactsdomain.OutcomeRefused ||
		rows[0].Reason != artifactsdomain.RefusedTrashed || rows[0].WorkItem != "ENG-12" {
		t.Fatalf("the raced link's rows are %+v, want one refused as trashed, naming ENG-12", rows)
	}
	if h := n.head(id); h.Status != artifactsdomain.StatusLive {
		t.Fatalf("the artifact is %s after its restore", h.Status)
	}
	if got := n.listedFor("ENG-12"); len(got) != 0 {
		t.Errorf("ENG-12 lists %v on the strength of a refused link", got)
	}
}

// EVERY HISTORY ROW IS FILED UNDER ONE OF [artifactsdomain.HistoryOps], and
// every one of them is written by some gesture: the list the dashboard words a
// row by and the trail narrows by is the list the applier writes.
//
// Mutation: drop an op from HistoryOps, or file a new history row under an op
// it does not name, and the comparison goes red.
func TestEveryHistoryRowIsFiledUnderAHistoryOp(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	ctx := t.Context()
	jane := person("jane", artifacts.RoleOwner)
	id := n.create(jane, "Every op", upload(t, "index.html", "<p>1</p>")).Artifact
	next := page(upload(t, "index.html", "<p>2</p>"))
	next.Artifact, next.Base = id, 1
	title := "Renamed"
	bob := artifacts.Audience{Kind: artifacts.AudienceSeat, ID: "bob"}
	steps := []func() error{
		func() error { _, err := n.writer.Publish(ctx, jane, next); return err },
		func() error {
			_, err := n.writer.Publish(ctx, jane, artifactsdomain.PublishInput{Artifact: id, Base: 2, RestoredFrom: 1})
			return err
		},
		func() error {
			_, err := n.writer.Patch(ctx, jane, id, artifactsdomain.Patch{Title: &title})
			return err
		},
		func() error {
			_, err := n.writer.Share(ctx, jane, id, artifacts.Share{Audience: bob, Role: artifacts.RoleViewer})
			return err
		},
		func() error { _, err := n.writer.Unshare(ctx, jane, id, bob); return err },
		func() error {
			asset, _, err := n.writer.AddAsset(ctx, jane, artifactsdomain.AssetInput{
				Artifact: id, Name: "logo.png", Object: upload(t, "logo.png", "png").Object})
			if err != nil {
				return err
			}
			_, err = n.writer.RemoveAsset(ctx, jane, id, asset.ID)
			return err
		},
		func() error { _, err := n.writer.LinkWorkItem(ctx, jane, id, "ENG-12"); return err },
		func() error {
			_, err := n.writer.ReadElevated(ctx, person("root", artifacts.RoleOwner), id, 3)
			return err
		},
		func() error { _, err := n.writer.Trash(ctx, jane, id); return err },
		func() error { _, err := n.writer.Restore(ctx, jane, id); return err },
		func() error { _, err := n.writer.Transfer(ctx, jane, id, "bob"); return err },
	}
	for i, step := range steps {
		if err := step(); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
	// A PURGE THAT BEGINS AND STOPS at a failing dependent leaves its first
	// step's row in the history it has not yet removed.
	n.useDependents(&fakeDependent{fail: errors.New("the datastore stopped part way")})
	admin := person("root", artifacts.RoleOwner)
	admin.Authority.Elevated = true
	if _, err := n.writer.Purge(ctx, admin, id); err == nil {
		t.Fatal("a purge whose dependent failed reported success")
	}

	trail, err := n.reader.Trail(ctx, artifactsdomain.TrailQuery{Limit: artifactsdomain.MaxListLimit})
	if err != nil {
		t.Fatal(err)
	}
	var written []artifactsdomain.OpKind
	for _, e := range trail.Entries {
		if op := artifactsdomain.OpKind(e.Op); !slices.Contains(written, op) {
			written = append(written, op)
		}
	}
	for _, op := range written {
		if !slices.Contains(artifactsdomain.HistoryOps, op) {
			t.Errorf("a history row is filed under %q, which HistoryOps does not name", op)
		}
	}
	for _, op := range artifactsdomain.HistoryOps {
		if !slices.Contains(written, op) {
			t.Errorf("HistoryOps names %q and no gesture wrote a row under it: wrote %v", op, written)
		}
	}
	if _, err := n.reader.Trail(ctx, artifactsdomain.TrailQuery{Op: artifactsdomain.OpPin}); !errors.Is(err, artifactsdomain.ErrInvalid) {
		t.Errorf("narrowing the trail to an op no history row carries answered %v", err)
	}
	if page, err := n.reader.Trail(ctx, artifactsdomain.TrailQuery{Op: "asset_create"}); err != nil || len(page.Entries) != 1 {
		t.Errorf("narrowing the trail to an asset's create answered %d rows (%v)", len(page.Entries), err)
	}
}
