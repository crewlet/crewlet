package executor

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// checkoutHome is where a runner handing the checkouts below its working
// directory their instructions and skills through the note helper's core
// (internal/notehelper's "Checkouts below the working directory") keeps them:
// the helper, its state and launch config ([checkoutSpec]); the home
// instruction file the harness reads (agents: the first candidate that exists,
// else the first; none for a harness whose home file is never held, its
// sections read where they are written), the home's own held beside it (held),
// the helper's sections (sections); the skills directory copies go into, each
// built in state/staging ([checkoutHome.staging]), with the list the run's end
// removes; roots, listing their roots for a harness the script points at them
// ("" for none); for a gated spec, gateCmd, writing what checkout-launch reads,
// and afterLaunch, run once it has claimed.
type checkoutHome struct {
	state, helper, launch string
	agents                []string
	agentsFrom            string
	held, sections        string
	skills, copied, roots string
	gateCmd, afterLaunch  string
	// own is the run's own instructions, written ahead of the home's
	// (Copilot's share its one file), "" for none.
	own string
}

// gateFile is where [checkoutHome]'s gateCmd writes the gate
// checkout-launch reads: in the state, remade by every run.
func (h checkoutHome) gateFile() string { return h.state + "/gate.json" }

// holdsHome reports whether h writes a home instruction file: from its
// candidates, or from the file the script lists them in at run time
// (agentsFrom, a path a line, for a harness whose file names a setting
// decides: Qwen Code's context.fileName).
func (h checkoutHome) holdsHome() bool { return len(h.agents) > 0 || h.agentsFrom != "" }

// heldLive marks which of several home instruction files a run holds
// (held+heldLive names it), so the put-back restores the one that existed,
// or removes the one the run made, whichever the next run would choose.
const heldLive = ".live"

// heldMade names the topmost directory the gate made for the home file
// (held+heldMade), written before it makes it, so the put-back, this run's or
// a killed run's next, removes what it made and nothing the home had.
const heldMade = ".made"

// staging is where the helper builds a checkout's skill before one rename
// moves it into the skills directory (internal/notehelper's stagingDir):
// under the run's state, outside every skills directory, which Grok Build
// 1.0.92 and Hermes Agent 0.23.4 list a dot directory under (measured), and on
// its filesystem, both under the box's one home. The gate removes it with the
// state, and restore after the harness stops, whatever a killed firing left.
func (h checkoutHome) staging() string { return h.state + "/staging" }

// removeSiblingStaging is the shell removing what a firing left in the
// staging beside the skills directory's real path, where the helper builds
// copies when the state is on another filesystem than the skills
// (internal/notehelper's siblingStaging); nothing where the skills directory
// is not there.
func (h checkoutHome) removeSiblingStaging() string {
	return fmt.Sprintf(`{ rs=$(cd -P %s 2> /dev/null && cd .. && pwd -P) && rm -rf "$rs/.crewlet-staging-skills"; } || :`,
		shellword.Quote(h.skills))
}

// checkoutSpec is what a runner's harness reads from a checkout's root, for
// [checkoutHome.launchConfig]: Instructions, the slots ([checkoutSlot]);
// Limit, the harness's own bound in bytes on a checkout's instructions, nil
// for none; SkillDirs ([checkoutSkillDir]); NestedSkillsOnly where the nested
// adapter hands skills alone; ImportInstructions where the harness reads them
// itself, each an `@<absolute path>` import, Limit not applying (Claude Code,
// measured on 2.1.310 and 2.1.330; Gemini CLI and Qwen Code import no absolute
// path from a home file, measured on 0.70.0 and 0.28.4); SkillSep, what joins
// a copy's repository and skill, "" for `:`; FiringLimit, a nested firing's
// bound in bytes on all its checkouts' files, nil for none; Gated where the
// harness's own code answers (internal/notehelper's checkoutGate); Budget, the
// bound the sections share with the run's own text, nil for none.
type checkoutSpec struct {
	Instructions       []checkoutSlot
	Limit              *int
	FiringLimit        *int
	SkillDirs          []checkoutSkillDir
	NestedSkillsOnly   bool
	ImportInstructions bool
	SkillSep           checkoutSkillSep
	Gated              bool
	Budget             *checkoutBudget
	// ListedAs and HomeSkills are the helper's ("grok", Grok Build's listing
	// rule; "kimi", Kimi Code's; "opencode", OpenCode's).
	ListedAs   string
	HomeSkills []checkoutSkillDir
}

