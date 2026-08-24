package envfile_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// THE GUARD: nobody builds an assignment line by hand.
//
// This grammar is only worth having if it is the ONLY one. The failure it
// prevents is silent and positional — a bare space ends the assignment,
// `source` runs the remainder as a command and abandons every credential
// below it — so a second implementation does not announce itself. It shows
// up as a company whose seats defined after some particular token stop
// authenticating.
//
// A scan of the tree is the only thing that can catch that, because the
// second implementation is always one line long and always looks reasonable.
//
// # It is scoped to files that touch a .env, deliberately
//
// The tree is full of legitimate KEY=VALUE building that has nothing to do
// with this grammar: every exec.Cmd.Env entry is one. Those go to execve as
// raw strings — no shell, no dotenv reader, no quoting — so applying this
// package to them would be actively wrong.
//
// What distinguishes the case that matters is the DESTINATION: text written
// to a credential file that `source` and a dotenv loader both read. So the
// scan looks only at files that mention one, which is a boundary a reader
// can check and an author cannot cross by accident.
var handBuilt = regexp.MustCompile(
	`fmt\.(Sprintf|Fprintf)\([^)]*"[^"]*%s\s*=\s*%[svq]|` +
		`"export "\s*\+`)

// touchesEnvFile marks a source file as writing or rewriting a .env.
var touchesEnvFile = regexp.MustCompile(`\.env\b|envFile|EnvFile|envfile\.`)

func TestNobodyBuildsAnAssignmentByHand(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	var offenders []string

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// The grammar's own package is where the one implementation
			// lives, and vendored trees are not ours to police.
			switch d.Name() {
			case "envfile", ".git", "node_modules", "vendor", "schema":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !touchesEnvFile.Match(body) {
			return nil
		}
		for i, line := range strings.Split(string(body), "\n") {
			if handBuilt.MatchString(line) {
				rel, _ := filepath.Rel(root, path)
				offenders = append(offenders,
					rel+":"+itoa(i+1)+": "+strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(offenders) > 0 {
		t.Fatalf("these build an env assignment by hand in a file that writes "+
			"a .env; use envfile.FormatAssignment, which is the only form "+
			"both a dotenv reader and `source` agree on:\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

// AND THE GUARD ITSELF WORKS, which a scan that silently matches nothing
// cannot demonstrate. A regex narrowed until it is green is the way this
// kind of test rots.
func TestTheGuardRecognisesAHandBuiltAssignment(t *testing.T) {
	t.Parallel()
	for _, line := range []string{
		`fmt.Fprintf(w, "%s=%s\n", name, token)`,
		`fmt.Sprintf("%s=%q", name, token)`,
		`line := "export " + name + "=" + token`,
	} {
		if !handBuilt.MatchString(line) {
			t.Errorf("the guard does not recognise %q", line)
		}
	}
	// And does NOT recognise a process-environment pair, which is the
	// false positive that made the first version of this useless.
	for _, line := range []string{
		`cmd.Env = append(os.Environ(), key+"="+value)`,
		`out = append(out, k+"="+v)`,
	} {
		if handBuilt.MatchString(line) {
			t.Errorf("the guard flags a process-environment pair: %q", line)
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// repoRoot walks up to the directory holding go.mod.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the working directory")
		}
		dir = parent
	}
}
