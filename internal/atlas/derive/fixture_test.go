package derive

import (
	"reflect"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/secrets"
)

// Canary starts every value PlantSecrets writes, so a test finds one anywhere
// in an answer by this prefix alone.
const Canary = "atlas-canary-"

// awsConnection is a stack output for the slug acme in us-east-2.
const awsConnection = "arn:aws:rolesanywhere:us-east-2:123456789012:trust-anchor/0f0e0d0c-0b0a-4908-8706-050403020100," +
	"arn:aws:rolesanywhere:us-east-2:123456789012:profile/00010203-0405-4607-8809-0a0b0c0d0e0f," +
	"arn:aws:iam::123456789012:role/crewlet/acme/crewlet-acme-provisioner," +
	"arn:aws:rolesanywhere:us-east-2:123456789012:profile/10010203-0405-4607-8809-0a0b0c0d0e0f," +
	"arn:aws:iam::123456789012:role/crewlet/acme/crewlet-acme-broker"

// AcmeCompany is a company that connects every integration the engine knows,
// declares its servers, tool servers, models and sandbox, and hosts its code
// on Gitlet: what the settings alone tell the atlas.
func AcmeCompany() *config.Company {
	c := &config.Company{Name: "acme"}
	in := &c.Integrations
	in.Slack = &config.Slack{}
	in.Mattermost = &config.Mattermost{Enabled: true, URL: "https://chat.acme.example", Team: "acme"}
	in.Jira = &config.Jira{CloudID: "11111111-2222-4333-8444-555555555555", SiteURL: "https://acme.atlassian.net"}
	in.Confluence = &config.Confluence{CloudID: "11111111-2222-4333-8444-555555555555"}
	in.GitHub = &config.GitHub{Enabled: true,
		Provisioning: &config.GitHubProvisioning{Org: "acme", Repos: []string{"acme/api", "web"}}}
	in.GitLab = &config.GitLab{Enabled: true, URL: "https://gitlab.acme.example",
		Provisioning: &config.GitLabProvisioning{Group: "platform", Projects: []string{"platform/infra"}}}
	in.Datadog = &config.Datadog{Enabled: true, Provisioning: &config.DatadogProvisioning{Site: "us5.datadoghq.com"}}
	in.Cloudflare = &config.Cloudflare{Enabled: true, Provisioning: &config.CloudflareProvisioning{
		AccountID: "0123456789abcdef0123456789abcdef", Zones: []string{"acme.example", "Acme-Status.example."}}}
	in.Expo = &config.Expo{Enabled: true, Provisioning: &config.ExpoProvisioning{AccountName: "acme"}}
	in.DigitalOcean = &config.DigitalOcean{Provisioning: &config.DigitalOceanProvisioning{Team: "acme"}}
	in.Scaleway = &config.Scaleway{Enabled: true,
		Provisioning: &config.ScalewayProvisioning{OrganizationID: "A1B2C3D4-0000-4000-8000-000000000001"}}
	in.GoogleCloud = &config.GoogleCloud{Enabled: true,
		Provisioning: &config.GoogleCloudProvisioning{Provisioner: "crewlet-provisioner@acme-prod.iam.gserviceaccount.com"}}
	in.Atlassian = &config.Atlassian{OrgID: "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d"}
	in.AWS = &config.AWS{Enabled: true, Provisioning: &config.AWSProvisioning{Slug: "acme", HomeRegion: "us-east-2",
		Regions: []string{"us-east-2", "eu-west-1"}, Connection: awsConnection}}
	in.SSH = &config.SSH{Enabled: true, Hosts: []config.SSHHost{
		{ID: "web-1", Address: "9.9.9.9", Containers: true},
		{ID: "db-1", Address: "10.0.0.5", Port: 2222},
	}}
	c.Code.Backend = config.CodeGitlet
	c.Code.Repos = []config.CodeRepo{
		{Name: "api", MirrorFrom: &config.CodeMirrorSource{URL: "https://github.com/acme/api.git"}},
		{Name: "payroll", Visibility: config.CodeVisibility("unit"),
			MirrorTo: &config.CodeMirrorTarget{URL: "https://github.com/acme/payroll", As: "release"}},
	}
	c.Providers.LLM = map[string]config.LLMProvider{
		"main":   {Harness: config.HarnessClaudeCode, Model: "claude-opus-5-5"},
		"backup": {Harness: config.HarnessCodex, Model: "gpt-5"},
	}
	c.Providers.LLMOrder = []string{"main", "backup"}
	c.Providers.Embeddings = &config.EmbeddingProvider{Type: config.EmbeddingOpenAI, Model: "text-embedding-3-small"}
	c.Providers.Sandbox = &config.SandboxProvider{E2B: &config.E2BSandbox{}}
	c.MCPServers = []config.MCPServer{
		{Name: "tickets", Transport: config.TransportHTTP, URL: "https://mcp.tickets.example/mcp"},
		{Name: "files", Transport: config.TransportStdio, Command: "/usr/local/bin/files-mcp",
			Args: []string{"--root", "/srv/files"}},
	}
	return c
}

