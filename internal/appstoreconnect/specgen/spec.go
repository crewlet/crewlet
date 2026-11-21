package main

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"sort"
	"strings"
)

// spec is the part of Apple's OpenAPI document the generator reads.
type spec struct {
	Info struct {
		Version string `json:"version"`
	} `json:"info"`
	Paths      map[string]map[string]json.RawMessage `json:"paths"`
	Components struct {
		Schemas map[string]schema `json:"schemas"`
	} `json:"components"`
}

type operation struct {
	OperationID string              `json:"operationId"`
	Tags        []string            `json:"tags"`
	Parameters  []parameter         `json:"parameters"`
	Responses   map[string]response `json:"responses"`
	RequestBody *struct {
		Content map[string]struct {
			Schema schema `json:"schema"`
		} `json:"content"`
	} `json:"requestBody"`
}

type parameter struct {
	Name   string `json:"name"`
	Schema schema `json:"schema"`
}

type response struct {
	Content map[string]struct {
		Schema schema `json:"schema"`
	} `json:"content"`
}

type schema struct {
	Ref        string            `json:"$ref"`
	Type       string            `json:"type"`
	Enum       []string          `json:"enum"`
	Items      *schema           `json:"items"`
	Properties map[string]schema `json:"properties"`
}

// op is one operation as the generator emits it.
type op struct {
	ID, Method, Path, Tag string
	// Writes is the resource type a POST creates, read from its body.
	Creates string
}

var methods = map[string]bool{"get": true, "post": true, "patch": true, "put": true, "delete": true}

func readSpec(r io.Reader) (*spec, error) {
	var s spec
	if err := json.NewDecoder(r).Decode(&s); err != nil {
		return nil, fmt.Errorf("specgen: read the OpenAPI document: %w", err)
	}
	if len(s.Paths) == 0 {
		return nil, fmt.Errorf("specgen: the document lists no paths")
	}
	return &s, nil
}

// resolve follows sc's local references, each a JSON Pointer to a schema of
// the document's components.
func (s *spec) resolve(sc schema) schema {
	for i := 0; sc.Ref != "" && i < 8; i++ {
		parts := strings.Split(strings.TrimPrefix(sc.Ref, "#"), "/")
		if len(parts) != 4 || parts[0] != "" || parts[1] != "components" || parts[2] != "schemas" {
			return schema{}
		}
		sc = s.Components.Schemas[pointerToken.Replace(parts[3])]
	}
	return sc
}

// pointerToken unescapes a JSON Pointer reference token (RFC 6901).
var pointerToken = strings.NewReplacer("~1", "/", "~0", "~")

// resourceType is the JSON:API type of a schema's data, and whether it is one
// resource rather than a list.
func (s *spec) resourceType(sc schema) (string, bool) {
	doc := s.resolve(sc)
	data, ok := doc.Properties["data"]
	if !ok {
		return "", false
	}
	data = s.resolve(data)
	one := data.Type != "array"
	if !one && data.Items != nil {
		data = s.resolve(*data.Items)
	}
	t := s.resolve(data.Properties["type"])
	if len(t.Enum) == 0 {
		return "", false
	}
	return t.Enum[0], one
}

// operations is every operation of the document, by path then method.
func (s *spec) operations() ([]op, error) {
	var out []op
	paths := make([]string, 0, len(s.Paths))
	for p := range s.Paths {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		ms := make([]string, 0)
		for m := range s.Paths[p] {
			if methods[m] {
				ms = append(ms, m)
			}
		}
		sort.Strings(ms)
		for _, m := range ms {
			var o operation
			if err := json.Unmarshal(s.Paths[p][m], &o); err != nil {
				return nil, fmt.Errorf("specgen: %s %s: %w", m, p, err)
			}
			if o.OperationID == "" || len(o.Tags) == 0 {
				return nil, fmt.Errorf("specgen: %s %s has no operationId or tag", m, p)
			}
			e := op{ID: o.OperationID, Method: strings.ToUpper(m), Path: p, Tag: o.Tags[0]}
			if m == "post" && o.RequestBody != nil {
				if c, ok := o.RequestBody.Content["application/json"]; ok {
					e.Creates, _ = s.resourceType(c.Schema)
				}
			}
			out = append(out, e)
		}
	}
	return out, nil
}

