package artifactsdomain_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/manifest"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A CREATE WRITES THE ARTIFACT, ITS FIRST VERSION, ITS OBJECTS AND THE COUNTERS
// IN ONE RECORD, and the id is the operation's.
func TestACreateWritesTheArtifactItsVersionAndItsObjectsInOneRecord(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	jane := person("jane", artifacts.RoleOwner)
	index, css := upload(t, "index.html", "<p>hi</p>"), upload(t, "site.css", "p{}")
	got := n.create(jane, "Quarterly report", index, css)

	head := n.head(got.Artifact)
	switch {
	case head.HeadNumber != 1, head.Owner != "jane", head.Status != artifactsdomain.StatusLive,
		head.Title != "Quarterly report", head.Label != got.Artifact.Label():
		t.Fatalf("the head reads %+v", head)
	case head.BytesHeld != index.Object.Size+css.Object.Size:
		t.Errorf("the head holds %d bytes, want %d", head.BytesHeld, index.Object.Size+css.Object.Size)
	}
	v, err := n.reader.Version(t.Context(), got.Artifact, 1)
	if err != nil {
		t.Fatalf("version 1: %v", err)
	}
	if v.VersionID != got.VersionID || len(v.Manifest.Files) != 2 || v.Manifest.Entry != "index.html" {
		t.Errorf("version 1 reads %+v", v)
	}
	held, err := n.reader.HeldObjects(t.Context(), got.Artifact,
		[]objstore.Hash{index.Object.Hash, css.Object.Hash})
	if err != nil || len(held) != 2 || held[index.Object.Hash] != index.Object {
		t.Errorf("the held objects read %v (%v)", held, err)
	}
	usage, err := n.reader.Usage(t.Context())
	if err != nil || usage.Artifacts != 1 || usage.Versions != 1 || usage.Bytes != head.BytesHeld {
		t.Errorf("usage reads %+v (%v)", usage, err)
	}
}

// A RETRIED CREATE NAMES THE ARTIFACT ITS FIRST ATTEMPT MADE: the id is a
// function of the operation, so the retry an unknown outcome asks for is
// answered from the ledger with the same artifact rather than creating a second.
//
// Mutation: mint the id per call instead of from the operation, and the retry
// answers an artifact nothing holds.
func TestARetriedCreateNamesTheArtifactItsFirstAttemptMade(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	jane := person("jane", artifacts.RoleOwner)
	jane.OpKey = statelog.NewOpID(monday, "")
	in := artifactsdomain.CreateInput{
		Kind: artifacts.KindHTML, Title: "Once", Owner: "jane", OwnerKind: iam.ActorHuman,
		First: page(upload(t, "index.html", "<p>once</p>")),
	}
	first, err := n.writer.Create(t.Context(), jane, in)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	again, err := n.writer.Create(t.Context(), jane, in)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if again.Artifact != first.Artifact || !again.Result.Collapsed {
		t.Fatalf("the retry answered %s (collapsed %v), want %s", again.Artifact,
			again.Result.Collapsed, first.Artifact)
	}
	if u, _ := n.reader.Usage(t.Context()); u.Artifacts != 1 {
		t.Errorf("the retry made a second artifact: %d", u.Artifacts)
	}
}

// A PUBLISH AGAINST A MOVED HEAD IS REFUSED WITH THE LIVE HEAD (412
// version_moved), carrying what a merge needs: the live version's manifest.
// A forced publish lands over it, marked forced.
//
// Mutation: drop the base check in Publish and the stale publish lands.
func TestAPublishAgainstAMovedHeadIsRefusedWithTheLiveHead(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	jane := person("jane", artifacts.RoleEditor)
	id := n.create(jane, "Doc", upload(t, "index.html", "<p>1</p>")).Artifact

	v2 := page(upload(t, "index.html", "<p>2</p>"))
	v2.Artifact, v2.Base = id, 1
	if _, err := n.writer.Publish(t.Context(), jane, v2); err != nil {
		t.Fatalf("publish 2: %v", err)
	}
	stale := page(upload(t, "index.html", "<p>bob's</p>"))
	stale.Artifact, stale.Base = id, 1
	_, err := n.writer.Publish(t.Context(), person("bob", artifacts.RoleEditor), stale)
	var moved *artifactsdomain.VersionMoved
	if !errors.As(err, &moved) {
		t.Fatalf("a publish on a stale base answered %v", err)
	}
	if moved.Head.HeadNumber != 2 || moved.Live.Number != 2 || len(moved.Live.Manifest.Files) != 1 {
		t.Errorf("the refusal carries head %d, live %+v", moved.Head.HeadNumber, moved.Live)
	}

	stale.Force = true
	forced, err := n.writer.Publish(t.Context(), person("bob", artifacts.RoleEditor), stale)
	if err != nil {
		t.Fatalf("forced publish: %v", err)
	}
	v, err := n.reader.Version(t.Context(), id, forced.Number)
	if err != nil || forced.Number != 3 || !v.Forced {
		t.Errorf("the forced publish made version %d forced=%v (%v)", forced.Number, v.Forced, err)
	}
}

