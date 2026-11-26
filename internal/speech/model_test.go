package speech

import (
	"path"
	"regexp"
	"strings"
	"testing"
)

var hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// EVERY PINNED MODEL IS LOADABLE BY ITS TABLE ALONE: each role filled exactly
// once, under a name no other file of the model shares (they sit in one
// directory), every digest a SHA-256 with its size, and every file at an
// official address: k2-fsa's Hugging Face repository at a commit, or a k2-fsa
// release archive for a file published no other way.
func TestEveryPinnedModelIsWholeAndOfficial(t *testing.T) {
	commit := regexp.MustCompile(`^https://huggingface\.co/csukuangfj/[^/]+/resolve/[0-9a-f]{40}/[^/]+$`)
	for name, m := range models {
		if name != m.Name {
			t.Errorf("model %q is listed as %q", m.Name, name)
		}
		filled := map[Role]int{}
		names := map[string]bool{}
		for _, f := range m.Files {
			filled[f.Role]++
			if names[f.Name] || path.Base(f.Name) != f.Name {
				t.Errorf("%s: %s is not a file name of its own", name, f.Name)
			}
			names[f.Name] = true
			if !hex64.MatchString(f.SHA256) || f.Size <= 0 {
				t.Errorf("%s: %s is pinned by %q and %d bytes", name, f.Name, f.SHA256, f.Size)
			}
			switch {
			case f.From == nil && !commit.MatchString(f.URL):
				t.Errorf("%s: %s is fetched from %s, not a pinned commit of the model's repository", name, f.Name, f.URL)
			case f.From != nil && (f.URL != "" || path.Base(f.Member) != f.Name || !official(*f.From)):
				t.Errorf("%s: %s is member %s of %+v", name, f.Name, f.Member, f.From)
			}
		}
		for _, role := range roles {
			if filled[role] != 1 {
				t.Errorf("%s fills %s %d times, want once", name, role, filled[role])
			}
		}
	}
}

// official reports whether a is a k2-fsa release archive pinned with its size.
func official(a ModelArchive) bool {
	return strings.HasPrefix(a.URL, "https://github.com/k2-fsa/sherpa-onnx/releases/download/") &&
		hex64.MatchString(a.SHA256) && a.Size > 0
}

// EVERY RELEASE PLATFORM'S LIBRARY IS PINNED: the archive of [SherpaVersion]
// with its size, and its two members, under one directory name per platform.
func TestEveryReleasePlatformsLibraryIsPinned(t *testing.T) {
	want := []string{"darwin/amd64", "darwin/arm64", "linux/amd64", "linux/arm64"}
	if got := Platforms(); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("libraries are pinned for %v, want the release platforms %v", got, want)
	}
	for _, p := range natives {
		if !official(p.archive) || !strings.Contains(p.archive.URL, "/"+SherpaVersion+"/sherpa-onnx-"+SherpaVersion+"-") {
			t.Errorf("%s: the archive %+v is not the pinned release's", p.platform, p.archive)
		}
		for _, f := range p.files() {
			if !hex64.MatchString(f.SHA256) || !strings.HasPrefix(f.Path, strings.TrimSuffix(path.Base(p.archive.URL), ".tar.bz2")+"/lib/") {
				t.Errorf("%s: member %+v", p.platform, f)
			}
		}
		if !strings.HasSuffix(p.Name(), strings.ReplaceAll(p.platform, "/", "-")) {
			t.Errorf("%s is kept as %s", p.platform, p.Name())
		}
	}
}
