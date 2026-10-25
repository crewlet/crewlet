package artifactupload_test

import (
	"os"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/artifactupload"
	"github.com/crewlet/crewlet/internal/artifactupload/lint"
)

// boardFiles is the board the board format's own tests read.
func boardFiles(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, p := range []string{"dashboard.json", "data/targets.csv"} {
		b, err := os.ReadFile("../artifacts/dashspec/testdata/board/" + p)
		if err != nil {
			t.Fatal(err)
		}
		out[p] = string(b)
	}
	return out
}

// createBoard makes a dashboard artifact from files, entry dashboard.json.
func (h *harness) createBoard(c artifactsdomain.Call, files map[string]string) (artifactsdomain.Published, error) {
	h.t.Helper()
	up := h.upload(c, artifacts.ID{}, files)
	got, _, err := h.svc.Create(h.t.Context(), c, artifactupload.CreateRequest{
		Kind: artifacts.KindDashboard, Title: "Delivery", Owner: c.Authority.Actor.Name,
		OwnerKind: c.Authority.Actor.Kind, Upload: up, Files: specs(files), Entry: "dashboard.json",
	})
	return got, err
}

// A BOARD PUBLISHES AS WRITTEN, its file dataset with it, and the gallery
// searches the board's words: its widgets' titles and its notes, never a
// figure.
func TestABoardPublishesAsWritten(t *testing.T) {
	t.Parallel()
	h := newHarness(t, options{})
	files := boardFiles(t)
	got, err := h.createBoard(h.agent("writer", "turn-1", h.opKey("create")), files)
	if err != nil {
		t.Fatal(err)
	}
	v := h.version(got.Artifact, 1)
	if v.Manifest.Entry != "dashboard.json" || len(v.Manifest.Files) != 2 {
		t.Fatalf("the board's version holds %d files opening on %q", len(v.Manifest.Files), v.Manifest.Entry)
	}
	for p, body := range files {
		if stored := h.file(v, p); stored != body {
			t.Errorf("%s was stored as\n%s", p, stored)
		}
	}
	if page, err := h.domain.reader.List(t.Context(), artifactsdomain.Viewer{Admin: true},
		artifactsdomain.ListQuery{Q: "seven company days"}); err != nil || len(page.Contents) != 1 {
		t.Errorf("a search for the board's note found %d (%v)", len(page.Contents), err)
	}
	if page, _ := h.domain.reader.List(t.Context(), artifactsdomain.Viewer{Admin: true},
		artifactsdomain.ListQuery{Q: "250000"}); len(page.Contents) != 0 {
		t.Error("the search indexed a figure of a file dataset")
	}
}

// A BOARD OUTSIDE THE FORMAT IS REFUSED WHOLE, every finding naming the field,
// in the linter's shape under its own code; a file dataset's row naming its
// file and line.
func TestABoardOutsideTheFormatIsRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t, options{})
	files := boardFiles(t)
	files["dashboard.json"] = strings.Replace(files["dashboard.json"], `"w": 6`, `"w": 13`, 1)
	found := policyFindings(t, func() error {
		_, err := h.createBoard(h.person("ada", h.opKey("create")), files)
		return err
	}())
	if len(found) != 1 || found[0].Code != lint.CodeDashboardFormat ||
		!strings.Contains(found[0].Message, "widgets[2].w") {
		t.Fatalf("findings %v", found)
	}
	files = boardFiles(t)
	files["data/targets.csv"] = "region,target\nemea,lots\n"
	found = policyFindings(t, func() error {
		_, err := h.createBoard(h.person("ada", h.opKey("create-2")), files)
		return err
	}())
	if len(found) != 1 || found[0].Path != "data/targets.csv" || found[0].Line != 2 {
		t.Fatalf("findings %v", found)
	}
}
