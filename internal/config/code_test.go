package config_test

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/gitref"
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
			"    max_push_bytes: 1073741824\n    max_blob_bytes: 1024\n    repo_max_bytes: 1099511627776\n" +
			"    pushes_per_seat_day: 100000\n"},
		"a seat's quota of one push a day": {code: "code:\n  native: true\n  limits:\n    pushes_per_seat_day: 1\n"},
		"limits with nothing hosting code": {
			code: "code:\n  limits:\n    max_blob_bytes: 1048576\n",
			path: "code.limits", kind: "conflict", says: []string{"code.native"},
		},
		"a seat's quota with nothing hosting code": {
			code: "code:\n  limits:\n    pushes_per_seat_day: 50\n",
			path: "code.limits", kind: "conflict", says: []string{"code.native"},
		},
		"a seat's quota below one push": {
			code: "code:\n  native: true\n  limits:\n    pushes_per_seat_day: -1\n",
			path: "code.limits.pushes_per_seat_day", kind: "out_of_range", says: []string{"1..100000", "1000"},
		},
		"a seat's quota past its ceiling": {
			code: "code:\n  native: true\n  limits:\n    pushes_per_seat_day: 100001\n",
			path: "code.limits.pushes_per_seat_day", kind: "out_of_range", says: []string{"100001", "1..100000"},
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
		{"PushesPerSeatDay", config.CodeMinPushesPerSeatDay, config.CodePushesPerSeatDayCeiling},
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

// THE SCRATCH POLICY IS BOUNDED, refused at the field an operator wrote: an
// idle time of at least a day — what a live turn's hold record is derived from
// — and at most a year, and a cap of 0..1000 scratch repositories a seat.
func TestTheCodeScratchIsBounded(t *testing.T) {
	t.Parallel()
	const company = "name: Acme\ncode:\n  native: true\n  scratch:\n"
	for name, tc := range map[string]struct {
		scratch    string
		path, kind string
		says       []string
	}{
		"both at their upper bounds": {scratch: "    idle_days: 365\n    max_per_seat: 1000\n"},
		"an idle time of a day":      {scratch: "    idle_days: 1\n"},
		"an idle time below a day": {scratch: "    idle_days: -1\n",
			path: "code.scratch.idle_days", kind: "out_of_range", says: []string{"1..365", "7"}},
		"an idle time past a year": {scratch: "    idle_days: 366\n",
			path: "code.scratch.idle_days", kind: "out_of_range", says: []string{"366", "1..365"}},
		"a negative cap": {scratch: "    max_per_seat: -1\n",
			path: "code.scratch.max_per_seat", kind: "out_of_range", says: []string{"0..1000", "50"}},
		"a cap past its ceiling": {scratch: "    max_per_seat: 1001\n",
			path: "code.scratch.max_per_seat", kind: "out_of_range", says: []string{"1001", "0..1000"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := config.ParseCompany([]byte(company + tc.scratch))
			if tc.path == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			problems := config.Problems(err)
			if len(problems) != 1 {
				t.Fatalf("problems = %+v, want the one at %s", problems, tc.path)
			}
			if p := problems[0]; p.Path != tc.path || p.Kind != tc.kind {
				t.Errorf("refused at %s (%s), want %s (%s): %s", p.Path, p.Kind, tc.path, tc.kind, p.Message)
			} else {
				for _, want := range tc.says {
					if !strings.Contains(p.Message, want) {
						t.Errorf("message = %q, want it to name %q", p.Message, want)
					}
				}
			}
		})
	}
}

// NOTHING HOSTS A SCRATCH POLICY WITHOUT THE NATIVE CODE HOST: a block that
// would read as live and govern nothing is refused, as code.limits is.
//
// Mutation: drop the scratch arm of the rule and the block is accepted — red.
func TestCodeScratchNeedsTheNativeHost(t *testing.T) {
	t.Parallel()
	_, err := config.ParseCompany([]byte("name: Acme\ncode:\n  scratch:\n    idle_days: 3\n"))
	problems := config.Problems(err)
	if len(problems) != 1 || problems[0].Path != "code.scratch" || problems[0].Kind != "conflict" ||
		!strings.Contains(problems[0].Message, "code.native: true") {
		t.Fatalf("code.scratch with nothing hosting code = %+v, want one conflict at code.scratch naming "+
			"code.native: true", problems)
	}
}

// ZERO SCRATCH REPOSITORIES A SEAT IS A SETTING, AND UNSET IS FIFTY: the cap is
// a pointer because a company that lets no seat keep a scratch repository says
// 0, and an int field could not tell that from a document that said nothing.
//
// Mutation: make MaxPerSeat an int, and 0 resolves to fifty — red (it fails
// to compile, which is the field's whole point, beside the readings below).
func TestScratchMaxPerSeatZeroIsASetting(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		scratch          string
		idle, maxPerSeat int
	}{
		"unset":                   {scratch: "", idle: config.DefaultCodeScratchIdleDays, maxPerSeat: config.DefaultCodeScratchMaxPerSeat},
		"no scratch repositories": {scratch: "  scratch:\n    max_per_seat: 0\n", idle: 7, maxPerSeat: 0},
		"set":                     {scratch: "  scratch:\n    idle_days: 3\n    max_per_seat: 12\n", idle: 3, maxPerSeat: 12},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c, err := config.ParseCompany([]byte("name: Acme\ncode:\n  native: true\n" + tc.scratch))
			if err != nil {
				t.Fatalf("refused: %v", err)
			}
			if idle, most := c.Code.Scratch.Resolved(); idle != tc.idle || most != tc.maxPerSeat {
				t.Errorf("code.scratch resolves to (%d days, %d a seat), want (%d, %d)", idle, most,
					tc.idle, tc.maxPerSeat)
			}
		})
	}
	if config.DefaultCodeScratchIdleDays != 7 || config.DefaultCodeScratchMaxPerSeat != 50 {
		t.Errorf("the scratch defaults are %d days and %d a seat, and the approved design states 7 and 50",
			config.DefaultCodeScratchIdleDays, config.DefaultCodeScratchMaxPerSeat)
	}
}

