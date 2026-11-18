package config

import (
	"reflect"
	"slices"
	"strings"
	"sync"
)

// The top-level YAML keys each tier owns, DERIVED FROM THE STRUCTS: a
// hand-written copy elsewhere drifted (it listed a nonexistent `agents` and
// omitted `roles`, `units` and others), so a `units:`-only document was
// reported undecidable.
//
// Only keys UNIQUE to one tier tell the tiers apart, so the sets are
// subtracted here rather than at each caller.

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
