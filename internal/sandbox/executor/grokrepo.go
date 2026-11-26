package executor

import "github.com/crewlet/crewlet/internal/shellword"

// grokInstructionNames is the instruction files Grok Build 1.0.92 loads from a
// directory a trusted session starts in, every one that exists, by exact name,
// in this order (measured): each its own slot, so all are handed.
var grokInstructionNames = []string{"Agents.md", "Claude.md", "CLAUDE.md", "CLAUDE.local.md", "AGENT.md", "AGENTS.md",
	".claude/CLAUDE.md", ".claude/CLAUDE.local.md"}

// grokVendors is the directories Grok Build 1.0.92 reads skills and commands
// from at each level, in its order, the first winning a name (measured).
var grokVendors = []string{".grok", ".agents", ".claude", ".cursor"}

// grokSkillWalk is how Grok Build 1.0.92 walks a skills directory (measured):
// on below a skill, dot directories and node_modules too, to 6 directories
// below it, a deeper skill unlisted.
var grokSkillWalk = checkoutWalk{Through: true, Every: true, Depth: 6}

// grokCheckouts is what a Grok Build 1.0.92 session started at a repository's
// root with --trust reads of it (measured): each of [grokInstructionNames]
// that exists, followed through a link only within the repository, then the
// `*.md` files directly in .grok/rules and .claude/rules, dot files too, by
// name, a file reached twice handed once; whole, since Grok bounds none (a
// 4,000,000-byte file reached the model whole); and the skills and commands
// of [grokVendors], listed by Grok's rule (skillname.GrokListed), beside the
// home's, which are the run's skills directory and paths.Home's own vendor
// directories (W is the box home). Never its MCP servers, hooks, settings,
// agents or plugins: a trusted session runs a checkout's hooks and servers,
// and the run starts none a seat did not declare.
func grokCheckouts(paths Paths) checkoutSpec {
	var spec checkoutSpec
	for _, name := range grokInstructionNames {
		spec.Instructions = append(spec.Instructions, checkoutSlot{Names: []string{name}, Inside: true, Once: true})
	}
	for _, dir := range []string{".grok/rules", ".claude/rules"} {
		spec.Instructions = append(spec.Instructions,
			checkoutSlot{Tree: &checkoutTree{Tree: dir, Suffix: ".md", Form: checkoutFormGrok, Once: true}})
	}
	commands := checkoutWalk{Commands: true}
	for _, vendor := range grokVendors {
		spec.SkillDirs = append(spec.SkillDirs, checkoutSkillDir{Dir: vendor + "/skills", Recursive: true,
			checkoutWalk: grokSkillWalk}, checkoutSkillDir{Dir: vendor + "/commands", checkoutWalk: commands})
		home := paths.Home + "/" + vendor
		if vendor != ".grok" {
			spec.HomeSkills = append(spec.HomeSkills, checkoutSkillDir{Dir: home + "/skills", Recursive: true,
				checkoutWalk: grokSkillWalk})
		}
		spec.HomeSkills = append(spec.HomeSkills, checkoutSkillDir{Dir: home + "/commands", checkoutWalk: commands})
	}
	spec.ListedAs = "grok"
	return spec
}

// checkoutHome is where a Grok Build run keeps its checkouts'
// instructions and skills ([checkoutHome]): ~/.grok/AGENTS.md, which 1.0.92
// reads whole before W's own AGENTS.md (measured), the home's own held beside
// the run; and ~/.grok/skills, each copied as `<repository>:<skill>`, listed
// as Grok rewrites that name.
func (Grok) checkoutHome(paths Paths) checkoutHome {
	return checkoutHome{state: paths.GrokCheckouts(), helper: paths.GrokNoteHelper(), launch: paths.GrokCheckoutLaunch(),
		agents: []string{paths.GrokHome() + "/AGENTS.md"}, held: paths.GrokHomeInstructions(),
		sections: paths.GrokCheckoutSections(), skills: paths.GrokSkills(), copied: paths.GrokCheckoutCopied()}
}

var _ homeKeeper = Grok{}

// homeRestore puts ~/.grok/AGENTS.md back and removes the copies where a
// killed Grok Build run left them ([checkoutHome.restore]) and drops the skip
// log it left ([checkoutHome.dropSkipped]), run by the clear before every
// run, whichever harness it runs.
func (g Grok) homeRestore(p Paths) string {
	h := g.checkoutHome(p)
	return everyStep(h.restore(), h.dropSkipped())
}

// grokNestedCommand is the nested hook's command: the run's note helper as the
// one-shot `grok-nested` adapter, reading the state checkout-launch wrote.
func grokNestedCommand(paths Paths) string {
	q := shellword.Quote
	return q(paths.GrokNoteHelper()) + " -adapter grok-nested -config " + q(paths.GrokCheckouts()+"/nested.json")
}

// GrokCheckouts is the run's checkout state ([Grok.checkoutHome]): the
// checkouts handed, the lock and the nested hook's config, remade by every run.
func (p Paths) GrokCheckouts() string { return p.WorkDir() + "/grok-checkouts" }

// GrokCheckoutLaunch is the run's checkout-launch config ([grokCheckouts]).
func (p Paths) GrokCheckoutLaunch() string { return p.WorkDir() + "/grok-checkout-launch.json" }

// GrokCheckoutSections is where checkout-launch writes the run's checkouts'
// instructions, which the gate writes after the home's own into
// ~/.grok/AGENTS.md ([Grok.checkoutHome]).
func (p Paths) GrokCheckoutSections() string { return p.WorkDir() + "/grok-checkout-sections.md" }

// GrokCheckoutCopied lists, a name a line, the checkouts' skills a run copied
// into [Paths.GrokSkills], so the next run's gate, or the clear, removes them.
func (p Paths) GrokCheckoutCopied() string { return p.WorkDir() + "/grok-checkout-copied" }

// GrokHomeInstructions is ~/.grok/AGENTS.md as the home keeps it, held while
// the run's stands in its place; a killed run's copy waits there for the
// clear ([Grok.homeRestore]).
func (p Paths) GrokHomeInstructions() string { return p.WorkDir() + "/grok-home-AGENTS.md" }
