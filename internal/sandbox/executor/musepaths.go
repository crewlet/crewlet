package executor

// MuseConfig is the directory Muse Code reads its user settings from: the box
// home's `.config/muse`, where a local box is seeded with the entry's login
// (`auth.json`) beside the home's own settings. XDG_CONFIG_HOME is unset for the harness
// ([museSwitchesOff]), so this is the one place it looks.
func (p Paths) MuseConfig() string { return p.Home + "/.config/muse" }

// MuseSettings is the box home's user settings, which hold a run's standing
// instructions after the home's own ([museMergeSystem]), its MCP servers,
// added and removed by name ([museWiring]), and Muse's own rewrite on the
// notes plugin's approval while it runs, and are put back as it ends.
func (p Paths) MuseSettings() string { return p.MuseConfig() + "/settings.json" }

// MuseHeldSettings is the box home's own settings as a run found them, held
// while the run's stand in their place ([museMergeSystem]) and put back as it
// ends ([museRestore]) or by the next clear ([Muse.mendHome]): empty where
// the home had none.
func (p Paths) MuseHeldSettings() string { return p.WorkDir() + "/muse-home-settings.json" }

// MuseSessions is the directory Muse Code files every session's directory in,
// by the day it began: `<YYYY>/<MM>/<DD>/<session>/` (measured on 1.4.25).
// XDG_DATA_HOME is unset for the harness, so this is the one place.
func (p Paths) MuseSessions() string { return p.Home + "/.local/share/muse/sessions" }

// MuseInstalled is Muse Code's record of the plugins installed in the home,
// each with whether it is enabled (`muse plugins disable` writes false).
func (p Paths) MuseInstalled() string { return p.Home + "/.local/share/muse/plugins/installed.json" }

// MuseChildLines is where a run's script keeps, before its harness starts, how
// many lines each subagent's record of the session already holds, which its
// usage after the run is read past ([museChildUsage]).
func (p Paths) MuseChildLines() string { return p.WorkDir() + "/muse-child-lines" }

// MuseSkills is where a Muse Code run's own skills are written, one
// directory each, named as listed (`crewlet:<name>`): the box home's
// `.agents/skills`, which Muse catalogues as the user's (measured on 1.4.25),
// beside the home's own ([writeMuseSkills]).
func (p Paths) MuseSkills() string { return p.Home + "/.agents/skills" }

// MuseServersAdded is the names of the MCP servers a run added to the box
// home's settings, one a line, by which its script takes them out as it ends
// ([museUnwire]), or the next clear where it was killed first ([Muse.mendHome]).
func (p Paths) MuseServersAdded() string { return p.WorkDir() + "/muse-servers-added" }

// MuseServers is the directory of a Muse Code run's MCP server definitions,
// one JSON file each, which its script adds to its settings with `muse mcp
// add-json` once the notes plugin is approved: an approval rewrites the
// settings and drops a server's `required` and `tool_timeout_sec` (measured).
// MuseServersFresh holds a fresh session's, wired to the job's other token.
func (p Paths) MuseServers() string { return p.WorkDir() + "/muse-servers" }

// MuseServersFresh is [Paths.MuseServers] for a session a continuing run's
// script starts of its own ([museSessionGate]).
func (p Paths) MuseServersFresh() string { return p.WorkDir() + "/muse-servers-fresh" }

// MuseTakeConfig is the note helper's take config ([oneShotTake]), which every
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

// MuseNoteHelper is the note helper's build for the box (internal/notehelper),
// written at each launch of a run wired to its notes or skills.
func (p Paths) MuseNoteHelper() string { return p.WorkDir() + "/muse-notehelper" }
