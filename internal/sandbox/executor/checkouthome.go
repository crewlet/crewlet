package executor

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// checkoutHome is where a runner that hands the checkouts below its run's
// working directory their instructions and skills through the note helper's
// checkout core (internal/notehelper's "Checkouts below the working
// directory") keeps them: the helper and its state, the launch config
// ([checkoutSpec]), the home instruction file the harness reads (agents), the
// home's own copy of it held beside the run while it runs (held), the file
// the helper writes the checkouts' sections into (sections), and the skills
// directory they are copied into, with the staging directory a copy is built
// in ([checkoutStaging]) and the list of copies the next run removes.
type checkoutHome struct {
	state, helper, launch   string
	agents, held, sections  string
	skills, staging, copied string
}

// checkoutStaging is the directory under the skills directory skills a
// checkout's skill is built in before one rename moves it into place: on the
// same filesystem, a dot directory, under which neither Codex 0.171.0 nor
// Claude Code 2.1.310 lists a skill (measured). The next run's gate removes
// it whole, with whatever a killed firing left there.
func checkoutStaging(skills string) string { return skills + "/.crewlet-staging" }

// checkoutSpec is what a runner's harness reads from a checkout's root, for
// [checkoutHome.launchConfig]: Instructions, the slots of file names; Limit,
// the harness's own bound in bytes on a checkout's instructions, nil for a
// harness with none; SkillDirs, the checkout's skill directories;
// NestedSkillsOnly where the harness's nested adapter hands skills alone, so
// its firings read no instruction file; and ImportInstructions where the
// harness reads them itself, each named as an `@<absolute path>` import in
// place of its text, Limit not applying (Claude Code, measured on 2.1.310 and
// 2.1.330; Gemini CLI and Qwen Code need their own measurement of absolute
// imports before they set it).
type checkoutSpec struct {
	Instructions       [][]string
	Limit              *int
	SkillDirs          []string
	NestedSkillsOnly   bool
	ImportInstructions bool
}

// checkoutUnlimited is the [checkoutSpec.Limit] of a harness that bounds no
// instruction file it reads.
var checkoutUnlimited *int

// launchConfig is the helper's checkout-launch config: spec, h's places, the
// repositories the run pushes to first, and the seats' namespace reserved, so
// no repository's skills are listed under it.
func (h checkoutHome) launchConfig(spec checkoutSpec, repos []string) []byte {
	launch, _ := json.Marshal(map[string]any{"state": h.state, "agents": h.sections, "first": repos,
		"instructions": spec.Instructions, "limit": spec.Limit, "skillDirs": spec.SkillDirs,
		"skills": h.skills, "staging": h.staging, "copied": h.copied, "reserved": []string{sandbox.SkillNamespace},
		"nestedSkillsOnly": spec.NestedSkillsOnly, "importInstructions": spec.ImportInstructions})
	return launch // strings, lists and an int cannot fail to encode
}

// gate is the part of a run's script that hands the harness its checkouts'
// instructions and skills before it starts, and sets hk where the box holds
// the helper a nested hook runs: an earlier run's instructions put back
// ([putBackHome]) and the home's own held afresh, the copies an earlier run
// listed and the staging directory removed, checkout-launch run, and the
// checkouts' sections written after the home's own through a link as the home
// keeps the file, which is left untouched where they hand nothing.
func (h checkoutHome) gate() string {
	q := shellword.Quote
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	line(`rr=%s; rh=%s; rk=%s; ra=%s; rha=%s; rw=%s; rq=%s; hk=`, q(h.state), q(h.helper), q(h.skills), q(h.agents),
		q(h.held), q(h.copied), q(h.sections))
	line(`%s || exit 1`, putBackHome(h.agents, h.held))
	line(`if [ -e "$ra" ]; then cp "$ra" "$rha"; else : > "$rha%s"; fi || exit 1`, heldNone)
	line(`if [ -f "$rw" ]; then while IFS= read -r rl; do case "$rl" in ""|*/*|.|..) ;; *) rm -rf "$rk/$rl";; esac; done < "$rw"; fi`)
	line(`: > "$rw" && rm -rf "$rr" "$rq" %s && mkdir -p "$rr" || exit 1`, q(h.staging))
	line(`if [ -f "$rh" ]; then`)
	line(`  chmod 700 "$rh" || exit 1`)
	line(`  "$rh" -adapter checkout-launch -config %s < /dev/null || { echo 'crewlet: the run could not hand its repositories their instructions' >&2; exit 1; }`,
		q(h.launch))
	line(`  hk=1`)
	line(`else`)
	line(`  echo 'crewlet: this box has no note helper for its machine, so the run starts without the instructions and skills of its repositories, and without the hook that hands a checkout cloned during it its instructions and skills' >&2`)
	line(`fi`)
	line(`if [ -s "$rq" ]; then { if [ -s "$rha" ]; then cat "$rha"; printf '\n\n'; fi; cat "$rq"; } > "$rq.all" && cat "$rq.all" > "$ra" && rm -f "$rq.all" || exit 1; fi`)
	return b.String()
}

// restore is the shell putting the home's own instruction file back once the
// harness has stopped ([putBackHome]).
func (h checkoutHome) restore() string { return putBackHome(h.agents, h.held) }
