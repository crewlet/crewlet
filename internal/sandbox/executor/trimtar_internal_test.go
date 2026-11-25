package executor

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// trimLastLine is a stand-in [trimmer] for [trimTar]'s own cases: release 9.9.9,
// and a cut keeping a record's last line alone, where it has two or more.
type trimLastLine struct{}

func (trimLastLine) trimRelease() string { return "9.9.9" }

func (trimLastLine) trimRecord(src, dst string) string {
	return `[ "$(wc -l < ` + src + ` | tr -d ' ')" -gt 1 ] && tail -n 1 ` + src + ` > ` + dst
}

// trimTarMember is the record [trimTarRun] packs, under its home, and
// trimTarBeside the member it packs beside it, a file in a directory.
const (
	trimTarMember     = "rec/session.jsonl"
	trimTarBeside     = "rec/beside/index.md"
	trimTarBesideBody = "# beside\n"
)

// trimTarRun packs record in a box of its own as a tar-in-place pack does
// ([trimTar] around tar's own archive of the record and [trimTarBeside]'s
// directory), written by release, at at, and returns the archive's members,
// its size and the word left beside it ("" for none).
func trimTarRun(t *testing.T, record []byte, release string, at trimPoints) (members map[string]string,
	size int64, mark string) {
	t.Helper()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, filepath.Dir(trimTarBeside)), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string][]byte{trimTarMember: record, trimTarBeside: []byte(trimTarBesideBody)} {
		if err := os.WriteFile(filepath.Join(home, name), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	q := shellword.Quote
	staged, dest := filepath.Join(home, "staged"), filepath.Join(home, "capture.tar.gz")
	tar := `tar -czf ` + q(staged+"/capture") + ` -C ` + q(home) + ` "$@"`
	script := trimRelVar + "=" + q(release) + "\n" + `set -- ` + trimTarMember + ` ` + filepath.Dir(trimTarBeside) + "\n" +
		`rm -rf ` + q(staged) + ` && mkdir -p ` + q(staged) + ` && ` +
		trimTar(trimLastLine{}, at, q(home), trimTarMember, staged, tar) + ` && ` + trimUnmark(dest) + ` && mv -f ` +
		q(staged+"/capture") + ` ` + q(dest) + ` && ` + trimMarked(dest)
	if _, errText, code := codexShell(t, home, script); code != 0 {
		t.Fatalf("the pack exited %d: %s", code, errText)
	}
	archive := codexFile(t, dest)
	word, err := os.ReadFile(trimMarkOf(dest))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return codexMembers(t, archive), int64(len(archive)), string(word)
}

// A TAR IN PLACE CUTS ONLY PAST A POINT, AND ONLY ITS OWN RELEASE'S RECORD: below
// both points, on another release, or where no line can be cut, the archive is
// the pack's own tar, the record as it lies (its torn tail too); past the
// plaintext point, or with an archive past the packed one, it holds the cut of
// the record's whole lines under the same name beside the pack's other members,
// and the pack says so beside it in a word the take reads back.
//
// Mutation: cut below a point, skip the release's check, cut the live record
// rather than a copy of its whole lines, skip the re-tar past the packed point,
// leave the other members out of it, or leave no word, and a case goes red.
func TestATarInPlaceCutsOnlyPastAPointAndOnlyItsOwnRelease(t *testing.T) {
	t.Parallel()
	whole := []byte("{\"n\":1}\n{\"n\":2}\n{\"n\":3}\n")
	torn := append(append([]byte(nil), whole...), `{"n":4,"t`...)
	cut := []byte("{\"n\":3}\n")
	for name, c := range map[string]struct {
		record    []byte
		release   string
		at        trimPoints
		want      []byte
		trimmedOf int
	}{
		"below both points":        {torn, "9.9.9", trimPoints{plain: int64(len(torn)), packed: 1 << 40}, torn, 0},
		"another release":          {torn, "9.9.8", trimEverything, torn, 0},
		"no release":               {torn, "", trimEverything, torn, 0},
		"nothing to cut":           {[]byte("{\"n\":1}\n"), "9.9.9", trimEverything, []byte("{\"n\":1}\n"), 0},
		"past the plaintext point": {torn, "9.9.9", trimEverything, cut, len(whole)},
		"past the packed point":    {torn, "9.9.9", trimPoints{plain: 1 << 40, packed: 1}, cut, len(whole)},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			members, size, mark := trimTarRun(t, c.record, c.release, c.at)
			if !bytes.Equal([]byte(members[trimTarMember]), c.want) {
				t.Errorf("the archive holds %q, want %q", members[trimTarMember], c.want)
			}
			if members[trimTarBeside] != trimTarBesideBody {
				t.Errorf("the archive holds %q beside the record, want the member beside it as it lies",
					members[trimTarBeside])
			}
			want := sandbox.ArchiveTrim{}
			if c.trimmedOf > 0 {
				want = sandbox.ArchiveTrim{RecordBytes: int64(c.trimmedOf), KeptBytes: int64(len(c.want))}
			}
			if got := parseTrimMark(mark, size); got != want {
				t.Errorf("the pack's word %q reads as %+v, want %+v", mark, got, want)
			}
		})
	}
}

// A TAR IN PLACE THAT CANNOT COPY ITS RECORD PACKS IT AS ITS OWN TAR DOES: a
// record past the point with no whole line to copy is the pack's own archive,
// said untrimmed, never an empty member or a failed pack.
//
// Mutation: copy a record of no whole line, or fail the pack where the copy
// fails, and the case goes red.
func TestATarInPlaceThatCannotCopyItsRecordPacksItAsItsOwnTarDoes(t *testing.T) {
	t.Parallel()
	record := []byte(`{"n":1,"t`)
	members, _, mark := trimTarRun(t, record, "9.9.9", trimEverything)
	if members[trimTarMember] != string(record) || mark != "" {
		t.Errorf("the archive holds %q and says %q, want the record as it lies and no word", members[trimTarMember],
			mark)
	}
}
