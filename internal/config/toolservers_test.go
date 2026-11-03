package config

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/skills"
	"github.com/crewlet/crewlet/internal/mcp/catalogue"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/sourcetree"

	"gopkg.in/yaml.v3"
)

// The tool servers the docs, the examples and the curated MCP catalogue hand
// an operator are held here, because each way they go wrong is SILENT: the
// server starts, reports itself healthy, and the company it serves quietly
// loses something. A floating version is a build nobody can name afterwards,
// and a release that renames a tool disarms every skill triggered on the old
// name without a sound. And slack-mcp-server answers no post unless its entry
// asks it to, so a seat built from a page that forgot reads every message and
// answers none. Running a server cannot catch either, since the engine is
// tool-agnostic in how it runs one, so each place a launch is written is
// checked where it is written.
//
// A PAGE PINS AND AN EXAMPLE DOES NOT. A page states what one release does:
// its tools, its variables, what it takes to post. So it names that release.
// A shipped example is a company somebody copies and runs long after it was
// written, and nothing moves a pin in it, so it fetches the newest of
// everything, except the server a bundled tool skill triggers on by tool name:
// there a renamed tool would disarm the skill without a sound. The curated
// catalogue is a page in this sense: what it offers is one release, read.

// pin is one package a launch fetches, at the release what is said about it
// was read against.
type pin struct {
	name string
	// readAt is the release whose source what the docs and the catalogue
	// say about this server was read against. A bump that reaches the docs
	// fails here until somebody reads the new release and moves the
	// catalogue entry's Release, because every one of those claims is about
	// one release and none of them fails on its own.
	readAt string
}

// pinnedToolServers are EVERY package a document launches through a runner,
// each at the release it was read against: the curated MCP catalogue's
// (internal/mcp/catalogue), whose entries name the package and the exact
// release their facts were read against, with what was read beside each. ONE
// LIST, so the release the catalogue offers and the release a page launches
// cannot be two. A package missing there fails
// [TestEveryDocumentedLaunchNamesAnExactRelease], so a new one cannot reach
// the docs without somebody reading it and cataloguing it first.
func pinnedToolServers() []pin {
	var out []pin
	for _, e := range catalogue.Curated() {
		if e.Package != "" {
			out = append(out, pin{name: e.Package, readAt: e.Release})
		}
	}
	return out
}

// catalogueLaunches are the curated catalogue's own launches, read as a page's
// are: the catalogue is a document an operator copies a launch from.
func catalogueLaunches() []launch {
	var out []launch
	for _, e := range catalogue.Curated() {
		if e.Transport == catalogue.TransportStdio {
			out = append(out, launch{where: "internal/mcp/catalogue, " + e.ID, name: e.Name,
				line: append([]string{e.Command}, e.Args...)})
		}
	}
	return out
}

// catalogueServers are the curated catalogue's local servers as the entries a
// person drafts from them: the launch, the fixed values the entry sets, its
// tool prefix and its annotation corrections.
func catalogueServers() map[string]MCPServer {
	out := map[string]MCPServer{}
	for _, e := range catalogue.Curated() {
		if e.Transport != catalogue.TransportStdio {
			continue
		}
		s := MCPServer{Name: e.Name, Command: e.Command, Args: e.Args, ToolPrefix: e.ToolPrefix,
			Env: map[string]string{}, ToolAnnotations: map[string]ToolAnnotations{}}
		for _, in := range e.Env {
			if in.Value != "" {
				s.Env[in.Name] = in.Value
			}
		}
		for tool, h := range e.ToolAnnotations {
			s.ToolAnnotations[tool] = ToolAnnotations{ReadOnly: toggleOf(h.ReadOnly),
				Destructive: toggleOf(h.Destructive), Idempotent: toggleOf(h.Idempotent),
				OpenWorld: toggleOf(h.OpenWorld)}
		}
		out["internal/mcp/catalogue, "+e.ID] = s
	}
	return out
}

func toggleOf(b *bool) Toggle {
	switch {
	case b == nil:
		return Toggle{}
	case *b:
		return org.On()
	}
	return org.Off()
}

var exactRelease = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

// newest reports whether a package spec runs whatever release is newest when
// it is fetched: a bare name, or the `@latest` npm and uvx both take.
func newest(spec string) bool {
	_, version := splitRelease(spec)
	return version == "" || version == "latest"
}

// shippedExample reports whether where — a path from the repository's root, as
// the documents are reported by — is in the shipped examples, which run the
// newest release of what they fetch rather than an exact one: see
// [TestTheShippedExamplesRunTheNewestRelease].
func shippedExample(where string) bool {
	return strings.HasPrefix(filepath.ToSlash(where), "examples/")
}

func isPinnedToolServer(name string) bool {
	for _, p := range pinnedToolServers() {
		if p.name == name {
			return true
		}
	}
	return false
}

// runnerGrammar is how one package runner reads the arguments after its own
// words: which flags consume a value, which of those name a package, and
// whether its positionals are packages to install or one package to run
// followed by that package's own arguments.
//
// A flag that takes a value and is missing from valued makes the value read
// as a positional, which then names no exact release and FAILS — the grammar
// errs loud, never silent, so a flag no document uses yet costs nothing.
type runnerGrammar struct {
	valued []string
	// fromFlags name the package to run; the first positional is then the
	// command that package provides (uvx --from, pipx run --spec, npx -p).
	fromFlags []string
	// withFlags name packages fetched beside the one that runs (uvx --with).
	withFlags []string
	// shellFlags hand a shell string instead of a command (npx -c), so
	// nothing positional is a package.
	shellFlags []string
	// editableFlags install what they name as editable (pip -e, uv
	// --with-editable): a local directory, or a VCS or archive URL that is
	// a package fetched like any other.
	editableFlags []string
	// install says every positional is a package to install, not a command.
	install bool
}

var (
	uvRun = runnerGrammar{
		valued: []string{"--from", "--with", "--with-editable", "--with-requirements",
			"--python", "-p", "--index", "--default-index", "--index-url", "--extra-index-url",
			"--find-links", "-f"},
		fromFlags:     []string{"--from"},
		withFlags:     []string{"--with"},
		editableFlags: []string{"--with-editable"},
	}
	uvInstall = runnerGrammar{
		valued:        uvRun.valued,
		withFlags:     []string{"--with"},
		editableFlags: []string{"--with-editable"},
		install:       true,
	}
	pipxRun = runnerGrammar{
		valued:    []string{"--spec", "--python", "--index-url", "-i", "--pip-args"},
		fromFlags: []string{"--spec"},
	}
	pipxInstall = runnerGrammar{
		valued:  []string{"--python", "--index-url", "-i", "--pip-args", "--suffix"},
		install: true,
	}
	pipInstall = runnerGrammar{
		valued: []string{"-r", "--requirement", "-c", "--constraint", "-e", "--editable",
			"-t", "--target", "-i", "--index-url", "--extra-index-url", "-f", "--find-links",
			"--prefix", "--root", "--upgrade-strategy", "--platform", "--python-version",
			"--implementation", "--abi", "--src", "--progress-bar", "--report",
			"--root-user-action", "--config-settings", "-C"},
		editableFlags: []string{"-e", "--editable"},
		install:       true,
	}
	npmExec = runnerGrammar{
		valued:     []string{"-p", "--package", "-c", "--call", "-w", "--workspace"},
		fromFlags:  []string{"-p", "--package"},
		shellFlags: []string{"-c", "--call"},
	}
	npmInstall = runnerGrammar{
		valued: []string{"-w", "--workspace", "--prefix", "--registry", "--tag",
			"--omit", "--include", "--install-strategy"},
		install: true,
	}
)

