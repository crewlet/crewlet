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

// MuseSettingsFresh is where a continuing run's settings are written a second
// time, wired to the job's other token, for a session its script starts in the
// continued one's place ([sandbox.RunRequest.FreshWiring]); the script moves it
// over [Paths.MuseSettings] where it starts one and removes it where it
// continues ([museSessionGate]). Beside the run's other artefacts, never in
// the directory Muse reads.
func (p Paths) MuseSettingsFresh() string { return p.WorkDir() + "/muse-settings-fresh.json" }

// MuseChildLines is where a run's script keeps, before its harness starts, how
// many lines each subagent's record of the session already holds, which its
// usage after the run is read past ([museChildUsage]).
func (p Paths) MuseChildLines() string { return p.WorkDir() + "/muse-child-lines" }