// THE SCHEMA ADMITS EXACTLY WHAT THE VALIDATOR DOES for the scratch policy, as
// for the limits.
func TestTheCodeScratchIsTheSchemasBounds(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		field    string
		min, max int64
	}{
		{"IdleDays", config.CodeScratchIdleDaysFloor, config.CodeScratchIdleDaysCeiling},
		{"MaxPerSeat", 0, config.CodeScratchMaxPerSeatCeiling},
	} {
		field, ok := reflect.TypeFor[config.CodeScratch]().FieldByName(tc.field)
		if !ok {
			t.Errorf("config.CodeScratch has no field %s", tc.field)
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
			t.Errorf("%s's schema admits %d..%d and validation %d..%d", tc.field,
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
		MaxPushBytes:     config.DefaultCodeMaxPushBytes,
		MaxBlobBytes:     config.DefaultCodeMaxBlobBytes,
		RepoMaxBytes:     config.DefaultCodeRepoMaxBytes,
		PushesPerSeatDay: config.DefaultCodePushesPerSeatDay,
	}
	if got != want {
		t.Errorf("the unset limits resolve to %+v, want the defaults %+v", got, want)
	}
	set := config.CodeLimits{MaxPushBytes: 1 << 25, MaxBlobBytes: 1 << 20, RepoMaxBytes: 1 << 31,
		PushesPerSeatDay: 7}
	if got := set.Resolved(); got != set {
		t.Errorf("set limits resolve to %+v, want them as written %+v", got, set)
	}
}

// codeChart is a company with a chart to own repositories: a root seat, and two
// units — one keyed by its name, one by an id its name does not spell.
const codeChart = `name: Acme
roles:
  - name: CEO
    handle: ceo
units:
  - name: Engineering
    roles:
      - name: SWE
        handle: swe
  - name: Platform Team
    id: platform
    roles:
      - name: SRE
        handle: sre
`

// codeRepos is codeChart hosting code, with repos as code.repos' entries.
func codeRepos(repos string) string {
	return codeChart + "code:\n  native: true\n  repos:\n" + repos
}

// EVERY RULE OF code.repos REFUSES AT THE FIELD IT IS ABOUT, AS ONE PROBLEM.
//
// The reconcile builds each repository's record from its declaration against
// the chart of the activation it applies, so a declaration it cannot record —
// a name that is no repository name, an owner the chart does not resolve, two
// rules on one pattern, force on a tag — is refused before it is applied,
// where an author can find the line, rather than logged on every data node at
// every activation. One mistake is one problem: a name that is not one is not
// also a duplicate, and a rule with no pattern is not also a pattern twice.
func TestTheCodeBlockRefusesByField(t *testing.T) {
	t.Parallel()
	web := "    - name: web\n      owner: {unit: Engineering}\n"
	rule := func(r string) string {
		return web + "      protect:\n        - " + r + "\n"
	}
	owner := func(entries ...string) string {
		return web + "      path_owners:\n        - " + strings.Join(entries, "\n        - ") + "\n"
	}
	for name, tc := range map[string]struct {
		doc string
		// path and kind are the one problem the document must be refused
		// with.
		path, kind string
		// says is what the message must name, beside the path.
		says []string
	}{
		"repositories with nothing hosting code": {
			doc:  codeChart + "code:\n  repos:\n" + web,
			path: "code.repos", kind: "conflict", says: []string{"code.native"},
		},
		"a retention with nothing hosting code": {
			doc:  codeChart + "code:\n  deleted_retention_days: 30\n",
			path: "code.deleted_retention_days", kind: "conflict", says: []string{"code.native"},
		},
		"a retention past a year": {
			doc:  codeChart + "code:\n  native: true\n  deleted_retention_days: 366\n",
			path: "code.deleted_retention_days", kind: "out_of_range", says: []string{"1..365", "366"},
		},
		"a negative retention": {
			doc:  codeChart + "code:\n  native: true\n  deleted_retention_days: -1\n",
			path: "code.deleted_retention_days", kind: "out_of_range", says: []string{"1..365"},
		},
		"no name": {
			doc:  codeRepos("    - owner: {unit: Engineering}\n"),
			path: "code.repos[0].name", kind: "missing",
		},
		"a name that is no repository name": {
			doc:  codeRepos("    - name: Web.App\n      owner: {unit: Engineering}\n"),
			path: "code.repos[0].name", kind: "shape", says: []string{"Web.App", "63"},
		},
		"a name ending in a hyphen": {
			doc:  codeRepos("    - name: web-\n      owner: {unit: Engineering}\n"),
			path: "code.repos[0].name", kind: "shape",
		},
		"one name declared twice": {
			doc:  codeRepos(web + "    - name: api\n      owner: {seat: swe}\n" + web),
			path: "code.repos[2].name", kind: "conflict", says: []string{`"web"`, "repos[0]"},
		},
		"no owner": {
			doc:  codeRepos("    - name: web\n"),
			path: "code.repos[0].owner", kind: "missing",
		},
		"two owners": {
			doc:  codeRepos("    - name: web\n      owner: {unit: Engineering, seat: swe}\n"),
			path: "code.repos[0].owner", kind: "conflict",
		},
		// EACH UNIT BY THE KEY IT IS REFERENCED BY AND ITS NAME: the write
		// mints Engineering's key, so the person who typed a name reads the
		// key beside the name they know, and a unit whose id is not its name
		// is found by either.
		"a unit the chart does not have": {
			doc:  codeRepos("    - name: web\n      owner: {unit: Marketing}\n"),
			path: "code.repos[0].owner.unit", kind: "unknown_value",
			says: []string{`"Marketing"`,
				"the units are: engineering (Engineering), platform (Platform Team)"},
		},
		"a seat the chart does not have": {
			doc:  codeRepos("    - name: web\n      owner: {seat: cto}\n"),
			path: "code.repos[0].owner.seat", kind: "unknown_value", says: []string{"cto", "ceo", "swe"},
		},
		"a default branch that is no branch": {
			doc:  codeRepos(web + "      default_branch: main..next\n"),
			path: "code.repos[0].default_branch", kind: "shape", says: []string{"main..next"},
		},
		// refs/heads/seats holds only each seat's own namespace: no push may
		// create the root or a namespace as a branch, so a repository whose
		// default branch is one would have one nobody can ever push.
		"the seats root as the default branch": {
			doc:  codeRepos(web + "      default_branch: seats\n"),
			path: "code.repos[0].default_branch", kind: "conflict",
			says: []string{`"seats"`, "reserved", "seats/<handle>/"},
		},
		"a seat's namespace as the default branch": {
			doc:  codeRepos(web + "      default_branch: seats/main\n"),
			path: "code.repos[0].default_branch", kind: "conflict",
			says: []string{`"seats/main"`, "reserved", "seats/<handle>/"},
		},
		"a description past its bound": {
			doc: codeRepos(web + "      description: " +
				strings.Repeat("x", config.CodeMaxDescriptionBytes+1) + "\n"),
			path: "code.repos[0].description", kind: "out_of_range", says: []string{"1025", "1024"},
		},
		"too many rules": {
			doc: func() string {
				var b strings.Builder
				b.WriteString(web + "      protect:\n")
				for i := range config.CodeMaxProtections + 1 {
					fmt.Fprintf(&b, "        - branch: b%d\n", i)
				}
				return codeRepos(b.String())
			}(),
			path: "code.repos[0].protect", kind: "out_of_range", says: []string{"65", "64"},
		},
		"a rule protecting nothing": {
			doc:  codeRepos(rule("{allow_delete: true}")),
			path: "code.repos[0].protect[0]", kind: "missing",
		},
		"a rule on a branch and a tag": {
			doc:  codeRepos(rule("{branch: main, tag: v1}")),
			path: "code.repos[0].protect[0]", kind: "conflict",
		},
		"a branch pattern that is no pattern": {
			doc:  codeRepos(rule("{branch: 'release/../x'}")),
			path: "code.repos[0].protect[0].branch", kind: "shape", says: []string{"release/../x"},
		},
		"a tag pattern that is no pattern": {
			doc:  codeRepos(rule("{tag: 'v1:x'}")),
			path: "code.repos[0].protect[0].tag", kind: "shape", says: []string{"v1:x"},
		},
		"one pattern protected twice": {
			doc: codeRepos(web + "      protect:\n        - branch: main\n        - tag: main\n" +
				"        - {branch: main, allow_delete: true}\n"),
			path: "code.repos[0].protect[2]", kind: "conflict",
			says: []string{"refs/heads/main", "protect[0]"},
		},
		"force on a tag": {
			doc:  codeRepos(rule("{tag: 'v*', allow_force: true}")),
			path: "code.repos[0].protect[0].allow_force", kind: "conflict", says: []string{"tag never moves"},
		},
		"a direct push nobody knows": {
			doc:  codeRepos(rule("{branch: main, direct_push: admins}")),
			path: "code.repos[0].protect[0].direct_push", kind: "unknown_value",
			says: []string{"admins", "persons, none"},
		},
		"a path owner owning no path": {
			doc:  codeRepos(owner("{unit: Engineering}")),
			path: "code.repos[0].path_owners[0].paths", kind: "missing", says: []string{"dir/**"},
		},
		"a path owner who is nobody": {
			doc:  codeRepos(owner(`{paths: ["web/**"]}`)),
			path: "code.repos[0].path_owners[0]", kind: "missing",
		},
		"a path owner who is two": {
			doc:  codeRepos(owner(`{paths: ["web/**"], unit: Engineering, seat: swe}`)),
			path: "code.repos[0].path_owners[0]", kind: "conflict",
		},
		"a path owner the chart does not have": {
			doc:  codeRepos(owner(`{paths: ["web/**"], unit: Marketing}`)),
			path: "code.repos[0].path_owners[0].unit", kind: "unknown_value",
			says: []string{`"Marketing"`, "the units are: engineering (Engineering), platform (Platform Team)"},
		},
		"a path owner's seat the chart does not have": {
			doc:  codeRepos(owner(`{paths: ["web/**"], seat: cto}`)),
			path: "code.repos[0].path_owners[0].seat", kind: "unknown_value", says: []string{"cto", "ceo", "swe"},
		},
		"a path that is no path glob": {
			doc:  codeRepos(owner(`{paths: ["web/**", "/docs/**"], unit: Engineering}`)),
			path: "code.repos[0].path_owners[0].paths[1]", kind: "shape",
			says: []string{`"/docs/**"`, "relative to the repository's root"},
		},
		"a path listed twice": {
			doc:  codeRepos(owner(`{paths: ["web/**", "docs/**", "web/**"], unit: Engineering}`)),
			path: "code.repos[0].path_owners[0].paths[2]", kind: "conflict", says: []string{`"web/**"`, "paths[0]"},
		},
		"a path owner past its paths": {
			doc: func() string {
				var globs []string
				for i := range config.CodeMaxPathOwnerPaths + 1 {
					globs = append(globs, fmt.Sprintf("%q", fmt.Sprintf("d%d/**", i)))
				}
				return codeRepos(owner("{paths: [" + strings.Join(globs, ", ") + "], seat: swe}"))
			}(),
			path: "code.repos[0].path_owners[0].paths", kind: "out_of_range", says: []string{"33", "32"},
		},
		// ONE OWNER BY TWO SPELLINGS IS ONE OWNER NAMED TWICE: the record
		// names a unit by its key, whichever the document used.
		"one path owner named twice": {
			doc: codeRepos(owner(`{paths: ["web/**"], unit: Engineering}`,
				`{paths: ["docs/**"], unit: engineering}`)),
			path: "code.repos[0].path_owners[1]", kind: "conflict",
			says: []string{"unit engineering", "path_owners[0]"},
		},
		// THE MERGE GATE, each field at itself.
		"a merge authority nobody knows": {
			doc:  codeRepos(rule("{branch: main, merge_by: admins}")),
			path: "code.repos[0].protect[0].merge_by", kind: "unknown_value",
			says: []string{"admins", "writers, maintainers, persons"},
		},
		"two approvals": {
			doc:  codeRepos(rule("{branch: main, approvals: two}")),
			path: "code.repos[0].protect[0].approvals", kind: "unknown_value", says: []string{"none, one"},
		},
		"no_code_change exempting approvals": {
			doc:  codeRepos(rule("{branch: main, approvals: one, approvals_unless: [no_code_change]}")),
			path: "code.repos[0].protect[0].approvals_unless[0]", kind: "conflict",
			says: []string{"already carries", "whitespace_only, comment_only"},
		},
		"an exempting class nobody knows": {
			doc:  codeRepos(rule("{branch: main, approvals: one, approvals_unless: [docs_only]}")),
			path: "code.repos[0].protect[0].approvals_unless[0]", kind: "unknown_value", says: []string{"docs_only"},
		},
		"an exempting class twice": {
			doc: codeRepos(rule("{branch: main, approvals: one, " +
				"approvals_unless: [comment_only, comment_only]}")),
			path: "code.repos[0].protect[0].approvals_unless[1]", kind: "conflict",
			says: []string{"comment_only", "approvals_unless[0]"},
		},
		"a human path with no glob": {
			doc:  codeRepos(rule("{branch: main, human_paths: [{unless: [comment_only]}]}")),
			path: "code.repos[0].protect[0].human_paths[0].glob", kind: "missing", says: []string{"deploy/**"},
		},
		"a human path that is no glob": {
			doc:  codeRepos(rule("{branch: main, human_paths: [{glob: '/deploy/**'}]}")),
			path: "code.repos[0].protect[0].human_paths[0].glob", kind: "shape",
			says: []string{`"/deploy/**"`, "relative to the repository's root"},
		},
		"a human path twice": {
			doc:  codeRepos(rule("{branch: main, human_paths: [{glob: 'deploy/**'}, {glob: 'deploy/**'}]}")),
			path: "code.repos[0].protect[0].human_paths[1].glob", kind: "conflict",
			says: []string{`"deploy/**"`, "human_paths[0]"},
		},
		"no_code_change sparing a human path": {
			doc:  codeRepos(rule("{branch: main, human_paths: [{glob: 'deploy/**', unless: [no_code_change]}]}")),
			path: "code.repos[0].protect[0].human_paths[0].unless[0]", kind: "conflict",
			says: []string{"already carries"},
		},
		"too many human paths": {
			doc: func() string {
				var globs []string
				for i := range config.CodeMaxHumanPaths + 1 {
					globs = append(globs, fmt.Sprintf("{glob: 'd%d/**'}", i))
				}
				return codeRepos(rule("{branch: main, human_paths: [" + strings.Join(globs, ", ") + "]}"))
			}(),
			path: "code.repos[0].protect[0].human_paths", kind: "out_of_range", says: []string{"33", "32"},
		},
		"a check that is no name": {
			doc:  codeRepos(rule("{branch: main, checks: ['Unit Tests']}")),
			path: "code.repos[0].protect[0].checks[0]", kind: "shape", says: []string{`"Unit Tests"`},
		},
		"a check twice": {
			doc:  codeRepos(rule("{branch: main, checks: [unit, lint, unit]}")),
			path: "code.repos[0].protect[0].checks[2]", kind: "conflict", says: []string{`"unit"`, "checks[0]"},
		},
		"too many checks": {
			doc: func() string {
				var checks []string
				for i := range config.CodeMaxRequiredChecks + 1 {
					checks = append(checks, fmt.Sprintf("c%d", i))
				}
				return codeRepos(rule("{branch: main, checks: [" + strings.Join(checks, ", ") + "]}"))
			}(),
			path: "code.repos[0].protect[0].checks", kind: "out_of_range", says: []string{"33", "32"},
		},
		"a queue past its depth": {
			doc:  codeRepos(rule("{branch: main, checks: [unit], landing: {depth: 9}}")),
			path: "code.repos[0].protect[0].landing.depth", kind: "out_of_range", says: []string{"1..8", "4"},
		},
		"a negative depth": {
			doc:  codeRepos(rule("{branch: main, checks: [unit], landing: {depth: -1}}")),
			path: "code.repos[0].protect[0].landing.depth", kind: "out_of_range", says: []string{"1..8"},
		},
		"a batch past its bound": {
			doc:  codeRepos(rule("{branch: main, checks: [unit], landing: {batch: 9}}")),
			path: "code.repos[0].protect[0].landing.batch", kind: "out_of_range",
			says: []string{"1..8", "default " + strconv.Itoa(config.CodeDefaultQueueBatch)},
		},
		"a source branch's fate nobody knows": {
			doc:  codeRepos(rule("{branch: main, delete_source: always}")),
			path: "code.repos[0].protect[0].delete_source", kind: "unknown_value", says: []string{"seats, none"},
		},
		// A TAG RULE'S GATE IS ONE MISTAKE, refused at the first field it sets
		// and naming every one.
		"a merge gate on a tag rule": {
			doc:  codeRepos(rule("{tag: 'v*', merge_by: persons, checks: [unit]}")),
			path: "code.repos[0].protect[0].merge_by", kind: "conflict",
			says: []string{"tag never moves", "merge_by, checks"},
		},
		"a merge floor nobody knows": {
			doc:  codeChart + "code:\n  native: true\n  merge_by_floor: admins\n",
			path: "code.merge_by_floor", kind: "unknown_value", says: []string{"admins", "writers"},
		},
		"a merge floor with nothing hosting code": {
			doc:  codeChart + "code:\n  merge_by_floor: persons\n",
			path: "code.merge_by_floor", kind: "conflict", says: []string{"code.native"},
		},
		// A REPOSITORY'S MIRRORS AND ITS IMPORT: each URL read by the rule its
		// dial reads it by, each credential one whole ${VAR}, a push mirror's
		// seat one that reads what it publishes and its refs off the seats'
		// own branches, and no combination that would do nothing.
		"a pull mirror over plain http": {
			doc:  codeRepos(web + "      mirror_from: {url: 'http://forge.example.com/acme/web.git'}\n"),
			path: "code.repos[0].mirror_from.url", kind: "shape", says: []string{"https"},
		},
		"a credential written as the token": {
			doc: codeRepos(web + "      mirror_from: {url: 'https://forge.example.com/acme/web.git', " +
				"credential: ghp_0123456789abcdef}\n"),
			path: "code.repos[0].mirror_from.credential", kind: "shape",
			says: []string{"one whole ${VAR} reference", "crewlet secrets set"},
		},
		"a credential with text around its reference": {
			doc: codeRepos(web + "      import: {url: 'https://forge.example.com/acme/web.git', " +
				"credential: 'Bearer ${FORGE_TOKEN}'}\n"),
			path: "code.repos[0].import.credential", kind: "shape", says: []string{"one whole ${VAR} reference"},
		},
		"a push mirror with no credential": {
			doc:  codeRepos(web + "      mirror_to: {url: 'https://forge.example.com/acme/web.git', as: swe}\n"),
			path: "code.repos[0].mirror_to.credential", kind: "missing", says: []string{"a push authenticates"},
		},
		"a push mirror as no seat": {
			doc: codeRepos(web + "      mirror_to: {url: 'https://forge.example.com/acme/web.git', " +
				"credential: '${FORGE_TOKEN}', as: nobody}\n"),
			path: "code.repos[0].mirror_to.as", kind: "unknown_value", says: []string{`"nobody"`, "swe"},
		},
		"a push mirror as a seat that does not read it": {
			doc: codeRepos(web + "      visibility: unit\n      mirror_to: {url: 'https://forge.example.com/acme/web.git', " +
				"credential: '${FORGE_TOKEN}', as: sre}\n"),
			path: "code.repos[0].mirror_to.as", kind: "conflict", says: []string{"seat sre does not read web"},
		},
		"a push mirror of the seats' own branches": {
			doc: codeRepos(web + "      mirror_to: {url: 'https://forge.example.com/acme/web.git', " +
				"credential: '${FORGE_TOKEN}', as: swe, refs: ['refs/heads/**']}\n"),
			path: "code.repos[0].mirror_to.refs[0]", kind: "conflict", says: []string{"refs/heads/seats/"},
		},
		"an import beside a pull mirror": {
			doc: codeRepos(web + "      mirror_from: {url: 'https://forge.example.com/acme/web.git'}\n" +
				"      import: {url: 'https://forge.example.com/acme/web.git'}\n"),
			path: "code.repos[0].import", kind: "conflict", says: []string{"second source"},
		},
		"an archived pull mirror": {
			doc: codeRepos(web + "      archived: true\n" +
				"      mirror_from: {url: 'https://forge.example.com/acme/web.git'}\n"),
			path: "code.repos[0].mirror_from", kind: "conflict", says: []string{"takes no sync"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := config.ParseCompany([]byte(tc.doc))
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

// A REPOSITORY NAMES AT MOST CodeMaxPathOwners PATH OWNERS, refused past it at
// its path_owners as one problem — over a chart holding as many units, so no
// owner past the bound is also unknown or named twice — and accepted at it.
func TestPathOwnersAreBounded(t *testing.T) {
	t.Parallel()
	doc := func(owners int) string {
		var units, list strings.Builder
		for i := range config.CodeMaxPathOwners + 1 {
			fmt.Fprintf(&units, "  - name: Team %d\n    id: team-%d\n", i, i)
		}
		for i := range owners {
			fmt.Fprintf(&list, "        - {paths: [\"t%d/**\"], unit: team-%d}\n", i, i)
		}
		return "name: Acme\nroles:\n  - name: CEO\n    handle: ceo\nunits:\n" + units.String() +
			"code:\n  native: true\n  repos:\n    - name: web\n      owner: {seat: ceo}\n      path_owners:\n" +
			list.String()
	}
	if _, err := config.ParseCompany([]byte(doc(config.CodeMaxPathOwners))); err != nil {
		t.Fatalf("%d path owners are refused: %v", config.CodeMaxPathOwners, err)
	}
	_, err := config.ParseCompany([]byte(doc(config.CodeMaxPathOwners + 1)))
	problems := config.Problems(err)
	if len(problems) != 1 || problems[0].Path != "code.repos[0].path_owners" || problems[0].Kind != "out_of_range" ||
		!strings.Contains(problems[0].Message, "65") || !strings.Contains(problems[0].Message, "64") {
		t.Fatalf("problems = %+v, want the one at code.repos[0].path_owners naming 65 and 64", problems)
	}
}

// A REPOSITORY'S PATH OWNERS WEIGH AT MOST CodeMaxPathOwnersBytes AS THE CODE
// LOG RECORDS THEM, refused past it at its path_owners as one problem naming
// what they weigh and the bound — weighed as recorded, each unit by its KEY
// whatever the document named it by, so a figure taken from the document's
// spelling names another weight — and accepted under it. Each owner here owns
// every glob it may at gitref's bound, each byte one JSON spells as six, and
// is named by a name longer than its key.
//
// Mutation: weigh a unit by the name the document gave it — the figure the
// refusal names is not the recorded one, red.
func TestPathOwnersAreBoundedByWhatTheyWeigh(t *testing.T) {
	t.Parallel()
	type recordedOwner struct {
		Kind string `json:"kind"`
		Ref  string `json:"ref"`
	}
	type recorded struct {
		Owner recordedOwner `json:"owner"`
		Paths []string      `json:"paths"`
	}
	globs := make([]string, config.CodeMaxPathOwnerPaths)
	quoted := make([]string, len(globs))
	for i := range globs {
		globs[i] = fmt.Sprintf("%02d", i) + strings.Repeat("&", gitref.MaxPathGlobBytes-2)
		quoted[i] = strconv.Quote(globs[i])
	}
	doc := func(owners int) (string, int) {
		var units, list strings.Builder
		var want []recorded
		for i := range owners {
			fmt.Fprintf(&units, "  - name: The Team Numbered %d\n    id: t%d\n", i, i)
			fmt.Fprintf(&list, "        - {paths: [%s], unit: The Team Numbered %d}\n", strings.Join(quoted, ", "), i)
			want = append(want, recorded{Owner: recordedOwner{Kind: "unit", Ref: fmt.Sprintf("t%d", i)}, Paths: globs})
		}
		weight, err := json.Marshal(want)
		if err != nil {
			t.Fatal(err)
		}
		return "name: Acme\nroles:\n  - name: CEO\n    handle: ceo\nunits:\n" + units.String() +
			"code:\n  native: true\n  repos:\n    - name: web\n      owner: {seat: ceo}\n      path_owners:\n" +
			list.String(), len(weight)
	}
	under, weighs := doc(1)
	for owners := 2; weighs <= config.CodeMaxPathOwnersBytes; owners++ {
		next, w := doc(owners)
		if w > config.CodeMaxPathOwnersBytes {
			break
		}
		under, weighs = next, w
	}
	if _, err := config.ParseCompany([]byte(under)); err != nil {
		t.Fatalf("path owners weighing %d bytes are refused: %v", weighs, err)
	}
	over, w := doc(config.CodeMaxPathOwners)
	_, err := config.ParseCompany([]byte(over))
	problems := config.Problems(err)
	if len(problems) != 1 || problems[0].Path != "code.repos[0].path_owners" || problems[0].Kind != "out_of_range" ||
		!strings.Contains(problems[0].Message, fmt.Sprintf("%d bytes", w)) ||
		!strings.Contains(problems[0].Message, strconv.Itoa(config.CodeMaxPathOwnersBytes)) {
		t.Fatalf("problems = %+v, want the one at code.repos[0].path_owners naming %d bytes and %d", problems, w,
			config.CodeMaxPathOwnersBytes)
	}
}

// A GATE ON A TAG RULE IS REFUSED, whichever of its fields the rule sets: a tag
// never moves, so no merge reaches one and the gate has nothing to hold — one
// problem, at the field, as `allow_force` on a tag is.
func TestAGateOnATagRuleIsRefused(t *testing.T) {
	t.Parallel()
	for field, value := range map[string]string{
		"merge_by": "persons", "approvals": "one", "approvals_unless": "[comment_only]",
		"human_paths": "[{glob: 'deploy/**'}]", "checks": "[unit]", "landing": "{depth: 2}",
		"owners_approve": "true", "delete_source": "none", "agents_edit_checks": "true",
	} {
		_, err := config.ParseCompany([]byte(codeRepos("    - name: web\n      owner: {seat: swe}\n" +
			"      protect:\n        - {branch: main}\n        - {tag: 'v*', " + field + ": " + value + "}\n")))
		problems := config.Problems(err)
		if len(problems) != 1 || problems[0].Path != "code.repos[0].protect[1]."+field ||
			problems[0].Kind != "conflict" || !strings.Contains(problems[0].Message, "tag never moves") {
			t.Errorf("%s on a tag rule: problems = %+v, want one conflict at code.repos[0].protect[1].%s",
				field, problems, field)
		}
	}
}

// A GATE NAMES NO LANDING POLICY AND NO REVERT CONSENT: a branch's queue tests
// every change before it lands, and nothing reverts a landing on its own, so
// `landing.policy` and `auto_revert` are no settings — each refused as the
// one unknown field it is, at its line, by the loader's rule for any key the
// schema does not define, rather than accepted and never acted on.
//
// Mutation: give CodeLanding a `policy` field, or CodeProtection an
// `auto_revert` one, again — the document is accepted, red.
func TestAGateNamesNoLandingPolicyAndNoRevertConsent(t *testing.T) {
	t.Parallel()
	for path, rule := range map[string]string{
		"code.repos[0].protect[0].landing.policy": "{branch: main, checks: [unit], landing: {policy: pre_land}}",
		"code.repos[0].protect[0].auto_revert":    "{branch: main, checks: [unit], auto_revert: true}",
	} {
		_, err := config.ParseCompany([]byte(codeRepos("    - name: web\n      owner: {seat: swe}\n" +
			"      protect:\n        - " + rule + "\n")))
		problems := config.Problems(err)
		if !errors.Is(err, config.ErrUnknownField) || len(problems) != 1 || problems[0].Path != path ||
			problems[0].Kind != "unknown_field" {
			t.Errorf("%s: err = %v, problems = %+v, want the one unknown_field at %s", rule, err, problems, path)
		}
	}
}

// A GATE IS FORMED BY ANY OF ITS FIELDS, THE REST RESOLVED — writers, none,
// a queue at depth 4 and batch 2, seats — in one place (CodeProtection.Gate),
// which the reconcile records and the gate's bound weighs; and a rule that
// sets none of them has no gate.
func TestAGateResolvesItsDefaults(t *testing.T) {
	t.Parallel()
	if g := (config.CodeProtection{Branch: "main", AllowDelete: true}).Gate(); g != nil {
		t.Fatalf("a rule setting no gate field has the gate %+v", *g)
	}
	defaults := config.CodeGate{MergeBy: config.CodeMergeByWriters, Approvals: config.CodeApprovalsNone,
		Landing:      config.CodeLanding{Depth: config.CodeDefaultQueueDepth, Batch: config.CodeDefaultQueueBatch},
		DeleteSource: config.CodeDeleteSeats}
	for name, tc := range map[string]struct {
		rule config.CodeProtection
		want func(*config.CodeGate)
	}{
		"merge_by":  {config.CodeProtection{MergeBy: config.CodeMergeByPersons}, func(g *config.CodeGate) { g.MergeBy = config.CodeMergeByPersons }},
		"approvals": {config.CodeProtection{Approvals: config.CodeApprovalsOne}, func(g *config.CodeGate) { g.Approvals = config.CodeApprovalsOne }},
		"checks":    {config.CodeProtection{Checks: []string{"unit"}}, func(g *config.CodeGate) { g.Checks = []string{"unit"} }},
		"a depth":   {config.CodeProtection{Landing: config.CodeLanding{Depth: 2}}, func(g *config.CodeGate) { g.Landing.Depth = 2 }},
		"a batch":   {config.CodeProtection{Landing: config.CodeLanding{Batch: 8}}, func(g *config.CodeGate) { g.Landing.Batch = 8 }},
		"delete_source": {config.CodeProtection{DeleteSource: config.CodeDeleteNone},
			func(g *config.CodeGate) { g.DeleteSource = config.CodeDeleteNone }},
		"agents_edit_checks": {config.CodeProtection{AgentsEditChecks: true},
			func(g *config.CodeGate) { g.AgentsEditChecks = true }},
	} {
		tc.rule.Branch = "main"
		want := defaults
		tc.want(&want)
		if got := tc.rule.Gate(); got == nil || !reflect.DeepEqual(*got, want) {
			t.Errorf("%s alone forms the gate %+v, want %+v", name, got, want)
		}
	}
	if got := config.CodeMergeBy("").Resolved(); got != config.CodeMergeByWriters {
		t.Errorf("an unset code.merge_by_floor is %q, want writers", got)
	}
}

// A GATE WEIGHS AT MOST CodeMaxGateBytes as the code log records it, refused at
// its rule past it — reached here with no field at fault, every check at its
// longest name and human paths of globs JSON spells long (each & six bytes) —
// and accepted at it.
func TestAGateIsBoundedByItsBytes(t *testing.T) {
	t.Parallel()
	doc := func(paths int) (string, int) {
		rule := config.CodeProtection{Branch: "main"}
		for i := range config.CodeMaxRequiredChecks {
			rule.Checks = append(rule.Checks, fmt.Sprintf("%02d", i)+strings.Repeat("x", 62))
		}
		var globs []string
		for i := range paths {
			glob := fmt.Sprintf("%02d", i) + strings.Repeat("&", 120)
			rule.HumanPaths = append(rule.HumanPaths, config.CodeHumanPath{Glob: glob})
			globs = append(globs, fmt.Sprintf("{glob: %q}", glob))
		}
		return codeRepos(fmt.Sprintf("    - name: web\n      owner: {seat: swe}\n      protect:\n"+
			"        - {branch: main, checks: [%s], human_paths: [%s]}\n", strings.Join(rule.Checks, ", "),
			strings.Join(globs, ", "))), rule.Gate().EncodedBytes()
	}
	under, n := 0, 0
	for {
		_, weight := doc(under + 1)
		if weight > config.CodeMaxGateBytes {
			break
		}
		under++
		n = weight
	}
	accepted, _ := doc(under)
	if _, err := config.ParseCompany([]byte(accepted)); err != nil {
		t.Fatalf("a gate of %d bytes is refused: %v", n, err)
	}
	refused, weight := doc(under + 1)
	_, err := config.ParseCompany([]byte(refused))
	problems := config.Problems(err)
	if len(problems) != 1 || problems[0].Path != "code.repos[0].protect[0]" || problems[0].Kind != "out_of_range" ||
		!strings.Contains(problems[0].Message, fmt.Sprint(weight)) ||
		!strings.Contains(problems[0].Message, fmt.Sprint(config.CodeMaxGateBytes)) {
		t.Fatalf("a gate of %d bytes: problems = %+v, want one out_of_range at code.repos[0].protect[0] naming "+
			"%d and %d", weight, problems, weight, config.CodeMaxGateBytes)
	}
}

// A MERGE GATE FIELD THAT DOES NOTHING IS WARNED, NEVER REFUSED, at that field:
// owners_approve where the repository names no path owners, landing where the
// rule requires no checks — only a branch whose merges wait for checks has a
// queue — and approvals_unless beside approvals: none.
// Each is silent once the field beside it is set.
func TestAGateFieldThatDoesNothingIsWarned(t *testing.T) {
	t.Parallel()
	owned := "      path_owners: [{paths: ['web/**'], seat: swe}]\n"
	for name, tc := range map[string]struct {
		rule, beside string
		warns        string // the field warned at, or none
	}{
		"owners_approve and no path owners": {rule: "owners_approve: true", warns: "owners_approve"},
		"owners_approve and path owners":    {rule: "owners_approve: true", beside: owned},
		"a landing and no checks":           {rule: "landing: {depth: 2}", warns: "landing"},
		"a landing and checks":              {rule: "landing: {depth: 2}, checks: [unit]"},
		"approvals_unless and no approval":  {rule: "approvals_unless: [comment_only]", warns: "approvals_unless"},
		"approvals_unless and an approval":  {rule: "approvals: one, approvals_unless: [comment_only]"},
	} {
		c, err := config.ParseCompany([]byte(codeRepos("    - name: web\n      owner: {seat: swe}\n" + tc.beside +
			"      protect:\n        - {branch: main, " + tc.rule + "}\n")))
		if err != nil {
			t.Fatalf("%s: refused: %v", name, err)
		}
		var found []config.Warning
		for _, w := range c.Warnings() {
			if strings.HasPrefix(w.Path, "code.") {
				found = append(found, w)
			}
		}
		switch {
		case tc.warns == "" && len(found) != 0:
			t.Errorf("%s: warned %+v, want nothing", name, found)
		case tc.warns == "":
		case len(found) != 1 || found[0].Path != "code.repos[0].protect[0]."+tc.warns ||
			found[0].Kind != config.WarningAdvisory || !strings.Contains(found[0].Message, tc.warns):
			t.Errorf("%s: warned %+v, want one advisory at code.repos[0].protect[0].%s", name, found, tc.warns)
		}
	}
}

// A QUEUE'S CAPACITY IS ITS ROUNDS OVER A SPAN (M5-D28): ⌊depth × batch ×
// over / round⌋, exact — PLAN's 576 and 1 152 a day at depths 4 and 8 with
// ten-minute checks and one change a candidate, a product floored once rather
// than a count of whole rounds (three candidates of seven minutes land 25 an
// hour, not 24), a round of no length unbounded, a product past the int range
// its top, and a queue of no depth, no batch or no span nothing.
//
// Mutation: floor over/round before multiplying → 24, red; return 0 for a
// round of no length → red; drop the overflow guard → Div64 panics, red.
func TestAQueuesCapacityIsItsRoundsOverASpan(t *testing.T) {
	t.Parallel()
	day := 24 * time.Hour
	for _, tc := range []struct {
		depth, batch int
		round, over  time.Duration
		want         int
	}{
		{4, 1, 10 * time.Minute, day, 576},
		{8, 1, 10 * time.Minute, day, 1_152},
		{4, 2, 10 * time.Minute, time.Hour, 48},
		{3, 1, 7 * time.Minute, time.Hour, 25},
		{1, 1, 6 * time.Hour, day, 4},
		{1, 1, 0, time.Hour, math.MaxInt},
		{8, 8, time.Nanosecond, math.MaxInt64, math.MaxInt},
		{0, 4, time.Minute, time.Hour, 0},
		{4, 0, time.Minute, time.Hour, 0},
		{4, 4, time.Minute, 0, 0},
	} {
		if got := config.LandingCapacity(tc.depth, tc.batch, tc.round, tc.over); got != tc.want {
			t.Errorf("LandingCapacity(%d, %d, %s, %s) = %d, want %d", tc.depth, tc.batch, tc.round, tc.over,
				got, tc.want)
		}
	}
}

// VALIDATE WARNS A QUEUE SLOWER THAN ITS WRITERS (M5-D28): a merge queue
// that cannot land one change a day for every seat that may merge into its
// branch is advised at the rule's landing, from the configured depth, batch
// and code.checks.max_timeout_seconds — `crewlet validate` observes no rate.
// Who may merge is counted through the chart: every human seat, and each
// full_access agent seat that writes the repository — the owning unit's lead
// and every seat of its subtree, a child unit's included, never a review seat
// nor one outside it — under the stricter of the rule's merge_by and
// code.merge_by_floor. Here five may merge, against a queue of one candidate
// of one change at six-hour rounds, four a day: warned; a queue of two
// candidates, the fifty-minute default rounds, merge_by maintainers or a
// maintainers floor (two may merge) and merge_by persons (the founder alone)
// are each silent.
//
// Mutation: count the unit's own seats and not its subtree's → four, silent,
// red; count a review seat → six named, red; read the rule's merge_by and not
// the floor → the floor's case warns, red; count the agent seats under
// merge_by persons → the persons case warns, red.
func TestValidateWarnsAQueueSlowerThanItsWriters(t *testing.T) {
	t.Parallel()
	const chart = `name: Acme
roles:
  - {name: Founder, handle: founder, kind: human}
units:
  - name: Engineering
    id: eng
    lead: lead
    roles:
      - {name: Lead, handle: lead, code: {access: full_access}}
      - {name: Dev A, handle: dev-a, code: {access: full_access}}
      - {name: Dev B, handle: dev-b, code: {access: review}}
    children:
      - name: Web
        id: web-team
        roles:
          - {name: Dev C, handle: dev-c, code: {access: full_access}}
          - {name: Dev D, handle: dev-d, code: {access: full_access}}
  - name: Ops
    id: ops
    roles:
      - {name: Ops, handle: ops, code: {access: full_access}}
code:
  native: true
`
	const slow = "{branch: main, checks: [unit], landing: {depth: 1, batch: 1}"
	for name, tc := range map[string]struct {
		code, rule string
		warns      bool
	}{
		"slower than its writers":   {code: "  checks: {max_timeout_seconds: 21600}\n", rule: slow + "}", warns: true},
		"two candidates a round":    {code: "  checks: {max_timeout_seconds: 21600}\n", rule: "{branch: main, checks: [unit], landing: {depth: 2, batch: 1}}"},
		"the default rounds":        {rule: slow + "}"},
		"merged by its maintainers": {code: "  checks: {max_timeout_seconds: 21600}\n", rule: slow + ", merge_by: maintainers}"},
		"merged by persons alone":   {code: "  checks: {max_timeout_seconds: 21600}\n", rule: slow + ", merge_by: persons}"},
		"a maintainers floor":       {code: "  checks: {max_timeout_seconds: 21600}\n  merge_by_floor: maintainers\n", rule: slow + "}"},
	} {
		doc := chart + tc.code + "  repos:\n    - name: web\n      owner: {unit: eng}\n      protect:\n        - " +
			tc.rule + "\n"
		c, err := config.ParseCompany([]byte(doc))
		if err != nil {
			t.Fatalf("%s: refused: %v", name, err)
		}
		var found []config.Warning
		for _, w := range c.Warnings() {
			if strings.Contains(w.Message, "that may merge") {
				found = append(found, w)
			}
		}
		switch {
		case !tc.warns && len(found) != 0:
			t.Errorf("%s: warned %+v, want nothing", name, found)
		case !tc.warns:
		case len(found) != 1 || found[0].Path != "code.repos[0].protect[0].landing" ||
			found[0].Kind != config.WarningAdvisory || !strings.Contains(found[0].Message, "at most 4 change(s)") ||
			!strings.Contains(found[0].Message, "the 5 seat(s) that may merge"):
			t.Errorf("%s: warned %+v, want one advisory at code.repos[0].protect[0].landing naming 4 a day "+
				"against 5 seats", name, found)
		}
	}
}

// THE SCHEMA ADMITS THE LANDING THE VALIDATOR DOES: the js tags are a second
// spelling of the queue's bounds.
func TestTheCodeLandingIsTheSchemasBounds(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		field    string
		min, max int64
	}{
		{"Depth", 1, config.CodeMaxQueueDepth},
		{"Batch", 1, config.CodeMaxQueueBatch},
	} {
		field, ok := reflect.TypeFor[config.CodeLanding]().FieldByName(tc.field)
		if !ok {
			t.Fatalf("config.CodeLanding has no field %s", tc.field)
		}
		if want := fmt.Sprintf("min=%d;max=%d", tc.min, tc.max); field.Tag.Get("js") != want {
			t.Errorf("%s's schema says %q and validation %q", tc.field, field.Tag.Get("js"), want)
		}
	}
}

// ONE RULE NAMES A CHECK: a lowercase letter or digit, then up to 63 more of
// them, '.', '_' and '-' — no capital, no space, nothing a shell splits.
func TestACheckIsNamedByOneRule(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]bool{
		"unit": true, "a": true, "0": true, "e2e": true, "lint.go": true, "build_linux-arm64": true,
		strings.Repeat("a", config.CodeMaxCheckNameBytes): true,
		"": false, "Unit": false, "-unit": false, ".unit": false, "_unit": false, "unit tests": false,
		"unit/fast": false, "ünit": false, "unit:fast": false, strings.Repeat("a", config.CodeMaxCheckNameBytes+1): false,
	} {
		if got := config.ValidCodeCheckName(name); got != want {
			t.Errorf("ValidCodeCheckName(%q) = %v, want %v", name, got, want)
		}
	}
}

// A WHOLE DECLARATION IS ACCEPTED: an owner by a unit's NAME, one by a unit's
// id, one by a seat, a branch glob, a frozen branch and a tag rule, a merge
// gate setting every field and a merge floor, and path owners naming units by
// name and by id and a seat — and every repository's default branch protected
// and every gate field doing something, so nothing is warned either.
func TestADeclaredCodeBlockIsAccepted(t *testing.T) {
	t.Parallel()
	c, err := config.ParseCompany([]byte(codeChart + `code:
  native: true
  deleted_retention_days: 30
  merge_by_floor: maintainers
  repos:
    - name: web
      owner: {unit: Engineering}
      description: The customer-facing web app.
      protect:
        - branch: main
          merge_by: persons
          approvals: one
          approvals_unless: [whitespace_only, comment_only]
          human_paths:
            - glob: .crewlet/**
            - {glob: "deploy/**", unless: [comment_only]}
          checks: [unit, lint, e2e.linux]
          landing: {depth: 8, batch: 1}
          owners_approve: true
          delete_source: none
          agents_edit_checks: true
        - branch: release/*
          direct_push: none
        - tag: "v*"
          allow_delete: true
      path_owners:
        - {paths: ["web/**", "**/*.go"], unit: Engineering}
        - {paths: ["Makefile", ".github/workflows/*.yml"], seat: swe}
        - {paths: ["deploy/**"], unit: Platform Team}
    - name: infra
      owner: {unit: platform}
      default_branch: trunk
      archived: true
      protect:
        - {branch: trunk, allow_force: true}
      path_owners:
        - {paths: ["**"], unit: platform}
    - name: notes
      owner: {seat: ceo}
      protect:
        - branch: "**"
`))
	if err != nil {
		t.Fatalf("refused: %v", err)
	}
	if got := c.Code.DeletedRetention(); got != 30*24*time.Hour {
		t.Errorf("deleted_retention_days: 30 reads as %v", got)
	}
	for _, w := range c.Warnings() {
		if strings.HasPrefix(w.Path, "code.") {
			t.Errorf("a declaration protecting every default branch warned: %s: %s", w.Path, w.Message)
		}
	}
}

// A REPOSITORY'S MIRRORS AND ITS IMPORT ARE ACCEPTED as declared, each
// credential kept as the ${VAR} reference it was written as — Tier B is never
// resolved at its parse, and the code log records the reference — and a push
// mirror's seat accepted where the chart lets it read the repository. A pull
// mirror's protect list is warned, at it, since a mirror takes no push for a
// rule to hold.
//
// Mutation: drop the pull mirror's arm from codeWarnings — nothing is
// warned, red.
func TestARepositorysMirrorsAreAccepted(t *testing.T) {
	t.Parallel()
	c, err := config.ParseCompany([]byte(codeRepos(`    - name: web
      owner: {unit: Engineering}
      protect: [{branch: main}]
      mirror_to:
        url: https://forge.example.com/acme/web.git
        credential: ${FORGE_TOKEN}
        as: swe
        refs: [refs/heads/main, 'refs/tags/v*']
    - name: api
      owner: {unit: Engineering}
      protect: [{branch: main}]
      import: {url: 'https://forge.example.com/acme/api.git', credential: '${FORGE_TOKEN}'}
    - name: upstream
      owner: {unit: Engineering}
      protect: [{branch: main}]
      mirror_from: {url: 'https://forge.example.com/upstream/lib.git'}
`)))
	if err != nil {
		t.Fatalf("refused: %v", err)
	}
	repos := c.Code.Repos
	if got := repos[0].MirrorTo.Credential; got != "${FORGE_TOKEN}" {
		t.Errorf("mirror_to.credential reads %q, want the reference as written", got)
	}
	if got := repos[1].Import.Credential; got != "${FORGE_TOKEN}" {
		t.Errorf("import.credential reads %q, want the reference as written", got)
	}
	var warned []config.Warning
	for _, w := range c.Warnings() {
		if strings.HasPrefix(w.Path, "code.") {
			warned = append(warned, w)
		}
	}
	if len(warned) != 1 || warned[0].Path != "code.repos[2].protect" ||
		!strings.Contains(warned[0].Message, "pull mirror") {
		t.Errorf("warned %+v, want the one at code.repos[2].protect naming the pull mirror", warned)
	}
}

// AN UNPROTECTED DEFAULT BRANCH IS WARNED, NEVER REFUSED — through the matcher
// a push's decide asks (gitref), so a glob that covers the branch protects it
// and one beside it does not. The warning sits at the field to add, the
// repository's protect list, names the branch it is about, and names the
// fields a rule gates what merges into it with.
func TestAnUnprotectedDefaultBranchIsWarned(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		repo   string
		warns  bool
		branch string
	}{
		"a glob beside it":           {repo: "      protect: [{branch: 'release/*'}]\n", warns: true, branch: "main"},
		"no rules at all":            {repo: "", warns: true, branch: "main"},
		"a tag of the same name":     {repo: "      protect: [{tag: main}]\n", warns: true, branch: "main"},
		"a rule on another branch":   {repo: "      default_branch: trunk\n      protect: [{branch: main}]\n", warns: true, branch: "trunk"},
		"the branch itself":          {repo: "      protect: [{branch: main}]\n"},
		"a glob covering every name": {repo: "      protect: [{branch: '**'}]\n"},
		"a glob covering its own":    {repo: "      default_branch: dev/main\n      protect: [{branch: 'dev/*'}]\n"},
		// AN ARCHIVED REPOSITORY TAKES NO PUSH AT ALL — the decide refuses
		// the whole push — so nobody can force-push or delete its branch,
		// and a warning saying so would be false.
		"archived, with no rules": {repo: "      archived: true\n"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c, err := config.ParseCompany([]byte(codeRepos(
				"    - name: web\n      owner: {seat: swe}\n" + tc.repo)))
			if err != nil {
				t.Fatalf("refused: %v", err)
			}
			var found []config.Warning
			for _, w := range c.Warnings() {
				if strings.HasPrefix(w.Path, "code.") {
					found = append(found, w)
				}
			}
			switch {
			case !tc.warns && len(found) != 0:
				t.Fatalf("a default branch nobody can force-push or delete warned: %+v", found)
			case !tc.warns:
			case len(found) != 1:
				t.Fatalf("warnings = %+v, want one about the unprotected default branch", found)
			case found[0].Path != "code.repos[0].protect" || found[0].Kind != config.WarningAdvisory:
				t.Errorf("warned at %s (%s), want code.repos[0].protect, an advisory",
					found[0].Path, found[0].Kind)
			case !strings.Contains(found[0].Message, "web's default branch "+tc.branch+" is unprotected"):
				t.Errorf("message = %q, want it to name web's default branch %s", found[0].Message, tc.branch)
			case !strings.Contains(found[0].Message, "merge gate") || !strings.Contains(found[0].Message, "checks"):
				t.Errorf("message = %q, want it to name the merge gate's fields a protect rule gates merges "+
					"with", found[0].Message)
			}
		})
	}
}

// A DEFAULT BRANCH VALIDATION REFUSES IS NEVER ALSO WARNED ABOUT: the refusal is
// the line to read, and a warning that a branch no push can ever create is
// unprotected would send a person to protect it. Read off the decoded document
// with its validation's refusal beside it, as `crewlet validate` prints both.
func TestARefusedDefaultBranchIsNotWarned(t *testing.T) {
	t.Parallel()
	for _, branch := range []string{"HEAD", "-x", "@", "seats", "seats/main"} {
		c, err := config.ParseCompanyDocument([]byte(codeRepos(
			"    - name: web\n      owner: {seat: swe}\n      default_branch: '" + branch + "'\n")))
		if err != nil {
			t.Fatalf("%s: not decoded: %v", branch, err)
		}
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "code.repos[0].default_branch") {
			t.Errorf("%s: validation answered %v, want a refusal of the field", branch, err)
		}
		for _, w := range c.Warnings() {
			if strings.HasPrefix(w.Path, "code.") {
				t.Errorf("%s: a default branch validation refuses warned: %s: %s", branch, w.Path, w.Message)
			}
		}
	}
}

