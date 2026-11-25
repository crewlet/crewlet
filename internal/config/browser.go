package config

import (
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/browser"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
	"github.com/crewlet/crewlet/internal/words"
)

// The words a person is shown for each refusal of a browser seat.
const (
	outsideContainerSaid = "This agent's browser runs only in a container, and this agent can run " +
		"outside one. Run it in a container, or turn its browser off."
	hostNetworkSaid = "This agent's browser could share the host machine's network. Take its " +
		"container off the host's network, or turn its browser off."
	webDeniedSaid = "This agent's model has the web turned off, and a browser would turn it back " +
		"on. Turn the web on for its model, or turn its browser off."
)

// validateBrowserSeats holds each seat with sandbox.browser to where its
// browser may run, and local.chromium_sandbox to a seat that reads it. Every
// rule is an admission rule (the package doc's "A seat's browser").
//
// A seat is refused at its sandbox.browser where a box its executor can launch
// in ([Company.SeatBoxes]) is outside a container, by one comparison for every
// other cell; where that container joins its host's network
// ([hostNetwork]); or where an entry it can launch on denies the web.
// One problem per seat and cause, naming the first entry that reaches it, as
// [Company.validateSetupFileHomes] does.
func (c *Company) validateBrowserSeats() error {
	var p problems
	local := c.Providers.Sandbox.localBlock()
	var joins *hostNetworkSetting
	if local != nil {
		joins = hostNetwork(local.Network, local.RunArgs)
	}
	boxes := map[*Role][]SeatBox{}
	for box := range c.SeatBoxes() {
		boxes[box.Seat] = append(boxes[box.Seat], box)
	}
	read := false
	for seat := range c.executorSeats() {
		if seat.role.Sandbox == nil || !seat.role.Sandbox.Browser {
			continue
		}
		here := at(seat.path, "sandbox.browser")
		var outside, inside *SeatBox
		for i := range boxes[seat.role] {
			switch box := &boxes[seat.role][i]; {
			case !slices.Contains(Placements, box.Cell):
				// No cell, or a value no cell has: each refused on its own.
			case box.Cell != PlacementContainer:
				if outside == nil {
					outside = box
				}
			case inside == nil:
				inside = box
			}
		}
		if outside != nil {
			p.addSaying(here, ErrConflict, outsideContainerSaid, "%s", c.outsideContainer(outside))
		}
		if inside != nil {
			read = true
			if joins != nil {
				p.addSaying(here, ErrConflict, hostNetworkSaid,
					"the seat's executor can launch on providers.llm.%s in a container, and "+
						"providers.sandbox.local.%s can put that container on the host's network or let "+
						"it reach the host's loopback, where every service listening there is in its "+
						"reach, and a page could lead the agent to one. Take the container off the "+
						"host's network, or remove sandbox.browser", inside.Executor, joins)
			}
		}
		for _, key := range c.runners(seat.chain) {
			if entry := c.Providers.LLM[key]; entry.deniesWeb() {
				p.addSaying(here, ErrConflict, webDeniedSaid,
					"the seat's executor can launch on providers.llm.%s, which denies its harness "+
						"the web (overrides.web_tools: denied), and a browser would open the web that "+
						"denial closes. Point the seat at an entry that keeps the web, allow it on that "+
						"entry, or remove sandbox.browser", key)
				break
			}
		}
	}
	if local != nil && local.ChromiumSandbox != "" {
		path := field("providers.sandbox.local.chromium_sandbox")
		if !local.ChromiumSandbox.Valid() {
			p.add(path, ErrUnknownValue, "%q (want %s)", local.ChromiumSandbox,
				words.Or(browser.ChromiumSandboxes()))
		}
		if !read {
			p.add(path, ErrConflict,
				"only a browser seat's container box reads this, and no seat with sandbox.browser "+
					"launches in a container. Give such a seat an entry whose run_in is container, or "+
					"remove the field")
		}
	}
	return p.err()
}

