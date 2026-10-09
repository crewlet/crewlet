package envfile_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
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

// What each pattern cannot match without, derived from the pattern itself
// (see [sourcetree.Required]). Neither expression has a literal prefix the
// regexp package can jump to, so over the whole tree each ran at the race
// detector's megabyte a second; a file holding none of these substrings is
// one the expression provably does not match, and is skipped unread by it.
var (
	handBuiltNeeds      = sourcetree.Required(handBuilt)
	touchesEnvFileNeeds = sourcetree.Required(touchesEnvFile)
)

// minInScope is the fewest files the scope may find in this tree. Forty-seven
// mention a .env today, forty-five of them through `.env` itself; a floor at
// under half of that survives a refactor that consolidates a few writers and
// fails the day the scope expression stops recognising its main spelling —
// which would otherwise leave this guard checking almost nothing, green.
const minInScope = 20

func TestNobodyBuildsAnAssignmentByHand(t *testing.T) {
	t.Parallel()
	scan := scanForHandBuilt(t, sourcetree.Root(t))
	// An absence is asserted by reading everything, and a walk that read
	// nothing asserts it just as confidently.
	if scan.files == 0 {
		t.Fatal("read no Go files — this guard was certifying nothing")
	}
	// AND A SCOPE THAT MATCHES NOTHING asserts it just as confidently too:
	// every offender lives in a file the scope admitted.
	if scan.inScope < minInScope {
		t.Fatalf("only %d of %d Go files were found to write a .env, under the "+
			"floor of %d — the scope expression has stopped recognising the "+
			"files this guard is for, so it is checking almost nothing",
			scan.inScope, scan.files, minInScope)
	}
	if len(scan.offenders) > 0 {
		t.Fatalf("these build an env assignment by hand in a file that writes "+
			"a .env; use envfile.FormatAssignment, which is the only form "+
			"both a dotenv reader and `source` agree on:\n  %s",
			strings.Join(scan.offenders, "\n  "))
	}
	t.Logf("%d Go files read, %d of them write a .env", scan.files, scan.inScope)
}

// envScan is what one walk of a tree found.
type envScan struct {
	// files is every Go file read, counted before anything is skipped.
	files int
	// inScope is the files that write a .env.
	inScope   int
	offenders []string
}

// scanForHandBuilt walks root for hand-built assignments in files that write
// a .env. The tree's guard and its control both run it, so the control
// certifies the scope, the prefilters and the line matcher together.
func scanForHandBuilt(t *testing.T, root string) envScan {
	t.Helper()
	var scan envScan
	err := sourcetree.Walk(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// The grammar's own package is where the one implementation
			// lives, and vendored trees are not ours to police.
			switch d.Name() {
			case "envfile", "vendor", "schema":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		scan.files++
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !writesEnvFile(body) {
			return nil
		}
		scan.inScope++
		if !handBuiltNeeds.Admits(body) {
			return nil
		}
		for i, line := range strings.Split(string(body), "\n") {
			if handBuiltLine(line) {
				rel, _ := filepath.Rel(root, path)
				scan.offenders = append(scan.offenders,
					filepath.ToSlash(rel)+":"+itoa(i+1)+": "+strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	return scan
}

// writesEnvFile reports whether a source file is in the guard's scope.
func writesEnvFile(body []byte) bool {
	return touchesEnvFileNeeds.Admits(body) && touchesEnvFile.Match(body)
}

// handBuiltLine reports whether one line builds an assignment by hand.
func handBuiltLine(line string) bool {
	return handBuiltNeeds.AdmitsString(line) && handBuilt.MatchString(line)
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
		if !handBuiltLine(line) {
			t.Errorf("the guard does not recognise %q", line)
		}
	}
	// And does NOT recognise a process-environment pair, which is the
	// false positive that made the first version of this useless.
	for _, line := range []string{
		`cmd.Env = append(os.Environ(), key+"="+value)`,
		`out = append(out, k+"="+v)`,
	} {
		if handBuiltLine(line) {
			t.Errorf("the guard flags a process-environment pair: %q", line)
		}
	}
}

// THE SCOPE, ON A TREE WHOSE VERDICT IS KNOWN — through the same walk, the
// same prefilters and the same matchers the guard runs.
//
// The scope is the half of this guard that can go inert unnoticed: an
// expression that stopped recognising a file writing a .env passes every
// hand-built line in it. So a file that writes one is planted with the very
// assignment the guard exists for, beside an exec environment built the same
// way, which is out of scope, and the grammar's own package, which is
// skipped.
func TestTheGuardFindsAHandBuiltAssignmentInAFileThatWritesAnEnv(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for path, body := range map[string]string{
		"internal/writer/writer.go": "package writer\n\n" +
			"func write(w io.Writer, k, v string) {\n" +
			"\t_ = os.WriteFile(\".env\", nil, 0o600)\n" +
			"\tfmt.Fprintf(w, \"%s=%s\\n\", k, v)\n" +
			"}\n",
		"internal/run/run.go": "package run\n\n" +
			"func env(k, v string) string { return fmt.Sprintf(\"%s=%s\", k, v) }\n",
		"internal/envfile/envfile.go": "package envfile\n\n" +
			"// .env\nvar _ = fmt.Sprintf(\"%s=%s\", \"k\", \"v\")\n",
	} {
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	scan := scanForHandBuilt(t, root)
	if scan.files != 2 || scan.inScope != 1 {
		t.Errorf("read %d files and found %d writing a .env; want 2 and 1 — the "+
			"writer, and not the exec environment beside it", scan.files, scan.inScope)
	}
	want := `internal/writer/writer.go:5: fmt.Fprintf(w, "%s=%s\n", k, v)`
	if len(scan.offenders) != 1 || scan.offenders[0] != want {
		t.Errorf("offenders = %q, want exactly %q", scan.offenders, want)
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