// THE SCHEMA ADMITS THE RETENTION THE VALIDATOR DOES: 0 is unset, which the
// decoder reads like an absent key, and a year is the most.
func TestTheDeletedRetentionIsTheSchemasBounds(t *testing.T) {
	t.Parallel()
	field, ok := reflect.TypeFor[config.Code]().FieldByName("DeletedRetentionDays")
	if !ok {
		t.Fatal("config.Code has no DeletedRetentionDays")
	}
	if got, want := field.Tag.Get("js"),
		fmt.Sprintf("min=0;max=%d", config.CodeMaxDeletedRetentionDays); got != want {
		t.Errorf("deleted_retention_days' schema bounds are %q, want %q — the tag was left "+
			"behind when the constant moved", got, want)
	}
}

// THE IDENTITY DOMAIN IS A LOWERCASE DNS NAME every address spelled in it fits:
// a name the engine's signer writes into every commit it builds, as written —
// so one in capitals would be a second spelling of one committer, and one past
// 189 bytes an address past RFC 5321's 254 for a seat's longest handle. The
// schema's pattern and length admit exactly what validation admits, row by
// row; unset is seats.invalid, and a domain with nothing hosting code is
// refused as the code block's other settings are.
//
// Mutations: drop the hyphen-at-an-end rule — "-a.example" accepted, red;
// widen the bound to 253 — the 190-byte row accepted, red.
func TestTheIdentityDomainIsADNSName(t *testing.T) {
	t.Parallel()
	field, ok := reflect.TypeFor[config.Code]().FieldByName("IdentityDomain")
	if !ok {
		t.Fatal("config.Code has no IdentityDomain")
	}
	tags := map[string]string{}
	for _, part := range strings.Split(field.Tag.Get("js"), ";") {
		key, value, _ := strings.Cut(part, "=")
		tags[key] = value
	}
	pattern, err := regexp.Compile(tags["pattern"])
	if err != nil {
		t.Fatalf("identity_domain's schema pattern %q: %v", tags["pattern"], err)
	}
	if got := tags["maxlen"]; got != strconv.Itoa(config.CodeMaxIdentityDomainBytes) {
		t.Errorf("identity_domain's schema length is %s, validation's %d", got, config.CodeMaxIdentityDomainBytes)
	}
	label := strings.Repeat("a", 63)
	long := strings.Repeat(label+".", 2) + strings.Repeat("b", 189-2*64) // 189 bytes
	for _, tc := range []struct {
		domain string
		ok     bool
	}{
		{"example.com", true}, {"seats.invalid", true}, {"localhost", true}, {"a-b.c0.example", true},
		{"123.example", true}, {label + ".example", true}, {long, true},
		{long + "b", false}, {strings.Repeat("a", 64) + ".example", false}, {"Example.com", false},
		{"-a.example", false}, {"a-.example", false}, {"a..example", false}, {".example", false},
		{"example.", false}, {"ex ample.com", false}, {"ex_ample.com", false}, {"bücher.example", false},
		{"a@b.example", false},
	} {
		doc := fmt.Sprintf("name: Acme\ncode:\n  native: true\n  identity_domain: %q\n", tc.domain)
		_, err := config.ParseCompany([]byte(doc))
		problems := config.Problems(err)
		switch {
		case tc.ok && err != nil:
			t.Errorf("%q refused: %v", tc.domain, err)
		case !tc.ok && (len(problems) != 1 || problems[0].Path != "code.identity_domain" ||
			problems[0].Kind != "shape"):
			t.Errorf("%q: problems %+v, want one shape problem at code.identity_domain", tc.domain, problems)
		}
		schema := pattern.MatchString(tc.domain) && len(tc.domain) <= config.CodeMaxIdentityDomainBytes
		if schema != tc.ok {
			t.Errorf("%q: the schema admits it %t, validation %t — the two spellings of the rule drifted",
				tc.domain, schema, tc.ok)
		}
	}
	_, err = config.ParseCompany([]byte("name: Acme\ncode:\n  identity_domain: example.com\n"))
	if problems := config.Problems(err); len(problems) != 1 || problems[0].Path != "code.identity_domain" ||
		problems[0].Kind != "conflict" || !strings.Contains(problems[0].Message, "code.native") {
		t.Errorf("an identity domain with nothing hosting code: problems %+v, want one conflict naming "+
			"code.native", problems)
	}
	if config.DefaultCodeIdentityDomain != "seats.invalid" {
		t.Errorf("the default identity domain is %q, want seats.invalid: a name under RFC 2606's .invalid, "+
			"which nobody can own", config.DefaultCodeIdentityDomain)
	}
	if got := (config.Code{}).IdentityDomainResolved(); got != config.DefaultCodeIdentityDomain {
		t.Errorf("an unset identity domain resolves to %q, want the default %q", got,
			config.DefaultCodeIdentityDomain)
	}
	if got := (config.Code{IdentityDomain: "example.com"}).IdentityDomainResolved(); got != "example.com" {
		t.Errorf("a set identity domain resolves to %q", got)
	}
}