// checkoutBudget is the bound a launch's sections share with the text the
// harness reads beside them (internal/notehelper's launchBudget): Form,
// whose count applies; Own, the file of the run's own text; Bytes, the bound.
type checkoutBudget struct {
	Form  string `json:"form"`
	Own   string `json:"own"`
	Bytes int    `json:"bytes"`
}

// checkoutSkillSep is what joins a copy's repository and skill (the helper's
// skillSep).
type checkoutSkillSep string

// checkoutSkillSepHyphen joins them for Gemini CLI and Qwen Code, as Gemini
// CLI 0.70.0 lists `a:b` as `a-b` and the runners name their own skills so.
const checkoutSkillSepHyphen checkoutSkillSep = "-"

// checkoutSkillDir is a checkout's skill directory as the helper reads it:
// Dir, below the checkout's root, whose skills are the directories directly
// in it, written as a string; or, Recursive, every one a walk of it reaches,
// or a walk's options set, written as an object.
type checkoutSkillDir struct {
	Dir       string `json:"dir"`
	Recursive bool   `json:"recursive,omitempty"`
	checkoutWalk
}

// checkoutWalk is how the helper walks a skill directory past Pi's walk, as
// Grok Build 1.0.92 does: on below a skill (Through), dot directories and
// node_modules too (Every), to Depth directories below it, or its command
// files as skills (Commands).
type checkoutWalk struct {
	Through  bool `json:"through,omitempty"`
	Every    bool `json:"every,omitempty"`
	Depth    int  `json:"depth,omitempty"`
	Commands bool `json:"commands,omitempty"`
}

// MarshalJSON writes d in the helper's shape.
func (d checkoutSkillDir) MarshalJSON() ([]byte, error) {
	if !d.Recursive && d.checkoutWalk == (checkoutWalk{}) {
		return json.Marshal(d.Dir)
	}
	type wire checkoutSkillDir
	return json.Marshal(wire(d))
}

// checkoutSlot is one instruction slot as the helper reads it: Names, the
// file names of which it hands the first, written as a list; or Tree, a
// directory walked for its files, written as an object (internal/notehelper's
// "Checkouts below the working directory").
type checkoutSlot struct {
	Names []string
	Tree  *checkoutTree
	// Inside follows a name's link only within the checkout; Once hands no
	// file an earlier slot handed; SkipBlank passes over a name's file of
	// whitespace alone for the next name (Kimi Code, measured on 2.7.2).
	Inside    bool `json:"inside,omitempty"`
	Once      bool `json:"once,omitempty"`
	SkipBlank bool `json:"skipBlank,omitempty"`
}

// checkoutTree is a tree slot: the directory below a checkout's root, the
// suffix of a handed file's name, the harness whose walk and frontmatter
// reading the helper follows, and the frontmatter scope that leaves a file
// out, nil for none; Once, as [checkoutSlot]'s.
type checkoutTree struct {
	Tree   string         `json:"tree"`
	Suffix string         `json:"suffix"`
	Form   checkoutForm   `json:"form"`
	Scoped *checkoutScope `json:"scoped,omitempty"`
	Once   bool           `json:"once,omitempty"`
}

// checkoutForm is a harness whose tree walk the helper knows (its treeForm).
type checkoutForm string

const (
	// checkoutFormClaude is Claude Code's walk of .claude/rules.
	checkoutFormClaude checkoutForm = "claude"
	// checkoutFormCopilot is Copilot 1.0.114's walk of .claude/rules.
	checkoutFormCopilot checkoutForm = "copilot"
	// checkoutFormQwen is Qwen Code's walk of .qwen/rules.
	checkoutFormQwen checkoutForm = "qwen"
	// checkoutFormGrok is Grok Build's read of .grok/rules and .claude/rules
	// (flat, dot files too).
	checkoutFormGrok checkoutForm = "grok"
)

// checkoutScope is the frontmatter keys that scope a file to paths, the first
// present deciding, and the patterns that match every path.
type checkoutScope struct {
	Keys  []string `json:"keys"`
	Whole []string `json:"whole"`
}