// outsideContainer is the refusal of a browser seat whose executor can launch
// in box, outside a container. One sentence frame for every cell, naming it
// as written and worded by what placed the box there, never by which cell it
// is: the entry's own run_in, else the default_run_in written, else the
// catalogue resolving alone (the block [SandboxProvider.runInAt] names).
func (c *Company) outsideContainer(box *SeatBox) string {
	const frame = "a seat's browser runs only in a container box, and the seat's executor can " +
		"launch on providers.llm.%s, %s. %s, or remove sandbox.browser"
	switch {
	case c.Providers.LLM[box.Executor].RunIn != "":
		return fmt.Sprintf(frame, box.Executor, "whose run_in is "+string(box.Cell),
			"Set that entry's run_in to container, point the seat at an entry that runs in one")
	case c.Providers.Sandbox.DefaultRunIn != "":
		return fmt.Sprintf(frame, box.Executor, "which names no run_in and so launches in "+
			"providers.sandbox.default_run_in, "+string(box.Cell),
			"Give that entry run_in: container, make the default container")
	}
	return fmt.Sprintf(frame, box.Executor, "which names no run_in, and providers.sandbox names "+
		"no default_run_in, so the run lands where the catalogue resolves on its own, "+string(box.Cell),
		"Give that entry run_in: container, set providers.sandbox.default_run_in to container")
}

// playwrightOnHostAdvice is a server that launches Playwright MCP on the
// engine host ([Company.playwrightWarnings]).
var playwrightOnHostAdvice = says("A browser on the engine host",
	"This MCP server starts a web browser on the machine that runs Crewlet, outside every "+
		"agent's box. Give the agents that browse a browser of their own instead.")

// playwrightImage is the image Playwright MCP is published as.
const playwrightImage = "mcr.microsoft.com/playwright/mcp"

// playwrightWarnings is one advisory per enabled stdio server that launches
// Playwright MCP, a browser on the engine host for every agent and phase that
// reaches it, where a seat's own browser runs in its box. It reads the command
// and each argument, each split on white space so a shell's -c string is read
// too, and is located at the first that names the package (with or without
// @<version>), its playwright-mcp binary, or its image (with or without a tag
// or digest). A launch a wrapper script hides is not seen: nothing depends on
// an advisory. Its Item is the server's name.
func (c *Company) playwrightWarnings() []Warning {
	var out []Warning
	for i := range c.MCPServers {
		server := &c.MCPServers[i]
		if server.Kind() != TransportStdio || !server.IsEnabled() {
			continue
		}
		entryAt := idx(field("mcp_servers"), i)
		where, spelled := at(entryAt, "command"), playwrightLaunch(server.Command)
		for j := 0; spelled == "" && j < len(server.Args); j++ {
			where, spelled = idx(at(entryAt, "args"), j), playwrightLaunch(server.Args[j])
		}
		if spelled == "" {
			continue
		}
		w := playwrightOnHostAdvice.raise(where, fmt.Sprintf("this server launches Playwright MCP "+
			"(%s), which runs a browser on the engine host for every agent and phase that reaches "+
			"it, outside every box and beside the engine's own keys. A seat that needs a browser "+
			"gets one in its own container box with role.sandbox.browser: true; keep this server "+
			"only where a browser on the host is what you mean", spelled))
		w.Item = server.Name
		out = append(out, w)
	}
	return out
}

// playwrightLaunch is the word of value, split on white space and stripped of
// a shell's quotes, that names Playwright MCP, or empty for none.
func playwrightLaunch(value string) string {
	for _, word := range strings.Fields(value) {
		word = strings.Trim(word, `"'`)
		switch {
		case word == "@playwright/mcp", strings.HasPrefix(word, "@playwright/mcp@"),
			word[strings.LastIndex(word, "/")+1:] == "playwright-mcp",
			word == playwrightImage, strings.HasPrefix(word, playwrightImage+":"),
			strings.HasPrefix(word, playwrightImage+"@"):
			return word
		}
	}
	return ""
}

// deniesWeb reports whether a run on this entry has its harness's own web
// tools off: its profile, overrides merged, says denied, as the engine reads
// the entry at launch. An entry whose profile does not load is refused by its
// own rule, so it denies nothing here.
func (l *LLMProvider) deniesWeb() bool {
	profile, err := harnessprofile.Load(l.HarnessName(), l.Overrides)
	return err == nil && profile.EffectiveWebTools() == harnessprofile.WebToolsDenied
}
