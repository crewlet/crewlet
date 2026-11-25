package config_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/browser"
	"github.com/crewlet/crewlet/internal/caveat/caveattest"
	"github.com/crewlet/crewlet/internal/config"
)

// browserDoc is a company whose catalogue is catalogue and whose providers.llm
// entries are llm, each already indented for its block, and whose one seat,
// SWE, has a browser and an executor chain of chain (its default when empty).
func browserDoc(catalogue, llm, chain string) string {
	doc := "name: Acme\nproviders:\n  sandbox:\n" + catalogue + "  llm:\n" + llm + "roles:\n  - name: SWE\n"
	if chain != "" {
		doc += "    llm: " + chain + "\n"
	}
	return doc + "    sandbox:\n      browser: true\n"
}

// entryIn is a claude-code providers.llm entry named key, placed in cell, or
// in the catalogue's default where cell is empty.
func entryIn(key string, cell config.Placement) string {
	out := "    " + key + ":\n      model: sonnet\n"
	if cell != "" {
		out += "      run_in: " + string(cell) + "\n"
	}
	return out
}

// notContainer is every cell but container: a refusal case walks them, so the
// rule is held against every other cell without a case naming one.
func notContainer() []config.Placement {
	return slices.DeleteFunc(slices.Clone(config.Placements),
		func(p config.Placement) bool { return p == config.PlacementContainer })
}

// problemsAt is every problem at path.
func problemsAt(err error, path string) []config.Problem {
	var out []config.Problem
	for _, p := range config.Problems(err) {
		if p.Path == path {
			out = append(out, p)
		}
	}
	return out
}

// heldToAPersonsWords fails where said is not a person's words: no key, path
// or engine vocabulary, ending as a sentence.
func heldToAPersonsWords(t *testing.T, said string) {
	t.Helper()
	if found := fieldPath.FindString(said); found != "" {
		t.Errorf("said %q names a field path (%q)", said, found)
	}
	for _, problem := range caveattest.Problems(said) {
		t.Errorf("said %q is not worded for a person: %s", said, problem)
	}
}

