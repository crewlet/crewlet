package artifactupload_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/manifest"
	"github.com/crewlet/crewlet/internal/artifactupload"
	"github.com/crewlet/crewlet/internal/objstore"
)

// THE REPORT COUNTS THE MANIFEST THAT LANDED, NOT THE UPLOAD: republishing an
// unchanged page sends it (its author's digest never matches the linted form
// the artifact holds) and the version still carries it, so the count says
// nothing new was stored. A retry of the same publish is answered with the
// same count.
func TestTheReportCountsTheFilesTheVersionCarried(t *testing.T) {
	t.Parallel()
	h := newHarness(t, options{})
	files := map[string]string{"index.html": page, "app.css": "body { margin: 0 }"}

	c := h.person("ada", h.opKey("create"))
	up := h.upload(c, artifacts.ID{}, files)
	created, report, err := h.svc.Create(t.Context(), c, artifactupload.CreateRequest{
		Kind: artifacts.KindHTML, Title: "A page", Owner: "ada", OwnerKind: c.Authority.Actor.Kind,
		Upload: up, Files: specs(files), Entry: "index.html",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// The page, the stylesheet and the page's copy of its CDN script.
	if report.Files == nil || *report.Files != (artifactupload.FileCounts{Stored: 3}) {
		t.Fatalf("a create reports %+v, want three files stored", report.Files)
	}

	c = h.person("ada", h.opKey("publish"))
	intent, err := h.svc.Intend(t.Context(), c, created.Artifact, declare(files))
	if err != nil {
		t.Fatalf("Intend: %v", err)
	}
	if len(intent.Transfers) != 1 || intent.Transfers[0].Path != "index.html" {
		t.Fatalf("the intent asks for %+v, want the unchanged page alone", intent.Transfers)
	}
	up = h.upload(c, created.Artifact, files)
	// As the CLI names them: what the intent found carried by its digest,
	// the rest by the upload.
	req := artifactupload.PublishRequest{Artifact: created.Artifact, Base: 1, Mode: manifest.ModeWhole,
		Upload: up, Files: []artifactupload.FileSpec{
			{Path: "index.html", Upload: true},
			{Path: "app.css", Carry: objstore.HashOf([]byte(files["app.css"]))},
		}}
	_, report, err = h.svc.Publish(t.Context(), c, req)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	want := artifactupload.FileCounts{Carried: 3}
	if report.Files == nil || *report.Files != want {
		t.Fatalf("republishing unchanged files reports %+v, want %+v", report.Files, want)
	}
	_, retried, err := h.svc.Publish(t.Context(), c, req)
	if err != nil {
		t.Fatalf("the retry: %v", err)
	}
	if retried.Files == nil || *retried.Files != want {
		t.Fatalf("the retry reports %+v, want %+v", retried.Files, want)
	}
}
