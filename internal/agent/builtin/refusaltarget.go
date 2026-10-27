package builtin

import "github.com/crewlet/crewlet/internal/tools"

// THE TARGET A WRITE ACTS ON, for the refusal-loop guard ([tools.Targeted]): a
// run of policy refusals is counted per tool and per target, so refusals
// across four artifacts are four first attempts rather than a loop, and a
// success on one artifact does not reset a loop on another. A write with no
// single target (create_artifact) is counted per tool.

// artifactTarget marks a tool whose every call acts on the artifact its
// `artifact` argument names.
type artifactTarget struct{}

var _ tools.Targeted = artifactTarget{}

// RefusalTarget implements [tools.Targeted].
func (artifactTarget) RefusalTarget(args map[string]any) string {
	return named("artifact", argString(args, "artifact"))
}

// itemTarget marks a tool whose every call acts on the work item its `item`
// argument names.
type itemTarget struct{}

var _ tools.Targeted = itemTarget{}

// RefusalTarget implements [tools.Targeted].
func (itemTarget) RefusalTarget(args map[string]any) string {
	return named("work item", argString(args, "item"))
}

// named is a target as the guard's answer names it, or "" (the whole tool)
// for a call that names none.
func named(what, id string) string {
	if id == "" {
		return ""
	}
	return what + " " + id
}
