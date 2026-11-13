package deployfiles

import (
	"path"
	"slices"
	"strings"
)

// Format is what a listed file is read as. A closed set.
type Format string

// The formats.
const (
	Dockerfile   Format = "dockerfile"
	Procfile     Format = "procfile"
	Compose      Format = "compose"
	Kubernetes   Format = "kubernetes"
	HelmChart    Format = "helm_chart"
	Terraform    Format = "terraform"
	Fly          Format = "fly"
	Render       Format = "render"
	PackageJSON  Format = "package_json"
	GoMod        Format = "go_mod"
	Requirements Format = "requirements"
	Pyproject    Format = "pyproject"
	Gemfile      Format = "gemfile"
	Cargo        Format = "cargo"
)

// Formats is every format.
var Formats = []Format{
	Dockerfile, Procfile, Compose, Kubernetes, HelmChart, Terraform, Fly, Render,
	PackageJSON, GoMod, Requirements, Pyproject, Gemfile, Cargo,
}

// Valid reports whether f is a member of the closed set.
func (f Format) Valid() bool { return slices.Contains(Formats, f) }

// Parsed reports whether a file of this format is read at all. A Dockerfile
// and a Procfile say what they say by being there: their lines are build
// arguments, environment values and commands, which nothing may read.
func (f Format) Parsed() bool { return f.Valid() && f != Dockerfile && f != Procfile }

const (
	// MaxFileBytes is the largest file read: 64 KiB, the bound of the one
	// other file the engine reads from a repository (its checks file). A
	// hand-written manifest is a few kibibytes; one past this is generated,
	// and is counted rather than read.
	MaxFileBytes = 64 << 10
	// MaxFiles is how many listed files one read of a repository takes: a
	// monorepo of a hundred services, each with a manifest and a dependency
	// file, fits. The rest are counted, in path order, never read.
	MaxFiles = 200
	// MaxPathBytes is the longest path listed, so a listing of MaxFiles is
	// bounded: no real manifest sits deeper, and a longer one is counted.
	MaxPathBytes = 200
)

// skippedDirs is every directory whose files describe tooling, somebody
// else's software or a sample, never what the company runs.
var skippedDirs = []string{
	"node_modules", "vendor", "third_party", "testdata", "test", "tests", "fixtures", "example", "examples",
}

// manifestDirs is every directory a Kubernetes manifest is read under.
var manifestDirs = []string{"k8s", "kubernetes", "manifests", "deploy"}

// Classify is the format the file at p (from the repository's root, slashes
// between its parts) is read as, and false for a file that is not on the
// list. It decides by the path alone.
func Classify(p string) (Format, bool) {
	if p == "" || len(p) > MaxPathBytes || path.Clean(p) != p || strings.HasPrefix(p, "/") {
		return "", false
	}
	// A PATH IS ONE LINE OF PRINTABLE TEXT: it reaches git on a line of its
	// own, and a control character in one is no name a manifest has.
	if strings.ContainsFunc(p, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return "", false
	}
	dir, base := path.Split(p)
	dirs := strings.FieldsFunc(dir, func(r rune) bool { return r == '/' })
	for _, d := range dirs {
		if strings.HasPrefix(d, ".") || slices.Contains(skippedDirs, strings.ToLower(d)) {
			return "", false
		}
	}
	root := len(dirs) == 0
	lower := strings.ToLower(base)
	if strings.HasPrefix(base, ".") || refusedName(lower) {
		return "", false
	}
	switch {
	case base == "Dockerfile":
		return Dockerfile, root
	case base == "Procfile":
		return Procfile, root
	case composeName(lower):
		return Compose, true
	case base == "Chart.yaml":
		return HelmChart, true
	case base == "fly.toml":
		return Fly, true
	case base == "render.yaml":
		return Render, root
	case base == "package.json":
		return PackageJSON, true
	case base == "go.mod":
		return GoMod, true
	case base == "pyproject.toml":
		return Pyproject, true
	case base == "Gemfile":
		return Gemfile, true
	case base == "Cargo.toml":
		return Cargo, true
	case base == "requirements.txt" || (strings.HasPrefix(base, "requirements-") && strings.HasSuffix(base, ".txt")):
		return Requirements, true
	case strings.HasSuffix(lower, ".tf") || strings.HasSuffix(lower, ".tofu"):
		return Terraform, true
	case strings.HasSuffix(lower, ".yaml") || strings.HasSuffix(lower, ".yml"):
		// A HELM TEMPLATE IS NOT A MANIFEST: it is a template of one, and
		// its directory is where a chart keeps them.
		if slices.Contains(dirs, "templates") {
			return "", false
		}
		for _, d := range dirs {
			if slices.Contains(manifestDirs, strings.ToLower(d)) {
				return Kubernetes, true
			}
		}
	}
	return "", false
}

// refusedName reports a file refused by its (lowered) name whatever directory
// holds it: a values file, a variable or state file, and anything named for
// the secrets it carries.
func refusedName(lower string) bool {
	stem := strings.TrimSuffix(strings.TrimSuffix(lower, ".yaml"), ".yml")
	switch {
	case stem != lower && (stem == "values" || strings.HasPrefix(stem, "values-") || strings.HasPrefix(stem, "values.")):
		return true
	case strings.Contains(lower, "secret"), strings.Contains(lower, "credential"):
		return true
	case strings.HasSuffix(lower, ".tfvars"), strings.Contains(lower, ".tfvars."), strings.Contains(lower, ".tfstate"):
		return true
	}
	return false
}

// composeName reports a compose file's (lowered) name: the two names compose
// reads, and an override of either (docker-compose.prod.yml).
func composeName(lower string) bool {
	stem, ok := strings.CutSuffix(lower, ".yaml")
	if !ok {
		if stem, ok = strings.CutSuffix(lower, ".yml"); !ok {
			return false
		}
	}
	for _, name := range []string{"compose", "docker-compose"} {
		if stem == name || strings.HasPrefix(stem, name+".") {
			return true
		}
	}
	return false
}
