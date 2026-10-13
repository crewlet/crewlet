package config_test

import (
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// THE CODE HOST'S RULES, each refused at the line an operator wrote.
//
// code.limits is what every push is held to, so a value that admits no push a
// person could make — a mebibyte cap below a first commit, a file limit no
// push can carry, a push limit no repository can hold — is refused here rather
// than met as a refused push. The ORDER rules compare the RESOLVED limits,
// because that is what a push is held to: a push limit set alone below the
// default file limit is refused although only one field was written, and the
// problem sits on the field the document SET, naming the default it met.
func TestTheCodeBlocksRules(t *testing.T) {
	t.Parallel()
	const company = "name: Acme\n"
	for name, tc := range map[string]struct {
		code string
		// path and kind are the one problem the block must be refused
		// with; an empty path accepts it.
		path, kind string
		// says is what the message must name, beside the path.
		says []string
	}{
		"native alone": {code: "code:\n  native: true\n"},
		"native with every limit at its bound": {code: "code:\n  native: true\n  limits:\n" +
			"    max_push_bytes: 1073741824\n    max_blob_bytes: 1024\n    repo_max_bytes: 1099511627776\n"},
		"limits with nothing hosting code": {
			code: "code:\n  limits:\n    max_blob_bytes: 1048576\n",
			path: "code.limits", kind: "conflict", says: []string{"code.native"},
		},
		"a push limit below a mebibyte": {
			code: "code:\n  native: true\n  limits:\n    max_push_bytes: 512\n",
			path: "code.limits.max_push_bytes", kind: "out_of_range", says: []string{"1048576"},
		},
		"a file limit past the push ceiling": {
			code: "code:\n  native: true\n  limits:\n    max_blob_bytes: 2147483648\n",
			path: "code.limits.max_blob_bytes", kind: "out_of_range", says: []string{"1073741824"},
		},
		"a repository limit past a tebibyte": {
			code: "code:\n  native: true\n  limits:\n    repo_max_bytes: 2199023255552\n",
			path: "code.limits.repo_max_bytes", kind: "out_of_range", says: []string{"1099511627776"},
		},
		"a file limit past the push limit, both set": {
			code: "code:\n  native: true\n  limits:\n    max_push_bytes: 52428800\n" +
				"    max_blob_bytes: 62914560\n",
			path: "code.limits.max_blob_bytes", kind: "conflict",
			says: []string{"62914560", "52428800"},
		},
		"a push limit alone below the default file limit": {
			code: "code:\n  native: true\n  limits:\n    max_push_bytes: 10485760\n",
			path: "code.limits.max_push_bytes", kind: "conflict",
			says: []string{"max_blob_bytes (unset: its default, 52428800)", "10485760"},
		},
		"a repository limit alone below the default push limit": {
			code: "code:\n  native: true\n  limits:\n    repo_max_bytes: 1048576\n",
			path: "code.limits.repo_max_bytes", kind: "conflict",
			says: []string{"max_push_bytes (unset: its default, 1073741824)", "1048576"},
		},
		"a push limit past the repository limit, both set": {
			code: "code:\n  native: true\n  limits:\n    max_push_bytes: 20971520\n" +
				"    max_blob_bytes: 1048576\n    repo_max_bytes: 10485760\n",
			path: "code.limits.max_push_bytes", kind: "conflict",
			says: []string{"20971520", "10485760"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := config.ParseCompany([]byte(company + tc.code))
			if tc.path == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			problems := config.Problems(err)
			if len(problems) != 1 {
				t.Fatalf("problems = %+v, want the one at %s: one mistake, one line", problems, tc.path)
			}
			p := problems[0]
			if p.Path != tc.path || p.Kind != tc.kind {
				t.Errorf("refused at %s (%s), want %s (%s): %s", p.Path, p.Kind, tc.path, tc.kind, p.Message)
			}
			for _, want := range tc.says {
				if !strings.Contains(p.Message, want) {
					t.Errorf("message = %q, want it to name %q", p.Message, want)
				}
			}
		})
	}
}

// THE SCHEMA ADMITS EXACTLY WHAT THE VALIDATOR DOES, for every code limit: the
// js tag is a second spelling of each bound, and a bound moved in one place
// and not the other underlines a value the engine runs, or admits one it
// refuses.
func TestTheCodeLimitsAreTheSchemasBounds(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		field    string
		min, max int64
	}{
		{"MaxPushBytes", config.CodeMinPushBytes, config.CodeMaxPushBytesCeiling},
		{"MaxBlobBytes", config.CodeMinBlobBytes, config.CodeMaxPushBytesCeiling},
		{"RepoMaxBytes", config.CodeMinRepoBytes, config.CodeMaxRepoBytesCeiling},
	} {
		field, ok := reflect.TypeFor[config.CodeLimits]().FieldByName(tc.field)
		if !ok {
			t.Errorf("config.CodeLimits has no field %s", tc.field)
			continue
		}
		bounds := map[string]int64{}
		for _, part := range strings.Split(field.Tag.Get("js"), ";") {
			key, value, found := strings.Cut(part, "=")
			if !found {
				continue
			}
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				t.Errorf("%s's js tag %q: %v", tc.field, part, err)
				continue
			}
			bounds[key] = n
		}
		if bounds["min"] != tc.min || bounds["max"] != tc.max {
			t.Errorf("%s's schema admits %d..%d and validation %d..%d — the tag "+
				"was left behind when a constant moved", tc.field,
				bounds["min"], bounds["max"], tc.min, tc.max)
		}
	}
}

