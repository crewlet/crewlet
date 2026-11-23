package artifactupload_test

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/manifest"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/artifactupload"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// A REFUSED PUBLISH DROPS THE GENERATED FILES IT DELETED from the upload's
// record: the upload opens again for another publish, and one refused after
// another would otherwise pile its linted pages and CDN copies onto the record
// past what its key holds (coord.MaxUploadRecordBytes). Three refusals in a
// row leave the record holding the uploaded page alone.
//
// Mutation: mark the deleted files fileDeleted in fail rather than drop them,
// and the record holds every refusal's generated files.
func TestARefusedPublishDropsTheGeneratedFilesItDeleted(t *testing.T) {
	t.Parallel()
	h := newHarness(t, options{})
	created := h.create(h.person("ada", h.opKey("create")), map[string]string{"index.html": "<p>v1</p>"})
	if _, _, err := h.svc.Publish(t.Context(), h.person("ada", h.opKey("label")), artifactupload.PublishRequest{
		Artifact: created.Artifact, Base: 1, Mode: manifest.ModePatch, Label: "release",
		Files: []artifactupload.FileSpec{{Path: "notes.txt", Inline: []byte("v2 notes")}},
	}); err != nil {
		t.Fatal(err)
	}
	c := h.person("ada", h.opKey("upload"))
	up := h.upload(c, created.Artifact, map[string]string{
		"index.html": strings.ReplaceAll(page, "chart.js@4.4.1", "chart.js@4.4.2")})
	for try := range 3 {
		_, _, err := h.svc.Publish(t.Context(), h.person("ada", h.opKey(fmt.Sprintf("refused-%d", try))),
			artifactupload.PublishRequest{
				Artifact: created.Artifact, Base: 2, Mode: manifest.ModePatch, Upload: up, Label: "release",
				Files: []artifactupload.FileSpec{
					{Path: "index.html", Upload: true},
					{Path: "extra.txt", Inline: []byte(fmt.Sprintf("inline bytes %d", try))},
				},
			})
		if !errors.Is(err, artifactsdomain.ErrLabelTaken) {
			t.Fatalf("refusal %d answered %v, want the label taken", try, err)
		}
		files, _, err := artifactupload.UploadFiles(t.Context(), h.svc, up)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(files, []string{"engine:stored"}) {
			t.Errorf("after refusal %d the upload's record holds %v, want its uploaded page alone", try, files)
		}
	}
}

// A VERSION PAST ITS FILE COUNT IS REFUSED BEFORE A BYTE MOVES: the files a
// publish generates are recorded on its reservation before they are stored,
// so a request naming more than artifacts.MaxFilesPerVersion files would
// record and store them all before the manifest refused the version. It is
// refused as invalid, naming the bound, and nothing is stored.
//
// Mutation: drop stageProduced's count check, and the files are stored
// before the manifest refuses them.
func TestAVersionPastItsFileCountIsRefusedBeforeAByteMoves(t *testing.T) {
	t.Parallel()
	h := newHarness(t, options{})
	created := h.create(h.person("ada", h.opKey("create")), map[string]string{"index.html": "<p>v1</p>"})
	engine := metrics.Attrs{"transport": "engine"}
	before := h.meter.count(metrics.ArtifactsUploadBytes, engine)
	var files []artifactupload.FileSpec
	for i := range artifacts.MaxFilesPerVersion + 1 {
		files = append(files, artifactupload.FileSpec{Path: fmt.Sprintf("notes/%04d.txt", i),
			Inline: []byte(fmt.Sprintf("note %d", i))})
	}
	_, _, err := h.svc.Publish(t.Context(), h.person("ada", h.opKey("many")), artifactupload.PublishRequest{
		Artifact: created.Artifact, Base: 1, Mode: manifest.ModePatch, Files: files,
	})
	if !errors.Is(err, artifactupload.ErrInvalid) || !strings.Contains(err.Error(), "maximum is 511") {
		t.Fatalf("a publish of %d files answered %v, want it refused naming the 511 a version holds",
			len(files)+1, err)
	}
	if after := h.meter.count(metrics.ArtifactsUploadBytes, engine); after != before {
		t.Errorf("the refused publish stored %d bytes, want none", after-before)
	}
}

// AN UPLOAD RECORDS THE TYPE A FILE IS SERVED AS, never the one its client
// declared: the declared type is the client's string, of any length past its
// essence, and the record (and the presigned put that signs its type) holds
// the media table's, at most 30 bytes, which coord.MaxUploadRecordBytes
// counts.
//
// Mutation: keep the declared type in manifest.PlanCarry, and the record
// holds the client's 64 KiB.
func TestAnUploadRecordsTheTypeAFileIsServedAs(t *testing.T) {
	t.Parallel()
	h := newHarness(t, options{})
	body := "<p>typed</p>"
	req := declare(map[string]string{"index.html": body})
	req.Entries[0].ContentType = "text/html; charset=utf-8; x=" + strings.Repeat("y", 64<<10)
	intent, err := h.svc.Intend(t.Context(), h.person("ada", h.opKey("typed")), artifacts.ID{}, req)
	if err != nil {
		t.Fatalf("Intend: %v", err)
	}
	_, types, err := artifactupload.UploadFiles(t.Context(), h.svc, intent.Upload)
	if err != nil {
		t.Fatal(err)
	}
	if got := types["index.html"]; got != "text/html; charset=utf-8" {
		t.Errorf("the upload records index.html as %d bytes of type, want the served text/html; charset=utf-8",
			len(got))
	}
}

// A RETRY REFUSED AS MOVED LEAVES WHAT AN EARLIER ATTEMPT LANDED: a publish
// whose version landed and whose answer was lost is retried under its
// operation key, the head has moved past its base, and the retry is refused
// as moved before it stages a byte. The generated files the landed version
// names stay stored, since a refusal undoes only its own attempt's objects.
//
// Mutation: let fail delete every held generated file of the record, and the
// landed version's files are gone.
func TestARetryRefusedAsMovedLeavesWhatAnEarlierAttemptLanded(t *testing.T) {
	t.Parallel()
	h := newHarness(t, options{loseAnswer: true})
	created := h.create(h.person("ada", h.opKey("create")), map[string]string{"index.html": "<p>v1</p>"})
	c := h.person("ada", h.opKey("lost"))
	req := artifactupload.PublishRequest{Artifact: created.Artifact, Base: 1, Mode: manifest.ModePatch,
		Files: []artifactupload.FileSpec{{Path: "notes.txt", Inline: []byte("landed notes")}}}
	if _, _, err := h.svc.Publish(t.Context(), c, req); err == nil {
		t.Fatal("premise: the first attempt's answer was not lost")
	}
	_, _, err := h.svc.Publish(t.Context(), c, req)
	var moved *artifactsdomain.VersionMoved
	if !errors.As(err, &moved) {
		t.Fatalf("the retry answered %v, want it refused as moved", err)
	}
	if got := h.file(h.version(created.Artifact, 2), "notes.txt"); got != "landed notes" {
		t.Errorf("the landed version's notes read %q after the retry's refusal, want them as landed", got)
	}
}