// hop is one related-list read below a parent: its path, the child type it
// answers, and whether it answers one resource rather than a list.
type hop struct {
	Path, Child string
	One         bool
}

// lists is every related-list read, by parent type: `GET /v1/{parent}/{id}/{relationship}`.
func (s *spec) lists() map[string][]hop {
	out := map[string][]hop{}
	for p, ms := range s.Paths {
		raw, ok := ms["get"]
		if !ok {
			continue
		}
		parts := strings.Split(strings.Trim(p, "/"), "/")
		if len(parts) != 4 || parts[2] != "{id}" {
			continue
		}
		var o operation
		if json.Unmarshal(raw, &o) != nil {
			continue
		}
		r, ok := o.Responses["200"].Content["application/json"]
		if !ok {
			continue
		}
		if child, one := s.resourceType(r.Schema); child != "" {
			out[parts[1]] = append(out[parts[1]], hop{Path: p, Child: child, One: one})
		}
	}
	for k := range out {
		sort.Slice(out[k], func(i, j int) bool { return out[k][i].Path < out[k][j].Path })
	}
	return out
}

// relation is one relationship a resource type declares.
type relation struct {
	To  string
	One bool
}

// relations is, per resource type, the relationships its schema declares.
func (s *spec) relations() map[string]map[string]relation {
	out := map[string]map[string]relation{}
	for _, sc := range s.Components.Schemas {
		t := s.resolve(sc.Properties["type"])
		if _, hasID := sc.Properties["id"]; len(t.Enum) != 1 || !hasID {
			continue
		}
		rels := map[string]relation{}
		for name, r := range s.resolve(sc.Properties["relationships"]).Properties {
			data, ok := s.resolve(r).Properties["data"]
			if !ok {
				continue
			}
			data = s.resolve(data)
			one := data.Type != "array"
			if !one && data.Items != nil {
				data = s.resolve(*data.Items)
			}
			if to := s.resolve(data.Properties["type"]); len(to.Enum) == 1 {
				rels[name] = relation{To: to.Enum[0], One: one}
			}
		}
		if _, seen := out[t.Enum[0]]; !seen || len(rels) > len(out[t.Enum[0]]) {
			out[t.Enum[0]] = rels
		}
	}
	return out
}

// listable reports whether the team lists t at the top level (`GET /v1/t`):
// Apple's reference data and the team's own resources, which no app owns
// unless a to-one relationship says so.
func (s *spec) listable(t string) bool {
	for p, ms := range s.Paths {
		parts := strings.Split(strings.Trim(p, "/"), "/")
		if _, ok := ms["get"]; ok && len(parts) == 2 && parts[1] == t {
			return true
		}
	}
	return false
}

// owned is every resource type that belongs to exactly one app
// (appstoreconnect's package doc, "Scope"): never one reached many-to-many or
// listed team-wide.
func (s *spec) owned() map[string]bool {
	rels, lists := s.relations(), s.lists()
	owned := map[string]bool{}
	in := func(t string) bool { return t == "apps" || owned[t] }
	manyBack := func(t, parent string) bool {
		return slices.ContainsFunc(slices.Collect(maps.Values(rels[t])), func(b relation) bool {
			return b.To == parent && !b.One
		})
	}
	for changed := true; changed; {
		changed = false
		for t, rs := range rels {
			if t == "apps" || owned[t] {
				continue
			}
			for _, r := range rs {
				if r.One && in(r.To) {
					owned[t], changed = true, true
					break
				}
			}
		}
		for parent, hops := range lists {
			for _, h := range hops {
				if in(parent) && !in(h.Child) && !s.listable(h.Child) && !manyBack(h.Child, parent) {
					owned[h.Child], changed = true, true
				}
			}
		}
	}
	return owned
}

// up is how an owned resource is placed upward: its instance read, including
// the to-one relationships that lead to its app, nearest first.
type up struct {
	Path string
	Rels []string
}

