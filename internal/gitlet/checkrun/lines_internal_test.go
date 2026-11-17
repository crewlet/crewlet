package checkrun

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// A LINE COUNT IS THE NEWLINES UP TO THE TAIL'S END: the box's command, run by a shell over a file,
// counts the newlines in the file's first size bytes and none past them, so a command still
// writing after its tail was read is not counted.
//
// Mutation: drop `head -c` and count the whole file (the third case counts 3, red).
func TestALineCountIsTheNewlinesUpToTheTailsEnd(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for i, tc := range []struct {
		content string
		size    int64
		want    int
	}{
		{"a\n", 2, 1}, {"a\nb", 3, 1}, {"a\nb\nc\n", 4, 2}, {strings.Repeat("x", 1<<20), 1 << 20, 0},
	} {
		file := filepath.Join(dir, strconv.Itoa(i))
		if err := os.WriteFile(file, []byte(tc.content), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command("/bin/sh", "-c", lineCount(file, tc.size)).Output()
		if err != nil {
			t.Fatalf("%q at %d: %v", tc.content, tc.size, err)
		}
		if got, err := strconv.Atoi(strings.TrimSpace(string(out))); err != nil || got != tc.want {
			t.Errorf("%.8q at %d counts %q, want %d", tc.content, tc.size, out, tc.want)
		}
	}
}