// THE RETENTION'S DEFAULT IS APPLIED IN ONE PLACE: unset is the default seven
// days, a set value passes through.
func TestTheDeletedRetentionResolvesToItsDefault(t *testing.T) {
	t.Parallel()
	if got := (config.Code{}).DeletedRetention(); got != config.DefaultCodeDeletedRetentionDays*24*time.Hour {
		t.Errorf("an unset retention is %v, want the default %d days", got,
			config.DefaultCodeDeletedRetentionDays)
	}
	if got := (config.Code{DeletedRetentionDays: 1}).DeletedRetention(); got != 24*time.Hour {
		t.Errorf("a retention of one day is %v", got)
	}
}

// A DESCRIPTION THAT IS NOT UTF-8 IS REFUSED. YAML cannot carry one, but a
// document reaches validation as a value too — a PATCH merged in Go — and the
// description rides every settings record, which the domain refuses then.
func TestADescriptionThatIsNotUTF8IsRefused(t *testing.T) {
	t.Parallel()
	c, err := config.ParseCompany([]byte(codeRepos("    - name: web\n      owner: {seat: swe}\n")))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	c.Code.Repos[0].Description = "caf\xe9"
	problems := config.Problems(c.Validate())
	if len(problems) != 1 || problems[0].Path != "code.repos[0].description" || problems[0].Kind != "shape" {
		t.Fatalf("problems = %+v, want one shape problem at code.repos[0].description", problems)
	}
}