// MarshalJSON writes s in the helper's shape: a tree as an object, names as a
// list.
func (s checkoutSlot) MarshalJSON() ([]byte, error) {
	switch {
	case s.Tree != nil:
		return json.Marshal(s.Tree)
	case s.Inside || s.Once || s.SkipBlank:
		return json.Marshal(struct {
			Names     []string `json:"names"`
			Inside    bool     `json:"inside,omitempty"`
			Once      bool     `json:"once,omitempty"`
			SkipBlank bool     `json:"skipBlank,omitempty"`
		}{s.Names, s.Inside, s.Once, s.SkipBlank})
	}
	return json.Marshal(s.Names)
}

// checkoutUnlimited is the [checkoutSpec.Limit] of a harness that bounds no
// instruction file it reads.
var checkoutUnlimited *int

// launchConfig is the helper's checkout-launch config: spec, h's places, the
// repositories the run pushes to first, and the seats' namespace reserved, so
// no repository's skills are listed under it.
func (h checkoutHome) launchConfig(spec checkoutSpec, repos []string) []byte {
	config := map[string]any{"state": h.state, "agents": h.sections, "first": repos,
		"instructions": spec.Instructions, "limit": spec.Limit, "firingLimit": spec.FiringLimit,
		"skillDirs": spec.SkillDirs, "skills": h.skills, "copied": h.copied,
		"reserved":         []string{sandbox.SkillNamespace},
		"nestedSkillsOnly": spec.NestedSkillsOnly, "importInstructions": spec.ImportInstructions,
		"skillSep": spec.SkillSep, "roots": h.roots}
	if spec.ListedAs != "" {
		config["listedAs"] = spec.ListedAs
	}
	if len(spec.HomeSkills) > 0 {
		config["homeSkills"] = spec.HomeSkills
	}
	if spec.Gated {
		config["gated"] = true
	}
	if spec.Budget != nil {
		config["budget"] = spec.Budget
	}
	launch, _ := json.Marshal(config)
	return launch // strings, lists, booleans and ints cannot fail to encode
}

// gate is the part of a run's script that hands the harness its checkouts'
// instructions and skills before it starts, and sets hk where the box holds
// the helper a nested hook runs: an earlier run's instructions put back
// ([putBackHome]) and the home's own held afresh, whole ([heldPart]; a link
// leading nowhere set aside whole, [heldLink]), the copies an earlier run
// listed and the state with its staging removed, checkout-launch run, and the
// checkouts' sections written after the home's own through a link as the home
// keeps the file, its directory made where there is none (a fresh home, or a
// Qwen Code context.fileName naming a path below it; [heldMade]), the file
// left untouched where they hand nothing or the harness reads none.
func (h checkoutHome) gate() string { return h.prepare() + h.compose() }

// prepare is [checkoutHome.gate] up to checkout-launch: the hold, the
// removals and the launch, which write the sections [checkoutHome.compose]
// puts in place.
func (h checkoutHome) prepare() string {
	q := shellword.Quote
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	ra := ""
	if len(h.agents) == 1 {
		ra = q(h.agents[0])
	}
	line(`rr=%s; rh=%s; rk=%s; ra=%s; rha=%s; rw=%s; rq=%s; hk=`, q(h.state), q(h.helper), q(h.skills), ra,
		q(h.held), q(h.copied), q(h.sections))
	if h.holdsHome() {
		h.holdHome(line)
	}
	line(`%s || exit 1`, h.removeCopies())
	line(`%s`, h.removeSiblingStaging())
	line(`: > "$rw" && rm -rf "$rr" "$rq" && mkdir -p "$rr" || exit 1`)
	if h.roots != "" {
		line(`: > %s || exit 1`, q(h.roots))
	}
	line(`if [ -f "$rh" ]; then`)
	line(`  chmod 700 "$rh" || exit 1`)
	launch := fmt.Sprintf(`"$rh" -adapter checkout-launch -config %s < %%s || { echo 'crewlet: the run could not hand its repositories their instructions' >&2; exit 1; }`,
		q(h.launch))
	if h.gateCmd == "" {
		line(`  %s`, fmt.Sprintf(launch, "/dev/null"))
	} else {
		line(`  %s`, h.gateCmd) // which writes the gate, an empty one where the harness cannot answer
		line(`  %s`, fmt.Sprintf(launch, q(h.gateFile())))
		if h.afterLaunch != "" {
			line(`  %s`, h.afterLaunch)
		}
	}
	line(`  hk=1`)
	line(`else`)
	line(`  echo 'crewlet: this box has no note helper for its machine, so the run starts without the instructions and skills of its repositories, and without the hook that hands a checkout cloned during it its instructions and skills' >&2`)
	line(`fi`)
	return b.String()
}

