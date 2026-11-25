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

// QwenNoteHelper is where a Qwen Code run's launch writes the note helper its
// checkout gate and checkouts' hook run ([qwenCheckoutHome]).
func (p Paths) QwenNoteHelper() string { return p.WorkDir() + "/qwen-notehelper" }

// QwenCheckouts is the run's checkout state ([qwenCheckoutHome]): the
// checkouts handed, the lock, the skip log and the hook's config, remade by
// every run.
func (p Paths) QwenCheckouts() string { return p.WorkDir() + "/qwen-checkouts" }

// QwenCheckoutLaunch is the run's checkout-launch config, which its launch
// writes ([qwenCheckouts]) and its script hands the note helper.
func (p Paths) QwenCheckoutLaunch() string { return p.WorkDir() + "/qwen-checkout-launch.json" }

// QwenCheckoutSections is where checkout-launch writes the checkouts'
// instructions, which the gate writes after the home's own into ~/.qwen/QWEN.md.
func (p Paths) QwenCheckoutSections() string { return p.WorkDir() + "/qwen-checkout-sections.md" }

// QwenHomeInstructions is the home's own ~/.qwen/QWEN.md, held in the work
// directory while the run's, the home's with its checkouts' after it, stands
// in its place ([qwenCheckoutHome], [QwenCode.homeRestore]).
func (p Paths) QwenHomeInstructions() string { return p.WorkDir() + "/qwen-home-QWEN.md" }

// QwenCheckoutSkills lists, a name a line, the checkouts' skills a run copied
// into the home's ~/.qwen/skills, so the run's end removes them and none of
// the home's, or the next run's gate where the run was killed first.
func (p Paths) QwenCheckoutSkills() string { return p.WorkDir() + "/qwen-checkout-skills" }

// QwenContextHomes lists the home's own context files Qwen Code reads, a path
// a line, as the run's helper resolves context.fileName before the checkout
// gate ([qwenCheckoutsGate]), the first that is there taking the checkouts'
// sections.
func (p Paths) QwenContextHomes() string { return p.WorkDir() + "/qwen-context-homes" }
