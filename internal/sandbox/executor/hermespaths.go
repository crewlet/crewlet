package executor

// HermesHome is the directory a Hermes run keeps its state in, pinned as
// HERMES_HOME: the box home's `.hermes`, where Hermes keeps it by default, so
// the box's own configuration, skills, plugins and memories load as a person's
// session loads them. Every Hermes path under the harness's home is derived
// from it.
func (p Paths) HermesHome() string { return p.Home + "/.hermes" }

// HermesStateDB is Hermes's session store, the SQLite database a session lives
// in ([hermesCarry]).
func (p Paths) HermesStateDB() string { return p.HermesHome() + "/state.db" }

// HermesGatewayPID is the file Hermes's gateway keeps in [Paths.HermesHome]
// while it runs, a JSON object naming its pid ([hermesStopGateway]).
func (p Paths) HermesGatewayPID() string { return p.HermesHome() + "/gateway.pid" }

// HermesSkills is the skill root under [Paths.HermesHome] Hermes lists every
// `<dir>/SKILL.md` of, beside the box's own skills; a run's skills are
// installed there by its script ([hermesInstall]).
func (p Paths) HermesSkills() string { return p.HermesHome() + "/skills" }

// HermesPlugin is where the run's script installs the runner's plugin
// ([hermesPluginFiles]), a user plugin Hermes loads in its gateway.
func (p Paths) HermesPlugin() string { return p.HermesHome() + "/plugins/" + hermesPluginName }

// HermesManaged is the directory a Hermes run names as HERMES_MANAGED_DIR: its
// `config.yaml` is merged over the box's own per leaf ([hermesOverlay]), so the
// run's MCP servers and web denial reach Hermes and the box's configuration
// stays as it is. Under WorkDir, so a `git clean` cannot take it.
func (p Paths) HermesManaged() string { return p.WorkDir() + "/hermes-managed" }

// HermesOverlay is the overlay Hermes reads ([Paths.HermesManaged]); the
// session gate moves the one the run starts with into place.
func (p Paths) HermesOverlay() string { return p.HermesManaged() + "/config.yaml" }

// HermesOverlayStaged is the overlay WriteConfig writes for the session the run
// was asked to work in, and HermesOverlayFresh the same wired to a session of
// the run's own ([sandbox.RunRequest.FreshWiring]).
func (p Paths) HermesOverlayStaged() string { return p.WorkDir() + "/hermes-overlay.yaml" }

// HermesOverlayFresh is [Paths.HermesOverlayStaged] wired to the fresh session.
func (p Paths) HermesOverlayFresh() string { return p.WorkDir() + "/hermes-overlay-fresh.yaml" }

// HermesStage is where WriteConfig stages what the script installs into the
// Hermes home once the release is known to be one the runner drives: the
// plugin and the run's skills ([hermesInstall]).
func (p Paths) HermesStage() string { return p.WorkDir() + "/hermes-stage" }

// HermesPluginConfig is the plugin's settings for the run: the record and
// route files and the skills' digests, no secret ([hermesPluginSettings]).
func (p Paths) HermesPluginConfig() string { return p.WorkDir() + "/hermes-plugin.json" }

// HermesRecord is the file the plugin appends one line to for every response
// Hermes's model gives ([hermesRequestEvent]): its usage, model, stop and
// window.
func (p Paths) HermesRecord() string { return p.WorkDir() + "/hermes-requests.jsonl" }

// HermesNotesURL is the file holding the job's notes route for the plugin, read
// at each poll: the route carries the job's token, so it never reaches argv
// nor any environment.
func (p Paths) HermesNotesURL() string { return p.WorkDir() + "/hermes-notes-url" }

// HermesNotesURLFresh is [Paths.HermesNotesURL] under the job's other bridge
// token, for a session the run's script starts of its own.
func (p Paths) HermesNotesURLFresh() string { return p.WorkDir() + "/hermes-notes-url-fresh" }

// HermesSkillsURL is the file holding the job's skill-load route for the
// plugin, read as [Paths.HermesNotesURL] is.
func (p Paths) HermesSkillsURL() string { return p.WorkDir() + "/hermes-skills-url" }

// HermesSkillsURLFresh is [Paths.HermesSkillsURL] under the job's other token.
func (p Paths) HermesSkillsURLFresh() string { return p.WorkDir() + "/hermes-skills-url-fresh" }

// HermesBrief is the brief as the run's script keeps it, so a relaunch after
// Hermes asks for a lost turn to be discarded hands the same words again.
func (p Paths) HermesBrief() string { return p.WorkDir() + "/hermes-brief.md" }

// HermesStderr is where the Hermes client's error stream goes while it runs:
// its first line names the session Hermes stored the run under
// ([hermesStoredLine]); a failed run's error stream ends with its tail.
func (p Paths) HermesStderr() string { return p.WorkDir() + "/hermes-stderr" }

// HermesSessions is the box's own index of the sessions it holds: one line per
// session, `<runner's id> <Hermes's stored id>`, written once Hermes names it.
func (p Paths) HermesSessions() string { return p.WorkDir() + "/hermes-sessions" }

// HermesPolicy is the launch a session was created under, the policy Hermes
// freezes with it ([hermesPolicy]), one file per session the box holds.
func (p Paths) HermesPolicy(session string) string { return p.WorkDir() + "/hermes-policy-" + session }

// HermesCarry is the session reader and writer the run's script runs under
// Hermes's own interpreter ([hermesCarryRun]), written at every launch.
func (p Paths) HermesCarry() string { return p.WorkDir() + "/hermes-carry.py" }

// HermesRecordRead is where a confirm of the run's notes has the carry script
// dump the session's record to read it back ([Hermes.notesHeld]).
func (p Paths) HermesRecordRead() string { return p.WorkDir() + "/hermes-record-read.jsonl" }

// HermesCarryErrors is where the carry script's error stream goes, which a
// failed pack's error names.
func (p Paths) HermesCarryErrors() string { return p.WorkDir() + "/hermes-carry.err" }

// HermesEnabled marks that the box's Hermes configuration enables the runner's
// plugin, so later runs in the box skip the enabling command.
func (p Paths) HermesEnabled() string { return p.WorkDir() + "/hermes-plugin-enabled" }