// runners are the commands that fetch a package by name, by the words that
// invoke them. Every command the docs spell through one of these names what
// it fetches at an exact release.
var runners = []struct {
	words   []string
	grammar runnerGrammar
}{
	{[]string{"uvx"}, uvRun},
	{[]string{"uv", "tool", "run"}, uvRun},
	{[]string{"uv", "tool", "install"}, uvInstall},
	{[]string{"uv", "pip", "install"}, pipInstall},
	{[]string{"pipx", "run"}, pipxRun},
	{[]string{"pipx", "install"}, pipxInstall},
	{[]string{"pip", "install"}, pipInstall},
	{[]string{"pip3", "install"}, pipInstall},
	{[]string{"npx"}, npmExec},
	{[]string{"npm", "exec"}, npmExec},
	{[]string{"npm", "x"}, npmExec},
	{[]string{"npm", "install"}, npmInstall},
	{[]string{"npm", "i"}, npmInstall},
}

// invocation is what one runner command line fetches and runs.
type invocation struct {
	// packages is every package the command fetches by name, as spelled.
	packages []string
	// run is the package whose command runs, "" for an install or a shell
	// string; argv is what that command is handed.
	run  string
	argv []string
}

// invocationOf reads a command line — the command and its arguments — as the
// package runner it starts with reads it, or reports that it is not one: a
// server launched any other way (glab, a binary on PATH, an http URL) has no
// version in its config to pin.
func invocationOf(line []string) (invocation, bool) {
	if len(line) == 0 {
		return invocation{}, false
	}
	line = append([]string{filepath.Base(line[0])}, line[1:]...)
	for _, r := range runners {
		if len(line) < len(r.words) || !slices.Equal(line[:len(r.words)], r.words) {
			continue
		}
		return r.grammar.read(line[len(r.words):]), true
	}
	return invocation{}, false
}

func (g runnerGrammar) read(args []string) invocation {
	var inv invocation
	runs := !g.install // the next positional is the package to run
	commandNext := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			// The end of the options: what follows is the command and its
			// arguments (npm exec), or nothing but packages (an install).
			rest := args[i+1:]
			if g.install {
				for _, p := range rest {
					if !localPath(p) {
						inv.packages = append(inv.packages, p)
					}
				}
			} else if len(rest) > 0 {
				inv.take(rest, runs, commandNext)
			}
			return inv
		}
		if strings.HasPrefix(a, "-") && a != "-" {
			flag, value, inline := strings.Cut(a, "=")
			if !slices.Contains(g.valued, flag) {
				continue
			}
			if !inline {
				if i+1 == len(args) {
					return inv
				}
				i++
				value = args[i]
			}
			switch {
			case slices.Contains(g.fromFlags, flag):
				inv.packages = append(inv.packages, value)
				inv.run = value
				runs, commandNext = false, true
			case slices.Contains(g.withFlags, flag):
				inv.packages = append(inv.packages, value)
			case slices.Contains(g.editableFlags, flag):
				if !localPath(value) {
					inv.packages = append(inv.packages, value)
				}
			case slices.Contains(g.shellFlags, flag):
				runs = false
			}
			continue
		}
		if g.install {
			if !localPath(a) {
				inv.packages = append(inv.packages, a)
			}
			continue
		}
		inv.take(args[i:], runs, commandNext)
		return inv
	}
	return inv
}

// take reads the first positional of a run-type command: the package to run,
// or — after a --from — the command that package provides.
func (inv *invocation) take(rest []string, runs, commandNext bool) {
	switch {
	case runs:
		inv.packages = append(inv.packages, rest[0])
		inv.run, inv.argv = rest[0], rest[1:]
	case commandNext:
		inv.argv = rest[1:]
	}
}

// localPath is an install argument that names a directory or a file rather
// than a package fetched by name.
func localPath(arg string) bool {
	return arg == "." || strings.HasPrefix(arg, "./") || strings.HasPrefix(arg, "../") ||
		strings.HasPrefix(arg, "/") || strings.HasPrefix(arg, "~")
}

// launchedPackage is the package a server entry's runner starts and the
// arguments that package is handed, and whether the entry launches through a
// package runner at all.
func launchedPackage(s MCPServer) (spec string, argv []string, ok bool) {
	commands := commandsOf(append([]string{s.Command}, s.Args...))
	inv, ok := invocationOf(commands[len(commands)-1])
	return inv.run, inv.argv, ok
}

// shells are the commands that run a string handed to -c.
var shells = []string{"sh", "bash", "dash", "zsh"}

// commandsOf is every command a launch line runs, through the wrappers a
// configuration can put in front of a runner: `env` with its assignments and
// options, and a shell's -c string split at its separators. The LAST is the
// one whose stdio the engine speaks to — the server; the others run first.
// A line that is none of these is its own single command.
func commandsOf(line []string) [][]string {
	if len(line) == 0 {
		return [][]string{line}
	}
	switch base := filepath.Base(line[0]); {
	case base == "env":
		rest := line[1:]
		for len(rest) > 0 {
			switch a := rest[0]; {
			case a == "-S" || a == "--split-string":
				// The next word is a whole command line of its own.
				if len(rest) < 2 {
					return [][]string{line}
				}
				return commandsOf(append(strings.Fields(rest[1]), rest[2:]...))
			case a == "-u" || a == "--unset" || a == "-C" || a == "--chdir":
				rest = rest[min(2, len(rest)):]
			case strings.HasPrefix(a, "-") || strings.Contains(a, "="):
				rest = rest[1:]
			default:
				return commandsOf(rest)
			}
		}
		return [][]string{line}
	case slices.Contains(shells, base):
		for i := 1; i < len(line); i++ {
			a := line[i]
			if strings.HasPrefix(a, "-") && strings.HasSuffix(a, "c") && !strings.HasPrefix(a, "--") {
				if i+1 == len(line) {
					break
				}
				var out [][]string
				for _, segment := range shellSegment.Split(line[i+1], -1) {
					words := strings.Fields(segment)
					for len(words) > 0 && (words[0] == "exec" || (strings.Contains(words[0], "=") && !strings.HasPrefix(words[0], "-"))) {
						words = words[1:]
					}
					if len(words) > 0 {
						out = append(out, commandsOf(words)...)
					}
				}
				if len(out) == 0 {
					return [][]string{line}
				}
				return out
			}
		}
		return [][]string{line}
	default:
		return [][]string{line}
	}
}

// shellSegment is where a shell string starts another command.
var shellSegment = regexp.MustCompile(`&&|\|\||[;|\n]`)

// splitRelease splits a runner's package spec into the package and the
// version it names, or "" when it names none. A scoped npm package's leading
// @ is part of its name, not a version.
func splitRelease(spec string) (name, version string) {
	if n, v, ok := strings.Cut(spec, "=="); ok {
		return n, v
	}
	if i := strings.LastIndex(spec, "@"); i > 0 {
		return spec[:i], spec[i+1:]
	}
	return spec, ""
}

