package sandbox_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sourcetree"

	"gopkg.in/yaml.v3"
)

// THE GUIDE'S STOCK-IMAGE RECIPE SAYS WHICH OF ITS FAILURES A WAIT CLEARS:
// docs/concepts/code-sandbox.md recommends it, and it downloads on every new
// box. A registry or download server still failing on its side after the
// command's own retries exits 75; everything else (a pin naming nothing, a
// refused connection, a timeout, a bad checksum) keeps its own status, a
// setup a person fixes. The step runs as the page writes it, under sh, with
// stubbed npm, curl and checksum tool and nothing on PATH that reaches a
// network.
//
// Mutation: `|| exit 75` on the npm install or a fetch reddens the 404,
// refused and timeout rows; dropping either `exit 75` the 5xx rows; `|| exit
// 75` on the checksum the mismatch row; dropping npm's own words the npm rows.
func TestTheGuidesStockImageRecipeIsToldByWhoClearsIt(t *testing.T) {
	t.Parallel()
	step := stockImageRecipe(t)
	stubs := t.TempDir()
	// Written where no fork can hold one open ([sandbox.WriteExecutable]):
	// a stub a concurrent fork inherited open for writing is one Linux
	// refuses to run (ETXTBSY).
	for name, body := range recipeStubs {
		sandbox.WriteExecutable(t, filepath.Join(stubs, name), body)
	}
	// PATH is this one directory: the stubs plus the host's own copy of each
	// tool that reaches nothing ([recipeTools]). A shell that cannot run a
	// stub (a noexec TMPDIR) would otherwise go on down PATH to the host's
	// real npm or curl; here the case fails instead.
	for _, tool := range recipeTools {
		host, err := exec.LookPath(tool)
		if err != nil {
			t.Fatalf("the recipe runs %s, which this host does not have: %v", tool, err)
		}
		if err := os.Symlink(host, filepath.Join(stubs, tool)); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name string
		env  []string
		want sandbox.Why // "" for a step that provisions the box
	}{
		{"both install", []string{"TARBALL=200", "SUMS=200", "SUM_MATCHES=1"}, ""},
		{"the registry still answers 503", []string{"NPM_CODE=E503"}, sandbox.WhySetupUnavailable},
		{"the registry still answers 429", []string{"NPM_CODE=E429"}, sandbox.WhySetupUnavailable},
		{"no such version", []string{"NPM_CODE=ETARGET"}, sandbox.WhyConfiguration},
		{"the registry answers 404", []string{"NPM_CODE=E404"}, sandbox.WhyConfiguration},
		{"the registry refuses the connection", []string{"NPM_CODE=ECONNREFUSED"}, sandbox.WhyConfiguration},
		{"the tarball still answers 503", []string{"TARBALL=503", "SUMS=200"}, sandbox.WhySetupUnavailable},
		{"the checksums still answer 502", []string{"TARBALL=200", "SUMS=502"}, sandbox.WhySetupUnavailable},
		{"the tarball answers 404", []string{"TARBALL=404", "SUMS=200"}, sandbox.WhyConfiguration},
		{"GitHub refuses the connection", []string{"TARBALL=exit:7", "SUMS=200"}, sandbox.WhyConfiguration},
		{"the tarball times out", []string{"TARBALL=exit:28", "SUMS=200"}, sandbox.WhyConfiguration},
		{"a checksum that doesn't match", []string{"TARBALL=200", "SUMS=200", "SUM_MATCHES=0"}, sandbox.WhyConfiguration},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			env := append([]string{"PATH=" + stubs, "HOME=" + home, "TMPDIR=" + home}, tc.env...)
			box := sandbox.NewFakeSandbox("b1")
			box.ExecFunc = func(ctx context.Context, cmd string, _ sandbox.ExecOptions) (sandbox.ExecResult, error) {
				run := exec.CommandContext(ctx, "/bin/sh", "-c", cmd)
				run.Env = env
				var stderr strings.Builder
				run.Stderr = &stderr
				err := run.Run()
				var exit *exec.ExitError
				if errors.As(err, &exit) {
					return sandbox.ExecResult{ExitCode: exit.ExitCode(), Stderr: stderr.String()}, nil
				}
				return sandbox.ExecResult{Stderr: stderr.String()}, err
			}
			err := sandbox.ApplySetup(t.Context(), box, []sandbox.SetupStep{step}, nil)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("the step failed: %v", err)
				}
				return
			}
			if why, ok := sandbox.WhyOf(err); !ok || why != tc.want {
				t.Errorf("classed %q (%v), want %q: %v", why, ok, tc.want, err)
			}
			// NPM'S OWN WORDS STILL REACH THE OPERATOR: the step reads them
			// to decide its status, and hands them on to the failure.
			var failed *sandbox.SetupError
			if strings.HasPrefix(tc.env[0], "NPM_CODE=") &&
				(!errors.As(err, &failed) || !strings.Contains(failed.Detail, "npm error code")) {
				t.Errorf("npm's own account of the failure is gone from it: %v", err)
			}
		})
	}
}

