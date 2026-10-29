package config

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// THE ENGINE'S GIT VARIABLES ARE RESERVED, in each of the three envs a coding
// run's environment is assembled from — a seat's sandbox block, a seat's own
// setup step and a provider-wide one — each refused at its own path, naming
// the variable: an operator's value for one would break the run's clones
// (the credential, the base) or make a box commit's trailers disagree with
// its push's record (the three the commit hook reads). The table spells every
// name literally, so a name dropped from ReservedSandboxEnv is a row that
// passes.
//
// AN ADMISSION RULE: a stored revision carrying one is held to the runnable
// rules alone, which take it — the engine's own value wins for the names it
// sets — so a build whose rule a newer peer relaxed never splits the fleet.
//
// Mutations: drop a place from validateReservedSandboxEnv → its rows pass,
// red; drop a name from ReservedSandboxEnv → its rows pass, red.
func TestTheEnginesGitVariablesAreReserved(t *testing.T) {
	t.Parallel()
	names := []string{"CREWLET_GIT_TOKEN", "CREWLET_GIT_URL", "CREWLET_GIT_TRAILER_SEAT",
		"CREWLET_GIT_TRAILER_RUN", "CREWLET_GIT_TRAILER_WORK_ITEM"}
	if got := ReservedSandboxEnv(); !slices.Equal(got, names) {
		t.Errorf("ReservedSandboxEnv() = %v, want %v", got, names)
	}
	places := []struct{ name, doc, path string }{
		{"a seat's sandbox env", `
name: Acme
providers:
  sandbox:
    fake: true
roles:
  - name: Dev
    handle: dev
    sandbox:
      enabled: true
      env: {%s: "x"}
`, "roles[0].sandbox.env.%s"},
		{"a seat's setup step", `
name: Acme
providers:
  sandbox:
    fake: true
roles:
  - name: Dev
    handle: dev
    sandbox:
      enabled: true
      setup:
        - name: tools
          env: {%s: "x"}
`, "roles[0].sandbox.setup[0].env.%s"},
		{"a provider-wide setup step", `
name: Acme
providers:
  sandbox:
    fake: true
    setup:
      - name: tools
        env: {%s: "x"}
`, "providers.sandbox.setup[0].env.%s"},
	}
	for _, name := range names {
		for _, place := range places {
			t.Run(name+" in "+place.name, func(t *testing.T) {
				t.Parallel()
				doc := strings.ReplaceAll(place.doc, "%s", name)
				err := rejects(t, doc, strings.ReplaceAll(place.path, "%s", name))
				if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), name+" is the engine's") {
					t.Errorf("want a conflict saying %s is the engine's; got:\n%v", name, err)
				}
				// RUNNABLE: what a node applies from a peer's revision.
				cfg, perr := ParseCompanyDocument([]byte(doc))
				if perr != nil {
					t.Fatalf("parse: %v", perr)
				}
				if err := cfg.ValidateRunnable(); err != nil {
					t.Errorf("a stored revision carrying %s is refused to run: %v", name, err)
				}
			})
		}
	}
	// A NAME OF THE OPERATOR'S OWN beside them is theirs.
	mustCompany(t, `
name: Acme
providers:
  sandbox:
    fake: true
roles:
  - name: Dev
    handle: dev
    sandbox:
      enabled: true
      env: {GITLAB_TOKEN: "x", CREWLET_GIT_PROXY: "y", GIT_CONFIG_COUNT: "0"}
`)
}

// directCompany is a company hosting code (or not) whose catalogue is the
// local backend, a seat placed by run (its own run_in, or the default's), and
// an agent-mode executor's entry where entryRunIn is not "-".
func directCompany(native bool, defaultRunIn, seatRunIn string) string {
	// THE IMAGE ONLY WHERE A CONTAINER BOX IS REACHED: a document naming one
	// nothing reads is refused.
	local := "    local: {}\n"
	if defaultRunIn == "container" || seatRunIn == "container" {
		local = "    local: {image: \"ghcr.io/example/coder:1\"}\n"
	}
	doc := "\nname: Acme\nproviders:\n  sandbox:\n" + local
	if defaultRunIn != "" {
		doc += "    default_run_in: " + defaultRunIn + "\n"
	}
	doc += `roles:
  - name: Dev
    handle: dev
    sandbox:
      enabled: true
`
	if seatRunIn != "" {
		doc += "      run_in: " + seatRunIn + "\n"
	}
	doc += `units:
  - name: Engineering
    id: eng
`
	if native {
		doc += `code:
  native: true
  repos:
    - name: web
      owner: {unit: Engineering}
      protect:
        - branch: main
`
	}
	return doc
}

// directWarnings is c's advisories about a `direct` box.
func directWarnings(c *Company) []Warning {
	var out []Warning
	for _, w := range c.AdvisoryWarnings() {
		if strings.Contains(w.Message, "a `direct` box runs as the engine's own user") {
			out = append(out, w)
		}
	}
	return out
}

