package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// moduleDir lays out a module root holding the named files.
func moduleDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, text := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
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
	toolchain := module{Path: "Go toolchain", Version: "go1.99", Dir: moduleDir(t, map[string]string{
		"LICENSE": "Go license text", "PATENTS": "Go patent grant", "README.md": "not a notice",
	})}
	server := module{Path: "example.com/server", Version: "v2.0.0", Dir: moduleDir(t, map[string]string{
		"LICENSE": "Apache License 2.0 text\n\n", "NOTICE": "Copyright the server authors", "main.go": "package x",
	})}
	lib := module{Path: "example.com/lib", Version: "v1.0.0", Dir: moduleDir(t, map[string]string{
		"COPYING.txt": "BSD text", "LICENSE-MIT": "MIT text",
	})}

	var out bytes.Buffer
	if err := render(&out, toolchain, merge([]module{server, lib})); err != nil {
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
	for _, unwanted := range []string{"not a notice", "package x"} {
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

// A MODULE WITH NO NOTICE STOPS THE RELEASE, naming every one of them.
func TestAModuleWithNoLicenseIsRefusedByName(t *testing.T) {
	t.Parallel()
	toolchain := module{Path: "Go toolchain", Version: "go1.99", Dir: moduleDir(t, map[string]string{"LICENSE": "x"})}
	bare := []module{
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

// THE LIST IS WHAT IS LINKED: the main module and the standard library are
// not third-party modules, a replacement is read from where it was linked, and
// a module listed for several targets appears once.
func TestTheListIsTheLinkedThirdPartyModules(t *testing.T) {
	t.Parallel()
	stream := `
{"ImportPath": "fmt", "Standard": true}
{"ImportPath": "github.com/crewlet/crewlet/cmd/crewlet", "Module": {"Path": "github.com/crewlet/crewlet", "Main": true, "Dir": "/src"}}
{"ImportPath": "example.com/a/pkg", "Module": {"Path": "example.com/a", "Version": "v1.2.0", "Dir": "/cache/a@v1.2.0"}}
{"ImportPath": "example.com/a/other", "Module": {"Path": "example.com/a", "Version": "v1.2.0", "Dir": "/cache/a@v1.2.0"}}
{"ImportPath": "example.com/b", "Module": {"Path": "example.com/b", "Version": "v1.0.0", "Dir": "/cache/b@v1.0.0",
  "Replace": {"Path": "example.com/fork/b", "Version": "v1.0.1", "Dir": "/cache/fork/b@v1.0.1"}}}
`
	listed, err := parseList(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("parseList: %v", err)
	}
	got := merge(listed, listed[:1])
	want := []module{
		{Path: "example.com/a", Version: "v1.2.0", Dir: "/cache/a@v1.2.0"},
		{Path: "example.com/b", Version: "v1.0.1", Dir: "/cache/fork/b@v1.0.1"},
	}
	if len(got) != len(want) {
		t.Fatalf("modules = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("module %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// A LINKED MODULE WITH NO SOURCE ON DISK is an error that says what to run,
// never a section silently left out.
func TestAModuleWithNoSourceDirectoryIsAnError(t *testing.T) {
	t.Parallel()
	stream := `{"ImportPath": "example.com/a", "Module": {"Path": "example.com/a", "Version": "v1.0.0"}}`
	if _, err := parseList(strings.NewReader(stream)); err == nil || !strings.Contains(err.Error(), "go mod download") {
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
