package codescan

import (
	"bytes"
	"errors"
	"io"
	"strings"

	"github.com/BurntSushi/toml"
	"gopkg.in/yaml.v3"

	"github.com/crewlet/crewlet/internal/atlas"
)

// THE DEPLOY FILES. Each is decoded into the closed types below and nowhere
// else: yaml.v3 and the TOML decoder skip a key no field declares, so what
// these types leave out is never read into anything.

// present records that a key is there and reads nothing under it: a compose
// service's build block holds build arguments, which are values nobody reads.
type present bool

// UnmarshalYAML notes the key and decodes nothing.
func (p *present) UnmarshalYAML(*yaml.Node) error { *p = true; return nil }

// keys is the names a list or a mapping gives, and never a mapping's values:
// compose writes depends_on either way.
type keys []string

// UnmarshalYAML takes a sequence's scalars or a mapping's keys.
func (k *keys) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.SequenceNode:
		for _, item := range n.Content {
			if item.Kind == yaml.ScalarNode {
				*k = append(*k, item.Value)
			}
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			*k = append(*k, n.Content[i].Value)
		}
	}
	return nil
}

// composeFile is the part of a compose file the scan reads.
type composeFile struct {
	Name     string                    `yaml:"name"`
	Services map[string]composeService `yaml:"services"`
}

// composeService is the part of one service the scan reads: the image it
// runs, whether the repository builds it, and what it waits for.
type composeService struct {
	Image     string  `yaml:"image"`
	Build     present `yaml:"build"`
	DependsOn keys    `yaml:"depends_on"`
}

// compose reads a compose file: each service as a service, or as the
// database or queue its image is known to run, keyed under the project the
// way compose names its containers, and what each waits for.
func (f *finder) compose(raw []byte) error {
	var doc composeFile
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return errUnparsed
	}
	project := strings.TrimSpace(doc.Name)
	if project == "" {
		project = f.dirName()
	}
	refs := map[string]atlas.Ref{}
	for _, name := range sortedKeys(doc.Services) {
		svc := doc.Services[name]
		where := f.where("services", name)
		img, imgErr := parseImage(svc.Image)
		if svc.Image != "" && imgErr != nil {
			f.b.Withhold(where+".image", imgErr)
		}
		if e, ok := img.engine(); ok && imgErr == nil && !bool(svc.Build) {
			if r, ok := f.store(where, e, project, name, img.tag); ok {
				refs[name] = r
			}
			continue
		}
		r, ok := f.service(where, project+"-"+name)
		if !ok {
			continue
		}
		refs[name] = r
		if svc.Build {
			if repo, ok := f.repository(where); ok {
				f.b.Relates(where, r, atlas.RelationBuiltFrom, repo)
			}
		} else if imgErr == nil {
			f.builtFromForge(where+".image", r, img)
		}
	}
	for _, name := range sortedKeys(doc.Services) {
		from, ok := refs[name]
		if !ok || from.Kind != atlas.KindService {
			continue
		}
		for _, dep := range doc.Services[name].DependsOn {
			if to, ok := refs[dep]; ok {
				f.b.Relates(f.where("services", name, "depends_on"), from, atlas.RelationDependsOn, to)
			}
		}
	}
	return nil
}

// builtFromForge claims r is built from the forge repository img's registry
// path names, where it is one.
func (f *finder) builtFromForge(where string, r atlas.Ref, img image) {
	key, ok := img.forgeRepository()
	if !ok {
		return
	}
	if repo, ok := f.b.Names(where, atlas.KindRepository, key); ok {
		f.b.Relates(where, r, atlas.RelationBuiltFrom, repo)
	}
}

// manifest is the part of a Kubernetes manifest the scan reads: what it is,
// its name, and the images its pod template runs. It declares no field for a
// container's env, args or command, nor for a Secret's or a ConfigMap's data.
type manifest struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		Template    podTemplate `yaml:"template"`
		JobTemplate struct {
			Spec struct {
				Template podTemplate `yaml:"template"`
			} `yaml:"spec"`
		} `yaml:"jobTemplate"`
	} `yaml:"spec"`
}

