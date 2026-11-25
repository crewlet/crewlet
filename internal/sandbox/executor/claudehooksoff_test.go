package executor_test

import (
	"context"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// A HOME OR A REPOSITORY CANNOT SWITCH A CLAUDE CODE RUN'S HOOKS OR SKILLS OFF
// UNSEEN. 2.1.310 runs none of the run's hooks (notes, skills, checkouts,
// bridge guard) where the merged settings leave `disableAllHooks` true, and
// drops its skills where `enabledPlugins` or a `Skill` deny says so
// (measured); the run overrules neither. So a run starting a hook of the
// engine's is refused before it starts, naming file and key, a later
// set-back-on clearing nothing, but one whose only hook would be its
// checkouts' starts without it, saying so; it starts where the key is no
// boolean, the file no JSON, in the home's settings.local.json, nested, or
// allowManagedHooksOnly outside a managed file (measured). Red: drop the gate;
// let a later value clear an off; read a string as true; name no file; skip
// the skills' check; refuse the checkouts' alone; leave the guard ungated.
func TestClaudeCodesHooksCannotBeSwitchedOffUnseen(t *testing.T) {
	t.Parallel()
	const off = `{"disableAllHooks": true}`
	const said = "Claude Code would run none of the run's hooks"
	skills := sandbox.RunRequest{Brief: "go", Skills: handedSkills()[1:], SkillLoads: skillWiring()}
	notes := sandbox.RunRequest{Brief: "go", Steer: steerWiring}
	bridged := sandbox.RunRequest{Brief: "go", MCPServers: map[string]sandbox.MCPServer{
		"crewlet": {Transport: sandbox.TransportHTTP, URL: "http://127.0.0.1:7/mcp/tok", AlwaysLoaded: true}}}
	for name, tc := range map[string]struct {
		release, machine string
		req              sandbox.RunRequest
		switchOffCase
	}{
		"nothing switches them off": {req: skills},
		"the home's settings switch them off": {req: skills, switchOffCase: switchOffCase{
			plant: map[string]string{".claude/settings.json": off},
			fails: []string{said, "/.claude/settings.json sets disableAllHooks to true", "Remove that key"}}},
		"the repository's settings switch them off for the notes' hooks": {req: notes, switchOffCase: switchOffCase{
			plant: map[string]string{".claude/settings.json": `{"disableAllHooks": false}`,
				"workspace/.claude/settings.json": "{\n  \"disableAllHooks\"\n  :\n  true\n}"},
			fails: []string{said, "/workspace/.claude/settings.json sets disableAllHooks to true"}}},
		"the home's settings switch them off for the bridge's guard": {req: bridged, switchOffCase: switchOffCase{
			plant: map[string]string{".claude/settings.json": off},
			fails: []string{said, "bridge guard", "/.claude/settings.json sets disableAllHooks to true"}}},
		// No note helper build, so no checkouts' hook: the guard alone asks.
		"the home's settings switch them off for the bridge's guard, the helper unbuilt": {machine: "riscv64",
			req: bridged, switchOffCase: switchOffCase{plant: map[string]string{".claude/settings.json": off},
				fails: []string{said, "bridge guard", "/.claude/settings.json sets disableAllHooks to true"}}},
		"the repository's local settings switch them off": {req: skills, switchOffCase: switchOffCase{
			plant: map[string]string{"workspace/.claude/settings.local.json": off},
			fails: []string{said, "/workspace/.claude/settings.local.json sets disableAllHooks"}}},
		"the repository's false clears nothing of the home's true": {req: skills, switchOffCase: switchOffCase{
			plant: map[string]string{".claude/settings.json": off,
				"workspace/.claude/settings.json": `{"disableAllHooks": false}`},
			fails: []string{"/.claude/settings.json sets disableAllHooks to true"}}},
		"a string, a comment and a nested key decide nothing": {req: notes, switchOffCase: switchOffCase{
			plant: map[string]string{".claude/settings.json": `{"disableAllHooks": "true"}`,
				"workspace/.claude/settings.json":       "// off\n" + off,
				"workspace/.claude/settings.local.json": `{"env": {"disableAllHooks": true}}`}}},
		"the home's local settings are never read": {req: skills, switchOffCase: switchOffCase{
			plant: map[string]string{".claude/settings.local.json": off}}},
		"only managed settings can allow managed hooks alone": {req: skills, switchOffCase: switchOffCase{
			plant: map[string]string{".claude/settings.json": `{"allowManagedHooksOnly": true}`,
				"workspace/.claude/settings.json": `{"allowManagedHooksOnly": true}`}}},
		"the home's settings switch the skills' plugin off": {req: skills, switchOffCase: switchOffCase{
			plant: map[string]string{".claude/settings.json": `{"enabledPlugins": {"crewlet@inline": false}}`},
			fails: []string{`/.claude/settings.json sets enabledPlugins["crewlet@inline"] to false`}}},
		"the repository denies the seat skill": {req: skills, switchOffCase: switchOffCase{
			plant: map[string]string{"workspace/.claude/settings.json": `{"permissions": {"deny": ["Skill(crewlet:getting-unstuck)"]}}`},
			fails: []string{`/workspace/.claude/settings.json's permissions.deny holds "Skill(crewlet:getting-unstuck)"`}}},
		"the home denies the skill tool": {req: skills, switchOffCase: switchOffCase{
			plant: map[string]string{".claude/settings.json": `{"permissions": {"deny": ["Skill"]}}`},
			fails: []string{`permissions.deny holds "Skill", which takes the skill tool away`}}},
		"a later file's true clears nothing of the skills' plugin off": {req: skills, switchOffCase: switchOffCase{
			plant: map[string]string{".claude/settings.json": `{"enabledPlugins": {"crewlet@inline": false}}`,
				"workspace/.claude/settings.json": `{"enabledPlugins": {"crewlet@inline": true}}`},
			fails: []string{`/.claude/settings.json sets enabledPlugins["crewlet@inline"] to false`}}},
		"keys of another case, which Claude Code ignores": {req: skills, switchOffCase: switchOffCase{
			plant: map[string]string{".claude/settings.json": `{"EnabledPlugins": {"crewlet@inline": false}, ` +
				`"Permissions": {"Deny": ["Skill"]}}`}}},
		"a prefix deny covering a seat skill": {req: skills, switchOffCase: switchOffCase{
			plant: map[string]string{".claude/settings.json": `{"permissions": {"deny": ["Skill(crewlet:getting-*)"]}}`},
			fails: []string{`permissions.deny holds "Skill(crewlet:getting-*)", the seat skill crewlet:getting-unstuck`}}},
		"switches of other plugins and skills": {req: skills, switchOffCase: switchOffCase{
			plant: map[string]string{".claude/settings.json": `{"enabledPlugins": {"crewlet": false, "x@y": false}, ` +
				`"permissions": {"deny": ["Skill(other)", "Bash", "Read(crewlet:getting-unstuck)"]}}`}}},
		"a run whose only hook would be its checkouts' starts without it": {req: sandbox.RunRequest{Brief: "go"},
			switchOffCase: switchOffCase{plant: map[string]string{".claude/settings.json": off},
				says: []string{"not handed their skills: ", "/.claude/settings.json sets disableAllHooks to true, so"}}},
		"a run with no hook of the engine's, below the checkouts' floor": {release: "2.1.309",
			req: sandbox.RunRequest{Brief: "go"}, switchOffCase: switchOffCase{plant: map[string]string{".claude/settings.json": off}}},
		"a release older than every hook's floor": {release: "2.1.117", req: skills, switchOffCase: switchOffCase{
			plant: map[string]string{".claude/settings.json": off}}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			release := tc.release
			if release == "" {
				release = "2.1.330"
			}
			runner, box := realBox(t)
			if tc.machine != "" {
				box = machineBox{Sandbox: box, machine: tc.machine}
			}
			writeBoxStandIn(t, box, executor.PathsFor(box).BinDir()+"/claude", "#!/bin/sh\n"+
				`case "$1" in --version) echo '`+release+` (Claude Code)'; exit 0;; esac`+"\ncat > /dev/null\n"+
				`echo '{"type":"result","subtype":"success","is_error":false,"result":"done"}'`+"\n")
			plant(t, box, tc.plant)
			_, res := runToEnd(t, runner, box, tc.req)
			checkSwitchOff(t, box, res, tc.switchOffCase)
		})
	}
}

// machineBox is a box whose `uname -m` answers machine, so a launch takes it
// for a machine the note helper has no build for.
type machineBox struct {
	sandbox.Sandbox
	machine string
}

// Exec answers `uname -m` with b's machine and runs the rest in the box.
func (b machineBox) Exec(ctx context.Context, cmd string, opts sandbox.ExecOptions) (sandbox.ExecResult, error) {
	if cmd == "uname -m" {
		return sandbox.ExecResult{Stdout: b.machine + "\n"}, nil
	}
	return b.Sandbox.Exec(ctx, cmd, opts)
}
