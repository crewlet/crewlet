package executor

// OpenCodeConfig is where an OpenCode run's own configuration is written
// ([OpenCode.WriteConfig]), and read from by the run's script, which hands its
// content to the run's server alone as OPENCODE_CONFIG_CONTENT
// ([OpenCode.Command]).
//
// BESIDE the run's other artefacts, never in the checkout: the checkout is the
// repository's, and an opencode.json written there would replace the
// repository's own file, where content handed to the server ranks above it and
// leaves it in place. Under WorkDir, so a `git clean` in the brief cannot
// remove it from under the run.
func (p Paths) OpenCodeConfig() string { return p.WorkDir() + "/opencode.json" }

// OpenCodeValues is the directory holding the values an OpenCode run's MCP
// servers are configured with — each server's url, each header value, each
// environment value — one file apiece, which [Paths.OpenCodeConfig] names by a
// `{file:<path>}` reference rather than carrying the value
// ([openCodeValueFiles]). A directory of the run's own, removed whole before
// every run ([Runner.ClearArtifacts]), so a run configured with fewer values
// than the last keeps none of the last one's.
func (p Paths) OpenCodeValues() string { return p.WorkDir() + "/opencode-values" }

// OpenCodeValuesFresh is the directory holding an OpenCode run's values as they
// are under the job's other bridge token, for a session its script starts of
// its own in place of the one it continues ([sandbox.RunRequest.FreshWiring]):
// the same files by the same names, so the configuration's references name
// either set, which the script moves over [Paths.OpenCodeValues] where it
// starts one and removes where it continues — before the run's server starts,
// which is what reads them.
func (p Paths) OpenCodeValuesFresh() string { return p.WorkDir() + "/opencode-values-fresh" }

// OpenCodeSteerPlugin is the DIRECTORY an OpenCode run's notes plugin is written
// into, as index.js ([OpenCode.WriteConfig]), and named by the run's
// configuration: the plugin that admits a person's notes to the run's turn into
// its session at its next step (opencode_steer.js).
//
// A DIRECTORY, because OpenCode 2 loads a configured local plugin only from one
// (a file named there is skipped with a warning), trying its `server` module
// and then its `index`. Under WorkDir, never in the checkout or a
// `.opencode/plugins/` OpenCode would discover on its own; removed whole before
// every run ([Runner.ClearArtifacts]): OpenCode reloads a configured plugin on
// any change in its directory, so nothing is written there once the run's
// server starts, and the plugin itself writes nothing at all.
func (p Paths) OpenCodeSteerPlugin() string { return p.WorkDir() + "/opencode-steer" }

// OpenCodeToolEnvPlugin is the DIRECTORY of the plugin that keeps the variables
// the harness alone reads out of the commands an OpenCode run's model runs
// (opencode_toolenv.js), written and named as the notes plugin is
// ([Paths.OpenCodeSteerPlugin]), and for the same reasons a directory of its own.
func (p Paths) OpenCodeToolEnvPlugin() string { return p.WorkDir() + "/opencode-tool-env" }

// OpenCodeSkills is the DIRECTORY an OpenCode run's own skills are written into
// ([sandbox.RunRequest.Skills], [OpenCode.WriteConfig]), a directory per skill,
// and named in the run's configuration's `skills`. Its own, under WorkDir, for
// the reasons [Paths.ClaudeSkillsPlugin] gives; and written before the run's
// server starts, which watches it.
func (p Paths) OpenCodeSkills() string { return p.WorkDir() + "/opencode-skills" }

// OpenCodeSkillsPlugin is the DIRECTORY of the plugin that reports each skill an
// OpenCode run's model loads to the run's bridge (opencode_skills.js), written
// and named as the notes plugin is ([Paths.OpenCodeSteerPlugin]), and for the
// same reasons a directory of its own.
func (p Paths) OpenCodeSkillsPlugin() string { return p.WorkDir() + "/opencode-skill-loads" }

// OpenCodeRenewExport is where a continuing run's gate exports the box's own
// copy of its session to re-import it ([openCodeRenew]), kept there where the
// import failed so the run can say what it held.
func (p Paths) OpenCodeRenewExport() string { return p.WorkDir() + "/opencode-renew-export.json" }

// OpenCodeCheckouts is the DIRECTORY of what an OpenCode run hands the
// checkouts below its working directory with ([OpenCode.checkoutHome]): the
// note helper, its launch config, the sections file and skills directory the
// run's configuration names, and the checkouts' state. Under WorkDir, removed
// whole before every run ([OpenCode.artifactDirs]), so no run's copies or
// sections outlive the next run's clear.
func (p Paths) OpenCodeCheckouts() string { return p.WorkDir() + "/opencode-checkouts" }

// OpenCodeNoteHelper is the note helper an OpenCode run writes at every
// launch ([writeNoteHelper]): its gate runs checkout-launch, and its
// checkouts plugin the opencode-nested adapter.
func (p Paths) OpenCodeNoteHelper() string { return p.OpenCodeCheckouts() + "/notehelper" }

// OpenCodeCheckoutState is the run's checkout state ([OpenCode.checkoutHome]):
// the checkouts handed, the lock, the skip log and the nested adapter's
// config, remade by every run's gate.
func (p Paths) OpenCodeCheckoutState() string { return p.OpenCodeCheckouts() + "/state" }

// OpenCodeCheckoutLaunch is the run's checkout-launch config
// ([openCodeCheckouts]).
func (p Paths) OpenCodeCheckoutLaunch() string { return p.OpenCodeCheckouts() + "/launch.json" }

// OpenCodeCheckoutSections is where checkout-launch writes the run's
// checkouts' instructions, a file the run's configuration names in its
// `instructions` ([OpenCode.WriteConfig]), which OpenCode skips while it is
// not there (measured on 2.3.2).
func (p Paths) OpenCodeCheckoutSections() string { return p.OpenCodeCheckouts() + "/instructions.md" }

// OpenCodeCheckoutSkills is the directory the checkouts' skills are copied
// into, as `<repository>:<skill>`, which the run's configuration names in its
// `skills`, apart from the seat's own ([Paths.OpenCodeSkills]).
func (p Paths) OpenCodeCheckoutSkills() string { return p.OpenCodeCheckouts() + "/skills" }

// OpenCodeCheckoutSkillsCopied lists, a name a line, the skills a run copied
// into [Paths.OpenCodeCheckoutSkills], so the run's end removes them.
func (p Paths) OpenCodeCheckoutSkillsCopied() string { return p.OpenCodeCheckouts() + "/skills-copied" }

// OpenCodeCheckoutsOffBy is each file and rule that keeps the run's checkouts
// plugin from loading, written by [OpenCode.WriteConfig] where one does, for
// the run's skip log ([openCodeCheckoutsGate]).
func (p Paths) OpenCodeCheckoutsOffBy() string { return p.OpenCodeCheckouts() + "/plugin-off-by" }

// OpenCodeCheckoutsPlugin is the DIRECTORY of the plugin that hands a checkout
// cloned during the run its instructions and skills (opencode_checkouts.js),
// written and named as the notes plugin is ([Paths.OpenCodeSteerPlugin]), and
// for the same reasons a directory of its own.
func (p Paths) OpenCodeCheckoutsPlugin() string { return p.WorkDir() + "/opencode-checkouts-plugin" }
