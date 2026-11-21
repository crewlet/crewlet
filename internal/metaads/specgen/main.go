// Command specgen holds the Meta Ads operation table to Meta's API specs (the
// api_specs/specs folder of facebook/facebook-business-sdk-codegen at the
// version the table names):
//
//	go run ./internal/metaads/specgen -specs api_specs/specs check
//
// It reports a table row whose spec, method, path, rule field or answer type
// the specs no longer have; a spec operation of a node the table reaches that
// is neither in the table nor left out by the coverage matrix, which needs a
// class; and a node type reachable from an ad account or a business that the
// table and the matrix leave unclassed.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/metaads"
)

func main() {
	dir := flag.String("specs", "", "Meta's api_specs/specs folder")
	flag.Parse()
	if *dir == "" || flag.NArg() != 1 || flag.Arg(0) != "check" {
		fmt.Fprintln(os.Stderr, "usage: specgen -specs api_specs/specs check")
		os.Exit(2)
	}
	specs, version, err := load(*dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	problems := check(specs, version, metaads.SpecRefs(), metaads.AccountOwned(), outOfScope, merged)
	fmt.Printf("specs %s: %d node types; table %d operations; %d disagreements\n", version, len(specs),
		len(metaads.SpecRefs()), len(problems))
	for _, p := range problems {
		fmt.Println(p)
	}
	if len(problems) > 0 {
		os.Exit(1)
	}
}

// spec is one node type's spec file.
type spec struct {
	APIs   []api   `json:"apis"`
	Fields []field `json:"fields"`
}

type api struct {
	Name     string  `json:"name"`
	Method   string  `json:"method"`
	Endpoint string  `json:"endpoint"`
	BasePath string  `json:"basePath"`
	Return   string  `json:"return"`
	Params   []field `json:"params"`
}

type field struct {
	Name string `json:"name"`
}

// key is how the table and the matrix name an operation in its file: its
// method and its edge, or its name where it has no edge.
func (a api) key() string {
	if a.Endpoint != "" {
		return a.Method + " " + a.Endpoint
	}
	return a.Method + " " + a.Name
}

// load reads every spec file and the specs' version.
func load(dir string) (map[string]spec, string, error) {
	version, err := os.ReadFile(filepath.Join(dir, "version.txt"))
	if err != nil {
		return nil, "", fmt.Errorf("specgen: read the specs' version: %w", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, "", fmt.Errorf("specgen: read %s: %w", dir, err)
	}
	specs := map[string]spec{}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".json") || name == "enum_types.json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, "", err
		}
		var s spec
		if err := json.Unmarshal(data, &s); err != nil {
			return nil, "", fmt.Errorf("specgen: %s: %w", name, err)
		}
		specs[strings.TrimSuffix(name, ".json")] = s
	}
	return specs, strings.TrimSpace(string(version)), nil
}