// THE LIMITS ARE RESOLVED IN ONE PLACE: an unset field is its default, a set
// one passes through — what config's order rules compare and what the code
// domain holds a push to (code.LimitsOf reads this).
func TestTheCodeLimitsResolveToTheirDefaults(t *testing.T) {
	t.Parallel()
	got := config.CodeLimits{}.Resolved()
	want := config.CodeLimits{
		MaxPushBytes: config.DefaultCodeMaxPushBytes,
		MaxBlobBytes: config.DefaultCodeMaxBlobBytes,
		RepoMaxBytes: config.DefaultCodeRepoMaxBytes,
	}
	if got != want {
		t.Errorf("the unset limits resolve to %+v, want the defaults %+v", got, want)
	}
	set := config.CodeLimits{MaxPushBytes: 1 << 25, MaxBlobBytes: 1 << 20, RepoMaxBytes: 1 << 31}
	if got := set.Resolved(); got != set {
		t.Errorf("set limits resolve to %+v, want them as written %+v", got, set)
	}
}

// THE CODE HOST STARTS THE STATE LOG ON ITS OWN. Its log is one of the
// register's, so a company on vendors for its tracker and its knowledge base
// that hosts its repositories still keeps its own records on the stream — and
// a node that did not start the log for it would have no committer to decide a
// push with.
func TestRunsStateLogCountsTheCodeHost(t *testing.T) {
	t.Parallel()
	vendors := config.Company{
		Name:    "Acme",
		Tracker: config.Tracker{Backend: config.TrackerNone},
		Integrations: config.Integrations{Confluence: &config.Confluence{
			URL: "https://wiki.example.com", Token: "${T}",
		}},
	}
	if vendors.KnowledgeBackendFor() != config.KnowledgeConfluence {
		t.Fatalf("the premise: the knowledge base is %q, want confluence",
			vendors.KnowledgeBackendFor())
	}
	if vendors.RunsStateLog() {
		t.Error("a company on vendors for both halves, hosting no code, runs the state log")
	}
	hosting := vendors
	hosting.Code.Native = true
	if !hosting.RunsStateLog() {
		t.Error("a company hosting its repositories in the engine runs no state log: " +
			"nothing would decide a push")
	}
}
