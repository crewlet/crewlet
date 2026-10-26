package org

// THE REFUSALS OF A CREATE WITH NO HOME, one sentence each for every surface
// that meets them: the seat and operator tools that file the work, and the
// Dashboard Assistant's preview, which resolves the same default through
// `GET /viewer` before any card is drawn (ADR-0055). Worded once, here, so the
// person's preview and the write it stands for refuse in the same words.

// NoHomeProject is the refusal of a create of tool that names no project,
// for a writer whose team, and every team above it, owns none.
func NoHomeProject(tool string) string {
	return tool + " needs a `project`: your team owns none, so there is no default. " +
		"Ask which project this belongs in rather than guessing."
}

// NoHomeSpace is the refusal of a page write of tool that names no container,
// for a writer whose team, and every team above it, owns no space.
func NoHomeSpace(tool string) string {
	return tool + " needs a `container`: your team owns none, so there is no default. " +
		"Ask where this belongs rather than guessing."
}
