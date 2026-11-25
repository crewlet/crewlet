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
// extension, its skills, the note helper and its checkouts' state and skills,
// removed whole before the next run's are written ([Pi.artifactDirs]).
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

// PiNoteHelper is the note helper a Pi run writes at every launch
// ([writeNoteHelper]): its gate runs checkout-launch, and its extension the
// pi-nested adapter after each tool call ([piCheckoutHome]).
func (p Paths) PiNoteHelper() string { return p.PiRun() + "/notehelper" }

// PiCheckouts is the run's checkout state ([piCheckoutHome]): the checkouts
// handed, the lock and the nested adapter's config, remade by every run.
func (p Paths) PiCheckouts() string { return p.PiRun() + "/checkouts" }

// PiCheckoutLaunch is the run's checkout-launch config ([piCheckouts]).
func (p Paths) PiCheckoutLaunch() string { return p.PiRun() + "/checkout-launch.json" }

// PiCheckoutSections is where checkout-launch writes the run's checkouts'
// instructions, which the gate then writes after the home's own into the
// agent directory's instruction file ([piCheckoutHome]).
func (p Paths) PiCheckoutSections() string { return p.PiRun() + "/checkout-sections.md" }

// PiCheckoutSkills is the directory the checkouts' skills are copied into,
// which the run names with a `--skill` of its own ([piCheckoutHome]).
func (p Paths) PiCheckoutSkills() string { return p.PiRun() + "/checkout-skills" }

// PiCheckoutSkillsCopied lists, a name a line, the skills a run copied into
// [Paths.PiCheckoutSkills], so the run's end removes them, or the next run's
// gate where the run was killed first.
func (p Paths) PiCheckoutSkillsCopied() string { return p.PiRun() + "/checkout-skills-copied" }

// PiHomeInstructions is the agent directory's own instruction file, held
// while the run's stands in its place, outside [Paths.PiRun], which the
// clear removes: a killed run's copy waits there to be put back
// ([Pi.homeRestore]).
func (p Paths) PiHomeInstructions() string { return p.WorkDir() + "/pi-home-instructions.md" }
