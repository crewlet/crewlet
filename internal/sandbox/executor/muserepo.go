package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// museRulesNames is the rules file Muse Code 1.4.25 loads from a directory a
// session starts in: the first of these that exists, by exact name, the
// others named on its error stream as ignored (measured).
var museRulesNames = []string{"AGENTS.md", "CLAUDE.md", ".agents/AGENTS.md", ".claude/CLAUDE.md"}

// museCheckouts is what a Muse Code 1.4.25 session started at a repository's
// root reads of it (measured): one rules file ([museRulesNames]) and the
// skills of its .agents/skills, .claude/skills and .codex/skills, .agents'
// winning a name; all of a checkout's whole, within the rules reminder's bound
// it shares with the home's file and the start directory's own (the core's
// "muse" budget); never its hooks, MCP servers, settings, agents or commands.
// A copy is left out where Muse would not list it beside a skill listed under
// its name (rows so1-so6, fn1-fn4): in ~/.config/muse/skills or the start
// directory's (listed in its place), ~/.agents/skills (neither listed), and,
// where foreign says Muse lists them, ~/.claude/skills and ~/.codex/skills
// (the copy listed in theirs).
func museCheckouts(paths Paths, foreign bool) checkoutSpec {
	work := paths.Home + "/" + sandbox.WorkspaceSubdir
	spec := checkoutSpec{Instructions: []checkoutSlot{{Names: museRulesNames}}, Limit: checkoutUnlimited,
		SkillDirs: []checkoutSkillDir{{Dir: ".agents/skills"}, {Dir: ".claude/skills"}, {Dir: ".codex/skills"}},
		Budget:    museBudget}
	dirs := []string{paths.MuseConfig() + "/skills", paths.MuseSkills(), work + "/.agents/skills",
		work + "/.claude/skills", work + "/.codex/skills"}
	if foreign {
		dirs = append(dirs, paths.Home+"/.claude/skills", paths.Home+"/.codex/skills")
	}
	for _, dir := range dirs {
		spec.HomeSkills = append(spec.HomeSkills, checkoutSkillDir{Dir: dir})
	}
	spec.ListedAs, spec.Reserved = museListedAs, museReserved
	return spec
}

// museReserved is the tags no checkout takes on a Muse Code run: 1.4.25
// refuses a skill named `bundled:...` or `plugin:...` at load, saying so only on
// its error stream (measured, rows p2 and p5), so a checkout named either
// would lose every copy unseen; it is tagged `bundled-2`, `plugin-2` instead.
var museReserved = []string{"bundled", "plugin"}

// museListedAs is the note helper's listing rule for Muse Code 1.4.25: a
// home skill listed under its frontmatter's name, else its directory's.
const museListedAs = "muse"

// museBudget is the note helper's budget form for Muse Code 1.4.25's rules
// reminder (internal/notehelper's checkoutmuse.go).
const museBudget = "muse"

// museCheckoutHome is where a Muse Code run keeps its checkouts' instructions
// and skills ([checkoutHome]): the home's rules file, $CONFIG_DIR/AGENTS.md
// or $CONFIG_DIR/CLAUDE.md, whichever Muse loads, held beside the run, else
// an AGENTS.md of the run's opening with the foreign personal rules Muse
// would have loaded in its absence ([Paths.MuseForeignRules]); and
// ~/.agents/skills, each copied as `<repository>:<skill>`.
func museCheckoutHome(paths Paths) checkoutHome {
	return checkoutHome{state: paths.MuseCheckouts(), helper: paths.MuseNoteHelper(), launch: paths.MuseCheckoutLaunch(),
		agents: []string{paths.MuseConfig() + "/AGENTS.md", paths.MuseConfig() + "/CLAUDE.md"},
		held:   paths.MuseHomeRules(), sections: paths.MuseCheckoutSections(), skills: paths.MuseSkills(),
		copied: paths.MuseCheckoutCopied(), fallback: paths.MuseForeignRules(),
		homeText: paths.MuseHomeRulesOwn()}
}

var _ homeKeeper = Muse{}

// homeRestore puts the home's rules file back and removes the copies where a
// killed Muse Code run left them ([checkoutHome.restore]) and drops the skip
// log it left ([checkoutHome.dropSkipped]), run by the clear before every
// run, whichever harness it runs.
func (Muse) homeRestore(p Paths) string {
	h := museCheckoutHome(p)
	return everyStep(h.restore(), h.dropSkipped())
}

