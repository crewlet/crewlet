package executor

// GeminiDir is where Gemini CLI keeps its state, settings and sessions: the box
// home's `.gemini`, which no setting or variable but GEMINI_CLI_HOME moves, and
// the wrapper unsets that ([GeminiCLI.Output]).
func (p Paths) GeminiDir() string { return p.Home + "/.gemini" }

// GeminiSettings is the run's settings file ([geminiSettings]), which the
// script puts into the home's own ([Paths.GeminiHomeSettings]).
func (p Paths) GeminiSettings() string { return p.WorkDir() + "/gemini-settings.json" }

// GeminiHomeSettings is the home's own settings file, Gemini CLI's user tier,
// which holds the run's settings over the home's while Gemini CLI runs
// (gemini_helper.mjs's settings mode). Not the system tier: Gemini CLI reads
// that only from a file root owns, in directories root owns and nobody else
// may write, up to `/`, and drops the whole file otherwise, so a run by any
// other user would lose its bridge, hooks and sign-in (measured on 0.70.0).
func (p Paths) GeminiHomeSettings() string { return p.GeminiDir() + "/settings.json" }

// GeminiHeldSettings is the home's own settings file, held in the run's work
// directory while the run's stand in its place ([GeminiCLI.homeRestore]): empty
// where the home had none.
func (p Paths) GeminiHeldSettings() string { return p.WorkDir() + "/gemini-home-settings.json" }

// GeminiPolicy is the run's policy file ([geminiPolicy]), in the home's
// policies directory, which Gemini CLI reads every file of.
func (p Paths) GeminiPolicy() string { return p.GeminiDir() + "/policies/crewlet.toml" }

// GeminiAdminPolicy is the directory of the run's admin-tier policy
// ([geminiAdminPolicy]), which `--admin-policy` names.
func (p Paths) GeminiAdminPolicy() string { return p.WorkDir() + "/gemini-admin-policy" }

// GeminiSkills is where a run's own skills are written, a directory each
// ([geminiSkillName]): the home's skills directory, which Gemini CLI lists
// in its system prompt.
func (p Paths) GeminiSkills() string { return p.GeminiDir() + "/skills" }

// GeminiProjects is Gemini CLI's registry naming each working directory's
// project slug, which the run's helper pins to [GeminiProjectSlug] before every
// run so the run's session is filed where the runner reads it.
func (p Paths) GeminiProjects() string { return p.GeminiDir() + "/projects.json" }

// GeminiSlugDir is the pinned slug's directory, which a carried session's
// archive names its chat file relative to ([geminiArchiveFormat]).
func (p Paths) GeminiSlugDir() string { return p.GeminiDir() + "/tmp/" + GeminiProjectSlug }

// GeminiChats is the directory Gemini CLI files a run's session under.
func (p Paths) GeminiChats() string { return p.GeminiSlugDir() + "/chats" }

// GeminiUsageBefore lists the responses a continued session's chat file holds
// as its run starts, which the run's spend leaves out ([geminiScript]).
func (p Paths) GeminiUsageBefore() string { return p.WorkDir() + "/gemini-usage-before" }

// GeminiProjectSlug is the project slug every run's working directory is
// pinned to: one constant, so the session sits at the same path whatever the
// box's working directory is called (Gemini CLI otherwise slugs its basename).
const GeminiProjectSlug = "crewlet"

// GeminiHelper is the helper the run's script runs ([geminiHelperScript]).
func (p Paths) GeminiHelper() string { return p.WorkDir() + "/gemini-helper.mjs" }

// GeminiTake is the take script both hooks run ([geminiTakeScript]).
func (p Paths) GeminiTake() string { return p.WorkDir() + "/gemini-take.mjs" }

// GeminiTakeConfig is what the take script reads: the bridge's endpoint and
// the routes, so no token is on a hook's command line or in any process's
// environment.
func (p Paths) GeminiTakeConfig() string { return p.WorkDir() + "/gemini-take.json" }

// GeminiDefaultSystem is Gemini CLI's prompt as the script exports it
// ([geminiScript]).
func (p Paths) GeminiDefaultSystem() string { return p.WorkDir() + "/gemini-default-system.md" }

// GeminiExportErr is what the export printed.
func (p Paths) GeminiExportErr() string { return p.WorkDir() + "/gemini-export.err" }

// GeminiSystem is the export and the run's own text ([Paths.System]) joined,
// which GEMINI_SYSTEM_MD names.
func (p Paths) GeminiSystem() string { return p.WorkDir() + "/gemini-system.md" }

// GeminiTakes is where the take script records each take it handed Gemini CLI
// in session, a line of its token and its text ([geminiRecordedTake]), which
// that session's receipts are read against. One file per session and no run's
// artifact, so a later launch in the box leaves an earlier job's record whole.
func (p Paths) GeminiTakes(session string) string {
	return p.GeminiTakesDir() + "/" + session + ".jsonl"
}

// GeminiTakesDir holds every session's record of takes ([Paths.GeminiTakes]),
// never pruned: a box serves one turn's launches and is reclaimed as its run
// settles (code-sandbox.md, "Reuse across launches"), so it holds a few.
func (p Paths) GeminiTakesDir() string { return p.WorkDir() + "/gemini-takes" }

// GeminiUnbridged is where the BeforeTool hook says why it stopped a call
// named inside the bridge's names, which the run's script reads once Gemini
// CLI exits ([geminiBridgedHook]).
func (p Paths) GeminiUnbridged() string { return p.WorkDir() + "/gemini-unbridged" }

// GeminiNoteHelper is where a Gemini CLI run's launch writes the note helper
// its checkout gate and checkouts' hook run ([geminiCheckoutHome]).
func (p Paths) GeminiNoteHelper() string { return p.WorkDir() + "/gemini-notehelper" }

// GeminiCheckouts is the run's checkout state ([geminiCheckoutHome]): the
// checkouts handed, the lock, the skip log and the hook's config, remade by
// every run.
func (p Paths) GeminiCheckouts() string { return p.WorkDir() + "/gemini-checkouts" }

// GeminiCheckoutLaunch is the run's checkout-launch config, which its launch
// writes ([geminiCheckouts]) and its script hands the note helper.
func (p Paths) GeminiCheckoutLaunch() string { return p.WorkDir() + "/gemini-checkout-launch.json" }

// GeminiCheckoutSections is where checkout-launch would write the checkouts'
// instructions: none for Gemini CLI, which reads them itself.
func (p Paths) GeminiCheckoutSections() string { return p.WorkDir() + "/gemini-checkout-sections.md" }

// GeminiCheckoutSkills lists, a name a line, the checkouts' skills a run
// copied into [Paths.GeminiSkills], so the run's end removes them and none of
// the home's, or the next run's gate where the run was killed first.
func (p Paths) GeminiCheckoutSkills() string { return p.WorkDir() + "/gemini-checkout-skills" }

// GeminiCheckoutRoots lists the root of each checkout the launch handed, a
// line each, which the script names with --include-directories.
func (p Paths) GeminiCheckoutRoots() string { return p.WorkDir() + "/gemini-checkout-roots" }
