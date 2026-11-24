package executor_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// A HOME OR A REPOSITORY CANNOT SWITCH A CLAUDE CODE RUN'S HOOKS OR SKILLS OFF
// UNSEEN. 2.1.330 runs none of the run's hooks where the merged settings leave
// `disableAllHooks` true, and drops its skills where `enabledPlugins` holds
// their plugin false or a `Skill` deny names them (measured); the run never
// overrules either. So a run starting a hook of the engine's is refused before
// the harness starts, naming the file and the key; it starts where a later file
// sets false, the key is no boolean or the file no JSON (ignored whole), in the
// home's settings.local.json (never read), nested, allowManagedHooksOnly outside
// a managed file, other plugins' and skills' switches, and where it starts no
// hook of the engine's (all measured). Mutations, each red: drop the gate; let a
// file's false decide nothing; read a string as true; name no file; skip the
// skills' check.
func TestClaudeCodesHooksCannotBeSwitchedOffUnseen(t *testing.T) {
	t.Parallel()
	const off = `{"disableAllHooks": true}`
	const said = "Claude Code would run none of the run's hooks"
	skills := sandbox.RunRequest{Brief: "go", Skills: handedSkills()[1:], SkillLoads: skillWiring()}
	notes := sandbox.RunRequest{Brief: "go", Steer: steerWiring}
	for name, tc := range map[string]struct {
		release string
		req     sandbox.RunRequest
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
		"the repository's local settings switch them off": {req: skills, switchOffCase: switchOffCase{
			plant: map[string]string{"workspace/.claude/settings.local.json": off},
			fails: []string{said, "/workspace/.claude/settings.local.json sets disableAllHooks"}}},
		"the repository's false wins over the home's true": {req: skills, switchOffCase: switchOffCase{
			plant: map[string]string{".claude/settings.json": off,
				"workspace/.claude/settings.json": `{"disableAllHooks": false}`}}},
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
		"a later file switches the skills' plugin back on": {req: skills, switchOffCase: switchOffCase{
			plant: map[string]string{".claude/settings.json": `{"enabledPlugins": {"crewlet@inline": false}}`,
				"workspace/.claude/settings.json": `{"enabledPlugins": {"crewlet@inline": true}}`}}},
		"switches of other plugins and skills": {req: skills, switchOffCase: switchOffCase{
			plant: map[string]string{".claude/settings.json": `{"enabledPlugins": {"crewlet": false, "x@y": false}, ` +
				`"permissions": {"deny": ["Skill(other)", "Bash", "Read(crewlet:getting-unstuck)"]}}`}}},
		"a run with no hook of the engine's": {req: sandbox.RunRequest{Brief: "go"}, switchOffCase: switchOffCase{
			plant: map[string]string{".claude/settings.json": off}}},
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
			writeBoxStandIn(t, box, executor.PathsFor(box).BinDir()+"/claude", "#!/bin/sh\n"+
				`case "$1" in --version) echo '`+release+` (Claude Code)'; exit 0;; esac`+"\ncat > /dev/null\n"+
				`echo '{"type":"result","subtype":"success","is_error":false,"result":"done"}'`+"\n")
			plant(t, box, tc.plant)
			_, res := runToEnd(t, runner, box, tc.req)
			checkSwitchOff(t, box, res, tc.switchOffCase)
		})
	}
}
