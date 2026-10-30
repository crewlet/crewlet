package artifactsdomain_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
)

// A REFUSAL ABOUT THE ARTIFACT ITSELF IS TOLD WITHOUT ITS ID, AND LOGGED WITH IT.
//
// Not there, in the trash, being purged, purged and moved past a publish's
// base each name the artifact by id in their error, which is the log's; a
// person read "the artifact was purged: 0192f…" and "artifact 0192f… is at
// version 4 and this publish was based on an older one" on the screen of the
// artifact they meant. What else such a refusal says stays — a version that
// is not kept is named by its number. Mutations: drop a sentinel from
// aboutTheArtifact, or answer the moved head's Error, and its row goes red;
// cut the id from the error itself and the log rows do.
func TestARefusalAboutTheArtifactIsToldWithoutItsID(t *testing.T) {
	t.Parallel()
	id := artifacts.IDFor("binned")
	moved := &artifactsdomain.VersionMoved{Head: artifactsdomain.Head{ID: id, HeadNumber: 4}}
	for _, tc := range []struct {
		name string
		err  error
		says string
	}{
		{"not there", fmt.Errorf("%w: %s", artifactsdomain.ErrNotFound, id), "no such artifact"},
		{"in the trash", fmt.Errorf("%w: %s", artifactsdomain.ErrTrashed, id),
			"the artifact is in the trash"},
		{"purged", fmt.Errorf("%w: %s", artifactsdomain.ErrPurged, id),
			"the artifact was purged"},
		{"being purged", fmt.Errorf("%w: %s", artifactsdomain.ErrPurging, id),
			strings.TrimPrefix(artifactsdomain.ErrPurging.Error(), "artifactsdomain: ")},
		{"wrapped by a caller", fmt.Errorf("publish: %w",
			fmt.Errorf("%w: %s", artifactsdomain.ErrPurged, id)),
			"publish: the artifact was purged"},
		{"moved past the base", moved, "this artifact is at version 4, and this " +
			"publish was based on an older one. Merge the change onto version 4 " +
			"and publish again based on it"},
	} {
		said := artifactsdomain.Sentence(tc.err)
		if said != tc.says {
			t.Errorf("%s: a person is told %q, want %q", tc.name, said, tc.says)
		}
		if !strings.Contains(tc.err.Error(), id.String()) {
			t.Errorf("%s: the log is told %q, which lost the id", tc.name, tc.err)
		}
	}
	// TEXT THAT ONLY LOOKS LIKE A WRAP IS LEFT: no id follows the words.
	other := fmt.Errorf("%w: kind %q is not one of the kinds", artifactsdomain.ErrInvalid, "x")
	if said := artifactsdomain.Sentence(other); said != `kind "x" is not one of the kinds` {
		t.Errorf("a refusal with no id after its words is told %q", said)
	}
}

