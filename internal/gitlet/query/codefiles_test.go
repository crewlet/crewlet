package query_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/deployfiles"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/gitlettest"
	"github.com/crewlet/crewlet/internal/gitlet/query"
)

// codeFilesQ is the atlas's read of a commit's deploy and dependency files
// (the engine's own, which names no asker): the listing, or one file.
func codeFilesQ(repo, commit, file string) gitlet.GitQuery {
	return gitlet.GitQuery{Kind: gitlet.GitCodeFiles, RepoID: repo, Home: true,
		CodeFiles: &gitlet.CodeFilesQuery{Commit: commit, File: file}}
}

// A COMMIT'S DEPLOY AND DEPENDENCY FILES ARE LISTED BY THE CLOSED LIST, AND
// ONLY A FILE ON IT IS READ: the listing holds the regular files the list
// admits, in path order, at the commit asked, never a file off it (an .env, a
// values file, source code) and never a symbolic link at a listed path; one
// listed file is read whole, by its exact bytes; and a file off the list is
// refused before any git runs, whatever the commit holds there.
//
// Mutation: drop the Classify call from codeFilesList, and the listing names
// the .env file.
func TestACommitsDeployFilesAreListedByTheClosedListAndOnlyThoseAreRead(t *testing.T) {
	t.Parallel()
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	compose := "services:\n  api:\n    image: acme/api\n    environment:\n      TOKEN: never-read\n# caf\xe9\n"
	first := x.commit(map[string]string{
		"Dockerfile":                  "FROM scratch\n",
		"compose.yaml":                compose,
		"services/api/go.mod":         "module example.com/api\n",
		".env":                        "TOKEN=never-read\n",
		"services/api/.env":           "TOKEN=never-read\n",
		"charts/api/values.yaml":      "password: never-read\n",
		"k8s/secrets.yaml":            "kind: Secret\n",
		"main.go":                     "package main\n",
		"node_modules/x/package.json": "{}\n",
	}, "deploy files")
	x.git("rm", "-q", "compose.yaml")
	if err := os.Symlink(".env", filepath.Join(x.work, "fly.toml")); err != nil {
		t.Fatal(err)
	}
	second := x.commit(map[string]string{
		"k8s/api.yaml": "kind: Deployment\n",
		"k8s/big.yaml": strings.Repeat("#", deployfiles.MaxFileBytes+1),
	}, "a manifest, a link and a file past the bound")
	repo := x.repo(gitlettest.RepoName)
	s := x.server(0, query.Options{})

	paths := func(commit string) []string {
		t.Helper()
		a := mustRead(t, s, codeFilesQ(repo, commit, "")).CodeFiles
		if a == nil || a.Commit != commit || a.More != 0 {
			t.Fatalf("the listing at %s answered %+v", commit, a)
		}
		var out []string
		for _, f := range a.Files {
			if f.OID == "" {
				t.Errorf("%s is listed with no blob", f.Path)
			}
			out = append(out, f.Path)
		}
		return out
	}
	if got, want := paths(first), []string{"Dockerfile", "compose.yaml", "services/api/go.mod"}; !slices.Equal(got, want) {
		t.Errorf("the first commit lists %v, want %v", got, want)
	}
	// The link at fly.toml is a listed path and no regular file, so it is
	// not listed: its blob is the path it points at.
	if got, want := paths(second), []string{"Dockerfile", "k8s/api.yaml", "k8s/big.yaml", "services/api/go.mod"}; !slices.Equal(got, want) {
		t.Errorf("the second commit lists %v, want %v", got, want)
	}

	a := mustRead(t, s, codeFilesQ(repo, first, "compose.yaml")).CodeFiles
	if a == nil || a.File == nil || a.File.OID == "" || !bytes.Equal(a.Raw, []byte(compose)) ||
		a.File.Size != int64(len(compose)) {
		t.Errorf("compose.yaml at the first commit answered %+v, want its %d bytes exactly", a, len(compose))
	}
	if a := mustRead(t, s, codeFilesQ(repo, second, "compose.yaml")).CodeFiles; a == nil || a.File == nil ||
		a.File.OID != "" || a.Raw != nil {
		t.Errorf("a file the commit no longer holds answered %+v, want nothing at the path", a)
	}
	if a := mustRead(t, s, codeFilesQ(repo, second, "k8s/big.yaml")).CodeFiles; a == nil || a.File == nil ||
		a.File.Size != deployfiles.MaxFileBytes+1 || a.Raw != nil {
		t.Errorf("a file past the byte bound answered %+v, want its size and no bytes", a)
	}

	// OFF THE LIST IS NEVER READ: each is in the commit, and each is refused.
	for _, file := range []string{".env", "services/api/.env", "charts/api/values.yaml", "k8s/secrets.yaml", "main.go",
		"node_modules/x/package.json", "Dockerfile"} {
		if _, err := s.Read(t.Context(), codeFilesQ(repo, first, file)); !errors.Is(err, gitlet.ErrInvalid) {
			t.Errorf("a read of %s answered %v, want ErrInvalid", file, err)
		}
	}
	// THE ENGINE'S OWN READ names no asker.
	q := codeFilesQ(repo, first, "")
	q.Asker = person
	if _, err := s.Read(t.Context(), q); !errors.Is(err, gitlet.ErrInvalid) {
		t.Errorf("a code files read naming an asker answered %v, want ErrInvalid", err)
	}
}

// A LISTING IS BOUNDED: a commit holding more listed files than one read
// takes answers the first by path and counts the rest.
func TestACommitsDeployFilesListingIsBounded(t *testing.T) {
	t.Parallel()
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	files := map[string]string{}
	for i := range deployfiles.MaxFiles + 7 {
		files["s/"+strings.Repeat("a", i%5+1)+"/"+string(rune('a'+i%26))+strings.Repeat("x", i/26)+"/go.mod"] =
			"module example.com/x\n"
	}
	commit := x.commit(files, "a monorepo")
	a := mustRead(t, x.server(0, query.Options{}), codeFilesQ(x.repo(gitlettest.RepoName), commit, "")).CodeFiles
	if a == nil || len(a.Files) != deployfiles.MaxFiles || a.More != len(files)-deployfiles.MaxFiles {
		t.Fatalf("the listing holds %d files and counts %d more, want %d and %d", len(a.Files), a.More,
			deployfiles.MaxFiles, len(files)-deployfiles.MaxFiles)
	}
	if !slices.IsSortedFunc(a.Files, func(p, q gitlet.CodeFile) int { return strings.Compare(p.Path, q.Path) }) {
		t.Error("the listing is not in path order")
	}
}
