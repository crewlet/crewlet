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
// ([checkoutSpec]), the home instruction file the sections go into after the
// home's own (held beside the run while it runs), and the skills directory
// they are copied into, with the list of copies the next run removes.
type checkoutHome struct {
	state, helper, launch string
	agents, held          string
	skills, copied        string
}

// checkoutSpec is what a runner's harness reads from a checkout's root, for
// [checkoutHome.launchConfig]: Instructions, the slots of file names; Limit,
// the harness's own bound in bytes on a checkout's instructions, nil for a
// harness with none; SkillDirs, the checkout's skill directories.
type checkoutSpec struct {
	Instructions [][]string
	Limit        *int
	SkillDirs    []string
}

// checkoutUnlimited is the [checkoutSpec.Limit] of a harness that bounds no
// instruction file it reads.
var checkoutUnlimited *int

// launchConfig is the helper's checkout-launch config: spec, h's places, the
// repositories the run pushes to first, and the seats' namespace reserved, so
// no repository's skills are listed under it.
func (h checkoutHome) launchConfig(spec checkoutSpec, repos []string) []byte {
	launch, _ := json.Marshal(map[string]any{"state": h.state, "agents": h.agents, "first": repos,
		"instructions": spec.Instructions, "limit": spec.Limit, "skillDirs": spec.SkillDirs,
		"skills": h.skills, "copied": h.copied, "reserved": []string{sandbox.SkillNamespace}})
	return launch // strings, lists and an int cannot fail to encode
}

// gate is the part of a run's script that hands the harness its checkouts'
// instructions and skills before it starts, and sets hk where the box holds
// the helper a nested hook runs: the home's own instruction file held (once
// per run, the restore putting it back), the copies an earlier run listed
// removed, checkout-launch run, and the home's own instructions put first in
// the file the harness reads, the sections after them.
func (h checkoutHome) gate() string {
	q := shellword.Quote
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	line(`rr=%s; rh=%s; rk=%s; ra=%s; rha=%s; rw=%s; hk=`, q(h.state), q(h.helper), q(h.skills), q(h.agents),
		q(h.held), q(h.copied))
	line(`mkdir -p "$rr" || exit 1`)
	line(`if [ ! -e "$rha" ]; then if [ -e "$ra" ]; then cp "$ra" "$rha"; else : > "$rha"; fi || exit 1; fi`)
	line(`if [ -f "$rw" ]; then while IFS= read -r rl; do case "$rl" in ""|*/*|.|..) ;; *) rm -rf "$rk/$rl";; esac; done < "$rw"; fi`)
	line(`: > "$rw" && rm -rf "$rr" "$ra" && mkdir -p "$rr" || exit 1`)
	line(`if [ -f "$rh" ]; then`)
	line(`  chmod 700 "$rh" || exit 1`)
	line(`  "$rh" -adapter checkout-launch -config %s < /dev/null || { echo 'crewlet: the run could not hand its repositories their instructions' >&2; exit 1; }`,
		q(h.launch))
	line(`  hk=1`)
	line(`else`)
	line(`  echo 'crewlet: this box has no note helper for its machine, so the run starts without the instructions and skills of its repositories, and without the hook that hands a checkout cloned during it its instructions and skills' >&2`)
	line(`fi`)
	line(`if [ -s "$rha" ]; then { cat "$rha"; if [ -s "$ra" ]; then printf '\n\n'; cat "$ra"; fi; } > "$ra.crewlet" && mv -f "$ra.crewlet" "$ra" || exit 1; fi`)
	return b.String()
}

// restore is the shell putting the home's own instruction file back once the
// harness has stopped: the held copy over the run's, or neither where the home
// had none; nothing where none is held, so it runs any number of times.
func (h checkoutHome) restore() string {
	q := shellword.Quote
	return fmt.Sprintf(`if [ -e %[1]s ]; then if [ -s %[1]s ]; then mv -f %[1]s %[2]s; else rm -f %[1]s %[2]s; fi; fi`,
		q(h.held), q(h.agents))
}
