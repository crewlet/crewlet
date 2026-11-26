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
// as moved before it stages a byte. The generated and the uploaded file the
// landed version names stay stored and reserved through what may follow on
// the upload the refusal reopened: its window closing and a ledger change
// releasing it early, or another publish of it landing, with the uploaded file
// or without it.
//
// Mutation: let fail delete another attempt's generated files, drop
// takeClaim's marks, or let releaseAbandoned or close delete an offered file,
// and the landed version names an object the store no longer holds.
func TestARetryRefusedAsMovedLeavesWhatAnEarlierAttemptLanded(t *testing.T) {
	t.Parallel()
	again := func(names bool) func(h *harness, id artifacts.ID, up string) {
		return func(h *harness, id artifacts.ID, up string) {
			req := artifactupload.PublishRequest{Artifact: id, Base: 2, Mode: manifest.ModePatch, Upload: up,
				Files: []artifactupload.FileSpec{{Path: "b.txt", Upload: true}}}
			if names {
				req.Files = append(req.Files, artifactupload.FileSpec{Path: "a.txt", Upload: true})
			}
			if _, _, err := h.svc.Publish(h.t.Context(), h.person("ada", h.opKey("again")), req); err != nil {
				h.t.Fatalf("publish the reopened upload again: %v", err)
			}
		}
	}
	released := func(h *harness, id artifacts.ID, _ string) {
		h.clock.advance(objstore.MaxPresignTerm + time.Minute)
		h.upload(h.person("ada", h.opKey("next")), id, map[string]string{"c.txt": "next"})
	}
	for _, c := range []struct {
		name string
		// inline and upload are the files the publish names: inline notes,
		// an uploaded file, or both.
		inline, upload bool
		// then is what follows the refusal on the upload it reopened.
		then func(h *harness, id artifacts.ID, up string)
	}{
		{"inline", true, false, func(*harness, artifacts.ID, string) {}},
		{"with an upload, released once its window closes", true, true, released},
		{"an upload alone, released once its window closes", false, true, released},
		{"with an upload, published again with its file", true, true, again(true)},
		{"with an upload, published again without its file", true, true, again(false)},
		{"an upload alone, published again without its file", false, true, again(false)},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, options{loseAnswer: true})
			created := h.create(h.person("ada", h.opKey("create")), map[string]string{"index.html": "<p>v1</p>"})
			req := artifactupload.PublishRequest{Artifact: created.Artifact, Base: 1, Mode: manifest.ModePatch}
			if c.inline {
				req.Files = []artifactupload.FileSpec{{Path: "notes.txt", Inline: []byte("landed notes")}}
			}
			if c.upload {
				req.Upload = h.upload(h.person("ada", h.opKey("upload")), created.Artifact,
					map[string]string{"a.txt": "uploaded", "b.txt": "uploaded later"})
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
			if gone := h.gone(); len(gone) > 0 {
				t.Errorf("rows name %v, which the store no longer holds", gone)
			}
			landed := h.version(created.Artifact, 2)
			if c.inline {
				if got := h.file(landed, "notes.txt"); got != "landed notes" {
					t.Errorf("the landed version's notes read %q, want them as landed", got)
				}
			}
			if c.upload {
				if got := h.file(landed, "a.txt"); got != "uploaded" {
					t.Errorf("the landed version's uploaded file reads %q, want it as uploaded", got)
				}
			}
			if unnamed, held := h.unnamedBytes(), h.held(); unnamed > held {
				t.Errorf("the store holds %d bytes nothing names and the reservations hold %d", unnamed, held)
			}
		})
	}
}

