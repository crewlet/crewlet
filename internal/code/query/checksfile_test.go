package query_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/query"
)

// checksFileQ is the home's read of a commit's checks file — the engine's own,
// which names no asker.
func checksFileQ(repo, commit string) code.GitQuery {
	return code.GitQuery{Kind: code.GitChecksFile, RepoID: repo, Home: true,
		ChecksFile: &code.ChecksFileQuery{Commit: commit}}
}

// A COMMIT'S CHECKS FILE IS READ WHOLE, AS GIT HOLDS IT, AT THE COMMIT ASKED:
// its bytes exact — an invalid UTF-8 byte included, which a text answer would
// have replaced and the parse must see — and nothing where the commit holds
// none, the path's own object where it is no file, and the size alone where
// the parse refuses the file by its size. A commit after it that changed the
// file answers its own, so a read is of the commit it names and no other.
func TestACommitsChecksFileIsReadWholeAtThatCommit(t *testing.T) {
	t.Parallel()
	x := newFixture(t, codetest.SoloOptions{}, codetest.Settings())
	none := x.commit(map[string]string{"README.md": "hello\n"}, "no checks")
	first := "checks:\n  - name: unit\n    run: make test\n    timeout_seconds: 60\n# caf\xe9\n"
	defined := x.commit(map[string]string{code.ChecksFilePath: first}, "checks")
	second := "checks:\n  - name: lint\n    run: make lint\n    timeout_seconds: 60\n"
	changed := x.commit(map[string]string{code.ChecksFilePath: second}, "other checks")
	big := x.commit(map[string]string{code.ChecksFilePath: strings.Repeat("#", code.MaxChecksFileBytes+1)},
		"a file past the bound")
	x.git("rm", "-q", code.ChecksFilePath)
	dir := x.commit(map[string]string{code.ChecksFilePath + "/inner": "a directory\n"}, "a directory")
	repo := x.repo(codetest.RepoName)
	s := x.server(0, query.Options{})

	if a := mustRead(t, s, checksFileQ(repo, none)).ChecksFile; a == nil || a.OID != "" || a.Raw != nil {
		t.Errorf("a commit holding no checks file answered %+v, want nothing at the path", a)
	}
	for _, c := range []struct{ commit, want string }{{defined, first}, {changed, second}} {
		a := mustRead(t, s, checksFileQ(repo, c.commit)).ChecksFile
		if a == nil || a.Type != "blob" || a.Commit != c.commit || !bytes.Equal(a.Raw, []byte(c.want)) ||
			a.Size != int64(len(c.want)) {
			t.Errorf("the checks file at %s answered %+v, want its %d bytes exactly: %q", c.commit, a,
				len(c.want), c.want)
		}
	}
	if a := mustRead(t, s, checksFileQ(repo, big)).ChecksFile; a == nil || a.Type != "blob" ||
		a.Size != code.MaxChecksFileBytes+1 || a.Raw != nil {
		t.Errorf("a checks file past MaxChecksFileBytes answered %+v, want its size and no bytes", a)
	}
	if a := mustRead(t, s, checksFileQ(repo, dir)).ChecksFile; a == nil || a.Type != "tree" || a.OID == "" ||
		a.Raw != nil {
		t.Errorf("a directory at the checks file's path answered %+v, want the tree it is", a)
	}

	// THE ENGINE'S OWN READ names no asker: one that names somebody is no
	// read of its kind.
	q := checksFileQ(repo, defined)
	q.Asker = person
	if _, err := s.Read(t.Context(), q); !errors.Is(err, code.ErrInvalid) {
		t.Errorf("a checks file read naming an asker answered %v, want ErrInvalid", err)
	}
}
