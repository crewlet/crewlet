package cliprofile

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
)

// THE REFERENCE BOX IMAGE INSTALLS ONLY A BUILD ITS RUNNER WAS MEASURED ON.
//
// images/coding pins each coding CLI the engine has a runner for, and a box
// runs that CLI with flags, output events and a sign-in this package's
// profile was written against. A pin that moved past every build the profile
// names ran an unmeasured CLI in every container box, and the first sign was
// an agent's run failing on a flag the new build renamed. Dependabot moves the
// pin on its own and the bump merges once `ci` is green, so this is the check
// that keeps it red until somebody measures the new build and names it in the
// profile's `written_for`, in the same change.
//
// The lock is held to the manifest too: `npm ci` installs the lock, so a
// manifest that says one version over a lock that holds another would pass
// this check and ship the other.
func TestTheCodingImagePinsOnlyMeasuredBuilds(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(sourcetree.Root(t), "images", "coding")

	var manifest struct {
		Dependencies map[string]string `json:"dependencies"`
	}
	readJSON(t, filepath.Join(dir, "package.json"), &manifest)
	var lock struct {
		Packages map[string]struct {
			Version      string            `json:"version"`
			Dependencies map[string]string `json:"dependencies"`
		} `json:"packages"`
	}
	readJSON(t, filepath.Join(dir, "package-lock.json"), &lock)

	// Every package the image installs, by the profile that drives it. A
	// package added to the manifest without a profile here is a CLI no
	// runner reads, and is refused below rather than left unchecked.
	profiles := map[string]string{
		"@anthropic-ai/claude-code": "claude-code",
		"@opencode/cli":             "opencode",
	}
	exact := regexp.MustCompile(`^\d+\.\d+\.\d+$`)
	measured := regexp.MustCompile(`\d+\.\d+\.\d+`)
	for pkg, pin := range manifest.Dependencies {
		name, ok := profiles[pkg]
		if !ok {
			t.Errorf("images/coding installs %s, which no coding-agent profile drives", pkg)
			continue
		}
		if !exact.MatchString(pin) {
			t.Errorf("images/coding pins %s at %q; pin an exact release", pkg, pin)
			continue
		}
		if got := lock.Packages["node_modules/"+pkg].Version; got != pin {
			t.Errorf("package-lock.json holds %s %q, the manifest pins %q: run "+
				"`npm install --package-lock-only` in images/coding", pkg, got, pin)
		}
		profile, ok := Builtin(name)
		if !ok {
			t.Fatalf("no built-in profile %q", name)
		}
		if !slices.Contains(measured.FindAllString(profile.WrittenFor, -1), pin) {
			t.Errorf("images/coding pins %s %s, and the %q profile was measured on none "+
				"of that: it names %q. Measure the new build and name it in the "+
				"profile's written_for, or keep the pin", pkg, pin, name, profile.WrittenFor)
		}
	}
	for pkg := range profiles {
		if _, ok := manifest.Dependencies[pkg]; !ok {
			t.Errorf("images/coding does not install %s, whose runner the engine ships", pkg)
		}
	}
}

func readJSON(t *testing.T, path string, into any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}
