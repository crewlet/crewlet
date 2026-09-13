package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// moduleDir lays out a component root holding the named files, which may sit
// in subdirectories.
func moduleDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, text := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// EVERY NOTICE FILE OF EVERY MODULE, VERBATIM, and nothing else from it.
//
// A NOTICE beside an Apache license is the file the license obliges a
// redistributor to pass on, so dropping it while keeping LICENSE would look
// complete and not be.
func TestTheDocumentCarriesEachModulesOwnNotices(t *testing.T) {
	t.Parallel()
	toolchain := component{Path: "Go toolchain", Version: "go1.99", Dir: moduleDir(t, map[string]string{
		"LICENSE": "Go license text", "PATENTS": "Go patent grant", "README.md": "not a notice",
	})}
	server := component{Path: "example.com/server", Version: "v2.0.0", Dir: moduleDir(t, map[string]string{
		"LICENSE": "Apache License 2.0 text\n\n", "NOTICE": "Copyright the server authors", "main.go": "package x",
		// Source files whose names the notice pattern would otherwise take.
		"license.go": "package licensecode", "notice_amd64.s": "TEXT noticeasm",
	})}
	lib := component{Path: "example.com/lib", Version: "v1.0.0", Dir: moduleDir(t, map[string]string{
		"COPYING.txt": "BSD text", "LICENSE-MIT": "MIT text",
	})}

	var out bytes.Buffer
	if err := render(&out, toolchain, merge([]component{server, lib})); err != nil {
		t.Fatalf("render: %v", err)
	}
	doc := out.String()
	for _, want := range []string{
		"Go license text", "Go patent grant", "Apache License 2.0 text",
		"Copyright the server authors", "BSD text", "MIT text",
		"example.com/server v2.0.0", "example.com/lib v1.0.0", "Go toolchain go1.99",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("the document is missing %q:\n%s", want, doc)
		}
	}
	for _, unwanted := range []string{"not a notice", "package x", "licensecode", "noticeasm"} {
		if strings.Contains(doc, unwanted) {
			t.Errorf("the document carries %q, which is not a notice file", unwanted)
		}
	}
	// SORTED, toolchain first, so a reader finds a module by name and two
	// releases of one checkout differ only where their dependencies do.
	toolchainAt := strings.Index(doc, "Go toolchain go1.99")
	libAt := strings.Index(doc, "example.com/lib v1.0.0")
	serverAt := strings.Index(doc, "example.com/server v2.0.0")
	if toolchainAt >= libAt || libAt >= serverAt {
		t.Errorf("sections are out of order: toolchain %d, lib %d, server %d", toolchainAt, libAt, serverAt)
	}
}

// A LICENSE BESIDE A LINKED PACKAGE IS REPRODUCED, and one beside code the
// binary does not link is not.
//
// The case is real: the NATS server's internal/fastrand carries the LevelDB-Go
// authors' BSD license, which its root Apache license does not reproduce and
// which a binary redistribution has to carry. A generator that read module
// roots only shipped without it.
func TestANoticeBesideALinkedPackageIsCarried(t *testing.T) {
	t.Parallel()
	root := moduleDir(t, map[string]string{
		"LICENSE":                       "Apache text for the server",
		"internal/fastrand/LICENSE":     "BSD text for the borrowed hash",
		"internal/fastrand/fastrand.go": "package fastrand",
		"internal/unused/LICENSE":       "text for code nobody links",
		"internal/unused/unused.go":     "package unused",
		// The same text again under a vendored directory is written once.
		"vendor/golang.org/x/LICENSE":    "Apache text for the server",
		"vendor/golang.org/x/net/net.go": "package net",
	})
	server := component{
		Path: "example.com/server", Version: "v2.14.6", Dir: root,
		Packages: []string{
			filepath.Join(root, "internal", "fastrand"),
			filepath.Join(root, "vendor", "golang.org", "x", "net"),
		},
	}
	toolchain := component{Path: "Go toolchain", Version: "go1.99", Dir: moduleDir(t, map[string]string{"LICENSE": "Go"})}

	var out bytes.Buffer
	if err := render(&out, toolchain, merge([]component{server})); err != nil {
		t.Fatalf("render: %v", err)
	}
	doc := out.String()
	if !strings.Contains(doc, "--- internal/fastrand/LICENSE ---\n\nBSD text for the borrowed hash") {
		t.Errorf("the linked package's own license is missing or unlabelled:\n%s", doc)
	}
	if strings.Contains(doc, "code nobody links") {
		t.Errorf("a license beside a package the binary does not link was carried:\n%s", doc)
	}
	if n := strings.Count(doc, "Apache text for the server"); n != 1 {
		t.Errorf("identical text appears %d times, want once:\n%s", n, doc)
	}
	if strings.Index(doc, "--- LICENSE ---") > strings.Index(doc, "--- internal/fastrand/LICENSE ---") {
		t.Errorf("the root license does not come first:\n%s", doc)
	}
}

