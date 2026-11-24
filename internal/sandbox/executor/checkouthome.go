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
// directory they are copied into, with the list of copies the next run
// removes. A copy is built in state/staging ([checkoutHome.staging]).
type checkoutHome struct {
	state, helper, launch  string
	agents, held, sections string
	skills, copied         string
}

// staging is where the helper builds a checkout's skill before one rename
// moves it into the skills directory (internal/notehelper's stagingDir):
// under the run's state, outside every skills directory, which Grok Build
// 1.0.92 and Hermes Agent 0.23.4 list a dot directory under (measured), and on
// its filesystem, both under the box's one home. The gate removes it with the
// state, and restore after the harness stops, whatever a killed firing left.
func (h checkoutHome) staging() string { return h.state + "/staging" }

// checkoutSpec is what a runner's harness reads from a checkout's root, for
// [checkoutHome.launchConfig]: Instructions, the slots ([checkoutSlot]);
// Limit, the harness's own bound in bytes on a checkout's instructions, nil
// for a harness with none; SkillDirs, the checkout's skill directories.
type checkoutSpec struct {
	Instructions []checkoutSlot
	Limit        *int
	SkillDirs    []string
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

// launchConfig is the helper's checkout-launch config: spec, h's places, the
// repositories the run pushes to first, and the seats' namespace reserved, so
// no repository's skills are listed under it.
func (h checkoutHome) launchConfig(spec checkoutSpec, repos []string) []byte {
	launch, _ := json.Marshal(map[string]any{"state": h.state, "agents": h.sections, "first": repos,
		"instructions": spec.Instructions, "limit": spec.Limit, "skillDirs": spec.SkillDirs,
		"skills": h.skills, "copied": h.copied, "reserved": []string{sandbox.SkillNamespace}})
	return launch // strings, lists and an int cannot fail to encode
}

// gate is the part of a run's script that hands the harness its checkouts'
// instructions and skills before it starts, and sets hk where the box holds
// the helper a nested hook runs: an earlier run's instructions put back
// ([putBackHome]) and the home's own held afresh, whole ([heldPart]; a link
// leading nowhere set aside whole, [heldLink]), the copies an earlier run
// listed and the state with its staging removed, checkout-launch run, and the
// checkouts' sections written after the home's own through a link as the home
// keeps the file, which is left untouched where they hand nothing.
func (h checkoutHome) gate() string {
	q := shellword.Quote
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	line(`rr=%s; rh=%s; rk=%s; ra=%s; rha=%s; rw=%s; rq=%s; hk=`, q(h.state), q(h.helper), q(h.skills), q(h.agents),
		q(h.held), q(h.copied), q(h.sections))
	line(`%s || exit 1`, putBackHome(h.agents, h.held))
	line(`if [ -e "$ra" ]; then cp "$ra" "$rha%s" && mv -f "$rha%s" "$rha"; elif [ -L "$ra" ]; then mv -f "$ra" "$rha%s"; `+
		`else : > "$rha%s"; fi || exit 1`, heldPart, heldPart, heldLink, heldNone)
	line(`%s || exit 1`, h.removeCopies())
	line(`: > "$rw" && rm -rf "$rr" "$rq" && mkdir -p "$rr" || exit 1`)
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
// harness has stopped ([putBackHome]), and removing the run's copies
// ([checkoutHome.removeCopies]) and what a firing left in staging
// ([checkoutHome.staging]), so no repository's skill stays in the home's
// skills past its run. It does nothing a second time; the next run's gate,
// or the clear before it, repairs a run killed before it ran.
func (h checkoutHome) restore() string {
	return putBackHome(h.agents, h.held) + " && " + h.removeCopies() + " && rm -rf " + shellword.Quote(h.staging())
}

// removeCopies is the shell removing from the skills directory every copy
// the run listed in copied, a line naming no single entry (empty, holding a
// slash, `.` or `..`) refused, and then the list.
func (h checkoutHome) removeCopies() string {
	q := shellword.Quote
	return fmt.Sprintf(`{ if [ -f %[1]s ]; then while IFS= read -r rl; do case "$rl" in ""|*/*|.|..) ;; *) rm -rf %[2]s/"$rl";; `+
		`esac; done < %[1]s; fi && rm -f %[1]s; }`, q(h.copied), q(h.skills))
}

// saySkipped is the shell printing what the run's nested hook left out
// (internal/notehelper's skippedLog) to the run's error stream once the
// harness has stopped, and removing it, so a second run of it, or of the
// script's end, says no line twice: no harness shows a hook's stderr in the
// run's output (measured: Codex 0.171.0, and 0.172.0, keep it nowhere; Claude
// Code 2.1.330 only in its session transcript).
func (h checkoutHome) saySkipped() string {
	return fmt.Sprintf(`if [ -s %[1]s ]; then cat %[1]s >&2; rm -f %[1]s; fi`, shellword.Quote(h.state+"/skipped"))
}