// museForeignRules is the foreign personal rules files Muse Code 1.4.25
// loads, the first that exists, where neither of the home's own does and
// `context.foreign_personal_rules` is not false (measured, rows hf7-hf10,
// fr4-fr7): ~/.claude/CLAUDE.md, then ~/.codex/AGENTS.md.
func museForeignRules(paths Paths) []string {
	return []string{paths.Home + "/.claude/CLAUDE.md", paths.Home + "/.codex/AGENTS.md"}
}

// museForeignOn is whether the home's settings leave Muse loading another
// harness's personal rules and skills: `context.foreign_personal_rules` and
// `foreign_personal_skills`, each on unless false (measured, rows fr4-fr7 and
// swforeign). Settings Muse cannot read refuse the run at its start.
func museForeignOn(settings []byte) (rules, skills bool) {
	var held struct {
		Context struct {
			Rules  json.RawMessage `json:"foreign_personal_rules"`
			Skills json.RawMessage `json:"foreign_personal_skills"`
		} `json:"context"`
	}
	_ = json.Unmarshal(settings, &held)
	return string(held.Context.Rules) != "false", string(held.Context.Skills) != "false"
}

// writeMuseForeign records for the run's gate the foreign personal rules
// files Muse would load in place of a home rules file that is not there
// ([Paths.MuseForeignRules]): none where rules says Muse loads none.
func writeMuseForeign(ctx context.Context, box sandbox.Sandbox, paths Paths, rules bool) error {
	files := museForeignRules(paths)
	if !rules {
		files = nil
	}
	body := ""
	if len(files) > 0 {
		body = strings.Join(files, "\n") + "\n"
	}
	if err := box.WriteFile(ctx, paths.MuseForeignRules(), []byte(body)); err != nil {
		return fmt.Errorf("executor: recording the foreign personal rules Muse Code would load: %w", err)
	}
	return nil
}

// museNestedHookID is the id of the run's nested hook in its plugin
// ([musePluginManifest]).
const museNestedHookID = "posttooluse-checkouts"

// museNestedCommand is the nested hook's command: the run's note helper as the
// one-shot `muse-nested` adapter, reading the state checkout-launch wrote.
func museNestedCommand(paths Paths) []string {
	return []string{paths.MuseNoteHelper(), "-adapter", "muse-nested", "-config", paths.MuseCheckouts() + "/nested.json"}
}

// MuseCheckouts is the run's checkout state ([museCheckoutHome]): the
// checkouts handed, the lock and the nested hook's config, remade by every run.
func (p Paths) MuseCheckouts() string { return p.WorkDir() + "/muse-checkouts" }

// MuseCheckoutLaunch is the run's checkout-launch config ([museCheckouts]).
func (p Paths) MuseCheckoutLaunch() string { return p.WorkDir() + "/muse-checkout-launch.json" }

// MuseCheckoutSections is where checkout-launch writes the run's checkouts'
// instructions, which the gate writes after the home's own into the home's
// rules file ([museCheckoutHome]).
func (p Paths) MuseCheckoutSections() string { return p.WorkDir() + "/muse-checkout-sections.md" }

// MuseCheckoutCopied lists, a name a line, the checkouts' skills a run copied
// into [Paths.MuseSkills], so the next run's gate, or the clear, removes them.
func (p Paths) MuseCheckoutCopied() string { return p.WorkDir() + "/muse-checkout-copied" }

// MuseHomeRules is the home's rules file as the home keeps it, held while the
// run's stands in its place; a killed run's copy waits there for the clear
// ([Muse.homeRestore]).
func (p Paths) MuseHomeRules() string { return p.WorkDir() + "/muse-home-rules.md" }

// MuseHomeRulesOwn is the text the run's rules file opens with before the
// checkouts' sections: the home's own, or the foreign personal rules Muse
// would have loaded in its place ([Paths.MuseForeignRules]).
func (p Paths) MuseHomeRulesOwn() string { return p.WorkDir() + "/muse-home-rules-own.md" }

// MuseForeignRules lists, a path a line, the foreign personal rules files
// Muse Code would load where the home has no rules file of its own, none
// where the home switched them off ([writeMuseForeign]).
func (p Paths) MuseForeignRules() string { return p.WorkDir() + "/muse-foreign-rules" }