// A CONCURRENT REPUBLISH IS REFUSED: two nodes publishing on one base contend at
// the broker on the artifact's subject, and exactly one lands; the other is told
// the live head.
func TestAConcurrentRepublishIsRefused(t *testing.T) {
	t.Parallel()
	log := openLog(t)
	a := newNode(t, log, openStore(t, "a.db"), "node-a")
	b := newNode(t, log, openStore(t, "b.db"), "node-b")
	jane := person("jane", artifacts.RoleEditor)
	id := a.create(jane, "Race", upload(t, "index.html", "<p>0</p>")).Artifact
	b.drain()

	start := make(chan struct{})
	errs := make(chan error, 2)
	for i, n := range []*node{a, b} {
		go func() {
			in := page(upload(t, "index.html", fmt.Sprintf("<p>%d</p>", i)))
			in.Artifact, in.Base = id, 1
			<-start
			_, err := n.writer.Publish(context.Background(), jane, in)
			errs <- err
		}()
	}
	close(start)
	var landed, refused int
	for range 2 {
		err := <-errs
		var moved *artifactsdomain.VersionMoved
		switch {
		case err == nil:
			landed++
		case errors.As(err, &moved):
			refused++
		default:
			t.Fatalf("a racing publish answered %v", err)
		}
	}
	if landed != 1 || refused != 1 {
		t.Fatalf("%d publishes landed and %d were refused, want one of each", landed, refused)
	}
	a.drain()
	if h := a.head(id); h.HeadNumber != 2 {
		t.Errorf("the head is %d, want 2", h.HeadNumber)
	}
}

// VERSION RETENTION KEEPS A HUNDRED UNLABELLED VERSIONS AND EVERY LABELLED ONE,
// the oldest unlabelled retired by the publish that pushes past the bound, and a
// label's removal retires what it pushes past it in the same record.
//
// Mutation: stop counting the new version in retention, or let it retire a
// labelled version, and a count goes wrong.
func TestRetentionKeepsAHundredUnlabelledVersionsAndEveryLabelledOne(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	jane := person("jane", artifacts.RoleOwner)
	id := n.create(jane, "Saves itself", upload(t, "index.html", "<p>1</p>")).Artifact
	label2 := "the second"
	for number := 2; number <= artifacts.VersionsKeptUnlabelled+3; number++ {
		in := page(upload(t, "index.html", fmt.Sprintf("<p>%d</p>", number)))
		in.Artifact, in.Base = id, number-1
		if number == 2 {
			in.Label = label2
		}
		if _, err := n.writer.Publish(t.Context(), jane, in); err != nil {
			t.Fatalf("publish %d: %v", number, err)
		}
	}
	versions := n.versionNumbers(id)
	head := artifacts.VersionsKeptUnlabelled + 3
	if !slices.Contains(versions, 2) {
		t.Errorf("the labelled version 2 was retired")
	}
	if slices.Contains(versions, 1) || slices.Contains(versions, 3) {
		t.Errorf("the oldest unlabelled versions survived: %v", versions[:5])
	}
	if len(versions) != artifacts.VersionsKeptUnlabelled+1 || versions[len(versions)-1] != head {
		t.Errorf("%d versions kept (newest %d), want %d unlabelled plus the label",
			len(versions), versions[len(versions)-1], artifacts.VersionsKeptUnlabelled)
	}
	// REMOVING THE LABEL makes version 2 the oldest unlabelled one, and the
	// patch retires it in the same record.
	if _, err := n.writer.Patch(t.Context(), jane, id, artifactsdomain.Patch{
		Label: &artifactsdomain.VersionLabel{Number: 2, Label: ""},
	}); err != nil {
		t.Fatalf("remove the label: %v", err)
	}
	if got := n.versionNumbers(id); slices.Contains(got, 2) || len(got) != artifacts.VersionsKeptUnlabelled {
		t.Errorf("after the label's removal %d versions are kept (2 kept: %v)", len(got), slices.Contains(got, 2))
	}
	usage, err := n.reader.Usage(t.Context())
	if err != nil || usage.Versions != int64(artifacts.VersionsKeptUnlabelled) {
		t.Errorf("usage counts %d versions (%v)", usage.Versions, err)
	}
}

// A LABEL ANOTHER VERSION HOLDS AND A FULL SET OF LABELS ARE TWO REFUSALS,
// because they send a caller to different places: a held label is a collision,
// which another label avoids at once, and every kept label in use is the
// artifact's own state, which no other label changes until one is removed. One
// sentinel answered both, so a caller told to choose another label for a full
// set chose one and was refused again.
//
// Mutation: refuse the full set with ErrLabelTaken in Publish or in Patch, or a
// held label with ErrLabelsFull, and a case goes red.
func TestAHeldLabelAndAFullSetOfLabelsAreTwoRefusals(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	jane := person("jane", artifacts.RoleOwner)
	id := n.create(jane, "Labelled", upload(t, "index.html", "<p>1</p>")).Artifact
	number := 1
	publish := func(label string) error {
		in := page(upload(t, "index.html", fmt.Sprintf("<p>%d</p>", number+1)))
		in.Artifact, in.Base, in.Label = id, number, label
		_, err := n.writer.Publish(t.Context(), jane, in)
		if err == nil {
			number++
		}
		return err
	}
	if err := publish("first"); err != nil {
		t.Fatalf("publish the first label: %v", err)
	}
	if err := publish("first"); !errors.Is(err, artifactsdomain.ErrLabelTaken) ||
		errors.Is(err, artifactsdomain.ErrLabelsFull) {
		t.Fatalf("a label version 2 holds was refused %v, want ErrLabelTaken alone", err)
	}
	for kept := 1; kept < artifacts.MaxLabels; kept++ {
		if err := publish(fmt.Sprintf("label %d", kept)); err != nil {
			t.Fatalf("publish label %d: %v", kept, err)
		}
	}
	err := publish("one too many")
	if !errors.Is(err, artifactsdomain.ErrLabelsFull) || errors.Is(err, artifactsdomain.ErrLabelTaken) {
		t.Fatalf("a label past the %d kept was refused %v, want ErrLabelsFull alone",
			artifacts.MaxLabels, err)
	}
	// THE PATCH meets the same two refusals: version 1 carries no label.
	_, err = n.writer.Patch(t.Context(), jane, id, artifactsdomain.Patch{
		Label: &artifactsdomain.VersionLabel{Number: 1, Label: "one too many"}})
	if !errors.Is(err, artifactsdomain.ErrLabelsFull) || errors.Is(err, artifactsdomain.ErrLabelTaken) {
		t.Fatalf("a patch labelling a version past the cap was refused %v, want ErrLabelsFull", err)
	}
	_, err = n.writer.Patch(t.Context(), jane, id, artifactsdomain.Patch{
		Label: &artifactsdomain.VersionLabel{Number: 1, Label: "first"}})
	if !errors.Is(err, artifactsdomain.ErrLabelTaken) || errors.Is(err, artifactsdomain.ErrLabelsFull) {
		t.Fatalf("a patch taking version 2's label was refused %v, want ErrLabelTaken", err)
	}
	// AND THE SAME REQUEST LANDS ONCE A LABEL IS REMOVED, which is what makes
	// the full set the artifact's state rather than the request's.
	if _, err := n.writer.Patch(t.Context(), jane, id, artifactsdomain.Patch{
		Label: &artifactsdomain.VersionLabel{Number: 2, Label: ""}}); err != nil {
		t.Fatalf("remove a label: %v", err)
	}
	if err := publish("one too many"); err != nil {
		t.Fatalf("the refused label, once a label was removed: %v", err)
	}
}

