package config

import (
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// EVERY `js` DIRECTIVE MEANS WHAT IT SAYS, or the build fails.
//
// The generator reads a directive it does not understand as no directive at
// all: `min=0,max=64` — a comma where the grammar takes a semicolon — is one
// key, `min`, with the value `0,max=64`, which does not parse as a number, so
// the published schema carried NEITHER bound and nothing said so. A schema is
// a public artifact an editor validates against; a bound silently missing
// from it is a rule an author is told does not exist.
func TestEveryDirectiveMeansWhatItSays(t *testing.T) {
	t.Parallel()

	// THE CHECK, ON INPUT WHOSE VERDICT IS KNOWN — a guard that finds no
	// fault passes identically when there is none and when it has gone
	// inert.
	if problems := directiveProblems("min=0,max=64"); len(problems) == 0 {
		t.Fatal("control: a comma-separated directive was accepted")
	}
	if problems := directiveProblems("min=1;max=10;required"); len(problems) != 0 {
		t.Fatalf("control: a well-formed directive was refused: %v", problems)
	}

	seen := map[reflect.Type]bool{}
	var walk func(reflect.Type, string)
	walk = func(typ reflect.Type, path string) {
		for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice ||
			typ.Kind() == reflect.Array || typ.Kind() == reflect.Map {
			typ = typ.Elem()
		}
		if typ.Kind() != reflect.Struct || seen[typ] {
			return
		}
		seen[typ] = true
		for i := range typ.NumField() {
			f := typ.Field(i)
			if tag := f.Tag.Get("js"); tag != "" {
				for _, problem := range directiveProblems(tag) {
					t.Errorf("%s.%s: %s", path, f.Name, problem)
				}
			}
			walk(f.Type, path+"."+f.Name)
		}
	}
	walk(reflect.TypeFor[Bootstrap](), "Bootstrap")
	walk(reflect.TypeFor[Company](), "Company")
	if len(seen) < 10 {
		t.Fatalf("walked %d struct types — the walk is not reaching the config", len(seen))
	}
}

// directiveProblems is every way one tag fails the grammar the generator
// reads: an unknown key, a bound that is not a number, a pattern that does
// not compile.
func directiveProblems(tag string) []string {
	var out []string
	for key, value := range parseDirectives(tag) {
		switch key {
		case "required":
			if value != "" {
				out = append(out, "required takes no value, got "+strconv.Quote(value))
			}
		case "min", "max":
			if _, err := strconv.ParseFloat(value, 64); err != nil {
				out = append(out, key+" is not a number: "+strconv.Quote(value))
			}
		case "enum":
			if value == "" || strings.Contains(value, ",") {
				out = append(out, "enum is |-separated and non-empty, got "+strconv.Quote(value))
			}
		case "pattern":
			if _, err := regexp.Compile(value); err != nil {
				out = append(out, "pattern does not compile: "+err.Error())
			}
		default:
			out = append(out, "unknown directive "+strconv.Quote(key))
		}
	}
	return out
}