// holdHome is the gate's shell putting an earlier run's instruction file back
// and holding the home's own afresh ([checkoutHome.gate]).
func (h checkoutHome) holdHome(line func(string, ...any)) {
	q := shellword.Quote
	line(`%s || exit 1`, h.putBack())
	if h.agentsFrom != "" {
		// NO CANDIDATE where the setting names no file: the harness reads no
		// home file, so none is held or written.
		line(`ra= rf=; while IFS= read -r rc; do [ -n "$rf" ] || rf=$rc; if [ -e "$rc" ]; then ra=$rc; break; fi; done < %s; [ -n "$ra" ] || ra=$rf`,
			q(h.agentsFrom))
		line(`[ -z "$ra" ] || printf '%%s\n' "$ra" > "$rha%s" || exit 1`, heldLive)
	} else if len(h.agents) > 1 {
		// THE FILE THE HARNESS READS, named before it is held, so a run killed
		// past here is put back by the next ([checkoutHome.putBack]).
		words := make([]string, len(h.agents))
		for i, a := range h.agents {
			words[i] = q(a)
		}
		line(`for rc in %s; do if [ -e "$rc" ]; then ra=$rc; break; fi; done; [ -n "$ra" ] || ra=%s`,
			strings.Join(words, " "), q(h.agents[0]))
		line(`printf '%%s\n' "$ra" > "$rha%s" || exit 1`, heldLive)
	}
	line(`if [ -z "$ra" ]; then :; elif [ -e "$ra" ]; then cp "$ra" "$rha%s" && mv -f "$rha%s" "$rha"; elif [ -L "$ra" ]; then mv -f "$ra" "$rha%s"; `+
		`else : > "$rha%s"; fi || exit 1`, heldPart, heldPart, heldLink, heldNone)
}

// compose is the shell writing the home's instruction file from what
// [checkoutHome.prepare] held and the launch wrote: the run's own (where h has
// a file of them), the home's own, then the sections, through a link as the
// home keeps it, its directory made where there is none ([heldMade]); left
// untouched where the run adds nothing or the harness reads no home file. A
// Copilot run composes again for each launch attempt (copilotAttempts).
func (h checkoutHome) compose() string {
	if !h.holdsHome() {
		return ""
	}
	has, text := `[ -s "$rq" ]`, `{ if [ -s "$rha" ]; then cat "$rha"; printf '\n\n'; fi; cat "$rq"; }`
	if h.own != "" {
		o := shellword.Quote(h.own)
		has = `{ [ -s "$rq" ] || [ -s ` + o + ` ]; }`
		text = `{ rs=; for rf in ` + o + ` "$rha" "$rq"; do if [ -s "$rf" ]; then printf "$rs"; cat "$rf"; rs='\n\n'; fi; done; }`
	}
	return fmt.Sprintf(`if %s && [ -n "$ra" ]; then %s > "$rq.all" && `+
		`{ rmd=$(dirname "$ra"); rmt=; while [ ! -e "$rmd" ]; do rmt=$rmd; rmd=$(dirname "$rmd"); done; `+
		`[ -z "$rmt" ] || { printf '%%s\n' "$rmt" > "$rha%s" && mkdir -p "$(dirname "$ra")"; }; } && `+
		`cat "$rq.all" > "$ra" && rm -f "$rq.all" || exit 1; fi`+"\n", has, text, heldMade)
}