// versionNumbers is every version an artifact keeps, oldest first.
func (n *node) versionNumbers(id artifacts.ID) []int {
	n.t.Helper()
	var out []int
	cursor := ""
	for {
		page, err := n.reader.Versions(n.t.Context(), id, cursor, artifactsdomain.MaxListLimit)
		if err != nil {
			n.t.Fatalf("versions: %v", err)
		}
		for _, v := range page.Versions {
			out = append(out, v.Number)
		}
		if page.After == "" {
			break
		}
		cursor = page.After
	}
	slices.Sort(out)
	return out
}

// A ROLLBACK IS A NEW VERSION WHOSE EVERY ENTRY IS CARRIED, and it moves no
// byte: the objects are the restored version's, named once more.
func TestARollbackCarriesEveryEntry(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	jane := person("jane", artifacts.RoleEditor)
	first := upload(t, "index.html", "<p>good</p>")
	id := n.create(jane, "Rollback", first).Artifact
	bad := page(upload(t, "index.html", "<p>bad</p>"))
	bad.Artifact, bad.Base = id, 1
	if _, err := n.writer.Publish(t.Context(), jane, bad); err != nil {
		t.Fatalf("publish 2: %v", err)
	}
	before, _ := n.reader.Usage(t.Context())
	got, err := n.writer.Publish(t.Context(), jane, artifactsdomain.PublishInput{
		Artifact: id, Base: 2, RestoredFrom: 1,
	})
	if err != nil {
		t.Fatalf("roll back: %v", err)
	}
	v, err := n.reader.Version(t.Context(), id, got.Number)
	if err != nil || got.Number != 3 || v.RestoredFrom != 1 {
		t.Fatalf("the rollback made %+v (%v)", v, err)
	}
	if len(v.Manifest.Files) != 1 || !v.Manifest.Files[0].Carried ||
		v.Manifest.Files[0].Object != first.Object {
		t.Errorf("the rollback's files are %+v, want version 1's carried", v.Manifest.Files)
	}
	if after, _ := n.reader.Usage(t.Context()); after.Bytes != before.Bytes {
		t.Errorf("a rollback moved the held bytes from %d to %d", before.Bytes, after.Bytes)
	}
}

// A SHARE NEVER GRANTS ABOVE THE SHARER'S ROLE, AND AN AGENT NEVER SHARES WITH
// THE WHOLE COMPANY (D10).
//
// Mutation: drop either check in checkShare and the share lands.
func TestAShareIsHeldToTheSharersRoleAndAnAgentNeverSharesCompanyWide(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	id := n.create(person("jane", artifacts.RoleOwner), "Shared",
		upload(t, "index.html", "<p>s</p>")).Artifact
	_, err := n.writer.Share(t.Context(), person("ed", artifacts.RoleEditor), id, artifacts.Share{
		Audience: artifacts.Audience{Kind: artifacts.AudienceSeat, ID: "bob"}, Role: artifacts.RoleEditor,
	})
	if err != nil {
		t.Fatalf("an editor sharing editor: %v", err)
	}
	_, err = n.writer.Share(t.Context(), person("vic", artifacts.RoleViewer), id, artifacts.Share{
		Audience: artifacts.Audience{Kind: artifacts.AudienceSeat, ID: "eve"}, Role: artifacts.RoleEditor,
	})
	if !errors.Is(err, artifactsdomain.ErrAboveOwnRole) {
		t.Errorf("a viewer granting editor answered %v", err)
	}
	_, err = n.writer.Share(t.Context(), agent("agent-writer", artifacts.RoleOwner), id, artifacts.Share{
		Audience: artifacts.Audience{Kind: artifacts.AudienceCompany}, Role: artifacts.RoleViewer,
	})
	if !errors.Is(err, artifactsdomain.ErrAgentCompanyShare) {
		t.Errorf("an agent sharing company-wide answered %v", err)
	}
	_, err = n.writer.Share(t.Context(), person("jane", artifacts.RoleOwner), id, artifacts.Share{
		Audience: artifacts.Audience{Kind: artifacts.AudienceSeat, ID: "bob"}, Role: artifacts.RoleOwner,
	})
	if !errors.Is(err, artifactsdomain.ErrInvalid) {
		t.Errorf("sharing the owner role answered %v", err)
	}
	head := n.head(id)
	if len(head.Shares) != 1 || head.Shares[0].Audience.ID != "bob" ||
		head.Shares[0].Role != artifacts.RoleEditor || head.Shares[0].GrantedBy.Name != "ed" {
		t.Errorf("the shares read %+v", head.Shares)
	}
}

