package executor

// CodexSocket is the unix socket a Codex run's app-server listens on and its
// `exec --remote` and note helper dial. In the work directory, which the box's
// user owns and no other account may write: Codex refuses a socket whose
// parent another user could replace entries in (measured on 0.171.0). Held to
// [maxCodexSocketPath] by [Codex.WriteConfig].
func (p Paths) CodexSocket() string { return p.WorkDir() + "/codex.sock" }

// CodexThread is where a Codex run's script writes the thread id `exec
// --json` reported (`thread.started`), for the note helper to steer that
// thread: one line, moved into place whole.
func (p Paths) CodexThread() string { return p.WorkDir() + "/codex-thread" }

// CodexRunStart is where the box's last run began in its session: the thread
// id and the count of its rollout's lines as the run started, written once the
// thread is known ([codexScript]), which the run's notes are confirmed past
// ([codexRunFrom]).
func (p Paths) CodexRunStart() string { return p.WorkDir() + "/codex-run-start" }

// CodexNoteHelper is the note helper binary a Codex run wired to its notes
// runs beside its harness (internal/notehelper), written at every launch.
func (p Paths) CodexNoteHelper() string { return p.WorkDir() + "/notehelper" }

// CodexHooksOffBy is each file and key that has Codex run no hook of the
// run's, `; `-separated, empty where none ([codexHooksSwitchedOff]): written by
// the launch where the box holds the helper, read by the script, which then
// starts Codex without its nested hook ([codexScript]).
func (p Paths) CodexHooksOffBy() string { return p.WorkDir() + "/codex-hooks-off-by" }

// CodexNotesURL is the file holding the job's notes route for the note
// helper, read into the helper's environment alone: the route carries the
// job's token, so it never reaches argv or the harness's environment.
func (p Paths) CodexNotesURL() string { return p.WorkDir() + "/codex-notes-url" }

// CodexNotesURLFresh is [Paths.CodexNotesURL] under the job's other bridge
// token, for a session the run's script starts of its own.
func (p Paths) CodexNotesURLFresh() string { return p.WorkDir() + "/codex-notes-url-fresh" }

// CodexSkills is the skill root under [Paths.CodexHome] Codex lists every
// `<dir>/SKILL.md` of, even with the user's configuration ignored, and where a
// run's own skills are written, each in a directory named as it is listed
// ([sandbox.Skill.Listed]).
func (p Paths) CodexSkills() string { return p.CodexHome() + "/skills" }

// CodexRepo is the run's directory of its repositories' state ([Codex.checkoutHome]):
// the checkouts handed, the lock and the nested hook's config, remade
// by every run.
func (p Paths) CodexRepo() string { return p.WorkDir() + "/codex-repo" }

// CodexRepoAgents is where checkout-launch writes the instructions of a run's
// repositories ([Codex.checkoutHome]), which the gate then writes after the
// home's own into CODEX_HOME/AGENTS.md through a link as the home keeps it.
func (p Paths) CodexRepoAgents() string { return p.WorkDir() + "/codex-repo-agents.md" }

// CodexRepoSkills lists, a name a line, the repositories' skills a run copied
// into [Paths.CodexSkills] ([Codex.checkoutHome]), so the run's end removes them
// and none of the home's, or the next run's gate where the run was killed first.
func (p Paths) CodexRepoSkills() string { return p.WorkDir() + "/codex-repo-skills" }

// CodexCapFired is the marker the note helper writes, before it interrupts the
// turn, where the run's round cap ended it (internal/notehelper's -cap-marker).
func (p Paths) CodexCapFired() string { return p.WorkDir() + "/codex-cap-fired" }

// CodexHelperLost is the mark the run's guard leaves where the note helper
// ended while Codex still worked ([codexStartHelper]).
func (p Paths) CodexHelperLost() string { return p.WorkDir() + "/codex-helper-lost" }

// CodexSkillsURL is the file holding the job's skill-load route for the note
// helper, read into its environment alone, as [Paths.CodexNotesURL] is.
func (p Paths) CodexSkillsURL() string { return p.WorkDir() + "/codex-skills-url" }

// CodexSkillsURLFresh is [Paths.CodexSkillsURL] under the job's other bridge
// token, for a session the run's script starts of its own.
func (p Paths) CodexSkillsURLFresh() string { return p.WorkDir() + "/codex-skills-url-fresh" }

// CodexRenew is the config of the note helper's codex-renew, written for a run
// that continues a session: the standing instructions its rollout is renewed
// to before the resume ([codexSessionGate]), as config.toml hands them.
func (p Paths) CodexRenew() string { return p.WorkDir() + "/codex-renew.json" }

// maxCodexSocketPath is the longest [Paths.CodexSocket] a box can listen on:
// a Linux sockaddr_un holds 108 bytes, its terminating NUL among them.
const maxCodexSocketPath = 107