type podTemplate struct {
	Spec struct {
		Containers []struct {
			Image string `yaml:"image"`
		} `yaml:"containers"`
	} `yaml:"spec"`
}

// maxManifests is how many documents of one manifest file are read: a file
// at its byte bound holds a few dozen, and the rest are generated.
const maxManifests = 64

// kubernetes reads a manifest file, one document at a time: a workload as a
// service (or the database or queue its image runs), and an operator's
// resource as the database, queue or bucket it asks for.
func (f *finder) kubernetes(raw []byte) error {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	for range maxManifests {
		var m manifest
		err := dec.Decode(&m)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return errUnparsed
		}
		f.manifest(m)
	}
	return nil
}

func (f *finder) manifest(m manifest) {
	name := strings.TrimSpace(m.Metadata.Name)
	if name == "" {
		return
	}
	group, _, grouped := strings.Cut(m.APIVersion, "/")
	where := f.where(m.Kind, name)
	for _, op := range operators {
		if grouped && group == op.group && m.Kind == op.kind {
			f.store(where, op.as, f.repoName, name, "")
			return
		}
	}
	if want, ok := workloads[m.Kind]; !ok || !grouped || group != want {
		return
	}
	containers := m.Spec.Template.Spec.Containers
	if m.Kind == "CronJob" {
		containers = m.Spec.JobTemplate.Spec.Template.Spec.Containers
	}
	var first image
	parsed := false
	if len(containers) > 0 && containers[0].Image != "" {
		img, err := parseImage(containers[0].Image)
		if err != nil {
			f.b.Withhold(where+".image", err)
		} else {
			first, parsed = img, true
		}
	}
	if e, ok := first.engine(); parsed && ok {
		f.store(where, e, f.repoName, name, first.tag)
		return
	}
	if r, ok := f.service(where, name); ok && parsed {
		f.builtFromForge(where+".image", r, first)
	}
}

// chart is the part of a Helm chart's Chart.yaml the scan reads.
type chart struct {
	Name         string `yaml:"name"`
	Dependencies []struct {
		Name string `yaml:"name"`
	} `yaml:"dependencies"`
}

// helmChart reads a chart: the service it deploys, built from this
// repository, and the databases and queues its dependencies are known to be.
func (f *finder) helmChart(raw []byte) error {
	var c chart
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return errUnparsed
	}
	name := strings.TrimSpace(c.Name)
	if name == "" {
		return nil
	}
	svc, ok := f.builtHere(name)
	if !ok {
		return nil
	}
	for _, dep := range c.Dependencies {
		e, known := engines[strings.ToLower(strings.TrimSpace(dep.Name))]
		if !known {
			continue
		}
		where := f.where("dependencies", dep.Name)
		if r, ok := f.store(where, e, name, dep.Name, ""); ok {
			f.b.Relates(where, svc, atlas.RelationDependsOn, r)
		}
	}
	return nil
}

// flyFile is the part of a fly.toml the scan reads: the application's name.
type flyFile struct {
	App string `toml:"app"`
}

// fly reads a fly.toml: the application as a service this repository builds,
// running on the company's Fly.io account.
func (f *finder) fly(raw []byte) error {
	var doc flyFile
	if _, err := toml.Decode(string(raw), &doc); err != nil {
		return errUnparsed
	}
	if app := strings.TrimSpace(doc.App); app != "" {
		f.platformService(app, vendorFly)
	}
	return nil
}

// platformService claims the service keyed name, built from this repository
// and running on the company's account at the platform.
func (f *finder) platformService(name string, platform vendor) {
	svc, ok := f.builtHere(name)
	if !ok {
		return
	}
	where := f.where("service", name)
	if account, ok := f.account(where, platform); ok {
		f.b.Relates(where, svc, atlas.RelationRunsOn, account)
	}
}