// A STORED REVISION KEEPING doc STILL APPLIES, and is warned about at path:
// each browser rule is an admission rule, which nothing running depends on.
func appliesAndWarns(t *testing.T, doc, path string) {
	t.Helper()
	stored, err := config.ParseCompanyDocument([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if err := stored.ValidateRunnable(); err != nil {
		t.Errorf("a stored revision keeping it was refused at apply: %v", err)
	}
	if !slices.ContainsFunc(stored.AdmissionWarnings(), func(w config.Warning) bool {
		return w.Kind == config.WarningAdmission && w.Path == path
	}) {
		t.Errorf("a stored revision keeping it is not warned about at %s: %+v", path, stored.AdmissionWarnings())
	}
}

// A SEAT'S BROWSER RUNS ONLY WHERE EVERY RUN OF THE SEAT LANDS IN A CONTAINER
// BOX OFF ITS HOST'S NETWORK. The seat is refused at its own sandbox.browser,
// in its person's words, once per cause and naming the first entry that
// reaches the cell, worded by what placed the run there: the entry's run_in,
// the default_run_in written, or the catalogue resolving alone. Every cell but
// container is refused by walking config.Placements under the double, which
// configures each. Mutation: check only the chain's head; refuse at the entry;
// write the rule as == PlacementDirect; word the catalogue-alone refusal as
// the default_run_in one.
func TestABrowserSeatIsRefusedWhereAnyRunOfItLandsOutsideAContainer(t *testing.T) {
	t.Parallel()
	const at = "roles[0].sandbox.browser"
	const said = "This agent's browser runs only in a container, and this agent can run " +
		"outside one. Run it in a container, or turn its browser off."
	type refusal struct {
		name, doc, path string
		says, never     []string
	}
	var cases []refusal
	for _, cell := range notContainer() {
		cases = append(cases, refusal{
			name: "by the entry's run_in, " + string(cell),
			doc:  browserDoc("    fake: true\n", entryIn("main", cell), ""), path: at,
			says: []string{"providers.llm.main, whose run_in is " + string(cell),
				"Set that entry's run_in to container", "remove sandbox.browser"},
		}, refusal{
			name: "by default_run_in, " + string(cell),
			doc:  browserDoc("    fake: true\n    default_run_in: "+string(cell)+"\n", entryIn("main", ""), ""),
			path: at,
			says: []string{"providers.llm.main, which names no run_in and so launches in " +
				"providers.sandbox.default_run_in, " + string(cell), "make the default container"},
		}, refusal{
			name: "by a fallback member, " + string(cell),
			doc: browserDoc("    fake: true\n", entryIn("boxed", config.PlacementContainer)+entryIn("other", cell),
				"[boxed, other]"),
			path: at, says: []string{"providers.llm.other, whose run_in is " + string(cell)},
			never: []string{"providers.llm.boxed"},
		}, refusal{
			name: "a unit's seat, " + string(cell),
			doc: "name: Acme\nproviders:\n  sandbox:\n    fake: true\n  llm:\n" + entryIn("main", cell) +
				"units:\n  - name: Eng\n    id: eng\n    roles:\n      - name: SWE\n" +
				"        sandbox:\n          browser: true\n",
			path: "units[0].roles[0].sandbox.browser",
			says: []string{"providers.llm.main, whose run_in is " + string(cell)},
		})
	}
	cases = append(cases, refusal{
		// The double resolves alone: no default_run_in is written, and the
		// refusal names the block rather than a field nobody wrote.
		name: "by the catalogue alone",
		doc:  browserDoc("    fake: true\n", entryIn("main", ""), ""), path: at,
		says: []string{"which names no run_in, and providers.sandbox names no default_run_in, so the " +
			"run lands where the catalogue resolves on its own, " + string(config.PlacementDirect),
			"set providers.sandbox.default_run_in to container"},
		never: []string{"launches in providers.sandbox.default_run_in"},
	})
	if others := notContainer(); len(others) > 1 {
		cases = append(cases, refusal{
			name: "two cells outside, one refusal naming the first",
			doc: browserDoc("    fake: true\n", entryIn("first", others[0])+entryIn("second", others[1]),
				"[first, second]"),
			path: at, says: []string{"providers.llm.first"}, never: []string{"providers.llm.second"},
		})
	}
	for _, tc := range cases {
		_, err := config.ParseCompany([]byte(tc.doc))
		got := problemsAt(err, tc.path)
		if len(got) != 1 {
			t.Errorf("%s: problems at %s = %+v, want one (all: %v)", tc.name, tc.path, got, err)
			continue
		}
		p := got[0]
		if p.Kind != "conflict" || p.Seat != "swe" || p.Said != said {
			t.Errorf("%s: %+v, want a conflict about seat swe, said %q", tc.name, p, said)
		}
		for _, want := range tc.says {
			if !strings.Contains(p.Message, want) {
				t.Errorf("%s: %q does not say %q", tc.name, p.Message, want)
			}
		}
		for _, unwanted := range tc.never {
			if strings.Contains(p.Message, unwanted) {
				t.Errorf("%s: %q says %q", tc.name, p.Message, unwanted)
			}
		}
		heldToAPersonsWords(t, p.Said)
		appliesAndWarns(t, tc.doc, tc.path)
	}

	for _, ok := range []struct{ name, doc string }{
		{"every run in a container, under the double",
			browserDoc("    fake: true\n    default_run_in: container\n", entryIn("main", ""), "")},
		{"every run in a container box",
			browserDoc("    local: {image: img}\n    default_run_in: container\n", entryIn("main", ""), "")},
		{"a fallback member in a container too",
			browserDoc("    local: {image: img}\n", entryIn("first", config.PlacementContainer)+
				entryIn("second", config.PlacementContainer), "[first, second]")},
		{"a container on a bridge",
			browserDoc("    local: {image: img, network: bridge}\n    default_run_in: container\n",
				entryIn("main", ""), "")},
		{"a container on a named network",
			browserDoc("    local: {image: img, run_args: [--network, crewlet-boxes]}\n"+
				"    default_run_in: container\n", entryIn("main", ""), "")},
	} {
		if _, err := config.ParseCompany([]byte(ok.doc)); err != nil {
			t.Errorf("%s: refused: %v", ok.name, err)
		}
	}
}

// A CONTAINER ON ITS HOST'S NETWORK IS NO PLACE FOR A SEAT'S BROWSER, in any
// spelling the host-network rule finds: refused at the seat's sandbox.browser,
// naming the setting where it is written. Mutation: read local.network alone;
// name the setting without its index.
func TestABrowserSeatIsRefusedInAContainerOnItsHostsNetwork(t *testing.T) {
	t.Parallel()
	const at = "roles[0].sandbox.browser"
	for _, tc := range []struct{ local, setting string }{
		{"{image: img, network: host}", "providers.sandbox.local.network: host"},
		{"{image: img, run_args: [--network=host]}", "providers.sandbox.local.run_args[0]: --network=host"},
		{"{image: img, run_args: [--cpus, \"2\", --network, host]}",
			"providers.sandbox.local.run_args[2]: --network host"},
		{"{image: img, run_args: [--net=host]}", "providers.sandbox.local.run_args[0]: --net=host"},
		{"{image: img, run_args: [--net, host]}", "providers.sandbox.local.run_args[0]: --net host"},
		{"{image: img, run_args: [\"--network=container:db\"]}",
			"providers.sandbox.local.run_args[0]: --network=container:db"},
	} {
		doc := browserDoc("    local: "+tc.local+"\n    default_run_in: container\n", entryIn("main", ""), "")
		_, err := config.ParseCompany([]byte(doc))
		got := problemsAt(err, at)
		if len(got) != 1 {
			t.Errorf("%s: problems at %s = %+v, want one (all: %v)", tc.local, at, got, err)
			continue
		}
		p := got[0]
		if p.Kind != "conflict" || p.Seat != "swe" ||
			!strings.Contains(p.Message, "providers.llm.main in a container, and "+tc.setting+" can put") ||
			!strings.Contains(p.Message, "loopback") || !strings.Contains(p.Message, "remove sandbox.browser") {
			t.Errorf("%s: %+v, want a conflict naming %q", tc.local, p, tc.setting)
		}
		heldToAPersonsWords(t, p.Said)
		appliesAndWarns(t, doc, at)
	}
}

// A SEAT WHOSE ENTRY DENIES ITS HARNESS THE WEB HAS NO BROWSER: a browser would
// open the web that denial closes. Refused at the seat's sandbox.browser,
// naming the first entry of the chain that denies it. Mutation: check only the
// chain's head; drop the check.
func TestABrowserSeatIsRefusedOnAnEntryThatDeniesTheWeb(t *testing.T) {
	t.Parallel()
	const at = "roles[0].sandbox.browser"
	const catalogue = "    local: {image: img}\n    default_run_in: container\n"
	denied := func(key string) string { return entryIn(key, "") + "      overrides: {web_tools: denied}\n" }
	for _, tc := range []struct{ name, doc, names string }{
		{"the head", browserDoc(catalogue, denied("main"), ""), "providers.llm.main"},
		{"a fallback member", browserDoc(catalogue, entryIn("open", "")+denied("closed"), "[open, closed]"),
			"providers.llm.closed"},
	} {
		_, err := config.ParseCompany([]byte(tc.doc))
		got := problemsAt(err, at)
		if len(got) != 1 {
			t.Errorf("%s: problems at %s = %+v, want one (all: %v)", tc.name, at, got, err)
			continue
		}
		p := got[0]
		if p.Kind != "conflict" || !strings.Contains(p.Message, tc.names+", which denies its harness the web") ||
			!strings.Contains(p.Message, "overrides.web_tools: denied") {
			t.Errorf("%s: %+v, want a conflict naming %s's denial", tc.name, p, tc.names)
		}
		heldToAPersonsWords(t, p.Said)
		appliesAndWarns(t, tc.doc, at)
	}
	allowed := browserDoc(catalogue, entryIn("main", "")+"      overrides: {web_tools: allowed}\n", "")
	if _, err := config.ParseCompany([]byte(allowed)); err != nil {
		t.Errorf("an entry that allows the web: %v", err)
	}
}

// THE NAME browser IS A SEAT'S OWN BROWSER'S, which its box sees under it: a
// company server called so is refused at its name, switched off or not, and a
// name that only holds the word is not. An admission rule: a stored revision
// keeping one applies. Mutation: make it runnable.
func TestTheBrowserNameIsReservedForASeatsOwnBrowser(t *testing.T) {
	t.Parallel()
	const at = "mcp_servers[0].name"
	for _, server := range []string{
		"{name: browser, command: some-mcp}",
		"{name: browser, command: some-mcp, enabled: false}",
	} {
		doc := "name: Acme\nmcp_servers:\n  - " + server + "\n"
		_, err := config.ParseCompany([]byte(doc))
		p, ok := problemAt(err, at)
		if !ok || p.Kind != "conflict" || !errors.Is(err, config.ErrConflict) ||
			!strings.Contains(p.Message, `"browser" is reserved for a seat's own browser`) {
			t.Errorf("%s: %v, want a conflict at %s", server, err, at)
			continue
		}
		heldToAPersonsWords(t, p.Said)
		appliesAndWarns(t, doc, at)
	}
	if _, err := config.ParseCompany([]byte("name: Acme\nmcp_servers:\n  - {name: browser-tools, command: some-mcp}\n")); err != nil {
		t.Errorf("a name holding the word was refused: %v", err)
	}
	if browser.ServerName != "browser" {
		t.Errorf("the name reserved is %q: the cases above spell browser", browser.ServerName)
	}
}

// A chromium_sandbox NOTHING READS IS REFUSED, at the field: only a browser
// seat's container box reads it, so it is refused where no browser seat
// launches in a container, even beside another seat's container box.
// Mutation: refuse it only where no box at all is a container.
func TestAChromiumSandboxNothingReadsIsRefused(t *testing.T) {
	t.Parallel()
	const at = "providers.sandbox.local.chromium_sandbox"
	const local = "    local: {image: img, chromium_sandbox: waived}\n"
	for _, tc := range []struct{ name, doc string }{
		{"no seat", "name: Acme\nproviders:\n  sandbox:\n" + local + "    default_run_in: container\n"},
		{"a container box with no browser",
			"name: Acme\nproviders:\n  sandbox:\n" + local + "    default_run_in: container\n  llm:\n" +
				entryIn("main", "") + "roles:\n  - name: SWE\n"},
		{"a browser seat only in another cell, beside another seat's container box",
			"name: Acme\nproviders:\n  sandbox:\n" + local + "  llm:\n" +
				entryIn("boxed", config.PlacementContainer) + entryIn("host", config.PlacementDirect) +
				"roles:\n  - name: SWE\n    llm: host\n    sandbox:\n      browser: true\n" +
				"  - name: Ops\n    llm: boxed\n"},
	} {
		_, err := config.ParseCompany([]byte(tc.doc))
		p, ok := problemAt(err, at)
		if !ok || p.Kind != "conflict" || !strings.Contains(p.Message, "only a browser seat's container box reads this") {
			t.Errorf("%s: %v, want a conflict at %s", tc.name, err, at)
			continue
		}
		appliesAndWarns(t, tc.doc, at)
	}
	read := browserDoc(local+"    default_run_in: container\n", entryIn("main", ""), "")
	if _, err := config.ParseCompany([]byte(read)); err != nil {
		t.Errorf("a browser seat's container box reads it, yet: %v", err)
	}
}

// A chromium_sandbox OFF ITS LIST IS REFUSED ON THE WRITE, at the field, and a
// stored revision holding one still applies and runs on the stricter reading,
// required: a later build may add a value. Mutation: move the check into a
// runnable rule.
func TestAChromiumSandboxOutsideItsListIsRefusedOnTheWriteAndRunAsRequired(t *testing.T) {
	t.Parallel()
	const at = "providers.sandbox.local.chromium_sandbox"
	doc := browserDoc("    local: {image: img, chromium_sandbox: \"off\"}\n    default_run_in: container\n",
		entryIn("main", ""), "")
	_, err := config.ParseCompany([]byte(doc))
	p, ok := problemAt(err, at)
	if !ok || p.Kind != "unknown_value" || !strings.Contains(p.Message, `"off" (want "required" or "waived")`) {
		t.Fatalf("%v, want an unknown value at %s naming the list", err, at)
	}
	appliesAndWarns(t, doc, at)
	stored, err := config.ParseCompanyDocument([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if got := stored.Providers.Sandbox.ChromiumSandbox(); got != browser.SandboxRequired {
		t.Errorf("a stored revision's unknown value reads %q, want required", got)
	}
}

// CHROMIUM'S OWN SANDBOX IS REQUIRED UNLESS THE CONTAINER CELL WAIVES IT in
// exactly those words: unset, any other value and no catalogue at all read as
// required, the stricter. Mutation: default to waived.
func TestTheChromiumSandboxIsRequiredUnlessTheContainerCellWaivesIt(t *testing.T) {
	t.Parallel()
	var none *config.SandboxProvider
	if got := none.ChromiumSandbox(); got != browser.SandboxRequired {
		t.Errorf("no catalogue reads %q, want required", got)
	}
	if got := (&config.SandboxProvider{Fake: true}).ChromiumSandbox(); got != browser.SandboxRequired {
		t.Errorf("a catalogue with no local block reads %q, want required", got)
	}
	for value, want := range map[browser.ChromiumSandbox]browser.ChromiumSandbox{
		"":       browser.SandboxRequired,
		"waived": browser.SandboxWaived, "required": browser.SandboxRequired,
		"Waived": browser.SandboxRequired, "off": browser.SandboxRequired,
	} {
		catalogue := &config.SandboxProvider{Local: &config.LocalSandbox{ChromiumSandbox: value}}
		if got := catalogue.ChromiumSandbox(); got != want {
			t.Errorf("chromium_sandbox %q reads %q, want %q", value, got, want)
		}
	}
}

// A SERVER THAT LAUNCHES PLAYWRIGHT MCP RUNS A BROWSER ON THE ENGINE HOST, and
// is advised about at the command or argument that launches it, in each
// spelling: the package with and without a release, its binary, its image
// with and without a tag, and the package inside a shell's -c string. A
// package that only starts with the name, an http server and a server
// switched off are not. Mutation: match a substring.
func TestAPlaywrightServerOnTheEngineHostIsAdvised(t *testing.T) {
	t.Parallel()
	doc := "name: Acme\nmcp_servers:\n" +
		"  - {name: pinned, command: npx, args: [-y, \"@playwright/mcp@0.0.86\"]}\n" +
		"  - {name: bare, command: npx, args: [\"@playwright/mcp\"]}\n" +
		"  - {name: installed, command: /usr/local/bin/playwright-mcp}\n" +
		"  - {name: image, command: docker, args: [run, -i, --rm, mcr.microsoft.com/playwright/mcp]}\n" +
		"  - {name: tagged, command: docker, args: [run, -i, --rm, \"mcr.microsoft.com/playwright/mcp:v0.0.86\"]}\n" +
		"  - {name: shell, command: sh, args: [-c, \"npx @playwright/mcp@latest --headless\"]}\n" +
		"  - {name: lookalike, command: npx, args: [\"@playwright/mcpx\"]}\n" +
		"  - {name: remote, transport: http, url: \"https://playwright-mcp.example.com/mcp\"}\n" +
		"  - {name: switched-off, command: npx, args: [\"@playwright/mcp\"], enabled: false}\n"
	c, err := config.ParseCompany([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]struct{ path, spelled string }{
		"pinned":    {"mcp_servers[0].args[1]", "@playwright/mcp@0.0.86"},
		"bare":      {"mcp_servers[1].args[0]", "@playwright/mcp"},
		"installed": {"mcp_servers[2].command", "/usr/local/bin/playwright-mcp"},
		"image":     {"mcp_servers[3].args[3]", "mcr.microsoft.com/playwright/mcp"},
		"tagged":    {"mcp_servers[4].args[3]", "mcr.microsoft.com/playwright/mcp:v0.0.86"},
		"shell":     {"mcp_servers[5].args[1]", "@playwright/mcp@latest"},
	}
	var title, person string
	got := map[string]bool{}
	for _, w := range c.AdvisoryWarnings() {
		if !strings.Contains(w.Message, "launches Playwright MCP") {
			continue
		}
		title, person = w.Title, w.Person
		got[w.Item] = true
		expected, ok := want[w.Item]
		switch {
		case !ok:
			t.Errorf("%s is advised: %+v", w.Item, w)
		case w.Kind != config.WarningAdvisory || w.Path != expected.path ||
			!strings.Contains(w.Message, "("+expected.spelled+")"):
			t.Errorf("%s: %+v, want an advisory at %s naming %s", w.Item, w, expected.path, expected.spelled)
		}
	}
	for item := range want {
		if !got[item] {
			t.Errorf("%s is not advised", item)
		}
	}
	if title != "A browser on the engine host" || person == "" {
		t.Errorf("the advisory carries title %q and words %q, not its rule's", title, person)
	}
}