// TRASH IS A STAMP AND RESTORE LIFTS IT: the rows stay, a write to a trashed
// artifact is refused, and the source address stays held so nobody takes the
// address of something that may come back.
func TestTrashKeepsTheRowsAndTheAddress(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	jane := person("jane", artifacts.RoleOwner)
	in := artifactsdomain.CreateInput{
		Kind: artifacts.KindHTML, Title: "Sourced", Owner: "jane", OwnerKind: iam.ActorHuman,
		SourceKey: "dashboards/sales.html", First: page(upload(t, "index.html", "<p>s</p>")),
	}
	first, err := n.writer.Create(t.Context(), jane, in)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := n.writer.Trash(t.Context(), jane, first.Artifact); err != nil {
		t.Fatalf("trash: %v", err)
	}
	if h := n.head(first.Artifact); h.Status != artifactsdomain.StatusTrashed || h.TrashedBy.Name != "jane" {
		t.Errorf("the trashed head reads %+v", h)
	}
	retitle := "renamed"
	if _, err := n.writer.Patch(t.Context(), jane, first.Artifact,
		artifactsdomain.Patch{Title: &retitle}); !errors.Is(err, artifactsdomain.ErrTrashed) {
		t.Errorf("a patch of a trashed artifact answered %v", err)
	}
	in.First = page(upload(t, "index.html", "<p>other</p>"))
	if _, err := n.writer.Create(t.Context(), jane, in); !errors.Is(err, artifactsdomain.ErrSourceTaken) {
		t.Errorf("a second create on the trashed artifact's address answered %v", err)
	}
	if _, err := n.writer.Restore(t.Context(), jane, first.Artifact); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if h := n.head(first.Artifact); h.Status != artifactsdomain.StatusLive {
		t.Errorf("the restored head reads %s", h.Status)
	}
}

// A TRANSFER MOVES OWNERSHIP AND RELEASES THE ADDRESS: the new owner's kind is
// the chart's, and the previous owner may create at the address again.
func TestATransferMovesOwnershipAndReleasesTheAddress(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	jane := person("jane", artifacts.RoleOwner)
	in := artifactsdomain.CreateInput{
		Kind: artifacts.KindHTML, Title: "Handed over", Owner: "jane", OwnerKind: iam.ActorHuman,
		SourceKey: "handover.html", First: page(upload(t, "index.html", "<p>h</p>")),
	}
	got, err := n.writer.Create(t.Context(), jane, in)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := n.writer.Transfer(t.Context(), jane, got.Artifact, "agent-keeper"); err != nil {
		t.Fatalf("transfer: %v", err)
	}
	h := n.head(got.Artifact)
	if h.Owner != "agent-keeper" || h.OwnerKind != iam.ActorAgent || h.SourceKey != "" || h.AccessEpoch != 1 {
		t.Errorf("the transferred head reads owner %s/%s source %q epoch %d",
			h.Owner, h.OwnerKind, h.SourceKey, h.AccessEpoch)
	}
	in.First = page(upload(t, "index.html", "<p>again</p>"))
	if _, err := n.writer.Create(t.Context(), jane, in); err != nil {
		t.Errorf("the released address could not be claimed again: %v", err)
	}
}

// fakeDependent records the purge steps it was asked to run.
type fakeDependent struct {
	calls []string
	fail  error
}

func (f *fakeDependent) PurgeArtifactState(_ context.Context, _ artifactsdomain.Call, opID string,
	id artifacts.ID) error {
	f.calls = append(f.calls, opID+"|"+id.String())
	return f.fail
}

// A PURGE RUNS ITS DEPENDENTS FIRST, step by step, and lands the artifact's own
// record only after them: a dependent that fails leaves the artifact in place,
// and a retry under the same key finishes the gesture.
//
// Mutation: run the dependents after the artifact step and the failing
// dependent leaves a purged artifact with its data in place.
func TestAPurgeRunsItsDependentsFirst(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	dep := &fakeDependent{fail: errors.New("the datastore is not answering")}
	n.useDependents(dep)
	admin := person("root", artifacts.RoleOwner)
	admin.Authority.Elevated = true
	admin.OpKey = statelog.NewOpID(monday, "")
	id := n.create(person("jane", artifacts.RoleOwner), "Doomed",
		upload(t, "index.html", "<p>d</p>")).Artifact

	if _, err := n.writer.Purge(t.Context(), admin, id); err == nil {
		t.Fatal("a purge whose dependent failed reported success")
	}
	if h := n.head(id); h.ID != id {
		t.Fatal("the artifact went although its dependent's state did not")
	}
	dep.fail = nil
	if _, err := n.writer.Purge(t.Context(), admin, id); err != nil {
		t.Fatalf("the retried purge: %v", err)
	}
	if len(dep.calls) != 2 || dep.calls[0] != dep.calls[1] {
		t.Errorf("the dependent was asked %v, want the same step twice", dep.calls)
	}
	_, _, err := n.reader.Artifact(t.Context(), id, statelog.Freshness{Level: statelog.ReadSession})
	if !errors.Is(err, artifactsdomain.ErrPurged) {
		t.Errorf("reading the purged artifact answered %v", err)
	}
	// AND A THIRD RUN OF THE SAME GESTURE IS ITS OWN ANSWER, not "purged".
	if _, err := n.writer.Purge(t.Context(), admin, id); err != nil {
		t.Errorf("a repeat of the landed purge answered %v", err)
	}
}

