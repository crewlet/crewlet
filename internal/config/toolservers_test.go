package config

import (
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"

	"gopkg.in/yaml.v3"
)

// The tool servers the docs and the examples hand an operator are held here,
// because each way they go wrong is SILENT: the server starts, reports itself
// healthy, and the company it serves quietly loses something. A floating
// version is a build nobody can name afterwards, and a release that renames a
// tool disarms every skill triggered on the old name without a sound. And
// slack-mcp-server answers no post unless its entry asks it to, so a seat
// built from a page that forgot reads every message and answers none. Nothing
// in the engine can catch either — it is tool-agnostic by design and never
// names a server — so the documents are checked where they are written.

// pinnedToolServers are the servers whose RELEASE the docs and examples
// promise: the bundled tool skills trigger on mcp-server-mattermost's tool
// names, and slack-mcp-server's write tools exist only under switches that a
// release may rename. Each must be named at one exact release everywhere.
var pinnedToolServers = []struct {
	name string
	// sep is how the package runner that launches it spells a version:
	// uvx takes `pkg==1.2.3`, npm `pkg@1.2.3`.
	sep string
	// readAt is the release whose source what the docs say about this server
	// was read against — for mcp-server-mattermost the tool names the bundled
	// skills trigger on and the variables its page names, for
	// slack-mcp-server the switches, the channel cache and the rules
	// [slackRefusal] restates. A bump that reaches the docs fails here until
	// somebody reads the new release and moves this, because every one of
	// those claims is about one release and none of them fails on its own.
	readAt string
}{
	{"mcp-server-mattermost", "==", "0.6.1"},
	{"slack-mcp-server", "@", "1.3.0"},
}

var exactRelease = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

// launchedPackage is the package a package-runner command starts, as the
// runner was handed it, the arguments the runner hands that package, and
// whether the command is a package runner at all. A server launched any
// other way (glab, a binary on PATH, an http URL) has no version in its
// config to pin.
func launchedPackage(s MCPServer) (spec string, argv []string, ok bool) {
	args := s.Args
	switch s.Command {
	case "uvx", "npx":
	case "npm":
		if len(args) == 0 || args[0] != "exec" {
			return "", nil, false
		}
		args = args[1:]
		// npm exec takes its package after `--` when flags precede it.
		if i := slices.Index(args, "--"); i >= 0 {
			args = args[i+1:]
		}
	default:
		return "", nil, false
	}
	for i, a := range args {
		if !strings.HasPrefix(a, "-") {
			return a, args[i+1:], true
		}
	}
	return "", nil, true
}

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

// EVERY SERVER A SHIPPED EXAMPLE LAUNCHES THROUGH A PACKAGE RUNNER NAMES AN
// EXACT RELEASE. The examples are what an operator copies and runs, and every
// other dependency surface in this repository is pinned; `@latest` here is a
// working run nobody can reproduce.
func TestShippedExamplesPinEveryToolServerTheyLaunch(t *testing.T) {
	t.Parallel()
	for _, name := range shippedCompanies {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg, err := ParseCompany(readRepoFile(t, "examples", name))
			if err != nil {
				t.Fatalf("examples/%s no longer loads:\n%v", name, err)
			}
			launched := 0
			for _, s := range cfg.MCPServers {
				spec, _, ok := launchedPackage(s)
				if !ok {
					continue
				}
				launched++
				if _, version := splitRelease(spec); !exactRelease.MatchString(version) {
					t.Errorf("mcp_servers %q launches %q, which names no exact release", s.Name, spec)
				}
			}
			if launched == 0 {
				t.Fatal("the example launches no server through a package runner — this check was certifying nothing")
			}
		})
	}
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

