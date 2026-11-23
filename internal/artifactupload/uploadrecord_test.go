package artifactupload_test

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/manifest"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/artifactupload"
	"github.com/crewlet/crewlet/internal/objstore"
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
// as moved before it stages a byte. The generated file the landed version
// names stays stored, inline or beside an upload's file, through what may
// follow on the upload the refusal reopens: its window closing and a ledger
// change releasing it early, or another publish of it closing.
//
// Mutation: let fail delete every held generated file of the record, or keep
// another attempt's generated files on the record it reopens, and the landed
// version's notes are gone.
func TestARetryRefusedAsMovedLeavesWhatAnEarlierAttemptLanded(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		// upload carries an uploaded file beside the inline one.
		upload bool
		// then is what follows the refusal on the upload it reopened.
		then func(h *harness, id artifacts.ID, up string)
	}{
		{"inline", false, func(*harness, artifacts.ID, string) {}},
		{"with an upload, released once its window closes", true, func(h *harness, id artifacts.ID, _ string) {
			h.clock.advance(objstore.MaxPresignTerm + time.Minute)
			h.upload(h.person("ada", h.opKey("next")), id, map[string]string{"b.txt": "next"})
		}},
		{"with an upload, published again", true, func(h *harness, id artifacts.ID, up string) {
			if _, _, err := h.svc.Publish(h.t.Context(), h.person("ada", h.opKey("again")),
				artifactupload.PublishRequest{Artifact: id, Base: 2, Mode: manifest.ModePatch, Upload: up,
					Files: []artifactupload.FileSpec{{Path: "a.txt", Upload: true}}}); err != nil {
				h.t.Fatalf("publish the reopened upload again: %v", err)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, options{loseAnswer: true})
			created := h.create(h.person("ada", h.opKey("create")), map[string]string{"index.html": "<p>v1</p>"})
			req := artifactupload.PublishRequest{Artifact: created.Artifact, Base: 1, Mode: manifest.ModePatch,
				Files: []artifactupload.FileSpec{{Path: "notes.txt", Inline: []byte("landed notes")}}}
			if c.upload {
				req.Upload = h.upload(h.person("ada", h.opKey("upload")), created.Artifact,
					map[string]string{"a.txt": "uploaded"})
				req.Files = append(req.Files, artifactupload.FileSpec{Path: "a.txt", Upload: true})
			}
			k := h.person("ada", h.opKey("lost"))
			if _, _, err := h.svc.Publish(t.Context(), k, req); err == nil {
				t.Fatal("premise: the first attempt's answer was not lost")
			}
			_, _, err := h.svc.Publish(t.Context(), k, req)
			var moved *artifactsdomain.VersionMoved
			if !errors.As(err, &moved) {
				t.Fatalf("the retry answered %v, want it refused as moved", err)
			}
			c.then(h, created.Artifact, req.Upload)
			if got := h.file(h.version(created.Artifact, 2), "notes.txt"); got != "landed notes" {
				t.Errorf("the landed version's notes read %q, want them as landed", got)
			}
		})
	}
}