// useDependents makes the node's writer purge through deps.
func (n *node) useDependents(deps ...artifactsdomain.Dependent) {
	n.t.Helper()
	rebuilt, err := artifactsdomain.NewWriter(artifactsdomain.WriterDeps{
		Publisher: n.publisher, DB: n.db.Reader(),
		Now: func() time.Time { return monday }, SeatKind: seatKinds,
		Dependents: func() []artifactsdomain.Dependent { return deps },
	})
	if err != nil {
		n.t.Fatalf("rebuild the writer: %v", err)
	}
	n.writer = rebuilt
}

// AN ASSET IS COUNTED AGAINST ITS ARTIFACT, AND ITS REMOVAL LEAVES A TOMBSTONE
// that names no object.
func TestAnAssetIsCountedAndItsRemovalLeavesATombstone(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	jane := person("jane", artifacts.RoleEditor)
	id := n.create(jane, "With assets", upload(t, "index.html", "<p>a</p>")).Artifact
	before := n.head(id).BytesHeld
	logo := upload(t, "logo.png", "\x89PNG bytes")
	asset, _, err := n.writer.AddAsset(t.Context(), jane, artifactsdomain.AssetInput{
		Artifact: id, Name: "logo.png", Object: logo.Object,
	})
	if err != nil {
		t.Fatalf("add the asset: %v", err)
	}
	if asset.ContentType != "image/png" {
		t.Errorf("the asset is served as %q", asset.ContentType)
	}
	if after := n.head(id).BytesHeld; after != before+logo.Object.Size {
		t.Errorf("the head holds %d bytes after the asset, want %d", after, before+logo.Object.Size)
	}
	if _, err := n.writer.RemoveAsset(t.Context(), jane, id, asset.ID); err != nil {
		t.Fatalf("remove the asset: %v", err)
	}
	assets, err := n.reader.Assets(t.Context(), id)
	if err != nil || len(assets) != 0 {
		t.Errorf("assets after the removal: %v (%v)", assets, err)
	}
	if after := n.head(id).BytesHeld; after != before {
		t.Errorf("the head holds %d bytes after the removal, want %d", after, before)
	}
	if _, err := n.writer.RemoveAsset(t.Context(), jane, id, asset.ID); !errors.Is(err, artifactsdomain.ErrNotFound) {
		t.Errorf("removing the removed asset answered %v", err)
	}
}

// MOVEMENTS ARE TOLD AFTER THE COMMIT, built from the rows each record wrote: a
// new version, a narrowing's epoch, a purge.
func TestMovementsAreToldAfterTheCommit(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	jane := person("jane", artifacts.RoleOwner)
	id := n.create(jane, "Moving", upload(t, "index.html", "<p>m</p>")).Artifact
	if _, err := n.writer.Trash(t.Context(), jane, id); err != nil {
		t.Fatalf("trash: %v", err)
	}
	var kinds []artifactsdomain.MoveKind
	for _, m := range n.moved {
		if m.Artifact != id {
			t.Errorf("a movement names %s", m.Artifact)
		}
		kinds = append(kinds, m.What)
	}
	if !slices.Equal(kinds, []artifactsdomain.MoveKind{artifactsdomain.MoveVersion, artifactsdomain.MoveTrash}) {
		t.Errorf("the movements are %v", kinds)
	}
	if last := n.moved[len(n.moved)-1]; last.AccessEpoch != 1 || last.Head != 1 {
		t.Errorf("the trash's movement reads %+v", last)
	}
}

// THE TRASH REAPER PURGES ONLY WHAT HAS BEEN TRASHED SINCE BEFORE ITS CUTOFF: a
// listing a moment old does not purge an artifact restored since, nor one
// trashed after the cutoff, and each refusal says so by name.
//
// Mutation: drop the guard from PurgeTrashed and the restored artifact is
// purged.
func TestTheTrashPurgeTakesOnlyWhatWasTrashedBeforeItsCutoff(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	jane := person("jane", artifacts.RoleOwner)
	old := n.create(jane, "Old", upload(t, "index.html", "<p>o</p>")).Artifact
	restored := n.create(jane, "Restored", upload(t, "index.html", "<p>r</p>")).Artifact
	for _, id := range []artifacts.ID{old, restored} {
		if _, err := n.writer.Trash(t.Context(), jane, id); err != nil {
			t.Fatalf("trash: %v", err)
		}
	}
	cutoff := time.Now().Add(time.Hour)
	listed, err := n.reader.TrashedBefore(t.Context(), cutoff, 10)
	if err != nil || len(listed) != 2 {
		t.Fatalf("the trash listed %v (%v)", listed, err)
	}
	if _, err := n.writer.Restore(t.Context(), jane, restored); err != nil {
		t.Fatalf("restore: %v", err)
	}
	reaper := artifactsdomain.Call{Authority: artifacts.Authority{
		Actor: iam.Actor{Name: "trash-reaper", Kind: iam.ActorSystem}, Role: artifacts.RoleOwner,
	}}
	if _, err := n.writer.PurgeTrashed(t.Context(), reaper, old,
		time.Now().Add(-time.Hour)); !errors.Is(err, artifactsdomain.ErrNotInTrash) {
		t.Errorf("a purge with a cutoff before the trash answered %v", err)
	}
	if _, err := n.writer.PurgeTrashed(t.Context(), reaper, restored, cutoff); !errors.Is(err, artifactsdomain.ErrNotInTrash) {
		t.Errorf("a purge of the restored artifact answered %v", err)
	}
	if _, err := n.writer.PurgeTrashed(t.Context(), reaper, old, cutoff); err != nil {
		t.Fatalf("purge the long-trashed artifact: %v", err)
	}
	if _, _, err := n.reader.Artifact(t.Context(), old, statelog.Freshness{Level: statelog.ReadSession}); !errors.Is(err, artifactsdomain.ErrPurged) {
		t.Errorf("the long-trashed artifact answered %v", err)
	}
	if h := n.head(restored); h.Status != artifactsdomain.StatusLive {
		t.Errorf("the restored artifact reads %s", h.Status)
	}
}

