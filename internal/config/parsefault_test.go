package config_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// found is what a test asserts about one parser fault.
type found struct {
	path string
	line int
	kind string
}

func faultsOf(err error) []found {
	var out []found
	for _, f := range config.Faults(err) {
		out = append(out, found{path: f.Path.String(), line: f.Line, kind: f.KindName()})
	}
	return out
}

// A PARSER FAILURE IS REPORTED AT THE KEY THE AUTHOR WROTE, ON THE LINE THEY
// WROTE IT. The strict decoder decodes a re-encoded copy of each node, so the
// line yaml reports is a line of that copy: comments and blank lines gone, a
// flow mapping reflowed, and a typo inside a seat's `llm:` mapping counted from
// the top of that mapping. None of that may reach an author, who searches
// their file for the path and jumps to the line.
func TestAParserFailureIsReportedWhereItWasWritten(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		doc  string
		want []found
	}{
		{
			name: "a top-level key below comments and blank lines",
			doc:  "# Acme\n\nname: Acme\n# the charter\n\nmission: x\nbackstroy: 1\n",
			want: []found{{"backstroy", 7, "unknown_field"}},
		},
		{
			name: "a key inside a seat's llm mapping, in a unit",
			doc: "name: Acme\nunits:\n  - name: Eng\n    roles:\n      - name: Dev\n" +
				"        # the chain\n\n        llm:\n          default: x\n          pln: y\n",
			want: []found{{"units[0].roles[0].llm.pln", 10, "unknown_field"}},
		},
		{
			name: "a key inside a tool annotation block",
			doc: "name: Acme\nmcp_servers:\n  - name: s\n    command: x\n    tool_annotations:\n" +
				"      search:\n        readOnlyHint: true\n        bogus: 1\n",
			want: []found{{"mcp_servers[0].tool_annotations.search.bogus", 8, "unknown_field"}},
		},
		{
			name: "a value of the wrong type",
			doc:  "name: Acme\nroles:\n  - name: Dev\n    token_budget: [1]\n    goal: {a: 1}\n",
			want: []found{
				{"roles[0].token_budget", 4, "shape"},
				{"roles[0].goal", 5, "shape"},
			},
		},
		{
			name: "an llm field that is neither a key nor a list",
			doc:  "name: Acme\nroles:\n  - name: Dev\n    llm_review: {a: b}\n",
			want: []found{{"roles[0].llm_review", 4, "shape"}},
		},
		{
			// A compact JSON body is one line, so only the path tells its
			// failures apart, and a typo in a nested mapping must not hide
			// the one written before it.
			name: "a compact JSON body",
			doc:  `{"name":"Acme","roles":[{"name":"Dev","backstroy":"x"},{"name":"Ops","llm":{"pln":"y"}}]}`,
			want: []found{
				{"roles[0].backstroy", 1, "unknown_field"},
				{"roles[1].llm.pln", 1, "unknown_field"},
			},
		},
		{
			// An alias decodes its anchor again at every use; it is still
			// one mistake, written once.
			name: "a typo inside an anchored block used twice",
			doc:  "name: Acme\nroles:\n  - name: Dev\n    llm: &chain {pln: y}\n  - name: Ops\n    llm: *chain\n",
			want: []found{{"roles[0].llm.pln", 4, "unknown_field"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := config.ParseCompanyDocument([]byte(tc.doc))
			if got := faultsOf(err); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("faults = %+v, want %+v\nerror: %v", got, tc.want, err)
			}
		})
	}
}

// A FAILURE IN A NESTED DECODER DOES NOT HIDE THE OTHERS. Any error but a
// yaml.TypeError returned from a custom unmarshaler aborts the whole decode,
// so a typo inside one seat's `llm:` mapping used to be the only failure a
// document reported, and its author paid a round trip per mistake.
func TestANestedFailureDoesNotHideTheOthers(t *testing.T) {
	t.Parallel()
	doc := "name: Acme\nroles:\n  - name: Dev\n    llm: {pln: y}\n    backstroy: x\n" +
		"  - name: Ops\n    llm: {default: {a: b}}\n    goal: [a]\n"
	_, err := config.ParseCompanyDocument([]byte(doc))
	want := []found{
		{"roles[0].llm.pln", 4, "unknown_field"},
		{"roles[0].backstroy", 5, "unknown_field"},
		{"roles[1].llm.default", 7, "shape"},
		{"roles[1].goal", 8, "shape"},
	}
	if got := faultsOf(err); !reflect.DeepEqual(got, want) {
		t.Errorf("faults = %+v, want %+v\nerror: %v", got, want, err)
	}
}

// A DOCUMENT THAT DOES NOT PARSE IS ONE PROBLEM. The wrap that marked it a
// wrong shape used two %w verbs, whose Unwrap looks like a join's, and the
// walk split it in two: one fault carrying yaml's message and one carrying
// nothing but "wrong shape".
func TestADocumentThatDoesNotParseIsOneProblem(t *testing.T) {
	t.Parallel()
	_, err := config.ParseCompanyDocument([]byte("name: Acme\nmission: x\nvision 2\npolicies: []\n"))
	faults := config.Faults(err)
	if len(faults) != 1 {
		t.Fatalf("%d faults, want 1: %+v", len(faults), faults)
	}
	if f := faults[0]; len(f.Path) != 0 || f.Line != 3 || !errors.Is(f.Kind, config.ErrShape) {
		t.Errorf("fault = %+v, want a wrong shape with no path on line 3", f)
	}

	// The rule is about how an error renders, not about where it came from:
	// one line is one failure, however many errors it wraps.
	if got := config.Faults(fmt.Errorf("%w: %w", config.ErrShape, errors.New("x"))); len(got) != 1 {
		t.Errorf("a two-%%w error flattened into %d faults", len(got))
	}
}

// EVERY PROBLEM IN A FILE IS REPORTED, not the first. A company file's
// failures reach a caller wrapped in the file's name, and the walk used to
// stop at that wrap and take the first fault inside it, so `crewlet validate
// -json` listed one problem for a file with several.
func TestEveryProblemInAFileIsReported(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "company.yaml")
	doc := "name: Acme\ntoken_budget: -1\nnotification_rate_limit: -2\n"
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := config.LoadCompany(path)
	want := []found{
		{"token_budget", 0, "out_of_range"},
		{"notification_rate_limit", 0, "out_of_range"},
	}
	if got := faultsOf(err); !reflect.DeepEqual(got, want) {
		t.Errorf("faults = %+v, want %+v\nerror: %v", got, want, err)
	}
}

// TIER A IS PLACED THE SAME WAY, after its references are resolved in place:
// a retired key keeps its own advice and gains its path and line.
func TestABootstrapParserFailureIsReportedWhereItWasWritten(t *testing.T) {
	t.Parallel()
	_, err := config.ParseBootstrap([]byte("# node\n\nstore:\n  driver: sqlite\n"), config.EnvOnly())
	if got, want := faultsOf(err), []found{{"store.driver", 4, "unknown_field"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("faults = %+v, want %+v\nerror: %v", got, want, err)
	}
}