// THE RUNNER GRAMMAR READS A COMMAND LINE AS THE RUNNER DOES. Each case is a
// spelling a document could use; the package it fetches is what the pin
// checks read, so a case that read the wrong token would check the wrong
// thing — or nothing.
func TestTheRunnerGrammarFindsWhatACommandFetches(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		line     string
		packages []string
		run      string
		argv     []string
	}{
		{"uvx mcp-atlassian==0.23.1", []string{"mcp-atlassian==0.23.1"}, "mcp-atlassian==0.23.1", nil},
		{"uvx --python 3.12 mcp-server-mattermost", []string{"mcp-server-mattermost"}, "mcp-server-mattermost", nil},
		{"uvx --from mcp-atlassian==0.23.1 mcp-atlassian --transport stdio", []string{"mcp-atlassian==0.23.1"}, "mcp-atlassian==0.23.1", []string{"--transport", "stdio"}},
		{"uvx --with mcp==1.30.0 mcp-atlassian==0.23.1", []string{"mcp==1.30.0", "mcp-atlassian==0.23.1"}, "mcp-atlassian==0.23.1", nil},
		{"uvx mcp-atlassian@0.23.1", []string{"mcp-atlassian@0.23.1"}, "mcp-atlassian@0.23.1", nil},
		{"uvx --with-editable ./plugin mcp-atlassian==0.23.1", []string{"mcp-atlassian==0.23.1"}, "mcp-atlassian==0.23.1", nil},
		{"uvx --with-editable git+https://example.com/plugin mcp-atlassian==0.23.1", []string{"git+https://example.com/plugin", "mcp-atlassian==0.23.1"}, "mcp-atlassian==0.23.1", nil},
		{"uv tool run --from=mcp-server-mattermost==0.6.1 mcp-server-mattermost", []string{"mcp-server-mattermost==0.6.1"}, "mcp-server-mattermost==0.6.1", nil},
		{"uv tool install mcp-atlassian", []string{"mcp-atlassian"}, "", nil},
		{"pipx run --spec mcp-atlassian==0.23.1 mcp-atlassian", []string{"mcp-atlassian==0.23.1"}, "mcp-atlassian==0.23.1", nil},
		{"pip install --quiet uv==0.13.0", []string{"uv==0.13.0"}, "", nil},
		{"pip install -r requirements.txt -e . uv", []string{"uv"}, "", nil},
		{"pip install -e git+https://github.com/sooperset/mcp-atlassian", []string{"git+https://github.com/sooperset/mcp-atlassian"}, "", nil},
		{"pip install --editable=../mcp-atlassian", nil, "", nil},
		{"uv pip install mcp-server-mattermost", []string{"mcp-server-mattermost"}, "", nil},
		{"npx -y slack-mcp-server --transport stdio", []string{"slack-mcp-server"}, "slack-mcp-server", []string{"--transport", "stdio"}},
		{"npx -p tavily-mcp@0.2.22 tavily-mcp", []string{"tavily-mcp@0.2.22"}, "tavily-mcp@0.2.22", nil},
		{"npx -c 'tavily-mcp'", nil, "", nil},
		{"npm exec --yes -- slack-mcp-server@1.3.0 --transport stdio", []string{"slack-mcp-server@1.3.0"}, "slack-mcp-server@1.3.0", []string{"--transport", "stdio"}},
		{"npm exec slack-mcp-server", []string{"slack-mcp-server"}, "slack-mcp-server", nil},
		{"npm install", nil, "", nil},
		{"npm i @scope/pkg@1.0.0 ./local", []string{"@scope/pkg@1.0.0"}, "", nil},
		{"pip install -q -- uv mcp-server-mattermost==0.6.1", []string{"uv", "mcp-server-mattermost==0.6.1"}, "", nil},
	} {
		inv, ok := invocationOf(strings.Fields(c.line))
		if !ok {
			t.Errorf("%q: not read as a package runner", c.line)
			continue
		}
		if !slices.Equal(inv.packages, c.packages) || inv.run != c.run || !slices.Equal(inv.argv, c.argv) {
			t.Errorf("%q: fetches %q, runs %q with %q; want %q, %q with %q",
				c.line, inv.packages, inv.run, inv.argv, c.packages, c.run, c.argv)
		}
	}
	for _, line := range []string{"glab mcp serve", "npm start", "uv sync", "docker run x"} {
		if _, ok := invocationOf(strings.Fields(line)); ok {
			t.Errorf("%q was read as a package runner", line)
		}
	}
}

// A LAUNCH BEHIND A WRAPPER IS READ AS WHAT IT RUNS. `env` and a shell's -c
// string are what a configuration puts in front of a runner to set a variable
// or a directory first, and a launch read only by its first word would check
// `env` and certify nothing. The last command is the server.
func TestALaunchBehindAWrapperIsReadAsWhatItRuns(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		line []string
		want [][]string
	}{
		{[]string{"env", "FOO=1", "-u", "BAR", "uvx", "mcp-atlassian"}, [][]string{{"uvx", "mcp-atlassian"}}},
		{[]string{"/usr/bin/env", "-i", "PATH=/bin", "npx", "-y", "tavily-mcp"}, [][]string{{"npx", "-y", "tavily-mcp"}}},
		{[]string{"env", "-S", "uvx mcp-atlassian==0.23.1"}, [][]string{{"uvx", "mcp-atlassian==0.23.1"}}},
		{[]string{"sh", "-c", "cd /srv && exec uvx mcp-atlassian"}, [][]string{{"cd", "/srv"}, {"uvx", "mcp-atlassian"}}},
		{[]string{"bash", "-lc", "pip install uv; FOO=1 uvx mcp-server-mattermost"}, [][]string{{"pip", "install", "uv"}, {"uvx", "mcp-server-mattermost"}}},
		{[]string{"sh", "-c", "env A=b npx -y slack-mcp-server"}, [][]string{{"npx", "-y", "slack-mcp-server"}}},
		{[]string{"glab", "mcp", "serve"}, [][]string{{"glab", "mcp", "serve"}}},
	} {
		if got := commandsOf(c.line); !slices.EqualFunc(got, c.want, slices.Equal) {
			t.Errorf("%q runs %q, want %q", c.line, got, c.want)
		}
	}
}

// A FENCED BLOCK IS READ WHATEVER ITS LABEL: a configuration in a bare fence,
// a tilde fence or a fence indented under a list item is still a
// configuration somebody copies, and a scan of ```yaml and ```json alone
// passed a launch in any other.
func TestEveryFencedBlockIsFound(t *testing.T) {
	t.Parallel()
	page := strings.Join([]string{
		"Prose with `inline` code.",
		"```",
		"command: npx",
		"```",
		"~~~yaml",
		"a: 1",
		"~~~",
		"1. A step:",
		"",
		"   ```json",
		`   {"command": "uvx"}`,
		"   ```",
		"````markdown",
		"```yaml",
		"inner: true",
		"```",
		"````",
		"```bash",
		"uvx unclosed",
	}, "\n")
	want := []fencedBlock{
		{info: "", text: "command: npx\n"},
		{info: "yaml", text: "a: 1\n"},
		{info: "json", text: `{"command": "uvx"}` + "\n"},
		{info: "markdown", text: "```yaml\ninner: true\n```\n"},
		{info: "bash", text: "uvx unclosed\n"},
	}
	if got := fencedBlocks(page); !slices.Equal(got, want) {
		t.Errorf("fenced blocks = %q\nwant %q", got, want)
	}
}

// documentedConfig is one configuration a page, an example or a skill spells
// out: any fenced block of a page, whatever its label, or a whole .yaml file.
type documentedConfig struct {
	where string
	// info is the fence's label ("yaml", "bash", "" for a bare fence), and
	// "yaml" for a whole .yaml file.
	info string
	text string
}

// fencedBlock is one fenced code block of a page: its label and its text,
// with the fence's own indentation taken off every line.
type fencedBlock struct {
	info string
	text string
}

var fenceOpen = regexp.MustCompile("^( *)(`{3,}|~{3,})[ \t]*([^`\\s]*)[^`]*$")

// fencedBlocks is every fenced block of a page, by CommonMark's rule: a run of
// three or more backticks or tildes opens one, the same character at least as
// many times closes it, and one left open runs to the end of the page.
func fencedBlocks(page string) []fencedBlock {
	var out []fencedBlock
	lines := strings.Split(page, "\n")
	for i := 0; i < len(lines); i++ {
		m := fenceOpen.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		indent, fence, info := len(m[1]), m[2], m[3]
		var body strings.Builder
		for i++; i < len(lines); i++ {
			line := lines[i]
			if trimmed := strings.TrimSpace(line); len(trimmed) >= len(fence) &&
				strings.Trim(trimmed, fence[:1]) == "" {
				break
			}
			// The fence's indentation comes off each line, and no more.
			lead := len(line) - len(strings.TrimLeft(line, " "))
			body.WriteString(line[min(indent, lead):])
			body.WriteString("\n")
		}
		out = append(out, fencedBlock{info: strings.ToLower(info), text: body.String()})
	}
	return out
}