// A PURGE IS COMMITTED BEFORE ITS DATA GOES: once its first step lands, the
// artifact is in the trash, a restore is refused, and only a purge finishes
// it. A purge that stopped part way (its dependent failed after the artifact
// was committed) leaves an artifact nobody may bring back without its data,
// which the trash reaper nominates whatever its trash term and finishes.
//
// Mutation: drop the applier's purging refusal from a restore and the
// half-purged artifact comes back live; drop the begun purges from the trash
// listing and the reaper never finishes it.
func TestAPurgeIsCommittedBeforeItsDataGoes(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	dep := &fakeDependent{fail: errors.New("the datastore stopped part way")}
	n.useDependents(dep)
	jane := person("jane", artifacts.RoleOwner)
	admin := person("root", artifacts.RoleOwner)
	admin.Authority.Elevated = true
	admin.OpKey = statelog.NewOpID(monday, "")
	id := n.create(jane, "Half gone", upload(t, "index.html", "<p>h</p>")).Artifact

	if _, err := n.writer.Purge(t.Context(), admin, id); err == nil {
		t.Fatal("a purge whose dependent failed reported success")
	}
	h := n.head(id)
	if h.Status != artifactsdomain.StatusTrashed || h.PurgeBegunAt.IsZero() || h.AccessEpoch != 1 {
		t.Fatalf("the artifact a purge began on reads status %s, purge begun %v, epoch %d; "+
			"want trashed, stamped and narrowed", h.Status, h.PurgeBegunAt, h.AccessEpoch)
	}
	if _, err := n.writer.Restore(t.Context(), jane, id); !errors.Is(err, artifactsdomain.ErrPurging) {
		t.Fatalf("a restore of an artifact a purge began on answered %v", err)
	}
	if h := n.head(id); h.Status != artifactsdomain.StatusTrashed {
		t.Fatalf("the refused restore left the artifact %s", h.Status)
	}

	// THE REAPER NOMINATES IT although it went in after the cutoff, and its
	// guard, judged only where a purge is committed, does not refuse it.
	cutoff := time.Now().Add(-time.Hour) // the broker stamps the trash at the wall clock
	listed, err := n.reader.TrashedBefore(t.Context(), cutoff, 10)
	if err != nil || !slices.Equal(listed, []artifacts.ID{id}) {
		t.Fatalf("the trash listing before the trash went in: %v (%v), want the begun purge", listed, err)
	}
	dep.fail = nil
	reaper := artifactsdomain.Call{Authority: artifacts.Authority{
		Actor: iam.Actor{Name: "trash-reaper", Kind: iam.ActorSystem}, Role: artifacts.RoleOwner,
	}}
	if _, err := n.writer.PurgeTrashed(t.Context(), reaper, id, cutoff); err != nil {
		t.Fatalf("the reaper's purge of the begun purge: %v", err)
	}
	if _, _, err := n.reader.Artifact(t.Context(), id, statelog.Freshness{Level: statelog.ReadSession}); !errors.Is(err, artifactsdomain.ErrPurged) {
		t.Errorf("the finished purge's artifact answered %v", err)
	}
	if len(dep.calls) != 2 {
		t.Errorf("the dependent was asked %v, want the failed step and the reaper's", dep.calls)
	}
}