// stockImageRecipe is the guide's stock-image setup step as the page writes
// it: the step that installs the harness with npm and gh with curl.
func stockImageRecipe(t *testing.T) sandbox.SetupStep {
	t.Helper()
	page, err := os.ReadFile(filepath.Join(sourcetree.Root(t), "docs", "concepts", "code-sandbox.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, block := range regexp.MustCompile("(?s)```yaml\n(.*?)```").FindAllStringSubmatch(string(page), -1) {
		var doc struct {
			Providers struct {
				Sandbox struct {
					Setup []sandbox.SetupStep `yaml:"setup"`
				} `yaml:"sandbox"`
			} `yaml:"providers"`
		}
		if yaml.Unmarshal([]byte(block[1]), &doc) != nil {
			continue
		}
		for _, step := range doc.Providers.Sandbox.Setup {
			joined := strings.Join(step.Commands, "\n")
			if strings.Contains(joined, "npm install") && strings.Contains(joined, "curl") {
				return step
			}
		}
	}
	t.Fatal("docs/concepts/code-sandbox.md no longer carries a setup step that installs with npm and curl")
	return sandbox.SetupStep{}
}

// recipeTools are the tools the recipe runs that reach neither a network nor a
// machine's architecture, so each case runs the host's own.
var recipeTools = []string{"cat", "grep", "mkdir", "mktemp"}

// recipeStubs are the tools the recipe runs that reach a network or a
// machine's architecture, each answering as the case's environment says.
var recipeStubs = map[string]string{
	// npm as its own retries ended: NPM_CODE is the error code it prints,
	// and none installs.
	"npm": `#!/bin/sh
if [ -n "${NPM_CODE:-}" ]; then
	echo "npm error code $NPM_CODE" >&2
	echo "npm error A complete log of this run can be found in: $HOME/.npm/_logs/debug-0.log" >&2
	exit 1
fi
echo "added 3 packages in 2s"
`,
	// curl as its retries ended: TARBALL and SUMS are the last answer's
	// status, or exit:N for one that never reached HTTP (a refused
	// connection, a timeout), which curl reports as 000 and exit status N.
	"curl": `#!/bin/sh
out=
url=
while [ $# -gt 0 ]; do
	case $1 in
	-o) out=$2; shift ;;
	-w|--retry|--retry-delay) shift ;;
	-*) ;;
	*) url=$1 ;;
	esac
	shift
done
case $url in
*_checksums.txt) answer=$SUMS ;;
*) answer=$TARBALL ;;
esac
case $answer in
exit:*) printf 000; exit "${answer#exit:}" ;;
200)
	v=${url##*/download/v}; v=${v%%/*}
	printf '%s  %s\n' 0123abcd "gh_${v}_linux_amd64.tar.gz" >"$out"
	printf 200 ;;
*) echo "curl: (22) The requested URL returned error: $answer" >&2; printf %s "$answer"; exit 22 ;;
esac
`,
	// The checksum check, as SUM_MATCHES says the download came out.
	"sha256sum": `#!/bin/sh
read -r _ file
if [ "${SUM_MATCHES:-}" = 1 ]; then echo "$file: OK"; exit 0; fi
echo "$file: FAILED"
echo "sha256sum: WARNING: 1 computed checksum did NOT match" >&2
exit 1
`,
	"dpkg": "#!/bin/sh\necho amd64\n",
	"tar":  "#!/bin/sh\nexit 0\n",
	"mv":   "#!/bin/sh\nexit 0\n",
}
