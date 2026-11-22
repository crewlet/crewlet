package executor

// MuseConfig is the directory Muse Code reads its user settings from: the box
// home's `.config/muse`, where a local box is seeded with the entry's login
// (`auth.json`) and where the run's settings are written whole
// ([Muse.WriteConfig]). XDG_CONFIG_HOME is unset for the harness
// ([museSwitchesOff]), so this is the one place it looks.
func (p Paths) MuseConfig() string { return p.Home + "/.config/muse" }

// MuseSettings is the run's own settings file, written whole by every run
// ([museSettings]).
func (p Paths) MuseSettings() string { return p.MuseConfig() + "/settings.json" }

// MuseSessions is the directory Muse Code files every session's directory in,
// by the day it began: `<YYYY>/<MM>/<DD>/<session>/` (measured on 1.4.25).
// XDG_DATA_HOME is unset for the harness, so this is the one place.
func (p Paths) MuseSessions() string { return p.Home + "/.local/share/muse/sessions" }

// MuseChildLines is where a run's script keeps, before its harness starts, how
// many lines each subagent's record of the session already holds, which its
// usage after the run is read past ([museChildUsage]).
func (p Paths) MuseChildLines() string { return p.WorkDir() + "/muse-child-lines" }

// MuseSkills is where a Muse Code run's own skills are written, one
// directory each, named as listed (`crewlet:<name>`): the box home's
// `.agents/skills`, which Muse catalogues as the user's (measured on 1.4.25).
// The clear removes it whole ([Muse.artifacts]).
func (p Paths) MuseSkills() string { return p.Home + "/.agents/skills" }

// MuseConfigSkills is the box home's `.config/muse/skills`, which Muse
// catalogues as the user's skills too (measured on 1.4.25). No run writes it;
// the clear removes it ([Muse.artifacts]).
func (p Paths) MuseConfigSkills() string { return p.MuseConfig() + "/skills" }

// MuseInstructions is the box home's `.config/muse/AGENTS.md`, which Muse adds
// to every request as the user's own instructions (measured on 1.4.25). No run
// writes it; the clear removes it ([Muse.artifacts]).
func (p Paths) MuseInstructions() string { return p.MuseConfig() + "/AGENTS.md" }

// MusePlugins is where Muse keeps the plugins installed in the box home: the
// record of them (`installed.json`), their packages and their data. An
// installed plugin's skills load with no approval (measured on 1.4.25), so the
// clear removes it whole ([Muse.artifacts]) and a run's script installs the
// run's own notes plugin afresh; Muse stages its built-in plugins again itself.
func (p Paths) MusePlugins() string { return p.Home + "/.local/share/muse/plugins" }

// MuseServers is the directory of a Muse Code run's MCP server definitions,
// one JSON file each, which its script adds to its settings with `muse mcp
// add-json` once the notes plugin is approved: an approval rewrites the
// settings and drops a server's `required` and `tool_timeout_sec` (measured).
// MuseServersFresh holds a fresh session's, wired to the job's other token.
func (p Paths) MuseServers() string { return p.WorkDir() + "/muse-servers" }

// MuseServersFresh is [Paths.MuseServers] for a session a continuing run's
// script starts of its own ([museSessionGate]).
func (p Paths) MuseServersFresh() string { return p.WorkDir() + "/muse-servers-fresh" }

// MuseTakeConfig is the note helper's take config ([museTake]), which every
// hook of the run reads; MuseTakeConfigFresh is a fresh session's, which the
// script moves over it where it starts one.
func (p Paths) MuseTakeConfig() string { return p.WorkDir() + "/muse-take.json" }

// MuseTakeConfigFresh is [Paths.MuseTakeConfig] for a session a continuing
// run's script starts of its own.
func (p Paths) MuseTakeConfigFresh() string { return p.WorkDir() + "/muse-take-fresh.json" }

// MuseNotesPlugin is the notes plugin's package ([musePluginManifest]), which
// the run's script installs and approves.
func (p Paths) MuseNotesPlugin() string { return p.WorkDir() + "/muse-notes-plugin" }

// MuseNotesInstall is Muse Code's answer to the script's install of the notes
// plugin (`muse plugins install --json`), whose `installed.package_sha256`
// every run of the plugin's hooks records as its `source_digest`: the confirm
// counts no other hook's ([museInstalledDigest]).
func (p Paths) MuseNotesInstall() string { return p.WorkDir() + "/muse-notes-install.json" }