// restore is the shell marking the run ended once no firing holds the
// state's lock, so a firing that began claims and copies all it will before
// the copies are removed, and one after claims nothing (internal/notehelper's
// endedMarker; [checkoutEndedWait]), putting the home's
// own instruction file back once the harness has stopped ([putBackHome]), and
// removing the run's copies ([checkoutHome.removeCopies]) and what a firing
// left in staging ([checkoutHome.staging], [checkoutHome.removeSiblingStaging]),
// so no repository's skill stays in the home's skills past its run, each step
// whatever came of the others ([everyStep]). A second run of it changes
// nothing; the next run's gate, or the clear before it, repairs a run killed
// before it ran.
func (h checkoutHome) restore() string {
	q := shellword.Quote
	ended := fmt.Sprintf(`[ ! -d %[1]s ] || flock -w %[2]d %[1]s/lock touch %[1]s/ended 2> /dev/null || : > %[1]s/ended`,
		q(h.state), checkoutEndedWait)
	return everyStep(ended, h.putBack(), h.removeCopies(), "rm -rf "+q(h.staging()),
		h.removeSiblingStaging())
}

// removeCopies is the shell removing from the skills directory every copy
// the run listed in copied, a line naming no single entry (empty, holding a
// slash, `.` or `..`) refused, and then the list.
func (h checkoutHome) removeCopies() string {
	q := shellword.Quote
	return fmt.Sprintf(`{ if [ -f %[1]s ]; then while IFS= read -r rl; do case "$rl" in ""|*/*|.|..) ;; *) rm -rf %[2]s/"$rl";; `+
		`esac; done < %[1]s; fi && rm -f %[1]s; }`, q(h.copied), q(h.skills))
}

// checkoutEndedWait is the most seconds restore waits for a firing to let go
// of the state's lock before it marks the run ended all the same: a firing's
// own bound (codexNestedTimeout, piNestedTimeout), past which its harness has
// given up on it. A box without flock(1) marks it at once, as before.
const checkoutEndedWait = codexNestedTimeout

// dropSkipped is the shell removing a skip log no script printed
// ([checkoutHome.saySkipped]), a killed run's: run by the clear before every
// run, so the next run's script, ended before its gate remakes the state,
// prints no line of another run's as its own.
func (h checkoutHome) dropSkipped() string {
	return "rm -f " + shellword.Quote(h.state+"/skipped")
}

// saySkipped is the shell printing what the run's nested hook left out
// (internal/notehelper's skippedLog) to the run's error stream once the
// harness has stopped, and removing it, so a second run of it, or of the
// script's end, says no line twice: no harness shows a hook's stderr in the
// run's output (measured: Codex 0.171.0, and 0.172.0, keep it nowhere; Claude
// Code 2.1.330 only in its session transcript).
func (h checkoutHome) saySkipped() string {
	return fmt.Sprintf(`if [ -s %[1]s ]; then cat %[1]s >&2; rm -f %[1]s; fi`, shellword.Quote(h.skipped()))
}

// skipped is the run's skip log (internal/notehelper's skippedLog), which
// [checkoutHome.saySkipped] prints once the harness has stopped.
func (h checkoutHome) skipped() string { return h.state + "/skipped" }

// putBack is the shell putting the home's own instruction file back
// ([putBackHome]): of several candidates, the one the run held, named at
// held+[heldLive], which goes last, so it runs any number of times. One
// candidate is the only file the run can hold, so nothing names it.
func (h checkoutHome) putBack() string {
	if !h.holdsHome() {
		return ":"
	}
	if len(h.agents) == 1 {
		live := shellword.Quote(h.agents[0])
		return "{ " + putBackHomeAt(live, h.held) + " && " + h.removeMade(live) + "; }"
	}
	at := shellword.Quote(h.held + heldLive)
	return fmt.Sprintf(`{ if [ -f %[1]s ]; then rhl=$(cat %[1]s) && %[2]s && %[3]s && rm -f %[1]s; fi; }`, at,
		putBackHomeAt(`"$rhl"`, h.held), h.removeMade(`"$rhl"`))
}

// removeMade is the shell removing, once the home file at the shell word live
// is put back, each directory the gate made for it ([heldMade]), deepest
// first, while it is empty, up to the one recorded; one the home had, or a
// record naming no directory above live, is left alone.
func (h checkoutHome) removeMade(live string) string {
	m := shellword.Quote(h.held + heldMade)
	return fmt.Sprintf(`if [ -f %[1]s ]; then rmt=$(cat %[1]s) && rmd=$(dirname %[2]s) && case $rmd in "$rmt"|"$rmt"/*) `+
		`while rmdir "$rmd" 2> /dev/null && [ "$rmd" != "$rmt" ]; do rmd=$(dirname "$rmd"); done;; esac; rm -f %[1]s; fi`, m, live)
}
