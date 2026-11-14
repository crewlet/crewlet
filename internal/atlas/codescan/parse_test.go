package codescan_test

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlas/codescan"
	"github.com/crewlet/crewlet/internal/deployfiles"
	"github.com/crewlet/crewlet/internal/redact/redacttest"
)

var scanAt = time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)

// said is a file's claims as lines a case can list: the entry, then what is
// said of it.
func said(p codescan.Parsed) []string {
	var out []string
	for _, c := range p.Claims {
		line := c.Subject.String()
		switch c.Facet.Kind {
		case atlas.FacetFact:
			line += " " + string(c.Facet.Fact) + "=" + c.Value.Text
		case atlas.FacetRelation:
			line += " " + string(c.Facet.Relation) + " " + c.Facet.Target.Entry.String()
		}
		out = append(out, line)
	}
	sort.Strings(out)
	return out
}

func parse(t *testing.T, file, body string) codescan.Parsed {
	t.Helper()
	format, ok := deployfiles.Classify(file)
	if !ok {
		t.Fatalf("%s is not on the closed list", file)
	}
	return codescan.Parse(codescan.GitletRepo("shop"), file, format, []byte(body), scanAt)
}

// Each format declares what its doc says it declares, keyed exactly, and
// nothing else.
func TestEachFileDeclaresWhatItNames(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, file, body string
		want             []string
	}{
		{"a root Dockerfile is a service this repository builds", "Dockerfile", "FROM scratch\nENV TOKEN=abc\n", []string{
			"service:shop", "service:shop built_from repository:gitlet/shop",
		}},
		{"a compose file's services, stores and what waits for what", "compose.yaml", `
name: store
services:
  api:
    build: .
    depends_on: [db, events, worker]
  worker:
    image: ghcr.io/acme/worker:1.4.2
    depends_on:
      db: {condition: service_healthy}
  db:
    image: postgres:16.2-alpine
  events:
    image: docker.io/library/nats:latest
`, []string{
			"datastore:store/db", "datastore:store/db engine=postgres", "datastore:store/db engine_version=16.2",
			"queue:store/events", "queue:store/events engine=nats",
			"service:store-api", "service:store-api built_from repository:gitlet/shop",
			"service:store-api depends_on datastore:store/db", "service:store-api depends_on queue:store/events",
			"service:store-api depends_on service:store-worker",
			"service:store-worker", "service:store-worker built_from repository:github.com/acme/worker",
			"service:store-worker depends_on datastore:store/db",
		}},
		{"a compose project is its directory where the file names none", "infra/local/compose.yml",
			"services:\n  web:\n    image: nginx\n", []string{"service:local-web"}},
		{"manifests: workloads, a database a workload runs, and an operator's resources", "k8s/all.yaml", `
apiVersion: apps/v1
kind: Deployment
metadata: {name: checkout-api}
spec:
  template:
    spec:
      containers:
        - name: api
          image: registry.gitlab.com/acme/checkout:2.0
---
apiVersion: apps/v1
kind: StatefulSet
metadata: {name: cache}
spec:
  template:
    spec:
      containers: [{image: "redis:7"}]
---
apiVersion: batch/v1
kind: CronJob
metadata: {name: nightly}
spec:
  jobTemplate:
    spec:
      template:
        spec:
          containers: [{image: acme/nightly}]
---
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata: {name: orders-db}
---
apiVersion: objectbucket.io/v1alpha1
kind: ObjectBucketClaim
metadata: {name: assets}
---
apiVersion: kafka.strimzi.io/v1beta2
kind: KafkaTopic
metadata: {name: orders}
---
apiVersion: v1
kind: Service
metadata: {name: ignored}
`, []string{
			"bucket:shop/assets",
			"datastore:shop/cache", "datastore:shop/cache engine=redis", "datastore:shop/cache engine_version=7",
			"datastore:shop/orders-db", "datastore:shop/orders-db engine=postgres",
			"queue:shop/orders", "queue:shop/orders engine=kafka",
			"service:checkout-api", "service:checkout-api built_from repository:gitlab.com/acme/checkout",
			"service:nightly",
		}},
		{"a chart is a service, and its known dependencies are stores", "charts/api/Chart.yaml", `
name: checkout
version: 1.2.3
dependencies:
  - {name: postgresql, repository: "https://charts.example/"}
  - {name: common}
`, []string{
			"datastore:checkout/postgresql", "datastore:checkout/postgresql engine=postgres",
			"service:checkout", "service:checkout built_from repository:gitlet/shop",
			"service:checkout depends_on datastore:checkout/postgresql",
		}},
		{"terraform names its providers and what they are used for", "infra/main.tf", `
terraform {
  required_providers {
    cloudflare = {
      source  = "cloudflare/cloudflare"
      version = "~> 4"
    }
  }
}
provider "aws" {
  region = "eu-west-1"
}
resource "aws_s3_bucket" "assets" {
  bucket = "acme-assets"
}
resource "aws_db_instance" "orders" {
  password = <<EOT
resource "datadog_monitor" "inside_a_heredoc" {
EOT
}
# resource "scaleway_instance_server" "commented" {
resource "random_id" "suffix" {}
`, []string{
			"vendor_account:aws", "vendor_account:aws capability=database",
			"vendor_account:aws capability=object_storage", "vendor_account:aws vendor=aws",
			"vendor_account:cloudflare", "vendor_account:cloudflare vendor=cloudflare",
		}},
		{"fly names the app and where it runs", "fly.toml", "app = \"shop-web\"\nprimary_region = \"ams\"\n[env]\nTOKEN = \"x\"\n",
			[]string{
				"service:shop-web", "service:shop-web built_from repository:gitlet/shop",
				"service:shop-web runs_on vendor_account:other/fly",
				"vendor_account:other/fly", "vendor_account:other/fly capability=app_delivery",
				"vendor_account:other/fly name=Fly.io", "vendor_account:other/fly vendor=other",
			}},
		{"render names its services and its databases", "render.yaml", `
services:
  - {type: web, name: shop-web, envVars: [{key: TOKEN, value: x}]}
  - {type: keyvalue, name: shop-cache}
databases:
  - {name: shop-db}
`, []string{
			"datastore:render/shop-cache", "datastore:render/shop-cache engine=valkey",
			"datastore:render/shop-db", "datastore:render/shop-db engine=postgres",
			"service:shop-web", "service:shop-web built_from repository:gitlet/shop",
			"service:shop-web runs_on vendor_account:other/render",
			"vendor_account:other/render", "vendor_account:other/render capability=app_delivery",
			"vendor_account:other/render name=Render", "vendor_account:other/render vendor=other",
		}},
		{"package.json: runtime dependencies only", "package.json",
			`{"dependencies":{"@aws-sdk/client-s3":"^3","stripe":"14","left-pad":"1"},"devDependencies":{"@sentry/cli":"2"}}`,
			[]string{
				"vendor_account:aws", "vendor_account:aws vendor=aws",
				"vendor_account:other/stripe", "vendor_account:other/stripe name=Stripe",
				"vendor_account:other/stripe vendor=other",
			}},
		{"go.mod: required module paths", "go.mod", `module example.com/shop

go 1.27

require github.com/openai/openai-go v1.0.0

require (
	github.com/DataDog/datadog-go/v5 v5.5.0 // indirect
	gopkg.in/yaml.v3 v3.0.1
)
`, []string{
			"vendor_account:datadog", "vendor_account:datadog capability=monitoring", "vendor_account:datadog vendor=datadog",
			"vendor_account:openai", "vendor_account:openai capability=model_api", "vendor_account:openai vendor=openai",
		}},
		{"requirements: names, never an option line or an address", "requirements.txt", `
# pinned
boto3==1.34.0
Sentry_SDK[flask]>=2 ; python_version > "3.8"
--extra-index-url https://user:pass@pypi.acme.example/simple
internal @ https://pkgs.acme.example/internal.whl
-r other.txt
`, []string{
			"vendor_account:aws", "vendor_account:aws vendor=aws",
			"vendor_account:other/sentry", "vendor_account:other/sentry capability=monitoring",
			"vendor_account:other/sentry name=Sentry", "vendor_account:other/sentry vendor=other",
		}},
		{"pyproject: the project's requirements and Poetry's names", "pyproject.toml", `
[project]
dependencies = ["anthropic>=0.30", "requests"]
[tool.poetry.dependencies]
python = "^3.12"
twilio = {version = "^9", source = "private"}
`, []string{
			"vendor_account:anthropic", "vendor_account:anthropic capability=model_api",
			"vendor_account:anthropic vendor=anthropic",
			"vendor_account:other/twilio", "vendor_account:other/twilio name=Twilio",
			"vendor_account:other/twilio vendor=other",
		}},
		{"Gemfile: gem names", "Gemfile", "source 'https://rubygems.org'\ngem 'rails', '~> 7'\ngem \"octokit\", git: 'https://x:y@example.com/o.git'\n# gem 'stripe'\n",
			[]string{"vendor_account:github", "vendor_account:github vendor=github"}},
		{"Cargo.toml: crate names", "Cargo.toml", "[dependencies]\naws-sdk-s3 = \"1\"\nserde = { version = \"1\" }\n[dev-dependencies]\nsentry = \"0.3\"\n",
			[]string{"vendor_account:aws", "vendor_account:aws vendor=aws"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := parse(t, tc.file, tc.body)
			if p.Unparsed {
				t.Fatal("the file was not read as its format")
			}
			want := slices.Clone(tc.want)
			sort.Strings(want)
			if got := said(p); !slices.Equal(got, want) {
				t.Errorf("declared:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
			}
			for _, c := range p.Claims {
				if c.Source != atlas.CodeSource("shop") || c.Confidence().Display() != atlas.WordLikely {
					t.Errorf("%s: source %+v, shown %s; a file's claim is the repository's, and likely",
						c.Subject, c.Source, c.Confidence().Display())
				}
			}
		})
	}
}