// A REFUSAL THAT SAYS MORE THAN ITS SENTINEL IS TOLD AS WHAT IT SAYS, AND
// NEVER BY THE ARTIFACT'S ID.
//
// A version the artifact does not keep, an asset it does not hold and a
// restore of an artifact that is not in the trash each read "artifact 0192f…
// keeps no version 9" and the like to the person on that artifact's screen,
// and the first two under "no such artifact", which the sentence contradicts.
// Each now names the artifact by id right after its sentinel for the log and
// is told as its own sentence. Walked through the real reader and writer.
// Mutations: name the artifact in the sentence again, or keep the sentinel's
// words before it, and that row goes red; cut the id from the error itself
// and the log check does.
func TestARefusalSayingMoreIsToldWithoutTheArtifactsID(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	jane := person("jane", artifacts.RoleOwner)
	id := n.create(jane, "Kept", upload(t, "index.html", "<p>hi</p>")).Artifact
	_, notKept := n.reader.Version(t.Context(), id, 9)
	asset := uuid.New()
	_, notHeld := n.writer.RemoveAsset(t.Context(), jane, id, asset)
	// THE APPLIER'S VERDICT on a restore that met a live artifact: the
	// writer's decide makes nothing of one it can see is live already.
	notTrashed := artifactsdomain.RefusalError(t.Context(), &artifactsdomain.Writer{},
		id.String(), artifactsdomain.RefusedLive, artifactsdomain.HistorySummary{}, 0)
	// AND ITS VERDICT ON A LABEL FOR A VERSION THE ARTIFACT NO LONGER KEEPS,
	// which a record meets when the version goes between its decide and its
	// apply: told as the decide tells the same fact, never as "no such
	// artifact" about the artifact the person is looking at.
	labelGone := artifactsdomain.RefusalError(t.Context(), &artifactsdomain.Writer{},
		id.String(), artifactsdomain.RefusedVersionNotFound, artifactsdomain.HistorySummary{}, 7)
	// THE ASSET STORE AT ITS CAPS, seeded in the rows a real writer then
	// decides on: one artifact holding its count of assets, another its
	// bytes. And an upload another file already names, whose object key the
	// store minted and nobody reading the refusal chose.
	full := n.create(jane, "Full", upload(t, "index.html", "<p>full</p>")).Artifact
	heavy := n.create(jane, "Heavy", upload(t, "index.html", "<p>heavy</p>")).Artifact
	n.seedAssets(full, artifacts.MaxAssetsPerArtifact, 1)
	n.seedAssets(heavy, 1, artifacts.MaxAssetBytesPerArtifact)
	logo := upload(t, "logo.png", "\x89PNG bytes")
	_, _, atCount := n.writer.AddAsset(t.Context(), jane, artifactsdomain.AssetInput{
		Artifact: full, Name: "logo.png", Object: logo.Object})
	_, _, atBytes := n.writer.AddAsset(t.Context(), jane, artifactsdomain.AssetInput{
		Artifact: heavy, Name: "logo.png", Object: logo.Object})
	named := upload(t, "index.html", "<p>hi</p>")
	_, _, elsewhere := n.writer.AddAsset(t.Context(), jane, artifactsdomain.AssetInput{
		Artifact: id, Name: "copy.html", Object: named.Object})
	_, restoreMissing := n.writer.Publish(t.Context(), jane, artifactsdomain.PublishInput{
		Artifact: id, Base: 1, RestoredFrom: 9})
	for _, tc := range []struct {
		name string
		err  error
		says string
		of   artifacts.ID
	}{
		{"an asset store at its count", atCount, "this artifact holds 1000 assets, " +
			"the most an asset store keeps", full},
		{"an asset store at its bytes", atBytes, "this asset would take this " +
			"artifact's assets over 1 GiB, the most they can hold", heavy},
		{"an upload another file names", elsewhere, "the upload this asset names is " +
			"already used by another file, so upload its bytes again", id},
		{"a restore from a version it does not keep", restoreMissing,
			"this artifact doesn't keep version 9", id},
	} {
		if tc.err == nil {
			t.Errorf("%s was not refused", tc.name)
			continue
		}
		said := artifactsdomain.Sentence(tc.err)
		if said != tc.says || strings.Contains(said, tc.of.String()) ||
			strings.Contains(said, named.Object.Key.String()) {
			t.Errorf("%s: a person is told %q, want %q", tc.name, said, tc.says)
		}
		if !strings.Contains(tc.err.Error(), tc.of.String()) {
			t.Errorf("%s: the log is told %q, which lost the id", tc.name, tc.err)
		}
	}
	for _, tc := range []struct {
		name string
		err  error
		says string
	}{
		{"a version it does not keep", notKept, "this artifact doesn't keep version 9"},
		{"an asset it does not hold", notHeld, "this artifact holds no asset " + asset.String()},
		{"a restore of a live artifact", notTrashed,
			"this artifact isn't in the trash, so there's nothing to restore"},
		{"a label for a version gone before its apply", labelGone,
			"this artifact doesn't keep version 7"},
	} {
		if tc.err == nil {
			t.Errorf("%s was not refused", tc.name)
			continue
		}
		if said := artifactsdomain.Sentence(tc.err); said != tc.says {
			t.Errorf("%s: a person is told %q, want %q", tc.name, said, tc.says)
		}
		if !strings.Contains(tc.err.Error(), id.String()) {
			t.Errorf("%s: the log is told %q, which lost the id", tc.name, tc.err)
		}
	}
}