// AcmeDeployment is where acme's deployment answers: inside a zone the
// settings declare, and a vendors' address beside it.
var AcmeDeployment = Deployment{APIAddress: "https://crewlet.acme.example", PublicAddress: "https://hooks.acme.example"}

// PlantSecrets writes a canary into every secret position of c: every
// pointer on the way is allocated, every list given a member and every map an
// entry, so each position tagged secret:"true" (or "content") holds a value
// that names its path. It answers how many it planted.
func PlantSecrets(c *config.Company) int {
	return plant(reflect.ValueOf(c).Elem(), false, "", 0)
}

// plantDepth bounds the walk: a unit holds units, and four levels reach every
// secret position a seat or a unit carries.
const plantDepth = 8

func plant(v reflect.Value, secret bool, path string, depth int) int {
	if depth > plantDepth {
		return 0
	}
	switch v.Kind() {
	case reflect.String:
		if secret && v.CanSet() {
			v.SetString(Canary + path)
			return 1
		}
	case reflect.Pointer:
		if v.IsNil() {
			if !v.CanSet() {
				return 0
			}
			v.Set(reflect.New(v.Type().Elem()))
		}
		return plant(v.Elem(), secret, path, depth+1)
	case reflect.Struct:
		n := 0
		for i := range v.NumField() {
			f := v.Type().Field(i)
			if !f.IsExported() {
				continue
			}
			name := strings.Split(f.Tag.Get("yaml"), ",")[0]
			n += plant(v.Field(i), secret || secrets.Field(f), path+"."+name, depth+1)
		}
		return n
	case reflect.Slice:
		if v.Len() == 0 && v.CanSet() {
			v.Set(reflect.Append(v, reflect.Zero(v.Type().Elem())))
		}
		n := 0
		for i := range v.Len() {
			n += plant(v.Index(i), secret, path+"["+strconv.Itoa(i)+"]", depth+1)
		}
		return n
	case reflect.Map:
		if v.Type().Key().Kind() != reflect.String || !v.CanSet() {
			return 0
		}
		if v.IsNil() {
			v.Set(reflect.MakeMap(v.Type()))
		}
		keys := v.MapKeys()
		if len(keys) == 0 {
			keys = []reflect.Value{reflect.ValueOf("planted").Convert(v.Type().Key())}
		}
		n := 0
		for _, k := range keys {
			elem := reflect.New(v.Type().Elem()).Elem()
			if existing := v.MapIndex(k); existing.IsValid() {
				elem.Set(existing)
			}
			n += plant(elem, secret, path+"."+k.String(), depth+1)
			v.SetMapIndex(k, elem)
		}
		return n
	}
	return 0
}
