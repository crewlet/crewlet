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
// same filesystem, a dot directory, under which Codex 0.171.0 lists no skill
// (measured). The next run's gate removes it whole, with whatever a killed
// firing left there.
func checkoutStaging(skills string) string { return skills + "/.crewlet-staging" }

// checkoutSpec is what a runner's harness reads from a checkout's root, for
// [checkoutHome.launchConfig]: Instructions, the slots ([checkoutSlot]);
// Limit, the harness's own bound in bytes on a checkout's instructions, nil
// for a harness with none; SkillDirs, the checkout's skill directories
// ([checkoutSkillDir]).
type checkoutSpec struct {
	Instructions []checkoutSlot
	Limit        *int
	SkillDirs    []checkoutSkillDir
}

// checkoutSkillDir is a checkout's skill directory as the helper reads it:
// Dir, below the checkout's root, whose skills are the directories directly
// in it, written as a string; or, Recursive, every one a walk of it reaches,
// written as {"dir": ..., "recursive": true}.
type checkoutSkillDir struct {
	Dir       string
	Recursive bool
}

// MarshalJSON writes d in the helper's shape.
func (d checkoutSkillDir) MarshalJSON() ([]byte, error) {
	if !d.Recursive {
		return json.Marshal(d.Dir)
	}
	return json.Marshal(map[string]any{"dir": d.Dir, "recursive": true})
}

// checkoutSlot is one instruction slot as the helper reads it: Names, the
// file names of which it hands the first, written as a list; or Tree, a
// directory walked for its files, written as an object (internal/notehelper's
// "Checkouts below the working directory").
type checkoutSlot struct {
	Names []string
	Tree  *checkoutTree
}

// checkoutTree is a tree slot: the directory below a checkout's root, the
// suffix of a handed file's name, and the frontmatter scope that leaves a
// file out, nil for none.
type checkoutTree struct {
	Tree   string         `json:"tree"`
	Suffix string         `json:"suffix"`
	Scoped *checkoutScope `json:"scoped,omitempty"`
}

// checkoutScope is the frontmatter keys that scope a file to paths, the first
// present deciding, and the patterns that match every path.
type checkoutScope struct {
	Keys  []string `json:"keys"`
	Whole []string `json:"whole"`
}

// MarshalJSON writes s in the helper's shape: a tree as an object, names as a
// list.
func (s checkoutSlot) MarshalJSON() ([]byte, error) {
	if s.Tree != nil {
		return json.Marshal(s.Tree)
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
	launch, _ := json.Marshal(map[string]any{"state": h.state, "agents": h.sections, "first": repos,
		"instructions": spec.Instructions, "limit": spec.Limit, "skillDirs": spec.SkillDirs,
		"skills": h.skills, "staging": h.staging, "copied": h.copied, "reserved": []string{sandbox.SkillNamespace}})
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