// documentedBlocks is every configuration the docs, the examples and the
// skills spell out, read the way the loader reads one. A block that names one
// of the pinned servers or a slack-mcp-server variable but cannot be read is
// a failure rather than a skip: it is exactly the block nobody would check.
func documentedBlocks(t *testing.T) []docBlock {
	t.Helper()
	root := sourcetree.Root(t)
	var found []docBlock
	for _, dir := range []string{"docs", "examples", "skills"} {
		err := sourcetree.Walk(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			var blocks []string
			switch filepath.Ext(p) {
			case ".md":
				body, err := os.ReadFile(p) //nolint:gosec // a page this repository ships
				if err != nil {
					return err
				}
				for _, m := range yamlBlockRE.FindAllStringSubmatch(string(body), -1) {
					blocks = append(blocks, m[1])
				}
			case ".yaml", ".yml":
				body, err := os.ReadFile(p) //nolint:gosec // an example this repository ships
				if err != nil {
					return err
				}
				blocks = append(blocks, string(body))
			default:
				return nil
			}
			rel, _ := filepath.Rel(root, p)
			for i, block := range blocks {
				names := strings.Contains(block, "SLACK_MCP_")
				for _, pkg := range pinnedToolServers {
					names = names || strings.Contains(block, pkg.name)
				}
				doc := docBlock{where: rel}
				if len(blocks) > 1 {
					doc.where = fmt.Sprintf("%s, YAML block %d", rel, i+1)
				}
				if err := yaml.Unmarshal([]byte(block), &doc); err != nil {
					if names {
						t.Errorf("%s configures a pinned tool server but does not parse: %v", doc.where, err)
					}
					continue
				}
				found = append(found, doc)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
	return found
}

// runnerCommand is a command a page tells somebody to run that fetches a
// package by name, up to where the command visibly ends: a code span's
// backtick, a quote, a table cell, a list separator or the end of a sentence.
var runnerCommand = regexp.MustCompile("\\b(?:uvx|uv\\s+tool\\s+(?:run|install)|pipx\\s+(?:run|install)|pip3?\\s+install|npx|npm\\s+(?:exec|x|install|i))\\s[^\\n`\"'|,;()]*")

// ONE RELEASE OF EACH PINNED SERVER, EVERYWHERE IT IS NAMED, AND THE ONE ITS
// CLAIMS WERE READ AGAINST. Every entry that launches one names an exact
// release, every runner command a page spells in prose does too, a link into
// the server's own repository at a tag counts as naming that release, and
// they all name the same one — so a bump that reaches the examples and misses
// a troubleshooting step, or the reverse, fails here instead of in front of
// somebody following that step.
func TestThePinnedToolServersRunOneReleaseEverywhere(t *testing.T) {
	t.Parallel()
	blocks := documentedBlocks(t)
	root := sourcetree.Root(t)

	for _, pkg := range pinnedToolServers {
		t.Run(pkg.name, func(t *testing.T) {
			t.Parallel()
			releases := map[string][]string{}
			entries := 0
			for _, block := range blocks {
				for _, s := range block.MCPServers {
					spec, _, ok := launchedPackage(s)
					if !ok {
						continue
					}
					name, version := splitRelease(spec)
					if name != pkg.name {
						continue
					}
					entries++
					if !exactRelease.MatchString(version) {
						t.Errorf("%s: mcp_servers %q launches %q, which names no exact release", block.where, s.Name, spec)
						continue
					}
					releases[version] = append(releases[version], block.where)
				}
			}
			if entries == 0 {
				t.Fatalf("no document configures %s — this check was certifying nothing", pkg.name)
			}

			// Prose names it too: a troubleshooting step that says what to run,
			// a concept page that says which release a skill was written for,
			// a link to the settings of one tagged release.
			mention := regexp.MustCompile(regexp.QuoteMeta(pkg.name) + "(" + regexp.QuoteMeta(pkg.sep) + "([^\\s\"'`\\],)]*))?")
			tagged := regexp.MustCompile(`/` + regexp.QuoteMeta(pkg.name) + `/(?:blob|tree)/v([^/\s)#"'` + "`" + `]+)`)
			pinned := pkg.name + pkg.sep
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
						if m[1] == "" {
							continue
						}
						if !exactRelease.MatchString(m[2]) {
							t.Errorf("%s names %s%s%s, which is not an exact release", rel, pkg.name, pkg.sep, m[2])
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
					// A command that fetches the package by name names a release
					// somewhere in it: `uvx --from pkg==1.2.3 pkg` names it in its
					// first token and runs it by its bare name in its second.
					for _, cmd := range runnerCommand.FindAllString(string(body), -1) {
						if i := strings.Index(cmd, ". "); i >= 0 {
							cmd = cmd[:i]
						}
						fields := strings.Fields(cmd)
						if slices.Contains(fields, pkg.name) && !slices.ContainsFunc(fields, func(f string) bool {
							return strings.HasPrefix(f, pinned)
						}) {
							t.Errorf("%s tells somebody to run %s with no release: %q", rel, pkg.name, strings.TrimSpace(cmd))
						}
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
					t.Errorf("  %s in %s", version, strings.Join(slices.Compact(where), ", "))
				}
				return
			}
			for version := range releases {
				if version != pkg.readAt {
					t.Errorf("the docs run %s %s, but what they say about it was read from %s's source: "+
						"read %s's (its tools, its variables, and for slack-mcp-server the rules slackRefusal "+
						"restates), correct whatever moved, then set readAt to %s",
						pkg.name, version, pkg.readAt, version, version)
				}
			}
		})
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
	if entries == 0 {
		t.Fatal("no document configures slack-mcp-server — this check was certifying nothing")
	}
}

// slackTokenVars are the variables slack-mcp-server authenticates with at
// readAt (pkg/provider/api.go): a bot token, a user token, or a browser
// session's pair. A seat's mcp_env block carrying one is that server's
// credential, whatever the server entry is called.
var slackTokenVars = []string{"SLACK_MCP_XOXB_TOKEN", "SLACK_MCP_XOXP_TOKEN", "SLACK_MCP_XOXC_TOKEN", "SLACK_MCP_XOXD_TOKEN"}

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
		entryEnv := map[string]map[string]string{}
		for _, s := range block.MCPServers {
			entryEnv[s.Name] = s.Env
		}
		units := map[string]docUnit{}
		var walk func([]docUnit, func(docSeat, docUnit))
		walk = func(list []docUnit, visit func(docSeat, docUnit)) {
			for _, u := range list {
				if _, seen := units[u.Name]; !seen {
					units[u.Name] = u
				}
				for _, r := range u.Roles {
					visit(r, u)
				}
				walk(u.Children, visit)
			}
		}
		type placed struct {
			seat docSeat
			unit docUnit
		}
		var all []placed
		walk(block.Units, func(r docSeat, u docUnit) { all = append(all, placed{r, u}) })
		for _, r := range block.Roles {
			all = append(all, placed{r, units[r.Unit]})
		}

		readers := map[string]string{} // cache path -> the identity reading it
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
			for server := range servers {
				env := map[string]string{}
				for _, layer := range []map[string]string{entryEnv[server], p.unit.MCPEnv[server], p.seat.MCPEnv[server]} {
					for k, v := range layer {
						env[k] = v
					}
				}
				var identity []string
				for _, v := range slackTokenVars {
					if env[v] != "" {
						identity = append(identity, v+"="+env[v])
					}
				}
				if len(identity) == 0 {
					continue
				}
				seats++
				cache := env["SLACK_MCP_CHANNELS_CACHE"]
				if cache == "" {
					t.Errorf("%s: seat %q starts slack-mcp-server (mcp_env.%s) with no SLACK_MCP_CHANNELS_CACHE, "+
						"so it shares the workspace's one channel cache with every other Slack seat on its node",
						block.where, p.seat.Name, server)
					continue
				}
				who := strings.Join(identity, " ")
				if other, ok := readers[cache]; ok && other != who {
					t.Errorf("%s: seat %q reads the channel cache %s that another Slack identity (%s) writes",
						block.where, p.seat.Name, cache, other)
				}
				readers[cache] = who
			}
		}
	}
	if seats == 0 {
		t.Fatal("no document configures a Slack seat — this check was certifying nothing")
	}
}