// THE CODE HOST'S POLICY BOUNDS ARE ADMISSION RULES: a document submitted with
// more than CodeMaxRepos repositories, or a retention past
// CodeMaxDeletedRetentionDays, is refused at the field — and a revision being
// APPLIED that breaks either still runs.
//
// Nothing about running depends on either: the reconcile decides any number of
// declarations, and the retention duty purges by any retention a duration can
// hold. So a later build that raises either bound admits revisions this build
// must apply during a rolling upgrade, or the fleet's epoch splits — which is
// what holding them as runnable rules would do (config's ValidateRunnable).
//
// Mutation: put either check back in Code.validate and ValidateRunnable
// refuses the stored revision.
func TestTheCodeHostsPolicyBoundsAreAdmissionRules(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	for i := range config.CodeMaxRepos + 1 {
		fmt.Fprintf(&b, "    - {name: r%d, owner: {seat: swe}, protect: [{branch: main}]}\n", i)
	}
	doc := strings.Replace(codeRepos(b.String()), "  native: true\n",
		fmt.Sprintf("  native: true\n  deleted_retention_days: %d\n", config.CodeMaxDeletedRetentionDays+1), 1)
	c, err := config.ParseCompanyDocument([]byte(doc))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := c.ValidateRunnable(); err != nil {
		t.Fatalf("ValidateRunnable() = %v: an applied revision past a policy bound still runs, "+
			"so nothing may refuse to apply it", err)
	}
	want := map[string]string{
		"code.repos":                  strconv.Itoa(config.CodeMaxRepos),
		"code.deleted_retention_days": strconv.Itoa(config.CodeMaxDeletedRetentionDays),
	}
	for _, check := range []struct {
		name string
		err  error
	}{{"Validate", c.Validate()}, {"ValidateAdmission", c.ValidateAdmission()}} {
		problems := config.Problems(check.err)
		if len(problems) != len(want) {
			t.Errorf("%s() = %+v, want one problem each at %v", check.name, problems, want)
			continue
		}
		for _, p := range problems {
			names, ok := want[p.Path]
			if !ok || p.Kind != "out_of_range" || !strings.Contains(p.Message, names) {
				t.Errorf("%s(): %s (%s) %q, want out_of_range at one of %v naming its bound",
					check.name, p.Path, p.Kind, p.Message, want)
			}
		}
	}
}

