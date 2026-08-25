package envfile_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/envfile"
)

// THE VALUES A REAL TOKEN LOOKS LIKE. Each is here because a naive
// fmt.Sprintf("%s=%s") mangles it in a different way.
var awkwardTokens = map[string]string{
	"a plain token":       "glpat-abcdefghijklmnop",
	"base64 with padding": "c2VjcmV0LXZhbHVl==",
	"a literal dollar":    "sk-ant$notavariable",
	"a braced reference":  "token-${HOME}-suffix",
	"a space":             "two words",
	"a hash":              "abc#notacomment",
	"a backtick":          "abc`whoami`def",
	"a double quote":      `abc"def`,
	"a backslash":         `abc\ndef`,
	"a semicolon":         "abc;echo pwned",
	"an ampersand":        "abc&&echo pwned",
	"leading whitespace":  "  padded",
	"an equals sign":      "key=value",
	"a slash and plus":    "a/b+c=",
}

// A WRITTEN ASSIGNMENT SURVIVES A REAL `source`, which is the reader this
// grammar exists for: a bare space ends the assignment and the rest of the
// line becomes a command, and the shell then abandons every credential BELOW
// it in the file.
func TestEveryWrittenAssignmentSurvivesARealShell(t *testing.T) {
	t.Parallel()
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no shell on PATH to source the file with")
	}

	for name, value := range awkwardTokens {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			line, err := envfile.FormatAssignment("CREWLET_TEST_TOKEN", value)
			if err != nil {
				t.Fatalf("FormatAssignment(%q): %v", value, err)
			}
			dir := t.TempDir()
			path := filepath.Join(dir, ".env")
			// A SECOND ASSIGNMENT BELOW IT, because the failure this
			// guards against is positional: a broken line does not lose
			// its own credential, it loses every one after it.
			body := line + "\nexport CREWLET_TEST_SENTINEL='still-here'\n"
			if err = os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}

			out, err := exec.Command(shell, "-c",
				". "+path+`; printf '%s\n%s' "$CREWLET_TEST_TOKEN" "$CREWLET_TEST_SENTINEL"`,
			).CombinedOutput()
			if err != nil {
				t.Fatalf("sourcing %q failed: %v\n%s", line, err, out)
			}
			got := strings.SplitN(string(out), "\n", 2)
			if got[0] != value {
				t.Errorf("the shell read %q, want %q (line: %s)", got[0], value, line)
			}
			if len(got) < 2 || got[1] != "still-here" {
				t.Errorf("the assignment below was abandoned (line: %s)", line)
			}
		})
	}
}

// AND IT ROUND-TRIPS through this package's own reader, which is what makes
// a rotation replace a value rather than append a second assignment nobody
// can predict the winner of.
func TestEveryWrittenAssignmentParsesBack(t *testing.T) {
	t.Parallel()
	for name, value := range awkwardTokens {
		line, err := envfile.FormatAssignment("TOKEN", value)
		if err != nil {
			t.Fatalf("%s: FormatAssignment: %v", name, err)
		}
		gotName, gotValue, ok := envfile.ParseAssignment(line)
		if !ok {
			t.Errorf("%s: %q did not parse back", name, line)
			continue
		}
		if gotName != "TOKEN" || gotValue != value {
			t.Errorf("%s: round-tripped to %s=%q, want TOKEN=%q",
				name, gotName, gotValue, value)
		}
	}
}

// A VALUE THAT CANNOT BE WRITTEN SAFELY IS REFUSED, not escaped. The usual
// '\” splice is handled by `source` and mangled by several dotenv readers,
// so writing one would put a different credential in front of each reader —
// which is worse than refusing, because the provisioner can mint another.
func TestAnUnrepresentableValueIsRefused(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"has'quote", "has\nnewline", "has\rreturn"} {
		if _, err := envfile.FormatAssignment("TOKEN", value); !errors.Is(err, envfile.ErrUnrepresentable) {
			t.Errorf("FormatAssignment(%q) = %v, want ErrUnrepresentable", value, err)
		}
	}
}

func TestAVariableNeedsAUsableName(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"", "1TOKEN", "TO KEN", "TO-KEN", "TO.KEN", "TOKEN!"} {
		if _, err := envfile.FormatAssignment(name, "v"); err == nil {
			t.Errorf("%q was accepted as a variable name", name)
		}
	}
	for _, name := range []string{"T", "_T", "TOKEN_1", "a_b_2"} {
		if !envfile.NameOK(name) {
			t.Errorf("%q was refused as a variable name", name)
		}
	}
}

// A HAND-WRITTEN FILE IS READ BACK, in every quoting form. The file being
// rotated was usually written by a person, and an assignment this cannot
// recognise becomes a second one whose winner depends on the reader.
func TestAHandWrittenAssignmentIsRecognised(t *testing.T) {
	t.Parallel()
	for line, want := range map[string]struct{ name, value string }{
		`TOKEN=abc`:            {"TOKEN", "abc"},
		`export TOKEN=abc`:     {"TOKEN", "abc"},
		`  export  TOKEN=abc `: {"TOKEN", "abc"},
		`TOKEN='abc'`:          {"TOKEN", "abc"},
		`TOKEN="abc"`:          {"TOKEN", "abc"},
		`TOKEN=`:               {"TOKEN", ""},
		`TOKEN=''`:             {"TOKEN", ""},
	} {
		name, value, ok := envfile.ParseAssignment(line)
		if !ok {
			t.Errorf("%q did not parse", line)
			continue
		}
		if name != want.name || value != want.value {
			t.Errorf("%q parsed to %s=%q, want %s=%q",
				line, name, value, want.name, want.value)
		}
	}
}

func TestWhatIsNotAnAssignment(t *testing.T) {
	t.Parallel()
	for _, line := range []string{
		"", "   ", "# a comment", "TOKEN", "1TOKEN=abc",
		"TO-KEN=abc", "just some prose",
	} {
		if _, _, ok := envfile.ParseAssignment(line); ok {
			t.Errorf("%q was parsed as an assignment", line)
		}
	}
}