// renderFile is the part of a render.yaml the scan reads.
type renderFile struct {
	Services []struct {
		Name string `yaml:"name"`
		Type string `yaml:"type"`
	} `yaml:"services"`
	Databases []struct {
		Name string `yaml:"name"`
	} `yaml:"databases"`
}

// renderStores is the service types of a render.yaml that are a database.
var renderStores = map[string]engine{
	"redis": {atlas.KindDatastore, "redis"}, "keyvalue": {atlas.KindDatastore, "valkey"},
}

// render reads a render.yaml: each service on the company's Render account,
// and each database as the Postgres database Render runs.
func (f *finder) render(raw []byte) error {
	var doc renderFile
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return errUnparsed
	}
	for _, s := range doc.Services {
		name := strings.TrimSpace(s.Name)
		if name == "" {
			continue
		}
		if e, ok := renderStores[s.Type]; ok {
			f.store(f.where("services", name), e, vendorRender.slug, name, "")
			continue
		}
		f.platformService(name, vendorRender)
	}
	for _, d := range doc.Databases {
		if name := strings.TrimSpace(d.Name); name != "" {
			f.store(f.where("databases", name), engine{atlas.KindDatastore, "postgres"}, vendorRender.slug, name, "")
		}
	}
	return nil
}

// Terraform and OpenTofu are read for three block headers and one line, and
// for no attribute's value: `provider "<name>"`, `resource "<type>" ...`,
// `data "<type>" ...`, and a required provider's `source = "<ns>/<name>"`.
// A heredoc's body and a comment are skipped, so text inside one is never
// read as a header.
func (f *finder) terraform(raw []byte) {
	heredoc := ""
	capabilities := map[string][]atlas.Capability{}
	seen := map[string]bool{}
	note := func(provider string) {
		if _, ok := providers[provider]; ok {
			seen[provider] = true
		}
	}
	for line := range strings.Lines(string(raw)) {
		line = strings.TrimSpace(line)
		if heredoc != "" {
			if line == heredoc {
				heredoc = ""
			}
			continue
		}
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
			continue
		}
		if i := strings.Index(line, "<<"); i >= 0 {
			heredoc = strings.Trim(strings.TrimLeft(line[i+2:], "-"), `"' `)
		}
		fields := strings.Fields(line)
		switch {
		case len(fields) >= 2 && fields[0] == "provider":
			note(unquote(fields[1]))
		case len(fields) >= 3 && (fields[0] == "resource" || fields[0] == "data"):
			provider, rest, ok := strings.Cut(unquote(fields[1]), "_")
			if !ok {
				continue
			}
			if provider == "google-beta" {
				provider = "google"
			}
			note(provider)
			for _, row := range resourceCapabilities[provider] {
				if strings.HasPrefix(rest, row.prefix) {
					capabilities[provider] = append(capabilities[provider], row.capability)
					break
				}
			}
		case len(fields) == 3 && fields[0] == "source" && fields[1] == "=":
			// A REQUIRED PROVIDER'S SOURCE is namespace/name, or with a
			// registry host before them. A module's source is a path or an
			// address, which names no provider on the table.
			if parts := strings.Split(unquote(fields[2]), "/"); len(parts) == 2 || len(parts) == 3 {
				note(parts[len(parts)-1])
			}
		}
	}
	for _, provider := range sortedKeys(seen) {
		v := providers[provider]
		v.capabilities = append(append([]atlas.Capability{}, v.capabilities...), capabilities[provider]...)
		f.account(f.where("provider", provider), v)
	}
}

// unquote is a block label without its quotes and anything after them.
func unquote(s string) string {
	s = strings.TrimPrefix(s, `"`)
	if i := strings.IndexByte(s, '"'); i >= 0 {
		s = s[:i]
	}
	return s
}