// THE LISTING SHOWS WHAT THE VIEWER CAN SEE: their own, what is shared with
// their seat, a unit they sit in or the company, and an agent they lead's.
func TestTheListingShowsWhatTheViewerCanSee(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	jane, bob := person("jane", artifacts.RoleOwner), person("bob", artifacts.RoleOwner)
	mine := n.create(jane, "Mine", upload(t, "index.html", "<p>1</p>")).Artifact
	toSeat := n.create(bob, "To jane", upload(t, "index.html", "<p>2</p>")).Artifact
	toUnit := n.create(bob, "To ENG", upload(t, "index.html", "<p>3</p>")).Artifact
	private := n.create(bob, "Bob's own", upload(t, "index.html", "<p>4</p>")).Artifact
	led := n.create(agent("agent-scribe", artifacts.RoleOwner), "Scribe's notes",
		upload(t, "index.html", "<p>5</p>")).Artifact
	share := func(id artifacts.ID, a artifacts.Audience) {
		if _, err := n.writer.Share(t.Context(), bob, id, artifacts.Share{Audience: a, Role: artifacts.RoleViewer}); err != nil {
			t.Fatalf("share: %v", err)
		}
	}
	share(toSeat, artifacts.Audience{Kind: artifacts.AudienceSeat, ID: "jane"})
	share(toUnit, artifacts.Audience{Kind: artifacts.AudienceUnit, ID: "ENG"})
	if _, err := n.writer.Pin(t.Context(), jane, toUnit, true); err != nil {
		t.Fatalf("pin: %v", err)
	}
	viewer := artifactsdomain.Viewer{Record: "jane", Units: []string{"ENG"}, LedOwners: []string{"agent-scribe"}}
	ids := func(q artifactsdomain.ListQuery) []artifacts.ID {
		t.Helper()
		page, err := n.reader.List(t.Context(), viewer, q)
		if err != nil {
			t.Fatalf("list %+v: %v", q, err)
		}
		var out []artifacts.ID
		for _, item := range page.Contents {
			out = append(out, item.ID)
		}
		slices.SortFunc(out, func(a, b artifacts.ID) int { return compareIDs(a, b) })
		return out
	}
	sorted := func(xs ...artifacts.ID) []artifacts.ID {
		slices.SortFunc(xs, func(a, b artifacts.ID) int { return compareIDs(a, b) })
		return xs
	}
	if got := ids(artifactsdomain.ListQuery{}); !slices.Equal(got, sorted(mine, toSeat, toUnit, led)) {
		t.Errorf("everything jane sees: %v", got)
	}
	if got := ids(artifactsdomain.ListQuery{Scope: artifactsdomain.ScopeMine}); !slices.Equal(got, sorted(mine)) {
		t.Errorf("jane's own: %v", got)
	}
	if got := ids(artifactsdomain.ListQuery{Scope: artifactsdomain.ScopeShared}); !slices.Equal(got, sorted(toSeat, toUnit)) {
		t.Errorf("shared with jane: %v", got)
	}
	if got := ids(artifactsdomain.ListQuery{Scope: artifactsdomain.ScopeAgents}); !slices.Equal(got, sorted(led)) {
		t.Errorf("agents' artifacts jane sees: %v", got)
	}
	if got := ids(artifactsdomain.ListQuery{Pinned: true}); !slices.Equal(got, sorted(toUnit)) {
		t.Errorf("jane's pins: %v", got)
	}
	admin, err := n.reader.List(t.Context(), artifactsdomain.Viewer{Admin: true}, artifactsdomain.ListQuery{})
	if err != nil || admin.Total != 5 {
		t.Errorf("the admin path lists %d (%v), want every artifact", admin.Total, err)
	}
	if slices.Contains(ids(artifactsdomain.ListQuery{}), private) {
		t.Error("bob's unshared artifact is listed for jane")
	}
	// A PAGE AT A TIME, the cursor carrying on where the last page ended.
	first, err := n.reader.List(t.Context(), viewer, artifactsdomain.ListQuery{MaxKeys: 2, Sort: artifactsdomain.SortTitle})
	if err != nil || len(first.Contents) != 2 || first.NextContinuationToken == "" || first.Total != 4 ||
		!first.IsTruncated || first.KeyCount != 2 {
		t.Fatalf("the first page: %d items, after %q, total %d (%v)", len(first.Contents), first.NextContinuationToken, first.Total, err)
	}
	rest, err := n.reader.List(t.Context(), viewer, artifactsdomain.ListQuery{
		MaxKeys: 2, Sort: artifactsdomain.SortTitle, ContinuationToken: first.NextContinuationToken,
	})
	if err != nil || len(rest.Contents) != 2 || rest.NextContinuationToken != "" || rest.IsTruncated ||
		rest.Contents[0].Title <= first.Contents[1].Title {
		t.Errorf("the second page: %+v (%v)", rest, err)
	}
}

// A WORK ITEM LISTS THE ARTIFACTS MADE FOR IT: a write stamped with the item
// it was charged to links its artifact there, whichever version it made, and
// the listing narrowed to the item shows those and nothing else, still only
// to a viewer who may see them.
func TestAWorkItemListsTheArtifactsMadeForIt(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	jane := person("jane", artifacts.RoleOwner)
	forItem := jane
	forItem.Authority.WorkItem = "CRW-412"
	made := n.create(forItem, "Made for the item", upload(t, "index.html", "<p>1</p>")).Artifact
	later := n.create(jane, "Published for it later", upload(t, "index.html", "<p>2</p>")).Artifact
	n.create(jane, "Unrelated", upload(t, "index.html", "<p>3</p>"))
	next := page(upload(t, "index.html", "<p>2b</p>"))
	next.Artifact, next.Base = later, 1
	if _, err := n.writer.Publish(t.Context(), forItem, next); err != nil {
		t.Fatalf("publish for the item: %v", err)
	}
	bob := person("bob", artifacts.RoleOwner)
	bob.Authority.WorkItem = "CRW-412"
	n.create(bob, "Bob's, for the item, unshared", upload(t, "index.html", "<p>4</p>"))

	list := func(v artifactsdomain.Viewer) []artifacts.ID {
		t.Helper()
		got, err := n.reader.List(t.Context(), v, artifactsdomain.ListQuery{WorkItem: "CRW-412"})
		if err != nil {
			t.Fatalf("list for the item: %v", err)
		}
		var out []artifacts.ID
		for _, item := range got.Contents {
			out = append(out, item.ID)
		}
		slices.SortFunc(out, compareIDs)
		return out
	}
	want := []artifacts.ID{made, later}
	slices.SortFunc(want, compareIDs)
	if got := list(artifactsdomain.Viewer{Record: "jane"}); !slices.Equal(got, want) {
		t.Errorf("jane sees %v on CRW-412, want %v", got, want)
	}
	if got := list(artifactsdomain.Viewer{Admin: true}); len(got) != 3 {
		t.Errorf("the admin path lists %d artifacts on CRW-412, want 3", len(got))
	}
	none, err := n.reader.List(t.Context(), artifactsdomain.Viewer{Record: "jane"},
		artifactsdomain.ListQuery{WorkItem: "CRW-9"})
	if err != nil || len(none.Contents) != 0 {
		t.Errorf("an item nothing was made for lists %d (%v)", len(none.Contents), err)
	}
}

// compareIDs orders artifact ids by their text.
func compareIDs(a, b artifacts.ID) int {
	switch {
	case a.String() < b.String():
		return -1
	case a.String() > b.String():
		return 1
	}
	return 0
}

