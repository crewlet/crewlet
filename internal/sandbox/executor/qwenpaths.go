package executor

// QwenDir is where Qwen Code keeps its settings and sessions: the box home's
// `.qwen`, which only QWEN_HOME moves, and the wrapper unsets it
// ([QwenCode.Output]).
func (p Paths) QwenDir() string { return p.Home + "/.qwen" }

// QwenSettings is the run's settings file ([qwenSettings]), Qwen Code's
// system tier (QWEN_CODE_SYSTEM_SETTINGS_PATH, [QwenCode.Pins]): read after
// the home's and the working directory's own settings, which it overrides key
// by key while their MCP servers, hooks and skill directories stay beside the
// run's.
func (p Paths) QwenSettings() string { return p.WorkDir() + "/qwen-settings.json" }

// QwenSkills is where a run's own skills are written, a directory each
// ([qwenSkillName]): a directory of the run's that the settings name in
// `skills.directories`, apart from the home's own skills directory, which
// Qwen Code would list at the same level whatever the run wrote.
func (p Paths) QwenSkills() string { return p.WorkDir() + "/qwen-skills" }

// QwenHelper is the helper the run's script starts as its harness and its
// skill hook runs ([qwenHelperScript]).
func (p Paths) QwenHelper() string { return p.WorkDir() + "/qwen-helper.mjs" }

// QwenHelperConfig is what the helper reads: the servers to wait for and the
// routes, so no token is on a command line or in any process's environment.
func (p Paths) QwenHelperConfig() string { return p.WorkDir() + "/qwen-helper.json" }

// QwenRunStart is where the box's last run began in its session: the session's
// id and the count of its chat file's lines as the run started
// ([qwenScript]), which the run's notes are confirmed past ([qwenNotesHeld]).
func (p Paths) QwenRunStart() string { return p.WorkDir() + "/qwen-run-start" }
