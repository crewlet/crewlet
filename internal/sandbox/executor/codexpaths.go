package executor

// CodexConfig is the configuration file a Codex run's app-server reads, under
// [Paths.CodexHome]: the run's MCP servers and its standing instructions,
// which the script moves into place from [Paths.CodexConfigStaged] (or, for a
// session of the run's own, [Paths.CodexConfigFresh]) before the server starts.
//
// A FILE, never `-c` on argv: a bridge server's url carries the job's token in
// its path (internal/api/mcpbridge), and a command line is readable by every
// account on a `direct` box's host. CODEX_HOME is the box's own, so the file
// Codex calls the user's configuration is the run's, rewritten every run; the
// run's posture still goes on argv, above anything a repository's own
// configuration sets ([codexPosture]).
func (p Paths) CodexConfig() string { return p.CodexHome() + "/config.toml" }

// CodexConfigStaged is where [Codex.WriteConfig] writes a run's configuration
// for the session it was asked to work in, beside the run's other artefacts.
func (p Paths) CodexConfigStaged() string { return p.WorkDir() + "/codex-config.toml" }

// CodexConfigFresh is the run's configuration under the job's other bridge
// token, for a session its script starts in place of the one it continues
// ([sandbox.RunRequest.FreshWiring]).
func (p Paths) CodexConfigFresh() string { return p.WorkDir() + "/codex-config-fresh.toml" }

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

// CodexNoteHelper is the note helper binary a Codex run wired to its notes
// runs beside its harness (internal/notehelper), written at every launch.
func (p Paths) CodexNoteHelper() string { return p.WorkDir() + "/notehelper" }

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

// maxCodexSocketPath is the longest [Paths.CodexSocket] a box can listen on:
// a Linux sockaddr_un holds 108 bytes, its terminating NUL among them.
const maxCodexSocketPath = 107