// BenchmarkValidatingTheMostRepositories is what an apply pays for code.repos'
// runnable rules at their largest: [config.CodeMaxRepos] repositories, half
// owned by a unit (by name) and half by a seat, each with a rule, on a chart
// of 300 units and 900 seats whose handles are derived from their names —
// ValidateRunnable over the whole document, the check every data node runs on
// every revision it applies, inside the apply. Measured (four vCPUs of a
// 2.1 GHz Xeon) at 95–101 ms and 29 MB a validation with the owners resolved
// through one org.Index, against 1.92–1.93 s and 338 MB through the chart's
// one-off lookups — a walk of every seat, deriving each handle, an owner.
func BenchmarkValidatingTheMostRepositories(b *testing.B) {
	var doc strings.Builder
	doc.WriteString("name: Acme\nunits:\n")
	for u := range 300 {
		fmt.Fprintf(&doc, "  - name: Unit %d\n    roles:\n", u)
		for s := range 3 {
			fmt.Fprintf(&doc, "      - {name: Seat %d %d}\n", u, s)
		}
	}
	doc.WriteString("code:\n  native: true\n  repos:\n")
	for i := range config.CodeMaxRepos {
		owner := fmt.Sprintf("{unit: Unit %d}", i%300)
		if i%2 == 1 {
			owner = fmt.Sprintf("{seat: seat-%d-%d}", i%300, i%3)
		}
		fmt.Fprintf(&doc, "    - {name: r%d, owner: %s, protect: [{branch: main}]}\n", i, owner)
	}
	c, err := config.ParseCompanyDocument([]byte(doc.String()))
	if err != nil {
		b.Fatalf("decode: %v", err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if err := c.ValidateRunnable(); err != nil {
			b.Fatalf("ValidateRunnable() = %v", err)
		}
	}
}

// BenchmarkWarningAboutTheMostUnitRepositories is what the advisory warnings
// cost a document at the largest code.repos admits — [config.CodeMaxRepos]
// repositories, each owned by one of 300 teams of three seats that name no
// lead and inherit their division's (30 divisions led by a seat of their own:
// 330 units and 930 seats) — once with every repository `visibility: unit`,
// which asks each one's owning unit for its effective lead, and once with the
// visibility unset, which asks nothing. It is paid on every PATCH /config
// answer and every `crewlet validate`.
func BenchmarkWarningAboutTheMostUnitRepositories(b *testing.B) {
	for _, visibility := range []string{"unit", ""} {
		var doc strings.Builder
		doc.WriteString(ledChartDoc(30))
		doc.WriteString("code:\n  native: true\n  repos:\n")
		field := ""
		if visibility != "" {
			field = ", visibility: " + visibility
		}
		for i := range config.CodeMaxRepos {
			fmt.Fprintf(&doc, "    - {name: r%d, owner: {unit: Team %d %d}, protect: [{branch: main}]%s}\n",
				i, i%30, (i/30)%10, field)
		}
		c, err := config.ParseCompanyDocument([]byte(doc.String()))
		if err != nil {
			b.Fatalf("decode: %v", err)
		}
		name := "visibility=" + cmp.Or(visibility, "unset")
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				for _, w := range c.AdvisoryWarnings() {
					if strings.Contains(w.Message, "has no lead") {
						b.Fatalf("warned of a unit with a lead: %s", w.Message)
					}
				}
			}
		})
	}
}

