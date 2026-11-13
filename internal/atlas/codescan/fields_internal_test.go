package codescan

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

// neverDecoded is every key a deploy or dependency file keeps a value, a
// command or a secret under. No parser may declare a field for one.
var neverDecoded = []string{
	"env", "environment", "env_file", "envfrom", "envvars", "envvargroups", "command", "args", "entrypoint",
	"startcommand", "buildcommand", "data", "stringdata", "secrets", "secret", "configs", "labels",
	"annotations", "volumes", "devdependencies", "scripts", "version", "values",
}

// openFields is every field under t (through structs, slices, maps and
// pointers) that could carry a value nobody chose: one tagged with a key
// that is never decoded, or one of a type that takes whatever the file holds.
func openFields(t reflect.Type, path string, seen map[reflect.Type]bool) []string {
	switch t.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array:
		return openFields(t.Elem(), path, seen)
	case reflect.Map:
		if t.Elem().Kind() == reflect.String {
			return []string{path + ": a map of strings takes any value"}
		}
		return openFields(t.Elem(), path, seen)
	case reflect.Interface:
		return []string{path + ": an interface takes any value"}
	case reflect.Struct:
	default:
		return nil
	}
	if seen[t] {
		return nil
	}
	seen[t] = true
	if t.Name() != "" && t.PkgPath() != reflect.TypeFor[finder]().PkgPath() {
		return []string{path + ": " + t.String() + " is a type of another package, which takes any value"}
	}
	var out []string
	for i := range t.NumField() {
		f := t.Field(i)
		at := path + "." + f.Name
		for _, tag := range []string{"yaml", "json", "toml"} {
			key, _, _ := strings.Cut(f.Tag.Get(tag), ",")
			if slices.Contains(neverDecoded, strings.ToLower(key)) {
				out = append(out, at+": decodes "+key)
			}
		}
		out = append(out, openFields(f.Type, at, seen)...)
	}
	return out
}

// NO PARSER DECLARES A FIELD FOR AN ENVIRONMENT VALUE, a command, a secret's
// data or a dependency's version, and none declares a field that takes
// whatever a file holds: what a parser's types leave out is never decoded.
func TestNoParserDeclaresAFieldForAnEnvironmentValue(t *testing.T) {
	t.Parallel()
	for _, doc := range []any{composeFile{}, manifest{}, chart{}, flyFile{}, renderFile{}, packageFile{},
		pyprojectFile{}, cargoFile{}} {
		ty := reflect.TypeOf(doc)
		if open := openFields(ty, ty.Name(), map[reflect.Type]bool{}); len(open) > 0 {
			t.Errorf("%s reads what it must never read:\n  %s", ty.Name(), strings.Join(open, "\n  "))
		}
	}
}

// The gate above can fail: a type that breaks the rule on purpose is caught,
// each way a field can break it.
func TestTheFieldGateCatchesABrokenParser(t *testing.T) {
	t.Parallel()
	type brokenService struct {
		Image       string            `yaml:"image"`
		Environment map[string]string `yaml:"environment"`
		Extra       map[string]any    `yaml:"x-extra"`
		Env         []struct {
			Value string `yaml:"value"`
		} `yaml:"env"`
	}
	type broken struct {
		Services map[string]brokenService `yaml:"services"`
	}
	open := strings.Join(openFields(reflect.TypeFor[broken](), "broken", map[reflect.Type]bool{}), "\n")
	for _, want := range []string{"Environment: decodes environment", "Environment: a map of strings",
		"Extra: an interface takes any value", "Env: decodes env"} {
		if !strings.Contains(open, want) {
			t.Errorf("the gate missed %q in:\n%s", want, open)
		}
	}
}