// A DIRECT SEAT IN A COMPANY THAT HOSTS CODE IS WARNED, whatever its
// repositories' visibility: a `direct` box runs as the engine's user, so it
// reads every repository a git node caches and the keyring that mints any live
// run's token — at the seat's run_in where it names the cell, and once at the
// default for the seats that take it. No warning where the company hosts no
// code, nor for a `container` seat.
//
// Mutation: drop the code.native term → the company hosting no code warns,
// red.
func TestADirectSeatInACompanyThatHostsCodeIsWarned(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name               string
		native             bool
		defaultRun, seatIn string
		at                 string // "" is no warning
		seat               string
	}{
		{"a seat naming direct", true, "container", "direct", "roles[0].sandbox.run_in", "dev"},
		{"a seat inheriting direct", true, "direct", "", "providers.sandbox.default_run_in", ""},
		{"a company hosting no code", false, "direct", "direct", "", ""},
		{"a container seat", true, "direct", "container", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := directWarnings(mustCompany(t, directCompany(tc.native, tc.defaultRun, tc.seatIn)))
			switch {
			case tc.at == "" && len(got) != 0:
				t.Fatalf("warned %+v", got)
			case tc.at != "" && (len(got) != 1 || got[0].Path != tc.at || got[0].Seat != tc.seat):
				t.Fatalf("warnings %+v, want one at %s for seat %q", got, tc.at, tc.seat)
			case tc.at != "":
				for _, says := range []string{"Tier A keyring", "ANY live coding run", "`visibility: unit`",
					"run it in `container` or `e2b`"} {
					if !strings.Contains(got[0].Message, says) {
						t.Errorf("the warning does not say %q: %q", says, got[0].Message)
					}
				}
			}
		})
	}
}

// A CONTAINER SEAT ON A CLUSTERED FLEET IS WARNED, while the fleet's broker
// authenticates nobody (until the broker's authentication lands): on a fleet
// with peers, on a member serving leaves and on a leaf — main's split topology
// runs its seats, and so their boxes, on leaves — one warning at `stream`.
// None on a single node serving no leaves, none where the company hosts no
// code, none for an `e2b` seat.
//
// Mutations: drop the cluster term → the fleet rows pass silently and a
// single node never warns, red; drop the leaf terms → the member serving
// leaves and the leaf rows, red.
func TestAContainerSeatOnAClusteredFleetIsWarned(t *testing.T) {
	t.Parallel()
	fleet := func(b *Bootstrap) {
		b.Coordination = Coordination{Type: CoordinationEmbeddedKV}
		b.Stream.StoreDir = "/var/lib/crewlet/stream"
		b.Stream.Cluster = StreamCluster{Name: "acme", Port: 6222,
			Peers: []string{"nats://b.example.com:6222", "nats://c.example.com:6222"}}
	}
	servesLeaves := func(b *Bootstrap) {
		b.Coordination = Coordination{Type: CoordinationEmbeddedKV}
		b.Stream.StoreDir = "/var/lib/crewlet/stream"
		b.Stream.Leaf.Port = 7422
	}
	leaf := func(b *Bootstrap) {
		b.Coordination = Coordination{Type: CoordinationEmbeddedKV}
		b.Node.Roles = []string{"seats", "git"}
		b.Stream.Leaf.URLs = []string{"nats-leaf://a.example.com:7422"}
	}
	for _, tc := range []struct {
		name   string
		boot   func(*Bootstrap)
		native bool
		cell   string
		warns  bool
	}{
		{"a clustered fleet", fleet, true, "container", true},
		{"a member serving leaves", servesLeaves, true, "container", true},
		{"a leaf", leaf, true, "container", true},
		{"a single node serving no leaves", func(*Bootstrap) {}, true, "container", false},
		{"a fleet hosting no code", fleet, false, "container", false},
		{"an e2b seat on a fleet", fleet, true, "e2b", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := DefaultBootstrap()
			tc.boot(&b)
			doc := directCompany(tc.native, tc.cell, "")
			if tc.cell == "e2b" {
				doc = strings.Replace(doc, "    local: {}\n", "    e2b: {api_key: \"${E2B_API_KEY}\"}\n", 1)
			}
			c := mustCompany(t, doc)
			var got []Warning
			for _, w := range TierWarnings(&b, c) {
				if w.Path == "stream" {
					got = append(got, w)
				}
			}
			switch {
			case !tc.warns && len(got) != 0:
				t.Fatalf("warned %+v", got)
			case tc.warns && len(got) != 1:
				t.Fatalf("warnings %+v, want one at stream", got)
			case tc.warns:
				for _, says := range []string{"`container` coding box", "authenticates no client",
					"leaf listeners"} {
					if !strings.Contains(got[0].Message, says) {
						t.Errorf("the warning does not say %q: %q", says, got[0].Message)
					}
				}
			}
		})
	}
}
