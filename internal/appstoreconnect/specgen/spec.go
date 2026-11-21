package main

import (
	"encoding/json"
	"fmt"
	"io"
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
	Responses   map[string]response `json:"responses"`
	RequestBody *struct {
		Content map[string]struct {
			Schema schema `json:"schema"`
		} `json:"content"`
	} `json:"requestBody"`
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

// resourceType is the JSON:API type of a schema's data.
func (s *spec) resourceType(sc schema) string {
	doc := s.resolve(sc)
	data, ok := doc.Properties["data"]
	if !ok {
		return ""
	}
	data = s.resolve(data)
	if data.Type == "array" && data.Items != nil {
		data = s.resolve(*data.Items)
	}
	t := s.resolve(data.Properties["type"])
	if len(t.Enum) == 0 {
		return ""
	}
	return t.Enum[0]
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
					e.Creates = s.resourceType(c.Schema)
				}
			}
			out = append(out, e)
		}
	}
	return out, nil
}

// lists is every related-list read: from a parent type, the child type a
// `GET /v1/{parent}/{id}/{relationship}` answers, and that path.
func (s *spec) lists() map[string][][2]string {
	out := map[string][][2]string{}
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
		if child := s.resourceType(r.Schema); child != "" {
			out[parts[1]] = append(out[parts[1]], [2]string{child, p})
		}
	}
	for k := range out {
		sort.Slice(out[k], func(i, j int) bool { return out[k][i][1] < out[k][j][1] })
	}
	return out
}

// maxDepth bounds how far below an app a resource is looked for: four related
// lists reach every app-owned type Apple's document has.
const maxDepth = 4

// downward is, for each resource type an app owns, the related-list paths
// from an app down to it, shortest first.
func (s *spec) downward() map[string][]string {
	lists := s.lists()
	chain := map[string][]string{"apps": nil}
	queue := []string{"apps"}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if len(chain[cur]) >= maxDepth {
			continue
		}
		for _, l := range lists[cur] {
			if _, seen := chain[l[0]]; seen {
				continue
			}
			chain[l[0]] = append(append([]string(nil), chain[cur]...), l[1])
			queue = append(queue, l[0])
		}
	}
	return chain
}