// upward is, per owned type whose instance read can include a to-one
// relationship to its app or an owned container, that read: never one to a
// child, which names the type back to-one from farther down.
func (s *spec) upward(owned map[string]bool) map[string]up {
	rels := s.relations()
	level := s.levels(owned)
	out := map[string]up{}
	for t := range owned {
		path, includes := s.instance(t)
		var cands []string
		for name, r := range rels[t] {
			l, ok := level[r.To]
			back := slices.ContainsFunc(slices.Collect(maps.Values(rels[r.To])), func(b relation) bool {
				return b.To == t && b.One
			})
			if ok && r.One && r.To != t && (l < level[t] || !back) && slices.Contains(includes, name) {
				cands = append(cands, name)
			}
		}
		if len(cands) == 0 {
			continue
		}
		sort.Slice(cands, func(i, j int) bool {
			a, b := level[rels[t][cands[i]].To], level[rels[t][cands[j]].To]
			return a < b || (a == b && cands[i] < cands[j])
		})
		out[t] = up{Path: path, Rels: cands}
	}
	return out
}

// levels is each owned type's distance from its app: the fewest to-one
// relationships up to it, or related reads down from it.
func (s *spec) levels(owned map[string]bool) map[string]int {
	rels, lists := s.relations(), s.lists()
	level := map[string]int{"apps": 0}
	for changed := true; changed; {
		changed = false
		near := func(t string, d int) {
			if cur, has := level[t]; owned[t] && (!has || d < cur) {
				level[t], changed = d, true
			}
		}
		for t := range owned {
			for _, r := range rels[t] {
				if d, ok := level[r.To]; ok && r.One {
					near(t, d+1)
				}
			}
		}
		for parent, hops := range lists {
			for _, h := range hops {
				if d, ok := level[parent]; ok {
					near(h.Child, d+1)
				}
			}
		}
	}
	return level
}

// instance is t's instance read and the relationships it may include.
func (s *spec) instance(t string) (string, []string) {
	for _, v := range []string{"v1", "v2", "v3"} {
		p := "/" + v + "/" + t + "/{id}"
		raw, ok := s.Paths[p]["get"]
		if !ok {
			continue
		}
		var o operation
		if json.Unmarshal(raw, &o) != nil {
			return "", nil
		}
		for _, prm := range o.Parameters {
			if prm.Name == "include" && prm.Schema.Items != nil {
				return p, s.resolve(*prm.Schema.Items).Enum
			}
		}
		return p, nil
	}
	return "", nil
}

// maxDepth bounds how far below an app a resource is looked for: four related
// lists reach every owned type Apple's document has.
const maxDepth = 4

// downward is, for each owned type with no upward read, the related lists
// from an app down to it through owned types alone, shortest first.
func (s *spec) downward(owned map[string]bool, ups map[string]up) map[string][]hop {
	lists := s.lists()
	chain := map[string][]hop{"apps": nil}
	queue := []string{"apps"}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if len(chain[cur]) >= maxDepth {
			continue
		}
		for _, l := range lists[cur] {
			if _, seen := chain[l.Child]; seen || !owned[l.Child] {
				continue
			}
			chain[l.Child] = append(append([]hop(nil), chain[cur]...), l)
			queue = append(queue, l.Child)
		}
	}
	for t := range chain {
		if _, placed := ups[t]; placed || t == "apps" {
			delete(chain, t)
		}
	}
	return chain
}

// idFilters is, per list read, the filters that name resources by ID: a
// filter named after a relationship of the listed type, and its type.
func (s *spec) idFilters() map[string]map[string]string {
	rels := s.relations()
	out := map[string]map[string]string{}
	for p, ms := range s.Paths {
		raw, ok := ms["get"]
		if !ok {
			continue
		}
		var o operation
		if json.Unmarshal(raw, &o) != nil {
			continue
		}
		r, ok := o.Responses["200"].Content["application/json"]
		if !ok {
			continue
		}
		listed, _ := s.resourceType(r.Schema)
		for _, prm := range o.Parameters {
			name, isFilter := strings.CutPrefix(prm.Name, "filter[")
			name = strings.TrimSuffix(name, "]")
			if !isFilter {
				continue
			}
			to := ""
			if name == "id" {
				to = listed
			} else if rel, ok := rels[listed][name]; ok {
				to = rel.To
			}
			if to == "" {
				continue
			}
			if out[p] == nil {
				out[p] = map[string]string{}
			}
			out[p][name] = to
		}
	}
	return out
}