// check is every disagreement between the table and the specs.
func check(specs map[string]spec, version string, refs []metaads.SpecRef, owned []string, left map[[2]string]string,
	merged map[[2]string]bool) []string {
	var out []string
	bad := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }
	if version != metaads.Version {
		bad("version: the specs are %s and the table %s", version, metaads.Version)
	}
	accounted := map[[2]string]bool{}
	files := map[string]bool{}
	for _, r := range refs {
		file := strings.TrimSuffix(r.Spec, ".json")
		files[file] = true
		accounted[[2]string{r.Spec, r.API}] = true
		s, ok := specs[file]
		if !ok {
			bad("gone: %s (%s): its spec %s is no longer in the specs", r.ID, r.Path, r.Spec)
			continue
		}
		i := slices.IndexFunc(s.APIs, func(a api) bool { return a.key() == r.API })
		if i < 0 {
			bad("gone: %s (%s %s): %s has no %s", r.ID, r.Method, r.Path, r.Spec, r.API)
			continue
		}
		a := s.APIs[i]
		if a.Method != r.Method {
			bad("changed: %s is %s in the table and %s in %s", r.ID, r.Method, a.Method, r.Spec)
		}
		if p := pathOf(r.Resource, a, strings.HasPrefix(r.Path, "/{id}/")); p != r.Path {
			bad("changed: %s's path is %s in the table and %s in %s", r.ID, r.Path, p, r.Spec)
		}
		for _, f := range r.Fields {
			top := strings.FieldsFunc(f, func(c rune) bool { return c == '.' || c == '[' })[0]
			if !slices.ContainsFunc(a.Params, func(p field) bool { return p.Name == top }) {
				bad("changed: %s's rule names %s, which %s %s does not take", r.ID, f, r.Spec, r.API)
			}
		}
		if ret := baseType(a.Return, specs); ret != r.Returns {
			bad("changed: %s answers %q in the table and %q in %s", r.ID, r.Returns, ret, r.Spec)
		}
	}
	for k := range left {
		files[strings.TrimSuffix(k[0], ".json")] = true
	}
	for k := range merged {
		files[strings.TrimSuffix(k[0], ".json")] = true
	}
	for file := range files {
		for _, a := range specs[file].APIs {
			k := [2]string{file + ".json", a.key()}
			if !accounted[k] && left[k] == "" && !merged[k] {
				bad("new: %s %s needs a class, or the matrix leaves it out", file+".json", a.key())
			}
		}
	}
	for k := range left {
		if !slices.ContainsFunc(specs[strings.TrimSuffix(k[0], ".json")].APIs, func(a api) bool { return a.key() == k[1] }) {
			bad("gone: %s %s, which the matrix leaves out, is no longer in the specs", k[0], k[1])
		}
	}
	for k := range merged {
		if !slices.ContainsFunc(specs[strings.TrimSuffix(k[0], ".json")].APIs, func(a api) bool { return a.key() == k[1] }) {
			bad("gone: %s %s, merged into another row, is no longer in the specs", k[0], k[1])
		}
	}
	for _, n := range reachable(specs) {
		if len(specs[n].APIs) > 0 && !files[n] {
			bad("new: node type %s is reached from an ad account or a business and nothing classes it", n)
		}
	}
	var want []string
	for _, r := range refs {
		s, ok := specs[r.Resource]
		if ok && slices.ContainsFunc(s.Fields, func(f field) bool { return f.Name == "account_id" }) && !slices.Contains(want, r.Resource) {
			want = append(want, r.Resource)
		}
	}
	slices.Sort(want)
	if !slices.Equal(want, owned) {
		bad("changed: the node types placed by account_id are %v in the table and %v in the specs", owned, want)
	}
	slices.Sort(out)
	return out
}

// baseType is the node type a return names, "" for one that is no spec.
func baseType(ret string, specs map[string]spec) string {
	if strings.HasPrefix(ret, "list<") && strings.HasSuffix(ret, ">") {
		ret = ret[len("list<") : len(ret)-1]
	}
	if _, ok := specs[ret]; ok {
		return ret
	}
	return ""
}

// reachable are the node types an ad account or a business reaches through
// the types its operations return, sorted.
func reachable(specs map[string]spec) []string {
	seen := map[string]bool{"AdAccount": true, "Business": true}
	queue := []string{"AdAccount", "Business"}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		for _, a := range specs[n].APIs {
			if r := baseType(a.Return, specs); r != "" && !seen[r] {
				seen[r] = true
				queue = append(queue, r)
			}
		}
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// pathOf is an operation's path as the table writes it: its base path for a
// root operation, else its node's placeholder (act_ and the ad account's ID
// for an ad account, {id} for a spec naming no node) and its edge.
func pathOf(node string, a api, parentless bool) string {
	if a.BasePath != "" {
		if a.Endpoint != "" {
			return "/" + a.BasePath + "/" + a.Endpoint
		}
		return "/" + a.BasePath
	}
	head := "/{" + snake(node) + "_id}"
	switch {
	case node == "AdAccount":
		head = "/act_{ad_account_id}"
	case parentless:
		head = "/{id}"
	}
	if strings.HasPrefix(a.Name, "#") || (strings.HasPrefix(a.Name, "gen") && a.Endpoint == "") {
		return head
	}
	return head + "/" + a.Endpoint
}

// snake is a node type in snake case: a word starts at a capital followed by
// a small letter, and at a capital after a small letter or a digit.
func snake(n string) string {
	var b strings.Builder
	for i := 0; i < len(n); i++ {
		c := n[i]
		upper := c >= 'A' && c <= 'Z'
		if i > 0 && upper {
			prev := n[i-1]
			nextLower := i+1 < len(n) && n[i+1] >= 'a' && n[i+1] <= 'z'
			prevLowerOrDigit := (prev >= 'a' && prev <= 'z') || (prev >= '0' && prev <= '9')
			if nextLower || prevLowerOrDigit {
				b.WriteByte('_')
			}
		}
		if upper {
			c += 'a' - 'A'
		}
		b.WriteByte(c)
	}
	return b.String()
}
