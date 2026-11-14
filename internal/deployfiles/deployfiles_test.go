package deployfiles_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/deployfiles"
)

// The list admits exactly the files its doc names, each as its format.
func TestTheListAdmitsOnlyWhatItNames(t *testing.T) {
	admitted := map[string]deployfiles.Format{
		"Dockerfile":                       deployfiles.Dockerfile,
		"Procfile":                         deployfiles.Procfile,
		"compose.yaml":                     deployfiles.Compose,
		"docker-compose.yml":               deployfiles.Compose,
		"infra/docker-compose.prod.yml":    deployfiles.Compose,
		"k8s/api.yaml":                     deployfiles.Kubernetes,
		"services/api/deploy/workload.yml": deployfiles.Kubernetes,
		"charts/api/Chart.yaml":            deployfiles.HelmChart,
		"infra/main.tf":                    deployfiles.Terraform,
		"infra/dns.tofu":                   deployfiles.Terraform,
		"fly.toml":                         deployfiles.Fly,
		"render.yaml":                      deployfiles.Render,
		"package.json":                     deployfiles.PackageJSON,
		"services/api/go.mod":              deployfiles.GoMod,
		"requirements.txt":                 deployfiles.Requirements,
		"requirements-prod.txt":            deployfiles.Requirements,
		"pyproject.toml":                   deployfiles.Pyproject,
		"Gemfile":                          deployfiles.Gemfile,
		"Cargo.toml":                       deployfiles.Cargo,
	}
	seen := map[deployfiles.Format]bool{}
	for p, want := range admitted {
		got, ok := deployfiles.Classify(p)
		if !ok || got != want {
			t.Errorf("Classify(%q) = %q, %v; want %q", p, got, ok, want)
		}
		seen[got] = true
	}
	for _, f := range deployfiles.Formats {
		if !seen[f] {
			t.Errorf("no case admits a %s file", f)
		}
	}
}

// A file that can hold a secret by its nature, or that describes somebody
// else's software, is refused by its path whatever else matches.
func TestAFileThatHoldsSecretsIsNeverListed(t *testing.T) {
	for _, p := range []string{
		".env", ".env.production", "services/api/.env", "k8s/.env",
		"k8s/secrets.yaml", "k8s/api-secret.yml", "deploy/credentials.yaml",
		"charts/api/values.yaml", "charts/api/values-prod.yaml", "deploy/values.production.yml",
		"charts/api/templates/deployment.yaml",
		"infra/prod.tfvars", "infra/prod.auto.tfvars.json", "infra/terraform.tfstate", "infra/terraform.tfstate.backup",
		".github/workflows/deploy.yaml", ".devcontainer/docker-compose.yml",
		"node_modules/left-pad/package.json", "vendor/github.com/acme/lib/go.mod", "examples/compose.yaml",
		"testdata/k8s/api.yaml", "docker/Dockerfile", "services/api/Procfile", "services/api/render.yaml",
		"src/config.yaml", "README.md", "k8s/api\n.yaml", "k8s/a\x00.yaml", "main.go", "", "/etc/compose.yaml", "a/../compose.yaml",
		"k8s/" + strings.Repeat("a", deployfiles.MaxPathBytes) + ".yaml",
	} {
		if f, ok := deployfiles.Classify(p); ok {
			t.Errorf("Classify(%q) admits it as %s", p, f)
		}
	}
}

// A file listed by its presence is never one the scan reads.
func TestAPresenceFileIsNeverParsed(t *testing.T) {
	for _, f := range deployfiles.Formats {
		presence := f == deployfiles.Dockerfile || f == deployfiles.Procfile
		if f.Parsed() == presence {
			t.Errorf("%s: Parsed() = %v", f, f.Parsed())
		}
	}
	if deployfiles.Format("env").Parsed() {
		t.Error("a format outside the set is parsed")
	}
}
