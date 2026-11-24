package executor

import (
	"strings"
)

// GrokHome is Grok Build's own directory in the box home, which it reads its
// settings from and files its sessions under. GROK_HOME is unset for the
// harness ([grokSwitchesOff]), so this is the one place it looks.
func (p Paths) GrokHome() string { return p.Home + "/.grok" }

// GrokConfig is the run's settings file, the box's own with the run's tables
// laid in by every run ([grokMergeConfig]); GrokConfigFresh is a fresh
// session's, wired to the job's other token, which the script moves over it
// where it starts one.
func (p Paths) GrokConfig() string { return p.GrokHome() + "/config.toml" }

// GrokConfigBox is the box's own ~/.grok/config.toml as it stood before a Grok
// Build run wrote it ([grokBoxConfig]), which the run's settings start from,
// kept until the run puts it back ([grokRestoreConfig]). The clear leaves it:
// a run cut off before that leaves the box's own here.
func (p Paths) GrokConfigBox() string { return p.WorkDir() + "/grok-config-box.toml" }

// GrokConfigOurs is the names the run's servers stand under in
// [Paths.GrokConfig], one a line, recorded before they are written there and
// removed once the box's own settings are back: the guard holds Grok to them
// ([grokGuard]), and the next run's start takes a cut-off run's out of the
// settings as they then stand ([grokLeftOut]).
func (p Paths) GrokConfigOurs() string { return p.WorkDir() + "/grok-config-ours" }

// GrokConfigFresh is [Paths.GrokConfig] for a session a continuing run's script
// starts of its own ([grokSessionGate]).
func (p Paths) GrokConfigFresh() string { return p.WorkDir() + "/grok-config-fresh.toml" }

// GrokSessionsKey is the directory Grok Build files the sessions of a run in
// the box home under: the home's path with every byte but a letter, a digit,
// `-`, `_`, `.` and `~` written `%XX` (measured on 1.0.92). A session runs in
// the directory its key names, so a carried one is restored under this box's
// key ([grokSessionGate]).
func (p Paths) GrokSessionsKey() string { return p.GrokSessions() + "/" + grokPathKey(p.Home) }

// GrokSessions is the directory Grok Build files every session under, one
// directory per working directory's key: a subagent started in another
// directory is filed under that one's ([grokPack]).
func (p Paths) GrokSessions() string { return p.GrokHome() + "/sessions" }

// grokPathKey is dir as Grok Build names its sessions' directory after it.
func grokPathKey(dir string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := range len(dir) {
		c := dir[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&15])
		}
	}
	return b.String()
}

// GrokSkills is where a Grok Build run's own skills are written, one directory
// each, named as listed (`crewlet:<name>`): the box home's `.grok/skills`,
// which Grok lists by each file's absolute path (measured on 1.0.92).
func (p Paths) GrokSkills() string { return p.GrokHome() + "/skills" }

// GrokAgent is the run's agent file, whose body Grok Build appends to its own
// system prompt: the run's standing instructions ([grokAgentFile]).
func (p Paths) GrokAgent() string { return p.WorkDir() + "/grok-agent.md" }

// GrokRules is the run's standing instructions as a JSON string's content, which
// a continuing run's script writes into its session's first line
// ([grokRenewRules]).
func (p Paths) GrokRules() string { return p.WorkDir() + "/grok-rules.json" }

// GrokTakeConfig is the note helper's take config ([grokTake]), which every
// hook of the run reads; GrokTakeConfigFresh is a fresh session's.
func (p Paths) GrokTakeConfig() string { return p.WorkDir() + "/grok-take.json" }

// GrokTakeConfigFresh is [Paths.GrokTakeConfig] for a session a continuing run's
// script starts of its own.
func (p Paths) GrokTakeConfigFresh() string { return p.WorkDir() + "/grok-take-fresh.json" }

// GrokThread is the file a run's script writes its main session's id to once
// its gate has named it, which a note hook reads so that a subagent's firing
// takes nothing ([grokHooks]).
func (p Paths) GrokThread() string { return p.WorkDir() + "/grok-thread" }

// GrokHookIDs is the ids Grok Build gives the run's hooks, one a line, as
// `grok inspect --json` lists them and `~/.grok/disabled-hooks` names them
// ([grokHookIDs]), which the guard reads ([grokGuard]).
func (p Paths) GrokHookIDs() string { return p.WorkDir() + "/grok-hook-ids" }

// GrokDisabledHooks is the box home's list of the hooks Grok Build skips, one
// id a line ([grokGuard]).
func (p Paths) GrokDisabledHooks() string { return p.GrokHome() + "/disabled-hooks" }

// GrokNoteHelper is the note helper's build for the box (internal/notehelper),
// written at each launch of a run wired to its notes or skills.
func (p Paths) GrokNoteHelper() string { return p.WorkDir() + "/grok-notehelper" }
