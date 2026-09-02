package config

import (
	"reflect"
	"slices"
	"strings"
	"sync"
)

// The top-level YAML keys each tier owns, DERIVED FROM THE STRUCTS.
//
// A second package used to keep its own hand-written copy of these lists to
// decide which tier an unnamed document is, and it had drifted the way a
// restated list does: it counted `agents`, which appears nowhere in this tree,
// the schema, the examples or the docs — while omitting `roles`, `units`,
// `mcp_servers`, `turn_engine` and `scheduling`. A document with only `units:`
// therefore scored nothing on either tier and was reported undecidable, which
// is the commonest authoring shape and the one a fix loop is most often
// pointed at.
//
// Only keys UNIQUE to one tier are useful for telling them apart, so the two
// sets are subtracted from each other here rather than at each caller.

// CompanyKeys is every top-level key only a Tier B document has.
func CompanyKeys() []string { return companyKeys() }

// BootstrapKeys is every top-level key only a Tier A document has.
func BootstrapKeys() []string { return bootstrapKeys() }

var (
	companyKeys   = sync.OnceValue(func() []string { return uniqueTo[Company, Bootstrap]() })
	bootstrapKeys = sync.OnceValue(func() []string { return uniqueTo[Bootstrap, Company]() })
)

// uniqueTo is the yaml keys of A that B does not also have.
func uniqueTo[A, B any]() []string {
	shared := yamlKeys(reflect.TypeFor[B]())
	var out []string
	for _, key := range yamlKeys(reflect.TypeFor[A]()) {
		if !slices.Contains(shared, key) {
			out = append(out, key)
		}
	}
	slices.Sort(out)
	return out
}

// yamlKeys is a struct's top-level yaml field names.
func yamlKeys(t reflect.Type) []string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil
	}
	var out []string
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		if name == "" || name == "-" {
			continue
		}
		out = append(out, name)
	}
	return out
}