// A PACKAGE OUTSIDE ITS ROOT is an inconsistency to stop on, never a walk
// that wanders up the file system collecting whatever it finds.
func TestAPackageOutsideItsRootIsRefused(t *testing.T) {
	t.Parallel()
	root := moduleDir(t, map[string]string{"LICENSE": "x"})
	elsewhere := t.TempDir()
	_, err := noticesOf(component{Path: "example.com/m", Version: "v1.0.0", Dir: root, Packages: []string{elsewhere}})
	if err == nil || !strings.Contains(err.Error(), "not inside its root") {
		t.Errorf("err = %v, want a refusal naming the package outside the root", err)
	}
}

// A MODULE WITH NO NOTICE STOPS THE RELEASE, naming every one of them.
func TestAModuleWithNoLicenseIsRefusedByName(t *testing.T) {
	t.Parallel()
	toolchain := component{Path: "Go toolchain", Version: "go1.99", Dir: moduleDir(t, map[string]string{"LICENSE": "x"})}
	bare := []component{
		{Path: "example.com/one", Version: "v1.0.0", Dir: moduleDir(t, map[string]string{"README": "no license"})},
		{Path: "example.com/two", Version: "v1.0.0", Dir: moduleDir(t, nil)},
	}
	err := render(&bytes.Buffer{}, toolchain, bare)
	if err == nil {
		t.Fatal("a module with no license file was rendered without one")
	}
	for _, name := range []string{"example.com/one v1.0.0", "example.com/two v1.0.0"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the refusal does not name %s: %v", name, err)
		}
	}
}

// THE LIST IS WHAT IS LINKED: the main module is not a third-party module, the
// standard library's packages belong to the toolchain, a replacement is read
// from where it was linked, and a module listed for several packages or
// targets appears once with every package directory.
func TestTheListIsTheLinkedThirdPartyModules(t *testing.T) {
	t.Parallel()
	stream := `
{"ImportPath": "fmt", "Dir": "/goroot/src/fmt", "Standard": true}
{"ImportPath": "github.com/crewlet/crewlet/cmd/crewlet", "Dir": "/src/cmd/crewlet", "Module": {"Path": "github.com/crewlet/crewlet", "Main": true, "Dir": "/src"}}
{"ImportPath": "example.com/a/pkg", "Dir": "/cache/a@v1.2.0/pkg", "Module": {"Path": "example.com/a", "Version": "v1.2.0", "Dir": "/cache/a@v1.2.0"}}
{"ImportPath": "example.com/a/other", "Dir": "/cache/a@v1.2.0/other", "Module": {"Path": "example.com/a", "Version": "v1.2.0", "Dir": "/cache/a@v1.2.0"}}
{"ImportPath": "example.com/b", "Dir": "/cache/fork/b@v1.0.1", "Module": {"Path": "example.com/b", "Version": "v1.0.0", "Dir": "/cache/b@v1.0.0",
  "Replace": {"Path": "example.com/fork/b", "Version": "v1.0.1", "Dir": "/cache/fork/b@v1.0.1"}}}
`
	listed, std, err := parseList(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("parseList: %v", err)
	}
	if len(std) != 1 || std[0] != "/goroot/src/fmt" {
		t.Errorf("standard package directories = %v, want the one fmt lives in", std)
	}
	got := merge(listed, listed[:1])
	want := []component{
		{Path: "example.com/a", Version: "v1.2.0", Dir: "/cache/a@v1.2.0",
			Packages: []string{"/cache/a@v1.2.0/other", "/cache/a@v1.2.0/pkg"}},
		{Path: "example.com/b", Version: "v1.0.1", Dir: "/cache/fork/b@v1.0.1",
			Packages: []string{"/cache/fork/b@v1.0.1"}},
	}
	if len(got) != len(want) {
		t.Fatalf("modules = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].Path != want[i].Path || got[i].Version != want[i].Version || got[i].Dir != want[i].Dir ||
			strings.Join(got[i].Packages, ",") != strings.Join(want[i].Packages, ",") {
			t.Errorf("module %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// A LINKED MODULE WITH NO SOURCE ON DISK is an error that says what to run,
// never a section silently left out.
func TestAModuleWithNoSourceDirectoryIsAnError(t *testing.T) {
	t.Parallel()
	stream := `{"ImportPath": "example.com/a", "Module": {"Path": "example.com/a", "Version": "v1.0.0"}}`
	if _, _, err := parseList(strings.NewReader(stream)); err == nil || !strings.Contains(err.Error(), "go mod download") {
		t.Errorf("err = %v, want a refusal naming go mod download", err)
	}
}

func TestTargetsMustBeGOOSSlashGOARCH(t *testing.T) {
	t.Parallel()
	got, err := parseTargets("linux/amd64, darwin/arm64")
	if err != nil || len(got) != 2 || got[1] != (target{"darwin", "arm64"}) {
		t.Errorf("parseTargets = %+v, %v", got, err)
	}
	for _, bad := range []string{"linux", "/amd64", "linux/", ""} {
		if _, err := parseTargets(bad); err == nil {
			t.Errorf("parseTargets(%q) accepted a malformed target", bad)
		}
	}
}