// documentedConfigs is every configuration under docs/, examples/ and
// skills/, read from the tree this test runs in.
func documentedConfigs(t *testing.T) []documentedConfig {
	t.Helper()
	root := sourcetree.Root(t)
	var found []documentedConfig
	for _, dir := range []string{"docs", "examples", "skills"} {
		err := sourcetree.Walk(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			var blocks []fencedBlock
			switch filepath.Ext(p) {
			case ".md":
				body, err := os.ReadFile(p) //nolint:gosec // a page this repository ships
				if err != nil {
					return err
				}
				blocks = fencedBlocks(string(body))
			case ".yaml", ".yml":
				body, err := os.ReadFile(p) //nolint:gosec // an example this repository ships
				if err != nil {
					return err
				}
				blocks = []fencedBlock{{info: "yaml", text: string(body)}}
			default:
				return nil
			}
			rel, _ := filepath.Rel(root, p)
			for i, block := range blocks {
				where := rel
				if len(blocks) > 1 {
					where = fmt.Sprintf("%s, block %d", rel, i+1)
				}
				found = append(found, documentedConfig{where: where, info: block.info, text: block.text})
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
	return found
}

// docSeat and docUnit are as much of a seat and a unit as these checks read,
// decoded leniently because a page's block is often a fragment the loader
// would refuse for reasons that have nothing to do with tool servers.
type docSeat struct {
	Name   string                       `yaml:"name"`
	Kind   string                       `yaml:"kind"`
	Unit   string                       `yaml:"unit"`
	MCPEnv map[string]map[string]string `yaml:"mcp_env"`
}

type docUnit struct {
	Name     string                       `yaml:"name"`
	MCPEnv   map[string]map[string]string `yaml:"mcp_env"`
	Roles    []docSeat                    `yaml:"roles"`
	Children []docUnit                    `yaml:"children"`
}

// docBlock is one configuration a page or an example spells out.
type docBlock struct {
	where      string
	MCPServers []MCPServer `yaml:"mcp_servers"`
	Roles      []docSeat   `yaml:"roles"`
	Units      []docUnit   `yaml:"units"`
}

// configLabels are the fence labels a configuration is written under; a bare
// fence is one too, since nothing about a missing label makes it a shell.
var configLabels = []string{"", "yaml", "yml", "json"}

// documentedBlocks is every documented configuration read the way the loader
// reads one. A configuration block that names one of the pinned servers or a
// slack-mcp-server variable but cannot be read is a failure rather than a
// skip: it is exactly the block nobody would check. A shell or log block that
// names one is prose about it, and its commands are documentedCommands'.
func documentedBlocks(t *testing.T) []docBlock {
	t.Helper()
	var found []docBlock
	for _, c := range documentedConfigs(t) {
		doc := docBlock{where: c.where}
		if err := yaml.Unmarshal([]byte(c.text), &doc); err != nil {
			if slices.Contains(configLabels, c.info) && namesAPinnedServer(c.text) {
				t.Errorf("%s configures a pinned tool server but does not parse: %v", c.where, err)
			}
			continue
		}
		found = append(found, doc)
	}
	return found
}

func namesAPinnedServer(text string) bool {
	names := strings.Contains(text, "SLACK_MCP_")
	for _, pkg := range pinnedToolServers() {
		names = names || strings.Contains(text, pkg.name)
	}
	return names
}

// launch is one stdio launch a document spells: a mapping carrying a
// `command`, wherever it sits — an mcp_servers entry, a fragment of one, an
// API answer reporting one.
type launch struct {
	where string
	name  string
	line  []string
}

// launchKey finds a mapping that declares a command in a block that does not
// parse, so that block is reported rather than skipped.
var launchKey = regexp.MustCompile(`["']?\bcommand["']?\s*:\s*["']?(?:uvx|uv|npx|npm|pipx|pip3?)\b`)

// documentedLaunches is every launch in every documented configuration,
// found by walking the document rather than by its shape, so a launch is
// read wherever a page puts one.
func documentedLaunches(t *testing.T) []launch {
	t.Helper()
	var found []launch
	for _, c := range documentedConfigs(t) {
		dec := yaml.NewDecoder(bytes.NewReader([]byte(c.text)))
		for {
			var node yaml.Node
			err := dec.Decode(&node)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				if launchKey.MatchString(c.text) {
					t.Errorf("%s launches a server through a package runner but does not parse: %v", c.where, err)
				}
				break
			}
			found = append(found, launchesIn(c.where, &node)...)
		}
	}
	return found
}

func launchesIn(where string, node *yaml.Node) []launch {
	var found []launch
	if node.Kind == yaml.AliasNode && node.Alias != nil {
		node = node.Alias
	}
	if node.Kind == yaml.MappingNode {
		var l launch
		declares := false
		for i := 0; i+1 < len(node.Content); i += 2 {
			key, value := node.Content[i].Value, node.Content[i+1]
			switch {
			case key == "command" && value.Kind == yaml.ScalarNode:
				l.line = append([]string{value.Value}, l.line...)
				declares = true
			case key == "args" && value.Kind == yaml.SequenceNode:
				for _, arg := range value.Content {
					l.line = append(l.line, arg.Value)
				}
			case key == "name" && value.Kind == yaml.ScalarNode:
				l.name = value.Value
			}
		}
		if declares {
			l.where = where
			found = append(found, l)
		}
	}
	for _, child := range node.Content {
		found = append(found, launchesIn(where, child)...)
	}
	return found
}

// runnerCommand is a command a page tells somebody to run that fetches a
// package by name, up to where the command visibly ends: a code span's
// backtick, a quote, a table cell, a list separator or the end of a line. It
// never crosses a line, so a YAML `command: uvx` is the launch walk's, not
// this one's.
var runnerCommand = regexp.MustCompile("\\b(?:uvx|uv[ \\t]+(?:tool[ \\t]+(?:run|install)|pip[ \\t]+install)|pipx[ \\t]+(?:run|install)|pip3?[ \\t]+install|npx|npm[ \\t]+(?:exec|x|install|i))[ \\t][^\\n`\"'|,;()]*")

// commandLine is the words of one runner command a page spells, up to where
// the shell would end it: a sentence's end, `&&`, or a redirection.
func commandLine(cmd string) []string {
	if i := strings.Index(cmd, ". "); i >= 0 {
		cmd = cmd[:i]
	}
	var line []string
	for _, f := range strings.Fields(cmd) {
		if f == "&&" || f == "&" || f == "\\" || strings.ContainsAny(f[:1], "<>") ||
			strings.HasPrefix(f, "2>") {
			break
		}
		line = append(line, f)
	}
	// A command that ends a sentence carries its full stop; "." alone is a
	// directory, and keeps it.
	if n := len(line); n > 0 {
		if trimmed := strings.TrimRight(line[n-1], ".:"); trimmed != "" {
			line[n-1] = trimmed
		}
	}
	return line
}

// documentedCommand is one runner command a file spells, where it spells it.
type documentedCommand struct {
	where string
	text  string
	line  []string
}

// documentedCommands is every runner command spelled anywhere in a page, an
// example or a skill — prose, a table, a shell block, a setup step's string.
func documentedCommands(t *testing.T) []documentedCommand {
	t.Helper()
	root := sourcetree.Root(t)
	var found []documentedCommand
	for _, dir := range []string{"docs", "examples", "skills"} {
		err := sourcetree.Walk(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			body, err := os.ReadFile(p) //nolint:gosec // a file this repository ships
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, p)
			for _, cmd := range runnerCommand.FindAllString(string(body), -1) {
				found = append(found, documentedCommand{where: rel, text: strings.TrimSpace(cmd), line: commandLine(cmd)})
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
	return found
}

// EVERY PACKAGE A PAGE FETCHES BY NAME IS AN EXACT RELEASE, AND EVERY SERVER A
// DOCUMENT LAUNCHES IS ONE WHOSE CLAIMS WERE READ.
//
// Every launch a configuration spells — an mcp_servers entry in any fenced
// block of a page or a skill, or an API answer that reports one, and every
// command it runs through `env` or a shell's -c string — and every runner
// command spelled anywhere (a troubleshooting step, a setup string, a shell
// block) names each package it fetches at an exact release, an editable
// install from a URL included: a page says what one release does, and
// `@latest` or a bare name runs one nobody read. The shipped examples are held
// the other way round, by [TestTheShippedExamplesRunTheNewestRelease]. And the
// server a launch runs — its last command — is one of [pinnedToolServers]
// wherever it is launched, an example included, so what the documents say
// about it is tied to the release it was read at.
//
// Not held: `go install …@latest` of Crewlet itself, whose version is its tag
// and which nothing in this tree records (RELEASING.md).
func TestEveryDocumentedLaunchNamesAnExactRelease(t *testing.T) {
	t.Parallel()
	exact := func(spec string) bool {
		_, version := splitRelease(spec)
		return exactRelease.MatchString(version)
	}

	launched := map[string]int{}
	for _, l := range append(documentedLaunches(t), catalogueLaunches()...) {
		commands := commandsOf(l.line)
		for i, command := range commands {
			inv, ok := invocationOf(command)
			if !ok {
				continue
			}
			for _, spec := range inv.packages {
				if !exact(spec) && !shippedExample(l.where) {
					t.Errorf("%s: %q launches %q, which names no exact release of %s", l.where, l.name, strings.Join(l.line, " "), spec)
				}
			}
			if i < len(commands)-1 {
				// A step the launch runs first, not the server.
				continue
			}
			file, _, _ := strings.Cut(l.where, ",")
			launched[file]++
			if inv.run == "" {
				t.Errorf("%s: %q runs %q through a package runner and names no package to run", l.where, l.name, strings.Join(l.line, " "))
			}
			if name, _ := splitRelease(inv.run); inv.run != "" && !isPinnedToolServer(name) {
				t.Errorf("%s: %q launches %s, which the curated MCP catalogue does not pin: read the release "+
					"the documents name (its tools, its variables) against what they say, then catalogue it "+
					"in internal/mcp/catalogue with that Package and Release", l.where, l.name, name)
			}
		}
	}
	for _, name := range shippedCompanies {
		if launched[filepath.Join("examples", name)] == 0 {
			t.Errorf("examples/%s launches no server through a package runner — this check was certifying nothing there", name)
		}
	}

	commands := 0
	for _, c := range documentedCommands(t) {
		inv, ok := invocationOf(c.line)
		if !ok {
			continue
		}
		commands++
		for _, spec := range inv.packages {
			if !exact(spec) && !shippedExample(c.where) {
				t.Errorf("%s tells somebody to run %q, which fetches %s at no exact release", c.where, c.text, spec)
			}
		}
	}
	if commands == 0 {
		t.Fatal("no document spells a runner command — this check was certifying nothing")
	}
}

// ONE RELEASE OF EACH PINNED SERVER, EVERYWHERE IT IS NAMED, AND THE ONE ITS
// CLAIMS WERE READ AGAINST. Every launch of it, every runner command that
// fetches it, every place a page names it with a version in either spelling,
// and a link into its own repository at a tag all name the same release — so a
// bump that reaches the examples and misses a troubleshooting step, or the
// reverse, fails here instead of in front of somebody following that step.
// That each of them names an exact release at all is
// [TestEveryDocumentedLaunchNamesAnExactRelease]'s, and an example running the
// newest names no release to count: that is
// [TestTheShippedExamplesRunTheNewestRelease]'s.
func TestThePinnedToolServersRunOneReleaseEverywhere(t *testing.T) {
	t.Parallel()
	// Every command that fetches a package: each a launch runs, wrappers
	// unwrapped, and each a page spells in prose, a table or a shell block.
	type fetch struct {
		where    string
		packages []string
	}
	var fetches []fetch
	for _, l := range append(documentedLaunches(t), catalogueLaunches()...) {
		for _, command := range commandsOf(l.line) {
			if inv, ok := invocationOf(command); ok {
				fetches = append(fetches, fetch{l.where, inv.packages})
			}
		}
	}
	launched := len(fetches)
	for _, c := range documentedCommands(t) {
		if inv, ok := invocationOf(c.line); ok {
			fetches = append(fetches, fetch{c.where, inv.packages})
		}
	}
	root := sourcetree.Root(t)

	for _, pkg := range pinnedToolServers() {
		t.Run(pkg.name, func(t *testing.T) {
			t.Parallel()
			releases := map[string][]string{}
			entries := 0
			for i, f := range fetches {
				for _, spec := range f.packages {
					name, version := splitRelease(spec)
					if name != pkg.name {
						continue
					}
					if i < launched {
						entries++
					}
					if shippedExample(f.where) && newest(spec) {
						continue
					}
					releases[version] = append(releases[version], f.where)
				}
			}
			// THE CATALOGUE LAUNCHES EVERY PIN ITSELF, so a pin no page
			// launches is still a release somebody is offered, and its
			// own launch is held to it below with every page's.
			if entries == 0 {
				t.Fatalf("the catalogue pins %s and nothing launches it, not even its own entry: "+
					"its launch no longer fetches the package it names", pkg.name)
			}

			// Prose names it too: a troubleshooting step that says what to run,
			// a concept page that says which release a skill was written for,
			// a link to the settings of one tagged release. Under either
			// separator, whichever the runner that launches it takes: uvx
			// reads `pkg@1.2.3` as well as `pkg==1.2.3`, so a mention in the
			// other spelling is still a release somebody would run.
			mention := regexp.MustCompile(regexp.QuoteMeta(pkg.name) + "((?:==|@)([^\\s\"'`\\],)]*))?")
			tagged := regexp.MustCompile(`/` + regexp.QuoteMeta(pkg.name) + `/(?:blob|tree)/v([^/\s)#"'` + "`" + `]+)`)
			for _, dir := range []string{"docs", "examples", "skills"} {
				err := sourcetree.Walk(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
					if err != nil || d.IsDir() {
						return err
					}
					body, err := os.ReadFile(p) //nolint:gosec // a file this repository ships
					if err != nil {
						return err
					}
					rel, _ := filepath.Rel(root, p)
					for _, m := range mention.FindAllStringSubmatch(string(body), -1) {
						if m[1] == "" || (shippedExample(rel) && newest(m[0])) {
							continue
						}
						if !exactRelease.MatchString(m[2]) {
							t.Errorf("%s names %s%s, which is not an exact release", rel, pkg.name, m[1])
							continue
						}
						releases[m[2]] = append(releases[m[2]], rel)
					}
					for _, m := range tagged.FindAllStringSubmatch(string(body), -1) {
						if !exactRelease.MatchString(m[1]) {
							t.Errorf("%s links %s at the tag v%s, which is not an exact release", rel, pkg.name, m[1])
							continue
						}
						releases[m[1]] = append(releases[m[1]], rel)
					}
					return nil
				})
				if err != nil {
					t.Fatalf("walk %s: %v", dir, err)
				}
			}

			if len(releases) > 1 {
				t.Errorf("%s is named at %d releases; every document must name the same one:", pkg.name, len(releases))
				for version, where := range releases {
					slices.Sort(where)
					t.Errorf("  %q in %s", version, strings.Join(slices.Compact(where), ", "))
				}
				return
			}
			for version := range releases {
				if version != pkg.readAt {
					t.Errorf("the docs run %s %s, but what they and the curated catalogue say about it "+
						"was read from %s's source: read %s's (its tools, its variables, and for "+
						"slack-mcp-server the rules slackRefusal restates), correct whatever moved, then set "+
						"its catalogue entry's Release to %s",
						pkg.name, version, pkg.readAt, version, version)
				}
			}
		})
	}
}

// bundledSkillTools is every tool a bundled tool skill's trigger names, with
// the key of a skill naming it: every page under examples/tool-skills/, read
// by the engine's own parser and walked at every depth of its trigger.
func bundledSkillTools(t *testing.T) map[string]string {
	t.Helper()
	pages, err := filepath.Glob(filepath.Join(sourcetree.Root(t), "examples", "tool-skills", "*.md"))
	if err != nil {
		t.Fatalf("list the bundled skills: %v", err)
	}
	tools := map[string]string{}
	var walk func(trigger skills.Trigger, key string)
	walk = func(trigger skills.Trigger, key string) {
		if trigger.Tool != "" {
			tools[trigger.Tool] = key
		}
		for _, sub := range append(slices.Clone(trigger.AnyOf), trigger.AllOf...) {
			walk(sub, key)
		}
	}
	for _, page := range pages {
		body, err := os.ReadFile(page) //nolint:gosec // a skill this repository ships
		if err != nil {
			t.Fatalf("read %s: %v", page, err)
		}
		skill, err := skills.Parse(string(body), skills.Source{})
		if err != nil {
			t.Fatalf("parse %s: %v", page, err)
		}
		walk(skill.Trigger, skill.Key)
	}
	if len(tools) == 0 {
		t.Fatal("no bundled skill triggers on a tool — the examples' one pinned exception was certifying nothing")
	}
	return tools
}

// skillGate is a bundled skill triggering on a tool the entry serves, and the
// tool — as far as the entry itself says what it serves: a name under its
// tool_prefix, or a tool its tool_annotations names, in either spelling an
// override is matched by. Both are empty when no bundled skill names one.
func skillGate(s MCPServer, tools map[string]string) (skill, tool string) {
	for _, name := range slices.Sorted(maps.Keys(tools)) {
		serves := s.ToolPrefix != "" && strings.HasPrefix(name, s.ToolPrefix)
		for key := range s.ToolAnnotations {
			serves = serves || name == key || name == s.ToolPrefix+key
		}
		if serves {
			return tools[name], name
		}
	}
	return "", ""
}

// THE SHIPPED EXAMPLES RUN THE NEWEST RELEASE OF EVERYTHING THEY FETCH, BUT A
// SERVER A BUNDLED SKILL TRIGGERS ON BY TOOL NAME.
//
// An example is a company somebody copies and runs long after it was written,
// and a pin in it is a release nothing moves — no manifest holds it, so no
// pull request ever proposes the next one — handing every new reader a
// release that only gets older. So every package an example fetches by name,
// through a launch or a command it tells somebody to run, names no release
// (`@latest` at most) and runs the newest on the day it is copied.
//
// The exception is the one way a newer release fails there without a sound: a
// bundled tool skill (examples/tool-skills/) triggering on a tool the server
// serves, which a release renaming that tool disarms. Such a server's entry
// names the exact release the skill was read against, and only while a skill
// still names one of its tools. WHICH SERVERS THOSE ARE IS DERIVED, never
// listed here: every tool any bundled skill's trigger names
// ([bundledSkillTools]), matched to the server whose launch serves it
// ([skillGate]) — a list kept in this comment had already drifted, naming two
// of the four skills that trigger on one server's tools. Which release that
// is, is [TestThePinnedToolServersRunOneReleaseEverywhere]'s.
func TestTheShippedExamplesRunTheNewestRelease(t *testing.T) {
	t.Parallel()
	tools := bundledSkillTools(t)
	entries := map[string]map[string]MCPServer{} // where -> name -> entry
	for _, block := range documentedBlocks(t) {
		if !shippedExample(block.where) {
			continue
		}
		for _, s := range block.MCPServers {
			if entries[block.where] == nil {
				entries[block.where] = map[string]MCPServer{}
			}
			entries[block.where][s.Name] = s
		}
	}

	servers := 0
	for _, l := range documentedLaunches(t) {
		if !shippedExample(l.where) {
			continue
		}
		var skill, tool string
		if s, ok := entries[l.where][l.name]; ok {
			skill, tool = skillGate(s, tools)
		}
		commands := commandsOf(l.line)
		for i, command := range commands {
			inv, ok := invocationOf(command)
			if !ok {
				continue
			}
			for _, spec := range inv.packages {
				server := i == len(commands)-1 && spec == inv.run
				if server {
					servers++
				}
				switch _, version := splitRelease(spec); {
				case server && skill != "":
					if !exactRelease.MatchString(version) {
						t.Errorf("%s: %q launches %s at no exact release, and the bundled skill %s triggers on "+
							"its tool %s: a release that renamed it would disarm the skill without a sound, "+
							"so name the release the skill was read against", l.where, l.name, spec, skill, tool)
					}
				case !newest(spec):
					t.Errorf("%s: %q fetches %s at a fixed release, where an example runs the newest of "+
						"what it fetches (no version, or @latest) because nothing moves a pin there — only "+
						"a server a bundled skill triggers on by tool name is pinned, and this is not one",
						l.where, l.name, spec)
				}
			}
		}
	}
	if servers == 0 {
		t.Fatal("no shipped example launches a server through a package runner — this check was certifying nothing")
	}

	for _, c := range documentedCommands(t) {
		if !shippedExample(c.where) {
			continue
		}
		inv, ok := invocationOf(c.line)
		if !ok {
			continue
		}
		for _, spec := range inv.packages {
			if !newest(spec) {
				t.Errorf("%s tells somebody to run %q, which fetches %s at a fixed release, where an "+
					"example runs the newest of what it fetches (no version, or @latest) because "+
					"nothing moves a pin there", c.where, c.text, spec)
			}
		}
	}
}

// slackWriteTool is one of slack-mcp-server's write tools: what registers it
// and what lets a call through, at the release readAt names.
type slackWriteTool struct {
	name string
	// gate is the variable that switches it on, and that the call path reads
	// as a channel policy.
	gate string
	// listedAs are the names whose presence ANYWHERE in SLACK_MCP_ENABLED_TOOLS
	// lets a call through when the gate is unset: the call path tests the
	// variable's text with strings.Contains, and either reaction tool's name
	// admits both.
	listedAs []string
}

var slackWriteTools = []slackWriteTool{
	{"conversations_add_message", "SLACK_MCP_ADD_MESSAGE_TOOL", []string{"conversations_add_message"}},
	{"reactions_add", "SLACK_MCP_REACTION_TOOL", []string{"reactions_add", "reactions_remove"}},
	{"reactions_remove", "SLACK_MCP_REACTION_TOOL", []string{"reactions_add", "reactions_remove"}},
}

// slackSeatChannel stands for whichever channel a seat is addressed in: an id
// no document names, since a seat answers wherever it was woken and its entry
// cannot know where that will be.
const slackSeatChannel = "C0SEATCHANNEL"

// slackRefusal is why slack-mcp-server, started from an entry with this env
// and these arguments, would NOT carry out tool in the channel a seat was
// addressed in — or "" when it would. The server decides it in two places,
// and both must say yes:
//
//   - REGISTRATION, in pkg/server/server.go (shouldAddTool), from the enabled
//     tools list — `-e` / `--enabled-tools` on its command line, else
//     SLACK_MCP_ENABLED_TOOLS — which decides alone when it names anything;
//     with no list, the tool exists only when its gate variable is set at all.
//   - THE CALL, in pkg/handler/conversations.go (parseParamsToolAddMessage,
//     parseParamsToolReaction), from the ENVIRONMENT ALONE: the gate, or with
//     no gate SLACK_MCP_ENABLED_TOOLS naming the tool, and then the gate read
//     as a channel policy (isChannelAllowedForConfig).
//
// So a list given only on the command line registers a tool that refuses
// every call, and a gate of "false" is a one-channel allowlist naming a
// channel that does not exist: both list the tool and post nothing, which is
// the failure this models. A configuration the server refuses to START with —
// a tool name it does not know, a gate mixing listed and excluded channels —
// ends the child at once, the engine reports it failed, and it is left to
// that; the flags are parsed here only because the list they carry decides
// registration, by the flag package the server parses them with, so a command
// line it would refuse is refused here too.
func slackRefusal(env map[string]string, argv []string, tool slackWriteTool) string {
	flags := flag.NewFlagSet("slack-mcp-server", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var transport, enabledFlag string
	flags.StringVar(&transport, "t", "stdio", "")
	flags.StringVar(&transport, "transport", "stdio", "")
	flags.StringVar(&enabledFlag, "e", "", "")
	flags.StringVar(&enabledFlag, "enabled-tools", "", "")
	flags.Bool("no-cache", false, "")
	if err := flags.Parse(argv); err != nil {
		return "the server refuses its own command line and exits: " + err.Error()
	}

	list := enabledFlag
	if list == "" {
		list = env["SLACK_MCP_ENABLED_TOOLS"]
	}
	var enabled []string
	for item := range strings.SplitSeq(list, ",") {
		if item = strings.TrimSpace(item); item != "" {
			enabled = append(enabled, item)
		}
	}
	switch {
	case len(enabled) > 0 && !slices.Contains(enabled, tool.name):
		return "never registers " + tool.name + ": the enabled-tools list leaves it out"
	case len(enabled) == 0 && env[tool.gate] == "":
		return "never registers " + tool.name + ": set " + tool.gate + ` to "true" in its env`
	}

	policy := env[tool.gate]
	if policy == "" {
		if !slices.ContainsFunc(tool.listedAs, func(name string) bool {
			return strings.Contains(env["SLACK_MCP_ENABLED_TOOLS"], name)
		}) {
			return "registers " + tool.name + " and refuses every call: the call reads SLACK_MCP_ENABLED_TOOLS " +
				"from the environment and never the --enabled-tools flag, so set " + tool.gate + ` to "true" in its env`
		}
		policy = "true"
	}
	if !slackChannelAllowed(slackSeatChannel, policy) {
		return "registers " + tool.name + " and refuses it in any channel " + tool.gate + "=" + policy +
			` does not name, which is wherever a seat is addressed: set it to "true"`
	}
	return ""
}

// slackChannelAllowed is the server's isChannelAllowedForConfig: a gate of
// "", "true" or "1" admits every channel; otherwise it is a comma-separated
// list of channel ids admitted, or, when its first entry is `!`-prefixed,
// excluded.
func slackChannelAllowed(channel, policy string) bool {
	if policy == "" || policy == "true" || policy == "1" {
		return true
	}
	items := strings.Split(policy, ",")
	negated := strings.HasPrefix(strings.TrimSpace(items[0]), "!")
	for _, item := range items {
		item = strings.TrimSpace(item)
		if negated && strings.TrimPrefix(item, "!") == channel {
			return false
		}
		if !negated && item == channel {
			return true
		}
	}
	return negated
}

// THE MODEL READS THE SERVER'S RULES, NOT A CONVENTION. Each case is one the
// server's own source decides (pkg/server/server.go, pkg/handler/
// conversations.go at readAt) and the documents could plausibly write; the
// ones that refuse are the shapes that list the tool and post nothing.
func TestTheSlackModelReadsTheServersRules(t *testing.T) {
	t.Parallel()
	post := slackWriteTools[0]
	for _, c := range []struct {
		name  string
		env   map[string]string
		argv  []string
		posts bool
	}{
		{"the gate switched on", map[string]string{post.gate: "true"}, nil, true},
		{"the gate as 1", map[string]string{post.gate: "1"}, nil, true},
		{"nothing switched on", nil, nil, false},
		{"a list naming it in the environment", map[string]string{"SLACK_MCP_ENABLED_TOOLS": "conversations_history,conversations_add_message"}, nil, true},
		{"a list naming it only on the command line", nil, []string{"--enabled-tools", "conversations_history,conversations_add_message"}, false},
		{"a list naming it only on the command line, short", nil, []string{"-e=conversations_add_message"}, false},
		{"a list on the command line and the gate", map[string]string{post.gate: "true"}, []string{"--enabled-tools", "conversations_add_message"}, true},
		{"a list leaving it out, with the gate", map[string]string{post.gate: "true", "SLACK_MCP_ENABLED_TOOLS": "conversations_history"}, nil, false},
		{"the gate as false", map[string]string{post.gate: "false"}, nil, false},
		{"the gate listing other channels", map[string]string{post.gate: "C0123456789,D0987654321"}, nil, false},
		{"the gate excluding another channel", map[string]string{post.gate: "!C0123456789"}, nil, true},
		{"the gate excluding the seat's channel", map[string]string{post.gate: "!" + slackSeatChannel}, nil, false},
		{"a flag the server does not define", map[string]string{post.gate: "true"}, []string{"--read-only"}, false},
		{"the transport flags", map[string]string{post.gate: "true"}, []string{"--transport", "stdio", "-no-cache"}, true},
	} {
		if got := slackRefusal(c.env, c.argv, post) == ""; got != c.posts {
			t.Errorf("%s: model says posts=%v (%q), the server's rules say %v",
				c.name, got, slackRefusal(c.env, c.argv, post), c.posts)
		}
	}
	// Either reaction's name in the environment's list lets both through.
	react := slackWriteTools[2]
	if r := slackRefusal(map[string]string{"SLACK_MCP_ENABLED_TOOLS": "reactions_remove,reactions_add"}, nil, react); r != "" {
		t.Errorf("a list naming both reactions: %s", r)
	}
}

// EVERY SLACK TOOL SERVER A DOCUMENT CONFIGURES CAN POST, AND REACT, wherever
// its seat is addressed.
//
// conversations_add_message is the one tool an agent on Slack answers with;
// reactions_add and reactions_remove are what the manifest's reactions:write
// is requested for. Each is off unless the entry switches it on, and a page
// that switches it on the wrong way builds a seat that reads every message
// and answers none, with the server reporting itself healthy throughout. The
// switches are read from the server entry and not from a seat's mcp_env: they
// are not credentials, and one set in a single seat's mcp_env leaves every
// other seat mute.
func TestEverySlackToolServerCanPost(t *testing.T) {
	t.Parallel()
	entries := 0
	for _, block := range documentedBlocks(t) {
		for _, s := range block.MCPServers {
			spec, argv, ok := launchedPackage(s)
			if !ok {
				continue
			}
			if name, _ := splitRelease(spec); name != "slack-mcp-server" {
				continue
			}
			entries++
			for _, tool := range slackWriteTools {
				if why := slackRefusal(s.Env, argv, tool); why != "" {
					t.Errorf("%s: mcp_servers %q %s", block.where, s.Name, why)
				}
			}
		}
	}
	catalogued := 0
	for where, s := range catalogueServers() {
		spec, argv, ok := launchedPackage(s)
		if name, _ := splitRelease(spec); !ok || name != "slack-mcp-server" {
			continue
		}
		catalogued++
		for _, tool := range slackWriteTools {
			if why := slackRefusal(s.Env, argv, tool); why != "" {
				t.Errorf("%s: the drafted entry %s", where, why)
			}
		}
	}
	if entries == 0 || catalogued == 0 {
		t.Fatalf("%d documents and %d catalogue entries configure slack-mcp-server: this check was "+
			"certifying nothing for one of them", entries, catalogued)
	}
}

// underAnnotatedReads are the pinned servers that advertise no annotation on
// a tool at readAt, with every tool each lists — all of which only read.
// tavily-mcp 0.2.22, run through initialize and tools/list, lists these five
// with no annotations; build/index.js has each search, extract, crawl, map or
// research the web and write nothing.
var underAnnotatedReads = map[string][]string{
	"tavily-mcp": {"tavily_search", "tavily_extract", "tavily_crawl", "tavily_map", "tavily_research"},
}

// EVERY ENTRY FOR A SERVER THAT ADVERTISES NO ANNOTATIONS SAYS ITS READS ARE
// READS.
//
// The engine reads a tool's hints off the wire and trusts only an explicit
// one, so an unannotated tool is not proven read-only, and the delivery check
// counts every such tool as a possible delivery: a turn that only searched the
// web can pass it as one that reached somebody. The server will not say, so
// each documented entry for it must, under the tool's own name or with the
// entry's tool_prefix — the two spellings an override is matched by.
func TestEveryUnderAnnotatedServerHasItsReadsMarked(t *testing.T) {
	t.Parallel()
	entries := 0
	for _, block := range documentedBlocks(t) {
		for _, s := range block.MCPServers {
			spec, _, ok := launchedPackage(s)
			if !ok {
				continue
			}
			name, _ := splitRelease(spec)
			reads, under := underAnnotatedReads[name]
			if !under {
				continue
			}
			entries++
			for _, tool := range reads {
				hints, ok := s.ToolAnnotations[tool]
				if !ok {
					hints, ok = s.ToolAnnotations[s.ToolPrefix+tool]
				}
				if !ok || !hints.ReadOnly.IsSet() || !hints.ReadOnly.Or(false) {
					t.Errorf("%s: mcp_servers %q runs %s, which advertises no annotation, and does not mark "+
						"%s read_only: true, so it counts as a possible delivery", block.where, s.Name, name, tool)
				}
			}
		}
	}
	catalogued := 0
	for where, s := range catalogueServers() {
		spec, _, ok := launchedPackage(s)
		name, _ := splitRelease(spec)
		reads, under := underAnnotatedReads[name]
		if !ok || !under {
			continue
		}
		catalogued++
		for _, tool := range reads {
			hints, ok := s.ToolAnnotations[tool]
			if !ok || !hints.ReadOnly.Or(false) {
				t.Errorf("%s runs %s, which advertises no annotation, and its tool_annotations do not "+
					"mark %s read_only, so a server drafted from it counts the tool as a possible delivery",
					where, name, tool)
			}
		}
	}
	if entries == 0 || catalogued == 0 {
		t.Fatalf("%d documents and %d catalogue entries configure a server that advertises no "+
			"annotations: this check was certifying nothing for one of them", entries, catalogued)
	}
}

// slackTokenVars are the variables slack-mcp-server authenticates with at
// readAt (pkg/provider/api.go): a bot token, a user token, or a browser
// session's pair. A seat's mcp_env block carrying one is that server's
// credential, whatever the server entry is called.
var slackTokenVars = []string{"SLACK_MCP_XOXB_TOKEN", "SLACK_MCP_XOXP_TOKEN", "SLACK_MCP_XOXC_TOKEN", "SLACK_MCP_XOXD_TOKEN"}

// seatServer is one tool server one agent seat starts, with the environment
// it starts with: the entry's, under the seat's unit's mcp_env for it, under
// the seat's own.
type seatServer struct {
	seat   docSeat
	server string
	// entry is the server's mcp_servers entry in the same block, nil when the
	// block is a fragment that declares the seats alone.
	entry *MCPServer
	env   map[string]string
}

// seatServers is every tool server every agent seat of a documented block
// starts, the environment layered as the engine layers it: the entry's
// under its unit's under its own. Only a unit's direct seats inherit the
// unit's block, and a human seat starts nothing. A unit with an mcp_env of
// its own also stands for the direct seat that sets nothing — the one the
// block exists for, whether or not the page goes on to declare it.
func seatServers(block docBlock) []seatServer {
	entries := map[string]*MCPServer{}
	for i := range block.MCPServers {
		entries[block.MCPServers[i].Name] = &block.MCPServers[i]
	}
	units := map[string]docUnit{}
	type placed struct {
		seat docSeat
		unit docUnit
	}
	var all []placed
	var walk func([]docUnit)
	walk = func(list []docUnit) {
		for _, u := range list {
			if _, seen := units[u.Name]; !seen {
				units[u.Name] = u
			}
			for _, r := range u.Roles {
				all = append(all, placed{r, u})
			}
			if len(u.MCPEnv) > 0 {
				all = append(all, placed{docSeat{Name: "any direct seat of " + u.Name + " that sets none"}, u})
			}
			walk(u.Children)
		}
	}
	walk(block.Units)
	for _, r := range block.Roles {
		all = append(all, placed{r, units[r.Unit]})
	}

	var out []seatServer
	for _, p := range all {
		if p.seat.Kind == "human" {
			continue
		}
		servers := map[string]bool{}
		for name := range p.seat.MCPEnv {
			servers[name] = true
		}
		for name := range p.unit.MCPEnv {
			servers[name] = true
		}
		for _, server := range slices.Sorted(maps.Keys(servers)) {
			env := map[string]string{}
			var entryEnv map[string]string
			if e := entries[server]; e != nil {
				entryEnv = e.Env
			}
			for _, layer := range []map[string]string{entryEnv, p.unit.MCPEnv[server], p.seat.MCPEnv[server]} {
				maps.Copy(env, layer)
			}
			out = append(out, seatServer{seat: p.seat, server: server, entry: entries[server], env: env})
		}
	}
	return out
}

// EVERY SLACK SEAT A DOCUMENT CONFIGURES KEEPS A CHANNEL CACHE OF ITS OWN.
//
// slack-mcp-server keeps the channels its token can see in a file named after
// the WORKSPACE alone and serves it for a day, and every seat's server on a
// node inherits one environment, so seats that name no cache share one file:
// whichever bot wrote it last tells the others its private channels and DMs
// and hides their own. What is held is the property that leak needs broken —
// no cache file is read by two Slack identities — over the env each seat's
// server actually starts with: the entry's, under its unit's, under its own.
func TestEverySlackSeatKeepsItsOwnChannelCache(t *testing.T) {
	t.Parallel()
	seats := 0
	for _, block := range documentedBlocks(t) {
		readers := map[string]string{} // cache path -> the identity reading it
		for _, s := range seatServers(block) {
			var identity []string
			for _, v := range slackTokenVars {
				if s.env[v] != "" {
					identity = append(identity, v+"="+s.env[v])
				}
			}
			if len(identity) == 0 {
				continue
			}
			seats++
			cache := s.env["SLACK_MCP_CHANNELS_CACHE"]
			if cache == "" {
				t.Errorf("%s: seat %q starts slack-mcp-server (mcp_env.%s) with no SLACK_MCP_CHANNELS_CACHE, "+
					"so it shares the workspace's one channel cache with every other Slack seat on its node",
					block.where, s.seat.Name, s.server)
				continue
			}
			who := strings.Join(identity, " ")
			if other, ok := readers[cache]; ok && other != who {
				t.Errorf("%s: seat %q reads the channel cache %s that another Slack identity (%s) writes",
					block.where, s.seat.Name, cache, other)
			}
			readers[cache] = who
		}
	}
	if seats == 0 {
		t.Fatal("no document configures a Slack seat — this check was certifying nothing")
	}
}

// atlassianProducts are the two products mcp-atlassian serves and the pair it
// authenticates each with by an API token at readAt: JiraConfig.from_env and
// ConfluenceConfig.from_env read the token only beside the account's username.
var atlassianProducts = []struct{ name, username, token string }{
	{"Jira", "JIRA_USERNAME", "JIRA_API_TOKEN"},
	{"Confluence", "CONFLUENCE_USERNAME", "CONFLUENCE_API_TOKEN"},
}

// atlassianServerNames are the mcp_env keys the documents give mcp-atlassian
// where a block declares no entry for them: the shared `atlassian` entry every
// page runs, or one product's alone.
var atlassianServerNames = []string{"atlassian", "jira", "confluence"}

// EVERY ATLASSIAN SEAT A DOCUMENT CONFIGURES NAMES ITS TOKEN'S ACCOUNT.
//
// mcp-atlassian takes an API token only beside the account it belongs to: a
// token alone raises on Cloud and on a Data Center with no personal access
// token, the server logs that to the stderr only the engine's log sees, and
// starts with none of that product's tools — reported by the engine as a
// server that started. Run against a Cloud address at readAt it lists no tool
// at all without JIRA_USERNAME and 63 Jira tools with it. So a seat copied
// from a page that forgot the username can neither read nor write an issue,
// and on Cloud the engine cannot resolve its account either. Held over the
// env each seat's server starts with — the entry's, under its unit's, under
// its own — so a unit's address and a seat's token count together.
func TestEveryAtlassianSeatNamesItsTokensAccount(t *testing.T) {
	t.Parallel()
	seats := 0
	for _, block := range documentedBlocks(t) {
		for _, s := range seatServers(block) {
			if s.entry != nil {
				spec, _, ok := launchedPackage(*s.entry)
				if name, _ := splitRelease(spec); !ok || name != "mcp-atlassian" {
					continue
				}
			} else if !slices.Contains(atlassianServerNames, s.server) {
				continue
			}
			for _, p := range atlassianProducts {
				user, token := s.env[p.username] != "", s.env[p.token] != ""
				if !user && !token {
					continue
				}
				seats++
				switch {
				case token && !user:
					t.Errorf("%s: seat %q gives mcp-atlassian (mcp_env.%s) %s and no %s, so it starts with no %s tool",
						block.where, s.seat.Name, s.server, p.token, p.username, p.name)
				case user && !token:
					t.Errorf("%s: seat %q gives mcp-atlassian (mcp_env.%s) %s and no %s, so it starts with no %s tool",
						block.where, s.seat.Name, s.server, p.username, p.token, p.name)
				}
			}
		}
	}
	if seats == 0 {
		t.Fatal("no document gives an Atlassian seat a credential — this check was certifying nothing")
	}
}
