package sandbox_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// A QUOTED WORD IS READ BACK AS IT WAS WRITTEN, whatever it holds: a POSIX
// shell given `printf %s <word>` prints exactly the value — its quotes, a
// command substitution, a backquote, a separator, a glob, a newline — and
// runs none of it. The values are the shapes a box's commands carry: a path a
// model named, a branch spelled from a tracker's key, a box home under an
// operator's state_dir.
//
// Mutation: quote without escaping the single quote inside — the quote rows
// end the word early and fail (and the substitution row runs `touch`).
func TestAShellWordIsReadBackLiterally(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, value := range []string{
		"",
		"plain",
		"dir with 'quote'",
		"'",
		"''",
		`a$(touch x)'b`,
		"`touch y`",
		"; touch z",
		`"$HOME" * ?`,
		"line one\nline two\n",
		`back\slash\'`,
		"tab\there",
		"ü and ☃",
	} {
		cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", "printf %s "+sandbox.ShellQuote(value))
		cmd.Dir = dir
		out, err := cmd.Output()
		if err != nil {
			t.Errorf("printf of %q: %v", value, err)
			continue
		}
		if string(out) != value {
			t.Errorf("the shell read %q back as %q", value, out)
		}
	}
	for _, name := range []string{"x", "y", "z"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Errorf("a quoted word ran a command: %s was created", name)
		}
	}
}