// A KEYED RETRY OF OTHER BYTES LEAVES WHAT ITS FIRST ATTEMPT LANDED: a create,
// inline or of an upload, lands and loses its answer, and its retry under the
// same operation key brings other bytes, so the domain takes it for another
// operation and lands a second artifact. The retry's close deletes nothing
// the first attempt stored, which the first landing names.
//
// Mutation: let close delete a held generated file the run did not stage, or
// an uploaded file offered to the first attempt, and a landed version names an
// object the store no longer holds.
func TestAKeyedRetryOfOtherBytesLeavesWhatItsFirstAttemptLanded(t *testing.T) {
	t.Parallel()
	const firstBody = "<p>the first attempt</p>"
	for _, c := range []struct {
		name string
		o    options
		// attempt sends one attempt of the operation, bringing body.
		attempt func(h *harness, k artifactsdomain.Call, up, body string) (artifactsdomain.Published, error)
		// upload is what the case uploads first; nil for none.
		upload map[string]string
	}{
		{"a create", options{loseCreate: true},
			func(h *harness, k artifactsdomain.Call, _, body string) (artifactsdomain.Published, error) {
				p, _, err := h.svc.Create(h.t.Context(), k, artifactupload.CreateRequest{
					Kind: artifacts.KindHTML, Title: "A page", Owner: "ada", OwnerKind: k.Authority.Actor.Kind,
					Entry: "index.html", Files: []artifactupload.FileSpec{{Path: "index.html", Inline: []byte(body)}}})
				return p, err
			}, nil},
		{"a create of an upload", options{loseCreate: true},
			// THE RETRY NAMES NO UPLOADED FILE (the domain refuses a second
			// artifact one the first names), leaving the first's to close.
			func(h *harness, k artifactsdomain.Call, up, body string) (artifactsdomain.Published, error) {
				files := []artifactupload.FileSpec{{Path: "index.html", Inline: []byte(body)}}
				if body == firstBody {
					files = append(files, artifactupload.FileSpec{Path: "a.txt", Upload: true})
				}
				p, _, err := h.svc.Create(h.t.Context(), k, artifactupload.CreateRequest{
					Kind: artifacts.KindHTML, Title: "A page", Owner: "ada", OwnerKind: k.Authority.Actor.Kind,
					Entry: "index.html", Upload: up, Files: files})
				return p, err
			}, map[string]string{"a.txt": "uploaded"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, c.o)
			var up string
			if c.upload != nil {
				up = h.upload(h.person("ada", h.opKey("upload")), artifacts.ID{}, c.upload)
			}
			before := h.named()
			k := h.person("ada", h.opKey("lost"))
			if _, err := c.attempt(h, k, up, firstBody); err == nil ||
				!strings.Contains(err.Error(), "answer was lost") {
				t.Fatalf("premise: the first attempt answered %v, want its answer lost", err)
			}
			first := map[objstore.Key]bool{}
			for key := range h.named() {
				if !before[key] {
					first[key] = true
				}
			}
			if len(first) == 0 {
				t.Fatal("premise: the first attempt landed nothing")
			}
			retried, err := c.attempt(h, k, up, "<p>the retry</p>")
			if err != nil {
				t.Fatalf("the retry of other bytes was refused: %v", err)
			}
			second := map[objstore.Key]bool{}
			for _, e := range h.version(retried.Artifact, retried.Number).Manifest.Files {
				second[e.Object.Key] = true
			}
			// THE BYTES ONLY THE FIRST LANDING NAMES stay on the retry's
			// record, which holds them until the hold lapses.
			stored := h.stored()
			var firstBytes int64
			for key := range first {
				size, ok := stored[key]
				if !ok {
					t.Errorf("the first attempt's landed object %s is gone after the retry landed", key)
				}
				if !second[key] {
					firstBytes += size
				}
			}
			if gone := h.gone(); len(gone) > 0 {
				t.Errorf("rows name %v, which the store no longer holds", gone)
			}
			if unnamed, held := h.unnamedBytes(), h.held(); unnamed > held || held < firstBytes {
				t.Errorf("the reservations hold %d bytes, with %d unnamed and %d only the first landing names",
					held, unnamed, firstBytes)
			}
		})
	}
}

