package executor

import (
	_ "embed"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/shellword"
)

// copilotCheckouts is what a Copilot 1.0.114 session started in a checkout
// loads from its root at launch beside .github/instructions, which the run
// names in COPILOT_CUSTOM_INSTRUCTIONS_DIRS ([copilotInstructionDirs]):
// .github/copilot-instructions.md, AGENTS.md, CLAUDE.md and GEMINI.md, each
// whole, then .claude/rules' unscoped files (measured, row c1; scope as
// Copilot's line reader reads it, row F1), with no bound of its own (Copilot
// never cuts; it refuses a launch past its static budget, which the run's
// relaunch answers); and the skills of .github/skills, .agents/skills and
// .claude/skills, .github's winning a name (row c1). Its nested firings hand
// skills alone: Copilot loads a cloned checkout's instructions itself.
func copilotCheckouts() checkoutSpec {
	return checkoutSpec{Instructions: []checkoutSlot{{Names: []string{".github/copilot-instructions.md"}},
		{Names: []string{"AGENTS.md"}}, {Names: []string{"CLAUDE.md"}}, {Names: []string{"GEMINI.md"}},
		{Tree: &checkoutTree{Tree: ".claude/rules", Suffix: ".md", Form: checkoutFormCopilot, Scoped: &checkoutScope{
			Keys: []string{"applyTo", "paths"}, Whole: []string{"**", "**/*", "*"}}}}},
		Limit:            checkoutUnlimited,
		SkillDirs:        []checkoutSkillDir{{Dir: ".github/skills"}, {Dir: ".agents/skills"}, {Dir: ".claude/skills"}},
		NestedSkillsOnly: true}
}

// copilotCheckoutHome is where a Copilot run keeps its checkouts'
// instructions and skills ([checkoutHome]): the home's copilot-instructions.md,
// the run's standing instructions first and the home's own held beside the
// run, and the home's skills, which Copilot lists at launch, each copy named
// `<repository>:<skill>` within the 64 characters past which 1.0.114 drops a
// skill without a word (measured, row k2).
func copilotCheckoutHome(paths Paths) checkoutHome {
	return checkoutHome{state: paths.CopilotRepo(), helper: paths.CopilotNoteHelper(),
		launch: paths.CopilotCheckoutLaunch(), agents: []string{paths.CopilotInstructions()},
		held: paths.CopilotHeldInstructions(), sections: paths.CopilotRepoInstructions(),
		skills: paths.CopilotSkills(), copied: paths.CopilotRepoSkills(), own: paths.CopilotRunInstructions()}
}

// copilotInstructionDirs is the shell setting COPILOT_CUSTOM_INSTRUCTIONS_DIRS
// for one launch attempt: the person's own value as the script found it
// (pi, and ps whether it was set), with the attempt's directories (the
// launch's dirs file) appended after a `,`, which keeps the person's
// directories loading beside them (measured, row Nd). Never `=<checkout>`,
// which loads stray `*.instructions.md` from anywhere below it (row N7).
func copilotInstructionDirs(state string) string {
	return fmt.Sprintf(`cd=; [ ! -f %s ] || cd=$(cat %[1]s); if [ -n "$cd" ]; then COPILOT_CUSTOM_INSTRUCTIONS_DIRS=${pi:+$pi,}$cd; `+
		`export COPILOT_CUSTOM_INSTRUCTIONS_DIRS; elif [ -n "$ps" ]; then COPILOT_CUSTOM_INSTRUCTIONS_DIRS=$pi; `+
		`export COPILOT_CUSTOM_INSTRUCTIONS_DIRS; else unset COPILOT_CUSTOM_INSTRUCTIONS_DIRS; fi`,
		shellword.Quote(state+"/dirs"))
}

// copilotLaunchCompose is the shell composing one launch attempt's
// instructions: copilot-launch keeping args' sets where the box holds the
// helper (hk), n the sets there are, then the home file and the instruction
// directories.
func copilotLaunchCompose(paths Paths, args string) string {
	h := copilotCheckoutHome(paths)
	q := shellword.Quote
	var b strings.Builder
	fmt.Fprintf(&b, `if [ -n "$hk" ]; then n=$(%s -adapter copilot-launch -config %s %s < /dev/null) || `+
		`{ echo 'crewlet: the run could not compose its repositories'"'"' instructions' >&2; exit 1; }; fi`+"\n",
		q(h.helper), q(h.launch), args)
	b.WriteString(h.compose())
	b.WriteString(copilotInstructionDirs(h.state) + "\n")
	return b.String()
}

// copilotStoreCheck is copilot_storecheck.mjs, which a run's script runs on
// the box's node after a refused launch attempt ([copilotAttempts]).
//
//go:embed copilot_storecheck.mjs
var copilotStoreCheck string

// copilotExtensionOff is the line a run says once, through its skip log
// ([checkoutHome.saySkipped]), where its home or the machine's policy switches
// the extension off (off, as the guard names each) and the run needs it for
// its checkouts alone: that never refuses the run (as a person's own session
// loads no extension either), so a checkout cloned during it is not handed,
// and a launch Copilot refuses for its static budget fails rather than start
// again onto an attempt nothing deleted ([copilotAttempts]).
func copilotExtensionOff(off []string) string {
	return "crewlet: a repository cloned during the run is not handed, and a launch Copilot refuses for its static " +
		"budget is not started again, since the run's extension is switched off: " + strings.Join(off, "; ") + "\n"
}

