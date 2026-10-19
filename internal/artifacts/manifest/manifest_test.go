package manifest

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/mediatype"
	"github.com/crewlet/crewlet/internal/objstore"
)

// object is a stored object holding body, under a key minted at a fixed
// instant plus n milliseconds so every fixture names a distinct key.
func object(n int, body string) objstore.Object {
	return objstore.Object{
		Key:  objstore.KeyAt(time.Unix(1_800_000_000, 0).Add(time.Duration(n) * time.Millisecond)),
		Hash: objstore.HashOf([]byte(body)), Size: int64(len(body)),
	}
}

// entry is a valid entry at path holding body.
func entry(t *testing.T, n int, path, body string) artifacts.Entry {
	t.Helper()
	e, err := NewEntry(path, "", object(n, body), false)
	if err != nil {
		t.Fatalf("NewEntry(%q): %v", path, err)
	}
	return e
}

// THE ONE PATH RULE: the tracker's grammar plus the reserved prefixes, which
// the serving origin routes itself.
func TestAPathFollowsTheEngineRuleAndAvoidsTheOriginsRoutes(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]string{
		"index.html":         "index.html",
		"/css/site.css":      "css/site.css",
		"  img/logo.png ":    "img/logo.png",
		"deep/preflight.js":  "deep/preflight.js",
		"_crewlet_notes.txt": "_crewlet_notes.txt",
	} {
		got, err := CleanPath(raw)
		if err != nil || got != want {
			t.Errorf("CleanPath(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	for _, raw := range []string{"", "a/../b", "./a", "a//b", `a\b`, "dir/", strings.Repeat("a", artifacts.MaxPathBytes+1)} {
		if _, err := CleanPath(raw); !errors.Is(err, ErrInvalid) {
			t.Errorf("CleanPath(%q) = %v, want ErrInvalid", raw, err)
		}
	}
	for _, raw := range []string{"_crewlet/runtime.js", "_assets/x.png", "_Assets/X.png", "_CREWLET/a.js"} {
		if _, err := CleanPath(raw); !errors.Is(err, ErrReserved) {
			t.Errorf("CleanPath(%q) = %v, want ErrReserved", raw, err)
		}
	}
}

// THE PREFLIGHT SCRIPT IS JAVASCRIPT AND SMALL, at the root and nowhere else.
func TestThePreflightScriptIsHeldToItsBounds(t *testing.T) {
	t.Parallel()
	if _, err := NewEntry(PreflightPath, "", object(1, "ok()"), false); err != nil {
		t.Fatalf("a small preflight script was refused: %v", err)
	}
	big := object(2, strings.Repeat("x", int(artifacts.MaxPreflightBytes)+1))
	if _, err := NewEntry(PreflightPath, "", big, false); !errors.Is(err, ErrReserved) {
		t.Fatalf("an oversized preflight script = %v, want ErrReserved", err)
	}
	if _, err := NewEntry("nested/preflight.js", "", big, false); err != nil {
		t.Fatalf("a preflight.js below the root is an ordinary file: %v", err)
	}
}

// A FILE IS HELD TO THE BOUND OF ITS CLASS, and a type the table does not serve
// is refused naming the path.
func TestAFileIsHeldToItsClassBound(t *testing.T) {
	t.Parallel()
	bin := objstore.Object{Key: object(1, "").Key, Hash: objstore.HashOf(nil), Size: artifacts.MaxBinaryFileBytes + 1}
	if _, err := NewEntry("photo.png", "", bin, false); !errors.Is(err, ErrInvalid) {
		t.Fatalf("an oversized binary = %v, want ErrInvalid", err)
	}
	text := bin
	text.Size = artifacts.MaxTextFileBytes
	if _, err := NewEntry("data.json", "", text, false); err != nil {
		t.Fatalf("a text file at the text bound was refused: %v", err)
	}
	if _, err := NewEntry("tool.exe", "", object(3, "MZ"), false); !errors.Is(err, mediatype.ErrRefused) {
		t.Fatalf("an unserved type = %v, want mediatype.ErrRefused", err)
	}
}

// A VERSION'S MANIFEST IS SORTED, UNIQUE, BOUNDED, AND ITS ENTRY IS A PAGE ITS
// KIND RENDERS, and a sheet may hold nothing at all.
func TestAManifestIsValidatedAgainstItsKind(t *testing.T) {
	t.Parallel()
	page := entry(t, 1, "index.html", "<!doctype html>")
	css := entry(t, 2, "site.css", "body{}")
	doc := entry(t, 3, "README.md", "# hi")
	good := artifacts.Manifest{Entry: "index.html", Files: []artifacts.Entry{page, css}}
	if err := Validate(artifacts.KindHTML, good); err != nil {
		t.Fatalf("a good html manifest was refused: %v", err)
	}
	if err := Validate(artifacts.KindSheet, artifacts.Manifest{}); err != nil {
		t.Fatalf("an empty sheet manifest was refused: %v", err)
	}
	if err := Validate(artifacts.KindDoc, artifacts.Manifest{Entry: "README.md",
		Files: []artifacts.Entry{doc}}); err != nil {
		t.Fatalf("a good doc manifest was refused: %v", err)
	}
	cases := map[string]struct {
		kind artifacts.Kind
		m    artifacts.Manifest
	}{
		"unsorted":       {artifacts.KindHTML, artifacts.Manifest{Entry: "index.html", Files: []artifacts.Entry{css, page}}},
		"repeated":       {artifacts.KindHTML, artifacts.Manifest{Entry: "index.html", Files: []artifacts.Entry{page, page}}},
		"no entry":       {artifacts.KindHTML, artifacts.Manifest{Files: []artifacts.Entry{page}}},
		"absent entry":   {artifacts.KindHTML, artifacts.Manifest{Entry: "home.html", Files: []artifacts.Entry{page}}},
		"wrong kind":     {artifacts.KindDoc, good},
		"css entry":      {artifacts.KindHTML, artifacts.Manifest{Entry: "site.css", Files: []artifacts.Entry{page, css}}},
		"client type":    {artifacts.KindHTML, artifacts.Manifest{Entry: "index.html", Files: []artifacts.Entry{withType(page, "text/plain")}}},
		"script on css":  {artifacts.KindHTML, artifacts.Manifest{Entry: "index.html", Files: []artifacts.Entry{page, withScript(css)}}},
		"unknown kind":   {"whiteboard", good},
		"too many files": {artifacts.KindHTML, manyFiles(t, artifacts.MaxFilesPerVersion+1)},
	}
	for name, c := range cases {
		if err := Validate(c.kind, c.m); err == nil {
			t.Errorf("%s: the manifest validated", name)
		}
	}
}

func withType(e artifacts.Entry, typ string) artifacts.Entry { e.ContentType = typ; return e }

func withScript(e artifacts.Entry) artifacts.Entry {
	e.Script = &artifacts.ScriptPolicy{Nonced: true}
	return e
}

func manyFiles(t *testing.T, n int) artifacts.Manifest {
	t.Helper()
	m := artifacts.Manifest{Entry: "f0000.html"}
	for i := range n {
		m.Files = append(m.Files, entry(t, i, fmt.Sprintf("f%04d.html", i), "x"))
	}
	return m
}

// THE STORED FORM IS CANONICAL, so two writers that listed the same files in
// another order, or a handler hash twice, store one row, and a row in another
// form is refused on read rather than trusted.
func TestTheManifestHasOneStoredForm(t *testing.T) {
	t.Parallel()
	page := entry(t, 1, "index.html", "<p>")
	page.Script = &artifacts.ScriptPolicy{Nonced: true, HandlerHashes: []string{"b", "a", "b"}}
	css := entry(t, 2, "a.css", "x")
	one, err := Encode(artifacts.Manifest{Entry: "index.html", Files: []artifacts.Entry{page, css}})
	if err != nil {
		t.Fatal(err)
	}
	page.Script = &artifacts.ScriptPolicy{Nonced: true, HandlerHashes: []string{"a", "b"}}
	other, err := Encode(artifacts.Manifest{Entry: "index.html", Files: []artifacts.Entry{css, page}})
	if err != nil {
		t.Fatal(err)
	}
	if string(one) != string(other) {
		t.Fatalf("two spellings of one manifest encoded differently:\n%s\n%s", one, other)
	}
	back, err := Decode(one)
	if err != nil {
		t.Fatalf("decode the canonical form: %v", err)
	}
	if again, _ := Encode(back); string(again) != string(one) {
		t.Fatalf("the canonical form did not round-trip:\n%s\n%s", again, one)
	}
	unsorted := strings.Replace(string(one), `"entry":"index.html"`, `"entry": "index.html"`, 1)
	if _, err := Decode([]byte(unsorted)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a non-canonical stored form decoded: %v", err)
	}
	if empty, _ := Encode(artifacts.Manifest{}); string(empty) != `{"entry":"","files":[]}` {
		t.Fatalf("the empty manifest encodes as %s", empty)
	}
}

// A PATCH KEEPS WHAT IT DOES NOT NAME (CARRIED), A WHOLE PUBLISH STATES THE SET.
func TestNextComposesWholeAndPatchPublishes(t *testing.T) {
	t.Parallel()
	page := entry(t, 1, "index.html", "<p>1")
	css := entry(t, 2, "site.css", "a{}")
	js := entry(t, 3, "app.js", "go()")
	base := artifacts.Manifest{Entry: "index.html", Files: []artifacts.Entry{page, css}}

	newPage := entry(t, 4, "index.html", "<p>2")
	patched, err := Next(ModePatch, base, "", []Change{
		{Path: "index.html", Entry: newPage},
		{Path: "site.css", Remove: true},
		{Path: "app.js", Entry: js},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(patched.Files) != 2 || patched.Files[0].Path != "app.js" || patched.Files[1].Object != newPage.Object {
		t.Fatalf("patch = %+v", patched)
	}
	if patched.Entry != "index.html" {
		t.Fatalf("the patch lost the base's entry: %q", patched.Entry)
	}
	kept, err := Next(ModePatch, base, "", []Change{{Path: "app.js", Entry: js}})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := EntryAt(kept, "site.css"); !got.Carried || got.Object != css.Object {
		t.Fatalf("a path the patch did not name was not carried: %+v", got)
	}

	whole, err := Next(ModeWhole, base, "app.html", []Change{{Path: "app.html", Entry: entry(t, 5, "app.html", "<p>")}})
	if err != nil {
		t.Fatal(err)
	}
	if len(whole.Files) != 1 || whole.Entry != "app.html" {
		t.Fatalf("a whole publish kept the base's files: %+v", whole)
	}

	for name, changes := range map[string][]Change{
		"remove in whole": {{Path: "site.css", Remove: true}},
		"named twice":     {{Path: "app.js", Entry: js}, {Path: "app.js", Entry: js}},
		"mismatched":      {{Path: "x.js", Entry: js}},
	} {
		if _, err := Next(ModeWhole, base, "", changes); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: Next = %v, want ErrInvalid", name, err)
		}
	}
	if _, err := Next(ModePatch, base, "", []Change{{Path: "gone.css", Remove: true}}); !errors.Is(err, ErrInvalid) {
		t.Errorf("removing a path the base does not hold = %v, want ErrInvalid", err)
	}
}

// A CARRY IS MATCHED ON THE DIGEST AND THE SIZE, against the artifact's own
// held objects only.
func TestPlanCarryNamesOnlyBytesTheArtifactHolds(t *testing.T) {
	t.Parallel()
	held := object(1, "body{}")
	plan, err := PlanCarry([]Declared{
		{Path: "site.css", SHA256: held.Hash, Size: held.Size},
		{Path: "index.html", SHA256: objstore.HashOf([]byte("<p>")), Size: 3},
		{Path: "liar.css", SHA256: held.Hash, Size: held.Size + 1},
	}, map[objstore.Hash]objstore.Object{held.Hash: held})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Carried) != 1 || plan.Carried[0].Object != held || !plan.Carried[0].Carried {
		t.Fatalf("carried = %+v", plan.Carried)
	}
	if len(plan.Upload) != 2 || plan.Upload[0].Path != "index.html" || plan.Upload[1].Path != "liar.css" {
		t.Fatalf("upload = %+v", plan.Upload)
	}
	if plan.Upload[0].ContentType != "text/html; charset=utf-8" {
		t.Fatalf("an upload's type is the table's, got %q", plan.Upload[0].ContentType)
	}
	if _, err := PlanCarry([]Declared{{Path: "a.css", SHA256: "nope", Size: 1}}, nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a malformed digest = %v, want ErrInvalid", err)
	}
	if got := Hashes([]Declared{{SHA256: "b"}, {SHA256: "a"}, {SHA256: "b"}}); len(got) != 2 || got[0] != "a" {
		t.Fatalf("Hashes = %v", got)
	}
}
