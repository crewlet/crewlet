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

// maxCodexSocketPath is the longest [Paths.CodexSocket] a box can listen on:
// a Linux sockaddr_un holds 108 bytes, its terminating NUL among them.
const maxCodexSocketPath = 107