// A LANDED PUBLISH DELETES WHAT IT OWNS AND THE VERSION DOES NOT NAME: the
// page its upload streamed, which the version names linted instead, is
// deleted at once and its reservation released, as no write of unknown
// outcome was offered it.
//
// Mutation: let close delete nothing of the upload's own, or treat every
// uploaded file as offered, and the page stays stored and held.
func TestALandedPublishDeletesWhatItOwnsAndTheVersionDoesNotName(t *testing.T) {
	t.Parallel()
	h := newHarness(t, options{})
	created := h.create(h.person("ada", h.opKey("create")), map[string]string{"index.html": "<p>v1</p>"})
	c := h.person("ada", h.opKey("publish"))
	up := h.upload(c, created.Artifact, map[string]string{"index.html": page})
	raw, err := artifactupload.UploadKey(t.Context(), h.svc, up, "index.html")
	if err != nil {
		t.Fatal(err)
	}
	key, err := objstore.ParseKey(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.svc.Publish(t.Context(), c, artifactupload.PublishRequest{
		Artifact: created.Artifact, Base: 1, Mode: manifest.ModePatch, Upload: up,
		Files: []artifactupload.FileSpec{{Path: "index.html", Upload: true}},
	}); err != nil {
		t.Fatal(err)
	}
	if h.named()[key] {
		t.Fatal("premise: the version names the page as uploaded, not linted")
	}
	if _, ok := h.stored()[key]; ok {
		t.Error("the uploaded page the version superseded is still stored")
	}
	if held, unnamed := h.held(), h.unnamedBytes(); held != 0 || unnamed != 0 {
		t.Errorf("after the publish %d bytes are held and %d unnamed, want none", held, unnamed)
	}
}

// AN ASSET WHOSE ANSWER WAS LOST KEEPS ITS OBJECT THROUGH THE EARLY RELEASE:
// the asset lands and its answer is lost, so its upload is still open when
// its window closes; the ledger change that would release it early leaves it
// to lapse, since the asset names the object.
//
// Mutation: drop the mark addAsset makes before its write, or let
// releaseAbandoned release a record holding an offered file, and the asset
// names an object the store no longer holds.
func TestAnAssetWhoseAnswerWasLostKeepsItsObjectThroughTheEarlyRelease(t *testing.T) {
	t.Parallel()
	h := newHarness(t, options{loseAsset: true})
	created := h.create(h.person("ada", h.opKey("create")), map[string]string{"index.html": "<p>x</p>"})
	c := h.person("ada", h.opKey("asset"))
	body := "asset notes"
	intent, err := h.svc.Intend(t.Context(), c, created.Artifact, artifacts.UploadRequest{
		Entries: declare(map[string]string{"notes.txt": body}).Entries, Asset: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.svc.Transfer(t.Context(), c, intent.Upload, "notes.txt", strings.NewReader(body), -1); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.AddAsset(t.Context(), h.person("ada", h.opKey("add")), created.Artifact, intent.Upload,
		"notes.txt"); err == nil {
		t.Fatal("premise: the asset's answer was not lost")
	}
	h.clock.advance(objstore.MaxPresignTerm + time.Minute)
	h.upload(h.person("ada", h.opKey("next")), created.Artifact, map[string]string{"c.txt": "next"})
	if gone := h.gone(); len(gone) > 0 {
		t.Errorf("rows name %v, which the store no longer holds", gone)
	}
	if held := h.held(); held < int64(len(body)) {
		t.Errorf("the reservations hold %d bytes, less than the asset's %d", held, len(body))
	}
}

// A REFUSAL TAKES BACK ONLY ITS OWN ASSET OFFER: an asset add counts its
// offer before its write. One the domain refuses outright (past the company's
// quota here) landed nothing and takes its offer back, so once the window
// closes the upload is released early, its bytes deleted. One refused after
// an add whose answer was lost takes back its own offer alone, and the
// object the first add may have landed is kept until the hold lapses.
//
// Mutation: keep the offer after a refusal, and the refused upload is never
// released early; clear every offer on a refusal, and the landed asset names
// an object the store no longer holds.
func TestARefusalTakesBackOnlyItsOwnAssetOffer(t *testing.T) {
	t.Parallel()
	for _, lost := range []bool{false, true} {
		t.Run(fmt.Sprintf("after a lost answer=%v", lost), func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, options{loseAsset: lost})
			created := h.create(h.person("ada", h.opKey("create")), map[string]string{"index.html": "<p>x</p>"})
			c := h.person("ada", h.opKey("asset"))
			body := "asset notes"
			intent, err := h.svc.Intend(t.Context(), c, created.Artifact, artifacts.UploadRequest{
				Entries: declare(map[string]string{"notes.txt": body}).Entries, Asset: true})
			if err != nil {
				t.Fatal(err)
			}
			if err := h.svc.Transfer(t.Context(), c, intent.Upload, "notes.txt", strings.NewReader(body),
				-1); err != nil {
				t.Fatal(err)
			}
			if lost {
				if _, err := h.svc.AddAsset(t.Context(), h.person("ada", h.opKey("lost")), created.Artifact,
					intent.Upload, "notes.txt"); err == nil {
					t.Fatal("premise: the first add's answer was not lost")
				}
			}
			k := h.person("ada", h.opKey("refused"))
			k.Authority.Limits.CompanyBytes = 1
			if _, err := h.svc.AddAsset(t.Context(), k, created.Artifact, intent.Upload, "notes.txt"); err == nil ||
				artifactupload.ReasonOf(err) == "other" {
				t.Fatalf("premise: the add answered %v, want it refused", err)
			}
			h.clock.advance(objstore.MaxPresignTerm + time.Minute)
			h.upload(h.person("ada", h.opKey("next")), created.Artifact, map[string]string{"c.txt": "next"})
			if gone := h.gone(); len(gone) > 0 {
				t.Errorf("rows name %v, which the store no longer holds", gone)
			}
			state, _, err := artifactupload.UploadState(t.Context(), h.svc, intent.Upload)
			if err != nil {
				t.Fatal(err)
			}
			if released := state == ""; released == lost {
				t.Errorf("the asset upload's record is %q once its window closed, want it released early "+
					"only where no add's answer was lost", state)
			}
		})
	}
}