// A file that is not its format declares nothing, says so, and never fails.
func TestAFileThatIsNotItsFormatDeclaresNothing(t *testing.T) {
	t.Parallel()
	for file, body := range map[string]string{
		"compose.yaml":   "services: [unterminated",
		"k8s/api.yaml":   "{{ template \"x\" . }}\nkind: [",
		"package.json":   "{",
		"pyproject.toml": "[project\n",
		"Cargo.toml":     "= =",
		"fly.toml":       "app = ",
		"render.yaml":    "\tservices: {",
	} {
		if p := parse(t, file, body); !p.Unparsed || len(p.Claims) != 0 {
			t.Errorf("%s: unparsed=%v with %d claims", file, p.Unparsed, len(p.Claims))
		}
	}
}

// A file past the claims one file may make is cut there and says by how much.
func TestAFilePastItsClaimsIsCutAndCounted(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	b.WriteString("services:\n")
	for i := range codescan.MaxClaimsPerFile + 10 {
		fmt.Fprintf(&b, "  s%03d:\n    image: nginx\n", i)
	}
	p := parse(t, "compose.yaml", b.String())
	if len(p.Claims) != codescan.MaxClaimsPerFile || p.Truncated != 10 {
		t.Errorf("%d claims kept, %d cut; want %d and 10", len(p.Claims), p.Truncated, codescan.MaxClaimsPerFile)
	}
}