// A RETENTION NO DURATION HOLDS IS REFUSED TO RUN, however it was admitted:
// one day past the longest a duration holds, DeletedRetention overflows to a
// negative duration — a retention every deleted repository has outlived, which
// the retention duty would answer by purging them all at its next pass, and a
// purge has no undo. The longest a duration holds still runs, as the policy
// bound's admission rule allows a later build to raise it that far.
//
// Mutation: drop the runnable bound and the overflowing revision applies.
func TestARetentionNoDurationHoldsIsRefusedToRun(t *testing.T) {
	t.Parallel()
	held := int(math.MaxInt64 / int64(24*time.Hour))
	for _, tc := range []struct {
		days   int
		refuse bool
	}{{held, false}, {held + 1, true}} {
		doc := codeRepos("    - {name: web, owner: {seat: swe}, protect: [{branch: main}]}\n")
		doc = strings.Replace(doc, "  native: true\n",
			fmt.Sprintf("  native: true\n  deleted_retention_days: %d\n", tc.days), 1)
		c, err := config.ParseCompanyDocument([]byte(doc))
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		problems := config.Problems(c.ValidateRunnable())
		switch {
		case !tc.refuse && len(problems) != 0:
			t.Errorf("%d days: ValidateRunnable() = %+v, want it to run", tc.days, problems)
		case !tc.refuse && c.Code.DeletedRetention() <= 0:
			t.Errorf("%d days: DeletedRetention() = %v, want the duration it says", tc.days,
				c.Code.DeletedRetention())
		case tc.refuse && (len(problems) != 1 || problems[0].Path != "code.deleted_retention_days" ||
			problems[0].Kind != "out_of_range"):
			t.Errorf("%d days: ValidateRunnable() = %+v, want one out_of_range at "+
				"code.deleted_retention_days", tc.days, problems)
		}
		if tc.refuse {
			// AND ONCE: the admission rule leaves what the runnable
			// rule refuses to it.
			if all := config.Problems(c.Validate()); len(all) != 1 {
				t.Errorf("%d days: Validate() = %+v, want the one problem", tc.days, all)
			}
		}
	}
}

// A RULE'S PATTERN IS ONE SPELLING, its namespace before its name, and none
// for a rule naming neither or both — which the validator refuses, so the
// reconcile never meets it.
func TestAProtectionsPatternIsItsNamespacedName(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		rule config.CodeProtection
		want string
	}{
		{config.CodeProtection{Branch: "release/*"}, "refs/heads/release/*"},
		{config.CodeProtection{Tag: "v*"}, "refs/tags/v*"},
		{config.CodeProtection{}, ""},
		{config.CodeProtection{Branch: "main", Tag: "v1"}, ""},
	} {
		if got := tc.rule.Pattern(); got != tc.want {
			t.Errorf("%+v.Pattern() = %q, want %q", tc.rule, got, tc.want)
		}
	}
	if config.CodeDirectPush("").Resolved() != config.CodeDirectPersons ||
		config.CodeDirectNone.Resolved() != config.CodeDirectNone {
		t.Error("an unset direct_push does not read as persons, or a set one moved")
	}
}