// copilotKeptSets is the file in a Copilot session's directory naming the
// checkouts whose launch sets its accepted launch kept, a name a line, packed
// with the session ([copilotPack]), written by the extension as the new
// session starts (copilot_steer.js), so a pack taken while it runs holds it: a
// resume launches with exactly them, never searching again ([copilotAttempts]).
// Copilot 1.0.114 resumes a session holding it and leaves it there (measured).
const copilotKeptSets = "crewlet-checkouts"

// The stream lines a Copilot run's script prints of its launch attempts
// ([copilotAttempts]): a relaunch, after which the stream is the next
// attempt's, and a run it ended for a cause the stream does not name.
const (
	copilotRelaunchMark = "crewlet.relaunch"
	copilotRefusedMark  = "crewlet.refused"
)

// copilotStaticBlocked is the warningType of Copilot 1.0.114's refusal of a
// launch, or of a request, whose static context passes the model's budget
// (measured, section 13).
const copilotStaticBlocked = "compaction_static_context_blocked"

// copilotAttempts is the part of a Copilot run's script that launches harness,
// with every handed checkout's launch set first, and again without the last
// set kept each time Copilot refuses a NEW session before any request for its
// static budget, which no run can know before launch (a hosted model's limits
// come from a catalogue at run time): at most one attempt more than the
// checkouts with a set, each refused one costing about 4.4 s on the pinned
// 1.0.114 and about 65 s on 1.0.115, which holds a self-deleted launch 60 s
// (measured). Before a relaunch the refused attempt must be gone, its
// directory and its store rows (copilot_storecheck.mjs, on the box's node,
// which must load node:sqlite), else the run fails naming what was found. A
// resume launches with the sets its session stored and is never relaunched;
// neither is a run past its first answered request.
func copilotAttempts(paths Paths, harness string) string {
	q := shellword.Quote
	res := q(paths.Result())
	sessions := q(paths.CopilotHome() + "/" + copilotSessionState)
	refused := func(reason string) string {
		return `printf '%s\n' '{"type":"` + copilotRefusedMark + `","data":{"reason":"` + reason + `"}}'`
	}
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	line(`[ ! -s %s ] || [ ! -d "$rr" ] || cat %[1]s >> "$rr/skipped"`, q(paths.CopilotExtensionOff()))
	line(`ks=%s/"$s"/%s; kf=-keep kv=-1 n=0 at=0`, sessions, copilotKeptSets)
	line(`[ "$k" = no ] || { kf=-kept; kv=$ks; }`)
	line(`refused() { tail -c +"$((so + 1))" %[1]s 2> /dev/null | grep -qF '"warningType":"%[2]s"' && `+
		`! tail -c +"$((so + 1))" %[1]s 2> /dev/null | grep -qF '"type":"assistant.message"'; }`, res, copilotStaticBlocked)
	// THE STORE CHECK'S NODE, asked only once a relaunch is due: node:sqlite
	// loads without a flag from Node 22.13 and 23.4, and Copilot names no floor
	// (a standalone Copilot needs no node at all), so a launch that fits needs none.
	line(`sqlite() { node -e 'require("node:sqlite")' > /dev/null 2>&1 && return 0; `+
		`nv=$(node --version 2> /dev/null) || nv=; case $nv in v[0-9]*) case $nv in *[!v0-9.]*) nv=unreadable;; esac;; *) nv=none;; esac; `+
		`printf '{"type":"%s","data":{"reason":"node","node":"%%s"}}\n' "$nv"; return 1; }`, copilotRefusedMark)
	line(`gone() { if [ -e %s/"$s" ]; then printf '{"type":"%s","data":{"reason":"state","session":"%%s"}}\n' "$s"; return 1; fi; `+
		`node %s %s "$s"; }`, sessions, copilotRefusedMark, q(paths.CopilotStoreCheck()),
		q(paths.CopilotHome()+"/session-store.db"))
	line(`while :; do`)
	line(`at=$((at + 1))`)
	b.WriteString(copilotLaunchCompose(paths, `"$kf" "$kv"`))
	line(`printf '%%s\n' "$at" > %s || exit 1`, q(paths.CopilotAttempt()))
	line(`[ "$at" = 1 ] || exec < "$bf"`)
	line(`so=$(wc -c < %s 2> /dev/null) || so=0`, res)
	b.WriteString(launchHarness(harness, paths))
	line(`{ [ -z "$endedas" ] && [ "$code" != 0 ] && refused; } || break`)
	line(`if [ "$k" = yes ]; then %s; break; fi`, refused("resume"))
	line(`left=$((n)); [ "$kv" = -1 ] || left=$kv`)
	line(`if [ "$left" -le 0 ]; then %s; break; fi`, refused("alone"))
	line(`if [ -f %s ]; then %s; break; fi`, q(paths.CopilotExtensionOff()), refused("extension"))
	line(`sqlite || break`)
	line(`gone || break`)
	line(`kv=$((left - 1))`)
	line(`printf '{"type":"%s","data":{"kept":%%d,"sets":%%d}}\n' "$kv" "$((n))"`, copilotRelaunchMark)
	line(`done`)
	return b.String()
}