// A NATIVE SHEET HOLDS NO FILE (D03): its content is its datastore, so its
// versions' manifests are empty and it holds no bytes, while an HTML artifact
// with no entry is refused before anything is published.
func TestANativeSheetHoldsNoFile(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	jane := person("jane", artifacts.RoleOwner)
	sheet, err := n.writer.Create(t.Context(), jane, artifactsdomain.CreateInput{
		Kind: artifacts.KindSheet, Title: "Review sheet", Owner: "jane", OwnerKind: iam.ActorHuman,
		First: artifactsdomain.PublishInput{Capabilities: []artifacts.Capability{artifacts.CapabilityDB}},
	})
	if err != nil {
		t.Fatalf("create a sheet: %v", err)
	}
	head := n.head(sheet.Artifact)
	if head.Kind != artifacts.KindSheet || head.BytesHeld != 0 ||
		!slices.Equal(head.Capabilities, []artifacts.Capability{artifacts.CapabilityDB}) {
		t.Errorf("the sheet reads %+v", head)
	}
	v, err := n.reader.Version(t.Context(), sheet.Artifact, 1)
	if err != nil || len(v.Manifest.Files) != 0 || v.Manifest.Entry != "" {
		t.Errorf("the sheet's first version is %+v (%v)", v, err)
	}
	_, err = n.writer.Create(t.Context(), jane, artifactsdomain.CreateInput{
		Kind: artifacts.KindHTML, Title: "Empty page", Owner: "jane", OwnerKind: iam.ActorHuman,
	})
	if !errors.Is(err, artifactsdomain.ErrInvalid) || !errors.Is(err, manifest.ErrInvalid) {
		t.Errorf("an HTML artifact with no entry answered %v, want the manifest's refusal "+
			"as the domain's", err)
	}
}

// NO CAPABILITY IS AN EMPTY SET ON THE WIRE, NEVER A MISSING FIELD: a head
// and a version that declare nothing answer `"capabilities": []`, so a reader
// (the CLI's `get` and `versions`, the dashboard's frame) tells a version
// whose capabilities were stripped from one whose answer lost the field.
func TestNoCapabilityIsAnEmptySetOnTheWire(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	page := n.create(person("jane", artifacts.RoleOwner), "Plain page",
		upload(t, "index.html", "<p>plain</p>")).Artifact
	v, err := n.reader.Version(t.Context(), page, 1)
	if err != nil {
		t.Fatalf("read version 1: %v", err)
	}
	for what, value := range map[string]any{"the head": n.head(page), "the version": v,
		"the summary": v.VersionSummary} {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("encode %s: %v", what, err)
		}
		if !bytes.Contains(raw, []byte(`"capabilities":[]`)) {
			t.Errorf("%s encodes no empty capability set: %s", what, raw)
		}
	}
}

// THE COMPANY TRAIL READS EVERY ARTIFACT'S HISTORY (D09): an administrator's
// elevated read of bob's private page is found company-wide, beside every
// other record, newest first in the log's order; narrowed to `read_elevated`
// it is the overrides alone; a page and its cursor walk the whole trail with
// nothing repeated or skipped; an op the log does not record and a cursor it
// never minted are refused. Mutation: drop the op filter, or resume a page
// at the cursor's row rather than after it, and this goes red.
func TestTheCompanyTrailReadsEveryArtifactsHistory(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	jane, bob := person("jane", artifacts.RoleOwner), person("bob", artifacts.RoleOwner)
	mine := n.create(jane, "Mine", upload(t, "index.html", "<p>1</p>")).Artifact
	private := n.create(bob, "Bob's own", upload(t, "index.html", "<p>2</p>")).Artifact
	ada := person("ada", artifacts.RoleOwner)
	ada.Authority.Elevated = true
	if _, err := n.writer.ReadElevated(t.Context(), ada, private, 1); err != nil {
		t.Fatalf("elevated read: %v", err)
	}

	all, err := n.reader.Trail(t.Context(), artifactsdomain.TrailQuery{})
	if err != nil {
		t.Fatal(err)
	}
	var walked []string
	for _, e := range all.Entries {
		walked = append(walked, e.Op+" "+e.Title)
	}
	if want := []string{"read_elevated Bob's own", "create Bob's own", "create Mine"}; !slices.Equal(walked, want) {
		t.Errorf("the company trail is %v, want %v", walked, want)
	}
	overrides, err := n.reader.Trail(t.Context(), artifactsdomain.TrailQuery{Op: artifactsdomain.OpReadElevated})
	if err != nil {
		t.Fatal(err)
	}
	if len(overrides.Entries) != 1 || overrides.Entries[0].Artifact != private ||
		overrides.Entries[0].Actor.Name != "ada" || !overrides.Entries[0].Elevated ||
		overrides.Entries[0].Owner != "bob" || overrides.Entries[0].Number != 1 {
		t.Errorf("the overrides are %+v", overrides.Entries)
	}

	var paged []artifacts.ID
	cursor := ""
	for range 4 {
		page, err := n.reader.Trail(t.Context(), artifactsdomain.TrailQuery{Cursor: cursor, Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range page.Entries {
			paged = append(paged, e.Artifact)
		}
		if cursor = page.After; cursor == "" {
			break
		}
	}
	if want := []artifacts.ID{private, private, mine}; !slices.Equal(paged, want) {
		t.Errorf("paging one row at a time walked %v, want %v", paged, want)
	}

	if _, err := n.reader.Trail(t.Context(), artifactsdomain.TrailQuery{Op: "glance"}); !errors.Is(err,
		artifactsdomain.ErrInvalid) {
		t.Errorf("an op the log does not record: %v, want ErrInvalid", err)
	}
	if _, err := n.reader.Trail(t.Context(), artifactsdomain.TrailQuery{Cursor: "not-a-cursor"}); !errors.Is(err,
		artifactsdomain.ErrBadCursor) {
		t.Errorf("a cursor nothing minted: %v, want ErrBadCursor", err)
	}
}