// A VISIBILITY THIS BUILD DOES NOT KNOW IS REFUSED at the field, naming the
// closed set, as one problem: read as `company` it would hand every seat what
// its author meant to narrow, and read as `unit` it would shut seats out that
// the author meant to let in — so it is neither. Unset reads as company, and
// each known value as itself.
func TestAnUnknownVisibilityIsRefused(t *testing.T) {
	t.Parallel()
	web := "    - name: web\n      owner: {unit: Engineering}\n"
	for _, v := range []string{"team", "Unit", "private", " company"} {
		_, err := config.ParseCompany([]byte(codeRepos(web + fmt.Sprintf("      visibility: %q\n", v))))
		problems := config.Problems(err)
		if len(problems) != 1 || problems[0].Path != "code.repos[0].visibility" ||
			problems[0].Kind != "unknown_value" || !strings.Contains(problems[0].Message, "company, unit") {
			t.Errorf("visibility %q: problems = %+v, want one unknown value at code.repos[0].visibility "+
				"naming company, unit", v, problems)
		}
	}
	for written, want := range map[string]config.CodeVisibility{
		"":                            config.CodeVisibilityCompany,
		"      visibility: company\n": config.CodeVisibilityCompany,
		"      visibility: unit\n":    config.CodeVisibilityUnit,
	} {
		c, err := config.ParseCompany([]byte(codeRepos(web + written)))
		if err != nil {
			t.Errorf("%q: refused: %v", written, err)
			continue
		}
		if got := c.Code.Repos[0].Visibility.Resolved(); got != want {
			t.Errorf("%q resolves to %q, want %q", written, got, want)
		}
	}
}

// A SEARCH MODE THIS BUILD DOES NOT KNOW IS REFUSED at the field, naming the
// closed set, as one problem: read as `indexed` it would have every git node
// keeping an index build one its author did not ask for, and read as `grep` it
// would leave a repository out of every search across the company's that its
// author meant to be in. Unset reads as indexed — a declared repository is a
// project the company named — and each known value as itself.
//
// Mutation: resolve unset to grep — the unset row reads grep, red.
func TestAnUnknownSearchModeIsRefused(t *testing.T) {
	t.Parallel()
	web := "    - name: web\n      owner: {unit: Engineering}\n"
	for _, v := range []string{"index", "Grep", "full", " indexed"} {
		_, err := config.ParseCompany([]byte(codeRepos(web + fmt.Sprintf("      search: %q\n", v))))
		problems := config.Problems(err)
		if len(problems) != 1 || problems[0].Path != "code.repos[0].search" ||
			problems[0].Kind != "unknown_value" || !strings.Contains(problems[0].Message, "indexed, grep") {
			t.Errorf("search %q: problems = %+v, want one unknown value at code.repos[0].search naming "+
				"indexed, grep", v, problems)
		}
	}
	for written, want := range map[string]config.CodeSearchMode{
		"":                        config.CodeSearchIndexed,
		"      search: indexed\n": config.CodeSearchIndexed,
		"      search: grep\n":    config.CodeSearchGrep,
	} {
		c, err := config.ParseCompany([]byte(codeRepos(web + written)))
		if err != nil {
			t.Errorf("%q: refused: %v", written, err)
			continue
		}
		if got := c.Code.Repos[0].Search.Resolved(); got != want {
			t.Errorf("%q resolves to %q, want %q", written, got, want)
		}
	}
}

// A `visibility: unit` REPOSITORY OWNED BY A UNIT WITH NO LEAD IS WARNED,
// NEVER REFUSED: valid, and read by no lead's seat, because there is none — so
// only persons and the unit's own members read it, which is rarely what
// narrowing it meant. The lead is the EFFECTIVE one, so a lead inherited from
// a parent unit reads the repository and nothing is warned; the warning sits
// at the repository's visibility and names the repository and the unit as its
// author wrote it.
//
// Mutation: the declared lead read instead of the effective one → the child
// unit under a led parent warns.
func TestAUnitRepositoryWithNoLeadWarns(t *testing.T) {
	t.Parallel()
	const chart = `name: Acme
roles:
  - name: CEO
    handle: ceo
units:
  - name: Engineering
    lead: cto
    roles:
      - name: CTO
        handle: cto
    children:
      - name: Platform
        roles:
          - name: SRE
            handle: sre
  - name: Sales
    roles:
      - name: Rep
        handle: rep
`
	for name, tc := range map[string]struct {
		repo  string
		warns bool
	}{
		"a unit repository of a unit with no lead": {repo: "owner: {unit: Sales}\n      visibility: unit", warns: true},
		"named by the unit's other spelling":       {repo: "owner: {unit: sales}\n      visibility: unit", warns: true},
		"an archived one, which is still read": {
			repo: "owner: {unit: Sales}\n      visibility: unit\n      archived: true", warns: true},
		"a unit repository of a led unit":                {repo: "owner: {unit: Engineering}\n      visibility: unit"},
		"one of a unit that inherits its lead":           {repo: "owner: {unit: Platform}\n      visibility: unit"},
		"a company repository of a unit with no lead":    {repo: "owner: {unit: Sales}"},
		"a unit repository a seat owns":                  {repo: "owner: {seat: rep}\n      visibility: unit"},
		"an owner the chart does not have, refused only": {repo: "owner: {unit: Marketing}\n      visibility: unit"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c, err := config.ParseCompanyDocument([]byte(chart + "code:\n  native: true\n  repos:\n" +
				"    - name: web\n      " + tc.repo + "\n      protect: [{branch: main}]\n"))
			if err != nil {
				t.Fatalf("not decoded: %v", err)
			}
			var found []config.Warning
			for _, w := range c.Warnings() {
				if strings.HasPrefix(w.Path, "code.") {
					found = append(found, w)
				}
			}
			switch {
			case !tc.warns && len(found) != 0:
				t.Fatalf("warned: %+v", found)
			case !tc.warns:
			case len(found) != 1:
				t.Fatalf("warnings = %+v, want one about the unit with no lead", found)
			case found[0].Path != "code.repos[0].visibility" || found[0].Kind != config.WarningAdvisory:
				t.Errorf("warned at %s (%s), want code.repos[0].visibility, an advisory",
					found[0].Path, found[0].Kind)
			case !strings.Contains(found[0].Message, "web is visibility: unit") ||
				!strings.Contains(found[0].Message, "which has no lead") || found[0].Unit != "Sales":
				t.Errorf("warning = %+v, want it to name web and its unit Sales", found[0])
			}
		})
	}
}

// A CODE ACCESS TIER NO CODE HOST READS IS WARNED, NEVER REFUSED: on a company
// that hosts no code of its own, a seat has a host that caps anything by its
// tier only where it has a GitHub App of its own AND the company's GitHub
// integration is enabled — the only place the engine reconciles that app and
// mints its tokens — so anywhere else `code.access` reads as a grant and
// grants nothing, and the warning says which of the two is missing. Every
// tier, read_only included; not where the company sets code.native or the seat
// has an app the enabled integration reads; and not on a human seat, whose
// tier is an admission refusal of its own, nor for a tier validation refuses.
//
// Mutation: read only the seat's own app (the company's integration ignored),
// and both rows without an enabled integration go silent.
func TestATierNoCodeHostReadsWarns(t *testing.T) {
	t.Parallel()
	const ownApp = "    code: {access: review}\n    integrations:\n      github: {repos: [acme/api]}\n"
	on := func(github, extra string) string {
		return "name: Acme\n" + github + "roles:\n  - name: Dev\n    handle: dev\n" + extra
	}
	seat := func(extra string) string {
		return on("integrations:\n  github: {enabled: true, webhook_secret: s}\n", extra)
	}
	for name, tc := range map[string]struct {
		doc   string
		warns string
		why   string
	}{
		"full_access on no host": {doc: seat("    code: {access: full_access}\n"), warns: "full_access",
			why: "no GitHub App of its own"},
		"read_only on no host": {doc: seat("    code: {access: read_only}\n"), warns: "read_only",
			why: "no GitHub App of its own"},
		"no tier at all":          {doc: seat("")},
		"a seat with its own app": {doc: seat(ownApp)},
		// THE SEAT'S APP IS READ ONLY WHERE THE COMPANY'S INTEGRATION IS:
		// the engine reconciles a seat's app and mints its tokens only
		// under an enabled integrations.github, so an app of its own on a
		// company without one is a tier nothing reads.
		"its own app, no github integration": {doc: on("", ownApp), warns: "review",
			why: "no GitHub integration enabled"},
		"its own app, the integration off": {doc: on("integrations:\n  github: {enabled: false}\n", ownApp),
			warns: "review", why: "no GitHub integration enabled"},
		"the native host": {doc: seat("    code: {access: full_access}\n") + "code:\n  native: true\n"},
		"a human seat, refused instead": {doc: "name: Acme\nroles:\n  - name: Founder\n    kind: human\n" +
			"    contact: {slack_user_id: U0FOUNDER}\n    code: {access: full_access}\n"},
		"a tier validation refuses": {doc: seat("    code: {access: admin}\n")},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c, err := config.ParseCompanyDocument([]byte(tc.doc))
			if err != nil {
				t.Fatalf("not decoded: %v", err)
			}
			var found []config.Warning
			for _, w := range c.Warnings() {
				if strings.HasSuffix(w.Path, "code.access") && w.Kind == config.WarningAdvisory {
					found = append(found, w)
				}
			}
			switch {
			case tc.warns == "" && len(found) != 0:
				t.Fatalf("warned: %+v", found)
			case tc.warns == "":
			case len(found) != 1:
				t.Fatalf("warnings = %+v, want one about a tier that grants nothing", found)
			case found[0].Path != "roles[0].code.access" || found[0].Seat != "dev" ||
				!strings.Contains(found[0].Message, tc.warns+" grants nothing") ||
				!strings.Contains(found[0].Message, tc.why):
				t.Errorf("warning = %+v, want dev's code.access %s, granting nothing: %s", found[0], tc.warns,
					tc.why)
			}
		})
	}
}
