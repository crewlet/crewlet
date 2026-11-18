package boxhelper

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"maps"
	"path/filepath"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/browser"
	"github.com/crewlet/crewlet/internal/prereqtest"
	"github.com/crewlet/crewlet/internal/sourcetree"
)

// THE EMBEDDED TREE IS THE PINNED RELEASE: pw/ embeds exactly the two
// tarballs scripts/playwright-mcp-pin.env names, each of its pinned SHA-256
// and its own package.json naming its pinned release; @playwright/mcp depends
// on exactly the pinned playwright-core, which depends on nothing, so a bump
// that grows the tree fails here until somebody reads it; and
// internal/browser's release constants are the pin's, its expected Chromium
// the one playwright-core's browsers.json names.
//
// Mutations, each red: one digit of PLAYWRIGHT_CORE_SHA256; another release's
// tarball in pw/; PlaywrightMCPRelease = "0.0.84"; ExpectedChromiumVersion
// off by a patch.
func TestTheEmbeddedTreeIsThePinnedRelease(t *testing.T) {
	t.Parallel()
	pins := prereqtest.ReadPins(t, filepath.Join(sourcetree.Root(t), "scripts", "playwright-mcp-pin.env"))
	names := []string{"PLAYWRIGHT_CORE_SHA256", "PLAYWRIGHT_CORE_VERSION", "PLAYWRIGHT_MCP_SHA256", "PLAYWRIGHT_MCP_VERSION"}
	if got := slices.Sorted(maps.Keys(pins)); !slices.Equal(got, names) {
		t.Fatalf("scripts/playwright-mcp-pin.env names %q, want exactly %q", got, names)
	}
	if browser.PlaywrightMCPRelease != pins["PLAYWRIGHT_MCP_VERSION"] ||
		browser.PlaywrightCoreRelease != pins["PLAYWRIGHT_CORE_VERSION"] {
		t.Fatalf("internal/browser pins @playwright/mcp %s and playwright-core %s, and the pin file %s and %s: a bump "+
			"moves both", browser.PlaywrightMCPRelease, browser.PlaywrightCoreRelease, pins["PLAYWRIGHT_MCP_VERSION"],
			pins["PLAYWRIGHT_CORE_VERSION"])
	}

	type pinned struct{ pkg, file, version, sha string }
	want := []pinned{
		{"@playwright/mcp", "mcp-" + pins["PLAYWRIGHT_MCP_VERSION"] + ".tgz", pins["PLAYWRIGHT_MCP_VERSION"],
			pins["PLAYWRIGHT_MCP_SHA256"]},
		{"playwright-core", "playwright-core-" + pins["PLAYWRIGHT_CORE_VERSION"] + ".tgz", pins["PLAYWRIGHT_CORE_VERSION"],
			pins["PLAYWRIGHT_CORE_SHA256"]},
	}
	entries, err := fs.ReadDir(pw, "pw")
	if err != nil {
		t.Fatal(err)
	}
	var embedded []string
	for _, e := range entries {
		embedded = append(embedded, e.Name())
	}
	if files := []string{want[0].file, want[1].file}; !slices.Equal(embedded, slices.Sorted(slices.Values(files))) {
		t.Fatalf("pw/ embeds %q, want exactly %q: `make playwright-mcp` writes the pin's tree there", embedded, files)
	}

	got := Tarballs()
	if len(got) != len(want) {
		t.Fatalf("Tarballs() is %d tarballs, want %d", len(got), len(want))
	}
	manifests := map[string]manifest{}
	for i, w := range want {
		tb := got[i]
		sum := sha256.Sum256(tb.Bytes)
		if tb.Name != w.file || tb.SHA256 != w.sha || hex.EncodeToString(sum[:]) != w.sha {
			t.Errorf("Tarballs()[%d] is %s of SHA-256 %s (its bytes %x); the pin names %s of %s", i, tb.Name, tb.SHA256,
				sum, w.file, w.sha)
			continue
		}
		files := packageFiles(t, tb.Name, tb.Bytes, "package.json", "browsers.json")
		var m manifest
		if err := json.Unmarshal(files["package.json"], &m); err != nil {
			t.Fatalf("%s: package.json: %v", tb.Name, err)
		}
		if m.Name != w.pkg || m.Version != w.version {
			t.Errorf("%s holds %s@%s, want %s@%s", tb.Name, m.Name, m.Version, w.pkg, w.version)
		}
		m.browsers = files["browsers.json"]
		manifests[w.pkg] = m
	}
	if t.Failed() {
		return
	}

	mcp, core := manifests["@playwright/mcp"], manifests["playwright-core"]
	if dep := mcp.Dependencies["playwright-core"]; dep != pins["PLAYWRIGHT_CORE_VERSION"] {
		t.Errorf("@playwright/mcp %s depends on playwright-core %q, and the pin is %s: the pin file's lines move "+
			"together, from scripts/playwright-mcp-pin-check.sh's report", mcp.Version, dep, pins["PLAYWRIGHT_CORE_VERSION"])
	}
	if len(core.Dependencies) != 0 {
		t.Errorf("playwright-core %s depends on %v, and the tree ships it alone: read what the release added before "+
			"shipping it", core.Version, core.Dependencies)
	}
	var registry struct {
		Browsers []build `json:"browsers"`
	}
	if err := json.Unmarshal(core.browsers, &registry); err != nil {
		t.Fatalf("playwright-core's browsers.json: %v", err)
	}
	i := slices.IndexFunc(registry.Browsers, func(b build) bool { return b.Name == "chromium" })
	if i < 0 {
		t.Fatal("playwright-core's browsers.json names no chromium")
	}
	if c := registry.Browsers[i]; c.Revision != browser.ExpectedChromiumRevision || c.BrowserVersion != browser.ExpectedChromiumVersion {
		t.Errorf("playwright-core %s expects Chromium %s (revision %s), and internal/browser says %s (revision %s)",
			core.Version, c.BrowserVersion, c.Revision, browser.ExpectedChromiumVersion, browser.ExpectedChromiumRevision)
	}
}

// manifest is what the test reads of a tarball's package.json, and the
// browsers.json beside it, where the package has one.
type manifest struct {
	Name         string            `json:"name"`
	Version      string            `json:"version"`
	Dependencies map[string]string `json:"dependencies"`
	browsers     []byte
}

// build is one browser playwright-core's browsers.json names.
type build struct {
	Name           string `json:"name"`
	Revision       string `json:"revision"`
	BrowserVersion string `json:"browserVersion"`
}

// packageFiles is each named file at the top of an npm tarball's package/,
// failing t where a tarball is no gzipped tar or lacks package.json.
func packageFiles(t *testing.T, name string, body []byte, files ...string) map[string][]byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	out := map[string][]byte{}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, f := range files {
			if h.Name == "package/"+f {
				if out[f], err = io.ReadAll(tr); err != nil {
					t.Fatalf("%s: %s: %v", name, h.Name, err)
				}
			}
		}
	}
	if out["package.json"] == nil {
		t.Fatalf("%s holds no package/package.json", name)
	}
	return out
}
