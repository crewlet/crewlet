package executor

// PiAgentDir is the directory a Pi run keeps its state in, pinned as
// PI_CODING_AGENT_DIR ([Pi.Pins]): the box home's `.pi/agent`, where the
// harness keeps it by default and where a local box is seeded with the
// entry's login (`auth.json`, which pi refreshes in place).
func (p Paths) PiAgentDir() string { return p.Home + "/.pi/agent" }

// PiSessions is the directory a Pi run's sessions are kept in, one file each
// ([Paths.PiSessionFile]), outside the agent directory's own `sessions/`.
func (p Paths) PiSessions() string { return p.Home + "/.pi/sessions" }

// PiSessionFile is session's file: the conversation, which the run's script
// opens with `--session` at this one path in every box, its header written
// by the script ([piSessionGate]), so the confirm reader, the pack and a
// restore name it without a search.
func (p Paths) PiSessionFile(session string) string { return p.PiSessions() + "/" + session + ".jsonl" }

// PiRun is the directory of what a Pi run is written of its own: its
// extension and its skills, removed whole before the next run's are written
// ([Pi.artifactDirs]).
func (p Paths) PiRun() string { return p.Home + "/.pi/crewlet" }

// PiExtension is the run's extension (pi_extension.mjs), loaded with `-e`.
func (p Paths) PiExtension() string { return p.PiRun() + "/extension.mjs" }

// PiExtensionFresh is where a continuing run's extension is written a second
// time, wired to the job's other token, for a session its script starts in
// the continued one's place ([sandbox.RunRequest.FreshWiring]), which the
// script moves over [Paths.PiExtension] where it starts one and removes where
// it continues ([piSessionGate]).
func (p Paths) PiExtensionFresh() string { return p.PiRun() + "/extension-fresh.mjs" }

// PiSkills is the directory of the run's own skills, a directory each named as
// the skill is listed, which the run names with `--skill`.
func (p Paths) PiSkills() string { return p.PiRun() + "/skills" }
