package config

import (
	"reflect"
	"regexp"
	"slices"
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
	if problems := directiveProblems("pointer"); len(problems) == 0 {
		t.Fatal("control: a pointer with no rule to stand beside was accepted")
	}
	if problems := directiveProblems("pattern=^[a-z]+$;pointer"); len(problems) != 0 {
		t.Fatalf("control: a pointer beside a pattern was refused: %v", problems)
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
				if _, pointer := parseDirectives(tag)["pointer"]; pointer && elem(f.Type).Kind() != reflect.String {
					t.Errorf("%s.%s: pointer is a text field's directive — a %s is "+
						"never a ${VAR} held verbatim", path, f.Name, elem(f.Type).Kind())
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

// A POINTER DIRECTIVE IS A TIER B STATEMENT, and every one the generator
// reads is one it acts on.
//
// Tier A resolves every reference before it decodes, so a text field there
// already admits one wherever it sits, and `pointer` on it would state a rule
// the schema never applies. Asserted over what Tier A actually reaches rather
// than by a list of Tier B types, so a Tier B type later shared with Tier A is
// caught the day it is. The Company half is the control: a walk that reached
// no pointer at all would pass the Tier A half having read nothing.
func TestAPointerDirectiveIsTierBs(t *testing.T) {
	t.Parallel()
	if got := pointerFields(reflect.TypeFor[Bootstrap](), "Bootstrap"); len(got) != 0 {
		t.Errorf("Tier A resolves every ${VAR} at load, so a pointer directive "+
			"there states nothing: %v", got)
	}
	got := pointerFields(reflect.TypeFor[Company](), "Company")
	if !slices.ContainsFunc(got, func(path string) bool {
		return strings.HasSuffix(path, "RoleMattermost.Username")
	}) {
		t.Fatalf("the walk over Tier B found the pointers %v and not the "+
			"Mattermost username's — it is not reaching the config", got)
	}
}

// pointerFields is every field carrying a pointer directive that a walk from
// root reaches, as the owning type's name and the field's.
func pointerFields(root reflect.Type, name string) []string {
	var out []string
	seen := map[reflect.Type]bool{}
	var walk func(reflect.Type)
	walk = func(typ reflect.Type) {
		typ = elem(typ)
		if typ.Kind() != reflect.Struct || seen[typ] {
			return
		}
		seen[typ] = true
		for i := range typ.NumField() {
			f := typ.Field(i)
			if _, pointer := parseDirectives(f.Tag.Get("js"))["pointer"]; pointer {
				out = append(out, name+": "+typ.Name()+"."+f.Name)
			}
			walk(f.Type)
		}
	}
	walk(root)
	return out
}

// elem is the type a field's value is finally made of, beneath its pointers,
// slices, arrays and maps.
func elem(typ reflect.Type) reflect.Type {
	for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice ||
		typ.Kind() == reflect.Array || typ.Kind() == reflect.Map {
		typ = typ.Elem()
	}
	return typ
}

// directiveProblems is every way one tag fails the grammar the generator
// reads: an unknown key, a bound that is not a number, a pattern that does
// not compile, a pointer with no rule to stand beside.
func directiveProblems(tag string) []string {
	var out []string
	directives := parseDirectives(tag)
	for key, value := range directives {
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
		case "pointer":
			// A text field with neither rule already admits any text, a
			// whole reference included, so the directive would change
			// nothing the schema says.
			_, pattern := directives["pattern"]
			_, enum := directives["enum"]
			switch {
			case value != "":
				out = append(out, "pointer takes no value, got "+strconv.Quote(value))
			case !pattern && !enum:
				out = append(out, "pointer admits a whole ${VAR} beside a pattern or an "+
					"enum, and this tag has neither")
			}
		default:
			out = append(out, "unknown directive "+strconv.Quote(key))
		}
	}
	return out
}