// quoted is s as a YAML, JSON and TOML string alike.
func quoted(t *testing.T, s string) string {
	t.Helper()
	out, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// planted is one fixture per format with the shape s in EVERY field a parser
// reads, and how many of those fields become a key or a value of a claim (so
// each must be refused and counted).
func planted(t *testing.T, s string) map[string]struct {
	body    string
	refused int
} {
	t.Helper()
	q := quoted(t, s)
	compose, err := yaml.Marshal(map[string]any{
		"name": s,
		"services": map[string]any{
			s:     map[string]any{"image": s, "depends_on": []string{s}},
			"api": map[string]any{"image": "ghcr.io/" + s + "/" + s + ":" + s, "depends_on": map[string]any{s: map[string]any{}}},
			"db":  map[string]any{"image": "postgres:" + s},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return map[string]struct {
		body    string
		refused int
	}{
		// The project names every key, so all three services are refused.
		"compose.yaml": {string(compose), 3},
		"k8s/all.yaml": {fmt.Sprintf(`
apiVersion: apps/v1
kind: Deployment
metadata: {name: %[1]s}
spec: {template: {spec: {containers: [{image: %[1]s}]}}}
---
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata: {name: %[1]s}
---
apiVersion: %[1]s
kind: %[1]s
metadata: {name: %[1]s}
`, q), 2},
		"charts/api/Chart.yaml": {fmt.Sprintf("name: %[1]s\ndependencies: [{name: %[1]s}]\n", q), 1},
		"fly.toml":              {"app = " + q + "\n", 1},
		"render.yaml": {fmt.Sprintf("services: [{type: web, name: %[1]s}, {type: %[1]s, name: ok-web}]\n"+
			"databases: [{name: %[1]s}]\n", q), 2},
		// A name that is looked up in a table and found nowhere says nothing.
		"infra/main.tf":    {fmt.Sprintf("provider %[1]s {}\nresource %[1]s \"x\" {}\nsource = %[1]s\n", q), 0},
		"package.json":     {fmt.Sprintf(`{"dependencies":{%[1]s:%[1]s}}`, q), 0},
		"go.mod":           {"require " + strings.ReplaceAll(s, "\n", " ") + " v1.0.0\n", 0},
		"requirements.txt": {strings.ReplaceAll(s, "\n", " ") + "==1.0\n", 0},
		"pyproject.toml":   {fmt.Sprintf("[project]\ndependencies = [%[1]s]\n[tool.poetry.dependencies]\n%[1]s = %[1]s\n", q), 0},
		"Gemfile":          {"gem " + q + "\n", 0},
		"Cargo.toml":       {fmt.Sprintf("[dependencies]\n%[1]s = %[1]s\n", q), 0},
	}
}

// NO CREDENTIAL SHAPE REACHES THE ATLAS FROM A FILE: every shape the
// redaction pass knows, planted in every field a parser reads, leaves no
// piece of itself in a claim, and each field that would have become a key or
// a value is counted as withheld.
//
// Mutation: make finder.service or finder.store build its Ref with
// atlas.NewRef in place of the builder's recorded one, and a shape a key's
// grammar admits reaches the claims.
func TestNoCredentialShapeReachesTheAtlasFromAFile(t *testing.T) {
	t.Parallel()
	for _, shape := range redacttest.Shapes() {
		s := shape.Sample
		fixtures := planted(t, s)
		for _, format := range deployfiles.Formats {
			if !format.Parsed() {
				continue
			}
			found := false
			for file := range fixtures {
				if f, _ := deployfiles.Classify(file); f == format {
					found = true
				}
			}
			if !found {
				t.Fatalf("no fixture plants a shape in a %s file", format)
			}
		}
		for file, fx := range fixtures {
			p := parse(t, file, fx.body)
			out, err := json.Marshal(p.Claims)
			if err != nil {
				t.Fatal(err)
			}
			for _, piece := range []string{s[len(s)/2:], s[len(s)/4 : len(s)/4+12]} {
				if strings.Contains(string(out), piece) {
					at := strings.Index(string(out), piece)
					t.Errorf("%s in %s: a piece of the shape reached a claim near %q", shape.Name, file,
						string(out)[max(0, at-120):at])
				}
			}
			if !p.Unparsed && p.Withheld < fx.refused {
				t.Errorf("%s in %s: %d values withheld, want at least %d", shape.Name, file, p.Withheld, fx.refused)
			}
		}
	}
}

// AN ENVIRONMENT VALUE IS NEVER READ: every shape planted under every key a
// deploy file keeps values, commands and secrets under changes nothing about
// what the file declares, and is not even counted, because it was never
// decoded.
//
// Mutation: give composeService a field for `environment` and read it into a
// claim (a service's name fact, say), and this goes red.
func TestAnEnvironmentValueIsNeverRead(t *testing.T) {
	t.Parallel()
	clean := map[string]string{
		"compose.yaml": `
services:
  api:
    image: acme/api
    depends_on: [db]
  db:
    image: postgres:16
`,
		"k8s/api.yaml": `
apiVersion: apps/v1
kind: Deployment
metadata: {name: checkout-api}
spec: {template: {spec: {containers: [{image: acme/api}]}}}
`,
		"render.yaml": "services: [{type: web, name: shop-web}]\n",
		"fly.toml":    "app = \"shop-web\"\n",
	}
	for _, shape := range redacttest.Shapes() {
		q := quoted(t, shape.Sample)
		dirty := map[string]string{
			"compose.yaml": fmt.Sprintf(`
x-common: &common
  environment: {TOKEN: %[1]s}
services:
  api:
    image: acme/api
    depends_on: [db]
    environment:
      TOKEN: %[1]s
      DATABASE_URL: %[1]s
    env_file: [%[1]s]
    command: [%[1]s]
    entrypoint: %[1]s
    labels: {token: %[1]s}
    secrets: [%[1]s]
    build: {context: ., args: {TOKEN: %[1]s}}
  db:
    image: postgres:16
    environment: [%[1]s]
secrets:
  token: {environment: %[1]s}
`, q),
			"k8s/api.yaml": fmt.Sprintf(`
apiVersion: apps/v1
kind: Deployment
metadata: {name: checkout-api, annotations: {token: %[1]s}, labels: {token: %[1]s}}
spec:
  template:
    spec:
      containers:
        - image: acme/api
          env: [{name: TOKEN, value: %[1]s}]
          envFrom: [{secretRef: {name: %[1]s}}]
          args: [%[1]s]
          command: [%[1]s]
---
apiVersion: v1
kind: Secret
metadata: {name: %[1]s}
stringData: {token: %[1]s}
data: {token: %[1]s}
---
apiVersion: v1
kind: ConfigMap
metadata: {name: settings}
data: {token: %[1]s}
`, q),
			"render.yaml": fmt.Sprintf("services: [{type: web, name: shop-web, envVars: [{key: TOKEN, value: %[1]s}], "+
				"startCommand: %[1]s}]\nenvVarGroups: [{name: g, envVars: [{key: T, value: %[1]s}]}]\n", q),
			"fly.toml": fmt.Sprintf("app = \"shop-web\"\n[env]\nTOKEN = %[1]s\n[build.args]\nTOKEN = %[1]s\n", q),
		}
		for file, body := range dirty {
			want, got := parse(t, file, clean[file]), parse(t, file, body)
			if file == "compose.yaml" {
				// The dirty file's api builds here, which the clean one does not.
				got.Claims = slices.DeleteFunc(got.Claims, func(c atlas.Claim) bool {
					return c.Facet.Relation == atlas.RelationBuiltFrom
				})
			}
			if !slices.Equal(said(got), said(want)) || got.Withheld != 0 || got.Unparsed {
				t.Errorf("%s in %s: declared %v (withheld %d, unparsed %v); want %v and nothing withheld",
					shape.Name, file, said(got), got.Withheld, got.Unparsed, said(want))
			}
		}
	}
}
