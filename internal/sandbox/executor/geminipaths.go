package executor

// GeminiDir is where Gemini CLI keeps its state, settings and sessions: the box
// home's `.gemini`, which no setting or variable but GEMINI_CLI_HOME moves, and
// the wrapper unsets that ([GeminiCLI.Output]).
func (p Paths) GeminiDir() string { return p.Home + "/.gemini" }

// GeminiSettings is the run's settings file ([geminiSettings]), the home's
// own: Gemini CLI reads no settings path it is handed, and a workspace's
// `.gemini/settings.json` is the repository's, which a commit would carry.
func (p Paths) GeminiSettings() string { return p.GeminiDir() + "/settings.json" }

// GeminiPolicy is the run's policy file ([geminiPolicy]), in the home's
// policies directory, which Gemini CLI reads every file of.
func (p Paths) GeminiPolicy() string { return p.GeminiDir() + "/policies/crewlet.toml" }

// GeminiSkills is where a run's own skills are written, a directory each
// ([geminiSkillName]): the home's skills directory, which Gemini CLI lists
// in its system prompt.
func (p Paths) GeminiSkills() string { return p.GeminiDir() + "/skills" }

// GeminiProjects is Gemini CLI's registry naming each working directory's
// project slug, which the run's helper pins to [GeminiProjectSlug] before every
// run so the run's session is filed where the runner reads it.
func (p Paths) GeminiProjects() string { return p.GeminiDir() + "/projects.json" }

// GeminiChats is the directory the pinned slug files a run's session under.
func (p Paths) GeminiChats() string { return p.GeminiDir() + "/tmp/" + GeminiProjectSlug + "/chats" }

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
